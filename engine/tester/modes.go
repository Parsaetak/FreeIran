package tester

import (
	"context"
	"fmt"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// modes.go implements the user-selectable test modes (v0.9.6 §9):
//
//      Ping               — repeated TCP samples against the endpoint
//      URL                — real HTTP request through the tunnel
//      Ping + URL         — both, aggregated
//      Protocol Handshake — core spawn → listener-ready timing
//      Full Connectivity  — handshake + ping + URL (verify usable)
//
// Every mode measures its own facet and records it into the canonical
// Config metrics fields; nothing is inferred from a mode that did not
// run. The verdict of each mode is independent: a candidate can have
// a brilliant ping (the path is fast) and still fail the URL test
// (the tunnel does not forward) — both facts stay visible instead of
// collapsing into one number.

// Mode is the user-selectable test mode.
type Mode string

const (
	// ModePing: TCP ping samples only. Cheapest; no core started.
	ModePing Mode = "ping"

	// ModeURL: HTTP connectivity through the tunnel only.
	ModeURL Mode = "url"

	// ModePingURL: ping and URL.
	ModePingURL Mode = "ping_url"

	// ModeHandshake: protocol-core handshake only (spawn → ready).
	ModeHandshake Mode = "handshake"

	// ModeFull: handshake + ping + URL — the complete verification.
	ModeFull Mode = "full"
)

// AllModes lists the modes the UI offers.
var AllModes = []Mode{ModePing, ModeURL, ModePingURL, ModeHandshake, ModeFull}

// ModeLabel returns the human label of a mode.
func ModeLabel(m Mode) string {
	switch m {
	case ModePing:
		return "Ping"
	case ModeURL:
		return "URL"
	case ModePingURL:
		return "Ping + URL"
	case ModeHandshake:
		return "Protocol Handshake"
	case ModeFull:
		return "Full Connectivity"
	default:
		return string(m)
	}
}

// ModeOptions tunes a mode-driven test run. Defaults are safe and
// lightweight (§9): 4 ping samples, standard timeouts, the standard
// test URL.
type ModeOptions struct {
	Mode Mode

	// PingSamples / PingTimeout / PingInterval configure the ping
	// facet (0 = defaults from PingProbe).
	PingSamples  int
	PingTimeout  time.Duration
	PingInterval time.Duration

	// URL is the connectivity-verification target
	// ("" = DefaultURLTestTarget).
	URL string

	// URLTimeout bounds the URL facet (0 = DefaultURLTestTimeout).
	URLTimeout time.Duration
}

// normalize fills defaults.
func (o ModeOptions) normalize() ModeOptions {
	if o.Mode == "" {
		o.Mode = ModePingURL
	}

	return o
}

// ModeOutcome is the aggregated result of one mode-driven test.
type ModeOutcome struct {
	Mode      Mode
	TestedAt  time.Time
	VerdictOK bool

	// Ping is set when the mode measured ping (nil otherwise).
	Ping *config.PingMetrics

	// URLTest is set when the mode measured URL connectivity.
	URLTest *config.URLTestMetrics

	// Handshake is set when the mode measured the core handshake.
	Handshake *config.HandshakeMetrics

	// WorkingBackwards-compatible verdict: for ping-only mode it is
	// "endpoint responds"; for any mode with URL it is "tunnel
	// forwards usable traffic"; for handshake-only it is "core
	// started and became ready".
	Working bool

	// Latency is the best latency the executed facets produced
	// (URL total > ping median); zero when nothing succeeded. It
	// feeds the legacy Result surface, never the display of a facet
	// that did not run.
	Latency time.Duration

	// LastError is the classified failure of the last failing facet
	// (empty when everything ran clean).
	LastError string

	// Backend is the core that executed the tunnel facets.
	Backend string
}

// ModeTester executes user-selected test modes against candidates.
type ModeTester struct {
	// Registry resolves protocol cores for the tunnel facets
	// (URL / Handshake / Full). Ping-only mode works without it.
	Registry *core.Registry

	// Options carry the user's mode selection and its tuning.
	Options ModeOptions

	// v0.9.9: the receiver carries no mutable state — Test is safe
	// for concurrent use with one shared instance.
}

// NewModeTester creates a mode-driven tester bound to a registry.
func NewModeTester(registry *core.Registry, opts ModeOptions) *ModeTester {
	opts = opts.normalize()

	return &ModeTester{Registry: registry, Options: opts}
}

// Test runs the selected mode against one candidate and returns the
// aggregated outcome. Cancellation propagates; partial measurements
// survive in the outcome.
func (t *ModeTester) Test(ctx context.Context, cfg config.Config) ModeOutcome {
	// v0.9.9: the per-call probe is a LOCAL, not a struct field. It
	// is rebuilt on every call and never reused, and storing it into
	// the shared receiver made concurrent Test calls (the Quick
	// Connect fresh-testing worker pool) race on the field.
	pinger := &PingProbe{
		Samples:  t.Options.PingSamples,
		Timeout:  t.Options.PingTimeout,
		Interval: t.Options.PingInterval,
	}

	out := ModeOutcome{
		Mode:     t.Options.Mode,
		TestedAt: time.Now().UTC(),
	}

	runPing := t.Options.Mode == ModePing || t.Options.Mode == ModePingURL || t.Options.Mode == ModeFull
	runURL := t.Options.Mode == ModeURL || t.Options.Mode == ModePingURL || t.Options.Mode == ModeFull
	runHandshake := t.Options.Mode == ModeHandshake || t.Options.Mode == ModeFull

	cfg.Normalize()

	// --- Ping facet: no core required ---
	if runPing {
		ping, err := pinger.Ping(ctx, cfg.Address, cfg.Port)

		pingCopy := ping
		out.Ping = &pingCopy

		if err != nil && ctx.Err() != nil {
			out.LastError = "ping cancelled"

			return out
		}

		if ping.Samples > 0 {
			out.Latency = time.Duration(ping.MedianMS) * time.Millisecond
		} else {
			out.LastError = fmt.Sprintf("ping: 0/%d samples succeeded (%d timeouts)", ping.Failures, ping.Timeouts)
		}
	}

	// --- Tunnel facets: core lifecycle required ---
	if (runURL || runHandshake) && ctx.Err() == nil {
		tunnelOK := t.runTunnelFacets(ctx, cfg, runURL, runHandshake, &out)
		if !tunnelOK && ctx.Err() != nil {
			return out
		}
	}

	out.Working = modeVerdict(t.Options.Mode, out)
	out.VerdictOK = out.Working

	if out.Working {
		out.LastError = ""
	}

	return out
}

// runTunnelFacets starts a temporary core for the candidate and runs
// the requested tunnel facets (handshake timing and/or URL test)
// through it. The instance is always closed.
func (t *ModeTester) runTunnelFacets(
	ctx context.Context,
	cfg config.Config,
	runURL, runHandshake bool,
	out *ModeOutcome,
) bool {
	if t == nil || t.Registry == nil {
		out.LastError = "mode tester is not bound to a core registry"

		return false
	}

	selection, err := t.Registry.Select(cfg, core.Preferences{AllowFallback: true, MaxAttempts: core.DefaultMaxAttempts})
	if err != nil {
		out.LastError = "backend selection failed: " + err.Error()

		return false
	}

	backend := selection.Core
	out.Backend = backend.Name()

	if err := backend.Validate(ctx, cfg); err != nil {
		out.LastError = "backend rejected configuration: " + err.Error()

		return false
	}

	opts := core.RuntimeOptions{
		BinaryPath:      t.Registry.BinaryPath(backend.Name()),
		StartupTimeout:  core.DefaultStartupTimeout,
		DisableGenCache: true,
	}

	spawnStart := time.Now()

	instance, err := backend.Start(ctx, cfg, opts)
	if err != nil {
		out.LastError = "core start failed: " + err.Error()

		return false
	}

	// Never leave test instances running — regardless of outcome.
	defer func() { _ = instance.Close() }()

	readyErr := instance.WaitReady(ctx)

	// v0.9.8.1: capture the true spawn-to-ready duration once — the
	// old code re-projected it through ReadyMS (milliseconds), which
	// truncated sub-millisecond startups to a zero duration.
	readyDur := MeasuredLatency(time.Since(spawnStart))

	handshake := config.HandshakeMetrics{
		ReadyMS: readyDur.Milliseconds(),
		OK:      readyErr == nil,
		At:      time.Now().UTC().UnixMilli(),
	}

	if runHandshake {
		handshakeCopy := handshake
		out.Handshake = &handshakeCopy
	}

	if readyErr != nil {
		out.LastError = "core did not become ready: " + readyErr.Error()

		return false
	}

	if !runURL {
		// Handshake-only: the verdict is readiness. v0.9.8.1: use the
		// captured true duration (sub-ms startups no longer collapse
		// to zero through the ReadyMS projection).
		out.Latency = readyDur

		return true
	}

	// --- URL facet through the tunnel ---
	endpoint := instance.Endpoint()
	if endpoint == "" {
		out.LastError = "core instance reports no local endpoint"

		return false
	}

	dialer := &socks5.Dialer{ProxyAddr: endpoint, Timeout: urlTimeoutOf(t.Options)}

	// v0.9.9: per-call local (was a shared receiver field — the same
	// concurrent-Test race the ping probe had).
	urler := &URLTester{Timeout: t.Options.URLTimeout}

	// Probe latency through the tunnel (SOCKS CONNECT RTT) — the
	// honest end-to-end ping for URL-driven modes.
	probeStart := time.Now()

	probeConn, dialErr := dialer.Dial(ctx, "tcp", hostPortOf(urlTargetOf(t.Options)))

	probeRTT := time.Since(probeStart)

	if probeConn != nil {
		_ = probeConn.Close()
	}

	if dialErr == nil {
		// v0.9.8.1: a successful tunnel probe is a real measurement;
		// quantize coarse-clock zeros before the ms projection.
		probeRTT = MeasuredLatency(probeRTT)
		handshake.ProbeMS = probeRTT.Milliseconds()

		if out.Handshake != nil {
			out.Handshake.ProbeMS = handshake.ProbeMS
		}
	}

	metrics := urler.Test(ctx, dialer.Dial, urlTargetOf(t.Options))

	out.URLTest = &metrics

	if metrics.OK {
		// The tunnel forwards usable traffic: record the freshest
		// honest latency from the facets that actually ran. All
		// branches keep the canonical v0.9.8.1 representation:
		// strictly positive for a measured success (sub-ms projected
		// values are carried by Working + a zero LatencyMS).
		if out.Ping != nil && out.Ping.Samples > 0 {
			out.Latency = MeasuredLatency(time.Duration(out.Ping.MedianMS) * time.Millisecond)
		} else if dialErr == nil {
			out.Latency = probeRTT
		} else {
			out.Latency = MeasuredLatency(time.Duration(metrics.TotalMS) * time.Millisecond)
		}

		return true
	}

	out.LastError = "url test failed: " + metrics.Error

	return false
}

// modeVerdict decides the overall working flag per mode semantics.
func modeVerdict(m Mode, out ModeOutcome) bool {
	switch m {
	case ModePing:
		return out.Ping != nil && out.Ping.Samples > 0
	case ModeURL:
		return out.URLTest != nil && out.URLTest.OK
	case ModePingURL:
		return out.Ping != nil && out.Ping.Samples > 0 && out.URLTest != nil && out.URLTest.OK
	case ModeHandshake:
		return out.Handshake != nil && out.Handshake.OK
	case ModeFull:
		return out.Handshake != nil && out.Handshake.OK &&
			out.URLTest != nil && out.URLTest.OK
	default:
		return false
	}
}

func urlTargetOf(o ModeOptions) string {
	if o.URL != "" {
		return o.URL
	}

	return DefaultURLTestTarget
}

func urlTimeoutOf(o ModeOptions) time.Duration {
	if o.URLTimeout > 0 {
		return o.URLTimeout
	}

	return DefaultURLTestTimeout
}

// ApplyModeOutcome writes a mode outcome into the candidate's runtime
// state: the facet metrics land in their canonical fields, the
// success/failure streaks update, and one bounded history observation
// is appended so ranking scores remain grounded in real outcomes.
func ApplyModeOutcome(cfg *config.Config, out ModeOutcome) {
	if cfg == nil {
		return
	}

	if out.Ping != nil {
		cfg.Ping = out.Ping
	}

	if out.URLTest != nil {
		cfg.URLTest = out.URLTest
	}

	if out.Handshake != nil {
		cfg.Handshake = out.Handshake
	}

	cfg.Working = out.Working
	cfg.TestedAt = out.TestedAt.UnixMilli()
	cfg.TestBackend = out.Backend

	if out.Working {
		cfg.LastSuccessAt = out.TestedAt.UnixMilli()
		cfg.FailureStreak = 0
		cfg.LastFailureReason = ""
		cfg.LatencyMS = out.Latency.Milliseconds()
	} else {
		cfg.FailureStreak++
		cfg.LastFailureReason = out.LastError
	}

	cfg.AppendTestObservation(config.TestObservation{
		At:        out.TestedAt.UnixMilli(),
		Working:   out.Working,
		LatencyMS: out.Latency.Milliseconds(),
		TimedOut:  !out.Working && isTimeoutOutcome(out),
		Backend:   out.Backend,
	})
}

func isTimeoutOutcome(out ModeOutcome) bool {
	if out.URLTest != nil && out.URLTest.Timeout {
		return true
	}

	if out.Ping != nil && out.Ping.Timeouts > 0 && out.Ping.Samples == 0 {
		return true
	}

	return false
}

// TestAndApplyMode runs one mode test and applies it in place.
func (t *ModeTester) TestAndApplyMode(ctx context.Context, cfg *config.Config) ModeOutcome {
	if cfg == nil {
		return ModeOutcome{Mode: t.Options.Mode, TestedAt: time.Now().UTC()}
	}

	out := t.Test(ctx, *cfg)
	ApplyModeOutcome(cfg, out)

	return out
}
