import { describe, expect, it } from "vitest";
import { levelAtLeast, levelRank, normalizeLevel } from "./logFilter";

/**
 * Mirrors the backend severity semantics (engine/app/loggingservice.go):
 * debug < info < warn < error, filters are "at or above", and an empty
 * filter accepts everything.
 */

describe("levelRank", () => {
  it("ranks severities in increasing order", () => {
    expect(levelRank("debug")).toBeLessThan(levelRank("info"));
    expect(levelRank("info")).toBeLessThan(levelRank("warn"));
    expect(levelRank("warn")).toBeLessThan(levelRank("error"));
  });

  it("treats unknown levels as info", () => {
    expect(levelRank("")).toBe(1);
    expect(levelRank("fatal")).toBe(1);
  });
});

describe("levelAtLeast", () => {
  it("passes entries at or above the minimum", () => {
    expect(levelAtLeast("warn", "warn")).toBe(true);
    expect(levelAtLeast("error", "warn")).toBe(true);
    expect(levelAtLeast("info", "warn")).toBe(false);
    expect(levelAtLeast("debug", "info")).toBe(false);
  });

  it("accepts everything with an empty filter", () => {
    expect(levelAtLeast("debug", "")).toBe(true);
    expect(levelAtLeast("error", "")).toBe(true);
  });

  it("tolerates unknown entry levels", () => {
    expect(levelAtLeast("trace", "debug")).toBe(true);
    expect(levelAtLeast("trace", "info")).toBe(true);
    expect(levelAtLeast("trace", "warn")).toBe(false);
  });
});

describe("normalizeLevel", () => {
  it("maps unknown levels onto info", () => {
    expect(normalizeLevel("warn")).toBe("warn");
    expect(normalizeLevel("")).toBe("info");
    expect(normalizeLevel("verbose")).toBe("info");
  });
});
