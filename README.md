# FreeIran

A lightweight, free, open-source VPN configuration manager and proxy
client for Windows (and, architecturally, any desktop platform), built
around a shared Go engine for discovering, testing, maintaining,
running and tunneling through publicly available proxy/VPN
configurations.

**Project:** FreeIran
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.6.0 (see `VERSION`)
**Status:** production architecture — multi-core protocol runtime with
managed installation, test queue, system proxy and TUN mode

---

## What's new in v0.6.0

This release turns FreeIran from a configuration database with core
adapters into a **genuinely usable Windows VPN/proxy client**.

### Managed Core Manager

Xray, V2Ray and sing-box are now first-class managed dependencies.
FreeIran installs, verifies, updates and rolls them back through a
single subsystem:

- **Discover + install** from the official upstream GitHub Releases
  (XTLS/Xray-core, v2fly/v2ray-core, SagerNet/sing-box). No third-
  party mirrors, no bundled binaries.
- **Verify** every downloaded archive against the published SHA-256
  digest before activation. A binary that fails verification is never
  activated.
- **Atomic activation**: a downloaded binary lands in a staging
  directory, is smoke-tested, and only then renamed into place.
  The previous healthy binary is retained as the rollback target.
- **Health checks**: each core is exercised through a minimal SOCKS
  inbound + listener probe; the result is surfaced in the UI as
  `Ready / Broken / Update available`.
- **Stable + prerelease channels** are separate. The user never
  receives a prerelease unless they explicitly opt in.
- **Repair** button: tries rollback first, then a fresh install.
- **Check for updates** in v2rayN-style: current version, latest
  version, release URL, changelog URL, asset size, install button.

### Windows process launch with no console window

FreeIran and every protocol-core it spawns (Xray, V2Ray, sing-box)
now launches **without a visible CMD/console window**, via
`CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS`.
Each child is bound to a kill-on-close Windows job object so an
abnormal FreeIran exit reaps every orphan at the kernel level —
**no orphan core remains after exit**.

### Test Queue

Testing moved from sequential blocking operations to a real queue:

- Bounded worker pool with priority (newly discovered configs and
  user-selected tests jump ahead of bulk re-tests)
- Duplicate task suppression by fingerprint
- Backend-aware concurrency limits (a slow V2Ray cannot starve Xray)
- Per-config timeout + global test timeout
- Exponential-backoff retry
- Cancellation by task ID, by source, or globally
- Graceful shutdown
- Live progress statistics (tests/sec, queue depth, active workers,
  per-backend counts)

### Testing modes

`Quick`, `Balanced`, `Deep`, `Re-test failed`, `Test all`, `Test
selected`, `Continuous` — each presets workers / timeout / attempts /
measurements.

### Source Manager

Every default public source gained full metadata (provider, project,
protocol hints, region, format, priority, refresh interval) and is
persisted to a sidecar `sources.json`. Three new high-quality sources
were added:

- **ShadowsocksAggregator/Eternity** — `mahdibland/ShadowsocksAggregator/master/Eternity.txt`
- **MahsaFreeConfig/MTN** — `mahsanet/MahsaFreeConfig/main/mtn/sub_1.txt`
- **ScrapeAndCategorize/Netherlands** — `10ium/ScrapeAndCategorize/main/output_configs/Netherlands.txt`

The collector now supports **conditional requests** (ETag,
Last-Modified), **gzip/deflate** transport and **content-hash
short-circuit**: an unchanged source skips parse + persistence
entirely.

### System Proxy

A proper Windows system-proxy integration through **WinINet's
per-connection options** (not registry edits). The previous proxy
settings are saved before activation and restored on disable. Running
apps are notified through `INTERNET_OPTION_SETTINGS_CHANGED` +
`INTERNET_OPTION_REFRESH` so they pick up the new proxy without a
restart. Modes: `Direct | System Proxy | SOCKS | HTTP`.

### TUN mode

A real Windows TUN interface backed by **Wintun** (the official
maintained driver). FreeIran resolves `wintun.dll` from the managed
cores directory, the executable directory, or the system directory.
Install + Enable require elevation; the TUN interface is torn down on
disable and on application shutdown (kill-switch-safe: a crash takes
the TUN down with it via the job-object binding).

### Capability-driven backend selection

Configuration → backend selection now follows:

`configuration capability → preferred core → installed healthy cores →
priority → health → recent success`

If the selected core fails, the manager records the failure, classifies
it (network / auth / protocol / config / backend_down / timeout /
cancelled / unknown) and tries the next compatible installed core.
Incompatible cores are never retried for the same configuration. The
final error is explainable in the UI.

### Performance: Speed Booster

An adaptive controller monitors CPU pressure, memory pressure, queue
backlog, core startup failures and network errors. When the system is
healthy and the queue is deep it raises concurrency; when pressure
rises it throttles back. Correctness is never sacrificed for raw
throughput — a configuration that fails is removed from the active
pool, not retried in a tight loop.

### Documentation

Every Markdown file was rewritten to describe the real architecture.
Stale "v0.5.0" claims were removed. See `REPLACEMENT_MANIFEST.md` for
the full change manifest.

---

## Vision

> Continuously find publicly available configurations, test them, keep
> the ones that work, archive the ones that fail, remove duplicates,
> and make the working pool immediately usable from a lightweight
> client.

