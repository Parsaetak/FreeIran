// Package testqueue implements FreeIran's configuration test queue: a
// bounded-worker, priority-ordered, cancellable, persistent scheduler
// for testing proxy configurations.
//
// Design goals (per project specification):
//
//  1. Bounded worker pool — never start 10 tests of the same config
//     simultaneously, never spawn unbounded goroutines.
//  2. Priority queue — newly discovered configs and high-priority
//     configs jump ahead of bulk re-tests.
//  3. Duplicate task suppression by fingerprint — if a config is
//     already queued, the second Enqueue is a no-op (or updates the
//     priority, when higher).
//  4. Backend-aware concurrency limits — Xray tests, V2Ray tests and
//     sing-box tests each have their own per-backend concurrency cap
//     so a slow backend cannot starve others.
//  5. Cancellation — by task ID, by fingerprint, by source, or
//     global (Pause/Resume/Stop). In-flight tests have their per-task
//     context cancelled so the tester returns promptly.
//  6. Retry policy — failed tasks are retried up to MaxAttempts with
//     exponential backoff.
//  7. Per-config timeout + global test timeout.
//  8. Backpressure — when the queue is full, Enqueue blocks or
//     returns ErrQueueFull based on the EnqueueOption.
//  9. Graceful shutdown — Stop() drains in-flight tasks and returns.
//  10. Progress statistics — tests/sec, average duration, queue depth,
//     active workers, pass/fail counts, per-backend counts.
//
// State machine (single authoritative transition path):
//
//	(none) → Queued (Enqueue)
//	Queued → Preparing (worker dequeue)
//	Queued → Cancelled (Cancel*/Stop)  [finishTask]
//	Preparing → Testing (worker starts test)
//	Preparing → Cancelled (Cancel*)    [finishTask cancels testCtx]
//	Testing → Measuring (test passed, taking measurements)
//	Testing → Failed/TimedOut/Cancelled (test outcome / Cancel*)
//	Measuring → Passed (measurements done)
//	Measuring → Failed (measurement failed)
//
// Terminal states: Passed, Failed, TimedOut, Cancelled.
//
// finishTask is the ONLY function that moves a task to a terminal state
// and updates the global counters. It is idempotent (returns false if
// the task is already terminal), so workers and cancellation paths can
// both call it safely without double-counting. The per-task cancelFunc
// (set by the worker when it creates the test context, cleared when the
// test returns) is invoked by finishTask so an in-flight test unblocks
// promptly.
//
// The queue is intentionally backend-agnostic: it accepts a Tester
// interface (the engine/tester.Tester satisfies this) so the queue
// can be tested in isolation with a fake tester.
package testqueue

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Subsystem identifies the queue in structured logs.
const Subsystem = "testqueue"

// TaskState is the lifecycle state of one queued test.
type TaskState string

const (
	// StateQueued: task is in the queue waiting for a worker.
	StateQueued TaskState = "queued"

	// StatePreparing: worker picked up the task, building runtime config.
	StatePreparing TaskState = "preparing"

	// StateTesting: core process spawned, readiness probe in flight.
	StateTesting TaskState = "testing"

	// StateMeasuring: core is ready, latency probe in flight.
	StateMeasuring TaskState = "measuring"

	// StatePassed: test completed successfully.
	StatePassed TaskState = "passed"

	// StateFailed: test completed with a non-timeout failure.
	StateFailed TaskState = "failed"

	// StateTimedOut: test exceeded its per-config timeout.
	StateTimedOut TaskState = "timed_out"

	// StateCancelled: task was cancelled before completion.
	StateCancelled TaskState = "cancelled"
)

// IsTerminal reports whether the state is a final state (no further
// transitions allowed). Queued/Preparing/Testing/Measuring are
// transient; the rest are terminal.
func (s TaskState) IsTerminal() bool {
	switch s {
	case StatePassed, StateFailed, StateTimedOut, StateCancelled:
		return true
	}
	return false
}

// FailureCategory classifies why a test failed. Used by the UI to
// render failure chips and by the re-test policy to decide whether
// to retry.
type FailureCategory string

const (
	FailureNone          FailureCategory = ""
	FailureNetwork       FailureCategory = "network"
	FailureAuth          FailureCategory = "auth"
	FailureProtocol      FailureCategory = "protocol"
	FailureConfig        FailureCategory = "config"
	FailureBackendDown   FailureCategory = "backend_down"
	FailureBackendReject FailureCategory = "backend_reject"
	FailureTimeout       FailureCategory = "timeout"
	FailureCancelled     FailureCategory = "cancelled"
	FailureUnknown       FailureCategory = "unknown"
)

// Task is one queued test.
type Task struct {
	// ID is the queue-assigned task identifier (monotonic).
	ID int64 `json:"id"`

	// Fingerprint is the SHA-256 of the configuration under test.
	// Two tasks with the same fingerprint are duplicates; the queue
	// suppresses them by default.
	Fingerprint string `json:"fingerprint"`

	// Protocol is the configuration's protocol (vless, vmess, etc.).
	Protocol string `json:"protocol"`

	// Backends is the ordered list of candidate backends. The first
	// available + healthy backend is used; on failure the next is
	// tried.
	Backends []string `json:"backends,omitempty"`

	// Priority orders the queue. Higher value = higher priority.
	// New discoveries get +100; user-selected tests get +500.
	Priority int `json:"priority"`

	// Source is the source ID the config came from (for stats).
	Source string `json:"source,omitempty"`

	// Attempt is the current attempt number (1 = first try).
	Attempt int `json:"attempt"`

	// MaxAttempts is the retry cap. 1 = no retry.
	MaxAttempts int `json:"max_attempts"`

	// CreatedAt is when the task was enqueued.
	CreatedAt time.Time `json:"created_at"`

	// Deadline is the absolute time after which the task is
	// considered timed out. Zero = no per-task deadline (the
	// per-config Timeout is used instead).
	Deadline time.Time `json:"deadline,omitempty"`

	// State is the current lifecycle state.
	State TaskState `json:"state"`

	// Result is set when the task reaches a terminal state.
	Result Result `json:"result"`

	// --- unexported, runtime-only fields ---

	// mu guards State, Result, Attempt, cancelFunc, and heapIdx.
	// Lock order: q.mu → task.mu. A goroutine must NEVER acquire
	// q.mu while holding task.mu (would invert the order); task.mu
	// is for short critical sections only.
	mu sync.Mutex

	// cancelFunc, when non-nil, cancels the in-flight test context.
	// Set by the worker under task.mu before calling tester.Test;
	// cleared under task.mu after the test returns. Read by
	// finishTask under task.mu so cancellation unblocks the test.
	cancelFunc context.CancelFunc

	// heapIdx is the index of this task in the pending heap, or -1
	// if not in the heap. Guarded by q.mu (not task.mu) because the
	// heap is owned by the queue.
	heapIdx int
}

