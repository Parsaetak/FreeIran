# FreeIran Architecture

This document describes the architecture that exists in the repository
today. Every component listed here is implemented and tested.

## 1. Layer model

```text
TypeScript UI (React + zustand + virtualized lists)
        │ generated Wails v3 bindings + events
Go application orchestration (engine/app)
        │
        ├── engine/pipeline      streaming ingestion (worker pools)
        ├── engine/store         chunked persistence (WAL + index)
        ├── engine/chunks        deterministic chunk format (FIRC)
        ├── engine/cache         bounded cache layers
        ├── engine/parser        multi-format configuration parsing
        ├── engine/source        sources + HTTP fetching + metadata
        ├── engine/tester        probe interface + TCP reachability
        ├── engine/core          protocol-core execution boundary
        ├── engine/coremgr       [v0.6] managed core install/update/rollback
        ├── engine/testqueue     [v0.6] bounded-worker test queue
        ├── engine/tunnel        [v0.6] system proxy (WinINet); TUN disabled (v0.9.8.6, experimental)
        ├── engine/provider      [v0.9.8.1] Tor/Psiphon/core provider lifecycle
        ├── engine/scheduler     interval scheduling
        ├── engine/native        optional C++ acceleration bridge
        └── system               filesystem, processes, network, platform
                                  (Windows: CREATE_NO_WINDOW + job objects)
```

Design rules:

- Boundaries are explicit: the UI never talks to Go over HTTP; the store
  never rewrites the whole dataset; the native layer is optional for
  every operation it accelerates.
- Services are grouped by responsibility (AppService, SourceService,
  DataService, StorageService, DiagnosticsService, CoreService,
  TestQueueService, TunnelService) and bound to the frontend through
  Wails. Interfaces exist where a real boundary exists (Core, Probe,
  Sink, Tester, SystemProxyBackend, TUNBackend); otherwise concrete
  types are shared.

## 2. Application lifecycle

```text
BOOT
 ↓ minimal system init (directories, layout)
 ↓ store open: metadata + index only   ← UI can bind here
 ↓ app.Start(): priority-staged background work (v0.9.9):
 │    immediate  — memory sampling, core-registry refresh,
 │                recovery watch, scheduler cadence
 │    +3 s window — storage verification, cache warm-up, the boot
 │                ingestion cycle (heavy I/O cannot compete with the
 │                first interactive connection; fully cancellable)
 ↓ UI READY (status: ready)
 ↓ scheduled source refresh (jittered interval, skip-if-busy)
 ↓ background testing (TCP reachability probe)
```

Startup never blocks on data: chunk files are read lazily, record by
record, and verified in the background. The UI observes
`loading → ready → degraded` transitions through the `freeiran:state`
event.

**UI synchronization model:** both UI event streams are published
from the authoritative transition paths — never from ticker loops.
Every meaningful mutation (boot-phase advance, degraded/healthy
transition, ingestion start/finish, shutdown, every connection
state-machine mutation) pushes its snapshot into a publisher
(`internal/statepub`) that (1) drops semantically identical
snapshots, (2) delivers every real change in publication order with
ZERO artificial delay through a bounded in-memory queue, (3) never
silently drops lifecycle-critical transitions under queue saturation
— each snapshot carries a critical/replaceable class; overflow
compaction merges same-stage pending entries and sheds replaceable
telemetry first — and (4) stops synchronously during `App.Shutdown`
after draining pending snapshots, so the final terminal state is
delivered and no emit callback can fire into a closing UI runtime.
The composition root bridges the streams into the UI runtime through
a bounded, nonblocking emitter (`statepub.BoundedEmitter`): a slow
webview event pipeline coalesces to the newest snapshot instead of
stalling the engine's publication path. The connection manager
owns its snapshot publisher (`Subscribe` registers a listener and
converges it with the current snapshot); the composition root
registers the UI bridges directly. The publishers are dedup +
ordered delivery only — they are NOT the source of truth, and there
is no periodic full-state heartbeat on the normal path.

## 3. Ingestion pipeline

Stages are connected by bounded channels; each stage runs a bounded
worker pool.

```text
sources ──▶ FETCH (4 workers) ──▶ PARSE (4 workers) ──▶ DEDUP+PERSIST
                   │                   │
            content hash            parser stats:
            change detection        rejected / duplicates counted
```

- **Backpressure**: every channel is bounded (`QueueSize`); a slow
  source applies backpressure to fetch only, never to other sources.
- **Cancellation**: contexts propagate through all stages; shutdown
  drains in-flight work.
- **Change detection**: each payload gets a 64-bit content hash; when
  unchanged since the previous cycle, parse and persistence are skipped
  entirely (reported as `unchanged`).
- **Deduplication**: fingerprinted per the canonical SHA-256 model; a
  sharded 64-bit working index keeps contention low. The full SHA-256
  remains the stored identity.
- **Persistence**: batches of 512 records cost one journal fsync.

## 4. Storage engine (engine/store)

```text
data/
├── store.meta              JSON chunk registry (atomic replace)
├── index.bin               binary fingerprint index (atomic replace)
├── chunks/NNNNNN.firc      checksummed immutable chunk files
└── wal/seg-XXXXXXXX.wal    segmented write-ahead journal
```

