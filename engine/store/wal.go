package store

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Parsaetak/FreeIran/engine/chunks"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Segmented write-ahead journal.
//
// Layout (little-endian), one directory of segments:
//
//	wal/seg-<8-hex-digit-seq>.wal
//
// Segment header: "FIRW" | uint16 version | uint64 baseLSN
// Record: uint8 op | 32-byte key | uint32 valLen (upserts only) |
//
//	value | uint32 crc32(op..value)
//
// Semantics:
//
//   - Every UpsertBatch/Upsert/Delete appends one batch and fsyncs
//     once before returning: a journal-acknowledged write survives a
//     crash even before its chunk is flushed (replay re-applies it).
//   - baseLSN of a segment is the LSN of the record BEFORE its first
//     record; LSNs are contiguous across segments in segment order.
//   - Checkpoint(targetLSN) removes segments whose records all have
//     LSN <= targetLSN. Files are closed before removal, so deletion
//     works on Windows. A segment straddling targetLSN is kept; replay
//     skips its already-incorporated records using the checkpoint LSN.
//   - Segments roll at segmentMaxBytes, bounding each file and making
//     checkpoint removal granular.
//   - A corrupt TAIL of the last segment (crash mid-append) is
//     detected via CRC/length errors and discarded; corruption in a
//     NON-last segment fails the open (real data loss must be loud).
const (
	walMagic   = "FIRW"
	walVersion = uint16(2)

	opUpsert walOp = 1
	opDelete walOp = 2

	walHeaderSize       = 4 + 2 + 8
	legacyWalHeaderSize = 4 + 2 + 8 + 4 // v1 carried a u32 reserved field

	defaultSegmentMaxBytes = 8 << 20 // 8 MiB per segment
)

type walOp byte

// journalRecord is one logical journal entry.
type journalRecord struct {
	Op    walOp
	Key   [32]byte
	Value []byte
}

// replayFunc receives replayed records with their LSN; it returns
// an error to abort.
type replayFunc func(op walOp, key [32]byte, value []byte, lsn uint64) error

// segmentInfo describes one on-disk segment.
type segmentInfo struct {
	path    string
	seq     uint64
	baseLSN uint64
	endLSN  uint64 // last record LSN; equal to baseLSN when empty
	size    int64
}

// journal is the segmented write-ahead log.
type journal struct {
	mu sync.Mutex

	dir         string
	file        *os.File // active segment
	segSeq      uint64   // active segment sequence
	lsn         uint64   // last assigned LSN
	activeBytes int64    // current size of the active segment

	segmentMax int64

	// segEnd tracks, in segment order, the last LSN covered by each
	// live segment (the active segment is the last element).
	segEnd []uint64

	buf []byte // append scratch buffer (reused)
}

// walSegmentName formats a segment file name.
func walSegmentName(seq uint64) string {
	return fmt.Sprintf("seg-%08x.wal", seq)
}

// openJournal opens (creating if needed) the journal directory and
// replays records newer than checkpointLSN. minLSN is the LSN watermark
// recovered from store metadata; the journal never starts counting
// below it even when every segment was checkpointed away.
func openJournal(
	dir string,
	checkpointLSN uint64,
	minLSN uint64,
	replay replayFunc,
) (*journal, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "create journal directory")
	}

	j := &journal{
		dir:        dir,
		segmentMax: defaultSegmentMaxBytes,
		lsn:        minLSN,
	}

	segments, err := j.listSegments()
	if err != nil {
		return nil, err
	}

	if len(segments) > 0 {
		if err := j.replay(segments, checkpointLSN, replay); err != nil {
			return nil, err
		}
	}

	if err := j.openActive(segments); err != nil {
		return nil, err
	}

	// Upgrade path: a v1 single-file journal (journal.log) is streamed,
	// re-applied and re-journaled into the segmented format before its
	// file is removed, so no pending write is ever lost by the upgrade.
	if err := j.migrateLegacyLog(replay); err != nil {
		return nil, err
	}

	return j, nil
}

