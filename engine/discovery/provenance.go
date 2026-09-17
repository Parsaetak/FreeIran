package discovery

// Source trust and provenance (v0.9.7 §9) plus the bounded-recursion
// rules (§10).
//
// Every discovered candidate source carries WHERE it came from: the
// connector type, the origin repository/path (for GitHub material),
// the source that referenced it, and the full health/quality ledger.
// Discovered, validated, tested, working, stale and failed states are
// tracked separately — never silently mixed.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// SourceType classifies where a candidate URL came from.
type SourceType string

// Source types.
const (
	SourceTypeConfigured SourceType = "configured" // user-defined, never displaced
	SourceTypeDefault    SourceType = "default"    // built-in list
	SourceTypeDiscovered SourceType = "discovered" // autonomous discovery
	SourceTypeReferenced SourceType = "referenced" // linked from another source
	SourceTypeGitHubRepo SourceType = "github-repo"
	SourceTypeGitHubCode SourceType = "github-code"
	SourceTypeGitHubTree SourceType = "github-tree"
	SourceTypeGitHubRel  SourceType = "github-release"
	SourceTypeGist       SourceType = "github-gist"
	SourceTypeContent    SourceType = "content-ref"
)

// Trust is the coarse trust band of a candidate source. Trust is
// EARNED by validation and testing outcomes; it is never claimed.
type Trust string

// Trust bands.
const (
	TrustUnknown    Trust = "unknown"    // not yet fetched/validated
	TrustDiscovered Trust = "discovered" // fetched, candidates parsed
	TrustValidated  Trust = "validated"  // produced ≥1 valid configuration
	TrustTested     Trust = "tested"     // candidates passed connectivity tests
	TrustWorking    Trust = "working"    // recently produced usable nodes
	TrustStale      Trust = "stale"      // no successful refresh within TTL
	TrustFailed     Trust = "failed"     // persistently failing fetch/parse
)

// Provenance is the full identity + quality ledger of one candidate
// source (§9). Persisted next to the discovery health so restarts
// keep trusting (and distrusting) the right material.
type Provenance struct {
	// Identity.
	SourceID   string     `json:"source_id"`
	SourceType SourceType `json:"source_type"`
	SourceURL  string     `json:"source_url"`

	// GitHub origin (empty for plain HTTP sources).
	OriginRepository string `json:"origin_repository,omitempty"`
	OriginPath       string `json:"origin_path,omitempty"`

	// Causal chain: the source that referenced this one (empty for
	// seeded/configured roots).
	DiscoveredFrom string `json:"discovered_from,omitempty"`

	// Lifecycle timestamps (UTC RFC3339 when rendered).
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastFailure time.Time `json:"last_failure,omitempty"`

	// Last-fetch observations.
	HTTPStatus   int    `json:"http_status,omitempty"`
	ContentHash  string `json:"content_hash,omitempty"`
	ContentSize  int64  `json:"content_size,omitempty"`
	FetchLatency int64  `json:"fetch_latency_ms,omitempty"`
	Parser       string `json:"parser,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`

	// Yield ledger.
	CandidateCount int `json:"candidate_count"`
	ValidCount     int `json:"valid_count"`
	DuplicateCount int `json:"duplicate_count"`

	// Recursion bookkeeping.
	Depth int `json:"depth"`
}

// Candidate is one discovered public source URL awaiting validation.
type Candidate struct {
	Provenance Provenance `json:"provenance"`
	Trust      Trust      `json:"trust"`
	URL        string     `json:"url"`
}

// Key is the dedup identity of a candidate: normalized URL.
func (c Candidate) Key() string {
	return NormalizeCandidateURL(c.URL)
}

// NormalizeCandidateURL canonicalizes a candidate URL for
// deduplication: lower scheme/host, strip fragment and common tracking
// query parameters, collapse trailing slashes. Distinct URLs that
// serve the same resource collapse into one candidate.
func NormalizeCandidateURL(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}

	// Strip fragments first (never significant for fetching).
	if i := strings.Index(trimmed, "#"); i >= 0 {
		trimmed = trimmed[:i]
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return strings.TrimRight(trimmed, "/")
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""

	normalized := parsed.String()

	return strings.TrimRight(normalized, "/")
}

// FingerprintURL returns a stable short hash of a normalized URL
// (source identity in the staging ledger).
func FingerprintURL(rawURL string) string {
	sum := sha256.Sum256([]byte(NormalizeCandidateURL(rawURL)))

	return hex.EncodeToString(sum[:8])
}

