# TUN mode

Windows-first system-wide tunneling. The CURRENT dataplane is the
FreeIran first-party TUN (Wintun → the FreeIran userspace IP stack →
engine sessions → the ONE Router → the first-party outbound); the
managed sing-box core's native TUN inbound remains the EXPLICIT
compatibility fallback for configurations beyond the first-party
capability gate. This document is the design + evidence record for
BOTH paths at their current state (v0.14.1); the sing-box path's
v0.11.3/v0.11.4 history (address-selection fail-closed, exact-adapter
activation identity, native route-path observation) and the historical
removal rationale (v0.9.8.6) are preserved in the CHANGELOG and in the
code comments of `engine/tunnel/tun.go`. Future protection work (a
first-party DNS packet path, the WFP kill switch) is PLANNED and lives
in [autonomous-connectivity.md](autonomous-connectivity.md) — none of
it is claimed here.

## Status — the evidence ladder (v0.14.1)

Each class below is stated at exactly the level actually proven;
classes are NOT interchangeable. The first-party backend surfaces the
ladder itself in `TUNSnapshot` (Compiled / PlatformReady / StackReady /
RouteReady / TrafficVerified) — `Active` means the ROUTE-READY rung
and never more.

| Evidence class | State (current release) |
| --- | --- |
| Generated-config verification (sing-box fallback path) | VERIFIED — `TestSingBoxTUNDocumentRealBinary` passes the complete TUN document (dual-stack and IPv4-only shadowsocks, WireGuard endpoint form) through `sing-box check` of the real pinned **1.14.1** binary; unit tests pin every structural invariant |
| Linux/unit verification | VERIFIED — the full `engine/tunnel` + `engine/freecore` + `engine/freecore/tun` suites (fail-closed selection, exact-adapter identity, route verdicts, additive-mutation ownership contracts, upstream-constraint fail-closed matrix, TUN admission lifetime, rollback/residual contracts) run on Linux against deterministic seams |
| Windows compile verification | VERIFIED — `windows/amd64` build + vet pass (CGO off, WebView2 path), including the native IP Helper collectors, the dual-family socket-option encoding tests and the Wintun send-error contract tests |
| Windows CI behavioral verification | EXECUTED BY CI — the Windows job runs the platform test surface (compile-all + behavioral + repeated battery); see docs/ci.md for the v0.11.3 hosted-runner incident and the v0.11.4 bounded-timeout hardening. CI runs NO privileged TUN operation |
| Physical elevated Windows TUN runtime (real Wintun adapter + routes + tunneled application traffic on a physical Windows host) | **NOT VERIFIED** — requires an elevated physical Windows host; no such runtime was executed for this release. FreeIran deliberately does not claim it, for either dataplane |

## The first-party TUN dataplane (v0.14.1 evidence record)

The FreeIran-owned dataplane is the PREFERRED TUN authority for
configurations the FreeIran Engine genuinely supports; selection is
per activation and logged (`tun_backend_selected`):