- **Writes** go to the WAL (one write + one fsync per batch) and the
  active memtable under a serialized writer path. When the active
  table crosses a threshold it is FROZEN into an immutable table and
  flushed by a single background worker as ONE new chunk; index and
  registry are replaced atomically; the journal is checkpointed
  (incorporated segments removed — closed before deletion, so this is
  Windows-safe). Backpressure bounds the frozen queue: writers beyond
  `maxFrozen` pending tables throttle until the worker catches up.
- **Reads** consult the memtable levels (newest first), then the
  in-memory index, then read the record directly from the chunk file
  at the indexed offset through a pinned handle from the bounded
  chunk-handle cache (the sole owner of open descriptors — eviction
  closes files, shutdown closes everything).
- **Iteration** captures a consistent point-in-time snapshot (index
  references + immutable memtable slice headers) and streams chunk
  records through pinned handles; concurrent writers cannot make a
  scan skip, duplicate or resurrect records.
- **Integrity**: chunk payloads carry CRC-32; the journal is
  CRC-per-record; a corrupt or truncated journal tail is discarded
  safely; a continuity break between segments fails the open loudly;
  missing/corrupt meta or index triggers deterministic rebuilds from
  chunk files; orphan chunk files (crash between chunk write and meta
  persist) are removed on open and recovered from the WAL.
- **Compaction**: chunks whose dead-record ratio exceeds 50% are
  rewritten (merged) with only live records; fully dead chunks are
  deleted. Victim files are removed only after their cached handles
  are closed and active scans have drained — never while open. Index
  swaps are compare-and-swap against the snapshotted locations, so
  concurrent deletes can never be resurrected by a compaction racing
  them. Dead-record accounting drives the garbage-ratio stat.
- **Key contract**: keys are lowercase hex SHA-256 fingerprints
  (64 chars), stored as 32 raw bytes. Chunk records are
  `[32-byte key][value]` pairs so the index can always be rebuilt.
- **Lifecycle**: every public operation registers in a barrier;
  `Close()` rejects new work, drains active operations, flushes,
  closes the journal and every cached handle — after it returns the
  store owns no file descriptor, so the store directory is deletable
  immediately on every platform (including Windows).

See docs/storage-format.md for the byte-level formats and the full
resource-ownership model.

## 5. Cache architecture

| Layer | Type | Purpose | Bounds |
|-------|------|---------|--------|
| source cache | LRU+TTL | fetched payloads (resilience, re-parse) | 128 entries / 64 MiB / 6h |
| hot configuration cache | LRU+TTL | decoded configs for UI pages | 4096 entries / 30 min |
| chunk-handle LRU | LRU | open chunk files | 32 handles |
| dedup shards | sharded maps | 64-bit working fingerprint index | 16 shards |

All layers track hits, misses, evictions and hit rate; stale
generations are invalidated rather than served.

## 6. Native acceleration (optional)

Three operations are accelerated behind a stable C ABI
(`native/include/freeiran.h`):

1. `fir_hash64_batch` — batch FNV-1a 64 hashing (dedup prefilter)
2. `fir_crc32` — CRC-32 (IEEE) for chunk checksums
3. `fir_scan_urls` — byte-offset scan of configuration URL lines in
   large text payloads

The Go fallbacks are bit-identical and verified against the same
reference vectors. Builds without the `native_accel` tag never link the
C++ code; builds with it verify the ABI version at startup and fall
back if it mismatches. `FREEIRAN_NATIVE=off` disables the native path
at runtime, recorded in metrics as `native_fallback_hits`.

## 7. System engine

`system/` provides the local integration surface: application directory
layout, atomic file writes, process lifecycle for protocol cores
(polite stop → forced kill, output capture through caller-supplied
writers, redaction), core binary discovery (`xray`, `v2ray`,
`sing-box`, `wireguard`) with multi-form version probes
(`--version`, `-version`, `version` — the cores disagree), and
reachability probing. Platform differences live in
`process_unix.go` / `process_windows.go` / `paths_*`.

**Process supervision (v0.8.0).** The supervision invariant — a
protocol core must never become an unmanaged/orphaned process — is
enforced through a deterministic lifecycle state machine
(`running → stopping → stopped / exited / cancelled`) plus a
platform kill domain:

