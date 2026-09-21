// sourcereliability.go implements the v0.7 roadmap work "source
// reliability dashboards" on the EXISTING architecture (v0.9.10):
// an evidence-based health view of every configuration source.
//
// Three real evidence streams feed the report — nothing is invented:
//
//  1. FETCH evidence   — the source registry's own counters
//     (fetch attempts, successes, last success/failure), collected
//     by the source collector after every cycle.
//  2. REFRESH evidence — the last ingestion pipeline's per-source
//     results (discovered / duplicates / invalid / unchanged /
//     duration), retained as LastIngestion.
//  3. STORE evidence   — a bounded scan of the actual persisted
//     records (per-source persisted/tested/working/failed counts,
//     measured latencies, last successful test, staleness).
//
// Where evidence is insufficient the report says so with explicit
// flags (EnoughData=false, negative or zero sentinels documented on
// the fields) — the UI renders "Not enough data" instead of a score.
// The report is cached for a short window and invalidated by
// ingestion cycles and test persistence, so the dashboard never
// re-scans the store per frame.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/pipeline"
	"github.com/Parsaetak/FreeIran/engine/source"
)

// reliabilityCacheTTL bounds how long a computed report is served
// before the next request recomputes it from live evidence.
const reliabilityCacheTTL = 5 * time.Second

// staleSourceTestAge marks a source's tested configurations as stale
// when the newest measurement is older than this.
const staleSourceTestAge = 7 * 24 * time.Hour

// SourceReliabilityReport is the dashboard's top-level document.
type SourceReliabilityReport struct {
	// GeneratedAt is when the evidence was collected (Unix ms).
	GeneratedAt int64 `json:"generated_at"`

	// Overall aggregates across every source. SuccessRatePct is -1
	// when there is not enough measured data to compute one.
	Overall OverallSourceHealth `json:"overall"`

	// Sources carries one entry per configured source (enabled and
	// disabled), ordered by name.
	Sources []SourceHealthEntry `json:"sources"`
}

// OverallSourceHealth is the cross-source aggregate.
type OverallSourceHealth struct {
	TotalSources   int `json:"total_sources"`
	EnabledSources int `json:"enabled_sources"`

	// LastRefreshState describes the most recent ingestion cycle
	// ("idle" when none has run).
	LastRefreshState string `json:"last_refresh_state"`
	// LastRefreshAt is when the last ingestion cycle finished
	// (Unix ms; 0 = never).
	LastRefreshAt int64 `json:"last_refresh_at,omitempty"`

	// Pipeline totals from the last ingestion cycle (0 when none ran).
	Discovered int64 `json:"discovered,omitempty"`
	Duplicates int64 `json:"duplicates,omitempty"`
	Invalid    int64 `json:"invalid,omitempty"`
	Persisted  int64 `json:"persisted,omitempty"`

	// Store evidence (bounded scan).
	PersistedConfigs int `json:"persisted_configs"`
	TestedConfigs    int `json:"tested_configs"`
	WorkingConfigs   int `json:"working_configs"`
	FailedConfigs    int `json:"failed_configs"`
	UntestedConfigs  int `json:"untested_configs"`

	// SuccessRatePct is the measured working/tested ratio in percent,
	// or -1 when fewer than one tested configuration exists (the UI
	// renders "Not enough data").
	SuccessRatePct int `json:"success_rate_pct"`

	// MedianLatencyMS is the median measured latency of WORKING
	// configurations (0 = no working configuration was ever measured).
	MedianLatencyMS int64 `json:"median_latency_ms,omitempty"`
}

