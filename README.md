# FreeIran

A lightweight, free, open-source VPN configuration manager and proxy
client for Windows (and, architecturally, any desktop platform), built
around a shared Go engine for discovering, testing, maintaining,
running and tunneling through publicly available proxy/VPN
configurations.

**Project:** FreeIran — A SHEYTAN Digital System
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.12.2 (see `VERSION`)
**Status:** production architecture — multi-core protocol runtime with
managed installation, multi-level node discovery, Ping/URL test modes
with measured ranking, verified-connection engine with racing,
environment intelligence, system proxy mode (WinINet), functional
Windows TUN mode through the managed sing-box core's native TUN
dataplane, unified adaptive memory control and kernel-level process
supervision, evidence-based failure classification with
transport-agile route selection, proxy chains compiled into a single
core process, and schema-verified ECH support through sing-box.
v0.12.2 removed Tor and Psiphon from the active product (history
lives in CHANGELOG.md).

## What FreeIran is

FreeIran discovers publicly available proxy/VPN configurations,
tests them, keeps the ones that work, archives the ones that fail,
removes duplicates, and makes the working pool immediately usable
from a lightweight client — local-first: no account, no central
backend, no cloud service, no remote telemetry. All network activity
relates to fetching public configuration sources, downloading
official core binaries from their upstream release pages, or testing
configurations. It is intended for environments where ordinary
Internet connectivity can be heavily restricted, including Iran.

The long-term direction is an autonomous connectivity engine —
connectivity, privacy, censorship-resilience, routing, evidence and
recovery — captured in
[docs/autonomous-connectivity.md](docs/autonomous-connectivity.md)
as a future-work contract (PLANNED; not implemented).

## Current verified capabilities

- Multi-core protocol runtime: Xray, V2Ray and sing-box as managed,
  digest-verified backends (Mihomo is core-manager-managed; its
  connection adapter is deliberately out of scope). See
  [docs/protocols.md](docs/protocols.md) for the truthful
  capability matrix and its evidence classes.
- Multi-level discovery (sources, channels, smart search), bounded
  streaming ingestion, chunked storage, dedup and fingerprints.
- Ping/URL test modes, measured ranking, bounded test queue with
  workers/priority/cancellation.
- Verified-connection engine: state machine, controlled racing,
  environment intelligence, evidence-based failure classification,
  transport-agile selection.
- System Proxy mode (WinINet) and Windows TUN mode through the
  managed sing-box core's native TUN dataplane (transactional,
  observed activation; see [docs/tun.md](docs/tun.md)).
- Proxy Chains (v0.12.2): an ordered hop list over EXISTING
  configurations (2–4 hops), built and validated in the
  Configurations workspace, persisted in the collections sidecar and
  compiled into ONE protocol-core process (Xray
  `streamSettings.sockopt.dialerProxy` / sing-box `detour`) — never
  one process per hop. Chain sessions run the SAME verified
  connection state machine (readiness is never success; the same
  multi-target Internet verification gate applies), and the chain
  check reports per-hop evidence plus a fresh end-to-end
  measurement. Cores without a chaining primitive (V2Ray) refuse
  chains explicitly.
- Windows CI with real-core verification, GUI launch proof, security
  scanning ([docs/ci.md](docs/ci.md), [docs/security.md](docs/security.md)).

## Current limitations (honest)

- The Windows TUN physical runtime on an elevated physical host is
  NOT VERIFIED — evidence stops at generated-config (real pinned
  sing-box binary), Linux/unit, Windows-compile and Windows-CI
  classes ([docs/tun.md](docs/tun.md) evidence ladder).
- TUN is a traffic-routing feature, NOT a kill switch; no WFP
  firewall layer exists.
