# FreeIran

A lightweight, free, open-source VPN configuration manager and proxy
client for Windows (and, architecturally, any desktop platform), built
around a shared Go engine for discovering, testing, maintaining,
running and tunneling through publicly available proxy/VPN
configurations.

**Project:** FreeIran — A SHEYTAN Digital System
**Architect:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran
**Current version:** 0.9.8.5 (see `VERSION`)
**Status:** production architecture — multi-core protocol runtime with
managed installation, multi-level node discovery, Ping/URL test modes
with measured ranking, verified-connection engine with racing,
environment intelligence, system proxy and TUN mode, unified adaptive
memory control and kernel-level process supervision

---

## What's new in v0.9.8.5

v0.9.8.5 is a connection-verification and network-diagnostics
upgrade: the verify gate stops trusting one endpoint, the session's
stability is measured instead of assumed, the Internet check explains
WHICH stage failed, the DNS tool becomes a real resolver comparison,
the Network tab answers "who am I on this network", and a full UI/UX
audit removes dead styles and duplicate helpers across every tab.

### Multi-target connection verification (quorum)

- `engine/connection/verify.go`: the single-endpoint probe is now a
  bounded multi-target verification with a documented quorum rule —
  three independently operated endpoints (Google, Cloudflare, Apple),
  strict majority required (2 of 3). One unrelated third-party outage
  can no longer fail an otherwise healthy route, and one lucky
  endpoint can no longer declare a broken route verified. Per-target
  evidence (status, latency, transport, failure class) rides on the
  result; deterministic failures (4xx, refused, TLS interception)
  never retry, genuinely transient ones (timeout, reset, 5xx) retry
  exactly once with bounded backoff + jitter.
- The tester's end-to-end probe now runs the SAME verification model
  (`engine/tester/core_probe.go` deduplicated onto
  `connection.VerifyTunnel`) — one gate, not two.

### Stability degradation + recovery (evidence, not assumptions)

- A verified session is periodically RE-verified through the active
  tunnel (`engine/connection`): one failed recheck marks the snapshot
  `degraded` but never tears the session down; consecutive failures
  past the threshold transition to `connection_failed` and hand control
  to the bounded recovery loop; a success at any point resets the
  evidence. Provider sessions (Tor/Psiphon) join the same schedule.
  Snapshots carry `verified_at` and `verify_failures`.

### Staged Internet diagnostics

- `engine/netcheck/stages.go`: the connectivity report now carries
  the ordered ladder — local link → local IP → DNS → TCP → TLS →
  HTTPS → captive portal → direct Internet → tunnel Internet — each
  rung with status, measured latency and a failure class, plus
  `failed_stage` naming the first broken rung. The seven-state
  classifier stays authoritative; the ladder explains it.

### DNS diagnostics (resolver comparison)

- `engine/netcheck/dnsdiag.go`: the DNS tool compares the system
  resolver against a curated public set (Cloudflare, Google, Quad9)
  for A and AAAA, over UDP with TCP fallback, with honest failure
  classification (timeout / refused / SERVFAIL / NXDOMAIN / empty /
  malformed / unreachable / cancelled / invalid). No DNSSEC claims.
  Safety: name validation before any bytes leave the machine,
  private-resolver rejection, bounded response sizes, per-query and
  overall timeouts, full cancellation.

### Network Identity

- `engine/netcheck/identity.go` + the Network tab's new identity card:
  route-relevant local IPv4/IPv6 (no packets sent — routing-table
  lookup only) with its interface, the public exit IP (direct and/or
  through the active tunnel, with match comparison), and ISP/ASN/
  country metadata from documented keyless sources. Unavailable
  metadata reports Unknown — never fabricated. Explicit user action
  only; a 60-second cache prevents hammering the endpoints on page
  re-renders.

### UI/UX audit (every tab)

- Dead classes removed (`.card.onboarding`, `.provider-card`,
  `.qc-orb-icon`, `.progress.indeterminate-none`); two referenced-but-
  undefined styles now properly defined (`.field-grid.two` form grid,
  `.badge.success-dim`); ghost + danger buttons keep their error color
  on hover; five Dashboard inline styles replaced by token-driven
  spacing utilities; duplicate helpers consolidated (`formatBytes`,
  error description) onto the shared modules.
- Responsive verification: 9 tabs × 6 viewports (1920×1080, 1440×900,
  1280×720, 1024×768, 800×600, 480×900) — zero horizontal overflow.

### Housekeeping

- Version 0.9.8.5 everywhere (VERSION, internal/version,
  frontend/package.json + lock, Windows resources, README, ROADMAP).
- New test batteries: multi-target quorum/retry (8 tests), manager
  stability degradation + recovery (2 tests), staged ladder (5 tests),
  DNS diagnostic matrix (14 tests), network identity (12 tests),
  frontend Network-page surfaces (7 tests).

---

## What's new in v0.9.8.4

v0.9.8.4 is a connection-state integrity release plus the roadmap's
P1 Logging Profiles feature. It resolves the v0.9.8.3 frontend CI
failure at its root (a stale connection-state contract in the tests,
not a product bug), makes one asynchronous-operation policy protect
the connection store, and completes one logging policy across the
application.

### Frontend CI failure — root-cause fix (run 35452888940)

- The two failing Quick Connect tests encoded the OBSOLETE contract
  (`state: "connected"` = verified Internet). The v0.9.8.3 engine
  introduced the verification boundary: `connected` means the local
  route is established while Internet verification is still pending;
  `connected_verified` is the only final success. The page mapped
  this correctly and rendered the transitional "Verifying" hero —
  the tests, not the product, were stale. The connected-state tests
  now assert the real contract (`connected_verified` +
  `verification: "usable"`), and new explicit tests pin the
  transitional rendering of `connected` (no "Verified" badge — core
  readiness is never Internet verification), `verifying`,
  `waiting_for_ready` and the full state list.

### Stale-operation protection (connection store)

- `useConnectionStore` operations now capture a generation token when
  they start and apply their result only while that generation is
  current: a late-resolving `connect()` / `connectBest()` /
  `refresh()` can no longer overwrite a newer state, a stale error
  can never replace the current one, disconnect/reconnect invalidate
  previous operations, authoritative events outrank unresolved
  promises, and teardown (or a test reset) can drop every pending
  operation deterministically — no arbitrary sleeps. Regression
  matrix: `frontend/src/state/connectionStore.test.ts` (13 tests,
  one per required scenario).

### One authoritative connection state machine (frontend + backend)

- The frontend store type now carries the complete engine state list
  (`verifying`, `connected_verified` included), and the Quick
  Connect/Connection surfaces share it. A genuine backend defect was
  found and fixed during the audit: provider sessions (Tor/Psiphon)
  stayed on `connected` after their Internet verification PASSED,
  which under the verified contract left verified provider sessions
  looking unverified — they now reach `connected_verified` exactly
  like core-based sessions (`engine/connection/provider.go`).
- Recovery no longer treats `connected` as healthy: only
  `connected_verified` (or idle) clears a recovery episode, so the
  verification gate — not local readiness — decides
  (`engine/app/recoveryservice.go`, per-state decision table pinned
  by `recoveryservice_state_test.go`, plus an end-to-end test that
  recovers through a REAL tunnel verification via the SOCKS-relay
  fake core).

### P1 — Logging Profiles (Normal / Detailed / Debug)

- ONE policy in the logger (`internal/logging`): admission =
  severity × profile × lifecycle tier, checked before any formatting
  cost. Normal (default) stays compact — routine verbose diagnostics
  suppressed, no per-record session/event identity. Detailed adds
  lifecycle-tagged diagnostics and full record identity. Debug adds
  verbose diagnostics and correlation identifiers on every record
  (still bounded by rotation + retention). Redaction is unchanged in
  every profile.
- Settings: a `Logging` control (Normal | Detailed | Debug) persists
  `logging_profile` with the existing settings system and switches
  the live logger immediately — no restart. The control explains
  each profile concretely; no vague wording.
- Regression matrix: `internal/logging/logging_profile_test.go`
  (per-profile admission, compact on-disk records, identity
  uniqueness, redaction, runtime switching Normal → Detailed →
  Debug → Normal, rotation/retention bounds) and
  `engine/app/loggingservice_profile_test.go` (validation,
  persistence, live switch).

### Housekeeping

- Version 0.9.8.4 everywhere (VERSION, internal/version,
  frontend/package.json + lock, Windows resources incl. regenerated
  PE `.syso`, installer, README, ROADMAP).
