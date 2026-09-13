# FreeIran v0.9.0 — Updated Files

Complete replacement for the FreeIran repository at
`6b6a776` (`v0.8.0`, `main`).

The ZIP contains the full updated source tree. Excluded: `.git`,
`frontend/node_modules`, `native/build` (build output),
`FreeIran-v*-windows-amd64` / `FreeIran-windows-amd64` (packaging
output), `.cores` / `testcores` (CI/local core installs and
fixtures), no secrets, no local runtime data.

Every path below is relative to the repository root. "Replaced"
means the shipped file fully replaces the v0.8.0 version.

---

## 1. Files added

| File | Purpose |
|------|---------|
| `engine/netcheck/netcheck.go` | Internet / Network Diagnostics (§3): concurrent, timeout-bounded probes across local links / DNS / TCP / HTTPS with a classified seven-state report (`no_internet`, `dns_failure`, `https_failure`, `high_latency`, `ok`, `proxy_only`, `core_no_internet`). Configurable targets, cancellation, honest measured latency. |
| `engine/netcheck/netcheck_test.go` | Classification table tests (all seven states), offline run, live local endpoints, cancellation, per-class independence. |
| `engine/socks5/socks5.go` | Minimal RFC 1928 no-auth CONNECT dialer (stdlib-only), shared by netcheck and the tester. |
| `engine/socks5/socks5_test.go` | Full handshake against a local fake SOCKS5 proxy + echo tunnel; non-SOCKS5 rejection; request-shape test. |
| `engine/coremgr/exec_windows.go` | `applyHiddenConsole` (`CREATE_NO_WINDOW \| CREATE_NEW_PROCESS_GROUP \| HideWindow`) + Windows-safe `gracefulStop` (TerminateProcess semantics for detached children). |
| `engine/coremgr/exec_other.go` | Unix equivalents (Setpgid + SIGINT→SIGKILL). |
| `engine/coremgr/exec_windows_test.go` | Windows regression test: every manager-spawned child must carry the hidden-console creation flags. |
| `engine/coremgr/progress.go` | Install progress events (`InstallProgress`, stage enum, `OnProgress` listener, byte-level download reporting). |
| `engine/coremgr/humanize.go` | `HumanizeInstallFailure` / `HumanizeHealthFailure` (readable failure reasons) and `ExtractVersionToken` (loose-semver extraction). |
| `engine/coremgr/install_test.go` | End-to-end install pipeline tests against a fake release server: happy path with progress assertions, corrupted-version rejection, legacy asset-naming regression, `Repair` deadlock regression (timeout-guarded), version-token table. |
| `engine/errors/humanize.go` | `Humanize(err, subject)` — translates port-in-use, permission-denied, missing executable, timeout, TLS, auth and cancellation errors into first-line readable sentences (§8). |
| `engine/tester/chain.go` | `ChainedProbe` — first accepting probe executes the test; its verdict is final (no dishonest downgrade to TCP-reachability). |
| `engine/app/networkservice.go` | `NetworkService` (CheckConnection / LastReport, proxy-path probing through the active session) + `DiagnosticReport` build/format for the sanitized copy/export diagnostics (§8). |
| `engine/app/core_integration_test.go` | Regression test for the v0.8 integration gap: managed `bin/` dirs are discovery inputs; simulated installs are discovered by the registry after `RefreshCores`. |
| `frontend/bindings/.../networkservice.js` | Hand-written bindings for `NetworkService.CheckConnection` / `LastReport`. |
| `frontend/src/pages/Cores.tsx` | New Cores tab: lifecycle cards (11-state badges), one-click Install → Verify → Start using, live install progress (download meter), recovery actions (Retry/Repair → Rollback → Reinstall), technical-details expander. |
| `frontend/src/pages/Network.tsx` | New Network tab: manual Check connection, classified verdict callout, latency/quality tiles, per-probe result lists. |
| `frontend/src/services/index.test.ts` | Vitest coverage for `parseHumanizedError` (readable/technical split). |
| `cmd/freeiran/frontend/dist/assets/index-DKS8AUZc.js` | Rebuilt embedded UI bundle (replaces the stale v0.8 bundles). |
| `cmd/freeiran/frontend/dist/assets/index-DPCdlAyt.css` | Rebuilt embedded stylesheet (8-tab shell, cores/network/onboarding/queue styles). |

