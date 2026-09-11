import { create } from "zustand";
import { Events } from "@wailsio/runtime";
import {
  connectionService,
  call,
  type ConnectionSnapshot,
  type BackendView,
  type CoreHealthReport,
} from "../services";

/**
 * Connection store: mirrors the backend connection state machine
 * (engine/connection) through the "freeiran:connection" event plus
 * explicit service calls. The state string is the single source of
 * truth — no ambiguous booleans.
 */
export type ConnectionState =
  | "disconnected"
  | "selecting"
  | "preparing"
  | "starting_core"
  | "waiting_for_ready"
  | "connected"
  | "disconnecting"
  | "connection_failed";

interface ConnectionStore {
  snapshot: ConnectionSnapshot | null;
  backends: BackendView[];
  health: CoreHealthReport | null;
  busy: boolean;
  error: string | null;

  refresh: () => Promise<void>;
  refreshBackends: () => Promise<void>;
  ingestEvent: (snapshot: ConnectionSnapshot) => void;
  connect: (configID: string) => Promise<void>;
  disconnect: () => Promise<void>;
  reconnect: () => Promise<void>;
}

export const useConnectionStore = create<ConnectionStore>((set, get) => ({
  snapshot: null,
  backends: [],
  health: null,
  busy: false,
  error: null,

  refresh: async () => {
    try {
      const snapshot = await call(() => connectionService.ConnectionState());
      set({ snapshot, error: null });
    } catch (error) {
      set({
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  refreshBackends: async () => {
    try {
      const backends = await call(() => connectionService.RefreshBackends());
      set({ backends });
    } catch (error) {
      set({
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  ingestEvent: (snapshot) => set({ snapshot }),

  connect: async (configID) => {
    if (get().busy) return;
    set({ busy: true, error: null });

    try {
      const snapshot = await call(() => connectionService.Connect(configID));
      set({ snapshot, busy: false });
    } catch (error) {
      // The backend already transitioned to connection_failed; pull
      // the authoritative snapshot rather than synthesizing state.
      await get().refresh();
      set({
        busy: false,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  disconnect: async () => {
    if (get().busy) return;
    set({ busy: true, error: null });

    try {
      const snapshot = await call(() => connectionService.Disconnect());
      set({ snapshot, busy: false, health: null });
    } catch (error) {
      set({
        busy: false,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  reconnect: async () => {
    if (get().busy) return;
    set({ busy: true, error: null });

    try {
      const snapshot = await call(() => connectionService.Reconnect());
      set({ snapshot, busy: false });
    } catch (error) {
      await get().refresh();
      set({
        busy: false,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },
}));

/**
 * Subscribes to connection state broadcasts ("freeiran:connection")
 * and performs an initial fetch + backend discovery. Returns a
 * disposer.
 */
export function connectConnectionStore(): () => void {
  void useConnectionStore.getState().refresh();
  void useConnectionStore.getState().refreshBackends();

  const off = Events.On("freeiran:connection", (event: { data: ConnectionSnapshot }) => {
    useConnectionStore.getState().ingestEvent(event.data);
  });

  return () => {
    if (typeof off === "function") off();
  };
}
