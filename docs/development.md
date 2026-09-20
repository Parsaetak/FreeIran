# FreeIran Development & Release Guide

## Toolchain

| Tool | Version | Required for |
|------|---------|--------------|
| Go | **1.26.8** (exact toolchain; go.mod floor is 1.25) | engine, desktop app, govulncheck |
| Node.js + npm | 22+ | frontend |
| C++17 compiler | gcc/clang/MSVC | optional native acceleration |
| make | any | optional native build helper |
| wails3 CLI | v3.0.0-beta.19 | regenerating bindings |
| govulncheck | **v1.8.0** (pinned) | security scanning |

Protocol-core runtimes (external executables, discovered at runtime —
never Go dependencies):

| Core | Verified release | Source | SHA-256 (linux-amd64 binary) |
|------|-----------------|--------|------------------------------|
| Xray | **26.3.27** | github.com/XTLS/Xray-core | `8255dd939c34cf966cc91517b6324dd3c8d0bcf49ffac8beca049a38c46845ed` |
| V2Ray (V2Fly) | **5.53.0** | github.com/v2fly/v2ray-core | `a7bc11ff3ee286bc15d8191440ebb10810ac801ced54bbd4ce79ce4d291f7f25` |
| sing-box | **1.14.0** | github.com/SagerNet/sing-box | `57b3da14e264b6e05e8f46aee027c02d7dd7f1594d19aa39e2f4d2b9459bbd04` |

Core versions are pinned deliberately — never "latest". The pins in
`engine/core/versions.go` record what the adapters were verified
against; CI's `protocol-cores` job installs exactly these releases
(checksum-verified) and runs the real-binary smoke suites. The
capability declarations were verified empirically against these
builds: `v2ray test`, `xray run -test` and `sing-box check` accept or
reject the generated documents, and full startup cycles (spawn →
listener-ready → shutdown) run against every supported protocol
combination. Notable verified divergences: V2Ray retains QUIC and
plain HTTP/2 transports that current Xray REMOVED (migrated to
XHTTP); REALITY and xtls-rprx-vision exist in Xray and sing-box but
not in V2Fly.

### Go toolchain policy (one intentional policy)

- `go.mod` declares `go 1.25.0` (the minimum language/toolchain level,
  matching the Wails v3 requirement) and `toolchain go1.26.8` (the
  enforced build toolchain).
- CI pins `go-version: "1.26.8"` via setup-go and sets
  `GOTOOLCHAIN=local`: if the runner's Go does not satisfy the
  toolchain directive, the job fails loudly instead of silently
  downloading a different version.
- Local development with the default `GOTOOLCHAIN=auto` automatically
  downloads go1.26.8 when needed.
- 1.26.8 was chosen deliberately: the 1.25 series is end-of-life and
  carries known stdlib vulnerabilities (GO-2026-6218 in net/url,
  GO-2026-6090 in crypto/tls); 1.26 is the oldest fully supported
  series and .8 is its latest security patch. Upgrading the toolchain
  is a reviewed change that updates go.mod, CI and this document
  together.
- govulncheck is pinned to v1.8.0 (compatible with go1.26). Never use
  `@latest` in CI: tool upgrades are deliberate, reviewed changes. The
  pinned version is also documented in docs/security.md.

The wails3 CLI is only needed when service signatures change:

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.19
wails3 generate bindings -clean -d frontend/bindings ./cmd/freeiran
```

v0.9.8.7 correction: the CLI itself does NOT need the GTK development
packages — it builds cleanly without CGO, so bindings can always be
regenerated anywhere:

```bash
# in a scratch module (keeps the project go.mod clean):
go mod init tmp && go get github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.19
CGO_ENABLED=0 go build -o wails3 github.com/wailsapp/wails/v3/cmd/wails3
./wails3 generate bindings -clean -d frontend/bindings ./cmd/freeiran
```

Reproducibility contract (v0.9.8.7): running the generation TWICE must
produce byte-identical output. Hand-maintained binding patches are
forbidden — the v0.9.8.7 regeneration removed the last hand-written
ByName shims (the old "generator needs GTK" note above was wrong; it
was verified with `CGO_ENABLED=0`). CI's contract test
(TestFrontendBindingsMatchGoServices) plus the frontend typecheck
guard the call surface.

## Protocol-core development

Running the adapter tests needs no protocol core installed: the
process lifecycle is exercised against the fake core helper
(`engine/core/testdata/fakecore`), compiled on the fly by the test
suite.

Real-binary smoke tests are opt-in through environment variables
(CI sets them after checksum-verified installs):

```bash
export FREEIRAN_TEST_V2RAY_BIN=/path/to/v2ray
export FREEIRAN_TEST_XRAY_BIN=/path/to/xray
export FREEIRAN_TEST_SINGBOX_BIN=/path/to/sing-box

