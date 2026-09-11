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
