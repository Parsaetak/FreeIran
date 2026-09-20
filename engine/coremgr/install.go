package coremgr

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
	"github.com/Parsaetak/FreeIran/internal/httpx"
	"github.com/Parsaetak/FreeIran/internal/safearchive"
)

// Install downloads, verifies and activates the latest stable release
// of the core on the configured channel. It is idempotent: re-running
// it for an already-up-to-date core re-validates the active binary and
// corrects the manifest.
//
// Concurrent Install calls for the same core are deduplicated through
// a per-core singleflight: the first caller runs the pipeline, every
// concurrent caller waits and receives the same outcome. A second,
// LATER install runs normally.
//
// Transactional pipeline (the previous working core stays ACTIVE
// until the new one has passed every check):
//
//  1. setState(Installing)
//  2. RESOLVE   the latest release for the configured channel
//     (conditional, cached release lookup)
//  3. SELECT    the platform asset + verify its identity (repo,
//     platform, architecture, size) — never trusting the filename alone
//  4. DOWNLOAD  the asset into staging as a .part file (streamed,
//     resumable, stall-watched, with byte/speed/ETA telemetry)
//  5. VERIFY    the size and SHA-256 (published digest when available)
//  6. UNPACK    into staging/unpacked/
//  7. VALIDATE  version probe + minimal-config acceptance of the
//     STAGED executable
//  8. SMOKE TEST the STAGED executable (launch → listener → shutdown)
//  9. ACTIVATE  atomically: retain rollback, rename into bin/
//
// Any failure before activation leaves the previous binary active and
// untouched; a failure during activation automatically restores the
// previous binary. Staging is always cleaned up.
func (m *Manager) Install(ctx context.Context, name CoreName) error {
	// ---- Per-core singleflight ----
	m.flightMu.Lock()

	if fl, ok := m.installFlight[name]; ok {
		m.flightMu.Unlock()

		select {
		case <-fl.done:
			return fl.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	fl := &installFlight{done: make(chan struct{})}
	m.installFlight[name] = fl
	m.flightMu.Unlock()

	defer func() {
		close(fl.done)

		m.flightMu.Lock()
		delete(m.installFlight, name)
		m.flightMu.Unlock()
	}()

	fl.err = m.installCore(ctx, name)

	return fl.err
}

// installCore runs the transactional install pipeline while holding
// the per-core mutex.
func (m *Manager) installCore(ctx context.Context, name CoreName) error {
	unlock := m.lock(name)
	defer unlock()

	src, ok := m.sources[name]
	if !ok {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "install", "no source defined for %s", name)
	}

	// Snapshot the pre-install state: a healthy previous binary stays
	// active (and its state intact) when an UPDATE fails; a fresh
	// install that fails ends up Broken.
	preSnap, _ := m.snapshotManifest(name)
	hasActiveBinary := false

	if preSnap.BinaryPath != "" {
		if _, statErr := os.Stat(preSnap.BinaryPath); statErr == nil {
			hasActiveBinary = true
		}
	}

	if err := m.setState(name, StateInstalling); err != nil {
		return err
	}

	m.logger.Info(Subsystem, "install_start",
		"installing core %s from %s", name, src.Repo)

	// fail records the failure, cleans staging and — for updates over
	// a healthy core — preserves the previous core's active state.
	fail := func(stage string, ferr error) error {
		_ = os.RemoveAll(m.StagingDir(name))

		reason := HumanizeInstallFailure(stage, ferr)

		if hasActiveBinary && (preSnap.State == StateReady || preSnap.State == StateUpdateAvailable) {
			// The previous core is still active and healthy: restore
			// its state instead of marking it Broken.
			_ = m.updateManifest(name, func(mf *Manifest) {
				mf.State = preSnap.State
				mf.FailureReason = ""
				mf.FailureStage = ""
				mf.UpdatedAt = time.Now().UTC()
			})
		} else {
			_ = m.updateManifest(name, func(mf *Manifest) {
				mf.State = StateBroken
				mf.FailureReason = reason
				mf.FailureStage = stage
				mf.UpdatedAt = time.Now().UTC()
			})
		}

		emitProgress(name, StageFailed, reason, 0, 0)
		m.logger.Error(Subsystem, "install_failed", "install", string(firerrors.KindDependencyUnavailable),
			"core %s install failed at %s: %v", name, stage, ferr)

		return firerrors.Wrap(ferr, firerrors.KindDependencyUnavailable,
			Subsystem, "install", "%s: %s", name, stage)
	}

	// ---- 2. RESOLVE ------------------------------------------------
	emitProgress(name, StageResolving, "resolving latest release for channel "+string(preSnap.Channel), 0, 0)

	update, err := m.checkRelease(ctx, name, preSnap.Channel)
	if err != nil {
		return fail("resolve_release", err)
	}

	// ---- 3. SELECT + identity verification -------------------------
	emitProgress(name, StageResolving, "selected "+path.Base(update.AssetURL), 0, 0)

	if err := verifyAssetIdentity(src, m.platform, update); err != nil {
		return fail("select_asset", err)
	}

	// ---- 4. Prepare staging ----------------------------------------
	if err := os.RemoveAll(m.StagingDir(name)); err != nil {
		return fail("reset_staging", err)
	}
	if err := os.MkdirAll(m.StagingDir(name), 0o700); err != nil {
		return fail("mkdir_staging", err)
	}

	// ---- 5. DOWNLOAD (streamed, resumable, .part) -------------------
	// The staged file keeps the asset's real extension so
	// unpackArchive can pick the format.
	assetName := path.Base(update.AssetURL)
	if assetName == "" || assetName == "." || strings.Contains(assetName, "?") {
		assetName = "asset.zip"
	}

	assetPath := filepath.Join(m.StagingDir(name), assetName)

	emitProgress(name, StageDownloading, "downloading "+update.AssetURL, 0, update.AssetSize)

	dlResult, err := m.httpClient.Download(ctx, update.AssetURL, httpx.DownloadOptions{
		DestPath:     assetPath,
		ExpectedSize: update.AssetSize,
		StallTimeout: m.dlStallTimeout,
		MaxRetries:   m.dlMaxRetries,
		OnProgress: func(p httpx.Progress) {
			emitDownloadProgress(name, p)
		},
	})
	if err != nil {
		return fail("download", err)
	}

	if dlResult.Resumed {
		m.logger.Info(Subsystem, "download_resumed",
			"core %s download resumed from %s (retries=%d)",
			name, formatSize(dlResult.ResumedFrom), dlResult.Retries)
	}

	// ---- 6. VERIFY size + checksum ---------------------------------
	emitProgress(name, StageVerifying, "verifying SHA-256", dlResult.Bytes, update.AssetSize)

	actual := dlResult.SHA256
	if actual == "" {
		actual, err = fileSHA256(assetPath)
		if err != nil {
			return fail("hash", err)
		}
	}

	// Size check: the published size is part of asset identity.
	if update.AssetSize > 0 && dlResult.Bytes != update.AssetSize {
		return fail("verify_digest", fmt.Errorf(
			"downloaded size %d does not match the published size %d",
			dlResult.Bytes, update.AssetSize))
	}

	// Digest verification: prefer the API-provided digest, then the
	// published .dgst sidecar, then record the computed hash.
	if err := m.verifyAssetDigest(ctx, src, update, actual); err != nil {
		return fail("verify_digest", err)
	}

	// ---- 7. UNPACK --------------------------------------------------
	emitProgress(name, StageUnpacking, "unpacking archive", 0, 0)

	unpackedDir := filepath.Join(m.StagingDir(name), "unpacked")
	if err := os.MkdirAll(unpackedDir, 0o700); err != nil {
		return fail("mkdir_unpacked", err)
	}
	if err := unpackArchive(assetPath, unpackedDir); err != nil {
		return fail("unpack", err)
	}

	// ---- 8. VALIDATE the STAGED executable --------------------------
	emitProgress(name, StageValidating, "locating executable", 0, 0)

	execPath, err := findExecutable(unpackedDir, string(name))
	if err != nil {
		return fail("locate_executable", err)
	}

	// Ensure the executable bit BEFORE any probe runs: zip extraction
	// writes 0600, so a version probe would fail with
	// permission-denied on Unix.
	if err := ensureExecutable(execPath); err != nil {
		return fail("chmod", err)
	}

	emitProgress(name, StageValidating, "probing version", 0, 0)

	versionStr, err := m.queryVersion(ctx, execPath, src.VersionProbeArgs)
	if err != nil || versionStr == "" {
		return fail("probe_version",
			fmt.Errorf("version query returned %q (err=%v)", versionStr, err))
	}

	// Sanity-check the probed version against the release tag.
	if probeVer := ExtractVersionToken(versionStr); probeVer != "" {
		if tagVer := ExtractVersionToken(update.LatestVersion); tagVer != "" {
			if compareVersions(probeVer, tagVer) != 0 {
				return fail("probe_version", fmt.Errorf(
					"downloaded binary reports version %s but release %s expects %s",
					probeVer, update.ReleaseTag, tagVer))
			}
		}
	}

	emitProgress(name, StageValidating, "validating executable", 0, 0)

	if err := m.validateExecutable(ctx, name, execPath, src); err != nil {
		return fail("validate_executable", err)
	}

	// ---- 9. SMOKE TEST the STAGED executable ------------------------
	// The new binary proves itself BEFORE activation; the previous
	// binary remains the active one until this passes.
	emitProgress(name, StageValidating, "running smoke test", 0, 0)

	result := m.smokeTest(ctx, name, execPath, src)
	if !result.OK {
		return fail("smoke_test", fmt.Errorf(
			"smoke test failed: %s", HumanizeHealthFailure(result)))
	}

	// ---- 10. ACTIVATE atomically ------------------------------------
	emitProgress(name, StageActivating, "activating", 0, 0)

	finalPath, prevPath, prevExists, err := m.activate(name, execPath, preSnap)
	if err != nil {
		return fail("activate", err)
	}

	// ---- 11. Record the successful transaction ----------------------
	var prevChecksumStr string
	if prevExists {
		prevChecksumStr, _ = fileSHA256(prevPath)
	}

	previousVersion := preSnap.Version

	if err := m.updateManifest(name, func(mf *Manifest) {
		mf.State = StateReady
		mf.Version = versionStr
		mf.BinaryPath = finalPath
		mf.ChecksumSHA256 = actual
		mf.SourceURL = update.AssetURL
		mf.ReleaseTag = update.ReleaseTag
		mf.ReleaseDate = update.ReleaseDate
		mf.ReleaseURL = update.ReleaseURL
		mf.InstalledAt = time.Now().UTC()
		mf.LastChecked = time.Now().UTC()
		mf.UpdatedAt = time.Now().UTC()
		mf.FailureReason = ""
		mf.FailureStage = ""
		if prevExists {
			mf.PreviousChecksum = prevChecksumStr
			mf.PreviousPath = prevPath
			mf.PreviousVersion = previousVersion
		}
		mf.LastHealthCheck = time.Now().UTC()
		mf.LastHealthResult = result
	}); err != nil {
		m.logger.Warn(Subsystem, "persist_failed",
			"could not persist manifest for %s: %v", name, err)
	}

	// ---- 12. Cleanup staging ----------------------------------------
	_ = os.RemoveAll(m.StagingDir(name))

	emitProgress(name, StageComplete, "installed "+versionStr, 0, 0)
	m.logger.Info(Subsystem, "install_complete",
		"core %s %s installed (sha256=%s)",
		name, versionStr, actual[:12])

	return nil
}

// activate moves the staged executable into BinDir atomically and
// retains the previous binary as the rollback target. On any failure
// it restores the previous binary (automatic rollback) so the manager
// never ends up without an active core.
func (m *Manager) activate(name CoreName, execPath string, preSnap Manifest) (finalPath, prevPath string, prevExists bool, err error) {
	prevPath = m.RollbackPath(name)

	currentBinaryPath := preSnap.BinaryPath

	if currentBinaryPath != "" {
		if _, statErr := os.Stat(currentBinaryPath); statErr == nil {
			// Move the current binary aside (rollback retention).
			_ = os.Remove(prevPath)

			if rerr := os.Rename(currentBinaryPath, prevPath); rerr != nil {
				// Rename failed (file locked): keep the old binary in
				// place; this update will have no rollback target.
				m.logger.Warn(Subsystem, "rollback_retain_failed",
					"could not retain rollback for %s: %v", name, rerr)
				prevExists = false
			} else {
				prevExists = true
			}
		}
	}

	if err := os.MkdirAll(m.BinDir(name), 0o700); err != nil {
		return "", prevPath, prevExists, err
	}

	finalPath = m.BinaryPath(name)

	if err := os.Rename(execPath, finalPath); err != nil {
		// Copy fallback (cross-device staging).
		if cerr := copyFile(execPath, finalPath, 0o700); cerr != nil {
			// Automatic rollback: restore the previous binary.
			if prevExists {
				_ = os.Remove(finalPath)
				_ = os.Rename(prevPath, finalPath)
			}

			return "", prevPath, prevExists, cerr
		}
	}

	return finalPath, prevPath, prevExists, nil
}

// verifyAssetIdentity proves the SELECTED asset really belongs to the
// expected repository, release, platform and architecture — never
// trusting the filename alone:
//
//   - https scheme and an allowlisted host;
//   - the URL path names THIS repository's releases/download area;
//   - the asset name matches the target platform AND architecture
//     (a wrong-OS or wrong-arch asset is rejected);
//   - the published size is present and positive;
//   - the release tag is present.
func verifyAssetIdentity(src Source, p Platform, info UpdateInfo) error {
	if info.ReleaseTag == "" {
		return firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "select_asset", "release has no tag")
	}

	if info.AssetURL == "" {
		return firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "select_asset", "no asset for %s/%s", p.OS, p.Arch)
	}

	u, err := url.Parse(info.AssetURL)
	if err != nil {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "select_asset", "asset URL is malformed: %s", info.AssetURL)
	}

	// v0.9.8.7: HTTPS is MANDATORY, exactly as docs/security.md
	// documents. Plain http:// would let a downgraded asset URL bypass
	// the transport security the trust anchor (GitHub over TLS) is
	// built on; the only exception is a loopback authority (local test
	// harnesses / pinned local mirrors), which previously existed as
	// the dead helper isLoopbackHost without ever being enforced.
	if u.Scheme != "https" && !isLoopbackHost(u.Host) {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "select_asset",
			"asset URL scheme %q is not usable: only https (or a loopback test authority) is permitted", u.Scheme)
	}

	// Host trust: the asset must be served by the source's own release
	// API authority (the configured trust anchor — same host) or by a
	// known GitHub release host. A compromised API response cannot
	// redirect the downloader at an arbitrary server.
	if !trustedAssetHost(u.Host, src.ReleaseAPI) {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "select_asset", "asset host %q is not a trusted release host", u.Host)
	}

	// Repository identity: the download path must live under this
	// source's repo ("github.com/<repo>/releases/download/...").
	repoPath := "/" + strings.TrimPrefix(src.Repo, "/") + "/releases/download/"
	if !strings.Contains(strings.ToLower(u.Path), strings.ToLower(repoPath)) {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "select_asset",
			"asset URL %s does not belong to repository %s", u.Path, src.Repo)
	}

	// Platform + architecture identity from the asset NAME.
	assetName := path.Base(u.Path)

	if !assetNamesPlatform(assetName, p.OS) {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "select_asset",
			"asset %q does not target platform %s (wrong platform asset)", assetName, p.OS)
	}

	if !assetNamesArch(assetName, p.Arch) {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "select_asset",
			"asset %q does not target architecture %s (wrong architecture asset)", assetName, p.Arch)
	}

	if info.AssetSize <= 0 {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "select_asset",
			"asset %q has no published size", assetName)
	}

	return nil
}

