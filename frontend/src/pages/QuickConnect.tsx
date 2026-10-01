import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useConnectionStore } from "../state/connectionStore";
import { useQuickConnectStore } from "../state/quickConnectStore";
import { useStartFlowStore } from "../state/startflowStore";
import { useSettingsStore, effectiveReducedMotion } from "../state/settingsStore";
import { useConnectModeStore, CONNECT_MODE_LABELS, type ConnectMode } from "../state/connectModeStore";
import { useChainStore } from "../state/chainStore";
import { useProfilesStore, PROFILE_MODE_LABELS, normalizeProfileMode, type ProfileMode } from "../state/profilesStore";
import { call, connectionService, tunnelService, type ProfileSpec } from "../services";
import * as tunnelOwnership from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/tunnelservice.js";
import { describeError, toast } from "../state/toastStore";
import { ProfileSpec as ProfileSpecModel } from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js";
import type { Page } from "../types/ui";
import { formatLatency, truncate } from "../utilities/format";
import {
  quickPickerRows,
  pickerLatencyText,
  pickerStatusClass,
  selectionReason,
  type QuickCandidateRow,
} from "../utilities/quickConnectModel";
import { humanizeConnectionError, EDUCATION_HINTS } from "../utilities/connectionErrors";
import { IconCheck, IconChevronDown, IconRefresh, IconZap } from "../components/Icons";

/**
 * Quick Connect (v0.9.8) — the application's home connection surface.
 *
 * This page is a thin UX layer over the EXISTING connection engine:
 * every action routes through useConnectionStore (connect /
 * connectBest) or the v0.9.6 adaptive start flow; the backend state
 * machine remains the single source of truth. Nothing here duplicates
 * the engine — no new connection logic, no fake states, no invented
 * metrics.
 *
 * One primary action only: CONNECT (which becomes the CONNECTED state
 * representation while a session is live). Detailed diagnostics stay
 * on the Connection and Diagnostics pages.
 */

/** Hero states rendered by this page (derived from real backend state). */
type QuickHeroState =
  | "ready"
  | "preparing"
  | "connecting"
  | "verifying"
  | "connected"
  | "failed"
  | "disconnecting";

/** Start-flow stages that happen BEFORE a connection is attempted. */
const FLOW_PREPARING_STAGES = new Set(["detecting", "discovering", "testing", "ranking"]);

/**
 * v0.9.7 connection-snapshot fields that the generated binding does
 * not carry yet; read structurally (same pattern as appStore).
 */
interface SnapshotExtras {
  verification?: string;
  ping_median_ms?: number;
}

const HEADLINES: Record<QuickHeroState, string> = {
  ready: "Ready to connect",
  preparing: "Preparing connection",
  connecting: "Connecting",
  verifying: "Verifying",
  connected: "Connected",
  failed: "Connection failed",
  disconnecting: "Disconnecting",
};

const BUTTON_LABELS: Record<QuickHeroState, string> = {
  ready: "CONNECT",
  preparing: "PREPARING…",
  connecting: "CONNECTING…",
  verifying: "VERIFYING…",
  connected: "CONNECTED",
  failed: "CONNECT",
  disconnecting: "DISCONNECTING…",
};

