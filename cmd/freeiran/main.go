// Command freeiran is the FreeIran desktop application entrypoint
// built on Wails v3.
//
// The binary composes the engine services (engine/app) with a native
// webview window serving the TypeScript frontend. The UI communicates
// exclusively through generated Wails bindings and events — there is
// no localhost HTTP API.
//
// Runtime logging: a persistent structured log
// (<AppData>/FreeIran/logs/freeiran.log) is opened before anything
// else so boot failures are captured; it is closed last after every
// subsystem has flushed.
package main

import (
	"embed"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/Parsaetak/FreeIran/engine/app"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/coremgr"
	"github.com/Parsaetak/FreeIran/internal/appicon"
	"github.com/Parsaetak/FreeIran/internal/logging"
	"github.com/Parsaetak/FreeIran/internal/statepub"
	"github.com/Parsaetak/FreeIran/internal/version"
	"github.com/Parsaetak/FreeIran/system"
)

// assets embeds the built frontend. The dist directory is produced by
// the frontend build (npm run build) before compiling this binary.
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	// Headless smoke mode (CI): boot the full engine, verify the
	// service surface, shut down cleanly and exit — the webview is
	// never launched. Guards the §58 startup → store → shutdown path.
	if len(os.Args) > 1 && os.Args[1] == "--smoke-test" {
		os.Exit(smokeTest())
	}

	slog.Info("FreeIran starting", "version", version.String())

	// v0.9.2 workspace model: one root (the executable's directory,
	// override FREEIRAN_HOME) holds config, data, cache, logs, cores
	// and runtime state. The persistent runtime log is opened first
	// so boot failures are captured; it is closed last.
	logger, err := logging.Open(logging.Options{
		Dir:          system.WorkspaceLayout().Logs,
		Name:         "freeiran.log",
		MirrorStderr: true,
	})
	if err != nil {
		slog.Error("runtime log unavailable; continuing without it", "error", err)

		logger = nil
	}

	logging.SetGlobal(logger)

	if logger != nil {
		logger.Info("app", "application_start",
			"FreeIran %s starting (workspace %s)", version.String(), system.WorkspaceRoot())
	}

	// BaseDir is intentionally NOT set: app.New resolves the single
	// Workspace Root itself and runs the one-time legacy migration
	// when the workspace is fresh.
	applicationInstance, err := app.New(app.Options{
		Logger: logger,
	})
	if err != nil {
		if logger != nil {
			logger.Error("app", "application_start", "boot", "fatal",
				"boot failed: %v", err)
			_ = logger.Close()
		}

		// v0.9.2: a GUI-subsystem executable has no console, so a
		// plain log.Fatalf would die silently for the user. Surface
		// the failure natively and leave a readable artifact.
		reportBootFailure(err)
	}

	wailsApp := application.New(application.Options{
		Name:        "FreeIran",
		Description: "Free, open-source VPN configuration manager",
		Services: []application.Service{
			application.NewService(app.NewAppService(applicationInstance)),
			application.NewService(app.NewSourceService(applicationInstance)),
			application.NewService(app.NewDataService(applicationInstance)),
			application.NewService(app.NewStorageService(applicationInstance)),
			application.NewService(app.NewDiagnosticsService(applicationInstance)),
			application.NewService(app.NewConnectionService(applicationInstance)),
			application.NewService(app.NewLogService(applicationInstance)),
			application.NewService(app.NewSettingsService(applicationInstance)),
			// v0.6 service surface that v0.7 shipped unbound: the
			// methods existed but were never registered, so no UI
			// action could reach them. v0.8 closes the wiring gap.
			application.NewService(app.NewCoreService(applicationInstance)),
			application.NewService(app.NewTestQueueService(applicationInstance)),
			application.NewService(app.NewTunnelService(applicationInstance)),
			// v0.9.0: manual Internet / Network Diagnostics (§3).
			application.NewService(app.NewNetworkService(applicationInstance)),
			// v0.9.6: discovery engine, environment intelligence and
			// the adaptive start flow (§5/§15).
			application.NewService(app.NewDiscoveryService(applicationInstance)),
			// v0.9.8.1: first-class provider surface (§12/§13) and the
			// shared Internet-Tools engine (§6).
			application.NewService(app.NewProviderService(applicationInstance)),
			application.NewService(app.NewInternetToolsService(applicationInstance)),
		},
		Assets: application.AssetOptions{
			Handler: versionedAssetCache(application.BundledAssetFileServer(assets)),
		},
	})

	// Event-driven UI synchronization — the authoritative
	// transition paths publish through the statepub publishers
	// (deduplicated, ordered, zero-delay; see internal/statepub);
	// these callbacks are the only bridge to the UI runtime.
	// Registration happens BEFORE Start() so no transition is
	// missed, and each registration immediately publishes the
	// current snapshot for initial convergence.
	//
	// v0.9.9 delivery boundary: the callbacks hand snapshots to
	// bounded emitters instead of emitting inline on the publisher's
	// delivery goroutine. A slow or stalled webview event pipeline
	// can no longer stall the engine's state publication path — the
	// emitter coalesces to the newest snapshot under saturation
	// (full-state semantics: newest supersedes; the UI converges on
	// resume) and the engine keeps publishing. No polling, no
	// unbounded buffering.
	stateEmitter := statepub.NewBoundedEmitter("ui-state",
		func(state app.AppState) { wailsApp.Event.Emit("freeiran:state", state) })

	connEmitter := statepub.NewBoundedEmitter("ui-connection",
		func(snapshot connection.Snapshot) {
			wailsApp.Event.Emit("freeiran:connection", snapshot)
		})

	// The emitters stop when main returns (after wailsApp.Run() has
	// finished and the OnShutdown hook has torn the engine down):
	// draining/joining them guarantees no emit callback outlives the
	// process's UI runtime and no pump goroutine survives.
	defer stateEmitter.Stop()
	defer connEmitter.Stop()

	applicationInstance.SetStateListener(stateEmitter.Submit)

	applicationInstance.SetConnectionListener(connEmitter.Submit)

	// Background work starts after the publishers are wired: every
	// state change it produces is observed event-driven.
	applicationInstance.Start()

	// v0.9.0: forward Managed Core Manager install progress to the
	// UI as events — the one-click Install path reports download,
	// verification and smoke-test stages live (§9).
	// v0.9.9: install progress rides the same bounded boundary
	// (monotonic progress; the newest snapshot is the informative one).
	progressEmitter := statepub.NewBoundedEmitter("ui-coreprogress",
		func(progress coremgr.InstallProgress) {
			wailsApp.Event.Emit("freeiran:coreprogress", progress)
		})

	defer progressEmitter.Stop()

	applicationInstance.SetCoreProgressListener(progressEmitter.Submit)

	// v0.9.6: forward adaptive start-flow progress (detect → discover →
	// test → rank → connect → verify) to the UI as events — real stage
	// transitions with measured durations, never a fake animation.
	applicationInstance.Discovery().SetFlowListener(func(ev app.StartFlowEvent) {
		wailsApp.Event.Emit("freeiran:startflow", ev)
	})

	// Startup lifecycle (v0.9.4): the engine services are bound to
	// the frontend runtime — record ui_runtime_ready. The frontend
	// emits freeiran:ui-ready once its first frame is interactive;
	// that event records ui_ready. Nothing here blocks the UI: the
	// markers are pure telemetry writes.
	applicationInstance.MarkUIRuntimeReady()

	cancelUIReady := wailsApp.Event.On("freeiran:ui-ready",
		func(*application.CustomEvent) {
			applicationInstance.MarkUIReady()
		})
	defer cancelUIReady()

	// WindowCentered is the default start position; no override needed.
	//
	// Icon (v0.9.1): Linux gets the embedded PNG as the GTK window
	// icon. On Windows the titlebar/taskbar/executable identity
	// comes from the linked resource (cmd/freeiran/*.syso, built
	// from assets/freeiran-icon.ico), so no runtime bytes are
	// needed there.
	wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "FreeIran",
		Width:     1280,
		Height:    800,
		MinWidth:  960,
		MinHeight: 600,
		Linux: application.LinuxWindow{
			Icon: appicon.PNG,
		},
	})

	// Graceful shutdown: the OnShutdown hook runs before process
	// exit and must not lose data. App.Shutdown flushes and closes
	// the store and (last of all) the runtime log.
	wailsApp.OnShutdown(func() {
		applicationInstance.Shutdown()
	})

	if err := wailsApp.Run(); err != nil {
		if logger != nil {
			logger.Error("app", "shutdown_error", "run", "fatal",
				"webview run failed: %v", err)
		}

		log.Fatalf("run failed: %v", err)
	}
}