// SourceHealthEntry is one source's evidence-based health view.
type SourceHealthEntry struct {
	// Identity (from the source registry).
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Trust   string `json:"trust"`
	Region  string `json:"region,omitempty"`

	// Fetch evidence. FetchCount == 0 means the source was never
	// fetched (FetchEnoughData false — no reliability claim).
	FetchCount      int  `json:"fetch_count"`
	SuccessCount    int  `json:"success_count"`
	FetchEnoughData bool `json:"fetch_enough_data"`
	// FetchSuccessPct is measured fetch success in percent (-1 when
	// FetchCount == 0).
	FetchSuccessPct   int    `json:"fetch_success_pct"`
	LastSuccessAt     int64  `json:"last_success_at,omitempty"` // Unix ms
	LastFailureAt     int64  `json:"last_failure_at,omitempty"`
	LastFailureReason string `json:"last_failure_reason,omitempty"`

	// Refresh evidence from the last ingestion cycle (zero values =
	// the source did not participate in it).
	LastRefreshDiscovered int64  `json:"last_refresh_discovered,omitempty"`
	LastRefreshDuplicates int64  `json:"last_refresh_duplicates,omitempty"`
	LastRefreshInvalid    int64  `json:"last_refresh_invalid,omitempty"`
	LastRefreshUnchanged  bool   `json:"last_refresh_unchanged,omitempty"`
	LastRefreshDurationMS int64  `json:"last_refresh_duration_ms,omitempty"`
	LastRefreshError      string `json:"last_refresh_error,omitempty"`

	// Store evidence (bounded scan of persisted records attributed to
	// this source by its ID).
	PersistedConfigs int `json:"persisted_configs"`
	TestedConfigs    int `json:"tested_configs"`
	WorkingConfigs   int `json:"working_configs"`
	FailedConfigs    int `json:"failed_configs"`
	UntestedConfigs  int `json:"untested_configs"`
	StaleConfigs     int `json:"stale_configs"`

	// SuccessRatePct is the measured working/tested ratio in percent,
	// or -1 when the source has no tested configuration ("Not enough
	// data").
	SuccessRatePct int `json:"success_rate_pct"`

	// MedianLatencyMS is the median measured latency of this source's
	// WORKING configurations (0 = none measured).
	MedianLatencyMS int64 `json:"median_latency_ms,omitempty"`

	// LastSuccessfulTestAt is the newest LastSuccessAt among this
	// source's configurations (Unix ms; 0 = none ever verified).
	LastSuccessfulTestAt int64 `json:"last_successful_test_at,omitempty"`
}

// reliabilityCache is the bounded report cache.
type reliabilityCache struct {
	mu        sync.Mutex
	report    *SourceReliabilityReport
	generated time.Time
}

// invalidateReliability marks the cached report stale. Called when
// real evidence changes (ingestion finished, test results persisted).
func (a *App) invalidateReliability() {
	a.reliability.mu.Lock()
	a.reliability.report = nil
	a.reliability.mu.Unlock()
}

// SourceReliability renders the evidence-based source health report
// (cached; recomputed at most once per reliabilityCacheTTL or after
// an evidence change).
func (s *SourceService) SourceReliability() (*SourceReliabilityReport, error) {
	s.app.reliability.mu.Lock()
	cached := s.app.reliability.report
	generated := s.app.reliability.generated
	s.app.reliability.mu.Unlock()

	if cached != nil && time.Since(generated) < reliabilityCacheTTL {
		return cached, nil
	}

	report, err := s.app.computeSourceReliability()
	if err != nil {
		return nil, err
	}

	s.app.reliability.mu.Lock()
	s.app.reliability.report = report
	s.app.reliability.generated = time.Now()
	s.app.reliability.mu.Unlock()

	return report, nil
}

