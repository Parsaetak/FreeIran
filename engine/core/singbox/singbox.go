// Package singbox implements the sing-box backend adapter
// (github.com/SagerNet/sing-box).
//
// sing-box uses its own JSON schema (inbounds/outbounds with typed
// transport and TLS objects, a "mixed" local inbound). The adapter
// converts the normalized model into that schema; capability
// declarations encode the verified 1.14 feature set.
package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Subsystem identifies the sing-box backend in structured errors.
const Subsystem = "core.singbox"

// Backend is the sing-box adapter.
type Backend struct {
	genCache *core.GenCache
}

// New creates a sing-box backend adapter.
func New() *Backend {
	return &Backend{genCache: core.NewGenCache()}
}

// Name returns the canonical backend identifier.
func (b *Backend) Name() string { return "sing-box" }

// Capabilities declares the verified sing-box feature set. Verified
// against sing-box v1.14.0: all listed protocols and transports pass
// `sing-box check` (and the real-binary smoke suite); REALITY and
// vision are supported through the tls/utls/reality objects.
//
// v0.10.2: Hysteria2, TUIC, WireGuard and Hysteria are DECLARED and
// GENERATED here after schema verification against the pinned v1.14.0
// binary (`sing-box check`): hysteria2/tuic/hysteria require TLS
// enabled, WireGuard uses the endpoint form (the outbound form was
// removed in sing-box 1.11). WireGuard support additionally requires
// the gvisor/wireguard build tag — present in every official release
// binary the core manager installs.
func (b *Backend) Capabilities() core.Capabilities {
	return core.Capabilities{
		Protocols: []config.Type{
			config.TypeVLESS,
			config.TypeVMess,
			config.TypeTrojan,
			config.TypeShadowsocks,
			config.TypeHysteria2,
			config.TypeTUIC,
			config.TypeWireGuard,
			config.TypeHysteria,
			config.TypeSOCKS,
			config.TypeHTTP,
		},
		Transports: []string{
			"tcp", "ws", "grpc", "http", "quic", "httpupgrade",
		},
		Securities: []string{"none", "tls", "reality"},
		Flows:      []string{"", "xtls-rprx-vision"},
		TLSMandatory: []config.Type{
			config.TypeTrojan,
		},
		Notes: []string{
			"REALITY supported through tls.reality",
			"xtls-rprx-vision flow supported (with uTLS)",
			"mixed inbound serves SOCKS and HTTP on one port",
			"hysteria2/tuic/hysteria verified against v1.14.0 (TLS mandatory, QUIC)",
			"wireguard verified against v1.14.0 (endpoint form, local address auto-generated when absent)",
		},
	}
}

// Supports reports whether sing-box can execute the configuration.
func (b *Backend) Supports(cfg config.Config) bool {
	return b.Capabilities().Matches(cfg)
}

// Validate performs deep validation against sing-box capabilities.
func (b *Backend) Validate(_ context.Context, cfg config.Config) error {
	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		return firerrors.Wrap(err, firerrors.KindInvalidInput,
			Subsystem, "validate", "configuration rejected")
	}

	caps := b.Capabilities()

	if !caps.Matches(cfg) {
		return capabilityError(cfg, caps)
	}

	if v2raySecurity(cfg) == config.SecurityReality &&
		cfg.PublicKey == "" {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"REALITY configuration requires a public key (pbk)")
	}

	if cfg.Flow == "xtls-rprx-vision" {
		if network := normalizedNetwork(cfg); network != string(config.NetworkTCP) {
			return firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "validate",
				"xtls-rprx-vision requires plain TCP transport (got %q)", network)
		}
	}

	// v0.10.2 QUIC-family and WireGuard deep validation — the same
	// requirements the real v1.14.0 binary enforces at startup.
	switch cfg.Type {
	case config.TypeHysteria2:
		if strings.TrimSpace(cfg.Password) == "" {
			return firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "validate",
				"Hysteria2 configuration requires an auth password")
		}

	case config.TypeTUIC:
		if strings.TrimSpace(cfg.UUID) == "" {
			return firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "validate",
				"TUIC configuration requires a UUID")
		}

	case config.TypeHysteria:
		if cfg.UpMbps <= 0 || cfg.DownMbps <= 0 {
			return firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "validate",
				"Hysteria (v1) requires upmbps and downmbps bandwidth caps")
		}

	case config.TypeWireGuard:
		if strings.TrimSpace(cfg.PrivateKey) == "" {
			return firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "validate",
				"WireGuard configuration requires the local private key")
		}

		if strings.TrimSpace(cfg.PublicKey) == "" {
			return firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "validate",
				"WireGuard configuration requires the peer public key")
		}
	}

	return nil
}

