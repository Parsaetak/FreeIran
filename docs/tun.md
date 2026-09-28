# TUN mode (v0.11.3)

Windows-first system-wide tunneling through the managed sing-box core's
native TUN inbound. This document is the design + evidence record for
the v0.11.3 TUN implementation; the historical removal rationale
(v0.9.8.6) is preserved in the CHANGELOG and in the code comments of
`engine/tunnel/tun.go`.

## Status

| Aspect | State |
| --- | --- |
| Configuration generation | VERIFIED — the generated TUN document passes `sing-box check` against the pinned v1.14.1 binary (both the shadowsocks outbound and the WireGuard endpoint form); unit tests pin every structural invariant |
| Platform code | VERIFIED by build + tests on Linux; Windows/amd64 build passes (CGO off, WebView2 path) |
| Windows TUN runtime (real Wintun adapter + tunneled request on a physical Windows host) | **NOT VERIFIED** — requires an elevated Windows host with the managed sing-box core; no such runtime executed for this release. FreeIran deliberately does not claim it. |

## Architecture

TUN is NOT a second packet engine. The EXISTING managed sing-box core
(the one the Cores page installs with pinned-digest verification) is
launched with a TUN-flavored runtime document:

```
user selects TUN
→ verify active/selected configuration (store) + sing-box compatibility (backend.Validate)
→ verify elevation (Windows token; no UAC prompts from product code)
→ ensure managed sing-box core (coremgr; digest-verified)
→ generate TUN document (engine/core/singbox.BuildTUNDocument)
→ start through the existing process supervisor (core.Launch → system.Start, job objects)
→ observe the actual TUN interface (by configured address, then exact name)
→ verify a REAL tunneled Internet request (clean transport, no explicit proxy)
→ publish TUN Active
```

`Active` is therefore never "the process started": it is "the adapter
was observed and traffic provably flowed".

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
TUN's routes. No shell route exceptions, no hardcoded interface, no
hardcoded gateway: the dataplane derives the route context from real
system state at runtime.

### Address selection

TUN addresses are chosen at activation time from a small candidate
list (sing-box's documentation default 172.19.0.1/30 first) after
enumerating the LIVE interface table; the first candidate that does
not overlap any existing local network wins. IPv6 is best-effort — a
session runs IPv4-only (and says so) when every IPv6 candidate
collides.

### Wintun (no second downloader, no invented digests)

The official sing-box Windows build embeds the official wintun.dll
(via `golang.zx2c4.com/wintun`, the official Wintun Go bindings) and
loads it from the binary at runtime; the driver itself is installed on
demand by the DLL per the official Wintun deployment model. FreeIran
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
→ cancel the session context
→ poll the interface table until the adapter is GONE (bounded)
→ consume the durable session marker
→ publish off
```

If the adapter is still present after the dataplane stopped, the call
returns a RESIDUAL error and the UI shows status `failed` with the
reason — cleanup success is never silently claimed. The marker is kept
in that case so the next boot reports the stale state.

## Crash recovery

Activation writes a durable marker (`<workspace>/runtime/tun-session.json`)
with the adapter identity; a verified disable consumes it. At boot:

- marker present → inspect ONLY the recorded adapter name/address;
- report `tun_stale_session` in the runtime log with an honest detail
  line (adapter still present → the next elevated enable/disable cycle
  completes the cleanup; adapter already gone → only marker residue);
- never touch unrelated interfaces, routes or DNS; never "reset" the
  network stack; never DHCP-reset an adapter.

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
- The only new allowlist entries in this release are the two documented
  in docs/security.md; neither weakens a scan.
