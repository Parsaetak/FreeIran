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

	"github.com/Parsaetak/FreeIran/engine/booster"
	"github.com/Parsaetak/FreeIran/engine/cache"
	"github.com/Parsaetak/FreeIran/engine/cleanup"
	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/engine/core/singbox"
	"github.com/Parsaetak/FreeIran/engine/core/v2ray"
	"github.com/Parsaetak/FreeIran/engine/core/xray"
	"github.com/Parsaetak/FreeIran/engine/coremgr"
	"github.com/Parsaetak/FreeIran/engine/metrics"
	"github.com/Parsaetak/FreeIran/engine/native"
	"github.com/Parsaetak/FreeIran/engine/pipeline"
	"github.com/Parsaetak/FreeIran/engine/scheduler"
	"github.com/Parsaetak/FreeIran/engine/source"
	"github.com/Parsaetak/FreeIran/engine/store"
	"github.com/Parsaetak/FreeIran/engine/tester"
	"github.com/Parsaetak/FreeIran/engine/testqueue"
	"github.com/Parsaetak/FreeIran/engine/tunnel"
	"github.com/Parsaetak/FreeIran/internal/logging"
	"github.com/Parsaetak/FreeIran/internal/version"
	"github.com/Parsaetak/FreeIran/system"

	"github.com/Parsaetak/FreeIran/engine/provider"

	"github.com/Parsaetak/FreeIran/internal/httpx"
	"github.com/Parsaetak/FreeIran/internal/statepub"
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
	//
	// Ownership: New owns the supplied logger for the whole
	// construction — on success the App closes it during Shutdown, on
	// failure the startup transaction closes it before returning. A
	// caller-side Close after either outcome is a safe idempotent
	// no-op (the double close is how the entrypoint keeps its own
	// early-failure paths simple).
	Logger *logging.Logger

	// SkipConnectVerification relaxes the connection manager's
	// Internet-verification gate. Production wiring NEVER sets this
	// (readiness is never success); tests and fake-core harnesses use
	// it because their staged cores expose no real tunnel.
	SkipConnectVerification bool

	// VerifyTarget overrides the tunnel-verification URL for the
	// connection manager (default: the standard 204 endpoint).
	// v0.9.8.4: test harnesses with a SOCKS-relay fake core point
	// this at a local HTTP target so the FULL verified path — real
	// request through the tunnel, state connected_verified — runs
	// without any public network.
	VerifyTarget string
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

	// v0.6 managed subsystems (lazy-initialized by the service layer).
	// initMu serializes the lazy init of coreMgr/testQueue/tunnelCtrl
	// so concurrent UI calls don't create duplicate managers (which
	// would leak the prior manager's worker goroutines). Regular
	// field reads (e.g. a.testQueue != nil checks) are safe without
	// initMu because the pointer is only ever written once (init or
	// SetMode), and SetMode acquires initMu too.
	initMu       sync.Mutex
	coreMgr      *coremgr.Manager
	testQueue    *testqueue.Queue
	testQueueCfg testqueue.Config
	tunnelCtrl   *tunnel.Controller

	// memory is the unified adaptive memory controller (Memory
	// Booster 2.0): pressure sampling + adaptive settings for the
	// queue, caches, store thresholds and ingestion knobs.
	memory *MemoryService

	// providerMgr is the unified provider manager (v0.9.8.1 §10):
	// xray/v2ray/sing-box adapters plus the first-class Tor and
	// Psiphon engines, one managed instance per provider.
	providerMgr   *provider.Manager
	torEngine     *provider.TorEngine
	psiphonEngine *provider.PsiphonEngine

	// providerEvidence tracks real provider session outcomes for the
	// Auto provider mode (§12): availability, health, verification,
	// latency, stability, recent success.
	providerEvidence *ProviderEvidence

	// lastBooster + boosterSettingsMu remember the most recently
	// applied adaptive settings so memory_policy_changed records
	// fire only on material changes (v0.9.7).
	boosterSettingsMu sync.Mutex
	lastBooster       booster.Settings

	// cleanups is the central cleanup coordinator (v0.9.2): one
	// place decides when reconstructable/replaceable data is
	// reclaimed, driven by memory pressure and the opportunistic
	// maintenance interval.
	cleanups *cleanup.Manager

	// recovery is the v0.9.3 automatic-recovery supervisor: when
	// the active connection fails it switches to the next viable
	// candidate with bounded retries, cooldowns and failure memory.
	recovery *RecoveryService

	// discovery is the v0.9.6 discovery service: the multi-level
	// node discovery engine, environment intelligence and the
	// adaptive START → DETECT → DISCOVER → TEST → RANK → CONNECT →
	// VERIFY flow. Lazily created by Discovery(); initMu serializes
	// the lazy init exactly like the other v0.6 subsystems.
	discovery *DiscoveryService

	// publicSources is the v0.9.7 bounded public-URL discovery
	// orchestrator (§7): GitHub adapter + generic HTTP connector
	// with SSRF/rate-limit/budget guardrails and a staging ledger
	// for discovered sources. Created with the app.
	publicSources *publicSourceDiscovery

	ctx    context.Context
	cancel context.CancelFunc

	// bootStart anchors the startup telemetry clock (first line of
	// New); bootTimings records phase → elapsed-ms transitions.
	bootStart   time.Time
	bootTimings map[string]int64

	// rankMu guards the ranked-candidate snapshot (rankingservice.go).
	rankMu   sync.Mutex
	rankSnap *rankSnapshot

	// v0.9.8.3: Quick Connect failure memory — recently failed
	// candidates cool down (decayed lazily) so one Quick Connect or
	// recovery loop never restarts the same dead candidate.
	qcMu       sync.Mutex
	qcFailures map[string]time.Time

	// v0.9.10: persistent favorites + user-defined groups
	// (collections.go — one versioned sidecar, stable IDs).
	collections collectionsState

	// v0.9.11: Connection Profiles (profiles.go — one versioned
	// sidecar, stable IDs, activation through the one settings path).
	profiles profilesState

	// v0.9.10: source-reliability report cache (sourcereliability.go)
	// + when the last ingestion cycle finished (evidence timestamp).
	reliability     reliabilityCache
	lastIngestionAt int64

	// cfgOrder caches the persisted manual configuration order
	// (configorderservice.go).
	cfgOrder configOrderState

	mu         sync.RWMutex
	state      AppState
	sources    []source.Source
	seenHashes map[string]string
	lastStats  *pipeline.Stats

	// sourcesFutureSchema (v0.9.12): sources.json was written by a
	// NEWER binary — set at load, checked by every save (§14: an
	// older binary must never silently rewrite a future schema).
	sourcesFutureSchema bool

	// settingsWrite serializes the settings.json read-mutate-write
	// cycle across ALL writers (SettingsService.Save and
	// persistSettings) so concurrent saves can never interleave
	// and leave memory and disk diverging (v0.9.12 §14).
	settingsWrite sync.Mutex

	// Event-driven UI synchronization. The application-state
	// publisher (internal/statepub) is created on the first
	// SetStateListener registration and stopped synchronously in
	// Shutdown; the connection stream is published by the
	// connection manager itself (engine/connection), and
	// SetConnectionListener only subscribes the UI bridge to it —
	// one publisher per stream, no stacked dispatch layers.
	// pubMu serializes registration/stop.
	pubMu              sync.Mutex
	statePub           *statepub.Publisher[AppState]
	connSubCancel      func()
	queueStateListener func(testqueue.LiveStateView)
	queueWatchCancel   func()

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
	Status  string `json:"status"`
	Version string `json:"version"`
	// Identity is the product identity line
	// ("FreeIran — A SHEYTAN Digital System", v0.9.6 §19).
	Identity         string          `json:"identity"`
	StartedAt        int64           `json:"started_at"`
	ConfigCount      int             `json:"config_count"`
	IngestionRunning bool            `json:"ingestion_running"`
	NativeAcceler    string          `json:"native_acceleration"`
	Storage          store.Stats     `json:"storage"`
	LastIngestion    *pipeline.Stats `json:"last_ingestion,omitempty"`

	// BootPhase is the unified startup phase (bootphase.go):
	// boot → workspace_ready → store_metadata_ready → services_ready →
	// ui_runtime_ready → ui_ready → background_warmup → ready.
	BootPhase string `json:"boot_phase"`

	// BootTimings is the phase → elapsed-ms startup telemetry table.
	BootTimings map[string]int64 `json:"boot_timings,omitempty"`
}

