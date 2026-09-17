// Package discovery implements FreeIran's multi-level node
// discovery pipeline (v0.9.6 §5):
//
//	DISCOVER → INGEST → PARSE → NORMALIZE → DEDUPLICATE → VALIDATE
//	         → (TEST → SCORE → RANK → SELECT are the downstream
//	            engine/tester/ranking/connection stages)
//
// Discovery levels, cheapest and most trusted first:
//
//  1. cached known nodes        (the local store's existing pool)
//  2. configured sources        (the user's source list)
//  3. trusted public sources    (the built-in verified registry)
//  4. fresh public discovery    (smart search over GitHub)
//  5. content-derived sources   (references inside valid content)
//  6. recovery discovery        (emergency re-discovery on failure)
//  7. deep discovery            (restrictive-network escalation)
//
// The engine is bounded, concurrent but controlled, timeout-aware,
// rate-limit aware, cache-aware, duplicate-resistant, source-health
// aware, failure-isolated and cancellation-safe: a broken source
// never stops discovery from other sources, and every level degrades
// independently.
package discovery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/parser"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// Level identifies one discovery level.
type Level int

const (
	// LevelCached: the store's known node pool (cheapest; no network).
	LevelCached Level = 1

	// LevelConfigured: the user's configured sources.
	LevelConfigured Level = 2

	// LevelTrustedPublic: the built-in trusted public registry.
	LevelTrustedPublic Level = 3

	// LevelSearch: fresh public-source discovery via smart search.
	LevelSearch Level = 4

	// LevelContent: sources discovered from references inside valid
	// fetched content.
	LevelContent Level = 5

	// LevelRecovery: emergency re-discovery after connection
	// failures exhausted the known pool.
	LevelRecovery Level = 6

	// LevelDeep: restrictive-network escalation — search with more
	// queries and accept lower-yield sources.
	LevelDeep Level = 7
)

// LevelName returns the stable identifier of a level.
func LevelName(l Level) string {
	switch l {
	case LevelCached:
		return "cached"
	case LevelConfigured:
		return "configured"
	case LevelTrustedPublic:
		return "trusted_public"
	case LevelSearch:
		return "search"
	case LevelContent:
		return "content"
	case LevelRecovery:
		return "recovery"
	case LevelDeep:
		return "deep"
	default:
		return fmt.Sprintf("level_%d", int(l))
	}
}

// LevelSet selects which levels a discovery run executes. Levels run
// in ascending order; later levels only run when the earlier ones did
// not already satisfy the target (unless ForceAll is set).
type LevelSet struct {
	Cached        bool
	Configured    bool
	TrustedPublic bool
	Search        bool
	Content       bool
	Recovery      bool
	Deep          bool

	// ForceAll runs every selected level even when the target is
	// already satisfied (used by manual "discover everything").
	ForceAll bool
}

// StandardLevels is the default adaptive set: cache, configured and
// trusted public sources.
func StandardLevels() LevelSet {
	return LevelSet{Cached: true, Configured: true, TrustedPublic: true}
}

// FullLevels adds smart search and content-derived discovery.
func FullLevels() LevelSet {
	return LevelSet{Cached: true, Configured: true, TrustedPublic: true, Search: true, Content: true}
}

// RecoveryLevels is the emergency set used by the connection
// recovery path: everything except deep discovery.
func RecoveryLevels() LevelSet {
	return LevelSet{Cached: true, Configured: true, TrustedPublic: true, Search: true, Content: true, Recovery: true}
}

// DeepLevels is the full escalation used when the network
// environment is classified restrictive.
func DeepLevels() LevelSet {
	return LevelSet{
		Cached: true, Configured: true, TrustedPublic: true,
		Search: true, Content: true, Recovery: true, Deep: true,
	}
}

// EngineConfig tunes the discovery engine. Zero values select safe
// defaults.
type EngineConfig struct {
	// FetchConcurrency bounds concurrent source fetches.
	FetchConcurrency int

	// Search tunes the smart searcher (see SearchConfig).
	Search SearchConfig

	// TargetCandidates is the pool size that satisfies a run before
	// later levels are skipped (default 200).
	TargetCandidates int

	// MinSourceCandidates is the parse yield for a probed candidate
	// to be promoted to a discovered source (default 3).
	MinSourceCandidates int

	// FetchTimeout bounds one source fetch.
	FetchTimeout time.Duration

	// MaxBodySize caps one fetched body.
	MaxBodySize int64
}

