package connection_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/system"
)

// generation_test.go pins the v0.9.8.6 session-generation and
// deterministic-teardown contract:
//
//   - a stability teardown COMPLETES before connection_failed becomes
//     observable (process exited, executable deletable, directory
//     removable) — the v0.9.8.5 Windows CI failure where v2ray.exe
//     outlived the test and TempDir cleanup died with "Access is
//     denied";
//   - disconnect / reconnect / shutdown DURING an in-flight
//     verification discards the stale result — a stale failure can
//     never poison a newer session;
//   - the monitor join means returning from Disconnect/Shutdown proves
//     the supervised process is gone.
//
// The in-flight verification window is captured DETERMINISTICALLY
// through the mux probe signal (an accepted blackholed connection IS
// a running probe): no arbitrary sleeps anywhere in this file.

// ---- deterministic teardown regression (the v0.9.8.5 CI failure) ----

// TestStabilityTeardownReleasesProcessAndFiles is the end-to-end
// regression for the Windows CI failure:
//
//	connect → stability failure → teardown → process gone →
//	executable deletable → temp directory removable
//
// The staging directory is managed by the test itself (NOT t.TempDir)
// so the deletability assertions are explicit evidence, not testing
// framework side effects. On Windows, deleting a still-running
// executable fails with "Access is denied" — which is exactly what
// the v0.9.8.5 bug produced at TempDir cleanup time.
func TestStabilityTeardownReleasesProcessAndFiles(t *testing.T) {
	target := fakeVerifyTarget(t)

	mux := startTunnelMux(t, target.Listener.Addr().String())

	t.Setenv("FAKECORE_SOCKS_RELAY", mux.addr())

	// Self-managed staging: the test proves the directory becomes
	// removable AFTER connection_failed is observed.
	dir, err := os.MkdirTemp("", "freeriran-teardown-")
	if err != nil {
		t.Fatal(err)
	}

	exePath := contract.StageFakeCore(t, dir, "v2ray")

	registry := core.NewRegistry(system.NewCoreLocator(dir))

	if err := registry.Register(v2ray.New(), 1); err != nil {
		t.Fatalf("register v2ray: %v", err)
	}

	if err := registry.Register(xray.New(), 0); err != nil {
		t.Fatalf("register xray: %v", err)
	}

	registry.Refresh(context.Background())

	manager := connection.New(connection.Options{
		Registry:               registry,
		StartupTimeout:         10 * time.Second,
		MonitorInterval:        50 * time.Millisecond,
		VerifyInterval:         250 * time.Millisecond,
		VerifyGrace:            100 * time.Millisecond,
		VerifyFailureThreshold: 3,
		Verify:                 connection.VerifyPolicy{Target: target.URL, Timeout: 2 * time.Second},
	})

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v (state %s)", err, snapshot.State)
	}

	if snapshot.State != connection.StateConnectedVerified {
		t.Fatalf("state = %s, want connected_verified", snapshot.State)
	}

	pid := snapshot.CorePID
	if pid <= 0 {
		t.Fatalf("snapshot carries no core pid (core_pid=%d); teardown cannot be proven", pid)
	}

	if !system.ProcessAlive(pid) {
		t.Fatalf("core process %d must be alive while the session runs", pid)
	}

	// The path dies (the core process stays alive).
	mux.setHealthy(false)

	// v0.9.8.6: connection_failed is observable only AFTER the teardown
	// completed — observing it means the process is already gone.
	waitFor(t, 30*time.Second, "connection_failed after the threshold", func() bool {
		return manager.State() == connection.StateConnectionFailed
	})

	final := manager.Snapshot()

	if final.CorePID != 0 {
		t.Fatalf("snapshot core_pid = %d after teardown, want 0", final.CorePID)
	}

	// The supervised process must have ACTUALLY exited.
	if system.ProcessAlive(pid) {
		t.Fatalf("core process %d is still alive after connection_failed — "+
			"teardown was not deterministic (the v0.9.8.5 defect)", pid)
	}

	// The executable must be deletable: on Windows this fails while the
	// process (or any handle it opened) still holds the image file.
	if err := os.Remove(exePath); err != nil {
		t.Fatalf("staged executable %s is not deletable after teardown: %v", exePath, err)
	}

	// The staging directory must be removable as a whole.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("staging directory %s is not removable after teardown: %v", dir, err)
	}

	// A Shutdown after the failed state must be a harmless no-op.
	manager.Shutdown()
}