| Gate the first-party activation verifies | Evidence class reached in v0.14.1 |
| --- | --- |
| Packet path Wintun → userspace stack → engine flow → Router → first-party outbound → remote → reverse path | VERIFIED end-to-end in memory (IPv4 + IPv6, real bytes, `-race`) — `TestTUNDataplaneEndToEndThroughSOCKS5Remote`, `TestTUNDataplaneIPv6ThroughSOCKS5Remote`, `engine/freecore/netstack` integration suite |
| Userspace IP stack behavior (TCP handshake, teardown, RST-on-refusal, bounded flows, counters) | VERIFIED in memory against the pinned gVisor fork (`github.com/sagernet/gvisor@v0.0.0-20250325023245-7a9c0f5725fb`, isolated behind `engine/freecore/netstack`) |
| OS covering routes, BOTH families | VERIFIED at the seam level: the activation installs the IPv4 pair (0.0.0.0/1 + 128.0.0.0/1) AND the IPv6 pair (::/1 + 8000::/1) additively (`CreateIpForwardEntry2`), and `Enable` verifies ownership of every covered route through the dual-stack `MIB_IPFORWARD_ROW2` lookup bound to the TUN's LUID (`VerifyRoutesOwnedByLUID`) — on Windows, by the activation gate itself |
| IP/address mutation ownership | VERIFIED at the seam level: additive-only (`CreateUnicastIpAddressEntry` per address; the family-wide flush primitive is banned from the activation), rollback deletes exactly the session's entries, and foreign (user-created) addresses survive a full enable/disable cycle — `TestApplyInterfaceConfigPreservesForeignState` |
| Windows platform code (Wintun open/send-error surfacing, `winipcfg` address/route mutation + undo, elevation check, dual-family `IP_UNICAST_IF`/`IPV6_UNICAST_IF` binding, context-aware dialing) | COMPILE-VERIFIED + contract-tested (`windows/amd64` build + vet; the socket-option encoding, family selection, blocked-connect cancellation and Wintun send-error suites are Windows test-surface tests) — no elevated Windows runtime executed for this release |
| Activation verification gate actually executed by `Enable` on Windows | adapter observed with the exact FreeIran identity, covered routes owned by FreeIran's interface (both families, LUID-bound), physical upstream present and distinct from the TUN (fail-closed on missing/stale identity), loop-prevention constraint validated before any dial |
| Actual tunneled APPLICATION traffic on a physical Windows host | **NOT VERIFIED** — the Level-5 rung (`TrafficVerified`). A successful first-party activation is NOT claimed as an internet-traffic proof; `TUNSnapshot.TrafficVerified` is never set without a real runtime traffic probe. FreeIran deliberately does not claim it |
| TUN-originated DNS | **NOT IMPLEMENTED (fail-closed, honestly stated)** — there is no first-party DNS packet path through the TUN; DNS resolution semantics are the engine authority's (below) and bootstrap resolution is constrained to the physical interface. No Windows adapter DNS settings are ever mutated |
| UDP through the first-party TUN | **NOT IMPLEMENTED (fail-closed)** — UDP datagrams entering the userspace stack are classified `Unsupported`, counted, and never leaked to the physical interface. SOCKS5 UDP ASSOCIATE remains planned (ROADMAP) |

Loop prevention is a dialer contract (`freecore.NewUpstreamDialer`)
built on an EXPLICIT two-fact constraint model (`UpstreamConstraint`):
`TUNInterfaceIndex` is the FreeIran adapter (recorded, forbidden as a
bind target) and `PhysicalInterfaceIndex` is the live-observed
default-route owner (the bind target). Upstream sockets bind to the
PHYSICAL interface with the family-correct socket option — IPv4
`IP_UNICAST_IF` in network byte order, IPv6 `IPV6_UNICAST_IF` native —
decided from the RESOLVED destination, never from hostname text. The
binding seam is context-aware (`DialContext` end to end): a cancelled
upstream dial stops dialing. When the constraint cannot be honored
(missing physical interface, physical == TUN, platform refusal) the
upstream dial is REFUSED — fail-closed beats a loop. The adapter DNS
settings are never touched by the first-party path; DNS behavior is
defined in the engine's DNS authority section below and in
docs/architecture.md.


## Architecture

TUN is NOT a second packet engine. The EXISTING managed sing-box core
(the one the Cores page installs with pinned-digest verification) is
launched with a TUN-flavored runtime document:

```
user selects TUN
→ verify active/selected configuration (store) + sing-box compatibility (backend.Validate — refused BEFORE any mutation)
→ verify platform (Windows-only in this release) + elevation (Windows token; no UAC prompts from product code)
→ resolve the managed sing-box core (coremgr; digest-verified) + version gate (≥ 1.12)
→ choose the session identity: collision-free adapter name + TUN addresses
    (v0.11.4: FAILS CLOSED when no collision-free IPv4 candidate exists)
→ generate TUN document (engine/core/singbox.BuildTUNDocument)
→ start through the existing process supervisor (core.Launch → system.Start, job objects)
→ wait readiness (the mixed inbound listener — the shared launch verdict)
→ observe the EXACT session adapter (recorded name AND expected address on that same interface)
→ observe the route path (native IP Helper forwarding table: the TUN must own the covering IPv4 routes)
→ verify a REAL tunneled Internet request (clean transport, no explicit proxy)
→ observe upstream pinning (no sing-box TCP socket sourced from the TUN address; positive physical-side pinning when TCP-observable)
→ persist the durable session marker
→ publish TUN Active
```

