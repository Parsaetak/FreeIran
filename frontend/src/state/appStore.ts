import { create } from "zustand";
import { Events } from "@wailsio/runtime";
import { appService, call, type AppState } from "../services";
import { useQuickConnectStore } from "./quickConnectStore";

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

/**
 * Content equality for the store's embedded sub-objects. State
 * broadcasts re-deserialize an identical payload every interval; if
 * the new sub-object replaced the old reference unconditionally,
 * effects keyed on `backend?.last_ingestion` re-fired per broadcast
 * (Diagnostics stacked a full diagnostics reload per event). Same
 * construction path → stable key order → stringify compare is exact.
 */
function sameJSON(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a == null || b == null) return false;

  try {
    return JSON.stringify(a) === JSON.stringify(b);
  } catch {
    return false;
  }
}

/**
 * v0.9.12: monotonic poll generation — authoritative app-state events
 * and newer polls invalidate in-flight ones (a stale poll response
 * must never regress the displayed backend state).
 */
let refreshGeneration = 0;

export const useAppStore = create<AppStore>((set, get) => ({
  status: "loading",
  backend: null,
  lastError: null,
  connected: false,
  bootPhase: null,
  bootTimings: null,

  refresh: async () => {
    // v0.9.12: stale-read guard — a slow poll that fails after the
    // backend already recovered (or after a newer authoritative
    // event) must not flip the store to backend_unavailable.
    const generation = ++refreshGeneration;

    try {
      const state = await call(() => appService.State());

      if (generation !== refreshGeneration) return;

      set({
        backend: state,
        connected: true,
        status: deriveStatus(state, true),
        lastError: null,
        ...readBootFields(state),
      });
    } catch (error) {
      if (generation !== refreshGeneration) return;

      set({
        connected: false,
        status: "backend_unavailable",
        lastError: error instanceof Error ? error.message : String(error),
      });
    }
  },

  ingestEvent: (state: AppState) => {
    // v0.9.12: an authoritative broadcast invalidates every in-flight
    // poll (its stale response must never overwrite newer evidence).
    refreshGeneration++;

    // Preserve previous sub-object references when content is
    // unchanged (see sameJSON): reference-keyed consumers only re-run
    // when the underlying evidence actually changed.
    const prev = get().backend;

    // v0.9.8.7 — meaningful-invalidation signal: an ingestion cycle
    // FINISHING changed the configuration pool. That is exactly the
    // event the Quick Connect candidate list waits for (one bounded
    // refresh, no polling). Rising edge (ingestion starting) needs no
    // refresh — the pool only changes when the cycle lands.
    if (prev?.ingestion_running && !state.ingestion_running) {
      useQuickConnectStore.getState().invalidate();
    }

    if (prev) {
      if (sameJSON(prev.last_ingestion, state.last_ingestion)) {
        state.last_ingestion = prev.last_ingestion;
      }

      if (sameJSON(prev.storage, state.storage)) {
        state.storage = prev.storage;
      }
    }

    set({
      backend: state,
      connected: true,
      status: deriveStatus(state, true),
      lastError: null,
      ...readBootFields(state),
    });
  },
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
