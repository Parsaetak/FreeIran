package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// fakeGetter is an in-memory httpx.Getter for deterministic tests.
type fakeGetter struct {
	handler func(url string) (int, string)
	calls   int64
}

func (f *fakeGetter) Get(ctx context.Context, url string, o httpx.GetOptions) (*httpx.Response, error) {
	atomic.AddInt64(&f.calls, 1)

	if f.handler == nil {
		return &httpx.Response{StatusCode: 404, Body: []byte("{}")}, nil
	}

	code, body := f.handler(url)

	return &httpx.Response{StatusCode: code, Body: []byte(body)}, nil
}

// vlessBody builds a parseable subscription body with n distinct
// endpoints.
func vlessBody(prefix string, n int) string {
	var b strings.Builder

	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "vless://%s@%s-%d.example.com:443?encryption=none&security=tls&sni=example.com&type=tcp#node-%d\n",
			uuidFor(i), prefix, i, i)
	}

	return b.String()
}

func uuidFor(i int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
}

// TestDiscoveryLevelsAndDedup drives a full engine run with one good
// source and one always-failing source and proves:
//
//   - failure isolation (the good source's candidates survive);
//   - normalization + validation actually filter garbage;
//   - cross-source deduplication collapses identical candidates;
//   - stats record every level honestly.
func TestDiscoveryLevelsAndDedup(t *testing.T) {
	good1 := vlessBody("src1", 5)
	good2 := vlessBody("src2", 5)
	shared := vlessBody("shared", 3)

	g := &fakeGetter{handler: func(url string) (int, string) {
		switch {
		case strings.Contains(url, "good1"):
			return 200, good1 + shared
		case strings.Contains(url, "good2"):
			return 200, good2 + shared
		default:
			return 500, "boom"
		}
	}}

	eng := NewEngine(g, EngineConfig{FetchConcurrency: 2, MinSourceCandidates: 1}, "")

	sources := []source.Source{
		{ID: "good1", Name: "g1", URL: "https://example.com/good1.txt", Enabled: true},
		{ID: "good2", Name: "g2", URL: "https://example.com/good2.txt", Enabled: true},
		{ID: "bad", Name: "b", URL: "https://example.com/bad.txt", Enabled: true},
	}

	nodes, stats := eng.Discover(context.Background(), LevelSet{Configured: true}, nil, sources)

	// 5 + 5 + 3 unique endpoints (shared appears in both, deduped).
	if len(nodes) != 13 {
		t.Fatalf("nodes = %d, want 13 (dedup across sources)", len(nodes))
	}

	found := false
	for _, l := range stats.Levels {
		if l.Name == "configured" && l.Failed == 1 && l.FetchedOK == 2 {
			found = true
		}
	}

	if !found {
		t.Fatalf("failed source not isolated in stats: %+v", stats.Levels)
	}

	// The failing source must be in backoff but NOT blacklisted.
	h := eng.Health().Get("bad")
	if h.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", h.ConsecutiveFailures)
	}

	if !h.InBackoff(time.Now()) {
		t.Fatal("failing source should be in backoff")
	}

	// The good sources' health must reflect measured yield. Source
	// completion order is concurrent, so the 3 shared duplicates land
	// on EITHER source — assert the aggregate.
	h1, h2 := eng.Health().Get("good1"), eng.Health().Get("good2")

	if h1.FetchOK != 1 || h1.ParseOK != 1 {
		t.Fatalf("good1 health = %+v", h1)
	}

	if h2.FetchOK != 1 || h2.ParseOK != 1 {
		t.Fatalf("good2 health = %+v", h2)
	}

	if total := h1.Valid + h2.Valid; total != 13 {
		t.Fatalf("aggregate valid = %d, want 13", total)
	}

	if total := h1.Duplicates + h2.Duplicates; total != 3 {
		t.Fatalf("aggregate duplicates = %d, want 3", total)
	}
}

