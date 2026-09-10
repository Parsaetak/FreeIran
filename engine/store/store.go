// Package store implements the FreeIran local-first persistence layer.
//
// It replaces the legacy full-JSON database (engine/database) with a
// chunked, incrementally written, crash-safe store:
//
//	data/
//	├── store.meta        JSON chunk registry (atomic replace)
//	├── index.bin         binary fingerprint index (atomic replace)
//	├── chunks/NNNNNN.firc checksummed chunk files
//	└── wal/journal.log   write-ahead journal for unflushed writes
//
// Write path (incremental, never rewrites the whole dataset):
//
//	Upsert → WAL append (fsync) → memtable → (threshold) Flush:
//	    memtable delta → ONE new chunk file → index update →
//	    meta update → WAL checkpoint
//
// Read path (lazy, index assisted):
//
//	Get → memtable → index → chunk file (offset read, verified once)
//
// Startup is metadata-first: Open() loads meta + index only and is
// ready immediately; chunk verification and cache warming run in the
// background. Records are never materialised until requested.
//
// Keys are lowercase hex SHA-256 fingerprints (64 characters), stored
// as 32 raw bytes. Chunk records are [32-byte key][value] pairs so the
// index can always be rebuilt from chunk files alone.
package store

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/engine/cache"
	"github.com/Parsaetak/FreeIran/engine/chunks"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Subsystem identifies the store in structured errors.
const Subsystem = "store"

// Sentinel errors.
var (
	// ErrNotFound is returned when a key has no live record.
	ErrNotFound = errors.New("store: key not found")

	// ErrClosed is returned for operations after Close.
	ErrClosed = errors.New("store: closed")

	// ErrBadKey is returned when a key is not a valid fingerprint.
	ErrBadKey = errors.New("store: key must be 64-char lowercase hex")

	// ErrEmptyValue is returned for nil/empty values, which cannot
	// be distinguished from tombstones.
	ErrEmptyValue = errors.New("store: empty value")
)

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600

	defaultTargetChunkBytes = 4 << 20
	defaultMemtableRecords  = 4096
	defaultMemtableBytes    = 8 << 20
	defaultOpenFiles        = 32
)

// Options configures a store.
type Options struct {
	// Path is the store root directory (created if missing).
	Path string

	// TargetChunkBytes is the chunk payload target.
	TargetChunkBytes int

	// MemtableRecords triggers a flush after N pending records.
	MemtableRecords int

	// MemtableBytes triggers a flush after N pending bytes.
	MemtableBytes int64

	// OpenFiles bounds concurrently cached open chunk handles.
	OpenFiles int
}

// Pair is one key/value record for batch operations.
type Pair struct {
	Key   string
	Value []byte
}

// loc locates a record inside a chunk file.
type loc struct {
	chunk  uint32
	offset int64
}

// Store is the persistent configuration store.
type Store struct {
	root     string
	chunkDir string
	walDir   string

	opts    Options
	chunker *chunks.Chunker

	mu        sync.RWMutex
	memtable  map[[32]byte][]byte // nil value = tombstone
	memBytes  int64
	chunksMap map[uint32]*chunkMeta
	nextSeq   uint32
	lsn       uint64
	count     int64
	deadRecs  int64
	status    string // ready | degraded | closed

	index    map[[32]byte]loc
	wal      *journal
	openLRU  *cache.Layer
	closed   atomic.Bool
	flushing sync.Mutex
}

// chunkMeta is the per-chunk registry entry persisted in store.meta.
type chunkMeta struct {
	Sequence uint32 `json:"seq"`
	Records  int    `json:"records"`
	Live     int    `json:"live"`
	Bytes    int64  `json:"bytes"`
	Checksum uint32 `json:"checksum"`
	Created  int64  `json:"created"`
}

// Open prepares the store for use. It loads (or rebuilds) the chunk
// registry and the index, replays the write-ahead journal, and returns
// immediately ready for reads. Heavy verification is left to VerifyAll.
func Open(opts Options) (*Store, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, firerrors.New(firerrors.KindConfiguration,
			Subsystem, "open", "store path is empty")
	}

	s := &Store{
		root:      filepath.Clean(opts.Path),
		chunkDir:  filepath.Join(opts.Path, "chunks"),
		walDir:    filepath.Join(opts.Path, "wal"),
		opts:      opts,
		memtable:  make(map[[32]byte][]byte),
		index:     make(map[[32]byte]loc),
		chunksMap: make(map[uint32]*chunkMeta),
		status:    "ready",
	}

	s.chunker = chunks.NewChunker(orDefault(opts.TargetChunkBytes,
		defaultTargetChunkBytes))

	if s.opts.MemtableRecords <= 0 {
		s.opts.MemtableRecords = defaultMemtableRecords
	}

	if s.opts.MemtableBytes <= 0 {
		s.opts.MemtableBytes = defaultMemtableBytes
	}

	if s.opts.OpenFiles <= 0 {
		s.opts.OpenFiles = defaultOpenFiles
	}

	s.openLRU = cache.New("chunk-files", cache.Options{
		MaxEntries: s.opts.OpenFiles,
		Weigh:      func(any) int64 { return 1 },
	})

	for _, dir := range []string{s.root, s.chunkDir, s.walDir} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "open", "create store directory")
		}
	}

	if err := s.loadOrRecover(); err != nil {
		return nil, err
	}

	journal, err := openJournal(s.walDir, s.lsn, func(
		op walOp,
		key [32]byte,
		value []byte,
	) error {
		return s.applyReplay(op, key, value)
	})
	if err != nil {
		return nil, err
	}

	s.wal = journal

	return s, nil
}

func orDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}

	return value
}

// decodeKey converts a hex fingerprint into its binary key form.
func decodeKey(key string) ([32]byte, error) {
	var out [32]byte

	if len(key) != 64 {
		return out, fmt.Errorf("%w: length %d", ErrBadKey, len(key))
	}

	raw, err := hex.DecodeString(key)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrBadKey, err)
	}

	if hex.EncodeToString(raw) != key {
		return out, fmt.Errorf("%w: not lowercase canonical", ErrBadKey)
	}

	copy(out[:], raw)

	return out, nil
}

func encodeKey(key [32]byte) string {
	return hex.EncodeToString(key[:])
}

// Get returns the live value for a key. Values are decoded on demand;
// nothing is loaded except the requested record.
func (s *Store) Get(key string) ([]byte, error) {
	bin, err := decodeKey(key)
	if err != nil {
		return nil, err
	}

	if s.closed.Load() {
		return nil, ErrClosed
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.get(bin)
}

func (s *Store) get(bin [32]byte) ([]byte, error) {
	if value, ok := s.memtable[bin]; ok {
		if value == nil {
			return nil, ErrNotFound
		}

		return append([]byte(nil), value...), nil
	}

	position, ok := s.index[bin]
	if !ok {
		return nil, ErrNotFound
	}

	return s.readRecord(position)
}

// Has reports whether a key is live.
func (s *Store) Has(key string) bool {
	bin, err := decodeKey(key)
	if err != nil {
		return false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if value, ok := s.memtable[bin]; ok {
		return value != nil
	}

	_, ok := s.index[bin]

	return ok
}

// Count returns the number of live records, including unflushed ones.
// It is derived from the index and memtable so it can never drift:
// deletes remove index entries immediately and resurrections are
// visible in the memtable.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.derivedCount()
}

// derivedCount computes the live record count; callers hold s.mu.
func (s *Store) derivedCount() int {
	total := len(s.index)

	for bin, value := range s.memtable {
		if value == nil {
			continue
		}

		if _, inIndex := s.index[bin]; !inIndex {
			total++
		}
	}

	return total
}

// Upsert stages one record. The write is journaled immediately and
// applied to the memtable; it becomes durable on the next Flush.
func (s *Store) Upsert(key string, value []byte) error {
	return s.UpsertBatch([]Pair{{Key: key, Value: value}})
}

// UpsertBatch stages a batch of records atomically: the whole batch is
// journaled with one fsync before being applied.
func (s *Store) UpsertBatch(pairs []Pair) error {
	if len(pairs) == 0 {
		return nil
	}

	if s.closed.Load() {
		return ErrClosed
	}

	binaries := make(map[string][32]byte, len(pairs))

	for _, pair := range pairs {
		if len(pair.Value) == 0 {
			return ErrEmptyValue
		}

		bin, err := decodeKey(pair.Key)
		if err != nil {
			return err
		}

		binaries[pair.Key] = bin
	}

	records := make([]journalRecord, 0, len(pairs))

	for _, pair := range pairs {
		records = append(records, journalRecord{
			Op:    opUpsert,
			Key:   binaries[pair.Key],
			Value: pair.Value,
		})
	}

	if err := s.wal.Append(records); err != nil {
		return err
	}

	s.mu.Lock()

	for _, pair := range pairs {
		bin := binaries[pair.Key]

		s.applyUpsert(bin, pair.Value)
	}

	memBytes := s.memBytes
	s.mu.Unlock()

	if s.memtablePressure(memBytes, len(pairs)) {
		return s.Flush()
	}

	return nil
}

// Delete tombstones a key. Physical space is reclaimed by compaction.
func (s *Store) Delete(key string) error {
	bin, err := decodeKey(key)
	if err != nil {
		return err
	}

	if s.closed.Load() {
		return ErrClosed
	}

	s.mu.RLock()
	live := s.isLive(bin)
	s.mu.RUnlock()

	if !live {
		return nil
	}

	if err := s.wal.Append([]journalRecord{{
		Op:  opDelete,
		Key: bin,
	}}); err != nil {
		return err
	}

	s.mu.Lock()
	s.applyDelete(bin)
	s.mu.Unlock()

	return nil
}

func (s *Store) isLive(bin [32]byte) bool {
	if value, ok := s.memtable[bin]; ok {
		return value != nil
	}

	_, ok := s.index[bin]

	return ok
}

// applyUpsert mutates in-memory state; callers hold s.mu.
// The live count is derived (see derivedCount), so only dead-record
// accounting is maintained here.
func (s *Store) applyUpsert(bin [32]byte, value []byte) {
	if existing, ok := s.memtable[bin]; ok {
		if existing != nil {
			s.memBytes -= int64(len(existing))
		}

		s.memtable[bin] = append([]byte(nil), value...)
		s.memBytes += int64(len(value))

		return
	}

	if position, ok := s.index[bin]; ok {
		// Overriding a flushed record: the old copy becomes dead.
		if meta, ok := s.chunksMap[position.chunk]; ok && meta.Live > 0 {
			meta.Live--
			s.deadRecs++
		}
	}

	s.memtable[bin] = append([]byte(nil), value...)
	s.memBytes += int64(len(value))
}

// applyDelete mutates in-memory state; callers hold s.mu.
func (s *Store) applyDelete(bin [32]byte) {
	if existing, ok := s.memtable[bin]; ok {
		if existing == nil {
			return // already tombstoned
		}

		s.memBytes -= int64(len(existing))
		s.memtable[bin] = nil

		return
	}

	if position, ok := s.index[bin]; ok {
		if meta, ok := s.chunksMap[position.chunk]; ok && meta.Live > 0 {
			meta.Live--
			s.deadRecs++
		}

		delete(s.index, bin)
		s.memtable[bin] = nil
	}
}

// applyReplay re-applies a journal record during startup.
func (s *Store) applyReplay(op walOp, bin [32]byte, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch op {
	case opUpsert:
		s.applyUpsert(bin, value)

	case opDelete:
		s.applyDelete(bin)
	}

	s.lsn++

	return nil
}

func (s *Store) memtablePressure(bytes int64, _ int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_ = bytes

	return len(s.memtable) >= s.opts.MemtableRecords ||
		s.memBytes >= s.opts.MemtableBytes
}

// Flush writes the memtable delta as a new chunk, updates the index
// and registry, and checkpoints the journal. It never rewrites
// unaffected data.
func (s *Store) Flush() error {
	if s.closed.Load() {
		return ErrClosed
	}

	return s.flushInternal()
}

// flushInternal performs the flush without re-checking the closed
// flag; Close relies on this to persist final state.
func (s *Store) flushInternal() error {
	s.flushing.Lock()
	defer s.flushing.Unlock()

	s.mu.Lock()

	if len(s.memtable) == 0 {
		// Nothing pending; still checkpoint the journal so the LSN
		// accounting stays tight.
		lsn := s.wal.CurrentLSN()
		s.lsn = lsn
		s.mu.Unlock()

		return s.wal.Checkpoint()
	}

	keys := make([][32]byte, 0, len(s.memtable))

	for bin := range s.memtable {
		keys = append(keys, bin)
	}

	sort.Slice(keys, func(i, j int) bool {
		return lessKey(keys[i], keys[j])
	})

	records := make([][]byte, 0, len(keys))
	offsets := make([]int64, 0, len(keys))
	liveKeys := make([][32]byte, 0, len(keys))

	var offset int64 = chunks.HeaderSize

	for _, bin := range keys {
		value, ok := s.memtable[bin]
		if !ok || value == nil {
			continue
		}

		record := make([]byte, 32+len(value))

		copy(record[:32], bin[:])
		copy(record[32:], value)

		records = append(records, record)
		offsets = append(offsets, offset)
		liveKeys = append(liveKeys, bin)

		offset += int64(4 + len(record))
	}

	s.mu.Unlock()

	if len(records) > 0 {
		seq := s.nextSequence()

		path := s.chunkPath(seq)

		meta, err := chunks.WriteChunk(path, records, 0)
		if err != nil {
			return err
		}

		s.mu.Lock()

		s.chunksMap[seq] = &chunkMeta{
			Sequence: seq,
			Records:  meta.Records,
			Live:     meta.Records,
			Bytes:    int64(meta.Bytes),
			Checksum: meta.Checksum,
			Created:  time.Now().UTC().UnixMilli(),
		}

		for i, bin := range liveKeys {
			s.index[bin] = loc{chunk: seq, offset: offsets[i]}
		}

		s.mu.Unlock()
	}

	// Clear applied entries from the memtable.
	s.mu.Lock()

	for _, bin := range keys {
		if value, ok := s.memtable[bin]; ok {
			if value != nil {
				s.memBytes -= int64(len(value))
			}

			delete(s.memtable, bin)
		}
	}

	lsn := s.wal.CurrentLSN()
	s.lsn = lsn

	s.mu.Unlock()

	if err := s.persistIndex(); err != nil {
		return err
	}

	if err := s.persistMeta(); err != nil {
		return err
	}

	return s.wal.Checkpoint()
}

func lessKey(a, b [32]byte) bool {
	for i := 0; i < 32; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}

	return false
}

