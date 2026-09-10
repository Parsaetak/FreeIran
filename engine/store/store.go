// Package store implements the FreeIran local-first persistence layer.
//
// It replaces the legacy full-JSON database (migrated on demand via
// MigrateFromJSON) with a chunked, incrementally written, crash-safe
// store:
//
//	data/
//	├── store.meta        JSON chunk registry (atomic replace)
//	├── index.bin         binary fingerprint index (atomic replace)
//	├── chunks/NNNNNN.firc checksummed immutable chunk files
//	└── wal/seg-XXXXXXXX.wal segmented write-ahead journal
//
// Write path (incremental, never rewrites the whole dataset):
//
//	UpsertBatch → WAL append (one write + one fsync) → memtable →
//	    (threshold) freeze → immutable frozen table →
//	    background flush worker: ONE new chunk file → index update →
//	    meta update → WAL checkpoint
//
// Backpressure is explicit: when too many frozen tables are pending,
// writers block until the flush worker drains room (bounded memory).
//
// Read path (lazy, index assisted):
//
//	Get → memtable levels (newest first) → index → chunk file
//	    (pinned handle, offset read) → decode value
//
// Startup is metadata-first: Open() loads meta + index only and is
// ready immediately; chunk verification is available via VerifyAll.
// Records are never materialised until requested.
//
// Resource ownership and shutdown:
//
//   - The chunkHandleCache is the sole owner of open chunk
//     descriptors; eviction and shutdown CLOSE files deterministically.
//   - Every public operation registers in an operation barrier;
//     Close() rejects new operations, waits for active ones, flushes,
//     closes the journal, closes all cached handles and only then
//     returns. After Close() the store owns no file descriptor, so the
//     store directory is immediately deletable on every platform
//     (including Windows, where open handles block deletion).
//
// Keys are lowercase hex SHA-256 fingerprints (64 characters), stored
// as 32 raw bytes. Chunk records are [32-byte key][value] pairs so the
// index can always be rebuilt from chunk files alone.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// defaultMaxFrozen bounds the number of pending frozen tables
	// (plus the one being flushed). Writers exceeding this bound are
	// throttled until the flush worker catches up: explicit,
	// bounded-memory backpressure.
	defaultMaxFrozen = 2

	// flushRetryDelay backs off automatic retries after a flush
	// failure (disk full, transient IO errors).
	flushRetryDelay = time.Second
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
	MemtableBytes int

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

	// files is the sole owner of open chunk descriptors.
	files *chunkHandleCache

	// writeMu serializes writers: the WAL append and the memtable
	// apply of one batch happen together so the applied-LSN watermark
	// is always exact. It is never held while reading.
	writeMu sync.Mutex

	// mu guards the index, chunk registry and memtable levels.
	mu        sync.RWMutex
	active    *memtable
	frozen    []*frozenTable
	flushing  *frozenTable
	chunksMap map[uint32]*chunkMeta
	nextSeq   uint32

	// lsn is the WAL watermark of records applied to the memtable
	// levels; checkpointLSN is the watermark already durable in
	// chunks+meta (persisted in store.meta, used to filter replay).
	lsn           uint64
	checkpointLSN uint64

	deadRecs int64
	status   string // ready | degraded | closed

	index map[[32]byte]loc
	wal   *journal

	// Lifecycle: closed rejects new operations; gate serializes op
	// registration with the shutdown flip (an Add racing a Wait on a
	// zero counter is a data race per the sync docs); ops tracks
	// in-flight operations; scans tracks long-running chunk readers
	// (iteration, verification) whose open handles must outlive
	// compaction file removal.
	closed atomic.Bool
	gate   sync.RWMutex
	ops    sync.WaitGroup
	scans  sync.WaitGroup

	// Background flush worker.
	flushCond *sync.Cond
	flushWake chan struct{}
	flushStop chan struct{}
	flushWG   sync.WaitGroup
	stopping  atomic.Bool
	flushErr  error // guarded by mu; sticky until a flush succeeds

	// compactMu serializes compaction runs.
	compactMu sync.Mutex

	// Internal counters (atomic; exposed via Diagnostics).
	flushCount    atomic.Int64
	compactCount  atomic.Int64
	lastFlushMS   atomic.Int64
	lastCompactMS atomic.Int64
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
		active:    newMemtable(),
		chunksMap: make(map[uint32]*chunkMeta),
		index:     make(map[[32]byte]loc),
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

	s.files = newChunkHandleCache(s.opts.OpenFiles, s.chunkPath)

	for _, dir := range []string{s.root, s.chunkDir, s.walDir} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "open", "create store directory")
		}
	}

	if err := s.loadOrRecover(); err != nil {
		return nil, err
	}

	journal, err := openJournal(s.walDir, s.checkpointLSN, s.checkpointLSN, func(
		op walOp,
		key [32]byte,
		value []byte,
		lsn uint64,
	) error {
		return s.applyReplay(op, key, value, lsn)
	})
	if err != nil {
		return nil, err
	}

	s.wal = journal

	// Bring the applied-LSN watermark to at least the journal's last
	// assigned LSN so fresh freezes never checkpoint below reality.
	if lsn := journal.CurrentLSN(); lsn > s.lsn {
		s.lsn = lsn
	}

	s.flushCond = sync.NewCond(&s.mu)
	s.flushWake = make(chan struct{}, 1)
	s.flushStop = make(chan struct{})

	s.startFlushWorker()

	return s, nil
}

func orDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}

	return value
}

// decodeKey converts a hex fingerprint into its binary key form in a
// single pass with no intermediate allocations. It enforces the exact
// canonical lowercase form.
func decodeKey(key string) ([32]byte, error) {
	var out [32]byte

	if len(key) != 64 {
		return out, fmt.Errorf("%w: length %d", ErrBadKey, len(key))
	}

	for i := 0; i < 32; i++ {
		hi, ok := hexNibble(key[2*i])
		if !ok {
			return out, fmt.Errorf("%w: byte %d", ErrBadKey, 2*i)
		}

		lo, ok := hexNibble(key[2*i+1])
		if !ok {
			return out, fmt.Errorf("%w: byte %d", ErrBadKey, 2*i+1)
		}

		out[i] = hi<<4 | lo
	}

	return out, nil
}

// hexNibble decodes one lowercase hex digit.
func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}

func encodeKey(key [32]byte) string {
	const hexDigits = "0123456789abcdef"

	out := make([]byte, 64)
	for i := 0; i < 32; i++ {
		out[2*i] = hexDigits[key[i]>>4]
		out[2*i+1] = hexDigits[key[i]&0x0f]
	}

	return string(out)
}

// beginOp registers a public operation; it reports false once the
// store is closed. The gate makes registration race-free against
// Close: either the operation registers before the shutdown flip is
// observed (and Close waits for it), or it observes the flip and is
// rejected without ever touching the counter.
func (s *Store) beginOp() bool {
	s.gate.RLock()
	defer s.gate.RUnlock()

	if s.closed.Load() {
		return false
	}

	s.ops.Add(1)

	return true
}

func (s *Store) endOp() {
	s.ops.Done()
}

// Get returns the live value for a key. Values are decoded on demand;
// nothing is loaded except the requested record. The returned buffer
// is owned by the caller.
func (s *Store) Get(key string) ([]byte, error) {
	bin, err := decodeKey(key)
	if err != nil {
		return nil, err
	}

	if !s.beginOp() {
		return nil, ErrClosed
	}

	defer s.endOp()

	// The read lock is held through the chunk read: compaction swaps
	// index entries under the write lock, so a location observed under
	// RLock stays valid until the read completes.
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.get(bin)
}

// get serves one lookup; callers hold s.mu (read or write).
func (s *Store) get(bin [32]byte) ([]byte, error) {
	if value, state := s.lookupLevels(bin); state != levelAbsent {
		if state == levelTombstone {
			return nil, ErrNotFound
		}

		// Defensive copy: the table's buffer stays owned by the table.
		return append([]byte(nil), value...), nil
	}

	position, ok := s.index[bin]
	if !ok {
		return nil, ErrNotFound
	}

	return s.readRecord(position)
}

