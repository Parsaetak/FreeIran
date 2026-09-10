package store

// Lifecycle and resource-ownership tests.
//
// These encode the acceptance criteria of the storage rework:
//
//   - Open → use → Close → delete directory works IMMEDIATELY on every
//     platform (on Windows an open handle blocks deletion, so this is
//     the direct regression test for the CI failure).
//   - No file descriptor remains open after Close (verified through
//     /proc where available).
//   - Chunk-handle cache eviction really closes evicted files.
//   - Compaction removes victims only after their handles are closed.
//   - Concurrent readers and Close cooperate deterministically.
//   - Operations after Close fail with ErrClosed, never panic.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// listOpenChunkFiles reports chunk files of this store still open by
// the current process (Linux-only, via /proc/self/fd).
func listOpenChunkFiles(root string) []string {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}

	chunkDir := filepath.Join(root, "chunks")

	var leaks []string

	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			continue
		}

		if filepath.Dir(target) == chunkDir {
			leaks = append(leaks, filepath.Base(target))
		}
	}

	return leaks
}

// TestDeleteDirectoryImmediatelyAfterClose is the portable encoding of
// the Windows acceptance criterion: after Close the store directory
// must be deletable without error. On Windows this fails loudly while
// any handle is still open.
func TestDeleteDirectoryImmediatelyAfterClose(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "store")

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// Force chunk handles open through the read path.
	for i := 0; i < 50; i++ {
		if _, err := s.Get(testKey(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The store directory must be deletable immediately.
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("store directory not deletable after Close: %v", err)
	}
}

// TestCloseReleasesAllFileHandles verifies directly (via /proc) that
// no chunk descriptor outlives Close.
func TestCloseReleasesAllFileHandles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc/self/fd")
	}

	path := t.TempDir()

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		if _, err := s.Get(testKey(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if leaks := listOpenChunkFiles(path); len(leaks) > 0 {
		t.Fatalf("chunk file(s) still open after Close: %v", leaks)
	}
}

// TestRepeatedOpenClose runs many open/use/close cycles on one
// directory and deletes it at the end.
func TestRepeatedOpenClose(t *testing.T) {
	path := t.TempDir()

	const cycles = 5

	for c := 0; c < cycles; c++ {
		s, err := Open(Options{Path: path})
		if err != nil {
			t.Fatalf("cycle %d open: %v", c, err)
		}

		for i := 0; i < 10; i++ {
			if err := s.Upsert(testKey(c*100+i), testValue(i)); err != nil {
				t.Fatalf("cycle %d upsert: %v", c, err)
			}
		}

		if _, err := s.Get(testKey(c * 100)); err != nil {
			t.Fatalf("cycle %d get: %v", c, err)
		}

		if err := s.Close(); err != nil {
			t.Fatalf("cycle %d close: %v", c, err)
		}
	}

	// All data written across cycles must survive.
	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()

	if got := s.Count(); got != cycles*10 {
		t.Fatalf("count after %d cycles = %d, want %d", cycles, got, cycles*10)
	}
}

// TestCacheEvictionClosesFiles proves that LRU eviction of chunk
// handles closes the underlying descriptors.
func TestCacheEvictionClosesFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc/self/fd")
	}

	path := t.TempDir()

	s, err := Open(Options{
		Path:            path,
		MemtableRecords: 5,
		OpenFiles:       2,
	})
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()

	// Create at least 4 chunks.
	for i := 0; i < 25; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// Touch more chunks than the cache can hold.
	for i := 0; i < 25; i++ {
		if _, err := s.Get(testKey(i)); err != nil {
			t.Fatal(err)
		}
	}

	stats := s.files.snapshotStats()
	if stats.Evictions == 0 {
		t.Fatal("expected evictions with OpenFiles=2 and 5 chunks")
	}

	if open := s.files.len(); open > 2 {
		t.Fatalf("open handles = %d, want <= 2", open)
	}
}

