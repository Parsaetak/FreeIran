package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logging_profile_test.go — v0.9.8.4 regression matrix for the three
// logging profiles (roadmap P1 §15): one authoritative admission
// policy inside the logger, runtime switching without restart,
// compact Normal output, bounded disk behaviour and unchanged
// redaction in every profile.

func newProfileLogger(t *testing.T, profile Profile) *Logger {
	t.Helper()

	logger, err := Open(Options{
		Dir:        t.TempDir(),
		MaxBytes:   1 << 20,
		MinLevel:   LevelInfo,
		Profile:    profile,
		MaxBackups: 3,
	})
	if err != nil {
		t.Fatalf("open logger: %v", err)
	}

	t.Cleanup(func() { _ = logger.Close() })

	return logger
}

func events(t *testing.T, l *Logger) []Entry {
	t.Helper()

	entries := l.Recent(0, 1000, "", "")
	if entries == nil {
		t.Fatal("no entries returned")
	}

	return entries
}

func hasEvent(entries []Entry, event string) bool {
	for _, entry := range entries {
		if entry.Event == event {
			return true
		}
	}

	return false
}

// --- Normal -------------------------------------------------------------

func TestNormalProfileSuppressesRoutineDiagnostics(t *testing.T) {
	logger := newProfileLogger(t, ProfileNormal)

	logger.Debug("conn", "routine_probe", "verbose detail %d", 1)
	logger.DebugLifecycle("conn", "lifecycle_detail", "stage detail %d", 2)
	logger.Info("conn", "connection_start", "connecting")
	logger.Warn("conn", "verification_failed", "not usable")
	logger.Error("conn", "connect_failed", "connect", "network", "boom")

	entries := events(t, logger)

	if hasEvent(entries, "routine_probe") {
		t.Fatal("Normal must suppress routine debug diagnostics")
	}

	if hasEvent(entries, "lifecycle_detail") {
		t.Fatal("Normal must suppress lifecycle-tagged debug diagnostics")
	}

	for _, want := range []string{"connection_start", "verification_failed", "connect_failed"} {
		if !hasEvent(entries, want) {
			t.Fatalf("Normal must keep operational record %q", want)
		}
	}

	if len(entries) != 3 {
		t.Fatalf("entries = %d, want exactly the 3 operational records", len(entries))
	}
}

func TestNormalProfileRecordsStayCompact(t *testing.T) {
	logger := newProfileLogger(t, ProfileNormal)

	logger.Info("app", "application_start", "starting")
	logger.Log(Record{
		Level: LevelInfo, Subsystem: "connection", Event: "quick_connect_verified",
		Correlate: true, Message: "verified",
	})

	entries := events(t, logger)

	if entries[0].SessionID != "" || entries[0].EventID != "" {
		t.Fatal("ordinary Normal records must stay compact (no identity)")
	}

	if entries[1].SessionID == "" || entries[1].EventID == "" {
		t.Fatal("correlation-opt-in records must carry identity in Normal")
	}

	// The on-disk record is compact too: the JSON line of an ordinary
	// record must not contain session/event identity keys.
	raw, err := os.ReadFile(filepath.Join(logger.opts.Dir, logger.opts.Name))
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")

	var ordinary map[string]any

	if err := json.Unmarshal([]byte(lines[0]), &ordinary); err != nil {
		t.Fatal(err)
	}

	if _, ok := ordinary["session_id"]; ok {
		t.Fatal("on-disk ordinary record must not carry session_id")
	}

	if _, ok := ordinary["event_id"]; ok {
		t.Fatal("on-disk ordinary record must not carry event_id")
	}
}

// --- Detailed -----------------------------------------------------------

func TestDetailedProfileAddsLifecycleWithoutSpam(t *testing.T) {
	logger := newProfileLogger(t, ProfileDetailed)

	logger.Debug("conn", "routine_probe", "verbose detail")
	logger.DebugLifecycle("core", "core_stage", "bootstrap stage %d", 3)
	logger.Info("conn", "connection_start", "connecting")

	entries := events(t, logger)

	if hasEvent(entries, "routine_probe") {
		t.Fatal("Detailed must still suppress routine debug diagnostics")
	}

	if !hasEvent(entries, "core_stage") {
		t.Fatal("Detailed must emit lifecycle-tagged diagnostics")
	}

	// Every emitted record carries full identity (lifecycle
	// traceability).
	for i, entry := range entries {
		if entry.SessionID == "" || entry.EventID == "" {
			t.Fatalf("entry %d: Detailed records must carry identity", i)
		}
	}

	// Dedupe still applies: repetitive ticks collapse to the first
	// occurrence (no uncontrolled high-frequency spam).
	dedupe := NewDedupe()

	for i := 0; i < 50; i++ {
		dedupe.Do(logger, LevelInfo, "provider", "bootstrap_tick", "tick", "tick %d", i)
	}

	if got := len(events(t, logger)); got != 3 {
		t.Fatalf("entries after 50 deduped ticks = %d, want 3 (spam must collapse)", got)
	}
}

