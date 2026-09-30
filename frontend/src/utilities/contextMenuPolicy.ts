import { useEffect } from "react";

/**
 * v0.12.1 (§25) — the ONE application-wide right-click policy.
 *
 * The native browser/WebView context menu must never appear inside
 * FreeIran: right-click on configurations opens the application's own
 * MenuSurface (row-level handlers), and right-click anywhere else —
 * table whitespace, detail panel, configuration tabs, buttons, the
 * page background — gets NO menu instead of the foreign browser menu.
 *
 * Text fields are the single exception: their native editing menu
 * stays, so Ctrl+C / Ctrl+V / Ctrl+A clipboard behaviour and IME
 * editing are never broken (suppressing the menu must not suppress
 * editing).
 *
 * Mounted exactly once by the app shell; every surface inherits the
 * policy — no second context-menu engine exists anywhere.
 */
export function isTextEditable(target: EventTarget | null): boolean {
  const element = target as HTMLElement | null;

  return Boolean(
    element &&
      (element.tagName === "INPUT" ||
        element.tagName === "TEXTAREA" ||
        element.isContentEditable),
  );
}

export function useSuppressNativeContextMenu(): void {
  useEffect(() => {
    const onContextMenu = (event: MouseEvent) => {
      if (!isTextEditable(event.target)) {
        event.preventDefault();
      }
    };

    document.addEventListener("contextmenu", onContextMenu);

    return () => document.removeEventListener("contextmenu", onContextMenu);
  }, []);
}
