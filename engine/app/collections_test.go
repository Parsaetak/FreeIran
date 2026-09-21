// collections_test.go verifies the v0.9.10 collections surface:
// persistent favorites, user-defined groups (stable IDs, versioned
// sidecar, atomic persistence), built-in evidence groups (computed
// from real records — never invented) and the group-aware filter
// pipeline. Favorites and groups must never bypass testing or trust:
// the filter only narrows which records are returned.
package app

import (
	"os"
	"testing"
	"time"
)

func newCollectionsTestApp(t *testing.T) *App {
	t.Helper()

	application := newQuickConnectTestApp(t)
	application.loadCollections()

	return application
}

func TestFavoritesToggleAndPersistence(t *testing.T) {
	application := newCollectionsTestApp(t)

	service := NewCollectionService(application)

	id := storeConfig(t, application, trustedConfig("fav-cfg", 41000))

	if service.IsFavorite(id) {
		t.Fatal("a fresh configuration must not be a favorite")
	}

	now, err := service.ToggleFavorite(id)
	if err != nil {
		t.Fatalf("toggle on: %v", err)
	}

	if !now || !service.IsFavorite(id) {
		t.Fatal("toggle must mark the configuration a favorite")
	}

	favorites := service.Favorites()
	if len(favorites) != 1 || favorites[0] != id {
		t.Fatalf("favorites = %v, want [%s]", favorites, id)
	}

	// Persistence: a fresh app instance over the same workspace sees
	// the same favorite.
	second := NewCollectionService(application)
	if !second.IsFavorite(id) {
		t.Fatal("favorite did not persist through the collections sidecar")
	}

	now, err = service.ToggleFavorite(id)
	if err != nil {
		t.Fatalf("toggle off: %v", err)
	}

	if now || service.IsFavorite(id) {
		t.Fatal("toggle must clear the favorite")
	}

	if _, err := service.ToggleFavorite("   "); err == nil {
		t.Fatal("an empty configuration id must be rejected")
	}
}

