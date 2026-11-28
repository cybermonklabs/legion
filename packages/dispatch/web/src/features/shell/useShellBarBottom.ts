import { type RefCallback, useCallback } from "react";

/** Where the compact navigation bar ends - the shell's top bar, or on the new issue page below
 *  `md` the issue's own row that holds Open navigation - in pixels from the top of the window, as
 *  a custom property on the document element: the widened side panel starts there
 *  (`wideInsetOf`). Unset wherever no such bar is drawn. */
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
    // The edge last written: every scroll of every scroller calls `publish`, and one that leaves
    // the edge where it was writes nothing.
    let written: number | undefined;
    const publish = () => {
      const bottom = Math.max(0, bar.getBoundingClientRect().bottom);
      if (bottom === written) return;
      written = bottom;
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
