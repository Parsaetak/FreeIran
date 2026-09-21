package connection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// verify.go implements the VERIFY stage of the connection engine
// (v0.9.6 §12): a tunnel whose core process is "connected" (listener
// ready) is NOT yet proven usable. FreeIran verifies by issuing a
// real HTTP request through the tunnel — the same definition of
// usable the URL test mode uses — and classifies the failure when
// verification fails, so the recovery path can act on evidence
// instead of guessing.
//
// v0.9.8.5 upgrades the single-target probe into an ADAPTIVE
// MULTI-TARGET verification with a documented quorum rule:
//
//      probe independent targets (bounded set, bounded concurrency)
//      → retry genuinely transient failures once, bounded backoff + jitter
//      → require quorum (majority of the target set, min 1)
//      → connected_verified only on quorum success
//
// One arbitrary endpoint answering no longer declares a session
// verified, and one unrelated third-party outage no longer kills an
// otherwise healthy route. Deterministic failures (invalid target,
// refused/credential-style rejection, TLS interception, 4xx) are
// never retried — retrying them only wastes the connection budget.
//
// A successful core startup followed by a failed verification means:
// the local process is healthy but the remote path is not (blocked,
// credential-rejected, protocol-broken). Those are exactly the cases
// where switching candidates is the correct response.

// DefaultVerifyTarget is the connectivity-verification endpoint
// (legacy single-target surface; kept for callers that pin exactly
// one target).
const DefaultVerifyTarget = "https://www.gstatic.com/generate_204"

// DefaultVerifyTimeout bounds one verification round (overall).
const DefaultVerifyTimeout = 12 * time.Second

// DefaultVerifyTargets is the bounded set of independent verification
// endpoints (v0.9.8.5). Three operators, same trust anchors the
// netcheck defaults already use: Google (gstatic), Cloudflare,
// Apple. One unrelated outage cannot fail a quorum of two.
var DefaultVerifyTargets = []string{
	"https://www.gstatic.com/generate_204",
	"https://cp.cloudflare.com/generate_204",
	"https://www.apple.com/library/test/success.html",
}

// MaxVerifyTargets bounds the target set (defensive: callers may
// inject configuration-derived target lists; the verification budget
// never scales with them).
const MaxVerifyTargets = 4

// VerifyOptions tune one tunnel verification.
type VerifyOptions struct {
	// URL is a single verification target. When set it takes
	// precedence over Targets (legacy single-target contract —
	// the quorum of a one-target set is one).
	URL string

	// Targets is the bounded multi-target set. Empty entries are
	// dropped; more than MaxVerifyTargets are truncated. Empty URL
	// and empty Targets select DefaultVerifyTargets.
	Targets []string

	// Timeout bounds the whole verification (default 12s) — the
	// overall deadline across all probes and the bounded retry.
	Timeout time.Duration

	// PerTarget bounds ONE target attempt (0 = Timeout/2 clamped
	// to [1s, 8s]). The SOCKS CONNECT round trip and the HTTP
	// request share this budget.
	PerTarget time.Duration

	// Retries is the maximum number of EXTRA attempts per target
	// for genuinely transient failures (default 1; hard cap 2).
	Retries int

	// Backoff bounds the delay before a transient retry
	// (0 = 400ms; a jitter of up to +50% avoids synchronized
	// retry storms). Tests may shrink it.
	Backoff time.Duration

	// Quorum is the minimum number of successful targets
	// (0 = majority of the effective target set, minimum 1).
	Quorum int
}

// normalize applies the documented defaults and bounds.
func (o VerifyOptions) normalize() VerifyOptions {
	if o.Timeout <= 0 {
		o.Timeout = DefaultVerifyTimeout
	}

	if o.PerTarget <= 0 {
		o.PerTarget = o.Timeout / 2
	}

	if o.PerTarget < time.Second {
		o.PerTarget = time.Second
	}

	if o.PerTarget > 8*time.Second {
		o.PerTarget = 8 * time.Second
	}

	if o.Retries < 0 {
		o.Retries = 0
	}

	if o.Retries > 2 {
		o.Retries = 2
	}

	if o.Retries == 0 {
		o.Retries = 1
	}

	if o.Backoff <= 0 {
		o.Backoff = 400 * time.Millisecond
	}

	return o
}

