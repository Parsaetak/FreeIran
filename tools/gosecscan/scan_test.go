package main

// scan_test.go — regression suite for the v0.10.5 comment-aware
// security scanner.
//
// The suite pins BOTH directions of the contract:
//
//   - documentation (line comments, block comments, inline comments,
//     Hysteria/Hysteria2 protocol examples — the exact shapes that
//     made the v0.10.4 raw grep fail on validate.go:105) is ACCEPTED;
//   - a real hardcoded credential pattern in EXECUTABLE text
//     (string literals, raw strings, identifier assignments) is
//     REJECTED, with the original source line number;
//   - the scan is fail-closed: unscannable input, missing patterns
//     and missing files are operational failures (exit 2), never
//     silent passes.
//
// All credential-looking strings below are SYNTHETIC documentation
// values (example, <pw>, synthetic-*); nothing real appears in this
// repository, and this file is a *_test.go excluded from the product
// scans it supports.

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// scanForTests compiles the patterns case-insensitively (the way the
// workflow invokes the tool) and scans src.
func scanForTests(t *testing.T, src string, patterns ...string) []Violation {
	t.Helper()

	compiled := make([]*regexp.Regexp, 0, len(patterns))

	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			t.Fatalf("pattern %q: %v", p, err)
		}

		compiled = append(compiled, re)
	}

	violations, err := scanSource([]byte(src), "fixture.go", compiled)
	if err != nil {
		t.Fatalf("scanSource: %v", err)
	}

	return violations
}

func requireClean(t *testing.T, name, src string, patterns ...string) {
	t.Helper()

	if v := scanForTests(t, src, patterns...); len(v) != 0 {
		t.Errorf("%s: expected clean scan, got %d violation(s): %v", name, len(v), v)
	}
}

func requireViolation(t *testing.T, name, src, wantLineText string, wantLine int, patterns ...string) {
	t.Helper()

	v := scanForTests(t, src, patterns...)

	if len(v) != 1 {
		t.Fatalf("%s: expected exactly 1 violation, got %d: %v", name, len(v), v)
	}

	if v[0].Line != wantLine {
		t.Errorf("%s: violation on line %d, want line %d", name, v[0].Line, wantLine)
	}

	if !strings.Contains(v[0].Text, wantLineText) {
		t.Errorf("%s: violation text %q does not contain %q", name, v[0].Text, wantLineText)
	}
}

// TestDocumentationCommentsAccepted proves every documentation shape
// that failed (or could fail) the old raw-text scan now passes.
func TestDocumentationCommentsAccepted(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "line comment with credential pattern",
			src: `package main

// password=example
// secret=example
func main() {}
`,
		},
		{
			name: "block comment, interior lines not star-prefixed",
			src: `package main

/*
the raw-text scanner matched interior lines of block comments;
password=example
secret=example
*/
func main() {}
`,
		},
		{
			name: "inline comment after executable code",
			src: `package main

var x = 1 // password=example must not trigger
var y = 2 /* secret=example must not trigger either */
`,
		},
		{
			name: "validate.go:105 Hysteria2 documentation (the v0.10.4 false positive)",
			src: `package config

func validateHysteria(c *Config) error {
        // v0.10.4: the two Hysteria protocols have DIFFERENT obfs
        // semantics and must not share one validation rule.
        //
        //   - Hysteria2: the URI carries obfs=<type> + obfs-password=<pw>;
        //     the type is an ENUMERATED value ("" | salamander | gecko
        //     for sing-box v1.14). v0.10.3 only knew salamander and
        //     rejected the documented gecko.
        return nil
}
`,
		},
		{
			name: "Hysteria / Hysteria2 protocol documentation examples",
			src: `package parser

// Hysteria2 URI form:
//   hysteria2://user:pass@host:443/?obfs=salamander&obfs-password=<pw>&sni=example.com
//
// Hysteria v1 URI form (obfs is a plain string password, not a
// type/password object):
//   hysteria://host:443?auth=secret-example&obfs=obfs-string-value
//
// TUIC URI form (congestion_control is its OWN field, never the
// transport slot):
//   tuic://uuid:password@host:443?congestion_control=bbr&udp_relay_mode=native
func parse(uri string) bool { return true }
`,
		},
		{
			name: "build directive comments",
			src: `//go:build windows

package main

//go:generate stringer -type=Mode
// password=example (directive-adjacent comment)
func main() {}
`,
		},
		{
			name: "identifier assignment with spaces (raw-scan parity: not a hardcoded credential)",
			src: `package main

func f() {
        c.Password = v
}
`,
		},
	}

	patterns := []string{`password=`, `secret=`}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireClean(t, tc.name, tc.src, patterns...)
		})
	}
}

// TestExecutableCredentialRejected proves real findings still fail,
// at the correct original line numbers.
func TestExecutableCredentialRejected(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		lineText string
		line     int
	}{
		{
			name: "interpreted string literal",
			src: `package main

var endpoint = "https://relay.example/?password=synthetic-pass"
`,
			lineText: `password=synthetic-pass`,
			line:     3,
		},
		{
			name: "raw string literal spanning lines",
			src: `package main

var doc = ` + "`" + `usage:
  password=synthetic-raw
` + "`" + `
`,
			lineText: "password=synthetic-raw",
			line:     4,
		},
		{
			name: "identifier assignment without spaces (raw-scan parity)",
			src: `package main

func f() {
        c.Password=v
}
`,
			lineText: "c.Password=v",
			line:     4,
		},
		{
			name: "code after a block comment keeps true line numbers",
			src: `package main

/*
multi-line
documentation block
*/
var leaked = "secret=synthetic-leak"
`,
			lineText: "secret=synthetic-leak",
			line:     7,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireViolation(t, tc.name, tc.src, tc.lineText, tc.line, `password=`, `secret=`)
		})
	}
}

