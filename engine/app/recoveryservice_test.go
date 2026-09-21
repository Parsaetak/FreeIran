package app

import (
	"context"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// stageFailedConnection drives the connection manager into the
// ConnectionFailed state by connecting a configuration no core can
// serve (the honest failure path, exactly like a real failure).
func stageFailedConnection(t *testing.T, application *App, name string) string {
	t.Helper()

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	badID := storeConfig(t, application, config.Config{
		Type:    config.TypeWireGuard, // no fake core declares wireguard
		Name:    name,
		Address: "wg.example.org",
		Port:    51820,
	})

	if _, err := service.Connect(badID); err == nil {
		t.Fatal("connect of an unservable config must fail")
	}

	if state := application.connMgr.Snapshot().State; state != "connection_failed" {
		t.Fatalf("state = %s, want connection_failed", state)
	}

	return badID
}

// TestRecoverySwitchesToNextCandidate is the §17 end-to-end contract:
// the active connection fails → recovery classifies, avoids the dead
// candidate, selects the next viable one, connects and verifies.
func TestRecoverySwitchesToNextCandidate(t *testing.T) {
	application := newConnectionTestApp(t)
	defer application.Shutdown()

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	deadID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "was-good", Address: "dead.example.org",
		Port: 443, UUID: "11111111-1111-1111-1111-111111111111",
		Network: "tcp", Security: "tls",
	}, goodHistory(2))

	fallbackID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "fallback", Address: "fb.example.org",
		Port: 443, UUID: "22222222-2222-2222-2222-222222222222",
		Network: "tcp", Security: "tls",
	}, goodHistory(3))

	// The previously active session dies (simulated by a failed
	// connect on an unrelated unservable config — the state machine
	// and the recovery path are identical either way).
	_ = stageFailedConnection(t, application, "unservable")

	// Seed the failure memory exactly as the watch loop would after
	// observing the dead session: the failed candidate gets a cooldown.
	recovery := application.recovery

	now := time.Now().UTC()
	recovery.mu.Lock()
	recovery.failures[deadID] = now // the dead candidate is cooling down
	recovery.mu.Unlock()

	recovery.tick(context.Background(), now)

	snapshot := application.connMgr.Snapshot()

	if snapshot.State != "connected" {
		t.Fatalf("state after recovery tick = %s (%s), want connected",
			snapshot.State, snapshot.LastError)
	}

	if snapshot.ConfigID != fallbackID {
		t.Fatalf("recovered onto %s, want fallback %s", snapshot.ConfigID, fallbackID)
	}

	// The recovery status must reflect success (episode cleared).
	status := recovery.Status()
	if status.EpisodeActive {
		t.Fatalf("episode still active after recovery: %+v", status)
	}

	recovery.mu.Lock()
	cooled := recovery.failures[deadID]
	recovery.mu.Unlock()

	if cooled.IsZero() {
		t.Fatal("dead candidate must remain in failure memory after recovery")
	}

	recovery.Stop()
}

// TestRecoveryBoundedAttempts verifies the hard bounds: with no
// viable alternative, recovery attempts exactly recoveryMaxAttempts
// times with growing backoff and then goes idle — never an infinite
// retry loop.
func TestRecoveryBoundedAttempts(t *testing.T) {
	application := newConnectionTestApp(t)
	defer application.Shutdown()

	_ = stageFailedConnection(t, application, "unservable")

	recovery := application.recovery

	now := time.Now().UTC()

	// Episode 1: three attempts (each fails: nothing viable), with
	// the backoff schedule between them. The tick that finds
	// attempts == maxAttempts declares the episode exhausted.
	recovery.tick(context.Background(), now)

	for attempt := 1; attempt <= recoveryMaxAttempts; attempt++ {
		status := recovery.Status()

		if !status.EpisodeActive || status.Attempts != attempt {
			t.Fatalf("attempt %d: status = %+v", attempt, status)
		}

		if status.NextAttemptInMS <= 0 {
			t.Fatalf("attempt %d must schedule a backoff", attempt)
		}

		// Advance past this attempt's backoff (10s * 2^(attempt-1)).
		now = now.Add(recoveryBackoff*time.Duration(1<<uint(attempt-1)) + time.Second)
		recovery.tick(context.Background(), now)
	}

	// The final tick exhausted the episode: idle now, episode
	// counter recorded.
	status := recovery.Status()
	if status.EpisodeActive {
		t.Fatalf("episode must be exhausted: %+v", status)
	}

	// Phase 2 — the budget allows recoveryMaxEpisodes consecutive
	// exhausted episodes. A tick shortly after exhaustion re-arms
	// (episodes < max), the second episode exhausts the budget.
	now = now.Add(time.Minute)
	recovery.tick(context.Background(), now)

	if status := recovery.Status(); !status.EpisodeActive {
		t.Fatalf("episode 2 must re-arm within the budget: %+v", status)
	}

	for attempt := 1; attempt <= recoveryMaxAttempts; attempt++ {
		status := recovery.Status()

		if status.Attempts != attempt {
			t.Fatalf("episode 2 attempt %d: status = %+v", attempt, status)
		}

		now = now.Add(recoveryBackoff*time.Duration(1<<uint(attempt-1)) + time.Second)
		recovery.tick(context.Background(), now)
	}

	if status := recovery.Status(); status.EpisodeActive {
		t.Fatalf("episode 2 must be exhausted: %+v", status)
	}

	// After the budget is spent: ticks stay idle until the idle
	// reset (or user action).
	now = now.Add(10 * time.Minute)
	recovery.tick(context.Background(), now)

	if status := recovery.Status(); status.EpisodeActive {
		t.Fatalf("recovery re-armed beyond the episode bound: %+v", status)
	}
}

// TestRecoveryDisabledBySettings verifies the explicit opt-out.
func TestRecoveryDisabledBySettings(t *testing.T) {
	application := newConnectionTestApp(t)
	defer application.Shutdown()

	_ = stageFailedConnection(t, application, "unservable")

	recovery := application.recovery

	settings := application.currentSettings()
	settings.DisableAutoRecovery = true

	application.mu.Lock()
	application.settings = settings
	application.mu.Unlock()

	if recovery.Enabled() {
		t.Fatal("recovery must honour the opt-out")
	}

	recovery.tick(context.Background(), time.Now().UTC())

	if status := recovery.Status(); status.EpisodeActive {
		t.Fatalf("disabled recovery must not act: %+v", status)
	}
}

// TestRecoveryExcludesCooledCandidates verifies that a candidate in
// its cooldown window is never re-selected within the same episode.
func TestRecoveryExcludesCooledCandidates(t *testing.T) {
	application := newConnectionTestApp(t)
	defer application.Shutdown()

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	onlyID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "only", Address: "only.example.org",
		Port: 443, UUID: "11111111-1111-1111-1111-111111111111",
		Network: "tcp", Security: "tls",
	}, goodHistory(2))

	_ = stageFailedConnection(t, application, "unservable")

	recovery := application.recovery

	now := time.Now().UTC()

	// The only viable candidate just failed: it must be excluded.
	recovery.mu.Lock()
	recovery.failures[onlyID] = now
	recovery.mu.Unlock()

	recovery.tick(context.Background(), now)

	// Nothing viable → the attempt failed honestly; the episode is
	// active with an error, and the cooled candidate was NOT connected.
	if state := application.connMgr.Snapshot().State; state == "connected" {
		t.Fatal("cooled candidate must not be re-selected during cooldown")
	}

	status := recovery.Status()
	if !status.EpisodeActive || status.LastError == "" {
		t.Fatalf("expected a bounded failing episode: %+v", status)
	}
}
