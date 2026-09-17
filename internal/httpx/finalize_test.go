package httpx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// finalize_test.go guards the v0.9.6 Windows finalization fix. The
// v0.9.5 implementation opened the .part read-only and called Sync,
// which FlushFileBuffers rejects on Windows (it requires GENERIC_
// WRITE) — every completed download failed with "Access is denied"
// on Windows while Linux stayed green because fsync(2) accepts an
// O_RDONLY descriptor. TestFinalizeCompletedPartDirect exercises the
// production sequence end-to-end on every platform; with the v0.9.5
// code it fails on Windows, which is exactly what the Windows CI
// matrix needs to keep proving.

// withRenameSeam installs a synthetic rename implementation plus a
// zero-cost sleep and restores both afterwards. The returned func is
// NOT used to count calls; tests keep their own counter captured by
// the closure they pass in.
func withRenameSeam(
	t *testing.T,
	renamer func(src, dst string) error,
) func() {
	t.Helper()

	origRename, origWait := osRename, renameWait

	osRename = renamer
	renameWait = func(time.Duration) {}

	return func() {
		osRename, renameWait = origRename, origWait
	}
}

// TestFinalizeCompletedPartDirect is the cross-platform regression
// test for the v0.9.5 Windows CI failure: finalization must succeed
// on a pre-existing .part and move it to the destination. On Windows
// the v0.9.5 read-only-open + Sync failed right here; on POSIX it
// silently passed, which is why the defect escaped.
func TestFinalizeCompletedPartDirect(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "asset.zip.part")
	dest := filepath.Join(dir, "asset.zip")

	payload := []byte("deterministic finalize payload")

	if err := os.WriteFile(part, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := finalizeCompletedPart(part, dest); err != nil {
		t.Fatalf("finalizeCompletedPart: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("destination missing after finalize: %v", err)
	}

	if string(got) != string(payload) {
		t.Fatalf("content changed during finalize: %q", got)
	}

	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf(".part still exists after successful finalize (stat err=%v)", err)
	}
}

// TestFinalizeRetriesSharingViolation proves a transient Windows
// sharing violation is absorbed by the bounded retry: two failed
// attempts followed by success must complete the finalization.
func TestFinalizeRetriesSharingViolation(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "core.zip.part")
	dest := filepath.Join(dir, "core.zip")

	if err := os.WriteFile(part, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-create the destination with older content: a successful
	// replacement must overwrite it.
	if err := os.WriteFile(dest, []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls int

	restore := withRenameSeam(t, func(src, dst string) error {
		calls++

		if calls <= 2 {
			return errStillLocked // transient scanner lock
		}

		return os.Rename(src, dst)
	})
	defer restore()

	if err := finalizeCompletedPart(part, dest); err != nil {
		t.Fatalf("finalize should absorb the transient lock: %v", err)
	}

	if calls != 3 {
		t.Fatalf("rename attempts = %d, want 3", calls)
	}

	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "payload" {
		t.Fatalf("dest = %q (err=%v), want payload", got, err)
	}
}

// TestFinalizeGivesUpAfterBoundedRetries proves the retry is BOUNDED:
// a permanently locked destination fails with a descriptive error,
// the .part file remains recoverable on disk, and the existing
// destination is left untouched (rollback safety).
func TestFinalizeGivesUpAfterBoundedRetries(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "core.zip.part")
	dest := filepath.Join(dir, "core.zip")

	if err := os.WriteFile(part, []byte("new-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dest, []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls int

	restore := withRenameSeam(t, func(src, dst string) error {
		calls++
		return errStillLocked
	})
	defer restore()

	err := finalizeCompletedPart(part, dest)
	if err == nil {
		t.Fatal("permanently locked rename must fail")
	}

	wantAttempts := 1 + len(renameRetryDelays)
	if calls != wantAttempts {
		t.Fatalf("rename attempts = %d, want %d", calls, wantAttempts)
	}

	// The .part stays recoverable for the next Download resume.
	if _, serr := os.Stat(part); serr != nil {
		t.Fatalf(".part must survive a failed finalize: %v", serr)
	}

	got, rerr := os.ReadFile(dest)
	if rerr != nil || string(got) != "OLD" {
		t.Fatalf("existing destination must stay intact: %q (err=%v)", got, rerr)
	}
}

// TestFinalizeDoesNotRetryNonSharingErrors proves only genuine
// sharing violations are retried: a plain failure returns after
// exactly one attempt.
func TestFinalizeDoesNotRetryNonSharingErrors(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "core.zip.part")
	dest := filepath.Join(dir, "core.zip")

	if err := os.WriteFile(part, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls int

	restore := withRenameSeam(t, func(src, dst string) error {
		calls++
		return errors.New("disk on fire")
	})
	defer restore()

	if err := finalizeCompletedPart(part, dest); err == nil {
		t.Fatal("non-transient rename failure must surface")
	}

	if calls != 1 {
		t.Fatalf("rename attempts = %d, want exactly 1 (no retry)", calls)
	}

	if _, serr := os.Stat(part); serr != nil {
		t.Fatalf(".part must survive: %v", serr)
	}
}

// TestRenameAtomicallyImmediateSuccess exercises the success path of
// the retry wrapper itself.
func TestRenameAtomicallyImmediateSuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.part")
	dst := filepath.Join(dir, "a")

	if err := os.WriteFile(src, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := renameAtomically(src, dst); err != nil {
		t.Fatalf("renameAtomically: %v", err)
	}

	if _, serr := os.Stat(dst); serr != nil {
		t.Fatalf("dst missing: %v", serr)
	}
}

// TestFinalizeMissingPartFailsCleanly proves a missing .part produces
// a wrapped error (not a panic and not a silent success).
func TestFinalizeMissingPartFailsCleanly(t *testing.T) {
	dir := t.TempDir()

	err := finalizeCompletedPart(filepath.Join(dir, "ghost.part"), filepath.Join(dir, "ghost"))
	if err == nil {
		t.Fatal("missing .part must fail")
	}
}
