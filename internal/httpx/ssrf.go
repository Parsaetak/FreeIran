package httpx

// SSRF-safe fetching (v0.9.7 §11). The discovery system fetches
// automatically discovered public URLs; without guardrails that fetch
// path could be turned into an arbitrary internal-network fetcher.
// This file implements the guard layer:
//
//   - scheme allowlist (http/https only — never file/gopher/etc.);
//   - port validation (only 80/443 by default; explicit odd ports are
//     rejected);
//   - hostname/IP validation: localhost names, private, link-local,
//     loopback, unique-local, multicast and reserved ranges are
//     rejected by default;
//   - DNS rebinding prevention: the transport-level dial Control hook
//     re-validates EVERY resolved IP right before the connection is
//     used, so a hostname that resolves public on lookup but private
//     on connect is rejected;
//   - bounded redirects: every hop is re-validated (scheme, port,
//     destination IP class) and the chain is capped;
//   - user-configured sources may pass AllowPrivate (documented
//     override path), which relaxes the IP-class checks ONLY —
//     scheme/port/redirect caps always apply.
//
// Nothing here executes content: no JavaScript, no browser, no HTML
// rendering — plain bounded GET requests.

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Parsaetak/FreeIran/internal/version"
)

// SSRF errors.
var (
	// ErrSSFRestricted reports a non-public destination.
	ErrSSFRestricted = errors.New("httpx: destination is not a public internet address")
	// ErrSSFScheme reports a non-http(s) URL scheme.
	ErrSSFScheme = errors.New("httpx: only http/https URLs are allowed")
	// ErrSSRFPort reports a disallowed destination port.
	ErrSSRFPort = errors.New("httpx: destination port is not allowed")
	// ErrSSRFRedirect reports a disallowed redirect target or chain.
	ErrSSRFRedirect = errors.New("httpx: redirect target is not allowed")
)

// SSRFOptions configure the SSRF guard.
type SSRFOptions struct {
	// AllowPrivate relaxes the private/loopback/reserved IP checks for
	// explicitly user-configured sources (documented override). Scheme,
	// port and redirect caps still apply.
	AllowPrivate bool

	// MaxRedirects bounds the redirect chain (<= 0 means 5).
	MaxRedirects int

	// AllowedHosts, when non-empty, restricts fetching to exactly these
	// hosts (case-insensitive). Used for provider pinning.
	AllowedHosts []string

	// AllowedPorts, when non-empty, replaces the default public set
	// (80/443, plus 8443/8080 under AllowPrivate). Test harnesses and
	// tightly-pinned deployments use this to widen/narrow the set.
	AllowedPorts []int
}

// privateHostnames lists host names that always resolve "somewhere
// local" regardless of what DNS claims.
var privateHostnames = map[string]struct{}{
	"localhost":                {},
	"localhost.localdomain":    {},
	"ip6-localhost":            {},
	"ip6-loopback":             {},
	"broadcasthost":            {},
	"localhost6":               {},
	"metadata.google.internal": {},
}

// isPrivateIP reports whether ip belongs to a non-public range:
// loopback, link-local, private (RFC1918/4193), unique-local,
// multicast, unspecified or reserved blocks.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	// IPv4-mapped IPv6 must be evaluated in 4-byte form.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	privateBlocks := []string{
		"127.0.0.0/8",     // IPv4 loopback
		"10.0.0.0/8",      // RFC1918 private
		"172.16.0.0/12",   // RFC1918 private
		"192.168.0.0/16",  // RFC1918 private
		"169.254.0.0/16",  // link-local (cloud metadata)
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // CGNAT shared address space
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmark testing
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved
		"::1/128",         // IPv6 loopback
		"fe80::/10",       // IPv6 link-local
		"fc00::/7",        // IPv6 unique-local
		"ff00::/8",        // IPv6 multicast
		"::/128",          // unspecified
	}

	for _, cidr := range privateBlocks {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		if block.Contains(ip) {
			return true
		}
	}

	return false
}

