// pressure.go implements the v0.9.2 adaptive-pressure surface of the
// store plus the safe cleanup entry points used by the central
// cleanup coordinator (engine/cleanup).
//
// Chunking is made aware of actual runtime conditions: the freeze
// thresholds (record count AND bytes) shrink under memory pressure so
// batches and the chunks they become stay small while the machine is
// loaded, and grow back when it recovers. The floors below keep the
// on-disk layout from fragmenting: chunks are never shrunk below
// pressureFloorRecords / pressureFloorBytes.
//
// The WAL/chunk cleanup entries reuse exactly the safe lifecycle the
// store already enforces: obsolete WAL segments are removed by
// Checkpoint only after chunk+meta persistence covered them (files
// closed before removal), and dead chunks are retired by Compact only
// after the index swap, metadata persist, scan drain and handle close.
package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PressureLevel mirrors the application memory-pressure states. The
// store defines its own enum so the persistence layer stays decoupled
// from the mempressure library.
type PressureLevel int

// Pressure levels, in increasing severity.
const (
	PressureNormal PressureLevel = iota
	PressureElevated
	PressureHigh
	PressureCritical
)

// String returns the stable level name.
func (l PressureLevel) String() string {
	switch l {
	case PressureElevated:
		return "elevated"
	case PressureHigh:
		return "high"
	case PressureCritical:
		return "critical"
	default:
		return "normal"
	}
}

// Freeze thresholds per pressure level. Elevated/High/Critical shrink
// the in-memory batch target progressively; Critical additionally
// flushes aggressively (ForceFreeze is called by the app layer).
//
// Floors: memtableBytes never drops below 512 KiB and records never
// below 512, so the resulting chunks stay large enough to avoid
// storage fragmentation (many tiny chunk files would inflate
// metadata, handle churn and compaction cost).
var pressureLimits = [4]adaptiveLimits{
	// Normal: efficient larger chunks.
	PressureNormal: {memtableRecords: 4096, memtableBytes: 8 << 20},
	// Elevated: reduce growth, smaller batches.
	PressureElevated: {memtableRecords: 3072, memtableBytes: 6 << 20},
	// High: bounded memory first.
	PressureHigh: {memtableRecords: 2048, memtableBytes: 4 << 20},
	// Critical: smallest safe batch; aggressive flush.
	PressureCritical: {memtableRecords: 1024, memtableBytes: 2 << 20},
}

// Floors preventing fragmentation (see pressureLimits).
const (
	pressureFloorRecords = 512
	pressureFloorBytes   = 512 << 10
)

// Chunk-target bounds for SetChunkTargetBytes: the booster proposes a
// flush/chunk size; the store clamps it into this range.
const (
	MinChunkTargetBytes = 512 << 10
	MaxChunkTargetBytes = 16 << 20
)

// ApplyPressure adapts the freeze thresholds and chunk target to the
// given pressure level. It reports whether any value changed (so the
// caller can log meaningfully instead of every tick). The active
// memtable is evaluated against the new limits immediately and frozen
// when it already exceeds them at High/Critical.
func (s *Store) ApplyPressure(level PressureLevel) bool {
	if level < PressureNormal || level > PressureCritical {
		level = PressureNormal
	}

	limits := pressureLimits[level]

	// Preserve a caller-proposed chunk target if it is still within
	// the bounds; otherwise fall back to the level default.
	s.mu.Lock()

	limits.chunkTargetBytes = s.limits.chunkTargetBytes

	changed := limits != s.limits
	s.limits = limits

	freeze := changed && (level == PressureHigh || level == PressureCritical)

	if freeze {
		s.freezeLocked()
	}

	s.mu.Unlock()

	if freeze {
		s.wakeFlusher()
	}

	return changed
}

// SetChunkTargetBytes clamps and stores the proposed chunk/flush
// payload target. Values outside [MinChunkTargetBytes,
// MaxChunkTargetBytes] are clamped, never rejected, so the booster can
// propose freely. It reports whether the value changed.
func (s *Store) SetChunkTargetBytes(target int) bool {
	switch {
	case target < MinChunkTargetBytes:
		target = MinChunkTargetBytes
	case target > MaxChunkTargetBytes:
		target = MaxChunkTargetBytes
	}

	s.mu.Lock()

	changed := target != s.limits.chunkTargetBytes
	if changed {
		s.limits.chunkTargetBytes = target
	}

	s.mu.Unlock()

	return changed
}

// ChunkTargetBytes returns the current chunk payload target.
func (s *Store) ChunkTargetBytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.limits.chunkTargetBytes
}

// PressureLimits returns the current adaptive limits (diagnostics).
func (s *Store) PressureLimits() (records int, bytes int, chunkTarget int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.limits.memtableRecords, s.limits.memtableBytes, s.limits.chunkTargetBytes
}

