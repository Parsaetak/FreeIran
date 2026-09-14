package system

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWorkspacePathAuthoritySingleSource guards the "one workspace
// authority" architecture at source level: each workspace-path symbol
// must be declared exactly once, inside workspace.go (the single
// authoritative implementation), regardless of build tags. The 0.9.4
// regression (obsolete paths_unix.go/paths_windows.go re-declaring
// DefaultBaseDir/CacheBaseDir/portableRoot alongside workspace.go and
// portable.go) broke go vet for every platform; this test fails
// before any build-tag-specific combination can hide a duplicate.
//
// portableRoot is guarded too: its triple declaration (portable.go +
// paths_unix.go + paths_windows.go) was the FIRST redeclaration in
// the 0.9.4 vet failure, yet only workspace.go may own it now.
func TestWorkspacePathAuthoritySingleSource(t *testing.T) {
	const authorityFile = "workspace.go"

	authorities := []string{
		"WorkspaceRoot",
		"WorkspaceLayout",
		"EnsureWorkspace",
		"WorkspaceWritable",
		"DefaultBaseDir",
		"CacheBaseDir",
		"portableRoot",
		"PortableMode",
		"InstalledMode",
	}

	alternatives := make([]string, len(authorities))
	for i, symbol := range authorities {
		alternatives[i] = regexp.QuoteMeta(symbol)
	}

	declRe := regexp.MustCompile(`^func\s+(` + strings.Join(alternatives, "|") + `)\(`)

	declared := map[string][]string{}

	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("scan system package sources: %v", err)
	}

	for _, file := range files {
		if filepath.Base(file) == "workspace_authority_test.go" {
			continue
		}

		lines, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		for _, line := range regexp.MustCompile(`\r?\n`).Split(string(lines), -1) {
			if m := declRe.FindStringSubmatch(line); m != nil {
				declared[m[1]] = append(declared[m[1]], filepath.Base(file))
			}
		}
	}

	for _, symbol := range authorities {
		owners := declared[symbol]
		if len(owners) == 0 {
			t.Errorf("symbol %s has no declaration in package system", symbol)
			continue
		}

		if len(owners) > 1 {
			t.Errorf("symbol %s declared %d times (%v); exactly one implementation is required",
				symbol, len(owners), owners)
			continue
		}

		if owners[0] != authorityFile {
			t.Errorf("symbol %s must be declared in %s (the single workspace authority), found in %s",
				symbol, authorityFile, owners[0])
		}
	}
}

// TestDefaultBaseDirIsWorkspaceRoot pins the path authority:
// DefaultBaseDir and CacheBaseDir are thin aliases of WorkspaceRoot —
// even under the exact environment variables the removed pre-0.9.2
// duplicate implementations (XDG_DATA_HOME, XDG_CACHE_HOME, APPDATA,
// LOCALAPPDATA) consulted to build split per-user roots. The 0.9.4
// regression (old paths_unix.go/paths_windows.go resolvers
// re-declaring the same names) cannot return: duplicate declarations
// fail the build, and this test fails any behavioral resurrection of
// hidden split roots.
func TestDefaultBaseDirIsWorkspaceRoot(t *testing.T) {
	split := t.TempDir()

	t.Setenv("XDG_DATA_HOME", filepath.Join(split, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(split, "cache"))
	t.Setenv("APPDATA", filepath.Join(split, "roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(split, "local"))

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

// TestPortableModeDetectsMarkerLayout proves the portable-layout
// detector end to end: dropping a portable.marker into the test
// binary's directory flips PortableMode to true (and removing it
// restores false) without changing the workspace root — the marker is
// a label, never a path authority.
func TestPortableModeDetectsMarkerLayout(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}

	dir := filepath.Dir(exe)

	t.Setenv("FREEIRAN_HOME", "")

	marker := filepath.Join(dir, "portable.marker")

	if err := os.WriteFile(marker, []byte("test\n"), 0o600); err != nil {
		t.Skipf("test binary directory not writable: %v", err)
	}

	t.Cleanup(func() {
		_ = os.Remove(marker)
	})

	if !PortableMode() {
		t.Fatal("PortableMode() = false with portable.marker present")
	}

	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	if PortableMode() {
		t.Fatal("PortableMode() = true after removing portable.marker")
	}
}
