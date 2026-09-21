import { create } from "zustand";
import { call, providerService, type ProviderInfoView } from "../services";

/**
 * v0.9.8.1 provider store (§12): the Quick Connect provider choice
 * (Auto / Configurations / Tor / Psiphon) and the provider list the
 * Cores page renders. All state comes from the backend — the store
 * never fabricates availability.
 */

export type ProviderMode = "auto" | "configs" | "tor" | "psiphon";

/**
 * v0.9.12: monotonic generation for the SetMode path — only the
 * newest call may write the store (out-of-order responses never
 * settle the wrong mode).
 */
let modeGeneration = 0;

interface ProviderStore {
  mode: ProviderMode;
  providers: ProviderInfoView[];
  loaded: boolean;
  loading: boolean;
  error: string | null;

  /** Loads the persisted mode + the provider list once per mount. */
  load: () => Promise<void>;
  /** Re-fetches just the provider list (post-action refresh). */
  refresh: () => Promise<void>;
  /** Persists a mode change. */
  setMode: (mode: ProviderMode) => Promise<void>;
}

export const useProviderStore = create<ProviderStore>((set, get) => ({
  mode: "auto",
  providers: [],
  loaded: false,
  loading: false,
  error: null,

  load: async () => {
    if (get().loading) return;

    set({ loading: true });

    try {
      const [mode, providers] = await Promise.all([
        call<string>(() => providerService.Mode()),
        call<ProviderInfoView[]>(() => providerService.List()),
      ]);

      set({
        mode: normalizeMode(mode),
        providers: providers ?? [],
        loaded: true,
        loading: false,
        error: null,
      });
    } catch (err) {
      // The page stays usable with the default mode on failure.
      set({ loaded: true, loading: false, error: String(err) });
    }
  },

  refresh: async () => {
    try {
      const providers = await call<ProviderInfoView[]>(() => providerService.List());

      set({ providers: providers ?? [], error: null });
    } catch (err) {
      set({ error: String(err) });
    }
  },

  setMode: async (mode) => {
    // v0.9.12: out-of-order SetMode responses must never settle the
    // wrong mode — only the newest call may write the store.
    const generation = ++modeGeneration;

    try {
      const persisted = await call<string>(() => providerService.SetMode(mode));

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

function normalizeMode(mode: unknown): ProviderMode {
  switch (mode) {
    case "configs":
    case "tor":
    case "psiphon":
    case "auto":
      return mode;
    default:
      return "auto";
  }
}

/** Label lookup shared by the Quick Connect selector and Cores page. */
export const PROVIDER_MODE_LABELS: Record<ProviderMode, string> = {
  auto: "Auto",
  configs: "Configurations",
  tor: "Tor",
  psiphon: "Psiphon",
};
