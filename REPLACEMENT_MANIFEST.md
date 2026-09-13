# FreeIran Replacement Manifest — v0.7.0

## Package

| Field | Value |
|-------|-------|
| Version | 0.7.0 |
| Previous version | 0.6.0 |
| Base reference | `d8527cdb912514a844f2e0ab71d353f0b6b5e972` (v0.6.0, the failing commit) |
| Package | `FreeIran-0.7.0.zip` — complete source repository replacement |
| Verified by | Full build + test matrix re-run from the extracted tree (see "Verification performed") |
| Excluded from package | `.git`, `frontend/node_modules`, `frontend/dist` (build output), `cmd/freeiran/frontend/dist` (embed staging, except the placeholder), `native/build`, `.cores` (CI core installs), `testcores/` (CI fixture output), no secrets, no local runtime data, no test-generated binaries |

## Objective

Fix the CI failure at its root, then audit the whole codebase for
concurrency bugs, memory waste, and missing production-readiness
infrastructure — all while preserving the existing FreeIran
architecture and project goals.

## 1. Root cause of the CI failure

**Failing test:** `TestCancelBySource` in `engine/testqueue/queue_test.go`
```
queue_test.go:90: cancelled = 1, want 2
queue_test.go:95: TotalCancelled = 1, want 2
```

**Root cause:** `Queue.New()` treated `Concurrency: 0` as "use
defaults" and replaced the ENTIRE config with `DefaultConfig()`
(Concurrency=4). The test intended `Concurrency: 0` to mean "no
workers" (tasks stay queued), but 4 workers were spawned and dequeued
the src-A tasks into inflight before `CancelBySource` ran.
`CancelBySource` only scanned `pending`, so it found 0–1 src-A tasks
instead of 2.

**Secondary bugs discovered in the same audit:**
1. `Cancel()` for inflight tasks set `task.State = StateCancelled`
   but never cancelled the test's context — the in-flight test ran to
   completion and overwrote the cancelled state.
2. `runTask` called `testCancel()` before `classifyFailure()`, so
   `testCtx.Err()` was always `context.Canceled` and every failure was
   misclassified as `FailureCancelled`.
3. `Stats().ActiveWorkers` formula was `Concurrency - depth + inflight
   - inflight` which simplified to the wrong value.
4. The pending list was an O(n) linear scan per dequeue/cancel.
5. `totalCancelled` could double-count when both `Cancel` and the
   worker's `complete` fired for the same task.

## 2. Queue fixes (`engine/testqueue`)

**Files:** `engine/testqueue/queue.go` (full rewrite), `queue_test.go`
(expanded), `queue_bench_test.go` (new), `queue_mem_test.go` (new).

### Single authoritative state-transition path

`finishTask(task, state, result)` / `finishTaskLocked(...)` is now the
ONLY function that moves a task to a terminal state and updates
counters. It is idempotent (returns false if already terminal), so
workers and cancellation paths can both call it safely without
double-counting. It:
- acquires `q.mu` (or is called by a holder of `q.mu`)
- acquires `task.mu` to check/set state
- calls `task.cancelFunc` (if set) to unblock the in-flight test
- removes the task from `pending` (via `heap.Remove`), `inflight`, and
  `byFingerprint`
- caches the result
- updates `totalCompleted` and the state-specific counter

### Per-task context cancellation

Each `Task` now carries a `cancelFunc context.CancelFunc` (guarded by
`task.mu`). The worker publishes it before calling `tester.Test` and
clears it after. `finishTask` calls it so an in-flight cancellation
unblocks the test promptly. The worker checks for a racing cancellation
at every transition point (before test, after test, before
measurement, after measurement) and returns early if the task was
cancelled, discarding its result.

### Fixed `Concurrency: 0`

`New()` now defaults individual fields instead of replacing the whole
config. `Concurrency: 0` means "no workers" (tasks stay queued) —
exactly what the test intended. `Start()` spawns zero workers in this
mode.

### Fixed failure classification

`runTask` captures `testCtxErr := testCtx.Err()` BEFORE calling
`testCancel()`. `classifyCtxFailure(err, testCtxErr)` uses the captured
error so the real test outcome (network/auth/protocol/config/timeout)
is preserved instead of being masked as "cancelled".

### Heap-based priority queue

`pendingHeap` implements `container/heap.Interface` with O(log n)
Push/Pop/Remove/Fix. Each `Task` carries `heapIdx` (guarded by `q.mu`)
so `Cancel(taskID)` removes a specific task in O(log n). Priority is
`(Priority desc, CreatedAt asc)` — higher priority first, FIFO within
a priority.

### `CancelByFingerprint` (new)