// capabilityError renders a precise capability mismatch.
func capabilityError(cfg config.Config, caps core.Capabilities) error {
	switch {
	case !caps.SupportsProtocol(cfg.Type):
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"sing-box does not support protocol %s", cfg.Type)

	case !caps.SupportsTransport(cfg.Network):
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"sing-box does not support transport %q", cfg.Network)

	case !caps.SupportsSecurity(cfg.Security):
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"sing-box does not support security %q", cfg.Security)

	default:
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"sing-box capability mismatch for %s", cfg.DisplayURL())
	}
}

// BuildConfig converts a normalized configuration into a sing-box
// runtime document.
func (b *Backend) BuildConfig(cfg config.Config, opts core.RuntimeOptions) (core.RuntimeConfig, error) {
	cfg.Normalize()

	cacheKey := core.GenCacheKey(cfg.Fingerprint(), "sing-box", opts.LocalHost, opts.LocalPort, opts.HTTPPort)
	generation := core.GenerationFor("sing-box", opts.BinaryPath, opts.BackendVersion)

	if !opts.DisableGenCache {
		if doc, ok := b.genCache.Get(cacheKey, generation); ok {
			return doc, nil
		}
	}

	data, summary, err := BuildSingBoxDocument(cfg, opts)
	if err != nil {
		return core.RuntimeConfig{}, err
	}

	doc := core.RuntimeConfig{
		FileName:        fmt.Sprintf("singbox-%s.json", shortFingerprint(cfg)),
		Data:            data,
		RedactedSummary: summary,
		Format:          "sing-box",
	}

	if !opts.DisableGenCache {
		b.genCache.Put(cacheKey, generation, doc)
	}

	return doc, nil
}

// Start builds the runtime configuration and launches sing-box
// through the shared core launcher.
func (b *Backend) Start(
	ctx context.Context,
	cfg config.Config,
	opts core.RuntimeOptions,
) (*core.Instance, error) {
	if opts.BinaryPath == "" {
		return nil, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "start",
			"sing-box executable path is not resolved (registry discovery required)")
	}

	opts = opts.WithDefaults()

	// v0.9.9: the inbound port is resolved through the ONE
	// authoritative execution-stage resolver, BEFORE generation (the
	// runtime document embeds the final inbound port).
	port, err := core.ResolveInboundPort(opts.LocalHost, opts.LocalPort)
	if err != nil {
		return nil, err
	}

	opts.LocalPort = port

	doc, err := b.BuildConfig(cfg, opts)
	if err != nil {
		return nil, err
	}

	return core.Launch(ctx, b, cfg, opts, doc, singBoxArgs)
}

// singBoxArgs renders the sing-box command line: `run -c <file>`.
func singBoxArgs(configFile string, _ int) []string {
	return []string{"run", "-c", configFile}
}

// shortFingerprint renders an 8-character fingerprint prefix.
func shortFingerprint(cfg config.Config) string {
	fp := cfg.Fingerprint()

	if len(fp) > 8 {
		return fp[:8]
	}

	return fp
}

// --- sing-box document model --------------------------------------------

type sbDocument struct {
	Log       *sbLog       `json:"log,omitempty"`
	Inbounds  []sbInbound  `json:"inbounds"`
	Outbounds []sbOutbound `json:"outbounds"`
	Endpoints []sbEndpoint `json:"endpoints,omitempty"`
	Route     *sbRoute     `json:"route,omitempty"`
}

type sbLog struct {
	Level string `json:"level"`
}

type sbInbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag,omitempty"`
	Listen     string `json:"listen"`
	ListenPort int    `json:"listen_port"`
}

