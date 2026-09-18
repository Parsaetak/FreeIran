// binary.go implements the shared managed-binary acquisition
// pipeline (§8/§9) used by the Tor and Psiphon engines. It mirrors
// engine/coremgr's proven transaction discipline and reuses
// internal/httpx for both control and data planes:
//
//	RESOLVE (metadata, official source, checksum authority)
//	→ DOWNLOAD (.part streamed, resumable, stall-watched, capped)
//	→ VERIFY (size + SHA-256 against the published checksum)
//	→ UNPACK (tar.gz with tar-slip guards)
//	→ VALIDATE (version probe of the staged executable)
//	→ SMOKE (launch + observe the local endpoint)
//	→ ACTIVATE (atomic rename; previous binary retained)
//	→ MANIFEST (persisted atomically; rollback on any failure)
//
// The workspace layout is <root>/<name>/{bin/, staging/, manifest.json}.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/internal/httpx"
	"github.com/Parsaetak/FreeIran/internal/logging"
)

// Manifest is the persisted provider install state.
type Manifest struct {
	Name             string    `json:"name"`
	Version          string    `json:"version,omitempty"`
	BinaryPath       string    `json:"binary_path,omitempty"`
	ChecksumSHA256   string    `json:"checksum_sha256,omitempty"`
	SourceURL        string    `json:"source_url,omitempty"`
	ReleaseURL       string    `json:"release_url,omitempty"`
	InstalledAt      time.Time `json:"installed_at,omitempty"`
	LastChecked      time.Time `json:"last_checked,omitempty"`
	LastHealthCheck  time.Time `json:"last_health_check,omitempty"`
	State            string    `json:"state"`
	PreviousVersion  string    `json:"previous_version,omitempty"`
	PreviousChecksum string    `json:"previous_checksum,omitempty"`
	PreviousPath     string    `json:"previous_path,omitempty"`
	FailureReason    string    `json:"failure_reason,omitempty"`
	FailureStage     string    `json:"failure_stage,omitempty"`
}

// BinaryManager owns one provider's binary slot on disk.
type BinaryManager struct {
	// Name is the provider id.
	Name string

	// RootDir is the providers root (manifest.json + bin/ under
	// <RootDir>/<Name>/).
	RootDir string

	// HTTP is the shared client (httpx.Default() in production).
	HTTP httpx.Interface

	// Platform selects assets ("windows-amd64" / "linux-amd64" ...).
	Platform string

	// ValidateBinary probes the staged executable (version output,
	// exit code). Returning an error fails the transaction.
	ValidateBinary func(ctx context.Context, path string) (string, error)

	// SmokeTest launches the staged binary once and verifies it can
	// expose a local endpoint; it MUST fully terminate the process.
	SmokeTest func(ctx context.Context, path string) error

	// ExecutableName locates the executable inside the unpacked tree.
	ExecutableName string

	// MaxDownloadBytes caps the transfer (default 512 MiB).
	MaxDownloadBytes int64
}

// Dir is the provider's slot directory.
func (b *BinaryManager) Dir() string { return filepath.Join(b.RootDir, b.Name) }

// BinDir holds the activated executable.
func (b *BinaryManager) BinDir() string { return filepath.Join(b.Dir(), "bin") }

// StagingDir holds in-flight transactions.
func (b *BinaryManager) StagingDir() string { return filepath.Join(b.Dir(), "staging") }

// managedUserBinaryPath derives the content-addressed managed path
// for an adopted user-provided binary: the executable's platform name
// plus the first 16 hex chars of the source SHA-256. Same bytes →
// same path → adoption is idempotent and never renames over an
// in-use Windows image; different bytes → a different path, so a
// running older copy is never overwritten either.
func (b *BinaryManager) managedUserBinaryPath(sourceSHA256 string) string {
	name := executableFileName(b.ExecutableName)

	short := strings.ToLower(strings.TrimSpace(sourceSHA256))
	if len(short) > 16 {
		short = short[:16]
	}

	if len(short) < 16 { // not a real checksum: fall back to the plain name
		return filepath.Join(b.BinDir(), name)
	}

	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)

	return filepath.Join(b.BinDir(), stem+"-"+short+ext)
}

