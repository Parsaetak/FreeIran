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

## Current Baseline (v0.13.0)

- Multi-core managed runtime (Xray, V2Ray, sing-box — digest-verified
  install/update/rollback; Mihomo core-manager-managed without a
  connection adapter), one managed-binary pipeline, one
  process supervisor, one downloader.
- Discovery/testing/ranking: multi-level discovery, ping/URL test
  modes, measured ranking, bounded test queue with adaptive memory
  control and deferred bulk admission.
- Connection engine: state machine, controlled racing, environment
  intelligence, nine-class failure classification, transport-agile
  selection.
- System Proxy (WinINet) and Windows TUN (managed sing-box native
  dataplane — transactional, observed activation; physical elevated
  host runtime NOT VERIFIED; see docs/tun.md evidence ladder).
- Configuration workspace: one server-side filter/sort/pagination
  pipeline with the TRUE global total and a global config-ID
  tie-breaker (no bounded prefixes), authoritative uncapped group
  counts, source scopes with factual freshness ("Updated 12m ago" /
  "Never fetched"; content hashes are internal evidence only), and
  real paginated infinite scroll in every scope.
- System integration as a first-class Main-page block (v0.13.0):
  System Proxy and TUN toggles plus local inbound ports on the same
  TunnelService and settings path the rest of the app uses, with the
  native tray mirroring the REAL state (no drift after external
  changes or failed enables) and honest prerequisites — a dataplane
  enable without a selected/connected configuration states and
  enforces the prerequisite instead of faking success.
- Proxy chains (v0.12.2 contract unchanged): ordered existing config
  IDs, max 4 hops, compiled into ONE core process (Xray
  `sockopt.dialerProxy` / sing-box `detour`), one connection state
  machine.
- CI: full Linux suite, Windows two-layer proof, real-core
  verification with pinned binaries, security battery, GUI launch
  proof, PE resource-icon regression check.

Known not-verified carried forward: physical elevated Windows TUN
runtime; Mihomo as a runnable backend.

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

Each transport enters only as a verified protocol-core adapter or a
compile-time topology inside one — never as a marketing claim, and
never described as universally faster or censorship-proof.

## Phase 3 — Circumvention transport slot (PLANNED — generic)

The v0.12.2 product removed Tor (and Psiphon) from the active
runtime; the historical "Tor circumvention phase" is retired rather
than promised. What remains PLANNED is a GENERIC
circumvention-transport slot: IF a future transport earns its way in,
it must enter through the EXISTING architecture contracts — one core
manager, one binary trust pipeline, one connection state machine, one
verification gate, one MenuSurface, one configuration store. Any
future mechanism plugs in as either (a) a new protocol-core adapter
with a verified capability matrix or (b) a compile-time topology
within an existing adapter (the path proxy chains take). No
provider-per-mechanism architecture will be reintroduced.

## Phase 4 — Canonical routing/DNS

```text
central routing engine     (N: engine/routing/, compiled per core)
DNS/routing integration    (route decision carries resolver decision)
actions: Direct / Proxy / Block (I2P: generic future slot)
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
Multi-hop chains (v0.12.2 ships the single-core compilation model)
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

## Engineering rule

> The transport connects. The engine decides. The evidence proves.

Every phase lands only with verification at the class it touches;
claims never exceed evidence; history stays historical; future work
stays PLANNED until its proof exists.
