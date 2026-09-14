// engine/app/storageoverview.go
//
// v0.9.2 — user-visible storage/memory information (P1) and the
// developer cleanup controls (P1).
//
// StorageOverview answers, in one binding, everything the Diagnostics
// → Storage & workspace card renders: where the workspace is, how much
// each subsystem occupies, what the memory controller is doing and
// what the last cleanup pass reclaimed. Values are live measurements;
// directory sizing walks are entry-bounded so a large workspace can
// never stall the UI thread.
//
// The developer actions (CleanupNow, RemoveStaleRuntime, RebuildIndex,
// OpenWorkspace) are deliberately SEPARATE from the destructive
// surface: none of them can delete user data — they only touch the
// reconstructable/replaceable classes defined in engine/cleanup, or
// rebuild derived state (the index) from authoritative chunks.
package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/Parsaetak/FreeIran/engine/core"
	"github.com/Parsaetak/FreeIran/system"
)

// StorageOverview is the structured workspace/storage/memory report.
type StorageOverview struct {
	// Workspace identity.
	WorkspacePath     string `json:"workspace_path"`
	WorkspaceWritable bool   `json:"workspace_writable"`
	PortableMode      bool   `json:"portable_mode"`

	// Per-subsystem disk usage (bytes).
	DataBytes    int64 `json:"data_bytes"`
	ChunkBytes   int64 `json:"chunk_bytes"`
	WALBytes     int64 `json:"wal_bytes"`
	CacheBytes   int64 `json:"cache_bytes"`
	LogsBytes    int64 `json:"logs_bytes"`
	CoreBytes    int64 `json:"core_bytes"`
	RuntimeBytes int64 `json:"runtime_bytes"`
	TotalBytes   int64 `json:"total_bytes"`

	// Store counters (live snapshot).
	Records      int64   `json:"records"`
	DiskBytes    int64   `json:"disk_bytes"`
	ChunkCount   int64   `json:"chunk_count"`
	GarbageRatio float64 `json:"garbage_ratio"`

	// Memory picture.
	HeapAllocBytes uint64  `json:"heap_alloc_bytes"`
	HeapSysBytes   uint64  `json:"heap_sys_bytes"`
	RSSBytes       uint64  `json:"rss_bytes"`
	PressureState  string  `json:"pressure_state"`
	UsageFraction  float64 `json:"usage_fraction"`

	// Adaptive store limits (current pressure response).
	MemtableRecords int `json:"memtable_records"`
	MemtableBytes   int `json:"memtable_bytes"`
	ChunkTarget     int `json:"chunk_target_bytes"`

	// Cleanup state.
	CleanupPasses    int64             `json:"cleanup_passes"`
	TotalReclaimed   int64             `json:"total_reclaimed_bytes"`
	LastCleanupBytes int64             `json:"last_cleanup_bytes"`
	LastCleanupAt    string            `json:"last_cleanup_at,omitempty"`
	LastCleanupTasks []LastCleanupTask `json:"last_cleanup_tasks,omitempty"`

	// One-time workspace migration status.
	Migration system.WorkspaceStatus `json:"migration"`
}

// LastCleanupTask reports one task of the last cleanup pass.
type LastCleanupTask struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Items  int64  `json:"items"`
	Error  string `json:"error,omitempty"`
	Status string `json:"status"`
}

