package shadowsocks

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

// Wire constants of the AEAD TCP framing. All supported methods share the
// same tag size, nonce size, length-field size and payload limit; only
// key and salt sizes differ per method.
const (
	subkeyInfo      = "ss-subkey" // HKDF info string mandated by the spec
	nonceSize       = 12          // AEAD nonce; little-endian counter
	lengthFieldSize = 2           // plaintext size of the chunk length field
	tagSize         = 16          // AEAD tag size for all supported methods
	maxPayloadSize  = 0x3FFF      // maximum chunk payload per the spec
)

// errChunkAuth is the sentinel for AEAD Open failures. Every path that
// hits it fails the connection closed: the error is wrapped into the
// caller's error chain (errors.Is works) and the connection is torn down,
// never silently EOF'd.
var errChunkAuth = errors.New("shadowsocks: authentication failed")

// newAEAD instantiates the cipher.AEAD for a session subkey. AES-GCM
// comes from the standard library; ChaCha20-IETF-Poly1305 (no AEAD
// primitive in the standard library) from golang.org/x/crypto, which the
// module already requires transitively. All three produce 12-byte nonces
// and 16-byte tags as the spec requires.
func newAEAD(method Method, key []byte) (cipher.AEAD, error) {
	switch method {
	case MethodAES128GCM, MethodAES256GCM:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("shadowsocks: %s: AES cipher: %w", method, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("shadowsocks: %s: GCM: %w", method, err)
		}
		return aead, nil
	case MethodChaCha20IETFPoly1305:
		aead, err := chacha20poly1305.New(key)
		if err != nil {
			return nil, fmt.Errorf("shadowsocks: %s: ChaCha20-Poly1305: %w", method, err)
		}
		return aead, nil
	default:
		return nil, fmt.Errorf("shadowsocks: unsupported method %q", method)
	}
}

// newSalt produces a fresh cryptographically random salt. A random salt
// per connection is what makes the subkey (and thus the nonce stream)
// unique; salt reuse across connections with the same master key would
// enable nonce reuse and break the AEAD completely.
func newSalt(size int) ([]byte, error) {
	salt := make([]byte, size)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("shadowsocks: generate salt: %w", err)
	}
	return salt, nil
}

// nextNonce advances the AEAD nonce in place. The spec defines the nonce
// as a 12-byte little-endian counter starting at zero and incremented
// once per AEAD operation, so nonce[0] is the least-significant byte and
// the counter wraps around on full overflow.
func nextNonce(nonce []byte) {
	for i := range nonce {
		nonce[i]++
		if nonce[i] != 0 {
			return
		}
	}
}

// writeAll writes the entire buffer. net.Conn.Write already returns an
// error on short writes, but the framing must hold for any io.Writer
// (tests, buffers), so short writes are turned into real errors here.
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// chunkReader decrypts the AEAD chunk framing layered on a raw stream
// (after the salt). Read bounds: the length piece is a fixed 18-byte
// buffer and the payload piece fits in a single pre-allocated
// (0x3FFF+16)-byte buffer — no unbounded buffering is possible regardless
// of what the peer sends.
//
// End-of-stream: a decrypted length of 0 is the spec's end marker and
// yields io.EOF (no payload piece follows a zero-length chunk). A raw EOF
// at a chunk boundary is also reported as io.EOF because mainstream
// implementations — including go-shadowsocks2 — end streams by closing
// the TCP connection without an explicit zero-length chunk; an EOF in the
// middle of a chunk (truncation) is an error, never a clean end.
type chunkReader struct {
	src     io.Reader
	aead    cipher.AEAD
	nonce   [nonceSize]byte
	lenBuf  [lengthFieldSize + tagSize]byte
	buf     []byte // full-size wire buffer for the current chunk (allocated once)
	payload []byte // decrypted bytes of the current chunk not yet consumed

	// mu serializes the mutable framing state above. net.Conn permits
	// concurrent Read calls; without the lock two concurrent Reads
	// would corrupt the nonce counter and interleave chunk assembly.
	// The write direction has its own lock (chunkWriter.mu): Read and
	// Write stay parallel by design.
	mu sync.Mutex
}

func newChunkReader(src io.Reader, aead cipher.AEAD) *chunkReader {
	return &chunkReader{
		src:     src,
		aead:    aead,
		buf:     make([]byte, maxPayloadSize+tagSize),
		payload: make([]byte, 0, maxPayloadSize+tagSize),
	}
}

// Read returns decrypted plaintext. It never returns (n>0, err).
// Safe for concurrent calls: the framing state is mutated under r.mu.
func (r *chunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		if len(r.payload) > 0 {
			n := copy(p, r.payload)
			r.payload = r.payload[n:]
			return n, nil
		}
		if err := r.pullChunk(); err != nil {
			return 0, err
		}
	}
}

