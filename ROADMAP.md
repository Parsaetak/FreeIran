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
- One engine, one connection state machine, one routing authority,
  one DNS authority, one TUN authority, one system-proxy authority,
  one store, one test queue, one process supervisor. Every item below
  lands inside those boundaries or not at all.

## The two-phase transition

v0.13.1 begins a deliberate two-phase transition from a managed
external-core runtime to a first-party connectivity engine:

- **PHASE 1 — v0.13.1 FOUNDATION + APPLICATION REPAIR (this
  release)**: documentation/architecture contract, Windows icon root
  repair, Settings layout repair, the first-party FreeIran Engine
  foundation with a real in-process local proxy path (HTTP CONNECT +
  SOCKS5 inbounds; SOCKS5/HTTP/direct outbounds), System Proxy
  decoupled from sing-box for first-party-supported routes, and the
  first-party TUN device/control-plane foundation. External cores
  remain the compatibility fallback for everything the engine does
  not yet implement.
- **PHASE 2 — FIRST-PARTY CONNECTIVITY ENGINE EXPANSION (next
  release(s))**: complete the FreeIran-owned TUN packet dataplane,
  move DNS/routing into the engine, and expand protocol/transport
  coverage one capability at a time until the external
  Xray/V2Ray/sing-box/Mihomo roles can be retired one capability at a
  time — each only behind its own evidence gate.

## PHASE 1 — v0.13.1 FOUNDATION + APPLICATION REPAIR

What v0.13.1 actually ships (details and evidence classes in
CHANGELOG.md and the technical docs):

- **Documentation/architecture contract** — this roadmap, the README,
  docs/architecture.md, docs/tun.md, docs/protocols.md and
  docs/autonomous-connectivity.md now describe the two-phase engine
  transition and the exact Phase 2 implementation contract.
- **Windows icon root repair** — the executable's icon group now
  carries the numeric resource ID the pinned Wails v3.0.0-beta.19
  window path loads (RT_GROUP_ICON ID 3, previously a named "APP"
  group the loader could never find), plus the documented
  `application.Options.Icon` fallback from the same canonical asset
  authority, and a strengthened PE regression test that pins the
  numeric ID.
- **Settings UI/UX repair** — numeric controls (ports, counts,
  thresholds, timeouts, intervals) render in genuinely compact
  fields with compact rows; range sliders are styled; the profile
  form shares the same compact treatment; DOM + CSS regression
  coverage keeps them compact.
- **FreeIran Engine foundation** (`engine/freecore`) — the first-party
  Go engine: bounded session lifecycle, inbounds/outbounds/dialer/
  router-decision/DNS-interface model, one-time normalization from
  the universal `config.Config`, and capability detection. Real
  bytes flow through FreeIran-owned Go code on the supported path.
- **First-party local proxy path** — local HTTP CONNECT and SOCKS5
  inbounds; SOCKS5, HTTP CONNECT and direct outbounds; context
  cancellation and deadline propagation end-to-end; bounded buffers
  and bounded session accounting.
- **System Proxy decoupling** — when the selected route is supported
  by the engine (SOCKS5/HTTP remote over plain TCP), the connection
  runs in-process on the FreeIran Engine and System Proxy points
  WinINet at the engine-owned local endpoint. No sing-box (or any
  external core) process is launched for that session. WinINet
  capture/apply/verify/restore, idempotency, rollback and crash
  recovery are unchanged under the one TunnelService authority.
- **TUN device/control-plane foundation**
  (`engine/freecore/tun`) — the first-party TUN boundary: device
  interface, deterministic adapter identity, transaction/rollback
  state, loop-prevention metadata, the Wintun binding path and the
  IP Helper observation seam, with platform unit tests for
  ownership/identity/rollback decisions. NOT exposed as a working
  TUN dataplane: the sing-box native TUN backend remains the only
  runnable TUN mode, selected explicitly.