type sbOutbound struct {
	Type       string       `json:"type"`
	Tag        string       `json:"tag,omitempty"`
	Server     string       `json:"server,omitempty"`
	ServerPort int          `json:"server_port,omitempty"`
	UUID       string       `json:"uuid,omitempty"`
	Password   string       `json:"password,omitempty"`
	Method     string       `json:"method,omitempty"`
	Username   string       `json:"username,omitempty"`
	Flow       string       `json:"flow,omitempty"`
	TLS        *sbTLS       `json:"tls,omitempty"`
	Transport  *sbTransport `json:"transport,omitempty"`
	Version    string       `json:"version,omitempty"`  // socks outbound
	Security   string       `json:"security,omitempty"` // vmess cipher
	AlterID    int          `json:"alter_id,omitempty"`

	// QUIC family (v0.10.2, verified against sing-box v1.14.0).
	UpMbps            int     `json:"up_mbps,omitempty"`            // hysteria/hysteria2
	DownMbps          int     `json:"down_mbps,omitempty"`          // hysteria/hysteria2
	AuthStr           string  `json:"auth_str,omitempty"`           // hysteria (v1)
	CongestionControl string  `json:"congestion_control,omitempty"` // tuic
	UDPRelayMode      string  `json:"udp_relay_mode,omitempty"`     // tuic
	Obfs              *sbObfs `json:"obfs,omitempty"`               // hysteria/hysteria2 salamander
}

// sbObfs is the salamander obfuscation object of the hysteria
// outbounds.
type sbObfs struct {
	Type     string `json:"type"`
	Password string `json:"password,omitempty"`
}

// sbEndpoint is the sing-box endpoint form (v1.11+): WireGuard is an
// ENDPOINT, not an outbound — the "wireguard" outbound type was
// removed in sing-box 1.11.0 and the pinned v1.14.0 accepts only
// this shape (verified with `sing-box check`).
type sbEndpoint struct {
	Type       string     `json:"type"`
	Tag        string     `json:"tag"`
	System     bool       `json:"system"`
	Address    []string   `json:"address,omitempty"`
	PrivateKey string     `json:"private_key"`
	MTU        int        `json:"mtu,omitempty"`
	Peers      []sbWGPeer `json:"peers"`
}

// sbWGPeer is one WireGuard peer inside an endpoint.
type sbWGPeer struct {
	Address                     string   `json:"address"`
	Port                        int      `json:"port"`
	PublicKey                   string   `json:"public_key"`
	PreSharedKey                string   `json:"pre_shared_key,omitempty"`
	AllowedIPs                  []string `json:"allowed_ips"`
	PersistentKeepaliveInterval int      `json:"persistent_keepalive_interval,omitempty"`
}

type sbTLS struct {
	Enabled    bool       `json:"enabled"`
	ServerName string     `json:"server_name,omitempty"`
	Insecure   bool       `json:"insecure,omitempty"`
	ALPN       []string   `json:"alpn,omitempty"`
	UTLS       *sbUTLS    `json:"utls,omitempty"`
	Reality    *sbReality `json:"reality,omitempty"`
}

type sbUTLS struct {
	Enabled     bool   `json:"enabled"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type sbReality struct {
	Enabled   bool   `json:"enabled"`
	PublicKey string `json:"public_key,omitempty"`
	ShortID   string `json:"short_id,omitempty"`
}

type sbTransport struct {
	Type        string            `json:"type"`
	Path        string            `json:"path,omitempty"`
	ServiceName string            `json:"service_name,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Host        []string          `json:"host,omitempty"`
}

type sbRoute struct {
	Rules []sbRule `json:"rules,omitempty"`
	Final string   `json:"final,omitempty"`
}

type sbRule struct {
	IPIsPrivate bool   `json:"ip_is_private,omitempty"`
	Outbound    string `json:"outbound"`
}

