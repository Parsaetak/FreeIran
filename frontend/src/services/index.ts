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
import * as networkService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/networkservice.js";
import * as discoveryService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/discoveryservice.js";
// v0.9.8.1: first-class provider surface (§12/§13) + Internet-Tools
// engine (§6). Hand-maintained ByName bindings (same pattern the
// v0.9.3 methods used until the next generator run).
import * as providerService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/providerservice.js";
import * as toolsService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/internettoolsservice.js";
// v0.9.10: favorites, user groups and the evidence-based source
// reliability dashboard (machine-generated bindings).
import * as collectionService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/collectionservice.js";
// v0.9.11: Connection Profiles — hand-maintained ByName binding (the
// wails3 generator cannot run on the current host; the contract test
// verifies every method name against the Go service).
import * as profileService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/profileservice.js";
// v0.10.2: personal configuration import (hand-maintained ByName
// binding, same contract-test discipline).
import * as importService from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/importservice.js";
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
  networkService,
  discoveryService,
  providerService,
  toolsService,
  collectionService,
  profileService,
  importService,
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
export type ProfileView = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").ProfileView;
export type ProfileSpec = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").ProfileSpec;
export type ImportPreview = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").ImportPreview;
export type ImportResult = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").ImportResult;
export type ImportedConfigView = import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").ImportedConfigView;

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
  avg_latency_ms?: number;
  fastest_latency_ms?: number;
  slowest_latency_ms?: number;
  /** v0.9.7: temporary core processes alive right now (bounded pool). */
  active_cores?: number;
  /** v0.9.7: effective core-probe cap. */
  core_probe_concurrency?: number;
}

/**
 * v0.9.15: the ONE authoritative, COMPLETE queue-state view
 * (TestQueueService.LiveState and the freeiran:queuestate event — one
 * shape everywhere). `fingerprints` is the full live set (pending +
 * in-flight), NOT a bounded page: absence from it is a real terminal
 * transition, which is the completeness contract the false-completion
 * fix is built on.
 */
export interface QueueLiveStateView {
  version: number;
  fingerprints: string[];
  stats: QueueStatsView;
  paused: boolean;
}

// ---------------------------------------------------------------------------
// v0.9.0 view types
// ---------------------------------------------------------------------------

/** One netcheck probe outcome (engine/netcheck CheckResult). */
export interface NetCheckResult {
  name: string;
  target: string;
  ok: boolean;
  latency_ms?: number;
  error?: string;
}

/**
 * One staged-diagnostic rung (v0.9.8.5 §4): local link → local IP →
 * DNS → TCP → TLS → HTTPS → captive portal → direct Internet →
 * tunnel Internet. Additive evidence that explains the verdict — it
 * never replaces it.
 */
export interface NetCheckStageView {
  stage: string;
  status: "ok" | "failed" | "skipped" | "not_checked";
  latency_ms?: number;
  measured?: boolean;
  target?: string;
  detail?: string;
  failure_class?: string;
}

/** The classified connectivity report (engine/netcheck Report). */
export interface NetCheckReport {
  state: string;
  summary: string;
  checked_at: string;
  duration_ms: number;
  latency_ms?: number;
  target_used?: string;
  local_links: NetCheckResult[];
  dns: NetCheckResult[];
  tcp: NetCheckResult[];
  https: NetCheckResult[];
  proxy?: NetCheckResult | null;
  cancelled: boolean;
  target_count: number;
  /** v0.9.8.5: the ordered ladder evidence + the first failed rung. */
  stages?: NetCheckStageView[];
  failed_stage?: string;
}