// listSegments reads the directory and returns segments in replay
// order (ascending sequence numbers), decoding each header.
func (j *journal) listSegments() ([]segmentInfo, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "scan journal directory")
	}

	segments := make([]segmentInfo, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "seg-") ||
			!strings.HasSuffix(entry.Name(), ".wal") {
			continue
		}

		var seq uint64

		if _, scanErr := fmt.Sscanf(
			strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "seg-"), ".wal"),
			"%x", &seq); scanErr != nil {
			continue // unrelated file
		}

		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}

		baseLSN, err := readSegmentHeaderLSN(
			filepath.Join(j.dir, entry.Name()), info.Size())
		if err != nil {
			return nil, err
		}

		segments = append(segments, segmentInfo{
			path:    filepath.Join(j.dir, entry.Name()),
			seq:     seq,
			baseLSN: baseLSN,
			endLSN:  baseLSN,
			size:    info.Size(),
		})
	}

	sort.Slice(segments, func(i, k int) bool {
		return segments[i].seq < segments[k].seq
	})

	return segments, nil
}

// readSegmentHeaderLSN decodes only the baseLSN of a segment header.
func readSegmentHeaderLSN(path string, size int64) (uint64, error) {
	if size < walHeaderSize {
		// A file smaller than the header is the signature of a crash
		// while creating a segment; treat it as an empty segment that
		// will be truncated away.
		return 0, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return 0, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "open segment header")
	}

	defer file.Close()

	head := make([]byte, walHeaderSize)

	if _, err := io.ReadFull(file, head); err != nil {
		return 0, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "wal", "read segment header")
	}

	if string(head[0:4]) != walMagic {
		return 0, firerrors.New(firerrors.KindCorruptData,
			Subsystem, "wal", "segment %s has bad magic",
			filepath.Base(path))
	}

	if version := binary.LittleEndian.Uint16(head[4:6]); version != walVersion {
		return 0, firerrors.New(firerrors.KindCorruptData,
			Subsystem, "wal", "segment %s has unsupported version %d",
			filepath.Base(path), version)
	}

	return binary.LittleEndian.Uint64(head[6:14]), nil
}

// replay validates and applies journal records newer than the
// checkpoint, discarding a corrupt tail of the last segment.
func (j *journal) replay(
	segments []segmentInfo,
	checkpointLSN uint64,
	replay replayFunc,
) error {
	table := crc32.MakeTable(crc32.IEEE)

	prevEnd := uint64(0)

	for i := range segments {
		last := i == len(segments)-1

		file, err := os.Open(segments[i].path)
		if err != nil {
			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "open segment %s",
				filepath.Base(segments[i].path))
		}

		endLSN, goodBytes, replayErr := replaySegment(
			file, segments[i], checkpointLSN, last, table, replay)

		_ = file.Close()

		if replayErr != nil {
			return firerrors.Wrap(replayErr, firerrors.KindCorruptData,
				Subsystem, "wal", "segment %s",
				filepath.Base(segments[i].path))
		}

		// Cross-segment LSN continuity: a gap means a missing segment
		// (real data loss) and must fail the open.
		if prevEnd != 0 && segments[i].baseLSN != prevEnd {
			return firerrors.New(firerrors.KindCorruptData,
				Subsystem, "wal",
				"segment %s base LSN %d breaks continuity (expected %d)",
				filepath.Base(segments[i].path), segments[i].baseLSN, prevEnd)
		}

		prevEnd = endLSN
		segments[i].endLSN = endLSN

		// Truncate a corrupt tail so future appends stay clean.
		if goodBytes >= 0 && segments[i].size > goodBytes {
			if err := os.Truncate(segments[i].path, goodBytes); err != nil {
				return firerrors.Wrap(err, firerrors.KindEnvironment,
					Subsystem, "wal", "truncate corrupt tail of %s",
					filepath.Base(segments[i].path))
			}

			segments[i].size = goodBytes
		}

		if endLSN > j.lsn {
			j.lsn = endLSN
		}
	}

	return nil
}

