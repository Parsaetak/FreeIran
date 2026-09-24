# FreeIran

A lightweight, free, open-source VPN configuration manager and proxy
client for Windows (and, architecturally, any desktop platform), built
around a shared Go engine for discovering, testing, maintaining,
running and tunneling through publicly available proxy/VPN
configurations.

**Project:** FreeIran — A SHEYTAN Digital System
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.9.15 (see `VERSION`)
**Status:** production architecture — multi-core protocol runtime with
managed installation, multi-level node discovery, Ping/URL test modes
with measured ranking, verified-connection engine with racing,
environment intelligence, system proxy mode (WinINet), unified adaptive
memory control and kernel-level process supervision. TUN mode is
EXPERIMENTAL and disabled in this release (see "TUN mode" below).

---

## What's new in v0.9.15

v0.9.15 is a deep-fix release: it repairs the root causes found by a
full acquisition/testing/UI audit instead of adding retries around
them. No new subsystem — the existing queue, HTTP infrastructure,
supervision and persistence carry every change.

### Tor acquisition actually works again (root cause: host mismatch)

- The Tor Project's distribution is split across two official hosts:
  `dist.torproject.org/torbrowser/<ver>/` serves the version listing
  and `sha256sums-signed-build.txt` (the checksum authority, published
  with its `.asc` signature) but **not** the expert bundles — the
  historical single-host asset URL answered HTTP 404 for every
  `tor-expert-bundle-*`, which is exactly why managed Tor installs
  failed while every checksum lookup succeeded. `Resolve` now builds
  the asset URL against `archive.torproject.org/tor-package-archive/`
  (the official full package archive, verified live: the downloaded
  bundle's SHA-256 matches dist's published digest exactly). The
  integrity chain is unchanged: digest from dist's signed checksums,
  bytes from the official archive, VERIFY proves they agree before
  anything executes.
- An explicit Install over a healthy managed bundle now means
  "acquire the resolved release" (adoption is no longer allowed to
  silently short-circuit it); external adoption still happens whenever
  no usable managed engine exists, and external files are never
  touched.
- Local-first behavior preserved: remote metadata failure keeps any
  locally usable engine working (`EnsureAvailable`), failed downloads
  never destroy the previous verified runtime, and failures record the
  exact stage.

### Psiphon: honest provider states, first-class local path

- Verified upstream (2026-09): `Psiphon-Labs/psiphon-tunnel-core`
  releases publish ONLY mobile/client library archives (with digests),
  and `psiphon-tunnel-core-binaries` (moving `master`, no digests)
  hosts Linux ConsoleClient + psiphond only — **no authoritative
  Windows ConsoleClient artifact exists**. The managed channel stays
  honestly unavailable; master content is never downloaded as a
  managed release.
- Provider manifests and `Info` now carry `acquisition`
  (`managed-release` / `user-binary` / `external-reference`), so the
  UI distinguishes managed installation, user-supplied validated
  copies, adopted external installations and an unavailable channel
  instead of collapsing them into one boolean. The Cores page states
  plainly that Psiphon's supported install path is the validated
  user-binary workflow.

### ONE authoritative testing path (single tests converge on the queue)

- `DataService.TestConfig` no longer runs a synchronous network test
  on the UI's call. It enqueues into the SAME test queue bulk testing
  uses (priority 500 — a person is waiting) and returns promptly with
  the config's current snapshot. Repeated clicks collapse into the
  queued task (`ErrDuplicate` is success). Single tests now share every
  queue guarantee: deduplication, per-backend concurrency, supervised
  core processes, cancellation, retry, persistence.
- "Retry timed out" is a REAL scope now (classified failure reason)
  instead of a synonym for "retry failed".

### Configs UI: incremental, not rebuild-the-world

- The live test lifecycle (`queued → preparing → testing → measuring`)
  comes from ONE shared projection of the queue snapshot
  (`state/testProgress.ts`) read by rows, the detail panel and
  Connection — no page-private testing state.
