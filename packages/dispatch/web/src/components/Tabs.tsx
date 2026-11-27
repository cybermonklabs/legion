import { type KeyboardEvent, type ReactNode, useRef } from "react";

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

/**
 * A tablist and, after it, `trailing` (a toolbar of the selected tab's). Its groups, in this
 * order: the tabs, and `trailing`. A tablist narrower than its tabs - the issue's ten tabs on a
 * phone, or beside the comment margin - wraps onto a further line rather than scrolling, so every
 * tab is in view at every width; `trailing` moves to the next line whole when it does not fit
 * beside the tabs. The tabs take the keyboard as one roving stop: the arrow keys, Home and End
 * select and focus a tab.
 */
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
    tabRefs.current[next.id]?.focus();
  };

  // The classes are `tab-classes.ts`, which the box the issue page keeps for this strip reads
  // too, so the strip lands in a box of its own size.
  return (
    <div className={tabStripClassName(compact)}>
      <div aria-label={ariaLabel} className={tabListClassName(compact)} role="tablist">
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