// bootTx is the startup transaction of app.New: it tracks EVERY
// resource the construction acquires — including a caller-injected
// logger, which New owns from the moment Options.Logger is received
// until New either succeeds (ownership transfers to App.Shutdown) or
// fails (the transaction closes it). One authoritative failure path
// releases exactly those resources once, in reverse acquisition order,
// no matter how early construction fails:
//
//	resource acquisition → startup transaction →
//	    success: App owns the resources
//	    failure: tx.fail cleans them
//
// Owned resources: the runtime logger, the global-logger registration
// (only when this boot installed it), the store, and the lifecycle
// cancellation. Temporary runtime/global manifests (runtime root,
// managed-process manifest path) carry no handle and are re-set by the
// next boot, so they need no rollback.
type bootTx struct {
	logger *logging.Logger
	// global records that logging.SetGlobal was called with
	// tx.logger — only then may a failure uninstall the global.
	global bool
	store  *store.Store
	cancel context.CancelFunc
}

// fail is the ONE authoritative boot-failure path. It records the
// fatal event, releases every resource acquired so far exactly once,
// uninstalls the global logger when THIS boot installed it (a closed
// logger must never stay reachable through logging.Global) and wraps
// the error for the caller.
func (tx *bootTx) fail(stage string, ferr error) (*App, error) {
	if tx.logger != nil {
		tx.logger.Error("app", "application_start", "boot", "fatal",
			"boot failed at %s: %v", stage, ferr)
	}

	if tx.cancel != nil {
		tx.cancel()
	}

	if tx.store != nil {
		_ = tx.store.Close()
	}

	if tx.global {
		logging.ClearGlobalIfCurrent(tx.logger)
	}

	if tx.logger != nil {
		_ = tx.logger.Close()
	}

	return nil, fmt.Errorf("app: %s: %w", stage, ferr)
}

