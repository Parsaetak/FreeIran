package tester

import (
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// failure_classify_test.go — v0.11.0 ClassifyOutcomeFailure: the
// classification must come from OBSERVED evidence (facet shape, the
// URL-test's own canonical vocabulary), never from invented signals.
// The phase TIMINGS are explicitly proven NOT to be failure evidence.

func TestClassifyOutcomeHandshakeShape(t *testing.T) {
	out := ModeOutcome{
		Working:   false,
		TestedAt:  time.Now(),
		LastError: "inbound not ready",
		Handshake: &config.HandshakeMetrics{OK: false},
	}

	if got := ClassifyOutcomeFailure(out); got != config.FailureClassHandshake {
		t.Fatalf("class = %q, want handshake (the handshake facet failed)", got)
	}
}

func TestClassifyOutcomePingAllTimeout(t *testing.T) {
	out := ModeOutcome{
		Working:   false,
		TestedAt:  time.Now(),
		LastError: "timeout",
		Ping:      &config.PingMetrics{Timeouts: 4, Samples: 0},
	}

	if got := ClassifyOutcomeFailure(out); got != config.FailureClassTimeout {
		t.Fatalf("class = %q, want timeout (every ping sample timed out)", got)
	}
}

func TestClassifyOutcomeURLTimeoutFlag(t *testing.T) {
	out := ModeOutcome{
		Working:   false,
		TestedAt:  time.Now(),
		LastError: "timeout",
		URLTest: &config.URLTestMetrics{
			OK: false, Timeout: true, Error: "timeout",
			// Phase timings deliberately present: they are NOT
			// failure evidence (DNSMS is 0 for proxied requests,
			// TLSMS -1 for plain-HTTP targets, ConnectMS is measured
			// even on failure).
			DNSMS: 0, ConnectMS: 210, TLSMS: -1,
		},
	}

	if got := ClassifyOutcomeFailure(out); got != config.FailureClassTimeout {
		t.Fatalf("class = %q, want timeout (the URL test's Timeout flag)", got)
	}
}

func TestClassifyOutcomeHTTPStatusIsVerify(t *testing.T) {
	out := ModeOutcome{
		Working:   false,
		TestedAt:  time.Now(),
		LastError: "HTTP 403 via tunnel",
		URLTest: &config.URLTestMetrics{
			OK: false, Error: "HTTP 403 via tunnel",
			ConnectMS: 150, TLSMS: 300, Status: 403,
		},
	}

	if got := ClassifyOutcomeFailure(out); got != config.FailureClassVerify {
		t.Fatalf("class = %q, want verify (tunnel forwarded; response was not usable)", got)
	}
}

func TestClassifyOutcomeTextFallbackDNS(t *testing.T) {
	out := ModeOutcome{
		Working:   false,
		TestedAt:  time.Now(),
		LastError: "dial tcp: lookup node.example.com: no such host",
		URLTest: &config.URLTestMetrics{
			OK: false, Error: "dial tcp: lookup node.example.com: no such host",
		},
	}

	if got := ClassifyOutcomeFailure(out); got != config.FailureClassDNS {
		t.Fatalf("class = %q, want dns (from the observed error text)", got)
	}
}

func TestClassifyOutcomeUnclassifiableStaysEmpty(t *testing.T) {
	out := ModeOutcome{
		Working:   false,
		TestedAt:  time.Now(),
		LastError: "something unprecedented",
	}

	if got := ClassifyOutcomeFailure(out); got != "" {
		t.Fatalf("class = %q, want \"\" (never guess a class)", got)
	}
}

func TestApplyModeOutcomeRecordsFailureEvidence(t *testing.T) {
	at := time.Now().UTC()

	// A failing outcome with TLS-classified evidence.
	cfg := &config.Config{}
	ApplyModeOutcome(cfg, ModeOutcome{
		TestedAt:  at,
		Working:   false,
		LastError: "remote error: tls: bad certificate",
		URLTest: &config.URLTestMetrics{
			OK: false, Error: "remote error: tls: bad certificate",
		},
	})

	if cfg.FailureStreak != 1 {
		t.Fatalf("FailureStreak = %d, want 1", cfg.FailureStreak)
	}

	if cfg.LastFailureAt != at.UnixMilli() {
		t.Fatalf("LastFailureAt = %d, want the outcome time", cfg.LastFailureAt)
	}

	if cfg.LastFailureClass != config.FailureClassTLS {
		t.Fatalf("LastFailureClass = %q, want tls", cfg.LastFailureClass)
	}

	if len(cfg.TestHistory) != 1 || cfg.TestHistory[0].FailureClass != config.FailureClassTLS {
		t.Fatalf("history observation must carry the failure class: %+v", cfg.TestHistory)
	}

	// A success clears the failure evidence.
	ApplyModeOutcome(cfg, ModeOutcome{
		TestedAt: at.Add(time.Second),
		Working:  true,
		Latency:  50 * time.Millisecond,
	})

	if cfg.FailureStreak != 0 || cfg.LastFailureAt != 0 || cfg.LastFailureClass != "" {
		t.Fatalf("success must clear the failure evidence: streak=%d at=%d class=%q",
			cfg.FailureStreak, cfg.LastFailureAt, cfg.LastFailureClass)
	}

	if len(cfg.TestHistory) != 2 || cfg.TestHistory[1].FailureClass != "" {
		t.Fatalf("success observation must carry no failure class: %+v", cfg.TestHistory)
	}
}

func TestApplyModeOutcomeClassifiesEachOutcome(t *testing.T) {
	at := time.Now().UTC()
	cfg := &config.Config{}

	// First failure: handshake shape.
	ApplyModeOutcome(cfg, ModeOutcome{TestedAt: at, LastError: "inbound not ready",
		Handshake: &config.HandshakeMetrics{OK: false}})

	if cfg.LastFailureClass != config.FailureClassHandshake {
		t.Fatalf("class = %q, want handshake", cfg.LastFailureClass)
	}

	// Second failure: verify shape.
	ApplyModeOutcome(cfg, ModeOutcome{TestedAt: at.Add(time.Second),
		LastError: "HTTP 500 via tunnel",
		URLTest:   &config.URLTestMetrics{OK: false, Error: "HTTP 500 via tunnel", Status: 500}})

	if cfg.LastFailureClass != config.FailureClassVerify {
		t.Fatalf("class = %q, want verify (the most recent observation wins)", cfg.LastFailureClass)
	}

	if len(cfg.TestHistory) != 2 {
		t.Fatalf("history = %d entries, want 2", len(cfg.TestHistory))
	}

	if cfg.TestHistory[0].FailureClass != config.FailureClassHandshake ||
		cfg.TestHistory[1].FailureClass != config.FailureClassVerify {
		t.Fatalf("each observation must carry its own class: %+v", cfg.TestHistory)
	}
}
