# FreeIran

A lightweight, free, open-source VPN configuration manager for Windows (and,
architecturally, any desktop platform), built around a shared Go engine for
discovering, testing, maintaining and running publicly available
proxy/VPN configurations.

**Project:** FreeIran
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.4.1 (see `VERSION`)
**Status:** production architecture — multi-core protocol runtime with chunked local storage

---

## Vision

> Continuously find publicly available configurations, test them, keep the
> ones that work, archive the ones that fail, remove duplicates, and make
> the working pool immediately usable from a lightweight client.

FreeIran is intended for environments where ordinary Internet connectivity
can be heavily restricted, including Iran. It is local-first: no account,
no central backend, no cloud service, and no remote telemetry. All network
activity relates to fetching public configuration sources or testing
configurations.

---

## Architecture Overview

```text
                       FREEIRAN
                          │
                    WAILS v3 APP
                          │
                 TypeScript UI layer        (frontend/)
                          │
                   Generated bindings        (frontend/bindings)
                          │
                    Go application
                     orchestration           (engine/app)
                          │
        ┌─────────────────┼─────────────────┐
        │                 │                 │
    Data Engine       Core Manager      System Engine
  (engine/store,     (engine/core:       (system/)
   engine/chunks,     registry, selection,
   engine/cache)      lifecycle)             │
        │                 │               C++ native layer
   Ingestion pipeline    │                (native/, engine/native)
   (engine/pipeline)     │                optional, with Go fallback
        │                │
   FETCH → PARSE →    Connection Manager
   NORMALIZE →        (engine/connection:
   VALIDATE →         state machine, fallback)
   DEDUP → PERSIST        │
                 ┌─────────┼─────────┐
                 │         │         │
              Xray Core  V2Ray Core  sing-box
             (core/xray) (core/v2ray) (core/singbox)
                 │         │         │
                 └─────────┼─────────┘
                           │
                  local SOCKS/HTTP listener
```

- **Go** is the primary orchestration/system language: lifecycle,
  scheduling, concurrency, storage, sources, testing, the
  protocol-core manager (Xray / V2Ray / sing-box backends behind one
  abstraction) and the API exposed to the UI.
- **C++** provides a small, measurable acceleration layer (batch hashing,
  CRC-32 checksums, URL scanning) behind a stable C ABI, with bit-exact
  pure-Go fallbacks so correctness never depends on it.
- **TypeScript (React + Vite)** is the UI layer: application state,
  configuration browsing with virtualized lists, source management,
  diagnostics. The UI talks to Go exclusively through generated Wails v3
  bindings and events — there is no localhost HTTP API.

Details: [docs/architecture.md](docs/architecture.md),
[docs/storage-format.md](docs/storage-format.md),
[docs/performance.md](docs/performance.md),
[docs/ci.md](docs/ci.md), [docs/security.md](docs/security.md).

---

## Key Features (v0.4.x)

- **Chunked local storage** — checksummed immutable chunk files with a
  binary fingerprint index, segmented write-ahead journal, incremental
  writes, atomic commits, compaction and crash recovery. No full-dataset
  JSON dumps.
- **Deterministic resource lifecycle** — a bounded chunk-handle cache is
  the sole owner of open chunk descriptors: eviction closes files, pins
  protect in-flight reads, and `Close()` releases every descriptor before
  returning, so the store directory is deletable immediately on every
  platform (the Windows guarantee, enforced by tests).
- **Asynchronous flush with backpressure** — memtables freeze into
  immutable tables that a background worker persists as chunks without
  blocking writers; the pending queue is bounded, and writers throttle
  when it is full.
- **Streaming ingestion pipeline** — bounded worker pools for fetch,
  parse and dedup/persist with backpressure and cancellation; a slow or
  failed source never blocks other sources.
- **Content-hash change detection** — unchanged sources skip parse and
  persistence entirely.
- **Progressive startup** — the app opens on metadata (registry + index)
  immediately; storage verification and cache warm-up run in the
  background; the UI reports `loading / ready / degraded / ingesting`.
- **Multi-layer caching** — source cache, parse stats, chunk-handle cache
  and a hot-configuration cache for instant UI pages, with hit/miss
  tracking and size bounds.
- **Optional C++ acceleration** — built with `-tags native_accel`; falls
  back to identical pure-Go implementations at build time or runtime.
- **Deterministic fingerprints** — the original SHA-256 identity model is
  preserved; duplicates are eliminated before testing.
