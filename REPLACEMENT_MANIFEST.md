# FreeIran Replacement Manifest — v0.8.0

## Package

| Field | Value |
|-------|-------|
| Version | 0.8.0 |
| Previous version | 0.7.0 |
| Base reference | `3f7f394734f1442d79da4a1c8a69681ebe29bc7e` (v0.7.0) |
| Package | `FreeIran-0.8.0.zip` — complete source repository replacement |
| Verified by | Full build + test matrix re-run from the working tree (see "Verification performed") |
| Excluded from package | `.git`, `frontend/node_modules`, `frontend/dist` (build output), `native/build`, `.cores` (local real-core installs), no secrets, no local runtime data, no test-generated binaries |

## Objective

Fix the Windows CI failure at its root (the `bind kill-on-close job`
class), then deliver the v0.8.0 production-readiness programme:
deterministic process supervision, Memory Booster 2.0 wiring, desktop
service-registration repair, observability and measured benchmark
evidence — all while preserving the v0.7 architecture.

## 1. Root cause of the Windows CI failure

**Failing run:** 34735036772, job `103665043573 — Windows tests and
desktop build`, `go test -count=1 ./...`. Every process-launching test
(`engine/core/{singbox,v2ray,xray}/TestContract/*`, connection tests,
app shutdown, tester core probe) failed with:

```
system/start: environment: bind kill-on-close job
```

**Root cause:** a Win32 return-value protocol bug in
`system/job_windows.go`. `SetInformationJobObject` returns a **BOOL**;
the v0.7 code discarded the BOOL and judged success from the thread's
`GetLastError()` value captured by `LazyProc.Call`. BOOL-returning
APIs do not reset LastError on success, so the value is a **stale
errno left by earlier syscalls** in the launch sequence
(CreateProcessW internals, pipe setup). On GitHub Actions Windows
runners the stale value is nonzero, so a *successful* job
configuration was misread as a failure: the freshly spawned child was
killed and the launch aborted. Desktop machines (whose stale value
happened to be 0) kept passing, which is exactly why v0.7.0 shipped.

**Fix:** every Win32 call in the file is now validated by its actual
return value (BOOL != 0, HANDLE != 0); `GetLastError()` is consulted
only as a diagnostic on genuine failure.

**Secondary Windows bugs fixed in the same audit:**

1. `Start()` validated `spec.Path` with `os.Stat` — CWD-relative, so
   `cmd.exe` (the documented test stand-in) failed to resolve
   anywhere cmd.exe was not in the working directory. Fixed with a
   platform resolver: `%COMSPEC%` (validated to actually name
   cmd.exe) with a validated `%SystemRoot%\System32\cmd.exe`
   fallback; core paths keep strict explicit validation.
2. `bytesWriter` in `process_test.go` claimed thread-safety with a
   placeholder empty `mu struct{}` and no synchronization — a data
   race by construction (exec copies stdout and stderr through two
   concurrent goroutines). Replaced with a genuinely locked
   `syncWriter`; the 50 ms post-exit sleep was removed (the exited
   channel already guarantees writer completion — cmd.Wait joins the
   copy goroutines first).
3. Windows `terminateProcess` killed only the direct pid via
   `os.FindProcess` and never used the job; concurrent `Stop` callers
   did not synchronize; context-cancellation exits were classified as
   errors; descendants surviving a politely-exited child were never
   reaped.

## 2. Process supervision rewrite (`system/`)

**Files:** `job_windows.go` (rewritten), `process_windows.go`
(rewritten), `process_unix.go` (rewritten), `system.go` (rewritten
lifecycle), `job_other.go` (rewritten contract), `resolve_windows.go`
(new), `resolve_other.go` (new).

### Three-tier job binding strategy

1. Direct `AssignProcessToJobObject` (Windows 8+ nests job
   hierarchies — runner jobs are no obstacle).
2. On `ERROR_ACCESS_DENIED` (checked via `errors.As` through the
   structured-error wrapper): kill, respawn once with
   `CREATE_BREAKAWAY_FROM_JOB`, assign again.
3. Supervised fallback: launch succeeds WITHOUT a job; Stop performs
   deterministic Toolhelp32 process-tree termination; the degradation
   is logged, counted, and exposed (`JobBound()`, `Diagnostics()`) —
   never silent, never a failed launch.

### Deterministic lifecycle state machine

`running → stopping → stopped / exited / cancelled` with distinct
classifications for launch failure, natural exit (with exit code),
context cancellation (Wait returns `context.Canceled`), and
environment degradation. `Stop` is idempotent and synchronizing:
concurrent callers all wait for the same termination and observe the
same result. `WaitDelay` (5 s) bounds pipe drain so `cmd.Wait` — and
therefore writer completion — is deterministic even if a stray
descendant briefly holds the pipe.

### No-descendant-survives on every exit path

`finish()` reaps the whole kill domain (job close / group SIGKILL /
tree kill) before signalling exit observers, so grandchildren that
ignore the polite signal cannot outlive the supervisor — including
on natural and cancelled exits, not only explicit Stop.

### Lifecycle battery (15 tests, `-race` clean)

No visible console (behavioural: the child never attaches to the
parent console, `GetConsoleProcessList`), stdout AND stderr capture
through separate writers, natural exit code, cancellation, forced
termination, repeated Stop, concurrent Stop + Wait, startup-failure
cleanup (kind `dependency_unavailable`, no process returned),
job-binding-failure fallback (injected via the test hook),
restricted-environment retry (injected `ERROR_ACCESS_DENIED`), and
grandchild-cannot-survive-supervisor-shutdown (observed through the
job member list on Windows / process-group probe on Unix).

