package app

// v0.9.13 runtime-logging regression coverage (§1):
//
//   - exactly ONE successful application_start record per launch
//     (the v0.9.12 entrypoint + engine double emission is gone);
//   - the Normal profile carries no boot_telemetry record and no
//     standalone workspace_ready record;
//   - boot timings remain available to diagnostics/state regardless
//     of profile;
//   - Detailed/Debug retain the full boot timing table;
//   - Normal keeps the useful lifecycle records (store_open,
//     application_ready, warmup_complete);
//   - boot failures produce exactly one fatal record, owned by
//     app.New;
//   - structured field redaction still applies.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

// bootWithProfile boots a test app bound to a temp workspace with a
// real runtime logger. The profile is applied AFTER New — the same
// order the settings service uses at runtime (applySettings owns the
// live profile switch, so a pre-set profile would be overwritten by
// the empty saved-settings default).
func bootWithProfile(t *testing.T, profile logging.Profile) (*App, *logging.Logger) {
	t.Helper()

	logDir := filepath.Join(t.TempDir(), "logs")

	logger, err := logging.Open(logging.Options{
		Dir:          logDir,
		Name:         "test.log",
		Profile:      logging.ProfileNormal,
		MirrorStderr: false,
	})
	if err != nil {
		t.Fatalf("open test logger: %v", err)
	}

	application, err := New(Options{
		BaseDir:                 filepath.Join(t.TempDir(), "freeiran"),
		Logger:                  logger,
		RefreshInterval:         time.Hour,
		RunIngestionOnStart:     false,
		SkipDefaultSources:      true,
		SkipConnectVerification: true,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	logger.SetProfile(profile)

	t.Cleanup(application.Shutdown)

	return application, logger
}

func entries(events []logging.Entry, event string) []logging.Entry {
	out := make([]logging.Entry, 0, 2)

	for _, entry := range events {
		if entry.Event == event {
			out = append(out, entry)
		}
	}

	return out
}

func allEntries(logger *logging.Logger) []logging.Entry {
	return logger.Recent(0, 1000, "", "")
}

// TestSingleSuccessfulApplicationStart proves the architectural fix:
// one launch → exactly one successful application_start record, with
// the identity fields the Normal surface needs.
func TestSingleSuccessfulApplicationStart(t *testing.T) {
	application, logger := bootWithProfile(t, logging.ProfileNormal)

	events := allEntries(logger)

	starts := entries(events, "application_start")
	if len(starts) != 1 {
		t.Fatalf("successful application_start count = %d, want 1", len(starts))
	}

	start := starts[0]
	if start.Status == "fatal" {
		t.Fatal("the only application_start record must not be the fatal variant")
	}

	if start.Fields["version"] == "" || start.Fields["commit"] == "" {
		t.Fatalf("application_start missing version/commit fields: %v", start.Fields)
	}

	if start.Fields["base_dir"] == "" {
		t.Fatalf("application_start missing base_dir field: %v", start.Fields)
	}

	// The entrypoint-side duplicate carried the same event with no
	// structured fields; its removal is covered by the count above.
	// The boot itself must still be healthy.
	if state := application.State(); state.Status != "ready" {
		t.Fatalf("status = %s, want ready", state.Status)
	}
}

// TestNormalProfileIsCompact proves the Normal-profile startup shape:
// no boot_telemetry, no workspace_ready, but the useful lifecycle
// records survive.
func TestNormalProfileIsCompact(t *testing.T) {
	application, logger := bootWithProfile(t, logging.ProfileNormal)

	// Drive the warm-up completion synchronously (no 3s warmup window
	// outside Start).
	application.warmCaches()

	events := allEntries(logger)

	if got := entries(events, "boot_telemetry"); len(got) != 0 {
		t.Fatalf("Normal profile emitted boot_telemetry (%d records)", len(got))
	}

	if got := entries(events, "workspace_ready"); len(got) != 0 {
		t.Fatalf("Normal profile emitted workspace_ready (%d records)", len(got))
	}

	for _, event := range []string{"store_open", "application_ready", "warmup_complete"} {
		if got := entries(events, event); len(got) != 1 {
			t.Fatalf("Normal profile %s count = %d, want 1", event, len(got))
		}
	}

	// store_open facts ride structured fields now.
	storeOpen := entries(events, "store_open")[0]
	if _, ok := storeOpen.Fields["records"]; !ok {
		t.Fatalf("store_open missing records field: %v", storeOpen.Fields)
	}
}

// TestBootTimingsRemainAvailable proves the timings survive for
// diagnostics and state even though the Normal log no longer carries
// the telemetry line.
func TestBootTimingsRemainAvailable(t *testing.T) {
	application, _ := bootWithProfile(t, logging.ProfileNormal)

	application.warmCaches()

	timings := application.BootTimings()
	if timings == nil {
		t.Fatal("BootTimings() = nil after warmup")
	}

	for _, phase := range []string{BootBoot, BootWorkspaceReady, BootStoreReady, BootServicesReady, BootReady} {
		if _, ok := timings[phase]; !ok {
			t.Fatalf("boot timing for phase %q missing", phase)
		}
	}

	if state := application.State(); len(state.BootTimings) == 0 {
		t.Fatal("AppState.BootTimings empty — diagnostics would lose startup telemetry")
	}
}

// TestDetailedProfileRetainsBootTelemetry proves Detailed/Debug keep
// the compact structured timing record. The profile becomes active the
// way the runtime settings switch does (after boot); the warmup-time
// boot_telemetry record is then admitted.
func TestDetailedProfileRetainsBootTelemetry(t *testing.T) {
	for _, profile := range []logging.Profile{logging.ProfileDetailed, logging.ProfileDebug} {
		application, logger := bootWithProfile(t, profile)

		application.warmCaches()

		events := allEntries(logger)

		telemetry := entries(events, "boot_telemetry")
		if len(telemetry) != 1 {
			t.Fatalf("%s: boot_telemetry count = %d, want 1", profile, len(telemetry))
		}

		if telemetry[0].Level != logging.LevelDebug {
			t.Fatalf("%s: boot_telemetry level = %s, want debug", profile, telemetry[0].Level)
		}

		timings, ok := telemetry[0].Fields["timings"].(map[string]any)
		if !ok || len(timings) == 0 {
			t.Fatalf("%s: boot_telemetry timings field missing/empty: %v", profile, telemetry[0].Fields)
		}

		// The compact Normal-profile summary stays present too — the
		// detailed profile is a superset of Normal.
		if got := entries(events, "warmup_complete"); len(got) != 1 {
			t.Fatalf("%s: warmup_complete count = %d, want 1", profile, len(got))
		}
	}
}

// TestWorkspaceReadyFollowsOpenProfile pins the emission-time profile
// contract for boot records: a logger opened as Detailed emits the
// workspace_ready lifecycle record (DebugLifecycle) during boot —
// Normal never does. (Boot-time records precede the saved-settings
// profile application, so the open-time profile governs them.)
func TestWorkspaceReadyFollowsOpenProfile(t *testing.T) {
	for _, tc := range []struct {
		profile logging.Profile
		want    int
	}{{logging.ProfileNormal, 0}, {logging.ProfileDetailed, 1}} {
		logDir := filepath.Join(t.TempDir(), "logs")

		logger, err := logging.Open(logging.Options{
			Dir:     logDir,
			Name:    "test.log",
			Profile: tc.profile,
		})
		if err != nil {
			t.Fatalf("open test logger: %v", err)
		}

		application, err := New(Options{
			BaseDir:                 filepath.Join(t.TempDir(), "freeiran"),
			Logger:                  logger,
			RefreshInterval:         time.Hour,
			RunIngestionOnStart:     false,
			SkipDefaultSources:      true,
			SkipConnectVerification: true,
		})
		if err != nil {
			t.Fatalf("new app: %v", err)
		}

		got := len(entries(allEntries(logger), "workspace_ready"))
		if got != tc.want {
			t.Fatalf("%s: workspace_ready count = %d, want %d", tc.profile, got, tc.want)
		}

		application.Shutdown()
	}
}

// TestBootFailureSingleFatalRecord proves boot-failure ownership: a
// failed boot leaves exactly one fatal application_start record and
// never a successful one.
func TestBootFailureSingleFatalRecord(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")

	logger, err := logging.Open(logging.Options{
		Dir:     logDir,
		Name:    "test.log",
		Profile: logging.ProfileNormal,
	})
	if err != nil {
		t.Fatalf("open test logger: %v", err)
	}

	// A FILE where the workspace root must be created: EnsureLayout
	// fails before any other subsystem starts.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	if _, err := New(Options{
		BaseDir: blocker,
		Logger:  logger,
	}); err == nil {
		t.Fatal("app.New must fail when the base dir is a file")
	}

	events := allEntries(logger)

	starts := entries(events, "application_start")
	if len(starts) != 1 {
		t.Fatalf("application_start records after failed boot = %d, want exactly 1 fatal", len(starts))
	}

	// Logger.Error maps (operation, error kind) — the fatal variant is
	// operation=boot / error_kind=fatal.
	if starts[0].ErrorKind != "fatal" || starts[0].Operation != "boot" {
		t.Fatalf("failed-boot record op/kind = %q/%q, want boot/fatal",
			starts[0].Operation, starts[0].ErrorKind)
	}
}

