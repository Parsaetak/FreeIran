import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import type { MenuItem } from "./common";

/**
 * v0.9.15 — ONE viewport-aware popup positioning mechanism for every
 * menu in the application.
 *
 * The previous implementation was fragmented: the shared <Menu> used a
 * CSS-absolute dropdown that any `overflow` ancestor could clip (and
 * that could poke past the window's right edge), while the Configs row
 * context-menu hard-coded its own geometry (240/340/348px guesses).
 * Both are replaced by a single portal-based surface:
 *
 *   1. anchors to the ACTUAL trigger (element rect or pointer point);
 *   2. renders through a portal to document.body — no clipping
 *      ancestor can touch it;
 *   3. positions in viewport coordinates (position: fixed);
 *   4. measures the REAL popup size after render (and re-measures via
 *      ResizeObserver for long menus);
 *   5. prefers opening downward / to the right;
 *   6. flips left when the right side lacks room;
 *   7. flips right when the left side lacks room;
 *   8. flips upward when below lacks room;
 *   9. flips downward when above lacks room;
 *   10. clamps inside a small viewport gutter when nothing fits.
 *
 * It stays correct across narrow windows, scrolling, resizing, long
 * menus, any menu width, high-DPI displays and all four viewport
 * edges. Escape-to-close, outside-click, keyboard navigation, focus
 * return and ARIA semantics are implemented ONCE, here.
 */

/** Small gutter kept between the popup and the viewport edges. */
const VIEWPORT_GUTTER = 8;

/** Gap between the anchor and the popup. */
const ANCHOR_GAP = 4;

export type MenuAnchor =
  | { kind: "point"; x: number; y: number }
  | { kind: "rect"; rect: { left: number; top: number; right: number; bottom: number } };

interface ComputedPosition {
  left: number;
  top: number;
}

/**
 * The placement algorithm — pure, so it is trivially predictable on
 * every edge of every viewport.
 */
export function computeMenuPosition(
  anchor: MenuAnchor,
  size: { width: number; height: number },
  viewport: { width: number; height: number },
): ComputedPosition {
  const maxX = viewport.width - VIEWPORT_GUTTER;
  const maxY = viewport.height - VIEWPORT_GUTTER;

  // ---- vertical: prefer below, flip above, clamp last ----------------
  const belowGap = anchor.kind === "rect" ? ANCHOR_GAP : 2;
  const topBelow = (anchor.kind === "rect" ? anchor.rect.bottom : anchor.y) + belowGap;
  const topAbove = (anchor.kind === "rect" ? anchor.rect.top : anchor.y) - ANCHOR_GAP - size.height;

  let top: number;

  if (topBelow + size.height <= maxY) {
    top = topBelow; // 5. downward fits
  } else if (topAbove >= VIEWPORT_GUTTER) {
    top = topAbove; // 8. flip upward
  } else {
    // 10. neither preferred placement fits: clamp with the gutter.
    // (A menu taller than the usable viewport overflows the bottom —
    // the surface scrolls; the top still respects the gutter.)
    top = Math.max(VIEWPORT_GUTTER, Math.min(topBelow, maxY - size.height));
  }

  // ---- horizontal: prefer rightward, flip leftward, clamp last -------
  const anchorLeft = anchor.kind === "rect" ? anchor.rect.left : anchor.x;
  const anchorRight = anchor.kind === "rect" ? anchor.rect.right : anchor.x;

  const rightward = anchorLeft; // menu's left edge at the anchor → extends right
  const leftward = anchorRight - size.width; // menu's right edge at the anchor → extends left

  let left: number;

  if (rightward + size.width <= maxX) {
    left = rightward; // 6. opens to the right
  } else if (leftward >= VIEWPORT_GUTTER) {
    left = leftward; // 6. flip to the left
  } else {
    // 7. both directions overflow: clamp inside the gutter (never
    // beyond the window border, whichever side the anchor hugs).
    left = Math.max(VIEWPORT_GUTTER, Math.min(rightward, maxX - size.width));
  }

  // An anchor hugging an edge (pointer at x=2) can drag a preferred
  // placement across the gutter: the final clamp keeps the WHOLE menu
  // inside the viewport without changing any placement that already
  // fits.
  const leftFloor = VIEWPORT_GUTTER;
  const leftCeiling = Math.max(leftFloor, maxX - size.width);
  left = Math.min(Math.max(left, leftFloor), leftCeiling);

  return { left, top };
}

/**
 * MenuSurface renders the popup content through a portal and owns
 * placement, dismissal, keyboard navigation and focus return.
 */
