/**
 * @vitest-environment jsdom
 *
 * Network page tests for the v0.9.8.5 surfaces:
 *
 *   - the Network Identity card (§6): renders its empty state, runs
 *     NOTHING on mount, and only on the explicit "Check identity"
 *     click calls the bounded identity service and renders the
 *     measured evidence (local IP, public IP, ISP/ASN — Unknown when
 *     unavailable, never fabricated);
 *   - the staged diagnostics ladder (§4): the report's stage rows
 *     render with their statuses and the first failed rung named;
 *   - the DNS diagnostic evidence (§5): per-resolver rows with
 *     A/AAAA outcomes and honest failure classes.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { NetworkPage } from "./Network";
import type { IdentityReportView, NetCheckReport, ToolResultView } from "../services";

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

function identityReport(overrides: Partial<IdentityReportView> = {}): IdentityReportView {
  return {
    local: {
      primary_ipv4: "192.168.1.42",
      primary_ipv6: "",
      interface: "en0",
      others: [],
      measured: true,
    },
    public: { direct_ip: "203.0.113.7", endpoint: "https://api.ipify.org" },
    metadata: {
      organization: "Example ISP Ltd.",
      asn: "AS64512",
      country: "TC",
      region: "Test Region",
      source: "https://ipinfo.io/json",
      available: true,
    },
    path: "direct",
    checked_at: "2026-09-20T00:00:00Z",
    duration_ms: 420,
    ...overrides,
  };
}

function stageReport(): NetCheckReport {
  return {
    state: "dns_failure",
    summary: "DNS resolution failed.",
    checked_at: "2026-09-20T00:00:00Z",
    duration_ms: 900,
    local_links: [{ name: "eth0", target: "", ok: true }],
    dns: [{ name: "resolver", target: "127.0.0.1:53", ok: false, error: "timeout" }],
    tcp: [{ name: "tcp", target: "127.0.0.1:80", ok: true, latency_ms: 12 }],
    https: [{ name: "https", target: "http://127.0.0.1/x", ok: true, latency_ms: 33 }],
    cancelled: false,
    target_count: 3,
    stages: [
      { stage: "local_link", status: "ok" },
      { stage: "local_ip", status: "ok", detail: "192.168.1.42 via en0" },
      { stage: "dns", status: "failed", failure_class: "timeout", detail: "resolver timed out" },
      { stage: "tcp", status: "ok", latency_ms: 12 },
      { stage: "tls", status: "skipped" },
      { stage: "https", status: "ok", latency_ms: 33 },
      { stage: "captive_portal", status: "skipped" },
      { stage: "direct_internet", status: "ok" },
      { stage: "tunnel_internet", status: "not_checked", detail: "no active session endpoint" },
    ],
    failed_stage: "dns",
  };
}

beforeEach(() => {
  mocks.LastReport.mockReset().mockResolvedValue(null);
  mocks.CheckConnection.mockReset().mockResolvedValue(stageReport());
  mocks.Tools.mockReset().mockResolvedValue([]);
  mocks.RunTool.mockReset();
  mocks.LiveTunnel.mockReset().mockResolvedValue({ active: false });
  mocks.NetworkIdentity.mockReset();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Network Identity card (v0.9.8.5 §6)", () => {
  it("renders the empty state and runs NOTHING outbound on mount", () => {
    render(<NetworkPage />);

    expect(screen.getByText("No identity check has run")).toBeTruthy();
    expect(mocks.NetworkIdentity).not.toHaveBeenCalled();
    expect(mocks.RunTool).not.toHaveBeenCalled();
  });

  it("calls the identity service only from the explicit button", async () => {
    mocks.NetworkIdentity.mockResolvedValue(identityReport());

    render(<NetworkPage />);

    const button = screen.getByRole("button", { name: /Check identity/i });
    fireEvent.click(button);

    await waitFor(() => {
      expect(mocks.NetworkIdentity).toHaveBeenCalledTimes(1);
    });

    expect(mocks.NetworkIdentity).toHaveBeenCalledWith({ tunneled: false });

    // The measured evidence renders.
    expect(screen.getByText("192.168.1.42")).toBeTruthy();
    expect(screen.getByText("203.0.113.7")).toBeTruthy();
    expect(screen.getByText("Example ISP Ltd.")).toBeTruthy();
    expect(screen.getByText("AS64512 · TC")).toBeTruthy();
  });

  it("reports Unknown metadata honestly when unavailable", async () => {
    const report = identityReport({
      metadata: { available: false },
    });

    mocks.NetworkIdentity.mockResolvedValue(report);

    render(<NetworkPage />);

    fireEvent.click(screen.getByRole("button", { name: /Check identity/i }));

    await waitFor(() => {
      expect(screen.getByText("Unknown")).toBeTruthy();
    });

    expect(screen.getByText("unavailable — never fabricated")).toBeTruthy();
  });

  it("offers the via-tunnel choice only when a live tunnel exists", async () => {
    mocks.LiveTunnel.mockResolvedValue({
      active: true,
      provider: "tor",
      endpoint: "127.0.0.1:1080",
      healthy: true,
    });

    mocks.NetworkIdentity.mockResolvedValue(
      identityReport({
        path: "tunneled",
        provider: "tor",
        public: { tunnel_ip: "198.51.100.9", direct_ip: "203.0.113.7", endpoint: "https://api.ipify.org" },
      }),
    );

    render(<NetworkPage />);

    const toggle = screen.getByLabelText(/via tunnel/i) as HTMLInputElement;
    await waitFor(() => {
      expect(toggle.disabled).toBe(false);
    });

    fireEvent.click(toggle);
    fireEvent.click(screen.getByRole("button", { name: /Check identity/i }));

    await waitFor(() => {
      expect(mocks.NetworkIdentity).toHaveBeenCalledWith({ tunneled: true });
    });

    // The tunneled exit identity renders with the direct comparison.
    expect(screen.getByText("198.51.100.9")).toBeTruthy();
  });
});

describe("Staged diagnostics ladder (v0.9.8.5 §4)", () => {
  it("renders the stage rows with statuses and names the first failed rung", async () => {
    render(<NetworkPage />);

    fireEvent.click(screen.getByRole("button", { name: /Check connection/i }));

    await waitFor(() => {
      expect(screen.getByText("Connection stages")).toBeTruthy();
    });

    // Every canonical stage renders.
    for (const label of [
      "Local link",
      "Local IP",
      "DNS",
      "TCP",
      "TLS",
      "HTTPS",
      "Captive portal",
      "Direct Internet",
      "Tunnel Internet",
    ]) {
      expect(screen.getByText(label)).toBeTruthy();
    }

    // The failed DNS stage carries its honest detail.
    const dnsRow = screen.getByText("DNS").closest("li");
    expect(dnsRow?.textContent).toContain("timeout");

    // The tunnel stage is honestly not checked.
    const tunnelRow = screen.getByText("Tunnel Internet").closest("li");
    expect(tunnelRow?.textContent).toContain("no active session endpoint");
  });

  it("omits the ladder when the backend report carries no stages (pre-0.9.8.5)", async () => {
    mocks.CheckConnection.mockResolvedValue({
      ...stageReport(),
      stages: undefined,
      failed_stage: undefined,
    });

    render(<NetworkPage />);

    fireEvent.click(screen.getByRole("button", { name: /Check connection/i }));

    await waitFor(() => {
      expect(screen.getByText("DNS resolution")).toBeTruthy();
    });

    expect(screen.queryByText("Connection stages")).toBeNull();
  });
});

describe("DNS diagnostic evidence (v0.9.8.5 §5)", () => {
  it("renders per-resolver rows with A/AAAA outcomes and failure classes", async () => {
    const dnsResult = {
      tool_id: "dns",
      started_at: "2026-09-20T00:00:00Z",
      finished_at: "2026-09-20T00:00:01Z",
      duration_ms: 1000,
      status: "ok",
      transport: "udp/tcp",
      dns: {
        name: "www.gstatic.com",
        started_at: "2026-09-20T00:00:00Z",
        duration_ms: 900,
        resolvers: [
          {
            resolver: "System",
            ok: true,
            transport: "system",
            latency_ms: 24,
            queries: [
              {
                resolver: "System",
                transport: "system",
                query_name: "www.gstatic.com",
                record_type: "A",
                ok: true,
                latency_ms: 24,
                answer_count: 1,
                addresses: ["142.250.0.1"],
                at: "2026-09-20T00:00:00Z",
              },
              {
                resolver: "System",
                transport: "system",
                query_name: "www.gstatic.com",
                record_type: "AAAA",
                ok: false,
                failure_class: "empty_answer",
                error: "no answers of this type",
                at: "2026-09-20T00:00:00Z",
              },
            ],
          },
          {
            resolver: "Cloudflare",
            address: "1.1.1.1",
            ok: true,
            transport: "udp",
            latency_ms: 18,
            queries: [
              {
                resolver: "Cloudflare",
                address: "1.1.1.1",
                transport: "udp",
                query_name: "www.gstatic.com",
                record_type: "A",
                ok: true,
                latency_ms: 18,
                answer_count: 2,
                addresses: ["142.250.0.1", "142.250.0.2"],
                at: "2026-09-20T00:00:00Z",
              },
            ],
          },
        ],
      },
    } as unknown as ToolResultView;

    mocks.Tools.mockResolvedValue([
      { id: "dns", label: "DNS diagnostic", group: "connectivity", takes_target: true, description: "" },
    ]);
    mocks.RunTool.mockResolvedValue(dnsResult);

    render(<NetworkPage />);

    await waitFor(() => {
      expect(screen.getByText("DNS diagnostic")).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: /^Run$/i }));

    await waitFor(() => {
      expect(screen.getByText("www.gstatic.com · 2 resolvers · A + AAAA")).toBeTruthy();
    });

    // Resolver rows with their evidence.
    expect(screen.getByText("System")).toBeTruthy();
    expect(screen.getByText("Cloudflare")).toBeTruthy();
    expect(screen.getByText(/1 answer/)).toBeTruthy();
    expect(screen.getByText(/2 answers/)).toBeTruthy();
    expect(screen.getByText(/empty_answer — no answers of this type/)).toBeTruthy();
  });
});