// ManifestPath persists install state.
func (b *BinaryManager) ManifestPath() string { return filepath.Join(b.Dir(), "manifest.json") }

// BinaryPath returns the CURRENT activated executable path (empty
// when none).
func (b *BinaryManager) BinaryPath() string {
	manifest := b.LoadManifest()
	if manifest.BinaryPath == "" {
		return ""
	}

	if _, err := os.Stat(manifest.BinaryPath); err != nil {
		return ""
	}

	return manifest.BinaryPath
}

// LoadManifest reads the manifest (zero value when absent/corrupt).
func (b *BinaryManager) LoadManifest() Manifest {
	data, err := os.ReadFile(b.ManifestPath())
	if err != nil {
		return Manifest{Name: b.Name, State: string(StateNotInstalled)}
	}

	var manifest Manifest

	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{Name: b.Name, State: string(StateNotInstalled)}
	}

	return manifest
}

// saveManifest persists atomically (temp + rename).
func (b *BinaryManager) saveManifest(manifest Manifest) error {
	if err := os.MkdirAll(b.Dir(), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}

	tmp := b.ManifestPath() + ".tmp"

	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, b.ManifestPath())
}

// Install runs the full transaction for one release.
func (b *BinaryManager) Install(ctx context.Context, release Release) error {
	logInstall(b.Name, "provider_install_start", map[string]any{
		"version": release.Version,
	})

	if err := os.MkdirAll(b.BinDir(), 0o700); err != nil {
		return b.fail(manifestStage("prepare"), err)
	}

	_ = os.RemoveAll(b.StagingDir())

	if err := os.MkdirAll(b.StagingDir(), 0o700); err != nil {
		return b.fail(manifestStage("prepare"), err)
	}

	// ---- DOWNLOAD --------------------------------------------------
	archivePath := filepath.Join(b.StagingDir(), release.AssetName)

	maxBytes := b.MaxDownloadBytes
	if maxBytes <= 0 {
		maxBytes = 512 << 20
	}

	options := httpx.DownloadOptions{
		DestPath:     archivePath,
		ExpectedSize: release.Size,
		MaxBytes:     maxBytes,
		StallTimeout: 30 * time.Second,
		MaxRetries:   4,
	}

	result, err := b.HTTP.Download(ctx, release.AssetURL, options)
	if err != nil {
		return b.fail(manifestStage("download"), err)
	}

	// ---- VERIFY ----------------------------------------------------
	// The SHA-256 authority is mandatory: a release without a
	// published checksum is refused (§9 "verify downloaded binaries
	// before execution").
	if strings.TrimSpace(release.SHA256) == "" {
		err := fmt.Errorf("release carries no published checksum; refusing to install unverified binary")

		return b.fail(manifestStage("verify"), err)
	}

	sum, err := fileSHA256(archivePath)
	if err != nil {
		return b.fail(manifestStage("verify"), err)
	}

	if !strings.EqualFold(sum, release.SHA256) {
		err := fmt.Errorf("checksum mismatch: downloaded %s, published %s", sum, release.SHA256)

		return b.fail(manifestStage("verify"), err)
	}

	if release.Size > 0 && result.Bytes != release.Size {
		err := fmt.Errorf("size mismatch: downloaded %d, published %d", result.Bytes, release.Size)

		return b.fail(manifestStage("verify"), err)
	}

	// ---- UNPACK ----------------------------------------------------
	unpacked := filepath.Join(b.StagingDir(), "unpacked")

	if err := unpackTarGz(archivePath, unpacked); err != nil {
		return b.fail(manifestStage("unpack"), err)
	}

	execPath, err := findExecutable(unpacked, b.ExecutableName)
	if err != nil {
		return b.fail(manifestStage("unpack"), err)
	}

	if err := ensureExecutable(execPath); err != nil {
		return b.fail(manifestStage("validate"), err)
	}

	// ---- VALIDATE ---------------------------------------------------
	version := ""

	if b.ValidateBinary != nil {
		version, err = b.ValidateBinary(ctx, execPath)
		if err != nil {
			return b.fail(manifestStage("validate"), err)
		}
	}

	// ---- SMOKE -----------------------------------------------------
	if b.SmokeTest != nil {
		if err := b.SmokeTest(ctx, execPath); err != nil {
			return b.fail(manifestStage("smoke"), err)
		}
	}

	// ---- ACTIVATE ---------------------------------------------------
	previous := b.LoadManifest()

	target := filepath.Join(b.BinDir(), executableFileName(b.ExecutableName))

	// Retain the previous binary for rollback.
	if previous.BinaryPath != "" && previous.BinaryPath != target {
		if _, statErr := os.Stat(previous.BinaryPath); statErr == nil {
			_ = os.Rename(previous.BinaryPath, previous.BinaryPath+".previous") //nolint:errcheck // best-effort retention
		}
	}

	if err := moveFile(execPath, target); err != nil {
		// Restore any retained previous binary.
		if previous.BinaryPath != "" {
			_ = os.Rename(previous.BinaryPath+".previous", previous.BinaryPath) //nolint:errcheck // best-effort rollback
		}

		return b.fail(manifestStage("activate"), err)
	}

	_ = os.RemoveAll(b.StagingDir())

	manifest := Manifest{
		Name:             b.Name,
		Version:          version,
		BinaryPath:       target,
		ChecksumSHA256:   sum,
		SourceURL:        release.AssetURL,
		ReleaseURL:       release.ReleaseURL,
		InstalledAt:      time.Now().UTC(),
		LastChecked:      time.Now().UTC(),
		State:            string(StateInstalled),
		PreviousVersion:  previous.Version,
		PreviousChecksum: previous.ChecksumSHA256,
		PreviousPath:     previous.BinaryPath + ".previous",
	}

	if err := b.saveManifest(manifest); err != nil {
		return b.fail(manifestStage("activate"), err)
	}

	logInstall(b.Name, "provider_install_complete", map[string]any{
		"version":  version,
		"checksum": sum[:16] + "…",
	})

	return nil
}

