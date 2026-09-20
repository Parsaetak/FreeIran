// tools_impl.go implements the connectivity (§6) and protocol tools
// on top of the shared ToolRunner contract. Every implementation is
// bounded by the runner's context, caps response sizes, follows at
// most Safety.MaxRedirects redirects and reports measurements with
// the canonical v0.9.8.1 latency semantics.
package netcheck

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// ---- shared helpers -------------------------------------------------

// defaultToolTarget returns the safe autonomous default per tool.
// Every default is a public, curated endpoint — private ranges are
// blocked for autonomous targets by policy.
func defaultToolTarget(tool ToolID) string {
	switch tool {
	case ToolInternet:
		return "builtin probe set"
	case ToolDNS:
		return "system resolver"
	case ToolTCP:
		return "1.1.1.1:443"
	case ToolTLS:
		return "www.gstatic.com:443"
	case ToolHTTPS:
		return "https://www.gstatic.com/generate_204"
	case ToolHTTPConnect:
		return "http://127.0.0.1:1080"
	case ToolSOCKS5:
		return "127.0.0.1:1080"
	case ToolWebSocket:
		return "wss://echo.websocket.events"
	case ToolUDP:
		return "1.1.1.1:53"
	case ToolQUIC:
		return ""
	case ToolTraceroute:
		return "1.1.1.1"
	case ToolPathMTU:
		return "1.1.1.1:53"
	case ToolCaptivePortal:
		return "builtin portal probe set"
	case ToolPublicIP:
		return "builtin identity endpoints"
	case ToolTunnelDiagnostics:
		return "active tunnel"
	default:
		return ""
	}
}

// toolDial connects to host:port honouring the request path (direct
// with the rebinding guard, or through the supplied tunnel dialer)
// and returns the connection plus the measured dial latency.
func toolDial(ctx context.Context, req ToolRequest, safety Safety, host string, port int) (net.Conn, time.Duration, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	started := time.Now()

	var (
		conn net.Conn
		err  error
	)

	if req.Path == PathTunneled && req.Dial != nil {
		conn, err = req.Dial(ctx, "tcp", addr)
	} else {
		conn, err = safety.dialer(req.Tool).Dial(ctx, "tcp", addr)
	}

	if err != nil {
		return nil, 0, err
	}

	return conn, measuredSince(started), nil
}

// measuredSince converts an elapsed wall reading into the canonical
// measurement (strictly positive on success, sub-ms flagged).
func measuredSince(started time.Time) time.Duration {
	d := time.Since(started)
	if d <= 0 {
		return ClockFloorTool
	}

	return d
}

// ClockFloorTool mirrors tester.ClockFloor for the tools layer (the
// packages are deliberately decoupled; the semantics are identical).
const ClockFloorTool = time.Nanosecond

// measurementFor builds the canonical latency measurement fields.
func measurementFor(d time.Duration) ToolMeasurement {
	ms := d.Milliseconds()

	return ToolMeasurement{
		LatencyMS: ms,
		Measured:  true,
		SubMS:     ms <= 0,
	}
}

// setStatusFromError classifies ctx/timeout errors into statuses.
// Policy rejections (targetRejectedError — from validation, the
// dial-time rebinding guard, or a redirect hop) classify as
// invalid_target: the safety layer refused the destination, which is
// not a network failure.
func setStatusFromError(result *ToolResult, err error) {
	if err == nil {
		return
	}

	var rejected *targetRejectedError

	if errors.As(err, &rejected) {
		result.Status = ToolStatusInvalid
		result.Error = err.Error()

		return
	}

	msg := err.Error()

	switch {
	case errors.Is(err, context.Canceled), isCtxCancelled(err):
		result.Status = ToolStatusCancelled
	case errors.Is(err, context.DeadlineExceeded), isCtxDeadline(err):
		result.Status = ToolStatusTimeout
	default:
		result.Status = ToolStatusFailed
	}

	result.Error = msg
}

func isCtxCancelled(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}

	msg := err.Error()

	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "operation was canceled")
}

func isCtxDeadline(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "timeout")
}

// targetHostPort splits a validated target into host + default port.
func targetHostPort(req ToolRequest, defaultPort int) (string, int) {
	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = defaultToolTarget(req.Tool)
	}

	if host, portStr, err := net.SplitHostPort(target); err == nil {
		if port, perr := strconv.Atoi(portStr); perr == nil {
			return host, port
		}
	}

	return target, defaultPort
}

