# FreeIran

> **FINAL / FROZEN — v0.final (implementation baseline 0.14.2), finalized 2026-10-07.**
>
> FreeIran is feature-complete for its verified scope and is no longer under active
> development: there is no active FreeIran roadmap, and no upcoming FreeIran phase is
> described anywhere in this repository. This README documents the ACTUAL capabilities
> and limitations of the frozen product; the technical docs under `docs/` are the
> authority for the frozen implementation. Historical research and archived future
> designs are clearly marked as such and are not commitments. An independent project
> with a similar purpose exists at <https://github.com/mlmvpn/mlmvpn_windows> — it is
> a separate repository with no ownership, migration or continuation relationship to
> FreeIran.

A lightweight, free, open-source VPN configuration manager and proxy
client for Windows (and, architecturally, any desktop platform), built
around a shared Go engine for discovering, testing, maintaining,
running and tunneling through publicly available proxy/VPN
configurations.

**Project:** FreeIran — A SHEYTAN Digital System
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.14.2 (see `VERSION`)
**Status:** production architecture — multi-core protocol runtime with
managed installation, multi-level node discovery, Ping/URL test modes
with measured ranking, verified-connection engine with racing,
environment intelligence, system proxy mode (WinINet), functional
Windows TUN mode (the FreeIran first-party dataplane where the
capability gate holds, the managed sing-box core's native TUN as the
explicit fallback), unified adaptive memory control and kernel-level
process supervision, evidence-based failure classification with
transport-agile route selection, proxy chains compiled into a single
core process, and schema-verified ECH support through sing-box.
v0.14.0 crossed the Phase 2 boundary of the first-party engine
transition (ROADMAP.md): the FreeIran Engine carries supported
SOCKS/HTTP/Shadowsocks-AEAD routes entirely in-process (System Proxy
needs no external core for them) and owns its TUN dataplane (Wintun
→ FreeIran userspace IP stack → engine sessions → real routing/DNS
authorities → first-party outbound, with loop-prevention-bound
upstreams). v0.14.1 is a correctness release on top of that boundary:
CI-failure root causes removed, Shadowsocks `net.Conn` concurrency
and cancellation made deterministic, HTTP absolute-form routed through
the central Router, the DNS authority actually wired, the TUN
admission lifetime made explicit, the Windows control plane (socket
binding byte order, context cancellation, Wintun send errors,
additive-only IP mutation, dual-family covering routes) corrected —
with `Active` evidence never exceeding what the activation gates
prove. The external cores remain the explicit compatibility fallback
for everything else (history lives in CHANGELOG.md).

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

The long-term direction of an autonomous connectivity engine —
connectivity, privacy, censorship-resilience, routing, evidence and
recovery — was captured in
[docs/autonomous-connectivity.md](docs/autonomous-connectivity.md).
That document is **ARCHIVED / HISTORICAL RESEARCH — NOT AN ACTIVE
FREEIRAN ROADMAP**; the project is frozen and none of its target
models are upcoming FreeIran work.

## Current verified capabilities

- **First-party engine (v0.14.1: correctness-hardened real dataplane).** The FreeIran
  Engine (`engine/freecore`) is a registered in-process backend:
  SOCKS, HTTP and Shadowsocks-AEAD (aes-128-gcm, aes-256-gcm,
  chacha20-ietf-poly1305 — reference-interop evidenced) remote
  configurations run entirely inside the FreeIran process, and the
  first-party TUN dataplane carries real TCP flows through the
  FreeIran-owned userspace IP stack, a real Router (DIRECT / PROXY /
  BLOCK with explainable decisions) and a real bounded DNS authority.
  First-party TUN selection prefers the FreeIran dataplane whenever
  the capability gate holds; sing-box TUN remains the explicit
  fallback. Its capability gate is deliberately narrow; see
  [docs/protocols.md](docs/protocols.md) for the exact matrix and
  evidence class and [docs/tun.md](docs/tun.md) for the TUN evidence
  ladder.
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
- System Proxy mode (WinINet) — pointed at the active session's
  local endpoint, which for first-party-supported routes is owned by
  the FreeIran Engine (no external core launched) and for every
  other route by the selected external core — and Windows TUN mode
  through the first-party dataplane (transactional, additively
  mutated, evidence-laddered activation; the managed sing-box native
  TUN remains the explicit fallback) — see
  [docs/tun.md](docs/tun.md).
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
- Configuration workspace over the REAL dataset: one server-side
  filter/sort/pagination pipeline with the true global total and a
  global config-ID tie-breaker, authoritative uncapped group counts,
  source scopes with factual freshness ("Updated 12m ago" /
  "Never fetched"; content hashes stay internal evidence), and
  paginated infinite scroll in every scope.
- System integration as a first-class Main-page block: System Proxy
  and TUN toggles plus local inbound ports through the ONE
  TunnelService and settings paths, mirrored by the native tray
  (state synced from the authoritative backend, never from the
  click), with honest prerequisites and disconnected-state cleanup.
