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
	"github.com/Parsaetak/FreeIran/internal/safearchive"
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

	// ---- v0.9.14: ownership and provenance --------------------------
	//
	// Ownership is "managed" (FreeIran-owned binary inside the
	// provider slot) or "external" (an already-installed engine the
	// provider only REFERENCES — never deleted, renamed or
	// overwritten). Empty keeps the historical default: managed.
	Ownership string `json:"ownership,omitempty"`

	// Origin records where the active binary came from: "managed"
	// (downloaded bundle), "path" (OS PATH), "system" (known
	// installation location) or "user" (user-supplied adoption).
	Origin string `json:"origin,omitempty"`

	// ExternalPath is the referenced external executable's canonical
	// path (equal to BinaryPath while an external reference is
	// active).
	ExternalPath string `json:"external_path,omitempty"`

	// Acquisition records HOW the active binary was acquired, so the
	// UI can distinguish the honest provider states instead of
	// collapsing them into one "installed" boolean:
	//
	//   "managed-release"    downloaded from an authoritative,
	//                        digest-publishing channel and verified
	//   "user-binary"        user-supplied, content-addressed managed
	//                        copy (validated + smoke-tested)
	//   "external-reference" discovered external installation adopted
	//                        by reference (never modified)
	//
	// Empty = the manifest predates the field or no binary is active.
	Acquisition string `json:"acquisition,omitempty"`
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

	// v0.9.14: staging is NOT wiped wholesale — a complete staged
	// archive from an interrupted install is verified and reused, a
	// partial one is resumed by the downloader, and only artifacts of
	// a DIFFERENT release are removed.
	if err := os.MkdirAll(b.StagingDir(), 0o700); err != nil {
		return b.fail(manifestStage("prepare"), err)
	}

	if err := cleanStaleProviderStaging(b.StagingDir(), release.AssetName); err != nil {
		return b.fail(manifestStage("prepare"), err)
	}

	// ---- DOWNLOAD (only when no reusable artifact exists) ----------
	archivePath := filepath.Join(b.StagingDir(), release.AssetName)

	result, downloadErr := b.downloadOrReuse(ctx, archivePath, release)
	if downloadErr != nil {
		return b.fail(manifestStage("download"), downloadErr)
	}

	// ---- VERIFY ----------------------------------------------------
	// The SHA-256 authority is mandatory: a release without a
	// published checksum is refused (§9 "verify downloaded binaries
	// before execution").
	if strings.TrimSpace(release.SHA256) == "" {
		err := fmt.Errorf("release carries no published checksum; refusing to install unverified binary")

		// The staged artifact can never be verified against an absent
		// authority: it is unsafe data, not resumable state — remove
		// that artifact alone before recording the failure.
		_ = os.Remove(archivePath)

		return b.fail(manifestStage("verify"), err)
	}

	sum, err := fileSHA256(archivePath)
	if err != nil {
		return b.fail(manifestStage("verify"), err)
	}

	if !strings.EqualFold(sum, release.SHA256) {
		err := fmt.Errorf("checksum mismatch: downloaded %s, published %s", sum, release.SHA256)

		// PROVEN unsafe: a complete artifact whose digest does not
		// match the published authority is removed — that artifact
		// alone. Resumable .part bytes and every other staging entry
		// survive (see fail/reconcileStagingAfterFailure).
		_ = os.Remove(archivePath)

		return b.fail(manifestStage("verify"), err)
	}

	if release.Size > 0 && result.Bytes != release.Size {
		err := fmt.Errorf("size mismatch: downloaded %d, published %d", result.Bytes, release.Size)

		// Same proof standard as the checksum mismatch above.
		_ = os.Remove(archivePath)

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
		var verr error

		version, verr = b.ValidateBinary(ctx, execPath)
		if verr != nil {
			return b.fail(manifestStage("validate"), verr)
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

	// v0.9.14 ownership guard: an EXTERNAL binary (system-discovered
	// or user-adopted original) is NEVER moved aside or overwritten —
	// only FreeIran's reference changes. Rollback retention applies to
	// managed binaries exclusively.
	previousIsExternal := previous.Ownership == string(OwnershipExternal)

	// Retain the previous binary for rollback.
	if !previousIsExternal && previous.BinaryPath != "" && previous.BinaryPath != target {
		if _, statErr := os.Stat(previous.BinaryPath); statErr == nil {
			_ = os.Rename(previous.BinaryPath, previous.BinaryPath+".previous") //nolint:errcheck // best-effort retention
		}
	}

	// v0.9.15: the archive ships the executable with SIBLINGS it needs
	// at runtime — shared libraries next to the Linux binary, pluggable
	// transports beside the Windows one (verified live: the 15.0.23
	// bundle carries tor/libcrypto.so.3 + libevent + libssl, without
	// which the validated binary could not launch). Move those payload
	// siblings into bin/ alongside the executable; the retained
	// .previous binary is never touched.
	if err := movePayloadSiblings(filepath.Dir(execPath), b.BinDir(), filepath.Base(execPath), filepath.Base(target)); err != nil {
		return b.fail(manifestStage("activate"), err)
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
		// v0.9.14: a downloaded-and-activated bundle is FreeIran-owned.
		Ownership:   string(OwnershipManaged),
		Origin:      string(OriginManaged),
		Acquisition: "managed-release",
	}
	if previousIsExternal {
		manifest.PreviousPath = ""
		manifest.PreviousVersion = ""
		manifest.PreviousChecksum = ""
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

// fail records the failure and reconciles the staging directory to
// the transaction's recovery semantics (v0.9.15 — the pre-fix
// implementation removed the WHOLE staging directory, destroying the
// exact resumable and reuse-ready state the downloader preserves).
func (b *BinaryManager) fail(stage string, err error) error {
	b.reconcileStagingAfterFailure(stage)

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

// reconcileStagingAfterFailure removes ONLY the staging state the
// failed transaction has proven unsafe, and never the recoverable
// state. The transaction must stay recoverable after an application
// crash, process termination, network interruption, download timeout,
// checksum failure, extraction/validation/smoke failure or activation
// failure:
//
//	prepare    nothing recoverable was staged yet.
//	download   the .part file IS the resumable transfer and a complete
//	           artifact (if any) is re-validated by downloadOrReuse on
//	           the next attempt — keep everything.
//	verify     the proven-unsafe artifact was removed where it was
//	           proven (checksum/size/authority checks); resumable and
//	           verified bytes stay.
//	unpack, validate, smoke, activate
//	           the archive already passed checksum verification, so it
//	           is kept for artifact reuse on retry; only the derived
//	           unpacked tree (possibly half-moved or corrupt) is
//	           discarded — it is rebuilt on every attempt.
//
// bin/ (the last known-good activated binary) and manifest.json are
// never touched by a failed transaction.
func (b *BinaryManager) reconcileStagingAfterFailure(stage string) {
	if stage == manifestStage("prepare") ||
		stage == manifestStage("download") ||
		stage == manifestStage("verify") {
		return
	}

	_ = os.RemoveAll(filepath.Join(b.StagingDir(), "unpacked"))
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
	return executableFileNameFor(runtime.GOOS, name)
}

// executableFileNameFor is the pure platform-naming core of
// executableFileName: "tor" gains ".exe" on Windows (and only there),
// an already-.exe name is kept as-is, POSIX names pass through. It
// exists so the Windows/POSIX fixture contract stays unit-testable on
// every build host (the v0.9.15 Windows CI failure was exactly a
// POSIX-named fixture meeting a Windows-named expectation).
func executableFileNameFor(goos, name string) string {
	if goos == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return name + ".exe"
	}

	return name
}

// findExecutable locates the named executable inside the unpacked
// tree. Recent expert bundles ship a debug/ variant NEXT to the real
// binary (debug/tor, debug/tor.exe), so the first walk hit is NOT
// necessarily the real engine (verified live, v0.9.15). Matches are
// scored deterministically: no "debug" path segment beats a debug
// variant, a shallower path beats a deeper one, then lexicographic
// order keeps the choice stable.
func findExecutable(root, name string) (string, error) {
	want := strings.ToLower(executableFileName(name))

	var matches []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		if strings.ToLower(d.Name()) == want {
			matches = append(matches, path)
		}

		return nil
	})
	if err != nil && len(matches) == 0 {
		return "", fmt.Errorf("executable %q not found in archive: %w", name, err)
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("executable %q not found in archive", name)
	}

	best := matches[0]
	bestScore := scoreExecutablePath(root, best)

	for _, candidate := range matches[1:] {
		if score := scoreExecutablePath(root, candidate); score < bestScore {
			best = candidate
			bestScore = score
		}
	}

	return best, nil
}

// scoreExecutablePath ranks one match: debug variants are worst, then
// deeper paths, then the lexicographic tail.
func scoreExecutablePath(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}

	score := 0

	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(segment, "debug") {
			score += 1000 // debug builds never win
		}
	}

	score += strings.Count(filepath.ToSlash(rel), "/") * 10

	return score
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

