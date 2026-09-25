# FreeIran CI Architecture

Three workflows under `.github/workflows/`, all using current
(v7/v8-era) GitHub Action majors with Node 22 runners. Every step is a
real gate: no `|| true`, no allow-failure annotations, no skipped
packages. Workflow files must contain NO duplicate YAML mapping keys
(a duplicate key makes the entire file invalid — the v0.3.0/v0.4.0
`ci.yml` declared two `env:` blocks on one step, so every run failed
instantly with zero jobs started); `scripts` and reviewers treat that
class of defect as a hard failure.

## ci.yml — continuous integration (push/PR to main)

```text
go (ubuntu)                     native (ubuntu)      frontend (ubuntu)
├─ gofmt check                  ├─ make -C native     ├─ npm ci (cached)
├─ go vet (engine/system/… )    │   test              ├─ typecheck (tsc)
├─ go build engine              ├─ build with         ├─ vitest
├─ go test                      │   native_accel      ├─ vite build
├─ go test -race                ├─ cross-language     └─ dist artifact
├─ benchmark smoke              │   tests
└─ windows/amd64 desktop        └─ bridge benchmarks
   compile-validation
                       │
protocol-cores (ubuntu)         │
├─ install pinned v2ray 5.53.0  │  (SHA-256 verified, official sources)
├─ install pinned xray 26.3.27  │
├─ install pinned sing-box 1.14 │
├─ version-report checks        │
├─ v2ray adapter smoke (real)  │  ← v2ray test + run + listener + stop
├─ xray adapter smoke (real)   │  ← xray run -test + run + stop
└─ sing-box adapter smoke      │  ← sing-box check + run + stop
                       │
                       ▼ (needs: go + frontend + protocol-cores)
              windows (windows-latest)
              ├─ npm ci && npm run build:embed
              ├─ go test -count=1 ./...      ← full matrix, incl. store
              ├─ desktop build (ldflags version)
              ├─ executable smoke check
              └─ artifact upload
```

### The protocol-cores job

Adapter correctness has two layers. The deterministic layer —
contract suite, capability declarations, config structure — runs in
the `go` job against the fake core helper with no protocol core
installed. The acceptance layer installs the three pinned releases
(checksum-verified downloads from the official repositories) and runs
the smoke suites: each core's own config validator
(`v2ray test` / `xray run -test` / `sing-box check`) accepts every
generated document, and a full startup cycle (spawn → local listener
ready → shutdown → cleanup) executes for every supported protocol
combination.

The pinned SHA-256 values are the hashes of the official release
archives exactly as published (v2fly/v2ray-core v5.53.0,
XTLS/Xray-core v26.3.27, SagerNet/sing-box v1.14.0, all linux-64
assets). The v0.4.0 workflow pinned three INCORRECT checksums, which
would have failed the `sha256sum -c` gate on every run; they were
re-verified against the downloaded official assets and corrected in
v0.4.1. When a core pin is upgraded, the new checksum MUST be taken
from an actually downloaded archive (never transcribed from memory).

No public proxy server is ever contacted: the smoke tests exercise
config acceptance and the local runtime lifecycle only, so CI
correctness never depends on external infrastructure.

### Why the Windows job runs the full test matrix

`engine/store` owns file descriptors, and Windows is the only platform
where an open handle blocks file deletion. The lifecycle tests
(`open → use → close → delete temp dir`, repeated open/close, cache
eviction, compaction + close, concurrent read + close) therefore only
prove what they claim on the Windows runner. Skipping `engine/store`
there — or marking its failures allowed — would hide exactly the class
of regression that broke CI before v0.3.0. The v0.3.0 store passes the
full matrix on both platforms.

### Toolchain pinning

Every Go job uses `actions/setup-go@v7` with `go-version: "1.26.8"`
and `GOTOOLCHAIN: local`. If the pinned version and `go.mod`'s
`toolchain go1.26.8` ever disagree, the job fails loudly instead of
silently downloading a newer toolchain mid-build.

