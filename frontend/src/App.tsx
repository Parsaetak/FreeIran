import { useEffect, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { useAppStore, connectAppStore } from "./state/appStore";
import { useSourcesStore, useConfigsStore } from "./state/stores";
import { connectConnectionStore } from "./state/connectionStore";
import { subscribeStartFlow, useStartFlowStore } from "./state/startflowStore";
import { useSettingsStore, effectiveReducedMotion } from "./state/settingsStore";
import { DashboardPage } from "./pages/Dashboard";
import { QuickConnectPage } from "./pages/QuickConnect";
import { SourcesPage } from "./pages/Sources";
import { ConfigsPage } from "./pages/Configs";
import { CoresPage } from "./pages/Cores";
import { ConnectionPage } from "./pages/Connection";
import { NetworkPage } from "./pages/Network";
import { DiagnosticsPage } from "./pages/Diagnostics";
import { SettingsPage } from "./pages/Settings";
import { StatusBar } from "./components/StatusBar";
import { BootProgress } from "./components/LoadState";
import { Toasts } from "./components/Toasts";
import { ErrorBoundary } from "./components/ErrorBoundary";
import {
  IconChevronLeft,
  IconChevronRight,
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

const NAV: Array<{ id: Page; label: string; icon: typeof IconDashboard }> = [
  // v0.9.8: Quick Connect first — the fastest user path and the
  // application's home connection surface.
  { id: "quick", label: "Quick Connect", icon: IconZap },
  { id: "dashboard", label: "Dashboard", icon: IconDashboard },
  { id: "configs", label: "Configurations", icon: IconConfigs },
  { id: "sources", label: "Sources", icon: IconSources },
  { id: "cores", label: "Cores", icon: IconCores },
  { id: "connection", label: "Connection", icon: IconPlug },
  { id: "network", label: "Network", icon: IconGlobe },
  { id: "diagnostics", label: "Diagnostics", icon: IconDiagnostics },
  { id: "settings", label: "Settings", icon: IconSettings },
];

export function App() {
  // v0.9.8: Quick Connect is the landing surface — the application's
  // home connection action.
  const [page, setPage] = useState<Page>("quick");
  const [collapsed, setCollapsed] = useState(false);
  const [version, setVersion] = useState("");
  const status = useAppStore((state) => state.status);
  const bootPhase = useAppStore((state) => state.bootPhase);
  const bootTimings = useAppStore((state) => state.bootTimings);

  useEffect(() => {
    const disposeAppState = connectAppStore();
    const disposeConnection = connectConnectionStore();

    // v0.9.6: start-flow broadcasts (detect → discover → test →
    // rank → connect → verify) keep the Smart Start panel live.
    const disposeStartFlow = subscribeStartFlow();

    // Initial flow status + environment analysis for display.
    void useStartFlowStore.getState().refresh();
    void useStartFlowStore.getState().refreshEnvironment();

    // Startup lifecycle (v0.9.4): the first mounted frame reports
    // ui_ready to the backend — the real "interface usable" moment on
    // the boot telemetry scale. One-shot; failures are irrelevant.
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

  // Initial data load. Fetch ONCE per backend lifetime: `status` flips
  // on every `freeiran:state` broadcast (loading → ready, and
  // ready → ingesting → ready per ingestion cycle), and re-running
  // this bundle per flip issued ~4 duplicate service calls per
  // transition — a rolling fetch storm exactly while the engine is
  // busy ingesting. The ref gates the load to the first reachable
  // backend; a later `backend_unavailable` resets it so recovery
  // re-fetches stale stores.
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
            {NAV.map((item) => {
              const Icon = item.icon;

              return (
                <button
                  key={item.id}
                  type="button"
                  className={`nav-item ${page === item.id ? "active" : ""}`}
                  aria-current={page === item.id ? "page" : undefined}
                  title={collapsed ? item.label : undefined}
                  onClick={() => setPage(item.id)}
                >
                  <span className="nav-icon" aria-hidden>
                    <Icon size={17} />
                  </span>
                  <span className="nav-label">{item.label}</span>
                </button>
              );
            })}
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
            {/* Real boot progress (§10): driven by backend telemetry,
                disappears the moment the app is ready. */}
            <BootProgress phase={bootPhase} timings={bootTimings} />
            <ErrorBoundary onOpenDiagnostics={() => setPage("diagnostics")}>
              <div className="page" key={page}>
                {page === "quick" && <QuickConnectPage onNavigate={setPage} />}
                {page === "dashboard" && <DashboardPage onNavigate={setPage} />}
                {page === "sources" && <SourcesPage />}
                {page === "configs" && <ConfigsPage />}
                {page === "cores" && <CoresPage />}
                {page === "connection" && <ConnectionPage />}
                {page === "network" && <NetworkPage />}
                {page === "diagnostics" && <DiagnosticsPage />}
                {page === "settings" && <SettingsPage />}
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
