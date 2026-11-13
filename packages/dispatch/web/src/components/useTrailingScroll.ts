import { useLayoutEffect, useState } from "react";

/**
 * Whether a strip that scrolls sideways has anywhere left to scroll, and whether it is scrolled
 * away from its start: which ends of it fade. Two strips read it. An issue's details rail wears
 * a trailing fade (`scrollFadeTrailing`) while there is more to the right. A control row's chips
 * fade at each end that has a chip out of view beyond it (`chipStripFade`), and draw a scroll
 * button over that end.
 *
 * The strip element is state rather than a ref, so this runs once per strip and not once per
 * render: three things change the answers and none of them is a render of the strip's owner -
 * the reader scrolling it, its box changing with the viewport, and its content arriving or
 * leaving (a label, a count, a chip the reader turned on). One listener and two observers cover
 * them, and the mutation observer re-observes children a render added.
 *
 * Hand the returned setter to the strip's `ref`. The two readings follow it: more to the right,
 * then scrolled away from the start.
 */
export function useTrailingScroll(): readonly [
  (node: HTMLElement | null) => void,
  boolean,
  boolean,
] {
  const [strip, setStrip] = useState<HTMLElement | null>(null);
  const [scrollable, setScrollable] = useState(false);
  const [scrolled, setScrolled] = useState(false);
  useLayoutEffect(() => {
    if (strip === null) {
      return;
    }
    // A fractional layout leaves scrollLeft a hair short of either end; one pixel of slack
    // keeps a strip scrolled to its end from wearing the fade, and one at its start from
    // reading as scrolled.
    const measure = () => {
      setScrollable(strip.scrollWidth - strip.clientWidth - strip.scrollLeft > 1);
      setScrolled(strip.scrollLeft > 1);
    };
    // happy-dom lays nothing out and has no `ResizeObserver` of its own; the unit suite installs
    // one, and a test DOM without it still measures once and on every scroll.
    const sizes = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    const observeAll = () => {
      sizes?.observe(strip);
      for (const child of strip.children) {
        sizes?.observe(child);
      }
    };
    observeAll();
    const contents = new MutationObserver(() => {
      observeAll();
      measure();
    });
    contents.observe(strip, { characterData: true, childList: true, subtree: true });
    strip.addEventListener("scroll", measure, { passive: true });
    measure();
    return () => {
      strip.removeEventListener("scroll", measure);
      sizes?.disconnect();
      contents.disconnect();
    };
  }, [strip]);
  return [setStrip, scrollable, scrolled] as const;
}
