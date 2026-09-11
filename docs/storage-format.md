# FreeIran Storage Format v2 (v0.3.0 lifecycle rework; unchanged in v0.4.0)

This document specifies the on-disk formats implemented by
`engine/store` and `engine/chunks`, and — equally important — the
resource-ownership model that makes the store safe to delete on
Windows. All integers are little-endian.

## 1. Directory layout

```text
<appdata>/data/
├── store.meta              JSON chunk registry (atomic replace)
├── index.bin               binary fingerprint index (atomic replace)
├── chunks/
│   └── NNNNNN.firc         immutable chunk files (zero-padded sequence)
└── wal/
    └── seg-XXXXXXXX.wal    segmented write-ahead journal
```

## 2. Chunk file (.firc)

```text
offset  size  field
0       4     magic "FIRC"
4       2     format version (1)
6       2     flags (bit 0 = compressed; reserved bits must be 0)
8       4     record count (uint32)
12      4     reserved (0)
16      4     payload CRC-32 (IEEE), over all [len][data] pairs
20      ..    payload: repeated records:
                4   record length (uint32)
                n   record body
```

A record body is `[32-byte key][value bytes]` where the key is the raw
binary form of the record's SHA-256 fingerprint and the value is the
compact JSON encoding of the configuration.

Invariants:

- Records are never split across chunks; a chunk boundary always falls
  between records.
- Chunks are immutable after their atomic commit (temp file + fsync +
  rename). Nothing ever mutates a committed chunk in place; compaction
  writes a NEW chunk and deletes the victims.
- Point reads never scan a chunk: `Get` preads exactly the length
  prefix and body at the indexed offset.
- The chunk checksum covers the payload region; `Reader.Verify()`
  recomputes it incrementally during a scan.

## 3. Write-ahead journal (segmented, format v2)

```text
wal/
├── seg-00000000.wal
├── seg-00000001.wal      (rolled at 8 MiB)
└── ...
```

Segment header:

```text
offset  size  field
0       4     magic "FIRW"
4       2     format version (2)
6       8     baseLSN — LSN of the record BEFORE the first record
```

Record:

```text
1   op (1 = upsert, 2 = delete)
32  key (binary fingerprint)
4   value length (uint32; ALWAYS present, 0 for deletes)
n   value bytes (upserts only)
4   CRC-32 (IEEE) over op + key + length + value
```

Semantics:

- **Durability.** Every `Upsert`/`UpsertBatch`/`Delete` appends one
  batch with a single write + a single fsync before returning. A
  journal-acknowledged write survives a crash even before its chunk is
  flushed; replay re-applies it idempotently.
- **LSN continuity.** LSNs are contiguous across segments in segment
  order; each segment header records the LSN just before its first
  record. A gap between segments fails the open loudly (missing data),
  never silently.
- **Checkpoint.** After a flush makes a table's chunk and metadata
  durable, `Checkpoint(targetLSN)` removes every segment whose records
  all have LSN ≤ targetLSN. Files are closed BEFORE removal, so
  deletion works on Windows. A segment straddling the target is kept;
  replay skips its incorporated records using the checkpoint LSN from
  store.meta.
- **Crash tails.** A truncated final record (detected via length/CRC
  errors) is discarded and the segment truncated; corruption in a
  non-final segment fails the open — real data loss must be loud.
- **v1 upgrade.** A legacy single-file `wal/journal.log` is streamed,
  re-applied, re-journaled into segments (one fsync) and only then
  removed. Note: the v1 READER skipped the length prefix for delete
  records (which the v1 writer still emitted), so replayed deletes
  failed their CRC and were silently discarded; the v2 framing is
  uniform and the upgrade path re-reads v1 tails correctly.

## 4. Chunk registry (store.meta) and index (index.bin)

`store.meta` (JSON, atomic replace) records the format version, the
checkpoint LSN, the next chunk sequence, dead-record accounting and one
entry per live chunk (sequence, record/live counts, bytes, CRC).
`index.bin` (binary, atomic replace) holds `[32B key][u32 chunk][u64
offset]` rows — 40 bytes per live record, always rebuildable from chunk
files alone.

Recovery matrix on open:

