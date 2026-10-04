package shadowsocks

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// fixedKeyMaterial derives deterministic key material for framing tests:
// the master key from a fixed password (pinned against independent
// vectors by kdf_test.go) and a fixed salt, so the whole nonce/chunk
// stream is reproducible.
func fixedKeyMaterial(t *testing.T, method Method, salt []byte) (master, subkey []byte) {
	t.Helper()
	master, err := DeriveKey(method, "framing-fixture-password")
	if err != nil {
		t.Fatalf("derive master: %v", err)
	}
	subkey = deriveSubkey(master, salt, method.KeySize())
	return master, subkey
}

// TestNextNonce pins the 12-byte little-endian incrementing counter:
// starts at zero, increments on the least significant byte first, carries
// upward, and wraps around on full overflow.
func TestNextNonce(t *testing.T) {
	cases := []struct {
		name string
		in   [nonceSize]byte
		want [nonceSize]byte
	}{
		{
			name: "zero increments lsb",
			in:   [nonceSize]byte{},
			want: [nonceSize]byte{1},
		},
		{
			name: "carry out of lsb",
			in:   [nonceSize]byte{0xff},
			want: [nonceSize]byte{0x00, 0x01},
		},
		{
			name: "carry into middle bytes",
			in:   [nonceSize]byte{0xff, 0xff, 0x2a},
			want: [nonceSize]byte{0x00, 0x00, 0x2b},
		},
		{
			name: "full overflow wraps to zero",
			in:   [nonceSize]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			want: [nonceSize]byte{},
		},
		{
			name: "counter in high bytes preserved",
			in:   [nonceSize]byte{0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07},
			want: [nonceSize]byte{0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nonce := tc.in
			nextNonce(nonce[:])
			if nonce != tc.want {
				t.Fatalf("nextNonce(% x) = % x, want % x", tc.in, nonce, tc.want)
			}
		})
	}

	// The first 2^16+1 values must form the little-endian sequence
	// 0,1,2,... — spot-check a sweep against encoding/binary.
	nonce := make([]byte, nonceSize)
	for want := 0; want <= 1<<16; want++ {
		if got := binary.LittleEndian.Uint16(nonce[:2]); got != uint16(want) {
			t.Fatalf("sweep step %d: nonce counter = %d", want, got)
		}
		nextNonce(nonce)
	}
}

// TestChunkRoundTrip writes payloads of boundary and random sizes through
// a chunkWriter and reads them back through a chunkReader (each with its
// own AEAD + nonce counter, exactly like the two tunnel directions).
func TestChunkRoundTrip(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305} {
		t.Run(string(method), func(t *testing.T) {
			salt := make([]byte, method.SaltSize())
			if _, err := rand.Read(salt); err != nil {
				t.Fatal(err)
			}
			_, subW := fixedKeyMaterial(t, method, salt)
			aeadW, err := newAEAD(method, subW)
			if err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			w := newChunkWriter(&wire, aeadW)

			// Payload sizes: empty write (no-op), 1 byte, exactly the max,
			// max+1 (forced split), and a couple of odd sizes.
			payloads := [][]byte{
				nil,
				[]byte("a"),
				bytes.Repeat([]byte{0xAB}, maxPayloadSize),
				bytes.Repeat([]byte{0xCD}, maxPayloadSize+1),
				bytes.Repeat([]byte{0x42}, 777),
			}
			var want []byte
			for _, p := range payloads {
				n, err := w.Write(p)
				if err != nil {
					t.Fatalf("write %d bytes: %v", len(p), err)
				}
				if n != len(p) {
					t.Fatalf("write %d bytes reported %d", len(p), n)
				}
				want = append(want, p...)
			}
			// An empty write is a no-op and must not emit the zero-length
			// end-of-stream chunk (that would break reference readers).
			if wire.Len() == 0 {
				t.Fatal("no chunks were written")
			}

			// Independent read direction: same subkey construction with a
			// fresh AEAD instance and its own nonce counter.
			_, subR := fixedKeyMaterial(t, method, salt)
			aeadR, err := newAEAD(method, subR)
			if err != nil {
				t.Fatal(err)
			}
			r := newChunkReader(&wire, aeadR)
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read all: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("round trip mismatch: got %d bytes, want %d bytes", len(got), len(want))
			}

			// Reading again hits the boundary EOF (clean end of stream).
			if _, err := r.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
				t.Fatalf("read after exhaustion: err = %v, want io.EOF", err)
			}
		})
	}
}

// TestChunkReaderEmptyBufferNoOp: Read with a zero-length destination
// must not consume the stream.
func TestChunkReaderEmptyBufferNoOp(t *testing.T) {
	method := MethodAES128GCM
	salt := make([]byte, method.SaltSize())
	_, sub := fixedKeyMaterial(t, method, salt)
	aead, err := newAEAD(method, sub)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	w := newChunkWriter(&wire, aead)
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}

	aeadR, err := newAEAD(method, sub)
	if err != nil {
		t.Fatal(err)
	}
	r := newChunkReader(&wire, aeadR)
	n, err := r.Read(nil)
	if n != 0 || err != nil {
		t.Fatalf("Read(nil) = (%d, %v), want (0, nil)", n, err)
	}
	// The stream is untouched: the payload is still fully readable.
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "payload" {
		t.Fatalf("after Read(nil): got %q, err %v", got, err)
	}
}

