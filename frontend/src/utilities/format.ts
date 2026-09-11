/** Small formatting helpers shared by the UI. */

export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";

  const units = ["B", "KiB", "MiB", "GiB"];
  const index = Math.min(
    units.length - 1,
    Math.floor(Math.log(bytes) / Math.log(1024)),
  );

  const value = bytes / 1024 ** index;

  return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`;
}

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "0 ms";

  if (ms < 1000) return `${Math.round(ms)} ms`;

  const seconds = ms / 1000;

  if (seconds < 60) return `${seconds.toFixed(1)} s`;

  const minutes = Math.floor(seconds / 60);
  const rest = Math.round(seconds % 60);

  return `${minutes}m ${rest}s`;
}

export function formatNumber(value: number): string {
  return new Intl.NumberFormat().format(value);
}

export function formatPercent(rate: number): string {
  return `${(rate * 100).toFixed(1)}%`;
}

export function truncate(value: string, max = 42): string {
  return value.length > max ? `${value.slice(0, max - 1)}…` : value;
}

export function relativeTime(unixMS: number): string {
  if (!unixMS) return "never";

  const delta = Date.now() - unixMS;

  if (delta < 60_000) return "just now";
  if (delta < 3_600_000) return `${Math.floor(delta / 60_000)}m ago`;
  if (delta < 86_400_000) return `${Math.floor(delta / 3_600_000)}h ago`;

  return `${Math.floor(delta / 86_400_000)}d ago`;
}

/** Monospace-friendly latency label; "—" for untested/invalid values. */
export function formatLatency(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || !Number.isFinite(ms) || ms <= 0) {
    return "—";
  }

  return `${Math.round(ms)} ms`;
}

/** Severity class for latency color coding (<300 ok, <800 warn, else error). */
export function latencyClass(
  ms: number | undefined | null,
): "ok" | "warn" | "err" | "none" {
  if (ms === undefined || ms === null || !Number.isFinite(ms) || ms <= 0) {
    return "none";
  }

  if (ms < 300) return "ok";
  if (ms < 800) return "warn";

  return "err";
}

/** Local wall-clock time (HH:MM:SS) for RFC3339 log timestamps. */
export function formatClock(ts: string): string {
  if (!ts) return "";

  const date = new Date(ts);

  if (Number.isNaN(date.getTime())) return ts;

  const pad = (n: number) => String(n).padStart(2, "0");

  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

/** Human uptime for running sessions (e.g. "2m 07s", "3h 12m", "2d 4h"). */
export function formatUptime(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "0s";

  const totalSeconds = Math.floor(ms / 1000);
  const days = Math.floor(totalSeconds / 86_400);
  const hours = Math.floor((totalSeconds % 86_400) / 3_600);
  const minutes = Math.floor((totalSeconds % 3_600) / 60);
  const seconds = totalSeconds % 60;

  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${String(minutes).padStart(2, "0")}m`;
  if (minutes > 0) return `${minutes}m ${String(seconds).padStart(2, "0")}s`;

  return `${seconds}s`;
}
