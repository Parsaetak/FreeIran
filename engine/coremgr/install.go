package coremgr

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Install downloads, verifies and activates the latest stable release
// of the core on the configured channel. It is idempotent: re-running
// it for an already-up-to-date core re-validates the active binary and
// corrects the manifest.
//
// Pipeline (every step fails-safe: a partial install never replaces
// the active binary):
//
//  1. setState(Installing)
//  2. resolve the latest release for the configured channel
//  3. download the asset into StagingDir
//  4. compute SHA-256; verify against the GitHub-provided digest if
//     one is available (Xray and V2Ray publish .dgst files;
//     sing-box relies on asset SHA-256 from the release API)
//  5. unpack into StagingDir/unpacked/
//  6. locate the executable inside the unpacked archive
//  7. query the executable's version (must match the release tag)
//  8. validate the executable can accept a minimal config (test/check
//     subcommand)
//  9. retain the current active binary as the rollback target
//  10. atomic rename staged executable into BinDir
//  11. setState(Ready); persist manifest
//
// If any step fails, StagingDir is removed and the previous binary is
// left untouched. State becomes Broken with details in LastHealthResult.
func (m *Manager) Install(ctx context.Context, name CoreName) error {
	unlock := m.lock(name)
	defer unlock()

	src, ok := m.sources[name]
	if !ok {
		return firerrors.New(firerrors.KindConfiguration,
			Subsystem, "install", "no source defined for %s", name)
	}

	// Mark installing. If state was already Installing, refuse the
	// second call so we never run two installs for the same core.
	snap, _ := m.snapshotManifest(name)
	if snap.State == StateInstalling {
		return ErrAlreadyInstalling
	}
	channel := snap.Channel

	if err := m.setState(name, StateInstalling); err != nil {
		return err
	}

	m.logger.Info(Subsystem, "install_start",
		"installing core %s from %s", name, src.Repo)

	// Failure handler: clear staging, set Broken with a human-readable
	// reason (surfaced verbatim in the UI), log, emit progress.
	fail := func(stage string, ferr error) error {
		_ = os.RemoveAll(m.StagingDir(name))

		reason := HumanizeInstallFailure(stage, ferr)
		_ = m.updateManifest(name, func(mf *Manifest) {
			mf.State = StateBroken
			mf.FailureReason = reason
			mf.FailureStage = stage
			mf.UpdatedAt = time.Now().UTC()
		})

		emitProgress(name, StageFailed, reason, 0, 0)
		m.logger.Error(Subsystem, "install_failed", "install", string(firerrors.KindDependencyUnavailable),
			"core %s install failed at %s: %v", name, stage, ferr)

		return firerrors.Wrap(ferr, firerrors.KindDependencyUnavailable,
			Subsystem, "install", "%s: %s", name, stage)
	}

	// 1. Resolve the latest release.
	emitProgress(name, StageResolveRelease, "resolving latest release for channel "+string(channel), 0, 0)
	update, err := m.checkRelease(ctx, name, channel)
	if err != nil {
		return fail("resolve_release", err)
	}

	if update.AssetURL == "" {
		return fail("select_asset",
			firerrors.New(firerrors.KindDependencyUnavailable,
				Subsystem, "install",
				"no asset for %s/%s", m.platform.OS, m.platform.Arch))
	}

	// 2. Prepare staging directory.
	if err := os.RemoveAll(m.StagingDir(name)); err != nil {
		return fail("reset_staging", err)
	}
	if err := os.MkdirAll(m.StagingDir(name), 0o700); err != nil {
		return fail("mkdir_staging", err)
	}

	// 3. Download (streamed, with byte progress through the injected
	// HTTP downloader). The staged file keeps the asset's real
	// extension so unpackArchive can pick the format — v0.8 staged
	// everything as "asset.bin" and unpack always failed with
	// "unknown archive format".
	assetName := path.Base(update.AssetURL)
	if assetName == "" || assetName == "." || strings.Contains(assetName, "?") {
		assetName = "asset.zip"
	}

	assetPath := filepath.Join(m.StagingDir(name), assetName)
	emitProgress(name, StageDownload, "downloading "+update.AssetURL, 0, update.AssetSize)
	if err := m.downloadFile(ctx, update.AssetURL, assetPath, func(done, total int64) {
		emitProgress(name, StageDownload, "downloading", done, total)
	}); err != nil {
		return fail("download", err)
	}

	// 4. Verify SHA-256.
	emitProgress(name, StageVerifyChecksum, "verifying SHA-256", 0, 0)
	actual, err := fileSHA256(assetPath)
	if err != nil {
		return fail("hash", err)
	}
	// If the source provided a digest URL (Xray/V2Ray convention),
	// verify against it. Otherwise, store the computed hash.
	if err := m.verifyAgainstPublishedDigest(ctx, src, update, actual); err != nil {
		return fail("verify_digest", err)
	}

	// 5. Unpack.
	emitProgress(name, StageUnpack, "unpacking archive", 0, 0)
	unpackedDir := filepath.Join(m.StagingDir(name), "unpacked")
	if err := os.MkdirAll(unpackedDir, 0o700); err != nil {
		return fail("mkdir_unpacked", err)
	}
	if err := unpackArchive(assetPath, unpackedDir); err != nil {
		return fail("unpack", err)
	}

	// 6. Locate the executable.
	emitProgress(name, StageLocate, "locating executable", 0, 0)
	execPath, err := findExecutable(unpackedDir, string(name))
	if err != nil {
		return fail("locate_executable", err)
	}

	// 6b. Ensure the executable bit BEFORE any probe runs: zip
	// extraction writes 0600, so a version probe would fail with
	// permission-denied on Unix (v0.8 chmodded only after the
	// probes — a Linux install could never succeed).
	if err := ensureExecutable(execPath); err != nil {
		return fail("chmod", err)
	}

	// 7. Query version.
	emitProgress(name, StageValidate, "probing version", 0, 0)
	versionStr, err := m.queryVersion(ctx, execPath, src.VersionProbeArgs)
	if err != nil || versionStr == "" {
		return fail("probe_version",
			fmt.Errorf("version query returned %q (err=%v)", versionStr, err))
	}

	// 7b. Sanity-check the probed version against the release tag.
	// Both sides pass through the same loose-semver extraction, so
	// a corrupted or wrong asset fails here instead of activating.
	if probeVer := ExtractVersionToken(versionStr); probeVer != "" {
		if tagVer := ExtractVersionToken(update.LatestVersion); tagVer != "" {
			if compareVersions(probeVer, tagVer) != 0 {
				return fail("probe_version", fmt.Errorf(
					"downloaded binary reports version %s but release %s expects %s",
					probeVer, update.ReleaseTag, tagVer))
			}
		}
	}

	// 8. Validate executable accepts a minimal config.
	emitProgress(name, StageValidate, "validating executable", 0, 0)
	if err := m.validateExecutable(ctx, name, execPath, src); err != nil {
		return fail("validate_executable", err)
	}

	// 9. Ensure executable bit (Unix) — also covers the tar path
	// where the archive lost its mode bits.
	if err := ensureExecutable(execPath); err != nil {
		return fail("chmod", err)
	}

	// 10. Retain rollback target.
	emitProgress(name, StageActivate, "activating", 0, 0)
	prevPath := m.RollbackPath(name)
	prevExists := false
	currentBinaryPath := snap.BinaryPath
	if currentBinaryPath == "" {
		// Re-snapshot in case it was set since the start of Install.
		snap2, _ := m.snapshotManifest(name)
		currentBinaryPath = snap2.BinaryPath
	}
	if currentBinaryPath != "" {
		if _, statErr := os.Stat(currentBinaryPath); statErr == nil {
			// Move the current binary aside.
			_ = os.Remove(prevPath)
			if err := os.Rename(currentBinaryPath, prevPath); err != nil {
				// If rename fails (file locked), keep the old
				// binary in place; rollback will not be available
				// for this update.
				m.logger.Warn(Subsystem, "rollback_retain_failed",
					"could not retain rollback for %s: %v", name, err)
				prevExists = false
			} else {
				prevExists = true
			}
		}
	}

	// 11. Atomic activation: rename staged exec into BinDir.
	if err := os.MkdirAll(m.BinDir(name), 0o700); err != nil {
		return fail("mkdir_bin", err)
	}
	finalPath := m.BinaryPath(name)
	if err := os.Rename(execPath, finalPath); err != nil {
		// Try copy fallback if rename fails (cross-device).
		if err := copyFile(execPath, finalPath, 0o700); err != nil {
			// Restore the previous binary so we are not left
			// without one.
			if prevExists {
				_ = os.Rename(prevPath, finalPath)
			}
			return fail("activate", err)
		}
	}

	// 12. Update the manifest under m.mu via updateManifest.
	var prevChecksumStr string
	if prevExists {
		prevChecksumStr, _ = fileSHA256(prevPath)
	}
	prevChecksumFinal := prevChecksumStr

	// 13. Run a smoke test to confirm readiness.
	emitProgress(name, StageSmokeTest, "running smoke test", 0, 0)
	result := m.smokeTest(ctx, name, finalPath, src)

	// Capture the OLD version for the rollback trail before the
	// manifest overwrite (v0.8 never recorded it).
	previousVersion := snap.Version

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
			mf.PreviousChecksum = prevChecksumFinal
			mf.PreviousPath = prevPath
			mf.PreviousVersion = previousVersion
		}
		mf.LastHealthCheck = time.Now().UTC()
		mf.LastHealthResult = result
		if !result.OK {
			mf.State = StateBroken
			mf.FailureReason = HumanizeHealthFailure(result)
			mf.FailureStage = "smoke_test"
		}
	}); err != nil {
		m.logger.Warn(Subsystem, "persist_failed",
			"could not persist manifest for %s: %v", name, err)
	}

	// 14. Cleanup staging.
	_ = os.RemoveAll(m.StagingDir(name))

	// Re-snapshot to log the final state.
	finalSnap, _ := m.snapshotManifest(name)
	if finalSnap.State == StateReady {
		emitProgress(name, StageComplete, "installed "+versionStr, 0, 0)
		m.logger.Info(Subsystem, "install_complete",
			"core %s %s installed (sha256=%s)",
			name, versionStr, actual[:12])
	}

	return nil
}

