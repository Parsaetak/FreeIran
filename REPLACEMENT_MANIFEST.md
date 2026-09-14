# FreeIran Replacement Manifest — v0.9.2

## Package

| Field | Value |
|-------|-------|
| Version | 0.9.2 |
| Previous version | 0.9.1 |
| Base reference | `79d763c` (v0.9.1, main) |
| Package | `FreeIran-0.9.2.zip` — complete source repository replacement |
| Verified by | Full build + test + race matrix re-run from the working tree (see "Verification performed") |
| Excluded from package | `.git`, `frontend/node_modules`, `native/build`, `.cores` (local real-core installs), `FreeIran-v*-windows-amd64` / `FreeIran-v*-linux-amd64` (packaging outputs), no secrets, no local runtime data |

## Objective

The maintenance/performance update, implemented in the specified
priority order on top of the existing architecture (no store rewrite,
no booster rewrite, no frontend rewrite):

1. **Unified Workspace Root (P0)** — FreeIran now owns its entire
   workspace. `system/workspace.go` introduces `WorkspaceRoot()`
   (default: the directory containing the FreeIran executable;
   `FREEIRAN_HOME` overrides), `WorkspaceLayout()`,
   `EnsureWorkspace()` with directory creation, a writable probe and
   actionable startup diagnostics (native error dialog + boot-error
   file when read-only). The split per-user roots
   (`%APPDATA%` / `%LOCALAPPDATA%` / XDG data/cache) are removed;
   `paths_windows.go` / `paths_unix.go` deleted. Every subsystem —
   store, logs, cache, core manager, runtime temp configs,
   diagnostics, source metadata, deployment metadata, settings —
   resolves through the single resolver. `DirNames` gained
   `Runtime` (`<root>/runtime`).
2. **Consistent data paths (P0)** — per-launch core runtime configs
   (`engine/core/runtime_root.go`) and core-manager validation/smoke
   directories moved out of the system temp root into
   `<workspace>/runtime`; the wintun DLL directory now follows the
   workspace (`<workspace>/cores/wintun`) instead of hard-coded
   `%APPDATA%\FreeIran`. Temp files are cleaned after use; an
   age-bounded sweeper reclaims crash leftovers.
3. **Storage reading/writing preserved (P0)** — metadata-first
   startup, chunked immutable records, WAL, memtables, background
   flush, compaction, lazy indexed point reads, bounded handles: all
   unchanged. Added: cached live-record count already avoided per-poll
   map rebuilds (v0.9.1); batch writes remain batched with explicit
   backpressure; fsync amortisation unchanged.
4. **Pressure-adaptive chunking (P0)** —
   `engine/store/pressure.go`: freeze thresholds shrink per pressure
   level (4096 rec/8 MiB → 1024 rec/2 MiB) with hard floors (512 rec /
   512 KiB) so storage never fragments; Critical additionally freezes
   and flushes aggressively; the booster's chunk-flush proposal is
   clamped into `[512 KiB, 16 MiB]` and finally wired to the store.
5. **Data lifecycle / cleanup (P0)** — `engine/cleanup` is the single
   coordinator: permanent user data is never touched; reconstructable
   (runtime dirs, store atomic-write temps, staging) and replaceable
   historical data (checkpointed WAL segments, dead chunk files via
   compaction, stale staging, failed downloads) are reclaimed by
   age/size/reference with bounded, cancellable, rate-limited passes
   that report reclaimed bytes. WAL segments are removed immediately
   after being covered by the checkpoint; chunks retire only after
   index swap → meta persist → scan drain → handle close (existing,
   tested lifecycle).
6. **Memory Booster lifecycle control (P0)** — pressure transitions
   now trigger safe reclamation (Elevated/High/Critical ladder:
   smaller batches, cold-cache eviction, aggressive flush, idle-handle
   release, GC) without ever deleting user configurations. Logging
   stays transition-based with reason (heap/cache/queue %) and
   per-action lines including reclaimed-byte totals.
7. **CMD/console never appears (P0)** — audit completed:
   `queryCoreVersion` (the last unprotected launch path), `net
   session` elevation probe, Explorer launches and contract smoke
   launches all apply hidden-console attributes; `system.Start`
   already enforced `CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP |
   DETACHED_PROCESS` in one place. The application executable is a
   verified WINDOWS_GUI-subsystem binary (`-H=windowsgui` everywhere;
   `TestWindowsGUISubsystem` parses the built PE header in CI and
   release — a console-subsystem artifact fails the pipeline). Close
   remains orphan-free (kill-on-close job objects / process groups).
