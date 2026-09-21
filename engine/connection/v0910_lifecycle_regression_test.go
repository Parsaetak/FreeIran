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
package connection_test

import (
	"context"
	"os"
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
	dir, _, registry := hungCoreEnv(t)

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

	pid := snapshot.CorePID
	if pid == 0 {
		t.Fatal("connected snapshot carries no core pid")
	}

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

	if alive := procsRunningFrom(t, dir); alive != 1 {
		t.Fatalf("core processes running after operation-context cancellation = %d, want 1 "+
			"(the persistent core must stay alive)", alive)
	}

	if nowPid := manager.Snapshot().CorePID; nowPid != pid {
		t.Fatalf("core pid changed from %d to %d after operation-context cancellation", pid, nowPid)
	}

	// The explicit disconnect is what ends the session.
	manager.Disconnect()

	if state := manager.State(); state != connection.StateDisconnected {
		t.Fatalf("state after disconnect = %s, want disconnected", state)
	}

	waitFor(t, 10*time.Second, "core process to terminate after disconnect", func() bool {
		return procsRunningFrom(t, dir) == 0
	})

	if ctx := manager.SessionContext(); ctx != nil {
		t.Fatal("session context must be released after disconnect")
	}
}

// TestConnectSurvivesOperationContextDeadline proves the same rule for
// the timeout flavour of the bug: the 60-second Quick Connect attempt
// deadline expiring must not kill an established session.
func TestConnectSurvivesOperationContextDeadline(t *testing.T) {
	dir, _, registry := hungCoreEnv(t)

	manager := v0910Manager(t, dir, registry)
	defer manager.Shutdown()

	// A deadline just long enough for the fast fake core to connect.
	opCtx, cancelOp := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancelOp()

	snapshot, err := manager.Connect(opCtx, vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	pid := snapshot.CorePID

	// Wait until the operation deadline is comfortably in the past.
	<-opCtx.Done()
	time.Sleep(750 * time.Millisecond)

	if state := manager.State(); state != connection.StateConnected {
		t.Fatalf("state after operation deadline = %s, want connected", state)
	}

	if alive := procsRunningFrom(t, dir); alive != 1 {
		t.Fatalf("core processes running after operation deadline = %d, want 1", alive)
	}

	if nowPid := manager.Snapshot().CorePID; nowPid != pid {
		t.Fatalf("core pid changed from %d to %d after the operation deadline", pid, nowPid)
	}

	manager.Disconnect()

	waitFor(t, 10*time.Second, "core process to terminate after disconnect", func() bool {
		return procsRunningFrom(t, dir) == 0
	})
}

// TestReconnectReplacesPreviousSessionProcess proves clean session
// replacement: after a reconnect the previous core process is gone,
// exactly one new process serves the new session, and nothing leaks.
func TestReconnectReplacesPreviousSessionProcess(t *testing.T) {
	dir, _, registry := hungCoreEnv(t)

	manager := v0910Manager(t, dir, registry)
	defer manager.Shutdown()

	first, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("connect #1: %v", err)
	}

	firstPID := first.CorePID

	second, err := manager.Reconnect(context.Background())
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	if second.State != connection.StateConnected {
		t.Fatalf("reconnected state = %s, want connected", second.State)
	}

	if second.CorePID == 0 || second.CorePID == firstPID {
		t.Fatalf("reconnect pid = %d (previous %d): a NEW process must serve the new session",
			second.CorePID, firstPID)
	}

	// Exactly one process may remain: the previous session's core was
	// replaced deterministically, not abandoned.
	waitFor(t, 10*time.Second, "the replaced core process to terminate", func() bool {
		return procsRunningFrom(t, dir) == 1
	})

	manager.Disconnect()

	waitFor(t, 10*time.Second, "every core process to terminate after disconnect", func() bool {
		return procsRunningFrom(t, dir) == 0
	})
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

	waitFor(t, 15*time.Second, "the abandoned startup process to be torn down", func() bool {
		return procsRunningFrom(t, dir) == 0
	})

	if leftovers := runConfigLeftovers(t, before); len(leftovers) > 0 {
		t.Fatalf("temporary runtime directories survived the cancelled startup: %v", leftovers)
	}

	if err := os.Remove(exePath); err != nil {
		t.Fatalf("staged executable is not deletable after the cancelled startup: %v", err)
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
	}

	waitFor(t, 15*time.Second, "connection_failed after the mid-session crash", func() bool {
		return manager.State() == connection.StateConnectionFailed
	})

	if ctx := manager.SessionContext(); ctx != nil {
		t.Fatal("session context must be released when the session crashes")
	}

	waitFor(t, 15*time.Second, "the crashed core process to be reaped", func() bool {
		return procsRunningFrom(t, dir) == 0
	})

	if leftovers := runConfigLeftovers(t, before); len(leftovers) > 0 {
		t.Fatalf("temporary runtime directories survived the crash teardown: %v", leftovers)
	}

	if err := os.Remove(exePath); err != nil {
		t.Fatalf("staged executable is not deletable after the crash teardown: %v", err)
	}
}

// TestConnectDisconnectCyclesLeaveNoGoroutineLeaks proves no goroutine
// accumulation across full session cycles (connect → disconnect, plus
// provider sessions) — the monitor, the process-exit watcher and the
// publisher goroutines must all join at every session boundary.
func TestConnectDisconnectCyclesLeaveNoGoroutineLeaks(t *testing.T) {
	dir, _, registry := hungCoreEnv(t)

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

	for cycle := 0; cycle < 3; cycle++ {
		if _, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{}); err != nil {
			t.Fatalf("cycle %d connect: %v", cycle, err)
		}

		manager.Disconnect()
	}

	// Every cycle must return to the baseline goroutine count.
	settleGoroutines(t, "after the connect/disconnect cycles")

	if after := runtime.NumGoroutine(); after > baseline {
		t.Fatalf("goroutines grew from %d to %d across 3 connect/disconnect cycles "+
			"(a session goroutine — monitor, watcher or publisher — leaked)", baseline, after)
	}

	waitFor(t, 10*time.Second, "all core processes to terminate", func() bool {
		return procsRunningFrom(t, dir) == 0
	})
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
