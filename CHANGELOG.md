# Changelog

Historical release record — the authoritative home for release facts
and evidence. Structure since the v0.12.0 consolidation: current
product description lives in the README, forward phases in ROADMAP.md,
detailed architecture in the docs/ tree (see docs/README.md). Older
entries below v0.11.0 are compacted to version / purpose / key
changes / important evidence and limitations; their full original
narratives remain retrievable from git history. Historical entries
are historical: version numbers, core pins and verification claims
inside them describe the state of that release, not the current
state.

## v0.12.2 — blank-window root-cause repair, Tor/Psiphon removal, proxy chains, route evidence

v0.12.2 repairs the v0.12.1 blank-window regression at its root,
removes Tor and Psiphon from the active product (vertical removal —
implementation, lifecycle, UI, bindings, settings and discovery —
never only buttons), introduces real proxy chains compiled into ONE
core process, and polishes verified-route selection with an honest
evidence line.

### Blank-window root cause (frontend)

- **Root cause.** `App.tsx` called `useSuppressNativeContextMenu()`
  INSIDE a `useEffect` callback while the helper itself calls
  `useEffect` — a Rules of Hooks violation. The invalid-hook error
  threw inside App's own effect, above every error boundary, and
  React unmounted the whole tree: the backend reached
  `application_ready` / `window_show` / `warmup_complete` and the
  window appeared, but the visible UI was blank.
- **Fix.** The policy hook is mounted at the TOP LEVEL of the App
  component (the smallest correct architecture); the app-wide
  right-click policy itself is unchanged — rows still open FreeIran's
  own MenuSurface, text fields keep native clipboard/IME behaviour.
- **Regression guards.** A shell test renders the REAL `<App/>` and
  must paint the FreeIran navigation (the exact failure class the
  v0.12.1 tests could not see), plus a static Rules-of-Hooks check
  that walks every production source file's AST and rejects any hook
  called inside a nested callback (no ESLint exists in this repo;
  the checker also proves itself against the historical defect).

### Tor / Psiphon removal (vertical)

- `engine/provider` (Tor engine, Psiphon engine, managed-binary
  pipeline, adoption/discovery, fake binaries, all provider tests)
  deleted; `engine/connection/provider.go` provider sessions deleted;
  the `ProviderService` surface and its bindings deleted.
- Provider settings (`provider_mode`, `tor_bridge_lines`,
  `tor_transport_plugins`, `psiphon_extra_config`,
  `psiphon_user_binary`) removed; a persisted legacy
  `provider_mode` MIGRATES to the surviving Quick Connect route mode
  (`tor`/`psiphon` -> `auto`) — startup never bricks on a removed
  mode. Legacy tor/psiphon PROFILE modes migrate to `auto` instead
  of dropping the record.
- Core discovery specs for tor/psiphon removed from
  `system/execdiscovery`; the Cores page "Providers" section is gone
  (protocol-core management is the whole surface); Quick Connect lost
  the Tor/Psiphon modes and their education copy.
- Preserved: the core managers (xray/v2ray/sing-box/mihomo), core
  discovery/reuse, the checksum/trust pipeline, process supervision,
  and the connection lifecycle — untouched.

### Proxy Chains (one topology, one process)

- **Data model on the EXISTING collections authority.**
  `collections.json` moves to schema v2: groups carry a stable
  `kind` — `user` or `proxy_chain`; v1 documents migrate on load
  (every existing group becomes `user`). Chain records reference
  configuration IDs only (2-4 ordered hops) — credentials and
  configuration payloads are never copied; atomic persistence and
  the future-schema refusal are unchanged.
- **Service.** `ProxyChainService` (same authority): List / Create /
  Rename / Delete / AddHop / RemoveHop / ReorderHop / Details /
  Validate / CheckChain, with credential-free projections and
  actionable errors. A chain is not a remote source — no update
  semantics anywhere.
- **Compilation.** Chains compile into ONE core process through the
  existing per-core compiler boundaries: Xray via
  `streamSettings.sockopt.dialerProxy` (egress = `proxy`, earlier
  hops = `chain-N` tags, each dialing its predecessor), sing-box via
  the outbound `detour` field. V2Ray refuses chains explicitly
  ("does not support proxy chains"); Mihomo has no connection
  adapter (unchanged). Chain compositions fingerprint over every
  hop (`ChainFingerprint`), so runtime files and generation-cache
  keys never collide across chains.
- **Execution.** `ConnectionService.ConnectChain` rides the EXISTING
  connection state machine (select -> prepare -> start ONE core ->
  ready -> verify -> connected -> monitor -> recover). Chain
  readiness alone is never success; the same multi-target
  verification gate applies. Snapshots display the chain identity
  ("(proxy chain)") and the session label distinguishes the route
  kind for network tools.
- **Checks.** `CheckChain` = per-hop stored evidence PLUS a fresh
  end-to-end measurement through the compiled chain (existing
  tester, bounded, instance closed afterwards). "Check hops" runs
  through the ONE test queue.

### Configurations UX (v2rayN interaction concepts, FreeIran design)

- Scope rail gains PROXY CHAIN scopes (name + hop count) beside the
  source scopes and user groups, plus "+ New chain".
