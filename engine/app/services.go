package app

// This file defines the service surface bound to the TypeScript
// frontend. Each service groups related operations so the generated
// bindings read as namespaced APIs. Methods never block the UI: long
// operations either run in the background (reported via State) or
// accept a short-lived wait bounded by context.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/metrics"
	"github.com/Parsaetak/FreeIran/engine/pipeline"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/store"
	"github.com/Parsaetak/FreeIran/engine/testqueue"
	"github.com/Parsaetak/FreeIran/internal/version"
	"github.com/Parsaetak/FreeIran/system"
)

// SourceView is the UI projection of a source.
type SourceView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
	LastHash string `json:"last_hash,omitempty"`
	// Trust is the ROUTE-trust band of the source (v0.9.8.6):
	// "official" | "user" | "public". The Sources UI labels untrusted
	// public routes explicitly; the Quick Connect policy enforces the
	// boundary on the backend.
	Trust string `json:"trust,omitempty"`
}

// SourceService manages configuration sources.
type SourceService struct {
	app *App
}

// NewSourceService binds a source service to the app.
func NewSourceService(a *App) *SourceService {
	return &SourceService{app: a}
}

// List returns all configured sources.
func (s *SourceService) List() []SourceView {
	s.app.mu.RLock()
	defer s.app.mu.RUnlock()

	out := make([]SourceView, 0, len(s.app.sources))

	for _, src := range s.app.sources {
		out = append(out, SourceView{
			ID:       src.ID,
			Name:     src.Name,
			URL:      src.URL,
			Enabled:  src.Enabled,
			LastHash: s.app.seenHashes[src.ID],
			Trust:    string(src.RouteTrust()),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})

	return out
}

// SetEnabled enables or disables a source; the change persists
// immediately.
func (s *SourceService) SetEnabled(id string, enabled bool) error {
	s.app.mu.Lock()

	found := false

	for i := range s.app.sources {
		if s.app.sources[i].ID == id {
			s.app.sources[i].Enabled = enabled
			found = true

			break
		}
	}

	s.app.mu.Unlock()

	if !found {
		return fmt.Errorf("app: source %q not found", id)
	}

	return s.app.saveSources()
}

// Add registers a new source.
func (s *SourceService) Add(id, name, url string) error {
	if id == "" || url == "" {
		return fmt.Errorf("app: source id and url are required")
	}

	s.app.mu.Lock()

	for _, src := range s.app.sources {
		if src.ID == id {
			s.app.mu.Unlock()

			return fmt.Errorf("app: source %q already exists", id)
		}
	}

	s.app.sources = append(s.app.sources, source.Source{
		ID:      id,
		Name:    name,
		URL:     url,
		Enabled: true,
		Custom:  true,
		Trust:   source.TrustUser,
	})

	s.app.mu.Unlock()

	return s.app.saveSources()
}

// Remove deletes a source.
func (s *SourceService) Remove(id string) error {
	s.app.mu.Lock()

	kept := s.app.sources[:0]

	found := false

	for _, src := range s.app.sources {
		if src.ID == id {
			found = true

			continue
		}

		kept = append(kept, src)
	}

	s.app.sources = kept

	delete(s.app.seenHashes, id)

	s.app.mu.Unlock()

	if !found {
		return fmt.Errorf("app: source %q not found", id)
	}

	return s.app.saveSources()
}

// RefreshNow triggers an immediate ingestion cycle and waits for it
// to finish. The UI should call this from a background task and show
// progress via State.
func (s *SourceService) RefreshNow() (*pipeline.Stats, error) {
	if s.app.ctx == nil {
		return nil, fmt.Errorf("app: not started")
	}

	if !s.app.started.Load() {
		// Scheduler not started (tests/CLI): run inline.
		if err := s.app.runIngestionCycle(s.app.ctx); err != nil {
			return nil, err
		}

		s.app.mu.RLock()
		defer s.app.mu.RUnlock()

		return s.app.lastStats, nil
	}

	// Ask the scheduler to wake and wait for the cycle to complete.
	wasIngesting := s.app.ingesting.Load()

	s.app.scheduler.Wake()

	deadline := time.Now().Add(10 * time.Minute)

	for time.Now().Before(deadline) {
		if wasIngesting {
			// Wait for the pre-existing cycle then confirm a fresh
			// one has not started behind it.
			time.Sleep(200 * time.Millisecond)
			wasIngesting = s.app.ingesting.Load()

			continue
		}

		break
	}

	// Wait for any in-flight cycle triggered by Wake to finish.
	for s.app.ingesting.Load() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}

	s.app.mu.RLock()
	defer s.app.mu.RUnlock()

	return s.app.lastStats, nil
}

