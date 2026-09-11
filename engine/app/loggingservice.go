package app

// Settings and runtime-log services bound to the TypeScript frontend.
//
// SettingsService persists user preferences (preferred backend,
// refresh cadence, testing policy, storage-adjacent log limits,
// reduced motion) safely under the app config directory and applies
// the engine-affecting ones immediately.
//
// LogService exposes the persistent runtime log to the diagnostics
// view: incremental bounded reads (never a full-file transfer), live
// generation counters and an "open log location" action. Every entry
// was redacted at write time by the logging package, so nothing in
// this surface can leak credential material.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Parsaetak/FreeIran/internal/logging"
	"github.com/Parsaetak/FreeIran/system"
)

// Settings is the persisted user preference set.
type Settings struct {
	// PreferredBackend is the user-selected protocol core
	// ("" / "xray" / "v2ray" / "sing-box"). It only influences
	// selection when the backend is compatible and available.
	PreferredBackend string `json:"preferred_backend,omitempty"`

	// RefreshIntervalMinutes is the source refresh cadence
	// (0 = engine default).
	RefreshIntervalMinutes int `json:"refresh_interval_minutes,omitempty"`

	// TestingPolicy is one of "off", "on_add", "periodic".
	TestingPolicy string `json:"testing_policy,omitempty"`

	// LogLevel is the runtime log minimum level
	// ("debug"/"info"/"warn"/"error").
	LogLevel string `json:"log_level,omitempty"`

	// LogMaxBytesMB bounds the primary runtime log file before
	// rotation (0 = default 5 MiB).
	LogMaxBytesMB int `json:"log_max_bytes_mb,omitempty"`

	// LogMaxBackups bounds the kept rotated log files (0 = default 4).
	LogMaxBackups int `json:"log_max_backups,omitempty"`

	// ReducedMotion asks the UI to minimize animation (accessibility).
	ReducedMotion bool `json:"reduced_motion"`
}

// settingsPath is the persisted settings file.
func (a *App) settingsPath() string {
	return filepath.Join(a.layout.Config, "settings.json")
}

// loadSettings reads the persisted settings (missing or unreadable
// settings fall back to defaults — never brick the app).
func (a *App) loadSettings() Settings {
	settings := Settings{}

	raw, err := os.ReadFile(a.settingsPath())
	if err != nil {
		return settings // first boot or unreadable: defaults
	}

	if err := json.Unmarshal(raw, &settings); err != nil {
		return settings
	}

	return settings
}

// currentSettings returns a snapshot of the active settings.
func (a *App) currentSettings() Settings {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.settings
}

// applySettings routes engine-affecting preferences into the running
// subsystems. Callers persist before (or after) calling.
func (a *App) applySettings(settings Settings) {
	if a.logger == nil {
		return
	}

	if settings.LogLevel != "" {
		a.logger.SetLevel(logging.Level(settings.LogLevel))
	}

	if settings.LogMaxBytesMB > 0 {
		a.logger.SetLimits(int64(settings.LogMaxBytesMB)<<20, settings.LogMaxBackups)
	}
}

// SettingsService exposes user preferences to the UI.
type SettingsService struct {
	app *App
}

// NewSettingsService binds a settings service to the app.
func NewSettingsService(a *App) *SettingsService {
	return &SettingsService{app: a}
}

// Get returns the active settings.
func (s *SettingsService) Get() Settings {
	return s.app.currentSettings()
}

// Save validates, persists and applies new settings. The saved value
// is returned so the UI can reconcile.
func (s *SettingsService) Save(settings Settings) (Settings, error) {
	if err := validateSettings(settings); err != nil {
		return s.app.currentSettings(), err
	}

	raw, err := json.MarshalIndent(&settings, "", "  ")
	if err != nil {
		return s.app.currentSettings(), fmt.Errorf("app: encode settings: %w", err)
	}

	if err := system.WriteFileAtomic(s.app.settingsPath(), raw, 0o600); err != nil {
		return s.app.currentSettings(), fmt.Errorf("app: persist settings: %w", err)
	}

	s.app.mu.Lock()
	s.app.settings = settings
	s.app.mu.Unlock()

	s.app.applySettings(settings)

	logging.E("app", "settings_updated",
		"settings saved (backend %q, log level %s)",
		settings.PreferredBackend, settings.LogLevel)

	return settings, nil
}

