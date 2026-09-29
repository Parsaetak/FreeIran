# TUN mode (v0.11.4)

Windows-first system-wide tunneling through the managed sing-box core's
native TUN inbound. This document is the design + evidence record; the
v0.11.3 implementation was hardened in v0.11.4 (address-selection
fail-closed, exact-adapter activation identity, native route-path
observation) and the historical removal rationale (v0.9.8.6) is
preserved in the CHANGELOG and in the code comments of
`engine/tunnel/tun.go`.

## Status — the evidence ladder

Each class below is stated at exactly the level actually proven;
classes are NOT interchangeable.

| Evidence class | State (v0.11.4) |
| --- | --- |
| Generated-config verification | VERIFIED — `TestSingBoxTUNDocumentRealBinary` passes the complete TUN document (dual-stack and IPv4-only shadowsocks, WireGuard endpoint form) through `sing-box check` of the real pinned **1.14.1** binary; unit tests pin every structural invariant |
| Linux/unit verification | VERIFIED — the full `engine/tunnel` suite (fail-closed selection, exact-adapter identity, route verdicts, pinning verdicts, activation evidence matrix, rollback/residual contracts) runs on Linux against deterministic seams |
| Windows compile verification | VERIFIED — `windows/amd64` build + vet pass (CGO off, WebView2 path), including the native IP Helper collectors |
| Windows CI behavioral verification | EXECUTED BY CI — the Windows job runs the platform test surface (compile-all + behavioral + repeated battery); see docs/ci.md for the v0.11.3 hosted-runner incident and the v0.11.4 bounded-timeout hardening. CI runs NO privileged TUN operation |
| Physical elevated Windows TUN runtime (real Wintun adapter + route + tunneled request on a physical Windows host) | **NOT VERIFIED** — requires an elevated Windows host with the managed sing-box core; no such runtime was executed for this release. FreeIran deliberately does not claim it |

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