// replaySegment streams one segment. It returns the last valid LSN,
// the byte offset of the end of the last VALID record (or -1 when the
// segment is structurally unusable), and an error for structural
// corruption that is not an acceptable crash-tail.
func replaySegment(
	file *os.File,
	seg segmentInfo,
	checkpointLSN uint64,
	allowCorruptTail bool,
	table *crc32.Table,
	replay replayFunc,
) (uint64, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return seg.baseLSN, -1, err
	}

	if seg.size < walHeaderSize {
		// Crash during segment creation: no valid records.
		return seg.baseLSN, walHeaderSize, nil
	}

	reader := bufio.NewReaderSize(file, 64<<10)

	head := make([]byte, walHeaderSize)

	if _, err := io.ReadFull(reader, head); err != nil {
		return seg.baseLSN, -1, fmt.Errorf("short header")
	}

	lsn := seg.baseLSN
	goodOffset := int64(walHeaderSize)

	for {
		record, err := readJournalRecord(reader, table)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			if isCorruptTail(err) {
				if !allowCorruptTail {
					return lsn, -1, err
				}

				break // discard truncated tail
			}

			return lsn, -1, err
		}

		lsn++

		if lsn > checkpointLSN {
			if err := replay(walOp(record.Op), record.Key, record.Value, lsn); err != nil {
				return lsn, -1, err
			}
		}

		goodOffset += int64(journalRecordSize(record.Op, record.Value))
	}

	return lsn, goodOffset, nil
}

type rawRecord struct {
	Op    walOp
	Key   [32]byte
	Value []byte
}

func (r rawRecord) encodedSize() int {
	return journalRecordSize(r.Op, r.Value)
}

// journalRecordSize returns the encoded byte size of one journal
// record: op (1) + key (32) + value length (4) + value + crc (4).
func journalRecordSize(op walOp, value []byte) int {
	_ = op

	return 1 + 32 + 4 + len(value) + 4
}

// journalEncodedSize returns the encoded size of one journalRecord.
func journalEncodedSize(r journalRecord) int {
	return journalRecordSize(r.Op, r.Value)
}

func isCorruptTail(err error) bool {
	return errors.Is(err, errCorruptTail)
}

// errCorruptTail marks a record that cannot be decoded completely —
// the signature of a crash mid-append.
var errCorruptTail = firerrors.New(firerrors.KindCorruptData,
	Subsystem, "wal", "corrupt journal tail")

func readJournalRecord(
	reader *bufio.Reader,
	table *crc32.Table,
) (rawRecord, error) {
	var out rawRecord

	opByte, err := reader.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return out, io.EOF
		}

		return out, err
	}

	out.Op = walOp(opByte)

	if out.Op != opUpsert && out.Op != opDelete {
		return out, fmt.Errorf("%w: op %d", errCorruptTail, opByte)
	}

	key, err := readExact(reader, 32)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return out, io.EOF
		}

		return out, fmt.Errorf("%w: short key", errCorruptTail)
	}

	copy(out.Key[:], key)

	// The length prefix is uniform: deletes carry length 0. The
	// v1 WRITER already emitted it for every record, but the v1
	// READER skipped it for deletes, so replayed deletes always
	// failed their CRC and were silently discarded. v2 fixes the
	// reader; v1 journal.log tails migrate cleanly through
	// migrateLegacyLog.
	lenBuf, err := readExact(reader, 4)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return out, io.EOF
		}

		return out, fmt.Errorf("%w: short length", errCorruptTail)
	}

	valueLen := binary.LittleEndian.Uint32(lenBuf)

	if valueLen > chunks.MaxRecordBytes {
		return out, fmt.Errorf("%w: length %d", errCorruptTail, valueLen)
	}

	if out.Op == opDelete && valueLen != 0 {
		return out, fmt.Errorf("%w: delete with value", errCorruptTail)
	}

	if valueLen > 0 {
		value, err := readExact(reader, int(valueLen))
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, io.EOF
			}

			return out, fmt.Errorf("%w: short value", errCorruptTail)
		}

		out.Value = value
	}

	crcBuf, err := readExact(reader, 4)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return out, io.EOF
		}

		return out, fmt.Errorf("%w: short crc", errCorruptTail)
	}

	if binary.LittleEndian.Uint32(crcBuf) != journalCRC(table, out) {
		return out, fmt.Errorf("%w: crc mismatch", errCorruptTail)
	}

	return out, nil
}

