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
// `sing-box check`; REALITY and vision are supported through the
// tls/utls/reality objects.
func (b *Backend) Capabilities() core.Capabilities {
	return core.Capabilities{
		Protocols: []config.Type{
			config.TypeVLESS,
			config.TypeVMess,
			config.TypeTrojan,
			config.TypeShadowsocks,
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

	cacheKey := core.GenCacheKey(cfg.Fingerprint(), "sing-box")
	generation := core.GenerationFor("sing-box", opts.BinaryPath, opts)

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

	// Ephemeral ports are allocated BEFORE generation: the runtime
	// document embeds the final inbound port.
	if opts.LocalPort == 0 {
		port, err := core.ReserveLocalPort(opts.LocalHost)
		if err != nil {
			return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "start", "reserve local port")
		}

		opts.LocalPort = port
	}

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

	proxyOutbound, err := buildSBOutbound(cfg, security)
	if err != nil {
		return nil, "", err
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
			*proxyOutbound,
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

	if security == config.SecurityTLS || security == config.SecurityReality {
		outbound.TLS = buildSBTLS(cfg, security)
	}

	if transport := buildSBTransport(cfg); transport != nil {
		outbound.Transport = transport
	}

	return outbound, nil
}

// buildSBTLS renders the sing-box TLS object including uTLS and
// REALITY. Trojan implies TLS (sing-box enforces it at runtime).
func buildSBTLS(cfg config.Config, security config.Security) *sbTLS {
	tls := &sbTLS{
		Enabled:    true,
		ServerName: tlsServerName(cfg),
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