// ---- connectivity tools ---------------------------------------------

// runInternet wraps the classic multi-target connectivity report as
// one aggregate tool execution.
func (r *ToolRunner) runInternet(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "tcp/https"

	cfg := Defaults()
	if req.Path == PathTunneled && req.Dial != nil {
		// Route the probe set through the active tunnel.
		cfg.ProxyAddr = "" // proxy field expects a raw address; dialer path below
	}

	checker := &tunnelChecker{dial: req.Dial, provider: req.Provider}

	report := checker.run(ctx, cfg)

	result.Target = fmt.Sprintf("%d targets", report.TargetCount)

	if report.Cancelled {
		result.Status = ToolStatusCancelled
		result.Error = "cancelled"

		return
	}

	result.Status = stateToToolStatus(report.State)
	result.Measurement = ToolMeasurement{
		LatencyMS: report.LatencyMS,
		Measured:  report.LatencyMS > 0,
		Probes:    report.TargetCount,
	}
	result.Details = map[string]string{
		"state":    string(report.State),
		"summary":  report.Summary,
		"checked":  report.CheckedAt.Format(time.RFC3339),
		"duration": strconv.FormatInt(report.DurationMS, 10) + " ms",
	}
}

func stateToToolStatus(state State) ToolStatus {
	switch state {
	case StateOK, StateHighLatency, StateProxyOnly:
		return ToolStatusOK
	default:
		return ToolStatusFailed
	}
}

// runDNS is the v0.9.8.5 DNS DIAGNOSTIC (§5): one user-triggered
// run compares the system resolver against the curated public
// resolver set (or one explicit user-supplied resolver), for both
// A and AAAA, over UDP with TCP fallback — every row carrying its
// transport, measured latency, answer count and honest failure
// class. The structured evidence rides on result.DNS.
func (r *ToolRunner) runDNS(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "udp/tcp"

	host, _ := targetHostPort(req, 53)

	host = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))

	// Target semantics (unchanged contract, upgraded engine):
	//   ""            → full diagnostic (system + curated, default name)
	//   hostname      → diagnostic of that name
	//   IP literal    → diagnostic through that ONE explicit resolver
	var (
		diagOpts = DNSDiagnosticOptions{}
		mode     = "system + curated"
	)

	switch {
	case req.Target != "" && net.ParseIP(host) != nil:
		diagOpts.Resolvers = []string{host}
		mode = "resolver " + host
		result.Target = host
	case req.Target != "":
		diagOpts.Name = host
		result.Target = host
	default:
		result.Target = "system + curated resolvers"
	}

	if req.Path == PathTunneled && req.Dial != nil {
		// A tunneled DNS diagnostic resolves through the tunnel: the
		// SYSTEM resolver row would silently leak around the tunnel,
		// so an explicit resolver set is required (the curated public
		// set by default — queries travel through the supplied dialer).
		diagOpts.Resolvers = curatedResolverAddresses()
		diagOpts.Dial = req.Dial
		mode = "curated via tunnel"
	}

	report := RunDNSDiagnostic(ctx, diagOpts)

	result.DNS = &report

	// Aggregate surface: the best successful evidence feeds the
	// shared measurement fields; the status is honest about how many
	// resolvers answered.
	bestLatency, bestAnswers, okCount, total := summarizeDNSReport(&report)

	result.Measurement = ToolMeasurement{
		LatencyMS:    bestLatency,
		Measured:     bestLatency > 0,
		Addresses:    bestAnswers,
		AddressCount: len(bestAnswers),
		Probes:       total,
	}

	result.Details = map[string]string{
		"name":        report.Name,
		"mode":        mode,
		"types":       "A, AAAA",
		"resolvers":   strconv.Itoa(len(report.Resolvers)),
		"user_action": "DNS diagnostics are never run automatically",
	}

	switch {
	case okCount > 0:
		result.Status = ToolStatusOK
	case ctx.Err() != nil:
		result.Status = ToolStatusCancelled
		result.Error = "cancelled"
	default:
		result.Status = ToolStatusFailed

		if class, msg := dominantDNSFailure(&report); class != "" {
			result.Error = msg
			result.Details["failure_class"] = class
		} else {
			result.Error = "every resolver failed"
		}
	}
}

