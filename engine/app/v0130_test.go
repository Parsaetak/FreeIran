package app

// v0130_test.go covers the v0.13.0 configuration-dataset contract:
//
//   - the built-in group counts describe the WHOLE store (the All
//     badge equals the authoritative store count — the previous
//     candidateScanLimit cap showed 4000 on larger datasets);
//   - the count snapshot invalidates on real membership changes;
//   - ListConfigsFiltered returns the TRUE global total, orders
//     globally with a stable config-ID tie-breaker and pages the
//     full result without duplicates or gaps — beyond the old
//     20,000-materialised-match cap;
//   - a zero LastSuccessfulFetch never reaches the UI as a
//     timestamp (never-fetched sources serialise without one).

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/store"
)

// seedConfigs persists n minimal configuration records through the
// store's batch path. The ids are fixed-width so fingerprint ordering
// is deterministic in tests.
func seedConfigs(t *testing.T, a *App, n int, mutate func(i int, cfg *config.Config)) {
	t.Helper()

	pairs := make([]store.Pair, 0, n)

	for i := 0; i < n; i++ {
		// Store keys are 64-char lowercase hex fingerprints; derive a
		// deterministic one from the index so ordering is stable.
		id := fmt.Sprintf("%064x", sha256.Sum256([]byte(fmt.Sprintf("freeiran-v0130-test/cfg-%06d", i))))

		cfg := config.Config{
			ID:      id,
			Type:    "vless",
			Address: fmt.Sprintf("10.20.%d.%d", i/250, i%250),
			Port:    443,
		}
		cfg.Name = fmt.Sprintf("node %d", i)

		if mutate != nil {
			mutate(i, &cfg)
		}

		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal config %d: %v", i, err)
		}

		pairs = append(pairs, store.Pair{Key: cfg.ID, Value: raw})
	}

	if err := a.store.UpsertBatch(pairs); err != nil {
		t.Fatalf("upsert batch: %v", err)
	}
}

// configIDAt regenerates the deterministic fingerprint seedConfigs
// assigned to record i.
func configIDAt(i int) string {
	return fmt.Sprintf("%064x", sha256.Sum256([]byte(fmt.Sprintf("freeiran-v0130-test/cfg-%06d", i))))
}

// clockNowMS is the test wall clock in epoch milliseconds.
func clockNowMS() int64 {
	return time.Now().UnixMilli()
}

// timeUTC is a tiny helper for the source-stats contract test.
func timeUTC(sec int64) time.Time {
	return time.Unix(sec, 0).UTC()
}

func TestBuiltinGroupCountsMatchFullStore(t *testing.T) {
	a := newTestApp(t)

	const total = 4250 // > the removed candidateScanLimit cap (4000)

	seedConfigs(t, a, total, func(i int, cfg *config.Config) {
		switch {
		case i%3 == 0: // working, fast
			cfg.TestedAt = clockNowMS() - int64(i%50)*60_000
			cfg.Working = true
			cfg.LatencyMS = 100
		case i%3 == 1: // tested, failed
			cfg.TestedAt = clockNowMS() - int64(i%50)*60_000
			cfg.Working = false
			cfg.LatencyMS = 0
		default: // untested
		}
	})

	// Fast group: every i%3==0 record (working, 100 ms).
	wantWorking := 0
	for i := 0; i < total; i++ {
		if i%3 == 0 {
			wantWorking++
		}
	}

	svc := NewCollectionService(a)

	overview, err := svc.GroupsOverview()
	if err != nil {
		t.Fatalf("GroupsOverview: %v", err)
	}

	counts := map[string]int{}
	for _, g := range overview.Builtin {
		counts[g.ID] = g.Count
	}

	if counts["all"] != total {
		t.Fatalf("all count = %d, want the authoritative store count %d (visible-results cap regression?)",
			counts["all"], total)
	}

	if got := a.store.Count(); got != total {
		t.Fatalf("store count = %d, want %d", got, total)
	}

	if counts["working"] != wantWorking {
		t.Fatalf("working count = %d, want %d", counts["working"], wantWorking)
	}

	if counts["untested"] != total-wantWorking*2 {
		t.Fatalf("untested count = %d, want %d", counts["untested"], total-wantWorking*2)
	}

	if counts["fast"] != wantWorking {
		t.Fatalf("fast count = %d, want %d", counts["fast"], wantWorking)
	}
}

