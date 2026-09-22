import { useCallback, useEffect, useState } from "react";
import { Events as $events } from "@wailsio/runtime";
import { Browser } from "@wailsio/runtime";
import {
  appService,
  call,
  coreService,
  diagnosticsService,
  providerService,
} from "../services";
import type {
  CoreInstallProgress,
  CoreLifecycleView,
  CoreManifest,
  ProviderEndpointView,
  ProviderHealthView,
  ProviderInfoView,
} from "../services";
import { describeError, toast } from "../state/toastStore";
import { useProviderStore } from "../state/providerStore";
import { useQuickConnectStore } from "../state/quickConnectStore";
import { EmptyState, Menu, SkeletonPage } from "../components/common";
import { IconDots, IconDownload, IconPlay, IconRefresh, IconShield, IconStop } from "../components/Icons";
import { formatBytes, truncate } from "../utilities/format";

/**
 * CoresPage — the dedicated core-management tab (v0.9.0 §7): install,
 * update, verify, repair, rollback, disable. The lifecycle badge
 * mirrors the backend's eleven-state model and every broken state
 * carries a human-readable failure reason with a technical-details
 * expander plus recovery actions.
 */
export function CoresPage() {
  const [cores, setCores] = useState<CoreLifecycleView[] | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [progress, setProgress] = useState<Record<string, CoreInstallProgress>>({});
  const [expanded, setExpanded] = useState<string | null>(null);
  const load = useCallback(async () => {
    try {
      const list = await call(() => coreService.LifecycleInfo());
      setCores(list ?? []);
    } catch (error) {
      setCores([]);
      toast("error", "Could not load cores", describeError(error));
    }
  }, []);

  useEffect(() => {
    void load();

    // Live install progress: download bytes, smoke-test stage, done.
    const off = $events.On("freeiran:coreprogress", (event: any) => {
      const p = (Array.isArray(event?.data) ? event.data[0] : event?.data) as CoreInstallProgress;
      if (!p?.core) return;

      setProgress((prev) => ({ ...prev, [p.core]: p }));

      if (p.stage === "complete" || p.stage === "failed") {
        void load();
      }
    });

    return () => off?.();
  }, [load]);

  const run = async (name: string, action: string, fn: () => Promise<unknown>) => {
    setBusy(`${name}:${action}`);

    try {
      await fn();
      await load();
      // v0.9.8.7: a core install/update/remove changed which backends
      // candidates are connectable with — one bounded Quick Connect
      // refresh per availability change (meaningful invalidation).
      useQuickConnectStore.getState().invalidate();
    } catch (error) {
      toast("error", "Action failed", describeError(error));
    } finally {
      setBusy(null);
    }
  };

  if (cores === null) {
    return <SkeletonPage tiles={4} rows={4} />;
  }

  const installable = cores.filter((c) => c.manifest.state === "not_installed").length > 0;

  return (
    <div className="page-body">
      <div className="page-header">
        <div className="page-heading">
          <h1 className="page-title">Cores</h1>
          <div className="page-subtitle">
            Protocol cores are downloaded from their official upstream releases, verified by SHA-256,
            smoke-tested and only then activated. Never a bundled binary, never an unverified download.
          </div>
        </div>
        <div className="page-actions">
          {/* v0.9.13: aggregate actions over the EXISTING backend
              operations — CheckAllForUpdates and HealthCheckAll are
              already parallel and bounded on the engine side. */}
          <button
            type="button"
            className="btn ghost"
            disabled={busy !== null}
            onClick={() => void run("all", "check", () => call(() => coreService.CheckAllForUpdates()))}
          >
            <IconRefresh size={14} /> Check all
          </button>
          <button
            type="button"
            className="btn ghost"
            disabled={busy !== null}
            onClick={() => void run("all", "verify", () => call(() => coreService.HealthCheckAll()))}
          >
            <IconShield size={14} /> Verify all
          </button>
          <button
            type="button"
            className="btn ghost"
            disabled={busy !== null}
            onClick={() => void run("all", "update", () => call(() => coreService.UpdateAll()))}
          >
            <IconDownload size={14} /> Update all
          </button>
        </div>
      </div>

      {/* v0.9.10 education (§5 Cores + §5 Education): explain WHY a
          core is required before asking for the install — in one short
          paragraph, not documentation. */}
      {installable && (
        <div className="callout info">
          <strong>Why do I need a core?</strong> A configuration is just a
          recipe; a core is the engine that actually runs it and carries your
          traffic. Different protocols need different engines —{" "}
          <b>Xray</b> covers the widest range, so it is the best first install.
          Downloads come only from each project's official release page, are
          checksum-verified and smoke-tested before activation.
        </div>
      )}

      {cores.length === 0 ? (
        <EmptyState
          title="No cores installed yet"
          hint="Install Xray to get started — one click, verified download, ready in under a minute."
        />
      ) : (
        <div className="card-grid">
          {cores.map((view) => (
            <CoreCard
              key={view.manifest.name}
              view={view}
              progress={progress[view.manifest.name]}
              busy={busy}
              expanded={expanded === view.manifest.name}
              onToggleDetails={() =>
                setExpanded((cur) => (cur === view.manifest.name ? null : view.manifest.name))
              }
              onAction={(action, fn) => void run(view.manifest.name, action, fn)}
            />
          ))}
        </div>
      )}

      {/* v0.9.13: compact truthful runtime status — every value comes
          from a real engine surface (lifecycle manifests, AppState
          native-acceleration, the live memory/booster snapshot). No
          invented percentages, no "optimized" claims. */}
      <RuntimeSection cores={cores} />

      {/* v0.9.8.1 (§12/§13): Tor and Psiphon are first-class providers */}
      <ProvidersSection />
    </div>
  );
}