- **Windows, tier 1** — spawn, then `AssignProcessToJobObject` into a
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` job. Windows 8+ nests job
  hierarchies, so this succeeds even when the parent (a CI runner
  agent, a shell sandbox) already runs inside a job.
- **Windows, tier 2** — when assignment is denied with
  `ERROR_ACCESS_DENIED` (pre-Windows-8 semantics or a non-nestable
  hierarchy) and the child is still alive, the child is terminated
  and respawned once with `CREATE_BREAKAWAY_FROM_JOB`, then assigned.
- **Windows, tier 3** — supervised fallback: the launch succeeds
  without a job; Stop performs a Toolhelp32 process-tree
  termination, the degradation is logged and exposed through
  `JobBound()` / `Diagnostics()`, and the launch is never silently
  weakened.
- **Unix** — the child is a process-group leader (setpgid); Stop
  SIGTERMs the group, waits the grace period, then SIGKILLs it.

Every exit path — natural, cancelled, stopped — additionally reaps
the whole kill domain (job close on Windows-bound, group SIGKILL on
Unix, tree kill in fallback), so a grandchild that ignores the
polite signal cannot outlive the supervisor. Win32 calls are
validated by their BOOL/HANDLE return values; `GetLastError()` is
only consulted as a diagnostic on genuine failure (the v0.7 CI
failure was exactly a stale-Last-error misread of a successful
`SetInformationJobObject`). Executable resolution for system shell
binaries goes through `%COMSPEC%` with a validated
`%SystemRoot%\System32\cmd.exe` fallback — never the working
directory or inherited PATH — while protocol-core paths keep strict
explicit `os.Stat` validation.

## 8. Protocol cores (engine/core)

The engine does not implement VPN protocols. `engine/core` is the
execution boundary: a backend abstraction, a capability model, a
registry with executable discovery, deterministic backend selection
and the supervised process lifecycle shared by every backend.

```text
                        FREEIRAN
                           │
                      WAILS v3 UI
                           │
                    TypeScript / Go
                           │
                    Core Manager (engine/core)
                           │
          ┌────────────────┼─────────────────┐
          │                │                 │
       Xray Core       V2Ray Core       sing-box
     (core/xray)      (core/v2ray)    (core/singbox)
          │                │                 │
          └────────────────┼─────────────────┘
                           │
                  Normalized Config
                           │
              Connection Manager (engine/connection)
                           │
                Tester / Selector (engine/tester)
                           │
                    Local Connection
```

**Backends** (`core.Core` interface): `Name`, `Supports`,
`Validate`, `BuildConfig`, `Start`. Three adapters ship in v0.4:

| Backend | Package | Config dialect | Distinct capabilities |
|---------|---------|----------------|----------------------|
| Xray | `engine/core/xray` | V4 JSON | REALITY, xtls-rprx-vision, XHTTP (plain QUIC/H2 removed upstream) |
| V2Ray | `engine/core/v2ray` | V4 JSON | classic transports incl. QUIC + HTTP/2; no REALITY/flow |
| sing-box | `engine/core/singbox` | native JSON | REALITY + vision via tls/utls; mixed inbound; Hysteria2/TUIC reserved for v0.5 |

V2Ray (V2Fly) and Xray share the V4 JSON lineage — the Xray adapter
extends the shared generator (`v2ray.BuildV4Document`) with
Xray-only branches, so the two dialects can never drift apart.

**Capability model** (`core.Capabilities`): each backend declares
protocols, transports, securities, flows and TLS-mandatory protocols.
Declarations are verified against the pinned real binaries
(docs/development.md); the contract suite enforces them. Capability
resolution — not scattered protocol checks — decides support
everywhere in the application.

**Registry** (`core.Registry`): registration (priority-ordered),
executable discovery through `system.CoreLocator` (v0.9.14: managed
cores directories → PATH → bounded known platform installation
locations, all through ONE shared discovery authority with a
file-identity probe cache and in-flight probe deduplication), provenance
reporting (origin: managed/path/system, ownership: managed/external),
availability state (available / missing / invalid) and version
reporting. Refresh is background work and cache-aware; an explicit
user refresh bypasses the freshness windows (`RefreshForce`). The app
boots with zero cores installed and reports them as missing. See
[reuse.md](reuse.md) for the authoritative reuse/freshness policy.

**Selection** (`core.Select`): deterministic and explainable —
compatible candidates are ordered by user preference (when compatible
AND available), then registry priority, then name; the winner carries
a human-readable reason and the ordered fallback list. REALITY
configs resolve to Xray or sing-box; QUIC/H2 configs resolve to V2Ray
or sing-box; no compatible backend produces an error naming every
considered backend.

**Process lifecycle** (`core.Instance`): explicit states —
`created → starting → running → stopping → stopped` with error states
`start_failed / crashed / unhealthy / timed_out`. The shared launcher
writes the generated runtime configuration into a 0600 file inside a
0700 temporary directory and spawns the core with redacting output
capture. Readiness is ONE authoritative supervision path (v0.9.9):
`awaitListener` probes the local listener on a bounded adaptive
schedule (immediate probe, then a short ramp to a 100 ms cadence),
watches the process for early death through a single wait observer,
and publishes the verdict exactly once; the startup timeout's bounded
teardown lives there too. The inbound port is resolved exactly once,
BEFORE configuration generation, through `core.ResolveInboundPort`
(explicit ports pass through; ephemeral ports allocate once). Close
stops the process FIRST (Windows file-lock discipline), then removes
the temporary files with bounded retry.

**Health** (`core.HealthReport`): process health and network health
are separate axes — `process_alive` + `listener_ready` + latency.
A living process never implies a working tunnel.

**Connection manager** (`engine/connection`): one active session,
explicit state machine — `disconnected → selecting → preparing →
starting_core → waiting_for_ready → connected → disconnecting`, with
`connection_failed` as the failure state. No contradictory booleans:
the state string is the single source of truth. Bounded fallback
tries the selection's fallback backends (default max 3 attempts) and
records every attempt with its failure reason.

**Secret redaction**: credential material (UUID, passwords, keys)
never reaches logs, diagnostics, selection reasons or UI snapshots.
`config.DisplayURL()` renders `vless://***@host:443`; the log capture
buffer redacts both explicit secret values and URL userinfo patterns
before storing a line. The configuration details view exposes
presence flags (`has_uuid`, `has_password`), never the values.

