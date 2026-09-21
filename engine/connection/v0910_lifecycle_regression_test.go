// v0910_lifecycle_regression_test.go proves the v0.9.10
// runtime-context separation at the connection layer.
//
// The v0.9.9 defect: the caller's OPERATION context (bounded attempts,
// 60-second Quick Connect deadlines, request-scoped cancellations)
// flowed unchanged into system.Start → exec.CommandContext, so the
// persistent core process was killed the moment the operation context
// expired or its defer cancel() ran — every successful Quick Connect
// died immediately after success, the monitor reported an "unexpected"
// crash and the recovery loop flapped. The required architecture
// separates:
//
//	operation context  — candidate selection, preparation, startup
//	                     deadline, readiness waiting, verification,
//	                     bounded attempts
//	session context    — persistent core/provider process lifetime,
//	                     cancelled ONLY by disconnect, shutdown,
//	                     session replacement or unrecoverable failure
//
// These tests prove each rule with real processes (the deterministic
// fakecore), not mocks: a successful connection SURVIVES operation
// cancellation, stays alive after Connect returns, dies on explicit
// disconnect, is replaced cleanly by reconnect, cleans up after failed
// startups and crashes, releases the session context at every session
// boundary, and leaves neither processes nor goroutines behind.
//
// v0.9.11: every lifetime assertion uses the CROSS-PLATFORM
// sessionEvidence oracle (kernel pid liveness + listener-serving
// evidence + platform image evidence). The pre-0.9.11 oracle returned
// a fake 0 on Windows, which made the three previously-failing tests
// (TestConnectSurvivesOperationContextCancellation,
// TestConnectSurvivesOperationContextDeadline,
// TestReconnectReplacesPreviousSessionProcess) false-fail on the
// Windows job of CI run 35571120221. The assertions below carry REAL
// evidence on Windows — no skips, no fake values.
package connection_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// v0910Manager builds a manager on the staged fake core with the
// verification gate disabled (these tests prove LIFETIME semantics;
// the verification gate itself is covered by the v0.9.8.3 suite) and
// monitor pacing fast enough for bounded crash observation.
func v0910Manager(t *testing.T, dir string, registry *core.Registry) *connection.Manager {
	t.Helper()

	return connection.New(connection.Options{
		Registry:        registry,
		StartupTimeout:  10 * time.Second,
		GracePeriod:     time.Second,
		MonitorInterval: 100 * time.Millisecond,
		Verify:          connection.VerifyPolicy{Skip: true},
	})
}

