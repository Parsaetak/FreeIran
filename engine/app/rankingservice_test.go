package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// storeConfigWithHistory persists a configuration with a pre-seeded
// test history (the ranking layer's real data source).
func storeConfigWithHistory(
	t *testing.T,
	application *App,
	cfg config.Config,
	history []config.TestObservation,
) string {
	t.Helper()

	cfg.Normalize()
	cfg.SetID()
	cfg.TestHistory = history

	if len(history) > 0 {
		last := history[len(history)-1]

		cfg.Working = last.Working
		cfg.LatencyMS = last.LatencyMS
		cfg.TestedAt = last.At
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := application.store.Upsert(cfg.ID, raw); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	return cfg.ID
}

func goodHistory(minuteAgo int) []config.TestObservation {
	now := time.Now().UTC()

	return []config.TestObservation{
		{At: now.Add(-time.Duration(minuteAgo+4) * time.Minute).UnixMilli(), Working: true, LatencyMS: 33, Backend: "v2ray"},
		{At: now.Add(-time.Duration(minuteAgo+3) * time.Minute).UnixMilli(), Working: true, LatencyMS: 31, Backend: "v2ray"},
		{At: now.Add(-time.Duration(minuteAgo+2) * time.Minute).UnixMilli(), Working: true, LatencyMS: 32, Backend: "v2ray"},
		{At: now.Add(-time.Duration(minuteAgo+1) * time.Minute).UnixMilli(), Working: true, LatencyMS: 31, Backend: "v2ray"},
		{At: now.Add(-time.Duration(minuteAgo) * time.Minute).UnixMilli(), Working: true, LatencyMS: 31, Backend: "v2ray"},
	}
}

func deadHistory(minuteAgo int) []config.TestObservation {
	now := time.Now().UTC()

	return []config.TestObservation{
		{At: now.Add(-time.Duration(minuteAgo+2) * time.Minute).UnixMilli(), Working: false},
		{At: now.Add(-time.Duration(minuteAgo+1) * time.Minute).UnixMilli(), Working: false, TimedOut: true},
		{At: now.Add(-time.Duration(minuteAgo) * time.Minute).UnixMilli(), Working: false},
	}
}

// TestBestCandidatesRanking verifies the ranked, credential-free view:
// best history first, dead candidates flagged, never-tested marked
// unknown.
func TestBestCandidatesRanking(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)

	// Candidate compatibility counts AVAILABLE backends: force the
	// same synchronous discovery the Connect tests use (in production
	// Start() refreshes in the background before the UI ranks).
	_ = service.RefreshBackends()

	bestID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "Netherlands", Address: "nl.example.org",
		Port: 443, UUID: "11111111-1111-1111-1111-111111111111",
		Network: "tcp", Security: "tls",
	}, goodHistory(2))

	deadID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "Dead-Server", Address: "dead.example.org",
		Port: 443, UUID: "22222222-2222-2222-2222-222222222222",
		Network: "tcp", Security: "tls",
	}, deadHistory(1))

	untestedID := storeConfig(t, application, config.Config{
		Type: config.TypeVLESS, Name: "Untested", Address: "un.example.org",
		Port: 443, UUID: "33333333-3333-3333-3333-333333333333",
		Network: "tcp", Security: "tls",
	})

	views := service.BestCandidates(5)

	if len(views) != 3 {
		t.Fatalf("views = %d, want 3", len(views))
	}

	if views[0].Fingerprint != bestID || views[0].Class != "best" {
		t.Fatalf("top candidate = %s (%s), want %s (best)",
			views[0].Fingerprint, views[0].Class, bestID)
	}

	if views[0].LatencyMS != 31 {
		t.Fatalf("top latency = %d, want 31", views[0].LatencyMS)
	}

	if len(views[0].Explanation) == 0 {
		t.Fatal("top candidate must explain its score")
	}

	// No credential material may cross the boundary.
	for _, view := range views {
		if contains(view.Endpoint, "1111") || contains(view.Name, "1111") {
			t.Fatalf("candidate view leaks credential-like material: %+v", view)
		}
	}

	classes := map[string]string{}
	for _, view := range views {
		classes[view.Fingerprint] = view.Class
	}

	if classes[deadID] != "dead" {
		t.Fatalf("dead candidate class = %q, want dead", classes[deadID])
	}

	if classes[untestedID] != "unknown" {
		t.Fatalf("untested class = %q, want unknown", classes[untestedID])
	}
}

// TestConnectBestSelectsBestCandidate verifies the automatic connect
// path end-to-end: the best candidate is chosen and connected through
// the real (fake-binary) state machine.
func TestConnectBestSelectsBestCandidate(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "mediocre", Address: "mid.example.org",
		Port: 443, UUID: "11111111-1111-1111-1111-111111111111",
		Network: "tcp", Security: "tls",
	}, []config.TestObservation{
		{At: time.Now().Add(-5 * time.Minute).UnixMilli(), Working: true, LatencyMS: 700, Backend: "v2ray"},
		{At: time.Now().Add(-3 * time.Minute).UnixMilli(), Working: false},
		{At: time.Now().Add(-1 * time.Minute).UnixMilli(), Working: true, LatencyMS: 750, Backend: "v2ray"},
	})

	bestID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "Netherlands", Address: "nl.example.org",
		Port: 443, UUID: "22222222-2222-2222-2222-222222222222",
		Network: "tcp", Security: "tls",
	}, goodHistory(2))

	result, err := service.ConnectBest(nil)
	if err != nil {
		t.Fatalf("ConnectBest() = %v (state %s)", err, result.Snapshot.State)
	}

	if result.Snapshot.State != "connected" {
		t.Fatalf("state = %s, want connected", result.Snapshot.State)
	}

	if result.Chosen.Fingerprint != bestID {
		t.Fatalf("chosen = %s, want best %s", result.Chosen.Fingerprint, bestID)
	}

	if result.Snapshot.ConfigName != "Netherlands" {
		t.Fatalf("connected config = %s, want Netherlands", result.Snapshot.ConfigName)
	}

	// The user-facing failure surface must stay empty.
	if service.ConnectionState().LastError != "" {
		t.Fatalf("unexpected last error: %s", service.ConnectionState().LastError)
	}

	service.Disconnect()
}

// TestConnectBestExcludes verifies the exclusion contract: the best
// overall candidate can be ruled out (recovery + "find better" path).
func TestConnectBestExcludes(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	firstID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "best-ever", Address: "b.example.org",
		Port: 443, UUID: "11111111-1111-1111-1111-111111111111",
		Network: "tcp", Security: "tls",
	}, goodHistory(2))

	secondID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "second-best", Address: "s.example.org",
		Port: 443, UUID: "22222222-2222-2222-2222-222222222222",
		Network: "tcp", Security: "tls",
	}, goodHistory(4))

	result, err := service.ConnectBest([]string{firstID})
	if err != nil {
		t.Fatalf("ConnectBest(exclude) = %v", err)
	}

	if result.Chosen.Fingerprint != secondID {
		t.Fatalf("chosen = %s, want %s after exclusion", result.Chosen.Fingerprint, secondID)
	}

	service.Disconnect()
}

// TestConnectBestNoCandidates refuses honestly on an empty store.
func TestConnectBestNoCandidates(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)

	if _, err := service.ConnectBest(nil); err == nil {
		t.Fatal("ConnectBest must fail when nothing is stored")
	}
}
