/**
 * v0.9.10 collections state: persistent favorites, user-defined
 * groups and the evidence-based source reliability report.
 *
 * Everything here is a thin, bounded view over the EXISTING backend
 * services (CollectionService / SourceService.SourceReliability):
 * no duplicated ranking, no invented scores, no polling — loads are
 * explicit (mount / user action / evidence-change events).
 */
import { create } from "zustand";
import { call, collectionService, sourceService } from "../services";
import type { GroupsOverview, SourceReliabilityReport } from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js";
import { describeError, toast } from "./toastStore";

export interface BuiltinGroupView {
  id: string;
  count: number;
}

export interface UserGroupView {
  id: string;
  name: string;
  count: number;
  created_at?: number;
}

interface CollectionsState {
  /** Stable ids of favorite configurations (ordered by add time). */
  favorites: string[];
  favoritesLoaded: boolean;
  /** Built-in evidence groups with live counts. */
  builtinGroups: BuiltinGroupView[];
  /** User-defined groups. */
  userGroups: UserGroupView[];
  groupsLoaded: boolean;
  loading: boolean;
  lastError: string | null;

  /** Load favorites + groups overview (bounded, one call each). */
  load: () => Promise<void>;
  /** Toggle one favorite; returns the new state. */
  toggleFavorite: (configID: string) => Promise<boolean>;
  /** Create a user group. */
  createUserGroup: (name: string) => Promise<void>;
  /** Delete a user group. */
  deleteUserGroup: (groupID: string) => Promise<void>;
  /** Add a configuration to a user group. */
  addToGroup: (groupID: string, configID: string) => Promise<void>;
  /** Remove a configuration from a user group. */
  removeFromGroup: (groupID: string, configID: string) => Promise<void>;
  /** Local optimistic helpers used by list rows. */
  isFavorite: (configID: string) => boolean;
}

export const useCollectionsStore = create<CollectionsState>((set, get) => ({
  favorites: [],
  favoritesLoaded: false,
  builtinGroups: [],
  userGroups: [],
  groupsLoaded: false,
  loading: false,
  lastError: null,

  load: async () => {
    set({ loading: true });

    try {
      const [favorites, overview] = await Promise.all([
        call(() => collectionService.Favorites()),
        call(() => collectionService.GroupsOverview()),
      ]);

      const groupsOverview: GroupsOverview | null = overview ?? null;

      set({
        favorites: favorites ?? [],
        favoritesLoaded: true,
        builtinGroups: groupsOverview?.builtin ?? [],
        userGroups: groupsOverview?.user ?? [],
        groupsLoaded: true,
        loading: false,
        lastError: null,
      });
    } catch (error) {
      set({ loading: false, lastError: describeError(error) });
    }
  },

  toggleFavorite: async (configID) => {
    const nowFavorite = await call(() => collectionService.ToggleFavorite(configID));

    set((state) => ({
      favorites: nowFavorite
        ? [...state.favorites, configID]
        : state.favorites.filter((id) => id !== configID),
      // Keep the built-in count honest without a re-scan round trip:
      // the overview refreshes on the next explicit load.
      builtinGroups: state.builtinGroups.map((group) =>
        group.id === "favorites"
          ? { ...group, count: Math.max(0, group.count + (nowFavorite ? 1 : -1)) }
          : group,
      ),
    }));

    return nowFavorite;
  },

  createUserGroup: async (name) => {
    await call(() => collectionService.CreateUserGroup(name));
    await get().load();
  },

  deleteUserGroup: async (groupID) => {
    await call(() => collectionService.DeleteUserGroup(groupID));
    await get().load();
  },

  addToGroup: async (groupID, configID) => {
    await call(() => collectionService.AddToUserGroup(groupID, configID));
    await get().load();
  },

  removeFromGroup: async (groupID, configID) => {
    await call(() => collectionService.RemoveFromUserGroup(groupID, configID));
    await get().load();
  },

  isFavorite: (configID) => get().favorites.includes(configID),
}));

interface ReliabilityState {
  report: SourceReliabilityReport | null;
  loading: boolean;
  loaded: boolean;
  lastError: string | null;

  /** Load the evidence-based source reliability report (cached backend-side). */
  load: () => Promise<void>;
}

export const useReliabilityStore = create<ReliabilityState>((set) => ({
  report: null,
  loading: false,
  loaded: false,
  lastError: null,

  load: async () => {
    set({ loading: true });

    try {
      const report = await call(() => sourceService.SourceReliability());

      set({ report: report ?? null, loading: false, loaded: true, lastError: null });
    } catch (error) {
      set({ loading: false, loaded: true, lastError: describeError(error) });
    }
  },
}));

/** Human labels for the built-in group ids (shared by Configs + Quick Connect). */
export const BUILTIN_GROUP_LABELS: Record<string, string> = {
  all: "All",
  favorites: "Favorites",
  working: "Working",
  untested: "Untested",
  fast: "Fast",
  recently_tested: "Recently tested",
};

/** Friendly descriptions for the built-in groups (education, §5). */
export const BUILTIN_GROUP_HINTS: Record<string, string> = {
  all: "Every discovered configuration",
  favorites: "Configurations you saved for quick access",
  working: "Passed a connection test",
  untested: "Discovered but never tested",
  fast: "Working and measured at 400 ms or better",
  recently_tested: "Tested within the last 24 hours",
};

/** Surface a collections failure without crashing the page. */
export function notifyCollectionsError(action: string, error: unknown): void {
  toast("error", action, describeError(error));
}
