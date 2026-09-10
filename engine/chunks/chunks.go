// Package chunks implements the FreeIran chunking subsystem: the
// deterministic unit of persistence for the local data store.
//
// A chunk is a self-describing, checksummed binary file holding a
// sequence of length-prefixed records. Records are NEVER split across
// chunk boundaries; a chunk boundary always falls between records.
//
// On-disk format (.firc, little-endian):
//
//	offset  size  field
//	0       4     magic "FIRC"
//	4       2     format version (uint16)
//	6       2     flags (uint16; bit 0 = payload compressed)
//	8       4     record count (uint32)
//	12      4     header checksum placeholder (uint32, reserved 0)
//	16      4     payload CRC-32 IEEE (uint32)
//	20      ..    payload: repeated [uint32 length][length bytes]
//
// Determinism: feeding the same record sequence through the same
// configuration produces byte-identical chunk files, so chunk IDs are
// stable across restarts and machines.
package chunks

import (
	"bufio"
	"encoding/binary"
	stderrors "errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Parsaetak/FreeIran/engine/errors"
)

const (
	// Magic identifies a FreeIran chunk file.
	Magic = "FIRC"

	// FormatVersion is the current chunk file format version.
	FormatVersion uint16 = 1

	// HeaderSize is the fixed size of the chunk header in bytes.
	HeaderSize = 20

	// FlagCompressed marks the payload as compressed.
	FlagCompressed uint16 = 1 << 0

	// DefaultTargetBytes is the default chunk payload target.
	DefaultTargetBytes = 4 << 20 // 4 MiB

	// MinTargetBytes is the smallest allowed payload target.
	MinTargetBytes = 64 << 10 // 64 KiB

	// MaxTargetBytes is the largest allowed payload target.
	MaxTargetBytes = 64 << 20 // 64 MiB

	// MaxRecordBytes is the largest single record accepted.
	MaxRecordBytes = 8 << 20 // 8 MiB

	defaultDirMode  os.FileMode = 0o700
	defaultFileMode os.FileMode = 0o600
)

// Sentinel errors surfaced to callers.
var (
	ErrCorrupt      = stderrors.New("chunks: corrupt chunk")
	ErrTooLarge     = stderrors.New("chunks: record exceeds limit")
	ErrBadHeader    = stderrors.New("chunks: bad header")
	ErrNoRecords    = stderrors.New("chunks: no records written")
	ErrOpenBoundary = stderrors.New("chunks: record would split chunk")
)

// Subsystem is used for structured error classification.
const Subsystem = "chunks"

// Header is the decoded chunk header.
type Header struct {
	RecordCount uint32
	Checksum    uint32
	Flags       uint16
	Version     uint16
}

// Validate checks magic, version and structural sanity.
func (h Header) Validate() error {
	if h.Version != FormatVersion {
		return fmt.Errorf("%w: version %d", ErrBadHeader, h.Version)
	}

	if h.Flags&^FlagCompressed != 0 {
		return fmt.Errorf("%w: unknown flags %#x", ErrBadHeader, h.Flags)
	}

	return nil
}

// EncodeHeader serialises a header into dst (len(dst) >= HeaderSize).
func EncodeHeader(dst []byte, h Header) error {
	if len(dst) < HeaderSize {
		return stderrors.New("chunks: header buffer too small")
	}

	copy(dst[0:4], Magic)

	binary.LittleEndian.PutUint16(dst[4:6], h.Version)
	binary.LittleEndian.PutUint16(dst[6:8], h.Flags)
	binary.LittleEndian.PutUint32(dst[8:12], h.RecordCount)
	binary.LittleEndian.PutUint32(dst[12:16], 0)
	binary.LittleEndian.PutUint32(dst[16:20], h.Checksum)

	return nil
}

// DecodeHeader parses a chunk header.
func DecodeHeader(src []byte) (Header, error) {
	if len(src) < HeaderSize {
		return Header{}, fmt.Errorf("%w: truncated", ErrBadHeader)
	}

	if string(src[0:4]) != Magic {
		return Header{}, fmt.Errorf("%w: bad magic", ErrBadHeader)
	}

	return Header{
		Version:     binary.LittleEndian.Uint16(src[4:6]),
		Flags:       binary.LittleEndian.Uint16(src[6:8]),
		RecordCount: binary.LittleEndian.Uint32(src[8:12]),
		Checksum:    binary.LittleEndian.Uint32(src[16:20]),
	}, nil
}