/**
 * RuntimeSection (v0.9.13): the honest runtime/performance picture.
 * Core counts come from the lifecycle manifests already loaded by the
 * page; native acceleration comes from AppState; memory pressure and
 * the adaptive booster come from the diagnostics Memory snapshot.
 * Both extra snapshots are ONE bounded call on mount — no polling.
 */
function RuntimeSection({ cores }: { cores: CoreLifecycleView[] }) {
  const [nativeAccel, setNativeAccel] = useState<string | null>(null);
  const [pressure, setPressure] = useState<string | null>(null);
  const [boosterWorkers, setBoosterWorkers] = useState<number | null>(null);

  useEffect(() => {
    let cancelled = false;

    void call(() => appService.State())
      .then((state) => {
        if (cancelled || !state) return;

        const fields = state as unknown as { native_acceleration?: string };

        setNativeAccel(fields.native_acceleration || null);
      })
      .catch(() => {
        /* best-effort status read */
      });

    void call(() => diagnosticsService.Memory())
      .then((snap) => {
        if (cancelled || !snap) return;

        const fields = snap as unknown as {
          pressure?: { state?: string };
          booster?: { queue_concurrency?: number };
        };

        setPressure(fields.pressure?.state || null);
        setBoosterWorkers(
          typeof fields.booster?.queue_concurrency === "number" ? fields.booster.queue_concurrency : null,
        );
      })
      .catch(() => {
        /* best-effort status read */
      });

    return () => {
      cancelled = true;
    };
  }, []);

  const installed = cores.filter((c) => c.manifest.state !== "not_installed");
  const ready = cores.filter(
    (c) => c.manifest.state === "ready" || c.manifest.state === "installed" || c.manifest.state === "update_available",
  );
  const updates = cores.filter((c) => c.manifest.state === "update_available");
  const healthy = cores.filter((c) => c.manifest.last_health_result?.ok === true);
  const healthChecked = cores.filter((c) => c.manifest.last_health_result != null);

  return (
    <section className="card runtime-status" aria-label="Runtime status">
      <h3 className="card-title">Runtime</h3>
      <dl className="kv runtime-grid">
        <dt>Cores</dt>
        <dd>
          {ready.length}/{installed.length} ready
          {updates.length > 0 ? ` · ${updates.length} update${updates.length === 1 ? "" : "s"} available` : ""}
        </dd>

        <dt>Health</dt>
        <dd>
          {healthChecked.length > 0
            ? `${healthy.length}/${healthChecked.length} passed the last smoke test`
            : "no smoke test run yet"}
        </dd>

        <dt>Native acceleration</dt>
        <dd>{nativeAccel ?? "—"}</dd>

        <dt>Memory pressure</dt>
        <dd>{pressure ?? "—"}</dd>

        <dt>Adaptive booster</dt>
        <dd>
          {boosterWorkers !== null
            ? `active · ${boosterWorkers} test worker${boosterWorkers === 1 ? "" : "s"}`
            : "—"}
        </dd>
      </dl>
    </section>
  );
}

