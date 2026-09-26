# FreeIran Roadmap

## FreeIran — Autonomous Local Connectivity Engine

FreeIran is a local Windows connectivity engine that discovers, tests, ranks, connects, monitors and recovers proxy/VPN connectivity through managed protocol cores and first-class providers.

The product pipeline is:

**discover → ingest → parse → deduplicate → test → score → select → connect → verify → monitor → recover → learn**

The application should feel like a mature desktop connectivity client rather than a collection of independent tools.

---

# Roadmap ladder (v0.11.0)

The roadmap is a four-phase ladder. Each phase must be TRUE before the
next one starts; the acceptance standard is the user-visible pipeline
below, not internal test counts.

### v0.11.0 current-release status (factual)

v0.11.0 is a CI-architecture + transport-resilience-foundation
release. The Windows CI bottleneck (the full Go matrix re-executed on
a Windows runner under `-p 1`) is replaced by a measured two-layer
proof: Layer A compiles the COMPLETE Windows test surface
(`go test -run '^$' ./...`), Layer B executes the Windows-SENSITIVE
behavior (tunnel/system/store/httpx full packages + targeted
coremgr/netcheck/app/connection patterns), Layer C keeps the repeated
WinINet/recovery battery (`-count=5 -p 1`), each layer with explicit
bounded timeouts (10m/10m/4-8m/10m). Platform-neutral behavioral
correctness remains the Linux job's authority (full suite + `-race`).
No Windows-specific coverage was removed: every package with a
documented Windows defect history still executes its Windows-relevant
tests, and the whole surface still compiles.

The resilience foundation: a nine-class failure taxonomy derived from
observations (dns/tcp/tls/handshake/listener/verify/reset/timeout/
transport) recorded per configuration and per history observation; a
bounded freshness/evidence tuple (last_verified_at, last_failure_at,
recent_failure_class, verification_age); and transport agility
WITHOUT a second failover engine — a trailing streak of ≥2
protocol-specific failures (tls/handshake/transport) demotes a
candidate in both ranking surfaces so Quick Connect/recovery/
discovery naturally prefer a different already-supported transport.

ECH (Encrypted Client Hello): modeled with four dedicated fields
(ech_enabled/ech_config/ech_config_path/ech_query_server_name →
sing-box tls.ech), capability-routed to sing-box ONLY (verified
against the pinned v1.14.0: PEM "ECH CONFIGS" form, REALITY conflict,
query_server_name DNS discovery; Xray's field is not
content-validated at check level and V2Ray 5.53.0 ignores it —
neither is claimed). Evidence is SCHEMA-LEVEL (check + startup, all
three shapes ride the real-binary smoke suite); no live ECH
negotination is claimed anywhere. See docs/protocols.md for the full
evidence table.

Also in this release: the wails3 bindings are fully machine-generated
again (the v0.9.11 hand-maintained files were removed after
regeneration with the pinned CLI; reproducibility verified; the
models are pinned field-for-field to the Go structs), and
docs/android.md documents the implementation-ready Android
architecture note (plan only — no Android product, TUN stays
experimental/disabled).

Evidence actually executed for v0.11.0: full Linux Go suite,
`-race` full suite, frontend typecheck/tests/production
build/embed validation, real-core verification with the three
SHA-256-pinned cores (v2ray 5.53.0, xray 26.3.27, sing-box 1.14.0 —
including the new ECH smoke shapes: inline PEM config, config_path
file, query_server_name), the gosecscan/Gitleaks/govulncheck/go-vet
security battery, Windows-amd64 cross-build with PE
windowsgui-subsystem verification from Linux, and a Windows
compile-surface proof (all test binaries compile for GOOS=windows)
from Linux.

NOT yet executed at release: the Windows-NATIVE CI gate (the new
two-layer matrix + battery + runtime smoke + desktop build on a
Windows runner) — it runs when this tree is pushed and must pass
before the release is described as Windows-verified. No Internet
verification over a live tunnel was performed; ECH support is
schema-level evidence, not live-negotiation evidence.

### v0.10.5 current-release status (factual)

v0.10.5 repairs the security scanner at the root: the raw-text
suspicious-pattern scan (which failed on a protocol documentation
comment in validate.go) is replaced by a comment-aware tokenizing
scanner (tools/gosecscan) whose executable-text detection domain is
byte-identical to the old scan's; the child-process audit moves from a
line-oriented comment filter to the same structural tokenization with
its allowlist unchanged; and the discovery start flow's automatic
selection now enforces the v0.9.8.6 route-trust boundary (trusted
routes only unless untrusted public routes are explicitly allowed).

Evidence actually executed for v0.10.5: full Linux Go suite, `-race`
full suite, frontend npm ci/typecheck/tests/build/embed validation,
real-core verification with the three SHA-256-pinned cores (Xray
26.3.27, V2Ray 5.53.0, sing-box 1.14.0 — `check`/`test` + startup +
listener readiness for the full TUIC/Hysteria/Hysteria2/WireGuard
semantic matrix including the new_reno variant), and windows/amd64
cross-build with PE windowsgui-subsystem verification from Linux.

NOT yet executed at release: the Windows-native CI gate (WinINet
round-trip battery, Windows runtime smoke, Windows desktop build on a
Windows runner) — it runs when this tree is pushed and must pass
before the release is described as Windows-verified. No Internet
verification over a live tunnel was performed for this release (the
real-core battery validates generated documents and listener
readiness, not remote reachability).

### v0.10.4 current-release status (factual)

v0.10.4 corrects protocol semantics against the pinned sing-box
v1.14.0 (TUIC `udp_relay_mode` native | quic — the v0.10.3
"quadratic" was invented; Hysteria2 obfs salamander | gecko;
Hysteria v1 obfs as the documented JSON string) and root-causes the
last v0.10.3 Windows test failure (an invalid WinINet bypass fixture —
CIDR is not WinINet syntax; production WinINet code was not widened).

Evidence actually executed for v0.10.4: full Linux Go suite,
`-race` on the tunnel/config/parser/core packages, `GOOS=windows`
vet/build/test-binary-compile checks, frontend typecheck + unit tests
+ production build, and real-core verification (sing-box 1.14.0,
SHA-256-pinned: `sing-box check` + startup + listener readiness for
every TUIC/Hysteria/Hysteria2/WireGuard semantic variant, added to the
CI real-binary smoke).

