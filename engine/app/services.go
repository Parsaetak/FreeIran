package app

// This file defines the service surface bound to the TypeScript
// frontend. Each service groups related operations so the generated
// bindings read as namespaced APIs. Methods never block the UI: long
// operations either run in the background (reported via State) or
// accept a short-lived wait bounded by context.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/metrics"
	"github.com/Parsaetak/FreeIran/engine/pipeline"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/store"
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
		// Hot cache: decoded configs are reused across pages.
		if cached, ok := s.app.hotCache.Get(key, 0); ok {
			if cfg, isCfg := cached.(config.Config); isCfg {
				if skipped >= offset && len(page.Items) < limit {
					page.Items = append(page.Items, cfg)
				} else {
					skipped++
				}

				return nil
			}
		}

		var cfg config.Config

		if err := json.Unmarshal(value, &cfg); err != nil {
			return nil // skip undecodable record; never crash the UI
		}

		if skipped >= offset && len(page.Items) < limit {
			s.app.hotCache.Put(key, cfg, 0)

			page.Items = append(page.Items, cfg)

			return nil
		}

		skipped++

		return nil
	})

	if err != nil {
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

// TestConfig runs a reachability test and persists the outcome.
func (s *DataService) TestConfig(id string) (*config.Config, error) {
	cfg, err := s.GetConfig(id)
	if err != nil {
		return nil, err
	}

	result := s.app.tester.TestAndApply(s.app.ctx, cfg)

	s.app.metricsR.AddTestExecuted(result.Working)

	// Persist the updated runtime fields.
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("app: encode config: %w", err)
	}

	if err := s.app.store.Upsert(id, raw); err != nil {
		return nil, err
	}

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