// versionedAssetCache wraps the bundled asset server with the cache
// policy the STABLE asset filenames require. The embed tree uses
// fixed logical names (assets/index.js, assets/index.css,
// assets/export-worker.js) that are replaced IN PLACE on every
// release — so the webview must REVALIDATE instead of trusting a
// cached response across application upgrades. "no-cache"
// (revalidate before use) does exactly that: every launch re-reads
// the assets from the in-process embedded filesystem (memory-speed,
// no network), and an upgraded binary can never serve stale
// JavaScript or CSS. Hashed filenames remain forbidden — this policy
// is the reason they can stay forbidden safely.
func versionedAssetCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// reportBootFailure surfaces a fatal boot error to a GUI user: a
// native (console-free) message dialog plus a boot-error file next to
// the executable — or the system temp directory when the workspace is
// read-only — because a windowsgui binary has no console to print to.
func reportBootFailure(err error) {
	message := fmt.Sprintf("FreeIran failed to start:\n\n%v", err)

	fix := "\n\nPossible fixes:\n" +
		"  • Move FreeIran to a folder that is writable\n" +
		"  • Or set the FREEIRAN_HOME environment variable to a writable path"

	target := filepath.Join(system.WorkspaceRoot(), "boot-error.txt")

	if writeErr := os.WriteFile(target, []byte(message+fix+"\n"), 0o600); writeErr != nil {
		fallback := filepath.Join(os.TempDir(), "freeiran-boot-error.txt")
		_ = os.WriteFile(fallback, []byte(message+fix+"\n"), 0o600)
	}

	wailsApp := application.New(application.Options{Name: "FreeIran"})

	dialog := wailsApp.Dialog.Error()
	dialog.SetTitle("FreeIran cannot start")
	dialog.SetMessage(message + fix)
	dialog.Show()

	log.Fatalf("boot failed: %v", err)
}

