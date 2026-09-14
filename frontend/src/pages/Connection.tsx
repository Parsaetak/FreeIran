import { Fragment, useEffect, useMemo, useState } from "react";
import { useConnectionStore } from "../state/connectionStore";
import { useConfigsStore, makeSearchRunner } from "../state/stores";
import { useSettingsStore } from "../state/settingsStore";
import { call, type BackendView, type CandidateView, type Config } from "../services";
import { connectionService, tunnelService } from "../services";
import { formatDuration, formatLatency, formatNumber, formatUptime, truncate } from "../utilities/format";
import {
  BootDots,
  CONNECTION_STATE_LABELS,
  CONNECT_STEPS,
  OrbIcon,
  TechDetails,
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
 * ConnectionFailure renders "what happened + why + what to do next":
 * the friendly part of the backend message, a targeted suggestion
 * when the failure kind is recognizable, and the raw technical text
 * behind an expandable disclosure (v0.9.1).
 */
function ConnectionFailure({ message }: { message: string }) {
  const marker = "\n---\nTechnical details: ";
  const idx = message.indexOf(marker);

  const readable = idx >= 0 ? message.slice(0, idx) : message;
  const technical = idx >= 0 ? message.slice(idx + marker.length) : "";
  const haystack = message.toLowerCase();

  let next = "";

  if (haystack.includes("no core") || haystack.includes("no available backend") || haystack.includes("not installed")) {
    next = "Install a protocol core on the Cores page, then try again.";
  } else if (haystack.includes("timeout") || haystack.includes("timed out")) {
    next = "The server did not answer in time — it may be offline or very slow. Try another configuration.";
  } else if (haystack.includes("auth") || haystack.includes("credential") || haystack.includes("uuid")) {
    next = "The server rejected the credentials in this configuration. It is probably outdated — pick a fresher one.";
  } else if (haystack.includes("dns") || haystack.includes("lookup")) {
    next = "The server address could not be resolved. Check your Internet connection or switch networks.";
  } else if (haystack.includes("refused") || haystack.includes("unreachable") || haystack.includes("network")) {
    next = "The server refused the connection or is unreachable. Run a Network check to confirm your Internet access.";
  } else if (haystack.includes("tun") && haystack.includes("elevat")) {
    next = "TUN mode needs administrator rights. Re-run the application as administrator to use it.";
  } else if (haystack.includes("port")) {
    next = "The local proxy port may be in use by another application. Disconnect other VPN tools and retry.";
  }

  return (
    <div>
      <div>{readable}</div>
      {next && <div className="field-hint">{next}</div>}
      <TechDetails details={technical} />
    </div>
  );
}

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
  const connectBest = useConnectionStore((state) => state.connectBest);
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
    // v0.9.3 autonomous connect: without an explicit previous session,
    // CONNECT picks the best viable candidate from real test history
    // (the engine validates, selects the core, verifies readiness).
    if (!snapshot?.config_id) {
      void connectBest().then(() => {
        const failure = useConnectionStore.getState().error;

        if (failure) toast("error", "Connection failed", failure);
      });

      return;
    }

    void reconnect().then(() => {
      const failure = useConnectionStore.getState().error;

      if (failure) toast("error", "Reconnect failed", failure);
    });
  };

  // Find better connection (§18): switch to the best candidate other
  // than the current one. Available while connected.
  const findBetter = () => {
    const exclude = snapshot?.config_id ? [snapshot.config_id] : [];

    void connectBest(exclude).then((result) => {
      const failure = useConnectionStore.getState().error;

      if (failure) {
        toast("error", "Could not find a better connection", failure);
      } else if (result?.chosen) {
        toast(
          "success",
          "Switched connection",
          `${result.chosen.name || "Best candidate"} — ${result.chosen.explanation?.join(" · ") ?? ""}`.trim(),
        );
      }
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
            <div className="error-banner flush-bottom">
              <div>
                <div>The last connection attempt failed. Pick a different configuration or retry — the error below explains what happened.</div>
                <ConnectionFailure message={snapshot.last_error} />
              </div>
            </div>
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

        {connected && (
          <button type="button" className="btn sm find-better" onClick={findBetter} disabled={busy}>
            <IconRefresh size={13} />
            Find better connection
          </button>
        )}
      </section>

      <BestCandidateCard disabled={uiState === "connecting" || connected || busy} />

      {error && (
        <div className="error-banner">
          <div>
            <div>The action could not complete. See the details below for the cause.</div>
            <ConnectionFailure message={error} />
          </div>
        </div>
      )}

      <ConfigPicker disabled={uiState === "connecting" || connected || busy} />

      <TunnelModeCard connected={connected} endpoint={snapshot?.endpoint ?? ""} />

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

/** TunnelModeCard controls the system-level integration modes
 * (system proxy / TUN). The actions reach the backend through the
 * v0.8 TunnelService bindings — the wiring gap v0.7 shipped with
 * (service existed, was never registered) is closed.
 * Modes require a live session: the tunnel routes system traffic
 * through the connected core's local inbound. */
function TunnelModeCard({ connected, endpoint }: { connected: boolean; endpoint: string }) {
  const [mode, setMode] = useState<"off" | "system_proxy" | "tun" | "">("");
  const [busy, setBusy] = useState(false);

  const [host, port] = useMemo(() => {
    const idx = endpoint.lastIndexOf(":");
    if (idx <= 0) return ["127.0.0.1", 0];
    const parsed = Number.parseInt(endpoint.slice(idx + 1), 10);
    return [endpoint.slice(0, idx), Number.isFinite(parsed) ? parsed : 0];
  }, [endpoint]);

  useEffect(() => {
    let cancelled = false;

    void call(() => tunnelService.State())
      .then((state) => {
        if (cancelled) return;
        const value = String(state ?? "off");
        if (value === "system_proxy" || value === "tun" || value === "off") {
          setMode(value);
        } else {
          setMode("");
        }
      })
      .catch(() => {
        if (!cancelled) setMode("off");
      });

    return () => {
      cancelled = true;
    };
  }, []);

  const apply = async (action: "proxy" | "tun" | "off") => {
    setBusy(true);

    try {
      if (action === "proxy") {
        await call(() => tunnelService.EnableSystemProxy(host, port, false, null));
        setMode("system_proxy");
      } else if (action === "tun") {
        await call(() => tunnelService.EnableTUN(host, port));
        setMode("tun");
      } else {
        await call(() => tunnelService.Disable());
        setMode("off");
      }
    } catch (error) {
      toast("error", "Tunnel mode change failed", describeErrorTunnel(error));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <div className="card-header">
        <h3 className="card-title">System integration</h3>
        <span className={`badge ${mode === "off" || mode === "" ? "info" : "success"}`}>
          {mode === "" ? "unknown" : mode === "system_proxy" ? "system proxy" : mode}
        </span>
      </div>

      <p className="card-subtitle">
        {connected
          ? `Route system traffic through the local inbound at ${host}:${port}.`
          : "Connect first: system proxy and TUN need a live local inbound."}
      </p>

      <div className="toolbar">
        <button
          type="button"
          className="btn sm"
          disabled={!connected || busy || port === 0}
          onClick={() => void apply("proxy")}
        >
          Enable system proxy
        </button>
        <button
          type="button"
          className="btn sm"
          disabled={!connected || busy || port === 0}
          onClick={() => void apply("tun")}
        >
          Enable TUN
        </button>
        <button
          type="button"
          className="btn sm"
          disabled={busy || mode === "off" || mode === ""}
          onClick={() => void apply("off")}
        >
          Disable
        </button>
      </div>
    </div>
  );
}

/** describeErrorTunnel normalizes backend rejection messages. */
function describeErrorTunnel(error: unknown): string {
  return error instanceof Error ? error.message : String(error ?? "unknown error");
}

/**
 * BestCandidateCard surfaces the ranking engine's top choice (§20):
 * name, latency, quality class and the reasons behind the score.
 * Normal users see one honest summary — internals stay hidden.
 */
function BestCandidateCard({ disabled }: { disabled: boolean }) {
  const [candidate, setCandidate] = useState<CandidateView | null>(null);
  const [loading, setLoading] = useState(false);

  const load = async () => {
    setLoading(true);

    try {
      const views = await call(() => connectionService.BestCandidates(1));

      setCandidate(views.length > 0 ? views[0] : null);
    } catch {
      setCandidate(null); // ranking is best-effort for the UI
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const qualityLabel: Record<string, string> = {
    best: "Excellent",
    good: "Good",
    unstable: "Unstable",
    dead: "Down",
    unknown: "Untested",
  };

  const qualityClass: Record<string, string> = {
    best: "success",
    good: "info",
    unstable: "warn",
    dead: "error",
    unknown: "neutral",
  };

  return (
    <div className="card">
      <div className="card-header">
        <h3 className="card-title eyebrow">Best candidate</h3>
        <button type="button" className="btn sm" onClick={() => void load()} disabled={loading}>
          <IconRefresh size={13} />
          Refresh
        </button>
      </div>

      {!candidate ? (
        <div className="log-empty">
          {loading
            ? "Ranking configurations…"
            : "No tested candidates yet. Run Test connections to measure the available servers."}
        </div>
      ) : (
        <div className="card-body best-candidate">
          <div className="cell-main">
            <div className="cell-title">
              {candidate.name || candidate.endpoint || "Unnamed candidate"}
            </div>
            <div className="cell-sub">
              {candidate.protocol.toUpperCase()} · {candidate.endpoint}
            </div>
          </div>

          <div className="best-candidate-facts">
            <span className="conn-fact">
              <span>latency</span>
              <b>{candidate.latency_ms > 0 ? formatLatency(candidate.latency_ms) : "—"}</b>
            </span>
            <span className="conn-fact">
              <span>success</span>
              <b>{Math.round(candidate.success_rate * 100)}%</b>
            </span>
            <span className={`badge ${qualityClass[candidate.class] ?? "neutral"}`}>
              {qualityLabel[candidate.class] ?? candidate.class}
            </span>
          </div>

          {candidate.explanation && candidate.explanation.length > 0 && (
            <div className="cell-sub">{candidate.explanation.join(" · ")}</div>
          )}

          <button
            type="button"
            className="btn sm primary"
            disabled={disabled || !candidate.connectable}
            onClick={() => {
              const store = useConnectionStore.getState();

              void store.connectBest().then(() => {
                const failure = useConnectionStore.getState().error;

                if (failure) toast("error", "Connection failed", failure);
              });
            }}
          >
            <IconPlay size={11} />
            Connect to best
          </button>
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
    const { total, loading, searching, searchQuery } =
      useConfigsStore.getState();

    // Load only when the store has never been filled: an empty items
    // array is also what a zero-result SEARCH looks like, and an
    // unconditional loadPage(0) here would clobber that result with
    // the unfiltered first page (and re-fire per state change).
    if (total === 0 && !loading && !searching && searchQuery.trim() === "") {
      void loadPage(0);
    }
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
