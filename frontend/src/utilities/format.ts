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

// Module-level formatter: constructing Intl.NumberFormat is
// comparatively expensive and formatNumber runs inside virtualized
// rows and stat tiles on every render.
const numberFormat = new Intl.NumberFormat();

export function formatNumber(value: number): string {
  return numberFormat.format(value);
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

/** Age sanity bound: anything "older" than 10 years is a broken
 * timestamp (e.g. a year-1 zero time that leaked through a contract),
 * never a real observation. v0.13.0 defense in depth for the source
 * freshness contract. */
const MAX_SANE_AGE_MS = 10 * 365.25 * 86_400_000;

/**
 * v0.13.0 source freshness label (v2rayN-style factual status):
 *
 *   null / undefined / ""  → "Never fetched"
 *   age < 1 min            → "Updated just now"
 *   age < 1 h              → "Updated 12m ago"
 *   same calendar day      → "Updated 13:05"
 *   otherwise              → "Updated Oct 1, 13:05"
 *
 * The exact timestamp belongs in the row's title/detail attribute, not
 * the label. Broken (absurd) timestamps render as "Never fetched" —
 * an impossible age is never shown as if it were real.
 */
export function sourceFreshness(iso: string | null | undefined, now = new Date()): string {
  if (!iso) return "Never fetched";

  const then = new Date(iso).getTime();

  if (!Number.isFinite(then)) return "Never fetched";

  const age = now.getTime() - then;

  if (age < 0 || age > MAX_SANE_AGE_MS) return "Never fetched";

  if (age < 60_000) return "Updated just now";
  if (age < 3_600_000) return `Updated ${Math.floor(age / 60_000)}m ago`;

  const sameDay =
    then >= new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();

  const clock = `${String(new Date(then).getHours()).padStart(2, "0")}:${String(
    new Date(then).getMinutes(),
  ).padStart(2, "0")}`;

  if (sameDay) return `Updated ${clock}`;

  const month = new Date(then).toLocaleString(undefined, { month: "short" });
  const day = new Date(then).getDate();

  return `Updated ${month} ${day}, ${clock}`;
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
