// Package app composes FreeIran's engine services into the runnable
// application: chunked storage, streaming ingestion, caching, testing,
// scheduling and system integration.
//
// Lifecycle (staged startup):
//
//	BOOT → minimal system init → store open (metadata-first) →
//	READY → background storage verification → cache warm-up →
//	scheduled source refresh → background testing
//
// The UI can bind as soon as New returns; heavy work continues in the
// background and is reported through State().
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/engine/cache"
	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/engine/metrics"
	"github.com/Parsaetak/FreeIran/engine/native"
	"github.com/Parsaetak/FreeIran/engine/pipeline"
	"github.com/Parsaetak/FreeIran/engine/scheduler"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/store"
	"github.com/Parsaetak/FreeIran/engine/tester"
	"github.com/Parsaetak/FreeIran/internal/logging"
	"github.com/Parsaetak/FreeIran/internal/version"
	"github.com/Parsaetak/FreeIran/system"
)

// Subsystem identifies the app layer in structured errors.
const Subsystem = "app"

// Options configures the application.
type Options struct {
	// BaseDir overrides the platform default data directory.
	BaseDir string

	// Pipeline tunes ingestion concurrency.
	Pipeline pipeline.Config

	// RefreshInterval is the source refresh cadence.
	RefreshInterval time.Duration

	// RefreshJitter randomizes the refresh cadence.
	RefreshJitter time.Duration

	// RunIngestionOnStart triggers a refresh cycle at boot.
	RunIngestionOnStart bool

	// SkipDefaultSources starts with an empty source list instead
	// of the built-in public sources. Used by tests and headless
	// setups.
	SkipDefaultSources bool

	// Logger overrides the runtime log (the desktop entrypoint opens
	// it early so boot failures are captured). When nil, a logger is
	// created under <BaseDir>/logs.
	Logger *logging.Logger
}

// DefaultOptions returns production defaults.
func DefaultOptions() Options {
	return Options{
		Pipeline:            pipeline.DefaultConfig(),
		RefreshInterval:     time.Hour,
		RefreshJitter:       5 * time.Minute,
		RunIngestionOnStart: true,
	}
}

// persistedSources is the on-disk source configuration.
type persistedSources struct {
	Version int             `json:"version"`
	Sources []source.Source `json:"sources"`
	// ContentHashes maps source IDs to the last seen payload hash.
	ContentHashes map[string]string `json:"content_hashes"`
}

const sourcesFormatVersion = 1

// App is the FreeIran application orchestrator.
type App struct {
	opts   Options
	layout system.DirNames

	store    *store.Store
	pipe     *pipeline.Pipeline
	metricsR *metrics.Registry

	sourceCache *cache.Layer
	hotCache    *cache.Layer

	coreLocator  *system.CoreLocator
	coreRegistry *core.Registry
	connMgr      *connection.Manager
	tester       *tester.Tester
	scheduler    *scheduler.Scheduler

	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.RWMutex
	state      AppState
	sources    []source.Source
	seenHashes map[string]string
	lastStats  *pipeline.Stats

	ingesting atomic.Bool
	started   atomic.Bool

	logger   *logging.Logger
	settings Settings

	// shutdownOnce guarantees Shutdown runs exactly once: subsystems
	// stop first (scheduler, connection manager, store), then the
	// logger closes last. Double-Shutdown is a safe no-op.
	shutdownOnce sync.Once
	shutdownErr  error
}

// AppState is the application state surfaced to the UI.
type AppState struct {
	Status           string          `json:"status"`
	Version          string          `json:"version"`
	StartedAt        int64           `json:"started_at"`
	ConfigCount      int             `json:"config_count"`
	IngestionRunning bool            `json:"ingestion_running"`
	NativeAcceler    string          `json:"native_acceleration"`
	Storage          store.Stats     `json:"storage"`
	LastIngestion    *pipeline.Stats `json:"last_ingestion,omitempty"`
}