FreeIran is intended for environments where ordinary Internet
connectivity can be heavily restricted, including Iran. It is local-
first: no account, no central backend, no cloud service, no remote
telemetry. All network activity relates to fetching public
configuration sources, downloading official core binaries from their
upstream release pages, or testing configurations.

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
  (engine/store,     (engine/coremgr:   (system/)
   engine/chunks,     install / update /    │
   engine/cache)      rollback / health)   C++ native layer
        │                 │                (native/, engine/native)
   Ingestion pipeline    │                optional, with Go fallback
   (engine/pipeline,     │
    engine/source)    Connection Manager
        │            (engine/connection: state machine, fallback)
   FETCH → PARSE →        │
   NORMALIZE →       ┌────┴────┐
   VALIDATE →        │         │
   DEDUP → PERSIST   │         │
                     │         │
              Protocol cores (engine/core)
              ┌──────────────┬──────────────┐
              │ xray         │ v2ray        │ sing-box
              └──────────────┴──────────────┘
                           │
              Test Queue (engine/testqueue)
              bounded workers + priority + cancellation
                           │
              Tunnel (engine/tunnel)
              System Proxy (WinINet) | TUN (Wintun)
```

- **Go** is the primary orchestration/system language: lifecycle,
  scheduling, concurrency, storage, sources, testing, the managed
  protocol-core lifecycle, the test queue, the tunnel modes and the
  API exposed to the UI.
- **C++** provides a small, measurable acceleration layer (batch
  hashing, CRC-32 checksums, URL scanning) behind a stable C ABI,
  with bit-exact pure-Go fallbacks so correctness never depends on it.
- **TypeScript (React + Vite)** is the UI layer: application state,
  configuration browsing with virtualized lists, source management,
  diagnostics, core manager, test queue, tunnel mode selector. The
  UI talks to Go exclusively through generated Wails v3 bindings and
  events — there is no localhost HTTP API.

Details: [docs/architecture.md](docs/architecture.md),
[docs/storage-format.md](docs/storage-format.md),
[docs/performance.md](docs/performance.md),
[docs/ci.md](docs/ci.md), [docs/security.md](docs/security.md),
[docs/development.md](docs/development.md).

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
./cmd/freeiran` working before any frontend build; the real UI is
staged by `npm run build:embed` (used by CI).

---

## Repository Structure

```text
FreeIran/
├── cmd/freeiran/          Desktop application entrypoint (Wails v3)
├── engine/                Shared Go engine
│   ├── app/               Application orchestration + UI service surface
│   ├── cache/             Bounded LRU cache layers
│   ├── chunks/            Chunking subsystem (FIRC format)
│   ├── config/            Universal configuration model + fingerprints
│   ├── core/              Protocol-core execution boundary (adapters)
│   ├── coremgr/           [v0.6] Managed Core Manager (install/update/rollback)
│   ├── connection/        Connection manager + state machine + failover
│   ├── errors/            Structured, classified errors
│   ├── metrics/           Local performance counters
│   ├── native/            Go↔C++ bridge (pure-Go fallbacks)
│   ├── parser/            Multi-format configuration parser
│   ├── pipeline/          Streaming ingestion pipeline (worker pools)
│   ├── scheduler/         Interval scheduler (skip-if-busy, jitter)
│   ├── source/            Source model + HTTP fetcher + collector
│   ├── store/             Chunked persistence: WAL, memtables, compaction
│   ├── tester/            Probe interface + TCP / core probes
│   ├── testqueue/         [v0.6] Bounded-worker test queue (priority, retry, cancel)
│   └── tunnel/            [v0.6] System Proxy (WinINet) + TUN (Wintun)
├── frontend/              TypeScript UI (Vite + React + zustand)
├── native/                C++ acceleration layer (C ABI, no deps)
├── system/                System engine: paths, processes, network, platform
├── internal/version/      Single source of truth for versioning
├── .github/workflows/     CI, release and security pipelines
├── docs/                  Architecture, storage, performance, CI, security, dev
├── VERSION                Application version (0.6.0)
└── worklog.md             Engineering worklog
```

---

## Data Migration

Existing users of the v0.1 JSON database keep their data:

1. On first run (or via *Diagnostics → Migrate*), the legacy JSON file
   is detected and validated.
2. Records are streamed into the chunked store in bounded batches
   with their original fingerprints preserved.
3. The import is verified record-by-record through the full disk
   path in a second streaming pass.
4. The legacy file is renamed to `<original>.migrated` — never
   deleted.

Migration is idempotent: running it again is a no-op, and an
interrupted run is safely re-runnable.

---

## Security & Privacy

- Downloaded configuration data is untrusted input: it is parsed,
  normalized, validated and deduplicated before storage or testing.
- **Protocol-core binaries are downloaded only from the official
  upstream GitHub Releases** of XTLS/Xray-core, v2fly/v2ray-core and
  SagerNet/sing-box. Every archive's SHA-256 is verified against the
  published digest before activation.
- No arbitrary scripts are executed; no certificates are installed; no
  credentials are written to logs (log paths pass through redaction).
- The app is local-first: no account, no cloud, no browsing history,
  no remote telemetry. Metrics are local diagnostics.
- CI runs `govulncheck` and `gitleaks` on every push; see
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
- [x] v0.5 — Windows lifecycle repair, deterministic fake-core test
      harness, persistent runtime logging with redaction, professional
      UI, settings persistence
- [x] v0.6 — **Managed Core Manager (install/update/rollback),
      no-console process launch, Test Queue with bounded workers,
      System Proxy (WinINet) + TUN (Wintun), Speed Booster, expanded
      sources with metadata, capability-driven failover**
- [ ] v0.7 — UI polish, source reliability dashboards, config grouping
- [ ] v1.0 — Stable releases, security review, reproducible builds

---

## Attribution & License

**Architect / Project Originator:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran

License: see [LICENSE](LICENSE).
