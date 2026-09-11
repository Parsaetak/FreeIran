import { Fragment, useEffect, useRef, useState } from "react";
import { useAppStore } from "../state/appStore";
import { useConnectionStore } from "../state/connectionStore";
import { call, logService, type IngestionStats, type LogEntry } from "../services";
import {
  formatBytes,
  formatLatency,
  formatNumber,
  formatUptime,
  relativeTime,
} from "../utilities/format";
import {
  BootDots,
  CONNECTION_STATE_LABELS,
  OrbIcon,
  connectStepIndex,
  connectionUiState,
  CONNECT_STEPS,
  StatTile,
} from "../components/common";
import { IconClock, IconCpu, IconPlay, IconSignal, IconStop } from "../components/Icons";
import type { Page } from "../types/ui";

const ACTIVITY_LIMIT = 8;
const ACTIVITY_POLL_MS = 2000;

/**
 * Primary dashboard: live connection panel, key totals, recent
 * runtime activity and core availability at a glance.
 */
export function DashboardPage({ onNavigate }: { onNavigate: (page: Page) => void }) {
  const backend = useAppStore((state) => state.backend);
  const status = useAppStore((state) => state.status);
  const refreshApp = useAppStore((state) => state.refresh);
  const snapshot = useConnectionStore((state) => state.snapshot);
  const backends = useConnectionStore((state) => state.backends);
  const busy = useConnectionStore((state) => state.busy);
  const connect = useConnectionStore((state) => state.connect);
  const disconnect = useConnectionStore((state) => state.disconnect);
  const refreshBackends = useConnectionStore((state) => state.refreshBackends);

  if (status === "backend_unavailable") {
    return (
      <div className="empty-state tall">
        <div className="empty-icon" aria-hidden>
          <IconCpu size={22} />
        </div>
        <div className="empty-title">Backend unavailable</div>
        <p className="empty-hint">
          The engine did not answer its state poll. It may still be starting,
          or the process may have exited — check the runtime log for the last
          lifecycle events.
        </p>
        <div className="toolbar center-row">
          <button
            type="button"
            className="btn primary"
            onClick={() => {
              void refreshApp();
              void refreshBackends();
            }}
          >
            Retry now
          </button>
          <button type="button" className="btn" onClick={() => onNavigate("diagnostics")}>
            Open diagnostics
          </button>
        </div>
      </div>
    );
  }

  if (!backend) {
    return (
      <div className="skeleton-panel" aria-busy="true" aria-label="Loading dashboard">
        <div className="skeleton tile" />
        <div className="stat-grid">
          {Array.from({ length: 4 }, (_, i) => (
            <div key={i} className="skeleton tile" />
          ))}
        </div>
        <div className="skeleton tile" />
      </div>
    );
  }

  const ingestion = backend.last_ingestion as IngestionStats | null;
  const availableCores = backends.filter((b) => b.status === "available").length;
  const uiState = snapshot ? connectionUiState(snapshot.state) : "disconnected";
  const connected = uiState === "connected";
  const canConnectHere = Boolean(snapshot?.config_id);

  return (
    <div>
      <div className="page-header">
        <div>
          <h1 className="page-title">Dashboard</h1>
          <div className="page-subtitle">
            App started {relativeTime(backend.started_at)} · native acceleration:{" "}
            {backend.native_acceleration || "unknown"}
          </div>
        </div>
      </div>

      {/* Live connection panel */}
      <section className="conn-panel" aria-label="Connection status">
        <div className={`orb-wrap ${uiState}`}>
          <div className="orb">
            <span className="orb-icon" aria-hidden>
              <OrbIcon state={uiState} />
            </span>
          </div>
        </div>

        <div className="conn-meta">
          <div className="conn-state-line">
            <span className="conn-state-label">
              {snapshot
                ? CONNECTION_STATE_LABELS[snapshot.state] ?? snapshot.state
                : "Disconnected"}
            </span>
            {(uiState === "connecting" || uiState === "disconnecting") && <BootDots />}
          </div>

          <div className="conn-facts">
            <span className="conn-fact">
              <span>config</span>
              <b>{snapshot?.config_name || snapshot?.config_display || "not selected"}</b>
            </span>
            <span className="conn-fact">
              <span>core</span>
              <b>
                {snapshot?.core ? `${snapshot.core} ${snapshot.core_version ?? ""}` : "—"}
              </b>
            </span>
            <span className="conn-fact">
              <span>latency</span>
              <b>{connected ? formatLatency(snapshot?.latency_ms) : "—"}</b>
            </span>
            <span className="conn-fact hide-md">
              <span>endpoint</span>
              <b>{connected ? snapshot?.endpoint || "—" : "—"}</b>
            </span>
            <span className="conn-fact hide-md">
              <span>uptime</span>
              <b>
                {connected && snapshot?.started_at
                  ? formatUptime(Date.now() - snapshot.started_at)
                  : "—"}
              </b>
            </span>
          </div>

          {uiState === "connecting" && snapshot && (
            <div className="conn-steps" aria-label="Connection progress">
              {CONNECT_STEPS.map((step, index) => {
                const current = connectStepIndex(snapshot.state);

                return (
                  <Fragment key={step.key}>
                    {index > 0 && <span className="conn-step-sep" aria-hidden />}
                    <span
                      className={`conn-step ${index < current ? "done" : index === current ? "current" : ""}`}
                    >
                      {step.label}
                    </span>
                  </Fragment>
                );
              })}
            </div>
          )}
        </div>

        <div className="conn-actions">
          <button
            type="button"
            className="btn primary lg"
            disabled={uiState === "connecting" || uiState === "disconnecting" || busy}
            onClick={() => {
              if (canConnectHere && snapshot?.config_id) {
                void connect(snapshot.config_id);
              } else {
                onNavigate("connection");
              }
            }}
          >
            <IconPlay size={14} />
            {canConnectHere ? "Connect" : "Set up connection"}
          </button>

          <button
            type="button"
            className="btn danger lg"
            disabled={uiState !== "connected" || busy}
            onClick={() => void disconnect()}
          >
            <IconStop size={14} />
            Disconnect
          </button>
        </div>

        {snapshot?.last_error && uiState === "failed" && (
          <div className="error-banner full-row">{snapshot.last_error}</div>
        )}
      </section>

      {/* Key totals */}
      <div className="stat-grid">
        <StatTile
          label="Configurations"
          value={formatNumber(backend.config_count)}
          sub={`${formatNumber(backend.storage?.count ?? 0)} records`}
        />
        <StatTile
          label="Available cores"
          value={`${availableCores}/${backends.length || 3}`}
          sub={backends.map((b) => b.name).join(" · ") || "xray · v2ray · sing-box"}
        />
        <StatTile
          label="Last ingestion"
          value={ingestion ? formatNumber(ingestion.persisted) : "—"}
          sub={ingestion ? "new configs persisted" : "no cycle yet"}
        />
        <StatTile
          label="Storage used"
          value={formatBytes(backend.storage?.disk_bytes ?? 0)}
          sub={`${formatNumber(backend.storage?.chunk_count ?? 0)} chunks`}
        />
      </div>

      {status === "degraded" && (
        <div className="error-banner mt-4">
          Storage verification reported problems — run a check from Diagnostics.
        </div>
      )}

      <div className="page-grid-2col">
        <div className="card mb-0">
          <div className="card-header">
            <h3 className="card-title">
              <IconClock size={14} /> Recent activity
            </h3>
            <button type="button" className="btn sm ghost" onClick={() => onNavigate("diagnostics")}>
              Full log
            </button>
          </div>

          <ActivityFeed />
        </div>

        <div className="card mb-0">
          <div className="card-header">
            <h3 className="card-title">
              <IconSignal size={14} /> Core status
            </h3>
            <button type="button" className="btn sm ghost" onClick={() => onNavigate("connection")}>
              Manage
            </button>
          </div>

          {backends.length === 0 ? (
            <div className="log-empty">No cores detected yet.</div>
          ) : (
            backends.slice(0, 3).map((core) => (
              <div className="row" key={core.name}>
                <span className="cell-main">
                  <div className="cell-title">{core.name}</div>
                  {core.summary && <div className="cell-sub">{core.summary}</div>}
                </span>
                <span className={`badge ${badgeForStatus(core.status)}`}>
                  {core.status}
                  {core.status === "available" && core.version ? ` · ${core.version}` : ""}
                </span>
              </div>
            ))
          )}
        </div>
      </div>

      {ingestion && (
        <div className="card mt-4">
          <div className="card-header">
            <h3 className="card-title eyebrow">Last ingestion</h3>
          </div>

          <div className="stat-grid">
            <StatTile label="Sources OK" value={`${ingestion.sources_ok}/${ingestion.sources_total}`} />
            <StatTile label="Discovered" value={formatNumber(ingestion.discovered)} />
            <StatTile label="Duplicates" value={formatNumber(ingestion.duplicates)} />
            <StatTile label="Persisted" value={formatNumber(ingestion.persisted)} />
            <StatTile label="Invalid" value={formatNumber(ingestion.invalid)} />
            <StatTile label="Unchanged" value={formatNumber(ingestion.sources_unchanged)} />
          </div>
        </div>
      )}
    </div>
  );
}

