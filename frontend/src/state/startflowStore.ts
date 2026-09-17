/**
 * Start-flow store (v0.9.6 §15): the adaptive
 * START → DETECT → DISCOVER → TEST → RANK → CONNECT → VERIFY flow,
 * fed by "freeiran:startflow" events plus polling fallback for the
 * terminal status. Progress is real: stage transitions arrive from
 * the engine with measured durations — nothing is animated.
 */

import { create } from "zustand";
import { Events } from "@wailsio/runtime";
import { call, discoveryService } from "../services";
import type {
  EnvironmentReport,
  StartFlowEvent,
  StartFlowResult,
  StartFlowStage,
  StartFlowStatus,
} from "../types/discovery";

interface StartFlowState {
  /** Current status snapshot from the backend. */
  status: StartFlowStatus | null;
  /** Latest environment analysis. */
  environment: EnvironmentReport | null;
  /** Whether a flow-triggered connect is in flight. */
  busy: boolean;
  /** Error from the last flow run, if any. */
  error: string | null;
  /** Last completed result (kept for the summary card). */
  lastResult: StartFlowResult | null;

  refresh: () => Promise<void>;
  refreshEnvironment: () => Promise<void>;
  run: (manualFingerprint?: string) => Promise<void>;
  cancel: () => Promise<void>;
  discoverNow: (deep: boolean) => Promise<void>;
}

const STAGE_ORDER: StartFlowStage[] = [
  "idle",
  "detecting",
  "discovering",
  "testing",
  "ranking",
  "connecting",
  "verifying",
  "connected",
  "failed",
  "no_usable_candidates",
];

function stageIndex(stage: StartFlowStage | string | undefined): number {
  if (!stage) return 0;
  const idx = STAGE_ORDER.indexOf(stage as StartFlowStage);
  return idx < 0 ? 0 : idx;
}

export const useStartFlowStore = create<StartFlowState>((set, get) => ({
  status: null,
  environment: null,
  busy: false,
  error: null,
  lastResult: null,

  refresh: async () => {
    try {
      const status = await call(() => discoveryService.StartFlowStatus());
      set({ status: status as StartFlowStatus });
    } catch {
      // The backend may not be bound yet; the next refresh retries.
    }
  },

  refreshEnvironment: async () => {
    try {
      const env = await call(() => discoveryService.Environment());
      set({ environment: env as EnvironmentReport });
    } catch {
      // Environment analysis is best-effort for display.
    }
  },

  run: async (manualFingerprint?: string) => {
    if (get().busy) return;
    set({ busy: true, error: null });
    try {
      const result = await call(() =>
        discoveryService.RunStartFlow(manualFingerprint ?? ""),
      );
      set({ lastResult: (result as StartFlowResult) ?? null });
      await get().refresh();
    } catch (error) {
      set({
        error: error instanceof Error ? error.message : String(error),
      });
    } finally {
      set({ busy: false });
      await get().refresh();
    }
  },

  cancel: async () => {
    try {
      await call(() => discoveryService.CancelStartFlow());
    } catch {
      // Cancellation is best-effort.
    } finally {
      await get().refresh();
    }
  },

  discoverNow: async (deep: boolean) => {
    if (get().busy) return;
    set({ busy: true, error: null });
    try {
      await call(() => discoveryService.DiscoverNow(deep));
    } catch (error) {
      set({
        error: error instanceof Error ? error.message : String(error),
      });
    } finally {
      set({ busy: false });
    }
  },
}));

/**
 * Subscribes to start-flow broadcasts ("freeiran:startflow") and
 * keeps the store's status in sync. Returns the unsubscribe function.
 */
export function subscribeStartFlow(): () => void {
  const off = Events.On(
    "freeiran:startflow",
    (event: { data: StartFlowEvent }) => {
      const ev = event.data;
      if (!ev) return;
      useStartFlowStore.setState((state) => ({
        status: {
          ...(state.status ?? {
            stage: "idle",
            running: false,
          }),
          stage: ev.stage,
          message: ev.message ?? undefined,
          running: ev.stage !== "connected" && ev.stage !== "failed" &&
            ev.stage !== "no_usable_candidates",
        },
        lastResult:
          ev.detail && (ev.detail as Record<string, unknown>)["result"]
            ? ((ev.detail as Record<string, unknown>)["result"] as StartFlowResult)
            : state.lastResult,
      }));
    },
  );
  return off;
}

export { stageIndex, STAGE_ORDER };