// DataService exposes stored configurations to the UI.
type DataService struct {
	app *App
}

// NewDataService binds a data service to the app.
func NewDataService(a *App) *DataService {
	return &DataService{app: a}
}

// ConfigPage is one page of configurations for virtualized lists.
type ConfigPage struct {
	Items   []config.Config `json:"items"`
	Total   int             `json:"total"`
	Offset  int             `json:"offset"`
	HasMore bool            `json:"has_more"`
}

// errPageComplete stops the store iteration once the requested page
// is full (a normal condition, never an error): Total/HasMore derive
// from Count(), so walking past the page boundary only burns decodes.
// A dedicated sentinel — not context.Canceled — keeps a genuine
// context timeout distinguishable from the clean stop.
var errPageComplete = errors.New("page complete")

// ListConfigs returns a page of configurations ordered by fingerprint.
// Pages are virtualized in the UI; only the requested window is
// decoded and materialised.
func (s *DataService) ListConfigs(offset, limit int) (*ConfigPage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	if offset < 0 {
		offset = 0
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 10*time.Second)
	defer cancel()

	page := &ConfigPage{Offset: offset, Items: make([]config.Config, 0, limit)}

	skipped := 0

	err := s.app.store.Iterate(ctx, func(key string, value []byte) error {
		// Purely counted records: never decoded, never cached. Decoding
		// records only to throw them away made deep pages cost O(offset)
		// JSON unmarshals and page 0 of a large store decode the whole
		// index; Count() below needs no iteration, so the walk can stop
		// the moment the page is full.
		if skipped < offset {
			skipped++

			return nil
		}

		if len(page.Items) >= limit {
			return errPageComplete
		}

		// Hot cache: decoded configs are reused across pages.
		if cached, ok := s.app.hotCache.Get(key, 0); ok {
			if cfg, isCfg := cached.(config.Config); isCfg {
				page.Items = append(page.Items, cfg)

				return nil
			}
		}

		var cfg config.Config

		if err := json.Unmarshal(value, &cfg); err != nil {
			return nil // skip undecodable record; never crash the UI
		}

		s.app.hotCache.Put(key, cfg, 0)

		page.Items = append(page.Items, cfg)

		return nil
	})

	if err != nil && !errors.Is(err, errPageComplete) {
		return nil, err
	}

	page.Total = s.app.store.Count()
	page.HasMore = offset+len(page.Items) < page.Total

	return page, nil
}

// SearchConfigs returns configurations matching a case-insensitive
// substring across address, name and protocol. Results are bounded.
func (s *DataService) SearchConfigs(query string, limit int) ([]config.Config, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 10*time.Second)
	defer cancel()

	matches := make([]config.Config, 0, 16)

	q := strings.ToLower(query)

	if q == "" {
		page, err := s.ListConfigs(0, limit)

		if err != nil {
			return nil, err
		}

		return page.Items, nil
	}

	err := s.app.store.Iterate(ctx, func(key string, value []byte) error {
		if len(matches) >= limit {
			return context.Canceled
		}

		var cfg config.Config

		if err := json.Unmarshal(value, &cfg); err != nil {
			return nil
		}

		if strings.Contains(strings.ToLower(cfg.Address), q) ||
			strings.Contains(strings.ToLower(cfg.Name), q) ||
			strings.Contains(strings.ToLower(string(cfg.Type)), q) {
			matches = append(matches, cfg)
		}

		return nil
	})

	if err != nil && err != context.Canceled {
		return nil, err
	}

	return matches, nil
}

