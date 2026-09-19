package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core/contract"
)

// recoveryservice_state_test.go — v0.9.8.4 §6 regression tests: the
// recovery decision table against the v0.9.8.3 connection states.
//
//   - connected_verified is healthy (clears an episode);
//   - connected is NOT falsely treated as verified: it is in progress
//     and clears nothing;
//   - verifying / disconnecting are in progress — no interference;
//   - connection_failed triggers the bounded recovery episode;
//   - recovered sessions require actual verification (the end-to-end
//     verified case runs a real request through the SOCKS-relay fake
//     core tunnel — no public network involved).

// stageEpisode arms one active recovery episode (as a prior failure
// would have left it).
func stageEpisode(t *testing.T, recovery *RecoveryService, now time.Time) {
	t.Helper()

	recovery.mu.Lock()
	recovery.episode = &recoveryEpisode{
		number:      1,
		startedAt:   now,
		nextAttempt: now,
	}
	recovery.mu.Unlock()
}

func episodeActive(recovery *RecoveryService) bool {
	recovery.mu.Lock()
	defer recovery.mu.Unlock()

	return recovery.episode != nil
}

// TestRecoveryDecisionByState pins the per-state policy table.
func TestRecoveryDecisionByState(t *testing.T) {
	application := newConnectionTestApp(t)
	defer application.Shutdown()

	recovery := application.recovery
	now := time.Now().UTC()

	cases := []struct {
		state        connection.State
		wantCleared  bool
		wantActed    bool
		wantFailures int
	}{
		// Final success: the only connected state that is healthy.
		{connection.StateConnectedVerified, true, false, 0},
		// Idle by user choice: healthy.
		{connection.StateDisconnected, true, false, 0},
		// v0.9.8.4 fix: route established is NOT verified — an episode
		// must survive, nothing may act, no cooldown may be recorded.
		{connection.StateConnected, false, false, 0},
		// Verification in progress: no interference.
		{connection.StateVerifying, false, false, 0},
		// Teardown in progress: no interference.
		{connection.StateDisconnecting, false, false, 0},
		// Machine-internal transitional states: no interference.
		{connection.StateSelecting, false, false, 0},
		{connection.StatePreparing, false, false, 0},
		{connection.StateStartingCore, false, false, 0},
		{connection.StateWaitingForReady, false, false, 0},
	}

	for _, tc := range cases {
		stageEpisode(t, recovery, now)

		recovery.decide(now, connection.Snapshot{
			State:    tc.state,
			ConfigID: "cfg-x",
		})

		if got := episodeActive(recovery); got != !tc.wantCleared {
			t.Fatalf("state %s: episode active = %v, want %v", tc.state, got, !tc.wantCleared)
		}

		recovery.mu.Lock()
		failed := len(recovery.failures)
		acted := recovery.episode != nil && recovery.episode.attempts > 0
		recovery.mu.Unlock()

		if tc.wantFailures == 0 && failed != 0 {
			t.Fatalf("state %s: recorded %d failure memories, want 0", tc.state, failed)
		}

		if acted {
			t.Fatalf("state %s: recovery acted on a non-failed state", tc.state)
		}

		// Reset the episode for the next case.
		recovery.mu.Lock()
		recovery.episode = nil
		recovery.failures = map[string]time.Time{}
		recovery.mu.Unlock()
	}
}

// TestRecoveryFailedStateTriggersBoundedEpisode verifies that an
// explicit connection_failed snapshot drives the bounded attempt
// schedule (and records the failed candidate's cooldown).
func TestRecoveryFailedStateTriggersBoundedEpisode(t *testing.T) {
	application := newConnectionTestApp(t)
	defer application.Shutdown()

	recovery := application.recovery
	now := time.Now().UTC()

	recovery.decide(now, connection.Snapshot{
		State:     connection.StateConnectionFailed,
		ConfigID:  "cfg-dead",
		LastError: "boom",
	})

	if !episodeActive(recovery) {
		t.Fatal("connection_failed must start a recovery episode")
	}

	recovery.mu.Lock()
	cooled := recovery.failures["cfg-dead"]
	attempts := recovery.episode.attempts
	recovery.mu.Unlock()

	if cooled.IsZero() {
		t.Fatal("failed candidate must enter the cooldown memory")
	}

	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (the first bounded attempt fired)", attempts)
	}
}

// newVerifiedConnectionTestApp boots an app whose fake v2ray core
// relays SOCKS CONNECTs to a local HTTP target, so the FULL verified
// path (real request through the tunnel → connected_verified) runs
// end-to-end without any public network.
func newVerifiedConnectionTestApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	// The fake core process reads this at launch: minimal SOCKS5 on
	// its inbound, every CONNECT relayed to the local target.
	t.Setenv("FAKECORE_SOCKS_RELAY", target.Listener.Addr().String())

	application, err := New(Options{
		BaseDir:                 filepath.Join(t.TempDir(), "freeiran"),
		RefreshInterval:         time.Hour,
		RunIngestionOnStart:     false,
		SkipDefaultSources:      true,
		SkipConnectVerification: false, // the whole point of this harness
		VerifyTarget:            target.URL,
	})
	if err != nil {
		t.Fatalf("app.New() = %v", err)
	}

	t.Cleanup(application.Shutdown)

	contract.StageFakeCore(t, ensureCoresDir(application.opts.BaseDir), "v2ray")

	return application, target
}

// TestRecoveryRequiresActualVerification is the §6 end-to-end gate:
// recovery succeeds only on a session whose Internet verification
// genuinely passed (state connected_verified, verification usable) —
// and a verified recovery clears the episode.
func TestRecoveryRequiresActualVerification(t *testing.T) {
	application, _ := newVerifiedConnectionTestApp(t)

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	deadID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeWireGuard, Name: "dead", Address: "dead.example.org",
		Port: 51820,
	}, goodHistory(2))

	fallbackID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "fallback", Address: "fb.example.org",
		Port: 443, UUID: "22222222-2222-2222-2222-222222222222",
		Network: "tcp", Security: "tls",
	}, goodHistory(3))

	_ = deadID

	// Drive the machine into connection_failed the honest way.
	_ = stageFailedConnection(t, application, "unservable")

	recovery := application.recovery

	// The dead candidate is cooled down exactly as the watch loop
	// would after observing the failure.
	now := time.Now().UTC()
	recovery.mu.Lock()
	recovery.failures[deadID] = now
	recovery.mu.Unlock()

	recovery.tick(now)

	snapshot := application.connMgr.Snapshot()

	// The recovered session must be VERIFIED — the final success
	// state, not merely a ready listener.
	if snapshot.State != connection.StateConnectedVerified {
		t.Fatalf("recovered state = %s (%s), want connected_verified",
			snapshot.State, snapshot.LastError)
	}

	if snapshot.Verification != "usable" {
		t.Fatalf("verification = %q, want usable", snapshot.Verification)
	}

	if snapshot.ConfigID != fallbackID {
		t.Fatalf("recovered onto %s, want fallback %s", snapshot.ConfigID, fallbackID)
	}

	if episodeActive(recovery) {
		t.Fatalf("episode must be cleared after a verified recovery: %+v", recovery.Status())
	}

	recovery.Stop()
}