// Chunk is the metadata of one persisted chunk.
type Chunk struct {
	ID        string `json:"id"`
	Sequence  int    `json:"sequence"`
	Records   int    `json:"records"`
	Bytes     int    `json:"bytes"`
	Checksum  uint32 `json:"checksum"`
	Flags     uint16 `json:"flags"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	Schema    int    `json:"schema"`
}

// Chunker groups an ordered record stream into deterministic chunks.
//
// The zero value is not usable; use NewChunker.
type Chunker struct {
	targetBytes int
	sequence    int
}

// NewChunker creates a chunker with the given payload target. Targets
// are clamped to [MinTargetBytes, MaxTargetBytes].
func NewChunker(targetBytes int) *Chunker {
	if targetBytes < MinTargetBytes {
		targetBytes = MinTargetBytes
	}

	if targetBytes > MaxTargetBytes {
		targetBytes = MaxTargetBytes
	}

	return &Chunker{targetBytes: targetBytes}
}

// TargetBytes returns the effective payload target.
func (c *Chunker) TargetBytes() int {
	return c.targetBytes
}

// Group splits records into consecutive batches. A batch grows until
// adding the NEXT record would exceed the byte target, so batches stay
// within the target whenever individual records do. Every record is
// preserved intact; the method never returns empty batches.
//
// This function is deterministic: identical inputs and targets produce
// identical batches.
func (c *Chunker) Group(sizes []int) [][]int {
	if len(sizes) == 0 {
		return nil
	}

	batches := make([][]int, 0)

	current := make([]int, 0, 64)
	currentBytes := 0

	for i, size := range sizes {
		if size < 0 {
			size = 0
		}

		if size > MaxRecordBytes {
			// Oversized records still get their own chunk so the
			// caller can handle or reject them explicitly.
			if len(current) > 0 {
				batches = append(batches, current)
				current = make([]int, 0, 64)
				currentBytes = 0
			}

			batches = append(batches, []int{i})
			currentBytes = 0

			continue
		}

		if len(current) > 0 && currentBytes+size > c.targetBytes {
			batches = append(batches, current)
			current = make([]int, 0, 64)
			currentBytes = 0
		}

		current = append(current, i)
		currentBytes += size
	}

	if len(current) > 0 {
		batches = append(batches, current)
	}

	return batches
}

// WriteChunk writes one chunk file atomically: the data is written to
// a temporary file in the same directory, flushed, and renamed over
// the destination. Records must already be serialised; lengths are
// written by this function.
func WriteChunk(
	path string,
	records [][]byte,
	flags uint16,
) (Chunk, error) {
	if len(records) == 0 {
		return Chunk{}, ErrNoRecords
	}

	if err := os.MkdirAll(dirOf(path), defaultDirMode); err != nil {
		return Chunk{}, errors.Wrap(err, errors.KindEnvironment,
			Subsystem, "write", "create chunk directory")
	}

	payloadSize := 0

	for _, rec := range records {
		payloadSize += 4 + len(rec)
	}

	payload := make([]byte, 0, payloadSize)

	for _, rec := range records {
		var prefix [4]byte

		binary.LittleEndian.PutUint32(prefix[:], uint32(len(rec)))
		payload = append(payload, prefix[:]...)
		payload = append(payload, rec...)
	}

	checksum := crc32.ChecksumIEEE(payload)

	temp, err := os.CreateTemp(dirOf(path), ".firc-*")
	if err != nil {
		return Chunk{}, errors.Wrap(err, errors.KindEnvironment,
			Subsystem, "write", "create temporary chunk")
	}

	tempPath := temp.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	now := time.Now().UTC().UnixMilli()

	header := Header{
		Version:     FormatVersion,
		Flags:       flags,
		RecordCount: uint32(len(records)),
		Checksum:    checksum,
	}

	head := make([]byte, HeaderSize)

	if err := EncodeHeader(head, header); err != nil {
		_ = temp.Close()

		return Chunk{}, err
	}

	if _, err := temp.Write(head); err != nil {
		_ = temp.Close()

		return Chunk{}, errors.Wrap(err, errors.KindEnvironment,
			Subsystem, "write", "write header")
	}

	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()

		return Chunk{}, errors.Wrap(err, errors.KindEnvironment,
			Subsystem, "write", "write payload")
	}

	if err := temp.Sync(); err != nil {
		_ = temp.Close()

		return Chunk{}, errors.Wrap(err, errors.KindEnvironment,
			Subsystem, "write", "sync chunk")
	}

	if err := temp.Close(); err != nil {
		return Chunk{}, errors.Wrap(err, errors.KindEnvironment,
			Subsystem, "write", "close chunk")
	}

	if err := os.Rename(tempPath, path); err != nil {
		return Chunk{}, errors.Wrap(err, errors.KindEnvironment,
			Subsystem, "write", "commit chunk")
	}

	return Chunk{
		ID:        ChunkID(path),
		Records:   len(records),
		Bytes:     HeaderSize + payloadSize,
		Checksum:  checksum,
		Flags:     flags,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// ChunkID derives the deterministic chunk identifier from the chunk
// file base name (e.g. "000123" from "/data/chunks/000123.firc").
func ChunkID(path string) string {
	base := filepath.Base(path)

	return strings.TrimSuffix(base, FileExt)
}

// FileExt is the chunk file extension.
const FileExt = ".firc"

func dirOf(path string) string {
	if dir := filepath.Dir(path); dir != "" {
		return dir
	}

	return "."
}

// Reader iterates the records of a chunk file, verifying structure and
// checksums. Records are decoded lazily so large chunks can be scanned
// without materialising the whole payload.
type Reader struct {
	file   *os.File
	reader *bufio.Reader
	header Header
	seen   uint32
}

// OpenReader opens a chunk file and validates its header.
func OpenReader(path string) (*Reader, error) {
	file, err := os.Open(path)
	if err != nil {
		if stderrors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrap(err, errors.KindRecoverable,
				Subsystem, "read", "chunk is missing")
		}

		return nil, errors.Wrap(err, errors.KindEnvironment,
			Subsystem, "read", "open chunk")
	}

	head := make([]byte, HeaderSize)

	if _, err := io.ReadFull(file, head); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("%w: %s", ErrCorrupt, path)
	}

	h, err := DecodeHeader(head)
	if err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("%w: %s", err, path)
	}

	return &Reader{
		file:   file,
		reader: bufio.NewReaderSize(file, 64<<10),
		header: h,
	}, nil
}

// Header returns the decoded chunk header.
func (r *Reader) Header() Header {
	return r.header
}

// Next returns the next record. io.EOF signals a clean end after the
// announced record count; a mismatched count or a checksum failure is
// reported as ErrCorrupt.
func (r *Reader) Next() ([]byte, error) {
	if r.seen >= r.header.RecordCount {
		return nil, io.EOF
	}

	var prefix [4]byte

	if _, err := io.ReadFull(r.reader, prefix[:]); err != nil {
		return nil, fmt.Errorf("%w: truncated record length", ErrCorrupt)
	}

	length := binary.LittleEndian.Uint32(prefix[:])

	if length > MaxRecordBytes {
		return nil, fmt.Errorf("%w: record length %d", ErrTooLarge, length)
	}

	record := make([]byte, length)

	if _, err := io.ReadFull(r.reader, record); err != nil {
		return nil, fmt.Errorf("%w: truncated record payload", ErrCorrupt)
	}

	r.seen++

	return record, nil
}

// Verify walks the whole payload and checks the stored CRC-32. It
// returns nil for a byte-clean chunk. The hash covers the identical
// byte region as WriteChunk: length prefixes and record bodies.
func (r *Reader) Verify() error {
	table := crc32.MakeTable(crc32.IEEE)
	crc := uint32(0)

	var prefixBuf [4]byte

	for {
		rec, err := r.Next()
		if stderrors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return err
		}

		binary.LittleEndian.PutUint32(prefixBuf[:], uint32(len(rec)))
		crc = crc32.Update(crc, table, prefixBuf[:])
		crc = crc32.Update(crc, table, rec)
	}

	if crc != r.header.Checksum {
		return fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}

	return nil
}

// Close closes the underlying file.
func (r *Reader) Close() error {
	return r.file.Close()
}

// ReadAll decodes every record of a chunk into memory. Use for small
// chunks or tests; prefer Next for large ones.
func ReadAll(path string) ([][]byte, error) {
	r, err := OpenReader(path)
	if err != nil {
		return nil, err
	}

	defer r.Close()

	records := make([][]byte, 0, r.header.RecordCount)

	for {
		rec, err := r.Next()
		if stderrors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err
		}

		records = append(records, rec)
	}

	return records, nil
}