// Result is the outcome of one test.
type Result struct {
	Working bool          `json:"working"`
	Latency time.Duration `json:"latency"`
	// Measured is the authoritative measurement flag (v0.9.8.1,
	// engine/tester/latency.go rule R3): Latency/PingMS == 0 with
	// Measured == true is a measured sub-millisecond round trip.
	Measured        bool            `json:"measured,omitempty"`
	Backend         string          `json:"backend,omitempty"`
	TestedAt        time.Time       `json:"tested_at"`
	LastError       string          `json:"last_error,omitempty"`
	FailureCategory FailureCategory `json:"failure_category,omitempty"`

	// --- v0.9.0: honest test reporting (§4) ---
	Protocol   string `json:"protocol,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
	PingMS     int64  `json:"ping_ms,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Quality    string `json:"quality,omitempty"`
}

// Tester is the interface the queue calls to execute one test. The
// real implementation is engine/tester.Tester.
type Tester interface {
	Test(ctx context.Context, fingerprint string, backends []string) (Result, error)
}

// Stats is the runtime statistics block, surfaced to the UI.
type Stats struct {
	QueueDepth     int            `json:"queue_depth"`
	ActiveWorkers  int            `json:"active_workers"`
	TotalEnqueued  int64          `json:"total_enqueued"`
	TotalCompleted int64          `json:"total_completed"`
	TotalPassed    int64          `json:"total_passed"`
	TotalFailed    int64          `json:"total_failed"`
	TotalTimedOut  int64          `json:"total_timed_out"`
	TotalCancelled int64          `json:"total_cancelled"`
	TestsPerSec    float64        `json:"tests_per_sec"`
	AvgDurationMS  int64          `json:"avg_duration_ms"`
	PerBackend     map[string]int `json:"per_backend"`
	StartedAt      time.Time      `json:"started_at"`

	// v0.9.0 (§5): measured-latency aggregates for the live
	// progress panel. 0 = no successful measurement yet.
	AvgLatencyMS     int64 `json:"avg_latency_ms,omitempty"`
	FastestLatencyMS int64 `json:"fastest_latency_ms,omitempty"`
	SlowestLatencyMS int64 `json:"slowest_latency_ms,omitempty"`

	// v0.9.7 (§13/§14): temporary core-process accounting for the UI.
	// ActiveCores is the number of temporary protocol-core processes
	// alive right now (≤ CoreProbeConcurrency); CoreProbeConcurrency
	// is the effective cap.
	ActiveCores          int `json:"active_cores"`
	CoreProbeConcurrency int `json:"core_probe_concurrency"`
}

// Mode selects the testing mode. Each mode presets Concurrency,
// Timeout, MaxAttempts and MeasurementSamples.
type Mode string

const (
	// ModeQuick: 2 workers, 5s timeout, 1 attempt. For "is this
	// config alive?" checks.
	ModeQuick Mode = "quick"

	// ModeBalanced: default. 4 workers, 10s timeout, 2 attempts.
	ModeBalanced Mode = "balanced"

	// ModeDeep: 2 workers, 20s timeout, 3 attempts, 3 measurements
	// averaged. For "verify this config thoroughly".
	ModeDeep Mode = "deep"

	// ModeRetestFailed: only re-test configs whose last result was
	// failed or timed_out. 4 workers, 10s timeout, 2 attempts.
	ModeRetestFailed Mode = "retest_failed"

	// ModeTestAll: queue every untested config. Inherits Balanced
	// settings.
	ModeTestAll Mode = "test_all"

	// ModeTestSelected: queue the user's selection. Inherits Balanced
	// settings.
	ModeTestSelected Mode = "test_selected"

	// ModeContinuous: background queue that keeps working configs
	// fresh and re-tests stale ones. 2 workers, 10s timeout, 1
	// attempt. Runs until stopped.
	ModeContinuous Mode = "continuous"
)

// ModeConfig returns the queue settings for one mode.
func ModeConfig(m Mode) Config {
	switch m {
	case ModeQuick:
		return Config{Concurrency: 2, Timeout: 5 * time.Second, MaxAttempts: 1, Measurements: 1}
	case ModeBalanced:
		return Config{Concurrency: 4, Timeout: 10 * time.Second, MaxAttempts: 2, Measurements: 1}
	case ModeDeep:
		return Config{Concurrency: 2, Timeout: 20 * time.Second, MaxAttempts: 3, Measurements: 3}
	case ModeRetestFailed:
		return Config{Concurrency: 4, Timeout: 10 * time.Second, MaxAttempts: 2, Measurements: 1, OnlyFailed: true}
	case ModeTestAll:
		return Config{Concurrency: 4, Timeout: 10 * time.Second, MaxAttempts: 2, Measurements: 1}
	case ModeTestSelected:
		return Config{Concurrency: 4, Timeout: 10 * time.Second, MaxAttempts: 2, Measurements: 1}
	case ModeContinuous:
		return Config{Concurrency: 2, Timeout: 10 * time.Second, MaxAttempts: 1, Measurements: 1, Continuous: true}
	default:
		return ModeConfig(ModeBalanced)
	}
}

// Config tunes the queue.
type Config struct {
	// Concurrency is the global worker count. Default 4.
	// A value of 0 means NO workers — tasks remain queued until
	// cancelled or until the queue is restarted with workers. This
	// is used by tests that need to inspect the pending state.
	Concurrency int

	// PerBackendConcurrency caps how many tests of the same backend
	// may run in parallel. Default 2.
	PerBackendConcurrency int

	// Timeout is the per-test hard timeout. Default 10s.
	Timeout time.Duration

	// MaxAttempts is the retry cap per task. Default 2.
	MaxAttempts int

	// CoreProbeConcurrency caps how many temporary protocol-core
	// PROCESSES may be alive simultaneously during testing (v0.9.7
	// §14: 10,000 queued configurations must not imply uncontrolled
	// core-process creation). Default 2; hard ceiling
	// MaxCoreProbeConcurrency (4) — adaptive memory tuning may lower
	// the effective value under pressure but never raises it above
	// the ceiling. Queue workers (goroutines) may exceed this:
	// excess workers block on the core-slot semaphore (backpressure).
	CoreProbeConcurrency int

	// Measurements is how many latency samples to take and average
	// for a passing test. Default 1.
	Measurements int

	// MaxQueueSize bounds the queue. 0 = unbounded.
	MaxQueueSize int

	// Continuous puts the queue in continuous mode: it never goes
	// idle until Stop is called.
	Continuous bool

	// OnlyFailed filters the input to only tasks with a previous
	// failed/timed_out result.
	OnlyFailed bool
}