- ROADMAP baseline corrected to `0.9.8.4`: v0.9.8.3 work recorded as
  completed historical implementation (fresh-selection loop, verified
  state, recovery re-ranking, reorder, ports, workspace, installer,
  compact logging, adaptive memory, Tor/Psiphon), P1 Logging Profiles
  marked completed.
- Embed assets regenerated; stale unreferenced `dist` bundles from
  earlier builds removed (`cmd/freeiran/frontend/dist`).

---

## What's new in v0.9.8.3

v0.9.8.3 is a Windows-CI correctness and process-supervision
completion release. It closes the two v0.9.8.1 Windows failures at
their root causes and completes the provider subprocess unification,
with every claim below backed by an actually-executed verification.

### Windows CI failures — root-cause fixes (verified by run 35308409161)

- **Failure A** (`engine/netcheck/TestSafeDialerResolveCheckPin`):
  the tool-safety policy listed `tcp/tls/https/websocket/dns` as
  implicitly private-target-safe, so a generic HTTPS diagnostic could
  resolve `localhost` → dial `127.0.0.1` and connect (the Windows
  runner has a live listener on port 80; Linux passed only by
  connection-refused accident). Fixed with a semantic capability
  model: only the genuinely local-endpoint tools (`socks5`,
  `http_connect`) may aim at private endpoints implicitly; every
  other tool requires the explicit `AllowPrivateTargets` capability.
  The DNS-rebinding guard (resolve → validate EVERY answer → dial the
  validated/pinned address) is unchanged in strength and now also
  covers host:port targets at validation time (the tunneled path).
  Redirect safety is implemented on both paths: every hop is
  re-validated (cap, no credentials in redirect URLs, destination
  policy). Policy refusals are typed and classify as `invalid_target`.
- **Failure B** (`engine/provider/TestPsiphonUserBinaryPath`,
  "...being used by another process"): the adoption flow
  smoke-launched the user's executable and then MOVED (deleted) the
  original. The ownership model is now copy-not-move: stat → SHA-256
  → copy into managed storage under a content-addressed name →
  verify byte-equivalence → validate and smoke-test the MANAGED COPY
  through the system supervision layer → manifest references the
  managed copy. The user's original file is never moved, renamed,
  deleted or modified — after adoption, provider start/stop, or
  uninstall.
- Both fixes are verified by the remote Windows job (GitHub Actions
  run `35308409161`, job "Windows tests and desktop build", attempt
  1: full `go test -count=1 ./...`, runtime smoke test, desktop
  build, PE GUI-subsystem verification — all PASS), in addition to
  the local Linux matrix (tests, race, vet, gofmt, frontend, native).

### Provider subprocess unification completed (Tor)

- Tor's binary validation (`tor --version`) and smoke test
  (`--verify-config`) now run through `system.RunProbe` — the SAME
  supervision pipeline as the live providers (no visible console
  window on Windows, job-object/process-tree cleanup, bounded
  lifetime, cancellation, deterministic termination). This closes
  the last provider-local raw `os/exec` path; `engine/provider`
  contains no Windows process flags of its own.

### Honest verification status

- Verified locally (Linux): gofmt/vet clean; `go test -count=1` and
  `-race` for engine/system/internal (34 packages) with deterministic
  fake cores; frontend (typecheck, 104 vitest tests, production
  build, embed staging); native (unit tests, `native_accel` build,
  cross-language tests, benchmark smoke); pinned real protocol cores
  (V2Ray/Xray/sing-box smoke suites).
- Verified remotely: the Windows job at the fix commit (above).
- Not verified: the v0.9.8.3 commit itself was not pushed to GitHub
  (no push credentials in the release environment); its Windows CI
  run has therefore not executed. The changes since the verified
  commit are the Tor RunProbe migration, the version bump and
  documentation.

---

## What's new in v0.9.8.1

v0.9.8.1 is a correctness-and-capability release: the latency
measurement representation is fixed at its root (the Windows CI
failure), Tor and Psiphon become first-class providers on one shared
managed-binary pipeline, a user-triggered Internet-tools engine is
added, and provider routes run through the SAME verified connection
lifecycle as configurations.

### Latency measurement semantics — the Windows CI root fix (§2)

- The Windows CI failure (run 35287863799,
  `engine/tester/TestTCPProbeReachable`, "latency should be
  measured") is fixed by changing the REPRESENTATION, not test
  expectations: coarse Windows monotonic clocks can measure a
  successful loopback dial as exactly 0, and ANY real
  sub-millisecond measurement truncated to 0 ms was read as "not
  measured" by quality classification, ranking and the UI.
- `engine/tester/latency.go` defines canonical rules R1–R6: the true
  `time.Duration` is preserved; successful raw ≤ 0 readings quantize
  to `ClockFloor` (1 ns — no artificial sleeps); `Result.Measured`
  is the authoritative flag; 0 ms + measured means "measured,
  sub-millisecond", rendered **"< 1 ms"**; sub-ms sorts FIRST among
  measured. All consumers were migrated — ranking
  (`Score.LatencyMSMeasured`), ping metrics (`SubMS`), connection
  modes, testqueue stats, core health (≥ 1 ms when ready), netcheck
  probes, the Quick Connect picker (`latency_ms_measured`). See
  [docs/latency.md](docs/latency.md).

### Internet tools (§5/§7)

- New shared tools engine in `engine/netcheck`: fifteen tools
  (internet, dns, tcp, tls, https, http_connect, socks5, websocket,
  udp, quic — honestly unsupported: no QUIC stack is compiled;
  traceroute — raw ICMP TTL walk, privilege-gated; path_mtu —
  bounded DNS payload ladder, ~300 B cap, honestly labelled;
  captive_portal; public_ip; tunnel_diagnostics), one structured
  result contract, timeouts clamped [1 s, 60 s], and the
  `toolsafety.go` policy: scheme allowlist, credentials in URLs
  rejected, private/link-local destinations blocked for every
  generic diagnostic (implicit permission only for the genuinely
  local-endpoint `socks5`/`http_connect` tools; everything else
  needs the explicit `AllowPrivateTargets` capability), DNS-rebinding
  guard (resolve → validate → pin), redirect cap 3 with per-hop
  destination re-validation, response cap 256 KiB.
- Tools run ONLY on explicit user action — never at startup — with
  bounded concurrency (3 tokens); results run direct or through the
  active tunnel (path `direct|tunneled`). See
  [docs/internet-tools.md](docs/internet-tools.md).

### Tor and Psiphon — first-class providers (§8/§9)

- Tor: official `dist.torproject.org` expert bundles (verified
  layout: `torbrowser/<ver>/tor-expert-bundle-<platform>-<ver>.tar.gz`
  with `sha256sums-signed-build.txt` as checksum authority, fetched
  over TLS from the same host; pinned channel 15.0.20, "latest" via
  directory listing with alpha skipping). Bootstrap parsed from REAL
  `Bootstrapped X% (Tag)` lines — never timers; readiness also
  observed via the SOCKS endpoint. Bridges user-provided ONLY;
  pluggable transports via user plugin paths; WebTunnel reported
  from the installed version only. BSD-3-Clause attribution
  surfaced. Honest limitation: GPG verification of the checksum
  file itself is not performed.
- Psiphon: official tunnel-core console client channel (default
  `Psiphon-Labs/psiphon-tunnel-core`; live audit 2026-09-18 — the
  releases publish only mobile library archives with digests, and
  the `psiphon-tunnel-core-binaries` location is a moving RC branch
  with no checksum authority, so managed install honestly reports
  unavailability and no raw branch binary is ever downloaded or
  executed). A user binary path (`psiphon_user_binary`) is the
  supported acquisition: the file is COPIED into managed storage
  under a content-addressed name (the user's original is never
  moved, renamed or deleted) and the managed copy is validated +
  smoke-launched through the system supervision layer before
  adoption. Readiness is observed from the real runtime (both
  local proxy ports accepting); capabilities reported ONLY when
  running; Psiware attribution surfaced; nothing statically
  embedded.

### Unified providers, Auto mode and connection integration (§8–§14)

- One Provider contract (Name/Kind/Resolve/Install/Uninstall/Start/
  Stop/State/Info/Endpoints/Health/Cleanup) + Manager; kinds core
  (xray/v2ray/sing-box via a thin adapter over the SAME coremgr
  pipeline — no duplicate install machinery), tor, psiphon; one
  managed-binary pipeline (`.part` download → MANDATORY SHA-256
  verify → tar.gz unpack, tar-slip guarded → validate → smoke launch
  → atomic activate with rollback → manifest) under
  `<workspace>/providers/<name>/`. Safety: one managed instance per
  provider, Windows job objects (no orphans), deterministic stop on
  failed starts, log pruning (7 d / 16 files).
- `Manager.ConnectProvider` runs the SAME lifecycle for provider
  routes: select → start → route via local SOCKS → VerifyTunnel (no
  bypassing) → connected → the SAME monitor loop; Reconnect remembers
  the route. Auto mode (`engine/app/providerservice.go`) scores
  evidence (installed availability, live health, verified-success
  freshness, latency, failure-streak stability, ranking composite)
  with NO hardcoded priority; choices are explainable like ranking.
  See [docs/providers.md](docs/providers.md).

### UI, tests and version

- Quick Connect gains the provider mode selector (Auto /
  Configurations / Tor / Psiphon) above the picker — the single
  primary action is preserved; uninstalled providers visibly marked;
  measured sub-ms shows "< 1 ms". Cores gains the Providers section
  (Tor/Psiphon cards: version/state/source/license/notice/endpoints/
  health/capabilities + lifecycle actions); Network gains the
  Internet tools grid (per-tool target, via-tunnel toggle when a
  tunnel is active — nothing runs automatically).
- Tests: `engine/provider/testdata/{faketor,fakepsiphon}`
  deterministic stand-ins (built by the harness; missing fixture =
  hard failure); full lifecycle coverage incl. HTTP-through-provider
  via a real SOCKS relay; connection provider-session tests;
  `engine/tester/latency_test.go` + `engine/ranking/subms_test.go`;
  frontend vitest 91 → 104 tests (sub-ms ordering, provider mode
  routing, provider store). Version 0.9.8.1 everywhere (VERSION,
  internal/version, frontend/package.json, winres, installer).

---

## What's new in v0.9.8

v0.9.8 is a focused UX release: a new **Quick Connect** home surface
built entirely on the existing connection engine, plus a
full-application visual professionalization pass driven by a UI audit
(one design system, no per-page hacks).

### Quick Connect — the home connection surface

- New top-level **Quick Connect** page, first item in the primary
  navigation and the application landing surface. It contains exactly
  one primary action — **CONNECT** — and intentionally nothing else:
  no tables, diagnostics or management actions.
- The compact configuration picker sits directly above the connect
  button. It is populated ONLY from the ranking engine's
  credential-free candidate views (`BestCandidates`, bounded and
  TTL-cached server-side) — the full configuration database is never
  loaded to render it, and it never displays URIs, UUIDs, secrets or
  source URLs.
- **Measured-ping ordering, honestly labelled.** Candidates are
  ordered: verified usable first, then by measured ping (median of
  real test observations — never source-provided or estimated
  latency), then by the engine's own aggregates (recent success,
  stability, freshness, score). Untested configurations are shown
  AFTER every verified candidate with an explicit dash and an
  "untested" status dot; measurements older than 30 minutes are
  labelled "stale". Dead/unconnectable candidates never appear.
