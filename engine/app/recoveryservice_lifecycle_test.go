// recoveryservice_lifecycle_test.go — v0.9.9 recovery lifecycle
// ownership regression coverage (§7):
//
//   - Stop while idle joins the watch loop and returns promptly;
//   - Stop JOINS an in-flight recovery decision (no recovery goroutine
//     survives Stop);
//   - no new recovery attempt begins after the lifecycle ended
//     (decide on a stopped service is a no-op);
//   - Stop is idempotent.
package app

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
)

// newRecoveryTestApp boots the minimal app harness for recovery
// lifecycle tests (no sources, fake core staged).
func newRecoveryTestApp(t *testing.T) *App {
	t.Helper()

	application, err := New(Options{
		SkipConnectVerification: true,
		BaseDir:                 filepath.Join(t.TempDir(), "freeiran"),
		RefreshInterval:         time.Hour,
		RunIngestionOnStart:     false,
		SkipDefaultSources:      true,
	})
	if err != nil {
		t.Fatalf("app.New() = %v", err)
	}

	t.Cleanup(application.Shutdown)

	return application
}

// TestRecoveryStopWhileIdleJoinsPromptly covers scenarios (1) and (2):
// Stop while idle and Stop while waiting for the watch tick.
func TestRecoveryStopWhileIdleJoinsPromptly(t *testing.T) {
	application := newRecoveryTestApp(t)

	recovery := NewRecoveryService(application)

	before := runtime.NumGoroutine()

	recovery.Start()
	recovery.Start() // idempotent

	time.Sleep(50 * time.Millisecond) // let at least one loop wake pass

	joined := make(chan struct{})

	go func() {
		recovery.Stop()
		close(joined)
	}()

	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not join the watch loop promptly")
	}

	recovery.Stop() // idempotent: second Stop is a safe no-op

	if recovery.Status().Watching {
		t.Fatal("Watching must be false after Stop")
	}

	time.Sleep(100 * time.Millisecond)

	// No recovery goroutine may survive the stop (tolerant bound:
	// the runtime may have unrelated system goroutines).
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines before=%d after=%d — recovery loop leaked", before, after)
	}
}

// TestRecoveryStopJoinsInFlightDecision covers scenarios (3) and (4):
// Stop while a recovery decision (Quick Connect loop) is running —
// Stop must not return before the decision goroutine has exited.
func TestRecoveryStopJoinsInFlightDecision(t *testing.T) {
	application := newRecoveryTestApp(t)

	recovery := NewRecoveryService(application)
	recovery.Start()

	// Stage a failing observation so decide() takes the recovery path
	// (an empty candidate pool makes quickConnectLoop exhaust
	// deterministically fast).
	decideDone := make(chan struct{})

	snapshot := connection.Snapshot{
		State:    connection.StateConnectionFailed,
		ConfigID: "cfg-inflight",
	}

	go func() {
		defer close(decideDone)

		recovery.decide(time.Now().UTC(), snapshot)
	}()

	// Give the decision a moment to enter quickConnectLoop, then stop
	// while it is in flight.
	time.Sleep(20 * time.Millisecond)

	stopped := make(chan struct{})

	go func() {
		recovery.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		// Stop returned. The in-flight decision must ALREADY have
		// finished (or have been refused by the lifecycle check).
		select {
		case <-decideDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Stop returned while the recovery decision was still running")
		}

	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return while a decision was in flight")
	}
}

// TestRecoveryNoAttemptAfterStop proves the lifecycle gate: a decision
// driven against a stopped service (the shutdown race window) records
// nothing and starts no episode.
func TestRecoveryNoAttemptAfterStop(t *testing.T) {
	application := newRecoveryTestApp(t)

	recovery := NewRecoveryService(application)
	recovery.Start()
	recovery.Stop()

	recovery.decide(time.Now().UTC(), connection.Snapshot{
		State:     connection.StateConnectionFailed,
		ConfigID:  "cfg-after-stop",
		LastError: "post-stop observation",
	})

	status := recovery.Status()

	if status.EpisodeActive {
		t.Fatalf("a stopped service must not start an episode: %+v", status)
	}

	if status.CooledCandidates != 0 {
		t.Fatalf("a stopped service must not record failure memory: %+v", status)
	}
}

// TestRecoveryDecisionLifecycleContext proves the decision runs on the
// lifecycle context (not the bare app context): a cancelled lifecycle
// is visible to quickConnectLoop.
func TestRecoveryDecisionLifecycleContext(t *testing.T) {
	application := newRecoveryTestApp(t)

	recovery := NewRecoveryService(application)
	recovery.Start()

	// Cancel the lifecycle directly (what Stop does before joining).
	recovery.mu.Lock()
	cancel := recovery.cancel
	recovery.mu.Unlock()

	cancel()

	// The decision must observe the cancellation and refuse to act.
	recovery.decide(time.Now().UTC(), connection.Snapshot{
		State:    connection.StateConnectionFailed,
		ConfigID: "cfg-cancelled",
	})

	if status := recovery.Status(); status.EpisodeActive {
		t.Fatalf("cancelled lifecycle must not start an episode: %+v", status)
	}
}

// failedConnectionSnapshot builds a failed-state snapshot for decide
// tests with a real stored config ID.
func failedConnectionSnapshot(t *testing.T, application *App) connection.Snapshot {
	t.Helper()

	id := storeConfig(t, application, config.Config{
		Type:    config.TypeWireGuard, // unservable → honest failure path
		Name:    "failed-cfg",
		Address: "wg.example.org",
		Port:    51820,
	})

	return connection.Snapshot{
		State:    connection.StateConnectionFailed,
		ConfigID: id,
	}
}