// effectiveTargets resolves the bounded target set (URL wins).
func (o VerifyOptions) effectiveTargets() []string {
	if o.URL != "" {
		return []string{o.URL}
	}

	targets := make([]string, 0, len(o.Targets))

	for _, t := range o.Targets {
		if t = strings.TrimSpace(t); t != "" {
			targets = append(targets, t)
		}
	}

	if len(targets) == 0 {
		targets = append(targets, DefaultVerifyTargets...)
	}

	if len(targets) > MaxVerifyTargets {
		targets = targets[:MaxVerifyTargets]
	}

	return targets
}

// quorumFor derives the documented quorum rule: a strict majority of
// the effective target set, minimum one (a pinned single target —
// legacy URL callers and tests — keeps the historical contract).
func quorumFor(targets []string, requested int) int {
	if requested > 0 {
		if requested > len(targets) {
			return len(targets)
		}

		return requested
	}

	q := (len(targets) + 1) / 2

	if q < 1 {
		q = 1
	}

	return q
}

// TargetResult is the evidence of ONE verification target.
type TargetResult struct {
	// URL is the probed target.
	URL string `json:"url"`

	// OK reports a successful HTTPS round trip through the tunnel.
	OK bool `json:"ok"`

	// Status is the HTTP status of the (last) attempt.
	Status int `json:"status,omitempty"`

	// LatencyMS is the measured full-request latency of the
	// successful attempt (or the last failed attempt).
	LatencyMS int64 `json:"latency_ms,omitempty"`

	// Measured marks a real measurement (v0.9.8.1 semantics).
	Measured bool `json:"measured,omitempty"`

	// TunnelProbeMS is the SOCKS CONNECT round-trip latency.
	TunnelProbeMS int64 `json:"tunnel_probe_ms,omitempty"`

	// Attempts counts the probes this target consumed.
	Attempts int `json:"attempts,omitempty"`

	// Retried marks that a transient retry was issued.
	Retried bool `json:"retried,omitempty"`

	// Error is the credential-free failure text ("" on success).
	Error string `json:"error,omitempty"`

	// FailureClass classifies the failure ("" on success).
	FailureClass FailureClass `json:"failure_class,omitempty"`

	// Timeout marks the failure as a timeout family error.
	Timeout bool `json:"timeout,omitempty"`
}

// isTransient reports whether the failure class justifies ONE bounded
// retry: timeouts, temporary resets and transient proxy failures
// (v0.9.8.5 §2.2). Deterministic failures — refused (the endpoint
// rejected the route), TLS interception, 4xx objections and invalid
// targets — never retry.
func (t TargetResult) isTransient() bool {
	switch t.FailureClass {
	case FailureTimeout, FailureReset, FailureProxyHand:
		return true
	case FailureHTTP:
		return t.Status >= 500 // server-side hiccup; 4xx is a deterministic objection
	default:
		return false
	}
}

// VerifyResult is the outcome of one tunnel verification.
type VerifyResult struct {
	// OK reports verified usable connectivity (quorum reached).
	OK bool

	// Metrics is the winning URL test (phases, status, timing) —
	// the best successful probe, so latency reporting keeps the
	// "measured, not fabricated" contract.
	Metrics config.URLTestMetrics

	// TunnelProbeMS is the SOCKS CONNECT round-trip through the
	// tunnel (the end-to-end ping implied by a working tunnel).
	TunnelProbeMS int64

	// FailureClass classifies the failure ("" on success). With
	// multiple failing targets the most severe common class wins
	// (core > deterministic > transient).
	FailureClass FailureClass

	// Targets carries the per-target evidence (bounded set).
	Targets []TargetResult `json:"targets,omitempty"`

	// Succeeded is the number of targets that answered OK.
	Succeeded int `json:"succeeded,omitempty"`

	// Required is the quorum that was demanded.
	Required int `json:"required,omitempty"`

	// Probes is the total number of HTTP probes issued (including
	// retries) — bounded by design.
	Probes int `json:"probes,omitempty"`

	// Retries is the number of transient retries issued.
	Retries int `json:"retries,omitempty"`
}

// FailureClass is the evidence-based classification of a verification
// failure. Classes drive recovery decisions:
//
//   - timeout/handshake: path problems → try another candidate;
//   - refused/reset: endpoint rejected us → another candidate;
//   - tls: interception/mismatch → another candidate, note transport;
//   - http_status: the TUNNEL works but the target objects → the
//     candidate may still be usable; verification flags it, selection
//     does not immediately discard it;
//   - core: the local core failed → backend fallback, not candidate
//     switch.
type FailureClass string

