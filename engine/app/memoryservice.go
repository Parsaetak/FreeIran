// engine/app/memoryservice.go
//
// Memory Booster 2.0 — the unified adaptive memory controller.
//
// v0.7.0 shipped engine/mempressure (the pressure classifier) and
// engine/booster (the adaptive settings controller) as standalone
// libraries with NO runtime wiring: nothing sampled them, nothing
// reported to them, and nothing reacted to them. This service composes
// them with the real application subsystems instead of creating a
// parallel controller:
//
//	reporters (pulled every sample):
//	  - cache bytes        source + hot-config cache layers
//	  - queue memory       testqueue.MemoryEstimate
//	  - pending writes     store memtable + WAL bytes (Inspect)
//	  - queue backlog / active workers / throughput / cache hit rate
//
//	adaptive actions (pushed on change):
//	  - testqueue.SetConcurrency  (worker pool grows/shrinks)
//	  - testqueue.SetMaxQueueSize (queue-depth shedding)
//	  - hot-cache SetMaxEntries   (cache target)
//	  - critical pressure: cache shedding + runtime.GC
//
// The controller runs one sampler goroutine (2s) and one booster tick
// (5s); every action is logged and surfaced through Diagnostics, so a
// degraded machine is explainable — never silently slower.
package app

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/engine/booster"
	"github.com/Parsaetak/FreeIran/engine/mempressure"
	"github.com/Parsaetak/FreeIran/engine/store"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// SampleInterval is the pressure sampling cadence.
const SampleInterval = 2 * time.Second

// BoosterInterval is the adaptive-settings tick cadence.
const BoosterInterval = 5 * time.Second

// MemoryService owns the unified adaptive memory controller.
type MemoryService struct {
	app      *App
	pressure *mempressure.Controller
	boost    *booster.Controller

	stopCh      chan struct{}
	stopOnce    sync.Once
	samplesDone atomic.Int64
}

// boosterPolicyChanged reports whether a booster proposal materially
// differs from the previously applied settings (v0.9.7: the trigger
// for one "memory_policy_changed" record; stable operation logs
// nothing).
func boosterPolicyChanged(previous, next booster.Settings, effective int) bool {
	return previous.QueueConcurrency != next.QueueConcurrency ||
		previous.QueueDepth != next.QueueDepth ||
		previous.CacheEntries != next.CacheEntries ||
		previous.BatchSize != next.BatchSize ||
		previous.ChunkFlushBytes != next.ChunkFlushBytes ||
		previous.QueueConcurrency != effective
}

// severityRank ranks mempressure states for recovery detection.
func severityRank(s mempressure.State) int {
	switch s {
	case mempressure.StateCritical:
		return 4
	case mempressure.StateHigh:
		return 3
	case mempressure.StateElevated:
		return 2
	default:
		return 1
	}
}

// lastBoosterSettings returns the most recently applied booster
// settings (zero value before the first tick).
func (a *App) lastBoosterSettings() booster.Settings {
	a.boosterSettingsMu.Lock()
	defer a.boosterSettingsMu.Unlock()

	return a.lastBooster
}

// rememberBoosterSettings stores the applied booster settings.
func (a *App) rememberBoosterSettings(s booster.Settings) {
	a.boosterSettingsMu.Lock()
	a.lastBooster = s
	a.boosterSettingsMu.Unlock()
}

// MemorySnapshot is the structured, UI-bindable memory report. It
// combines the pressure picture, the adaptive settings and the
// subsystem measurements that produced them, so the diagnostics
// surface shows cause and effect together.
type MemorySnapshot struct {
	Pressure   mempressure.Snapshot `json:"pressure"`
	Booster    booster.Settings     `json:"booster"`
	QueueBytes int64                `json:"queue_bytes"`
	CacheBytes int64                `json:"cache_bytes"`
	PendingWAL int64                `json:"pending_write_bytes"`
	Samples    int64                `json:"samples"`
}

