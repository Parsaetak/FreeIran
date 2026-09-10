package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Parsaetak/FreeIran/engine/chunks"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// diskMeta is the on-disk chunk registry (store.meta).
type diskMeta struct {
	Version      int                   `json:"version"`
	Schema       int                   `json:"schema"`
	LSN          uint64                `json:"lsn"`
	NextSequence uint32                `json:"next_sequence"`
	Count        int64                 `json:"count"`
	DeadRecords  int64                 `json:"dead_records"`
	Chunks       map[string]*chunkMeta `json:"chunks"`
}

const (
	metaFormatVersion = 2
	storeSchema       = 1
)

// loadOrRecover restores in-memory state from disk, rebuilding from
// chunk files when the registry or index are missing or mismatched.
func (s *Store) loadOrRecover() error {
	meta, err := s.readMeta()

	switch {
	case err == nil:
		// Load registry.
		for key, chunk := range meta.Chunks {
			var seq uint32

			if _, scanErr := fmt.Sscanf(key, "%d", &seq); scanErr != nil {
				continue
			}

			chunk.Sequence = seq
			s.chunksMap[seq] = chunk
		}

		s.nextSeq = meta.NextSequence
		s.checkpointLSN = meta.LSN
		s.lsn = meta.LSN
		s.deadRecs = meta.DeadRecords

		// Drop registry entries whose chunk files vanished; the store
		// still opens but reports degraded status.
		for seq := range s.chunksMap {
			if _, statErr := os.Stat(s.chunkPath(seq)); statErr != nil {
				delete(s.chunksMap, seq)
				s.status = "degraded"
			}
		}

		// Remove orphan chunk files: crash leftovers of a flush that
		// wrote its chunk but never persisted the registry. Their
		// records are still in the WAL (the checkpoint never advanced)
		// and are replayed right after this call.
		if err := s.removeOrphanChunks(); err != nil {
			return err
		}

	case os.IsNotExist(err) || isEmptyMeta(err):
		// Fresh store or registry loss: rebuild from chunk files.
		if rebuildErr := s.rebuildFromChunks(); rebuildErr != nil {
			return rebuildErr
		}

	default:
		return firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "open", "read store metadata")
	}

	if err := s.loadIndex(); err != nil {
		// Index unusable: rebuild deterministically from chunks.
		if rebuildErr := s.rebuildIndex(); rebuildErr != nil {
			return rebuildErr
		}
	} else if err := s.validateIndex(); err != nil {
		if rebuildErr := s.rebuildIndex(); rebuildErr != nil {
			return rebuildErr
		}
	}

	return nil
}

// removeOrphanChunks deletes chunk files not referenced by the loaded
// registry. Only valid when the registry itself was readable: in the
// rebuild path every chunk file IS the truth and must be kept.
func (s *Store) removeOrphanChunks() error {
	entries, err := os.ReadDir(s.chunkDir)
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "open", "scan chunk directory")
	}

	for _, entry := range entries {
		if entry.IsDir() ||
			!strings.HasSuffix(entry.Name(), chunks.FileExt) {
			continue
		}

		var seq uint32

		if _, scanErr := fmt.Sscanf(entry.Name(), "%d", &seq); scanErr != nil {
			continue
		}

		if _, known := s.chunksMap[seq]; known {
			continue
		}

		path := filepath.Join(s.chunkDir, entry.Name())

		if removeErr := os.Remove(path); removeErr != nil &&
			!os.IsNotExist(removeErr) {
			return firerrors.Wrap(removeErr, firerrors.KindEnvironment,
				Subsystem, "open", "remove orphan chunk %s", entry.Name())
		}
	}

	return nil
}

func isEmptyMeta(err error) bool {
	if firerrors.KindOf(err) == firerrors.KindCorruptData {
		return true
	}

	return strings.Contains(err.Error(), "meta is empty")
}

