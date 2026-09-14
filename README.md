# FreeIran

A lightweight, free, open-source VPN configuration manager and proxy
client for Windows (and, architecturally, any desktop platform), built
around a shared Go engine for discovering, testing, maintaining,
running and tunneling through publicly available proxy/VPN
configurations.

**Project:** FreeIran
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.9.3 (see `VERSION`)
**Status:** production architecture — multi-core protocol runtime with
managed installation, test queue, system proxy and TUN mode, unified
adaptive memory control and kernel-level process supervision

---

## What's new in v0.9.3

### The autonomous connection engine

FreeIran now behaves like an automatic connectivity engine rather
than a configuration manager:

- **Connect means connect.** Pressing CONNECT selects the best viable
  candidate from real test history, validates it, chooses a
  compatible core, waits for readiness and verifies the tunnel — no
  manual configuration picking, no manual core selection.
- **Deterministic ranking.** Every stored configuration is scored
  from its actual observations (success rate, median latency,
  stability, timeout frequency, test age, source reliability,
  core compatibility) into BEST / GOOD / UNSTABLE / DEAD / UNKNOWN
  classes, each score carrying a human-readable explanation
  ("31 ms median · 96% recent success · tested 4 min ago").
- **Automatic recovery.** When the active connection dies, the
  recovery supervisor classifies the failure, skips recently failed
  candidates (failure memory + cooldown), switches to the next viable
  one and verifies the result — bounded to 3 attempts per episode
  with growing backoff, 2 episodes per 30-minute window. No infinite
  retries, no reconnect loops, no dead-core launches. An explicit
  opt-out lives in Settings → Reliability.
- **Bounded test history.** Each configuration keeps its last 12 test
  outcomes (workspace-persisted, credential-free) — the data the
  ranking and recovery decisions run on.

### Production repair

- **Single path authority.** The v0.9.2 workspace migration left the
  old `paths_unix.go`/`paths_windows.go` resolvers behind, duplicating
  `DefaultBaseDir`/`CacheBaseDir`/`portableRoot` and breaking every CI
  job. One workspace resolver (`system/workspace.go`) now owns the
  persistence model; regression tests guard it.
- **Worker-pool race fixed.** A shrink of the test queue's dynamic
  pool could retire every worker at once (stale-count overshoot),
  leaving queued tests undrained forever. Retirement is now an atomic
  slot claim; a dedicated regression test reproduces the original
  failure under `-race`.

## What's new in v0.9.1

### A testing workspace you can read

The Configurations page had a structural layout bug: the configuration
row grid declared seven columns while every row rendered eight
elements, so the per-row **Test** button wrapped onto an invisible
second grid row and overlapped the row below at common window sizes.
v0.9.1 rebuilds the row as an explicit eight-column grid (select,
protocol, endpoint, transport, latency, health, source, action) with
the sticky header aligned one-to-one, and reorganizes the workspace:

- **Primary actions** (Test selected / Test all / Test untested) stay
  visible; **secondary actions** (Retest failed, Retest working,
  Cancel all) moved into a compact overflow menu so the toolbar can
  never overflow into the list.
- The **test-queue panel** is now a proper progress surface: a real
  progress bar with counts, large **avg / fastest / slowest ping**
  tiles, and quiet counters for queued, active, passed, failed and
  cancelled.
- The configuration list fills the available height exactly — local
  scrolling only, no page-wide scrollbars, no clipped controls at
  960 / 1100 / 1280 / 1440 px or with the sidebar collapsed.
- The v0.9.0 style appendix referenced a set of design tokens that
  were never defined (`--radius-md`, `--surface-1`, `--surface-2`,
  `--font-mono`, `--danger`, `--warning`), silently degrading callouts,
  queue panel and badges. The tokens are now defined once, and the
  duplicated `.card-title` / `.page-header` / badge overrides that made
  pages drift apart were removed — every page shares one header,
  spacing and control rhythm.

### A real application icon

FreeIran ships a proper brand icon (`assets/freeiran-icon.svg` is the
canonical source; regenerate derivatives with `scripts/genicon.py`):

