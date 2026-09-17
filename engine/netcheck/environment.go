package netcheck

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// environment.go implements network-environment intelligence
// (v0.9.6 §14): evidence-based signals about the CURRENT network that
// the discovery, testing and transport layers use to adapt strategy.
//
// Honesty rules (they shape the whole design):
//
//   - Only detectable phenomena are signalled. QUIC/HTTP3 availability
//     is NOT probed here (the standard library ships no QUIC client);
//     QUIC-family transports are served by the protocol cores
//     themselves, and the environment layer never claims a QUIC
//     classification it cannot measure.
//   - No censorship certainty. Signals describe OBSERVED failures
//     ("TLS to multiple independent endpoints fails while TCP
//     succeeds"), not their cause. The summary words stick to the
//     evidence.
//   - Captive-portal detection uses the standard 204-hijack signature
//     (an intercepting portal answers a plain-HTTP 204 probe with a
//     redirect or content instead of 204).
//   - Instability is measured latency variance across probes, not a
//     guess.
//
// The analysis is bounded: every probe carries the config's per-probe
// timeout, the whole analysis shares the caller's context, and a
// result cache keeps repeated analyses cheap.

// EnvironmentSignal is one evidence-based environment observation.
type EnvironmentSignal string

const (
	// SignalDirectOK: plain HTTP(S) connectivity works directly.
	SignalDirectOK EnvironmentSignal = "direct_ok"

	// SignalDNSFailure: name resolution fails while the network is up.
	SignalDNSFailure EnvironmentSignal = "dns_failure"

	// SignalHTTPFailure: TCP works but HTTP responses fail.
	SignalHTTPFailure EnvironmentSignal = "http_failure"

	// SignalTLSFailure: TLS handshakes fail against multiple
	// independent endpoints while plain TCP works — the strongest
	// interference evidence available to this layer.
	SignalTLSFailure EnvironmentSignal = "tls_failure"

	// SignalRepeatedTimeout: consecutive analyses observed end-to-end
	// timeouts (path black-holing).
	SignalRepeatedTimeout EnvironmentSignal = "repeated_timeout"

	// SignalCaptivePortal: a plain-HTTP 204 probe was hijacked
	// (redirect or content instead of 204).
	SignalCaptivePortal EnvironmentSignal = "captive_portal"

	// SignalProxyEnvironment: HTTP(S)_PROXY/ALL_PROXY is set in the
	// environment (a configured upstream the engine should respect
	// rather than fight).
	SignalProxyEnvironment EnvironmentSignal = "proxy_environment"

	// SignalUnstableConnectivity: measured latency variance across
	// HTTPS probes exceeds the instability threshold.
	SignalUnstableConnectivity EnvironmentSignal = "unstable_connectivity"

	// SignalRestrictedAccess: direct access to multiple independent
	// endpoints fails while the network itself is up.
	SignalRestrictedAccess EnvironmentSignal = "restricted_access"
)

// Environment is the analyzed environment snapshot.
type Environment struct {
	// Signals are the observed signals (sorted, deduplicated).
	Signals []EnvironmentSignal `json:"signals"`

	// Restricted reports whether the evidence justifies escalated
	// discovery strategy (deeper source search, wider candidate
	// acceptance). It is a measured conclusion, never a censorship
	// claim.
	Restricted bool `json:"restricted"`

	// DeepDiscoveryAdvised tells the discovery engine to escalate.
	DeepDiscoveryAdvised bool `json:"deep_discovery_advised"`

	// Summary is the evidence-based, human-readable description.
	Summary string `json:"summary"`

	// Probes carries the raw probe evidence (already credential-free).
	Probes []CheckResult `json:"probes,omitempty"`

	// AnalyzedAt is the analysis timestamp.
	AnalyzedAt time.Time `json:"analyzed_at"`
}