- The chain scope header shows hop count, completeness, the compiled
  preview (A -> B -> C), per-hop working evidence, and Check chain /
  Connect / Edit — never "Update source".
- The dense table gains a dedicated SECURITY column (transport and
  security are separate facts now) and an explicit Address : Port
  column; virtualization and server-side filtering are unchanged.
- Row context menu (the ONE MenuSurface) gains real chain actions:
  build a chain from the selected rows, add the selection to an
  existing chain, and remove/reorder hops inside a chain scope. No
  fake edit actions.
- The chain editor: ordered hop list with move up/down and remove,
  protocol + endpoint + latest evidence per hop, a live preview,
  validation before save, and Check hops / Check chain / Connect.
  Hops are referenced, never copied or edited through the chain.

### Verified-route selection evidence

- The Quick Connect picker shows the compact evidence line for the
  selected route — "Selected because: verified 2 min ago · 126 ms ·
  8/10 recent successes" — derived only from recorded observations
  (measurement freshness, real latency, the success rate over the
  recorded samples). No invented confidence percentages, no opaque
  score; with no verified candidate the line says exactly that.

### Packaging, version, docs

- VERSION, `internal/version`, `build/winres.json`, the frontend
  package version and all active version strings are 0.12.2;
  historical v0.12.1 entries remain historical only.
- README is current-product-only (no release-history duplication);
  CHANGELOG owns the release narrative; docs updated for the
  provider removal and the chain architecture.

## v0.12.1 — network-tool truth, per-source configuration workspace, desktop-class context UX

v0.12.1 fixes what the v0.12.0 runtime log exposed (six mislabeled
network diagnostics, the `core_discovered` provenance leak) and turns
Configurations into a source/subscription-aware workspace — on the
existing architecture, with no second engine anywhere.

### Network tools — status semantics and logging (§2–§4)

- **Ten-state tool model.** `ToolStatus` grows `partial`,
  `not_configured`, `not_applicable` and `unreachable` beside the
  existing `ok` / `failed` / `timeout` / `cancelled` /
  `invalid_target` / `unsupported`. Every state has a DISTINCT
  structured event (`network_tool_partial`,
  `network_tool_not_configured`, `network_tool_unreachable`, ...):
  the old contract collapsed every non-OK outcome into
  `network_tool_failed`. Pinned by `TestToolStatusEventMapping`.
- **Severity policy.** Environment facts (not configured / not
  applicable / unsupported / cancelled / partial) log at info;
  measured path verdicts (unreachable) and genuine failures warn.
- **DNS (§5).** Aggregation is query-level: some answers + some
  failures → `partial` with the successful measurements retained and
  the dominant failure class attached; all applicable resolvers
  failing → `failed`; a bounded encrypted-DoH comparison row
  (cloudflare-dns.com, RFC 8484 JSON) now rides INSIDE the existing
  diagnostic (never a second resolver subsystem) — a hijacked
  plaintext path beside a working encrypted path is real censorship
  evidence. Tunneled runs fetch DoH through the tunnel dialer.
- **HTTP CONNECT / SOCKS5 (§6/§7).** The hard-coded
  `127.0.0.1:1080` default is gone. Target resolution:
  explicit user target → protocol-compatible local endpoint supplied
  by the service layer (the live session's SOCKS endpoint for SOCKS5;
  a configured local HTTP inbound for HTTP CONNECT — the SOCKS
  session endpoint is never offered to the HTTP tool) →
  `not_configured`. A missing proxy renders "Not configured", not a
  red failure.
- **QUIC (§8).** A real, bounded QUIC v1 handshake probe with the
  HTTP/3 ALPN, built on `github.com/quic-go/quic-go` — the minimal
  dedicated diagnostic dependency (measurement only; never a
  dataplane, never user traffic). Resolve→validate→pin honours the
  private-range policy; silence classifies `unreachable`, an active
  but failed handshake classifies `failed`, completion is `ok` with
  ALPN/TLS/cipher details. Deterministic local `quic-go` listener
  fixtures; CI never depends on a public QUIC endpoint.
- **WebSocket (§9).** Default runs probe a bounded two-endpoint
  curated set and aggregate honestly: all succeed → `ok`, mixed →
  `partial` with per-target evidence, all fail → `failed`. An
  explicit target answers exactly that target (one probe, no
  fallback). An endpoint outage never becomes a censorship verdict.
- **Traceroute (§10).** Windows now walks the path through the native
  IP Helper ICMP API (IcmpCreateFile/IcmpSendEcho with per-probe TTL
  in IP_OPTION_INFORMATION) — user-mode, no elevation, no
  tracert.exe/route.exe/PowerShell/shell parsing. Unix keeps the raw
  ICMP walker behind the shared `hopWalker` interface. Status
  semantics: privilege gap → `unsupported`; explicit
  destination-unreachable evidence → `unreachable`; hops observed
  then silence → `unreachable` WITH the hop evidence; complete route
  → `ok`. Hops are never fabricated.
- **Tunnel diagnostics (§11).** Direct path + no tunnel →
  `not_applicable` ("No active tunnel" — the v0.11.2 wording read as
  a tunnel breakdown); tunneled + no tunnel → `not_configured`;
  active tunnel → the existing live snapshot + real SOCKS5 CONNECT.
  The UI greys the action with the honest hint while no tunnel is
  active.
