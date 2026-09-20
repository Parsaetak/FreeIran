import { useCallback, useEffect, useState } from "react";
import { call, networkService, toolsService } from "../services";
import type {
  DNSDiagnosticReportView,
  IdentityReportView,
  NetCheckReport,
  NetCheckStageView,
  ToolInfoView,
  ToolMeasurementView,
  ToolResultView,
  ToolRunRequest,
  TunnelSnapshotView,
} from "../services";
import { EmptyState, SkeletonPage, StatTile } from "../components/common";
import { IconGlobe, IconPlay, IconRefresh, IconShield } from "../components/Icons";
import { describeError, toast } from "../state/toastStore";
import { truncate } from "../utilities/format";

/**
 * NetworkPage — the dedicated manual Internet / Network diagnostics
 * tab (v0.9.0 §3). One "Check connection" action classifies the
 * connectivity state across seven possibilities, shows per-probe
 * results (local links, DNS, TCP, HTTPS, latency, proxy path) and
 * never blocks the UI.
 */
export function NetworkPage() {
  const [report, setReport] = useState<NetCheckReport | null>(null);
  const [checking, setChecking] = useState(false);

  useEffect(() => {
    // Restore the last report so the page shows the previous findings
    // without immediately retesting.
    void call(() => networkService.LastReport())
      .then((last) => {
        if (last) setReport(last);
      })
      .catch(() => {
        /* best-effort */
      });
  }, []);

  const check = useCallback(async () => {
    setChecking(true);

    try {
      const next = await call(() => networkService.CheckConnection());
      setReport(next);
    } catch (error) {
      toast("error", "Network check failed", describeError(error));
    } finally {
      setChecking(false);
    }
  }, []);

  return (
    <div className="page-body">
      <div className="page-header">
        <div className="page-heading">
          <h1 className="page-title">Network</h1>
          <div className="page-subtitle">Independent probes classify your connection and the tunnel.</div>
        </div>
        <div className="page-actions">
          <button type="button" className="btn primary" disabled={checking} onClick={() => void check()}>
            <IconRefresh size={14} className={checking ? "spin" : undefined} />
            {checking ? "Checking…" : "Check connection"}
          </button>
        </div>
      </div>

      {/* v0.9.8.5 (§6): the Network Identity card — local IP, public
          IP and ISP/ASN metadata, strictly on explicit user action. */}
      <NetworkIdentityCard />

      {report === null && !checking && (
        <EmptyState
          icon={<IconGlobe size={20} />}
          title="No check has run yet"
          hint="Run a manual check to classify your current connectivity."
        />
      )}

      {checking && report === null && <SkeletonPage tiles={4} rows={3} />}

      {report && (
        <>
          <div className={`callout ${calloutKind(report.state)}`} data-state={report.state}>
            <strong>{stateLabel(report.state)}</strong> {report.summary}
          </div>

          <div className="stat-grid">
            <StatTile
              label="Latency"
              value={report.latency_ms ? `${report.latency_ms} ms` : "—"}
              sub={report.latency_ms ? qualityLabel(report.latency_ms) : "not measured"}
            />
            <StatTile label="Probes run" value={report.target_count} sub={`${report.duration_ms} ms total`} />
            <StatTile
              label="Checked at"
              value={new Date(report.checked_at).toLocaleTimeString()}
              sub={report.cancelled ? "cancelled" : "completed"}
            />
            <StatTile
              label="Proxy path"
              value={report.proxy ? (report.proxy.ok ? "working" : "failed") : "not tested"}
              sub={report.proxy?.latency_ms ? `${report.proxy.latency_ms} ms` : undefined}
            />
          </div>

          <div className="card-grid two">
            <StageLadder stages={report.stages} failedStage={report.failed_stage} />
            <ProbeList title="DNS resolution" results={report.dns} />
            <ProbeList title="TCP connectivity" results={report.tcp} />
            <ProbeList title="HTTPS reachability" results={report.https} />
            <ProbeList title="Local network links" results={report.local_links} />
          </div>
        </>
      )}

      {/* v0.9.8.1 (§6): the shared Internet-Tools engine — strictly
          user-triggered. Nothing in this section runs on mount. */}
      <InternetToolsSection />
    </div>
  );
}