// DefaultConfig returns the Balanced defaults.
func DefaultConfig() Config {
	return ModeConfig(ModeBalanced)
}

// ErrQueueFull is returned when Enqueue is called with TryEnqueue and
// the queue is at capacity.
var ErrQueueFull = errors.New("testqueue: queue is full")

// Core-probe pool sizing (v0.9.7 §13/§14): memory availability must
// not automatically create more Xray/V2Ray/sing-box processes. The
// default cap of 2 keeps bulk testing light; 4 is the absolute
// maximum no adaptive tuner may exceed.
const (
	DefaultCoreProbeConcurrency = 2
	MaxCoreProbeConcurrency     = 4
)

// ErrDuplicate is returned when Enqueue is called for a fingerprint
// that is already queued and the EnqueueOption does not allow
// duplicates.
var ErrDuplicate = errors.New("testqueue: duplicate task")

// ErrQueueStopped is returned when Enqueue is called after Stop.
var ErrQueueStopped = errors.New("testqueue: queue stopped")

// EnqueueOption controls Enqueue behavior.
type EnqueueOption int

const (
	// EnqueueDefault: if the fingerprint is already queued, return
	// ErrDuplicate.
	EnqueueDefault EnqueueOption = iota

	// EnqueueAllowDuplicate: always add a new task, even if a
	// duplicate is in flight. Use for explicit re-test requests.
	EnqueueAllowDuplicate

	// EnqueueReplaceIfHigher: if the fingerprint is already queued,
	// replace its priority when the new priority is higher. Otherwise
	// return ErrDuplicate.
	EnqueueReplaceIfHigher

	// EnqueueTry: never block; return ErrQueueFull when at capacity.
	EnqueueTry
)

// ErrResize is returned by Dequeue when the worker pool was resized:
// idle workers wake so excess ones can retire promptly. External
// Dequeue callers should simply retry; it is a spurious wake, not a
// failure.
var ErrResize = errors.New("testqueue: worker pool resized")

// Queue is the test scheduler.
type Queue struct {
	tester Tester
	config Config

	mu            sync.Mutex
	pending       pendingHeap        // min-heap by (Priority desc, CreatedAt asc)
	inflight      map[int64]*Task    // tasks currently being tested
	byFingerprint map[string]*Task   // index for duplicate suppression (pending + inflight)
	results       map[string]*Result // last result per fingerprint (size-bounded LRU)
	nextID        int64

	workersWG sync.WaitGroup
	cancel    context.CancelFunc
	ctx       context.Context

	// desiredWorkers is the adaptive worker-pool target managed by
	// SetConcurrency (the memory booster's primary pressure valve).
	// liveWorkers tracks the actual pool size; workers above the
	// target retire at the loop top or on a resize wake.
	desiredWorkers atomic.Int32
	liveWorkers    atomic.Int32

	// Stats counters (atomic for hot paths).
	totalEnqueued  atomic.Int64
	totalCompleted atomic.Int64
	totalPassed    atomic.Int64
	totalFailed    atomic.Int64
	totalTimedOut  atomic.Int64
	totalCancelled atomic.Int64

	// per-backend counters
	backendMu  sync.Mutex
	perBackend map[string]int

	// Duration stats
	durationSum   atomic.Int64 // total duration in nanoseconds
	durationCount atomic.Int64

	// v0.9.0 latency stats (§5): average / fastest / slowest of the
	// measured pings across this queue's lifetime.
	latencySum     atomic.Int64
	latencyCount   atomic.Int64
	fastestLatency atomic.Int64
	slowestLatency atomic.Int64

	startedAt time.Time
	stopped   atomic.Bool

	// notifyCh is closed+recreated on every state change to wake
	// blocked Dequeue callers.
	notifyCh chan struct{}

	// resizeCh is closed+recreated by SetConcurrency to wake idle
	// workers so the pool can shrink without waiting for work.
	resizeCh chan struct{}

	// coreSlots is the bounded CORE-PROBE POOL (v0.9.7 §14): every
	// tester.Test call that spawns a temporary protocol-core
	// process holds exactly one slot. The queue may keep many
	// worker goroutines, but at most cap(coreSlots) temporary core
	// processes are alive at any moment; excess workers park here
	// (backpressure) instead of multiplying processes.
	coreSlots chan struct{}

	// activeCores counts in-flight tests holding a core slot.
	activeCores atomic.Int32

	// paused gates task pickup: while paused, workers block in
	// Dequeue and pending tasks stay queued (v0.9.7 bulk UX).
	paused atomic.Bool
}

// New constructs a Queue. The queue is not started; call Start.
//
// Config field defaults: Concurrency < 0 → 4 (Balanced); Concurrency
// == 0 → no workers (testing mode); PerBackendConcurrency ≤ 0 → 2;
// Timeout ≤ 0 → 10s; MaxAttempts ≤ 0 → 2; Measurements ≤ 0 → 1;
// MaxQueueSize == 0 → 10000.
func New(tester Tester, config Config) *Queue {
	if tester == nil {
		tester = NoopTester{}
	}
	if config.Concurrency < 0 {
		config.Concurrency = 4
	}
	if config.PerBackendConcurrency <= 0 {
		config.PerBackendConcurrency = 2
	}
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Second
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 2
	}
	if config.CoreProbeConcurrency <= 0 {
		config.CoreProbeConcurrency = DefaultCoreProbeConcurrency
	}
	// v0.9.7 hard ceiling: no configuration path may raise the
	// temporary-core process cap above the safe maximum.
	if config.CoreProbeConcurrency > MaxCoreProbeConcurrency {
		config.CoreProbeConcurrency = MaxCoreProbeConcurrency
	}
	if config.Measurements <= 0 {
		config.Measurements = 1
	}
	if config.MaxQueueSize == 0 {
		config.MaxQueueSize = 10000
	}

	q := &Queue{
		tester:        tester,
		config:        config,
		inflight:      make(map[int64]*Task),
		byFingerprint: make(map[string]*Task),
		results:       make(map[string]*Result),
		perBackend:    make(map[string]int),
		coreSlots:     make(chan struct{}, config.CoreProbeConcurrency),
		startedAt:     time.Now().UTC(),
		notifyCh:      make(chan struct{}),
		resizeCh:      make(chan struct{}),
	}
	q.pending.heap = make([]*Task, 0, 64)
	q.desiredWorkers.Store(int32(config.Concurrency))
	return q
}

