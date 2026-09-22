package version

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestVersionSurfacesConsistent is the structural regression gate for
// the v0.9.13 CI failure: build/winres.json kept 0.9.12 resource
// metadata while the rest of the tree moved on, so the Windows PE
// resources shipped stale versions. v0.9.14 pins EVERY authoritative
// version surface to the VERSION file — and this test proves it
// without touching the CI script.
//
// The test walks up from the package directory to the repository root
// (identified by go.mod + VERSION) and skips itself when the module is
// consumed outside a full checkout.
func TestVersionSurfacesConsistent(t *testing.T) {
	root, ok := findRepoRoot(t)
	if !ok {
		t.Skip("not running inside a full repository checkout")
	}

	version := strings.TrimSpace(readFile(t, filepath.Join(root, "VERSION")))
	if version == "" {
		t.Fatal("VERSION is empty")
	}

	if version != Version {
		t.Errorf("internal/version.Version = %q, want %q (VERSION)", Version, version)
	}

	// frontend/package.json — the npm package version.
	pkgJSON := readFile(t, filepath.Join(root, "frontend", "package.json"))

	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(pkgJSON), &pkg); err != nil {
		t.Fatalf("frontend/package.json: %v", err)
	}

	if pkg.Version != version {
		t.Errorf("frontend/package.json version = %q, want %q", pkg.Version, version)
	}

	// frontend/package-lock.json — the lockfile must resolve the same
	// package version in both the root and packages[""] entries.
	lockJSON := readFile(t, filepath.Join(root, "frontend", "package-lock.json"))

	var lock struct {
		Version  string `json:"version"`
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal([]byte(lockJSON), &lock); err != nil {
		t.Fatalf("frontend/package-lock.json: %v", err)
	}

	if lock.Version != version {
		t.Errorf("package-lock.json root version = %q, want %q", lock.Version, version)
	}

	if entry, ok := lock.Packages[""]; ok && entry.Version != "" && entry.Version != version {
		t.Errorf("package-lock.json packages[\"\"] version = %q, want %q", entry.Version, version)
	}

	// build/winres.json — every version literal in the Windows PE
	// resource must be the current release (or its four-part form).
	winres := readFile(t, filepath.Join(root, "build", "winres.json"))

	literals := versionLiterals(winres)
	if len(literals) == 0 {
		t.Fatal("build/winres.json carries no version literals")
	}

	fourPart := version + ".0"

	// The PE resource must contain exactly TWO spellings: the release
	// form and its four-part zero-patch expansion — nothing stale.
	for lit := range literals {
		if lit == version || lit == fourPart {
			continue
		}

		t.Errorf("build/winres.json carries stale version %q (want %q or %q only)", lit, version, fourPart)
	}

	if !literals[version] {
		t.Errorf("build/winres.json missing the current release form %q", version)
	}

	if !literals[fourPart] {
		t.Errorf("build/winres.json missing the four-part PE form %q", fourPart)
	}
}

// findRepoRoot walks up from the working directory looking for the
// repository root (go.mod + VERSION + build/winres.json).
func findRepoRoot(t *testing.T) (string, bool) {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}

	for range 8 {
		if fileExistsAt(filepath.Join(dir, "go.mod")) &&
			fileExistsAt(filepath.Join(dir, "VERSION")) &&
			fileExistsAt(filepath.Join(dir, "build", "winres.json")) {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}

		dir = parent
	}

	return "", false
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(raw)
}

func fileExistsAt(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}

// versionLiterals extracts the distinct x.y.z / x.y.z.w literals from
// a resource document — the same extraction the CI gate performs.
func versionLiterals(doc string) map[string]bool {
	re := regexp.MustCompile(`\d+\.\d+\.\d+(?:\.\d+)?`)

	out := make(map[string]bool)

	for _, m := range re.FindAllString(doc, -1) {
		out[m] = true
	}

	return out
}
