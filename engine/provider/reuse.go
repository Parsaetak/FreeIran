package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
	"github.com/Parsaetak/FreeIran/system"
)

// v0.9.14 — provider-side reuse-first semantics. Tor and Psiphon share
// the SAME rules the core manager implements: discover and reuse an
// already-installed working engine before downloading, adopt external
// binaries by reference (never modify them), and reuse complete
// staged artifacts instead of redownloading.

// Ownership values used by provider manifests (mirror of the coremgr
// vocabulary, kept package-local for the provider UI surface).
const (
	OwnershipManaged  = system.OwnershipManaged
	OwnershipExternal = system.OwnershipExternal
)

// Origin values used by provider manifests.
const (
	OriginManaged = system.OriginManaged
	OriginPath    = system.OriginPath
	OriginSystem  = system.OriginSystem
	OriginUser    = system.OriginUser
)

// cleanStaleProviderStaging removes staging entries of a DIFFERENT
// release; the current asset, its .part and the unpacked tree of the
// current attempt are preserved. (Unpacked is rebuilt every install.)
func cleanStaleProviderStaging(stagingDir, currentAsset string) error {
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

// downloadOrReuse acquires the release asset, reusing a complete,
// checksum-verified staged archive instead of touching the network.
// The checksum authority stays mandatory: a staged artifact without a
// published digest to compare against is never reused.
func (b *BinaryManager) downloadOrReuse(ctx context.Context, archivePath string, release Release) (*httpx.DownloadResult, error) {
	info, err := os.Stat(archivePath)
	if err == nil && !info.IsDir() {
		switch {
		case release.Size > 0 && info.Size() > release.Size:
			// Oversized: corrupt artifact — discard it alone.
			logInstall(b.Name, "artifact_invalid", map[string]any{
				"asset": filepath.Base(archivePath),
			})

			_ = os.Remove(archivePath)

		case release.Size > 0 && info.Size() < release.Size:
			// Interrupted download without the .part naming: move it to
			// the .part name so the downloader RESUMES it.
			part := archivePath + ".part"

			_ = os.Remove(part)

			if rerr := os.Rename(archivePath, part); rerr == nil {
				logInstall(b.Name, "artifact_reused", map[string]any{
					"asset": filepath.Base(part),
					"mode":  "resume",
				})
			}

		default:
			// Size matches (or unknown): the checksum authority is
			// mandatory, so a reuse candidate must equal the published
			// SHA-256.
			if strings.TrimSpace(release.SHA256) != "" {
				if sum, serr := fileSHA256(archivePath); serr == nil && strings.EqualFold(sum, release.SHA256) {
					logInstall(b.Name, "artifact_reused", map[string]any{
						"asset":    filepath.Base(archivePath),
						"mode":     "complete",
						"checksum": sum[:16] + "…",
					})

					size := release.Size
					if size <= 0 {
						size = info.Size()
					}

					return &httpx.DownloadResult{Bytes: size, SHA256: sum}, nil
				}
			}

			// Not verifiable against the published digest: discard the
			// staged artifact (that artifact alone) and download fresh.
			_ = os.Remove(archivePath)
		}
	}

	maxBytes := b.MaxDownloadBytes
	if maxBytes <= 0 {
		maxBytes = 512 << 20
	}

	return b.HTTP.Download(ctx, release.AssetURL, httpx.DownloadOptions{
		DestPath:     archivePath,
		ExpectedSize: release.Size,
		MaxBytes:     maxBytes,
		StallTimeout: 30 * time.Second,
		MaxRetries:   4,
	})
}

// EnsureAvailable is the reuse-first availability check for a target
// release: it returns true when a suitable binary is already usable —
// the exact managed release (checksum- or version-verified) or a
// healthy external installation — WITHOUT downloading anything.
//
// It NEVER acquires: acquisition is Install's job (explicit user
// intent). This keeps automatic discovery from silently becoming
// automatic software updating.
func (b *BinaryManager) EnsureAvailable(ctx context.Context, release Release, validate func(ctx context.Context, path string) (string, error)) bool {
	manifest := b.LoadManifest()

	if manifest.BinaryPath == "" {
		return false
	}

	if _, err := os.Stat(manifest.BinaryPath); err != nil {
		// The referenced binary vanished: drop the reference honestly.
		return false
	}

	// Exact managed release: same verified bundle.
	if manifest.ChecksumSHA256 != "" && release.SHA256 != "" &&
		strings.EqualFold(manifest.ChecksumSHA256, release.SHA256) {
		return true
	}

	// Same version, externally owned and validated: reuse without
	// download.
	if manifest.Ownership == string(OwnershipExternal) && release.Version != "" &&
		manifest.Version == release.Version && validate != nil {
		if _, err := validate(ctx, manifest.BinaryPath); err == nil {
			return true
		}
	}

	return false
}
