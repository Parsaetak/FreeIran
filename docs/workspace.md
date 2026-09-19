# The FreeIran Workspace (v0.9.8.3)

FreeIran has **one** Workspace Root. Everything the application creates
or needs lives below that root; no runtime state is ever silently split
across `%APPDATA%`, `%LOCALAPPDATA%`, XDG data/cache or the system temp
directory.

## Layout

```text
FreeIran/                  ← workspace root (default: directory of FreeIran.exe)
├── FreeIran.exe           # or FreeIran on Linux
├── portable.marker        # shipped in release ZIPs (deployment label)
├── config/                # sources.json, settings, workspace.json status
├── data/                  # chunked store
│   ├── chunks/            #   immutable chunk files (NNNNNN.firc)
│   ├── wal/               #   segmented write-ahead journal
│   ├── store.meta         #   chunk registry (atomic replace)
│   └── index.bin          #   fingerprint index (atomic replace)
├── cache/
├── logs/                  # freeiran.log + rotated backups
├── cores/                 # managed protocol cores (+ wintun on Windows)
├── runtime/               # short-lived temp files, always cleaned
└── docs/, deployment/     # static release content
```

## Resolution

`system.WorkspaceRoot()` resolves, in order:

1. **`FREEIRAN_HOME`** — explicit override for CI, tests and custom
   deployments (absolute path; the whole tree relocates there).
2. **The directory containing the FreeIran executable** — the default
   for portable deployments *and* regular installs. One model.
3. **Nothing else.** v0.9.8.3: the application folder is the
   workspace root for EVERY deployment — installed and portable share
   one model. The installer defaults to a per-user writable directory
   (`%LOCALAPPDATA%\Programs\FreeIran`) and validates writability,
   because the running application must be able to write its state
   next to the executable. A stale `installed.marker` from a
   pre-0.9.8.3 install is inert for path resolution; it only tells
   diagnostics and the one-time migration (below) that this
   deployment used to keep its data in the per-user directory. The
   decision is folded into `WorkspaceRoot()` itself — one authority,
   two deployment styles; `InstalledMode()`/`PortableMode()` only
   label the style for diagnostics.

The root is validated at boot: every directory is created and a
writable probe is performed. If the workspace is read-only, startup
fails with a native error dialog (and a `boot-error.txt` next to the
executable) explaining the two fixes: move FreeIran to a writable
folder, or set `FREEIRAN_HOME`.

## One-time migration from pre-0.9.2 locations

Older versions stored state under per-user directories
(`%APPDATA%\FreeIran`, `%LOCALAPPDATA%\FreeIran`, XDG data/cache). At
boot, when the workspace is **fresh** (no `data/store.meta`, no
`config/sources.json`), FreeIran:

1. detects a legacy location holding authoritative data,
2. copies `config/`, `data/`, `cores/` and `cache/` into the workspace
   (never `logs/` or `runtime/` — both are reconstructable),
3. verifies every copied file (byte counts; the store's checksummed
   chunks detect anything else at open time),
4. records the outcome in `config/workspace.json`,
5. leaves the source untouched — it is only a copy; removing the old
   folder is always a separate, explicit user decision.

The migration never runs twice: once the workspace holds authoritative
data, the gate skips deterministically (the skip reason is recorded in
the status file), so the dataset can never be silently duplicated.

### Environment flags

| Variable | Effect |
|----------|--------|
| `FREEIRAN_HOME` | Relocate the workspace root (absolute path). |
| `FREEIRAN_SKIP_MIGRATION` | `1` disables legacy discovery entirely (automated deployments). |

The status file is surfaced in Settings → Developer and in the
Diagnostics → Storage & workspace card.

## Data lifecycle

| Class | Contents | Policy |
|-------|----------|--------|
| Permanent | live configs, config metadata, source definitions, user settings, active core metadata, rollback binaries | never deleted automatically |
| Reconstructable | cache entries, temp parser buffers, `<workspace>/runtime` config dirs, store atomic-write temp files, core-install staging | removed opportunistically (age-bounded) |
| Replaceable historical | old logs (rotated), checkpointed WAL segments, dead chunk files (compaction), stale staging, failed-download artifacts | removed by age/size/pressure policy |

The **central cleanup coordinator** (`engine/cleanup`) runs the
reclamation tasks:

- `runtime` — leftover `freeiran-*` config dirs older than 6 h
- `store-tmp` — stale `.fir-tmp-*` / `.firc-*` / `.fir-sys-*` files older than 1 h
- `wal` — WAL segments fully covered by the durable checkpoint (never the active segment, never uncheckpointed records)
- `chunks` — compaction when the dead-record ratio reaches 35 % (a chunk holding a live authoritative record is never deleted — enforced by the store's lifecycle: index swap → meta persist → scan drain → handle close → remove)
- `staging` — core-install staging and failed downloads older than 7 d

Passes are bounded (45 s cap, entry caps per task, ctx-cancellable),
rate-limited (≥ 30 s between passes), triggered by memory-pressure
transitions and a 15-minute opportunistic cadence, and always report
what they reclaimed (structured log + Diagnostics card).

## Memory-pressure response

The Memory Booster (`engine/mempressure` + `engine/booster`) classifies
pressure as Normal / Elevated / High / Critical and the store adapts
its freeze thresholds (records + bytes) to each level — with hard
floors (512 records / 512 KiB) so chunks never fragment into tiny
files:

| Level | Memtable freeze | Actions |
|-------|-----------------|---------|
| Normal | 4096 rec / 8 MiB | normal caching/batching |
| Elevated | 3072 rec / 6 MiB | cold-cache eviction, stale-runtime cleanup |
| High | 2048 rec / 4 MiB | cache clear, aggressive flush, full cleanup pass |
| Critical | 1024 rec / 2 MiB | release caches + idle handles, aggressive flush, GC, cleanup |

Transitions are logged once (never per sampling tick) with the reason
(heap/cache/queue percentages) and every action taken.