// validateSettings rejects malformed preferences with readable
// errors (never leaks internals).
func validateSettings(settings Settings) error {
	switch settings.PreferredBackend {
	case "", "xray", "v2ray", "sing-box":
	default:
		return fmt.Errorf("app: unknown preferred backend %q", settings.PreferredBackend)
	}

	switch settings.TestingPolicy {
	case "", "off", "on_add", "periodic":
	default:
		return fmt.Errorf("app: unknown testing policy %q", settings.TestingPolicy)
	}

	switch settings.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("app: unknown log level %q", settings.LogLevel)
	}

	if settings.RefreshIntervalMinutes < 0 || settings.RefreshIntervalMinutes > 24*60 {
		return fmt.Errorf("app: refresh interval out of range")
	}

	if settings.LogMaxBytesMB < 0 || settings.LogMaxBytesMB > 1024 {
		return fmt.Errorf("app: log size limit out of range")
	}

	if settings.LogMaxBackups < 0 || settings.LogMaxBackups > 64 {
		return fmt.Errorf("app: log backup count out of range")
	}

	return nil
}

// LogService exposes the persistent runtime log to the diagnostics UI.
type LogService struct {
	app *App
}

// NewLogService binds a log service to the app.
func NewLogService(a *App) *LogService {
	return &LogService{app: a}
}

// LogFilter selects which entries Recent returns.
type LogFilter struct {
	// SinceSeq returns only entries newer than this sequence number
	// (incremental polling; 0 = from the beginning of the buffer).
	SinceSeq uint64 `json:"since_seq,omitempty"`

	// Limit bounds the page (default 200, max 1000).
	Limit int `json:"limit,omitempty"`

	// Subsystem filters by subsystem ("" = all).
	Subsystem string `json:"subsystem,omitempty"`

	// Query is a case-insensitive substring over message and event.
	Query string `json:"query,omitempty"`

	// Level filters to entries at or above the level ("" = all).
	Level string `json:"level,omitempty"`
}

// LogPage is one bounded page of runtime log entries.
type LogPage struct {
	Entries []logging.Entry `json:"entries"`
	LastSeq uint64          `json:"last_seq"`
}

// Recent returns a bounded, incremental page of redacted entries.
func (s *LogService) Recent(filter LogFilter) LogPage {
	if s.app.logger == nil {
		return LogPage{Entries: []logging.Entry{}}
	}

	entries := s.app.logger.Recent(
		filter.SinceSeq, filter.Limit, filter.Subsystem, filter.Query)

	if filter.Level != "" {
		min := logging.Level(filter.Level)

		kept := entries[:0]
		for _, entry := range entries {
			if severityAtLeast(entry.Level, min) {
				kept = append(kept, entry)
			}
		}

		entries = kept
	}

	page := LogPage{
		Entries: entries,
		LastSeq: s.app.logger.LastSeq(),
	}

	if page.Entries == nil {
		page.Entries = []logging.Entry{}
	}

	return page
}

// severityAtLeast compares levels for the UI filter.
func severityAtLeast(level, min logging.Level) bool {
	rank := map[logging.Level]int{
		logging.LevelDebug: 0,
		logging.LevelInfo:  1,
		logging.LevelWarn:  2,
		logging.LevelError: 3,
	}

	return rank[level] >= rank[min]
}

// Subsystems lists the subsystems present in the current buffer
// (filter dropdown values).
func (s *LogService) Subsystems() []string {
	if s.app.logger == nil {
		return []string{}
	}

	seen := map[string]bool{}
	out := []string{}

	for _, entry := range s.app.logger.Recent(0, 1000, "", "") {
		if !seen[entry.Subsystem] {
			seen[entry.Subsystem] = true

			out = append(out, entry.Subsystem)
		}
	}

	return out
}

// Clear empties the in-memory diagnostics buffer. The on-disk log is
// preserved: it is the durable diagnostic record.
func (s *LogService) Clear() {
	if s.app.logger != nil {
		s.app.logger.Clear()
	}
}

// LogFile returns the primary runtime log file path for display.
func (s *LogService) LogFile() string {
	return filepath.Join(s.app.layout.Logs, "freeiran.log")
}

// OpenLogsDir opens the platform file manager at the log directory.
func (s *LogService) OpenLogsDir() error {
	if s.app.logger != nil {
		s.app.logger.Info("app", "logs_dir_opened", "user opened the log location")
	}

	return system.OpenDirectory(s.app.layout.Logs)
}
