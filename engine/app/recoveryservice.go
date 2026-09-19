// recoveryservice.go implements the MONITOR/RECOVER/LEARN stages of
// the autonomous connection engine: when the active connection dies,
// FreeIran classifies the failure, avoids recently failed candidates,
// switches to the next viable one, verifies it, and records the
// outcome — with HARD bounds so recovery can never degenerate into
// infinite retries or rapid reconnect loops.
//
// Guarantees (per the v0.9.3 recovery contract):
//
//   - bounded attempts per episode (3, with 10s/20s/40s backoff);
//   - bounded consecutive exhausted episodes (2) — after that
//     auto-recovery stays quiet for the idle-reset window (30 min),
//     which re-arms the budget once (so an overnight outage can still
//     self-heal from refreshed rankings, without any retry loop);
//   - a per-candidate cooldown (5 min) backed by failure memory, so
//     a dead candidate is not retried within the same episode;
//   - every transition is logged with its reason; user data is never
//     touched; credential redaction applies to all log lines.
package app

import (
	"fmt"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// Recovery tuning constants. Plain numbers, one place, documented.
const (
	// recoveryWatchInterval paces the state watch.
	recoveryWatchInterval = 5 * time.Second

	// recoveryMaxAttempts bounds one recovery episode.
	recoveryMaxAttempts = 3

	// recoveryBackoff is the pre-attempt wait schedule (doubles per
	// attempt: 10s → 20s → 40s).
	recoveryBackoff = 10 * time.Second

	// candidateCooldown keeps a failed candidate out of selection.
	candidateCooldown = 5 * time.Minute

	// recoveryMaxEpisodes bounds consecutive exhausted episodes
	// before auto-recovery goes idle until manual user action.
	recoveryMaxEpisodes = 2

	// episodeIdleReset clears the consecutive-episode counter after
	// this much time without a failing episode.
	episodeIdleReset = 30 * time.Minute
)

// RecoveryStatus is the credential-free recovery view for the
// developer diagnostics surface.
type RecoveryStatus struct {
	Enabled          bool   `json:"enabled"`
	Watching         bool   `json:"watching"`
	EpisodeActive    bool   `json:"episode_active"`
	EpisodeNumber    int    `json:"episode_number"`
	Attempts         int    `json:"attempts"`
	MaxAttempts      int    `json:"max_attempts"`
	CooledCandidates int    `json:"cooled_candidates"`
	LastError        string `json:"last_error,omitempty"`
	NextAttemptInMS  int64  `json:"next_attempt_in_ms,omitempty"`
}

// recoveryEpisode tracks one bounded recovery sequence.
type recoveryEpisode struct {
	number      int
	startedAt   time.Time
	attempts    int
	lastError   string
	nextAttempt time.Time
}

// RecoveryService watches the connection state machine and runs the
// bounded automatic-recovery policy.
type RecoveryService struct {
	app *App

	mu          sync.Mutex
	watching    bool
	stop        chan struct{}
	episodes    int                  // consecutive exhausted episodes
	lastEpisode time.Time            // when the last episode ended
	failures    map[string]time.Time // fingerprint → last failure
	episode     *recoveryEpisode
}

// NewRecoveryService binds the recovery supervisor to the app.
func NewRecoveryService(a *App) *RecoveryService {
	return &RecoveryService{
		app:      a,
		failures: map[string]time.Time{},
	}
}

// Enabled reports whether automatic recovery is on (default on; the
// user opt-out is Settings.DisableAutoRecovery).
func (r *RecoveryService) Enabled() bool {
	return !r.app.currentSettings().DisableAutoRecovery
}

// Start launches the recovery watch loop (idempotent). The stop
// channel is handed to the loop as a parameter so the goroutine never
// reads the struct field while Stop closes it (race-free by design).
func (r *RecoveryService) Start() {
	r.mu.Lock()

	if r.watching {
		r.mu.Unlock()
		return
	}

	r.watching = true
	stop := make(chan struct{})
	r.stop = stop

	r.mu.Unlock()

	go r.loop(stop)
}

// Stop cancels the watch loop (idempotent).
func (r *RecoveryService) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.watching {
		return
	}

	r.watching = false
	close(r.stop)
}

