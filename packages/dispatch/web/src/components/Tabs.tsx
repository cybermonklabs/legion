import { type KeyboardEvent, type ReactNode, useEffect, useRef } from "react";

import { observeStrip } from "../hooks/observe-strip";
import { useScrollFadeTrailing } from "../hooks/useScrollFadeTrailing";
import { scrollFadeTrailingPx } from "../theme/classes";
import { Pill } from "./Pill";
import {
  tabClassName,
  tabCountClassName,
  tabListClassName,
  tabStripClassName,
} from "./tab-classes";

export interface TabDefinition<Id extends string> {
  id: Id;
  label: string;
  /** Shown as a small pill after the label; the tab's accessible name is `<label> <count>`. */
  count?: number;
}

/** The DOM id of the tablist button for `id` under `idPrefix`. A keyboard binding that
 *  activates a tab finds its button by this, so the format has one owner. */
export function tabButtonId(idPrefix: string, id: string): string {
  return `${idPrefix}-${id}-tab`;
}

/** Puts the tablist where its current layout alone puts `node`, the selected tab or the one a
 *  keystroke focused. A tablist may be narrower than its tabs (a phone, or the issue page beside
 *  its comment margin), and that tab must be fully visible, which Chromium's `focus()` alone does
 *  not guarantee. Only the tablist scrolls: `scrollIntoView` would also move the page, undoing the
 *  per-tab scroll position IssuePage restores when a panel comes back.
 *
 *  While tabs lie past its trailing edge, the tablist's last `scrollFadeTrailingPx` wear the
 *  trailing fade, so a tab is whole only clear of it. The tablist goes to the smallest scroll that
 *  shows the tab whole and clear of the fade: the start when the tab fits there, the tab's own left
 *  edge when the tab is wider than what the fade leaves, and the end of the scroll, where the fade
 *  is off, when the tab is too near the end for that.
 *
 *  The scroll is set, not moved by a delta, so where the tablist stops does not depend on the
 *  layouts it passed through: a narrower moment that scrolled the tab clear of the fade leaves
 *  nothing behind once the tablist widens again. For the same reason a sideways scroll the reader
 *  made is put back to that scroll whenever this runs: when the selected tab changes, when the
 *  tablist or a tab changes size (a live count), when a tab is added or removed, and on a
 *  keystroke. A scroll alone does not run it, so the reader's scroll stays until one of those. */
function settleTab(node: HTMLElement | null | undefined): void {
  const list = node?.parentElement;
  if (node === null || node === undefined || list === null || list === undefined) {
    return;
  }
  const listLeft = list.getBoundingClientRect().left;
  const box = node.getBoundingClientRect();
  // The tab's edges from the start of the scroll, whatever the tablist is scrolled to now.
  const left = box.left - listLeft + list.scrollLeft;
  const right = box.right - listLeft + list.scrollLeft;
  list.scrollLeft = Math.min(Math.max(0, right - (list.clientWidth - scrollFadeTrailingPx)), left);
}

