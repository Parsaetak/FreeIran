// backlog.go implements v0.11.0 bounded batch admission: the ONE test
// queue gains a deferred-admission backlog so a huge bulk test (for
// example "test 20,000 untested configurations") never materializes
// 20,000 queued tasks at once.
//
// Root cause this fixes (v0.11.0 runtime evidence): EnqueueByFilter
// admitted up to 10,000 tasks in one burst ("untested enqueued=10000").
// Every admitted task eventually launches a temporary protocol-core
// process, so the burst produced a wall of Xray launches, per-launch
// lifecycle records and memory-pressure oscillation — excessive
// runtime work AND excessive runtime logging with it.
//
// The admission model is:
//
//	small bounded batch → execute → collect outcomes
//	→ admit the next batch only when capacity permits
//
// The backlog lives INSIDE the queue (this package, the ONE
// scheduler): it is not a second queue, it holds no workers, and it
// cannot execute anything. It only defers materialization. Workers,
// the core-probe pool and cancellation remain the queue's own.
//
// Memory-aware backpressure: the memory controller holds admission
// (PauseAdmission) when pressure reaches high/critical and releases it
// on recovery, so the queue drains instead of refilling while the
// heap is under stress. Worker concurrency stays under the adaptive
// booster's control; the core-probe ceiling keeps the number of live
// core processes bounded regardless of backlog size.
package testqueue

import (
	"container/heap"
	"time"
)

// Admission defaults (v0.11.0). A batch is the number of backlog
// candidates materialized per admission step; the floor is the
// pending+inflight bound under which admission continues. Together
// they keep the materialized queue small while still feeding workers
// faster than they drain.
const (
	DefaultAdmissionBatch = 200
	// DefaultAdmissionFloor keeps roughly a few hundred tasks ready
	// for workers — far above what the core-probe pool can run at
	// once, far below the 10k-burst the runtime log showed.
	DefaultAdmissionFloor = 1000
)

// BacklogCandidate is one deferred-admission candidate.
type BacklogCandidate struct {
	Fingerprint string
	Protocol    string
	Backends    []string
	Source      string
}

// backlogItem is the internal queue element of the admission backlog.
type backlogItem struct {
	candidate BacklogCandidate
	batchID   string
	priority  int
}

// batchState tracks ONE open bulk-test batch for aggregation: the
// planned count grows as candidates materialize into real tasks; the
// completed counters grow as those tasks reach terminal states.
type batchState struct {
	id string

	startedAt time.Time

	// planned is the number of tasks MATERIALIZED for this batch so
	// far (not the backlog size — deferred candidates are not tasks).
	planned int64

	passed    int64
	failed    int64
	timedOut  int64
	cancelled int64

	// dropped counts backlog candidates that never materialized
	// (cancellation, stop) — reported honestly in the completion event.
	dropped int64

	// lastProgress is the last time a progress event was emitted for
	// this batch (bounded emission cadence).
	lastProgress time.Time

	// finished guards one-shot completion emission.
	finished bool
}

// ProgressEvent is the batch-aggregation projection the queue emits
// through SetReporter: bounded bulk-test lifecycle evidence
// (bulk_test_start is emitted by the caller that opens the batch; the
// queue owns progress and completion).
type ProgressEvent struct {
	// BatchID identifies the bulk batch (correlation id for logs and
	// the diagnostics surface).
	BatchID string `json:"batch_id"`

	// Phase is "progress" or "complete".
	Phase string `json:"phase"`

	// Planned is the number of tasks materialized for the batch.
	Planned int64 `json:"planned"`

	// Completed aggregates terminal outcomes.
	Completed int64 `json:"completed"`
	Passed    int64 `json:"passed"`
	Failed    int64 `json:"failed"`
	TimedOut  int64 `json:"timed_out"`
	Cancelled int64 `json:"cancelled"`

	// Dropped counts backlog candidates that never materialized.
	Dropped int64 `json:"dropped"`

	// BacklogRemaining is how many candidates still wait to be
	// admitted.
	BacklogRemaining int `json:"backlog_remaining"`

	// ElapsedMS is the batch wall time so far (to completion on the
	// "complete" phase).
	ElapsedMS int64 `json:"elapsed_ms"`
}