- Completed tests patch their configuration in place (one GetConfig
  per changed fingerprint through the queue's own persistence path).
  The old per-click full `runSearch()` over ~17k configs is gone.
  Search, filters, sorting, organization, selection and scroll
  position all survive a test completion. Quick Connect invalidates
  once per persisted result, whatever page triggered the test.
- Connection converges with Configs: the best-candidate view refreshes
  once (debounced) when results land; the config picker reads the same
  patched store. No duplicated tests, no stale state presented as
  fresh.

### Menus: ONE viewport-aware positioning mechanism

- Every menu — trigger dropdowns (Configs, Cores) and the Configs row
  context menu (⋮ button, right-click, Shift+F10) — renders through a
  single portal-based surface (`components/MenuSurface.tsx`): anchor
  to the real trigger, measure the real popup size, prefer
  down/right, flip up/left when room runs out, clamp inside a small
  gutter at the edges, reposition on scroll/resize, and keep Escape,
  outside-click, keyboard navigation, focus return and ARIA semantics
  in one place. The hard-coded 240/340/348 geometry and the
  clip-vulnerable absolute dropdown are gone; the placement algorithm
  is pinned by unit tests across all four viewport edges.

### Housekeeping

- Dead code removed (`moveTreePayload`/`copyTreeEntry`, the unused
  manager-level `EnsureAvailable`); version surfaces synced to 0.9.15;
  new regression tests for the queue convergence, the timed-out scope,
  the Tor host split and the menu placement rules.

## What's new in v0.9.14

v0.9.14 closes the v0.9.13 update (the Windows PE resource metadata
stayed on 0.9.12 — every authoritative version surface is now 0.9.14,
proved by a structural regression test) and makes "reuse existing
valid work before repeating it" an architectural invariant:

- **FreeIran now discovers and reuses already-installed engines.** One
  bounded, platform-aware discovery authority (managed directories →
  PATH → known installation locations; never a disk walk) feeds the
  registry, the core manager, the providers and the UI. Version probes
  are cached per file identity with a bounded freshness window and
  deduplicated in flight.
- **A working installed binary beats an unnecessary download.** A
  current external core is adopted by reference; an older-but-working
  one is retained with "update available" surfaced; a newer one is
  never downgraded; only a missing or unusable local core triggers a
  download. Explicit Update (`Acquire`) still acquires the newer
  release — external files are never deleted, renamed or overwritten.
- **Artifacts and metadata are idempotent.** Complete staged archives
  are verified and reused without touching the network, partial ones
  resume, corrupt ones are discarded alone; concurrent identical
  release-metadata requests share one network request with correct
  cancellation semantics.
- **Honest provenance everywhere**: `upstream-verified` versus
  `locally-validated` trust, managed/external ownership on every core
  card, and the reuse decision recorded on the manifest. See
  [docs/reuse.md](docs/reuse.md) for the authoritative policy.

---

## What's new in v0.9.12

v0.9.12 is a **full engineering closure of the v0.9.11 state**: the
failing race gate (`TestTorLifecycleStartBootstrapStopRestart`,
observed as `restart state = "starting"`) is fixed at its root cause,
the "stale event must never mutate current lifecycle state" invariant
is enforced across the provider layer, the hand-maintained profile
bindings are now verified field-for-field in CI, and the persistence
layer can no longer silently rewrite a future schema. No tests were
modified to pass; no security, trust, verification or recovery policy
relaxed.

### The provider lifecycle root cause (fixed and regression-proved)

Two asynchronous readiness authorities competed inside the Tor
engine: the endpoint probe could return `Start()` before any
bootstrap line was observed, after which `Start()` manufactured
`bootstrap.Complete = true` — while the log-line scanner kept
draining stdout and its later `Bootstrapped 0..90%` lines overwrote
`Complete = false`, regressing the published state from `ready` back
to `starting`. On a loaded CI runner the post-restart assertion
landed inside that window. The fix is one monotonic,
generation-scoped run-state model shared by both engines
(`engine/provider/runstate.go`):

- every run gets a generation; every scanner event carries its
  generation and is DISCARDED when stale (old run, or run ended);
- observed bootstrap progress is monotonic — a late lower-% line
  cannot regress it;
- the readiness supervisor owns the verdict and follows the
  documented contract: spawn → process alive → bootstrap progress →
  `Bootstrapped 100%` observed → SOCKS endpoint verified → publish
  READY exactly once, immutable for the run. An endpoint accepting
  before 100% is EVIDENCE, not readiness;
- `Stop` and failed starts close the event gate before the process
  dies — no in-flight scanner line can mutate or resurrect state
  across a restart;
- `Start()` no longer writes bootstrap state at all.

Tor readiness now means the documented contract (100% observed AND
endpoint verified), not the endpoint shortcut; the failure path
reports honest evidence (last progress + endpoint evidence).
Psiphon follows the same model with its documented endpoints-verified
contract. The regression battery (`v0912_lifecycle_test.go`) covers
late-line monotonicity, stale generations, post-stop events,
cross-restart stale ingest, three full restart cycles with exact
state assertions, and the readiness-contract pin (a fixture that
accepts on the endpoint but never reaches 100% must FAIL, never
publish Ready).

### Contract, persistence and UI-state hardening

- Binding contract: the hand-maintained profile bindings are now
  verified FIELD-FOR-FIELD in CI (`TestProfileBindingModelsMatchGoStructs`
  reflects the Go JSON tags against `profiletypes.js`), on top of the
  existing method-existence verification — silent model drift fails
  the build instead of surfacing as `undefined` at runtime.
- Persistence: a future `store.meta` is preserved verbatim (renamed
  `store.meta.future-preserved`) with the registry rebuilt from
  chunks, instead of being silently overwritten; profiles/sources/
  collections sidecars refuse loudly to downgrade a future schema;
  settings writers share one serialization mutex; the config-order
  sidecar uses the shared atomic-write path.
- UI state: an authoritative connection event now releases the busy
  flag of the operation it invalidates — the previous leak disabled
  Connect/Disconnect/Reconnect until restart whenever the machine
  broadcast during a blocking connect. The remaining store mutation
  paths (profile activation, start-flow status, app-state polls,
  provider mode) gained generation guards against stale responses.

### Verification (executed)

Normal Go tests, the full CI-scope race suite (plus repeated targeted
runs of the previously failing test), the C++ native layer with
`native_accel`, frontend typecheck/tests/production build, the pinned
real-core smoke suites (V2Ray v5.53.0, Xray v26.3.27, sing-box
v1.14.0, SHA-256 verified), the `windows/amd64` desktop build
validation and the clean-room embed check all PASS. The Windows
test/build job and Windows runtime smoke require a Windows runner and
remain post-push CI verification (stated nowhere as already done).

---

## What's new in v0.9.11

v0.9.11 is a **full engineering closure of the v0.9.10 connection-lifecycle
work plus the first P2 roadmap feature** — the Windows test-oracle defect
behind CI run 35571120221 is fixed at its root, the archive sanitizer is
now genuinely host-independent (a real cross-platform security defect the
Windows run surfaced on audit), and **Connection Profiles** ship on the
existing settings + connection engine. No architecture replaced; no
verification, trust or recovery policy relaxed.

### The Windows lifecycle oracle (fixed, no skips, no fake values)

The v0.9.10 lifecycle regression tests were CORRECT — but the test oracle
under them was not cross-platform: `procsRunningFrom()` returned a fake 0
on every non-Linux platform while the new tests required 1, so
`TestConnectSurvivesOperationContextCancellation`,
`TestConnectSurvivesOperationContextDeadline` and
`TestReconnectReplacesPreviousSessionProcess` false-failed on Windows
(CI run 35571120221, job "Windows tests and desktop build", step
"Run Go tests" — the only failing step; the runtime smoke test and the
desktop build never ran).

The oracle is now genuinely cross-platform, with REAL evidence on every
platform:

- kernel process liveness of the recorded PID (`system.ProcessAlive`:
  OpenProcess/GetExitCodeProcess on Windows, kill(0)/EPERM on Unix);
- the session's local listener still accepting TCP connections
  (serving evidence — a live process whose listener is gone is not a
  serving session, and a port squatter without the process is not the
  session either);
- Linux: the `/proc/<pid>/exe` image scan (kept, now Linux-only BY
  CONTRACT — calling it on another platform fails the test loudly);
- Windows: the running image's executable-file lock (deletion must fail
  while the core is alive and must succeed again after teardown —
  bounded polls, never skips).

