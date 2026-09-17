package discovery

// GitHub discovery adapter (v0.9.7 §7, connectors 2–3) and the
// GitHub rate-limit engineering (§8).
//
// Uses the public GitHub REST API and raw resources through multiple
// BOUNDED strategies:
//
//   A. Repository search  — public repos matching configuration terms;
//   B. Code search        — protocol URI patterns (vless://, vmess://…)
//                           via /search/code (stricter limits: budgeted
//                           separately, skipped while cooling down);
//   C. Tree inspection    — /git/trees/<branch>?recursive=1 to find
//                           candidate files (.txt/.yaml/.yml/.json/
//                           .conf/.list/.sub) in promising repos;
//   D. Raw fetching       — raw.githubusercontent.com URLs (never
//                           HTML pages);
//   E. README references  — fetched READMEs feed absolute https URLs
//                           back into the bounded discovery queue;
//   F. Releases/assets    — /releases assets that carry plausible
//                           configuration file names;
//   G. Public gists       — gist files via the gist API (same budget).
//
// Unauthenticated public access is assumed: all requests are
// accounted by the shared RateLimiter (per provider), and EVERY
// failure degrades to cached/local sources instead of retrying
// aggressively. Rate limiting is never a fatal application error.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// GitHubConnector implements the GitHub strategies over the shared
// guard stack.
type GitHubConnector struct {
	// Client is the SSRF-guarded client for api.github.com.
	Client *httpx.Client

	// RawClient fetches raw.githubusercontent.com resources.
	RawClient *httpx.Client

	// Limits bound the expansion.
	Limits BoundedRecursion

	// Queue is the shared candidate queue for this run.
	Queue *CandidateQueue

	// RateLimiter accounts per-provider budgets.
	RateLimiter *RateLimiter

	// Config tunes strategy budgets.
	Config GitHubSearchConfig

	// APIBase overrides the API base (tests point it at a local
	// mock; production leaves it empty for https://api.github.com).
	APIBase string
}

// GitHubSearchConfig bounds each strategy.
type GitHubSearchConfig struct {
	MaxQueries    int           // A+B: total search queries per run (0 = 3)
	MaxRepos      int           // A: repositories inspected per run (0 = 12)
	MaxTrees      int           // C: tree inspections per run (0 = 6)
	MaxProbes     int           // D: raw file fetches per run (0 = 24)
	MaxReleases   int           // F: release listings per run (0 = 4)
	MaxGists      int           // G: gist fetches per run (0 = 4)
	RequestTimeot time.Duration // per-request budget (0 = 15s)
}

// normalize applies defaults.
func (c GitHubSearchConfig) normalize() GitHubSearchConfig {
	if c.MaxQueries <= 0 {
		c.MaxQueries = 3
	}

	if c.MaxRepos <= 0 {
		c.MaxRepos = 12
	}

	if c.MaxTrees <= 0 {
		c.MaxTrees = 6
	}

	if c.MaxProbes <= 0 {
		c.MaxProbes = 24
	}

	if c.MaxReleases <= 0 {
		c.MaxReleases = 4
	}

	if c.MaxGists <= 0 {
		c.MaxGists = 4
	}

	if c.RequestTimeot <= 0 {
		c.RequestTimeot = 15 * time.Second
	}

	return c
}

// githubRepo is the subset of the repo-search payload we consume.
type githubRepo struct {
	FullName    string `json:"full_name"`
	DefaultBrch string `json:"default_branch"`
	PushedAt    string `json:"pushed_at"`
	Description string `json:"description"`
	Stars       int    `json:"stargazers_count"`
}

// githubTreeEntry is one tree row.
type githubTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int    `json:"size"`
	SHA  string `json:"sha"`
}

// githubRelease is the subset of release payloads we consume.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
	Body    string        `json:"body"`
}

type githubAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// githubGist is the subset of gist payloads we consume.
type githubGist struct {
	ID    string `json:"id"`
	Files map[string]struct {
		RawURL string `json:"raw_url"`
	} `json:"files"`
}

// candidateFileExtensions select tree paths worth fetching.
var candidateFileExtensions = []string{
	".txt", ".yaml", ".yml", ".json", ".conf", ".list", ".sub",
}

// candidatePathKeywords select tree paths whose names suggest
// configuration material.
var candidatePathKeywords = []string{
	"sub", "subscription", "subscriptions", "config", "configs",
	"node", "nodes", "proxy", "proxies", "clash", "singbox", "sing-box",
	"v2ray", "xray", "trojan", "hysteria", "merged", "mix", "all",
}