// computeSourceReliability aggregates the three evidence streams.
func (a *App) computeSourceReliability() (*SourceReliabilityReport, error) {
	report := &SourceReliabilityReport{GeneratedAt: time.Now().UTC().UnixMilli()}

	// --- registry + last-pipeline evidence ---------------------------
	a.mu.RLock()
	sources := append([]source.Source(nil), a.sources...)
	lastIngestion := a.lastStats
	a.mu.RUnlock()

	perSource := map[string]pipeline.SourceResult{}
	a.mu.RLock()
	refreshAt := a.lastIngestionAt
	a.mu.RUnlock()

	if lastIngestion != nil {
		for _, result := range lastIngestion.PerSource {
			perSource[result.SourceID] = result
		}
	}

	// --- store evidence (one bounded scan) ----------------------------
	type storeAgg struct {
		persisted          int
		tested             int
		working            int
		untested           int
		stale              int
		workingLatencies   []int64
		lastSuccessfulTest int64
	}

	aggregates := map[string]*storeAgg{}

	getAgg := func(sourceID string) *storeAgg {
		agg, ok := aggregates[sourceID]
		if !ok {
			agg = &storeAgg{}
			aggregates[sourceID] = agg
		}

		return agg
	}

	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()

	scanned := 0
	nowMS := time.Now().UnixMilli()

	err := a.store.Iterate(ctx, func(key string, value []byte) error {
		if scanned >= candidateScanLimit {
			return errCandidateLimit
		}

		scanned++

		var cfg struct {
			Source        string `json:"source,omitempty"`
			Working       bool   `json:"working"`
			LatencyMS     int64  `json:"latency_ms,omitempty"`
			TestedAt      int64  `json:"tested_at,omitempty"`
			LastSuccessAt int64  `json:"last_success_at,omitempty"`
		}

		if err := json.Unmarshal(value, &cfg); err != nil {
			return nil // undecodable: skip, never abort the scan
		}

		agg := getAgg(cfg.Source)
		agg.persisted++

		if cfg.TestedAt == 0 {
			agg.untested++

			return nil
		}

		agg.tested++

		if nowMS-cfg.TestedAt > staleSourceTestAge.Milliseconds() {
			agg.stale++
		}

		if cfg.Working {
			agg.working++
			agg.workingLatencies = append(agg.workingLatencies, cfg.LatencyMS)

			if cfg.LastSuccessAt > agg.lastSuccessfulTest {
				agg.lastSuccessfulTest = cfg.LastSuccessAt
			}
		}

		return nil
	})

	if err != nil && !errors.Is(err, errCandidateLimit) && ctx.Err() == nil {
		return nil, err
	}

	// --- assemble per-source entries -----------------------------------
	// The entry set is the UNION of the registry sources and the
	// source IDs that actually appear in the store: configurations
	// from a since-removed source keep their evidence visible
	// (Enabled=false, no fetch evidence) instead of vanishing from
	// the dashboard.
	entryIDs := make(map[string]bool, len(sources)+len(aggregates))

	for _, src := range sources {
		entryIDs[src.ID] = true

		entry := SourceHealthEntry{
			ID:      src.ID,
			Name:    src.Name,
			Enabled: src.Enabled,
			Trust:   string(src.RouteTrust()),
			Region:  src.Region,
		}

		// Fetch evidence.
		entry.FetchCount = src.FetchCount
		entry.SuccessCount = src.SuccessCount

		if src.FetchCount > 0 {
			entry.FetchEnoughData = true
			entry.FetchSuccessPct = int(math.Round(float64(src.SuccessCount) / float64(src.FetchCount) * 100))
		} else {
			entry.FetchSuccessPct = -1
		}

		if !src.LastSuccessfulFetch.IsZero() {
			entry.LastSuccessAt = src.LastSuccessfulFetch.UnixMilli()
		}

		if !src.LastFailure.IsZero() {
			entry.LastFailureAt = src.LastFailure.UnixMilli()
			entry.LastFailureReason = src.LastFailureReason
		}

		report.Sources = append(report.Sources, entry)
	}

	// Store-only sources (removed from the registry but still
	// holding persisted configurations) surface as retired entries.
	storeOnlyIDs := make([]string, 0, len(aggregates))

	for sourceID := range aggregates {
		if sourceID == "" {
			continue // unattributed records are counted in overall only
		}

		if !entryIDs[sourceID] {
			storeOnlyIDs = append(storeOnlyIDs, sourceID)
		}
	}

	sort.Strings(storeOnlyIDs)

	for _, sourceID := range storeOnlyIDs {
		report.Sources = append(report.Sources, SourceHealthEntry{
			ID:              sourceID,
			Name:            sourceID,
			Enabled:         false,
			Trust:           string(source.TrustPublic),
			FetchSuccessPct: -1, // no fetch evidence
			SuccessRatePct:  -1, // filled from store evidence below
		})

		entryIDs[sourceID] = true
	}

	// Attach refresh + store evidence to every entry.
	for i := range report.Sources {
		entry := &report.Sources[i]

		// Refresh evidence.
		if result, ok := perSource[entry.ID]; ok {
			entry.LastRefreshDiscovered = result.Discovered
			entry.LastRefreshDuplicates = result.Duplicates
			entry.LastRefreshInvalid = result.Invalid
			entry.LastRefreshUnchanged = result.Unchanged
			entry.LastRefreshDurationMS = result.DurationMS
			entry.LastRefreshError = result.Error
		}

		// Store evidence.
		agg, ok := aggregates[entry.ID]
		if !ok || agg == nil {
			if entry.SuccessRatePct == 0 {
				entry.SuccessRatePct = -1 // no persisted evidence at all
			}

			continue
		}

		entry.PersistedConfigs = agg.persisted
		entry.TestedConfigs = agg.tested
		entry.WorkingConfigs = agg.working
		entry.FailedConfigs = agg.tested - agg.working
		entry.UntestedConfigs = agg.untested
		entry.StaleConfigs = agg.stale
		entry.MedianLatencyMS = medianInt64(agg.workingLatencies)
		entry.LastSuccessfulTestAt = agg.lastSuccessfulTest

		if agg.tested > 0 {
			entry.SuccessRatePct = int(math.Round(float64(agg.working) / float64(agg.tested) * 100))
		} else {
			entry.SuccessRatePct = -1 // not enough data
		}
	}

	sort.Slice(report.Sources, func(i, j int) bool {
		return report.Sources[i].Name < report.Sources[j].Name
	})

	// --- overall ---------------------------------------------------------
	overall := &report.Overall
	overall.TotalSources = len(sources)
	overall.PersistedConfigs = scanned

	var allWorkingLatencies []int64

	for _, entry := range report.Sources {
		if entry.Enabled {
			overall.EnabledSources++
		}

		overall.TestedConfigs += entry.TestedConfigs
		overall.WorkingConfigs += entry.WorkingConfigs
		overall.FailedConfigs += entry.FailedConfigs
		overall.UntestedConfigs += entry.UntestedConfigs

	}

	// Median latency across sources (bounded by the per-source medians
	// to keep the aggregate O(sources), not O(records)).
	for _, entry := range report.Sources {
		if entry.WorkingConfigs > 0 && entry.MedianLatencyMS > 0 {
			allWorkingLatencies = append(allWorkingLatencies, entry.MedianLatencyMS)
		}
	}

	overall.MedianLatencyMS = medianInt64(allWorkingLatencies)

	if overall.TestedConfigs > 0 {
		overall.SuccessRatePct = int(math.Round(
			float64(overall.WorkingConfigs) / float64(overall.TestedConfigs) * 100))
	} else {
		overall.SuccessRatePct = -1 // not enough data
	}

	overall.LastRefreshAt = refreshAt

	if lastIngestion == nil {
		overall.LastRefreshState = "idle"
	} else if lastIngestion.SourcesFailed > 0 {
		overall.LastRefreshState = "partial"
	} else {
		overall.LastRefreshState = "ok"
	}

	overall.Discovered = lastIngestionSum(lastIngestion, func(r pipeline.SourceResult) int64 { return r.Discovered })
	overall.Duplicates = lastIngestionSum(lastIngestion, func(r pipeline.SourceResult) int64 { return r.Duplicates })
	overall.Invalid = lastIngestionSum(lastIngestion, func(r pipeline.SourceResult) int64 { return r.Invalid })
	overall.Persisted = lastIngestionSum(lastIngestion, func(r pipeline.SourceResult) int64 { return r.Unique })

	return report, nil
}

// lastIngestionSum folds a per-source field of the last pipeline run
// (0 when no run is retained).
func lastIngestionSum(stats *pipeline.Stats, pick func(pipeline.SourceResult) int64) int64 {
	if stats == nil {
		return 0
	}

	var total int64

	for _, result := range stats.PerSource {
		total += pick(result)
	}

	return total
}

// medianInt64 renders the median of a sample (0 for empty input; a
// single 0 reading is a measured sub-millisecond value and stays 0).
func medianInt64(sample []int64) int64 {
	if len(sample) == 0 {
		return 0
	}

	sorted := append([]int64(nil), sample...)

	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	mid := len(sorted) / 2

	if len(sorted)%2 == 1 {
		return sorted[mid]
	}

	return (sorted[mid-1] + sorted[mid]) / 2
}