- **Full catalogue audit (§12).** Every tool's target handling,
  prerequisites, status classification, timeout, cancellation,
  direct/tunneled semantics, logging and UI label verified; the
  shared ToolRunner, catalogue, safety layer, timeout policy, result
  model and log path remain the ONE of each (no duplicate network
  diagnostics engine).

### Runtime-log privacy (§33)

- **`core_discovered` is provenance-free again.** The normal log
  prints the compact form ("xray 26.3.27 available"); commit hashes,
  `go1.x` tuples and paths move to a debug-severity
  `core_discovered_details` record (developer diagnostics keep full
  provenance). `compactCoreVersion` accepts digits-and-dots tokens
  only, so runtime/commit fragments can never re-enter the normal
  log. Pinned by `TestCoreDiscoveredMessageProvenanceFree`.

### Configuration workspace (§14–§22)

- **Source scopes in the scope rail (§15/§16).** A "folder" is a
  logical browsing scope; source/subscription scopes map to
  `cfg.Source` and filter through the ONE server-side pipeline
  (`DataService.ListConfigsFiltered` with `source`). Counts come from
  the authoritative `SourceStatsList` — no React-side health
  recomputation, no duplicated counters.
- **Source scope header (§17).** Name, enabled/disabled, trust band,
  configuration/working counts, last successful fetch, last failure
  and its recorded reason — backend evidence only. Actions: **Update
  source**, **Check source**, **Check untested**, and More
  (retry failed / retest working in scope).
- **Targeted refresh (§18).** `SourceService.RefreshSource(id)`
  refreshes exactly one source through the SAME ingestion
  architecture (one fetcher, one parser pipeline, one store, one
  scheduler): validates the ID, respects the enabled policy,
  preserves dedupe/content-hash behaviour, updates stored configs,
  publishes normal source-refresh state and returns the source's
  bounded stats. Single-owner gate: targeted refresh and full cycles
  share the `ingesting` flag — two requests can never create two
  ingestion authorities (`TestConcurrentRefreshSourceOnlyOneCycle`).
- **Targeted check (§19).** Per-source check actions enqueue through
  the existing `TestQueueService.EnqueueByFilter` with
  `source = selectedSourceID` — the queue stays the sole test
  authority.
- **User groups (§20).** Test/rename/membership actions only — a user
  group is not a remote source and never offers "Update source".
- **Interaction density (§21/§26).** The dense virtualized table,
  sticky sortable header, organize-by, detail tabs and multi-select
  action model are preserved unchanged; v2rayN's interaction patterns
  remain the reference, never its visuals.
- **Sources integration (§27).** Source rows gain targeted actions
  (update this source / view its configurations / check them) that
  land in the matching configuration scope through the existing
  `freeiran:navigate` event model.
- **Performance (§28).** Source selection is a server-side filter,
  single-test results patch one record, source refresh reconciles the
  affected scope — no full-database refresh, no N+1 calls, and the
  ~20k-configuration virtualized surface is untouched.

### Desktop-class context/keyboard UX (§25/§29/§30)

- **The native browser/WebView context menu is suppressed
  app-wide** through one shared policy
  (`utilities/contextMenuPolicy.ts`): right-click on configurations
  opens FreeIran's own MenuSurface (unchanged single menu engine —
  Escape, arrows, Home/End, focus return, viewport flipping, scroll
  handling and ARIA semantics preserved); right-click anywhere else
  opens nothing. Text fields keep their native editing menu so
  Ctrl+C/Ctrl+V/Ctrl+A and IME editing are never broken.
- **Keyboard basics:** Ctrl+A selects the visible scope;
  Shift+F10 / ContextMenu open the row menu (existing); Delete in a
  user-group scope is the existing membership removal.

### Version + verification (§32/§34/§37)

- Version surfaces bumped to **0.12.1** (VERSION,
  `internal/version/version.go`, frontend package, package-lock,
  `build/winres.json`); historical release references preserved.
- Docs: README/ROADMAP/CHANGELOG updated; `docs/internet-tools.md`
  documents the exact ten-state semantics; `docs/configurations.md`
  (new) is the authoritative description of the configuration
  workspace; `docs/ui.md`, `docs/architecture.md` and
  `docs/autonomous-connectivity.md` reconciled.
- Tests: Go — status/event mapping, DNS aggregation, endpoint-tool
  prerequisites, QUIC local handshake + silence + tunneled-unsupported,
  WebSocket aggregation, tunnel-diagnostics applicability, targeted
  refresh (scoping/enabled policy/single-owner gate/concurrency),
  provenance-free core messages. Frontend — prerequisite vs failure
  presentation, tunnel-diagnostics gating, tool descriptions, source
  scope rail/header/actions, user-group action differences, row
  right-click MenuSurface, native-menu suppression policy,
  keyboard Ctrl+A. All tool fixtures local and deterministic.

## v0.12.0 — documentation consolidation + autonomous connectivity architecture handoff

v0.12.0 is a documentation/architecture-consolidation release: no
features, no architecture replaced, no dataplane change. Its purpose
is to permanently capture the FreeIran R&D direction (September 2026
report, `Parsaetak/Contents` Research-and-Development
`FreeIran-2026-09.md`) inside the canonical repository documentation
so future coding agents can implement it incrementally without
rediscovering the research.