// pullChunk reads, authenticates and decrypts the next chunk into the
// pending buffer. Every failure path fails closed and poisons this
// reader: authentication failures wrap errChunkAuth, oversized lengths
// and truncation are hard errors, and none of them are recoverable.
func (r *chunkReader) pullChunk() error {
	if _, err := io.ReadFull(r.src, r.lenBuf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF // boundary EOF = clean end of stream (see type doc)
		}
		return fmt.Errorf("shadowsocks: read chunk header: %w", err)
	}

	// Open reuses lenBuf as destination (plaintext overwrites ciphertext).
	plain, err := r.aead.Open(r.lenBuf[:0], r.nonce[:], r.lenBuf[:], nil)
	nextNonce(r.nonce[:]) // the operation consumed the nonce regardless of outcome
	if err != nil {
		return fmt.Errorf("shadowsocks: chunk header: %w: %v", errChunkAuth, err)
	}

	length := int(binary.BigEndian.Uint16(plain))
	if length == 0 {
		return io.EOF // spec end-of-stream marker
	}
	if length > maxPayloadSize {
		return fmt.Errorf("shadowsocks: chunk payload length %d exceeds limit %d", length, maxPayloadSize)
	}

	// The wire buffer is the fixed full-size allocation, NEVER the
	// (resliced, shrinking) payload window: Read() advances payload, and
	// a slice expression over it would run past the shrunken capacity.
	buf := r.buf[:length+tagSize]
	if _, err := io.ReadFull(r.src, buf); err != nil {
		return fmt.Errorf("shadowsocks: read chunk payload: %w", err)
	}
	plain, err = r.aead.Open(buf[:0], r.nonce[:], buf, nil)
	nextNonce(r.nonce[:])
	if err != nil {
		return fmt.Errorf("shadowsocks: chunk payload: %w: %v", errChunkAuth, err)
	}

	r.payload = plain
	return nil
}

// chunkWriter applies the AEAD chunk framing on top of a raw stream
// (after the salt). Each chunk — length piece and payload piece — is
// handed to the underlying writer in a single Write, so chunks are never
// interleaved and stay whole on the wire.
type chunkWriter struct {
	dst   io.Writer
	aead  cipher.AEAD
	nonce [nonceSize]byte
	buf   []byte // scratch: length piece (2+tag) + payload piece (max+tag)

	// mu serializes chunk assembly: net.Conn permits concurrent Write
	// calls, and the nonce counter plus the shared scratch buffer are
	// only correct when one chunk is sealed and handed to the wire at
	// a time. The read direction (chunkReader.mu) stays independent.
	mu sync.Mutex
}

func newChunkWriter(dst io.Writer, aead cipher.AEAD) *chunkWriter {
	return &chunkWriter{
		dst:  dst,
		aead: aead,
		buf:  make([]byte, lengthFieldSize+tagSize+maxPayloadSize+tagSize),
	}
}

// Write encrypts p as one or more chunks. An empty p is a no-op: the
// spec's zero-length end-of-stream chunk is deliberately NOT emitted
// implicitly (and CloseWrite is NOT wired to one — half-close through
// the tunnel is unsupported, see tunnelConn), because the reference
// implementation (go-shadowsocks2) cannot parse one — it would try to
// read a 16-byte payload tag that does not exist. End-of-stream is
// signaled by closing the connection; the reader still honors an
// explicit zero-length chunk per the spec.
// Safe for concurrent calls: chunks are assembled under w.mu.
func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	written := 0
	for len(p) > 0 {
		size := len(p)
		if size > maxPayloadSize {
			size = maxPayloadSize
		}
		if err := w.writeChunk(p[:size]); err != nil {
			return written, err
		}
		p = p[size:]
		written += size
	}
	return written, nil
}

// writeChunk seals and writes one chunk: the 2-byte big-endian payload
// length encrypted with the current nonce, then the payload encrypted
// with the next nonce — one AEAD operation per piece, per the spec.
func (w *chunkWriter) writeChunk(payload []byte) error {
	binary.BigEndian.PutUint16(w.buf[:lengthFieldSize], uint16(len(payload)))
	// Seal in place over the length field (documented aliasing mode).
	w.aead.Seal(w.buf[:0], w.nonce[:], w.buf[:lengthFieldSize], nil)
	nextNonce(w.nonce[:])

	frameLen := lengthFieldSize + tagSize + len(payload) + tagSize
	w.aead.Seal(w.buf[:lengthFieldSize+tagSize], w.nonce[:], payload, nil)
	nextNonce(w.nonce[:])

	if err := writeAll(w.dst, w.buf[:frameLen]); err != nil {
		return fmt.Errorf("shadowsocks: write chunk: %w", err)
	}
	return nil
}