// newMemoryService wires the controllers to the application. It is
// called from New once the subsystems the reporters observe exist.
func newMemoryService(a *App) *MemoryService {
	pressure := mempressure.New(mempressure.DefaultCeiling())
	boost := booster.New(booster.DefaultLimits(), pressure)

	svc := &MemoryService{
		app:      a,
		pressure: pressure,
		boost:    boost,
		stopCh:   make(chan struct{}),
	}

	// Adaptive actions: apply the booster's settings to the live
	// subsystems the moment they change. Lazy-initialized subsystems
	// (the test queue) pick the current settings up on creation —
	// ensureQueue consults the service via currentSettings.
	// v0.9.1: a fixed developer override (DevQueueWorkers) wins over
	// the adaptive proposal so the override is never silently undone.
	// v0.9.2: the booster's chunk-flush proposal is finally wired to
	// the store (clamped by the store into its safe range).
	boost.OnChange(func(s booster.Settings) {
		a.initMu.Lock()
		tq := a.testQueue
		a.initMu.Unlock()

		previous := a.lastBoosterSettings()

		concurrency := a.effectiveQueueConcurrency(s.QueueConcurrency)

		if tq != nil {
			tq.SetConcurrency(concurrency)
			tq.SetMaxQueueSize(s.QueueDepth)
		}

		if a.hotCache != nil {
			a.hotCache.SetMaxEntries(s.CacheEntries)
		}

		if a.store != nil {
			a.store.SetChunkTargetBytes(s.ChunkFlushBytes)
		}

		a.rememberBoosterSettings(s)

		// v0.9.5: routine booster adjustments are NO LONGER logged
		// (the old per-tick "memory_adjust" stream had no diagnostic
		// value). v0.9.7: instead of silence, emit ONE compact
		// "memory_policy_changed" record when a policy value
		// materially changes (workers, queue depth, cache target,
		// batch size) — normal stable operation stays silent.
		if a.logger != nil && boosterPolicyChanged(previous, s, concurrency) {
			a.logger.Log(logging.Record{
				Level:     logging.LevelInfo,
				Subsystem: "app",
				Event:     "memory_policy_changed",
				Message: fmt.Sprintf("memory policy adjusted: workers %d→%d, queue depth %d→%d, cache entries %d→%d",
					previous.QueueConcurrency, s.QueueConcurrency,
					previous.QueueDepth, s.QueueDepth,
					previous.CacheEntries, s.CacheEntries),
				Fields: map[string]any{
					"workers_from":       previous.QueueConcurrency,
					"workers_to":         s.QueueConcurrency,
					"workers_effective":  concurrency,
					"queue_depth_from":   previous.QueueDepth,
					"queue_depth_to":     s.QueueDepth,
					"cache_entries_from": previous.CacheEntries,
					"cache_entries_to":   s.CacheEntries,
					"batch_size":         s.BatchSize,
					"chunk_flush_bytes":  s.ChunkFlushBytes,
				},
			})
		}
	})

	// Pressure reactions (v0.9.2): memory pressure now controls the
	// FULL lifecycle, not just concurrency.
	//
	//   Normal    — normal caching/batching; light background cleanup
	//   Elevated  — smaller store batches, cold-cache eviction, stale
	//               runtime cleanup
	//   High      — smaller batches + aggressive flush, compaction of
	//               eligible data, staging cleanup
	//   Critical  — release non-essential caches and idle handles,
	//               smallest safe batches, GC
	//
	// Persistent user configuration (config/, sources, settings,
	// live chunks, core metadata) is NEVER a reclamation target.
	//
	// Logging: state transitions only — never every sampling tick.
	pressure.SetListener(func(old, new mempressure.State, snap mempressure.Snapshot) {
		level := storePressureLevel(new)

		if a.store != nil {
			if a.store.ApplyPressure(level) {
				records, bytes, chunkTarget := a.store.PressureLimits()

				if a.logger != nil {
					a.logger.Info("app", "memory_action",
						"store adapted: memtable freeze %d records / %d MiB, chunk target %d KiB",
						records, bytes>>20, chunkTarget>>10)
				}
			}
		}

		switch new {
		case mempressure.StateElevated:
			go a.runCleanupNow(a.ctx)

		case mempressure.StateHigh:
			if a.hotCache != nil {
				a.hotCache.Clear()
			}

			if a.store != nil {
				a.store.ForceFreeze() // flush pending writes
			}

			go a.runCleanupNow(a.ctx)

		case mempressure.StateCritical:
			if a.hotCache != nil {
				a.hotCache.Clear()
			}

			if a.sourceCache != nil {
				a.sourceCache.Clear()
			}

			if a.store != nil {
				a.store.ForceFreeze()
				a.store.CloseIdleHandles()
			}

			go a.runCleanupNow(a.ctx)

			runtime.GC()
		}

		if a.logger != nil {
			record := logging.Record{
				Level:     logging.LevelWarn,
				Subsystem: "app",
				Event:     "memory_pressure",
				Message: fmt.Sprintf("memory pressure %s → %s | reason: heap %d%% / cache %d%% / queue %d%% (heap %d MiB, cache %d MiB, queue %d MiB)",
					old, new,
					percentOf(snap.HeapAlloc, snap.Ceiling.HeapBytes),
					percentOf(snap.CacheBytes, snap.Ceiling.CacheBytes),
					percentOf(snap.QueueBytes, snap.Ceiling.QueueBytes),
					snap.HeapAlloc>>20, snap.CacheBytes>>20, snap.QueueBytes>>20),
				Status: new.String(),
				Fields: map[string]any{
					"from":      old.String(),
					"to":        new.String(),
					"heap_mib":  snap.HeapAlloc >> 20,
					"cache_mib": snap.CacheBytes >> 20,
					"queue_mib": snap.QueueBytes >> 20,
				},
			}

			// v0.9.7: upward transitions (toward pressure) are
			// actionable warnings; downward RECOVERY transitions
			// are informational — a machine healing must not look
			// like an incident.
			if severityRank(new) < severityRank(old) {
				record.Level = logging.LevelInfo
				record.Event = "memory_pressure_recovered"
			}

			a.logger.Log(record)
		}
	})

	return svc
}