Cancels the task matching a fingerprint (pending or inflight). Returns
1 if cancelled, 0 if not found or already terminal.

### Fixed `Stats().ActiveWorkers`

Now `min(inflight, Concurrency)` — the actual number of workers
running a test.

### Stress / regression tests (17 tests)

- `TestCancelBySource` (original, now passes reliably under `-race -count=20`)
- `TestCancelBySourceQueuedOnly` (50 + 30 tasks, only queued)
- `TestCancelBySourceWhileWorkersDequeue` (200 + 200 tasks, 4 workers)
- `TestCancelBySourceWhileTasksRunning` (4 inflight, verifies per-task ctx cancel)
- `TestCancelByFingerprintDuringExecution` (cancel a 30s test, verify it returns)
- `TestGlobalCancellation` (100 tasks, CancelAll)
- `TestCancellationDuringRetryBackoff` (cancel during 1s backoff)
- `TestCancellationDuringTimeout` (cancel after timeout — must be no-op)
- `TestCancellationDuringShutdown` (Stop must return < 5s)
- `TestConcurrentEnqueueCancel` (8 enqueuers + 4 cancellers, 500 ops each)
- `TestDuplicateSuppressionAndCancel` (dup rejected, original cancellable, re-enqueue succeeds)
- `TestRepeatedCancellation` (10 cancels of same task → TotalCancelled=1)
- `TestReplaceIfHigher` (priority bump via EnqueueReplaceIfHigher)
- `TestStatsActiveWorkers` (4 inflight → ActiveWorkers=4)
- `TestStopWithoutStart` (safe no-op)
- `TestEnqueueAfterStop` (ErrQueueStopped)
- `TestQueueFull` (ErrQueueFull at capacity)
- `TestQueueMemoryGrowth` (50k enqueue + cancel, no leak)
- `TestQueueCancellationCleanup` (100 enqueue/cancel/re-enqueue cycles)

### Benchmarks

- `BenchmarkQueueEnqueue` at n=1k/10k/100k: 504/567/805 ns/task
- `BenchmarkQueueCancelBySource` at n=1k/10k/100k: 483/864/1058 ns/cancel
- `BenchmarkQueueTaskSize`: per-task allocation profile

## 3. Codebase-wide concurrency fixes

### `engine/coremgr` — manifest data race (HIGH)

**Files:** `engine/coremgr/manager.go`, `health.go`, `install.go`,
`update_check.go`.

**Bug:** `manifestOrCreate` read/wrote `m.manifests[name]` and mutated
`Manifest` fields WITHOUT holding `m.mu`, while `Info`/`All` read them
under `m.mu.RLock()`. Confirmed data race on `Manifest.State`,
`.Version`, `.BinaryPath`.

**Fix:** Introduced `manifestOrCreateLocked` (caller MUST hold
`m.mu.Lock`), `snapshotManifest` (value copy under `m.mu.RLock`),
`updateManifest` (applies a mutation fn under `m.mu.Lock` then
persists). All field mutations now go through `updateManifest`. Long
operations (download, smoke test) snapshot the needed fields first,
work outside the lock, then write back via `updateManifest`.

**Regression test:** `TestManagerConcurrentAccess` — 4 readers + 4
writers for 2s under `-race`.

### `engine/app` — lazy-init race (HIGH)

**Files:** `engine/app/app.go`, `v6_services.go`.

**Bug:** `ensureCoreMgr`, `ensureQueue`, `ensureController` did
check-then-set on `app.coreMgr`/`testQueue`/`tunnelCtrl` without
holding any lock. Concurrent UI calls created duplicate managers,
leaking the prior manager's worker goroutines. `SetMode` stopped the
old queue and assigned a new one without synchronizing against
concurrent `ensureQueue`.

**Fix:** Added `initMu sync.Mutex` to `App`. All three `ensure*`
methods acquire `initMu` before the check-then-set. `SetMode` acquires
`initMu`, starts the new queue, swaps the pointer, THEN stops the old
queue. `Shutdown` snapshots the lazy-init subsystems under `initMu`
before stopping them.

### `engine/native` — fallbackHook race (MEDIUM)

**Files:** `engine/native/native.go`.

**Bug:** `fallbackHook` was a package-level `var func()` read by
`useNative()` on every dispatch and written by `SetFallbackHook`
without synchronization. Data race if `SetFallbackHook` is called after
init.

**Fix:** Replaced with `fallbackHookPtr atomic.Pointer[func()]`,
initialized to a no-op sentinel at init. `useNative` loads + derefs;
`SetFallbackHook` stores. Safe for concurrent use.

## 4. C++ memory acceleration layer (`native/` + `engine/native`)