func readExact(reader *bufio.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)

	if _, err := io.ReadFull(reader, buf); err != nil {
		return nil, err
	}

	return buf, nil
}

func journalCRC(table *crc32.Table, r rawRecord) uint32 {
	crc := crc32.ChecksumIEEE([]byte{byte(r.Op)})
	crc = crc32.Update(crc, table, r.Key[:])

	var lenBuf [4]byte

	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(r.Value)))
	crc = crc32.Update(crc, table, lenBuf[:])

	if len(r.Value) > 0 {
		crc = crc32.Update(crc, table, r.Value)
	}

	return crc
}

// openActive prepares the segment that appends will go to.
func (j *journal) openActive(segments []segmentInfo) error {
	j.segEnd = j.segEnd[:0]

	for _, seg := range segments {
		j.segEnd = append(j.segEnd, seg.endLSN)
	}

	if len(segments) > 0 {
		last := segments[len(segments)-1]

		file, err := os.OpenFile(last.path, os.O_RDWR, filePerm)
		if err != nil {
			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "reopen active segment")
		}

		info, err := file.Stat()
		if err != nil {
			_ = file.Close()

			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "stat active segment")
		}

		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			_ = file.Close()

			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "seek active segment end")
		}

		j.file = file
		j.segSeq = last.seq
		j.activeBytes = info.Size()

		if len(j.segEnd) > 0 {
			j.segEnd[len(j.segEnd)-1] = j.lsn
		}

		// Roll immediately when the tail segment is already large so
		// the next append starts a fresh segment.
		if j.activeBytes >= j.segmentMax {
			return j.rollLocked()
		}

		return nil
	}

	// No segments at all: create the first one, seeded at the highest
	// known LSN (minLSN from metadata or replay).
	return j.createSegment(0)
}

// createSegment opens a brand-new empty segment with baseLSN = j.lsn.
// Callers hold j.mu.
func (j *journal) createSegment(seq uint64) error {
	path := filepath.Join(j.dir, walSegmentName(seq))

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "create segment %s", filepath.Base(path))
	}

	head := make([]byte, walHeaderSize)
	copy(head[0:4], walMagic)
	binary.LittleEndian.PutUint16(head[4:6], walVersion)
	binary.LittleEndian.PutUint64(head[6:14], j.lsn)

	if _, err := file.Write(head); err != nil {
		_ = file.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "write segment header")
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "sync segment header")
	}

	j.file = file
	j.segSeq = seq
	j.activeBytes = walHeaderSize
	j.segEnd = append(j.segEnd, j.lsn)

	return nil
}

// rollLocked closes the active segment and starts a new one.
// Callers hold j.mu.
func (j *journal) rollLocked() error {
	if err := j.file.Sync(); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "sync before roll")
	}

	if err := j.file.Close(); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "close segment before roll")
	}

	return j.createSegment(j.segSeq + 1)
}

