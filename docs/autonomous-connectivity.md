# ARCHIVED / HISTORICAL RESEARCH — NOT AN ACTIVE FREEIRAN ROADMAP

> FreeIran is **FINAL / FROZEN as of 2026-10-07** (v0.final,
> implementation baseline 0.14.2 — see [../FINAL.md](../FINAL.md)).
> This document preserves the September 2026 research-and-development
> work toward an autonomous connectivity engine. It is kept because the
> research is useful and the engineering discipline it records is real.
> It is NOT a commitment, NOT a roadmap, and NOT upcoming FreeIran
> development: everything marked PLANNED below is unimplemented,
> archived research.

---

# Autonomous Connectivity Architecture — the long-term FreeIran contract (ARCHIVED)

Historical research, preserved at freeze time. This was written as the
**canonical implementation handoff** for the long-term FreeIran
direction before the project froze. It distills the September 2026 R&D
report (`Parsaetak/Contents`, Research-and-Development,
`FreeIran-2026-09.md`) into an implementation-ready architecture
contract so future coding agents can implement it incrementally
without rediscovering the research.

Authority rules:

- **Current behavior** is documented by the existing technical docs
  (`docs/architecture.md`, `docs/tun.md`, `docs/protocols.md`,
  `docs/providers.md`, `docs/security.md`, `docs/ci.md`) and by the
  source itself. This document does not override them.
- **Future behavior** is defined HERE, and only here. It is a
  contract for future work, not a description of what exists.
- v0.12.0 records this contract **without implementing it**. Nothing
  in this document may be cited as evidence of a current capability.
- **v0.12.1 boundary note:** the v0.12.1 network-tool work (real QUIC
  handshake probe, DNS DoH comparison row, native Windows ICMP walker,
  ten-state status semantics) are MEASUREMENT capabilities only. They
  do not implement the ConnectionProof, DNS authority, WFP kill
  switch, adaptive transport engine or any other planned component
  below, and they must never be cited as censorship-resistance or
  protection claims. The boundary stands: the tools measure; the
  planned engines protect.

Status vocabulary used throughout:

| Term | Meaning |
|---|---|
| **CURRENT / IMPLEMENTED** | Exists in the repository today, at the evidence level the technical docs record |
| **PLANNED** | Committed future work with a defined contract; not implemented |
| **EXPERIMENTAL** | May be attempted behind a flag; not a product claim |
| **NOT VERIFIED** | Implemented or designed but never proven at the required level; must never be marketed |
| **NON-GOAL** | Explicitly rejected claim or direction |

Core principle (unchanged from the R&D report):

```text
The transport connects.
The engine decides.
The evidence proves.
```

---

## A. Product objective

FreeIran is a Windows-first:

```text
connectivity
privacy
censorship-resilience
routing
evidence
recovery
```

system — an autonomous local connectivity engine that remains usable
under censorship, minimizes network and device-identifying leakage,
adapts to changing network conditions, and **verifies its own
protection rather than merely claiming that a tunnel is active**.

FreeIran does NOT promise invisibility or untraceability (see
section W — Non-goals). The defensible objective is: maximize
resistance to censorship, minimize network exposure and linkability,
prevent accidental leakage, reduce unnecessary metadata, provide
multiple independent connectivity paths, and verify protection
continuously.

A `Connected` state is never equivalent to a `Protected` state.
`Protected` is only ever published when the required evidence for it
exists (see section E).

---

## B. Threat model

The observation chain — each layer observes different information:

```text
Local LAN / Wi-Fi
        ↓
ISP / access provider
        ↓
Censorship / DPI infrastructure
        ↓
Entry / relay (VPN, proxy, future circumvention transport)
        ↓
Exit / destination
        ↓
Application / account
        ↓
Local device (OS, hardware, files)
```

A VPN solves only part of this chain. VPN trust is **shifted** from
the ISP toward the VPN provider — not eliminated. The identifier
categories FreeIran must keep explicitly distinct:

- **public IP** — network identity as seen by destinations;
  changeable by routing through an exit;
