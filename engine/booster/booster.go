// Package booster implements an adaptive runtime optimisation
// controller for FreeIran. It watches memory pressure, CPU pressure,
// queue backlog, cache hit rate, GC pressure, throughput and latency,
// and adjusts runtime parameters to improve both speed and memory
// efficiency — always respecting hard ceilings.
//
// The booster NEVER trades correctness or stability for throughput.
// When in doubt, it errs on the side of lower concurrency and smaller
// buffers.
//
// Parameters it adapts:
//
//   - worker concurrency (test queue, ingestion pipeline, parser)
//   - batch size (native batch operations, chunk flush)
//   - queue depth (test queue max size)
//   - cache targets (hot-config cache entry count)
//   - chunk flush size
//   - parser batching
//   - native batch thresholds
//
// It never raises a parameter above its configured hard ceiling, and
// never lowers it below its hard floor. Transitions are gradual (one
// step per tick) to avoid oscillation.
package booster

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/engine/mempressure"
)

// Limits are the hard floors and ceilings for each tunable parameter.
// The booster never violates these.
type Limits struct {
	// QueueConcurrency: [min, max] for the test queue worker count.
	QueueConcurrencyMin int
	QueueConcurrencyMax int

	// IngestionConcurrency: [min, max] for the pipeline fetch+parse workers.
	IngestionConcurrencyMin int
	IngestionConcurrencyMax int

	// ParserConcurrency: [min, max] for the parser worker count.
	ParserConcurrencyMin int
	ParserConcurrencyMax int

	// BatchSize: [min, max] for native batch operations.
	BatchSizeMin int
	BatchSizeMax int

	// QueueDepth: [min, max] for the test queue max size.
	QueueDepthMin int
	QueueDepthMax int

	// CacheEntries: [min, max] for the hot-config cache entry count.
	CacheEntriesMin int
	CacheEntriesMax int

	// ChunkFlushBytes: [min, max] for the chunk flush target.
	ChunkFlushBytesMin int
	ChunkFlushBytesMax int
}

// DefaultLimits returns conservative defaults for a desktop client.
func DefaultLimits() Limits {
	return Limits{
		QueueConcurrencyMin:     1,
		QueueConcurrencyMax:     8,
		IngestionConcurrencyMin: 1,
		IngestionConcurrencyMax: 8,
		ParserConcurrencyMin:    1,
		ParserConcurrencyMax:    6,
		BatchSizeMin:            64,
		BatchSizeMax:            8192,
		QueueDepthMin:           1000,
		QueueDepthMax:           50000,
		CacheEntriesMin:         512,
		CacheEntriesMax:         16384,
		ChunkFlushBytesMin:      256 << 10, // 256 KiB
		ChunkFlushBytesMax:      16 << 20,  // 16 MiB
	}
}

// Settings is the current set of tunable values. The booster writes
// these; subsystems read them via Snapshot().
type Settings struct {
	QueueConcurrency     int
	IngestionConcurrency int
	ParserConcurrency    int
	BatchSize            int
	QueueDepth           int
	CacheEntries         int
	ChunkFlushBytes      int
}

// Inputs are the workload signals the booster uses to decide
// adjustments. Subsystems report these via Set* methods.
type Inputs struct {
	// QueueBacklog: number of pending tasks in the test queue.
	QueueBacklog atomic.Int64

	// CacheHitRate: 0..1, rolling cache hit rate.
	CacheHitRate atomic.Uint64 // fixed-point: actual / 1e6

	// Throughput: tasks completed per second (test queue).
	Throughput atomic.Uint64 // fixed-point: actual * 1000

	// AvgLatencyMS: average test latency in milliseconds.
	AvgLatencyMS atomic.Int64

	// ActiveWorkers: current number of active test workers.
	ActiveWorkers atomic.Int64

	// CPUPressure: 0..1, fraction of CPU time used.
	CPUPressure atomic.Uint64 // fixed-point: actual / 1e6
}

// Controller adjusts Settings based on mempressure state + workload
// inputs. It is safe for concurrent use: Tick() runs on a single
// goroutine, but Set* methods may be called from any goroutine.
type Controller struct {
	mu        sync.RWMutex
	limits    Limits
	settings  Settings
	pressure  *mempressure.Controller
	listeners []func(Settings)

	// inputs holds the atomic workload signal store. Accessed via
	// Inputs(); the field is an embedded value (not a pointer) so the
	// Controller owns it and callers get a stable pointer.
	inputs Inputs
}

