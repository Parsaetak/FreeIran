// Package metrics provides lightweight, local-only performance
// instrumentation for the FreeIran engine.
//
// Counters are atomic and safe for concurrent use. Durations are tracked
// as cumulative nanoseconds plus a call count so observers can derive
// averages without allocating per-call. Nothing here is sent anywhere:
// metrics are local diagnostics surfaced through the UI.
package metrics

import (
	"runtime"
	"sync/atomic"
	"time"
)

// Registry holds all engine performance counters.
type Registry struct {
	startedAt time.Time

	startupMS        atomic.Int64
	sourceFetchNS    atomic.Int64
	sourceFetchCount atomic.Int64
	parseNS          atomic.Int64
	parseCount       atomic.Int64
	normalizeNS      atomic.Int64
	normalizeCount   atomic.Int64
	dedupNS          atomic.Int64
	dedupCount       atomic.Int64
	persistNS        atomic.Int64
	persistCount     atomic.Int64

	cacheHits      atomic.Int64
	cacheMisses    atomic.Int64
	recordsIn      atomic.Int64
	recordsUnique  atomic.Int64
	recordsDupes   atomic.Int64
	chunksRead     atomic.Int64
	chunksWritten  atomic.Int64
	activeWorkers  atomic.Int64
	queueDepth     atomic.Int64
	testExecuted   atomic.Int64
	testWorking    atomic.Int64
	nativeFallback atomic.Int64
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{startedAt: time.Now().UTC()}
}

// ObserveDuration records one timed operation.
func (r *Registry) ObserveDuration(
	operation string,
	d time.Duration,
) {
	if r == nil {
		return
	}

	ns := d.Nanoseconds()

	switch operation {
	case "startup_ms":
		r.startupMS.Store(d.Milliseconds())

	case "source_fetch":
		r.sourceFetchNS.Add(ns)
		r.sourceFetchCount.Add(1)

	case "parse":
		r.parseNS.Add(ns)
		r.parseCount.Add(1)

	case "normalize":
		r.normalizeNS.Add(ns)
		r.normalizeCount.Add(1)

	case "dedup":
		r.dedupNS.Add(ns)
		r.dedupCount.Add(1)

	case "persist":
		r.persistNS.Add(ns)
		r.persistCount.Add(1)
	}
}

// Time executes operation and records its duration under name.
func (r *Registry) Time(name string, operation func()) {
	if r == nil || operation == nil {
		return
	}

	started := time.Now()

	operation()

	r.ObserveDuration(name, time.Since(started))
}

// AddCacheHit records a cache hit.
func (r *Registry) AddCacheHit(n int64) {
	if r != nil {
		r.cacheHits.Add(n)
	}
}

// AddCacheMiss records a cache miss.
func (r *Registry) AddCacheMiss(n int64) {
	if r != nil {
		r.cacheMisses.Add(n)
	}
}

// AddRecordsIn records records ingested before deduplication.
func (r *Registry) AddRecordsIn(n int64) {
	if r != nil {
		r.recordsIn.Add(n)
	}
}

// AddRecordsUnique records unique records after deduplication.
func (r *Registry) AddRecordsUnique(n int64) {
	if r != nil {
		r.recordsUnique.Add(n)
	}
}

// AddRecordsDupes records duplicates eliminated.
func (r *Registry) AddRecordsDupes(n int64) {
	if r != nil {
		r.recordsDupes.Add(n)
	}
}

// AddChunksRead records chunk files read.
func (r *Registry) AddChunksRead(n int64) {
	if r != nil {
		r.chunksRead.Add(n)
	}
}

// AddChunksWritten records chunk files written.
func (r *Registry) AddChunksWritten(n int64) {
	if r != nil {
		r.chunksWritten.Add(n)
	}
}

// AddTestExecuted records one executed configuration test.
func (r *Registry) AddTestExecuted(working bool) {
	if r == nil {
		return
	}

	r.testExecuted.Add(1)

	if working {
		r.testWorking.Add(1)
	}
}

// AddNativeFallback records that a native operation fell back to Go.
func (r *Registry) AddNativeFallback(n int64) {
	if r != nil {
		r.nativeFallback.Add(n)
	}
}

