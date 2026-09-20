# FreeIran

A lightweight, free, open-source VPN configuration manager and proxy
client for Windows (and, architecturally, any desktop platform), built
around a shared Go engine for discovering, testing, maintaining,
running and tunneling through publicly available proxy/VPN
configurations.

**Project:** FreeIran — A SHEYTAN Digital System
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.9.8.7 (see `VERSION`)
**Status:** production architecture — multi-core protocol runtime with
managed installation, multi-level node discovery, Ping/URL test modes
with measured ranking, verified-connection engine with racing,
environment intelligence, system proxy mode (WinINet), unified adaptive
memory control and kernel-level process supervision. TUN mode is
EXPERIMENTAL and disabled in this release (see "TUN mode" below).

---

## What's new in v0.9.8.7

v0.9.8.7 is a **determinism and responsiveness release** — the same
FreeIran architecture with the waiting removed, not the safety
boundaries. No new connectivity features.

### CI recovered (run 35492972394)

- The "Wails toolchain pair consistency" check compared the Go module
  version (`v3.0.0-beta.19`) against the npm version
  (`3.0.0-beta.19`) string-wise, so the Go module's leading `v` was
  treated as version drift and every downstream stage (Go tests, race
  tests, Windows tests, Windows desktop build) was skipped. The check
  now normalizes the optional leading `v` on both sides before
  comparing; a real mismatch still fails the job.

### Event-driven UI synchronization (no more tickers)

- The desktop entrypoint no longer broadcasts `freeiran:state` /
  `freeiran:connection` from 2-second ticker loops. Both streams are
  published from the AUTHORITATIVE transition paths (boot phases,
  degraded/healthy transitions, ingestion start/finish, shutdown,
  every connection state-machine mutation) through a new deduplicating
  publisher (`internal/statepub`): identical snapshots never emit, a
  burst of transitions collapses into one emission of the newest
  snapshot within a 25 ms coalescing window, and `Stop()` joins the
  publisher goroutine so no callback can fire into a closing UI
  runtime. A slow heartbeat watchdog remains in the memory/recovery
  services where it is diagnostics, not synchronization.
- The frontend keeps its event subscriptions and generation guards;
  the previous always-on 5 s provider poll on the Cores page now runs
  ONLY while a provider is actually transitioning.

### Stable, unified frontend asset filenames

- The embedded production tree no longer carries content-hashed
  bundles. After a clean build it is EXACTLY:
  `index.html`, `assets/app.js`, `assets/app.css`,
  `assets/export-worker.js` — replaced in place on every release.
  Because the filenames are stable, the Wails asset handler is wrapped
  with a `Cache-Control: no-cache` policy so an upgraded binary never
  serves stale JavaScript/CSS from the webview cache. CI enforces the
  exact inventory with an explicit allowlist (any hashed, stale or
  unexpected artifact fails the job).

### Truthful Wails bindings

- The committed bindings were regenerated with the pinned wails3
  toolchain (second generation byte-identical). The regeneration
  removed the old hand-maintained ByName shims, surfaced the real
  optionality of Go pointer fields, and exposed two real defects:
  a phantom `log_retention_days` UI field (removed) and local inbound
  port preferences whose controls shipped in v0.9.8.3 but were never
  wired into the backend. The port settings are now persisted,
  validated server-side (0 or 1024-65535) and applied to the live
  connection manager on save.

### Reliability & security verification

- The two dormant fakecore failure injections are now regression
  coverage: a core that never becomes ready is force-stopped with the
  executable deletable and the temp workspace removed
  (`FAKECORE_HANG`), and a mid-session crash transitions the session
  to `connection_failed` with deterministic cleanup
  (`FAKECORE_CRASH_AFTER_START`).
- Core-install asset URLs are now HTTPS-only at code level (loopback
  test authorities excepted), matching what the security
  documentation always claimed.
