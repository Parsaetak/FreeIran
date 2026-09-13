package coremgr

import (
	"context"
	"fmt"
	"sync"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// CheckForUpdates queries the latest releases for one core and
// returns UpdateInfo. It does NOT install anything. Stable channel
// returns the latest non-prerelease; prerelease channel returns the
// latest of any kind.
//
// Implementation notes:
//   - The /releases endpoint is queried (not /releases/latest) so the
//     same call can surface both the latest stable AND the latest
//     prerelease. The manager then filters based on the configured
//     channel.
//   - GitHub rate-limits unauthenticated /releases to 60 req/hour per
//     IP. With 3 cores that is 20 cycles/hour — comfortably above the
//     user-driven "check for updates" frequency.
func (m *Manager) CheckForUpdates(ctx context.Context, name CoreName) (UpdateInfo, error) {
	src, ok := m.sources[name]
	if !ok {
		return UpdateInfo{}, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "check", "no source for core %s", name)
	}

	client := m.resolveHTTPClient()
	raw, status, err := m.httpGet(ctx, client, src.ReleaseAPI+"?per_page=20")
	if err != nil {
		return UpdateInfo{Err: err.Error()}, err
	}
	if status != 200 {
		return UpdateInfo{Err: fmt.Sprintf("HTTP %d", status)},
			firerrors.New(firerrors.KindRetryable, Subsystem, "check",
				"GitHub returned HTTP %d for %s", status, name)
	}

	releases, err := parseAllReleases(raw)
	if err != nil {
		return UpdateInfo{Err: err.Error()}, err
	}

	// Find latest stable (non-prerelease) and latest prerelease.
	var latestStable, latestPre *githubRelease
	for i := range releases {
		rel := &releases[i]
		if rel.Prerelease {
			if latestPre == nil {
				latestPre = rel
			}
			continue
		}
		if latestStable == nil {
			latestStable = rel
		}
	}

	var chosen *githubRelease
	mf, _ := m.Info(name)
	if mf.Channel == ChannelPrerelease && latestPre != nil {
		chosen = latestPre
	} else if latestStable != nil {
		chosen = latestStable
	} else if latestPre != nil {
		chosen = latestPre
	}

	if chosen == nil {
		return UpdateInfo{Err: "no releases found"},
			firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "check", "no releases for %s", name)
	}

	assetURL, assetSize := selectAsset(chosen.Assets, m.platform, src)
	info := UpdateInfo{
		Name:           name,
		CurrentVersion: mf.Version,
		LatestVersion:  stripV(chosen.TagName),
		ReleaseDate:    chosen.PublishedAt,
		ReleaseURL:     chosen.HTMLURL,
		ChangelogURL:   chosen.HTMLURL,
		AssetURL:       assetURL,
		AssetSize:      assetSize,
		CheckedAt:      time.Now().UTC(),
	}
	if latestPre != nil {
		info.LatestPrerelease = stripV(latestPre.TagName)
	}

	if info.CurrentVersion == "" {
		info.UpdateAvailable = true
	} else if compareVersions(info.LatestVersion, info.CurrentVersion) > 0 {
		info.UpdateAvailable = true
	}

	// Persist the last-checked timestamp + state transition.
	unlock := m.lock(name)
	defer unlock()

	_ = m.updateManifest(name, func(mf *Manifest) {
		mf.LastChecked = info.CheckedAt
		if info.UpdateAvailable && mf.State == StateReady {
			mf.State = StateUpdateAvailable
		}
		mf.UpdatedAt = time.Now().UTC()
	})

	return info, nil
}

// CheckAllForUpdates queries every core in parallel and returns the
// combined UpdateInfo. Errors are recorded in UpdateInfo.Err per
// core so one failing request does not block the rest.
func (m *Manager) CheckAllForUpdates(ctx context.Context) []UpdateInfo {
	results := make([]UpdateInfo, len(AllCores))
	var wg sync.WaitGroup

	for i, name := range AllCores {
		wg.Add(1)
		go func(idx int, n CoreName) {
			defer wg.Done()
			info, _ := m.CheckForUpdates(ctx, n)
			results[idx] = info
		}(i, name)
	}

	wg.Wait()
	return results
}

// Repair re-runs the smoke test; if it fails, the manager attempts to
// roll back to the retained previous version; if that also fails, it
// performs a fresh install. Repair is the user's "fix this broken
// core" button.
//
// v0.9.0 deadlock fix: the v0.8 implementation held the per-core
// mutex for the whole function AND called Rollback/HealthCheck/
// Install, which each acquire the same non-reentrant mutex — every
// Repair call deadlocked. The fix drops the outer lock: each
// sub-operation serializes itself, and a concurrent install simply
// wins (Install refuses double installs via the Installing state).
func (m *Manager) Repair(ctx context.Context, name CoreName) error {
	mf, ok := m.Info(name)
	if !ok {
		return m.Install(ctx, name)
	}

	if mf.PreviousPath == "" {
		return m.Install(ctx, name)
	}

	if err := m.Rollback(ctx, name); err == nil {
		if result, _ := m.HealthCheck(ctx, name); result.OK {
			return nil
		}
	}

	return m.Install(ctx, name)
}

// UpdateAll checks every core for updates and installs anything that
// is newer. Cores that fail to update keep their previous healthy
// binary (the install pipeline never replaces a verified binary with
// a broken download). The returned map reports the outcome per core;
// a nil error with update_available=false means already current.
func (m *Manager) UpdateAll(ctx context.Context) map[CoreName]error {
	out := make(map[CoreName]error, len(AllCores))

	for _, name := range AllCores {
		info, err := m.CheckForUpdates(ctx, name)
		if err != nil {
			out[name] = err

			continue
		}

		if !info.UpdateAvailable {
			out[name] = nil

			continue
		}

		out[name] = m.Install(ctx, name)
	}

	return out
}

// StartBackgroundUpdateChecker runs CheckAllForUpdates on an interval
// and emits a log entry when updates are available. The function
// blocks until ctx is cancelled, so it should be run as a goroutine.
func (m *Manager) StartBackgroundUpdateChecker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			infos := m.CheckAllForUpdates(ctx)
			for _, info := range infos {
				if info.Err != "" {
					continue
				}
				if info.UpdateAvailable {
					m.logger.Info(Subsystem, "update_available",
						"core %s: %s -> %s",
						info.Name, info.CurrentVersion, info.LatestVersion)
				}
			}
		}
	}
}
