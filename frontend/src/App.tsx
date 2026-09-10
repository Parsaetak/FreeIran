import { useEffect, useState } from "react";
import { useAppStore, connectAppStore } from "./state/appStore";
import { useSourcesStore, useConfigsStore } from "./state/stores";
import { DashboardPage } from "./pages/Dashboard";
import { SourcesPage } from "./pages/Sources";
import { ConfigsPage } from "./pages/Configs";
import { DiagnosticsPage } from "./pages/Diagnostics";
import { StatusBar } from "./components/StatusBar";
import type { Page } from "./types/ui";

const NAV: Array<{ id: Page; label: string }> = [
  { id: "dashboard", label: "Dashboard" },
  { id: "sources", label: "Sources" },
  { id: "configs", label: "Configurations" },
  { id: "diagnostics", label: "Diagnostics" },
];

export function App() {
  const [page, setPage] = useState<Page>("dashboard");
  const status = useAppStore((state) => state.status);

  useEffect(() => connectAppStore(), []);

  useEffect(() => {
    if (status !== "backend_unavailable") {
      void useSourcesStore.getState().load();
      void useConfigsStore.getState().loadPage(0);
    }
  }, [status]);

  return (
    <div className="shell">
      <div className="shell-body">
        <nav className="sidebar">
          <div className="brand">
            <span className="brand-dot" />
            FreeIran
          </div>

          {NAV.map((item) => (
            <button
              key={item.id}
              className={`nav-item ${page === item.id ? "active" : ""}`}
              onClick={() => setPage(item.id)}
            >
              {item.label}
            </button>
          ))}
        </nav>

        <main className="content">
          {page === "dashboard" && <DashboardPage />}
          {page === "sources" && <SourcesPage />}
          {page === "configs" && <ConfigsPage />}
          {page === "diagnostics" && <DiagnosticsPage />}
        </main>
      </div>

      <StatusBar />
    </div>
  );
}
