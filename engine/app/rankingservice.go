// rankingservice.go implements the SELECT/CONNECT stages of the
// autonomous connection engine at the application level:
//
//	CollectBestCandidates — bounded store scan → ranked candidates
//	ConnectBest           — pick the best viable candidate and connect
//
// The heavy lifting (scoring, classes, explainability) lives in the
// pure engine/ranking package; this service only gathers REAL data
// (stored configs with their bounded test history, installed-core
// compatibility, observed source reliability) and routes the decision
// through the existing connection state machine — auto-connection
// uses exactly the same validated path as a manual connect.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/ranking"
)

// candidateScanLimit bounds how many stored configurations one
// ranking pass decodes. With ~400-byte records this caps the pass at
// a couple of megabytes — enough for very large ingests while the
// per-candidate scoring stays O(samples) with ≤ 8 samples each.
const candidateScanLimit = 4000

// minSourceSamples is the number of tested configs a source needs
// before its observed reliability (instead of the neutral value)
// feeds the ranking.
const minSourceSamples = 5

// errCandidateLimit stops the store iteration once the scan bound is
// reached (a normal condition on large datasets, never an error).
var errCandidateLimit = errors.New("candidate scan limit reached")

// rankingSnapshotTTL bounds how long a ranked candidate snapshot is
// served without a store rescan (§13: normal navigation must not
// repeatedly rescan thousands of entries). Meaningful changes — a
// persisted test result or a completed ingestion cycle — invalidate
// the snapshot eagerly; the store count guards add/remove drift.
const rankingSnapshotTTL = 45 * time.Second

// maxSnapshotViews bounds the cached view list. BestCandidates
// clamps its limit to [1, 100], so 100 cached views serve every
// request the API can express.
const maxSnapshotViews = 100

// rankSnapshot is the cached ranking outcome: the built view list
// plus its identity (build time + store size).
type rankSnapshot struct {
	builtAt    time.Time
	storeCount int
	views      []CandidateView
}

// CandidateView is the credential-free ranking outcome surfaced to
// the UI: class, score and the explanation that produced it.
type CandidateView struct {
	Fingerprint string  `json:"fingerprint"`
	Name        string  `json:"name"`
	Protocol    string  `json:"protocol"`
	Endpoint    string  `json:"endpoint"`
	Class       string  `json:"class"`
	Score       float64 `json:"score"`
	LatencyMS   int64   `json:"latency_ms"`
	// LatencyMSMeasured (v0.9.8.1): LatencyMS is a real measurement.
	// 0 ms + measured = sub-millisecond (render "< 1 ms", sort FIRST).
	LatencyMSMeasured bool     `json:"latency_ms_measured"`
	SuccessRate       float64  `json:"success_rate"`
	Samples           int      `json:"samples"`
	TestedAt          int64    `json:"tested_at,omitempty"`
	Connectable       bool     `json:"connectable"`
	Explanation       []string `json:"explanation,omitempty"`
}

// ConnectBestResult is the outcome of an automatic connection.
type ConnectBestResult struct {
	Snapshot   connection.Snapshot `json:"snapshot"`
	Chosen     CandidateView       `json:"chosen"`
	Candidates int                 `json:"candidates"`
}

// collectCandidates performs the bounded store scan and builds the
// rankable candidate set. The same pass computes per-source observed
// reliability, so the ranking input is always derived from real
// stored outcomes.
func (a *App) collectCandidates(ctx context.Context) []ranking.Candidate {
	type sourceStats struct {
		tested  int
		working int
	}

	sources := map[string]*sourceStats{}

	candidates := make([]ranking.Candidate, 0, 64)

	err := a.store.Iterate(ctx, func(key string, raw []byte) error {
		if len(candidates) >= candidateScanLimit {
			return errCandidateLimit
		}

		var cfg config.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil // undecodable record: skip, never abort the scan
		}

		if cfg.ID == "" {
			cfg.ID = key
		}

		if cfg.TestedAt != 0 {
			stats := sources[cfg.Source]

			if stats == nil {
				stats = &sourceStats{}
				sources[cfg.Source] = stats
			}

			stats.tested++

			if cfg.Working {
				stats.working++
			}
		}

		candidates = append(candidates, ranking.Candidate{
			Fingerprint:        cfg.ID,
			Name:               cfg.Name,
			Protocol:           string(cfg.Type),
			Endpoint:           cfg.DisplayURL(),
			Source:             cfg.Source,
			History:            cfg.TestHistory,
			CompatibleBackends: a.coreRegistry.CompatibleBackends(cfg.Type),
		})

		return nil
	})

	if err != nil && !errors.Is(err, errCandidateLimit) && ctx.Err() == nil {
		a.logger.Warn("ranking", "candidate_scan",
			"candidate scan ended early: %v", err)
	}

	// Attach observed source reliability (real data only: a source
	// needs a minimum sample before its fraction is trusted).
	for i := range candidates {
		stats := sources[candidates[i].Source]

		if stats == nil || stats.tested < minSourceSamples {
			continue
		}

		candidates[i].SourceReliability =
			float64(stats.working) / float64(stats.tested)
	}

	return candidates
}