## 2. Files replaced

| File | Change summary |
|------|----------------|
| `.github/workflows/release.yml` | Complete deployment package: staging directory with the standard tree + `deployment.json`, package validation gate (fails on missing exe/dirs/README/VERSION/metadata), single-root ZIP check, SHA-256 (§10/§11). |
| `cmd/freeiran/main.go` | `NetworkService` registered; `freeiran:coreprogress` event forwarding from `coremgr.OnProgress`. |
| `engine/app/app.go` | Core manager boots eagerly; managed bin dirs wired into the core locator; `RefreshCores`; tester chain (CoreProbe end-to-end → TCPProbe fallback); `SetCoreProgressListener`. |
| `engine/app/v6_services.go` | CoreService uses the shared manager + refreshes discovery after lifecycle actions; `LifecycleInfo` view; `Reinstall` / `UpdateAll`; testerAdapter persists results to the store and maps the enriched result fields; `EnqueueByFilter` bulk testing (`TestFilter` / `TestBatchResult`). |
| `engine/app/services.go` | `ConfigFilter` + `ListConfigsFiltered` (engine-side status/protocol/source/backend filtering + sorting, bounded match window); `storeGetConfig` helper. |
| `engine/app/connectionservice.go` | Connect/ConnectConfig errors rendered through `humanizeWithDetails` (readable sentence + technical details separator, §8). |
| `engine/config/config.go` | Config gains runtime test metadata (`test_backend`, `test_endpoint`, `test_duration_ms`; never fingerprinted). |
| `engine/coremgr/install.go` | Streamed download through the injected HTTPDownloader with byte progress; asset-name staging fix (unpack now always resolves — v0.8 could never unpack `asset.bin`); executable bit before probes (Unix installs); version sanity check vs release tag; `PreviousVersion` captured; failure reasons persisted; progress emissions. |
| `engine/coremgr/health.go` | Hidden console on all exec sites; Windows-safe graceful shutdown (v0.8's unsupported `os.Interrupt` marked every healthy install Broken). |
| `engine/coremgr/manager.go` | Manifest gains `FailureReason` / `FailureStage` / `LatestKnown`; `Reinstall`; `ExplainFailure`; Enable restores `installed` with cleared diagnostics. |
| `engine/coremgr/release.go` | Three-pass asset matcher (fixes Xray/V2Ray `windows-64.zip` selection on the most common platforms; arch-aware so windows/arm64 never receives the x86_64 asset). |
| `engine/coremgr/update_check.go` | `Repair` deadlock removed; `UpdateAll` added. |
| `engine/coremgr/http.go` | `HTTPDownloader` / `HTTPStream`; downloads honor the injected client. |
| `engine/core/testdata/fakecore/main.go` | Accepts the inbound port as JSON number *or* string (mirrors real Xray V4 parsing; the manager's minimal validation doc uses the string form). |
| `engine/tester/tester.go` | `Result` gains Backend / Protocol / Endpoint / PingMS / DurationMS / Quality; `QualityFor` bands (excellent ≤150 ms → very slow >2000 ms); `ApplyResult` persists metadata. |
| `engine/tester/core_probe.go` | `EndToEnd` mode: real SOCKS5 CONNECT ping through the instance's local listener + 204 fetch through the tunnel (no fabricated latency); results carry backend/endpoint/duration/quality. |
| `engine/tester/tcp_probe.go` | Reachability fallback fills the enriched result fields. |
| `engine/testqueue/queue.go` | Result/Stats carry protocol/endpoint/ping/duration/quality; latency aggregates (avg / fastest / slowest); `ClassifyByError`. |
| `engine/errors/` (new file above) | Humanized error layer. |
| `system/paths_windows.go` / `system/paths_unix.go` | Portable deployment base-dir: `portable.marker` or `config/` next to the executable wins over the per-user location (§10 extract-and-run). |
| `frontend/src/App.tsx` | Eight-tab navigation (Cores, Network added). |
| `frontend/src/types/ui.ts` | `Page` union extended. |
| `frontend/src/components/Icons.tsx` | `IconCores`, `IconGlobe`. |
| `frontend/src/pages/Configs.tsx` | Status filter, sort control, multi-select, bulk test actions (all/untested/failed/working/selected), live queue progress panel with cancel + ping aggregates, backend chip + quality ping badge per row. |
| `frontend/src/pages/Dashboard.tsx` | First-launch onboarding checklist (§12): install core → import configs → test → connect. |
| `frontend/src/pages/Settings.tsx` | Diagnostics & support card: sanitized report copy/export. |
| `frontend/src/services/index.ts` | `networkService` export; v0.9.0 view types (`NetCheckReport`, `CoreLifecycleView`, `CoreInstallProgress`, `DiagnosticReportView`, `QueueStatsView` aggregates); `parseHumanizedError`. |
| `frontend/src/styles/index.css` | Design-system header bumped; section 10 (v0.9.0): callouts, card grids, core cards, install meter, probe lists, onboarding, queue panel, selection styles. |
| `frontend/bindings/.../coreservice.js` | `LifecycleInfo`, `Reinstall`, `UpdateAll`. |
| `frontend/bindings/.../testqueueservice.js` | `EnqueueByFilter`. |
| `frontend/bindings/.../dataservice.js` | `ListConfigsFiltered`. |
| `frontend/bindings/.../diagnosticsservice.js` | **Fixed `Memory()` ByName path** (`engine.app.` → `engine/app/` — the old path could never resolve); `BuildDiagnosticReport`. |
| `frontend/bindings/.../config/models.js` | `test_backend` / `test_endpoint` / `test_duration_ms` members. |
| `cmd/freeiran/frontend/dist/index.html` | Rebuilt embed references. |
| `README.md` | v0.9.0 section (what's new), version metadata. |
| `docs/architecture.md`, `docs/ci.md`, `docs/performance.md` | v0.9.0 sections (core-manager integration + netcheck/socks5 + testing model + portable deployment; release packaging; performance notes). |
| `frontend/package.json`, `frontend/package-lock.json` | `0.9.0` (lock reconciled with the manifest). |
| `VERSION`, `internal/version/version.go` | `0.9.0`. |
| `worklog.md` | Broken opening fence repaired (markdown corruption), stale header metadata fixed, full v0.9.0 entry. |
| `.gitignore` | Packaging output entries. |

## 3. Files deleted

| File | Reason |
|------|--------|
| `cmd/freeiran/frontend/dist/assets/index-BnHoOyMr.js` | Stale v0.8 embed bundle (referenced by the old index.html only). |
| `cmd/freeiran/frontend/dist/assets/index-BoDTHTpt.js` | Stale v0.8 embed bundle (v0.8's Updated-Files.md already claimed this deleted; now actually gone). |
| `cmd/freeiran/frontend/dist/assets/index-COB-JYnw.css` | Stale v0.8 stylesheet. |

## 4. Verification summary

- `go test -count=1 ./engine/... ./system/... ./internal/...` — all green (including the new coremgr E2E install, netcheck, socks5 and app integration suites).
- `go test -race -count=1` over the new/modified packages — race-clean.
- `go vet`, `gofmt -l` — clean on Linux and for `GOOS=windows`.
- `make -C native test` — native C++ suite passes.
- `npm ci && npm run typecheck && npm test && npm run build && npm run build:embed` — 31 vitest cases pass, production bundle rebuilt and staged.
- Windows amd64 cross-build of the desktop binary with version/commit ldflags — OK.
- Deployment package assembled, validated and checksum-verified (see `Release-Manifest.md`).
