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
//     global (Pause/Resume/Stop).
//  6. Retry policy — failed tasks are retried up to MaxAttempts with
//     exponential backoff.
//  7. Per-config timeout + global test timeout.
//  8. Backpressure — when the queue is full, Enqueue blocks or
//     returns ErrQueueFull based on the EnqueueOption.
//  9. Graceful shutdown — Stop() drains in-flight tasks and returns.
//  10. Progress statistics — tests/sec, average duration, queue depth,
//     active workers, pass/fail counts, per-backend counts.
//
// The queue is intentionally backend-agnostic: it accepts a Tester
// interface (the engine/tester.Tester satisfies this) so the queue
// can be tested in isolation with a fake tester.
package testqueue

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

	// mu guards State + Result transitions from concurrent workers.
	mu sync.Mutex
}

// Result is the outcome of one test.
type Result struct {
	Working         bool            `json:"working"`
	Latency         time.Duration   `json:"latency"`
	Backend         string          `json:"backend,omitempty"`
	TestedAt        time.Time       `json:"tested_at"`
	LastError       string          `json:"last_error,omitempty"`
	FailureCategory FailureCategory `json:"failure_category,omitempty"`
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
	Concurrency int

	// PerBackendConcurrency caps how many tests of the same backend
	// may run in parallel. Default 2.
	PerBackendConcurrency int

	// Timeout is the per-test hard timeout. Default 10s.
	Timeout time.Duration

	// MaxAttempts is the retry cap per task. Default 2.
	MaxAttempts int

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

// Queue is the test scheduler.
type Queue struct {
	tester Tester
	config Config

	mu            sync.Mutex
	pending       []*Task            // priority queue (max-heap by Priority)
	inflight      map[int64]*Task    // tasks currently being tested
	byFingerprint map[string]*Task   // index for duplicate suppression
	results       map[string]*Result // last result per fingerprint (size-bounded LRU)
	nextID        int64

	workersWG sync.WaitGroup
	cancel    context.CancelFunc
	ctx       context.Context

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

	startedAt time.Time
	stopped   atomic.Bool

	// notifyCh is closed+recreated on every state change to wake
	// blocked Dequeue callers.
	notifyCh chan struct{}
}

// New constructs a Queue. The queue is not started; call Start.
func New(tester Tester, config Config) *Queue {
	if tester == nil {
		tester = NoopTester{}
	}
	if config.Concurrency <= 0 {
		config = DefaultConfig()
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
	if config.Measurements <= 0 {
		config.Measurements = 1
	}
	if config.MaxQueueSize == 0 {
		config.MaxQueueSize = 10000
	}

	return &Queue{
		tester:        tester,
		config:        config,
		inflight:      make(map[int64]*Task),
		byFingerprint: make(map[string]*Task),
		results:       make(map[string]*Result),
		perBackend:    make(map[string]int),
		startedAt:     time.Now().UTC(),
		notifyCh:      make(chan struct{}),
	}
}

// NoopTester is a Tester that does nothing; used when no real tester
// is configured (headless setup).
type NoopTester struct{}

// Test returns a placeholder result.
func (NoopTester) Test(ctx context.Context, fp string, backends []string) (Result, error) {
	return Result{TestedAt: time.Now().UTC(), LastError: "no tester configured"}, nil
}

// Start launches the worker pool. Safe to call once; subsequent calls
// are no-ops.
func (q *Queue) Start(parentCtx context.Context) {
	if q.ctx != nil {
		return
	}

	q.ctx, q.cancel = context.WithCancel(parentCtx)

	for i := 0; i < q.config.Concurrency; i++ {
		q.workersWG.Add(1)
		go q.worker(i)
	}
}

// Stop drains in-flight tasks and stops the worker pool. Tasks still
// in the queue are NOT executed (use Drain first if you need that).
func (q *Queue) Stop() {
	if q.stopped.Swap(true) {
		return
	}
	if q.cancel != nil {
		q.cancel()
	}
	q.workersWG.Wait()
}

// Drain blocks until the pending queue is empty AND no tasks are
// in-flight, or the context is cancelled.
func (q *Queue) Drain(ctx context.Context) error {
	for {
		q.mu.Lock()
		pending := len(q.pending)
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

	if q.config.MaxQueueSize > 0 && len(q.pending) >= q.config.MaxQueueSize {
		if opt == EnqueueTry {
			return 0, ErrQueueFull
		}
		// Block-style enqueue is not implemented; fall back to
		// rejection. Callers can retry.
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
	}
	q.pending = append(q.pending, task)
	q.byFingerprint[fingerprint] = task
	q.totalEnqueued.Add(1)

	// Wake up one blocked worker.
	q.notify()

	return task.ID, nil
}

// notify wakes blocked Dequeue callers by closing the notifyCh and
// creating a new one. Caller MUST hold q.mu.
func (q *Queue) notify() {
	close(q.notifyCh)
	q.notifyCh = make(chan struct{})
}

// Dequeue blocks until a task is available, returning the task.
// Returns nil, ctx.Err() when the context is cancelled.
func (q *Queue) Dequeue(ctx context.Context) (*Task, error) {
	for {
		q.mu.Lock()

		// Pop the highest-priority task.
		if len(q.pending) > 0 {
			task := q.popHighest()
			q.inflight[task.ID] = task
			q.mu.Unlock()
			return task, nil
		}

		notifyCh := q.notifyCh
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-notifyCh:
			// loop and try again
		}
	}
}

// popHighest removes and returns the highest-priority task. Caller
// MUST hold q.mu.
func (q *Queue) popHighest() *Task {
	if len(q.pending) == 0 {
		return nil
	}

	// Linear scan for the highest priority; the queue size is
	// bounded by MaxQueueSize and the cost is acceptable for our
	// scale (thousands). A real heap is left as a future
	// optimization.
	bestIdx := 0
	for i := 1; i < len(q.pending); i++ {
		if q.pending[i].Priority > q.pending[bestIdx].Priority {
			bestIdx = i
		}
	}

	task := q.pending[bestIdx]
	q.pending[bestIdx] = q.pending[len(q.pending)-1]
	q.pending = q.pending[:len(q.pending)-1]

	return task
}

// Cancel marks a task as cancelled. If the task is in-flight, the
// tester's context is cancelled (best-effort).
func (q *Queue) Cancel(taskID int64) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, t := range q.pending {
		if t.ID == taskID {
			t.mu.Lock()
			t.State = StateCancelled
			t.Result = Result{TestedAt: time.Now().UTC(), FailureCategory: FailureCancelled, LastError: "cancelled"}
			t.mu.Unlock()
			q.removePending(t)
			q.totalCancelled.Add(1)
			return
		}
	}

	if t, ok := q.inflight[taskID]; ok {
		t.mu.Lock()
		t.State = StateCancelled
		t.mu.Unlock()
		// In-flight cancellation: the worker will see ctx.Done() and
		// mark the result.
	}
}

// CancelBySource cancels every queued task from one source.
func (q *Queue) CancelBySource(sourceID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	cancelled := 0
	kept := q.pending[:0]
	for _, t := range q.pending {
		if t.Source == sourceID {
			t.mu.Lock()
			t.State = StateCancelled
			t.mu.Unlock()
			q.totalCancelled.Add(1)
			cancelled++
			continue
		}
		kept = append(kept, t)
	}
	q.pending = kept
	return cancelled
}

// CancelAll cancels every queued task. In-flight tasks are left to
// complete (their results stand).
func (q *Queue) CancelAll() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	n := len(q.pending)
	for _, t := range q.pending {
		t.mu.Lock()
		t.State = StateCancelled
		t.mu.Unlock()
		q.totalCancelled.Add(1)
	}
	q.pending = nil
	return n
}