`Active` is therefore never "the process started": it is "the exact
session adapter was observed, the covering route state was observed,
traffic provably flowed, and no upstream loop was observed".

### The v0.11.4 activation gate (what changed and why)

The v0.11.3 gate accepted two weak observations; v0.11.4 replaces both:

1. **Exact adapter identity** (`findTUNInterface`): the observed
   interface must match the recorded FreeIran adapter NAME and carry
   the session's expected ADDRESS on that SAME interface. The
   v0.11.3 address-first scan accepted the expected address on a
   DIFFERENT adapter (false positive) and the right name WITHOUT the
   address (activation without proof). Both are now rejected
   (`TestFindTUNInterfaceExactIdentity`).
2. **Native route-path observation** (`engine/tunnel/tun_routes.go` +
   `tun_routes_windows.go`): before traffic is trusted, the Windows IP
   Helper forwarding table (`GetIpForwardTable`) must show the TUN
   interface OWNING the covering IPv4 route state — the 0.0.0.0/0
   default route or the 0.0.0.0/1 + 128.0.0.0/1 pair. A probe that
   succeeds while the default route still points elsewhere is no
   longer sufficient evidence.
3. **Upstream pinning / route-loop check**
   (`GetExtendedTcpTable`): after the tunneled request succeeds, the
   sing-box process must own NO TCP socket sourced from the TUN
   address — that sourcing IS the route-loop failure mode
   `route.auto_detect_interface` exists to prevent, and it is a hard
   activation failure. For TCP-based outbounds the same snapshot
   yields POSITIVE pinned-to-physical evidence (a sing-box upstream
   socket sourced from a non-TUN local address). UDP-family outbounds
   (WireGuard/Hysteria2/TUIC/QUIC) hold no TCP upstream socket: the
   verdict honestly reports "not observable through the TCP owner
   table" instead of guessing — the adapter-identity, route and
   traffic gates still fully apply.

The observation layer is READ-ONLY (query APIs only), BOUNDED (single
snapshot per gate, no polling loops), DETERMINISTIC (pure verdict
functions over fact slices) and TESTABLE (the collectors are seam
variables; every verdict and every gate failure mode is unit-tested
on Linux with fakes). No `netsh`, no `route.exe`, no PowerShell, no
shell parsing — anywhere.

All three observations also fail CLOSED on the collection error
itself: if the native table cannot be read, activation refuses to
proceed on unobserved routing state.

### Address selection (v0.11.4: fail-closed)

