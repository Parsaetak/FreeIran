// Package v2ray implements the V2Ray Core backend adapter for the
// actively maintained V2Fly edition (github.com/v2fly/v2ray-core).
//
// V2Ray is a first-class runtime, NOT a synonym for Xray: the two
// projects share the V4 JSON configuration lineage but have diverged
// — V2Ray retains the classic transports (tcp/ws/grpc/http/quic)
// while Xray removed plain QUIC/HTTP in favour of XHTTP and added
// REALITY and the xtls-rprx-vision flow, which V2Ray does not
// implement. The capability declarations in this package encode the
// verified V2Ray feature set (see engine/core/versions.go).
package v2ray

import (
	"context"
	"fmt"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Subsystem identifies the V2Ray backend in structured errors.
const Subsystem = "core.v2ray"

// Backend is the V2Ray Core adapter.
type Backend struct {
	genCache *core.GenCache
}

// New creates a V2Ray backend adapter.
func New() *Backend {
	return &Backend{genCache: core.NewGenCache()}
}

// Name returns the canonical backend identifier.
func (b *Backend) Name() string { return "v2ray" }

// Capabilities declares the verified V2Ray feature set. Verified
// against v2ray-core v5.53.0 (V2Fly): every listed protocol,
// transport and security combination passes `v2ray test` and the
// startup smoke test; REALITY and VLESS flow values are absent from
// the V2Fly codebase and therefore rejected.
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
			"tcp", "ws", "grpc", "http", "quic",
		},
		Securities: []string{"none", "tls"},
		Flows:      []string{""},
		Notes: []string{
			"V2Fly edition (v2fly/v2ray-core), v5 series",
			"no REALITY support (Xray-only feature)",
			"no xtls-rprx-vision flow (Xray-only feature)",
		},
	}
}

// Supports reports whether V2Ray can execute the configuration.
func (b *Backend) Supports(cfg config.Config) bool {
	return b.Capabilities().Matches(cfg)
}

// Validate performs deep validation against V2Ray capabilities.
func (b *Backend) Validate(_ context.Context, cfg config.Config) error {
	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		return firerrors.Wrap(err, firerrors.KindInvalidInput,
			Subsystem, "validate", "configuration rejected")
	}

	caps := b.Capabilities()

	if !caps.Matches(cfg) {
		return capabilityError("v2ray", cfg, caps)
	}

	if cfg.Flow != "" {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"V2Ray does not support VLESS flow %q (Xray or sing-box required)",
			cfg.Flow)
	}

	return nil
}

// capabilityError renders a precise capability mismatch.
func capabilityError(backend string, cfg config.Config, caps core.Capabilities) error {
	switch {
	case !caps.SupportsProtocol(cfg.Type):
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"%s does not support protocol %s (supported: %s)",
			backend, cfg.Type, joinTypes(caps.Protocols))

	case !supportsTransportOrPlain(caps, cfg.Network):
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"%s does not support transport %q (supported: %s)",
			backend, cfg.Network, strings.Join(caps.Transports, "/"))

	case !caps.SupportsSecurity(cfg.Security):
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"%s does not support security %q (supported: %s)",
			backend, cfg.Security, strings.Join(caps.Securities, "/"))

	default:
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"%s capability mismatch for %s", backend, cfg.DisplayURL())
	}
}

// BuildConfig converts a normalized configuration into a V4-format
// V2Ray runtime document. Generation is deterministic: identical
// inputs produce byte-identical documents (pinned by tests).
func (b *Backend) BuildConfig(cfg config.Config, opts core.RuntimeOptions) (core.RuntimeConfig, error) {
	return b.buildDocument(cfg, opts)
}

// buildDocument resolves the generation cache then generates.
func (b *Backend) buildDocument(cfg config.Config, opts core.RuntimeOptions) (core.RuntimeConfig, error) {
	cfg.Normalize()

	cacheKey := core.GenCacheKey(cfg.Fingerprint(), "v2ray")
	generation := core.GenerationFor("v2ray", opts.BinaryPath, opts)

	if !opts.DisableGenCache {
		if doc, ok := b.genCache.Get(cacheKey, generation); ok {
			return doc, nil
		}
	}

	data, summary, err := BuildV4Document(cfg, opts, V4Options{})
	if err != nil {
		return core.RuntimeConfig{}, err
	}

	doc := core.RuntimeConfig{
		FileName:        fmt.Sprintf("v2ray-%s.json", shortFingerprint(cfg)),
		Data:            data,
		RedactedSummary: summary,
		Format:          "v4",
	}

	if !opts.DisableGenCache {
		b.genCache.Put(cacheKey, generation, doc)
	}

	return doc, nil
}

// Start builds the runtime configuration and launches the V2Ray
// process through the shared core launcher.
func (b *Backend) Start(
	ctx context.Context,
	cfg config.Config,
	opts core.RuntimeOptions,
) (*core.Instance, error) {
	if opts.BinaryPath == "" {
		return nil, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "start",
			"v2ray executable path is not resolved (registry discovery required)")
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

	doc, err := b.buildDocument(cfg, opts)
	if err != nil {
		return nil, err
	}

	return core.Launch(ctx, b, cfg, opts, doc, v2rayArgs)
}

// v2rayArgs renders the v2ray command line: `run -c <file>`.
func v2rayArgs(configFile string, _ int) []string {
	return []string{"run", "-c", configFile}
}

// shortFingerprint renders an 8-character fingerprint prefix for
// file names (never a credential).
func shortFingerprint(cfg config.Config) string {
	fp := cfg.Fingerprint()

	if len(fp) > 8 {
		return fp[:8]
	}

	return fp
}

// joinTypes renders a type list for error messages.
func joinTypes(types []config.Type) string {
	values := make([]string, 0, len(types))

	for _, t := range types {
		values = append(values, string(t))
	}

	return strings.Join(values, "/")
}

// supportsTransportOrPlain treats "" as plain TCP.
func supportsTransportOrPlain(caps core.Capabilities, network string) bool {
	if network == "" {
		network = string(config.NetworkTCP)
	}

	for _, t := range caps.Transports {
		if t == network {
			return true
		}
	}

	return false
}
