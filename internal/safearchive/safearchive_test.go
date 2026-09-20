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

// testLimits keeps every test fast while exercising the same code
// paths as the production budgets.
func testLimits() Limits {
	return Limits{
		MaxArchiveBytes: 1 << 20,
		MaxTotalBytes:   64 << 10,
		MaxFileBytes:    32 << 10,
		MaxFiles:        16,
	}
}

func writeZip(t *testing.T, entries map[string]string) string {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return writeFile(t, buf.Bytes(), ".zip")
}

func writeFile(t *testing.T, data []byte, ext string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "archive"+ext)

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func wantErrLimit(t *testing.T, err error, wantSub string) {
	t.Helper()

	if err == nil {
		t.Fatalf("want a safety-limit error mentioning %q, got nil", wantSub)
	}

	if !errors.Is(err, ErrLimit) {
		t.Fatalf("error %v is not ErrLimit", err)
	}

	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("error %q does not mention %q", err.Error(), wantSub)
	}
}

// ---- happy paths ---------------------------------------------------------

func TestUnpackZipExtractsEntries(t *testing.T) {
	path := writeZip(t, map[string]string{
		"v2ray":             "binary-bytes",
		"README.md":         "docs",
		"sub/dir/asset.dat": "nested",
	})

	dst := t.TempDir()

	if err := Unpack(path, dst, testLimits()); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	for rel, want := range map[string]string{
		"v2ray":             "binary-bytes",
		"README.md":         "docs",
		"sub/dir/asset.dat": "nested",
	} {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("entry %s missing: %v", rel, err)
		}

		if string(got) != want {
			t.Fatalf("entry %s = %q, want %q", rel, got, want)
		}
	}
}

func TestUnpackTarGzExtractsEntries(t *testing.T) {
	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	files := map[string]string{
		"tor":          "tor-binary",
		"data/geo.txt": "geo",
	}

	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	path := writeFile(t, buf.Bytes(), ".tar.gz")
	dst := t.TempDir()

	if err := Unpack(path, dst, testLimits()); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "tor"))
	if err != nil || string(got) != "tor-binary" {
		t.Fatalf("tor entry = %q, %v", got, err)
	}
}

