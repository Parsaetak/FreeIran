package tester

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// DefaultE2ETarget is the URL fetched through the tunnel to verify a
// configuration actually forwards traffic. 204 endpoints are ideal:
// tiny, cache-free, run by independent operators.
const DefaultE2ETarget = "https://www.gstatic.com/generate_204"

// DefaultE2ETimeout bounds one end-to-end probe.
const DefaultE2ETimeout = 12 * time.Second

// CoreProbe tests a configuration by actually executing it through a
// protocol core: capability resolution picks the candidate backend,
// the core is started on an ephemeral local port, readiness is
// observed, and the instance is shut down deterministically. Never
// leaves test instances running.
//
// Bounded fallback: only the best compatible candidate is started;
// fallbacks are tried only when the primary fails validation or
// startup, up to core.DefaultMaxAttempts.
//
// v0.9.0: EndToEnd upgrades the readiness-only verdict into a real
// connectivity measurement. When enabled, the probe measures the
// actual round-trip through the generated tunnel (SOCKS5 CONNECT to
// the e2e target) and fetches the target URL through it. The reported
// ping is then a real network latency, not the core's local startup
// time — the specification's "do not fabricate latency" rule.
type CoreProbe struct {
	// Registry resolves candidate backends.
	Registry *core.Registry

	// StartupTimeout bounds spawn-to-ready (default 15s).
	StartupTimeout time.Duration

	// Fallback allows secondary backends when the primary fails.
	Fallback bool

	// EndToEnd verifies the tunnel by fetching a target URL through
	// it and measures the real round-trip. When false the result only
	// proves the core started and its listener is healthy.
	EndToEnd bool

	// E2ETarget is the URL fetched through the tunnel
	// (default DefaultE2ETarget).
	E2ETarget string

	// E2ETimeout bounds the end-to-end probe
	// (default DefaultE2ETimeout).
	E2ETimeout time.Duration
}

// NewCoreProbe creates a probe bound to a core registry.
func NewCoreProbe(registry *core.Registry) *CoreProbe {
	return &CoreProbe{Registry: registry}
}

// Supports reports whether any available backend can execute the
// protocol — cheap capability resolution before a full test.
func (p *CoreProbe) Supports(t config.Type) bool {
	if p == nil || p.Registry == nil {
		return false
	}

	for _, backend := range p.Registry.Backends() {
		if backend.Status != core.StatusAvailable {
			continue
		}

		if c, ok := p.Registry.Get(backend.Name); ok {
			if c.Supports(config.Config{Type: t}) {
				return true
			}
		}
	}

	return false
}

// Test executes one configuration through a protocol core.
//
// Flow (§12): configuration → candidate backend → validate → start
// temporary core → wait for readiness → latency measurement →
// shutdown → result.
func (p *CoreProbe) Test(ctx context.Context, cfg config.Config) (Result, error) {
	if p == nil || p.Registry == nil {
		return Result{
			Working:   false,
			TestedAt:  time.Now().UTC(),
			LastError: "core probe is not bound to a registry",
		}, nil
	}

	testStart := time.Now()

	cfg.Normalize()

	selection, err := p.Registry.Select(cfg, core.Preferences{
		AllowFallback: p.Fallback,
		MaxAttempts:   core.DefaultMaxAttempts,
	})
	if err != nil {
		return Result{
			Working:   false,
			TestedAt:  time.Now().UTC(),
			LastError: "backend selection failed: " + err.Error(),
		}, nil
	}

	candidates := []core.Core{selection.Core}

	if p.Fallback {
		for _, name := range selection.Fallbacks {
			if len(candidates) >= core.DefaultMaxAttempts {
				break
			}

			if fallback, ok := p.Registry.Get(name); ok {
				candidates = append(candidates, fallback)
			}
		}
	}

	var lastError string

	for _, candidate := range candidates {
		result, candidateErr := p.testOne(ctx, candidate, cfg, testStart)
		if candidateErr == nil {
			return result, nil
		}

		lastError = candidate.Name() + ": " + candidateErr.Error()

		if ctx.Err() != nil {
			break
		}
	}

	return Result{
		Working:    false,
		TestedAt:   time.Now().UTC(),
		LastError:  lastError,
		Protocol:   string(cfg.Type),
		Endpoint:   configEndpoint(cfg),
		DurationMS: time.Since(testStart).Milliseconds(),
		Quality:    QualityFailed,
	}, nil
}

