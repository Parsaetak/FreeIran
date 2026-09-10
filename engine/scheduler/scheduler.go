// Package scheduler drives FreeIran's background maintenance cycles:
// periodic source refresh, cache warming and deferred testing.
//
// The scheduler guarantees:
//
//   - cycles never overlap (skip-if-busy),
//   - cancellation propagates immediately,
//   - graceful shutdown drains the running cycle,
//   - jittered intervals so mass-deployed instances do not synchronize.
package scheduler

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// Subsystem identifies the scheduler in structured errors.
const Subsystem = "scheduler"

// Cycle is one unit of scheduled work. Implementations must be safe
// to call sequentially; the scheduler never overlaps cycles.
type Cycle func(ctx context.Context) error

// Options configures the scheduler.
type Options struct {
	// Interval is the base delay between cycles.
	Interval time.Duration

	// Jitter randomizes each interval by ±Jitter/2 (0 disables).
	Jitter time.Duration

	// RunOnStart executes one cycle immediately when started.
	RunOnStart bool
}

// Scheduler runs a cycle on an interval.
type Scheduler struct {
	opts  Options
	cycle Cycle

	mu       sync.Mutex
	running  bool
	busy     bool
	lastRun  time.Time
	lastErr  error
	runCount uint64

	cancel context.CancelFunc
	done   chan struct{}
	wakeCh chan struct{}
}

// New creates a scheduler for the given cycle.
func New(opts Options, cycle Cycle) *Scheduler {
	if opts.Interval <= 0 {
		opts.Interval = time.Hour
	}

	if opts.Jitter < 0 {
		opts.Jitter = 0
	}

	return &Scheduler{
		opts:   opts,
		cycle:  cycle,
		wakeCh: make(chan struct{}, 1),
	}
}

// Start begins the scheduling loop.
func (s *Scheduler) Start(parent context.Context) {
	s.mu.Lock()

	if s.running {
		s.mu.Unlock()

		return
	}

	s.running = true

	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})

	s.mu.Unlock()

	go s.loop(ctx)
}

// Stop cancels the loop and waits for the running cycle to finish.
func (s *Scheduler) Stop() {
	s.mu.Lock()

	if !s.running {
		s.mu.Unlock()

		return
	}

	cancel := s.cancel
	done := s.done

	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	if done != nil {
		<-done
	}

	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

// Wake triggers an immediate cycle (also used for manual refresh).
func (s *Scheduler) Wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
		// A wake is already pending.
	}
}

// Status reports scheduler state for the UI.
type Status struct {
	Running  bool      `json:"running"`
	Busy     bool      `json:"busy"`
	Interval float64   `json:"interval_seconds"`
	LastRun  time.Time `json:"last_run"`
	LastErr  string    `json:"last_error,omitempty"`
	RunCount uint64    `json:"run_count"`
}

// Status returns the current scheduler status.
func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := Status{
		Running:  s.running,
		Busy:     s.busy,
		Interval: s.opts.Interval.Seconds(),
		LastRun:  s.lastRun,
		LastErr:  "",
		RunCount: s.runCount,
	}

	if s.lastErr != nil {
		st.LastErr = s.lastErr.Error()
	}

	return st
}

func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.done)

	if s.opts.RunOnStart {
		s.runCycle(ctx)
	}

	for {
		delay := s.jittered()

		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return

		case <-s.wakeCh:
			timer.Stop()

		case <-timer.C:
		}

		if err := ctx.Err(); err != nil {
			return
		}

		s.runCycle(ctx)
	}
}

func (s *Scheduler) jittered() time.Duration {
	if s.opts.Jitter == 0 {
		return s.opts.Interval
	}

	half := s.opts.Jitter / 2

	offset := time.Duration(rand.Int63n(int64(s.opts.Jitter)+1)) - half

	adjusted := s.opts.Interval + offset
	if adjusted < time.Second {
		adjusted = time.Second
	}

	return adjusted
}

func (s *Scheduler) runCycle(ctx context.Context) {
	s.mu.Lock()

	if s.busy {
		s.mu.Unlock()

		return // skip-if-busy
	}

	s.busy = true

	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.busy = false
		s.lastRun = time.Now().UTC()
		s.runCount++
		s.mu.Unlock()
	}()

	if s.cycle != nil {
		s.lastErr = s.cycle(ctx)
	}
}