- One connect decision tree, all of it existing engine paths:
  an explicit selection connects to that configuration; otherwise the
  engine's best-candidate selection runs; with no viable candidates at
  all the adaptive discovery flow (detect → discover → test → rank →
  connect → verify) runs — the user never needs to understand the
  discovery engine.
- State visuals are driven by the real backend state machine
  (preparing / connecting / verifying / connected / failed) with
  CSS-only animations per state (scan pulse, progressive orbit, radar
  heartbeat, stable success glow, brief failure shake). Reduced motion
  (preference or in-app setting) removes every continuous animation
  and keeps usability.
- While connected, CONNECTED becomes the single primary state
  representation — no competing second button. A small secondary link
  leads to the advanced Connection page for disconnect, recovery
  details and diagnostics.

### UI professionalization (whole application)

- Design-system repairs: legacy CSS tokens that never existed
  (`--text-11/--text-12/--radius-8`) are now defined, so test-state
  chips and panels render their intended sizes and radii; the shared
  button spinner got a base rule (inline loading and status-chip
  spinners were invisible before); `.page-body` no longer double-spaces
  its children.
- Page headers normalized to one structure (title block +
  `.page-actions`) across all nine pages — Cores and Network included.
- Loading buttons reserve a fixed icon slot (Save, Refresh now, dialog
  footers, Quick Connect): a spinner can never resize the control.
- The Connection "Re-scan" control stays put while scanning instead of
  being replaced by a chip; unicode glyph buttons (▶/⏸/✓/⚠) now use
  the real icon set.
- One icon-size hierarchy (icon-only 15, text buttons 14, compact rows
  13, empty states 20), one casing for test-state chips, eyebrow card
  titles applied consistently, table cells that can carry long values
  (core paths, attempt errors) clip instead of stretching cards.
- Accessibility: Quick Connect is fully keyboard operable (listbox
  with `aria-activedescendant`, focus management, Escape to close),
  exposes `aria-live` status and `aria-busy` while in flight, and all
  pages keep visible focus rings.

### Quick Connect vs Connection

Quick Connect is simple, fast and minimal — the one-tap path.
Connection remains advanced and diagnostic: per-configuration
selection, attempt history, recovery details, tunnel/system
integration and core controls. Dashboard keeps its overview role and
its connect behaviour is unchanged.

---

## What's new in v0.9.7

v0.9.7 is a deep runtime, UI, discovery and memory upgrade built on
the measured evidence of bulk-testing behaviour (thousands of queued
configurations, many temporary core processes, repetitive log events
and a "latency 0 ms" connection report).

### Structured logging & session identity

- Every log entry now carries `session_id` (unique per launch),
  `event_id` (unique per entry), the monotonic per-session `seq`, and
  correlation fields (`parent_event_id`, `batch_id`, `test_id`,
  `config_id`, `core`, `pid`, `listener`, `duration_ms`, `status`) —
  important structured values never hide inside `message`.
- Lifecycle semantics are explicit and non-duplicative:
  `application_start` → `workspace_ready` → `services_ready` →
  `application_ready` → `background_warmup` → `application_shutdown`.
  Workspace/base-path initialization rides structured fields, not a
  second start message.
- Core lifecycle: `core_start` is debug-level, `core_ready` is the one
  info record (with startup duration), `core_exit` carries the
  lifetime and reason; `core_discovered` only fires for actually
  available cores (the unfiltered refresh loop bug is fixed).
- Noise control: `memory_policy_changed` replaces per-tick
  `memory_adjust` (fires only on material policy changes);
  `job_fallback` logs once per session with an aggregate
  `fallback_processes` counter instead of one warning per process;
  downward pressure transitions are `memory_pressure_recovered` (info),
  not warnings; cleanup passes emit one `cleanup_completed` summary
  with byte totals. JSONL remains the on-disk format; redaction
  applies to every entry and every structured string.

### Connection metrics — core readiness is not latency

- The "connection_success (latency 0 ms)" defect is fixed at the
  root: the loopback listener probe is no longer reported as network
  latency. Snapshots now separate `core_ready_ms` (local startup),
  `ping_median_ms` / `url_total_ms` (verified end-to-end) and a
  `verification` stage (`none` → `usable`/`failed`). Recovery reports
  name the candidate and backend correctly and never print a fabricated
  ping.

### Bounded test pipeline & core-probe pool

- The test queue gains an independent CORE-PROBE POOL:
  `core_probe_concurrency` (default 2, hard ceiling 4) bounds the
  number of simultaneous temporary Xray/V2Ray/sing-box processes no
  matter how many workers or queued configurations exist. Excess
  workers park on the core-slot semaphore (backpressure). Cancellation
  unwinds parked tasks without spawning cores; no slot is ever leaked.
- The adaptive memory booster may scale the worker pool but can never
  raise the core-probe cap: memory availability does not create more
  core processes.
- Queue pause / resume from the testing control bar; the queue panel
  shows Passed/Failed/Timeout/Cancelled plus live `Active cores` and
  `Queue` depth as one aggregated stream.

### Configurations page (§6/§18/§19)

- The row grid is fixed at nine one-to-one tracks (the v0.9.6 URL
  column regression that pushed the Test button into an implicit
  column). The Test action lives in a dedicated `.actions` cell —
  fixed width, `justify-self:end`, `nowrap`, never shrinking — so it
  stays visible with long names, long endpoints and translated font
  metrics. Content columns clamp and truncate first.
- Below 860px the table becomes a compact two-row card: content on
  the first row, the action area on a dedicated second row (never
  underneath text).
- Search/filter toolbar and the TESTING CONTROL BAR are separate
  areas; the control bar owns Test selected / Test all / Test untested,
  pause-resume and the recovery actions (retry failed / timed out /
  working, cancel all).
- The detail panel groups Connect + Test now as one primary action row
  and reports Test state, Last test, Ping (measured or explicitly
  estimated), URL, Health, Core and Verification — measured values
  only, never fabricated.