// ---- generation guards: stale results never mutate newer sessions ----

// degradedSession reaches the state where the FIRST failed recheck
// completed and the grace recheck is about to run, then returns after
// the grace recheck's probe has ARRIVED at the blackholed mux — the
// deterministic "verification in flight" marker.
func degradedSessionWithInFlightRecheck(t *testing.T) (*connection.Manager, *tunnelMux, int) {
	t.Helper()

	target := fakeVerifyTarget(t)

	mux := startTunnelMux(t, target.Listener.Addr().String())

	manager := stabilityEnv(t, mux, target.URL)

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v (state %s)", err, snapshot.State)
	}

	pid := snapshot.CorePID

	// First failed recheck: degraded evidence, session standing.
	mux.setHealthy(false)

	waitFor(t, 15*time.Second, "the degraded snapshot", func() bool {
		return manager.Snapshot().Verification == "degraded"
	})

	// The grace recheck is scheduled VerifyGrace out; its probe hitting
	// the blackholed mux proves it is RUNNING (it will time out seconds
	// later — a wide, deterministic window).
	mux.drainProbes()
	mux.awaitProbe(t, "the in-flight grace recheck probe")

	return manager, mux, pid
}

// TestDisconnectDuringVerificationDiscardsStaleResult pins the
// disconnect-during-verification case: the in-flight failed recheck
// completes while Disconnect JOINS the monitor; its result is
// discarded (generation guard) and the process teardown is PROVEN
// when Disconnect returns.
func TestDisconnectDuringVerificationDiscardsStaleResult(t *testing.T) {
	manager, _, pid := degradedSessionWithInFlightRecheck(t)

	manager.Disconnect()

	// Disconnect returning means the process is gone (join + Close).
	if system.ProcessAlive(pid) {
		t.Fatalf("core process %d outlived Disconnect() — teardown not deterministic", pid)
	}

	// The stale failed recheck must have been discarded: the manager
	// reports a clean disconnected state, not a poisoned one.
	s := manager.Snapshot()

	if s.State != connection.StateDisconnected {
		t.Fatalf("state = %s, want disconnected (stale recheck resurrected state)", s.State)
	}

	if s.Verification != "" || s.VerifyFailures != 0 {
		t.Fatalf("stale failure poisoned the post-disconnect snapshot: "+
			"verification=%q failures=%d", s.Verification, s.VerifyFailures)
	}

	manager.Shutdown()
}

// TestReconnectDuringVerificationNotPoisonedByStaleFailure pins the
// reconnect/new-session case: the new session replaces the old one
// while a failed recheck is in flight; the stale failure can never
// mark the fresh session degraded or tear it down.
func TestReconnectDuringVerificationNotPoisonedByStaleFailure(t *testing.T) {
	manager, mux, oldPID := degradedSessionWithInFlightRecheck(t)

	// Restore the path so the replacement session can verify.
	mux.setHealthy(true)

	snapshot, err := manager.Reconnect(context.Background())
	if err != nil {
		t.Fatalf("Reconnect() = %v (state %s)", err, snapshot.State)
	}

	// The OLD session's process is gone.
	if system.ProcessAlive(oldPID) {
		t.Fatalf("old core process %d outlived Reconnect()", oldPID)
	}

	// The NEW session is verified usable with ZERO stale evidence.
	s := manager.Snapshot()

	if s.State != connection.StateConnectedVerified {
		t.Fatalf("state = %s, want connected_verified", s.State)
	}

	if s.Verification != "usable" {
		t.Fatalf("verification = %q, want usable — a stale failure poisoned the new session", s.Verification)
	}

	if s.VerifyFailures != 0 {
		t.Fatalf("verify_failures = %d, want 0 (stale failure evidence leaked)", s.VerifyFailures)
	}

	if s.CorePID == 0 || s.CorePID == oldPID {
		t.Fatalf("new session pid = %d (old %d): the replacement session did not start a fresh core", s.CorePID, oldPID)
	}

	manager.Shutdown()

	if system.ProcessAlive(s.CorePID) {
		t.Fatalf("new core process %d outlived Shutdown()", s.CorePID)
	}
}

