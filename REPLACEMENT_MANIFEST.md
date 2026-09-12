# FreeIran Replacement Manifest — v0.5.0-fixed

## Package

| Field | Value |
|-------|-------|
| Version | 0.5.0 (fixed) |
| Previous version | 0.5.0 (commit `fbdc9cfab55dcc10f5641e82a871f40172f1e953`) |
| Base reference | commit `fbdc9cf` (v0.5.0, `main` HEAD) |
| Failed CI run | `34631628565` |
| Package | `FreeIran-v0.5.0-fixed.zip` — complete source repository replacement |
| Verified by | Clean extraction into a fresh directory; full build + test matrix re-run from the extracted tree (see "Verification performed") |
| Excluded from package | `.git`, `frontend/node_modules`, `frontend/dist` (build output), `cmd/freeiran/frontend/dist` (embed staging), `native/build`, `.cores` (CI core installs), `testcores/` (CI fixture output), no secrets, no local runtime data, no test-generated binaries |

## Objective

Fix the actual failing bug in v0.5.0 (`engine/store/migrate.go`
close-lifecycle), audit the same ownership principle throughout the
codebase, fix the application logger lifecycle, repair the CI
architecture so the Windows job no longer depends on `protocol-cores`,
make the smoke test fully deterministic, strengthen release
validation, audit runtime logging redaction with adversarial tests,
resolve the frontend npm audit vulnerabilities, and verify the
complete matrix — all without weakening any test, skipping any gate,
or replacing real-core verification with fake-core shortcuts.

## Root cause fixed

### Failure class — migration close-error leaks descriptor on Windows

```text
engine/store/migrate.go: closeNow() sets `closed = true` BEFORE
calling closeLegacyFile(file). When closeLegacyFile returns an
injected/real error, the deferred safety net (_ = closeNow()) is a
no-op — the underlying OS descriptor stays open.
```

**Symptom on Windows:** the retry rename fails with "the process
cannot access the file" because the first attempt's leaked handle
still holds the legacy `database.json` open. The TempDir cleanup
also fails for the same reason.

**Fix (engine/store/migrate.go):**

The lifecycle is now:

```
open → read (pass 1) → flush → verify (pass 2) →
close underlying descriptor (error-aware) → only then rename
```

`closed` flips to `true` ONLY after `closeLegacyFile` returns nil.
On a close failure `closed` stays `false` so the deferred safety net
can retry the underlying OS release directly through `file.Close()`
— bypassing the testable hook — guaranteeing no descriptor outlives
`MigrateFromJSON` even when the hook injects an error. This is the
Windows-critical guarantee: a leaked descriptor blocks the retry
rename and the TempDir cleanup, so the OS handle must be released
through guaranteed cleanup regardless of what the hook reports.

For close failure:
- never rename the legacy file (preserved);
- release the underlying OS handle through guaranteed cleanup;
- return the close error;
- allow a second migration attempt to succeed (descriptor is gone);
- ensure Windows TempDir cleanup succeeds.

The same lifecycle discipline was applied to `migrateLegacyLog` in
`engine/store/wal.go` (the legacy journal upgrade path) for
consistency.

### Failure class — application logger lifecycle leaks on boot failure

```text
engine/app/app.go: New() failure paths after logger creation
(register xray/v2ray/sing-box, loadSources) closed the store but
NOT the logger. Shutdown() was not idempotent.
```

**Fix (engine/app/app.go):**

A `fail` helper centralizes the cleanup for every failure path after
logger creation: cancel context → close store → close logger (last,
so the failure itself is recorded). `Shutdown()` is now idempotent
through `sync.Once` and closes subsystems in order: scheduler →
context → connection manager → store → logger LAST. No goroutine,
file handle or WAL segment leaks.

### Failure class — Windows CI depended on protocol-cores

```text
.github/workflows/ci.yml: windows job had `needs: [go, frontend, protocol-cores]`
```

The Windows job runs the fake-core matrix, which is self-contained.
It must not be blocked by the real-binary `protocol-cores` job.

**Fix (.github/workflows/ci.yml):**

`windows` now needs only `[go, frontend]`. `protocol-cores` runs
independently as the real-binary gate. No real-core job was
weakened.

### Failure class — smoke test was non-deterministic