| Condition | Behaviour |
|-----------|-----------|
| meta present, chunk file missing | registry entry dropped, status=degraded |
| meta present, chunk file not referenced (orphan from crash between chunk write and meta persist) | file removed; records recovered from WAL replay |
| meta missing/corrupt | registry rebuilt from chunk files (each chunk's header), WAL replayed on top |
| index missing/corrupt/referencing unknown chunks | index rebuilt deterministically from chunks |
| journal segment with corrupt tail | tail discarded, open continues |
| journal continuity break between segments | open fails (data loss is loud) |

## 5. Resource ownership model (the Windows fix)

The v0.2 failure mode: chunk `*os.File` handles were stored in a
generic LRU cache with no eviction callback. Eviction merely forgot the
pointer (fd leak), `Close()` never closed cached files, and compaction
called `os.Remove` on victims that were still open. On Windows an open
handle blocks deletion, so `t.TempDir()` cleanup failed with "The
process cannot access the file because it is being used by another
process" — the exact CI failure this rework fixes.

v0.3.0 establishes one owner per resource class:

### Chunk handles — `chunkHandleCache`

```text
acquire(seq)  → pin (refs++), detach from LRU
use           → ReadAt (pread; safe for concurrent readers)
release(seq)  → unpin (refs--); unpinned entries become evictable
eviction      → LRU eviction of UNPINNED entries CLOSES the file
purge(seq)    → close + forget one entry (compaction victims)
closeAll()    → close every entry (store shutdown)
```

- Pinned entries are excluded from the LRU list, so eviction can never
  close a handle a reader is using.
- `os.File.ReadAt` is positional and safe for concurrent use, so one
  pinned handle serves many parallel readers.
- Path resolution happens only on cache misses — hot reads pay no
  formatting cost.

### Operation barrier — deterministic Close

Every public operation registers in a gate-guarded WaitGroup.
`Close()`:

```text
closed flag set (new operations rejected)
  → drain in-flight operations
  → freeze the active memtable
  → stop the flush worker (it drains the remaining queue first)
  → close the journal (final sync + close)
  → close ALL cached chunk handles
  → return joined errors (flush, WAL, handles)
```

After `Close()` returns, the store owns no file descriptor; the store
directory is immediately deletable on every platform. This is enforced
by tests (`TestDeleteDirectoryImmediatelyAfterClose`,
`TestCloseReleasesAllFileHandles`).

### Scans — deletion barrier

Long-running chunk readers (`Iterate`, `VerifyAll`) register as scans.
Compaction removes victim files only after (a) the registry swap made
new snapshots reference-free, and (b) active scans drained. Combined
with handle purging this guarantees no chunk file is ever deleted while
any descriptor is open.

### Compaction sequence per group

```text
snapshot live references            (read lock)
  → read survivor records           (pinned handles, no lock)
  → write replacement chunk         (temp + fsync + rename)
  → CAS-swap index entries          (write lock; entries moved by
                                      concurrent writers keep their
                                      newer state — no resurrection)
  → persist index + meta
  → wait for active scans
  → purge victim handles            (CLOSES the files)
  → remove victim chunk files
```

### Memtable levels and value ownership

```text
writer → active memtable → freeze → frozen queue → flush worker → chunk
```

- Writers serialize on a write mutex across the WAL append and the
  memtable apply, so the applied-LSN watermark is exact and the
  checkpoint can never advance past unapplied records (a durability
  bug in v0.2's flush path under concurrent writes).
- At most `maxFrozen` tables may be pending; beyond that writers block
  (explicit backpressure, bounded memory).
- A memtable value is immutable once stored (upserts replace the slice
  header with a private copy). Snapshots therefore capture slice
  headers without copying bodies.
- Tombstones are dropped when their table flushes; the delete is durable
  in the WAL until the checkpoint passes it.

### Migration streaming

`MigrateFromJSON` walks the legacy JSON with a token-walking decoder:
one decoded entry and one bounded batch (1024 records) exist at a time.
Each batch is verified through the memtable before the next is staged;
after the final flush a second streaming pass verifies every record
through the full disk path. The legacy file is renamed (never deleted)
only after verification succeeds, and re-running is a no-op.


## 9. Fingerprint stability across v0.4.0

The v0.4.0 protocol-core work added protocol-detail fields to the
normalized model (VLESS flow/encryption, VMess alterId/header type,
ALPN, REALITY spider path). These fields are deliberately NOT part of
the fingerprint: record identity remains endpoint + credentials +
transport, so every fingerprint computed by v0.2/v0.3 stores stays
valid in v0.4 — no re-keying, no duplicate re-ingestion, no
migration. The stored JSON gains new optional fields (all
`omitempty`), so old records decode unchanged and new records remain
readable by the same decoder.

The full store format (chunks, WAL segments, registry, index) is
byte-identical to v0.3.0; the v0.4 release re-ran the complete
lifecycle and recovery test matrix against the protocol-core build
as a regression gate.
