package connection

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// racing.go implements controlled connection racing (v0.9.6 §13):
// when the environment justifies it, the engine races the top N
// ranked candidates in parallel — every candidate independently gets
// a temporary core instance, and the FIRST connection that proves
// VERIFIED USABLE connectivity wins. Losing attempts are cancelled
// cleanly and their instances are shut down deterministically.
//
// Racing is deliberately opt-in and bounded:
//
//   - only 2-4 racers ever run (clamped from RaceOptions.Racers);
//   - every racer has its own cancellation derived from the parent;
//   - resource limits are explicit: one core process per racer for
//     the race duration only;
//   - the winner's CONFIG (not process) is returned — the manager
//     performs the definitive Connect, so the production session
//     lifecycle is unchanged and single-owner;
//   - a racer that fails verification records its classified failure
//     and stops: no repeated attempts inside a race.
//
// When NOT to race (caller policy): small candidate pools, tight
// resource budgets, or environments where spawning concurrent cores
// is risky. The engine exposes the capability; the strategy layer
// decides when it is justified.

// RaceOptions configure one race.
type RaceOptions struct {
	// Candidates are the ordered candidates (best first). The race
	// uses at most the first `Racers` of them.
	Candidates []config.Config

	// Racers is how many candidates race in parallel (clamped 2-4;
	// default 2).
	Racers int

	// Verify tunes the per-candidate verification.
	Verify VerifyOptions

	// Registry resolves protocol cores (required).
	Registry *core.Registry

	// StartupTimeout bounds each racer's core startup
	// (default core.DefaultStartupTimeout).
	StartupTimeout time.Duration

	// Env carries additional environment variables for racer core
	// processes (test fixture injection). Never derived from
	// untrusted source data.
	Env []string
}

// RaceAttempt records one racer's outcome.
type RaceAttempt struct {
	ConfigFingerprint string       `json:"config_fingerprint"`
	ConfigName        string       `json:"config_name,omitempty"`
	OK                bool         `json:"ok"`
	FailureClass      FailureClass `json:"failure_class,omitempty"`
	TunnelProbeMS     int64        `json:"tunnel_probe_ms,omitempty"`
	VerifyTotalMS     int64        `json:"verify_total_ms,omitempty"`
	Error             string       `json:"error,omitempty"`
	DurationMS        int64        `json:"duration_ms"`
}

// RaceOutcome is the result of one race.
type RaceOutcome struct {
	// Winner is the verified-usable candidate (nil when all failed).
	Winner *config.Config

	// WinnerVerify carries the winning verification measurements.
	WinnerVerify VerifyResult

	// Attempts records every racer (winner included).
	Attempts []RaceAttempt

	// DurationMS is the wall-clock duration of the race.
	DurationMS int64
}

// ErrNoRacerCandidates is returned when a race is requested with
// fewer than two usable candidates.
var ErrNoRacerCandidates = fmt.Errorf("connection: race needs at least one candidate")

// ErrRaceRegistryMissing is returned when the registry is absent.
var ErrRaceRegistryMissing = fmt.Errorf("connection: race requires a core registry")

