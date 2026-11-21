import { useLayoutEffect, useState } from "react";

import { observeStrip } from "../hooks/observe-strip";
import { scrollFadeTrailing } from "../theme/classes";

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
  /** The trailing fade (`scrollFadeTrailing`) while `trailing`, else nothing: the cue a strip
   *  whose only fade is its trailing one puts in its class list. */
  readonly fade: string;
}

/**
 * Which ends of a strip that scrolls sideways have something out of view beyond them: which ends
 * of it fade. An issue's details rail, the tablist of `components/Tabs.tsx` and the scroll box
 * of each Steps table (`features/steps/StepsTab.tsx`) wear the trailing fade alone (`fade`), so
 * the app shows a cut-off strip one way. A control row's chips fade at each end
 * that has a chip out of view beyond it (`chipStripFade`), and draw a scroll button over that
 * end, which scrolls the strip `node`.
 *
 * The strip element is state rather than a ref, so this runs once per strip and not once per
 * render: three things change the answers and none of them is a render of the strip's owner -
 * the reader scrolling it, its box changing with the viewport, and its content arriving or
 * leaving (a label, a count, a chip the reader turned on). One listener covers the first and
 * `observeStrip` the other two.
 */
export function useScrollEnds({
  ends = "both",
}: {
  /** Which ends the caller reads. A strip whose only fade is its trailing one reads `trailing`
   *  alone, so a scroll that changes only how far it is from its start does not draw its owner
   *  again; `leading` then stays false. */
  readonly ends?: "both" | "trailing";
} = {}): ScrollEnds {
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
      if (ends === "both") setLeading(node.scrollLeft > 1);
    };
    const stop = observeStrip(node, measure, {
      characterData: true,
      childList: true,
      subtree: true,
    });
    node.addEventListener("scroll", measure, { passive: true });
    measure();
    return () => {
      node.removeEventListener("scroll", measure);
      stop();
    };
  }, [ends, node]);
  return { fade: trailing ? scrollFadeTrailing : "", leading, node, ref, trailing };
}
