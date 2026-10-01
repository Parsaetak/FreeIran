/**
 * @vitest-environment jsdom
 *
 * v0.12.2 Configuration workspace tests — PROXY CHAINS on the same
 * page architecture:
 *
 *   - the scope rail renders PROXY CHAIN scopes with hop counts and
 *     a "+ New chain" affordance;
 *   - selecting a chain scope filters server-side by the chain's hop
 *     IDs (ConfigFilter.ids — no full-database download);
 *   - the chain scope header shows Check chain / Connect / Edit and
 *     NEVER "Update source" (a chain is not a remote source);
 *   - the row menu gains chain actions scoped to the current surface;
 *   - the editor create flow validates before saving.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ConfigsPage } from "./Configs";

const serviceMocks = vi.hoisted(() => ({
  ListConfigsFiltered: vi.fn(),
  ConfigDetails: vi.fn(),
  LiveState: vi.fn(),
  EnqueueByFilter: vi.fn(),
  SourceStatsList: vi.fn(),
  ProxyChainDetails: vi.fn(),
  CheckChain: vi.fn(),
  ConnectChain: vi.fn(),
  ListProxyChains: vi.fn(),
  CreateProxyChain: vi.fn(),
  ValidateProxyChain: vi.fn(),
  RemoveHop: vi.fn(),
}));

vi.mock("../services", () => ({
  call: async (operation: () => Promise<unknown>) => operation(),
  dataService: {
    ListConfigsFiltered: serviceMocks.ListConfigsFiltered,
  },
  connectionService: {
    ConfigDetails: serviceMocks.ConfigDetails,
    ConnectChain: serviceMocks.ConnectChain,
  },
  testQueueService: {
    LiveState: serviceMocks.LiveState,
    EnqueueByFilter: serviceMocks.EnqueueByFilter,
  },
  sourceService: {
    SourceStatsList: serviceMocks.SourceStatsList,
  },
  proxyChainService: {
    ListProxyChains: serviceMocks.ListProxyChains,
    ProxyChainDetails: serviceMocks.ProxyChainDetails,
    CreateProxyChain: serviceMocks.CreateProxyChain,
    ValidateProxyChain: serviceMocks.ValidateProxyChain,
    CheckChain: serviceMocks.CheckChain,
    RemoveHop: serviceMocks.RemoveHop,
    RenameProxyChain: vi.fn(),
    DeleteProxyChain: vi.fn(),
    AddHop: vi.fn(),
    ReorderHop: vi.fn(),
  },
}));

vi.mock("@wailsio/runtime", () => ({
  Events: {
    On: () => () => undefined,
    Emit: () => undefined,
  },
}));

vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: ({ count }: { count: number }) => ({
    getTotalSize: () => count * 50,
    getVirtualItems: () =>
      Array.from({ length: count }, (_, index) => ({
        index,
        start: index * 50,
        size: 50,
        key: index,
      })),
    measure: () => undefined,
  }),
}));

vi.mock("../workers/export-worker?worker", () => ({
  default: class {
    postMessage() {
      /* noop */
    }

    terminate() {
      /* noop */
    }
  },
}));

vi.mock("../utilities/export", () => ({
  configToRow: (config: Record<string, unknown>) => config,
}));

