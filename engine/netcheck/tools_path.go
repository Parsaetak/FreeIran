// tools_path.go implements the path (§6), identity and tunnel
// diagnostics tools: traceroute (privilege-gated ICMP TTL walk),
// path-MTU payload ladder, captive-portal detection, public exit IP
// and the active-tunnel truth snapshot.
package netcheck

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// ---- traceroute ------------------------------------------------------

// runTraceroute walks the path with ICMP echo probes of increasing
// TTL. Raw ICMP sockets require elevated privileges on Unix; on
// Windows the native ICMP Helper API (iphlpapi IcmpSendEcho) provides
// user-mode TTL walks without elevation — the platform walkers live
// in traceroute_unix.go / traceroute_windows.go and the shared walk
// loop is in walkTTLs. Without any working walker the tool reports
// the honest "unsupported" status instead of pretending.
//
// v0.12.1 status semantics (§3/§10):
//
//   - privilege/capability missing
//     → unsupported (never red "failed")
//   - explicit destination-unreachable evidence from the path
//     → unreachable
//   - hops observed, then answers stopped
//     → unreachable WITH the hop evidence kept (bounded)
//   - complete route observed
//     → ok
//   - no hops and no verdict
//     → failed (genuinely undiagnosed)
func (r *ToolRunner) runTraceroute(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "icmp"

	host, _ := targetHostPort(req, 0)

	if req.Path == PathTunneled {
		result.Status = ToolStatusUnsupported
		result.Error = "traceroute operates on the direct path only (ICMP cannot traverse the SOCKS tunnel)"

		return
	}

	ip := net.ParseIP(host)
	if ip == nil {
		answers, err := net.ResolveIPAddr("ip4", host)
		if err != nil {
			setStatusFromError(result, fmt.Errorf("resolve %s: %w", host, err))

			return
		}

		ip = answers.IP
	}

	if ip.To4() == nil {
		result.Status = ToolStatusUnsupported
		result.Error = "IPv6 traceroute is not supported in this build"

		return
	}

	if !r.Safety.AllowPrivateTargets && IsPrivateIP(ip) {
		result.Status = ToolStatusInvalid
		result.Error = "private destination blocked for autonomous traceroute"

		return
	}

	walker, err := newHopWalker(ctx)
	if err != nil {
		// No usable ICMP walker in this context (raw sockets refused
		// on Unix, ICMP handle refused on Windows): an honest
		// capability report — never a red "failed".
		result.Status = ToolStatusUnsupported
		result.Error = "ICMP probing is unavailable in this context (privilege or policy); traceroute cannot run"

		result.Details = map[string]string{"reason": "privilege", "detail": boundedString(err.Error(), 160)}

		return
	}

	defer walker.Close()

	echoID := uint16(time.Now().UnixNano() & 0xffff)

	hops, done, reached, lastErr := walkTTLs(ctx, walker, ip, echoID)

	result.Measurement.Hops = hops
	result.Measurement.HopCount = len(hops)

	// Bounded route metadata (never fabricated): the resolved target
	// and the probe count accompany every terminal classification.
	baseDetails := map[string]string{
		"target": ip.String(),
		"probes": strconv.Itoa(done),
	}

	unreachableEvidence := lastErr != nil && strings.Contains(strings.ToLower(lastErr.Error()), "unreachable")

	switch {
	case ctx.Err() != nil && len(hops) == 0:
		result.Status = ToolStatusCancelled
		result.Error = ctx.Err().Error()
	case reached:
		result.Status = ToolStatusOK
		result.Details = baseDetails
		result.Details["completed"] = "yes"
	case unreachableEvidence:
		// The path explicitly reported the destination unreachable
		// (ICMP type 3 observed for our probe): a measured path
		// verdict — the diagnostic worked.
		result.Status = ToolStatusUnreachable
		result.Error = "destination explicitly reported unreachable: " + lastErr.Error()
		result.Details = baseDetails
		result.Details["completed"] = "no"
	case len(hops) > 0:
		// Hop answers stopped before the target (filtered or the
		// destination dropped off): keep the observed hops and report
		// the path verdict honestly — unreachable, not "failed".
		result.Status = ToolStatusUnreachable
		result.Error = fmt.Sprintf("path stops after %d observed hop(s); the target never answered (filtered or unreachable)", len(hops))
		result.Details = baseDetails
		result.Details["completed"] = "no"
	case lastErr != nil:
		setStatusFromError(result, lastErr)
	default:
		// No hops, no explicit verdict: genuinely undiagnosed.
		result.Status = ToolStatusFailed
		result.Error = "no ICMP answer was observed and no verdict could be formed"
		result.Details = baseDetails
		result.Details["completed"] = "no"
	}
}