// New boots the application to the READY state. Heavy verification
// and cache warming continue after Start.
func New(opts Options) (*App, error) {
	if opts.BaseDir == "" {
		opts.BaseDir = system.DefaultBaseDir()
	}

	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = time.Hour
	}

	layout, err := system.EnsureLayout(opts.BaseDir)
	if err != nil {
		return nil, err
	}

	// Persistent runtime log: opened BEFORE the store so storage and
	// migration failures are captured (§16).
	logger := opts.Logger

	if logger == nil {
		logger, err = logging.Open(logging.Options{
			Dir:  layout.Logs,
			Name: "freeiran.log",
		})
		if err != nil {
			return nil, fmt.Errorf("app: open runtime log: %w", err)
		}
	}

	logging.SetGlobal(logger)
	logger.Info("app", "application_start",
		"FreeIran %s starting (base %s)", version.String(), opts.BaseDir)

	st, err := store.Open(store.Options{
		Path: layout.Data,
	})
	if err != nil {
		logger.Error("store", "store_error", "open", "environment",
			"store open failed: %v", err)

		_ = logger.Close()

		return nil, fmt.Errorf("app: open store: %w", err)
	}

	logger.Info("store", "store_open",
		"store ready (%d records, %d chunks)",
		st.Count(), st.Snapshot().ChunkCount)

	mreg := metrics.New()

	ctx, cancel := context.WithCancel(context.Background())

	// The core registry binds backend adapters to executable
	// discovery: xray (priority 0), v2ray (1), sing-box (2).
	coreRegistry := core.NewRegistry(system.NewCoreLocator(layout.Cores))

	// fail closes every resource already created by New so the
	// caller never has to clean up after a partial boot. The logger
	// is closed LAST so the failure itself is recorded.
	fail := func(stage string, ferr error) (*App, error) {
		logger.Error("app", "application_start", "boot", "fatal",
			"boot failed at %s: %v", stage, ferr)

		cancel()
		_ = st.Close()
		_ = logger.Close()

		return nil, fmt.Errorf("app: %s: %w", stage, ferr)
	}

	if err := coreRegistry.Register(xray.New(), 0); err != nil {
		return fail("register xray", err)
	}

	if err := coreRegistry.Register(v2ray.New(), 1); err != nil {
		return fail("register v2ray", err)
	}

	if err := coreRegistry.Register(singbox.New(), 2); err != nil {
		return fail("register sing-box", err)
	}

	// Availability refresh runs in the background: the app must boot
	// instantly with zero cores installed.

	app := &App{
		opts:     opts,
		layout:   layout,
		logger:   logger,
		store:    st,
		pipe:     pipeline.New(opts.Pipeline, mreg),
		metricsR: mreg,
		sourceCache: cache.New("source", cache.Options{
			MaxEntries: 128,
			MaxBytes:   64 << 20,
			Weigh:      cache.ByteWeight,
			TTL:        6 * time.Hour,
		}),
		hotCache: cache.New("hot-configs", cache.Options{
			MaxEntries: 4096,
			TTL:        30 * time.Minute,
		}),
		coreLocator:  system.NewCoreLocator(layout.Cores),
		coreRegistry: coreRegistry,
		seenHashes:   make(map[string]string),
		state: AppState{
			Status:        "ready",
			Version:       version.Version,
			StartedAt:     time.Now().UTC().UnixMilli(),
			NativeAcceler: nativeMode(),
			Storage:       st.Snapshot(),
		},
	}

	app.tester = tester.New(tester.NewTCPProbe())

	if !opts.SkipDefaultSources {
		app.sources = source.DefaultSources()
	}

	app.connMgr = connection.New(connection.Options{
		Registry: coreRegistry,
		Metrics:  mreg,
	})

	app.ctx = ctx
	app.cancel = cancel

	if err := app.loadSources(); err != nil {
		return fail("load sources", err)
	}

	app.settings = app.loadSettings()
	app.applySettings(app.settings)

	logger.Info("app", "application_ready",
		"application ready (%d configurations)", st.Count())

	return app, nil
}

// Start begins background work: scheduler, verification, warm-up.
func (a *App) Start() {
	if a.started.Swap(true) {
		return
	}

	a.scheduler = scheduler.New(scheduler.Options{
		Interval:   a.opts.RefreshInterval,
		Jitter:     a.opts.RefreshJitter,
		RunOnStart: a.opts.RunIngestionOnStart,
	}, a.runIngestionCycle)

	a.scheduler.Start(a.ctx)

	// Core availability refresh is background work: the registry
	// stays usable (selection fails gracefully) while discovery is
	// still running.
	go func() {
		a.coreRegistry.Refresh(a.ctx)

		for _, backend := range a.coreRegistry.Backends() {
			if backend.Status == core.StatusAvailable {
				a.logger.Info("core", "core_discovered",
					"%s %s available at %s",
					backend.Name, backend.Version, backend.Path)
			}
		}
	}()

	// Staged startup: verify storage and warm caches in the
	// background so the UI is interactive immediately.
	go func() {
		if err := a.store.VerifyAll(a.ctx, nil); err != nil {
			a.mu.Lock()

			if a.state.Status == "ready" {
				a.state.Status = "degraded"
			}

			a.mu.Unlock()
		}
	}()

	go a.warmCaches()
}

// warmCaches preloads the most recent configurations into the hot
// cache so the first UI page renders without chunk reads.
func (a *App) warmCaches() {
	const warmTarget = 256

	count := 0

	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()

	_ = a.store.Iterate(ctx, func(key string, value []byte) error {
		if count >= warmTarget {
			return context.Canceled // stop iteration
		}

		var cfg config.Config

		if err := json.Unmarshal(value, &cfg); err == nil {
			a.hotCache.Put(key, cfg, 0)
			count++
		}

		return nil
	})
}

