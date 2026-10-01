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
// subsystem has flushed. v0.11.5: Wails-internal warnings/errors are
// bridged into the same log (wailslog.go) and the user-facing version
// surfaces are metadata-free (internal/version).
package main

import (
	"embed"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/Parsaetak/FreeIran/engine/app"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/coremgr"
	"github.com/Parsaetak/FreeIran/engine/testqueue"
	"github.com/Parsaetak/FreeIran/internal/appicon"
	"github.com/Parsaetak/FreeIran/internal/logging"
	"github.com/Parsaetak/FreeIran/internal/statepub"
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

	// v0.11.5: the redundant stderr startup announcement is gone —
	// engine/app.New owns the ONE authoritative application_start
	// record (compact, user-facing version only). A GUI-subsystem
	// binary has no console, so a stderr line was never visible to
	// the user; it merely duplicated the runtime log.

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

	// v0.9.13: the entrypoint no longer emits application_start —
	// engine/app.New is the ONE authoritative producer of the
	// successful startup record (version/commit/workspace fields),
	// and it also owns every boot-failure record. The entrypoint
	// owns the runtime-log lifecycle and the failure dialog only.

	// BaseDir is intentionally NOT set: app.New resolves the single
	// Workspace Root itself and runs the one-time legacy migration
	// when the workspace is fresh.
	applicationInstance, err := app.New(app.Options{
		Logger: logger,
	})
	if err != nil {
		// Boot-failure logging is owned by app.New (v0.9.13) —
		// including failures before its own logger is resolved,
		// which reach this same file through opts.Logger. New also
		// OWNS the injected logger on every failure path and closes
		// it before returning; the entrypoint close below is the
		// idempotent safety net that keeps this path correct even
		// for boot stages that never reach the transaction.
		if logger != nil {
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
		// v0.11.5: Wails-internal failures (WebView2 runtime
		// probing, environment/controller creation, navigation)
		// previously went to Wails' own logger — io.Discard in
		// non-debug builds — or to stderr, which a windowsgui
		// binary has no consumer for. They now land in the
		// FreeIran runtime log (subsystem "wails", warnings and
		// errors only). The Warn floor is deliberate: Wails'
		// dev-mode startup records ("Build Info" / "Platform
		// Info") are info/debug and would carry Go-toolchain
		// and vcs-revision metadata — which the runtime log
		// must never reintroduce (v0.11.5 log contract).
		Logger:       newWailsLogHandler(logger),
		LogLevel:     slog.LevelWarn,
		ErrorHandler: newWailsErrorHandler(logger),
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
			application.NewService(app.NewInternetToolsService(applicationInstance)),
			// v0.9.10: persistent favorites, user groups and the
			// evidence-based source reliability dashboard (v0.7
			// roadmap work completed on the existing architecture).
			application.NewService(app.NewCollectionService(applicationInstance)),
			application.NewService(app.NewProxyChainService(applicationInstance)),
			// v0.9.11: Connection Profiles (P2 §18) — named connection
			// preference sets over the EXISTING settings + connection
			// engine; activation runs through the one settings path.
			application.NewService(app.NewProfileService(applicationInstance)),
			// v0.10.2: first-class personal configuration import.
			application.NewService(app.NewImportService(applicationInstance)),
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

	// v0.9.15: forward the ONE authoritative queue-state stream to the
	// UI (freeiran:queuestate). The payload is the complete
	// LiveStateView (live fingerprint set + change version + stats +
	// pause) — the same shape the LiveState binding returns as the
	// recovery read, so event and recovery can never disagree. The
	// emitter is bounded: bursts coalesce into the newest complete
	// state.
	queueStateEmitter := statepub.NewBoundedEmitter("ui-queuestate",
		func(view testqueue.LiveStateView) {
			wailsApp.Event.Emit("freeiran:queuestate", view)
		})

	defer queueStateEmitter.Stop()

	applicationInstance.SetQueueStateListener(queueStateEmitter.Submit)

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

	// v0.11.2: native system tray. Built on Wails v3's SystemTray
	// (NOT a React fake): the icon, menu and toggle behavior come
	// from the platform's tray surface so FreeIran behaves like a
	// normal Windows desktop client.
	//
	// v0.11.3: the tray is governed by the PERSISTENT
	// `tray_enabled` setting (Settings struct, default true via
	// nil-pointer semantics). The trayManager owns the lifecycle:
	//
	//   - tray_enabled = true  → the native tray is created,
	//     close-to-tray is active and the tray menu carries a
	//     "System Tray" checkbox that reflects the REAL setting.
	//   - tray_enabled = false → the tray is destroyed (no dead
	//     icon), close-to-tray is OFF (closing the window is a
	//     normal application close) and the main window's close
	//     path runs the standard quit lifecycle. The setting stays
	//     recoverable from Settings (or the tray checkbox while
	//     the tray exists).
	//
	// Behavior when enabled (unchanged from v0.11.2):
	//
	//   - close main window → hide to tray (does not quit).
	//   - tray Show → restore the main window.
	//   - tray navigation actions (Configurations / Network /
	//     Diagnostics / Settings) → restore the window AND emit a
	//     freeiran:navigate event the React shell listens for. No
	//     duplicate application windows are ever opened.
	//   - tray Quit → run the existing OnShutdown hook
	//     (applicationInstance.Shutdown), then wailsApp.Quit.
	//   - Disconnect when connected → stops the active tunnel.
	//
	// v0.11.5: the manager is constructed when the main window is
	// (inside ApplicationStarted — see below) and published through
	// the holder so the settings listener and the shutdown hook can
	// reach it. A nil holder means the startup show never ran; both
	// consumers are nil-safe.
	var trayHolder atomic.Pointer[trayManager]

	settingsService := app.NewSettingsService(applicationInstance)

	// React to settings mutations (Settings UI + tray checkbox —
	// both write through the ONE settings path). The reconcile
	// creates/destroys the tray to match the persisted setting.
	// (No synthetic initial dispatch exists: the initial reconcile
	// is owned by the ApplicationStarted handler below.)
	applicationInstance.SetSettingsListener(func(s app.Settings) {
		if tm := trayHolder.Load(); tm != nil {
			tm.Reconcile(s.TrayEnabledOrDefault())
		}
	})

	// v0.11.5 — THE GUI LAUNCH FIX (root-caused against the pinned
	// wails v3.0.0-beta.19 source; see docs/architecture.md §2):
	//
	// 1. The v0.11.2–v0.11.4 code called trayManager.Reconcile
	//    BEFORE Run(). Reconcile dispatches through
	//    application.InvokeAsync → App.dispatchOnMainThread, whose
	//    first statement dereferences the platform app (a.impl)
	//    WITHOUT a nil check — and a.impl does not exist until
	//    Run() assigns it. The call therefore panicked on the main
	//    goroutine BEFORE Run(), AFTER the engine had already
	//    logged application_start…application_ready: process dead,
	//    no window, no console trace, CI blind (the headless smoke
	//    exits before this path and the PE test reads only the
	//    subsystem field).
	//
	// 2. In beta.19 a window created BEFORE Run() is built with
	//    style WS_OVERLAPPEDWINDOW, which does NOT include
	//    WS_VISIBLE, and its only automatic show path is the
	//    WebView2 navigation-completed callback; a Show() issued
	//    before Run() is a no-op (the platform app does not exist
	//    yet). Visibility therefore depended entirely on WebView2
	//    succeeding.
	//
	// The fix builds the ONE main window FROM the ApplicationStarted
	// event — the exact pattern wails' own updater host uses
	// (NewWithOptions runs the window construction synchronously
	// once the app is running, and its HWND is created on the main
	// thread) — then shows it immediately: create → show → hook →
	// tray, all deterministic, no pre-Run dispatch, no Show()
	// fallback path, no second window, no sleeps. The window is
	// visible even if WebView2 subsequently fails (an empty shell
	// beats an invisible process, and the failure itself is
	// captured by the wails log bridge above). The engine-side
	// state publishers are already wired, so no transition is lost
	// by the window starting a few ticks after Run.
	cancelStartupShow := wailsApp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		if logger != nil {
			logger.Info("app", "window_create",
				"creating main window (ApplicationStarted)")
		}

		// WindowCentered is the default start position; no
		// override needed.
		//
		// Icon (v0.9.1): Linux gets the embedded PNG as the GTK
		// window icon. On Windows the titlebar/taskbar/
		// executable identity comes from the linked resource
		// (cmd/freeiran/*.syso, built from
		// assets/freeiran-icon.ico), so no runtime bytes are
		// needed there.
		mainWindow := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
			Title:     "FreeIran",
			Width:     1280,
			Height:    800,
			MinWidth:  960,
			MinHeight: 600,
			Linux: application.LinuxWindow{
				Icon: appicon.PNG,
			},
		})

		mainWindow.Show()

		if logger != nil {
			logger.Info("app", "window_show",
				"main window shown (ApplicationStarted)")
		}

		// close-to-tray (v0.11.2 behavior, now SETTING-AWARE):
		// with tray_enabled the window close is intercepted and
		// hidden to tray; with the tray disabled, closing the
		// window is a NORMAL application close — the process
		// must not stay resident only because an old tray
		// existed. The current setting is read per close (cheap
		// mutex read; always authoritative).
		mainWindow.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
			if settingsService.Get().TrayEnabledOrDefault() {
				mainWindow.Hide()
				e.Cancel()
			}
			// else: fall through — the OS closes the window
			// and the Wails app quits through the default
			// path (OnShutdown runs, tray already absent).
		})

		tm := newTrayManager(trayManagerOptions{
			app:                 wailsApp,
			applicationInstance: applicationInstance,
			mainWindow:          mainWindow,
			settingsService:     settingsService,
		})

		trayHolder.Store(tm)

		// Initial creation follows the persisted setting — the
		// SAME reconcile the settings listener uses at runtime,
		// now safely post-Run (never pre-Run). Tray semantics
		// are unchanged from v0.11.3.
		tm.Reconcile(settingsService.Get().TrayEnabledOrDefault())
	})
	defer cancelStartupShow()

	// Graceful shutdown: the OnShutdown hook runs before process
	// exit and must not lose data. Order per docs (§ shutdown):
	// engine stops (connection → TUN → providers → cores) inside
	// applicationInstance.Shutdown, the tray is destroyed BEFORE
	// that so no menu handler can fire into a tearing-down
	// engine, and the runtime log is flushed last by Shutdown.
	wailsApp.OnShutdown(func() {
		if tm := trayHolder.Load(); tm != nil {
			tm.Destroy()
		}

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

// trayMenuEntry pairs a label with its click handler for the tray
// menu builder.
type trayMenuEntry struct {
	label   string
	handler func(*application.Context)
}

// trayManagerOptions carries the wired dependencies of the tray
// manager (v0.11.3): the Wails app, the engine instance, the main
// window and the ONE settings service.
type trayManagerOptions struct {
	app                 *application.App
	applicationInstance *app.App
	mainWindow          *application.WebviewWindow
	settingsService     *app.SettingsService
}

// trayManager owns the native tray lifecycle under tray_enabled.
// All fields are guarded by mu; Wails menu item state is read/written
// on the reconcile path only.
type trayManager struct {
	mu           sync.Mutex
	opts         trayManagerOptions
	tray         *application.SystemTray
	trayMenu     *application.Menu
	trayCheckbox *application.MenuItem
}

func newTrayManager(opts trayManagerOptions) *trayManager {
	return &trayManager{opts: opts}
}

// Reconcile makes the tray match the requested state. Safe from any
// goroutine: platform work is posted to the UI thread via
// application.InvokeAsync (a no-op queue before Run, main-thread
// dispatch after).
func (t *trayManager) Reconcile(enabled bool) {
	application.InvokeAsync(func() {
		t.mu.Lock()
		defer t.mu.Unlock()

		if enabled && t.tray == nil {
			t.buildLocked()
		} else if !enabled && t.tray != nil {
			t.destroyLocked()
		}
	})
}

// Destroy tears the tray down (shutdown path). Idempotent.
func (t *trayManager) Destroy() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.tray != nil {
		t.destroyLocked()
	}
}

// buildLocked constructs the tray + menu. Callers hold t.mu and run
// on the UI thread (InvokeAsync / pre-Run composition).
func (t *trayManager) buildLocked() {
	tray := t.opts.app.SystemTray.New()
	tray.SetIcon(appicon.PNG)

	trayMenu := t.opts.app.Menu.New()
	trayMenu.Add("Show").OnClick(func(*application.Context) {
		t.opts.mainWindow.Show()
		t.opts.mainWindow.Focus()
	})
	trayMenu.Add("Hide").OnClick(func(*application.Context) {
		t.opts.mainWindow.Hide()
	})
	trayMenu.AddSeparator()
	trayMenu.Add("Configurations").OnClick(func(*application.Context) {
		t.opts.mainWindow.Show()
		t.opts.mainWindow.Focus()
		t.opts.app.Event.Emit("freeiran:navigate", map[string]string{"page": "configs"})
	})
	trayMenu.Add("Network").OnClick(func(*application.Context) {
		t.opts.mainWindow.Show()
		t.opts.mainWindow.Focus()
		t.opts.app.Event.Emit("freeiran:navigate", map[string]string{"page": "network"})
	})
	trayMenu.Add("Diagnostics").OnClick(func(*application.Context) {
		t.opts.mainWindow.Show()
		t.opts.mainWindow.Focus()
		t.opts.app.Event.Emit("freeiran:navigate", map[string]string{"page": "diagnostics"})
	})
	trayMenu.Add("Settings").OnClick(func(*application.Context) {
		t.opts.mainWindow.Show()
		t.opts.mainWindow.Focus()
		t.opts.app.Event.Emit("freeiran:navigate", map[string]string{"page": "settings"})
	})
	trayMenu.AddSeparator()

	// Disconnect when connected: stops the active core session and
	// any active tunnel through the SAME services the UI buttons
	// use (no parallel control path).
	trayMenu.Add("Disconnect when connected").OnClick(func(*application.Context) {
		_ = app.NewTunnelService(t.opts.applicationInstance).Disable()
		_ = app.NewConnectionService(t.opts.applicationInstance).Disconnect()
	})
	trayMenu.AddSeparator()

	// v0.11.3: the "System Tray" checkbox reflects the REAL
	// persisted setting. Wails toggles the checkbox state itself on
	// click; the handler persists the new value through the ONE
	// settings path and the settings listener reconciles the tray.
	checkbox := trayMenu.AddCheckbox("System Tray", t.opts.settingsService.Get().TrayEnabledOrDefault())
	checkbox.OnClick(func(*application.Context) {
		next := checkbox.Checked()

		settings := t.opts.settingsService.Get()
		settings.TrayEnabled = &next

		if _, err := t.opts.settingsService.Save(settings); err != nil {
			slog.Error("tray setting persist failed", "error", err)
		}
		// The SetSettingsListener reconcile performs any
		// creation/destruction; no direct action here.
	})

	trayMenu.AddSeparator()
	trayMenu.Add("Quit").OnClick(func(*application.Context) {
		// Run the SAME graceful shutdown the OnShutdown hook runs
		// on a process-level close — no orphan providers/cores.
		t.opts.applicationInstance.Shutdown()
		t.opts.app.Quit()
	})

	tray.SetMenu(trayMenu)

	t.tray = tray
	t.trayMenu = trayMenu
	t.trayCheckbox = checkbox
}

// destroyLocked removes the tray icon and invalidates the menu
// references (use-after-destroy is impossible: every handler only
// touches t.opts fields, and the references are cleared here). The
// persisted setting survives — Reconcile(true) rebuilds the tray.
func (t *trayManager) destroyLocked() {
	t.tray.Destroy()
	t.tray = nil
	t.trayMenu = nil
	t.trayCheckbox = nil
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

	// v0.9.13: no application_start here — app.New is the single
	// authoritative producer (see the desktop path above).

	applicationInstance, err := app.New(app.Options{
		BaseDir:             baseDir,
		Logger:              logger,
		RunIngestionOnStart: false,
		SkipDefaultSources:  true,
	})
	if err != nil {
		// app.New owns the fatal boot record (v0.9.13).
		if logger != nil {
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