// ConfigFilter describes server-side filtering + sorting for the
// configuration workspace (v0.9.0 §4). Empty fields match everything.
type ConfigFilter struct {
	// Protocol filters by protocol ("vless", "vmess", ...).
	Protocol string `json:"protocol,omitempty"`

	// Status filters by test outcome: "working", "failed" or
	// "untested" ("" = all).
	Status string `json:"status,omitempty"`

	// Source filters by source ID.
	Source string `json:"source,omitempty"`

	// Backend filters by the backend that ran the last test
	// ("xray", "v2ray", "sing-box", "tcp").
	Backend string `json:"backend,omitempty"`

	// Query is a case-insensitive substring over address/name.
	Query string `json:"query,omitempty"`

	// Group filters by a built-in group ("favorites", "working",
	// "untested", "fast", "recently_tested"; "all"/"" = everything)
	// or a user group id ("g-1", ...). v0.9.10: one filter pipeline,
	// the same evidence fields the ranking engine scores — favorites
	// and groups never bypass testing or trust.
	Group string `json:"group,omitempty"`

	// SortBy is one of: "fingerprint", "latency", "tested_at",
	// "protocol", "source", "address" (default "fingerprint").
	SortBy string `json:"sort_by,omitempty"`

	// SortDesc flips the ordering.
	SortDesc bool `json:"sort_desc,omitempty"`
}

// maxFilteredMatches bounds the materialised match set so a filter
// over a huge store cannot balloon memory (the queue and detail views
// work on fingerprints; the list only needs this window).
const maxFilteredMatches = 20000

// ListConfigsFiltered returns a sorted, filtered page of
// configurations. Filtering happens engine-side; only the requested
// window crosses the service boundary.
func (s *DataService) ListConfigsFiltered(filter ConfigFilter, offset, limit int) (*ConfigPage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	if offset < 0 {
		offset = 0
	}

	ctx, cancel := context.WithTimeout(s.app.ctx, 30*time.Second)
	defer cancel()

	query := strings.ToLower(filter.Query)

	matches := make([]config.Config, 0, 256)

	if err := s.app.store.Iterate(ctx, func(key string, value []byte) error {
		if len(matches) >= maxFilteredMatches {
			return context.Canceled
		}

		var cfg config.Config
		if err := json.Unmarshal(value, &cfg); err != nil {
			return nil // skip undecodable record; never crash the UI
		}

		if !s.filterMatches(filter, cfg, query) {
			return nil
		}

		matches = append(matches, cfg)

		return nil
	}); err != nil && err != context.Canceled {
		return nil, err
	}

	// Manual ordering (v0.9.8.3) applies unless the caller asked for
	// an explicit sort — explicit sorts are view-local, the stored
	// order stays intact underneath.
	if filter.SortBy == "" {
		matches = s.app.applyConfigOrder(matches)
	}

	sortConfigs(matches, filter)

	total := len(matches)
	page := &ConfigPage{Offset: offset, Total: total}

	if offset < total {
		end := offset + limit
		if end > total {
			end = total
		}

		page.Items = matches[offset:end]
	} else {
		page.Items = []config.Config{}
	}

	page.HasMore = offset+len(page.Items) < total

	return page, nil
}

// filterMatches applies the filter predicate (a DataService method so
// the v0.9.10 Group filter can consult the app's collections without
// globals or duplicated pipelines).
func (s *DataService) filterMatches(filter ConfigFilter, cfg config.Config, query string) bool {
	if filter.Protocol != "" && string(cfg.Type) != filter.Protocol {
		return false
	}

	switch filter.Status {
	case "working":
		if !(cfg.TestedAt > 0 && cfg.Working) {
			return false
		}
	case "failed":
		if !(cfg.TestedAt > 0 && !cfg.Working) {
			return false
		}
	case "untested":
		if cfg.TestedAt != 0 {
			return false
		}
	}

	if filter.Source != "" && cfg.Source != filter.Source {
		return false
	}

	if filter.Backend != "" && cfg.TestBackend != filter.Backend {
		return false
	}

	if filter.Group != "" && !s.groupMatches(filter.Group, cfg) {
		return false
	}

	if query != "" &&
		!strings.Contains(strings.ToLower(cfg.Address), query) &&
		!strings.Contains(strings.ToLower(cfg.Name), query) {
		return false
	}

	return true
}

