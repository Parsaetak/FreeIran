# FreeIran v0.8.0 — Updated Files

Complete replacement for the FreeIran repository at
`3f7f394734f1442d79da4a1c8a69681ebe29bc7e` (v0.7.0, main).

The ZIP contains the full updated source tree. Excluded: `.git`,
`frontend/node_modules`, `frontend/dist` (build output),
`native/build` (build output), `.cores`/`testcores` (CI/local core
installs and fixtures), no secrets, no local runtime data.

---

## 1. Files added

| File | Purpose |
|------|---------|
| `system/resolve_windows.go` | Windows executable resolver: validated `%COMSPEC%` with validated `%SystemRoot%\System32\cmd.exe` fallback; strict validation for explicit paths (P0-B). |
| `system/resolve_other.go` | Unix resolver preserving the exact v0.7 stat semantics. |
| `system/process_windows_test.go` | Windows lifecycle-test fixtures: job member observation, console-attach assertion (`GetConsoleProcessList`), restricted-environment injection (`ERROR_ACCESS_DENIED`). |
| `system/process_unix_test.go` | Unix lifecycle-test fixtures: pidfile-based grandchild observation, zombie-tolerant group-liveness checks. |
| `engine/app/memoryservice.go` | Memory Booster 2.0 — the unified adaptive memory controller wiring `engine/mempressure` + `engine/booster` into the running application (reporters, adaptive actions, hard reactions, metrics mirroring). |
| `engine/app/memoryservice_test.go` | Boot-wiring, pressure→worker-shed integration, Start/Shutdown lifecycle tests. |
| `engine/testqueue/queue_resize_test.go` | Dynamic worker-pool grow/shrink, concurrent-resize racing, O(1) memory estimate, runtime queue-capacity bound. |
| `engine/cache/cache_resize_test.go` | Runtime cache-target shrink/grow/disable test. |
| `frontend/bindings/github.com/Parsaetak/FreeIran/engine/app/coreservice.js` | Bindings for the previously-unregistered CoreService (install/update/rollback/repair/health). |
| `frontend/bindings/github.com/Parsaetak/FreeIran/engine/app/testqueueservice.js` | Bindings for the previously-unregistered TestQueueService (enqueue/cancel/stats/drain). |
| `frontend/bindings/github.com/Parsaetak/FreeIran/engine/app/tunnelservice.js` | Bindings for the previously-unregistered TunnelService (System Proxy / TUN). |
| `cmd/freeiran/frontend/dist/assets/index-BnHoOyMr.js` | Rebuilt embedded frontend bundle (includes the new UI panels). |
| `Updated-Files.md` | This file. |

## 2. Files replaced

