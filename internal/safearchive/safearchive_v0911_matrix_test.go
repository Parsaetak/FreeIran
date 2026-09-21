// safearchive_v0911_matrix_test.go — the v0.9.11 cross-platform
// archive-security matrix.
//
// The v0.9.10 sanitizer ran the host's filepath.Clean() over archive
// entry names BEFORE validating them, so its verdicts depended on the
// host OS (on Windows a POSIX-absolute entry such as "/etc/x" became
// backslash-rooted and slipped past the absolute-entry test). The
// contract under test here: the sanitizer's verdict for every entry
// name below is IDENTICAL on Linux and Windows, extraction is
// fail-closed, and honest archives — including EMPTY entries, which
// the pre-0.9.11 code falsely rejected — still extract.
package safearchive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostileEntryNames is the cross-platform rejection matrix: every
// name here must fail with ErrLimit on EVERY platform, through zip
// and tar alike.
//
//	POSIX absolute            /absolute
//	Windows rooted            \absolute
//	Windows drive absolute    C:/absolute, C:\absolute
//	Windows drive relative    C:relative
//	UNC forms                 //UNC/share, \\UNC\share
//	Device namespaces         \\?\C:\x, \\.\x, \??\x
//	Traversal                 ../evil, a/../../evil, a/../..
//	Mixed separators          ..\..\evil, a/..\evil
//	Alternate data streams    file.txt:stream
//	Reserved device names     CON, con.txt, NUL, com1
//	Windows write quirks      trailing dot / trailing space
//	Degenerate                "" (file), "." (file)
func hostileEntryNames() []string {
	return []string{
		// POSIX absolute
		"/absolute",
		"/etc/passwd-clone",
		"/absolute/deeper/file",
		// Windows rooted (single backslash)
		`\absolute`,
		`\absolute\deeper\file`,
		// Windows drive forms
		"C:/absolute",
		`C:\absolute`,
		"C:relative-drive-form",
		`C:relative`,
		"c:/lowercase-drive",
		// UNC
		"//UNC/share",
		`\\UNC\share`,
		`\\server\share\evil.txt`,
		// Device namespaces
		`\\?\C:\evil`,
		`\\?\C:\absolute`,
		`\\.\evil`,
		`\\.\PhysicalDrive0`,
		`\??\evil`,
		`\??\C:\evil`,
		// Traversal
		"../evil",
		"a/../../evil",
		"a/../..",
		"../../evil",
		"....//....//evil",
		"a/./../../evil",
		// Mixed separators
		`..\..\evil`,
		`a\..\..\evil`,
		`dir\sub/../..`,
		// Windows alternate-data-stream / unsafe colon usage
		"file.txt:stream",
		"x:y",
		"dir/ads:hidden",
		// Reserved device names (with and without extension, any case)
		"CON",
		"con",
		"CON.txt",
		"nul",
		"NUL.zip",
		"COM1",
		"com5.log",
		"LPT9",
		// Trailing dot / space components (Windows write-collision quirk)
		"trailing.",
		"trailing ",
		"dir./file",
		"dir /file",
		// Degenerate file forms
		"",
		".",
		"./",
	}
}

// writerEmbeddable filters the degenerate forms the standard-library
// ARCHIVE WRITERS refuse to embed ("", ".", "./" as file entries).
// Those forms are pinned authoritatively by
// TestMatrixSanitizerRejectedForms against the sanitizer itself; the
// embedded-archive tests prove the end-to-end extraction behaviour
// for every name a real hostile archive can carry.
func writerEmbeddable(names []string) []string {
	kept := make([]string, 0, len(names))

	for _, name := range names {
		switch name {
		case "", ".", "./":
			continue
		}

		kept = append(kept, name)
	}

	return kept
}

