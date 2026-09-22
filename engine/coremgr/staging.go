package coremgr

import (
	"context"
	"os"
	"path/filepath"

	"github.com/Parsaetak/FreeIran/internal/httpx"
)

// cleanStaleStaging removes staging entries that do not belong to the
// current asset — the partial or complete artifacts of an OLDER
// release, and stale unpacked trees. The current asset file, its
// .part file and the unpacked directory of the current attempt are
// preserved; invalidation stays precise (v0.9.14: never flush
// unrelated work).
func cleanStaleStaging(stagingDir, currentAsset string) error {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	keep := map[string]bool{
		currentAsset:           true,
		currentAsset + ".part": true,
	}

	for _, entry := range entries {
		name := entry.Name()

		// The unpacked tree is rebuilt for every install of the
		// CURRENT asset; an old tree from a different asset version
		// would shadow the fresh unpack.
		if name == "unpacked" {
			continue
		}

		if keep[name] {
			continue
		}

		_ = os.RemoveAll(filepath.Join(stagingDir, name))
	}

	return nil
}

// reuseStagedArtifact implements the download-level idempotency rule:
// a COMPLETE staged archive from an interrupted install is verified
// (size + SHA-256 against the authoritative digest) and reused without
// touching the network; a partial one is left for the downloader to
// resume; a corrupt one is discarded — that artifact alone.
//
// The bool result reports whether the staged artifact is complete and
// valid; sha is its computed digest when reused.
func (m *Manager) reuseStagedArtifact(ctx context.Context, name CoreName, src Source, assetPath string, update UpdateInfo) (bool, string) {
	info, err := os.Stat(assetPath)
	if err != nil || info.IsDir() {
		// No staged artifact (or a stale directory) — nothing to reuse.
		// A partial .part file is handled by the downloader's resume.
		return false, ""
	}

	// A complete-but-unverified staged file is only trusted when the
	// published size is known and matches; an unknown size cannot
	// prove completeness.
	if update.AssetSize > 0 && info.Size() != update.AssetSize {
		if info.Size() > update.AssetSize {
			// Oversized: corrupt or truncated tail from a different
			// asset — discard this artifact only.
			m.logger.Warn(Subsystem, "artifact_invalid",
				"core %s staged artifact %s is oversized (%d > %d); discarding",
				name, filepath.Base(assetPath), info.Size(), update.AssetSize)

			_ = os.Remove(assetPath)

			return false, ""
		}

		// Smaller than published: an interrupted download without the
		// .part naming — move it to the .part name so the downloader
		// RESUMES it instead of restarting from zero.
		partPath := assetPath + ".part"

		_ = os.Remove(partPath)

		if rerr := os.Rename(assetPath, partPath); rerr == nil {
			m.logger.Info(Subsystem, "artifact_reused",
				"core %s partial staged artifact moved to %s for resume",
				name, filepath.Base(partPath))
		}

		return false, ""
	}

	// Size matches (or is unknown): verify the digest before reuse.
	sha, err := fileSHA256(assetPath)
	if err != nil {
		m.logger.Warn(Subsystem, "artifact_invalid",
			"core %s staged artifact could not be hashed: %v", name, err)

		_ = os.Remove(assetPath)

		return false, ""
	}

	if err := m.verifyAssetDigest(ctx, src, update, sha); err != nil {
		// Corrupt or from a different release: discard exactly this
		// artifact and download fresh.
		m.logger.Warn(Subsystem, "artifact_invalid",
			"core %s staged artifact failed digest verification; discarding: %v", name, err)

		_ = os.Remove(assetPath)

		return false, ""
	}

	m.logger.Info(Subsystem, "artifact_reused",
		"core %s complete staged artifact verified and reused (sha256=%s); no network download",
		name, shortSHA(sha))

	return true, sha
}

// shortSHA renders the first 12 hex characters of a digest for logs.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}

	return sha
}

// downloadOrReuse acquires the asset, reusing a complete staged
// artifact when verification allows it. The returned result satisfies
// the same downstream verification (size + digest) the downloaded
// path always ran.
func (m *Manager) downloadOrReuse(
	ctx context.Context,
	name CoreName,
	src Source,
	assetPath string,
	update UpdateInfo,
	onProgress func(p httpx.Progress),
) (*httpx.DownloadResult, error) {
	if reused, sha := m.reuseStagedArtifact(ctx, name, src, assetPath, update); reused {
		size := update.AssetSize

		if info, err := os.Stat(assetPath); err == nil {
			size = info.Size()
		}

		return &httpx.DownloadResult{
			Bytes:  size,
			SHA256: sha,
		}, nil
	}

	return m.httpClient.Download(ctx, update.AssetURL, httpx.DownloadOptions{
		DestPath:     assetPath,
		ExpectedSize: update.AssetSize,
		StallTimeout: m.dlStallTimeout,
		MaxRetries:   m.dlMaxRetries,
		OnProgress:   onProgress,
	})
}