## 3. Memory Booster 2.0 (`engine/app/memoryservice.go`, new)

The v0.7 `engine/mempressure` + `engine/booster` libraries were
standalone and unwired. v0.8 composes them into one adaptive
controller wired to the real subsystems:

- **Reporters (2 s sampler):** cache-layer bytes (source + hot),
  `testqueue.MemoryEstimate()` (new, O(1)), store memtable + WAL
  bytes (`Inspect`), Go heap, RSS, GC CPU fraction; workload inputs
  (backlog, active workers, throughput, cache hit rate) feed the
  booster.
- **Adaptive actions (5 s tick, applied live):**
  `testqueue.Queue.SetConcurrency(n)` (new — immediate growth,
  prompt idle-worker retirement through a resize wake; busy workers
  finish their task; no task dropped), `SetMaxQueueSize(n)` (new),
  `cache.Layer.SetMaxEntries(n)` (new — immediate oldest-first
  eviction). Hard ceilings/floors and one-step-per-tick hysteresis
  are preserved from v0.7.
- **Hard reactions:** High clears the hot cache; Critical clears both
  cache layers and requests GC.
- **Integration:** the sampler starts in `App.Start()`, stops first
  in `Shutdown()`; lazy-created queues start at the adapted settings
  (`ensureQueue` consults the controller).
- **Verification:** `TestMemoryServiceWiredOnBoot`,
  `TestMemoryServicePressureShedsQueueWorkers` (critical injection →
  floor workers on the live queue),
  `TestSetConcurrencyGrowShrink`, `TestSetConcurrencyConcurrent`
  (race-tested), `TestMemoryEstimate`, `TestSetMaxQueueSize`,
  `TestSetMaxEntriesShrinksAndGrows`, `TestMemoryPressureGauge`.

## 4. Desktop wiring repair (§11 audit finding)

The v0.6 `CoreService`, `TestQueueService` and `TunnelService`
existed in the Go backend but were **never registered** with the
Wails runtime and had no bindings — no UI action could ever reach
them (acknowledged as a v0.7 follow-up in the old worklog). v0.8:

- registers all three in `cmd/freeiran/main.go`;
- ships frontend bindings `coreservice.js`, `testqueueservice.js`,
  `tunnelservice.js` (stable `Call.ByName` path; regenerate with the
  wails3 generator on a GUI toolchain host when convenient) plus
  `DiagnosticsService.Memory`;
- adds UI: system-integration card (System Proxy / TUN enable /
  disable, live state) on the Connection page; Memory Booster 2.0
  card (pressure state, heap/RSS/GC/usage, cache/queue/pending
  bytes, adaptive settings) and Test Queue card (depth, workers,
  throughput, completion classes, cancel-all) on the Diagnostics
  page — every button reaches the backend through the bound service.

## 5. Observability

`metrics.Registry` gains live gauges — `memory_pressure`,
`rss_bytes`, `heap_alloc_bytes`, `heap_live_bytes`, `gc_cpu_pct` —
mirrored from the controller's samples so the engine metrics
snapshot and the memory diagnostics page agree on one
classification. The queue sampler already maintained
`SetQueueDepth` / `SetActiveWorkers`.

## 6. Benchmarks and evidence (§13)

New: `BenchmarkQueueCancelByID`, `BenchmarkQueueDuplicateDetection`
(1k/10k/100k), `BenchmarkQueueMemoryEstimate` (O(1), ~550 ns, 0
allocs). Measured results are recorded in
`docs/performance.md §12` — including the honest negative finding:
the allocation audit justified NO new `sync.Pool` use beyond the
v0.7 chunk-encode pool; v0.8 eliminated the unsampled-controller
waste instead of micro-optimizing allocations.

## Verification performed

From the working tree (Linux, Go 1.26.8, matching the CI matrix):

- `go build ./engine/... ./system/... ./internal/...` — pass
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/freeiran` — pass
- `GOOS=windows go vet ./system/... ./engine/... ./internal/...` — pass
- `GOOS=windows go test -c ./system` (Windows test binary compiles) — pass
- `go vet ./engine/... ./system/... ./internal/...` — pass
- `gofmt -l ./engine ./system ./internal ./cmd` — clean
- `go test -count=1 ./engine/... ./system/... ./internal/...` with
  fake cores (`FREEIRAN_TEST_CORES`) — **all packages pass**
- `go test -race -count=1 ./engine/... ./system/... ./internal/...` —
  all pass (process battery repeated ×2 for stability)
- Native C++: `make -C native test` — pass;
  `go build -tags native_accel ./engine/native` — pass;
  `CGO_ENABLED=1 go test -tags native_accel ./engine/native` — pass
- Real cores (pinned, SHA-256-verified):
  `TestV2RaySmokeRealBinary`, `TestXraySmokeRealBinary`,
  `TestSingBoxSmokeRealBinary` — all pass
- Benchmark smoke: `go test -bench=. -benchtime=1x -run=NONE
  ./engine/chunks ./engine/store ./engine/pipeline ./engine/core
  ./engine/core/v2ray ./internal/logging` + native bench — pass
- Frontend: `npm run typecheck`, `npm test` (28 tests),
  `npm run build`, `npm run build:embed` — pass

**Windows runtime note:** the Windows-specific runtime behaviour is
verified by construction (the battery is compiled for Windows and
executed by the CI `windows` job), by the identical shared lifecycle
code paths that pass under `-race` on Linux, and by
cross-compilation. The actual Windows job must confirm at CI time —
no Windows host was available to this verification environment.
