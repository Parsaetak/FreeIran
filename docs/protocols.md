# Protocol × core capability matrix (v0.11.0)

This document is the truthful statement of what FreeIran can execute.
Status levels:

- **tested** — verified against the pinned real binary in CI or in
  the real-binary smoke suite (config accepted → process starts →
  listener ready).
- **implemented** — runtime document generation + capability
  declaration exist and are unit-tested, but the real-binary smoke
  did not cover this exact shape in this release cycle.
- **parser-only** — a share link is recognized and importable, but no
  installed core can execute it (shown honestly in the import
  preview as "not runnable").

Traffic-through-tunnel and Internet verification are NOT claims this
matrix makes for any protocol: those run at connect time against the
user's real server through the standard verification gate. CI proves
everything up to listener readiness with controlled local fixtures
and the pinned core binaries — never "connected because a process
launched", never fabricated public-IP evidence.

Pinned core versions (engine/core/versions.go): xray 26.3.27,
v2ray 5.53.0, sing-box 1.14.0.

## Outbound protocols

| Protocol | Parsed | Validated | Xray runtime | V2Ray runtime | Sing-box runtime | Evidence |
|---|---|---|---|---|---|---|
| VLESS (incl. REALITY, vision, ws/grpc/http/httpupgrade) | yes | yes | tested | tested | tested | real-binary smoke suites |
| VMess (ws/grpc/quic/h2) | yes | yes | tested | tested | tested | real-binary smoke suites |
| Trojan (TLS mandatory) | yes | yes | tested | tested | tested | real-binary smoke suites |
| Shadowsocks | yes | yes | tested | tested | tested | real-binary smoke suites |
| SOCKS | yes | yes | tested | tested | tested | real-binary smoke suites |
| HTTP | yes | yes | tested | tested | tested | real-binary smoke suites |
| Hysteria2 | yes (v0.10.2: insecure; v0.10.3: obfs/obfs-password in their own fields) | yes (obfs validated: empty, salamander or gecko) | not supported by this core | not supported by this core | **tested (new in v0.10.2)** | sing-box 1.14.0 real-binary smoke |
| TUIC | yes (v0.10.3: congestion_control + udp_relay_mode in their own fields; v0.10.4: relay domain corrected to native \| quic) | yes (value domains enforced) | not supported by this core | not supported by this core | **tested (new in v0.10.2)** | sing-box 1.14.0 real-binary smoke |
| WireGuard | yes (v0.10.3: INI/URL `wireguard://` reachable, local Address parsed) | yes | not supported by this core | not supported by this core | **tested (new in v0.10.2, endpoint form; v0.10.3: endpoint always carries a local address)** | sing-box 1.14.0 real-binary smoke |
| Hysteria (v1) | yes (v0.10.2: up/down; v0.10.3: obfs fields; v0.10.4: obfs is the v1 password string) | yes (obfs generated as the documented JSON string; omitted when absent) | not supported by this core | not supported by this core | **tested (new in v0.10.2)** | sing-box 1.14.0 real-binary smoke |

Notes on the QUIC family and WireGuard (all verified against the real
pinned binary with `sing-box check` and smoke startup):

- Hysteria2 / TUIC / Hysteria are TLS-mandatory in sing-box; the
  adapter always emits `tls.enabled = true` for them. `insecure=1`
  from the URI is honored (surfaced as a warning in the import
  preview).
- Hysteria (v1) REQUIRES `up_mbps`/`down_mbps` — the binary refuses
  the outbound otherwise ("missing upload speed"). Import/validate
  reject configurations without them, with the reason.
- WireGuard uses the sing-box ENDPOINT form (`"type": "wireguard"` in
  `endpoints`; the `"wireguard"` OUTBOUND was removed in sing-box
  1.11). Both local keys are required at validate time. The LOCAL
  interface address (INI `Address` / URL `address`) maps to the
  endpoint's `address` — the real binary requires it at startup;
  when a configuration omits it, the generator emits a deterministic
  FreeIran-generated fallback pair (172.19.0.2/32 +
  fdfe:dcba:9876::2/128). This fallback is FreeIran's own choice for
  reproducible documents — it is NOT a sing-box "documented default"
  (sing-box only requires SOME local address).
  `AllowedIPs` is the PEER routing list
  and defaults to `0.0.0.0/0` + `::/0`; it never carries interface
  addresses.
- v0.10.4 field semantics: TUIC `udp_relay_mode` is native | quic (the
  v0.10.3 "quadratic" was an invented value, never documented by any
  sing-box release); Hysteria2 `obfs` is salamander | gecko (gecko was
  wrongly rejected in v0.10.3) and generates the object
  `{"type", "password"}`; Hysteria (v1) `obfs` is the obfuscation
  PASSWORD string — a different protocol with a different sing-box
  schema — and generates the JSON string, omitted when absent. The
  v0.10.4 semantic variants (TUIC relay/congestion matrix, Hysteria2
  salamander + gecko, Hysteria v1 obfs string, WireGuard endpoint)
  were all passed through the real pinned sing-box 1.14.0 binary
  (`sing-box check` + startup + listener readiness).
- v0.10.3 field semantics: `obfs`/`obfs-password` are Hysteria's own
  fields (never the TLS security slot or the WS host slot); TUIC
  `congestion_control` (bbr | cubic | new_reno) and `udp_relay_mode`
  are TUIC's own fields and never touch the transport slot (v0.10.2
  overloaded congestion_control into Network, which the capability
  matcher read as a transport name).