// checkRelease fetches the latest release for the given channel and
// returns the resolved asset URL + metadata.
func (m *Manager) checkRelease(ctx context.Context, name CoreName, ch Channel) (UpdateInfo, error) {
	src := m.sources[name]
	client := m.resolveHTTPClient()

	// Stable channel: GET /releases/latest.
	// Prerelease channel: GET /releases, take the first entry.
	var raw []byte
	var err error
	var releaseURL string

	if ch == ChannelPrerelease {
		raw, _, err = m.httpGet(ctx, client, src.ReleaseAPI+"?per_page=10")
		if err != nil {
			return UpdateInfo{}, err
		}
		releaseURL = src.ReleasePage
	} else {
		raw, _, err = m.httpGet(ctx, client, src.ReleaseAPI+"/latest")
		if err != nil {
			return UpdateInfo{}, err
		}
		releaseURL = src.ReleasePage + "/latest"
	}

	info, err := parseRelease(raw, name, m.platform, src, releaseURL)
	if err != nil {
		return UpdateInfo{}, err
	}

	if ch == ChannelPrerelease {
		// parseRelease returns the latest non-prerelease by default.
		// For the prerelease channel we want the first entry of the
		// list (which may be a prerelease).
		info, err = parseFirstRelease(raw, name, m.platform, src, releaseURL)
		if err != nil {
			return UpdateInfo{}, err
		}
	}

	return info, nil
}