// New boots the application to the READY state. Heavy verification
// and cache warming continue after Start.
func New(opts Options) (*App, error) {
	bootStart := time.Now()
	bootTimings := map[string]int64{}
	markPhase := func(phase string) {
		bootTimings[phase] = time.Since(bootStart).Milliseconds()
	}

	markPhase(BootBoot)

	// The startup transaction starts with whatever logger the caller
	// injected (nil in production when the entrypoint could not open
	// one). A supplied logger must not leak merely because failure
	// occurs before the normal logger-resolution point.
	tx := &bootTx{logger: opts.Logger}

	// v0.9.2 workspace model: an explicitly provided BaseDir (tests,
	// smoke test) is used as-is; otherwise the single Workspace Root
	// (default: the executable's directory, override: FREEIRAN_HOME)
	// is authoritative and the one-time legacy-location migration
	// runs before anything opens the store.
	defaultedWorkspace := opts.BaseDir == ""

	if defaultedWorkspace {
		opts.BaseDir = system.WorkspaceRoot()
	}

	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = time.Hour
	}

	var (
		layout system.DirNames
		err    error
	)

	// v0.9.13 fatal-boot ownership: app.New is the single producer
	// of boot-failure records. Failures before the app-level logger
	// is resolved still reach the caller-opened runtime log through
	// the injected logger (the entrypoint passes it in the desktop
	// path) — and the transaction closes that logger on the way out.
	if defaultedWorkspace {
		layout, err = system.EnsureWorkspace()
		if err != nil {
			return tx.fail("workspace init", err)
		}
	} else {
		layout, err = system.EnsureLayout(opts.BaseDir)
		if err != nil {
			return tx.fail("workspace init", err)
		}
	}

	markPhase(BootWorkspaceReady)

	// Persistent runtime log: opened BEFORE the store so storage and
	// migration failures are captured (§16).
	logger := opts.Logger

	if logger == nil {
		logger, err = logging.Open(logging.Options{
			Dir:  layout.Logs,
			Name: "freeiran.log",
		})
		if err != nil {
			// Nothing was acquired beyond the (nil) injected logger:
			// no fatal record is possible without a logger and there
			// is nothing to release.
			return nil, fmt.Errorf("app: open runtime log: %w", err)
		}

		tx.logger = logger
	}

	logging.SetGlobal(logger)
	tx.global = true

	// v0.9.7 lifecycle semantics: ONE application_start record per
	// launch. Workspace/base-path initialization is represented as
	// structured fields on this record — never as a second start
	// message.
	logger.Log(logging.Record{
		Level:      logging.LevelInfo,
		Subsystem:  "app",
		Event:      "application_start",
		Message:    fmt.Sprintf("FreeIran %s starting", version.String()),
		Status:     "starting",
		DurationMS: time.Since(bootStart).Milliseconds(),
		Fields: map[string]any{
			"version":             version.Version,
			"commit":              version.Commit,
			"base_dir":            opts.BaseDir,
			"defaulted_workspace": defaultedWorkspace,
			"session_id":          logger.SessionID(),
		},
	})

	// v0.9.7: workspace-ready is its own lifecycle stage (distinct
	// event name — no duplicate start messages).
	// v0.9.13: the base directory is already a structured field on
	// application_start, so the Normal profile no longer carries a
	// separate workspace_ready record; Detailed/Debug keep it.
	logger.DebugLifecycle("app", "workspace_ready", "workspace ready (%s)", opts.BaseDir)

	// v0.9.2 one-time legacy migration: when the defaulted workspace
	// is fresh, discover pre-0.9.2 per-user data and copy it in
	// (verified, source preserved, status recorded). Explicit base
	// directories (tests, smoke) never migrate.
	if defaultedWorkspace {
		report, err := system.EnsureWorkspaceMigrated(func(format string, args ...any) {
			logger.Info("workspace", "migration", format, args...)
		})
		if err != nil {
			logger.Error("workspace", "migration_error", "migrate", "environment",
				"legacy workspace migration failed: %v", err)

			return tx.fail("workspace migration", err)
		}

		if report.Performed {
			logger.Info("workspace", "migration_complete",
				"legacy data migrated from %s (%d files verified)",
				report.Source, report.Files)
		}
	}

	// v0.9.2: per-launch runtime-config directories (and the core
	// manager's validation/smoke folders) live under
	// <workspace>/runtime instead of the system temp root.
	core.SetRuntimeRoot(layout.Runtime)

	// Managed-process manifest: every supervised child process
	// (cores, providers) is recorded with its PID and executable
	// path so external cleanup (the Windows installer) can kill
	// exactly the processes FreeIran owns — never an unrelated
	// process that happens to share an image name.
	system.SetProcessManifestPath(filepath.Join(layout.Runtime, "managed-processes.txt"))

	// v0.10.1 crash-safe system-proxy restoration: while the
	// system proxy is FreeIran-owned, a durable marker in the
	// workspace records the PREVIOUS proxy state. A marker found
	// HERE proves the last session died without a clean Disable
	// (crash, kill, power loss) while it still owned the proxy —
	// the recorded state is restored before any service starts,
	// so the user never boots into a broken system proxy. The
	// marker is consumed on success and retried on the next boot
	// when the platform refuses the restore.
	tunnel.SetRecoveryMarkerPath(filepath.Join(layout.Runtime, "system-proxy.json"))

	if recovered, recoverErr := tunnel.RecoverStaleProxy(); recovered {
		if recoverErr == nil {
			logger.Warn("tunnel", "proxy_recovered",
				"restored the system-proxy state recorded before an unclean shutdown")
		} else {
			logger.Error("tunnel", "proxy_recovery_failed", "recover", "environment",
				"could not restore the system-proxy state after an unclean shutdown: %v",
				recoverErr)
		}
	}

	st, err := store.Open(store.Options{
		Path: layout.Data,
	})
	if err != nil {
		logger.Error("store", "store_error", "open", "environment",
			"store open failed: %v", err)

		return tx.fail("open store", err)
	}

	tx.store = st

	// v0.9.13: the record/chunk facts ride structured fields — the
	// message no longer duplicates them.
	storeSnapshot := st.Snapshot()

	logger.Log(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: "store",
		Event:     "store_open",
		Message:   "store ready",
		Status:    "ready",
		Fields: map[string]any{
			"records": st.Count(),
			"chunks":  storeSnapshot.ChunkCount,
		},
	})

	markPhase(BootStoreReady)

	mreg := metrics.New()

	ctx, cancel := context.WithCancel(context.Background())
	tx.cancel = cancel

	// v0.9.0: the Managed Core Manager boots with the app (not
	// lazily) because its install locations ARE discovery inputs:
	// a managed core lives in <cores>/<core>/bin/<core>.exe, and
	// the registry's locator must search those bin directories or
	// every successful install stays invisible to Connect/Backends
	// (the v0.8 integration gap). The manager is cheap to create:
	// it reads three small manifest files.
	manager, err := coremgr.New(coremgr.Options{
		RootDir:    layout.Cores,
		RuntimeDir: layout.Runtime,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("coremgr", "coremgr_error", "init", "environment",
			"core manager init failed: %v", err)

		return tx.fail("init core manager", err)
	}

	extraDirs := make([]string, 0, len(coremgr.AllCores))
	for _, name := range coremgr.AllCores {
		extraDirs = append(extraDirs, manager.BinDir(name))
	}

	locator := system.NewCoreLocator(layout.Cores, extraDirs...)

	// v0.9.14: ONE discovery authority shared app-wide. The same
	// locator drives the registry's availability view AND the core
	// manager's reuse-first install decisions, so no subsystem walks
	// PATH or installation directories twice and version probes are
	// cached/deduplicated across both consumers.
	manager.SetLocator(locator)

	// The core registry binds backend adapters to executable
	// discovery: xray (priority 0), v2ray (1), sing-box (2).
	coreRegistry := core.NewRegistry(locator)

	// fail closes every resource already created by New so the
	// caller never has to clean up after a partial boot. It is the
	// same transaction path the earlier failure stages use — one
	// authoritative cleanup, with the logger closed LAST so the
	// failure itself is recorded.
	fail := tx.fail

	if err := coreRegistry.Register(xray.New(), 0); err != nil {
		return fail("register xray", err)
	}

	if err := coreRegistry.Register(v2ray.New(), 1); err != nil {
		return fail("register v2ray", err)
	}

	if err := coreRegistry.Register(singbox.New(), 2); err != nil {
		return fail("register sing-box", err)
	}

	// v0.9.8.1: the unified provider manager binds the managed cores
	// (thin adapters over the SAME coremgr pipeline — no duplicate
	// install machinery) plus the first-class Tor and Psiphon engines.
	providerMgr := provider.NewManager()

	for _, adapter := range provider.NewCoreAdapters(manager) {
		providerMgr.Register(adapter)
	}

	httpClient := httpx.Default()

	torEngine := provider.NewTorEngine(layout.Providers, httpClient, true)
	torEngine.SetDiscovery(locator)
	psiphonEngine := provider.NewPsiphonEngine(layout.Providers, httpClient)
	psiphonEngine.SetDiscovery(locator)

	providerMgr.Register(torEngine)
	providerMgr.Register(psiphonEngine)

	// Availability refresh runs in the background: the app must boot
	// instantly with zero cores installed.

	app := &App{
		opts:        opts,
		layout:      layout,
		logger:      logger,
		bootStart:   bootStart,
		bootTimings: bootTimings,
		store:       st,
		qcFailures:  make(map[string]time.Time),
		pipe:        pipeline.New(opts.Pipeline, mreg),
		metricsR:    mreg,
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
		coreLocator:      locator,
		coreRegistry:     coreRegistry,
		coreMgr:          manager,
		providerMgr:      providerMgr,
		torEngine:        torEngine,
		psiphonEngine:    psiphonEngine,
		providerEvidence: newProviderEvidence(),
		seenHashes:       make(map[string]string),
		state: AppState{
			Status:        "ready",
			Version:       version.Version,
			Identity:      version.IdentityLine(),
			StartedAt:     time.Now().UTC().UnixMilli(),
			NativeAcceler: nativeMode(),
			Storage:       st.Snapshot(),
		},
	}

	// v0.9.0 testing model (§4/§5): the primary probe executes the
	// configuration through a real core and — when EndToEnd is on —
	// measures the actual round-trip through the generated tunnel
	// instead of fabricating one. The TCP reachability probe is the
	// fallback for protocol classes with no available core.
	coreProbe := tester.NewCoreProbe(coreRegistry)
	coreProbe.Fallback = true
	coreProbe.EndToEnd = true

	app.tester = tester.New(tester.NewChainedProbe(coreProbe, tester.NewTCPProbe()))

	// Memory Booster 2.0: the adaptive controller observes the store,
	// caches and (once created) the test queue; Start() launches its
	// sampling goroutine.
	app.memory = newMemoryService(app)

	// v0.9.7: bounded public-source discovery (GitHub + generic HTTP
	// connectors, staging ledger, rate-limit accounting).
	app.publicSources = newPublicSourceDiscovery(app)

	// v0.9.2 central cleanup coordinator: registers the bounded
	// reclamation tasks and starts with the app's lifecycle.
	app.registerCleanupTasks()

	if !opts.SkipDefaultSources {
		app.sources = source.DefaultSources()
	}

	app.connMgr = connection.New(connection.Options{
		Registry: coreRegistry,
		Metrics:  mreg,
		Verify: connection.VerifyPolicy{
			Skip:   opts.SkipConnectVerification,
			Target: opts.VerifyTarget,
		},
	})

	// v0.9.3 autonomous connection engine: the recovery supervisor
	// watches the connection state machine and re-selects the best
	// viable candidate on failure (bounded, never a reconnect loop).
	app.recovery = NewRecoveryService(app)

	app.ctx = ctx
	app.cancel = cancel

	if err := app.loadSources(); err != nil {
		return fail("load sources", err)
	}

	app.settings = app.loadSettings()
	app.applySettings(app.settings)

	// v0.9.11 Connection Profiles: load the sidecar and apply the
	// startup (default) profile in memory — through the one settings
	// path, before any service can read the settings snapshot.
	app.initProfiles()

	logger.Log(logging.Record{
		Level:      logging.LevelInfo,
		Subsystem:  "app",
		Event:      "application_ready",
		Message:    fmt.Sprintf("application ready (%d configurations)", st.Count()),
		Status:     "ready",
		DurationMS: time.Since(bootStart).Milliseconds(),
		Fields: map[string]any{
			"config_count": st.Count(),
		},
	})

	app.markBoot(BootServicesReady)

	return app, nil
}