// currentSettings exposes the booster settings so lazy subsystem
// creation (ensureQueue) starts at the adapted values instead of the
// static defaults.
func (m *MemoryService) currentSettings() booster.Settings {
	if m == nil {
		return booster.New(booster.DefaultLimits(), nil).Settings()
	}

	return m.boost.Settings()
}

// Start launches the sampler and booster goroutines.
func (m *MemoryService) Start() {
	if m == nil {
		return
	}

	go m.loop()
}

// Stop terminates the controller goroutines. Idempotent.
func (m *MemoryService) Stop() {
	if m == nil {
		return
	}

	m.stopOnce.Do(func() { close(m.stopCh) })
}

func (m *MemoryService) loop() {
	sampleTicker := time.NewTicker(SampleInterval)
	boostTicker := time.NewTicker(BoosterInterval)

	defer sampleTicker.Stop()
	defer boostTicker.Stop()

	for {
		select {
		case <-m.stopCh:
			return

		case <-sampleTicker.C:
			m.sample()

		case <-boostTicker.C:
			m.boost.Tick()
		}
	}
}

// sample pulls the live subsystem measurements into the pressure
// controller and classifies the new state. It is also the single
// place the workload inputs (backlog, hit rate, throughput, active
// workers) are refreshed for the booster.
func (m *MemoryService) sample() {
	a := m.app

	// Cache layers.
	var cacheBytes int64

	if a.sourceCache != nil {
		cacheBytes += a.sourceCache.Snapshot().Bytes
	}

	if a.hotCache != nil {
		cacheBytes += a.hotCache.Snapshot().Bytes
	}

	m.pressure.SetCacheBytes(clampU64(cacheBytes))

	// Test queue: memory estimate + workload inputs.
	var (
		queueBytes int64
		backlog    int64
		active     int64
		throughput float64
	)

	a.initMu.Lock()
	tq := a.testQueue
	a.initMu.Unlock()

	if tq != nil {
		stats := tq.Stats()
		queueBytes = tq.MemoryEstimate()
		backlog = int64(stats.QueueDepth)
		active = int64(stats.ActiveWorkers)
		throughput = stats.TestsPerSec

		a.metricsR.SetQueueDepth(backlog)
		a.metricsR.SetActiveWorkers(active)
	}

	m.pressure.SetQueueBytes(clampU64(queueBytes))

	// Store pending write memory: memtable + WAL.
	if a.store != nil {
		diag := a.store.Inspect()
		m.pressure.SetPendingWriteBytes(clampU64(diag.MemtableBytes + diag.WALBytes))
	}

	// Booster workload inputs.
	inputs := m.boost.Inputs()
	inputs.QueueBacklog.Store(backlog)
	inputs.ActiveWorkers.Store(active)
	inputs.Throughput.Store(uint64(throughput * 1000))

	if hitRate := a.combinedCacheHitRate(); hitRate >= 0 {
		inputs.CacheHitRate.Store(uint64(hitRate * 1e6))
	}

	snap := m.pressure.Sample()
	m.samplesDone.Add(1)

	// Mirror the classification into the metrics registry so the
	// engine metrics snapshot and the memory diagnostics page agree.
	a.metricsR.SetMemoryPressure(snap.State.String())
	a.metricsR.SetRSSBytes(int64(snap.RSS))
}

