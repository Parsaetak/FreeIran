# FreeIran Replacement Manifest — v0.3.0

## Package

| Field | Value |
|-------|-------|
| Version | 0.3.0 |
| Base reference | commit `a536191ffdb22c094fb076948d1135219537ef6c` (2026-09-10, "2026.Sep.10") |
| Package | `FreeIran-upgraded-0.3.0.zip` — complete source repository replacement |
| Verified by | Clean extraction into a fresh directory; full build + test matrix re-run from the extracted tree (see "Verification") |
| Excluded from package | `.git`, `frontend/node_modules`, `frontend/dist` (build output), `native/build`, no secrets, no local runtime data |

## Root causes fixed

1. **Windows chunk-file handle leak** (CI run 34432214132, Windows job "Run Go tests").
   `Store.chunkHandle` stored live `*os.File` values in `cache.Layer` (`openLRU`), which has no
   eviction callback: evicted handles were forgotten without being closed, `Store.Close()` never
   closed cached files, and compaction called `os.Remove` on victims still cached open. On
   Windows an open handle blocks deletion → `t.TempDir()` RemoveAll failed with
   "The process cannot access the file because it is being used by another process"
   (chunks/000001.firc, chunks/000011.firc …). Fixed by the resource-ownership rework below.
2. **govulncheck failure** (CI run 34432214136, step "govulncheck (Go)").
   Scanning `./cmd/...` on Linux resolves Wails v3 to GTK4/WebKitGTK-6.0 CGO packages that are
   absent on CI runners ("could not import C"). Fixed with platform-targeted analysis: pure-Go
   packages (`engine/system/internal`) scanned natively; the desktop package scanned with
   `GOOS=windows GOARCH=amd64` (its real deployment target — the Wails Windows webview is pure
   Go). No `|| true`, no muted failures — both scans fail on any detected vulnerability.
3. **Toolchain inconsistency / EOL toolchain with known vulnerabilities.** go1.25.0 stdlib
   carries GO-2026-6218 (net/url) and GO-2026-6090 (crypto/tls), and the 1.25 series is EOL.
   Policy: `go.mod` → `go 1.25.0` + `toolchain go1.26.8`; CI pins `go-version: "1.26.8"` with
   `GOTOOLCHAIN=local`; govulncheck pinned to **v1.8.0** (never `@latest`); documented in
   docs/development.md and docs/security.md.
4. **Storage correctness bugs found during the audit** (all fixed, each with a regression test):
   - flush checkpointed the WAL at `CurrentLSN()` — under concurrent writes this can include
     appended-but-unapplied records, so a crash window silently lost journaled writes;
   - deleting an unflushed override resurrected the old value after the tombstone flushed
     (stale index entry);
   - the v1 journal reader skipped the length prefix for delete records while the writer
     emitted it — replayed deletes always failed their CRC and were silently discarded;
   - compaction's unlock/read/relock window let a concurrent delete be undone by the final
     index swap (resurrection race);
   - `persistMeta` marshalled shared `*chunkMeta` pointers after releasing the read lock
     (data race, exposed by `-race` once flushes went background);
   - `MigrateFromJSON` materialised the entire legacy database in RAM.
5. **CI weaknesses.** `|| true` on the native benchmark step (removed); a suspicious-pattern
   grep chain that could never fail (now a real gate); Node 20 era action majors forcing Node 24
   deprecation warnings (upgraded to checkout@v7, setup-go@v7, setup-node@v7,
   upload-artifact@v7, download-artifact@v8, gitleaks-action@v3, action-gh-release@v3 — all
   Node 24 based).
6. **Goroutine leak** in the desktop state broadcaster (ticker ran forever) — now bound to the
   application context.

## Architecture summary (v0.3.0)

```text
                    FREEIRAN
                       │
              TypeScript / Wails v3
                       │
                     Go 1.26.8
                       │
          ┌────────────┼────────────┐
          │            │            │
       Pipeline      Storage      System
          │            │            │
          │       ┌────┼────┐       │
          │       │    │    │       │
          │      WAL  Mem  Index    │
          │     (segments, frozen   │
          │      tables, bg flush)  │
          │          Chunks         │
          │       (immutable FIRC)  │
          │            │            │
          │     chunkHandleCache    │
          │   (sole fd owner,       │
          │    refcounted pins)     │
          │                         │
          └────────── C++ ──────────┘
              only for hot paths
```

