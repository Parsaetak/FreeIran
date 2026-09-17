/**
 * UI-level shared types.
 *
 * Backend models are NOT duplicated here — they come from the
 * generated bindings via src/services (single source of truth). This
 * file only holds frontend-own concepts.
 */

/** Top-level navigation pages. v0.9.8 adds "quick" (Quick Connect). */
export type Page =
  | "quick"
  | "dashboard"
  | "sources"
  | "configs"
  | "cores"
  | "connection"
  | "network"
  | "diagnostics"
  | "settings";

/** Connection state of the UI towards the desktop backend. */
export type BackendConnection = "connected" | "unavailable" | "unknown";

/** Rows used by the export worker. */
export interface ExportRow {
  type: string;
  address: string;
  port: number;
  working: boolean;
  latency_ms: number;
  source: string;
}