// DefaultEngineConfig returns conservative desktop defaults.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		FetchConcurrency:    6,
		Search:              DefaultSearchConfig(),
		TargetCandidates:    200,
		MinSourceCandidates: 3,
		FetchTimeout:        20 * time.Second,
		MaxBodySize:         10 << 20,
	}
}

func (c EngineConfig) normalize() EngineConfig {
	if c.FetchConcurrency <= 0 {
		c.FetchConcurrency = 6
	}

	if c.TargetCandidates <= 0 {
		c.TargetCandidates = 200
	}

	if c.MinSourceCandidates <= 0 {
		c.MinSourceCandidates = 3
	}

	if c.FetchTimeout <= 0 {
		c.FetchTimeout = 20 * time.Second
	}

	if c.MaxBodySize <= 0 {
		c.MaxBodySize = 10 << 20
	}

	c.Search = c.Search.normalize()

	return c
}

// Node is the canonical representation of one discovered candidate
// (v0.9.6 §7). The protocol-specific details live in the embedded
// config.Config — the ONE model the whole engine shares — while the
// discovery-specific provenance and measurements are carried here.
// Parsing stays protocol-specific (engine/parser); the node model
// stays canonical.
type Node struct {
	// Config is the normalized, validated candidate.
	config.Config

	// DiscoveredAt is when this node entered the pool this run.
	DiscoveredAt time.Time

	// Level is the discovery level that produced the node.
	Level Level

	// SourceID / SourceURL identify the origin (source.ID / URL).
	SourceID  string
	SourceURL string

	// SourceTimestamp is the source content's own update time when
	// the source declared one (0 = unknown, never invented).
	SourceTimestamp time.Time
}

// Stage names the pipeline stage for progress reporting.
type Stage string

const (
	StageDiscovering   Stage = "discovering"
	StageParsing       Stage = "parsing"
	StageNormalizing   Stage = "normalizing"
	StageValidating    Stage = "validating"
	StageDeduplicating Stage = "deduplicating"
	StageComplete      Stage = "complete"
)

// Progress is one progress event emitted during a discovery run.
// Progress is REAL: stage transitions and level completions are
// reported as they happen, never as a fake animation.
type Progress struct {
	Stage      Stage     `json:"stage"`
	Level      Level     `json:"level"`
	LevelName  string    `json:"level_name"`
	Discovered int       `json:"discovered"`
	Valid      int       `json:"valid"`
	Duplicates int       `json:"duplicates"`
	At         time.Time `json:"at"`
	Message    string    `json:"message,omitempty"`
}