// Startup priority model (v0.9.9 §12): background work is staged so
// it cannot unnecessarily compete with the first interactive
// connection.
//
//	Priority 0  interactive connect/disconnect/reconnect + process teardown
//	Priority 1  UI state publication + readiness-critical discovery
//	            (the core-registry refresh — Connect cannot select a
//	            backend before it has run, so it stays immediate)
//	Priority 2  verification and recovery (bounded already)
//	Priority 3  cache warming, storage verification, ingestion,
//	            cleanup — heavy I/O deferred behind backgroundWarmupDelay
//	            and cancelled with the app lifecycle
const backgroundWarmupDelay = 3 * time.Second

// Start begins background work: scheduler, verification, warm-up —
// staged by the priority model above.
func (a *App) Start() {
	if a.started.Swap(true) {
		return
	}

	// Memory Booster 2.0 begins sampling before any workload starts,
	// so pressure reactions apply from the first ingestion cycle.
	// (Sampling itself is cheap — an occasional runtime.ReadMemStats —
	// and stays at priority 1.)
	if a.memory != nil {
		a.memory.Start()
	}

	a.scheduler = scheduler.New(scheduler.Options{
		Interval:     a.opts.RefreshInterval,
		Jitter:       a.opts.RefreshJitter,
		RunOnStart:   a.opts.RunIngestionOnStart,
		InitialDelay: backgroundWarmupDelay,
	}, a.runIngestionCycle)

	a.scheduler.Start(a.ctx)

	// v0.9.2: opportunistic cleanup cadence (bounded, cancellable).
	// Priority 3: the cadence is opportunistic by design; the first
	// pass additionally waits out the warmup window.
	go a.cleanupLoop()

	// v0.9.3: automatic recovery watch (bounded attempts, cooldowns,
	// failure memory; user opt-out via settings). Priority 2.
	if a.recovery != nil {
		a.recovery.Start()
	}

	// Priority 1: core availability refresh runs IMMEDIATELY (cheap:
	// it reads the core manifests) because interactive connection
	// depends on it — the registry stays usable (selection fails
	// gracefully) while discovery is still running.
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

	// Priority 3, deferred: storage verification (full-chunk I/O) and
	// cache warming (store scan) must not compete with the first
	// interactive connection for disk and CPU. Both remain fully
	// cancellable (app lifecycle ctx).
	a.markBoot(BootBackgroundWarm)
	go func() {
		if !a.waitForWarmupWindow() {
			return
		}

		if err := a.store.VerifyAll(a.ctx, nil); err != nil {
			a.mu.Lock()

			if a.state.Status == "ready" {
				a.state.Status = "degraded"
			}

			a.mu.Unlock()

			// The degraded transition reaches the UI as a real
			// event, never delayed behind a poll.
			a.publishState()
		}
	}()

	go func() {
		if !a.waitForWarmupWindow() {
			return
		}

		a.warmCaches()
	}()
}

