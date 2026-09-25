// gosecscan — the comment-aware suspicious-pattern scanner
// (.github/workflows/security.yml, v0.10.5).
//
// Exit codes (both non-zero exits FAIL the security scan):
//
//	0 — clean: no pattern matched executable token text
//	1 — violations found (file:line:text reported on stdout)
//	2 — operational failure (unscannable file, unreadable file,
//	    missing patterns or missing file list) — fail-closed
//
// Usage:
//
//	gosecscan [-i] -e <pattern> [-e <pattern>...] <file.go> [...]
//
// Patterns are Go regular expressions matched against the raw text
// of every non-comment token of each file, concatenated per line
// exactly as written in the source (comments structurally removed).
// -i enables case-insensitive matching, like grep -i.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
)

const (
	exitClean       = 0
	exitViolations  = 1
	exitOperational = 2
)

// multiFlag collects repeated -e pattern flags.
type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint(*m) }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)

	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gosecscan", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		insensitive = fs.Bool("i", false, "case-insensitive pattern matching")
		patterns    multiFlag
	)

	fs.Var(&patterns, "e", "pattern to match against executable token text (repeatable)")

	if err := fs.Parse(args); err != nil {
		return exitOperational
	}

	if len(patterns) == 0 {
		fmt.Fprintln(stderr, "gosecscan: at least one -e pattern is required")
		return exitOperational
	}

	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "gosecscan: no input files (a scan over nothing must not pass)")
		return exitOperational
	}

	compiled := make([]*regexp.Regexp, 0, len(patterns))

	for _, p := range patterns {
		expr := p

		if *insensitive {
			expr = "(?i)" + expr
		}

		re, err := regexp.Compile(expr)
		if err != nil {
			fmt.Fprintf(stderr, "gosecscan: invalid pattern %q: %v\n", p, err)
			return exitOperational
		}

		compiled = append(compiled, re)
	}

	var (
		allViolations []Violation
		hadFailure    bool
	)

	for _, file := range fs.Args() {
		src, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "gosecscan: %v\n", err)
			hadFailure = true

			continue
		}

		violations, err := scanSource(src, file, compiled)
		if err != nil {
			fmt.Fprintf(stderr, "gosecscan: %v\n", err)
			hadFailure = true

			continue
		}

		allViolations = append(allViolations, violations...)
	}

	for _, v := range allViolations {
		fmt.Fprintf(stdout, "%s:%d:%s\n", v.File, v.Line, v.Text)
	}

	// Operational failures ALWAYS win over findings: the workflow must
	// see that the scan surface was incomplete (fail-closed), even if
	// some files did match.
	if hadFailure {
		return exitOperational
	}

	if len(allViolations) > 0 {
		return exitViolations
	}

	return exitClean
}
