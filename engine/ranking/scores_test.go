package ranking

import (
	"testing"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// msAt builds a Unix-milli timestamp `age` old.
func msAt(now time.Time, age time.Duration) int64 {
	return now.Add(-age).UnixMilli()
}

// rich builds a rich candidate with the minimum viable shape.
func rich(fp string, opts ...func(*RichCandidate)) RichCandidate {
	c := RichCandidate{
		Candidate: Candidate{
			Fingerprint:        fp,
			Name:               fp,
			Protocol:           "vless",
			CompatibleBackends: 1,
		},
	}

	for _, o := range opts {
		o(&c)
	}

	return c
}

func withPing(p config.PingMetrics) func(*RichCandidate) {
	return func(c *RichCandidate) { c.Ping = &p }
}

func withURL(u config.URLTestMetrics) func(*RichCandidate) {
	return func(c *RichCandidate) { c.URLTest = &u }
}

func withHistory(obs ...config.TestObservation) func(*RichCandidate) {
	return func(c *RichCandidate) { c.History = obs }
}

func withLastSuccess(now time.Time, age time.Duration) func(*RichCandidate) {
	return func(c *RichCandidate) { c.LastSuccessAt = msAt(now, age) }
}

// TestPingProvenanceRules verifies the provenance classification:
// fresh measured, stale, estimated from a working probe, unavailable.
func TestPingProvenanceRules(t *testing.T) {
	now := time.Now().UTC()

	fresh := config.PingMetrics{Samples: 4, MedianMS: 30, At: msAt(now, time.Minute)}
	stale := config.PingMetrics{Samples: 4, MedianMS: 30, At: msAt(now, time.Hour)}

	cases := []struct {
		name string
		c    RichCandidate
		want Provenance
	}{
		{"measured", rich("a", withPing(fresh)), ProvenanceMeasured},
		{"stale", rich("b", withPing(stale)), ProvenanceStale},
		{
			"estimated from working probe",
			rich("c", withHistory(config.TestObservation{At: msAt(now, time.Minute), Working: true, LatencyMS: 40})),
			ProvenanceEstimated,
		},
		{
			"untried",
			rich("d"),
			ProvenanceUnavailable,
		},
	}

	for _, tc := range cases {
		got, _ := pingProvenance(tc.c, now)
		if got != tc.want {
			t.Fatalf("%s: provenance = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestSpecExampleFastButBrokenVsSlowerButWorking is the specification's
// own example (§11): a 20 ms node with failed connectivity must NOT
// outrank a 75 ms node that consistently provides working Internet.
func TestSpecExampleFastButBrokenVsSlowerButWorking(t *testing.T) {
	now := time.Now().UTC()

	fastButBroken := rich("fast-broken",
		withPing(config.PingMetrics{Samples: 4, MedianMS: 20, At: msAt(now, time.Minute)}),
		withURL(config.URLTestMetrics{OK: false, Timeout: true, At: msAt(now, time.Minute), TotalMS: 12000}),
		withHistory(
			config.TestObservation{At: msAt(now, 2*time.Minute), Working: false, TimedOut: true},
			config.TestObservation{At: msAt(now, 4*time.Minute), Working: false, TimedOut: true},
		),
	)

	slowerButWorking := rich("slow-working",
		withPing(config.PingMetrics{Samples: 4, MedianMS: 75, At: msAt(now, time.Minute)}),
		withURL(config.URLTestMetrics{OK: true, Status: 204, TotalMS: 210, At: msAt(now, time.Minute)}),
		withHistory(
			config.TestObservation{At: msAt(now, time.Minute), Working: true, LatencyMS: 75},
			config.TestObservation{At: msAt(now, 3*time.Minute), Working: true, LatencyMS: 80},
		),
		withLastSuccess(now, time.Minute),
	)

	ranked := RankMetrics([]RichCandidate{fastButBroken, slowerButWorking}, SortBestOverall, now)

	if ranked[0].Candidate.Fingerprint != "slow-working" {
		t.Fatalf("composite ordering wrong: %s outranks %s",
			ranked[0].Candidate.Fingerprint, ranked[1].Candidate.Fingerprint)
	}

	if ranked[0].Metrics.OverallScore <= ranked[1].Metrics.OverallScore {
		t.Fatalf("working candidate must score higher: %f vs %f",
			ranked[0].Metrics.OverallScore, ranked[1].Metrics.OverallScore)
	}
}

// TestPingSortNeverUsesEstimates verifies the partition rule: in a
// ping-sorted list, a measured candidate always precedes estimated
// ones, and an estimated candidate is NEVER sorted by its implied
// number.
func TestPingSortNeverUsesEstimates(t *testing.T) {
	now := time.Now().UTC()

	// Estimated 10ms (implied by tunnel probe) vs measured 300ms.
	estimatedFast := rich("est-fast",
		withHistory(config.TestObservation{At: msAt(now, time.Minute), Working: true, LatencyMS: 10}),
	)

	measuredSlow := rich("meas-slow",
		withPing(config.PingMetrics{Samples: 4, MedianMS: 300, At: msAt(now, time.Minute)}),
	)

	stale := rich("stale",
		withPing(config.PingMetrics{Samples: 4, MedianMS: 15, At: msAt(now, 2*time.Hour)}),
	)

	untried := rich("untried")

	ranked := RankMetrics([]RichCandidate{estimatedFast, untried, stale, measuredSlow}, SortLowestPing, now)

	wantOrder := []string{"meas-slow", "stale", "est-fast", "untried"}

	for i, want := range wantOrder {
		if ranked[i].Candidate.Fingerprint != want {
			t.Fatalf("ping sort position %d = %s, want %s (full order: %v)",
				i, ranked[i].Candidate.Fingerprint, want, fingerprints(ranked))
		}
	}
}

// TestPingSortMeasuredOrdering verifies measured candidates sort by
// their real median.
func TestPingSortMeasuredOrdering(t *testing.T) {
	now := time.Now().UTC()

	mk := func(fp string, median int64) RichCandidate {
		return rich(fp, withPing(config.PingMetrics{Samples: 4, MedianMS: median, At: msAt(now, time.Minute)}))
	}

	ranked := RankMetrics([]RichCandidate{mk("c", 300), mk("a", 80), mk("b", 200)}, SortLowestMedianPing, now)

	want := []string{"a", "b", "c"}
	for i, w := range want {
		if ranked[i].Candidate.Fingerprint != w {
			t.Fatalf("median ordering wrong: %v", fingerprints(ranked))
		}
	}
}

// TestURLSortFailedSinks verifies measured-and-failed URL tests sink
// below measured-and-OK regardless of speed.
func TestURLSortFailedSinks(t *testing.T) {
	now := time.Now().UTC()

	ok := rich("ok",
		withURL(config.URLTestMetrics{OK: true, Status: 204, TotalMS: 400, At: msAt(now, time.Minute)}),
	)

	failedFast := rich("failed-fast",
		withURL(config.URLTestMetrics{OK: false, Timeout: true, TotalMS: 12000, At: msAt(now, time.Minute)}),
	)

	ranked := RankMetrics([]RichCandidate{failedFast, ok}, SortBestURLResponse, now)

	if ranked[0].Candidate.Fingerprint != "ok" {
		t.Fatalf("failed URL test must sink: %v", fingerprints(ranked))
	}
}

// TestRecentlyVerifiedSort orders by the last verified timestamp.
func TestRecentlyVerifiedSort(t *testing.T) {
	now := time.Now().UTC()

	old := rich("old", withLastSuccess(now, time.Hour))
	fresh := rich("fresh", withLastSuccess(now, time.Minute))
	never := rich("never")

	ranked := RankMetrics([]RichCandidate{old, never, fresh}, SortRecentlyVerified, now)

	want := []string{"fresh", "old", "never"}
	for i, w := range want {
		if ranked[i].Candidate.Fingerprint != w {
			t.Fatalf("recently-verified order: %v", fingerprints(ranked))
		}
	}
}

// TestMetricScoresExplanations verifies every score surface carries
// human explanations.
func TestMetricScoresExplanations(t *testing.T) {
	now := time.Now().UTC()

	c := rich("explained",
		withPing(config.PingMetrics{Samples: 4, MedianMS: 42, JitterMS: 5, PacketLoss: 0, At: msAt(now, time.Minute)}),
		withURL(config.URLTestMetrics{OK: true, Status: 204, TotalMS: 150, At: msAt(now, time.Minute)}),
		withHistory(config.TestObservation{At: msAt(now, time.Minute), Working: true, LatencyMS: 42}),
		withLastSuccess(now, time.Minute),
	)

	m := EvaluateMetrics(c, now)

	if len(m.Explanation) < 3 {
		t.Fatalf("explanation too thin: %v", m.Explanation)
	}

	if m.PingProvenance != ProvenanceMeasured || m.URLProvenance != ProvenanceMeasured {
		t.Fatalf("provenance wrong: %s/%s", m.PingProvenance, m.URLProvenance)
	}

	if m.OverallScore <= 0 || m.OverallScore > 1 {
		t.Fatalf("overall out of range: %f", m.OverallScore)
	}
}

// TestUnmeasuredCandidateIsNeutral verifies an unmeasured candidate
// scores neutrally (never punished) and reports unavailable.
func TestUnmeasuredCandidateIsNeutral(t *testing.T) {
	now := time.Now().UTC()

	m := EvaluateMetrics(rich("blank"), now)

	if m.PingProvenance != ProvenanceUnavailable || m.URLProvenance != ProvenanceUnavailable {
		t.Fatalf("provenance should be unavailable: %s/%s", m.PingProvenance, m.URLProvenance)
	}

	if m.PingScore != 0.5 || m.URLScore != 0.5 {
		t.Fatalf("neutral scores wrong: %f/%f", m.PingScore, m.URLScore)
	}
}

// TestSortModeLabelsAreComplete keeps the UI list in sync.
func TestSortModeLabelsAreComplete(t *testing.T) {
	if len(AllSortModes) != 9 {
		t.Fatalf("sort modes = %d, want 9", len(AllSortModes))
	}

	for _, m := range AllSortModes {
		if SortModeLabel(m) == "" {
			t.Fatalf("mode %s has no label", m)
		}
	}
}

// TestRankMetricsDeterminism verifies identical inputs produce
// identical orderings across calls.
func TestRankMetricsDeterminism(t *testing.T) {
	now := time.Now().UTC()

	cands := []RichCandidate{
		rich("z", withPing(config.PingMetrics{Samples: 4, MedianMS: 50, At: msAt(now, time.Minute)})),
		rich("y", withPing(config.PingMetrics{Samples: 4, MedianMS: 50, At: msAt(now, time.Minute)})),
		rich("x", withPing(config.PingMetrics{Samples: 4, MedianMS: 60, At: msAt(now, time.Minute)})),
	}

	a := RankMetrics(cands, SortLowestPing, now)
	b := RankMetrics(cands, SortLowestPing, now)

	for i := range a {
		if a[i].Candidate.Fingerprint != b[i].Candidate.Fingerprint {
			t.Fatalf("nondeterministic ordering at %d", i)
		}
	}

	// Equal-measurement ties break by fingerprint deterministically.
	if a[0].Candidate.Fingerprint != "y" || a[1].Candidate.Fingerprint != "z" {
		t.Fatalf("tie-break by fingerprint violated: %v", fingerprints(a))
	}
}

func fingerprints(list []RankedMetrics) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.Candidate.Fingerprint)
	}

	return out
}