// smokeTest exercises the headless application lifecycle:
//
//	open the runtime log → boot the engine → verify the service
//	surface → shut down cleanly → exit 0.
//
// The smoke test is fully DETERMINISTIC:
//   - no network refresh (RunIngestionOnStart=false);
//   - no public-source download (SkipDefaultSources=true);
//   - no scheduler activity (Start is never called);
//   - no real VPN core required (the registry boots with zero cores
//     available and the smoke test never connects);
//   - the base directory is a process-local temp root removed at the
//     end so the smoke test never touches the user's AppData.
//
// Any failure prints a readable diagnostic and exits non-zero.
func smokeTest() int {
	// Isolated base directory: the smoke test must not read or write
	// the user's real FreeIran data, and the directory must be
	// removable on every platform after Shutdown releases every
	// handle (the Windows lifecycle guarantee).
	baseDir := filepath.Join(os.TempDir(),
		fmt.Sprintf("freeiran-smoke-%d", time.Now().UnixNano()))

	defer func() {
		_ = os.RemoveAll(baseDir)
	}()

	layout := system.Layout(baseDir)

	logger, err := logging.Open(logging.Options{
		Dir:          layout.Logs,
		Name:         "freeiran.log",
		MirrorStderr: true,
	})
	if err != nil {
		fmt.Printf("smoke: runtime log unavailable: %v\n", err)

		logger = nil
	}

	logging.SetGlobal(logger)

	if logger != nil {
		logger.Info("app", "application_start",
			"smoke test starting (%s)", version.String())
	}

	applicationInstance, err := app.New(app.Options{
		BaseDir:             baseDir,
		Logger:              logger,
		RunIngestionOnStart: false,
		SkipDefaultSources:  true,
	})
	if err != nil {
		if logger != nil {
			logger.Error("app", "application_start", "boot", "fatal",
				"smoke boot failed: %v", err)
			_ = logger.Close()
		}

		fmt.Printf("smoke: boot failed: %v\n", err)

		return 1
	}

	state := applicationInstance.State()
	if state.Status != "ready" {
		fmt.Printf("smoke: unexpected boot status %q\n", state.Status)

		applicationInstance.Shutdown()

		if logger != nil {
			_ = logger.Close()
		}

		return 1
	}

	if state.Version == "" {
		fmt.Println("smoke: version missing from state")

		applicationInstance.Shutdown()

		if logger != nil {
			_ = logger.Close()
		}

		return 1
	}

	// Storage service probe: chunked-store stats must be readable and
	// the store must report zero records for a fresh base directory.
	storage := app.NewStorageService(applicationInstance)
	stats := storage.Stats()

	if stats.Count != 0 {
		fmt.Printf("smoke: fresh store count = %d, want 0\n", stats.Count)

		applicationInstance.Shutdown()

		if logger != nil {
			_ = logger.Close()
		}

		return 1
	}

	// Log service probe: the runtime logger must expose at least the
	// startup entries written above.
	logSvc := app.NewLogService(applicationInstance)
	recent := logSvc.Recent(app.LogFilter{Limit: 100})

	if len(recent.Entries) == 0 {
		fmt.Println("smoke: runtime log is empty after boot")

		applicationInstance.Shutdown()

		if logger != nil {
			_ = logger.Close()
		}

		return 1
	}

	if logger != nil {
		logger.Info("app", "application_ready",
			"smoke test state verified (storage count=%d, log entries=%d)",
			stats.Count, len(recent.Entries))
	}

	// Shutdown WITHOUT calling Start: no scheduler, no background
	// ingestion, no core refresh — the smoke test verifies the clean
	// startup→shutdown path only.
	applicationInstance.Shutdown()

	if logger != nil {
		_ = logger.Close()
	}

	fmt.Println("smoke: OK (boot, state, storage, log, shutdown)")

	return 0
}