| File | Change |
|------|--------|
| `system/job_windows.go` | **P0-A root cause fix**: every Win32 call now validated by its BOOL/HANDLE return value (`SetInformationJobObject` success was previously misread from a stale `GetLastError()`, which is exactly what failed the Windows CI); newJob/assign/terminate/processIDs split; `JobObjectBasicProcessIdList` query; bind-failure test hook; `errors.As`-based restricted detection; correct `PROCESS_SET_QUOTA \| PROCESS_TERMINATE` access mask. |
| `system/process_windows.go` | Three-tier job binding (direct assign → `CREATE_BREAKAWAY_FROM_JOB` retry on `ERROR_ACCESS_DENIED` → supervised Toolhelp32 tree-kill fallback, never silent); `WaitDelay` pipe bound; `processTreeIDs`/`killProcessTree`; `reapDescendants` on every exit path; console-attach verification support. |
| `system/process_unix.go` | Group-wide polite/hard kill, `reapDescendants` group SIGKILL on every exit path, same supervision contract as Windows. |
| `system/system.go` | Deterministic lifecycle state machine (`running/stopping/stopped/exited/cancelled`); synchronizing idempotent `Stop` (concurrent callers observe the same completion); cancellation classification; `ExitCode()`, `JobBound()`, `Diagnostics()`; `Start()` uses the platform resolver; bounded hard-kill deadline. |
| `system/job_other.go` | Unix `jobHandle` contract aligned with the Windows API (assign/terminate/processIDs + test hook). |
| `system/process_test.go` | **P0-C**: genuinely concurrency-safe `syncWriter` (the v0.7 `bytesWriter` had no synchronization); all fixed sleeps removed (deterministic completion via the exited channel); full 15-test lifecycle battery (no-console, stdout+stderr capture, natural exit, cancellation, forced termination, repeated Stop, startup-failure cleanup, job-binding-failure fallback, restricted-retry, concurrent Stop, grandchild-cannot-survive, diagnostics shape). |
| `engine/testqueue/queue.go` | Dynamic worker pool: `SetConcurrency` (immediate growth, prompt idle retirement via resize wake; busy workers finish their task), `SetMaxQueueSize`, `MemoryEstimate` (O(1)), `DesiredWorkers`; `ErrResize` spurious-wake contract for `Dequeue`. |
| `engine/testqueue/queue_bench_test.go` | New benchmarks: cancel-by-ID, duplicate detection (1k/10k/100k), O(1) memory estimate. |
| `engine/cache/cache.go` | `SetMaxEntries` runtime cache-target adjustment (immediate oldest-first eviction; preserves the layer's 0=disabled contract). |
| `engine/app/app.go` | Memory Booster 2.0 wiring (service created in `New`, sampler started in `Start`, stopped first in `Shutdown`). |
| `engine/app/services.go` | `DiagnosticsService.Memory()` — the structured memory report for the UI. |
| `engine/app/v6_services.go` | `ensureQueue` starts the queue at the booster's current adapted settings. |
| `engine/metrics/metrics.go` | Live gauges: `memory_pressure`, `rss_bytes`, `heap_alloc_bytes`, `heap_live_bytes`, `gc_cpu_pct` (mirrored from the controller's samples). |
| `engine/metrics/metrics_test.go` | Gauge test. |
| `cmd/freeiran/main.go` | Registers `CoreService`, `TestQueueService`, `TunnelService` (existed since v0.6 but were never bound — the §11 wiring repair). |
| `frontend/src/services/index.ts` | Exports the three new services; v0.8 view types (`MemorySnapshotView`, `QueueStatsView`). |
| `frontend/src/pages/Diagnostics.tsx` | Memory Booster 2.0 card (pressure state, heap/RSS/GC/usage, cache/queue/pending bytes, adaptive settings) and Test Queue card (depth, workers, throughput, completion classes, cancel-all) — wired to the backend services. |
| `frontend/src/pages/Connection.tsx` | System-integration card: System Proxy / TUN enable/disable with live state, driven by the connected session's local inbound. |
| `frontend/bindings/.../diagnosticsservice.js` | `Memory()` binding added (ByName path). |
| `frontend/package.json`, `VERSION`, `internal/version/version.go` | Version → 0.8.0. |
| `cmd/freeiran/frontend/dist/index.html` | Rebuilt embed entry (new hashed asset). |
| `README.md` | v0.8.0 release section (root cause, supervision strategy, Booster 2.0, wiring repair). |
| `docs/architecture.md` | §7 System engine rewritten for the v0.8 supervision design; v0.8.0 additions section. |
| `docs/performance.md` | §12: measured v0.8 benchmark results + the honest no-new-pools finding. |
| `docs/ci.md` | Windows lifecycle battery documentation. |
| `docs/development.md` | Commands for the new test suites. |
| `docs/security.md` | v0.8 process-supervision security properties. |
| `worklog.md` | v0.8.0 entry. |
| `REPLACEMENT_MANIFEST.md` | Full v0.8.0 manifest (root cause, fixes, verification). |

## 3. Files deleted

| File | Reason |
|------|--------|
| `cmd/freeiran/frontend/dist/assets/index-BoDTHTpt.js` | Superseded by the rebuilt bundle hash (`index-BnHoOyMr.js`). |

## 4. Important architectural changes

1. **Win32 return-value protocol (P0).** Success of BOOL/HANDLE APIs
   is now judged from the return value; `GetLastError()` is a
   diagnostic consulted only on genuine failure. The v0.7 failure
   (`system/start: environment: bind kill-on-close job`) was a
   successful `SetInformationJobObject` misread through a stale
   thread errno left by earlier launch syscalls — present on Actions
   runners, absent on dev desktops.
2. **Three-tier supervision with a non-silent fallback.** Direct job
   assignment (Win8+ nests hierarchies) → breakaway relaunch on
   access-denied → supervised tree-kill fallback. The invariant "a
   protocol core must never become an unmanaged/orphaned process"
   holds on every tier, and degradation is visible through
   `JobBound()`/`Diagnostics()` and logs.
3. **No-descendant-survives on every exit path.** `finish()` reaps
   the whole kill domain (job close / group SIGKILL / tree kill)
   before exit observers wake — covering natural, cancelled and
   stopped exits, not only explicit Stop.
4. **Deterministic lifecycle state machine.** `running → stopping →
   stopped/exited/cancelled`; `Stop` is idempotent and synchronizing;
   cancellation surfaces `context.Canceled`; launch failures never
   produce a `ManagedProcess`.
5. **Memory Booster 2.0 is live.** The previously-unwired mempressure
   + booster libraries now drive real adaptation: 2 s sampling of
   cache/queue/store/heap/RSS/GC; 5 s adaptive ticks applying
   `Queue.SetConcurrency` / `SetMaxQueueSize` / `cache.SetMaxEntries`
   with hard floors/ceilings and hysteresis; High/Critical cache
   shedding + GC hint; everything logged and mirrored into metrics.
6. **Desktop wiring repair.** CoreService, TestQueueService and
   TunnelService were unreachable since v0.6 (never registered);
   v0.8 registers them, ships bindings, and adds the System Proxy /
   TUN, Memory and Test Queue UI surfaces.

## 5. Migration / compatibility notes

- **No v0.7 architecture was reverted.** All changes are extensions
  or in-place fixes; the storage format, source registry, core
  adapter contracts, settings and data layout are untouched. A v0.7
  data directory opens without migration.
- `ManagedProcess` API is additive: `Stop`, `Wait`, `Running`, `PID`
  behave as before (Stop now also synchronizes concurrent callers);
  new: `State`, `ExitCode`, `JobBound`, `Diagnostics`.
- `testqueue.Dequeue` can additionally return the sentinel
  `ErrResize` (spurious wake; retry). Existing callers that treat any
  non-nil error as terminal see no change because workers handle it
  internally.
- `cache.Layer.SetMaxEntries(0)` follows the layer's existing
  0=disabled contract (new puts dropped).
- Frontend bindings for the three repaired services use the stable
  `Call.ByName` path; regenerating with the wails3 generator later
  will switch them to numeric IDs with no behavioural change.
- The memory-controller ceiling defaults (512 MiB heap / 1 GiB RSS
  / 20% GC / …) are unchanged from the v0.7 library defaults.

## 6. Verification commands and results

Run from the extracted tree (Linux, Go 1.26.8 — the CI-pinned
toolchain; fake cores built to `$FREEIRAN_TEST_CORES`):

```text
go build ./engine/... ./system/... ./internal/...                 PASS
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/freeiran   PASS (18.5 MB)
GOOS=windows go vet ./system/... ./engine/... ./internal/...      PASS
GOOS=windows go test -c ./system                                  PASS (compiles)
go vet ./engine/... ./system/... ./internal/...                   PASS
gofmt -l ./engine ./system ./internal ./cmd                       CLEAN

FREEIRAN_TEST_CORES=<fixtures> go test -count=1 \
  ./engine/... ./system/... ./internal/...                        PASS (25 pkgs, exit 0)
FREEIRAN_TEST_CORES=<fixtures> go test -race -count=1 \
  ./engine/... ./system/... ./internal/...                        PASS (exit 0)

make -C native test                                               PASS
go build -tags native_accel ./engine/native                      PASS
CGO_ENABLED=1 go test -tags native_accel -count=1 ./engine/native PASS

FREEIRAN_TEST_V2RAY_BIN=<v2ray 5.53.0>  go test -run \
  TestV2RaySmokeRealBinary ./engine/core/v2ray                    PASS
FREEIRAN_TEST_XRAY_BIN=<xray 26.3.27>   go test -run \
  TestXraySmokeRealBinary ./engine/core/xray                      PASS
FREEIRAN_TEST_SINGBOX_BIN=<sing-box 1.14.0> go test -run \
  TestSingBoxSmokeRealBinary ./engine/core/singbox                PASS

go test -bench=. -benchtime=1x -run=NONE ./engine/chunks \
  ./engine/store ./engine/pipeline ./engine/core \
  ./engine/core/v2ray ./internal/logging                          PASS
CGO_ENABLED=1 go test -tags native_accel -bench=Native \
  -benchtime=1x -run=NONE ./engine/native                         PASS

frontend: npm ci && npm run typecheck && npm test && \
  npm run build && npm run build:embed                            PASS (28 tests)

headless app boot+start+shutdown sequence (the smoke-test path):
engine/app tests incl. TestAppShutdownIsClean,
TestMemoryServiceStartStopLifecycle                               PASS
```

**Notes and limits, stated honestly.**

- `go test ./...` (with `./cmd/...`) cannot build on THIS Linux host:
  the Wails Linux webview needs GTK4/webkitgtk dev packages. This is
  the pre-existing platform constraint (the CI `go` job tests
  `./engine/... ./system/... ./internal/...` on Linux and runs
  `go test ./...` in the `windows` job, where the webview builds
  without GTK). The Windows desktop application cross-builds cleanly
  from this tree, and the Windows test binaries compile.
- The Windows-only runtime behaviour (job binding, no-console,
  console-attach assertion) is verified by construction, by the
  shared lifecycle code paths that pass under `-race` on Linux, and
  by the Windows test binary compiling; the authoritative runtime
  confirmation is the repository's `windows` CI job, which runs the
  identical battery. No Windows host was available to this
  verification environment.
- Benchmark evidence (measured, before/after context) is recorded in
  `docs/performance.md` §12; no unmeasured performance claims are
  made anywhere.
