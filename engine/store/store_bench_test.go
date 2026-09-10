package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkStoreUpsertFlush(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	keys := make([]string, 5000)
	values := make([][]byte, 5000)

	for i := range keys {
		keys[i] = testKey(i)
		values[i] = testValue(i)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := s.Upsert(keys[i%5000], values[i%5000]); err != nil {
			b.Fatal(err)
		}
	}

	b.StopTimer()

	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkStoreGet(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	for i := 0; i < 5000; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			b.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}

	keys := make([]string, 5000)

	for i := range keys {
		keys[i] = testKey(i)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := s.Get(keys[i%5000]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStoreGetMemtable measures the hottest read path: a value
// served from the active memtable without any chunk I/O.
func BenchmarkStoreGetMemtable(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	for i := 0; i < 1000; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			b.Fatal(err)
		}
	}

	keys := make([]string, 1000)

	for i := range keys {
		keys[i] = testKey(i)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := s.Get(keys[i%1000]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreUpsert(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	keys := make([]string, b.N)
	for i := range keys {
		keys[i] = testKey(i)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := s.Upsert(keys[i], testValue(i)); err != nil {
			b.Fatal(err)
		}
	}

	b.StopTimer()
}

// BenchmarkStoreUpsertBatch measures the batch write path: one WAL
// append + one fsync + one memtable application per batch.
func BenchmarkStoreUpsertBatch(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	const batchSize = 512

	keys := make([]string, batchSize)
	for i := range keys {
		keys[i] = testKey(i)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		pairs := make([]Pair, batchSize)

		for j := range pairs {
			pairs[j] = Pair{Key: keys[j], Value: testValue(j)}
		}

		if err := s.UpsertBatch(pairs); err != nil {
			b.Fatal(err)
		}
	}

	b.StopTimer()
}

// BenchmarkStoreFlush measures synchronous flush latency for one
// fully-loaded memtable.
func BenchmarkStoreFlush(b *testing.B) {
	path := b.TempDir()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()

		s, err := Open(Options{Path: path})
		if err != nil {
			b.Fatal(err)
		}

		for j := 0; j < 2048; j++ {
			if err := s.Upsert(testKey(j), testValue(j)); err != nil {
				b.Fatal(err)
			}
		}

		b.StartTimer()

		if err := s.Flush(); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()

		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompaction measures a full compaction pass over a store
// with 50% dead records.
func BenchmarkCompaction(b *testing.B) {
	b.StopTimer()

	path := b.TempDir()

	s, err := Open(Options{Path: path, MemtableRecords: 512})
	if err != nil {
		b.Fatal(err)
	}

	for round := 0; round < 2; round++ {
		for i := 0; i < 2000; i++ {
			if err := s.Upsert(testKey(i), testValue(i)); err != nil {
				b.Fatal(err)
			}
		}
	}

	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.StartTimer()

	for i := 0; i < b.N; i++ {
		if err := s.Compact(context.Background()); err != nil {
			b.Fatal(err)
		}
	}

	b.StopTimer()

	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkMigration measures the streaming legacy-JSON migration
// (decode + batch + flush + verification passes).
func BenchmarkMigration(b *testing.B) {
	b.StopTimer()

	path := b.TempDir()
	legacyPath := path + "-legacy.json"

	const records = 4000

	entries := make(map[string]any, records)

	for i := 0; i < records; i++ {
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

	if err := writeFileJSON(legacyPath, map[string]any{
		"version": 1,
		"entries": entries,
	}); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()

		s, err := Open(Options{Path: filepath.Join(path, fmt.Sprintf("store-%d", i))})
		if err != nil {
			b.Fatal(err)
		}

		// Fresh legacy file per iteration (migration renames it on
		// success).
		if err := writeFileJSON(legacyPath, map[string]any{
			"version": 1,
			"entries": entries,
		}); err != nil {
			b.Fatal(err)
		}

		b.StartTimer()

		if _, err := s.MigrateFromJSON(context.Background(),
			MigrateOptions{LegacyPath: legacyPath}); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()

		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIterate measures a full ordered scan of 20k records.
func BenchmarkStoreIterate(b *testing.B) {
	s := openBenchStore(b)
	defer s.Close()

	for i := 0; i < 20000; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			b.Fatal(err)
		}
	}

	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		seen := 0

		err := s.Iterate(context.Background(), func(key string, value []byte) error {
			seen++

			return nil
		})

		if err != nil {
			b.Fatal(err)
		}

		if seen != 20000 {
			b.Fatalf("iterated %d", seen)
		}
	}
}

func BenchmarkStoreReopen(b *testing.B) {
	path := b.TempDir()

	s, err := Open(Options{Path: path})
	if err != nil {
		b.Fatal(err)
	}

	for i := 0; i < 20000; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			b.Fatal(err)
		}
	}

	if err := s.Close(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reopened, err := Open(Options{Path: path})
		if err != nil {
			b.Fatal(err)
		}

		reopened.Close()
	}
}

func openBenchStore(b *testing.B) *Store {
	b.Helper()

	s, err := Open(Options{
		Path:            b.TempDir(),
		MemtableRecords: 4096,
	})
	if err != nil {
		b.Fatal(err)
	}

	return s
}

// TestAllocsHotPaths pins allocation counts of the hottest paths so
// regressions are caught without running benchmarks.
func TestAllocsHotPaths(t *testing.T) {
	path := t.TempDir()

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()

	key := testKey(1)

	if err := s.Upsert(key, testValue(1)); err != nil {
		t.Fatal(err)
	}

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// Key decoding must stay allocation-free.
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := decodeKey(key); err != nil {
			t.Fatal(err)
		}
	})

	if allocs != 0 {
		t.Fatalf("decodeKey allocates %v times per call, want 0", allocs)
	}

	// Get from a chunk: at most three allocations — the record body,
	// the caller's defensive copy and one deferred release. (The
	// pre-rework path allocated seven or more: key formatting, cache
	// key boxing, path building and the value copy.)
	allocs = testing.AllocsPerRun(100, func() {
		if _, err := s.Get(key); err != nil {
			t.Fatal(err)
		}
	})

	if allocs > 3 {
		t.Fatalf("Get allocates %v times per call, want <= 3", allocs)
	}

	// Encoding a key for iteration: exactly one allocation (the
	// resulting string; the scratch buffer is stack-allocated).
	bin := testKeyBytes(1)

	var sink string

	allocs = testing.AllocsPerRun(100, func() {
		sink = encodeKey(bin)
	})

	_ = sink

	if allocs != 1 {
		t.Fatalf("encodeKey allocates %v times per call, want 1", allocs)
	}
}

// writeFileJSON and copyFile are benchmark helpers.
func writeFileJSON(path string, payload any) error {
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, raw, 0o600)
}
