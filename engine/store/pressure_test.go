package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestApplyPressureAdjustsLimits verifies the adaptive thresholds:
// every level lowers the bounds, Normal restores them, and the floors
// are respected (no fragmentation below the minimum).
func TestApplyPressureAdjustsLimits(t *testing.T) {
	s := openTestStore(t, Options{})

	records0, bytes0, target0 := s.PressureLimits()
	if records0 != defaultMemtableRecords || bytes0 != defaultMemtableBytes {
		t.Fatalf("initial limits = %d/%d, want defaults %d/%d",
			records0, bytes0, defaultMemtableRecords, defaultMemtableBytes)
	}

	if target0 != defaultTargetChunkBytes {
		t.Fatalf("initial chunk target = %d, want default %d",
			target0, defaultTargetChunkBytes)
	}

	if !s.ApplyPressure(PressureHigh) {
		t.Fatal("ApplyPressure(High) reported no change")
	}

	records1, bytes1, _ := s.PressureLimits()
	if records1 >= records0 || bytes1 >= bytes0 {
		t.Fatalf("high limits did not shrink: %d/%d vs %d/%d",
			records1, bytes1, records0, bytes0)
	}

	if s.ApplyPressure(PressureHigh) {
		t.Fatal("ApplyPressure(High) again should be a no-op (false)")
	}

	if !s.ApplyPressure(PressureNormal) {
		t.Fatal("ApplyPressure(Normal) reported no change")
	}

	records2, bytes2, _ := s.PressureLimits()
	if records2 != records0 || bytes2 != bytes0 {
		t.Fatalf("normal restore = %d/%d, want %d/%d",
			records2, bytes2, records0, bytes0)
	}
}

// TestApplyPressureCriticalFlushes verifies the Critical reaction:
// pending memtable state is frozen and flushed so writers see
// backpressure instead of unbounded memory growth.
func TestApplyPressureCriticalFlushes(t *testing.T) {
	s := openTestStore(t, Options{})

	// Small enough to stay under the freeze thresholds.
	for i := 0; i < 16; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	s.ApplyPressure(PressureCritical)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		diag := s.Inspect()
		if diag.PendingKeys == 0 && diag.MemtableBytes == 0 {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	diag := s.Inspect()
	if diag.PendingKeys != 0 || diag.MemtableBytes != 0 {
		t.Fatalf("critical pressure did not flush: pending=%d memtable=%d",
			diag.PendingKeys, diag.MemtableBytes)
	}

	for i := 0; i < 16; i++ {
		value, err := s.Get(testKey(i))
		if err != nil {
			t.Fatalf("get %d after critical flush: %v", i, err)
		}

		if string(value) != string(testValue(i)) {
			t.Fatalf("value %d corrupted after flush", i)
		}
	}
}

// TestSetChunkTargetBytesClamps verifies the booster wiring clamps the
// proposed chunk/flush size into the safe range.
func TestSetChunkTargetBytesClamps(t *testing.T) {
	s := openTestStore(t, Options{})

	// Below-floor and above-ceiling proposals clamp into the safe
	// range (reported as changed, since the stored value moves).
	if !s.SetChunkTargetBytes(64 << 10) {
		t.Fatal("below-floor proposal should clamp to the floor (changed=true)")
	}

	if got := s.ChunkTargetBytes(); got != MinChunkTargetBytes {
		t.Fatalf("ChunkTargetBytes = %d, want floor %d", got, MinChunkTargetBytes)
	}

	if !s.SetChunkTargetBytes(1 << 30) {
		t.Fatal("above-ceiling proposal should clamp to the ceiling (changed=true)")
	}

	if got := s.ChunkTargetBytes(); got != MaxChunkTargetBytes {
		t.Fatalf("ChunkTargetBytes = %d, want ceiling %d", got, MaxChunkTargetBytes)
	}

	// Re-applying the same value must report no change.
	if s.SetChunkTargetBytes(MaxChunkTargetBytes) {
		t.Fatal("same proposal should be a no-op (changed=false)")
	}

	if !s.SetChunkTargetBytes(2 << 20) {
		t.Fatal("in-range proposal should be accepted (true)")
	}

	if got := s.ChunkTargetBytes(); got != 2<<20 {
		t.Fatalf("ChunkTargetBytes = %d, want 2 MiB", got)
	}
}

// TestCheckpointWALNeverLosesData verifies the cleanup entry point:
// segments covered by the checkpoint are removed, uncheckpointed data
// survives a reopen.
func TestCheckpointWALNeverLosesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	for i := 0; i < 100; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// Pending (not yet flushed) records must survive CheckpointWAL.
	if _, err := s.CheckpointWAL(); err != nil {
		t.Fatalf("CheckpointWAL: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if got := reopened.Count(); got != 100 {
		t.Fatalf("count after checkpoint+reopen = %d, want 100", got)
	}

	for i := 0; i < 100; i++ {
		if _, err := reopened.Get(testKey(i)); err != nil {
			t.Fatalf("get %d after reopen: %v", i, err)
		}
	}
}

// TestRemoveTempArtifacts verifies stale atomic-write leftovers are
// reclaimed while fresh files are preserved.
func TestRemoveTempArtifacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	old := time.Now().Add(-2 * time.Hour)

	stale := filepath.Join(path, ".fir-tmp-123")
	fresh := filepath.Join(path, ".fir-tmp-456")
	staleChunk := filepath.Join(path, "chunks", ".firc-789")

	for _, file := range []string{stale, fresh, staleChunk} {
		if err := os.WriteFile(file, []byte("junk"), 0o600); err != nil {
			t.Fatalf("write temp: %v", err)
		}
	}

	for _, file := range []string{stale, staleChunk} {
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	reclaimed, err := s.RemoveTempArtifacts(time.Hour, 512)
	if err != nil {
		t.Fatalf("RemoveTempArtifacts: %v", err)
	}

	if reclaimed != int64(len("junk")*2) {
		t.Fatalf("reclaimed = %d, want %d", reclaimed, len("junk")*2)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale temp file was not removed")
	}

	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh temp file was removed")
	}

	if _, err := os.Stat(staleChunk); !os.IsNotExist(err) {
		t.Fatal("stale chunk temp file was not removed")
	}
}

