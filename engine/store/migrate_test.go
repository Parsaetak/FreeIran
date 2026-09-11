package store

// Regression tests for the Windows lifecycle failures fixed in
// v0.5.0:
//
//   - MigrateFromJSON must CLOSE the legacy file (error-aware) before
//     renaming it to ".migrated"; on Windows a rename against an open
//     handle fails with "the process cannot access the file".
//   - The legacy journal.log upgrade must CLOSE before REMOVE.
//   - Close, rename and context-cancellation errors must preserve
//     data and keep migration idempotent.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyFixture writes a valid legacy JSON database with count
// entries and returns its path.
func legacyFixture(t *testing.T, dir string, count int) string {
	t.Helper()

	entries := make(map[string]any, count)

	for i := 0; i < count; i++ {
		entries[fmt.Sprintf("%064x", i)] = map[string]string{
			"config": fmt.Sprintf(
				`{"id":"legacy-%d","type":"vless","address":"srv%d.example.com","port":443}`,
				i, i),
			"added":   "2026-01-01T00:00:00Z",
			"updated": "2026-01-01T00:00:00Z",
		}
	}

	envelope := map[string]any{
		"version": 1,
		"entries": entries,
	}

	raw, err := jsonMarshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "database.json")

	if err := os.WriteFile(path, raw, filePerm); err != nil {
		t.Fatal(err)
	}

	return path
}

// lifecycleRecorder records close/rename/remove ordering and can
// inject failures at each step.
type lifecycleRecorder struct {
	events    []string
	closeErr  error
	renameErr error
	removeErr error
}

func (r *lifecycleRecorder) install(tb testing.TB) {
	tb.Helper()

	previousClose, previousRename := closeLegacyFile, renameLegacyFile
	previousJournalClose, previousJournalRemove := closeLegacyJournal, removeLegacyJournal

	closeLegacyFile = func(file *os.File) error {
		r.events = append(r.events, "close")

		if r.closeErr != nil {
			return r.closeErr
		}

		return file.Close()
	}

	renameLegacyFile = func(from, to string) error {
		r.events = append(r.events, "rename")

		if r.renameErr != nil {
			return r.renameErr
		}

		return os.Rename(from, to)
	}

	closeLegacyJournal = func(file *os.File) error {
		r.events = append(r.events, "journal-close")

		if r.closeErr != nil {
			return r.closeErr
		}

		return file.Close()
	}

	removeLegacyJournal = func(path string) error {
		r.events = append(r.events, "journal-remove")

		if r.removeErr != nil {
			return r.removeErr
		}

		return os.Remove(path)
	}

	tb.Cleanup(func() {
		closeLegacyFile, renameLegacyFile = previousClose, previousRename
		closeLegacyJournal, removeLegacyJournal = previousJournalClose, previousJournalRemove
	})
}

// eventsJoined renders recorded events for failure messages.
func eventsJoined(events []string) string {
	return strings.Join(events, ",")
}

// writeLegacyV1Journal builds a v1 single-file journal (journal.log)
// with count uniform upsert records.
func writeLegacyV1Journal(t *testing.T, path string, count int) {
	t.Helper()

	table := crc32Table()

	buf := make([]byte, 0, 128)
	buf = append(buf, walMagic...)
	buf = append(buf, 0x01, 0x00) // version 1

	var base [8]byte

	buf = append(buf, base[:]...) // baseLSN 0
	buf = append(buf, 0, 0, 0, 0) // reserved (v1)

	for i := 0; i < count; i++ {
		raw := rawRecord{
			Op:    opUpsert,
			Key:   testKeyBytes(i),
			Value: []byte("legacy"),
		}

		buf = append(buf, byte(raw.Op))
		buf = append(buf, raw.Key[:]...)

		var valLen [4]byte

		valLen[0] = byte(len(raw.Value))
		buf = append(buf, valLen[:]...)
		buf = append(buf, raw.Value...)

		crc := journalCRC(table, raw)
		buf = append(buf, byte(crc), byte(crc>>8), byte(crc>>16), byte(crc>>24))
	}

	if err := os.WriteFile(path, buf, filePerm); err != nil {
		t.Fatal(err)
	}
}