function badgeForStatus(status: string): string {
  if (status === "available") return "success";
  if (status === "invalid") return "error";

  return "neutral";
}

/** Live tail of the runtime log (2s poll, newest last, slide-in). */
function ActivityFeed() {
  const [entries, setEntries] = useState<LogEntry[]>([]);
  const lastSeqRef = useRef(0);

  useEffect(() => {
    let cancelled = false;

    const tick = async () => {
      try {
        const page = await call(() =>
          logService.Recent({
            since_seq: lastSeqRef.current,
            limit: ACTIVITY_LIMIT,
            subsystem: "",
            query: "",
            level: "",
          }),
        );

        if (cancelled) return;

        if (page.entries.length > 0) {
          setEntries((previous) =>
            [...previous, ...page.entries].slice(-ACTIVITY_LIMIT),
          );
        }

        lastSeqRef.current = page.last_seq;
      } catch {
        // Backend busy or unavailable — keep the previous view.
      }
    };

    void tick();

    const timer = window.setInterval(() => void tick(), ACTIVITY_POLL_MS);

    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, []);

  if (entries.length === 0) {
    return <div className="log-empty">Waiting for runtime events…</div>;
  }

  return (
    <div aria-live="polite">
      {entries.map((entry) => (
        <div className="row" key={entry.seq}>
          <span className={`status-dot ${dotForLevel(entry.level)}`} aria-hidden />
          <span className="cell-main">
            <div className="cell-title">{entry.message || entry.event}</div>
            <div className="cell-sub">
              {entry.subsystem} · {entry.event}
            </div>
          </span>
          <span className="latency none hide-md">{formatEventAge(entry.ts)}</span>
        </div>
      ))}
    </div>
  );
}

function dotForLevel(level: string): string {
  if (level === "error") return "bad";
  if (level === "warn") return "busy";

  return "ok";
}

/** Short age label computed from an RFC3339 timestamp. */
function formatEventAge(ts: string): string {
  const time = Date.parse(ts);

  if (Number.isNaN(time)) return "";

  return relativeTime(time);
}