const (
	FailureNone      FailureClass = ""
	FailureTimeout   FailureClass = "timeout"
	FailureRefused   FailureClass = "refused"
	FailureReset     FailureClass = "reset"
	FailureTLS       FailureClass = "tls"
	FailureHTTP      FailureClass = "http_status"
	FailureProxyHand FailureClass = "proxy_handshake"
	FailureCore      FailureClass = "core"
)

// ClassifyVerifyFailure maps a failed URL test onto a failure class.
func ClassifyVerifyFailure(m config.URLTestMetrics) FailureClass {
	if m.OK {
		return FailureNone
	}

	switch m.Error {
	case "timeout":
		return FailureTimeout
	case "refused":
		return FailureRefused
	case "reset":
		return FailureReset
	case "proxy":
		return FailureProxyHand
	case "cancelled":
		return FailureTimeout
	}

	// v0.9.9: one lowercase allocation, not three.
	lower := strings.ToLower(m.Error)

	if strings.Contains(lower, "tls") ||
		strings.Contains(lower, "certificate") ||
		strings.Contains(lower, "handshake") {
		return FailureTLS
	}

	if m.Status >= 400 {
		return FailureHTTP
	}

	return FailureReset
}

// Describe renders a credential-free explanation.
func (r VerifyResult) Describe() string {
	if r.OK {
		target := "quorum"
		if r.Required > 0 {
			target = fmt.Sprintf("quorum %d/%d", r.Succeeded, r.Required)
		}

		return fmt.Sprintf("verified: %s (HTTP %d in %d ms, tunnel probe %d ms, %d probes)",
			target, r.Metrics.Status, r.Metrics.TotalMS, r.TunnelProbeMS, r.maxOne(r.Probes))
	}

	if r.FailureClass == FailureCore {
		return fmt.Sprintf("verification failed (%s): %s", r.FailureClass, r.Metrics.Error)
	}

	return fmt.Sprintf("verification failed (%s, %d/%d targets): %s",
		r.FailureClass, r.Succeeded, r.maxOne(r.Required), r.Metrics.Error)
}

func (r VerifyResult) maxOne(v int) int {
	if v < 1 {
		return 1
	}

	return v
}

// aggregateFailureClass folds per-target failures into one class:
// core failures dominate (local problem), then deterministic
// classes, then transient ones.
func aggregateFailureClass(targets []TargetResult, cancelled bool) FailureClass {
	if cancelled {
		return FailureTimeout
	}

	counts := map[FailureClass]int{}

	for _, t := range targets {
		if t.OK || t.FailureClass == FailureNone {
			continue
		}

		counts[t.FailureClass]++
	}

	if len(counts) == 0 {
		return FailureReset
	}

	// Severity order (first match wins).
	for _, class := range []FailureClass{
		FailureCore, FailureRefused, FailureTLS, FailureHTTP,
		FailureProxyHand, FailureReset, FailureTimeout,
	} {
		if counts[class] > 0 {
			return class
		}
	}

	return FailureReset
}

// VerifyTunnel verifies usable connectivity through the local SOCKS5
// endpoint of a running core instance. endpoint is the instance's
// local inbound address.
//
// v0.9.8.5 flow: bounded parallel probes over the target set →
// quorum check → one bounded transient retry for the targets that
// failed transiently → final quorum decision, all under the overall
// deadline. Cancellation propagates to every probe.
func VerifyTunnel(ctx context.Context, endpoint string, opts VerifyOptions) VerifyResult {
	opts = opts.normalize()

	targets := opts.effectiveTargets()

	result := VerifyResult{Required: quorumFor(targets, opts.Quorum)}

	if endpoint == "" {
		result.Metrics = config.URLTestMetrics{Error: "no local endpoint"}
		result.FailureClass = FailureCore

		return result
	}

	// Overall verification deadline.
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// v0.9.9: the SOCKS dialer is immutable per-round configuration
	// (same endpoint, same budget for every target) — one instance is
	// shared by all probes instead of one allocation per target per
	// phase. Target isolation is unchanged: every probe opens its own
	// connection and owns its own evidence.
	dialer := &socks5.Dialer{ProxyAddr: endpoint, Timeout: opts.PerTarget}

	// --- Phase 1: probe every target with bounded concurrency ----
	// (the target set itself is bounded to MaxVerifyTargets).
	results := probeTargets(ctx, dialer, targets, opts)

	succeeded, probes := tally(results)
	result.Targets = results
	result.Succeeded = succeeded
	result.Probes = probes

	if succeeded >= result.Required {
		return finishSuccess(&result)
	}

	// --- Phase 2: one bounded transient retry --------------------
	// Only genuinely transient failures retry (timeout / reset /
	// proxy handshake / 5xx); deterministic classes stand as-is.
	// The backoff carries a bounded jitter so concurrent
	// verifications do not retry in lockstep.
	if retries := retryableTargets(results); len(retries) > 0 && ctx.Err() == nil {
		sleepWithContext(ctx, opts.Backoff+jitter(opts.Backoff))

		if ctx.Err() == nil {
			index := make(map[string]int, len(results))
			for i, t := range results {
				index[t.URL] = i
			}

			retried := probeTargets(ctx, dialer, retries, opts)

			// Merge the retry evidence back into the result set.
			for _, r := range retried {
				if i, ok := index[r.URL]; ok {
					mergeTargetResult(&results[i], r)
					result.Probes++
				}
			}

			succeeded, _ = tally(results)
			result.Succeeded = succeeded
			result.Retries = len(retried)
		}
	}

	if ctx.Err() != nil {
		result.FailureClass = aggregateFailureClass(results, true)
		result.Metrics = failureMetrics(results)

		return result
	}

	if succeeded >= result.Required {
		return finishSuccess(&result)
	}

	// --- Failure: fold the evidence ------------------------------
	result.FailureClass = aggregateFailureClass(results, false)
	result.Metrics = failureMetrics(results)

	return result
}