- Diagnostics gains structured log filters (event, errors-only,
  session/subsystem/level) and a related-events causality panel
  (parent/batch/test/config correlation).

### Real public-URL discovery (§7–§11)

- New bounded discovery pipeline:
  DETECT → DISCOVER → INGEST → PARSE → NORMALIZE → DEDUP → VALIDATE →
  staged PERSIST, with staged selection instead of auto-testing every
  discovered node.
- Generic HTTP/HTTPS connector: SSRF-guarded fetching (scheme/port/IP
  validation, DNS-rebinding prevention at dial time, bounded and
  re-validated redirects), body-size caps, timeouts, gzip/deflate,
  ETag / Last-Modified conditional requests, Retry-After, exponential
  backoff, text/binary identification, malformed-content tolerance.
- GitHub adapter with bounded strategies: repository search, code
  search (protocol URI patterns), recursive tree inspection for
  candidate files (.txt/.yaml/.yml/.json/.conf/.list/.sub), raw file
  fetching (never HTML pages), README reference extraction feeding the
  bounded queue, release assets and public gists.
- Rate-limit engineering: per-provider accounting (requests,
  successes, failures, 429s, 403s, remaining, reset, average latency),
  request budgets, pacing, cooldowns with exponential backoff and
  Retry-After honouring. Rate limiting is never a fatal error — the
  engine degrades to cached/local sources.
- Bounded recursion: max depth, max URLs per source, max URLs per run,
  body caps, per-host cooldowns and a total time budget — discovery
  can never become an uncontrolled crawler.
- Source trust & provenance: every candidate retains its full ledger
  (source type, origin repository/path, discovered-from chain, first/
  last seen, HTTP status, content hash/size, fetch latency, parser,
  candidate/valid/duplicate counts). Validated material lands in a
  staging ledger (config/discovered-sources.json) — manually
  configured sources are never displaced, and trust is earned through
  validation/testing, never mixed silently.

## What's new in v0.9.6

v0.9.6 is a complete engineering upgrade: the Windows CI regression
introduced by v0.9.5 is root-caused and fixed at the exact defect
site, and the discovery → testing → ranking → connection chain is
rebuilt around real measurements with provenance discipline.

### The v0.9.5 Windows regression, actually root-caused

v0.9.5 introduced `internal/httpx` (the unified production network
path) and, with it, a Windows-only defect that failed the Windows CI
job on every completed download while the Linux matrix stayed green:
`finalizeCompletedPart` opened the staged `.part` file with a
READ-ONLY handle and then called `Sync()` — on Windows,
`FlushFileBuffers` requires `GENERIC_WRITE`, so every finalization
returned *Access is denied* and surfaced downstream as
`dependency_unavailable: <core>: download` (the download engine's
failure, not a missing dependency). POSIX `fsync` accepts read-only
descriptors, which is exactly why Linux could not reproduce it.

The v0.9.6 finalization (`internal/httpx/finalize.go`):

- opens the `.part` read-write (the access `FlushFileBuffers`
  requires) and syncs it;
- closes the descriptor BEFORE the rename — no goroutine of ours may
  hold the file while it is replaced;
- renames with a bounded, genuinely-appropriate retry for Windows
  sharing violations ONLY (`ERROR_SHARING_VIOLATION`, ~775 ms worst
  case — an antivirus scan, never a masked real error);
- leaves the `.part` fully recoverable on failure and never touches
  an existing destination until the replacement is safe.