// hopWalker is the platform ICMP abstraction behind the shared TTL
// walk loop: send one echo request at the given TTL and classify the
// answer (intermediate hop / target replied / error). Implementations
// live in traceroute_unix.go (raw ICMP socket) and
// traceroute_windows.go (native IP Helper ICMP API — user-mode, no
// elevation, no tracert.exe, no shell).
type hopWalker interface {
	// probe sends one echo request with the given TTL and waits the
	// bounded window. hopIP is non-empty when an intermediate router
	// (or the target) answered; replied is true only when the TARGET
	// itself answered the echo.
	probe(ctx context.Context, target net.IP, echoID uint16, ttl int) (hopIP string, replied bool, err error)

	// Close releases the underlying socket/handle.
	Close() error
}

// walkTTLs sends bounded echo probes with increasing TTL and collects
// hop addresses (from time-exceeded sources) until the target itself
// answers.
func walkTTLs(ctx context.Context, walker hopWalker, target net.IP, echoID uint16) (hops []string, probes int, reached bool, lastErr error) {
	const (
		maxHops   = 20
		probesPer = 2
	)

	for ttl := 1; ttl <= maxHops; ttl++ {
		if ctx.Err() != nil {
			return hops, probes, reached, ctx.Err()
		}

		var (
			hopIP     string
			gotReply  bool
			hopFailed bool
		)

		for i := 0; i < probesPer; i++ {
			if ctx.Err() != nil {
				return hops, probes, reached, ctx.Err()
			}

			probes++

			ip, replied, err := walker.probe(ctx, target, echoID, ttl)
			if err != nil {
				hopFailed = true
				lastErr = err

				// An explicit destination-unreachable verdict ends the
				// walk immediately: the path itself answered.
				if strings.Contains(strings.ToLower(err.Error()), "unreachable") {
					return hops, probes, false, err
				}

				continue
			}

			hopFailed = false
			lastErr = nil

			if ip != "" {
				hopIP = ip
			}

			if replied {
				gotReply = true

				break
			}
		}

		if gotReply {
			if hopIP != "" {
				hops = append(hops, hopIP)
			}

			return hops, probes, true, nil
		}

		if hopIP != "" {
			hops = append(hops, hopIP)
		}

		// No answer at all for this TTL after retries: the path is
		// being filtered. Stop honestly rather than looping to
		// maxHops blind.
		if hopIP == "" && !hopFailed {
			return hops, probes, false, nil
		}
	}

	return hops, probes, false, lastErr
}

// buildICMPEcho encodes an ICMP echo request (RFC 792).
func buildICMPEcho(id, seq uint16, payload []byte) []byte {
	packet := make([]byte, 8+len(payload))

	packet[0] = 8 // echo request
	packet[1] = 0

	packet[4] = byte(id >> 8)
	packet[5] = byte(id)
	packet[6] = byte(seq >> 8)
	packet[7] = byte(seq)

	copy(packet[8:], payload)

	// Checksum.
	sum := icmpChecksum(packet)
	packet[2] = byte(sum >> 8)
	packet[3] = byte(sum)

	return packet
}

func icmpChecksum(packet []byte) uint16 {
	var sum uint32

	for i := 0; i+1 < len(packet); i += 2 {
		sum += uint32(packet[i])<<8 | uint32(packet[i+1])
	}

	if len(packet)%2 == 1 {
		sum += uint32(packet[len(packet)-1]) << 8
	}

	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}

	return ^uint16(sum)
}