// curatedResolverAddresses lists the curated set as bare addresses.
func curatedResolverAddresses() []string {
	out := make([]string, 0, len(CuratedDNSResolvers))
	for _, r := range CuratedDNSResolvers {
		out = append(out, r.Address)
	}
	return out
}

// summarizeDNSReport extracts the best successful evidence.
func summarizeDNSReport(report *DNSDiagnosticReport) (bestLatency int64, bestAnswers []string, okCount, total int) {
	bestSet := false

	for _, resolver := range report.Resolvers {
		for _, q := range resolver.Queries {
			total++

			if !q.OK {
				continue
			}

			if !bestSet || (q.LatencyMS > 0 && q.LatencyMS < bestLatency) {
				bestSet = true
				bestLatency = q.LatencyMS
				bestAnswers = q.Addresses
			}
		}
	}

	for _, resolver := range report.Resolvers {
		if resolver.OK {
			okCount++
		}
	}

	return bestLatency, bestAnswers, okCount, total
}

// dominantDNSFailure renders the most common failure class of a
// fully failed run.
func dominantDNSFailure(report *DNSDiagnosticReport) (string, string) {
	counts := map[DNSFailureClass]int{}
	samples := map[DNSFailureClass]string{}

	for _, resolver := range report.Resolvers {
		for _, q := range resolver.Queries {
			if q.OK || q.FailureClass == DNSFailNone {
				continue
			}

			counts[q.FailureClass]++
			samples[q.FailureClass] = q.Resolver + " " + string(q.RecordType) + ": " + q.Error
		}
	}

	best, bestN := DNSFailureClass(""), 0
	for class, n := range counts {
		if n > bestN {
			best, bestN = class, n
		}
	}

	if best == DNSFailureClass("") {
		return "", ""
	}

	return best.HumanLabel(), samples[best]
}

// boundedAddresses caps and deduplicates reported addresses.
func boundedAddresses(addrs []string, limit int) []string {
	seen := make(map[string]struct{}, len(addrs))
	out := make([]string, 0, limit)

	for _, a := range addrs {
		if _, dup := seen[a]; dup {
			continue
		}

		seen[a] = struct{}{}

		if len(out) < limit {
			out = append(out, a)
		}
	}

	return out
}

// runTCP dials one endpoint and reports the measured RTT.
func (r *ToolRunner) runTCP(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "tcp"

	host, port := targetHostPort(req, 443)

	conn, latency, err := toolDial(ctx, req, r.Safety, host, port)
	if err != nil {
		setStatusFromError(result, err)
		result.Details = map[string]string{"endpoint": net.JoinHostPort(host, strconv.Itoa(port))}

		return
	}

	_ = conn.Close()

	result.Status = ToolStatusOK
	result.Measurement = measurementFor(latency)
	result.Details = map[string]string{"endpoint": net.JoinHostPort(host, strconv.Itoa(port))}
}

// runTLS performs the TCP dial plus a real TLS handshake and reports
// the negotiated protocol facts.
func (r *ToolRunner) runTLS(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "tls"

	host, port := targetHostPort(req, 443)

	conn, latency, err := toolDial(ctx, req, r.Safety, host, port)
	if err != nil {
		setStatusFromError(result, err)

		return
	}

	serverName := host
	if ip := net.ParseIP(host); ip != nil {
		serverName = "" // IP-literal: no SNI spoofing
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: r.Safety.AllowInsecureTLS, // explicit user opt-in only
	})

	handshakeStart := time.Now()

	handshakeErr := tlsConn.HandshakeContext(ctx)
	handshakeElapsed := time.Since(handshakeStart)

	if handshakeErr != nil {
		_ = tlsConn.Close()
		setStatusFromError(result, handshakeErr)

		return
	}

	state := tlsConn.ConnectionState()

	_ = tlsConn.Close()

	total := latency + handshakeElapsed

	result.Status = ToolStatusOK
	result.Measurement = measurementFor(total)
	result.Measurement.TLSVersion = tlsVersionLabel(state.Version)
	result.Measurement.Cipher = tls.CipherSuiteName(state.CipherSuite)
	result.Details = map[string]string{
		"handshake_ms": strconv.FormatInt(handshakeElapsed.Milliseconds(), 10),
		"peer_certs":   strconv.Itoa(len(state.PeerCertificates)),
	}
}

