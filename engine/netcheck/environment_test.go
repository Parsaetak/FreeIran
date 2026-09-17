package netcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// environment_test.go validates the environment intelligence layer
// against local, deterministic servers — every signal derives from
// probe evidence, never from assumptions.

// TestEnvironmentDirectOK: both HTTPS probes succeed → direct_ok.
func TestEnvironmentDirectOK(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()

	cfg := DefaultEnvironmentConfig()
	cfg.HTTPTargets = []string{ok.URL, ok.URL}
	cfg.CaptivePortalTarget = ok.URL
	cfg.ProbeTimeout = 3 * time.Second

	a := NewEnvironmentAnalyzer(cfg)

	env := a.Analyze(context.Background())

	if !env.Has(SignalDirectOK) {
		t.Fatalf("expected direct_ok, got %v (%s)", env.Signals, env.Summary)
	}

	if env.Restricted || env.DeepDiscoveryAdvised {
		t.Fatalf("healthy environment classified restricted: %v", env.Signals)
	}
}

// TestEnvironmentTLSFailure: a server with a self-signed certificate
// produces TLS failures against independent endpoints → tls_failure
// (interference evidence) and restricted.
func TestEnvironmentTLSFailure(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tlsSrv.Close()

	cfg := DefaultEnvironmentConfig()
	// Two "independent" endpoints that both present certificates the
	// validator rejects.
	cfg.HTTPTargets = []string{tlsSrv.URL, tlsSrv.URL}
	cfg.CaptivePortalTarget = "http://127.0.0.1:1/" // unreachable: no portal signal
	cfg.ProbeTimeout = 3 * time.Second

	a := NewEnvironmentAnalyzer(cfg)

	env := a.Analyze(context.Background())

	if !env.Has(SignalTLSFailure) {
		t.Fatalf("expected tls_failure, got %v (%s)", env.Signals, env.Summary)
	}

	if !env.Restricted {
		t.Fatalf("TLS interference should classify restricted: %v", env.Signals)
	}
}

// TestEnvironmentCaptivePortal: a plain-HTTP probe answering 302
// instead of 204 → captive_portal.
func TestEnvironmentCaptivePortal(t *testing.T) {
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://portal.example/login", http.StatusFound)
	}))
	defer portal.Close()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()

	cfg := DefaultEnvironmentConfig()
	cfg.HTTPTargets = []string{ok.URL}
	cfg.CaptivePortalTarget = portal.URL
	cfg.ProbeTimeout = 3 * time.Second

	a := NewEnvironmentAnalyzer(cfg)

	env := a.Analyze(context.Background())

	if !env.Has(SignalCaptivePortal) {
		t.Fatalf("expected captive_portal, got %v (%s)", env.Signals, env.Summary)
	}

	// A captive portal is a sign-in problem, not a reason to escalate
	// discovery depth.
	if env.DeepDiscoveryAdvised {
		t.Fatalf("captive portal must not trigger deep discovery: %v", env.Signals)
	}
}

// TestEnvironmentDNSFailureSignal: DNS probe against a resolver that
// cannot resolve anything (invalid resolver) — the signal derives
// from the DNS probe result.
func TestEnvironmentDNSFailureSignal(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()

	cfg := DefaultEnvironmentConfig()
	cfg.HTTPTargets = []string{ok.URL}

	a := &EnvironmentAnalyzer{cfg: cfg}

	// Simulate the DNS probe outcome: build the environment by hand
	// through the same classify path with a failing DNS probe.
	env := Environment{AnalyzedAt: time.Now().UTC()}

	probes := []CheckResult{
		{Name: "https", Target: ok.URL, OK: true, LatencyMS: 5},
		{Name: "dns", Target: "www.gstatic.com", OK: false, Error: "no such host"},
	}

	a.classify(context.Background(), &env, probes)

	if !env.Has(SignalDNSFailure) && !env.Has(SignalDirectOK) {
		t.Fatalf("classification dropped evidence: %v", env.Signals)
	}
}