// waitForWarmupWindow parks a priority-3 goroutine behind the startup
// warmup delay (or until the app lifecycle ends first — in which case
// the work is never started at all).
func (a *App) waitForWarmupWindow() bool {
	timer := time.NewTimer(backgroundWarmupDelay)
	defer timer.Stop()

	select {
	case <-a.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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

	// Warm-up finished: the last startup phase. The full phase
	// timing table stays available to Detailed/Debug logs and to
	// diagnostics via AppState.BootTimings; Normal gets one compact
	// measured summary (v0.9.13).
	a.markBoot(BootReady)
	a.logBootTelemetry()
	a.logWarmupComplete()
}

// Context returns the application lifecycle context. It is
// cancelled during Shutdown; background loops must select on
// Context().Done() so no goroutine outlives the application.
func (a *App) Context() context.Context {
	return a.ctx
}

// CoreManager returns the Managed Core Manager. It is created during
// New and never nil for a successfully booted app.
func (a *App) CoreManager() *coremgr.Manager {
	return a.coreMgr
}

// Discovery returns the v0.9.6 discovery service (multi-level node
// discovery, environment intelligence, adaptive start flow). The
// service is created lazily on first use and is a singleton for the
// application lifetime.
func (a *App) Discovery() *DiscoveryService {
	a.initMu.Lock()
	defer a.initMu.Unlock()

	if a.discovery == nil {
		a.discovery = NewDiscoveryService(a)
	}

	return a.discovery
}

// RefreshCores re-runs protocol-core discovery against the locator
// (which includes the managed bin directories). Call it after any
// install/update/remove so Connect, Backends and the tester observe
// the change immediately.
func (a *App) RefreshCores() {
	if a.coreRegistry == nil {
		return
	}

	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()

	a.coreRegistry.Refresh(ctx)

	for _, backend := range a.coreRegistry.Backends() {
		// v0.9.7 fix: only AVAILABLE cores are "discovered" — the
		// previous unfiltered loop logged "xray  available at " for
		// missing/invalid backends on every refresh.
		if backend.Status != core.StatusAvailable {
			continue
		}

		a.logger.Log(logging.Record{
			Level:     logging.LevelInfo,
			Subsystem: "core",
			Event:     "core_discovered",
			Message:   fmt.Sprintf("%s %s available at %s", backend.Name, backend.Version, backend.Path),
			Core:      backend.Name,
			Status:    "available",
			Fields: map[string]any{
				"version": backend.Version,
				"path":    backend.Path,
			},
		})
	}
}

// SetCoreProgressListener registers the install-progress sink the
// desktop entrypoint forwards to the UI as Wails events. The engine
// itself never imports the Wails runtime. Passing nil removes it.
func (a *App) SetCoreProgressListener(fn func(coremgr.InstallProgress)) {
	coremgr.OnProgress(fn)
}

// Shutdown coordinates a safe stop, in order:
//
//	stop scheduler (no new ingestion) → cancel background work →
//	disconnect the active session (stops the protocol core and
//	removes temporary runtime configs BEFORE the store closes) →
//	stop the test queue (no new tests start; in-flight tests finish
//	or are cancelled) → disable the tunnel mode (restores the
//	previous system proxy; tears down TUN) →
//	flush and close the store → close the runtime logger LAST and
//	uninstall it from the process-global logger slot.
//
// Idempotent: safe to call any number of times. No core process may
// outlive this call (the Windows guarantee: process first, temp-config
// file second, store third, logger last). No goroutine, file handle
// or WAL segment leaks. The tunnel restoration is best-effort: if the
// system was already direct, no work is done; if WinINet fails to
// restore (rare), the user is notified via the log.
func (a *App) Shutdown() {
	a.shutdownOnce.Do(func() {
		if a.logger != nil {
			a.logger.Info("app", "application_shutdown", "stopping subsystems")
		}

		// The memory controller stops first: it observes the very
		// subsystems being torn down below.
		if a.memory != nil {
			a.memory.Stop()
		}

		// The recovery supervisor must not race the connection
		// manager's shutdown: stop watching before teardown.
		if a.recovery != nil {
			a.recovery.Stop()
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

		// The shutting-down transition is published before
		// the subsystem teardown starts.
		a.publishState()

		// v0.9.8.1: provider engines stop deterministically before the
		// connection manager (their endpoints feed active sessions).
		if a.providerMgr != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			a.providerMgr.StopAll(stopCtx)
			stopCancel()
		}

		if a.connMgr != nil {
			a.connMgr.Shutdown()
		}

		// The connection manager has joined its own publisher
		// (final disconnected snapshot delivered); now stop the
		// UI wiring synchronously — the connection subscription
		// is cancelled and the state publisher stopped after
		// draining, so no emit callback survives this point and
		// nothing fires into a closing UI runtime.
		a.stopPublishers()

		// Snapshot the lazy-init subsystems under initMu so
		// we stop the exact queue/tunnel that was initialized,
		// not nil. A concurrent ensureQueue/ensureController
		// that hasn't taken initMu yet will see the cancelled
		// ctx and refuse to init.
		a.initMu.Lock()
		tq := a.testQueue
		tc := a.tunnelCtrl
		a.initMu.Unlock()

		if tq != nil {
			tq.Stop()
		}

		if tc != nil {
			// Restore previous system-proxy state. Best-effort:
			// a failure here does not block the rest of shutdown.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := tc.Disable(ctx); err != nil && a.logger != nil {
				a.logger.Warn("tunnel", "shutdown_restore",
					"could not restore tunnel state on shutdown: %v", err)
			}
			cancel()
		}

		if a.store != nil {
			if err := a.store.Close(); err != nil && a.logger != nil {
				a.logger.Error("store", "store_error", "close", "environment",
					"store close failed: %v", err)
			}
		}

		if a.logger != nil {
			// v0.9.7: aggregate counters (job fallbacks, etc.) close out
			// the session as compact summaries instead of per-event spam.
			system.FlushFallbackSummary()

			a.logger.Log(logging.Record{
				Level:      logging.LevelInfo,
				Subsystem:  "app",
				Event:      "application_shutdown",
				Message:    "all subsystems stopped",
				Status:     "shutdown",
				DurationMS: time.Since(a.bootStart).Milliseconds(),
			})

			_ = a.logger.Close()

			// The closed logger must not stay reachable through
			// logging.Global: uninstall it when it is still the
			// installed one (an owner that swapped in a different
			// global meanwhile is never clobbered).
			logging.ClearGlobalIfCurrent(a.logger)
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

// MarkUIRuntimeReady records the ui_runtime_ready phase: the
// entrypoint calls it once the engine services are bound to the
// frontend runtime. Safe to call multiple times (idempotent).
func (a *App) MarkUIRuntimeReady() {
	a.markBoot(BootUIRuntimeReady)
}

// MarkUIReady records the ui_ready phase: the frontend reported
// its first usable frame. Safe to call multiple times; a late
// call can never regress an already-recorded READY state.
func (a *App) MarkUIReady() {
	a.markBoot(BootUIReady)
}

// runIngestionCycle executes one full source refresh. Safe to call
// concurrently: a cycle already running wins and the rest are skipped.
func (a *App) runIngestionCycle(ctx context.Context) error {
	if !a.ingesting.CompareAndSwap(false, true) {
		return nil // skip-if-busy
	}

	// Ingestion start/finish are meaningful state changes —
	// published from the transition path instead of the next tick.
	a.publishState()

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

	// The ingestion cycle changed the candidate pool: drop the
	// ranking snapshot (also covers the failed-cycle case: any
	// persisted subset still changed the inputs).
	a.InvalidateRankingSnapshot()

	// Merge: unchanged sources keep their previous hashes.
	a.mu.Lock()

	for id, hash := range newHashes {
		a.seenHashes[id] = hash
	}

	if stats != nil {
		statsCopy := *stats
		a.lastStats = &statsCopy
	}

	// v0.9.10: the ingestion cycle produced fresh evidence — the
	// source-reliability dashboard's cache is stale from here on.
	a.lastIngestionAt = time.Now().UTC().UnixMilli()

	a.mu.Unlock()

	a.invalidateReliability()

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

	// Ingestion finished (or failed) — publish the final
	// state (ingestion_running=false + fresh storage/ingestion
	// stats) as a real event.
	a.publishState()

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

	if persisted.Version > sourcesFormatVersion {
		// FUTURE SCHEMA (v0.9.12 §14): run with defaults, but arm
		// the save guard — an older binary must never silently
		// rewrite a future schema. The on-disk document stays
		// untouched for the newer version.
		a.sourcesFutureSchema = true

		return nil
	}

	if persisted.Version != sourcesFormatVersion {
		return nil
	}

	if len(persisted.Sources) > 0 {
		a.sources = normalizePersistedSources(persisted.Sources)
	}

	if persisted.ContentHashes != nil {
		a.seenHashes = persisted.ContentHashes
	}

	return nil
}

// legacySourceURLs maps known legacy endpoint forms of mandated
// sources to their canonical raw URLs. The ScrapeAndCategorize
// registry entry moved from the /main/ shorthand to the fully
// qualified /refs/heads/main/ form; both resolve to the same content,
// so the migration is purely cosmetic — but the persisted registry
// must converge on the canonical form exactly once.
var legacySourceURLs = map[string]string{
	"https://raw.githubusercontent.com/10ium/ScrapeAndCategorize/main/output_configs/Netherlands.txt": "https://raw.githubusercontent.com/10ium/ScrapeAndCategorize/refs/heads/main/output_configs/Netherlands.txt",
}

// normalizePersistedSources upgrades legacy source URLs to their
// canonical form and drops EXACT duplicate URLs (same endpoint
// registered under two IDs), keeping the first (default) entry.
func normalizePersistedSources(sources []source.Source) []source.Source {
	seen := make(map[string]int, len(sources))

	out := make([]source.Source, 0, len(sources))

	for i, src := range sources {
		if canonical, ok := legacySourceURLs[src.URL]; ok {
			src.URL = canonical
			sources[i].URL = canonical
		}

		if first, dup := seen[src.URL]; dup {
			// Duplicate endpoint: keep the first registration only.
			if !sources[first].Custom && src.Custom {
				// Prefer keeping a custom entry over a default one.
				sources[first] = src
			}

			continue
		}

		seen[src.URL] = i

		out = append(out, src)
	}

	return out
}

func (a *App) saveSources() error {
	if a.sourcesFutureSchema {
		// v0.9.12 §14: never silently rewrite a future schema with an
		// older binary — the on-disk document stays intact and the
		// caller's write fails loudly.
		return fmt.Errorf("app: sources sidecar uses a newer schema; refusing to overwrite (run the newer version first)")
	}

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