- **local IP** — visible to the local access network;
- **IPv6** — an independent leak surface equal in rank to IPv4 (E);
- **MAC / local-link visibility** — visible to the local access
  network; a VPN does NOT hide the local MAC from the access point
  (Wi-Fi randomized hardware addresses are an OS feature that reduces
  link-layer tracking; they are not a VPN capability);
- **device identifiers** — SMBIOS serial/UUID, motherboard, disk
  serial, CPU hardware IDs: firmware/OS-level facts a VPN cannot
  rewrite (see Q — the policy is data minimization, not pretending
  they changed);
- **browser/application fingerprinting** — outside the network
  engine's control (see T);
- **VPN/provider visibility** — the provider and its exit observe the
  originating connection and metadata; the ISP still sees that the
  subscriber talks to the VPN endpoint;
- **traffic correlation** — timing/volume analysis across observers;
  NOT defeated by any single hop.

Different modes (VPN, I2P, multi-hop chains) change WHICH parties see
WHICH side of the connection. That is the honest framing for every
trust claim in the UI (see M).

---

## C. Full-device protection architecture

Target model (PLANNED unless a line is marked CURRENT):

```text
FULL TUN                                            [CURRENT: sing-box native TUN]
+ DNS protection                                    [PARTIAL: TUN DNS hijack; central DNS authority PLANNED]
+ IPv4 protection                                   [CURRENT: route observation at activation]
+ IPv6 protection                                   [PLANNED: leak proof not implemented]
+ route proof                                       [CURRENT: IP Helper forwarding-table observation]
+ traffic proof                                     [CURRENT: tunneled-request verification at activation]
+ WFP kill switch                                   [PLANNED — future work, see F]
+ crash recovery                                    [CURRENT: durable session marker + stale report]
+ continuous monitoring                             [PLANNED: activation-time proofs only today]
```

**State (v0.12.0): the current v0.11.x TUN is NOT a WFP kill
switch.** Routing answers "where should a packet go?"; a kill switch
answers "is a packet allowed to leave the machine at all?". TUN
today is a traffic-routing feature that is transactionally enabled
and verified at activation time. The kill-switch gap is section F.

The existing sing-box native TUN/Wintun architecture stays: the
managed, digest-verified sing-box core is the TUN dataplane (see
docs/tun.md for the full evidence ladder, including the
physical-elevated-Windows-host runtime that remains NOT VERIFIED).
IPv6-disabled and IPv6-protected are different states (see E).

---

## D. DNS as a first-class authority

PLANNED target — a central DNS engine:

```text
engine/dns/
    resolver
    bootstrap
    encrypted transport
    routing
    leak test
    cache
    evidence
```

Modes:

```text
Tunnel DNS        — hijacked into the tunnel resolver (CURRENT behavior inside sing-box TUN)
Encrypted DNS     — DoH/DoQ to chosen resolvers
Bootstrap DNS     — minimal plaintext/system resolution needed to reach the proxy itself
Direct DNS        — explicit domestic/direct resolution (split rules)
Fake-IP DNS       — sing-box fake-IP mode where compatible
Split DNS         — per-domain resolver policy compiled from routing rules
Block             — refuse resolution by rule
Fail-Closed       — no resolution when policy cannot be guaranteed
```

Architectural rule:

> DNS policy is decided centrally and compiled into core-specific
> runtime configurations.

No core (Xray, V2Ray, sing-box, future backends) gets an independent
DNS authority. Routing and DNS must never disagree: a route decision
carries its resolver decision (domain → route + which resolver +
which transport + which bootstrap path). This engine does NOT exist
today as a FreeIran-owned component — what exists is the sing-box
TUN document's DNS module (dns-remote DoH over the proxy +
dns-local bootstrap; system adapter DNS never mutated). The central
engine is PLANNED.

---

## E. IPv4 / IPv6 evidence

Target proof states (PLANNED — the fields belong to the contract
now; the implementation comes later):

```text
IPv4 protected
IPv4 leak
IPv4 unavailable

IPv6 protected
IPv6 leak
IPv6 unavailable
```

```text
IPv6 disabled  !=  IPv6 protected
```

"A session runs IPv4-only (and says so)" — the current honest
address-selection behavior — is the `IPv6 unavailable` shape, not
protection proof. Leak detection (traffic escaping outside the TUN
on either family) is future work.