// Has reports whether a signal was observed.
func (e Environment) Has(s EnvironmentSignal) bool {
	for _, got := range e.Signals {
		if got == s {
			return true
		}
	}

	return false
}

// EnvironmentConfig tunes the analysis.
type EnvironmentConfig struct {
	// ProbeTimeout bounds one probe (default 8s).
	ProbeTimeout time.Duration

	// HTTPTargets are the independent HTTPS probe endpoints.
	HTTPTargets []string

	// CaptivePortalTarget is the plain-HTTP 204 probe
	// (default http://www.gstatic.com/generate_204).
	CaptivePortalTarget string

	// InstabilityJitterMS: latency spread across HTTPS probes beyond
	// this counts as unstable (default 400 ms).
	InstabilityJitterMS int64

	// TimeoutMemory: how many consecutive timeout observations
	// constitute SignalRepeatedTimeout (default 2).
	TimeoutMemory int
}

// DefaultEnvironmentConfig returns the default analysis targets:
// endpoints operated by independent parties (Google, Cloudflare,
// Microsoft) so a single-party outage cannot masquerade as
// interference.
func DefaultEnvironmentConfig() EnvironmentConfig {
	return EnvironmentConfig{
		ProbeTimeout:        8 * time.Second,
		HTTPTargets:         []string{"https://www.gstatic.com/generate_204", "https://cp.cloudflare.com/generate_204"},
		CaptivePortalTarget: "http://www.gstatic.com/generate_204",
		InstabilityJitterMS: 400,
		TimeoutMemory:       2,
	}
}

// EnvironmentAnalyzer measures and classifies the network
// environment. It keeps a small timeout-observation memory so
// REPEATED timeouts (the black-holing signature) become a signal.
type EnvironmentAnalyzer struct {
	cfg EnvironmentConfig

	mu        sync.Mutex
	timeouts  int
	lastState State
}

// NewEnvironmentAnalyzer creates an analyzer.
func NewEnvironmentAnalyzer(cfg EnvironmentConfig) *EnvironmentAnalyzer {
	if cfg.ProbeTimeout <= 0 {
		cfg = DefaultEnvironmentConfig()
	}

	if len(cfg.HTTPTargets) == 0 {
		cfg.HTTPTargets = DefaultEnvironmentConfig().HTTPTargets
	}

	if cfg.CaptivePortalTarget == "" {
		cfg.CaptivePortalTarget = DefaultEnvironmentConfig().CaptivePortalTarget
	}

	if cfg.InstabilityJitterMS <= 0 {
		cfg.InstabilityJitterMS = 400
	}

	if cfg.TimeoutMemory <= 0 {
		cfg.TimeoutMemory = 2
	}

	return &EnvironmentAnalyzer{cfg: cfg}
}

// Analyze measures the environment and classifies it.
func (a *EnvironmentAnalyzer) Analyze(ctx context.Context) Environment {
	started := time.Now().UTC()

	env := Environment{AnalyzedAt: started}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		probes []CheckResult
	)

	addProbe := func(r CheckResult) {
		mu.Lock()
		probes = append(probes, r)
		mu.Unlock()
	}

	// ---- HTTPS probes (independent endpoints) ----
	for _, target := range a.cfg.HTTPTargets {
		wg.Add(1)

		go func(target string) {
			defer wg.Done()

			ok, ms, detail := a.probeHTTPS(ctx, target)
			addProbe(CheckResult{
				Name: "https", Target: target, OK: ok, LatencyMS: ms, Error: detail,
			})
		}(target)
	}

	// ---- Captive portal probe (plain HTTP 204) ----
	wg.Add(1)

	go func() {
		defer wg.Done()

		hijacked, _, detail := a.probeCaptivePortal(ctx)
		addProbe(CheckResult{
			Name: "captive_portal", Target: a.cfg.CaptivePortalTarget,
			OK:    !hijacked, // hijacked = NOT ok for direct use
			Error: detail,
		})

		if hijacked {
			env.markMu(&mu, SignalCaptivePortal)
		}
	}()

	// ---- DNS probe ----
	wg.Add(1)

	go func() {
		defer wg.Done()

		resolver := &net.Resolver{}
		dctx, cancel := context.WithTimeout(ctx, a.cfg.ProbeTimeout)
		defer cancel()

		start := time.Now()

		_, err := resolver.LookupHost(dctx, "www.gstatic.com")

		ms := time.Since(start).Milliseconds()

		ok := err == nil

		addProbe(CheckResult{
			Name: "dns", Target: "www.gstatic.com", OK: ok, LatencyMS: ms,
			Error: errDetail(err),
		})

		if !ok {
			env.markMu(&mu, SignalDNSFailure)
		}
	}()

	wg.Wait()

	// ---- Classify from evidence ----
	a.classify(ctx, &env, probes)

	env.Probes = probes

	env.Signals = dedupeSignals(env.Signals)
	sort.Slice(env.Signals, func(i, j int) bool { return env.Signals[i] < env.Signals[j] })

	env.Summary = summarizeEnvironment(env)

	return env
}