export function QuickConnectPage({ onNavigate }: { onNavigate: (page: Page) => void }) {
  // Selective subscriptions only (§29): primitives and stable refs,
  // never whole-store objects.
  const snapshot = useConnectionStore((state) => state.snapshot);
  const busy = useConnectionStore((state) => state.busy);
  const connectError = useConnectionStore((state) => state.error);
  const connect = useConnectionStore((state) => state.connect);
  const connectBest = useConnectionStore((state) => state.connectBest);

  const flowStage = useStartFlowStore((state) => state.status?.stage ?? "idle");
  const flowRunning = useStartFlowStore((state) => state.status?.running ?? false);
  const flowMessage = useStartFlowStore((state) => state.status?.message ?? "");

  const candidates = useQuickConnectStore((state) => state.candidates);
  const candidatesLoading = useQuickConnectStore((state) => state.loading);
  const candidatesLoaded = useQuickConnectStore((state) => state.loaded);
  const selected = useQuickConnectStore((state) => state.selected);
  const select = useQuickConnectStore((state) => state.select);
  const loadCandidates = useQuickConnectStore((state) => state.load);

  // v0.12.2: the Quick Connect route choice (Auto / Configurations /
  // Proxy Chains) — the surviving surface of the removed provider
  // modes. Loaded once per mount.
  const connectMode = useConnectModeStore((state) => state.mode);
  const modeLoaded = useConnectModeStore((state) => state.loaded);
  const modeLoading = useConnectModeStore((state) => state.loading);
  const setConnectMode = useConnectModeStore((state) => state.setMode);
  const loadConnectMode = useConnectModeStore((state) => state.load);

  // Chains load once per mount (list only; details load in the
  // editor and the chain scope).
  const chains = useChainStore((state) => state.chains);
  const chainsLoaded = useChainStore((state) => state.loaded);
  const chainsLoading = useChainStore((state) => state.loading);
  const selectedChain = useChainStore((state) => state.selected);
  const selectChain = useChainStore((state) => state.select);
  const loadChains = useChainStore((state) => state.load);

  /**
   * v0.12.2: the compact evidence line for the explicitly selected
   * candidate. Derived ONLY from recorded observations.
   */
  const selectedReason = useMemo(() => {
    const rows = quickPickerRows(candidates);
    const chosen = rows.find((row) => row.fingerprint === selected);

    return chosen ? selectionReason(chosen) : null;
  }, [candidates, selected]);

  // Candidates + provider + profiles load once per mount; the backend
  // caches the ranking pass, so these are bounded calls — never
  // polls.
  const loadProfiles = useProfilesStore((state) => state.load);

  useEffect(() => {
    void loadCandidates();
    void loadConnectMode();
    void loadChains();
    void loadProfiles();
  }, [loadCandidates, loadConnectMode, loadChains, loadProfiles]);

  // v0.12.2: entering Chains mode preselects the FIRST chain
  // (deterministic — the list is name-ordered). The picker still lets
  // the user pick another chain explicitly; nothing auto-connects.
  useEffect(() => {
    if (connectMode === "chains" && chainsLoaded && !selectedChain && chains.length > 0) {
      selectChain(chains[0].id);
    }
  }, [connectMode, chainsLoaded, chains, selectedChain, selectChain]);

  /**
   * Hero state: mapped from the real connection state machine first,
   * then from the start-flow stages (which run while no connection
   * attempt is in flight yet). Terminal states are honest — the page
   * never invents progress.
   */
  const heroState: QuickHeroState = useMemo(() => {
    const state = snapshot?.state ?? "disconnected";

    if (state === "connected_verified") return "connected";
    if (state === "connected") return "verifying"; // route up, verification pending
    if (state === "disconnecting") return "disconnecting";
    if (state === "selecting" || state === "preparing") return "preparing";
    if (state === "starting_core") return "connecting";
    if (state === "waiting_for_ready") return "verifying";
    if (state === "verifying") return "verifying";
    if (state === "connection_failed") return "failed";

    if (flowRunning && FLOW_PREPARING_STAGES.has(flowStage)) return "preparing";

    if (connectError) return "failed";

    return "ready";
  }, [snapshot, flowRunning, flowStage, connectError]);

  const inFlight =
    busy ||
    heroState === "preparing" ||
    heroState === "connecting" ||
    heroState === "verifying" ||
    heroState === "disconnecting";

  const reducedMotion = useSettingsStore(effectiveReducedMotion);

  /** v0.9.10: the classified, humanized failure view (§5). */
  const humanized = useMemo(
    () => (heroState === "failed" ? humanizeConnectionError(connectError ?? "") : null),
    [heroState, connectError],
  );

  /** v0.9.10 “Fix my connection”: one action that refreshes evidence
   * (sources), re-tests what is stale and reconnects — the same
   * engine flow a power user would drive manually, condensed to a
   * single, honest button. */
  const [fixing, setFixing] = useState(false);

  const onFixConnection = useCallback(async () => {
    if (fixing) return;

    setFixing(true);

    try {
      // The adaptive start flow already runs: detect → discover →
      // test → rank → connect → verify — with real progress states.
      await useStartFlowStore.getState().run();
    } finally {
      setFixing(false);
    }
  }, [fixing]);

  /** Detail line under the headline — real backend messages only. */
  const detail = useMemo(() => {
    switch (heroState) {
      case "preparing":
        return flowMessage || "Selecting best available route…";
      case "connecting":
        return "Starting tunnel…";
      case "verifying":
        return "Checking that real Internet traffic flows…";
      case "failed":
        return humanized ? humanized.whatHappened : "No usable connection was verified.";
      case "disconnecting":
        return "Closing the tunnel…";
      case "connected":
        return "";
      default:
        return "Fastest measured connection, one tap away.";
    }
  }, [heroState, flowMessage, humanized]);

  const onConnect = useCallback(() => {
    // v0.12.2: every mode runs the SAME high-level lifecycle in the
    // backend (select → start ONE core → wait ready → verify actual
    // Internet → connected → monitor → recover). Proxy chains compile
    // into ONE core process — never one process per hop.
    if (connectMode === "chains") {
      if (selectedChain) {
        void runProviderRoute(() => connectionService.ConnectChain(selectedChain));
      }

      return;
    }

    // Auto mode: the existing evidence-based best-candidate engine
    // (fresh verified successes win; bounded retesting; cooldowns).
    if (connectMode === "auto") {
      const qc = useQuickConnectStore.getState();

      if (qc.candidates.length > 0) {
        void connectBest();
      } else {
        void useStartFlowStore.getState().run();
      }

      return;
    }

    // Configurations mode — the classic engine flow.

    // §13 — Case A: explicit selection wins.
    if (selected) {
      void connect(selected);

      return;
    }

    // Case B: engine best-candidate selection from real test history.
    // Case C/D are handled by the state machine itself (connected
    // short-circuits the UI; recovery/fallback runs in the engine).
    const qc = useQuickConnectStore.getState();

    if (qc.candidates.length > 0) {
      void connectBest();
    } else {
      // §10 — no candidates: the existing adaptive discovery path
      // (detect → discover → test → rank → connect → verify).
      void useStartFlowStore.getState().run();
    }
  }, [connectMode, selectedChain, selected, connect, connectBest]);

  const extras = (snapshot ?? {}) as SnapshotExtras;
  const connectedName = snapshot?.config_name || snapshot?.config_display || "";
  const snapshotPing = snapshot?.latency_ms ?? 0;
  const medianPing = extras.ping_median_ms ?? 0;
  const connectedPing = snapshotPing > 0 ? snapshotPing : medianPing > 0 ? medianPing : 0;
  const verified = extras.verification === "usable";
  // v0.9.8.1: a verified session with a 0 ms reading is a MEASURED
  // sub-millisecond round trip — displayed honestly, never hidden.
  const connectedPingMeasured =
    verified && (snapshotPing > 0 || medianPing > 0 || snapshotPing === 0);

  return (
    <div>
      <div className="page-header">
        <div className="page-heading">
          <h1 className="page-title">Quick Connect</h1>
          <div className="page-subtitle">One tap to the fastest measured connection.</div>
        </div>
      </div>

      <section
        className={`qc-hero ${heroState} ${reducedMotion ? "reduced" : ""}`}
        aria-label="Quick Connect"
        aria-busy={inFlight}
      >
        <QuickOrb state={heroState} />

        <div className="qc-state" aria-live="polite">
          <div className="qc-headline">{HEADLINES[heroState]}</div>
          {heroState === "connected" ? (
            <div className="qc-result">
              <div className="qc-result-line">
                {connectedName ? truncate(connectedName, 36) : "Session active"}
                {connectedPing > 0 ? (
                  <span className="qc-result-ping"> · {formatLatency(connectedPing)}</span>
                ) : (
                  connectedPingMeasured && <span className="qc-result-ping"> · &lt; 1 ms</span>
                )}
              </div>
              <div className="qc-result-sub">
                {snapshot?.core
                  ? `${snapshot.core}${snapshot.core_version ? ` · ${snapshot.core_version}` : ""}`
                  : ""}
              </div>
            </div>
          ) : (
            <div className="qc-detail">{detail}</div>
          )}
          {heroState === "connected" && verified && (
            <span className="badge success qc-verified">
              <IconCheck size={11} />
              Verified
            </span>
          )}
        </div>

        {heroState !== "connected" && (
          <ProfileSelector disabled={inFlight} />
        )}

        {heroState !== "connected" && (
          <ConnectModeSelector
            mode={connectMode}
            onModeChange={(mode) => void setConnectMode(mode)}
            disabled={inFlight}
            loaded={modeLoaded}
            loading={modeLoading}
          />
        )}

        {heroState !== "connected" && connectMode === "configs" && (
          <>
            <QuickPicker
              rows={quickPickerRows(candidates)}
              loading={candidatesLoading && !candidatesLoaded}
              loaded={candidatesLoaded}
              selected={selected}
              disabled={inFlight}
              onSelect={select}
            />
            {selectedReason && (
              <p className="qc-selection-reason" role="note" aria-live="polite">
                {selectedReason}
              </p>
            )}
          </>
        )}

        {heroState !== "connected" && connectMode === "chains" && (
          <ChainPicker
            chains={chains}
            loaded={chainsLoaded}
            loading={chainsLoading}
            selected={selectedChain}
            disabled={inFlight}
            onSelect={selectChain}
          />
        )}

        {heroState === "connected" ? (
          // §4: while connected, CONNECTED is the single primary state
          // representation — no competing action button lives here.
          <div className="qc-connect connected" role="status">
            <span className="qc-btn-slot" aria-hidden>
              <IconCheck size={15} />
            </span>
            CONNECTED
          </div>
        ) : (
          <button
            type="button"
            className="btn primary qc-connect"
            disabled={inFlight}
            aria-busy={inFlight}
            onClick={onConnect}
          >
            <span className="qc-btn-slot" aria-hidden>
              {inFlight ? <span className="btn-spinner" /> : <IconZap size={15} />}
            </span>
            {BUTTON_LABELS[heroState]}
          </button>
        )}

        {heroState === "failed" && humanized && (
          <div className="qc-error-panel" role="alert">
            <div className="qc-error-what">
              <span className="qc-error-label">What happened</span>
              <p>{humanized.whatHappened}</p>
            </div>
            <div className="qc-error-doing">
              <span className="qc-error-label">What FreeIran is doing</span>
              <p>{humanized.doingNow}</p>
            </div>
            <div className="qc-error-cando">
              <span className="qc-error-label">What you can do</span>
              <ul>
                {humanized.canDo.map((step) => (
                  <li key={step}>{step}</li>
                ))}
              </ul>
            </div>
            <details className="qc-error-technical">
              <summary>Technical details</summary>
              <pre className="mono-cell">{humanized.technical}</pre>
            </details>
          </div>
        )}

        <div className="qc-links">
          {heroState === "connected" && (
            <button type="button" className="linklike" onClick={() => onNavigate("connection")}>
              Disconnect &amp; advanced controls
            </button>
          )}
          {heroState === "ready" && candidates.length > 0 && (
            <span className="qc-hint" title={EDUCATION_HINTS.verification}>
              Ordered by measured ping · verified connections first
            </span>
          )}
          {heroState === "ready" && candidates.length === 0 && !candidatesLoading && (
            <span className="qc-hint" title={EDUCATION_HINTS.source}>
              First time here? Press Connect — FreeIran will discover, test and pick a working route.
            </span>
          )}
        </div>
      </section>

      {/* v0.9.10: the recovery action lives directly under the failed
          hero — one obvious, honest path forward (§5 Errors). */}
      {heroState === "failed" && (
        <section className="qc-recover" aria-label="Recovery actions">
          <button
            type="button"
            className="btn primary qc-fix"
            disabled={fixing || busy}
            aria-busy={fixing}
            onClick={() => void onFixConnection()}
          >
            <span className="btn-icon-slot" aria-hidden>
              {fixing ? <span className="btn-spinner" /> : <IconRefresh size={14} />}
            </span>
            Fix my connection
          </button>
          <span className="qc-recover-note">
            Refreshes sources, re-tests the stale candidates and reconnects — with live progress.
          </span>
          <button type="button" className="linklike" onClick={() => onNavigate("connection")}>
            Connection details &amp; diagnostics
          </button>
        </section>
      )}

      {heroState === "ready" && (
        <section className="qc-learn" aria-label="What do these mean?">
          <h3 className="qc-learn-title">What do these mean?</h3>
          <dl className="qc-learn-grid">
            <div>
              <dt>Configuration</dt>
              <dd>{EDUCATION_HINTS.configuration}</dd>
            </div>
            <div>
              <dt>Verified</dt>
              <dd>{EDUCATION_HINTS.verification}</dd>
            </div>
            <div>
              <dt>Proxy Chains</dt>
              <dd>{EDUCATION_HINTS.proxyChain}</dd>
            </div>
          </dl>
        </section>
      )}

      {/* v0.13.0: system integration is a FIRST-CLASS block on the Main
          page — the user's fastest path from "connected" to "the system
          actually uses the tunnel". Connection stays the detailed
          lifecycle page; the controls here call the SAME TunnelService. */}
      <SystemIntegrationCard onNavigate={onNavigate} />
    </div>
  );
}

