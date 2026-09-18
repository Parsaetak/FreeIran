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
//     blocked unless the policy explicitly allows local targets.
//   - Private-target permission is EXPLICIT and tool-scoped: only the
//     genuinely local-endpoint tools (socks5, http_connect — the
//     tools whose whole purpose is testing the user's own local
//     proxy) may target private addresses implicitly. Generic
//     diagnostics (tcp, tls, https, websocket, dns, udp, traceroute,
//     path_mtu) block loopback/private/link-local/reserved targets
//     by default; intentional local testing requires the explicit
//     AllowPrivateTargets capability.
//   - Redirects: capped at MaxRedirects (default 3) and every hop's
//     destination is re-validated against the same URL contract and
//     private-destination policy as the initial target.
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

// targetRejectedError marks a destination refused by the safety
// policy: a private/reserved literal, or a hostname whose answers all
// fall in blocked ranges (rebinding guard). The runner classifies it
// as invalid_target — a policy refusal — wherever it surfaces:
// validation, dial time, or a redirect hop (errors.As unwraps the
// url.Error the HTTP client wraps around CheckRedirect failures).
type targetRejectedError struct {
	reason string
}

func (e *targetRejectedError) Error() string { return e.reason }

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

// localTargetTools are the ONLY tools that may implicitly aim at
// the user's own private endpoints: socks5 and http_connect exist
// precisely to test a local proxy listener, and their tool defaults
// are local (127.0.0.1). Every other tool — including the generic
// tcp/tls/https/websocket/dns/udp diagnostics — requires the
// explicit Safety.AllowPrivateTargets capability for intentional
// local testing (tool DEFAULTS always stay public; see
// defaultToolTarget).
//
// This list was over-broad in v0.9.8.1 (tcp/tls/websocket/dns/https
// were implicitly private-target-safe), which silently bypassed the
// DNS-rebinding/private-address guard for generic HTTPS diagnostics
// — the root cause of the Windows CI failure
// TestSafeDialerResolveCheckPin: "localhost" resolved to 127.0.0.1
// and the https dialer connected because the tool was treated as
// local-endpoint-safe.
func localTargetTools(tool ToolID) bool {
	switch tool {
	case ToolSOCKS5, ToolHTTPConnect:
		return true
	default:
		return false
	}
}

// privateTargetsAllowed reports whether THIS request may aim at
// loopback/private/link-local destinations: either the policy
// explicitly opted in (AllowPrivateTargets — the capability the
// service layer sets when the user intentionally tests a local
// endpoint with a generic tool), or the tool is one of the two
// genuinely local-endpoint tools (see localTargetTools).
//
// This is the single policy decision shared by target validation,
// the dialer's rebinding guard and the tool-specific literal checks —
// one source of truth, no per-site improvisation.
func (s Safety) privateTargetsAllowed(tool ToolID) bool {
	return s.AllowPrivateTargets || localTargetTools(tool)
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
			return s.validateURLDestination(tool, req.Target,
				[]string{schemeHTTP, schemeHTTPS})
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
	if err := validateHostPort(s, tool, target); err != nil {
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

// validateHostPort validates a host[:port] target string: port
// bounds, then the destination policy for the host (private IP
// literals are blocked for generic tools at VALIDATION time — this
// covers the TUNNELED path, where the proxy resolves remotely and
// the safeDialer rebinding guard is not in the dial path).
func validateHostPort(s Safety, tool ToolID, target string) error {
	host, port, err := splitHostPortLoose(target)
	if err != nil {
		return err
	}

	if port != 0 && (port < 1 || port > 65535) {
		return fmt.Errorf("port %d out of range", port)
	}

	return s.checkHost(tool, host)
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
// destination policy. IP literals are checked directly; hostnames
// get syntactic validation here — the resolve-and-validate-every-
// answer step runs at DIAL time in safeDial (the rebinding guard:
// resolving twice would widen the TOCTOU window instead of closing
// it).
func (s Safety) checkHost(tool ToolID, host string) error {
	if host == "" {
		return fmt.Errorf("empty host")
	}

	allowPrivate := s.privateTargetsAllowed(tool)

	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && IsPrivateIP(ip) {
			return &targetRejectedError{
				fmt.Sprintf("private or reserved destination %s is blocked for this tool", host),
			}
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

// resolveIPAddrs resolves a hostname to its IP addresses through the
// system resolver. It is a package-level function variable (not an
// inline call) so the rebinding guard's every-answer validation can
// be exercised deterministically in tests with stub answers (mixed
// public/private, IPv6 loopback, rebinding flips) — the production
// path is unchanged.
var resolveIPAddrs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return (&net.Resolver{}).LookupIPAddr(ctx, host)
}

// netDial performs the raw TCP dial once the destination has been
// validated and pinned. Package-level function variable for the same
// reason as resolveIPAddrs: tests observe the PINNED address without
// a real connection (proving which validated IP the guard chose).
var netDial = (&net.Dialer{}).DialContext

// safeDial is the DIRECT dial path with the DNS-rebinding guard:
// resolve the hostname, validate EVERY answer against the private-
// range policy (unless local endpoints are allowed for this tool),
// then dial a VALIDATED address so the connection cannot be re-pinned
// between check and connect. IP-literal targets skip resolution.
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

	allowPrivate := d.safety.privateTargetsAllowed(d.tool)

	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && IsPrivateIP(ip) {
			return nil, &targetRejectedError{
				fmt.Sprintf("private destination %s blocked", host),
			}
		}

		return netDial(ctx, network, addr)
	}

	// Resolve → validate → pin.
	addrs, err := resolveIPAddrs(ctx, host)
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
		return nil, &targetRejectedError{
			fmt.Sprintf("hostname %s resolves only to blocked private addresses", host),
		}
	}

	pinned := net.JoinHostPort(valid[0].String(), strconv.Itoa(port))

	return netDial(ctx, network, pinned)
}

// validateRedirectTarget applies the destination-safety policy to
// one redirect hop: the same no-credentials URL contract and the same
// private-destination policy as the initial target. Redirected HTTPS
// (and captive-portal) requests therefore CANNOT be steered onto a
// loopback/private endpoint — including on the TUNNELED path, where
// the safeDialer rebinding guard is not in the dial path and the
// proxy resolves remotely.
func (s Safety) validateRedirectTarget(tool ToolID, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("no redirect URL")
	}

	if u.User != nil {
		return fmt.Errorf("credentials in redirect URLs are not permitted")
	}

	if u.Host == "" {
		return fmt.Errorf("redirect URL has no host")
	}

	return s.checkHost(tool, u.Hostname())
}