- Windows CI with real-core verification, GUI launch proof, PE
  resource-icon regression check, security scanning
  ([docs/ci.md](docs/ci.md), [docs/security.md](docs/security.md)).

## Current limitations (honest)

- The first-party engine implements ONLY local proxy forwarding for
  SOCKS/HTTP remotes over plain TCP (plus direct dialing). Every
  other protocol/transport/security combination — VLESS, VMess,
  Trojan, Shadowsocks, Hysteria/Hysteria2, TUIC, WireGuard,
  TLS/REALITY, UDP, proxy chains — still runs through the external
  cores, exactly as before. The first-party TUN dataplane serves the
  configurations inside its capability gate; everything else falls
  back to the managed sing-box native TUN, and unsupported
  configurations are refused fail-closed (see
  [docs/tun.md](docs/tun.md)).
- The Windows TUN physical runtime on an elevated physical host is
  NOT VERIFIED — evidence stops at generated-config (real pinned
  sing-box binary), Linux/unit, Windows-compile and Windows-CI
  classes; application traffic crossing the TUN has not been proven
  by a physical runtime traffic probe and is never claimed
  ([docs/tun.md](docs/tun.md) evidence ladder). UDP through the
  first-party TUN is fail-closed (not implemented).
- TUN is a traffic-routing feature, NOT a kill switch; no WFP
  firewall layer exists.
- No FreeIran-owned DNS engine (DNS inside TUN is the sing-box
  document's module); no IPv6 leak-proof monitoring; `Connected` is
  never `Protected`.
- The Windows window/taskbar icon fix is proven at the
  resource/code-path level (icon group at the numeric resource ID
  the pinned Wails loads, byte-identical to the canonical asset) and
  by the strengthened PE regression test; live shell rendering is
  not executable in CI and is not claimed.
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
   FETCH → PARSE →       ┌────┴────┐
   NORMALIZE →       │         │
   VALIDATE →        │         │
   DEDUP → PERSIST   │         │
                     │         │
          Backend registry (engine/core)
          ┌────────────┬────────────┬────────────┐
          │ freecore   │ xray       │ sing-box   │
          │ (in-proc)  │ v2ray      │            │
          └────────────┴────────────┴────────────┘
               FreeIran Engine    external protocol
               (engine/freecore)  cores (managed)
               SOCKS/HTTP local
               proxy path (new in
               v0.13.1)
                           │
              Test Queue (engine/testqueue)
              bounded workers + priority + cancellation
                           │
              Tunnel (engine/tunnel)
              System Proxy (WinINet) — production
              endpoint owned by FreeIran Engine on
              first-party-supported routes, external
              core otherwise
              TUN — Windows: first-party FreeIran dataplane
              (engine/freecore + engine/tunnel) for supported
              configurations; managed sing-box native TUN as the
              explicit fallback; unsupported configurations refused
              fail-closed
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
  identifiers. Historical research toward a fuller privacy contract
  (minimal metadata, WFP kill-switch target, banned marketing claims)
  is archived in
  [docs/autonomous-connectivity.md](docs/autonomous-connectivity.md)
  and [docs/security.md](docs/security.md) — archived, not upcoming
  work.
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

`cmd/freeiran/frontend/dist` is EPHEMERAL generated output, not a
committed tree (v0.14.1 removed it from Git; v0.14.2 `.gitignore`s
it): plain `go build ./cmd/freeiran` needs a frontend build first —
run `npm run build:embed` in `frontend/` to stage the embed tree
(copy-dist.mjs wipes and re-copies the target, then validates it
against the canonical asset inventory). CI stages the tree per job
and transfers it to the desktop-validation job as the
`freeiran-frontend-embed` artifact; `go:embed` is unchanged and no
placeholder exists.

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
├── ROADMAP.md             ARCHIVED historical roadmap (frozen 2026-10-07)
└── VERSION                Application version (current source of truth)
```

## Final status

FreeIran is **FINAL / FROZEN as of 2026-10-07** with the human-facing
designation **v0.final** and the machine-readable implementation
version **0.14.2** (the value in `VERSION`, which all build and
release machinery requires). The phased roadmap is closed:
[ROADMAP.md](ROADMAP.md) is an archived historical record, and the
future-design research in [docs/autonomous-connectivity.md](docs/autonomous-connectivity.md)
is archived, not upcoming work. See [FINAL.md](FINAL.md) for the
canonical freeze statement — final scope, known limitations and
evidence boundaries. Release history lives in
[CHANGELOG.md](CHANGELOG.md); this README describes the frozen
product only.

An independent project with a similar purpose exists at
[mlmvpn/mlmvpn_windows](https://github.com/mlmvpn/mlmvpn_windows). It
is a separate, unrelated repository — not a FreeIran successor, fork
or continuation; FreeIran development has not moved there.

## Attribution & License

**Architect / Project Originator:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran

License: see [LICENSE](LICENSE).