// NoopTester is a Tester that does nothing; used when no real tester
// is configured (headless setup).
type NoopTester struct{}

// Test returns a placeholder result.
func (NoopTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	return Result{TestedAt: time.Now().UTC(), LastError: "no tester configured"}, nil
}

// Start launches the worker pool. Safe to call once; subsequent calls
// are no-ops. If config.Concurrency == 0, no workers are spawned —
// tasks remain queued until cancelled or until the queue is restarted
// with a positive concurrency (SetConcurrency).
func (q *Queue) Start(parentCtx context.Context) {
	q.mu.Lock()
	if q.ctx != nil {
		q.mu.Unlock()
		return
	}
	q.ctx, q.cancel = context.WithCancel(parentCtx)
	concurrency := q.config.Concurrency
	q.mu.Unlock()

	q.desiredWorkers.Store(int32(concurrency))

	for i := 0; i < concurrency; i++ {
		q.workersWG.Add(1)
		q.liveWorkers.Add(1)
		go q.worker(i)
	}
}

// SetConcurrency adjusts the worker-pool size at runtime — the
// memory booster's primary pressure valve. Growth is immediate;
// shrinkage takes effect as idle workers retire (busy workers finish
// their current task first, so no task is ever dropped). Safe to call
// concurrently; a no-op after Stop. Values are clamped to [0, 64].
func (q *Queue) SetConcurrency(n int) {
	// ctx is written under q.mu by Start; read it under the same lock
	// to stay race-free against a concurrent (re)start.
	q.mu.Lock()
	started := q.ctx != nil
	q.mu.Unlock()

	if q.stopped.Load() || !started {
		return
	}

	if n < 0 {
		n = 0
	}

	if n > 64 {
		n = 64
	}

	// v0.9.7: the WORKER pool may scale with memory/CPU availability,
	// but the CORE-PROBE cap is untouched — adaptive tuning must never
	// create more temporary protocol-core processes (§13). Workers
	// beyond the cap simply park on the core-slot semaphore.

	q.desiredWorkers.Store(int32(n))

	// Wake every idle worker: excess ones retire at the loop top;
	// the rest re-block. Waking is required so a shrink never waits
	// for the next task to arrive.
	q.mu.Lock()
	close(q.resizeCh)
	q.resizeCh = make(chan struct{})
	q.mu.Unlock()

	// Grow the pool to the target. The CAS loop prevents double
	// spawning under concurrent SetConcurrency calls. The stopped
	// re-check keeps the loop from registering a worker on workersWG
	// after Stop began waiting (WaitGroup misuse guard).
	for {
		if q.stopped.Load() {
			return
		}

		live := int(q.liveWorkers.Load())
		if live >= n {
			break
		}

		if q.liveWorkers.CompareAndSwap(int32(live), int32(live+1)) {
			q.workersWG.Add(1)
			go q.worker(live)
		}
	}
}

// SetMaxQueueSize adjusts the queue capacity bound at runtime.
// A smaller bound stops NEW enqueues (ErrQueueFull) but never drops
// already-queued tasks. The memory booster uses this to shed queue
// load under pressure.
func (q *Queue) SetMaxQueueSize(n int) {
	if n < 0 {
		n = 0
	}

	q.mu.Lock()
	q.config.MaxQueueSize = n
	q.mu.Unlock()
}

// taskBytesEstimate is the measured average resident cost of one
// queued or in-flight task: the Task struct, its fingerprint string
// (64-byte hex), protocol/source strings, backends slice header and
// the three map index entries. Rounded up to a whole cache line.
const taskBytesEstimate = 400

// DesiredWorkers reports the current worker-pool target (the value
// the memory booster last applied through SetConcurrency). It is a
// diagnostics accessor; use Stats().ActiveWorkers for the number of
// currently busy workers.
func (q *Queue) DesiredWorkers() int {
	return int(q.desiredWorkers.Load())
}

// MemoryEstimate returns the approximate heap bytes held by queued
// and in-flight tasks. It is O(1) (two map lengths under the lock) —
// deliberately an estimate rather than a per-task sum, because the
// memory-pressure controller samples it every two seconds. The
// estimate drives pressure classification, not billing.
func (q *Queue) MemoryEstimate() int64 {
	q.mu.Lock()
	depth := q.pending.Len() + len(q.inflight)
	q.mu.Unlock()

	return int64(depth) * taskBytesEstimate
}

// Stop drains in-flight tasks and stops the worker pool. Tasks still
// in the queue are NOT executed (use Drain first if you need that).
// In-flight tasks have their per-test context cancelled so they
// return promptly. Stop is idempotent.
func (q *Queue) Stop() {
	if q.stopped.Swap(true) {
		return
	}

	q.mu.Lock()
	if q.cancel != nil {
		q.cancel()
	}
	// Wake any blocked Dequeue callers so workers can observe ctx.Done.
	q.notifyLocked()
	q.mu.Unlock()

	q.workersWG.Wait()
}

