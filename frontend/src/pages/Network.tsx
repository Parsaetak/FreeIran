import { useCallback, useEffect, useState } from "react";
import { call, networkService } from "../services";
import type { NetCheckReport } from "../services";
import { EmptyState, SkeletonPage, StatTile } from "../components/common";
import { IconGlobe, IconRefresh } from "../components/Icons";
import { describeError, toast } from "../state/toastStore";

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
            <ProbeList title="DNS resolution" results={report.dns} />
            <ProbeList title="TCP connectivity" results={report.tcp} />
            <ProbeList title="HTTPS reachability" results={report.https} />
            <ProbeList title="Local network links" results={report.local_links} />
          </div>
        </>
      )}
    </div>
  );
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
