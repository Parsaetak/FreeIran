# FreeIran v0.9.1 — Release Manifest

| Field | Value |
|-------|-------|
| **Project** | FreeIran |
| **Version** | 0.9.1 |
| **Base commit** | `8e50d09` (v0.9.0, main) |
| **Release commit** | see `git log -1` in the repository ZIP (tagged `v0.9.1`) |
| **Platforms** | windows/amd64 + linux/amd64 |
| **Repository ZIP** | `FreeIran-0.9.1.zip` — complete replacement source tree |
| **Deployment ZIPs** | `FreeIran-windows-amd64.zip`, `FreeIran-linux-amd64.zip` (produced and published by GitHub Actions) |

---

## 1. Package contents

### 1.1 Repository ZIP (`FreeIran-0.9.1.zip`)

The full source tree at v0.9.1:

- `cmd/freeiran/` — Wails v3 desktop entrypoint (rebuilt embedded UI,
  Windows resource objects, embedded application icon)
- `engine/` — Go engine (store, pipeline, parser, cores, coremgr,
  connection, tester, testqueue, tunnel, netcheck, socks5, memory,
  native + **developer info service** (new))
- `system/` — process launch (job objects, hidden console), paths
  (portable deployment, **PortableMode** (new)), network, filesystem
- `internal/version`, `internal/logging`, **`internal/appicon`** (new)
- `assets/` — **application icon sources/derivatives** (new: SVG
  canonical, ICO, PNG)
- `build/winres.json` — **Windows resource source config** (new)
- `scripts/genicon.py` — **icon generator** (new)
- `frontend/` — TypeScript UI + bindings (8 tabs, v0.9.1 workspace)
- `native/` — C++ acceleration layer
- `docs/` — architecture / storage / performance / CI / security /
  development
- `.github/workflows/` — ci.yml, security.yml, release.yml (two
  platform pipelines, no checksum artifacts)
- `README.md`, `LICENSE`, `VERSION`, `Updated-Files.md`,
  `Release-Manifest.md`, `worklog.md`

Excluded: `.git`, `node_modules`, build outputs, packaging outputs,
local runtime data.

### 1.2 Deployment ZIPs (produced by the release workflow)

```text
FreeIran-v0.9.1-windows-amd64/
├── FreeIran.exe              (windows/amd64, embedded UI, icon + version
│                              resources, -H=windowsgui: no console window)
├── README.md
├── LICENSE
├── VERSION                   (0.9.1)
├── portable.marker
├── config/  data/  logs/  cache/  cores/  runtime/   (.gitkeep placeholders)
├── docs/                     (all docs/*.md)
└── deployment/
    ├── deployment.json       (version, commit, platform, layout, created_at)
    └── freeiran-icon.svg

FreeIran-v0.9.1-linux-amd64/
├── FreeIran                  (linux/amd64, gtk3 frontend, embedded window icon)
├── freeiran.sh               (launcher: runs from the deployment root)
├── README.md
├── LICENSE
├── VERSION                   (0.9.1)
├── portable.marker
├── config/  data/  logs/  cache/  cores/  runtime/   (.gitkeep placeholders)
├── docs/                     (all docs/*.md)
└── deployment/
    ├── deployment.json       (version, commit, platform, layout, created_at)
    └── freeiran-icon.svg
```

The GitHub release contains **only** the two platform ZIPs — no
`.sha256` checksum files are generated, uploaded or published.

---

## 2. Release pipeline (summary)

1. **verify** (ubuntu): version-consistency checks across `VERSION` /
   `internal/version` / `frontend/package.json`, icon asset chain
   validation (SVG/ICO/PNG/syso, embedded copy byte-identical, ICO
   size ladder), gofmt, `go vet`, engine builds, full Go test matrix
   with deterministic fake cores, race tests, native C++ tests,
   frontend typecheck + unit tests + production build, windows/amd64
   compile validation.
2. **build-windows** (windows): frontend build + embed staging, full
   Go tests, `--smoke-test` boot gate, production build
   (`-trimpath -ldflags "-s -w -H=windowsgui …"`), executable
   verification (size, `ProductName`, resource version, `.rsrc`
   section), complete deployment directory, package validation, ZIP
   (single root, ≥ 12 entries), artifact upload.
3. **build-linux** (ubuntu): GTK3 + WebKit2GTK 4.1 dev packages,
   frontend build + embed staging, Go tests, `--smoke-test`
   (`-tags gtk3`), production build (`-tags gtk3`), executable
   verification, complete portable deployment directory (with
   `freeiran.sh` launcher), package validation, ZIP (single root),
   artifact upload.
4. **publish**: downloads both artifacts, asserts the set is exactly
   the two ZIPs (fails on any `.sha256`), publishes the GitHub release
   with generated notes.

---

## 3. Verification performed for this release

- `gofmt -l ./engine ./system ./cmd ./internal` — clean
- `go vet ./engine/... ./system/... ./internal/...` — clean
- `go test -count=1 ./engine/... ./system/... ./internal/...`
  (fake-core matrix) — all packages pass
- `go test -race -count=1 ./engine/... ./system/... ./internal/...` —
  all packages pass
- `make -C native test` — all native tests pass
- `npm ci && npm run typecheck && npm test && npm run build &&
  npm run build:embed` — 31/31 frontend tests pass, production bundle
  rebuilt into `cmd/freeiran/frontend/dist`
- `--smoke-test` boot path: covered by the engine/app lifecycle test
  suite on linux/amd64 (the desktop binary itself requires GTK headers
  for linking; the full `--smoke-test` + packaging gates run in the
  GitHub Actions runners, where the workflow executes them for both
  platforms)
- `windows/amd64` production build: verified locally — 12.9 MB exe,
  `.rsrc` section present, `ProductName`/`0.9.1` version resource
  embedded, `-H=windowsgui` applied
- linux/amd64: engine + system + internal packages build and test
  natively on linux/amd64; the desktop link (`-tags gtk3`) requires
  the GTK/WebKit headers installed by the release workflow

---

## 4. Upgrade notes

- v0.9.1 is a drop-in replacement for the v0.9.0 source tree; the
  settings file gains four optional developer keys and older files
  load unchanged.
- Deployment ZIPs keep the same portable layout as v0.9.0; extracting
  a v0.9.1 deployment over a v0.9.0 one is supported. Linux is a new
  sibling layout (`FreeIran` + `freeiran.sh` instead of
  `FreeIran.exe`).
- GitHub release artifacts change: checksum files are intentionally
  gone; consumers verifying integrity should pin to the release tag.
