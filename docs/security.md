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
   - Dangerous child-process patterns (PowerShell, cmd, curl, wget,
     netsh, route, Start-Process, Invoke-WebRequest,
     Expand-Archive, bitsadmin, certutil, mshta) in product Go code
     — allowlist-based, every exception justified inline (v0.9.8.6).
   - `password=` / `secret=` literals in non-test executable Go code
     (v0.10.5: matched through the comment-aware tokenizing scanner
     described below, not raw grep — see the v0.10.5 section).

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
Backends resolve their binary through `system.CoreLocator` in
controlled, bounded locations (v0.9.14): the application-managed
`<base>/cores` directories, the system PATH, and a fixed list of known
platform installation locations (root × known-subdirectory ×
known-executable-name — never a recursive filesystem walk). Nothing
found inside downloaded configuration data is ever executed.
Externally discovered binaries are REFERENCED, never replaced or
modified, and they are validated (version probe, config-dialect check,
smoke test) before use; a version string alone never proves
provenance — see the trust distinctions in [reuse.md](reuse.md).
Discovery reports path, version, origin and availability; a missing
core is a reportable state, not an error.

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

## v0.10.5 — comment-aware suspicious-pattern scanning

The v0.10.4-and-earlier suspicious-pattern scan matched raw text and
could not distinguish DOCUMENTATION from executable code: the Static
analysis job failed on `engine/config/validate.go:105` — a protocol
documentation comment describing the Hysteria2 URI form
(`obfs-password=<pw>`). The inverse defect sat in the child-process
check's line-oriented comment filter: inline comments and block-
comment interior lines not starting with `*` could false-positive,
while raw-string lines starting with `//` were invisible to it
(executable string data escaping the scan).

The scan now runs `tools/gosecscan`, which tokenizes each Go file
with `go/scanner` and reconstructs the source with every comment span
removed — multiline block comments keep their line count, so reported
`file:line` references stay true — then matches the SAME patterns
against the remaining executable text:

- Documentation (line/block/inline comments) can never trigger the
  credential gate again.
- The executable-text domain is byte-identical to the old raw scan's
  (spacing and string-literal contents included) — no weaker, no
  stronger.
- Fail-closed in both failure dimensions: a pattern match exits 1; a
  file that cannot be tokenized exits 2; the workflow refuses an
  empty file list rather than passing vacuously.
- No allowlists, no `|| true`, no `continue-on-error`, no disabled
  checks. Gitleaks and govulncheck are untouched.
- The regression suite (`tools/gosecscan/scan_test.go`) pins both
  directions: documentation classes accepted (including the exact
  validate.go shape and a scan of the real file), executable-finding
  classes rejected with line-number assertions, and the CLI
  exit-code contract (0 clean / 1 violations / 2 operational).

## v0.9.4 — release checksums and the application-update checker

- **Checksum sidecars on every release artifact.** The release
  pipeline publishes `<artifact>.sha256` for both deployment ZIPs and
  the Windows installer. A failed verification stops the operation —
  nothing ever weakens verification to make installation easier.
- **Application-update check stage (internal/appupdate).** The check
  resolves ONLY the official release feed (`Parsaetak/FreeIran`),
  selects the platform asset by exact name, compares versions
  honestly (pre-releases never outrank their release) and surfaces
  the checksum sidecar URL alongside the asset. It performs no
  download and no side effects; the download/stage/activate/rollback
  stages reuse the managed-core primitives (atomic writes, SHA-256
  verification, health checks) and inherit their guarantees.
- **Installed deployments.** The installer writes `installed.marker`;
  the workspace relocates to the per-user application-data directory.
  Uninstalling removes only the installed files — user data in the
  per-user workspace is never touched by the installer or uninstaller.

## v0.8.0 — process supervision security properties

The no-orphan guarantee is now enforced on every exit path, not only
on explicit Stop: job kill-on-close (Windows bound mode), process-
group SIGKILL (Unix), or Toolhelp32 tree termination (Windows
fallback) run when the supervised child exits for ANY reason —
natural exit, cancellation, forced termination — so a protocol core
or its descendants cannot outlive the supervisor even when the core
process itself exits first. Win32 return-value validation follows
the documented BOOL/HANDLE protocol; stale `GetLastError()` values
are never treated as failures. Executable resolution for system
shell binaries validates `%COMSPEC%` (must actually name cmd.exe)
before use and falls back to the validated `%SystemRoot%\System32`
location — a hijacked COMSPEC cannot smuggle an arbitrary binary.

