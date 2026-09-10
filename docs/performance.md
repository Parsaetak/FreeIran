# FreeIran Performance Architecture

Performance work follows one rule: correctness first, then measurability,
then architecture, then optimisation. Every claim below is backed by a
benchmark in the repository (`go test -bench`).

## 1. What was wrong in v0.1 (baseline)

| Area | v0.1 behaviour | Cost |
|------|----------------|------|
| Persistence | full JSON `MarshalIndent` of the entire dataset on every save | O(n) CPU + memory spike per write, entire DB rewritten |
| Loading | `Load()` materialised the whole dataset before any use | slow startup, unbounded memory |
| Collection | sources fetched sequentially | total time = sum of all sources |
| Testing | configurations tested sequentially | total time = sum of all tests |
| Caching | none | repeated parse/dedup of identical data |
| Processing | whole payloads in memory | 10 MiB sources → 10 MiB+ allocations |

## 2. Current design

### Startup (metadata-first)

`store.Open` loads `store.meta` + `index.bin` only. For a 20,000-record
store that is ~2 ms (BenchmarkStoreReopen) versus reading and JSON-decoding
every record. Chunk verification and hot-cache warm-up run in the
background; the UI is interactive before they finish.

### Writes (incremental)

`UpsertBatch` appends one journal batch (one fsync per 512-record batch)
and applies to the memtable. Flush writes only the delta as one new chunk
file. The full dataset is never re-serialized; unaffected chunks are never
rewritten.

### Reads (index-assisted, lazy)

`Get` reads a single record directly at its indexed offset inside a chunk
file. Open chunk handles are pooled in a bounded LRU. A page of
configurations decodes only the requested records; the hot-configuration
cache makes repeated UI pages free (hit/miss tracked).

### Ingestion (streaming, bounded)

Fetch, parse and dedup/persist run as bounded worker stages. Throughput
BenchmarkPipelineRun: 5,000 configurations end-to-end (fetch from local
HTTP → parse → normalize → validate → dedup → chunk → persist → journal
fsync) in ~50 ms on the CI-class VM. A slow source applies backpressure to
its own stage only; failed sources never block others.

### Dedup (fingerprint scale)

The canonical SHA-256 fingerprint is unchanged. In-memory dedup uses a
sharded 64-bit working index (16 shards) to avoid global lock contention.
When built with `-tags native_accel`, batch hashing and CRC-32 run in the
C++ layer; the Go fallback is bit-identical and dispatch is chosen per
call (metrics record fallbacks under `native_fallback_hits`).

## 3. Benchmarks in the repository

```bash
go test -bench=. -run=NONE ./engine/chunks ./engine/store ./engine/pipeline
```

| Benchmark | What it measures |
|-----------|------------------|
| BenchmarkChunkerGrouping | deterministic record→chunk grouping |
| BenchmarkChunkWriteRead | chunk write + full read round trip |
| BenchmarkStoreUpsertFlush | upsert throughput incl. journal + flush |
| BenchmarkStoreGet | point-read latency via index + offset |
| BenchmarkStoreReopen | metadata-first startup for 20k records |
| BenchmarkStoreIterate | full-scan throughput |
| BenchmarkPipelineRun | end-to-end ingestion of 5,000 configs |

Run the native bridge benchmark on an accelerated build:

```bash
make -C native
CGO_ENABLED=1 go test -tags native_accel -bench=. -benchtime=1x \
  -run=NONE ./engine/native
```

## 4. UI performance

- Configuration lists are virtualized (`@tanstack/react-virtual`): the
  DOM holds ~30 rows regardless of dataset size.
- Search input is debounced (250 ms) and executed server-side over the
  store's index; the main thread never scans the dataset.
- Config pages are decoded on demand and cached in the hot layer.
- CSV export runs in a Web Worker so serializing tens of thousands of
  rows never blocks rendering.
- Backend state arrives via events; the UI never polls the store.

## 5. Memory discipline

- Payloads are capped by the fetcher (10 MiB) and processed line-wise by
  the parser.
- Chunk reads are per-record (`ReadAt`), never whole-file.
- Bounded buffers everywhere: queue sizes, memtable thresholds
  (records + bytes), LRU bounds on handles and cache layers.
- Store snapshots are read from atomic counters; no locks are held
  while the UI serializes state.
