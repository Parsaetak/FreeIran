import { create } from "zustand";
import { call, appService } from "../services";

/**
 * v0.12.2 Quick Connect route-mode store: the Quick Connect choice
 * (Auto / Configurations / Proxy Chains) — the surviving surface of
 * the removed provider-mode store. All state comes from the backend;
 * the store never fabricates availability.
 */

export type ConnectMode = "auto" | "configs" | "chains";

/**
 * Monotonic generation for the SetMode path — only the newest call
 * may write the store (out-of-order responses never settle the wrong
 * mode).
 */
let modeGeneration = 0;

interface ConnectModeStore {
  mode: ConnectMode;
  loaded: boolean;
  loading: boolean;
  error: string | null;

  /** Loads the persisted route mode once per mount. */
  load: () => Promise<void>;
  /** Persists a mode change. */
  setMode: (mode: ConnectMode) => Promise<void>;
}

export const useConnectModeStore = create<ConnectModeStore>((set, get) => ({
  mode: "auto",
  loaded: false,
  loading: false,
  error: null,

  load: async () => {
    if (get().loading) return;

    set({ loading: true });

    try {
      const mode = await call<string>(() => appService.ConnectMode());

      set({ mode: normalizeMode(mode), loaded: true, loading: false, error: null });
    } catch (err) {
      // The page stays usable with the default mode on failure.
      set({ loaded: true, loading: false, error: String(err) });
    }
  },

  setMode: async (mode) => {
    // Out-of-order SetMode responses must never settle the wrong
    // mode — only the newest call may write the store.
    const generation = ++modeGeneration;

    try {
      const persisted = await call<string>(() => appService.SetConnectMode(mode));

      if (generation !== modeGeneration) return;

      set({ mode: normalizeMode(persisted) });
    } catch (err) {
      // The caller may not handle the rejection: surface the failure
      // in the store so the UI can render it (never silent).
      if (generation !== modeGeneration) return;

      set({ error: String(err) });

      throw err;
    }
  },
}));

function normalizeMode(mode: unknown): ConnectMode {
  switch (mode) {
    case "configs":
    case "chains":
    case "auto":
      return mode;
    default:
      return "auto";
  }
}

/** Label lookup shared by the Quick Connect selector and scope rails. */
export const CONNECT_MODE_LABELS: Record<ConnectMode, string> = {
  auto: "Auto",
  configs: "Configurations",
  chains: "Proxy Chains",
};