// Rollback restores the retained previous binary.
func (b *BinaryManager) Rollback() error {
	manifest := b.LoadManifest()
	if manifest.PreviousPath == "" {
		return fmt.Errorf("no previous binary to roll back to")
	}

	if _, err := os.Stat(manifest.PreviousPath); err != nil {
		return fmt.Errorf("previous binary missing: %w", err)
	}

	current := manifest.BinaryPath

	if err := moveFile(manifest.PreviousPath, current); err != nil {
		return err
	}

	manifest.BinaryPath = current
	manifest.Version = manifest.PreviousVersion
	manifest.ChecksumSHA256 = manifest.PreviousChecksum
	manifest.PreviousPath = ""
	manifest.PreviousVersion = ""
	manifest.PreviousChecksum = ""
	manifest.State = string(StateInstalled)
	manifest.FailureReason = ""
	manifest.FailureStage = ""

	return b.saveManifest(manifest)
}

// Uninstall removes the whole provider slot.
func (b *BinaryManager) Uninstall() error {
	return os.RemoveAll(b.Dir())
}

// MarkState persists a lifecycle state with optional failure info.
func (b *BinaryManager) MarkState(state LifecycleState, failureStage, failureReason string) error {
	manifest := b.LoadManifest()
	manifest.Name = b.Name
	manifest.State = string(state)
	manifest.FailureStage = failureStage
	manifest.FailureReason = failureReason

	if state == StateInstalled || state == StateReady {
		manifest.FailureStage = ""
		manifest.FailureReason = ""
	}

	return b.saveManifest(manifest)
}

// fail records the failure and cleans staging.
func (b *BinaryManager) fail(stage string, err error) error {
	_ = os.RemoveAll(b.StagingDir())

	manifest := b.LoadManifest()
	manifest.Name = b.Name
	manifest.State = string(StateFailed)
	manifest.FailureStage = stage
	manifest.FailureReason = err.Error()
	_ = b.saveManifest(manifest)

	logInstall(b.Name, "provider_install_failed", map[string]any{
		"stage":      stage,
		"error_kind": "install",
	})

	return fmt.Errorf("%s: %s: %w", b.Name, stage, err)
}

func manifestStage(stage string) string { return stage }

func logInstall(providerName, event string, fields map[string]any) {
	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     event,
		Message:   providerName + ": " + event,
		Fields:    fields,
	})
}

