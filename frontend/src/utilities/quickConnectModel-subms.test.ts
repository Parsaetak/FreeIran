import { describe, expect, it } from "vitest";
import type { CandidateView } from "../services";
import { orderForQuickConnect, pickerLatencyText, quickPickerRows } from "./quickConnectModel";

/**
 * v0.9.8.1 sub-millisecond semantics (picker model contract): a
 * MEASURED 0 ms latency is the fastest band — never "unmeasured" —
 * and only the measured flag unlocks the "< 1 ms" presentation.
 *
 * Complements quickConnectModel.test.ts; test names deliberately do
 * not overlap with the existing suite.
 */

const NOW = 1_800_000_000_000;

function view(overrides: Partial<CandidateView>): CandidateView {
  return {
    fingerprint: "fp",
    name: "candidate",
    protocol: "vless",
    endpoint: "example.com:443",
    class: "good",
    score: 0,
    latency_ms: 0,
    success_rate: 1,
    samples: 2,
    tested_at: NOW - 1000,
    connectable: true,
    ...overrides,
  };
}

describe("measured sub-millisecond candidates", () => {
  it("measured sub-millisecond candidate sorts first among measured", () => {
    const subms = view({ fingerprint: "subms", latency_ms: 0, latency_ms_measured: true });
    const ninety = view({ fingerprint: "ninety", latency_ms: 90, latency_ms_measured: true });
    const twoHundred = view({ fingerprint: "two-hundred", latency_ms: 200, latency_ms_measured: true });

    const ordered = orderForQuickConnect([twoHundred, ninety, subms], NOW);

    expect(ordered.map((v) => v.fingerprint)).toEqual(["subms", "ninety", "two-hundred"]);
  });

  it("latency_ms 0 without the measured flag stays unmeasured", () => {
    const zero = view({ fingerprint: "zero", latency_ms: 0 });
    const fifty = view({ fingerprint: "fifty", latency_ms: 50 });

    const ordered = orderForQuickConnect([zero, fifty], NOW);

    // Only the 50 ms candidate is measured — it orders first.
    expect(ordered.map((v) => v.fingerprint)).toEqual(["fifty", "zero"]);
  });

  it("pickerLatencyText renders < 1 ms only for measured sub-ms", () => {
    expect(pickerLatencyText(0, true)).toBe("< 1 ms");
    expect(pickerLatencyText(0)).toBe("—");
    expect(pickerLatencyText(82, true)).toBe("82 ms");
  });

  it("quickPickerRows carries the measured flag", () => {
    const rows = quickPickerRows(
      [view({ fingerprint: "subms", latency_ms: 0, latency_ms_measured: true })],
      NOW,
    );

    expect(rows).toHaveLength(1);
    expect(rows[0].measured).toBe(true);
    expect(rows[0].latencyMS).toBe(0);
  });
});