### Canonical future-architecture document

- **`docs/autonomous-connectivity.md` (new)** — the single authority
  for long-term architecture: product objective
  ("the transport connects, the engine decides, the evidence
  proves"), the seven-layer threat model, full-device protection
  target, central DNS authority, IPv4/IPv6 proof states +
  `ConnectionProof` contract, the Windows Filtering Platform
  kill-switch state machine (explicitly future work — the current
  TUN is NOT a kill switch), censorship differential evidence
  (probe ladder + privacy-safe failure metadata), the adaptive
  transport engine, transport/protocol roadmap, AmneziaWG
  integration model (one lifecycle, no GUI fork, no second process
  manager), Tor modes (Tor Circumvention / Onion Only), I2P as a
  private overlay, multi-hop as trust topology, the canonical routing
  engine (engine/routing/), infrastructure-aware recovery,
  evidence-first recovery policy, the privacy exposure audit,
  public-IP verification states, privacy modes, the
  browser-anonymity boundary, the extended provider lifecycle,
  testing/clean-room acceptance, and the non-goals (banned claims).
  Every future item is explicitly PLANNED/NOT VERIFIED; no current
  capability is invented.
- **`docs/README.md` (new)** — documentation index: which document is
  authoritative for which domain (current behavior vs future
  contract vs release history), version source of truth, wording
  rules for current/historical/planned statements.

### Existing documentation updated (no duplication)

- `docs/architecture.md`: "Current architecture vs target
  architecture" section added (target components explicitly do not
  exist yet); stale Android-section wording that still described the
  desktop TUN as EXPERIMENTAL/DISABLED corrected (the desktop TUN is
  current since v0.11.3; Android remains a plan).
- `docs/tun.md`: evergreen header; new "Planned future work"
  section (IPv6 leak proof, DNS engine, WFP kill switch, continuous
  monitoring — all PLANNED); the evidence ladder is unchanged and
  not weakened.
- `docs/protocols.md`: current pin corrected to sing-box 1.14.1
  (stale 1.14.0-as-current wording; the 1.14.0 references in
  per-row evidence remain, now explicitly labeled as the evidence as
  executed in those release cycles); compact future
  adaptive-transport section (implemented / planned / not verified
  split; upstream support never promotes a row).
- `docs/providers.md`: future providers section (Tor circumvention,
  I2P, AmneziaWG) under the SAME one-manager lifecycle.
- `docs/security.md`: future privacy-security contract (minimal
  metadata, no hardware identifiers, no telemetry, evidence-first
  protection, WFP kill-switch target, privacy-safe diagnostics,
  banned claims).
- `docs/ui.md`: future UI concepts (privacy modes, ConnectionProof,
  trust topology, evidence surfaces) — current UI not redesigned;
  stale 1.14.0-as-current ECH wording corrected.
- `docs/ci.md`: future acceptance matrix (unit/integration/Windows/
  clean-room targets); all existing v0.11.5 CI evidence preserved.
- `docs/development.md`: documentation-maintenance rules (version
  source of truth, docs authority hierarchy, generated
  frontend/embed workflow, no stale current-version claims,
  current-vs-historical terminology, core-pin policy).

### README / ROADMAP / CHANGELOG de-duplication

- README rewritten around the current product: identity, current
  verified capabilities, current limitations, architecture summary,
  security/privacy contract, build, repository structure, short
  roadmap, documentation links, license. The fourteen repeated
  "What's new in v0.x" sections (v0.9.8.6 → v0.11.5) moved out —
  release history lives here. The stale repository-structure line
  that said `VERSION 0.9.11` is fixed (generic reference to the
  version source of truth).
- ROADMAP rewritten as a forward phase ladder (baseline, v0.12.0,
  Phases 1–6 matching the R&D phase order, testing/evidence
  standard, release acceptance, compact historical milestone table);
  the old per-release status narratives and duplicated
  implementation detail are removed (facts preserved here and in
  docs/); no future item is marked completed.
- This changelog keeps release facts; older entries compacted per
  the header note.

### Version bump + stale-consistency cleanup

- Version metadata moved to 0.12.0 in: `VERSION`,
  `internal/version/version.go` (default + comment),
  `frontend/package.json`, `frontend/package-lock.json`,
  `build/winres.json` (file/product version + manifest identity).
  Historical references (v0.11.4/v0.11.5/v0.10.4 and older, core
  pins, fixture versions) are untouched — classified by a repo-wide
  audit, only current-release surfaces updated.
- `engine/tunnel/tunnel.go` package comment repaired: it still said
  "TUN mode: DISABLED in this release" (the pre-v0.11.3 state);
  it now describes the current sing-box dataplane backend and
  preserves the v0.9.8.6 removal rationale as history. Comment-only
  change — no code behavior touched.
- sing-box upstream re-checked: upstream stable is 1.14.2 (also
  1.15.0-alpha.x prereleases); FreeIran intentionally retains the
  verified 1.14.1 pin — no digest fabricated, no half-upgrade, no
  dataplane change for documentation freshness (see
  docs/development.md core-pin policy).

### Verification scope (v0.12.0, explicit)