- The `BestCandidates` ranking path now labels every candidate with
  its source route-trust band, matching the Quick Connect chosen view.

---

## What's new in v0.9.8.6

v0.9.8.6 is a **reliability, security and hygiene release** — no new
connectivity features. It fixes the v0.9.8.5 Windows CI failure at its
root (nondeterministic session teardown), closes the stale-result and
route-trust boundaries, removes the unsafe unfinished TUN backend, and
aligns executable trust, archive handling, the HTTP proxy policy, the
Wails toolchain pair and the documentation with what the code actually
does.

### Deterministic Windows session teardown

- `engine/connection`: `stopMonitor` now JOINS the monitor goroutine
  (cancelling alone is not synchronization), the stability teardown
  completes BEFORE `connection_failed` becomes observable, and
  teardown errors are preserved as evidence instead of being dropped.
  Returning from Disconnect/Shutdown now PROVES the supervised process
  is gone — snapshots carry `core_pid` and a regression test asserts
  connect → stability failure → teardown → process gone → executable
  deletable → temp directory removable. This is the root cause of the
  v0.9.8.5 `TestConnectionStabilityDegradation` TempDir failure
  ("Access is denied": `v2ray.exe` outlived the test).
- The monitor crash path now closes the crashed instance's owned
  runtime files instead of leaking its temporary directory.

### Session generations (stale results can never poison newer sessions)

- Every session boundary (new session, disconnect, reconnect,
  shutdown, stability teardown) increments a generation. Every
  asynchronous verification — monitor recheck, connect-time probe,
  manual `VerifyConnected` — captures the generation and may apply its
  result only while that generation is current; stale successes and
  stale failures are both discarded. Deterministic tests cover
  disconnect/reconnect/shutdown during an in-flight verification.

### Route-trust boundary (public nodes are untrusted routes)

- Sources are classified `official | user | public`; every ingested
  configuration carries its source's trust band. Quick Connect / Auto
  connect through trusted routes only unless the user explicitly
  enables "Allow public untrusted routes" in Settings; public nodes
  remain fully usable through explicit selection. A public node can be
  fast + stable + verified reachable + untrusted — reliability and
  route trust are separate dimensions.
- The invalid `nirevil-vless` default (a README documentation URL with
  zero configurations; its referenced subscription paths 404) was
  removed. The pipeline now stamps each config's source identity (the
  field was previously never populated).

### TUN mode disabled (honest, not faked)

- The unfinished Wintun backend was REMOVED: its route tracking was
  inverted, DNS "restore" wrote DHCP instead of the prior state,
  Wintun was downloaded via raw curl/PowerShell with no digest
  verification, and extraction was unbounded. TUN is reported as
  experimental/unavailable on every surface, and TUN is NOT a kill
  switch — process supervision does not filter packets. A
  transactional implementation is required before it can return.

### Executable trust and bounded archives

- No remotely acquired executable may become runnable without
  AUTHORITATIVE integrity evidence: core installs are now REJECTED
  when a release publishes no digest (a locally computed SHA-256 is
  tamper evidence, never a trust anchor), and asset URLs must be HTTPS
  (loopback authorities excepted). Provider binaries keep their
  mandatory checksum gate.
- All core/provider archive extraction moved to
  `internal/safearchive`: bounded archive/total/per-file/file-count
  limits, path-traversal and absolute-path rejection, symlink/
  hardlink rejection and fail-closed malformed-archive handling, with
  zip-bomb and tar-slip test batteries.

### Explicit HTTP proxy policy

- One transport policy (`direct | environment | user URL | tunnel`)
  with `direct` as the default everywhere: ambient HTTP_PROXY/
  HTTPS_PROXY/ALL_PROXY are no longer silently inherited by the
  production client, the SSRF-guarded discovery client (where a proxy
  would bypass the dial-time destination validation) or netcheck's
  direct-path measurements. Opting in is explicit; tests cover
  proxy-assisted destination confusion.

