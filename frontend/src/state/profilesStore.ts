import { create } from "zustand";
import { call, profileService, type ProfileSpec, type ProfileView } from "../services";

/**
 * v0.9.11 Connection Profiles store (P2 §18): the named connection
 * preference sets (Home / Work / Travel / Privacy) surfaced on the
 * Quick Connect page. All state comes from the backend — the store
 * never fabricates availability, never stores credentials and never
 * duplicates the configuration store. Activation applies preferences
 * through the ONE backend settings path; the connect itself still
 * runs the existing verified engine flows.
 */

export type ProfileMode = "auto" | "configs" | "tor" | "psiphon";

interface ProfilesStore {
  profiles: ProfileView[];
  active: ProfileView | null;
  loaded: boolean;
  loading: boolean;
  error: string | null;

  /** Loads the profile list + active marker once per mount. */
  load: () => Promise<void>;
  /** Re-fetches the list (post-action refresh). */
  refresh: () => Promise<void>;
  /** Activates a profile (backend applies preferences; UI re-renders from the returned authoritative view). */
  setActive: (profileID: string) => Promise<void>;
  /** Creates a profile and returns the authoritative view. */
  create: (spec: ProfileSpec) => Promise<ProfileView>;
  /** Edits a profile. */
  update: (profileID: string, spec: ProfileSpec) => Promise<ProfileView>;
  /** Renames a profile. */
  rename: (profileID: string, name: string) => Promise<ProfileView>;
  /** Duplicates a profile. */
  duplicate: (profileID: string) => Promise<ProfileView>;
  /** Deletes a profile (configurations are never touched). */
  remove: (profileID: string) => Promise<void>;
  /** Marks a profile as the startup profile. */
  setDefault: (profileID: string) => Promise<void>;
}

export const useProfilesStore = create<ProfilesStore>((set, get) => ({
  profiles: [],
  active: null,
  loaded: false,
  loading: false,
  error: null,

  load: async () => {
    if (get().loading) return;

    set({ loading: true });

    try {
      const [profiles, activePair] = await Promise.all([
        call<ProfileView[]>(() => profileService.List()),
        call<[ProfileView, boolean]>(() => profileService.Active()),
      ]);

      set({
        profiles: profiles ?? [],
        active: activePair?.[1] ? activePair[0] : null,
        loaded: true,
        loading: false,
        error: null,
      });
    } catch (err) {
      // The page stays usable without profiles on failure.
      set({ loaded: true, loading: false, error: String(err) });
    }
  },

  refresh: async () => {
    try {
      const [profiles, activePair] = await Promise.all([
        call<ProfileView[]>(() => profileService.List()),
        call<[ProfileView, boolean]>(() => profileService.Active()),
      ]);

      set({
        profiles: profiles ?? [],
        active: activePair?.[1] ? activePair[0] : null,
        error: null,
      });
    } catch (err) {
      set({ error: String(err) });
    }
  },

  setActive: async (profileID) => {
    try {
      // Backend first: the returned view is the authoritative state and
      // the UI updates from it immediately (no optimistic local state).
      const applied = await call<ProfileView>(() => profileService.SetActive(profileID));

      await get().refresh();

      set({ active: applied, error: null });
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);

      set({ error: message });

      throw err;
    }
  },

  create: async (spec) => {
    const created = await call<ProfileView>(() => profileService.Create(spec));

    await get().refresh();

    return created;
  },

  update: async (profileID, spec) => {
    const updated = await call<ProfileView>(() => profileService.Update(profileID, spec));

    await get().refresh();

    return updated;
  },

  rename: async (profileID, name) => {
    const renamed = await call<ProfileView>(() => profileService.Rename(profileID, name));

    await get().refresh();

    return renamed;
  },

  duplicate: async (profileID) => {
    const copy = await call<ProfileView>(() => profileService.Duplicate(profileID));

    await get().refresh();

    return copy;
  },

  remove: async (profileID) => {
    await call<void>(() => profileService.Delete(profileID));

    await get().refresh();
  },

  setDefault: async (profileID) => {
    await call<void>(() => profileService.SetDefault(profileID));

    await get().refresh();
  },
}));

/** Label lookup shared by the profile editor UI. */
export const PROFILE_MODE_LABELS: Record<ProfileMode, string> = {
  auto: "Auto",
  configs: "Configurations",
  tor: "Tor",
  psiphon: "Psiphon",
};

export function normalizeProfileMode(mode: unknown): ProfileMode {
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
