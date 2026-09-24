// quickconnect.go implements the v0.9.8.3 fresh-selection loop shared
// by Quick Connect, "find a better connection" and automatic recovery:
//
//	load recent verified successes + top ranked candidates
//	→ merge + deduplicate by stable config ID
//	→ bounded shortlist
//	→ fresh-test stale shortlist entries (bounded)
//	→ persist fresh measurements
//	→ re-rank
//	→ connect highest-ranked viable candidate
//	→ verify actual Internet through the route (connection manager)
//	→ CONNECTED only after verification succeeds
//
// The loop NEVER presents local core readiness as success: the
// connection manager's verification gate turns a candidate that
// cannot carry real traffic into an ordinary attempt failure, so the
// loop simply continues with the next freshly-ranked candidate. Every
// failure is recorded, penalized with a cooldown, and the same failed
// candidate is never retried inside one loop.
//
// Recovery reuses this exact loop (with its own exclusions) — there
// is no second recovery ranking algorithm.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/config"
	"github.com/Parsaetak/FreeIran/engine/connection"
	"github.com/Parsaetak/FreeIran/engine/ranking"
	"github.com/Parsaetak/FreeIran/engine/tester"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// Quick Connect tuning constants. Plain numbers, one place.
const (
	// qcShortlistLimit bounds the merged shortlist (~5-10 candidates
	// per the product contract).
	qcShortlistLimit = 8

	// qcRetestStaleAfter is the evidence-freshness threshold: a
	// shortlist candidate whose last measurement is older is re-tested
	// before it may be connected.
	qcRetestStaleAfter = 30 * time.Minute

	// qcRetestBudget bounds the total fresh-testing phase so Quick
	// Connect stays responsive even on slow networks.
	qcRetestBudget = 90 * time.Second

	// qcCandidateCooldown keeps a just-failed candidate out of the
	// next Quick Connect / recovery attempt (decayed lazily).
	qcCandidateCooldown = 5 * time.Minute

	// qcAttemptTimeout bounds one connect attempt (the connection
	// manager verifies Internet inside the attempt).
	qcAttemptTimeout = 60 * time.Second

	// qcOverallTimeout bounds the whole loop (testing + ranking +
	// attempts). Bounded failure, never a hang.
	qcOverallTimeout = 4 * time.Minute
)

// errNoViableCandidate is the honest Quick Connect exhaustion error.
var errNoViableCandidate = errors.New(
	"no verified connection could be established from the ranked " +
		"candidates; run \"Test connections\" and try again")

// errOnlyUntrustedCandidates reports the explicit route-trust policy
// (v0.9.8.6): candidates existed, but every one of them came from
// public untrusted sources, and the user has not opted automatic
// selection into untrusted routes.
var errOnlyUntrustedCandidates = errors.New(
	"only public untrusted nodes are available; Quick Connect does not " +
		"silently route through untrusted sources — connect one explicitly " +
		"from the Configs page or enable \"Allow public untrusted routes\" " +
		"in Settings")

// qcRecord is one candidate record with its evidence snapshot.
type qcRecord struct {
	cfg config.Config
}

// collectCandidateRecords performs ONE bounded store scan that keeps
// the full configuration records (configuration, stable ID, test
// history, LastSuccessAt, FailureStreak, TestedAt, source and trust
// state) so the fresh-selection loop can reason about evidence
// freshness.
//
// v0.9.9: the pre-0.9.9 implementation ran collectCandidates FIRST (a
// full bounded scan whose result was used only as a capacity hint)
// and then scanned the store AGAIN — twice the JSON decoding and
// chunk reads per Quick Connect, and its own scan had no bound at
// all. The single pass below carries the same candidateScanLimit
// bound as every other candidate scan.
func (a *App) collectCandidateRecords(ctx context.Context) []qcRecord {
	records := make([]qcRecord, 0, 64)

	err := a.store.Iterate(ctx, func(key string, raw []byte) error {
		if len(records) >= candidateScanLimit {
			return errCandidateLimit
		}

		var cfg config.Config

		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil // undecodable record: skip, never abort the scan
		}

		if cfg.ID == "" {
			cfg.ID = key
		}

		records = append(records, qcRecord{cfg: cfg})

		return nil
	})

	if err != nil && !errors.Is(err, errCandidateLimit) && ctx.Err() == nil {
		a.logger.Warn("connection", "quick_connect_scan",
			"candidate record scan ended early: %v", err)
	}

	return records
}