// SetActiveWorkers reports the current worker count.
func (r *Registry) SetActiveWorkers(n int64) {
	if r != nil {
		r.activeWorkers.Store(n)
	}
}

// SetQueueDepth reports the current bounded-queue depth.
func (r *Registry) SetQueueDepth(n int64) {
	if r != nil {
		r.queueDepth.Store(n)
	}
}

// Snapshot is a point-in-time view of all counters, safe to serialize
// to the UI.
type Snapshot struct {
	Version            string  `json:"version"`
	UptimeSeconds      float64 `json:"uptime_seconds"`
	StartupMS          int64   `json:"startup_ms"`
	SourceFetchMS      float64 `json:"source_fetch_ms"`
	SourceFetchCount   int64   `json:"source_fetch_count"`
	ParseMS            float64 `json:"parse_ms"`
	ParseCount         int64   `json:"parse_count"`
	NormalizeMS        float64 `json:"normalize_ms"`
	DedupMS            float64 `json:"dedup_ms"`
	PersistMS          float64 `json:"persist_ms"`
	CacheHits          int64   `json:"cache_hits"`
	CacheMisses        int64   `json:"cache_misses"`
	CacheHitRate       float64 `json:"cache_hit_rate"`
	RecordsProcessed   int64   `json:"records_processed"`
	RecordsUnique      int64   `json:"records_unique"`
	RecordsDedup       int64   `json:"records_deduplicated"`
	ChunksRead         int64   `json:"chunks_read"`
	ChunksWritten      int64   `json:"chunks_written"`
	MemoryEstimateMB   float64 `json:"memory_estimate_mb"`
	ActiveWorkers      int64   `json:"active_workers"`
	QueueDepth         int64   `json:"queue_depth"`
	TestsExecuted      int64   `json:"tests_executed"`
	TestsWorking       int64   `json:"tests_working"`
	NativeFallbackHits int64   `json:"native_fallback_hits"`
	NumGoroutine       int     `json:"num_goroutine"`
}

// ms converts cumulative nanoseconds and a call count into total
// milliseconds.
func ms(totalNS, count int64) float64 {
	if count == 0 {
		return 0
	}

	return float64(totalNS) / 1e6
}

// Snapshot returns the current counter values.
func (r *Registry) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}

	var mem runtime.MemStats

	// Soft statistic: reading memstats forces a short stop-the-world.
	// It is cheap enough for a diagnostics page and valuable for the UI.
	runtime.ReadMemStats(&mem)

	hits := r.cacheHits.Load()
	misses := r.cacheMisses.Load()

	var hitRate float64

	if total := hits + misses; total > 0 {
		hitRate = float64(hits) / float64(total)
	}

	return Snapshot{
		Version:            "see internal/version",
		UptimeSeconds:      time.Since(r.startedAt).Seconds(),
		StartupMS:          r.startupMS.Load(),
		SourceFetchMS:      ms(r.sourceFetchNS.Load(), r.sourceFetchCount.Load()),
		SourceFetchCount:   r.sourceFetchCount.Load(),
		ParseMS:            ms(r.parseNS.Load(), r.parseCount.Load()),
		ParseCount:         r.parseCount.Load(),
		NormalizeMS:        ms(r.normalizeNS.Load(), r.normalizeCount.Load()),
		DedupMS:            ms(r.dedupNS.Load(), r.dedupCount.Load()),
		PersistMS:          ms(r.persistNS.Load(), r.persistCount.Load()),
		CacheHits:          hits,
		CacheMisses:        misses,
		CacheHitRate:       hitRate,
		RecordsProcessed:   r.recordsIn.Load(),
		RecordsUnique:      r.recordsUnique.Load(),
		RecordsDedup:       r.recordsDupes.Load(),
		ChunksRead:         r.chunksRead.Load(),
		ChunksWritten:      r.chunksWritten.Load(),
		MemoryEstimateMB:   float64(mem.Alloc) / (1 << 20),
		ActiveWorkers:      r.activeWorkers.Load(),
		QueueDepth:         r.queueDepth.Load(),
		TestsExecuted:      r.testExecuted.Load(),
		TestsWorking:       r.testWorking.Load(),
		NativeFallbackHits: r.nativeFallback.Load(),
		NumGoroutine:       runtime.NumGoroutine(),
	}
}