// LevelStats records what one level contributed.
type LevelStats struct {
	Level      Level  `json:"level"`
	Name       string `json:"name"`
	Sources    int    `json:"sources"`
	FetchedOK  int    `json:"fetched_ok"`
	Failed     int    `json:"failed"`
	Candidates int    `json:"candidates"`
	Valid      int    `json:"valid"`
	Duplicates int    `json:"duplicates"`
	DurationMS int64  `json:"duration_ms"`
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// Stats summarizes one discovery run.
type Stats struct {
	Levels     []LevelStats `json:"levels"`
	Discovered int          `json:"discovered"`
	Valid      int          `json:"valid"`
	Duplicates int          `json:"duplicates"`
	DurationMS int64        `json:"duration_ms"`
}

// Engine executes multi-level discovery.
type Engine struct {
	cfg    EngineConfig
	getter httpx.Getter
	parser *parser.Parser
	health *Health
	search *Searcher

	// pendingContentRefs collects raw-endpoint references found
	// inside valid fetched content during the source levels; the
	// content level consumes them.
	pendingContentRefs []string

	onProgress func(Progress)
}

// NewEngine creates a discovery engine. getter nil = production
// client; healthPath "" = in-memory health.
func NewEngine(getter httpx.Getter, cfg EngineConfig, healthPath string) *Engine {
	cfg = cfg.normalize()

	if getter == nil {
		getter = httpx.Default()
	}

	return &Engine{
		cfg:    cfg,
		getter: getter,
		parser: parser.New(),
		health: NewHealth(healthPath),
		search: NewSearcher(getter, cfg.Search),
	}
}

// Health exposes the source-health tracker.
func (e *Engine) Health() *Health { return e.health }

// OnProgress installs the progress callback (cheap, non-blocking).
func (e *Engine) OnProgress(fn func(Progress)) { e.onProgress = fn }

func (e *Engine) emit(p Progress) {
	if e.onProgress != nil {
		e.onProgress(p)
	}
}

// Discover runs the selected levels and returns the validated,
// deduplicated candidate pool. cached is the known-node pool (level 1
// is satisfied from it without any network use).
func (e *Engine) Discover(
	ctx context.Context,
	levels LevelSet,
	cached []config.Config,
	extraSources []source.Source,
) ([]Node, *Stats) {
	started := time.Now()

	stats := &Stats{}

	dedup := make(map[string]struct{})
	nodes := make([]Node, 0, len(cached))

	// ---- Level 1: cached known nodes ----
	if levels.Cached {
		ls := LevelStats{Level: LevelCached, Name: LevelName(LevelCached)}

		for i := range cached {
			cached[i].Normalize()

			if err := cached[i].Validate(); err != nil {
				continue
			}

			fp := cached[i].Fingerprint()
			if fp == "" {
				continue
			}

			if _, dup := dedup[fp]; dup {
				ls.Duplicates++
				continue
			}

			dedup[fp] = struct{}{}
			nodes = append(nodes, Node{
				Config:       cached[i],
				DiscoveredAt: started,
				Level:        LevelCached,
				SourceID:     cached[i].Source,
			})
		}

		ls.Valid = len(nodes)
		ls.DurationMS = time.Since(started).Milliseconds()
		stats.Levels = append(stats.Levels, ls)

		e.emit(Progress{
			Stage: StageDiscovering, Level: LevelCached, LevelName: LevelName(LevelCached),
			Discovered: len(nodes), Valid: len(nodes), At: time.Now(),
			Message: fmt.Sprintf("%d known nodes loaded", len(nodes)),
		})
	}

	// ---- Levels 2-3: fetch configured + trusted public sources ----
	var fetchSources []source.Source

	if levels.Configured {
		fetchSources = append(fetchSources, extraSources...)
	}

	if levels.TrustedPublic {
		for _, src := range source.DefaultSources() {
			if !containsSource(fetchSources, src.ID) {
				fetchSources = append(fetchSources, src)
			}
		}
	}

	if len(fetchSources) > 0 {
		nodes = e.fetchSourcesLevel(ctx, LevelConfigured, fetchSources, nodes, dedup, stats)
	}

	targetMet := func() bool { return len(nodes) >= e.cfg.TargetCandidates }

	// ---- Level 4: smart search ----
	if levels.Search && (levels.ForceAll || !targetMet()) {
		nodes = e.searchLevel(ctx, nodes, dedup, stats, false)
	}

	// ---- Level 5: content-derived sources ----
	if levels.Content && (levels.ForceAll || !targetMet()) {
		nodes = e.contentLevel(ctx, nodes, dedup, stats)
	}

	// ---- Level 6: recovery ----
	if levels.Recovery {
		// Recovery re-runs the search level with fresh queries when
		// the pool is thin — the emergency widening path.
		if len(nodes) < e.cfg.TargetCandidates/2 {
			nodes = e.searchLevel(ctx, nodes, dedup, stats, true)
		} else {
			stats.Levels = append(stats.Levels, LevelStats{
				Level: LevelRecovery, Name: LevelName(LevelRecovery),
				Skipped: true, SkipReason: "pool sufficient",
			})
		}
	}

	// ---- Level 7: deep discovery ----
	if levels.Deep {
		// Deep discovery accepts lower-yield sources and probes more
		// paths — only sensible when the environment is restrictive.
		nodes = e.searchLevel(ctx, nodes, dedup, stats, true)
	}

	stats.Discovered = len(nodes) + countDuplicates(stats)
	stats.Valid = len(nodes)
	stats.DurationMS = time.Since(started).Milliseconds()

	e.emit(Progress{
		Stage: StageComplete, At: time.Now(),
		Discovered: stats.Discovered, Valid: stats.Valid, Duplicates: stats.Duplicates,
		Message: fmt.Sprintf("discovery complete: %d valid candidates", len(nodes)),
	})

	e.health.Snapshot()

	return nodes, stats
}

// fetchSourcesLevel fetches, parses, normalizes, validates and
// deduplicates a set of sources with bounded concurrency and
// per-source failure isolation.
func (e *Engine) fetchSourcesLevel(
	ctx context.Context,
	level Level,
	sources []source.Source,
	nodes []Node,
	dedup map[string]struct{},
	stats *Stats,
) []Node {
	levelStart := time.Now()

	ls := LevelStats{Level: level, Name: LevelName(level), Sources: len(sources)}

	e.emit(Progress{
		Stage: StageDiscovering, Level: level, LevelName: LevelName(level),
		At: time.Now(), Message: fmt.Sprintf("fetching %d sources", len(sources)),
	})

	// Health-ranked order: healthy sources first (in-backoff sources
	// still run, just last — never blacklisted).
	now := time.Now()

	ids := make([]string, len(sources))
	for i, s := range sources {
		ids[i] = s.ID
	}

	ranked := e.health.Rank(now, ids)

	byID := make(map[string]source.Source, len(sources))
	for _, s := range sources {
		byID[s.ID] = s
	}

	ordered := make([]source.Source, 0, len(sources))
	for _, id := range ranked {
		if s, ok := byID[id]; ok {
			ordered = append(ordered, s)
		}
	}

	type fetchOut struct {
		src      source.Source
		result   source.Result
		err      error
		duration time.Duration
	}

	out := make(chan fetchOut, len(ordered))

	sem := make(chan struct{}, e.cfg.FetchConcurrency)

	var wg sync.WaitGroup

	for _, src := range ordered {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)

		go func(src source.Source) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			fetcher := &source.Fetcher{
				Client:      e.getter,
				Timeout:     e.cfg.FetchTimeout,
				MaxBodySize: e.cfg.MaxBodySize,
			}

			fStart := time.Now()

			result, err := fetcher.Fetch(ctx, src)

			out <- fetchOut{src: src, result: result, err: err, duration: time.Since(fStart)}
		}(src)
	}

	wg.Wait()
	close(out)

	var contentBodies []fetchOut

	for fo := range out {
		if fo.err != nil {
			ls.Failed++
			e.health.RecordFetch(fo.src.ID, false, fo.duration.Milliseconds(), classifyFetchFailure(fo.err))
			continue
		}

		ls.FetchedOK++
		e.health.RecordFetch(fo.src.ID, true, fo.duration.Milliseconds(), "")

		if fo.result.NotModified || len(fo.result.Content) == 0 {
			continue
		}

		contentBodies = append(contentBodies, fo)
	}

	// ---- Parse stage (sequential per body: parsers are CPU-bound;
	// parallelism already happened at fetch time) ----
	e.emit(Progress{Stage: StageParsing, Level: level, LevelName: LevelName(level), At: time.Now()})

	var (
		parsedCandidates int
		validCount       int
		dupCount         int
		contentRefs      []string
	)

	for _, fo := range contentBodies {
		if ctx.Err() != nil {
			break
		}

		configs, err := e.parser.Parse(fo.result.Content)
		if err != nil || len(configs) == 0 {
			e.health.RecordParse(fo.src.ID, false)
			continue
		}

		e.health.RecordParse(fo.src.ID, true)
		parsedCandidates += len(configs)

		var (
			srcValid int
			srcDup   int
		)

		for i := range configs {
			configs[i].Normalize()

			if err := configs[i].Validate(); err != nil {
				continue
			}

			fp := configs[i].Fingerprint()
			if fp == "" {
				continue
			}

			if _, dup := dedup[fp]; dup {
				srcDup++
				continue
			}

			dedup[fp] = struct{}{}
			srcValid++

			nodes = append(nodes, Node{
				Config:       configs[i],
				DiscoveredAt: time.Now(),
				Level:        level,
				SourceID:     fo.src.ID,
				SourceURL:    fo.src.URL,
			})
		}

		validCount += srcValid
		dupCount += srcDup

		e.health.RecordYield(fo.src.ID, len(configs), srcValid, srcDup)

		// Collect content references for level 5 (bounded per body).
		if refs := ContentRefs(fo.result.Content); len(refs) > 0 {
			contentRefs = append(contentRefs, refs...)
		}
	}

	ls.Candidates = parsedCandidates
	ls.Valid = validCount
	ls.Duplicates = dupCount
	ls.DurationMS = time.Since(levelStart).Milliseconds()

	stats.Levels = append(stats.Levels, ls)

	e.emit(Progress{
		Stage: StageValidating, Level: level, LevelName: LevelName(level),
		At: time.Now(), Discovered: len(nodes), Valid: validCount, Duplicates: dupCount,
		Message: fmt.Sprintf("%d sources: %d valid, %d duplicates, %d failed",
			len(sources), validCount, dupCount, ls.Failed),
	})

	// Stash content references for the content level.
	e.pendingContentRefs = dedupeStrings(contentRefs)

	return nodes
}

