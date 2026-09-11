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


## Protocol-core runtime security (v0.4.0)

Protocol cores are external executables with full local privileges —
the runtime treats them accordingly.

**Executable discovery, never execution of downloaded data.**
Backends resolve their binary through `system.CoreLocator` in exactly
two controlled locations: the application-managed `<base>/cores`
directory and the system PATH. Nothing found inside downloaded
configuration data is ever executed, and user-installed binaries are
never replaced or modified. Discovery reports path, version and
availability; a missing core is a reportable state, not an error.

**Untrusted configuration flow.** Source data crosses a one-way
pipeline: parse → normalize → validate → capability resolution →
backend-specific conversion → temporary runtime config → core. The
generated document is written with 0600 permissions inside a 0700
temporary directory, exists only for the life of the launch, and is
removed deterministically — after failed startups as well as clean
shutdowns, with the owning process stopped first (Windows file-lock
discipline).

**Credential redaction.** UUIDs, passwords and keys never reach logs,
diagnostics, selection reasons, error messages or UI snapshots.
`config.DisplayURL()` renders `vless://***@host:443`; captured core
output passes through `core.RedactLogText` (explicit secret values +
URL userinfo patterns) before entering the bounded log buffer; the
configuration details view exposes presence flags only.

**Future managed distribution.** The architecture reserves a managed
runtime directory and version pins (`engine/core/versions.go`) so a
future installer can bundle or fetch verified cores. When automatic
downloads are implemented they MUST follow: HTTPS from the official
source only, pinned versions, SHA-256 verification before anything is
executed, atomic installation and rollback on failure. Verification
before execution is a hard ordering — an unverified binary is never
started, not even once.
## Runtime log security (v0.5.0)

The persistent runtime log (`internal/logging`) is security-reviewed
surface:

- **Redaction before storage.** Every entry passes pattern redaction
  (UUIDs, credential-bearing protocol URLs, password/token/key
  parameters) plus caller-registered secret values BEFORE it reaches
  the file, the in-memory ring or any subscriber. There is no code
  path that writes raw entries.
- **Protocol-core output.** Core stdout/stderr is captured through
  `engine/core.LogBuffer`, which redacts the active configuration's
  secret fields and URL credentials line-wise before anything else
  sees the text.
- **File hygiene.** The log lives under the platform application-data
  directory with 0600 file permissions inside a 0700 directory; it is
  size-rotated with a bounded backup count so it can neither grow
  unbounded nor be truncated into an inconsistent state (startup
  recovery rotates an oversized leftover primary).
- **UI exposure.** The log viewer receives redacted entries only,
  incrementally by sequence number; "copy diagnostics" copies the same
  redacted text. The UI never reads the raw file.
- **No secrets in new settings.** Settings persist non-sensitive
  preferences only (backend name, intervals, log limits, motion
  preference) under the config directory with 0600 permissions.
