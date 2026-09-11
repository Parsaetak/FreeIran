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
        ├── engine/source        sources + HTTP fetching
        ├── engine/tester        probe interface + TCP reachability
        ├── engine/core          protocol-core execution boundary
        ├── engine/scheduler     interval scheduling
        ├── engine/native        optional C++ acceleration bridge
        └── system               filesystem, processes, network, platform
```

Design rules:

- Boundaries are explicit: the UI never talks to Go over HTTP; the store
  never rewrites the whole dataset; the native layer is optional for
  every operation it accelerates.
- Services are grouped by responsibility (AppService, SourceService,
  DataService, StorageService, DiagnosticsService) and bound to the
  frontend through Wails. Interfaces exist where a real boundary exists
  (Core, Probe, Sink); otherwise concrete types are shared.

## 2. Application lifecycle

```text
BOOT
 ↓ minimal system init (directories, layout)
 ↓ store open: metadata + index only   ← UI can bind here
 ↓ app.Start(): scheduler + background verification + cache warm-up
 ↓ UI READY (status: ready)
 ↓ scheduled source refresh (jittered interval, skip-if-busy)
 ↓ background testing (TCP reachability probe)
```

Startup never blocks on data: chunk files are read lazily, record by
record, and verified in the background. The UI observes
`loading → ready → degraded` transitions through the `freeiran:state`
event.

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
executable discovery through `system.CoreLocator` (managed cores
directory → PATH), availability state (available / missing /
invalid), version reporting. Refresh is background work; the app
boots with zero cores installed and reports them as missing.

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
0700 temporary directory, spawns the core with redacting output
capture, and polls the local listener for readiness. Close stops the
process FIRST (Windows file-lock discipline), then removes the
temporary files with bounded retry.

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
