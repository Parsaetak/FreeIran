import { lazy, Suspense, useEffect, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { useAppStore, connectAppStore } from "./state/appStore";
import { useSourcesStore, useConfigsStore } from "./state/stores";
import { connectConnectionStore } from "./state/connectionStore";
import { subscribeStartFlow, useStartFlowStore } from "./state/startflowStore";
import { useSettingsStore, effectiveReducedMotion } from "./state/settingsStore";
import { QuickConnectPage } from "./pages/QuickConnect";
import { SourcesPage } from "./pages/Sources";
import { ConfigsPage } from "./pages/Configs";
import { StatusBar } from "./components/StatusBar";
import { BootProgress } from "./components/LoadState";
import { Toasts } from "./components/Toasts";
import { ErrorBoundary } from "./components/ErrorBoundary";
import {
  IconChevronLeft,
  IconChevronRight,
  IconChevronDown,
  IconConfigs,
  IconCores,
  IconDashboard,
  IconDiagnostics,
  IconGlobe,
  IconPlug,
  IconSettings,
  IconSources,
  IconZap,
} from "./components/Icons";
import { call, diagnosticsService } from "./services";
import type { Page } from "./types/ui";

/**
 * v0.9.10 beginner-first navigation: the primary experience is
 *
 *     Connect → Configurations → Sources
 *
 * and every technical/diagnostic surface (Dashboard, Connection,
 * Cores, Network, Diagnostics, Settings) lives under a secondary
 * "More" section — one click away, progressively revealed, never
 * removed. The same structure appears on every page.
 */

/** Primary navigation: the everyday path for a non-technical user. */
const PRIMARY_NAV: Array<{ id: Page; label: string; icon: typeof IconZap }> = [
  { id: "quick", label: "Connect", icon: IconZap },
  { id: "configs", label: "Configurations", icon: IconConfigs },
  { id: "sources", label: "Sources", icon: IconSources },
];

/** Secondary navigation ("More"): technical and diagnostic surfaces. */
const SECONDARY_NAV: Array<{ id: Page; label: string; icon: typeof IconZap }> = [
  { id: "dashboard", label: "Dashboard", icon: IconDashboard },
  { id: "connection", label: "Connection", icon: IconPlug },
  { id: "cores", label: "Cores", icon: IconCores },
  { id: "network", label: "Network", icon: IconGlobe },
  { id: "diagnostics", label: "Diagnostics", icon: IconDiagnostics },
  { id: "settings", label: "Settings", icon: IconSettings },
];

/**
 * v0.9.10 startup performance: the "More" surfaces are code-split —
 * they load only on first visit, so the initial bundle renders the
 * primary experience (Connect / Configurations / Sources) without
 * paying for the diagnostic pages. Every lazy surface keeps its real
 * loading state; nothing fakes progress.
 */
const DashboardPage = lazy(() =>
  import("./pages/Dashboard").then((module) => ({ default: module.DashboardPage })),
);
const CoresPage = lazy(() =>
  import("./pages/Cores").then((module) => ({ default: module.CoresPage })),
);
const ConnectionPage = lazy(() =>
  import("./pages/Connection").then((module) => ({ default: module.ConnectionPage })),
);
const NetworkPage = lazy(() =>
  import("./pages/Network").then((module) => ({ default: module.NetworkPage })),
);
const DiagnosticsPage = lazy(() =>
  import("./pages/Diagnostics").then((module) => ({ default: module.DiagnosticsPage })),
);
const SettingsPage = lazy(() =>
  import("./pages/Settings").then((module) => ({ default: module.SettingsPage })),
);

export function App() {
  // Quick Connect is the landing surface — the application's home
  // connection action.
  const [page, setPage] = useState<Page>("quick");
  const [collapsed, setCollapsed] = useState(false);
  const [moreOpen, setMoreOpen] = useState(false);
  const [version, setVersion] = useState("");
  const status = useAppStore((state) => state.status);
  const bootPhase = useAppStore((state) => state.bootPhase);
  const bootTimings = useAppStore((state) => state.bootTimings);

  useEffect(() => {
    const disposeAppState = connectAppStore();
    const disposeConnection = connectConnectionStore();

    // start-flow broadcasts keep the Smart Start panel live.
    const disposeStartFlow = subscribeStartFlow();

    // Initial flow status + environment analysis for display.
    void useStartFlowStore.getState().refresh();
    void useStartFlowStore.getState().refreshEnvironment();

    // Startup lifecycle: the first mounted frame reports ui_ready to
    // the backend. One-shot; failures are irrelevant.
    try {
      Events.Emit("freeiran:ui-ready");
    } catch {
      /* telemetry only — never blocks the UI */
    }

    return () => {
      disposeAppState();
      disposeConnection();
      disposeStartFlow();
    };
  }, []);

  // Initial data load. Fetch ONCE per backend lifetime (see §29): the
  // ref gates the load to the first reachable backend; a later
  // `backend_unavailable` resets it so recovery re-fetches stale stores.
  const initialLoadRef = useRef(false);

  useEffect(() => {
    if (status === "backend_unavailable") {
      initialLoadRef.current = false;

      return;
    }

    if (initialLoadRef.current) return;

    initialLoadRef.current = true;

    void useSourcesStore.getState().load();
    void useConfigsStore.getState().loadPage(0);
    void useSettingsStore.getState().load();
  }, [status]);

  useEffect(() => {
    let cancelled = false;

    void call(() => diagnosticsService.Version())
      .then((v) => {
        if (!cancelled) setVersion(v);
      })
      .catch(() => {
        /* version display is best-effort */
      });

    return () => {
      cancelled = true;
    };
  }, []);

  // Reduced-motion accessibility setting drives an html-level class.
  const reducedMotion = useSettingsStore(effectiveReducedMotion);

  useEffect(() => {
    document.documentElement.classList.toggle("reduced-motion", reducedMotion);
  }, [reducedMotion]);

  // Collapse the sidebar to icons on narrow windows.
  useEffect(() => {
    const query = window.matchMedia("(max-width: 1100px)");
    const apply = () => {
      if (query.matches) setCollapsed(true);
    };

    apply();
    query.addEventListener("change", apply);

    return () => query.removeEventListener("change", apply);
  }, []);

  const onSecondaryPage = SECONDARY_NAV.some((item) => item.id === page);

  // A "More" page is active: keep the section expanded so the current
  // location is always visible (consistent navigation, §2).
  useEffect(() => {
    if (onSecondaryPage) setMoreOpen(true);
  }, [onSecondaryPage]);

  const navigate = (target: Page) => setPage(target);

  return (
    <div className={`shell ${collapsed ? "nav-collapsed" : ""}`}>
      <div className="shell-body">
        <nav className="sidebar" aria-label="Primary">
          <div className="brand">
            <span className="brand-mark" aria-hidden>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" focusable="false">
                <path d="M13 2 4.5 13.5H11L9.5 22 19 10h-6.5L13 2Z" />
              </svg>
            </span>
            <span className="brand-name">FreeIran</span>
            {version && <span className="brand-version">{version}</span>}
          </div>

          <div className="nav-section">
            {PRIMARY_NAV.map((item) => {
              const Icon = item.icon;

              return (
                <button
                  key={item.id}
                  type="button"
                  className={`nav-item ${page === item.id ? "active" : ""}`}
                  aria-current={page === item.id ? "page" : undefined}
                  title={collapsed ? item.label : undefined}
                  onClick={() => navigate(item.id)}
                >
                  <span className="nav-icon" aria-hidden>
                    <Icon size={17} />
                  </span>
                  <span className="nav-label">{item.label}</span>
                </button>
              );
            })}
          </div>

          <div className="nav-section">
            <button
              type="button"
              className={`nav-item nav-more-toggle ${moreOpen || onSecondaryPage ? "expanded" : ""} ${
                onSecondaryPage ? "active-parent" : ""
              }`}
              aria-expanded={moreOpen || onSecondaryPage}
              aria-current={onSecondaryPage ? "true" : undefined}
              title={collapsed ? "More" : undefined}
              onClick={() => setMoreOpen((value) => !value)}
            >
              <span className="nav-icon" aria-hidden>
                <IconSettings size={17} />
              </span>
              <span className="nav-label">More</span>
              <span className="nav-more-chevron" aria-hidden>
                <IconChevronDown size={13} />
              </span>
            </button>

            {(moreOpen || onSecondaryPage) && !collapsed && (
              <div className="nav-subsection">
                {SECONDARY_NAV.map((item) => {
                  const Icon = item.icon;

                  return (
                    <button
                      key={item.id}
                      type="button"
                      className={`nav-item nav-subitem ${page === item.id ? "active" : ""}`}
                      aria-current={page === item.id ? "page" : undefined}
                      onClick={() => navigate(item.id)}
                    >
                      <span className="nav-icon" aria-hidden>
                        <Icon size={16} />
                      </span>
                      <span className="nav-label">{item.label}</span>
                    </button>
                  );
                })}
              </div>
            )}
          </div>

          <div className="sidebar-footer">
            <button
              type="button"
              className="nav-item"
              aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
              aria-expanded={!collapsed}
              onClick={() => setCollapsed((value) => !value)}
            >
              <span className="nav-icon" aria-hidden>
                {collapsed ? <IconChevronRight size={17} /> : <IconChevronLeft size={17} />}
              </span>
              <span className="nav-label">Collapse</span>
            </button>
          </div>
        </nav>

        <main className="content">
          <div className="content-inner">
            {/* Real boot progress: driven by backend telemetry,
                disappears the moment the app is ready. */}
            <BootProgress phase={bootPhase} timings={bootTimings} />
            <ErrorBoundary onOpenDiagnostics={() => setPage("diagnostics")}>
              <div className="page" key={page}>
                {page === "quick" && <QuickConnectPage onNavigate={navigate} />}
                {page === "configs" && <ConfigsPage />}
                {page === "sources" && <SourcesPage />}

                {/* Secondary surfaces: code-split, loaded on first
                    visit with a real loading state. */}
                {page === "dashboard" && (
                  <Suspense fallback={<PageLoading label="Dashboard" />}>
                    <DashboardPage onNavigate={navigate} />
                  </Suspense>
                )}
                {page === "connection" && (
                  <Suspense fallback={<PageLoading label="Connection" />}>
                    <ConnectionPage />
                  </Suspense>
                )}
                {page === "cores" && (
                  <Suspense fallback={<PageLoading label="Cores" />}>
                    <CoresPage />
                  </Suspense>
                )}
                {page === "network" && (
                  <Suspense fallback={<PageLoading label="Network" />}>
                    <NetworkPage />
                  </Suspense>
                )}
                {page === "diagnostics" && (
                  <Suspense fallback={<PageLoading label="Diagnostics" />}>
                    <DiagnosticsPage />
                  </Suspense>
                )}
                {page === "settings" && (
                  <Suspense fallback={<PageLoading label="Settings" />}>
                    <SettingsPage />
                  </Suspense>
                )}
              </div>
            </ErrorBoundary>
          </div>
        </main>
      </div>

      <StatusBar version={version} />

      <Toasts />
    </div>
  );
}

/** Real loading state for a lazily loaded page (represents real work). */
function PageLoading({ label }: { label: string }) {
  return (
    <div className="loading-inline page-loading" role="status" aria-live="polite">
      <span className="btn-spinner" aria-hidden />
      Loading {label}…
    </div>
  );
}
