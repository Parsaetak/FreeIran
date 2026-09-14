package system

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetWorkspaceCache clears the memoized workspace root between
// tests so each test resolves its own environment.
func resetWorkspaceCache(root string) {
	cachedWorkspaceRoot.Store(root)
}

func setEnv(t *testing.T, key, value string) {
	t.Helper()

	old, had := os.LookupEnv(key)

	if value == "" {
		_ = os.Unsetenv(key)
	} else {
		_ = os.Setenv(key, value)
	}

	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// TestWorkspaceRootOverride verifies FREEIRAN_HOME relocates the root
// deterministically.
func TestWorkspaceRootOverride(t *testing.T) {
	setEnv(t, workspaceOverrideEnv, "")

	want := filepath.Dir(exePathOrSkip(t))
	got := WorkspaceRoot()
	if got != want {
		resetWorkspaceCache("")

		t.Fatalf("WorkspaceRoot() = %q, want exe dir %q", got, want)
	}

	custom := filepath.Join(t.TempDir(), "custom-home")
	setEnv(t, workspaceOverrideEnv, custom)

	resetWorkspaceCache("")

	defer resetWorkspaceCache("")

	if got := WorkspaceRoot(); got != custom {
		t.Fatalf("override WorkspaceRoot() = %q, want %q", got, custom)
	}

	layout := WorkspaceLayout()
	if layout.Root != custom || layout.Runtime != filepath.Join(custom, "runtime") {
		t.Fatalf("layout mismatch: %+v", layout)
	}
}

// TestEnsureWorkspace verifies the directory tree is created and the
// writable probe passes on a fresh root.
func TestEnsureWorkspace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "FreeIran")

	setEnv(t, workspaceOverrideEnv, root)
	resetWorkspaceCache("")
	defer resetWorkspaceCache("")

	layout, err := EnsureWorkspace()
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	for _, dir := range []string{
		layout.Root, layout.Data, layout.Cache, layout.Logs,
		layout.Cores, layout.Config, layout.Runtime,
	} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("directory %s missing after EnsureWorkspace", dir)
		}
	}

	if err := WorkspaceWritable(root); err != nil {
		t.Fatalf("writable probe failed: %v", err)
	}

	// No leaked probe files.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".workspace-probe-") {
			t.Fatalf("probe file leaked: %s", entry.Name())
		}
	}
}

