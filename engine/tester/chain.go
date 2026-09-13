package tester

import (
	"context"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// ChainedProbe tries probes in order; the FIRST probe whose Supports()
// accepts the configuration executes the test and its verdict is
// final. Later probes act as universal fallbacks for protocol classes
// the earlier ones cannot attempt (e.g. no core installed: the core
// probe declines, the TCP reachability probe answers instead).
//
// The verdict of a probe that ATTEMPTED a test is never overridden by
// a weaker probe: a core test that failed end-to-end stays failed even
// though a raw TCP dial to the same endpoint might succeed — reporting
// "working" for a dead proxy would be exactly the dishonesty v0.9.0
// removes.
type ChainedProbe struct {
	Probes []Probe
}

// NewChainedProbe composes probes in priority order.
func NewChainedProbe(probes ...Probe) *ChainedProbe {
	return &ChainedProbe{Probes: probes}
}

// Supports reports whether ANY chained probe can attempt the type.
func (p *ChainedProbe) Supports(t config.Type) bool {
	if p == nil {
		return false
	}

	for _, probe := range p.Probes {
		if probe.Supports(t) {
			return true
		}
	}

	return false
}

// Test runs the first accepting probe.
func (p *ChainedProbe) Test(ctx context.Context, cfg config.Config) (Result, error) {
	if p == nil || len(p.Probes) == 0 {
		return Result{
			Working:   false,
			TestedAt:  time.Now().UTC(),
			LastError: "no probe configured",
		}, nil
	}

	for _, probe := range p.Probes {
		if !probe.Supports(cfg.Type) {
			continue
		}

		return probe.Test(ctx, cfg)
	}

	return Result{
		Working:   false,
		TestedAt:  time.Now().UTC(),
		LastError: "no probe supports protocol: " + string(cfg.Type),
	}, nil
}
