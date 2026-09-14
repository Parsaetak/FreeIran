// Package mempressure implements a central memory-pressure controller
// for FreeIran.
//
// The controller tracks several memory consumers:
//
//   - Go heap allocation (HeapAlloc from runtime.ReadMemStats)
//   - live heap (HeapAlloc after GC)
//   - process RSS (read from /proc/self/statm on Linux; best-effort
//     0 on other platforms)
//   - GC activity (percentage of time spent in GC)
//   - native arena usage (reported by the native layer)
//   - cache bytes (reported by cache layers via SetCacheBytes)
//   - queue memory estimate (reported by the test queue via SetQueueBytes)
//   - pending write bytes (reported by the store via SetPendingWriteBytes)
//   - source-buffer and parser-buffer memory (reported by the pipeline)
//
// Based on the combined picture, the controller computes a Pressure
// State with hysteresis:
//
//	Normal → Elevated → High → Critical
//
// Transitions up happen when the usage fraction (current / ceiling)
// exceeds the up threshold; transitions down happen when it drops
// below the down threshold (which is lower, to avoid oscillation).
//
// Subsystems register Listener callbacks to receive state changes and
// react (e.g. the booster reduces concurrency, the cache flushes, the
// store accelerates chunk flushes).
//
// The controller is safe for concurrent use. Sample() should be called
// on a ticker (e.g. every 2 seconds) from a single goroutine.
package mempressure

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// State is the memory pressure level.
type State int

const (
	// StateNormal: memory is comfortable. Subsystems run at full capacity.
	StateNormal State = iota

	// StateElevated: memory is moderately used. Subsystems should
	// reduce concurrency slightly and avoid warming unnecessary data.
	StateElevated

	// StateHigh: memory is scarce. Subsystems should aggressively
	// reduce concurrency, flush selected caches, and accelerate
	// chunk flushes.
	StateHigh

	// StateCritical: memory is exhausted. Subsystems should shed all
	// non-essential memory, release native free lists, and invoke GC.
	StateCritical
)

// String returns a human-readable state name.
func (s State) String() string {
	switch s {
	case StateNormal:
		return "normal"
	case StateElevated:
		return "elevated"
	case StateHigh:
		return "high"
	case StateCritical:
		return "critical"
	}
	return "unknown"
}

// Ceiling is the hard memory budget for the process. The controller
// computes usage fractions against this. Defaults are conservative
// for a desktop VPN client.
type Ceiling struct {
	// HeapBytes is the soft ceiling for Go heap allocation.
	HeapBytes uint64

	// RSSBytes is the soft ceiling for process RSS.
	RSSBytes uint64

	// GCPct is the maximum acceptable GC CPU fraction (0–100).
	GCPct float64

	// ArenaBytes is the ceiling for native arena allocation.
	ArenaBytes uint64

	// CacheBytes is the ceiling for all cache layers combined.
	CacheBytes uint64

	// QueueBytes is the ceiling for the test queue's memory.
	QueueBytes uint64
}

// DefaultCeiling returns conservative defaults for a desktop client
// (512 MiB heap, 1 GiB RSS, 20% GC, 128 MiB arena, 256 MiB cache,
// 128 MiB queue).
func DefaultCeiling() Ceiling {
	return Ceiling{
		HeapBytes:  512 << 20,
		RSSBytes:   1024 << 20,
		GCPct:      20.0,
		ArenaBytes: 128 << 20,
		CacheBytes: 256 << 20,
		QueueBytes: 128 << 20,
	}
}