// GitHubOutcome is the per-run result summary of the GitHub adapter.
type GitHubOutcome struct {
	ReposInspected int             `json:"repos_inspected"`
	TreesInspected int             `json:"trees_inspected"`
	ProbesFetched  int             `json:"probes_fetched"`
	Candidates     int             `json:"candidates"`
	CodeHits       int             `json:"code_hits"`
	StrategyNotes  []string        `json:"strategy_notes,omitempty"`
	ProviderStats  []ProviderStats `json:"provider_stats,omitempty"`
}

// Run executes strategies A–G within the given context and budget.
// Every strategy degrades independently: a rate-limited search does
// not stop tree inspection of already-known repositories.
func (g *GitHubConnector) Run(ctx context.Context, seededRepos []string) GitHubOutcome {
	cfg := g.Config.normalize()
	outcome := GitHubOutcome{}

	deadline := time.Now().Add(g.Limits.normalize().TimeBudget)

	// --- A. Repository search -------------------------------------
	repos := make([]githubRepo, 0, cfg.MaxRepos)

	queries := DefaultGitHubQueries
	if len(queries) > cfg.MaxQueries {
		queries = queries[:cfg.MaxQueries]
	}

	for _, query := range queries {
		if ctx.Err() != nil || time.Now().After(deadline) {
			outcome.StrategyNotes = append(outcome.StrategyNotes, "A: budget exhausted")
			break
		}

		found, err := SearchRepositories(ctx, g, query)
		if err != nil {
			outcome.StrategyNotes = append(outcome.StrategyNotes,
				"A: query "+query+" failed: "+err.Error())

			// Rate-limited: degrade instead of hammering.
			if errorsIs(err, ErrProviderCoolingDown) || errorsIs(err, ErrBudgetExhausted) {
				break
			}

			continue
		}

		repos = append(repos, found...)

		if len(repos) >= cfg.MaxRepos {
			repos = repos[:cfg.MaxRepos]

			break
		}
	}

	outcome.ReposInspected = len(repos)

	// --- B. Code search (only while budget remains) ----------------
	if ctx.Err() == nil && !time.Now().After(deadline) {
		codeQueries := DefaultCodeQueries
		if len(codeQueries) > cfg.MaxQueries {
			codeQueries = codeQueries[:cfg.MaxQueries]
		}

		for _, query := range codeQueries {
			hits, err := SearchCode(ctx, g, query)
			if err != nil {
				outcome.StrategyNotes = append(outcome.StrategyNotes,
					"B: query "+query+" failed: "+err.Error())

				if errorsIs(err, ErrProviderCoolingDown) || errorsIs(err, ErrBudgetExhausted) {
					break
				}

				continue
			}

			outcome.CodeHits += len(hits)

			for _, hit := range hits {
				repos = append(repos, githubRepo{
					FullName:    hit.Repository,
					DefaultBrch: hit.Branch,
				})
			}
		}
	}

	// Dedup repositories.
	seenRepo := make(map[string]struct{}, len(repos))

	unique := repos[:0]

	for _, repo := range repos {
		if repo.FullName == "" || repo.DefaultBrch == "" {
			continue
		}

		if _, dup := seenRepo[repo.FullName]; dup {
			continue
		}

		seenRepo[repo.FullName] = struct{}{}
		unique = append(unique, repo)
	}

	repos = unique

	// --- C. Tree inspection ----------------------------------------
	treeBudget := cfg.MaxTrees

	for _, repo := range repos {
		if treeBudget <= 0 || ctx.Err() != nil || time.Now().After(deadline) {
			break
		}

		raws, err := InspectTree(ctx, g, repo.FullName, repo.DefaultBrch)
		if err != nil {
			outcome.StrategyNotes = append(outcome.StrategyNotes,
				"C: tree "+repo.FullName+" failed: "+err.Error())

			continue
		}

		treeBudget--
		outcome.TreesInspected++

		now := time.Now().UTC()

		candidates := make([]Candidate, 0, len(raws))

		for _, raw := range raws {
			candidates = append(candidates, Candidate{
				URL:   raw,
				Trust: TrustUnknown,
				Provenance: Provenance{
					SourceID:         "github:" + FingerprintURL(raw),
					SourceType:       SourceTypeGitHubTree,
					SourceURL:        raw,
					OriginRepository: repo.FullName,
					OriginPath:       raw,
					FirstSeen:        now,
					LastSeen:         now,
					Depth:            1,
				},
			})
		}

		outcome.Candidates += g.Queue.Add(candidates)
	}

	// --- F. Release assets ------------------------------------------
	for _, repo := range repos {
		if cfg.MaxReleases <= 0 || ctx.Err() != nil || time.Now().After(deadline) {
			break
		}

		urls, err := ReleaseAssets(ctx, g, repo.FullName)
		if err != nil {
			continue
		}

		cfg.MaxReleases--

		now := time.Now().UTC()

		candidates := make([]Candidate, 0, len(urls))

		for _, assetURL := range urls {
			candidates = append(candidates, Candidate{
				URL:   assetURL,
				Trust: TrustUnknown,
				Provenance: Provenance{
					SourceID:         "github-rel:" + FingerprintURL(assetURL),
					SourceType:       SourceTypeGitHubRel,
					SourceURL:        assetURL,
					OriginRepository: repo.FullName,
					FirstSeen:        now,
					LastSeen:         now,
					Depth:            1,
				},
			})
		}

		outcome.Candidates += g.Queue.Add(candidates)
	}

	// --- G. Gists -----------------------------------------------------
	// Gist discovery runs only against explicitly seeded gist IDs
	// (public gist search is not offered unauthenticated).
	for _, gistID := range seededRepos {
		if !strings.Contains(gistID, "gist:") {
			continue
		}

		if cfg.MaxGists <= 0 || ctx.Err() != nil {
			break
		}

		id := strings.TrimPrefix(gistID, "gist:")

		urls, err := GistRawURLs(ctx, g, id)
		if err != nil {
			continue
		}

		cfg.MaxGists--

		now := time.Now().UTC()

		candidates := make([]Candidate, 0, len(urls))

		for _, raw := range urls {
			candidates = append(candidates, Candidate{
				URL:   raw,
				Trust: TrustUnknown,
				Provenance: Provenance{
					SourceID:   "gist:" + FingerprintURL(raw),
					SourceType: SourceTypeGist,
					SourceURL:  raw,
					FirstSeen:  now,
					LastSeen:   now,
					Depth:      1,
				},
			})
		}

		outcome.Candidates += g.Queue.Add(candidates)
	}

	outcome.ProviderStats = g.RateLimiter.AllStats()

	return outcome
}

