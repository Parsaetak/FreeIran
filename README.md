# FreeIran

A lightweight, free, open-source VPN configuration manager for Windows (and,
architecturally, any desktop platform), built around a shared Go engine for
discovering, testing, maintaining and running publicly available
proxy/VPN configurations.

**Project:** FreeIran
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.2.0 (see `VERSION`)
**Status:** production architecture — desktop application with chunked local storage

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
  (engine/store,     (engine/core,        (system/)
   engine/chunks,     process mgmt)
   engine/cache)
        │
   Ingestion pipeline                    C++ native layer
   (engine/pipeline)                     (native/, engine/native)
        │                                optional, with Go fallback
   FETCH → PARSE → NORMALIZE →
   VALIDATE → DEDUP → PERSIST
```

- **Go** is the primary orchestration/system language: lifecycle,
  scheduling, concurrency, storage, sources, testing, protocol-core
  management, and the API exposed to the UI.
- **C++** provides a small, measurable acceleration layer (batch hashing,
  CRC-32 checksums, URL scanning) behind a stable C ABI, with bit-exact
  pure-Go fallbacks so correctness never depends on it.
- **TypeScript (React + Vite)** is the UI layer: application state,
  configuration browsing with virtualized lists, source management,
  diagnostics. The UI talks to Go exclusively through generated Wails v3
  bindings and events — there is no localhost HTTP API.

Details: [docs/architecture.md](docs/architecture.md),
[docs/storage-format.md](docs/storage-format.md),
[docs/performance.md](docs/performance.md).

---

## Key Features (v0.2.0)

- **Chunked local storage** — checksummed chunk files with a binary
  fingerprint index, write-ahead journal, incremental writes, atomic
  commits, compaction and crash recovery. No full-dataset JSON dumps.
- **Streaming ingestion pipeline** — bounded worker pools for fetch,
  parse and dedup/persist with backpressure and cancellation; a slow or
  failed source never blocks other sources.
- **Content-hash change detection** — unchanged sources skip parse and
  persistence entirely.
- **Progressive startup** — the app opens on metadata (registry + index)
  immediately; storage verification and cache warm-up run in the
  background; the UI reports `loading / ready / degraded / ingesting`.
- **Multi-layer caching** — source cache, parse stats, chunk-handle LRU
  and a hot-configuration cache for instant UI pages, with hit/miss
  tracking and size bounds.
- **Optional C++ acceleration** — built with `-tags native_accel`; falls
  back to identical pure-Go implementations at build time or runtime.
- **Deterministic fingerprints** — the original SHA-256 identity model is
  preserved; duplicates are eliminated before testing.
- **Legacy migration** — the old JSON database (`database v1`) is
  detected, validated, imported, verified and preserved as
  `<file>.migrated`. Idempotent and never destructive.
- **Local diagnostics** — startup time, stage timings, cache hit rates,
  worker/queue gauges; local-only, never sent anywhere.
- **GitHub Actions CI/CD** — tests, race detector, native tests,
  frontend typecheck/build, Windows desktop builds, tagged releases with
  checksums, dependency vulnerability scanning and secret scanning.

---

## Building

Requirements: Go 1.25+, Node.js 22+, npm, a C++17 compiler (optional —
only for the native acceleration layer), make (optional).

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
│   ├── archive/           Failed-configuration archive (JSON v1, gzip-ready)
│   ├── cache/             Bounded LRU cache layers (TTL, versions, stats)
│   ├── chunks/            Chunking subsystem (FIRC format)
│   ├── config/            Universal configuration model + fingerprints
│   ├── core/              Protocol-core execution boundary (registry)
│   ├── database/          Legacy JSON database (kept for migration)
│   ├── errors/            Structured, classified errors
│   ├── metrics/           Local performance counters
│   ├── native/            Go↔C++ bridge (pure-Go fallbacks)
│   ├── parser/            Multi-format configuration parser
│   ├── pipeline/          Streaming ingestion pipeline (worker pools)
│   ├── pool/              In-memory configuration pool
│   ├── scheduler/         Interval scheduler (skip-if-busy, jitter)
│   ├── source/            Source model, HTTP fetcher, collector
│   ├── store/             Chunked persistence (WAL, index, compaction,
│   │                      JSON v1 migration)
│   └── tester/            Probe interface + TCP reachability probe
├── frontend/              TypeScript UI (Vite + React + zustand)
│   ├── bindings/          Generated Wails bindings (do not edit)
│   └── src/               components/ pages/ state/ services/ workers/
│                          utilities/ styles/
├── native/                C++ acceleration layer (C ABI, no deps)
├── system/                System engine: paths, processes, network,
│   └── platform           platform-specific files (unix/windows)
├── internal/version/      Single source of truth for versioning
├── .github/workflows/     CI, release and security pipelines
├── docs/                  Architecture, storage format, performance docs
├── VERSION                Application version (0.2.0)
└── worklog.md             Engineering worklog
```

---

## Data Migration

Existing users of the v0.1 JSON database keep their data:

1. On first run (or via *Diagnostics → Migrate*), the legacy JSON file is
   detected and validated.
2. Every record is imported into the chunked store with its original
   fingerprint preserved.
3. The import is verified record-by-record.
4. The legacy file is renamed to `<original>.migrated` — never deleted.

Migration is idempotent: running it again is a no-op.

---

## Security & Privacy

- Downloaded configuration data is untrusted input: it is parsed,
  normalized, validated and deduplicated before storage or testing.
- No arbitrary scripts are executed; no certificates are installed; no
  credentials are written to logs (log paths pass through redaction).
- The app is local-first: no account, no cloud, no browsing history,
  no remote telemetry. Metrics are local diagnostics.
- CI runs `govulncheck` and gitleaks on every push.

---

## Roadmap

- [x] v0.1 — Engine foundation (config model, parser, dedup, JSON store)
- [x] v0.2 — Architecture upgrade: chunked store, streaming pipeline,
      caching, native acceleration layer, Wails v3 desktop shell,
      TypeScript UI, CI/CD, migration
- [ ] v0.3 — Protocol core integration (Xray), full connect/disconnect
- [ ] v0.4 — sing-box, Hysteria2, TUIC, WireGuard runtimes
- [ ] v0.5 — Source expansion, reliability statistics
- [ ] v1.0 — Stable releases, security review, reproducible builds

---

## Attribution & License

**Architect / Project Originator:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran

License: see [LICENSE](LICENSE).