func tlsVersionLabel(v uint16) string {
	switch v {
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

// runHTTPS performs a bounded GET with capped redirects and a capped
// body drain, direct or through the active tunnel.
func (r *ToolRunner) runHTTPS(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "https"

	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = defaultToolTarget(ToolHTTPS)
	}

	if _, err := url.Parse(target); err != nil {
		result.Status = ToolStatusInvalid
		result.Error = "invalid URL: " + err.Error()

		return
	}

	client := r.boundedHTTPClient(req)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		result.Status = ToolStatusInvalid
		result.Error = err.Error()

		return
	}

	request.Header.Set("User-Agent", "FreeIran-Tools/1.0")
	request.Header.Set("Cache-Control", "no-cache")

	// Hop counting for the redirect detail (cap + destination
	// re-validation still apply — see redirectPolicy).
	redirectHops := 0
	client.CheckRedirect = r.redirectPolicy(req.Tool, &redirectHops)

	started := time.Now()

	resp, err := client.Do(request)
	elapsed := time.Since(started)

	if err != nil {
		setStatusFromError(result, err)

		return
	}

	defer resp.Body.Close()

	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, r.Safety.responseCap()))

	result.Measurement = measurementFor(elapsed)
	result.Measurement.Status = resp.StatusCode
	result.Measurement.Bytes = n

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		result.Status = ToolStatusOK
	} else {
		result.Status = ToolStatusFailed
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	result.Details = map[string]string{
		"final_url": boundedString(resp.Request.URL.String(), 200),
		"redirects": strconv.Itoa(redirectHops),
		"tls":       boolLabel(resp.TLS != nil),
	}
}

// boundedString truncates for reporting.
func boundedString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	return s[:limit] + "…"
}

func boolLabel(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

// responseCap returns the body cap with a floor.
func (s Safety) responseCap() int64 {
	if s.MaxResponseBytes <= 0 {
		return 256 << 10
	}

	return s.MaxResponseBytes
}

// redirectCap returns the redirect cap with a floor.
func (s Safety) redirectCap() int {
	if s.MaxRedirects <= 0 {
		return 3
	}

	return s.MaxRedirects
}

// redirectPolicy builds the shared CheckRedirect closure: cap the
// chain, count hops for reporting (when hops != nil), and re-validate
// every hop's destination against the same policy as the initial
// target.
func (r *ToolRunner) redirectPolicy(tool ToolID, hops *int) func(*http.Request, []*http.Request) error {
	return func(next *http.Request, via []*http.Request) error {
		if hops != nil {
			*hops = len(via)
		}

		if len(via) >= r.Safety.redirectCap() {
			return fmt.Errorf("too many redirects (>%d)", r.Safety.redirectCap())
		}

		return r.Safety.validateRedirectTarget(tool, next.URL)
	}
}

// boundedHTTPClient builds the disposable, bounded transport for HTTP
// tools (direct with rebinding guard, or through the tunnel dialer).
// Redirect policy: chains are capped AND every hop's destination is
// re-validated against the same no-credentials and private-
// destination policy as the initial target — a redirect can never
// steer the request onto a loopback/private endpoint (direct hops
// are additionally guarded at dial time by the safeDialer).
func (r *ToolRunner) boundedHTTPClient(req ToolRequest) *http.Client {
	transport := &http.Transport{
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	if req.Path == PathTunneled && req.Dial != nil {
		transport.DialContext = req.Dial
	} else {
		transport.DialContext = r.Safety.dialer(req.Tool).Dial
	}

	return &http.Client{
		Transport:     transport,
		Timeout:       30 * time.Second,
		CheckRedirect: r.redirectPolicy(req.Tool, nil),
	}
}

// ---- protocol tools ---------------------------------------------------

// runHTTPConnect tests an HTTP proxy's CONNECT capability with a
// bounded, manual exchange (no credentials — public proxies are not
// supported by policy; local endpoints are the use case).
func (r *ToolRunner) runHTTPConnect(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "http-connect"

	// The proxy under test.
	proxyHost, proxyPort := targetHostPort(req, 1080)

	// CONNECT destination: a public, fixed target (never user URLs).
	const connectHost = "www.gstatic.com"
	const connectPort = 443

	conn, dialLatency, err := toolDial(ctx, req, r.Safety, proxyHost, proxyPort)
	if err != nil {
		setStatusFromError(result, err)

		return
	}

	defer conn.Close()

	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	_ = conn.SetDeadline(deadline)

	request := fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n",
		connectHost, connectPort, connectHost, connectPort)

	writeStart := time.Now()

	if _, err := conn.Write([]byte(request)); err != nil {
		setStatusFromError(result, err)

		return
	}

	line, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadString('\n')
	elapsed := time.Since(writeStart)

	if err != nil {
		setStatusFromError(result, err)

		return
	}

	result.Measurement = measurementFor(dialLatency + elapsed)

	status := parseHTTPStatusLine(line)
	result.Measurement.Status = status

	switch {
	case status >= 200 && status < 300:
		result.Status = ToolStatusOK
	case status == 407:
		result.Status = ToolStatusFailed
		result.Error = "proxy requires authentication (not supported by design)"
	default:
		result.Status = ToolStatusFailed
		result.Error = strings.TrimSpace(line)
	}

	result.Details = map[string]string{
		"proxy":       net.JoinHostPort(proxyHost, strconv.Itoa(proxyPort)),
		"connects_to": net.JoinHostPort(connectHost, strconv.Itoa(connectPort)),
	}
}

// parseHTTPStatusLine extracts the code from "HTTP/1.x CODE ...".
func parseHTTPStatusLine(line string) int {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0
	}

	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}

	return code
}

