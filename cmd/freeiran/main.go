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
	"os"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/Parsaetak/FreeIran/engine/app"
	"github.com/Parsaetak/FreeIran/internal/logging"
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

	// Open the persistent runtime log first: every later failure
	// (boot, store, migration, core) is captured with full context.
	// Mirroring to stderr keeps development runs observable too.
	logger, err := logging.Open(logging.Options{
		Dir:          system.Layout(system.DefaultBaseDir()).Logs,
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
			"FreeIran %s starting", version.String())
	}

	applicationInstance, err := app.New(app.Options{
		BaseDir: system.DefaultBaseDir(),
		Logger:  logger,
	})
	if err != nil {
		if logger != nil {
			logger.Error("app", "application_start", "boot", "fatal",
				"boot failed: %v", err)
			_ = logger.Close()
		}

		log.Fatalf("boot failed: %v", err)
	}

	applicationInstance.Start()

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
		},
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(assets),
		},
	})

	// Push state transitions to the UI so it never needs polling.
	startStateBroadcaster(wailsApp, applicationInstance)

	// Push connection state machine transitions (core identity,
	// lifecycle state, latency) on the same event-driven model.
	startConnectionBroadcaster(wailsApp, applicationInstance)

	// WindowCentered is the default start position; no override needed.
	wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "FreeIran",
		Width:     1280,
		Height:    800,
		MinWidth:  960,
		MinHeight: 600,
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

// smokeTest exercises the headless application lifecycle:
//
//	open the runtime log → boot the engine → verify the service
//	surface → shut down cleanly → exit 0.
//
// Any failure prints a readable diagnostic and exits non-zero.
func smokeTest() int {
	logger, err := logging.Open(logging.Options{
		Dir:          system.Layout(system.DefaultBaseDir()).Logs,
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
		BaseDir: system.DefaultBaseDir(),
		Logger:  logger,
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

		return 1
	}

	if state.Version == "" {
		fmt.Println("smoke: version missing from state")

		applicationInstance.Shutdown()

		return 1
	}

	// Service surface probe: storage stats must be readable.
	storage := app.NewStorageService(applicationInstance)
	_ = storage.Stats()

	applicationInstance.Start()

	if logger != nil {
		logger.Info("app", "application_ready", "smoke test state verified")
	}

	applicationInstance.Shutdown()

	if logger != nil {
		_ = logger.Close()
	}

	fmt.Println("smoke: OK (boot, state, services, shutdown)")

	return 0
}

// startConnectionBroadcaster emits the connection state machine
// snapshot periodically. The loop terminates with the application
// context; like the state broadcaster it pushes instead of letting
// the UI poll.
func startConnectionBroadcaster(
	wailsApp *application.App,
	applicationInstance *app.App,
) {
	broadcast := func() {
		wailsApp.Event.Emit("freeiran:connection",
			app.NewConnectionService(applicationInstance).ConnectionState())
	}

	go func() {
		time.Sleep(700 * time.Millisecond)
		broadcast()
	}()

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		ctx := applicationInstance.Context()

		for {
			select {
			case <-ticker.C:
				broadcast()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// startStateBroadcaster emits the application state periodically so
// the UI observes loading, ingestion and degradation without polling.
// The loop terminates with the application context: no goroutine
// outlives the webview run.
func startStateBroadcaster(
	wailsApp *application.App,
	applicationInstance *app.App,
) {
	broadcast := func() {
		wailsApp.Event.Emit("freeiran:state", applicationInstance.State())
	}

	go func() {
		time.Sleep(500 * time.Millisecond)
		broadcast()
	}()

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		ctx := applicationInstance.Context()

		for {
			select {
			case <-ticker.C:
				broadcast()
			case <-ctx.Done():
				return
			}
		}
	}()
}