Documentation/version/comment changes only. Verified locally:
`git diff --check` (clean), full-tree `go build ./...` + `go vet` of
the touched Go surfaces (internal/version, engine/tunnel) + the
version-surface unit tests, frontend dependency install (`npm ci`)
+ typecheck + unit tests + production `build:embed`, a repo-wide
version/stale-text audit, a documentation link/path audit and a
clean-package audit of the source archive. Remote Actions were NOT
re-run (no push, no PR, no release — per the release rules); the
next maintainer push produces that evidence. The Windows TUN
physical runtime remains NOT VERIFIED (unchanged; docs/tun.md).

NOT VERIFIED in v0.12.0: any runtime behavior change (there is none
— no functional code changed); post-change remote CI/Security
verdicts (arrive with the next push).

---

## v0.11.5 — Windows GUI launch fix, real GUI launch proof, metadata-free runtime log

Focused repair release; no features, no architecture replaced.
Evidence scope: full local Linux `go test` suite (engine/system/
internal), windows/amd64 `go vet` of the complete tree, the
windows/amd64 `-H=windowsgui` desktop build with release stamps,
PE-subsystem check (WINDOWS_GUI) via debug/pe, frontend typecheck +
unit tests + production `build:embed`, binding regeneration with the
pinned wails3 v3.0.0-beta.19 CLI (twice, byte-identical, binding
contract tests green). Remote Actions NOT re-run.

- **GUI startup defect — root-caused and fixed (the headline).** The
  v0.11.4-ci binary booted the engine (application_start →
  store_open → manager_init → application_ready) but never showed a
  window. Root cause verified against the pinned wails
  v3.0.0-beta.19 source: the pre-Run initial tray reconcile
  dispatched through `application.InvokeAsync`, whose first step
  dereferences Wails' platform-app handle — assigned only inside
  `Run()` — so the panic killed the process on the main goroutine
  after the engine logs and before any window existed (windowsgui
  binaries have no console; no visible trace). Contributing
  fragility: a beta.19 pre-Run window is built without
  `WS_VISIBLE`, pre-Run `Show()` is a no-op, and visibility
  otherwise depended on WebView2 navigation completing. Fix: the ONE
  main window is created and shown from the `ApplicationStarted`
  event (deterministic create → show → hook → tray sequence); all
  v0.11.3 tray semantics unchanged; no second window, no sleeps, no
  headless downgrade.
- **Real Windows GUI launch proof.** New windows-only
  `TestWindowsGUILaunchProof` launches the ACTUAL built FreeIran.exe
  (not `--smoke-test`) in an isolated `FREEIRAN_HOME` workspace and
  FAILS unless a visible top-level window owned by the launched PID
  is observed natively (user32 EnumWindows + IsWindowVisible +
  GetWindowThreadProcessId) within a bounded timeout; distinguishes
  "process alive" from "GUI visible"; dumps window inventory/output/
  runtime-log tail on failure; always terminates by exact PID
  (tree force-kill). Wired into the CI Windows job and the release
  workflow after the real desktop executable is built; the release
  install rehearsal runs it against the INSTALLED executable.
- **Runtime log is metadata-free.** `ts` removed from the Entry
  schema and every UI/copy surface; no `commit` field and no
  Go-toolchain metadata; `seq` remains the ordering/paging
  mechanism; `application_start` compact ("FreeIran v0.11.5
  starting"); user-facing version renders exactly `v0.11.5`
  everywhere through `version.String()`; copied diagnostics carry
  the per-session seq prefix; developer-only provenance (commit, Go
  version) stays on the Developer Info surface. Regressions assert
  the raw JSON shape.
- **Wails warning bridge.** Wails-internal warnings/errors
  (WebView2 probing, navigation, environment) land in the runtime
  log (subsystem `wails`) through an slog bridge + ErrorHandler,
  with a deliberate Warn floor so Wails' own toolchain/vcs startup
  records can never reintroduce metadata.
- **Version/binding cleanup.** All version surfaces moved to 0.11.5;
  bindings regenerated with the pinned wails3 CLI (closing
  PRE-EXISTING v0.11.4 drift: OpenShellAtDataDir, CoreMihomo,
  TUNSnapshot, removed Entry timestamp).
- **sing-box drift check.** Upstream stable was 1.14.2; the verified
  1.14.1 pin retained deliberately (no unverified checksum, no
  half-upgrade); the Windows TUN physical runtime remained NOT
  VERIFIED (docs/tun.md).

NOT VERIFIED: the GUI launch proof's live verdict in the authoring
environment (executes in CI/release); post-release remote Actions.

---

## v0.11.4 — security repair, Windows CI hardening, TUN correctness hardening, documentation alignment

Repair + hardening release; no features, no architecture replaced.
Evidence scope: full Linux Go suite, windows/amd64 vet + build,
generated-config verification against the real pinned 1.14.1 binary,
Gitleaks reproduction with the exact CI scanner version, live-run
diagnosis of the Windows CI failure.

- **Security root-cause fix (the headline).** The v0.11.3 Security
  run's secret-scan failure (a committed WireGuard-shaped test
  literal in `engine/core/singbox/tun_test.go`) fixed at the ROOT:
  fixture material derived deterministically at runtime — no
  allowlist added, no rule weakened, no path excluded — plus a
  package-level regression test failing during ordinary `go test` if
  any committed key-shaped literal returns to the singbox test
  surface. docs/security.md describes the ACTUAL scan semantics.
