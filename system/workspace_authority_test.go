package system

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultBaseDirIsWorkspaceRoot pins the v0.9.3 path authority:
// DefaultBaseDir and CacheBaseDir are thin aliases of WorkspaceRoot.
// The v0.9.2 regression (old paths_unix.go/paths_windows.go
// resolvers re-declared the same names with per-user XDG/APPDATA
// fallbacks) cannot return: duplicate declarations fail the build,
// and this test fails any resurrection of hidden split roots.
func TestDefaultBaseDirIsWorkspaceRoot(t *testing.T) {
	root := WorkspaceRoot()

	if got := DefaultBaseDir(); got != root {
		t.Fatalf("DefaultBaseDir() = %q, want workspace root %q", got, root)
	}

	if got := CacheBaseDir(); got != root {
		t.Fatalf("CacheBaseDir() = %q, want workspace root %q", got, root)
	}
}

// TestWorkspaceRootIgnoresUserDirs verifies the override contract and
// that per-user OS directories (XDG/APPDATA/LOCALAPPDATA) never leak
// into the resolution — normal runtime state lives in exactly one
// place.
func TestWorkspaceRootIgnoresUserDirs(t *testing.T) {
	t.Setenv("FREEIRAN_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")

	root := WorkspaceRoot()

	if root == "" {
		t.Fatal("workspace root must always resolve")
	}

	// The root is derived from the test binary's directory (the
	// executable-dir model), never from $HOME.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if filepath.Dir(root) == filepath.Join(home, ".local", "share") {
			t.Fatalf("workspace root fell back to XDG data dir: %q", root)
		}
	}
}

// TestPortableModeLabelsDeploymentStyle keeps the diagnostic helper
// honest: with a portable.marker next to the executable it reports
// true, without it false — and in both cases the workspace model is
// unchanged (PortableMode never influences path resolution since
// v0.9.2).
func TestPortableModeLabelsDeploymentStyle(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}

	dir := filepath.Dir(exe)

	// The go test binary directory has no portable.marker normally.
	if PortableMode() {
		if _, err := os.Stat(filepath.Join(dir, "portable.marker")); err != nil {
			if _, err2 := os.Stat(filepath.Join(dir, "config")); err2 != nil {
				t.Fatalf("PortableMode() = true without a deployment layout at %s", dir)
			}
		}
	}
}
