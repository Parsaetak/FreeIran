package httpx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastPolicy is the test policy: near-zero backoff so retry tests run
// in milliseconds instead of seconds.
func fastPolicy() Policy {
	return Policy{
		RequestTimeout:    5 * time.Second,
		MaxRetries:        3,
		BackoffBase:       5 * time.Millisecond,
		BackoffMax:        20 * time.Millisecond,
		BackoffJitter:     0.1,
		MaxRetryAfterWait: 60 * time.Second,
		MaxBodyBytes:      1 << 20,
	}
}

// TestGetNormal verifies a plain 200 round trip.
func TestGetNormal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	resp, err := c.Get(context.Background(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if string(resp.Body) != `{"ok":true}` {
		t.Fatalf("body = %q", resp.Body)
	}

	if resp.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", resp.Attempts)
	}
}

// TestGetRetries429ThenSucceeds proves the release-resolution failure
// mode is fixed: a 429 is retried (not decoded into an empty release).
func TestGetRetries429ThenSucceeds(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limit"}`))

			return
		}

		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	resp, err := c.Get(context.Background(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if resp.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", resp.Attempts)
	}

	if got := hits.Load(); got != 3 {
		t.Fatalf("server hits = %d, want 3", got)
	}
}

// TestGetRetries503ThenSucceeds mirrors the 429 test for 5xx.
func TestGetRetries503ThenSucceeds(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		_, _ = w.Write([]byte("recovered"))
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	resp, err := c.Get(context.Background(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if resp.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", resp.Attempts)
	}
}

// TestGetHonoursRetryAfter proves the server-declared wait is
// honoured: the gap between attempt 1 and attempt 2 must be at least
// the declared second.
func TestGetHonoursRetryAfter(t *testing.T) {
	var hits atomic.Int32

	var firstAttempt time.Time

	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)

		if n == 1 {
			mu.Lock()
			firstAttempt = time.Now()
			mu.Unlock()

			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		mu.Lock()
		gap := time.Since(firstAttempt)
		mu.Unlock()

		if gap < 900*time.Millisecond {
			t.Errorf("retry honoured too early: gap = %s, want >= ~1s", gap)
		}

		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	started := time.Now()

	resp, err := c.Get(context.Background(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Errorf("total elapsed = %s, Retry-After: 1 was not honoured", elapsed)
	}

	if resp.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", resp.Attempts)
	}
}

// TestGetRetryAfterTooLong proves a huge Retry-After fails loudly with
// ErrRateLimited instead of blocking the caller.
func TestGetRetryAfterTooLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	started := time.Now()

	_, err := c.Get(context.Background(), srv.URL, GetOptions{})
	if err == nil {
		t.Fatal("Get succeeded, want ErrRateLimited")
	}

	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("blocked for %s, want immediate failure", elapsed)
	}
}

// TestGetTransientNetworkErrorRetried proves abrupt connection resets
// are retried.
func TestGetTransientNetworkErrorRetried(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// Reset the connection mid-exchange.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}

			conn, _, _ := hj.Hijack()
			_ = conn.Close()

			return
		}

		_, _ = w.Write([]byte("second attempt ok"))
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	resp, err := c.Get(context.Background(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if string(resp.Body) != "second attempt ok" {
		t.Fatalf("body = %q", resp.Body)
	}
}

// TestGetNonRetryableFailsImmediately proves a 404 is not retried.
func TestGetNonRetryableFailsImmediately(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	_, err := c.Get(context.Background(), srv.URL, GetOptions{})
	if err == nil {
		t.Fatal("Get succeeded, want error")
	}

	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 (no retries for 404)", got)
	}
}

// TestGetBoundedBody proves the response-size cap is enforced.
func TestGetBoundedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		big := make([]byte, 2<<20) // 2 MiB > 1 MiB cap
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	_, err := c.Get(context.Background(), srv.URL, GetOptions{})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
}

// TestGetCancellationDuringBackoff proves cancellation propagates
// immediately instead of finishing the retry ladder.
func TestGetCancellationDuringBackoff(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := fastPolicy()
	p.BackoffBase = 500 * time.Millisecond // long enough to cancel mid-backoff
	p.BackoffMax = 8 * time.Second

	c := NewClient(p)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	started := time.Now()

	_, err := c.Get(ctx, srv.URL, GetOptions{})
	if err == nil {
		t.Fatal("Get succeeded, want cancellation error")
	}

	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Errorf("cancellation took %s, want immediate", elapsed)
	}

	if got := hits.Load(); got > 2 {
		t.Fatalf("server hits = %d, want <= 2", got)
	}
}

