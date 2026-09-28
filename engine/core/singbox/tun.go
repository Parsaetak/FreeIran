// tun.go implements the v0.11.3 sing-box TUN document: the native
// sing-box tun inbound used as FreeIran's Windows TUN dataplane.
//
// ARCHITECTURE (docs/architecture.md "TUN mode"): TUN is NOT a
// separate packet stack. The managed, digest-verified sing-box core
// is launched with a document that adds a tun inbound (Wintun-backed
// on Windows) BESIDE the standard mixed inbound; routing, DNS
// hijacking and loop prevention are delegated to sing-box's own
// documented route model:
//
//   - auto_route + strict_route  — kernel routing into the TUN, with
//     strict_route closing the ordinary multihomed Windows DNS leak.
//   - route.auto_detect_interface — sing-box binds its OWN upstream
//     (proxy) sockets to the detected default physical interface, so
//     the proxy connection can never loop back into the TUN.
//   - route rule {"protocol":"dns","action":"hijack-dns"} — plain DNS
//     arriving through the TUN is answered by sing-box's DNS module;
//     the remote resolver runs over the proxy (DoH), so queries never
//     leave in clear text. The system's adapter DNS settings are NOT
//     touched (no DHCP/netsh mutation, nothing to restore).
//   - rule {"ip_is_private"} → direct — local/LAN traffic stays local.
//
// The Wintun driver itself arrives EMBEDDED in the official sing-box
// Windows build (the Go wintun bindings compile the official
// wintun.dll into the binary and load it from there at runtime).
// FreeIran never downloads, extracts or PATH-resolves a Wintun
// binary; the integrity anchor is the managed sing-box install
// (coremgr pinned-digest verification, docs/security.md).
package singbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// TUNSettings carries the runtime TUN parameters the backend observes
// or derives from the real system state (never hardcoded network
// facts about the user's machine).
type TUNSettings struct {
	// InterfaceName is the requested TUN adapter name. The caller
	// picks a collision-free name (engine/tunnel observes the real
	// adapter list first and records the final name in its snapshot).
	InterfaceName string

	// IPv4Address / IPv6Address are the TUN interface addresses in
	// CIDR form (e.g. "172.19.0.1/30"). IPv6 is optional; empty
	// disables the IPv6 address family for this session. Callers
	// SHOULD avoid ranges colliding with existing local networks —
	// engine/tunnel picks from a small candidate set after inspecting
	// the live interface addresses.
	IPv4Address string
	IPv6Address string

	// RemoteDNS is the resolver (IP or DoH hostname) the sing-box DNS
	// module queries THROUGH the proxy for hijacked client queries.
	// The bootstrap resolver (used ONLY to resolve the proxy server's
	// own domain, when it is a domain) is sing-box's "local" DNS bound
	// to the physical interface — the unavoidable bootstrap hop,
	// documented honestly in docs/tun.md.
	RemoteDNS string

	// StrictRoute enables sing-box's strict_route (recommended on
	// Windows: prevents DNS leakage through ordinary multihomed
	// behavior).
	StrictRoute bool
}

// tunDocument is the sing-box document for TUN sessions. It is a
// SEPARATE model from sbDocument because the tun inbound and the DNS
// module have no overlap with the plain mixed-inbound schema; the
// outbound/endpoint models are shared verbatim.
type tunDocument struct {
	Log       *sbLog       `json:"log,omitempty"`
	DNS       *tunDNS      `json:"dns"`
	Inbounds  []tunInbound `json:"inbounds"`
	Outbounds []sbOutbound `json:"outbounds"`
	Endpoints []sbEndpoint `json:"endpoints,omitempty"`
	Route     *tunRoute    `json:"route"`
}

// tunInbound covers BOTH inbound shapes used in a TUN session (tun +
// mixed) with omitempty fields; only the fields each type needs are
// emitted.
type tunInbound struct {
	Type string `json:"type"`
	Tag  string `json:"tag,omitempty"`

	// tun-specific
	InterfaceName string   `json:"interface_name,omitempty"`
	Address       []string `json:"address,omitempty"`
	AutoRoute     bool     `json:"auto_route,omitempty"`
	StrictRoute   bool     `json:"strict_route,omitempty"`

	// mixed-specific
	Listen     string `json:"listen,omitempty"`
	ListenPort int    `json:"listen_port,omitempty"`
}

// tunDNS is the sing-box v1.12+ DNS module in the CURRENT server
// format ({"type": ...} objects; the legacy "address" string form is
// deprecated and must not be emitted for the pinned 1.14 runtime).
type tunDNS struct {
	Servers  []tunDNSServer `json:"servers"`
	Final    string         `json:"final,omitempty"`
	Strategy string         `json:"strategy,omitempty"`
}

type tunDNSServer struct {
	Type   string `json:"type"`
	Tag    string `json:"tag"`
	Server string `json:"server,omitempty"`
	Detour string `json:"detour,omitempty"`
}

// tunRoute extends the route model with the TUN-critical fields:
// auto_detect_interface (loop prevention) and the v1.11+ rule ACTION
// form (sniff / hijack-dns).
type tunRoute struct {
	Rules                 []tunRule    `json:"rules,omitempty"`
	Final                 string       `json:"final,omitempty"`
	AutoDetectInterface   bool         `json:"auto_detect_interface"`
	DefaultDomainResolver *tunResolver `json:"default_domain_resolver,omitempty"`
}

type tunRule struct {
	// Action is the v1.11+ rule action ("route" is the implicit
	// default and is never emitted; "sniff" and "hijack-dns" are used
	// here).
	Action string `json:"action,omitempty"`
	// Protocol matches a sniffed protocol ("dns").
	Protocol string `json:"protocol,omitempty"`
	// IPIsPrivate + Outbound are the classic route-match fields
	// (shared semantics with sbRule).
	IPIsPrivate bool   `json:"ip_is_private,omitempty"`
	Outbound    string `json:"outbound,omitempty"`
}