function CoreCard({
  view,
  progress,
  busy,
  expanded,
  onToggleDetails,
  onAction,
}: {
  view: CoreLifecycleView;
  progress?: CoreInstallProgress;
  busy: string | null;
  expanded: boolean;
  onToggleDetails: () => void;
  onAction: (action: string, fn: () => Promise<unknown>) => void;
}) {
  const m = view.manifest;
  const isBusy = busy?.startsWith(`${m.name}:`) ?? false;
  const displayName = displayNameOf(m.name);

  // v0.9.13: the retained authoritative upstream snapshot (persisted
  // by the last update check — no re-query, no guessing). A zero size
  // means the size is genuinely unavailable; the fallback says so.
  const updateAvailable = m.state === "update_available" && !!m.latest_known;
  const updateSizeText =
    m.latest_asset_size && m.latest_asset_size > 0
      ? ` · ${formatBytes(m.latest_asset_size)} download`
      : " · Download size unavailable";

  // Secondary operations — reachable through the ⋮ overflow instead of
  // one button per operation on every card (§4).
  const overflowItems = [
    {
      id: "check",
      label: "Check update",
      disabled: isBusy,
      onSelect: () => onAction("check", () => call(() => coreService.CheckForUpdates(m.name))),
    },
    {
      id: "verify",
      label: "Verify",
      disabled: isBusy,
      onSelect: () => onAction("health", () => call(() => coreService.HealthCheck(m.name))),
    },
    {
      id: "repair",
      label: "Repair",
      disabled: isBusy,
      onSelect: () => onAction("repair", () => call(() => coreService.Repair(m.name))),
    },
    {
      id: "reinstall",
      label: "Reinstall",
      disabled: isBusy,
      onSelect: () => onAction("reinstall", () => call(() => coreService.Reinstall(m.name))),
    },
    {
      id: "rollback",
      label: "Roll back",
      disabled: isBusy || !m.previous_version,
      onSelect: () => onAction("rollback", () => call(() => coreService.Rollback(m.name))),
    },
    {
      id: "enable-disable",
      label: m.state === "disabled" ? "Enable" : "Disable",
      disabled: isBusy || (m.state !== "disabled" && !(m.state === "ready" || m.state === "installed")),
      onSelect: () =>
        m.state === "disabled"
          ? onAction("enable", () => call(() => coreService.Enable(m.name)))
          : onAction("disable", () => call(() => coreService.Disable(m.name))),
    },
    {
      id: "release-page",
      label: "Open official release page",
      disabled: !m.release_url,
      onSelect: () => {
        // The desktop runtime opens the URL in the user's browser
        // (window.open is unreliable inside the webview).
        void Browser.OpenURL(m.release_url).catch((error: unknown) => {
          toast("error", "Could not open browser", describeError(error));
        });
      },
    },
    {
      id: "uninstall",
      label: "Uninstall",
      danger: true,
      disabled: isBusy || !m.binary_path,
      onSelect: () => onAction("uninstall", () => call(() => coreService.Uninstall(m.name))),
    },
  ];

  return (
    <section className={`card core-card ${m.state === "broken" ? "card-broken" : ""}`}>
      <header className="card-head">
        <div>
          <h3>{displayName}</h3>
          <div className="core-meta muted">
            {m.version ? `v${stripV(m.version)}` : "not installed"}
            {/* v0.9.13: the version TRANSITION is the informative line
                when an update exists (v26.3.27 → v26.3.30). */}
            {updateAvailable && m.latest_known ? ` → v${stripV(m.latest_known)}` : ""}
            {/* v0.9.14: trust distinction — "verified" is claimed ONLY
                when the digest matched the authoritative upstream asset;
                external binaries without a digest match are "working",
                never "verified". */}
            {m.trust === "upstream-verified" ? " · verified" : m.trust === "locally-validated" ? " · working" : ""}
          </div>
        </div>
        <StateBadge state={m.state} progress={progress} />
      </header>

      {updateAvailable && (
        <div className="update-callout" role="status">
          <span className="update-size">
            Update available{updateSizeText}
          </span>
        </div>
      )}

      {isBusy && progress && (
        <div className="install-progress">
          <div className="install-stage">{stageLabel(progress.stage)}</div>
          {progress.message && progress.stage !== "complete" && progress.stage !== "failed" && (
            <div className="install-note">{progress.message}</div>
          )}
          {progress.stage === "downloading" && progress.bytes_total! > 0 && (
            <>
              <div className="meter" role="progressbar">
                <div
                  className="meter-fill"
                  style={{ width: `${Math.min(100, Math.round((progress.bytes_done! / progress.bytes_total!) * 100))}%` }}
                />
              </div>
              {downloadTelemetry(progress) && (
                <div className="install-telemetry">{downloadTelemetry(progress)}</div>
              )}
            </>
          )}
        </div>
      )}

      {m.state === "broken" && (view.failure_message || m.failure_reason) && (
        <div className="callout error">
          {view.failure_message || m.failure_reason}
          <button type="button" className="linklike" onClick={onToggleDetails}>
            {expanded ? "Hide technical details" : "Technical details"}
          </button>
          {expanded && (
            <pre className="tech-details">{technicalText(m)}</pre>
          )}
          <div className="toolbar wrap recovery">
            <RecoveryActions
              name={m.name}
              hasPrevious={!!m.previous_version}
              busy={busy}
              onAction={onAction}
            />
          </div>
        </div>
      )}

      {/* v0.9.14: honest non-failure status remark (e.g. "newer than
          stable; automatic downgrade refused"). */}
      {m.status_note && m.state !== "broken" && (
        <div className="update-callout" role="status">
          <span className="update-size">{m.status_note}</span>
        </div>
      )}

      <dl className="kv">
        {/* v0.9.14: provenance — where the active binary came from and
            who owns it. External installations are referenced, never
            modified. */}
        {view.origin && (
          <>
            <dt>Origin</dt>
            <dd>{originLabel(view.origin)}</dd>
          </>
        )}
        <dt>Channel</dt>
        <dd>{m.channel}</dd>
        <dt>Executable</dt>
        <dd className="mono ellipsis" title={view.path || m.binary_path}>
          {view.discovered ? view.path || m.binary_path : "—"}
        </dd>
        <dt>Last health check</dt>
        <dd>{m.last_health_check ? new Date(m.last_health_check).toLocaleString() : "never"}</dd>
      </dl>

      {/*
       * v0.9.13 footer: ONE essential action per state + the ⋮
       * overflow carrying every secondary operation (check/verify/
       * repair/reinstall/rollback/channel enable-disable/uninstall/
       * release page). The broken-state recovery block above keeps
       * its inline recovery actions.
       */}
      <footer className="card-actions">
        {(m.state === "not_installed" || m.state === "broken") && (
          <button
            type="button"
            className="btn primary"
            disabled={isBusy}
            onClick={() => onAction("install", () => call(() => coreService.Install(m.name)))}
          >
            <IconDownload size={14} /> {m.state === "broken" ? "Reinstall" : "Install"}
          </button>
        )}

        {updateAvailable && (
          <button
            type="button"
            className="btn primary"
            disabled={isBusy}
            onClick={() => onAction("install", () => call(() => coreService.Install(m.name)))}
          >
            <IconDownload size={14} /> Update to {m.latest_known ? stripV(m.latest_known) : "latest"}
          </button>
        )}

        {(m.state === "ready" || m.state === "installed") && (
          <button
            type="button"
            className="btn primary"
            disabled={isBusy}
            onClick={() => onAction("health", () => call(() => coreService.HealthCheck(m.name)))}
          >
            <IconShield size={14} /> Verify
          </button>
        )}

        {m.state === "disabled" && m.binary_path && (
          <button
            type="button"
            className="btn primary"
            disabled={isBusy}
            onClick={() => onAction("enable", () => call(() => coreService.Enable(m.name)))}
          >
            Enable
          </button>
        )}

        <Menu
          ariaLabel={`More actions for ${displayName}`}
          label={<IconDots size={15} />}
          items={overflowItems}
        />
      </footer>
    </section>
  );
}