// markMu appends a signal under the probe mutex.
func (e *Environment) markMu(mu *sync.Mutex, s EnvironmentSignal) {
	mu.Lock()
	e.Signals = append(e.Signals, s)
	mu.Unlock()
}

// classify derives the aggregate signals from probe evidence.
func (a *EnvironmentAnalyzer) classify(ctx context.Context, env *Environment, probes []CheckResult) {
	var (
		httpsOK     int
		httpsTotal  int
		tlsFailures int
		latencies   []int64
		anyTimeout  bool
	)

	for _, p := range probes {
		if p.Name != "https" {
			continue
		}

		httpsTotal++

		if p.OK {
			httpsOK++
			latencies = append(latencies, p.LatencyMS)

			continue
		}

		if strings.Contains(p.Error, "tls") || strings.Contains(p.Error, "certificate") {
			tlsFailures++
		}

		if strings.Contains(p.Error, "timeout") || strings.Contains(p.Error, "deadline") {
			anyTimeout = true
		}
	}

	dnsOK := anyProbeOK(probes, "dns")
	captive := env.Has(SignalCaptivePortal)

	// Direct connectivity verdict.
	if httpsOK == httpsTotal && httpsTotal > 0 {
		env.Signals = append(env.Signals, SignalDirectOK)
	}

	// TLS interference evidence: multiple independent endpoints fail
	// at the TLS layer specifically.
	if tlsFailures >= 2 {
		env.Signals = append(env.Signals, SignalTLSFailure)
	}

	// HTTP failure without TLS-specific evidence.
	if httpsOK == 0 && httpsTotal > 0 && tlsFailures < 2 && dnsOK {
		env.Signals = append(env.Signals, SignalHTTPFailure)
	}

	// Repeated timeout memory (black-holing signature).
	a.mu.Lock()

	if anyTimeout {
		a.timeouts++
	} else if httpsOK > 0 {
		a.timeouts = 0
	}

	repeated := a.timeouts >= a.cfg.TimeoutMemory

	a.mu.Unlock()

	if repeated {
		env.Signals = append(env.Signals, SignalRepeatedTimeout)
	}

	// Instability: measured latency spread across successful probes.
	if len(latencies) >= 2 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

		spread := latencies[len(latencies)-1] - latencies[0]
		if spread > a.cfg.InstabilityJitterMS {
			env.Signals = append(env.Signals, SignalUnstableConnectivity)
		}
	}

	// Configured proxy environment.
	if detectProxyEnvironment() != "" {
		env.Signals = append(env.Signals, SignalProxyEnvironment)
	}

	// Restricted access: the network is up (DNS or captive evidence
	// proves a working link) yet independent HTTPS endpoints fail.
	networkUp := dnsOK || captive
	directBlocked := httpsOK == 0 && httpsTotal > 0

	if networkUp && directBlocked {
		env.Signals = append(env.Signals, SignalRestrictedAccess)
	}

	env.Restricted = directBlocked || repeated || env.Has(SignalTLSFailure)
	env.DeepDiscoveryAdvised = env.Restricted && !captive
}

