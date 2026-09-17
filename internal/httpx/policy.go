// Package httpx is FreeIran's single production HTTP policy engine.
//
// Every artifact transfer in the application — GitHub release API
// queries (core manager, application updater) and large core-archive
// downloads, plus the configuration-source fetcher — routes through
// one Client with one policy: bounded timeouts, retries for
// transient failures with exponential backoff + jitter, Retry-After
// honouring, connection reuse and bounded response bodies.
//
// Two planes, one policy object:
//
//   - Control plane (Get): small JSON/metadata requests. Each attempt
//     carries a bounded total deadline; 429/502/503/504 and transient
//     network errors are retried with backoff; bodies are capped.
//   - Data plane (Download): large files. NEVER a short total timeout —
//     the stream is watched by a no-progress stall watchdog instead,
//     written directly to a .part file, resumable via HTTP Range with
//     Content-Range validation, and never buffered in RAM.
//
// The package is deliberately dependency-light: only the standard
// library plus internal/version (User-Agent). Probe-style clients
// (engine/netcheck, engine/tester) intentionally do NOT use this
// package: they measure per-attempt latency through disposable
// transports and keep-alive would corrupt the measurement.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/internal/version"
)

// Subsystem identifies this package in structured errors.
const Subsystem = "httpx"

// Policy tunes the shared client. Zero values select the defaults in
// normalize(); the same Policy value drives both planes.
type Policy struct {
	// RequestTimeout bounds ONE control-plane attempt (default 20s).
	// It is deliberately NOT applied to data-plane downloads.
	RequestTimeout time.Duration

	// DialTimeout, TLSHandshakeTimeout and ResponseHeaderTimeout bound
	// the connection phases for BOTH planes (defaults 10s/10s/20s).
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration

	// MaxIdleConnsPerHost enables connection reuse (default 4).
	MaxIdleConnsPerHost int
	// IdleConnTimeout closes idle keep-alive connections (default 60s).
	IdleConnTimeout time.Duration

	// MaxRetries is the number of RETRIES after the first attempt
	// (default 3 → up to 4 attempts).
	MaxRetries int

	// BackoffBase / BackoffMax bound the exponential backoff between
	// retries (defaults 500ms / 8s). Jitter of ±BackoffJitter
	// (fraction, default 0.2) is applied to every sleep.
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	BackoffJitter float64

	// MaxRetryAfterWait caps how long a server-provided Retry-After is
	// honoured before the request fails with an explicit rate-limit
	// error instead of sleeping (default 60s).
	MaxRetryAfterWait time.Duration

	// MaxBodyBytes caps control-plane response bodies (default 8 MiB).
	MaxBodyBytes int64
}

// normalize fills zero fields with the defaults described on Policy.
func (p Policy) normalize() Policy {
	if p.RequestTimeout <= 0 {
		p.RequestTimeout = 20 * time.Second
	}
	if p.DialTimeout <= 0 {
		p.DialTimeout = 10 * time.Second
	}
	if p.TLSHandshakeTimeout <= 0 {
		p.TLSHandshakeTimeout = 10 * time.Second
	}
	if p.ResponseHeaderTimeout <= 0 {
		p.ResponseHeaderTimeout = 20 * time.Second
	}
	if p.MaxIdleConnsPerHost <= 0 {
		p.MaxIdleConnsPerHost = 4
	}
	if p.IdleConnTimeout <= 0 {
		p.IdleConnTimeout = 60 * time.Second
	}
	if p.MaxRetries == 0 {
		p.MaxRetries = 3
	}
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	}
	if p.BackoffBase <= 0 {
		p.BackoffBase = 500 * time.Millisecond
	}
	if p.BackoffMax <= 0 {
		p.BackoffMax = 8 * time.Second
	}
	if p.BackoffJitter <= 0 {
		p.BackoffJitter = 0.2
	}
	if p.MaxRetryAfterWait <= 0 {
		p.MaxRetryAfterWait = 60 * time.Second
	}
	if p.MaxBodyBytes <= 0 {
		p.MaxBodyBytes = 8 << 20
	}
	return p
}

// Client is the production HTTP client. It is safe for concurrent
// use and shares one transport (connection pool) across requests.
type Client struct {
	policy Policy
	http   *http.Client // no total Timeout: per-request contexts bound attempts
	tr     *http.Transport

	// preGet, when non-nil, validates one request before any network
	// activity (SSRF guard, v0.9.7). A returned error fails the Get
	// immediately — no attempt, no retry.
	preGet func(ctx context.Context, url string, o GetOptions) error

	// userAgent overrides the default request identity when non-empty
	// (discovery fetchers declare themselves honestly).
	userAgent string

	rng   *rand.Rand
	rngMu sync.Mutex
}

// Interface is the minimal surface consumers (coremgr, appupdate,
// source fetcher) depend on. Tests substitute their own fake.
type Interface interface {
	Getter
	Downloader
}