/** Managed core manifest (engine/coremgr Manifest). */
export interface CoreManifest {
  name: string;
  state: string;
  version: string;
  channel: string;
  binary_path: string;
  checksum_sha256: string;
  release_tag: string;
  release_url: string;
  installed_at: string;
  last_checked: string;
  last_health_check: string;
  last_health_result: {
    ok: boolean;
    executable_exists: boolean;
    version_query: boolean;
    config_validate: boolean;
    smoke_launch: boolean;
    clean_shutdown: boolean;
    details?: string;
  };
  previous_version?: string;
  failure_reason?: string;
  failure_stage?: string;
  latest_known?: string;
  /** v0.9.13: release tag of the retained upstream snapshot. */
  latest_tag?: string;
  /**
   * v0.9.13: retained platform asset download size in bytes.
   * Absent/0 = unavailable — the UI shows a truthful fallback,
   * never a guessed value.
   */
  latest_asset_size?: number;
  /** v0.9.14: "managed" | "external" — external binaries are only referenced. */
  ownership?: string;
  /** v0.9.14: where the active binary came from (managed|path|system|user). */
  origin?: string;
  /** v0.9.14: canonical path of a referenced external executable. */
  external_path?: string;
  /** v0.9.14: "upstream-verified" | "locally-validated". */
  trust?: string;
  /** v0.9.14: honest non-failure status remark (e.g. "newer than stable"). */
  status_note?: string;
  /** v0.9.14: reuse decision of the last install call. */
  last_decision?: string;
}

/** The complete core lifecycle view (app.CoreLifecycleView). */
export interface CoreLifecycleView {
  manifest: CoreManifest;
  discovered: boolean;
  runtime_state: string;
  runtime_version?: string;
  path?: string;
  failure_message?: string;
  /** v0.9.14: discovery provenance of the active binary. */
  origin?: string;
  /** v0.9.14: "managed" | "external". */
  ownership?: string;
  /** v0.9.14: honest non-failure status remark. */
  status_note?: string;
  /** v0.9.14: reuse decision of the last install call. */
  last_decision?: string;
}

/**
 * Install progress event (coremgr.InstallProgress). The stage is the
 * unified lifecycle: resolving | downloading | verifying | unpacking |
 * validating | activating | complete | failed — every stage reflects
 * real work; telemetry (bytes/speed/ETA/retries/resumed) is populated
 * from the downloader's actual measurements, never fabricated.
 */
export interface CoreInstallProgress {
  core: string;
  stage: string;
  message?: string;
  bytes_done?: number;
  bytes_total?: number;
  /** Measured download throughput in bytes/second. */
  speed_bps?: number;
  /** Estimated seconds remaining (-1 = unknown). */
  eta_seconds?: number;
  /** Download resume attempts so far. */
  retries?: number;
  /** Byte offset a resumed download continued from. */
  resumed_bytes?: number;
  at: string;
}

/** Sanitized diagnostic report (app.DiagnosticReport). */
export interface DiagnosticReportView {
  generated_at: string;
  version: string;
  platform: string;
  app_status: string;
  config_count: number;
  cores?: string[];
  storage: string;
  network_state: string;
  network_note?: string;
  connection: string;
  warnings?: string[];
  technical?: string[];
}

/** Developer/build information snapshot (app.DeveloperInfoView). */
export interface DeveloperInfoView {
  version: string;
  commit: string;
  go_version: string;
  platform: string;
  user_agent: string;
  base_dir: string;
  data_dir: string;
  logs_dir: string;
  cores_dir: string;
  runtime_dir?: string;
  portable_mode: boolean;
  workspace_writable?: boolean;
  migration?: WorkspaceStatus;
  native_acceleration: string;
  queue_workers_override: number;
  net_timeout_override_seconds: number;
  queue_depth: number;
  active_workers: number;
  total_enqueued: number;
  total_passed: number;
  total_failed: number;
}

// ---------------------------------------------------------------------------
// v0.9.2 view types (workspace / storage / cleanup)
// ---------------------------------------------------------------------------

/** One-time workspace migration record (system.WorkspaceStatus). */
export interface WorkspaceStatus {
  version: number;
  migrated: boolean;
  source?: string;
  migrated_at?: string;
  files?: number;
  bytes?: number;
  skipped?: string;
}

/** One task of the last cleanup pass (app.LastCleanupTask). */
export interface CleanupTaskView {
  name: string;
  bytes: number;
  items: number;
  error?: string;
  status: string;
}

