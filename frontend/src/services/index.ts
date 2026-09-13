/**
 * Single import surface over the generated Wails bindings.
 *
 * The generated modules under /bindings mirror the Go services in
 * engine/app one-to-one; importing them through this module keeps the
 * rest of the UI insulated from binding paths and provides one place
 * to normalize backend errors.
 */
import * as appService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/appservice.js";
import * as sourceService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/sourceservice.js";
import * as dataService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/dataservice.js";
import * as storageService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/storageservice.js";
import * as diagnosticsService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/diagnosticsservice.js";
import * as connectionService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/connectionservice.js";
import * as logService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/logservice.js";
import * as settingsService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/settingsservice.js";
import * as coreService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/coreservice.js";
import * as testQueueService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/testqueueservice.js";
import * as tunnelService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/tunnelservice.js";
import * as loggingModels from "../../bindings/github.com/Parsaetak/FreeIran/internal/logging/models.js";

export {
  appService,
  sourceService,
  dataService,
  storageService,
  diagnosticsService,
  connectionService,
  logService,
  settingsService,
  coreService,
  testQueueService,
  tunnelService,
  loggingModels,
};

// Generated model types (synchronized with the Go backend by the
// wails3 generator — do not duplicate these by hand).
export type AppState = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").AppState;
export type CacheStats = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").CacheStats;
export type SourceView = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").SourceView;
export type ConfigPage = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").ConfigPage;
export type VerifyResult = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").VerifyResult;
export type Config = import("../../bindings/github.com/Parsaetak/FreeIran/engine/config/models.js").Config;
export type MetricsSnapshot = import("../../bindings/github.com/Parsaetak/FreeIran/engine/metrics/models.js").Snapshot;
export type StorageStats = import("../../bindings/github.com/Parsaetak/FreeIran/engine/store/models.js").Stats;
export type StorageDiagnostics = import("../../bindings/github.com/Parsaetak/FreeIran/engine/store/models.js").Diagnostics;
export type IngestionStats = import("../../bindings/github.com/Parsaetak/FreeIran/engine/pipeline/models.js").Stats;
export type SystemInfo = import("../../bindings/github.com/Parsaetak/FreeIran/system/models.js").Info;
export type CoreBinary = import("../../bindings/github.com/Parsaetak/FreeIran/system/models.js").CoreBinary;
export type ConnectionSnapshot = import("../../bindings/github.com/Parsaetak/FreeIran/engine/connection/models.js").Snapshot;
export type ConnectionAttempt = import("../../bindings/github.com/Parsaetak/FreeIran/engine/connection/models.js").Attempt;
export type BackendView = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").BackendView;
export type ConfigDetail = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").ConfigDetail;
export type CoreHealthReport = import("../../bindings/github.com/Parsaetak/FreeIran/engine/core/models.js").HealthReport;
export type LogEntry = import("../../bindings/github.com/Parsaetak/FreeIran/internal/logging/models.js").Entry;
export type LogFilter = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").LogFilter;
export type LogPage = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").LogPage;
export type Settings = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").Settings;

/**
 * Wraps a binding call so UI code receives a single, readable error.
 * The desktop runtime reports method failures as rejected promises.
 */
export async function call<T>(operation: () => Promise<T>): Promise<T> {
  try {
    return await operation();
  } catch (error) {
    const message =
      error instanceof Error ? error.message : String(error ?? "unknown error");

    throw new BackendError(message);
  }
}

/** Error surfaced to the UI when the backend call fails. */
export class BackendError extends Error {
  constructor(message: string) {
    super(message);

    this.name = "BackendError";
  }
}

// v0.8 Memory Booster 2.0 view types. These mirror the Go structs in
// engine/app/memoryservice.go / engine/booster / engine/mempressure
// (field-for-field). When the wails3 generator is next run on a GUI
// toolchain host, these can be swapped for generated models.
export type MemoryPressureState = "normal" | "elevated" | "high" | "critical";

export interface MemoryPressureSnapshot {
  state: MemoryPressureState;
  heap_alloc: number;
  heap_in_use: number;
  rss: number;
  gc_cpu_fraction: number;
  arena_bytes: number;
  cache_bytes: number;
  queue_bytes: number;
  pending_write: number;
  source_buffers: number;
  parser_buffers: number;
  usage_fraction: number;
  sampled_at: string;
}

export interface BoosterSettings {
  QueueConcurrency: number;
  IngestionConcurrency: number;
  ParserConcurrency: number;
  BatchSize: number;
  QueueDepth: number;
  CacheEntries: number;
  ChunkFlushBytes: number;
}

export interface MemorySnapshotView {
  pressure: MemoryPressureSnapshot;
  booster: BoosterSettings;
  queue_bytes: number;
  cache_bytes: number;
  pending_write_bytes: number;
  samples: number;
}

/** Test-queue statistics (engine/testqueue Stats). */
export interface QueueStatsView {
  queue_depth: number;
  active_workers: number;
  total_enqueued: number;
  total_completed: number;
  total_passed: number;
  total_failed: number;
  total_timed_out: number;
  total_cancelled: number;
  tests_per_sec: number;
  avg_duration_ms: number;
  per_backend: Record<string, number>;
  started_at: string;
}
