package tester

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// urltest.go implements the URL test mode (v0.9.6 §8): an actual HTTP
// request through the candidate's tunnel with a per-phase timing
// breakdown, proving USABLE connectivity rather than endpoint
// reachability.
//
// Phase semantics (documented honestly, never fabricated):
//
//   - DNS: for requests through a proxy tunnel the target hostname is
//     resolved BY THE PROXY (remote DNS); the local resolver phase is
//     empty and its cost is part of the connect phase. For direct
//     (unproxied) requests the DNS phase is the real local resolution.
//   - Connect: the SOCKS5 CONNECT round-trip through the tunnel (or
//     the plain TCP dial for direct tests).
//   - TLS: the TLS handshake performed over the established tunnel.
//   - TTFB: request written → first response byte.
//   - Total: the complete request wall time, including body drain.
//
// The transport is disposable (DisableKeepAlives): a measurement must
// never ride a warmed-up connection.

// DefaultURLTestTarget is the default connectivity-verification URL:
// a 204 endpoint that is tiny, cache-free and operated independently
// of any configuration source.
const DefaultURLTestTarget = "https://www.gstatic.com/generate_204"

// DefaultURLTestTimeout bounds one URL test.
const DefaultURLTestTimeout = 12 * time.Second

// URLTester executes one URL test through a supplied dial function
// (normally a socks5.Dialer bound to a running core instance) or
// directly when no dialer is supplied.
type URLTester struct {
	// Timeout bounds the whole request (default DefaultURLTestTimeout).
	Timeout time.Duration

	// MaxBody bounds how many response bytes are drained
	// (default 64 KiB — enough to prove real payload delivery without
	// turning a connectivity probe into a download).
	MaxBody int64
}

// DialFunc connects to host:port on behalf of the URL test. The
// socks5.Dialer satisfies it.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// normalize fills defaults.
func (u *URLTester) normalize() *URLTester {
	if u == nil {
		u = &URLTester{}
	}

	if u.Timeout <= 0 {
		u.Timeout = DefaultURLTestTimeout
	}

	if u.MaxBody <= 0 {
		u.MaxBody = 64 << 10
	}

	return u
}

// Test performs one HTTP GET against target through dial (nil =
// direct connection) and returns the phase-timed metrics.
func (u *URLTester) Test(ctx context.Context, dial DialFunc, target string) config.URLTestMetrics {
	u = u.normalize()

	metrics := config.URLTestMetrics{
		URL: target,
		At:  time.Now().UTC().UnixMilli(),
	}

	if target == "" {
		target = DefaultURLTestTarget
		metrics.URL = target
	}

	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		metrics.Error = fmt.Sprintf("invalid test URL %q", target)

		return metrics
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		metrics.Error = fmt.Sprintf("unsupported test URL scheme %q", parsed.Scheme)

		return metrics
	}

	ctx, cancel := context.WithTimeout(ctx, u.Timeout)
	defer cancel()

	// ---- Phase instrumentation ----
	var (
		requestStart = time.Now()
		dnsStart     time.Time
		tlsStart     time.Time
		dnsDur       int64
		connectDur   int64
		tlsDur       int64
		tlsUsed      bool
		reqWritten   time.Time
		firstByteAt  time.Time
	)

	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				dnsDur = time.Since(dnsStart).Milliseconds()
			}
		},
		ConnectStart: func(network, addr string) {},
		ConnectDone:  func(network, addr string, err error) {},
		TLSHandshakeStart: func() {
			tlsUsed = true
			tlsStart = time.Now()
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			if tlsUsed && !tlsStart.IsZero() {
				tlsDur = time.Since(tlsStart).Milliseconds()
			}
		},
		WroteRequest: func(wr httptrace.WroteRequestInfo) {
			reqWritten = time.Now()
		},
		GotFirstResponseByte: func() {
			firstByteAt = time.Now()
		},
	}

	// The custom dialer timing covers the SOCKS CONNECT round trip
	// (and, for proxied requests, the remote DNS the proxy performs).
	innerDial := dial

	if innerDial == nil {
		innerDial = (&net.Dialer{Timeout: u.Timeout}).DialContext
	}

	timedDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		started := time.Now()

		conn, err := innerDial(ctx, network, addr)

		connectDur = time.Since(started).Milliseconds()

		return conn, err
	}

	transport := &http.Transport{
		DialContext:         timedDial,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: u.Timeout,
		// Phase timings are measured by the trace hooks; the proxy has
		// already authenticated by the time TLS runs.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}

	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   u.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Follow redirects (they are real connectivity); cap the
			// chain like browsers do.
			if len(via) >= 5 {
				return errors.New("tester: too many redirects")
			}

			return nil
		},
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, target, nil)
	if err != nil {
		metrics.Error = classifyURLFailure(err)

		return metrics
	}

	req.Header.Set("User-Agent", "FreeIran-URLTest/1.0")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)

	metrics.DNSMS = dnsDur
	metrics.ConnectMS = connectDur

	if tlsUsed {
		metrics.TLSMS = tlsDur
	} else {
		metrics.TLSMS = -1 // no TLS on this request
	}

	if !reqWritten.IsZero() && !firstByteAt.IsZero() {
		metrics.TTFBMS = firstByteAt.Sub(reqWritten).Milliseconds()
	}

	if err != nil {
		metrics.Timeout = isDeadlineErr(err) || errors.Is(err, context.DeadlineExceeded)
		metrics.Error = classifyURLFailure(err)
		metrics.TotalMS = time.Since(requestStart).Milliseconds()

		return metrics
	}

	defer resp.Body.Close()

	metrics.Status = resp.StatusCode

	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, u.MaxBody))

	metrics.Bytes = n
	metrics.TotalMS = time.Since(requestStart).Milliseconds()
	metrics.OK = resp.StatusCode >= 200 && resp.StatusCode < 400

	if !metrics.OK {
		metrics.Error = fmt.Sprintf("HTTP %d via tunnel", resp.StatusCode)
	}

	return metrics
}

// classifyURLFailure turns a transport error into a short, credential
// free failure class for persistence and display.
func classifyURLFailure(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()

	switch {
	case errors.Is(err, context.DeadlineExceeded) || isDeadlineErr(err):
		return "timeout"
	case strings.Contains(msg, "connection refused"):
		return "refused"
	case strings.Contains(msg, "connection reset"):
		return "reset"
	case strings.Contains(msg, "proxy"):
		return "proxy"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		if len(msg) > 80 {
			msg = msg[:80]
		}

		return msg
	}
}

// hostPortOf extracts the host:port a URL test would dial.
func hostPortOf(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return ""
	}

	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	return net.JoinHostPort(parsed.Hostname(), port)
}