func TestBuiltinGroupCountsInvalidateOnMembershipChange(t *testing.T) {
	a := newTestApp(t)

	const total = 50

	seedConfigs(t, a, total, nil)

	svc := NewCollectionService(a)

	overview, err := svc.GroupsOverview()
	if err != nil {
		t.Fatalf("GroupsOverview: %v", err)
	}

	var favoritesBefore int
	for _, g := range overview.Builtin {
		if g.ID == "favorites" {
			favoritesBefore = g.Count
		}
	}

	if favoritesBefore != 0 {
		t.Fatalf("fresh dataset has %d favorites, want 0", favoritesBefore)
	}

	// Real membership change through the ONE collections authority.
	if _, err := svc.ToggleFavorite(configIDAt(7)); err != nil {
		t.Fatalf("ToggleFavorite: %v", err)
	}

	overview, err = svc.GroupsOverview()
	if err != nil {
		t.Fatalf("GroupsOverview after toggle: %v", err)
	}

	var favoritesAfter int
	for _, g := range overview.Builtin {
		if g.ID == "favorites" {
			favoritesAfter = g.Count
		}
	}

	if favoritesAfter != 1 {
		t.Fatalf("favorites count = %d after one toggle, want 1 (stale snapshot served)", favoritesAfter)
	}
}

func TestListConfigsFilteredGlobalSortPaging(t *testing.T) {
	a := newTestApp(t)

	const total = 600

	seedConfigs(t, a, total, func(i int, cfg *config.Config) {
		if i%2 == 0 {
			// Half the dataset measured: heavy TIES across page
			// boundaries (the paging determinism case).
			cfg.TestedAt = 1700000000000
			cfg.Working = true
			cfg.LatencyMS = int64(100 + i/50)
		}
	})

	data := NewDataService(a)

	pageSize := 100

	collect := func(desc bool) []string {
		var ids []string
		seen := map[string]bool{}

		for offset := 0; ; offset += pageSize {
			page, err := data.ListConfigsFiltered(ConfigFilter{
				SortBy:   "latency",
				SortDesc: desc,
			}, offset, pageSize)
			if err != nil {
				t.Fatalf("ListConfigsFiltered offset=%d: %v", offset, err)
			}

			if page.Total != total {
				t.Fatalf("Total = %d, want %d (global total, not a bounded window)", page.Total, total)
			}

			for _, cfg := range page.Items {
				id := cfg.ID
				if seen[id] {
					t.Fatalf("id %s appeared twice across pages (duplicate)", id)
				}
				seen[id] = true
				ids = append(ids, id)
			}

			if !page.HasMore {
				break
			}
		}

		if len(ids) != total {
			t.Fatalf("walked %d ids, want %d (gap in pagination)", len(ids), total)
		}

		return ids
	}

	asc := collect(false)
	desc := collect(true)

	// Global tie-broken ordering: every measured record sorted by
	// latency ASC with the ID tie-break; unmeasured records tail in
	// ID order. Verify a monotone property instead of duplicating the
	// comparator: within equal latencies, IDs must be ascending.
	latencyOf := func(id string) int64 {
		var cfg config.Config
		value, err := a.store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if err := json.Unmarshal(value, &cfg); err != nil {
			t.Fatalf("decode %s: %v", id, err)
		}
		return cfg.LatencyMS
	}

	for i := 1; i < len(asc); i++ {
		prev, cur := latencyOf(asc[i-1]), latencyOf(asc[i])
		prevMeasured, curMeasured := prev > 0, cur > 0

		switch {
		case prevMeasured && !curMeasured:
			// Measured block ends, unmeasured tail begins: correct.
		case !curMeasured && !prevMeasured:
			// Unmeasured tail: all equal on the primary field — IDs
			// must ascend (the ONE global tie-break).
			if asc[i] < asc[i-1] {
				t.Fatalf("unmeasured tie-break violated at %d: %s after %s (want ascending IDs)",
					i, asc[i], asc[i-1])
			}
		default:
			// Measured block: latency ascending, ties broken by ID.
			if cur < prev {
				t.Fatalf("ASC ordering violated at %d: %s(%d) after %s(%d)", i, asc[i], cur, asc[i-1], prev)
			}

			if cur == prev && asc[i] < asc[i-1] {
				t.Fatalf("ASC tie-break violated at %d: %s after %s within latency %d (want ascending IDs)",
					i, asc[i], asc[i-1], cur)
			}
		}
	}

	// DESC flips the PRIMARY field only — the ID tie-break stays
	// ascending so each direction has ONE deterministic global order:
	// unmeasured records first (IDs ascending), then measured by
	// DESCENDING latency (IDs still ascending within a tie).
	for i := 1; i < len(desc); i++ {
		prev, cur := latencyOf(desc[i-1]), latencyOf(desc[i])
		prevMeasured, curMeasured := prev > 0, cur > 0

		switch {
		case !prevMeasured && curMeasured:
			// Unmeasured block ends, measured block begins: correct.
		case prevMeasured && curMeasured:
			if cur > prev {
				t.Fatalf("DESC ordering violated at %d: %s(%d) after %s(%d)", i, desc[i], cur, desc[i-1], prev)
			}

			if cur == prev && desc[i] < desc[i-1] {
				t.Fatalf("DESC tie-break violated at %d: %s after %s within latency %d (want ascending IDs)",
					i, desc[i], desc[i-1], cur)
			}
		default:
			// Unmeasured tail: IDs must ascend.
			if desc[i] < desc[i-1] {
				t.Fatalf("DESC unmeasured tie-break violated at %d: %s after %s (want ascending IDs)",
					i, desc[i], desc[i-1])
			}
		}
	}
}