// trustedRoute reports whether the candidate's source is trusted for
// AUTOMATIC selection (v0.9.8.6): official or user-configured.
// Public/untrusted (and legacy records with no trust stamp —
// untrusted by default) require the explicit user opt-in.
func (r qcRecord) trustedRoute() bool {
	return r.cfg.RouteTrusted()
}

// lastSuccessTime returns the candidate's last verified success.
func (r qcRecord) lastSuccessTime() time.Time {
	if r.cfg.LastSuccessAt <= 0 {
		return time.Time{}
	}

	return time.UnixMilli(r.cfg.LastSuccessAt)
}

// lastTestTime returns the candidate's most recent measurement time.
func (r qcRecord) lastTestTime() time.Time {
	if r.cfg.TestedAt <= 0 {
		return time.Time{}
	}

	return time.UnixMilli(r.cfg.TestedAt)
}

// evidenceAge classifies candidate evidence freshness (v0.9.8.3):
// fresh (≤ 30 min), recent (≤ 24 h), stale (≤ 7 d) and unknown
// (older or never measured).
func (r qcRecord) evidenceAge(now time.Time) string {
	last := r.lastTestTime()
	if last.IsZero() {
		return "unknown"
	}

	age := now.Sub(last)

	switch {
	case age <= qcRetestStaleAfter:
		return "fresh"
	case age <= 24*time.Hour:
		return "recent"
	case age <= 7*24*time.Hour:
		return "stale"
	default:
		return "unknown"
	}
}

// quickConnectLoop is the shared fresh-selection loop. exclude lists
// candidate fingerprints to skip (recovery failure memory); limits
// bounds the shortlist (0 = default).
func (a *App) quickConnectLoop(
	ctx context.Context,
	exclude []string,
	shortlist int,
) (ConnectBestResult, error) {
	if shortlist <= 0 || shortlist > qcShortlistLimit {
		shortlist = qcShortlistLimit
	}

	ctx, cancel := context.WithTimeout(ctx, qcOverallTimeout)
	defer cancel()

	started := time.Now().UTC()

	a.logger.Info("connection", "quick_connect_start",
		"fresh-selection loop started (shortlist %d)", shortlist)

	now := time.Now().UTC()

	records := a.collectCandidateRecords(ctx)

	excluded := make(map[string]struct{}, len(exclude))
	for _, fp := range exclude {
		if fp != "" {
			excluded[fp] = struct{}{}
		}
	}

	// Cooldown filter: recently failed candidates sit out (the
	// cooldown decays, so previously good configurations return).
	records = a.filterQuickConnectCooldowns(records, now, excluded)

	// Route-trust policy (v0.9.8.6): automatic selection (Quick
	// Connect / Auto / recovery candidate discovery) considers TRUSTED
	// routes only — official and user-configured sources — unless the
	// user explicitly allowed public untrusted routes. Public nodes
	// stay fully reachable through EXPLICIT selection; this filter is
	// the boundary that keeps untrusted routes out of the automatic
	// path without removing the autonomous discovery architecture.
	allowPublic := a.currentSettings().AllowUntrustedPublicRoutes

	var untrusted int

	trusted := make([]qcRecord, 0, len(records))

	for _, rec := range records {
		if rec.trustedRoute() || allowPublic {
			trusted = append(trusted, rec)

			continue
		}

		untrusted++
	}

	if len(trusted) == 0 {
		if untrusted > 0 {
			a.logger.Warn("connection", "quick_connect_untrusted_only",
				"%d candidates from untrusted public sources were excluded by the route-trust policy",
				untrusted)

			return ConnectBestResult{Candidates: len(records)},
				fmt.Errorf("%w (%d untrusted public candidates were excluded)",
					errOnlyUntrustedCandidates, untrusted)
		}

		a.logger.Warn("connection", "quick_connect_exhausted",
			"no viable candidates after cooldowns and exclusions")

		return ConnectBestResult{}, errNoViableCandidate
	}

	records = trusted

	// ---- 1. Merge recent verified successes with the ranked set ----
	shortlistRecords := a.buildQuickConnectShortlist(records, now, shortlist)

	// ---- 2. Fresh-test stale shortlist entries ---------------------
	tested := a.freshTestShortlist(ctx, shortlistRecords)

	if tested > 0 {
		a.InvalidateRankingSnapshot()

		// Reload the tested records with their fresh evidence.
		records = a.collectCandidateRecords(ctx)
		records = a.filterQuickConnectCooldowns(records, time.Now().UTC(), excluded)
	}

	// ---- 3. Re-rank the shortlist from the refreshed evidence ------
	ranked := a.rankQuickConnectShortlist(records, shortlistRecords, time.Now().UTC())

	// ---- 4. Connect + verify loop ----------------------------------
	attempted := 0

	var lastErr error

	for _, rec := range ranked {
		if ctx.Err() != nil {
			break
		}

		if _, skip := excluded[rec.cfg.ID]; skip {
			continue
		}

		attempted++

		snapshot, err := a.connectVerified(ctx, rec.cfg)
		if err == nil {
			a.metricsR.AddCoreSelection()

			a.logger.Log(correlatedRecord(logging.LevelInfo, "connection",
				"quick_connect_verified",
				fmt.Sprintf("verified connection via %s on %s (core ready in %d ms, verified in %d ms)",
					snapshot.Core, snapshot.Endpoint, snapshot.CoreReadyMS,
					time.Since(started).Milliseconds()),
				snapshot))

			return ConnectBestResult{
				Snapshot:   snapshot,
				Chosen:     quickConnectView(rec),
				Candidates: len(records),
			}, nil
		}

		lastErr = err

		// LEARN: penalize/cool down the exact failed candidate so the
		// same one is never restarted inside this or the next loop.
		a.recordQuickConnectFailure(rec.cfg.ID, err)

		a.logger.Warn("connection", "quick_connect_candidate_failed",
			"candidate %s failed (%v); continuing with the next ranked candidate",
			rec.cfg.ID, err)
	}

	reason := "no candidate verified"
	if lastErr != nil {
		reason = fmt.Sprintf("no candidate verified; last failure: %v", lastErr)
	}

	a.logger.Warn("connection", "quick_connect_exhausted",
		"fresh-selection loop exhausted after %d attempts (%s)", attempted, reason)

	return ConnectBestResult{
		Candidates: len(records),
	}, fmt.Errorf("%w (%s)", errNoViableCandidate, reason)
}

