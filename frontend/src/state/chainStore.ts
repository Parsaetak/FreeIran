import { create } from "zustand";
import { call, proxyChainService } from "../services";
import type { ChainCheckResult, ProxyChainDetails, ProxyChainView } from "../services";

/**
 * v0.12.2 proxy-chain store: the user-built chains over the EXISTING
 * collections authority. All state comes from the backend — the store
 * never fabricates availability or invents chains. Chains connect
 * through the SAME verified connection state machine as plain
 * configurations (ConnectionService.ConnectChain).
 */

interface ChainStore {
  chains: ProxyChainView[];
  loaded: boolean;
  loading: boolean;
  error: string | null;

  /** Selected chain (Quick Connect / chain scope). */
  selected: string | null;

  /** Loads the chain list once per mount. */
  load: () => Promise<void>;
  /** Persists a selection (UI state only). */
  select: (chainID: string | null) => void;
  /** Creates a chain and refreshes the list. */
  create: (name: string, configIDs: string[]) => Promise<ProxyChainView>;
  /** Renames a chain. */
  rename: (chainID: string, name: string) => Promise<void>;
  /** Deletes a chain. */
  remove: (chainID: string) => Promise<void>;
  /** Ordered hop mutation (editor). */
  addHop: (chainID: string, configID: string, position: number) => Promise<void>;
  removeHop: (chainID: string, configID: string) => Promise<void>;
  reorderHop: (chainID: string, configID: string, position: number) => Promise<void>;
}

export const useChainStore = create<ChainStore>((set, get) => ({
  chains: [],
  loaded: false,
  loading: false,
  error: null,
  selected: null,

  load: async () => {
    if (get().loading) return;

    set({ loading: true });

    try {
      const chains = await call<ProxyChainView[]>(() =>
        proxyChainService.ListProxyChains(),
      );

      set({
        chains: chains ?? [],
        loaded: true,
        loading: false,
        error: null,
      });
    } catch (err) {
      set({ loaded: true, loading: false, error: String(err) });
    }
  },

  select: (chainID) => set({ selected: chainID }),

  create: async (name, configIDs) => {
    const created = await call<ProxyChainView>(() =>
      proxyChainService.CreateProxyChain(name, configIDs),
    );

    await get().load();

    return created;
  },

  rename: async (chainID, name) => {
    await call<void>(() => proxyChainService.RenameProxyChain(chainID, name));

    await get().load();
  },

  remove: async (chainID) => {
    await call<void>(() => proxyChainService.DeleteProxyChain(chainID));

    if (get().selected === chainID) {
      set({ selected: null });
    }

    await get().load();
  },

  addHop: async (chainID, configID, position) => {
    await call<void>(() => proxyChainService.AddHop(chainID, configID, position));

    await get().load();
  },

  removeHop: async (chainID, configID) => {
    await call<void>(() => proxyChainService.RemoveHop(chainID, configID));

    await get().load();
  },

  reorderHop: async (chainID, configID, position) => {
    await call<void>(() => proxyChainService.ReorderHop(chainID, configID, position));

    await get().load();
  },
}));

/** Loads one chain's full editor projection (null when the chain no
 * longer exists — the binding is truthful about the absence). */
export async function loadChainDetails(
  chainID: string,
): Promise<ProxyChainDetails | null> {
  return call<ProxyChainDetails | null>(() => proxyChainService.ProxyChainDetails(chainID));
}

/** Validates a candidate hop list (editor preview). */
export async function validateChainHops(
  configIDs: string[],
): Promise<void> {
  await call<void>(() => proxyChainService.ValidateProxyChain(configIDs));
}

/** Runs the honest chain check (per-hop evidence + fresh e2e); null
 * when the chain vanished before the check ran. */
export async function checkChain(
  chainID: string,
): Promise<ChainCheckResult | null> {
  return call<ChainCheckResult | null>(() => proxyChainService.CheckChain(chainID));
}