// testOne runs the full lifecycle against one backend candidate.
func (p *CoreProbe) testOne(
	ctx context.Context,
	backend core.Core,
	cfg config.Config,
	testStart time.Time,
) (Result, error) {
	endpoint := configEndpoint(cfg)

	if err := backend.Validate(ctx, cfg); err != nil {
		return Result{}, err
	}

	timeout := p.StartupTimeout

	if timeout <= 0 {
		timeout = core.DefaultStartupTimeout
	}

	opts := core.RuntimeOptions{
		BinaryPath:      p.Registry.BinaryPath(backend.Name()),
		StartupTimeout:  timeout,
		DisableGenCache: true,
		// v0.11.0: this launch is a routine test probe — the lifecycle
		// logger keeps it out of the Normal profile (the bulk-test
		// aggregator summarizes the run); failures still log.
		Purpose: core.PurposeProbe,
	}

	instance, err := backend.Start(ctx, cfg, opts)
	if err != nil {
		return Result{}, err
	}

	// Never leave test instances running — regardless of outcome.
	defer func() { _ = instance.Close() }()

	if err := instance.WaitReady(ctx); err != nil {
		return Result{}, err
	}

	health := instance.Health(ctx)
	if !health.ProcessAlive || !health.ListenerReady {
		return Result{
			Working:   false,
			TestedAt:  time.Now().UTC(),
			LastError: "core reports unhealthy: " + health.Details,
		}, nil
	}

	base := Result{
		Working:    true,
		TestedAt:   time.Now().UTC(),
		Backend:    backend.Name(),
		Protocol:   string(cfg.Type),
		Endpoint:   endpoint,
		DurationMS: time.Since(testStart).Milliseconds(),
	}

	// Without EndToEnd, keep the historical semantic: latency =
	// spawn-to-listener-ready (the local protocol path only).
	// v0.9.8.1: the local startup measurement is a real measurement —
	// quantized positive, Measured=true, sub-ms startup classified
	// excellent rather than failed.
	if !p.EndToEnd {
		base.Latency = MeasuredLatency(time.Since(testStart))
		base.Measured = true
		base.PingMS = MSOf(base.Latency)
		base.Quality = QualityForDuration(base.Latency)

		return base, nil
	}

	ping, _, e2eErr := p.probeEndToEnd(ctx, instance)
	if e2eErr != nil {
		return Result{
			Working:    false,
			TestedAt:   time.Now().UTC(),
			Backend:    backend.Name(),
			Protocol:   string(cfg.Type),
			Endpoint:   endpoint,
			DurationMS: time.Since(testStart).Milliseconds(),
			LastError:  e2eErr.Error(),
			Quality:    QualityFailed,
		}, nil
	}

	base.Latency = MeasuredLatency(ping)
	base.Measured = true
	base.PingMS = MSOf(base.Latency)
	base.Quality = QualityForDuration(base.Latency)
	base.DurationMS = time.Since(testStart).Milliseconds()

	return base, nil
}

// probeEndToEnd verifies the tunnel forwards usable traffic through
// the SAME canonical multi-target verification the connection engine
// gates success on (v0.9.8.5 §3: one verification model — the tester
// never carries a second, weaker verdict path). The measured ping is
// the SOCKS CONNECT round-trip; the fetch duration is the winning
// HTTP round-trip through the tunnel.
func (p *CoreProbe) probeEndToEnd(ctx context.Context, instance *core.Instance) (time.Duration, time.Duration, error) {
	budget := p.E2ETimeout

	if budget <= 0 {
		budget = DefaultE2ETimeout
	}

	endpoint := instance.Endpoint()
	if endpoint == "" {
		return 0, 0, errNoEndpoint
	}

	opts := connection.VerifyOptions{Timeout: budget, Backoff: verifyBackoff}

	if p.E2ETarget != "" {
		// A pinned target (tests / explicit configuration) keeps
		// the single-target contract.
		opts.URL = p.E2ETarget
	}

	result := connection.VerifyTunnel(ctx, endpoint, opts)

	if !result.OK {
		if result.Metrics.TotalMS > 0 {
			return time.Duration(result.TunnelProbeMS) * time.Millisecond,
				time.Duration(result.Metrics.TotalMS) * time.Millisecond,
				errVerification{describe: result.Describe()}
		}

		return time.Duration(result.TunnelProbeMS) * time.Millisecond, 0,
			errVerification{describe: result.Describe()}
	}

	ping := time.Duration(result.TunnelProbeMS) * time.Millisecond
	fetch := time.Duration(result.Metrics.TotalMS) * time.Millisecond

	return ping, fetch, nil
}

// errVerification carries a credential-free verification failure.
type errVerification struct {
	describe string
}

func (e errVerification) Error() string { return e.describe }

// errNoEndpoint is the deterministic no-endpoint failure.
var errNoEndpoint = errorString("core instance reports no local endpoint")

type errorString string

func (e errorString) Error() string { return string(e) }

// verifyBackoff keeps the tester's transient retry tight: testing
// throughput matters more than waiting out third-party blips.
const verifyBackoff = 150 * time.Millisecond

// configEndpoint renders the remote server address for reporting.
func configEndpoint(cfg config.Config) string {
	return net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port))
}