// TestConnectSurvivesOperationContextCancellation is THE v0.9.10
// regression: cancelling the operation context that started a
// successful connection must NOT terminate the connection. Pre-0.9.10
// the core was bound to this context through exec.CommandContext and
// died the moment cancel() ran.
func TestConnectSurvivesOperationContextCancellation(t *testing.T) {
	dir, exePath, registry := hungCoreEnv(t)

	manager := v0910Manager(t, dir, registry)
	defer manager.Shutdown()

	opCtx, cancelOp := context.WithCancel(context.Background())

	snapshot, err := manager.Connect(opCtx, vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	if snapshot.State != connection.StateConnected {
		t.Fatalf("state = %s, want connected", snapshot.State)
	}

	ev := sessionEvidenceFrom(t, snapshot)

	pid := snapshot.CorePID

	// The operation context dies — exactly what happens when the
	// service layer's `defer cancel()` runs after Connect returns.
	cancelOp()

	// Give any (wrong) exec.CommandContext binding ample time to kill
	// the process: 750 ms is far beyond the propagation of a context
	// cancellation kill signal.
	time.Sleep(750 * time.Millisecond)

	if state := manager.State(); state != connection.StateConnected {
		t.Fatalf("state after operation-context cancellation = %s, want connected "+
			"(the session must survive its operation context)", state)
	}

	// REAL ownership evidence on every platform: the recorded pid is
	// alive, the listener still serves and the platform image
	// evidence agrees (Linux /proc scan; Windows image lock).
	waitFor(t, 10*time.Second, "the persistent core to stay alive after operation-context cancellation", func() bool {
		return ev.alive(t, dir, exePath)
	})

	if nowPid := manager.Snapshot().CorePID; nowPid != pid {
		t.Fatalf("core pid changed from %d to %d after operation-context cancellation", pid, nowPid)
	}

	// The explicit disconnect is what ends the session.
	manager.Disconnect()

	if state := manager.State(); state != connection.StateDisconnected {
		t.Fatalf("state after disconnect = %s, want disconnected", state)
	}

	ev.deadWait(t, dir, 0, false)

	assertTeardownComplete(t, dir, exePath)

	if ctx := manager.SessionContext(); ctx != nil {
		t.Fatal("session context must be released after disconnect")
	}
}

// TestConnectSurvivesOperationContextDeadline proves the same rule for
// the timeout flavour of the bug: the 60-second Quick Connect attempt
// deadline expiring must not kill an established session.
func TestConnectSurvivesOperationContextDeadline(t *testing.T) {
	dir, exePath, registry := hungCoreEnv(t)

	manager := v0910Manager(t, dir, registry)
	defer manager.Shutdown()

	// A deadline just long enough for the fast fake core to connect.
	opCtx, cancelOp := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancelOp()

	snapshot, err := manager.Connect(opCtx, vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	ev := sessionEvidenceFrom(t, snapshot)

	pid := snapshot.CorePID

	// Wait until the operation deadline is comfortably in the past.
	<-opCtx.Done()
	time.Sleep(750 * time.Millisecond)

	if state := manager.State(); state != connection.StateConnected {
		t.Fatalf("state after operation deadline = %s, want connected", state)
	}

	waitFor(t, 10*time.Second, "the persistent core to stay alive after the operation deadline", func() bool {
		return ev.alive(t, dir, exePath)
	})

	if nowPid := manager.Snapshot().CorePID; nowPid != pid {
		t.Fatalf("core pid changed from %d to %d after the operation deadline", pid, nowPid)
	}

	manager.Disconnect()

	ev.deadWait(t, dir, 0, false)

	assertTeardownComplete(t, dir, exePath)
}

// TestReconnectReplacesPreviousSessionProcess proves clean session
// replacement: after a reconnect the previous core process is gone,
// exactly one new process serves the new session, and nothing leaks.
//
// Evidence is platform-real: the OLD pid reports dead (kernel), the
// NEW session's pid is alive and serves its listener, and on Linux
// the /proc image scan converges to exactly ONE process from the
// staging directory. On Windows the staged image legitimately stays
// locked by the NEW session (same executable path), so replacement
// ownership is proven by the pid transition plus the new session's
// serving evidence.
func TestReconnectReplacesPreviousSessionProcess(t *testing.T) {
	dir, exePath, registry := hungCoreEnv(t)

	manager := v0910Manager(t, dir, registry)
	defer manager.Shutdown()

	first, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("connect #1: %v", err)
	}

	firstEv := sessionEvidenceFrom(t, first)

	second, err := manager.Reconnect(context.Background())
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	if second.State != connection.StateConnected {
		t.Fatalf("reconnected state = %s, want connected", second.State)
	}

	if second.CorePID == 0 || second.CorePID == firstEv.pid {
		t.Fatalf("reconnect pid = %d (previous %d): a NEW process must serve the new session",
			second.CorePID, firstEv.pid)
	}

	secondEv := sessionEvidenceFrom(t, second)

	// The NEW session must be a real, serving process.
	waitFor(t, 10*time.Second, "the replacement session's core to be alive and serving", func() bool {
		return secondEv.alive(t, dir, exePath)
	})

	// The previous session's core must be deterministically gone —
	// not abandoned. On Linux the /proc scan converges to exactly
	// one process (the new session's); on Windows the old pid
	// reports dead (the listener check is skipped there: the
	// replacement may rebind the same port, by design).
	firstEv.deadWait(t, dir, 1, true)

	manager.Disconnect()

	secondEv.deadWait(t, dir, 0, false)

	assertTeardownComplete(t, dir, exePath)
}

// TestOperationCancelledDuringStartupLeavesNoProcess proves the
// inverse rule: an operation that FAILS (its context is cancelled
// before readiness) must tear its process down explicitly — the fix
// moved the process onto the session context, so cancellation alone
// would no longer reap it; the deterministic instance.Close() path
// must.
func TestOperationCancelledDuringStartupLeavesNoProcess(t *testing.T) {
	// A core that never becomes ready keeps the attempt in the
	// startup window until the operation context gives up.
	t.Setenv("FAKECORE_HANG", "1")

	dir, exePath, registry := hungCoreEnv(t)

	before := existingRunConfigs(t)

	manager := v0910Manager(t, dir, registry)
	defer manager.Shutdown()

	opCtx, cancelOp := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancelOp()

	snapshot, err := manager.Connect(opCtx, vlessTestConfig(), core.Preferences{})
	if err == nil {
		t.Fatalf("connect with a hung core unexpectedly succeeded (state %s)", snapshot.State)
	}

	if snapshot.State != connection.StateConnectionFailed {
		t.Fatalf("state = %s, want connection_failed after a cancelled startup", snapshot.State)
	}

	if snapshot.CorePID != 0 {
		t.Fatalf("failed snapshot carries core_pid %d, want 0", snapshot.CorePID)
	}

	// Bounded teardown proof with real platform evidence (Linux:
	// /proc image scan; Windows: image file becomes deletable).
	assertTeardownComplete(t, dir, exePath)

	if leftovers := runConfigLeftovers(t, before); len(leftovers) > 0 {
		t.Fatalf("temporary runtime directories survived the cancelled startup: %v", leftovers)
	}

	if ctx := manager.SessionContext(); ctx != nil {
		t.Fatal("session context must be released after a failed session")
	}
}

// TestCrashEndsSessionAndReleasesRuntimeContext proves that an
// unexpected process exit is a REAL session boundary: the state
// machine reports connection_failed, the process is reaped, and the
// session's runtime context dies with the session (no dangling
// context survives a crashed session).
func TestCrashEndsSessionAndReleasesRuntimeContext(t *testing.T) {
	t.Setenv("FAKECORE_CRASH_AFTER_START", "1")

	dir, exePath, registry := hungCoreEnv(t)

	before := existingRunConfigs(t)

	manager := v0910Manager(t, dir, registry)
	defer manager.Shutdown()

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		if snapshot.State != connection.StateConnectionFailed {
			t.Fatalf("connect state = %s, want connection_failed (crash inside the startup window)", snapshot.State)
		}
	} else {
		// Record the live session's evidence so the crash
		// transition is provable on every platform.
		if ev := sessionEvidenceFrom(t, snapshot); !ev.alive(t, dir, exePath) {
			t.Fatalf("session evidence broken before the crash: pid %d not alive or listener not serving",
				ev.pid)
		}
	}

	waitFor(t, 15*time.Second, "connection_failed after the mid-session crash", func() bool {
		return manager.State() == connection.StateConnectionFailed
	})

	if ctx := manager.SessionContext(); ctx != nil {
		t.Fatal("session context must be released when the session crashes")
	}

	assertTeardownComplete(t, dir, exePath)

	if leftovers := runConfigLeftovers(t, before); len(leftovers) > 0 {
		t.Fatalf("temporary runtime directories survived the crash teardown: %v", leftovers)
	}
}