// TestMixedCommentAndCodeOnlyReportsCode proves a line carrying both
// is scanned WITHOUT its comment payload, and executable code on
// later lines is not lost.
func TestMixedCommentAndCodeOnlyReportsCode(t *testing.T) {
	src := `package main

var a = 1 // note: password=example lives only in the comment
var b = "password=synthetic-real"
// trailing password=example comment
var c = 3
`

	v := scanForTests(t, src, `password=`)

	if len(v) != 1 {
		t.Fatalf("expected exactly 1 violation, got %d: %v", len(v), v)
	}

	if v[0].Line != 4 {
		t.Errorf("violation on line %d, want 4 (the string literal, not the comments)", v[0].Line)
	}
}

// TestSpacingTolerantShellInjectionPattern proves the sh -c guard
// rail matches the exec.Command("sh", "-c", ...) call shape regardless
// of source spacing (the old pattern relied on one exact spelling).
func TestSpacingTolerantShellInjectionPattern(t *testing.T) {
	pattern := `exec\.Command\(\s*"sh"\s*,\s*"-c"`

	exact := `package main

import "os/exec"

func f() {
        exec.Command("sh", "-c", script)
}
`

	requireViolation(t, "sh-c exact spelling", exact, `exec.Command("sh", "-c"`, 6, pattern)

	spaced := `package main

import "os/exec"

func g() {
        exec.Command( "sh" , "-c" , script )
}
`

	requireViolation(t, "sh-c odd spacing", spaced, `exec.Command( "sh" , "-c"`, 6, pattern)

	// The command pair inside a plain string that is not the exec
	// call shape is not the injection surface — no false positive.
	doc := `package main

// exec.Command("sh", "-c", ...) is forbidden — see security.yml
const note = "the scanner pattern is exec.Command(sh, -c) shaped"
`

	requireClean(t, "sh-c in documentation", doc, pattern)
}

// TestUnscannableInputFailsClosed proves the scanner refuses to pass
// input it cannot tokenize (a silent pass would be a scanner hole).
func TestUnscannableInputFailsClosed(t *testing.T) {
	broken := "package main\n\nfunc f() { s := \"unterminated\n"

	compiled, err := regexp.Compile(`password=`)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := scanSource([]byte(broken), "broken.go", []*regexp.Regexp{compiled}); err == nil {
		t.Fatal("expected a ScanError for untokenizable input, got nil")
	}
}

// TestRunExitCodes pins the CLI contract: 0 clean, 1 violations,
// 2 operational failure.
func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()

	cleanFile := filepath.Join(dir, "clean.go")
	if err := os.WriteFile(cleanFile, []byte("package main\n\n// password=example\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dirtyFile := filepath.Join(dir, "dirty.go")
	if err := os.WriteFile(dirtyFile, []byte("package main\n\nvar x = \"password=synthetic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	brokenFile := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(brokenFile, []byte("func broken := \"unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("clean file exits 0", func(t *testing.T) {
		var out, errOut bytes.Buffer

		if code := run([]string{"-i", "-e", `password=`, cleanFile}, &out, &errOut); code != 0 {
			t.Errorf("exit %d, want 0 (stderr: %s)", code, errOut.String())
		}
	})

	t.Run("dirty file exits 1 with grep-compatible output", func(t *testing.T) {
		var out, errOut bytes.Buffer

		code := run([]string{"-i", "-e", `password=`, dirtyFile}, &out, &errOut)
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}

		want := dirtyFile + ":3:"
		if !strings.HasPrefix(out.String(), want) {
			t.Errorf("output %q does not start with %q", out.String(), want)
		}
	})

	t.Run("missing pattern exits 2", func(t *testing.T) {
		var out, errOut bytes.Buffer

		if code := run([]string{cleanFile}, &out, &errOut); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
	})

	t.Run("no files exits 2 (never a vacuous pass)", func(t *testing.T) {
		var out, errOut bytes.Buffer

		if code := run([]string{"-e", `password=`}, &out, &errOut); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
	})

	t.Run("unscannable file exits 2 even with violations elsewhere", func(t *testing.T) {
		var out, errOut bytes.Buffer

		code := run([]string{"-i", "-e", `password=`, dirtyFile, brokenFile}, &out, &errOut)
		if code != 2 {
			t.Errorf("exit %d, want 2 (operational failure must dominate)", code)
		}

		if !strings.Contains(errOut.String(), brokenFile) {
			t.Errorf("stderr %q does not name the broken file", errOut.String())
		}
	})
}

// TestRealValidateGoIsAccepted scans the ACTUAL engine/config/
// validate.go — the file whose documentation triggered the v0.10.4
// failure — so the original false positive can never silently return.
func TestRealValidateGoIsAccepted(t *testing.T) {
	const validateGo = "../../engine/config/validate.go"

	src, err := os.ReadFile(validateGo)
	if err != nil {
		t.Fatalf("read %s: %v", validateGo, err)
	}

	requireClean(t, "engine/config/validate.go", string(src), `password=`, `secret=`)
}