function RecoveryActions({
  name,
  hasPrevious,
  busy,
  onAction,
}: {
  name: string;
  hasPrevious: boolean;
  busy: string | null;
  onAction: (action: string, fn: () => Promise<unknown>) => void;
}) {
  const isBusy = busy !== null;

  return (
    <>
      <button
        type="button"
        className="btn ghost"
        disabled={isBusy}
        onClick={() => onAction("repair", () => call(() => coreService.Repair(name)))}
      >
        Retry / Repair
      </button>
      {hasPrevious && (
        <button
          type="button"
          className="btn ghost"
          disabled={isBusy}
          onClick={() => onAction("rollback", () => call(() => coreService.Rollback(name)))}
        >
          Roll back
        </button>
      )}
      <button
        type="button"
        className="btn ghost"
        disabled={isBusy}
        onClick={() => onAction("reinstall", () => call(() => coreService.Reinstall(name)))}
      >
        Reinstall
      </button>
    </>
  );
}

// ---------------------------------------------------------------------------
// v0.9.8.1 Providers section (§12/§13): Tor and Psiphon — first-class
// providers with their own managed lifecycle (install → start →
// bootstrap → ready → stop), rendered below the protocol-core cards.
// ---------------------------------------------------------------------------

/**
 * Provider refresh cadence — ONLY used while a provider is actually
 * TRANSITIONING (installing / starting / stopping / bootstrapping).
 * v0.9.8.7: the previous always-on 5s poll is gone; stable provider
 * state changes only through user actions, which refresh explicitly.
 */