**ABI version bumped 1 → 2** (v1 functions unchanged; v2 adds arena +
batch CRC32).

**Files:** `native/include/freeiran.h`, `native/src/freeiran.cpp`,
`native/tests/test_native.cpp`, `engine/native/native.go`,
`engine/native/bridge_cgo.go`, `engine/native/bridge_stub.go`,
`engine/native/native_test.go`.

### Native arena

New C ABI:
```
fir_arena_create(max_blocks) → fir_arena_t
fir_arena_alloc(arena, size) → void*
fir_arena_reset(arena)
fir_arena_destroy(arena)
fir_arena_stats(arena, *stats) → int32
```

- Bounded growth: `max_blocks` caps total 64 KiB blocks (default 256 =
  16 MiB). Alloc returns NULL at capacity.
- Thread-safe alloc (mutex-protected bump pointer).
- Reset reclaims all blocks for reuse without deallocation.
- Destroy releases all memory.
- 16-byte alignment.
- `-fno-exceptions` compatible (uses `new(std::nothrow)` + `malloc`).

Go bridge (`native.Arena`):
- `NewArena(maxBlocks)`, `Alloc(size)`, `Reset()`, `Destroy()`, `Stats()`
- Transparent Go fallback when native layer not compiled in
  (make([]byte) per alloc, mutex-guarded for concurrent safety)

### Batch CRC-32

New C ABI: `fir_crc32_batch(data, offsets, count, out_crcs)`.
Each string hashed independently from seed 0. Go fallback
(`crc32BatchGo`) uses `crc32.ChecksumIEEE` per string.

### Native tests (C++)

`testArenaBasic`, `testArenaBoundedGrowth`, `testArenaNullSafety`,
`testArenaConcurrentAlloc` (8 threads × 1000 allocs),
`testCrc32Batch`. All pass.

### Go parity tests

`TestCRC32Batch`, `TestArenaBasic`, `TestArenaReset`,
`TestArenaDestroy`, `TestArenaConcurrent` (8 goroutines × 200 allocs
under `-race`), `TestArenaBoundedGrowth`. All pass in both Go-fallback
and native_accel modes.

## 5. Memory pressure controller (`engine/mempressure`)

**Files:** `engine/mempressure/mempressure.go`, `rss_linux.go`,
`rss_other.go`, `mempressure_test.go` (new package).

Tracks: Go heap (HeapAlloc), RSS (Linux /proc/self/statm), GC CPU
fraction, native arena bytes, cache bytes, queue bytes, pending write
bytes, source/parser buffer bytes.

Four-level state with hysteresis (up/down thresholds):
- Normal → Elevated: 0.60 / —
- Elevated → High: 0.75 / 0.45 (down to Normal)
- High → Critical: 0.88 / 0.60 (down to Elevated)
- Critical → —: — / 0.72 (down to High)

Single Sample() can transition multiple levels (e.g. Normal → Critical
on a sudden spike). Subsystems register `Listener` callbacks.

**Tests:** `TestStateString`, `TestDefaultCeiling`,
`TestControllerHysteresis` (full up/down cycle, 6+ state changes),
`TestControllerSetCeiling`, `TestControllerConcurrentSet` (10k
concurrent Set* + 100 Sample under `-race`).

## 6. Memory Booster (`engine/booster`)

**Files:** `engine/booster/booster.go`, `booster_test.go` (new
package).

Adapts: QueueConcurrency, IngestionConcurrency, ParserConcurrency,
BatchSize, QueueDepth, CacheEntries, ChunkFlushBytes.

Inputs: QueueBacklog, CacheHitRate, Throughput, AvgLatencyMS,
ActiveWorkers, CPUPressure (all atomic, settable from any goroutine).

Behavior per pressure state:
- Normal: grow concurrency if backlog deep + CPU < 70%; grow cache if
  hit rate > 80%; grow batch if throughput > 100
- Elevated: hold concurrency; shrink cache if hit rate < 50%; halve
  batch
- High: cut concurrency 25%; halve caches/buffers/chunk-flush
- Critical: everything to floor

NEVER exceeds hard `Limits` floors/ceilings. One step per Tick to
avoid oscillation. `OnChange` listeners fire on actual changes.

**Tests:** `TestDefaultLimits`, `TestNewClampsToLimits`,
`TestTickCriticalHitsFloor`, `TestTickNormalGrowsOnBacklog`,
`TestTickRespectsCeiling`, `TestOnChangeFires`,
`TestInputsAccessors`.

## 7. Store + chunk memory improvements

**Files:** `engine/store/store.go`, `engine/store/compact.go`,
`engine/chunks/chunks.go`.

