import { describe, expect, it } from "vitest";
import {
  applyConfigFilters,
  testStateFor,
} from "./configRowModel";

/**
 * v0.9.7 UI-model tests (§23): test-state feedback vocabulary and the
 * row/grid consistency rules behind the actions column.
 */

type Row = Record<string, unknown>;

describe("testStateFor", () => {
  it("labels live-queued rows as queued", () => {
    expect(
      testStateFor(
        { id: "fp-1", tested_at: 0 } as Row,
        false,
        new Set(["fp-1"]),
      ),
    ).toBe("queued");
  });

  it("labels in-flight rows as testing", () => {
    expect(testStateFor({ id: "fp-2" } as Row, true, new Set())).toBe("testing");
  });

  it("prefers testing over queued when both hold", () => {
    expect(
      testStateFor({ id: "fp-3" } as Row, true, new Set(["fp-3"])),
    ).toBe("testing");
  });

  it("returns none when the row is idle", () => {
    expect(testStateFor({ id: "fp-4" } as Row, false, new Set())).toBe("none");
  });
});

describe("applyConfigFilters", () => {
  const rows: Row[] = [
    { id: "a", name: "alpha", address: "a.example.com", port: 443, type: "vless" },
    { id: "b", name: "a-very-long-configuration-name-that-should-clamp-before-the-actions-column", address: "long.example.com", port: 8443, type: "vmess" },
    { id: "c", name: "gamma", address: "c.example.com", port: 443, type: "trojan" },
  ];

  it("filters by protocol", () => {
    expect(applyConfigFilters(rows, { protocol: "vless" }).length).toBe(1);
  });

  it("matches long names on search without error", () => {
    const hits = applyConfigFilters(rows, { query: "long.example.com" });
    expect(hits.length).toBe(1);
    expect(String(hits[0].id)).toBe("b");
  });

  it("never truncates the id used by the actions column", () => {
    const hits = applyConfigFilters(rows, { query: "" });
    for (const hit of hits) {
      expect(String(hit.id)).not.toContain("…");
    }
  });
});
