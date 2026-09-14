/**
 * Unified loading-state policy (v0.9.4 §8/§9).
 *
 * One language for every loading surface in the app:
 *
 *   <100 ms   render immediately — no indicator at all
 *   100–180 ms avoid visual disturbance — still nothing
 *   >180 ms   subtle skeleton / placeholder transition
 *   known progress   determinate (boot phases, core install)
 *   unknown duration indeterminate (shimmer, dots)
 *
 * The thresholds live here as the single authority; components call
 * the pure policy functions so the behaviour is unit-testable without
 * React. No artificial delays are ever added — the policy only
 * SUPPRESSES indicators for fast operations.
 */

/** Below this duration an operation is perceptually instant. */
export const IMMEDIATE_MS = 100;

/** At or beyond this duration a skeleton/placeholder becomes visible. */
export const DEFER_SKELETON_MS = 180;

/**
 * Pure visibility policy: a skeleton becomes visible once its load
 * has been running for at least `threshold` ms. `sinceMs === null`
 * means "not loading". Fast loads (< 180 ms) never show a skeleton —
 * the UI renders the real content instead of flickering.
 */
export function skeletonVisible(
  sinceMs: number | null,
  nowMs: number,
  threshold: number = DEFER_SKELETON_MS,
): boolean {
  if (sinceMs === null) return false;
  return nowMs - sinceMs >= threshold;
}

/** Unified load status for pages, panels and components. */
export type LoadStatus = "idle" | "loading" | "ready" | "error";

/**
 * The unified boot lifecycle (engine/app/bootphase.go), in order.
 * BootProgress uses this as the determinate progress scale — the UI
 * shows REAL startup progress, never a fake spinner.
 */
export const BOOT_PHASES: Array<{ key: string; label: string }> = [
  { key: "boot", label: "Starting" },
  { key: "workspace_ready", label: "Workspace ready" },
  { key: "store_metadata_ready", label: "Store ready" },
  { key: "services_ready", label: "Services ready" },
  { key: "ui_runtime_ready", label: "UI runtime ready" },
  { key: "ui_ready", label: "Interface ready" },
  { key: "background_warmup", label: "Preparing data" },
  { key: "ready", label: "Ready" },
];

/**
 * Index of the current boot phase on the determinate scale (-1 when
 * unknown). Phases the frontend never observes (ui_ready is emitted
 * BY the frontend) are still valid positions on the scale.
 */
export function bootPhaseIndex(phase: string | null | undefined): number {
  if (!phase) return -1;
  return BOOT_PHASES.findIndex((entry) => entry.key === phase);
}

/**
 * Human label for a boot phase; unknown phases (forward compat with
 * a newer backend) render raw but never break the indicator.
 */
export function bootPhaseLabel(phase: string | null | undefined): string {
  if (!phase) return "";
  const found = BOOT_PHASES.find((entry) => entry.key === phase);
  return found ? found.label : phase;
}