// contentLevel fetches and validates content-derived references.
func (e *Engine) contentLevel(
	ctx context.Context,
	nodes []Node,
	dedup map[string]struct{},
	stats *Stats,
) []Node {
	levelStart := time.Now()

	ls := LevelStats{Level: LevelContent, Name: LevelName(LevelContent)}

	defer func() {
		ls.DurationMS = time.Since(levelStart).Milliseconds()
		stats.Levels = append(stats.Levels, ls)
	}()

	refs := e.pendingContentRefs
	if len(refs) == 0 {
		ls.Skipped = true
		ls.SkipReason = "no content references"

		return nodes
	}

	// Bounded: at most MaxProbes references per run.
	if len(refs) > e.cfg.Search.MaxProbes {
		refs = refs[:e.cfg.Search.MaxProbes]
	}

	ls.Sources = len(refs)

	var (
		parsedCandidates int
		validCount       int
		dupCount         int
	)

	for _, ref := range refs {
		if ctx.Err() != nil {
			break
		}

		src := source.Source{
			ID:      "content:" + refHash(ref),
			Name:    truncatePath(ref),
			URL:     ref,
			Enabled: true,
			Format:  "auto",
		}

		fetcher := &source.Fetcher{
			Client:      e.getter,
			Timeout:     e.cfg.FetchTimeout,
			MaxBodySize: e.cfg.MaxBodySize,
		}

		fStart := time.Now()

		result, err := fetcher.Fetch(ctx, src)
		if err != nil {
			ls.Failed++
			e.health.RecordFetch(src.ID, false, time.Since(fStart).Milliseconds(), classifyFetchFailure(err))
			continue
		}

		e.health.RecordFetch(src.ID, true, time.Since(fStart).Milliseconds(), "")

		if result.NotModified || len(result.Content) == 0 {
			continue
		}

		configs, perr := e.parser.Parse(result.Content)
		if perr != nil || len(configs) == 0 {
			e.health.RecordParse(src.ID, false)
			continue
		}

		e.health.RecordParse(src.ID, true)
		parsedCandidates += len(configs)

		var ( //nolint:dupl // same shape as fetchSourcesLevel by design
			srcValid int
			srcDup   int
		)

		for i := range configs {
			configs[i].Normalize()

			if err := configs[i].Validate(); err != nil {
				continue
			}

			fp := configs[i].Fingerprint()
			if fp == "" {
				continue
			}

			if _, dup := dedup[fp]; dup {
				srcDup++
				continue
			}

			dedup[fp] = struct{}{}
			srcValid++

			nodes = append(nodes, Node{
				Config:       configs[i],
				DiscoveredAt: time.Now(),
				Level:        LevelContent,
				SourceID:     src.ID,
				SourceURL:    ref,
			})
		}

		validCount += srcValid
		dupCount += srcDup

		e.health.RecordYield(src.ID, len(configs), srcValid, srcDup)
	}

	ls.Candidates = parsedCandidates
	ls.Valid = validCount
	ls.Duplicates = dupCount

	return nodes
}

