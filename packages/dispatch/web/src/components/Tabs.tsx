import { type KeyboardEvent, type ReactNode, useRef } from "react";

import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  borderDefault,
  borderTransparent,
  focusVisibleRing,
  surfaceMutedBg,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { Pill } from "./Pill";

export interface TabDefinition<Id extends string> {
  id: Id;
  label: string;
  /** Shown after the label: as a small pill, or under the label on a phone in a `stacked`
   *  tablist. The tab's accessible name is `<label> <count>`, unless `name` gives it. */
  count?: number;
  /** The tab's accessible name where it says more than its label and its count, as a view tab's
   *  "Open, 3 issues, changed" does. It starts with the words the tab shows, in their order, or
   *  a voice user who says them cannot press it (WCAG 2.5.3). */
  name?: string;
  /** The view the tab opens is changed: a dot follows the label, on the label's line. */
  changed?: boolean;
}

/** The count of a `stacked` tab: smaller than the shared pill, so the Issues tab's view tabs and
 *  a reader's saved ones keep to one row. */
const stackedCount = `rounded-full px-1 text-[11px] font-medium tabular-nums md:py-0.5 ${surfaceMutedBg} ${textSecondaryOnCanvas}`;

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
  panels = true,
  stacked = false,
  tabs,
  trailing,
}: {
  activeTab: Id;
  ariaLabel: string;
  compact?: boolean;
  idPrefix: string;
  onBeforeTabChange?: (current: Id) => void;
  onSelect: (tab: Id) => void;
  /** Each tab names the panel it shows (`aria-controls`, `<idPrefix>-<id>-panel`). Tabs that
   *  show one panel for every tab, as the Issues tab's view tabs show one table, name none. */
  panels?: boolean;
  /** The Issues tab's view tabs: on a phone each tab stands its count under its label. The tabs
   *  wrap onto more lines at every width, as every tablist here does, so every tab is in view
   *  whatever the counts. */
  stacked?: boolean;
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

  const tabMinHeight = compact ? "min-h-11 md:min-h-9" : "min-h-11";
  const tabPadding = compact ? "px-1.5 md:px-2" : "px-3";
  const tabGap = compact ? "gap-0 md:gap-1" : "gap-1";

  return (
    <div
      className={
        stacked
          ? `flex flex-wrap items-center border-b md:flex-nowrap ${borderDefault}`
          : `${compact ? "mb-0" : "mb-3"} flex flex-wrap items-center gap-1 border-b ${borderDefault}`
      }
    >
      <div
        aria-label={ariaLabel}
        className={
          stacked ? "flex min-w-0 flex-wrap items-center" : `flex min-w-0 flex-wrap ${tabGap}`
        }
        role="tablist"
      >
        {tabs.map((tab) => {
          const selected = activeTab === tab.id;
          const dot = tab.changed ? (
            <span
              aria-hidden
              className={`size-1.5 rounded-full bg-current ${textPrimaryOnCanvas}`}
              data-view-changed=""
            />
          ) : null;
          return (
            <button
              aria-controls={panels ? `${idPrefix}-${tab.id}-panel` : undefined}
              // The name has to contain the visible text, word for word, or a voice user who
              // says what the tab reads cannot press it (WCAG 2.5.3, axe's
              // `label-content-name-mismatch`). The pill's count is part of that text, and
              // parentheses around it are dropped before the two are compared, so the name
              // spells the count plainly.
              aria-label={
                tab.name ?? (tab.count === undefined ? undefined : `${tab.label} ${tab.count}`)
              }
              aria-selected={selected}
              // A stacked tab stands its count under its label on a phone; from `md` up the count
              // follows the label on its line, and the selected tab's label is bold.
              className={
                stacked
                  ? `inline-flex min-h-11 shrink-0 flex-col items-center justify-center border-b-2 px-1.5 py-1 text-[13px] leading-tight whitespace-nowrap md:min-h-9 md:flex-row md:gap-1 md:px-1 md:py-2 ${focusVisibleRing} ${
                      selected
                        ? `md:font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
                        : `${borderTransparent} ${textSecondaryOnCanvas}`
                    }`
                  : selected
                    ? `${tabMinHeight} ${tabPadding} inline-flex shrink-0 items-center gap-1.5 border-b-2 py-2 text-sm font-semibold whitespace-nowrap ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
                    : `${tabMinHeight} ${tabPadding} inline-flex shrink-0 items-center gap-1.5 py-2 text-sm whitespace-nowrap ${textSecondaryOnCanvas}`
              }
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
              tabIndex={selected ? 0 : -1}
              type="button"
            >
              {/* A stacked tab's label keeps the dot on its line, over the count on a phone. */}
              {stacked ? (
                <span className="inline-flex items-center gap-1">
                  {tab.label}
                  {dot}
                </span>
              ) : (
                <>
                  {tab.label}
                  {dot}
                </>
              )}
              {/* The space is the one between the label and the count: a flex container does
                  not render a white-space-only child, so this changes no pixel, and without it
                  the label and the count read as one word against the name above. */}
              {tab.count === undefined ? null : (
                <>
                  {" "}
                  {stacked ? (
                    <span className={stackedCount}>{tab.count}</span>
                  ) : (
                    <Pill>{tab.count}</Pill>
                  )}
                </>
              )}
            </button>
          );
        })}
      </div>
      {trailing}
    </div>
  );
}
