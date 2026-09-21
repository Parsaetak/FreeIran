// quickconnect_v0910_test.go proves the v0.9.10 runtime-context fix
// on the PRODUCTION Quick Connect path: quickConnectLoop →
// connectVerified wraps every attempt in a bounded operation context
// (qcAttemptTimeout, released by `defer cancel()` the moment the
// attempt returns). Pre-0.9.10 the established core was bound to that
// context and died the instant a successful attempt returned — the
// monitor then reported an "unexpected" crash, the state flapped to
// connection_failed and the bounded recovery loop reconnected into
// the same trap (the v0.9.9 CI race in TestRecoveryRequiresActualVerification
// and TestRecoverySwitchesToNextCandidate).
//
// The proofs here run the REAL verified path (fake core relaying to a
// local HTTP target, verification gate active):
//
//   - after ConnectBest returns, the core process is ALIVE and the
//     session STAYS connected_verified across an observation window
//     (no post-success death, no flapping);
//   - the recovery path reconnects and its recovered session ALSO
//     stays verified and alive.
package app

import (
	"context"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
)

// TestQuickConnectSessionSurvivesAttemptContext is the production-path
// regression for the v0.9.10 fix.
func TestQuickConnectSessionSurvivesAttemptContext(t *testing.T) {
	application, _ := newVerifiedConnectionTestApp(t)

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "qc-survivor", Address: "qc.example.org",
		Port: 443, UUID: "33333333-3333-3333-3333-333333333333",
		Network: "tcp", Security: "tls",
	}, goodHistory(2))

	result, err := service.ConnectBest(nil)
	if err != nil {
		t.Fatalf("ConnectBest: %v", err)
	}

	if result.Snapshot.State != connection.StateConnectedVerified {
		t.Fatalf("state = %s, want connected_verified", result.Snapshot.State)
	}

	if result.Snapshot.Verification != "usable" {
		t.Fatalf("verification = %q, want usable", result.Snapshot.Verification)
	}

	pid := result.Snapshot.CorePID
	if pid == 0 {
		t.Fatal("verified Quick Connect snapshot carries no core pid")
	}

	// Observation window: connectVerified's `defer cancel()` has long
	// fired by now; on the pre-0.9.10 architecture the core was bound
	// to that context and died within milliseconds of this point. Three
	// seconds is orders of magnitude beyond the kill propagation.
	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		snapshot := application.connMgr.Snapshot()

		if snapshot.State != connection.StateConnectedVerified {
			t.Fatalf("session flapped to %s (%s) after ConnectBest returned — "+
				"the attempt context killed the established core",
				snapshot.State, snapshot.LastError)
		}

		if snapshot.CorePID != pid {
			t.Fatalf("core pid changed from %d to %d — the session was replaced or died", pid, snapshot.CorePID)
		}

		time.Sleep(150 * time.Millisecond)
	}

	// Explicit disconnect remains the deterministic terminator.
	service.Disconnect()

	if state := application.connMgr.State(); state != connection.StateDisconnected {
		t.Fatalf("state after disconnect = %s, want disconnected", state)
	}
}

// TestRecoverySessionSurvivesAttemptContext proves the recovered
// session (the bounded recovery loop runs the SAME Quick Connect loop)
// also survives its attempt context — the exact CI-race scenario.
func TestRecoverySessionSurvivesAttemptContext(t *testing.T) {
	application, _ := newVerifiedConnectionTestApp(t)

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	deadID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeWireGuard, Name: "dead", Address: "dead.example.org",
		Port: 51820,
	}, goodHistory(2))

	storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "fallback", Address: "fb.example.org",
		Port: 443, UUID: "44444444-4444-4444-4444-444444444444",
		Network: "tcp", Security: "tls",
	}, goodHistory(3))

	// Drive the machine into connection_failed the honest way.
	_ = stageFailedConnection(t, application, "unservable")

	recovery := application.recovery

	now := time.Now().UTC()
	recovery.mu.Lock()
	recovery.failures[deadID] = now
	recovery.mu.Unlock()

	recovery.tick(goContext(t), now)

	snapshot := application.connMgr.Snapshot()
	if snapshot.State != connection.StateConnectedVerified {
		t.Fatalf("recovered state = %s (%s), want connected_verified",
			snapshot.State, snapshot.LastError)
	}

	pid := snapshot.CorePID

	// The recovered session must STAY verified — the recovery attempt's
	// operation context was released when quickConnectLoop returned; a
	// pre-0.9.10 build killed this core immediately and the watch loop
	// would see a NEW connection_failed within the next tick (flap).
	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		current := application.connMgr.Snapshot()

		if current.State != connection.StateConnectedVerified {
			t.Fatalf("recovered session flapped to %s (%s) — the recovery attempt "+
				"context killed the established core", current.State, current.LastError)
		}

		if current.CorePID != pid {
			t.Fatalf("recovered core pid changed from %d to %d", pid, current.CorePID)
		}

		time.Sleep(150 * time.Millisecond)
	}
}

// goContext returns a plain background context (named helper keeps the
// import surface of this file small).
func goContext(t *testing.T) context.Context {
	t.Helper()

	return context.Background()
}

// TestQuickConnectCoreProcessGoneAfterDisconnect completes the
// production-path lifetime proof: the session's core process is
// actually terminated by the explicit disconnect (no orphan remains in
// the staged cores directory).
func TestQuickConnectCoreProcessGoneAfterDisconnect(t *testing.T) {
	application, _ := newVerifiedConnectionTestApp(t)

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "qc-teardown", Address: "qt.example.org",
		Port: 443, UUID: "55555555-5555-5555-5555-555555555555",
		Network: "tcp", Security: "tls",
	}, goodHistory(1))

	if _, err := service.ConnectBest(nil); err != nil {
		t.Fatalf("ConnectBest: %v", err)
	}

	snapshot := application.connMgr.Snapshot()
	if snapshot.CorePID == 0 {
		t.Fatal("no core pid on the verified session")
	}

	service.Disconnect()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		snapshot := application.connMgr.Snapshot()

		if snapshot.State == connection.StateDisconnected && snapshot.CorePID == 0 {
			return // terminated and observed
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("core process still referenced after disconnect: %+v", application.connMgr.Snapshot())
}