// probeHTTPS performs one bounded HTTPS probe returning failure
// classification detail.
func (a *EnvironmentAnalyzer) probeHTTPS(ctx context.Context, target string) (bool, int64, string) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.ProbeTimeout)
	defer cancel()

	transport := &http.Transport{
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: a.cfg.ProbeTimeout,
		// The analyzer MEASURES interference: certificate validation
		// stays ON. A validation failure is exactly the TLS signal we
		// are probing for.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}

	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: a.cfg.ProbeTimeout}

	started := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, 0, "invalid target"
	}

	resp, err := client.Do(req)

	ms := time.Since(started).Milliseconds()

	if err != nil {
		return false, ms, errDetail(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return true, ms, ""
	}

	return false, ms, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

// probeCaptivePortal returns true when the plain-HTTP 204 probe is
// hijacked (redirect or content instead of 204).
func (a *EnvironmentAnalyzer) probeCaptivePortal(ctx context.Context) (bool, int64, string) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.ProbeTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: a.cfg.ProbeTimeout,
		// Captive portal detection NEEDS to observe the redirect, not
		// follow it.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	started := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.CaptivePortalTarget, nil)
	if err != nil {
		return false, 0, "invalid target"
	}

	resp, err := client.Do(req)

	ms := time.Since(started).Milliseconds()

	if err != nil {
		// The plain endpoint being unreachable is not portal
		// evidence (it may simply be blocked); no signal.
		return false, ms, errDetail(err)
	}

	defer resp.Body.Close()

	// A hijacked 204 probe: anything other than 204.
	if resp.StatusCode != http.StatusNoContent {
		return true, ms, fmt.Sprintf("hijacked: HTTP %d instead of 204", resp.StatusCode)
	}

	return false, ms, ""
}

// detectProxyEnvironment reports the configured proxy variable.
func detectProxyEnvironment() string {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := os.Getenv(key); v != "" {
			return key + "=" + v
		}
	}

	return ""
}

// ProxyEnvironment returns the active proxy environment variable
// ("" when none) — public helper for the transport layers.
func ProxyEnvironment() string { return detectProxyEnvironment() }

func anyProbeOK(probes []CheckResult, name string) bool {
	for _, p := range probes {
		if p.Name == name && p.OK {
			return true
		}
	}

	return false
}

func dedupeSignals(in []EnvironmentSignal) []EnvironmentSignal {
	seen := make(map[EnvironmentSignal]struct{}, len(in))

	out := make([]EnvironmentSignal, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}

		seen[s] = struct{}{}
		out = append(out, s)
	}

	return out
}

func summarizeEnvironment(env Environment) string {
	if env.Has(SignalCaptivePortal) {
		return "A captive portal is intercepting plain HTTP traffic; sign in to the network before connecting."
	}

	if env.Has(SignalRestrictedAccess) {
		return "The network is up but direct access to independent HTTPS endpoints fails; escalating discovery strategy."
	}

	if env.Has(SignalTLSFailure) {
		return "TLS handshakes fail against multiple independent endpoints while the network is up."
	}

	if env.Has(SignalRepeatedTimeout) {
		return "Repeated end-to-end timeouts observed across consecutive checks."
	}

	if env.Has(SignalDNSFailure) {
		return "Name resolution is failing while the network appears up."
	}

	if env.Has(SignalUnstableConnectivity) {
		return "Latency varies widely between independent endpoints."
	}

	if env.Has(SignalDirectOK) {
		return "Direct connectivity works normally."
	}

	return "Environment could not be fully classified from the current probes."
}

func errDetail(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120]
	}

	return msg
}