// TestMigrationClosesLegacyBeforeRename is THE regression test for the
// Windows "preserve legacy file" failure: the rename must never run
// while the legacy descriptor is still open.
func TestMigrationClosesLegacyBeforeRename(t *testing.T) {
	s := openTestStore(t, Options{})

	dir := t.TempDir()
	path := legacyFixture(t, dir, 10)

	rec := &lifecycleRecorder{}
	rec.install(t)

	result, err := s.MigrateFromJSON(context.Background(), MigrateOptions{LegacyPath: path})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if result.Migrated != 10 {
		t.Fatalf("migrated = %d, want 10", result.Migrated)
	}

	if !result.Renamed {
		t.Fatal("legacy file was not renamed")
	}

	// Ordering contract: close strictly before rename.
	if eventsJoined(rec.events) != "close,rename" {
		t.Fatalf("lifecycle events = [%s], want [close,rename]",
			eventsJoined(rec.events))
	}

	// The renamed backup must exist with the original content.
	if _, err := os.Stat(path + ".migrated"); err != nil {
		t.Fatalf("backup missing: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("original legacy file still present after rename")
	}
}

// TestMigrationCloseErrorPreservesLegacyFile verifies a close failure
// aborts the rename (the legacy file is kept, data stays migrated, and
// re-running is a no-op that still reports correctly).
func TestMigrationCloseErrorPreservesLegacyFile(t *testing.T) {
	s := openTestStore(t, Options{})

	dir := t.TempDir()
	path := legacyFixture(t, dir, 5)

	rec := &lifecycleRecorder{closeErr: errors.New("injected close failure")}
	rec.install(t)

	_, err := s.MigrateFromJSON(context.Background(), MigrateOptions{LegacyPath: path})
	if err == nil {
		t.Fatal("close error must propagate")
	}

	if !strings.Contains(err.Error(), "close legacy database before rename") {
		t.Fatalf("error = %v, want close-before-rename wrap", err)
	}

	// The file must NOT have been renamed while its handle was open.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("legacy file must be preserved after close failure: %v", statErr)
	}

	if _, statErr := os.Stat(path + ".migrated"); !os.IsNotExist(statErr) {
		t.Fatal("no backup may exist after a failed close")
	}

	// Recovery: with the failure cleared, migration re-runs
	// idempotently (records already staged are upserted again).
	rec.closeErr = nil

	result, err := s.MigrateFromJSON(context.Background(), MigrateOptions{LegacyPath: path})
	if err != nil {
		t.Fatalf("retry after close failure: %v", err)
	}

	if !result.Renamed {
		t.Fatal("retry must complete the rename")
	}

	if s.Count() != 5 {
		t.Fatalf("count after recovery = %d, want 5", s.Count())
	}
}

// TestMigrationRenameErrorPropagates verifies rename failures surface
// with the original wrap ("preserve legacy file") and leave a
// retryable state.
func TestMigrationRenameErrorPropagates(t *testing.T) {
	s := openTestStore(t, Options{})

	dir := t.TempDir()
	path := legacyFixture(t, dir, 3)

	rec := &lifecycleRecorder{renameErr: errors.New("access denied")}
	rec.install(t)

	_, err := s.MigrateFromJSON(context.Background(), MigrateOptions{LegacyPath: path})
	if err == nil {
		t.Fatal("rename error must propagate")
	}

	if !strings.Contains(err.Error(), "preserve legacy file") {
		t.Fatalf("error = %v, want preserve-legacy-file wrap", err)
	}

	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("legacy file must survive a failed rename: %v", statErr)
	}

	// Recovery: retry succeeds and re-applies the batches.
	rec.renameErr = nil

	result, err := s.MigrateFromJSON(context.Background(), MigrateOptions{LegacyPath: path})
	if err != nil {
		t.Fatalf("retry after rename failure: %v", err)
	}

	if result.Migrated != 3 || !result.Renamed {
		t.Fatalf("retry result = %+v", result)
	}
}

// TestMigrationIdempotentAfterRename verifies re-running a completed
// migration is a successful no-op.
func TestMigrationIdempotentAfterRename(t *testing.T) {
	s := openTestStore(t, Options{})

	dir := t.TempDir()
	path := legacyFixture(t, dir, 4)

	if _, err := s.MigrateFromJSON(context.Background(), MigrateOptions{LegacyPath: path}); err != nil {
		t.Fatalf("first migration: %v", err)
	}

	second, err := s.MigrateFromJSON(context.Background(), MigrateOptions{LegacyPath: path})
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}

	if second.Migrated != 0 || second.Renamed {
		t.Fatalf("second migration must be a no-op, got %+v", second)
	}

	if s.Count() != 4 {
		t.Fatalf("count = %d, want 4", s.Count())
	}
}