TUN addresses are chosen at activation time from a small candidate
list (sing-box's documentation default 172.19.0.1/30 first) after
enumerating the LIVE interface table; the first candidate that does
not overlap any existing local network wins. IPv6 is best-effort — a
session runs IPv4-only (and says so) when every IPv6 candidate
collides, which is safe. IPv4 is NOT best-effort: when EVERY IPv4
candidate overlaps a live prefix the enable FAILS with an explicit
error before the core is launched — the v0.11.3 silent fallback onto
a colliding range (a silent misroute of the user's real network into
the tunnel) is removed (`TestPickTUNAddressesFailClosed`).

### The generated document (sing-box 1.12+ semantics)

- `tun` inbound: `interface_name` (collision-free name chosen from the
  live adapter list), `address` (IPv4 always, IPv6 when a
  non-colliding candidate exists), `auto_route: true`,
  `strict_route: true`.
- `mixed` inbound on 127.0.0.1:ephemeral — the readiness probe and a
  local diagnostic endpoint; readiness uses the SAME machinery as a
  normal connection.
- DNS module (current server format): `dns-remote` = DoH 1.1.1.1 with
  `detour: proxy` (hijacked client DNS is answered over the tunnel),
  `dns-local` = system resolver used ONLY as the bootstrap resolver
  for the proxy server's own domain. System adapter DNS settings are
  never read, never written — there is nothing to restore.
- Route: `{"action":"sniff"}` → `{"protocol":"dns","action":"hijack-dns"}`
  → `{"ip_is_private":true,"outbound":"direct"}`, `final: proxy`,
  `auto_detect_interface: true`,
  `default_domain_resolver: dns-local`.

### Loop prevention (the critical invariant)

The upstream proxy connection must never re-enter the TUN.
`route.auto_detect_interface` makes sing-box bind its own outbound
sockets to the detected default physical interface, so the proxy
connection always leaves through the physical NIC regardless of the
TUN's routes. v0.11.4 makes this claim OBSERVABLE at activation time:
the upstream-pinning check above fails the activation if a sing-box
TCP socket is ever sourced from the TUN address, and logs the
positive physical-side pinning evidence when the TCP table exposes
it. No shell route exceptions, no hardcoded interface, no hardcoded
gateway: the dataplane derives the route context from real system
state at runtime.

### Wintun (verified packaging model, no invented digests)

Verified against the ACTUAL official sing-box 1.14.1 windows-amd64
release binary (dependency `github.com/sagernet/sing-tun v0.9.3`):
the TUN inbound creates adapters through sing-tun's internal Wintun
bindings, which EMBED the official wintun.dll inside the sing-box
executable (`internal/wintun/dll_windows_amd64.go`,
`//go:embed amd64/wintun.dll`) and load it FROM MEMORY through the
bundled memory-module loader (`memmod.LoadLibrary(dllContent)`) —
no disk copy is written and no DLL search path is consulted for the
TUN inbound. (The binary also links the upstream
`golang.zx2c4.com/wintun` bindings, which load wintun.dll from the
application directory/System32 — that copy serves the WireGuard
transport path, not the TUN inbound.) The driver installation itself
(wintun.sys) is performed by Windows on demand through the official
Wintun driver installation flow inside the DLL — FreeIran never
installs, extracts or PATH-resolves any Wintun binary. FreeIran
therefore:

- does NOT download Wintun (no curl/wget/PowerShell/Invoke-WebRequest);
- does NOT place or trust a random wintun.dll from PATH;
- does NOT distribute wintun.sys;
- anchors the integrity of the whole dependency set in the managed
  sing-box install (coremgr's release-API digest / sidecar
  verification, fail-closed).

The tunnel snapshot reports `installed` as "the verified dataplane
binary is present" — with the embedded model, that IS the Wintun
dependency state.

## Disable / rollback

```
disable request
→ stop the sing-box dataplane (existing supervisor: graceful stop → kill, job object)
→ wait for process termination
→ cancel the session context
→ poll the interface table until the FreeIran-owned adapter NAME is gone (bounded)
→ consume the durable session marker
→ publish off
```

If the adapter is still present after the dataplane stopped, the call
returns a RESIDUAL error (`tun_residual` event) and the UI shows
status `failed` with the reason — cleanup success is never silently
claimed. The marker is kept in that case so the next boot reports the
stale state. The enable rollback follows the same contract: stop the
supervised instance, verify the owned adapter disappears, and SURFACE
any residual in the composed failure (`TestRollbackEnableSurfacesResidual`).

## Crash recovery

Activation writes a durable marker (`<workspace>/runtime/tun-session.json`)
with the adapter identity; a verified disable consumes it. At boot:

- marker present → inspect ONLY the recorded adapter name/address
  (unrelated adapters are never enumerated for action);
- report `tun_stale_session` in the runtime log with an honest,
  precise detail line — v0.11.4 distinguishes "adapter present WITH
  its session address", "adapter with the recorded name present
  WITHOUT the recorded address" and "adapter already gone";
- never touch unrelated interfaces, routes or DNS; never "reset" the
  network stack; never DHCP-reset an adapter.

## Compatibility honesty

The TUN dataplane is the **sing-box native TUN inbound on Windows** —
nothing else. Concretely:

- Xray/V2Ray configurations are NOT TUN backends and are never
  advertised as TUN-capable; Mihomo being coremgr-managed does NOT
  make it a TUN backend;
- the sing-box compatibility gate (`backend.Validate`) runs BEFORE
  the controller seam and refuses incompatible configurations before
  any system mutation (`TestControllerEnableTUNRefusesBeforeMutation`);
- the sing-box version gate refuses cores older than 1.12.0 (rule
  actions + current DNS format) before launch;
- the UI labels the dataplane "sing-box native TUN (Wintun)" — the
  Connection page never implies "all cores".

## Runtime observability

Structured TUN events (privacy-safe — never keys, passwords, UUID
credentials or raw authenticated URLs):

`tun_starting` → `tun_interface_observed` → `tun_route_observed` →
`tun_traffic_verified` → `tun_upstream_observed` → `tun_active` →
`tun_stop`, with `tun_residual` and `tun_stale_session` for the
honest-failure paths. Polling iterations are deliberately NOT logged.

## UI

Connection page → System integration card: **Direct / System Proxy /
TUN** with Enable TUN / Disable buttons. While active the card shows
interface, IPv4, IPv6 (when used), DNS design, routes design, core
version and the redacted configuration; failures surface the real
error text. TUN is a traffic-routing feature — it is never labelled a
kill switch.

## Security posture

- No `netsh`, no `route add/delete`, no PowerShell network
  configuration, no curl/wget — the dangerous child-process scanner in
  `.github/workflows/security.yml` remains fail-closed for the whole
  product surface.
- TUN requires elevation and REFUSES (honestly) without it; FreeIran
  never triggers a UAC escalation prompt from product code.
- The only allowlist entries in the security workflow are the
  documented ones in docs/security.md; none were weakened for this
  release.

## Planned future work (NOT VERIFIED — not part of the current TUN)

The current TUN activation proves its state at activation time
(transactional, observed). The following are PLANNED future
protections with contracts in
[autonomous-connectivity.md](autonomous-connectivity.md):

- **IPv6 leak proof** — today IPv6 is honestly reported as
  used/unused per session address selection (`IPv6 unavailable` is
  not `IPv6 protected`); continuous leak detection is future work.
- **FreeIran-owned central DNS engine** — today DNS is the sing-box
  TUN document's module (DoH over the proxy + system-resolver
  bootstrap, hijacked); the central authority is future work.
