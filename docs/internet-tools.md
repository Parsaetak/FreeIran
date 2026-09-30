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
- **Statuses** (v0.12.1 — the exact, authoritative list):
  - `ok` — the requested measurement completed successfully;
  - `partial` — meaningful successful evidence exists while one or
    more subchecks failed (e.g. two curated resolvers answered, the
    system resolver did not); the successful measurements are
    retained;
  - `failed` — the measurement was applicable and configured, but the
    actual operation failed (refused, reset, protocol error,
    HTTP >= 400 ...);
  - `timeout` — the run hit its configured deadline;
  - `cancelled` — the caller cancelled the context;
  - `invalid_target` — the safety layer rejected the target before
    any bytes left the machine;
  - `unsupported` — the capability genuinely cannot be executed by
    this build/platform/context (a privilege gap, an IPv6-only
    target on an IPv4 walker, QUIC through a SOCKS tunnel). Never a
    lazy placeholder for an unfinished implementation;
  - `not_configured` — the tool requires a local proxy / endpoint /
    configuration and none exists (a missing prerequisite is an
    environment state, NOT a network failure);
  - `not_applicable` — the tool cannot meaningfully run in the
    selected context (tunnel diagnostics on the Direct path);
  - `unreachable` — the tool itself ran, but the target/path was
    explicitly observed to be unreachable (ICMP "destination
    unreachable", a hop chain that provably stops, a silent QUIC
    path). The diagnostic implementation worked; the path did not.

  The UI renders `not_configured`, `unsupported` and `not_applicable`
  in calm info/neutral bands — never red failure styling.
- **Measurements** (`ToolMeasurement`) carry the v0.9.8.1 canonical
  latency semantics (`engine/tester/latency.go`): `Measured` is the
  authority, 0 ms + `Measured` = sub-millisecond, `SubMS` marks
  "< 1 ms" round trips. All values are credential-free.

## Tool catalogue

| Tool | Group | Transport | Default target | Notes |
|------|-------|-----------|----------------|-------|
| `internet` | connectivity | aggregate | builtin probe set | the classic connectivity report (bounded sub-probes); v0.9.8.5: carries the staged ladder evidence (below) |
| `dns` | connectivity | UDP/TCP 53 + DoH | system + curated resolvers | v0.9.8.5 DNS DIAGNOSTIC: system vs Cloudflare/Google/Quad9, A + AAAA, UDP with TCP fallback; per-resolver rows with transports, latencies, answer counts and honest failure classes (structured evidence in `result.dns`). v0.12.1: query-level aggregation — some answers + some failures → `partial` (successful measurements retained); all applicable resolvers failing → `failed`; plus one bounded encrypted-DoH comparison row (RFC 8484 JSON, inside the same diagnostic — a hijacked plaintext path beside a working encrypted path is real censorship evidence). |
| `tcp` | connectivity | TCP | `1.1.1.1:443` | raw reachability + RTT |
| `tls` | connectivity | TLS | `www.gstatic.com:443` | TCP + TLS handshake; negotiated version/cipher reported |
| `https` | connectivity | HTTP(S) | `https://www.gstatic.com/generate_204` | full GET with redirect/size caps |
| `http_connect` | protocol | HTTP proxy CONNECT | none — resolved per run (v0.12.1) | target resolution: explicit user target → configured local HTTP inbound (settings `local_http_port`; the SOCKS session endpoint is NOT protocol-compatible) → `not_configured`. A missing proxy renders "Not configured", never a red failure. |
| `socks5` | protocol | SOCKS5 | none — resolved per run (v0.12.1) | RFC 1928 CONNECT round trip. Target resolution: explicit user target → the ACTIVE session endpoint (a local SOCKS listener) → `not_configured`. No blind `127.0.0.1:1080` probe. |
| `websocket` | protocol | WS/WSS | bounded curated set (2 endpoints) | upgrade handshake. Default runs probe the curated set and aggregate: all succeed → `ok`, mixed → `partial` (per-target evidence kept), all fail → `failed`. An explicit target answers exactly that target (one probe, no fallback). |
| `udp` | protocol | UDP | `1.1.1.1:53` | datagram round trip (DNS query payload) |
| `quic` | protocol | QUIC v1 | `www.cloudflare.com:443` (verified HTTP/3 endpoint) | **v0.12.1: a real bounded handshake probe** (HTTP/3 ALPN) built on `quic-go` — the minimal dedicated diagnostic dependency, measurement only, never a dataplane. Resolve→validate→pin honours the private-range policy; silence → `unreachable`; active-but-failed handshake → `failed`; completion → `ok` with ALPN/TLS/cipher details. Direct path only (SOCKS relays no UDP → `unsupported` when tunneled). |
| `traceroute` | path | ICMP TTL walk | `1.1.1.1` | **v0.12.1: native Windows walker** — IP Helper ICMP API (`IcmpSendEcho` with per-probe TTL), user-mode, no elevation, no tracert.exe/route.exe/PowerShell/shell parsing; Unix keeps the raw-ICMP walker (privilege-gated → honest `unsupported` when refused). Status semantics: explicit destination-unreachable evidence → `unreachable`; hops observed then silence → `unreachable` WITH hop evidence; complete route → `ok`; no usable walker → `unsupported`. Hops are never fabricated. |
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
- **v0.12.1: one DISTINCT completion event per status class** —
  `network_tool_complete` (ok), `network_tool_partial`,
  `network_tool_failed`, `network_tool_timeout`,
  `network_tool_cancelled`, `network_tool_invalid`,
  `network_tool_unsupported`, `network_tool_not_configured`,
  `network_tool_not_applicable`, `network_tool_unreachable`. Every
  event carries tool, status, `duration_ms`, target and
  `error_kind` (fixed classes: timeout, refused, reset, tls, dns,
  unsupported, not_configured, not_applicable, unreachable, ...).
  Severity: environment facts (partial / not_configured /
  not_applicable / unsupported / cancelled) log at info; unreachable
  and failed warn. The old contract — every non-OK outcome collapsed
  into `network_tool_failed` — mislabeled environment states as
  failures and is fixed by `TestToolStatusEventMapping`.

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

