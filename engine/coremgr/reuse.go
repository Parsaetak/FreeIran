package coremgr

import (
	"context"
	"os"
	"strings"
	"time"

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
// Decision flow (v0.9.14):
//
//  1. resolve the latest stable release metadata (cached + deduplicated)
//  2. evaluate the ACTIVE binary (managed or external):
//     current → validate identity/health → REUSE, no download
//     newer   → validate → REUSE, never downgrade
//     older   → external: REUSE + surface update (unless forced)
//     managed:  fall through to acquisition (explicit update)
//  3. when the resolve itself fails (offline) a healthy active binary
//     is still reused rather than failing the call
//  4. DISCOVER other local candidates (managed dirs, PATH, known
//     system locations) and ADOPT the best validated one
//  5. otherwise acquire (download), with staged-artifact reuse.
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

	// ---- 1. Resolve the target release ------------------------------
	update, err := m.checkRelease(ctx, name, preSnap.Channel)
	if err != nil {
		// Offline (or the release authority is unreachable): a healthy
		// active binary is reused rather than failing the call — the
		// application must stay usable without the remote API.
		if activeAlive && healthyState(preSnap.State) {
			_ = m.updateManifest(name, func(mf *Manifest) {
				mf.LastChecked = time.Now().UTC()
				mf.LastDecision = string(DecisionManagedHealthy)
				mf.UpdatedAt = time.Now().UTC()
			})

			m.logger.Info(Subsystem, "core_reused",
				"core %s reused %s v%s; release check unavailable, existing binary kept",
				name, preSnap.OwnershipLabel(), preSnap.Version)

			return reuseResult{
				done:     true,
				decision: DecisionManagedHealthy,
				message:  "existing binary kept (release check unavailable)",
			}, UpdateInfo{}, nil
		}

		return reuseResult{}, UpdateInfo{}, err
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
