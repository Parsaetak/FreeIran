package tester

import (
	"context"
	"fmt"
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
type Result struct {
	Working   bool
	Latency   time.Duration
	TestedAt  time.Time
	LastError string
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
			Working:   false,
			TestedAt:  now,
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
func ApplyResult(cfg *config.Config, result Result) {
	if cfg == nil {
		return
	}

	cfg.Working = result.Working
	cfg.LatencyMS = result.Latency.Milliseconds()
	cfg.TestedAt = result.TestedAt.UnixMilli()
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