- **TUN correctness hardening (three defects closed):** address
  selection FAILS CLOSED when every IPv4 candidate collides (no
  silent overlapping-range fallback); activation identity requires
  the EXACT session adapter (recorded name AND expected address on
  that same interface); "real traffic" proof strengthened with
  native read-only route observation (Windows IP Helper forwarding
  table: the TUN must OWN the covering IPv4 routes) plus the
  upstream route-loop check (no sing-box TCP socket sourced from the
  TUN address; positive physical-side pinning when TCP-observable;
  UDP-family outbounds honestly reported "not observable"). No
  netsh/route.exe/PowerShell anywhere; observation is lazy,
  bounded, fail-closed syscalls.
- **Windows CI hardening.** The v0.11.3 Windows job failed with a
  hosted-runner communication loss (47m vs v0.11.2's 3m07s);
  diagnosed from live run evidence; hardened with bounded
  `timeout-minutes: 60` and ZERO coverage loss.
- **sing-box 1.14.0 → 1.14.1 alignment.** CI real-core job + docs +
  evidence aligned; SHA-256 computed from the OFFICIAL upstream
  release asset; new real-binary test passes the complete TUN
  document through `sing-box check` of the pinned binary; the full
  existing real-binary smoke suite passes against 1.14.1.
- **Tray persistence regression checks + documentation truth
  repairs** (corrected Wintun packaging model — the official
  wintun.dll embedded in the sing-box binary via sing-tun v0.9.3,
  loaded from memory; honest evidence ladder; corrected scan
  semantics).

NOT VERIFIED: end-to-end Windows TUN runtime on a physical elevated
host (documented as such in docs/tun.md); post-release remote
Actions.

---

## v0.11.3 — functional Windows TUN (sing-box dataplane), tray ON/OFF setting, Security workflow repairs

Capability + repair release. Evidence scope: full Linux Go suite,
windows/amd64 cross-build + vet, frontend typecheck + tests +
production build, `sing-box check` of the generated TUN document
against the pinned real binary.

- **Functional Windows TUN (the headline).** TUN is no longer the
  v0.9.8.6 honest refusal: the managed, digest-verified sing-box
  core IS the TUN dataplane (native sing-box tun inbound,
  Wintun-backed). Transactional, OBSERVED activation: elevation
  check → verified core → compatibility validation → collision-free
  addressing from the live interface table → launch through the
  existing supervisor → readiness → adapter observed → DNS hijack
  (answered by sing-box's resolver over the proxy; system adapter
  DNS never mutated) → real no-explicit-proxy tunneled request
  verified → durable session marker → Active. Transactional disable
  with honest residual reporting; crash recovery via boot-time stale
  report inspecting only FreeIran's own recorded adapter. No new
  supervisor/queue/downloader. Full design: docs/tun.md.
- **Security workflow repairs (both push failures):** historical
  synthetic-fixture commit allowlist in `.gitleaks.toml`
  (commit-scoped, default rules fully active) and the reviewed
  `system/open_shell.go` dangerous-pattern exception with the
  contract inline (docs/security.md).
- **System Tray ON/OFF as a persistent setting** (`tray_enabled`,
  default ON, through the ONE settings store; native checkbox;
  setting-aware close-to-tray; recoverable from Settings).
- Everything from v0.11.2 preserved.

NOT VERIFIED: end-to-end Windows TUN runtime on a physical elevated
host (requires an elevated Windows host; documented in docs/tun.md).

---

## v0.11.2 — Mihomo managed core, internal config tabs, native Windows tray, honest tunnel diagnostics

Feature + repair release. Evidence scope: full Go suite (Linux) +
windows/amd64 cross-build + vet + the frontend battery, all executed
before packaging.

- **Mihomo as a coremgr-managed core** (download/health/UI; same
  verified pipeline, `.gz` extraction added); the connection-engine
  adapter deliberately out of scope — Mihomo is NOT advertised as a
  runnable backend until that adapter ships (asserted by test).
- **Configurations All scope + internal configuration tabs** keyed by
  stable configuration ID (open/focus/close/close others/close
  all/return to All).
- **Honest tunnel diagnostics** (Direct → "No tunnel selected";
  Tunnel path inactive → "Tunnel unavailable: no active tunnel" —
  never silently reported as tunneled).
- **Native Windows taskbar + system tray** (menu, show/focus
  restore, close-to-tray, teardown ordering).
- **Tor/Psiphon verification hardening; safe "Open shell here"**
  (exact, justified scanner exception); Diagnostics numerical
  spacing; queue/memory/logging safeguards preserved.

---

## v0.11.0 — compact Windows CI (two-layer proof), failure classification, transport agility, ECH foundation

CI-architecture + resilience-foundation release. Evidence scope:
full Linux suite including `-race`, frontend battery, real-core
verification with the three pinned cores (including the new ECH
shapes), security battery, windows/amd64 cross-build +
PE-subsystem verification, Windows compile-surface proof.

- **Windows CI redesigned to a two-layer proof** (local
  windows/amd64 vet+build from Linux + the Windows-native job)
  after measuring that the full hosted matrix duplicated
  platform-neutral execution; every Windows-specific behavior still
  executes on Windows.
- **Nine-class failure classification** (dns/tcp/tls/handshake/
  listener/verify/reset/timeout/transport) derived from observed
  failed-facet shapes only; stored per configuration and per
  history; repeated protocol-specific streaks demote candidates in
  automatic selection.
- **Route freshness / evidence tuple** (rank must not outrun the
  evidence that produced it).
- **Transport agility — preference, not a second failover engine.**
- **ECH support, schema-verified, sing-box-only** (four dedicated
  fields; evidence table of accept/reject shapes against the pinned
  binaries; honestly scoped: check/startup-level acceptance, NOT
  live ECH negotiation — docs/protocols.md).
- Machine-generated bindings restored (hand-maintained layer
  removed); bounded test admission; compact Normal logging; dense
  Configurations table; Black/White/Red icon; Android strategy
  documented as a plan.

---

## v0.10.5 — comment-aware security scanning, discovery route-trust policy

Security-tooling correctness + trust-policy gap fix. The raw-text
suspicious-pattern scanner (which could not distinguish documentation
from executable code) was replaced with a tokenizing, comment-aware
scanner — same detection intent, fewer false classes; discovery gained
an explicit route-trust boundary (source content routing policy).
Evidence: full local suites + the security battery; no scan rule
weakened.

## v0.10.4 — TUIC/Hysteria semantic corrections, WinINet bypass-grammar fix, documented query order

Protocol-semantics correction release against the pinned sing-box
binary: TUIC `udp_relay_mode` domain corrected to native | quic (the
prior "quadratic" was an invented value); Hysteria2 obfs domain
(salamander | gecko) and Hysteria v1 obfs (the v1 password string)
corrected; WireGuard endpoint semantics pinned; WinINet bypass-list
grammar fixed; the URL-test query order documented. All variants
re-verified through the real pinned binary (`check` + startup +
listener readiness).

## v0.10.3 — Windows WinINet ABI/semantics repair, protocol data-model truth pass, secret-free test material

Correctness release: v0.10.2 Windows failures repaired at root cause
(wrong test expectation, WinINet fidelity gap, cross-process test
race); TUIC/Hysteria/WireGuard protocol details moved into their OWN
semantic fields (replacing the v0.10.2 Network-slot overloading that
broke capability matching); test material made secret-free
(deterministically derived fixtures — the discipline that later
became the v0.11.4 root fix).

## v0.10.2 — Windows system-proxy repair, transactional ownership, personal import, real QUIC/WireGuard runtime

The v0.10.1 Windows regression fixed AT ROOT CAUSE (a broken WinINet
ABI that made every system-proxy activation fail on Windows); the
system-proxy ownership lifecycle made fully transactional; first-class
personal configuration import (URL lists, base64 subscriptions,
v2ray-style JSON, WireGuard INI — see docs/protocols.md); REAL runtime
support for Hysteria2, TUIC, WireGuard and Hysteria v1 through
sing-box (real-binary smoke evidence in docs/protocols.md).

## v0.10.1 — crash-safe system proxy, Windows CI survivability, WHITE/BLACK/RED theme

Trust-boundary release: system-proxy ownership durable across crashes
(durable marker + boot-time stale report — the pattern TUN later
adopted); the Windows CI failure class that made runs un-diagnosable
addressed at the workflow level; visual system reworked into the
WHITE/BLACK/RED identity.

## v0.9.15 — deep fixes: Tor acquisition root cause, one testing path, incremental UI, shared menu placement

Deep-fix release from a full acquisition/testing/UI audit: Tor
acquisition root cause repaired (not retried around); one testing
path (no duplicate admission paths); incremental UI updates; shared
menu placement. No new subsystem.

## v0.9.14 — idempotent local discovery, artifact reuse, full v0.9.13 closure

"Do the work once" as an architectural invariant: one discovery
authority, one reuse decision model, precise invalidation; version
consistency closed (winres had kept a stale 0.9.12 PE resource while
VERSION said 0.9.13 — the class of drift v0.12.0 audits for).

## v0.9.13 — clean runtime logging, configuration surface redesign, update-size transparency

Product-quality update: exactly ONE producer of the successful
`application_start` record; configuration surface redesigned;
update-size transparency (download sizes surfaced honestly).

## v0.9.12 — provider lifecycle monotonicity, binding-contract verification, persistence hardening

Full engineering closure of v0.9.11: the provider restart race
(`restart state = "starting"`) fixed at ROOT CAUSE; the same
monotonic-state invariant enforced across every asynchronous event
source in the provider layer; Wails binding-contract verification
introduced (the mechanism that later caught binding drift in
v0.11.5); persistence hardening.

## v0.9.11 — Windows lifecycle closure, host-independent archive security, Connection Profiles

Windows test-oracle root cause closed (CI run evidence); archive
security made host-independent (no toolchain-dependent extraction
behavior); Connection Profiles (first P2 roadmap feature). Validation
bounded by the actually-executed matrix.

## v0.9.10 — connection-lifetime architecture repair + beginner-first product release

The connection-lifecycle root-cause release: lifecycle proofs
asserted by the repository's own test matrix (unit + -race + native
+ fake-core lifecycle batteries + frontend battery + Windows
cross-compile + clean-room build); beginner-first product surface.