// --- Strategy primitives ---------------------------------------------

// searchHeaders carries the API Accept profile.
func searchHeaders() map[string]string {
	return map[string]string{
		"Accept": "application/vnd.github+json",
	}
}

// apiBase resolves the API base URL.
func (g *GitHubConnector) apiBase() string {
	if g.APIBase != "" {
		return strings.TrimRight(g.APIBase, "/")
	}

	return "https://api.github.com"
}

// SearchRepositories runs ONE repository-search query (strategy A).
func SearchRepositories(ctx context.Context, g *GitHubConnector, query string) ([]githubRepo, error) {
	cfg := g.Config.normalize()

	if err := g.RateLimiter.Acquire(ProviderGitHub); err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf(
		"%s/search/repositories?q=%s&sort=updated&order=desc&per_page=%d",
		g.apiBase(), url.QueryEscape(query), cfg.MaxRepos)

	requestCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeot)
	defer cancel()

	started := time.Now()

	resp, err := g.Client.Get(requestCtx, endpoint, httpx.GetOptions{Header: searchHeaders()})
	latency := time.Since(started)

	if err != nil {
		g.RateLimiter.Observe(ProviderGitHub, classifyGitHubError(err), latency, 0, -1, "")

		return nil, err
	}

	remaining, resetAt := githubRateLimitHeaders(resp.Header)

	g.RateLimiter.Observe(ProviderGitHub, resp.StatusCode, latency, 0, remaining, resetAt)

	if resp.StatusCode == http.StatusForbidden && remaining == 0 {
		return nil, ErrProviderCoolingDown
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: repository search status %d", resp.StatusCode)
	}

	var payload struct {
		Items []githubRepo `json:"items"`
	}

	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, fmt.Errorf("github: decode repository search: %w", err)
	}

	return payload.Items, nil
}

// CodeHit is one code-search match.
type CodeHit struct {
	Repository string
	Path       string
	Branch     string
}

