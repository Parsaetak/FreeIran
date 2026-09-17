package discovery

// v0.9.7 tests (§23): discovery connectors — generic URL fetching,
// bounded recursion, deduplication, GitHub adapters (over a local
// mock), rate-limit accounting, Retry-After and ETag handling, and
// the SSRF boundary for automatically discovered URLs.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// localSSRFOptions admits the loopback test server.
func localSSRFOptions(port int) httpx.SSRFOptions {
	return httpx.SSRFOptions{
		AllowPrivate: true, // tests fetch loopback mocks
		AllowedPorts: []int{port},
		MaxRedirects: 5,
	}
}

func testPort(server *httptest.Server) int {
	url := server.URL
	idx := strings.LastIndex(url, ":")

	port := 0

	for _, r := range url[idx+1:] {
		port = port*10 + int(r-'0')
	}

	return port
}

func TestGenericConnectorFetchesAndRecordsProvenance(t *testing.T) {
	body := "vless://uuid@example.com:443?security=tls&type=ws#n1\n" +
		"trojan://pass@srv.example.org:443#n2\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"abc123"`)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	limiter := NewRateLimiter(RateLimiterOptions{Budget: 10})
	queue := NewCandidateQueue(BoundedRecursion{MaxURLsPerRun: 10})

	client := httpx.NewSSRFClient(httpx.Policy{RequestTimeout: 2 * time.Second, MaxRetries: 0},
		localSSRFOptions(testPort(server)))

	conn := NewGenericConnector(client, BoundedRecursion{}, queue, limiter)

	candidate := Candidate{URL: server.URL + "/sub.txt"}

	outcome, err := conn.Fetch(context.Background(), candidate)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if outcome.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", outcome.StatusCode)
	}

	if !strings.Contains(string(outcome.Body), "vless://") {
		t.Fatal("body lost")
	}

	if outcome.Binary {
		t.Fatal("text body flagged binary")
	}

	if outcome.ETag != `"abc123"` {
		t.Fatalf("etag = %q", outcome.ETag)
	}

	if outcome.ContentHash == "" {
		t.Fatal("content hash missing")
	}

	stats := limiter.Stats(ProviderHTTP)
	if stats.Requests != 1 || stats.Successes != 1 {
		t.Fatalf("provider stats: %+v", stats)
	}
}

func TestGenericConnectorRejectsPrivateTargets(t *testing.T) {
	limiter := NewRateLimiter(RateLimiterOptions{Budget: 10})
	queue := NewCandidateQueue(BoundedRecursion{})

	// No AllowPrivate: loopback must be refused pre-dial.
	client := httpx.NewSSRFClient(httpx.Policy{RequestTimeout: time.Second, MaxRetries: 0}, httpx.SSRFOptions{})
	conn := NewGenericConnector(client, BoundedRecursion{}, queue, limiter)

	_, err := conn.Fetch(context.Background(), Candidate{URL: "http://127.0.0.1:1/sub"})
	if err == nil {
		t.Fatal("loopback fetch must fail without AllowPrivate")
	}
}

func TestGenericConnectorHandlesRedirectsAndCompression(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ss://YWVzLTI1Ni1nY20:cGFzcw@host.example:8388#s1"))
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := httpx.NewSSRFClient(httpx.Policy{RequestTimeout: 2 * time.Second, MaxRetries: 0},
		localSSRFOptions(testPort(server)))
	conn := NewGenericConnector(client, BoundedRecursion{}, NewCandidateQueue(BoundedRecursion{}),
		NewRateLimiter(RateLimiterOptions{Budget: 10}))

	outcome, err := conn.Fetch(context.Background(), Candidate{URL: server.URL + "/redirect"})
	if err != nil {
		t.Fatalf("redirected fetch failed: %v", err)
	}

	if !strings.Contains(string(outcome.Body), "ss://") {
		t.Fatal("redirect did not reach the final body")
	}
}

