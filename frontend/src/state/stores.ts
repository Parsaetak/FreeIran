import { create } from "zustand";
import {
  dataService,
  sourceService,
  call,
  type Config,
  type SourceView,
  type IngestionStats,
} from "../services";

interface SourcesStore {
  sources: SourceView[];
  loading: boolean;
  refreshing: boolean;
  lastError: string | null;

  /** Result of the most recent explicit "refresh now" (null = none). */
  lastIngestion: IngestionStats | null;
  lastRefreshAt: number;

  load: () => Promise<void>;
  setEnabled: (id: string, enabled: boolean) => Promise<void>;
  add: (id: string, name: string, url: string) => Promise<void>;
  remove: (id: string) => Promise<void>;
  refresh: () => Promise<IngestionStats | null>;
}

export const useSourcesStore = create<SourcesStore>((set, get) => ({
  sources: [],
  loading: false,
  refreshing: false,
  lastError: null,
  lastIngestion: null,
  lastRefreshAt: 0,

  load: async () => {
    set({ loading: true });

    try {
      const sources = await call(() => sourceService.List());

      set({ sources, loading: false, lastError: null });
    } catch (error) {
      set({
        loading: false,
        lastError: error instanceof Error ? error.message : String(error),
      });
    }
  },

  setEnabled: async (id, enabled) => {
    await call(() => sourceService.SetEnabled(id, enabled));

    await get().load();
  },

  add: async (id, name, url) => {
    await call(() => sourceService.Add(id, name, url));

    await get().load();
  },

  remove: async (id) => {
    await call(() => sourceService.Remove(id));

    await get().load();
  },

  refresh: async () => {
    set({ refreshing: true });

    try {
      const stats = await call(() => sourceService.RefreshNow());

      await get().load();

      set({ refreshing: false, lastIngestion: stats ?? null, lastRefreshAt: Date.now() });

      return stats ?? null;
    } catch (error) {
      set({ refreshing: false });
      throw error;
    }
  },
}));

const PAGE_SIZE = 200;

interface ConfigsStore {
  items: Config[];
  total: number;
  loading: boolean;
  searching: boolean;
  searchQuery: string;
  lastError: string | null;

  /** Pagination cursor for the unfiltered list (infinite scroll). */
  offset: number;
  hasMore: boolean;

  loadPage: (offset: number) => Promise<void>;
  loadMore: () => Promise<void>;
  setSearchQuery: (query: string) => void;
  runSearch: () => Promise<void>;
}

export const useConfigsStore = create<ConfigsStore>((set, get) => ({
  items: [],
  total: 0,
  loading: false,
  searching: false,
  searchQuery: "",
  lastError: null,
  offset: 0,
  hasMore: false,

  loadPage: async (offset) => {
    set({ loading: true });

    try {
      const page = await call(() => dataService.ListConfigs(offset, PAGE_SIZE));

      if (page == null) {
        set({ loading: false });

        return;
      }

      set({
        items: page.items,
        total: page.total,
        offset,
        hasMore: page.has_more,
        loading: false,
        lastError: null,
      });
    } catch (error) {
      set({
        loading: false,
        lastError: error instanceof Error ? error.message : String(error),
      });
    }
  },

  /** Appends the next page when the unfiltered list scrolls near the end. */
  loadMore: async () => {
    const { loading, hasMore, searchQuery, offset } = get();

    if (loading || !hasMore || searchQuery.trim() !== "") return;

    set({ loading: true });

    try {
      const page = await call(() => dataService.ListConfigs(offset + PAGE_SIZE, PAGE_SIZE));

      if (page == null) {
        set({ loading: false });

        return;
      }

      set((state) => ({
        items: [...state.items, ...page.items],
        total: page.total,
        offset: offset + PAGE_SIZE,
        hasMore: page.has_more,
        loading: false,
        lastError: null,
      }));
    } catch (error) {
      set({
        loading: false,
        lastError: error instanceof Error ? error.message : String(error),
      });
    }
  },

  setSearchQuery: (query) => set({ searchQuery: query, offset: 0, hasMore: false }),

  runSearch: async () => {
    const query = get().searchQuery.trim();

    set({ searching: true });

    try {
      if (query === "") {
        await get().loadPage(0);
        set({ searching: false });
      } else {
        const results = await call(() => dataService.SearchConfigs(query, 200));

        set({
          items: results,
          searching: false,
          offset: 0,
          hasMore: false,
          lastError: null,
        });
      }
    } catch (error) {
      set({
        searching: false,
        lastError: error instanceof Error ? error.message : String(error),
      });
    }
  },
}));

/** Debounces search input: typing stays smooth on huge datasets. */
export function makeSearchRunner(delayMs = 250): () => void {
  let timer: ReturnType<typeof setTimeout> | null = null;

  return () => {
    if (timer !== null) clearTimeout(timer);

    timer = setTimeout(() => {
      void useConfigsStore.getState().runSearch();
    }, delayMs);
  };
}
