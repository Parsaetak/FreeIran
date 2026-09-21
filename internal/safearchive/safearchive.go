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
// v0.9.11 hardening: the sanitizer is HOST-INDEPENDENT. Archive
// entry names are validated in ARCHIVE space — lexically, with '/'
// as the only separator — BEFORE any host path conversion. The
// pre-0.9.11 sanitizer ran the host's filepath.Clean() over the raw
// name first: on Windows that translated "/" into "\" (so a
// POSIX-absolute entry such as "/etc/passwd-clone" became
// backslash-rooted and slipped past the absolute-entry test, only to
// be saved — or not — by the last-resort containment check), and the
// whole contract silently depended on host path semantics. The
// sanitized result must be IDENTICAL on every platform; the final
// containment decision is made with an OS-aware filepath.Rel check
// (case-insensitive on Windows) after the lexical gate.
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

	// v0.9.11: reject EVERY non-regular, non-directory type bit —
	// symlinks, devices, named pipes and sockets (the pre-0.9.11
	// code checked only the symlink bit). A remotely acquired
	// executable archive needs regular files and directories only.
	if !mode.IsDir() && mode.Type() != 0 {
		return fmt.Errorf("%w: zip entry %q has unsupported/hostile type %s (rejected)",
			ErrLimit, f.Name, mode.Type())
	}

	isDir := mode.IsDir()

	target, err := safeJoin(dst, f.Name, isDir)
	if err != nil {
		return err
	}

	if isDir {
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

	if err := writeBounded(target, rc, int64(f.UncompressedSize64), limits, total, f.Name); err != nil {
		return err
	}

	// v0.9.11 lying-header probe: an entry that carries MORE data
	// than its header declares is a malformed/hostile archive. The
	// pre-0.9.11 code silently truncated it (io.CopyN caps at the
	// declared size) — a corrupted file reported as success. The
	// stdlib checksumReader flags over-delivery with ErrFormat (and
	// swallows the offending byte), so the probe must reject on ANY
	// non-EOF outcome — a byte, or the reader's format error. The
	// zip entry reader is bounded to the entry's own compressed
	// stream, so one extra read distinguishes an honest EOF from
	// smuggled data. Fail closed.
	var probe [1]byte

	if n, err := io.ReadFull(rc, probe[:]); n > 0 || (err != nil && err != io.EOF) {
		return fmt.Errorf("%w: zip entry %q carries more data than declared (rejected)",
			ErrLimit, f.Name)
	}

	return nil
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
			target, err := safeJoin(dst, hdr.Name, true)
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
			target, err := safeJoin(dst, hdr.Name, false)
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
//
// v0.9.11 contract fix: a declared size of ZERO is a legitimate empty
// entry — it creates an empty file. The pre-0.9.11 code promoted a
// zero declared size to a 1 GiB hard ceiling and then treated the
// guaranteed EOF as "ended early", falsely rejecting every archive
// that legitimately carries an empty file. The zip layer probes for
// smuggled data behind a zero/lying header itself (see
// extractZipFile); the tar layer is structurally safe (hdr.Size
// exactly delimits the entry's stream segment).
func writeBounded(dst string, r io.Reader, declared int64, limits Limits, total *int64, name string) error {
	// Defense-in-depth budget check BEFORE opening the file: the
	// declared pre-checks should have caught this; never write a
	// partial entry and report success.
	if limits.MaxFileBytes > 0 && declared > limits.MaxFileBytes {
		return fmt.Errorf("%w: entry %q declares %d bytes (max %d)",
			ErrLimit, name, declared, limits.MaxFileBytes)
	}

	if limits.MaxTotalBytes > 0 && *total+declared > limits.MaxTotalBytes {
		return fmt.Errorf("%w: total would exceed %d bytes at entry %q",
			ErrLimit, limits.MaxTotalBytes, name)
	}

	if declared < 0 {
		// Pathological header (never produced by the stdlib readers).
		return fmt.Errorf("%w: entry %q declares a negative size %d", ErrLimit, name, declared)
	}

	if declared == 0 {
		// Legitimate empty entry: create the file, add nothing.
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("safearchive: create %q: %w", dst, err)
		}

		if err := out.Close(); err != nil {
			return fmt.Errorf("safearchive: close %q: %w", dst, err)
		}

		return nil
	}

	limit := declared

	if limits.MaxFileBytes > 0 && limit > limits.MaxFileBytes {
		limit = limits.MaxFileBytes
	}

	if limits.MaxTotalBytes > 0 && *total+limit > limits.MaxTotalBytes {
		limit = limits.MaxTotalBytes - *total
	}

	if limit < declared {
		return fmt.Errorf("%w: budget exhausted before entry %q (%d of %d declared bytes)",
			ErrLimit, name, limit, declared)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("safearchive: create %q: %w", dst, err)
	}

	written, cerr := io.CopyN(out, r, limit)

	closeErr := out.Close()

	if cerr == io.EOF {
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

// ---- archive-space path validation (host-independent) --------------------
//
// The contract: validation decisions about an ARCHIVE entry name are
// made in ARCHIVE space — lexically, with '/' as the only separator —
// and produce the SAME verdict on every platform. Only AFTER the
// lexical gate converts the name to a filesystem path does an
// OS-aware containment check (filepath.Rel) run, so the final target
// is provably inside the destination on each host's terms.

// maxEntryComponentBytes is the per-component sanity bound (the
// common filesystem limit); a longer component would fail at the OS
// layer anyway — reject it consistently on every platform instead.
const maxEntryComponentBytes = 255

// maxEntryPathBytes bounds the full normalized archive path.
const maxEntryPathBytes = 4096

// windowsReservedNames are the legacy Windows device names. A file
// named CON (with or without extension) is reserved on Windows; the
// set is rejected in archive space so extraction behaviour is
// identical on every platform.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// validateArchiveEntry validates one archive entry name and returns
// the destination-relative path in archive form ('/'-separated,
// already lexically cleaned, never escaping the archive root).
// An empty result means the name resolves to the archive root itself
// — valid ONLY for directory entries (a file entry at the root is a
// hostile degenerate form).
//
// Rejected (fail-closed), independent of the host OS:
//
//   - empty names (except directory forms that resolve to the root)
//   - NUL bytes and other control characters
//   - Windows device namespaces: \\?\, \\.\, \??\ (any case)
//   - POSIX-absolute names (/abs), Windows-rooted names (\abs)
//   - UNC forms (//server/share, \\server\share)
//   - drive forms (C:/abs, C:\abs, C:relative)
//   - ANY other colon usage (Windows alternate data streams and
//     every other unsafe colon form — remotely acquired archives
//     never need a colon in a file name)
//   - traversal outside the root (../evil, a/../../evil, a/../..)
//   - Windows reserved device names (CON, NUL, COM1.., with or
//     without extension)
//   - components with trailing dots or spaces (Windows strips them
//     on write: two distinct entries could silently collide)
//   - components longer than 255 bytes / paths longer than 4096
func validateArchiveEntry(name string, isDir bool) (string, error) {
	if name == "" {
		if isDir {
			// A directory entry with an empty name is the archive
			// root itself (seen in tarballs created as "tar czf x .").
			return "", nil
		}

		return "", fmt.Errorf("%w: empty entry name", ErrLimit)
	}

	if len(name) > maxEntryPathBytes {
		return "", fmt.Errorf("%w: entry name exceeds %d bytes (rejected)",
			ErrLimit, maxEntryPathBytes)
	}

	// Control characters (NUL included) are illegal in Windows paths
	// and hostile everywhere; reject them before any transformation.
	for i := 0; i < len(name); i++ {
		c := name[i]

		if c < 0x20 || c == 0x7f {
			return "", fmt.Errorf("%w: entry %q contains control character 0x%02x (rejected)",
				ErrLimit, name, c)
		}
	}

	// Windows device namespaces, checked on the RAW name (before any
	// separator translation) case-insensitively: \\?\, \\.\ and the
	// NT object-manager root-local form \??\.
	raw := name

	for _, prefix := range []string{`\\?\`, `\\.\`, `\??\`} {
		if hasPrefixFoldASCIISep(raw, prefix) {
			return "", fmt.Errorf("%w: entry %q uses a Windows device namespace (rejected)",
				ErrLimit, name)
		}
	}

	// Archive-space normalization: '\' is translated to '/' FIRST so
	// every later decision is made on one canonical separator and
	// cannot be subverted by host path semantics.
	canon := strings.ReplaceAll(raw, "\\", "/")

	// Absolute and UNC forms: any leading separator is absolute; two
	// leading separators are UNC.
	if strings.HasPrefix(canon, "/") {
		if strings.HasPrefix(canon, "//") {
			return "", fmt.Errorf("%w: UNC entry path %q (rejected)", ErrLimit, name)
		}

		return "", fmt.Errorf("%w: absolute entry path %q (rejected)", ErrLimit, name)
	}

	// Windows drive forms: "C:/x" (absolute), "C:\x" (absolute) and
	// "C:x" (drive-relative) all begin "X:".
	if len(canon) >= 2 && isASCIIAlpha(canon[0]) && canon[1] == ':' {
		return "", fmt.Errorf("%w: Windows drive entry path %q (rejected)", ErrLimit, name)
	}

	// ANY remaining colon is unsafe on Windows (alternate data
	// streams: "file.txt:stream" writes a hidden ADS) and never
	// legitimate in a remotely acquired archive. Reject in archive
	// space so behaviour is host-independent.
	if strings.ContainsRune(canon, ':') {
		return "", fmt.Errorf("%w: entry %q contains a colon (Windows stream/drive form, rejected)",
			ErrLimit, name)
	}

	// Walk the elements lexically against a virtual root.
	elements := strings.Split(canon, "/")

	stack := make([]string, 0, len(elements))

	for _, element := range elements {
		switch {
		case element == "" || element == ".":
			continue // duplicate separators, "./", trailing "/" (dirs)

		case element == "..":
			if len(stack) == 0 {
				return "", fmt.Errorf("%w: path traversal in entry %q (rejected)", ErrLimit, name)
			}

			stack = stack[:len(stack)-1]

		default:
			if len(element) > maxEntryComponentBytes {
				return "", fmt.Errorf("%w: entry %q has a component longer than %d bytes (rejected)",
					ErrLimit, name, maxEntryComponentBytes)
			}

			// Trailing dots/spaces: Windows silently strips them on
			// write, so two distinct entries could collide into one.
			last := element[len(element)-1]

			if last == '.' || last == ' ' {
				return "", fmt.Errorf("%w: entry %q has a component with a trailing dot or space (rejected)",
					ErrLimit, name)
			}

			// Reserved device names, with or without extension
			// (CON, CON.txt — case-insensitive).
			if dot := strings.IndexByte(element, '.'); dot >= 0 {
				if windowsReservedNames[strings.ToUpper(element[:dot])] {
					return "", fmt.Errorf("%w: entry %q uses a Windows reserved device name (rejected)",
						ErrLimit, name)
				}
			} else if windowsReservedNames[strings.ToUpper(element)] {
				return "", fmt.Errorf("%w: entry %q uses a Windows reserved device name (rejected)",
					ErrLimit, name)
			}

			stack = append(stack, element)
		}
	}

	if len(stack) == 0 {
		// The name resolved to the archive root.
		if isDir {
			return "", nil
		}

		return "", fmt.Errorf("%w: entry %q resolves to the archive root (rejected)", ErrLimit, name)
	}

	rel := strings.Join(stack, "/")

	if len(rel) > maxEntryPathBytes {
		return "", fmt.Errorf("%w: entry path exceeds %d bytes (rejected)", ErrLimit, maxEntryPathBytes)
	}

	return rel, nil
}

// safeJoin resolves one validated archive entry name to a path
// strictly inside dst. The lexical gate runs first (see
// validateArchiveEntry); the final containment decision is OS-aware:
// filepath.Rel is case-insensitive on Windows (sameWord → EqualFold)
// and exact on Unix, so the check respects the host's own path
// comparison rules. The host's filepath.Clean is never used to
// DECIDE whether an archive path is safe — only to render the
// already-validated relative path into host form.
func safeJoin(dst, name string, isDir bool) (string, error) {
	rel, err := validateArchiveEntry(name, isDir)
	if err != nil {
		return "", err
	}

	if rel == "" {
		// The archive root itself (directory entries only).
		return filepath.Clean(dst), nil
	}

	target := filepath.Join(dst, filepath.FromSlash(rel))

	root := filepath.Clean(dst)

	// OS-aware containment: rel must be a plain path inside root.
	// On Windows this comparison is case-insensitive; on Unix exact.
	relOS, err := filepath.Rel(root, target)
	if err != nil {
		return "", fmt.Errorf("%w: entry %q cannot be contained in the destination (rejected)",
			ErrLimit, name)
	}

	if relOS == ".." || strings.HasPrefix(relOS, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: path traversal in entry %q (rejected)", ErrLimit, name)
	}

	// Belt-and-braces: the target must be the root (directory root
	// entries) or carry the root as a path prefix.
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: path traversal in entry %q (rejected)", ErrLimit, name)
	}

	return target, nil
}

// isASCIIAlpha reports whether c is an ASCII letter (drive-letter
// check; other code points were already rejected by the control
// character scan or survive as ordinary name bytes).
func isASCIIAlpha(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// hasPrefixFoldASCIISep reports whether s begins with prefix,
// ignoring case and treating '/' and '\' as equivalent separators.
// Used for the raw device-namespace probes.
func hasPrefixFoldASCIISep(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}

	for i := 0; i < len(prefix); i++ {
		ps := prefix[i]

		if ps == '\\' || ps == '/' {
			if s[i] != '\\' && s[i] != '/' {
				return false
			}

			continue
		}

		if toUpperASCII(ps) != toUpperASCII(s[i]) {
			return false
		}
	}

	return true
}

// toUpperASCII upper-cases one ASCII byte.
func toUpperASCII(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}

	return c
}
