package tester

import (
	"context"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// CoreProbe tests a configuration by actually executing it through a
// protocol core: capability resolution picks the candidate backend,
// the core is started on an ephemeral local port, readiness is
// observed, and the instance is shut down deterministically. Never
// leaves test instances running.
//
// Bounded fallback: only the best compatible candidate is started;
// fallbacks are tried only when the primary fails validation or
// startup, up to core.DefaultMaxAttempts.
type CoreProbe struct {
	// Registry resolves candidate backends.
	Registry *core.Registry

	// StartupTimeout bounds spawn-to-ready (default 15s).
	StartupTimeout time.Duration

	// Fallback allows secondary backends when the primary fails.
	Fallback bool
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
		result, candidateErr := p.testOne(ctx, candidate, cfg)
		if candidateErr == nil {
			return result, nil
		}

		lastError = candidate.Name() + ": " + candidateErr.Error()

		if ctx.Err() != nil {
			break
		}
	}

	return Result{
		Working:   false,
		TestedAt:  time.Now().UTC(),
		LastError: lastError,
	}, nil
}

// testOne runs the full lifecycle against one backend candidate.
func (p *CoreProbe) testOne(
	ctx context.Context,
	backend core.Core,
	cfg config.Config,
) (Result, error) {
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

	started := time.Now()

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

	// Latency = spawn-to-listener-ready: the full local protocol
	// path through the generated runtime configuration.
	return Result{
		Working:  true,
		Latency:  time.Since(started),
		TestedAt: time.Now().UTC(),
	}, nil
}