export function Tabs<Id extends string>({
  activeTab,
  ariaLabel,
  compact = false,
  idPrefix,
  onBeforeTabChange,
  onSelect,
  tabs,
  trailing,
}: {
  activeTab: Id;
  ariaLabel: string;
  compact?: boolean;
  idPrefix: string;
  onBeforeTabChange?: (current: Id) => void;
  onSelect: (tab: Id) => void;
  trailing?: ReactNode;
  tabs: readonly TabDefinition<Id>[];
}): ReactNode {
  const tabRefs = useRef<Partial<Record<Id, HTMLButtonElement | null>>>({});
  const pointerCapturedScroll = useRef(false);
  const tablistFade = useScrollFadeTrailing();

  const selectTab = (tab: Id) => {
    if (!pointerCapturedScroll.current) {
      onBeforeTabChange?.(activeTab);
    }
    pointerCapturedScroll.current = false;
    onSelect(tab);
  };
  const onTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>, current: Id) => {
    const index = tabs.findIndex((tab) => tab.id === current);
    let nextIndex: number | undefined;
    if (event.key === "ArrowRight") {
      nextIndex = (index + 1) % tabs.length;
    } else if (event.key === "ArrowLeft") {
      nextIndex = (index - 1 + tabs.length) % tabs.length;
    } else if (event.key === "Home") {
      nextIndex = 0;
    } else if (event.key === "End") {
      nextIndex = tabs.length - 1;
    }
    if (nextIndex === undefined) {
      return;
    }
    event.preventDefault();
    const next = tabs[nextIndex];
    if (next === undefined) {
      return;
    }
    selectTab(next.id);
    const node = tabRefs.current[next.id];
    node?.focus();
    // Settled in the reader's own keystroke too, for a key that changes no selection and so runs
    // no effect below: Home or End onto the tab already selected after the reader scrolled the
    // strip away, an arrow key in a strip of one tab, or a key whose selection the page refuses or
    // defers. Without this the tab could stay out of view or cut off: `focus()` does not scroll a
    // tab that already has focus, and leaves a partly hidden one where it is. A key that changes
    // the selection has the effect settle the same tab again.
    settleTab(node);
  };

  // Settle the selected tab when it is chosen, when the list narrows or widens (the comment margin
  // opens, or the window changes), when a tab's width changes (a live count grows), and when a tab
  // is added or taken away (the project's Architecture tab arriving before Issues). A preceding
  // tab can move the selection under the fade without changing the list's or selected tab's
  // width. Each of these puts back a sideways scroll the reader made: the strip goes to where its
  // layout puts the selected tab. Any scroll alone - a wheel, a touch, a key, find-in-page or an
  // assistive-technology focus move - is none of them, so it stays until one comes.
  //
  // The settle on choosing is in a frame of its own, never in the commit that mounted or
  // switched the tab: the two rectangles are the first thing on the page to ask for a layout, so
  // inside the commit they force the whole page's style and layout to be computed there. On a
  // phone at 4x CPU throttling that was 170-200 ms of the one task that mounts an issue page - a
  // task already over the 500 ms a reader feels as a freeze (dispatch://INFRA-341, R11). An
  // animation frame reads the layout the browser was about to compute anyway, and scrolls the
  // tablist before that frame is painted, so nothing a reader sees moves later than it did. A
  // tab the reader reached from the keyboard is still settled in their own keystroke
  // (`onTabKeyDown`), where the measurement is all the task holds.
  useEffect(() => {
    const selected = tabRefs.current[activeTab];
    const settle = () => settleTab(selected);
    const frame = requestAnimationFrame(settle);
    const list = selected?.parentElement;
    if (list === null || list === undefined) {
      return () => cancelAnimationFrame(frame);
    }
    const stop = observeStrip(list, settle, { childList: true });
    return () => {
      cancelAnimationFrame(frame);
      stop();
    };
  }, [activeTab]);

  // Compact tabs sit tighter on phones, which is what keeps the shorter tablists on one line.
  // The issue's ten tabs, one carrying a count pill, no longer fit a 390 px viewport, nor the
  // issue page at 1440 px beside its comment margin: there the tablist scrolls sideways, as it
  // already did at 360 px, wears the trailing fade while tabs lie past its edge, and `settleTab`
  // keeps the selected tab, and the one a keystroke focused, fully visible and clear of the fade.
  // Every tab stays reachable and keeps its 44 px target. The classes themselves are
  // `tab-classes.ts`, which the box the issue page keeps for this strip reads too.
  return (
    <div className={tabStripClassName(compact)}>
      <div
        aria-label={ariaLabel}
        className={`${tabListClassName(compact)} ${tablistFade.fade}`}
        ref={tablistFade.ref}
        role="tablist"
      >
        {tabs.map((tab) => (
          <button
            aria-controls={`${idPrefix}-${tab.id}-panel`}
            // The name has to contain the visible text, word for word, or a voice user who says
            // what the tab reads cannot press it (WCAG 2.5.3, axe's
            // `label-content-name-mismatch`). The pill's count is part of that text, and
            // parentheses around it are dropped before the two are compared, so the name spells
            // the count plainly.
            aria-label={tab.count === undefined ? undefined : `${tab.label} ${tab.count}`}
            aria-selected={activeTab === tab.id}
            className={tabClassName(compact, activeTab === tab.id)}
            id={tabButtonId(idPrefix, tab.id)}
            key={tab.id}
            onClick={() => selectTab(tab.id)}
            onKeyDown={(event) => onTabKeyDown(event, tab.id)}
            onPointerDown={() => {
              onBeforeTabChange?.(activeTab);
              pointerCapturedScroll.current = true;
            }}
            ref={(node) => {
              tabRefs.current[tab.id] = node;
            }}
            role="tab"
            tabIndex={activeTab === tab.id ? 0 : -1}
            type="button"
          >
            {tab.label}
            {/* The space is the one between the label and the count: a flex container does not
                render a white-space-only child, so this changes no pixel, and without it the
                label and the count read as one word against the name above. */}
            {tab.count === undefined ? null : (
              <>
                {" "}
                <Pill className={tabCountClassName}>{tab.count}</Pill>
              </>
            )}
          </button>
        ))}
      </div>
      {trailing}
    </div>
  );
}