// groupMatches evaluates the v0.9.10 Group filter for one record
// against the built-in evidence groups (computed from the record's
// own measured fields — never stored labels) and the user's group
// membership (stable config IDs, persisted in collections.json).
// Favorites and groups never bypass testing or trust: they only
// narrow which records the filter returns.
func (s *DataService) groupMatches(group string, cfg config.Config) bool {
	now := time.Now().UnixMilli()

	switch group {
	case "", "all":
		return true

	case "favorites":
		s.app.loadCollections()

		s.app.collections.mu.Lock()
		defer s.app.collections.mu.Unlock()

		for _, id := range s.app.collections.favorites {
			if id == cfg.ID {
				return true
			}
		}

		return false

	case "working":
		return cfg.TestedAt > 0 && cfg.Working

	case "untested":
		return cfg.TestedAt == 0

	case "fast":
		// A WORKING config with a real measurement at or below the
		// threshold; 0 ms on a working config is measured (fastest).
		return cfg.TestedAt > 0 && cfg.Working && cfg.LatencyMS <= fastGroupThresholdMS

	case "recently_tested":
		return cfg.TestedAt > 0 && now-cfg.TestedAt <= recentlyTestedWindow.Milliseconds()
	}

	// User group id: membership by stable config ID.
	if strings.HasPrefix(group, userGroupIDPrefix) {
		s.app.loadCollections()

		s.app.collections.mu.Lock()
		defer s.app.collections.mu.Unlock()

		for _, grp := range s.app.collections.groups {
			if grp.ID != group {
				continue
			}

			for _, id := range grp.ConfigIDs {
				if id == cfg.ID {
					return true
				}
			}

			return false
		}
	}

	return false
}

// sortConfigs orders the match set in place.
func sortConfigs(list []config.Config, filter ConfigFilter) {
	less := func(i, j int) bool { return list[i].ID < list[j].ID } //nolint:gocritic // default

	switch filter.SortBy {
	case "latency":
		// v0.9.8.1: a WORKING config always carries a measurement, and
		// LatencyMS == 0 on a working config is a measured sub-milli-
		// second round trip — the fastest, not "unmeasured". Sort
		// measured-first, then ascending ms, then deterministically.
		less = func(i, j int) bool {
			a, b := list[i], list[j]
			am := a.Working && a.TestedAt > 0
			bm := b.Working && b.TestedAt > 0
			if am != bm {
				return am
			}
			if am && a.LatencyMS != b.LatencyMS {
				return a.LatencyMS < b.LatencyMS
			}
			return a.ID < b.ID
		}
	case "tested_at":
		less = func(i, j int) bool { return list[i].TestedAt < list[j].TestedAt }
	case "protocol":
		less = func(i, j int) bool { return list[i].Type < list[j].Type }
	case "source":
		less = func(i, j int) bool { return list[i].Source < list[j].Source }
	case "address":
		less = func(i, j int) bool { return list[i].Address < list[j].Address }
	default:
		less = func(i, j int) bool { return list[i].ID < list[j].ID }
	}

	if filter.SortDesc {
		sort.Slice(list, func(i, j int) bool { return less(j, i) })
	} else {
		sort.Slice(list, less)
	}
}