/** OwnershipStatusView mirrors the generated tunnel OwnershipStatus
 * class (structural subset used for rendering). */
interface OwnershipStatusView {
  present: boolean;
  phase?: string;
  endpoint?: string;
}

/** TUNSnapshotView mirrors the generated tunnel TUNSnapshot class
 * (structural subset used for rendering). */
interface TUNSnapshotView {
  available?: boolean;
  installed?: boolean;
  active?: boolean;
  status?: string;
  backend?: string;
  details?: string;
  requires_elevation?: boolean;
}

/** TunnelStateView mirrors the generated tunnel State class. */
interface TunnelStateView {
  mode?: string;
  active?: boolean;
  endpoint?: string;
  tun?: TUNSnapshotView;
}

/**
 * v0.13.0 System Integration block (Main page, first class):
 *
 *   - System Proxy ON/OFF and TUN/VPN ON/OFF — real toggles wired to
 *     the ONE TunnelService (no second integration manager);
 *   - local inbound port settings (SOCKS5 / HTTP) — compact numeric
 *     inputs persisted through the ONE settings path;
 *   - the current endpoint and ownership/state read from the backend,
 *     never invented client-side.
 *
 * Honest prerequisites: the system proxy needs a CONNECTED core (it
 * routes system traffic into the live local inbound — with no session
 * there is nothing to point WinINet at, and the card says so); TUN
 * runs its own managed sing-box dataplane, so it needs a selected or
 * connected configuration, not a live session. Disconnected state is
 * still meaningful: the real backend mode is shown, OFF really turns
 * integration off, and stale app-owned proxy residue from a crashed
 * session is removable without any connection.
 */