## v0.9.9 — core engine execution, runtime performance and deep functional upgrade

Core-engine/runtime release: execution and correctness upgrades
across the core pipeline, verified by unit + -race + native + real
protocol-core smoke + clean-room build.

## v0.9.8.8 — deep cleanup, stable filenames and repository hygiene

The TS2393 duplicate-worker root cause (CI run 35519469195) closed;
final canonical embedded-asset inventory (`index.html`,
`assets/index.js`, `assets/index.css`, `assets/export-worker.js` —
stale hashed/legacy files removed); repository hygiene. (Entry
compacted from the original README-referencing note in v0.12.0.)

## v0.9.8.7 — determinism and responsiveness release

CI recovery, event-driven UI synchronization, stable embedded asset
filenames, truthful Wails bindings (v-prefix normalization that
un-skipped the Windows suite), verified lifecycle/security
regressions.

## v0.9.8.6 — reliability, security and hygiene release

Deterministic Windows session teardown; session generation guards;
the route-trust boundary; **TUN disabled for cause** (the
non-transactional Wintun backend: inverted route tracking, DNS
"restore" that wrote DHCP, unverifiable curl/PowerShell Wintun
acquisition, unbounded extraction — removed rather than fixed; the
unique removal rationale is preserved historically in
`engine/tunnel/tun.go` comments and docs/tun.md); executable digest
enforcement; bounded archive extraction; explicit HTTP proxy policy;
pinned Wails toolchain contract.