## 9. Runtime logging (internal/logging)

A persistent, structured runtime log records errors, warnings and
important lifecycle events for every subsystem. It lives in the
platform application-data directory (`<BaseDir>/logs/freeiran.log`,
never inside the repository).

```text
entry shape:  {seq, ts(RFC3339 UTC), level, subsystem, event,
               message, operation?, error_kind?}
file format:  JSON lines (machine-readable)
rotation:     size-based (default 5 MiB), 4 numbered backups,
              sequential-rename shift, startup recovery of an
              interrupted rotation
redaction:    applied to EVERY entry before storage/broadcast —
              UUIDs, protocol URLs (vless/vmess/trojan/ss/...),
              password/token/key parameters and caller-registered
              secret values are replaced with [REDACTED]
consumers:    engine packages log through a process-wide no-op-safe
              global (logging.E/W/Err/D); the desktop app installs
              the real logger at boot and closes it last
UI surface:   DiagnosticsService + LogService bindings expose an
              incremental, bounded read (Recent by sequence number),
              so the log viewer streams without ever transferring
              the whole file
```

Logged lifecycle events include `application_start`, `application_ready`,
`store_open`, `store_error`, `migration_start/success/error`,
`source_refresh_start/success/error`, `core_discovered`, `core_start`,
`core_ready`, `core_exit`, `core_error`, `connection_start/success/failure`,
`disconnect_start/success`, `shutdown_start`, `shutdown_complete` and
`settings_updated`. Protocol-core stdout/stderr is captured through the
existing redacting `LogBuffer` (engine/core/logs.go) and never appended
raw.

## 10. Error model

Every subsystem reports structured errors (`engine/errors`): kind
(recoverable, retryable, invalid_input, configuration, environment,
dependency_unavailable, corrupt_data, fatal), subsystem, operation and
cause. Kinds drive UI presentation and retry policy; causes never carry
credentials.

---

## v0.6.0 additions

### Managed Core Manager (`engine/coremgr`)

A dedicated subsystem responsible for install, discover, inspect,
verify, update, rollback, enable/disable, remove, health-check and
version reporting for Xray, V2Ray and sing-box.

**Directory layout** (relative to the workspace root — see
docs/workspace.md; the pre-v0.9.2 %AppData% path is history):

```
<workspace>/cores/
  xray/
    bin/xray.exe              ← active executable
    bin/xray.exe.previous     ← rollback target (last healthy)
    manifest.json             ← version, source URL, SHA-256, dates, state
    staging/                  ← download + unpack area (transient)
  v2ray/...
  sing-box/...
```

**Install pipeline:**

```
1. resolve latest release for the configured channel
2. download asset into staging
3. compute SHA-256; verify against published .dgst (Xray/V2Ray)
4. unpack archive (zip / tar.gz)
5. locate the executable inside the unpacked tree
6. query version (xray version / v2ray version / sing-box version)
7. validate executable accepts minimal SOCKS inbound config
8. retain current binary as rollback target
9. atomic rename staged → active
10. smoke test: launch + listener readiness + clean shutdown
11. mark StateReady (or StateBroken on any step failing)
```

**States:** `not_installed → installing → installed → checking →
ready / broken / update_available / disabled`

**Channels:** `stable` (default) and `prerelease` (opt-in). Stable
queries `/releases/latest`; prerelease queries `/releases` and
includes prereleases.

### Test Queue (`engine/testqueue`)

A bounded-worker, priority-ordered, cancellable scheduler for
testing configurations.

**Task states:** `queued → preparing → testing → measuring →
passed / failed / timed_out / cancelled`

**Modes:** `Quick | Balanced | Deep | Re-test failed | Test all |
Test selected | Continuous`. Each presets Concurrency, Timeout,
MaxAttempts, Measurements.

**Features:**
- Bounded worker pool (default 4 workers)
- Priority queue (newly discovered configs and user-selected tests
  jump ahead of bulk re-tests)
- Duplicate fingerprint suppression
- Per-config timeout + global test timeout
- Exponential-backoff retry (cap = MaxAttempts)
- Cancellation by task ID, by source, or globally
- Graceful shutdown (Stop drains in-flight tests)
- Live stats: tests/sec, queue depth, active workers, per-backend
  counts, average duration
- Failure categories: `network / auth / protocol / config /
  backend_down / backend_reject / timeout / cancelled / unknown`

### System Proxy (`engine/tunnel`)

A proper Windows system-proxy integration through **WinINet's
per-connection options** (INTERNET_OPTION_PER_CONNECTION_OPTION),
NOT direct registry edits.

**Pipeline:**
1. Save current per-connection proxy settings
   (PROXY_TYPE_FLAGS, PROXY_SERVER, PROXY_BYPASS)
2. Set new proxy: `socks=host:port` (or `http=host:port`)
3. Set bypass list (semicolon-separated)
4. Broadcast `INTERNET_OPTION_SETTINGS_CHANGED` +
   `INTERNET_OPTION_REFRESH` so running apps refresh

**Disable:** restore the previously saved settings + broadcast
refresh.

### TUN mode (`engine/tunnel`)

A real Windows TUN interface backed by **Wintun** (the official
maintained driver from wintun.net).