## v0.9.6 — discovery, testing and identity surfaces

- **Smart search stays read-only and bounded.** The searcher only
  issues GETs to GitHub's public repository-search API and
  raw.githubusercontent.com; it never posts, never authenticates and
  never scrapes HTML pages (raw endpoints only, enforced by the
  reference extractor's pattern). Every budget exists to avoid
  hammering third parties: 3 queries / 12 repositories / 24 probes
  per cycle, 10-minute backoff after rate-limit responses, 6-hour
  result caching.
- **Untrusted content discipline.** Discovered candidates are
  untrusted input exactly like classic source content: parse →
  normalize → validate → dedupe before anything is stored; the parser
  panics fail only their own body. Content-derived discovery only
  follows references found inside content that already parsed
  successfully (≤8 per body), never blind Internet crawling.
- **Measurement records are credential-free.** Ping/URL/handshake
  metrics, provenance labels and failure classes store numbers and
  short classified strings; the URL-test failure classifier strips
  errors to fixed classes before persistence. No credential ever
  enters a metric record.
- **Verification probes are bounded and disposable.** VerifyTunnel
  issues one bounded HTTP request through the local tunnel with
  `DisableKeepAlives` (measurement integrity AND no lingering
  connections); the probe target honours the user's configured test
  URL.
- **Source health is observed, never self-reported.** Availability,
  parse success and yield are computed from FreeIran's own fetch and
  parse outcomes — no trust in source-provided claims.
- **SHEYTAN identity is metadata only.** The digital-system identity
  is a display string in the About surface, version resources and
  installer metadata; it changes no code path and collects nothing.

## v0.9.8.1 — network-tool safety and provider install integrity

**Internet-tools safety (`engine/netcheck/toolsafety.go`, §7).** Every
user-triggered diagnostic tool shares one hard safety policy:

- Targets are validated BEFORE any bytes leave the machine: URL
  scheme allowlist per tool family, credentials in URLs rejected
  outright (credentials never ride a diagnostic URL), port bounds.
- Private / link-local / loopback destinations are blocked by default
  for every generic diagnostic (tcp/tls/https/websocket/dns/udp/
  traceroute/path_mtu). Private-target permission is explicit and
  tool-scoped: only the genuinely local-endpoint tools (`socks5`,
  `http_connect` — testing the user's own local proxy) may target
  them implicitly; intentional local testing with a generic tool
  requires the `AllowPrivateTargets` capability. (Windows-CI root
  fix: the earlier over-broad policy made generic tools implicitly
  private-target-safe and bypassed the rebinding guard.)
- DNS-rebinding guard on direct probes: the hostname is resolved,
  every answer must pass the destination policy, and the connection
  is dialed to a validated address (resolve → validate → pin).
- Redirects are capped (3) and every hop's destination is
  re-validated against the same no-credentials and
  private-destination policy — on both the direct and the tunneled
  path. Response bodies are capped (256 KiB; bytes beyond the cap
  are neither read nor stored).
- Tools run ONLY on explicit user action — never automatically at
  startup or in the background; the service layer enforces this, and
  public-IP lookups are never background work. Concurrency is bounded
  (3 tokens in the app service).
- No remote JavaScript is ever executed and no configuration
  credentials are ever transmitted. Events and results carry only the
  validated host:port / URL forms, fixed error kinds and measurements.

**User-binary adoption ownership (`engine/provider/psiphon.go`).**
Adopting a user-provided Psiphon binary is copy-not-move: the source
is SHA-256'd and copied into FreeIran-managed storage under a
content-addressed name, the managed copy is verified
byte-equivalent, then validated and smoke-launched through the
`system` supervision layer (no visible console window, job-object /
process-tree cleanup, bounded lifetime, cancellation — the same
guarantees as the live provider). The user's original file is never
moved, renamed or deleted; it survives adoption, provider
uninstall, and Windows sharing-violation conditions. `Uninstall`
removes only FreeIran's managed state.

**Provider install integrity (`engine/provider/binary.go`).** The
managed-binary pipeline for Tor and Psiphon inherits the
protocol-core ordering rule — an unverified binary is never started,
not even once — and makes the checksum MANDATORY:

- SHA-256 verification against the published checksum authority
  happens after the resumable `.part` download and before unpack;
  verification failure aborts with rollback — nothing is weakened to
  make installation easier.
- Releases without published checksums are REFUSED: when the Psiphon
  channel exposes no release assets with digests, `Resolve` reports
  unavailability and `Install` refuses. A user-provided binary
  (`psiphon_user_binary`) is validated and smoke-launched before
  adoption.
- Tor verifies against the Tor Project's own
  `sha256sums-signed-build.txt`, fetched over TLS from the same
  official host (`dist.torproject.org`); the archive unpack is
  tar-slip guarded; activation is atomic with the previous binary
  retained for rollback. Honest limitation, documented in
  docs/providers.md: GPG verification of the checksum file itself is
  not performed (TLS from the official host — the same authority
  model as the Tor Browser updater's initial bootstrap).
- Provider processes run under `system.ManagedProcess` supervision
  (job objects — no orphans); runtime data, caches and logs are
  confined to `<workspace>/providers/<name>/` and logs are pruned
  (7 days / 16 files). No secrets are logged: provider settings carry
  only user-provided, non-secret inputs (bridge lines, plugin paths,
  extra config JSON), and the existing entry-level redaction applies
  to every log line.

## v0.9.8.6 — executable trust, bounded extraction, explicit proxies, command-surface audit

- **Digest-mandatory core installs.** A remotely acquired executable
  may never become runnable without AUTHORITATIVE integrity evidence.
  Core installs are REJECTED when the release publishes no digest
  (release-API digest field or `.dgst` sidecar); a locally computed
  SHA-256 is recorded as tamper evidence but is never a trust anchor.
  Asset URLs must be HTTPS (loopback test authorities excepted).
  Provider binaries keep their mandatory published-checksum gate and
  the copy-never-move adoption contract for user-provided files.
- **Bounded archive extraction** (`internal/safearchive`): archive
  size, total expansion, per-file size and entry-count limits;
  path-traversal and absolute-path rejection (POSIX and Windows
  forms); symlink/hardlink/device/fifo entries rejected; malformed
  archives fail closed. Zip-bomb and tar-slip test batteries cover
  the limits.
- **Explicit HTTP proxy policy** (`internal/httpx`): transports state
  their proxy mode (direct / environment / user URL / tunnel).
  DIRECT is the default everywhere — ambient `HTTP_PROXY` /
  `HTTPS_PROXY` / `ALL_PROXY` are ignored unless a caller explicitly
  opts in. The SSRF-guarded discovery client is direct by policy: an
  ambient proxy would bypass the dial-time destination validation
  (proxy-assisted destination confusion), so this is enforced, tested
  and documented — not assumed.
- **Route-trust boundary:** sources are classified official / user /
  public. Quick Connect and Auto connect through trusted routes only,
  unless the user explicitly enables untrusted public routes. A
  public node can be fast, stable, verified reachable and untrusted —
  reliability and route trust are separate dimensions.
- **TUN removed from the trusted surface:** the v0.9.8.5 Wintun
  backend (raw curl/PowerShell acquisition without digest
  verification, inverted route tracking, DHCP-not-restore DNS,
  unbounded extraction) was deleted. TUN reports
  experimental/unavailable on every platform and is NOT a kill
  switch.
- **Allowlist-based child-process audit** (security.yml): product Go
  code must not invoke PowerShell, cmd, curl, wget, netsh, route,
  Start-Process, Invoke-WebRequest, Expand-Archive or similar
  download/exec tools. Every exception is explicit and justified
  inline (currently exactly one: the validated cmd.exe PATH RESOLVER
  in system/resolve_windows.go, which never executes anything).
  The scan's push trigger was also repaired — `branches: ain]`
  never matched a real branch, so pushes to main were never scanned.
- **Release signing state:** Authenticode signing is ACTIVE only when
  the `WINDOWS_SIGNING_PFX` / `WINDOWS_SIGNING_PASSWORD` secrets are
  configured; signtool signs `FreeIran.exe` (before anything embeds
  it) and the installer, and `Get-AuthenticodeSignature` verifies both
  in CI. Without the secrets the release ships UNSIGNED and every
  artifact, `SIGNING-STATUS.txt` and the release notes say so —
  signing is never fabricated.