// verifyAgainstPublishedDigest downloads the .dgst file published
// alongside the asset (Xray/V2Ray convention) and verifies the
// computed SHA-256 matches. If no digest file exists for the asset,
// the function returns nil (the manager records the computed hash in
// the manifest, so the user has a tamper-evidence trail even without
// a published digest).
func (m *Manager) verifyAgainstPublishedDigest(ctx context.Context, src Source, info UpdateInfo, computedSHA string) error {
	// Xray publishes <asset>.dgst next to <asset>. V2Ray publishes
	// <asset>.dgst. sing-box does not publish digests.
	if info.AssetURL == "" {
		return nil
	}

	digestURL := info.AssetURL + ".dgst"
	raw, status, err := m.httpGet(ctx, m.resolveHTTPClient(), digestURL)
	if err != nil || status != 200 {
		// No digest published; not an error.
		return nil
	}

	expected, err := extractSHA256FromDigest(string(raw))
	if err != nil {
		return nil // ignore unparseable digest; the computed hash is recorded
	}

	if expected != computedSHA {
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
				hex := strings.TrimSpace(parts[1])
				if len(hex) == 64 {
					return hex, nil
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

// downloadFile streams a URL to a local path with bounded memory and
// byte-level progress reporting. It routes through the manager's
// resolved HTTPDownloader so injected clients (and test fakes) apply
// to asset downloads — the v0.8 implementation bypassed them.
func (m *Manager) downloadFile(ctx context.Context, url, dst string, onBytes func(done, total int64)) error {
	stream, err := m.resolveDownloader().Download(ctx, url)
	if err != nil {
		return err
	}
	defer stream.Body.Close()

	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		return fmt.Errorf("download %s: HTTP %d", url, stream.StatusCode)
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	buf := make([]byte, 256<<10)
	var done int64

	for {
		n, readErr := stream.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}

			done += int64(n)

			if onBytes != nil {
				onBytes(done, stream.Total)
			}
		}

		if readErr == io.EOF {
			return nil
		}

		if readErr != nil {
			return readErr
		}
	}
}

// httpGet fetches a URL and returns body + status code.
func (m *Manager) httpGet(ctx context.Context, client HTTPDoer, url string) ([]byte, int, error) {
	resp, err := client.Do(url)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.StatusCode, nil
}

// unpackArchive unpacks a .zip, .tar.gz or .tgz archive into dst.
// The format is selected by filename extension, with a content
// sniffing fallback (PK zip magic / gzip magic) so a misnamed staged
// file still unpacks instead of failing the whole install.
func unpackArchive(archivePath, dst string) error {
	name := strings.ToLower(filepath.Base(archivePath))
	switch {
	case strings.HasSuffix(name, ".zip"):
		return unpackZip(archivePath, dst)
	case strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz"):
		return unpackTarGz(archivePath, dst)
	case strings.HasSuffix(name, ".tar"):
		return unpackTar(archivePath, dst)
	default:
		// Content sniffing fallback.
		magic := make([]byte, 2)

		if f, err := os.Open(archivePath); err == nil {
			_, _ = io.ReadFull(f, magic)
			_ = f.Close()
		}

		switch {
		case bytes.Equal(magic, []byte("PK")):
			return unpackZip(archivePath, dst)
		case bytes.Equal(magic, []byte{0x1f, 0x8b}):
			return unpackTarGz(archivePath, dst)
		default:
			return fmt.Errorf("unknown archive format: %s", name)
		}
	}
}

func unpackZip(archivePath, dst string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if err := extractZipFile(f, dst); err != nil {
			return err
		}
	}
	return nil
}

func extractZipFile(f *zip.File, dst string) error {
	path := filepath.Join(dst, f.Name)
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(dst)+string(os.PathSeparator)) && path != dst {
		return fmt.Errorf("zip slip: %s", path)
	}

	if f.FileInfo().IsDir() {
		return os.MkdirAll(path, 0o700)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, rc)
	return err
}

func unpackTarGz(archivePath, dst string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	return unpackTarReader(tar.NewReader(gz), dst)
}

func unpackTar(archivePath, dst string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	return unpackTarReader(tar.NewReader(f), dst)
}

func unpackTarReader(tr *tar.Reader, dst string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		path := filepath.Join(dst, hdr.Name)
		if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(dst)+string(os.PathSeparator)) && path != dst {
			return fmt.Errorf("tar slip: %s", path)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode)&0o700)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			_ = out.Close()
		}
	}
	return nil
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
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Base(path), target) {
			found = path
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