// reporterFunc receives batch progress events. It is invoked from the
// queue's completion path; implementations must be cheap and
// non-blocking (log emission is a ring write).
type reporterFunc func(ProgressEvent)

// SetReporter installs the batch progress reporter (the app wires it
// to the runtime logger). Safe to call before Start or at any time;
// the value is guarded by the queue mutex like the rest of the
// aggregation state.
func (q *Queue) SetReporter(fn reporterFunc) {
	q.mu.Lock()
	q.reporter = fn
	q.mu.Unlock()
}

// OpenBatch declares a bulk-test batch. A batch id correlates the
// start record (emitted by the caller), the throttled progress events
// and the completion event. Opening a new batch finalizes any still
// open one (its remaining backlog stays valid but no longer reports).
// Passing an empty batchID disables aggregation for the admission.
func (q *Queue) OpenBatch(batchID string) {
	if batchID == "" {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.batch = &batchState{id: batchID, startedAt: time.Now().UTC()}
}

// EnqueueBacklog admits candidates into the deferred-admission
// backlog under the batch id ("" = no batch aggregation). The FIRST
// admission step runs immediately, so the queue holds only the
// bounded first batch; the rest materialize as the queue drains.
// Duplicates against the live queue AND the backlog are skipped.
//
// It returns the number of candidates accepted (backlog size after
// this call, including the immediately materialized ones) and the
// number materialized right away.
func (q *Queue) EnqueueBacklog(batchID string, candidates []BacklogCandidate, priority int) (accepted, materialized int) {
	if len(candidates) == 0 {
		return 0, 0
	}

	q.mu.Lock()

	if q.stopped.Load() {
		q.mu.Unlock()

		return 0, 0
	}

	for _, c := range candidates {
		if c.Fingerprint == "" {
			continue
		}

		// Dedupe against live tasks and already-deferred candidates:
		// one test per fingerprint, exactly like the live queue.
		if _, live := q.byFingerprint[c.Fingerprint]; live {
			continue
		}

		if q.backlogSeen[c.Fingerprint] {
			continue
		}

		q.backlogSeen[c.Fingerprint] = true
		q.backlog = append(q.backlog, backlogItem{
			candidate: c,
			batchID:   batchID,
			priority:  priority,
		})

		accepted++
	}

	q.mu.Unlock()

	materialized = q.admit()

	// NOTE: admit() owns the batch planned counter (it increments it
	// per materialized task under q.mu) — EnqueueBacklog must NOT add
	// materialized again (a double count once deferred batches drain).

	return accepted, materialized
}

// BacklogRemaining reports how many candidates still wait in the
// admission backlog.
func (q *Queue) BacklogRemaining() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.backlog)
}

// PauseAdmission holds deferred admission: the backlog stops feeding
// the queue while in-flight tests finish. Used by the memory
// controller at high/critical pressure ("stop admitting more work;
// allow the queue to drain and recover"). Pausing admission is NOT
// Pause (task pickup) — workers keep consuming what is already
// materialized.
func (q *Queue) PauseAdmission() {
	q.admissionHeld.Store(true)
}

// ResumeAdmission releases a held admission and immediately runs one
// admission step.
func (q *Queue) ResumeAdmission() {
	q.admissionHeld.Store(false)
	q.admit()
}

// AdmissionHeld reports whether deferred admission is currently held.
func (q *Queue) AdmissionHeld() bool {
	return q.admissionHeld.Load()
}

// dropBacklogLocked clears the backlog (CancelAll / Stop). It counts
// the dropped candidates per open batch for honest completion
// reporting. Caller holds q.mu.
func (q *Queue) dropBacklogLocked() int {
	dropped := len(q.backlog)

	if dropped > 0 && q.batch != nil && !q.batch.finished {
		q.batch.dropped += int64(dropped)
	}

	q.backlog = nil
	q.backlogSeen = make(map[string]bool)

	return dropped
}