8. **Workspace migration (P1)** — one-time discovery of legacy
   per-user data, verified copy, status recorded in
   `config/workspace.json`, source preserved, deterministic gating
   (never duplicates the dataset; `FREEIRAN_SKIP_MIGRATION=1` disables
   for automated deployments).
9. **Diagnostics & developer controls (P1)** — new Storage &
   workspace overview (workspace path, per-subsystem sizes, heap/RSS/
   pressure state, adaptive store limits, cleanup history, migration
   status) plus Cleanup now / Refresh / Open workspace / data / logs
   actions; developer settings gain workspace cleanup, stale-runtime
   removal and index rebuild behind a confirmation dialog. Safe
   cleanup and destructive actions remain clearly separated (there is
   no one-click destructive reset).
10. **UI consistency pass (P1)** — the new cards reuse the existing
    card/stat-grid/toolbar primitives; no overlapping controls; empty
    and loading states for the new surface; workspace state explained
    without exposing internal counters.
11. **Version + docs (P1)** — 0.9.2 across VERSION / version.go /
    winres.json / package.json / regenerated `.syso`; new
    `docs/workspace.md` documenting the workspace model, migration,
    lifecycle and pressure response; `docs/development.md` now marks
    `-H=windowsgui` as mandatory.

## Files added

- `system/workspace.go` — workspace root resolver/layout/validation
- `system/workspace_migrate.go` — legacy detection + verified migration
- `system/workspace_test.go` — override/ensure/detect/migrate tests
- `engine/cleanup/cleanup.go`, `engine/cleanup/cleanup_test.go` — coordinator
- `engine/store/pressure.go`, `engine/store/pressure_test.go` — adaptive pressure + cleanup entry points
- `engine/core/runtime_root.go`, `engine/core/runtime_root_test.go` — workspace runtime root
- `engine/coremgr/cleanup_test.go` — stale-staging cleanup test
- `engine/core/contract/procattr_windows.go`, `procattr_other.go` — hidden console for contract smoke
- `engine/app/cleanupservice.go` — task registration + cadence + logging
- `engine/app/storageoverview.go` — Overview/CleanupNow/RemoveStaleRuntime/RebuildIndex/OpenWorkspace services
- `cmd/freeiran/gui_check_windows_test.go` — PE subsystem regression check
- `docs/workspace.md` — workspace/lifecycle documentation

## Files replaced

Key: `system/system.go` (Layout+Runtime, concealed version probe),
`system/portable.go`, `system/process_windows.go` / `process_unix.go`
(`concealChild`), `system/open_windows.go`; `engine/app/app.go`
(workspace boot + migration + cleanup wiring), `engine/app/memoryservice.go`
(lifecycle integration + non-spammy logging), `engine/app/developerservice.go`,
`engine/core/runconfig.go`, `engine/coremgr/manager.go`, `engine/coremgr/health.go`,
`engine/tunnel/tun_windows.go`, `engine/store/store.go`, `engine/store/filecache.go`,
`cmd/freeiran/main.go` (workspace + boot failure dialog), frontend
`services/index.ts`, `pages/Diagnostics.tsx`, `pages/Settings.tsx`,
binding `storageservice.js`; `.github/workflows/ci.yml` + `release.yml`
(`-H=windowsgui` + subsystem regression check); docs/manifests/worklog;
`VERSION`, `internal/version/version.go`, `build/winres.json`,
`frontend/package.json`, regenerated `cmd/freeiran/*.syso`.

## Verification performed

- `gofmt -l .` clean (all packages).
- `go vet ./...` clean (Windows target — covers every build-tagged file).
- `go test -count=1 ./engine/... ./system/... ./internal/...` — all green
  (incl. new: workspace resolution/migration, store pressure/cleanup,
  cleanup coordinator, stale staging, runtime root).
- `go test -race -count=1` green for store / cleanup / app / system / core.
- Store benchmarks re-run (upsert, batch upsert, flush, get, memtable
  get, iterate, compaction, reopen, migration).
- Windows amd64 + 386 cross-builds green; final
  `-trimpath -ldflags "-s -w -H=windowsgui"` binary verified:
  PE subsystem = 2 (WINDOWS_GUI ⇒ no CMD on double-click), `.syso`
  resources (icon + 0.9.2 version info + manifest) linked, ~14.5 MB.
- Frontend `tsc --noEmit && vite build` green; 31/31 vitest tests green.
- No visible console at startup: guaranteed by the PE subsystem check
  in CI/release plus hidden-console attributes on every audited
  subprocess launch path.
