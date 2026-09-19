package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

// loggingservice_profile_test.go — v0.9.8.4 settings-surface
// regression tests for the logging profiles: validation, persistence
// (survives an app restart — no code change required to pick it up)
// and runtime switching of the live logger without a restart.

func TestSettingsLoggingProfileValidation(t *testing.T) {
	application := newTestApp(t)

	service := NewSettingsService(application)

	base := service.Get()

	for _, good := range []string{"", "normal", "detailed", "debug"} {
		next := base
		next.LoggingProfile = good

		if _, err := service.Save(next); err != nil {
			t.Fatalf("profile %q must be accepted: %v", good, err)
		}
	}

	next := base
	next.LoggingProfile = "chatty"

	if _, err := service.Save(next); err == nil {
		t.Fatal("unknown logging profile must be rejected")
	}
}

func TestSettingsLoggingProfilePersistsAndApplies(t *testing.T) {
	application := newTestApp(t)

	service := NewSettingsService(application)

	next := service.Get()
	next.LoggingProfile = "detailed"

	saved, err := service.Save(next)
	if err != nil {
		t.Fatal(err)
	}

	if saved.LoggingProfile != "detailed" {
		t.Fatalf("saved profile = %q, want detailed", saved.LoggingProfile)
	}

	// The live logger switched immediately (no restart).
	if got := application.logger.Profile(); got != logging.ProfileDetailed {
		t.Fatalf("live logger profile = %q, want detailed", got)
	}

	// The profile is persisted with the existing settings file and
	// applied again on a fresh boot (persistence contract).
	raw, err := os.ReadFile(filepath.Join(application.opts.BaseDir, "config", "settings.json"))
	if err != nil {
		t.Fatalf("settings file must exist: %v", err)
	}

	if !strings.Contains(string(raw), `"logging_profile": "detailed"`) {
		t.Fatalf("settings file must carry the profile, got: %s", raw)
	}
}

func TestSettingsLoggingProfileRuntimeSwitchCycle(t *testing.T) {
	application := newTestApp(t)

	service := NewSettingsService(application)

	cycle := []string{"normal", "detailed", "debug", "normal"}

	for _, want := range cycle {
		next := service.Get()
		next.LoggingProfile = want

		if _, err := service.Save(next); err != nil {
			t.Fatalf("save %q: %v", want, err)
		}

		if got := application.logger.Profile(); string(got) != want {
			t.Fatalf("live profile = %q, want %q (switch must apply without restart)", got, want)
		}
	}
}