// connectVerified runs one bounded connect attempt through the
// standard state machine. The manager's verification gate (v0.9.8.3)
// turns a route that cannot carry real traffic into an error, so a
// nil error means verified Internet.
func (a *App) connectVerified(ctx context.Context, cfg config.Config) (connection.Snapshot, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, qcAttemptTimeout)
	defer cancel()

	snapshot, err := a.connMgr.Connect(attemptCtx, cfg, corePreferences(a.currentSettings()))
	if err != nil {
		return snapshot, err
	}

	// Verification-disabled harnesses (tests): the manager's skip
	// flag governs — do not double-verify here.
	if a.opts.SkipConnectVerification {
		return snapshot, nil
	}

	// The verification gate already produced connected_verified.
	if snapshot.Verification != "usable" {
		// Defensive: a manager built with verification disabled must
		// never be reported as verified. Verify explicitly.
		verify, verr := a.connMgr.VerifyConnected(attemptCtx, connection.VerifyOptions{})
		if verr != nil || !verify.OK {
			_ = a.connMgr.Disconnect()

			if verr != nil {
				return snapshot, verr
			}

			return snapshot, fmt.Errorf("internet verification failed: %s", verify.Describe())
		}
	}

	return snapshot, nil
}

// buildQuickConnectShortlist merges the top recent verified successes
// with the top ranked candidates, deduplicates by stable config ID
// and bounds the result.
func (a *App) buildQuickConnectShortlist(
	records []qcRecord,
	now time.Time,
	limit int,
) []qcRecord {
	verified := make([]qcRecord, 0, len(records))
	unverified := make([]qcRecord, 0, len(records))

	for _, rec := range records {
		if rec.lastSuccessTime().IsZero() || rec.cfg.FailureStreak > 0 &&
			time.Since(rec.lastSuccessTime()) < time.Hour &&
			rec.lastTestTime().After(rec.lastSuccessTime()) {
			unverified = append(unverified, rec)

			continue
		}

		verified = append(verified, rec)
	}

	// Verified successes first, most recent first.
	sort.SliceStable(verified, func(i, j int) bool {
		return verified[i].lastSuccessTime().After(verified[j].lastSuccessTime())
	})

	// Ranked order for the rest.
	ranked := ranking.Rank(recordsToCandidates(a, unverified), now)
	byFingerprint := make(map[string]qcRecord, len(unverified))
	for _, rec := range unverified {
		byFingerprint[rec.cfg.ID] = rec
	}

	merged := make([]qcRecord, 0, limit)
	seen := make(map[string]struct{}, limit)

	add := func(rec qcRecord) {
		if rec.cfg.ID == "" {
			return
		}

		if _, dup := seen[rec.cfg.ID]; dup {
			return
		}

		if len(merged) >= limit {
			return
		}

		seen[rec.cfg.ID] = struct{}{}
		merged = append(merged, rec)
	}

	for _, rec := range verified {
		add(rec)
	}

	for _, score := range ranked {
		if rec, ok := byFingerprint[score.Fingerprint]; ok {
			add(rec)
		}
	}

	return merged
}