// isLoopbackHost reports whether host:port names a loopback authority
// (127.0.0.0/8, ::1, localhost) — the documented http exception for
// test harnesses and pinned local mirrors.
func isLoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}

	host = strings.Trim(host, "[]")

	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// trustedAssetHost reports whether an asset host is acceptable: the
// host serving the source's release API (same authority) or a known
// GitHub release host. Comparison is case-insensitive; a port is
// preserved so local test authorities (127.0.0.1:PORT) qualify.
func trustedAssetHost(assetHost, releaseAPIURL string) bool {
	assetHost = strings.ToLower(strings.TrimSpace(assetHost))
	if assetHost == "" {
		return false
	}

	// GitHub's official release hosts.
	switch assetHost {
	case "github.com", "api.github.com",
		"objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return true
	}

	// The source's own release-API authority (e.g. a pinned mirror or
	// a test harness).
	if api, err := url.Parse(releaseAPIURL); err == nil {
		if strings.ToLower(api.Host) == assetHost {
			return true
		}
	}

	return false
}

// assetNamesPlatform reports whether an asset name targets osName,
// using the shared OS token table (case-insensitive).
func assetNamesPlatform(assetName, osName string) bool {
	lower := strings.ToLower(assetName)

	osTokens := map[string][]string{
		"windows": {"windows", "win32", "win64"},
		"linux":   {"linux"},
		"darwin":  {"darwin", "macos", "macosx", "osx", "mac"},
		"freebsd": {"freebsd"},
	}

	tokens, ok := osTokens[osName]
	if !ok {
		return strings.Contains(lower, osName)
	}

	for _, token := range tokens {
		if strings.Contains(lower, token) {
			return true
		}
	}

	return false
}

