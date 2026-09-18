package ranking

import (
	"sort"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// scores.go implements the v0.9.6 metric-separation layer (§10/§11):
// nothing collapses into one unexplained number, and every latency
// number carries its PROVENANCE — measured, estimated, unavailable
// or stale — so a ping ranking can never silently display a number
// that was not actually measured against the candidate.
//
// The layer composes with the existing Evaluate()/Rank() engine: the
// classic Score stays the compatibility surface, and MetricScores
// adds the separated, explainable dimensions the UI sorts by.

// Provenance states exactly where a displayed number comes from.
type Provenance string

const (
	// ProvenanceMeasured: the number is a real measurement of THIS
	// candidate taken by THIS application's test modes.
	ProvenanceMeasured Provenance = "measured"

	// ProvenanceEstimated: the number is derived (e.g. latency
	// implied by a working tunnel probe), NOT a dedicated ping run.
	// It is never displayed in a ping-sorted list as a ping.
	ProvenanceEstimated Provenance = "estimated"

	// ProvenanceUnavailable: no measurement exists.
	ProvenanceUnavailable Provenance = "unavailable"

	// ProvenanceStale: a measurement exists but is older than
	// MetricStaleAfter — it must not outrank fresh measurements.
	ProvenanceStale Provenance = "stale"
)

// MetricStaleAfter demotes measurements older than 30 minutes: proxy
// latency drifts with time-of-day routing, and "recently verified"
// is only meaningful against a fresh window.
const MetricStaleAfter = 30 * time.Minute

// SortMode is a user-selectable ranking order (§10).
type SortMode string

const (
	SortBestOverall      SortMode = "best_overall"
	SortLowestPing       SortMode = "lowest_ping"
	SortLowestMedianPing SortMode = "lowest_median_ping"
	SortLowestJitter     SortMode = "lowest_jitter"
	SortLowestPacketLoss SortMode = "lowest_packet_loss"
	SortBestURLResponse  SortMode = "best_url_response"
	SortHighestSuccess   SortMode = "highest_success_rate"
	SortMostStable       SortMode = "most_stable"
	SortRecentlyVerified SortMode = "recently_verified"
)

// AllSortModes is the complete, ordered list offered by the UI.
var AllSortModes = []SortMode{
	SortBestOverall,
	SortLowestPing,
	SortLowestMedianPing,
	SortLowestJitter,
	SortLowestPacketLoss,
	SortBestURLResponse,
	SortHighestSuccess,
	SortMostStable,
	SortRecentlyVerified,
}

// SortModeLabel returns the human label of a sort mode.
func SortModeLabel(m SortMode) string {
	switch m {
	case SortBestOverall:
		return "Best Overall"
	case SortLowestPing:
		return "Lowest Ping"
	case SortLowestMedianPing:
		return "Lowest Median Ping"
	case SortLowestJitter:
		return "Lowest Jitter"
	case SortLowestPacketLoss:
		return "Lowest Packet Loss"
	case SortBestURLResponse:
		return "Best URL Response"
	case SortHighestSuccess:
		return "Highest Success Rate"
	case SortMostStable:
		return "Most Stable"
	case SortRecentlyVerified:
		return "Recently Verified"
	default:
		return string(m)
	}
}

// MetricScores is the separated, explainable score surface (§11).
// Each dimension is in [0,1] and carries its own explanation lines;
// OverallScore is their weighted composite — but the UI always shows
// the components, so a user can see WHY a candidate ranks where it
// does.
type MetricScores struct {
	// PingScore rewards low, fresh, sample-backed TCP ping medians.
	PingScore float64 `json:"ping_score"`

	// PingProvenance states whether PingScore comes from a real ping
	// run (measured), a tunnel probe (estimated), nothing
	// (unavailable) or an old run (stale).
	PingProvenance Provenance `json:"ping_provenance"`

	// URLScore rewards fast, successful, fresh URL tests — actual
	// usable connectivity.
	URLScore      float64    `json:"url_score"`
	URLProvenance Provenance `json:"url_provenance"`

	// StabilityScore rewards low jitter and consistent history.
	StabilityScore float64 `json:"stability_score"`

	// SuccessScore is the recency-weighted working fraction.
	SuccessScore float64 `json:"success_score"`

	// FreshnessScore decays with measurement age.
	FreshnessScore float64 `json:"freshness_score"`

	// SourceScore reflects the measured reliability of the source
	// that produced the candidate (neutral when unknown).
	SourceScore float64 `json:"source_score"`

	// CompatibilityScore reflects how many installed cores can serve
	// the candidate (redundancy is resilience).
	CompatibilityScore float64 `json:"compatibility_score"`

	// OverallScore is the weighted composite of the components. A
	// candidate with brilliant ping but failed URL connectivity
	// CANNOT win: URL (usable connectivity) dominates the composite
	// exactly as the specification demands.
	OverallScore float64 `json:"overall_score"`

	// MeasuredMedianPingMS carries the raw measured number (with its
	// provenance) for ping-sorted display. NEVER an estimate.
	MeasuredMedianPingMS int64 `json:"measured_median_ping_ms"`

	// MeasuredJitterMS / MeasuredLoss carry the raw ping statistics.
	MeasuredJitterMS int64   `json:"measured_jitter_ms"`
	MeasuredLoss     float64 `json:"measured_loss"`
	MeasuredURLMS    int64   `json:"measured_url_ms"`
	LastVerifiedAt   int64   `json:"last_verified_at,omitempty"`

	// Explanation lists the human-readable reasons, in order of
	// weight, that produced the ranking.
	Explanation []string `json:"explanation,omitempty"`
}

// RichCandidate is a Candidate plus the v0.9.6 measurement fields.
type RichCandidate struct {
	Candidate

	// Ping / URLTest / Handshake are the candidate's latest mode
	// measurements (nil = not measured).
	Ping      *config.PingMetrics
	URLTest   *config.URLTestMetrics
	Handshake *config.HandshakeMetrics

	// LastSuccessAt is the last verified-usable timestamp
	// (Unix milliseconds; 0 = never).
	LastSuccessAt int64

	// FailureStreak counts consecutive failures since the last
	// success.
	FailureStreak int
}

// pingProvenance decides where a ping number comes from.
func pingProvenance(c RichCandidate, now time.Time) (Provenance, *config.PingMetrics) {
	if c.Ping != nil && c.Ping.Samples > 0 {
		if time.Duration(now.UnixMilli()-c.Ping.At)*time.Millisecond > MetricStaleAfter {
			return ProvenanceStale, c.Ping
		}

		return ProvenanceMeasured, c.Ping
	}

	// A working tunnel probe implies an end-to-end latency — an
	// ESTIMATE, never displayed as a ping. v0.9.8.1: Working implies a
	// measurement exists (rule R5); a sub-ms observation (LatencyMS
	// == 0) is just as valid an estimate basis as a positive one.
	if len(c.History) > 0 {
		last := c.History[len(c.History)-1]
		if last.Working {
			return ProvenanceEstimated, nil
		}
	}

	return ProvenanceUnavailable, nil
}

func urlProvenance(c RichCandidate, now time.Time) (Provenance, *config.URLTestMetrics) {
	if c.URLTest != nil && c.URLTest.At > 0 {
		if time.Duration(now.UnixMilli()-c.URLTest.At)*time.Millisecond > MetricStaleAfter {
			return ProvenanceStale, c.URLTest
		}

		return ProvenanceMeasured, c.URLTest
	}

	return ProvenanceUnavailable, c.URLTest
}

// EvaluateMetrics scores one rich candidate into the separated
// metric surface. All factors derive from measurements; unknown
// dimensions score neutrally (0.5) and say so.
func EvaluateMetrics(c RichCandidate, now time.Time) MetricScores {
	var m MetricScores

	// --- Ping dimension ---
	pingProv, ping := pingProvenance(c, now)
	m.PingProvenance = pingProv

	switch pingProv {
	case ProvenanceMeasured:
		m.PingScore = latencyFactorOf(ping.MedianMS)
		m.MeasuredMedianPingMS = ping.MedianMS
		m.MeasuredJitterMS = ping.JitterMS
		m.MeasuredLoss = ping.PacketLoss

		// Loss erodes the score linearly: 100% loss is 0.
		m.PingScore *= 1 - ping.PacketLoss
	case ProvenanceEstimated:
		// An estimate is better than nothing but ranks below any
		// measured candidate: neutral-low.
		m.PingScore = 0.35
	case ProvenanceStale:
		m.PingScore = 0.3
		m.MeasuredMedianPingMS = ping.MedianMS
		m.MeasuredJitterMS = ping.JitterMS
		m.MeasuredLoss = ping.PacketLoss
	default: // unavailable
		m.PingScore = 0.5 // neutral: never punished, never promoted
	}

	// --- URL dimension (usable connectivity) ---
	urlProv, url := urlProvenance(c, now)
	m.URLProvenance = urlProv

	switch urlProv {
	case ProvenanceMeasured:
		if url.OK {
			m.URLScore = latencyFactorOf(url.TotalMS)
			m.MeasuredURLMS = url.TotalMS
		} else {
			m.URLScore = 0 // measured-and-failed is the worst outcome
			m.MeasuredURLMS = url.TotalMS
		}
	case ProvenanceStale:
		m.URLScore = 0.3
		m.MeasuredURLMS = url.TotalMS
	default:
		m.URLScore = 0.5 // neutral
	}

	// --- Success dimension (recency-weighted history) ---
	window := recentWindow(c.History, 8)

	if len(window) > 0 {
		var weightSum, successWeight float64

		for i, obs := range window {
			w := float64(i + 1)
			weightSum += w

			if obs.Working {
				successWeight += w
			}
		}

		m.SuccessScore = successWeight / weightSum
	} else {
		m.SuccessScore = 0.5 // never tested: neutral
	}

	// --- Stability dimension ---
	// v0.9.8.1: working observations always carry a measurement; a
	// sub-ms (0 ms) sample is included, not dropped.
	var working []int64

	for _, obs := range window {
		if obs.Working {
			working = append(working, obs.LatencyMS)
		}
	}

	m.StabilityScore = stabilityScore(working, window)

	// Jitter refines stability when a real ping run exists.
	if pingProv == ProvenanceMeasured && ping.Samples > 1 {
		jitterFactor := latencyFactorOf(ping.JitterMS * 4) // jitter weighs 4x less than latency
		m.StabilityScore = 0.5*m.StabilityScore + 0.5*jitterFactor
	}

	// --- Freshness dimension ---
	freshAt := c.LastSuccessAt
	if freshAt == 0 && len(window) > 0 {
		freshAt = window[len(window)-1].At
	}

	m.LastVerifiedAt = freshAt

	if freshAt > 0 {
		age := time.Duration(now.UnixMilli()-freshAt) * time.Millisecond
		m.FreshnessScore = freshnessFactorOf(age)
	} else {
		m.FreshnessScore = 0
	}

	// --- Source dimension (neutral when unknown) ---
	m.SourceScore = 0.5

	if c.SourceReliability > 0 {
		m.SourceScore = c.SourceReliability
	}

	// --- Compatibility dimension ---
	switch {
	case c.CompatibleBackends >= 2:
		m.CompatibilityScore = 1
	case c.CompatibleBackends == 1:
		m.CompatibilityScore = 0.7
	default:
		m.CompatibilityScore = 0
	}

	// --- Composite: usable connectivity dominates ---
	//
	// The specification's example: a 20 ms node with failed
	// connectivity must NOT outrank a 75 ms node that consistently
	// works. URL (0.30) + Success (0.25) outweigh Ping (0.20);
	// stability, freshness, source and compatibility complete the
	// composite. A measured-and-failed URL test drives the overall
	// score below every working candidate.
	m.OverallScore = 0.30*m.URLScore +
		0.25*m.SuccessScore +
		0.20*m.PingScore +
		0.10*m.StabilityScore +
		0.08*m.FreshnessScore +
		0.04*m.SourceScore +
		0.03*m.CompatibilityScore

	m.Explanation = explainMetrics(m, c, pingProv, urlProv, ping)

	return m
}

// latencyFactorOf maps a MEASURED latency in ms onto [0,1] (lower is
// better). The bands mirror tester.QualityFor so the score matches the
// labels the UI already shows.
//
// v0.9.8.1: every caller passes a measured value — 0 ms means a
// measured sub-millisecond round trip (the best band, rule R4), not
// "unmeasured". Negative input (invalid) scores 0.
func latencyFactorOf(ms int64) float64 {
	switch {
	case ms < 0:
		return 0
	case ms <= 150:
		return 1
	case ms <= 400:
		return 0.8
	case ms <= 800:
		return 0.6
	case ms <= 2000:
		return 0.35
	default:
		return 0.15
	}
}

// freshnessFactorOf maps measurement age onto [0,1].
func freshnessFactorOf(age time.Duration) float64 {
	for _, band := range freshnessBands {
		if age <= band.maxAge {
			return band.factor
		}
	}

	return 0.2 // very old, but nonzero: history still counts for something
}

// explainMetrics builds the human-readable ranking reasons.
func explainMetrics(m MetricScores, c RichCandidate, pingProv, urlProv Provenance, ping *config.PingMetrics) []string {
	var out []string

	switch urlProv {
	case ProvenanceMeasured:
		if m.MeasuredURLMS > 0 {
			out = append(out, "URL test "+itoa(m.MeasuredURLMS)+" ms "+okLabel(m.URLScore > 0.5))
		} else {
			out = append(out, "URL test failed")
		}
	case ProvenanceStale:
		out = append(out, "URL test stale")
	default:
		out = append(out, "URL test not run")
	}

	switch pingProv {
	case ProvenanceMeasured:
		if m.MeasuredMedianPingMS > 0 {
			out = append(out, "median ping "+itoa(m.MeasuredMedianPingMS)+" ms ("+itoa(int64(ping.Samples))+" samples)")
		} else {
			// v0.9.8.1: measured sub-millisecond median — honest display.
			out = append(out, "median ping < 1 ms ("+itoa(int64(ping.Samples))+" samples)")
		}
	case ProvenanceEstimated:
		out = append(out, "latency estimated from tunnel probe")
	case ProvenanceStale:
		out = append(out, "ping measurement stale")
	default:
		out = append(out, "ping not measured")
	}

	if m.SuccessScore > 0 {
		out = append(out, "success "+pctOf(m.SuccessScore))
	}

	if ping != nil && ping.PacketLoss > 0 {
		out = append(out, "packet loss "+pctOf(ping.PacketLoss))
	}

	if c.FailureStreak >= 2 {
		out = append(out, itoa(int64(c.FailureStreak))+" consecutive failures")
	}

	return out
}

func okLabel(ok bool) string {
	if ok {
		return "ok"
	}

	return "failed"
}

func pctOf(v float64) string {
	return itoa(int64(v*100)) + "%"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}

	neg := n < 0
	if neg {
		n = -n
	}

	var buf [20]byte

	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}

