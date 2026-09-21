import { create } from "zustand";
import { Events } from "@wailsio/runtime";
import {
  connectionService,
  call,
  type ConnectionSnapshot,
  type BackendView,
  type CoreHealthReport,
  type ConnectBestResultView,
} from "../services";

/**
 * Connection store: mirrors the backend connection state machine
 * (engine/connection) through the "freeiran:connection" event plus
 * explicit service calls. The state string is the single source of
 * truth — no ambiguous booleans.
 *
 * v0.9.8.4 — connection-state integrity: the backend machine exposes
 * the full v0.9.8.3 lifecycle, including the verification boundary:
 *
 *      connected          route established — NOT verified Internet
 *      verifying          end-to-end verification running
 *      connected_verified final success — real traffic crossed the tunnel
 *
 * v0.9.8.4 — stale-operation protection: every asynchronous store
 * operation captures a generation token when it STARTS and applies
 * its result ONLY while that generation is still current. A newer
 * operation (or an authoritative event, or an explicit invalidation)
 * bumps the generation, so a late-resolving connect/connectBest/
 * refresh can never overwrite newer state, a stale error can never
 * replace the current one, and disconnect/reconnect/unmount
 * invalidate everything still in flight. The busy flag follows the
 * same ownership rule — v0.9.12: an operation invalidated by an
 * authoritative EVENT also loses busy ownership (the event releases
 * it), because no operation can ever apply its own completion after
 * the machine already broadcast the newer state.
 */

export type ConnectionState =
  | "disconnected"
  | "selecting"
  | "preparing"
  | "starting_core"
  | "waiting_for_ready"
  | "connected"
  | "verifying"
  | "connected_verified"
  | "disconnecting"
  | "connection_failed";

/**
 * Monotonic operation generation (module scope, shared by every
 * store instance — there is exactly one connection store). Bumped
 * by every state-mutating operation start, by authoritative event
 * ingestion and by explicit invalidation.
 */
let operationGeneration = 0;

function nextGeneration(): number {
  operationGeneration += 1;

  return operationGeneration;
}

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
  connectBest: (exclude?: string[]) => Promise<ConnectBestResultView | null>;
  disconnect: () => Promise<void>;
  reconnect: () => Promise<void>;
  /**
   * v0.9.8.4: marks every in-flight operation stale so a late
   * resolution can never mutate the store (called on teardown and
   * by tests between cases — deterministic without arbitrary sleeps).
   */
  invalidatePendingOperations: () => void;
}

export const useConnectionStore = create<ConnectionStore>((set, get) => ({
  snapshot: null,
  backends: [],
  health: null,
  busy: false,
  error: null,

  refresh: async () => {
    const generation = nextGeneration();

    try {
      const snapshot = await call(() => connectionService.ConnectionState());

      // Stale read: a newer operation or event already changed the
      // state — applying this result would regress the machine.
      if (generation !== operationGeneration) return;

      set({ snapshot: snapshot ?? null, error: null });
    } catch (error) {
      if (generation !== operationGeneration) return;

      set({
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  refreshBackends: async () => {
    const generation = nextGeneration();

    try {
      const backends = await call(() => connectionService.RefreshBackends());

      if (generation !== operationGeneration) return;

      set({ backends: backends ?? [] });
    } catch (error) {
      if (generation !== operationGeneration) return;

      set({
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  /**
   * Authoritative state-machine broadcast: always applied, and it
   * invalidates every outstanding operation result — the event is
   * newer evidence than any promise that has not resolved yet.
   *
   * v0.9.12 — busy ownership transfers to the event: an in-flight
   * operation's own completion can no longer apply (its generation
   * is stale the moment this event lands), so it must not keep
   * owning the busy flag. The backend Connect/Reconnect calls are
   * long-running and the machine publishes real transitions WHILE
   * they run — without the release here, every such operation
   * resolved stale and left busy=true forever, disabling Connect,
   * Disconnect and Reconnect until restart (the v0.9.11 UI defect).
   * The dropped result loses nothing: the authoritative snapshot
   * carries the machine's own state and LastError.
   */
  ingestEvent: (snapshot) => {
    operationGeneration = nextGeneration();

    set({ snapshot, busy: false });
  },

  connect: async (configID) => {
    if (get().busy) return;

    const generation = nextGeneration();

    set({ busy: true, error: null });

    try {
      const snapshot = await call(() => connectionService.Connect(configID));

      // A newer operation/event owns the store now; this late result
      // (and its busy ownership) is dropped entirely.
      if (generation !== operationGeneration) return;

      set({ snapshot, busy: false });
    } catch (error) {
      // The backend already transitioned to connection_failed; pull
      // the authoritative snapshot rather than synthesizing state.
      // This read deliberately does NOT start a new generation: the
      // failed operation stays current (unless a NEWER operation
      // started during the await), so its error is still its own.
      let failed: ConnectionSnapshot | null = null;

      try {
        failed = (await connectionService.ConnectionState()) ?? null;
      } catch {
        failed = null; // keep the local state; the error still applies
      }

      if (generation !== operationGeneration) return;

      set({
        snapshot: failed ?? get().snapshot,
        busy: false,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  connectBest: async (exclude = []) => {
    if (get().busy) return null;

    const generation = nextGeneration();

    set({ busy: true, error: null });

    try {
      const result = await call(() => connectionService.ConnectBest(exclude));

      if (generation !== operationGeneration) return null;

      set({ snapshot: result.snapshot, busy: false });

      return result;
    } catch (error) {
      let failed: ConnectionSnapshot | null = null;

      try {
        failed = (await connectionService.ConnectionState()) ?? null;
      } catch {
        failed = null;
      }

      if (generation !== operationGeneration) return null;

      set({
        snapshot: failed ?? get().snapshot,
        busy: false,
        error: error instanceof Error ? error.message : String(error),
      });

      return null;
    }
  },

  disconnect: async () => {
    if (get().busy) return;

    // Starting the disconnect invalidates every previous operation:
    // their results can no longer overwrite the teardown.
    const generation = nextGeneration();

    set({ busy: true, error: null });

    try {
      const snapshot = await call(() => connectionService.Disconnect());

      if (generation !== operationGeneration) return;

      set({ snapshot, busy: false, health: null });
    } catch (error) {
      if (generation !== operationGeneration) return;

      set({
        busy: false,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  reconnect: async () => {
    if (get().busy) return;

    const generation = nextGeneration();

    set({ busy: true, error: null });

    try {
      const snapshot = await call(() => connectionService.Reconnect());

      if (generation !== operationGeneration) return;

      set({ snapshot, busy: false });
    } catch (error) {
      let failed: ConnectionSnapshot | null = null;

      try {
        failed = (await connectionService.ConnectionState()) ?? null;
      } catch {
        failed = null;
      }

      if (generation !== operationGeneration) return;

      set({
        snapshot: failed ?? get().snapshot,
        busy: false,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  invalidatePendingOperations: () => {
    operationGeneration = nextGeneration();
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

    // Teardown must not leave dangerous pending mutations armed: any
    // operation still in flight becomes stale the moment the store
    // wiring goes away.
    useConnectionStore.getState().invalidatePendingOperations();
  };
}