- **Compatibility fallback preserved** — every configuration the
  engine cannot genuinely run (VLESS, VMess, Trojan, Shadowsocks,
  QUIC family, WireGuard, TLS/REALITY transports, proxy chains,
  UDP) continues through the existing external cores through the
  same registry/selection/connection machinery.

Phase 1 success criteria (all verified at the classes CI can
execute; physical Windows shell/TUN runtime remains explicitly
unverified where marked): documentation internally consistent;
System Proxy first-party path provably process-free; unsupported
routes provably still reach external cores; no duplicate
connection/tunnel/proxy/store/queue/supervisor authority; TUN
foundation tested without pretending to be a dataplane.

## PHASE 2 — FIRST-PARTY CONNECTIVITY ENGINE EXPANSION

The ordered implementation program. Every item is PLANNED. Each item
records: **purpose** (why it exists), **dependencies** (what must
land first), **boundary** (where the code lives and what it may NOT
do), **evidence** (what proves it), **fallback** (what happens until
it is proven), and **retirement gate** (when the corresponding
external-core role may be retired). The universal rules: FreeIran
Engine slices enter `engine/freecore` behind its existing model;
external cores stay compatible backends until a capability's
retirement gate is met; nothing is claimed without its evidence.

### 1. FreeIran TUN packet dataplane (complete Phase 1's foundation)

- Purpose: route OS packets through the first-party engine —
  the core deliverable that makes "TUN without sing-box" real.
- Dependencies: Phase 1 `engine/freecore/tun` foundation; the
  engine's session/outbound model; IP Helper observation seam.
- Boundary: packet read/write loop over the Wintun session owned by
  `engine/freecore/tun`; TCP/IP stack handling (a reviewed,
  license-audited in-process stack or a carefully reviewed minimal
  TCP state machine); NAT/routing decisions inside the ONE routing
  authority; DNS handling inside the ONE DNS authority. No netsh,
  route.exe, PowerShell, tun2socks executable, or second VPN
  process.
- Evidence: platform unit tests for loop/drop/forward decisions;
  Windows runtime evidence ladder per docs/tun.md — interface
  observed, routes observed, REAL tunneled request verified,
  upstream pinning observed; physical elevated Windows host run
  before any "TUN works" claim.
- Fallback: sing-box native TUN remains the selectable backend;
  first-party TUN stays unexposed until its activation gate can
  honestly pass.
- Retirement gate: sing-box TUN role retires only when first-party
  TUN passes the full docs/tun.md evidence ladder on a physical
  elevated Windows host, including rollback and crash recovery.

### 2. Central DNS authority and leak-aware DNS path

- Purpose: one resolver model for the whole engine; no DNS decision
  anywhere else; leak-aware behavior (system resolver bypass
  detection).
- Dependencies: engine session model; TUN dataplane (hijack path);
  routing decisions carrying resolver choices.
- Boundary: `engine/freecore` DNS resolver implementation over the
  Phase 1 `Resolver` interface; remote/plaintext/DoH strategies;
  per-session resolver pinning; observation of the OS resolver state
  stays read-only (IP Helper).
- Evidence: unit tests for resolution strategy selection and
  leak-verdict logic; runtime DNS leak evidence class defined in
  docs/tun.md / autonomous-connectivity.md before any claim.
- Fallback: engine sessions keep using the remote proxy's own
  resolution (target-side DNS) exactly as Phase 1 does.
- Retirement gate: retire per-core DNS compilation (sing-box dns
  blocks) only when the engine authority covers every shipped
  configuration shape with leak evidence.

### 3. Routing engine and rule evaluation

- Purpose: one rule model (domain/IP/process/geo) evaluated inside
  the engine with Direct/Proxy/Block actions.
- Dependencies: DNS authority; session model.
- Boundary: `engine/freecore` router; Phase 1's `RouterDecision`
  becomes the real decision point; external-core configs stop
  embedding route rules once the engine owns routing.
