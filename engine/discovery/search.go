package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// search.go implements smart node search (v0.9.6 §6): instead of a
// fixed hardcoded source list being the only entry point, the
// discovery engine can SEARCH for public configuration candidates —
// GitHub repositories that publish subscription-style configuration
// files — using search terms appropriate to the supported protocols
// and configuration formats, then probe a small, bounded set of
// conventional raw-file paths and VALIDATE the content by parsing it
// (only content that yields valid candidates is accepted).
//
// Discipline (the anti-hammering rules):
//
//   - search budget: at most MaxQueries GitHub API searches per
//     cycle (default 3);
//   - repo budget: at most MaxRepos repositories per cycle
//     (default 12), ordered by GitHub's relevance ranking;
//   - probe budget: at most MaxProbes raw-file fetches per cycle
//     (default 24), spread across the candidate paths;
//   - request timeout: every request is bounded;
//   - rate-limit awareness: a 403/429 from the API backs the whole
//     searcher off for RateLimitBackoff (default 10 minutes);
//   - result deduplication: by repository full name and by raw URL;
//   - stale-result filtering: search results carry GitHub's
//     pushed_at; repositories not updated within StaleAfter
//     (default 30 days) rank last;
//   - cache expiration: the repository list per query is cached for
//     CacheTTL (default 6 hours);
//   - content validation: a probed file only becomes a source when
//     its body parses into at least MinValidCandidates valid
//     configurations (default 3 — one lucky URI is not a source).
//
// Unauthenticated GitHub API use is rate limited to 10 searches per
// minute per IP; the budgets above keep a discovery cycle well inside
// that. Code search would require authentication, so repository
// search + raw path probing is the unauthenticated-compatible design.

// SearchConfig tunes the searcher. Zero values select defaults.
type SearchConfig struct {
	// MaxQueries bounds GitHub API search requests per cycle.
	MaxQueries int

	// MaxRepos bounds repositories considered per cycle.
	MaxRepos int

	// MaxProbes bounds raw-file fetches per cycle.
	MaxProbes int

	// RequestTimeout bounds one HTTP request.
	RequestTimeout time.Duration

	// RateLimitBackoff is how long the searcher pauses after a
	// 403/429 from the API.
	RateLimitBackoff time.Duration

	// CacheTTL is how long search results are reused.
	CacheTTL time.Duration

	// StaleAfter demotes repositories not pushed recently.
	StaleAfter time.Duration

	// MinValidCandidates is the parse yield a probed file must have
	// to become a source.
	MinValidCandidates int
}

// DefaultSearchConfig returns the safe defaults described above.
func DefaultSearchConfig() SearchConfig {
	return SearchConfig{
		MaxQueries:         3,
		MaxRepos:           12,
		MaxProbes:          24,
		RequestTimeout:     15 * time.Second,
		RateLimitBackoff:   10 * time.Minute,
		CacheTTL:           6 * time.Hour,
		StaleAfter:         30 * 24 * time.Hour,
		MinValidCandidates: 3,
	}
}

func (c SearchConfig) normalize() SearchConfig {
	if c.MaxQueries <= 0 {
		c.MaxQueries = 3
	}

	if c.MaxRepos <= 0 {
		c.MaxRepos = 12
	}

	if c.MaxProbes <= 0 {
		c.MaxProbes = 24
	}

	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 15 * time.Second
	}

	if c.RateLimitBackoff <= 0 {
		c.RateLimitBackoff = 10 * time.Minute
	}

	if c.CacheTTL <= 0 {
		c.CacheTTL = 6 * time.Hour
	}

	if c.StaleAfter <= 0 {
		c.StaleAfter = 30 * 24 * time.Hour
	}

	if c.MinValidCandidates <= 0 {
		c.MinValidCandidates = 3
	}

	return c
}

// DefaultSearchQueries are the search terms for the supported
// protocols and configuration formats. Terms are chosen for what the
// aggregators actually call their repositories, verified against the
// formats engine/parser accepts (URI subscriptions, base64
// subscriptions, Clash YAML).
var DefaultSearchQueries = []string{
	"v2ray free config subscription",
	"vless config aggregator",
	"free proxy config daily",
	"clash config aggregator",
	"proxy subscription merged",
}

// probePaths are the conventional raw file locations aggregators
// publish to, derived from the paths the built-in registry's sources
// actually use (README.md-style inline URIs, sub.txt bundles,
// protocol-specific .txt dumps). The list is ordered by observed
// frequency; the probe budget is spread across it.
var probePaths = []string{
	"README.md",
	"sub.txt",
	"configs.txt",
	"merged.txt",
	"config.txt",
	"base64.txt",
	"vless.txt",
	"vmess.txt",
	"trojan.txt",
	"proxies.txt",
	"Eternity.txt",
	"sub/mix.txt",
}

// githubRepoSearchResult is the subset of the GitHub repository
// search response the searcher consumes.
type githubRepoSearchResult struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		FullName      string    `json:"full_name"`
		Description   string    `json:"description"`
		DefaultBranch string    `json:"default_branch"`
		PushedAt      time.Time `json:"pushed_at"`
		Stars         int       `json:"stargazers_count"`
		HTMLURL       string    `json:"html_url"`
	} `json:"items"`
}

// SearchCandidate is one discovered repository candidate.
type SearchCandidate struct {
	// Repo is "owner/name".
	Repo string `json:"repo"`

	// Branch is the default branch for raw URL construction.
	Branch string `json:"branch"`

	// Description is the repository's own description.
	Description string `json:"description,omitempty"`

	// PushedAt is the repository's last push (staleness filter).
	PushedAt time.Time `json:"pushed_at"`

	// Stars is the repository's star count (display only).
	Stars int `json:"stars"`

	// Query is the search term that surfaced the repository.
	Query string `json:"query"`
}

