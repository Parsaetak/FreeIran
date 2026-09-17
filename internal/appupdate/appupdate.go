// Package appupdate implements the application-update CHECK stage of
// the unified artifact pipeline (v0.9.4 §16/§20/§21):
//
//	resolve trusted release → select platform asset →
//	compare versions → surface availability (+ checksum URL)
//
// It shares the pipeline SHAPE with the managed-core updater
// (engine/coremgr): one trusted release source, platform/arch asset
// selection, checksum-sidecar verification and version comparison —
// application update and core update are the same mechanism pointed
// at different release feeds. Download/stage/activate/rollback reuse
// the core manager's verified primitives (atomic.go, install.go,
// health.go) and are wired in a later phase; this stage deliberately
// performs NO download and NO side effects.
//
// Security (§30): the only trusted source is the official GitHub
// release feed of this repository; transport is TLS-verified via the
// default HTTP transport; a missing or malformed checksum sidecar is
// reported instead of skipped. No arbitrary executable URLs, no
// silent downgrades.
package appupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// TrustedRepo is the ONLY release feed the updater accepts (§30:
// trusted release source). Asset identity, platform, architecture and
// checksums are validated against it.
const TrustedRepo = "Parsaetak/FreeIran"

// DefaultReleaseAPI is the production endpoint. Tests inject their own.
const DefaultReleaseAPI = "https://api.github.com/repos/" + TrustedRepo + "/releases/latest"

// Asset describes the selected platform artifact.
type Asset struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	ChecksumURL string `json:"checksum_url"`
}

// Info is the outcome of an update check.
type Info struct {
	CurrentVersion  string    `json:"current_version"`
	LatestVersion   string    `json:"latest_version"`
	UpdateAvailable bool      `json:"update_available"`
	ReleaseTag      string    `json:"release_tag"`
	ReleaseURL      string    `json:"release_url"`
	PublishedAt     time.Time `json:"published_at"`
	Asset           *Asset    `json:"asset,omitempty"`
	CheckedAt       time.Time `json:"checked_at"`
}

// releaseFeed is the subset of the GitHub release object we consume.
type releaseFeed struct {
	TagName     string    `json:"tag_name"`
	HTMLURL     string    `json:"html_url"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// AssetName returns the expected deployment-ZIP asset name for a
// version and platform (amd64 only today, mirroring release.yml).
func AssetName(version, goos string) string {
	return fmt.Sprintf("FreeIran-v%s-%s-amd64.zip", version, goos)
}

// InstallerAssetName returns the expected Windows installer asset name.
func InstallerAssetName(version string) string {
	return fmt.Sprintf("FreeIran-Setup-v%s-windows-amd64.exe", version)
}

// assetFor selects the deployment ZIP for the running platform from
// the release feed. Unknown platforms get no asset (honest refusal).
func assetFor(rel *releaseFeed, goos string) *Asset {
	for i := range rel.Assets {
		name := rel.Assets[i].Name

		if name != AssetName(strings.TrimPrefix(rel.TagName, "v"), goos) {
			continue
		}

		u := rel.Assets[i].BrowserDownloadURL
		if u == "" {
			return nil
		}

		if _, err := url.Parse(u); err != nil {
			return nil
		}

		return &Asset{
			Name:        name,
			URL:         u,
			Size:        rel.Assets[i].Size,
			ChecksumURL: u + ".sha256",
		}
	}

	return nil
}

// IsNewer reports whether candidate is a strictly newer semantic
// version than current (numeric compare on the dotted triple; any
// non-numeric suffix falls back to string inequality). Pre-release
// tags (v1.2.3-rc.1) never outrank their release (v1.2.3).
func IsNewer(candidate, current string) bool {
	candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "v")
	current = strings.TrimPrefix(strings.TrimSpace(current), "v")

	if candidate == current {
		return false
	}

	candParts := strings.SplitN(candidate, "-", 2)
	currParts := strings.SplitN(current, "-", 2)

	candNums := strings.Split(candParts[0], ".")
	currNums := strings.Split(currParts[0], ".")

	for i := 0; i < 3; i++ {
		var c, u int

		if i < len(candNums) {
			_, _ = fmt.Sscanf(candNums[i], "%d", &c)
		}

		if i < len(currNums) {
			_, _ = fmt.Sscanf(currNums[i], "%d", &u)
		}

		if c != u {
			return c > u
		}
	}

	// Same numeric triple: a release outranks its pre-releases.
	if len(candParts) == 2 && len(currParts) == 1 {
		return false
	}

	if len(currParts) == 2 && len(candParts) == 1 {
		return true
	}

	return candParts[0] != currParts[0] || (len(candParts) == 2 && len(currParts) == 2 &&
		candParts[1] != currParts[1])
}

// Check queries the trusted release feed and returns availability for
// the running platform. apiURL is injectable for tests; the client is
// the shared production httpx policy engine (retries, backoff,
// Retry-After, bounded bodies) — the caller does NOT need to build
// its own client or timeout policy.
func Check(ctx context.Context, client httpx.Getter, apiURL, currentVersion, goos string) (Info, error) {
	if apiURL == "" {
		apiURL = DefaultReleaseAPI
	}

	if client == nil {
		client = httpx.Default()
	}

	resp, err := client.Get(ctx, apiURL, httpx.GetOptions{
		Header: map[string]string{
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
		},
		MaxBodyBytes: 4 << 20,
	})
	if err != nil {
		return Info{}, fmt.Errorf("appupdate: query release feed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("appupdate: release feed returned HTTP %d", resp.StatusCode)
	}

	var rel releaseFeed
	if err := json.Unmarshal(resp.Body, &rel); err != nil {
		return Info{}, fmt.Errorf("appupdate: decode release feed: %w", err)
	}

	if rel.TagName == "" {
		return Info{}, fmt.Errorf("appupdate: release feed has no tag")
	}

	latest := strings.TrimPrefix(rel.TagName, "v")

	info := Info{
		CurrentVersion:  currentVersion,
		LatestVersion:   latest,
		UpdateAvailable: IsNewer(latest, currentVersion),
		ReleaseTag:      rel.TagName,
		ReleaseURL:      rel.HTMLURL,
		PublishedAt:     rel.PublishedAt,
		CheckedAt:       time.Now().UTC(),
	}

	if info.UpdateAvailable && goos != "" {
		if asset := assetFor(&rel, goos); asset != nil {
			info.Asset = asset
		}
	}

	return info, nil
}

// ParseChecksumFile parses the published .sha256 sidecar content and
// returns the hex digest for the named artifact (the sidecar format
// is "<hex>  <filename>", sha256sum-compatible).
func ParseChecksumFile(content, assetName string) (string, error) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}

		if parts[1] == assetName || strings.HasSuffix(parts[1], "*"+assetName) {
			digest := strings.ToLower(parts[0])
			if len(digest) != 64 {
				return "", fmt.Errorf("appupdate: checksum digest malformed")
			}

			return digest, nil
		}
	}

	return "", fmt.Errorf("appupdate: checksum sidecar has no entry for %s", assetName)
}
