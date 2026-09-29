package version

import (
	"regexp"
	"strings"
	"testing"
)

// TestUserFacingVersionIsCompact is the v0.11.5 regression gate for the
// user-facing version representation: for VERSION=x.y.z the display
// string must render exactly "vx.y.z" — no git commit, no Go runtime
// version, no build tuple. The runtime log (application_start) and every
// user-visible surface render through String()/Display(), so this
// contract is what keeps toolchain metadata out of the product UI.
func TestUserFacingVersionIsCompact(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"0.11.5", "v0.11.5"},
		{"1.0.0", "v1.0.0"},
		{"0.11.5-ci", "v0.11.5-ci"},
	}

	for _, tc := range tests {
		t.Run(tc.version, func(t *testing.T) {
			original := Version
			defer func() { Version = original }()

			Version = tc.version

			if got := Display(); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}

			if got := String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUserFacingVersionCarriesNoMetadata proves the compact contract
// against the CURRENT default build values: neither Display() nor
// String() may contain the commit, a Go runtime token (go1.), or any
// parenthesized build tuple.
func TestUserFacingVersionCarriesNoMetadata(t *testing.T) {
	display := Display()
	str := String()

	for _, out := range []string{display, str} {
		if strings.Contains(out, Commit) && Commit != "dev" {
			t.Errorf("user-facing version %q leaks the commit %q", out, Commit)
		}

		if strings.Contains(out, "go1.") {
			t.Errorf("user-facing version %q leaks the Go runtime version", out)
		}

		if strings.Contains(out, "(") || strings.Contains(out, ")") {
			t.Errorf("user-facing version %q contains a parenthesized build tuple", out)
		}
	}

	if str != Display() {
		t.Errorf("String() = %q must stay identical to Display() = %q", str, display)
	}
}

// TestUserAgentUsesBareVersion pins the HTTP user agent to the bare
// semantic version (no "v" prefix duplication): "FreeIran/0.11.5".
func TestUserAgentUsesBareVersion(t *testing.T) {
	original := Version
	defer func() { Version = original }()

	Version = "0.11.5"

	if got, want := UserAgent(), "FreeIran/0.11.5"; got != want {
		t.Errorf("UserAgent() = %q, want %q", got, want)
	}
}

// TestDisplayMatchesVersionFile proves the display contract against the
// repository VERSION file when running inside a full checkout: VERSION
// is semver-shaped and Display() renders exactly "v" + its content.
func TestDisplayMatchesVersionFile(t *testing.T) {
	root, ok := findRepoRoot(t)
	if !ok {
		t.Skip("not running inside a full repository checkout")
	}

	raw := strings.TrimSpace(readFile(t, root+"/VERSION"))

	if raw == "" {
		t.Fatal("VERSION file is empty")
	}

	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(raw) {
		t.Fatalf("VERSION file content %q is not plain semver", raw)
	}

	if got := Display(); got != "v"+raw {
		t.Errorf("Display() = %q, want %q (from VERSION)", got, "v"+raw)
	}
}
