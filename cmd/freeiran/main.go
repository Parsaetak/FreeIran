// Command freeiran is the FreeIran desktop application entrypoint
// built on Wails v3.
//
// The binary composes the engine services (engine/app) with a native
// webview window serving the TypeScript frontend. The UI communicates
// exclusively through generated Wails bindings and events — there is
// no localhost HTTP API.
package main

import (
	"embed"
	"log"
	"log/slog"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/Parsaetak/FreeIran/engine/app"
	"github.com/Parsaetak/FreeIran/internal/version"
)

// assets embeds the built frontend. The dist directory is produced by
// the frontend build (npm run build) before compiling this binary.
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	slog.Info("FreeIran starting", "version", version.String())

	applicationInstance, err := app.New(app.DefaultOptions())
	if err != nil {
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
	// exit and must not lose data.
	wailsApp.OnShutdown(func() {
		applicationInstance.Shutdown()
	})

	if err := wailsApp.Run(); err != nil {
		log.Fatalf("run failed: %v", err)
	}
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