const PROVIDER_POLL_MS = 5000;

function ProvidersSection() {
  const providers = useProviderStore((state) => state.providers);
  const loaded = useProviderStore((state) => state.loaded);
  const load = useProviderStore((state) => state.load);
  const refresh = useProviderStore((state) => state.refresh);

  useEffect(() => {
    void load();
  }, [load]);

  // kind "core" providers (xray / v2ray / sing-box) are the managed
  // core cards above — only the first-class engines render here.
  const managed = providers.filter((info) => info.kind === "tor" || info.kind === "psiphon");

  // v0.9.8.7 — event-driven replacement for the always-on provider
  // poll: a bounded 5s refresh runs ONLY while some provider is in a
  // transitional state (install/start/stop/bootstrap in flight); the
  // timer stops itself once every provider reaches a stable state.
  // Stable-state changes always come from user actions, which call
  // refresh() explicitly (busy completion / onRefresh callbacks).
  const transitioning = managed.some(
    (info) =>
      info.bootstrap?.active === true ||
      info.state === "installing" ||
      info.state === "starting" ||
      info.state === "stopping",
  );

  useEffect(() => {
    if (!transitioning) return;

    const timer = window.setInterval(() => void refresh(), PROVIDER_POLL_MS);

    return () => window.clearInterval(timer);
  }, [transitioning, refresh]);

  return (
    <section aria-label="Providers">
      <div className="page-header">
        <div className="page-heading">
          <h2 className="page-title">Providers</h2>
          <div className="page-subtitle">
            Tor · Psiphon — first-class providers with managed, checksum-verified installation.
          </div>
        </div>
      </div>

      {managed.length === 0 ? (
        loaded ? (
          <EmptyState
            title="No providers reported"
            hint="Tor and Psiphon appear here once the provider engine lists them."
          />
        ) : null
      ) : (
        <div className="card-grid">
          {managed.map((info) => (
            <ProviderCard key={info.name} info={info} onRefresh={() => void refresh()} />
          ))}
        </div>
      )}
    </section>
  );
}

