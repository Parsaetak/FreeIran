# FreeIran Replacement Manifest — v0.2.0

Deliverable: complete repository replacement (`FreeIran-upgraded-0.2.0.zip`)
for https://github.com/Parsaetak/FreeIran.

- **Version:** 0.2.0 (`VERSION`, `internal/version`, `frontend/package.json`)
- **Baseline:** commit `6b9aff9` ("Format worklog.md with code block syntax")
- **Date:** 2026-09-10

---

## Architecture Summary

| Layer | Implementation |
|-------|----------------|
| Desktop shell | Wails v3.0.0-beta.19 (`cmd/freeiran`), Go 1.25.0, no localhost HTTP API |
| UI | TypeScript + React 18 + Vite + zustand + @tanstack/react-virtual (`frontend/`) |
| Backend contract | Generated Wails bindings (`frontend/bindings`, 5 services / 20 methods / 13 models) + `freeiran:state` events |
| Orchestration | `engine/app` — staged startup, service surface, source persistence, scheduler |
| Persistence | `engine/store` — chunk files (FIRC v1), binary fingerprint index, WAL with LSN checkpointing, incremental batched writes, atomic commits, compaction, deterministic recovery |
| Chunking | `engine/chunks` — deterministic FIRC format, records never split, CRC-32 integrity |
| Ingestion | `engine/pipeline` — bounded worker stages (fetch/parse/dedup+persist), backpressure, cancellation, content-hash change detection |
| Caching | `engine/cache` — bounded LRU layers (source / hot configs / chunk handles / dedup shards) with TTL, generations, hit-miss stats |
| Native | `native/` C++17 C-ABI layer (batch FNV-1a 64, CRC-32, URL scanner) + `engine/native` bridge with bit-identical Go fallbacks (`-tags native_accel`, `FREEIRAN_NATIVE=off` runtime override) |
| System | `system/` — dir layout, atomic writes, core process lifecycle, core discovery, reachability, platform files |
| Observability | `engine/metrics` — local-only counters surfaced via DiagnosticsService |
| Errors | `engine/errors` — 8 classified kinds with subsystem/operation context |
| CI/CD | `.github/workflows/{ci,release,security}.yml` |
| Versioning | `VERSION` file + `internal/version` + ldflags injection |

Preserved from v0.1 and evolved: `engine/config` (universal model,
SHA-256 fingerprints, validation), `engine/parser` (multi-format parser,
now with reject stats), `engine/source` (fetcher, defaults), `engine/core`
(registry), `engine/pool`, `engine/tester` (probe interface, now with a
concrete TCP probe), `engine/archive`, `engine/database` (legacy JSON
reader for migration), `engine/engine.go` (deprecated sequential
orchestrator, kept for v0.1 library API compatibility).

## Changed Files

| Path | Change |
|------|--------|
| `README.md` | rewritten for the delivered architecture |
| `.gitignore` | replaced (Python template → project-specific) |
| `go.mod` / `go.sum` | Go 1.25.0, wails v3.0.0-beta.19 added |
| `worklog.md` | appended v0.2.0 section (history preserved) |
| `engine/engine.go` | deprecation notice; logic otherwise preserved |
| `engine/parser/parser.go` | added `ParseStats`/`ParseDetailed` (observability) |
| `engine/source/*` | unchanged except User-Agent now uses `internal/version` |

## New Files

```text
VERSION
internal/version/version.go
engine/errors/          errors.go, errors_test.go
engine/metrics/         metrics.go, metrics_test.go
engine/chunks/          chunks.go, chunks_test.go
engine/store/           store.go, wal.go, meta.go, compact.go, migrate.go,
                        store_test.go, store_bench_test.go, helpers_test.go
engine/cache/           cache.go, cache_test.go
engine/native/          native.go, bridge_stub.go, bridge_cgo.go, native_test.go
engine/pipeline/        pipeline.go, pipeline_test.go (+ benchmark)
engine/scheduler/       scheduler.go, scheduler_test.go
engine/tester/          tcp_probe.go, tcp_probe_test.go
engine/app/             app.go, services.go, app_test.go, helpers_test.go
cmd/freeiran/           main.go (+ embedded frontend/dist)
system/                 system.go, filesystem.go, process_unix.go,
                        process_windows.go, paths_unix.go, paths_windows.go,
                        platform_stub.go, system_test.go
native/                 include/freeiran.h, src/freeiran.cpp,
                        tests/test_native.cpp, Makefile, CMakeLists.txt
frontend/               package.json, tsconfig.json, vite.config.ts,
                        index.html, src/** (components, pages, state,
                        services, types, utilities, workers, styles),
                        bindings/** (generated), dist/** (built)
.github/workflows/      ci.yml, release.yml, security.yml
docs/                   architecture.md, storage-format.md,
                        performance.md, development.md
REPLACEMENT_MANIFEST.md
```

