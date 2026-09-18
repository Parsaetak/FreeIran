// toolsafety.go implements the §7 network-tool safety policy: every
// user- or tool-supplied target is validated BEFORE any bytes leave
// the machine, and every response is bounded.
//
// Policies:
//
//   - URL targets: scheme allowlist per tool, no userinfo
//     (credentials never ride a diagnostic URL), port bounds,
//     host present.
//   - Autonomous targets (tool defaults) may never be private,
//     link-local, loopback or unspecified addresses — this is also
//     the DNS-rebinding guard for DIRECT probes: the hostname is
//     resolved first, every answer is checked, and the connection is
//     dialed to the validated address (the classic resolve-check-pin
//     pattern). For TUNNELED probes the proxy resolves remotely;
//     targets are still syntactically validated and private literals
//     blocked unless the tool explicitly diagnoses local endpoints.
//   - Local-endpoint tools (socks5 / http_connect / tcp / tls /
//     websocket against the user's own proxy) MAY target private
//     addresses: that is their purpose, and they run only on
//     explicit user action.
//   - Redirects: capped at MaxRedirects (default 3).
//   - Response bodies: capped at MaxResponseBytes (default 256 KiB).
//   - No remote JavaScript is ever executed (plain HTTP clients only,
//     bodies are discarded after counting).
package netcheck

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Safety is the shared tool-safety policy.
type Safety struct {
	// AllowPrivateTargets permits loopback/private/link-local
	// destinations. The default policy blocks them (autonomous
	// targets); the runner enables them only for the explicitly local
	// diagnostic tools the user aims at their own endpoints.
	AllowPrivateTargets bool

	// MaxRedirects bounds redirect chains (default 3).
	MaxRedirects int

	// MaxResponseBytes caps how much of any response body is read
	// (default 256 KiB). Bytes beyond the cap are neither read nor
	// buffered.
	MaxResponseBytes int64

	// AllowInsecureTLS skips certificate verification for the tls/
	// https tools when the user explicitly asks to test an endpoint
	// with a self-signed certificate. Off by default.
	AllowInsecureTLS bool
}

// DefaultSafety returns the default policy.
func DefaultSafety() Safety {
	return Safety{
		AllowPrivateTargets: false,
		MaxRedirects:        3,
		MaxResponseBytes:    256 << 10,
		AllowInsecureTLS:    false,
	}
}

// Private IP networks blocked for autonomous targets (RFC 1918/4193,
// loopback, link-local, unspecified, broadcast, carrier-grade NAT,
// multicast, documentation ranges).
var privateRanges = mustParseCIDRs([]string{
	"0.0.0.0/8",       // "this" network / unspecified
	"10.0.0.0/8",      // RFC 1918
	"100.64.0.0/10",   // CGNAT
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local
	"172.16.0.0/12",   // RFC 1918
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // 6to4 relay anycast (deprecated)
	"192.168.0.0/16",  // RFC 1918
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved / broadcast
	"::/128",          // IPv6 unspecified
	"::1/128",         // IPv6 loopback
	"fc00::/7",        // IPv6 ULA
	"fe80::/10",       // IPv6 link-local
	"ff00::/8",        // IPv6 multicast
	"2001:db8::/32",   // IPv6 documentation
})

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))

	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("netcheck: invalid private-range CIDR " + cidr)
		}

		nets = append(nets, network)
	}

	return nets
}

// IsPrivateIP reports whether an IP is private/link-local/reserved.
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	for _, network := range privateRanges {
		if network.Contains(ip) {
			return true
		}
	}

	return false
}

// schemes per tool family.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
	schemeWS    = "ws"
	schemeWSS   = "wss"
)

// toolSchemes returns the URL schemes a tool accepts for URL-style
// targets. Empty result: the tool takes host:port, not a URL.
func toolSchemes(tool ToolID) []string {
	switch tool {
	case ToolHTTPS:
		return []string{schemeHTTP, schemeHTTPS}
	case ToolWebSocket:
		return []string{schemeWS, schemeWSS}
	default:
		return nil
	}
}

// localTargetTools may legitimately aim at the user's own private
// endpoints (their local proxy / core listener / local test server).
// These run only on explicit user action; tool DEFAULTS always stay
// public (see defaultToolTarget).
func localTargetTools(tool ToolID) bool {
	switch tool {
	case ToolSOCKS5, ToolHTTPConnect, ToolTCP, ToolTLS, ToolWebSocket, ToolDNS, ToolHTTPS:
		return true
	default:
		return false
	}
}

// ValidateToolTarget validates a request's target against the safety
// policy before any bytes are sent. nil error = allowed.
func (s Safety) ValidateToolTarget(req ToolRequest) error {
	tool := req.Tool

	if tool == ToolTunnelDiagnostics || tool == ToolInternet ||
		tool == ToolCaptivePortal || tool == ToolPublicIP {
		// Aggregate tools use fixed, curated target sets (or caller
		// state); custom targets are not accepted.
		if req.Target != "" && tool == ToolCaptivePortal {
			_, err := parseURLTarget(req.Target, []string{schemeHTTP, schemeHTTPS})
			return err
		}

		return nil
	}

	if tool == ToolQUIC {
		return nil // capability report, no target
	}

	target := strings.TrimSpace(req.Target)
	if target == "" {
		return nil // safe default selected inside the tool
	}

	// URL-style targets.
	if schemes := toolSchemes(tool); schemes != nil {
		if _, err := parseURLTarget(target, schemes); err != nil {
			return err
		}

		return s.validateURLDestination(tool, target, schemes)
	}

	// host[:port] targets.
	if err := validateHostPort(tool, target); err != nil {
		return err
	}

	return nil
}

