# Documentation index

Entry point for developers and AI agents working on FreeIran. Each
domain has exactly ONE authoritative document.

## Authority map

| Domain | Authoritative document |
|---|---|
| Current architecture (what exists today) | [architecture.md](architecture.md) |
| **Long-term architecture / future-work contract** | [autonomous-connectivity.md](autonomous-connectivity.md) |
| Protocol × core capability evidence | [protocols.md](protocols.md) |
| Core acquisition & trust (managed cores; v0.12.2 chains) | [providers.md](providers.md) |
| TUN implementation + evidence ladder | [tun.md](tun.md) |
| Security contract / scanner evidence | [security.md](security.md) |
| UI architecture + surface contracts | [ui.md](ui.md) |
| Verification architecture (CI/release/security) | [ci.md](ci.md) |
| Development workflow / toolchain policy | [development.md](development.md) |
| Workspace layout | [workspace.md](workspace.md) |
| On-disk storage format | [storage-format.md](storage-format.md) |
| Performance budgets | [performance.md](performance.md) |

Supporting topic docs: [android.md](android.md) (Android plan),
[configurations.md](configurations.md) (the configuration workspace),
[discovery.md](discovery.md), [internet-tools.md](internet-tools.md),
[latency.md](latency.md), [reuse.md](reuse.md).

Repository-level documents: `README.md` (current product),
`ROADMAP.md` (future work, phased), `CHANGELOG.md` (historical
release record), `VERSION` (version source of truth).

## Rules

- **`autonomous-connectivity.md` is the single authority for future
  architecture.** Existing technical docs remain authoritative for
  currently implemented behavior. When they disagree with reality,
  the source wins and the doc gets fixed.
- Version source of truth: the `VERSION` file, injected at build
  time via ldflags; `internal/version/version.go` defaults to it.
  Never hand-maintain version strings elsewhere.
- Current behavior → current wording. Historical behavior →
  explicitly historical wording (version-labeled). Future behavior →
  explicitly planned wording, referencing
  `autonomous-connectivity.md`. Never present future work as
  implemented, and never let a historical narrative read as the
  current state.
- Release history belongs in `CHANGELOG.md` only — README and
  ROADMAP do not repeat it.
- Protocol capability claims require real-core evidence at the level
  `protocols.md` records; upstream support is never sufficient.
- Detailed maintenance rules (docs authority hierarchy, generated
  frontend/embed workflow, version audit) live in
  [development.md](development.md).