// Snapshot is a point-in-time view of all memory consumers. It is
// safe to copy and surface to the UI.
type Snapshot struct {
	State         State     `json:"state"`
	HeapAlloc     uint64    `json:"heap_alloc"`
	HeapInUse     uint64    `json:"heap_in_use"`
	RSS           uint64    `json:"rss"`
	GCCPUFraction float64   `json:"gc_cpu_fraction"`
	ArenaBytes    uint64    `json:"arena_bytes"`
	CacheBytes    uint64    `json:"cache_bytes"`
	QueueBytes    uint64    `json:"queue_bytes"`
	PendingWrite  uint64    `json:"pending_write"`
	SourceBuffers uint64    `json:"source_buffers"`
	ParserBuffers uint64    `json:"parser_buffers"`
	UsageFraction float64   `json:"usage_fraction"` // 0..1, weighted
	SampledAt     time.Time `json:"sampled_at"`
	Ceiling       Ceiling   `json:"-"`
}

// Listener is invoked whenever the pressure state changes.
type Listener func(old, new State, snap Snapshot)

// Controller tracks memory pressure and notifies listeners on state
// changes. It is safe for concurrent use: Sample() runs on a single
// goroutine, but Set* methods may be called from any goroutine.
type Controller struct {
	mu       sync.Mutex
	ceiling  Ceiling
	state    State
	listener Listener

	// Live counters updated by subsystems (atomic).
	arenaBytes    atomic.Uint64
	cacheBytes    atomic.Uint64
	queueBytes    atomic.Uint64
	pendingWrite  atomic.Uint64
	sourceBuffers atomic.Uint64
	parserBuffers atomic.Uint64

	// up/down thresholds for hysteresis. Indexed by current state.
	up   []float64 // fraction at which we transition UP from state i
	down []float64 // fraction at which we transition DOWN from state i

	// last mirrors the most recent Sample() result. Snapshot() serves
	// it without re-sampling: ReadMemStats is a documented
	// stop-the-world pause and the sampler ticker keeps the stored
	// value at most one sampling interval old.
	last Snapshot
}

// New constructs a Controller with the given ceiling. If ceiling is
// zero-valued, DefaultCeiling is used.
func New(ceiling Ceiling) *Controller {
	if ceiling.HeapBytes == 0 {
		ceiling = DefaultCeiling()
	}
	return &Controller{
		ceiling: ceiling,
		state:   StateNormal,
		// Hysteresis bands: transition up when usage exceeds up[i],
		// transition down when usage drops below down[i]. The gap
		// between up[i] and down[i+1] prevents oscillation.
		up:   []float64{0.60, 0.75, 0.88, 1.00},
		down: []float64{0.00, 0.45, 0.60, 0.72},
	}
}

// SetListener installs a state-change callback. Must be called before
// the first Sample(). Returns the controller for chaining.
func (c *Controller) SetListener(fn Listener) *Controller {
	c.mu.Lock()
	c.listener = fn
	c.mu.Unlock()
	return c
}

// State returns the current pressure state (thread-safe).
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Ceiling returns the configured ceiling.
func (c *Controller) Ceiling() Ceiling {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ceiling
}

// SetCeiling updates the ceiling. Takes effect on the next Sample().
func (c *Controller) SetCeiling(ceiling Ceiling) {
	c.mu.Lock()
	c.ceiling = ceiling
	c.mu.Unlock()
}

// SetArenaBytes reports the current native arena usage.
func (c *Controller) SetArenaBytes(n uint64) { c.arenaBytes.Store(n) }

// SetCacheBytes reports the current cache layer usage (sum of all layers).
func (c *Controller) SetCacheBytes(n uint64) { c.cacheBytes.Store(n) }

// SetQueueBytes reports the current test queue memory estimate.
func (c *Controller) SetQueueBytes(n uint64) { c.queueBytes.Store(n) }

// SetPendingWriteBytes reports the current pending WAL/flush bytes.
func (c *Controller) SetPendingWriteBytes(n uint64) { c.pendingWrite.Store(n) }

// SetSourceBufferBytes reports the current source-buffer memory.
func (c *Controller) SetSourceBufferBytes(n uint64) { c.sourceBuffers.Store(n) }

// SetParserBufferBytes reports the current parser-buffer memory.
func (c *Controller) SetParserBufferBytes(n uint64) { c.parserBuffers.Store(n) }