// New constructs a booster. If limits is zero-valued, DefaultLimits is
// used. pressure may be nil (the booster will treat pressure as Normal).
func New(limits Limits, pressure *mempressure.Controller) *Controller {
	if limits.QueueConcurrencyMax == 0 {
		limits = DefaultLimits()
	}
	s := Settings{
		QueueConcurrency:     limits.QueueConcurrencyMax / 2,
		IngestionConcurrency: limits.IngestionConcurrencyMax / 2,
		ParserConcurrency:    limits.ParserConcurrencyMax / 2,
		BatchSize:            limits.BatchSizeMax / 2,
		QueueDepth:           limits.QueueDepthMax / 2,
		CacheEntries:         limits.CacheEntriesMax / 2,
		ChunkFlushBytes:      limits.ChunkFlushBytesMax / 2,
	}
	// Clamp to [min, max].
	s.QueueConcurrency = clamp(s.QueueConcurrency, limits.QueueConcurrencyMin, limits.QueueConcurrencyMax)
	s.IngestionConcurrency = clamp(s.IngestionConcurrency, limits.IngestionConcurrencyMin, limits.IngestionConcurrencyMax)
	s.ParserConcurrency = clamp(s.ParserConcurrency, limits.ParserConcurrencyMin, limits.ParserConcurrencyMax)
	s.BatchSize = clamp(s.BatchSize, limits.BatchSizeMin, limits.BatchSizeMax)
	s.QueueDepth = clamp(s.QueueDepth, limits.QueueDepthMin, limits.QueueDepthMax)
	s.CacheEntries = clamp(s.CacheEntries, limits.CacheEntriesMin, limits.CacheEntriesMax)
	s.ChunkFlushBytes = clamp(s.ChunkFlushBytes, limits.ChunkFlushBytesMin, limits.ChunkFlushBytesMax)

	return &Controller{limits: limits, settings: s, pressure: pressure}
}

// Inputs returns a pointer to the atomic inputs struct so subsystems
// can update workload signals directly.
func (c *Controller) Inputs() *Inputs {
	return &c.inputs
}

// Settings returns the current settings (thread-safe snapshot).
func (c *Controller) Settings() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings
}

// OnChange registers a callback invoked whenever Settings change.
func (c *Controller) OnChange(fn func(Settings)) {
	c.mu.Lock()
	c.listeners = append(c.listeners, fn)
	c.mu.Unlock()
}

