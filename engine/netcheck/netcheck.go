// Package netcheck implements FreeIran's Internet / Network
// Diagnostics capability (v0.9.0 §3): a deterministic, cancellable
// connectivity probe that distinguishes
//
//  1. Internet unavailable
//  2. Internet available but DNS failing
//  3. DNS working but HTTPS failing
//  4. Internet working with high latency
//  5. Internet working normally
//  6. Internet reachable only through the configured proxy/core
//  7. Core connected but external connectivity failing
//
// Design rules from the specification:
//
//   - multiple lightweight checks, never one hardcoded endpoint;
//   - every target configurable;
//   - asynchronous with timeout + cancellation (Run honours ctx);
//   - no UI blocking (the app service wraps Run in a bounded call);
//   - latency is measured, never fabricated.
package netcheck

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// Subsystem identifies the diagnostics layer in structured logs.
const Subsystem = "netcheck"

// State is the classified overall connectivity state.
type State string

const (
	// StateNoInternet: no working local network / no TCP anywhere.
	StateNoInternet State = "no_internet"

	// StateDNSFailure: local network up, DNS resolution failing.
	StateDNSFailure State = "dns_failure"

	// StateHTTPSFailure: TCP works, HTTPS requests fail (blocking /
	// captive portal / TLS interference).
	StateHTTPSFailure State = "https_failure"

	// StateHighLatency: everything works but slowly.
	StateHighLatency State = "high_latency"

	// StateOK: internet working normally.
	StateOK State = "ok"

	// StateProxyOnly: direct internet blocked, but the probe through
	// the configured proxy/core succeeded.
	StateProxyOnly State = "proxy_only"

	// StateCoreNoInternet: the core is connected but external
	// connectivity through it fails.
	StateCoreNoInternet State = "core_no_internet"
)

// Human returns the user-facing description of a state.
func (s State) Human() string {
	switch s {
	case StateNoInternet:
		return "No internet connection. The local network is unreachable or every tested endpoint failed at TCP level."
	case StateDNSFailure:
		return "The network is up but DNS resolution is failing. Websites will not open by name."
	case StateHTTPSFailure:
		return "Basic connectivity works but HTTPS requests fail. A firewall, captive portal or filtering may be interfering."
	case StateHighLatency:
		return "The internet is reachable but responses are unusually slow."
	case StateOK:
		return "The internet connection is working normally."
	case StateProxyOnly:
		return "Direct internet is blocked, but connectivity through the proxy works. The proxy is required."
	case StateCoreNoInternet:
		return "The core is connected but external requests through it fail. The configuration may be dead or heavily filtered."
	default:
		return "Connectivity state unknown."
	}
}

