// workspace_installed_test.go verifies the v0.9.4 installed-deployment
// model: installed.marker (written by the Windows installer) relocates
// the workspace to the per-user application-data directory, while
// portable/default deployments keep the executable directory.
package system

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInstalledMarkerRelocatesWorkspace proves the resolution order:
// FREEIRAN_HOME > installed.marker (per-user data dir) > executable
// directory. The marker is created in the test binary's own directory
// (writable on every platform CI runs on) and removed afterwards.
func TestInstalledMarkerRelocatesWorkspace(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	exeDir := filepath.Dir(exe)

	// Deterministic per-user base: point XDG_CONFIG_HOME (Unix) /
	// fake APPDATA-independent UserConfigDir at a temp dir.
	tmpBase := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpBase)
	t.Setenv("APPDATA", tmpBase)
	t.Setenv("FREEIRAN_HOME", "")

	marker := filepath.Join(exeDir, installedMarkerName)

	if err := os.WriteFile(marker, []byte("test\n"), 0o600); err != nil {
		t.Skipf("test binary directory not writable: %v", err)
	}

	t.Cleanup(func() {
		_ = os.Remove(marker)
		resetWorkspaceCache("")
	})

	resetWorkspaceCache("")

	root := WorkspaceRoot()
	want := filepath.Join(tmpBase, "FreeIran")

	if root != want {
		t.Fatalf("installed WorkspaceRoot() = %q, want %q", root, want)
	}

	if !InstalledMode() {
		t.Fatal("InstalledMode() = false with installed.marker present")
	}

	// Removing the marker restores the portable model (exe dir).
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	resetWorkspaceCache("")

	if got := WorkspaceRoot(); got != exeDir {
		t.Fatalf("portable WorkspaceRoot() = %q, want exe dir %q", got, exeDir)
	}

	if InstalledMode() {
		t.Fatal("InstalledMode() = true without installed.marker")
	}
}

// TestPortableDeploymentKeepsExecutableDir proves the default model
// is untouched: no marker, no override → the executable directory.
func TestPortableDeploymentKeepsExecutableDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	exeDir := filepath.Dir(exe)

	t.Setenv("FREEIRAN_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	// Defensive: no marker may linger from an interrupted sibling test.
	_ = os.Remove(filepath.Join(exeDir, installedMarkerName))

	resetWorkspaceCache("")
	defer resetWorkspaceCache("")

	if got := WorkspaceRoot(); got != exeDir {
		t.Fatalf("WorkspaceRoot() = %q, want %q", got, exeDir)
	}

	if InstalledMode() {
		t.Fatal("InstalledMode() = true in a portable deployment")
	}
}
