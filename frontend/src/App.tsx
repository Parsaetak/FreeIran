import { useEffect, useState } from "react";
import { useAppStore, connectAppStore } from "./state/appStore";
import { useSourcesStore, useConfigsStore } from "./state/stores";
import { connectConnectionStore, useConnectionStore } from "./state/connectionStore";
import { useSettingsStore, effectiveReducedMotion } from "./state/settingsStore";
import { DashboardPage } from "./pages/Dashboard";
import { SourcesPage } from "./pages/Sources";
import { ConfigsPage } from "./pages/Configs";
import { ConnectionPage } from "./pages/Connection";
import { DiagnosticsPage } from "./pages/Diagnostics";
import { SettingsPage } from "./pages/Settings";
import { StatusBar } from "./components/StatusBar";
import { Toasts } from "./components/Toasts";
import { ErrorBoundary } from "./components/ErrorBoundary";
import {
  IconChevronLeft,
  IconChevronRight,
  IconConfigs,
  IconConnection,
  IconDashboard,
  IconDiagnostics,
  IconSettings,
  IconSources,
} from "./components/Icons";
import { call, diagnosticsService } from "./services";
import type { Page } from "./types/ui";

const NAV: Array<{ id: Page; label: string; icon: typeof IconDashboard }> = [
  { id: "dashboard", label: "Dashboard", icon: IconDashboard },
  { id: "sources", label: "Sources", icon: IconSources },
  { id: "configs", label: "Configurations", icon: IconConfigs },
  { id: "connection", label: "Connection", icon: IconConnection },
  { id: "diagnostics", label: "Diagnostics", icon: IconDiagnostics },
  { id: "settings", label: "Settings", icon: IconSettings },
];

export function App() {
  const [page, setPage] = useState<Page>("dashboard");
  const [collapsed, setCollapsed] = useState(false);
  const [version, setVersion] = useState("");
  const status = useAppStore((state) => state.status);

  useEffect(() => {
    const disposeAppState = connectAppStore();
    const disposeConnection = connectConnectionStore();

    return () => {
      disposeAppState();
      disposeConnection();
    };
  }, []);

  useEffect(() => {
    if (status !== "backend_unavailable") {
      void useSourcesStore.getState().load();
      void useConfigsStore.getState().loadPage(0);
      void useSettingsStore.getState().load();
      void useConnectionStore.getState().refreshBackends();
    }
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
            <ErrorBoundary onOpenDiagnostics={() => setPage("diagnostics")}>
              <div className="page" key={page}>
                {page === "dashboard" && <DashboardPage onNavigate={setPage} />}
                {page === "sources" && <SourcesPage />}
                {page === "configs" && <ConfigsPage />}
                {page === "connection" && <ConnectionPage />}
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