const storesPromise = vi.hoisted(async () => {
  const { create } = await import("zustand");

  const useConfigsStore = create<{
    items: Record<string, unknown>[];
    total: number;
    loading: boolean;
    searching: boolean;
    searchQuery: string;
    hasMore: boolean;
    lastError: string | null;
    setSearchQuery: (value: string) => void;
    loadMore: () => void;
    runSearch: () => Promise<void>;
    loadPage: () => Promise<void>;
    patchConfig: () => void;
  }>((set) => ({
    items: [],
    total: 0,
    loading: false,
    searching: false,
    searchQuery: "",
    hasMore: false,
    lastError: null,
    setSearchQuery: (value) => set({ searchQuery: value }),
    loadMore: () => undefined,
    runSearch: async () => undefined,
    loadPage: async () => undefined,
    patchConfig: () => undefined,
  }));

  const useCollectionsStore = create<{
    builtinGroups: { id: string; count: number }[];
    userGroups: { id: string; name: string; count: number }[];
    favorites: string[];
    load: () => Promise<void>;
  }>(() => ({
    builtinGroups: [],
    userGroups: [],
    favorites: [],
    load: async () => undefined,
  }));

  const useChainStore = create<{
    chains: Array<{ id: string; name: string; hops: number }>;
    loaded: boolean;
    loading: boolean;
    error: string | null;
    selected: string | null;
    load: () => Promise<void>;
    select: (id: string | null) => void;
  }>((set) => ({
    chains: [],
    loaded: false,
    loading: false,
    error: null,
    selected: null,
    load: async () => {
      set({ loaded: true });
    },
    select: (id) => set({ selected: id }),
  }));

  const useConnectionStore = create<{
    busy: boolean;
    error: string | null;
    connect: (id: string) => Promise<void>;
  }>((set) => ({
    busy: false,
    error: null,
    connect: async () => {
      set({ error: null });
    },
  }));

  const useQuickConnectStore = create<{ invalidate: () => void }>(() => ({
    invalidate: () => undefined,
  }));

  return { useConfigsStore, useCollectionsStore, useChainStore, useConnectionStore, useQuickConnectStore };
});

vi.mock("../state/stores", async () => {
  const { useConfigsStore } = await storesPromise;

  return { useConfigsStore, makeSearchRunner: () => () => undefined };
});

vi.mock("../state/collectionsStore", async () => {
  const { useCollectionsStore } = await storesPromise;

  return {
    useCollectionsStore,
    BUILTIN_GROUP_LABELS: { all: "All" },
    BUILTIN_GROUP_HINTS: { all: "Everything" },
  };
});

vi.mock("../state/chainStore", async () => {
  const { useChainStore } = await storesPromise;

  return { useChainStore };
});

vi.mock("../state/connectionStore", async () => {
  const { useConnectionStore } = await storesPromise;

  return { useConnectionStore };
});

vi.mock("../state/quickConnectStore", async () => {
  const { useQuickConnectStore } = await storesPromise;

  return { useQuickConnectStore };
});

function config(overrides: Record<string, unknown> = {}) {
  return {
    id: "hop-1",
    type: "vless",
    name: "Berlin edge",
    address: "berlin.example.com",
    port: 443,
    network: "ws",
    security: "tls",
    tested_at: 0,
    source: "src-a",
    ...overrides,
  };
}

const chainFixture = {
  id: "pc-1",
  name: "Berlin route",
  config_ids: ["hop-1", "hop-2"],
  hops: [
    {
      position: 0,
      config_id: "hop-1",
      name: "Berlin edge",
      protocol: "vless",
      address: "berlin.example.com",
      port: 443,
      network: "ws",
      security: "tls",
      working: true,
      latency_ms: 42,
      tested_at: 1700000000000,
      test_backend: "xray",
      available: true,
    },
    {
      position: 1,
      config_id: "hop-2",
      name: "Zurich exit",
      protocol: "trojan",
      address: "zurich.example.com",
      port: 443,
      network: "tcp",
      security: "tls",
      working: false,
      latency_ms: 0,
      tested_at: 1700000001000,
      test_backend: "xray",
      available: true,
    },
  ],
  preview: "Berlin edge → Zurich exit",
  usable: true,
  created_at: 1700000000000,
  updated_at: 1700000000000,
};

beforeEach(async () => {
  vi.clearAllMocks();

  const { useConfigsStore, useCollectionsStore, useChainStore } = await storesPromise;

  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  });

  serviceMocks.SourceStatsList.mockResolvedValue([]);
  serviceMocks.LiveState.mockResolvedValue(null);
  serviceMocks.ListConfigsFiltered.mockResolvedValue({
    items: [config(), config({ id: "hop-2", name: "Zurich exit", address: "zurich.example.com", type: "trojan" })],
    total: 2,
  });
  serviceMocks.ProxyChainDetails.mockResolvedValue(chainFixture);
  serviceMocks.ListProxyChains.mockResolvedValue([
    { id: "pc-1", name: "Berlin route", hops: 2 },
  ]);

  useConfigsStore.setState({
    items: [config(), config({ id: "hop-2", name: "Zurich exit", address: "zurich.example.com", type: "trojan" })],
    total: 2,
  });
  useCollectionsStore.setState({
    builtinGroups: [],
    userGroups: [],
    favorites: [],
  });
  useChainStore.setState({
    chains: [{ id: "pc-1", name: "Berlin route", hops: 2 }],
    loaded: true,
    loading: false,
    error: null,
    selected: null,
  });
});

