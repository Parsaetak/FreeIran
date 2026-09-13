# FreeIran v0.9.0 — Release Manifest

| Field | Value |
|-------|-------|
| **Project** | FreeIran |
| **Version** | 0.9.0 |
| **Base commit** | `6b6a776` (v0.8.0, main) |
| **Release commit** | see `git log -1` in the repository ZIP (tagged `v0.9.0`) |
| **Platform** | windows/amd64 (primary) |
| **Repository ZIP** | `FreeIran-0.9.0.zip` — complete replacement source tree |
| **Deployment ZIP** | `FreeIran-windows-amd64.zip` — complete runnable deployment (+ `.sha256`) |

---

## 1. Package contents

### 1.1 Repository ZIP (`FreeIran-0.9.0.zip`)

The full source tree at v0.9.0:

- `cmd/freeiran/` — Wails v3 desktop entrypoint (with rebuilt embedded UI)
- `engine/` — Go engine (store, pipeline, parser, cores, coremgr, connection,
  tester, testqueue, tunnel, **netcheck** (new), **socks5** (new), memory, native)
- `system/` — process launch (job objects, hidden console), paths (portable
  deployment), network, filesystem
- `internal/version`, `internal/logging`
- `frontend/` — TypeScript UI + bindings (8 tabs)
- `native/` — C++ acceleration layer
- `docs/` — architecture / storage / performance / CI / security / development
- `.github/workflows/` — ci.yml, security.yml, release.yml (complete deployment packaging)
- `README.md`, `LICENSE`, `VERSION`, `Updated-Files.md`, `Release-Manifest.md`, `worklog.md`

Excluded: `.git`, `node_modules`, build outputs, packaging outputs, local runtime data.

### 1.2 Deployment ZIP (`FreeIran-windows-amd64.zip`)

```text
FreeIran-v0.9.0-windows-amd64/
├── FreeIran.exe              (13.4 MB, windows/amd64, embedded UI, version-stamped)
├── README.md
├── LICENSE
├── VERSION                   (0.9.0)
├── portable.marker           (activates portable mode: state stays in this tree)
├── config/                   (settings + sources.json land here; starts empty, .gitkeep)
├── data/                     (chunked store; starts empty, .gitkeep)
├── logs/                     (freeiran.log; starts empty, .gitkeep)
├── cache/                    (starts empty, .gitkeep)
├── cores/                    (managed cores install into <core>/bin/)
├── runtime/                  (runtime working area, .gitkeep)
├── docs/                     (architecture, ci, development, performance, security, storage-format)
└── deployment/
    └── deployment.json       (version, commit, platform, layout manifest)
```

`FreeIran-windows-amd64.zip.sha256` contains the SHA-256 checksum
(`<hash>  <filename>`, sha256sum-compatible).

The archive has exactly one root directory and ≥ 12 entries; both facts are
validated by the release pipeline before publishing.

## 2. Architecture changes (summary)

1. **Core loading lifecycle** — `discover → verify → version → config
   validation → launch → readiness → health → usable` is now enforced by the
   Managed Core Manager and integrated with runtime discovery: the manager
   boots eagerly and its `bin/` directories are `CoreLocator` inputs;
   lifecycle actions refresh the registry, so an installed core is immediately
   connectable. States exposed: `not_installed / installing / installed /
   checking / ready / broken / disabled / update_available` (persisted) plus
   `starting / running / stopping / failed` (runtime, connection state
   machine). Every broken state carries a human-readable reason + technical
   details.
2. **Console-window elimination** — the last un-hidden launch sites (coremgr
   version probe / validation / smoke test) now use
   `CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | HideWindow`, with Windows
   regression tests asserting the flags. Application startup, core startup,
   core shutdown and child-process cleanup keep the v0.8 no-visible-console
   guarantees; no orphan shells/cores (job objects + tree-kill remain in
   `system/`).
3. **Network diagnostics** — `engine/netcheck` (new) with seven-state
   classification, multi-target DNS/TCP/HTTPS probes and a proxy-path probe
   through the connected core's local listener; exposed by the new
   `NetworkService` and the Network tab.
4. **Honest testing** — `engine/socks5` (new) + `CoreProbe.EndToEnd`: the
   measured ping is the real SOCKS5 CONNECT round-trip through the generated
   tunnel (204 fetch through the tunnel verifies forwarding); results persist
   to the store with backend/endpoint/duration/quality; bulk testing via
   `EnqueueByFilter`; latency aggregates in the queue stats.