**Install pipeline:**
1. Download `wintun-0.14.1.zip` from `wintun.net/builds/`
2. Extract `wintun/bin/<arch>/wintun.dll` to
   `<AppData>/FreeIran/cores/wintun/wintun.dll`
3. LoadLibrary + resolve `WintunCreateAdapter`, `WintunCloseAdapter`

**Enable pipeline:**
1. `WintunCreateAdapter("FreeIran", "FreeIran")`
2. `netsh interface ipv4 set address name=Freeiran static
   10.211.211.1 255.255.255.0`
3. `route add 0.0.0.0/1 10.211.211.1` + `route add 128.0.0.0/1
   10.211.211.1`
4. `netsh interface ipv4 set dnsservers name=Freeiran static
   1.1.1.1 primary`

**Disable pipeline:**
1. `route delete 0.0.0.0/1` + `route delete 128.0.0.0/1`
2. `WintunCloseAdapter(handle)`
3. `netsh interface ipv4 set dnsservers name=Freeiran source dhcp`

All operations require elevation.

### Windows process launch (`system/process_windows.go`)

`launchProcess` now sets `SysProcAttr` with:
- `HideWindow = true`
- `CreationFlags = CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP |
  DETACHED_PROCESS`

Each spawned protocol core is bound to a kill-on-close Windows job
object (`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`). An abnormal FreeIran
exit (crash, Task Manager kill, OS shutdown) reaps every spawned
core at the kernel level — no orphan process survives.

### Source registry expansion

Every default source carries full metadata (provider, project,
protocol hints, region, format, priority, refresh interval). Three
new high-quality sources added:
- `shadowsocks-aggregator-eternity` (mahdibland)
- `mahsa-free-config-mtn` (mahsanet)
- `scrape-and-categorize-netherlands` (10ium)

The fetcher now supports conditional requests (ETag, Last-Modified)
and content-hash short-circuit. An unchanged source skips parse +
persistence entirely.

### Capability-driven backend selection

Configuration → backend selection now follows:

```
configuration capability → preferred core → installed healthy cores →
priority → health → recent success
```

If the selected core fails, the manager records the failure, classifies
it (network / auth / protocol / config / backend_down / timeout /
cancelled / unknown) and tries the next compatible installed core.
Incompatible cores are never retried for the same configuration.

## v0.8.0 additions

### Unified adaptive memory controller (Memory Booster 2.0)

`engine/app/memoryservice.go` composes the v0.7 `mempressure` and
`booster` libraries into the running application (previously they
were standalone, unwired). One sampler goroutine pulls real
measurements every 2 s — cache-layer bytes, `testqueue` memory
estimate, store memtable + WAL bytes, Go heap, RSS, GC CPU fraction
— classifies the pressure state, and mirrors it into the metrics
registry. A booster tick (5 s) adapts settings within hard
floor/ceiling limits with hysteresis; changes are applied live
through the new dynamic knobs:

- `testqueue.Queue.SetConcurrency(n)` — grows the worker pool
  immediately; shrinkage retires idle workers through a resize wake
  (busy workers finish their current task; no task is ever dropped).
- `testqueue.Queue.SetMaxQueueSize(n)` — queue-depth shedding.
- `cache.Layer.SetMaxEntries(n)` — cache target shrink/grow with
  immediate oldest-first eviction.

Hard pressure reactions: High clears the hot-config cache; Critical
clears both cache layers and requests a GC. Every transition and
adjustment is logged; the Diagnostics UI surfaces the whole picture
(pressure state, measurements, adaptive settings) through
`DiagnosticsService.Memory()`.

### Desktop service wiring

The v0.6 `CoreService`, `TestQueueService` and `TunnelService`
existed in the Go backend but were never registered with the Wails
runtime. v0.8 registers all three in `cmd/freeiran/main.go`, ships
frontend bindings (`coreservice.js`, `testqueueservice.js`,
`tunnelservice.js`, plus `DiagnosticsService.Memory`), and adds the
system-integration (System Proxy / TUN) controls and live
memory/test-queue panels to the UI.

### Observability

`metrics.Registry` gains live gauges (`memory_pressure`, `rss_bytes`,
`heap_alloc_bytes`, `heap_live_bytes`, `gc_cpu_pct`) mirrored from
the controller's samples, so the engine metrics snapshot and the
memory diagnostics page always agree on one classification.

## v0.9.0 additions — core reliability, network diagnostics, deployment

### Core manager ↔ registry integration

The Managed Core Manager now boots eagerly with the application
(`app.New`) because its install locations are discovery inputs: every
managed core lives in `<cores>/<core>/bin/<core>.exe`, and those bin
directories are passed to the registry's `system.CoreLocator` as
extra search roots. Lifecycle actions (install, update, repair,
reinstall, rollback, remove) re-run `app.RefreshCores`, so Connect,
Backends and the tester observe changes immediately — a successful
install is usable without restarting the application.

The install pipeline hardened: staged downloads keep the asset's
filename (with content-sniffing fallback) so archive formats always
resolve; the executable bit is set before any probe; the probed
version is sanity-checked against the release tag; failure states
record a human-readable `FailureReason` + `FailureStage`; byte-level
download progress and per-stage events (`InstallProgress`) stream to
the UI via the `freeiran:coreprogress` event. All manager-spawned
children (version probe, config validation, smoke test) launch with
`CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP` on Windows, and the
smoke-test shutdown uses Windows-safe termination semantics.

