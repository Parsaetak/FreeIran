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
(polite stop → forced kill, stdout/stderr draining, redaction),
core binary discovery (`xray`, `sing-box`, `wireguard`) with version
queries, and reachability probing. Platform differences live in
`process_unix.go` / `process_windows.go` / `paths_*`.

## 8. Protocol core strategy

The engine does not implement VPN protocols. `engine/core` defines the
execution boundary (registry per protocol type); the system engine
manages external cores (Xray, sing-box, WireGuard) with controlled
lifecycle, version discovery and health checks. Downloaded
configurations remain untrusted input at every boundary.

## 9. Error model

Every subsystem reports structured errors (`engine/errors`): kind
(recoverable, retryable, invalid_input, configuration, environment,
dependency_unavailable, corrupt_data, fatal), subsystem, operation and
cause. Kinds drive UI presentation and retry policy; causes never carry
credentials.