/** Storage & workspace overview (app.StorageOverview). */
export interface StorageOverviewView {
  workspace_path: string;
  workspace_writable: boolean;
  portable_mode: boolean;
  data_bytes: number;
  chunk_bytes: number;
  wal_bytes: number;
  cache_bytes: number;
  logs_bytes: number;
  core_bytes: number;
  runtime_bytes: number;
  total_bytes: number;
  records: number;
  disk_bytes: number;
  chunk_count: number;
  garbage_ratio: number;
  heap_alloc_bytes: number;
  heap_sys_bytes: number;
  rss_bytes: number;
  pressure_state: string;
  usage_fraction: number;
  memtable_records: number;
  memtable_bytes: number;
  chunk_target_bytes: number;
  cleanup_passes: number;
  total_reclaimed_bytes: number;
  last_cleanup_bytes: number;
  last_cleanup_at?: string;
  last_cleanup_tasks?: CleanupTaskView[];
  migration: WorkspaceStatus;
}

/** CleanupNow outcome (app.CleanupResult). */
export interface CleanupResultView {
  ok: boolean;
  rate_limited?: boolean;
  bytes: number;
  duration_ms: number;
  tasks?: CleanupTaskView[];
}

// ---------------------------------------------------------------------------
// v0.9.3 view types (autonomous connection engine)
// ---------------------------------------------------------------------------

/** One ranked candidate (app.CandidateView, credential-free). */
export interface CandidateView {
  fingerprint: string;
  name: string;
  protocol: string;
  endpoint: string;
  class: string;
  score: number;
  latency_ms: number;
  /**
   * v0.9.8.1 measurement flag: latency_ms is a REAL measurement.
   * latency_ms == 0 with this flag true means a measured
   * sub-millisecond median — the fastest band — never "unmeasured".
   */
  latency_ms_measured?: boolean;
  success_rate: number;
  samples: number;
  tested_at?: number;
  connectable: boolean;
  explanation?: string[];
}

// ---- v0.9.8.1 provider surface (§12/§13) ----------------------------

/** One provider choice row (auto mode). */
export interface ProviderChoiceView {
  kind: string;
  name: string;
  score: number;
  reasons?: string[];
}

/** One provider mode option. */
export interface ProviderModeView {
  mode: string;
  label: string;
}

/** Local proxy endpoint discovered from the real runtime. */
export interface ProviderEndpointView {
  network: string;
  host: string;
  port: number;
  verified?: boolean;
}

/** Live bootstrap progress from the provider's ACTUAL state. */
export interface ProviderBootstrapView {
  active?: boolean;
  progress?: number;
  tag?: string;
  complete?: boolean;
  updated_at?: string;
}

/** Provider Info (credential-free, §13). */
export interface ProviderInfoView {
  name: string;
  kind: string;
  installed: boolean;
  version?: string;
  state: string;
  runtime_state?: string;
  source?: string;
  /** managed-release | user-binary | external-reference (v0.9.15 honest acquisition states). */
  acquisition?: string;
  license?: string;
  notice?: string;
  last_check?: string;
  endpoints?: ProviderEndpointView[];
  capabilities?: string[];
  failure_reason?: string;
  bootstrap?: ProviderBootstrapView;
}

/** Measured provider health. */
export interface ProviderHealthView {
  ok: boolean;
  process_alive: boolean;
  listener_ready: boolean;
  latency_ms?: number;
  measured?: boolean;
  details?: string;
  checked_at: string;
}

// ---- v0.9.8.1 Internet-Tools surface (§6) ---------------------------

/** One catalogue entry. */
export interface ToolInfoView {
  id: string;
  label: string;
  group: string;
  takes_target: boolean;
  timeout_ms: number;
}

/** Structured tool measurement (v0.9.8.1 latency semantics). */
export interface ToolMeasurementView {
  latency_ms?: number;
  measured?: boolean;
  sub_ms?: boolean;
  status?: number;
  bytes?: number;
  addresses?: string[];
  address_count?: number;
  tls_version?: string;
  cipher?: string;
  hop_count?: number;
  hops?: string[];
  mtu_bytes?: number;
  exit_ip_direct?: string;
  exit_ip_tunnel?: string;
  exit_ip_match?: boolean;
  captive_detected?: boolean;
  captive_redirect?: string;
  probes?: number;
}