- No FreeIran-owned DNS engine (DNS inside TUN is the sing-box
  document's module); no IPv6 leak-proof monitoring; `Connected` is
  never `Protected`.
- Core support is bounded by the verified matrix; upstream core
  capabilities are not automatically exposed.
- The GUI launch proof executes in CI, not in the authoring
  environment; remote Actions verdicts arrive with the next push.

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
              System Proxy (WinINet) — production
              TUN — sing-box native dataplane (Windows)
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

Details: [docs/README.md](docs/README.md) (documentation index),
[docs/architecture.md](docs/architecture.md),
[docs/storage-format.md](docs/storage-format.md),
[docs/performance.md](docs/performance.md),
[docs/ci.md](docs/ci.md), [docs/security.md](docs/security.md),
[docs/development.md](docs/development.md).

## Security & Privacy

- Downloaded configuration data is untrusted input: it is parsed,
  normalized, validated and deduplicated before storage or testing.
- **Protocol-core binaries are downloaded only from the official
  upstream GitHub Releases** of XTLS/Xray-core, v2fly/v2ray-core and
  SagerNet/sing-box, over HTTPS. An install is REJECTED when a
  release publishes no authoritative digest (release-API digest or
  .dgst sidecar) — a locally computed hash is tamper evidence, never
  a trust anchor. Every managed binary passes the mandatory checksum
  gate, and every archive extraction is bounded
  (`internal/safearchive`).
- No arbitrary scripts are executed; no certificates are installed; no
  credentials are written to logs (log paths pass through redaction).
- The app is local-first: no account, no cloud, no browsing history,
  no remote telemetry, no hardware-identifier collection. Metrics are
  local diagnostics.
- VPN trust is shifted, not eliminated; a VPN does not hide the local
  MAC from the access network and does not rewrite device
  identifiers. The future privacy contract (minimal metadata, WFP
  kill-switch target, banned marketing claims) is recorded in
  [docs/autonomous-connectivity.md](docs/autonomous-connectivity.md)
  and [docs/security.md](docs/security.md).
- CI runs `govulncheck` and `gitleaks` on every push; see
  [docs/security.md](docs/security.md).

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
│   ├── coremgr/           Managed Core Manager (install/update/rollback)
│   ├── connection/        Connection manager + state machine + failover
│   ├── errors/            Structured, classified errors
│   ├── metrics/           Local performance counters
│   ├── native/            Go↔C++ bridge (pure-Go fallbacks)
│   ├── netcheck/          Connectivity diagnostics + Internet tools engine
│   ├── parser/            Multi-format configuration parser
│   ├── pipeline/          Streaming ingestion pipeline (worker pools)
│   ├── scheduler/         Interval scheduler (skip-if-busy, jitter)
│   ├── source/            Source model + HTTP fetcher + collector
│   ├── store/             Chunked persistence: WAL, memtables, compaction
│   ├── tester/            Probe interface + TCP / core probes + latency semantics
│   ├── testqueue/         Bounded-worker test queue (priority, retry, cancel)
│   └── tunnel/            System Proxy (WinINet); TUN via sing-box dataplane
├── frontend/              TypeScript UI (Vite + React + zustand)
├── native/                C++ acceleration layer (C ABI, no deps)
├── system/                System engine: paths, processes, network, platform
├── internal/version/      Single source of truth for versioning
├── .github/workflows/     CI, release and security pipelines
├── docs/                  Technical documentation (see docs/README.md)
├── CHANGELOG.md           Release history
├── ROADMAP.md             Forward roadmap (phased)
└── VERSION                Application version (current source of truth)
```

## Roadmap

The forward roadmap is maintained in [ROADMAP.md](ROADMAP.md) —
future phases (full-device protection, adaptive censorship engine,
canonical routing/DNS, privacy diagnostics, advanced topologies) with
the future architecture contracts in
[docs/autonomous-connectivity.md](docs/autonomous-connectivity.md).
Nothing beyond the current baseline is complete. Release history
lives in [CHANGELOG.md](CHANGELOG.md) — this README describes the
current product only.

## Attribution & License

**Architect / Project Originator:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran

License: see [LICENSE](LICENSE).
