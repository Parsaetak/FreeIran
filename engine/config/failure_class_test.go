package config

import "testing"

// failure_class_test.go — v0.11.0 failure-class vocabulary,
// classification precedence and the freshness evidence tuple. Every
// input below is a REAL error shape produced by the engine (the
// tester's canonical short forms, Go net errors, core supervision
// events), never an invented string.

func TestClassifyFailureVocabulary(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		// DNS layer.
		{"dial tcp: lookup node.example.com: no such host", FailureClassDNS},
		{"resolver: servfail for example.com", FailureClassDNS},
		{"name resolution failed", FailureClassDNS},

		// Listener layer.
		{"inbound: bind: address already in use", FailureClassListener},
		{"local listener port in use", FailureClassListener},

		// Handshake layer (core never became ready).
		{"inbound not ready: startup timeout", FailureClassHandshake},
		{"core exited during startup (code 1)", FailureClassHandshake},
		{"process exited during startup", FailureClassHandshake},
		{"handshake timeout", FailureClassHandshake},

		// TLS layer.
		{"tls: handshake failure", FailureClassTLS},
		{"x509: certificate signed by unknown authority", FailureClassTLS},
		{"remote error: tls: bad certificate", FailureClassTLS},
		{"ech: invalid configs", FailureClassTLS},
		{"reality: server rejected", FailureClassTLS},

		// Reset.
		{"read: connection reset by peer", FailureClassReset},
		{"write: broken pipe", FailureClassReset},
		{"reset", FailureClassReset},

		// TCP layer.
		{"dial tcp 1.2.3.4:443: connect: connection refused", FailureClassTCP},
		{"refused", FailureClassTCP},
		{"dial tcp: connect: no route to host", FailureClassTCP},

		// Verification.
		{"verification failed: target fetch rejected", FailureClassVerify},
		{"quorum not reached (1 of 3)", FailureClassVerify},

		// Transport-specific.
		{"socks: general failure", FailureClassTransport},
		{"proxy: authentication failed", FailureClassTransport},
		{"protocol version unsupported", FailureClassTransport},

		// Timeout — matched LAST so a "TLS handshake timeout" stays
		// classified by its observable layer (TLS).
		{"i/o timeout", FailureClassTimeout},
		{"context deadline exceeded", FailureClassTimeout},
		{"timeout", FailureClassTimeout},

		// Unknown → unclassified, never a wrong class.
		{"weird thing happened", ""},
		{"", ""},
	}

	for _, tc := range cases {
		if got := ClassifyFailure(tc.reason); got != tc.want {
			t.Errorf("ClassifyFailure(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestClassifyFailureTLSOutranksTimeout(t *testing.T) {
	// "TLS handshake timeout" — the deadline fired, but the actionable
	// fact for transport selection is that TLS never completed.
	if got := ClassifyFailure("tls: handshake timeout"); got != FailureClassTLS {
		t.Fatalf("ClassifyFailure(tls handshake timeout) = %q, want %q", got, FailureClassTLS)
	}
}

func TestValidFailureClassDomain(t *testing.T) {
	for _, class := range AllFailureClasses {
		if !ValidFailureClass(class) {
			t.Errorf("domain member %q rejected by ValidFailureClass", class)
		}
	}

	for _, bad := range []string{"", "confidence", "unknown-class", "TIMEOUT"} {
		if ValidFailureClass(bad) {
			t.Errorf("ValidFailureClass(%q) = true, want false", bad)
		}
	}
}

func TestNormalizeDropsUnknownFailureClass(t *testing.T) {
	cfg := Config{LastFailureClass: "some-future-class"}
	cfg.Normalize()

	if cfg.LastFailureClass != "" {
		t.Fatalf("unknown failure class survived normalization: %q", cfg.LastFailureClass)
	}

	cfg = Config{LastFailureClass: FailureClassHandshake}
	cfg.Normalize()

	if cfg.LastFailureClass != FailureClassHandshake {
		t.Fatalf("known failure class dropped by normalization: %q", cfg.LastFailureClass)
	}
}

func TestEvidenceTuple(t *testing.T) {
	now := int64(1_700_000_000_000)

	// Never observed anything.
	var zero Config
	ev := zero.Evidence(now)
	if ev.LastVerifiedAt != 0 || ev.LastFailureAt != 0 ||
		ev.RecentFailureClass != "" || ev.VerificationAgeMS != 0 {
		t.Fatalf("zero config evidence = %+v, want all-zero", ev)
	}

	// Fresh success, old failure → age measured to the success.
	cfg := Config{
		LastSuccessAt:    now - 60_000,
		LastFailureAt:    now - 600_000,
		LastFailureClass: FailureClassTimeout,
	}
	ev = cfg.Evidence(now)

	if ev.LastVerifiedAt != now-60_000 {
		t.Fatalf("LastVerifiedAt = %d, want the success time", ev.LastVerifiedAt)
	}

	if ev.VerificationAgeMS != 60_000 {
		t.Fatalf("VerificationAgeMS = %d, want 60000 (age to the more recent success)", ev.VerificationAgeMS)
	}

	if ev.RecentFailureClass != FailureClassTimeout {
		t.Fatalf("RecentFailureClass = %q, want the recorded class", ev.RecentFailureClass)
	}

	// Fresh failure, older success → age measured to the failure.
	cfg = Config{
		LastSuccessAt:    now - 600_000,
		LastFailureAt:    now - 30_000,
		LastFailureClass: FailureClassTLS,
	}
	ev = cfg.Evidence(now)

	if ev.VerificationAgeMS != 30_000 {
		t.Fatalf("VerificationAgeMS = %d, want 30000 (age to the more recent failure)", ev.VerificationAgeMS)
	}

	// Future timestamps must not produce negative ages.
	cfg = Config{LastSuccessAt: now + 5_000}
	ev = cfg.Evidence(now)

	if ev.VerificationAgeMS != 0 {
		t.Fatalf("VerificationAgeMS = %d, want 0 (clamped)", ev.VerificationAgeMS)
	}
}
