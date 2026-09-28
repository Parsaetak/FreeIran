// safearchive_mihomo_gz_test.go is the v0.11.2 regression gate for the
// single-file .gz extraction path added to support MetaCubeX Mihomo
// Linux releases (mihomo-linux-amd64-vX.Y.Z.gz — a gzipped executable,
// not a tar.gz). The test verifies:
//
//   - a bare .gz of one executable extracts to a single file with the
//     .gz suffix stripped and the executable bit set
//   - a bare .gz of a tar.gz stream still extracts the tar entries
//     (the tar-peek fallback path)
//   - a hostile single-file gzip exceeding MaxTotalBytes is rejected
//     (the single-file path inherits the same bounds as the tar path)
package safearchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// TestUnpackSingleGzExtractsExecutable covers the happy path: a .gz
// payload that is a single binary, not a tar stream. The output file
// keeps the archive's base name with .gz stripped and is executable.
func TestUnpackSingleGzExtractsExecutable(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "mihomo-linux-amd64-v1.18.0.gz")

	payload := []byte("#!/bin/sh\n# fake mihomo binary payload\n\x7fELF\x02\x01\x01")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}

	gw := gzip.NewWriter(f)
	if _, err := gw.Write(payload); err != nil {
		_ = gw.Close()
		_ = f.Close()
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		_ = f.Close()
		t.Fatalf("gzip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}

	dst := filepath.Join(tmp, "out")
	if err := Unpack(archivePath, dst, DefaultLimits()); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	expected := filepath.Join(dst, "mihomo-linux-amd64-v1.18.0")
	info, err := os.Stat(expected)
	if err != nil {
		t.Fatalf("expected staged file %s: %v", expected, err)
	}

	if info.Size() != int64(len(payload)) {
		t.Errorf("staged file size = %d, want %d", info.Size(), len(payload))
	}

	// Executable bit must be set (mirrors the tar TypeReg + 0111 path).
	if info.Mode()&0o111 == 0 {
		t.Errorf("staged file mode = %v, want executable bit", info.Mode())
	}
}

// TestUnpackGzWithTarStreamInside covers the fallback: a .gz file whose
// payload is a real tar.gz stream. The tar-peek must recognize it and
// walk every tar entry through the existing tar path.
func TestUnpackGzWithTarStreamInside(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "fakecore-linux-amd64.gz")

	// Build a tar.gz payload (NOT a bare single-file gzip). The gzip
	// layer wraps the tar layer; the file extension is .gz so the
	// unpacker must reach the tar-peek fallback to recognize it.
	var tarGzBuf bytes.Buffer

	gzOuter := gzip.NewWriter(&tarGzBuf)
	tw := tar.NewWriter(gzOuter)

	body := []byte("payload-body")
	if err := tw.WriteHeader(&tar.Header{
		Name: "fakecore",
		Mode: 0o755,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gzOuter.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	if err := os.WriteFile(archivePath, tarGzBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	dst := filepath.Join(tmp, "out")
	if err := Unpack(archivePath, dst, DefaultLimits()); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	staged := filepath.Join(dst, "fakecore")
	if _, err := os.Stat(staged); err != nil {
		t.Errorf("expected staged tar entry %s: %v", staged, err)
	}
}

// TestUnpackSingleGzRejectsOversized verifies the single-file path
// inherits the same MaxTotalBytes ceiling as the tar path.
func TestUnpackSingleGzRejectsOversized(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "big.gz")

	payload := bytes.Repeat([]byte("A"), 4096)

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}

	gw := gzip.NewWriter(f)
	if _, err := gw.Write(payload); err != nil {
		_ = gw.Close()
		_ = f.Close()
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		_ = f.Close()
		t.Fatalf("gzip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}

	dst := filepath.Join(tmp, "out")
	limits := Limits{
		MaxArchiveBytes: 1 << 20,
		MaxTotalBytes:   1024, // smaller than payload
		MaxFileBytes:    1 << 20,
		MaxFiles:        4096,
	}

	if err := Unpack(archivePath, dst, limits); err == nil {
		t.Fatalf("unpack: expected error for oversized single-gzip, got nil")
	}
}