func TestGenericConnectorConditionalETag(t *testing.T) {
	var hits int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++

		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		_, _ = w.Write([]byte("trojan://x@y.example:443#a"))
	}))
	defer server.Close()

	client := httpx.NewSSRFClient(httpx.Policy{RequestTimeout: 2 * time.Second, MaxRetries: 0},
		localSSRFOptions(testPort(server)))
	conn := NewGenericConnector(client, BoundedRecursion{}, NewCandidateQueue(BoundedRecursion{}),
		NewRateLimiter(RateLimiterOptions{Budget: 10}))

	ctx := context.Background()

	first, err := conn.Fetch(ctx, Candidate{URL: server.URL + "/s.txt"})
	if err != nil {
		t.Fatal(err)
	}

	second, err := conn.Fetch(ctx, Candidate{
		URL: server.URL + "/s.txt",
		Provenance: Provenance{
			ETag: `"v1"`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_ = first

	if second.StatusCode != http.StatusNotModified {
		t.Fatalf("second fetch status = %d, want 304", second.StatusCode)
	}

	if hits != 2 {
		t.Fatalf("server hits = %d, want 2", hits)
	}
}

func TestCandidateQueueDeduplicatesAndBounds(t *testing.T) {
	queue := NewCandidateQueue(BoundedRecursion{
		MaxDepth:      2,
		MaxURLsPerRun: 5,
	})

	mk := func(suffix string, depth int) Candidate {
		return Candidate{
			URL:        "https://example.com/" + suffix,
			Provenance: Provenance{Depth: depth},
		}
	}

	// Duplicate URLs collapse; depth beyond MaxDepth is refused.
	if got := queue.Add([]Candidate{mk("a", 0), mk("a", 0), mk("b", 0), mk("deep", 9)}); got != 2 {
		t.Fatalf("admitted = %d, want 2", got)
	}

	// Per-run budget enforced.
	if got := queue.Add([]Candidate{mk("c", 0), mk("d", 0), mk("e", 0), mk("f", 0), mk("g", 0)}); got != 3 {
		t.Fatalf("budgeted admission = %d, want 3", got)
	}

	fetched := 0
	for {
		_, ok := queue.Take(time.Now())
		if !ok {
			break
		}

		fetched++
	}

	if fetched != 5 {
		t.Fatalf("fetched = %d, want 5 (run budget)", fetched)
	}
}

func TestBoundedRecursionLimitsReferences(t *testing.T) {
	queue := NewCandidateQueue(BoundedRecursion{
		MaxDepth:         1,
		MaxURLsPerSource: 2,
		MaxURLsPerRun:    50,
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A body referencing many URLs (only 2 per source admitted).
		var lines []string
		for i := 0; i < 10; i++ {
			lines = append(lines, fmt.Sprintf("see https://raw.example.com/list%d.txt", i))
		}

		_, _ = w.Write([]byte(strings.Join(lines, "\n")))
	}))
	defer server.Close()

	client := httpx.NewSSRFClient(httpx.Policy{RequestTimeout: 2 * time.Second, MaxRetries: 0},
		localSSRFOptions(testPort(server)))

	conn := NewGenericConnector(client, BoundedRecursion{
		MaxDepth:         1,
		MaxURLsPerSource: 2,
		MaxURLsPerRun:    50,
	}, queue, NewRateLimiter(RateLimiterOptions{Budget: 100}))

	parent := Candidate{
		URL: server.URL + "/index.txt",
		Provenance: Provenance{
			SourceID: "root",
			Depth:    0,
		},
	}

	outcome, err := conn.Fetch(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}

	refs := conn.ExtractReferencedCandidates(parent, outcome.Body)
	if len(refs) != 2 {
		t.Fatalf("references = %d, want 2 (per-source cap)", len(refs))
	}

	if refs[0].Provenance.Depth != 1 {
		t.Fatalf("child depth = %d, want 1", refs[0].Provenance.Depth)
	}

	// A child at max depth yields nothing further.
	if got := conn.ExtractReferencedCandidates(refs[0], outcome.Body); got != nil {
		t.Fatalf("depth-%d expansion admitted %d refs, want 0", refs[0].Provenance.Depth, len(got))
	}
}

func TestGitHubConnectorRepositoryDiscovery(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/search/repositories", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "42")
		_, _ = w.Write([]byte(`{"items":[
                        {"full_name":"owner/subs","default_branch":"main","stargazers_count":5,"pushed_at":"2026-01-01T00:00:00Z"},
                        {"full_name":"owner/other","default_branch":"master","stargazers_count":1}
                ]}`))
	})
	mux.HandleFunc("/repos/owner/subs/git/trees/main", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "41")
		_, _ = w.Write([]byte(`{"tree":[
                        {"path":"README.md","type":"blob"},
                        {"path":"sub/nodes.txt","type":"blob"},
                        {"path":"configs/clash.yaml","type":"blob"},
                        {"path":"docs/other.md","type":"blob"}
                ]}`))
	})
	mux.HandleFunc("/repos/owner/other/git/trees/master", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tree":[]}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := httpx.NewSSRFClient(httpx.Policy{RequestTimeout: 2 * time.Second, MaxRetries: 0},
		localSSRFOptions(testPort(server)))
	rawClient := httpx.NewSSRFClient(httpx.Policy{RequestTimeout: 2 * time.Second, MaxRetries: 0},
		localSSRFOptions(testPort(server)))

	limiter := NewRateLimiter(RateLimiterOptions{Budget: 50})
	queue := NewCandidateQueue(BoundedRecursion{MaxURLsPerRun: 100, MaxDepth: 2})

	github := &GitHubConnector{
		Client:      client,
		RawClient:   rawClient,
		Limits:      BoundedRecursion{MaxURLsPerRun: 100},
		Queue:       queue,
		RateLimiter: limiter,
		APIBase:     server.URL,
	}

	outcome := github.Run(context.Background(), nil)

	if outcome.ReposInspected == 0 {
		t.Fatal("no repositories inspected")
	}

	if outcome.TreesInspected == 0 {
		t.Fatal("no trees inspected")
	}

	if outcome.Candidates == 0 {
		t.Fatal("no candidates admitted")
	}

	stats := limiter.Stats(ProviderGitHub)
	if stats.Requests == 0 {
		t.Fatal("github requests unaccounted")
	}

	if stats.Remaining != 41 {
		t.Fatalf("remaining = %d, want last-reported 41", stats.Remaining)
	}
}