// levelState classifies the newest memtable state of a key.
type levelState int

const (
	levelAbsent levelState = iota
	levelLive
	levelTombstone
)

// lookupLevels finds the newest memtable state for bin: the active
// table first, then frozen tables from newest to oldest. Callers hold
// s.mu.
func (s *Store) lookupLevels(bin [32]byte) ([]byte, levelState) {
	if value, ok := s.active.entries[bin]; ok {
		if value == nil {
			return nil, levelTombstone
		}

		return value, levelLive
	}

	for i := len(s.frozen) - 1; i >= 0; i-- {
		if value, ok := s.frozen[i].table.entries[bin]; ok {
			if value == nil {
				return nil, levelTombstone
			}

			return value, levelLive
		}
	}

	if s.flushing != nil {
		if value, ok := s.flushing.table.entries[bin]; ok {
			if value == nil {
				return nil, levelTombstone
			}

			return value, levelLive
		}
	}

	return nil, levelAbsent
}

// Has reports whether a key is live.
func (s *Store) Has(key string) bool {
	bin, err := decodeKey(key)
	if err != nil {
		return false
	}

	if !s.beginOp() {
		return false
	}

	defer s.endOp()

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.isLive(bin)
}

// isLive reports whether bin has a live record; callers hold s.mu.
func (s *Store) isLive(bin [32]byte) bool {
	if _, state := s.lookupLevels(bin); state != levelAbsent {
		return state == levelLive
	}

	_, ok := s.index[bin]

	return ok
}

// Count returns the number of live records, including unflushed ones.
// It is derived from the index and memtable levels so it can never
// drift: deletes remove index entries immediately and resurrections
// are visible in the memtable.
func (s *Store) Count() int {
	if !s.beginOp() {
		return 0
	}

	defer s.endOp()

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.derivedCount()
}

// derivedCount computes the live record count; callers hold s.mu.
func (s *Store) derivedCount() int {
	total := len(s.index)

	// Walk levels newest → oldest; the first level mentioning a key
	// decides its fate, and `seen` stops older duplicates.
	seen := make(map[[32]byte]struct{}, len(s.active.entries))

	check := func(table *memtable) {
		for bin, value := range table.entries {
			if _, dup := seen[bin]; dup {
				continue
			}

			seen[bin] = struct{}{}

			if value == nil {
				if _, inIndex := s.index[bin]; inIndex {
					total--
				}
			} else if _, inIndex := s.index[bin]; !inIndex {
				total++
			}
		}
	}

	if s.flushing != nil {
		check(s.flushing.table)
	}

	for i := len(s.frozen) - 1; i >= 0; i-- {
		check(s.frozen[i].table)
	}

	check(s.active)

	return total
}

// Upsert stages one record. The write is journaled (and fsynced)
// immediately; it becomes a chunk on the next flush.
func (s *Store) Upsert(key string, value []byte) error {
	return s.UpsertBatch([]Pair{{Key: key, Value: value}})
}

// UpsertBatch stages a batch of records atomically: the whole batch
// is journaled with one write and one fsync before any of it is
// applied to the memtable. Keys are decoded once; the write lock is
// acquired once. When too many frozen tables are pending, the caller
// is throttled until the background flush worker drains room.
func (s *Store) UpsertBatch(pairs []Pair) error {
	if len(pairs) == 0 {
		return nil
	}

	if !s.beginOp() {
		return ErrClosed
	}

	defer s.endOp()

	records := make([]journalRecord, 0, len(pairs))

	for i := range pairs {
		if len(pairs[i].Value) == 0 {
			return ErrEmptyValue
		}

		bin, err := decodeKey(pairs[i].Key)
		if err != nil {
			return err
		}

		records = append(records, journalRecord{
			Op:    opUpsert,
			Key:   bin,
			Value: pairs[i].Value,
		})
	}

	return s.writeRecords(records)
}