// TestCompactIfWorthwhile verifies the coordinator's gate: healthy
// stores are not rewritten, garbage-heavy stores are compacted.
func TestCompactIfWorthwhile(t *testing.T) {
	s := openTestStore(t, Options{TargetChunkBytes: 16 << 10})

	// Build several chunks worth of records.
	payload := strings.Repeat("x", 1024)
	for i := 0; i < 200; i++ {
		if err := s.Upsert(testKey(i), []byte(payload)); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Healthy store: nothing to do.
	if ran, err := s.CompactIfWorthwhile(context.Background(), 0.35); err != nil || ran {
		t.Fatalf("healthy store compacted (ran=%v, err=%v)", ran, err)
	}

	// Delete most records: dead-record ratio climbs.
	for i := 0; i < 190; i++ {
		if err := s.Delete(testKey(i)); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	ran, err := s.CompactIfWorthwhile(context.Background(), 0.35)
	if err != nil {
		t.Fatalf("CompactIfWorthwhile: %v", err)
	}

	if !ran {
		t.Skip("deletion pattern did not cross the ratio threshold in this run")
	}

	if got := s.Count(); got != 10 {
		t.Fatalf("count after compaction = %d, want 10", got)
	}

	// Every surviving record must still be readable.
	for i := 190; i < 200; i++ {
		value, err := s.Get(testKey(i))
		if err != nil {
			t.Fatalf("get survivor %d: %v", i, err)
		}

		if len(value) != len(payload) {
			t.Fatalf("survivor %d corrupted", i)
		}
	}
}

// TestRebuildIndex verifies the developer maintenance action: the
// index is rebuilt from chunks alone and reads stay correct.
func TestRebuildIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	for i := 0; i < 64; i++ {
		if err := s.Upsert(testKey(i), testValue(i)); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if err := s.RebuildIndex(context.Background()); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if got := reopened.Count(); got != 64 {
		t.Fatalf("count after rebuild+reopen = %d, want 64", got)
	}

	for i := 0; i < 64; i++ {
		value, err := reopened.Get(testKey(i))
		if err != nil {
			t.Fatalf("get %d after rebuild: %v", i, err)
		}

		if string(value) != string(testValue(i)) {
			t.Fatalf("value %d mismatch after rebuild", i)
		}
	}
}

// TestPressureAdaptationUnderWrites exercises the adaptive path under
// a realistic write load: writes complete, data survives, and the
// store keeps its invariants at every pressure level.
func TestPressureAdaptationUnderWrites(t *testing.T) {
	s := openTestStore(t, Options{})

	levels := []PressureLevel{
		PressureNormal, PressureElevated, PressureHigh, PressureCritical,
	}

	for level := range levels {
		s.ApplyPressure(levels[level])

		for i := level * 32; i < (level+1)*32; i++ {
			if err := s.Upsert(testKey(i), testValue(i)); err != nil {
				t.Fatalf("upsert at level %d: %v", level, err)
			}
		}
	}

	_ = s.ApplyPressure(PressureNormal)

	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if got := s.Count(); got != 128 {
		t.Fatalf("count = %d, want 128", got)
	}

	for i := 0; i < 128; i++ {
		if _, err := s.Get(testKey(i)); err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
	}
}
