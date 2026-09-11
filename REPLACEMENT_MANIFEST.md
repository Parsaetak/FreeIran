# FreeIran Replacement Manifest — v0.4.1

## Package

| Field | Value |
|-------|-------|
| Version | 0.4.1 |
| Base reference | commit `e661a28523b0265b16a07e3a0de807755b9cfd79` (v0.4.0) |
| Package | `FreeIran-v0.4.1.zip` — complete source repository replacement |
| Verified by | Clean extraction into a fresh directory; full build + test matrix re-run from the extracted tree, including real protocol-core smoke suites (see "Verification") |
| Excluded from package | `.git`, `frontend/node_modules`, `frontend/dist` (build output), `native/build`, `.cores` (CI core installs), no secrets, no local runtime data |

## Objective

Repair the failing CI pipeline at v0.4.0 (run 34546093190, head
`e661a28`) WITHOUT weakening any gate, and re-verify the entire
application functionality matrix — the v0.4.0 repository already
contained the full multi-core runtime (Xray, V2Ray, sing-box); the
repair makes its verification pipeline actually execute and pass.

## Exact Actions failures discovered

The failing CI run 34546093190 completed in **0 seconds with ZERO
jobs started** (created 2026-09-11T00:20:42Z, updated the same
second; the jobs API reports `total_count: 0`). That signature is not
a test failure — it is GitHub Actions rejecting the workflow file
before scheduling anything.

### Root cause 1 (workflow failure): invalid YAML — duplicate `env:` key

The `windows` job's "Build desktop application" step in
`.github/workflows/ci.yml` declared TWO `env:` mappings (one before
`run:`, one after). Duplicate mapping keys make the whole workflow
file invalid, so every push since v0.3.0 (commits `e6defe2` and
`e661a28`, runs 34534589667 and 34546093190) failed instantly with
no jobs — the entire test matrix (go, native, frontend,
protocol-cores, windows) was silently NEVER executed for v0.3.0 and
v0.4.0.

**Fix**: merged both variables into a single `env:` block
(`CGO_ENABLED: "0"`; the `VERSION: ${{ github.sha }}` entry was
dead — the script already reads `$env:GITHUB_SHA` — and was dropped).
All three workflows now pass strict duplicate-key YAML validation.

### Root cause 2 (latent, masked by #1): three wrong pinned core checksums

The `protocol-cores` job pins SHA-256 values for the three real core
downloads. All three v0.4.0 pins were WRONG (the official release
archives carry different hashes):

| Asset | v0.4.0 pin (wrong) | Actual official SHA-256 (v0.4.1) |
|-------|-------------------|--------------------------------|
| v2fly/v2ray-core v5.53.0 linux-64 | `a7bc11ff…` | `6bbb8aee65a57d0b12599b4b7c842b3ad0daca4436e661d94015c447cb31b4fa` |
| XTLS/Xray-core v26.3.27 linux-64 | `8255dd93…` | `23cd9af937744d97776ee35ecad4972cf4b2109d1e0fe6be9930467608f7c8ae` |
| SagerNet/sing-box v1.14.0 linux-amd64 | `57b3da14…` | `2375de6999f4f56ab46b4fc5ddf26a6aba1d3e61a0f4e7ddec2f4690457d5f63` |

Each binary was downloaded, hash-verified against the corrected pin,
and confirmed to report the pinned version (`v2ray version` → 5.53.0,
`xray version` → 26.3.27, `sing-box version` → 1.14.0) before the
pins were updated. With the old pins, the job would have failed at
the first `sha256sum -c` on every run.

### Historical note

The last CI run with a VALID workflow file (commit `a536191`, run
34432214132) failed in the Windows job's "Run Go tests" step. That
failure predates the v0.3.0/v0.4.0 store/process/connection rework
(which included the Windows file-lifecycle hardening and the
StdoutPipe → direct-writer process refactor). The current matrix
passes on Linux (3 consecutive full runs, plus `-race`), compiles the
entire test suite for windows/amd64, and the Windows-specific hazards
(`.exe` discovery via `executableName`, fake-core `.exe` staging,
process-group kill, open-handle-before-delete discipline) were audited
in code. Full runtime confirmation happens on the windows-latest
runner once the repaired workflow executes.

## Fixes applied

1. `ci.yml`: single `env:` per step (root cause 1). The step now also
   reads the version stamp from the `VERSION` file (single source of
   truth) instead of hard-coding `0.4.0-ci` — future bumps no longer
   require workflow edits.
2. `ci.yml`: the three corrected pinned checksums (root cause 2).
3. Version advance `0.4.0` → `0.4.1` (patch: verification-pipeline
   repair, no application-behaviour change) in `VERSION`,
   `internal/version/version.go`, `frontend/package.json`, `README.md`.