- Evidence: deterministic rule-evaluation unit tests; equivalence
  suites against the external-core route semantics for the shipped
  rule subset.
- Fallback: external cores keep evaluating their own generated route
  rules.
- Retirement gate: route-rule generation for external cores is
  removed only when the engine router covers the shipped subset with
  equivalence evidence.

### 4. SOCKS/HTTP remote coverage completion + connection pooling

- Purpose: harden and complete the Phase 1 outbounds (auth variants,
  keep-alive pooling, happy-eyeballs dialing, timeout classes).
- Dependencies: Phase 1 outbounds; routing engine for per-route dial
  decisions.
- Boundary: `engine/freecore` outbound implementations only.
- Evidence: loopback integration tests with real bytes for every
  auth/variant matrix entry; cancellation/timeout/close tests;
  pooling reuse proven under concurrency (race-tested).
- Fallback: Phase 1 behavior (no pooling, per-connection dial).
- Retirement gate: Xray/V2Ray/sing-box never were required for
  plain SOCKS/HTTP after Phase 1; this item only removes the
  "external core preferred" leftovers if any remain.

### 5. Shadowsocks

- Purpose: first-party AEAD cipher suite for the most common public
  configurations.
- Dependencies: routing engine; crypto review.
- Boundary: `engine/freecore` outbound; reviewed cipher
  implementations only (license-audited; no wholesale code copy from
  external cores); capability declaration stays honest per method.
- Evidence: unit tests against RFC/documented test vectors;
  interop tests against a reference server binary in CI (the same
  real-binary discipline docs/protocols.md records).
- Fallback: sing-box/Xray continue serving Shadowsocks.
- Retirement gate: external-core Shadowsocks support retires when
  the engine passes the interop suite for every declared method.

### 6. Trojan

- Purpose: first-party Trojan protocol (TLS-carried).
- Dependencies: TLS transport layer (item 12); routing engine.
- Boundary: `engine/freecore` outbound + TLS config handling.
- Evidence: real-server interop suite; TLS SNI/ALPN handling tests.
- Fallback: external cores serve Trojan (TLS mandatory today).
- Retirement gate: same interop standard as item 5.

### 7. VLESS

- Purpose: first-party VLESS (incl. flow semantics where legally and
  technically supportable).
- Dependencies: TLS/REALITY (item 12); transport matrix.
- Boundary: `engine/freecore` outbound; REALITY only after its own
  evidence item passes.
- Evidence: interop against reference servers per flow/transport
  combination actually declared.
- Fallback: Xray/sing-box serve VLESS.
- Retirement gate: per flow/transport combination, never blanket.

### 8. VMess

- Purpose: first-party VMess (legacy + AEAD).
- Dependencies: transport matrix; crypto review.
- Boundary: `engine/freecore` outbound.
- Evidence: documented test vectors + reference-server interop.
- Fallback: external cores serve VMess.
- Retirement gate: per (alterId, security, transport) matrix entry.

### 9. Hysteria / Hysteria2

- Purpose: first-party QUIC-based transports.
- Dependencies: QUIC stack review (quic-go is already a direct
  dependency); congestion/brutal-CC licensing review.
- Boundary: `engine/freecore` outbound; QUIC multiplexing inside the
  engine process.
- Evidence: interop against reference servers; bandwidth/CC
  behavior tests.
- Fallback: sing-box serves the QUIC family.
- Retirement gate: per protocol, after interop + license review.

### 10. TUIC

- Purpose: first-party TUIC v5.
- Dependencies: QUIC stack; item 9's review work.
- Boundary/evidence/fallback/retirement gate: same pattern as 9.

### 11. WireGuard (and later variants where licensing/security
  review permits)

- Purpose: first-party WireGuard outbound (and, later, AmneziaWG-style
  variants only after their own review).
- Dependencies: TUN dataplane for full-tunnel use; crypto review.
- Boundary: `engine/freecore` outbound; no kernel driver work.
- Evidence: interop against reference peers; handshake/roaming
  tests.