// ValidateGlobalURL checks scheme, host and port of a URL against the
// SSRF policy. DNS is intentionally NOT resolved here (the dial-time
// Control hook handles the resolved-IP check to defeat rebinding).
func ValidateGlobalURL(rawURL string, opts SSRFOptions) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSSFScheme, err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w (%q)", ErrSSFScheme, parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrSSFRestricted)
	}

	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if !opts.AllowPrivate && isPrivateIP(ip) {
			return fmt.Errorf("%w: %s", ErrSSFRestricted, host)
		}
	} else {
		lowered := strings.ToLower(strings.TrimSuffix(host, "."))

		if _, blocked := privateHostnames[lowered]; blocked && !opts.AllowPrivate {
			return fmt.Errorf("%w: %s", ErrSSFRestricted, host)
		}

		// .local / .internal names are never public internet.
		if !opts.AllowPrivate &&
			(strings.HasSuffix(lowered, ".local") || strings.HasSuffix(lowered, ".internal")) {
			return fmt.Errorf("%w: %s", ErrSSFRestricted, host)
		}
	}

	if len(opts.AllowedHosts) > 0 {
		allowed := false

		for _, candidate := range opts.AllowedHosts {
			if strings.EqualFold(candidate, host) {
				allowed = true

				break
			}
		}

		if !allowed {
			return fmt.Errorf("%w: host %s outside the allowed set", ErrSSFRestricted, host)
		}
	}

	return validatePort(parsed, opts)
}

// validatePort rejects suspicious destination ports.
func validatePort(parsed *url.URL, opts SSRFOptions) error {
	port := parsed.Port()
	if port == "" {
		return nil // scheme default (80/443)
	}

	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return fmt.Errorf("%w: %q", ErrSSRFPort, port)
	}

	// Explicit allowlist overrides the default public set entirely.
	if len(opts.AllowedPorts) > 0 {
		for _, allowed := range opts.AllowedPorts {
			if number == allowed {
				return nil
			}
		}

		return fmt.Errorf("%w: %d", ErrSSRFPort, number)
	}

	switch number {
	case 80, 443:
		return nil
	}

	// AllowPrivate sources keep a small escape hatch for common
	// self-hosted subscription ports; infrastructure ports stay closed.
	if opts.AllowPrivate {
		switch number {
		case 8443, 8080:
			return nil
		}
	}

	return fmt.Errorf("%w: %d", ErrSSRFPort, number)
}

// ssrfDialControl returns the dial Control hook that re-validates the
// RESOLVED address at connect time — the anti-DNS-rebinding backstop.
func ssrfDialControl(opts SSRFOptions) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: bad address %q", ErrSSFRestricted, address)
		}

		if !opts.AllowPrivate && isPrivateIP(net.ParseIP(host)) {
			return fmt.Errorf("%w: resolved to %s", ErrSSFRestricted, host)
		}

		return nil
	}
}

// NewSSRFClient builds a Client whose every request and every redirect
// hop is validated by the SSRF guard. A pre-flight hook rejects an
// invalid initial URL before any network activity; the transport-level
// Control hook aborts DNS-rebound connections.
func NewSSRFClient(p Policy, opts SSRFOptions) *Client {
	p = p.normalize()

	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = 5
	}

	dialer := &net.Dialer{
		Timeout:   p.DialTimeout,
		KeepAlive: 30 * time.Second,
		Control:   ssrfDialControl(opts),
	}

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   p.MaxIdleConnsPerHost,
		IdleConnTimeout:       p.IdleConnTimeout,
		TLSHandshakeTimeout:   p.TLSHandshakeTimeout,
		ResponseHeaderTimeout: p.ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}

	var seedBytes [16]byte

	if _, err := cryptorand.Read(seedBytes[:]); err != nil {
		binary.LittleEndian.PutUint64(seedBytes[:8], uint64(time.Now().UnixNano()))
	}

	seedA := binary.LittleEndian.Uint64(seedBytes[:8])
	seedB := binary.LittleEndian.Uint64(seedBytes[8:])

	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= opts.MaxRedirects {
				return fmt.Errorf("%w: more than %d redirects", ErrSSRFRedirect, opts.MaxRedirects)
			}

			if err := ValidateGlobalURL(req.URL.String(), opts); err != nil {
				return errors.Join(ErrSSRFRedirect, err)
			}

			return nil
		},
	}

	c := &Client{
		policy: p,
		http:   client,
		tr:     tr,
		rng:    rand.New(rand.NewPCG(seedA, seedB)),
	}

	// Pre-flight validation: every Get re-checks the URL before any
	// attempt is made (see Client.Get).
	c.preGet = func(_ context.Context, url string, _ GetOptions) error {
		return ValidateGlobalURL(url, opts)
	}

	// The SSRF fetcher identifies itself honestly to public sources.
	c.userAgent = version.UserAgent() + " (+discovery)"

	return c
}