// Context returns the application lifecycle context. It is
// cancelled during Shutdown; background loops must select on
// Context().Done() so no goroutine outlives the application.
func (a *App) Context() context.Context {
	return a.ctx
}

// Shutdown coordinates a safe stop, in order:
//
//	stop scheduler (no new ingestion) → cancel background work →
//	disconnect the active session (stops the protocol core and
//	removes temporary runtime configs BEFORE the store closes) →
//	flush and close the store → close the runtime logger LAST.
//
// Idempotent: safe to call any number of times. No core process may
// outlive this call (the Windows guarantee: process first, temp-config
// file second, store third, logger last). No goroutine, file handle
// or WAL segment leaks.
func (a *App) Shutdown() {
	a.shutdownOnce.Do(func() {
		if a.logger != nil {
			a.logger.Info("app", "shutdown_start", "stopping subsystems")
		}

		if a.scheduler != nil {
			a.scheduler.Stop()
		}

		if a.cancel != nil {
			a.cancel()
		}

		a.mu.Lock()
		a.state.Status = "shutting_down"
		a.mu.Unlock()

		if a.connMgr != nil {
			a.connMgr.Shutdown()
		}

		if a.store != nil {
			if err := a.store.Close(); err != nil && a.logger != nil {
				a.logger.Error("store", "store_error", "close", "environment",
					"store close failed: %v", err)
			}
		}

		if a.logger != nil {
			a.logger.Info("app", "shutdown_complete", "all subsystems stopped")
			_ = a.logger.Close()
		}
	})
}

// State returns the current application state.
func (a *App) State() AppState {
	a.mu.RLock()
	defer a.mu.RUnlock()

	state := a.state
	state.IngestionRunning = a.ingesting.Load()
	state.ConfigCount = a.store.Count()
	state.Storage = a.store.Snapshot()

	return state
}

// runIngestionCycle executes one full source refresh. Safe to call
// concurrently: a cycle already running wins and the rest are skipped.
func (a *App) runIngestionCycle(ctx context.Context) error {
	if !a.ingesting.CompareAndSwap(false, true) {
		return nil // skip-if-busy
	}

	defer a.ingesting.Store(false)

	a.mu.RLock()

	sources := append([]source.Source(nil), a.sources...)
	hashes := make(map[string]string, len(a.seenHashes))

	for id, hash := range a.seenHashes {
		hashes[id] = hash
	}

	a.mu.RUnlock()

	sink := pipeline.NewStoreSink(a.store, 512)

	a.logger.Info("source", "source_refresh_start",
		"refreshing %d sources", len(sources))

	stats, newHashes, err := a.pipe.Run(ctx, sources, sink, hashes)

	// Merge: unchanged sources keep their previous hashes.
	a.mu.Lock()

	for id, hash := range newHashes {
		a.seenHashes[id] = hash
	}

	if stats != nil {
		statsCopy := *stats
		a.lastStats = &statsCopy
	}

	a.mu.Unlock()

	if saveErr := a.saveSources(); saveErr != nil && err == nil {
		err = saveErr
	}

	if err != nil {
		persisted := int64(0)

		if stats != nil {
			persisted = stats.Persisted
		}

		a.logger.Error("source", "source_refresh_error", "cycle", "pipeline",
			"refresh failed after %d persisted: %v", persisted, err)
	} else if stats != nil {
		a.logger.Info("source", "source_refresh_success",
			"refresh complete: %d discovered, %d persisted, %d duplicates",
			stats.Discovered, stats.Persisted, stats.Duplicates)
	}

	return err
}

// sourcesPath is the persisted source configuration file.
func (a *App) sourcesPath() string {
	return filepath.Join(a.layout.Config, "sources.json")
}

func (a *App) loadSources() error {
	raw, err := os.ReadFile(a.sourcesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first boot: defaults apply
		}

		return fmt.Errorf("app: read sources: %w", err)
	}

	var persisted persistedSources

	if err := json.Unmarshal(raw, &persisted); err != nil {
		// Unreadable source config must not brick the app; fall
		// back to defaults.
		return nil
	}

	if persisted.Version != sourcesFormatVersion {
		return nil
	}

	if len(persisted.Sources) > 0 {
		a.sources = persisted.Sources
	}

	if persisted.ContentHashes != nil {
		a.seenHashes = persisted.ContentHashes
	}

	return nil
}

func (a *App) saveSources() error {
	a.mu.RLock()

	payload := persistedSources{
		Version:       sourcesFormatVersion,
		Sources:       a.sources,
		ContentHashes: a.seenHashes,
	}

	a.mu.RUnlock()

	raw, err := json.MarshalIndent(&payload, "", "  ")
	if err != nil {
		return fmt.Errorf("app: encode sources: %w", err)
	}

	return system.WriteFileAtomic(a.sourcesPath(), raw, 0o600)
}

// nativeMode reports which native implementation is active.
func nativeMode() string {
	if native.Available() {
		return string(native.ModeNative)
	}

	return string(native.ModeGo)
}