`Repair` no longer re-enters the per-core mutex (the v0.8
implementation deadlocked on every call), and the manager gained
`UpdateAll` and `Reinstall`.

### engine/netcheck + engine/socks5

`engine/netcheck` implements the manual connectivity diagnostics:
concurrent, individually timeout-bounded probes across four classes
(local links, DNS via dedicated resolvers, raw TCP, HTTPS) with a
classified seven-state report (`no_internet`, `dns_failure`,
`https_failure`, `high_latency`, `ok`, `proxy_only`,
`core_no_internet`). When a session is connected, an additional probe
runs through the core's local SOCKS listener, distinguishing
proxy-only connectivity from a core whose outbound path is dead.

`engine/socks5` is a minimal RFC 1928 no-auth CONNECT dialer shared
by netcheck and the tester.

### Testing model

`engine/tester` composes probes through `ChainedProbe`: the core
probe (real backend execution with `EndToEnd` verification — a real
SOCKS5 CONNECT round-trip plus a 204 fetch through the generated
tunnel) is primary; the TCP reachability probe is the no-core
fallback. Results carry backend, protocol, endpoint, ping,
duration and a quality band, and the app layer persists them into
the configuration records, so the UI displays the latest verdict
without retesting. `engine/testqueue` aggregates average, fastest
and slowest measured latency for the live progress panel.

### Portable deployment

`system.WorkspaceRoot()` — the single path authority since v0.9.2, as
documented in `docs/workspace.md` — resolves every deployment style
through one model: `$FREEIRAN_HOME` wins when set, `installed.marker`
relocates the workspace to the per-user application-data directory,
and every other deployment keeps the executable directory.
`system.DefaultBaseDir()` and `system.CacheBaseDir()` are thin aliases
of that root (never split per-user resolvers), so the official
release ZIP — a full directory tree with `config/ data/ logs/ cache/
cores/ runtime/ docs/ deployment/` and metadata — keeps all state
inside the extracted folder. `portable.marker` and the diagnostic
helpers `PortableMode()`/`InstalledMode()` only label the deployment
style; they never influence path resolution. The release workflow
validates the package (required files, directories, VERSION and
metadata consistency, single archive root) before publishing.

## v0.9.6 additions — discovery pipeline, test modes, measured ranking, verified connections

### engine/discovery — the multi-level discovery engine

The ingestion pipeline of §3 remains the classic background refresh
path. `engine/discovery` adds the interactive, strategy-driven
discovery engine that the adaptive start flow drives:

```text
DISCOVER → INGEST → PARSE → NORMALIZE → DEDUPLICATE → VALIDATE
     (TEST → SCORE → RANK → SELECT live downstream in
      engine/tester, engine/ranking and engine/connection)
```

Discovery levels, cheapest and most trusted first:

| Level | Name            | Source of candidates                                   |
|-------|-----------------|--------------------------------------------------------|
| 1     | cached          | the local store's pool (no network)                     |
| 2     | configured      | the user's source list                                  |
| 3     | trusted_public  | the built-in verified registry                          |
| 4     | search          | smart GitHub repository search + raw-path probing       |
| 5     | content         | references inside valid fetched content                 |
| 6     | recovery        | emergency re-discovery on connection failure            |
| 7     | deep            | restrictive-network escalation (more queries, min yield 1) |

Guarantees: bounded fetch concurrency (6 default); per-source failure
isolation; per-level statistics; target-satisfaction skipping (later
levels only run when the pool is still short, unless ForceAll); and
full context cancellation. The engine emits real `Progress` events at
stage boundaries — parsing, validating, complete — never a fabricated
animation.

**Smart search** (`engine/discovery/search.go`) queries GitHub's
repository search with protocol-appropriate terms, orders results by
freshness (30-day staleness demotion) then stars, and probes a
bounded set of conventional raw file paths
(`raw.githubusercontent.com` ONLY — HTML blob pages are never
scraped). Budgets: 3 queries, 12 repositories and 24 probes per
cycle; a 403/429 backs the searcher off 10 minutes; results cache
6 hours per query. A probe is promoted to a source only when its
content parses into ≥3 valid candidates (≥1 in deep mode).

**Source intelligence** (`engine/discovery/health.go`) tracks
availability, parse success, valid yield, duplicate rate, latency,
freshness and consecutive failures per source; fetches are issued
healthiest-first, and failures drive an exponential backoff capped
at six hours — never a permanent blacklist. State persists to
`config/discovery-health.json` (corrupt files are discarded).

**Canonical node model**: the ONE internal representation remains
`config.Config` (protocol-specific parsing stays in `engine/parser`);
v0.9.6 extends it with the measurement fields below.
`discovery.Node` is the pipeline-stage view: Config plus provenance
(level, source identity, discovery timestamp).

### Test modes — engine/tester (ping.go, urltest.go, modes.go)

`PingProbe` measures endpoint latency over repeated TCP handshake
samples (default 4, 3 s timeout, 120 ms spacing): min/median/avg/max,
jitter (mean absolute deviation of consecutive samples), packet loss,
failure and timeout counts. TCP, not ICMP: ICMP echo needs raw
sockets/privileges everywhere we run, and the TCP SYN/SYN-ACK RTT to
the candidate's own port measures the exact path the production
protocol uses. The UI labels the number a TCP ping accordingly.