// loop is the watch tick: observe, decide, act — never block the
// connection state machine longer than one Connect attempt.
func (r *RecoveryService) loop(stop <-chan struct{}) {
	ticker := time.NewTicker(recoveryWatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			r.tick(time.Now().UTC())
		}
	}
}

// tick runs one recovery decision against the live connection
// snapshot.
func (r *RecoveryService) tick(now time.Time) {
	r.decide(now, r.app.connMgr.Snapshot())
}

// decide is the recovery decision for one observed snapshot. It is
// split from tick so the per-state policy table (v0.9.8.4 §6) is
// regression-testable deterministically for every connection state —
// including the transient ones (verifying, disconnecting) that a live
// watch can only race past.
func (r *RecoveryService) decide(now time.Time, snapshot connection.Snapshot) {
	if !r.Enabled() {
		return
	}

	r.mu.Lock()

	// Expire cooldowns lazily.
	for fp, at := range r.failures {
		if now.Sub(at) >= candidateCooldown {
			delete(r.failures, fp)
		}
	}

	// Idle reset: long stretches without an episode forgive the
	// consecutive-episode budget.
	if !r.lastEpisode.IsZero() && now.Sub(r.lastEpisode) >= episodeIdleReset {
		r.episodes = 0
		r.lastEpisode = time.Time{}
	}

	switch snapshot.State {
	case connection.StateConnectedVerified, connection.StateDisconnected:
		// Healthy (or idle by user choice): clear any episode.
		//
		// v0.9.8.4: only connected_verified counts as healthy here.
		// The plain connected state means the route is established
		// while verification is still pending — treating it as
		// healthy falsely presented readiness as verified Internet
		// and cleared a recovery episode before the verification
		// gate had spoken. It now falls through to the in-progress
		// branch below (recovery never acts on it; it interferes
		// only with explicit failures).
		if r.episode != nil {
			r.app.logger.Info("recovery", "episode_cleared",
				"connection is %s; recovery episode cleared", snapshot.State)
			r.episode = nil
		}

		r.mu.Unlock()
		return

	case connection.StateConnectionFailed:
		// The recovery path acts on explicit failure only.

	default:
		// Selecting/Preparing/StartingCore/WaitingForReady/Connected/
		// Verifying/Disconnecting: in progress — do not interfere.
		//
		// v0.9.8.4: `connected` deliberately sits here (not in the
		// healthy branch): the route is established but the
		// verification gate has not spoken yet, so an episode must
		// neither be cleared nor advanced on its account. If the
		// verification succeeds, the state becomes connected_verified
		// (episode cleared); if it fails, the machine reports
		// connection_failed and the bounded episode continues.
		r.mu.Unlock()
		return
	}

	// Record the failed candidate (LEARN): the config that just died
	// gets a cooldown regardless of which stage failed.
	if snapshot.ConfigID != "" {
		r.failures[snapshot.ConfigID] = now
	}

	// Episode management.
	if r.episode == nil {
		if r.episodes >= recoveryMaxEpisodes {
			r.mu.Unlock()
			return // user action required; do not loop forever
		}

		r.episode = &recoveryEpisode{
			number:      r.episodes + 1,
			startedAt:   now,
			nextAttempt: now,
		}

		r.app.logger.Warn("recovery", "episode_start",
			"connection failed (%s); starting recovery episode %d of %d",
			snapshot.LastError, r.episode.number, recoveryMaxEpisodes)
	}

	if now.Before(r.episode.nextAttempt) {
		r.mu.Unlock()
		return
	}

	if r.episode.attempts >= recoveryMaxAttempts {
		// Episode exhausted.
		r.episodes = r.episode.number
		r.lastEpisode = now
		r.episode = nil

		r.app.logger.Warn("recovery", "episode_exhausted",
			"recovery gave up after %d attempts; auto-recovery stays quiet "+
				"until the user connects manually or the %d-minute idle reset",
			recoveryMaxAttempts, int(episodeIdleReset.Minutes()))

		r.mu.Unlock()
		return
	}

	// Reset the state machine so Connect() is legal: the manager is
	// in ConnectionFailed (terminal), which its own Connect path
	// accepts as a fresh start.
	r.episode.attempts++
	attempt := r.episode.attempts

	excluded := r.excludedLocked()

	r.mu.Unlock()

	// SELECT + CONNECT: the shared fresh-selection loop (v0.9.8.3)
	// — recovery re-tests stale candidates, re-ranks, connects and
	// verifies through the exact same path as Quick Connect. The
	// failure memory applies as exclusions.
	result, err := r.app.quickConnectLoop(r.app.ctx, excluded, 0)

	r.mu.Lock()
	defer r.mu.Unlock()

	if err != nil {
		r.episode.lastError = err.Error()
		r.episode.nextAttempt = now.Add(
			recoveryBackoff * time.Duration(1<<uint(attempt-1)))

		r.app.logger.Warn("recovery", "attempt_failed",
			"recovery attempt %d/%d failed: %s (next in %s)",
			attempt, recoveryMaxAttempts, err.Error(),
			time.Until(r.episode.nextAttempt).Round(time.Second))

		return
	}

	// CONNECTED: final success requires a VERIFIED session
	// (v0.9.8.3: the loop already gates on Internet verification;
	// the state must agree). Verification-disabled harnesses
	// (tests) report the skip-mode state honestly.
	verified := result.Snapshot.Verification == "usable" ||
		r.app.opts.SkipConnectVerification

	if result.Snapshot.State.ConnectedLike() && verified {
		r.episodes = 0
		r.episode = nil

		// v0.9.7: the recovery report separates local core readiness
		// from verified connectivity (the pre-0.9.7 line reported the
		// loopback probe as "latency", typically "0 ms") and names the
		// candidate/backend in the right order.
		verification := result.Snapshot.Verification
		if verification == "" {
			verification = "none"
		}

		r.app.logger.Log(logging.Record{
			Level:      logging.LevelInfo,
			Subsystem:  "recovery",
			Event:      "recovered",
			ConfigID:   result.Snapshot.ConfigID,
			Core:       result.Snapshot.Core,
			Listener:   result.Snapshot.Endpoint,
			DurationMS: result.Snapshot.CoreReadyMS,
			Status:     verification,
			Message: fmt.Sprintf("recovered on candidate %s via %s (core ready in %d ms, verification %s)",
				result.Chosen.Name, result.Snapshot.Core,
				result.Snapshot.CoreReadyMS, verification),
		})

		return
	}

	r.episode.lastError = "post-switch verification failed (state " +
		string(result.Snapshot.State) + ")"

	r.episode.nextAttempt = now.Add(
		recoveryBackoff * time.Duration(1<<uint(attempt-1)))
}