function ProviderCard({ info, onRefresh }: { info: ProviderInfoView; onRefresh: () => void }) {
  const [busyAction, setBusyAction] = useState<string | null>(null);
  const [health, setHealth] = useState<ProviderHealthView | null>(null);

  const isReady = info.state === "ready";
  const endpoints = info.endpoints ?? [];
  const capabilities = info.capabilities ?? [];

  // ONE bounded health measurement when the card mounts in the
  // running state and whenever it (re)enters it — a passive reading,
  // never a poll. Failures surface as an honest "—".
  useEffect(() => {
    if (!isReady) return;

    let cancelled = false;

    void call(() => providerService.Health(info.name))
      .then((view) => {
        if (!cancelled && view) setHealth(view);
      })
      .catch(() => {
        /* passive measurement — displayed as "—" */
      });

    return () => {
      cancelled = true;
    };
  }, [info.name, isReady]);

  const run = async (action: string, fn: () => Promise<unknown>) => {
    setBusyAction(action);

    try {
      await fn();
    } catch (error) {
      toast("error", "Provider action failed", describeError(error));
    } finally {
      setBusyAction(null);
      onRefresh();
    }
  };

  const busy = busyAction !== null;

  return (
    <section className="card" data-state={info.state}>
      <header className="card-head">
        <div>
          <h3>{providerDisplayName(info.name)}</h3>
          <div className="core-meta muted">
            {info.version ? `v${stripV(info.version)}` : "not installed"}
          </div>
        </div>
        <div>
          {info.bootstrap?.active && (
            <span className="chip">bootstrap {providerBootstrapPercent(info.bootstrap.progress)}%</span>
          )}{" "}
          <ProviderStateBadge state={info.state} />
        </div>
      </header>

      {info.failure_reason && <div className="callout error">{info.failure_reason}</div>}

      <dl className="kv">
        <dt>State</dt>
        <dd>{STATE_LABELS[info.state] ?? info.state}</dd>
        <dt>Runtime</dt>
        <dd>{info.runtime_state || "—"}</dd>
        <dt>Source</dt>
        <dd className="mono ellipsis" title={info.source || undefined}>
          {truncate(info.source || "—", 40)}
        </dd>
        <dt>License</dt>
        <dd>{info.license || "—"}</dd>
        <dt>Last check</dt>
        <dd>{providerLastCheckText(info.last_check)}</dd>
      </dl>

      {isReady && (
        <div>
          {endpoints.length > 0 && (
            <div className="provider-endpoint-list">
              {endpoints.map((endpoint) => (
                <div
                  className="provider-endpoint"
                  key={`${endpoint.network}-${endpoint.host}:${endpoint.port}`}
                >
                  {providerEndpointLabel(endpoint)}
                </div>
              ))}
            </div>
          )}
          <div className="provider-endpoint">endpoint latency: {providerHealthLatencyText(health)}</div>
          {capabilities.length > 0 && (
            <div>
              {capabilities.map((capability) => (
                <span className="provider-cap" key={capability}>
                  {capability}
                </span>
              ))}
            </div>
          )}
        </div>
      )}

      {info.notice && <p className="provider-notice">{info.notice}</p>}

      <footer className="card-actions">
        {!info.installed && (
          <button
            type="button"
            className="btn sm primary"
            disabled={busy}
            onClick={() => void run("install", () => call(() => providerService.Install(info.name)))}
          >
            {busyAction === "install" ? <span className="btn-spinner" /> : <IconDownload size={14} />} Install
          </button>
        )}

        {info.installed && (
          <>
            <button
              type="button"
              className="btn sm ghost"
              disabled={busy}
              onClick={() =>
                void run("verify", async () => {
                  // Verify re-measures and refreshes the card's latency row.
                  const view = await call(() => providerService.Health(info.name));
                  setHealth(view ?? null);
                })
              }
            >
              {busyAction === "verify" ? <span className="btn-spinner" /> : <IconShield size={14} />} Verify
            </button>

            {info.state !== "ready" && (
              <button
                type="button"
                className="btn sm"
                disabled={busy}
                onClick={() => void run("start", () => call(() => providerService.Start(info.name)))}
              >
                {busyAction === "start" ? <span className="btn-spinner" /> : <IconPlay size={14} />} Start
              </button>
            )}

            {info.state === "ready" && (
              <button
                type="button"
                className="btn sm ghost"
                disabled={busy}
                onClick={() => void run("stop", () => call(() => providerService.Stop(info.name)))}
              >
                {busyAction === "stop" ? <span className="btn-spinner" /> : <IconStop size={14} />} Stop
              </button>
            )}

            <button
              type="button"
              className="btn sm danger ghost"
              disabled={busy}
              onClick={() => void run("uninstall", () => call(() => providerService.Uninstall(info.name)))}
            >
              Uninstall
            </button>
          </>
        )}
      </footer>
    </section>
  );
}

