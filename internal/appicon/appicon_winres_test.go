package appicon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCanonicalIconFamily is the cross-platform (non-Windows too)
// contract of the ONE canonical icon authority (v0.13.1).
//
// The Windows desktop-icon defect of v0.13.0 was a resource-LAYOUT
// bug: the executable's only icon group was stored under the
// resource NAME "APP", while the pinned Wails v3.0.0-beta.19 loads
// the window icon through LoadIconW(exe, MAKEINTRESOURCE(3)) — a
// NUMERIC ID 3 lookup that a named group can never satisfy. This
// test pins the source of that layout (build/winres.json) so the
// regression is caught on every platform, before any Windows build
// exists:
//
//   - the icon group is declared under the numeric key "#3" (the
//     only spelling go-winres turns into a numeric resource ID);
//   - it is the ONLY icon group declared;
//   - it points at the canonical assets/freeiran-icon.ico;
//   - the embedded runtime PNG and the canonical PNG are the same
//     bytes (one icon family: executable, window, tray).
//
// The PE-level consequence (numeric ID 3 actually linked into the
// built executable) is asserted by TestWindowsGUIIcon on Windows
// with FREEIRAN_GUI_EXE set — this test is its always-runnable
// source-level companion.
func TestCanonicalIconFamily(t *testing.T) {
	root, ok := findRepoRoot(t)
	if !ok {
		t.Skip("repository root not found; skipping icon-source contract")
	}

	winresPath := filepath.Join(root, "build", "winres.json")
	raw, err := os.ReadFile(winresPath)
	if err != nil {
		t.Fatalf("read build/winres.json: %v", err)
	}

	var doc struct {
		RTGroupIcon map[string]map[string]string `json:"RT_GROUP_ICON"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse build/winres.json: %v", err)
	}

	if len(doc.RTGroupIcon) == 0 {
		t.Fatal("build/winres.json declares no RT_GROUP_ICON")
	}

	for key, langs := range doc.RTGroupIcon {
		if key != "#3" {
			t.Fatalf("build/winres.json registers the icon group as %q; "+
				"want the numeric key \"#3\" (Wails v3.0.0-beta.19 loads "+
				"LoadIconW(exe, MAKEINTRESOURCE(3)); a named group such as "+
				"the v0.13.0 \"APP\" key is invisible to that lookup and "+
				"ships a broken window/taskbar icon)", key)
		}

		for lang, target := range langs {
			if !strings.HasSuffix(target, "assets/freeiran-icon.ico") {
				t.Fatalf("build/winres.json icon group %q/%q points at %q; "+
					"want the canonical assets/freeiran-icon.ico", key, lang, target)
			}
		}
	}

	// One icon family: the embedded runtime PNG and the canonical
	// asset PNG must be the same bytes (regenerate with
	// scripts/genicon.py if they drift).
	canonicalPNG, err := os.ReadFile(filepath.Join(root, "assets", "freeiran-icon.png"))
	if err != nil {
		t.Fatalf("read canonical PNG: %v", err)
	}

	if string(PNG) != string(canonicalPNG) {
		t.Fatal("internal/appicon/freeiran-icon.png differs from " +
			"assets/freeiran-icon.png — the embedded window/tray icon and " +
			"the canonical asset must be the same bytes (run scripts/genicon.py)")
	}

	// The canonical ICO must carry the multi-size family (>=5
	// images); a single-image ICO would make every shell surface
	// render a scaled 256px bitmap.
	ico, err := os.ReadFile(filepath.Join(root, "assets", "freeiran-icon.ico"))
	if err != nil {
		t.Fatalf("read canonical ICO: %v", err)
	}

	if len(ico) < 6 {
		t.Fatal("canonical ICO is truncated")
	}

	if count := int(ico[4]) | int(ico[5])<<8; count < 5 {
		t.Fatalf("canonical ICO carries %d images, want the multi-size family (>=5)", count)
	}
}

// findRepoRoot walks up from the working directory to the repository
// root (the same markers the version-consistency checks use).
func findRepoRoot(t *testing.T) (string, bool) {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}

	for range 6 {
		if fileExists(filepath.Join(dir, "go.mod")) &&
			fileExists(filepath.Join(dir, "build", "winres.json")) &&
			fileExists(filepath.Join(dir, "assets", "freeiran-icon.ico")) {
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

func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}