// parseICMPResponse classifies one received packet: an echo REPLY from
// the target, a TIME-EXCEEDED from an intermediate hop, or noise.
// Raw ip4:icmp sockets deliver the full IP header.
func parseICMPResponse(
	packet []byte,
	from net.Addr,
	target net.IP,
	echoID uint16,
) (hopIP string, targetReplied bool, err error) {
	// Skip the IPv4 header (20 bytes typical; respect IHL).
	if len(packet) < 28 {
		return "", false, fmt.Errorf("short packet")
	}

	ihl := int(packet[0]&0x0f) * 4
	if len(packet) < ihl+8 {
		return "", false, fmt.Errorf("short packet")
	}

	icmp := packet[ihl:]

	src := ""
	if from != nil {
		if addr, ok := from.(*net.IPAddr); ok {
			src = addr.IP.String()
		} else {
			src = from.String()
		}
	}

	switch icmp[0] {
	case 0: // echo reply — from the target?
		if len(icmp) < 8 {
			return "", false, fmt.Errorf("short reply")
		}

		replyID := uint16(icmp[4])<<8 | uint16(icmp[5])
		if replyID != echoID {
			return "", false, fmt.Errorf("foreign echo id")
		}

		if src != "" && target != nil && src != target.String() {
			// Spoofed/aliased reply; still our probe's answer.
			return src, true, nil
		}

		return src, true, nil
	case 11: // time exceeded — intermediate hop
		if len(icmp) < 8+20+8 { // needs the quoted original
			return "", false, fmt.Errorf("short time-exceeded")
		}

		quoted := icmp[8:] // quoted IPv4 header + 8 bytes
		if len(quoted) < 28 {
			return "", false, fmt.Errorf("short quote")
		}

		qihl := int(quoted[0]&0x0f) * 4
		if len(quoted) < qihl+8 {
			return "", false, fmt.Errorf("short quote")
		}

		quotedICMP := quoted[qihl:]
		origID := uint16(quotedICMP[4])<<8 | uint16(quotedICMP[5])
		if origID != echoID {
			return "", false, fmt.Errorf("foreign quoted id")
		}

		return src, false, nil
	case 3: // destination unreachable
		if len(icmp) >= 8+28 {
			quoted := icmp[8:]

			qihl := int(quoted[0]&0x0f) * 4
			if len(quoted) >= qihl+8 {
				origID := uint16(quoted[qihl+4])<<8 | uint16(quoted[qihl+5])
				if origID != echoID {
					return "", false, fmt.Errorf("foreign quoted id")
				}
			}
		}

		return "", false, fmt.Errorf("destination unreachable")
	default:
		return "", false, fmt.Errorf("icmp type %d", icmp[0])
	}
}

// ---- path MTU ---------------------------------------------------------

// runPathMTU climbs an unfragmented-UDP payload ladder against a
// public resolver: DNS queries whose QNAME is padded to a target
// size. The largest size that still elicits a response is the
// VERIFIED unfragmented path payload. The DNS method caps at ~300
// bytes (QNAME limit); probing the full 1500-byte MTU needs
// privileged raw sockets, which is reported honestly.
func (r *ToolRunner) runPathMTU(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "udp"

	host, port := targetHostPort(req, 53)
	resolver := net.JoinHostPort(host, strconv.Itoa(port))

	if ip := net.ParseIP(host); ip != nil && !r.Safety.privateTargetsAllowed(req.Tool) && IsPrivateIP(ip) {
		result.Status = ToolStatusInvalid
		result.Error = "private resolver blocked"

		return
	}

	// Ladder within the DNS QNAME limits (255-byte name → ~300-byte
	// query). Bounded: 6 rungs, 2s each.
	ladder := []int{64, 128, 192, 256, 300}

	verified := 0
	probes := 0

	for _, size := range ladder {
		if ctx.Err() != nil {
			result.Status = ToolStatusCancelled
			result.Error = ctx.Err().Error()

			return
		}

		probes++

		if mtuProbeOnce(ctx, resolver, size) {
			verified = size
		} else if verified > 0 {
			// First failure after successes: stop (path filters at
			// this size).
			break
		}
	}

	if verified == 0 {
		result.Status = ToolStatusFailed
		result.Error = "no probe size was verified — path may black-hole UDP or the resolver is unreachable"
		result.Measurement.Probes = probes
		result.Details = map[string]string{"resolver": resolver}

		return
	}

	result.Status = ToolStatusOK
	result.Measurement.MTUBytes = verified
	result.Measurement.Probes = probes
	result.Details = map[string]string{
		"resolver": resolver,
		"method":   "unfragmented DNS query ladder",
		"verified": strconv.Itoa(verified) + "+ bytes",
		"limit":    "DNS QNAME encoding caps the ladder at ~300 bytes; verifying the full 1500-byte MTU requires privileged raw-socket probing",
	}
}