/** Provider lifecycle badge — mirrors the core StateBadge contract. */
function ProviderStateBadge({ state }: { state: string }) {
  return <span className={`badge ${providerStateVariant(state)}`}>{STATE_LABELS[state] ?? state}</span>;
}

function providerStateVariant(state: string): string {
  switch (state) {
    case "ready":
    case "running":
      return "success";
    case "starting":
    case "installing":
    case "stopping":
      return "info";
    case "failed":
      return "error";
    case "installed":
      return "success-dim";
    default:
      // not_installed / disabled / unknown stay neutral.
      return "neutral";
  }
}

function providerDisplayName(name: string): string {
  switch (name) {
    case "tor":
      return "Tor";
    case "psiphon":
      return "Psiphon";
    default:
      return name.charAt(0).toUpperCase() + name.slice(1);
  }
}

/** Endpoints are discovered from the REAL runtime (§13), never invented. */
function providerEndpointLabel(endpoint: ProviderEndpointView): string {
  const address = `${endpoint.host}:${endpoint.port}`;
  const label =
    endpoint.network.toLowerCase() === "http" ? `HTTP proxy ${address}` : `SOCKS5 ${address}`;

  return endpoint.verified ? `${label} · verified` : label;
}

/** v0.9.8.1 latency semantics: measured 0 ms is sub-millisecond, not "unmeasured". */
function providerHealthLatencyText(health: ProviderHealthView | null): string {
  if (!health || health.measured !== true) return "—";

  return health.latency_ms && health.latency_ms > 0 ? `${Math.round(health.latency_ms)} ms` : "< 1 ms";
}

function providerBootstrapPercent(progress: number | undefined): number {
  return Math.max(0, Math.min(100, Math.round(progress ?? 0)));
}

/** Zero times (Go's time.Time{}) render as an honest "—". */
function providerLastCheckText(lastCheck: string | undefined): string {
  if (!lastCheck) return "—";

  const date = new Date(lastCheck);

  if (Number.isNaN(date.getTime()) || date.getFullYear() <= 1) return "—";

  return date.toLocaleString();
}