// noteExternalFailure records a candidate failure reported by another
// subsystem (Quick Connect) so recovery's exclusion set stays in sync.
func (r *RecoveryService) noteExternalFailure(fingerprint string) {
	if fingerprint == "" {
		return
	}

	r.mu.Lock()
	r.failures[fingerprint] = time.Now().UTC()
	r.mu.Unlock()
}

// excludedLocked renders the current failure memory as an exclusion
// list (caller holds r.mu).
func (r *RecoveryService) excludedLocked() []string {
	excluded := make([]string, 0, len(r.failures))

	for fp := range r.failures {
		excluded = append(excluded, fp)
	}

	return excluded
}

// Status renders the recovery state for developer diagnostics.
func (r *RecoveryService) Status() RecoveryStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	status := RecoveryStatus{
		Enabled:          r.Enabled(),
		Watching:         r.watching,
		MaxAttempts:      recoveryMaxAttempts,
		CooledCandidates: len(r.failures),
	}

	if r.episode != nil {
		status.EpisodeActive = true
		status.EpisodeNumber = r.episode.number
		status.Attempts = r.episode.attempts
		status.LastError = r.episode.lastError

		if wait := time.Until(r.episode.nextAttempt); wait > 0 {
			status.NextAttemptInMS = wait.Milliseconds()
		}
	}

	return status
}