// qcTestWorkers is the fixed size of the fresh-testing worker pool
// (v0.9.9). Quick Connect must never spawn one goroutine per
// candidate: each in-flight test launches a REAL protocol-core
// process, so the pool bound is the bound on concurrent cores (and on
// the network/CPU load Quick Connect can cause). Three workers keep
// the retest phase of an 8-candidate shortlist within ~3 serial
// rounds without ever multiplying process launches.
const qcTestWorkers = 3

// freshTestShortlist re-measures candidates whose evidence is stale
// or missing (bounded by the retest budget AND by the fixed worker
// pool). It persists fresh measurements and returns how many records
// were refreshed. This is the SAME tester the discovery start flow
// uses — one testing system.
//
// v0.9.9: the pre-0.9.9 implementation tested the stale shortlist
// strictly sequentially; the bounded pool overlaps those launches
// without changing the per-candidate semantics (same tester, same
// persistence, same budget, deterministic cancellation — a cancelled
// ctx abandons the remaining queue).
func (a *App) freshTestShortlist(ctx context.Context, records []qcRecord) int {
	if len(records) == 0 {
		return 0
	}

	now := time.Now().UTC()

	stale := make([]config.Config, 0, len(records))

	for _, rec := range records {
		if rec.evidenceAge(now) == "fresh" {
			continue
		}

		stale = append(stale, rec.cfg)
	}

	if len(stale) == 0 {
		return 0
	}

	testCtx, cancel := context.WithTimeout(ctx, qcRetestBudget)
	defer cancel()

	mode := a.currentTestMode()
	modeTester := tester.NewModeTester(a.coreRegistry, mode.Options)

	var (
		mu     sync.Mutex
		tested int
		wg     sync.WaitGroup
	)

	queue := make(chan config.Config, len(stale))

	for _, cfg := range stale {
		queue <- cfg
	}

	close(queue)

	persist := func(cfg config.Config) {
		if raw, err := json.Marshal(&cfg); err == nil {
			if err := a.store.Upsert(cfg.Fingerprint(), raw); err == nil {
				mu.Lock()
				tested++
				mu.Unlock()
			}
		}
	}

	workers := qcTestWorkers
	if len(stale) < workers {
		workers = len(stale)
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for cfg := range queue {
				if testCtx.Err() != nil {
					continue // drain the queue; remaining work is abandoned
				}

				// v0.9.15 one-engine guard: skip fingerprints the
				// shared test queue is already testing — the queue
				// stays the single executor and last writer for that
				// config.
				if a.queueHas(cfg.Fingerprint()) {
					continue
				}

				out := modeTester.TestAndApplyMode(testCtx, &cfg)

				if out.Ping != nil || out.URLTest != nil || cfg.TestedAt != 0 {
					persist(cfg)
				}
			}
		}()
	}

	wg.Wait()

	if tested > 0 {
		a.logger.Info("connection", "quick_connect_retested",
			"fresh-tested %d of %d shortlist candidates (%d workers)",
			tested, len(stale), workers)
	}

	return tested
}

