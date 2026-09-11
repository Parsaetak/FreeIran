import { create } from "zustand";
import { call, settingsService, type Settings } from "../services";
import { describeError } from "./toastStore";

/**
 * Authoritative view of the persisted user settings. The Settings
 * page keeps its own editable draft; this store holds the saved
 * truth (plus a transient reduced-motion override so the toggle
 * applies to the running UI before an explicit save).
 */

interface SettingsStore {
  settings: Settings | null;
  loading: boolean;
  saving: boolean;
  lastError: string | null;

  /** Transient preview of reduced motion (null = follow saved settings). */
  motionOverride: boolean | null;

  load: () => Promise<void>;
  save: (next: Settings) => Promise<Settings | null>;
  setMotionOverride: (value: boolean | null) => void;
}

export const useSettingsStore = create<SettingsStore>((set) => ({
  settings: null,
  loading: false,
  saving: false,
  lastError: null,
  motionOverride: null,

  load: async () => {
    set({ loading: true });

    try {
      const settings = await call(() => settingsService.Get());

      set({ settings, loading: false, lastError: null });
    } catch (error) {
      set({ loading: false, lastError: describeError(error, "Settings unavailable") });
    }
  },

  save: async (next) => {
    set({ saving: true, lastError: null });

    try {
      const saved = await call(() => settingsService.Save(next));

      set({ settings: saved, saving: false, motionOverride: null });

      return saved;
    } catch (error) {
      set({ saving: false, lastError: describeError(error, "Could not save settings") });

      return null;
    }
  },

  setMotionOverride: (value) => set({ motionOverride: value }),
}));

/** Effective reduced-motion preference for the running UI. */
export function effectiveReducedMotion(state: SettingsStore): boolean {
  return state.motionOverride ?? state.settings?.reduced_motion ?? false;
}