Future `ConnectionProof` contract (the fields are the contract; the
exact Go implementation can come later):

```go
type ConnectionProof struct {
    CoreReady        bool
    TunnelInterface  bool
    RouteObserved    bool
    DNSObserved      bool
    IPv4Verified     bool
    IPv6Verified     bool
    PublicIPVerified bool
    TrafficVerified  bool
    KillSwitchArmed  bool
    VerifiedAt       time.Time
}
```

FreeIran never publishes `Protected` until the required evidence is
satisfied. The UI must be able to say "Connected but IPv6 not
verified" or "Tunnel active but Internet verification unavailable".
The existing connection state machine
(`Disconnected → Selecting → Preparing → StartingCore →
WaitingForReady → Connected → Verifying → ConnectedVerified`)
evolves toward a proof-backed ladder
(`IDLE → DETECTING → PREPARING → CONNECTING → CORE_READY →
ROUTE_READY → DNS_READY → TRAFFIC_VERIFIED → PROTECTED →
MONITORING → DEGRADED → REPAIR/RECONNECT/TRANSPORT_SWITCH/
BACKEND_SWITCH`), with `Protected` as a proof-gated state — never a
process-start synonym.

---

## F. Windows WFP kill switch

**PLANNED — future work.** Nothing below exists today.

Target state machine:

```text
DIRECT
  → ARMING
  → BOOTSTRAP-ALLOW
  → TUN ESTABLISHED
  → PROTECTED-ALLOW
  → MONITORING
```

Failure path:

```text
core loss
  → TUN loss
  → proof failure
  → protected traffic blocked
  → recovery
```

Critical property: **tunnel lost must never mean traffic silently
falls back to the direct Internet.** The policy must survive
unexpected process death and be safely recoverable after restart
(no stale firewall state after crash or uninstall).

Requirements for the future implementation:

- use **Windows Filtering Platform** with a narrowly defined allow
  policy (tunnel traffic + required local services) that blocks
  unrelated outbound traffic — the WireGuard-for-Windows kill switch
  is the reference design;
- **no shell-based firewall configuration** (no netsh advfirewall,
  no PowerShell) — consistent with the repo-wide no-shell network
  configuration rule;
- WFP policy generation is a unit-testable pure function (see V);
- privileged components get a dedicated security review.

---

## G. Censorship differential evidence

Censorship is not static: commercial DPI classifies multiple
protocols and keeps fingerprints; national filtering has shown
service-level, progressive, and staged behavior where DNS, TCP/443,
TLS, UDP/QUIC and HTTP behaved differently at the same time. A fixed
"best protocol" is therefore the wrong abstraction.

The probe ladder (PLANNED — extends the CURRENT nine-class failure
classification vocabulary `dns/tcp/tls/handshake/listener/verify/
reset/timeout/transport`):

```text
DNS
 → TCP
 → TLS
 → handshake
 → HTTP
 → end-to-end
```

Example distinction the evidence must capture: "DNS succeeds, TCP
succeeds, TLS ClientHello sent, TLS handshake repeatedly times out"
is materially different from "DNS fails" — the failing STAGE
dictates the recovery action (see P).

Future failure evidence carries only privacy-safe metadata:

```text
FailureStage
FailureClass
RemoteEndpoint
Transport
SNI
Timing
RetryCount
Environment
```

No payloads, no credentials, no unnecessary content — the existing
redaction discipline applies.

Rule:

> Protocol selection must respond to observed failure stages, not to
> a fixed "best protocol" ranking.

---

## H. Adaptive transport engine

Target architecture (PLANNED):

```text
Environment Detection
    → Cheap Probes
    → Candidate Generation
    → Core/Transport Compatibility
    → Bounded Attempts
    → End-to-End Verification
    → Evidence
    → Ranking
    → Adaptive Selection
```

Candidate metadata (conceptual):

```text
Protocol
Transport
Core
Endpoint
Port
SNI
TLS profile
REALITY parameters
Provider
Historical success/failure
Freshness
Environment
Infrastructure fingerprint (see O)
```