// Searcher performs the bounded GitHub search. Safe for concurrent
// use.
type Searcher struct {
	getter httpx.Getter
	cfg    SearchConfig

	mu          sync.Mutex
	cache       map[string]cachedSearch
	backoffTill time.Time
}

type cachedSearch struct {
	candidates []SearchCandidate
	cachedAt   time.Time
}

// NewSearcher creates a searcher over the supplied HTTP getter
// (nil = the shared production client).
func NewSearcher(getter httpx.Getter, cfg SearchConfig) *Searcher {
	if getter == nil {
		getter = httpx.Default()
	}

	return &Searcher{getter: getter, cfg: cfg.normalize(), cache: make(map[string]cachedSearch)}
}

// RateLimited reports whether the searcher is currently backing off
// from the GitHub API.
func (s *Searcher) RateLimited(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return now.Before(s.backoffTill)
}

// Search returns repository candidates for the given queries within
// the configured budgets. Results are deduplicated by repository
// name across queries and ordered by (fresh, more stars, name).
func (s *Searcher) Search(ctx context.Context, queries []string, now time.Time) ([]SearchCandidate, error) {
	if s.RateLimited(now) {
		return nil, fmt.Errorf("discovery: search backed off until %s", s.backoffTill.Format(time.RFC3339))
	}

	if len(queries) == 0 {
		queries = DefaultSearchQueries
	}

	if len(queries) > s.cfg.MaxQueries {
		queries = queries[:s.cfg.MaxQueries]
	}

	byRepo := make(map[string]SearchCandidate)

	var (
		queryErrs []string
		failedAll = true
	)

	for _, q := range queries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		cands, err := s.searchOne(ctx, q, now)
		if err != nil {
			// A failed query fails alone; the remaining queries still
			// run (failure isolation).
			queryErrs = append(queryErrs, q+": "+err.Error())

			continue
		}

		failedAll = false

		for _, c := range cands {
			if _, dup := byRepo[c.Repo]; !dup {
				byRepo[c.Repo] = c
			}
		}
	}

	// Every query failed: surface the failure (the caller decides
	// whether a search level failure is fatal — it never is for the
	// overall discovery run).
	if failedAll && len(queryErrs) > 0 {
		return nil, fmt.Errorf("discovery: all %d search queries failed: %s",
			len(queryErrs), strings.Join(queryErrs, "; "))
	}

	out := make([]SearchCandidate, 0, len(byRepo))
	for _, c := range byRepo {
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool {
		// Fresh first, then stars, then name for determinism.
		iStale := now.Sub(out[i].PushedAt) > s.cfg.StaleAfter
		jStale := now.Sub(out[j].PushedAt) > s.cfg.StaleAfter

		if iStale != jStale {
			return !iStale
		}

		if out[i].Stars != out[j].Stars {
			return out[i].Stars > out[j].Stars
		}

		return out[i].Repo < out[j].Repo
	})

	if len(out) > s.cfg.MaxRepos {
		out = out[:s.cfg.MaxRepos]
	}

	return out, nil
}

// searchOne executes one query with caching.
func (s *Searcher) searchOne(ctx context.Context, query string, now time.Time) ([]SearchCandidate, error) {
	// Cache hit?
	s.mu.Lock()

	if c, ok := s.cache[query]; ok && now.Sub(c.cachedAt) < s.cfg.CacheTTL {
		s.mu.Unlock()

		return c.candidates, nil
	}

	s.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()

	apiURL := "https://api.github.com/search/repositories?q=" + sanitizeQuery(query) +
		"&sort=updated&order=desc&per_page=10"

	resp, err := s.getter.Get(reqCtx, apiURL, httpx.GetOptions{
		Header: map[string]string{"Accept": "application/vnd.github+json"},
	})
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		s.mu.Lock()
		s.backoffTill = now.Add(s.cfg.RateLimitBackoff)
		s.mu.Unlock()

		return nil, fmt.Errorf("discovery: github api rate limited (HTTP %d)", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery: github search HTTP %d", resp.StatusCode)
	}

	var decoded githubRepoSearchResult
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		return nil, fmt.Errorf("discovery: decode search response: %w", err)
	}

	cands := make([]SearchCandidate, 0, len(decoded.Items))

	for _, item := range decoded.Items {
		if item.FullName == "" {
			continue
		}

		branch := item.DefaultBranch
		if branch == "" {
			branch = "main"
		}

		cands = append(cands, SearchCandidate{
			Repo:        item.FullName,
			Branch:      branch,
			Description: item.Description,
			PushedAt:    item.PushedAt,
			Stars:       item.Stars,
			Query:       query,
		})
	}

	s.mu.Lock()
	s.cache[query] = cachedSearch{candidates: cands, cachedAt: now}
	s.mu.Unlock()

	return cands, nil
}

// RawURLFor builds the raw.githubusercontent.com URL for a candidate
// repository and path. Only raw endpoints are ever used — the HTML
// blob pages are never scraped.
func RawURLFor(repo, branch, path string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", repo, branch, path)
}

// ProbePathsFor returns the bounded raw-path probe list for one
// repository: the full conventional list when probes are plentiful,
// a deterministic prefix when they are scarce.
func ProbePathsFor(repo string, probeBudget int) []string {
	if probeBudget >= len(probePaths) {
		return append([]string(nil), probePaths...)
	}

	return append([]string(nil), probePaths[:maxInt(probeBudget, 1)]...)
}

// sanitizeQuery URL-encodes the characters GitHub search forbids.
func sanitizeQuery(q string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9\-_. ]+`)

	q = re.ReplaceAllString(q, " ")
	q = strings.Join(strings.Fields(q), "+")

	return q
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}

	return b
}
