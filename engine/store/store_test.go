package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func testKey(i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("config-%d", i)))
	return hex.EncodeToString(sum[:])
}

func testValue(i int) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"config-%d","type":"vless","address":"srv%d.example.com","port":443}`,
		i, i))
}

func openTestStore(t *testing.T, opts Options) *Store {
	t.Helper()

	if opts.Path == "" {
		opts.Path = t.TempDir()
	}

	s, err := Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() {
		_ = s.Close()
	})

	return s
}

func TestBasicRoundTrip(t *testing.T) {
	s := openTestStore(t, Options{})

	for i := 0; i < 100; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	if got := s.Count(); got != 100 {
		t.Fatalf("count = %d, want 100", got)
	}

	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	for i := 0; i < 100; i++ {
		value, err := s.Get(testKey(i))
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}

		if string(value) != string(testValue(i)) {
			t.Fatalf("value %d mismatch", i)
		}
	}

	if got := s.Count(); got != 100 {
		t.Fatalf("count after flush = %d", got)
	}
}

func TestReopenPersistence(t *testing.T) {
	path := t.TempDir()

	s := openTestStore(t, Options{Path: path})

	for i := 0; i < 50; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	defer reopened.Close()

	if got := reopened.Count(); got != 50 {
		t.Fatalf("count after reopen = %d, want 50", got)
	}

	for i := 0; i < 50; i++ {
		value, err := reopened.Get(testKey(i))
		if err != nil {
			t.Fatalf("get %d after reopen: %v", i, err)
		}

		if string(value) != string(testValue(i)) {
			t.Fatalf("value %d corrupted across reopen", i)
		}
	}
}

func TestUnflushedWritesSurviveCrash(t *testing.T) {
	// Simulates a crash: writes journaled but never flushed. The WAL
	// must replay them on the next open.
	path := t.TempDir()

	s, err := Open(Options{Path: path, MemtableRecords: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	// Close WITHOUT flush by closing the journal directly.
	if err := s.wal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Path: path, MemtableRecords: 1 << 30})
	if err != nil {
		t.Fatalf("crash recovery open: %v", err)
	}

	defer reopened.Close()

	if got := reopened.Count(); got != 10 {
		t.Fatalf("replayed count = %d, want 10", got)
	}

	value, err := reopened.Get(testKey(7))
	if err != nil || string(value) != string(testValue(7)) {
		t.Fatalf("replayed value wrong: %v", err)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	s := openTestStore(t, Options{})

	if err := s.Upsert(testKey(1), testValue(1)); err != nil {
		t.Fatal(err)
	}

	updated := []byte(`{"id":"config-1","type":"vless","address":"new.example.com","port":8443}`)

	if err := s.Upsert(testKey(1), updated); err != nil {
		t.Fatal(err)
	}

	value, err := s.Get(testKey(1))
	if err != nil || string(value) != string(updated) {
		t.Fatalf("update failed: %v", err)
	}

	if got := s.Count(); got != 1 {
		t.Fatalf("count after update = %d", got)
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(testKey(1)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(testKey(1)); err != ErrNotFound {
		t.Fatalf("deleted key should be ErrNotFound, got %v", err)
	}

	if s.Count() != 0 {
		t.Fatalf("count after delete = %d", s.Count())
	}

	// Reopen: delete must persist.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestStore(t, Options{Path: s.root})

	if reopened.Has(testKey(1)) {
		t.Fatal("delete did not persist")
	}
}

func TestAutoFlushThreshold(t *testing.T) {
	s := openTestStore(t, Options{
		MemtableRecords:  10,
		TargetChunkBytes: 1 << 20,
	})

	for i := 0; i < 25; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	// Two automatic flushes must have happened.
	s.mu.RLock()
	chunksCount := len(s.chunksMap)
	s.mu.RUnlock()

	if chunksCount < 2 {
		t.Fatalf("expected >= 2 chunks after 25 records, got %d", chunksCount)
	}

	if got := s.Count(); got != 25 {
		t.Fatalf("count = %d, want 25", got)
	}
}

func TestIterateAll(t *testing.T) {
	s := openTestStore(t, Options{MemtableRecords: 7})

	for i := 0; i < 30; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	seen := 0
	previous := ""

	err := s.Iterate(context.Background(), func(key string, value []byte) error {
		if previous != "" && key <= previous {
			t.Fatalf("iteration not in key order: %s after %s", key, previous)
		}

		previous = key
		seen++

		return nil
	})

	if err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if seen != 30 {
		t.Fatalf("iterated %d records, want 30", seen)
	}
}

func TestIndexRebuildFromChunks(t *testing.T) {
	path := t.TempDir()

	s := openTestStore(t, Options{Path: path})

	for i := 0; i < 20; i++ {
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

	// Destroy the index to simulate corruption.
	if err := os.Remove(filepath.Join(path, "index.bin")); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("open after index loss: %v", err)
	}

	defer reopened.Close()

	for i := 0; i < 20; i++ {
		if !reopened.Has(testKey(i)) {
			t.Fatalf("key %d missing after index rebuild", i)
		}
	}
}

func TestMetaRebuildFromChunks(t *testing.T) {
	path := t.TempDir()

	s := openTestStore(t, Options{Path: path})

	for i := 0; i < 15; i++ {
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

	if err := os.Remove(filepath.Join(path, "store.meta")); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("open after meta loss: %v", err)
	}

	defer reopened.Close()

	if got := reopened.Count(); got != 15 {
		t.Fatalf("count after meta rebuild = %d, want 15", got)
	}
}

func TestCompactionReclaimsDeadRecords(t *testing.T) {
	s := openTestStore(t, Options{MemtableRecords: 5})

	// Produce churn: overwrites create dead records in old chunks.
	for round := 0; round < 6; round++ {
		for i := 0; i < 10; i++ {
			if err := s.Upsert(testKey(i),
				[]byte(fmt.Sprintf(`{"round":%d,"key":%d}`, round, i))); err != nil {
				t.Fatal(err)
			}
		}
	}

	before := s.Snapshot()

	if err := s.Compact(context.Background()); err != nil {
		t.Fatalf("compact: %v", err)
	}

	after := s.Snapshot()

	if after.DeadRecords >= before.DeadRecords {
		t.Fatalf("compaction did not reclaim: before=%d after=%d",
			before.DeadRecords, after.DeadRecords)
	}

	// All 10 keys must remain readable with the newest value.
	for i := 0; i < 10; i++ {
		value, err := s.Get(testKey(i))
		if err != nil {
			t.Fatalf("get after compact %d: %v", i, err)
		}

		if want := fmt.Sprintf(`{"round":5,"key":%d}`, i); string(value) != want {
			t.Fatalf("value %d = %s, want %s", i, value, want)
		}
	}

	if got := s.Count(); got != 10 {
		t.Fatalf("count after compaction = %d", got)
	}
}

func TestVerifyAll(t *testing.T) {
	s := openTestStore(t, Options{MemtableRecords: 5})

	for i := 0; i < 22; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	progressCalls := 0

	if err := s.VerifyAll(context.Background(), func(done, total int) {
		progressCalls++
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}

	if progressCalls == 0 {
		t.Fatal("progress callback never invoked")
	}
}

func TestBadKeysRejected(t *testing.T) {
	s := openTestStore(t, Options{})

	if err := s.Upsert("short", []byte("x")); !errors.Is(err, ErrBadKey) {
		t.Fatalf("short key: %v", err)
	}

	if err := s.Upsert(testKey(1), nil); !errors.Is(err, ErrEmptyValue) {
		t.Fatalf("empty value: %v", err)
	}
}

func TestMigrationFromLegacyJSON(t *testing.T) {
	path := t.TempDir()

	legacyPath := filepath.Join(path, "freeiran-db.json")

	// Build a legacy v1 database file matching the old format.
	legacy := map[string]any{
		"version": 1,
		"entries": map[string]any{},
	}

	entries := legacy["entries"].(map[string]any)

	for i := 0; i < 40; i++ {
		entries[testKey(i)] = map[string]any{
			"config": map[string]any{
				"id":      testKey(i),
				"type":    "vless",
				"address": fmt.Sprintf("srv%d.example.com", i),
				"port":    443,
			},
			"added":   "2026-01-01T00:00:00Z",
			"updated": "2026-01-02T00:00:00Z",
		}
	}

	raw, err := jsonMarshal(legacy)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(legacyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s := openTestStore(t, Options{Path: filepath.Join(path, "store")})

	result, err := s.MigrateFromJSON(context.Background(), MigrateOptions{
		LegacyPath: legacyPath,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if result.Migrated != 40 {
		t.Fatalf("migrated = %d, want 40", result.Migrated)
	}

	if !result.Renamed {
		t.Fatal("legacy file should be renamed after migration")
	}

	// Old file must not exist; the .migrated backup must.
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatal("legacy file still present after migration")
	}

	if _, err := os.Stat(legacyPath + ".migrated"); err != nil {
		t.Fatal("migrated backup missing")
	}

	// Idempotency: second run is a no-op.
	second, err := s.MigrateFromJSON(context.Background(), MigrateOptions{
		LegacyPath: legacyPath,
	})
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	if second.Migrated != 0 {
		t.Fatalf("second migration moved %d records", second.Migrated)
	}

	// All fingerprints preserved and readable.
	for i := 0; i < 40; i++ {
		if !s.Has(testKey(i)) {
			t.Fatalf("fingerprint %d missing after migration", i)
		}
	}
}

func TestConcurrentUpsertsAndReads(t *testing.T) {
	s := openTestStore(t, Options{MemtableRecords: 64})

	var wg sync.WaitGroup

	for g := 0; g < 6; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			for i := 0; i < 80; i++ {
				id := g*1000 + i

				if err := s.Upsert(testKey(id), testValue(id)); err != nil {
					t.Errorf("upsert: %v", err)

					return
				}

				if _, err := s.Get(testKey(id)); err != nil {
					t.Errorf("read-own-write: %v", err)

					return
				}
			}
		}(g)
	}

	wg.Wait()

	if got := s.Count(); got != 480 {
		t.Fatalf("count = %d, want 480", got)
	}
}

func TestStatsSnapshot(t *testing.T) {
	s := openTestStore(t, Options{})

	for i := 0; i < 12; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	stats := s.Snapshot()

	if stats.Count != 12 || stats.ChunkCount != 1 {
		t.Fatalf("stats wrong: %+v", stats)
	}

	if stats.DiskBytes == 0 {
		t.Fatal("disk bytes should be positive")
	}
}