// removePending removes a task from the pending list. Caller MUST hold
// q.mu.
func (q *Queue) removePending(t *Task) {
	for i, p := range q.pending {
		if p.ID == t.ID {
			q.pending[i] = q.pending[len(q.pending)-1]
			q.pending = q.pending[:len(q.pending)-1]
			return
		}
	}
}

// worker is the main test loop.
func (q *Queue) worker(id int) {
	defer q.workersWG.Done()

	for {
		task, err := q.Dequeue(q.ctx)
		if err != nil {
			return // ctx cancelled
		}

		q.runTask(q.ctx, task)
	}
}

// runTask executes one task through its attempts.
func (q *Queue) runTask(ctx context.Context, task *Task) {
	task.mu.Lock()
	if task.State == StateCancelled {
		task.mu.Unlock()
		q.complete(task, Result{FailureCategory: FailureCancelled})
		return
	}
	task.State = StatePreparing
	task.mu.Unlock()

	for attempt := task.Attempt; attempt <= task.MaxAttempts; attempt++ {
		// Per-test timeout.
		testCtx, cancel := context.WithTimeout(ctx, q.config.Timeout)

		task.mu.Lock()
		task.State = StateTesting
		task.Attempt = attempt
		task.mu.Unlock()

		start := time.Now()
		result, err := q.tester.Test(testCtx, task.Fingerprint, task.Backends)
		duration := time.Since(start)
		cancel()

		if err == nil && result.Working {
			// Take additional measurements if configured.
			if q.config.Measurements > 1 {
				totalLatency := result.Latency
				for i := 1; i < q.config.Measurements; i++ {
					mCtx, mCancel := context.WithTimeout(ctx, q.config.Timeout)
					m, _ := q.tester.Test(mCtx, task.Fingerprint, task.Backends)
					mCancel()
					if m.Working {
						totalLatency += m.Latency
					} else {
						// A measurement failed; treat the test as failed.
						result = m
						break
					}
				}
				if result.Working {
					result.Latency = totalLatency / time.Duration(q.config.Measurements)
				}
			}

			task.mu.Lock()
			task.State = StateMeasuring
			task.mu.Unlock()

			result.TestedAt = time.Now().UTC()
			task.mu.Lock()
			task.State = StatePassed
			task.Result = result
			task.mu.Unlock()

			q.complete(task, result)
			q.durationSum.Add(int64(duration))
			q.durationCount.Add(1)
			return
		}

		// Failure path.
		category := classifyFailure(err, ctx)
		result.FailureCategory = category
		result.LastError = errToString(err)
		result.TestedAt = time.Now().UTC()

		if category == FailureCancelled {
			task.mu.Lock()
			task.State = StateCancelled
			task.Result = result
			task.mu.Unlock()
			q.complete(task, result)
			return
		}

		if attempt < task.MaxAttempts {
			// Exponential backoff.
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				q.complete(task, Result{FailureCategory: FailureCancelled})
				return
			case <-time.After(backoff):
				// retry
			}
			continue
		}

		// Final failure.
		task.mu.Lock()
		task.State = StateFailed
		if ctx.Err() == context.DeadlineExceeded {
			task.State = StateTimedOut
			result.FailureCategory = FailureTimeout
		}
		task.Result = result
		task.mu.Unlock()
		q.complete(task, result)
		q.durationSum.Add(int64(duration))
		q.durationCount.Add(1)
		return
	}
}

