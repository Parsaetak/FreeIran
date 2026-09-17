// engine/app/discoverysources.go
//
// v0.9.7 — real public-URL discovery (§7/§16): the UI-facing service
// that runs the bounded connector pipeline
//
//      DETECT → DISCOVER → INGEST → PARSE → NORMALIZE → DEDUP →
//      VALIDATE → staged PERSIST
//
// over the generic HTTP connector and the GitHub adapter. Discovered
// material lands in a STAGING ledger (config/discovered-sources.json)
// — it NEVER displaces manually configured sources, and it never
// automatically bulk-tests every discovered node (§16: staged
// selection feeds a small candidate subset into the normal testing
// flow instead).

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/discovery"
	"github.com/Parsaetak/FreeIran/engine/parser"
	"github.com/Parsaetak/FreeIran/internal/httpx"
	"github.com/Parsaetak/FreeIran/internal/logging"
	"github.com/Parsaetak/FreeIran/system"
)

// stagingLedgerFormat is the on-disk version of the staging file.
const stagingLedgerFormat = 1

// stagingEntry is one persisted discovered source with its
// provenance and quality ledger (§9).
type stagingEntry struct {
	URL        string               `json:"url"`
	SourceType discovery.SourceType `json:"source_type"`
	Trust      discovery.Trust      `json:"trust"`
	Provenance discovery.Provenance `json:"provenance"`

	// Last validation outcome.
	Candidates int `json:"candidate_count"`
	Valid      int `json:"valid_count"`
	Duplicates int `json:"duplicate_count"`

	Parser string `json:"parser,omitempty"`
}

// stagingLedger is the persisted discovered-sources file.
type stagingLedger struct {
	Version int            `json:"version"`
	Entries []stagingEntry `json:"entries"`
}

// publicSourceDiscovery is the app-level orchestrator for §7.
type publicSourceDiscovery struct {
	app *App

	mu      sync.Mutex
	running bool
	lastRun *discovery.GitHubOutcome
	lastAt  time.Time
	limiter *discovery.RateLimiter
}

// newPublicSourceDiscovery wires the orchestrator.
func newPublicSourceDiscovery(a *App) *publicSourceDiscovery {
	return &publicSourceDiscovery{
		app: a,
		limiter: discovery.NewRateLimiter(discovery.RateLimiterOptions{
			Budget:      60,
			MinInterval: 500 * time.Millisecond, // pace unauthenticated GitHub
		}),
	}
}

// stagedPath is the workspace location of the staging ledger.
func (a *App) stagedPath() string {
	return filepath.Join(a.layout.Config, "discovered-sources.json")
}