4. `docs/ci.md`: documents the duplicate-key failure mode and the
   checksum-source policy; fixes the runner description ("Node 22",
   not "Node 24").
5. No test was skipped, weakened, or removed. No package was excluded
   from any job. No assertion was relaxed.

## Application functionality audit (v0.4.1 re-verification)

The complete chain was re-verified on the extracted clean tree:

```text
launch → app.New (store open, registry, connection manager) →
UI services bound (6 Wails services, 30 methods — every binding ID
FNV-verified against the Go source) → staged background init →
store a config → Connect → capability resolution → config generation →
core start → WaitReady (listener poll) → connected state → health
axes (process + listener) → Disconnect → cleanup → Shutdown ordering
(scheduler stop → session teardown → store close)
```

- **Connection state machine** (`engine/connection`): all eight states
  exercised via the service-level E2E suite; bounded fallback, crash
  detection in the health monitor, secret redaction in attempt
  records.
- **Protocol cores**: real-binary smoke suites pass — V2Ray 7/7,
  Xray 9/9, sing-box 10/10 protocol combinations (config validation
  by the core's own validator + full startup/listener/shutdown
  cycles, synthetic credentials only).
- **No dead UI**: grep sweep for TODO/FIXME/not-implemented/dummy
  patterns found only HTML input placeholders and a reserved protocol
  header field.
- **Frontend data flow**: paged queries (`ConfigPage`), virtualized
  config list (`@tanstack/react-virtual`), event-driven state (no
  polling of full datasets).

## Performance

Benchmark smoke results (2 vCPU, go1.26.8, `-benchtime=1x`):

| Benchmark | Result |
|-----------|--------|
| PipelineRun (engine/pipeline) | ~45 ms/op |
| BackendSelection | ~2.3 ms/op (first call, cold) |
| BackendSelectionCompatible | ~38 µs/op |
| GenCache | ~11 µs/op |
| v2ray BuildConfig (vless/tcp) | ~278 µs/op |
| v2ray BuildConfigCached | ~64 µs/op |
| RedactLogText | ~31 µs/op |
| Native bridge (CGO) | pass, sub-ms |

No performance regressions were introduced; the generation cache and
per-attempt latency recording behave as documented in
`docs/performance.md`.

## Tests executed (from the clean extracted tree)

- `gofmt -l ./engine ./system ./cmd ./internal` — clean
- `go vet ./engine/... ./system/... ./internal/...` — pass
- `go vet` `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 ./...` — pass
- `go build ./engine/... ./system/... ./internal/...` — pass
- `go test -count=1 ./engine/... ./system/... ./internal/...` — 19 packages pass
- `go test -race -count=1 ./engine/... ./system/... ./internal/...` — pass
- `go test -count=3` (flakiness sweep) — zero failures across 3 runs
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -run '^$' ./...` — full suite compiles for Windows
- Windows desktop build (CGO_ENABLED=0, trimpath, ldflags version) — 17.8 MB executable
- `make -C native test` — C++ tests pass
- `go build -tags native_accel ./engine/native` + CGO bridge tests + bridge benchmark — pass
- Frontend: `npm ci`, `typecheck`, `vitest` (12/12), `vite build` — pass
- Protocol cores (real pinned binaries, checksum-verified): V2Ray 7/7, Xray 9/9, sing-box 10/10 smoke combinations — pass
- Security equivalents: `govulncheck` (linux engine + windows-target cmd) — no vulnerabilities; credential/shell-injection pattern scans — clean
- Workflow strict-YAML validation (duplicate-key detection) — all three workflows pass

## Workflow changes

- `ci.yml` — the two root-cause fixes above (duplicate `env:` merge,
  dynamic version stamp, corrected checksums). Job/step structure
  otherwise unchanged; nothing is skipped.
- `security.yml`, `release.yml` — unchanged (validated, no defects
  found; the Security workflow was already green at v0.4.0).

## Known limitations

- The Windows job's runtime test execution happens on
  `windows-latest` only — no local Windows runner exists in this
  environment; Windows coverage locally is compile-level plus the
  code-audit of platform-specific paths (verified: `.exe` discovery,
  fake-core staging, process lifecycle, file-handle discipline).
- `go test ./...` cannot run the `cmd/freeiran` package on a Linux
  desktop without GTK4/WebKitGTK dev packages (Wails v3 linux webview
  is CGO/GTK). This is an environment constraint, not a repository
  defect; CI compiles and tests that package on windows-latest where
  the webview is the pure-Go WebView2 path.
- Protocol-core smoke tests exercise config acceptance and local
  lifecycle only; no real tunnel traffic (by design — no dependency
  on external proxy infrastructure).