function SystemIntegrationCard({ onNavigate }: { onNavigate: (page: Page) => void }) {
  const snapshot = useConnectionStore((state) => state.snapshot);
  // The state string rides the ONE authoritative snapshot (the store
  // has no separate state field).
  const connected =
    snapshot?.state === "connected" || snapshot?.state === "connected_verified";

  const settings = useSettingsStore((state) => state.settings);
  const loadSettings = useSettingsStore((state) => state.load);
  const saveSettings = useSettingsStore((state) => state.save);

  const [tunnel, setTunnel] = useState<TunnelStateView | null>(null);
  const [ownership, setOwnership] = useState<OwnershipStatusView | null>(null);
  const [busy, setBusy] = useState<"proxy" | "tun" | "off" | "cleanup" | null>(null);
  const [socksPort, setSocksPort] = useState(String(settings?.local_socks_port || 0));
  const [httpPort, setHttpPort] = useState(String(settings?.local_http_port || 0));
  const [portsSaved, setPortsSaved] = useState(false);

  const proxyOn = tunnel?.mode === "system_proxy" && tunnel?.active;
  const tunOn = tunnel?.mode === "tun" && Boolean(tunnel?.tun?.active);

  const refreshState = useCallback(async () => {
    try {
      const [state, own] = await Promise.all([
        call(() => tunnelService.State()),
        call(() => tunnelOwnership.OwnershipStatus()).catch(() => null),
      ]);

      setTunnel((state ?? null) as TunnelStateView | null);
      setOwnership((own ?? null) as OwnershipStatusView | null);
    } catch {
      setTunnel({ mode: "off" }); // never a fake active state
    }
  }, []);

  useEffect(() => {
    void refreshState();
    void loadSettings();
  }, [refreshState, loadSettings]);

  // The connection event stream refreshes the endpoint read: the
  // tray-free state never drifts after connects, disconnects or
  // external changes.
  useEffect(() => {
    void refreshState();
  }, [connected, snapshot?.endpoint, refreshState]);

  useEffect(() => {
    setSocksPort(String(settings?.local_socks_port || 0));
    setHttpPort(String(settings?.local_http_port || 0));
  }, [settings?.local_socks_port, settings?.local_http_port]);

  const endpoint = tunnel?.endpoint || snapshot?.endpoint || "";

  const toggleProxy = async () => {
    setBusy("proxy");

    try {
      if (proxyOn) {
        await call(() => tunnelService.Disable());
      } else if (connected && endpoint) {
        const idx = endpoint.lastIndexOf(":");

        if (idx > 0) {
          const host = endpoint.slice(0, idx);
          const port = Number.parseInt(endpoint.slice(idx + 1), 10);

          await call(() => tunnelService.EnableSystemProxy(host, port || 0, false, []));
        }
      }
    } finally {
      setBusy(null);
      void refreshState();
    }
  };

  const toggleTUN = async () => {
    setBusy("tun");

    try {
      if (tunOn || tunnel?.mode === "tun") {
        await call(() => tunnelService.Disable());
      } else {
        const configID = String(snapshot?.config_id ?? "");

        if (configID) {
          await call(() => tunnelService.EnableTUN(configID));
        }
      }
    } catch (error) {
      toast("error", "TUN could not be enabled", describeError(error));
    } finally {
      setBusy(null);
      void refreshState();
    }
  };

  const cleanupResidue = async () => {
    setBusy("cleanup");

    try {
      const cleaned = await call(() => tunnelService.CleanupStaleOwnership());

      toast(
        cleaned ? "success" : "info",
        cleaned ? "Stale proxy removed" : "Nothing to clean",
        cleaned ? "The system proxy state recorded before the crash was restored." : "No app-owned proxy residue was found.",
      );
    } catch (error) {
      toast("error", "Cleanup failed", describeError(error));
    } finally {
      setBusy(null);
      void refreshState();
    }
  };

  const savePorts = async () => {
    if (!settings) return;

    const next = {
      ...settings,
      local_socks_port: Math.max(0, Math.min(65535, Number(socksPort) || 0)),
      local_http_port: Math.max(0, Math.min(65535, Number(httpPort) || 0)),
    };

    const saved = await saveSettings(next);

    if (saved) {
      setPortsSaved(true);
      window.setTimeout(() => setPortsSaved(false), 2000);
    }
  };

  const residuePresent = ownership?.present && !proxyOn && !tunOn;

  return (
    <section className="card sysint" aria-label="System integration">
      <div className="card-header">
        <h3 className="card-title eyebrow">System integration</h3>
        <span className={`badge ${proxyOn || tunOn ? "success" : "info"}`}>
          {tunOn ? "tun" : proxyOn ? "system proxy" : "direct"}
        </span>
      </div>

      <div className="sysint-toggles">
        <div className="sysint-row">
          <div className="sysint-label">
            <span className="cell-title">System proxy</span>
            <span className="cell-sub" data-tip="Points Windows at FreeIran's local inbound (WinINet). Needs a connected configuration.">
              {proxyOn && endpoint
                ? `on — ${endpoint}`
                : connected
                  ? "off — connect routes through the tunnel once enabled"
                  : "needs a connected configuration"}
            </span>
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={proxyOn}
            aria-label="System proxy"
            className={`switch ${proxyOn ? "on" : ""}`}
            disabled={busy !== null || (!proxyOn && (!connected || !endpoint))}
            onClick={() => void toggleProxy()}
          />
        </div>

        <div className="sysint-row">
          <div className="sysint-label">
            <span className="cell-title">TUN / VPN mode</span>
            <span className="cell-sub" data-tip="Routes ALL system traffic through the managed sing-box core's native TUN adapter (Windows, Wintun). Needs elevation and a selected configuration; not a kill switch.">
            {tunnel?.mode === "tun"
              ? tunnel.tun?.status === "active"
                ? "active — all system traffic through the tunnel"
                : `status: ${tunnel.tun?.status ?? "unknown"}`
              : connected || snapshot?.config_id
                ? "off — selected configuration runs through the TUN adapter"
                : "needs a selected or connected configuration"}
            </span>
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={tunOn}
            aria-label="TUN VPN mode"
            className={`switch ${tunOn ? "on" : ""}`}
            disabled={
              busy !== null ||
              (!tunOn && tunnel?.mode !== "tun" && !(connected || snapshot?.config_id))
            }
            onClick={() => void toggleTUN()}
          />
        </div>
      </div>

      {residuePresent && (
        <div className="sysint-residue" role="status">
          <span>
            FreeIran owns system-proxy state from a crashed session (ownership phase: {ownership?.phase ?? "unknown"}).
          </span>
          <button type="button" className="btn sm" disabled={busy !== null} onClick={() => void cleanupResidue()}>
            {busy === "cleanup" ? "Cleaning…" : "Remove stale proxy"}
          </button>
        </div>
      )}

      <div className="sysint-ports">
        <span className="sysint-ports-title">Local inbound ports</span>
        <div className="field-grid two">
          <div className="field">
            <label className="field-label" htmlFor="sysint-socks-port">
              SOCKS5 (0 = automatic)
            </label>
            <input
              id="sysint-socks-port"
              className="input input-compact"
              type="number"
              min={0}
              max={65535}
              value={socksPort}
              onChange={(event) => setSocksPort(event.target.value)}
            />
          </div>
          <div className="field">
            <label className="field-label" htmlFor="sysint-http-port">
              HTTP (0 = off)
            </label>
            <input
              id="sysint-http-port"
              className="input input-compact"
              type="number"
              min={0}
              max={65535}
              value={httpPort}
              onChange={(event) => setHttpPort(event.target.value)}
            />
          </div>
        </div>
        <button type="button" className="btn sm ghost" disabled={!settings} onClick={() => void savePorts()}>
          {portsSaved ? "Saved" : "Save ports"}
        </button>
      </div>

      <button type="button" className="linklike" onClick={() => onNavigate("connection")}>
        Tunnel lifecycle &amp; diagnostics
      </button>
    </section>
  );
}