go test -count=1 ./engine/core/...
```

Adding a backend (checklist):

1. Create `engine/core/<name>/` implementing `core.Core`
   (Name/Supports/Validate/BuildConfig/Start) with verified
   `Capabilities` declarations.
2. Run the shared contract suite
   (`engine/core/contract.Run`) plus deterministic config tests.
3. Add a real-binary smoke test guarded by an environment variable.
4. Register the backend in `engine/app` (`app.New`) with a priority.
5. Extend the selection/compatibility tests with the new capability
   matrix lines.

Discovery locations for core executables (in order): the
application-managed `<base>/cores` directory, then the system PATH.
A core found on PATH is never copied or modified — FreeIran only
executes binaries it discovered in controlled locations, never
anything extracted from downloaded configuration data.

Commit the regenerated bindings together with the backend change so the
frontend contract stays in sync (frontend/backend contract rule).
Binding method IDs are FNV-32a of the fully-qualified
`package.Service.Method` name, so hand-maintaining a single added
method (as done for `DiagnosticsService.StoreDiagnostics` in v0.3.0)
follows the generator's own scheme.

## Daily workflow

```bash
# Go
go test ./engine/... ./system/... ./internal/...
go test -race ./engine/... ./system/... ./internal/...
go vet ./engine/... ./system/... ./internal/...
gofmt -w ./engine ./system ./cmd ./internal

# Store lifecycle acceptance (Windows-critical):
go test -count=1 -run 'Lifecycle|Close|Delete|Compaction|Eviction' ./engine/store

# Frontend
cd frontend
npm ci
npm run typecheck && npm test && npm run build

# Native (optional)
make -C native test
```

## Desktop builds

Windows amd64 is the primary target (the Wails Windows backend needs no
cgo). `-H=windowsgui` is MANDATORY for any binary a user will run: it
selects the WINDOWS_GUI PE subsystem, so double-clicking the executable
never opens a console window. CI and the release workflow assert the
subsystem of the built binary (`TestWindowsGUISubsystem` reads the PE
header of the artifact):

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
  -trimpath -ldflags "-s -w -H=windowsgui \
  -X github.com/Parsaetak/FreeIran/internal/version.Version=$(cat VERSION | tr -d '[:space:]')" \
  -o FreeIran-windows-amd64.exe ./cmd/freeiran
```

Linux GUI builds require GTK4/WebKitGTK development packages
(`libgtk-4-dev`, `libwebkitgtk-6.0-dev` on Debian/Ubuntu). The engine
packages build everywhere without GUI dependencies, which is also why
CI vets and tests them separately from the desktop package.

## CI pipelines (.github/workflows)

All workflows use Node 24 based action majors (checkout@v7, setup-go@v7,
setup-node@v7, upload-artifact@v7, download-artifact@v8) — GitHub has
deprecated Node 20 based actions.

### ci.yml — every push/PR

| Job | Runner | Steps |
|-----|--------|-------|
| go | ubuntu | gofmt check, vet, build engine, tests, race tests, benchmark smoke, windows/amd64 desktop compile-validation |
| native | ubuntu | `make -C native test`, accelerated Go build + cross-language tests + bridge benchmarks |
| frontend | ubuntu | npm ci (cached), typecheck, vitest, production build, dist artifact |
| windows | windows | frontend build, **`go test -count=1 ./...`** (the full matrix including engine/store — no package is skipped, no failure tolerated), full desktop build, executable smoke check, artifact upload |

The Windows job runs the store lifecycle tests on purpose: on Windows
an open file handle blocks deletion, so `open → use → close → delete
temp dir` is only provable there. See docs/storage-format.md §5.

Caching: `actions/setup-go` (Go modules + build cache),
`actions/setup-node` with `cache: npm` keyed on
`frontend/package-lock.json`.

### release.yml — tags `v*.*.*`

1. **verify** — tag must equal `VERSION` and `frontend/package.json`
   version; Go tests, race tests and native tests must pass.
2. **build** — frontend production build, versioned desktop build via
   ldflags, `FreeIran-windows-amd64.zip` + SHA-256 checksum, executable
   smoke test.
3. **publish** — GitHub Release with the zip, checksum and generated
   notes. Prerelease is set automatically for pre-release tags
   (e.g. `v0.4.0-rc.1`).