// BuildSingBoxDocument renders the sing-box runtime configuration
// with a mixed (SOCKS+HTTP) local inbound, the proxy outbound from
// the normalized configuration and a direct route for private
// ranges.
func BuildSingBoxDocument(cfg config.Config, opts core.RuntimeOptions) ([]byte, string, error) {
	cfg.Normalize()

	opts = opts.WithDefaults()

	security := v2raySecurity(cfg)

	if security == config.SecurityReality && cfg.PublicKey == "" {
		return nil, "", firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "build",
			"REALITY configuration requires a public key")
	}

	port := opts.LocalPort

	if port == 0 {
		return nil, "", firerrors.New(firerrors.KindConfiguration,
			Subsystem, "build",
			"local port must be allocated before generation")
	}

	var proxyOutbound *sbOutbound

	if cfg.Type != config.TypeWireGuard {
		// WireGuard rides the endpoint form and has no outbound.
		built, err := buildSBOutbound(cfg, security)
		if err != nil {
			return nil, "", err
		}

		proxyOutbound = built
	}

	doc := sbDocument{
		Log: &sbLog{Level: "warn"},
		Inbounds: []sbInbound{
			{
				Type:       "mixed",
				Tag:        "mixed-in",
				Listen:     opts.LocalHost,
				ListenPort: port,
			},
		},
		Outbounds: []sbOutbound{
			{Type: "direct", Tag: "direct"},
			{Type: "block", Tag: "block"},
		},
		Route: &sbRoute{
			Rules: []sbRule{
				{IPIsPrivate: true, Outbound: "direct"},
			},
			Final: "proxy",
		},
	}

	if cfg.Type == config.TypeWireGuard {
		// WireGuard rides the v1.11+ ENDPOINT form: the proxy target is
		// an endpoint, and the outbounds are only the plumbing. The
		// route's final "proxy" tag addresses the endpoint directly.
		doc.Endpoints = []sbEndpoint{*buildSBWireGuardEndpoint(cfg)}
	} else {
		doc.Outbounds = append([]sbOutbound{*proxyOutbound}, doc.Outbounds...)
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, "", firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "build", "encode document")
	}

	summary := fmt.Sprintf("sing-box config: %s, mixed %s:%d",
		cfg.DisplayURL(), opts.LocalHost, port)

	return data, summary, nil
}

