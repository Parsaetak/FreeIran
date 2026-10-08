# FINAL — FreeIran is FINAL / FROZEN

**Designation:** `v0.final`
**Implementation baseline:** `0.14.2` (the machine-readable version in
[`VERSION`](VERSION); all build, updater and resource contracts require
the numeric form — `v0.final` is the human-facing final designation
only)
**Finalization date:** 2026-10-07

This is the canonical freeze statement. It deliberately does not
duplicate the product description: the current product is documented by
[README.md](README.md) and the technical docs
([docs/README.md](docs/README.md) indexes authority); release history
lives in [CHANGELOG.md](CHANGELOG.md).

## What "final" means

- The `v0.14.2` implementation is the product baseline. The final
  release was a **functional finalization + freeze**, not a feature
  cycle: no new runtime features were added at freeze time.
- Advertised capabilities were inventoried against the implementation
  and exercised to the maximum extent verifiable in the available
  environments; genuine defects found during that verification were
  repaired (the v0.11.3-era stale Windows TUN test contracts, the
  Security static-analysis embed staging, two missing precise Gitleaks
  historical-exception entries).
- The phased roadmap is **closed**. [ROADMAP.md](ROADMAP.md) is an
  archived historical record; the future-design research in
  [docs/autonomous-connectivity.md](docs/autonomous-connectivity.md)
  is **ARCHIVED / HISTORICAL RESEARCH** — not upcoming work. Nothing
  in this repository presents unfinished FreeIran work as an upcoming
  phase.
- Post-final work is maintenance (correctness, reliability,
  environment repairs) unless the project is intentionally reopened.

## Final scope (verified capabilities)

The frozen product's actual capabilities are exactly those documented
in the README ("Current verified capabilities") and detailed in the
technical docs — first-party in-process engine (SOCKS/HTTP and
Shadowsocks-AEAD routes; first-party Windows TUN dataplane for
configurations inside its capability gate), managed Xray/V2Ray/sing-box
external cores as the explicit compatibility fallback, multi-level
discovery and ingestion, ping/URL test modes with measured ranking,
verified-connection engine, System Proxy (WinINet) and Windows TUN
tunnel modes, proxy chains compiled into one core process,
configuration workspace, and the Windows CI verification architecture.

## Known limitations and evidence boundaries (honest)

- The first-party engine forwards local proxy traffic for
  SOCKS/HTTP-over-plain-TCP remotes only; every other protocol,
  transport and security combination runs through the external cores.
- The Windows TUN physical runtime on an elevated physical host is
  **NOT verified**: evidence stops at the classes recorded in
  [docs/tun.md](docs/tun.md) (generated-config, Linux/unit,
  Windows-compile, Windows-CI). Application traffic crossing the TUN
  has not been proven by a physical runtime traffic probe and is never
  claimed — `TrafficVerified` stays false by design.
- UDP through the first-party TUN is fail-closed (not implemented).
- TUN is a traffic-routing feature, not a kill switch; no WFP
  firewall layer exists; no FreeIran-owned central DNS engine.
- Unsupported configurations are refused fail-closed; failures are
  surfaced honestly and never converted into simulated success.

## Related repositories

An independent project with a similar purpose exists at
[mlmvpn/mlmvpn_windows](https://github.com/mlmvpn/mlmvpn_windows). It
is a separate repository with its own ownership and goals. It is not a
FreeIran successor, fork or continuation: FreeIran development did not
move to, continue in, merge into, or get transferred to it. FreeIran
mentions it only as a neutral reference for users seeking an
independent project with similar goals.