// TestDiscoveryCachedLevel proves level 1 needs no network: cached
// configs flow into the pool untouched, invalid ones are filtered.
func TestDiscoveryCachedLevel(t *testing.T) {
	g := &fakeGetter{} // would fail everything if called

	cached := []config.Config{}

	for i := 0; i < 4; i++ {
		c := config.Config{
			Type:    config.TypeVLESS,
			Address: fmt.Sprintf("cached-%d.example.com", i),
			Port:    443,
			UUID:    uuidFor(100 + i),
			Network: "tcp",
		}
		c.Normalize()
		cached = append(cached, c)
	}

	// One invalid candidate: no address.
	cached = append(cached, config.Config{Type: config.TypeVLESS, Port: 443, UUID: uuidFor(1)})

	eng := NewEngine(g, EngineConfig{}, "")

	nodes, stats := eng.Discover(context.Background(), LevelSet{Cached: true}, cached, nil)

	if len(nodes) != 4 {
		t.Fatalf("nodes = %d, want 4 valid cached nodes", len(nodes))
	}

	if g.calls != 0 {
		t.Fatalf("cached level made %d network calls, want 0", g.calls)
	}

	if len(stats.Levels) != 1 || stats.Levels[0].Name != "cached" {
		t.Fatalf("levels = %+v", stats.Levels)
	}
}

// TestDiscoveryCancellation propagates promptly.
func TestDiscoveryCancellation(t *testing.T) {
	g := &fakeGetter{handler: func(url string) (int, string) {
		return 200, vlessBody("x", 10)
	}}

	eng := NewEngine(g, EngineConfig{}, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sources := []source.Source{{ID: "s", Name: "s", URL: "https://example.com/s.txt", Enabled: true}}

	// A cancelled context yields (at most) nothing; it must not hang
	// or panic.
	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _ = eng.Discover(ctx, LevelSet{Configured: true}, nil, sources)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not respect cancellation")
	}
}

// TestDiscoveryProgressReporting verifies stage/level progress events
// are emitted (no fake animation: real transitions only).
func TestDiscoveryProgressReporting(t *testing.T) {
	g := &fakeGetter{handler: func(url string) (int, string) {
		return 200, vlessBody("p", 3)
	}}

	eng := NewEngine(g, EngineConfig{MinSourceCandidates: 1}, "")

	var events []Progress

	eng.OnProgress(func(p Progress) { events = append(events, p) })

	sources := []source.Source{{ID: "s", Name: "s", URL: "https://example.com/s.txt", Enabled: true}}

	_, _ = eng.Discover(context.Background(), LevelSet{Configured: true}, nil, sources)

	var sawDiscovering, sawParsing, sawComplete bool

	for _, e := range events {
		switch e.Stage {
		case StageDiscovering:
			sawDiscovering = true
		case StageParsing:
			sawParsing = true
		case StageComplete:
			sawComplete = true
		}
	}

	if !sawDiscovering || !sawParsing || !sawComplete {
		t.Fatalf("progress events incomplete: discovering=%v parsing=%v complete=%v",
			sawDiscovering, sawParsing, sawComplete)
	}
}

// TestHealthPersistence round-trips the health file.
func TestHealthPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")

	h := NewHealth(path)
	h.RecordFetch("a", true, 120, "")
	h.RecordYield("a", 10, 8, 2)
	h.Snapshot()

	// A new tracker loads the same state.
	h2 := NewHealth(path)
	got := h2.Get("a")

	if got.FetchOK != 1 || got.Valid != 8 || got.Duplicates != 2 || got.LastLatencyMS != 120 {
		t.Fatalf("health did not round-trip: %+v", got)
	}
}

// TestHealthCorruptFileDiscarded proves a corrupt health file is
// discarded, not fatal.
func TestHealthCorruptFileDiscarded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewHealth(path)
	h.RecordFetch("x", true, 50, "")
	h.Snapshot()

	if got := h.Get("x"); got.FetchOK != 1 {
		t.Fatalf("corrupt file should not block tracking: %+v", got)
	}
}

