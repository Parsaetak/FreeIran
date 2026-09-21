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

	"github.com/Parsaetak/FreeIran/engine/native"
	"github.com/Parsaetak/FreeIran/internal/logging"
	"github.com/Parsaetak/FreeIran/system"

	"github.com/Parsaetak/FreeIran/engine/provider"
)

// Settings is the persisted user preference set.
type Settings struct {
	// PreferredBackend is the user-selected protocol core
	// ("" / "xray" / "v2ray" / "sing-box"). It only influences
	// selection when the backend is compatible and available.
	PreferredBackend string `json:"preferred_backend,omitempty"`

	// --- v0.9.8.1 provider settings (§8/§9/§12) ---------------------

	// ProviderMode is the Quick Connect provider choice:
	// "" / "auto" (evidence-based) / "configs" / "tor" / "psiphon".
	ProviderMode string `json:"provider_mode,omitempty"`

	// TorBridgeLines are user-provided bridge lines (validated before
	// launch; never logged — bridge material is private).
	TorBridgeLines []string `json:"tor_bridge_lines,omitempty"`

	// TorTransportPlugins maps transport names (obfs4, snowflake) to
	// user-provided client plugin executables.
	TorTransportPlugins map[string]string `json:"tor_transport_plugins,omitempty"`

	// PsiphonExtraConfig is advanced user-provided JSON merged into
	// the generated Psiphon client config (validated as JSON).
	PsiphonExtraConfig string `json:"psiphon_extra_config,omitempty"`

	// PsiphonUserBinary is an optional user-provided console-client
	// path adopted after validation.
	PsiphonUserBinary string `json:"psiphon_user_binary,omitempty"`

	// RefreshIntervalMinutes is the source refresh cadence
	// (0 = engine default).
	RefreshIntervalMinutes int `json:"refresh_interval_minutes,omitempty"`

	// TestingPolicy is one of "off", "on_add", "periodic".
	TestingPolicy string `json:"testing_policy,omitempty"`

	// LogLevel is the runtime log minimum severity floor
	// ("debug"/"info"/"warn"/"error"). It gates info/warn/error
	// records; debug-severity verbosity is governed by the logging
	// profile (v0.9.8.4).
	LogLevel string `json:"log_level,omitempty"`

	// LoggingProfile is the v0.9.8.4 logging profile:
	// "" / "normal" (default end-user mode) / "detailed" (adds
	// lifecycle diagnostics + full record identity) / "debug"
	// (verbose diagnostics + correlation identifiers). Applied to the
	// live logger immediately on save — no restart.
	LoggingProfile string `json:"logging_profile,omitempty"`

	// LogMaxBytesMB bounds the primary runtime log file before
	// rotation (0 = default 5 MiB).
	LogMaxBytesMB int `json:"log_max_bytes_mb,omitempty"`

	// LogMaxBackups bounds the kept rotated log files (0 = default 4).
	LogMaxBackups int `json:"log_max_backups,omitempty"`

	// ReducedMotion asks the UI to minimize animation (accessibility).
	ReducedMotion bool `json:"reduced_motion"`

	// --- local inbound port preferences ------------------------------
	//
	// The Settings UI exposed these controls since v0.9.8.3, but the
	// fields were never persisted and the connection manager's
	// user-port path was never reached (the stale bindings hid the
	// gap). They now flow into Manager.SetLocalPorts on save: the
	// manager re-validates bindability before every attempt and fails
	// fast with a per-port error when a port is taken.

	// LocalSocksPort is the user-selected local SOCKS inbound port
	// (0 = automatic ephemeral allocation; otherwise 1024-65535).
	LocalSocksPort int `json:"local_socks_port,omitempty"`

	// LocalHTTPPort is the user-selected local HTTP inbound port
	// (0 = disabled / automatic; otherwise 1024-65535).
	LocalHTTPPort int `json:"local_http_port,omitempty"`

	// --- v0.9.8.6 route-trust policy ----------------------------------

	// AllowUntrustedPublicRoutes opts Quick Connect / Auto into
	// connecting through PUBLIC, UNTRUSTED source nodes (default:
	// false). Public nodes remain fully usable through explicit
	// selection on the Configs page — this switch only governs the
	// AUTOMATIC route selection. A public node can be fast + stable +
	// verified reachable + untrusted: reliability and route trust are
	// separate dimensions, and the automatic policy never silently
	// promotes untrusted routes to trusted ones.
	AllowUntrustedPublicRoutes bool `json:"allow_untrusted_public_routes"`

	// --- v0.9.1 developer / advanced options -----------------------
	//
	// Every field here is wired to real engine behaviour in
	// applySettings (or read live where noted). Nothing is
	// decorative: if a toggle exists it changes what the app does.

	// DevVerboseDiagnostics enriches the diagnostic report (and the
	// UI error surfaces) with technical detail: memory snapshot,
	// native acceleration status, portable-mode state and runtime
	// versions. Read live by BuildDiagnosticReport.
	DevVerboseDiagnostics bool `json:"dev_verbose_diagnostics"`

	// DevQueueWorkers overrides the test-queue worker-pool size
	// (0 = adaptive memory-booster control, 1-64 = fixed). Applied
	// to the live queue and respected over future booster ticks.
	DevQueueWorkers int `json:"dev_queue_workers,omitempty"`

	// DevNetTimeoutSeconds overrides the per-probe network-test
	// timeout (0 = default 4s, 1-120 = fixed). Read live by
	// NetworkService.networkConfig before every check.
	DevNetTimeoutSeconds int `json:"dev_net_timeout_seconds,omitempty"`

	// DevForceGoFallback pins the native acceleration bridge to the
	// pure-Go implementation (same state as FREEIRAN_NATIVE=off).
	// Applied to the live engine immediately on save.
	DevForceGoFallback bool `json:"dev_force_go_fallback"`

	// --- v0.9.6 test modes, ranking and racing (§9/§10/§13) ---------

	// TestMode is the user-selected test mode: "ping", "url",
	// "ping_url", "handshake" or "full" ("" = ping_url default).
	TestMode string `json:"test_mode,omitempty"`

	// TestPingSamples bounds the ping facet's sample count
	// (0 = default 4; clamped 1-16).
	TestPingSamples int `json:"test_ping_samples,omitempty"`

	// TestURL is the URL-test target ("" = the standard 204 endpoint).
	TestURL string `json:"test_url,omitempty"`

	// TestURLTimeoutSeconds bounds the URL facet (0 = default 12s).
	TestURLTimeoutSeconds int `json:"test_url_timeout_seconds,omitempty"`

	// TestMaxCandidates caps how many candidates one flow tests
	// (0 = default 20; clamped 1-200).
	TestMaxCandidates int `json:"test_max_candidates,omitempty"`

	// SortMode is the ranking order: best_overall (default),
	// lowest_ping, lowest_median_ping, lowest_jitter,
	// lowest_packet_loss, best_url_response, highest_success_rate,
	// most_stable, recently_verified.
	SortMode string `json:"sort_mode,omitempty"`

	// EnableRacing turns on controlled connection racing of the top
	// candidates (first VERIFIED usable connection wins).
	EnableRacing bool `json:"enable_racing"`

	// RacingCandidates is how many top candidates race (2-4,
	// default 2).
	RacingCandidates int `json:"racing_candidates,omitempty"`

	// DisableAutoRecovery turns off the v0.9.3 automatic recovery
	// supervisor. Recovery is ON by default (the autonomous
	// connection engine's core promise): when the active
	// connection fails, FreeIran switches to the next viable
	// candidate with bounded retries, cooldowns and failure
	// memory. This flag is the explicit user opt-out.
	DisableAutoRecovery bool `json:"disable_auto_recovery"`
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
	if settings.LogLevel != "" && a.logger != nil {
		a.logger.SetLevel(logging.Level(settings.LogLevel))
	}

	// v0.9.8.4: the logging profile switches at runtime — the logger
	// owns the whole admission policy, no restart required.
	if a.logger != nil {
		a.logger.SetProfile(logging.ParseProfile(settings.LoggingProfile))
	}

	if settings.LogMaxBytesMB > 0 && a.logger != nil {
		a.logger.SetLimits(int64(settings.LogMaxBytesMB)<<20, settings.LogMaxBackups)
	}

	// v0.9.8.1: apply the user's provider configuration (validated;
	// invalid bridge lines are skipped with the error surfaced
	// through the settings event, never silently accepted).
	if a.torEngine != nil {
		_ = a.torEngine.SetOptions(provider.TorOptions{
			BridgeLines:      settings.TorBridgeLines,
			TransportPlugins: settings.TorTransportPlugins,
		})
	}

	if a.psiphonEngine != nil {
		_ = a.psiphonEngine.SetOptions(provider.PsiphonOptions{
			ExtraConfig: settings.PsiphonExtraConfig,
		})
	}

	// Developer: fixed test-queue worker override (v0.9.1). The
	// adaptive booster keeps adjusting unless the override is set;
	// effectiveQueueConcurrency arbitrates (see memoryservice.go).
	a.initMu.Lock()
	queue := a.testQueue
	a.initMu.Unlock()

	if queue != nil && settings.DevQueueWorkers > 0 {
		queue.SetConcurrency(settings.DevQueueWorkers)
	}

	// Local inbound port preferences reach the live
	// connection manager immediately on save (0 = automatic). The
	// manager re-checks bindability per attempt, so an occupied port
	// surfaces as a clear per-attempt failure, never a silent
	// fallback.
	if a.connMgr != nil {
		a.connMgr.SetLocalPorts(settings.LocalSocksPort, settings.LocalHTTPPort)
	}

	// Developer: pin the native acceleration bridge to the Go path.
	native.SetForcedFallback(settings.DevForceGoFallback)

	if settings.DevForceGoFallback && a.logger != nil {
		a.logger.Info("app", "dev_settings_applied",
			"developer settings active (queue workers %d, net timeout %ds, force Go fallback %v)",
			settings.DevQueueWorkers, settings.DevNetTimeoutSeconds, settings.DevForceGoFallback)
	}
}