// Race races the top candidates and returns the first verified-usable
// one. A race with zero candidates fails fast; with one candidate it
// degenerates to a single verified attempt (still useful: verify
// before committing).
func Race(ctx context.Context, opts RaceOptions) (*RaceOutcome, error) {
	started := time.Now()

	if opts.Registry == nil {
		return nil, ErrRaceRegistryMissing
	}

	if len(opts.Candidates) == 0 {
		return nil, ErrNoRacerCandidates
	}

	racers := opts.Racers
	if racers < 2 {
		racers = 2
	}

	if racers > 4 {
		racers = 4
	}

	if racers > len(opts.Candidates) {
		racers = len(opts.Candidates)
	}

	// The outcome aggregates every racer's attempt.
	outcome := &RaceOutcome{}

	type racerResult struct {
		index   int
		attempt RaceAttempt
		cfg     *config.Config
		verify  VerifyResult
	}

	results := make(chan racerResult, racers)

	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	var once sync.Once

	for i := 0; i < racers; i++ {
		cfg := opts.Candidates[i]

		cfg.Normalize()
		cfg.SetID()

		go func(idx int, cfg config.Config) {
			racerStart := time.Now()

			attempt, verify := raceOne(raceCtx, cfg, opts)

			attempt.DurationMS = time.Since(racerStart).Milliseconds()

			results <- racerResult{index: idx, attempt: attempt, cfg: &cfg, verify: verify}
		}(i, cfg)
	}

	for i := 0; i < racers; i++ {
		select {
		case <-ctx.Done():
			// Parent cancelled: drain what already finished (bounded).
			raceCancel()

			select {
			case r := <-results:
				outcome.Attempts = append(outcome.Attempts, r.attempt)
			default:
			}

			outcome.DurationMS = time.Since(started).Milliseconds()

			return outcome, ctx.Err()

		case r := <-results:
			outcome.Attempts = append(outcome.Attempts, r.attempt)

			if r.attempt.OK && outcome.Winner == nil {
				winner := r.cfg
				outcome.Winner = winner
				outcome.WinnerVerify = r.verify

				// First verified usable connection wins: cancel the
				// remaining racers. once guards against a double
				// cancel racing the deferred cancel.
				once.Do(raceCancel)
			}
		}
	}

	// Drain any stragglers (a racer that finished after the winner
	// but before observing cancellation).
	for {
		select {
		case r := <-results:
			outcome.Attempts = append(outcome.Attempts, r.attempt)
		default:
			outcome.DurationMS = time.Since(started).Milliseconds()

			if outcome.Winner == nil {
				return outcome, fmt.Errorf(
					"connection: race exhausted: no candidate verified usable (%d racers)",
					racers)
			}

			return outcome, nil
		}
	}
}

// raceOne runs one candidate through start → ready → verify and
// ALWAYS closes its temporary instance.
func raceOne(ctx context.Context, cfg config.Config, opts RaceOptions) (RaceAttempt, VerifyResult) {
	attempt := RaceAttempt{
		ConfigFingerprint: cfg.Fingerprint(),
		ConfigName:        cfg.Name,
	}

	selection, err := opts.Registry.Select(cfg, core.Preferences{
		AllowFallback: true,
		MaxAttempts:   core.DefaultMaxAttempts,
	})
	if err != nil {
		attempt.Error = "backend selection: " + err.Error()

		return attempt, VerifyResult{}
	}

	backend := selection.Core

	if err := backend.Validate(ctx, cfg); err != nil {
		attempt.Error = "validation: " + err.Error()

		return attempt, VerifyResult{}
	}

	startup := opts.StartupTimeout
	if startup <= 0 {
		startup = core.DefaultStartupTimeout
	}

	instance, err := backend.Start(ctx, cfg, core.RuntimeOptions{
		BinaryPath:      opts.Registry.BinaryPath(backend.Name()),
		StartupTimeout:  startup,
		DisableGenCache: true,
		Env:             opts.Env,
	})
	if err != nil {
		attempt.Error = "core start: " + err.Error()

		return attempt, VerifyResult{}
	}

	// Deterministic cleanup regardless of outcome.
	defer func() { _ = instance.Close() }()

	if err := instance.WaitReady(ctx); err != nil {
		attempt.FailureClass = FailureCore
		attempt.Error = "core not ready: " + err.Error()

		return attempt, VerifyResult{}
	}

	verify := VerifyTunnel(ctx, instance.Endpoint(), opts.Verify)

	attempt.OK = verify.OK
	attempt.FailureClass = verify.FailureClass
	attempt.TunnelProbeMS = verify.TunnelProbeMS
	attempt.VerifyTotalMS = verify.Metrics.TotalMS

	if !verify.OK {
		attempt.Error = verify.Describe()
	}

	return attempt, verify
}
