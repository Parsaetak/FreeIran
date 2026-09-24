import { useRef, useState, useCallback, type ReactNode } from "react";
import { MenuSurface, type MenuAnchor } from "./MenuSurface";
import {
  IconAlert,
  IconCheck,
  IconPause,
  IconPlay,
  IconSignal,
  IconX,
} from "./Icons";

/** Empty/placeholder state with optional call-to-action. */
export function EmptyState({
  icon,
  title,
  hint,
  action,
}: {
  icon?: ReactNode;
  title: string;
  hint?: string;
  action?: ReactNode;
}) {
  return (
    <div className="empty-state">
      {icon && (
        <div className="empty-icon" aria-hidden>
          {icon}
        </div>
      )}
      <div className="empty-title">{title}</div>
      {hint && <div className="empty-hint">{hint}</div>}
      {action && <div className="toolbar center-row">{action}</div>}
    </div>
  );
}

/** Skeleton loading block for whole-page boot states. */
export function SkeletonPage({ tiles = 5, rows = 6 }: { tiles?: number; rows?: number }) {
  return (
    <div className="skeleton-panel" aria-hidden>
      <div className="skeleton title" />
      <div className="stat-grid">
        {Array.from({ length: tiles }, (_, i) => (
          <div key={i} className="skeleton tile" />
        ))}
      </div>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton row" />
      ))}
    </div>
  );
}

/** Compact segmented control (radio group semantics). */
export function SegmentedControl<T extends string>({
  options,
  value,
  onChange,
  ariaLabel,
}: {
  options: Array<{ value: T; label: string }>;
  value: T;
  onChange: (value: T) => void;
  ariaLabel: string;
}) {
  return (
    <div className="segmented" role="radiogroup" aria-label={ariaLabel}>
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          role="radio"
          aria-checked={value === option.value}
          className={`segmented-item ${value === option.value ? "active" : ""}`}
          onClick={() => onChange(option.value)}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}

/** Stat tile used across dashboard and diagnostics. */
export function StatTile({
  label,
  value,
  sub,
}: {
  label: string;
  value: ReactNode;
  sub?: ReactNode;
}) {
  return (
    <div className="stat">
      <div className="stat-value sm">{value}</div>
      <div className="stat-label">{label}</div>
      {sub !== undefined && <div className="stat-sub">{sub}</div>}
    </div>
  );
}

export type ConnectionUiState =
  | "disconnected"
  | "connecting"
  | "verifying"
  | "connected"
  | "disconnecting"
  | "failed";

/** Maps the backend state machine string onto a UI orb variant. */
export function connectionUiState(state: string): ConnectionUiState {
  switch (state) {
    case "connected_verified":
      return "connected";
    case "connected":
      return "connected";
    case "verifying":
      return "verifying";
    case "selecting":
    case "preparing":
    case "starting_core":
    case "waiting_for_ready":
      return "connecting";
    case "disconnecting":
      return "disconnecting";
    case "connection_failed":
      return "failed";
    default:
      return "disconnected";
  }
}

export const CONNECTION_STATE_LABELS: Record<string, string> = {
  disconnected: "Disconnected",
  selecting: "Selecting core",
  preparing: "Preparing config",
  starting_core: "Starting core",
  waiting_for_ready: "Waiting for core",
  connected: "Route established",
  verifying: "Verifying Internet",
  connected_verified: "Connected (verified)",
  disconnecting: "Disconnecting",
  connection_failed: "Connection failed",
};

/** Ordered connect steps shown while the state machine is busy. */
export const CONNECT_STEPS: Array<{ key: string; label: string }> = [
  { key: "selecting", label: "Select" },
  { key: "preparing", label: "Prepare" },
  { key: "starting_core", label: "Start core" },
  { key: "waiting_for_ready", label: "Ready" },
];

/** Index of the current step for a busy state (-1 when not busy). */
export function connectStepIndex(state: string): number {
  return CONNECT_STEPS.findIndex((step) => step.key === state);
}

/** Boot-sequence dots used during core startup / scanning. */
export function BootDots() {
  return (
    <span className="boot-dots" aria-hidden>
      <i />
      <i />
      <i />
    </span>
  );
}

/** Status orb icon per UI connection state. */
export function OrbIcon({ state }: { state: ConnectionUiState }) {
  if (state === "connected") return <IconSignal size={19} />;

  if (state === "failed") return <IconAlert size={19} />;

  if (state === "disconnected") return <IconPause size={17} />;

  if (state === "disconnecting") return <IconPause size={17} />;

  return <IconPlay size={17} />;
}

/** Small success/error badge for test results. */
export function ResultBadge({ ok, okLabel = "working", failLabel = "failed" }: { ok: boolean; okLabel?: string; failLabel?: string }) {
  return (
    <span className={`badge ${ok ? "success" : "error"}`}>
      {ok ? <IconCheck size={11} /> : <IconX size={11} />}
      {ok ? okLabel : failLabel}
    </span>
  );
}

export interface MenuItem {
  id: string;
  label: string;
  onSelect: () => void;
  disabled?: boolean;
  danger?: boolean;
  separatorBefore?: boolean;
}

/**
 * Compact overflow menu for secondary actions (v0.9.1). Closes on
 * outside click and Escape; items are real buttons so keyboard focus
 * and disabled states behave exactly like the rest of the UI.
 *
 * v0.9.15: the popup renders through the ONE shared portal surface
 * (MenuSurface) — viewport-aware placement, flip + clamp, keyboard
 * navigation and focus return for every menu in the application. The
 * old CSS-absolute dropdown could be clipped by overflow ancestors
 * and could poke past the window's right border.
 */
export function Menu({
  label,
  items,
  ariaLabel,
}: {
  label: ReactNode;
  items: MenuItem[];
  ariaLabel: string;
}) {
  const [open, setOpen] = useState(false);
  const [anchor, setAnchor] = useState<MenuAnchor | null>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  const openMenu = () => {
    const element = triggerRef.current;

    if (element) {
      const rect = element.getBoundingClientRect();

      setAnchor({
        kind: "rect",
        rect: { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom },
      });
    }

    setOpen(true);
  };

  const closeMenu = useCallback(() => setOpen(false), []);

  return (
    <div className="menu-wrap">
      <button
        ref={triggerRef}
        type="button"
        className="btn"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={ariaLabel}
        onClick={() => (open ? setOpen(false) : openMenu())}
      >
        {label}
      </button>

      {open && anchor && (
        <MenuSurface
          anchor={anchor}
          items={items}
          onClose={closeMenu}
          ariaLabel={ariaLabel}
          triggerRef={triggerRef}
          restoreFocusTo={triggerRef.current}
        />
      )}
    </div>
  );
}

/**
 * Collapsible raw-technical-details block (v0.9.1): the friendly
 * explanation stays visible; the raw error is one click away.
 */
export function TechDetails({ details }: { details: string }) {
  const [open, setOpen] = useState(false);

  if (!details) return null;

  return (
    <div className="tech-wrap">
      <button
        type="button"
        className="linklike"
        aria-expanded={open}
        onClick={() => setOpen((value) => !value)}
      >
        {open ? "Hide technical details" : "Show technical details"}
      </button>

      {open && (
        <pre className="tech-details">{details}</pre>
      )}
    </div>
  );
}