// Tick reads the current pressure state + workload inputs and adjusts
// Settings by one step. Should be called on a ticker (e.g. every 5s)
// from a single goroutine.
func (c *Controller) Tick() Settings {
	state := mempressure.StateNormal
	if c.pressure != nil {
		state = c.pressure.State()
	}

	c.mu.Lock()
	s := c.settings
	limits := c.limits
	listeners := c.listeners
	c.mu.Unlock()

	// Read workload inputs.
	backlog := c.inputs.QueueBacklog.Load()
	hitRate := float64(c.inputs.CacheHitRate.Load()) / 1e6
	throughput := float64(c.inputs.Throughput.Load()) / 1000.0
	activeWorkers := c.inputs.ActiveWorkers.Load()
	cpuPressure := float64(c.inputs.CPUPressure.Load()) / 1e6

	// Adjustment logic:
	//
	// - Pressure Normal + deep backlog + low CPU → raise concurrency.
	// - Pressure Normal + shallow backlog → hold.
	// - Pressure Elevated → hold concurrency, shrink caches slightly.
	// - Pressure High → cut concurrency by ~25%, shrink caches + buffers.
	// - Pressure Critical → cut concurrency to floor, shrink everything.
	//
	// We change at most ONE step per parameter per Tick to avoid
	// oscillation.

	adj := s // working copy

	switch state {
	case mempressure.StateNormal:
		// Grow if backlog is deep and we're not CPU-bound.
		if backlog > int64(adj.QueueConcurrency)*10 && cpuPressure < 0.7 {
			adj.QueueConcurrency = clamp(adj.QueueConcurrency+1, limits.QueueConcurrencyMin, limits.QueueConcurrencyMax)
		}
		if backlog > int64(adj.IngestionConcurrency)*50 && cpuPressure < 0.7 {
			adj.IngestionConcurrency = clamp(adj.IngestionConcurrency+1, limits.IngestionConcurrencyMin, limits.IngestionConcurrencyMax)
		}
		// Grow cache if hit rate is high (cache is useful).
		if hitRate > 0.8 && adj.CacheEntries < limits.CacheEntriesMax {
			adj.CacheEntries = clamp(adj.CacheEntries*2, limits.CacheEntriesMin, limits.CacheEntriesMax)
		}
		// Grow batch size if throughput is high.
		if throughput > 100 && adj.BatchSize < limits.BatchSizeMax {
			adj.BatchSize = clamp(adj.BatchSize*2, limits.BatchSizeMin, limits.BatchSizeMax)
		}

	case mempressure.StateElevated:
		// Hold concurrency. Shrink caches slightly if hit rate is low.
		if hitRate < 0.5 && adj.CacheEntries > limits.CacheEntriesMin {
			adj.CacheEntries = clamp(adj.CacheEntries/2, limits.CacheEntriesMin, limits.CacheEntriesMax)
		}
		// Shrink batch size slightly.
		if adj.BatchSize > limits.BatchSizeMin {
			adj.BatchSize = clamp(adj.BatchSize/2, limits.BatchSizeMin, limits.BatchSizeMax)
		}

	case mempressure.StateHigh:
		// Cut concurrency by ~25%.
		adj.QueueConcurrency = clamp(adj.QueueConcurrency*3/4, limits.QueueConcurrencyMin, limits.QueueConcurrencyMax)
		adj.IngestionConcurrency = clamp(adj.IngestionConcurrency*3/4, limits.IngestionConcurrencyMin, limits.IngestionConcurrencyMax)
		adj.ParserConcurrency = clamp(adj.ParserConcurrency*3/4, limits.ParserConcurrencyMin, limits.ParserConcurrencyMax)
		// Shrink caches and buffers.
		adj.CacheEntries = clamp(adj.CacheEntries/2, limits.CacheEntriesMin, limits.CacheEntriesMax)
		adj.BatchSize = clamp(adj.BatchSize/2, limits.BatchSizeMin, limits.BatchSizeMax)
		// Smaller chunk flushes to reduce peak memory.
		adj.ChunkFlushBytes = clamp(adj.ChunkFlushBytes/2, limits.ChunkFlushBytesMin, limits.ChunkFlushBytesMax)

	case mempressure.StateCritical:
		// Cut everything to the floor.
		adj.QueueConcurrency = limits.QueueConcurrencyMin
		adj.IngestionConcurrency = limits.IngestionConcurrencyMin
		adj.ParserConcurrency = limits.ParserConcurrencyMin
		adj.CacheEntries = limits.CacheEntriesMin
		adj.BatchSize = limits.BatchSizeMin
		adj.ChunkFlushBytes = limits.ChunkFlushBytesMin
		adj.QueueDepth = limits.QueueDepthMin
	}

	// On recovery from critical/high, gradually restore queue depth.
	if state == mempressure.StateNormal && s.QueueDepth < limits.QueueDepthMax {
		adj.QueueDepth = clamp(adj.QueueDepth+1000, limits.QueueDepthMin, limits.QueueDepthMax)
	}

	// Only fire listeners if something actually changed.
	changed := false
	c.mu.Lock()
	if adj != s {
		c.settings = adj
		changed = true
	}
	c.mu.Unlock()

	if changed {
		for _, fn := range listeners {
			fn(adj)
		}
	}

	_ = activeWorkers // reserved for future latency-based adjustment
	return adj
}

// clamp returns v clamped to [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// SetCacheHitRate reports the rolling cache hit rate (0..1).
func (i *Inputs) SetCacheHitRate(rate float64) {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	i.CacheHitRate.Store(uint64(rate * 1e6))
}

// SetThroughput reports the current throughput (tasks/sec).
func (i *Inputs) SetThroughput(perSec float64) {
	if perSec < 0 {
		perSec = 0
	}
	i.Throughput.Store(uint64(perSec * 1000))
}

// SetCPUPressure reports the CPU pressure (0..1).
func (i *Inputs) SetCPUPressure(p float64) {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	i.CPUPressure.Store(uint64(p * 1e6))
}

// CacheHitRate returns the reported hit rate as a float (0..1).
func (i *Inputs) CacheHitRateF() float64 {
	return float64(i.CacheHitRate.Load()) / 1e6
}

// ThroughputF returns the reported throughput as a float (tasks/sec).
func (i *Inputs) ThroughputF() float64 {
	return float64(i.Throughput.Load()) / 1000.0
}

// CPUPressureF returns the reported CPU pressure as a float (0..1).
func (i *Inputs) CPUPressureF() float64 {
	return float64(i.CPUPressure.Load()) / 1e6
}

// Start launches a background ticker that calls Tick() at the given
// interval. Returns a stop function. If pressure is nil, Tick treats
// the state as Normal.
func (c *Controller) Start(interval time.Duration) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				c.Tick()
			}
		}
	}()
	return func() { close(done) }
}
