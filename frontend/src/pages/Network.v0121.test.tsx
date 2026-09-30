/**
 * @vitest-environment jsdom
 *
 * v0.12.1 Network-page status semantics (§3/§13):
 *
 *   - prerequisite outcomes (not_configured / not_applicable /
 *     unsupported) render with calm info/neutral bands — never the
 *     red failure styling reserved for genuine failures;
 *   - tunnel diagnostics is blocked with the honest "No active
 *     tunnel" hint when no tunnel exists (no post-hoc red failure);
 *   - tool cards show WHAT each tool measures;
 *   - nothing runs automatically on mount.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { fireEvent } from "@testing-library/react";
import { NetworkPage } from "./Network";
import type { ToolInfoView, ToolResultView } from "../services";

const mocks = vi.hoisted(() => ({
  LastReport: vi.fn(),
  CheckConnection: vi.fn(),
  Tools: vi.fn(),
  RunTool: vi.fn(),
  LiveTunnel: vi.fn(),
  NetworkIdentity: vi.fn(),
}));

vi.mock("../services", () => ({
  call: async (operation: () => Promise<unknown>) => operation(),
  networkService: {
    LastReport: mocks.LastReport,
    CheckConnection: mocks.CheckConnection,
  },
  toolsService: {
    Tools: mocks.Tools,
    RunTool: mocks.RunTool,
    LiveTunnel: mocks.LiveTunnel,
    NetworkIdentity: mocks.NetworkIdentity,
  },
}));

function toolInfo(overrides: Partial<ToolInfoView> = {}): ToolInfoView {
  return {
    id: "tcp",
    label: "TCP",
    group: "connectivity",
    what: "Raw TCP connect round trip to one endpoint",
    takes_target: false,
    timeout_ms: 10000,
    ...overrides,
  };
}

function toolResult(overrides: Partial<ToolResultView> = {}): ToolResultView {
  return {
    tool_id: "tcp",
    started_at: "2026-09-30T00:00:00Z",
    finished_at: "2026-09-30T00:00:01Z",
    duration_ms: 4,
    status: "not_configured",
    error: "no HTTP proxy is configured",
    measurement: {},
    ...overrides,
  };
}

describe("Network v0.12.1 status semantics", () => {
  beforeEach(() => {
    mocks.LastReport.mockResolvedValue(null);
    mocks.CheckConnection.mockResolvedValue(null);
    mocks.Tools.mockResolvedValue([
      toolInfo(),
      toolInfo({
        id: "tunnel_diagnostics",
        label: "Tunnel diagnostics",
        group: "tunnel",
        what: "Live active-tunnel truth with a real SOCKS5 CONNECT check",
      }),
    ]);
    mocks.LiveTunnel.mockResolvedValue({ active: false });
    mocks.NetworkIdentity.mockResolvedValue(null);
    mocks.RunTool.mockResolvedValue(null);
  });

  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("runs NOTHING outbound on mount", async () => {
    render(<NetworkPage />);

    await screen.findByText("Internet tools");

    expect(mocks.RunTool).not.toHaveBeenCalled();
    expect(mocks.NetworkIdentity).not.toHaveBeenCalled();
  });

  it("greys the tunnel diagnostics action with an honest hint when no tunnel exists", async () => {
    render(<NetworkPage />);

    const blocked = await screen.findByRole("button", { name: "No active tunnel" });

    expect((blocked as HTMLButtonElement).disabled).toBe(true);
    expect(blocked.getAttribute("title")).toBe("No active tunnel");

    // The backend was never asked to run a doomed tool.
    expect(mocks.RunTool).not.toHaveBeenCalled();
  });

  it("shows what each tool measures on the card", async () => {
    render(<NetworkPage />);

    expect(await screen.findByText("Raw TCP connect round trip to one endpoint")).toBeTruthy();
    expect(
      await screen.findByText("Live active-tunnel truth with a real SOCKS5 CONNECT check"),
    ).toBeTruthy();
  });

  it.each([
    ["not_configured", "badge info", "Not configured"],
    ["unsupported", "badge info", "unsupported"],
    ["not_applicable", "badge neutral", "Not applicable"],
    ["failed", "badge error", "failed"],
    ["partial", "badge warn", "partial"],
  ])(
    "renders status %s in the %s band (never red for prerequisites)",
    async (status, expectedClass, expectedLabel) => {
      mocks.RunTool.mockResolvedValue(toolResult({ status: status as string }));

      render(<NetworkPage />);

      // Run the TCP tool from its card — the only trigger.
      const runButtons = await screen.findAllByRole("button", { name: /Run/ });
      fireEvent.click(runButtons[0]);

      await waitFor(() => {
        expect(screen.getByText(expectedLabel)).toBeTruthy();
      });

      const badge = screen.getByText(expectedLabel);

      expect(badge.className).toContain(expectedClass);
    },
  );
});