- WireGuard execution additionally depends on the sing-box build
  tags `with_wireguard`/`with_gvisor` — present in every official
  release binary the core manager installs.

## Encrypted Client Hello — ECH (v0.11.0)

ECH is a TLS-layer feature. FreeIran models it with four dedicated
fields (`ech_enabled`, `ech_config`, `ech_config_path`,
`ech_query_server_name` → sing-box `tls.ech.{enabled, config,
config_path, query_server_name}`); nothing overloads Host, Network,
Security, FingerprintProfile or SpiderX. ECH is NOT part of the
configuration fingerprint (same rationale as ALPN/Insecure: TLS-layer
tuning, not identity).

Evidence table — every row was probed against the PINNED real
binaries before a single line of support was declared:

| Case | sing-box 1.14.0 | xray 26.3.27 | v2ray 5.53.0 |
|---|---|---|---|
| `ech.enabled` + `ech.config` (PEM `-----BEGIN ECH CONFIGS-----`) | **accepted** (check + startup) | `echConfigList` field exists in schema but content is NOT validated by `xray run -test` | field silently IGNORED (wrong-typed value passes `v2ray test`) |
| `ech.config` raw base64 (no PEM envelope) | rejected: "invalid ECH configs pem" | — | — |
| `ech.config` PEM body not valid base64 | rejected: "invalid ECH configs pem" | — | — |
| PEM header spellings `ECH CONFIG` / `ECHCONFIG` | rejected | — | — |
| `ech.config_path` (file with the PEM) | **accepted** (check + startup) | — | — |
| `ech.query_server_name` (DNS HTTPS discovery) | **accepted** (check + startup) | — | — |
| `ech.enabled` alone (no config/path/name) | **accepted** — upstream DNS-discovery default | — | — |
| ECH + REALITY on one TLS object | rejected: "Reality is conflict with ECH" | — | — |
| ECH + uTLS fingerprint | **accepted** | — | — |
| ECH on the QUIC family (hysteria2, TLS-mandatory) | **accepted** | — | — |

Declared capability: **sing-box only.** The capability matcher routes
ECH-enabled configurations exclusively to sing-box; handing them to
Xray (whose field exists but is unverifiable at check level) or V2Ray
(which ignores it) would connect WITHOUT the encrypted client hello
the user configured — silently discarding a privacy feature is a lie
FreeIran does not tell. The core cards and the configuration detail
view surface this fact.

FreeIran-side validation encodes the same verified rules: ECH
requires TLS security (rejected on plaintext protocols and on
WireGuard, which has no TLS layer); REALITY + ECH is rejected before
generation; `ech.config` XOR `ech.config_path` (both set is
ambiguous); the config must be a PEM `ECH CONFIGS` block with a
base64 body. Normalization accepts the raw base64 ECHConfigList (the
DNS HTTPS record `ech=` payload shape) and wraps it into the PEM
envelope the pinned binary requires.

**Evidence scope — do not upgrade:** "accepted" means the pinned
sing-box 1.14.0 PEM-parses and base64-decodes the config at `check`
time and the core completes a full startup + listener-ready cycle
with the ECH object present (all three shapes ride the real-binary
smoke suite in CI). It does NOT mean live ECH negotiation with a real
ECH-capable server was observed, and nothing in the UI or docs claims
censorship resistance from ECH. The ECHConfigList BYTES are validated
by the core at connection time, not at check time (a syntactically
valid envelope with a structurally invalid list passes `check`) —
FreeIran's own validation therefore checks the envelope form exactly
as the core does, no more.

Import surface: ECH enters through the structured/JSON representation
(`ech_enabled`, `ech_config`, `ech_config_path`,
`ech_query_server_name` on the universal config object). The URI
share formats carry NO standardized ECH parameter — the ecosystem has
not agreed on one — and FreeIran does not invent one (a fake `ech=`
URI parameter is pinned as NOT parsed by test).

## Failure classification (v0.11.0)

Test/verification failures are classified into a stable nine-class
vocabulary derived from observations only: dns, tcp, tls, handshake,
listener, verify, reset, timeout, transport. The class is derived
from the failed-facet shape (handshake metrics, ping all-timeout),
the URL-test's own canonical vocabulary (Timeout flag; "HTTP <code>
via tunnel" → verify), and the error text — never from invented
signals, and the URL-test phase TIMINGS are explicitly not used as
failure evidence (they do not mark the failing phase). The most
recent failure class is stored per configuration
(`last_failure_class`) and per history observation, and repeated
protocol-specific streaks (tls/handshake/transport) demote a
candidate in automatic selection so a different already-supported
transport is preferred (see docs/architecture.md).

## Payload formats (personal import)

| Format | Detected | Notes |
|---|---|---|
| URL list (one share link per line) | yes | mixed protocols allowed |
| base64 subscription | yes | decoded, then parsed as a URL list |
| v2ray-style JSON | yes | outbound → config conversion |
| WireGuard INI (`[Interface]`/`[Peer]`) | yes | importable without any subscription source |
| FreeIran structured JSON (single object or array) | yes | the universal config representation; v0.11.0: carries the ECH fields |

## What this matrix does NOT claim

- No "VPN works everywhere" claim: reachability and handshake success
  depend on the user's server, network and censorship environment.
- CI smoke proves local acceptance/readiness with controlled
  fixtures; it does not contact user servers or fabricate Internet
  evidence.
- A protocol the installed core cannot execute is displayed as "not
  runnable" in the import preview — never silently saved as
  connectable.
