/**
 * @vitest-environment jsdom
 *
 * v0.12.1 Configuration workspace tests (§14–§22/§25/§30):
 *
 *   - the scope rail renders SOURCE scopes with their authoritative
 *     counts from SourceStatsList (never recomputed in React);
 *   - selecting a source scope filters through the server-side
 *     pipeline (cfg.Source == sourceID — no full-database download);
 *   - the source scope header shows measured evidence and the
 *     per-source actions: Update (targeted refresh) and Check
 *     (through the ONE shared test queue);
 *   - a user group scope NEVER offers "Update source" (a user group
 *     is not a remote source);
 *   - right-clicking a row still opens FreeIran's MenuSurface, while
 *     right-clicking page whitespace opens nothing (the native
 *     browser menu is suppressed app-wide);
 *   - keyboard UX: Ctrl+A selects the visible scope.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ConfigsPage } from "./Configs";

const serviceMocks = vi.hoisted(() => ({
  TestConfig: vi.fn(),
  MoveConfig: vi.fn(),
  ListConfigsFiltered: vi.fn(),
  ConfigDetails: vi.fn(),
  LiveState: vi.fn(),
  EnqueueByFilter: vi.fn(),
  CancelAll: vi.fn(),
  Pause: vi.fn(),
  Resume: vi.fn(),
  SourceStatsList: vi.fn(),
  RefreshSource: vi.fn(),
}));

vi.mock("../services", () => ({
  call: async (operation: () => Promise<unknown>) => operation(),
  dataService: {
    TestConfig: serviceMocks.TestConfig,
    MoveConfig: serviceMocks.MoveConfig,
    ListConfigsFiltered: serviceMocks.ListConfigsFiltered,
  },
  connectionService: {
    ConfigDetails: serviceMocks.ConfigDetails,
  },
  testQueueService: {
    LiveState: serviceMocks.LiveState,
    EnqueueByFilter: serviceMocks.EnqueueByFilter,
    CancelAll: serviceMocks.CancelAll,
    Pause: serviceMocks.Pause,
    Resume: serviceMocks.Resume,
  },
  sourceService: {
    SourceStatsList: serviceMocks.SourceStatsList,
    RefreshSource: serviceMocks.RefreshSource,
  },
}));

vi.mock("@wailsio/runtime", () => ({
  Events: {
    On: () => () => undefined,
    Emit: () => undefined,
  },
}));

vi.mock("@tanstack/react-virtual", () => ({
  // jsdom has no layout engine: render the full (small) item range.
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

vi.mock("../state/toastStore", () => ({
  toast: vi.fn(),
  describeError: (error: unknown) => String(error),
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
    toggleFavorite: (id: string) => Promise<boolean>;
    createUserGroup: () => Promise<void>;
    deleteUserGroup: () => Promise<void>;
    addToGroup: () => Promise<void>;
    addManyToGroup: (
      groupID: string,
      ids: string[],
    ) => Promise<{ added: number; failed: number; firstError: string | null }>;
    removeManyFromGroup: (
      groupID: string,
      ids: string[],
    ) => Promise<{ removed: number; failed: number; firstError: string | null }>;
    renameUserGroup: () => Promise<void>;
  }>(() => ({
    builtinGroups: [],
    userGroups: [],
    favorites: [],
    load: async () => undefined,
    toggleFavorite: async () => true,
    createUserGroup: async () => undefined,
    deleteUserGroup: async () => undefined,
    addToGroup: async () => undefined,
    addManyToGroup: async (_groupID, ids) => ({ added: ids.length, failed: 0, firstError: null }),
    removeManyFromGroup: async (_groupID, ids) => ({
      removed: ids.length,
      failed: 0,
      firstError: null,
    }),
    renameUserGroup: async () => undefined,
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

  return { useConfigsStore, useCollectionsStore, useConnectionStore, useQuickConnectStore };
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
    id: "cfg-1",
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

const sourceStatsFixture = [
  {
    id: "src-a",
    name: "Source A",
    url: "https://example.com/a",
    enabled: true,
    trust: "public",
    config_count: 1200,
    working_count: 340,
    last_successful_fetch: "2026-09-30T00:00:00Z",
    last_failure: "",
    last_failure_reason: "",
  },
  {
    id: "src-b",
    name: "Source B",
    url: "https://example.com/b",
    enabled: false,
    trust: "user",
    config_count: 40,
    working_count: 0,
    last_successful_fetch: "",
    last_failure: "2026-09-29T10:00:00Z",
    last_failure_reason: "http 503",
  },
];

beforeEach(async () => {
  vi.clearAllMocks();

  const { useConfigsStore, useCollectionsStore } = await storesPromise;

  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  });

  serviceMocks.SourceStatsList.mockResolvedValue(sourceStatsFixture);
  serviceMocks.RefreshSource.mockResolvedValue(sourceStatsFixture[0]);
  serviceMocks.LiveState.mockResolvedValue(null);
  serviceMocks.ListConfigsFiltered.mockResolvedValue({
    items: [config()],
    total: 1,
  });
  serviceMocks.ConfigDetails.mockResolvedValue({ ...config(), display: "vless://[REDACTED]" });
  serviceMocks.TestConfig.mockResolvedValue({});
  serviceMocks.EnqueueByFilter.mockResolvedValue({ enqueued: 3 });

  useConfigsStore.setState({ items: [config()], total: 1 });
  useCollectionsStore.setState({
    builtinGroups: [],
    userGroups: [],
    favorites: [],
  });
});

afterEach(() => {
  cleanup();
});

describe("Configuration scope rail (v0.12.1)", () => {
  it("renders source scopes with authoritative counts", async () => {
    render(<ConfigsPage />);

    const chip = await screen.findByRole("button", { name: /Source A/ });

    expect(chip.textContent).toContain("1200");

    const chipB = await screen.findByRole("button", { name: /Source B/ });

    expect(chipB.textContent).toContain("40");
  });

  it("selecting a source scope filters server-side by cfg.Source", async () => {
    render(<ConfigsPage />);

    const chip = await screen.findByRole("button", { name: /Source A/ });

    fireEvent.click(chip);

    await waitFor(() => {
      expect(serviceMocks.ListConfigsFiltered).toHaveBeenCalledWith(
        expect.objectContaining({ source: "src-a" }),
        0,
        1000,
      );
    });
  });

  it("shows the source scope header with measured evidence and per-source actions", async () => {
    render(<ConfigsPage />);

    fireEvent.click(await screen.findByRole("button", { name: /Source A/ }));

    // Measured evidence — from the backend's model, never recomputed.
    expect(await screen.findByText("340 working")).toBeTruthy();
    expect(screen.getByText("public")).toBeTruthy();

    // Per-source actions.
    expect(screen.getByRole("button", { name: /Update source/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Check source/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Check untested/ })).toBeTruthy();
  });

  it("updates ONE source through the targeted refresh service", async () => {
    render(<ConfigsPage />);

    fireEvent.click(await screen.findByRole("button", { name: /Source A/ }));

    const update = await screen.findByRole("button", { name: /Update source/ });

    fireEvent.click(update);

    await waitFor(() => {
      expect(serviceMocks.RefreshSource).toHaveBeenCalledWith("src-a");
    });

    // The scoped table re-reconciles through the server-side filter.
    expect(serviceMocks.ListConfigsFiltered).toHaveBeenCalled();
  });

  it("checks ONE source through the shared bounded test queue", async () => {
    render(<ConfigsPage />);

    fireEvent.click(await screen.findByRole("button", { name: /Source A/ }));

    fireEvent.click(await screen.findByRole("button", { name: /Check source/ }));

    await waitFor(() => {
      expect(serviceMocks.EnqueueByFilter).toHaveBeenCalledWith(
        expect.objectContaining({ source: "src-a", scope: "all", origin: "user" }),
      );
    });
  });

  it("never offers Update source for a user group scope", async () => {
    const { useCollectionsStore } = await storesPromise;

    useCollectionsStore.setState({
      userGroups: [{ id: "g1", name: "Work", count: 2 }],
    });

    render(<ConfigsPage />);

    const workChip = (await screen.findAllByRole("button", { name: /Work/ }))[0];

    fireEvent.click(workChip);

    await waitFor(() => {
      expect(serviceMocks.ListConfigsFiltered).toHaveBeenCalledWith(
        expect.objectContaining({ group: "g1" }),
        0,
        1000,
      );
    });

    expect(screen.queryByRole("button", { name: /Update source/ })).toBeNull();
  });

  it("keeps the disabled-source flag visible in the scope header", async () => {
    render(<ConfigsPage />);

    fireEvent.click(await screen.findByRole("button", { name: /Source B/ }));

    expect(await screen.findByText("Disabled")).toBeTruthy();

    // The failure evidence is the backend's, with the recorded reason.
    expect(screen.getByText(/failed .* — http 503/)).toBeTruthy();
  });
});

describe("Right-click policy (v0.12.1 §25)", () => {
  it("opens FreeIran's MenuSurface from a row right-click", async () => {
    render(<ConfigsPage />);

    const row = await screen.findByRole("listitem");

    fireEvent.contextMenu(row);

    const menu = await screen.findByRole("menu", { name: "Configuration actions" });

    expect(menu).toBeTruthy();
  });

  it("suppresses the native menu on page whitespace, keeps it for text fields", async () => {
    const { useSuppressNativeContextMenu, isTextEditable } = await import(
      "../utilities/contextMenuPolicy"
    );

    function Probe() {
      useSuppressNativeContextMenu();

      return (
        <div>
          <input type="text" aria-label="editable" />
          <button type="button">plain surface</button>
        </div>
      );
    }

    render(<Probe />);

    const plain = screen.getByRole("button", { name: "plain surface" });
    const input = screen.getByLabelText("editable") as HTMLInputElement;

    const onPlain = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    plain.dispatchEvent(onPlain);
    expect(onPlain.defaultPrevented).toBe(true);

    const onInput = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    input.dispatchEvent(onInput);
    expect(onInput.defaultPrevented).toBe(false);

    // The editable-target contract is the shared policy's decision.
    expect(isTextEditable(input)).toBe(true);
    expect(isTextEditable(plain)).toBe(false);
  });

  it("opens the row context menu from the keyboard (Shift+F10)", async () => {
    render(<ConfigsPage />);

    const row = await screen.findByRole("listitem");

    fireEvent.keyDown(row, { key: "F10", shiftKey: true });

    expect(await screen.findByRole("menu", { name: "Configuration actions" })).toBeTruthy();
  });
});

describe("Keyboard UX (v0.12.1 §30)", () => {
  it("Ctrl+A selects the visible scope", async () => {
    render(<ConfigsPage />);

    await screen.findByRole("list", { name: "Configurations" });

    fireEvent.keyDown(document.body, { key: "a", ctrlKey: true });

    const selectAll = await screen.findByRole("button", { name: /Test selected \(1\)/ });

    expect(selectAll).toBeTruthy();
  });
});
