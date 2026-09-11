package v2ray

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// --- V4 document model -------------------------------------------------
//
// The V4 JSON format is shared by V2Ray and Xray; Xray extends it
// with REALITY, VLESS flow and the XHTTP transport. The struct model
// below renders both dialects: Xray-only branches are emitted only
// when V4Options enables them, so a V2Ray document can never carry
// fields the V2Fly core does not understand.

// v4Document is the top-level V4 configuration.
type v4Document struct {
	Log       *v4Log       `json:"log,omitempty"`
	Inbounds  []v4Inbound  `json:"inbounds"`
	Outbounds []v4Outbound `json:"outbounds"`
	Routing   *v4Routing   `json:"routing,omitempty"`
}

type v4Log struct {
	LogLevel string `json:"loglevel"`
}

type v4Inbound struct {
	Tag      string          `json:"tag,omitempty"`
	Listen   string          `json:"listen"`
	Port     int             `json:"port"`
	Protocol string          `json:"protocol"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

type v4Outbound struct {
	Tag            string          `json:"tag,omitempty"`
	Protocol       string          `json:"protocol"`
	Settings       json.RawMessage `json:"settings,omitempty"`
	StreamSettings *v4Stream       `json:"streamSettings,omitempty"`
}

type v4Stream struct {
	Network         string           `json:"network"`
	Security        string           `json:"security,omitempty"`
	TLSSettings     *v4TLS           `json:"tlsSettings,omitempty"`
	RealitySettings *v4Reality       `json:"realitySettings,omitempty"`
	TCPSettings     *v4TCPTransport  `json:"tcpSettings,omitempty"`
	WSSettings      *v4WSTransport   `json:"wsSettings,omitempty"`
	GRPCSettings    *v4GRPCTransport `json:"grpcSettings,omitempty"`
	HTTPSettings    *v4HTTPTransport `json:"httpSettings,omitempty"`
	QUICSettings    *v4QUICTransport `json:"quicSettings,omitempty"`
}

type v4TLS struct {
	ServerName    string   `json:"serverName,omitempty"`
	AllowInsecure bool     `json:"allowInsecure,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	ALPN          []string `json:"alpn,omitempty"`
}

type v4Reality struct {
	ServerName  string `json:"serverName"`
	Fingerprint string `json:"fingerprint,omitempty"`
	PublicKey   string `json:"publicKey"`
	ShortID     string `json:"shortId,omitempty"`
	SpiderX     string `json:"spiderX,omitempty"`
}

type v4TCPTransport struct {
	Header *v4TCPHeader `json:"header,omitempty"`
}

type v4TCPHeader struct {
	Type string `json:"type"`
}

type v4WSTransport struct {
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type v4GRPCTransport struct {
	ServiceName string `json:"serviceName"`
}

type v4HTTPTransport struct {
	Path string   `json:"path,omitempty"`
	Host []string `json:"host,omitempty"`
}

type v4QUICTransport struct {
	Security string `json:"security,omitempty"`
}

type v4Routing struct {
	DomainStrategy string   `json:"domainStrategy,omitempty"`
	Rules          []v4Rule `json:"rules"`
}

type v4Rule struct {
	Type        string   `json:"type"`
	IP          []string `json:"ip,omitempty"`
	OutboundTag string   `json:"outboundTag"`
}

// V4Options tunes V4-format generation per backend dialect.
type V4Options struct {
	// AllowReality permits REALITY stream settings (Xray dialect).
	AllowReality bool

	// AllowFlow permits VLESS flow values (Xray dialect).
	AllowFlow bool

	// AllowXHTTP permits the xhttp transport (Xray dialect).
	AllowXHTTP bool

	// BackendName tags the document for diagnostics.
	BackendName string
}

// privateCIDRs are literal private/network ranges kept off the proxy
// path. Literal CIDRs avoid any dependency on geoip data files that
// minimal core installations may not ship.
var privateCIDRs = []string{
	"127.0.0.0/8",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
}

// BuildV4Document renders the V4-format configuration shared by
// V2Ray and Xray. The output is deterministic and carries the local
// inbound endpoints from opts.
//
// Inbounds: SOCKS on opts.LocalHost:opts.LocalPort (+ HTTP on
// opts.HTTPPort when set). Outbounds: the proxy (from cfg), direct
// and block. Routing: private ranges stay direct.
func BuildV4Document(
	cfg config.Config,
	opts core.RuntimeOptions,
	v4opts V4Options,
) ([]byte, string, error) {
	cfg.Normalize()

	opts = opts.WithDefaults()

	backend := v4opts.BackendName

	if backend == "" {
		backend = "v2ray"
	}

	// Dialect gate: refuse to emit fields the backend cannot execute.
	security := NormalizedSecurity(cfg)

	if security == config.SecurityReality && !v4opts.AllowReality {
		return nil, "", firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "build",
			"%s does not support REALITY (required by %s)",
			backend, cfg.DisplayURL())
	}

	if cfg.Flow != "" && !v4opts.AllowFlow {
		return nil, "", firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "build",
			"%s does not support VLESS flow %q", backend, cfg.Flow)
	}

	port := opts.LocalPort

	if port == 0 {
		return nil, "", firerrors.New(firerrors.KindConfiguration,
			Subsystem, "build",
			"local port must be allocated before generation")
	}

	proxyOutbound, err := buildV4Outbound(cfg, v4opts)
	if err != nil {
		return nil, "", err
	}

	doc := v4Document{
		Log: &v4Log{LogLevel: "warning"},
		Inbounds: []v4Inbound{
			{
				Tag:      "socks-in",
				Listen:   opts.LocalHost,
				Port:     port,
				Protocol: "socks",
				Settings: json.RawMessage(`{"auth":"noauth","udp":true}`),
			},
		},
		Outbounds: []v4Outbound{
			*proxyOutbound,
			{Tag: "direct", Protocol: "freedom"},
			{Tag: "block", Protocol: "blackhole"},
		},
		Routing: &v4Routing{
			DomainStrategy: "AsIs",
			Rules: []v4Rule{
				{
					Type:        "field",
					IP:          privateCIDRs,
					OutboundTag: "direct",
				},
			},
		},
	}

	if opts.HTTPPort > 0 {
		doc.Inbounds = append(doc.Inbounds, v4Inbound{
			Tag:      "http-in",
			Listen:   opts.LocalHost,
			Port:     opts.HTTPPort,
			Protocol: "http",
			Settings: json.RawMessage(`{}`),
		})
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, "", firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "build", "encode document")
	}

	summary := fmt.Sprintf("%s v4 config: %s, socks %s:%d, %s",
		backend, cfg.DisplayURL(), opts.LocalHost, port,
		DescribeTransport(cfg))

	return data, summary, nil
}