### Caching

- `setup-go` caches Go modules and the build cache.
- `setup-node` caches npm state keyed on
  `frontend/package-lock.json`.

## release.yml — tags `v*.*.*`

1. **verify** (ubuntu): `VERSION` and `frontend/package.json` must
   match the tag; Go tests, race tests and native C++ tests must pass.
2. **build** (windows): frontend build + embed staging, versioned
   desktop build, zip + SHA-256 checksum, size smoke test.
3. **publish** (ubuntu): attaches zip and checksum to a GitHub Release
   with generated notes; pre-release tags produce pre-releases.

A failing validation step aborts the pipeline — a release never
publishes broken artifacts.

## security.yml — see docs/security.md

Runs on push, PRs and weekly on schedule. Dependency vulnerabilities,
secret scanning and static analysis with real failure conditions.

## v0.9.4 — version gate, Windows installer, release checksums

- **Version consistency gate (ci.yml).** Every push verifies that
  `VERSION`, `frontend/package.json` and `internal/version/version.go`
  agree; release.yml additionally verifies the git tag. Version drift
  now fails in minutes instead of at release time.
- **Windows installer (release.yml).** The Windows job builds
  `FreeIran-Setup-vX.Y.Z-windows-amd64.exe` with Inno Setup
  (`scripts/freeiran.iss`; preinstalled on hosted runners, Chocolatey
  fallback). The installer writes `installed.marker`, registers
  shortcuts and uninstall metadata, stops a running FreeIran before
  replacing files, and never touches user data (which lives in the
  per-user workspace, see docs/workspace.md).
- **SHA-256 checksum sidecars (§20/§30).** Every release artifact
  (both deployment ZIPs and the installer) ships a `.sha256` sidecar,
  published alongside it. The v0.9.1 "no checksum files" policy is
  inverted: checksums are REQUIRED so the application updater can
  verify downloads without trusting the transport.

## v0.9.5 — the v0.9.4 regression actually fixed

- v0.9.4 shipped with the three obsolete path/portable helpers still
  present, so `go vet`, `go build` and all test jobs failed with
  duplicate declarations of
  `DefaultBaseDir`/`CacheBaseDir`/`portableRoot`/`PortableMode`.
  `system/paths_unix.go`, `system/paths_windows.go` and
  `system/portable.go` are now really deleted; the
  `TestWorkspacePathAuthoritySingleSource` guard added in v0.9.4 keeps
  failing any future re-introduction.
- Repository hygiene: the stale per-session packaging manifests at the
  repository root were removed, and the Wails embed directory
  (`cmd/freeiran/frontend/dist`) was reduced to exactly the current
  frontend build output.
- Version sources synced to 0.9.5 including the
  `package-lock.json` root entry, which had silently stayed at 0.9.1
  since that release (the CI version gate checks package.json, not the
  lockfile — the lockfile drift is now fixed at the source).

## Deterministic fake-core fixtures (v0.5.0)

Every ordinary unit/integration test runs against controlled fake
protocol-core executables — never real VPN binaries and never whatever
happens to be installed in the runner's `PATH`.

```text
engine/core/testdata/fakecore (Go source, single binary)
        ↓ go build during CI (fakecore[.exe] + fake-xray/-v2ray/-sing-box copies)
        ↓ $RUNNER_TEMP/freeiran-test-cores/
        ↓ FREEIRAN_TEST_CORES environment variable
        ↓ engine/core/contract.BuildFakeCore / StageFakeCore
        ↓ registry discovery → full lifecycle test matrix
```

Properties:

- the fake cores answer `version`/`--version`/`-version`, validate
  generated configuration documents (`check -c`), bind the declared
  local inbound, shut down on signal and support deterministic failure
  injection (`FAKECORE_FAIL_FAST`, `FAKECORE_CRASH_AFTER_START`,
  `FAKECORE_HANG`);