// tunResolver is the route.default_domain_resolver object (the DNS
// server tag used to resolve DOMAIN server addresses, e.g. the proxy
// server's own hostname — the bootstrap hop).
type tunResolver struct {
	Server string `json:"server"`
}

// ValidateTUNSettings enforces the non-negotiable TUN document
// invariants before anything reaches the core.
func ValidateTUNSettings(tun TUNSettings) error {
	if tun.InterfaceName == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "build-tun", "TUN interface name is required")
	}

	if tun.IPv4Address == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "build-tun", "TUN IPv4 address is required")
	}

	if tun.RemoteDNS == "" {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "build-tun", "TUN remote DNS resolver is required")
	}

	return nil
}

// BuildTUNDocument renders the sing-box runtime configuration for a
// TUN session: the tun inbound + the standard mixed inbound (the
// readiness probe and a local diagnostic endpoint), the proxy
// outbound from the normalized configuration, the DNS module and the
// auto-route document. The result must pass `sing-box check` for the
// pinned runtime — StartTUN launches with the same invariants the
// mixed document uses.
func BuildTUNDocument(cfg config.Config, opts core.RuntimeOptions, tun TUNSettings) ([]byte, string, error) {
	cfg.Normalize()

	opts = opts.WithDefaults()

	if err := ValidateTUNSettings(tun); err != nil {
		return nil, "", err
	}

	security := v2raySecurity(cfg)

	if security == config.SecurityReality && cfg.PublicKey == "" {
		return nil, "", firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "build-tun",
			"REALITY configuration requires a public key")
	}

	port := opts.LocalPort

	if port == 0 {
		return nil, "", firerrors.New(firerrors.KindConfiguration,
			Subsystem, "build-tun",
			"local port must be allocated before generation")
	}

	outbounds, endpoints, err := buildProxyTargets(cfg, security)
	if err != nil {
		return nil, "", err
	}

	address := []string{tun.IPv4Address}
	if tun.IPv6Address != "" {
		address = append(address, tun.IPv6Address)
	}

	doc := tunDocument{
		Log: &sbLog{Level: "warn"},
		DNS: &tunDNS{
			Servers: []tunDNSServer{
				{
					Type:   "https",
					Tag:    "dns-remote",
					Server: tun.RemoteDNS,
					Detour: "proxy",
				},
				{
					Type: "local",
					Tag:  "dns-local",
				},
			},
			Final:    "dns-remote",
			Strategy: "prefer_ipv4",
		},
		Inbounds: []tunInbound{
			{
				Type:          "tun",
				Tag:           "tun-in",
				InterfaceName: tun.InterfaceName,
				Address:       address,
				AutoRoute:     true,
				StrictRoute:   tun.StrictRoute,
			},
			{
				Type:       "mixed",
				Tag:        "mixed-in",
				Listen:     opts.LocalHost,
				ListenPort: port,
			},
		},
		Outbounds: outbounds,
		Endpoints: endpoints,
		Route: &tunRoute{
			Rules: []tunRule{
				// Sniff first so protocol-based rules see the real
				// protocol (DNS detection requires it).
				{Action: "sniff"},
				// Hijack ALL plain DNS arriving through the TUN into
				// sing-box's DNS module: client-configured DNS servers
				// are never contacted in clear text through the tunnel.
				{Action: "hijack-dns", Protocol: "dns"},
				// Local/LAN traffic never enters the proxy.
				{IPIsPrivate: true, Outbound: "direct"},
			},
			Final:               "proxy",
			AutoDetectInterface: true,
			// The ONLY DNS hop outside the tunnel: resolving the proxy
			// server's own domain (bootstrap). Bound to the physical
			// interface by auto_detect_interface.
			DefaultDomainResolver: &tunResolver{Server: "dns-local"},
		},
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, "", firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "build-tun", "encode document")
	}

	summary := fmt.Sprintf("sing-box TUN config: %s, tun %s (%s), mixed %s:%d",
		cfg.DisplayURL(), tun.InterfaceName, tun.IPv4Address, opts.LocalHost, port)

	return data, summary, nil
}

// StartTUN builds the TUN runtime document and launches sing-box
// through the shared core launcher. Readiness is the SAME observable
// the connection path uses (the mixed inbound listener) — the caller
// then observes the actual TUN interface separately, so "process
// started" is never confused with "TUN active".
func (b *Backend) StartTUN(
	ctx context.Context,
	cfg config.Config,
	opts core.RuntimeOptions,
	tun TUNSettings,
) (*core.Instance, error) {
	if opts.BinaryPath == "" {
		return nil, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "start-tun",
			"sing-box executable path is not resolved (managed core required)")
	}

	opts = opts.WithDefaults()

	// TUN documents embed per-session observed state (chosen
	// addresses, adapter name), so the generation cache is always
	// bypassed — a cached document could resurrect a stale address
	// choice.
	opts.DisableGenCache = true

	port, err := core.ResolveInboundPort(opts.LocalHost, opts.LocalPort)
	if err != nil {
		return nil, err
	}

	opts.LocalPort = port

	data, summary, err := BuildTUNDocument(cfg, opts, tun)
	if err != nil {
		return nil, err
	}

	doc := core.RuntimeConfig{
		FileName:        fmt.Sprintf("singbox-tun-%s.json", shortFingerprint(cfg)),
		Data:            data,
		RedactedSummary: summary,
		Format:          "sing-box",
	}

	return core.Launch(ctx, b, cfg, opts, doc, singBoxArgs)
}