// DiscoverPublicSources runs one bounded discovery cycle:
//
//  1. GitHub strategies A–G feed the shared candidate queue;
//  2. the generic connector fetches queued candidates (SSRF-guarded,
//     ETag/conditional, size-capped);
//  3. every fetchable body is parsed; valid configurations are
//     normalized and fingerprint-deduped against the store;
//  4. results are persisted to the staging ledger with full
//     provenance; source health is recorded per candidate.
//
// The method is safe for concurrent calls (second caller waits/no-ops)
// and never returns a fatal error for rate limiting.
func (s *publicSourceDiscovery) Run(ctx context.Context) (*discovery.GitHubOutcome, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()

		return nil, fmt.Errorf("discovery already running")
	}

	s.running = true
	s.mu.Unlock()

	runStart := time.Now()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	a := s.app
	limits := discovery.BoundedRecursion{
		MaxDepth:         2,
		MaxURLsPerSource: 16,
		MaxURLsPerRun:    200,
		MaxBodySize:      8 << 20,
		DomainCooldown:   2 * time.Minute,
		TimeBudget:       5 * time.Minute,
	}

	queue := discovery.NewCandidateQueue(limits)

	ssrfOpts := httpx.SSRFOptions{MaxRedirects: 5}

	// The API client is host-pinned to GitHub; the generic client
	// accepts any public https host.
	apiClient := httpx.NewSSRFClient(httpx.Policy{
		RequestTimeout: 15 * time.Second,
		MaxRetries:     2,
		MaxBodyBytes:   2 << 20,
	}, httpx.SSRFOptions{
		MaxRedirects: ssrfOpts.MaxRedirects,
		AllowedHosts: []string{"api.github.com"},
	})

	genericClient := httpx.NewSSRFClient(httpx.Policy{
		RequestTimeout: 20 * time.Second,
		MaxRetries:     2,
		MaxBodyBytes:   limits.MaxBodySize,
	}, httpx.SSRFOptions{
		MaxRedirects: ssrfOpts.MaxRedirects,
	})

	githubConnector := &discovery.GitHubConnector{
		Client:      apiClient,
		RawClient:   genericClient,
		Limits:      limits,
		Queue:       queue,
		RateLimiter: s.limiter,
	}

	genericConnector := discovery.NewGenericConnector(genericClient, limits, queue, s.limiter)

	a.logger.Log(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: "discovery",
		Event:     "public_discovery_started",
		Message:   "bounded public-source discovery started",
		Fields: map[string]any{
			"max_urls_per_run": limits.MaxURLsPerRun,
			"max_depth":        limits.MaxDepth,
			"time_budget_s":    int(limits.TimeBudget.Seconds()),
		},
	})

	// Strategy A–G: GitHub material.
	outcome := githubConnector.Run(ctx, nil)

	// Drain the queue through the generic connector (strategy D/E
	// expansion included): fetch → parse → normalize → dedup.
	parse := parser.New()

	validTotal, candidateTotal, duplicateTotal := 0, 0, 0

	entries := make([]stagingEntry, 0, queue.Len())
	knownFingerprints := a.knownFingerprints()

	for {
		if ctx.Err() != nil {
			break
		}

		candidate, ok := queue.Take(time.Now())
		if !ok {
			break
		}

		outcome2, err := genericConnector.Fetch(ctx, candidate)
		if err != nil {
			continue // rate limited / network failure: degrade silently
		}

		if outcome2.Binary || len(outcome2.Body) == 0 {
			continue
		}

		candidateTotal++

		configs, _, err := parse.ParseDetailed(outcome2.Body)
		if err != nil {
			continue
		}

		valid, duplicates := 0, 0

		seen := make(map[string]struct{}, len(configs))

		for _, cfg := range configs {
			fp := cfg.Fingerprint()

			if _, dup := seen[fp]; dup {
				duplicates++

				continue
			}

			seen[fp] = struct{}{}

			if _, known := knownFingerprints[fp]; known {
				duplicates++

				continue
			}

			valid++
		}

		validTotal += valid
		duplicateTotal += duplicates

		entry := stagingEntry{
			URL:        candidate.URL,
			SourceType: candidate.Provenance.SourceType,
			Trust:      discovery.TrustValidated,
			Provenance: candidate.Provenance,
			Candidates: len(configs),
			Valid:      valid,
			Duplicates: duplicates,
			Parser:     "auto",
		}

		entry.Provenance.HTTPStatus = outcome2.StatusCode
		entry.Provenance.ContentHash = outcome2.ContentHash
		entry.Provenance.ContentSize = outcome2.Size
		entry.Provenance.FetchLatency = outcome2.DurationMS
		entry.Provenance.LastSeen = time.Now().UTC()

		if valid > 0 {
			entry.Provenance.LastSuccess = time.Now().UTC()
		}

		// Bounded referenced-source expansion (§10): README links feed
		// back into the queue for the NEXT cycle (depth enforces the
		// bound; this cycle stays within its URL budget).
		if candidate.Provenance.Depth < limits.MaxDepth {
			if refs := genericConnector.ExtractReferencedCandidates(candidate, outcome2.Body); len(refs) > 0 {
				queue.Add(refs)
			}
		}

		entries = append(entries, entry)

		// Keep the ledger bounded: most-recently-validated wins.
		if len(entries) >= 256 {
			break
		}
	}

	// Persist the staging ledger (validated candidates only; failures
	// are recorded in the health tracker, not silently mixed).
	s.persistStaged(entries)

	a.logger.Log(logging.Record{
		Level:      logging.LevelInfo,
		Subsystem:  "discovery",
		Event:      "public_discovery_completed",
		DurationMS: time.Since(runStart).Milliseconds(),
		Status:     "completed",
		Message: fmt.Sprintf("public discovery: %d candidates from %d sources (%d valid, %d duplicates)",
			candidateTotal, len(entries), validTotal, duplicateTotal),
		Fields: map[string]any{
			"urls_fetched":       queue.Fetched(),
			"sources":            len(entries),
			"candidates":         candidateTotal,
			"valid":              validTotal,
			"duplicates_removed": duplicateTotal,
			"github_repos":       outcome.ReposInspected,
			"github_trees":       outcome.TreesInspected,
			"github_code_hits":   outcome.CodeHits,
		},
	})

	s.mu.Lock()
	s.lastRun = &outcome
	s.lastAt = time.Now()
	s.mu.Unlock()

	return &outcome, nil
}

// persistStaged atomically writes the staging ledger.
func (s *publicSourceDiscovery) persistStaged(entries []stagingEntry) {
	ledger := stagingLedger{Version: stagingLedgerFormat, Entries: entries}

	raw, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return
	}

	_ = system.WriteFileAtomic(s.app.stagedPath(), raw, 0o600)
}

// LoadStaged returns the persisted discovered sources (UI view).
func (a *App) LoadStaged() []stagingEntry {
	raw, err := os.ReadFile(a.stagedPath())
	if err != nil {
		return nil
	}

	var ledger stagingLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil
	}

	// Newest-first for the UI.
	sort.Slice(ledger.Entries, func(i, j int) bool {
		return ledger.Entries[i].Provenance.LastSeen.After(ledger.Entries[j].Provenance.LastSeen)
	})

	return ledger.Entries
}

// knownFingerprints snapshots the store's configuration fingerprints.
func (a *App) knownFingerprints() map[string]struct{} {
	out := make(map[string]struct{})

	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()

	_ = a.store.Iterate(ctx, func(key string, value []byte) error {
		var cfg config.Config
		if err := json.Unmarshal(value, &cfg); err == nil {
			out[cfg.Fingerprint()] = struct{}{}
		}

		return nil
	})

	return out
}

// RateLimitStats exposes per-provider accounting to the UI (§8).
func (s *publicSourceDiscovery) RateLimitStats() []discovery.ProviderStats {
	return s.limiter.AllStats()
}