// Append journals a batch with a single write and a single fsync, and
// returns the LSN assigned to the LAST record of the batch.
func (j *journal) Append(records []journalRecord) (uint64, error) {
	if len(records) == 0 {
		j.mu.Lock()
		lsn := j.lsn
		j.mu.Unlock()

		return lsn, nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	table := crc32.MakeTable(crc32.IEEE)

	size := 0

	for i := range records {
		size += journalEncodedSize(records[i])
	}

	if cap(j.buf) < size {
		j.buf = make([]byte, 0, size+size/2)
	}

	buffer := j.buf[:0]

	for i := range records {
		raw := rawRecord{
			Op:    records[i].Op,
			Key:   records[i].Key,
			Value: records[i].Value,
		}

		buffer = append(buffer, byte(raw.Op))
		buffer = append(buffer, raw.Key[:]...)

		var valLen [4]byte

		binary.LittleEndian.PutUint32(valLen[:], uint32(len(raw.Value)))
		buffer = append(buffer, valLen[:]...)
		buffer = append(buffer, raw.Value...)

		var crcBuf [4]byte

		binary.LittleEndian.PutUint32(crcBuf[:], journalCRC(table, raw))
		buffer = append(buffer, crcBuf[:]...)

		j.lsn++
	}

	j.buf = buffer

	if _, err := j.file.Write(buffer); err != nil {
		return j.lsn, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "append")
	}

	if err := j.file.Sync(); err != nil {
		return j.lsn, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "sync")
	}

	j.activeBytes += int64(len(buffer))

	if len(j.segEnd) > 0 {
		j.segEnd[len(j.segEnd)-1] = j.lsn
	}

	if j.activeBytes >= j.segmentMax {
		if err := j.rollLocked(); err != nil {
			return j.lsn, err
		}
	}

	return j.lsn, nil
}

// CurrentLSN returns the last assigned LSN.
func (j *journal) CurrentLSN() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.lsn
}

// segmentCount reports the number of live segments (diagnostics).
func (j *journal) segmentCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()

	return len(j.segEnd)
}

// walBytes reports total on-disk journal bytes (diagnostics).
func (j *journal) walBytes() int64 {
	j.mu.Lock()
	dir := j.dir
	j.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	var total int64

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
	}

	return total
}

// Checkpoint removes segments whose records are all incorporated
// (LSN <= targetLSN). The caller must have persisted the chunk and
// metadata covering targetLSN BEFORE calling. Files are closed before
// removal (Windows-safe). A segment straddling targetLSN is kept;
// replay skips its already-incorporated records using the checkpoint
// LSN recorded in store.meta.
func (j *journal) Checkpoint(targetLSN uint64) error {
	j.mu.Lock()

	if len(j.segEnd) == 0 || j.file == nil {
		j.mu.Unlock()

		return nil
	}

	removeCount := 0

	for removeCount < len(j.segEnd)-1 && j.segEnd[removeCount] <= targetLSN {
		removeCount++
	}

	// Fast path: everything incorporated (idle store / synchronous
	// flush with no concurrent writes) — drop all segments including
	// the active one and start a fresh successor.
	removeActive := targetLSN >= j.lsn
	if removeActive {
		removeCount = len(j.segEnd) - 1
	}

	if removeCount == 0 && !removeActive {
		j.mu.Unlock()

		return nil
	}

	firstSeq := j.segSeq + 1 - uint64(len(j.segEnd))

	toRemove := make([]string, 0, removeCount+1)

	for i := 0; i < removeCount; i++ {
		toRemove = append(toRemove,
			filepath.Join(j.dir, walSegmentName(firstSeq+uint64(i))))
	}

	if removeActive {
		if err := j.file.Sync(); err != nil {
			j.mu.Unlock()

			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "sync before checkpoint roll")
		}

		if err := j.file.Close(); err != nil {
			j.mu.Unlock()

			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "close active before checkpoint roll")
		}

		j.file = nil
		toRemove = append(toRemove,
			filepath.Join(j.dir, walSegmentName(j.segSeq)))

		// Every live segment (active included) is being dropped: clear
		// the bookkeeping BEFORE the successor segment appends itself.
		j.segEnd = j.segEnd[:0]

		if err := j.createSegment(j.segSeq + 1); err != nil {
			j.mu.Unlock()

			return err
		}
	} else {
		j.segEnd = j.segEnd[removeCount:]
	}

	j.mu.Unlock()

	// Remove files after their descriptors are closed and the journal
	// state is consistent. A crash between the state update and the
	// removal leaves an orphan segment whose records are all below the
	// checkpoint; the next open replays and skips them, and the next
	// checkpoint removes the file.
	for _, path := range toRemove {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "remove segment %s", filepath.Base(path))
		}
	}

	return nil
}