A release never publishes when validation fails.

### security.yml — pushes, PRs, weekly

- `govulncheck` (pinned v1.8.0) over the pure-Go packages on Linux, and
  over the desktop package with `GOOS=windows` (see docs/security.md
  for why the split is the correct analysis, not a workaround).
- gitleaks secret scanning over full history.
- `go vet` on both package scopes plus a guard-rail scan for
  credential-looking literals and shell-injection patterns in non-test
  Go code. Every check is a real failure condition.

See docs/ci.md and docs/security.md for the full reasoning.

## Versioning

Single source of truth: the `VERSION` file at the repository root.
`internal/version` embeds the same default and is overridden at build
time with ldflags; CI injects the git commit. Rules:

- Changing the app version changes `VERSION`, `frontend/package.json`
  and `internal/version/version.go` together, in one commit.
- Release tags are `v<VERSION>` — the release workflow refuses to run
  when they disagree.
- The fetcher's User-Agent (`internal/version.UserAgent`) always reports
  the built version.

## Dependency policy

- Go dependencies are pinned in `go.mod`/`go.sum`; no floating `@latest`
  in build scripts or CI.
- The Wails version (`v3.0.0-beta.19`) is pinned in `go.mod` and in the
  CLI install command above.
- govulncheck is pinned to `v1.8.0` in the security workflow and
  documented here and in docs/security.md.
- Frontend dependencies are pinned via `package-lock.json`; CI installs
  with `npm ci` (never bare `npm install`).
- Dependency updates go through CI + govulncheck before merge.

## Fake-core test harness (v0.5.0)

Lifecycle tests never require real protocol cores:

```bash
# Option A (default): tests compile engine/core/testdata/fakecore
# with the local Go toolchain automatically.
go test ./engine/...

# Option B (CI parity): pre-build the fixture directory and point
# the harness at it. A missing fixture is a hard failure.
go build -o /tmp/testcores/fakecore ./engine/core/testdata/fakecore
FREEIRAN_TEST_CORES=/tmp/testcores go test ./engine/...
```

Always stage test cores through `contract.StageFakeCore(tb, dir, name)`
— it applies `system.ExecutableName` (`.exe` suffix on Windows), which
is what registry discovery expects. Headless application smoke test:

```bash
go run ./cmd/freeiran --smoke-test   # boot → state → services → shutdown
```

## v0.8.0 — process lifecycle and memory-controller tests

```sh
# Full supervision battery (15 tests; runs on every platform, the
# Windows-specific assertions activate on windows runners):
go test -count=1 -run 'TestProcess|TestSyncWriter' ./system -v

# Same battery under the race detector:
go test -race -count=1 -run 'TestProcess|TestSyncWriter' ./system

# Memory Booster 2.0 wiring + adaptive-shed integration:
go test -count=1 -run 'TestMemory' ./engine/app -v

# Dynamic worker-pool resize + queue memory estimate:
go test -count=1 -run 'TestSetConcurrency|TestMemoryEstimate|TestSetMaxQueueSize' \
  ./engine/testqueue -v

# Cache-target resize:
go test -count=1 -run 'TestSetMaxEntries' ./engine/cache -v
```

Windows code paths compile-verify from Linux with
`GOOS=windows go vet ./...` and `GOOS=windows go test -c ./system`.

## v0.9.6 — discovery, test modes, measured ranking, verification

New packages and surfaces added by the v0.9.6 upgrade (see
docs/architecture.md §v0.9.6 for the design):

```sh
# Multi-level discovery engine (levels, smart search, content refs,
# source health; fake HTTP getter drives everything deterministically):
go test -count=1 ./engine/discovery -v

# Ping / URL test modes and the five-mode runner:
go test -count=1 -run 'TestPing|TestURLTest|TestMode|TestApplyMode' ./engine/tester -v

# Separated metric scores, provenance rules and the nine sort modes
# (including the spec's fast-but-broken vs slower-but-working case):
go test -count=1 -run 'TestSpecExample|TestPingSort|TestURLSort|TestRecentlyVerified|TestMetricScores|TestPingProvenance' ./engine/ranking -v

# Tunnel verification + controlled racing (drives a REAL local SOCKS5
# relay and the fake core in FAKECORE_SOCKS_RELAY mode):
go test -count=1 -run 'TestVerifyTunnel|TestRace|TestClassifyVerifyFailure' ./engine/connection -v

# Environment intelligence signals:
go test -count=1 -run 'TestEnvironment' ./engine/netcheck -v

# The httpx Windows finalization regression battery (the v0.9.5 CI
# failure — the direct test fails on Windows with the old code):
go test -count=1 -run 'TestFinalize|TestRenameAtomically' ./internal/httpx -v
```