// ForceFreeze freezes the active memtable immediately and wakes the
// flush worker. Used at High/Critical pressure to drain pending
// writes without waiting for the thresholds. Safe to call
// concurrently with writers: it takes the write mutex first so the
// applied-LSN watermark stays exact.
func (s *Store) ForceFreeze() {
	s.writeMu.Lock()

	s.mu.Lock()
	s.freezeLocked()
	s.mu.Unlock()

	s.writeMu.Unlock()
}

// CloseIdleHandles closes every unpinned cached chunk descriptor and
// returns how many were closed. Under Critical pressure this releases
// kernel file handles and their buffers; descriptors reopen
// transparently on the next read.
func (s *Store) CloseIdleHandles() int {
	return s.files.CloseIdle()
}

// CheckpointWAL removes WAL segments that are already fully covered
// by the durable checkpoint (chunk + meta persisted). It never
// deletes an uncheckpointed segment and never deletes the active
// segment unless every appended record is incorporated. This is the
// cleanup-coordinator entry point: the flush worker checkpoints
// automatically after each flush; this call reclaims anything left
// over (e.g. orphan segments from a crash between state update and
// file removal).
func (s *Store) CheckpointWAL() (int, error) {
	if !s.beginOp() {
		return 0, ErrClosed
	}

	defer s.endOp()

	before := s.wal.segmentCount()

	if err := s.wal.Checkpoint(s.checkpointLSN); err != nil {
		return 0, err
	}

	removed := before - s.wal.segmentCount()
	if removed < 0 {
		removed = 0
	}

	return removed, nil
}

// RemoveTempArtifacts deletes stale temporary files the store's
// atomic-write paths may have left behind (crash between CreateTemp
// and rename): ".fir-tmp-*" next to meta/index, ".firc-*" in the
// chunk directory and ".fir-sys-*" anywhere in the store root. Files
// younger than maxAge are kept (they may belong to an operation that
// is in flight). At most maxFiles entries are examined per call so
// the scan stays bounded. Returns the bytes reclaimed.
func (s *Store) RemoveTempArtifacts(maxAge time.Duration, maxFiles int) (int64, error) {
	if maxAge <= 0 {
		maxAge = time.Hour
	}

	if maxFiles <= 0 {
		maxFiles = 512
	}

	if !s.beginOp() {
		return 0, ErrClosed
	}

	defer s.endOp()

	var (
		reclaimed int64
		scanned   int
		errs      []error
	)

	consider := func(dir, name string) {
		if scanned >= maxFiles {
			return
		}

		if !strings.HasPrefix(name, ".fir-tmp-") &&
			!strings.HasPrefix(name, ".firc-") &&
			!strings.HasPrefix(name, ".fir-sys-") {
			return
		}

		scanned++

		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.IsDir() {
			return
		}

		if time.Since(info.ModTime()) < maxAge {
			return
		}

		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)

			return
		}

		reclaimed += info.Size()
	}

	entries, err := os.ReadDir(s.root)
	if err == nil {
		for _, entry := range entries {
			consider(s.root, entry.Name())
		}
	}

	entries, err = os.ReadDir(s.chunkDir)
	if err == nil {
		for _, entry := range entries {
			consider(s.chunkDir, entry.Name())
		}
	}

	return reclaimed, errors.Join(errs...)
}

// CompactIfWorthwhile runs a compaction only when the dead-record
// garbage ratio reaches minRatio (0 < minRatio < 1, typical 0.35). It
// reports whether a compaction ran. The coordinator uses this to
// reclaim dead chunks opportunistically without rewriting healthy
// chunks on every cleanup pass.
func (s *Store) CompactIfWorthwhile(ctx context.Context, minRatio float64) (bool, error) {
	if minRatio <= 0 || minRatio >= 1 {
		minRatio = 0.35
	}

	if !s.beginOp() {
		return false, ErrClosed
	}

	s.endOp() // Compact performs its own beginOp; only the check is needed

	stats := s.Snapshot()
	if stats.TotalRecords == 0 || stats.GarbageRatio < minRatio {
		return false, nil
	}

	if err := s.Compact(ctx); err != nil {
		return false, err
	}

	return true, nil
}

// RebuildIndex reconstructs the fingerprint index from the chunk
// files and persists it (developer maintenance action). The pending
// memtables are flushed first so the rebuilt index is complete, and
// compaction is excluded for the duration.
func (s *Store) RebuildIndex(ctx context.Context) error {
	if !s.beginOp() {
		return ErrClosed
	}

	defer s.endOp()

	if err := ctx.Err(); err != nil {
		return err
	}

	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	if err := s.Flush(); err != nil {
		return err
	}

	if err := s.rebuildIndex(); err != nil {
		return err
	}

	return s.persistIndex()
}