The current discovery/test/ranking/selection machinery
(engine/discovery, engine/tester, engine/ranking,
engine/connection) is the foundation this extends — the adaptive
engine is an evolution of the existing evidence-driven selection, NOT
a second connection manager.

Explicit contract:

> FreeIran chooses the best currently viable path, not a permanently
> best protocol.

UDP available does not imply UDP useful; a protocol can be
syntactically supported by a core yet operationally unusable in a
given environment. Every candidate is therefore verified end-to-end
before it may be selected.

---

## I. Transport / protocol roadmap

CURRENT capability is governed exclusively by the actual
`docs/protocols.md` matrix and real-core evidence. Upstream support
(in v2rayN or any core) does NOT promote an item to implemented.

Transports as adaptive candidates (first-class capability, PLANNED
beyond what the matrix records today):

```text
TCP            WebSocket      gRPC
HTTPUpgrade    XHTTP          QUIC
```

Protocol families as adaptive candidates:

```text
REALITY         [CURRENT: supported through Xray/sing-box configs — verified at matrix level]
XHTTP           [PLANNED as a first-class candidate]
Hysteria2       [CURRENT: sing-box runtime tested — matrix level]
TUIC            [CURRENT: sing-box runtime tested — matrix level]
Shadowsocks     [CURRENT: tested across cores]
WireGuard       [CURRENT: sing-box endpoint form tested]
AmneziaWG       [PLANNED — see J]
```

"Matrix level" means config acceptance/startup readiness against the
pinned real binaries in CI — NOT live censorship resistance, which
is only ever proven per-connection at runtime.

TLS fragmentation-type packet tricks must never become the
foundation: they are optional tactics, never the central
censorship-resistance layer (censors have demonstrated adapting to
them).

---

## J. AmneziaWG

PLANNED target integration:

```text
canonical FreeIran WireGuard model
    → WireGuard variant (standard | amneziawg)
    → official/verified AmneziaWG implementation
    → existing lifecycle/supervision
    → existing verification
```

Shared canonical model (variant-agnostic):

```text
PrivateKey PublicKey Endpoint AllowedIPs
InterfaceAddress DNS MTU Keepalive
```

with variant-specific obfuscation parameters added only when
required.

Hard rules:

- **no second process manager** — AmneziaWG runs under the existing
  provider/core lifecycle and supervision;
- **no GUI fork** — the official Windows GUI is not integrated;
- **no fake capability claim** — support is declared only after
  real-binary verification at the matrix level;
- the official Windows implementation contains privileged services
  and IPC components — **treat privileged Windows components as a
  dedicated security-review item** before any integration.

---

## K. Circumvention transports (generic future slot)

v0.12.2 note: the active product deliberately REMOVED the Tor and
Psiphon providers, so no "Tor phase" is planned as such. What remains
planned is a GENERIC circumvention-transport slot. Any future
transport must enter through the EXISTING contracts — one core
manager, one binary trust pipeline, one connection state machine, one
verification gate, one configuration store — either as a new
protocol-core adapter with a verified capability matrix or as a
compile-time topology within an existing adapter (the path proxy
chains take). Candidate transports (for study only, no commitment):

```text
obfs4-style pluggable transports
Snowflake-style broker transports
WebTunnel-style HTTPS tunneling
meek-style domain-fronting (legal/ToS analysis required)
```

Modes:

```text
(v0.12.2: the multi-hop topology this section once mapped onto Tor
is shipped as proxy chains — user-built, compiled into one core
process; any future transport enters the same slot.)
Circumvention mode:  FreeIran → <future transport> → Internet
Onion-style mode:    FreeIran → <future transport> → onion service
```

`Onion Only` must mean, as a genuine security mode:

```text
.onion destinations only
no public-Internet fallback
no accidental proxy escape
dedicated DNS behavior
independent proof
```

Bridge/transport-selection logic must be evidence-driven. Hard-coded
bridge lists are not permanent truth; user-provided configuration
remains the trusted model. Any future transport's trust topology must
be explained on its own terms — UI language must never market any
mechanism as "a more anonymous VPN".

---

## L. I2P

PLANNED:

