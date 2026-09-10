# FreeIran Storage Format v2

This document specifies the on-disk formats implemented by
`engine/store` and `engine/chunks`. All integers are little-endian.

## 1. Directory layout

```text
<appdata>/data/
├── store.meta        JSON chunk registry
├── index.bin         binary fingerprint index
├── chunks/
│   └── NNNNNN.firc   chunk files (NNNNNN = zero-padded sequence)
└── wal/
    └── journal.log   write-ahead journal
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
- Writing is atomic: the chunk is written to a temporary file in the
  same directory, fsynced, then renamed into place.
- Chunk file names are stable identifiers (`000001` …).

## 3. store.meta

JSON, atomically replaced:

```json
{
  "version": 2,
  "schema": 1,
  "lsn": 1234,
  "next_sequence": 9,
  "count": 5000,
  "dead_records": 41,
  "chunks": {
    "1": { "seq": 1, "records": 512, "live": 508,
            "bytes": 103340, "checksum": 2166136261, "created": 1730000000000 }
  }
}
```

`lsn` is the journal LSN incorporated into this meta (see §5).
`count` is informational; the authoritative live count is derived from
the index and memtable.

## 4. index.bin

```text
offset  size  field
0       4     magic "FIRI"
4       1     format version (1)
5       1     reserved (0)
6       4     entry count (uint32)
10      ..    entries, 40 bytes each:
                32  key (raw binary fingerprint)
                4   chunk sequence (uint32)
                8   record offset in chunk file (uint64)
```

The offset points at the record's length prefix inside the chunk file.
The index is atomically replaced after every flush. Any structural
problem (bad magic, truncated table, entry referencing an unknown
chunk) triggers a deterministic rebuild by scanning all chunk files.

## 5. Write-ahead journal (wal/journal.log)

```text
header (18 bytes):
0       4     magic "FIRW"
4       2     format version (1)
6       8     base LSN (uint64)
14      4     reserved (0)

records, repeated:
1       1     op (1 = upsert, 2 = delete)
32      32    key (raw binary fingerprint)
4       4     value length (uint32, upsert only)
n       n     value bytes (upsert only)
4       4     CRC-32 (IEEE) over op || key || value-length || value
```

Semantics:

- Every appended batch is fsynced before the batch is applied to the
  memtable, so acknowledged writes survive crashes.
- LSNs are implicit: the first record after the header has
  baseLSN + 1.
- **Checkpoint** (after a successful flush): the header baseLSN is
  advanced to the journal's current LSN and the record area is
  truncated. Records before the checkpoint LSN are already incorporated
  into chunks + index + meta.
- **Recovery**: on open, records with LSN greater than the store's
  checkpoint LSN (from store.meta) are re-applied idempotently. A
  truncated or CRC-invalid tail (crash mid-append) is detected and
  discarded; earlier records remain valid.
- Crash ordering is safe in all interleavings because meta is written
  before the checkpoint: if the process dies between the two, replay
  re-applies records that are already incorporated — harmless because
  upserts overwrite and deletes are idempotent.

## 6. JSON v1 legacy migration

The legacy format (engine/database, `diskState v1`) is:

```json
{ "version": 1,
  "entries": { "<fingerprint-hex>": { "config": {...},
                                        "added": "...", "updated": "..." } } }
```

Migration (`store.MigrateFromJSON`):

1. Reads and validates the legacy file (version must be 1).
2. Imports every entry as `[key][config JSON]` in sorted key order
   (deterministic chunk layout).
3. Verifies every migrated key is readable from the store.
4. Renames the legacy file to `<original>.migrated` (never deletes).

The operation is idempotent: a missing legacy file is a no-op success.
After migration, records carry their original fingerprints unchanged,
so old and new identifiers remain comparable.

## 7. Operational notes

- **Compaction** rewrites chunks whose dead-record ratio exceeds 50%,
  merging up to 8 victims per pass, in sequence order. Chunks that end
  up with zero live records are deleted without a rewrite.
- **Verification** (`VerifyAll`) walks every chunk and validates the
  payload CRC; progress is reportable; failures are classified as
  `corrupt_data`.
- **File permissions**: directories 0700, files 0600 (Windows maps
  these to ACL defaults).
