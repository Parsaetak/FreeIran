import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  BOOT_PHASES,
  bootPhaseIndex,
  bootPhaseLabel,
} from "../state/loading";
import { IconAlert } from "./Icons";

/**
 * useDeferredSkeleton — the unified skeleton policy as a hook.
 *
 * Returns true only while `loading` has been continuously true for at
 * least the defer threshold (180 ms): fast loads render content with
 * no indicator, slow loads get a single calm skeleton transition. No
 * timers outlive the loading state; the timer is cancelled the moment
 * loading ends so nothing repaints in the background.
 */
export function useDeferredSkeleton(
  loading: boolean,
  thresholdMs = 180,
): boolean {
  const [visible, setVisible] = useState(false);
  const sinceRef = useRef<number | null>(null);

  useEffect(() => {
    if (!loading) {
      sinceRef.current = null;
      setVisible(false);

      return;
    }

    sinceRef.current = Date.now();

    const elapsed = () => Date.now() - (sinceRef.current ?? Date.now());

    if (elapsed() >= thresholdMs) {
      setVisible(true);

      return;
    }

    const timer = window.setTimeout(() => setVisible(true), thresholdMs);

    return () => window.clearTimeout(timer);
  }, [loading, thresholdMs]);

  return visible;
}

/**
 * LoadBoundary — the unified page/panel loading state (§8).
 *
 *   loading → deferred skeleton (real content shows through for fast loads)
 *   error   → calm error panel with an explicit retry action
 *   else    → children
 *
 * Replaces ad-hoc per-page spinners; one component owns the policy so
 * every surface behaves identically.
 */
export function LoadBoundary({
  loading,
  error,
  onRetry,
  children,
  rows = 6,
}: {
  loading: boolean;
  error?: string | null;
  onRetry?: () => void;
  children: ReactNode;
  rows?: number;
}) {
  const showSkeleton = useDeferredSkeleton(loading);

  if (error) {
    return (
      <div className="load-error" role="alert">
        <span className="load-error-icon" aria-hidden>
          <IconAlert size={18} />
        </span>
        <div className="load-error-body">
          <div className="load-error-title">Something went wrong</div>
          <div className="load-error-detail">{error}</div>
        </div>
        {onRetry && (
          <button type="button" className="btn" onClick={onRetry}>
            Retry
          </button>
        )}
      </div>
    );
  }

  if (loading && showSkeleton) {
    return (
      <div className="skeleton-panel" aria-hidden>
        {Array.from({ length: rows }, (_, i) => (
          <div key={i} className="skeleton row" />
        ))}
      </div>
    );
  }

  if (loading) {
    // Inside the defer window: render nothing disruptive yet.
    return null;
  }

  return <>{children}</>;
}

/**
 * BootProgress — real startup progress (§10), driven by the backend's
 * boot_phase telemetry. Determinate over the known phase scale, with
 * the live phase label. Hidden once the app reaches ready; a phase
 * the frontend does not know (newer backend) still renders as raw
 * text instead of breaking.
 */
export function BootProgress({
  phase,
  timings,
}: {
  phase: string | null;
  timings?: Record<string, number> | null;
}) {
  if (!phase || phase === "ready") return null;

  const index = bootPhaseIndex(phase);
  const denominator = BOOT_PHASES.length - 1;
  const percent =
    index < 0 ? 0 : Math.min(100, Math.round((index / denominator) * 100));

  const elapsed = timings?.[phase];

  return (
    <div
      className="boot-progress"
      role="status"
      aria-label={`Starting: ${bootPhaseLabel(phase)}`}
    >
      <div className="boot-progress-head">
        <span className="boot-progress-label">{bootPhaseLabel(phase)}</span>
        {typeof elapsed === "number" && (
          <span className="boot-progress-ms">{elapsed} ms</span>
        )}
      </div>
      <div className="boot-progress-track" aria-hidden>
        <div
          className="boot-progress-fill"
          style={{ transform: `scaleX(${percent / 100})` }}
        />
      </div>
    </div>
  );
}
