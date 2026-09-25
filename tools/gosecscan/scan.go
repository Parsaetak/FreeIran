// Package main implements gosecscan, the comment-aware suspicious-
// pattern scanner used by .github/workflows/security.yml.
//
// v0.10.5 — WHY THIS TOOL EXISTS
//
// The v0.10.4-and-earlier security scan matched raw text:
//
//	grep -rniE 'password=|secret=' --include='*.go' ./engine ./system
//
// That scan cannot structurally distinguish EXECUTABLE code from
// DOCUMENTATION. It failed on engine/config/validate.go:105, a
// protocol documentation comment describing the Hysteria2 URI form
// (obfs-password=<pw>), while — worse — the line-oriented comment
// filter of the sibling child-process check had the inverse defect:
// inline comments, block-comment interior lines not starting with
// "*", and raw-string lines starting with "//" confused it in both
// directions (false positives AND false negatives).
//
// This tool replaces raw text matching with TOKENIZATION. Go source
// is scanned with go/scanner and the scanned text is reconstructed as
// the ORIGINAL SOURCE with every comment span removed:
//
//   - line comments (//...), general comments (/*...*/) and inline
//     comments after code are structurally EXCLUDED — documentation
//     can never trigger the credential gate again;
//   - the executable text that remains is byte-identical to the
//     original source (spacing, string literals and their verbatim
//     contents included), so the detection domain on executable code
//     is EXACTLY the old raw scan's domain — no weaker, no stronger;
//   - multiline block comments are replaced by the same number of
//     blank lines they occupied, so original line numbering is
//     preserved and reported file:line references match the source;
//   - the scan is FAIL-CLOSED in both failure dimensions: a pattern
//     match in executable text exits 1, and a file that cannot be
//     cleanly tokenized exits 2 (an unscannable file must never pass
//     silently).
package main

import (
	"fmt"
	"go/scanner"
	"go/token"
	"regexp"
	"strings"
)

// Violation is one suspicious-pattern hit found in executable
// (non-comment) text. Line is the ORIGINAL source line number.
type Violation struct {
	File string
	Line int
	Text string
}

// ScanError reports a file that could not be scanned. The scanner is
// fail-closed: such a file must fail the security scan, not pass it.
type ScanError struct {
	File string
	Err  error
}

func (e *ScanError) Error() string {
	return fmt.Sprintf("%s: %v", e.File, e.Err)
}

// stripComments reconstructs src with every comment span removed.
// Comments are replaced by the same number of newlines they occupied
// so line numbers stay true; everything else — spacing, string
// literals, identifiers — is byte-identical to the source.
func stripComments(src []byte, file string) (string, error) {
	fset := token.NewFileSet()
	f := fset.AddFile(file, fset.Base(), len(src))

	var errList scanner.ErrorList
	var s scanner.Scanner
	s.Init(f, src, func(pos token.Position, msg string) {
		errList.Add(pos, msg)
	}, scanner.ScanComments)

	var out strings.Builder
	cursor := 0

	// gap emits the source bytes between the cursor and the start of
	// the current token (whitespace — and newlines — between tokens).
	gap := func(tokStart int) {
		out.Write(src[cursor:tokStart])
		cursor = tokStart
	}

	for {
		pos, tok, lit := s.Scan()

		if tok == token.EOF {
			out.Write(src[cursor:])
			cursor = len(src)

			if len(errList) > 0 {
				return "", &ScanError{File: file, Err: errList.Err()}
			}

			return out.String(), nil
		}

		tokStart := f.Offset(pos)
		gap(tokStart)

		switch tok {
		case token.COMMENT:
			// THE structural exclusion: drop the comment's content,
			// keep the lines it occupied (multline block comments
			// must not shift subsequent line numbers).
			out.WriteString(strings.Repeat("\n", strings.Count(lit, "\n")))
			cursor += len(lit)

		case token.SEMICOLON:
			// An auto-inserted semicolon reports the newline that
			// triggered it — keep that byte so line numbering stays
			// exact. An explicit ";" is punctuation no pattern needs.
			out.WriteString(lit)
			cursor += len(lit)

		default:
			// Executable token: literals (identifiers, string/char/
			// numeric constants — verbatim contents included) carry
			// their source spelling in lit; operators, keywords and
			// punctuation have canonical spellings identical to the
			// source text.
			text := lit
			if text == "" {
				text = tok.String()
			}

			out.WriteString(text)
			cursor += len(text)
		}
	}
}

// scanSource strips comments (structurally, at the token level) and
// matches patterns against the remaining executable text, line by
// line, reporting original line numbers.
func scanSource(src []byte, file string, patterns []*regexp.Regexp) ([]Violation, error) {
	text, err := stripComments(src, file)
	if err != nil {
		return nil, err
	}

	var violations []Violation

	for i, line := range strings.Split(text, "\n") {
		for _, p := range patterns {
			if p.MatchString(line) {
				violations = append(violations, Violation{
					File: file,
					Line: i + 1,
					Text: line,
				})

				break
			}
		}
	}

	return violations, nil
}