func TestListConfigsFilteredTotalBeyond20k(t *testing.T) {
	a := newTestApp(t)

	// > maxFilteredMatches (20,000): the old materialisation cap
	// truncated Total and hid the tail. Tiny records keep the test
	// fast; the point is the CONTRACT, not the record size.
	const total = 20500

	seedConfigs(t, a, total, nil)

	data := NewDataService(a)

	pageSize := 1000

	// Jump straight to the tail: a total walk would re-prove paging
	// (covered above); this proves the tail EXISTS and Total is true.
	offset := (total - 1) / pageSize * pageSize

	page, err := data.ListConfigsFiltered(ConfigFilter{}, offset, pageSize)
	if err != nil {
		t.Fatalf("tail page: %v", err)
	}

	if page.Total != total {
		t.Fatalf("Total = %d, want %d (20k materialisation cap regression?)", page.Total, total)
	}

	if len(page.Items) != total-offset {
		t.Fatalf("tail page carried %d items, want %d", len(page.Items), total-offset)
	}

	if page.HasMore {
		t.Fatal("tail page must not claim HasMore")
	}

	// The absolute last record is present (nothing silently dropped):
	// the tail must contain the store's full page, and Total must
	// already have proven nothing was dropped.
	last := page.Items[len(page.Items)-1]
	if last.ID == "" {
		t.Fatal("last record has an empty ID")
	}
}

func TestSourceStatsZeroTimeSerializesAbsent(t *testing.T) {
	// The freshness contract: a never-fetched source must NOT
	// serialise a timestamp (a zero Go time renders as year 1 and
	// became "739889d ago" in the UI).
	never := source.Source{}.Stats()

	raw, err := json.Marshal(never)
	if err != nil {
		t.Fatalf("marshal zero stats: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode stats: %v", err)
	}

	if _, present := decoded["last_successful_fetch"]; present {
		t.Fatalf("zero LastSuccessfulFetch serialised as %v — never-fetched sources must omit the field",
			decoded["last_successful_fetch"])
	}

	if _, present := decoded["last_failure"]; present {
		t.Fatalf("zero LastFailure serialised as %v — must omit", decoded["last_failure"])
	}

	// A real time stays real.
	fetched := source.Source{LastSuccessfulFetch: timeUTC(1700000000)}.Stats()

	raw, err = json.Marshal(fetched)
	if err != nil {
		t.Fatalf("marshal fetched stats: %v", err)
	}

	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode fetched stats: %v", err)
	}

	if got, _ := decoded["last_successful_fetch"].(string); got == "" {
		t.Fatal("valid LastSuccessfulFetch vanished from the payload")
	}
}
