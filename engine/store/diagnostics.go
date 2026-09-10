package store

import (
	"context"
	"os"
	"sort"

	"github.com/Parsaetak/FreeIran/engine/chunks"
	firerrors "github.com/Parsaetak/FreeIran/engine/errors"
)

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
		Count:       int64(s.derivedCount()),
		ChunkCount:  len(s.chunksMap),
		PendingKeys: s.pendingKeysLocked(),
		LSN:         s.checkpointLSN,
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

// pendingKeysLocked counts entries across memtable levels; callers
// hold s.mu.
func (s *Store) pendingKeysLocked() int {
	pending := s.active.len()

	for i := range s.frozen {
		pending += s.frozen[i].table.len()
	}

	if s.flushing != nil {
		pending += s.flushing.table.len()
	}

	return pending
}

// Diagnostics is a deep operational report for maintenance UIs. Every
// value is a real measurement taken from live subsystem state.
type Diagnostics struct {
	Status         string `json:"status"`
	OpenFiles      int    `json:"open_files"`
	OpenFilesMax   int    `json:"open_files_max"`
	CacheOpens     int64  `json:"cache_opens"`
	CacheCloses    int64  `json:"cache_closes"`
	CacheHits      int64  `json:"cache_hits"`
	CacheMisses    int64  `json:"cache_misses"`
	CacheEvictions int64  `json:"cache_evictions"`
	MemtableBytes  int64  `json:"memtable_bytes"`
	PendingTables  int    `json:"pending_tables"`
	PendingKeys    int    `json:"pending_keys"`
	WALSegments    int    `json:"wal_segments"`
	WALBytes       int64  `json:"wal_bytes"`
	Flushes        int64  `json:"flushes"`
	LastFlushMS    int64  `json:"last_flush_ms"`
	Compactions    int64  `json:"compactions"`
	LastCompactMS  int64  `json:"last_compact_ms"`
	FlushError     string `json:"flush_error,omitempty"`
}

// Inspect returns the deep diagnostics snapshot.
func (s *Store) Inspect() Diagnostics {
	s.mu.RLock()

	diag := Diagnostics{
		Status:        s.status,
		PendingTables: s.pendingTables(),
		PendingKeys:   s.pendingKeysLocked(),
		MemtableBytes: s.active.bytes,
		FlushError:    errStringOf(s.flushErr),
	}

	for i := range s.frozen {
		diag.MemtableBytes += s.frozen[i].table.bytes
	}

	if s.flushing != nil {
		diag.MemtableBytes += s.flushing.table.bytes
	}

	s.mu.RUnlock()

	diag.OpenFiles = s.files.len()
	diag.OpenFilesMax = s.opts.OpenFiles

	handleStats := s.files.snapshotStats()
	diag.CacheOpens = handleStats.Opens
	diag.CacheCloses = handleStats.Closes
	diag.CacheHits = handleStats.Hits
	diag.CacheMisses = handleStats.Misses
	diag.CacheEvictions = handleStats.Evictions

	diag.WALSegments = s.wal.segmentCount()
	diag.WALBytes = s.wal.walBytes()

	diag.Flushes = s.flushCount.Load()
	diag.LastFlushMS = s.lastFlushMS.Load()
	diag.Compactions = s.compactCount.Load()
	diag.LastCompactMS = s.lastCompactMS.Load()

	return diag
}

func errStringOf(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// VerifyAll walks every chunk and validates checksums. It is intended
// for background maintenance; progress is reported through the
// optional callback. Chunks that vanish mid-run (concurrent
// compaction) are skipped: their removal already implies their
// records were merged elsewhere.
func (s *Store) VerifyAll(
	ctx context.Context,
	progress func(done, total int),
) error {
	if !s.beginOp() {
		return ErrClosed
	}

	defer s.endOp()

	// Register as a scan so compaction waits for this walk before
	// removing files that a verify reader might hold open (required
	// for Windows deletion semantics).
	s.scans.Add(1)
	defer s.scans.Done()

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

		if s.closed.Load() {
			return ErrClosed
		}

		reader, err := chunks.OpenReader(s.chunkPath(seq))
		if err != nil {
			if os.IsNotExist(err) || firerrors.KindOf(err) == firerrors.KindRecoverable {
				continue // compacted away mid-run
			}

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