// finishSuccess selects the winning probe's metrics (best successful
// latency) for the snapshot surface.
func finishSuccess(result *VerifyResult) VerifyResult {
	best := -1

	for i, t := range result.Targets {
		if !t.OK {
			continue
		}

		if best < 0 || t.LatencyMS < result.Targets[best].LatencyMS {
			best = i
		}
	}

	if best >= 0 {
		winner := result.Targets[best]

		result.Metrics = config.URLTestMetrics{
			URL:     winner.URL,
			At:      time.Now().UTC().UnixMilli(),
			Status:  winner.Status,
			OK:      true,
			TotalMS: winner.LatencyMS,
		}

		result.TunnelProbeMS = winner.TunnelProbeMS
	}

	result.OK = true

	return *result
}

// failureMetrics renders a representative failed-metrics view (the
// most informative failure: prefer a target that actually spoke
// HTTP, else the last failure).
func failureMetrics(results []TargetResult) config.URLTestMetrics {
	metrics := config.URLTestMetrics{}

	for _, t := range results {
		if t.OK {
			continue
		}

		metrics.URL = t.URL
		metrics.At = time.Now().UTC().UnixMilli()

		if t.Status > 0 {
			metrics.Status = t.Status

			metrics.Error = fmt.Sprintf("HTTP %d through tunnel", t.Status)

			break // a speaking target is the most informative failure
		}

		metrics.Error = boundedText(t.Error, 120)
		metrics.Timeout = t.Timeout
	}

	return metrics
}

// probeTargets probes the given targets concurrently (bounded by the
// target-set size itself, which is capped at MaxVerifyTargets). The
// dialer is the round's shared immutable SOCKS configuration.
func probeTargets(ctx context.Context, dialer *socks5.Dialer, targets []string, opts VerifyOptions) []TargetResult {
	results := make([]TargetResult, len(targets))

	var wg sync.WaitGroup

	for i, target := range targets {
		wg.Add(1)

		go func(slot int, targetURL string) {
			defer wg.Done()
			results[slot] = probeTarget(ctx, dialer, targetURL, opts)
		}(i, target)
	}

	wg.Wait()

	return results
}

// retryableTargets returns the URLs whose failures justify ONE more
// bounded attempt (v0.9.8.5 §2.2).
func retryableTargets(results []TargetResult) []string {
	var out []string

	for _, t := range results {
		if t.OK || !t.isTransient() {
			continue
		}

		if t.FailureClass == FailureTimeout && t.Timeout == false && t.Error == "" {
			continue
		}

		out = append(out, t.URL)
	}

	return out
}

// mergeTargetResult folds a retry outcome into the standing evidence.
func mergeTargetResult(standing *TargetResult, retry TargetResult) {
	retry.Attempts = standing.Attempts + 1
	retry.Retried = true

	*standing = retry
}

// tally counts successes and attempts.
func tally(results []TargetResult) (succeeded, probes int) {
	for _, t := range results {
		if t.OK {
			succeeded++
		}

		probes += t.Attempts
	}

	return succeeded, probes
}

