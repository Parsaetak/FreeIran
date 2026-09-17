import { formatLatency } from "./format";

/**
 * v0.9.7 UI-model helpers for the configuration browser rows
 * (§6/§18): the test-state vocabulary shared by rows and the detail
 * panel, plus the client-side filter used before server results land.
 * Kept as a pure module so the action-column behaviour is unit
 * testable without a DOM.
 */

export type ConfigRow = Record<string, unknown>;

export type TestState =
  | "queued"
  | "testing"
  | "none";

/**
 * testStateFor resolves the live row state chip: "testing" wins over
 * "queued" (an in-flight row is both), otherwise "none" — the row
 * falls back to its stored health badge.
 */
export function testStateFor(
  row: ConfigRow,
  isTesting: boolean,
  queuedIds: Set<string>,
): TestState {
  const id = String(row["id"] ?? "");

  if (isTesting) return "testing";
  if (queuedIds.has(id)) return "queued";

  return "none";
}

/**
 * applyConfigFilters narrows rows client-side by protocol and
 * case-insensitive name/address substring. Search terms longer than
 * the address are harmless; ids are never truncated (the actions
 * column relies on the exact id).
 */
export function applyConfigFilters(
  rows: ConfigRow[],
  filters: { protocol?: string; query?: string },
): ConfigRow[] {
  const query = (filters.query ?? "").trim().toLowerCase();
  const protocol = filters.protocol ?? "";

  return rows.filter((row) => {
    if (protocol && String(row["type"]) !== protocol) {
      return false;
    }

    if (!query) {
      return true;
    }

    const haystack = [
      String(row["name"] ?? ""),
      String(row["address"] ?? ""),
      String(row["type"] ?? ""),
    ]
      .join(" ")
      .toLowerCase();

    return haystack.includes(query);
  });
}

/**
 * formatStaleLatency renders a measured latency with a stale marker
 * when the sample is older than the freshness window (30 minutes) —
 * stale measurements are labelled, never silently shown (§18).
 */
export function formatStaleLatency(medianMS: number, testedAtMS: number): string {
  const base = formatLatency(medianMS);

  if (testedAtMS > 0 && Date.now() - testedAtMS > 30 * 60 * 1000) {
    return `${base} (stale)`;
  }

  return base;
}
