package tester

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/socks5"
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

// probeEndToEnd measures the real round-trip through the tunnel and
// fetches the target URL through it.
func (p *CoreProbe) probeEndToEnd(ctx context.Context, instance *core.Instance) (time.Duration, time.Duration, error) {
	target := p.E2ETarget

	if target == "" {
		target = DefaultE2ETarget
	}

	budget := p.E2ETimeout

	if budget <= 0 {
		budget = DefaultE2ETimeout
	}

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	endpoint := instance.Endpoint()
	if endpoint == "" {
		return 0, 0, fmt.Errorf("core instance reports no local endpoint")
	}

	parsed, err := url.Parse(target)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid e2e target: %w", err)
	}

	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	dialer := socks5.Dialer{ProxyAddr: endpoint, Timeout: budget}

	// 1. Ping: the SOCKS5 CONNECT round-trip through the tunnel.
	started := time.Now()

	conn, err := dialer.Dial(ctx, "tcp", net.JoinHostPort(parsed.Hostname(), port))
	if err != nil {
		return 0, 0, fmt.Errorf("tunnel connect to %s failed: %w", parsed.Hostname(), err)
	}

	ping := time.Since(started)
	_ = conn.Close()

	// 2. Verify the tunnel forwards real traffic.
	// Disposable per-probe transport (DisableKeepAlives): this probe
	// MEASURES round-trip latency through the tunnel — connection
	// reuse (internal/httpx) would corrupt the measurement. It is a
	// probe, not a download path.
	transport := &http.Transport{
		DialContext:           dialer.Dial,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   budget / 2,
		ResponseHeaderTimeout: budget / 2,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   budget,
	}

	fetchStart := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ping, 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return ping, time.Since(fetchStart), fmt.Errorf("request through tunnel failed: %w", err)
	}

	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode >= 400 {
		return ping, time.Since(fetchStart), fmt.Errorf("request through tunnel returned HTTP %d", resp.StatusCode)
	}

	return ping, time.Since(fetchStart), nil
}

// configEndpoint renders the remote server address for reporting.
func configEndpoint(cfg config.Config) string {
	return net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port))
}