// runSOCKS5 verifies a SOCKS5 endpoint performs a real CONNECT
// round trip through the existing engine dialer.
func (r *ToolRunner) runSOCKS5(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "socks5"

	host, port := targetHostPort(req, 1080)
	proxyAddr := net.JoinHostPort(host, strconv.Itoa(port))

	dialer := socks5.Dialer{ProxyAddr: proxyAddr, Timeout: 8 * time.Second}

	started := time.Now()

	conn, err := dialer.Dial(ctx, "tcp", "www.gstatic.com:443")
	elapsed := time.Since(started)

	if err != nil {
		setStatusFromError(result, err)

		return
	}

	_ = conn.Close()

	result.Status = ToolStatusOK
	result.Measurement = measurementFor(elapsed)
	result.Details = map[string]string{"proxy": proxyAddr, "connects_to": "www.gstatic.com:443"}
}

// runWebSocket performs the upgrade handshake (plain TCP or TLS, via
// the tunnel when requested) and reports the negotiated status.
func (r *ToolRunner) runWebSocket(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "ws"

	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = defaultToolTarget(ToolWebSocket)
	}

	parsed, err := url.Parse(target)
	if err != nil {
		result.Status = ToolStatusInvalid
		result.Error = "invalid WebSocket URL: " + err.Error()

		return
	}

	host := parsed.Hostname()
	port := 80

	if parsed.Port() != "" {
		if p, perr := strconv.Atoi(parsed.Port()); perr == nil {
			port = p
		}
	}

	useTLS := strings.EqualFold(parsed.Scheme, "wss")
	if useTLS && port == 80 {
		port = 443
	}

	conn, latency, err := toolDial(ctx, req, r.Safety, host, port)
	if err != nil {
		setStatusFromError(result, err)

		return
	}

	defer conn.Close()

	var raw net.Conn = conn

	if useTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: r.Safety.AllowInsecureTLS,
		})

		tlsStart := time.Now()

		if err := tlsConn.HandshakeContext(ctx); err != nil {
			setStatusFromError(result, err)

			return
		}

		latency += time.Since(tlsStart)
		raw = tlsConn
	}

	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	_ = raw.SetDeadline(deadline)

	path := parsed.Path
	if path == "" {
		path = "/"
	}

	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, parsed.Host, randomWSKey())

	writeStart := time.Now()

	if _, err := raw.Write([]byte(request)); err != nil {
		setStatusFromError(result, err)

		return
	}

	line, err := bufio.NewReader(io.LimitReader(raw, 4096)).ReadString('\n')
	elapsed := time.Since(writeStart)

	if err != nil {
		setStatusFromError(result, err)

		return
	}

	status := parseHTTPStatusLine(line)
	result.Measurement = measurementFor(latency + elapsed)
	result.Measurement.Status = status

	if status == 101 {
		result.Status = ToolStatusOK
	} else if status > 0 {
		result.Status = ToolStatusFailed
		result.Error = strings.TrimSpace(line)
	} else {
		result.Status = ToolStatusFailed
		result.Error = "no HTTP response"
	}
}