// GetConfig returns one configuration by fingerprint.
func (s *DataService) GetConfig(id string) (*config.Config, error) {
	if cached, ok := s.app.hotCache.Get(id, 0); ok {
		if cfg, isCfg := cached.(config.Config); isCfg {
			return &cfg, nil
		}
	}

	value, err := s.app.store.Get(id)
	if err != nil {
		return nil, err
	}

	var cfg config.Config

	if err := json.Unmarshal(value, &cfg); err != nil {
		return nil, fmt.Errorf("app: decode config: %w", err)
	}

	s.app.hotCache.Put(id, cfg, 0)

	return &cfg, nil
}

// TestPrioritySingle is the queue priority of ONE user-initiated test
// (above discovery (+100) and bulk tests (+400): a person clicked, a
// person waits).
const TestPrioritySingle = 500

// TestConfig enqueues ONE configuration for testing through the app's
// ONE authoritative testing engine (testqueue → worker → tester →
// persistence) and returns PROMPTLY with the config's current
// snapshot — v0.9.15: the UI never blocks on a synchronous network
// test, and single tests share every queue guarantee bulk testing has
// (deduplication, priority, per-backend concurrency, supervised core
// processes, cancellation, retry, honest persistence).
//
// The enqueue is idempotent: a repeated click on an already
// queued/running config collapses into the existing task (ErrDuplicate
// is success), so redundant work is never created. The outcome reaches
// the UI through the queue's own result path (the adapter persists the
// result with the config), which the frontend observes incrementally —
// no full-list refresh anywhere.
func (s *DataService) TestConfig(id string) (*config.Config, error) {
	cfg, err := s.GetConfig(id)
	if err != nil {
		return nil, err
	}

	q, err := s.app.ensureTestQueue()
	if err != nil {
		return nil, err
	}

	if _, err := q.Enqueue(id, string(cfg.Type), nil, TestPrioritySingle, cfg.Source, testqueue.EnqueueDefault); err != nil {
		// Already queued or already running: the task exists, one test
		// will run — exactly what the user asked for.
		if !errors.Is(err, testqueue.ErrDuplicate) {
			return nil, fmt.Errorf("enqueue test: %w", err)
		}
	}

	return cfg, nil
}

// storeGetConfig fetches one stored configuration by fingerprint
// (shared helper for the service layer).
func (a *App) storeGetConfig(fingerprint string) (*config.Config, error) {
	value, err := a.store.Get(fingerprint)
	if err != nil {
		return nil, err
	}

	cfg := &config.Config{}
	if err := json.Unmarshal(value, cfg); err != nil {
		return nil, fmt.Errorf("app: decode config: %w", err)
	}

	cfg.ID = fingerprint

	return cfg, nil
}

// StorageService exposes storage maintenance operations.
type StorageService struct {
	app *App
}

// NewStorageService binds a storage service.
func NewStorageService(a *App) *StorageService {
	return &StorageService{app: a}
}

// Stats returns the chunked-store statistics.
func (s *StorageService) Stats() store.Stats {
	return s.app.store.Snapshot()
}

// DataDir returns the resolved data directory (v0.9.1, Developer
// settings: "open data directory" shows the path next to the action).
func (s *StorageService) DataDir() string {
	return s.app.layout.Data
}

// OpenDataDir opens the platform file manager at the data directory
// (v0.9.1 Developer settings action).
func (s *StorageService) OpenDataDir() error {
	s.app.logger.Info("app", "data_dir_opened", "user opened the data location")

	return system.OpenDirectory(s.app.layout.Data)
}

// Verify runs a full chunk integrity check. Progress can be observed
// through the returned result.
func (s *StorageService) Verify() (VerifyResult, error) {
	total := 0

	err := s.app.store.VerifyAll(context.Background(), func(_, t int) {
		total = t
	})

	return VerifyResult{
		ChunksChecked: total,
		OK:            err == nil,
		Error:         errString(err),
	}, err
}

// Compact reclaims dead records.
func (s *StorageService) Compact() error {
	s.app.logger.Info("store", "compaction_start", "compaction started")

	err := s.app.store.Compact(context.Background())

	if err != nil {
		s.app.logger.Error("store", "compaction_error", "compact", "storage",
			"compaction failed: %v", err)

		return err
	}

	s.app.logger.Info("store", "compaction_success",
		"compaction complete (%d records)", s.app.store.Count())

	return nil
}