// assetNamesArch reports whether an asset name's architecture
// convention is compatible with archName. Legacy "64" means
// x86_64/amd64 and never arm64; arm64/aarch64 are explicit.
func assetNamesArch(assetName, archName string) bool {
	lower := strings.ToLower(assetName)

	hasArm64 := strings.Contains(lower, "arm64") || strings.Contains(lower, "aarch64")
	hasAmd64 := strings.Contains(lower, "amd64") || strings.Contains(lower, "x86_64") || strings.Contains(lower, "x86-64")
	has64 := strings.Contains(lower, "64")

	switch archName {
	case "arm64", "aarch64":
		return hasArm64
	case "amd64", "x86_64", "x86-64":
		// "arm64" contains "64": check it first.
		if hasArm64 {
			return false
		}

		return hasAmd64 || has64
	case "386", "i386":
		return strings.Contains(lower, "386") || strings.Contains(lower, "32")
	default:
		return true
	}
}

// verifyAssetDigest verifies the computed SHA-256 against the
// AUTHORITATIVE published digests, in order of authority:
//
//  1. the GitHub release-API digest field (asset.digest,
//     "sha256:<hex>" — strongest, comes with the signed metadata);
//  2. the .dgst sidecar file published next to the asset
//     (Xray/V2Ray convention).
//
// v0.9.8.6: NO third tier. When no authoritative digest is available
// the install is REJECTED — a locally computed SHA-256 is tamper
// evidence, never a trust anchor, and may never make a remotely
// acquired executable runnable.
func (m *Manager) verifyAssetDigest(ctx context.Context, src Source, info UpdateInfo, computedSHA string) error {
	// 1. API-provided digest.
	if info.AssetSHA256 != "" {
		if !strings.EqualFold(info.AssetSHA256, computedSHA) {
			return firerrors.New(firerrors.KindInvalidInput,
				Subsystem, "install",
				"checksum mismatch: release API digest=%s asset=%s",
				info.AssetSHA256, computedSHA)
		}

		return nil
	}

	// 2. Published .dgst sidecar (Xray/V2Ray convention).
	if info.AssetURL == "" {
		return firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "install",
			"no authoritative digest available for the selected asset; "+
				"refusing to install an unverified executable")
	}

	digestURL := info.AssetURL + ".dgst"

	resp, err := m.httpClient.Get(ctx, digestURL, httpx.GetOptions{
		Header: map[string]string{"Accept": "application/octet-stream"},
	})

	// httpx turns non-2xx into a status error; a 404 sidecar is the
	// normal "no digest published" answer, every other failure is a
	// digest-authority outage. BOTH reject the install (v0.9.8.6: fail
	// closed); only the explanation differs.
	if err != nil {
		if httpx.StatusCodeOf(err) == http.StatusNotFound {
			return firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "install",
				"release %s publishes no digest for %s; refusing to install an "+
					"unverified executable (computed sha256=%s recorded for "+
					"evidence only)", info.ReleaseTag, path.Base(info.AssetURL), computedSHA)
		}

		return firerrors.Wrap(err, firerrors.KindDependencyUnavailable,
			Subsystem, "install", "could not fetch the published digest %s", digestURL)
	}

	if resp.StatusCode != http.StatusOK {
		// Belt and braces (httpx already converts statuses to errors).
		return firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "install",
			"release %s publishes no digest for %s; refusing to install an "+
				"unverified executable (computed sha256=%s recorded for "+
				"evidence only)", info.ReleaseTag, path.Base(info.AssetURL), computedSHA)
	}

	expected, perr := extractSHA256FromDigest(string(resp.Body))
	if perr != nil {
		// A digest sidecar that exists but cannot be parsed is a
		// broken authority: fail closed rather than install on the
		// strength of the download itself.
		return firerrors.New(firerrors.KindDependencyUnavailable,
			Subsystem, "install",
			"published digest for %s is unparseable (%v); refusing to "+
				"install an unverified executable", path.Base(info.AssetURL), perr)
	}

	if !strings.EqualFold(expected, computedSHA) {
		return firerrors.New(firerrors.KindInvalidInput,
			Subsystem, "install",
			"checksum mismatch: digest=%s asset=%s", expected, computedSHA)
	}

	return nil
}