// RankedMetrics pairs a rich candidate with its separated scores.
type RankedMetrics struct {
	Candidate RichCandidate
	Score     Score // classic surface (compatibility)
	Metrics   MetricScores
}

// RankMetrics evaluates every candidate and sorts by the selected
// mode. The ordering rules per mode:
//
//   - ping modes: ONLY candidates with a MEASURED, non-stale ping
//     participate in the leading partition (measured first, then
//     stale, then estimated, then unavailable — never reordered
//     within a partition by estimated numbers);
//   - URL mode: measured-and-OK candidates lead;
//   - determinism: ties break by fingerprint.
func RankMetrics(candidates []RichCandidate, mode SortMode, now time.Time) []RankedMetrics {
	out := make([]RankedMetrics, 0, len(candidates))

	for i := range candidates {
		c := candidates[i]
		out = append(out, RankedMetrics{
			Candidate: c,
			Score:     Evaluate(c.Candidate, now),
			Metrics:   EvaluateMetrics(c, now),
		})
	}

	sortModeOf(mode).sort(out)

	return out
}

type modeSorter struct {
	mode SortMode
}

func sortModeOf(m SortMode) modeSorter { return modeSorter{mode: m} }

func (s modeSorter) sort(list []RankedMetrics) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]

		// Deterministic tie-break everywhere.
		less, equal := s.cmp(a, b)
		if equal {
			return a.Candidate.Fingerprint < b.Candidate.Fingerprint
		}

		return less
	})
}

