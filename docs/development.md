# FreeIran Development & Release Guide

## Toolchain

| Tool | Version | Required for |
|------|---------|--------------|
| Go | 1.25+ (Wails v3 requirement) | engine, desktop app |
| Node.js + npm | 22+ | frontend |
| C++17 compiler | gcc/clang/MSVC | optional native acceleration |
| make | any | optional native build helper |
| wails3 CLI | v3.0.0-beta.19 | regenerating bindings |

The wails3 CLI is only needed when service signatures change:

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.19
wails3 generate bindings -clean -d frontend/bindings ./cmd/freeiran
```

Commit the regenerated bindings together with the backend change so the
frontend contract stays in sync (frontend/backend contract rule).

## Daily workflow

```bash
# Go
go test ./engine/... ./system/... ./internal/...
go test -race ./engine/...
go vet ./engine/... ./system/... ./internal/...
gofmt -w ./engine ./system ./cmd ./internal

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
packages build everywhere without GUI dependencies.

## CI pipelines (.github/workflows)

### ci.yml — every push/PR

| Job | Runner | Steps |
|-----|--------|-------|
| go | ubuntu | gofmt check, vet, build engine, tests, race tests, benchmark smoke, windows/amd64 desktop compile-validation |
| native | ubuntu | `make -C native test`, accelerated Go build + cross-language tests |
| frontend | ubuntu | npm ci (cached), typecheck, vitest, production build, dist artifact |
| windows | windows | frontend build, Go tests, full desktop build, executable smoke check, artifact upload |

Caching: `actions/setup-go` (Go modules + build cache),
`actions/setup-node` with `cache: npm` keyed on
`frontend/package-lock.json`.

### release.yml — tags `v*.*.*`

1. **verify** — tag must equal `VERSION` and `frontend/package.json`
   version; Go + native tests must pass.
2. **build** — frontend production build, versioned desktop build via
   ldflags, `FreeIran-windows-amd64.zip` + SHA-256 checksum, executable
   smoke test.
3. **publish** — GitHub Release with the zip, checksum and generated
   notes. Prerelease is set automatically for pre-release tags
   (e.g. `v0.3.0-rc.1`).

A release never publishes when validation fails.

### security.yml — pushes, PRs, weekly

- `govulncheck` over all Go packages.
- gitleaks secret scanning over full history.
- `go vet` plus a guard-rail grep for credential-looking literals in
  non-test Go code.

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
  in build scripts.
- The Wails version (`v3.0.0-beta.19`) is pinned in `go.mod` and in the
  CLI install command above.
- Frontend dependencies are pinned via `package-lock.json`; CI installs
  with `npm ci` (never bare `npm install`).
- Dependency updates go through CI + govulncheck before merge.