// MigrateLegacy imports a legacy JSON database file into the store.
// The full migration lifecycle is written to the runtime log: start,
// success (with counts) and failure (with the storage error).
func (s *StorageService) MigrateLegacy(path string) (store.MigrationResult, error) {
	s.app.logger.Info("store", "migration_start",
		"legacy JSON migration started from %s", path)

	result, err := s.app.store.MigrateFromJSON(context.Background(),
		store.MigrateOptions{LegacyPath: path, Strict: false})

	if err != nil {
		s.app.logger.Error("store", "migration_error", "migrate", "storage",
			"legacy migration failed (%d migrated): %v", result.Migrated, err)

		return result, err
	}

	s.app.logger.Info("store", "migration_success",
		"legacy migration complete: %d migrated, %d skipped, renamed=%v",
		result.Migrated, result.Skipped, result.Renamed)

	return result, nil
}

// VerifyResult reports the outcome of a storage verification.
type VerifyResult struct {
	ChunksChecked int    `json:"chunks_checked"`
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
}

// DiagnosticsService exposes local-only performance diagnostics.
type DiagnosticsService struct {
	app *App
}

// NewDiagnosticsService binds a diagnostics service.
func NewDiagnosticsService(a *App) *DiagnosticsService {
	return &DiagnosticsService{app: a}
}

// Metrics returns the engine performance counters.
func (s *DiagnosticsService) Metrics() metrics.Snapshot {
	return s.app.metricsR.Snapshot()
}

// Memory returns the Memory Booster 2.0 report: pressure state,
// Go/native memory picture, adaptive settings and the subsystem
// measurements that produced them. Every value is a live measurement
// or a live controller state — nothing is synthetic.
func (s *DiagnosticsService) Memory() MemorySnapshot {
	return s.app.memory.Snapshot()
}

// StoreDiagnostics returns the deep storage-subsystem report: open
// file handles, cache hit rates, memtable pressure, WAL size, flush
// and compaction timings. Every value is a live measurement.
func (s *DiagnosticsService) StoreDiagnostics() store.Diagnostics {
	return s.app.store.Inspect()
}

// SystemInfo returns platform information.
func (s *DiagnosticsService) SystemInfo() system.Info {
	return system.GetInfo()
}

// Cores lists discovered protocol-core binaries.
func (s *DiagnosticsService) Cores() []system.CoreBinary {
	return s.app.coreLocator.DiscoverAll(context.Background())
}

// Version returns the application version string.
func (s *DiagnosticsService) Version() string {
	return version.String()
}

// AppService exposes lifecycle and aggregate state.
type AppService struct {
	app *App
}

// NewAppService binds the app service.
func NewAppService(a *App) *AppService {
	return &AppService{app: a}
}

// State returns the application state snapshot.
func (s *AppService) State() AppState {
	return s.app.State()
}

// CacheStats summarizes the cache layers.
type CacheStats struct {
	HotConfigEntries int     `json:"hot_config_entries"`
	HotConfigHits    int64   `json:"hot_config_hits"`
	HotConfigMisses  int64   `json:"hot_config_misses"`
	HotConfigHitRate float64 `json:"hot_config_hit_rate"`
	SourceEntries    int     `json:"source_entries"`
}

// CacheStats returns cache layer statistics.
func (s *AppService) CacheStats() CacheStats {
	hot := s.app.hotCache.Snapshot()
	src := s.app.sourceCache.Snapshot()

	return CacheStats{
		HotConfigEntries: hot.Entries,
		HotConfigHits:    hot.Hits,
		HotConfigMisses:  hot.Misses,
		HotConfigHitRate: hot.HitRate,
		SourceEntries:    src.Entries,
	}
}

// ClearCaches drops all cached data (does not touch stored records).
func (s *AppService) ClearCaches() {
	s.app.hotCache.Clear()
	s.app.sourceCache.Clear()
}

func errString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
