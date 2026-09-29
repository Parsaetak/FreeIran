package app

import (
	"encoding/json"
	"os"
	"testing"
)

// traysettings_test.go pins the v0.11.3 tray persistence contract
// after the v0.11.4 changes (regression check required by the
// release): the ONE settings path (settings.json) carries
// tray_enabled with nil-pointer migration semantics, true/false both
// survive a save/reload cycle, and the effective value the tray
// reconciler consumes (TrayEnabledOrDefault) never drifts from the
// persisted value.
//
// The NATURAL tray lifecycle itself (icon create/destroy, menu
// checkbox, close-to-tray) lives in the Wails desktop layer
// (cmd/freeiran main.go) and is exercised on the Windows CI surface;
// these tests pin the persistence + resolution state machine the
// layer depends on.

// TestTrayEnabledOrDefaultResolution pins the documented nil-pointer
// migration/default policy: nil (absent key — every pre-0.11.3
// settings file, and a fresh install) resolves ENABLED; explicit
// true resolves true; explicit false resolves false.
func TestTrayEnabledOrDefaultResolution(t *testing.T) {
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{name: "nil (legacy/absent key) resolves enabled", in: nil, want: true},
		{name: "explicit true", in: boolPtr(true), want: true},
		{name: "explicit false", in: boolPtr(false), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Settings{TrayEnabled: tc.in}

			if got := s.TrayEnabledOrDefault(); got != tc.want {
				t.Fatalf("TrayEnabledOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTrayEnabledPersistsTrueAndFalse pins the round-trip contract:
// both explicit values survive a Save → disk → load cycle with no
// drift between the saved return value, the persisted JSON and the
// reloaded in-memory settings.
func TestTrayEnabledPersistsTrueAndFalse(t *testing.T) {
	for _, value := range []bool{true, false} {
		t.Run(map[bool]string{true: "true", false: "false"}[value], func(t *testing.T) {
			application := newTestApp(t)
			service := NewSettingsService(application)

			next := service.Get()
			next.TrayEnabled = boolPtr(value)

			saved, err := service.Save(next)
			if err != nil {
				t.Fatalf("save: %v", err)
			}

			if saved.TrayEnabled == nil || *saved.TrayEnabled != value {
				t.Fatalf("saved value = %+v, want explicit %v (the UI reconciles from this)", saved.TrayEnabled, value)
			}

			if got := saved.TrayEnabledOrDefault(); got != value {
				t.Fatalf("saved effective value = %v, want %v", got, value)
			}

			// The persisted file carries the explicit key.
			raw, err := os.ReadFile(application.settingsPath())
			if err != nil {
				t.Fatalf("read settings file: %v", err)
			}

			var onDisk struct {
				TrayEnabled *bool `json:"tray_enabled"`
			}
			if err := json.Unmarshal(raw, &onDisk); err != nil {
				t.Fatalf("settings file is not valid JSON: %v", err)
			}

			if onDisk.TrayEnabled == nil || *onDisk.TrayEnabled != value {
				t.Fatalf("persisted tray_enabled = %+v, want %v", onDisk.TrayEnabled, value)
			}

			// The reload (next boot) path resolves the same value —
			// no drift between the UI state and the native tray.
			reloaded := application.loadSettings()
			if reloaded.TrayEnabled == nil || *reloaded.TrayEnabled != value {
				t.Fatalf("reloaded tray_enabled = %+v, want %v", reloaded.TrayEnabled, value)
			}

			if got := reloaded.TrayEnabledOrDefault(); got != value {
				t.Fatalf("reloaded effective value = %v, want %v", got, value)
			}
		})
	}
}

// TestTrayEnabledNilOmittedFromDisk pins the migration shape: a nil
// setting is OMITTED from settings.json (omitempty) — the exact
// on-disk shape of every pre-0.11.3 file — and a re-save that leaves
// it nil keeps the file free of the key (the default policy stays
// authoritative, never a written literal).
func TestTrayEnabledNilOmittedFromDisk(t *testing.T) {
	application := newTestApp(t)
	service := NewSettingsService(application)

	next := service.Get()
	next.TrayEnabled = nil

	if _, err := service.Save(next); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(application.settingsPath())
	if err != nil {
		t.Fatalf("read settings file: %v", err)
	}

	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("settings file is not valid JSON: %v", err)
	}

	if _, present := onDisk["tray_enabled"]; present {
		t.Fatalf("nil tray_enabled must be omitted from disk (found key in %s)", raw)
	}

	if got := application.loadSettings().TrayEnabledOrDefault(); got != true {
		t.Fatalf("absent key must resolve to the default (enabled), got %v", got)
	}
}

func boolPtr(v bool) *bool { return &v }