// legacyJournalPath is the v1 single-file journal.
func (j *journal) legacyJournalPath() string {
	return filepath.Join(j.dir, "journal.log")
}

// Lifecycle hooks for the legacy journal upgrade. Like the migration
// hooks they make the close-BEFORE-remove ordering regression-testable
// on every platform (Windows refuses to unlink open files).
var (
	closeLegacyJournal  = func(file *os.File) error { return file.Close() }
	removeLegacyJournal = os.Remove
)

// migrateLegacyLog upgrades a v1 journal.log in place: records are
// streamed (bounded memory), applied through the replay callback,
// re-appended into the segmented WAL (one fsync) and only then is the
// old file removed. The descriptor is closed EXPLICITLY (error-aware)
// before every removal: on Windows an os.Remove against a file this
// process still holds open fails with "the process cannot access the
// file because it is being used by another process".
func (j *journal) migrateLegacyLog(replay replayFunc) error {
	path := j.legacyJournalPath()

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "open legacy journal")
	}

	// closeAndRelease releases the legacy descriptor exactly once with
	// its error checked; removal below is only attempted after a
	// successful close. On close failure the legacy file is KEPT (the
	// upgrade re-runs idempotently next boot).
	closed := false

	closeAndRelease := func() error {
		if closed {
			return nil
		}

		closed = true

		if err := closeLegacyJournal(file); err != nil {
			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "close legacy journal before upgrade")
		}

		return nil
	}

	defer func() {
		_ = file.Close() // no-op when closeAndRelease already ran
	}()

	reader := bufio.NewReaderSize(file, 64<<10)

	head := make([]byte, legacyWalHeaderSize)

	if _, err := io.ReadFull(reader, head); err != nil {
		// Empty or unreadably short legacy journal: nothing to keep.
		// The descriptor must be released before the file is removed.
		// A close/removal failure here keeps the file for the next
		// boot: an empty journal can never lose data, so the upgrade
		// is deferred rather than failing the store open.
		if closeErr := closeAndRelease(); closeErr != nil {
			return nil
		}

		if err := removeLegacyJournal(path); err != nil && !os.IsNotExist(err) {
			return nil // deferred: retried on the next open
		}

		return nil
	}

	if string(head[0:4]) != walMagic {
		// Not a v1 journal: leave the unknown file alone.
		return nil
	}

	table := crc32.MakeTable(crc32.IEEE)

	var records []journalRecord

	for {
		record, err := readJournalRecord(reader, table)
		if err != nil {
			if errors.Is(err, io.EOF) || isCorruptTail(err) {
				break // clean end or crash tail
			}

			return firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "wal", "read legacy journal")
		}

		records = append(records, journalRecord{
			Op:    record.Op,
			Key:   record.Key,
			Value: record.Value,
		})
	}

	// Apply through the callback first: an abort (apply error) must
	// not consume the legacy file.
	for i := range records {
		if err := replay(records[i].Op, records[i].Key, records[i].Value, 0); err != nil {
			return err
		}
	}

	if len(records) > 0 {
		// Re-journal durably BEFORE removing the old file.
		if _, err := j.Append(records); err != nil {
			return err
		}
	}

	// Close-before-remove: release the legacy descriptor (error-aware)
	// and only then unlink the file. At this point every record is
	// already durably re-journaled into the segmented WAL, so a
	// close/removal failure can no longer lose data: the upgrade is
	// DEFERRED (retried on the next open) instead of failing the store
	// open — a third-party file lock must never brick the application.
	if err := closeAndRelease(); err != nil {
		return nil
	}

	if err := removeLegacyJournal(path); err != nil && !os.IsNotExist(err) {
		return nil // deferred: retried on the next open
	}

	return nil
}

// Close syncs and closes the active segment.
func (j *journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.file == nil {
		return nil
	}

	if err := j.file.Sync(); err != nil {
		_ = j.file.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "close sync")
	}

	if err := j.file.Close(); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "close")
	}

	j.file = nil

	return nil
}
