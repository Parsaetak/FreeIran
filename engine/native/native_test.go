package native

import (
	"bytes"
	"hash/crc32"
	"strings"
	"testing"
)

// Reference vectors shared with native/tests/test_native.cpp. Both
// implementations must satisfy them, which transitively guarantees the
// Go and C++ paths agree bit-for-bit.
func TestHash64ReferenceVectors(t *testing.T) {
	if got := Hash64(nil); got != 14695981039346656037 {
		t.Fatalf("empty hash = %d, want offset basis", got)
	}

	if got := Hash64([]byte("a")); got != 0xaf63dc4c8601ec8c {
		t.Fatalf("hash('a') = %#x, want 0xaf63dc4c8601ec8c", got)
	}

	if got := Hash64([]byte("foobar")); got != 0x85944171f73967e8 {
		t.Fatalf("hash('foobar') = %#x, want 0x85944171f73967e8", got)
	}
}

func TestHash64Batch(t *testing.T) {
	strs := []string{
		"vless://one.example:443",
		"",
		"trojan://three.example:8443",
	}

	var packed []byte

	offsets := make([]uint32, 0, len(strs)+1)

	offsets = append(offsets, 0)

	for _, s := range strs {
		packed = append(packed, s...)
		offsets = append(offsets, uint32(len(packed)))
	}

	hashes := Hash64Batch(packed, offsets)

	if len(hashes) != len(strs) {
		t.Fatalf("len(hashes) = %d, want %d", len(hashes), len(strs))
	}

	for i, s := range strs {
		want := Hash64([]byte(s))

		if hashes[i] != want {
			t.Fatalf("hashes[%d] = %d, want %d", i, hashes[i], want)
		}
	}

	if got := Hash64Batch(nil, []uint32{0}); got != nil {
		t.Fatal("empty batch should return nil")
	}
}

func TestCRC32ReferenceVector(t *testing.T) {
	// Canonical IEEE CRC-32 check value for "123456789".
	got := CRC32(0, []byte("123456789"))

	if got != 0xCBF43926 {
		t.Fatalf("CRC32 = %#x, want 0xCBF43926", got)
	}

	if CRC32(0, nil) != 0 {
		t.Fatal("CRC32 of empty must be 0")
	}

	// Incremental == one-shot.
	text := []byte("FreeIran chunk checksum payload")

	whole := CRC32(0, text)
	half := CRC32(0, text[:len(text)/2])
	tail := CRC32(half, text[len(text)/2:])

	if whole != tail {
		t.Fatalf("incremental %x != one-shot %x", tail, whole)
	}

	// Must agree with the standard library table implementation.
	table := crc32.MakeTable(crc32.IEEE)

	if whole != crc32.Checksum(text, table) {
		t.Fatal("bridge disagrees with hash/crc32")
	}
}

func TestScanURLs(t *testing.T) {
	lines := []string{
		"# comment line\n",
		"vless://uuid@host:443?security=tls\n",
		"  \n",
		"\tvmatching line with leading tab\n",
		"https://example.com/not-a-proxy-line\n",
		"vmess://eyJhZGQiOiJleGFtcGxlIn0=\n",
		"notaproxy://foo\n",
		"\r\n",
		"ss://YWVzLTI1Ni1nY206cGFzcw@host:8388\n",
		"trailing line without newline",
	}

	text := []byte(strings.Join(lines, ""))

	out := make([]uint32, 16)
	found, filled := ScanURLs(text, out)

	if found != 3 {
		t.Fatalf("found = %d, want 3", found)
	}

	// Line-start offsets computed from the literal pieces.
	off2 := len(lines[0])
	off6 := off2 + len(lines[1]) + len(lines[2]) + len(lines[3]) +
		len(lines[4])
	off9 := off6 + len(lines[5]) + len(lines[6]) + len(lines[7])

	wantOffsets := []uint32{
		uint32(off2),
		uint32(off6),
		uint32(off9),
	}

	if len(filled) != 3 {
		t.Fatalf("filled = %d entries, want 3", len(filled))
	}

	for i, want := range wantOffsets {
		if filled[i] != want {
			t.Fatalf("offset[%d] = %d, want %d", i, filled[i], want)
		}

		line := text[filled[i]:]

		if nl := bytes.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
		}

		trimmed := strings.TrimLeftFunc(
			string(line),
			func(r rune) bool {
				return r == ' ' || r == '\t'
			},
		)

		supported := false

		for _, s := range schemeList {
			if strings.HasPrefix(trimmed, s) {
				supported = true

				break
			}
		}

		if !supported {
			t.Fatalf("scanned line %d not supported: %q", i, trimmed)
		}
	}
}

func TestScanURLsGrowthProtocol(t *testing.T) {
	text := []byte("vless://a\nvless://b\nvless://c\n")

	// Zero-capacity buffer returns the full count.
	found, filled := ScanURLs(text, nil)

	if found != 3 || len(filled) != 0 {
		t.Fatalf("growth protocol: found=%d filled=%d", found, len(filled))
	}

	// One-slot buffer fills one offset.
	small := make([]uint32, 1)
	found, filled = ScanURLs(text, small)

	if found != 3 || len(filled) != 1 || filled[0] != 0 {
		t.Fatalf("small buffer: found=%d filled=%v", found, filled)
	}
}

func TestScanURLsEmpty(t *testing.T) {
	found, _ := ScanURLs(nil, nil)

	if found != 0 {
		t.Fatalf("empty scan found = %d, want 0", found)
	}
}

func TestForcedFallback(t *testing.T) {
	// The FREEIRAN_NATIVE=off environment path is validated by
	// TestMain-style subprocess checks; here we verify the dispatch
	// functions produce identical results regardless of the active
	// path, which is the property operators rely on.
	data := []byte("correctness must never depend on the accelerator")

	offsets := []uint32{0, uint32(len(data))}
	viaDispatch := Hash64Batch(data, offsets)[0]

	if viaDispatch != Hash64(data) {
		t.Fatal("dispatch and direct Hash64 disagree")
	}

	if CRC32(0, data) != crc32.Checksum(data, crc32.MakeTable(crc32.IEEE)) {
		t.Fatal("dispatch CRC32 disagrees with stdlib")
	}
}