// writeRecords journals and applies one batch. records must be
// pre-validated.
func (s *Store) writeRecords(records []journalRecord) error {
	s.writeMu.Lock()

	lastLSN, err := s.wal.Append(records)
	if err != nil {
		s.writeMu.Unlock()

		return err
	}

	var throttle bool

	s.mu.Lock()

	for i := range records {
		s.applyUpsert(records[i].Key, records[i].Value)
	}

	if lastLSN > s.lsn {
		s.lsn = lastLSN
	}

	if s.memtablePressureLocked() {
		s.freezeLocked()
	}

	if s.pendingTables() >= defaultMaxFrozen {
		throttle = true
	}

	s.mu.Unlock()
	s.writeMu.Unlock()

	// Explicit backpressure: the store never buffers more than
	// maxFrozen pending tables in memory. (Standard condition-variable
	// idiom: Lock once, Wait inside the predicate loop — Wait returns
	// holding the lock, so the predicate must be re-checked, never
	// re-locked.)
	if throttle {
		s.mu.Lock()

		for s.pendingTables() >= defaultMaxFrozen {
			s.flushCond.Wait()
		}

		s.mu.Unlock()
	}

	return nil
}

// pendingTables counts frozen tables plus the one being flushed.
// Callers hold s.mu.
func (s *Store) pendingTables() int {
	pending := len(s.frozen)

	if s.flushing != nil {
		pending++
	}

	return pending
}

// Delete tombstones a key. Physical space is reclaimed by compaction.
func (s *Store) Delete(key string) error {
	bin, err := decodeKey(key)
	if err != nil {
		return err
	}

	if !s.beginOp() {
		return ErrClosed
	}

	defer s.endOp()

	s.mu.RLock()
	live := s.isLive(bin)
	s.mu.RUnlock()

	if !live {
		return nil
	}

	s.writeMu.Lock()

	_, err = s.wal.Append([]journalRecord{{Op: opDelete, Key: bin}})
	if err != nil {
		s.writeMu.Unlock()

		return err
	}

	s.mu.Lock()
	s.applyDelete(bin)
	s.mu.Unlock()

	s.writeMu.Unlock()

	return nil
}

// applyUpsert mutates in-memory state; callers hold s.mu AND the
// write mutex (via writeRecords / applyReplay during open).
func (s *Store) applyUpsert(bin [32]byte, value []byte) {
	if _, state := s.lookupLevels(bin); state != levelAbsent {
		// The key already has a pending state; the newest write simply
		// replaces it in the active table. Dead-record accounting for
		// the flushed copy was done when the FIRST pending state was
		// created.
		s.active.put(bin, value)

		return
	}

	// First pending state for this key: a flushed copy (if any)
	// becomes dead once this value flushes on top of it.
	if position, ok := s.index[bin]; ok {
		if meta, metaOK := s.chunksMap[position.chunk]; metaOK && meta.Live > 0 {
			meta.Live--
			s.deadRecs++
		}
	}

	s.active.put(bin, value)
}

// applyDelete mutates in-memory state; callers hold s.mu. The index
// entry is removed immediately so the delete survives a flush of the
// tombstone (the tombstone itself is then dropped by the flush).
func (s *Store) applyDelete(bin [32]byte) {
	if _, state := s.lookupLevels(bin); state != levelAbsent {
		// Pending state exists: the flushed copy (if any) became dead
		// when the pending upsert was applied; remove the index entry
		// so nothing resurrects after the tombstone flushes.
		if position, ok := s.index[bin]; ok {
			if meta, metaOK := s.chunksMap[position.chunk]; metaOK && meta.Live > 0 {
				meta.Live--
				s.deadRecs++
			}

			delete(s.index, bin)
		}

		s.active.tombstone(bin)

		return
	}

	if position, ok := s.index[bin]; ok {
		if meta, metaOK := s.chunksMap[position.chunk]; metaOK && meta.Live > 0 {
			meta.Live--
			s.deadRecs++
		}

		delete(s.index, bin)

		s.active.tombstone(bin)
	}
}