- `FREEIRAN_TEST_CORES` is authoritative in CI: a missing fixture is a
  HARD test failure, never a silent skip;
- test staging uses `contract.StageFakeCore` → `system.ExecutableName`,
  so the Windows job stages `v2ray.exe` (the extensionless staging that
  broke Windows discovery in v0.4.x is structurally impossible now);
- the Windows job builds the fixtures BEFORE running the test matrix
  and keeps the full matrix (no package skipped);
- real Xray/V2Ray/sing-box binaries remain in the dedicated
  `protocol-cores` job only (pinned versions + SHA-256 verified).

## v0.8.0 — Windows lifecycle battery in the standard matrix

The Windows job (`Windows tests and desktop build`) runs
`go test -count=1 ./...`, which now includes the process-supervision
lifecycle battery in `system/process_test.go` (15 tests):

- launch with no visible console (behavioural: the child never
  attaches to the parent console; `GetConsoleProcessList`)
- stdout AND stderr capture through separate writers (deterministic
  flush via the exited channel — no sleeps)
- natural exit with exit-code classification
- context cancellation (`cancelled` state, `context.Canceled` from Wait)
- forced termination within the hard-kill deadline
- repeated Stop (idempotent) and concurrent Stop (synchronizing)
- startup-failure cleanup (no process, `dependency_unavailable`)
- job-binding-failure fallback (injected: launch still succeeds,
  degradation visible, cleanup still deterministic)
- restricted-environment retry (injected `ERROR_ACCESS_DENIED`)
- grandchild cannot survive supervisor shutdown (observed through
  the job member list / process tree)

The fake-core contract tests and the process battery share the same
supervision path, so the `bind kill-on-close job` regression class
the v0.7.0 Windows run exhibited is covered on every platform.

## v0.9.0 — release packaging

The release pipeline's Windows job no longer packages a bare exe.
After the desktop build it:

1. creates `FreeIran-v<version>-windows-amd64/` with `FreeIran.exe`,
   README, LICENSE, VERSION, `portable.marker`, the eight standard
   directories (`config`, `data`, `logs`, `cache`, `cores`,
   `runtime`, `docs`, `deployment`), every `docs/*.md`, and
   `deployment/deployment.json` metadata (version, commit, platform,
   layout);
2. validates the package — the job fails when the executable, any
   required directory, README, VERSION or metadata is missing or
   inconsistent, or when the executable is suspiciously small;
3. creates `FreeIran-windows-amd64.zip` (single root, ≥ 12 entries).

The runtime smoke test (`go run ./cmd/freeiran --smoke-test`)
continues to gate the Windows build before packaging.

## v0.9.1 — two platforms, one artifact contract

The release workflow builds two independent platform pipelines and
publishes exactly two artifacts:

- `FreeIran-windows-amd64.zip` (windows-latest; the executable is
  built with `-H=windowsgui` and links the committed `.syso` resource:
  application icon, version info, DPI manifest. The post-build check
  asserts `ProductName`, the resource version and the `.rsrc` section
  so the icon can never silently disappear);
- `FreeIran-linux-amd64.zip` (ubuntu-latest; built with
  `-tags gtk3` against GTK3 + WebKit2GTK 4.1 after installing
  `libgtk-3-dev` / `libwebkit2gtk-4.1-dev`, with a full Go test run
  and a `--smoke-test` boot gate before packaging).

Both ZIPs contain the complete portable deployment layout (binary,
README, LICENSE, VERSION, `portable.marker`, `config/`, `data/`,
`logs/`, `cache/`, `cores/`, `runtime/`, `docs/`, `deployment/` with
`deployment.json` + icon). Checksum `.sha256` files are no longer
generated, uploaded or published — the publish job verifies the
artifact set is exactly the two ZIPs and fails on any `.sha256`.