// TestMatrixSanitizerRejectedForms is the AUTHORITATIVE
// cross-platform contract: the sanitizer itself (validateArchiveEntry,
// file-entry form) rejects every hostile name. This runs the full
// matrix — including the degenerate forms the standard-library
// writers cannot embed — with zero filesystem interaction, so it is
// byte-for-byte identical on Linux and Windows.
func TestMatrixSanitizerRejectedForms(t *testing.T) {
	for _, name := range hostileEntryNames() {
		if _, err := validateArchiveEntry(name, false); !errors.Is(err, ErrLimit) {
			t.Fatalf("sanitizer accepted file entry %q (err=%v)", name, err)
		}

		// Directory form: only names that resolve to the ARCHIVE ROOT
		// (the "./"-style entries real tarballs carry) are accepted;
		// everything else stays hostile as a directory too.
		if name == "" || name == "." || name == "./" {
			if rel, err := validateArchiveEntry(name, true); err != nil || rel != "" {
				t.Fatalf("directory root form %q = (%q, %v)", name, rel, err)
			}

			continue
		}

		rel, dirErr := validateArchiveEntry(name, true)

		if dirErr != nil && !errors.Is(dirErr, ErrLimit) {
			t.Fatalf("directory entry %q produced unexpected error %v", name, dirErr)
		}

		if dirErr == nil && rel != "" {
			t.Fatalf("sanitizer accepted directory entry %q (rel=%q)", name, rel)
		}
	}
}

// TestMatrixHostileNamesRejectedInZip runs the hostile-name matrix
// through the ZIP reader path (end-to-end extraction).
func TestMatrixHostileNamesRejectedInZip(t *testing.T) {
	for _, name := range writerEmbeddable(hostileEntryNames()) {
		path := writeZip(t, map[string]string{name: "payload"})

		err := Unpack(path, t.TempDir(), testLimits())

		if !errors.Is(err, ErrLimit) {
			t.Fatalf("zip entry %q unpacked with err=%v, want ErrLimit", name, err)
		}
	}
}

// TestMatrixHostileNamesRejectedInTar runs the hostile-name matrix
// through the TAR reader path (end-to-end extraction).
func TestMatrixHostileNamesRejectedInTar(t *testing.T) {
	for _, name := range writerEmbeddable(hostileEntryNames()) {
		var buf bytes.Buffer

		tw := tar.NewWriter(&buf)

		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Size: 7, Mode: 0o600,
		}); err != nil {
			// A name the tar WRITER itself refuses is still a rejected
			// entry for our purposes — but the stdlib writer accepts
			// every name below; a failure here is a test bug.
			t.Fatalf("tar writer refused %q: %v", name, err)
		}

		if _, err := tw.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}

		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}

		path := writeFile(t, buf.Bytes(), ".tar")

		if err := Unpack(path, t.TempDir(), testLimits()); !errors.Is(err, ErrLimit) {
			t.Fatalf("tar entry %q unpacked with err=%v, want ErrLimit", name, err)
		}
	}
}

// TestMatrixHostileNamesRejectedInTarGz proves the gzip wrapper does
// not weaken validation (a hostile name must fail through .tar.gz
// exactly like .tar).
func TestMatrixHostileNamesRejectedInTarGz(t *testing.T) {
	for _, name := range []string{
		"/absolute", `C:\absolute`, `\\?\C:\evil`, "../evil", `..\..\evil`,
		"//UNC/share", "file.txt:stream", "CON", "trailing.",
	} {
		var buf bytes.Buffer

		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)

		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Size: 7, Mode: 0o600,
		}); err != nil {
			t.Fatalf("tar writer refused %q: %v", name, err)
		}

		if _, err := tw.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}

		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}

		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}

		path := writeFile(t, buf.Bytes(), ".tar.gz")

		if err := Unpack(path, t.TempDir(), testLimits()); !errors.Is(err, ErrLimit) {
			t.Fatalf("tar.gz entry %q unpacked with err=%v, want ErrLimit", name, err)
		}
	}
}

