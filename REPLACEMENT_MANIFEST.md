# FreeIran Replacement Manifest — v0.5.0

## Package

| Field | Value |
|-------|-------|
| Version | 0.5.0 |
| Previous version | 0.4.1 |
| Base reference | commit `8dff50e82e931770984baf7201138c18b50ea113` (v0.4.1, `main` HEAD) |
| Package | `FreeIran-v0.5.0.zip` — complete source repository replacement |
| Verified by | Clean extraction into a fresh directory; full build + test matrix re-run from the extracted tree (see "Verification") |
| Excluded from package | `.git`, `frontend/node_modules`, `frontend/dist` (build output), `cmd/freeiran/frontend/dist` (embed staging), `native/build`, `.cores` (CI core installs), `testcores/` (CI fixture output), no secrets, no local runtime data, no test-generated binaries |

## Objective

Make FreeIran genuinely functional and production-ready: fix every
failure in the latest Windows CI run at its root (not the symptoms),
add persistent runtime error/warning logging, deliver a professional
TypeScript/Wails UI with purposeful motion, and preserve the
high-performance storage architecture and the real Xray / V2Ray /
sing-box integrations.

## Exact Actions failures discovered (v0.4.1 run) and root causes

### Failure class A — migration rename on Windows

```text
engine/app: migrate: store/migrate: environment: preserve legacy file
```

**Root cause.** `Store.MigrateFromJSON` (engine/store/migrate.go) held
the legacy JSON file open through both streaming passes via
`defer file.Close()` and then executed `os.Rename(legacy, legacy+".migrated")`.
On Windows, renaming a file the process still holds open fails —
the descriptor must be fully released first.

**Fix.** Restructured the migration lifecycle:
`open → pass 1 → flush → pass 2 (verify) → explicit error-aware close →
rename`. The deferred close remains only as the ERROR-path safety net;
the success path closes explicitly and treats a close error as fatal
(data already migrated and verified, legacy file preserved, migration
stays retryable). Read errors, rewind errors, close errors, rename
errors and context cancellation are all handled; the legacy file is
never deleted and migration remains idempotent.

### Failure class B — legacy journal upgrade on Windows

```text
journal.log: The process cannot access the file because it is being
used by another process
seg-00000000.wal
```

**Root cause.** `journal.migrateLegacyLog` (engine/store/wal.go) opened
the v1 `journal.log`, streamed and re-journaled its records, then
executed `os.Remove(path)` — while the descriptor was still open
(`defer file.Close()`). The comment claimed the removal was
Windows-safe; it was not. Both removal sites (empty-journal early
return and the post-upgrade unlink) were affected.

**Fix.** Close-before-remove on both paths with an error-aware close.
Additionally, once every record is durably re-journaled into the
segmented WAL, a close/removal failure can no longer lose data — the
upgrade is now DEFERRED to the next boot instead of failing the store
open, so a third-party lock (antivirus, backup tool) can never brick
the application. The segmented-WAL checkpoint path was audited and was
already Windows-safe (active segment closed before removal).

### Failure class C — fake core discovery on Windows

```text
fake v2ray not discovered
installed but unavailable: xray, v2ray, sing-box
```

Affected tests: `TestConnectionServiceBackends`,
`TestSelectionWithRealBackends`, `TestSelectionPreferenceOverridesPriority`,
`TestCoreProbeSupports`, `TestCoreProbeTest`.

**Root cause.** Tests staged the fake core binary under an
extensionless name (`dir/v2ray`), but `system.CoreLocator.Discover`
searches for the platform-correct name (`v2ray.exe` on Windows). Every
fake-core staging site was structurally wrong on Windows.

**Fix.** `system.ExecutableName` is now exported and the new
`contract.StageFakeCore(tb, dir, coreName)` is the single staging
helper for all eight test sites (engine/core, engine/connection,
engine/tester, engine/app); extensionless staging is structurally
impossible. No real binaries are installed for fake test cases and no
test was weakened.

### Fixture harness (test-environment design fix)

`contract.BuildFakeCore` now resolves deterministically:

1. `FREEIRAN_TEST_CORES` (CI contract): a fixture directory with
   pre-built fake cores. Missing fixtures are a HARD test failure —
   never a silent skip.
2. Local toolchain build of `engine/core/testdata/fakecore`
   (one compilation per test process, per-test copies), skipping only
   when no Go toolchain exists on a developer machine.

