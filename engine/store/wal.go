package store

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/Parsaetak/FreeIran/engine/chunks"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// Write-ahead journal format (little-endian):
//
//	header: "FIRW" | uint16 version | uint64 baseLSN | uint32 reserved
//	record: uint8 op | uint32 keyLen | key | uint32 valLen | value |
//	        uint32 crc32(op..value)
//
// Records are appended with a single Sync per batch. On startup the
// journal is replayed after the last checkpoint LSN; a truncated tail
// (crash mid-write) is detected by CRC and discarded.
const (
	walMagic   = "FIRW"
	walVersion = uint16(1)

	opUpsert walOp = 1
	opDelete walOp = 2

	walHeaderSize = 4 + 2 + 8 + 4
)

type walOp byte

type journalRecord struct {
	Op    walOp
	Key   [32]byte
	Value []byte
}

// journal is the append-only write-ahead log.
type journal struct {
	mu   sync.Mutex
	path string
	file *os.File
	lsn  uint64 // last assigned LSN
}

// replayFunc receives replayed records; it returns an error to abort.
type replayFunc func(op walOp, key [32]byte, value []byte) error

// openJournal opens (creating if needed) the journal and replays
// records newer than checkpointLSN.
func openJournal(
	dir string,
	checkpointLSN uint64,
	replay replayFunc,
) (*journal, error) {
	path := filepath.Join(dir, "journal.log")

	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "open journal")
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()

		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "stat journal")
	}

	j := &journal{path: path, file: file}

	if info.Size() == 0 {
		if err := j.writeHeader(0); err != nil {
			_ = file.Close()

			return nil, err
		}

		// Position appends AFTER the header; WriteAt does not move
		// the file offset.
		if _, err := file.Seek(walHeaderSize, io.SeekStart); err != nil {
			_ = file.Close()

			return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "seek after header")
		}

		return j, nil
	}

	if err := j.replay(checkpointLSN, replay); err != nil {
		_ = file.Close()

		return nil, err
	}

	// Seek to append position.
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()

		return nil, firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "seek journal end")
	}

	return j, nil
}

func (j *journal) writeHeader(baseLSN uint64) error {
	head := make([]byte, walHeaderSize)

	copy(head[0:4], walMagic)
	binary.LittleEndian.PutUint16(head[4:6], walVersion)
	binary.LittleEndian.PutUint64(head[6:14], baseLSN)

	if _, err := j.file.WriteAt(head, 0); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "write header")
	}

	if err := j.file.Sync(); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "sync header")
	}

	return nil
}

// replay validates and applies journal records, truncating a corrupt
// tail instead of failing the open.
func (j *journal) replay(checkpointLSN uint64, replay replayFunc) error {
	if _, err := j.file.Seek(0, io.SeekStart); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "rewind")
	}

	reader := bufio.NewReaderSize(j.file, 64<<10)

	head := make([]byte, walHeaderSize)

	if _, err := io.ReadFull(reader, head); err != nil {
		// Unreadably short journal: reset it.
		return j.reset()
	}

	if string(head[0:4]) != walMagic {
		return j.reset()
	}

	// LSN accounting: records carry implicit sequential numbers
	// starting at the header base.
	lsn := binary.LittleEndian.Uint64(head[6:14])
	goodOffset := int64(walHeaderSize)

	table := crc32.MakeTable(crc32.IEEE)

	for {
		record, err := readJournalRecord(reader, table)
		if err != nil {
			if err == errJournalCorruptTail {
				break // truncated tail: discard
			}

			if err == io.EOF {
				break
			}

			return err
		}

		lsn++

		if lsn <= checkpointLSN {
			goodOffset += int64(record.encodedSize())

			continue
		}

		if err := replay(walOp(record.Op), record.Key, record.Value); err != nil {
			return err
		}

		goodOffset += int64(record.encodedSize())
	}

	j.lsn = lsn

	// Drop any corrupt tail so future appends stay clean.
	if info, err := j.file.Stat(); err == nil && info.Size() > goodOffset {
		if err := j.file.Truncate(goodOffset); err != nil {
			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "wal", "truncate corrupt tail")
		}
	}

	return nil
}