- **WFP kill switch** — the current TUN is a traffic-routing feature,
  NOT a kill switch; the Windows Filtering Platform allow-policy
  state machine is future work.
- **Continuous monitoring** — activation-time proofs today;
  MONITORING/DEGRADED states are future work.

None of the above weakens the evidence ladder in this document; a
future release may only extend it with newly executed evidence
classes.

## First-party TUN foundation (v0.13.1) — device/control plane only, NOT a dataplane

v0.13.1 lays the FreeIran-owned TUN boundary for Phase 2
(ROADMAP.md item 1) without changing what TUN mode does today:

- **What exists now** (`engine/freecore/tun`): the `Device`
  interface with a bounded packet-channel boundary; a deterministic
  FreeIran-owned adapter identity (stable name + GUID derivation,
  ownership-tagged); a transaction/rollback state representation
  (desired state, applied steps, inverse rollback plan); loop-
  prevention metadata (the interface index / route-prefix exclusion
  facts the future upstream dialer must honor so engine upstream
  sockets never route back into the adapter); the Windows Wintun
  adapter/session lifecycle through the reviewed
  `golang.zx2c4.com/wintun` binding (MIT; memory-loaded, no
  downloaded driver); and a read-only IP Helper observation seam
  (interfaces/addresses/routes via `golang.org/x/sys/windows`).
- **What does NOT exist**: any packet forwarding. The first-party
  device layer is not wired to an IP stack, is not selectable in the
  UI, is not reported anywhere as active, and cannot carry traffic.
- **Selection is explicit and unchanged**: the runnable TUN
  dataplane is the managed sing-box native TUN backend
  (`tunBackendName = "sing-box native TUN (Wintun)"`). When the
  Phase 2 dataplane lands, backend selection becomes an explicit,
  evidence-gated choice inside the ONE `TUNBackend` authority —
  never two parallel TUN systems.
- **Evidence class of the foundation**: platform-neutral
  ownership/identity/rollback/loop-guard decisions are unit-tested
  on Linux; the Windows-specific layers (Wintun binding, IP Helper
  collectors) compile in the windows/amd64 cross-build. NO Windows
  runtime execution happened for this foundation in this release —
  the same honesty rule as the evidence ladder above applies.

The Phase 2 implementation contract (purpose, dependencies,
boundary, evidence, fallback, retirement gate for the first-party
dataplane) lives in ROADMAP.md item 1; the long-term protection
architecture (DNS authority, IPv6 proof, WFP kill switch) remains
governed by [autonomous-connectivity.md](autonomous-connectivity.md).