// SearchCode runs ONE code-search query (strategy B).
func SearchCode(ctx context.Context, g *GitHubConnector, query string) ([]CodeHit, error) {
	cfg := g.Config.normalize()

	if err := g.RateLimiter.Acquire(ProviderGitHub); err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf(
		"%s/search/code?q=%s&per_page=10",
		g.apiBase(), url.QueryEscape(query))

	requestCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeot)
	defer cancel()

	started := time.Now()

	resp, err := g.Client.Get(requestCtx, endpoint, httpx.GetOptions{Header: searchHeaders()})
	latency := time.Since(started)

	if err != nil {
		g.RateLimiter.Observe(ProviderGitHub, classifyGitHubError(err), latency, 0, -1, "")

		return nil, err
	}

	remaining, resetAt := githubRateLimitHeaders(resp.Header)

	g.RateLimiter.Observe(ProviderGitHub, resp.StatusCode, latency, 0, remaining, resetAt)

	if resp.StatusCode == http.StatusForbidden && remaining == 0 {
		return nil, ErrProviderCoolingDown
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: code search status %d", resp.StatusCode)
	}

	var payload struct {
		Items []struct {
			Path       string `json:"path"`
			Repository struct {
				FullName    string `json:"full_name"`
				DefaultBrch string `json:"default_branch"`
			} `json:"repository"`
		} `json:"items"`
	}

	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, fmt.Errorf("github: decode code search: %w", err)
	}

	hits := make([]CodeHit, 0, len(payload.Items))

	for _, item := range payload.Items {
		hits = append(hits, CodeHit{
			Repository: item.Repository.FullName,
			Path:       item.Path,
			Branch:     item.Repository.DefaultBrch,
		})
	}

	return hits, nil
}

// InspectTree fetches the recursive tree of a repository branch and
// returns raw URLs for plausible configuration files (strategy C).
func InspectTree(ctx context.Context, g *GitHubConnector, repo, branch string) ([]string, error) {
	cfg := g.Config.normalize()

	if err := g.RateLimiter.Acquire(ProviderGitHub); err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", g.apiBase(), repo, branch)

	requestCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeot)
	defer cancel()

	started := time.Now()

	resp, err := g.Client.Get(requestCtx, endpoint, httpx.GetOptions{Header: searchHeaders()})
	latency := time.Since(started)

	if err != nil {
		g.RateLimiter.Observe(ProviderGitHub, classifyGitHubError(err), latency, 0, -1, "")

		return nil, err
	}

	remaining, resetAt := githubRateLimitHeaders(resp.Header)

	g.RateLimiter.Observe(ProviderGitHub, resp.StatusCode, latency, 0, remaining, resetAt)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: tree inspection status %d", resp.StatusCode)
	}

	var payload struct {
		Truncated bool              `json:"truncated"`
		Tree      []githubTreeEntry `json:"tree"`
	}

	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, fmt.Errorf("github: decode tree: %w", err)
	}

	paths := selectCandidatePaths(payload.Tree)

	raws := make([]string, 0, len(paths))

	for _, path := range paths {
		raws = append(raws, RawURLFor(repo, branch, path))
	}

	return raws, nil
}

// selectCandidatePaths filters tree entries to plausible config files,
// ranked by keyword score.
func selectCandidatePaths(entries []githubTreeEntry) []string {
	scores := make(map[string]int, len(entries))

	for _, entry := range entries {
		if entry.Type != "blob" {
			continue
		}

		lowered := strings.ToLower(entry.Path)

		extOK := false

		for _, ext := range candidateFileExtensions {
			if strings.HasSuffix(lowered, ext) {
				extOK = true

				break
			}
		}

		if !extOK {
			continue
		}

		if strings.HasPrefix(lowered, "readme") {
			continue
		}

		score := 0

		for _, keyword := range candidatePathKeywords {
			if strings.Contains(lowered, keyword) {
				score++
			}
		}

		if score == 0 {
			continue
		}

		scores[entry.Path] = score
	}

	paths := make([]string, 0, len(scores))

	for path := range scores {
		paths = append(paths, path)
	}

	// Deeper keyword matches first, then shorter paths.
	sort.Slice(paths, func(i, j int) bool {
		si, sj := scores[paths[i]], scores[paths[j]]

		if si != sj {
			return si > sj
		}

		return len(paths[i]) < len(paths[j])
	})

	if len(paths) > 8 {
		paths = paths[:8]
	}

	return paths
}

