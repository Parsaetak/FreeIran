/**
 * Discovery-surface types (v0.9.6).
 *
 * These describe the JSON shapes returned by the hand-written
 * DiscoveryService bindings (bindings/.../discoveryservice.js). The
 * authoritative definitions live in engine/app/discoveryservice.go,
 * engine/discovery and engine/netcheck; this file mirrors the wire
 * shapes for the UI.
 */

/** One stage of the adaptive start flow. */
export type StartFlowStage =
  | "detecting"
  | "discovering"
  | "testing"
  | "ranking"
  | "connecting"
  | "verifying"
  | "connected"
  | "failed"
  | "no_usable_candidates"
  | "idle";

/** One progress event of the start flow. */
export interface StartFlowEvent {
  stage: StartFlowStage;
  message?: string;
  duration_ms?: number;
  detail?: Record<string, unknown>;
  at: number;
}

/** Result summary of one completed flow run. */
export interface StartFlowResult {
  discovered: number;
  valid: number;
  duplicates: number;
  tested: number;
  connected_fingerprint?: string;
  verified: boolean;
  failure_class?: string;
  duration_ms: number;
}

/** Current status of the flow. */
export interface StartFlowStatus {
  stage: StartFlowStage;
  running: boolean;
  message?: string;
  started_at?: number;
  finished_at?: number;
  last_run_ms?: number;
  environment?: string[];
  last_result?: StartFlowResult | null;
}

/** Measured health of one configuration source. */
export interface SourceHealth {
  source_id: string;
  fetches: number;
  fetch_ok: number;
  parse_ok: number;
  candidates: number;
  valid: number;
  duplicates: number;
  last_latency_ms?: number;
  last_success_at?: number;
  last_failure_at?: number;
  consecutive_failures?: number;
  last_error?: string;
}

/** Per-level statistics of one discovery run. */
export interface DiscoveryLevelStats {
  level: number;
  name: string;
  sources: number;
  fetched_ok: number;
  failed: number;
  candidates: number;
  valid: number;
  duplicates: number;
  duration_ms: number;
  skipped?: boolean;
  skip_reason?: string;
}

/** Summary of one discovery run. */
export interface DiscoveryStats {
  levels: DiscoveryLevelStats[];
  discovered: number;
  valid: number;
  duplicates: number;
  duration_ms: number;
}

/** Environment analysis result. */
export interface EnvironmentReport {
  signals: string[];
  restricted: boolean;
  deep_discovery_advised: boolean;
  summary: string;
  analyzed_at: string;
}

/** Test mode identifiers (§9). */
export type TestMode = "ping" | "url" | "ping_url" | "handshake" | "full";

/** All selectable test modes with their UI labels. */
export const TEST_MODES: { id: TestMode; label: string }[] = [
  { id: "ping", label: "Ping" },
  { id: "url", label: "URL" },
  { id: "ping_url", label: "Ping + URL" },
  { id: "handshake", label: "Protocol Handshake" },
  { id: "full", label: "Full Connectivity" },
];

/** Sort mode identifiers (§10). */
export type SortMode =
  | "best_overall"
  | "lowest_ping"
  | "lowest_median_ping"
  | "lowest_jitter"
  | "lowest_packet_loss"
  | "best_url_response"
  | "highest_success_rate"
  | "most_stable"
  | "recently_verified";

/** All selectable sort modes with their UI labels. */
export const SORT_MODES: { id: SortMode; label: string }[] = [
  { id: "best_overall", label: "Best Overall" },
  { id: "lowest_ping", label: "Lowest Ping" },
  { id: "lowest_median_ping", label: "Lowest Median Ping" },
  { id: "lowest_jitter", label: "Lowest Jitter" },
  { id: "lowest_packet_loss", label: "Lowest Packet Loss" },
  { id: "best_url_response", label: "Best URL Response" },
  { id: "highest_success_rate", label: "Highest Success Rate" },
  { id: "most_stable", label: "Most Stable" },
  { id: "recently_verified", label: "Recently Verified" },
];

/** Latency-provenance label for honest display (§10). */
export type Provenance = "measured" | "estimated" | "unavailable" | "stale";

/** Render a provenance as a UI chip label. */
export function provenanceLabel(p: Provenance | string | undefined): string {
  switch (p) {
    case "measured":
      return "measured";
    case "estimated":
      return "estimated";
    case "stale":
      return "stale";
    default:
      return "not measured";
  }
}

/** Human label for a flow stage. */
export function flowStageLabel(stage: StartFlowStage | string): string {
  switch (stage) {
    case "detecting":
      return "Detecting environment";
    case "discovering":
      return "Discovering";
    case "testing":
      return "Testing";
    case "ranking":
      return "Ranking";
    case "connecting":
      return "Connecting";
    case "verifying":
      return "Verifying";
    case "connected":
      return "Connected";
    case "failed":
      return "Failed";
    case "no_usable_candidates":
      return "No usable candidates";
    default:
      return "Idle";
  }
}
