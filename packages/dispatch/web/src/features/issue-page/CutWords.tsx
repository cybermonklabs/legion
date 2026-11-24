import { type ReactNode, type RefObject, useLayoutEffect, useRef, useState } from "react";

import { MoreWords, useClamp } from "./NodeRow";
import { FOCUSABLE, useOutOfTabOrder } from "./RowText";

/**
 * A node's words as a filter's own body draws them, cut as the outline cuts an agent's words
 * (the architect's rulings 1 and 6, the prototype's
 * `CUT_LINES`): at six lines where more than 20 words would be hidden, with "more · N words"
 * opening the rest in place, and whole while `whole` holds - the focused node, or one the
 * reader opened. The cut is measured by `NodeRow.tsx`'s `useClamp`, so a row of the outline and
 * a statement of the Spec or the Verification filter cut at one place.
 *
 * The control leaves the focus where it is, as the outline's own does: focusing the node would
 * draw the words whole under the pointer and move the control before the press ends. It and
 * every control the words hold are out of the tab order, as everything inside a row of the
 * outline is, unless `tabStops` holds: a view that no tree's keys reach keeps them in it, save
 * the ones the cut hides, which leave it while the words are cut (`useCutOutOfTabOrder`).
 */
export function CutWords({
  children,
  className,
  tabStops = false,
  whole,
}: {
  /** The words, drawn as the node's own text is (`RowText`). */
  children: ReactNode;
  /** The words' type and colour. */
  className: string;
  /** Whether the control and the words' own controls keep their place in the tab order. */
  tabStops?: boolean;
  /** Whether the words are drawn whole whatever their length. */
  whole: boolean;
}): ReactNode {
  const words = useRef<HTMLDivElement | null>(null);
  const clamp = useClamp(words, true);
  useOutOfTabOrder(words, !tabStops);
  const [opened, setOpened] = useState(false);
  const clamped = clamp !== undefined && !opened && !whole;
  useCutOutOfTabOrder(words, tabStops && clamped ? clamp.height : undefined);
  return (
    <>
      <div
        className={`relative ${className} ${clamped ? "overflow-clip" : ""}`}
        data-node-words=""
        ref={words}
        style={clamped ? { maxHeight: clamp.height } : undefined}
      >
        {children}
        {clamped && !clamp.pictures ? (
          <span
            aria-hidden="true"
            className="pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-[linear-gradient(transparent,var(--color-page-paper))]"
          />
        ) : null}
      </div>
      {clamp === undefined || whole ? null : (
        <MoreWords
          className="self-start"
          hidden={clamp.hidden}
          onToggle={() => setOpened((held) => !held)}
          opened={opened}
          tabStops={tabStops}
        />
      )}
    </>
  );
}

/**
 * Takes each control the cut hides out of the tab order while the words are cut, `cut` px
 * below the top of `box` (decision 85): one whose box starts at or below the cut is drawn
 * nowhere, so Tab would put the focus where no reader sees it before it reaches "more". It
 * watches the words as they change - a reference's link once its target answers, a pass that
 * puts a link back in the tab order, a new width - and a control the cut stops hiding takes
 * back its place. Once the words are whole (`cut` undefined), each control takes back the tab
 * order it had before. `useOutOfTabOrder` takes every control out and never puts one back,
 * which is what the outline needs and not what a view with tab stops does.
 */
function useCutOutOfTabOrder(box: RefObject<HTMLElement | null>, cut: number | undefined): void {
  useLayoutEffect(() => {
    const element = box.current;
    if (cut === undefined || element === null) return;
    // Each control taken out, with its own `tabindex` as it stood before, or null for none.
    const earlier = new Map<HTMLElement, string | null>();
    const giveBack = (control: HTMLElement): void => {
      const before = earlier.get(control);
      earlier.delete(control);
      if (before === undefined || before === null) control.removeAttribute("tabindex");
      else control.setAttribute("tabindex", before);
    };
    const sync = (): void => {
      const foot = element.getBoundingClientRect().top + cut;
      for (const control of element.querySelectorAll<HTMLElement>(FOCUSABLE)) {
        if (control.getBoundingClientRect().top < foot) {
          if (earlier.has(control)) giveBack(control);
          continue;
        }
        if (!earlier.has(control)) earlier.set(control, control.getAttribute("tabindex"));
        if (control.tabIndex !== -1) control.tabIndex = -1;
      }
    };
    sync();
    const watching = new MutationObserver(sync);
    watching.observe(element, {
      attributeFilter: ["tabindex"],
      attributes: true,
      childList: true,
      subtree: true,
    });
    const resized = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(sync);
    resized?.observe(element);
    return () => {
      watching.disconnect();
      resized?.disconnect();
      for (const control of [...earlier.keys()]) giveBack(control);
    };
  }, [box, cut]);
}