/**
 * v0.9.8.1: fire-and-forget provider route with a final, safe state
 * refresh. The refresh itself is swallowed: the authoritative
 * connection state also arrives via the freeiran:connection events,
 * and an unhandled rejection here must never escape (the connection
 * store surfaces failures through its own error path).
 */
async function runProviderRoute(operation: () => Promise<unknown>): Promise<void> {
  await call(operation).catch(() => undefined);

  void useConnectionStore.getState().refresh().catch(() => undefined);
}

/**
 * v0.9.11 Connection Profiles selector (P2 §18): a lightweight row of
 * profile chips above the provider selector — selecting a profile
 * applies its preferences on the backend through the ONE settings
 * path and re-syncs the page from the returned authoritative state
 * (the provider chips and the configuration preselection follow).
 *
 * The default interaction stays simple: chips select, everything else
 * (create / edit / rename / duplicate / delete / set default) lives
 * behind the "Manage profiles" advanced section. No profile data is
 * invented here — chips render only what the backend returned, and
 * the whole surface disappears when no profile exists.
 */
function ProfileSelector({ disabled }: { disabled: boolean }) {
  const profiles = useProfilesStore((state) => state.profiles);
  const active = useProfilesStore((state) => state.active);
  const loaded = useProfilesStore((state) => state.loaded);
  const loading = useProfilesStore((state) => state.loading);
  const error = useProfilesStore((state) => state.error);
  const setActiveProfile = useProfilesStore((state) => state.setActive);

  if (loading && !loaded) return null; // first paint: no invented state

  if (loaded && profiles.length === 0) return null; // feature stays invisible until used

  /** Activate a profile and re-sync the page's mode/selection state. */
  const onActivate = (profileID: string, configID?: string) => {
    void (async () => {
      try {
        await setActiveProfile(profileID);

        // The backend applied the profile's preferences: re-read the
        // route mode and preselect the profile's configuration so
        // the whole page reflects the authoritative state immediately.
        await useConnectModeStore.getState().load();

        if (configID) {
          useQuickConnectStore.getState().select(configID);
        }
      } catch {
        // The store recorded the error; the alert below renders it.
      }
    })();
  };

  return (
    <div className="qc-profiles">
      <div className="qc-profile-chips" role="radiogroup" aria-label="Connection profile">
        {profiles.map((profile) => {
          const isActive = active?.id === profile.id;

          return (
            <button
              key={profile.id}
              type="button"
              role="radio"
              aria-checked={isActive}
              className={`qc-mode-chip qc-profile-chip ${isActive ? "active" : ""}`}
              disabled={disabled}
              title={
                profile.default
                  ? `${profile.name} (startup profile)`
                  : profile.name
              }
              onClick={() => onActivate(profile.id, profile.config_id || undefined)}
            >
              {profile.name}
              {profile.default && <span className="qc-profile-default" title="Startup profile">★</span>}
            </button>
          );
        })}
      </div>

      {error && (
        <div className="qc-picker-note" role="alert">
          Profiles could not be updated: {error}
        </div>
      )}

      <ProfileManager disabled={disabled} onActivate={onActivate} />
    </div>
  );
}