// buildSBOutbound converts the normalized configuration into the
// typed sing-box outbound.
func buildSBOutbound(cfg config.Config, security config.Security) (*sbOutbound, error) {
	outbound := &sbOutbound{
		Tag: "proxy",
	}

	switch cfg.Type {
	case config.TypeVLESS:
		outbound.Type = "vless"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.UUID = cfg.UUID
		outbound.Flow = cfg.Flow

	case config.TypeVMess:
		outbound.Type = "vmess"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.UUID = cfg.UUID
		outbound.Security = "auto"

		if cfg.AlterID > 0 {
			outbound.AlterID = cfg.AlterID
		}

	case config.TypeTrojan:
		outbound.Type = "trojan"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.Password = cfg.Password

	case config.TypeShadowsocks:
		outbound.Type = "shadowsocks"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.Method = cfg.Method
		outbound.Password = cfg.Password

	case config.TypeHysteria2:
		// Verified against v1.14.0: TLS required (checked at startup),
		// password auth, optional salamander obfs, optional
		// bandwidth caps. v0.10.3: obfs comes from its OWN fields
		// (Obfs/ObfsPassword) — v0.10.2 read it out of Security/Host,
		// which the parser no longer pollutes.
		outbound.Type = "hysteria2"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.Password = cfg.Password
		outbound.UpMbps = cfg.UpMbps
		outbound.DownMbps = cfg.DownMbps

		if cfg.Obfs != "" && cfg.Obfs != config.ObfsSalamander {
			return nil, firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "build",
				"invalid Hysteria2 obfs %q (allowed: empty or %q)", cfg.Obfs, config.ObfsSalamander)
		}

		if cfg.Obfs != "" {
			outbound.Obfs = &sbObfs{Type: cfg.Obfs, Password: cfg.ObfsPassword}
		}

	case config.TypeTUIC:
		// Verified against v1.14.0: TLS required, uuid+password (v5
		// auth), native UDP relay. v0.10.3: congestion_control comes
		// from its OWN field (v0.10.2 read it out of Network, which
		// the parser no longer pollutes — "bbr" is not a transport).
		// The value domain was validated upstream; enforce it here so
		// a programmatically-built config cannot smuggle garbage into
		// the core document.
		outbound.Type = "tuic"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.UUID = cfg.UUID
		outbound.Password = cfg.Password

		if cfg.CongestionControl != "" && !config.ValidTUICCongestionControl(cfg.CongestionControl) {
			return nil, firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "build",
				"invalid TUIC congestion_control %q (allowed: %s)",
				cfg.CongestionControl, strings.Join(config.TUICCongestionControlValues, ", "))
		}

		outbound.CongestionControl = cfg.CongestionControl

		if cfg.UDPRelayMode != "" && !config.ValidTUICUDPRelayMode(cfg.UDPRelayMode) {
			return nil, firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "build",
				"invalid TUIC udp_relay_mode %q (allowed: %s)",
				cfg.UDPRelayMode, strings.Join(config.TUICUDPRelayModeValues, ", "))
		}

		// sing-box defaults udp_relay_mode to "native"; pin the
		// documented default when the config did not choose one.
		outbound.UDPRelayMode = cfg.UDPRelayMode

		if outbound.UDPRelayMode == "" {
			outbound.UDPRelayMode = config.UDPRelayModeNative
		}

	case config.TypeHysteria:
		// Verified against v1.14.0: TLS required, auth_str auth,
		// QUIC transport (the "protocol" field of older schemas is
		// gone) and MANDATORY up/down bandwidth caps ("missing
		// upload speed" otherwise).
		outbound.Type = "hysteria"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.AuthStr = cfg.Password
		outbound.UpMbps = cfg.UpMbps
		outbound.DownMbps = cfg.DownMbps

		if cfg.Obfs != "" && cfg.Obfs != config.ObfsSalamander {
			return nil, firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "build",
				"invalid Hysteria obfs %q (allowed: empty or %q)", cfg.Obfs, config.ObfsSalamander)
		}

		if cfg.Obfs != "" {
			outbound.Obfs = &sbObfs{Type: cfg.Obfs, Password: cfg.ObfsPassword}
		}

	case config.TypeSOCKS:
		outbound.Type = "socks"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.Version = "5"
		outbound.Username = cfg.Username
		outbound.Password = cfg.Password

	case config.TypeHTTP:
		outbound.Type = "http"
		outbound.Server = cfg.Address
		outbound.ServerPort = cfg.Port
		outbound.Username = cfg.Username
		outbound.Password = cfg.Password

	default:
		return nil, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "build", "unsupported protocol %s", cfg.Type)
	}

	// SOCKS/HTTP outbounds have no TLS/transport layer in sing-box
	// for the plain remote-proxy case.
	if cfg.Type == config.TypeSOCKS || cfg.Type == config.TypeHTTP {
		return outbound, nil
	}

	if cfg.Type == config.TypeHysteria2 || cfg.Type == config.TypeTUIC || cfg.Type == config.TypeHysteria {
		// The QUIC family is TLS-mandatory (the real binary refuses
		// otherwise: "initialize outbound: TLS required"). Insecure is
		// the user's explicit choice from the URI.
		outbound.TLS = buildSBTLS(cfg, config.SecurityTLS)

		return outbound, nil
	}

	if security == config.SecurityTLS || security == config.SecurityReality {
		outbound.TLS = buildSBTLS(cfg, security)
	}

	if transport := buildSBTransport(cfg); transport != nil {
		outbound.Transport = transport
	}

	return outbound, nil
}

// buildSBWireGuardEndpoint converts a normalized WireGuard
// configuration into the sing-box endpoint form.
//
// v0.10.3 endpoint semantics (the real v1.14 binary is authoritative:
// a wireguard ENDPOINT without a local `address` is rejected at
// startup with "missing local address" — the v0.10.2 document had
// no local address and only ever passed `sing-box check` in the
// fake-core smoke, never against the real binary's stricter runtime
// path):
//
//   - `address`      — the LOCAL interface address list
//     (cfg.InterfaceAddress); when the configuration did not record
//     one, a deterministic link-local-style default pair is
//     generated (documented, stable across runs).
//   - `peers[].address/port` — the PEER endpoint (cfg.Address/Port).
//   - `peers[].allowed_ips` — the PEER routing list
//     (cfg.AllowedIPs, defaulting to 0.0.0.0/0 + ::/0).
func buildSBWireGuardEndpoint(cfg config.Config) *sbEndpoint {
	address := cfg.InterfaceAddress

	if len(address) == 0 {
		// Deterministic local addresses for the generated endpoint
		// (the documented default when the imported configuration did
		// not carry an INI Address / URI address parameter).
		address = []string{"172.19.0.2/32", "fdfe:dcba:9876::2/128"}
	}

	endpoint := &sbEndpoint{
		Type:       "wireguard",
		Tag:        "proxy",
		System:     false,
		Address:    address,
		PrivateKey: cfg.PrivateKey,
		MTU:        cfg.MTU,
		Peers: []sbWGPeer{
			{
				Address:                     cfg.Address,
				Port:                        cfg.Port,
				PublicKey:                   cfg.PublicKey,
				AllowedIPs:                  cfg.AllowedIPs,
				PersistentKeepaliveInterval: cfg.PersistentKeepalive,
			},
		},
	}

	if len(endpoint.Peers[0].AllowedIPs) == 0 {
		endpoint.Peers[0].AllowedIPs = []string{"0.0.0.0/0", "::/0"}
	}

	return endpoint
}

