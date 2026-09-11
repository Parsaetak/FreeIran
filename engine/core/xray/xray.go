// Package xray implements the Xray Core backend adapter
// (github.com/XTLS/Xray-core).
//
// Xray understands the V4 JSON format shared with V2Ray and extends
// it with REALITY, the xtls-rprx-vision VLESS flow and the XHTTP
// transport. Plain QUIC and HTTP/2 transports were REMOVED in
// current Xray releases (migrated onto XHTTP) — the capability
// declaration encodes that verified divergence, so backend
// selection routes such configurations to V2Ray or sing-box.
package xray

import (
	"context"
	"fmt"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Subsystem identifies the Xray backend in structured errors.
const Subsystem = "core.xray"

// Backend is the Xray Core adapter.
type Backend struct {
	genCache *core.GenCache
}

// New creates an Xray backend adapter.
func New() *Backend {
	return &Backend{genCache: core.NewGenCache()}
}

// Name returns the canonical backend identifier.
func (b *Backend) Name() string { return "xray" }

// Capabilities declares the verified Xray feature set. Verified
// against Xray-core v26.3.27: REALITY and xtls-rprx-vision pass
// `xray run -test`; plain "quic"/"http" transports are rejected by
// the same binary ("feature removed ... migrated to XHTTP").
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
			"tcp", "ws", "grpc", "xhttp",
		},
		Securities: []string{"none", "tls", "reality"},
		Flows:      []string{"", "xtls-rprx-vision"},
		Notes: []string{
			"REALITY supported",
			"xtls-rprx-vision flow supported",
			"plain QUIC/HTTP2 transports removed in current releases (XHTTP replaces them)",
		},
	}
}

// Supports reports whether Xray can execute the configuration.
func (b *Backend) Supports(cfg config.Config) bool {
	return b.Capabilities().Matches(cfg)
}

// Validate performs deep validation against Xray capabilities.
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

	// REALITY requires a public key; vision flow requires TCP.
	if v2ray.NormalizedSecurity(cfg) == config.SecurityReality &&
		strings.TrimSpace(cfg.PublicKey) == "" {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"REALITY configuration requires a public key (pbk)")
	}

	if cfg.Flow == "xtls-rprx-vision" {
		network := v2ray.NormalizedNetwork(cfg)

		if network != string(config.NetworkTCP) {
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
			"xray does not support protocol %s", cfg.Type)

	case !caps.SupportsTransport(cfg.Network):
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"xray does not support transport %q (supported: %s; plain quic/http were removed in favour of xhttp)",
			cfg.Network, strings.Join(caps.Transports, "/"))

	case !caps.SupportsSecurity(cfg.Security):
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"xray does not support security %q (supported: %s)",
			cfg.Security, strings.Join(caps.Securities, "/"))

	default:
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"xray capability mismatch for %s", cfg.DisplayURL())
	}
}

// xrayV4Options enables the Xray dialect of the shared V4 builder.
var xrayV4Options = v2ray.V4Options{
	AllowReality: true,
	AllowFlow:    true,
	AllowXHTTP:   true,
	BackendName:  "xray",
}

// BuildConfig converts a normalized configuration into an Xray
// runtime document (V4 format with Xray extensions).
func (b *Backend) BuildConfig(cfg config.Config, opts core.RuntimeOptions) (core.RuntimeConfig, error) {
	cfg.Normalize()

	cacheKey := core.GenCacheKey(cfg.Fingerprint(), "xray")
	generation := core.GenerationFor("xray", opts.BinaryPath, opts)

	if !opts.DisableGenCache {
		if doc, ok := b.genCache.Get(cacheKey, generation); ok {
			return doc, nil
		}
	}

	data, summary, err := v2ray.BuildV4Document(cfg, opts, xrayV4Options)
	if err != nil {
		return core.RuntimeConfig{}, firerrors.Wrap(err, firerrors.KindInvalidInput,
			Subsystem, "build", "xray document generation failed")
	}

	doc := core.RuntimeConfig{
		FileName:        fmt.Sprintf("xray-%s.json", shortFingerprint(cfg)),
		Data:            data,
		RedactedSummary: summary,
		Format:          "v4",
	}

	if !opts.DisableGenCache {
		b.genCache.Put(cacheKey, generation, doc)
	}

	return doc, nil
}

// Start builds the runtime configuration and launches Xray through
// the shared core launcher.
func (b *Backend) Start(
	ctx context.Context,
	cfg config.Config,
	opts core.RuntimeOptions,
) (*core.Instance, error) {
	if opts.BinaryPath == "" {
		return nil, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "start",
			"xray executable path is not resolved (registry discovery required)")
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

	return core.Launch(ctx, b, cfg, opts, doc, xrayArgs)
}

// xrayArgs renders the xray command line: `run -c <file>`.
func xrayArgs(configFile string, _ int) []string {
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