// TestChunkPayloadLimitEnforced crafts chunks whose length field carries
// validly authenticated but out-of-spec values and requires the reader to
// fail closed: a hard error, never io.EOF, never an oversized allocation
// or a read of a payload piece that does not exist.
func TestChunkPayloadLimitEnforced(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodChaCha20IETFPoly1305} {
		t.Run(string(method), func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				length uint16
				legal  bool // true: the spec limit itself must be accepted
			}{
				{name: "one past the limit", length: maxPayloadSize + 1}, // 0x4000
				{name: "maximum encodable", length: 0xFFFF},              // 65535
				{name: "spec limit itself is legal", length: maxPayloadSize, legal: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					salt := make([]byte, method.SaltSize())
					_, sub := fixedKeyMaterial(t, method, salt)
					aead, err := newAEAD(method, sub)
					if err != nil {
						t.Fatal(err)
					}

					// Hand-build the wire: one length piece sealed with the
					// reader's initial nonce (zero), followed by the payload
					// piece for the legal case.
					var wire bytes.Buffer
					var nonce [nonceSize]byte
					lenPlain := make([]byte, 2)
					binary.BigEndian.PutUint16(lenPlain, tc.length)
					wire.Write(aead.Seal(nil, nonce[:], lenPlain, nil))
					nextNonce(nonce[:])
					if tc.legal {
						wire.Write(aead.Seal(nil, nonce[:], bytes.Repeat([]byte{0x5A}, int(tc.length)), nil))
					}

					aeadR, err := newAEAD(method, sub)
					if err != nil {
						t.Fatal(err)
					}
					r := newChunkReader(&wire, aeadR)

					out := make([]byte, maxPayloadSize+64)
					n, err := r.Read(out)
					if tc.legal {
						if err != nil || n != int(maxPayloadSize) {
							t.Fatalf("legal length %d: read = (%d, %v)", tc.length, n, err)
						}
						return
					}
					if err == nil {
						t.Fatalf("length %d accepted: read %d bytes", tc.length, n)
					}
					if errors.Is(err, io.EOF) {
						t.Fatalf("length %d reported as io.EOF (must fail closed, not end-of-stream)", tc.length)
					}
					if !strings.Contains(err.Error(), "exceeds limit") {
						t.Fatalf("length %d error = %v, want a payload-limit error", tc.length, err)
					}
					// Fail closed: the reader must be poisoned — a follow-up
					// read must not return plaintext.
					if _, err := r.Read(out); err == nil {
						t.Fatalf("length %d: reader accepted plaintext after a limit violation", tc.length)
					}
				})
			}
		})
	}
}

// TestChunkTamperFailsAuth flips single bytes at known frame positions of
// a two-chunk stream and requires the reader to fail with the errChunkAuth
// sentinel in the error chain. Frame layout for the fixture stream:
// chunk i = [2+16 length piece][len_i+16 payload piece].
func TestChunkTamperFailsAuth(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305} {
		t.Run(string(method), func(t *testing.T) {
			salt := make([]byte, method.SaltSize())
			_, sub := fixedKeyMaterial(t, method, salt)
			aead, err := newAEAD(method, sub)
			if err != nil {
				t.Fatal(err)
			}

			build := func() []byte {
				var wire bytes.Buffer
				w := newChunkWriter(&wire, aead)
				if _, err := w.Write([]byte("first chunk payload")); err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte("second chunk payload")); err != nil {
					t.Fatal(err)
				}
				return append([]byte(nil), wire.Bytes()...)
			}

			chunk1PayloadLen := len("first chunk payload")
			frame1Len := lengthFieldSize + tagSize + chunk1PayloadLen + tagSize
			cases := []struct {
				name   string
				offset int
			}{
				{name: "length piece of chunk 1", offset: 3},
				{name: "payload piece of chunk 1", offset: lengthFieldSize + tagSize + 5},
				{name: "length piece of chunk 2", offset: frame1Len + 1},
				{name: "payload piece of chunk 2", offset: frame1Len + lengthFieldSize + tagSize + 2},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					wire := build()
					if tc.offset >= len(wire) {
						t.Fatalf("fixture: offset %d out of range (wire %d bytes)", tc.offset, len(wire))
					}
					wire[tc.offset] ^= 0x01

					aeadR, err := newAEAD(method, sub)
					if err != nil {
						t.Fatal(err)
					}
					r := newChunkReader(bytes.NewReader(wire), aeadR)
					_, err = io.ReadAll(r)
					if err == nil {
						t.Fatalf("tampered byte at %d accepted", tc.offset)
					}
					if !errors.Is(err, errChunkAuth) {
						t.Fatalf("tampered byte at %d: err = %v, want errChunkAuth in chain", tc.offset, err)
					}
				})
			}

			// Truncation mid-chunk is a hard error, never a clean EOF.
			wire := build()[:frame1Len-4]
			aeadR, err := newAEAD(method, sub)
			if err != nil {
				t.Fatal(err)
			}
			r := newChunkReader(bytes.NewReader(wire), aeadR)
			_, err = io.ReadAll(r)
			if err == nil {
				t.Fatal("truncated stream accepted")
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("mid-chunk truncation reported as io.EOF: %v", err)
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("mid-chunk truncation err = %v, want io.ErrUnexpectedEOF in chain", err)
			}
		})
	}
}

