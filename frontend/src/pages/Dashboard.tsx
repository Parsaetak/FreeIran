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
import { IconCheck, IconClock, IconCpu, IconPlay, IconSignal, IconStop } from "../components/Icons";
import type { Page } from "../types/ui";
import { useStartFlowStore, stageIndex, STAGE_ORDER } from "../state/startflowStore";
import { flowStageLabel } from "../types/discovery";

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
  const connectBest = useConnectionStore((state) => state.connectBest);
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
        <div className="page-heading">
          <h1 className="page-title">Dashboard</h1>
          <div className="page-subtitle">
            App started {relativeTime(backend.started_at)} · native acceleration:{" "}
            {backend.native_acceleration || "unknown"}
          </div>
        </div>
      </div>

      <OnboardingChecklist
        onNavigate={onNavigate}
        coresAvailable={availableCores}
        configCount={backend.config_count}
        connected={connected}
        connecting={uiState === "connecting"}
      />

      {/* v0.9.6 adaptive start flow: START → DETECT → DISCOVER →
          TEST → RANK → CONNECT → VERIFY, with real stage progress. */}
      <SmartStartPanel onNavigate={onNavigate} connected={connected} />

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
              // v0.9.3: CONNECT means "connect me" — with a previous
              // session it reconnects; otherwise the engine picks the
              // best viable candidate from real test history.
              if (canConnectHere && snapshot?.config_id) {
                void connect(snapshot.config_id);
              } else {
                void connectBest().then(() => {
                  if (useConnectionStore.getState().error) {
                    onNavigate("connection");
                  }
                });
              }
            }}
          >
            <IconPlay size={14} />
            Connect
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
            <h3 className="card-title">Last ingestion</h3>
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

/**
 * v0.9.6 Smart Start panel: the adaptive flow with real stage
 * progress, environment evidence and measured result summary.
 * Progress comes from engine events — every stage shows its measured
 * duration, never a fabricated animation.
 */