/**
 * v0.9.14: human labels for core provenance. "managed" renders as the
 * product's own installation; everything else marks an installation
 * FreeIran discovered and only references.
 */
function originLabel(origin?: string): string {
  switch (origin) {
    case "managed":
      return "FreeIran managed";
    case "path":
      return "system PATH";
    case "system":
      return "system";
    case "user":
      return "user-provided";
    default:
      return origin ?? "";
  }
}

const STATE_LABELS: Record<string, string> = {
  not_installed: "Not installed",
  installing: "Installing",
  installed: "Installed",
  checking: "Checking",
  ready: "Ready",
  broken: "Broken",
  disabled: "Disabled",
  update_available: "Update available",
  starting: "Starting",
  running: "Running",
  stopping: "Stopping",
  failed: "Failed",
};

function StateBadge({ state, progress }: { state: string; progress?: CoreInstallProgress }) {
  const variant =
    state === "ready" || state === "running"
      ? "success"
      : state === "broken" || state === "failed"
        ? "error"
        : state === "update_available"
          ? "warn"
          : "neutral";

  const label = state === "installing" && progress ? stageLabel(progress.stage) : STATE_LABELS[state] ?? state;

  return <span className={`badge ${variant}`}>{label}</span>;
}

function stageLabel(stage?: string): string {
  switch (stage) {
    // Unified lifecycle (v0.9.5): every stage is real work.
    case "resolving":
      return "Resolving release…";
    case "downloading":
      return "Downloading…";
    case "verifying":
      return "Verifying checksum…";
    case "unpacking":
      return "Unpacking…";
    case "validating":
      return "Validating…";
    case "activating":
      return "Activating…";
    case "complete":
      return "Installed";
    case "failed":
      return "Failed";
    default:
      return "Working…";
  }
}

/** Renders live download telemetry (real measurements only). */
function downloadTelemetry(p: CoreInstallProgress): string | null {
  const parts: string[] = [];

  if (p.bytes_done !== undefined && p.bytes_total) {
    parts.push(`${formatBytes(p.bytes_done)} / ${formatBytes(p.bytes_total)}`);
  }

  if (p.speed_bps && p.speed_bps > 0) {
    parts.push(`${formatBytes(p.speed_bps)}/s`);
  }

  if (p.eta_seconds !== undefined && p.eta_seconds > 0) {
    parts.push(`~${Math.ceil(p.eta_seconds)}s left`);
  }

  if (p.resumed_bytes) {
    parts.push(`resumed from ${formatBytes(p.resumed_bytes)}`);
  }

  if (p.retries) {
    parts.push(`${p.retries} retr${p.retries === 1 ? "y" : "ies"}`);
  }

  return parts.length > 0 ? parts.join(" · ") : null;
}

function displayNameOf(name: string): string {
  switch (name) {
    case "xray":
      return "Xray-core";
    case "v2ray":
      return "V2Ray (V2Fly)";
    case "sing-box":
      return "sing-box";
    default:
      return name;
  }
}

function stripV(v: string): string {
  return v.startsWith("v") ? v.slice(1) : v;
}

function technicalText(m: CoreManifest): string {
  const lines = [
    `state: ${m.state}`,
    `failure stage: ${m.failure_stage || "n/a"}`,
    `binary: ${m.binary_path || "n/a"}`,
    `checksum: ${m.checksum_sha256 || "n/a"}`,
    `release: ${m.release_tag || "n/a"}`,
  ];

  const h = m.last_health_result;

  if (h) {
    lines.push(
      `health: exe=${h.executable_exists} version=${h.version_query} config=${h.config_validate} launch=${h.smoke_launch} shutdown=${h.clean_shutdown}`,
    );

    if (h.details) lines.push(`details: ${h.details}`);
  }

  return lines.join("\n");
}
