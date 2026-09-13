# FreeIran v0.9.1 — Updated Files

Complete replacement for the FreeIran repository at
`8e50d09` (`v0.9.0`, `main`).

The ZIP contains the full updated source tree. Excluded: `.git`,
`frontend/node_modules`, `native/build` (build output),
`FreeIran-v*-windows-amd64` / `FreeIran-v*-linux-amd64` /
`FreeIran-windows-amd64` (packaging output), `.cores` / `testcores`
(CI/local core installs and fixtures), no secrets, no local runtime
data.

Every path below is relative to the repository root. "Replaced"
means the shipped file fully replaces the v0.9.0 version.

---

## 1. Files added

| File | Purpose |
|------|---------|
| `assets/freeiran-icon.svg` | Canonical vector application icon (brand-green lightning on the dark rounded card; geometry mirrors the in-app brand mark). |
| `assets/freeiran-icon.ico` | Windows icon derivative (16/24/32/48/64/128/256 ladder) generated from the SVG geometry by `scripts/genicon.py`. |
| `assets/freeiran-icon.png` | 256px raster derivative (docs, deployment metadata). |
| `internal/appicon/appicon.go` | `//go:embed` package exposing the 256px icon to the runtime (Linux GTK window icon). |
| `internal/appicon/freeiran-icon.png` | Embedded icon copy (validated byte-identical to `assets/freeiran-icon.png` in CI). |
| `cmd/freeiran/rsrc_windows_amd64.syso` | Committed Windows resource object: application icon, version info (0.9.1), DPI-aware manifest. Linked automatically by every windows/amd64 build. |
| `cmd/freeiran/rsrc_windows_386.syso` | Same resource set for 386 builds. |
| `build/winres.json` | go-winres source config for the resource objects (regenerate: `go-winres make --in build/winres.json --out cmd/freeiran/rsrc --arch amd64,386`). |
| `scripts/genicon.py` | Icon generator: renders the canonical geometry into SVG + ICO + PNG derivatives (single source of truth). |
| `engine/app/developerservice.go` | `DiagnosticsService.DeveloperInfo` — read-only developer/build snapshot (identity, layout, portable mode, native acceleration, queue internals) and the verbose-diagnostics technical block. |
| `system/portable.go` | `system.PortableMode()` — cross-platform portable-deployment detection for diagnostics/developer info. |
| `cmd/freeiran/frontend/dist/assets/index-Dt5cQsia.js` | Rebuilt embedded UI bundle (replaces the v0.9.0 bundles). |
| `cmd/freeiran/frontend/dist/assets/index-DUh6p0Ta.css` | Rebuilt embedded stylesheet (v0.9.1 design-system fixes, queue panel, menus). |

## 2. Files replaced