The fake core itself now emulates the full contract the registry and
process manager expect: `version`/`--version`/`-version` probes,
`check -c` config validation, startup, readiness (TCP inbound from the
generated document), controlled shutdown, deterministic exit and
failure injection (`FAKECORE_FAIL_FAST`, `FAKECORE_CRASH_AFTER_START`,
`FAKECORE_HANG`).

## Runtime logging design (new, `internal/logging`)

- Structured JSON-lines log at `<AppData>/FreeIran/logs/freeiran.log`
  (platform application-data directory, never the repository).
- Entry shape: `{seq, ts(RFC3339 UTC), level, subsystem, event,
  message, operation?, error_kind?}`.
- Size-based rotation (default 5 MiB) with bounded backups (default 4),
  sequential-rename shift and startup recovery of an interrupted
  rotation; limits are configurable and adjustable at runtime.
- Mandatory redaction on every entry (UUIDs, protocol URLs,
  password/token/key parameters, caller-registered secrets) before
  file, memory ring or subscriber delivery.
- No-op-safe package-level global (`logging.E/W/Err/D`) integrates the
  store, migration, core manager, connection manager and system layer
  without dependency cycles; the desktop entrypoint opens the log
  before anything else and closes it last.
- Core stdout/stderr continues to be captured through the redacting
  `engine/core.LogBuffer`; raw output is never appended.
- Logged events: `application_start`, `application_ready`, `store_open`,
  `store_error`, `migration_start/success/error`, `compaction_*`,
  `source_refresh_start/success/error`, `core_discovered`, `core_start`,
  `core_ready`, `core_exit`, `core_error`, `connection_start/success/
  failure`, `disconnect_start/success`, `shutdown_start`,
  `shutdown_complete`, `settings_updated`.
- UI: `LogService` (incremental `Recent(sinceSeq, limit, filters)`,
  `Subsystems`, `Clear`, `LogFile`, `OpenLogsDir`) feeds the
  diagnostics viewer without ever transferring the whole file.

## UI changes (frontend/)

- Rebuilt design system: tokens (surface hierarchy, radius, spacing,
  shadows, typography, status colors, motion), component classes
  (buttons, inputs, cards, badges, tables, dialogs, tooltips, toasts,
  skeletons, stat tiles, empty states).
- Dashboard: connection status panel, stat tiles, core mini-list,
  recent activity (live log tail), skeleton + backend-unavailable
  states.
- Configurations: virtualized list (@tanstack/react-virtual), protocol
  badges, latency color coding, debounced search, filters, pagination,
  detail side panel.
- Connection: authoritative state-machine presentation with per-state
  animations, attempt history, reconnect, config picker, core
  management cards (status/version/path/pinned/preferred).
- Diagnostics: professional log viewer — severity/subsystem/search
  filters, 1-second live tail, pause/resume, clear display, copy safe
  diagnostics text, open log location; storage diagnostics and system
  info cards.
- Settings (new): preferred backend, refresh cadence, testing policy,
  log level/size/backups, reduced motion; persisted under the config
  directory, applied live.
- Motion language: unique per function (connect signal pulse,
  disconnect collapse, refresh flow, boot dots, log slide-in),
  GPU-friendly transforms/opacity only, full
  `prefers-reduced-motion` support plus an in-app reduced-motion
  preference.

## Protocol-core changes

Xray, V2Ray (V2Fly) and sing-box adapters are UNCHANGED in behavior
and remain real integrations — none replaced by fakes, none collapsed
into another. The fake core is test-only and never ships in production
builds. Selection remains deterministic
(capability → preference → priority → availability → health) with a
human-readable reason; the user's preferred backend (Settings) is
honored when compatible and available.

## Storage changes

Behavior-preserving lifecycle hardening only (see failure classes A/B):
explicit error-aware close before rename/remove on every legacy
upgrade path, deferred (non-fatal) upgrade cleanup after durable
re-journaling. Streaming, chunking, WAL, lazy reads, caching,
background flushing and bounded memory are unchanged; the on-disk
format is unchanged.

## Performance changes

- No regression in store paths; `BenchmarkMigration` and the full
  benchmark set (parsing, normalization, fingerprinting, chunking,
  storage read/write, compaction, core selection, config generation)
  remain in place and pass.
- New benchmarks: `engine/cache` (Get/Put/Mixed) and
  `internal/logging` (WriteFile/WriteRingOnly/Redact).
- UI data paths audited: configuration pages stay virtualized and
  paginated; the log viewer reads incrementally; no new API
  serializes the whole store.

## Security changes

- Runtime log redaction enforced at the single write choke point;
  documented in docs/security.md.
