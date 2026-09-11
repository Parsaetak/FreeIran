import { describe, expect, it } from "vitest";
import {
  formatBytes,
  formatClock,
  formatDuration,
  formatLatency,
  formatNumber,
  formatPercent,
  formatUptime,
  latencyClass,
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

describe("formatLatency", () => {
  it("renders a dash for untested values", () => {
    expect(formatLatency(undefined)).toBe("—");
    expect(formatLatency(null)).toBe("—");
    expect(formatLatency(0)).toBe("—");
    expect(formatLatency(Number.NaN)).toBe("—");
  });

  it("rounds milliseconds", () => {
    expect(formatLatency(120.6)).toBe("121 ms");
    expect(formatLatency(842)).toBe("842 ms");
  });
});

describe("latencyClass", () => {
  it("classifies thresholds", () => {
    expect(latencyClass(120)).toBe("ok");
    expect(latencyClass(299)).toBe("ok");
    expect(latencyClass(300)).toBe("warn");
    expect(latencyClass(799)).toBe("warn");
    expect(latencyClass(800)).toBe("err");
  });

  it("treats missing values as untested", () => {
    expect(latencyClass(undefined)).toBe("none");
    expect(latencyClass(0)).toBe("none");
    expect(latencyClass(-5)).toBe("none");
  });
});

describe("formatClock", () => {
  it("renders local wall-clock time from RFC3339", () => {
    const ts = new Date(2025, 0, 15, 9, 5, 3).getTime();

    expect(formatClock(new Date(ts).toISOString())).toBe("09:05:03");
  });

  it("passes through unparsable input", () => {
    expect(formatClock("not-a-time")).toBe("not-a-time");
    expect(formatClock("")).toBe("");
  });
});

describe("formatUptime", () => {
  it("formats rising magnitudes", () => {
    expect(formatUptime(0)).toBe("0s");
    expect(formatUptime(45_000)).toBe("45s");
    expect(formatUptime(127_000)).toBe("2m 07s");
    expect(formatUptime(3_600_000 * 2 + 60_000 * 5)).toBe("2h 05m");
    expect(formatUptime(86_400_000 * 3 + 3_600_000 * 4)).toBe("3d 4h");
  });
});
