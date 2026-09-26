/**
 * @vitest-environment jsdom
 *
 * v0.9.13 Configurations page regression coverage (§2):
 *
 *   - the primary row is the compact two-line hierarchy (favorite |
 *     protocol | name+health | endpoint+ping | Test | ⋮) and no longer
 *     shows transport, security, URL-test, test backend or source
 *     columns (they moved to the detail panel);
 *   - left click opens the detail panel (grouped sections);
 *   - the ⋮ overflow, right-click and the keyboard context-menu
 *     invocation (Shift+F10) all open the SAME action menu;
 *   - the menu actions ride the existing services (test, connect,
 *     favorite, select, details, copy endpoint);
 *   - copy endpoint copies ONLY the safe address:port pair.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ConfigsPage } from "./Configs";

const serviceMocks = vi.hoisted(() => ({
  TestConfig: vi.fn(),
  MoveConfig: vi.fn(),
  ListConfigsFiltered: vi.fn(),
  ConfigDetails: vi.fn(),
  Stats: vi.fn(),
  Paused: vi.fn(),
  Snapshot: vi.fn(),
  EnqueueByFilter: vi.fn(),
  CancelAll: vi.fn(),
  Pause: vi.fn(),
  Resume: vi.fn(),
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
    Stats: serviceMocks.Stats,
    Paused: serviceMocks.Paused,
    Snapshot: serviceMocks.Snapshot,
    EnqueueByFilter: serviceMocks.EnqueueByFilter,
    CancelAll: serviceMocks.CancelAll,
    Pause: serviceMocks.Pause,
    Resume: serviceMocks.Resume,
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

// Minimal standalone stores mirroring the state shape the page reads.
// Built inside vi.hoisted (async) so the hoisted vi.mock factories can
// consume them without TDZ errors.
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
    userGroups: [{ id: "g1", name: "Work", count: 1 }],
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
    tested_at: Date.now(),
    working: true,
    latency_ms: 40,
    ping: { median_ms: 42, samples: 3, packet_loss: 0 },
    url_test: { ok: true, status: 204, total_ms: 180 },
    test_backend: "xray",
    source: "seed-source",
    ...overrides,
  };
}

function detailFixture(overrides: Record<string, unknown> = {}) {
  return {
    id: "cfg-1",
    type: "vless",
    name: "Berlin edge",
    address: "berlin.example.com",
    port: 443,
    network: "ws",
    security: "tls",
    has_uuid: true,
    working: true,
    latency_ms: 40,
    tested_at: Date.now(),
    source: "seed-source",
    display: "vless://[REDACTED]@berlin.example.com:443",
    compatible_backends: ["xray"],
    ...overrides,
  };
}

async function renderPage(items: Record<string, unknown>[] = [config()]) {
  const { useConfigsStore } = await storesPromise;

  useConfigsStore.setState({ items, total: items.length });

  render(<ConfigsPage />);

  await waitFor(() => {
    expect(screen.getByRole("list", { name: "Configurations" })).toBeTruthy();
  });

  return await waitFor(() => screen.getByText("Berlin edge"));
}

beforeEach(async () => {
  vi.clearAllMocks();

  const { useConfigsStore } = await storesPromise;

  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  });

  serviceMocks.Stats.mockResolvedValue(null);
  serviceMocks.Paused.mockResolvedValue(false);
  serviceMocks.Snapshot.mockResolvedValue([]);
  serviceMocks.ListConfigsFiltered.mockResolvedValue({ items: [], total: 0 });
  serviceMocks.ConfigDetails.mockResolvedValue(detailFixture());
  serviceMocks.TestConfig.mockResolvedValue({});
  serviceMocks.EnqueueByFilter.mockResolvedValue({ enqueued: 0 });

  useConfigsStore.setState({ items: [], total: 0 });
});

afterEach(() => {
  cleanup();
});

describe("Configs dense table row (v0.11.0)", () => {
  it("renders the dense single-line table with the sticky column header", async () => {
    await renderPage();

    // Column header row labels the dense table (wide viewport).
    expect(screen.getByText("Transport")).toBeTruthy();
    expect(screen.getByText("Latency")).toBeTruthy();
    expect(screen.getByText("Test status")).toBeTruthy();
    expect(screen.getByText("Source")).toBeTruthy();

    // Core node-table columns: name, endpoint, transport, measured
    // ping, truthful test status and source are all on the row.
    expect(screen.getByText("Berlin edge")).toBeTruthy();
    expect(screen.getByText((_, el) => el?.textContent === "berlin.example.com:443")).toBeTruthy();
    expect(screen.getByText("ws/tls")).toBeTruthy(); // transport/security column
    expect(screen.getByText("42 ms")).toBeTruthy(); // measured ping
    expect(screen.getByText("Passed")).toBeTruthy(); // terminal status from evidence
    expect(screen.getByText("seed-source")).toBeTruthy(); // source column

    // URL-test/backend internals stay OFF the row (detail panel only).
    expect(screen.queryByText("xray")).toBeNull();

    // The row carries the v3 base class, the v0.11.0 table class and
    // the ⋮ trigger.
    const row = screen.getByText("Berlin edge").closest(".config-row-v3");
    expect(row).toBeTruthy();
    expect(row?.className).toContain("config-row-table");
    expect(screen.getByRole("button", { name: "More actions for Berlin edge" })).toBeTruthy();
  });

  it("shows the queue indicator as the single contextual state", async () => {
    // no queued ids → no Queued chip
    await renderPage();
    expect(screen.queryByText("Queued")).toBeNull();
  });

  it("keeps bulk selection reachable and visible", async () => {
    await renderPage();

    fireEvent.click(screen.getByRole("button", { name: "More actions for Berlin edge" }));

    await waitFor(() => {
      expect(screen.getByRole("menu", { name: "Configuration actions" })).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("menuitem", { name: "Select for bulk testing" }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /Test selected \(1\)/ })).toBeTruthy();
    });

    // The bulk-selected row keeps a visible accent class.
    const row = screen.getByText("Berlin edge").closest(".config-row-v3");
    expect(row?.className).toContain("bulk-selected");
  });
});

describe("Configs detail panel (v0.9.13)", () => {
  it("opens on row click with grouped technical sections", async () => {
    const row = await renderPage();

    fireEvent.click(row);

    await waitFor(() => {
      expect(screen.getByRole("complementary", { name: "Configuration details" })).toBeTruthy();
    });

    const panel = screen.getByRole("complementary", { name: "Configuration details" });

    // Grouped sections render in order.
    expect(screen.getByText("Overview")).toBeTruthy();
    expect(screen.getByText("Endpoint")).toBeTruthy();
    expect(screen.getByText("Measurement")).toBeTruthy();
    expect(screen.getByText("Health")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Source" })).toBeTruthy();

    // Technical facts that left the wide ROW live here (the source
    // column also carries the source name, so match within the
    // panel; test backend AND compatible cores both report xray).
    expect(within(panel).getByText("ws")).toBeTruthy();
    expect(within(panel).getByText("tls")).toBeTruthy();
    expect(within(panel).getByText("seed-source")).toBeTruthy();
    expect(screen.getAllByText("xray").length).toBeGreaterThanOrEqual(2);
    expect(serviceMocks.ConfigDetails).toHaveBeenCalledWith("cfg-1");
  });
});

describe("Configs row action menu (v0.9.13)", () => {
  it("opens the same menu from ⋮, right-click and keyboard", async () => {
    const row = await renderPage();

    // Right-click on the row opens the context menu.
    fireEvent.contextMenu(row);

    await waitFor(() => {
      expect(screen.getByRole("menu", { name: "Configuration actions" })).toBeTruthy();
    });

    expect(screen.getByRole("menuitem", { name: "Retest" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Connect" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Save Berlin edge as favorite" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "View details" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Copy endpoint" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Add to Work" })).toBeTruthy();

    // Escape closes.
    fireEvent.keyDown(screen.getByRole("menu", { name: "Configuration actions" }), { key: "Escape" });

    await waitFor(() => {
      expect(screen.queryByRole("menu", { name: "Configuration actions" })).toBeNull();
    });

    // Keyboard invocation (Shift+F10) opens the same surface.
    fireEvent.keyDown(row, { key: "F10", shiftKey: true });

    await waitFor(() => {
      expect(screen.getByRole("menu", { name: "Configuration actions" })).toBeTruthy();
    });

    // The ⋮ button opens it as well.
    fireEvent.click(screen.getByRole("button", { name: "More actions for Berlin edge" }));

    await waitFor(() => {
      expect(screen.getAllByRole("menu", { name: "Configuration actions" }).length).toBeGreaterThan(0);
    });
  });

  it("toggles closed on a ⋮ re-click (trigger toggle race)", async () => {
    await renderPage();

    const dots = screen.getByRole("button", { name: "More actions for Berlin edge" });

    // Open via ⋮.
    fireEvent.click(dots);

    await waitFor(() => {
      expect(screen.getByRole("menu", { name: "Configuration actions" })).toBeTruthy();
    });

    // A real re-click on the trigger is mousedown (ignored as the
    // trigger) + click (must CLOSE, not reopen — the flip-flop bug).
    fireEvent.mouseDown(dots);
    fireEvent.click(dots);

    await waitFor(() => {
      expect(screen.queryByRole("menu", { name: "Configuration actions" })).toBeNull();
    });
  });

  it("runs Test through the existing service from the menu", async () => {
    const row = await renderPage();

    fireEvent.contextMenu(row);

    fireEvent.click(screen.getByRole("menuitem", { name: "Retest" }));

    await waitFor(() => {
      expect(serviceMocks.TestConfig).toHaveBeenCalledWith("cfg-1");
    });
  });

  it("copies only the safe address:port pair", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);

    Object.assign(navigator, { clipboard: { writeText } });

    const row = await renderPage();

    fireEvent.contextMenu(row);
    fireEvent.click(screen.getByRole("menuitem", { name: "Copy endpoint" }));

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledWith("berlin.example.com:443");
    });

    // Never the credential-bearing URL.
    expect(writeText.mock.calls[0][0]).not.toContain("vless://");
  });
});

describe("Group actions: honest result handling (v0.11.0)", () => {
  it("never reports success when every group addition failed", async () => {
    const { useCollectionsStore } = await storesPromise;
    const { toast } = await import("../state/toastStore");
    const toastMock = vi.mocked(toast);

    // The backend refuses BOTH additions.
    useCollectionsStore.setState({
      addManyToGroup: async (_groupID, ids) => ({
        added: 0,
        failed: ids.length,
        firstError: "backend refused the addition",
      }),
    });

    await renderPage([config(), config({ id: "cfg-2", name: "Osaka edge" })]);

    // Select both rows through the row action menu (the real model).
    for (const name of ["Berlin edge", "Osaka edge"]) {
      fireEvent.click(screen.getByRole("button", { name: `More actions for ${name}` }));

      await waitFor(() => {
        expect(screen.getByRole("menu", { name: "Configuration actions" })).toBeTruthy();
      });

      fireEvent.click(screen.getByRole("menuitem", { name: /Select for bulk testing/ }));
    }

    // Choose the group in the add-selected select.
    const select = screen.getByRole("combobox", { name: "Add selected to group" });
    fireEvent.change(select, { target: { value: "g1" } });

    await waitFor(() => {
      const kinds = toastMock.mock.calls.map((call) => call[0]);
      expect(kinds).toContain("error");
    });

    const kinds = toastMock.mock.calls.map((call) => call[0]);
    expect(kinds).not.toContain("success");
  });

  it("reports a partial failure as a diagnostic, not a success", async () => {
    const { useCollectionsStore } = await storesPromise;
    const { toast } = await import("../state/toastStore");
    const toastMock = vi.mocked(toast);

    useCollectionsStore.setState({
      addManyToGroup: async (_groupID, ids) => ({
        added: 1,
        failed: ids.length - 1,
        firstError: "one config vanished mid-add",
      }),
    });

    await renderPage([config(), config({ id: "cfg-2", name: "Osaka edge" })]);

    for (const name of ["Berlin edge", "Osaka edge"]) {
      fireEvent.click(screen.getByRole("button", { name: `More actions for ${name}` }));

      await waitFor(() => {
        expect(screen.getByRole("menu", { name: "Configuration actions" })).toBeTruthy();
      });

      fireEvent.click(screen.getByRole("menuitem", { name: /Select for bulk testing/ }));
    }

    const select = screen.getByRole("combobox", { name: "Add selected to group" });
    fireEvent.change(select, { target: { value: "g1" } });

    await waitFor(() => {
      const kinds = toastMock.mock.calls.map((call) => call[0]);
      expect(kinds).toContain("warn");
    });

    const kinds = toastMock.mock.calls.map((call) => call[0]);
    expect(kinds).not.toContain("success");
  });
});