/** Local editor form state for the profile manager. */
interface ProfileDraft {
  id: string | null;
  name: string;
  mode: ProfileMode;
  socks: string;
  http: string;
}

function draftFromProfile(
  id: string | null,
  profile?: { name: string; mode: string; local_socks_port?: number; local_http_port?: number },
): ProfileDraft {
  return {
    id,
    name: profile?.name ?? "",
    // The generated ProfileView carries mode as a plain string; the
    // local editor works on the narrow union (normalized here, never
    // trusted blindly).
    mode: normalizeProfileMode(profile?.mode),
    socks: profile?.local_socks_port ? String(profile.local_socks_port) : "",
    http: profile?.local_http_port ? String(profile.local_http_port) : "",
  };
}

/**
 * Profile manager (advanced section): create / edit (rename) /
 * duplicate / delete / set default. Every action goes through the
 * profiles store → backend → authoritative refresh; nothing is
 * stored client-side. No credential fields exist anywhere in a
 * profile.
 */
function ProfileManager({
  disabled,
  onActivate,
}: {
  disabled: boolean;
  onActivate: (profileID: string, configID?: string) => void;
}) {
  const profiles = useProfilesStore((state) => state.profiles);
  const active = useProfilesStore((state) => state.active);
  const create = useProfilesStore((state) => state.create);
  const update = useProfilesStore((state) => state.update);
  const duplicate = useProfilesStore((state) => state.duplicate);
  const remove = useProfilesStore((state) => state.remove);
  const setDefault = useProfilesStore((state) => state.setDefault);

  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<ProfileDraft | null>(null);
  const [localError, setLocalError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const onSave = () => {
    if (!draft || busy) return;

    // v0.11.0: the machine-generated binding models define the
    // ProfileSpec payload as a class; constructing it explicitly keeps
    // the wire format identical (only the provided fields serialize)
    // while satisfying the generated signature.
    const spec = new ProfileSpecModel({
      name: draft.name,
      mode: draft.mode,
      local_socks_port: draft.socks ? Number(draft.socks) : 0,
      local_http_port: draft.http ? Number(draft.http) : 0,
    }) as ProfileSpec;

    setBusy(true);
    setLocalError(null);

    void (async () => {
      try {
        if (draft.id) {
          await update(draft.id, spec);
        } else {
          await create(spec);
        }

        setDraft(null);
      } catch (err) {
        setLocalError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    })();
  };

  const onAction = (action: () => Promise<unknown>) => {
    if (busy) return;

    setBusy(true);
    setLocalError(null);

    void (async () => {
      try {
        await action();
      } catch (err) {
        setLocalError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    })();
  };

  return (
    <details className="qc-profiles-manage" open={open} onToggle={(event) => setOpen((event.target as HTMLDetailsElement).open)}>
      <summary>Manage profiles</summary>

      <ul className="qc-profile-list">
        {profiles.map((profile) => (
          <li key={profile.id} className="qc-profile-row" data-active={active?.id === profile.id ? "true" : undefined}>
            <span className="qc-profile-name">
              {profile.name}
              {profile.default && <span className="qc-profile-default" title="Startup profile">★</span>}
              {active?.id === profile.id && <span className="badge success">Active</span>}
            </span>
            <span className="qc-profile-meta">
              {PROFILE_MODE_LABELS[normalizeProfileMode(profile.mode)]}
              {profile.local_socks_port ? ` · SOCKS ${profile.local_socks_port}` : ""}
              {profile.local_http_port ? ` · HTTP ${profile.local_http_port}` : ""}
            </span>
            <span className="qc-profile-actions">
              {(!active || active.id !== profile.id) && (
                <button
                  type="button"
                  className="linklike"
                  disabled={disabled || busy}
                  onClick={() => onActivate(profile.id, profile.config_id || undefined)}
                >
                  Use
                </button>
              )}
              <button
                type="button"
                className="linklike"
                disabled={disabled || busy}
                onClick={() => onAction(() => setDefault(profile.id))}
              >
                {profile.default ? "Default" : "Set default"}
              </button>
              <button
                type="button"
                className="linklike"
                disabled={disabled || busy}
                onClick={() => setDraft(draftFromProfile(profile.id, profile))}
              >
                Rename / edit
              </button>
              <button
                type="button"
                className="linklike"
                disabled={disabled || busy}
                onClick={() => onAction(() => duplicate(profile.id))}
              >
                Duplicate
              </button>
              <button
                type="button"
                className="linklike danger"
                disabled={disabled || busy}
                onClick={() => onAction(() => remove(profile.id))}
              >
                Delete
              </button>
            </span>
          </li>
        ))}
      </ul>

      {draft ? (
        <div className="qc-profile-form">
          <label>
            Name
            <input
              value={draft.name}
              maxLength={64}
              onChange={(event) => setDraft({ ...draft, name: event.target.value })}
            />
          </label>
          <label>
            Mode
            <select
              value={draft.mode}
              onChange={(event) => setDraft({ ...draft, mode: event.target.value as ProfileMode })}
            >
              <option value="auto">Auto</option>
              <option value="configs">Configurations</option>
              <option value="tor">Tor</option>
              <option value="psiphon">Psiphon</option>
            </select>
          </label>
          <label>
            SOCKS port
            <input
              type="number"
              min={0}
              value={draft.socks}
              placeholder="auto"
              onChange={(event) => setDraft({ ...draft, socks: event.target.value })}
            />
          </label>
          <label>
            HTTP port
            <input
              type="number"
              min={0}
              value={draft.http}
              placeholder="auto"
              onChange={(event) => setDraft({ ...draft, http: event.target.value })}
            />
          </label>
          <div className="qc-profile-form-actions">
            <button type="button" className="btn primary" disabled={busy} onClick={onSave}>
              {draft.id ? "Save changes" : "Create profile"}
            </button>
            <button type="button" className="linklike" disabled={busy} onClick={() => setDraft(null)}>
              Cancel
            </button>
          </div>
        </div>
      ) : (
        <div className="qc-profile-form-actions">
          <button
            type="button"
            className="linklike"
            disabled={disabled || busy}
            onClick={() => setDraft(draftFromProfile(null))}
          >
            New profile
          </button>
        </div>
      )}

      {localError && (
        <div className="qc-picker-note" role="alert">
          {localError}
        </div>
      )}

      <p className="qc-picker-note">
        Profiles are saved sets of connection preferences. Activating one applies its preferences — every
        connection still runs the full verified engine flow, and no credentials are ever stored in a profile.
      </p>
    </details>
  );
}

/**
 * v0.12.2 route selector: a compact segmented control above the
 * pickers. Auto is evidence-based in the backend; Configurations is
 * explicit user selection; Proxy Chains connect through user-built
 * ordered hop lists compiled into ONE core process (the removed
 * Tor/Psiphon selector is gone).
 */
function ConnectModeSelector({
  mode,
  onModeChange,
  disabled,
  loaded,
  loading,
}: {
  mode: ConnectMode;
  onModeChange: (mode: ConnectMode) => void;
  disabled: boolean;
  loaded: boolean;
  loading: boolean;
}) {
  const modes: ConnectMode[] = ["auto", "configs", "chains"];

  return (
    <div className="qc-provider-mode" role="radiogroup" aria-label="Connection route" data-loading={loading ? "true" : undefined}>
      {modes.map((candidate) => {
        const active = candidate === mode;

        return (
          <button
            key={candidate}
            type="button"
            role="radio"
            aria-checked={active}
            aria-label={CONNECT_MODE_LABELS[candidate]}
            className={`qc-mode-chip ${active ? "active" : ""}`}
            disabled={disabled}
            onClick={() => onModeChange(candidate)}
          >
            {CONNECT_MODE_LABELS[candidate]}
          </button>
        );
      })}
      {loaded && null}
    </div>
  );
}

/**
 * Chain picker (v0.12.2): the Configurations-mode equivalent for
 * chains — a collapsed control expanding to the chain list with hop
 * counts. Connecting a chain runs the SAME verified lifecycle; the
 * chain compiles into ONE core process.
 */
function ChainPicker({
  chains,
  loaded,
  loading,
  selected,
  disabled,
  onSelect,
}: {
  chains: Array<{ id: string; name: string; hops: number }>;
  loaded: boolean;
  loading: boolean;
  selected: string | null;
  disabled: boolean;
  onSelect: (chainID: string) => void;
}) {
  const [open, setOpen] = useState(false);

  const selectedChain = chains.find((chain) => chain.id === selected) || null;

  if (loaded && chains.length === 0) {
    return (
      <p className="qc-picker-note">
        No proxy chains yet — build one in Configurations from two or more working configurations.
      </p>
    );
  }

  return (
    <div className="qc-picker">
      <button
        type="button"
        className="qc-picker-toggle"
        aria-expanded={open}
        disabled={disabled}
        onClick={() => setOpen((value) => !value)}
      >
        <span className={selectedChain ? "" : "muted"}>
          {selectedChain
            ? `${selectedChain.name} · ${selectedChain.hops} hop${selectedChain.hops === 1 ? "" : "s"}`
            : "Select a proxy chain"}
        </span>
        <IconChevronDown size={14} />
      </button>

      {open && (
        <ul className="qc-picker-list" role="listbox" aria-label="Proxy chains">
          {chains.map((chain) => (
            <li key={chain.id}>
              <button
                type="button"
                role="option"
                aria-selected={chain.id === selected}
                disabled={disabled}
                onClick={() => {
                  onSelect(chain.id);
                  setOpen(false);
                }}
              >
                <span>{chain.name}</span>
                <span className="muted">
                  {chain.hops} hop{chain.hops === 1 ? "" : "s"}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}

      {loading && !loaded && <p className="qc-picker-note">Loading chains…</p>}
    </div>
  );
}

/** Status orb with a per-state animation (CSS only — no images/GIFs). */
function QuickOrb({ state }: { state: QuickHeroState }) {
  return (
    <div className={`qc-orb-wrap ${state}`} aria-hidden>
      <div className="qc-orb-ring" />
      <div className="qc-orb">
        <IconZap size={26} />
      </div>
    </div>
  );
}

/** Option model for the compact picker (index 0 = Auto). */
interface PickerOption {
  fingerprint: string | null;
  name: string;
  latencyMS: number;
  measured: boolean;
  protocol: string | null;
  statusClass: string | null;
  statusText: string | null;
  quality: string | null;
}

/**
 * Compact configuration picker (§7-§9): one collapsed control above
 * the connect button, expanding to a keyboard-usable listbox of the
 * best candidates. Uses only redacted display data.
 */
export function QuickPicker({
  rows,
  loading,
  loaded,
  selected,
  disabled,
  onSelect,
}: {
  rows: QuickCandidateRow[];
  loading: boolean;
  loaded: boolean;
  selected: string | null;
  disabled: boolean;
  onSelect: (fingerprint: string | null) => void;
}) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  const options: PickerOption[] = useMemo(
    () => [
      {
        fingerprint: null,
        name: "Auto — fastest measured",
        latencyMS: 0,
        measured: false,
        protocol: null,
        statusClass: null,
        statusText: null,
        quality: null,
      },
      ...rows.map((row) => ({
        fingerprint: row.fingerprint,
        name: row.name,
        latencyMS: row.latencyMS,
        measured: row.measured,
        protocol: row.protocol,
        statusClass: pickerStatusClass(row.status),
        statusText: row.status,
        quality: row.quality,
      })),
    ],
    [rows],
  );

  const activeOption = options[active] ?? options[0];
  const selectedOption = options.find((option) => option.fingerprint === selected) ?? options[0];

  const close = useCallback((focusTrigger: boolean) => {
    setOpen(false);

    if (focusTrigger) {
      rootRef.current?.querySelector<HTMLButtonElement>(".qc-picker-trigger")?.focus();
    }
  }, []);

  // Keyboard: the listbox owns focus while open (aria-activedescendant
  // pattern) so Arrow/Enter/Escape work immediately after expanding.
  useEffect(() => {
    if (open) listRef.current?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;

    const onPointer = (event: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(event.target as Node)) {
        setOpen(false);
      }
    };

    document.addEventListener("mousedown", onPointer);

    return () => document.removeEventListener("mousedown", onPointer);
  }, [open]);

  // Keep the active option in view while arrowing through the list.
  useEffect(() => {
    if (!open || !listRef.current) return;

    const node = listRef.current.querySelector(`#qc-opt-${active}`);

    // Guarded: the API is missing in some embedded/DOM environments.
    if (node && typeof node.scrollIntoView === "function") {
      node.scrollIntoView({ block: "nearest" });
    }
  }, [active, open]);

  const commit = (index: number) => {
    const option = options[index];

    if (!option) return;

    onSelect(option.fingerprint);
    close(true);
  };

  const onListKeyDown = (event: React.KeyboardEvent) => {
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        setActive((index) => Math.min(index + 1, options.length - 1));
        break;
      case "ArrowUp":
        event.preventDefault();
        setActive((index) => Math.max(index - 1, 0));
        break;
      case "Home":
        event.preventDefault();
        setActive(0);
        break;
      case "End":
        event.preventDefault();
        setActive(options.length - 1);
        break;
      case "Enter":
      case " ":
        event.preventDefault();
        commit(active);
        break;
      case "Escape":
      case "Tab":
        close(event.key === "Escape");
        break;
    }
  };

  return (
    <div className="qc-picker" ref={rootRef}>
      <button
        type="button"
        className="qc-picker-trigger"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={
          selectedOption.fingerprint
            ? `Configuration: ${selectedOption.name}. Change configuration`
            : "Configuration: automatic best selection. Change configuration"
        }
        disabled={disabled}
        onClick={() => {
          if (rows.length === 0) return;
          setOpen((value) => !value);
          setActive(Math.max(0, options.findIndex((option) => option.fingerprint === selected)));
        }}
      >
        <span className="qc-picker-main">
          <IconZap size={13} aria-hidden />
          <span className="qc-picker-name">{truncate(selectedOption.name, 30)}</span>
        </span>
        <span className="qc-picker-meta">
          {selectedOption.fingerprint && (selectedOption.measured || selectedOption.latencyMS > 0) && (
            <span className="mono-cell">{pickerLatencyText(selectedOption.latencyMS, selectedOption.measured)}</span>
          )}
          <IconChevronDown size={14} aria-hidden />
        </span>
      </button>

      {open && rows.length > 0 && (
        <div
          className="qc-picker-list"
          role="listbox"
          aria-label="Configurations"
          tabIndex={-1}
          ref={listRef}
          aria-activedescendant={`qc-opt-${active}`}
          onKeyDown={onListKeyDown}
        >
          {options.map((option, index) => (
            <div
              key={option.fingerprint ?? "auto"}
              id={`qc-opt-${index}`}
              role="option"
              aria-selected={option.fingerprint === selected}
              aria-label={
                `${option.fingerprint ? "Configuration" : "Automatic selection"}: ${option.name}` +
                (option.protocol ? `, ${option.protocol}` : "") +
                (option.measured || option.latencyMS > 0 ? `, ${pickerLatencyText(option.latencyMS, option.measured)}` : "") +
                (option.statusText ? `, ${option.statusText}` : "")
              }
              className={`qc-picker-row ${index === active ? "active" : ""} ${
                option.fingerprint === selected ? "selected" : ""
              }`}
              onClick={() => commit(index)}
              onMouseEnter={() => setActive(index)}
            >
              <span className="qc-picker-row-main">
                <IconZap size={12} aria-hidden />
                <span className="qc-picker-row-name">{truncate(option.name, 32)}</span>
                {option.protocol && <span className="qc-picker-row-proto">{option.protocol}</span>}
              </span>
              <span className="qc-picker-row-meta">
                <span className="mono-cell qc-ping">{pickerLatencyText(option.latencyMS, option.measured)}</span>
                {option.statusText && (
                  <span className={`qc-dot ${option.statusClass}`} title={option.statusText}>
                    <span className="sr-only">{option.statusText}</span>
                  </span>
                )}
              </span>
            </div>
          ))}
        </div>
      )}

      {(loading || (loaded && rows.length === 0)) && (
        <div className="qc-picker-note" aria-live="polite">
          {loading
            ? "Ranking configurations…"
            : activeOption.fingerprint === null
              ? "No tested connections available — connect to discover and measure."
              : ""}
        </div>
      )}
    </div>
  );
}