// Drain blocks until the pending queue is empty AND no tasks are
// in-flight, or the context is cancelled.
func (q *Queue) Drain(ctx context.Context) error {
	for {
		q.mu.Lock()
		pending := q.pending.Len()
		inflight := len(q.inflight)
		q.mu.Unlock()

		if pending == 0 && inflight == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Pause suspends task pickup: in-flight tests run to completion (or
// cancellation) while queued tasks stay pending. Resume restores the
// configured worker pool. Idempotent; no-op after Stop.
func (q *Queue) Pause() {
	if q.stopped.Load() {
		return
	}

	q.paused.Store(true)
}

// Resume lifts a Pause and wakes the worker pool.
func (q *Queue) Resume() {
	q.paused.Store(false)

	q.mu.Lock()
	q.notifyLocked()
	q.mu.Unlock()
}

// Paused reports whether task pickup is suspended.
func (q *Queue) Paused() bool {
	return q.paused.Load()
}

// Enqueue adds one task. Returns the task ID, or an error.
func (q *Queue) Enqueue(
	fingerprint string,
	protocol string,
	backends []string,
	priority int,
	source string,
	opt EnqueueOption,
) (int64, error) {
	if q.stopped.Load() {
		return 0, ErrQueueStopped
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.config.MaxQueueSize > 0 && q.pending.Len() >= q.config.MaxQueueSize {
		return 0, ErrQueueFull
	}

	existing, dup := q.byFingerprint[fingerprint]
	if dup {
		switch opt {
		case EnqueueDefault:
			return existing.ID, ErrDuplicate
		case EnqueueReplaceIfHigher:
			if priority > existing.Priority {
				existing.Priority = priority
				heap.Fix(&q.pending, existing.heapIdx)
			}
			return existing.ID, ErrDuplicate
		case EnqueueAllowDuplicate, EnqueueTry:
			// fallthrough to create a new task
		}
	}

	q.nextID++
	task := &Task{
		ID:          q.nextID,
		Fingerprint: fingerprint,
		Protocol:    protocol,
		Backends:    backends,
		Priority:    priority,
		Source:      source,
		Attempt:     1,
		MaxAttempts: q.config.MaxAttempts,
		CreatedAt:   time.Now().UTC(),
		Deadline:    time.Now().Add(q.config.Timeout * time.Duration(q.config.MaxAttempts+1)),
		State:       StateQueued,
		heapIdx:     -1,
	}
	heap.Push(&q.pending, task)
	q.byFingerprint[fingerprint] = task
	q.totalEnqueued.Add(1)

	// Wake up one blocked worker.
	q.notifyLocked()

	return task.ID, nil
}

// notify wakes blocked Dequeue callers by closing the notifyCh and
// creating a new one. Caller MUST hold q.mu.
func (q *Queue) notifyLocked() {
	close(q.notifyCh)
	q.notifyCh = make(chan struct{})
}

// Dequeue blocks until a task is available, returning the task.
// Returns nil, ctx.Err() when the context is cancelled, and
// nil, ErrResize when the worker pool was resized (retry).
func (q *Queue) Dequeue(ctx context.Context) (*Task, error) {
	for {
		q.mu.Lock()

		// v0.9.7: a paused queue does not hand out tasks — pending
		// work stays queued until Resume. The notify channel wakes
		// paused workers so Resume is prompt.
		if q.pending.Len() > 0 && !q.paused.Load() {
			task := heap.Pop(&q.pending).(*Task)
			task.heapIdx = -1
			q.inflight[task.ID] = task

			task.mu.Lock()
			task.State = StatePreparing
			task.mu.Unlock()

			q.mu.Unlock()
			return task, nil
		}

		notifyCh := q.notifyCh
		resizeCh := q.resizeCh
		paused := q.paused.Load()
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-notifyCh:
			// loop and try again
		case <-resizeCh:
			// spurious wake for external callers; workers use it to
			// retire when the pool shrinks
			return nil, ErrResize
		case <-time.After(200 * time.Millisecond):
			// Pause poll: workers re-check the gate periodically so
			// Resume without a fresh enqueue is still observed even
			// if no notify fired.
			_ = paused
		}
	}
}

// Cancel marks a task as cancelled. If the task is in-flight, the
// tester's context is cancelled so the test returns promptly. Returns
// true if the task was cancelled by this call, false if it was already
// terminal or not found.
func (q *Queue) Cancel(taskID int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Search pending.
	for _, t := range q.pending.heap {
		if t.ID == taskID {
			return q.finishTaskLocked(t, StateCancelled, cancelledResult())
		}
	}
	// Search inflight.
	if t, ok := q.inflight[taskID]; ok {
		return q.finishTaskLocked(t, StateCancelled, cancelledResult())
	}
	return false
}

// CancelBySource cancels every queued AND in-flight task from one
// source. In-flight tasks have their test context cancelled. Returns
// the number of tasks cancelled by this call.
func (q *Queue) CancelBySource(sourceID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	cancelled := 0
	// Snapshot pending IDs, then finish each. We iterate the heap
	// directly but call finishTaskLocked which removes from the heap
	// via heap.Remove — so we collect the list first.
	var toCancel []*Task
	for _, t := range q.pending.heap {
		if t.Source == sourceID {
			toCancel = append(toCancel, t)
		}
	}
	for _, t := range toCancel {
		if q.finishTaskLocked(t, StateCancelled, cancelledResult()) {
			cancelled++
		}
	}
	// In-flight tasks.
	for _, t := range q.inflight {
		if t.Source == sourceID {
			if q.finishTaskLocked(t, StateCancelled, cancelledResult()) {
				cancelled++
			}
		}
	}
	return cancelled
}

// CancelByFingerprint cancels the task (pending or in-flight) matching
// the given fingerprint. Returns 1 if cancelled, 0 if not found or
// already terminal.
func (q *Queue) CancelByFingerprint(fp string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	t, ok := q.byFingerprint[fp]
	if !ok {
		return 0
	}
	if q.finishTaskLocked(t, StateCancelled, cancelledResult()) {
		return 1
	}
	return 0
}

// CancelAll cancels every queued AND in-flight task. In-flight tasks
// have their test context cancelled. Returns the number of tasks
// cancelled by this call.
func (q *Queue) CancelAll() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	cancelled := 0
	// Drain the heap.
	for q.pending.Len() > 0 {
		t := heap.Pop(&q.pending).(*Task)
		t.heapIdx = -1
		if q.finishTaskLocked(t, StateCancelled, cancelledResult()) {
			cancelled++
		}
	}
	// In-flight tasks.
	for _, t := range q.inflight {
		if q.finishTaskLocked(t, StateCancelled, cancelledResult()) {
			cancelled++
		}
	}
	return cancelled
}

// cancelledResult is the canonical Result for a cancelled task.
func cancelledResult() Result {
	return Result{
		TestedAt:        time.Now().UTC(),
		FailureCategory: FailureCancelled,
		LastError:       "cancelled",
	}
}

// finishTask is the public entry point for moving a task to a terminal
// state. It acquires q.mu. Returns true if the transition happened,
// false if the task was already terminal.
func (q *Queue) finishTask(task *Task, state TaskState, result Result) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.finishTaskLocked(task, state, result)
}

// finishTaskLocked moves a task to a terminal state and updates ALL
// bookkeeping: pending heap, inflight map, byFingerprint index,
// results cache, and global counters. It is the SINGLE authoritative
// state-transition path. Idempotent: returns false if the task is
// already terminal.
//
// Caller MUST hold q.mu.
func (q *Queue) finishTaskLocked(task *Task, state TaskState, result Result) bool {
	task.mu.Lock()
	if task.State.IsTerminal() {
		task.mu.Unlock()
		return false
	}
	task.State = state
	task.Result = result
	cf := task.cancelFunc
	task.cancelFunc = nil
	task.mu.Unlock()

	// Cancel the in-flight test context so the tester returns promptly.
	// Safe to call nil-checked; context.CancelFunc is safe to call
	// multiple times and after the test has returned.
	if cf != nil {
		cf()
	}

	// Remove from the pending heap if present.
	if task.heapIdx >= 0 {
		heap.Remove(&q.pending, task.heapIdx)
		task.heapIdx = -1
	}
	// Remove from inflight if present.
	delete(q.inflight, task.ID)
	// Remove from the fingerprint index so future Enqueue succeeds.
	delete(q.byFingerprint, task.Fingerprint)

	// Cache the result (bounded; evict one entry if at capacity).
	if len(q.results) >= resultsCacheLimit {
		for k := range q.results {
			delete(q.results, k)
			break
		}
	}
	r := result
	q.results[task.Fingerprint] = &r

	// Wake any blocked Dequeue/Enqueue callers.
	q.notifyLocked()

	// Update counters outside the lock (they're atomic). We do this
	// while still inside finishTaskLocked for ordering guarantees
	// with respect to the state change, but the atomics themselves
	// don't need the lock.
	q.totalCompleted.Add(1)
	switch state {
	case StatePassed:
		q.totalPassed.Add(1)
	case StateFailed:
		q.totalFailed.Add(1)
	case StateTimedOut:
		q.totalTimedOut.Add(1)
	case StateCancelled:
		q.totalCancelled.Add(1)
	}

	if result.Backend != "" {
		q.backendMu.Lock()
		q.perBackend[result.Backend]++
		q.backendMu.Unlock()
	}

	// Latency bookkeeping for the §5 live progress block.
	// v0.9.8.1: Measured is the authority (not ms > 0). Sub-ms
	// measurements are quantized to 1 ms for these coarse whole-ms
	// aggregates — 0 is the "no data yet" sentinel in the stats —
	// and ranking is fed from TestHistory, which keeps the honest 0.
	if result.Working && result.Measured {
		ms := result.PingMS
		if ms <= 0 {
			ms = result.Latency.Milliseconds()
		}

		if ms <= 0 {
			ms = 1 // measured sub-millisecond: safe quantization
		}

		q.recordLatency(ms)
	}

	return true
}

// resultsCacheLimit bounds the per-fingerprint result cache.
const resultsCacheLimit = 4096

// worker is the main test loop.
//
// Retirement (the shrink side of SetConcurrency) is a CAS decrement,
// not a read-then-deferred-decrement: each worker atomically claims
// one retirement slot, so the pool can never undershoot the desired
// size. The historical form let every worker observe the same stale
// liveWorkers count and ALL retire at once, leaving liveWorkers == 0
// with desiredWorkers > 0 — a dead pool no one respawns (spawning
// lives only in Start and SetConcurrency), so queued tasks never
// drained (v0.9.2 race-mode CI failure).
func (q *Queue) worker(id int) {
	defer q.workersWG.Done()

	for {
		// Retire when the pool is above target (shrink side of
		// SetConcurrency). Busy workers reach this check between
		// tasks; idle ones are woken by the resize signal.
		for {
			live := q.liveWorkers.Load()
			if live <= q.desiredWorkers.Load() {
				break // pool at/below target: keep working
			}

			if q.liveWorkers.CompareAndSwap(live, live-1) {
				return // retirement slot claimed: pool size decremented
			}

			// Another worker retired first: re-read and re-decide.
		}

		task, err := q.Dequeue(q.ctx)
		if err != nil {
			if errors.Is(err, ErrResize) {
				continue // re-evaluate retirement at the loop top
			}

			// Terminal exit (Stop cancelled the context). The pool is
			// dead by definition; keep liveWorkers accounting exact.
			q.liveWorkers.Add(-1)

			return
		}

		q.runTask(task)
	}
}

// runTask executes one task through its attempts. On any terminal
// outcome it calls finishTask (the single authoritative path). If the
// task was cancelled by a concurrent Cancel call, finishTask returns
// false and runTask simply returns without double-counting.
func (q *Queue) runTask(task *Task) {
	// Dequeue already set state to Preparing. Check for a racing
	// cancellation.
	task.mu.Lock()
	if task.State == StateCancelled {
		task.mu.Unlock()
		return // finishTask already handled bookkeeping
	}
	task.mu.Unlock()

	for attempt := 1; attempt <= task.MaxAttempts; attempt++ {
		// Check for cancellation before each attempt.
		task.mu.Lock()
		if task.State == StateCancelled {
			task.mu.Unlock()
			return
		}
		task.State = StateTesting
		task.Attempt = attempt
		task.mu.Unlock()

		// v0.9.7: acquire one bounded CORE-PROBE slot before spawning
		// the temporary core process. Workers beyond the cap park here
		// (backpressure) — thousands of queued configurations never
		// imply thousands of simultaneous processes. The short poll
		// keeps cancellation responsive while parked: a cancelled task
		// leaves the slot queue instead of waiting for a free slot.
		parked := false

		for !parked {
			task.mu.Lock()
			cancelled := task.State == StateCancelled
			task.mu.Unlock()

			if cancelled {
				return // finishTask already handled bookkeeping
			}

			select {
			case q.coreSlots <- struct{}{}:
				parked = true
			case <-q.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				// Re-check cancellation at loop top.
			}
		}

		testCtx, testCancel := context.WithTimeout(q.ctx, q.config.Timeout)

		// Publish the cancel func so Cancel can interrupt the test.
		task.mu.Lock()
		if task.State == StateCancelled {
			task.mu.Unlock()
			testCancel()
			<-q.coreSlots
			return
		}
		task.cancelFunc = testCancel
		task.mu.Unlock()

		q.activeCores.Add(1)

		start := time.Now()
		result, err := q.tester.Test(testCtx, task.Fingerprint, task.Backends)
		duration := time.Since(start)

		q.activeCores.Add(-1)
		<-q.coreSlots

		// Capture the test context's error BEFORE calling testCancel,
		// because testCancel itself sets testCtx.Err() = Canceled,
		// which would misclassify every failure as a cancellation.
		testCtxErr := testCtx.Err()
		testCancel()

		// Clear the cancel func and check for a racing cancellation.
		task.mu.Lock()
		task.cancelFunc = nil
		cancelledDuringTest := task.State == StateCancelled
		task.mu.Unlock()

		if cancelledDuringTest {
			// Cancel called finishTask during the test; discard result.
			q.recordDuration(duration)
			return
		}

		if err == nil && result.Working {
			// Take additional measurements if configured.
			if q.config.Measurements > 1 {
				totalLatency := result.Latency
				measurementsOK := true
				for i := 1; i < q.config.Measurements; i++ {
					mCtx, mCancel := context.WithTimeout(q.ctx, q.config.Timeout)
					task.mu.Lock()
					task.cancelFunc = mCancel
					task.mu.Unlock()

					m, mErr := q.tester.Test(mCtx, task.Fingerprint, task.Backends)
					mCancel()

					task.mu.Lock()
					task.cancelFunc = nil
					if task.State == StateCancelled {
						task.mu.Unlock()
						return
					}
					task.mu.Unlock()

					if mErr != nil || !m.Working {
						result = m
						if mErr != nil {
							result.LastError = mErr.Error()
						}
						measurementsOK = false
						break
					}
					totalLatency += m.Latency
				}
				if measurementsOK {
					result.Latency = totalLatency / time.Duration(q.config.Measurements)
				}
			}

			task.mu.Lock()
			if task.State == StateCancelled {
				task.mu.Unlock()
				return
			}
			task.State = StateMeasuring
			task.mu.Unlock()

			result.TestedAt = time.Now().UTC()
			if q.finishTask(task, StatePassed, result) {
				q.recordDuration(duration)
			}
			return
		}

		// Failure path. Use the captured testCtxErr (taken before
		// testCancel) so we classify the real test outcome, not the
		// post-cancel state.
		category := classifyCtxFailure(err, testCtxErr)
		result.FailureCategory = category
		result.LastError = errToString(err)
		result.TestedAt = time.Now().UTC()

		if category == FailureCancelled {
			q.finishTask(task, StateCancelled, result)
			return
		}

		if attempt < task.MaxAttempts {
			// Exponential backoff. Respect q.ctx for shutdown.
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-q.ctx.Done():
				q.finishTask(task, StateCancelled, cancelledResult())
				return
			case <-time.After(backoff):
				// retry
			}
			continue
		}

		// Final failure.
		finalState := StateFailed
		if testCtxErr == context.DeadlineExceeded {
			finalState = StateTimedOut
			result.FailureCategory = FailureTimeout
		}
		if q.finishTask(task, finalState, result) {
			q.recordDuration(duration)
		}
		return
	}
}

