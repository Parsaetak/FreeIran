# Internet Tools (v0.9.8.1, extended in v0.9.8.5)

This document describes the shared Internet-Tools engine (§5 of the
upgrade specification): ONE bounded, cancellable, structured tool
architecture for every user-triggered network diagnostic. The engine
lives in `engine/netcheck` (tools.go, tools_impl.go, tools_path.go,
toolsafety.go) and extends the existing connectivity-diagnostics
package instead of spawning unrelated services. The app service
(`engine/app/internettools.go`) exposes it to the UI.

v0.9.8.5 adds three surfaces on the same engine: the DNS diagnostic
(`dnsdiag.go`), the Network Identity check (`identity.go`) and the
staged connectivity ladder (`stages.go`).

## The result contract

Every tool execution — regardless of tool — produces one
`ToolResult`:

```text
tool_id / target / started_at / finished_at / duration_ms /
status / transport / path (direct|tunneled) / provider /
measurement / error / details
```

- **Timeouts** are clamped to a bounded window: minimum 1 s, per-tool
  default (10 s standard; 20 s internet; 30 s traceroute; 15 s
  path-mtu/public-ip), hard cap 60 s. Cancellation is honoured at
  every stage.
- **Statuses**: `ok`, `failed`, `timeout`, `cancelled`,
  `invalid_target` (the safety layer rejected the target before any
  bytes left the machine), `unsupported` (honest capability report —
  see below).
- **Measurements** (`ToolMeasurement`) carry the v0.9.8.1 canonical
  latency semantics (`engine/tester/latency.go`): `Measured` is the
  authority, 0 ms + `Measured` = sub-millisecond, `SubMS` marks
  "< 1 ms" round trips. All values are credential-free.

## Tool catalogue

| Tool | Group | Transport | Default target | Notes |
|------|-------|-----------|----------------|-------|
| `internet` | connectivity | aggregate | builtin probe set | the classic connectivity report (bounded sub-probes); v0.9.8.5: carries the staged ladder evidence (below) |
| `dns` | connectivity | UDP/TCP 53 | system + curated resolvers | v0.9.8.5 DNS DIAGNOSTIC: system vs Cloudflare/Google/Quad9, A + AAAA, UDP with TCP fallback; per-resolver rows with transports, latencies, answer counts and honest failure classes (structured evidence in `result.dns`) |
| `tcp` | connectivity | TCP | `1.1.1.1:443` | raw reachability + RTT |
| `tls` | connectivity | TLS | `www.gstatic.com:443` | TCP + TLS handshake; negotiated version/cipher reported |
| `https` | connectivity | HTTP(S) | `https://www.gstatic.com/generate_204` | full GET with redirect/size caps |
| `http_connect` | protocol | HTTP proxy CONNECT | `http://127.0.0.1:1080` | local proxy reachability |
| `socks5` | protocol | SOCKS5 | `127.0.0.1:1080` | RFC 1928 CONNECT round trip |
| `websocket` | protocol | WS/WSS | `wss://echo.websocket.events` | upgrade handshake |
| `udp` | protocol | UDP | `1.1.1.1:53` | datagram round trip (DNS query payload) |
| `quic` | protocol | QUIC | — | **honestly unsupported**: no QUIC stack is compiled into this build; reports `unsupported` and points at the UDP tool. Nothing is claimed that was not verified. |
| `traceroute` | path | raw ICMP TTL walk | `1.1.1.1` | **privilege-gated**: raw ICMP sockets require administrator privileges; reports the honest "requires administrator privileges" error (`unsupported`) instead of pretending when the socket cannot be created. |
| `path_mtu` | path | DNS payload ladder | `1.1.1.1:53` | bounded unfragmented-payload ladder, honestly labelled: DNS QNAME encoding caps the ladder at ~300 bytes — verifying the full 1500-byte MTU would require privileged raw-socket probing. |
| `captive_portal` | path | HTTP | builtin portal probe set | portal detection with redirect reporting |
| `public_ip` | identity | HTTPS | builtin identity endpoints | exit IP direct vs through the active tunnel, with match verdict |
| `tunnel_diagnostics` | tunnel | measured | active tunnel | live truth about the active provider/core session (endpoints, health, measured latency) |

The catalogue with labels, groups, target-taking flags and default
timeouts is served to the UI through `ToolCatalogue()`; the Network
page renders it as a grouped grid (`frontend/src/pages/Network.tsx`).

## DNS diagnostic (v0.9.8.5)

The `dns` tool is a resolver COMPARISON, not a single lookup. One run
produces one row per resolver (the system resolver plus the curated
public set — Cloudflare, Google, Quad9 — or one explicit
user-supplied public resolver), each with an A and an AAAA query:

```text
System       A     24 ms   2 answers   OK
             AAAA  27 ms   2 answers   OK
Cloudflare   A     18 ms   2 answers   OK  (udp)
             AAAA  20 ms   2 answers   OK  (udp)
```

- **Transports**: the explicit-resolver queries speak the RFC 1035
  wire protocol directly — UDP first, one TCP fallback on a
  truncation (TC bit) or a UDP transport failure; the transport that
  produced each answer is recorded. System rows use the OS
  resolver API.
- **Failure classes**: timeout / refused / SERVFAIL / NXDOMAIN /
  empty_answer / malformed / resolver_unreachable / cancelled /
  invalid_target — never one generic failure string.
- **Safety**: query names are validated before any bytes leave the
  machine (IP literals, URLs, empty labels, over-long names and
  invalid characters are rejected); user-supplied resolvers must be
  PUBLIC IP literals (private/loopback resolvers are blocked by the
  private-target policy); responses are bounded (4 KiB) and parsed
  with bounded compression-pointer loops; no DNSSEC claim is made —
  this is a plain diagnostic query engine.
