package store

import (
	"context"
	"os"
	"sort"

	"github.com/Parsaetak/FreeIran/engine/chunks"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

// compactionRatio is the maximum fraction of dead records tolerated
// inside a chunk before it is rewritten.
const compactionRatio = 0.5

// Compact rewrites chunks whose live-record ratio dropped below the
// threshold, merging survivors and physically removing dead records
// and tombstones. It is deterministic: records are rewritten in key
// order.
func (s *Store) Compact(ctx context.Context) error {
	if s.closed.Load() {
		return ErrClosed
	}

	if err := s.Flush(); err != nil {
		return err
	}

	s.mu.Lock()

	victims := make([]uint32, 0, len(s.chunksMap))

	for seq, meta := range s.chunksMap {
		if meta.Records == 0 {
			victims = append(victims, seq)

			continue
		}

		if float64(meta.Records-meta.Live)/float64(meta.Records) >
			compactionRatio {
			victims = append(victims, seq)
		}
	}

	// Deterministic victim order keeps the output stable.
	sort.Slice(victims, func(i, j int) bool {
		return victims[i] < victims[j]
	})

	// Merge small victims together; rewrite large ones alone.
	const mergeTarget = 8

	groups := make([][]uint32, 0)

	for len(victims) > 0 {
		size := mergeTarget
		if len(victims) < size {
			size = len(victims)
		}

		groups = append(groups, victims[:size])
		victims = victims[size:]
	}

	s.mu.Unlock()

	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := s.compactGroup(group); err != nil {
			return err
		}
	}

	if len(groups) > 0 {
		if err := s.persistIndex(); err != nil {
			return err
		}

		if err := s.persistMeta(); err != nil {
			return err
		}
	}

	return nil
}

// compactGroup rewrites one group of victim chunks into a single new
// chunk containing only live records, then removes the originals.
func (s *Store) compactGroup(group []uint32) error {
	s.mu.Lock()

	// Collect keys still pointing into the victim group.
	type ref struct {
		key      [32]byte
		position loc
	}

	refs := make([]ref, 0, 1024)

	victimSet := make(map[uint32]struct{}, len(group))

	for _, seq := range group {
		victimSet[seq] = struct{}{}
	}

	for bin, position := range s.index {
		if _, ok := victimSet[position.chunk]; ok {
			refs = append(refs, ref{key: bin, position: position})
		}
	}

	sort.Slice(refs, func(i, j int) bool {
		return lessKey(refs[i].key, refs[j].key)
	})

	records := make([][]byte, 0, len(refs))
	offsets := make([]int64, 0, len(refs))

	var offset int64 = chunks.HeaderSize

	for _, r := range refs {
		s.mu.Unlock()

		value, err := s.readRecord(r.position)
		if err != nil {
			return firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "compact", "read live record")
		}

		record := make([]byte, 32+len(value))

		copy(record[:32], r.key[:])
		copy(record[32:], value)

		records = append(records, record)
		offsets = append(offsets, offset)

		offset += int64(4 + len(record))

		s.mu.Lock()
	}

	s.mu.Unlock()

	// Nothing live in this group: drop the victims, write nothing.
	if len(records) == 0 {
		s.mu.Lock()

		for _, victim := range group {
			if victimMeta, ok := s.chunksMap[victim]; ok {
				s.deadRecs -= int64(victimMeta.Records - victimMeta.Live)
			}

			delete(s.chunksMap, victim)

			if removeErr := os.Remove(s.chunkPath(victim)); removeErr != nil &&
				!os.IsNotExist(removeErr) {
				s.mu.Unlock()

				return firerrors.Wrap(removeErr, firerrors.KindEnvironment,
					Subsystem, "compact", "remove chunk %d", victim)
			}
		}

		s.mu.Unlock()

		return nil
	}

	// Write the compacted chunk.
	seq := s.nextSequence()

	meta, err := chunks.WriteChunk(s.chunkPath(seq), records, 0)
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
	}

	for i, r := range refs {
		s.index[r.key] = loc{chunk: seq, offset: offsets[i]}
	}

	// Remove the old chunks and their dead-record accounting.
	for _, victim := range group {
		if victimMeta, ok := s.chunksMap[victim]; ok {
			s.deadRecs -= int64(victimMeta.Records - victimMeta.Live)
		}

		delete(s.chunksMap, victim)

		if removeErr := os.Remove(s.chunkPath(victim)); removeErr != nil &&
			!os.IsNotExist(removeErr) {
			s.mu.Unlock()

			return firerrors.Wrap(removeErr, firerrors.KindEnvironment,
				Subsystem, "compact", "remove chunk %d", victim)
		}
	}

	s.mu.Unlock()

	return nil
}

// Stats is a point-in-time store report for the UI and diagnostics.
type Stats struct {
	Status       string  `json:"status"`
	Count        int64   `json:"count"`
	ChunkCount   int     `json:"chunk_count"`
	TotalRecords int     `json:"total_records"`
	DeadRecords  int64   `json:"dead_records"`
	DiskBytes    int64   `json:"disk_bytes"`
	PendingKeys  int     `json:"pending_keys"`
	GarbageRatio float64 `json:"garbage_ratio"`
	LSN          uint64  `json:"lsn"`
}

// Snapshot returns the current store statistics.
func (s *Store) Snapshot() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := Stats{
		Status:      s.status,
		Count:       int64(derivedCountLocked(s)),
		ChunkCount:  len(s.chunksMap),
		PendingKeys: len(s.memtable),
		LSN:         s.lsn,
	}

	for _, meta := range s.chunksMap {
		stats.TotalRecords += meta.Records
		stats.DiskBytes += meta.Bytes
	}

	stats.DeadRecords = s.deadRecs

	if stats.TotalRecords > 0 {
		stats.GarbageRatio = float64(s.deadRecs) /
			float64(stats.TotalRecords)
	}

	return stats
}

// VerifyAll walks every chunk and validates checksums. It is intended
// for background maintenance; progress is reported through the
// optional callback.
func (s *Store) VerifyAll(
	ctx context.Context,
	progress func(done, total int),
) error {
	s.mu.RLock()

	sequences := make([]uint32, 0, len(s.chunksMap))

	for seq := range s.chunksMap {
		sequences = append(sequences, seq)
	}

	s.mu.RUnlock()

	sort.Slice(sequences, func(i, j int) bool {
		return sequences[i] < sequences[j]
	})

	for i, seq := range sequences {
		if err := ctx.Err(); err != nil {
			return err
		}

		reader, err := chunks.OpenReader(s.chunkPath(seq))
		if err != nil {
			return firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "verify", "chunk %d", seq)
		}

		verifyErr := reader.Verify()
		_ = reader.Close()

		if verifyErr != nil {
			return firerrors.Wrap(verifyErr, firerrors.KindCorruptData,
				Subsystem, "verify", "chunk %d failed integrity", seq)
		}

		if progress != nil {
			progress(i+1, len(sequences))
		}
	}

	return nil
}