- **Streaming legacy migration** — the old JSON database (`database v1`)
  is detected, streamed (bounded memory), imported in batches, verified
  through the full disk path and preserved as `<file>.migrated`.
  Idempotent and never destructive.
- **Local diagnostics** — startup time, stage timings, cache hit rates,
  open file handles, flush/compaction timings, WAL size; local-only,
  never sent anywhere.
- **Multi-core protocol runtime** — real Xray, V2Ray (V2Fly) and
  sing-box backends behind one `Core` abstraction with verified
  capability declarations. V2Ray is a first-class backend, never a
  synonym for Xray: capability resolution knows that REALITY lives in
  Xray/sing-box while classic QUIC/HTTP-2 transports live in V2Ray.
- **Deterministic backend selection** — configurations map to
  backends through the registry's capability model (preference →
  priority → name ordering), with explainable selection reasons,
  ordered fallbacks and bounded retry; no protocol decisions scattered
  through the application.
- **Connection lifecycle** — an explicit state machine
  (`disconnected → selecting → preparing → starting_core →
  waiting_for_ready → connected → disconnecting`), one active session,
  process health separated from network health (listener readiness),
  and core crash detection through a background monitor.
- **Runtime security** — generated core configurations live in 0600
  files inside 0700 temporary directories, are removed on shutdown
  AND after failed startups, and are written only after the owning
  process is stopped (Windows file-lock discipline). Credentials are
  redacted from logs, errors, selection reasons and every UI surface.
- **Core testing integration** — the tester can execute a
  configuration through a real backend (validate → temporary core →
  readiness → latency → deterministic teardown) with capability-first
  candidate resolution and bounded fallback.
- **GitHub Actions CI/CD** — tests, race detector, native tests,
  frontend typecheck/build, the full Windows test matrix (including the
  store lifecycle), a dedicated protocol-cores job that installs the
  three pinned releases (SHA-256 verified) and runs real-binary smoke
  suites, Windows desktop builds, tagged releases with checksums,
  dependency vulnerability scanning and secret scanning.

---

## Building

Requirements: Go **1.26.8** (the go.mod toolchain — see
[docs/development.md](docs/development.md) for the toolchain policy),
Node.js 22+, npm, a C++17 compiler (optional — only for the native
acceleration layer), make (optional).

```bash
# 1. Build the frontend and stage it for the Go embed
cd frontend
npm ci
npm run build:embed     # builds and copies dist into cmd/freeiran
cd ..

# 2. Build the desktop application (Windows amd64 primary target)
#    from a Windows machine:
go build -trimpath \
  -ldflags "-s -w -X github.com/Parsaetak/FreeIran/internal/version.Version=$(cat VERSION | tr -d '[:space:]')" \
  -o FreeIran-windows-amd64.exe ./cmd/freeiran

# From Linux/macOS you can cross-compile the Windows binary:
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -o FreeIran-windows-amd64.exe ./cmd/freeiran
```

### Optional C++ acceleration

```bash
make -C native            # builds libfreeiran_native + runs its tests
CGO_ENABLED=1 go build -tags native_accel -o FreeIran.exe ./cmd/freeiran
```

Without the `native_accel` tag the binary uses the pure-Go
implementations, which are tested to produce identical results.
`FREEIRAN_NATIVE=off` disables the native path at runtime.

### Development

```bash
go test ./engine/... ./system/... ./internal/...   # Go tests
go test -race ./engine/...                          # race detector
cd frontend && npm test                             # frontend tests
cd frontend && npm run dev                          # Vite dev server
```

A committed placeholder at `cmd/freeiran/frontend/dist` keeps `go build
./cmd/freeiran` working before any frontend build; the real UI is staged
by `npm run build:embed` (used by CI).

On Linux, building the GUI locally requires GTK4/WebKitGTK development
packages (`wails3 doctor` reports what is missing). CI validates the
desktop build for windows/amd64 where no GUI packages are needed.

---

## Repository Structure