### `Count()` cache

`store.Count()` caches the live-record count in `atomic.Int64`
(-1 = dirty). Invalidated on every write (UpsertBatch, Delete,
applyReplay, Compact). The UI hot path (`app.State()` → `Count()` on
every poll) now hits the cache instead of allocating a
`map[[32]byte]struct{}` per call.

### Chunk encode buffer pool

`chunks.WriteChunk` draws its payload buffer from a `sync.Pool`,
eliminating the per-flush `make([]byte, payloadSize)` allocation.
Buffers > 64 MiB are not pooled. Thread-safe, shared across flush
workers.

## 8. Documentation

- `docs/performance.md` — added §11 covering mempressure, booster,
  native arena, chunk pooling, Count cache, heap-based queue, and
  benchmark results.
- This manifest.

## Verification performed (all green)

### Go

```
gofmt -l ./engine ./system ./cmd ./internal          — clean
go vet ./engine/... ./system/... ./internal/...       — clean
go build ./engine/... ./system/... ./internal/...     — clean
go test -count=1 ./engine/... ./system/... ./internal/...
                                                       — all pass
go test -race -count=1 ./engine/... ./system/... ./internal/...
                                                       — all pass
go test -race -count=5 ./engine/testqueue              — all pass (5 repeats)
```

### Previously-failing test (repeated under -race)

```
go test -race -count=20 -run TestCancelBySource ./engine/testqueue/
  --- PASS: TestCancelBySource (0.00s)
  ok      github.com/Parsaetak/FreeIran/engine/testqueue   1.012s
```

### Native C++

```
make -C native clean test
  g++ ... -c src/freeiran.cpp -o build/freeiran.o
  g++ ... tests/test_native.cpp build/freeiran.o -o build/test_native
  ./build/test_native
  native: all tests passed
```

### Native-accelerated Go (cgo + race)

```
make -C native
CGO_ENABLED=1 go build -tags native_accel ./engine/native   — clean
CGO_ENABLED=1 go test -tags native_accel -count=1 ./engine/native  — pass
CGO_ENABLED=1 go test -race -tags native_accel -count=1 ./engine/native  — pass
```

### Windows cross-build

```
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/freeiran
                                                       — clean
```

### Benchmarks (smoke)

```
go test -bench=BenchmarkQueueEnqueue -benchtime=1x -run=NONE ./engine/testqueue/
  BenchmarkQueueEnqueue/n=1000-2          504,399 ns/op
  BenchmarkQueueEnqueue/n=10000-2       5,674,305 ns/op
  BenchmarkQueueEnqueue/n=100000-2     80,509,723 ns/op

go test -bench=BenchmarkQueueCancelBySource -benchtime=1x -run=NONE ./engine/testqueue/
  BenchmarkQueueCancelBySource/n=1000-2      483,031 ns/op
  BenchmarkQueueCancelBySource/n=10000-2   8,646,925 ns/op
  BenchmarkQueueCancelBySource/n=100000-2 105,810,328 ns/op

go test -bench=BenchmarkHashBatch -benchtime=1x -run=NONE ./engine/native/
  BenchmarkHashBatch-2        336,302 ns/op (4096 hashes, Go fallback)
```

## New packages

| Package | Purpose | LOC |
|---------|---------|-----|
| `engine/mempressure` | Central memory-pressure controller with hysteresis | ~340 |
| `engine/booster` | Adaptive runtime optimisation controller | ~370 |

## Remaining limitations

- The mempressure controller tracks RSS on Linux only (reads
  `/proc/self/statm`). On macOS/Windows RSS reads as 0; the heap
  fraction from `runtime.MemStats` remains the primary signal.
- The booster is wired as a library; integration into `app.App`
  (actually applying the adjusted concurrency to the live test queue +
  pipeline) is a follow-up. The controllers are tested in isolation.
- The native arena Go-fallback does a `make([]byte)` per alloc (no
  pooling); the native path is the high-throughput one. The fallback
  preserves the API contract.
- The v0.6 frontend UI pages for CoreService / TestQueueService /
  TunnelService remain stubbed (v0.7 milestone was engine-side).
- Wails binding regeneration is still a documented follow-up.

## Final status

**READY** — `TestCancelBySource` passes reliably under `-race
-count=20`. The complete Go/native/Windows verification matrix is
green. The codebase has a single authoritative queue state-transition
path, real in-flight cancellation, a heap-based priority queue, a
memory-pressure controller, an adaptive booster, a native arena, batch
CRC-32, pooled chunk buffers, and a cached store Count(). Four data
races (testqueue, coremgr, app lazy-init, native fallbackHook) are
fixed with regression tests.