// applyReplay re-applies a journal record during startup.
func (s *Store) applyReplay(op walOp, bin [32]byte, value []byte, lsn uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch op {
	case opUpsert:
		s.applyUpsert(bin, value)

	case opDelete:
		s.applyDelete(bin)
	}

	if lsn > s.lsn {
		s.lsn = lsn
	}

	return nil
}

// memtablePressureLocked reports whether the active table should be
// frozen. Callers hold s.mu.
func (s *Store) memtablePressureLocked() bool {
	return s.active.len() >= s.opts.MemtableRecords ||
		s.active.bytes >= int64(s.opts.MemtableBytes)
}

// freezeLocked moves the active table into the flush queue.
// Callers hold s.mu (and normally the write mutex).
func (s *Store) freezeLocked() {
	if s.active.len() == 0 {
		return
	}

	s.frozen = append(s.frozen, &frozenTable{
		table: s.active,
		lsn:   s.lsn,
	})

	s.active = newMemtable()
	s.wakeFlusher()
}

// wakeFlusher nudges the background worker without blocking.
func (s *Store) wakeFlusher() {
	select {
	case s.flushWake <- struct{}{}:
	default:
	}
}

// Flush durably persists all pending state: the active table is
// frozen and every queued table is written to chunks, after which the
// WAL is checkpointed. It returns when the store is fully drained.
func (s *Store) Flush() error {
	if !s.beginOp() {
		return ErrClosed
	}

	defer s.endOp()

	s.writeMu.Lock()

	s.mu.Lock()
	s.freezeLocked()
	s.mu.Unlock()

	s.writeMu.Unlock()

	s.mu.Lock()

	for s.pendingTables() > 0 {
		if s.flushErr != nil && s.stopping.Load() {
			err := s.flushErr
			s.mu.Unlock()

			return err
		}

		s.flushCond.Wait()
	}

	err := s.flushErr
	s.mu.Unlock()

	return err
}

// chunkPath is the filesystem location of one chunk.
func (s *Store) chunkPath(seq uint32) string {
	return filepath.Join(s.chunkDir,
		fmt.Sprintf("%06d%s", seq, chunks.FileExt))
}

// readRecord reads one record by index position through a pinned
// chunk handle. Callers must either hold s.mu (Get) or be registered
// as a scan (Iterate) so compaction cannot remove the file mid-read.
func (s *Store) readRecord(position loc) ([]byte, error) {
	handle, err := s.files.acquire(position.chunk)
	if err != nil {
		return nil, err
	}

	defer s.files.release(position.chunk)

	return readRecordAt(handle, position.offset)
}

// readRecordAt decodes one length-prefixed record at offset using
// positional reads; no other record of the chunk is touched.
func readRecordAt(handle readerAt, offset int64) ([]byte, error) {
	var prefix [4]byte

	if _, err := handle.ReadAt(prefix[:], offset); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "read", "record length at offset %d", offset)
	}

	length := binaryUint32(prefix)

	if length < 32 || length > chunks.MaxRecordBytes {
		return nil, firerrors.New(firerrors.KindCorruptData,
			Subsystem, "read", "record length %d out of range", length)
	}

	body := make([]byte, length)

	if _, err := handle.ReadAt(body, offset+4); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "read", "record body at offset %d", offset)
	}

	return body[32:], nil
}

// readerAt abstracts *os.File for tests.
type readerAt interface {
	ReadAt(p []byte, off int64) (int, error)
}

// binaryUint32 decodes a little-endian uint32.
func binaryUint32(buf [4]byte) uint32 {
	return uint32(buf[0]) | uint32(buf[1])<<8 |
		uint32(buf[2])<<16 | uint32(buf[3])<<24
}