- Log files 0600 inside 0700 application-data directories.
- Settings persist non-sensitive preferences only (0600).
- Security workflow (govulncheck, gitleaks, static analysis) unchanged
  and passing; the new logging/UI code is covered by the same gates.

## Workflow changes

- `go` job: builds the fake-core fixture directory and exports
  `FREEIRAN_TEST_CORES` for both the plain and the race test steps;
  benchmark smoke includes `internal/logging`.
- `windows` job: builds frontend → builds deterministic fake test
  cores (`fakecore.exe` + named copies) → exposes the fixture
  directory → runs the FULL Go test matrix (no package skipped) →
  headless runtime smoke test (`go run ./cmd/freeiran --smoke-test`:
  boot → state → services → shutdown) → builds the desktop binary →
  validates the executable → uploads the artifact. No real VPN
  infrastructure is installed for ordinary tests.
- `protocol-cores` job: unchanged — pinned real Xray/V2Ray/sing-box
  binaries with SHA-256 verification run the real-binary smoke suites
  separately.
- `security.yml`, `release.yml`: unchanged.

## Files added

- `internal/logging/logging.go`, `logging_test.go`, `logging_bench_test.go`
- `engine/store/migrate_test.go` (Windows lifecycle regression suite)
- `engine/cache/cache_bench_test.go`
- `engine/app/loggingservice.go` (LogService + SettingsService)
- `system/open_unix.go`, `system/open_windows.go` (OpenDirectory)
- `frontend/bindings/.../engine/app/logservice.js`,
  `settingsservice.js`, `frontend/bindings/.../internal/logging/models.js`
- `frontend/src/components/{Dialog,ErrorBoundary,Icons,Toasts,common}.tsx`
- `frontend/src/pages/Settings.tsx`

## Files changed (high level)

- `engine/store/migrate.go`, `engine/store/wal.go` (lifecycle fixes +
  hooks), `engine/store/migrate_test.go`
- `engine/core/core.go` (lifecycle logging), `engine/core/contract/
  contract.go` (fixture harness), `engine/core/testdata/fakecore/main.go`
  (version/check/hang), staging sites in `engine/core/registry_test.go`,
  `engine/connection/connection_test.go`, `engine/tester/
  core_probe_test.go`, `engine/app/connectionservice_test.go`
- `engine/app/app.go` (logger lifecycle, settings, ingestion events),
  `engine/app/services.go` (migration/compaction events),
  `engine/app/connectionservice.go` (connection events, preference
  wiring)
- `cmd/freeiran/main.go` (early logger boot, new services,
  `--smoke-test` headless mode)
- `system/system.go` (ExecutableName export)
- `.github/workflows/ci.yml` (fixture harness + smoke test)
- `VERSION` → 0.5.0, `internal/version/version.go`,
  `frontend/package.json`, `frontend/package-lock.json`
- `README.md`, `docs/architecture.md`, `docs/ci.md`,
  `docs/development.md`, `docs/performance.md`, `docs/security.md`
- `frontend/` full UI redesign (pages, components, styles, services
  surface, models bindings)

## Files removed

None. Every subsystem retains exactly one authoritative
implementation; no dead code was introduced by this release.

## Tests

- Regression: migration close-before-rename ordering, close-error and
  rename-error preservation, idempotency, cancellation; legacy journal
  close-before-remove ordering, close-failure preservation, deferred
  upgrade on removal failure.
- Logging: file creation/structure, rotation bounds, startup recovery,
  redaction (pattern + explicit secrets + file-level), concurrent
  writes, incremental reads, shutdown flush, subscribe/cancel, global
  no-op safety, hostile-input redaction safety.
- Harness: `FREEIRAN_TEST_CORES` resolution (hard failure on missing
  fixtures), platform-correct staging, fake core version/check paths.
- Frontend: typecheck, 28 unit tests, production build.
- Full suite: `go test ./...` and `go test -race ./...` across
  `./engine/... ./system/... ./internal/...`; `go vet` clean;
  gofmt-clean; `windows/amd64` build + vet of `./cmd/freeiran`.

## Known limitations

- The Windows smoke test boots the engine headlessly; the webview
  itself is validated by the desktop build step, not interactively
  (CI runners have no interactive desktop session).
- `wails3 generate bindings` requires GTK development packages on
  Linux; the v0.5.0 binding modules were produced against the exact
  generator output format (FNV-1a method IDs verified against the
  v0.4.1 generated files) and are byte-compatible in structure.
- Real-core behavior on exotic protocols (e.g. XRAY REALITY flows) is
  validated by the dedicated real-binary CI job, not by the fake-core
  matrix.
