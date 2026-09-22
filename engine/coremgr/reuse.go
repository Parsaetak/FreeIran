package coremgr

import (
	"context"
	"os"
	"strings"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/system"
)

// ReuseDecision names the outcome of the v0.9.14 reuse-first decision
// (DISCOVER → IDENTIFY → VALIDATE → COMPARE → REUSE / ADOPT / UPDATE /
// ACQUIRE). It is persisted on the manifest so the UI can show WHY the
// last install call reused, adopted or downloaded — never a lossy
// "installed=true" boolean.
type ReuseDecision string

const (
	// DecisionManagedCurrent: a healthy FreeIran-managed binary is
	// already at the target version — nothing to acquire.
	DecisionManagedCurrent ReuseDecision = "managed-current"

	// DecisionManagedHealthy: a healthy managed binary exists and is
	// kept; the latest-stable check could not complete (offline), so
	// no download is started.
	DecisionManagedHealthy ReuseDecision = "managed-healthy"

	// DecisionSystemCurrent: an externally installed binary matches
	// the target version and was adopted by reference — no download.
	DecisionSystemCurrent ReuseDecision = "system-current"

	// DecisionSystemNewer: the external binary is NEWER than the
	// current stable release; it is used and never downgraded.
	DecisionSystemNewer ReuseDecision = "system-newer"

	// DecisionSystemOlder: the external binary is older than stable
	// but working; it is used, "update available" is surfaced and the
	// download is skipped unless the user explicitly forces the
	// update.
	DecisionSystemOlder ReuseDecision = "system-older-but-working"

	// DecisionNeedsUpdate: a managed binary is older than stable —
	// the explicit update acquires the newer release.
	DecisionNeedsUpdate ReuseDecision = "needs-update"

	// DecisionNeedsDownload: no suitable working local candidate
	// exists — the official stable asset is downloaded.
	DecisionNeedsDownload ReuseDecision = "needs-download"

	// DecisionInvalidLocal: a local candidate existed but failed
	// validation; the next candidate (or the download) is used.
	DecisionInvalidLocal ReuseDecision = "invalid-local"

	// DecisionNotFound: nothing usable locally, nothing downloaded
	// yet.
	DecisionNotFound ReuseDecision = "not-found"
)

// TrustLevel records HOW a binary's identity was established — the
// v0.9.14 contract separates "officially checksum-verified" from
// "locally validated" and never claims official provenance from a
// matching version string alone.
type TrustLevel string

const (
	// TrustVerified: the binary's SHA-256 matches the authoritative
	// upstream asset digest (release API digest or published sidecar).
	TrustVerified TrustLevel = "upstream-verified"

	// TrustLocal: the binary exists, probes and passes its smoke
	// test, but no authoritative digest match was available.
	TrustLocal TrustLevel = "locally-validated"
)

// reuseResult reports what the reuse phase decided.
type reuseResult struct {
	// done is true when the install call is finished: reusable work
	// was found and no download may start.
	done bool

	decision ReuseDecision
	message  string

	// update is the resolved latest-stable metadata. The reuse phase
	// resolves it ONCE per install call; the acquisition pipeline
	// reuses this result instead of querying the release API again
	// (one authoritative resolve per install — no duplicate request).
	update UpdateInfo
}

// fileIdentityMatches reports whether the on-disk file still matches
// the identity recorded in the manifest (size + mtime). Manifests
// recorded before v0.9.14 carry no identity — the caller falls back to
// full revalidation and records the identity for next time.
func fileIdentityMatches(path string, mf Manifest) bool {
	if mf.BinarySize == 0 && mf.BinaryModTime.IsZero() {
		return false
	}

	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}

	if mf.BinarySize != 0 && info.Size() != mf.BinarySize {
		return false
	}

	if !mf.BinaryModTime.IsZero() && !info.ModTime().UTC().Equal(mf.BinaryModTime.UTC()) {
		return false
	}

	return true
}

// recordIdentity stores the current file identity on the manifest so
// later ensure calls can cheaply prove nothing changed.
func recordIdentity(mf *Manifest) {
	if info, err := os.Stat(mf.BinaryPath); err == nil && !info.IsDir() {
		mf.BinarySize = info.Size()
		mf.BinaryModTime = info.ModTime().UTC()
	}
}