- Fallback: sing-box serves WireGuard.
- Retirement gate: after interop evidence; variants never blanket-
  retire the base protocol.

### 12. Transport/security matrix: TCP/TLS/WebSocket/HTTP2/gRPC/QUIC/
  XHTTP/REALITY combinations

- Purpose: the transport/security layer that items 5–11 plug into.
- Dependencies: TLS stack choice (Go crypto/tls baseline; uTLS
  fingerprinting only after license/security review); HTTP/2, gRPC
  and WebSocket client layers.
- Boundary: `engine/freecore` transport abstractions (the Phase 1
  transport/security model becomes real implementations); REALITY
  enters only behind its own evidence gate.
- Evidence: per-combination interop against reference servers;
  fingerprint behavior tests where declared.
- Fallback: external cores keep serving every combination the engine
  has not declared.
- Retirement gate: per combination, evidenced; never a blanket
  "transports done" claim.

### 13. Proxy chains/detours inside one engine process

- Purpose: multi-hop chains as engine-internal detours (one process,
  one session graph) replacing the compiled single-core documents.
- Dependencies: outbound matrix breadth (items 4–12).
- Boundary: chain compilation into engine-internal detours; the
  existing chain validation/collection model stays the ONE chain
  authority.
- Evidence: loopback multi-hop integration tests; equivalence
  checks against the v0.12.2 single-core chain semantics.
- Fallback: chains compile to Xray/sing-box documents as today.
- Retirement gate: when every hop protocol the collections UI can
  build also runs in-engine with equal verification.

### 14. Adaptive failure-stage selection and transport switching

- Purpose: use the existing nine-class failure taxonomy to switch
  engine-side transports/routes mid-session within one authority.
- Dependencies: routing engine; outbound matrix; failure taxonomy
  (already in `engine/connection`).
- Boundary: selection logic extends the ONE connection state
  machine; no second selector.
- Evidence: fault-injection tests proving stage-accurate switching.
- Fallback: current bounded fallback across external cores.
- Retirement gate: n/a (behavioral, not a core-retirement item).

### 15. Protection/evidence expansion

- Purpose: continuous route/traffic/DNS proof (beyond
  activation-time), IPv4/IPv6 leak verdicts, and the future WFP
  kill switch (per autonomous-connectivity.md §F).
- Dependencies: TUN dataplane; DNS authority.
- Boundary: read-only observation seams; WFP work only as its own
  gated item (never bundled).
- Evidence: the evidence ladder classes in docs/tun.md extended to
  continuous proof; WFP needs physical Windows host evidence.
- Fallback: current activation-time verification only.
- Retirement gate: n/a (evidence work).

### 16. External-core retirement (per capability)

- Purpose: shrink the external-core surface only as first-party
  capabilities are proven.
- Dependencies: every item above that an external core currently
  owns.
- Boundary: retirement is per capability with its evidence recorded
  in docs/protocols.md; the managed core manager keeps serving any
  core that still owns at least one capability; Mihomo's managed
  role is re-evaluated at that point, not before.
- Evidence: the retirement gates of items 1–13.
- Fallback: everything not yet retired keeps working exactly as
  v0.13.1 ships it.
- Retirement gate: a core is fully retired only when zero shipped
  capabilities depend on it.

## Testing / Evidence Standard

Unit (routing, DNS policy, failure classification, candidate
scoring, proof/state transitions, engine inbounds/outbounds,
loop-guard and rollback decisions), integration (core startup,
engine local-proxy bytes, TUN, route ownership, DNS hijack,
public-IP verification, IPv6, crash, reconnect, rollback), Windows
(Wintun, IP Helper, WFP, WinINet, adapter/route lifecycle, restart
recovery) and the clean-room ladder (install → … → uninstall) are
specified in [docs/autonomous-connectivity.md](docs/autonomous-connectivity.md)
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