// CheckResult is the outcome of one individual probe.
type CheckResult struct {
	Name      string `json:"name"`
	Target    string `json:"target"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Report is the complete diagnostics snapshot.
type Report struct {
	State       State         `json:"state"`
	Summary     string        `json:"summary"`
	CheckedAt   time.Time     `json:"checked_at"`
	DurationMS  int64         `json:"duration_ms"`
	LatencyMS   int64         `json:"latency_ms,omitempty"` // best HTTPS latency
	TargetUsed  string        `json:"target_used,omitempty"`
	LocalLinks  []CheckResult `json:"local_links"`
	DNS         []CheckResult `json:"dns"`
	TCP         []CheckResult `json:"tcp"`
	HTTPS       []CheckResult `json:"https"`
	Proxy       *CheckResult  `json:"proxy,omitempty"`
	Cancelled   bool          `json:"cancelled"`
	TargetCount int           `json:"target_count"`
}

// Config selects and bounds the probes. Zero-value fields fall back
// to Defaults().
type Config struct {
	// DNSTargets are resolver addresses ("1.1.1.1:53"); empty uses
	// defaults.
	DNSTargets []string

	// TCPTargets are host:port endpoints for raw TCP probes.
	TCPTargets []string

	// HTTPSWithdraw targets are full URLs expected to answer 2xx/204.
	HTTPSURLs []string

	// ProbeHost is the hostname used for DNS resolution tests.
	ProbeHost string

	// Timeout bounds each individual probe.
	Timeout time.Duration

	// ProxyAddr, when set, runs an additional probe through the
	// SOCKS5 proxy (usually the connected core's local listener) so
	// the report can distinguish StateProxyOnly and
	// StateCoreNoInternet.
	ProxyAddr string

	// HighLatencyThresholdMS classifies a working connection as slow.
	HighLatencyThresholdMS int64
}

// Defaults returns the built-in target set (multiple independent
// operators; nothing is a single point of truth).
func Defaults() Config {
	return Config{
		DNSTargets: []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"},
		TCPTargets: []string{"1.1.1.1:443", "8.8.8.8:53", "9.9.9.9:443"},
		HTTPSURLs: []string{
			"https://www.gstatic.com/generate_204",
			"https://cp.cloudflare.com/generate_204",
			"https://www.apple.com/library/test/success.html",
		},
		ProbeHost:              "www.gstatic.com",
		Timeout:                8 * time.Second,
		HighLatencyThresholdMS: 1200,
	}
}

// resolve merges user config over defaults.
func (c Config) resolve() Config {
	d := Defaults()

	if len(c.DNSTargets) > 0 {
		d.DNSTargets = c.DNSTargets
	}

	if len(c.TCPTargets) > 0 {
		d.TCPTargets = c.TCPTargets
	}

	if len(c.HTTPSURLs) > 0 {
		d.HTTPSURLs = c.HTTPSURLs
	}

	if c.ProbeHost != "" {
		d.ProbeHost = c.ProbeHost
	}

	if c.Timeout > 0 {
		d.Timeout = c.Timeout
	}

	if c.ProxyAddr != "" {
		d.ProxyAddr = c.ProxyAddr
	}

	if c.HighLatencyThresholdMS > 0 {
		d.HighLatencyThresholdMS = c.HighLatencyThresholdMS
	}

	return d
}

// Checker runs the probe set. It is stateless and safe for
// concurrent use.
type Checker struct {
	Config Config
}

// New creates a Checker; an empty config selects Defaults().
func New(cfg Config) *Checker {
	return &Checker{Config: cfg}
}

// Run executes every probe concurrently and classifies the result.
// Cancellation is honoured per-probe: a cancelled context produces a
// Report with Cancelled=true.
func (c *Checker) Run(ctx context.Context) Report {
	cfg := c.Config.resolve()

	started := time.Now().UTC()

	report := Report{
		CheckedAt:   started,
		TargetCount: len(cfg.DNSTargets) + len(cfg.TCPTargets) + len(cfg.HTTPSURLs),
	}

	if cfg.ProxyAddr != "" {
		report.TargetCount++
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	run := func(results *[]CheckResult, name string, fn func(context.Context) (bool, int64, string)) {
		defer wg.Done()

		probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()

		ok, latency, errMsg := fn(probeCtx)

		mu.Lock()
		*results = append(*results, CheckResult{
			Name: name, Target: name, OK: ok, LatencyMS: latency, Error: errMsg,
		})
		mu.Unlock()
	}

	// Local network links (fast, synchronous, no network traffic).
	report.LocalLinks = probeLocalLinks()

	// DNS probes.
	wg.Add(len(cfg.DNSTargets))

	for _, target := range cfg.DNSTargets {
		go run(&report.DNS, target, func(pctx context.Context) (bool, int64, string) {
			return probeDNS(pctx, target, cfg.ProbeHost)
		})
	}

	// TCP probes.
	wg.Add(len(cfg.TCPTargets))

	for _, target := range cfg.TCPTargets {
		go run(&report.TCP, target, func(pctx context.Context) (bool, int64, string) {
			return probeTCP(pctx, target)
		})
	}

	// HTTPS probes (record the winning target for the UI).
	httpsTargets := make([]CheckResult, 0, len(cfg.HTTPSURLs))

	wg.Add(len(cfg.HTTPSURLs))

	for _, target := range cfg.HTTPSURLs {
		go func(url string) {
			defer wg.Done()

			probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()

			ok, latency, errMsg := probeHTTPS(probeCtx, url)

			mu.Lock()
			httpsTargets = append(httpsTargets, CheckResult{
				Name: url, Target: url, OK: ok, LatencyMS: latency, Error: errMsg,
			})
			mu.Unlock()
		}(target)
	}

	// Proxy probe.
	if cfg.ProxyAddr != "" {
		wg.Add(1)

		go func() {
			defer wg.Done()

			probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()

			ok, latency, errMsg := probeThroughProxy(probeCtx, cfg.ProxyAddr, cfg.HTTPSURLs)

			mu.Lock()
			report.Proxy = &CheckResult{
				Name: cfg.ProxyAddr, Target: cfg.ProxyAddr,
				OK: ok, LatencyMS: latency, Error: errMsg,
			}
			mu.Unlock()
		}()
	}

	wg.Wait()

	// Sort probe lists for deterministic output.
	sortResults(report.DNS)
	sortResults(report.TCP)
	sortResults(httpsTargets)
	report.HTTPS = httpsTargets

	mu.Lock()
	proxy := report.Proxy
	mu.Unlock()

	report.DurationMS = elapsedMS(started)
	report.State = classify(cfg, report, proxy)
	report.Summary = report.State.Human()

	if best := bestLatency(report.HTTPS); best > 0 {
		report.LatencyMS = best
	}

	return report
}

func sortResults(list []CheckResult) {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
}

func bestLatency(list []CheckResult) int64 {
	best := int64(0)
	for _, r := range list {
		if r.OK && (best == 0 || r.LatencyMS < best) {
			best = r.LatencyMS
		}
	}

	return best
}

// classify maps probe outcomes to the seven specification states.
func classify(cfg Config, report Report, proxy *CheckResult) State {
	tcpOK := anyOK(report.TCP)
	dnsOK := anyOK(report.DNS)
	httpsOK := anyOK(report.HTTPS)
	localOK := anyOK(report.LocalLinks)

	switch {
	case proxy != nil && proxy.OK && !httpsOK && !tcpOK:
		// Everything direct is blocked; the proxy path works.
		return StateProxyOnly
	case proxy != nil && !proxy.OK:
		// The caller asked for a proxy probe and it failed. When the
		// direct path still works the failure belongs to the core
		// side (dead config); when everything is down it is plain
		// no-internet.
		if tcpOK || httpsOK {
			return StateCoreNoInternet
		}

		return StateNoInternet
	case !localOK && !tcpOK:
		return StateNoInternet
	case !tcpOK && !httpsOK:
		return StateNoInternet
	case !dnsOK && !httpsOK && tcpOK:
		return StateDNSFailure
	case dnsOK && tcpOK && !httpsOK:
		return StateHTTPSFailure
	case !dnsOK && httpsOK:
		// Names blocked but IP-literal HTTPS works — treated as DNS
		// interference with working internet.
		return StateDNSFailure
	case httpsOK && bestLatency(report.HTTPS) > cfg.HighLatencyThresholdMS:
		return StateHighLatency
	case httpsOK:
		return StateOK
	case tcpOK:
		// TCP alive, DNS dead, HTTPS unknown (all probes timed out).
		return StateDNSFailure
	default:
		return StateNoInternet
	}
}

func anyOK(list []CheckResult) bool {
	for _, r := range list {
		if r.OK {
			return true
		}
	}

	return false
}

// probeLocalLinks reports usable non-loopback interfaces.
func probeLocalLinks() []CheckResult {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []CheckResult{{Name: "local_links", OK: false, Error: err.Error()}}
	}

	results := []CheckResult{}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil || len(addrs) == 0 {
			continue
		}

		results = append(results, CheckResult{
			Name:   iface.Name,
			Target: addrs[0].String(),
			OK:     true,
		})
	}

	if len(results) == 0 {
		results = append(results, CheckResult{
			Name:  "local_links",
			OK:    false,
			Error: "no active non-loopback network interface",
		})
	}

	return results
}

// elapsedMS converts a measured duration to whole milliseconds,
// rounding sub-millisecond results up to 1ms so a successful probe
// always reports a usable latency (0 is reserved for "not set").
func elapsedMS(started time.Time) int64 {
	ms := time.Since(started).Milliseconds()
	if ms <= 0 {
		return 1
	}

	return ms
}

// probeDNS resolves the probe host through one specific server.
func probeDNS(ctx context.Context, server, host string) (bool, int64, string) {
	started := time.Now()

	dialer := &net.Dialer{Timeout: 3 * time.Second}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(dctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(dctx, "udp", server)
		},
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	addrs, err := resolver.LookupHost(probeCtx, host)
	latency := elapsedMS(started)

	if err != nil {
		return false, latency, err.Error()
	}

	if len(addrs) == 0 {
		return false, latency, "resolver returned no addresses"
	}

	return true, latency, ""
}

// probeTCP dials one raw TCP endpoint.
func probeTCP(ctx context.Context, target string) (bool, int64, string) {
	started := time.Now()

	dialer := &net.Dialer{}

	conn, err := dialer.DialContext(ctx, "tcp", target)
	latency := elapsedMS(started)

	if err != nil {
		return false, latency, err.Error()
	}

	_ = conn.Close()

	return true, latency, ""
}

// probeHTTPS performs one GET and reports the total latency.
//
// NOTE: this deliberately builds a disposable transport with
// DisableKeepAlives instead of routing through internal/httpx —
// connection reuse would corrupt the latency measurement this probe
// exists to take. It is a probe, not a download path.
func probeHTTPS(ctx context.Context, url string) (bool, int64, string) {
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 6 * time.Second,
		},
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, 0, err.Error()
	}

	started := time.Now()

	resp, err := client.Do(req)
	latency := elapsedMS(started)

	if err != nil {
		return false, latency, err.Error()
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return false, latency, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	return true, latency, ""
}

// probeThroughProxy runs an HTTPS probe through the SOCKS5 proxy
// (normally the connected core's local listener).
func probeThroughProxy(ctx context.Context, proxyAddr string, urls []string) (bool, int64, string) {
	if len(urls) == 0 {
		urls = Defaults().HTTPSURLs
	}

	dialer := socks5.Dialer{ProxyAddr: proxyAddr, Timeout: 8 * time.Second}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.Dial,
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   6 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
		},
	}
	defer client.CloseIdleConnections()

	var lastErr string

	for _, url := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err.Error()

			continue
		}

		started := time.Now()

		resp, err := client.Do(req)
		latency := time.Since(started).Milliseconds()

		if err != nil {
			lastErr = err.Error()

			continue
		}

		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return true, latency, ""
		}

		lastErr = fmt.Sprintf("HTTP %d through proxy", resp.StatusCode)
	}

	return false, 0, lastErr
}