// BestCandidates returns the ranked candidate list for the UI (best
// first, credential-free). The limit is clamped to [1, 100].
func (s *ConnectionService) BestCandidates(limit int) []CandidateView {
	if limit <= 0 {
		limit = 5
	}

	if limit > 100 {
		limit = 100
	}

	// Serve from the cached snapshot when fresh; rebuild at most
	// once per TTL window (or after an explicit invalidation).
	views := s.app.rankedViews()

	if len(views) > limit {
		views = views[:limit]
	}

	// Copy: the caller must never alias the cache.
	out := make([]CandidateView, len(views))
	copy(out, views)

	return out
}

// rankedViews returns the cached ranked snapshot when fresh and
// rebuilds it with one bounded store scan otherwise. Cache identity
// is (build time, store count); meaningful result changes drop the
// snapshot eagerly via InvalidateRankingSnapshot. The rebuild runs
// WITHOUT holding rankMu: concurrent callers may share the previous
// snapshot for one call instead of queueing on a blocking rescan.
func (a *App) rankedViews() []CandidateView {
	count := a.store.Count()

	a.rankMu.Lock()

	snap := a.rankSnap
	if snap != nil &&
		time.Since(snap.builtAt) < rankingSnapshotTTL &&
		snap.storeCount == count {
		views := snap.views
		a.rankMu.Unlock()

		return views
	}

	a.rankMu.Unlock()

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	candidates := a.collectCandidates(ctx)
	scores := ranking.Rank(candidates, time.Now().UTC())

	byFingerprint := make(map[string]ranking.Candidate, len(candidates))
	for _, c := range candidates {
		byFingerprint[c.Fingerprint] = c
	}

	views := make([]CandidateView, 0, min(len(scores), maxSnapshotViews))

	for _, score := range scores {
		if len(views) >= maxSnapshotViews {
			break
		}

		cand := byFingerprint[score.Fingerprint]

		views = append(views, CandidateView{
			Fingerprint:       score.Fingerprint,
			Name:              cand.Name,
			Protocol:          cand.Protocol,
			Endpoint:          cand.Endpoint,
			Class:             string(score.Class),
			Score:             score.Score,
			LatencyMS:         score.LatencyMS,
			LatencyMSMeasured: score.LatencyMSMeasured,
			SuccessRate:       score.SuccessRate,
			Samples:           score.Samples,
			TestedAt:          score.TestedAt,
			Connectable:       score.Connectable,
			Explanation:       score.Explanation,
		})
	}

	a.rankMu.Lock()
	a.rankSnap = &rankSnapshot{
		builtAt:    time.Now().UTC(),
		storeCount: count,
		views:      views,
	}
	a.rankMu.Unlock()

	return views
}

// InvalidateRankingSnapshot drops the cached ranking snapshot so
// the next BestCandidates call rebuilds from current data. Called
// when a test result persists and when an ingestion cycle finishes
// — the two meaningful ranking-input changes.
func (a *App) InvalidateRankingSnapshot() {
	a.rankMu.Lock()
	a.rankSnap = nil
	a.rankMu.Unlock()
}

// ConnectBest implements the automatic connection path: rank every// ConnectBest implements the automatic connection path (v0.9.8.3):
// the fresh-selection loop merges recent verified successes with the
// top ranked candidates, fresh-tests stale evidence, re-ranks and
// connects with an Internet-verification gate — the exact same loop
// recovery uses. A candidate that fails start/readiness/verification
// is cooled down and the next freshly-ranked candidate runs; the call
// finishes with a verified connection, an explicit failure reason or
// a bounded exhaustion — never a mere "core ready".
func (s *ConnectionService) ConnectBest(exclude []string) (ConnectBestResult, error) {
	return s.app.quickConnectLoop(s.app.ctx, exclude, 0)
}

// candidateView renders one candidate + its score for the result.
func candidateView(c ranking.Candidate, score ranking.Score) CandidateView {
	return CandidateView{
		Fingerprint:       c.Fingerprint,
		Name:              c.Name,
		Protocol:          c.Protocol,
		Endpoint:          c.Endpoint,
		Class:             string(score.Class),
		Score:             score.Score,
		LatencyMS:         score.LatencyMS,
		LatencyMSMeasured: score.LatencyMSMeasured,
		SuccessRate:       score.SuccessRate,
		Samples:           score.Samples,
		TestedAt:          score.TestedAt,
		Connectable:       score.Connectable,
		Explanation:       score.Explanation,
	}
}

// sortCandidateViews is a small helper kept for stable test output.
func sortCandidateViews(views []CandidateView) {
	sort.SliceStable(views, func(i, j int) bool {
		return views[i].Score > views[j].Score
	})
}