NOT yet executed at release: the Windows-native CI gate (WinINet
round-trip battery, Windows runtime smoke, Windows desktop build) —
it runs on GitHub-hosted Windows runners when this tree is pushed and
must pass before the release is described as Windows-verified.

```
P0 — Fully usable personal connectivity client
P1 — Competitive Windows client
P2 — Advanced routing/privacy
P3 — Multi-platform expansion
```

## P0 — Fully usable personal connectivity client

The user must be able to:

```
install → launch → import OWN config/subscription → validate →
select compatible core → test → connect → real Internet
verification → usable traffic route → monitor/recover →
disconnect cleanly → reconnect
```

| Capability | Status in v0.10.1 |
|---|---|
| Personal config import (VLESS, VMess, Trojan, Shadowsocks, Hysteria/Hysteria2, TUIC, WireGuard, SOCKS, HTTP, supported JSON, base64 subscriptions) | Done in v0.10.2 — first-class Import flow (paste/file → detect → parse → validate → redacted capability preview → Save → Test → Connect); no subscription source needed, no second pipeline. v0.10.1's claim of a "validated runtime path per protocol" was FALSE for hysteria2/tuic/wireguard (parser-only, no core could execute them) — repaired in v0.10.2 |
| Subscriptions (add, refresh, parse, deduplicate, persist, imported/rejected counts, real errors; a failed refresh never erases working configs) | Done |
| Explicit-config connect (the user's own selection is never silently replaced by Quick Connect) | Done |
| Real protocol→core compatibility (verified against installed cores, not parser recognition) | Done — capability resolution against the live registry. v0.10.2 adds REAL sing-box runtime for Hysteria2/TUIC/WireGuard/Hysteria, schema-verified against the pinned v1.14.0 binary (see docs/protocols.md for the per-protocol matrix and evidence level) |
| Testing engine (preflight → core validation → startup → listener readiness → handshake → measured latency → Internet verification → persistent evidence; never "connected" because a process launched) | Done — E2E target fetch through the real tunnel |
| Connection engine (route → core/provider start → local proxy → route traffic → external verification → Connected) | Done — Connected is never reported before verification |
| Quick Connect (explicit config → THAT config; Auto → fresh test → rank → connect; cooldowns and recovery through the SAME engines) | Done |
| Recovery (bounded episodes, cooldown decay, re-testing through the one ranking path) | Done |
| Windows system proxy (connect → listener verified → proxy set → external traffic verified; restore on disconnect/shutdown/crash/reconnect) | Repaired + hardened in v0.10.2. v0.10.1's WinINet ABI was broken on Windows (every multi-option activation failed — the crash-recovery CI failure); the ABI is now byte-exact, ownership is transactional (durable record BEFORE activation, verified restoration before the marker is consumed, explicit residual errors), and the recovery battery runs repeatedly in CI |
| DNS/IPv4/IPv6 evidence (measured, never fabricated) | Done — staged diagnostics ladder + identity evidence |
| Core/provider management (install/update/repair/reinstall/verify/remove; truthful installed/version/path/origin/ownership/health) | Done |
| Windows installer (clean install/upgrade/repair/uninstall; writable workspace; single tree) | Done — Inno Setup path |
| WHITE/BLACK/RED UI | Done — v0.10.1 token rework |

## P1 — Competitive Windows client

Feature depth of modern v2ray/VPN clients on Windows without
sacrificing the architecture, trust model, local-first behavior or
truthful state reporting.

- [x] Viewport-aware menus everywhere (one portal-based surface — v0.9.15)
- [x] Incremental UI state (queue-driven, no rebuild-the-world — v0.9.15)
- [x] Tor lifecycle (resolve → verified download → extraction → validation → smoke test → activation → bootstrap → verify) with resumable staging (v0.9.15)
- [x] Honest Psiphon provider states (managed unavailable is reported as unavailable; user-binary path is first-class)
- [ ] Performance budget enforcement on every hot path (startup, import, refresh, single/batch test, Quick Connect, core start, connect, disconnect, recovery) — measured baselines exist, continuous enforcement is not wired
- [ ] TUN mode where GENUINELY supported (transactional implementation with verified rollback; today TUN remains honestly experimental/unavailable — see engine/tunnel/tun_unavailable.go for the defect list that blocked it)

## P2 — Advanced routing/privacy

- [ ] Rule-based routing profiles on top of the managed cores
- [ ] Deeper leak visibility (route exposure timelines, DNS route history)
- [ ] Reversible adapter privacy controls where Windows supports them (explicit, reversible, documented, safe — no spoofing, no hardware fingerprint tampering)

## P3 — Multi-platform expansion

- [ ] Linux/macOS desktop parity (the engine is OS-neutral; the
      tunnel/process layers carry the platform split)
- [ ] Per-platform installers

The sections below are the historical working documents that produced
the current state. They remain as the engineering record; the ladder
above is the commitment.

---

## Current Baseline

Current main:

* Version: `0.10.1`
* Platform focus: Windows x64
* Runtime: Go + Wails + React/TypeScript (toolchain pair pinned:
  `wails/v3 v3.0.0-beta.19` + `@wailsio/runtime 3.0.0-beta.19`,
  CI-enforced)
* Core families: Xray, V2Ray, sing-box
* First-class providers: Tor, Psiphon
* Quick Connect: enabled (fresh-selection loop, verification-gated,
  trusted-routes-only by default — public untrusted sources require
  the explicit "Allow public untrusted routes" opt-in)
* Connection verification: multi-target quorum (2 of 3 operators) with transient retry and evidence-based degradation/recovery
* Session teardown: deterministic (v0.9.8.6) — monitor join, teardown
  before the terminal state, stale generations discarded
* UI synchronization: event-driven (v0.9.9) — authoritative
  transition paths publish through ONE publisher boundary per stream
  (`internal/statepub`): deduplicated, ordered, zero-delay delivery
  with no artificial coalescing window; fast lifecycle bursts are
  delivered in order and never silently lost; the 2-second ticker
  broadcasts are gone
* Embedded assets: stable filenames (v0.9.9) — `index.html`,
  `assets/index.js`, `assets/index.css`, `assets/export-worker.js`
  produced directly by the build and replaced in place; hashed
  asset names and stale artifacts are CI-forbidden
* Wails bindings: machine-generated from the pinned toolchain for the
  core service surface, reproducible (second generation
  byte-identical), no hand-written shims — plus the v0.9.11
  hand-maintained Connection Profiles binding (the wails3 generator
  cannot run on the current host), which is a CI-VERIFIED mirror: every
  `$Call.ByName` target is checked against the registered Go service
  and, since v0.9.12, the model types are verified field-for-field
  against the Go JSON contract (`TestProfileBindingModelsMatchGoStructs`)
* Process cleanup: ownership-aware — supervised children carry kernel
  job objects; the managed-process manifest (PID + executable path)
  backs path-verified installer termination; broad image-name kills
  are forbidden
* Internet diagnostics: staged ladder (local link → local IP → DNS → TCP → TLS → HTTPS → captive portal → direct → tunnel) with per-stage failure classes
* DNS diagnostics: system vs curated public resolvers, A/AAAA, UDP→TCP fallback, honest failure classification
* Network identity: local IP + public IP + ISP/ASN on explicit user action
* Adaptive memory controller: enabled (evidence-gated growth)
* Managed workspace: enabled (application-folder-only)
* Managed provider/core installation: enabled (digest-mandatory:
  installs are rejected without an authoritative published digest;
  bounded archive extraction via internal/safearchive)
* HTTP policy: explicit transport proxy modes (direct by default —
  ambient HTTP(S)_PROXY variables are ignored)
* Tunnel modes: System Proxy (WinINet) production — transactional
  ownership since v0.10.2 (durable record BEFORE activation, verified
  restoration before marker consumption, explicit residual errors;
  v0.10.1's WinINet ABI defect repaired at root cause); TUN
  EXPERIMENTAL and DISABLED (v0.9.8.6 — not a kill switch)
* Logging profiles: enabled (Normal / Detailed / Debug)

### v0.9.12 — completed historical work

v0.9.12 is a provider-lifecycle monotonicity release closing the
v0.9.11 race-gate failure at its ROOT CAUSE — no verification, trust
or recovery policy relaxed, no tests modified to pass. Completed, each
verified by an executed battery (unit + `-race` incl. repeated
targeted runs of the previously failing test + native + frontend
suite + pinned real-core smokes + windows/amd64 build validation +
clean-room embed check): the ONE monotonic generation-scoped run-state
model shared by the Tor and Psiphon engines (stale events discarded,
monotonic progress, exactly-once immutable readiness verdict, event
gate closed on stop/failure), the Tor readiness contract enforced as
documented (Bootstrapped 100% observed AND endpoint verified — the
endpoint-early shortcut demoted to recorded EVIDENCE), the
field-for-field binding-contract verification for hand-maintained
models, the future-schema preservation guards (store.meta preserved
verbatim and rebuilt from chunks; profiles/sources/collections saves
refuse loudly to downgrade), the serialized settings writers, the
atomic config-order write, the HIGH UI busy-ownership leak fix, and
generation guards on the remaining store mutation paths.

### v0.9.11 — completed historical work

v0.9.11 closed the Windows test-oracle defect behind CI run
35571120221 at its root (a genuinely cross-platform process-liveness
oracle with real Windows evidence — no skips, no fake values) and
shipped the first P2 roadmap feature, Connection Profiles (named,
persistent preference sets activated through the ONE settings path),
plus host-independent fail-closed archive sanitization (archive-space
validation before any host conversion; complete rejection matrix for
POSIX/Windows absolute, drive/UNC/device namespaces, ADS, traversal,
symlink/hardlink/specials, zip/tar bombs, over-delivery and truncated
archives) and the Windows PE resource version-consistency gate.

### v0.9.10 — completed historical work

v0.9.10 is the connection-lifetime architecture repair + beginner-first
product release. Completed, each verified by an executed test battery
(unit + `-race` + fake-core lifecycle batteries + frontend suite +
clean-room build), with the lifecycle proofs additionally verified to
FAIL on the v0.9.9 tree: the session runtime-context separation
(operation context never bounds process lifetime; provider engines own
per-run runtime contexts; deterministic provider-over-core session
replacement; crash as a true session boundary), the v0.7 roadmap
completion (evidence-based source reliability dashboard with
not-enough-data sentinels, configuration grouping with persistent user
groups through the one filter pipeline, favorites), the beginner-first
navigation (Connect → Configurations → Sources + More), the humanized
failure surface with the Fix-my-connection action, the session status
card, the why-cores explainer, contextual education hints, and the
code-split secondary surfaces under the extended stable-filename embed
contract.

### v0.9.9 — completed historical work

v0.9.9 is a core-engine execution / runtime upgrade release — NO new
connectivity feature was advanced. Completed, each verified by an
executed test battery (unit + `-race` + native + real protocol-core
smoke + clean-room build): the CI embed-check path-context repair and
the single shared embed-inventory validation source, the
connection-manager mutex/generation audit (bare `m.state` and
`lastPref` reads fixed, provider failure paths generation-gated), the
single-count startup metrics contract, the consolidated readiness
supervision with the bounded adaptive probe schedule, the centralized
port resolution (`core.ResolveInboundPort`), the recovery-service
lifecycle join (Stop cancels and JOINS an in-flight recovery
decision), the event-driven process-exit monitor, the
critical/replaceable statepub queue semantics with the bounded UI
delivery boundary, the single-pass Quick Connect candidate collection
with the fixed worker-pool fresh testing, the truthful backend-
version-aware gen-cache contract, and the startup priority model for
background work.

### v0.9.8.8 — completed historical work

v0.9.8.8 is a deep cleanup / repository-hygiene release — NO new
connectivity feature was advanced. Completed, each verified by an
executed test battery: the TS2393 duplicate-worker fix (CI run
35519469195 root cause), the final canonical asset pipeline
(index.js/index.css/export-worker.js with the six stale artifacts
deleted from the committed tree), the single zero-delay statepub
publisher boundary (ordered lifecycle delivery, drain-on-stop,
no artificial sleep), the ownership-aware installer with the
managed-process manifest, and the documentation truth pass.

### v0.9.8.7 — completed historical work

v0.9.8.7 is a determinism/responsiveness release — NO new connectivity
feature was advanced. Completed, each verified by an executed test
battery: the CI v-prefix normalization fix (run 35492972394 root
cause), the `internal/statepub` deduplicating publisher and the
connection-manager subscription mechanism, the truthful regenerated
bindings (including the local-port settings wiring completion and the
phantom `log_retention_days` removal), the two dormant fakecore
failure injections activated as lifecycle regressions, HTTPS-only
asset-URL enforcement, and the SourceTrust labeling on the
`BestCandidates` ranking path. (Its stable-asset pipeline never
landed in the committed tree — a typecheck failure blocked CI before
the embed check ran — and was completed in v0.9.8.8 with the final
canonical names.)

> Note: this section previously reported `0.9.8.2` with runtime
> findings from that era (unbounded queue-depth growth, readiness
> presented as success). Those findings described real defects that
> the v0.9.8.3 implementation already fixed; the baseline text lagged
> the repository and was corrected as of v0.9.8.4, then again for the
> v0.9.8.6 trust/teardown changes above.

### v0.9.8.6 — completed historical work

v0.9.8.6 is a reliability/security/hygiene release — NO new
connectivity feature was advanced. Completed, each verified by an
executed test battery:

* Deterministic Windows session teardown — `stopMonitor` joins the
  monitor goroutine, the stability teardown completes before
  `connection_failed` is observable, teardown errors are preserved,
  and the crash path closes the crashed instance's owned files
  (root cause of the v0.9.8.5 Windows CI TempDir failure).
* Session generations — every async verification is generation-gated;
  stale results (success or failure) are discarded at disconnect,
  reconnect, shutdown and stability teardown.
* Route-trust boundary — sources classified official/user/public,
  configs carry their source's trust band, Quick Connect excludes
  untrusted public routes by default with an explicit opt-in; the
  invalid nirevil-vless README endpoint was removed.
* TUN disabled everywhere — the non-transactional Wintun backend was
  removed; TUN is reported experimental/unavailable and is not a
  kill switch.
* Executable trust — core installs rejected without an authoritative
  digest; https-only asset URLs (loopback excepted); bounded archive
  extraction (internal/safearchive) with zip-bomb/tar-slip tests.
* Explicit HTTP proxy policy — direct by default; SSRF and netcheck
  no longer inherit ambient proxies; proxy-confusion tests added.
* Wails contract — runtime pinned and lockfile re-resolved to
  beta.19 (the tree had drifted to beta.20); bindings contract test
  (41 ByName + 24 ByID calls verified); clean-room embed check in CI.
* Security CI — corrupted push trigger repaired (`branches: ain]`
  never fired); allowlist-based dangerous child-process scan with
  justified exceptions.
* Release signing — explicit Authenticode architecture with honest
  UNSIGNED state when secrets are absent.
* Hygiene — worklog.md, tools/neteval, rsrc_windows_386.syso and 11
  stale hashed frontend bundles removed; release history moved to
  CHANGELOG.md; documentation truth pass.

### v0.9.8.5 — completed historical work

The v0.9.8.5 release completed the following items, each verified by
an executed test battery (not by inspection alone):

* Multi-target connection verification (§10, verification stage) —
  the single-endpoint gate became a bounded multi-target quorum
  (2 of 3 independent operators), with one bounded transient retry
  (timeout/reset/proxy-handshake/5xx only — deterministic 4xx,
  refused and TLS-interception failures never retry) and per-target
  evidence (`engine/connection/verify.go`; regression battery
  `verify_multi_test.go`, 8 tests).
* Stability degradation + recovery (§1.2/§1.3 evidence rules) — a
  verified session is periodically re-verified through the active
  tunnel: one failed recheck degrades the snapshot without tearing
  the session down; the consecutive-failure threshold transitions to
  `connection_failed` and the bounded recovery loop; any success
  resets the evidence. Provider sessions join the same schedule
  (`engine/connection/connection.go`, `provider.go`; manager-level
  degradation + recovery tests).
* One testing-engine verification model — the tester's end-to-end
  probe runs the same `connection.VerifyTunnel` gate instead of a
  second, weaker single-target copy (`engine/tester/core_probe.go`).
* Staged Internet diagnostics — the connectivity report carries the
  ordered nine-rung ladder with per-stage status, latency and failure
  class, plus `failed_stage`; the seven-state classifier remains
  authoritative (`engine/netcheck/stages.go`; 5 ladder tests).
* DNS diagnostics — resolver comparison (system vs Cloudflare/
  Google/Quad9), A + AAAA, UDP with TCP fallback, failure classes
  (timeout/refused/SERVFAIL/NXDOMAIN/empty/malformed/unreachable/
  cancelled/invalid), query-name validation, private-resolver
  rejection, bounded responses, no DNSSEC claims
  (`engine/netcheck/dnsdiag.go`; 14-test matrix against a local fake
  resolver).
* Network identity — route-relevant local IPv4/IPv6 (no traffic),
  public exit IP direct vs tunnel with match comparison, ISP/ASN/
  country from documented keyless sources, Unknown-when-unavailable,
  explicit user action only with a 60-second response cache
  (`engine/netcheck/identity.go`, `engine/app/internettools.go`;
  12-test matrix).
* UI/UX audit (§17 alignment) — dead classes removed, missing styles
  defined, ghost-danger hover fixed, inline styles tokenized,
  duplicate helpers consolidated; 9 tabs × 6 viewports responsive
  verification with zero horizontal overflow.

### v0.9.8.3 — completed historical work

The v0.9.8.3 implementation (shipped on `main` before this baseline
correction) completed the following roadmap items, verified against
the actual code rather than the stale notes above:

* Quick Connect fresh-selection loop — recent verified successes
  merged with the ranked set, deduplicated, shortlisted, fresh-tested
  within a bounded budget, re-ranked from fresh evidence, connected
  and verified (`engine/app/quickconnect.go`).
* Verified connection state — `connected_verified` is the only final
  success; the connection engine gates every attempt on an end-to-end
  Internet verification (`engine/connection/connection.go`,
  `verify.go`).
* Recovery re-ranking — automatic recovery reuses the exact
  fresh-selection loop, with its own exclusions and cooldowns
  (`engine/app/recoveryservice.go`).
* Configuration reorder — stable IDs and persisted user ordering
  (`engine/app/configorderservice.go`).
* Local proxy ports — user-selected SOCKS5/HTTP inbounds with
  preflight bind checks and honest conflict reporting.
* Application-folder workspace — everything lives in the application
  folder; migration from legacy trees (`docs/workspace.md`).
* Installer/uninstaller improvements — desktop shortcut, complete
  application-owned cleanup, bounded questions (`scripts/freeiran.iss`).
* Compact logging — level filter before formatting, one JSON
  serialization per emitted record, session/event identity only on
  correlated records (`internal/logging`).
* Adaptive memory improvements — queue-depth growth is evidence-gated
  with cooldowns and hysteresis; idle is a no-op (`engine/booster`).
* Tor installation improvements — first-class managed acquisition
  with checksum verification.
* Psiphon trust-boundary handling — honest acquisition state, explicit
  user action only.

The runtime findings listed in earlier revisions of this section
(items 7–10: unconditional queue-depth growth, readiness marked as
success, Quick Connect presenting unverified sessions) no longer hold
against the current implementation; the regression suites in
`engine/app`, `engine/connection`, `engine/booster` and the frontend
store contract tests pin this behaviour.

---

# 1. Product Principles

## 1.1 One application, one workspace

Everything FreeIran creates, stores, downloads, executes or manages must live inside the selected FreeIran application/workspace folder.

Target layout:

```text
FreeIran/
├── FreeIran.exe
├── uninstall.exe
├── config/
├── data/
├── cache/
├── logs/
├── cores/
│   ├── xray/
│   ├── v2ray/
│   └── sing-box/
├── providers/
│   ├── tor/
│   └── psiphon/
├── runtime/
├── docs/
└── deployment/
```

No normal application state should silently move to:

* `%APPDATA%`
* `%LOCALAPPDATA%`
* arbitrary `%TEMP%`
* another hidden application-data tree
* another working directory

Explicit operator overrides such as `FREEIRAN_HOME` may remain supported.

Temporary OS facilities may still be used where technically unavoidable, but FreeIran-owned persistent/runtime data must remain under the application workspace and be cleaned deterministically.

---

## 1.2 Core readiness is not connection success

The state machine must remain:

**select → prepare → start core/provider → wait ready → establish route → verify Internet → connected → monitor**

A local listener opening successfully is never enough to claim a verified connection.

The UI and logs must clearly distinguish:

* starting
* core/provider ready
* route established
* verification running
* verified usable
* failed
* recovering

---

## 1.3 Evidence beats assumptions

Every selection decision should be based on real observations:

* recent successful connections
* fresh test results
* measured latency
* stability
* recent failure streak
* protocol compatibility
* current core/provider health
* actual Internet verification

Never select a route because it is merely present in the database.

---

# 2. P0 — Fix Quick Connect and Connection Reliability

## 2.1 Fresh-test the best recent configurations before Quick Connect

Quick Connect must not simply trust a stale ranking snapshot.

Before connecting in **Configurations** or **Auto** mode:

1. Load the strongest recently successful configurations.
2. Load the strongest currently-ranked candidates.
3. Merge and deduplicate them.
4. Prefer configurations with:

   * recent verified success
   * recent measured latency
   * stable history
   * low recent failure streak
5. Test a bounded top set concurrently.
6. Record fresh results.
7. Recalculate ranking from the new evidence.
8. Attempt the highest-ranked currently usable candidate.
9. If the candidate fails during connection or verification, continue with the next freshly-ranked candidate.
10. Only after the recent shortlist is exhausted should the adaptive discovery path expand the search.

The default shortlist should be configurable, with a sensible bounded default such as the top 5–10 recent candidates.

Quick Connect should therefore behave as:

```text
Quick Connect
    ↓
load recent winners + current best
    ↓
deduplicate
    ↓
fresh bounded test
    ↓
re-rank
    ↓
connect
    ↓
verify Internet
    ↓
monitor
    ↓
recover/re-test/re-rank if needed
```

---

## 2.2 Persist "last working" evidence properly

Each configuration should maintain durable, bounded evidence such as:

* last verified success
* last failure
* success count
* consecutive failure count
* last measured latency
* latency measurement timestamp
* recent test timestamp
* protocol/core used
* verification result
* cooldown/penalty state

Do not treat source freshness as connection freshness.

A configuration that worked five seconds ago should have substantially different evidence from one that worked three days ago.

---

## 2.3 Recovery must reuse fresh ranking

Recovery should not simply select the next static candidate.

Recovery:

```text
failure
→ identify recent viable candidates
→ test bounded shortlist
→ update observations
→ re-rank
→ connect
→ verify
→ monitor
```

Repeatedly failing candidates must rapidly lose priority.

Recently verified working candidates should regain priority without permanently dominating fresh candidates.

---

## 2.4 Internet verification must become mandatory before success

A configuration/session should only become:

**CONNECTED / VERIFIED**

after an actual Internet verification succeeds through the active route.

Core readiness may be shown as a lower-level state but must never be confused with usable connectivity.

---

# 3. P0 — Tor and Psiphon Provider Installation

## 3.1 Tor installation must become a first-class working flow

The current Tor provider architecture is structurally correct but the end-to-end acquisition/install experience must be fixed.

Required flow:

```text
Check installed
→ resolve official release
→ determine OS/architecture
→ fetch authoritative checksum metadata
→ select correct asset
→ download
→ verify SHA-256
→ verify published signature where supported
→ unpack safely
→ locate executable
→ validate executable
→ smoke test
→ activate atomically
→ write manifest
→ report Installed
```

The resolver must support current stable official releases rather than depending forever on an obsolete hardcoded version.

The official Tor distribution currently publishes Windows x86_64 expert bundles and signed checksum metadata; the implementation should use that official publication model.

Required UI:

* Check
* Install
* Update
* Repair
* Start
* Stop
* Uninstall
* Refresh
* View verification details

Show real progress:

```text
Resolving
Downloading
Verifying
Unpacking
Validating
Smoke testing
Activating
Ready
```

Never display success before the final managed executable has passed validation.

---

## 3.2 Psiphon must have a clearly honest acquisition state

Psiphon remains a first-class provider.

However, FreeIran must not weaken executable trust requirements just to make automatic installation appear to work.

The current official Psiphon tunnel-core documentation points to `psiphon-tunnel-core-binaries` for official binaries, but that repository currently has no GitHub releases and is not a checksum-bearing release channel.

Therefore implement two explicit states:

### Managed install available

Only when FreeIran can obtain:

* official source
* deterministic platform/architecture asset
* trusted checksum/signature authority

Then use the normal managed installation pipeline.

### Managed install unavailable

Show:

> Automatic Psiphon installation is currently unavailable because the official binary source does not publish a trusted release/checksum channel for this platform.

Provide:

* Select Psiphon executable
* Validate
* Smoke test
* Copy into managed storage
* Activate
* Run

Never:

* download a moving branch blindly
* execute an unverified executable
* silently substitute a third-party build

Also investigate whether an official Psiphon Windows distribution can provide a future deterministic, verifiable source without changing Psiphon into an ordinary node protocol. The official Psiphon Windows project currently publishes signed releases, so this should be researched as a separate provider-distribution option rather than assuming tunnel-core binaries are the only possible acquisition route.

---

# 4. P0 — Core Manager Redesign

The core manager should behave like a mature desktop core manager.

Supported cores:

* Xray
* V2Ray
* sing-box

For each core provide one consistent lifecycle:

```text
Unknown
→ Checking
→ Available
→ Installing
→ Validating
→ Ready
→ Update available
→ Repairing
→ Broken
→ Removed
```

## 4.1 Core catalog

Maintain a unified catalog containing:

* core name
* platform
* architecture
* latest known version
* installed version
* release source
* download asset
* checksum
* release date
* executable path
* health status

No hardcoded assumptions about the installed binary.

---

## 4.2 One-click core installation

A user should be able to:

**Cores → Xray → Install**

and receive:

```text
Resolve release
→ download
→ checksum verification
→ unpack
→ executable validation
→ config validation
→ smoke launch
→ shutdown test
→ activate
→ Ready
```

No manual folder copying should be required.

---

## 4.3 Update and repair

Each core needs:

* Check for update
* Update
* Reinstall
* Repair
* Roll back where a previous verified version exists
* Remove

A broken installation should be repairable without deleting unrelated configuration data.

---

## 4.4 Installation diagnostics

When installation fails, show the actual failing stage:

```text
Resolve failed
Download failed
Checksum failed
Archive invalid
Executable missing
Executable invalid
Config validation failed
Smoke test failed
Activation failed
```

Do not reduce all installation failures to:

> Installation failed.

---

# 5. P0 — Quick Connect Provider UX

Quick Connect must become the single reliable entry point.

### Configurations

```text
fresh-test recent winners
→ re-rank
→ connect
→ verify
```

### Tor

```text
if installed:
    start → bootstrap → verify Internet → connected
else:
    show Install Tor action
```

### Psiphon

```text
if installed:
    start → local proxy ready → verify Internet → connected
else:
    show valid acquisition/install path
```

### Auto

Auto must compare real evidence across:

* configurations
* Tor
* Psiphon

It must not silently install software.

Auto may recommend an unavailable provider, but the UI must clearly explain:

```text
Available
Installed
Not installed
Unavailable
Failed
```

---

# 6. P0 — Configuration Ordering Bug

The configuration ordering/reordering implementation must be corrected so that changing order actually updates the complete ordered collection.

Requirements:

* Reordering one item changes the entire persisted order.
* The UI immediately reflects the new order.
* Reloading the application preserves the same order.
* Sorting/filtering must not mutate the underlying order accidentally.
* Virtualized rows must use stable configuration IDs as keys.
* Reordering must operate on IDs/records, never array indexes alone.
* Concurrent refreshes must not overwrite a newer user reorder.
* Persisted ordering must be deterministic.
* Multi-selection or grouped configuration tabs must never reorder only the first/top item unintentionally.

Add regression tests for:

* first → last
* last → first
* middle → first
* middle → last
* repeated moves
* filtered view reorder
* refresh after reorder
* application restart
* large configuration sets

---

# 7. P0 — User-Controlled Local Proxy Ports

Add a dedicated proxy-port configuration system similar to mature proxy clients.

Settings:

```text
SOCKS5 port
HTTP proxy port
```

The user may choose the preferred port.

Example:

```text
SOCKS5: 10808
HTTP:   10809
```

## Port validation

Before starting a session:

1. Validate range.
2. Validate loopback binding.
3. Check IPv4 availability.
4. Check IPv6 availability where applicable.
5. Detect conflicts.
6. Ensure the port is actually bindable.
7. Start the core/provider.
8. Verify that the expected listener is actually accepting.
9. Confirm the final endpoint matches the requested configuration.

Avoid a check-then-use race by performing the preflight immediately before launch and always validating the actual listener afterward.

If the preferred port is occupied:

```text
Port 10808 is unavailable.
Choose another port or allow automatic selection.
```

Do not silently change the user's preferred port without reporting it.

Optional future mode:

```text
Preferred port
Automatic fallback
```

---

# 8. P0 — Logging and Runtime Performance

The current logging format is unnecessarily expensive.

The default runtime log should contain only useful information:

```text
timestamp
level
subsystem
event
message
important fields only
```

Do NOT write `session_id` and `event_id` on every single record.

## Correlation model

Use correlation identifiers only when necessary:

* startup session
* connection/recovery episode
* explicit debugging
* error investigation

They should not be duplicated onto every routine record.

---

## 8.1 Remove repetitive policy logs

The current log sequence:

```text
queue 35000 → 36000
queue 36000 → 37000
queue 37000 → 38000
...
```

is not useful telemetry.

Normal adaptive operation should remain silent.

Log only:

* meaningful policy state change
* pressure transition
* significant concurrency change
* critical memory action
* failure
* recovery

---

## 8.2 Fix the underlying queue-depth behavior

The current booster increases queue depth by 1,000 every normal tick until the ceiling.

That is directly reflected in the supplied runtime log.

Change the behavior so queue depth grows only when there is evidence that additional queue capacity is beneficial.

Use:

```text
backlog
throughput
active workers
CPU pressure
memory pressure
queue utilization
```

The queue must stop growing when additional capacity provides no measurable benefit.

Add:

* hysteresis
* bounded growth
* cooldown
* utilization threshold
* memory-aware ceiling
* no-op stability period

Do not optimize merely by lowering the log volume; fix the unnecessary adaptation itself.

---

# 9. P0 — Clean Application-Local Installation

Redesign the Windows installer around a writable application directory.

The installed deployment should behave like:

```text
Chosen FreeIran installation folder
        ↓
FreeIran.exe
        ↓
all application state
        ↓
all cores
        ↓
all providers
        ↓
all logs
        ↓
all runtime/cache/data
```

Do not relocate the workspace to `%AppData%\FreeIran`.

## Installer requirements

### Installation

* standard Windows installer
* architecture-aware
* safe replacement of a running instance
* version metadata
* application icon
* optional desktop shortcut
* Start Menu shortcut
* clear installation folder selection
* writable-folder validation
* no hidden second workspace

### Default folder

Use a user-writable default installation directory.

The application folder itself becomes the workspace root.

### Uninstall

Provide a real uninstaller inside the application installation folder.

Uninstall must:

1. stop FreeIran
2. stop managed cores
3. stop Tor/Psiphon
4. clean managed runtime processes
5. remove the application folder
6. remove shortcuts
7. remove installer-generated metadata
8. leave no hidden FreeIran runtime tree elsewhere

Before destruction, provide a clear confirmation of what will be removed.

Optionally provide:

```text
Keep user data
Remove everything
Export data first
```

but the product must not silently leave a second hidden workspace.

---

# 10. P1 — Connection Testing Engine Upgrade

Create one canonical testing strategy for configuration connectivity.

Testing stages:

```text
cheap reachability
→ protocol handshake
→ measured latency
→ tunnel establishment
→ Internet verification
```

Do not perform expensive testing when an earlier cheap test already proves failure.

Use bounded parallelism.

Each test result should capture:

* success/failure
* measured latency
* measurement provenance
* failure category
* timeout
* protocol
* target
* timestamp
* test path
* core used

---

## 10.1 Test freshness

Classify observations as:

* fresh
* recent
* stale
* unavailable

Ranking must prefer fresh evidence over stale evidence.

---

## 10.2 Failed candidate cooldown

Repeatedly failing configurations should temporarily enter cooldown.

Cooldown should decay automatically.

A previously excellent configuration should be able to return after a successful retest.

---

# 11. P1 — Better Ranking Model

Ranking should combine:

```text
verified success
freshness
measured latency
success rate
failure streak
protocol reliability
recent tests
core compatibility
network environment
```

Never use static source order as a proxy for quality.

Do not overfit to latency alone.

A 50 ms unstable node should not automatically dominate a 70 ms stable verified node.

---

# 12. P1 — Unified Provider/Core Status Center

The Cores page should become the authoritative runtime management center.

Sections:

```text
Protocol Cores
    Xray
    V2Ray
    sing-box

Connectivity Providers
    Tor
    Psiphon
```

Every item exposes:

* installation state
* installed version
* latest known version
* source
* checksum
* health
* executable
* local endpoints
* runtime state
* install/update/repair/remove actions

No duplicate provider/core installation logic.

---

# 13. P1 — Connection Session Management

There must be exactly one authoritative connection session model.

The session owns:

* selected route
* active provider/core
* local proxy ports
* verification state
* latency evidence
* monitoring
* recovery episode
* cleanup

Disconnect must clean everything created by that session.

A new connection must not inherit stale local listeners or previous unmanaged state.

---

# 14. P1 — Runtime Cleanup and Resource Control

All application-owned runtime material must be bounded.

Implement:

* log rotation
* stale `.part` cleanup
* stale provider runtime cleanup
* abandoned core process cleanup
* bounded diagnostics history
* cache limits
* queue limits
* startup cleanup
* shutdown cleanup

Routine cleanup must never delete durable user configuration without explicit user action.

---

# 15. P1 — Logging Profiles — COMPLETED in v0.9.8.4

Support three logging levels:

### Normal

Minimal operational information.

### Detailed

Connection/provider/core lifecycle and useful diagnostics.

### Debug

Verbose tracing, correlation IDs and detailed subsystem fields.

Normal mode must be the default.

Debug identifiers must not consume normal runtime storage.

### Implementation (v0.9.8.4)

ONE authoritative policy lives in the logger
(`internal/logging/logging.go`) — no scattered `if debug` checks:

* `Profile` (normal / detailed / debug) + `Record.Lifecycle` tier;
  admission = severity floor × profile × lifecycle tier
  (`admitsPolicy`), checked BEFORE any formatting cost.
* Normal (default): routine verbose diagnostics suppressed; records
  stay compact — session/event identity only on correlation-opt-in
  records (v0.9.8.3 contract preserved).
* Detailed: adds lifecycle-tagged diagnostics (core start/stop,
  cleanup reclamation) and full identity on every emitted record;
  the dedupe gate still collapses repetitive ticks.
* Debug: verbose diagnostics plus correlation identifiers on every
  record — connection/recovery episode identifiers universally
  available; still bounded by rotation and age retention.
* Settings: `logging_profile` persisted with the existing settings
  system, validated, and applied to the live logger immediately on
  save (`engine/app/loggingservice.go`); Settings UI segmented
  control with concrete per-profile explanations.
* Safety and performance unchanged: redaction applies to every
  record in every profile; no per-record identity allocations in
  Normal; rotation/retention bounds apply to all profiles.
* Regression matrix: `internal/logging/logging_profile_test.go`
  (per-profile admission, runtime switching Normal → Detailed →
  Debug → Normal without restart, redaction, rotation/retention) and
  `engine/app/loggingservice_profile_test.go` (validation,
  persistence, live switch).

---

# 16. P1 — Performance Budget

FreeIran should optimize for:

* fast startup
* low idle CPU
* low idle disk I/O
* bounded memory
* fast connection attempt
* fast recovery
* minimal logging overhead

Measure rather than assume.

Track:

```text
startup → UI ready
Quick Connect → test start
Quick Connect → first connection
Quick Connect → verified Internet
recovery → next candidate
core start → local listener
provider start → verified route
```

Do not invent benchmark claims.

---

# 17. P1 — UI State Integrity

All UI surfaces must reflect actual backend state.

Never display:

* Connected before verification
* Installed before activation
* Ready before readiness observation
* Healthy without a health measurement
* Current ranking from stale data without freshness indication

The UI should explain why an action is unavailable.

---

# 18. P2 — Advanced Connectivity Features

After the core reliability work is complete:

* configurable SOCKS/HTTP proxy exposure
* system proxy integration
* TUN mode
* per-application routing
* route diagnostics
* DNS strategy
* proxy chain support
* ~~connection profiles~~ — DONE in v0.9.11 (P2 §18): named
  connection-preference sets in one versioned workspace sidecar,
  activation through the ONE settings path, profiles reference
  configuration IDs, no credentials, no second networking flow and no
  bypass of verification/trust/recovery. Import/export profiles
  remains future work.
* import/export profiles
* environment-aware route selection
* richer provider diagnostics

These must build on the existing connection engine rather than creating parallel networking implementations.

---

# 19. P2 — Provider Expansion

Potential future first-class providers may include:

* additional censorship-resistance transports
* additional tunnel engines
* additional verified protocol runtimes

Every new provider must satisfy the same contract:

```text
Resolve
Install
Verify
Start
Ready
Health
Connect
Monitor
Recover
Stop
Cleanup
```

No provider becomes a fake node configuration merely for UI convenience.

---

# 20. P2 — Recovery Intelligence

The recovery system should eventually learn from:

* previous success
* previous failure
* failure type
* route quality
* time of day
* network environment
* provider/core reliability
* recent verification
* configuration age

Recovery decisions must remain explainable.

Example:

```text
Switched because:
previous route failed Internet verification
candidate had successful verification 4 minutes ago
candidate measured 83 ms
candidate has zero recent failures
```

---

# 21. Data and Safety Invariants

These are non-negotiable.

### User configuration

Never silently delete.

### User-provided executables

Never move, rename or delete the original file.

### Managed executables

Only activate after:

```text
trusted source
→ checksum/signature validation
→ archive safety
→ executable validation
→ smoke test
```

### Network tools

Preserve:

* SSRF protection
* DNS rebinding protection
* redirect validation
* private-target restrictions
* bounded body sizes
* bounded timeouts

### Processes

Every child process must use the unified process-supervision system.

No provider-specific raw Windows process management.

---

# 22. Testing Strategy

Every implementation phase must add regression coverage.

## Backend

```text
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

## Connection

Test:

* fresh ranking
* stale ranking
* last-working candidates
* failed candidate cooldown
* retry
* recovery
* Internet verification
* disconnect cleanup

## Providers

Test:

* resolve
* install
* checksum
* invalid archive
* missing executable
* start
* readiness
* health
* stop
* uninstall
* rollback
* user-binary adoption

## Core manager

Test:

* fresh install
* already installed
* update
* repair
* rollback
* failed download
* checksum mismatch
* corrupted archive
* wrong architecture
* missing executable

## Proxy ports

Test:

* free port
* occupied port
* invalid port
* preferred port
* fallback port
* actual listener verification
* restart
* disconnect/reconnect

## Workspace

Test:

* portable deployment
* installed deployment
* explicit `FREEIRAN_HOME`
* application-local runtime
* uninstall cleanup
* migration safety
* no hidden workspace creation

## Logging

Test:

* default compact format
* no per-record session ID
* no per-record event ID
* debug correlation mode
* rotation
* bounded file growth
* no repetitive memory-policy spam

## Frontend

Test:

* Quick Connect fresh-test flow
* provider mode
* unavailable provider
* installation states
* configuration reorder
* persistence after reload
* proxy-port configuration
* connection state accuracy

---

# 23. Release Acceptance Gate

A release is complete only when all of these are true:

## Connection

* Quick Connect tests the recent best configurations again.
* Fresh results feed ranking.
* Connection uses freshly ranked evidence.
* Internet verification is mandatory.
* Recovery repeats the evidence cycle.

## Providers

* Tor installs and runs successfully through the managed pipeline.
* Psiphon has a safe managed installation path when a trusted official binary exists.
* Otherwise Psiphon clearly exposes the verified user-binary workflow.
* No unverified moving-branch executable is executed.

## Cores

* Xray installs correctly.
* V2Ray installs correctly.
* sing-box installs correctly.
* Update/repair/remove work.
* Version and checksum state are visible.

## Ports

* User-selected proxy ports work.
* Occupied ports are detected before connection.
* Actual listeners are verified.
* No unexplained silent port changes occur.

## Workspace

* Everything persistent and runtime-owned is inside the application folder.
* No hidden `%AppData%\FreeIran` workspace exists.
* Uninstall removes the installed application tree.
* Optional user-data preservation is explicit.

## Logging

* Default logs are compact.
* No per-record session/event IDs.
* Memory-policy spam is gone.
* Adaptive queue growth is evidence-driven.
* Log storage is bounded.

## Configuration ordering

* Reordering modifies the whole collection correctly.
* Persisted order survives reload/restart.
* Regression tests cover the previous bug.

## Verification

* Local tests pass.
* Windows CI passes.
* Runtime Windows smoke passes.
* Clean-room installation passes.
* Clean uninstall passes.
* No unsupported claims are reported.

---

# 24. Recommended Implementation Order

### Phase 1 — Connectivity correctness

1. Quick Connect recent-success retesting
2. Fresh ranking integration
3. Mandatory Internet verification
4. Recovery re-ranking
5. Configuration reorder bug

### Phase 2 — Runtime performance

6. Fix adaptive queue-depth growth
7. Remove repetitive memory-policy logging
8. Compact default logging
9. Log rotation and bounds

### Phase 3 — Cores and providers

10. Repair Tor resolution/install
11. Modernize core catalog and install/update/repair flow
12. Improve Psiphon acquisition UX
13. Preserve checksum/signature safety

### Phase 4 — Proxy control

14. User-selected SOCKS5 port
15. Optional HTTP port
16. Port preflight
17. Listener verification
18. Automatic fallback policy

### Phase 5 — Installation model

19. Redesign installed workspace to remain inside application folder
20. Clean installer
21. Optional desktop shortcut
22. Embedded uninstaller
23. Complete application-folder cleanup
24. No hidden secondary workspace

### Phase 6 — Full verification

25. Backend regression suite
26. Frontend regression suite
27. Windows CI
28. Windows runtime validation
29. Clean-room installation
30. Clean uninstall
31. Performance/resource validation
32. Release documentation

---

# 25. Completion Standard

FreeIran should reach the point where a new user can:

```text
Install FreeIran
→ optionally create desktop shortcut
→ open FreeIran
→ core/provider status is detected automatically
→ install required cores/providers from the app
→ choose preferred local proxy port
→ press Quick Connect
→ FreeIran tests its best recent configurations
→ re-ranks them
→ connects
→ verifies Internet
→ exposes the configured local proxy
→ monitors the route
→ automatically recovers when necessary
→ disconnects cleanly
→ exits without leaving orphan processes
→ uninstall removes the application cleanly
```

The product should require as little manual technical knowledge as possible while preserving explicit control, trustworthy verification, and transparent failure reasons.

---

# 26. Engineering Rule

Do not solve these issues as isolated UI patches.

The desired architecture is:

**one workspace → one lifecycle → one connection engine → one ranking engine → one testing engine → one process supervisor → one core/provider management model → one source of truth for runtime state.**

Every future feature should reuse those foundations rather than creating another parallel subsystem.
