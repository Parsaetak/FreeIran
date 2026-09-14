import { create } from "zustand";
import { Events } from "@wailsio/runtime";
import { appService, call, type AppState } from "../services";

/**
 * AppState fields arrive as generated class instances; the store keeps
 * a plain structural view so React re-renders only on real changes.
 */
export type AppStatus =
  | "loading"
  | "ready"
  | "degraded"
  | "ingesting"
  | "shutting_down"
  | "backend_unavailable";

interface AppStore {
  status: AppStatus;
  backend: AppState | null;
  lastError: string | null;
  connected: boolean;
  /** Unified boot lifecycle phase from the backend (bootphase.go). */
  bootPhase: string | null;
  /** Phase → elapsed-ms startup telemetry from the backend. */
  bootTimings: Record<string, number> | null;

  refresh: () => Promise<void>;
  ingestEvent: (state: AppState) => void;
}

/**
 * The backend AppState gained boot_phase/boot_timings in v0.9.4; the
 * checked-in generated binding class does not carry the new fields
 * yet, so they are read structurally — type-safe, dependency-free.
 */
interface BootFields {
  boot_phase?: string;
  boot_timings?: Record<string, number>;
}

function readBootFields(state: AppState | null): {
  bootPhase: string | null;
  bootTimings: Record<string, number> | null;
} {
  if (!state) return { bootPhase: null, bootTimings: null };

  const fields = state as unknown as BootFields;

  return {
    bootPhase: fields.boot_phase ?? null,
    bootTimings: fields.boot_timings ?? null,
  };
}

function deriveStatus(state: AppState | null, connected: boolean): AppStatus {
  if (!connected) return "backend_unavailable";
  if (!state) return "loading";
  if (state.status === "ready" && state.ingestion_running) return "ingesting";
  if (state.status === "ready") return "ready";
  if (state.status === "degraded") return "degraded";
  if (state.status === "shutting_down") return "shutting_down";

  return "loading";
}

export const useAppStore = create<AppStore>((set) => ({
  status: "loading",
  backend: null,
  lastError: null,
  connected: false,
  bootPhase: null,
  bootTimings: null,

  refresh: async () => {
    try {
      const state = await call(() => appService.State());

      set({
        backend: state,
        connected: true,
        status: deriveStatus(state, true),
        lastError: null,
        ...readBootFields(state),
      });
    } catch (error) {
      set({
        connected: false,
        status: "backend_unavailable",
        lastError: error instanceof Error ? error.message : String(error),
      });
    }
  },

  ingestEvent: (state: AppState) =>
    set({
      backend: state,
      connected: true,
      status: deriveStatus(state, true),
      lastError: null,
      ...readBootFields(state),
    }),
}));

/**
 * Subscribes to backend state broadcasts ("freeiran:state") and
 * performs an initial fetch. Returns a disposer.
 */
export function connectAppStore(): () => void {
  void useAppStore.getState().refresh();

  const off = Events.On("freeiran:state", (event: { data: AppState }) => {
    useAppStore.getState().ingestEvent(event.data);
  });

  return () => {
    if (typeof off === "function") off();
  };
}