// cmp compares two ranked entries under the sort mode. The second
// return reports a tie.
func (s modeSorter) cmp(a, b RankedMetrics) (bool, bool) {
	switch s.mode {
	case SortLowestPing, SortLowestMedianPing:
		return cmpPing(a, b)

	case SortLowestJitter:
		// Only measured, fresh jitter participates.
		if aj, bj := jitterOfCandidate(a), jitterOfCandidate(b); aj >= 0 || bj >= 0 {
			if aj >= 0 && bj >= 0 {
				if aj == bj {
					return false, true
				}

				return aj < bj, false
			}

			return aj >= 0, false // measured first
		}

		return cmpOverall(a, b)

	case SortLowestPacketLoss:
		if al, bl := lossOfCandidate(a), lossOfCandidate(b); al >= 0 || bl >= 0 {
			if al >= 0 && bl >= 0 {
				if al == bl {
					return false, true
				}

				return al < bl, false
			}

			return al >= 0, false
		}

		return cmpOverall(a, b)

	case SortBestURLResponse:
		if am, bm := a.Metrics.URLProvenance, b.Metrics.URLProvenance; am == ProvenanceMeasured && bm == ProvenanceMeasured {
			// Failed URL tests sink regardless of speed.
			if (a.Metrics.URLScore > 0) != (b.Metrics.URLScore > 0) {
				return a.Metrics.URLScore > 0, false
			}

			if a.Metrics.MeasuredURLMS == b.Metrics.MeasuredURLMS {
				return false, true
			}

			return a.Metrics.MeasuredURLMS < b.Metrics.MeasuredURLMS, false
		}

		return cmpURLProvenance(a, b)

	case SortHighestSuccess:
		if a.Metrics.SuccessScore == b.Metrics.SuccessScore {
			return cmpOverall(a, b)
		}

		return a.Metrics.SuccessScore > b.Metrics.SuccessScore, false

	case SortMostStable:
		if a.Metrics.StabilityScore == b.Metrics.StabilityScore {
			return cmpOverall(a, b)
		}

		return a.Metrics.StabilityScore > b.Metrics.StabilityScore, false

	case SortRecentlyVerified:
		if a.Metrics.LastVerifiedAt == b.Metrics.LastVerifiedAt {
			return cmpOverall(a, b)
		}

		return a.Metrics.LastVerifiedAt > b.Metrics.LastVerifiedAt, false

	default: // SortBestOverall and unknown modes
		return cmpOverall(a, b)
	}
}

