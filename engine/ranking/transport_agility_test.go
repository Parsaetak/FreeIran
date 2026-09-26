package ranking

import (
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// transport_agility_test.go — v0.11.0: the repeated protocol-specific
// failure demotion must make the ranking prefer a DIFFERENT
// already-supported transport, using ONLY observed evidence (no
// parallel failover engine, no invented confidence score).

func ms(n int64) time.Time { return time.UnixMilli(n).UTC() }

func obsAt(at int64, working bool, class string) config.TestObservation {
	return config.TestObservation{At: at, Working: working, LatencyMS: 120, FailureClass: class}
}

func TestTrailingFailureEvidence(t *testing.T) {
	cases := []struct {
		name       string
		window     []config.TestObservation
		wantStreak int
		wantClass  string
	}{
		{
			name:       "no history",
			wantStreak: 0,
		},
		{
			name: "fresh success resets the streak",
			window: []config.TestObservation{
				obsAt(1, false, config.FailureClassTLS),
				obsAt(2, true, ""),
			},
			wantStreak: 0,
		},
		{
			name: "single trailing failure carries its class",
			window: []config.TestObservation{
				obsAt(1, true, ""),
				obsAt(2, false, config.FailureClassHandshake),
			},
			wantStreak: 1,
			wantClass:  config.FailureClassHandshake,
		},
		{
			name: "shared-class trailing streak",
			window: []config.TestObservation{
				obsAt(1, true, ""),
				obsAt(2, false, config.FailureClassTLS),
				obsAt(3, false, config.FailureClassTLS),
				obsAt(4, false, config.FailureClassTLS),
			},
			wantStreak: 3,
			wantClass:  config.FailureClassTLS,
		},
		{
			name: "mixed trailing classes stay unattributed",
			window: []config.TestObservation{
				obsAt(1, false, config.FailureClassTLS),
				obsAt(2, false, config.FailureClassTimeout),
			},
			wantStreak: 2,
			wantClass:  "",
		},
		{
			name: "unattributed trailing failures stay unattributed",
			window: []config.TestObservation{
				obsAt(1, false, ""),
				obsAt(2, false, config.FailureClassTLS),
			},
			wantStreak: 2,
			wantClass:  "",
		},
		{
			name: "working observation between failures stops the streak",
			window: []config.TestObservation{
				obsAt(1, false, config.FailureClassTLS),
				obsAt(2, false, config.FailureClassTLS),
				obsAt(3, true, ""),
				obsAt(4, false, config.FailureClassTLS),
			},
			wantStreak: 1,
			wantClass:  config.FailureClassTLS,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			streak, class := trailingFailureEvidence(tc.window)
			if streak != tc.wantStreak || class != tc.wantClass {
				t.Fatalf("trailingFailureEvidence = (%d, %q), want (%d, %q)",
					streak, class, tc.wantStreak, tc.wantClass)
			}
		})
	}
}

func TestProtocolFailureFactorBounds(t *testing.T) {
	cases := map[int]float64{
		2: 0.85,
		3: 0.70,
		4: 0.55,
		9: 0.55, // saturates — never zeroes the candidate
	}

	for streak, want := range cases {
		if got := protocolFailureFactor(streak); got != want {
			t.Errorf("protocolFailureFactor(%d) = %v, want %v", streak, got, want)
		}
	}
}

func TestProtocolSpecificClassDomain(t *testing.T) {
	protocolSpecific := []string{
		config.FailureClassTLS,
		config.FailureClassHandshake,
		config.FailureClassTransport,
	}

	for _, class := range protocolSpecific {
		if !isProtocolSpecificClass(class) {
			t.Errorf("isProtocolSpecificClass(%q) = false, want true", class)
		}
	}

	// Path/network classes must NOT drive transport preference.
	notProtocolSpecific := []string{
		config.FailureClassDNS,
		config.FailureClassTCP,
		config.FailureClassReset,
		config.FailureClassTimeout,
		config.FailureClassVerify,
		config.FailureClassListener,
		"",
	}

	for _, class := range notProtocolSpecific {
		if isProtocolSpecificClass(class) {
			t.Errorf("isProtocolSpecificClass(%q) = true, want false", class)
		}
	}
}