The fake test core gained `FAKECORE_SOCKS_RELAY=host:port`: the
inbound listener then speaks minimal RFC 1928 and relays every
CONNECT to the configured upstream, so verification, racing and
end-to-end tunnel tests run against the deterministic fixture
instead of real servers.

Frontend: the DiscoveryService bindings are hand-written
(`frontend/bindings/.../discoveryservice.js`, the Call.ByName
pattern documented in networkservice.js); their wire shapes live in
`frontend/src/types/discovery.ts`, and the start-flow store tests
mock only `Events.On` via a partial module mock.

## v0.9.8.1 — provider test fixtures

The provider suite (`engine/provider`) is hermetic: no test touches
the live Tor network or Psiphon servers. Everything runs against
deterministic stand-ins built by the test harness itself:

```text
engine/provider/testdata/faketor      (Go source, single binary)
engine/provider/testdata/fakepsiphon  (Go source, single binary)
        ↓ go build in TestMain with the SAME toolchain running the tests
        ↓ staged as tor / psiphon-tunnel-core-<platform> executables
        ↓ full lifecycle matrix (resolve → … → uninstall)
```

- `TestMain` (engine/provider/provider_test.go) compiles both fakes
  with `go build -o` into a temp directory before any test runs. A
  missing or broken fixture is a HARD failure that aborts the whole
  package (the same discipline as the fakecore harness) — never a
  silent skip, and never a fallback to whatever happens to be in
  `PATH`. No environment variable is needed; CI runs the package
  unchanged.
- **faketor** parses the generated torrc (`-f`), answers
  `--version` / `--verify-config`, emits REAL `Bootstrapped X% (Tag)`
  notice lines on stdout, and serves a minimal SOCKS5 endpoint on the
  configured `SocksPort` that actually relays CONNECT traffic —
  proving bootstrap observation, health probing and
  HTTP-through-provider end-to-end (via a real SOCKS relay) without
  any live Tor dependency.
- **fakepsiphon** parses the generated config JSON (`-config`), opens
  the configured local SOCKS and HTTP proxy ports, emits tunnel-up
  style output and relays traffic — proving negotiation observation,
  proxy readiness, health and HTTP-through-Psiphon.
- Download/install stages run against local `httptest` servers
  serving packed `tar.gz` archives with computed SHA-256 checksums,
  so digest verification (and refusal on mismatch / missing digest)
  is exercised for real.
- The connection provider-session tests (`engine/connection/provider_test.go`)
  drive `ConnectProvider` end-to-end against the same fixtures; the
  frontend provider store and mode routing are covered by vitest
  (`frontend/src/state/providerStore.test.ts`, 104 tests total).

## Wails toolchain contract (v0.9.8.6)

The desktop contract is a PINNED PAIR, enforced by CI:

* Go module: `github.com/wailsapp/wails/v3 v3.0.0-beta.19`
  (go.mod)
* Frontend runtime: `@wailsio/runtime` pinned to the EXACT same
  version in `frontend/package.json` (no `^` range) and re-resolved
  in `package-lock.json`.

At v0.9.8.5 the lockfile had drifted to `@wailsio/runtime@3.0.0-beta.20`
against Go beta.19 — the machine-generated bindings call
`$Call.ByID(<hash>)`, and those hashes are toolchain-version-coupled,
so a mismatched pair can break the runtime call surface silently. CI
now fails on any drift between the two.

### Bindings maintenance

The committed bindings (`frontend/bindings/...`) are of two
generations: machine-generated files (`appservice.js` etc., calling
`$Call.ByID`) and hand-written files (`coreservice.js`,
`discoveryservice.js`, `networkservice.js`, `testqueueservice.js`,
`tunnelservice.js`, calling `$Call.ByName` with a documented prefix
contract). Regenerating with the wails3 CLI is NOT part of the pinned
toolchain (no GUI toolchain on CI hosts); instead:

* `engine/app/bindings_contract_test.go` verifies every hand-written
  `Call.ByName` target exists as an exported method on its Go
  service, and structurally checks the generated files' services.
* The frontend CI job runs `npm run build:embed` and requires a clean
  `git diff --exit-code -- cmd/freeiran/frontend/dist`, so the
  committed embed output can never accumulate stale hashed bundles.

Do not delete the bindings: the build architecture requires them.
When a Go service method is renamed or removed, update the
corresponding binding file in the same change — the contract test
will catch misses.