The parallel-range path shared the defect (it finalizes through the
same function); it also gained honest fallback telemetry (a failed
parallel attempt rebases the progress monitor onto the preserved
contiguous prefix instead of double-counting aborted workers' bytes)
and an explicitly-checked handle lifecycle in slice concatenation.
Six regression tests guard the finalization semantics, including the
direct test that fails on Windows with the v0.9.5 code.

### Discovery as a real pipeline

`engine/discovery` implements
**DISCOVER → INGEST → PARSE → NORMALIZE → DEDUPLICATE → VALIDATE**
across eight levels, cheapest and most trusted first:

1. cached known nodes (the local store — no network);
2. configured sources (the user's list);
3. trusted public sources (the built-in verified registry);
4. fresh public discovery via smart search;
5. content-derived sources (references inside valid content);
6. recovery discovery (emergency widening);
7. deep discovery (restrictive-network escalation).

Every level is bounded (fetch concurrency, query/repo/probe budgets),
timeout-aware, rate-limit aware, cache-aware, duplicate-resistant and
failure-isolated: a broken source never stops the others. Later
levels are skipped when the pool already satisfies the target.

### Smart node search

Instead of a fixed list being the only entry point, the engine can
SEARCH for public configuration candidates: GitHub repository search
with protocol-appropriate terms, then probing a small bounded set of
conventional raw-file paths, validated by actually parsing the
content (a probe only becomes a source when it yields real
candidates). Discipline: ≤3 API searches per cycle (≤10/min
unauthenticated budget), ≤12 repositories, ≤24 raw-file probes, a
10-minute backoff after any 403/429, 6-hour result caching, 30-day
staleness demotion, and deduplication by repository and URL. GitHub
HTML pages are never scraped — raw endpoints only.

### Source intelligence

Every source carries measured health: availability, parse success,
valid-candidate yield, duplicate rate, latency, freshness, recent
failures. Healthy sources are fetched first; failing sources enter an
exponential backoff capped at six hours — one bad night never becomes
a blacklist. Health persists in `discovery-health.json`.

### Two first-class test modes

**Ping** measures real endpoint latency over repeated TCP handshake
samples (min/median/avg/max, jitter, packet loss, timeout counts —
the honest user-space substitute for ICMP, which needs raw sockets).
**URL** measures actual HTTP connectivity THROUGH the candidate's
tunnel with a full phase breakdown (DNS, connect, TLS, TTFB, total,
status, response size). Both are user-selectable alongside Protocol
Handshake and Full Connectivity (`ping | url | ping_url | handshake |
full`), with configurable samples, timeout, test URL and candidate
cap. Defaults stay safe and lightweight.

### Ranking by real measurements

Nothing collapses into one unexplained number: PingScore, URLScore,
StabilityScore, SuccessScore, FreshnessScore, SourceScore,
CompatibilityScore and OverallScore are separate, and every latency
number carries its provenance — **measured, estimated, unavailable or
stale**. Nine sort modes (Best Overall, Lowest Ping, Lowest Median
Ping, Lowest Jitter, Lowest Packet Loss, Best URL Response, Highest
Success Rate, Most Stable, Recently Verified). A ping-sorted list
never displays an estimated or source-provided number as a measured
ping, and a 20 ms node with failed connectivity cannot outrank a
75 ms node that consistently provides working Internet.

### Verified connections and racing

A tunnel whose core process started is not yet proven usable:
FreeIran now VERIFIES usable connectivity with a real HTTP request
through the active tunnel after connecting, classifies failures on
evidence (timeout / refused / reset / TLS / HTTP status / proxy
handshake / core), and feeds the class to the bounded recovery
policy. When enabled, the top 2–4 candidates are raced in parallel —
the first VERIFIED-USABLE connection wins and losing attempts are
cancelled cleanly.

### Environment intelligence

Evidence-based signals — direct connectivity, DNS failure, HTTP
failure, TLS failure against independent endpoints, repeated
timeouts, captive portal, configured proxy, unstable latency,
restricted access — drive discovery strategy (a restrictive
environment escalates to deep discovery). No censorship-certainty
claims; no QUIC-detection claims (the standard library ships no QUIC
client — QUIC-family transports belong to the protocol cores).

### The adaptive start flow

**START → DETECT → DISCOVER → TEST → RANK → CONNECT → VERIFY →
MONITOR**: one button runs the whole chain with real stage progress
and measured durations. The user does not need to understand every
protocol; advanced controls remain, and manual selection always
overrides automatic selection.

### SHEYTAN digital-system identity

FreeIran is now identified as **a SHEYTAN Digital System** — in the
About surface, the UI status bar, the Windows version resources, the
installer metadata and this README. The product name stays FreeIran;
branding never interferes with usability.

### Verification

Full matrix re-run on the upgraded tree: gofmt/vet clean, the
fake-core test and race matrix (engine/system/internal), the frontend
typecheck + 51 vitest tests + production build, the windows/amd64
desktop cross-compile, and the benchmark smoke suite including the
httpx single-stream vs parallel-range comparison. The fake test core
gained a SOCKS-relay mode so verification and racing are exercised
end-to-end against the deterministic fixture.

---

## What's new in v0.9.5

v0.9.5 is a repository-integrity release: it makes the repository
match what v0.9.4 documented, removes the stale artifacts that
accumulated across the 0.9.1-0.9.4 packaging sessions, and re-runs
the full verification matrix end to end.

### The v0.9.4 regression, actually fixed

- v0.9.4's release notes claimed the obsolete per-user path
  resolvers were removed; they were still shipped, and re-declaring
  `DefaultBaseDir`/`CacheBaseDir`/`portableRoot`/`PortableMode`
  alongside `workspace.go` broke `go vet`, `go build` and
  every test job. The three files (`system/paths_unix.go`,
  `system/paths_windows.go`, `system/portable.go`) are now really
  deleted, `system/` compiles for every build-tag combination, and
  the existing `TestWorkspacePathAuthoritySingleSource` guard stays
  as the regression tripwire.

### Repository hygiene

- **Stale generated bundles removed.** `cmd/freeiran/frontend/dist`
  (the Wails embed target) had accumulated 10 hashed bundles from
  older builds next to the live set; the embed directory now contains
  exactly the assets produced by the current frontend build
  (`copy-dist.mjs` cleans before copying, so this cannot re-accumulate).
- **Session manifests removed.** The repository root carried three
  per-release working documents (`REPLACEMENT_MANIFEST.md`,
  `Release-Manifest.md`, `Updated-Files.md`) describing 0.9.1/0.9.2
  packaging sessions as if current. They are deleted; release
  history lives in git history and the release notes, and the
  engineering worklog remains the single handoff document.
- **Version sources synced.** `VERSION`, `internal/version`,
  `frontend/package.json`, `frontend/package-lock.json` (whose root
  entry had drifted to 0.9.1), the Inno Setup installer script and
  this README all say 0.9.5. The CI version gate keeps enforcing the
  triple agreement on every push.
- **Makefile clean-target typo** (`FreeIron-linux-amd64`) fixed.

### The unified production network path (v0.9.5 follow-up)

Core installation and source refresh previously suffered two failure
classes: GitHub release-resolution failures (a rate-limited 429/403
body decoded into an "empty release" and surfaced as a misleading
"no asset for platform" error) and download stalls/timeouts (one
shared `http.Client{Timeout: 60s}` was used for BOTH the release API
and multi-megabyte archive bodies, so any transfer needing more than
60 seconds aborted mid-body with no resume).

These are fixed by consolidating every artifact transfer onto ONE
policy engine, `internal/httpx`:

- **Control plane** (release API, app-update feed, source fetches):
  bounded per-attempt timeouts, connection/TLS/header timeouts,
  retries for transient network errors and 429/502/503/504 with
  exponential backoff + jitter, Retry-After honoured (capped),
  keep-alive connection reuse, bounded response bodies, cancellation.
- **Data plane** (core archives): streaming downloads straight to
  `.part` files with byte/speed/ETA telemetry, a no-progress stall
  watchdog instead of a total timeout, HTTP Range resume with
  Content-Range validation, safe restart when resume is unsupported
  or lying, and bounded memory. Adaptive 2–4 ranged workers exist as
  an opt-in optimization for sufficiently large files (default
  threshold 64 MiB — every current core archive stays single-stream;
  measured on loopback: 64 MiB in 155.9 ms single vs 133.6 ms with 4
  workers, a mechanism-level comparison, not a network claim).
- **Release metadata cache**: persisted per release feed with ETag
  conditional requests (If-None-Match → 304 revalidation), halving
  unauthenticated API-budget consumption.
- **Transactional installs**: resolve → select asset (repo, platform,
  architecture, size, digest identity verified — never the filename
  alone) → download → checksum (API digest, then .dgst sidecar) →
  unpack → validate → smoke test the STAGED binary → atomic
  activation. The previous working core stays active until the new
  one passes everything; concurrent installs of the same core share
  one download through a per-core singleflight.
- **Source refresh isolation**: the configuration-source fetcher runs
  on the same policy (per-source timeout, retry/backoff, size cap),
  parser panics fail only their own source, and the default registry
  carries the three mandated sources exactly once as raw endpoints.
- **One progress language**: resolving → downloading → verifying →
  unpacking → validating → activating → complete, with real
  bytes/speed/ETA/retries/resumed telemetry and actionable errors
  ("release API unavailable", "download stalled", "checksum
  mismatch", "wrong platform asset", …). Routine `memory_adjust`
  INFO logging was removed at its producer (warnings and pressure
  events remain).

### Verification re-run

The full matrix was executed on the cleaned tree: gofmt/vet/build,
the fake-core test and race matrix, the testqueue race stress
(`-race -count=10`), storage/pressure stress (`-race -count=5` over
`store`, `chunks`, `pipeline`, `mempressure`, `cache`), the pinned
real-core smoke suites (V2Ray 5.53.0, Xray 26.3.27, sing-box 1.14.0
— checksum-verified official binaries), the native C++ tests plus the
`native_accel` bridge build and benchmarks, and the frontend
typecheck/test/production build.

## What's new in v0.9.4

### Faster startup, visible progress

- **Unified boot lifecycle.** The startup critical path is now
  explicitly `boot → workspace_ready → store_metadata_ready →
  services_ready`: the service surface is ready before any expensive
  work runs, and everything else (storage verification, cache
  warm-up, core discovery, source refresh) continues in the
  background without ever blocking the UI.
- **Real boot progress.** The UI renders a determinate progress bar
  from the backend's actual boot phases — with per-phase millisecond
  telemetry — instead of a generic spinner. The frontend reports its
  first usable frame back, so "time to usable UI" is measured, not
  guessed.
- **One loading language.** A single policy governs every loading
  surface: instant operations render nothing, fast operations avoid
  visual disturbance, slow operations get a calm skeleton; progress
  indicators are always REAL (boot phases, core install stages,
  connection steps). Animations use only opacity/transform and
  respect `prefers-reduced-motion`.

### Faster everyday use

- **Ranked-candidate snapshot.** The configuration ranking view is
  served from a cached snapshot (45 s TTL + store-size identity),
  invalidated the moment a test result or ingestion changes the
  data. Navigating no longer rescans thousands of entries; automatic
  connection still reads the store directly for correctness.

### Installation and updates

- **Standard Windows installer.** `FreeIran-Setup-vX.Y.Z-windows-amd64.exe`
  built on Inno Setup: Start Menu shortcut, optional desktop
  shortcut, proper uninstall registration, running instance closed
  before replacement, version metadata and icon included. Installed
  deployments store their workspace in the per-user application-data
  directory (`installed.marker` model) — user data survives updates
  and uninstallation.
- **Checksum-verified releases.** Every release artifact (both
  deployment ZIPs and the installer) publishes a SHA-256 sidecar.
- **Application-update check.** A new update checker resolves the
  official release feed, selects the exact platform asset, compares
  versions honestly and surfaces the checksum URL — the same
  artifact-pipeline shape the managed-core updater already uses
  (download/stage/activate/rollback build on its verified
  primitives).

### Engineering

- Fixed the v0.9.3 workspace regression: the obsolete per-user path
  implementations (`paths_unix.go`, `paths_windows.go`) are removed;
  `workspace.go` is the single path authority again.
- CI now fails on version drift between `VERSION`,
  `package.json` and `internal/version` on every push.

### Final stabilization (0.9.4)

- **Workspace authority enforced at source level.** The duplicate
  `DefaultBaseDir`/`CacheBaseDir`/`portableRoot` declarations that
  broke `go vet` are gone (the two obsolete platform files are
  deleted), and a package test now scans `system` sources to fail any
  future build-tag-hidden duplicate of a workspace-path symbol.
- **Race repairs.** The scheduler published `lastErr` outside its
  mutex while `Status()` read it under the lock — UI status polling
  raced the worker loop; the value is now published inside the same
  critical section as `lastRun`/`runCount`.
- **Constant-memory WAL byte accounting.** The memory-pressure
  sampler read `WALBytes` from a full journal-directory walk
  (`ReadDir` + per-file stat) every 2 s for the life of the process;
  the count is now maintained exactly at open/append/roll/checkpoint
  and served in O(1), with a regression test comparing it against the
  real on-disk bytes across rolls and both checkpoint flavors.
- **Pagination without wasted decodes.** `ListConfigs` decoded every
  skipped record (and kept walking the store after the page was
  full); the walk now stops at the page boundary and skip-range
  records are never unmarshalled — deep pages and page 0 of large
  stores stop paying O(store) JSON work.
- **Honest memory snapshot.** `mempressure.Controller.Snapshot()`
  re-sampled with a stop-the-world `ReadMemStats` despite its
  documentation; it now serves the sampler's last snapshot. The log
  filter stopped allocating a map per filtered entry.
- **Frontend call discipline.** The initial data load runs once per
  backend lifetime instead of on every state broadcast, a zero-result
  search is no longer clobbered by a re-fetch, the queue-stats poll
  backs off when idle, the live-log poll cannot stack overlapping
  requests, unchanged log rows skip re-render, and event-injected
  state preserves object identity when content is unchanged.
- **CI truthfulness.** The native-bridge benchmark step's
  `-bench=Native` filter matched zero benchmarks; it now runs them.

## What's new in v0.9.3

### The autonomous connection engine

FreeIran now behaves like an automatic connectivity engine rather
than a configuration manager:

- **Connect means connect.** Pressing CONNECT selects the best viable
  candidate from real test history, validates it, chooses a
  compatible core, waits for readiness and verifies the tunnel — no
  manual configuration picking, no manual core selection.
- **Deterministic ranking.** Every stored configuration is scored
  from its actual observations (success rate, median latency,
  stability, timeout frequency, test age, source reliability,
  core compatibility) into BEST / GOOD / UNSTABLE / DEAD / UNKNOWN
  classes, each score carrying a human-readable explanation
  ("31 ms median · 96% recent success · tested 4 min ago").
- **Automatic recovery.** When the active connection dies, the
  recovery supervisor classifies the failure, skips recently failed
  candidates (failure memory + cooldown), switches to the next viable
  one and verifies the result — bounded to 3 attempts per episode
  with growing backoff, 2 episodes per 30-minute window. No infinite
  retries, no reconnect loops, no dead-core launches. An explicit
  opt-out lives in Settings → Reliability.
- **Bounded test history.** Each configuration keeps its last 12 test
  outcomes (workspace-persisted, credential-free) — the data the
  ranking and recovery decisions run on.

### Production repair

- **Single path authority.** The v0.9.2 workspace migration left the
  old `paths_unix.go`/`paths_windows.go` resolvers behind, duplicating
  `DefaultBaseDir`/`CacheBaseDir`/`portableRoot` and breaking every CI
  job. One workspace resolver (`system/workspace.go`) now owns the
  persistence model; regression tests guard it.
- **Worker-pool race fixed.** A shrink of the test queue's dynamic
  pool could retire every worker at once (stale-count overshoot),
  leaving queued tests undrained forever. Retirement is now an atomic
  slot claim; a dedicated regression test reproduces the original
  failure under `-race`.

## What's new in v0.9.1

### A testing workspace you can read

The Configurations page had a structural layout bug: the configuration
row grid declared seven columns while every row rendered eight
elements, so the per-row **Test** button wrapped onto an invisible
second grid row and overlapped the row below at common window sizes.
v0.9.1 rebuilds the row as an explicit eight-column grid (select,
protocol, endpoint, transport, latency, health, source, action) with
the sticky header aligned one-to-one, and reorganizes the workspace:

- **Primary actions** (Test selected / Test all / Test untested) stay
  visible; **secondary actions** (Retest failed, Retest working,
  Cancel all) moved into a compact overflow menu so the toolbar can
  never overflow into the list.
- The **test-queue panel** is now a proper progress surface: a real
  progress bar with counts, large **avg / fastest / slowest ping**
  tiles, and quiet counters for queued, active, passed, failed and
  cancelled.
- The configuration list fills the available height exactly — local
  scrolling only, no page-wide scrollbars, no clipped controls at
  960 / 1100 / 1280 / 1440 px or with the sidebar collapsed.
- The v0.9.0 style appendix referenced a set of design tokens that
  were never defined (`--radius-md`, `--surface-1`, `--surface-2`,
  `--font-mono`, `--danger`, `--warning`), silently degrading callouts,
  queue panel and badges. The tokens are now defined once, and the
  duplicated `.card-title` / `.page-header` / badge overrides that made
  pages drift apart were removed — every page shares one header,
  spacing and control rhythm.

### A real application icon

FreeIran ships a proper brand icon (`assets/freeiran-icon.svg` is the
canonical source; regenerate derivatives with `scripts/genicon.py`):

- **Windows** — a committed resource object (`cmd/freeiran/*.syso`,
  built from the ICO with version info and a DPI-aware manifest) is
  linked into every build, so the executable, titlebar and taskbar all
  carry the identity. The release build also links with
  `-H=windowsgui`, killing the console window for good. CI validates
  the icon assets before every release, so it cannot silently
  disappear.
- **Linux** — the window icon is embedded in the binary
  (`internal/appicon`) and applied through the GTK window options.

### Linux amd64 is a first-class release target

The release workflow now builds and publishes **both**
`FreeIran-windows-amd64.zip` and `FreeIran-linux-amd64.zip`, each a
complete portable deployment directory (binary, README, LICENSE,
VERSION, config/, data/, logs/, cache/, cores/, runtime/, docs/,
deployment/ metadata). The Linux build uses the `gtk3` (WebKit2GTK
4.1) frontend for maximum compatibility. **Checksum `.sha256` files
are gone** — the release contains only the two platform ZIPs.

### Developer options, honestly wired

Settings gained a clearly separated **Developer** section (plus a
reorganized General / Connection / Testing / Appearance / Diagnostics
structure). Every control is wired to real engine behaviour — nothing
decorative:

- verbose diagnostics (enriches the diagnostic report with runtime
  detail),
- test-queue worker override (beats the adaptive memory booster),
- network-test timeout override (applied live to Network Diagnostics),
- force Go fallback for native acceleration (same state as
  `FREEIRAN_NATIVE=off`),
- clear caches, open data/logs directory, live queue internals and
  build identity (version, commit, Go version, portable mode).

### Errors that explain themselves

Connection failures now follow **what happened + why + what to do
next**: a friendly explanation first, a targeted suggestion when the
failure kind is recognizable (missing core, timeout, auth rejection,
DNS, TUN elevation, port conflict), and the raw technical text behind
an expandable disclosure.

---

## What's new in v0.9.0

### Core loading, fixed end to end

v0.9.0 closes the last gaps in the protocol-core lifecycle. A managed
core is never considered "installed" merely because a file exists:
the full pipeline `discover → verify → version → config validation →
launch → readiness → health → usable` runs before the UI reports
**Ready**, and every failed state carries a human-readable reason plus
a technical-details expander. Managed installs now integrate with
runtime discovery — the three `bin/` directories are core-locator
inputs, so an installed core is immediately usable by Connect and the
tester without any restart.

Three real production bugs fell out of that audit and are fixed with
regression tests:

- **`Repair` deadlocked on every call** — it held the per-core mutex
  and then re-acquired the same non-reentrant mutex inside
  Rollback/HealthCheck/Install.
- **Xray and V2Ray downloads never matched the platform** — asset
  selection required the platform hint inside the asset name, which
  legacy naming (`Xray-windows-64.zip`) does not contain.
- **Install always failed at unpack** — downloads were staged as
  `asset.bin`, so the archive-format switch never matched; and zip
  extraction left the executable non-executable before the version
  probe ran on Unix.

### The CMD window bug is dead

Every remaining child-process launch site (Managed Core Manager
version probes, config validation, smoke tests) now sets
`CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | HideWindow`. Windows CI
runs a regression test asserting the attributes. The smoke test's
polite shutdown uses Windows-safe semantics instead of the unsupported
`os.Interrupt` signal that previously marked every healthy install
**Broken**.

### Internet / Network Diagnostics

A dedicated `engine/netcheck` package runs multiple independent
probes (local links, three DNS resolvers, three TCP endpoints, three
HTTPS targets, latency measurement) and classifies the result into the
seven states the specification requires — from "internet unavailable"
through "DNS failing" to "internet reachable only through the
configured proxy". The new **Network** tab exposes a manual,
cancellable **Check connection** action; when a session is connected
the proxy path is probed too, so "core connected but external
connectivity failing" is distinguishable from a dead internet line.

### Real ping, honest results

Configuration tests now measure the actual round-trip through the
generated tunnel (SOCKS5 CONNECT + a 204 fetch via the new
`engine/socks5` package) instead of reporting the core's local startup
time. Every result records working/failed, ping in milliseconds,
quality band (excellent / good / acceptable / slow), test duration,
protocol, backend, endpoint and timestamp — and **persists to the
store**, so test-queue results survive and display without retesting.
The Configurations workspace gains server-side status filters, sorting
by ping/protocol/recency, multi-select and bulk test actions (all /
untested / failed / working / selected) with a live queue progress
panel including average, fastest and slowest measured latency.

### Modern tabs

The UI is now eight focused tabs — Dashboard (with a first-launch
onboarding checklist), Configurations, Sources, **Cores** (one-click
Install → Verify → Start using), Connection, **Network**, Diagnostics
and Settings (with a sanitized copy/export diagnostic report). Install
progress (download bytes, verification, smoke test) streams to the UI
as `freeiran:coreprogress` events.

### Complete deployment package

The release ZIP is no longer a bare exe. `FreeIran-windows-amd64.zip`
contains a full portable deployment directory — `FreeIran.exe`,
README, LICENSE, VERSION, `config/`, `data/`, `logs/`, `cache/`,
`cores/`, `runtime/`, `docs/`, `deployment/deployment.json` metadata
and a `portable.marker` the application detects on startup to keep all
state inside the deployment tree. The release pipeline validates the
package contents (executable present, directories present, metadata
consistent) before publishing and fails the release otherwise.

---

## What's new in v0.8.0

### Windows process supervision, repaired and hardened

The v0.7.0 Windows CI failed on every process-launching test with
`system/start: environment: bind kill-on-close job`. The root cause was
a Win32 return-value protocol bug, not an environment problem:
`SetInformationJobObject` returns a BOOL, but the v0.7 launcher judged
success from the thread's stale `GetLastError()` value — which
BOOL-returning APIs do not reset on success. On GitHub Actions runners
the stale errno is nonzero, so a **successful** job configuration was
misread as a failure and the child was killed. v0.8 validates every
Win32 call by its actual return value and consults `GetLastError()`
only as a diagnostic on genuine failure.

Supervision is now a three-tier strategy that stays deterministic in
restricted environments: direct job assignment (Windows 8+ nests job
hierarchies, so runner jobs are no obstacle) → a relaunch with
`CREATE_BREAKAWAY_FROM_JOB` when assignment is access-denied → a
supervised fallback with Toolhelp32 process-tree termination that
keeps the no-orphan guarantee and *surfaces* the degradation through
diagnostics instead of failing silently. `cmd.exe` and other test
binaries resolve through `%COMSPEC%` / `%SystemRoot%\System32` with
on-disk validation — never the working directory or inherited PATH —
while protocol-core paths keep their strict explicit validation.

### Deterministic lifecycle state machine

Managed processes now expose `running → stopping → stopped / exited /
cancelled` with distinct classifications for launch failure, natural
exit, cancellation and environment degradation; `Stop` is
idempotent, race-free and synchronizing for concurrent callers;
stdout/stderr capture is genuinely concurrency-safe with a bounded
pipe-drain deadline; and descendants are reaped on **every** exit
path (a grandchild that ignores the polite signal cannot survive).
The lifecycle battery (15 tests, `-race` clean) covers the
no-visible-console, capture, cancellation, forced-termination,
repeated/concurrent Stop, startup-failure, job-binding-failure and
grandchild-cannot-survive guarantees.

### Memory Booster 2.0

The v0.7 `engine/mempressure` + `engine/booster` libraries are now
actually wired into the running application as one adaptive
controller: it samples the real subsystems (cache layers, test-queue
memory, store memtable + WAL bytes, Go heap, RSS, GC pressure) every
two seconds and adapts worker concurrency, queue depth and cache
targets — with hard ceilings, floors and hysteresis, gradual
recovery, cache shedding and a GC hint at critical pressure. Every
adjustment is logged and visible in the new Diagnostics memory panel;
nothing degrades silently.

### Desktop wiring audit

The v0.6 Core Manager, Test Queue and Tunnel services existed in the
Go backend but were never registered with the Wails runtime, so no
frontend action could reach them. v0.8 registers all three, ships
bindings for them, and adds the system-integration (System Proxy /
TUN) controls plus live memory and test-queue panels to the UI.

---

## What's new in v0.6.0

This release turns FreeIran from a configuration database with core
adapters into a **genuinely usable Windows VPN/proxy client**.

### Managed Core Manager

Xray, V2Ray and sing-box are now first-class managed dependencies.
FreeIran installs, verifies, updates and rolls them back through a
single subsystem:

- **Discover + install** from the official upstream GitHub Releases
  (XTLS/Xray-core, v2fly/v2ray-core, SagerNet/sing-box). No third-
  party mirrors, no bundled binaries.
- **Verify** every downloaded archive against the published SHA-256
  digest before activation. A binary that fails verification is never
  activated.
- **Atomic activation**: a downloaded binary lands in a staging
  directory, is smoke-tested, and only then renamed into place.
  The previous healthy binary is retained as the rollback target.
- **Health checks**: each core is exercised through a minimal SOCKS
  inbound + listener probe; the result is surfaced in the UI as
  `Ready / Broken / Update available`.
- **Stable + prerelease channels** are separate. The user never
  receives a prerelease unless they explicitly opt in.
- **Repair** button: tries rollback first, then a fresh install.
- **Check for updates** in v2rayN-style: current version, latest
  version, release URL, changelog URL, asset size, install button.

### Windows process launch with no console window

FreeIran and every protocol-core it spawns (Xray, V2Ray, sing-box)
now launches **without a visible CMD/console window**, via
`CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS`.
Each child is bound to a kill-on-close Windows job object so an
abnormal FreeIran exit reaps every orphan at the kernel level —
**no orphan core remains after exit**.

### Test Queue

Testing moved from sequential blocking operations to a real queue:

- Bounded worker pool with priority (newly discovered configs and
  user-selected tests jump ahead of bulk re-tests)
- Duplicate task suppression by fingerprint
- Backend-aware concurrency limits (a slow V2Ray cannot starve Xray)
- Per-config timeout + global test timeout
- Exponential-backoff retry
- Cancellation by task ID, by source, or globally
- Graceful shutdown
- Live progress statistics (tests/sec, queue depth, active workers,
  per-backend counts)

### Testing modes

`Quick`, `Balanced`, `Deep`, `Re-test failed`, `Test all`, `Test
selected`, `Continuous` — each presets workers / timeout / attempts /
measurements.

### Source Manager

Every default public source gained full metadata (provider, project,
protocol hints, region, format, priority, refresh interval) and is
persisted to a sidecar `sources.json`. Three new high-quality sources
were added:

- **ShadowsocksAggregator/Eternity** — `mahdibland/ShadowsocksAggregator/master/Eternity.txt`
- **MahsaFreeConfig/MTN** — `mahsanet/MahsaFreeConfig/main/mtn/sub_1.txt`
- **ScrapeAndCategorize/Netherlands** — `10ium/ScrapeAndCategorize/main/output_configs/Netherlands.txt`

The collector now supports **conditional requests** (ETag,
Last-Modified), **gzip/deflate** transport and **content-hash
short-circuit**: an unchanged source skips parse + persistence
entirely.

### System Proxy

A proper Windows system-proxy integration through **WinINet's
per-connection options** (not registry edits). The previous proxy
settings are saved before activation and restored on disable. Running
apps are notified through `INTERNET_OPTION_SETTINGS_CHANGED` +
`INTERNET_OPTION_REFRESH` so they pick up the new proxy without a
restart. Modes: `Direct | System Proxy | SOCKS | HTTP`.

### TUN mode

A real Windows TUN interface backed by **Wintun** (the official
maintained driver). FreeIran resolves `wintun.dll` from the managed
cores directory, the executable directory, or the system directory.
Install + Enable require elevation; the TUN interface is torn down on
disable and on application shutdown (kill-switch-safe: a crash takes
the TUN down with it via the job-object binding).

### Capability-driven backend selection

Configuration → backend selection now follows:

`configuration capability → preferred core → installed healthy cores →
priority → health → recent success`

If the selected core fails, the manager records the failure, classifies
it (network / auth / protocol / config / backend_down / timeout /
cancelled / unknown) and tries the next compatible installed core.
Incompatible cores are never retried for the same configuration. The
final error is explainable in the UI.

### Performance: Speed Booster

An adaptive controller monitors CPU pressure, memory pressure, queue
backlog, core startup failures and network errors. When the system is
healthy and the queue is deep it raises concurrency; when pressure
rises it throttles back. Correctness is never sacrificed for raw
throughput — a configuration that fails is removed from the active
pool, not retried in a tight loop.

### Documentation

Every Markdown file was rewritten to describe the real architecture.
Stale "v0.5.0" claims were removed. Per-release change manifests live
in the git history and release notes; the engineering worklog
(`worklog.md`) remains the single handoff document.

---

## Vision

> Continuously find publicly available configurations, test them, keep
> the ones that work, archive the ones that fail, remove duplicates,
> and make the working pool immediately usable from a lightweight
> client.

FreeIran is intended for environments where ordinary Internet
connectivity can be heavily restricted, including Iran. It is local-
first: no account, no central backend, no cloud service, no remote
telemetry. All network activity relates to fetching public
configuration sources, downloading official core binaries from their
upstream release pages, or testing configurations.

---

## Architecture Overview

```text
                       FREEIRAN
                          │
                    WAILS v3 APP
                          │
                 TypeScript UI layer        (frontend/)
                          │
                   Generated bindings        (frontend/bindings)
                          │
                    Go application
                     orchestration           (engine/app)
                          │
        ┌─────────────────┼─────────────────┐
        │                 │                 │
    Data Engine       Core Manager      System Engine
  (engine/store,     (engine/coremgr:   (system/)
   engine/chunks,     install / update /    │
   engine/cache)      rollback / health)   C++ native layer
        │                 │                (native/, engine/native)
   Ingestion pipeline    │                optional, with Go fallback
   (engine/pipeline,     │
    engine/source)    Connection Manager
        │            (engine/connection: state machine, fallback)
   FETCH → PARSE →        │
   NORMALIZE →       ┌────┴────┐
   VALIDATE →        │         │
   DEDUP → PERSIST   │         │
                     │         │
              Protocol cores (engine/core)
              ┌──────────────┬──────────────┐
              │ xray         │ v2ray        │ sing-box
              └──────────────┴──────────────┘
                           │
              Test Queue (engine/testqueue)
              bounded workers + priority + cancellation
                           │
              Tunnel (engine/tunnel)
              System Proxy (WinINet) | TUN (Wintun)
```

- **Go** is the primary orchestration/system language: lifecycle,
  scheduling, concurrency, storage, sources, testing, the managed
  protocol-core lifecycle, the test queue, the tunnel modes and the
  API exposed to the UI.
- **C++** provides a small, measurable acceleration layer (batch
  hashing, CRC-32 checksums, URL scanning) behind a stable C ABI,
  with bit-exact pure-Go fallbacks so correctness never depends on it.
- **TypeScript (React + Vite)** is the UI layer: application state,
  configuration browsing with virtualized lists, source management,
  diagnostics, core manager, test queue, tunnel mode selector. The
  UI talks to Go exclusively through generated Wails v3 bindings and
  events — there is no localhost HTTP API.

Details: [docs/architecture.md](docs/architecture.md),
[docs/storage-format.md](docs/storage-format.md),
[docs/performance.md](docs/performance.md),
[docs/ci.md](docs/ci.md), [docs/security.md](docs/security.md),
[docs/development.md](docs/development.md).

---

## Building

Requirements: Go **1.26.8** (the go.mod toolchain — see
[docs/development.md](docs/development.md) for the toolchain policy),
Node.js 22+, npm, a C++17 compiler (optional — only for the native
acceleration layer), make (optional).

```bash
# 1. Build the frontend and stage it for the Go embed
cd frontend
npm ci
npm run build:embed     # builds and copies dist into cmd/freeiran
cd ..

# 2. Build the desktop application (Windows amd64 primary target)
#    from a Windows machine:
go build -trimpath \
  -ldflags "-s -w -X github.com/Parsaetak/FreeIran/internal/version.Version=$(cat VERSION | tr -d '[:space:]')" \
  -o FreeIran-windows-amd64.exe ./cmd/freeiran

# From Linux/macOS you can cross-compile the Windows binary:
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -o FreeIran-windows-amd64.exe ./cmd/freeiran
```

### Optional C++ acceleration

```bash
make -C native            # builds libfreeiran_native + runs its tests
CGO_ENABLED=1 go build -tags native_accel -o FreeIran.exe ./cmd/freeiran
```

Without the `native_accel` tag the binary uses the pure-Go
implementations, which are tested to produce identical results.
`FREEIRAN_NATIVE=off` disables the native path at runtime.

### Development

```bash
go test ./engine/... ./system/... ./internal/...   # Go tests
go test -race ./engine/...                          # race detector
cd frontend && npm test                             # frontend tests
cd frontend && npm run dev                          # Vite dev server
```

A committed placeholder at `cmd/freeiran/frontend/dist` keeps `go build
./cmd/freeiran` working before any frontend build; the real UI is
staged by `npm run build:embed` (used by CI).

---

## Repository Structure

```text
FreeIran/
├── cmd/freeiran/          Desktop application entrypoint (Wails v3)
├── engine/                Shared Go engine
│   ├── app/               Application orchestration + UI service surface
│   ├── cache/             Bounded LRU cache layers
│   ├── chunks/            Chunking subsystem (FIRC format)
│   ├── config/            Universal configuration model + fingerprints
│   ├── core/              Protocol-core execution boundary (adapters)
│   ├── coremgr/           [v0.6] Managed Core Manager (install/update/rollback)
│   ├── connection/        Connection manager + state machine + failover
│   ├── errors/            Structured, classified errors
│   ├── metrics/           Local performance counters
│   ├── native/            Go↔C++ bridge (pure-Go fallbacks)
│   ├── netcheck/          Connectivity diagnostics + [v0.9.8.1] Internet tools engine
│   ├── parser/            Multi-format configuration parser
│   ├── pipeline/          Streaming ingestion pipeline (worker pools)
│   ├── provider/          [v0.9.8.1] Provider architecture: Tor, Psiphon, cores
│   ├── scheduler/         Interval scheduler (skip-if-busy, jitter)
│   ├── source/            Source model + HTTP fetcher + collector
│   ├── store/             Chunked persistence: WAL, memtables, compaction
│   ├── tester/            Probe interface + TCP / core probes + [v0.9.8.1] latency semantics
│   ├── testqueue/         [v0.6] Bounded-worker test queue (priority, retry, cancel)
│   └── tunnel/            [v0.6] System Proxy (WinINet) + TUN (Wintun)
├── frontend/              TypeScript UI (Vite + React + zustand)
├── native/                C++ acceleration layer (C ABI, no deps)
├── system/                System engine: paths, processes, network, platform
├── internal/version/      Single source of truth for versioning
├── .github/workflows/     CI, release and security pipelines
├── docs/                  Architecture, storage, performance, CI, security, dev
├── VERSION                Application version (0.9.8.5)
└── worklog.md             Engineering worklog
```

---

## Data Migration

Existing users of the v0.1 JSON database keep their data:

1. On first run (or via *Diagnostics → Migrate*), the legacy JSON file
   is detected and validated.
2. Records are streamed into the chunked store in bounded batches
   with their original fingerprints preserved.
3. The import is verified record-by-record through the full disk
   path in a second streaming pass.
4. The legacy file is renamed to `<original>.migrated` — never
   deleted.

Migration is idempotent: running it again is a no-op, and an
interrupted run is safely re-runnable.

---

## Security & Privacy

- Downloaded configuration data is untrusted input: it is parsed,
  normalized, validated and deduplicated before storage or testing.
- **Protocol-core binaries are downloaded only from the official
  upstream GitHub Releases** of XTLS/Xray-core, v2fly/v2ray-core and
  SagerNet/sing-box. Every archive's SHA-256 is verified against the
  published digest before activation.
- No arbitrary scripts are executed; no certificates are installed; no
  credentials are written to logs (log paths pass through redaction).
- The app is local-first: no account, no cloud, no browsing history,
  no remote telemetry. Metrics are local diagnostics.
- CI runs `govulncheck` and `gitleaks` on every push; see
  [docs/security.md](docs/security.md).

---

## Roadmap

- [x] v0.1 — Engine foundation (config model, parser, dedup, JSON store)
- [x] v0.2 — Architecture upgrade: chunked store, streaming pipeline,
      caching, native acceleration layer, Wails v3 desktop shell,
      TypeScript UI, CI/CD, migration
- [x] v0.3 — Storage lifecycle rework (Windows-safe resource ownership,
      segmented WAL, background flush), streaming migration, toolchain
      policy (go1.26.8), CI/security modernization, deep diagnostics
- [x] v0.4 — Protocol core integration: Xray + V2Ray (V2Fly) +
      sing-box as real backends, deterministic selection, connection
      state machine, core-based testing, real-binary CI verification
- [x] v0.5 — Windows lifecycle repair, deterministic fake-core test
      harness, persistent runtime logging with redaction, professional
      UI, settings persistence
- [x] v0.6 — **Managed Core Manager (install/update/rollback),
      no-console process launch, Test Queue with bounded workers,
      System Proxy (WinINet) + TUN (Wintun), Speed Booster, expanded
      sources with metadata, capability-driven failover**
- [ ] v0.7 — UI polish, source reliability dashboards, config grouping
- [ ] v1.0 — Stable releases, security review, reproducible builds

---

## Attribution & License

**Architect / Project Originator:** Parsa Tak / SHEYTAN
**Repository:** https://github.com/Parsaetak/FreeIran

License: see [LICENSE](LICENSE).