// searchLevel runs the smart search and probes the discovered
// repositories' conventional paths. deep=true widens the query set
// (recovery/deep discovery).
func (e *Engine) searchLevel(
	ctx context.Context,
	nodes []Node,
	dedup map[string]struct{},
	stats *Stats,
	deep bool,
) []Node {
	levelStart := time.Now()

	level := LevelSearch
	name := LevelName(LevelSearch)
	if deep {
		level = LevelDeep
		name = LevelName(LevelDeep)
	}

	ls := LevelStats{Level: level, Name: name}

	defer func() {
		ls.DurationMS = time.Since(levelStart).Milliseconds()
		stats.Levels = append(stats.Levels, ls)
	}()

	if e.search.RateLimited(time.Now()) {
		ls.Skipped = true
		ls.SkipReason = "search rate-limited (backoff)"

		return nodes
	}

	e.emit(Progress{
		Stage: StageDiscovering, Level: level, LevelName: name,
		At: time.Now(), Message: "searching for public configuration sources",
	})

	now := time.Now()

	queries := DefaultSearchQueries
	if deep {
		queries = append(append([]string(nil), queries...),
			"free v2ray configs", "proxy list daily update")
	}

	candidates, err := e.search.Search(ctx, queries, now)
	if err != nil {
		ls.Failed = 1
		ls.SkipReason = classifyFetchFailure(err)

		return nodes // one failed search never blocks other levels
	}

	ls.Sources = len(candidates)

	// Probe conventional paths, bounded by the probe budget.
	probeBudget := e.cfg.Search.MaxProbes

	var (
		parsedCandidates int
		validCount       int
		dupCount         int
	)

	minYield := e.cfg.MinSourceCandidates
	if deep {
		minYield = 1 // restrictive network: accept thin sources
	}

	for _, cand := range candidates {
		if ctx.Err() != nil || probeBudget <= 0 {
			break
		}

		paths := ProbePathsFor(cand.Repo, probeBudget)

		for _, path := range paths {
			if ctx.Err() != nil || probeBudget <= 0 {
				break
			}

			rawURL := RawURLFor(cand.Repo, cand.Branch, path)

			src := source.Source{
				ID:      "search:" + cand.Repo + ":" + path,
				Name:    cand.Repo + "/" + path,
				URL:     rawURL,
				Enabled: true,
				Format:  "auto",
			}

			fetcher := &source.Fetcher{
				Client:      e.getter,
				Timeout:     e.cfg.FetchTimeout,
				MaxBodySize: e.cfg.MaxBodySize,
			}

			probeBudget--

			fStart := time.Now()

			result, ferr := fetcher.Fetch(ctx, src)
			if ferr != nil {
				ls.Failed++
				e.health.RecordFetch(src.ID, false, time.Since(fStart).Milliseconds(), classifyFetchFailure(ferr))
				continue
			}

			e.health.RecordFetch(src.ID, true, time.Since(fStart).Milliseconds(), "")

			if result.NotModified || len(result.Content) == 0 {
				continue
			}

			configs, perr := e.parser.Parse(result.Content)
			if perr != nil || len(configs) < minYield {
				e.health.RecordParse(src.ID, false)
				continue
			}

			e.health.RecordParse(src.ID, true)
			parsedCandidates += len(configs)

			var (
				srcValid int
				srcDup   int
			)

			for i := range configs {
				configs[i].Normalize()

				if verr := configs[i].Validate(); verr != nil {
					continue
				}

				fp := configs[i].Fingerprint()
				if fp == "" {
					continue
				}

				if _, dup := dedup[fp]; dup {
					srcDup++
					continue
				}

				dedup[fp] = struct{}{}
				srcValid++

				nodes = append(nodes, Node{
					Config:       configs[i],
					DiscoveredAt: time.Now(),
					Level:        level,
					SourceID:     src.ID,
					SourceURL:    rawURL,
				})
			}

			validCount += srcValid
			dupCount += srcDup

			e.health.RecordYield(src.ID, len(configs), srcValid, srcDup)
		}
	}

	ls.Candidates = parsedCandidates
	ls.Valid = validCount
	ls.Duplicates = dupCount

	e.emit(Progress{
		Stage: StageValidating, Level: level, LevelName: name,
		At: time.Now(), Discovered: len(nodes), Valid: validCount,
		Message: fmt.Sprintf("search: %d repos, %d valid candidates", ls.Sources, validCount),
	})

	return nodes
}