// probeTarget runs ONE bounded HTTPS probe through the tunnel.
func probeTarget(ctx context.Context, dialer *socks5.Dialer, targetURL string, opts VerifyOptions) TargetResult {
	result := TargetResult{URL: targetURL, Attempts: 1}

	parsed, err := url.Parse(targetURL)
	if err != nil || parsed.Host == "" {
		result.MarkedInvalid("invalid verify target")
		return result
	}

	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	// The probe budget: the remaining overall deadline or the
	// per-target bound, whichever is smaller.
	budget := opts.PerTarget

	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < budget {
			budget = remaining
		}
	}

	if budget <= 0 {
		result.MarkedInvalid("verification budget exhausted")
		return result
	}

	// Tunnel probe: SOCKS CONNECT round-trip (the end-to-end ping
	// implied by a working tunnel).
	probeStart := time.Now()

	probeConn, dialErr := dialer.Dial(ctx, "tcp", joinHostPort(parsed.Hostname(), port))

	probeMS := time.Since(probeStart).Milliseconds()

	if probeConn != nil {
		_ = probeConn.Close()
	}

	if dialErr != nil {
		result.LatencyMS = probeMS
		result.Measured = probeMS > 0
		result.Error = classifyDialError(dialErr)
		result.Timeout = isTimeoutKind(dialErr) || result.Error == "timeout"
		result.FailureClass = ClassifyVerifyFailure(config.URLTestMetrics{Error: result.Error})

		return result
	}

	result.TunnelProbeMS = probeMS

	// Full HTTP verification through the tunnel.
	metrics := verifyThroughDialer(ctx, dialer.Dial, targetURL, budget)

	result.Status = metrics.Status
	result.LatencyMS = metrics.TotalMS
	result.Measured = true
	result.Error = metrics.Error
	result.Timeout = metrics.Timeout
	result.FailureClass = ClassifyVerifyFailure(metrics)

	if metrics.OK {
		result.OK = true
		result.Error = ""
		result.FailureClass = FailureNone
	}

	return result
}

// MarkedInvalid records a deterministic (non-retryable) target-level
// rejection.
func (t *TargetResult) MarkedInvalid(reason string) {
	t.Error = reason
	t.FailureClass = FailureCore
}

// verifyThroughDialer runs one bounded HTTP GET through the dialer.
func verifyThroughDialer(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), targetURL string, timeout time.Duration) config.URLTestMetrics {
	transport := &http.Transport{
		DialContext:         dial,
		DisableKeepAlives:   true, // measurement, not reuse
		TLSHandshakeTimeout: timeout,
	}

	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: timeout}

	started := time.Now().UTC()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return config.URLTestMetrics{URL: targetURL, At: started.UnixMilli(), Error: "invalid request"}
	}

	resp, err := client.Do(req)
	if err != nil {
		return config.URLTestMetrics{
			URL: targetURL, At: started.UnixMilli(),
			Error: classifyDialError(err), Timeout: isTimeoutKind(err),
			TotalMS: time.Since(started).Milliseconds(),
		}
	}

	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))

	ok := resp.StatusCode >= 200 && resp.StatusCode < 400

	metrics := config.URLTestMetrics{
		URL:     targetURL,
		At:      started.UnixMilli(),
		Status:  resp.StatusCode,
		OK:      ok,
		TotalMS: time.Since(started).Milliseconds(),
	}

	if !ok {
		metrics.Error = fmt.Sprintf("HTTP %d through tunnel", resp.StatusCode)
	}

	return metrics
}

// sleepWithContext waits for the bounded backoff (or cancellation).
func sleepWithContext(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// jitter adds up to +50% randomized delay (bounded, seeded once).
func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	return time.Duration(rand.Int63n(int64(base/2) + 1)) //nolint:gosec // bounded jitter, no security role
}

func classifyDialError(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()

	// Timeout first: a SOCKS handshake that hit its deadline is a
	// timeout regardless of which layer reported it (the socks5
	// handshake wraps the deadline as %v, so the error chain alone is
	// not enough).
	switch {
	case isTimeoutKind(err) ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "refused"):
		return "refused"
	case strings.Contains(msg, "reset"):
		return "reset"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case strings.Contains(msg, "socks"):
		return "proxy"
	default:
		return boundedText(msg, 80)
	}
}

func isTimeoutKind(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var te interface{ Timeout() bool }

	return errors.As(err, &te) && te.Timeout()
}

func boundedText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	return s[:limit]
}

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}

	return host + ":" + port
}