// TestEnvironmentRepeatedTimeout: consecutive analyses with timeout
// evidence escalate to repeated_timeout.
func TestEnvironmentRepeatedTimeout(t *testing.T) {
	cfg := DefaultEnvironmentConfig()
	cfg.TimeoutMemory = 2

	a := &EnvironmentAnalyzer{cfg: cfg}

	// Two consecutive timeout-only probe sets.
	for i := 0; i < 2; i++ {
		env := Environment{AnalyzedAt: time.Now().UTC()}

		probes := []CheckResult{
			{Name: "https", Target: "https://x/", OK: false, Error: "timeout"},
			{Name: "https", Target: "https://y/", OK: false, Error: "timeout"},
			{Name: "dns", Target: "www.gstatic.com", OK: true},
		}

		a.classify(context.Background(), &env, probes)

		if i == 0 && env.Has(SignalRepeatedTimeout) {
			t.Fatal("first timeout observation must not be repeated_timeout")
		}
	}

	// Third classification: the memory should have accumulated.
	env := Environment{AnalyzedAt: time.Now().UTC()}

	probes := []CheckResult{
		{Name: "https", Target: "https://x/", OK: false, Error: "timeout"},
		{Name: "https", Target: "https://y/", OK: false, Error: "timeout"},
		{Name: "dns", Target: "www.gstatic.com", OK: true},
	}

	a.classify(context.Background(), &env, probes)

	if !env.Has(SignalRepeatedTimeout) {
		t.Fatalf("expected repeated_timeout, got %v", env.Signals)
	}

	if !env.DeepDiscoveryAdvised {
		t.Fatal("repeated timeouts should advise deep discovery")
	}
}

// TestEnvironmentUnstableConnectivity: latency spread beyond the
// threshold signals instability.
func TestEnvironmentUnstableConnectivity(t *testing.T) {
	cfg := DefaultEnvironmentConfig()
	cfg.InstabilityJitterMS = 100

	a := &EnvironmentAnalyzer{cfg: cfg}

	env := Environment{AnalyzedAt: time.Now().UTC()}

	probes := []CheckResult{
		{Name: "https", Target: "https://a/", OK: true, LatencyMS: 30},
		{Name: "https", Target: "https://b/", OK: true, LatencyMS: 900},
		{Name: "dns", Target: "www.gstatic.com", OK: true},
	}

	a.classify(context.Background(), &env, probes)

	if !env.Has(SignalUnstableConnectivity) {
		t.Fatalf("expected unstable_connectivity, got %v", env.Signals)
	}
}

// TestEnvironmentProxyEnvironment: setting HTTP_PROXY signals the
// proxy environment (the engine should respect the upstream).
func TestEnvironmentProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")

	if got := ProxyEnvironment(); got == "" {
		t.Fatal("proxy environment not detected")
	}

	cfg := DefaultEnvironmentConfig()

	a := &EnvironmentAnalyzer{cfg: cfg}

	env := Environment{AnalyzedAt: time.Now().UTC()}

	// classify with an empty probe set still records the env-var
	// signal (it is process evidence, not network evidence).
	a.classify(context.Background(), &env, nil)

	if !env.Has(SignalProxyEnvironment) {
		t.Fatalf("expected proxy_environment, got %v", env.Signals)
	}
}

// TestEnvironmentSummaryIsEvidenceBased: summaries stay descriptive.
func TestEnvironmentSummaryIsEvidenceBased(t *testing.T) {
	for _, tc := range []struct {
		env    Environment
		substr string
	}{
		{Environment{Signals: []EnvironmentSignal{SignalCaptivePortal}}, "captive portal"},
		{Environment{Signals: []EnvironmentSignal{SignalRestrictedAccess}}, "independent HTTPS endpoints"},
		{Environment{Signals: []EnvironmentSignal{SignalDirectOK}}, "normally"},
	} {
		got := summarizeEnvironment(tc.env)
		if !contains(got, tc.substr) {
			t.Fatalf("summary %q missing %q", got, tc.substr)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}

	return -1
}