// TestDetectLegacyWorkspace verifies discovery of pre-0.9.2 data in
// the old per-user locations.
func TestDetectLegacyWorkspace(t *testing.T) {
	setEnv(t, "APPDATA", "")
	setEnv(t, "LOCALAPPDATA", "")

	home := t.TempDir()
	setEnv(t, "USERPROFILE", home)
	setEnv(t, "HOME", home)

	xdgData := filepath.Join(home, ".local", "share")
	legacy := filepath.Join(xdgData, "FreeIran")

	// No data yet.
	if _, found := DetectLegacyWorkspace(); found {
		t.Fatal("detected legacy data where none exists")
	}

	// Seed a legacy store.
	if err := os.MkdirAll(filepath.Join(legacy, "data"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(legacy, "data", "store.meta"),
		[]byte(`{"version":1}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// DetectLegacyWorkspace consults LegacyLocations, which on Linux
	// walks the $HOME candidates.
	resetWorkspaceCache("")
	defer resetWorkspaceCache("")

	// LegacyLocations uses UserHomeDir (HOME on Unix).
	found, ok := DetectLegacyWorkspace()
	if !ok {
		t.Fatal("legacy data not detected")
	}

	if filepath.Clean(found) != filepath.Clean(legacy) {
		t.Fatalf("detected %q, want %q", found, legacy)
	}

	_ = xdgData
}

// TestMigrateLegacyWorkspace verifies the full copy → verify → record
// lifecycle and that the source is preserved untouched.
func TestMigrateLegacyWorkspace(t *testing.T) {
	source := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "FreeIran")

	setEnv(t, workspaceOverrideEnv, workspace)
	resetWorkspaceCache("")
	defer resetWorkspaceCache("")

	// Seed a realistic legacy tree.
	for _, rel := range []string{
		"config/sources.json",
		"data/store.meta",
		"data/chunks/000000.firc",
		"data/wal/seg-00000000.wal",
		"cores/xray/bin/xray",
		"cache/entries.bin",
		"logs/freeiran.log", // logs are NOT migrated
	} {
		path := filepath.Join(source, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(path, []byte("payload:"+rel), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	report, err := MigrateLegacyWorkspace(source, nil)
	if err != nil {
		t.Fatalf("MigrateLegacyWorkspace: %v", err)
	}

	if !report.Performed {
		t.Fatal("migration not performed")
	}

	if report.Files != 6 {
		t.Fatalf("files = %d, want 6 (logs excluded)", report.Files)
	}

	// Copied bytes are correct.
	raw, err := os.ReadFile(filepath.Join(workspace, "data", "chunks", "000000.firc"))
	if err != nil {
		t.Fatalf("read copied chunk: %v", err)
	}

	if !strings.HasPrefix(string(raw), "payload:data/") {
		t.Fatalf("copied content wrong: %q", raw)
	}

	// Logs stay behind.
	if _, err := os.Stat(filepath.Join(workspace, "logs", "freeiran.log")); !os.IsNotExist(err) {
		t.Fatal("logs must not be migrated")
	}

	// Source preserved byte-for-byte.
	original, err := os.ReadFile(filepath.Join(source, "config", "sources.json"))
	if err != nil || string(original) != "payload:config/sources.json" {
		t.Fatalf("source not preserved: %v %q", err, original)
	}

	// Status recorded.
	status := LoadWorkspaceStatus()
	if !status.Migrated || status.Source != source || status.Files != 6 {
		t.Fatalf("status wrong: %+v", status)
	}
}

// TestEnsureWorkspaceMigratedDeterministic verifies the boot-time gate:
// fresh workspace + legacy data in a per-user location → migrate;
// second call → skip (never duplicate the dataset).
func TestEnsureWorkspaceMigratedDeterministic(t *testing.T) {
	setEnv(t, "APPDATA", "")
	setEnv(t, "LOCALAPPDATA", "")

	home := t.TempDir()
	setEnv(t, "USERPROFILE", home)
	setEnv(t, "HOME", home)

	workspace := filepath.Join(t.TempDir(), "FreeIran")

	setEnv(t, workspaceOverrideEnv, workspace)
	setEnv(t, skipMigrationEnv, "")
	resetWorkspaceCache("")
	defer resetWorkspaceCache("")

	legacy := filepath.Join(home, ".local", "share", "FreeIran")

	if err := os.MkdirAll(filepath.Join(legacy, "config"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(legacy, "config", "sources.json"),
		[]byte(`{"version":1}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	report, err := EnsureWorkspaceMigrated(nil)
	if err != nil {
		t.Fatalf("first migration: %v", err)
	}

	if !report.Performed {
		t.Fatal("first migration should run on a fresh workspace")
	}

	// Second boot: the workspace now holds authoritative data.
	report2, err := EnsureWorkspaceMigrated(nil)
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}

	if report2.Performed {
		t.Fatal("second migration must not run (never duplicate the dataset)")
	}

	if report2.Reason == "" {
		t.Fatal("skip reason must be recorded")
	}

	// Status file is valid JSON.
	raw, err := os.ReadFile(filepath.Join(workspace, "config", "workspace.json"))
	if err != nil {
		t.Fatalf("status file missing: %v", err)
	}

	var status WorkspaceStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("status file not valid JSON: %v", err)
	}
}

// TestSkipMigrationEnv verifies the automated-deployment kill switch.
func TestSkipMigrationEnv(t *testing.T) {
	setEnv(t, workspaceOverrideEnv, filepath.Join(t.TempDir(), "FreeIran"))
	setEnv(t, skipMigrationEnv, "1")
	resetWorkspaceCache("")
	defer resetWorkspaceCache("")

	report, err := EnsureWorkspaceMigrated(nil)
	if err != nil {
		t.Fatalf("EnsureWorkspaceMigrated: %v", err)
	}

	if report.Performed {
		t.Fatal("migration must be skipped with FREEIRAN_SKIP_MIGRATION=1")
	}
}

func exePathOrSkip(t *testing.T) string {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable")
	}

	return exe
}