// recordDuration updates the running duration stats. Only called for
// tasks that actually ran a test (not for pre-test cancellations).
func (q *Queue) recordDuration(d time.Duration) {
	q.durationSum.Add(int64(d))
	q.durationCount.Add(1)
}

// recordLatency updates the running ping statistics (§5). Only
// successful, positively-measured results count.
func (q *Queue) recordLatency(ms int64) {
	if ms <= 0 {
		return
	}

	q.latencySum.Add(ms)
	q.latencyCount.Add(1)

	for {
		fastest := q.fastestLatency.Load()
		if fastest != 0 && fastest <= ms {
			break
		}

		if q.fastestLatency.CompareAndSwap(fastest, ms) {
			break
		}
	}

	for {
		slowest := q.slowestLatency.Load()
		if slowest >= ms {
			break
		}

		if q.slowestLatency.CompareAndSwap(slowest, ms) {
			break
		}
	}
}

// classifyFailure maps an error + context to a FailureCategory. This
// is the legacy entry point used by tests; production code uses
// classifyCtxFailure with a pre-captured context error.
// ClassifyByError is the exported classification entry point for
// callers outside the package (the app adapter maps tester errors to
// FailureCategory for the UI chips).
func ClassifyByError(err string) FailureCategory {
	if err == "" {
		return FailureNone
	}

	return classifyFailureString(err)
}