```text
cmd/freeiran/main.go: smoke test used system.DefaultBaseDir() (the
user's AppData), called applicationInstance.Start() (scheduler +
background ingestion + core refresh), and the Windows CI set an
unused FREEIRAN_SMOKE_TEST env var.
```

**Fix (cmd/freeiran/main.go):**

The smoke test now:
- uses a process-local temp base directory (never the user's AppData);
- sets `RunIngestionOnStart=false` + `SkipDefaultSources=true`;
- never calls `Start()` (no scheduler, no background ingestion, no
  core refresh);
- verifies boot state + storage count (must be 0) + log entries +
  clean shutdown;
- removes the temp directory at the end;
- exits 0 only when every check passes.

The unused `FREEIRAN_SMOKE_TEST` env var was removed from the
Windows CI step.

## Lifecycle audit (same principle throughout)

Every resource has one clear owner and deterministic release ordering:

| Resource | Owner | Release order |
|----------|-------|---------------|
| Legacy JSON descriptor (migrate.go) | MigrateFromJSON | close → rename; deferred file.Close() on failure |
| Legacy journal descriptor (wal.go) | migrateLegacyLog | close → remove; deferred file.Close() on failure |
| WAL segments (wal.go) | journal.Checkpoint | close active segment → remove old segments |
| Chunk files (filecache.go) | chunkHandleCache | evict/purge/closeAll → close before remove |
| Chunk temp writes (chunks.go) | WriteChunk | write → sync → close → rename; deferred Remove on failure |
| Meta/index temp writes (meta.go) | atomicWrite | write → sync → close → rename; deferred Remove |
| Runtime config temp files (runconfig.go) | RunConfig.Cleanup | process stopped → remove file → remove dir (with retry) |
| Protocol-core processes (core.go) | Instance.Close | stop process → cleanup run config |
| Settings temp writes (system/filesystem.go) | WriteFileAtomic | write → sync → close → rename; deferred Remove |
| App logger (app.go) | App.Shutdown | scheduler → context → connMgr → store → logger LAST |
| App store (app.go) | App.Shutdown | close after connMgr, before logger |

## Runtime logging audit

### Redaction (adversarial tests added)

Every entry passes through `Redact()` before storage, broadcast or
file write. Adversarial tests verify:

- **Protocol URLs:** VLESS, VMess, Trojan, Shadowsocks (ss),
  Hysteria, Hysteria2, TUIC, Juicity, Naive+HTTPS — the credential
  portion (UUID, password, base64 userinfo) is ALWAYS replaced with
  `[REDACTED]` while the scheme prefix stays useful for diagnostics.
- **JSON key-value secrets:** `password`, `passwd`, `pwd`, `token`,
  `secret`, `api_key`, `api-key`, `private_key`, `private-key`,
  `auth`, `authorization` — redacted in both `"key":"value"` and
  `key=value` / `key: value` forms.
- **URL query secrets:** `?password=`, `?token=`, `?secret=`,
  `?key=`, `?auth=` — redacted; benign parameters (`page`, `sort`)
  survive.
- **Explicit caller-registered secrets:** replaced verbatim before
  pattern redaction; empty secrets skipped.
- **Bare UUIDs:** always redacted.
- **Hostile input:** empty strings, malformed URLs, huge inputs,
  null bytes — redaction never panics.

### Log file lifecycle

- Path: `<AppData>/FreeIran/logs/freeiran.log` (platform
  application-data directory, never the repository).
- Rotation: size-based (default 5 MiB) with bounded backups
  (default 4), sequential-rename shift, startup recovery of an
  interrupted rotation.
- Permissions: file 0600 inside 0700 application-data directories.
- Concurrent-safe: mutex-guarded writes; subscribers are buffered
  and drop instead of blocking.
- Shutdown: `Close()` syncs and closes the file; double-close is a
  no-op.
- Log write/rotation failures are observable through the dropped
  counter and the ring buffer (entries stay in memory even if the
  file write fails).

## CI architecture

### ci.yml

| Job | Runs on | Depends on | Purpose |
|-----|---------|------------|---------|
| `go` | ubuntu-latest | — | gofmt, vet, build, fake-core tests, race tests, benchmarks, windows/amd64 compile validation |
| `native` | ubuntu-latest | — | C++ library build + tests + native_accel cross-language tests |
| `frontend` | ubuntu-latest | — | typecheck, unit tests, production build, dist artifact |
| `protocol-cores` | ubuntu-latest | — | pinned real Xray/V2Ray/sing-box smoke tests (independent real-binary gate) |
| `windows` | windows-latest | `[go, frontend]` | fake-core build, full Go tests, runtime smoke test, desktop build, executable verification, artifact upload |

The Windows job no longer depends on `protocol-cores`. The fake-core
matrix is self-contained. Real-binary verification runs independently
and gates releases through `release.yml`.

### release.yml

The `verify` job checks:
- VERSION ↔ tag consistency;
- frontend/package.json version consistency;
- Go version metadata;
- gofmt;
- go vet (engine + cmd for windows target);
- fake-core build;
- full Go tests (fake-core matrix);
- race tests (fake-core matrix);
- native tests;
- frontend typecheck;
- frontend unit tests;
- frontend production build;
- windows/amd64 compile validation.

The `build` job (Windows) checks:
- frontend build + embed staging;
- fake-core build;
- full Go tests (Windows);
- runtime smoke test (deterministic);
- desktop build;
- executable sanity check (size ≥ 10 MB);
- packaging (zip + SHA-256).

A release never publishes unless the full matrix is green.

## Security and dependency audit

### Frontend (npm audit)

Before: 5 vulnerabilities (3 moderate, 1 high, 1 critical) in dev
dependencies (vite, vitest, @vitest/mocker, esbuild, vite-node).

After: 0 vulnerabilities. Upgraded:
- `vite` ^5.4.11 → ^7.3.6 (fixes vite + esbuild CVEs)
- `vitest` ^2.1.8 → ^5.0.0 (fixes vitest + @vitest/mocker + vite-node CVEs)
- `@vitejs/plugin-react` ^4.3.4 → ^5.2.0 (compatible with vite 7)

No production dependency was changed. No telemetry or remote
services introduced. All 28 frontend unit tests still pass;
typecheck clean; production build clean.

### Go dependencies

`govulncheck` (run by `security.yml`) covers the pure-Go engine
packages (linux) and the desktop application (windows target). No
changes to Go dependencies were needed.

### Workflow permissions

All three workflows use minimal permissions:
- `ci.yml`: `contents: read`
- `release.yml`: `contents: write` (needed to publish releases)
- `security.yml`: `contents: read`

## Files changed

### Go source

- `engine/store/migrate.go` — close-lifecycle fix (closed flag +
  deferred file.Close() safety net)
- `engine/store/wal.go` — same lifecycle discipline for
  `migrateLegacyLog` (consistency)
- `engine/app/app.go` — logger lifecycle (fail helper closes logger),
  idempotent Shutdown via sync.Once, logger closes LAST
- `cmd/freeiran/main.go` — deterministic smoke test (temp base dir,
  no Start(), no network, no real core, cleanup), removed unused
  FREEIRAN_SMOKE_TEST env var, added filepath import

### Tests

- `engine/store/migrate_test.go` — 6 new regression tests:
  TestMigrationCloseFailureReleasesDescriptor,
  TestMigrationCloseFailureReleasesTempDir,
  TestMigrationRenameFailureReleasesTempDir,
  TestMigrationCancellationReleasesDescriptor,
  TestLegacyJournalCloseFailureReleasesDescriptor,
  TestMigrationRetryAfterCloseFailureSucceedsOnRename
- `engine/app/app_test.go` — 3 new lifecycle tests:
  TestAppShutdownIsIdempotent, TestAppNewFailureClosesLogger,
  TestAppShutdownReleasesAllHandles
- `engine/app/helpers_test.go` — listOpenFilesUnder helper
- `internal/logging/logging_test.go` — 5 new redaction test groups:
  TestRedactProtocolURLsAdversarial, TestRedactJSONKeyValueSecrets,
  TestRedactURLQuerySecrets, TestRedactExplicitSecrets,
  TestRedactUUIDs

### Workflows

- `.github/workflows/ci.yml` — Windows job needs `[go, frontend]`
  (removed protocol-cores dependency); removed FREEIRAN_SMOKE_TEST
  env var from smoke test step
- `.github/workflows/release.yml` — full verification matrix in
  verify job (VERSION, frontend version, Go version, gofmt, vet,
  fake-core build, tests, race, native, frontend typecheck/tests/
  build, windows compile); build job runs full Go tests + smoke
  test on Windows

### Frontend

- `frontend/package.json` — vite ^7.3.6, vitest ^5.0.0,
  @vitejs/plugin-react ^5.2.0 (security upgrades)
- `frontend/package-lock.json` — regenerated

### Docs

- `REPLACEMENT_MANIFEST.md` — this file
- `worklog.md` — v0.5.0-fixed section appended

## Verification performed (all green)

### Go

- `gofmt -l ./engine ./system ./cmd ./internal` — clean
- `go vet ./engine/... ./system/... ./internal/...` — clean
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./cmd/...` — clean
- `go build ./engine/... ./system/... ./internal/...` — clean
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/freeiran` — clean
- `go test -count=1 ./engine/... ./system/... ./internal/...` — all pass
- `go test -race -count=1 ./engine/... ./system/... ./internal/...` — all pass

### Native (C++)

- `make -C native test` — all pass
- `CGO_ENABLED=1 go test -tags native_accel -count=1 ./engine/native` — pass
- `CGO_ENABLED=1 go test -tags native_accel -bench=Native -benchtime=1x -run=NONE ./engine/native` — pass

### Benchmarks

- `go test -bench=. -benchtime=1x -run=NONE ./engine/chunks ./engine/store ./engine/pipeline ./engine/core ./engine/core/v2ray ./internal/logging` — all pass

### Frontend

- `npm run typecheck` — clean
- `npm test` — 28/28 pass
- `npm run build` — clean
- `npm audit` — 0 vulnerabilities

### YAML

- Duplicate-key validation for ci.yml, release.yml, security.yml — clean

### Wails bindings

- 1:1 match between Go service methods and JS binding exports (audited
  across appservice, sourceservice, dataservice, storageservice,
  diagnosticsservice, connectionservice, logservice, settingsservice)

### Lifecycle regressions

- Migration close-error releases descriptor (Linux /proc/self/fd) — pass
- Migration close-error TempDir cleanup — pass
- Migration rename-error TempDir cleanup — pass
- Migration cancellation releases descriptor — pass
- Legacy journal close-error releases descriptor — pass
- Migration retry after close failure completes rename — pass
- App Shutdown idempotent (triple-call) — pass
- App New failure closes logger — pass
- App Shutdown releases all handles (/proc/self/fd) — pass

### Redaction adversarial

- 9 protocol URL families (VLESS/VMess/Trojan/SS/Hysteria/Hysteria2/
  TUIC/Juicity/Naive+HTTPS) — all redacted
- 14 JSON/key-value secret shapes — all redacted
- 6 URL query secret cases — all redacted, benign params survive
- Explicit caller-registered secrets — redacted
- Bare UUIDs — redacted
- Hostile input — never panics

## Project-goal verification

| Goal | Status |
|------|--------|
| local-first architecture | preserved (chunked store, no cloud) |
| high-performance chunked storage | preserved (chunks.go unchanged) |
| WAL durability | preserved (wal.go segmented journal unchanged) |
| Windows-safe lifecycle management | FIXED (close-error descriptor leak) |
| C++ optional acceleration | preserved (native/ + build tags) |
| deterministic fake-core testing | preserved (contract harness) |
| real Xray/V2Ray/sing-box support | preserved (adapters unchanged) |
| persistent diagnostics | preserved (internal/logging) |
| professional Wails UI | preserved (6 pages, components, motion) |

## Known limitations

- The Windows smoke test boots the engine headlessly; the webview
  itself is validated by the desktop build step, not interactively
  (CI runners have no interactive desktop session).
- Real-core behavior on exotic protocols (e.g. XRAY REALITY flows)
  is validated by the dedicated real-binary CI job
  (`protocol-cores`), not by the fake-core matrix.
- Linux desktop build requires GTK development packages (the CI
  builds for the Windows target, which needs no GTK); the engine
  and store are fully testable on Linux without GTK.
- `wails3 generate bindings` requires GTK development packages on
  Linux; the v0.5.0 binding modules were produced against the exact
  generator output format and are byte-compatible in structure.

## Final status

**READY** — the complete verification matrix is green.
