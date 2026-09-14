// Package ranking implements FreeIran's deterministic connection
// ranking layer (the SCORE/SELECT stages of the autonomous
// connection engine).
//
// Scores are computed ONLY from recorded observations (the bounded
// per-config test history) plus the caller-supplied static facts
// (source reliability, backend compatibility). Nothing is invented:
// a candidate with no history is UNKNOWN, a candidate whose recent
// window is all failures is DEAD, and every score carries the
// human-readable reasons that produced it so the best candidate is
// explainable in the UI ("31 ms median", "96% recent success",
// "tested 4 min ago", "stable over the last 12 tests").
//
// Determinism: identical inputs always produce identical scores and
// an identical ordering. There is no randomness, no wall-clock
// dependence beyond the caller-supplied `now`, and ties break by
// fingerprint — so the ranking is reproducible across processes and
// safe to unit-test.
package ranking

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
)

// Class is the coarse viability bucket surfaced to the UI.
type Class string

const (
	// ClassBest: proven, fast, stable, fresh — connect with confidence.
	ClassBest Class = "best"

	// ClassGood: reliably connects; latency may be mediocre.
	ClassGood Class = "good"

	// ClassUnstable: mixed results or erratic latency — usable when
	// nothing better exists, never preferred.
	ClassUnstable Class = "unstable"

	// ClassDead: every observation in the recent window failed, or no
	// installed core can serve the configuration.
	ClassDead Class = "dead"

	// ClassUnknown: never tested, or history is too stale to trust.
	ClassUnknown Class = "unknown"
)

// Scoring weights. The success rate dominates (a connection that
// works is the only thing users care about), then latency, then
// stability, freshness and source reliability. The weights are plain
// constants — no hidden magic, no artificial precision.
const (
	weightSuccess     = 0.45
	weightLatency     = 0.25
	weightStability   = 0.15
	weightFreshness   = 0.10
	weightReliability = 0.05
)

// freshnessBands decay the score by observation age: a brilliant
// result from last week is not as trustworthy as a decent result
// from two minutes ago.
type freshnessBand struct {
	maxAge time.Duration
	factor float64
}

var freshnessBands = []freshnessBand{
	{30 * time.Minute, 1.0},
	{2 * time.Hour, 0.9},
	{6 * time.Hour, 0.8},
	{24 * time.Hour, 0.65},
	{72 * time.Hour, 0.45},
}

// staleAfter marks history too old to classify anything but UNKNOWN.
const staleAfter = 7 * 24 * time.Hour

// Candidate is one rankable configuration: its identity plus the
// actual observations recorded for it.
type Candidate struct {
	Fingerprint string
	Name        string
	Protocol    string
	Endpoint    string
	Source      string

	// History is the bounded observation ring (newest last).
	History []config.TestObservation

	// CompatibleBackends is the number of installed protocol cores
	// that can serve this configuration (0 = none). Candidates no
	// installed core can serve are not connectable — class DEAD
	// regardless of history.
	CompatibleBackends int

	// SourceReliability is the observed success fraction of the
	// source this config came from, in [0,1] (0 = unknown → neutral).
	SourceReliability float64
}

// Score is the ranking outcome for one candidate.
type Score struct {
	Fingerprint string   `json:"fingerprint"`
	Name        string   `json:"name"`
	Protocol    string   `json:"protocol"`
	Class       Class    `json:"class"`
	Score       float64  `json:"score"`
	LatencyMS   int64    `json:"latency_ms"`
	SuccessRate float64  `json:"success_rate"`
	Samples     int      `json:"samples"`
	TimeoutRate float64  `json:"timeout_rate"`
	TestedAt    int64    `json:"tested_at,omitempty"`
	Connectable bool     `json:"connectable"`
	Explanation []string `json:"explanation,omitempty"`
}