`URLTester` issues a real HTTP request through the candidate's tunnel
(a `socks5.Dialer` bound to a running core instance) with an
`httptrace` phase breakdown: DNS (local phase; for proxied requests
the proxy resolves remotely — documented, its cost sits in the
connect phase), connect (the SOCKS CONNECT round trip), TLS, TTFB,
total, status code, response bytes, timeout classification. The
transport is disposable (`DisableKeepAlives`) so measurements never
ride a warmed-up connection.

`ModeTester` composes five user-selectable modes —
`ping | url | ping_url | handshake | full` — with per-mode verdicts:
a candidate with a brilliant ping and a failed URL test is NOT
working in `ping_url` mode; both facts stay visible. Outcomes land in
the canonical Config fields (`Ping`, `URLTest`, `Handshake`,
`LastSuccessAt`, `FailureStreak`, `LastFailureReason`) plus one
bounded history observation, so ranking stays grounded in real
outcomes.

### Measured ranking — engine/ranking/scores.go

Alongside the classic `Evaluate`/`Rank` surface, every candidate
scores on SEPARATE dimensions: `PingScore`, `URLScore`,
`StabilityScore`, `SuccessScore`, `FreshnessScore`, `SourceScore`,
`CompatibilityScore`, and their weighted composite `OverallScore` —
usable connectivity (URL + success) dominates the composite, so a
fast-but-broken candidate cannot outrank a slower-working one. Every
latency number carries provenance — `measured`, `estimated`,
`unavailable` or `stale` (30-minute window) — and the ping sort modes
partition measured-first, so estimated numbers are never displayed or
ordered as measured pings. Nine sort modes (Best Overall through
Recently Verified) with deterministic fingerprint tie-breaks.

### Verified connections and racing — engine/connection (verify.go, racing.go)

`VerifyTunnel` proves USABLE connectivity through an active tunnel
with a bounded real HTTP request, classifying failures on evidence:
`timeout`, `refused`, `reset`, `tls`, `http_status`,
`proxy_handshake`, `core`. `Manager.VerifyConnected` exposes it for
the active session and records the result; verification failure does
not itself tear the session down — that decision belongs to the
bounded recovery policy, which consumes the failure class.

`Race` is the controlled racing capability: 2–4 top candidates each
get a temporary core instance; the first candidate whose tunnel is
VERIFIED USABLE wins; remaining racers are cancelled cleanly and
every instance is closed deterministically. The winner's
configuration is returned for the definitive `Manager.Connect`, so
the production session lifecycle stays single-owner. Racing is
opt-in (Settings), resource-bounded and fully cancellation-safe.

### Environment intelligence — engine/netcheck/environment.go

`EnvironmentAnalyzer` probes independent HTTPS endpoints
(204-style), a plain-HTTP captive-portal probe, DNS and latency
spread, producing evidence-based signals: `direct_ok`,
`dns_failure`, `http_failure`, `tls_failure`, `repeated_timeout`
(consecutive analyses), `captive_portal`, `proxy_environment`
(HTTP(S)_PROXY/ALL_PROXY), `unstable_connectivity`,
`restricted_access`. A restricted environment advises deep discovery;
a captive portal reports the sign-in problem instead (discovery
cannot fix an unauthenticated link). Deliberately NOT claimed: QUIC
availability (no stdlib QUIC client — QUIC-family transports belong
to the protocol cores) and censorship certainty (signals describe
observed failures, not causes).

### The adaptive start flow — engine/app/discoveryservice.go

```text
START → DETECT → DISCOVER → TEST → RANK → CONNECT → VERIFY → MONITOR
        (env)    (levels)   (mode)  (sort)  (race/seq)  (HTTP)  (recovery)
```

`DiscoveryService.RunStartFlow` runs the chain with real stage events
(`freeiran:startflow`: stage, message, measured duration) and a
result summary (discovered/valid/duplicates/tested/verified/duration).
Manual selection always overrides automatic selection. The service
also exposes `DiscoverNow` (manual discovery pass), `SourceHealthList`
(source intelligence for the UI) and the environment analysis.

## Providers and Internet tools (v0.9.8.1)

### engine/provider — the unified provider architecture

ONE lifecycle contract (`Provider`: Name/Kind/Resolve/Install/
Uninstall/Start/Stop/State/Info/Endpoints/Health/Cleanup) + a
`Manager` for every executable that can provide connectivity. Kinds:
`core` (xray/v2ray/sing-box via a thin `CoreProviderAdapter` over the
SAME `engine/coremgr` pipeline — no duplicate install machinery;
provider-level Start is honestly unsupported for cores, which run per
node-configuration through the connection engine), `tor` and
`psiphon`. Tor and Psiphon share one managed-binary pipeline
(`binary.go`: resumable `.part` download via `internal/httpx` →
MANDATORY SHA-256 verification → tar-slip-guarded unpack → version
validation → supervised smoke launch → atomic activate with rollback
→ manifest) under `<workspace>/providers/<name>/`, with runtime
data/logs confined to the workspace and log pruning by `Cleanup`.
One managed instance per provider; every process runs under
`system.ManagedProcess` (job objects — no orphans).