// unpackTarGz extracts a .tar.gz through internal/safearchive
// (v0.9.8.6): bounded archive/total/per-file/file-count limits,
// tar-slip and absolute-path rejection, symlink/hardlink rejection
// and fail-closed handling of malformed archives — the same
// discipline engine/coremgr now uses, in ONE shared implementation.
func unpackTarGz(archivePath, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}

	return safearchive.Unpack(archivePath, dst, safearchive.DefaultLimits())
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

// movePayloadSiblings moves the archive payload entries that sit NEXT
// to the staged executable (libraries, pluggable transports, support
// data) into the bin directory, preserving every archive sibling. The
// executable itself is excluded (activation moves it separately) and
// the retained .previous rollback binary is never touched. A move
// failure falls back to a copy so cross-device staging still
// activates. (v0.9.15: the historical moveTreePayload was never wired
// into activation at all — a single-file move left the validated
// binary without its runtime siblings.)
func movePayloadSiblings(payloadDir, toDir, execName, targetName string) error {
	if err := os.MkdirAll(toDir, 0o700); err != nil {
		return err
	}

	entries, err := os.ReadDir(payloadDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.Name() == execName || entry.Name() == targetName {
			continue // the executable itself: activation's moveFile handles it
		}

		src := filepath.Join(payloadDir, entry.Name())
		dst := filepath.Join(toDir, entry.Name())

		// Refresh in place: remove a stale destination entry of the same
		// name — never the retained .previous binary.
		if _, err := os.Stat(dst); err == nil {
			if strings.HasSuffix(dst, ".previous") {
				continue
			}

			if err := os.RemoveAll(dst); err != nil {
				return err
			}
		}

		if err := os.Rename(src, dst); err != nil {
			if err := copyTreeEntry(src, dst); err != nil {
				return err
			}
		}
	}

	return nil
}

// copyTreeEntry copies one file or directory recursively (move
// fallback for cross-device staging).
func copyTreeEntry(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}

		return os.WriteFile(dst, data, info.Mode().Perm())
	}

	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)

	for _, entry := range entries {
		if err := copyTreeEntry(
			filepath.Join(src, entry.Name()),
			filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}

	return nil
}