// Evaluate scores one candidate from its actual observations.
//
// The recent window is the last 8 observations: old failures should
// not outweigh a fresh trend, and 8 samples keep the arithmetic
// cheap for thousands of candidates.
func Evaluate(c Candidate, now time.Time) Score {
	score := Score{
		Fingerprint: c.Fingerprint,
		Name:        c.Name,
		Protocol:    c.Protocol,
	}

	window := recentWindow(c.History, 8)
	score.Samples = len(window)

	last := lastObservation(c.History)
	if last != nil {
		score.TestedAt = last.At
	}

	// No installed core can serve this protocol: not connectable.
	if c.CompatibleBackends == 0 {
		score.Class = ClassDead
		score.Connectable = false
		score.Explanation = append(score.Explanation,
			"no compatible protocol core installed")
		return score
	}

	score.Connectable = true

	if len(window) == 0 || last == nil {
		score.Class = ClassUnknown
		score.Explanation = append(score.Explanation, "never tested")
		return score
	}

	age := now.Sub(time.UnixMilli(last.At).UTC())
	if age > staleAfter {
		score.Class = ClassUnknown
		score.Explanation = append(score.Explanation,
			"test history is stale (older than 7 days)")
		return score
	}

	// --- success rate (recency-weighted) ---------------------------
	var weightSum, successWeight float64

	for i, obs := range window {
		w := float64(i + 1) // newest weighs most

		weightSum += w

		if obs.Working {
			successWeight += w
		}
	}

	successRate := successWeight / weightSum
	score.SuccessRate = successRate

	// --- latency + stability (working observations only) -----------
	var working []int64

	for _, obs := range window {
		if obs.Working && obs.LatencyMS > 0 {
			working = append(working, obs.LatencyMS)
		}
	}

	latencyFactor, median := latencyScore(working)
	score.LatencyMS = median

	stability := stabilityScore(working, window)

	// --- timeout pressure -------------------------------------------
	var timeouts int

	for _, obs := range window {
		if obs.TimedOut {
			timeouts++
		}
	}

	timeoutRate := 0.0

	if len(window) > 0 {
		timeoutRate = float64(timeouts) / float64(len(window))
	}

	score.TimeoutRate = timeoutRate

	// --- freshness ---------------------------------------------------
	fresh := 0.25

	for _, band := range freshnessBands {
		if age <= band.maxAge {
			fresh = band.factor
			break
		}
	}

	// --- source reliability (neutral when unknown) -------------------
	reliability := 0.5

	if c.SourceReliability > 0 {
		reliability = c.SourceReliability
	}

	composite := weightSuccess*successRate +
		weightLatency*latencyFactor +
		weightStability*stability +
		weightFreshness*fresh +
		weightReliability*reliability

	// Timeouts are the worst failure mode for auto-connection: they
	// waste a whole startup cycle before failing.
	composite *= 1 - 0.5*timeoutRate

	score.Score = composite * 100

	score.Class = classify(successRate, stability, latencyFactor, age)

	if score.Class == ClassDead {
		score.Connectable = false
	}

	score.Explanation = explain(score, median, age, len(working))

	return score
}

// classify maps the measured factors onto the viability buckets.
func classify(
	successRate, stability, latencyFactor float64,
	age time.Duration,
) Class {
	if successRate == 0 {
		return ClassDead
	}

	if successRate >= 0.85 && stability >= 0.7 &&
		latencyFactor >= 0.6 && age <= 24*time.Hour {
		return ClassBest
	}

	if successRate >= 0.6 && stability >= 0.4 {
		return ClassGood
	}

	return ClassUnstable
}

// SelectBest deterministically picks the best viable candidate:
// highest score wins; equal scores break by fingerprint so the
// result never depends on map iteration order.
//
// Viable means: connectable and not DEAD. UNKNOWN candidates are
// considered only as a last resort (never connect blind when real
// data is available).
func SelectBest(candidates []Candidate, now time.Time) (Candidate, Score, bool) {
	ranked := Rank(candidates, now)

	byFingerprint := make(map[string]Candidate, len(candidates))
	for _, c := range candidates {
		byFingerprint[c.Fingerprint] = c
	}

	for _, class := range []Class{ClassBest, ClassGood, ClassUnstable} {
		for _, score := range ranked {
			if score.Class == class && score.Connectable {
				if c, ok := byFingerprint[score.Fingerprint]; ok {
					return c, score, true
				}
			}
		}
	}

	// Last resort: an untested candidate is still better than
	// refusing to connect when the user asked for a connection.
	for _, score := range ranked {
		if score.Connectable && score.Class == ClassUnknown {
			if c, ok := byFingerprint[score.Fingerprint]; ok {
				return c, score, true
			}
		}
	}

	return Candidate{}, Score{}, false
}

