package freecore

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Session is one proxied connection inside the engine: from the
// local inbound handshake to the closed pipe. It is the unit of
// bounded lifecycle, cancellation and accounting — there is no
// per-request state outside a session.
type Session struct {
	ID        uint64
	Inbound   string // "socks5" | "http-connect"
	Target    string // destination host:port (credential-free)
	Outbound  string // outbound the router selected
	StartedAt time.Time

	cancel  context.CancelFunc
	done    chan struct{}
	closed  sync.Once
	upBytes atomic.Int64
	dnBytes atomic.Int64
}

// finish marks the session done. Idempotent.
func (s *Session) finish() {
	s.closed.Do(func() {
		s.cancel()

		close(s.done)
	})
}

// Done returns the session termination channel (closed when the
// proxied connection ends for any reason).
func (s *Session) Done() <-chan struct{} { return s.done }

// Cancel terminates the session (idempotent). Cancellation is real:
// it aborts the outbound dial and closes both pipes.
func (s *Session) Cancel() { s.finish() }

// Account adds to the session's byte counters (observable, honest
// accounting — never synthetic metrics).
func (s *Session) Account(up, dn int64) {
	if up > 0 {
		s.upBytes.Add(up)
	}

	if dn > 0 {
		s.dnBytes.Add(dn)
	}
}

// Stats returns the session's byte counters.
func (s *Session) Stats() (up, dn int64) {
	return s.upBytes.Load(), s.dnBytes.Load()
}

// sessionRegistry is the engine's bounded session table. The bound
// is the honest limit on concurrent in-flight proxied connections:
// beyond it the engine REFUSES new sessions (fail-closed) instead of
// growing unbounded goroutine/state per request.
type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[uint64]*Session
	next     uint64
	max      int
	draining bool
}

func newSessionRegistry(max int) *sessionRegistry {
	if max <= 0 {
		max = DefaultMaxSessions
	}

	return &sessionRegistry{
		sessions: make(map[uint64]*Session),
		max:      max,
	}
}

// ErrSessionLimit is returned when the concurrent session cap is
// reached. It is a real limit, surfaced honestly to the caller — the
// client sees a protocol-level refusal, never a silent drop.
var errSessionLimit = &sessionLimitError{}

type sessionLimitError struct{}

func (*sessionLimitError) Error() string {
	return "freecore: concurrent session limit reached"
}

// errDraining is returned when a session begins after shutdown
// started. It is distinct from the concurrency limit: the limit is a
// load bound, draining is a lifecycle refusal.
var errDraining = &drainingError{}

type drainingError struct{}

func (*drainingError) Error() string {
	return "freecore: engine is shutting down"
}

// begin opens a session. The returned end function must be called
// when the session ends (defer) — it removes the session from the
// registry and cancels its context. After beginDrain, every begin
// fails: shutdown owns the future.
func (r *sessionRegistry) begin(parent context.Context) (*Session, context.Context, func(), error) {
	r.mu.Lock()

	if r.draining {
		r.mu.Unlock()

		return nil, nil, nil, errDraining
	}

	if len(r.sessions) >= r.max {
		r.mu.Unlock()

		return nil, nil, nil, errSessionLimit
	}

	r.next++

	s := &Session{
		ID:        r.next,
		StartedAt: time.Now().UTC(),
		done:      make(chan struct{}),
	}

	r.sessions[s.ID] = s

	r.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)

	s.cancel = cancel

	end := func() {
		s.finish()

		r.mu.Lock()
		delete(r.sessions, s.ID)
		r.mu.Unlock()
	}

	return s, ctx, end, nil
}

// beginDrain switches the registry into shutdown mode: every future
// begin is refused. Idempotent.
func (r *sessionRegistry) beginDrain() {
	r.mu.Lock()
	r.draining = true
	r.mu.Unlock()
}

// draining reports whether the registry has begun draining.
func (r *sessionRegistry) isDraining() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.draining
}

// Len returns the number of live sessions.
func (r *sessionRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.sessions)
}

// DefaultMaxSessions bounds concurrent engine sessions (and therefore
// pump goroutines: two per session). 1024 sessions ≈ 2048 goroutines
// — far beyond real System Proxy load, tight enough to be a real
// bound.
const DefaultMaxSessions = 1024
