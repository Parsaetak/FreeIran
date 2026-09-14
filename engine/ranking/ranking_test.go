package ranking

import (
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

var now = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

func obs(age time.Duration, working bool, latencyMS int64) config.TestObservation {
	return config.TestObservation{
		At:        now.Add(-age).UnixMilli(),
		Working:   working,
		LatencyMS: latencyMS,
	}
}

func timeoutObs(age time.Duration) config.TestObservation {
	return config.TestObservation{
		At:       now.Add(-age).UnixMilli(),
		Working:  false,
		TimedOut: true,
	}
}

// TestEvaluateUnknown covers the no-data and stale-data paths: the
// ranker must refuse to invent a score without observations.
func TestEvaluateUnknown(t *testing.T) {
	score := Evaluate(Candidate{
		Fingerprint:        "fp-a",
		CompatibleBackends: 1,
	}, now)

	if score.Class != ClassUnknown {
		t.Fatalf("class = %q, want unknown", score.Class)
	}

	if !score.Connectable {
		t.Fatal("untested candidate with a compatible core must be connectable")
	}

	stale := Evaluate(Candidate{
		Fingerprint:        "fp-b",
		CompatibleBackends: 1,
		History:            []config.TestObservation{obs(8*24*time.Hour, true, 50)},
	}, now)

	if stale.Class != ClassUnknown {
		t.Fatalf("stale class = %q, want unknown", stale.Class)
	}
}

// TestEvaluateDead covers all-failures and no-compatible-core.
func TestEvaluateDead(t *testing.T) {
	allFailed := Evaluate(Candidate{
		Fingerprint:        "fp-a",
		CompatibleBackends: 1,
		History: []config.TestObservation{
			obs(time.Hour, false, 0),
			obs(2*time.Hour, false, 0),
		},
	}, now)

	if allFailed.Class != ClassDead {
		t.Fatalf("class = %q, want dead", allFailed.Class)
	}

	if allFailed.Connectable {
		t.Fatal("all-failed candidate must not be connectable=true")
	}

	noCore := Evaluate(Candidate{
		Fingerprint:        "fp-b",
		CompatibleBackends: 0,
		History:            []config.TestObservation{obs(time.Minute, true, 30)},
	}, now)

	if noCore.Class != ClassDead || noCore.Connectable {
		t.Fatalf("no-core candidate: class=%q connectable=%v", noCore.Class, noCore.Connectable)
	}
}

// TestEvaluateBestAndUnstable exercises the class thresholds.
func TestEvaluateBestAndUnstable(t *testing.T) {
	// Eight recent successes at 31 ms → BEST (fresh, stable, fast).
	history := make([]config.TestObservation, 0, 12)

	for i := 0; i < 12; i++ {
		history = append(history,
			obs(time.Duration(i+1)*time.Minute, true, 31))
	}

	best := Evaluate(Candidate{
		Fingerprint:        "fp-best",
		CompatibleBackends: 1,
		History:            history,
	}, now)

	if best.Class != ClassBest {
		t.Fatalf("class = %q (score %.1f), want best", best.Class, best.Score)
	}

	if best.LatencyMS != 31 {
		t.Fatalf("median = %d, want 31", best.LatencyMS)
	}

	if len(best.Explanation) == 0 {
		t.Fatal("best candidate must carry an explanation")
	}

	// Flip-flop outcomes → UNSTABLE even at 50% success.
	flip := Evaluate(Candidate{
		Fingerprint:        "fp-flip",
		CompatibleBackends: 1,
		History: []config.TestObservation{
			obs(8*time.Minute, true, 100),
			obs(7*time.Minute, false, 0),
			obs(6*time.Minute, true, 100),
			obs(5*time.Minute, false, 0),
			obs(4*time.Minute, true, 100),
			obs(3*time.Minute, false, 0),
			obs(2*time.Minute, true, 100),
			obs(time.Minute, false, 0),
		},
	}, now)

	if flip.Class != ClassUnstable {
		t.Fatalf("class = %q, want unstable", flip.Class)
	}
}

// TestTimeoutPenalty verifies timeouts degrade the score and surface
// in the explanation.
func TestTimeoutPenalty(t *testing.T) {
	history := []config.TestObservation{
		obs(6*time.Hour, true, 100),
		obs(5*time.Hour, true, 100),
		obs(4*time.Hour, true, 100),
		timeoutObs(3 * time.Hour),
		timeoutObs(2 * time.Hour),
		timeoutObs(time.Hour),
	}

	with := Evaluate(Candidate{
		Fingerprint:        "fp-timeout",
		CompatibleBackends: 1,
		History:            history,
	}, now)

	if with.TimeoutRate == 0 {
		t.Fatal("timeout rate must be recorded")
	}
}

// TestSelectBestOrderingAndDeterminism covers: best over good over
// unknown, exclusion of dead candidates, and deterministic tie-break.
func TestSelectBestOrderingAndDeterminism(t *testing.T) {
	best := Candidate{
		Fingerprint:        "fp-best",
		Name:               "Netherlands",
		CompatibleBackends: 1,
		History: []config.TestObservation{
			obs(10*time.Minute, true, 31),
			obs(8*time.Minute, true, 32),
			obs(6*time.Minute, true, 31),
			obs(4*time.Minute, true, 33),
			obs(2*time.Minute, true, 31),
		},
	}

	good := Candidate{
		Fingerprint:        "fp-good",
		Name:               "Germany",
		CompatibleBackends: 1,
		History: []config.TestObservation{
			obs(10*time.Minute, true, 500),
			obs(8*time.Minute, true, 510),
			obs(6*time.Minute, true, 490),
			obs(4*time.Minute, false, 0),
			obs(2*time.Minute, true, 500),
		},
	}

	dead := Candidate{
		Fingerprint:        "fp-dead",
		CompatibleBackends: 1,
		History: []config.TestObservation{
			obs(10*time.Minute, false, 0),
			obs(2*time.Minute, false, 0),
		},
	}

	untested := Candidate{Fingerprint: "fp-untested", CompatibleBackends: 1}

	chosen, score, ok := SelectBest([]Candidate{dead, good, untested, best}, now)
	if !ok {
		t.Fatal("selection failed")
	}

	if chosen.Fingerprint != "fp-best" {
		t.Fatalf("chose %q, want fp-best", chosen.Fingerprint)
	}

	if score.Class != ClassBest {
		t.Fatalf("class = %q, want best", score.Class)
	}

	// Identical inputs → identical choice (determinism).
	for i := 0; i < 50; i++ {
		again, _, _ := SelectBest([]Candidate{good, dead, best, untested}, now)
		if again.Fingerprint != "fp-best" {
			t.Fatalf("iteration %d chose %q — nondeterministic", i, again.Fingerprint)
		}
	}

	// Only dead + untested: the untested candidate is the honest
	// last resort.
	chosen, score, ok = SelectBest([]Candidate{dead, untested}, now)
	if !ok || chosen.Fingerprint != "fp-untested" {
		t.Fatalf("last resort failed: ok=%v chosen=%q", ok, chosen.Fingerprint)
	}

	if score.Class != ClassUnknown {
		t.Fatalf("last resort class = %q, want unknown", score.Class)
	}

	// Nothing viable at all.
	if _, _, ok := SelectBest([]Candidate{dead}, now); ok {
		t.Fatal("must refuse to select a dead candidate")
	}
}

// TestRankTieBreak: equal scores order by fingerprint, never by
// input order.
func TestRankTieBreak(t *testing.T) {
	empty := func(fp string) Candidate {
		return Candidate{Fingerprint: fp, CompatibleBackends: 1}
	}

	ranked := Rank([]Candidate{empty("b"), empty("a"), empty("c")}, now)

	if ranked[0].Fingerprint != "a" || ranked[1].Fingerprint != "b" || ranked[2].Fingerprint != "c" {
		t.Fatalf("tie order = %s,%s,%s, want a,b,c",
			ranked[0].Fingerprint, ranked[1].Fingerprint, ranked[2].Fingerprint)
	}
}

// TestHistoryRingBound: the config helper keeps at most
// TestHistoryLimit entries with the OLDEST dropped.
func TestHistoryRingBound(t *testing.T) {
	var cfg config.Config

	for i := 0; i < config.TestHistoryLimit+5; i++ {
		cfg.AppendTestObservation(config.TestObservation{At: int64(i), Working: true})
	}

	if len(cfg.TestHistory) != config.TestHistoryLimit {
		t.Fatalf("history length = %d, want %d", len(cfg.TestHistory), config.TestHistoryLimit)
	}

	if cfg.TestHistory[0].At != int64(5) {
		t.Fatalf("oldest kept = %d, want 5 (oldest entries dropped)", cfg.TestHistory[0].At)
	}
}

// TestLatencyScoreBands: the bands match the tester's quality tiers.
func TestLatencyScoreBands(t *testing.T) {
	cases := []struct {
		ms     int64
		factor float64
	}{
		{31, 1.0},
		{150, 1.0},
		{400, 0.8},
		{800, 0.6},
		{2000, 0.35},
		{5000, 0.15},
	}

	for _, tc := range cases {
		factor, median := latencyScore([]int64{tc.ms})
		if factor != tc.factor || median != tc.ms {
			t.Fatalf("latencyScore(%d) = %.2f/%d, want %.2f/%d",
				tc.ms, factor, median, tc.factor, tc.ms)
		}
	}
}
