package store

import (
	"sort"
	"time"

	"github.com/Parsaetak/FreeIran/engine/chunks"
)

// Background flush pipeline.
//
//	freeze (writer)  →  frozen queue  →  flush worker  →  chunk file
//	                                                   →  index swap
//	                                                   →  meta persist
//	                                                   →  WAL checkpoint
//
// Writers never wait for chunk generation: after the WAL fsync
// returns, the batch is durable and the memtable freeze hands the
// heavy work to the worker. Backpressure bounds memory: when
// maxFrozen tables are pending, writers block until room drains.
//
// Failure handling: a failed flush requeues its table at the head and
// is retried with a delay. The data stays readable through the
// memtable levels (the table is still queued) and stays durable in
// the WAL (the checkpoint never advanced past it), so a retry can
// never lose or duplicate records.

// startFlushWorker launches the single background flush goroutine.
func (s *Store) startFlushWorker() {
	s.flushWG.Add(1)

	go func() {
		defer s.flushWG.Done()

		for {
			select {
			case <-s.flushStop:
				s.drainForShutdown()
				return
			case <-s.flushWake:
			}

			for s.flushOne() {
			}
		}
	}()
}

// flushOne pops the oldest frozen table and persists it. It reports
// whether work was performed, so callers can drain the queue.
func (s *Store) flushOne() bool {
	s.mu.Lock()

	if s.flushing != nil || len(s.frozen) == 0 {
		s.mu.Unlock()

		return false
	}

	table := s.frozen[0]
	s.frozen = s.frozen[1:]
	s.flushing = table

	s.mu.Unlock()

	err := s.flushTable(table)

	s.mu.Lock()
	s.flushing = nil

	if err != nil {
		if s.stopping.Load() {
			// Shutdown: drop the table. Its records remain durable in
			// the WAL and are replayed on the next open.
			s.flushErr = err
		} else {
			// Requeue at the head so reads still see the data and the
			// next retry rewrites it.
			s.frozen = append([]*frozenTable{table}, s.frozen...)
			s.flushErr = err

			if s.status == "ready" {
				s.status = "degraded"
			}
		}
	} else {
		s.flushErr = nil
		s.flushCount.Add(1)
	}

	s.flushCond.Broadcast()
	s.mu.Unlock()

	if err != nil && !s.stopping.Load() {
		time.AfterFunc(flushRetryDelay, s.wakeFlusher)
	}

	return true
}

// drainForShutdown flushes every remaining table once. Failures drop
// the table (records stay durable in the WAL) and are reported through
// the sticky flush error surfaced by Close.
func (s *Store) drainForShutdown() {
	for {
		s.mu.Lock()

		if s.flushing != nil || len(s.frozen) == 0 {
			s.mu.Unlock()

			return
		}

		table := s.frozen[0]
		s.frozen = s.frozen[1:]
		s.flushing = table

		s.mu.Unlock()

		err := s.flushTable(table)

		s.mu.Lock()
		s.flushing = nil

		if err != nil {
			s.flushErr = err
		} else {
			s.flushCount.Add(1)
		}

		s.flushCond.Broadcast()
		s.mu.Unlock()
	}
}

// flushTable writes one frozen table as a single new chunk, updates
// the index and registry, persists both, and checkpoints the WAL to
// the table's LSN watermark. It performs its I/O WITHOUT holding the
// store lock, so reads and writes continue throughout.
func (s *Store) flushTable(table *frozenTable) error {
	started := time.Now()

	// Deterministic key order keeps chunk layout reproducible.
	keys := make([][32]byte, 0, len(table.table.entries))

	for bin := range table.table.entries {
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
		value := table.table.entries[bin]

		if value == nil {
			continue // tombstones are dropped at flush time
		}

		record := make([]byte, 32+len(value))
		copy(record[:32], bin[:])
		copy(record[32:], value)

		records = append(records, record)
		offsets = append(offsets, offset)
		liveKeys = append(liveKeys, bin)

		offset += int64(4 + len(record))
	}

	if len(records) > 0 {
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
			Created:  time.Now().UTC().UnixMilli(),
		}

		for i, bin := range liveKeys {
			s.index[bin] = loc{chunk: seq, offset: offsets[i]}
		}

		s.checkpointLSN = table.lsn
		s.mu.Unlock()

		if err := s.persistIndex(); err != nil {
			return err
		}

		if err := s.persistMeta(); err != nil {
			return err
		}
	} else {
		// Nothing live: just advance the durable watermark.
		s.mu.Lock()
		s.checkpointLSN = table.lsn
		s.mu.Unlock()

		if err := s.persistMeta(); err != nil {
			return err
		}
	}

	// The chunk and metadata are durable: journal records up to the
	// table's watermark can be discarded safely. A crash between the
	// persists and this checkpoint merely re-applies idempotently.
	if err := s.wal.Checkpoint(table.lsn); err != nil {
		return err
	}

	s.lastFlushMS.Store(time.Since(started).Milliseconds())

	return nil
}

// nextSequence allocates the next chunk sequence number.
func (s *Store) nextSequence() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextSeq++

	return s.nextSeq
}