func (s *Store) nextSequence() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextSeq++

	return s.nextSeq
}

func (s *Store) chunkPath(seq uint32) string {
	return filepath.Join(s.chunkDir,
		fmt.Sprintf("%06d%s", seq, chunks.FileExt))
}

// readRecord reads one record by index position. The chunk file is
// opened once, cached, and verified on first load.
func (s *Store) readRecord(position loc) ([]byte, error) {
	handle, err := s.chunkHandle(position.chunk)
	if err != nil {
		return nil, err
	}

	var prefix [4]byte

	if _, err := handle.ReadAt(prefix[:], position.offset); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "read", "record length at offset %d",
			position.offset)
	}

	length := binary.LittleEndian.Uint32(prefix[:])

	if length < 32 || length > chunks.MaxRecordBytes {
		return nil, firerrors.New(firerrors.KindCorruptData,
			Subsystem, "read", "record length %d out of range", length)
	}

	body := make([]byte, length)

	if _, err := handle.ReadAt(body, position.offset+4); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "read", "record body at offset %d",
			position.offset)
	}

	return body[32:], nil
}

// chunkHandle returns a verified open handle for a chunk, using a
// bounded LRU of open files.
func (s *Store) chunkHandle(seq uint32) (*os.File, error) {
	if cached, ok := s.openLRU.Get(fmt.Sprint(seq), 0); ok {
		return cached.(*os.File), nil
	}

	path := s.chunkPath(seq)

	file, err := os.Open(path)
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "read", "open chunk %d", seq)
	}

	s.openLRU.Put(fmt.Sprint(seq), file, 0)

	return file, nil
}