func cmpOverall(a, b RankedMetrics) (bool, bool) {
	if a.Metrics.OverallScore == b.Metrics.OverallScore {
		return false, true
	}

	return a.Metrics.OverallScore > b.Metrics.OverallScore, false
}

// cmpPing implements the measured-first partition rule for ping sorts
// (§10: "never substitute an estimated latency for actual measured
// latency when displaying a measured ping ranking").
func cmpPing(a, b RankedMetrics) (bool, bool) {
	ap, bp := a.Metrics.PingProvenance, b.Metrics.PingProvenance

	rank := func(p Provenance) int {
		switch p {
		case ProvenanceMeasured:
			return 0
		case ProvenanceStale:
			return 1
		case ProvenanceEstimated:
			return 2
		default:
			return 3
		}
	}

	ar, br := rank(ap), rank(bp)
	if ar != br {
		return ar < br, false
	}

	// Within the SAME provenance partition, compare the real number
	// (for measured/stale) or fall back to overall (estimated/
	// unavailable never compare fabricated numbers).
	if ap == ProvenanceMeasured || ap == ProvenanceStale {
		if a.Metrics.MeasuredMedianPingMS == b.Metrics.MeasuredMedianPingMS {
			return false, true
		}

		return a.Metrics.MeasuredMedianPingMS < b.Metrics.MeasuredMedianPingMS, false
	}

	return cmpOverall(a, b)
}

func cmpURLProvenance(a, b RankedMetrics) (bool, bool) {
	am, bm := a.Metrics.URLProvenance, b.Metrics.URLProvenance

	rank := func(p Provenance) int {
		switch p {
		case ProvenanceMeasured:
			return 0
		case ProvenanceStale:
			return 1
		default:
			return 2
		}
	}

	ar, br := rank(am), rank(bm)
	if ar != br {
		return ar < br, false
	}

	return cmpOverall(a, b)
}

func jitterOfCandidate(r RankedMetrics) int64 {
	if r.Metrics.PingProvenance != ProvenanceMeasured {
		return -1
	}

	return r.Metrics.MeasuredJitterMS
}

func lossOfCandidate(r RankedMetrics) float64 {
	if r.Metrics.PingProvenance != ProvenanceMeasured {
		return -1
	}

	return r.Metrics.MeasuredLoss
}

// SelectBestMetrics returns the top candidate under Best Overall.
func SelectBestMetrics(candidates []RichCandidate, now time.Time) (RankedMetrics, bool) {
	ranked := RankMetrics(candidates, SortBestOverall, now)
	if len(ranked) == 0 {
		return RankedMetrics{}, false
	}

	return ranked[0], true
}