// classifyFailureString classifies from a pre-rendered error string.
func classifyFailureString(err string) FailureCategory {
	lower := strings.ToLower(err)

	switch {
	case strings.Contains(lower, "cancelled"), strings.Contains(lower, "canceled"):
		return FailureCancelled
	case strings.Contains(lower, "deadline"), strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		return FailureTimeout
	case strings.Contains(lower, "auth"), strings.Contains(lower, "password"), strings.Contains(lower, "uuid"), strings.Contains(lower, "unauthorized"):
		return FailureAuth
	case strings.Contains(lower, "handshake"), strings.Contains(lower, "protocol"):
		return FailureProtocol
	case strings.Contains(lower, "invalid"), strings.Contains(lower, "config"):
		return FailureConfig
	case strings.Contains(lower, "unreachable"), strings.Contains(lower, "refused"), strings.Contains(lower, "network"), strings.Contains(lower, "no route"):
		return FailureNetwork
	default:
		return FailureUnknown
	}
}

func classifyFailure(err error, ctx context.Context) FailureCategory {
	if err == nil {
		return FailureNone
	}
	return classifyCtxFailure(err, ctx.Err())
}

// classifyCtxFailure classifies a failure given the error and the
// test context's error (captured BEFORE the testCancel call so it
// reflects the real test outcome, not the post-cancel state).
func classifyCtxFailure(err error, ctxErr error) FailureCategory {
	if err == nil {
		return FailureNone
	}
	if ctxErr == context.DeadlineExceeded {
		return FailureTimeout
	}
	if ctxErr == context.Canceled {
		return FailureCancelled
	}
	msg := err.Error()
	switch {
	case contains(msg, "connection refused"), contains(msg, "no such host"), contains(msg, "i/o timeout"):
		return FailureNetwork
	case contains(msg, "auth"), contains(msg, "credential"):
		return FailureAuth
	case contains(msg, "protocol"):
		return FailureProtocol
	case contains(msg, "config"), contains(msg, "invalid"):
		return FailureConfig
	case contains(msg, "backend"):
		return FailureBackendDown
	default:
		return FailureUnknown
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func errToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Stats returns the current queue statistics.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	pending := q.pending.Len()
	inflight := len(q.inflight)
	q.mu.Unlock()

	completed := q.totalCompleted.Load()
	passed := q.totalPassed.Load()
	failed := q.totalFailed.Load()
	timedOut := q.totalTimedOut.Load()
	cancelled := q.totalCancelled.Load()

	var tps float64
	elapsed := time.Since(q.startedAt).Seconds()
	if elapsed > 0 && completed > 0 {
		tps = float64(completed) / elapsed
	}

	var avgDurMS int64
	count := q.durationCount.Load()
	if count > 0 {
		avgDurMS = (q.durationSum.Load() / count) / int64(time.Millisecond)
	}

	q.backendMu.Lock()
	perBackendCopy := make(map[string]int, len(q.perBackend))
	for k, v := range q.perBackend {
		perBackendCopy[k] = v
	}
	q.backendMu.Unlock()

	// ActiveWorkers = workers currently running a test = inflight count,
	// capped at config.Concurrency.
	activeWorkers := inflight
	if activeWorkers > q.config.Concurrency {
		activeWorkers = q.config.Concurrency
	}

	return Stats{
		QueueDepth:     pending + inflight,
		ActiveWorkers:  activeWorkers,
		TotalEnqueued:  q.totalEnqueued.Load(),
		TotalCompleted: completed,
		TotalPassed:    passed,
		TotalFailed:    failed,
		TotalTimedOut:  timedOut,
		TotalCancelled: cancelled,
		TestsPerSec:    tps,
		AvgDurationMS:  avgDurMS,
		PerBackend:     perBackendCopy,
		StartedAt:      q.startedAt,

		AvgLatencyMS:     q.avgLatencyMS(),
		FastestLatencyMS: q.fastestLatency.Load(),
		SlowestLatencyMS: q.slowestLatency.Load(),

		ActiveCores:          int(q.activeCores.Load()),
		CoreProbeConcurrency: q.config.CoreProbeConcurrency,
	}
}

// avgLatencyMS computes the average measured ping under the atomics.
func (q *Queue) avgLatencyMS() int64 {
	count := q.latencyCount.Load()
	if count == 0 {
		return 0
	}

	return q.latencySum.Load() / count
}

// TaskSnapshot is a copy of a Task suitable for returning to the UI
// (no embedded mutex). It is JSON-marshallable for the Wails
// bindings.
type TaskSnapshot struct {
	ID          int64     `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	Protocol    string    `json:"protocol"`
	Backends    []string  `json:"backends,omitempty"`
	Priority    int       `json:"priority"`
	Source      string    `json:"source,omitempty"`
	Attempt     int       `json:"attempt"`
	MaxAttempts int       `json:"max_attempts"`
	CreatedAt   time.Time `json:"created_at"`
	Deadline    time.Time `json:"deadline,omitempty"`
	State       TaskState `json:"state"`
	Result      Result    `json:"result"`
}

// Snapshot returns a slice of the current pending + in-flight tasks
// for the UI. The slice is a snapshot; modifications to it do not
// affect the queue.
func (q *Queue) Snapshot(limit int) []TaskSnapshot {
	q.mu.Lock()
	defer q.mu.Unlock()

	if limit <= 0 {
		limit = 100
	}

	out := make([]TaskSnapshot, 0, min(limit, q.pending.Len()+len(q.inflight)))
	for _, t := range q.pending.heap {
		t.mu.Lock()
		out = append(out, snapshotTask(t))
		t.mu.Unlock()
		if len(out) >= limit {
			break
		}
	}
	if len(out) < limit {
		for _, t := range q.inflight {
			t.mu.Lock()
			out = append(out, snapshotTask(t))
			t.mu.Unlock()
			if len(out) >= limit {
				break
			}
		}
	}

	// Sort by priority descending then CreatedAt ascending.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})

	return out
}

// snapshotTask copies the public fields of t into a TaskSnapshot.
// Caller MUST hold t.mu.
func snapshotTask(t *Task) TaskSnapshot {
	return TaskSnapshot{
		ID:          t.ID,
		Fingerprint: t.Fingerprint,
		Protocol:    t.Protocol,
		Backends:    append([]string(nil), t.Backends...),
		Priority:    t.Priority,
		Source:      t.Source,
		Attempt:     t.Attempt,
		MaxAttempts: t.MaxAttempts,
		CreatedAt:   t.CreatedAt,
		Deadline:    t.Deadline,
		State:       t.State,
		Result:      t.Result,
	}
}

// Result returns the most recent result for a fingerprint, or nil.
func (q *Queue) Result(fingerprint string) *Result {
	q.mu.Lock()
	defer q.mu.Unlock()
	if r, ok := q.results[fingerprint]; ok {
		copy := *r
		return &copy
	}
	return nil
}

// String renders the queue state for diagnostics.
func (q *Queue) String() string {
	s := q.Stats()
	return fmt.Sprintf("testqueue[pending=%d inflight=%d done=%d passed=%d failed=%d tps=%.1f]",
		s.QueueDepth, s.ActiveWorkers, s.TotalCompleted, s.TotalPassed, s.TotalFailed, s.TestsPerSec)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- pendingHeap: a max-heap by (Priority desc, CreatedAt asc) ---
//
// Implemented via container/heap so Push/Pop/Remove/Fix are O(log n).
// Each Task carries its own heapIdx (guarded by q.mu) so Cancel can
// remove a specific task in O(log n) without a linear scan.

type pendingHeap struct {
	heap []*Task
}

func (h pendingHeap) Len() int { return len(h.heap) }
func (h pendingHeap) Less(i, j int) bool {
	if h.heap[i].Priority != h.heap[j].Priority {
		return h.heap[i].Priority > h.heap[j].Priority
	}
	return h.heap[i].CreatedAt.Before(h.heap[j].CreatedAt)
}
func (h pendingHeap) Swap(i, j int) {
	h.heap[i], h.heap[j] = h.heap[j], h.heap[i]
	h.heap[i].heapIdx = i
	h.heap[j].heapIdx = j
}
func (h *pendingHeap) Push(x any) {
	t := x.(*Task)
	t.heapIdx = len(h.heap)
	h.heap = append(h.heap, t)
}
func (h *pendingHeap) Pop() any {
	old := h.heap
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	x.heapIdx = -1
	h.heap = old[:n-1]
	return x
}