// mtuProbeOnce sends one padded DNS query of the target size and
// reports whether a response arrived.
func mtuProbeOnce(ctx context.Context, resolver string, size int) bool {
	dialer := &net.Dialer{Timeout: 2 * time.Second}

	conn, err := dialer.DialContext(ctx, "udp", resolver)
	if err != nil {
		return false
	}

	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	_ = conn.SetDeadline(deadline)

	query := paddedDNSQuery(size)

	if _, err := conn.Write(query); err != nil {
		return false
	}

	buf := make([]byte, 1500)

	_ = conn.SetReadDeadline(deadline)

	_, err = conn.Read(buf)

	return err == nil
}

// paddedDNSQuery builds a DNS A query padded (via a long QNAME) to
// approximately the requested packet size.
func paddedDNSQuery(size int) []byte {
	base := len(buildDNSQuery("tools.freeiran.probe"))

	if size <= base {
		return buildDNSQuery("tools.freeiran.probe")
	}

	// Grow the QNAME with filler labels (each label ≤ 63 bytes,
	// total name ≤ 255 bytes).
	fill := size - base
	if fill > 200 {
		fill = 200
	}

	var labels []string

	remaining := fill
	for remaining > 0 {
		n := remaining
		if n > 63 {
			n = 63
		}

		labels = append(labels, strings.Repeat("p", n))
		remaining -= n
	}

	name := "tools.freeiran.probe"
	if len(labels) > 0 {
		name = strings.Join(labels, ".") + ".freeiran.probe"
	}

	return buildDNSQuery(name)
}

// ---- captive portal ---------------------------------------------------

// runCaptivePortal fetches known 204 endpoints and detects portal
// signatures: redirects to other hosts, or non-204 bodies.
func (r *ToolRunner) runCaptivePortal(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "https"

	targets := []string{
		"https://www.gstatic.com/generate_204",
		"http://connectivitycheck.gstatic.com/generate_204",
	}

	if t := strings.TrimSpace(req.Target); t != "" {
		targets = []string{t}
	}

	detected := false
	redirect := ""
	probes := 0

	for _, target := range targets {
		if ctx.Err() != nil {
			result.Status = ToolStatusCancelled
			result.Error = ctx.Err().Error()

			return
		}

		probes++

		client := r.boundedHTTPClient(ToolRequest{Tool: ToolCaptivePortal, Path: req.Path, Dial: req.Dial})

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			continue
		}

		request.Header.Set("User-Agent", "FreeIran-Tools/1.0")

		resp, err := client.Do(request)
		if err != nil {
			continue
		}

		finalURL := resp.Request.URL.String()

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			detected = true
			redirect = boundedString(finalURL, 200)

			_ = resp.Body.Close()

			break
		}

		if resp.StatusCode != 204 && resp.StatusCode != 200 {
			detected = true
			redirect = boundedString(finalURL, 200)

			_ = resp.Body.Close()

			break
		}

		// A 200 (instead of 204) with an HTML body is the classic
		// portal interception signature.
		if resp.StatusCode == 200 {
			head := make([]byte, 256)

			n, _ := resp.Body.Read(head)
			_ = resp.Body.Close()

			if isHTMLSnippet(head[:n]) {
				detected = true
				redirect = boundedString(finalURL, 200)

				break
			}
		} else {
			_ = resp.Body.Close()
		}
	}

	result.Measurement.CaptiveDetected = detected
	result.Measurement.CaptiveRedirect = redirect
	result.Measurement.Probes = probes

	if detected {
		result.Status = ToolStatusFailed
		result.Error = "captive portal detected"
		result.Details = map[string]string{"redirect": redirect}
	} else if probes == 0 {
		result.Status = ToolStatusFailed
		result.Error = "no probe completed"
	} else {
		result.Status = ToolStatusOK
		result.Details = map[string]string{"probes": strconv.Itoa(probes)}
	}
}