// rankQuickConnectShortlist re-ranks the refreshed records, keeping
// the shortlist order (best first) and dropping records with no
// compatible backend (never connectable).
func (a *App) rankQuickConnectShortlist(
	all []qcRecord,
	shortlist []qcRecord,
	now time.Time,
) []qcRecord {
	byID := make(map[string]qcRecord, len(all))
	for _, rec := range all {
		byID[rec.cfg.ID] = rec
	}

	refreshed := make([]qcRecord, 0, len(shortlist))

	for _, rec := range shortlist {
		if latest, ok := byID[rec.cfg.ID]; ok {
			refreshed = append(refreshed, latest)

			continue
		}

		refreshed = append(refreshed, rec)
	}

	scores := ranking.Rank(recordsToCandidates(a, refreshed), now)
	byFingerprint := make(map[string]qcRecord, len(refreshed))
	for _, rec := range refreshed {
		byFingerprint[rec.cfg.ID] = rec
	}

	out := make([]qcRecord, 0, len(refreshed))

	for _, score := range scores {
		if rec, ok := byFingerprint[score.Fingerprint]; ok {
			out = append(out, rec)
		}
	}

	return out
}

// recordsToCandidates converts app-level records into rankable
// candidates (bounded store evidence, real compatibility data).
func recordsToCandidates(a *App, records []qcRecord) []ranking.Candidate {
	candidates := make([]ranking.Candidate, 0, len(records))

	for _, rec := range records {
		candidates = append(candidates, ranking.Candidate{
			Fingerprint:        rec.cfg.ID,
			Name:               rec.cfg.Name,
			Protocol:           string(rec.cfg.Type),
			Endpoint:           rec.cfg.DisplayURL(),
			Source:             rec.cfg.Source,
			History:            rec.cfg.TestHistory,
			CompatibleBackends: a.coreRegistry.CompatibleBackends(rec.cfg.Type),
		})
	}

	return candidates
}

// quickConnectView renders the chosen record for the result view.
func quickConnectView(rec qcRecord) CandidateView {
	return CandidateView{
		Fingerprint: rec.cfg.ID,
		Name:        rec.cfg.Name,
		Protocol:    string(rec.cfg.Type),
		Endpoint:    rec.cfg.DisplayURL(),
		TestedAt:    rec.cfg.TestedAt,
		Connectable: true,
		SourceTrust: rec.cfg.SourceTrust,
	}
}

// ---- Quick Connect failure memory (shared with recovery) ----------

// recordQuickConnectFailure stores a failed candidate with the exact
// failure reason in the cooldown memory.
func (a *App) recordQuickConnectFailure(fingerprint string, err error) {
	if fingerprint == "" {
		return
	}

	a.qcMu.Lock()
	a.qcFailures[fingerprint] = time.Now().UTC()
	a.qcMu.Unlock()

	// Keep the recovery failure memory in sync so a candidate Quick
	// Connect just proved dead is not immediately re-picked.
	if a.recovery != nil {
		a.recovery.noteExternalFailure(fingerprint)
	}
}

// filterQuickConnectCooldowns drops candidates whose cooldown has not
// decayed yet (expired entries are removed lazily).
func (a *App) filterQuickConnectCooldowns(
	records []qcRecord,
	now time.Time,
	excluded map[string]struct{},
) []qcRecord {
	a.qcMu.Lock()

	for fp, at := range a.qcFailures {
		if now.Sub(at) >= qcCandidateCooldown {
			delete(a.qcFailures, fp)
		}
	}

	cooled := make(map[string]struct{}, len(a.qcFailures))
	for fp := range a.qcFailures {
		cooled[fp] = struct{}{}
	}

	a.qcMu.Unlock()

	out := make([]qcRecord, 0, len(records))

	for _, rec := range records {
		if _, skip := excluded[rec.cfg.ID]; skip {
			continue
		}

		if _, hot := cooled[rec.cfg.ID]; hot {
			continue
		}

		out = append(out, rec)
	}

	return out
}

// correlatedRecord stamps one connection-episode record with
// correlation identity (v0.9.8.3: only connection/recovery episodes
// carry event identity — ordinary records stay compact).
func correlatedRecord(level logging.Level, subsystem, event, message string, snapshot connection.Snapshot) logging.Record {
	return logging.Record{
		Level:      level,
		Subsystem:  subsystem,
		Event:      event,
		Correlate:  true,
		ConfigID:   snapshot.ConfigID,
		Core:       snapshot.Core,
		Listener:   snapshot.Endpoint,
		DurationMS: snapshot.CoreReadyMS,
		Status:     snapshot.Verification,
		Message:    message,
	}
}
