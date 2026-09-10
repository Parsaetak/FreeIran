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
cgo):

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
  -trimpath -ldflags "-s -w \
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