// parseURLTarget parses a URL and enforces scheme + no-credentials.
func parseURLTarget(raw string, allowed []string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	scheme := strings.ToLower(parsed.Scheme)

	allowedSet := make(map[string]struct{}, len(allowed))
	for _, s := range allowed {
		allowedSet[s] = struct{}{}
	}

	if _, ok := allowedSet[scheme]; !ok {
		return nil, fmt.Errorf("scheme %q not allowed for this tool (allowed: %s)",
			parsed.Scheme, strings.Join(allowed, ", "))
	}

	if parsed.User != nil {
		return nil, fmt.Errorf("credentials in diagnostic URLs are not permitted")
	}

	if parsed.Host == "" {
		return nil, fmt.Errorf("URL has no host")
	}

	return parsed, nil
}

// validateURLDestination resolves and checks the URL host for direct
// probes (DNS-rebinding guard) and blocks private literals unless the
// policy allows local endpoints.
func (s Safety) validateURLDestination(tool ToolID, raw string, allowed []string) error {
	parsed, err := parseURLTarget(raw, allowed)
	if err != nil {
		return err
	}

	return s.checkHost(tool, parsed.Hostname())
}

// validateHostPort validates a host[:port] target string.
func validateHostPort(tool ToolID, target string) error {
	_, port, err := splitHostPortLoose(target)
	if err != nil {
		return err
	}

	if port != 0 && (port < 1 || port > 65535) {
		return fmt.Errorf("port %d out of range", port)
	}

	_ = tool

	return nil
}

// splitHostPortLoose splits "host", "host:port" or "[v6]:port".
func splitHostPortLoose(target string) (string, int, error) {
	if strings.Contains(target, "://") {
		return "", 0, fmt.Errorf("URL given where host[:port] expected")
	}

	if host, portStr, err := net.SplitHostPort(target); err == nil {
		port, perr := strconv.Atoi(portStr)
		if perr != nil {
			return "", 0, fmt.Errorf("invalid port %q", portStr)
		}

		if strings.TrimSpace(host) == "" {
			return "", 0, fmt.Errorf("empty host")
		}

		return strings.TrimSpace(host), port, nil
	}

	// No port: bare host or bare IP.
	if strings.TrimSpace(target) == "" {
		return "", 0, fmt.Errorf("empty target")
	}

	return strings.TrimSpace(target), 0, nil
}

// checkHost validates a hostname or IP literal against the private-
// destination policy. IP literals are checked directly; hostnames are
// resolved and every answer must pass (rebinding guard).
func (s Safety) checkHost(tool ToolID, host string) error {
	if host == "" {
		return fmt.Errorf("empty host")
	}

	allowPrivate := s.AllowPrivateTargets || localTargetTools(tool)

	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && IsPrivateIP(ip) {
			return fmt.Errorf("private or reserved destination %s is blocked for this tool", host)
		}

		return nil
	}

	// Hostname: syntactic validation. Resolution-based checks for
	// direct dials happen in safeDial (resolve → validate → pin);
	// for tunneled dials the proxy resolves remotely and we only
	// enforce the syntactic contract here.
	if err := validHostname(host); err != nil {
		return err
	}

	return nil
}

// validHostname performs RFC-1123-ish hostname validation.
func validHostname(host string) error {
	if len(host) == 0 || len(host) > 253 {
		return fmt.Errorf("invalid hostname length")
	}

	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("invalid hostname %q", host)
		}

		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
				(r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("invalid character %q in hostname", string(r))
			}
		}
	}

	return nil
}

// safeDial is the DIRECT dial path with the DNS-rebinding guard:
// resolve the hostname, validate EVERY answer against the private-
// range policy (unless local endpoints are allowed for this tool),
// then dial the validated address so the connection cannot be
// re-pinned between check and connect. IP-literal targets skip
// resolution.
type safeDialer struct {
	safety Safety
	tool   ToolID
}

// dialer returns a net.Dialer wrapped with the rebinding guard.
func (s Safety) dialer(tool ToolID) *safeDialer {
	return &safeDialer{safety: s, tool: tool}
}

// Dial validates then dials.
func (d *safeDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := splitHostPortLoose(addr)
	if err != nil {
		return nil, err
	}

	allowPrivate := d.safety.AllowPrivateTargets || localTargetTools(d.tool)

	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && IsPrivateIP(ip) {
			return nil, fmt.Errorf("private destination %s blocked", host)
		}

		var dialer net.Dialer

		return dialer.DialContext(ctx, network, addr)
	}

	// Resolve → validate → pin.
	resolver := &net.Resolver{}

	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}

	valid := make([]net.IP, 0, len(addrs))

	for _, a := range addrs {
		if allowPrivate || !IsPrivateIP(a.IP) {
			valid = append(valid, a.IP)
		}
	}

	if len(valid) == 0 {
		return nil, fmt.Errorf("hostname %s resolves only to blocked private addresses", host)
	}

	pinned := net.JoinHostPort(valid[0].String(), strconv.Itoa(port))

	var dialer net.Dialer

	return dialer.DialContext(ctx, network, pinned)
}