```text
I2P Provider
I2P-only mode
.i2p destinations
```

I2P is a private overlay network, NOT a generic Internet VPN and NOT
an anonymity guarantee. Arbitrary public-Internet outproxy routing must
NOT be the default. The I2P project itself scopes outproxy use as
limited and differently-trusted; FreeIran represents I2P as what it
is — an additional network topology for `.i2p` services under the
same provider lifecycle boundary.

---

## M. Multi-hop / trust topology

PLANNED explicit topologies:

```text
VPN → VPN        (two independent proxy hops)
VPN → chain
chain → VPN
```

plus future combinations. Each topology changes WHO can observe
WHAT — multi-hop is represented as a **trust topology**, never
marketed as automatically "more anonymous".

The UI should eventually explain:

```text
local observer → hop 1 → hop 2 → exit → destination
```

so the user understands which party can observe which side of the
connection. One authority stays: Connection Engine → Route Plan →
Core Adapter — no core gets its own independent chain/routing logic
(v2rayN's group/chain concepts are architectural references only,
adopted conceptually, never copied as code).

---

## N. Canonical routing engine

PLANNED future module:

```text
engine/routing/
    model      matcher     compiler
    ruleset    resolver    dns        profiles
```

Centrally owned matching and compilation. Target rule fields:

```text
Domain  DomainSuffix  DomainRegex
IPCIDR  Port  Protocol  ProcessName  ProcessPath
Action  Outbound  DNSPolicy
```

Target actions:

```text
Direct  Proxy  Reject  Block  I2P
```

Core adapters (Xray, sing-box, future backends) COMPILE the
canonical policy into core-specific runtime configurations. **No
independent routing authority per core.** Routing profiles (Global
Proxy / Iran-Direct + Global-Proxy / Global Direct / Block /
I2P / Custom) are policy presets, not promises — community geo-rule
sources, if ever consumed, are untrusted versioned inputs
(hash-pinned), never embedded security truth.

---

## O. Infrastructure-aware recovery

Several configurations can secretly represent the same failed
infrastructure:

```text
Node A → 1.2.3.4:443
Node B → same server
Node C → same ASN
Node D → same transport + endpoint family
```

Retrying all four after one infrastructure failure is wasted time.
The engine should fingerprint infrastructure:

```text
ServerIP  ASN  Port  Transport  SNI  Provider  ProtocolFamily
```

and maintain a local environment/censor fingerprint:

```text
DNS reachable          TCP/443 reachable
TLS intermittent       UDP blocked
XHTTP successful       REALITY successful
```