// TestHealthBackoffBounds verifies exponential backoff with a cap
// (never a permanent blacklist).
func TestHealthBackoffBounds(t *testing.T) {
	now := time.Now()

	h := SourceHealth{
		SourceID:            "s",
		ConsecutiveFailures: 1,
		LastFailureAt:       now.UnixMilli(),
	}

	if d := h.BackoffUntil().Sub(now); d > backoffBase*2 {
		t.Fatalf("first backoff too long: %v", d)
	}

	h.ConsecutiveFailures = 8
	if d := h.BackoffUntil().Sub(now); d > maxBackoff {
		t.Fatalf("backoff exceeded cap: %v > %v", d, maxBackoff)
	}

	h.ConsecutiveFailures = 0
	if !h.BackoffUntil().IsZero() {
		t.Fatal("healthy source must not be in backoff")
	}
}

// TestHealthRankOrdering verifies healthy sources rank above unknown
// which rank above backing-off sources, deterministically.
func TestHealthRankOrdering(t *testing.T) {
	h := NewHealth("")

	// healthy: fetched, parsed, yielded
	h.RecordFetch("healthy", true, 100, "")
	h.RecordParse("healthy", true)
	h.RecordYield("healthy", 10, 9, 1)

	// failing: in backoff
	h.RecordFetch("failing", false, 0, "timeout")

	now := time.Now()

	got := h.Rank(now, []string{"unknown", "failing", "healthy", "zzz"})

	if len(got) != 4 {
		t.Fatalf("rank result = %v", got)
	}

	if got[3] != "failing" {
		t.Fatalf("backoff source must rank last: %v", got)
	}

	if got[0] != "healthy" {
		t.Fatalf("healthy source must rank first: %v", got)
	}
}

// TestSearchBudgetsAndDedup drives the searcher against a fake API
// and verifies: query budget, cross-query repo dedup, rate-limit
// backoff, and staleness demotion.
func TestSearchBudgetsAndDedup(t *testing.T) {
	apiBody := func(repos ...string) string {
		var items []string

		for _, r := range repos {
			pushed := time.Now().UTC().Format(time.RFC3339)
			if strings.Contains(r, ":stale") {
				r = strings.TrimSuffix(r, ":stale")
				pushed = time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339)
			}

			items = append(items, fmt.Sprintf(
				`{"full_name":%q,"description":"d","default_branch":"main","pushed_at":%q,"stargazers_count":10,"html_url":"x"}`,
				r, pushed))
		}

		return `{"total_count":` + fmt.Sprint(len(items)) + `,"items":[` + strings.Join(items, ",") + `]}`
	}

	g := &fakeGetter{handler: func(url string) (int, string) {
		if strings.Contains(url, "search/repositories") {
			return 200, apiBody("alpha/beta", "gamma/delta:stale")
		}

		return 200, vlessBody("probe", 5)
	}}

	s := NewSearcher(g, SearchConfig{MaxQueries: 2, MaxRepos: 5, CacheTTL: time.Hour})

	cands, err := s.Search(context.Background(), []string{"q1", "q2", "q3", "q4"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// Both queries return the same two repos → dedup to 2.
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2 (deduped across queries)", len(cands))
	}

	// Fresh repo first, stale demoted.
	if cands[0].Repo != "alpha/beta" {
		t.Fatalf("fresh repo must outrank stale: %+v", cands)
	}

	// The query budget clamped 4 queries to 2.
	if got := atomic.LoadInt64(&g.calls); got != 2 {
		t.Fatalf("api calls = %d, want 2 (budget)", got)
	}
}