// TestMatrixDeepInsideRootResolves keeps the positive traversal
// boundary: a path that resolves strictly INSIDE the root stays
// allowed (../.. rejected, a/../b allowed). Mixed-separator forms
// normalize in archive space before the resolver runs, so `a/..\evil`
// is the same path as `a/../evil` and resolves to "evil" — inside.
func TestMatrixDeepInsideRootResolves(t *testing.T) {
	path := writeZip(t, map[string]string{
		"a/../b.txt":    "inside",
		"x/./y.txt":     "inside",
		`a/..\evil.txt`: "inside",
	})

	dst := t.TempDir()

	if err := Unpack(path, dst, testLimits()); err != nil {
		t.Fatalf("inside-root traversal unpack: %v", err)
	}

	for _, rel := range []string{"b.txt", filepath.Join("x", "y.txt")} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Fatalf("entry %s missing after extract: %v", rel, err)
		}
	}
}

// TestMatrixZipSpecialTypeBitsRejected proves every non-regular,
// non-directory zip type bit fails closed: symlink, device, named
// pipe, socket.
func TestMatrixZipSpecialTypeBitsRejected(t *testing.T) {
	cases := map[string]os.FileMode{
		"symlink": os.ModeSymlink | 0o777,
		"device":  os.ModeDevice | os.ModeCharDevice | 0o600,
		"fifo":    os.ModeNamedPipe | 0o600,
		"socket":  os.ModeSocket | 0o600,
	}

	for name, mode := range cases {
		var buf bytes.Buffer

		zw := zip.NewWriter(&buf)

		header := &zip.FileHeader{Name: "special"}

		header.SetMode(mode)

		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatalf("%s: create header: %v", name, err)
		}

		if _, err := w.Write([]byte("payload")); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}

		if err := zw.Close(); err != nil {
			t.Fatalf("%s: close: %v", name, err)
		}

		path := writeFile(t, buf.Bytes(), ".zip")

		if err := Unpack(path, t.TempDir(), testLimits()); !errors.Is(err, ErrLimit) {
			t.Fatalf("zip %s entry unpacked with err=%v, want ErrLimit", name, err)
		}
	}
}

