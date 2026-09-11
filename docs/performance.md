# FreeIran Performance Architecture

Performance work follows one rule: correctness first, then measurability,
then architecture, then optimisation. Every claim below is backed by a
benchmark in the repository (`go test -bench`). Where the v0.3.0 storage
lifecycle rework made something slower, that is documented too — the
numbers below are measurements, not marketing.

## 1. What was wrong in v0.1 (baseline)

| Area | v0.1 behaviour | Cost |
|------|----------------|------|
| Persistence | full JSON `MarshalIndent` of the entire dataset on every save | O(n) CPU + memory spike per write, entire DB rewritten |
| Loading | `Load()` materialised the whole dataset before any use | slow startup, unbounded memory |
| Collection | sources fetched sequentially | total time = sum of all sources |
| Testing | configurations tested sequentially | total time = sum of all tests |
| Caching | none | repeated parse/dedup of identical data |
| Processing | whole payloads in memory | 10 MiB sources → 10 MiB+ allocations |

## 2. Current design (v0.3.0)

### Startup (metadata-first)

`store.Open` loads `store.meta` + `index.bin` only. For a 20,000-record
store that is ~2.4 ms (BenchmarkStoreReopen) versus reading and
JSON-decoding every record. The store is ready for reads immediately;
the flush worker and optional verification run in the background with
deterministic shutdown.

### Writes (incremental, asynchronous flush)

`UpsertBatch` validates and hex-decodes keys in one pass, appends ONE
journal batch (one write + one fsync per 512-record batch), applies the
whole batch to the memtable under one lock acquisition, and returns.
When the active memtable crosses its threshold it is FROZEN into an
immutable table and handed to a single background flush worker that
writes the chunk file, swaps index entries, persists meta and
checkpoints the WAL — all without blocking writers or readers.

Backpressure is explicit: at most `maxFrozen` (2) tables may be
pending; beyond that writers block until the worker drains room, so
memory is bounded regardless of write rate.

### Reads (index-assisted, lazy, pinned)

`Get` reads a single record directly at its indexed offset inside a
chunk file through a pinned handle from the bounded chunk-handle cache.
The handle cache is the sole owner of open chunk descriptors: eviction
closes files, pins prevent eviction mid-read, and shutdown closes
everything (see docs/storage-format.md for the ownership model).

A point read allocates exactly: the record body, the caller's defensive
value copy and one deferred release — 3 allocations, 164 B/op
(TestAllocsHotPaths pins this; the v0.2 path allocated 7+ per read
through key formatting, cache boxing and path building).

### Iteration (consistent snapshots)

`Iterate` captures one merged snapshot (40-byte index references plus
slice headers of immutable memtable values), sorts it and streams
records through pinned handles. The callback observes a consistent
point-in-time view; concurrent writers cannot make an iteration skip,
duplicate or resurrect records. Values of pending records are never
copied — the memtable's immutability discipline makes the slice headers
safe to read after the lock is released.

### Ingestion (streaming, bounded)

Fetch, parse and dedup/persist run as bounded worker stages. Throughput
BenchmarkPipelineRun: 5,000 configurations end-to-end (fetch from local
HTTP → parse → normalize → validate → dedup → chunk → persist → journal
fsync) in ~50 ms on the CI-class VM. A slow source applies backpressure
to its own stage only; failed sources never block others.

### Dedup (fingerprint scale)

The canonical SHA-256 fingerprint is unchanged. In-memory dedup uses a
sharded 64-bit working index (16 shards) to avoid global lock
contention. When built with `-tags native_accel`, batch hashing and
CRC-32 run in the C++ layer; the Go fallback is bit-identical and
dispatch is chosen per call (metrics record fallbacks under
`native_fallback_hits`).

## 3. Benchmarks in the repository

```bash
go test -bench=. -benchmem -run=NONE \
  ./engine/chunks ./engine/store ./engine/native \
  ./engine/core ./engine/core/v2ray
```

| Benchmark | What it measures |
|-----------|------------------|
| BenchmarkChunkWrite | one 4k-record chunk (temp + fsync + rename) |
| BenchmarkChunkRead | full sequential scan of one chunk |
| BenchmarkChunkVerify | full-chunk CRC verification |
| BenchmarkChunkerGrouping | deterministic record→chunk grouping |
| BenchmarkChunkWriteRead | chunk write + full read round trip |
| BenchmarkStoreUpsert | single-record upserts (fsync per record) |
| BenchmarkStoreUpsertBatch | 512-record batches (one fsync per batch) |
| BenchmarkStoreUpsertFlush | upsert throughput incl. journal + flush |
| BenchmarkStoreFlush | synchronous flush latency for a loaded memtable |
| BenchmarkStoreGet | point-read latency via index + offset |
| BenchmarkStoreGetMemtable | point-read served from the active memtable |
| BenchmarkStoreIterate | full-scan throughput with consistent snapshots |
| BenchmarkStoreReopen | metadata-first startup for 20k records |
| BenchmarkCompaction | full compaction pass over 50% dead records |
| BenchmarkMigration | 4,000-record streaming legacy migration |
| BenchmarkHashBatch (+Mode/go, /native) | batch FNV-1a hashing, both implementations |
| BenchmarkScanURLs | subscription scanning |
| BenchmarkBuildConfig (v2ray) | V4 runtime-document generation per protocol+transport |
| BenchmarkBuildConfigCached | generation with the runtime cache warm (reconnect path) |
| BenchmarkValidate (v2ray) | backend deep validation |
| BenchmarkSupports (v2ray) | hot capability check (selection + details view) |
| BenchmarkBackendSelection | deterministic selection across three registered backends |
| BenchmarkBackendSelectionCompatible | capability resolution across available backends |
| BenchmarkRedactLogText | log/output redaction hot path |
| BenchmarkGenCache | generation-cache lookup |
| BenchmarkFingerprint | normalized-model fingerprint (dedup identity) |
| BenchmarkNormalize | config normalization (pipeline hot path) |
| BenchmarkDisplayURL | redacted display rendering |