// TestApplicationStartFieldsRedacted guards the redaction invariant on
// the new structured records: secret-shaped field keys never store raw
// values (engine/app.go relies on the logging package for this).
func TestApplicationStartFieldsRedacted(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")

	logger, err := logging.Open(logging.Options{
		Dir:     logDir,
		Name:    "test.log",
		Profile: logging.ProfileNormal,
	})
	if err != nil {
		t.Fatalf("open test logger: %v", err)
	}

	logger.Log(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: "app",
		Event:     "application_start",
		Message:   "field redaction probe",
		Fields: map[string]any{
			"password":     "super-secret-value",
			"api_token":    "token-value-123",
			"base_dir":     "/home/user/FreeIran",
			"endpoint_url": "vless://11111111-2222-3333-4444-555555555555@host:443",
		},
	})

	events := allEntries(logger)

	if len(events) != 1 {
		t.Fatalf("record count = %d, want 1", len(events))
	}

	fields := events[0].Fields

	if fields["password"] != "[REDACTED]" || fields["api_token"] != "[REDACTED]" {
		t.Fatalf("secret-shaped fields not redacted: %v", fields)
	}

	if fields["base_dir"] != "/home/user/FreeIran" {
		t.Fatalf("non-secret field must pass through: %v", fields["base_dir"])
	}

	if url, ok := fields["endpoint_url"].(string); ok && url != "vless://[REDACTED]" {
		t.Fatalf("credential-bearing URL not redacted: %q", url)
	}
}
