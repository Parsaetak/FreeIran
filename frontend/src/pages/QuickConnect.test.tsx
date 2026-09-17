/**
 * @vitest-environment jsdom
 *
 * Quick Connect page integration tests (§30): the page must render,
 * expose exactly ONE primary connect action, drive the EXISTING
 * connection store (connect / connectBest) and the adaptive start
 * flow, and reflect real backend states (connecting / connected /
 * failed) — plus keyboard operation and reduced motion.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QuickConnectPage } from "./QuickConnect";
import { useConnectionStore } from "../state/connectionStore";
import { useQuickConnectStore } from "../state/quickConnectStore";
import { useStartFlowStore } from "../state/startflowStore";
import { useSettingsStore } from "../state/settingsStore";
import type { CandidateView } from "../services";

const mocks = vi.hoisted(() => ({
  BestCandidates: vi.fn(),
  Connect: vi.fn(),
  ConnectBest: vi.fn(),
  RunStartFlow: vi.fn(),
}));

vi.mock("../services", () => ({
  call: (operation: () => Promise<unknown>) => operation(),
  connectionService: {
    BestCandidates: mocks.BestCandidates,
    Connect: mocks.Connect,
    ConnectBest: mocks.ConnectBest,
    ConnectionState: vi.fn(),
    RefreshBackends: vi.fn(),
    Disconnect: vi.fn(),
    Reconnect: vi.fn(),
  },
  discoveryService: {
    StartFlowStatus: vi.fn(async () => ({ stage: "idle", running: false })),
    Environment: vi.fn(async () => ({
      signals: [],
      restricted: false,
      deep_discovery_advised: false,
      summary: "",
      analyzed_at: "2026-01-01T00:00:00Z",
    })),
    RunStartFlow: mocks.RunStartFlow,
    CancelStartFlow: vi.fn(),
    DiscoverNow: vi.fn(),
  },
  settingsService: {
    Get: vi.fn(async () => ({ reduced_motion: false })),
    Save: vi.fn(),
  },
}));

vi.mock("@wailsio/runtime", () => ({
  Events: { On: vi.fn(() => () => undefined), Emit: vi.fn() },
}));

function candidate(fingerprint: string, latency: number, overrides: Partial<CandidateView> = {}): CandidateView {
  return {
    fingerprint,
    name: `cfg-${fingerprint}`,
    protocol: "vless",
    endpoint: `${fingerprint}.example:443`,
    class: "good",
    score: 0.5,
    latency_ms: latency,
    success_rate: 1,
    samples: 2,
    tested_at: Date.now() - 1000,
    connectable: true,
    ...overrides,
  };
}

/**
 * Partial snapshots including v0.9.7 fields the generated binding
 * does not carry yet (verification) — the store contract test uses
 * the same structural convention (`as never`).
 */
function snap(partial: Record<string, unknown>) {
  return partial as never;
}