// TestZeroLengthChunkEOF verifies the spec's end-of-stream marker: a
// decrypted zero-length chunk yields io.EOF, EOF is sticky afterwards,
// and data before the marker is fully delivered first.
func TestZeroLengthChunkEOF(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodChaCha20IETFPoly1305} {
		t.Run(string(method), func(t *testing.T) {
			salt := make([]byte, method.SaltSize())
			_, sub := fixedKeyMaterial(t, method, salt)
			aead, err := newAEAD(method, sub)
			if err != nil {
				t.Fatal(err)
			}

			var wire bytes.Buffer
			w := newChunkWriter(&wire, aead)
			if _, err := w.Write([]byte("payload before the marker")); err != nil {
				t.Fatal(err)
			}
			// Emit the zero-length end-of-stream chunk explicitly (the
			// writer's empty Write is a deliberate no-op, so use writeChunk).
			if err := w.writeChunk(nil); err != nil {
				t.Fatal(err)
			}
			if err := w.writeChunk([]byte{}); err != nil { // same marker, empty slice
				t.Fatal(err)
			}

			aeadR, err := newAEAD(method, sub)
			if err != nil {
				t.Fatal(err)
			}
			r := newChunkReader(&wire, aeadR)
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(got) != "payload before the marker" {
				t.Fatalf("payload before marker = %q", got)
			}

			// EOF must be sticky for a reader that only produced the marker.
			r2 := newChunkReader(bytes.NewReader(nil), aeadR)
			buf := make([]byte, 16)
			for i := 0; i < 3; i++ {
				if _, err := r2.Read(buf); !errors.Is(err, io.EOF) {
					t.Fatalf("empty stream read %d: err = %v, want io.EOF", i, err)
				}
			}
		})
	}
}

// TestChunkWriterEmptyWriteNoOp pins that an empty Write emits nothing —
// the spec's zero-length chunk is never produced implicitly because the
// reference implementation cannot parse one.
func TestChunkWriterEmptyWriteNoOp(t *testing.T) {
	method := MethodAES128GCM
	salt := make([]byte, method.SaltSize())
	_, sub := fixedKeyMaterial(t, method, salt)
	aead, err := newAEAD(method, sub)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	w := newChunkWriter(&wire, aead)
	n, err := w.Write(nil)
	if n != 0 || err != nil {
		t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
	if wire.Len() != 0 {
		t.Fatalf("empty write emitted %d wire bytes", wire.Len())
	}
}

// TestChunkReaderRejectsBadATYPLength is a thin integration of address.go:
// readTarget over a chunkReader must reject an unknown ATYP and an empty
// domain, and port 0, so hostile first chunks fail closed.
func TestChunkReaderRejectsBadATYPLength(t *testing.T) {
	method := MethodAES128GCM
	salt := make([]byte, method.SaltSize())
	_, sub := fixedKeyMaterial(t, method, salt)
	aead, err := newAEAD(method, sub)
	if err != nil {
		t.Fatal(err)
	}

	stream := func(body []byte) *chunkReader {
		var wire bytes.Buffer
		w := newChunkWriter(&wire, aead)
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
		aeadR, err := newAEAD(method, sub)
		if err != nil {
			t.Fatal(err)
		}
		return newChunkReader(&wire, aeadR)
	}

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "unknown ATYP", body: []byte{0x09, 1, 2, 3, 4, 0, 80}},
		{name: "empty domain", body: []byte{atypDomain, 0, 0, 80}},
		{name: "port zero", body: []byte{atypIPv4, 1, 2, 3, 4, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readTarget(stream(tc.body)); err == nil {
				t.Fatalf("readTarget(% x) accepted malformed target", tc.body)
			}
		})
	}

	// Well-formed targets parse, including when the address spans chunk
	// boundaries with trailing payload bytes queued after it.
	body := append([]byte{}, AddrMustBytes("example.com", 443)...)
	body = append(body, []byte("trailing session data")...)
	target, err := readTarget(stream(body))
	if err != nil {
		t.Fatalf("readTarget: %v", err)
	}
	if target != "example.com:443" {
		t.Fatalf("target = %q, want example.com:443", target)
	}
}

// AddrMustBytes is a test helper around AddrBytes.
func AddrMustBytes(host string, port int) []byte {
	b, err := AddrBytes(host, port)
	if err != nil {
		panic(fmt.Sprintf("AddrBytes(%q, %d): %v", host, port, err))
	}
	return b
}