// Close flushes pending writes and releases every resource the store
// owns. It is idempotent, rejects nothing silently and guarantees that
// no file descriptor remains open when it returns: the store directory
// can be deleted immediately afterwards on every platform.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}

	s.stopping.Store(true)

	// 1. Exclude new operations and drain in-flight ones. Acquiring the
	// gate write lock guarantees no beginOp critical section is still
	// running, so ops.Wait cannot race an Add.
	s.gate.Lock()
	s.ops.Wait()
	s.gate.Unlock()

	// 2. Freeze whatever is left so the worker flushes it.
	s.writeMu.Lock()

	s.mu.Lock()
	s.freezeLocked()
	s.mu.Unlock()

	s.writeMu.Unlock()

	// 3. Stop the worker; it drains the remaining queue first.
	close(s.flushStop)
	s.flushWG.Wait()

	// 4. Close the journal (final sync + close).
	walErr := s.wal.Close()

	// 5. Close every cached chunk handle.
	filesErr := s.files.closeAll()

	s.mu.Lock()
	s.status = "closed"
	flushErr := s.flushErr
	s.mu.Unlock()

	return errors.Join(flushErr, walErr, filesErr)
}

// Iterate walks every live record in key order. It never
// materialises the dataset: index entries are captured as 40-byte
// references and pending values as immutable slice headers; chunk
// records are streamed through pinned handles. The callback receives
// key strings and a value buffer owned by the snapshot (do not retain
// or mutate it).
func (s *Store) Iterate(
	ctx context.Context,
	fn func(key string, value []byte) error,
) error {
	if !s.beginOp() {
		return ErrClosed
	}

	defer s.endOp()

	// Register as a scan BEFORE snapshotting: compaction removes
	// victim files only when no scan is active, and a scan registered
	// before the compaction swap is guaranteed to complete first.
	s.scans.Add(1)
	defer s.scans.Done()

	snapshot, err := s.iterSnapshot()
	if err != nil {
		return err
	}

	sort.Slice(snapshot, func(i, j int) bool {
		return lessKey(snapshot[i].key, snapshot[j].key)
	})

	for i := range snapshot {
		if err := ctx.Err(); err != nil {
			return err
		}

		entry := &snapshot[i]

		if entry.memState == levelTombstone {
			continue
		}

		if entry.memState == levelLive {
			if err := fn(encodeKey(entry.key), entry.memValue); err != nil {
				return err
			}

			continue
		}

		value, err := s.readRecord(entry.position)
		if err != nil {
			return err
		}

		if err := fn(encodeKey(entry.key), value); err != nil {
			return err
		}
	}

	return nil
}

// iterEntry is one merged snapshot row.
type iterEntry struct {
	key      [32]byte
	position loc
	memState levelState
	memValue []byte // immutable; owned by the memtable level
}

// iterSnapshot captures the merged view (index + memtable levels)
// under one read lock. Values from memtable levels are immutable
// buffers, so their slice headers stay valid after the lock is
// released.
func (s *Store) iterSnapshot() ([]iterEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	merged := make(map[[32]byte]*iterEntry,
		len(s.index)+len(s.active.entries))

	for bin, position := range s.index {
		merged[bin] = &iterEntry{key: bin, position: position}
	}

	// Oldest level first so newer levels overwrite older state.
	setState := func(table *memtable) {
		for bin, value := range table.entries {
			entry, ok := merged[bin]
			if !ok {
				entry = &iterEntry{key: bin}
				merged[bin] = entry
			}

			if value == nil {
				entry.memState = levelTombstone
				entry.memValue = nil
			} else {
				entry.memState = levelLive
				entry.memValue = value
			}
		}
	}

	for i := 0; i < len(s.frozen); i++ {
		setState(s.frozen[i].table)
	}

	if s.flushing != nil {
		setState(s.flushing.table)
	}

	setState(s.active)

	out := make([]iterEntry, 0, len(merged))

	for _, entry := range merged {
		out = append(out, *entry)
	}

	return out, nil
}

func lessKey(a, b [32]byte) bool {
	for i := 0; i < 32; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}

	return false
}
