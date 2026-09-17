# Discovery Architecture (v0.9.7)

Public-source discovery, rate-limit engineering and security
boundaries. This document covers the v0.9.7 bounded discovery system
(`engine/discovery/connectors.go`, `github.go`, `ratelimit.go`,
`provenance.go`, and the application orchestrator
`engine/app/discoverysources.go`).

## Pipeline

```
DETECT → DISCOVER → INGEST → PARSE → NORMALIZE → DEDUP → VALIDATE →
SELECT → TEST → SCORE → RANK → CONNECT → VERIFY → MONITOR → RECOVER
```

Discovery feeds the front half of the pipeline; it never automatically
bulk-tests every discovered node. Staged selection (cheap validation →
protocol compatibility → freshness/source quality → small candidate
subset → Ping → URL → handshake/full verification) promotes a bounded
subset into the normal testing flow.

## Connectors

| Connector | Provider | Strategies |
|---|---|---|
| Generic HTTP/HTTPS | any public host | direct fetch of text configuration material (redirects, compression, conditional GETs) |
| GitHub adapter | api.github.com + raw.githubusercontent.com | A repository search · B code search (protocol URI patterns) · C recursive tree inspection (.txt/.yaml/.yml/.json/.conf/.list/.sub) · D raw file fetch · E README reference extraction → bounded queue · F release assets · G public gists |
| Existing configured sources | user-defined | unchanged; always fetched first, never displaced |

### Bounded recursion

Expansion (source A → B → C) is bounded on every axis:

- `MaxDepth` (default 2) — reference chains cannot recurse deeply;
- `MaxURLsPerSource` (default 16) — links extracted per fetched body;
- `MaxURLsPerRun` (default 200) — total fetches per discovery run;
- `MaxBodySize` (default 8 MiB) — per-response cap;
- domain cooldown (default 2 min) — per-host pacing;
- total time budget (default 5 min) — wall-clock ceiling.

The shared `CandidateQueue` enforces deduplication (normalized URL
identity: lower scheme/host, fragment-free), depth caps, per-run
budgets and domain cooldowns.

## SSRF protection

Automatically discovered URLs are fetched through
`httpx.NewSSRFClient`:

- scheme allowlist: `http`/`https` only;
- port validation: 80/443 by default (8443/8080 only for explicitly
  user-configured overrides; infrastructure ports never);
- destination validation: loopback, link-local (169.254.0.0/16 cloud
  metadata), RFC1918/4193 private, CGNAT, multicast and reserved
  ranges are rejected — as IP literals, `localhost`-family names and
  `.local`/`.internal` names;
- DNS-rebinding prevention: a dial `Control` hook re-validates the
  RESOLVED IP at connect time, so lookup-time and connect-time
  addresses cannot diverge;
- bounded redirects: every hop is re-validated and the chain is capped;
- response bodies are size-capped and never executed — no JavaScript,
  no browser, no HTML rendering anywhere in the discovery path.

User-configured sources keep a documented override path
(`AllowPrivate`) that relaxes only the IP-class checks; scheme, port
and redirect caps always apply.

## GitHub rate-limit engineering

Unauthenticated public GitHub access is limited (60 core requests per
hour per IP; search endpoints are stricter). `discovery.RateLimiter`
accounts per provider (`github`, `github-gist`, `http`):

```
requests, successes, failures, 429s, 403s,
rate-limit remaining, rate-limit reset, last request, average latency
```

Controls:

- per-run request budget (default 60 per provider);
- minimum interval pacing between requests (GitHub: 500 ms);
- 429/403 cooldowns: Retry-After honoured when present, otherwise
  exponential from 5 minutes doubling per occurrence, capped at 6 h;
- ETag conditional requests keep repeat runs cheap (the generic
  fetcher persists `ETag` / `Last-Modified` in the provenance ledger);
- `github_rate_limited`-class degradation is a structured log event,
  never a fatal application error — the engine continues with cached
  and local sources.

## Source trust & provenance

Every discovered candidate persists a full ledger
(`config/discovered-sources.json`, 256-entry cap, atomic write):

```
source_id, source_type, source_url, origin_repository, origin_path,
discovered_from, first_seen, last_seen, last_success, last_failure,
http_status, content_hash, content_size, fetch_latency, parser,
candidate_count, valid_count, duplicate_count, depth
```

Trust bands (`unknown → discovered → validated → tested → working`,
with `stale`/`failed` as terminal health states) are EARNED by fetch,
parse and test outcomes; states are never silently mixed. Manually
configured sources live in `config/sources.json` and are never
displaced by autonomous discovery.

## Data retention

Discovered staging entries, source bodies (never retained wholesale —
only hash/metadata/ETag), test history (bounded per configuration) and
runtime artifacts are governed by the cleanup coordinator's retention
policies. Pinned configurations, manually added sources, currently
working configurations and recently successful configurations are
never automatically removed.