// TestGetConditional304 proves If-None-Match is sent and a 304 is
// surfaced to the caller for cached-copy reuse.
func TestGetConditional304(t *testing.T) {
	var receivedIfNoneMatch atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("If-None-Match"); v != "" {
			receivedIfNoneMatch.Store(v)

			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("ETag", `"etag-1"`)
		_, _ = w.Write([]byte("body-v1"))
	}))
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	ctx := context.Background()

	resp, err := c.Get(ctx, srv.URL, GetOptions{})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("cold Get: resp=%v err=%v", resp, err)
	}

	cond, err := c.Get(ctx, srv.URL, GetOptions{IfNoneMatch: `"etag-1"`})
	if err != nil {
		t.Fatalf("conditional Get: %v", err)
	}

	if cond.StatusCode != 304 {
		t.Fatalf("status = %d, want 304", cond.StatusCode)
	}

	if got, _ := receivedIfNoneMatch.Load().(string); got != `"etag-1"` {
		t.Fatalf("server saw If-None-Match = %q, want the ETag", got)
	}
}

// TestGetConnectionReuse proves keep-alive: several sequential Gets
// over one Client share a single TCP connection.
func TestGetConnectionReuse(t *testing.T) {
	var conns atomic.Int32

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	srv.Listener = &countingListener{inner: srv.Listener, conns: &conns}
	srv.Start()
	defer srv.Close()

	c := NewClient(fastPolicy())
	defer c.Close()

	ctx := context.Background()

	for i := 0; i < 4; i++ {
		resp, err := c.Get(ctx, srv.URL, GetOptions{})
		if err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Get #%d: status %d", i, resp.StatusCode)
		}
	}

	if got := conns.Load(); got != 1 {
		t.Errorf("server connections = %d, want 1 (keep-alive reuse)", got)
	}
}

// countingListener counts accepted TCP connections.
type countingListener struct {
	inner net.Listener
	conns *atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.inner.Accept()
	if err == nil {
		l.conns.Add(1)
	}

	return conn, err
}

func (l *countingListener) Close() error   { return l.inner.Close() }
func (l *countingListener) Addr() net.Addr { return l.inner.Addr() }

// TestRetryDelayJitterBounds verifies the backoff curve stays within
// the jitter band.
func TestRetryDelayJitterBounds(t *testing.T) {
	c := NewClient(Policy{
		BackoffBase:   100 * time.Millisecond,
		BackoffMax:    800 * time.Millisecond,
		BackoffJitter: 0.2,
		MaxRetries:    5,
	})
	defer c.Close()

	for attempt := 1; attempt <= 4; attempt++ {
		base := 100 * time.Millisecond

		for i := 1; i < attempt; i++ {
			base *= 2
		}

		for i := 0; i < 200; i++ {
			wait, _, err := c.retryDelay(attempt, nil)
			if err != nil {
				t.Fatalf("retryDelay(%d): %v", attempt, err)
			}

			lo := time.Duration(float64(base) * (1 - 0.2))
			hi := time.Duration(float64(base) * (1 + 0.2))

			// Generous bounds tolerate boundary rounding only.
			if wait < lo-time.Millisecond || wait > hi+time.Millisecond {
				t.Fatalf("retryDelay(%d) = %s, want within [%s, %s]",
					attempt, wait, lo, hi)
			}
		}
	}
}

// TestParseRetryAfter covers both header forms.
func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("3"); got != 3*time.Second {
		t.Errorf("parseRetryAfter(3) = %s", got)
	}

	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("parseRetryAfter(empty) = %s, want 0", got)
	}

	if got := parseRetryAfter("garbage"); got != 0 {
		t.Errorf("parseRetryAfter(garbage) = %s, want 0", got)
	}

	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got < 25*time.Second || got > 35*time.Second {
		t.Errorf("parseRetryAfter(http-date) = %s, want ~30s", got)
	}
}

// TestParseContentRange covers the Content-Range parser used for
// resume validation.
func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in         string
		start, end int64
		total      int64
		ok         bool
	}{
		{"bytes 100-199/1000", 100, 199, 1000, true},
		{"bytes=100-199/1000", 100, 199, 1000, true},
		{"bytes 0-0/42", 0, 0, 42, true},
		{"bytes 100-199/*", 100, 199, -1, true},
		{"bytes 200-199/1000", 0, 0, 0, false},
		{"bytes abc/1000", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"octets 0-9/10", 0, 0, 0, false},
	}

	for _, c := range cases {
		start, end, total, ok := parseContentRange(c.in)
		if ok != c.ok || start != c.start || end != c.end || total != c.total {
			t.Errorf("parseContentRange(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.in, start, end, total, ok, c.start, c.end, c.total, c.ok)
		}
	}
}