Storage lifecycle (docs/storage-format.md §5 is the full specification):

- **Chunk handles**: `chunkHandleCache` is the sole owner of open chunk descriptors.
  acquire pins (refs++, detached from LRU) → ReadAt (pread, concurrent-safe) → release unpins
  (evictable) → LRU eviction of unpinned entries CLOSES the file; `purge` closes compaction
  victims; `closeAll` closes everything at shutdown.
- **Operation barrier**: every public store operation registers through a gate-guarded
  WaitGroup; `Close()` sets the closed flag, drains in-flight operations, freezes the active
  memtable, stops the flush worker (draining its queue), closes the journal, closes all cached
  handles and returns joined errors. After `Close()` the store owns no file descriptor.
- **Scan barrier**: `Iterate`/`VerifyAll` register as scans; compaction removes victim files
  only after the registry swap and scan drain, and only after purging their handles — no chunk
  file is ever deleted while any descriptor is open.
- **Writes**: WAL append (one write + one fsync per batch) and memtable apply are serialized
  under a write mutex, making the applied-LSN watermark exact; threshold crossing freezes the
  active table into an immutable queue flushed by a single background worker (chunk write →
  CAS index swap → meta persist → WAL checkpoint). Backpressure bounds the queue at 2 pending
  tables; writers throttle beyond that.
- **WAL**: segmented (`wal/seg-XXXXXXXX.wal`), per-segment baseLSN headers, contiguous LSNs,
  continuity validation, checkpoint removes fully-incorporated segments (closed before removal
  — Windows-safe), corrupt-tail discarding, v1 `journal.log` streamed upgrade.
- **Reads**: memtable levels (newest first) → index → pinned offset read; two preads per
  record; 3 allocations per point read (pinned by TestAllocsHotPaths).
- **Iteration**: consistent point-in-time snapshot (40-byte index refs + immutable slice
  headers), sorted, streamed through pinned handles.
- **Compaction**: snapshot refs → read via pins → write replacement chunk → CAS-swap index
  (concurrent deletes can never be resurrected) → persist → wait scans → purge handles →
  remove files.
- **Migration**: token-walking `json.Decoder` streams the legacy file; bounded 1024-record
  batches; per-batch verification; second streaming pass verifies the full disk path; rename
  (never delete) after success; idempotent re-runs.

## Performance (measured, 2 vCPU reference VM, go1.26.8, `-benchtime=1s`)

| Path | v0.2.0 (a536191) | v0.3.0 | Assessment |
|------|------------------|--------|------------|
| Write: Upsert+Flush | 3083 ns/op, 1019 B, 11 allocs | 2329 ns/op, 990 B, 6 allocs | 24% faster, 45% fewer allocs |
| Read: Get (chunk) | ~1045 ns/op, 272 B, 4 allocs | ~1230 ns/op, 164 B, 3 allocs | +18% latency, 40% less memory — the cost of the operation barrier + refcounted pins that make Windows deletion safe |
| Read: Get (memtable) | — | 376 ns/op, 80 B, 1 alloc | new |
| Reopen 20k | 2566 µs | 2373 µs | 7% faster |
| Iterate 20k | 28.5 ms, 60k allocs | 26.1 ms, 100k allocs | 9% faster; more allocs for consistent snapshots |
| Batch write | — | ~805 ns/record (512/batch, 1 fsync) | measured |
| Migration 4k | full-file unmarshal | ~54 ms, bounded memory | streaming |

No unmeasured claims. The read-path regression is documented and deliberate: correctness of
resource ownership is the acceptance criterion. Full tables: docs/performance.md.

## Changed files

- `engine/store/store.go` — rewritten: operation barrier (gate + WaitGroups), write mutex,
  allocation-free key decoding, level-based memtable lookups, pinned read path, snapshot
  iteration, deterministic Close.