Session replacement is proven by the PID transition (old PID reports
dead, new PID alive and serving); on Linux the image scan additionally
converges to exactly one process. The session-lifetime guarantees
proved are unchanged: operation cancellation and deadline survival,
explicit disconnect, reconnect replacement, failed-startup teardown,
mid-session crash teardown, runtime-context release at every session
boundary, and no monitor/provider/core goroutine leaks.

### safearchive: host-independent path security (fail-closed on every platform)

The pre-0.9.11 sanitizer ran the host's `filepath.Clean()` over archive
entry names BEFORE validating them, so its verdicts depended on the host
OS: on Windows, a POSIX-absolute entry such as `/etc/passwd-clone`
became backslash-rooted and slipped past the absolute-entry test — the
last-resort containment check still caught it, but the documented
reject-on-sight contract silently did not hold, and several entry
classes were not rejected at all. v0.9.11 validates entry names in
ARCHIVE space (lexically, '/'-separated) before any host conversion and
finishes with an OS-aware `filepath.Rel` containment check
(case-insensitive on Windows). Rejected on every platform, identically:
POSIX-absolute, Windows-rooted, drive-absolute and drive-relative forms;
UNC (`//server/share`, `\\server\share`); device namespaces
(`\\?\`, `\\.\`, `\??\`); ALL colon usage (alternate data
streams); traversal above the root; reserved device names (CON, NUL,
COM1-9, LPT1-9, with or without extension); components with trailing
dots/spaces (Windows write-collision quirk); NUL/control characters;
oversized components. Zip entries with ANY non-regular type bit
(symlink/device/FIFO/socket) are rejected; tar specials and unknown
flags stay rejected.

Two fail-closed contract repairs came out of the same audit: a
legitimate EMPTY entry (declared size 0) no longer produces a false
"ended after 0 of 1073741824 declared bytes" rejection, and a zip entry
whose stream carries MORE data than its header declares is rejected
instead of being silently truncated into a corrupted-but-reported-
successful file. The full archive-security matrix (both formats and all
name classes above) runs in the regression suite and is
platform-independent by construction.

### Connection Profiles (P2 roadmap feature)

One new feature, built on the existing architecture: a profile is a
persistent, NAMED SET OF CONNECTION PREFERENCES (Home / Work / Travel /
Privacy) — never a second configuration store and never a second
networking flow.

- A profile carries only preferences the engine already supports: the
  Quick Connect mode (Auto / Configurations / Tor / Psiphon), an
  optional selected configuration ID, an optional preferred core,
  preferred SOCKS/HTTP ports, and the existing recovery preference.
  No credentials or secrets are ever stored.
- Persistence is ONE small versioned, atomically written sidecar
  (`config/profiles.json`) with stable profile IDs, deterministic
  ordering, the same defensive decode as the collections sidecar, and
  a documented forward-migration point (a future schema version is
  never rewritten by an older binary).
- Profiles REFERENCE configuration IDs: deleting a profile never
  touches configurations; a configuration that disappears is reported
  honestly (available=false), its profile reference is pruned at boot,
  and activating a configs profile with a dangling reference fails
  loudly instead of silently connecting elsewhere.
- Activation passes through the ONE settings path
  (validate → persist → apply → the live connection manager) — the
  live manager observes new ports immediately — and every connection
  still runs the existing verified flows. A profile can NEVER bypass
  route trust, configuration validation, testing, verification,
  recovery safeguards or provider installation safety: activation is a
  preference change, not a connection.
- UI: a lightweight profile chip row on the Quick Connect surface;
  create / edit / rename / duplicate / delete / set default live
  behind a "Manage profiles" advanced section. The surface stays
  hidden until the backend reports at least one profile, and every
  action re-renders from the authoritative backend response.

### Validation (executed for this release; scope stated exactly)

gofmt, `go vet`, the full unit suite and the full `-race` suite across
every Go package, benchmark smoke, native C++ build + tests +
cross-language tests, frontend typecheck + unit tests + production
build + embed inventory validation, windows/amd64 cross-compile of the
desktop binary AND of the test binaries for every changed package, the
Windows PE resource regenerated from the synced winres source, and the
clean-room build from the final tree. The Windows CI job could not be
re-run without pushing (see the delivery note in the changelog): the
Windows lifecycle and archive-security regressions were re-designed to
carry REAL Windows evidence and compile for windows/amd64, but a green
Windows CI run remains a POST-PUSH verification, stated nowhere as
already done.

## What's new in v0.9.10

v0.9.10 is a **connection-lifecycle architecture repair and
beginner-first product release** — the v0.9.9 flaky recovery tests are
fixed at their ROOT CAUSE, the last unfinished v0.7 roadmap work
(source reliability dashboards, configuration grouping, favorites) is
completed on the existing architecture, and the primary experience is
reorganized around **Connect → Configurations → Sources**. No security
boundary weakened; no verification, trust or recovery policy relaxed.

### The connection-lifetime root cause (fixed and regression-proved)

The persistent core/provider process was bound — through
`exec.CommandContext` — to the CALLER's operation context (the 60
second Quick Connect attempt, the 60 second service-layer connect, the
3 minute provider start). Every successful connection was killed the
moment that context expired or its `defer cancel()` ran; the monitor
then reported an "unexpected" crash and the bounded recovery loop
reconnected into the same trap. This is exactly the race behind the
v0.9.9 `TestRecoveryRequiresActualVerification` /
`TestRecoverySwitchesToNextCandidate` CI failures.

The v0.9.10 architecture separates the two contexts:

- **Operation context** (what callers pass) bounds candidate
  selection, preparation, the startup deadline, readiness waiting,
  verification and the bounded attempts — never the process lifetime.
- **Session runtime context** (owned by the active session in
  `engine/connection`) bounds the persistent core process lifetime.
  It is cancelled ONLY by explicit disconnect, shutdown, session
  replacement or unrecoverable runtime failure — a successful
  connection SURVIVES its operation context.

Provider engines (Tor, Psiphon) own the same separation internally:
the process runs on a per-run runtime context, and `Start(ctx)`
bounds only the bootstrap/negotiate wait. Session replacement is
deterministic — a provider session taking over from a running core
session CLOSES the previous core (the pre-0.9.10 boundary dropped the
instance reference and orphaned the process). The crash transition is
now a true session boundary (generation bump + runtime-context
release), matching the stability-teardown semantics.

Regression-proved with real processes (the deterministic fakecore and
fake Tor stand-ins), and each proof FAILS on the v0.9.9 tree:
connect survives operation-context cancellation AND deadline; the core
stays alive after `Connect` returns; explicit disconnect terminates
it; reconnect replaces the previous session's process; cancelled and
failed startups tear down cleanly; crashes release the session
context; Quick Connect and recovery sessions stay `connected_verified`
across an observation window; Tor survives its operation context; no
goroutine leaks across cycles.

### v0.7 roadmap work completed (evidence only, never invented)

- **Source reliability dashboard** (Sources page): per-source fetch
  evidence, last-refresh pipeline evidence (discovered / duplicates /
  invalid per source) and a bounded store scan (persisted, tested,
  working, failed, untested, stale counts, median measured latency,
  last successful test). Sources removed from the registry keep their
  evidence visible. Where evidence is insufficient the dashboard says
  **Not enough data** instead of a score; the report is cached and
  invalidated by ingestion cycles.
- **Configuration grouping** (Configs page): built-in evidence groups
  (All / Favorites / Working / Untested / Fast / Recently tested)
  computed from measured records, persistent user groups (Work /
  Personal / Travel / anything) stored by stable IDs in one versioned
  sidecar (no duplicated configuration storage), and organize-by
  (source / protocol / status) with live counts — all through the ONE
  existing server-side filter pipeline.
- **Favorites / saved routes**: star any configuration; favorites
  never bypass testing, verification or the route-trust policy.

### Beginner-first product experience

- Navigation is now **Connect → Configurations → Sources**, with every
  technical/diagnostic surface (Dashboard, Connection, Cores, Network,
  Diagnostics, Settings) under a secondary **More** section —
  progressively revealed, never removed, consistent on every page.
- Quick Connect failures explain themselves: **What happened → What
  FreeIran is doing → What you can do**, with the raw technical detail
  behind an expandable section (trust and verification failures keep
  their full meaning). A **Fix my connection** action runs the
  adaptive discover → test → rank → connect flow with live progress.
- Contextual education where confusion is likely (what a configuration
  is, what verification means, why a core is needed, what favorites
  do) — small hints, not documentation.
- The Cores page explains WHY a core is required before asking for an
  install, and the Connection page gained a compact session status
  card (verification state, route, recovery activity — a read-only
  projection of the same bounded recovery service).

### Startup and performance

The six secondary surfaces are code-split (`React.lazy`) and load on
first visit, shrinking the initial bundle while every loading state
represents real work. The stable-filename embed contract now covers
the deterministic page chunks (hash-free, module-named); hashed
artifacts remain forbidden. Wails bindings were regenerated with the
pinned toolchain (two consecutive generations byte-identical).

### Validation (all executed, results in the release notes)

Full matrix: gofmt, `go vet`, unit tests, `-race` across every
package, benchmark smoke, frontend typecheck + tests + production
build, embed inventory, native C++ build + tests + cross-language
tests, Windows/amd64 cross-compile, and the clean-room build from the
final tree.

**Errata (v0.9.11):** the release's own CI run 35571120221 had one
failing job — "Windows tests and desktop build" failed at the
"Run Go tests" step (the runtime smoke test and the desktop build
never ran). The three failures were a TEST-ORACLE defect (see the
v0.9.11 notes): the lifecycle regression tests themselves were
correct and the v0.9.10 architecture held on Linux (unit + race +
native + frontend + real-core smoke all passed in the same run).
This section's original list was accurate about what ran, but the
release narrative did not state that the Windows job was red at the
time of writing — corrected here so no historical claim exceeds the
evidence.

---

## What's new in v0.9.9

v0.9.9 is a **core-engine execution and runtime upgrade** — the whole
execution path (select → prepare → generate config → allocate port →
spawn core → supervise → readiness → verify → publish state → monitor
→ recover → cleanup) was audited, measured and repaired. No new
connectivity features; no security boundary weakened; no architecture
deleted.

### Correct execution path and truthful metrics

- The frontend job's "Clean-room embed check" ran under the job-level
  `working-directory: frontend`, so every repo-root-relative path it
  used resolved to the nonexistent `frontend/cmd/...` tree (CI run
  35542123824) — and the `git diff -- <path>` guard passed vacuously
  on a nonexistent pathspec. The check now runs from the repository
  root and the inventory assertions live in ONE shared validation
  source (`frontend/scripts/embed-inventory.mjs`) used by both the
  staging script and CI.
- Every connection-manager field access is mutex-guarded: the
  startup-crash retry path read `m.state` bare and `Reconnect` read
  `lastPref` bare (both raced Disconnect/Reconnect transitions);
  provider failure paths are generation-gated so a superseded session
  can never clobber a newer session's state.
- Startup metrics counted once: `core_start` success and the startup
  timing are recorded exactly once, at readiness (previously both were
  recorded again at the verification boundary — every successful
  connection was double-counted).

### Consolidated readiness execution

- ONE authoritative startup supervision path per core launch: the
  duplicated per-consumer process-wait observers and the redundant
  "warm" listener probe after readiness are gone. Readiness detection
  uses a bounded adaptive schedule (immediate probe, then a
  2/5/10/20/40/80 ms ramp to a 100 ms cadence) with one reusable
  timer — the fixed 100 ms `time.After` polling loop allocated a
  fresh timer per iteration. The startup-timeout teardown (bounded
  stop of a core that never opened its listener) lives in the
  supervisor, so every consumer observes the same verdict.

### Centralized port resolution

- `core.ResolveInboundPort` is the single execution-stage port
  resolver: explicit user-selected ports pass through unchanged,
  ephemeral ports allocate exactly once BEFORE configuration
  generation (the generated document embeds the final port). The
  three adapters' duplicate allocation paths and the shared launcher's
  second allocation path were removed. User-port conflict detection,
  the ephemeral-port race retry and reconnect port stability are
  unchanged and regression-tested.

### Recovery and monitor lifecycle

- `RecoveryService.Stop` now cancels the recovery lifecycle context
  itself and JOINS both the watch loop and any in-flight recovery
  decision — an application shutdown can no longer race an in-flight
  recovery Quick Connect, no new recovery attempt can begin after
  shutdown, and no recovery goroutine survives.
- Process death is observed as an EVENT through `Instance.WaitProcess`
  with immediate crash detection; the monitor tick's duplicated
  `Health` process poll was removed (one lifecycle fact, one watcher,
  zero-interval detection instead of up to one monitor interval).

### State publication semantics

- The publisher's queue contract now matches its documentation:
  lifecycle-critical transitions (selecting, preparing, starting_core,
  waiting_for_ready, verifying, connected_verified, disconnecting,
  disconnected, connection_failed) are NEVER silently dropped under
  queue saturation. Overflow compaction merges same-stage pending
  entries (newest wins) and sheds replaceable telemetry first; memory
  stays bounded.
- Snapshot deduplication uses an explicit semantic equality instead of
  reflection (measured on the real snapshot type: 28 ns / 0 allocs vs
  424 ns / 2 allocs per comparison).
- The Wails delivery boundary is bounded and nonblocking
  (`statepub.BoundedEmitter`): a slow or stalled webview event
  pipeline can no longer stall the engine's state publication path.
  No polling was reintroduced.

### Selection, caching and startup priorities

- Quick Connect collects candidate records in ONE bounded store scan
  (previously: a full bounded scan whose result was discarded as a
  capacity hint, then an UNBOUNDED second scan) and fresh-tests stale
  shortlist entries through a fixed 3-worker pool instead of one at a
  time. Shortlist limit, trust policy, cooldowns and the verification
  gate are unchanged.
- The generated-config cache contract is truthful: the invalidation
  generation now really covers the backend version, and per-launch
  coordinates (ports) moved into the cache key — one candidate's port
  change no longer invalidates every other cached document.
- Heavy background work (storage verification, cache warming, the
  boot ingestion cycle) is staged behind a short warmup window so it
  cannot compete with the first interactive connection; the
  core-registry refresh (readiness-critical) stays immediate.
- Tor bootstrap completion is EVENT-driven (log scanner closes a ready
  channel on "Bootstrapped 100%"); Tor/Psiphon endpoint probes ride a
  shared adaptive schedule instead of fixed 200/250 ms tickers.
- Tunnel verification shares one immutable SOCKS dialer per
  verification round; target isolation, bounded concurrency, quorum
  and transient-retry classification are unchanged.

### CI version-check hardening

- The version-consistency check hard-coded the `0.9` series regex and
  an exact-match rule that could not reconcile a 3-part VERSION with
  the legitimate 4-part PE metadata form (`x.y.z.0`). It now accepts
  exactly `$version` or `$version.0` for any series and still fails
  on any stale literal.

---

## What's new in v0.9.8.8

v0.9.8.8 was a **deep cleanup, stable-filenames and repository-hygiene
release** — no new connectivity features, no safety boundary weakened.

### CI recovered (run 35519469195)

- The frontend typecheck failed with four `TS2393 Duplicate function
  implementation` errors: two byte-identical copies of the CSV export
  worker (`src/workers/export-worker.ts` and `src/workers/
  exportWorker.ts`) were both tracked, and because a worker module has
  no top-level imports/exports, TypeScript treated both as global
  scripts — their identically-named functions collided. The duplicate
  is deleted; the canonical import (`export-worker?worker`) is
  unchanged and the build emits exactly one `export-worker.js`.

### Final unified asset filename contract

- The embedded production tree is now EXACTLY `index.html`,
  `assets/index.js`, `assets/index.css`, `assets/export-worker.js` —
  stable logical names produced directly by the build (they are the
  entry's natural names; the previous release's index→app renaming
  step is gone), replaced in place on every release. All six stale
  hashed/legacy artifacts that the v0.9.8.7 tree still carried
  (app.js, app.css, two `exportWorker-*.js`, hashed `index-*` bundles)
  are deleted; the v0.9.8.7 asset pipeline never actually landed in
  the committed tree because the typecheck failure blocked the
  regeneration. `copy-dist.mjs` wipes the target completely, copies
  the fresh output and verifies the exact inventory; CI enforces the
  same allowlist plus an explicit hashed-filename scan.

### State publishing without artificial delay

- `internal/statepub` is now the single publisher boundary for both
  UI streams. The connection manager owns its snapshot publisher and
  pushes every real transition; the app layer no longer stacks a
  second dispatch layer on top. The 25 ms sleep inside the delivery
  loop is gone: delivery is ordered, deduplicated and ZERO-delay, and
  a bounded queue guarantees that fast lifecycle bursts
  (selecting → preparing → starting_core → waiting_for_ready) are
  delivered in order with none silently lost (regression-proved).
  `Stop()` drains pending snapshots before terminating, so the final
  terminal state always reaches the UI.

### Ownership-aware installer

- The uninstaller no longer runs broad `taskkill /f /im <name>.exe`
  sweeps (which could terminate an unrelated user process sharing an
  image name). The application records every supervised child process
  (PID + executable path) in a managed-process manifest under the
  workspace runtime directory; the uninstaller terminates only
  FreeIran.exe instances whose path is exactly the installed copy,
  plus manifest-recorded PIDs each verified against its current
  executable path before termination. Graceful install-time closure
  stays with the Windows Restart Manager.

### Repository hygiene

- Wails bindings verified reproducible (two consecutive generations
  byte-identical; every binding machine-generated, no hand-written
  shims — stale documentation claiming otherwise is corrected).
- Duplicate-implementation audit, dead-file scan, stale-reference
  scan and a documentation truth pass completed; release-narration
  comments replaced with invariant descriptions.

---

## What's new in v0.9.8.7

v0.9.8.7 was a **determinism and responsiveness release** — the same
FreeIran architecture with the waiting removed, not the safety
boundaries. No new connectivity features. (Its stable-asset-filenames
work was completed and superseded by v0.9.8.8 — see above.)

### CI recovered (run 35492972394)

- The "Wails toolchain pair consistency" check compared the Go module
  version (`v3.0.0-beta.19`) against the npm version
  (`3.0.0-beta.19`) string-wise, so the Go module's leading `v` was
  treated as version drift and every downstream stage (Go tests, race
  tests, Windows tests, Windows desktop build) was skipped. The check
  now normalizes the optional leading `v` on both sides before
  comparing; a real mismatch still fails the job.

### Event-driven UI synchronization (no more tickers)

- The desktop entrypoint no longer broadcasts `freeiran:state` /
  `freeiran:connection` from 2-second ticker loops. Both streams are
  published from the AUTHORITATIVE transition paths (boot phases,
  degraded/healthy transitions, ingestion start/finish, shutdown,
  every connection state-machine mutation) through a deduplicating
  publisher (`internal/statepub`): identical snapshots never emit,
  and `Stop()` joins the publisher goroutine so no callback can fire
  into a closing UI runtime. (The initial 25 ms coalescing window
  was removed in v0.9.8.8 in favor of ordered zero-delay delivery.)
  A slow heartbeat watchdog remains in the memory/recovery
  services where it is diagnostics, not synchronization.
- The frontend keeps its event subscriptions and generation guards;
  the previous always-on 5 s provider poll on the Cores page now runs
  ONLY while a provider is actually transitioning.

### Stable, unified frontend asset filenames

- The build configuration moved to stable, hash-free asset names and
  the previous release's 14 accumulated hashed bundles were addressed
  at the configuration level. (The v0.9.8.7 committed embed tree was
  never actually regenerated with this pipeline — a typecheck failure
  blocked CI before the embed check could run — so the stale hashed
  artifacts remained in Git until v0.9.8.8 completed the work with
  the final `index.js` / `index.css` / `export-worker.js` names.)
  The Wails asset handler is wrapped with a `Cache-Control: no-cache`
  policy so an upgraded binary never serves stale JavaScript/CSS
  from the webview cache.

### Truthful Wails bindings

- The committed bindings were regenerated with the pinned wails3
  toolchain (second generation byte-identical). The regeneration
  removed the old hand-maintained ByName shims, surfaced the real
  optionality of Go pointer fields, and exposed two real defects:
  a phantom `log_retention_days` UI field (removed) and local inbound
  port preferences whose controls shipped in v0.9.8.3 but were never
  wired into the backend. The port settings are now persisted,
  validated server-side (0 or 1024-65535) and applied to the live
  connection manager on save.

### Reliability & security verification

- The two dormant fakecore failure injections are now regression
  coverage: a core that never becomes ready is force-stopped with the
  executable deletable and the temp workspace removed
  (`FAKECORE_HANG`), and a mid-session crash transitions the session
  to `connection_failed` with deterministic cleanup
  (`FAKECORE_CRASH_AFTER_START`).
- Core-install asset URLs are now HTTPS-only at code level (loopback
  test authorities excepted), matching what the security
  documentation always claimed.
- The `BestCandidates` ranking path now labels every candidate with
  its source route-trust band, matching the Quick Connect chosen view.

---

## What's new in v0.9.8.6

v0.9.8.6 is a **reliability, security and hygiene release** — no new
connectivity features. It fixes the v0.9.8.5 Windows CI failure at its
root (nondeterministic session teardown), closes the stale-result and
route-trust boundaries, removes the unsafe unfinished TUN backend, and
aligns executable trust, archive handling, the HTTP proxy policy, the
Wails toolchain pair and the documentation with what the code actually
does.

### Deterministic Windows session teardown

- `engine/connection`: `stopMonitor` now JOINS the monitor goroutine
  (cancelling alone is not synchronization), the stability teardown
  completes BEFORE `connection_failed` becomes observable, and
  teardown errors are preserved as evidence instead of being dropped.
  Returning from Disconnect/Shutdown now PROVES the supervised process
  is gone — snapshots carry `core_pid` and a regression test asserts
  connect → stability failure → teardown → process gone → executable
  deletable → temp directory removable. This is the root cause of the
  v0.9.8.5 `TestConnectionStabilityDegradation` TempDir failure
  ("Access is denied": `v2ray.exe` outlived the test).
- The monitor crash path now closes the crashed instance's owned
  runtime files instead of leaking its temporary directory.

### Session generations (stale results can never poison newer sessions)

- Every session boundary (new session, disconnect, reconnect,
  shutdown, stability teardown) increments a generation. Every
  asynchronous verification — monitor recheck, connect-time probe,
  manual `VerifyConnected` — captures the generation and may apply its
  result only while that generation is current; stale successes and
  stale failures are both discarded. Deterministic tests cover
  disconnect/reconnect/shutdown during an in-flight verification.

### Route-trust boundary (public nodes are untrusted routes)

- Sources are classified `official | user | public`; every ingested
  configuration carries its source's trust band. Quick Connect / Auto
  connect through trusted routes only unless the user explicitly
  enables "Allow public untrusted routes" in Settings; public nodes
  remain fully usable through explicit selection. A public node can be
  fast + stable + verified reachable + untrusted — reliability and
  route trust are separate dimensions.
- The invalid `nirevil-vless` default (a README documentation URL with
  zero configurations; its referenced subscription paths 404) was
  removed. The pipeline now stamps each config's source identity (the
  field was previously never populated).

### TUN mode disabled (honest, not faked)

- The unfinished Wintun backend was REMOVED: its route tracking was
  inverted, DNS "restore" wrote DHCP instead of the prior state,
  Wintun was downloaded via raw curl/PowerShell with no digest
  verification, and extraction was unbounded. TUN is reported as
  experimental/unavailable on every surface, and TUN is NOT a kill
  switch — process supervision does not filter packets. A
  transactional implementation is required before it can return.

### Executable trust and bounded archives

- No remotely acquired executable may become runnable without
  AUTHORITATIVE integrity evidence: core installs are now REJECTED
  when a release publishes no digest (a locally computed SHA-256 is
  tamper evidence, never a trust anchor), and asset URLs must be HTTPS
  (loopback authorities excepted). Provider binaries keep their
  mandatory checksum gate.
- All core/provider archive extraction moved to
  `internal/safearchive`: bounded archive/total/per-file/file-count
  limits, path-traversal and absolute-path rejection, symlink/
  hardlink rejection and fail-closed malformed-archive handling, with
  zip-bomb and tar-slip test batteries.

### Explicit HTTP proxy policy

- One transport policy (`direct | environment | user URL | tunnel`)
  with `direct` as the default everywhere: ambient HTTP_PROXY/
  HTTPS_PROXY/ALL_PROXY are no longer silently inherited by the
  production client, the SSRF-guarded discovery client (where a proxy
  would bypass the dial-time destination validation) or netcheck's
  direct-path measurements. Opting in is explicit; tests cover
  proxy-assisted destination confusion.

### Wails contract, security CI and hygiene

- The Wails toolchain pair is pinned and CI-enforced: go.mod
  `wails/v3 v3.0.0-beta.19` + `@wailsio/runtime 3.0.0-beta.19`
  (the v0.9.8.5 lockfile had drifted to runtime beta.20). A bindings
  contract test verifies every hand-written `Call.ByName` target
  exists on its Go service; the embed output must regenerate clean
  (`npm run build:embed` + `git diff --exit-code`) so stale hashed
  bundles can never accumulate again — the tree carried 14 asset
  files of which only 3 were referenced.
- `security.yml` gained an allowlist-based audit of the Windows
  child-process surface (PowerShell/cmd/curl/wget/netsh/route/
  archive-extraction patterns) with every exception justified
  inline; its broken push trigger (`branches: ain]`, which never
  fired) was repaired.
- The release pipeline gained an EXPLICIT Authenticode architecture:
  when signing secrets are configured, `FreeIran.exe` and the
  installer are signed and `Get-AuthenticodeSignature`-verified in
  CI; without secrets the release ships UNSIGNED and says so in
  `SIGNING-STATUS.txt` and the release notes — signing is never
  fabricated.
- Removed: the stale `worklog.md`, the obsolete
  `cmd/freeiran/rsrc_windows_386.syso` (386 assumptions with no 386
  build), the `tools/neteval` helper and 11 stale hashed frontend
  bundles. Release history moved to `CHANGELOG.md`.

Older release notes live in [CHANGELOG.md](CHANGELOG.md).

## Vision

> Continuously find publicly available configurations, test them, keep
> the ones that work, archive the ones that fail, remove duplicates,
> and make the working pool immediately usable from a lightweight
> client.

FreeIran is intended for environments where ordinary Internet
connectivity can be heavily restricted, including Iran. It is local-
first: no account, no central backend, no cloud service, no remote
telemetry. All network activity relates to fetching public
configuration sources, downloading official core binaries from their
upstream release pages, or testing configurations.

---

## Architecture Overview

```text
                       FREEIRAN
                          │
                    WAILS v3 APP
                          │
                 TypeScript UI layer        (frontend/)
                          │
                   Generated bindings        (frontend/bindings)
                          │
                    Go application
                     orchestration           (engine/app)
                          │
        ┌─────────────────┼─────────────────┐
        │                 │                 │
    Data Engine       Core Manager      System Engine
  (engine/store,     (engine/coremgr:   (system/)
   engine/chunks,     install / update /    │
   engine/cache)      rollback / health)   C++ native layer
        │                 │                (native/, engine/native)
   Ingestion pipeline    │                optional, with Go fallback
   (engine/pipeline,     │
    engine/source)    Connection Manager
        │            (engine/connection: state machine, fallback)
   FETCH → PARSE →        │
   NORMALIZE →       ┌────┴────┐
   VALIDATE →        │         │
   DEDUP → PERSIST   │         │
                     │         │
              Protocol cores (engine/core)
              ┌──────────────┬──────────────┐
              │ xray         │ v2ray        │ sing-box
              └──────────────┴──────────────┘
                           │
              Test Queue (engine/testqueue)
              bounded workers + priority + cancellation
                           │
              Tunnel (engine/tunnel)
              System Proxy (WinINet) — production
              TUN — experimental, DISABLED (v0.9.8.6)
