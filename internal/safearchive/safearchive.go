// Package safearchive implements bounded, fail-closed archive
// extraction for every remotely acquired artifact in FreeIran
// (protocol-core releases, provider binaries, any future archive).
//
// v0.9.8.6 security contract — every extraction enforces:
//
//   - a maximum ARCHIVE size (the on-disk staged file is checked
//     before a single byte is unpacked);
//   - a maximum TOTAL extracted byte budget (zip-bomb defence: a
//     100-byte archive must not be able to exhaust the disk);
//   - a maximum INDIVIDUAL file size;
//   - a maximum FILE COUNT;
//   - path-traversal protection (zip-slip / tar-slip: every entry
//     must resolve strictly inside the destination);
//   - absolute-path rejection (POSIX "/abs" and Windows "C:\abs",
//     "\\abs" and drive-relative forms);
//   - symlink / hardlink entry rejection — a remotely acquired
//     executable archive never needs links, and following
//     attacker-controlled link targets is unacceptable. Other
//     non-regular entries (devices, fifos) are rejected as well:
//     malformed/hostile archives fail CLOSED, they never extract
//     partially and report success.
//
// Supported formats: .zip, .tar, .tar.gz/.tgz, with content sniffing
// (PK / gzip magic) for misnamed staged files — mirroring the
// historical engine/coremgr behaviour.
package safearchive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Limits bounds one extraction. Zero fields mean "no limit of this
// kind" — but callers handling remotely acquired archives must always
// set all four (DefaultLimits provides the shared safe values).
type Limits struct {
	// MaxArchiveBytes bounds the staged archive FILE size. The check
	// runs before any entry is read.
	MaxArchiveBytes int64

	// MaxTotalBytes bounds the sum of all extracted file sizes.
	MaxTotalBytes int64

	// MaxFileBytes bounds one extracted file.
	MaxFileBytes int64

	// MaxFiles bounds the number of extracted entries (files and
	// directories).
	MaxFiles int
}

// DefaultLimits returns the shared extraction budget for FreeIran's
// remotely acquired core/provider archives: archives up to 512 MiB,
// total expansion capped at 2 GiB, single files capped at 1 GiB and
// at most 4096 entries. These comfortably cover every official
// Xray/V2Ray/sing-box/Tor/Psiphon release shape while keeping a
// hostile archive orders of magnitude away from exhausting disk or
// memory.
func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes: 512 << 20,
		MaxTotalBytes:   2 << 30,
		MaxFileBytes:    1 << 30,
		MaxFiles:        4096,
	}
}

// ErrLimit is the fail-closed family: the archive exceeded a bound or
// carried a hostile entry. Extraction stopped; the destination holds
// a PARTIAL tree which callers must discard (the coremgr/provider
// pipelines unpack into staging and remove it on failure).
var ErrLimit = errors.New("safearchive: archive exceeded safety limits")

// Unpack extracts archivePath into dst with the given limits.
// The format is selected by file extension, falling back to content
// sniffing (PK zip magic / gzip magic) for misnamed files.
func Unpack(archivePath, dst string, limits Limits) error {
	if err := checkArchiveSize(archivePath, limits); err != nil {
		return err
	}

	name := strings.ToLower(filepath.Base(archivePath))

	switch {
	case strings.HasSuffix(name, ".zip"):
		return unpackZip(archivePath, dst, limits)
	case strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz"):
		return unpackTarGz(archivePath, dst, limits)
	case strings.HasSuffix(name, ".tar"):
		return unpackTar(archivePath, dst, limits)
	default:
		magic := make([]byte, 2)

		if f, err := os.Open(archivePath); err == nil {
			_, _ = io.ReadFull(f, magic)
			_ = f.Close()
		}

		switch {
		case bytes.Equal(magic, []byte("PK")):
			return unpackZip(archivePath, dst, limits)
		case bytes.Equal(magic, []byte{0x1f, 0x8b}):
			return unpackTarGz(archivePath, dst, limits)
		default:
			return fmt.Errorf("safearchive: unknown archive format: %s", name)
		}
	}
}

// checkArchiveSize enforces MaxArchiveBytes before any entry is read.
func checkArchiveSize(archivePath string, limits Limits) error {
	if limits.MaxArchiveBytes <= 0 {
		return nil
	}

	info, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("safearchive: stat archive: %w", err)
	}

	if info.Size() > limits.MaxArchiveBytes {
		return fmt.Errorf("%w: archive is %d bytes (max %d)",
			ErrLimit, info.Size(), limits.MaxArchiveBytes)
	}

	return nil
}

// ---- zip ---------------------------------------------------------------

func unpackZip(archivePath, dst string, limits Limits) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("safearchive: open zip: %w", err)
	}
	defer r.Close()

	if limits.MaxFiles > 0 && len(r.File) > limits.MaxFiles {
		return fmt.Errorf("%w: zip declares %d entries (max %d)",
			ErrLimit, len(r.File), limits.MaxFiles)
	}

	var total int64

	for _, f := range r.File {
		if err := extractZipFile(f, dst, limits, &total); err != nil {
			return err
		}
	}

	return nil
}