// activeOwnership resolves the ownership of the active binary for
// manifests recorded before the ownership field existed (v0.9.12 /
// v0.9.13): an empty value means FreeIran-managed.
func activeOwnership(mf Manifest) system.Ownership {
	if mf.Ownership == string(system.OwnershipExternal) {
		return system.OwnershipExternal
	}

	return system.OwnershipManaged
}

// invalidateDiscoveredPath drops the discovery cache entry for one
// executable when runtime evidence invalidated it (validation failure,
// vanished file) — precise invalidation, never a global flush.
func (m *Manager) invalidateDiscoveredPath(path string) {
	if loc := m.currentLocator(); loc != nil {
		loc.InvalidatePath(path)
	}
}

// ensureReusable implements the reuse-first phase of Install: before
// anything is downloaded it evaluates the existing local state and, if
// a suitable working candidate exists, reconciles the manifest and
// reports the install complete WITHOUT acquiring anything.
//
// Decision flow (v0.9.14, true local-first ordering):
//
//  1. LOCAL DISCOVERY → FILE IDENTITY → LOCAL HEALTH: a healthy,
//     identity-current active binary is reusable on LOCAL evidence
//     alone. The reuse answer is produced and returned with ZERO
//     network work — GitHub/release metadata can never veto that
//     answer and must never delay it (a machine with a valid runtime
//     stays fully usable while the remote API is unreachable,
//     poisoned or slow).
//  2. The release resolve that follows a local reuse is DETACHED
//     ENRICHMENT (spawnReuseEnrichment): it runs outside the install
//     call, bounded by reuseResolveBudget, and only refines the
//     decision label (managed-current vs managed-healthy, system-
//     current vs system-older) and the update surfaces when the
//     authority is reachable. Its failure changes nothing — the local
//     reuse stands.
//  3. When the resolve fails, the reuse decision falls back to the
//     manifest-cached target (LatestKnown) — current/newer/older
//     classification keeps working offline — and OTHER local
//     candidates are still discovered and adopted; remote metadata
//     failure never invalidates a usable local runtime.
//  4. WithForce (explicit update) bypasses the reuse gate entirely.
//  5. Otherwise (nothing reusable locally, or a managed binary older
//     than the target) the release resolve runs inline on the
//     caller's context and the acquisition pipeline proceeds — the
//     download genuinely needs remote metadata, so failing loudly
//     when offline is correct.
func (m *Manager) ensureReusable(ctx context.Context, name CoreName, src Source, preSnap Manifest, force bool) (reuseResult, UpdateInfo, error) {
	active := preSnap.BinaryPath
	activeOwned := activeOwnership(preSnap)

	activeAlive := false
	if active != "" {
		if _, err := os.Stat(active); err == nil {
			activeAlive = true
		} else if activeOwned == system.OwnershipExternal {
			// The referenced external executable disappeared: drop the
			// stale probe cache for it and rediscover alternatives
			// below instead of leaving a false "ready" state.
			m.logger.Warn(Subsystem, "external_missing",
				"core %s external executable %s no longer exists; rediscovering",
				name, active)

			if m.currentLocator() != nil {
				m.locator.InvalidatePath(active)
			}
		}
	}

	// ---- 0. Local-first reuse gate -----------------------------------
	// A healthy, identity-current active binary answers "can I use this
	// already-existing executable right now?" with YES on local
	// evidence alone (file identity + recorded health state) — the
	// answer is returned WITHOUT any network work. The release resolve
	// runs afterwards as detached, bounded enrichment: online it
	// refines the decision label and the update surfaces; offline or
	// black-holed it quietly fails and the local reuse stands. A
	// black-holed network can therefore never hold a healthy local
	// runtime hostage, not even for the enrichment budget.
	//
	// WithForce skips the gate: an explicit update request overrides
	// reuse by design (identical work is still not redownloaded —
	// see Acquire).
	if !force && activeAlive && active != "" && healthyState(preSnap.State) && fileIdentityMatches(active, preSnap) {
		res, upd, rerr := m.reuseOffline(ctx, name, src, preSnap, active, activeOwned)
		if rerr == nil && res.done {
			m.spawnReuseEnrichment(ctx, name, preSnap, activeOwned)

			return res, upd, nil
		}

		// reuseOffline could not answer from local state (the active
		// binary vanished or failed revalidation between the gate
		// check and the reuse validation): fall through to the
		// resolve-driven path below, which discovers alternatives.
	}

	// ---- 1. Resolve the target release ------------------------------
	// Reached when there is no healthy identity-current active binary
	// (nothing to reuse on local evidence — the network is required for
	// acquisition anyway) or when force requested an explicit update.
	update, err := m.checkRelease(ctx, name, preSnap.Channel)
	if err != nil {
		// Offline (or the release authority is unreachable): a healthy
		// active binary is reused rather than failing the call — the
		// application must stay usable without the remote API. The
		// offline path ALSO still discovers and adopts other local
		// candidates, which the previous revision did not (an external
		// installation could not be adopted while offline).
		if activeAlive || m.currentLocator() != nil {
			if res, _, rerr := m.reuseOffline(ctx, name, src, preSnap, active, activeOwned); rerr == nil && res.done {
				return res, UpdateInfo{}, nil
			}
		}

		return reuseResult{}, UpdateInfo{}, err
	}

	return m.reuseActiveOnline(ctx, name, src, preSnap, force, update)
}

