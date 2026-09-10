import { describe, expect, it } from "vitest";
import {
  formatBytes,
  formatDuration,
  formatNumber,
  formatPercent,
  truncate,
  relativeTime,
} from "./format";

describe("formatBytes", () => {
  it("formats byte sizes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(2048)).toBe("2.0 KiB");
    expect(formatBytes(4 * 1024 * 1024)).toBe("4.0 MiB");
  });

  it("tolerates invalid input", () => {
    expect(formatBytes(-5)).toBe("0 B");
    expect(formatBytes(Number.NaN)).toBe("0 B");
  });
});

describe("formatDuration", () => {
  it("formats milliseconds", () => {
    expect(formatDuration(0)).toBe("0 ms");
    expect(formatDuration(250)).toBe("250 ms");
    expect(formatDuration(1500)).toBe("1.5 s");
    expect(formatDuration(90_000)).toBe("1m 30s");
  });
});

describe("formatNumber", () => {
  it("groups digits", () => {
    expect(formatNumber(1_000_000)).toBe("1,000,000");
  });
});

describe("formatPercent", () => {
  it("formats rates", () => {
    expect(formatPercent(0.75)).toBe("75.0%");
    expect(formatPercent(1)).toBe("100.0%");
  });
});

describe("truncate", () => {
  it("shortens long strings", () => {
    expect(truncate("abcdefghij", 5)).toHaveLength(5);
    expect(truncate("abc", 5)).toBe("abc");
  });
});

describe("relativeTime", () => {
  it("describes recent times", () => {
    expect(relativeTime(0)).toBe("never");
    expect(relativeTime(Date.now() - 5_000)).toBe("just now");
    expect(relativeTime(Date.now() - 120_000)).toBe("2m ago");
  });
});