type rawRecord struct {
	Op    walOp
	Key   [32]byte
	Value []byte
}

func (r rawRecord) encodedSize() int {
	return 1 + 4 + 32 + 4 + len(r.Value) + 4
}

var errJournalCorruptTail = firerrors.New(firerrors.KindCorruptData,
	Subsystem, "wal", "corrupt journal tail")

func readJournalRecord(
	reader *bufio.Reader,
	table *crc32.Table,
) (rawRecord, error) {
	var out rawRecord

	opByte, err := reader.ReadByte()
	if err != nil {
		if err == io.EOF {
			return out, io.EOF
		}

		return out, err
	}

	out.Op = walOp(opByte)

	if out.Op != opUpsert && out.Op != opDelete {
		return out, errJournalCorruptTail
	}

	key, err := readExact(reader, 32)
	if err != nil {
		return out, errJournalCorruptTail
	}

	copy(out.Key[:], key)

	var valueLen uint32

	if out.Op == opUpsert {
		lenBuf, err := readExact(reader, 4)
		if err != nil {
			return out, errJournalCorruptTail
		}

		valueLen = binary.LittleEndian.Uint32(lenBuf)

		if valueLen > chunks.MaxRecordBytes {
			return out, errJournalCorruptTail
		}

		value, err := readExact(reader, int(valueLen))
		if err != nil {
			return out, errJournalCorruptTail
		}

		out.Value = value
	}

	crcBuf, err := readExact(reader, 4)
	if err != nil {
		return out, errJournalCorruptTail
	}

	if binary.LittleEndian.Uint32(crcBuf) != journalCRC(table, out) {
		return out, errJournalCorruptTail
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

// Append journals a batch with one fsync.
func (j *journal) Append(records []journalRecord) error {
	if len(records) == 0 {
		return nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	table := crc32.MakeTable(crc32.IEEE)

	var buffer []byte

	for _, record := range records {
		raw := rawRecord{
			Op:    record.Op,
			Key:   record.Key,
			Value: record.Value,
		}

		encoded := make([]byte, 0, raw.encodedSize())

		encoded = append(encoded, byte(raw.Op))

		// Keys are fixed-size 32 bytes; no length prefix needed.
		encoded = append(encoded, raw.Key[:]...)

		var valLen [4]byte

		binary.LittleEndian.PutUint32(valLen[:], uint32(len(raw.Value)))
		encoded = append(encoded, valLen[:]...)
		encoded = append(encoded, raw.Value...)

		var crcBuf [4]byte

		binary.LittleEndian.PutUint32(crcBuf[:], journalCRC(table, raw))
		encoded = append(encoded, crcBuf[:]...)

		buffer = append(buffer, encoded...)
		j.lsn++
	}

	if _, err := j.file.Write(buffer); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "append")
	}

	if err := j.file.Sync(); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "sync")
	}

	return nil
}

// CurrentLSN returns the last assigned LSN.
func (j *journal) CurrentLSN() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.lsn
}

// Checkpoint marks every journaled record as incorporated: the header
// base LSN advances to the journal's current LSN and the record area
// is truncated. It is safe because the caller persists meta/index
// before checkpointing; a crash in between merely re-applies records
// idempotently on the next open.
func (j *journal) Checkpoint() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if err := j.writeHeader(j.lsn); err != nil {
		return err
	}

	if err := j.file.Truncate(walHeaderSize); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "truncate checkpoint")
	}

	if _, err := j.file.Seek(walHeaderSize, io.SeekStart); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "seek checkpoint")
	}

	return nil
}

// reset reinitialises an unreadable journal.
func (j *journal) reset() error {
	if err := j.file.Truncate(0); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "reset")
	}

	j.lsn = 0

	return j.writeHeader(0)
}

// Close syncs and closes the journal file.
func (j *journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if err := j.file.Sync(); err != nil {
		_ = j.file.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "wal", "close sync")
	}

	return j.file.Close()
}
