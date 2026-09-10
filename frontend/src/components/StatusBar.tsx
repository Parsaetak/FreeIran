import { useAppStore } from "../state/appStore";

const STATUS_LABELS: Record<string, string> = {
  loading: "Loading",
  ready: "Ready",
  degraded: "Degraded",
  ingesting: "Refreshing sources",
  shutting_down: "Shutting down",
  backend_unavailable: "Backend unavailable",
};

export function StatusBar() {
  const status = useAppStore((state) => state.status);
  const backend = useAppStore((state) => state.backend);
  const lastError = useAppStore((state) => state.lastError);

  const dotClass =
    status === "ready"
      ? "ok"
      : status === "ingesting" || status === "loading"
        ? "busy"
        : "bad";

  return (
    <footer className="status-bar">
      <span className="status-pill">
        <span className={`status-dot ${dotClass}`} />
        {STATUS_LABELS[status] ?? status}
      </span>

      {backend && <span>v{backend.version}</span>}
      {backend && <span>{backend.config_count} configurations</span>}
      {backend && <span>accel: {backend.native_acceleration}</span>}
      {lastError && <span title={lastError}>⚠ {lastError}</span>}
    </footer>
  );
}
