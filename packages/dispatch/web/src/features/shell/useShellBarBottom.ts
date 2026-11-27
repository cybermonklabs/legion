import { type RefCallback, useCallback } from "react";

/** Where the shell's compact top bar ends, in pixels from the top of the window, as a custom
 *  property on the document element: the widened side panel starts there (`wideInsetOf`), so the
 *  bar and its Menu stay in view above it. Unset wherever the shell draws no such bar. */
export const SHELL_BAR_BOTTOM_PROPERTY = "--shell-bar-bottom";

/**
 * The ref of the compact navigation bar, which publishes
 * `SHELL_BAR_BOTTOM_PROPERTY` for as long as the bar is in the page: in the commit that draws it,
 * so the first frame already carries the edge, then again whenever the bar's size changes (the
 * counts in it wrap onto a second row at some widths) and whenever the page scrolls, since the bar
 * scrolls away with the page: once it has, the edge is the window's top and the panel fills the
 * window. The property goes when the bar does.
 */
export function useShellBarBottom(): RefCallback<HTMLElement> {
  return useCallback((bar: HTMLElement | null) => {
    if (bar === null) return undefined;
    const root = document.documentElement;
    const publish = () => {
      const bottom = Math.max(0, bar.getBoundingClientRect().bottom);
      root.style.setProperty(SHELL_BAR_BOTTOM_PROPERTY, `${bottom}px`);
    };
    const sizes = new ResizeObserver(publish);
    sizes.observe(bar);
    window.addEventListener("scroll", publish, { capture: true, passive: true });
    publish();
    return () => {
      sizes.disconnect();
      window.removeEventListener("scroll", publish, true);
      root.style.removeProperty(SHELL_BAR_BOTTOM_PROPERTY);
    };
  }, []);
}