// Getter is the control-plane operation.
type Getter interface {
	Get(ctx context.Context, url string, o GetOptions) (*Response, error)
}

// Downloader is the data-plane operation.
type Downloader interface {
	Download(ctx context.Context, url string, o DownloadOptions) (*DownloadResult, error)
}

// GetOptions tune one control-plane request.
type GetOptions struct {
	// Header entries are added to the request (e.g. Accept).
	Header map[string]string

	// IfNoneMatch issues a conditional request; a 304 response is
	// returned to the caller as-is (Status 304, empty Body) so the
	// caller can serve its cached copy.
	IfNoneMatch string

	// MaxBodyBytes caps THIS response body (overrides the policy's
	// MaxBodyBytes; 0 = policy default).
	MaxBodyBytes int64
}

// Response is one control-plane result. Body is already fully read
// and bounded by Policy.MaxBodyBytes.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	// Attempts is the number of HTTP attempts made (1 = no retry).
	Attempts int
	// RetryAfter, when non-zero, is the server-declared wait that was
	// honoured before the final successful attempt.
	RetryAfter time.Duration
}

// ErrRateLimited reports a Retry-After longer than the policy is
// willing to wait. Callers should surface "retry later" messaging.
var ErrRateLimited = errors.New("httpx: rate limited (Retry-After exceeds the configured wait budget)")

// ErrBodyTooLarge reports a control-plane body exceeding MaxBodyBytes.
var ErrBodyTooLarge = errors.New("httpx: response body exceeds the size limit")

// NewClient builds a Client with the given policy (zero value = all
// defaults). The returned client must not be copied.
func NewClient(p Policy) *Client {
	p = p.normalize()

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: p.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   p.MaxIdleConnsPerHost,
		IdleConnTimeout:       p.IdleConnTimeout,
		TLSHandshakeTimeout:   p.TLSHandshakeTimeout,
		ResponseHeaderTimeout: p.ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Client{
		policy: p,
		http:   &http.Client{Transport: tr},
		tr:     tr,
		rng:    rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
}

// Close releases idle connections. The client is unusable afterwards.
func (c *Client) Close() {
	c.tr.CloseIdleConnections()
}

// defaultClient is the process-wide shared production client: ONE
// policy, ONE connection pool, used by the core manager, the
// application updater and the configuration-source fetcher. (Probe
// clients in engine/netcheck and engine/tester deliberately build
// their own disposable transports — see the package documentation.)
var defaultClient = NewClient(Policy{
	RequestTimeout: 20 * time.Second,
	MaxRetries:     3,
	MaxBodyBytes:   8 << 20,
})

// Default returns the shared production client.
func Default() *Client { return defaultClient }

// Policy returns the normalized policy in effect.
func (c *Client) Policy() Policy { return c.policy }

// Get performs a bounded, retrying control-plane GET.
//
// Retries: transient network errors and HTTP 429/502/503/504.
// Backoff: exponential from Policy.BackoffBase doubling up to
// Policy.BackoffMax with ±jitter; a server-provided Retry-After
// (seconds or HTTP-date) overrides the computed delay and is capped
// by Policy.MaxRetryAfterWait (longer waits return ErrRateLimited).
// Every attempt re-checks ctx, so cancellation is immediate.
func (c *Client) Get(ctx context.Context, rawURL string, o GetOptions) (*Response, error) {
	p := c.policy

	// SSRF pre-flight (v0.9.7): reject before any network activity.
	if c.preGet != nil {
		if err := c.preGet(ctx, rawURL, o); err != nil {
			return nil, err
		}
	}

	var lastErr error

	for attempt := 0; attempt <= p.MaxRetries; attempt++ {
		if attempt > 0 {
			wait, _, err := c.retryDelay(attempt, lastErr)
			if err != nil {
				return nil, err
			}

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("httpx: GET %s cancelled during backoff: %w", redactURL(rawURL), ctx.Err())
			case <-time.After(wait):
			}
		}

		resp, err := c.getOnce(ctx, rawURL, o)
		if err == nil {
			resp.Attempts = attempt + 1
			return resp, nil
		}

		lastErr = err

		if !retryableErr(err) {
			return nil, err
		}
	}

	return nil, fmt.Errorf("httpx: GET %s failed after %d attempts: %w",
		redactURL(rawURL), p.MaxRetries+1, lastErr)
}

