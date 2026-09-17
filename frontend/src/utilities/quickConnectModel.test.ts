import { describe, expect, it } from "vitest";
import type { CandidateView } from "../services";
import {
  FRESH_WINDOW_MS,
  candidateStatus,
  orderForQuickConnect,
  pickerLatencyText,
  pickerStatusClass,
  qualityLabel,
  quickPickerRows,
} from "./quickConnectModel";

/**
 * Quick Connect picker model contract (§7/§8/§11):
 * verified-usable first, then measured ping, untested last, dead
 * excluded — deterministic, honest about provenance.
 */

function view(overrides: Partial<CandidateView>): CandidateView {
  return {
    fingerprint: "fp",
    name: "candidate",
    protocol: "vless",
    endpoint: "example.com:443",
    class: "unknown",
    score: 0,
    latency_ms: 0,
    success_rate: 0,
    samples: 0,
    connectable: true,
    ...overrides,
  };
}

const NOW = 1_800_000_000_000;

describe("candidateStatus provenance", () => {
  it("labels candidates without samples untested", () => {
    expect(candidateStatus({ samples: 0, tested_at: NOW }, NOW)).toBe("untested");
    expect(candidateStatus({ samples: 3 }, NOW)).toBe("untested");
  });

  it("labels fresh measurements verified", () => {
    expect(candidateStatus({ samples: 2, tested_at: NOW - 1000 }, NOW)).toBe("verified");
  });

  it("labels old measurements stale", () => {
    expect(
      candidateStatus({ samples: 2, tested_at: NOW - FRESH_WINDOW_MS - 1 }, NOW),
    ).toBe("stale");
  });
});

describe("orderForQuickConnect", () => {
  it("puts a verified usable candidate above an untested one even when the untested one would look faster", () => {
    const verified = view({
      fingerprint: "verified",
      class: "best",
      samples: 4,
      tested_at: NOW - 1000,
      latency_ms: 120,
      success_rate: 1,
      score: 0.9,
    });
    const untested = view({
      fingerprint: "untested",
      class: "unknown",
      latency_ms: 1, // theoretical-only value must not win
      score: 0.5,
    });

    const ordered = orderForQuickConnect([untested, verified], NOW);

    expect(ordered.map((v) => v.fingerprint)).toEqual(["verified", "untested"]);
  });

  it("orders verified candidates by measured ping ascending", () => {
    const slow = view({
      fingerprint: "slow",
      class: "good",
      samples: 2,
      tested_at: NOW - 1000,
      latency_ms: 200,
      success_rate: 1,
    });
    const fast = view({
      fingerprint: "fast",
      class: "good",
      samples: 2,
      tested_at: NOW - 1000,
      latency_ms: 80,
      success_rate: 1,
    });

    const ordered = orderForQuickConnect([slow, fast], NOW);

    expect(ordered.map((v) => v.fingerprint)).toEqual(["fast", "slow"]);
  });

  it("drops dead and unconnectable candidates entirely", () => {
    const dead = view({ fingerprint: "dead", class: "dead", connectable: false, samples: 3, tested_at: NOW });
    const blocked = view({ fingerprint: "blocked", class: "best", connectable: false, samples: 3, tested_at: NOW });
    const alive = view({ fingerprint: "alive", class: "good", samples: 3, tested_at: NOW, latency_ms: 90 });

    const ordered = orderForQuickConnect([dead, blocked, alive], NOW);

    expect(ordered.map((v) => v.fingerprint)).toEqual(["alive"]);
  });

  it("breaks ties deterministically by fingerprint", () => {
    const a = view({ fingerprint: "b", class: "good", samples: 2, tested_at: NOW, latency_ms: 100, success_rate: 1 });
    const b = view({ fingerprint: "a", class: "good", samples: 2, tested_at: NOW, latency_ms: 100, success_rate: 1 });

    const ordered = orderForQuickConnect([a, b], NOW);

    expect(ordered.map((v) => v.fingerprint)).toEqual(["a", "b"]);
  });

  it("orders untested candidates by score descending after verified ones", () => {
    const verified = view({ fingerprint: "v", class: "best", samples: 2, tested_at: NOW, latency_ms: 300 });
    const untestedHigh = view({ fingerprint: "uh", class: "unknown", score: 0.4 });
    const untestedLow = view({ fingerprint: "ul", class: "unknown", score: 0.1 });

    const ordered = orderForQuickConnect([untestedLow, verified, untestedHigh], NOW);

    expect(ordered.map((v) => v.fingerprint)).toEqual(["v", "uh", "ul"]);
  });

  it("places candidates with a measured ping above verified candidates without one", () => {
    const pinged = view({ fingerprint: "pinged", class: "good", samples: 2, tested_at: NOW, latency_ms: 250 });
    const urlOnly = view({
      fingerprint: "url",
      class: "good",
      samples: 2,
      tested_at: NOW,
      latency_ms: 0,
      success_rate: 1,
    });

    const ordered = orderForQuickConnect([urlOnly, pinged], NOW);

    expect(ordered.map((v) => v.fingerprint)).toEqual(["pinged", "url"]);
  });

  it("never mutates the input array", () => {
    const input = [
      view({ fingerprint: "b", class: "good", samples: 2, tested_at: NOW, latency_ms: 200 }),
      view({ fingerprint: "a", class: "good", samples: 2, tested_at: NOW, latency_ms: 100 }),
    ];
    const snapshot = [...input];

    orderForQuickConnect(input, NOW);

    expect(input).toEqual(snapshot);
  });
});

describe("quickPickerRows", () => {
  it("exposes only redacted display data with honest latency text", () => {
    const rows = quickPickerRows(
      [
        view({ fingerprint: "fp1", name: "Warsaw edge", protocol: "VLESS", samples: 2, tested_at: NOW, latency_ms: 82 }),
        view({ fingerprint: "fp2", name: "No data", samples: 0 }),
      ],
      NOW,
    );

    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatchObject({
      fingerprint: "fp1",
      name: "Warsaw edge",
      protocol: "vless",
      latencyMS: 82,
      status: "verified",
    });
    expect(rows[1]).toMatchObject({ latencyMS: 0, status: "untested" });
  });

  it("caps the rendered rows at the picker limit", () => {
    const many = Array.from({ length: 60 }, (_, i) =>
      view({ fingerprint: `fp${i}`, name: `c${i}`, class: "good", samples: 1, tested_at: NOW, latency_ms: 50 + i }),
    );

    expect(quickPickerRows(many, NOW)).toHaveLength(25);
  });
});

describe("picker cell helpers", () => {
  it("renders an honest dash for unmeasured latency", () => {
    expect(pickerLatencyText(0)).toBe("—");
    expect(pickerLatencyText(82.4)).toBe("82 ms");
  });

  it("maps statuses to chip classes", () => {
    expect(pickerStatusClass("verified")).toBe("success");
    expect(pickerStatusClass("stale")).toBe("warn");
    expect(pickerStatusClass("untested")).toBe("neutral");
  });

  it("maps quality classes to human labels", () => {
    expect(qualityLabel("best")).toBe("Excellent");
    expect(qualityLabel("dead")).toBe("Down");
    expect(qualityLabel("anything")).toBe("Untested");
  });
});