/** Live tunnel truth (never fabricated). */
export interface TunnelSnapshotView {
  active: boolean;
  provider?: string;
  endpoint?: string;
  healthy?: boolean;
  latency_ms?: number;
  details?: string;
}

/** The structured outcome of one tool run. */
export interface ToolResultView {
  tool_id: string;
  target?: string;
  started_at: string;
  finished_at: string;
  duration_ms: number;
  status: string;
  transport?: string;
  path?: string;
  provider?: string;
  measurement?: ToolMeasurementView;
  error?: string;
  details?: Record<string, string>;
  /** v0.9.8.5 (§5): the structured DNS-diagnostic evidence. */
  dns?: DNSDiagnosticReportView;
}

/** One tool run request (user-triggered only). */
/** v0.9.8.7: the truthful generated wire shape (ToolRunRequest's twin). */
export type ToolRequestView = import(
  "../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js"
).ToolRequestView;

export interface ToolRunRequest {
  tool: string;
  target?: string;
  timeout_ms?: number;
  tunneled?: boolean;
}

// ---------------------------------------------------------------------------
// v0.9.8.5 DNS diagnostic views (§5) — per-resolver rows with
// per-record-type evidence, transports and honest failure classes.
// ---------------------------------------------------------------------------

/** One (resolver, record type) row of DNS evidence. */
export interface DNSQueryView {
  resolver: string;
  address?: string;
  transport: string; // system | udp | tcp | none
  query_name: string;
  record_type: string; // A | AAAA
  ok: boolean;
  status?: number; // DNS RCODE
  latency_ms?: number;
  measured?: boolean;
  answer_count?: number;
  addresses?: string[];
  failure_class?: string;
  error?: string;
  at: string;
}

/** One resolver's full row (A + AAAA). */
export interface DNSResolverResultView {
  resolver: string;
  address?: string;
  transport?: string;
  ok: boolean;
  queries: DNSQueryView[];
  latency_ms?: number;
}

/** The structured DNS diagnostic report (v0.9.8.5 §5). */
export interface DNSDiagnosticReportView {
  name: string;
  resolvers: DNSResolverResultView[];
  started_at: string;
  duration_ms: number;
  cancelled?: boolean;
}

// ---------------------------------------------------------------------------
// v0.9.8.5 Network Identity views (§6) — local IP, public IP and
// ISP/ASN metadata, all measured on explicit user action only.
// ---------------------------------------------------------------------------

/** One active local address with its interface. */
export interface LocalAddressView {
  address: string;
  interface?: string;
  family: string; // ipv4 | ipv6
}

/** The measured local network identity (no traffic leaves the machine). */
export interface LocalIdentityView {
  primary_ipv4?: string;
  primary_ipv6?: string;
  interface?: string;
  others?: LocalAddressView[];
  measured: boolean;
}

/** The measured public (exit) identity. */
export interface PublicIdentityView {
  direct_ip?: string;
  tunnel_ip?: string;
  tunnel_match?: boolean;
  endpoint?: string;
}

/** ISP / network organization metadata (unavailable → available: false). */
export interface NetworkMetadataView {
  organization?: string;
  asn?: string;
  country?: string;
  region?: string;
  source?: string;
  available: boolean;
}

/** The complete Network Identity result (v0.9.8.5 §6). */
export interface IdentityReportView {
  local: LocalIdentityView;
  public: PublicIdentityView;
  metadata: NetworkMetadataView;
  path: string; // direct | tunneled
  provider?: string;
  checked_at: string;
  duration_ms: number;
  cancelled?: boolean;
  error?: string;
}

/** ConnectBest outcome (app.ConnectBestResult). */
export interface ConnectBestResultView {
  snapshot: ConnectionSnapshot;
  chosen: CandidateView;
  candidates: number;
}

/**
 * Splits the backend's humanized error format ("readable sentence\n---\nTechnical details: raw")
 * into its user-facing parts.
 */
export function parseHumanizedError(message: string): { readable: string; technical: string } {
  const marker = "\n---\nTechnical details: ";
  const idx = message.indexOf(marker);

  if (idx >= 0) {
    return {
      readable: message.slice(0, idx),
      technical: message.slice(idx + marker.length),
    };
  }

  return { readable: message, technical: "" };
}
