/**
 * Log severity helpers shared by the diagnostics log viewer.
 *
 * Mirrors the backend's level semantics (engine/app/loggingservice.go):
 * debug < info < warn < error, filters mean "at or above".
 */

export const LEVEL_ORDER = ["debug", "info", "warn", "error"] as const;

export type LogLevel = (typeof LEVEL_ORDER)[number];

/** Severity rank of a level string; unknown levels rank as info. */
export function levelRank(level: string): number {
  const index = LEVEL_ORDER.indexOf(level as LogLevel);

  return index === -1 ? 1 : index;
}

/**
 * Whether an entry level passes a minimum-level filter.
 * An empty filter level accepts everything (backend default).
 */
export function levelAtLeast(level: string, minLevel: string): boolean {
  if (!minLevel) return true;

  return levelRank(level) >= levelRank(minLevel);
}

/** Normalizes an arbitrary level string to one of the known levels. */
export function normalizeLevel(level: string): LogLevel {
  return (LEVEL_ORDER as readonly string[]).includes(level)
    ? (level as LogLevel)
    : "info";
}
