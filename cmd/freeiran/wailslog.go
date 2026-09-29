// wailslog.go bridges Wails-internal diagnostics into FreeIran's
// persistent runtime log (v0.11.5).
//
// WHY THIS EXISTS: Wails' own error/logger surface was invisible to the
// product. In non-debug builds its default logger is io.Discard, and a
// windowsgui-subsystem binary has no console for stderr. When WebView2
// initialization or navigation fails, every diagnostic vanished — the
// v0.11.4 "process boots, no window, no evidence" defect class. The
// bridge routes Wails warnings and errors into the SAME runtime log
// every other subsystem uses (redaction, rotation, seq ordering).
//
// The Warn floor is a deliberate part of the v0.11.5 log contract:
// Wails' dev-mode startup records ("Build Info", "Platform Info") are
// info/debug and carry Go-toolchain and vcs-revision metadata, which
// the runtime log must never contain. Only warnings and errors pass.

package main

import (
	"context"
	"log/slog"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

// newWailsLogHandler adapts the runtime logger into the slog.Logger
// Wails expects in application.Options.Logger. A nil runtime logger
// (log open failure) returns nil so Wails falls back to its own
// default — the bridge never becomes a logging bottleneck.
func newWailsLogHandler(logger *logging.Logger) *slog.Logger {
	if logger == nil {
		return nil
	}

	return slog.New(&wailsLogHandler{logger: logger})
}

// wailsLogHandler is the slog.Handler half of the bridge. It admits
// warnings and errors only and maps them onto the structured Record
// shape (subsystem "wails") so the entries ride the normal
// redaction/rotation path.
type wailsLogHandler struct {
	logger *logging.Logger
	attrs  []slog.Attr
}

// Enabled implements slog.Handler: the Warn floor keeps Wails'
// toolchain/vcs-bearing startup info records out of the runtime log.
func (h *wailsLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

// Handle implements slog.Handler.
func (h *wailsLogHandler) Handle(_ context.Context, record slog.Record) error {
	if h.logger == nil {
		return nil
	}

	level := logging.LevelWarn
	if record.Level >= slog.LevelError {
		level = logging.LevelError
	}

	rec := logging.Record{
		Level:     level,
		Subsystem: "wails",
		Event:     "internal",
		Message:   record.Message,
	}

	var fields map[string]any

	record.Attrs(func(a slog.Attr) bool {
		if fields == nil {
			fields = make(map[string]any)
		}

		fields[a.Key] = a.Value.Any()

		return true
	})

	for _, a := range h.attrs {
		if fields == nil {
			fields = make(map[string]any)
		}

		fields[a.Key] = a.Value.Any()
	}

	rec.Fields = fields

	h.logger.Log(rec)

	return nil
}

// WithAttrs implements slog.Handler (immutable clone, no slice
// aliasing with the source handler).
func (h *wailsLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	next = append(next, h.attrs...)
	next = append(next, attrs...)

	return &wailsLogHandler{logger: h.logger, attrs: next}
}

// WithGroup implements slog.Handler. Wails diagnostics are flat; groups
// resolve to the same subsystem mapping.
func (h *wailsLogHandler) WithGroup(string) slog.Handler { return h }

// newWailsErrorHandler returns the application.Options.ErrorHandler
// callback. Wails routes every internal error through it (WebView2
// probing, environment/controller creation, navigation failures) INSTEAD
// of its logger when set — so both bridges are installed and cover the
// full internal failure surface. A nil runtime logger yields a no-op.
func newWailsErrorHandler(logger *logging.Logger) func(error) {
	if logger == nil {
		return func(error) {}
	}

	return func(err error) {
		logger.Error("wails", "internal_error", "error", "internal", "%v", err)
	}
}
