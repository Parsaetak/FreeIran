package httpx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// finalize.go owns the atomic activation point of every finished
// download: syncing the staged .part file and renaming it over the
// destination.
//
// v0.9.6 root-cause fix for the v0.9.5 Windows CI regression. The
// v0.9.5 implementation opened the .part file with os.Open — a
// read-only (GENERIC_READ) handle — and then called f.Sync(). On
// Windows, File.Sync maps to FlushFileBuffers, which requires the
// handle to be opened with GENERIC_WRITE; a read-only handle fails
// with ERROR_ACCESS_DENIED. Every completed download therefore failed
// during finalization on Windows ("httpx: finalize sync: ... Access
// is denied."), which surfaced downstream through the core manager
// as dependency_unavailable: <core>: download. On POSIX systems
// fsync(2) on an O_RDONLY descriptor succeeds, which is exactly why
// the Linux CI matrix stayed green while the Windows matrix failed.
//
// The corrected sequence, with the invariants the task demands:
//
//  1. open the .part read-write (the write access FlushFileBuffers
//     requires on Windows);
//  2. Sync — the durable-content guarantee before activation;
//  3. Close — the descriptor is released BEFORE the rename: no
//     goroutine of ours may retain the file while it is replaced;
//  4. rename with a bounded, genuinely-appropriate retry for Windows
//     sharing violations only (antivirus/indexer scanners that hold
//     a freshly-synced file for a few hundred milliseconds).
//
// Failure semantics: if any step fails the .part file is left intact
// on disk (a later Download resumes from it), and the existing
// destination — if any — was never touched. Rollback safety and
// checksum/transactional semantics live in the callers; finalization
// only ever replaces the destination after the .part is fully synced.

// renameRetryDelays bounds how long finalization waits on a Windows
// sharing violation before giving up: one immediate attempt plus
// these five backoff sleeps (~775 ms total worst case). A scanner
// that holds the file longer than that is not transient, and the
// error is reported to the caller instead of being masked.
var renameRetryDelays = []time.Duration{
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
}

// Seams so tests can simulate Windows sharing violations and count
// attempts without sleeping. Production behaviour is os.Rename +
// time.Sleep.
var (
	osRename   = os.Rename
	renameWait = time.Sleep
)

// finalizeCompletedPart syncs the .part file and renames it into
// place (the atomic activation point of a finished download).
func finalizeCompletedPart(part, dest string) error {
	// O_RDWR — not os.Open's read-only mode — because Sync on Windows
	// requires a write-capable handle (see the package comment).
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("httpx: finalize: %w", err)
	}

	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		return fmt.Errorf("httpx: finalize sync: %w", serr)
	}

	// The descriptor must be closed before the rename: on Windows a
	// MoveFileEx over a file this process still holds open fails, and
	// on POSIX a leaked descriptor would keep the .part busy for
	// whoever cleans up after a failed activation.
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("httpx: finalize close: %w", cerr)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("httpx: finalize mkdir: %w", err)
	}

	if err := renameAtomically(part, dest); err != nil {
		return fmt.Errorf("httpx: finalize rename: %w", err)
	}

	return nil
}

// renameAtomically renames src over dst, retrying ONLY on errors that
// classify as a transient Windows sharing violation. A non-retryable
// error returns immediately — the .part stays recoverable and the
// existing destination stays untouched.
func renameAtomically(src, dst string) error {
	maxAttempts := 1 + len(renameRetryDelays)

	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			renameWait(renameRetryDelays[attempt-2])
		}

		err := osRename(src, dst)
		if err == nil {
			return nil
		}

		lastErr = err

		if !classifyRenameRetryable(err) {
			return err
		}
	}

	return fmt.Errorf("destination still locked after %d attempts: %w", maxAttempts, lastErr)
}

// errStillLocked marks the synthesized sharing-violation used by the
// unit tests on non-Windows platforms. Real Windows sharing
// violations arrive as syscall.Errno; the classifier on other systems
// accepts only this sentinel so production retries stay off.
var errStillLocked = errors.New("file in use by another process")