| File | Change |
|------|--------|
| `frontend/src/styles/index.css` | **Root layout fixes.** Defined the previously-undefined v0.9.0 tokens (`--radius-md`, `--surface-1`, `--surface-2`, `--font-mono`, `--danger`, `--warning`) plus a control-height scale; rebuilt `.config-row-v2` as an explicit 8-column grid matching the 8 row children (fixes the Test-button overlap bug); flex-based `.page-flex` workspace layout with local-only scrolling; redesigned queue panel (head/progress/ping tiles/chips); added overflow-menu and tech-details styles; removed the duplicate `.page-header` / `.card-title` / `.badge.warn` / `.badge.neutral` / `.mono` overrides; synced responsive breakpoints (1240/1100/960). |
| `frontend/src/pages/Configs.tsx` | Workspace restructure: `page-flex` root, 8-cell header/row alignment (checkbox + action spacers), primary test actions visible (Test selected / all / untested) with secondary actions (Retest failed / working, Cancel all) in an overflow menu, new queue-progress panel with prominent avg/fastest/slowest ping tiles and per-state counters (queued, active, passed, failed, cancelled, timed out). |
| `frontend/src/components/common.tsx` | Added `Menu` (accessible overflow menu: outside-click + Escape close, real disabled states) and `TechDetails` (expandable raw-error disclosure). |
| `frontend/src/components/Icons.tsx` | Added `IconChevronDown`. |
| `frontend/src/pages/Settings.tsx` | Full IA rework: General / Connection / Testing / Appearance / Diagnostics & support / Developer / About sections with per-option label + explanation + control + default, runtime-log controls restored under Diagnostics, new Developer section (verbose diagnostics, force Go fallback, queue worker override, network-test timeout override, clear caches, open data/logs, live build/queue info), draft validation for every numeric override. |
| `frontend/src/pages/Connection.tsx` | Failure banners now render "what happened + why + what to do next": friendly sentence, targeted suggestion per failure kind (missing core, timeout, auth, DNS, unreachable, TUN elevation, port conflict) and an expandable technical-details block. |
| `frontend/src/pages/Cores.tsx`, `frontend/src/pages/Network.tsx` | Page headers standardized to the shared `page-title` / `page-subtitle` pattern. |
| `frontend/src/state/toastStore.ts` | `describeError` now strips the backend's technical-details section (friendly part only) and `describeErrorFull` exposes both halves for detail surfaces. |
| `frontend/src/services/index.ts` | Added `DeveloperInfoView` view type; `DiagnosticReportView.technical` field. |
| `frontend/bindings/.../engine/app/models.js` | `Settings` class extended with the four developer fields (`dev_verbose_diagnostics`, `dev_queue_workers`, `dev_net_timeout_seconds`, `dev_force_go_fallback`). |
| `frontend/bindings/.../engine/app/diagnosticsservice.js` | `DeveloperInfo()` binding (`$Call.ByName`, matching the established post-generated pattern). |
| `frontend/bindings/.../engine/app/storageservice.js` | `DataDir()` / `OpenDataDir()` bindings (same pattern). |
| `engine/app/loggingservice.go` | `Settings` gained the four wired developer fields; `applySettings` now applies them to the live engine (queue worker override, native Go-fallback pin) without depending on logger presence; validation for the new ranges. |
| `engine/app/memoryservice.go` | Booster adaptive concurrency now arbitrates with the developer override via `effectiveQueueConcurrency` (override wins, never silently undone). |
| `engine/app/networkservice.go` | `networkConfig` honors the network-test timeout override live; `DiagnosticReport` carries the optional redaction-safe `Technical` block; report formatter renders it. |
| `engine/app/services.go` | `StorageService.DataDir` / `OpenDataDir`. |
| `engine/native/native.go` | `SetForcedFallback` — runtime Go-fallback pin (same state as `FREEIRAN_NATIVE=off`), atomic and race-safe. |
| `cmd/freeiran/main.go` | Window options: embedded application icon for Linux (`LinuxWindow.Icon`); documents the Windows resource path. |
| `.github/workflows/release.yml` | Two independent platform pipelines (windows-latest, ubuntu-latest) publishing exactly `FreeIran-windows-amd64.zip` + `FreeIran-linux-amd64.zip`; icon-asset validation step; Windows exe verified for icon/version resources and built with `-H=windowsgui`; Linux built with `-tags gtk3` after installing GTK/WebKit dev packages, with Go tests + smoke test before packaging; complete portable deployment dirs for both platforms; **all `.sha256` generation/upload/publish steps removed**; publish job fails if any checksum file appears. |
| `.gitignore` | Added `FreeIran-v*-linux-amd64/`. |
| `README.md` | v0.9.1 section (what's new), version metadata. |
| `docs/ci.md` | v0.9.1 release-pipeline section (two platforms, icon validation, no checksums). |
| `VERSION`, `internal/version/version.go` | `0.9.1`. |
| `frontend/package.json`, `frontend/package-lock.json` | `0.9.1` (lock reconciled). |
| `cmd/freeiran/frontend/dist/index.html` + `assets/` | Rebuilt embed of the v0.9.1 UI (old hashed bundles removed). |
| `Release-Manifest.md` | Rewritten for v0.9.1. |
| `REPLACEMENT_MANIFEST.md` | Updated for v0.9.1. |
| `worklog.md` | Full v0.9.1 entry. |

## 3. Unchanged on purpose

The backend engine was only touched where the release required it
(developer settings wiring, icon/build integration, release
packaging). Core manager, Xray/V2Ray/sing-box adapters, test queue,
ping measurement, source manager, network diagnostics, System Proxy,
TUN, memory booster, native acceleration, structured logging,
portable deployment mode and process supervision are untouched
apart from the additive changes listed above.