// admit runs one admission step: while the queue is below the
// admission floor and admission is not held, materialize up to
// AdmissionBatch backlog candidates as real tasks. Safe to call from
// any goroutine; the heavy work happens under q.mu (cheap pops and
// pushes, no I/O).
func (q *Queue) admit() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.stopped.Load() || q.admissionHeld.Load() {
		return 0
	}

	admitted := 0

	for len(q.backlog) > 0 && admitted < q.config.AdmissionBatch {
		pending := q.pending.Len() + len(q.inflight)
		if q.config.MaxQueueSize > 0 && pending >= q.admissionFloorLocked() {
			break
		}

		if q.paused.Load() {
			// A paused queue must not refill: pickup is suspended,
			// so admission would only accumulate pending work while
			// the user asked the queue to hold (bulk-testing UX).
			break
		}

		item := q.backlog[0]
		q.backlog = q.backlog[1:]

		fp := item.candidate.Fingerprint

		// Re-check duplicates against the live queue (a single test
		// may have raced with the backlog since the candidate was
		// deferred).
		if _, live := q.byFingerprint[fp]; live {
			delete(q.backlogSeen, fp)

			continue
		}

		q.nextID++
		task := &Task{
			ID:          q.nextID,
			Fingerprint: fp,
			Protocol:    item.candidate.Protocol,
			Backends:    item.candidate.Backends,
			Priority:    item.priority,
			Source:      item.candidate.Source,
			Attempt:     1,
			MaxAttempts: q.config.MaxAttempts,
			CreatedAt:   time.Now().UTC(),
			State:       StateQueued,
			heapIdx:     -1,
			batchID:     item.batchID,
		}
		task.Deadline = task.CreatedAt.Add(q.config.Timeout * time.Duration(q.config.MaxAttempts+1))

		heap.Push(&q.pending, task)
		q.byFingerprint[fp] = task
		q.totalEnqueued.Add(1)

		if q.batch != nil && item.batchID == q.batch.id && !q.batch.finished {
			q.batch.planned++
		}

		delete(q.backlogSeen, fp)
		admitted++
	}

	if admitted > 0 {
		q.notifyLocked()
	}

	return admitted
}

// admissionFloorLocked returns the pending+inflight bound under which
// admission continues. Caller holds q.mu.
func (q *Queue) admissionFloorLocked() int {
	floor := q.config.AdmissionFloor
	if floor <= 0 {
		floor = DefaultAdmissionFloor
	}

	if q.config.MaxQueueSize > 0 && floor > q.config.MaxQueueSize {
		floor = q.config.MaxQueueSize
	}

	return floor
}

// batchCompletedLocked emits the completion event exactly once when
// every materialized batch task is terminal and the backlog is
// empty. Caller holds q.mu.
func (q *Queue) batchCompletedLocked() {
	if q.batch == nil || q.batch.finished {
		return
	}

	if len(q.backlog) > 0 {
		return
	}

	b := q.batch
	completed := b.passed + b.failed + b.timedOut + b.cancelled

	if completed < b.planned {
		return
	}

	b.finished = true

	if fn := q.reporter; fn != nil {
		fn(ProgressEvent{
			BatchID:          b.id,
			Phase:            "complete",
			Planned:          b.planned,
			Completed:        completed,
			Passed:           b.passed,
			Failed:           b.failed,
			TimedOut:         b.timedOut,
			Cancelled:        b.cancelled,
			Dropped:          b.dropped,
			BacklogRemaining: 0,
			ElapsedMS:        time.Since(b.startedAt).Milliseconds(),
		})
	}
}

// batchProgressLocked emits a throttled progress event: at most one
// per progress cadence window, plus one on the final completion. This
// is the bounded-emission contract — repetitive bulk activity is
// summarized, never streamed per task. Caller holds q.mu.
func (q *Queue) batchProgressLocked(force bool) {
	if q.batch == nil || q.batch.finished {
		return
	}

	const progressCadence = 10 * time.Second

	now := time.Now().UTC()
	if !force && now.Sub(q.batch.lastProgress) < progressCadence {
		return
	}

	q.batch.lastProgress = now

	b := q.batch
	completed := b.passed + b.failed + b.timedOut + b.cancelled

	if completed == 0 {
		return // nothing to report yet
	}

	if fn := q.reporter; fn != nil {
		fn(ProgressEvent{
			BatchID:          b.id,
			Phase:            "progress",
			Planned:          b.planned,
			Completed:        completed,
			Passed:           b.passed,
			Failed:           b.failed,
			TimedOut:         b.timedOut,
			Cancelled:        b.cancelled,
			Dropped:          b.dropped,
			BacklogRemaining: len(q.backlog),
			ElapsedMS:        time.Since(b.startedAt).Milliseconds(),
		})
	}
}
