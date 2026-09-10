import type { Config } from "../services";
import type { ExportRow } from "../types/ui";

export type { ExportRow };

/** Converts configs into export rows off the UI hot path. */
export function configToRow(config: Config): ExportRow {
  return {
    type: config["type"] as string,
    address: config["address"] as string,
    port: config["port"] as number,
    working: Boolean(config["working"]),
    latency_ms: Number(config["latency_ms"] ?? 0),
    source: String(config["source"] ?? ""),
  };
}