func countDuplicates(stats *Stats) int {
	total := 0
	for _, l := range stats.Levels {
		total += l.Duplicates
	}
	return total
}

func containsSource(sources []source.Source, id string) bool {
	for _, s := range sources {
		if s.ID == id {
			return true
		}
	}

	return false
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))

	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}

		if _, dup := seen[s]; dup {
			continue
		}

		seen[s] = struct{}{}
		out = append(out, s)
	}

	return out
}

// classifyFetchFailure condenses a fetch error into a short,
// credential-free class for health records.
func classifyFetchFailure(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()

	switch {
	case strings.Contains(msg, "429"), strings.Contains(msg, "rate limited"):
		return "rate_limited"
	case strings.Contains(msg, "403"):
		return "forbidden"
	case strings.Contains(msg, "404"):
		return "not_found"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "cancelled"):
		return "cancelled"
	case strings.Contains(msg, "body exceeds"):
		return "too_large"
	default:
		if len(msg) > 60 {
			msg = msg[:60]
		}

		return msg
	}
}

// refHash builds a stable source ID for a content-derived reference.
func refHash(ref string) string {
	// Cheap stable transform: the URL itself is unique; strip scheme
	// separators for a filesystem-safe ID.
	h := strings.NewReplacer("https://", "", "http://", "", "/", "_", ".", "_", ":", "_")
	return h.Replace(ref)
}

func truncatePath(u string) string {
	if len(u) > 48 {
		return u[:48] + "…"
	}

	return u
}