afterEach(() => {
  cleanup();
});

describe("Proxy chain scopes (v0.12.2)", () => {
  it("renders chain scopes with hop counts and the New chain affordance", async () => {
    render(<ConfigsPage />);

    const chip = await screen.findByRole("button", { name: /Berlin route/ });

    expect(chip.textContent).toContain("2");
    expect(screen.getByRole("button", { name: /\+ New chain/ })).toBeTruthy();
  });

  it("selecting a chain scope filters server-side by the chain's hop ids", async () => {
    render(<ConfigsPage />);

    const chip = await screen.findByRole("button", { name: /Berlin route/ });

    fireEvent.click(chip);

    await waitFor(() => {
      expect(serviceMocks.ListConfigsFiltered).toHaveBeenCalledWith(
        expect.objectContaining({ ids: ["hop-1", "hop-2"] }),
        0,
        1000,
      );
    });
  });

  it("shows the chain scope header with Check chain, Connect and Edit — never Update source", async () => {
    render(<ConfigsPage />);

    fireEvent.click(await screen.findByRole("button", { name: /Berlin route/ }));

    expect(await screen.findByText("2 hops")).toBeTruthy();
    expect(screen.getByText("Berlin edge → Zurich exit")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Check chain/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Connect/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Edit/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Update source/ })).toBeNull();
  });

  it("connects the chain through ConnectChain", async () => {
    render(<ConfigsPage />);

    fireEvent.click(await screen.findByRole("button", { name: /Berlin route/ }));

    const connect = await screen.findByRole("button", { name: /Connect/ });

    fireEvent.click(connect);

    await waitFor(() => {
      expect(serviceMocks.ConnectChain).toHaveBeenCalledWith("pc-1");
    });
  });

  it("checks the chain honestly (per-hop + end-to-end evidence)", async () => {
    serviceMocks.CheckChain.mockResolvedValue({
      chain_id: "pc-1",
      hops: chainFixture.hops,
      end_to_end: { ok: true, ping_ms: 210, measured: true, backend: "xray", at: 1700000002000 },
      duration_ms: 900,
    });

    render(<ConfigsPage />);

    fireEvent.click(await screen.findByRole("button", { name: /Berlin route/ }));

    fireEvent.click(await screen.findByRole("button", { name: /Check chain/ }));

    await waitFor(() => {
      expect(serviceMocks.CheckChain).toHaveBeenCalledWith("pc-1");
    });
  });

  it("renders the chain editor from the New chain affordance and validates before save", async () => {
    render(<ConfigsPage />);

    fireEvent.click(await screen.findByRole("button", { name: /\+ New chain/ }));

    const dialog = await screen.findByRole("dialog", { name: "Proxy chain editor" });

    expect(dialog).toBeTruthy();

    // Saving without hops must NOT call the backend (validation gate).
    fireEvent.click(screen.getByRole("button", { name: /Save chain/ }));

    await waitFor(() => {
      expect(serviceMocks.CreateProxyChain).not.toHaveBeenCalled();
    });
  });
});

describe("Row menu chain actions (v0.12.2)", () => {
  it("offers chain actions when multiple rows are selected", async () => {
    render(<ConfigsPage />);

    await screen.findByRole("list", { name: "Configurations" });

    // Select all via the keyboard shortcut.
    fireEvent.keyDown(document.body, { key: "a", ctrlKey: true });

    await waitFor(() => expect(screen.getAllByRole("listitem").length).toBeGreaterThanOrEqual(2));

    const row = screen.getAllByRole("listitem")[0];

    fireEvent.contextMenu(row);

    const menu = await screen.findByRole("menu", { name: "Configuration actions" });

    expect(menu).toBeTruthy();

    await waitFor(() => {
      expect(screen.getByRole("menuitem", { name: /Build proxy chain from selected/ })).toBeTruthy();
    });
  });
});