- `engine/store/filecache.go` — NEW: refcounted chunk-handle cache (sole fd owner).
- `engine/store/memtable.go` — NEW: memtable + frozen table types and value-ownership rules.
- `engine/store/flush.go` — NEW: background flush worker, backpressure, table flush pipeline.
- `engine/store/wal.go` — rewritten: segmented journal, continuity checks, safe checkpoints,
  v1 journal.log streaming upgrade, uniform delete framing.
- `engine/store/compact.go` — rewritten: CAS-swap compaction, scan barrier, purge-before-remove.
- `engine/store/meta.go` — checkpoint-LSN watermark, orphan-chunk cleanup, race-free persist.
- `engine/store/migrate.go` — rewritten: two-pass streaming migration.
- `engine/store/diagnostics.go` — NEW: Stats/Snapshot, Diagnostics/Inspect, scan-registered VerifyAll.
- `engine/store/store_test.go` — adapted to async flush (drain before on-disk assertions).
- `engine/store/lifecycle_test.go` — NEW: 12 lifecycle/resource-ownership tests.
- `engine/store/wal_test.go` — NEW: 8 journal tests (segments, checkpoint, tails, upgrade).
- `engine/store/store_bench_test.go` — extended: batch/flush/compaction/migration benchmarks,
  allocation pins (TestAllocsHotPaths), hoisted key generation for honest numbers.
- `engine/cache/cache.go` — added `OnEvict` callback (release-on-evict for owned resources).
- `engine/app/app.go` — Context() accessor; shutdown ordering documented.
- `engine/app/services.go` — NEW DiagnosticsService.StoreDiagnostics (store.Inspect surface).
- `engine/chunks/chunks_bench_test.go` — NEW: chunk write/read/verify benchmarks.
- `engine/native/native_bench_test.go` — NEW: hash batch / URL scan benchmarks (Go + native).
- `cmd/freeiran/main.go` — broadcaster goroutine bound to app context (leak fix).
- `.github/workflows/ci.yml` — Node 24 majors, GOTOOLCHAIN=local, Windows runs full
  `go test ./...`, `|| true` removed.
- `.github/workflows/security.yml` — platform-targeted govulncheck (pinned v1.8.0), real
  pattern-scan gate, vet for both scopes.
- `.github/workflows/release.yml` — Node 24 majors, race tests in verify, toolchain pin.
- `go.mod` — `toolchain go1.26.8` directive.
- `internal/version/version.go` — default 0.3.0.
- `VERSION`, `frontend/package.json` — 0.3.0.
- `frontend/bindings/.../diagnosticsservice.js`, `.../store/models.js` — binding for
  StoreDiagnostics + Diagnostics model (generator style, FNV-32a method ID).
- `frontend/src/services/index.ts`, `frontend/src/pages/Diagnostics.tsx` — storage-subsystem
  diagnostics surfaced in the UI.
- `cmd/freeiran/frontend/dist/` — rebuilt production assets (fresh hashes).
- `README.md`, `docs/architecture.md`, `docs/storage-format.md`, `docs/development.md`,
  `docs/performance.md`, `worklog.md` — updated for the v0.3.0 architecture, lifecycle,
  toolchain policy and honest benchmark tables.
- `.gitignore` — coverage/benchmark artifacts.

## Added files

`engine/store/filecache.go`, `engine/store/memtable.go`, `engine/store/flush.go`,
`engine/store/diagnostics.go`, `engine/store/lifecycle_test.go`, `engine/store/wal_test.go`,
`engine/chunks/chunks_bench_test.go`, `engine/native/native_bench_test.go`,
`docs/ci.md`, `docs/security.md`.

## Removed files

`engine/engine.go`, `engine/engine_test.go` (deprecated v0.1 orchestrator),
`engine/database/database.go`, `engine/database/database_test.go` (legacy JSON persistence —
duplicated storage pathway; the format knowledge lives on, tested, in `engine/store/migrate.go`),
`engine/pool/pool.go`, `engine/pool/pool_test.go`, `engine/archive/archive.go`,
`engine/archive/archive_test.go` (unused v0.1 utilities; no non-test users existed).
Migration path for users of the legacy JSON database: unchanged — `MigrateFromJSON`
(streaming, verified, rename-only) is the supported path.