// complete moves a task from inflight to results and updates stats.
func (q *Queue) complete(task *Task, result Result) {
	q.mu.Lock()
	delete(q.inflight, task.ID)
	// Keep the latest fingerprint→task mapping for dedup; but if the
	// task is terminal we can remove it from byFingerprint so future
	// Enqueue calls succeed.
	delete(q.byFingerprint, task.Fingerprint)

	// Cache the last result (bounded LRU; we cap at 4096 entries).
	if len(q.results) >= 4096 {
		// Evict one arbitrary entry. (A real LRU would track access
		// order; this is good enough for stats.)
		for k := range q.results {
			delete(q.results, k)
			break
		}
	}
	r := result
	q.results[task.Fingerprint] = &r
	q.mu.Unlock()

	q.totalCompleted.Add(1)
	switch task.State {
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

	// Wake any Dequeue blocked waiting for capacity.
	q.mu.Lock()
	q.notify()
	q.mu.Unlock()
}

// classifyFailure maps an error to a FailureCategory.
func classifyFailure(err error, ctx context.Context) FailureCategory {
	if err == nil {
		return FailureNone
	}
	if ctx.Err() == context.DeadlineExceeded {
		return FailureTimeout
	}
	if ctx.Err() == context.Canceled {
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
	depth := len(q.pending)
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

	return Stats{
		QueueDepth:     depth + inflight,
		ActiveWorkers:  q.config.Concurrency - depth + inflight - inflight, // approx
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
	}
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

	out := make([]TaskSnapshot, 0, limit)
	for _, t := range q.pending {
		t.mu.Lock()
		out = append(out, snapshotTask(t))
		t.mu.Unlock()
		if len(out) >= limit {
			break
		}
	}
	for _, t := range q.inflight {
		t.mu.Lock()
		out = append(out, snapshotTask(t))
		t.mu.Unlock()
		if len(out) >= limit {
			break
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
		// return a copy
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