// reuseResolveBudget bounds the DETACHED release-metadata enrichment
// that follows a local reuse decision: online it refines the decision
// label (current vs healthy) and the update surfaces; a black-holed or
// poisoned network must never hold a healthy local runtime hostage —
// and because the enrichment runs outside the install call, the
// caller's answer is bounded by LOCAL work alone. The singleflight
// flight itself keeps its own detached 30s lifetime for OTHER callers;
// only this enrichment's wait is budgeted.
const reuseResolveBudget = 8 * time.Second

// spawnReuseEnrichment runs the OPTIONAL release-metadata refinement
// of an already-decided local reuse OUTSIDE the install call: the
// caller's reuse answer was produced from local evidence alone and is
// never delayed by the network. The enrichment is detached from the
// caller's context (the install call that spawned it has already
// returned) with its own reuseResolveBudget lifetime, so a black-holed
// network cannot leak an unbounded background request. Exactly one
// resolve per reuse event, singleflight-deduplicated with any
// concurrent caller of the same endpoint; its failure changes nothing.
func (m *Manager) spawnReuseEnrichment(ctx context.Context, name CoreName, preSnap Manifest, activeOwned system.Ownership) {
	enrichCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reuseResolveBudget)

	// enrichWG makes the detached enrichment's completion OBSERVABLE
	// (deterministic tests, future close-ordering) instead of
	// something callers have to poll for.
	m.enrichWG.Add(1)

	go func() {
		defer m.enrichWG.Done()
		defer cancel()

		update, err := m.checkRelease(enrichCtx, name, preSnap.Channel)
		if err != nil {
			// Offline / too slow / rate-limited: the local reuse
			// decision (cached-target classification) stands.
			m.logger.Info(Subsystem, "reuse_enrichment_skipped",
				"core %s: release metadata unavailable after local reuse; local decision stands (%v)", name, err)

			return
		}

		m.applyReuseEnrichment(name, preSnap, activeOwned, update)
	}()
}

// applyReuseEnrichment folds fresh release metadata into the manifest
// of a REUSED runtime: latest-known version/tag/size, the refreshed
// decision label and update availability. It never touches health
// evidence (no new local validation ran) and it no-ops when the
// manifest moved on (a concurrent install/uninstall owns it now) —
// the enrichment only ever refines the SAME healthy runtime it was
// spawned for.
func (m *Manager) applyReuseEnrichment(name CoreName, preSnap Manifest, activeOwned system.Ownership, update UpdateInfo) {
	target := ExtractVersionToken(update.LatestVersion)
	activeVersion := ExtractVersionToken(preSnap.Version)

	decision := DecisionManagedHealthy
	nextState := preSnap.State
	note := ""

	if target != "" && activeVersion != "" {
		switch cmp := compareVersions(activeVersion, target); {
		case cmp == 0:
			decision = DecisionManagedCurrent

			if activeOwned == system.OwnershipExternal {
				decision = DecisionSystemCurrent
			}

		case cmp > 0:
			decision = DecisionSystemNewer
			note = "newer than last known stable; automatic downgrade refused"

		case cmp < 0 && activeOwned == system.OwnershipExternal:
			decision = DecisionSystemOlder
			nextState = StateUpdateAvailable

		default: // cmp < 0, managed: the update is acquirable online
			decision = DecisionManagedHealthy
			nextState = StateUpdateAvailable
			note = "update available: " + update.LatestVersion
		}
	}

	if err := m.updateManifest(name, func(mf *Manifest) {
		if mf.BinaryPath != preSnap.BinaryPath || mf.Version != preSnap.Version || !healthyState(mf.State) {
			return // a concurrent install/uninstall owns the manifest now
		}

		mf.State = nextState
		mf.LastChecked = update.CheckedAt
		mf.LatestKnown = update.LatestVersion
		mf.LatestTag = update.ReleaseTag
		mf.LatestAssetSize = update.AssetSize
		mf.StatusNote = note
		mf.LastDecision = string(decision)
		mf.UpdatedAt = time.Now().UTC()
	}); err != nil {
		m.logger.Warn(Subsystem, "persist_failed",
			"could not persist reuse enrichment for %s: %v", name, err)
	}
}

