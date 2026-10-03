package freecore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/version"
)

// Backend adapts the FreeIran Engine to the ONE core execution
// boundary: it is a registered core.Core whose Start runs the engine
// IN-PROCESS (no managed process, no executable, no second
// supervisor). Selection, preference, fallback, the connection state
// machine and verification treat it exactly like any other backend —
// that is the architectural rule (docs/architecture.md v0.13.1).
type Backend struct {
	genCache *core.GenCache
}

// New creates the first-party engine backend.
func New() *Backend {
	return &Backend{genCache: core.NewGenCache()}
}

// Name returns the canonical backend identifier. The registry,
// selection and snapshots carry this id; user-facing surfaces render
// DisplayName.
func (b *Backend) Name() string { return Subsystem }

// Capabilities declares the first-party engine's VERIFIED feature
// set. This is the honest gate: everything outside it is refused by
// Supports/Validate and continues through the external compatibility
// cores. Declared facts are backed by the loopback integration suite
// (real bytes, cancellation, timeout, refusal coverage) — the same
// evidence discipline docs/protocols.md records per core.
func (b *Backend) Capabilities() core.Capabilities {
	return core.Capabilities{
		Protocols: []config.Type{
			config.TypeSOCKS,
			config.TypeHTTP,
		},
		Transports: []string{"tcp"},
		Securities: []string{"none"},
		Notes: []string{
			"first-party in-process engine: no external binary, no child process",
			"mixed local inbound serves SOCKS5 and HTTP CONNECT on one port",
			"SOCKS5 remote auth: RFC 1929 username/password",
			"HTTP CONNECT remote auth: Proxy-Authorization basic",
			"plain TCP without transport security only (Phase 1)",
			"proxy chains are not compiled in-engine (Phase 2 item 13)",
		},
	}
}

// Availability reports the in-process engine's static availability:
// it is compiled in, so there is no executable to discover. This
// implements the registry's StaticAvailability seam (v0.13.1).
func (b *Backend) Availability() (status core.BackendStatus, ver, path, note, origin, ownership string) {
	return core.StatusAvailable, version.Version, "",
		"in-process engine (compiled in; no external binary)", "builtin", "first-party"
}

// Supports reports whether the first-party engine can execute the
// configuration. Deterministic and side-effect free: the declared
// capability set decides, nothing else.
func (b *Backend) Supports(cfg config.Config) bool {
	if cfg.IsChain() {
		return false
	}

	return b.Capabilities().Matches(cfg)
}

// Validate performs the deep first-party check: capability match plus
// the normalization rules (address, port, auth shape).
func (b *Backend) Validate(_ context.Context, cfg config.Config) error {
	if !b.Supports(cfg) {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "validate",
			"FreeIran Engine does not support %s (protocol %s, network %q, security %q)",
			cfg.DisplayURL(), cfg.Type, cfg.Network, cfg.Security)
	}

	if _, err := Normalize(cfg); err != nil {
		return err
	}

	return nil
}

// sessionDescriptor is the deterministic runtime document the engine
// persists as its RunConfig artifact (the diagnostics-facing record
// of what a first-party session runs — the same role a generated
// sing-box/Xray document plays for the external cores).
type sessionDescriptor struct {
	Engine   string `json:"engine"`
	Version  string `json:"version"`
	Outbound string `json:"outbound"`
	Remote   string `json:"remote"`
	Auth     bool   `json:"auth"`
	Network  string `json:"network"`
	Security string `json:"security"`
}

// BuildConfig converts a normalized configuration into the engine's
// deterministic session descriptor. Generation is deterministic:
// identical inputs produce identical bytes (the generated-config
// cache contract every backend honors).
func (b *Backend) BuildConfig(cfg config.Config, opts core.RuntimeOptions) (core.RuntimeConfig, error) {
	route, err := Normalize(cfg)
	if err != nil {
		return core.RuntimeConfig{}, err
	}

	opts = opts.WithDefaults()

	cacheKey := core.GenCacheKey(cfg.Fingerprint(), Subsystem, opts.LocalHost, opts.LocalPort, opts.HTTPPort)
	generation := core.GenerationFor(Subsystem, "", version.Version)

	if !opts.DisableGenCache {
		if doc, ok := b.genCache.Get(cacheKey, generation); ok {
			return doc, nil
		}
	}

	descriptor := sessionDescriptor{
		Engine:   DisplayName,
		Version:  version.Version,
		Outbound: string(route.Outbound),
		Remote:   route.Endpoint.String(),
		Auth:     route.AuthRequired,
		Network:  string(route.Network),
		Security: string(route.Security),
	}

	data, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return core.RuntimeConfig{}, firerrors.Wrap(err, firerrors.KindInvalidInput,
			Subsystem, "build_config", "encode session descriptor")
	}

	doc := core.RuntimeConfig{
		FileName: fmt.Sprintf("freecore-%s.json", shortFingerprint(cfg)),
		Data:     data,
		RedactedSummary: fmt.Sprintf("FreeIran Engine session: %s via %s (auth=%t)",
			route.Endpoint, route.Outbound, route.AuthRequired),
		Format: "freecore",
	}

	if !opts.DisableGenCache {
		b.genCache.Put(cacheKey, generation, doc)
	}

	return doc, nil
}

// Start runs the engine in-process and returns the supervised
// instance. The listeners are bound synchronously BEFORE the instance
// exists, so a bind failure is an honest Start error; readiness is
// then observed through the same shared launch verdict every backend
// uses (core.LaunchInProcess → awaitListener).
func (b *Backend) Start(
	ctx context.Context,
	cfg config.Config,
	opts core.RuntimeOptions,
) (*core.Instance, error) {
	opts = opts.WithDefaults()

	route, err := Normalize(cfg)
	if err != nil {
		return nil, err
	}

	// The inbound port is resolved through the ONE authoritative
	// execution-stage resolver (v0.9.9 contract), exactly like the
	// external adapters.
	port, err := core.ResolveInboundPort(opts.LocalHost, opts.LocalPort)
	if err != nil {
		return nil, err
	}

	opts.LocalPort = port

	doc, err := b.BuildConfig(cfg, opts)
	if err != nil {
		return nil, err
	}

	engine, err := NewEngine(Options{
		Route:     route,
		LocalHost: opts.LocalHost,
		LocalPort: port,
		HTTPPort:  opts.HTTPPort,
	})
	if err != nil {
		return nil, err
	}

	// Bind + serve synchronously relative to instance creation: Run
	// binds before returning, so a port conflict fails Start here.
	if err := engine.Run(ctx); err != nil {
		return nil, err
	}

	listen := engine.Endpoint()

	instance, err := core.LaunchInProcess(ctx, b, cfg, opts, doc, listen, engine)
	if err != nil {
		engine.Stop(0)

		return nil, err
	}

	return instance, nil
}

// shortFingerprint renders an 8-character fingerprint prefix (the
// shared runtime-file naming convention).
func shortFingerprint(cfg config.Config) string {
	fp := cfg.Fingerprint()

	if len(fp) > 8 {
		return fp[:8]
	}

	return fp
}
