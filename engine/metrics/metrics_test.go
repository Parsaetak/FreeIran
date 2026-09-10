package metrics

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestSnapshotCounters(t *testing.T) {
	r := New()

	r.ObserveDuration("parse", 2_000_000) // 2ms
	r.ObserveDuration("parse", 3_000_000) // 3ms
	r.AddCacheHit(7)
	r.AddCacheMiss(3)
	r.AddRecordsIn(100)
	r.AddRecordsDupes(40)
	r.AddRecordsUnique(60)
	r.AddChunksRead(2)
	r.AddChunksWritten(1)
	r.AddTestExecuted(true)
	r.AddTestExecuted(false)
	r.SetActiveWorkers(4)
	r.SetQueueDepth(9)

	s := r.Snapshot()

	if s.ParseMS != 5.0 {
		t.Fatalf("ParseMS = %v, want 5.0", s.ParseMS)
	}

	if s.CacheHits != 7 || s.CacheMisses != 3 {
		t.Fatalf("cache counters wrong: %+v", s)
	}

	if s.CacheHitRate < 0.6999 || s.CacheHitRate > 0.7001 {
		t.Fatalf("CacheHitRate = %v, want 0.7", s.CacheHitRate)
	}

	if s.RecordsDedup != 40 || s.RecordsUnique != 60 {
		t.Fatalf("record counters wrong: %+v", s)
	}

	if s.ChunksRead != 2 || s.ChunksWritten != 1 {
		t.Fatalf("chunk counters wrong: %+v", s)
	}

	if s.TestsExecuted != 2 || s.TestsWorking != 1 {
		t.Fatalf("test counters wrong: %+v", s)
	}

	if s.ActiveWorkers != 4 || s.QueueDepth != 9 {
		t.Fatalf("worker counters wrong: %+v", s)
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	var r *Registry

	r.ObserveDuration("parse", 0)
	r.AddCacheHit(1)
	r.AddCacheMiss(1)

	s := r.Snapshot()

	if s.CacheHits != 0 {
		t.Fatal("nil registry snapshot should be zero valued")
	}
}

func TestSnapshotIsSerializable(t *testing.T) {
	r := New()
	r.AddCacheHit(1)

	data, err := json.Marshal(r.Snapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("empty snapshot json")
	}
}

func TestConcurrentSnapshot(t *testing.T) {
	r := New()

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := 0; j < 100; j++ {
				r.AddCacheHit(1)
				r.ObserveDuration("dedup", 1000)
				_ = r.Snapshot()
			}
		}()
	}

	wg.Wait()
}