// TestShutdownDuringVerificationDiscardsStaleResult pins the
// shutdown-during-verification case: the manager is permanently
// disabled, the process is gone and the stale result never surfaced.
func TestShutdownDuringVerificationDiscardsStaleResult(t *testing.T) {
	manager, _, pid := degradedSessionWithInFlightRecheck(t)

	manager.Shutdown()

	if system.ProcessAlive(pid) {
		t.Fatalf("core process %d outlived Shutdown() — teardown not deterministic", pid)
	}

	s := manager.Snapshot()

	if s.State != connection.StateDisconnected {
		t.Fatalf("state = %s, want disconnected after shutdown", s.State)
	}

	if s.Verification != "" || s.VerifyFailures != 0 {
		t.Fatalf("stale failure poisoned the post-shutdown snapshot: "+
			"verification=%q failures=%d", s.Verification, s.VerifyFailures)
	}

	// A shut-down manager refuses new sessions outright.
	if _, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{}); err == nil {
		t.Fatal("Connect after Shutdown must fail")
	}
}

// TestStaleFailureNeverTearsDownNewSession pins the hard boundary of
// the poisoning scenario: even a stale recheck that reached the
// FAILURE THRESHOLD (which would normally tear the session down)
// must not touch a newer session that replaced the old one.
func TestStaleFailureNeverTearsDownNewSession(t *testing.T) {
	target := fakeVerifyTarget(t)

	mux := startTunnelMux(t, target.Listener.Addr().String())

	manager := connection.New(connection.Options{
		Registry:               raceRegistry(t),
		StartupTimeout:         10 * time.Second,
		MonitorInterval:        50 * time.Millisecond,
		VerifyInterval:         250 * time.Millisecond,
		VerifyGrace:            100 * time.Millisecond,
		VerifyFailureThreshold: 1, // a single failed recheck tears down
		Verify:                 connection.VerifyPolicy{Target: target.URL, Timeout: 2 * time.Second},
	})

	t.Setenv("FAKECORE_SOCKS_RELAY", mux.addr())

	snapshot, err := manager.Connect(context.Background(), vlessTestConfig(), core.Preferences{})
	if err != nil {
		t.Fatalf("Connect() = %v (state %s)", err, snapshot.State)
	}

	oldPID := snapshot.CorePID

	// The path dies; with threshold 1 the recheck that is ABOUT to run
	// would tear the session down when its result lands.
	mux.setHealthy(false)
	mux.drainProbes()

	// The threshold-reaching recheck is now in flight (its probe hit
	// the blackholed mux). Before it lands, the user reconnects onto a
	// restored path — a NEW session generation.
	mux.awaitProbe(t, "the threshold-reaching recheck probe")
	mux.setHealthy(true)

	reconnected, err := manager.Reconnect(context.Background())
	if err != nil {
		t.Fatalf("Reconnect() = %v (state %s)", err, reconnected.State)
	}

	// The stale threshold result completed during the reconnect's
	// monitor join and was discarded by the generation guard: the new
	// session stands, verified, with no failure evidence.
	s := manager.Snapshot()

	if s.State != connection.StateConnectedVerified {
		t.Fatalf("state = %s, want connected_verified — the stale threshold "+
			"failure tore down the new session", s.State)
	}

	if s.Verification != "usable" || s.VerifyFailures != 0 {
		t.Fatalf("new session poisoned by stale threshold failure: "+
			"verification=%q failures=%d", s.Verification, s.VerifyFailures)
	}

	if system.ProcessAlive(oldPID) {
		t.Fatalf("old core process %d outlived the reconnect", oldPID)
	}

	manager.Shutdown()
}