## Migration behavior

- Legacy JSON database (v1): unchanged interface, now streaming and bounded; the file is
  renamed `<original>.migrated` only after a verified two-pass migration; re-runs are no-ops.
- v1 WAL (`wal/journal.log`): streamed, re-applied, re-journaled into the segmented format
  (one fsync) and removed — no pending write is lost by the format upgrade. The v1 reader's
  delete-record framing bug is thereby fixed for upgraded tails.
- Existing chunk files, `store.meta` and `index.bin`: format unchanged; open recovery now also
  removes orphan chunk files (crash leftovers) and enforces WAL segment LSN continuity.

## Toolchain versions

| Tool | Version |
|------|---------|
| Go | 1.26.8 (go.mod toolchain; floor go 1.25) |
| govulncheck | v1.8.0 (pinned in CI) |
| Node.js | 22 (CI) |
| Wails | v3.0.0-beta.19 |
| GitHub Actions | checkout@v7, setup-go@v7, setup-node@v7, upload-artifact@v7, download-artifact@v8, gitleaks-action@v3, action-gh-release@v3 |
| C++ | C++17 (gcc/clang/MSVC) |

## CI changes

- Windows job renamed to "Windows tests and desktop build" and runs `go test -count=1 ./...`
  (full matrix including `engine/store` — nothing skipped, no allowed failures).
- Both Go jobs and release jobs pin go1.26.8 + `GOTOOLCHAIN=local`.
- Security workflow: two platform-targeted govulncheck scans (both hard-failing), gitleaks@v3,
  vet on both scopes, real suspicious-pattern gate.
- All `|| true` occurrences removed.

## Tests executed (from the packaged tree)

- `gofmt -l ./engine ./system ./cmd ./internal` — clean
- `go vet ./engine/... ./system/... ./internal/...` — clean
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./cmd/...` — clean
- `go build ./engine/... ./system/... ./internal/...` — ok
- `go test -count=1 ./engine/... ./system/... ./internal/...` — all pass (incl. store
  lifecycle: open/use/close/delete-directory, repeated open/close, cache eviction closes
  files, compaction+close+delete, concurrent read+close, failed-op+close, operations after
  close, delete-resurrection regression, WAL crash replay with deletes, journal
  segments/checkpoint/corrupt-tail/v1-upgrade, orphan cleanup, consistent iteration
  snapshots, background flush drain)
- `go test -race -count=1 ./engine/... ./system/... ./internal/...` — all pass
- `go test -count=1 -tags native_accel ./engine/native` (CGO) — pass
- `make -C native test` (C++) — pass
- Frontend: `npm ci`, `npm run typecheck`, `npm test` (vitest 7/7), `npm run build:embed`
- Windows desktop build: `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build` — 17.4 MB binary
- Windows cross-compilation of all packages and store/chunks/app/cache/pipeline test binaries
- `govulncheck ./engine/... ./system/... ./internal/...` — No vulnerabilities found
- `GOOS=windows GOARCH=amd64 govulncheck ./cmd/...` — No vulnerabilities found
- Benchmarks: 16 store/chunks/pipeline benchmarks + native benchmarks (Go and native modes)

## Known limitations

- The Windows job executes on GitHub's runner: results were validated here by
  cross-compilation of all packages and test binaries plus the platform-independent lifecycle
  tests, but the actual Windows test execution is observed in CI (as designed).
- `Get` on the chunk path is ~18% slower than v0.2 (measured and documented) — the cost of
  the operation barrier and refcounted handle pins that guarantee Windows-safe deletion.
  Memory per read dropped 40%.
- govulncheck's OSV feed evolves; the weekly scheduled scan may flag future stdlib
  advisories — by design (the toolchain policy documents how to respond).
- The desktop package is analysed for the windows target only; a Linux GUI build (unrelated
  to this project's releases) would require GTK4/WebKitGTK packages.
- Chunk compression (FIRC flag bit 0) remains defined but unwritten; records are stored raw.