- **Tunneled runs**: when the tool runs through the active tunnel,
  the resolver queries travel through the supplied dialer; the
  SYSTEM resolver row is replaced by the curated public set (a
  system-resolver query would silently bypass the tunnel).

## Network Identity (v0.9.8.5)

The `NetworkIdentity` service method (Network tab, top card) answers
"who am I on this network right now" in one bounded check:

- **Local IP** — the route-relevant local IPv4 (and IPv6) plus the
  owning interface, discovered through connected-UDP route lookups
  that send NO packets; additional active non-loopback addresses are
  listed honestly.
- **Public IP** — through the same bounded identity endpoints the
  `public_ip` tool established; a tunneled run measures the tunnel
  exit AND the direct exit, with a match comparison.
- **ISP / ASN / country** — documented keyless HTTPS metadata
  sources (ipinfo.io primary, ipwho.is fallback). Unavailable
  metadata reports Unknown — never fabricated. The request reveals
  only the caller's public IP (the fact the public-IP check already
  measures); no configuration, credentials or proxy URLs are
  transmitted.
- **Explicit user action only** — never automatic; a 60-second
  response cache only prevents hammering the endpoints on page
  re-renders, and only serves the same question (direct vs
  tunneled) the user last asked.

## Staged connectivity ladder (v0.9.8.5)

The `internet` check's report now carries ordered stage evidence —
local link → local IP → DNS → TCP → TLS → HTTPS → captive portal →
direct Internet → tunnel Internet — each rung with status
(ok/failed/skipped/not_checked), measured latency, target and a
failure class, plus `failed_stage` naming the first broken rung. The
seven-state classifier remains authoritative; the ladder explains
it (`engine/netcheck/stages.go`).

## Safety policy (`engine/netcheck/toolsafety.go`, §7)

Every tool obeys the same hard rules:

- **Target validation before any bytes leave the machine**: URL
  scheme allowlist per tool family (`http`/`https`, `ws`/`wss`); URLs
  carrying credentials (userinfo) are rejected outright — credentials
  never ride a diagnostic URL; port bounds.
- **Private / link-local / loopback destinations are blocked by
  default for every generic diagnostic** (tcp, tls, https, websocket,
  dns, udp, traceroute, path_mtu). Private-target permission is
  EXPLICIT and tool-scoped: only the two genuinely local-endpoint
  tools — `socks5` and `http_connect`, whose defaults are
  `127.0.0.1` and whose purpose is testing the user's own local
  proxy — may target private addresses implicitly. Intentional local
  testing with a generic tool requires the explicit
  `AllowPrivateTargets` capability. (This is the v0.9.8.1
  Windows-CI root fix: the earlier policy treated generic tools as
  implicitly private-target-safe, which bypassed the rebinding
  guard.)
- **DNS-rebinding guard for direct probes**: the hostname is
  resolved, every answer is validated against the policy, and the
  connection is dialed to a validated address
  (resolve → validate → pin). A hostname resolving only to
  blocked ranges is refused before any connection.
- **Redirects are capped** at `MaxRedirects` (default 3) and every
  hop's destination is re-validated against the same no-credentials
  and private-destination policy as the initial target — on the
  direct path (where the dialer additionally guards at connection
  time) AND on the tunneled path (where the proxy resolves
  remotely, so hop validation is the guard). A redirect chain can
  never be steered onto a loopback/private endpoint.
- **Response bodies are capped** at `MaxResponseBytes`
  (default 256 KiB); bytes beyond the cap are neither read nor
  stored.
- **User-triggered ONLY**: no tool — especially public-IP lookups —
  ever runs automatically at startup or in the background; the
  service layer enforces this.
- **Bounded concurrency**: at most 3 tools run at once in the app
  service (`toolConcurrency = 3`); a fourth request fails honestly
  with "too many tools running concurrently" instead of queueing
  unbounded.
- Remote JavaScript is never executed and configuration credentials
  are never transmitted.

## Tunneled execution

When a tunnel (provider or core session) is active, tools can run
with `path = tunneled`: the runner's dialer routes through the active
session's local SOCKS endpoint, and the result names the provider.
The direct path keeps the DNS-rebinding guard; the tunneled path
delegates resolution to the remote endpoint (the proxy resolves
remotely — same documented semantics as the URL tester). The UI
offers the via-tunnel toggle only while a tunnel is active.

## Events

Every run emits structured, credential-free events through
`internal/logging`:

- `network_tool_start` (debug) — tool, timeout, path, provider;
- `network_tool_complete` (info) / `network_tool_failed` (warn) —
  tool, status, `duration_ms`, target and `error_kind` (fixed classes:
  timeout, refused, reset, tls, dns, unsupported, …).

Targets in events are the validated host:port / URL forms — never
credentials, UUIDs or subscription URIs.

## Testing

`engine/netcheck/tools_test.go` drives the catalogue, the result
contract, the timeout clamping ([1 s, 60 s]), the safety layer
(scheme allowlist, credential rejection, private-range blocking,
rebinding guard, redirect and size caps) and the honest
`unsupported`/`invalid_target` paths against local listeners and
`httptest` servers — no external infrastructure. Latency values in
results follow the canonical semantics and are covered by
`engine/tester/latency_test.go` (see docs/latency.md). The v0.9.8.5
surfaces have their own deterministic matrices: `dnsdiag_test.go`
(14 tests against a local fake RFC 1035 resolver over real
UDP/TCP sockets), `identity_test.go` (12 tests with local fake
identity endpoints and a relaying SOCKS proxy) and `stages_test.go`
(5 ladder tests).
