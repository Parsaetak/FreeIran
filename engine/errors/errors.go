// Package errors provides structured, subsystem-aware errors for FreeIran.
//
// Every major subsystem reports errors through Kind so callers (and the
// UI) can distinguish recoverable failures from fatal ones without
// parsing error strings. Errors never embed secrets: call sites must
// avoid formatting credentials into error values.
package errors

import (
	"errors"
	"fmt"
)

// Kind classifies an error by how the application should react to it.
type Kind string

const (
	// KindRecoverable indicates a transient condition the engine can
	// recover from on a later cycle (e.g. one source failed).
	KindRecoverable Kind = "recoverable"

	// KindRetryable indicates the operation may succeed if retried
	// after a delay (e.g. network timeout).
	KindRetryable Kind = "retryable"

	// KindInvalidInput indicates untrusted/invalid input data
	// (e.g. malformed configuration payload).
	KindInvalidInput Kind = "invalid_input"

	// KindConfiguration indicates a local configuration mistake.
	KindConfiguration Kind = "configuration"

	// KindEnvironment indicates a problem with the local environment
	// (permissions, missing directories, OS restrictions).
	KindEnvironment Kind = "environment"

	// KindDependencyUnavailable indicates an optional dependency
	// (protocol core, native acceleration) is not available.
	KindDependencyUnavailable Kind = "dependency_unavailable"

	// KindCorruptData indicates local persisted data failed
	// integrity checks and may need recovery.
	KindCorruptData Kind = "corrupt_data"

	// KindFatal indicates the engine cannot continue this operation
	// and the failure is not expected to resolve by itself.
	KindFatal Kind = "fatal"
)

// Recoverable reports whether the kind is expected to self-heal or be
// retried within normal operation.
func (k Kind) Recoverable() bool {
	switch k {
	case KindRecoverable, KindRetryable, KindDependencyUnavailable:
		return true
	}

	return false
}

// E is the structured error type used across the engine.
type E struct {
	Kind      Kind   `json:"kind"`
	Subsystem string `json:"subsystem"`
	Operation string `json:"operation"`
	Cause     string `json:"cause"`
	// Retryable hints that an explicit retry may help even when the
	// kind alone would not imply it.
	Retryable bool `json:"retryable,omitempty"`

	cause error
}

// Error implements the error interface.
func (e *E) Error() string {
	if e.Retryable {
		return fmt.Sprintf("%s/%s: %s (retryable): %s",
			e.Subsystem, e.Operation, e.Kind, e.Cause)
	}

	return fmt.Sprintf("%s/%s: %s: %s",
		e.Subsystem, e.Operation, e.Kind, e.Cause)
}

// New creates a structured error.
func New(kind Kind, subsystem, operation, format string, args ...any) *E {
	return &E{
		Kind:      kind,
		Subsystem: subsystem,
		Operation: operation,
		Cause:     fmt.Sprintf(format, args...),
	}
}

// Wrap creates a structured error that wraps an underlying error.
// The cause is preserved for errors.Is/As through Unwrap().
func Wrap(
	err error,
	kind Kind,
	subsystem, operation, format string, args ...any,
) *E {
	if err == nil {
		return nil
	}

	e := New(kind, subsystem, operation, format, args...)
	e.cause = err

	return e
}

// Unwrap exposes the wrapped cause.
func (e *E) Unwrap() error {
	return e.cause
}

// IsRetryable reports whether err is a structured error marked retryable.
func IsRetryable(err error) bool {
	var e *E

	if errors.As(err, &e) {
		return e.Retryable || e.Kind == KindRetryable
	}

	return false
}

// KindOf returns the kind of a structured error, or KindFatal for
// unknown errors.
func KindOf(err error) Kind {
	var e *E

	if errors.As(err, &e) {
		return e.Kind
	}

	return KindFatal
}
