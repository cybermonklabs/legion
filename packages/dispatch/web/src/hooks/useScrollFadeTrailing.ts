import { useLayoutEffect, useState } from "react";

import { scrollFadeTrailing } from "../theme/classes";
import { observeStrip } from "./observe-strip";

/**
 * The cue that a strip scrolling sideways holds more than it shows: `scrollFadeTrailing`, worn
 * only while the strip has somewhere left to scroll. Give the strip `ref` and put `fade` in its
 * class list. The issue header's details rail, the tablist of `components/Tabs.tsx` and the
 * scroll box of each Steps table (`features/steps/StepsTab.tsx`) wear it, so the app shows a
 * cut-off strip one way.
 *
 * The strip is state rather than a ref so the effect runs once per strip, not once per render:
 * three things change the answer and none of them is a render of the strip's owner - the reader
 * scrolling the strip, the strip's box changing with the viewport, and its content arriving (a
 * label, a count, a title). One listener covers the first and `observeStrip` the other two.
 */
export function useScrollFadeTrailing(): {
  fade: string;
  ref: (strip: HTMLElement | null) => void;
} {
  const [strip, setStrip] = useState<HTMLElement | null>(null);
  const [scrollsOn, setScrollsOn] = useState(false);
  useLayoutEffect(() => {
    if (strip === null) {
      return;
    }
    // A fractional layout leaves scrollLeft a hair short of the end; one pixel of slack keeps a
    // strip scrolled to its end from wearing the fade.
    const measure = () =>
      setScrollsOn(strip.scrollWidth - strip.clientWidth - strip.scrollLeft > 1);
    const stop = observeStrip(strip, measure, {
      characterData: true,
      childList: true,
      subtree: true,
    });
    strip.addEventListener("scroll", measure, { passive: true });
    measure();
    return () => {
      strip.removeEventListener("scroll", measure);
      stop();
    };
  }, [strip]);
  return { fade: scrollsOn ? scrollFadeTrailing : "", ref: setStrip };
}