// Overview assembles the report. Every walk is entry-bounded.
func (s *StorageService) Overview() StorageOverview {
	a := s.app

	layout := a.layout

	writable := system.WorkspaceWritable(layout.Root) == nil

	chunksDir := filepath.Join(layout.Data, "chunks")
	walDir := filepath.Join(layout.Data, "wal")

	stats := a.store.Snapshot()
	mem := a.memory.Snapshot()
	records, memtableBytes, chunkTarget := a.store.PressureLimits()

	overview := StorageOverview{
		WorkspacePath:     layout.Root,
		WorkspaceWritable: writable,
		PortableMode:      system.PortableMode(),

		DataBytes:    dirSizeBounded(layout.Data, 4096),
		ChunkBytes:   dirSizeBounded(chunksDir, 4096),
		WALBytes:     dirSizeBounded(walDir, 512),
		CacheBytes:   dirSizeBounded(layout.Cache, 2048),
		LogsBytes:    dirSizeBounded(layout.Logs, 512),
		CoreBytes:    dirSizeBounded(layout.Cores, 4096),
		RuntimeBytes: dirSizeBounded(layout.Runtime, 1024),

		Records:      stats.Count,
		DiskBytes:    stats.DiskBytes,
		ChunkCount:   int64(stats.ChunkCount),
		GarbageRatio: stats.GarbageRatio,

		HeapAllocBytes: mem.Pressure.HeapAlloc,
		HeapSysBytes:   mem.Pressure.HeapInUse,
		RSSBytes:       mem.Pressure.RSS,
		PressureState:  mem.Pressure.State.String(),
		UsageFraction:  mem.Pressure.UsageFraction,

		MemtableRecords: records,
		MemtableBytes:   memtableBytes,
		ChunkTarget:     chunkTarget,

		Migration: system.LoadWorkspaceStatus(),
	}

	overview.TotalBytes = overview.DataBytes + overview.CacheBytes +
		overview.LogsBytes + overview.CoreBytes + overview.RuntimeBytes

	if result, ok := a.cleanups.LastResult(); ok {
		overview.LastCleanupBytes = result.Bytes
		overview.LastCleanupAt = result.At.Format(time.RFC3339)

		for _, task := range result.Tasks {
			status := "ok"
			if task.Skipped {
				status = "skipped"
			} else if task.Error != "" {
				status = "error"
			}

			overview.LastCleanupTasks = append(overview.LastCleanupTasks, LastCleanupTask{
				Name:   task.Name,
				Bytes:  task.Bytes,
				Items:  task.Items,
				Error:  task.Error,
				Status: status,
			})
		}
	}

	overview.CleanupPasses = a.cleanups.Passes()
	overview.TotalReclaimed = a.cleanups.TotalReclaimed()

	return overview
}

// CleanupNow runs one immediate, bounded cleanup pass and returns the
// result. Safe (non-destructive): only reconstructable/replaceable
// data classes are eligible.
func (s *StorageService) CleanupNow() (CleanupResult, error) {
	s.app.logger.Info("app", "cleanup_requested", "user requested cleanup")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	result := s.app.runCleanupNow(ctx)

	out := CleanupResult{OK: true}
	if result == nil {
		// Rate-limited (a pass ran recently): report the last result.
		out.RateLimited = true

		if last, ok := s.app.cleanups.LastResult(); ok {
			out.Bytes = last.Bytes
		}

		return out, nil
	}

	out.Bytes = result.Bytes
	out.DurationMS = result.DurationMS

	for _, task := range result.Tasks {
		out.Tasks = append(out.Tasks, LastCleanupTask{
			Name:   task.Name,
			Bytes:  task.Bytes,
			Items:  task.Items,
			Error:  task.Error,
			Status: taskStatus(task.Skipped, task.Error),
		})
	}

	return out, nil
}

// CleanupResult is the user-facing cleanup outcome.
type CleanupResult struct {
	OK          bool              `json:"ok"`
	RateLimited bool              `json:"rate_limited,omitempty"`
	Bytes       int64             `json:"bytes"`
	DurationMS  int64             `json:"duration_ms"`
	Tasks       []LastCleanupTask `json:"tasks,omitempty"`
}

func taskStatus(skipped bool, errText string) string {
	if errText != "" {
		return "error"
	}

	if skipped {
		return "skipped"
	}

	return "ok"
}

// RemoveStaleRuntime deletes leftover runtime-config directories now
// (developer action), bypassing the coordinator's rate limit.
func (s *StorageService) RemoveStaleRuntime() (int64, error) {
	s.app.logger.Info("app", "runtime_cleanup", "user requested stale-runtime cleanup")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return core.RemoveStaleRunConfigs(ctx, 0, 0)
}

// RebuildIndex rebuilds the fingerprint index from the chunk files
// and persists it (developer maintenance; derived state only).
func (s *StorageService) RebuildIndex() error {
	s.app.logger.Info("store", "index_rebuild", "user requested index rebuild")

	return s.app.store.RebuildIndex(context.Background())
}

// WorkspacePath returns the workspace root path.
func (s *StorageService) WorkspacePath() string {
	return s.app.layout.Root
}

// OpenWorkspace opens the workspace root in the platform file manager.
func (s *StorageService) OpenWorkspace() error {
	s.app.logger.Info("app", "workspace_opened", "user opened the workspace")

	return system.OpenDirectory(s.app.layout.Root)
}

// dirSizeBounded sums file sizes under path, examining at most
// maxEntries entries so a huge tree cannot stall the caller.
func dirSizeBounded(path string, maxEntries int) int64 {
	var (
		total   int64
		entries int
	)

	_ = filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}

		entries++
		if entries > maxEntries {
			return filepath.SkipAll
		}

		if info, err := entry.Info(); err == nil && entry.Type().IsRegular() {
			total += info.Size()
		}

		return nil
	})

	return total
}