## Removed Files

None deleted. The v0.1 tree is fully preserved; obsolete mechanisms
(full-JSON persistence as the primary store) are superseded by the new
layers, with `engine/database` retained for migration. The old
`.gitignore` content is replaced as listed above.

## Migration Requirements (existing user data)

1. Locate the legacy JSON database (v0.1 `engine/database` format:
   `{"version":1,"entries":{...}}`).
2. In-app: *Diagnostics → Migrate* with the file path, or call
   `StorageService.MigrateLegacy(path)`.
3. The store imports all records (fingerprints preserved), verifies each
   migrated key is readable, then renames the legacy file to
   `<original>.migrated`.
4. Idempotent: re-running is a no-op; failure never loses data.

## Build Commands

```bash
# Frontend
cd frontend && npm ci && npm run build && cd ..

# Engine tests (all platforms)
go test ./engine/... ./system/... ./internal/...

# Desktop binary (windows/amd64 — primary target)
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
  -trimpath -ldflags "-s -w \
  -X github.com/Parsaetak/FreeIran/internal/version.Version=0.2.0" \
  -o FreeIran-windows-amd64.exe ./cmd/freeiran

# Optional native acceleration
make -C native && CGO_ENABLED=1 go build -tags native_accel ./cmd/freeiran
```

Note: building the GUI for Linux locally requires GTK4/WebKitGTK dev
packages; the engine packages and the windows/amd64 cross-build do not.

## Tests Executed (2026-09-10, Go 1.25.0 linux/amd64)

- `go build ./engine/... ./system/... ./internal/...` — clean
- `go vet ./engine/... ./system/... ./internal/...` — clean
- `gofmt -l` — clean
- `go test ./engine/... ./system/... ./internal/...` — **19 packages PASS**
  (includes end-to-end: source → ingestion → chunking → dedup →
  persist → read → UI service layer; migration; crash-recovery;
  compaction; scheduler; caches; native bridge)
- `go test -race` — cache, store, scheduler, app, pipeline PASS
- `make -C native test` — C++ tests PASS
- `go test -tags native_accel ./engine/native` — PASS (cross-language)
- `frontend`: `tsc --noEmit` clean, vitest 7/7 PASS, production build OK
- `GOOS=windows` full desktop build (embedded frontend): OK, 15.8 MB

## Benchmark Results (linux/amd64, sandbox VM, 3 iterations)

| Benchmark | Result |
|-----------|--------|
| ChunkerGrouping (100k record sizes) | ~0.84 ms/op |
| ChunkWriteRead (2,000 records) | ~0.72 ms/op |
| StoreUpsertFlush (incl. WAL fsync) | ~142 µs/op |
| StoreGet (index + offset read) | ~9.4 µs/op |
| **StoreReopen (20,000 records)** | **~2.6 ms/op** |
| StoreIterate (20,000 records) | ~25.7 ms/op |
| **PipelineRun (5,000 configs end-to-end)** | **~50 ms/op** |

Interpretation and baseline comparison: `docs/performance.md`.

## Known Limitations

1. **Protocol execution not yet wired** — Xray/sing-box/WireGuard runtimes
   are managed (discovery, process lifecycle) but no core is bundled or
   launched by the desktop app yet; testing is limited to TCP
   reachability probes (planned v0.3/v0.4 per roadmap).
2. **GUI runtime verified for windows/amd64** — Linux GUI builds compile
   in CI via cross-compilation validation; a native Linux GUI run needs
   GTK4/WebKitGTK packages (documented).
3. **Archive still JSON** — `engine/archive` keeps its compressed
   JSON format; moving it onto the chunked store is future work.
4. **Native acceleration is opt-in** — default builds are pure Go by
   design; accelerated builds require a C++ toolchain at compile time.
5. Wails v3 is a beta release; the pinned version
   (`v3.0.0-beta.19`) must move together with API changes.

## ZIP Contents

The ZIP contains the complete source replacement, including the built
frontend (`frontend/dist`) so the Go build works immediately after
extraction. Excluded: `.git/`, `node_modules`, native build output,
temporary files, IDE files, secrets, runtime data.