// TestConnectDisconnectCyclesLeaveNoGoroutineLeaks proves no goroutine
// accumulation across full session cycles (connect → disconnect, plus
// provider sessions) — the monitor, the process-exit watcher and the
// publisher goroutines must all join at every session boundary.
func TestConnectDisconnectCyclesLeaveNoGoroutineLeaks(t *testing.T) {
	dir, exePath, registry := hungCoreEnv(t)

	manager := v0910Manager(t, dir, registry)
	defer manager.Shutdown()

	// Warm-up cycle first: first-time code paths (log buffers, caches)
	// may legitimately allocate one-shot goroutines.
	if _, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{}); err != nil {
		t.Fatalf("warm-up connect: %v", err)
	}

	manager.Disconnect()

	settleGoroutines(t, "after the warm-up cycle")

	baseline := runtime.NumGoroutine()

	var lastEv sessionEvidence

	for cycle := 0; cycle < 3; cycle++ {
		snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
		if err != nil {
			t.Fatalf("cycle %d connect: %v", cycle, err)
		}

		lastEv = sessionEvidenceFrom(t, snapshot)

		manager.Disconnect()
	}

	// Every cycle must return to the baseline goroutine count.
	settleGoroutines(t, "after the connect/disconnect cycles")

	if after := runtime.NumGoroutine(); after > baseline {
		t.Fatalf("goroutines grew from %d to %d across 3 connect/disconnect cycles "+
			"(a session goroutine — monitor, watcher or publisher — leaked)", baseline, after)
	}

	// The last session's process must be gone and its listener closed
	// (real platform evidence; on Linux the /proc scan converges to 0).
	lastEv.deadWait(t, dir, 0, false)

	assertTeardownComplete(t, dir, exePath)
}

// settleGoroutines waits (bounded) for transient goroutines to exit so
// the leak assertion observes steady state, not in-flight teardown.
func settleGoroutines(t *testing.T, where string) {
	t.Helper()

	deadline := time.Now().Add(8 * time.Second)
	stableSince := time.Time{}
	last := -1

	for time.Now().Before(deadline) {
		current := runtime.NumGoroutine()

		if current == last {
			if stableSince.IsZero() {
				stableSince = time.Now()
			} else if time.Since(stableSince) >= 300*time.Millisecond {
				return
			}
		} else {
			stableSince = time.Time{}
			last = current
		}

		time.Sleep(50 * time.Millisecond)
	}
}