// buildV4Outbound converts the normalized configuration into the
// protocol-specific outbound.
func buildV4Outbound(cfg config.Config, v4opts V4Options) (*v4Outbound, error) {
	settings, err := buildV4Settings(cfg, v4opts)
	if err != nil {
		return nil, err
	}

	outbound := &v4Outbound{
		Tag:      "proxy",
		Protocol: V4Protocol(cfg.Type),
		Settings: settings,
	}

	// Plain SOCKS/HTTP outbounds carry no stream settings.
	if cfg.Type == config.TypeSOCKS || cfg.Type == config.TypeHTTP {
		return outbound, nil
	}

	stream, err := buildV4Stream(cfg, v4opts)
	if err != nil {
		return nil, err
	}

	outbound.StreamSettings = stream

	return outbound, nil
}

// V4Protocol maps normalized protocol types onto V4 protocol names.
func V4Protocol(t config.Type) string {
	switch t {
	case config.TypeVLESS:
		return "vless"

	case config.TypeVMess:
		return "vmess"

	case config.TypeTrojan:
		return "trojan"

	case config.TypeShadowsocks:
		return "shadowsocks"

	case config.TypeSOCKS:
		return "socks"

	case config.TypeHTTP:
		return "http"

	default:
		return string(t)
	}
}

// buildV4Settings renders the protocol-specific outbound settings.
func buildV4Settings(cfg config.Config, v4opts V4Options) (json.RawMessage, error) {
	switch cfg.Type {
	case config.TypeVLESS:
		user := map[string]any{
			"id":         cfg.UUID,
			"encryption": VLESSEncryption(cfg),
			"level":      0,
		}

		// Flow is emitted only by the Xray dialect (AllowFlow); the
		// V2Ray dialect refuses flow earlier in Validate.
		if cfg.Flow != "" && v4opts.AllowFlow {
			user["flow"] = cfg.Flow
		}

		return marshalSettings(map[string]any{
			"vnext": []any{map[string]any{
				"address": cfg.Address,
				"port":    cfg.Port,
				"users":   []any{user},
			}},
		})

	case config.TypeVMess:
		return marshalSettings(map[string]any{
			"vnext": []any{map[string]any{
				"address": cfg.Address,
				"port":    cfg.Port,
				"users": []any{map[string]any{
					"id":       cfg.UUID,
					"alterId":  AlterID(cfg),
					"security": "auto",
					"level":    0,
				}},
			}},
		})

	case config.TypeTrojan:
		return marshalSettings(map[string]any{
			"servers": []any{map[string]any{
				"address":  cfg.Address,
				"port":     cfg.Port,
				"password": cfg.Password,
			}},
		})

	case config.TypeShadowsocks:
		return marshalSettings(map[string]any{
			"servers": []any{map[string]any{
				"address":  cfg.Address,
				"port":     cfg.Port,
				"method":   cfg.Method,
				"password": cfg.Password,
			}},
		})

	case config.TypeSOCKS, config.TypeHTTP:
		server := map[string]any{
			"address": cfg.Address,
			"port":    cfg.Port,
		}

		if cfg.Username != "" || cfg.Password != "" {
			server["users"] = []any{map[string]any{
				"user": cfg.Username,
				"pass": cfg.Password,
			}}
		}

		return marshalSettings(map[string]any{"servers": []any{server}})

	default:
		return nil, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "build", "unsupported protocol %s", cfg.Type)
	}
}

