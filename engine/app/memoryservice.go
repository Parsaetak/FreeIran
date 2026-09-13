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
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/engine/booster"
	"github.com/Parsaetak/FreeIran/engine/mempressure"
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
	boost.OnChange(func(s booster.Settings) {
		a.initMu.Lock()
		tq := a.testQueue
		a.initMu.Unlock()

		concurrency := a.effectiveQueueConcurrency(s.QueueConcurrency)

		if tq != nil {
			tq.SetConcurrency(concurrency)
			tq.SetMaxQueueSize(s.QueueDepth)
		}

		if a.hotCache != nil {
			a.hotCache.SetMaxEntries(s.CacheEntries)
		}

		if a.logger != nil {
			a.logger.Info("app", "memory_adjust",
				"booster adjusted: workers=%d depth=%d cache=%d batch=%d",
				concurrency, s.QueueDepth, s.CacheEntries, s.BatchSize)
		}
	})

	// Pressure reactions beyond the gradual knobs: hard shedding at
	// the top of the band, logged so degradation is never silent.
	pressure.SetListener(func(old, new mempressure.State, snap mempressure.Snapshot) {
		switch new {
		case mempressure.StateHigh:
			if a.hotCache != nil {
				a.hotCache.Clear()
			}
		case mempressure.StateCritical:
			if a.hotCache != nil {
				a.hotCache.Clear()
			}
			if a.sourceCache != nil {
				a.sourceCache.Clear()
			}
			runtime.GC()
		}

		if a.logger != nil {
			a.logger.Warn("app", "memory_pressure",
				"pressure %s → %s (usage %.2f, heap %d MiB, rss %d MiB, cache %d MiB, queue %d MiB)",
				old, new, snap.UsageFraction,
				snap.HeapAlloc>>20, snap.RSS>>20,
				snap.CacheBytes>>20, snap.QueueBytes>>20)
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