// randomWSKey produces a 16-byte base64-ish key (content is
// irrelevant to the handshake outcome; fixed value keeps the tool
// deterministic and boring in logs).
func randomWSKey() string { return "ZnJlZWlyYW4tdG9vbHM=" }

// runUDP performs a real DNS query over UDP to the target resolver
// and reports whether a datagram round trip completed.
func (r *ToolRunner) runUDP(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "udp"

	host, port := targetHostPort(req, 53)
	resolver := net.JoinHostPort(host, strconv.Itoa(port))

	if !r.Safety.privateTargetsAllowed(req.Tool) {
		if ip := net.ParseIP(host); ip != nil && IsPrivateIP(ip) {
			result.Status = ToolStatusInvalid
			result.Error = "private resolver blocked"

			return
		}
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	var conn net.Conn

	started := time.Now()

	conn, err := dialer.DialContext(ctx, "udp", resolver)
	if err != nil {
		setStatusFromError(result, err)

		return
	}

	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}

	// A minimal, well-formed A query for a fixed probe name.
	query := buildDNSQuery("tools.freeiran.probe")

	if _, err := conn.Write(query); err != nil {
		setStatusFromError(result, err)

		return
	}

	buf := make([]byte, 1500)

	n, err := conn.Read(buf)
	elapsed := time.Since(started)

	if err != nil {
		setStatusFromError(result, err)

		return
	}

	result.Status = ToolStatusOK
	result.Measurement = measurementFor(elapsed)
	result.Measurement.Bytes = int64(n)
	result.Measurement.Status = dnsResponseCode(buf[:n])
	result.Details = map[string]string{"resolver": resolver, "query": "A tools.freeiran.probe"}
}

// buildDNSQuery encodes a minimal DNS A query (RFC 1035 §4.2.1).
func buildDNSQuery(name string) []byte {
	var out []byte

	// Header: ID 0xtool, RD, one question.
	out = append(out, 0x07, 0x0b, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)

	for _, label := range strings.Split(name, ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}

	out = append(out, 0x00)    // root label
	out = append(out, 0x00, 1) // QTYPE A
	out = append(out, 0x00, 1) // QCLASS IN

	return out
}

// dnsResponseCode extracts the RCODE from a response buffer.
func dnsResponseCode(resp []byte) int {
	if len(resp) < 4 {
		return -1
	}

	return int(resp[3] & 0x0f)
}

// tunnelChecker adapts the existing multi-target Checker to run
// through the active tunnel dialer when supplied (§6 "multiple
// bounded targets").
type tunnelChecker struct {
	dial     DialFunc
	provider string
}

// run executes the classic probe set, routing HTTPS probes through
// the tunnel dialer when present.
func (t *tunnelChecker) run(ctx context.Context, cfg Config) Report {
	if t.dial == nil {
		return New(cfg).Run(ctx)
	}

	// Reuse the classic report shape with a tunneled HTTPS client.
	started := time.Now().UTC()

	report := Report{
		CheckedAt:   started,
		TargetCount: len(cfg.HTTPSURLs) + 1,
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:         t.dial,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 6 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}

			return nil
		},
	}

	defer client.CloseIdleConnections()

	for _, target := range cfg.HTTPSURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			continue
		}

		probeStart := time.Now()

		resp, err := client.Do(req)
		latency := time.Since(probeStart).Milliseconds()

		ok := false
		errText := ""

		if err != nil {
			errText = err.Error()
		} else {
			_ = resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				ok = true
			} else {
				errText = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
		}

		report.HTTPS = append(report.HTTPS, CheckResult{
			Name: target, Target: target, OK: ok, LatencyMS: maxI64(latency, 1), Error: errText,
		})
	}

	sortResults(report.HTTPS)

	if best := bestLatency(report.HTTPS); best > 0 {
		report.LatencyMS = best
	}

	report.DurationMS = maxI64(time.Since(started).Milliseconds(), 1)

	if anyOK(report.HTTPS) {
		report.State = StateOK
		report.Summary = "Internet reachable through the active tunnel."

		if bestLatency(report.HTTPS) > cfg.HighLatencyThresholdMS {
			report.State = StateHighLatency
			report.Summary = "Internet reachable through the tunnel but slow."
		}
	} else {
		report.State = StateCoreNoInternet
		report.Summary = "No connectivity through the active tunnel."
	}

	if ctx.Err() != nil {
		report.Cancelled = true
	}

	return report
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}

	return b
}