// marshalSettings renders a settings object deterministically.
func marshalSettings(settings map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "build", "encode settings")
	}

	return raw, nil
}

// buildV4Stream renders streamSettings from transport + security.
func buildV4Stream(cfg config.Config, v4opts V4Options) (*v4Stream, error) {
	security := NormalizedSecurity(cfg)
	network := NormalizedNetwork(cfg)

	stream := &v4Stream{
		Network:  network,
		Security: string(security),
	}

	if security == config.SecurityTLS {
		stream.TLSSettings = buildV4TLS(cfg)
	}

	if security == config.SecurityReality && v4opts.AllowReality {
		stream.RealitySettings = &v4Reality{
			ServerName:  realityServerName(cfg),
			Fingerprint: cfg.FingerprintProfile,
			PublicKey:   cfg.PublicKey,
			ShortID:     cfg.ShortID,
			SpiderX:     cfg.SpiderX,
		}
	}

	switch network {
	case string(config.NetworkTCP):
		if cfg.HeaderType != "" && cfg.HeaderType != "none" {
			stream.TCPSettings = &v4TCPTransport{
				Header: &v4TCPHeader{Type: cfg.HeaderType},
			}
		}

	case string(config.NetworkWebSocket):
		ws := &v4WSTransport{Path: cfg.Path}

		if cfg.Host != "" {
			ws.Headers = map[string]string{"Host": cfg.Host}
		}

		stream.WSSettings = ws

	case string(config.NetworkGRPC):
		stream.GRPCSettings = &v4GRPCTransport{ServiceName: cfg.Service}

	case string(config.NetworkHTTP2), "http":
		http := &v4HTTPTransport{Path: cfg.Path}

		if cfg.Host != "" {
			http.Host = strings.Split(cfg.Host, ",")
		}

		stream.HTTPSettings = http

	case string(config.NetworkQUIC):
		stream.QUICSettings = &v4QUICTransport{}

	case string(config.NetworkXHTTP):
		if !v4opts.AllowXHTTP {
			return nil, firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "build",
				"%s does not support the xhttp transport", "v2ray")
		}

		// Xray XHTTP: path/host carried through xhttpSettings (the
		// v0.4 normalized model maps xhttp onto Path/Host).
		stream.Network = "xhttp"

		xhttp := &v4HTTPTransport{Path: cfg.Path}

		if cfg.Host != "" {
			xhttp.Host = strings.Split(cfg.Host, ",")
		}

		stream.HTTPSettings = xhttp

	default:
		return nil, firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "build", "unsupported transport %q", network)
	}

	return stream, nil
}

// buildV4TLS renders TLS settings. Trojan implies TLS by convention;
// an absent server name falls back to the remote address.
func buildV4TLS(cfg config.Config) *v4TLS {
	tls := &v4TLS{
		ServerName: TLSServerName(cfg),
	}

	if cfg.FingerprintProfile != "" {
		tls.Fingerprint = cfg.FingerprintProfile
	}

	if len(cfg.ALPN) > 0 {
		tls.ALPN = cfg.ALPN
	}

	return tls
}

// NormalizedSecurity maps "" → "none" and defaults trojan to TLS.
func NormalizedSecurity(cfg config.Config) config.Security {
	security := cfg.Security

	if security == "" {
		if cfg.Type == config.TypeTrojan {
			return config.SecurityTLS
		}

		return config.SecurityNone
	}

	return config.Security(security)
}

// NormalizedNetwork maps "" → "tcp" and normalizes http2 aliases.
func NormalizedNetwork(cfg config.Config) string {
	switch cfg.Network {
	case "":
		return string(config.NetworkTCP)

	case "h2":
		return "http"

	default:
		return cfg.Network
	}
}

// realityServerName resolves the REALITY SNI with fallbacks.
func realityServerName(cfg config.Config) string {
	if cfg.ServerName != "" {
		return cfg.ServerName
	}

	return cfg.Address
}

// TLSServerName resolves the TLS SNI with fallbacks.
func TLSServerName(cfg config.Config) string {
	if cfg.ServerName != "" {
		return cfg.ServerName
	}

	return cfg.Address
}

// VLESSEncryption resolves the VLESS encryption field (default
// "none"; the normalized model carries explicit values when the
// source published them).
func VLESSEncryption(cfg config.Config) string {
	if cfg.Encryption != "" {
		return cfg.Encryption
	}

	return "none"
}

// AlterID resolves the VMess alterId (0 for AEAD ciphers, the
// modern default).
func AlterID(cfg config.Config) int {
	if cfg.AlterID > 0 {
		return cfg.AlterID
	}

	return 0
}

// DescribeTransport renders a credential-free transport description.
func DescribeTransport(cfg config.Config) string {
	parts := []string{NormalizedNetwork(cfg)}

	if security := NormalizedSecurity(cfg); security != config.SecurityNone {
		parts = append(parts, string(security))
	}

	return strings.Join(parts, "+")
}