- **Windows** — a committed resource object (`cmd/freeiran/*.syso`,
  built from the ICO with version info and a DPI-aware manifest) is
  linked into every build, so the executable, titlebar and taskbar all
  carry the identity. The release build also links with
  `-H=windowsgui`, killing the console window for good. CI validates
  the icon assets before every release, so it cannot silently
  disappear.
- **Linux** — the window icon is embedded in the binary
  (`internal/appicon`) and applied through the GTK window options.

### Linux amd64 is a first-class release target

The release workflow now builds and publishes **both**
`FreeIran-windows-amd64.zip` and `FreeIran-linux-amd64.zip`, each a
complete portable deployment directory (binary, README, LICENSE,
VERSION, config/, data/, logs/, cache/, cores/, runtime/, docs/,
deployment/ metadata). The Linux build uses the `gtk3` (WebKit2GTK
4.1) frontend for maximum compatibility. **Checksum `.sha256` files
are gone** — the release contains only the two platform ZIPs.

### Developer options, honestly wired

Settings gained a clearly separated **Developer** section (plus a
reorganized General / Connection / Testing / Appearance / Diagnostics
structure). Every control is wired to real engine behaviour — nothing
decorative:

- verbose diagnostics (enriches the diagnostic report with runtime
  detail),
- test-queue worker override (beats the adaptive memory booster),
- network-test timeout override (applied live to Network Diagnostics),
- force Go fallback for native acceleration (same state as
  `FREEIRAN_NATIVE=off`),
- clear caches, open data/logs directory, live queue internals and
  build identity (version, commit, Go version, portable mode).

### Errors that explain themselves

Connection failures now follow **what happened + why + what to do
next**: a friendly explanation first, a targeted suggestion when the
failure kind is recognizable (missing core, timeout, auth rejection,
DNS, TUN elevation, port conflict), and the raw technical text behind
an expandable disclosure.

---

## What's new in v0.9.0

### Core loading, fixed end to end

v0.9.0 closes the last gaps in the protocol-core lifecycle. A managed
core is never considered "installed" merely because a file exists:
the full pipeline `discover → verify → version → config validation →
launch → readiness → health → usable` runs before the UI reports
**Ready**, and every failed state carries a human-readable reason plus
a technical-details expander. Managed installs now integrate with
runtime discovery — the three `bin/` directories are core-locator
inputs, so an installed core is immediately usable by Connect and the
tester without any restart.

Three real production bugs fell out of that audit and are fixed with
regression tests:

- **`Repair` deadlocked on every call** — it held the per-core mutex
  and then re-acquired the same non-reentrant mutex inside
  Rollback/HealthCheck/Install.
- **Xray and V2Ray downloads never matched the platform** — asset
  selection required the platform hint inside the asset name, which
  legacy naming (`Xray-windows-64.zip`) does not contain.
- **Install always failed at unpack** — downloads were staged as
  `asset.bin`, so the archive-format switch never matched; and zip
  extraction left the executable non-executable before the version
  probe ran on Unix.

### The CMD window bug is dead

Every remaining child-process launch site (Managed Core Manager
version probes, config validation, smoke tests) now sets
`CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | HideWindow`. Windows CI
runs a regression test asserting the attributes. The smoke test's
polite shutdown uses Windows-safe semantics instead of the unsupported
`os.Interrupt` signal that previously marked every healthy install
**Broken**.

### Internet / Network Diagnostics

A dedicated `engine/netcheck` package runs multiple independent
probes (local links, three DNS resolvers, three TCP endpoints, three
HTTPS targets, latency measurement) and classifies the result into the
seven states the specification requires — from "internet unavailable"
through "DNS failing" to "internet reachable only through the
configured proxy". The new **Network** tab exposes a manual,
cancellable **Check connection** action; when a session is connected
the proxy path is probed too, so "core connected but external
connectivity failing" is distinguishable from a dead internet line.

### Real ping, honest results

