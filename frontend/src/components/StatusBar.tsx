import { useAppStore } from "../state/appStore";
import { useConnectionStore } from "../state/connectionStore";
import { CONNECTION_STATE_LABELS } from "./common";
import { formatLatency } from "../utilities/format";

const STATUS_LABELS: Record<string, string> = {
  loading: "Loading",
  ready: "Ready",
  degraded: "Degraded",
  ingesting: "Refreshing sources",
  shutting_down: "Shutting down",
  backend_unavailable: "Backend unavailable",
};

/** Sticky bottom status bar: backend state, version and live session. */
export function StatusBar({ version }: { version?: string }) {
  const status = useAppStore((state) => state.status);
  const backend = useAppStore((state) => state.backend);
  const lastError = useAppStore((state) => state.lastError);
  const snapshot = useConnectionStore((state) => state.snapshot);

  const dotClass =
    status === "ready"
      ? "ok"
      : status === "ingesting" || status === "loading"
        ? "busy"
        : "bad";

  const connected = snapshot?.state === "connected";

  return (
    <footer className="status-bar">
      <span className="status-pill">
        <span className={`status-dot ${dotClass}`} aria-hidden />
        {STATUS_LABELS[status] ?? status}
      </span>

      <span className="status-bar-sep" aria-hidden />

      {snapshot && (
        <span className="status-pill truncate-cell">
          <span
            className={`status-dot ${connected ? "ok" : snapshot.state === "connection_failed" ? "bad" : "busy"}`}
            aria-hidden
          />
          {CONNECTION_STATE_LABELS[snapshot.state] ?? snapshot.state}
          {connected && (
            <span className="mono">
              {" "}
              {snapshot.core} · {formatLatency(snapshot.latency_ms)}
            </span>
          )}
        </span>
      )}

      <span className="status-bar-spacer" />

      {(backend || version) && (
        <span className="truncate-cell">{version || `v${backend?.version}`}</span>
      )}
      {backend && <span className="truncate-cell">{backend.config_count} configurations</span>}
      {backend && <span className="truncate-cell hide-sm">accel: {backend.native_acceleration}</span>}
      {lastError && (
        <span className="truncate-cell" data-tip={lastError}>
          ⚠ {lastError}
        </span>
      )}
    </footer>
  );
}
