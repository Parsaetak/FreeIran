import { Component, type ErrorInfo, type ReactNode } from "react";
import { IconAlert, IconDiagnostics, IconRefresh } from "./Icons";

interface ErrorBoundaryProps {
  children: ReactNode;
  /** Opens the diagnostics page (where the runtime log lives). */
  onOpenDiagnostics?: () => void;
}

interface ErrorBoundaryState {
  error: Error | null;
}

/**
 * Guards the content area: a render failure degrades to a
 * professional error state instead of a blank window. Retry
 * re-mounts the subtree; details stay in the runtime log.
 */
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // The full story belongs in the runtime log, not in the face of
    // the user — keep the console record minimal.
    console.error("render error:", error.message, info.componentStack);
  }

  private retry = () => {
    this.setState({ error: null });
  };

  render() {
    const { error } = this.state;

    if (!error) return this.props.children;

    return (
      <div className="empty-state tall">
        <div className="empty-icon" aria-hidden>
          <IconAlert size={22} />
        </div>

        <div className="empty-title">This view failed to render</div>

        <p className="empty-hint">
          The {affectedSubsystem(error.message)} subsystem hit an unexpected
          error. The rest of the application keeps running, and the full
          details are in the runtime log (Diagnostics → Runtime log).
        </p>

        <p className="empty-hint mono small">{error.message || "unknown error"}</p>

        <div className="toolbar center-row">
          <button type="button" className="btn primary" onClick={this.retry}>
            <IconRefresh size={14} />
            Retry
          </button>

          {this.props.onOpenDiagnostics && (
            <button
              type="button"
              className="btn"
              onClick={this.props.onOpenDiagnostics}
            >
              <IconDiagnostics size={14} />
              Open diagnostics
            </button>
          )}
        </div>
      </div>
    );
  }
}

/** Rough subsystem hint from the error message (kept intentionally vague). */
function affectedSubsystem(message: string): string {
  const m = message.toLowerCase();

  if (m.includes("connection") || m.includes("core")) return "connection";
  if (m.includes("store") || m.includes("storage") || m.includes("chunk")) return "storage";
  if (m.includes("source") || m.includes("ingest")) return "source ingestion";
  if (m.includes("log")) return "logging";

  return "page";
}