5. **Error humanization** — `engine/errors.Humanize` + per-subsystem readable
   failure reasons; connection errors ship as "readable sentence + technical
   details"; a sanitized diagnostic report is copy/export-ready.
6. **Portable deployment** — `system.DefaultBaseDir` prefers a deployment
   layout next to the executable; the release workflow builds, validates and
   publishes the complete ZIP with checksum.

## 3. Testing performed (evidence, not claims)

| Check | Command | Result |
|-------|---------|--------|
| Go tests (Linux, fake cores) | `FREEIRAN_TEST_CORES=… go test -count=1 ./engine/... ./system/... ./internal/...` | all packages `ok` |
| Race detector | `go test -race -count=1` over coremgr / netcheck / socks5 / tester / app / testqueue | all `ok`, no races |
| Vet | `go vet ./engine/... ./system/... ./internal/...` | clean |
| gofmt | `gofmt -l ./engine ./system ./cmd ./internal` | empty |
| Windows vet/build | `GOOS=windows go vet ./engine/coremgr`, `GOOS=windows CGO_ENABLED=0 go build ./cmd/freeiran` | OK |
| Native C++ | `make -C native test` | "native: all tests passed" |
| Frontend | `npm ci`, `npm run typecheck`, `npm test` (31 cases), `npm run build`, `npm run build:embed` | all pass |
| Desktop runtime lifecycle | `engine/app` boot/shutdown battery (Linux) — the same path `--smoke-test` exercises on Windows | all `ok` |
| Core install pipeline | new E2E tests: fake release server → download → verify → unpack → probe → validate → activate → smoke → ready; corrupted-version rejection; Repair deadlock timeout guard | pass |
| Windows console regression | `engine/coremgr/exec_windows_test.go` (runs on Windows CI) | asserts hidden-console flags |
| Deployment package | validation script (mirrors release.yml): required files/dirs, VERSION + metadata consistency, exe size, single-root ZIP, ≥ 12 entries, sha256 round-trip | pass |

Real-binary core smokes (Xray / V2Ray / sing-box) run in CI's
`protocol-cores` job; the release pipeline's Windows job re-runs the full
`go test ./...` plus `--smoke-test` before packaging.

## 4. Deployment instructions

1. Download `FreeIran-windows-amd64.zip` (and optionally verify:
   `sha256sum -c FreeIran-windows-amd64.zip.sha256`).
2. Extract the ZIP — it creates `FreeIran-v0.9.0-windows-amd64/`.
3. Launch `FreeIran.exe` from inside that folder (no installer, no admin).
4. The first-launch dashboard walks through: install a core (one click, from
   the official upstream release, SHA-256 verified) → import configurations →
   test → connect.
5. All state (configurations, cores, logs, caches) stays inside the
   deployment folder — move or back up the folder as a unit. Removing
   `portable.marker` reverts to per-user `%APPDATA%\FreeIran` storage.

## 5. Known limitations

- **Windows runtime smoke test**: executed in the release pipeline
  (windows-latest). The local verification environment cannot execute
  Windows binaries (no Windows host); the Linux runtime lifecycle battery
  covers the same boot/store/log/shutdown path, and the Windows cross-build
  was compile-verified end to end.
- **Wails bindings**: the v3 generator requires a GUI toolchain host; the new
  services ship as hand-written `Call.ByName` bindings (same pattern the
  v0.8 hand-written services use, verified against the runtime's name
  resolution). Regenerating remains a documented follow-up.
- **Configuration editor**: capability-driven per-protocol editing forms are
  planned for v0.10; v0.9 ships import/test/connect breadth and the
  capability tables already used for backend selection.
- **Reproducible ZIP**: the deployment ZIP is deterministic in structure and
  content ordering, but `Compress-Archive` timestamps are not frozen; the
  checksum is published with each release for integrity verification.
- **`protocol-cores` job in release.yml**: real-binary core verification
  still runs in ci.yml on every push; adding it to the tag pipeline is
  queued behind Windows runner bandwidth (documented, not hidden).
- **First-connection latency semantics**: with a core installed, test pings
  measure the real tunnel round-trip and can be slower than the v0.8
  startup-only number — by design (honest measurement).
