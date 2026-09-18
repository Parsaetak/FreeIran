package tester

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// Probe tests one configuration using a protocol-specific backend.
//
// Implementations will eventually wrap the appropriate local protocol
// core. The tester itself remains independent of the underlying core.
type Probe interface {
	Supports(config.Type) bool
	Test(context.Context, config.Config) (Result, error)
}

// Result contains the outcome of a configuration test.
//
// v0.9.8.1 measurement contract (see latency.go): Latency is the
// true measurement with full precision and is STRICTLY POSITIVE
// whenever Measured is true (successful probes quantize raw zero
// clock readings up to ClockFloor). Measured — not any millisecond
// projection — is the authoritative "was this measured" signal:
// PingMS/DurationMS == 0 with Measured == true means "measured,
// below one millisecond".
type Result struct {
	Working   bool
	Latency   time.Duration
	Measured  bool
	TestedAt  time.Time
	LastError string

	// --- v0.9.0: richer, honest test reporting (§4) ---

	// Backend is the core that executed the test ("xray", "v2ray",
	// "sing-box") or "tcp" for the reachability fallback.
	Backend string

	// Protocol is the configuration's protocol ("vless", ...).
	Protocol string

	// Endpoint is the server address the test targeted
	// (host:port of the remote proxy server, redaction-free).
	Endpoint string

	// PingMS is the measured round-trip time through the tunnel
	// (end-to-end probes only; TCP probe reports its dial RTT).
	// Millisecond projection of Latency: 0 = measured sub-ms
	// (check Measured), never "unmeasured".
	PingMS int64

	// DurationMS is the total wall time of the test (spawn + ready +
	// probe for core tests; dial time for TCP probes). Sub-ms tests
	// project to 0 — a fast result, not a missing one.
	DurationMS int64

	// Quality classifies the measured latency: excellent / good /
	// acceptable / slow / failed. Derived from the measurement, never
	// fabricated.
	Quality string
}

// Latency quality bands (milliseconds). Calibrated for the proxy
// use case: interactive browsing feels instant under 150 ms.
const (
	QualityExcellent  = "excellent"  // <= 150 ms
	QualityGood       = "good"       // <= 400 ms
	QualityAcceptable = "acceptable" // <= 800 ms
	QualitySlow       = "slow"       // <= 2000 ms
	QualityVerySlow   = "very_slow"  // > 2000 ms
	QualityFailed     = "failed"
)

// QualityFor classifies a measured latency in milliseconds.
//
// v0.9.8.1: this legacy entry point receives ONLY values known to be
// measured-and-positive. Callers holding a measured flag must use
// QualityForMeasured (a measured sub-millisecond latency is
// excellent, not failed); callers holding a duration use
// QualityForDuration.
func QualityFor(ms int64) string {
	switch {
	case ms <= 0:
		return QualityFailed
	case ms <= 150:
		return QualityExcellent
	case ms <= 400:
		return QualityGood
	case ms <= 800:
		return QualityAcceptable
	case ms <= 2000:
		return QualitySlow
	default:
		return QualityVerySlow
	}
}

// Tester executes configuration tests through a Probe.
type Tester struct {
	Probe Probe
}

// New creates a Tester using the supplied probe.
func New(probe Probe) *Tester {
	return &Tester{
		Probe: probe,
	}
}

// Test executes one configuration test.
func (t *Tester) Test(
	ctx context.Context,
	cfg config.Config,
) Result {
	now := time.Now().UTC()

	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		return Result{
			Working:   false,
			TestedAt:  now,
			LastError: err.Error(),
		}
	}

	if t == nil || t.Probe == nil {
		return Result{
			Working:   false,
			TestedAt:  now,
			LastError: "tester probe is not configured",
		}
	}

	if !t.Probe.Supports(cfg.Type) {
		return Result{
			Working:  false,
			TestedAt: now,
			LastError: fmt.Sprintf(
				"unsupported protocol: %s",
				cfg.Type,
			),
		}
	}

	result, err := t.Probe.Test(ctx, cfg)
	if err != nil {
		result.Working = false

		if result.TestedAt.IsZero() {
			result.TestedAt = now
		}

		if result.LastError == "" {
			result.LastError = err.Error()
		}

		return result
	}

	if result.TestedAt.IsZero() {
		result.TestedAt = now
	}

	if result.Latency < 0 {
		result.Latency = 0
	}

	return result
}

// ApplyResult writes the test result into the configuration runtime state.
//
// v0.9.3: besides the last-outcome fields, every outcome is appended
// to the bounded observation history (config.TestHistory) — the real
// data the ranking engine scores candidates from.
func ApplyResult(cfg *config.Config, result Result) {
	if cfg == nil {
		return
	}

	cfg.Working = result.Working
	// v0.9.8.1: LatencyMS is a millisecond projection — 0 means
	// measured sub-millisecond when Working is true (rule R5 in
	// latency.go), never "unmeasured".
	cfg.LatencyMS = MSOf(result.Latency)
	cfg.TestedAt = result.TestedAt.UnixMilli()

	// v0.9.0 test metadata (§4).
	cfg.TestBackend = result.Backend
	cfg.TestEndpoint = result.Endpoint
	cfg.TestDurationMS = result.DurationMS

	// v0.9.3: record the bounded history entry. Timeout failures are
	// flagged so ranking can penalise them harder than refusals.
	cfg.AppendTestObservation(config.TestObservation{
		At:        result.TestedAt.UnixMilli(),
		Working:   result.Working,
		LatencyMS: cfg.LatencyMS,
		TimedOut:  !result.Working && isTimeoutError(result.LastError),
		Backend:   result.Backend,
	})
}

// isTimeoutError reports whether a failure message describes a
// timeout (deadline exceeded) rather than a refusal/reset.
func isTimeoutError(message string) bool {
	if message == "" {
		return false
	}

	lower := strings.ToLower(message)

	return strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "timed out") ||
		strings.Contains(lower, "deadline exceeded")
}

// TestAndApply tests a configuration and immediately updates its runtime state.
func (t *Tester) TestAndApply(
	ctx context.Context,
	cfg *config.Config,
) Result {
	if cfg == nil {
		return Result{
			Working:   false,
			TestedAt:  time.Now().UTC(),
			LastError: "configuration is nil",
		}
	}

	result := t.Test(ctx, *cfg)
	ApplyResult(cfg, result)

	return result
}