Run the native bridge benchmarks on an accelerated build:

```bash
make -C native
CGO_ENABLED=1 go test -tags native_accel -bench=. -run=NONE ./engine/native
```

## 4. Measured results (v0.2.0 → v0.3.0 rework)

Reference VM: 2 vCPU CI-class Linux runner, go1.26.8, `-benchtime=1s`
medians over repeated runs. Absolute numbers vary by machine; the
deltas and allocation counts are the meaningful part.

| Path | v0.2.0 (a536191) | v0.3.0 | Assessment |
|------|------------------|--------|------------|
| Write: Upsert+Flush | 3083 ns/op, 1019 B, 11 allocs | 2329 ns/op, 990 B, 6 allocs | **24% faster, 45% fewer allocs** — journal append and memtable apply are batched under one lock; flush moved off the writer path |
| Read: Get (chunk hit) | ~1045 ns/op, 272 B, 4 allocs | ~1230 ns/op, 164 B, 3 allocs | **+18% latency, 40% less memory** — the operation barrier + reference-counted handle pins that make Windows deletion safe cost ~180 ns per read; allocation footprint dropped |
| Read: Get (memtable hit) | — | 376 ns/op, 80 B, 1 alloc | new capability measured |
| Reopen (20k records) | 2566 µs | 2373 µs | **7% faster** |
| Iterate (20k records) | 28.5 ms, 8.25 MB, 60k allocs | 26.1 ms, 9.34 MB, 100k allocs | **9% faster**, more allocations: the snapshot trades slice-header copies for per-key lock churn and gives consistent point-in-time views |
| Batch write (512) | — | ~805 ns/record | one fsync amortised over the batch |
| Migration (4k records, streaming) | full-file unmarshal | ~54 ms, bounded memory | never materialises the legacy DB |

The read-path latency regression is deliberate and documented: the
v0.2 cache leaked file descriptors (the Windows CI failure), so every
read now pays for deterministic ownership. For the application's real
workload — ingestion bursts of thousands of records plus UI paging of
100-row pages — the write path and startup are the hot paths, and both
improved.

## 4b. Protocol-core paths (v0.4.0)

The v0.4 protocol-core layer adds four measured paths. Reference VM:
2 vCPU CI-class Linux runner, go1.26.8, `-benchtime=1s` medians.

| Path | v0.4.0 (measured, 2 vCPU, go1.26.8) | Assessment |
|------|--------------------------------------|------------|
| Config generation (V4, per doc) | 12–16 µs/op, 5.0–6.8 KB, 59–78 allocs | one-time per launch; generation is NOT on any hot loop |
| Config generation (cached, reconnect) | 1.2 µs/op, 464 B, 10 allocs | ~12× faster through the runtime generation cache |
| Backend validation | 236 ns/op, 0 allocs | capability-table check only |
| Supports (capability check) | 37 ns/op, 0 allocs | used by selection and the details view |
| Backend selection (3 backends) | 3.1 µs/op, 3.9 KB, 40 allocs | once per connect; never in a loop |
| Fingerprint (identity) | 610 ns/op, 355 B, 5 allocs | unchanged hot path from ingestion |
| Normalize | 96 ns/op, 0 allocs | unchanged |
| Redacted display | 141 ns/op, 64 B, 3 allocs | per UI snapshot |
| Log redaction | 5.0 µs/op, 360 B, 7 allocs | per captured log write, bounded buffer |
| Generation-cache lookup | 70 ns/op, 0 allocs | reconnect path |
| Core startup (real binaries) | ~100–140 ms spawn→listener-ready | dominated by the core process itself; measured by the smoke suites |

Storage and pipeline paths are unchanged from v0.3.0: the protocol-core
work deliberately added zero regression surface to the data layer
(the full store benchmark suite re-runs in CI on every push). The
temporary-runtime-config write and its 0600 permissions add one
small-file write per core launch (sub-millisecond), amortised over a
connection session.

Startup sequence budget (per connect, measured): selection (3 µs) +
validation (0.24 µs) + generation (12–16 µs, or 1.2 µs cached) +
temp-file write (sub-ms) + process spawn to listener ready
(~100–140 ms, core-bound). Everything before the spawn is tens of
microseconds — the protocol core process itself is the entire startup
cost, which is why the generation cache exists for the reconnect path
rather than for first connect.

## 5. UI performance

- Configuration lists are virtualized (`@tanstack/react-virtual`): the
  DOM holds ~30 rows regardless of dataset size.
- Search input is debounced (250 ms) and executed server-side over the
  store's index; the main thread never scans the dataset.
- Config pages are decoded on demand and cached in the hot layer.
- CSV export runs in a Web Worker so serializing tens of thousands of
  rows never blocks rendering.
- Backend state arrives via events; the UI never polls the store.

## 6. Memory discipline

- Payloads are capped by the fetcher (10 MiB) and processed line-wise by
  the parser.
- Chunk reads are per-record (`ReadAt`), never whole-file.
- Bounded buffers everywhere: queue sizes, memtable thresholds
  (records + bytes), the frozen-table queue (maxFrozen), LRU bounds on
  handles and cache layers.
- Iteration snapshots copy references and slice headers, never record
  bodies (pending bodies are bounded by the memtable thresholds).
- Migration streams the legacy file with a token-walking decoder; only
  one decoded entry and one bounded batch exist at a time.
- Store snapshots are read from atomic counters; no locks are held
  while the UI serializes state.
