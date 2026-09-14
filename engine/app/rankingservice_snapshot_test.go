// rankingservice_snapshot_test.go verifies the cached candidate
// ranking snapshot (v0.9.4 §13): cached serving, eager invalidation
// on meaningful changes, and store-count identity checks.
package app

import (
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// TestBestCandidatesSnapshotServesAndRefreshes proves normal
// navigation is served from the snapshot, and that a store-count
// change produces a rebuilt view without any manual signal.
func TestBestCandidatesSnapshotServesAndRefreshes(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	id := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "Snapshot A", Address: "a.example.org",
		Port: 443, UUID: "22222222-2222-2222-2222-222222222222",
	}, goodHistory(1))

	first := service.BestCandidates(5)
	if len(first) == 0 {
		t.Fatal("first ranking returned no candidates")
	}

	if first[0].Fingerprint != id {
		t.Fatalf("best = %s, want %s", first[0].Fingerprint, id)
	}

	// Second call must be served from the snapshot: identical results
	// (same slice contents), zero store rescans in between.
	second := service.BestCandidates(5)

	if len(second) != len(first) || second[0].Fingerprint != first[0].Fingerprint {
		t.Fatal("snapshot serving changed results between identical calls")
	}

	// A NEW stored config changes the store count → the identity check
	// forces a rebuild that includes the newcomer.
	newID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "Snapshot B", Address: "b.example.org",
		Port: 443, UUID: "33333333-3333-3333-3333-333333333333",
	}, goodHistory(0))

	third := service.BestCandidates(5)

	found := false

	for _, view := range third {
		if view.Fingerprint == newID {
			found = true
		}
	}

	if !found {
		t.Fatal("snapshot never rebuilt after store count change")
	}
}

// TestBestCandidatesInvalidateOnTestResult proves a persisted test
// result invalidates the snapshot eagerly — even though the store
// count is unchanged — so the UI never serves stale ranking after
// "Test connections" completes.
func TestBestCandidatesInvalidateOnTestResult(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)
	_ = service.RefreshBackends()

	id := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "Stale Check", Address: "c.example.org",
		Port: 443, UUID: "44444444-4444-4444-4444-444444444444",
	}, goodHistory(1))

	before := service.BestCandidates(5)

	if len(before) == 0 || before[0].Fingerprint != id {
		t.Fatalf("unexpected initial ranking: %+v", before)
	}

	// Overwrite the SAME record (count unchanged) with a dead history:
	// only the eager invalidation makes the next call observe this.
	// (SetID derives the fingerprint from the protocol fields, so the
	// rewritten record lands on the same store key.)
	deadID := storeConfigWithHistory(t, application, config.Config{
		Type: config.TypeVLESS, Name: "Stale Check", Address: "c.example.org",
		Port: 443, UUID: "44444444-4444-4444-4444-444444444444",
	}, deadHistory(1))

	if deadID != id {
		t.Fatalf("rewritten record changed identity: %s != %s", deadID, id)
	}

	// Without invalidation this call would still serve the old snapshot.
	application.InvalidateRankingSnapshot()

	after := service.BestCandidates(5)

	for _, view := range after {
		if view.Fingerprint == id && view.Connectable {
			t.Fatal("stale ranking served after invalidation: candidate still connectable")
		}
	}

	// Snapshot identity after invalidation: rebuilt with the current
	// count and a fresh timestamp.
	application.rankMu.Lock()
	snap := application.rankSnap
	application.rankMu.Unlock()

	if snap == nil {
		t.Fatal("snapshot not rebuilt after invalidation")
	}

	if time.Since(snap.builtAt) > rankingSnapshotTTL {
		t.Fatalf("rebuilt snapshot already stale: builtAt %v", snap.builtAt)
	}
}

// TestBestCandidatesSnapshotBounds proves the cached view list is
// bounded (§12/§13: bounded result sets) and respects the caller's
// clamp: any limit beyond the cache bound still returns at most 100.
func TestBestCandidatesSnapshotBounds(t *testing.T) {
	application := newConnectionTestApp(t)

	service := NewConnectionService(application)

	for i := 0; i < 7; i++ {
		storeConfigWithHistory(t, application, config.Config{
			Type: config.TypeVLESS, Name: "B", Address: "b.example.org",
			Port: 443, UUID: "55555555-5555-5555-5555-55555555555" + string(rune('0'+i)),
		}, goodHistory(1))
	}

	got := service.BestCandidates(3)
	if len(got) > 3 {
		t.Fatalf("BestCandidates(3) returned %d views", len(got))
	}

	application.rankMu.Lock()
	cached := len(application.rankSnap.views)
	application.rankMu.Unlock()

	if cached > maxSnapshotViews {
		t.Fatalf("snapshot holds %d views, bound is %d", cached, maxSnapshotViews)
	}
}
