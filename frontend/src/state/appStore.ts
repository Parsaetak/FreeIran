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

  refresh: () => Promise<void>;
  ingestEvent: (state: AppState) => void;
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

  refresh: async () => {
    try {
      const state = await call(() => appService.State());

      set({
        backend: state,
        connected: true,
        status: deriveStatus(state, true),
        lastError: null,
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