// readMeta loads store.meta, distinguishing absence from corruption.
func (s *Store) readMeta() (*diskMeta, error) {
	raw, err := os.ReadFile(s.metaPath())
	if err != nil {
		return nil, err
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("store meta is empty")
	}

	var meta diskMeta

	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, firerrors.Wrap(err, firerrors.KindCorruptData,
			Subsystem, "open", "decode store metadata")
	}

	if meta.Version != metaFormatVersion {
		return nil, firerrors.New(firerrors.KindCorruptData,
			Subsystem, "open", "unsupported meta version %d",
			meta.Version)
	}

	return &meta, nil
}

func (s *Store) metaPath() string {
	return filepath.Join(s.root, "store.meta")
}

func (s *Store) indexPath() string {
	return filepath.Join(s.root, "index.bin")
}

// persistMeta atomically replaces store.meta. The chunk registry is
// deep-copied under the read lock: the shared *chunkMeta values are
// mutated by concurrent writers (live counters), so marshalling them
// after unlocking would be a data race.
func (s *Store) persistMeta() error {
	s.mu.RLock()

	meta := diskMeta{
		Version:      metaFormatVersion,
		Schema:       storeSchema,
		LSN:          s.checkpointLSN,
		NextSequence: s.nextSeq,
		Count:        int64(s.derivedCount()),
		DeadRecords:  s.deadRecs,
		Chunks: make(map[string]*chunkMeta,
			len(s.chunksMap)),
	}

	for seq, chunk := range s.chunksMap {
		// Value copy: snapshot the registry entry while the lock is
		// held so the JSON encoder never reads shared mutable state.
		copied := *chunk
		meta.Chunks[fmt.Sprint(seq)] = &copied
	}

	s.mu.RUnlock()

	raw, err := json.Marshal(&meta)
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindFatal,
			Subsystem, "persist", "encode meta")
	}

	return atomicWrite(s.metaPath(), raw)
}

// atomicWrite writes data to a temp file in the same directory, fsyncs
// and renames it over path.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)

	temp, err := os.CreateTemp(dir, ".fir-tmp-*")
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "persist", "create temp")
	}

	tempPath := temp.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(filePerm); err != nil {
		_ = temp.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "persist", "chmod temp")
	}

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "persist", "write temp")
	}

	if err := temp.Sync(); err != nil {
		_ = temp.Close()

		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "persist", "sync temp")
	}

	if err := temp.Close(); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "persist", "close temp")
	}

	if err := os.Rename(tempPath, path); err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "persist", "commit")
	}

	return nil
}

// indexEntry is one persisted index row: 32-byte key + chunk sequence
// + byte offset (40 bytes).
const indexEntrySize = 32 + 4 + 8

const indexMagic = "FIRI"

// persistIndex atomically rewrites index.bin from the in-memory index.
// Layout: "FIRI" | u16 version | u32 count | count × [32B key][u32
// chunk][u64 offset].
func (s *Store) persistIndex() error {
	s.mu.RLock()

	raw := make([]byte, 10+len(s.index)*indexEntrySize)

	copy(raw[0:4], indexMagic)
	raw[4] = 1 // format version

	count := len(s.index)

	raw[6] = byte(count)
	raw[7] = byte(count >> 8)
	raw[8] = byte(count >> 16)
	raw[9] = byte(count >> 24)

	cursor := 10

	for bin, position := range s.index {
		copy(raw[cursor:], bin[:])
		cursor += 32

		raw[cursor] = byte(position.chunk)
		raw[cursor+1] = byte(position.chunk >> 8)
		raw[cursor+2] = byte(position.chunk >> 16)
		raw[cursor+3] = byte(position.chunk >> 24)
		cursor += 4

		offset := uint64(position.offset)

		for bit := 0; bit < 8; bit++ {
			raw[cursor+bit] = byte(offset >> (8 * bit))
		}

		cursor += 8
	}

	s.mu.RUnlock()

	return atomicWrite(s.indexPath(), raw)
}

