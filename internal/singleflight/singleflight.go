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
	wg sync.WaitGroup

	// val and err are written once, before the waiters are released,
	// and then only read.
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
	c.wg.Add(1)
	g.m[string(key)] = c
	g.mu.Unlock()

	// Run the shared execution detached from this caller's context.
	execCtx, cancel := execTimeout()

	go func() {
		defer c.wg.Done()

		c.val, c.err = fn(execCtx)
		cancel()

		g.mu.Lock()
		delete(g.m, string(key))
		g.mu.Unlock()
	}()

	return wait(ctx, c)
}

// wait blocks until the shared execution completes or ctx is done.
func wait[V any](ctx context.Context, c *call[V]) (V, error) {
	done := make(chan struct{})

	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return c.val, c.err
	case <-ctx.Done():
		var zero V

		return zero, ctx.Err()
	}
}