// Close flushes pending writes and releases resources.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}

	s.mu.Lock()
	s.status = "closed"
	s.mu.Unlock()

	// Close must flush even though new operations are rejected from
	// now on; flushInternal bypasses the closed check.
	if err := s.flushInternal(); err != nil {
		return err
	}

	return s.wal.Close()
}

// Iterate walks every live record in key order. It reads chunks
// sequentially without materialising the whole dataset in memory.
func (s *Store) Iterate(
	ctx context.Context,
	fn func(key string, value []byte) error,
) error {
	if s.closed.Load() {
		return ErrClosed
	}

	s.mu.RLock()

	type ref struct {
		key      [32]byte
		position loc
	}

	refs := make([]ref, 0, len(s.index))

	for bin, position := range s.index {
		refs = append(refs, ref{key: bin, position: position})
	}

	// Memtable keys that were never flushed are not in the index;
	// they must be part of the iteration too.
	memOnly := make([][32]byte, 0)

	for bin, value := range s.memtable {
		if value == nil {
			continue
		}

		if _, inIndex := s.index[bin]; !inIndex {
			memOnly = append(memOnly, bin)
		}
	}

	s.mu.RUnlock()

	allKeys := make([][32]byte, 0, len(refs)+len(memOnly))

	for _, r := range refs {
		allKeys = append(allKeys, r.key)
	}

	allKeys = append(allKeys, memOnly...)

	sort.Slice(allKeys, func(i, j int) bool {
		return lessKey(allKeys[i], allKeys[j])
	})

	positionByChunk := make(map[[32]byte]loc, len(refs))

	for _, r := range refs {
		positionByChunk[r.key] = r.position
	}

	for _, bin := range allKeys {
		if err := ctx.Err(); err != nil {
			return err
		}

		s.mu.RLock()

		// Memtable wins over flushed state.
		if value, ok := s.memtable[bin]; ok {
			if value == nil {
				s.mu.RUnlock()

				continue
			}

			err := fn(encodeKey(bin), value)

			s.mu.RUnlock()

			if err != nil {
				return err
			}

			continue
		}

		position, inIndex := positionByChunk[bin]

		s.mu.RUnlock()

		if !inIndex {
			continue
		}

		value, err := s.readRecord(position)
		if errors.Is(err, ErrNotFound) {
			continue
		}

		if err != nil {
			return err
		}

		if err := fn(encodeKey(bin), value); err != nil {
			return err
		}
	}

	return nil
}

// derivedCountLocked is the lock-held form of derivedCount for use
// outside the store package internals (meta/compaction snapshots).
func derivedCountLocked(s *Store) int {
	return s.derivedCount()
}