### Wails contract, security CI and hygiene

- The Wails toolchain pair is pinned and CI-enforced: go.mod
  `wails/v3 v3.0.0-beta.19` + `@wailsio/runtime 3.0.0-beta.19`
  (the v0.9.8.5 lockfile had drifted to runtime beta.20). A bindings
  contract test verifies every hand-written `Call.ByName` target
  exists on its Go service; the embed output must regenerate clean
  (`npm run build:embed` + `git diff --exit-code`) so stale hashed
  bundles can never accumulate again — the tree carried 14 asset
  files of which only 3 were referenced.
- `security.yml` gained an allowlist-based audit of the Windows
  child-process surface (PowerShell/cmd/curl/wget/netsh/route/
  archive-extraction patterns) with every exception justified
  inline; its broken push trigger (`branches: ain]`, which never
  fired) was repaired.
- The release pipeline gained an EXPLICIT Authenticode architecture:
  when signing secrets are configured, `FreeIran.exe` and the
  installer are signed and `Get-AuthenticodeSignature`-verified in
  CI; without secrets the release ships UNSIGNED and says so in
  `SIGNING-STATUS.txt` and the release notes — signing is never
  fabricated.
- Removed: the stale `worklog.md`, the obsolete
  `cmd/freeiran/rsrc_windows_386.syso` (386 assumptions with no 386
  build), the `tools/neteval` helper and 11 stale hashed frontend
  bundles. Release history moved to `CHANGELOG.md`.

Older release notes live in [CHANGELOG.md](CHANGELOG.md).

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
              System Proxy (WinINet) — production
              TUN — experimental, DISABLED (v0.9.8.6)
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
│   ├── netcheck/          Connectivity diagnostics + [v0.9.8.1] Internet tools engine
│   ├── parser/            Multi-format configuration parser
│   ├── pipeline/          Streaming ingestion pipeline (worker pools)
│   ├── provider/          [v0.9.8.1] Provider architecture: Tor, Psiphon, cores
│   ├── scheduler/         Interval scheduler (skip-if-busy, jitter)
│   ├── source/            Source model + HTTP fetcher + collector
│   ├── store/             Chunked persistence: WAL, memtables, compaction
│   ├── tester/            Probe interface + TCP / core probes + [v0.9.8.1] latency semantics
│   ├── testqueue/         [v0.6] Bounded-worker test queue (priority, retry, cancel)
│   └── tunnel/            [v0.6] System Proxy (WinINet); TUN disabled (v0.9.8.6)
├── frontend/              TypeScript UI (Vite + React + zustand)
├── native/                C++ acceleration layer (C ABI, no deps)
├── system/                System engine: paths, processes, network, platform
├── internal/version/      Single source of truth for versioning
├── .github/workflows/     CI, release and security pipelines
├── docs/                  Architecture, storage, performance, CI, security, dev
├── CHANGELOG.md           Release history (moved out of README, v0.9.8.6)
└── VERSION                Application version (0.9.8.7)
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
  SagerNet/sing-box, over HTTPS. An install is REJECTED when a
  release publishes no authoritative digest (release-API digest or
  .dgst sidecar) — a locally computed hash is tamper evidence, never
  a trust anchor. Provider binaries (Tor, Psiphon) carry the same
  mandatory checksum gate, and every archive extraction is bounded
  (`internal/safearchive`).
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
      System Proxy (WinINet), Speed Booster, expanded
      sources with metadata, capability-driven failover** (the TUN
      mode shipped in v0.6 was DISABLED in v0.9.8.6 — its backend was
      not transactional and its Wintun acquisition was unverifiable)
- [ ] v0.7 — UI polish, source reliability dashboards, config grouping
- [ ] v1.0 — Stable releases, security review, reproducible builds

---

## Attribution & License

**Architect / Project Originator:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran

License: see [LICENSE](LICENSE).