export function MenuSurface({
  anchor,
  items,
  onClose,
  ariaLabel,
  restoreFocusTo,
}: {
  anchor: MenuAnchor;
  items: MenuItem[];
  onClose: () => void;
  ariaLabel: string;
  restoreFocusTo?: HTMLElement | null;
}) {
  const rootRef = useRef<HTMLDivElement>(null);
  // First paint is OFF-SCREEN but measured: position: fixed at
  // (0,0) with visibility hidden until the real placement is known —
  // prevents a visible jump from the default corner.
  const [position, setPosition] = useState<ComputedPosition | null>(null);

  // ---- 4. measure + place -------------------------------------------
  const place = useCallback(() => {
    const element = rootRef.current;

    if (!element) return;

    const size = { width: element.offsetWidth, height: element.offsetHeight };
    const viewport = { width: window.innerWidth, height: window.innerHeight };

    setPosition(computeMenuPosition(anchor, size, viewport));
  }, [anchor]);

  useLayoutEffect(() => {
    place();
  }, [place]);

  // Long menus, changing labels or font scaling: keep the placement
  // honest while the measured size changes.
  useEffect(() => {
    const element = rootRef.current;

    if (!element || typeof ResizeObserver === "undefined") return;

    const observer = new ResizeObserver(() => place());

    observer.observe(element);

    return () => observer.disconnect();
  }, [place]);

  // ---- scrolling / resizing keep the anchor honest -------------------
  useEffect(() => {
    const reposition = () => place();

    window.addEventListener("resize", reposition);
    // Capture: scroll events on any scrolling ancestor do not bubble,
    // but they DO reach the document in the capture phase.
    window.addEventListener("scroll", reposition, true);

    return () => {
      window.removeEventListener("resize", reposition);
      window.removeEventListener("scroll", reposition, true);
    };
  }, [place]);

  // ---- outside click + Escape ----------------------------------------
  useEffect(() => {
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target as Node | null;

      if (rootRef.current && target && !rootRef.current.contains(target)) {
        event.preventDefault();
        onClose();
      }
    };

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.stopPropagation();
        onClose();
      }
    };

    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);

    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [onClose]);

  // ---- keyboard navigation + focus management -------------------------
  useEffect(() => {
    const root = rootRef.current;

    if (!root) return;

    const focusables = (): HTMLButtonElement[] =>
      Array.from(root.querySelectorAll<HTMLButtonElement>("[role='menuitem']:not(:disabled)"));

    // Initial focus lands on the first ENABLED item (menus that open
    // with everything disabled stay focusable themselves).
    const initial = focusables()[0];

    if (initial) {
      initial.focus();
    } else {
      root.focus();
    }

    const onKeyDown = (event: KeyboardEvent) => {
      const candidates = focusables();

      if (candidates.length === 0) return;

      const currentIndex = candidates.findIndex((c) => c === document.activeElement);

      let nextIndex = -1;

      switch (event.key) {
        case "ArrowDown":
          nextIndex = (currentIndex + 1 + candidates.length) % candidates.length;
          break;
        case "ArrowUp":
          nextIndex = (currentIndex - 1 + candidates.length) % candidates.length;
          break;
        case "Home":
          nextIndex = 0;
          break;
        case "End":
          nextIndex = candidates.length - 1;
          break;
        default:
          return;
      }

      event.preventDefault();
      candidates[nextIndex]?.focus();
    };

    root.addEventListener("keydown", onKeyDown);

    return () => root.removeEventListener("keydown", onKeyDown);
  }, [items]);

  // ---- focus return ----------------------------------------------------
  useEffect(() => {
    const previous = restoreFocusTo ?? document.activeElement as HTMLElement | null;

    return () => {
      previous?.focus?.();
    };
  }, [restoreFocusTo]);

  return createPortal(
    <div
      ref={rootRef}
      className="ctx-menu menu-surface"
      role="menu"
      aria-label={ariaLabel}
      tabIndex={-1}
      style={
        position
          ? { left: position.left, top: position.top, visibility: "visible" }
          : { left: 0, top: 0, visibility: "hidden" }
      }
    >
      {items.map((item) => (
        <Fragment key={item.id}>
          {item.separatorBefore && <div className="menu-sep" role="separator" />}

          <button
            type="button"
            role="menuitem"
            className={`menu-item ${item.danger ? "danger" : ""}`}
            disabled={item.disabled}
            onClick={() => {
              onClose();
              item.onSelect();
            }}
          >
            {item.label}
          </button>
        </Fragment>
      ))}
    </div>,
    document.body,
  );
}