func extractZipFile(f *zip.File, dst string, limits Limits, total *int64) error {
	mode := f.FileInfo().Mode()

	if mode&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: zip entry %q is a symlink (rejected)", ErrLimit, f.Name)
	}

	target, err := safeJoin(dst, f.Name)
	if err != nil {
		return err
	}

	if f.FileInfo().IsDir() {
		return os.MkdirAll(target, 0o700)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}

	// Declared-size pre-check (the zip central directory carries the
	// uncompressed size): a lying header still cannot exceed the
	// runtime limit below.
	if limits.MaxFileBytes > 0 && int64(f.UncompressedSize64) > limits.MaxFileBytes {
		return fmt.Errorf("%w: zip entry %q declares %d bytes (max %d)",
			ErrLimit, f.Name, f.UncompressedSize64, limits.MaxFileBytes)
	}

	if limits.MaxTotalBytes > 0 && *total+int64(f.UncompressedSize64) > limits.MaxTotalBytes {
		return fmt.Errorf("%w: zip total would exceed %d bytes at entry %q",
			ErrLimit, limits.MaxTotalBytes, f.Name)
	}

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("safearchive: open zip entry %q: %w", f.Name, err)
	}
	defer rc.Close()

	return writeBounded(target, rc, int64(f.UncompressedSize64), limits, total, f.Name)
}

// ---- tar ----------------------------------------------------------------

func unpackTarGz(archivePath, dst string, limits Limits) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("safearchive: open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("safearchive: gzip header: %w", err)
	}
	defer gz.Close()

	return unpackTarReader(tar.NewReader(gz), dst, limits)
}

func unpackTar(archivePath, dst string, limits Limits) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("safearchive: open archive: %w", err)
	}
	defer f.Close()

	return unpackTarReader(tar.NewReader(f), dst, limits)
}

func unpackTarReader(tr *tar.Reader, dst string, limits Limits) error {
	var (
		total   int64
		entries int
	)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("safearchive: tar stream: %w", err)
		}

		entries++

		if limits.MaxFiles > 0 && entries > limits.MaxFiles {
			return fmt.Errorf("%w: tar carries more than %d entries", ErrLimit, limits.MaxFiles)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			target, err := safeJoin(dst, hdr.Name)
			if err != nil {
				return err
			}

			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}

		case tar.TypeReg:
			// Symlinks, hardlinks, devices, fifos and any other special
			// entry fail CLOSED (rejected below): a remotely acquired
			// executable archive never needs them, and following
			// attacker-controlled link targets is unacceptable.
			target, err := safeJoin(dst, hdr.Name)
			if err != nil {
				return err
			}

			if limits.MaxFileBytes > 0 && hdr.Size > limits.MaxFileBytes {
				return fmt.Errorf("%w: tar entry %q declares %d bytes (max %d)",
					ErrLimit, hdr.Name, hdr.Size, limits.MaxFileBytes)
			}

			if limits.MaxTotalBytes > 0 && total+hdr.Size > limits.MaxTotalBytes {
				return fmt.Errorf("%w: tar total would exceed %d bytes at entry %q",
					ErrLimit, limits.MaxTotalBytes, hdr.Name)
			}

			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}

			if err := writeBounded(target, tr, hdr.Size, limits, &total, hdr.Name); err != nil {
				return err
			}

			if hdr.Mode&0o111 != 0 {
				_ = os.Chmod(target, 0o755)
			}

		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("%w: tar entry %q has unsupported/hostile type %q (rejected)",
				ErrLimit, hdr.Name, string(rune(hdr.Typeflag)))

		default:
			return fmt.Errorf("%w: tar entry %q has unknown type flag %d (rejected)",
				ErrLimit, hdr.Name, hdr.Typeflag)
		}
	}
}

// ---- shared helpers -----------------------------------------------------

// writeBounded streams one entry to disk enforcing the per-file and
// total budgets AT RUNTIME (the declared size can lie; io.CopyN never
// trusts it).
func writeBounded(dst string, r io.Reader, declared int64, limits Limits, total *int64, name string) error {
	limit := declared

	if limit <= 0 {
		limit = 1 << 30 // pathological header: hard ceiling
	}

	if limits.MaxFileBytes > 0 && limit > limits.MaxFileBytes {
		limit = limits.MaxFileBytes
	}

	if limits.MaxTotalBytes > 0 && *total+limit > limits.MaxTotalBytes {
		limit = limits.MaxTotalBytes - *total
	}

	if limit < 0 {
		return fmt.Errorf("%w: budget exhausted before entry %q", ErrLimit, name)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("safearchive: create %q: %w", dst, err)
	}

	written, cerr := io.CopyN(out, r, limit)

	closeErr := out.Close()

	if cerr == io.EOF {
		cerr = nil // wrote exactly `limit` bytes: a full entry
	}

	if cerr == nil && written < limit {
		// The entry ended EARLY (fewer bytes than declared): that is a
		// truncated/malformed archive. Tar entries always deliver
		// exactly hdr.Size bytes; a short zip entry is equally broken.
		cerr = fmt.Errorf("%w: entry %q ended after %d of %d declared bytes",
			ErrLimit, name, written, limit)
	}

	*total += written

	if cerr != nil {
		return cerr
	}

	if closeErr != nil {
		return fmt.Errorf("safearchive: close %q: %w", dst, closeErr)
	}

	return nil
}

// safeJoin resolves one archive entry name to a path strictly inside
// dst. It rejects absolute names (POSIX and Windows forms), traversal
// outside dst, and empty names.
func safeJoin(dst, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty entry name", ErrLimit)
	}

	cleaned := filepath.Clean(strings.ReplaceAll(name, "\\", "/"))

	if strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: absolute entry path %q (rejected)", ErrLimit, name)
	}

	// Windows drive forms ("C:/x", "C:x") and UNC ("//server/share")
	// survive the slash translation above; reject any name whose
	// second character is ':'.
	if len(cleaned) >= 2 && cleaned[1] == ':' {
		return "", fmt.Errorf("%w: Windows drive entry path %q (rejected)", ErrLimit, name)
	}

	target := filepath.Join(dst, cleaned)

	root := filepath.Clean(dst)

	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: path traversal in entry %q (rejected)", ErrLimit, name)
	}

	return target, nil
}