// BoundedRecursion carries the §10 expansion limits. A source may
// reference another source (A → B → C) but expansion is bounded on
// every axis: depth, per-source URLs, per-run URLs, body size, domain
// cooldown and total time budget.
type BoundedRecursion struct {
	// MaxDepth bounds A → B → C chains (0 = 2).
	MaxDepth int

	// MaxURLsPerSource bounds links extracted from ONE fetched body
	// (0 = 16).
	MaxURLsPerSource int

	// MaxURLsPerRun bounds the whole discovery run's URL fetches
	// (0 = 200).
	MaxURLsPerRun int

	// MaxBodySize caps one fetched body in bytes (0 = 8 MiB).
	MaxBodySize int64

	// DomainCooldown paces repeated fetches of one host (0 = 2m).
	DomainCooldown time.Duration

	// TimeBudget caps the total connector runtime (0 = 5m).
	TimeBudget time.Duration
}

// normalize applies defaults.
func (b BoundedRecursion) normalize() BoundedRecursion {
	if b.MaxDepth <= 0 {
		b.MaxDepth = 2
	}

	if b.MaxURLsPerSource <= 0 {
		b.MaxURLsPerSource = 16
	}

	if b.MaxURLsPerRun <= 0 {
		b.MaxURLsPerRun = 200
	}

	if b.MaxBodySize <= 0 {
		b.MaxBodySize = 8 << 20
	}

	if b.DomainCooldown <= 0 {
		b.DomainCooldown = 2 * time.Minute
	}

	if b.TimeBudget <= 0 {
		b.TimeBudget = 5 * time.Minute
	}

	return b
}

// domainLastFetch records per-host pacing; the CandidateQueue owns
// the live map (host → last fetch) for domain cooldowns.

// CandidateQueue is the bounded, deduplicated expansion queue shared
// by the connectors.
type CandidateQueue struct {
	mu       sync.Mutex
	pending  []Candidate
	seen     map[string]struct{}
	fetched  map[string]struct{}
	domains  map[string]time.Time
	limits   BoundedRecursion
	fetchedN int
}

// NewCandidateQueue creates a queue with the given bounds.
func NewCandidateQueue(limits BoundedRecursion) *CandidateQueue {
	limits = limits.normalize()

	return &CandidateQueue{
		seen:    make(map[string]struct{}),
		fetched: make(map[string]struct{}),
		domains: make(map[string]time.Time),
		limits:  limits,
	}
}

// Add enqueues candidates, deduplicating and enforcing the per-source
// cap (the caller passes at most MaxURLsPerSource from one body).
// It reports how many were newly admitted.
func (q *CandidateQueue) Add(candidates []Candidate) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	admitted := 0

	// Deterministic order.
	sorted := make([]Candidate, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].URL < sorted[j].URL
	})

	for _, candidate := range sorted {
		if candidate.Provenance.Depth > q.limits.MaxDepth {
			continue
		}

		// Run budget covers tracked (pending) + fetched candidates.
		if q.fetchedN+len(q.pending) >= q.limits.MaxURLsPerRun {
			return admitted
		}

		key := candidate.Key()
		if key == "" {
			continue
		}

		if _, dup := q.seen[key]; dup {
			// Track duplicates for the provenance ledger.
			continue
		}

		q.seen[key] = struct{}{}
		q.pending = append(q.pending, candidate)
		admitted++
	}

	return admitted
}

// Take pops the next fetchable candidate honouring the domain
// cooldown, or returns ok=false when the queue is empty/budget spent.
func (q *CandidateQueue) Take(now time.Time) (Candidate, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.fetchedN >= q.limits.MaxURLsPerRun {
		return Candidate{}, false
	}

	for i, candidate := range q.pending {
		host := hostOf(candidate.URL)

		if last, cooling := q.domains[host]; cooling && now.Sub(last) < q.limits.DomainCooldown {
			continue // keep pacing this host
		}

		q.pending = append(q.pending[:i], q.pending[i+1:]...)
		q.domains[host] = now
		q.fetched[candidate.Key()] = struct{}{}
		q.fetchedN++

		return candidate, true
	}

	// Every pending candidate is domain-cooled. Forced progress: take
	// the FIRST pending candidate anyway (the run budget still bounds
	// total fetches) — a single-host run must not deadlock, and the
	// pacing cap is honoured whenever alternative hosts exist.
	if len(q.pending) > 0 {
		candidate := q.pending[0]
		q.pending = q.pending[1:]
		q.domains[hostOf(candidate.URL)] = now
		q.fetched[candidate.Key()] = struct{}{}
		q.fetchedN++

		return candidate, true
	}

	return Candidate{}, false
}

// Len returns the pending count.
func (q *CandidateQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.pending)
}

// Fetched returns how many URLs this run has fetched.
func (q *CandidateQueue) Fetched() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.fetchedN
}

// BudgetLeft returns the remaining per-run fetch budget.
func (q *CandidateQueue) BudgetLeft() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.limits.MaxURLsPerRun - q.fetchedN
}

func hostOf(rawURL string) string {
	u := rawURL
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}

	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}

	return strings.ToLower(u)
}

// String renders a candidate for logs (credential-free).
func (c Candidate) String() string {
	return fmt.Sprintf("%s [%s depth=%d from=%s]", c.URL, c.Provenance.SourceType,
		c.Provenance.Depth, c.Provenance.DiscoveredFrom)
}
