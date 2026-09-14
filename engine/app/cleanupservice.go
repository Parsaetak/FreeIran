// engine/app/cleanupservice.go
//
// v0.9.2 — the composition root's registration of the central cleanup
// coordinator (engine/cleanup) plus the opportunistic maintenance
// loop.
//
// Data-lifecycle map (what MAY be reclaimed, and by which task):
//
//	runtime   <workspace>/runtime/freeiran-* config dirs   reconstructable
//	store-tmp store atomic-write temp files                reconstructable
//	wal       WAL segments covered by the checkpoint       replaceable
//	chunks    dead chunk files (via compaction)            replaceable
//	staging   core-install staging / failed downloads      reconstructable
//
// NEVER touched automatically: config/ (sources, settings, workspace
// status), data/store.meta, data/index.bin, live chunks, cores/*/bin,
// cores/*/manifest.json and rollback binaries. The compaction lifecycle
// itself guarantees a chunk still holding the live authoritative
// record is never deleted (index swap → meta persist → scan drain →
// handle close → remove).
package app

import (
	"context"
	"time"

	"github.com/Parsaetak/FreeIran/engine/cleanup"
	"github.com/Parsaetak/FreeIran/engine/core"
)

// cleanup cadence: an opportunistic pass every 15 minutes, bounded to
// a few seconds of work, skipping itself when pressure cleanup ran
// recently (MinInterval in the coordinator).
const cleanupInterval = 15 * time.Minute

// registerCleanupTasks wires the coordinator to the live subsystems.
// Every task is bounded: age filters, entry caps, ctx cancellation.
func (a *App) registerCleanupTasks() {
	a.cleanups = cleanup.New(cleanup.Options{
		MinInterval: 30 * time.Second,
		MaxDuration: 45 * time.Second,
	})

	// Reconstructable runtime files: per-launch config dirs under
	// <workspace>/runtime older than 6h (a live session creates fresh
	// dirs and removes them on Stop; leftovers are crash debris).
	a.cleanups.Register(cleanup.Task{
		Name: "runtime",
		Run: func(ctx context.Context) (int64, int64, error) {
			bytes, err := coreCleanupRuntime(ctx)
			return bytes, 0, err
		},
	})

	// Stale atomic-write temp files inside the store root/chunk dir.
	a.cleanups.Register(cleanup.Task{
		Name: "store-tmp",
		Run: func(ctx context.Context) (int64, int64, error) {
			bytes, err := a.store.RemoveTempArtifacts(time.Hour, 512)
			return bytes, 0, err
		},
	})

	// WAL segments fully covered by the durable checkpoint. The flush
	// worker removes covered segments immediately after each flush;
	// this pass reclaims crash-orphaned segments. Never touches an
	// uncheckpointed or the active segment (enforced in the store).
	a.cleanups.Register(cleanup.Task{
		Name: "wal",
		Run: func(ctx context.Context) (int64, int64, error) {
			before := a.store.Inspect().WALBytes

			segments, err := a.store.CheckpointWAL()
			if err != nil {
				return 0, 0, err
			}

			reclaimed := before - a.store.Inspect().WALBytes
			if reclaimed < 0 {
				reclaimed = 0
			}

			return reclaimed, int64(segments), nil
		},
	})

	// Dead chunk files: compact only when the garbage ratio justifies
	// the rewrite. The store's own lifecycle keeps every live record.
	a.cleanups.Register(cleanup.Task{
		Name: "chunks",
		Run: func(ctx context.Context) (int64, int64, error) {
			before := a.store.Snapshot().DiskBytes

			ran, err := a.store.CompactIfWorthwhile(ctx, 0.35)
			if err != nil || !ran {
				return 0, 0, err
			}

			reclaimed := before - a.store.Snapshot().DiskBytes
			if reclaimed < 0 {
				reclaimed = 0
			}

			return reclaimed, 0, nil
		},
	})

	// Stale core-install staging trees and abandoned failed-download
	// artifacts (age ≥ 7d). Live bin/, manifest and rollback binaries
	// are never touched.
	if a.coreMgr != nil {
		a.cleanups.Register(cleanup.Task{
			Name: "staging",
			Run: func(ctx context.Context) (int64, int64, error) {
				bytes, err := a.coreMgr.CleanStaleStaging(ctx, 7*24*time.Hour)
				return bytes, 0, err
			},
		})
	}
}

// coreCleanupRuntime delegates to the core package's stale-run-config
// sweeper (bounded scan of the configured runtime root).
func coreCleanupRuntime(ctx context.Context) (int64, error) {
	return core.RemoveStaleRunConfigs(ctx, 6*time.Hour, 256)
}

// cleanupLoop runs the opportunistic maintenance cadence until the
// application context is cancelled. Results are logged so reclamation
// is always explainable.
func (a *App) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return

		case <-ticker.C:
			if result, ran := a.cleanups.Run(a.ctx); ran && result != nil {
				a.logCleanupResult("maintenance", result)
			}
		}
	}
}

// runCleanupNow executes an immediate bounded pass (pressure reaction
// or the user's "Cleanup now" action).
func (a *App) runCleanupNow(ctx context.Context) *cleanup.Result {
	result, ran := a.cleanups.Run(ctx)
	if ran && result != nil {
		a.logCleanupResult("requested", result)
	}

	return result
}

// logCleanupResult renders one pass into the structured log.
func (a *App) logCleanupResult(trigger string, result *cleanup.Result) {
	if a.logger == nil || result == nil {
		return
	}

	a.logger.Info("app", "cleanup",
		"cleanup (%s): reclaimed %d bytes in %d ms",
		trigger, result.Bytes, result.DurationMS)

	for _, task := range result.Tasks {
		if task.Skipped {
			continue
		}

		if task.Error != "" {
			a.logger.Warn("app", "cleanup_task",
				"cleanup task %s failed: %s", task.Name, task.Error)

			continue
		}

		a.logger.Info("app", "cleanup_task",
			"cleanup task %s: %d bytes, %d items (%d ms)",
			task.Name, task.Bytes, task.Items, task.DurationMS)
	}
}