// ---------------------------------------------------------------------------
// v0.9.8.5 Network Identity (§6): local IP, public IP and ISP/ASN
// metadata in one bounded check. The card NEVER runs on mount — the
// identity endpoints are contacted only from the explicit button.
// ---------------------------------------------------------------------------

function NetworkIdentityCard() {
  const [identity, setIdentity] = useState<IdentityReportView | null>(null);
  const [checking, setChecking] = useState(false);
  const [tunnel, setTunnel] = useState<TunnelSnapshotView | null>(null);
  const [tunneled, setTunneled] = useState(false);

  useEffect(() => {
    // Read-only tunnel header (decides whether the via-tunnel toggle
    // is offered). No identity lookup happens here.
    void call(() => toolsService.LiveTunnel())
      .then((snapshot) => setTunnel(snapshot ?? null))
      .catch(() => {
        /* best-effort toggle availability */
      });
  }, []);

  const check = useCallback(async () => {
    setChecking(true);

    try {
      const report = await call(() => toolsService.NetworkIdentity({ tunneled: tunneled && tunnel?.active === true }));
      setIdentity(report ?? null);
    } catch (error) {
      toast("error", "Identity check failed", describeError(error));
    } finally {
      setChecking(false);
    }
  }, [tunneled, tunnel]);

  const tunnelActive = tunnel?.active === true;
  const local = identity?.local;
  const pub = identity?.public;
  const meta = identity?.metadata;

  return (
    <section className="card" aria-label="Network identity">
      <div className="card-header">
        <div className="card-heading">
          <h3 className="card-title">Network identity</h3>
          <div className="card-subtitle">
            Who this machine is on the network right now — measured only on request.
          </div>
        </div>
        <div className="card-header-actions">
          <label title={tunnelActive ? undefined : "No active tunnel"}>
            <input
              type="checkbox"
              checked={tunneled && tunnelActive}
              disabled={!tunnelActive || checking}
              onChange={(event) => setTunneled(event.target.checked)}
            />{" "}
            via tunnel
          </label>
          <button type="button" className="btn sm" disabled={checking} onClick={() => void check()}>
            <IconRefresh size={14} className={checking ? "spin" : undefined} />
            {checking ? "Checking…" : "Check identity"}
          </button>
        </div>
      </div>

      {!identity ? (
        <div className="card-body">
          <EmptyState
            icon={<IconShield size={20} />}
            title="No identity check has run"
            hint="Local IP, public IP and ISP — one bounded, manual lookup. Nothing is sent automatically."
          />
        </div>
      ) : (
        <div className="card-body">
          <div className="stat-grid">
            <StatTile
              label="Local IP"
              value={localIdentityText(identity)}
              sub={local?.interface ? `via ${local.interface}` : local?.measured ? "measured" : "no route"}
            />
            <StatTile
              label={identity.path === "tunneled" ? "Tunnel exit IP" : "Public IP"}
              value={identity.path === "tunneled" ? pub?.tunnel_ip || "—" : pub?.direct_ip || "—"}
              sub={
                identity.path === "tunneled" && pub?.direct_ip
                  ? `direct ${pub.direct_ip}${pub.tunnel_match === true ? " · match" : " · no match"}`
                  : pub?.endpoint
                    ? truncate(pub.endpoint.replace(/^https?:\/\//, ""), 40)
                    : undefined
              }
            />
            <StatTile
              label="ISP / Network"
              value={meta?.available ? meta.organization || "—" : "Unknown"}
              sub={
                meta?.available
                  ? [meta.asn, meta.country].filter(Boolean).join(" · ") || "metadata source: private"
                  : "unavailable — never fabricated"
              }
            />
            <StatTile
              label="Checked"
              value={new Date(identity.checked_at).toLocaleTimeString()}
              sub={`${identity.duration_ms} ms${identity.path === "tunneled" ? " · via tunnel" : ""}${identity.cancelled ? " · cancelled" : ""}`}
            />
          </div>

          {local?.others && local.others.length > 0 && (
            <div className="identity-others">
              <span className="muted">Other local addresses: </span>
              {local.others
                .slice(0, 6)
                .map((other) => `${other.address}${other.interface ? ` (${other.interface})` : ""}`)
                .join(" · ")}
            </div>
          )}

          {identity.error && (
            <div className="identity-error muted" title={identity.error}>
              {truncate(identity.error, 160)}
            </div>
          )}
        </div>
      )}
    </section>
  );
}

/** Local identity text: primary IPv4 (or IPv6), never both-fabricated. */
function localIdentityText(identity: IdentityReportView): string {
  const local = identity.local;

  if (local.primary_ipv4) return local.primary_ipv4;

  if (local.primary_ipv6) return local.primary_ipv6;

  if (local.others && local.others.length > 0) return local.others[0].address;

  return "—";
}

// ---------------------------------------------------------------------------
// v0.9.8.5 staged diagnostics ladder (§4): the ordered explanation of
// the connectivity verdict — which stage failed and why.
// ---------------------------------------------------------------------------

const STAGE_LABELS: Record<string, string> = {
  local_link: "Local link",
  local_ip: "Local IP",
  dns: "DNS",
  tcp: "TCP",
  tls: "TLS",
  https: "HTTPS",
  captive_portal: "Captive portal",
  direct_internet: "Direct Internet",
  tunnel_internet: "Tunnel Internet",
};

function StageLadder({ stages, failedStage }: { stages?: NetCheckStageView[]; failedStage?: string }) {
  if (!stages || stages.length === 0) return null;

  return (
    <section className="card">
      <h3 className="card-title">Connection stages</h3>
      <ul className="probe-list">
        {stages.map((stage) => {
          const isFailed = stage.status === "failed";
          const isFailedStage = isFailed && failedStage === stage.stage;
          const detail = [stage.detail, stage.failure_class && isFailed ? `(${stage.failure_class})` : undefined]
            .filter(Boolean)
            .join(" ");

          return (
            <li
              key={stage.stage}
              className={`probe-row${isFailedStage ? " stage-first-failed" : ""}`}
              title={detail || undefined}
            >
              <span className={`dot ${stageDotClass(stage.status)}`} aria-hidden />
              <span className="probe-name">{STAGE_LABELS[stage.stage] ?? stage.stage}</span>
              <span className="probe-latency muted">
                {stage.status === "ok" && stage.latency_ms ? `${stage.latency_ms} ms` : ""}
              </span>
              {detail && (
                <span className={`probe-error ellipsis ${stage.status === "ok" ? "muted" : ""}`} title={detail}>
                  {truncate(detail, 60)}
                </span>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function stageDotClass(status: string): string {
  if (status === "ok") return "ok";
  if (status === "failed") return "bad";
  return "idle"; // skipped / not_checked — honestly neutral
}

function ProbeList({ title, results }: { title: string; results?: Array<{ name: string; ok: boolean; latency_ms?: number; error?: string }> }) {
  return (
    <section className="card">
      <h3 className="card-title">{title}</h3>

      {!results || results.length === 0 ? (
        <div className="log-empty">No probes in this class ran.</div>
      ) : (
        <ul className="probe-list">
          {results.map((r) => (
            <li key={r.name} className="probe-row">
              <span className={`dot ${r.ok ? "ok" : "bad"}`} aria-hidden />
              <span className="probe-name ellipsis" title={r.name}>
                {r.name}
              </span>
              <span className="probe-latency muted">{r.ok ? `${r.latency_ms ?? "—"} ms` : ""}</span>
              {!r.ok && r.error && (
                <span className="probe-error muted ellipsis" title={r.error}>
                  {r.error}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function stateLabel(state: string): string {
  switch (state) {
    case "no_internet":
      return "No internet.";
    case "dns_failure":
      return "DNS failure.";
    case "https_failure":
      return "HTTPS failing.";
    case "high_latency":
      return "Working, but slow.";
    case "ok":
      return "Internet OK.";
    case "proxy_only":
      return "Proxy required.";
    case "core_no_internet":
      return "Core connected, external traffic failing.";
    default:
      return "Unknown state.";
  }
}

function calloutKind(state: string): string {
  switch (state) {
    case "ok":
      return "success";
    case "high_latency":
    case "proxy_only":
      return "info";
    default:
      return "error";
  }
}

function qualityLabel(ms: number): string {
  if (ms <= 150) return "excellent";
  if (ms <= 400) return "good";
  if (ms <= 800) return "acceptable";
  if (ms <= 2000) return "slow";
  return "very slow";
}

// ---------------------------------------------------------------------------
// v0.9.8.1 Internet tools (§6): the shared, bounded tools engine
// surfaced for MANUAL use. The catalogue and the live-tunnel status
// are read-only lookups; every probe fires ONLY from an explicit
// click — nothing here ever runs automatically.
// ---------------------------------------------------------------------------

/** Newest-first result history cap (bounded by design). */
const TOOL_RESULT_LIMIT = 12;

/** Visual grouping order for the tool grid (connectivity → tunnel). */
const TOOL_GROUP_ORDER: Record<string, number> = {
  connectivity: 0,
  protocol: 1,
  path: 2,
  identity: 3,
  tunnel: 4,
};

function InternetToolsSection() {
  const [tools, setTools] = useState<ToolInfoView[]>([]);
  const [tunnel, setTunnel] = useState<TunnelSnapshotView | null>(null);
  const [results, setResults] = useState<ToolResultView[]>([]);
  const [runningTool, setRunningTool] = useState<string | null>(null);

  const refreshTunnel = useCallback(async () => {
    try {
      const snapshot = await call(() => toolsService.LiveTunnel());
      setTunnel(snapshot ?? null);
    } catch {
      /* best-effort header state */
    }
  }, []);

  useEffect(() => {
    // Read-only lookups only: the catalogue and the live-tunnel
    // header. No tool execution happens on mount (§6 hard rule).
    void call(() => toolsService.Tools())
      .then((catalogue) => setTools(catalogue ?? []))
      .catch(() => {
        /* catalogue unavailable — the grid simply stays empty */
      });

    void refreshTunnel();
  }, [refreshTunnel]);

  const runTool = useCallback(
    async (request: ToolRunRequest) => {
      setRunningTool(request.tool);

      try {
        const result = await call(() => toolsService.RunTool(request));

        if (result) {
          // Newest first, capped at twelve rows.
          setResults((previous) => [result, ...previous].slice(0, TOOL_RESULT_LIMIT));
        }
      } catch (error) {
        toast("error", "Tool run failed", describeError(error));
      } finally {
        setRunningTool(null);
        // A run may have changed the tunnel's health/latency.
        await refreshTunnel();
      }
    },
    [refreshTunnel],
  );

  const tunnelActive = tunnel?.active === true;

  // Grouped visually: connectivity, protocol, path, identity, tunnel.
  const ordered = [...tools].sort((a, b) => {
    const groupA = TOOL_GROUP_ORDER[a.group] ?? TOOL_GROUP_ORDER.length;
    const groupB = TOOL_GROUP_ORDER[b.group] ?? TOOL_GROUP_ORDER.length;

    return groupA !== groupB ? groupA - groupB : a.label.localeCompare(b.label);
  });

  const labelFor = (toolId: string): string =>
    tools.find((info) => info.id === toolId)?.label ?? toolId;

  return (
    <section aria-label="Internet tools">
      <div className="page-header">
        <div className="page-heading">
          <h2 className="page-title">Internet tools</h2>
          <div className="page-subtitle">User-triggered, bounded diagnostics — nothing runs automatically.</div>
        </div>
      </div>

      {tunnelActive && (
        <div className="callout info">
          Active tunnel: {tunnel?.provider || "—"} · {tunnel?.endpoint || "—"} · {tunnelLatencyText(tunnel)}
        </div>
      )}

      {ordered.length > 0 && (
        <div className="tools-grid">
          {ordered.map((info) => (
            <ToolCard
              key={info.id}
              info={info}
              tunnelActive={tunnelActive}
              running={runningTool === info.id}
              onRun={(request) => void runTool(request)}
            />
          ))}
        </div>
      )}

      {results.length > 0 && (
        <div className="tool-result-list">
          {results.map((result, index) => (
            <ToolResultRow
              key={`${result.tool_id}-${result.started_at}-${index}`}
              result={result}
              label={labelFor(result.tool_id)}
            />
          ))}
        </div>
      )}
    </section>
  );
}

function ToolCard({
  info,
  tunnelActive,
  running,
  onRun,
}: {
  info: ToolInfoView;
  tunnelActive: boolean;
  running: boolean;
  onRun: (request: ToolRunRequest) => void;
}) {
  const [target, setTarget] = useState("");
  const [tunneled, setTunneled] = useState(false);

  const submit = () => {
    if (running) return;

    const request: ToolRunRequest = {
      tool: info.id,
      // The toggle is only meaningful while a live tunnel exists —
      // otherwise the backend would honestly report "unsupported".
      tunneled: tunneled && tunnelActive,
    };

    const trimmed = target.trim();

    if (info.takes_target && trimmed) {
      request.target = trimmed;
    }

    onRun(request);
  };

  return (
    <div className="tool-card">
      <div className="tool-card-head">
        <span className="tool-card-title">{info.label}</span>
        <span className="tool-card-group">{info.group}</span>
      </div>

      {info.takes_target && (
        <input
          className="input tool-target"
          type="text"
          placeholder="target (default: safe public endpoint)"
          value={target}
          onChange={(event) => setTarget(event.target.value)}
        />
      )}

      <label title={tunnelActive ? undefined : "No active tunnel"}>
        <input
          type="checkbox"
          checked={tunneled && tunnelActive}
          disabled={!tunnelActive}
          onChange={(event) => setTunneled(event.target.checked)}
        />{" "}
        via tunnel
      </label>

      <button type="button" className="btn sm" disabled={running} onClick={submit}>
        {running ? <span className="btn-spinner" /> : <IconPlay size={14} />} {running ? "Running…" : "Run"}
      </button>
    </div>
  );
}

function ToolResultRow({ result, label }: { result: ToolResultView; label: string }) {
  const measurement = result.measurement;

  const meta = [
    result.target ? truncate(result.target, 60) : undefined,
    result.path,
    result.provider,
    result.duration_ms !== undefined ? `${result.duration_ms} ms` : undefined,
    result.transport,
  ]
    .filter((part): part is string => Boolean(part))
    .join(" · ");

  return (
    <div className="tool-result">
      <div className="tool-result-head">
        <span className="tool-result-title">{label}</span>
        <span className={`badge ${toolStatusVariant(result.status)}`}>{result.status}</span>
      </div>

      {meta && <div className="tool-result-meta">{meta}</div>}

      <div className="tool-result-detail">
        {measurement && (
          <>
            {/* Measured latency only — v0.9.8.1 semantics (0 + measured = sub-ms). */}
            {measurement.measured === true && (
              <div>
                latency:{" "}
                {measurement.latency_ms && measurement.latency_ms > 0
                  ? `${Math.round(measurement.latency_ms)} ms`
                  : "< 1 ms"}
              </div>
            )}

            {measurement.status !== undefined && measurement.status > 0 && (
              <div>HTTP status: {measurement.status}</div>
            )}

            {(measurement.exit_ip_direct || measurement.exit_ip_tunnel) && (
              <div>
                {[
                  measurement.exit_ip_direct ? `Direct: ${measurement.exit_ip_direct}` : undefined,
                  measurement.exit_ip_tunnel ? `Tunnel: ${measurement.exit_ip_tunnel}` : undefined,
                  exitIpMatchText(measurement),
                ]
                  .filter((part): part is string => Boolean(part))
                  .join(" · ")}
              </div>
            )}

            {measurement.captive_detected !== undefined && (
              <div>
                captive portal: {measurement.captive_detected ? "detected" : "not detected"}
                {measurement.captive_redirect ? ` (${truncate(measurement.captive_redirect, 60)})` : ""}
              </div>
            )}

            {measurement.hop_count !== undefined && measurement.hop_count > 0 && (
              <div>hops: {measurement.hop_count}</div>
            )}

            {measurement.mtu_bytes !== undefined && measurement.mtu_bytes > 0 && (
              <div>MTU: {measurement.mtu_bytes} bytes</div>
            )}

            {measurement.address_count !== undefined && measurement.address_count > 0 && (
              <div>addresses: {measurement.address_count}</div>
            )}
          </>
        )}

        {/* v0.9.8.5 (§5): the DNS diagnostic's structured per-resolver
            rows — every resolver, every record type, its transport and
            its honest failure class. */}
        {result.dns && <DNSEvidence report={result.dns} />}

        {/* Failures and unsupported capabilities carry their honest
            reason in error (e.g. "no active tunnel", "QUIC … not
            compiled into this build"). */}
        {result.error && (
          <div title={result.error}>{truncate(result.error, 160)}</div>
        )}
      </div>
    </div>
  );
}

/**
 * DNSEvidence renders the v0.9.8.5 DNS-diagnostic report: one block
 * per resolver with its A/AAAA rows (status, transport, latency,
 * answers or the honest failure class).
 */
function DNSEvidence({ report }: { report: DNSDiagnosticReportView }) {
  return (
    <div className="dns-evidence">
      <div className="dns-evidence-head">
        {report.name} · {report.resolvers.length} resolver{report.resolvers.length === 1 ? "" : "s"} · A + AAAA
        {report.cancelled ? " · cancelled" : ""}
      </div>

      {report.resolvers.map((resolver) => (
        <div key={`${resolver.resolver}-${resolver.address ?? ""}`} className="dns-resolver">
          <div className="dns-resolver-head">
            <span className={`dot ${resolver.ok ? "ok" : "bad"}`} aria-hidden />
            <span className="dns-resolver-name">{resolver.resolver}</span>
            {resolver.transport && <span className="badge neutral">{resolver.transport}</span>}
            {resolver.ok && resolver.latency_ms ? (
              <span className="muted">best {resolver.latency_ms} ms</span>
            ) : null}
          </div>

          <ul className="dns-queries">
            {resolver.queries.map((query, index) => (
              <li key={`${query.record_type}-${index}`} className="dns-query" title={query.error || undefined}>
                <span className="dns-qtype">{query.record_type}</span>
                {query.ok ? (
                  <span className="dns-answer">
                    {query.answer_count} answer{query.answer_count === 1 ? "" : "s"}
                    {query.addresses && query.addresses.length > 0
                      ? ` (${query.addresses.slice(0, 3).join(", ")}${query.addresses.length > 3 ? ", …" : ""})`
                      : ""}
                  </span>
                ) : (
                  <span className="dns-failure">
                    {query.failure_class || "failed"}
                    {query.error ? ` — ${truncate(query.error, 80)}` : ""}
                  </span>
                )}
                {query.ok && query.latency_ms ? <span className="muted">{query.latency_ms} ms</span> : null}
              </li>
            ))}
          </ul>
        </div>
      ))}
    </div>
  );
}

/**
 * Exit-IP match text: the backend omits a false flag (omitempty), so
 * a visible inequality between the two printed IPs is the honest
 * "no match" reading — never fabricated.
 */
function exitIpMatchText(measurement: ToolMeasurementView): string | undefined {
  if (measurement.exit_ip_match === true) return "match";

  if (
    measurement.exit_ip_direct &&
    measurement.exit_ip_tunnel &&
    measurement.exit_ip_direct !== measurement.exit_ip_tunnel
  ) {
    return "no match";
  }

  return undefined;
}

function toolStatusVariant(status: string): string {
  switch (status) {
    case "ok":
      return "success";
    case "failed":
      return "error";
    case "timeout":
    case "invalid_target":
      return "warn";
    case "cancelled":
      return "neutral";
    case "unsupported":
      return "info";
    default:
      return "neutral";
  }
}

/**
 * Live-tunnel latency: the backend measures it through the live
 * endpoint (0 = sub-millisecond; omitempty drops a measured 0, so an
 * absent value on a healthy tunnel is still the measured sub-ms band).
 */
function tunnelLatencyText(tunnel: TunnelSnapshotView | null): string {
  const ms = tunnel?.latency_ms ?? undefined;

  if (ms !== undefined) {
    return ms > 0 ? `${Math.round(ms)} ms` : "< 1 ms";
  }

  return tunnel?.healthy === true ? "< 1 ms" : "—";
}