// Sample reads the current memory state, computes the usage fraction,
// applies hysteresis to determine the new state, and fires the listener
// if the state changed. Should be called on a ticker from a single
// goroutine.
func (c *Controller) Sample() Snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	c.mu.Lock()
	ceiling := c.ceiling
	oldState := c.state
	c.mu.Unlock()

	arena := c.arenaBytes.Load()
	cache := c.cacheBytes.Load()
	queue := c.queueBytes.Load()
	pending := c.pendingWrite.Load()
	srcBuf := c.sourceBuffers.Load()
	parserBuf := c.parserBuffers.Load()

	// Compute per-consumer fractions, then take the max as the overall
	// pressure signal. This is conservative: any single consumer at
	// ceiling triggers the corresponding state.
	heapFrac := float64(ms.HeapAlloc) / float64(ceiling.HeapBytes)
	if ceiling.HeapBytes == 0 {
		heapFrac = 0
	}
	rss := rssBytes()
	rssFrac := float64(rss) / float64(ceiling.RSSBytes)
	if ceiling.RSSBytes == 0 {
		rssFrac = 0
	}
	gcFrac := ms.GCCPUFraction * 100 / ceiling.GCPct
	if ceiling.GCPct == 0 {
		gcFrac = 0
	}
	arenaFrac := float64(arena) / float64(ceiling.ArenaBytes)
	if ceiling.ArenaBytes == 0 {
		arenaFrac = 0
	}
	cacheFrac := float64(cache) / float64(ceiling.CacheBytes)
	if ceiling.CacheBytes == 0 {
		cacheFrac = 0
	}
	queueFrac := float64(queue) / float64(ceiling.QueueBytes)
	if ceiling.QueueBytes == 0 {
		queueFrac = 0
	}

	usage := heapFrac
	for _, f := range []float64{rssFrac, gcFrac, arenaFrac, cacheFrac, queueFrac} {
		if f > usage {
			usage = f
		}
	}
	if usage > 1.0 {
		usage = 1.0
	}

	snap := Snapshot{
		HeapAlloc:     ms.HeapAlloc,
		HeapInUse:     ms.HeapInuse,
		RSS:           rss,
		GCCPUFraction: ms.GCCPUFraction,
		ArenaBytes:    arena,
		CacheBytes:    cache,
		QueueBytes:    queue,
		PendingWrite:  pending,
		SourceBuffers: srcBuf,
		ParserBuffers: parserBuf,
		UsageFraction: usage,
		SampledAt:     time.Now().UTC(),
		Ceiling:       ceiling,
	}

	// Apply hysteresis. Loop so a single Sample can transition
	// multiple levels (e.g. Normal → Critical on a sudden spike).
	// Each iteration checks one level up and one level down against
	// the current state's thresholds; when neither fires, we stop.
	newState := oldState
	for {
		next := newState
		// Try to move up.
		if int(newState) < len(c.up)-1 && usage >= c.up[newState] {
			next = newState + 1
		}
		// Try to move down (only if we didn't just move up, to avoid
		// oscillation within one Sample).
		if next == newState && int(newState) > 0 && usage < c.down[newState] {
			next = newState - 1
		}
		if next == newState {
			break
		}
		newState = next
	}

	snap.State = newState

	c.mu.Lock()
	c.state = newState
	listener := c.listener
	c.last = snap
	c.mu.Unlock()

	if listener != nil && newState != oldState {
		listener(oldState, newState, snap)
	}

	return snap
}

// Snapshot returns the last-computed snapshot (without re-sampling).
// The returned Snapshot is a value copy. Before the first Sample()
// it returns the zero Snapshot; the application's memory sampler
// starts with the engine, so UI readers always observe a sampled
// value. Re-sampling here would defeat the point: ReadMemStats is a
// stop-the-world pause, not a cheap read, and every Diagnostics visit
// stacked one on top of the 2 s sampler's own pauses.
func (c *Controller) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.last
}
