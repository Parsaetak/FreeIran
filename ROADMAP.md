# FreeIran Roadmap

## FreeIran — Autonomous Local Connectivity Engine

Forward roadmap. The future-architecture contracts behind every phase
live in [docs/autonomous-connectivity.md](docs/autonomous-connectivity.md);
current implemented behavior lives in the technical docs
([docs/README.md](docs/README.md) indexes authority). Release history
lives in [CHANGELOG.md](CHANGELOG.md) and is not repeated here.

Rules:

- Nothing below the baseline is complete. Future items are PLANNED
  and never presented as current capability.
- Each phase must be TRUE (verified at its own evidence class) before
  the next one starts; a CI/unit result is not a physical elevated
  Windows TUN runtime result.
- `Connected` is never `Protected`; no future item may be marketed
  beyond its evidence.

## Current Baseline (v0.12.1)

- Multi-core managed runtime (Xray, V2Ray, sing-box — digest-verified
  install/update/rollback; Mihomo core-manager-managed without a
  connection adapter), one provider manager (Tor, Psiphon), one
  process supervisor, one downloader.
- Discovery/testing/ranking: multi-level discovery, ping/URL test
  modes, measured ranking, bounded test queue.
- Connection engine: state machine, controlled racing, environment
  intelligence, nine-class failure classification, transport-agile
  selection.
- System Proxy (WinINet) and Windows TUN (managed sing-box native
  dataplane — transactional, observed activation; physical elevated
  host runtime NOT VERIFIED; see docs/tun.md evidence ladder).
- CI: full Linux suite, Windows two-layer proof, real-core
  verification with pinned binaries (sing-box 1.14.1 — upstream
  stable is 1.14.2; the verified pin is retained deliberately),
  security battery. GUI launch proof executes in CI.
- Network tools as a truthful diagnostic system (v0.12.1): ten-state
  status semantics with distinct log events, honest endpoint-tool
  target resolution (no blind 127.0.0.1:1080), a real bounded QUIC v1
  handshake probe (measurement-only quic-go dependency), WebSocket
  multi-target aggregation, native Windows ICMP walker (IP Helper
  API, user mode), DNS partial evidence + bounded DoH comparison row,
  and provenance-free `core_discovered` normal logs.
- Configuration workspace as a source-aware browsing surface
  (v0.12.1): source/subscription scopes with authoritative counts,
  per-source targeted refresh through the ONE ingestion pipeline,
  per-source check through the ONE test queue, app-wide native
  context-menu suppression with FreeIran's own MenuSurface, and
  desktop-class keyboard basics.
- v0.12.0 itself: documentation/architecture consolidation — the
  long-term direction is now captured canonically (no new features,
  no architecture change).

Known not-verified carried forward: physical elevated Windows TUN
runtime; live GUI-launch verdict outside CI; Mihomo as a runnable
backend.

## Phase 1 — Full-device protection

```text
ConnectionProof          (E: proof fields, Protected as proof-gated state)
DNS authority            (D: central engine, compiled per core)
IPv4/IPv6 proof          (E: protected / leak / unavailable states)
route proof              (continuous, beyond activation-time)
traffic proof            (continuous end-to-end verification)
WFP kill switch          (F: Windows Filtering Platform narrow-allow
                          policy, state machine, crash-safe recovery)
crash recovery           (extended to kill-switch state)
```

## Phase 2 — Adaptive censorship engine

```text
DNS/TCP/TLS/HTTP differential probing   (G: probe ladder)
adaptive transport selection            (H: candidate engine)
REALITY  XHTTP  Hysteria2  TUIC
Shadowsocks  WireGuard  AmneziaWG       (I/J: as adaptive candidates)
```

## Phase 3 — Tor + circumvention

```text
Tor provider transports: obfs4, Snowflake, WebTunnel, meek
Tor Circumvention mode
Onion Only mode            (.onion only, no public-Internet fallback)
```

## Phase 4 — Canonical routing/DNS

```text
central routing engine     (N: engine/routing/, compiled per core)
DNS/routing integration    (route decision carries resolver decision)
actions: Direct / Proxy / Block / Tor / I2P
```

## Phase 5 — Privacy diagnostics

```text
privacy exposure audit     (Q)
hardware-ID minimization   (Q: collect nothing, honestly)
public-IP verification     (R: Verified / Not verified / Unavailable,
                            project/self-hosted endpoints)
low-metadata mode          (S)
```

## Phase 6 — Advanced topologies

```text
multi-hop                  (M: trust topology, explicit diagram)
Tor-over-VPN  VPN-over-Tor
I2P-only                   (L)
application-specific routing
```

## Testing / Evidence Standard

Unit (routing, DNS policy, failure classification, candidate scoring,
proof/state transitions, WFP policy generation), integration (core
startup, TUN, route ownership, DNS hijack, public-IP verification,
IPv6, crash, reconnect, rollback), Windows (Wintun, IP Helper, WFP,
WinINet, adapter/route lifecycle, restart recovery) and the clean-room
ladder (install → … → uninstall) are specified in
[docs/autonomous-connectivity.md](docs/autonomous-connectivity.md)
(section V) and [docs/ci.md](docs/ci.md). The evidence-class rule is
absolute: every claim is stated at the class actually executed.

## Release Acceptance

