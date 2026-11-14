import { useLayoutEffect, useState } from "react";

/** What `useScrollEnds` reads of one strip that scrolls sideways. */
interface ScrollEnds {
  /** The setter to hand to the strip's `ref`. */
  readonly ref: (node: HTMLElement | null) => void;
  /** The strip element the setter was last handed, or null before it is drawn. */
  readonly node: HTMLElement | null;
  /** Whether the strip is scrolled away from its start: something is out of view to the left. */
  readonly leading: boolean;
  /** Whether the strip has anywhere left to scroll: something is out of view to the right. */
  readonly trailing: boolean;
}

/**
 * Which ends of a strip that scrolls sideways have something out of view beyond them: which ends
 * of it fade. Two strips read it. An issue's details rail wears a trailing fade
 * (`scrollFadeTrailing`) while there is more to the right. A control row's chips fade at each
 * end that has a chip out of view beyond it (`chipStripFade`), and draw a scroll button over
 * that end, which scrolls the strip `node`.
 *
 * The strip element is state rather than a ref, so this runs once per strip and not once per
 * render: three things change the answers and none of them is a render of the strip's owner -
 * the reader scrolling it, its box changing with the viewport, and its content arriving or
 * leaving (a label, a count, a chip the reader turned on). One listener and two observers cover
 * them, and the mutation observer re-observes children a render added.
 */
export function useScrollEnds(): ScrollEnds {
  const [node, ref] = useState<HTMLElement | null>(null);
  const [leading, setLeading] = useState(false);
  const [trailing, setTrailing] = useState(false);
  useLayoutEffect(() => {
    if (node === null) {
      return;
    }
    // A fractional layout leaves scrollLeft a hair short of either end; one pixel of slack
    // keeps a strip scrolled to its end from wearing the trailing fade, and one at its start
    // from reading as scrolled.
    const measure = () => {
      setTrailing(node.scrollWidth - node.clientWidth - node.scrollLeft > 1);
      setLeading(node.scrollLeft > 1);
    };
    // happy-dom lays nothing out and has no `ResizeObserver` of its own; the unit suite installs
    // one, and a test DOM without it still measures once and on every scroll.
    const sizes = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    const observeAll = () => {
      sizes?.observe(node);
      for (const child of node.children) {
        sizes?.observe(child);
      }
    };
    observeAll();
    const contents = new MutationObserver(() => {
      observeAll();
      measure();
    });
    contents.observe(node, { characterData: true, childList: true, subtree: true });
    node.addEventListener("scroll", measure, { passive: true });
    measure();
    return () => {
      node.removeEventListener("scroll", measure);
      sizes?.disconnect();
      contents.disconnect();
    };
  }, [node]);
  return { leading, node, ref, trailing };
}
