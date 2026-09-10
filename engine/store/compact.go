package store

import (
	"context"
	"errors"
	"os"
	"sort"
	"time"

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
//
// Safe sequence per group (Windows-compatible):
//
//	snapshot live references (read lock)
//	    → read required records (pinned handles, no lock)
//	    → write replacement chunk (temp + fsync + rename)
//	    → CAS-swap index entries under the write lock
//	    → persist index + meta
//	    → wait for active scans to drain
//	    → purge chunk handles from the file cache (CLOSES the files)
//	    → remove the obsolete chunk files
//
// No chunk file is ever deleted while any descriptor is open, and no
// index swap can resurrect a concurrently deleted key: an entry is
// only redirected when it still points at the exact location the
// compaction snapshot observed.
func (s *Store) Compact(ctx context.Context) error {
	if !s.beginOp() {
		return ErrClosed
	}

	defer s.endOp()

	// Serialize compaction runs: overlapping victim sets would double
	// the rewrite work and race the swaps.
	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	// Drain pending memtables first so victims are all on-disk chunks.
	if err := s.Flush(); err != nil {
		return err
	}

	started := time.Now()

	s.mu.RLock()

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

	s.mu.RUnlock()

	if len(victims) == 0 {
		return nil
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

	s.compactCount.Add(1)
	s.lastCompactMS.Store(time.Since(started).Milliseconds())

	return nil
}

// compactGroup rewrites one group of victim chunks into a single new
// chunk containing only live records, then removes the originals.
func (s *Store) compactGroup(group []uint32) error {
	// Phase A: snapshot live references into the victims.
	s.mu.RLock()

	type ref struct {
		key      [32]byte
		position loc
	}

	victimSet := make(map[uint32]struct{}, len(group))

	for _, seq := range group {
		victimSet[seq] = struct{}{}
	}

	refs := make([]ref, 0, 1024)

	for bin, position := range s.index {
		if _, ok := victimSet[position.chunk]; ok {
			refs = append(refs, ref{key: bin, position: position})
		}
	}

	s.mu.RUnlock()

	sort.Slice(refs, func(i, j int) bool {
		return lessKey(refs[i].key, refs[j].key)
	})

	// Phase B: read the survivor records through pinned handles. The
	// chunk files are immutable, so no store lock is needed; compaction
	// is serialized and the operation barrier keeps Close away.
	records := make([][]byte, 0, len(refs))
	offsets := make([]int64, 0, len(refs))

	var offset int64 = chunks.HeaderSize

	for i := range refs {
		value, err := s.readRecord(refs[i].position)
		if err != nil {
			return firerrors.Wrap(err, firerrors.KindCorruptData,
				Subsystem, "compact", "read live record")
		}

		record := make([]byte, 32+len(value))
		copy(record[:32], refs[i].key[:])
		copy(record[32:], value)

		records = append(records, record)
		offsets = append(offsets, offset)

		offset += int64(4 + len(record))
	}

	// Nothing live in this group: drop the victims without writing.
	if len(records) == 0 {
		return s.retireVictims(group, 0)
	}

	// Phase B.5: write the replacement chunk (atomic temp+fsync+rename).
	seq := s.nextSequence()

	meta, err := chunks.WriteChunk(s.chunkPath(seq), records, 0)
	if err != nil {
		return err
	}

	// Phase C: CAS-swap index entries. Only entries that still point
	// at the exact snapshotted location are redirected; entries moved
	// by concurrent writes or deletes keep their newer state, and the
	// corresponding records simply become dead in the new chunk.
	s.mu.Lock()

	swapped := 0

	for i := range refs {
		current, ok := s.index[refs[i].key]
		if ok && current == refs[i].position {
			s.index[refs[i].key] = loc{chunk: seq, offset: offsets[i]}
			swapped++
		}
	}

	s.chunksMap[seq] = &chunkMeta{
		Sequence: seq,
		Records:  meta.Records,
		Live:     swapped,
		Bytes:    int64(meta.Bytes),
		Checksum: meta.Checksum,
		Created:  time.Now().UTC().UnixMilli(),
	}

	s.mu.Unlock()

	return s.retireVictims(group, len(records)-swapped)
}

// retireVictims removes the victim chunks from the registry and, after
// scans have drained and handles have been closed, deletes their
// files. extraDead counts snapshot records that lost their index entry
// concurrently and are now dead inside a replacement chunk (pass 0
// when no replacement was written).
func (s *Store) retireVictims(group []uint32, extraDead int) error {
	s.mu.Lock()

	var reclaimed int64

	for _, victim := range group {
		if meta, ok := s.chunksMap[victim]; ok {
			reclaimed += int64(meta.Records - meta.Live)
			delete(s.chunksMap, victim)
		}
	}

	s.deadRecs += int64(extraDead) - reclaimed

	s.mu.Unlock()

	// Wait for long-running scans (iteration, verification) that may
	// still read victim chunks through pinned handles. Compaction is
	// registered as a regular operation, and new snapshots taken after
	// the registry swap can no longer reference the victims, so this
	// wait is bounded by the lifetime of already-started scans.
	s.scans.Wait()

	// Close the cached descriptors of every victim BEFORE removing the
	// files: on Windows an open handle blocks deletion.
	for _, victim := range group {
		if err := s.files.purge(victim); err != nil {
			return firerrors.Wrap(err, firerrors.KindEnvironment,
				Subsystem, "compact", "purge chunk handle %d", victim)
		}
	}

	var errs []error

	for _, victim := range group {
		if removeErr := os.Remove(s.chunkPath(victim)); removeErr != nil &&
			!os.IsNotExist(removeErr) {
			errs = append(errs, firerrors.Wrap(removeErr,
				firerrors.KindEnvironment,
				Subsystem, "compact", "remove chunk %d", victim))
		}
	}

	return errors.Join(errs...)
}
