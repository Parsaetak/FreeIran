# Reuse, idempotency and freshness policies (v0.9.14)

This document is the ONE authoritative description of how FreeIran
decides what to reuse, what to re-verify and what to acquire. Every
subsystem follows the same decision shape:

```text
DISCOVER → IDENTIFY → VALIDATE → COMPARE → REUSE / ADOPT / UPDATE / ACQUIRE
```

The result of that decision is always explicit and explainable — never
a lossy `installed=true` boolean. The recorded decisions
(`coremgr.ReuseDecision`) are:

| Decision | Meaning |
|---|---|
| `managed-current` | a healthy FreeIran-managed binary is already at the target version — nothing to acquire |
| `managed-healthy` | a healthy managed binary is kept; the latest-stable check could not complete (offline) |
| `system-current` | an externally installed binary matches the target version and was adopted by reference — no download |
| `system-newer` | the external binary is NEWER than stable; used, never downgraded |
| `system-older-but-working` | the external binary is older but working; retained, update surfaced, no download |
| `needs-update` | a managed binary is older than stable; the explicit update acquires it |
| `needs-download` | no suitable working local candidate exists; the official stable asset is downloaded |
| `invalid-local` | a local candidate existed but failed validation; the next candidate (or download) is used |
| `not-found` | nothing usable locally, nothing downloaded yet |

## Trust distinctions (never blurred)

- **upstream-verified** — the binary's SHA-256 matches the
  authoritative upstream asset digest (release-API `digest` field or a
  published sidecar). This is the only state allowed to be called
  *verified*.
- **locally-validated** — the binary exists, answers its version
  probe, validates against the configuration dialect and passes its
  smoke test, but no authoritative digest match was available. It is
  *working*, never *verified*.
- A matching version string alone is NEVER proof of provenance.
- Psiphon's official channel publishes no digests: downloaded Psiphon
  binaries stay visibly unavailable rather than faking a verified
  install; user-supplied/adopted binaries are used only under the
  honest locally-validated semantics.

## Ownership model

- **managed** — the binary lives inside the FreeIran workspace and is
  installed, updated, rolled back and removed by it.
- **external** — an already-installed engine FreeIran only
  REFERENCES. FreeIran never deletes, renames, overwrites or replaces
  an external executable; Disable/Remove clear FreeIran's reference
  only; updates that replace an external reference leave the file
  byte-for-byte intact; Rollback never moves an external file (and
  reports honestly when it has no managed target to restore).

The manifest records the external reference (`ownership`, `origin`,
`external_path`, version, file identity, trust). When a referenced
external executable disappears, the runtime rediscovers alternatives
instead of trusting a false "ready" state.

## Controlled discovery roots (bounded — never a disk walk)

One discovery authority (`system.ExecDiscovery`, surfaced through
`system.CoreLocator`) serves every consumer: core registry, core
manager, provider manager, diagnostics, Cores UI and Quick Connect
backend selection. No subsystem walks PATH or installation
directories independently.

Priority order:

1. **FreeIran-managed locations** — `<workspace>/cores/<core>/bin/`,
   `<workspace>/providers/<provider>/`.
2. **PATH** — the real OS executable lookup (authoritative for normal
   system resolution).
3. **Known platform installation locations** — Windows:
   `%ProgramFiles%`, `%ProgramFiles(x86)%`, `%LocalAppData%`
   (including its `Programs` subtree), `%AppData%`,
   `%SystemDrive%\ProgramData`; Linux: `/usr/local/bin`, `/usr/bin`,
   `/opt/<engine>(/bin)`, `~/.local/bin`, `~/bin`; macOS: the
   Homebrew and per-user equivalents. Always root × known-subdir ×
   known-executable-name — a fixed, bounded candidate list. A
   recursive scan of any tree (in particular `Program Files`) is
   prohibited by design: unpredictable latency and privacy-relevant
   full-disk visibility buy nothing over the known layouts.

Multiple installations are returned as ranked, deduplicated
candidates (managed before external; validated before unvalidated;
deterministic order within a tier) so the best candidate is
explainable.

## Freshness policies (bounded — nothing is cached forever)

| Evidence | Reuse rule | Invalidated by |
|---|---|---|
| Executable version probe | cached per file identity (canonical path + size + mtime); positive TTL 15 min, negative TTL 1 min | file identity change, vanished path, explicit Verify/refresh, install/update completion, runtime failure, configuration change affecting selection |
| Release metadata | persisted ETag/304 cache with conditional requests | documented release-API semantics; 304 requires a cached copy |
| Release metadata in-flight | concurrent identical requests share ONE network request; the shared request is detached from the first caller's context so one cancelled caller cannot cancel it for everyone | completion (success or error) |
| Core health | reused for a bounded period while the binary identity is unchanged | identity change, failed validation, explicit Verify |
| Configuration connectivity evidence | existing evidence TTL and failure/cooldown rules (unchanged) | as documented in the ranking/test-queue docs |
| Source ingestion | ETag/content-hash short-circuit exactly as documented (unchanged) | as documented |
| Staged download artifacts | complete + size-matched + digest-verified → reused with NO network; partial → resumed; oversized/corrupt → that artifact alone discarded | digest mismatch, size mismatch |
| Generated runtime configs | existing generation cache and correct invalidation (unchanged) | core/version change for the affected entries only |
| Test evidence / Quick Connect | fresh trustworthy evidence is not retested; policy-driven retesting unchanged | evidence TTL expiry |

Invalidation is precise: a core change invalidates that core's
discovery/health records and its dependent caches — never the whole
application cache.

## Update behavior

- A functional installed binary beats an unnecessary download. FreeIran
  does NOT auto-update and does NOT auto-downgrade.
- `Install` is a reuse-first **ensure** operation: with a current,
  working candidate anywhere in the controlled roots it completes
  without downloading.
- `Acquire` (the explicit Update path used by "Update all") intentionally
  acquires the newer stable release when the local binary is older —
  while still never modifying external files and still reusing
  complete staged artifacts.
- An external binary newer than stable is used as-is and surfaced as
  "newer than stable; automatic downgrade refused".
- An external binary older than stable is used, "update available" is
  surfaced, and no download starts until the user explicitly updates.