This is evidence-aware exploration, NOT static blacklisting —
fingerprints age out and never masquerade as universal truths ("this
environment is currently hostile to UDP", not "UDP is blocked").

---

## P. Recovery policy

Evidence-first recovery — classify the problem BEFORE changing
protocols (PLANNED policy; the current recovery machinery stays the
foundation):

```text
DNS failure                    → alternate DNS/bootstrap
TCP failure                    → endpoint/port change
TLS/SNI failure                → transport switch
UDP blocked                    → TCP-family transport
core crash                     → same-backend retry
repeated core failure          → backend switch
route failure                  → TUN repair
DNS succeeds + traffic fails   → routing/compiler diagnosis
```

Emergency case, stated honestly:

```text
No viable path
```

may mean the network itself is unavailable (large-scale shutdown) —
not that FreeIran chose badly. The UI must make that distinction
visible instead of silently cycling candidates.

---

## Q. Privacy exposure

Target privacy audit (PLANNED). FreeIran does NOT collect or
transmit, unless genuinely required and explicitly authorized:

```text
SMBIOS serial        SMBIOS UUID
motherboard identifiers
disk serial          CPU hardware identifiers
other unnecessary device identifiers
```

The stronger architecture is **data minimization** — not pretending
identifiers were changed. The audit surface distinguishes:

```text
local MAC visibility     public IP exposure
IPv6 exposure            DNS exposure
hardware-ID collection   browser fingerprinting
provider visibility      traffic correlation
```

FreeIran never claims a VPN changes physical hardware identifiers.
An eventual local-link audit (Wi-Fi MAC randomization state:
randomized/stable/unknown; "physical MAC never transmitted by
FreeIran") reports OS state — it does not claim the access network
cannot see the MAC.

---

## R. Public-IP verification

Target evidence states:

```text
Verified        Not verified        Unavailable
```

Future verification should support:

```text
project-controlled endpoint
self-hosted endpoint
multiple trusted endpoints
offline/unavailable state
```

Third-party IP APIs must not be mandatory — a privacy check that
depends on a third-party request creates third-party
logging/metadata exposure. "Not verified" is an honest state;
confidence is never manufactured.

---

## S. Privacy modes

Eventual modes (PLANNED — each needs an explicit contract and must
never imply more protection than its evidence supports):

```text
Protected Mode          full TUN + DNS + IPv4/IPv6 proof + kill switch + monitoring
Censorship Adaptive     evidence-driven transport probing/selection
Circumvention slot      future transports (generic slot)
Onion Only              .onion only, no public exit fallback
I2P                     .i2p destinations
Multi-Hop               explicit trust topology
Low-Metadata Mode       minimal logs, no unnecessary IP checks,
                        no hardware-ID collection, updates only by policy
```

The old "invisible mode" ambition becomes a technical contract —
measurable statements like:

```text
Original public IPv4 hidden from destination:  VERIFIED
DNS outside tunnel:                            NOT OBSERVED
Kill switch:                                   ARMED
Hardware identifiers transmitted:              NONE
Local MAC:                                     visible to local network
Browser fingerprint:                           outside engine control
Global traffic correlation resistance:         NOT GUARANTEED
```

— which is more useful than any marketing claim, and never says
"you are invisible".

---

## T. Browser / application privacy boundary

```text
network anonymity  !=  browser anonymity
```

Cookies, account logins, local storage, screen characteristics,
fonts, APIs, browser configuration, behavioral patterns and
application-specific identifiers remain OUTSIDE the network engine's
guaranteed control — even with a VPN IP, TUN and DNS
protection. FreeIran exposes this limitation directly instead of
implying otherwise. A future privacy-browser layer may exist as a
separate project layer; it is NOT part of current FreeIran claims.

---

## U. Managed-executable lifecycle

v0.12.2 note: the provider abstraction this section once described
(engine/provider) was REMOVED with the Tor/Psiphon engines; managed
executables are the protocol cores through engine/coremgr. The
future contract standardizes around the EXISTING core-manager
lifecycle (one manager, no second lifecycle system):

```text
Resolve → Install → Verify → Prepare → Start → Ready
→ Connect → Health → Monitor → Recover → Stop → Cleanup
```

Core managers (Xray, V2Ray, sing-box, Mihomo; v0.12.2 removed the Tor/Psiphon providers),
future I2P and future AmneziaWG stay conceptually under ONE
lifecycle boundary so the Connection Engine remains independent of
the underlying implementation.

---

## V. Testing / clean-room acceptance

Target tests (PLANNED — current coverage is documented in
docs/ci.md and docs/development.md):

Unit:

```text
routing            DNS                 failure classification
candidate scoring  proof/state transitions
WFP policy generation
```

Integration:

```text
core startup       TUN                 route ownership
DNS hijack         public-IP verification
IPv6               crash               reconnect      rollback
```

Windows:

```text
Wintun             IP Helper           WFP
WinINet            adapter lifecycle   route lifecycle
restart recovery
```

Clean-room target:

```text
install → core acquire → verify → tunnel → DNS → route
→ IPv4 → IPv6 → Internet → kill switch → crash → recovery
→ disconnect → no orphan → no proxy leak → no stale firewall
→ uninstall
```

Evidence ladder rule (explicit): **a CI/unit result is not a
physical elevated Windows TUN runtime result.** Every claim is
stated at the class actually executed — the existing
docs/tun.md ladder (generated-config / Linux-unit /
Windows-compile / Windows-CI / physical-host) remains the model; new
features extend it, never blur its classes.

---

## W. Non-goals

Explicitly banned claims (repository-wide, forever):

```text
completely invisible
impossible to detect
untraceable
authorities cannot identify the user
hardware identity changed
browser fingerprint hidden
traffic correlation impossible
```

Preserved truths (evidence language, current and future):

```text
VPN trust is shifted, not eliminated.
VPN does not hide the local MAC from the access network.
VPN does not rewrite SMBIOS/device identifiers.
Browser fingerprinting is separate from network anonymity.
Circumvention transports have different trust topologies — never marketed as "more anonymous VPN".
I2P is a private overlay, not a normal VPN.
Multi-hop changes trust relationships; it is not automatically "more anonymous".
No censorship technique is permanent.
UDP availability does not imply UDP usefulness.
A protocol can be syntactically supported but operationally unusable.
A successful process start is not proof of protection.
A connected tunnel is not proof of end-to-end protection.
CI does not equal physical-host runtime verification.
```

---

## Implementation rules for future agents

- Preserve the existing architecture. Do NOT create duplicate
  connection managers, core registries, process supervisors,
  downloaders, routing authorities, DNS authorities or session
  models. Extend the existing foundations.
- Do not replace the managed sing-box TUN dataplane; do not replace
  Wails; do not redesign unrelated subsystems.
- v2rayN (and any upstream client) is an architectural REFERENCE
  only — never copy implementation code.
- Every feature moves through: INSPECTED → REPRODUCED → IMPLEMENTED
  → VERIFIED → REGRESSION-TESTED → CLEAN-ROOM VALIDATED. Never
  invent runtime evidence, censorship resistance, anonymity,
  privacy guarantees, benchmarks, network success or hardware-ID
  protection.
- sing-box core-pin changes are deliberate, verified upgrades only
  (official digest, full smoke suite + TUN document check); never a
  documentation-freshness side effect.
- Roadmap phase order lives in ROADMAP.md; the phase contracts live
  in this document.

---

## Phase 2 handoff (v0.13.1) — HISTORICAL (the target that v0.14.x partially executed; the remainder is archived)

v0.13.1 shipped the Phase 1 foundation (ROADMAP.md). The next
agent's implementation program, in order:

1. **Complete the FreeIran-owned TUN traffic dataplane** — build on
   `engine/freecore/tun` (Device, identity, transaction/rollback,
   loop-prevention, Wintun binding, IP Helper seam are in place and
   tested at the unit level). The packet loop, IP stack handling,
   NAT/route decisions and the DNS hijack path land inside the ONE
   engine; the sing-box TUN backend remains the fallback until the
   full docs/tun.md evidence ladder passes on a physical elevated
   Windows host.
2. **Move DNS/routing into the FreeIran Engine** — the `Resolver`
   and `RouterDecision` interfaces in `engine/freecore` become real
   authorities; external-core documents stop embedding route/DNS
   decisions only after the equivalence evidence exists.
3. **Expand protocol implementations incrementally** —
   Shadowsocks → Trojan → VLESS → VMess → Hysteria/Hysteria2 → TUIC
   → WireGuard, each behind its capability gate with reference-
   server interop evidence (docs/protocols.md records the class).
   Reuse the ONE session/routing/DNS/verification authority; never
   fork a second engine surface.
4. **Add transport/security combinations only where technically and
   legally supportable** — TCP/TLS/WebSocket/HTTP2/gRPC/QUIC/XHTTP/
   REALITY enter as reviewed implementations behind the engine's
   transport abstraction; uTLS-style fingerprinting requires its
   own license/security review before any capability declaration.
5. **Maintain external compatibility only until each capability is
   independently replaced** — the registry, selection, fallback and
   verification machinery already treat `freecore` as "just another
   backend"; keep it that way. Retire an external core only when
   zero shipped capabilities depend on it (per-capability gates in
   ROADMAP.md Phase 2 item 16).

Architectural invariants that must survive Phase 2 (from the rules
above and v0.13.1's architecture addendum): one engine authority, one
connection state machine, one routing authority, one DNS authority,
one TUN authority, one system-proxy authority, one store, one test
queue, one process-supervision boundary — the first-party engine
normally runs in-process and must never grow a private supervisor;
no wholesale copying of Xray/V2Ray/sing-box/Mihomo source; licenses
reviewed before code enters the tree; no capability is claimed
without evidence at the class this document defines.