// Rank scores every candidate and returns the scores sorted from
// best to worst (deterministic: score desc, then fingerprint asc).
func Rank(candidates []Candidate, now time.Time) []Score {
	scores := make([]Score, 0, len(candidates))

	for _, c := range candidates {
		scores = append(scores, Evaluate(c, now))
	}

	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].Score != scores[j].Score {
			return scores[i].Score > scores[j].Score
		}

		return scores[i].Fingerprint < scores[j].Fingerprint
	})

	return scores
}

// recentWindow returns up to n observations, newest last.
func recentWindow(history []config.TestObservation, n int) []config.TestObservation {
	if len(history) <= n {
		return history
	}

	return history[len(history)-n:]
}

// lastObservation returns the newest observation, or nil.
func lastObservation(history []config.TestObservation) *config.TestObservation {
	if len(history) == 0 {
		return nil
	}

	return &history[len(history)-1]
}

// latencyScore maps the median recent latency onto [0,1] using the
// same quality bands the tester reports (excellent ≤ 150 ms, good
// ≤ 400 ms, acceptable ≤ 800 ms, slow ≤ 2000 ms).
func latencyScore(working []int64) (factor float64, medianMS int64) {
	if len(working) == 0 {
		return 0, 0
	}

	sorted := append([]int64(nil), working...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	medianMS = sorted[len(sorted)/2]

	switch {
	case medianMS <= 150:
		factor = 1.0
	case medianMS <= 400:
		factor = 0.8
	case medianMS <= 800:
		factor = 0.6
	case medianMS <= 2000:
		factor = 0.35
	default:
		factor = 0.15
	}

	return factor, medianMS
}

// stabilityScore combines latency jitter and outcome consistency
// into [0,1]. Both halves come from real observations only.
func stabilityScore(working []int64, window []config.TestObservation) float64 {
	if len(window) == 0 {
		return 0
	}

	// Outcome consistency: 1 - normalised flip rate between
	// consecutive results (a config alternating pass/fail is the
	// definition of unstable).
	flips := 0

	for i := 1; i < len(window); i++ {
		if window[i].Working != window[i-1].Working {
			flips++
		}
	}

	consistency := 1.0

	if len(window) > 1 {
		consistency = 1 - float64(flips)/float64(len(window)-1)
	}

	// Latency steadiness: 1 - coefficient of variation, clamped.
	steadiness := 1.0

	if len(working) >= 2 {
		var sum float64

		for _, ms := range working {
			sum += float64(ms)
		}

		mean := sum / float64(len(working))

		var variance float64

		for _, ms := range working {
			d := float64(ms) - mean
			variance += d * d
		}

		variance /= float64(len(working))

		cv := 0.0

		if mean > 0 {
			cv = math.Sqrt(variance) / mean
		}

		steadiness = 1 - cv
	}

	if steadiness < 0 {
		steadiness = 0
	}

	return 0.6*consistency + 0.4*steadiness
}

// explain renders the human-readable reasons behind a score.
func explain(
	score Score,
	medianMS int64,
	age time.Duration,
	workingSamples int,
) []string {
	reasons := make([]string, 0, 5)

	if medianMS > 0 {
		reasons = append(reasons, fmt.Sprintf("%d ms median", medianMS))
	}

	reasons = append(reasons, fmt.Sprintf("%.0f%% recent success",
		score.SuccessRate*100))

	reasons = append(reasons, "tested "+humanAge(age))

	if score.Samples > 1 && score.SuccessRate >= 0.85 {
		reasons = append(reasons, fmt.Sprintf("stable over the last %d tests",
			score.Samples))
	} else if workingSamples > 0 && score.TimeoutRate > 0.2 {
		reasons = append(reasons, fmt.Sprintf("%d%% of tests timed out",
			int(score.TimeoutRate*100)))
	}

	return reasons
}

// humanAge renders a short relative age ("4 min", "2 h", "3 d").
func humanAge(age time.Duration) string {
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%d min", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%d h", int(age.Hours()))
	default:
		return fmt.Sprintf("%d d", int(age.Hours()/24))
	}
}