func isHTMLSnippet(b []byte) bool {
	s := strings.ToLower(string(b))

	return strings.Contains(s, "<html") || strings.Contains(s, "<!doctype") ||
		strings.Contains(s, "<title") || strings.Contains(s, "login")
}

// ---- public IP --------------------------------------------------------

// runPublicIP discovers the exit IP on the direct path and, when a
// tunnel is active, through it — comparing the two. It runs ONLY on
// explicit user action (the service layer never calls it at startup).
func (r *ToolRunner) runPublicIP(ctx context.Context, req ToolRequest, result *ToolResult) {
	result.Transport = "https"

	endpoints := []string{
		"https://api.ipify.org",
		"https://www.cloudflare.com/cdn-cgi/trace",
	}

	directIP := discoverExitIP(ctx, endpoints, nil)

	result.Measurement.ExitIPDirect = directIP

	if req.Path == PathTunneled && req.Dial != nil {
		tunnelIP := discoverExitIP(ctx, endpoints, req.Dial)
		result.Measurement.ExitIPTunnel = tunnelIP

		if directIP != "" && tunnelIP != "" {
			result.Measurement.ExitIPMatch = directIP == tunnelIP
		}
	}

	switch {
	case ctx.Err() != nil:
		result.Status = ToolStatusCancelled
		result.Error = ctx.Err().Error()
	case directIP == "" && result.Measurement.ExitIPTunnel == "":
		result.Status = ToolStatusFailed
		result.Error = "no identity endpoint answered"
	default:
		result.Status = ToolStatusOK
	}

	result.Details = map[string]string{
		"endpoints":   strings.Join(endpoints, ", "),
		"user_action": "public-ip lookups are never run automatically",
	}
}

// discoverExitIP queries the bounded identity endpoints through the
// supplied dialer (nil = direct) and parses the exit IP.
func discoverExitIP(ctx context.Context, endpoints []string, dial DialFunc) string {
	for _, endpoint := range endpoints {
		if ctx.Err() != nil {
			return ""
		}

		transport := &http.Transport{
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 6 * time.Second,
		}

		if dial != nil {
			transport.DialContext = dial
		}

		client := &http.Client{
			Timeout:   8 * time.Second,
			Transport: transport,
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}

		request.Header.Set("User-Agent", "FreeIran-Tools/1.0")

		resp, err := client.Do(request)
		if err != nil {
			client.CloseIdleConnections()

			continue
		}

		// Cap the body: identity endpoints return well under 1 KiB.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		client.CloseIdleConnections()

		if ip := parseExitIP(body); ip != "" {
			return ip
		}
	}

	return ""
}

// parseExitIP extracts an IP from ipify's plain body or Cloudflare's
// "ip=x.x.x.x" trace line.
func parseExitIP(body []byte) string {
	text := strings.TrimSpace(string(body))

	if ip := net.ParseIP(text); ip != nil {
		return ip.String()
	}

	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "ip=") {
			if ip := net.ParseIP(strings.TrimSpace(strings.TrimPrefix(line, "ip="))); ip != nil {
				return ip.String()
			}
		}
	}

	return ""
}

// ---- tunnel diagnostics ------------------------------------------------

