package ranking

import (
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// subms_test.go pins the v0.9.8.1 ordering-correctness rules for
// sub-millisecond measurements: a working observation with LatencyMS
// == 0 is a REAL measurement (engine/tester/latency.go rule R5) —
// the fastest possible candidate — never "unmeasured".
//
// Pre-fix behaviour (the bug): ranking dropped working observations
// with LatencyMS == 0 from the latency and stability statistics, so a
// sub-millisecond candidate scored WORSE than a 100 ms one and sorted
// last among measured candidates.

func submsCandidate(fp string, latencies ...int64) Candidate {
	history := make([]config.TestObservation, 0, len(latencies))
	for _, ms := range latencies {
		history = append(history, config.TestObservation{
			At:        time.Now().UTC().UnixMilli(),
			Working:   true,
			LatencyMS: ms,
			Backend:   "tcp",
		})
	}

	return Candidate{
		Fingerprint:        fp,
		Name:               fp,
		Protocol:           "vless",
		CompatibleBackends: 1,
		History:            history,
	}
}

func TestSubMillisecondWorkingObservationIsMeasured(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	// A loopback-fast candidate: every observation worked with a
	// sub-millisecond round trip (LatencyMS == 0).
	score := Evaluate(submsCandidate("subms", 0, 0, 0), now)

	if !score.LatencyMSMeasured {
		t.Fatal("working sub-ms observations must set LatencyMSMeasured=true")
	}

	if score.LatencyMS != 0 {
		t.Fatalf("median projection = %d, want 0 (sub-ms)", score.LatencyMS)
	}

	// The pre-fix bug: sub-ms working observations were dropped from
	// the working set, producing latencyFactor == 0 (score ≈ 45).
	// Post-fix the sub-ms median lands in the excellent band.
	if score.Score <= 45 {
		t.Fatalf("sub-ms working candidate scored %f — dropped from latency stats?", score.Score)
	}

	found := false
	for _, reason := range score.Explanation {
		if reason == "< 1 ms median" {
			found = true
		}
	}

	if !found {
		t.Fatalf("explanation must honestly report \"< 1 ms median\", got %v", score.Explanation)
	}
}

func TestSubMillisecondSortsFirstAmongMeasured(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	candidates := []Candidate{
		submsCandidate("fast-subms", 0, 0),
		submsCandidate("slow", 900, 900),
		submsCandidate("mid", 120, 120),
	}

	ranked := Rank(candidates, now)

	if ranked[0].Fingerprint != "fast-subms" {
		t.Fatalf("sub-millisecond candidate must rank first, got %q", ranked[0].Fingerprint)
	}

	if !ranked[0].LatencyMSMeasured {
		t.Fatal("top candidate must carry LatencyMSMeasured=true")
	}
}

func TestSelectBestPrefersSubMillisecond(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	// Equal composites tie-break deterministically by fingerprint
	// (by design); this case uses a distinguishable slow candidate so
	// the sub-millisecond measurement itself is what wins.
	candidates := []Candidate{
		submsCandidate("zzz-subms", 0, 0),
		submsCandidate("aaa-slow", 900, 900),
	}

	_, score, ok := SelectBest(candidates, now)
	if !ok {
		t.Fatal("a selection must exist")
	}

	if score.Fingerprint != "zzz-subms" {
		t.Fatalf("SelectBest chose %q, want the sub-millisecond candidate", score.Fingerprint)
	}
}

func TestMetricScoresSubMillisecondPing(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	// A real ping run whose median is sub-millisecond (projected 0).
	rich := RichCandidate{
		Candidate: submsCandidate("pinged", 0),
		Ping: &config.PingMetrics{
			MinMS:    0,
			MaxMS:    0,
			MedianMS: 0,
			AvgMS:    0,
			SubMS:    true,
			Samples:  4,
			At:       now.UnixMilli(),
		},
	}

	m := EvaluateMetrics(rich, now)

	if m.PingProvenance != ProvenanceMeasured {
		t.Fatalf("ping provenance = %q, want measured", m.PingProvenance)
	}

	if m.PingScore != 1 {
		t.Fatalf("sub-ms measured ping scored %f, want 1 (excellent band)", m.PingScore)
	}

	if m.MeasuredMedianPingMS != 0 {
		t.Fatalf("measured median = %d, want 0 (sub-ms projection)", m.MeasuredMedianPingMS)
	}

	explained := false
	for _, line := range m.Explanation {
		if line == "median ping < 1 ms (4 samples)" {
			explained = true
		}
	}

	if !explained {
		t.Fatalf("explanation must report \"median ping < 1 ms (4 samples)\", got %v", m.Explanation)
	}
}

func TestEstimatedProvenanceFromSubMillisecondWorkingProbe(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	// No dedicated ping run; the last observation worked with a
	// sub-millisecond latency — still a valid estimate basis.
	rich := RichCandidate{Candidate: submsCandidate("est", 0)}

	prov, ping := pingProvenance(rich, now)
	if prov != ProvenanceEstimated || ping != nil {
		t.Fatalf("provenance = %q (ping=%v), want estimated (nil)", prov, ping)
	}
}

func TestRankMetricsLowestPingSubMillisecondLeads(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	candidates := []RichCandidate{
		{
			Candidate: submsCandidate("subms", 0, 0),
			Ping: &config.PingMetrics{
				MedianMS: 0,
				SubMS:    true,
				Samples:  3,
				At:       now.UnixMilli(),
			},
		},
		{
			Candidate: submsCandidate("forty", 40, 40),
			Ping: &config.PingMetrics{
				MedianMS: 40,
				Samples:  3,
				At:       now.UnixMilli(),
			},
		},
	}

	ranked := RankMetrics(candidates, SortLowestMedianPing, now)

	if len(ranked) != 2 {
		t.Fatalf("ranked %d candidates, want 2", len(ranked))
	}

	if ranked[0].Candidate.Fingerprint != "subms" {
		t.Fatalf("lowest-median-ping sort must lead with the sub-ms candidate, got %q",
			ranked[0].Candidate.Fingerprint)
	}
}