## v0.9.8.4 — connection-state integrity + Logging Profiles (P1)

The v0.9.8.3 frontend CI failure resolved at its root (a stale
connection-state contract in tests); one asynchronous-operation
policy protecting the connection store; Logging Profiles shipped.

## v0.9.8.3 — Windows-CI correctness and process-supervision completion

The two v0.9.8.1 Windows failures closed at root causes; provider
subprocess unification completed; every claim backed by
actually-executed verification.

## v0.9.8.1 — latency representation fix, Tor/Psiphon providers, Internet tools

The latency measurement representation fixed at root (the Windows CI
failure — see docs/latency.md); Tor and Psiphon became first-class
providers on ONE shared managed-binary pipeline
(docs/providers.md); the user-triggered Internet-tools engine added
(engine/netcheck).

## v0.9.8 — Quick Connect UX + visual professionalization

A new Quick Connect home surface built entirely on the existing
connection engine; full-application visual professionalization (one
design system, no per-page hacks).

## v0.9.7 — structured logging, session identity, memory and discovery upgrades

Deep runtime/UI/discovery/memory upgrade from measured bulk-testing
behavior (thousands of queued configurations, many temporary core
processes, repetitive log events): structured logging with session
identity; adaptive memory pressure; discovery scaling.

## v0.9.6 — discovery → testing → ranking → connection rebuilt on real measurements

Complete engineering upgrade: the v0.9.5 Windows regression
root-caused and fixed at the exact defect site; the
discovery → testing → ranking → connection chain rebuilt around real
measurements with provenance discipline (multi-level discovery,
ping/URL test modes, measured ranking, verified connections +
controlled racing — docs/discovery.md, docs/latency.md).

## v0.9.5 — repository integrity

Repository made to match what v0.9.4 documented; stale artifacts
across the 0.9.1–0.9.4 packaging sessions removed; the v0.9.4
regression actually fixed (the httpx Windows finalization failure);
full verification matrix re-run end to end.

## v0.9.4 — startup lifecycle, release pipeline, version gate

Unified boot lifecycle (`boot → workspace_ready →
store_metadata_ready → services_ready`) with visible progress;
release packaging (Windows installer, release checksums); the
version-consistency gate (CI refuses when tag/VERSION disagree).

## v0.9.3 — the autonomous connection engine

FreeIran behaves like an automatic connectivity engine rather than a
configuration manager: Connect selects the best viable candidate from
real test history, validates it, chooses the backend and verifies the
result — the foundation the current connection engine still uses.

## v0.9.1 — configuration workspace fixes

Structural layout bug fixed (the per-row Test button wrapped onto an
invisible grid row); the testing workspace made readable.

## v0.9.0 — core loading fixed end to end

A managed core is never "installed" merely because a file exists: the
full pipeline `discover → verify → version → config validation →
launch → readiness → health → usable` runs before the UI reports
anything.

## v0.8.0 — Windows process supervision repaired and hardened

The Win32 job-object root cause (`SetInformationJobObject` BOOL
return-value protocol bug — the v0.7.0 CI failure) fixed; process
supervision hardened (kill-on-close job objects, CREATE_NO_WINDOW);
adaptive memory controller (Memory Booster 2.0) and desktop service
wiring.

## v0.6.0 — usable Windows VPN/proxy client

Managed Core Manager (Xray/V2Ray/sing-box install/verify/update/
rollback), no-console process launch, bounded-worker Test Queue,
System Proxy (WinINet), Speed Booster, expanded sources with
metadata, capability-driven failover. (The TUN mode shipped here was
DISABLED in v0.9.8.6 for cause; TUN returned in v0.11.3 as the
sing-box native dataplane — docs/tun.md.)