// TestTransportAgilityPrefersDifferentTransport is the core
// transport-agility property: two candidates with IDENTICAL history
// shape and latency, except one carries repeated TLS-class failures
// (its transport is demonstrably broken) — the other must rank
// higher so automatic selection moves to the working transport.
func TestTransportAgilityPrefersDifferentTransport(t *testing.T) {
	now := ms(1_000_000)

	broken := Candidate{
		Fingerprint: "aaa-broken", Protocol: "vless",
		CompatibleBackends: 2,
		History: []config.TestObservation{
			obsAt(400, true, ""),
			obsAt(500, true, ""),
			obsAt(600, false, config.FailureClassTLS),
			obsAt(700, false, config.FailureClassTLS),
			obsAt(800, false, config.FailureClassTLS),
		},
	}

	healthy := Candidate{
		Fingerprint: "zzz-healthy", Protocol: "trojan",
		CompatibleBackends: 2,
		History: []config.TestObservation{
			obsAt(400, true, ""),
			obsAt(500, true, ""),
			obsAt(600, false, config.FailureClassTimeout), // path trouble, not the transport
			obsAt(700, false, config.FailureClassTimeout),
			obsAt(800, false, config.FailureClassTimeout),
		},
	}

	scores := Rank([]Candidate{broken, healthy}, now)

	if scores[0].Fingerprint != "zzz-healthy" {
		t.Fatalf("ranking did not prefer the different transport: %+v", scores)
	}

	brokenScore := scoreOf(scores, "aaa-broken")
	if brokenScore.RecentFailureClass != config.FailureClassTLS {
		t.Fatalf("RecentFailureClass = %q, want tls", brokenScore.RecentFailureClass)
	}

	if brokenScore.FailureStreak != 3 {
		t.Fatalf("FailureStreak = %d, want 3", brokenScore.FailureStreak)
	}

	if len(brokenScore.Explanation) == 0 {
		t.Fatal("the demotion must be explainable in the score's reasons")
	}
}

// TestScatteredFailuresNotDemotedBeyondTimeoutPenalty: a candidate
// whose trailing failures MIX classes (transient network trouble)
// must NOT receive the protocol-specific demotion — its evidence does
// not point at the transport.
func TestScatteredFailuresNotDemoted(t *testing.T) {
	now := ms(1_000_000)

	scattered := Candidate{
		Fingerprint: "aaa-scattered", Protocol: "vless",
		CompatibleBackends: 1,
		History: []config.TestObservation{
			obsAt(400, true, ""),
			obsAt(500, false, config.FailureClassTimeout),
			obsAt(600, false, config.FailureClassTCP),
			obsAt(700, false, config.FailureClassReset),
		},
	}

	score := Evaluate(scattered, now)

	if score.RecentFailureClass != "" {
		t.Fatalf("mixed trailing classes must stay unattributed: %q", score.RecentFailureClass)
	}

	for _, reason := range score.Explanation {
		if contains(reason, "preferring other transports") {
			t.Fatalf("scattered failures must not drive transport preference: %v", score.Explanation)
		}
	}
}

// TestSingleProtocolFailureNotDemoted: ONE protocol-specific failure
// is not evidence — the demotion requires a repeated streak.
func TestSingleProtocolFailureNotDemoted(t *testing.T) {
	now := ms(1_000_000)

	single := Candidate{
		Fingerprint: "aaa-single", Protocol: "vless",
		CompatibleBackends: 1,
		History: []config.TestObservation{
			obsAt(400, true, ""),
			obsAt(500, true, ""),
			obsAt(600, false, config.FailureClassHandshake),
		},
	}

	score := Evaluate(single, now)

	if score.FailureStreak != 1 || score.RecentFailureClass != config.FailureClassHandshake {
		t.Fatalf("evidence must be recorded: streak=%d class=%q", score.FailureStreak, score.RecentFailureClass)
	}

	for _, reason := range score.Explanation {
		if contains(reason, "preferring other transports") {
			t.Fatalf("a single failure must not demote: %v", score.Explanation)
		}
	}
}

// TestRichCandidateSurfaceSameRule: the MetricScores composite (the
// discovery surface) applies the same demotion from its own evidence.
func TestRichCandidateSurfaceSameRule(t *testing.T) {
	now := ms(1_000_000)

	base := RichCandidate{
		Candidate: Candidate{Fingerprint: "x", CompatibleBackends: 1},
	}

	broken := base
	broken.History = []config.TestObservation{
		obsAt(400, true, ""),
		obsAt(500, true, ""),
		obsAt(600, false, config.FailureClassHandshake),
		obsAt(700, false, config.FailureClassHandshake),
	}
	broken.FailureStreak = 2
	broken.LastFailureClass = config.FailureClassHandshake
	broken.LastFailureAt = 700

	scattered := base
	scattered.History = []config.TestObservation{
		obsAt(400, true, ""),
		obsAt(500, true, ""),
		obsAt(600, false, config.FailureClassTimeout),
		obsAt(700, false, config.FailureClassTCP),
	}
	scattered.FailureStreak = 2
	scattered.LastFailureClass = config.FailureClassTimeout
	scattered.LastFailureAt = 700

	brokenScore := EvaluateMetrics(broken, now)
	scatteredScore := EvaluateMetrics(scattered, now)

	if brokenScore.OverallScore >= scatteredScore.OverallScore {
		t.Fatalf("protocol-specific streak must demote the composite: broken=%v scattered=%v",
			brokenScore.OverallScore, scatteredScore.OverallScore)
	}

	if brokenScore.RecentFailureClass != config.FailureClassHandshake ||
		brokenScore.FailureStreak != 2 || brokenScore.LastFailureAt != 700 {
		t.Fatalf("evidence must surface on MetricScores: %+v", brokenScore)
	}
}

func scoreOf(scores []Score, fingerprint string) Score {
	for _, s := range scores {
		if s.Fingerprint == fingerprint {
			return s
		}
	}

	panic("score not found: " + fingerprint)
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(haystack) > 0 && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}

	return -1
}