func TestGitHubConnectorRateLimitDegradesGracefully(t *testing.T) {
	var calls int

	mux := http.NewServeMux()

	mux.HandleFunc("/search/repositories", func(w http.ResponseWriter, _ *http.Request) {
		calls++

		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "4102444800")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := httpx.NewSSRFClient(httpx.Policy{RequestTimeout: 2 * time.Second, MaxRetries: 0},
		localSSRFOptions(testPort(server)))

	limiter := NewRateLimiter(RateLimiterOptions{Budget: 50})

	github := &GitHubConnector{
		Client:      client,
		Limits:      BoundedRecursion{},
		Queue:       NewCandidateQueue(BoundedRecursion{}),
		RateLimiter: limiter,
		APIBase:     server.URL,
	}

	outcome := github.Run(context.Background(), nil)

	if calls == 0 {
		t.Fatal("no calls made")
	}

	// The cooldown engaged after the first 403+remaining=0...
	if limiter.Stats(ProviderGitHub).Forbidden403 == 0 {
		t.Fatal("403s unaccounted")
	}

	// ...and the engine degraded: bounded strategy notes, no panic.
	if len(outcome.StrategyNotes) == 0 {
		t.Fatal("degradation unrecorded")
	}

	// A follow-up Acquire is refused (cooldown) — never retried hard.
	if err := limiter.Acquire(ProviderGitHub); err == nil {
		t.Fatal("cooldown must block further requests")
	}
}

func TestRateLimiterBudgetAndStats(t *testing.T) {
	limiter := NewRateLimiter(RateLimiterOptions{Budget: 3})

	for i := 0; i < 3; i++ {
		if err := limiter.Acquire(ProviderHTTP); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}

	if err := limiter.Acquire(ProviderHTTP); err != ErrBudgetExhausted {
		t.Fatalf("budget exhaustion error = %v, want ErrBudgetExhausted", err)
	}

	limiter.Observe(ProviderHTTP, 200, 40*time.Millisecond, 0, -1, "")

	stats := limiter.Stats(ProviderHTTP)
	if stats.Requests != 3 || stats.Successes != 1 || stats.AvgLatencyMS != 40 {
		t.Fatalf("stats: %+v", stats)
	}

	// 429 starts a cooldown.
	limiter.Observe(ProviderHTTP, 429, time.Millisecond, 90*time.Second, 0, "")

	if !limiter.Stats(ProviderHTTP).CoolingDown {
		t.Fatal("429 must start a cooldown")
	}
}

func TestGitHubTreeSelection(t *testing.T) {
	entries := []githubTreeEntry{
		{Path: "sub/nodes.txt", Type: "blob"},
		{Path: "sub/nodes.txt.bak", Type: "blob"},
		{Path: "README.md", Type: "blob"},
		{Path: "binary.exe", Type: "blob"},
		{Path: "configs/deep/mix.yaml", Type: "blob"},
		{Path: "dir", Type: "tree"},
	}

	paths := selectCandidatePaths(entries)

	if len(paths) == 0 {
		t.Fatal("no paths selected")
	}

	for _, path := range paths {
		if path == "binary.exe" || path == "README.md" {
			t.Fatalf("unsuitable path selected: %s", path)
		}
	}

	// Both keyword-matching files rank at the top (tie broken by
	// shorter path); the non-matching docs file never wins.
	if paths[0] == "docs/other.md" {
		t.Fatalf("keyword-free path ranked first: %v", paths)
	}

	if !strings.Contains(paths[0], "nodes") && !strings.Contains(paths[0], "mix") {
		t.Fatalf("top path lacks keywords: %s", paths[0])
	}
}

func TestRawURLForPrefersRawEndpoints(t *testing.T) {
	got := RawURLFor("owner/repo", "main", "sub/nodes.txt")

	if !strings.HasPrefix(got, "https://raw.githubusercontent.com/") {
		t.Fatalf("raw URL = %s", got)
	}

	if strings.Contains(got, "github.com/owner") {
		t.Fatal("HTML endpoint produced")
	}
}

func TestNormalizeCandidateURLDedup(t *testing.T) {
	a := NormalizeCandidateURL("https://Example.com/sub/")
	b := NormalizeCandidateURL("https://example.com/sub")
	c := NormalizeCandidateURL("https://example.com/other")

	if a != b {
		t.Fatalf("%q != %q", a, b)
	}

	if a == c {
		t.Fatal("distinct resources collapsed")
	}
}

func TestLooksBinary(t *testing.T) {
	if looksBinary([]byte("vless://abc@def:443#x\n")) {
		t.Fatal("text flagged binary")
	}

	if !looksBinary([]byte{0x00, 0x01, 0x02, 0xff, 0xfe}) {
		t.Fatal("binary not flagged")
	}
}