// getOnce performs a single attempt.
func (c *Client) getOnce(ctx context.Context, rawURL string, o GetOptions) (*Response, error) {
	p := c.policy

	attemptCtx, cancel := context.WithTimeout(ctx, p.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("httpx: build request: %w", err)
	}

	userAgent := c.userAgent
	if userAgent == "" {
		userAgent = version.UserAgent()
	}

	req.Header.Set("User-Agent", userAgent)

	for k, v := range o.Header {
		req.Header.Set(k, v)
	}

	if o.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", o.IfNoneMatch)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpx: request: %w", err)
	}

	defer resp.Body.Close()

	// Retry-After is read before the body so retry decisions and the
	// returned telemetry survive bodies we choose not to read.
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

	if resp.StatusCode == http.StatusNotModified {
		return &Response{StatusCode: resp.StatusCode, Header: resp.Header}, nil
	}

	if retryableStatus(resp.StatusCode) {
		// Drain a bounded body so the error carries server context.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))

		return nil, &retryableStatusError{
			status:     resp.StatusCode,
			retryAfter: retryAfter,
			snippet:    string(snippet),
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))

		return nil, &statusError{status: resp.StatusCode, snippet: string(snippet)}
	}

	maxBody := p.MaxBodyBytes
	if o.MaxBodyBytes > 0 {
		maxBody = o.MaxBodyBytes
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("httpx: read body: %w", err)
	}

	if int64(len(body)) > maxBody {
		return nil, fmt.Errorf("%w (%d > %d bytes)", ErrBodyTooLarge, len(body), maxBody)
	}

	out := &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
	if retryAfter > 0 {
		out.RetryAfter = retryAfter
	}

	return out, nil
}

// retryDelay computes the wait before attempt N (1-based) given the
// previous error. A Retry-After from a retryable status wins over the
// exponential curve; values above MaxRetryAfterWait fail loudly with
// ErrRateLimited instead of blocking the caller for minutes.
func (c *Client) retryDelay(attempt int, prevErr error) (time.Duration, time.Duration, error) {
	p := c.policy

	var ra time.Duration

	var rse *retryableStatusError

	if errors.As(prevErr, &rse) && rse.retryAfter > 0 {
		ra = rse.retryAfter
	}

	if ra > 0 {
		if ra > p.MaxRetryAfterWait {
			return 0, ra, fmt.Errorf("%w (server asked for %s)", ErrRateLimited, ra)
		}

		return ra, ra, nil
	}

	// Exponential backoff with jitter: base * 2^(attempt-1), capped.
	backoff := p.BackoffBase

	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff >= p.BackoffMax {
			backoff = p.BackoffMax
			break
		}
	}

	if backoff > p.BackoffMax {
		backoff = p.BackoffMax
	}

	c.rngMu.Lock()
	jitter := c.rng.Float64()
	c.rngMu.Unlock()

	spread := float64(backoff) * p.BackoffJitter
	wait := time.Duration(float64(backoff) - spread + 2*spread*jitter)

	if wait < 0 {
		wait = 0
	}

	return wait, 0, nil
}

// retryableStatus reports whether the status merits a retry.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	default:
		return false
	}
}

// retryableErr classifies an error from getOnce for retry decisions.
func retryableErr(err error) bool {
	if err == nil {
		return false
	}

	var rse *retryableStatusError
	if errors.As(err, &rse) {
		return true
	}

	// Caller cancellation is never retryable. (An attempt-level
	// deadline surfaces as a net.Error / timeout below and IS
	// retryable, because the next attempt gets a fresh budget.)
	if errors.Is(err, context.Canceled) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"eof",
		"server closed",
		"tls handshake",
		"no route to host",
		"i/o timeout",
		"context deadline exceeded",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}

	return false
}

// retryableStatusError marks an HTTP status that merits retrying and
// carries the server's Retry-After.
type retryableStatusError struct {
	status     int
	retryAfter time.Duration
	snippet    string
}

func (e *retryableStatusError) Error() string {
	if e.snippet != "" {
		return fmt.Sprintf("httpx: HTTP %d (Retry-After %s): %.200s", e.status, e.retryAfter, e.snippet)
	}

	return fmt.Sprintf("httpx: HTTP %d (Retry-After %s)", e.status, e.retryAfter)
}

// statusError is a non-retryable HTTP failure.
type statusError struct {
	status  int
	snippet string
}

func (e *statusError) Error() string {
	if e.snippet != "" {
		return fmt.Sprintf("httpx: HTTP %d: %.200s", e.status, e.snippet)
	}

	return fmt.Sprintf("httpx: HTTP %d", e.status)
}

// StatusCodeOf extracts the HTTP status carried by a request failure
// (-1 for transport errors, the status code for HTTP failures).
func StatusCodeOf(err error) int {
	var se *statusError
	if errors.As(err, &se) {
		return se.status
	}

	return -1
}

// parseRetryAfter parses the Retry-After header in either its
// delay-seconds or HTTP-date form. Zero when absent/invalid.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}

	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}

		return time.Duration(secs) * time.Second
	}

	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}

	return 0
}

// redactURL strips the query string (which may carry tokens) from a
// URL used in error messages.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	u.RawQuery = ""
	u.Fragment = ""

	return u.String()
}