func TestUnpackSniffsMisnamedFormats(t *testing.T) {
	// A ZIP named ".asset" (no extension) still unpacks by magic.
	path := writeZip(t, map[string]string{"core": "x"})
	path = rename(t, path, ".asset")

	dst := t.TempDir()

	if err := Unpack(path, dst, testLimits()); err != nil {
		t.Fatalf("sniffed zip Unpack: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "core")); err != nil {
		t.Fatalf("sniffed zip entry missing: %v", err)
	}
}

func rename(t *testing.T, path, ext string) string {
	t.Helper()

	next := strings.TrimSuffix(path, filepath.Ext(path)) + ext

	if err := os.Rename(path, next); err != nil {
		t.Fatal(err)
	}

	return next
}

// ---- zip-bomb / size limits ----------------------------------------------

// TestUnpackRejectsZipBomb pins the expansion-bomb defence: a tiny
// archive of zeroes expands past the 64 KiB total budget and must be
// rejected at the DECLARED-size pre-check, before it touches the disk.
func TestUnpackRejectsZipBomb(t *testing.T) {
	// 6 entries × 20 KiB of zeroes = 120 KiB total (each entry is
	// BELOW the 32 KiB per-file cap; only the TOTAL budget rejects).
	// Zip compresses zeroes ~1000:1 so the archive stays tiny.
	entries := map[string]string{}

	for i := 0; i < 6; i++ {
		entries[string(rune('a'+i))+".bin"] = strings.Repeat("\x00", 20<<10)
	}

	path := writeZip(t, entries)

	dst := t.TempDir()

	wantErrLimit(t, Unpack(path, dst, testLimits()), "total")
}

// TestUnpackRejectsOversizedSingleFile pins the per-file cap.
func TestUnpackRejectsOversizedSingleFile(t *testing.T) {
	path := writeZip(t, map[string]string{
		"big.bin": strings.Repeat("x", (32<<10)+1),
	})

	dst := t.TempDir()

	wantErrLimit(t, Unpack(path, dst, testLimits()), "big.bin")
}

// TestUnpackRejectsOversizedArchive pins the staged-file size cap: it
// runs before any entry is read.
func TestUnpackRejectsOversizedArchive(t *testing.T) {
	path := writeFile(t, []byte(strings.Repeat("x", (1<<20)+1)), ".zip")

	wantErrLimit(t, Unpack(path, t.TempDir(), testLimits()), "archive is")
}

// TestUnpackRejectsTooManyEntries pins the file-count cap (zip
// declares the count up front; tar counts while streaming).
func TestUnpackRejectsTooManyEntries(t *testing.T) {
	entries := map[string]string{}

	for i := 0; i < 20; i++ {
		entries[string(rune('a'+i))+string(rune('a'+i))+string(rune('a'+i))+string(rune('a'+i))+"/"+string(rune('A'+i))+".txt"] = "x"
	}

	path := writeZip(t, entries)

	wantErrLimit(t, Unpack(path, t.TempDir(), testLimits()), "entries")
}

// TestUnpackTarBombRuntimeBudget pins the RUNTIME total budget for
// tar streams (tar has no central directory: the budget is enforced
// while streaming, not from declared sizes alone).
func TestUnpackTarBombRuntimeBudget(t *testing.T) {
	// Two entries whose declared sizes fit individually and total, but
	// the stream delivers exactly the declared bytes — build an honest
	// tar.gz that EXCEEDS the total budget at entry two.
	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	payload := bytes.Repeat([]byte("z"), 30<<10) // 30 KiB per entry

	for i := 0; i < 3; i++ {
		name := string(rune('a'+i)) + ".bin"

		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o600,
			Size: int64(len(payload)),
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	path := writeFile(t, buf.Bytes(), ".tar.gz")

	// 3 × 30 KiB = 90 KiB exceeds the 64 KiB total budget at entry
	// three while every individual entry fits the per-file cap.
	wantErrLimit(t, Unpack(path, t.TempDir(), testLimits()), "total")
}

// ---- traversal / absolute / symlink --------------------------------------

func TestUnpackRejectsZipSlip(t *testing.T) {
	for _, name := range []string{
		"../evil.txt",
		"a/../../evil.txt",
		"/etc/passwd-clone",
		`C:\Windows\evil.txt`,
		`\\server\share\evil.txt`,
		"",
	} {
		path := writeZip(t, map[string]string{name: "evil"})

		err := Unpack(path, t.TempDir(), testLimits())

		if err == nil {
			t.Fatalf("traversal entry %q unpacked without error", name)
		}

		if !errors.Is(err, ErrLimit) {
			t.Fatalf("traversal entry %q produced %v, want ErrLimit", name, err)
		}
	}
}

func TestUnpackRejectsTarSlipAndSpecials(t *testing.T) {
	buildTar := func(headers []*tar.Header, bodies [][]byte) string {
		var buf bytes.Buffer

		tw := tar.NewWriter(&buf)

		for i, hdr := range headers {
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}

			if bodies[i] != nil {
				if _, err := tw.Write(bodies[i]); err != nil {
					t.Fatal(err)
				}
			}
		}

		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}

		return writeFile(t, buf.Bytes(), ".tar")
	}

	cases := []struct {
		name string
		hdrs []*tar.Header
	}{
		{"symlink", []*tar.Header{
			{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777},
		}},
		{"hardlink", []*tar.Header{
			{Name: "real", Typeflag: tar.TypeReg, Size: 3},
			{Name: "hard", Typeflag: tar.TypeLink, Linkname: "real"},
		}},
		{"device", []*tar.Header{
			{Name: "dev", Typeflag: tar.TypeBlock, Devmajor: 1, Devminor: 3},
		}},
		{"fifo", []*tar.Header{
			{Name: "pipe", Typeflag: tar.TypeFifo},
		}},
		{"traversal", []*tar.Header{
			{Name: "../evil", Typeflag: tar.TypeReg, Size: 5, Mode: 0o600},
		}},
		{"absolute", []*tar.Header{
			{Name: "/evil", Typeflag: tar.TypeReg, Size: 5, Mode: 0o600},
		}},
		{"unknown-flag", []*tar.Header{
			{Name: "weird", Typeflag: 'Z', Size: 0, Mode: 0o600},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodies := make([][]byte, len(tc.hdrs))

			for i, h := range tc.hdrs {
				if h.Typeflag == tar.TypeReg {
					bodies[i] = bytes.Repeat([]byte("x"), int(h.Size))
				}
			}

			path := buildTar(tc.hdrs, bodies)

			wantErrLimit(t, Unpack(path, t.TempDir(), testLimits()), "")
		})
	}
}

// ---- fail-closed malformed ------------------------------------------------

// TestUnpackRejectsTruncatedTarEntry pins the fail-closed contract on
// malformed archives: a stream that ends in the middle of an entry
// produces an error — never a silent partial success.
func TestUnpackRejectsTruncatedTarEntry(t *testing.T) {
	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	if err := tw.WriteHeader(&tar.Header{Name: "liar.bin", Mode: 0o600, Size: 1024}); err != nil {
		t.Fatal(err)
	}

	// Deliberately short body; keep the CUT stream before tar's
	// Close() pads it — the reader sees a header promising 1024
	// bytes and a stream that ends early.
	if _, err := tw.Write(bytes.Repeat([]byte("y"), 128)); err != nil {
		t.Fatal(err)
	}

	raw := append([]byte(nil), buf.Bytes()...)

	_ = tw.Close() // discard the padded closing — keep the cut copy

	cut := bytes.LastIndex(raw, []byte("y")) + 1
	raw = raw[:cut]

	path := writeFile(t, raw, ".tar")

	dst := t.TempDir()

	if err := Unpack(path, dst, testLimits()); err == nil {
		t.Fatal("truncated tar unpacked without error — not fail-closed")
	}

	// The partial entry must not have been reported as complete.
	if info, statErr := os.Stat(filepath.Join(dst, "liar.bin")); statErr == nil && info.Size() == 1024 {
		t.Fatal("truncated entry materialized at its full declared size")
	}
}
