import { useLayoutEffect, useState } from "react";

import { observeStrip } from "../hooks/observe-strip";
import { scrollFadeTrailing } from "../theme/classes";

/** What `useScrollEnds` hands a strip that scrolls sideways. */
interface ScrollEnds {
  /** The setter to hand to the strip's `ref`. */
  readonly ref: (node: HTMLElement | null) => void;
  /** The trailing fade (`scrollFadeTrailing`) while the strip has anywhere left to scroll -
   *  something is out of view to the right - else nothing: the cue the strip puts in its class
   *  list. */
  readonly fade: string;
}

/**
 * Whether a strip that scrolls sideways has something out of view past its trailing end: whether
 * it wears the trailing fade. An issue's details rail, the tablist of `components/Tabs.tsx` and
 * the scroll box of each Steps table (`features/steps/StepsTab.tsx`) wear it (`fade`), so the app
 * shows a cut-off strip one way.
 *
 * The strip element is state rather than a ref, so this runs once per strip and not once per
 * render: three things change the answer and none of them is a render of the strip's owner - the
 * reader scrolling it, its box changing with the viewport, and its content arriving or leaving (a
 * label, a count, a tab). One listener covers the first and `observeStrip` the other two.
 */
export function useScrollEnds(): ScrollEnds {
  const [node, ref] = useState<HTMLElement | null>(null);
  const [trailing, setTrailing] = useState(false);
  useLayoutEffect(() => {
    if (node === null) {
      return;
    }
    // A fractional layout leaves scrollLeft a hair short of the end; one pixel of slack keeps a
    // strip scrolled to its end from wearing the trailing fade.
    const measure = () => {
      setTrailing(node.scrollWidth - node.clientWidth - node.scrollLeft > 1);
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
  }, [node]);
  return { fade: trailing ? scrollFadeTrailing : "", ref };
}
