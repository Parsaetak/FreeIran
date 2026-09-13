package coremgr

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// githubAsset is the relevant subset of GitHub's release asset object.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

// githubRelease is the relevant subset of GitHub's release object.
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt time.Time     `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Body        string        `json:"body"`
	Assets      []githubAsset `json:"assets"`
}

// parseRelease parses the /releases/latest response and returns an
// UpdateInfo for the current platform.
func parseRelease(raw []byte, name CoreName, p Platform, src Source, releaseURL string) (UpdateInfo, error) {
	var rel githubRelease
	if err := json.Unmarshal(raw, &rel); err != nil {
		return UpdateInfo{}, firerrors.Wrap(err, firerrors.KindConfiguration,
			Subsystem, "release", "decode release JSON")
	}

	assetURL, assetSize := selectAsset(rel.Assets, p, src)
	if assetURL == "" {
		return UpdateInfo{}, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "release",
			"no asset for %s/%s in release %s",
			p.OS, p.Arch, rel.TagName)
	}

	return UpdateInfo{
		Name:            name,
		LatestVersion:   stripV(rel.TagName),
		ReleaseTag:      rel.TagName,
		ReleaseDate:     rel.PublishedAt,
		ReleaseURL:      rel.HTMLURL,
		ChangelogURL:    rel.HTMLURL,
		AssetURL:        assetURL,
		AssetSize:       assetSize,
		CheckedAt:       time.Now().UTC(),
		UpdateAvailable: true, // caller fills CurrentVersion to compute this
	}, nil
}

// parseFirstRelease parses /releases response (array) and returns the
// first entry, regardless of prerelease status. Used for the
// prerelease channel.
func parseFirstRelease(raw []byte, name CoreName, p Platform, src Source, releaseURL string) (UpdateInfo, error) {
	var releases []githubRelease
	if err := json.Unmarshal(raw, &releases); err != nil {
		return UpdateInfo{}, firerrors.Wrap(err, firerrors.KindConfiguration,
			Subsystem, "release", "decode releases JSON")
	}

	if len(releases) == 0 {
		return UpdateInfo{}, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "release", "no releases returned")
	}

	rel := releases[0]
	assetURL, assetSize := selectAsset(rel.Assets, p, src)
	if assetURL == "" {
		return UpdateInfo{}, firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "release",
			"no asset for %s/%s in release %s",
			p.OS, p.Arch, rel.TagName)
	}

	info := UpdateInfo{
		Name:          name,
		LatestVersion: stripV(rel.TagName),
		ReleaseTag:    rel.TagName,
		ReleaseDate:   rel.PublishedAt,
		ReleaseURL:    rel.HTMLURL,
		ChangelogURL:  rel.HTMLURL,
		AssetURL:      assetURL,
		AssetSize:     assetSize,
		CheckedAt:     time.Now().UTC(),
	}
	if rel.Prerelease {
		info.LatestPrerelease = info.LatestVersion
	}
	return info, nil
}

// parseAllReleases parses the /releases response and returns an
// ordered list of releases for use by CheckForUpdates (which needs
// both the latest stable and the latest prerelease).
func parseAllReleases(raw []byte) ([]githubRelease, error) {
	var releases []githubRelease
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindConfiguration,
			Subsystem, "release", "decode releases JSON")
	}
	return releases, nil
}

// selectAsset picks the asset that matches the current platform.
//
// v0.9.0 fix: the previous implementation required the asset name to
// contain BOTH the source's AssetPattern AND the platform hint. That
// conjunction never matched Xray/V2Ray assets ("Xray-windows-64.zip"
// contains the pattern "windows-64.zip" but not the hint
// "windows-amd64"), so Install failed with "no asset for
// windows/amd64" on the most common platform.
//
// The matcher now runs three passes and stops at the first hit:
//
//  1. pattern AND hint (most specific — e.g. sing-box
//     "sing-box-1.x.x-windows-amd64.zip" with hint windows-amd64);
//  2. pattern alone, but only when the pattern itself names the
//     target OS (e.g. "windows-64.zip" already contains "windows");
//  3. hint alone (last-resort, unchanged from v0.8).
//
// Ambiguous OS-less patterns (e.g. "64.zip") never take pass 2, so a
// wrong-OS asset cannot be selected by accident.
func selectAsset(assets []githubAsset, p Platform, src Source) (string, int64) {
	hint := AssetHintForPlatform(p)

	// Pass 1: pattern AND hint.
	for _, pattern := range src.AssetPatterns {
		for _, asset := range assets {
			name := strings.ToLower(asset.Name)
			if strings.Contains(name, strings.ToLower(pattern)) &&
				strings.Contains(name, strings.ToLower(hint)) {
				return asset.BrowserDownloadURL, asset.Size
			}
		}
	}

	// Pass 2: pattern alone when it names the target OS AND its arch
	// convention is compatible with the target arch ("64" in the
	// Xray/V2Ray naming scheme means x86_64/amd64, never arm64).
	for _, pattern := range src.AssetPatterns {
		lower := strings.ToLower(pattern)
		if !patternNamesOS(lower, p.OS) || !patternArchCompatible(lower, p.Arch) {
			continue
		}

		for _, asset := range assets {
			if strings.Contains(strings.ToLower(asset.Name), lower) {
				return asset.BrowserDownloadURL, asset.Size
			}
		}
	}

	// Pass 3: hint alone.
	for _, asset := range assets {
		if strings.Contains(strings.ToLower(asset.Name), strings.ToLower(hint)) {
			return asset.BrowserDownloadURL, asset.Size
		}
	}

	return "", 0
}

// patternArchCompatible reports whether an asset pattern's arch
// convention matches the target architecture. Legacy patterns use
// "64" for x86_64/amd64; "arm64"/"aarch64" name arm64 explicitly.
func patternArchCompatible(pattern, archName string) bool {
	hasArm64 := strings.Contains(pattern, "arm64") || strings.Contains(pattern, "aarch64")

	switch archName {
	case "arm64", "aarch64":
		return hasArm64 || (!strings.Contains(pattern, "64") && !strings.Contains(pattern, "amd64"))
	case "amd64", "x86_64", "x86-64":
		// "arm64" contains "64" — check it first.
		return !hasArm64
	default:
		return true
	}
}

// patternNamesOS reports whether an asset pattern unambiguously
// targets the given OS. Patterns that mention an arch variant of the
// wrong OS (e.g. "macos-arm64.zip" on windows) never match.
func patternNamesOS(pattern, osName string) bool {
	osTokens := map[string][]string{
		"windows": {"windows", "win32", "win64"},
		"linux":   {"linux"},
		"darwin":  {"darwin", "macos", "macosx", "osx", "mac"},
		"freebsd": {"freebsd"},
	}

	tokens, ok := osTokens[osName]
	if !ok {
		return strings.Contains(pattern, osName)
	}

	for _, token := range tokens {
		if strings.Contains(pattern, token) {
			return true
		}
	}

	return false
}

// AssetHintForPlatform returns the canonical platform hint used by
// selectAsset (the historical AssetPatternForPlatform name kept as an
// alias for compatibility with existing callers/tests).
func AssetHintForPlatform(p Platform) string {
	return AssetPatternForPlatform(p)
}

// stripV removes a leading "v" or "V" from a version tag, but only
// when the tag has at least one more character. "v" alone returns
// "" (no version digits to keep).
func stripV(tag string) string {
	tag = strings.TrimSpace(tag)
	if len(tag) > 1 && (tag[0] == 'v' || tag[0] == 'V') {
		return tag[1:]
	}
	if tag == "v" || tag == "V" {
		return ""
	}
	return tag
}

// compareVersions returns -1, 0 or 1 comparing two semver-ish
// versions. Only numeric components are compared; non-numeric
// components (e.g. "-rc1") are compared lexically.
func compareVersions(a, b string) int {
	aParts := splitVersion(a)
	bParts := splitVersion(b)
	max := len(aParts)
	if len(bParts) > max {
		max = len(bParts)
	}
	for i := 0; i < max; i++ {
		av, bv := "0", "0"
		if i < len(aParts) {
			av = aParts[i]
		}
		if i < len(bParts) {
			bv = bParts[i]
		}
		if c := compareNumericOrString(av, bv); c != 0 {
			return c
		}
	}
	return 0
}

func splitVersion(v string) []string {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	v = strings.ReplaceAll(v, "-", ".")
	return strings.Split(v, ".")
}

func compareNumericOrString(a, b string) int {
	ai, errA := atoiSafe(a)
	bi, errB := atoiSafe(b)
	if errA == nil && errB == nil {
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
		return 0
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func atoiSafe(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %s", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