Provider routes run through the SAME connection lifecycle
(`engine/connection/provider.go`, `Manager.ConnectProvider`): select →
start → route via the local SOCKS endpoint → `VerifyTunnel` (no
bypassing) → connected → the SAME monitor loop. Auto mode
(`engine/app/providerservice.go`) chooses between configurations and
providers on measured evidence (installed availability, live health,
verified-success freshness, latency, failure stability, ranking
composite) with no hardcoded priority; choices are explainable.

### engine/netcheck — the Internet-tools engine

The diagnostics package gains the shared user-triggered tools engine
(§5): fifteen tools (internet, dns, tcp, tls, https, http_connect,
socks5, websocket, udp, quic — honestly unsupported, traceroute —
privilege-gated, path_mtu — DNS-ladder capped ~300 B, captive_portal,
public_ip, tunnel_diagnostics) with one structured result contract,
timeouts clamped [1 s, 60 s], and the `toolsafety.go` policy (scheme
allowlist, no credentials in URLs, private-range blocking,
DNS-rebinding guard, redirect cap 3, response cap 256 KiB,
user-triggered ONLY, bounded concurrency of 3 in the app service).

### Latency measurement semantics

`engine/tester/latency.go` defines the canonical representation
(rules R1–R6) that fixed the Windows CI failure (run 35287863799):
the true duration is preserved; successful raw ≤ 0 readings quantize
to `ClockFloor`; `Result.Measured` is the authoritative flag; 0 ms +
measured = sub-millisecond, rendered "< 1 ms"; sub-ms sorts first
among measured. Ranking, scores, ping metrics, connection modes,
testqueue statistics, core health, netcheck probes and the Quick
Connect picker all honour the contract.

Full details: docs/providers.md, docs/internet-tools.md and
docs/latency.md.

## v0.11.0 additions — failure classification, route freshness, transport agility, ECH

### Failure classification (evidence, not taxonomy theater)

Every failed test/verification is classified into a NINE-class stable
vocabulary (`engine/config`): `dns`, `tcp`, `tls`, `handshake`,
`listener`, `verify`, `reset`, `timeout`, `transport`. The class is
DERIVED from observations, in precedence order
(`engine/tester.ClassifyOutcomeFailure`):

1. the failed-facet SHAPE (handshake metrics failed → the core never
   became ready; ping facet all-timeout → path blackholing);
2. the URL-test's own canonical vocabulary (Timeout flag → timeout;
   "HTTP <code> via tunnel" → the tunnel forwarded but the response
   was not usable → verify);
3. the classified error TEXT through `config.ClassifyFailure`
   (ordered needles; TLS outranks handshake — "tls: handshake
   failure" is a TLS alert — and timeout matches LAST).

The URL-test phase TIMINGS are deliberately NOT failure evidence:
DNSMS is 0 for proxied requests, TLSMS is −1 for plain-HTTP targets
regardless of success, and ConnectMS is measured even on dial failure
— none of them marks the failing phase. Unclassifiable evidence stays
`""` rather than guessing a class.

### Route freshness / evidence tuple

Per configuration, the bounded observed tuple
(`config.Config.Evidence`): `last_verified_at` (LastSuccessAt),
`last_failure_at`, `recent_failure_class`, `verification_age`
(distance to whichever of the two is more recent). All values are
actual observations; zero means "never observed", never "unknown".
These surface in the ranking scores (both surfaces), the connection
detail view, and the `timed_out` scope filter (class first, legacy
free-text fallback).

### Transport agility — preference, not a second failover engine

Repeated evidence of a PROTOCOL-SPECIFIC failure (a trailing streak
of ≥2 failures all sharing class `tls`, `handshake` or `transport`)
demotes a candidate in BOTH ranking surfaces
(`engine/ranking.Evaluate` and `EvaluateMetrics`) by a bounded factor
(2 ×0.85, 3 ×0.70, ≥4 ×0.55) with an explicit explanation line
("last N tests failed with X-class failures — preferring other
transports"). Quick Connect, recovery and discovery selection all
flow through this scoring, so after repeated protocol-specific
failures the system NATURALLY prefers a different already-supported
transport/configuration. There is no parallel failover engine, no
confidence score, and the demotion never zeroes a candidate (manual,
explicit selection still works). Mixed/unattributed trailing classes
do NOT demote — evidence that does not point at one layer must not
drive transport preference.

### Encrypted Client Hello

ECH is modeled with four dedicated fields mapped 1:1 onto sing-box
1.14's `tls.ech` object, capability-routed to sing-box ONLY (Xray's
`echConfigList` exists in schema but is not content-validated at
check level; V2Ray 5.53.0 ignores it — neither is claimable). The
full evidence table, validation rules and the honest evidence scope
(schema-level, not live ECH negotiation) are in docs/protocols.md.

### Android strategy

Android is an engineered PLAN, not a feature: see docs/android.md for
the implementation-ready architecture note (shared Go engine,
platform tunnel boundary via VPNService with the same transactional
ownership contract as WinINet, UI bridge, pinned per-ABI core
distribution, verification and permission requirements). The desktop
TUN experiment remains EXPERIMENTAL/DISABLED — an Android TUN must be
a new transactional implementation that passes real rollback/recovery
tests, never a re-enable of the old code.