// TestMigrationContextCancelledPreservesState verifies cancellation
// aborts before the rename and the store stays usable.
func TestMigrationContextCancelledPreservesState(t *testing.T) {
	s := openTestStore(t, Options{})

	dir := t.TempDir()
	path := legacyFixture(t, dir, 50)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the first pass

	if _, err := s.MigrateFromJSON(ctx, MigrateOptions{LegacyPath: path}); err == nil {
		t.Fatal("cancelled migration must fail")
	}

	// The legacy file must be untouched.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("legacy file must survive cancellation: %v", statErr)
	}

	// The store must remain fully usable afterwards.
	if err := s.Upsert(testKey(1), testValue(1)); err != nil {
		t.Fatalf("store unusable after cancelled migration: %v", err)
	}

	// And migration can complete normally afterwards.
	result, err := s.MigrateFromJSON(context.Background(), MigrateOptions{LegacyPath: path})
	if err != nil {
		t.Fatalf("migration after cancellation: %v", err)
	}

	if result.Migrated != 50 {
		t.Fatalf("migrated = %d, want 50", result.Migrated)
	}
}

// TestLegacyJournalUpgradeClosesBeforeRemove is the regression test
// for the Windows "journal.log: The process cannot access the file
// because it is being used by another process" failure: the v1
// journal upgrade must close (error-aware) before unlinking.
func TestLegacyJournalUpgradeClosesBeforeRemove(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "journal.log")

	writeLegacyV1Journal(t, legacy, 5)

	rec := &lifecycleRecorder{}
	rec.install(t)

	replayed := 0

	j, err := openJournal(dir, 0, 0, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		replayed++

		return nil
	})
	if err != nil {
		t.Fatalf("open with legacy journal: %v", err)
	}

	defer j.Close()

	if replayed != 5 {
		t.Fatalf("replayed %d records, want 5", replayed)
	}

	// Ordering contract: journal close strictly before remove.
	if eventsJoined(rec.events) != "journal-close,journal-remove" {
		t.Fatalf("lifecycle events = [%s], want [journal-close,journal-remove]",
			eventsJoined(rec.events))
	}

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy journal still present after upgrade")
	}
}

// TestLegacyJournalUpgradeCloseErrorKeepsFile verifies a close failure
// keeps the legacy journal (upgrade re-runs next boot) and still
// replays its records.
func TestLegacyJournalUpgradeCloseErrorKeepsFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "journal.log")

	writeLegacyV1Journal(t, legacy, 3)

	rec := &lifecycleRecorder{closeErr: errors.New("injected close failure")}
	rec.install(t)

	replayed := 0

	j, err := openJournal(dir, 0, 0, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		replayed++

		return nil
	})
	if err != nil {
		t.Fatalf("open with legacy journal: %v", err)
	}

	defer j.Close()

	if replayed != 3 {
		t.Fatalf("replayed %d records, want 3", replayed)
	}

	// The close error must have aborted before any removal.
	for _, event := range rec.events {
		if event == "journal-remove" {
			t.Fatal("legacy journal must not be removed after a close failure")
		}
	}

	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Fatalf("legacy journal must survive a close failure: %v", statErr)
	}
}

// TestLegacyJournalUpgradeRemoveFailureDefersUpgrade verifies a
// removal failure (e.g. a third-party lock) no longer fails the store
// open: the records are already durably re-journaled, so the upgrade
// is deferred to the next boot instead of bricking the application.
func TestLegacyJournalUpgradeRemoveFailureDefersUpgrade(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "journal.log")

	writeLegacyV1Journal(t, legacy, 2)

	rec := &lifecycleRecorder{removeErr: errors.New("sharing violation")}
	rec.install(t)

	replayed := 0

	j, err := openJournal(dir, 0, 0, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		replayed++

		return nil
	})
	if err != nil {
		t.Fatalf("open must succeed when only the cleanup fails: %v", err)
	}

	defer j.Close()

	if replayed != 2 {
		t.Fatalf("replayed %d records, want 2", replayed)
	}

	if got := j.CurrentLSN(); got != 2 {
		t.Fatalf("lsn = %d, want 2 (records must be re-journaled)", got)
	}

	// The legacy file stays behind and the upgrade retries next boot.
	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Fatalf("legacy journal must remain after a failed removal: %v", statErr)
	}
}