// ReleaseAssets lists plausible configuration assets from repository
// releases (strategy F).
func ReleaseAssets(ctx context.Context, g *GitHubConnector, repo string) ([]string, error) {
	cfg := g.Config.normalize()

	if err := g.RateLimiter.Acquire(ProviderGitHub); err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", g.apiBase(), repo, cfg.MaxReleases)

	requestCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeot)
	defer cancel()

	started := time.Now()

	resp, err := g.Client.Get(requestCtx, endpoint, httpx.GetOptions{Header: searchHeaders()})
	latency := time.Since(started)

	if err != nil {
		g.RateLimiter.Observe(ProviderGitHub, classifyGitHubError(err), latency, 0, -1, "")

		return nil, err
	}

	remaining, resetAt := githubRateLimitHeaders(resp.Header)

	g.RateLimiter.Observe(ProviderGitHub, resp.StatusCode, latency, 0, remaining, resetAt)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: releases status %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.Unmarshal(resp.Body, &releases); err != nil {
		return nil, fmt.Errorf("github: decode releases: %w", err)
	}

	var urls []string

	for _, release := range releases {
		for _, asset := range release.Assets {
			lowered := strings.ToLower(asset.Name)

			for _, ext := range candidateFileExtensions {
				if strings.HasSuffix(lowered, ext) {
					urls = append(urls, asset.URL)

					break
				}
			}
		}
	}

	return urls, nil
}

// GistRawURLs fetches one public gist's file list and returns raw
// URLs of plausible configuration files (strategy G).
func GistRawURLs(ctx context.Context, g *GitHubConnector, gistID string) ([]string, error) {
	cfg := g.Config.normalize()

	if err := g.RateLimiter.Acquire(ProviderGist); err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/gists/%s", g.apiBase(), gistID)

	requestCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeot)
	defer cancel()

	started := time.Now()

	resp, err := g.Client.Get(requestCtx, endpoint, httpx.GetOptions{Header: searchHeaders()})
	latency := time.Since(started)

	if err != nil {
		g.RateLimiter.Observe(ProviderGist, classifyGitHubError(err), latency, 0, -1, "")

		return nil, err
	}

	remaining, resetAt := githubRateLimitHeaders(resp.Header)

	g.RateLimiter.Observe(ProviderGist, resp.StatusCode, latency, 0, remaining, resetAt)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: gist status %d", resp.StatusCode)
	}

	var gist githubGist
	if err := json.Unmarshal(resp.Body, &gist); err != nil {
		return nil, fmt.Errorf("github: decode gist: %w", err)
	}

	var urls []string

	for name, file := range gist.Files {
		lowered := strings.ToLower(name)

		for _, ext := range candidateFileExtensions {
			if strings.HasSuffix(lowered, ext) && file.RawURL != "" {
				urls = append(urls, file.RawURL)

				break
			}
		}
	}

	return urls, nil
}

// githubRateLimitHeaders extracts X-RateLimit-Remaining / Reset.
func githubRateLimitHeaders(header http.Header) (int64, string) {
	remaining := int64(-1)

	if raw := header.Get("X-RateLimit-Remaining"); raw != "" {
		var parsed int64
		if _, err := fmt.Sscanf(raw, "%d", &parsed); err == nil {
			remaining = parsed
		}
	}

	reset := ""

	if raw := header.Get("X-RateLimit-Reset"); raw != "" {
		var epoch int64
		if _, err := fmt.Sscanf(raw, "%d", &epoch); err == nil && epoch > 0 {
			reset = time.Unix(epoch, 0).UTC().Format(time.RFC3339)
		}
	}

	return remaining, reset
}

// classifyGitHubError maps a fetch error into the provider ledger's
// status code space (-1 transport, otherwise the HTTP status).
func classifyGitHubError(err error) int {
	if err == nil {
		return 0
	}

	if code := httpx.StatusCodeOf(err); code > 0 {
		return code
	}

	return -1
}

// errorsIs is errors.Is without the extra import noise in hot paths.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}

		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}

		err = unwrapper.Unwrap()
	}

	return false
}

// DefaultGitHubQueries lists the bounded repository-search queries
// (strategy A). Terms target configuration aggregation projects and
// Iran-focused collections.
var DefaultGitHubQueries = []string{
	"vless vmess subscription",
	"free proxy nodes iran",
	"v2ray configs list",
	"clash sing-box subscription",
}

// DefaultCodeQueries lists the bounded code-search queries
// (strategy B): recognizable protocol URI patterns.
var DefaultCodeQueries = []string{
	"vless://",
	"vmess://",
	"trojan://",
	"hysteria2://",
}