Configuration tests now measure the actual round-trip through the
generated tunnel (SOCKS5 CONNECT + a 204 fetch via the new
`engine/socks5` package) instead of reporting the core's local startup
time. Every result records working/failed, ping in milliseconds,
quality band (excellent / good / acceptable / slow), test duration,
protocol, backend, endpoint and timestamp — and **persists to the
store**, so test-queue results survive and display without retesting.
The Configurations workspace gains server-side status filters, sorting
by ping/protocol/recency, multi-select and bulk test actions (all /
untested / failed / working / selected) with a live queue progress
panel including average, fastest and slowest measured latency.

### Modern tabs

The UI is now eight focused tabs — Dashboard (with a first-launch
onboarding checklist), Configurations, Sources, **Cores** (one-click
Install → Verify → Start using), Connection, **Network**, Diagnostics
and Settings (with a sanitized copy/export diagnostic report). Install
progress (download bytes, verification, smoke test) streams to the UI
as `freeiran:coreprogress` events.

### Complete deployment package

The release ZIP is no longer a bare exe. `FreeIran-windows-amd64.zip`
contains a full portable deployment directory — `FreeIran.exe`,
README, LICENSE, VERSION, `config/`, `data/`, `logs/`, `cache/`,
`cores/`, `runtime/`, `docs/`, `deployment/deployment.json` metadata
and a `portable.marker` the application detects on startup to keep all
state inside the deployment tree. The release pipeline validates the
package contents (executable present, directories present, metadata
consistent) before publishing and fails the release otherwise.

---

## What's new in v0.8.0

### Windows process supervision, repaired and hardened

The v0.7.0 Windows CI failed on every process-launching test with
`system/start: environment: bind kill-on-close job`. The root cause was
a Win32 return-value protocol bug, not an environment problem:
`SetInformationJobObject` returns a BOOL, but the v0.7 launcher judged
success from the thread's stale `GetLastError()` value — which
BOOL-returning APIs do not reset on success. On GitHub Actions runners
the stale errno is nonzero, so a **successful** job configuration was
misread as a failure and the child was killed. v0.8 validates every
Win32 call by its actual return value and consults `GetLastError()`
only as a diagnostic on genuine failure.

Supervision is now a three-tier strategy that stays deterministic in
restricted environments: direct job assignment (Windows 8+ nests job
hierarchies, so runner jobs are no obstacle) → a relaunch with
`CREATE_BREAKAWAY_FROM_JOB` when assignment is access-denied → a
supervised fallback with Toolhelp32 process-tree termination that
keeps the no-orphan guarantee and *surfaces* the degradation through
diagnostics instead of failing silently. `cmd.exe` and other test
binaries resolve through `%COMSPEC%` / `%SystemRoot%\System32` with
on-disk validation — never the working directory or inherited PATH —
while protocol-core paths keep their strict explicit validation.

### Deterministic lifecycle state machine

Managed processes now expose `running → stopping → stopped / exited /
cancelled` with distinct classifications for launch failure, natural
exit, cancellation and environment degradation; `Stop` is
idempotent, race-free and synchronizing for concurrent callers;
stdout/stderr capture is genuinely concurrency-safe with a bounded
pipe-drain deadline; and descendants are reaped on **every** exit
path (a grandchild that ignores the polite signal cannot survive).
The lifecycle battery (15 tests, `-race` clean) covers the
no-visible-console, capture, cancellation, forced-termination,
repeated/concurrent Stop, startup-failure, job-binding-failure and
grandchild-cannot-survive guarantees.

### Memory Booster 2.0

The v0.7 `engine/mempressure` + `engine/booster` libraries are now
actually wired into the running application as one adaptive
controller: it samples the real subsystems (cache layers, test-queue
memory, store memtable + WAL bytes, Go heap, RSS, GC pressure) every
two seconds and adapts worker concurrency, queue depth and cache
targets — with hard ceilings, floors and hysteresis, gradual
recovery, cache shedding and a GC hint at critical pressure. Every
adjustment is logged and visible in the new Diagnostics memory panel;
nothing degrades silently.

### Desktop wiring audit

The v0.6 Core Manager, Test Queue and Tunnel services existed in the
Go backend but were never registered with the Wails runtime, so no
frontend action could reach them. v0.8 registers all three, ships
bindings for them, and adds the system-integration (System Proxy /
TUN) controls plus live memory and test-queue panels to the UI.

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