```text
FreeIran/
├── cmd/freeiran/          Desktop application entrypoint (Wails v3)
├── engine/                Shared Go engine
│   ├── app/               Application orchestration + UI service surface
│   ├── cache/             Bounded LRU cache layers (TTL, versions, stats,
│   │                      eviction callbacks for owned resources)
│   ├── chunks/            Chunking subsystem (FIRC format)
│   ├── config/            Universal configuration model + fingerprints
│   ├── core/              Protocol-core execution boundary: backend
│   │                      abstraction, capability model, registry,
│   │                      deterministic selection, supervised process
│   │                      lifecycle, health checks
│   ├── core/xray/         Xray adapter (V4 dialect + REALITY/vision/XHTTP)
│   ├── core/v2ray/        V2Ray (V2Fly) adapter + shared V4 generator
│   ├── core/singbox/      sing-box adapter (native JSON dialect)
│   ├── core/contract/     shared backend contract test suite
│   ├── connection/        Connection manager + explicit state machine
│   ├── errors/            Structured, classified errors
│   ├── metrics/           Local performance counters
│   ├── native/            Go↔C++ bridge (pure-Go fallbacks)
│   ├── parser/            Multi-format configuration parser
│   ├── pipeline/          Streaming ingestion pipeline (worker pools)
│   ├── scheduler/         Interval scheduler (skip-if-busy, jitter)
│   ├── source/            Source model, HTTP fetcher, collector
│   ├── store/             Chunked persistence: segmented WAL, frozen
│   │                      memtables + background flush, refcounted
│   │                      chunk-handle cache, compaction, streaming
│   │                      JSON v1 migration, diagnostics
│   └── tester/            Probe interface + TCP reachability probe
│                          + core probe (real backend execution)
├── frontend/              TypeScript UI (Vite + React + zustand)
│   ├── bindings/          Generated Wails bindings
│   └── src/               components/ pages/ state/ services/ workers/
│                          utilities/ styles/
├── native/                C++ acceleration layer (C ABI, no deps)
├── system/                System engine: paths, processes, network,
│   └── platform           platform-specific files (unix/windows)
├── internal/version/      Single source of truth for versioning
├── .github/workflows/     CI, release and security pipelines
├── docs/                  Architecture, storage format, performance,
│                          CI and security docs
├── VERSION                Application version (0.4.1)
└── worklog.md             Engineering worklog
```

The v0.1-era `engine/database`, `engine/pool`, `engine/archive` and the
deprecated `engine.Engine` orchestrator were removed in v0.3.0: they
were dead code after the chunked store became the persistence path.
The legacy JSON *format* knowledge is preserved (and tested) inside
`engine/store/migrate.go`.

---

## Data Migration

Existing users of the v0.1 JSON database keep their data:

1. On first run (or via *Diagnostics → Migrate*), the legacy JSON file is
   detected and validated.
2. Records are streamed into the chunked store in bounded batches with
   their original fingerprints preserved — a huge legacy database never
   requires proportional RAM.
3. The import is verified record-by-record through the full disk path
   (index + chunk read) in a second streaming pass.
4. The legacy file is renamed to `<original>.migrated` — never deleted.

Migration is idempotent: running it again is a no-op, and an interrupted
run is safely re-runnable (records are re-applied idempotently and the
legacy file is only renamed after full verification).

---

## Security & Privacy

- Downloaded configuration data is untrusted input: it is parsed,
  normalized, validated and deduplicated before storage or testing.
- No arbitrary scripts are executed; no certificates are installed; no
  credentials are written to logs (log paths pass through redaction).
- The app is local-first: no account, no cloud, no browsing history,
  no remote telemetry. Metrics are local diagnostics.
- CI runs `govulncheck` (platform-targeted for the desktop app, pinned
  tool version) and gitleaks on every push; see
  [docs/security.md](docs/security.md).

---

## Roadmap

- [x] v0.1 — Engine foundation (config model, parser, dedup, JSON store)
- [x] v0.2 — Architecture upgrade: chunked store, streaming pipeline,
      caching, native acceleration layer, Wails v3 desktop shell,
      TypeScript UI, CI/CD, migration
- [x] v0.3 — Storage lifecycle rework (Windows-safe resource ownership,
      segmented WAL, background flush), streaming migration, toolchain
      policy (go1.26.8), CI/security modernization, deep diagnostics
- [x] v0.4 — Protocol core integration: Xray + V2Ray (V2Fly) +
      sing-box as real backends, deterministic selection, connection
      state machine, core-based testing, real-binary CI verification
- [ ] v0.5 — Hysteria2, TUIC, WireGuard runtimes; managed core
      installation (verified downloads)
- [ ] v0.6 — Source expansion, reliability statistics
- [ ] v1.0 — Stable releases, security review, reproducible builds

---

## Attribution & License

**Architect / Project Originator:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran

License: see [LICENSE](LICENSE).