// buildSBTLS renders the sing-box TLS object including uTLS and
// REALITY. Trojan implies TLS (sing-box enforces it at runtime).
func buildSBTLS(cfg config.Config, security config.Security) *sbTLS {
	tls := &sbTLS{
		Enabled:    true,
		ServerName: tlsServerName(cfg),
		Insecure:   cfg.Insecure,
	}

	if len(cfg.ALPN) > 0 {
		tls.ALPN = cfg.ALPN
	}

	if cfg.FingerprintProfile != "" {
		tls.UTLS = &sbUTLS{
			Enabled:     true,
			Fingerprint: cfg.FingerprintProfile,
		}
	}

	if security == config.SecurityReality {
		tls.Reality = &sbReality{
			Enabled:   true,
			PublicKey: cfg.PublicKey,
			ShortID:   cfg.ShortID,
		}

		// REALITY requires uTLS; default to chrome when no
		// fingerprint was published.
		if tls.UTLS == nil {
			tls.UTLS = &sbUTLS{Enabled: true, Fingerprint: "chrome"}
		}
	}

	return tls
}

// buildSBTransport renders the sing-box transport object.
func buildSBTransport(cfg config.Config) *sbTransport {
	network := normalizedNetwork(cfg)

	switch network {
	case "", string(config.NetworkTCP):
		return nil

	case string(config.NetworkWebSocket):
		transport := &sbTransport{Type: "ws", Path: cfg.Path}

		if cfg.Host != "" {
			transport.Headers = map[string]string{"Host": cfg.Host}
		}

		return transport

	case string(config.NetworkGRPC):
		return &sbTransport{
			Type:        "grpc",
			ServiceName: cfg.Service,
		}

	case string(config.NetworkHTTP2), "http":
		transport := &sbTransport{Type: "http", Path: cfg.Path}

		if cfg.Host != "" {
			transport.Host = splitCSV(cfg.Host)
		}

		return transport

	case string(config.NetworkQUIC):
		return &sbTransport{Type: "quic"}

	case "httpupgrade":
		transport := &sbTransport{Type: "httpupgrade", Path: cfg.Path}

		if cfg.Host != "" {
			transport.Host = splitCSV(cfg.Host)
		}

		return transport

	default:
		return nil
	}
}

// v2raySecurity mirrors the shared security normalization (trojan
// defaults to TLS) without importing the V2Ray adapter package.
func v2raySecurity(cfg config.Config) config.Security {
	if cfg.Security == "" {
		if cfg.Type == config.TypeTrojan {
			return config.SecurityTLS
		}

		return config.SecurityNone
	}

	return config.Security(cfg.Security)
}

// normalizedNetwork maps "" → "tcp" and normalizes aliases.
func normalizedNetwork(cfg config.Config) string {
	switch cfg.Network {
	case "":
		return string(config.NetworkTCP)

	case "h2":
		return "http"

	default:
		return cfg.Network
	}
}

// tlsServerName resolves the TLS SNI with fallbacks.
func tlsServerName(cfg config.Config) string {
	if cfg.ServerName != "" {
		return cfg.ServerName
	}

	return cfg.Address
}

// splitCSV splits a comma-separated host list.
func splitCSV(value string) []string {
	if value == "" {
		return nil
	}

	hosts := make([]string, 0, 4)

	for _, host := range splitString(value, ",") {
		if host != "" {
			hosts = append(hosts, host)
		}
	}

	return hosts
}

// splitString avoids importing strings for two call sites.
func splitString(value, sep string) []string {
	result := make([]string, 0, 4)

	start := 0

	for i := 0; i+len(sep) <= len(value); i++ {
		if value[i:i+len(sep)] == sep {
			result = append(result, value[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}

	return append(result, value[start:])
}