// runTunnelDiagnostics reports the caller-supplied LIVE tunnel truth
// (never fabricated) and, when a tunnel is active, verifies the local
// endpoint accepts a SOCKS5 CONNECT round trip.
//
// v0.11.2 routing semantics (statuses corrected v0.12.1):
//
//   - Direct path selected (req.Path == PathDirect): the user did not
//     select a tunnel. There is nothing to diagnose — the honest
//     status is not_applicable with the "No active tunnel" wording,
//     never a red generic failure and never the legacy message that
//     read as a tunnel breakdown. The Transport field is cleared so
//     the UI cannot mistake this for a successful socks5 probe.
//   - Tunnel path selected (req.Path == PathTunneled) but the caller
//     could not supply an active TunnelSnapshot: the tool requires a
//     local endpoint that does not exist right now → not_configured
//     with the precise reason — never silently falling back to a
//     Direct probe. The Dial guard in Run() already intercepts this
//     case for tools that need a DialContext; tunnel_diagnostics
//     re-checks the snapshot so it cannot be confused by a stale
//     caller-supplied snapshot whose Active flag is false.
//   - Tunnel path selected, snapshot Active, but no local endpoint:
//     not_configured with the precise "no local endpoint" class.
//   - Tunnel path selected, snapshot Active, endpoint present: run a
//     real SOCKS5 CONNECT through the endpoint. The measured latency
//     is the authoritative round trip; the result is OK only when the
//     CONNECT succeeds.
//
// The result.Path field mirrors req.Path: when the user selected
// Direct, the result says Direct; when the user selected Tunneled,
// the result says Tunneled even when the tunnel is unavailable (the
// failure reason distinguishes them). The Transport field is set to
// "socks5" only when a real SOCKS5 CONNECT was attempted.
func (r *ToolRunner) runTunnelDiagnostics(req ToolRequest, result *ToolResult) {
	snap := req.Tunnel
	if snap == nil {
		snap = &TunnelSnapshot{}
	}

	result.Measurement.Tunnel = snap

	// Direct path: the user did not ask for a tunnel probe. There is
	// no tunnel to diagnose in this context — not_applicable, and the
	// message answers the question the user actually asked.
	if req.Path != PathTunneled {
		result.Transport = ""
		result.Status = ToolStatusNotApplicable
		result.Error = "No active tunnel on the Direct path — nothing to diagnose. Switch the route to Tunnel and run again."
		result.Details = map[string]string{
			"route":     "direct",
			"hint":      "Use a tunnel-path tool to verify the active tunnel",
			"user_path": string(req.Path),
		}
		return
	}

	// Tunnel path: tunnel availability is now meaningful. Transport is
	// socks5 because that is the protocol a successful run will use; if
	// the run cannot reach the SOCKS5 layer, Transport stays empty.
	result.Transport = "socks5"

	if !snap.Active {
		result.Status = ToolStatusNotConfigured
		result.Error = "Tunnel unavailable: no active tunnel"
		result.Details = map[string]string{
			"route":  "tunneled",
			"reason": "no active tunnel",
			"hint":   "Start a provider or connect a configuration, then re-run",
			"active": boolLabel(false),
		}
		return
	}

	if snap.Endpoint == "" {
		result.Status = ToolStatusNotConfigured
		result.Error = "Tunnel unavailable: active tunnel reports no local endpoint"
		result.Details = map[string]string{
			"route":    "tunneled",
			"reason":   "endpoint empty",
			"provider": snap.Provider,
			"active":   boolLabel(true),
		}
		return
	}

	result.Target = snap.Endpoint

	// Real measurement against the live local endpoint.
	dialer := socks5.Dialer{ProxyAddr: snap.Endpoint, Timeout: 8 * time.Second}

	started := time.Now()

	conn, err := dialer.Dial(context.Background(), "tcp", "www.gstatic.com:443")
	elapsed := time.Since(started)

	if err != nil {
		result.Status = ToolStatusFailed
		result.Error = "Tunnel unavailable: local endpoint failed SOCKS5 CONNECT: " + err.Error()
		result.Details = map[string]string{
			"route":    "tunneled",
			"reason":   "socks5 connect failed",
			"provider": snap.Provider,
			"endpoint": snap.Endpoint,
			"active":   boolLabel(true),
		}
		return
	}

	_ = conn.Close()

	result.Status = ToolStatusOK
	result.Measurement.Measured = true
	result.Measurement.LatencyMS = elapsed.Milliseconds()
	result.Measurement.SubMS = elapsed.Milliseconds() <= 0

	result.Details = map[string]string{
		"route":    "tunneled",
		"provider": snap.Provider,
		"endpoint": snap.Endpoint,
		"health":   boolLabel(snap.Healthy),
		"verified": "SOCKS5 CONNECT round trip completed through the active tunnel",
	}
}
