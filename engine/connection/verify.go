package connection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
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
// A successful core startup followed by a failed verification means:
// the local process is healthy but the remote path is not (blocked,
// credential-rejected, protocol-broken). Those are exactly the cases
// where switching candidates is the correct response.

// VerifyOptions tune one tunnel verification.
type VerifyOptions struct {
	// URL is the verification target ("" = DefaultVerifyTarget).
	URL string

	// Timeout bounds the whole verification (default 12s).
	Timeout time.Duration
}

// DefaultVerifyTarget is the connectivity-verification endpoint.
const DefaultVerifyTarget = "https://www.gstatic.com/generate_204"

// DefaultVerifyTimeout bounds one verification.
const DefaultVerifyTimeout = 12 * time.Second

func (o VerifyOptions) normalize() VerifyOptions {
	if o.URL == "" {
		o.URL = DefaultVerifyTarget
	}

	if o.Timeout <= 0 {
		o.Timeout = DefaultVerifyTimeout
	}

	return o
}

// VerifyResult is the outcome of one tunnel verification.
type VerifyResult struct {
	// OK reports verified usable connectivity.
	OK bool

	// Metrics is the measured URL test (phases, status, timing).
	Metrics config.URLTestMetrics

	// TunnelProbeMS is the SOCKS CONNECT round-trip through the
	// tunnel (the end-to-end ping implied by a working tunnel).
	TunnelProbeMS int64

	// FailureClass classifies the failure ("" on success).
	FailureClass FailureClass
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

	if strings.Contains(strings.ToLower(m.Error), "tls") ||
		strings.Contains(strings.ToLower(m.Error), "certificate") ||
		strings.Contains(strings.ToLower(m.Error), "handshake") {
		return FailureTLS
	}

	if m.Status >= 400 {
		return FailureHTTP
	}

	return FailureReset
}

// DescribeVerification renders a credential-free explanation.
func (r VerifyResult) Describe() string {
	if r.OK {
		return fmt.Sprintf("verified: HTTP %d in %d ms (tunnel probe %d ms)",
			r.Metrics.Status, r.Metrics.TotalMS, r.TunnelProbeMS)
	}

	return fmt.Sprintf("verification failed (%s): %s", r.FailureClass, r.Metrics.Error)
}

// VerifyTunnel verifies usable connectivity through the local SOCKS5
// endpoint of a running core instance. endpoint is the instance's
// local inbound address.
func VerifyTunnel(ctx context.Context, endpoint string, opts VerifyOptions) VerifyResult {
	opts = opts.normalize()

	result := VerifyResult{}

	if endpoint == "" {
		result.Metrics.Error = "no local endpoint"
		result.FailureClass = FailureCore

		return result
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	parsed, err := url.Parse(opts.URL)
	if err != nil || parsed.Host == "" {
		result.Metrics.Error = "invalid verify target"
		result.FailureClass = FailureCore

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

	dialer := &socks5.Dialer{ProxyAddr: endpoint, Timeout: opts.Timeout}

	// Tunnel probe: SOCKS CONNECT round-trip.
	probeStart := time.Now()

	probeConn, dialErr := dialer.Dial(ctx, "tcp", joinHostPort(parsed.Hostname(), port))

	probeMS := time.Since(probeStart).Milliseconds()

	if probeConn != nil {
		_ = probeConn.Close()
	}

	if dialErr != nil {
		result.Metrics = config.URLTestMetrics{
			URL:     opts.URL,
			At:      time.Now().UTC().UnixMilli(),
			Error:   classifyDialError(dialErr),
			Timeout: isTimeoutKind(dialErr),
			TotalMS: probeMS,
		}
		result.FailureClass = ClassifyVerifyFailure(result.Metrics)

		return result
	}

	result.TunnelProbeMS = probeMS

	// Full HTTP verification through the tunnel.
	metrics := verifyThroughDialer(ctx, dialer.Dial, opts)

	result.Metrics = metrics
	result.OK = metrics.OK
	result.FailureClass = ClassifyVerifyFailure(metrics)

	return result
}

// verifyThroughDialer runs one bounded HTTP GET through the dialer.
func verifyThroughDialer(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), opts VerifyOptions) config.URLTestMetrics {
	transport := &http.Transport{
		DialContext:         dial,
		DisableKeepAlives:   true, // measurement, not reuse
		TLSHandshakeTimeout: opts.Timeout,
	}

	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: opts.Timeout}

	started := time.Now().UTC()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		return config.URLTestMetrics{URL: opts.URL, At: started.UnixMilli(), Error: "invalid request"}
	}

	resp, err := client.Do(req)
	if err != nil {
		return config.URLTestMetrics{
			URL: opts.URL, At: started.UnixMilli(),
			Error: classifyDialError(err), Timeout: isTimeoutKind(err),
			TotalMS: time.Since(started).Milliseconds(),
		}
	}

	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))

	ok := resp.StatusCode >= 200 && resp.StatusCode < 400

	metrics := config.URLTestMetrics{
		URL:     opts.URL,
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
		if len(msg) > 80 {
			msg = msg[:80]
		}

		return msg
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

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}

	return host + ":" + port
}