// reuseOffline answers the reuse question from LOCAL evidence only —
// the restrictive-network path (offline, DNS failure, poisoned
// resolver, GitHub unreachable, unreachable enrichment). It never
// touches the network:
//
//   - a healthy active binary (identity-current: no re-probe; stale
//     identity: one local smoke validation) is REUSED — never
//     downgraded, never invalidated by missing remote metadata;
//   - classification uses the manifest-cached target (LatestKnown):
//     current → managed-current/system-current, newer → system-newer,
//     older external → system-older (retained, update surfaced),
//     older managed → kept under managed-healthy (an offline machine
//     cannot acquire the update, so the working binary stays);
//   - without a cached target the honest "kept, release check
//     unavailable" decision (managed-healthy) is used;
//   - no healthy active binary: OTHER local candidates are still
//     discovered and adopted (the v0.9.14 initial revision failed the
//     whole call here — an externally installed core was unusable
//     offline);
//   - nothing usable locally: the caller's resolve error is returned.
func (m *Manager) reuseOffline(
	ctx context.Context,
	name CoreName,
	src Source,
	preSnap Manifest,
	active string,
	activeOwned system.Ownership,
) (reuseResult, UpdateInfo, error) {
	// Synthetic update metadata from the manifest cache: preserves the
	// update surfaces (LatestKnown/tag/size) without any network call.
	cachedTarget := ExtractVersionToken(preSnap.LatestKnown)

	offlineUpdate := UpdateInfo{
		Name:          name,
		LatestVersion: preSnap.LatestKnown,
		ReleaseTag:    preSnap.LatestTag,
		AssetSize:     preSnap.LatestAssetSize,
	}

	if active != "" {
		if _, err := os.Stat(active); err == nil && healthyState(preSnap.State) {
			identityOK := fileIdentityMatches(active, preSnap)

			activeVersion := ExtractVersionToken(preSnap.Version)

			cmp := 0
			knownTarget := false

			if activeVersion != "" && cachedTarget != "" {
				cmp = compareVersions(activeVersion, cachedTarget)
				knownTarget = true
			}

			// Validate only when the identity evidence is stale — the
			// same discipline as the online fast path.
			res := HealthResult{OK: identityOK}
			if !identityOK {
				res = m.smokeTest(ctx, name, active, src)
			}

			if res.OK {
				decision := DecisionManagedHealthy
				note := "existing binary kept (release check unavailable)"
				nextState := StateReady

				switch {
				case knownTarget && cmp == 0:
					decision = DecisionManagedCurrent
					if activeOwned == system.OwnershipExternal {
						decision = DecisionSystemCurrent
					}
					note = "current binary already present; release metadata unreachable"

				case knownTarget && cmp > 0:
					decision = DecisionSystemNewer
					note = "newer than last known stable; automatic downgrade refused"

				case knownTarget && cmp < 0 && activeOwned == system.OwnershipExternal:
					decision = DecisionSystemOlder
					note = "working external binary retained; update available"
					nextState = StateUpdateAvailable

				case knownTarget && cmp < 0:
					// Managed older-than-known-target offline: the update
					// cannot be acquired, so the working binary is kept.
					decision = DecisionManagedHealthy
					note = "update known but not acquirable offline; existing binary kept"
				}

				if err := m.updateManifest(name, func(mf *Manifest) {
					mf.State = nextState
					mf.LastChecked = time.Now().UTC()
					mf.LatestKnown = preSnap.LatestKnown
					mf.LatestTag = preSnap.LatestTag
					mf.LatestAssetSize = preSnap.LatestAssetSize
					mf.StatusNote = ""
					if decision == DecisionSystemNewer {
						mf.StatusNote = "newer than last known stable; automatic downgrade refused"
					}
					if res.CheckedAt.IsZero() {
						mf.LastHealthCheck = time.Now().UTC()
					} else {
						mf.LastHealthCheck = res.CheckedAt
					}
					mf.LastHealthResult = res
					mf.LastDecision = string(decision)
					if identityOK {
						recordIdentity(mf)
					}
					mf.UpdatedAt = time.Now().UTC()
				}); err != nil {
					m.logger.Warn(Subsystem, "persist_failed",
						"could not persist offline reuse manifest for %s: %v", name, err)
				}

				m.logger.Info(Subsystem, "core_reused",
					"core %s reused %s v%s offline; %s",
					name, preSnap.OwnershipLabel(), preSnap.Version, note)

				return reuseResult{
					done:     true,
					decision: decision,
					message:  note,
				}, offlineUpdate, nil
			}

			// The active binary no longer works: invalidate and fall
			// through to discovery below.
			m.logger.Warn(Subsystem, "artifact_invalid",
				"core %s active binary failed offline revalidation at %s", name, active)

			if m.currentLocator() != nil {
				m.locator.InvalidatePath(active)
			}
		}
	}

	// Nothing healthy active: discover and adopt another local
	// candidate against the cached target (pure local operation).
	if m.currentLocator() != nil {
		tried := map[string]bool{active: true}

		if _, decision, ok := m.adoptCandidate(ctx, name, src, offlineUpdate, cachedTarget, tried, false); ok {
			return reuseResult{
				done:     true,
				decision: decision,
				message:  "local candidate adopted offline; download skipped",
			}, offlineUpdate, nil
		}
	}

	return reuseResult{}, UpdateInfo{}, firerrors.New(firerrors.KindDependencyUnavailable,
		Subsystem, "reuse",
		"no usable local runtime for core %s and release metadata unavailable", name)
}