// TestConcurrentReadAndClose exercises readers racing a Close: every
// reader either succeeds or observes ErrClosed; Close itself must
// complete without deadlock.
func TestConcurrentReadAndClose(t *testing.T) {
	path := t.TempDir()

	s, err := Open(Options{Path: path, MemtableRecords: 8})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 40; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	sawErrors := make(chan error, 64)

	for g := 0; g < 4; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			for i := 0; i < 50; i++ {
				_, err := s.Get(testKey((g*50 + i) % 40))
				if err != nil && !errors.Is(err, ErrClosed) &&
					!errors.Is(err, errCacheClosed) {
					sawErrors <- err
				}
			}
		}(g)
	}

	// Close while readers are active.
	time.Sleep(2 * time.Millisecond)

	if err := s.Close(); err != nil {
		t.Fatalf("close during reads: %v", err)
	}

	wg.Wait()
	close(sawErrors)

	for err := range sawErrors {
		t.Fatalf("unexpected read error during close: %v", err)
	}
}

// TestOperationsAfterClose verifies every public operation rejects
// cleanly after Close.
func TestOperationsAfterClose(t *testing.T) {
	path := t.TempDir()

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Upsert(testKey(1), testValue(1)); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("second close must be a no-op, got %v", err)
	}

	if _, err := s.Get(testKey(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after close: %v", err)
	}

	if err := s.Upsert(testKey(2), testValue(2)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Upsert after close: %v", err)
	}

	if err := s.UpsertBatch([]Pair{{Key: testKey(3), Value: testValue(3)}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("UpsertBatch after close: %v", err)
	}

	if err := s.Delete(testKey(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete after close: %v", err)
	}

	if err := s.Flush(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Flush after close: %v", err)
	}

	if err := s.Iterate(context.Background(), func(string, []byte) error {
		return nil
	}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Iterate after close: %v", err)
	}

	if err := s.Compact(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Compact after close: %v", err)
	}

	if _, err := s.MigrateFromJSON(context.Background(),
		MigrateOptions{LegacyPath: filepath.Join(path, "missing.json")}); !errors.Is(err, ErrClosed) {
		t.Fatalf("MigrateFromJSON after close: %v", err)
	}

	if s.Has(testKey(1)) {
		t.Fatal("Has after close must be false")
	}
}

// TestFailedOperationThenClose verifies that a failed write (bad key)
// does not break the shutdown path.
func TestFailedOperationThenClose(t *testing.T) {
	path := t.TempDir()

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Upsert("not-a-fingerprint", []byte("x")); !errors.Is(err, ErrBadKey) {
		t.Fatalf("expected ErrBadKey, got %v", err)
	}

	if err := s.Upsert(testKey(1), nil); !errors.Is(err, ErrEmptyValue) {
		t.Fatalf("expected ErrEmptyValue, got %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close after failed ops: %v", err)
	}

	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("delete after failed ops: %v", err)
	}
}

// TestCompactionThenCloseDelete runs compaction (which removes chunk
// files) and then requires the directory to be deletable — the exact
// Windows scenario where cached victim handles used to block removal.
func TestCompactionThenCloseDelete(t *testing.T) {
	path := t.TempDir()

	s, err := Open(Options{Path: path, MemtableRecords: 5})
	if err != nil {
		t.Fatal(err)
	}

	// Churn to create dead records.
	for round := 0; round < 4; round++ {
		for i := 0; i < 12; i++ {
			if err := s.Upsert(testKey(i),
				[]byte("{\"round\":"+string(rune('0'+round))+"}")); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Read everything so chunk handles are cached and pinned.
	for i := 0; i < 12; i++ {
		if _, err := s.Get(testKey(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Compact(context.Background()); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// Survivors readable.
	for i := 0; i < 12; i++ {
		if _, err := s.Get(testKey(i)); err != nil {
			t.Fatalf("get after compact: %v", err)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("directory not deletable after compaction + close: %v", err)
	}
}

// TestDeleteOfUnflushedOverrideSurvivesFlush is the regression test
// for the resurrection bug: deleting a key whose newest value is still
// unflushed used to leave a stale index entry that resurrected the old
// value after the tombstone was dropped by the flush.
func TestDeleteOfUnflushedOverrideSurvivesFlush(t *testing.T) {
	s := openTestStore(t, Options{})

	// First value flushed into a chunk.
	if err := s.Upsert(testKey(1), []byte("v1")); err != nil {
		t.Fatal(err)
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// Override without flushing, then delete while unflushed.
	if err := s.Upsert(testKey(1), []byte("v2")); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(testKey(1)); err != nil {
		t.Fatal(err)
	}

	// Flush drops the tombstone; the key must stay deleted.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(testKey(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key resurrected after flush: %v", err)
	}

	if s.Has(testKey(1)) {
		t.Fatal("Has must be false after flush of tombstone")
	}

	if got := s.Count(); got != 0 {
		t.Fatalf("count after flush = %d, want 0", got)
	}
}

// TestCrashReplayWithDeletes verifies delete records survive a
// crash-replay cycle (regression for the v1 journal framing bug where
// replayed deletes failed their CRC and were silently discarded).
func TestCrashReplayWithDeletes(t *testing.T) {
	path := t.TempDir()

	s, err := Open(Options{Path: path, MemtableRecords: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 12; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// Delete some keys, then simulate a crash: journal closed without
	// a flush, memtable lost.
	for i := 0; i < 5; i++ {
		if err := s.Delete(testKey(i)); err != nil {
			t.Fatal(err)
		}
	}

	// One more upsert AFTER the deletes: it must not be swallowed by a
	// broken replay tail.
	if err := s.Upsert(testKey(99), testValue(99)); err != nil {
		t.Fatal(err)
	}

	if err := s.wal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Path: path, MemtableRecords: 1 << 30})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	defer reopened.Close()

	for i := 0; i < 5; i++ {
		if reopened.Has(testKey(i)) {
			t.Fatalf("delete %d lost across crash replay", i)
		}
	}

	for i := 5; i < 12; i++ {
		if !reopened.Has(testKey(i)) {
			t.Fatalf("upsert %d lost across crash replay", i)
		}
	}

	if !reopened.Has(testKey(99)) {
		t.Fatal("post-delete upsert lost across crash replay")
	}
}

// TestIterateConsistentSnapshot verifies iteration observes one
// consistent point-in-time view while writers mutate concurrently.
func TestIterateConsistentSnapshot(t *testing.T) {
	s := openTestStore(t, Options{MemtableRecords: 64})

	for i := 0; i < 40; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		for i := 40; i < 120; i++ {
			if err := s.Upsert(testKey(i), testValue(i)); err != nil {
				return
			}
		}
	}()

	seen := 0

	err := s.Iterate(context.Background(), func(key string, value []byte) error {
		seen++

		return nil
	})

	wg.Wait()

	if err != nil {
		t.Fatalf("iterate: %v", err)
	}

	// The snapshot must be a consistent prefix state: at least the 40
	// pre-existing records, at most all 120.
	if seen < 40 || seen > 120 {
		t.Fatalf("snapshot saw %d records, want between 40 and 120", seen)
	}
}

// TestBackgroundFlushDrains verifies the worker drains frozen tables
// without any explicit Flush call.
func TestBackgroundFlushDrains(t *testing.T) {
	s := openTestStore(t, Options{MemtableRecords: 8})

	for i := 0; i < 40; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	// The worker must eventually persist everything on its own.
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if s.Inspect().PendingTables == 0 && s.Inspect().MemtableBytes == 0 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	diag := s.Inspect()

	if diag.PendingTables != 0 || diag.MemtableBytes != 0 {
		t.Fatalf("background flush did not drain: %+v", diag)
	}

	if diag.Flushes == 0 {
		t.Fatal("no background flush executed")
	}

	if got := s.Count(); got != 40 {
		t.Fatalf("count = %d, want 40", got)
	}
}

// TestOrphanChunkCleanup verifies chunk files left behind by a crash
// between chunk write and meta persist are removed on open (their
// records remain recoverable through the WAL).
func TestOrphanChunkCleanup(t *testing.T) {
	path := t.TempDir()

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate an orphan: a chunk file not referenced by store.meta.
	orphan := filepath.Join(path, "chunks", "000042.firc")

	if err := os.WriteFile(orphan, []byte("FIRC garbage"), filePerm); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen with orphan: %v", err)
	}

	defer reopened.Close()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan chunk was not removed on open")
	}

	if got := reopened.Count(); got != 10 {
		t.Fatalf("count after orphan cleanup = %d, want 10", got)
	}
}
