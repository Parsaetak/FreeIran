package errors

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorFormatting(t *testing.T) {
	e := New(KindRetryable, "source", "fetch", "source %q timed out", "a")

	want := `source/fetch: retryable: source "a" timed out`
	if e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
}

func TestWrapPreservesCause(t *testing.T) {
	sentinel := errors.New("connection refused")

	e := Wrap(
		fmt.Errorf("do request: %w", sentinel),
		KindRetryable, "source", "fetch", "source %q failed", "s1",
	)

	if !errors.Is(e, sentinel) {
		t.Fatal("errors.Is should find the wrapped sentinel")
	}

	if KindOf(e) != KindRetryable {
		t.Fatalf("KindOf = %v", KindOf(e))
	}
}

func TestKindRecoverable(t *testing.T) {
	cases := map[Kind]bool{
		KindRecoverable:           true,
		KindRetryable:             true,
		KindDependencyUnavailable: true,
		KindFatal:                 false,
		KindCorruptData:           false,
	}

	for kind, want := range cases {
		if kind.Recoverable() != want {
			t.Fatalf("%s.Recoverable() = %v, want %v", kind, !want, want)
		}
	}
}

func TestIsRetryable(t *testing.T) {
	if !IsRetryable(New(KindRetryable, "s", "o", "x")) {
		t.Fatal("retryable kind should be retryable")
	}

	e := New(KindRecoverable, "s", "o", "x")
	e.Retryable = true

	if !IsRetryable(e) {
		t.Fatal("explicit Retryable flag should be honored")
	}

	if IsRetryable(errors.New("plain")) {
		t.Fatal("plain errors are not retryable")
	}
}

func TestKindOfUnknown(t *testing.T) {
	if KindOf(errors.New("x")) != KindFatal {
		t.Fatal("unknown errors should classify as fatal")
	}
}
