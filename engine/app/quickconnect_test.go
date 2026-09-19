package app

// v0.9.8.3 regression tests for the fresh-selection loop (P0 §3/§4):
// shortlist merge/dedupe/limits, evidence freshness classification,
// failure cooldowns and candidate iteration.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func qcStoreConfig(t *testing.T, a *App, cfg config.Config) string {
	t.Helper()

	cfg.Normalize()
	cfg.SetID()

	raw, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := a.store.Upsert(cfg.ID, raw); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	return cfg.ID
}

func qcConfig(name, uuid string) config.Config {
	return config.Config{
		Type:     config.TypeVLESS,
		Name:     name,
		Address:  name + ".example.org",
		Port:     443,
		UUID:     uuid,
		Network:  "tcp",
		Security: "tls",
	}
}

// TestEvidenceAgeClassification: fresh/recent/stale/unknown classes.
func TestEvidenceAgeClassification(t *testing.T) {
	now := time.Now().UTC()

	fresh := qcRecord{cfg: config.Config{TestedAt: now.Add(-10 * time.Minute).UnixMilli()}}
	recent := qcRecord{cfg: config.Config{TestedAt: now.Add(-6 * time.Hour).UnixMilli()}}
	stale := qcRecord{cfg: config.Config{TestedAt: now.Add(-3 * 24 * time.Hour).UnixMilli()}}
	unknown := qcRecord{cfg: config.Config{}}

	if got := fresh.evidenceAge(now); got != "fresh" {
		t.Fatalf("fresh = %s", got)
	}

	if got := recent.evidenceAge(now); got != "recent" {
		t.Fatalf("recent = %s", got)
	}

	if got := stale.evidenceAge(now); got != "stale" {
		t.Fatalf("stale = %s", got)
	}

	if got := unknown.evidenceAge(now); got != "unknown" {
		t.Fatalf("unknown = %s", got)
	}
}

// TestQuickConnectShortlistMerge: recent verified successes lead the
// shortlist, ranked unverified candidates follow, duplicates
// collapse, and the limit is respected.
func TestQuickConnectShortlistMerge(t *testing.T) {
	a := newTestApp(t)

	now := time.Now().UTC()

	recentSuccess := qcConfig("recent", "11111111-1111-1111-1111-111111111111")
	recentSuccess.LastSuccessAt = now.Add(-5 * time.Minute).UnixMilli()
	recentSuccess.TestedAt = now.Add(-5 * time.Minute).UnixMilli()

	oldSuccess := qcConfig("old", "22222222-2222-1111-1111-111111111111")
	oldSuccess.LastSuccessAt = now.Add(-48 * time.Hour).UnixMilli()
	oldSuccess.TestedAt = now.Add(-48 * time.Hour).UnixMilli()

	neverTested := qcConfig("never", "33333333-1111-1111-1111-111111111111")

	idRecent := qcStoreConfig(t, a, recentSuccess)
	idOld := qcStoreConfig(t, a, oldSuccess)
	idNever := qcStoreConfig(t, a, neverTested)

	records := a.collectCandidateRecords(a.ctx)
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}

	shortlist := a.buildQuickConnectShortlist(records, now, 2)
	if len(shortlist) != 2 {
		t.Fatalf("shortlist = %d, want 2 (bounded)", len(shortlist))
	}

	if shortlist[0].cfg.ID != idRecent {
		t.Fatalf("first shortlist entry = %s, want the recent verified success %s",
			shortlist[0].cfg.ID, idRecent)
	}

	seen := map[string]bool{}
	for _, rec := range shortlist {
		if seen[rec.cfg.ID] {
			t.Fatalf("duplicate candidate %s in shortlist", rec.cfg.ID)
		}

		seen[rec.cfg.ID] = true
	}

	// The old success and the untested candidate are the ranked tail.
	if shortlist[1].cfg.ID != idOld && shortlist[1].cfg.ID != idNever {
		t.Fatalf("second shortlist entry = %s, want %s or %s",
			shortlist[1].cfg.ID, idOld, idNever)
	}
}

// TestQuickConnectCooldownFilters: a just-failed candidate is skipped
// until its cooldown decays; expired cooldowns are dropped.
func TestQuickConnectCooldownFilters(t *testing.T) {
	a := newTestApp(t)

	id := qcStoreConfig(t, a, qcConfig("cooled", "44444444-1111-1111-1111-111111111111"))

	records := a.collectCandidateRecords(a.ctx)

	// Record a failure now: the candidate must be filtered out.
	a.recordQuickConnectFailure(id, errNoViableCandidate)

	filtered := a.filterQuickConnectCooldowns(records, time.Now().UTC(), nil)
	if len(filtered) != 0 {
		t.Fatalf("cooled candidate not filtered (got %d records)", len(filtered))
	}

	// After the cooldown window it returns, and the memory is pruned.
	future := time.Now().UTC().Add(qcCandidateCooldown + time.Minute)
	filtered = a.filterQuickConnectCooldowns(records, future, nil)
	if len(filtered) != 1 {
		t.Fatalf("cooldown did not decay (got %d records)", len(filtered))
	}

	a.qcMu.Lock()
	pending := len(a.qcFailures)
	a.qcMu.Unlock()

	if pending != 0 {
		t.Fatalf("expired cooldown entries not pruned: %d", pending)
	}
}

// TestQuickConnectLoopNoCandidates: an empty store fails honestly and
// quickly (bounded exhaustion, never a hang or a fake success).
func TestQuickConnectLoopNoCandidates(t *testing.T) {
	a := newTestApp(t)

	_, err := a.quickConnectLoop(a.ctx, nil, 0)
	if err == nil {
		t.Fatal("quickConnectLoop over an empty store must fail")
	}
}
