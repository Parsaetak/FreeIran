# Core Acquisition & Trust (v0.12.2)

This document describes how FreeIran acquires, validates and manages
the protocol cores that provide connectivity (Xray, V2Ray, sing-box —
Mihomo is core-manager-managed without a connection adapter).

**v0.12.2 removal note:** the former provider architecture — the
first-class Tor and Psiphon engines in `engine/provider`, the provider
session layer (`engine/connection/provider.go`), the provider service
surface, the Tor/Psiphon settings and their discovery specs — was
REMOVED from the active product. Historical design rationale lives in
CHANGELOG.md (the v0.9.8.x era) and in git history; this document
describes the current product only.

## Design rules

- Executables that provide connectivity are managed through the ONE
  core-manager pipeline (`engine/coremgr`) — one download path, one
  checksum gate, one health model, one supervisor. No second binary
  manager exists anywhere.
- Managed binaries come ONLY from the upstream GitHub Releases of the
  pinned cores (XTLS/Xray-core, v2fly/v2ray-core, SagerNet/sing-box),
  over HTTPS, with a mandatory digest gate: an install is REJECTED
  when a release publishes no authoritative digest (release-API
  digest or `.dgst` sidecar). A locally computed hash is tamper
  evidence, never a trust anchor.
- Every archive extraction is bounded (`internal/safearchive`):
  entry-count, total-size and path-escape limits apply to every
  managed download.
- No arbitrary scripts run; no certificates install; core binaries
  are validated (version probe + optional smoke test) before they are
  activated for connections.
- Each core owns its runtime document (`engine/core/*` compilers).
  Documents are generated deterministically, keyed by the
  configuration fingerprint and cached per backend generation.

## Discovery & adoption

`system.CoreLocator` + `system/execdiscovery.go` probe the platform
search roots (program directories, PATH, the workspace `cores/` tree)
for each core's known binary names and installation subdirectories.
Discovered binaries are version-probed; the registry exposes
availability, version, origin and ownership to the UI (`Cores` page)
and to the tester. Workspace migrations carry the `cores/` tree
forward (v0.11 workspace layout).

## Lifecycle

- Install / update / reinstall / disable flow through the CoreService
  and the core manager; progress is event-driven
  (`freeiran:coreprogress`) with per-stage failure reasons.
- Connection-time supervision is the ONE process supervisor
  (`system.ManagedProcess` + `engine/core.Launch`): startup deadline,
  readiness probe (listener), process-exit watching, graceful stop,
  Windows file-lock discipline. There is no second supervisor.
- Mihomo remains download-managed and health-checked; it has NO
  connection adapter (it is not a runnable protocol-core for
  FreeIran sessions).

## Proxy chains (v0.12.2)

A proxy chain is an ordered list of EXISTING configurations (2–4
hops, index 0 = first hop, last = egress) compiled into ONE core
process:

- Xray: `streamSettings.sockopt.dialerProxy` — the egress outbound is
  tagged `proxy`, earlier hops are `chain-N`, each dialing its
  predecessor; the first hop dials directly.
- sing-box: the outbound `detour` field with the same tag plan.
- V2Ray: refuses chains explicitly (no chaining primitive wired into
  the adapter) — never an emulated multi-process chain.
- Mihomo: not a connection adapter; chains never target it.

Chain sessions run the existing connection state machine and
verification gate; chain records live in the collections sidecar and
reference configuration IDs only. See
[configurations.md](configurations.md) for the workspace surface and
[architecture.md](architecture.md) for the topology model.

## Testing strategy

- Core manager: download/verify/activate against controlled local
  release servers (`engine/coremgr` tests).
- Compilers: generated documents are schema-asserted per backend
  (`engine/core/{v2ray,xray,singbox}` tests), including the chain
  tag plans (`TestXrayChainCompilesHopsInOrder`,
  `TestSingBoxChainCompilesDetourOrder`) and the explicit
  unsupported-core behaviour (`TestV2RayChainExplicitlyUnsupported`).
- Sessions: deterministic fake-core lifecycle tests
  (`engine/connection`) prove state-machine behaviour — including
  chain sessions — without live protocol runtimes.