// ---- helpers ------------------------------------------------------

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}

	defer f.Close()

	hasher := sha256.New()

	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func ensureExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}

	if runtime.GOOS != "windows" {
		return os.Chmod(path, 0o755)
	}

	return nil
}

func executableFileName(name string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return name + ".exe"
	}

	return name
}

// findExecutable locates the named executable (or any executable
// matching the name) inside the unpacked tree.
func findExecutable(root, name string) (string, error) {
	want := strings.ToLower(executableFileName(name))

	var found string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		if strings.ToLower(d.Name()) == want {
			found = path
			return filepath.SkipAll
		}

		return nil
	})
	if err != nil && found == "" {
		return "", fmt.Errorf("executable %q not found in archive: %w", name, err)
	}

	if found == "" {
		return "", fmt.Errorf("executable %q not found in archive", name)
	}

	return found, nil
}

// moveFile moves across filesystems with a copy fallback. RESERVED
// for managed installation transactions (staging → activation),
// where FreeIran owns BOTH endpoints and the source is meant to be
// consumed. NEVER use it on user-provided files — see safeCopyFile.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}

	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	if runtime.GOOS != "windows" {
		_ = os.Chmod(dst, 0o755)
	}

	return os.Remove(src)
}

// safeCopyFile copies src to dst WITHOUT ever moving, renaming or
// deleting the source — the ownership contract for adopting
// user-provided binaries: the user's original file is theirs, and it
// must survive even when Windows temporarily holds it locked (an
// executable image mapping can outlive process termination, which
// makes os.Remove fail with "being used by another process").
//
// The copy is atomic from the caller's perspective: bytes land in a
// uniquely-named .part file which is renamed into place, so a partial
// copy can never be mistaken for a complete managed binary. When dst
// already exists with the same content it is reused untouched —
// repeated adoption of the same binary is a no-op that cannot hit
// Windows in-use overwrite problems on the destination either.
func safeCopyFile(src, dst, expectedSHA256 string) error {
	if info, err := os.Stat(src); err != nil || info.IsDir() {
		return fmt.Errorf("source binary %q is not a readable regular file: %w", src, err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	// Idempotent reuse: an identical managed copy already exists (same
	// size AND same checksum) — do not touch it, so a running image is
	// never renamed over.
	if existing, err := os.Stat(dst); err == nil && !existing.IsDir() {
		if expectedSHA256 != "" && existing.Size() > 0 {
			if sum, sumErr := fileSHA256(dst); sumErr == nil && strings.EqualFold(sum, expectedSHA256) {
				return nil
			}
		}
	}

	part := fmt.Sprintf("%s.part-%d", dst, os.Getpid())

	_ = os.Remove(part) // stale part from a crashed run (best-effort)

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source binary: %w", err)
	}

	defer in.Close()

	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("stage managed copy: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(part)

		return fmt.Errorf("copy managed binary: %w", err)
	}

	if err := out.Close(); err != nil {
		_ = os.Remove(part)

		return fmt.Errorf("close staged copy: %w", err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(part, 0o755); err != nil {
			_ = os.Remove(part)

			return err
		}
	}

	if err := os.Rename(part, dst); err != nil {
		_ = os.Remove(part)

		// A same-name destination that is busy (Windows, image still
		// mapped) is the one in-place conflict we can see here; surface
		// it honestly instead of deleting anything.
		return fmt.Errorf("activate managed copy: %w", err)
	}

	return nil
}

// unpackTarGz extracts a .tar.gz with tar-slip protection (same
// discipline as engine/coremgr).
func unpackTarGz(archivePath, dst string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}

	defer f.Close()

	gz, err := newGzipReader(f)
	if err != nil {
		return err
	}

	defer gz.Close()

	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}

	return extractTar(gz, dst)
}

// PlatformSuffix renders the asset platform token for GOOS/GOARCH.
func PlatformSuffix(goos, goarch string) string {
	osToken := map[string]string{
		"windows": "windows",
		"linux":   "linux",
		"darwin":  "macos",
	}[goos]

	archToken := map[string]string{
		"amd64": "x86_64",
		"arm64": "aarch64",
		"386":   "i686",
	}[goarch]

	if osToken == "" {
		osToken = goos
	}

	if archToken == "" {
		archToken = goarch
	}

	return osToken + "-" + archToken
}