function SmartStartPanel({
  onNavigate,
  connected,
}: {
  onNavigate: (page: Page) => void;
  connected: boolean;
}) {
  const flowStatus = useStartFlowStore((state) => state.status);
  const environment = useStartFlowStore((state) => state.environment);
  const busy = useStartFlowStore((state) => state.busy);
  const error = useStartFlowStore((state) => state.error);
  const run = useStartFlowStore((state) => state.run);
  const cancel = useStartFlowStore((state) => state.cancel);
  const discoverNow = useStartFlowStore((state) => state.discoverNow);

  const running = Boolean(flowStatus?.running) || busy;
  const stage = flowStatus?.stage ?? "idle";
  const current = stageIndex(stage);
  const flowSteps = STAGE_ORDER.slice(1, 7); // detect … verify

  return (
    <section className="card mb-4" aria-label="Smart start">
      <div className="card-header">
        <h3 className="card-title">
          <IconSignal size={14} /> Smart start
        </h3>
        <div className="toolbar">
          <button
            type="button"
            className="btn sm ghost"
            disabled={running}
            onClick={() => void discoverNow(false)}
            title="Run one discovery pass over the configured and trusted public sources"
          >
            Discover now
          </button>
          {running ? (
            <button type="button" className="btn sm danger ghost" onClick={() => void cancel()}>
              Cancel
            </button>
          ) : null}
        </div>
      </div>

      <p className="page-subtitle" style={{ marginBottom: 12 }}>
        One button runs the full flow: environment detection, multi-level discovery,
        testing, ranking by your selected sort mode, connection and connectivity
        verification. Manual selection on the Connection page always overrides
        automatic selection.
      </p>

      {/* Stage progress: only real transitions, with measured durations */}
      <div className="conn-steps" aria-label="Start flow progress">
        {flowSteps.map((step, index) => {
          const stepDone = current > index + 1 || stage === "connected";
          const stepCurrent = stage === step || (stage === "connected" && step === "verifying");
          return (
            <Fragment key={step}>
              {index > 0 && <span className="conn-step-sep" aria-hidden />}
              <span
                className={`conn-step ${stepDone ? "done" : stepCurrent ? "current" : ""}`}
              >
                {flowStageLabel(step)}
              </span>
            </Fragment>
          );
        })}
      </div>

      {/* Live stage message */}
      {flowStatus?.message ? (
        <p className="cell-sub" style={{ marginTop: 8 }}>
          {flowStatus.message}
        </p>
      ) : null}

      {/* Environment evidence */}
      {environment ? (
        <p className="cell-sub" style={{ marginTop: 4 }}>
          <b>Environment:</b> {environment.summary}
          {environment.signals.length > 0 ? ` (${environment.signals.join(", ")})` : ""}
        </p>
      ) : null}

      {/* Measured result summary */}
      {flowStatus?.last_result ? (
        <div className="stat-grid" style={{ marginTop: 12 }}>
          <StatTile
            label="Valid candidates"
            value={formatNumber(flowStatus.last_result.valid)}
            sub={`${formatNumber(flowStatus.last_result.duplicates)} duplicates removed`}
          />
          <StatTile
            label="Tested"
            value={formatNumber(flowStatus.last_result.tested)}
            sub="this flow run"
          />
          <StatTile
            label="Verification"
            value={flowStatus.last_result.verified ? "verified" : "failed"}
            sub={flowStatus.last_result.failure_class || "usable connectivity"}
          />
          <StatTile
            label="Flow duration"
            value={`${(flowStatus.last_result.duration_ms / 1000).toFixed(1)}s`}
            sub="measured"
          />
        </div>
      ) : null}

      {error ? <div className="error-banner mt-4">{error}</div> : null}

      <div className="toolbar" style={{ marginTop: 12 }}>
        <button
          type="button"
          className="btn primary lg"
          disabled={running || connected}
          onClick={() => void run()}
        >
          <IconPlay size={14} />
          {connected ? "Connected" : "Start"}
        </button>
        <button
          type="button"
          className="btn ghost"
          disabled={running}
          onClick={() => void run()}
          title="Re-run the flow to refresh candidates and re-verify"
        >
          Re-run flow
        </button>
        <button type="button" className="btn ghost" onClick={() => onNavigate("sources")}>
          Sources &amp; health
        </button>
      </div>
    </section>
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
            event: "",
            batch_id: "",
            test_id: "",
            config_id: "",
            core: "",
            errors_only: false,
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

/**
 * OnboardingChecklist — the v0.9.0 first-launch experience (§12):
 * detect cores → install a core → import configurations → test →
 * connect. Every step links straight to its page; the checklist
 * disappears once the user is connected.
 */
function OnboardingChecklist({
  onNavigate,
  coresAvailable,
  configCount,
  connected,
  connecting,
}: {
  onNavigate: (page: Page) => void;
  coresAvailable: number;
  configCount: number;
  connected: boolean;
  connecting: boolean;
}) {
  const steps = [
    {
      key: "core",
      done: coresAvailable > 0,
      title: "Install a protocol core",
      hint: "One click installs Xray, V2Ray or sing-box from the official upstream release.",
      page: "cores" as Page,
    },
    {
      key: "configs",
      done: configCount > 0,
      title: "Import configurations",
      hint: "Add a public source or refresh the built-in ones to populate the database.",
      page: "sources" as Page,
    },
    {
      key: "test",
      done: false,
      title: "Test configurations",
      hint: "Bulk-test from the Configurations page — results include a real ping.",
      page: "configs" as Page,
    },
    {
      key: "connect",
      done: connected,
      title: "Connect",
      hint: "Pick the fastest working configuration and start the tunnel.",
      page: "connection" as Page,
    },
  ];

  const remaining = steps.filter((s) => !s.done);

  if (remaining.length === 0 || connecting) {
    return null;
  }

  return (
    <section className="card onboarding" aria-label="Get started">
      <h3 className="card-title">Get started</h3>
      <ol className="onboarding-steps">
        {steps.map((step, index) => (
          <li key={step.key} className={step.done ? "done" : ""}>
            <span className="step-num" aria-hidden>
              {step.done ? <IconCheck size={12} /> : index + 1}
            </span>
            <span className="step-body">
              <b>{step.title}</b>
              <span className="muted">{step.hint}</span>
            </span>
            {!step.done && (
              <button type="button" className="btn sm ghost" onClick={() => onNavigate(step.page)}>
                {step.key === "core" && coresAvailable === 0 ? "Go to Cores" : "Open"}
              </button>
            )}
          </li>
        ))}
      </ol>
    </section>
  );
}
