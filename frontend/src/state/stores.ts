import { create } from "zustand";
import {
  dataService,
  sourceService,
  call,
  type Config,
  type SourceView,
} from "../services";

interface SourcesStore {
  sources: SourceView[];
  loading: boolean;
  refreshing: boolean;
  lastError: string | null;

  load: () => Promise<void>;
  setEnabled: (id: string, enabled: boolean) => Promise<void>;
  add: (id: string, name: string, url: string) => Promise<void>;
  remove: (id: string) => Promise<void>;
  refresh: () => Promise<void>;
}

export const useSourcesStore = create<SourcesStore>((set, get) => ({
  sources: [],
  loading: false,
  refreshing: false,
  lastError: null,

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
      await call(() => sourceService.RefreshNow());

      await get().load();
    } finally {
      set({ refreshing: false });
    }
  },
}));

const PAGE_SIZE = 200;

interface ConfigsStore {
  items: Config[];
  total: number;
  loading: boolean;
  searchQuery: string;
  searching: boolean;
  lastError: string | null;

  loadPage: (offset: number) => Promise<void>;
  setSearchQuery: (query: string) => void;
  runSearch: () => Promise<void>;
}

export const useConfigsStore = create<ConfigsStore>((set, get) => ({
  items: [],
  total: 0,
  loading: false,
  searchQuery: "",
  searching: false,
  lastError: null,

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

  setSearchQuery: (query) => set({ searchQuery: query }),

  runSearch: async () => {
    const query = get().searchQuery.trim();

    set({ searching: true });

    try {
      if (query === "") {
        await get().loadPage(0);
      } else {
        const results = await call(() => dataService.SearchConfigs(query, 200));

        set({ items: results, searching: false, lastError: null });
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