// loadIndex restores the binary index. Any structural problem returns
// an error so the caller can rebuild from chunks.
func (s *Store) loadIndex() error {
	raw, err := os.ReadFile(s.indexPath())
	if err != nil {
		return err
	}

	if len(raw) < 10 || string(raw[0:4]) != indexMagic {
		return firerrors.New(firerrors.KindCorruptData,
			Subsystem, "open", "bad index header")
	}

	if raw[4] != 1 {
		return firerrors.New(firerrors.KindCorruptData,
			Subsystem, "open", "unsupported index version")
	}

	count := int(uint32(raw[6]) | uint32(raw[7])<<8 |
		uint32(raw[8])<<16 | uint32(raw[9])<<24)

	if len(raw) < 10+count*indexEntrySize {
		return firerrors.New(firerrors.KindCorruptData,
			Subsystem, "open", "truncated index")
	}

	index := make(map[[32]byte]loc, count)

	cursor := 10

	for i := 0; i < count; i++ {
		var bin [32]byte

		copy(bin[:], raw[cursor:])
		cursor += 32

		chunk := uint32(raw[cursor]) | uint32(raw[cursor+1])<<8 |
			uint32(raw[cursor+2])<<16 | uint32(raw[cursor+3])<<24
		cursor += 4

		var offset uint64

		for bit := 0; bit < 8; bit++ {
			offset |= uint64(raw[cursor+bit]) << (8 * bit)
		}
		cursor += 8

		index[bin] = loc{chunk: chunk, offset: int64(offset)}
	}

	s.mu.Lock()
	s.index = index
	s.mu.Unlock()

	return nil
}

// validateIndex ensures every index entry references a known chunk.
func (s *Store) validateIndex() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, position := range s.index {
		if _, ok := s.chunksMap[position.chunk]; !ok {
			return firerrors.New(firerrors.KindCorruptData,
				Subsystem, "open", "index references missing chunk %d",
				position.chunk)
		}
	}

	return nil
}

// rebuildIndex scans every chunk file and reconstructs the index.
func (s *Store) rebuildIndex() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := make(map[[32]byte]loc)

	sequences := make([]uint32, 0, len(s.chunksMap))

	for seq := range s.chunksMap {
		sequences = append(sequences, seq)
	}

	sort.Slice(sequences, func(i, j int) bool {
		return sequences[i] < sequences[j]
	})

	for _, seq := range sequences {
		records, err := chunks.ReadAll(s.chunkPath(seq))
		if err != nil {
			return firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "open", "rebuild index from chunk %d", seq)
		}

		var offset int64 = chunks.HeaderSize

		for _, record := range records {
			if len(record) > 32 {
				var bin [32]byte

				copy(bin[:], record[:32])

				index[bin] = loc{chunk: seq, offset: offset}
			}

			offset += int64(4 + len(record))
		}
	}

	s.index = index

	return nil
}

// rebuildFromChunks reconstructs the registry when store.meta is
// missing or unreadable. It is only correct for a store whose last
// flush completed; remaining WAL records are replayed afterwards.
func (s *Store) rebuildFromChunks() error {
	entries, err := os.ReadDir(s.chunkDir)
	if err != nil {
		return firerrors.Wrap(err, firerrors.KindEnvironment,
			Subsystem, "open", "scan chunk directory")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range entries {
		if entry.IsDir() ||
			!strings.HasSuffix(entry.Name(), chunks.FileExt) {
			continue
		}

		var seq uint32

		if _, scanErr := fmt.Sscanf(entry.Name(), "%d", &seq); scanErr != nil {
			continue
		}

		reader, openErr := chunks.OpenReader(
			filepath.Join(s.chunkDir, entry.Name()))
		if openErr != nil {
			continue // skip unreadable chunk
		}

		header := reader.Header()
		_ = reader.Close()

		s.chunksMap[seq] = &chunkMeta{
			Sequence: seq,
			Records:  int(header.RecordCount),
			Live:     int(header.RecordCount),
			Bytes:    int64(0),
			Checksum: header.Checksum,
		}

		if seq >= s.nextSeq {
			s.nextSeq = seq + 1
		}
	}

	return nil
}