A release ships only when its scope is verified at the classes it
touches: connectivity (fresh evidence → ranked candidates →
connection → end-to-end verification), protection (TUN/DNS/IPv4/
IPv6/kill-switch evidence), censorship (selection responds to real
failures), dark network (explicit, testable lifecycle states),
recovery (bounded, evidence-first), privacy (no hardware IDs, no
unnecessary telemetry, no fake anonymity claims). See
[docs/autonomous-connectivity.md](docs/autonomous-connectivity.md)
section V and the verification standard in
[docs/development.md](docs/development.md).

## Historical Milestones

Compact record — full facts and evidence live in
[CHANGELOG.md](CHANGELOG.md); unique security/TUN rationale that is
still load-bearing is linked where it matters.

| Version | Milestone |
|---|---|
| v0.1–v0.4 | Engine foundation: config model, parser, dedup, chunked store, streaming pipeline, caching, native acceleration, Wails v3 shell, CI/CD; Xray/V2Ray/sing-box as real backends, connection state machine, core-based testing, real-binary CI verification |
| v0.5–v0.6 | Windows lifecycle repair, fake-core harness, persistent redacted logging; managed Core Manager, bounded test queue, System Proxy (WinINet). The v0.6 TUN backend was later REMOVED (v0.9.8.6) for cause — non-transactional, unverifiable Wintun acquisition |
| v0.9.x | Reliability + security hardening: provider architecture (Tor/Psiphon), archive security, executable trust, discovery engine, measured ranking, verified connections, Connection Profiles, deep Windows lifecycle closure |
| v0.10.x | Windows system-proxy repair + transactional ownership, personal import, real QUIC/WireGuard runtime (sing-box), TUIC/Hysteria semantics, WinINet repairs, WHITE/BLACK/RED theme, comment-aware security scanning |
| v0.11.0 | Compact Windows CI (two-layer proof), nine-class failure classification, transport agility, ECH foundation (schema-verified, sing-box only) |
| v0.11.2 | Mihomo managed core, internal config tabs, native Windows tray, honest tunnel diagnostics, safe "Open shell here" |
| v0.11.3 | Functional Windows TUN (managed sing-box native dataplane — transactional, observed; embedded-Wintun packaging model verified), tray ON/OFF setting, Security workflow repairs |
| v0.11.4 | Security root-fix (secret-shaped test literals derived at runtime), Windows CI bounded-timeout hardening, TUN correctness hardening (fail-closed addressing, exact-adapter identity, native route-path observation), sing-box 1.14.0→1.14.1 alignment |
| v0.11.5 | Windows GUI startup fix (root-caused against pinned Wails beta.19 source), real GUI launch proof (native user32 observation, CI + release wired), metadata-free runtime log, Wails diagnostics bridge |
| v0.12.0 | Documentation/architecture consolidation: canonical future-architecture contract (docs/autonomous-connectivity.md), docs index, README/ROADMAP/CHANGELOG de-duplication, version bump, stale-wording repair |
| v0.12.1 | Network-tool truth (ten-state semantics, distinct log events, honest endpoint resolution, real QUIC probe, native Windows traceroute, DNS DoH row, provenance-free core logs) + source/subscription configuration workspace (targeted refresh + check, scope rail, scope header) + app-wide native context-menu suppression and keyboard UX |

---

# Legacy roadmap record (pre-v0.12.0)

The sections below preserve the prior roadmap's still-relevant
engineering rules and completed P0/P1 context, compactly. Everything
marked COMPLETED is history; nothing here authorizes new work beyond
the phases above.

## Product principles (unchanged)

- **One application, one workspace.** Every subsystem operates on the
  same bounded workspace; state is persisted, recoverable and
  inspectable.
- **Core readiness is not connection success.** A started core with a
  ready listener is not proof of a working tunnel; verification gates
  decide.
- **Evidence beats assumptions.** Rankings, recovery and UI states
  come from measured evidence, not guessed capability.

## Data and safety invariants

- Untrusted input (configurations, archives) is validated and bounded
  before storage/extraction.
- Core/provider binaries only from official upstream releases with
  authoritative digests; installs fail closed without them.
- No shell network configuration; dangerous child-process scanning
  stays fail-closed.
- Crash recovery only ever inspects FreeIran's own recorded state
  (adapter names, session markers) — never unrelated interfaces,
  routes or DNS.
- The runtime log is metadata-free (no timestamps, no commit, no
  toolchain metadata); redaction applies to every credential shape.

## Completed P0/P1 highlights (pre-v0.11)

- Quick-Connect reliability: fresh-test before connect, last-working
  evidence persistence, recovery reusing fresh ranking, mandatory
  Internet verification before success (v0.9.6+).
- Tor/Psiphon provider installation as first-class flows with honest
  acquisition states (v0.9.8.1+, repaired v0.9.15).
- Core manager: catalog, one-click install, update/repair,
  installation diagnostics (v0.6+).
- Local proxy ports user-controlled and validated; configuration
  ordering repaired; bounded logging with correlation by seq.
- Application-local clean installation (no system-wide residue).

## Engineering rule

> The transport connects. The engine decides. The evidence proves.

Every phase lands only with verification at the class it touches;
claims never exceed evidence; history stays historical; future work
stays PLANNED until its proof exists.
