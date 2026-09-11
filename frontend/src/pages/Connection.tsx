import { Fragment, useEffect, useMemo, useState } from "react";
import { useConnectionStore } from "../state/connectionStore";
import { useConfigsStore, makeSearchRunner } from "../state/stores";
import { useSettingsStore } from "../state/settingsStore";
import { type BackendView, type Config } from "../services";
import { formatDuration, formatLatency, formatNumber, formatUptime, truncate } from "../utilities/format";
import {
  BootDots,
  CONNECTION_STATE_LABELS,
  CONNECT_STEPS,
  OrbIcon,
  connectStepIndex,
  connectionUiState,
} from "../components/common";
import {
  IconPlay,
  IconRefresh,
  IconSearch,
  IconStar,
  IconStop,
} from "../components/Icons";
import { toast } from "../state/toastStore";

const searchRunner = makeSearchRunner(250);

/**
 * Connection page: the connection state machine with per-state
 * animations, quick actions, configuration picker, attempt history
 * and the protocol-core inventory (§29).
 */
export function ConnectionPage() {
  const snapshot = useConnectionStore((state) => state.snapshot);
  const backends = useConnectionStore((state) => state.backends);
  const busy = useConnectionStore((state) => state.busy);
  const error = useConnectionStore((state) => state.error);
  const disconnect = useConnectionStore((state) => state.disconnect);
  const reconnect = useConnectionStore((state) => state.reconnect);
  const refreshBackends = useConnectionStore((state) => state.refreshBackends);

  const uiState = snapshot ? connectionUiState(snapshot.state) : "disconnected";
  const connected = uiState === "connected";

  const [scanning, setScanning] = useState(false);

  // Uptime ticker — only runs while a session is live.
  const [, setTick] = useState(0);

  useEffect(() => {
    if (!connected) return;

    const timer = window.setInterval(() => setTick((t) => t + 1), 1000);

    return () => window.clearInterval(timer);
  }, [connected]);

  const rescan = async () => {
    setScanning(true);

    try {
      await refreshBackends();
      toast("info", "Core scan complete", "Protocol-core availability refreshed.");
    } finally {
      setScanning(false);
    }
  };

  const primaryConnect = () => {
    if (!snapshot?.config_id) {
      document.getElementById("config-picker")?.scrollIntoView({ behavior: "smooth" });

      return;
    }

    void reconnect().then(() => {
      const failure = useConnectionStore.getState().error;

      if (failure) toast("error", "Reconnect failed", failure);
    });
  };

  return (
    <div>
      <div className="page-header">
        <div>
          <h1 className="page-title">Connection</h1>
          <div className="page-subtitle">
            Credential-free view — all configuration material is redacted by the engine.
          </div>
        </div>
      </div>

      {/* State machine panel */}
      <section className="conn-panel" aria-label="Connection state">
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

          {snapshot && (
            <div className="conn-facts">
              <span className="conn-fact">
                <span>config</span>
                <b>{snapshot.config_name || snapshot.config_display || "—"}</b>
              </span>
              <span className="conn-fact">
                <span>core</span>
                <b>{snapshot.core ? `${snapshot.core} ${snapshot.core_version ?? ""}` : "—"}</b>
              </span>
              <span className="conn-fact">
                <span>latency</span>
                <b>{connected ? formatLatency(snapshot.latency_ms) : "—"}</b>
              </span>
              <span className="conn-fact hide-md">
                <span>endpoint</span>
                <b>{connected ? snapshot.endpoint || "—" : "—"}</b>
              </span>
              {connected && snapshot.started_at ? (
                <span className="conn-fact">
                  <span>uptime</span>
                  <b>{formatUptime(Date.now() - snapshot.started_at)}</b>
                </span>
              ) : null}
              {snapshot.fallbacks_used ? (
                <span className="conn-fact">
                  <span>fallbacks</span>
                  <b>{formatNumber(snapshot.fallbacks_used)}</b>
                </span>
              ) : null}
            </div>
          )}

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

          {snapshot?.last_error && uiState === "failed" && (
            <div className="error-banner flush-bottom">{snapshot.last_error}</div>
          )}

          {uiState === "disconnected" && (
            <div className="page-subtitle">No active session. Pick a configuration below and connect.</div>
          )}
        </div>

        <div className="conn-actions">
          <button
            type="button"
            className="btn primary lg"
            disabled={connected || uiState === "connecting" || uiState === "disconnecting" || busy}
            onClick={primaryConnect}
          >
            <IconPlay size={14} />
            {snapshot?.config_id ? "Reconnect" : "Connect"}
          </button>

          <button
            type="button"
            className="btn danger lg"
            disabled={(!connected && uiState !== "connecting") || busy}
            onClick={() => void disconnect()}
          >
            <IconStop size={14} />
            Disconnect
          </button>
        </div>
      </section>

      {error && <div className="error-banner">{error}</div>}

      <ConfigPicker disabled={uiState === "connecting" || connected || busy} />

      <CoresCard backends={backends} scanning={scanning} onRescan={() => void rescan()} />

      {snapshot && (snapshot.attempts?.length ?? 0) > 0 && (
        <div className="card">
          <div className="card-header">
            <h3 className="card-title eyebrow">Attempt history</h3>
          </div>

          <table className="data-table">
            <thead>
              <tr>
                <th>Backend</th>
                <th>Result</th>
                <th>Duration</th>
                <th>Detail</th>
              </tr>
            </thead>
            <tbody>
              {snapshot.attempts?.map((attempt, index) => (
                <tr key={`${attempt.backend}-${index}`}>
                  <td className="mono-cell">{attempt.backend}</td>
                  <td>
                    <span className={`badge ${attempt.ok ? "success" : "error"}`}>
                      {attempt.ok ? "started" : "failed"}
                    </span>
                  </td>
                  <td className="mono-cell">{formatDuration(attempt.duration_ms)}</td>
                  <td>{attempt.error || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

/** Searchable configuration picker feeding the connect action. */
function ConfigPicker({ disabled }: { disabled: boolean }) {
  const items = useConfigsStore((state) => state.items);
  const loadPage = useConfigsStore((state) => state.loadPage);
  const setSearchQuery = useConfigsStore((state) => state.setSearchQuery);
  const searchQuery = useConfigsStore((state) => state.searchQuery);
  const connect = useConnectionStore((state) => state.connect);

  const [selected, setSelected] = useState("");

  useEffect(() => {
    if (items.length === 0) void loadPage(0);
  }, [items.length, loadPage]);

  const top = useMemo(() => items.slice(0, 60), [items]);

  const connectSelected = (id: string) => {
    void connect(id).then(() => {
      const failure = useConnectionStore.getState().error;

      if (failure) toast("error", "Connection failed", failure);
    });
  };

  return (
    <div className="card" id="config-picker">
      <div className="card-header">
        <h3 className="card-title eyebrow">Connect a configuration</h3>
      </div>

      <div className="card-body">
        <div className="toolbar">
          <input
            className="input"
            placeholder="Search configurations…"
            aria-label="Search configurations"
            value={searchQuery}
            onChange={(event) => {
              setSearchQuery(event.target.value);
              searchRunner();
            }}
          />
        </div>

        {top.length === 0 ? (
          <div className="log-empty">
            <IconSearch size={16} /> No configurations loaded yet.
          </div>
        ) : (
          <div className="picker-list" role="listbox" aria-label="Configurations">
            {top.map((config) => (
              <PickerRow
                key={String(config["id"])}
                config={config}
                selected={selected === String(config["id"])}
                disabled={disabled}
                onSelect={() => setSelected(String(config["id"]))}
                onConnect={() => connectSelected(String(config["id"]))}
              />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function PickerRow({
  config,
  selected,
  disabled,
  onSelect,
  onConnect,
}: {
  config: Config;
  selected: boolean;
  disabled: boolean;
  onSelect: () => void;
  onConnect: () => void;
}) {
  return (
    <div
      role="option"
      aria-selected={selected}
      tabIndex={0}
      className={`row hoverable selectable ${selected ? "selected" : ""}`}
      onClick={onSelect}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onSelect();
        }
      }}
    >
      <span className={`proto-badge proto-${config["type"] || "unknown"}`}>
        {String(config["type"] || "—")}
      </span>

      <span className="cell-main">
        <div className="cell-title">{String(config["name"] || "unnamed")}</div>
        <div className="cell-sub">
          {truncate(String(config["address"]), 36)}:{String(config["port"])}
        </div>
      </span>

      <button
        type="button"
        className="btn sm primary"
        disabled={disabled || !selected}
        onClick={(event) => {
          event.stopPropagation();
          onConnect();
        }}
      >
        <IconPlay size={11} />
        Connect
      </button>
    </div>
  );
}

/** Protocol-core inventory (§29): status, version, path, notes, preference. */
function CoresCard({
  backends,
  scanning,
  onRescan,
}: {
  backends: BackendView[];
  scanning: boolean;
  onRescan: () => void;
}) {
  const settings = useSettingsStore((state) => state.settings);
  const preferred = settings?.preferred_backend ?? "";

  return (
    <div className="card">
      <div className="card-header">
        <h3 className="card-title eyebrow">Protocol cores</h3>

        {scanning ? (
          <span className="chip">
            scanning <BootDots />
          </span>
        ) : (
          <button type="button" className="btn sm" onClick={onRescan}>
            <IconRefresh size={13} />
            Re-scan
          </button>
        )}
      </div>

      {backends.length === 0 ? (
        <div className="log-empty">
          {scanning
            ? "Scanning for protocol cores…"
            : "No protocol cores discovered. Place xray, v2ray or sing-box executables in the managed cores directory or PATH."}
        </div>
      ) : (
        <div className="stat-grid cores-grid">
          {backends.map((backend) => (
            <CoreCard key={backend.name} backend={backend} preferred={preferred === backend.name} />
          ))}
        </div>
      )}
    </div>
  );
}

function CoreCard({ backend, preferred }: { backend: BackendView; preferred: boolean }) {
  const statusClass =
    backend.status === "available"
      ? "success"
      : backend.status === "invalid"
        ? "error"
        : "neutral";

  return (
    <div className={`stat core-tile ${preferred ? "preferred" : ""}`}>
      <div className="core-tile-head">
        <span className="core-name">{backend.name}</span>
        {preferred && (
          <span className="core-preferred" data-tip="Preferred backend (Settings)">
            <IconStar size={12} filled />
            preferred
          </span>
        )}
      </div>

      <div className="core-tile-status">
        <span className={`badge ${statusClass}`}>{backend.status || "unknown"}</span>
        {backend.version && <span className="mono-cell">{backend.version}</span>}
      </div>

      {backend.path && (
        <div className="cell-sub" data-tip={backend.path}>
          {truncate(backend.path, 34)}
        </div>
      )}

      {(backend.pinned_version || backend.source) && (
        <div className="core-fact">
          pinned {backend.pinned_version || "—"}
          {backend.source ? ` · ${backend.source}` : ""}
        </div>
      )}

      {backend.summary && <div className="core-fact">{backend.summary}</div>}

      {backend.note && <div className="core-fact warn">{backend.note}</div>}

      {backend.notes && backend.notes.length > 0 && (
        <div className="core-fact">{backend.notes.join(" · ")}</div>
      )}

      {backend.last_check ? (
        <div className="core-fact dim">last check {new Date(backend.last_check).toLocaleString()}</div>
      ) : null}

      {backend.status !== "available" && !backend.note && (
        <div className="core-fact dim">
          Install {backend.name} into the managed cores directory or PATH.
        </div>
      )}
    </div>
  );
}