// TestSearchRateLimitBackoff verifies a 403/429 backs the searcher
// off and subsequent searches short-circuit.
func TestSearchRateLimitBackoff(t *testing.T) {
	var calls int64

	g := &fakeGetter{handler: func(url string) (int, string) {
		atomic.AddInt64(&calls, 1)
		return 403, `{"message":"rate limited"}`
	}}

	s := NewSearcher(g, SearchConfig{RateLimitBackoff: time.Hour})

	if _, err := s.Search(context.Background(), []string{"q"}, time.Now()); err == nil {
		t.Fatal("403 must fail the search")
	}

	if !s.RateLimited(time.Now()) {
		t.Fatal("searcher must be in backoff after 403")
	}

	// Second search short-circuits without another API call.
	if _, err := s.Search(context.Background(), []string{"q"}, time.Now()); err == nil {
		t.Fatal("backed-off search must fail fast")
	}

	if got := atomic.LoadInt64(&g.calls); got != 1 {
		t.Fatalf("api calls = %d, want 1 (backoff short-circuit)", got)
	}
}

// TestSearchCaching verifies results are reused within the TTL.
func TestSearchCaching(t *testing.T) {
	var calls int64

	body := `{"total_count":1,"items":[{"full_name":"a/b","description":"","default_branch":"main","pushed_at":"2026-01-01T00:00:00Z","stargazers_count":1,"html_url":"x"}]}`

	g := &fakeGetter{handler: func(url string) (int, string) {
		atomic.AddInt64(&calls, 1)
		return 200, body
	}}

	s := NewSearcher(g, SearchConfig{CacheTTL: time.Hour})

	if _, err := s.Search(context.Background(), []string{"q"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Search(context.Background(), []string{"q"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt64(&g.calls); got != 1 {
		t.Fatalf("api calls = %d, want 1 (cache hit)", got)
	}
}

// TestContentRefs verifies bounded, deduplicated raw-endpoint
// extraction with HTML page exclusion.
func TestContentRefs(t *testing.T) {
	body := `
Some readme with links:
https://raw.githubusercontent.com/foo/bar/main/sub.txt
https://raw.githubusercontent.com/foo/bar/main/sub.txt (duplicate)
https://github.com/foo/bar/blob/main/sub.txt (HTML page - excluded)
https://example.com/configs/list.yaml
https://cdn.example.net/aggregator.json
` + strings.Repeat("https://raw.githubusercontent.com/zz/yy/main/f%d.txt\n", 20)

	refs := ContentRefs([]byte(body))

	if len(refs) > MaxContentRefs {
		t.Fatalf("refs = %d, want <= %d (bounded)", len(refs), MaxContentRefs)
	}

	for _, r := range refs {
		if strings.Contains(r, "github.com/") && !strings.Contains(r, "raw.githubusercontent.com") {
			t.Fatalf("HTML page accepted: %s", r)
		}
	}

	if len(refs) == 0 {
		t.Fatal("no references extracted")
	}
}

// TestDiscoverySearchLevelProbes drives the full search level with
// probe-URL routing and verifies the probe budget is honoured and
// valid content becomes candidates.
func TestDiscoverySearchLevelProbes(t *testing.T) {
	repoSearch := `{"total_count":1,"items":[{"full_name":"probe/repo","description":"","default_branch":"main","pushed_at":"` +
		time.Now().UTC().Format(time.RFC3339) + `","stargazers_count":5,"html_url":"x"}]}`

	var probes int64

	g := &fakeGetter{handler: func(url string) (int, string) {
		switch {
		case strings.Contains(url, "search/repositories"):
			return 200, repoSearch
		case strings.Contains(url, "raw.githubusercontent.com"):
			atomic.AddInt64(&probes, 1)

			// Only README.md carries candidates.
			if strings.HasSuffix(url, "README.md") {
				return 200, vlessBody("search", 6)
			}

			return 404, "not found"
		default:
			return 404, ""
		}
	}}

	eng := NewEngine(g, EngineConfig{
		Search:              SearchConfig{MaxQueries: 1, MaxRepos: 1, MaxProbes: 4, CacheTTL: time.Hour},
		MinSourceCandidates: 3,
	}, "")

	nodes, stats := eng.Discover(context.Background(), LevelSet{Search: true, ForceAll: true}, nil, nil)

	if len(nodes) != 6 {
		t.Fatalf("nodes = %d, want 6 from the probed README", len(nodes))
	}

	if probes > 4 {
		t.Fatalf("probe calls = %d, want <= 4 (budget)", probes)
	}

	found := false
	for _, l := range stats.Levels {
		if l.Name == "search" && l.Valid == 6 {
			found = true
		}
	}

	if !found {
		t.Fatalf("search level stats wrong: %+v", stats.Levels)
	}
}

// TestDiscoveryContentLevel verifies references inside fetched
// content are discovered and fetched at the content level.
func TestDiscoveryContentLevel(t *testing.T) {
	g := &fakeGetter{handler: func(url string) (int, string) {
		switch {
		case strings.Contains(url, "main-source.txt"):
			// The primary source references a sibling subscription.
			return 200, "see also https://raw.githubusercontent.com/up/steam/main/nodes.txt\n" + vlessBody("main", 2)
		case strings.Contains(url, "up/steam/main/nodes.txt"):
			return 200, vlessBody("steam", 4)
		default:
			return 404, ""
		}
	}}

	eng := NewEngine(g, EngineConfig{MinSourceCandidates: 1, TargetCandidates: 1000}, "")

	sources := []source.Source{
		{ID: "main", Name: "m", URL: "https://example.com/main-source.txt", Enabled: true},
	}

	nodes, stats := eng.Discover(context.Background(), LevelSet{Configured: true, Content: true, ForceAll: true}, nil, sources)

	total := 2 + 4
	if len(nodes) != total {
		t.Fatalf("nodes = %d, want %d (main + content-derived)", len(nodes), total)
	}

	var contentLevel *LevelStats
	for i := range stats.Levels {
		if stats.Levels[i].Name == "content" {
			contentLevel = &stats.Levels[i]
		}
	}

	if contentLevel == nil || contentLevel.Valid != 4 {
		t.Fatalf("content level stats wrong: %+v", stats.Levels)
	}
}

// TestTargetSatisfactionSkipsSearch proves later levels are skipped
// when the pool already satisfies the target.
func TestTargetSatisfactionSkipsSearch(t *testing.T) {
	g := &fakeGetter{handler: func(url string) (int, string) {
		if strings.Contains(url, "search/repositories") {
			t.Fatal("search must not run when the target is satisfied")
		}

		return 200, vlessBody("t", 10)
	}}

	eng := NewEngine(g, EngineConfig{TargetCandidates: 5, MinSourceCandidates: 1}, "")

	sources := []source.Source{{ID: "s", Name: "s", URL: "https://example.com/s.txt", Enabled: true}}

	nodes, stats := eng.Discover(
		context.Background(),
		LevelSet{Configured: true, Search: true, Content: true}, // no ForceAll
		nil, sources,
	)

	if len(nodes) != 10 {
		t.Fatalf("nodes = %d, want 10", len(nodes))
	}

	for _, l := range stats.Levels {
		if (l.Name == "search" || l.Name == "content") && !l.Skipped {
			t.Fatalf("level %s should be marked skipped: %+v", l.Name, l)
		}
	}
}

// TestDiscoveryOverRealServerNotRequired documents that the engine
// is fully covered by the fake-getter tests above; real-network
// validation runs out-of-band on operator machines (the v0.9.8.5
// tools/neteval helper was removed with the repository cleanup —
// its ad-hoc harness is not part of the product or CI surface),
// never by the CI matrix.
func TestDiscoveryOverRealServerNotRequired(t *testing.T) {
	t.Skip("real-network validation runs out-of-band on operator machines")
}

// Compile-time interface check: the engine tolerates the production
// httpx client.
var _ httpx.Getter = (*httpx.Client)(nil)
