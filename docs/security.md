# FreeIran Security Architecture

Local-first also means security-first: no telemetry, no remote
backends, no secrets in the repository. The security workflow
(.github/workflows/security.yml) enforces this continuously.

## The govulncheck platform-targeting decision

### The v0.2 failure

The original workflow ran:

```bash
govulncheck ./engine/... ./system/... ./cmd/... ./internal/...
```

`./cmd/freeiran` imports Wails v3, whose **Linux** webview bindings are
CGO packages requiring `pkg-config` metadata for GTK4 / WebKitGTK-6.0 /
libsoup-3.0. On a stock CI runner those native development packages are
not installed, so package LOADING (not even scanning) fails:

```text
Package gtk4 was not found in the pkg-config search path.
Package 'webkitgtk-6.0', required by 'virtual:world', not found.
could not import C (no metadata for C)
```

The fix is NOT `|| true`, and it is not installing a GTK toolchain to
scan a Windows application as a Linux GUI.

### The v0.3.0 analysis

`cmd/freeiran` ships exclusively as a **Windows amd64** desktop binary
(the release pipeline builds nothing else). Analysing it as a Linux GUI
program scans a dependency surface the project never builds, while the
surface it does build — the Windows webview (pure Go WebView2 loader,
no CGO pkg-config) — is what actually needs coverage.

The workflow therefore runs two targeted scans, both of which fail the
job on any detected vulnerability:

| Scan | Packages | Platform | Why |
|------|----------|----------|-----|
| 1 | `./engine/... ./system/... ./internal/...` | native Linux | pure-Go packages, no CGO, analyse exactly as built |
| 2 | `./cmd/...` | `GOOS=windows GOARCH=amd64` | the desktop app analysed for its real deployment target |

Both scopes were verified to load and analyse cleanly, and the split
immediately paid off: scanning with a patched toolchain surfaced real
stdlib vulnerabilities in the pinned go1.25.0 (GO-2026-6218 in
net/url, GO-2026-6090 in crypto/tls), which is what motivated the
deliberate toolchain upgrade to go1.26.8.

## Toolchain and scanner pinning

- Go: **1.26.8** everywhere — `go.mod` (`toolchain` directive), CI
  (`go-version: "1.26.8"`, `GOTOOLCHAIN=local`) and
  docs/development.md. The 1.25 series is EOL; a supported, fully
  patched toolchain is part of the vulnerability posture.
- govulncheck: **v1.8.0**, installed with
  `go install golang.org/x/vuln/cmd/govulncheck@v1.8.0` — never
  `@latest`. A scanner that auto-updates in CI is an unreviewed
  dependency change and a potential silent toolchain switcher. The
  pinned version is documented here and in docs/development.md; when an
  upgrade is needed it is a reviewed change that also updates this
  document.

## Secret scanning

`gitleaks/gitleaks-action@v3` scans the full history (checkout with
`fetch-depth: 0`) on every push, PR and weekly schedule. Findings fail
the job.

## Static analysis

1. `go vet` on the pure-Go packages (native Linux scope).
2. `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./cmd/...` — the
   desktop package type-checked for its real target.
3. A suspicious-pattern scan over `./engine` and `./system`:
   - `exec.Command("sh", "-c"` — shell-injection surface in engine code.
   - `password=` / `secret=` literals in non-test Go code (allowing the
     existing `REDACT` marker for deliberate tests/documentation).

   Every branch of the scan exits non-zero on a match. The v0.2
   version of this step ended in `|| true`, which made the entire check
   decorative; that is gone.

## Error handling and data discipline in the engine

- `engine/errors` classifies every failure (recoverable, retryable,
  invalid_input, configuration, environment, dependency_unavailable,
  corrupt_data, fatal) with subsystem and operation context; structured
  errors never embed raw configuration values.
- The system layer redacts protocol-core command output before logging.
- The store never logs record contents; diagnostics expose counters and
  sizes only.
- All local files are created with 0600/0700 permissions.