// --- Debug --------------------------------------------------------------

func TestDebugProfileVerboseWithUniqueIdsAndRedaction(t *testing.T) {
	logger := newProfileLogger(t, ProfileDebug)

	logger.Debug("conn", "routine_probe", "verbose detail")
	logger.DebugLifecycle("conn", "lifecycle_detail", "stage detail")
	logger.Info("conn", "connection_start", "connecting via vless://UUID@example.org:443")

	entries := events(t, logger)

	for _, want := range []string{"routine_probe", "lifecycle_detail", "connection_start"} {
		if !hasEvent(entries, want) {
			t.Fatalf("Debug must emit %q", want)
		}
	}

	seen := map[string]bool{}

	for i, entry := range entries {
		if entry.SessionID == "" {
			t.Fatalf("entry %d: Debug records carry correlation identity", i)
		}

		if entry.EventID == "" {
			t.Fatalf("entry %d: Debug records carry an event id", i)
		}

		if seen[entry.EventID] {
			t.Fatalf("entry %d: duplicate event id %q", i, entry.EventID)
		}

		seen[entry.EventID] = true
	}

	// Redaction is unchanged by the profile: secrets never survive.
	for _, entry := range entries {
		if strings.Contains(entry.Message, "UUID@example.org") {
			t.Fatalf("event %q leaked credential material: %q", entry.Event, entry.Message)
		}
	}

	if !strings.Contains(entries[2].Message, redactedMark) {
		t.Fatalf("expected %q in %q", redactedMark, entries[2].Message)
	}
}

// --- Runtime switching ---------------------------------------------------

func TestProfileSwitchesAtRuntimeWithoutRestart(t *testing.T) {
	logger := newProfileLogger(t, ProfileNormal)

	if logger.Profile() != ProfileNormal {
		t.Fatal("logger must start on the Normal profile")
	}

	// Normal: routine debug suppressed.
	logger.Debug("conn", "probe", "one")
	if hasEvent(events(t, logger), "probe") {
		t.Fatal("Normal must suppress the probe")
	}

	// Normal → Detailed: the SAME logger instance now admits
	// lifecycle records.
	logger.SetProfile(ProfileDetailed)
	logger.DebugLifecycle("conn", "lifecycle_probe", "two")
	if !hasEvent(events(t, logger), "lifecycle_probe") {
		t.Fatal("Detailed must admit lifecycle records after the switch")
	}

	// Detailed → Debug: routine verbose records become available.
	logger.SetProfile(ProfileDebug)
	logger.Debug("conn", "verbose_probe", "three")
	if !hasEvent(events(t, logger), "verbose_probe") {
		t.Fatal("Debug must admit verbose records after the switch")
	}

	// Debug → Normal: verbose records are suppressed again, without
	// touching anything else (no restart, same instance).
	logger.SetProfile(ProfileNormal)
	logger.Debug("conn", "post_switch_probe", "four")
	logger.Info("conn", "connection_start", "five")

	entries := events(t, logger)

	if hasEvent(entries, "post_switch_probe") {
		t.Fatal("back on Normal, verbose records must be suppressed")
	}

	if !hasEvent(entries, "connection_start") {
		t.Fatal("operational records must keep flowing after the switch")
	}

	// Unknown profiles are ignored (the current policy stays).
	logger.SetProfile(Profile("bogus"))
	if logger.Profile() != ProfileNormal {
		t.Fatal("unknown profile must not change the policy")
	}
}

// --- Disk behaviour -------------------------------------------------------

func TestDebugProfileRotationAndRetentionStillBound(t *testing.T) {
	dir := t.TempDir()

	logger, err := Open(Options{
		Dir:        dir,
		Name:       "freeiran.log",
		MaxBytes:   8 << 10,
		MaxBackups: 2,
		Profile:    ProfileDebug,
	})
	if err != nil {
		t.Fatal(err)
	}

	defer logger.Close()

	// A debug-profile burst: many verbose records.
	for i := 0; i < 400; i++ {
		logger.Debug("stress", "verbose_record", "filler %04d "+strings.Repeat("x", 64), i)
	}

	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	logFiles := 0

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "freeiran.log") {
			logFiles++
		}
	}

	// 400 × ~100 B ≫ 8 KiB: rotation MUST have happened, and the
	// backup count MUST be bounded (primary + ≤2 backups).
	if logFiles < 2 {
		t.Fatalf("log files = %d, rotation did not run", logFiles)
	}

	if logFiles > 3 {
		t.Fatalf("log files = %d, retention bound violated", logFiles)
	}
}

func TestParseProfileDefaults(t *testing.T) {
	if ParseProfile("") != ProfileNormal {
		t.Fatal("empty settings value must default to Normal")
	}

	if ParseProfile("nonsense") != ProfileNormal {
		t.Fatal("unknown settings value must default to Normal")
	}

	if ParseProfile("detailed") != ProfileDetailed || ParseProfile("debug") != ProfileDebug {
		t.Fatal("known profiles must parse")
	}
}
