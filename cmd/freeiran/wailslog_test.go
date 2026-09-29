//go:build windows

package main

// v0.11.5 — the Wails→runtime-log bridge contract: warnings and errors
// land in the runtime log with the "wails" subsystem, debug/info records
// are dropped by the Warn floor (which keeps Wails' toolchain/vcs
// startup records out of the log), and a nil runtime logger degrades to
// a no-op bridge instead of a nil-pointer path.

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

func TestWailsLogBridgeWarnFloor(t *testing.T) {
	logger, err := logging.Open(logging.Options{
		Dir:          t.TempDir(),
		Name:         "test.log",
		MirrorStderr: false,
	})
	if err != nil {
		t.Fatalf("open test logger: %v", err)
	}

	defer func() { _ = logger.Close() }()

	bridge := newWailsLogHandler(logger)

	if bridge == nil {
		t.Fatal("non-nil logger must produce a slog bridge")
	}

	handler := bridge // *slog.Logger — the surface Wails receives

	if !bridge.Handler().Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("warn level must be enabled")
	}

	if !bridge.Handler().Enabled(context.Background(), slog.LevelError) {
		t.Fatal("error level must be enabled")
	}

	if bridge.Handler().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info level must be BELOW the bridge floor")
	}

	// Info-level records (Wails "Build Info"/"Platform Info" carry
	// toolchain + vcs metadata) are dropped by the Enabled floor —
	// Handle would also drop nothing if called, but Wails honors
	// Enabled, so the info record never reaches the log.
	handler.Info("Build Info:", "Compiler", "go1.26.8", "vcs.revision", "8028f7f7")
	handler.Warn("chromium warning", "detail", "probe-warning")
	handler.Error("WebView2 environment failed")

	entries := logger.Recent(0, 100, "", "")

	var sawWarn, sawError, sawInfo bool

	for _, entry := range entries {
		if entry.Subsystem != "wails" {
			t.Fatalf("bridge entry subsystem = %q, want wails: %+v", entry.Subsystem, entry)
		}

		switch {
		case strings.Contains(entry.Message, "chromium warning"):
			sawWarn = true

			if entry.Level != logging.LevelWarn {
				t.Fatalf("warning mapped to %q", entry.Level)
			}

			if entry.Fields["detail"] != "probe-warning" {
				t.Fatalf("attribute not carried: %+v", entry)
			}

		case strings.Contains(entry.Message, "WebView2 environment failed"):
			sawError = true

			if entry.Level != logging.LevelError {
				t.Fatalf("error mapped to %q", entry.Level)
			}

		case strings.Contains(entry.Message, "Build Info"):
			sawInfo = true
		}
	}

	if sawInfo {
		t.Fatal("info-level Wails record leaked past the Warn floor")
	}

	if !sawWarn || !sawError {
		t.Fatalf("bridge missed records (warn=%v error=%v): %+v", sawWarn, sawError, entries)
	}
}

func TestWailsErrorHandlerWritesRuntimeLog(t *testing.T) {
	logger, err := logging.Open(logging.Options{
		Dir:          t.TempDir(),
		Name:         "test.log",
		MirrorStderr: false,
	})
	if err != nil {
		t.Fatalf("open test logger: %v", err)
	}

	defer func() { _ = logger.Close() }()

	handler := newWailsErrorHandler(logger)

	handler(errWebView2Probe)

	entries := logger.Recent(0, 100, "", "")

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	entry := entries[0]

	if entry.Subsystem != "wails" || entry.Event != "internal_error" ||
		entry.Level != logging.LevelError {
		t.Fatalf("error bridge entry = %+v", entry)
	}

	if !strings.Contains(entry.Message, "WebView2 runtime probe failed") {
		t.Fatalf("error text not carried: %+v", entry)
	}
}

func TestWailsBridgesNilSafe(t *testing.T) {
	// A nil runtime logger (log open failure) must yield nil-safe
	// bridges: the slog.Logger is nil (Wails falls back to its own
	// default) and the error handler is a no-op.
	if bridge := newWailsLogHandler(nil); bridge != nil {
		t.Fatal("nil logger must produce a nil slog bridge")
	}

	handler := newWailsErrorHandler(nil)

	handler(errWebView2Probe) // must not panic
}

// errWebView2Probe is a stand-in for the Wails-internal WebView2
// initialization failure the bridge exists to capture.
var errWebView2Probe = errorString("WebView2 runtime probe failed")

type errorString string

func (e errorString) Error() string { return string(e) }