// extractSHA256FromDigest parses a multi-line digest file (OpenSSL
// dgst format: "SHA256(filename)= <hex>") and returns the hex digest.
func extractSHA256FromDigest(raw string) (string, error) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Look for "SHA256(...)= hex"
		if strings.Contains(line, "SHA256") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				hexd := strings.TrimSpace(parts[1])
				if len(hexd) == 64 {
					return hexd, nil
				}
			}
		}
		// Bare hex line (sing-box convention when present)
		if len(line) == 64 && isHex(line) {
			return line, nil
		}
	}
	return "", fmt.Errorf("no SHA-256 line in digest")
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// unpackArchive unpacks a .zip, .tar.gz or .tgz archive into dst
// through internal/safearchive: bounded (archive size, total
// expansion, per-file size, entry count), path-traversal protected,
// absolute-path rejecting, symlink/hostile-entry rejecting and
// fail-closed on malformed archives (v0.9.8.6). The format is
// selected by filename extension with a content-sniffing fallback
// (PK zip magic / gzip magic) so a misnamed staged file still unpacks
// instead of failing the whole install.
func unpackArchive(archivePath, dst string) error {
	return safearchive.Unpack(archivePath, dst, safearchive.DefaultLimits())
}

// findExecutable walks a directory looking for the core's executable
// (e.g. "xray", "xray.exe", "v2ray", "sing-box"). The first match
// wins; archives are flat or single-level.
func findExecutable(root, name string) (string, error) {
	target := name
	if runtime.GOOS == "windows" {
		target = name + ".exe"
	}

	var found string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Base(p), target) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})

	if found == "" {
		return "", fmt.Errorf("executable %s not found in archive", target)
	}
	return found, nil
}

// copyFile copies src to dst with the given mode. Used as a fallback
// when rename fails (cross-device staging directory).
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
