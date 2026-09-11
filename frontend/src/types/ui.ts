/**
 * UI-level shared types.
 *
 * Backend models are NOT duplicated here — they come from the
 * generated bindings via src/services (single source of truth). This
 * file only holds frontend-own concepts.
 */

/** Top-level navigation pages. */
export type Page = "dashboard" | "sources" | "configs" | "connection" | "diagnostics";

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
