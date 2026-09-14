// Package cleanup implements FreeIran's central cleanup coordinator.
//
// Before v0.9.2 the store checkpointed the WAL and compacted dead
// chunks, logging rotated itself, but nothing reclaimed the rest, and
// every new subsystem would have invented its own deletion policy.
// This coordinator is the ONE place that decides when reclaimable
// data is removed, so the data-lifecycle rules stay uniform:
//
//	Permanent / user-important  → never touched automatically
//	  (live configurations, config metadata, source definitions,
//	   user settings, active core metadata)
//
//	Reconstructable → removed opportunistically
//	  (cache entries, stale derived indexes, temporary parser
//	   buffers, temporary runtime files, obsolete runtime
//	   artifacts, completed transient queue state)
//
//	Replaceable historical → removed by age/size/pressure policy
//	  (old logs, old diagnostic bundles, obsolete WAL segments
//	   already checkpointed, dead/obsolete chunk files, stale
//	   cache files, stale core-install staging, abandoned
//	   failed-download artifacts)
//
// Design: tasks are registered by the composition root; Run executes
// them sequentially under a context (cancellable), each task is
// internally bounded (age + entry caps — never a full-workspace scan
// every few seconds), and the result reports exactly what was
// reclaimed. Run never deletes a store chunk that still holds the
// live authoritative record — that guarantee lives in the store's
// compaction lifecycle, which the coordinator merely triggers.
package cleanup

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// TaskResult reports what one task reclaimed.
type TaskResult struct {
	// Name is the registered task name (stable identifier).
	Name string `json:"name"`

	// Bytes is the disk or memory bytes reclaimed.
	Bytes int64 `json:"bytes"`

	// Items is the number of files/entries/segments reclaimed.
	Items int64 `json:"items"`

	// Skipped is true when the task found nothing to do or was not
	// applicable in this pass.
	Skipped bool `json:"skipped,omitempty"`

	// Error names a task failure; the pass continues with other tasks.
	Error string `json:"error,omitempty"`

	// DurationMS bounds the task's runtime.
	DurationMS int64 `json:"duration_ms"`
}

// Result is the outcome of one cleanup pass.
type Result struct {
	// At is the pass start time (UTC).
	At time.Time `json:"at"`

	// DurationMS is the total pass runtime.
	DurationMS int64 `json:"duration_ms"`

	// Bytes is the sum reclaimed across successful tasks.
	Bytes int64 `json:"bytes"`

	// Tasks holds one entry per executed task, execution order.
	Tasks []TaskResult `json:"tasks"`
}

// Task is one bounded reclamation unit.
type Task struct {
	// Name is a stable identifier (used in logs and the UI).
	Name string

	// Run executes the task. Implementations must be bounded: honor
	// ctx cancellation, cap entries examined and only touch data that
	// is reconstructable or replaceable per the lifecycle rules.
	Run func(ctx context.Context) (bytes int64, items int64, err error)
}

// Options configure the coordinator.
type Options struct {
	// MinInterval suppresses back-to-back passes: Run is a no-op when
	// the previous pass finished less than MinInterval ago (opportunistic
	// runs triggered by pressure spikes cannot stampede). Zero = 30s.
	MinInterval time.Duration

	// MaxDuration bounds every pass; per-task Run also receives a
	// derived deadline. Zero = 45s.
	MaxDuration time.Duration

	// MaxTasks caps the number of tasks executed per pass (bounded work).
	// Zero = unlimited.
	MaxTasks int
}

// Manager is the central cleanup coordinator.
type Manager struct {
	opts Options

	mu      sync.Mutex
	tasks   []Task
	lastRun time.Time

	running      atomic.Bool
	lastResult   atomic.Value // Result
	passesDone   atomic.Int64
	bytesReclaim atomic.Int64
}

// New creates a coordinator with the given options.
func New(opts Options) *Manager {
	if opts.MinInterval <= 0 {
		opts.MinInterval = 30 * time.Second
	}

	if opts.MaxDuration <= 0 {
		opts.MaxDuration = 45 * time.Second
	}

	return &Manager{opts: opts}
}

// Register adds a task. Registration order is execution order.
func (m *Manager) Register(t Task) {
	if t.Name == "" || t.Run == nil {
		return
	}

	m.mu.Lock()
	m.tasks = append(m.tasks, t)
	m.mu.Unlock()
}

// Run executes a cleanup pass. It is cancellable (ctx), serialized
// (a pass already running wins; concurrent callers get nil, false),
// rate-limited by MinInterval, and bounded by MaxDuration plus each
// task's own caps. The returned bool reports whether a pass ran.
func (m *Manager) Run(ctx context.Context) (*Result, bool) {
	if m == nil {
		return nil, false
	}

	if !m.running.CompareAndSwap(false, true) {
		return nil, false // pass already in flight
	}

	defer m.running.Store(false)

	m.mu.Lock()
	last := m.lastRun
	tasks := make([]Task, len(m.tasks))
	copy(tasks, m.tasks)
	maxTasks := m.opts.MaxTasks
	m.mu.Unlock()

	if time.Since(last) < m.opts.MinInterval && !last.IsZero() {
		return nil, false
	}

	if len(tasks) == 0 {
		return nil, false
	}

	if maxTasks > 0 && len(tasks) > maxTasks {
		tasks = tasks[:maxTasks]
	}

	started := time.Now()

	passCtx, cancel := context.WithTimeout(ctx, m.opts.MaxDuration)
	defer cancel()

	result := Result{At: started.UTC()}

	for _, task := range tasks {
		if passCtx.Err() != nil {
			break
		}

		taskResult := TaskResult{Name: task.Name}

		taskStart := time.Now()

		taskCtx, taskCancel := context.WithTimeout(passCtx, m.opts.MaxDuration)
		bytes, items, err := task.Run(taskCtx)
		taskCancel()

		taskResult.DurationMS = time.Since(taskStart).Milliseconds()
		taskResult.Bytes = bytes
		taskResult.Items = items

		if err != nil {
			taskResult.Error = err.Error()

			if passCtx.Err() != nil {
				taskResult.Skipped = true
			}
		} else if bytes == 0 && items == 0 {
			taskResult.Skipped = true
		}

		if err == nil {
			result.Bytes += bytes
		}

		result.Tasks = append(result.Tasks, taskResult)
	}

	result.DurationMS = time.Since(started).Milliseconds()

	m.mu.Lock()
	m.lastRun = time.Now()
	m.mu.Unlock()

	m.lastResult.Store(result)
	m.passesDone.Add(1)
	m.bytesReclaim.Add(result.Bytes)

	return &result, true
}

// LastResult returns the most recent completed pass, if any.
func (m *Manager) LastResult() (Result, bool) {
	if m == nil {
		return Result{}, false
	}

	if result, ok := m.lastResult.Load().(Result); ok {
		return result, true
	}

	return Result{}, false
}

// Passes reports the number of completed passes (diagnostics).
func (m *Manager) Passes() int64 {
	if m == nil {
		return 0
	}

	return m.passesDone.Load()
}

// TotalReclaimed reports the cumulative bytes reclaimed (diagnostics).
func (m *Manager) TotalReclaimed() int64 {
	if m == nil {
		return 0
	}

	return m.bytesReclaim.Load()
}