// Snapshot returns the combined structured memory report.
func (m *MemoryService) Snapshot() MemorySnapshot {
	if m == nil {
		return MemorySnapshot{}
	}

	a := m.app

	var queueBytes, cacheBytes, pending int64

	a.initMu.Lock()
	tq := a.testQueue
	a.initMu.Unlock()

	if tq != nil {
		queueBytes = tq.MemoryEstimate()
	}

	if a.sourceCache != nil {
		cacheBytes += a.sourceCache.Snapshot().Bytes
	}

	if a.hotCache != nil {
		cacheBytes += a.hotCache.Snapshot().Bytes
	}

	if a.store != nil {
		diag := a.store.Inspect()
		pending = diag.MemtableBytes + diag.WALBytes
	}

	return MemorySnapshot{
		Pressure:   m.pressure.Snapshot(),
		Booster:    m.boost.Settings(),
		QueueBytes: queueBytes,
		CacheBytes: cacheBytes,
		PendingWAL: pending,
		Samples:    m.samplesDone.Load(),
	}
}

// combinedCacheHitRate returns the aggregate cache hit rate across
// the live layers, or -1 when nothing has been measured yet.
func (a *App) combinedCacheHitRate() float64 {
	var hits, misses int64

	if a.sourceCache != nil {
		s := a.sourceCache.Snapshot()
		hits += s.Hits
		misses += s.Misses
	}

	if a.hotCache != nil {
		s := a.hotCache.Snapshot()
		hits += s.Hits
		misses += s.Misses
	}

	if hits+misses == 0 {
		return -1
	}

	return float64(hits) / float64(hits+misses)
}

// clampU64 maps negative counters (transient mid-snapshot states) to
// zero without lying about them.
func clampU64(n int64) uint64 {
	if n < 0 {
		return 0
	}

	return uint64(n)
}

// storePressureLevel maps the mempressure state to the store's
// pressure levels (the persistence layer stays decoupled from the
// mempressure library).
func storePressureLevel(state mempressure.State) store.PressureLevel {
	switch state {
	case mempressure.StateElevated:
		return store.PressureElevated
	case mempressure.StateHigh:
		return store.PressureHigh
	case mempressure.StateCritical:
		return store.PressureCritical
	default:
		return store.PressureNormal
	}
}

// percentOf renders used/ceiling as a bounded percentage for the
// pressure-transition reason line.
func percentOf(used, ceiling uint64) int64 {
	if ceiling == 0 {
		return 0
	}

	return int64(used * 100 / ceiling)
}