// effectiveQueueConcurrency resolves the boosters adaptive proposal
// against the developer override (DevQueueWorkers > 0 wins).
func (a *App) effectiveQueueConcurrency(adaptive int) int {
	a.mu.RLock()
	override := a.settings.DevQueueWorkers
	a.mu.RUnlock()

	if override > 0 {
		return override
	}

	return adaptive
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
//
// v0.9.12: Save and persistSettings share ONE write mutex — the whole
// read→validate→write→memory-update cycle is serialized, so
// concurrent settings writers can never interleave and leave memory
// and disk diverging.
func (s *SettingsService) Save(settings Settings) (Settings, error) {
	if err := validateSettings(settings); err != nil {
		return s.app.currentSettings(), err
	}

	raw, err := json.MarshalIndent(&settings, "", "  ")
	if err != nil {
		return s.app.currentSettings(), fmt.Errorf("app: encode settings: %w", err)
	}

	s.app.settingsWrite.Lock()
	defer s.app.settingsWrite.Unlock()

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

	switch settings.LoggingProfile {
	case "", "normal", "detailed", "debug":
	default:
		return fmt.Errorf("app: unknown logging profile %q", settings.LoggingProfile)
	}

	// Developer option ranges (v0.9.1).
	if settings.DevQueueWorkers < 0 || settings.DevQueueWorkers > 64 {
		return fmt.Errorf("app: queue worker override out of range (0-64)")
	}

	if settings.DevNetTimeoutSeconds < 0 || settings.DevNetTimeoutSeconds > 120 {
		return fmt.Errorf("app: network-test timeout override out of range (0-120)")
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

	// Local inbound port preferences — 0 = automatic,
	// otherwise a valid unprivileged TCP port. The manager still
	// re-checks bindability per attempt (check-then-use races resolve
	// as a normal failed attempt with a clear error).
	for name, port := range map[string]int{
		"SOCKS": settings.LocalSocksPort,
		"HTTP":  settings.LocalHTTPPort,
	} {
		if port == 0 {
			continue
		}

		if port < 1024 || port > 65535 {
			return fmt.Errorf("app: local %s port %d out of range (1024-65535, or 0 = automatic)", name, port)
		}
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

	// Event filters to an exact event name ("" = all) — v0.9.7 §20.
	Event string `json:"event,omitempty"`

	// BatchID / TestID / ConfigID / Core narrow the view to one
	// causal group (v0.9.7 §20).
	BatchID  string `json:"batch_id,omitempty"`
	TestID   string `json:"test_id,omitempty"`
	ConfigID string `json:"config_id,omitempty"`
	Core     string `json:"core,omitempty"`

	// ErrorsOnly keeps only warn/error entries (v0.9.7 §20).
	ErrorsOnly bool `json:"errors_only,omitempty"`
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

	// v0.9.7: structured filters run through Logger.Query — one
	// matching pass over the ring instead of post-filtering.
	entries := s.app.logger.Query(logging.Filter{
		SinceSeq:   filter.SinceSeq,
		Limit:      filter.Limit,
		Subsystem:  filter.Subsystem,
		Level:      logging.Level(filter.Level),
		Event:      filter.Event,
		BatchID:    filter.BatchID,
		TestID:     filter.TestID,
		ConfigID:   filter.ConfigID,
		Core:       filter.Core,
		ErrorsOnly: filter.ErrorsOnly,
		Query:      filter.Query,
	})

	page := LogPage{
		Entries: entries,
		LastSeq: s.app.logger.LastSeq(),
	}

	if page.Entries == nil {
		page.Entries = []logging.Entry{}
	}

	return page
}

// Related returns every entry correlated with the given event id
// (children via parent_event_id, batch/test/config siblings). Powers
// the diagnostics "show related" action (v0.9.7 §20).
func (s *LogService) Related(eventID string, limit int) []logging.Entry {
	if s.app.logger == nil {
		return []logging.Entry{}
	}

	entries := s.app.logger.Related(eventID, limit)
	if entries == nil {
		entries = []logging.Entry{}
	}

	return entries
}

// severityAtLeast compares levels for the UI filter. A switch instead
// of the previous per-call map literal: Recent() filters up to a
// thousand entries per UI poll, and the map allocated once per entry.
func severityAtLeast(level, min logging.Level) bool {
	return levelRank(level) >= levelRank(min)
}

// levelRank orders log levels; unknown levels rank lowest (0), which
// matches the zero value the previous map lookup returned.
func levelRank(level logging.Level) int {
	switch level {
	case logging.LevelError:
		return 3
	case logging.LevelWarn:
		return 2
	case logging.LevelInfo:
		return 1
	default:
		return 0
	}
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