The verify job also validates the icon asset chain before anything
builds: `assets/freeiran-icon.svg` / `.ico` / `.png`, the embedded
window icon copy (`internal/appicon`), and both committed Windows
resource objects must exist, and the embedded PNG must be byte-identical
to the canonical raster.

## v0.9.6 — the regression the Windows matrix caught

The v0.9.5 push added `internal/httpx` and turned the Windows CI job
red while the Linux `go` job stayed green: the finalization bug
(read-only open + `Sync`) is invisible on POSIX, where `fsync`
accepts `O_RDONLY` descriptors, and fatal on Windows, where
`FlushFileBuffers` requires `GENERIC_WRITE`. That asymmetry is
precisely why the Windows job runs the FULL `go test ./...` matrix —
it is the only place the defect was reproducible. The v0.9.6 fix
(`internal/httpx/finalize.go`) ships with `TestFinalizeCompletedPartDirect`,
a cross-platform regression test that fails on Windows against the
v0.9.5 code, so the matrix keeps proving this class of defect.

The new v0.9.6 engine packages (discovery, tester modes, ranking
scores, connection verification/racing, netcheck environment) run in
the existing `go` job (linux) and Windows matrix unchanged: they are
pure-Go, network-free under tests (httptest + fake getters + the
fake core's SOCKS-relay mode), so no workflow changes were required.

## v0.9.8.1 — provider fixtures and the latency representation fix

- `engine/provider` tests build their own fixtures: `TestMain`
  compiles `testdata/faketor` and `testdata/fakepsiphon` with the
  running toolchain into a temp directory before the package runs.
  No environment variable, no pre-build step and no workflow change
  were needed — the `go` and `windows` jobs pick the package up
  unchanged, and a missing fixture fails the package outright.
- The Windows job also covers the latency representation fix that
  motivated it: `engine/tester/TestTCPProbeReachable` (the failure of
  run 35287863799, "latency should be measured") now passes BY
  CONSTRUCTION — a successful probe always yields a positive measured
  Latency under the canonical semantics of `engine/tester/latency.go`
  (rules R1–R6), regardless of Windows monotonic-clock granularity.
  The fix is in the representation, not in relaxed test expectations;
  the full matrix keeps proving it on every push.

## v0.10.1 — Windows matrix survivability (run 36074448116)

The v0.9.15 release commit lost the Windows runner mid-matrix: the
"Run Go tests" step (`go test -count=1 ./...`) started 23:52:21Z and
never completed; the run was declared failed at 00:37:18Z with "The
hosted runner lost communication with the server". Every other job
(Linux go/frontend/native, protocol-cores) was green, and the full
Linux matrix including `-race` passes — the failure class is hosted
runner resource starvation under the full Windows matrix load, not a
deterministic code failure.

Two changes make the job survivable and diagnosable (coverage is
unchanged — every test still runs):

- `go test -count=1 -p 2 -timeout=20m ./...`
- `-p 2` bounds the number of concurrently RUNNING test binaries.
  The Windows matrix is process-heavy: the `system`, `provider`,
  `connection` and `app` suites supervise real cmd.exe / ping /
  fake-core children under kernel job objects, and the provider
  `TestMain` compiles two more binaries before its tests even start.
  On the 4-vCPU hosted runner, the default `-p 4` lets four
  process-spawning suites plus their children contend with the runner
  agent itself; `-p 2` keeps headroom so the agent can always
  heartbeat.
- `-timeout=20m` makes the per-binary bound explicit and visible: a
  recurrence now panics with a full goroutine dump — an actionable,
  diagnosable failure — instead of a silent 45-minute runner death
  that destroys every diagnostic (which is exactly what run
  36074448116 delivered).

The historical sections below document why the Windows job runs the
FULL matrix in the first place (the v0.9.6 finalization asymmetry):
that principle is unchanged — the matrix is bounded, not reduced.