function resetStores() {
  act(() => {
    useConnectionStore.setState({
      snapshot: null,
      backends: [],
      health: null,
      busy: false,
      error: null,
    });
    useQuickConnectStore.setState({
      candidates: [],
      loading: false,
      loaded: false,
      error: null,
      selected: null,
    });
    useStartFlowStore.setState({
      status: null,
      environment: null,
      busy: false,
      error: null,
      lastResult: null,
    });
    useSettingsStore.setState({
      settings: null,
      loading: false,
      saving: false,
      lastError: null,
      motionOverride: null,
    });
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  resetStores();
  mocks.RunStartFlow.mockResolvedValue({
    discovered: 0,
    valid: 0,
    duplicates: 0,
    tested: 0,
    verified: false,
    duration_ms: 0,
  });
});

afterEach(() => {
  cleanup();
});

describe("Quick Connect page", () => {
  it("renders the ready state with a single primary CONNECT action", async () => {
    mocks.BestCandidates.mockResolvedValue([candidate("pl", 82)]);

    const { container } = render(<QuickConnectPage onNavigate={vi.fn()} />);

    expect(screen.getByText("Ready to connect")).toBeTruthy();
    expect(screen.getByRole("button", { name: /CONNECT/ })).toBeTruthy();

    await waitFor(() => expect(useQuickConnectStore.getState().loaded).toBe(true));

    // Exactly one primary action element in the hero.
    expect(container.querySelectorAll(".qc-connect")).toHaveLength(1);
  });

  it("loads candidates ordered by measured ping into the picker", async () => {
    mocks.BestCandidates.mockResolvedValue([candidate("slow", 200), candidate("fast", 80)]);

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await waitFor(() => expect(useQuickConnectStore.getState().loaded).toBe(true));

    const state = useQuickConnectStore.getState();

    expect(state.candidates.map((v) => v.fingerprint)).toEqual(["fast", "slow"]);
  });

  it("expands the picker, selects a configuration and connects to IT on CONNECT", async () => {
    const user = userEvent.setup();
    mocks.BestCandidates.mockResolvedValue([candidate("pl", 82), candidate("de", 91)]);
    mocks.Connect.mockResolvedValue({ state: "connected", config_id: "de" });

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await waitFor(() => expect(useQuickConnectStore.getState().loaded).toBe(true));

    const trigger = screen.getByRole("button", { name: /automatic best selection/i });

    await user.click(trigger);

    const listbox = screen.getByRole("listbox");

    expect(listbox).toBeTruthy();
    expect(screen.getAllByRole("option")).toHaveLength(3); // Auto + 2 candidates

    await user.click(screen.getByRole("option", { name: /cfg-de, 91 ms/ }));

    await user.click(screen.getByRole("button", { name: /CONNECT/ }));

    await waitFor(() => expect(mocks.Connect).toHaveBeenCalledWith("de"));
    expect(mocks.ConnectBest).not.toHaveBeenCalled();
  });

  it("routes the Auto path through the existing best-candidate engine", async () => {
    const user = userEvent.setup();
    mocks.BestCandidates.mockResolvedValue([candidate("pl", 82)]);
    mocks.ConnectBest.mockResolvedValue({
      snapshot: { state: "connected" },
      chosen: candidate("pl", 82),
      candidates: 1,
    });

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await waitFor(() => expect(useQuickConnectStore.getState().loaded).toBe(true));

    await user.click(screen.getByRole("button", { name: /CONNECT/ }));

    await waitFor(() => expect(mocks.ConnectBest).toHaveBeenCalledWith([]));
    expect(mocks.Connect).not.toHaveBeenCalled();
  });

  it("falls back to the adaptive discovery flow when no candidates exist", async () => {
    const user = userEvent.setup();
    mocks.BestCandidates.mockResolvedValue([]);

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await waitFor(() => expect(useQuickConnectStore.getState().loaded).toBe(true));

    expect(screen.getByText(/No tested connections available/i)).toBeTruthy();

    await user.click(screen.getByRole("button", { name: /CONNECT/ }));

    await waitFor(() => expect(mocks.RunStartFlow).toHaveBeenCalled());
    expect(mocks.ConnectBest).not.toHaveBeenCalled();
  });

  it("shows the preparing state from the real connection state machine", () => {
    act(() => {
      useConnectionStore.setState({ snapshot: snap({ state: "preparing" }) });
    });

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    expect(screen.getByText("Preparing connection")).toBeTruthy();
    expect(screen.getByText("Selecting best available route…")).toBeTruthy();

    const button = screen.getByRole("button", { name: /PREPARING/ }) as HTMLButtonElement;

    expect(button.disabled).toBe(true);
  });

  it("shows the verifying state while the core readiness check runs", () => {
    act(() => {
      useConnectionStore.setState({ snapshot: snap({ state: "waiting_for_ready" }) });
    });

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    expect(screen.getByText("Verifying")).toBeTruthy();
    expect(screen.getByText("Checking connectivity…")).toBeTruthy();
  });

  it("renders the connected state as a status representation — not a second button", () => {
    const { container } = render(<QuickConnectPage onNavigate={vi.fn()} />);

    act(() => {
      useConnectionStore.setState({
        snapshot: snap({
          state: "connected",
          config_name: "Warsaw edge",
          config_id: "pl",
          core: "xray",
          core_version: "26.3.27",
          latency_ms: 84,
          verification: "usable",
        }),
      });
    });

    expect(screen.getByText("Connected")).toBeTruthy();
    expect(screen.getByText(/Warsaw edge/)).toBeTruthy();
    expect(screen.getByText(/84 ms/)).toBeTruthy();
    expect(screen.getByText(/xray · 26.3.27/)).toBeTruthy();
    expect(screen.getByText("Verified")).toBeTruthy();

    // The primary element exists exactly once and is NOT a button:
    // no competing action lives next to CONNECTED.
    const primary = container.querySelector(".qc-connect");

    expect(primary).toBeTruthy();
    expect(primary?.tagName).not.toBe("BUTTON");
    expect(screen.queryByRole("button", { name: /CONNECT/ })).toBeNull();
  });

  it("shows the failed state with an honest message and a retry action", () => {
    render(<QuickConnectPage onNavigate={vi.fn()} />);

    act(() => {
      useConnectionStore.setState({ snapshot: snap({ state: "connection_failed" }) });
    });

    expect(screen.getByText("Connection failed")).toBeTruthy();
    expect(screen.getByText("No usable connection was verified.")).toBeTruthy();

    // Retry is the same single primary action.
    expect(screen.getByRole("button", { name: /CONNECT/ })).toBeTruthy();
  });

  it("marks the hero busy while connecting (aria-busy) and keeps a live status region", () => {
    const { container } = render(<QuickConnectPage onNavigate={vi.fn()} />);

    expect(container.querySelector('[aria-live="polite"]')).toBeTruthy();

    act(() => {
      useConnectionStore.setState({ snapshot: snap({ state: "starting_core" }), busy: true });
    });

    expect(container.querySelector('.qc-hero[aria-busy="true"]')).toBeTruthy();
  });

  it("operates the picker with the keyboard", async () => {
    const user = userEvent.setup();
    mocks.BestCandidates.mockResolvedValue([candidate("pl", 82), candidate("de", 91)]);
    mocks.Connect.mockResolvedValue({ state: "connected", config_id: "de" });

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await waitFor(() => expect(useQuickConnectStore.getState().loaded).toBe(true));

    await user.click(screen.getByRole("button", { name: /automatic best selection/i }));

    const listbox = screen.getByRole("listbox");

    expect(document.activeElement).toBe(listbox);

    // ArrowDown twice: Auto → cfg-pl → cfg-de, then Enter selects.
    fireEvent.keyDown(listbox, { key: "ArrowDown" });
    fireEvent.keyDown(listbox, { key: "ArrowDown" });
    fireEvent.keyDown(listbox, { key: "Enter" });

    expect(useQuickConnectStore.getState().selected).toBe("de");

    // Escape closes and returns focus to the trigger.
    await user.click(screen.getByRole("button", { name: /configuration: cfg-de/i }));
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Escape" });

    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("applies the reduced-motion class when the accessibility setting is on", async () => {
    act(() => {
      useSettingsStore.setState({ motionOverride: true });
    });

    const { container } = render(<QuickConnectPage onNavigate={vi.fn()} />);

    expect(container.querySelector(".qc-hero.reduced")).toBeTruthy();
  });

  it("does not poll: candidates load once for the mount", async () => {
    mocks.BestCandidates.mockResolvedValue([candidate("pl", 82)]);

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await waitFor(() => expect(useQuickConnectStore.getState().loaded).toBe(true));

    expect(mocks.BestCandidates).toHaveBeenCalledTimes(1);
  });

  it("navigates to the Connection page from the connected-state secondary link", async () => {
    const user = userEvent.setup();
    const onNavigate = vi.fn();

    render(<QuickConnectPage onNavigate={onNavigate} />);

    act(() => {
      useConnectionStore.setState({
        snapshot: snap({ state: "connected", config_name: "x", latency_ms: 10, verification: "usable" }),
      });
    });

    await user.click(screen.getByRole("button", { name: /Disconnect & advanced controls/i }));

    expect(onNavigate).toHaveBeenCalledWith("connection");
  });
});