```

- **Go** is the primary orchestration/system language: lifecycle,
  scheduling, concurrency, storage, sources, testing, the managed
  protocol-core lifecycle, the test queue, the tunnel modes and the
  API exposed to the UI.
- **C++** provides a small, measurable acceleration layer (batch
  hashing, CRC-32 checksums, URL scanning) behind a stable C ABI,
  with bit-exact pure-Go fallbacks so correctness never depends on it.
- **TypeScript (React + Vite)** is the UI layer: application state,
  configuration browsing with virtualized lists, source management,
  diagnostics, core manager, test queue, tunnel mode selector. The
  UI talks to Go exclusively through generated Wails v3 bindings and
  events — there is no localhost HTTP API.

Details: [docs/architecture.md](docs/architecture.md),
[docs/storage-format.md](docs/storage-format.md),
[docs/performance.md](docs/performance.md),
[docs/ci.md](docs/ci.md), [docs/security.md](docs/security.md),
[docs/development.md](docs/development.md).

---

## Building

Requirements: Go **1.26.8** (the go.mod toolchain — see
[docs/development.md](docs/development.md) for the toolchain policy),
Node.js 22+, npm, a C++17 compiler (optional — only for the native
acceleration layer), make (optional).

```bash
# 1. Build the frontend and stage it for the Go embed
cd frontend
npm ci
npm run build:embed     # builds and copies dist into cmd/freeiran
cd ..

