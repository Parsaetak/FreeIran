// Package singleflight provides a tiny, dependency-free in-flight
// deduplication primitive: concurrent callers requesting the same key
// share one execution.
//
// FreeIran v0.9.14 idempotency requirement: identical concurrent work
// (executable version probes, release metadata lookups, provider
// release resolves) must not duplicate the underlying network request
// or process spawn. The standard library has no such helper and
// golang.org/x/sync is deliberately not added as a dependency, so this
// package implements the small slice the codebase needs.
//
// Cancellation semantics (required by the v0.9.14 release-metadata
// dedup contract): the shared execution is detached from the FIRST
// caller's context, so one cancelled caller cannot cancel the shared
// work for everyone. A cancelled waiter simply stops waiting and
// returns its own context error; the shared execution completes for
// the remaining (and future) callers.
package singleflight

import (
	"context"
	"sync"
)

// call is one in-flight (or completed) shared execution.
type call[T any] struct {
	// done is closed exactly once, after val/err are final and the key
	// has been removed from the group's map. Waiters select on it —
	// no per-waiter goroutine is required (the previous WaitGroup +
	// helper-goroutine design burned one blocked goroutine per waiter
	// and every abandoned waiter kept that goroutine alive until the
	// shared execution finished; under the release-metadata 30s flights
	// a burst of expiring callers churned goroutines for nothing).
	//
	// Memory model: the write to val/err happens-before the close of
	// done, and a receive that observes the closed channel therefore
	// observes both final values (Go channel happens-before rule) —
	// the same guarantee sync.WaitGroup provided.
	done chan struct{}

	// val and err are written once, before done is closed, and then
	// only read.
	val T
	err error
}

// Group deduplicates executions by key. The zero value is ready to
// use. It must not be copied after first use.
type Group[T any] struct {
	mu sync.Mutex
	m  map[string]*call[T]
}

// Do executes fn for key, deduplicating concurrent calls with the same
// key: the first caller runs fn, every concurrent caller waits and
// receives the same result.
//
// fn runs detached from the caller's context (the execTimeout callback
// supplies the detached lifetime — its own timeout, cancellation and
// values, never the caller's deadline): one cancelled caller must not
// abort the shared work other callers depend on. Each caller's wait is
// still bounded by its own ctx — a cancelled caller returns ctx.Err()
// immediately while the shared execution continues.
func Do[V any, K ~string](g *Group[V], ctx context.Context, key K, execTimeout func() (context.Context, context.CancelFunc), fn func(ctx context.Context) (V, error)) (V, error) {
	g.mu.Lock()

	if g.m == nil {
		g.m = make(map[string]*call[V])
	}

	if c, ok := g.m[string(key)]; ok {
		g.mu.Unlock()
		return wait(ctx, c)
	}

	c := new(call[V])
	c.done = make(chan struct{})
	g.m[string(key)] = c
	g.mu.Unlock()

	// Run the shared execution detached from this caller's context.
	execCtx, cancel := execTimeout()

	go func() {
		c.val, c.err = fn(execCtx)
		cancel()

		// Retire the key BEFORE releasing the waiters: a caller that
		// arrives after the deletion starts a fresh execution instead
		// of joining a completed one — identical to the WaitGroup
		// design's ordering. Callers that already joined observe the
		// final result through the closed channel.
		g.mu.Lock()
		delete(g.m, string(key))
		g.mu.Unlock()

		close(c.done)
	}()

	return wait(ctx, c)
}

// wait blocks until the shared execution completes or ctx is done.
// It allocates nothing and blocks on a channel select only: abandoned
// (cancelled) waiters leave no goroutine behind.
func wait[V any](ctx context.Context, c *call[V]) (V, error) {
	select {
	case <-c.done:
		return c.val, c.err

	case <-ctx.Done():
		var zero V

		return zero, ctx.Err()
	}
}
