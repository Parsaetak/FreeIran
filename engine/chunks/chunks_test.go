package chunks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	buf := make([]byte, HeaderSize)

	h := Header{
		Version:     FormatVersion,
		Flags:       FlagCompressed,
		RecordCount: 42,
		Checksum:    0xDEADBEEF,
	}

	if err := EncodeHeader(buf, h); err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := DecodeHeader(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got != h {
		t.Fatalf("round trip mismatch: %+v != %+v", got, h)
	}

	if _, err := DecodeHeader([]byte("short")); err == nil {
		t.Fatal("short header should fail")
	}

	bad := append([]byte(nil), buf...)
	bad[0] = 'X'

	if _, err := DecodeHeader(bad); !errors.Is(err, ErrBadHeader) {
		t.Fatalf("bad magic should be ErrBadHeader, got %v", err)
	}
}

func TestChunkerDeterministicGrouping(t *testing.T) {
	c := NewChunker(1024)

	sizes := make([]int, 0, 500)

	for i := 0; i < 500; i++ {
		sizes = append(sizes, 90+(i%7)*10)
	}

	first := c.Group(sizes)
	second := NewChunker(1024).Group(sizes)

	if len(first) == 0 {
		t.Fatal("expected non-empty grouping")
	}

	if len(first) != len(second) {
		t.Fatalf("grouping not deterministic: %d vs %d batches",
			len(first), len(second))
	}

	for i := range first {
		if fmt.Sprint(first[i]) != fmt.Sprint(second[i]) {
			t.Fatalf("batch %d differs", i)
		}
	}

	// Every record index must appear exactly once, in order.
	cursor := 0

	for _, batch := range first {
		for _, idx := range batch {
			if idx != cursor {
				t.Fatalf("index %d out of order, want %d", idx, cursor)
			}

			cursor++
		}
	}

	if cursor != len(sizes) {
		t.Fatalf("records lost: %d of %d", cursor, len(sizes))
	}

	// A batch never exceeds the target unless a single record does.
	for batchIdx, batch := range first {
		total := 0

		for _, idx := range batch {
			total += sizes[idx]
		}

		if len(batch) > 1 && total > c.TargetBytes() {
			t.Fatalf("batch %d exceeds target: %d", batchIdx, total)
		}
	}
}

func TestChunkerEmpty(t *testing.T) {
	c := NewChunker(1024)

	if got := c.Group(nil); got != nil {
		t.Fatal("nil sizes must produce nil grouping")
	}
}

func TestWriteAndReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000001.firc")

	records := [][]byte{
		[]byte("vless://one"),
		[]byte("vmess://two-with-longer-payload"),
		[]byte(""), // empty records are valid
		[]byte("ss://three"),
	}

	meta, err := WriteChunk(path, records, 0)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	if meta.Records != len(records) {
		t.Fatalf("meta.Records = %d", meta.Records)
	}

	if meta.ID != "000001" {
		t.Fatalf("meta.ID = %q", meta.ID)
	}

	if meta.Bytes <= HeaderSize {
		t.Fatal("chunk byte size must include payload")
	}

	loaded, err := ReadAll(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(loaded) != len(records) {
		t.Fatalf("loaded %d records, want %d", len(loaded), len(records))
	}

	for i := range records {
		if !bytes.Equal(loaded[i], records[i]) {
			t.Fatalf("record %d mismatch", i)
		}
	}

	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	if err := r.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestWriteChunkAtomicNoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000002.firc")

	if _, err := WriteChunk(path, [][]byte{[]byte("x")}, 0); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if len(e.Name()) > 1 && e.Name()[0] == '.' {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestCorruptionDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000003.firc")

	records := [][]byte{
		[]byte("payload-one"),
		[]byte("payload-two"),
	}

	if _, err := WriteChunk(path, records, 0); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt one payload body byte (after header + length prefix).
	raw[HeaderSize+2+4] ^= 0xFF

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.Verify(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify should report ErrCorrupt, got %v", err)
	}
}

func TestTruncatedChunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000004.firc")

	if _, err := WriteChunk(path, [][]byte{[]byte("full payload")}, 0); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, raw[:len(raw)-3], 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = ReadAll(path)

	if err == nil || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated chunk should be ErrCorrupt, got %v", err)
	}
}

func TestEmptyRecordsRejected(t *testing.T) {
	dir := t.TempDir()

	if _, err := WriteChunk(filepath.Join(dir, "a.firc"), nil, 0); !errors.Is(err, ErrNoRecords) {
		t.Fatalf("empty write should be ErrNoRecords, got %v", err)
	}
}

func TestOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000005.firc")

	huge := make([]byte, MaxRecordBytes+1)

	if _, err := WriteChunk(path, [][]byte{huge}, 0); err != nil {
		t.Fatalf("oversized record write should succeed as its own chunk, got %v", err)
	}

	// Reading must reject it.
	_, err := ReadAll(path)

	if err == nil || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized read should be ErrTooLarge, got %v", err)
	}
}

func TestMissingChunk(t *testing.T) {
	_, err := ReadAll(filepath.Join(t.TempDir(), "missing.firc"))

	if err == nil {
		t.Fatal("missing chunk must error")
	}
}

func TestLazyIteration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000006.firc")

	records := [][]byte{
		[]byte("r1"), []byte("r2"), []byte("r3"),
	}

	if _, err := WriteChunk(path, records, 0); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	count := 0

	for {
		_, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("next: %v", err)
		}

		count++
	}

	if count != 3 {
		t.Fatalf("iterated %d records, want 3", count)
	}

	// EOF is stable.
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatal("second EOF expected")
	}
}

func BenchmarkGrouping(b *testing.B) {
	sizes := make([]int, 100_000)

	for i := range sizes {
		sizes[i] = 400 + i%250
	}

	c := NewChunker(DefaultTargetBytes)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = c.Group(sizes)
	}
}

func BenchmarkChunkWriteRead(b *testing.B) {
	dir := b.TempDir()

	records := make([][]byte, 2000)

	for i := range records {
		records[i] = []byte(fmt.Sprintf(
			`{"id":"fp-%06d","type":"vless","address":"srv%d.example.com","port":443,"uuid":"u"}`,
			i, i))
	}

	path := filepath.Join(dir, "bench.firc")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := WriteChunk(path, records, 0); err != nil {
			b.Fatal(err)
		}

		if _, err := ReadAll(path); err != nil {
			b.Fatal(err)
		}
	}
}