// reuseActiveOnline is the release-metadata-informed reuse evaluation
// of the ACTIVE binary plus the discovery/adoption fallback — the
// v0.9.14 flow, entered only after the local-first gate has either
// reused offline or confirmed there is no locally reusable evidence.
func (m *Manager) reuseActiveOnline(ctx context.Context, name CoreName, src Source, preSnap Manifest, force bool, update UpdateInfo) (reuseResult, UpdateInfo, error) {
	active := preSnap.BinaryPath
	activeOwned := activeOwnership(preSnap)

	activeAlive := false
	if active != "" {
		if _, err := os.Stat(active); err == nil {
			activeAlive = true
		}
	}

	target := ExtractVersionToken(update.LatestVersion)

	// ---- 2. Evaluate the active binary ------------------------------
	tried := make(map[string]bool)

	if activeAlive && active != "" {
		tried[active] = true

		activeVersion := ExtractVersionToken(preSnap.Version)
		cmp := 1 // unknown version → treat as "at least as new" only after validation

		if activeVersion != "" && target != "" {
			cmp = compareVersions(activeVersion, target)
		}

		identityOK := fileIdentityMatches(active, preSnap)

		needsRevalidation := !identityOK || !healthyState(preSnap.State)

		switch {
		case cmp == 0:
			if !needsRevalidation {
				_ = m.updateManifest(name, func(mf *Manifest) {
					mf.State = StateReady
					mf.LastChecked = time.Now().UTC()
					mf.LatestKnown = update.LatestVersion
					mf.LatestTag = update.ReleaseTag
					mf.LatestAssetSize = update.AssetSize
					mf.StatusNote = ""
					mf.LastDecision = string(DecisionManagedCurrent)
					if activeOwned == system.OwnershipExternal {
						mf.LastDecision = string(DecisionSystemCurrent)
					}
					mf.UpdatedAt = time.Now().UTC()
				})

				m.logger.Info(Subsystem, "core_reused",
					"core %s reused %s v%s; matching verified/current binary already present",
					name, preSnap.OwnershipLabel(), preSnap.Version)
				m.logger.Info(Subsystem, "core_download_skipped",
					"core %s download skipped; current %s binary already present",
					name, preSnap.OwnershipLabel())

				return reuseResult{
					done:     true,
					decision: DecisionManagedCurrent,
					message:  "current binary already present; download skipped",
					update:   update,
				}, update, nil
			}

			// Identity changed or health evidence is stale: revalidate
			// before reusing.
			if res := m.smokeTest(ctx, name, active, src); res.OK {
				_ = m.updateManifest(name, func(mf *Manifest) {
					mf.State = StateReady
					mf.LastChecked = time.Now().UTC()
					mf.LatestKnown = update.LatestVersion
					mf.LatestTag = update.ReleaseTag
					mf.LatestAssetSize = update.AssetSize
					mf.StatusNote = ""
					mf.LastHealthCheck = time.Now().UTC()
					mf.LastHealthResult = res
					mf.LastDecision = string(DecisionManagedCurrent)
					if activeOwned == system.OwnershipExternal {
						mf.LastDecision = string(DecisionSystemCurrent)
					}
					recordIdentity(mf)
					mf.UpdatedAt = time.Now().UTC()
				})

				m.logger.Info(Subsystem, "core_reused",
					"core %s reused %s v%s after revalidation",
					name, preSnap.OwnershipLabel(), preSnap.Version)

				return reuseResult{
					done:     true,
					decision: DecisionManagedCurrent,
					message:  "current binary revalidated; download skipped",
					update:   update,
				}, update, nil
			}

			// The current binary no longer works: invalidate and look
			// for another candidate below.
			m.logger.Warn(Subsystem, "artifact_invalid",
				"core %s active binary failed revalidation at %s", name, active)

			if m.currentLocator() != nil {
				m.locator.InvalidatePath(active)
			}

		case cmp > 0:
			// NEWER than stable: never downgrade automatically.
			if res := m.smokeTest(ctx, name, active, src); res.OK {
				note := "newer than stable; automatic downgrade refused"

				_ = m.updateManifest(name, func(mf *Manifest) {
					mf.State = StateReady
					mf.LastChecked = time.Now().UTC()
					mf.LatestKnown = update.LatestVersion
					mf.LatestTag = update.ReleaseTag
					mf.LatestAssetSize = update.AssetSize
					mf.StatusNote = note
					mf.LastHealthCheck = time.Now().UTC()
					mf.LastHealthResult = res
					mf.LastDecision = string(DecisionSystemNewer)
					recordIdentity(mf)
					mf.UpdatedAt = time.Now().UTC()
				})

				m.logger.Info(Subsystem, "core_reused",
					"core %s reused %s v%s; %s",
					name, preSnap.OwnershipLabel(), preSnap.Version, note)

				return reuseResult{
					done:     true,
					decision: DecisionSystemNewer,
					message:  note,
					update:   update,
				}, update, nil
			}

			m.logger.Warn(Subsystem, "artifact_invalid",
				"core %s newer-than-stable binary failed validation at %s", name, active)

			if m.currentLocator() != nil {
				m.locator.InvalidatePath(active)
			}

		default: // cmp < 0 — older than stable
			// An externally owned, working binary is USED and the
			// update is surfaced — functional installed binary beats
			// an unnecessary download. A managed older binary follows
			// the explicit update semantics (acquisition below).
			if activeOwned == system.OwnershipExternal && !force {
				if res := m.smokeTest(ctx, name, active, src); res.OK {
					_ = m.updateManifest(name, func(mf *Manifest) {
						mf.State = StateUpdateAvailable
						mf.LastChecked = time.Now().UTC()
						mf.LatestKnown = update.LatestVersion
						mf.LatestTag = update.ReleaseTag
						mf.LatestAssetSize = update.AssetSize
						mf.StatusNote = ""
						mf.LastHealthCheck = time.Now().UTC()
						mf.LastHealthResult = res
						mf.LastDecision = string(DecisionSystemOlder)
						recordIdentity(mf)
						mf.UpdatedAt = time.Now().UTC()
					})

					m.logger.Info(Subsystem, "core_reused",
						"core %s reused external v%s; update available v%s; existing binary retained",
						name, preSnap.Version, update.LatestVersion)

					return reuseResult{
						done:     true,
						decision: DecisionSystemOlder,
						message:  "working external binary retained; update available",
						update:   update,
					}, update, nil
				}

				m.logger.Warn(Subsystem, "artifact_invalid",
					"core %s external binary failed validation at %s", name, active)

				if m.currentLocator() != nil {
					m.locator.InvalidatePath(active)
				}
			}
		}
	}

	// ---- 3. Discover and adopt another local candidate ---------------
	// One discovery authority: the shared locator. No subsystem walks
	// PATH or installation directories independently.
	if m.currentLocator() != nil {
		if _, decision, ok := m.adoptCandidate(ctx, name, src, update, target, tried, force); ok {
			return reuseResult{
				done:     true,
				decision: decision,
				message:  "local candidate adopted; download skipped",
				update:   update,
			}, update, nil
		}
	}

	// ---- 4. Nothing reusable: acquire --------------------------------
	decision := DecisionNeedsDownload
	if activeAlive && activeOwned == system.OwnershipManaged {
		decision = DecisionNeedsUpdate
	}

	return reuseResult{decision: decision, update: update}, update, nil
}