func TestUserGroupLifecycle(t *testing.T) {
	application := newCollectionsTestApp(t)

	service := NewCollectionService(application)

	grp, err := service.CreateUserGroup("Work")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	if grp.ID == "" || grp.Name != "Work" {
		t.Fatalf("group = %+v", grp)
	}

	if _, err := service.CreateUserGroup("work"); err == nil {
		t.Fatal("duplicate group names (case-insensitive) must be rejected")
	}

	first := storeConfig(t, application, trustedConfig("grp-a", 42000))
	second := storeConfig(t, application, trustedConfig("grp-b", 42001))

	if err := service.AddToUserGroup(grp.ID, first); err != nil {
		t.Fatalf("add first: %v", err)
	}

	if err := service.AddToUserGroup(grp.ID, first); err != nil {
		t.Fatalf("idempotent add: %v", err)
	}

	if err := service.AddToUserGroup(grp.ID, second); err != nil {
		t.Fatalf("add second: %v", err)
	}

	members, err := service.GroupMembers(grp.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %v (err %v), want 2", members, err)
	}

	groups := service.UserGroups()
	if len(groups) != 1 || groups[0].Count != 2 {
		t.Fatalf("groups = %+v", groups)
	}

	// Persistence across a service rebind.
	rebound := NewCollectionService(application)

	if persisted, err := rebound.GroupMembers(grp.ID); err != nil || len(persisted) != 2 {
		t.Fatalf("group membership did not persist: %v (%v)", persisted, err)
	}

	if err := service.RemoveFromUserGroup(grp.ID, first); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if members, _ := service.GroupMembers(grp.ID); len(members) != 1 {
		t.Fatalf("members after remove = %v, want 1", members)
	}

	if err := service.DeleteUserGroup(grp.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := service.GroupMembers(grp.ID); err == nil {
		t.Fatal("deleted group membership must fail honestly")
	}
}

func TestBuiltinGroupCountsAreMeasured(t *testing.T) {
	application := newCollectionsTestApp(t)

	service := NewCollectionService(application)

	// One working + fast + recently tested configuration.
	working := trustedConfig("working-cfg", 43000)
	working.Working = true
	working.TestedAt = time.Now().UnixMilli()
	working.LatencyMS = 120

	storeConfig(t, application, working)

	// One failed (tested, not working) configuration.
	failed := trustedConfig("failed-cfg", 43001)
	failed.Working = false
	failed.TestedAt = time.Now().UnixMilli()

	storeConfig(t, application, failed)

	// One never-tested configuration.
	storeConfig(t, application, trustedConfig("untested-cfg", 43002))

	// One favorite (the working one).
	workingID := storedConfigByID(t, application, storeConfig(t, application, working)).ID

	if _, err := service.ToggleFavorite(workingID); err != nil {
		t.Fatalf("favorite: %v", err)
	}

	overview, err := service.GroupsOverview()
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	counts := map[string]int{}
	for _, group := range overview.Builtin {
		counts[group.ID] = group.Count
	}

	for _, id := range builtinGroupIDs {
		if _, ok := counts[id]; !ok {
			t.Fatalf("overview is missing the built-in group %q", id)
		}
	}

	if counts["all"] != 3 {
		t.Fatalf("all = %d, want 3", counts["all"])
	}

	if counts["working"] != 1 {
		t.Fatalf("working = %d, want 1 (measured, not invented)", counts["working"])
	}

	if counts["untested"] != 1 {
		t.Fatalf("untested = %d, want 1", counts["untested"])
	}

	if counts["fast"] != 1 {
		t.Fatalf("fast = %d, want 1 (the 120 ms working config)", counts["fast"])
	}

	if counts["recently_tested"] != 2 {
		t.Fatalf("recently_tested = %d, want 2", counts["recently_tested"])
	}

	if counts["favorites"] != 1 {
		t.Fatalf("favorites = %d, want 1", counts["favorites"])
	}
}

func TestGroupFilterPipeline(t *testing.T) {
	application := newCollectionsTestApp(t)

	data := NewDataService(application)
	collections := NewCollectionService(application)

	working := trustedConfig("filter-working", 44000)
	working.Working = true
	working.TestedAt = time.Now().UnixMilli()
	working.LatencyMS = 90

	storeConfig(t, application, working)
	storeConfig(t, application, trustedConfig("filter-untested", 44001))

	if _, err := collections.ToggleFavorite(storedConfigByID(t, application,
		storeConfig(t, application, working)).ID); err != nil {
		t.Fatalf("favorite: %v", err)
	}

	// Favorites group.
	page, err := data.ListConfigsFiltered(ConfigFilter{Group: "favorites"}, 0, 50)
	if err != nil {
		t.Fatalf("favorites filter: %v", err)
	}

	if len(page.Items) != 1 || page.Items[0].Name != "filter-working" {
		t.Fatalf("favorites filter returned %d items", len(page.Items))
	}

	// Working group.
	page, err = data.ListConfigsFiltered(ConfigFilter{Group: "working"}, 0, 50)
	if err != nil {
		t.Fatalf("working filter: %v", err)
	}

	if len(page.Items) != 1 {
		t.Fatalf("working filter returned %d items", len(page.Items))
	}

	// Untested group.
	page, err = data.ListConfigsFiltered(ConfigFilter{Group: "untested"}, 0, 50)
	if err != nil {
		t.Fatalf("untested filter: %v", err)
	}

	if len(page.Items) != 1 || page.Items[0].Name != "filter-untested" {
		t.Fatalf("untested filter returned %d items", len(page.Items))
	}

	// User group.
	grp, err := collections.CreateUserGroup("Travel")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	untestedID := storedConfigByID(t, application,
		storeConfig(t, application, trustedConfig("filter-untested", 44001))).ID

	if err := collections.AddToUserGroup(grp.ID, untestedID); err != nil {
		t.Fatalf("add to group: %v", err)
	}

	page, err = data.ListConfigsFiltered(ConfigFilter{Group: grp.ID}, 0, 50)
	if err != nil {
		t.Fatalf("user group filter: %v", err)
	}

	if len(page.Items) != 1 || page.Items[0].Name != "filter-untested" {
		t.Fatalf("user group filter returned %d items", len(page.Items))
	}

	// A favorite never bypasses the protocol filter: combining group
	// with a non-matching protocol yields zero rows.
	page, err = data.ListConfigsFiltered(ConfigFilter{Group: "favorites", Protocol: "trojan"}, 0, 50)
	if err != nil {
		t.Fatalf("combined filter: %v", err)
	}

	if len(page.Items) != 0 {
		t.Fatalf("combined filter must be empty, returned %d", len(page.Items))
	}
}

func TestCollectionsSidecarSurvivesSchemaBump(t *testing.T) {
	application := newCollectionsTestApp(t)

	service := NewCollectionService(application)

	id := storeConfig(t, application, trustedConfig("schema-cfg", 45000))

	if _, err := service.ToggleFavorite(id); err != nil {
		t.Fatalf("favorite: %v", err)
	}

	// Corrupt the sidecar version: a future schema must not brick the
	// app — the collections start empty and the app keeps working.
	writeCollectionsFile(t, application, []byte(`{"version":9999,"favorites":["gone"]}`))

	fresh := newQuickConnectTestApp(t)
	fresh.opts.BaseDir = application.opts.BaseDir

	// A new app instance over the same workspace starts with empty
	// collections (unknown schema) instead of crashing.
	freshService := NewCollectionService(fresh)

	if freshService.IsFavorite("gone") {
		t.Fatal("an unknown sidecar schema must not load stale data")
	}

	if _, err := freshService.ToggleFavorite(id); err != nil {
		t.Fatalf("favorites must keep working after a schema mismatch: %v", err)
	}
}

// writeCollectionsFile overwrites the sidecar for corruption tests.
func writeCollectionsFile(t *testing.T, application *App, raw []byte) {
	t.Helper()

	if err := os.WriteFile(application.collectionsPath(), raw, 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

func TestSourceReliabilityReportIsEvidenceBased(t *testing.T) {
	application := newCollectionsTestApp(t)

	// Seed the store with per-source evidence.
	working := trustedConfig("rel-working", 46000)
	working.Source = "morpheusadam-best"
	working.Working = true
	working.TestedAt = time.Now().UnixMilli()
	working.LatencyMS = 150
	working.LastSuccessAt = time.Now().UnixMilli()

	storeConfig(t, application, working)

	failed := trustedConfig("rel-failed", 46001)
	failed.Source = "morpheusadam-best"
	failed.Working = false
	failed.TestedAt = time.Now().UnixMilli()

	storeConfig(t, application, failed)

	untested := trustedConfig("rel-untested", 46002)
	untested.Source = "radikal-verified"

	storeConfig(t, application, untested)

	service := NewSourceService(application)

	report, err := service.SourceReliability()
	if err != nil {
		t.Fatalf("reliability: %v", err)
	}

	if report.Overall.PersistedConfigs != 3 {
		t.Fatalf("persisted = %d, want 3", report.Overall.PersistedConfigs)
	}

	if report.Overall.TestedConfigs != 2 || report.Overall.WorkingConfigs != 1 {
		t.Fatalf("tested/working = %d/%d, want 2/1",
			report.Overall.TestedConfigs, report.Overall.WorkingConfigs)
	}

	if report.Overall.SuccessRatePct != 50 {
		t.Fatalf("success rate = %d, want 50 (measured)", report.Overall.SuccessRatePct)
	}

	if report.Overall.UntestedConfigs != 1 {
		t.Fatalf("untested = %d, want 1", report.Overall.UntestedConfigs)
	}

	var entry *SourceHealthEntry

	for i := range report.Sources {
		if report.Sources[i].ID == "morpheusadam-best" {
			entry = &report.Sources[i]

			break
		}
	}

	if entry == nil {
		t.Fatal("the source entry is missing from the report")
	}

	if entry.PersistedConfigs != 2 || entry.TestedConfigs != 2 || entry.WorkingConfigs != 1 {
		t.Fatalf("per-source store evidence = %+v", entry)
	}

	if entry.SuccessRatePct != 50 {
		t.Fatalf("per-source success rate = %d, want 50", entry.SuccessRatePct)
	}

	if entry.MedianLatencyMS != 150 {
		t.Fatalf("median latency = %d, want 150 (measured)", entry.MedianLatencyMS)
	}

	if entry.LastSuccessfulTestAt == 0 {
		t.Fatal("last successful test must carry the measured timestamp")
	}

	// An untested source reports NOT ENOUGH DATA, never an invented score.
	var untestedEntry *SourceHealthEntry

	for i := range report.Sources {
		if report.Sources[i].ID == "radikal-verified" {
			untestedEntry = &report.Sources[i]

			break
		}
	}

	if untestedEntry == nil {
		t.Fatal("the untested source entry is missing")
	}

	if untestedEntry.SuccessRatePct != -1 {
		t.Fatalf("untested source success rate = %d, want -1 (not enough data)",
			untestedEntry.SuccessRatePct)
	}

	if untestedEntry.FetchEnoughData {
		t.Fatal("a never-fetched source must not claim fetch evidence")
	}

	// The report is served from the cache within the TTL.
	cached, err := service.SourceReliability()
	if err != nil {
		t.Fatalf("cached reliability: %v", err)
	}

	if cached.GeneratedAt != report.GeneratedAt {
		t.Fatal("the report must be cached within the TTL window")
	}
}

func TestSourceReliabilityInvalidatedByIngestion(t *testing.T) {
	application := newCollectionsTestApp(t)

	service := NewSourceService(application)

	first, err := service.SourceReliability()
	if err != nil {
		t.Fatalf("first report: %v", err)
	}

	// A completed ingestion cycle (even a failed one records evidence)
	// invalidates the cache: the next call recomputes.
	application.mu.Lock()
	application.lastIngestionAt = time.Now().UTC().UnixMilli()
	application.mu.Unlock()

	application.invalidateReliability()

	time.Sleep(5 * time.Millisecond) // GeneratedAt has millisecond resolution

	second, err := service.SourceReliability()
	if err != nil {
		t.Fatalf("second report: %v", err)
	}

	if second.GeneratedAt == first.GeneratedAt {
		t.Fatal("the report must be recomputed after the evidence changed")
	}
}