---

## v0.11.2 addendum — honest tunnel diagnostics routing

`engine/netcheck/tools_path.go` `runTunnelDiagnostics` now reports
the routing context precisely:

- **Direct path selected** → `result.Error = "No tunnel selected
  (Direct path). Switch the route to Tunnel and run again."`. The
  `Transport` field is cleared so the UI cannot mistake this for a
  successful socks5 probe. `result.Path == "direct"`.
- **Tunnel path selected, no active tunnel** → `result.Error =
  "Tunnel unavailable: no active tunnel"`. `result.Path ==
  "tunneled"` (the failure does not silently downgrade the path).
- **Tunnel path selected, snapshot Active, endpoint empty** →
  `result.Error = "Tunnel unavailable: active tunnel reports no
  local endpoint"`.
- **Tunnel path selected, SOCKS5 CONNECT failure** → `result.Error
  = "Tunnel unavailable: local endpoint failed SOCKS5 CONNECT:
  <underlying error>"` with the precise underlying error.
- **Active tunnel + endpoint, CONNECT succeeds** → `result.Status
  == "ok"`, `result.Measurement.Measured == true`, real measured
  latency.

The Run() guard in `engine/netcheck/tools.go` carves out
`ToolTunnelDiagnostics` from the generic "no active tunnel" early-
return so the tool itself can produce the precise failure classes
above instead of a generic "unsupported". The v0.11.0 contract that
"process exists / SOCKS endpoint exists / UI says Connected" never
implies tunnel success is preserved.

---

## v0.11.3 addendum — tunnel diagnostics for the TUN mode

The Internet-Tools tunnel diagnostics keep their trust policy (never
infer tunnel success from process existence, endpoint existence or UI
state). The v0.11.3 TUN mode is compatible by construction: its
snapshot reports OBSERVED state only (interface by address, verified
tunneled request), and the tools' live-tunnel view continues to
classify per routing class — TUN adds a routing class, not a new
trust shortcut.

---

## v0.12.1 addendum — truthful status semantics and the runtime log

The v0.12.0 runtime log exposed six tools mislabelled as failures and
a `core_discovered` provenance leak. v0.12.1 repairs both:

- The ten-state model above is the authority. Two rules govern it:
  **a missing prerequisite is an environment state** (`not_configured`
  / `not_applicable` / `unsupported`), and **a measured path verdict
  is evidence** (`unreachable`), never an implementation crash.
- HTTP CONNECT and SOCKS5 resolve their target through the documented
  priority (explicit target → protocol-compatible local endpoint →
  `not_configured`); the runner never guesses an endpoint.
- QUIC performs a real handshake (quic-go, measurement-only
  dependency). UDP merely being answerable is NOT "QUIC works" — the
  UDP tool already measures datagram reachability separately.
- Traceroute distinguishes privilege-gap `unsupported` from measured
  `unreachable` and keeps observed hop evidence.
- The normal runtime log contains no core build provenance:
  `core_discovered` prints the compact form ("xray 26.3.27
  available"); full probe lines (commit hashes, `go1.x` tuples,
  paths) are developer diagnostics at debug severity
  (`core_discovered_details`). `compactCoreVersion` accepts
  digits-and-dots tokens only.