# 2. Build the desktop application (Windows amd64 primary target)
#    from a Windows machine:
go build -trimpath \
  -ldflags "-s -w -X github.com/Parsaetak/FreeIran/internal/version.Version=$(cat VERSION | tr -d '[:space:]')" \
  -o FreeIran-windows-amd64.exe ./cmd/freeiran

# From Linux/macOS you can cross-compile the Windows binary:
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -o FreeIran-windows-amd64.exe ./cmd/freeiran
```

### Optional C++ acceleration

```bash
make -C native            # builds libfreeiran_native + runs its tests
CGO_ENABLED=1 go build -tags native_accel -o FreeIran.exe ./cmd/freeiran
```

Without the `native_accel` tag the binary uses the pure-Go
implementations, which are tested to produce identical results.
`FREEIRAN_NATIVE=off` disables the native path at runtime.

### Development

```bash
go test ./engine/... ./system/... ./internal/...   # Go tests
go test -race ./engine/...                          # race detector
cd frontend && npm test                             # frontend tests
cd frontend && npm run dev                          # Vite dev server
```

A committed placeholder at `cmd/freeiran/frontend/dist` keeps `go build
./cmd/freeiran` working before any frontend build; the real UI is
staged by `npm run build:embed` (used by CI).

---

## Repository Structure

```text
FreeIran/
├── cmd/freeiran/          Desktop application entrypoint (Wails v3)
├── engine/                Shared Go engine
│   ├── app/               Application orchestration + UI service surface
│   ├── cache/             Bounded LRU cache layers
│   ├── chunks/            Chunking subsystem (FIRC format)
│   ├── config/            Universal configuration model + fingerprints
│   ├── core/              Protocol-core execution boundary (adapters)
│   ├── coremgr/           [v0.6] Managed Core Manager (install/update/rollback)
│   ├── connection/        Connection manager + state machine + failover
│   ├── errors/            Structured, classified errors
│   ├── metrics/           Local performance counters
│   ├── native/            Go↔C++ bridge (pure-Go fallbacks)
│   ├── netcheck/          Connectivity diagnostics + [v0.9.8.1] Internet tools engine
│   ├── parser/            Multi-format configuration parser
│   ├── pipeline/          Streaming ingestion pipeline (worker pools)
│   ├── provider/          [v0.9.8.1] Provider architecture: Tor, Psiphon, cores
│   ├── scheduler/         Interval scheduler (skip-if-busy, jitter)
│   ├── source/            Source model + HTTP fetcher + collector
│   ├── store/             Chunked persistence: WAL, memtables, compaction
│   ├── tester/            Probe interface + TCP / core probes + [v0.9.8.1] latency semantics
│   ├── testqueue/         [v0.6] Bounded-worker test queue (priority, retry, cancel)
│   └── tunnel/            [v0.6] System Proxy (WinINet); TUN disabled (v0.9.8.6)
├── frontend/              TypeScript UI (Vite + React + zustand)
├── native/                C++ acceleration layer (C ABI, no deps)
├── system/                System engine: paths, processes, network, platform
├── internal/version/      Single source of truth for versioning
├── .github/workflows/     CI, release and security pipelines
├── docs/                  Architecture, storage, performance, CI, security, dev
├── CHANGELOG.md           Release history (moved out of README, v0.9.8.6)
└── VERSION                Application version (0.9.11)
```

---

## Data Migration

Existing users of the v0.1 JSON database keep their data:

1. On first run (or via *Diagnostics → Migrate*), the legacy JSON file
   is detected and validated.
2. Records are streamed into the chunked store in bounded batches
   with their original fingerprints preserved.
3. The import is verified record-by-record through the full disk
   path in a second streaming pass.
4. The legacy file is renamed to `<original>.migrated` — never
   deleted.

Migration is idempotent: running it again is a no-op, and an
interrupted run is safely re-runnable.

---

## Security & Privacy

- Downloaded configuration data is untrusted input: it is parsed,
  normalized, validated and deduplicated before storage or testing.
- **Protocol-core binaries are downloaded only from the official
  upstream GitHub Releases** of XTLS/Xray-core, v2fly/v2ray-core and
  SagerNet/sing-box, over HTTPS. An install is REJECTED when a
  release publishes no authoritative digest (release-API digest or
  .dgst sidecar) — a locally computed hash is tamper evidence, never
  a trust anchor. Provider binaries (Tor, Psiphon) carry the same
  mandatory checksum gate, and every archive extraction is bounded
  (`internal/safearchive`).
- No arbitrary scripts are executed; no certificates are installed; no
  credentials are written to logs (log paths pass through redaction).
- The app is local-first: no account, no cloud, no browsing history,
  no remote telemetry. Metrics are local diagnostics.
- CI runs `govulncheck` and `gitleaks` on every push; see
  [docs/security.md](docs/security.md).

---

## Roadmap

- [x] v0.1 — Engine foundation (config model, parser, dedup, JSON store)
- [x] v0.2 — Architecture upgrade: chunked store, streaming pipeline,
      caching, native acceleration layer, Wails v3 desktop shell,
      TypeScript UI, CI/CD, migration
- [x] v0.3 — Storage lifecycle rework (Windows-safe resource ownership,
      segmented WAL, background flush), streaming migration, toolchain
      policy (go1.26.8), CI/security modernization, deep diagnostics
- [x] v0.4 — Protocol core integration: Xray + V2Ray (V2Fly) +
      sing-box as real backends, deterministic selection, connection
      state machine, core-based testing, real-binary CI verification
- [x] v0.5 — Windows lifecycle repair, deterministic fake-core test
      harness, persistent runtime logging with redaction, professional
      UI, settings persistence
- [x] v0.6 — **Managed Core Manager (install/update/rollback),
      no-console process launch, Test Queue with bounded workers,
      System Proxy (WinINet), Speed Booster, expanded
      sources with metadata, capability-driven failover** (the TUN
      mode shipped in v0.6 was DISABLED in v0.9.8.6 — its backend was
      not transactional and its Wintun acquisition was unverifiable)
- [x] v0.7 — UI polish, source reliability dashboards, config grouping (completed in v0.9.10)
- [x] v0.9.11 — Windows lifecycle oracle repair, host-independent archive
      security, Connection Profiles (first P2 roadmap feature)
- [ ] v1.0 — Stable releases, security review, reproducible builds

---

## Attribution & License

**Architect / Project Originator:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran

License: see [LICENSE](LICENSE).