// adoptCandidate walks the discovered candidates in deterministic
// order (managed → PATH → system, validated first) and adopts the best
// one that passes the same validation the install pipeline demands:
// version probe, config-dialect validation and smoke test. Adopting an
// external binary only RECORDS a reference — the file is never copied,
// moved, renamed or deleted.
func (m *Manager) adoptCandidate(
	ctx context.Context,
	name CoreName,
	src Source,
	update UpdateInfo,
	target string,
	tried map[string]bool,
	force bool,
) (system.ExecCandidate, ReuseDecision, bool) {
	candidates := m.currentLocator().DiscoverCandidates(ctx, string(name))
	if len(candidates) == 0 {
		return system.ExecCandidate{}, DecisionNotFound, false
	}

	m.logger.Info(Subsystem, "core_discovered",
		"core %s: %d local candidate(s) discovered", name, len(candidates))

	// OFFLINE / NO-TARGET ADOPTION (v0.9.14 local-first revision): when
	// no target version is known (never resolved, or resolved before
	// the manifest existed), the classification passes below cannot
	// run — but a VALIDATED, smoke-passing candidate is still adopted
	// instead of failing the call. A working local runtime beats
	// "nothing" under restrictive networks; provenance is recorded
	// honestly (locally-validated trust, empty update surfaces).
	if target == "" && !force {
		for _, cand := range candidates {
			if !cand.Validated() || tried[cand.Path] {
				continue
			}

			if err := m.validateExecutable(ctx, name, cand.Path, src); err != nil {
				tried[cand.Path] = true

				continue
			}

			res := m.smokeTest(ctx, name, cand.Path, src)
			tried[cand.Path] = true

			if !res.OK {
				continue
			}

			sha, _ := fileSHA256(cand.Path)

			ownership := string(system.OwnershipExternal)
			if cand.Managed() {
				ownership = string(system.OwnershipManaged)
			}

			if err := m.updateManifest(name, func(mf *Manifest) {
				mf.State = StateReady
				mf.Version = cand.Version
				mf.Ownership = ownership
				mf.Origin = string(cand.Origin)
				mf.BinaryPath = cand.Path
				if ownership == string(system.OwnershipExternal) {
					mf.ExternalPath = cand.Path
				}
				mf.ChecksumSHA256 = sha
				mf.Trust = string(TrustLocal)
				mf.BinarySize = cand.Size
				mf.BinaryModTime = cand.ModTime
				mf.SourceURL = "local:" + string(cand.Origin)
				mf.LastChecked = time.Now().UTC()
				mf.LastHealthCheck = time.Now().UTC()
				mf.LastHealthResult = res
				mf.FailureReason = ""
				mf.FailureStage = ""
				mf.StatusNote = ""
				mf.LastDecision = string(DecisionManagedHealthy)
				mf.UpdatedAt = time.Now().UTC()
			}); err != nil {
				m.logger.Warn(Subsystem, "persist_failed",
					"could not persist offline adoption manifest for %s: %v", name, err)
			}

			m.logger.Info(Subsystem, "core_reused",
				"core %s adopted %s %s v%s with no release target known (%s); no download required",
				name, ownership, cand.Origin, cand.Version, TrustLocal)

			return cand, DecisionManagedHealthy, true
		}

		return system.ExecCandidate{}, DecisionInvalidLocal, false
	}

	// Rank per the v0.9.14 evidence ordering: an exact current binary
	// before a newer one, a newer one before an older one; ties keep
	// the discovery order (managed before external).
	passes := []struct {
		want     int // compareVersions(candidate, target)
		decision ReuseDecision
		allow    bool
	}{
		{want: 0, decision: DecisionSystemCurrent, allow: true},
		{want: 1, decision: DecisionSystemNewer, allow: true},
		{want: -1, decision: DecisionSystemOlder, allow: !force},
	}

	for _, pass := range passes {
		for _, cand := range candidates {
			if !cand.Validated() {
				continue // a binary with no version evidence is not a candidate
			}

			if tried[cand.Path] {
				continue // already evaluated as the active binary
			}

			candVersion := ExtractVersionToken(cand.Version)
			if candVersion == "" || target == "" {
				continue
			}

			if compareVersions(candVersion, target) != pass.want {
				continue
			}

			// Capability correctness: the candidate must pass the SAME
			// validation pipeline a freshly downloaded binary passes
			// (config dialect, minimum version, launch, smoke test) —
			// never bypass the existing core adapter.
			if err := m.validateExecutable(ctx, name, cand.Path, src); err != nil {
				tried[cand.Path] = true

				continue
			}

			res := m.smokeTest(ctx, name, cand.Path, src)
			tried[cand.Path] = true

			if !res.OK {
				continue
			}

			// Compute the local digest and establish the trust level:
			// an authoritative digest match proves official
			// provenance; anything less stays "locally validated".
			sha, _ := fileSHA256(cand.Path)

			trust := TrustLocal
			if update.AssetSHA256 != "" && strings.EqualFold(update.AssetSHA256, sha) {
				trust = TrustVerified
			}

			ownership := string(system.OwnershipExternal)
			if cand.Managed() {
				ownership = string(system.OwnershipManaged)
			}

			nextState := StateReady
			finalDecision := pass.decision

			if pass.want < 0 {
				nextState = StateUpdateAvailable
			}

			// An adopted NEWER binary must surface the honest status
			// remark — the refusal to downgrade is a decision, not a
			// failure.
			if pass.want > 0 {
				nextState = StateReady
			}

			if err := m.updateManifest(name, func(mf *Manifest) {
				mf.State = nextState
				mf.Version = cand.Version
				mf.Ownership = ownership
				mf.Origin = string(cand.Origin)
				// The active binary path is the candidate's real path. For
				// an external candidate the managed slot stays untouched:
				// the reference records where the executable lives, the
				// file itself is never copied, moved or deleted.
				mf.BinaryPath = cand.Path
				if ownership == string(system.OwnershipExternal) {
					mf.ExternalPath = cand.Path
				}
				mf.ChecksumSHA256 = sha
				mf.Trust = string(trust)
				mf.BinarySize = cand.Size
				mf.BinaryModTime = cand.ModTime
				mf.SourceURL = "local:" + string(cand.Origin)
				mf.LatestKnown = update.LatestVersion
				mf.LatestTag = update.ReleaseTag
				mf.LatestAssetSize = update.AssetSize
				mf.LastChecked = time.Now().UTC()
				mf.LastHealthCheck = time.Now().UTC()
				mf.LastHealthResult = res
				mf.FailureReason = ""
				mf.FailureStage = ""
				if pass.want > 0 {
					mf.StatusNote = "newer than stable; automatic downgrade refused"
				} else {
					mf.StatusNote = ""
				}
				mf.LastDecision = string(finalDecision)
				mf.UpdatedAt = time.Now().UTC()
			}); err != nil {
				m.logger.Warn(Subsystem, "persist_failed",
					"could not persist adoption manifest for %s: %v", name, err)
			}

			m.logger.Info(Subsystem, "core_reused",
				"core %s reused %s %s v%s (%s); no download required",
				name, ownership, cand.Origin, cand.Version, trust)

			return cand, finalDecision, true
		}
	}

	return system.ExecCandidate{}, DecisionInvalidLocal, false
}

// healthyState reports whether an install state represents a working,
// activatable core.
func healthyState(state InstallState) bool {
	switch state {
	case StateReady, StateInstalled, StateUpdateAvailable:
		return true
	default:
		return false
	}
}

// ownershipLabel renders the manifest ownership for logs ("managed"
// when the manifest predates the field).
func (mf Manifest) OwnershipLabel() string {
	if mf.Ownership == string(system.OwnershipExternal) {
		return "external"
	}

	return "managed"
}

// Acquire explicitly installs or updates a core to the latest stable
// release of its channel, bypassing only the reuse branch that keeps
// an OLDER external binary in place. Reuse of a current local binary
// and of complete staged artifacts still applies — explicit update is
// not a licence to redownload identical work.
func (m *Manager) Acquire(ctx context.Context, name CoreName) error {
	return m.Install(ctx, name, WithForce(true))
}
