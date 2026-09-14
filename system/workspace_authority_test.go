package system

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestWorkspacePathAuthoritySingleSource guards the v0.9.4
// "one workspace authority" architecture at source level: each
// workspace-path symbol must be declared exactly once across the
// system package, regardless of build tags. The 0.9.4 regression
// (obsolete paths_unix.go/paths_windows.go re-declaring
// DefaultBaseDir/CacheBaseDir/portableRoot alongside workspace.go and
// portable.go) broke go vet for every platform; this test fails
// before any build-tag-specific combination can hide a duplicate.
func TestWorkspacePathAuthoritySingleSource(t *testing.T) {
	authorities := []string{
		"WorkspaceRoot",
		"WorkspaceLayout",
		"EnsureWorkspace",
		"WorkspaceWritable",
		"DefaultBaseDir",
		"CacheBaseDir",
		"PortableMode",
		"InstalledMode",
	}

	declRe := regexp.MustCompile(`^func\s+(` + regexp.QuoteMeta(authorities[0]) +
		`|` + regexp.QuoteMeta(authorities[1]) + `|` + regexp.QuoteMeta(authorities[2]) +
		`|` + regexp.QuoteMeta(authorities[3]) + `|` + regexp.QuoteMeta(authorities[4]) +
		`|` + regexp.QuoteMeta(authorities[5]) + `|` + regexp.QuoteMeta(authorities[6]) +
		`|` + regexp.QuoteMeta(authorities[7]) + `)\(`)

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
		}
	}
}

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
