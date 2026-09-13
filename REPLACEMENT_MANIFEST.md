# FreeIran Replacement Manifest — v0.9.1

## Package

| Field | Value |
|-------|-------|
| Version | 0.9.1 |
| Previous version | 0.9.0 |
| Base reference | `8e50d09` (v0.9.0, main) |
| Package | `FreeIran-0.9.1.zip` — complete source repository replacement |
| Verified by | Full build + test matrix re-run from the working tree (see "Verification performed") |
| Excluded from package | `.git`, `frontend/node_modules`, `frontend/dist` (build output), `native/build`, `.cores` (local real-core installs), `FreeIran-v*-windows-amd64` / `FreeIran-v*-linux-amd64` (packaging outputs), no secrets, no local runtime data |

## Objective

A focused production-polish release, in the specified priority order:

1. **Testing/Ping UI overlap** — root-caused and fixed: the
   configuration-row grid declared 7 columns while every row rendered
   8 children, so the per-row Test button wrapped onto an implicit
   second grid row and overlapped the row below. Rebuilt as an
   explicit 8-column grid with an aligned sticky header, plus a
   flex-based workspace layout (local scrolling only) and a redesigned
   queue-progress panel with prominent ping tiles.
2. **Global UI/UX consistency** — defined the six v0.9.0 design
   tokens that were referenced but never declared; removed the
   duplicate `.page-header` / `.card-title` / badge / `.mono`
   overrides; standardized every page header; consistent control
   heights; accessible overflow menu and expandable technical-details
   components.
3. **Application icon** — canonical SVG + ICO/PNG derivatives
   (`assets/`, `scripts/genicon.py`), committed Windows resource
   objects (icon + version + DPI manifest) linked into every
   windows/amd64 build, embedded runtime icon for the Linux window,
   CI validation so the icon cannot silently disappear. Windows
   release builds now also link `-H=windowsgui` (no console).
4. **Windows amd64 + Linux amd64 release workflow** — two independent
   pipelines, complete portable deployment packages for both, linux
   built with `-tags gtk3` (WebKit2GTK 4.1); **`.sha256` checksum
   artifacts removed everywhere**; publish job fails on any checksum
   file.
5. **Developer/Advanced settings** — four genuinely wired engine
   controls (verbose diagnostics, queue worker override, network-test
   timeout override, force Go fallback) plus developer/build info,
   clear-caches and open data/logs actions in a reorganized Settings
   information architecture.
6. **User information** — connection failures now explain what
   happened, why, and what to do next, with raw details behind an
   expandable disclosure.
7. **Version + documentation** — 0.9.1 across VERSION / version.go /
   package.json / README / docs / manifests / worklog.

## 1. Files added

See `Updated-Files.md` §1 (icon assets, resource objects, generator,
developer service, portable-mode helper, rebuilt embed).

## 2. Files replaced

See `Updated-Files.md` §2.

## 3. Not touched (regression guard)

Core manager, Xray/V2Ray/sing-box adapters, test queue, ping
measurement, source manager, network diagnostics, System Proxy, TUN,
memory booster, native acceleration, structured logging, portable
deployment mode and process supervision are unchanged apart from the
additive developer-settings wiring listed above (all behind
`DevQueueWorkers > 0` / explicit toggles; defaults preserve the
v0.9.0 behaviour exactly).

## 4. Verification performed

- `gofmt -l ./engine ./system ./cmd ./internal` — clean
- `go vet ./engine/... ./system/... ./internal/...` — clean
- `go test -count=1 ./engine/... ./system/... ./internal/...`
  (deterministic fake-core matrix) — pass
- `go test -race -count=1 ./engine/... ./system/... ./internal/...` —
  pass
- `make -C native test` — pass
- Frontend: `npm ci`, `npm run typecheck`, `npm test` (31/31),
  `npm run build`, `npm run build:embed` — pass
- `windows/amd64` production build — 12.9 MB exe with `.rsrc`
  (icon, `ProductName=FreeIran`, version `0.9.1`), `-H=windowsgui`
- `linux/amd64` — engine/system/internal build + test natively; the
  desktop link and `--smoke-test` gate run in CI where the GTK3 /
  WebKit2GTK headers are installed by the workflow
- Package generation + validation logic — exercised in the workflow
  for both platforms (single-root ZIP, ≥ 12 entries, deployment.json
  version/platform match, no checksum files)

**Platform runtime note:** Windows/Linux GUI runtime behaviour is
verified in the GitHub Actions runners by the release workflow (smoke
test, package validation, resource checks). No GUI host was available
to this verification environment; everything compilable and testable
without GTK was verified locally.

## 5. Known limitations (honest)

- The linux/amd64 desktop link requires GTK3 + WebKit2GTK 4.1
  development headers; the release workflow installs them before
  building. A plain `CGO_ENABLED=0 go build ./cmd/freeiran` is NOT
  possible on Linux with Wails v3 beta.19 (upstream constraint), which
  is why the Linux job is a separate CI pipeline with the deps step.
- The generated Wails bindings were hand-extended for the new methods
  (`$Call.ByName`, the project's established post-generator pattern);
  rerunning the wails3 generator on a GUI host will fold them into the
  generated section without behaviour change.