// TestMatrixEmptyEntriesExtract proves the v0.9.11 fix: a legitimate
// EMPTY entry (declared size 0) creates an empty file on every
// platform. The pre-0.9.11 code falsely rejected every archive that
// carried an empty file ("ended after 0 of 1073741824 declared
// bytes").
func TestMatrixEmptyEntriesExtract(t *testing.T) {
	// ZIP with one empty file.
	zipPath := writeZip(t, map[string]string{"empty.txt": ""})

	zipDst := t.TempDir()

	if err := Unpack(zipPath, zipDst, testLimits()); err != nil {
		t.Fatalf("zip empty entry extract: %v", err)
	}

	assertEmptyFile(t, filepath.Join(zipDst, "empty.txt"))

	// TAR with one empty file.
	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	if err := tw.WriteHeader(&tar.Header{
		Name: "empty.txt", Typeflag: tar.TypeReg, Size: 0, Mode: 0o600,
	}); err != nil {
		t.Fatal(err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tarPath := writeFile(t, buf.Bytes(), ".tar")

	tarDst := t.TempDir()

	if err := Unpack(tarPath, tarDst, testLimits()); err != nil {
		t.Fatalf("tar empty entry extract: %v", err)
	}

	assertEmptyFile(t, filepath.Join(tarDst, "empty.txt"))

	// TAR root-directory entries ("./") from "tar czf x ." trees.
	var buf2 bytes.Buffer

	tw2 := tar.NewWriter(&buf2)

	for _, hdr := range []*tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./file.txt", Typeflag: tar.TypeReg, Size: 5, Mode: 0o600},
	} {
		if err := tw2.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}

		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw2.Write([]byte("bytes")); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := tw2.Close(); err != nil {
		t.Fatal(err)
	}

	rootPath := writeFile(t, buf2.Bytes(), ".tar")

	rootDst := t.TempDir()

	if err := Unpack(rootPath, rootDst, testLimits()); err != nil {
		t.Fatalf("tar ./ root entries extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(rootDst, "file.txt"))
	if err != nil || string(got) != "bytes" {
		t.Fatalf("tar ./file.txt = %q, %v", got, err)
	}
}

// TestMatrixZipLyingHeaderRejected proves the fail-closed contract
// for a zip entry whose stream carries MORE data than its header
// declares: rejected, not silently truncated.
func TestMatrixZipLyingHeaderRejected(t *testing.T) {
	buildLyingZip := func(uncompressed, compressed uint64) string {
		payload := []byte(strings.Repeat("x", int(compressed)))

		var buf bytes.Buffer

		zw := zip.NewWriter(&buf)

		header := &zip.FileHeader{
			Name:               "liar.bin",
			Method:             zip.Store,
			UncompressedSize64: uncompressed,
			CompressedSize64:   compressed,
		}

		w, err := zw.CreateRaw(header)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}

		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}

		return writeFile(t, buf.Bytes(), ".zip")
	}

	// Declares 5 bytes, carries 10: CopyN stops at 5; the probe must
	// catch the smuggled remainder.
	path := buildLyingZip(5, 10)

	if err := Unpack(path, t.TempDir(), testLimits()); !errors.Is(err, ErrLimit) {
		t.Fatalf("lying zip header unpacked with err=%v, want ErrLimit", err)
	}

	// Declares 0 bytes, carries 3: the empty-entry path must probe
	// for smuggled data too.
	path = buildLyingZip(0, 3)

	if err := Unpack(path, t.TempDir(), testLimits()); !errors.Is(err, ErrLimit) {
		t.Fatalf("lying empty zip entry unpacked with err=%v, want ErrLimit", err)
	}

	// Honest entry (declared == carried) still extracts.
	path = buildLyingZip(10, 10)

	dst := t.TempDir()

	if err := Unpack(path, dst, testLimits()); err != nil {
		t.Fatalf("honest raw zip entry extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "liar.bin"))
	if err != nil || len(got) != 10 {
		t.Fatalf("honest entry = %d bytes, %v; want 10", len(got), err)
	}
}

// TestMatrixTruncatedZipFailsClosed adds the zip flavour of the
// malformed-archive contract: a cut central directory must fail the
// whole extraction (the tar flavour is pinned by
// TestUnpackRejectsTruncatedTarEntry).
func TestMatrixTruncatedZipFailsClosed(t *testing.T) {
	path := writeZip(t, map[string]string{"file.txt": strings.Repeat("a", 4096)})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(raw) < 64 {
		t.Fatal("archive unexpectedly small")
	}

	// Cut the tail: the central directory (and part of the data) is
	// gone. However the reader obtains the entries, the extraction
	// must never report success.
	path = writeFile(t, raw[:len(raw)-48], ".zip")

	if err := Unpack(path, t.TempDir(), testLimits()); err == nil {
		t.Fatal("truncated zip unpacked without error — not fail-closed")
	}
}

// TestMatrixLongComponentsRejected pins the consistent component
// bound (a component the OS would refuse is rejected identically on
// every platform instead of failing late and differently).
func TestMatrixLongComponentsRejected(t *testing.T) {
	long := strings.Repeat("l", 256)

	path := writeZip(t, map[string]string{long: "x"})

	if err := Unpack(path, t.TempDir(), testLimits()); !errors.Is(err, ErrLimit) {
		t.Fatalf("256-byte component unpacked with err=%v, want ErrLimit", err)
	}
}

// assertEmptyFile asserts path exists as an empty regular file.
func assertEmptyFile(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("empty entry missing: %v", err)
	}

	if info.Size() != 0 {
		t.Fatalf("empty entry has size %d, want 0", info.Size())
	}
}
