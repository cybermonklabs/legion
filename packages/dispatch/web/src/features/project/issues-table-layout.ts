import type { ColumnOrderState, ColumnSizingState, VisibilityState } from "@tanstack/react-table";
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import {
  BOX_BORDERS,
  COLUMN_MIN,
  COLUMN_SIZES,
  COMPACT_COLUMN_MIN,
  keyColumnWidth,
  PHONE_KEY_SHARE,
  PHONE_PINNED_SHARE,
  PHONE_PRIMARY_MIN,
  PINNED,
  PINNED_SHARE,
} from "./issues-table-model";
import { readLayout, writeLayout } from "./issues-table-storage";

/** A table that names no primary column: one array, so the hook's default never changes
 *  identity and the widths are not recomputed for a new empty list on every render. */
const NO_PRIMARIES: readonly string[] = [];

/**
 * How wide the table's columns are, in what order, which are shown, and how big a box it gets
 * in the window. The widths, order and visibility are the reader's, saved per project; the box
 * and the pinned columns follow the window, so a phone keeps the key and the title readable.
 *
 * Measuring reads layout, so it happens only when something that changes the answer happens: a
 * `ResizeObserver` over the box and over everything drawn above it — which covers the rows the
 * page holds, a notice appearing above the table and moving its top edge, and a sidebar
 * changing its width — and a window resize, which the observer cannot see because the box's own
 * height is what this sets. A scroll does none of these, and the virtualizer commits on every
 * visible range, so measuring there would be a reflow per scrolled row.
 */
export function useTableLayout(
  project: string,
  longestKeyChars = 0,
  {
    columnsOverride,
    hiddenByDefault,
    primaryColumns = NO_PRIMARIES,
  }: {
    /** The columns a URL names for this load. Its layout never reaches browser storage. */
    columnsOverride?: readonly string[];
    /** Which columns this surface opens with hidden; a reader's own choice still wins. */
    hiddenByDefault?: Readonly<Record<string, boolean>>;
    /** The columns this table keeps below 640 px, in the order it draws them. Where it names
     *  any, the title is drawn under the key in the Key cell rather than in a column of its
     *  own, and the key and these columns share the box's width between them. Naming none
     *  leaves the table as it is at every width. */
    primaryColumns?: readonly string[];
  } = {}
) {
  const saved = useMemo(() => readLayout(project, hiddenByDefault), [project, hiddenByDefault]);
  const [sizing, setSizing] = useState<ColumnSizingState>(saved.sizing);
  const [order, setOrder] = useState<ColumnOrderState>(saved.order);
  const [visibility, setVisibility] = useState<VisibilityState>(saved.visibility);
  const [boxWidth, setBoxWidth] = useState(1000);
  const [boxHeight, setBoxHeight] = useState(400);
  const scrollRef = useRef<HTMLDivElement>(null);
  const pagerRef = useRef<HTMLDivElement>(null);
  const [box, setBox] = useState<HTMLDivElement | null>(null);
  const [above, setAbove] = useState<HTMLDivElement | null>(null);
  /** The box, held as both the ref the rest of the table reads and the state the measurement
   *  observes: it is not in the page on the first render, while the listing is still out. */
  const attachBox = useCallback((node: HTMLDivElement | null) => {
    scrollRef.current = node;
    setBox(node);
  }, []);
  /** Everything drawn above the box: the status bar, the toolbar and the notices. What it
   *  does to the box is move its top edge, which is not a size change the box itself sees. */
  const attachAbove = useCallback((node: HTMLDivElement | null) => setAbove(node), []);

  useEffect(() => {
    if (columnsOverride === undefined) writeLayout(project, { sizing, order, visibility });
  }, [columnsOverride, project, sizing, order, visibility]);

  const responsive = useMemo(() => {
    // The Key column never cuts a key: its floor is what the project's longest key needs.
    const keyFloor = longestKeyChars > 0 ? keyColumnWidth(longestKeyChars) : 0;
    const widths = Object.fromEntries(
      Object.entries(COLUMN_SIZES).map(([id, fallback]) => [
        id,
        Math.max(COLUMN_MIN[id] ?? 116, sizing[id] ?? fallback, id === "key" ? keyFloor : 0),
      ])
    );
    // On a phone the key and title are the row, so they take most of the box, leaving an edge of
    // the next column to show the table scrolls sideways; wider boxes keep room for the rest.
    const budget = boxWidth * (boxWidth < 640 ? PHONE_PINNED_SHARE : PINNED_SHARE);
    if (boxWidth < 640) {
      widths.key = Math.max(COMPACT_COLUMN_MIN.key ?? widths.key ?? 88, keyFloor);
      if (primaryColumns.length > 0) {
        // The title is a second line inside the Key cell, so the row is the Key column and the
        // primary columns, and the two share the box between them: the key takes a little over
        // half, the primaries split what is left evenly, and the key gives way where their
        // floors need more — the table fits the box, so nothing is cut and nothing scrolls
        // sideways. A key is never shortened for it: its floor wins over the share.
        const room = Math.max(0, boxWidth - BOX_BORDERS);
        const floors = primaryColumns.length * PHONE_PRIMARY_MIN;
        widths.key = Math.max(
          widths.key,
          Math.min(Math.round(room * PHONE_KEY_SHARE), room - floors)
        );
        const each = Math.floor(Math.max(0, room - widths.key) / primaryColumns.length);
        for (const id of primaryColumns) widths[id] = Math.max(PHONE_PRIMARY_MIN, each);
        return { sizing: widths, pinned: ["select", "key"] };
      }
      widths.title = Math.max(
        COMPACT_COLUMN_MIN.title ?? 120,
        Math.min(widths.title ?? 380, budget - (widths.select ?? 44) - (widths.key ?? 88))
      );
      return { sizing: widths, pinned: ["select", "key", "title"] };
    }
    const pinned = [...PINNED];
    const width = () => pinned.reduce((sum, id) => sum + (widths[id] ?? 0), 0);
    widths.title = Math.max(
      COLUMN_MIN.title ?? 116,
      (widths.title ?? 380) - Math.max(0, width() - budget)
    );
    // At readable minimums, unpin from the right rather than cover the viewport.
    while (pinned.length > 1 && width() > budget) pinned.pop();
    return { sizing: widths, pinned };
  }, [boxWidth, sizing, longestKeyChars, primaryColumns]);

  const measure = useCallback(() => {
    const box = scrollRef.current;
    if (!box) return;
    const bounds = box.getBoundingClientRect();
    // A box with no layout yet measures 0 wide: a grid inside a container that is not shown,
    // or the instant before the first paint of a full-page capture. Zero is not a phone, so
    // the last width that was real stands until the box is measured again.
    if (bounds.width > 0) setBoxWidth(bounds.width);
    const header = box.querySelector("thead")?.getBoundingClientRect().height ?? 61;
    const firstRows = [...box.querySelectorAll<HTMLElement>("[data-issue-row]")].slice(0, 3);
    const rowsHeight = firstRows.reduce(
      (height, row) => height + row.getBoundingClientRect().height,
      0
    );
    // The room left over under the pager, which the box takes: the window, less the bottom
    // padding the section and the page keep for the floating selection bar, less where the
    // pager's last pixel is now. Measured against the pager's own place rather than the page's
    // height — which on a page shorter than the window is the window, leaving the box only
    // able to shrink — so the box grows as well as shrinks, whatever stands above it.
    const pager = pagerRef.current;
    if (!pager) return;
    const padding = (element: Element | null) =>
      element === null ? 0 : Number.parseFloat(getComputedStyle(element).paddingBottom) || 0;
    let bottomPadding = 0;
    const main = box.closest("main");
    for (
      let ancestor = box.parentElement;
      ancestor !== null && ancestor !== main;
      ancestor = ancestor.parentElement
    )
      bottomPadding += padding(ancestor);
    bottomPadding += padding(main);
    const slack = window.innerHeight - bottomPadding - pager.getBoundingClientRect().bottom;
    const available = bounds.height + slack;
    setBoxHeight(Math.max(400, header + rowsHeight + 2, available));
  }, []);
  useLayoutEffect(() => {
    measure();
    window.addEventListener("resize", measure);
    // happy-dom lays nothing out and has no `ResizeObserver`; the unit suite installs one.
    const sizes = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    if (box) {
      sizes?.observe(box);
      // Everything the window holds above the table: the page's own header strip wraps on a
      // phone when a pill arrives, which moves the box's top edge without resizing the box.
      const page = box.closest("main") ?? box.parentElement;
      if (page) sizes?.observe(page);
    }
    if (above) sizes?.observe(above);
    return () => {
      window.removeEventListener("resize", measure);
      sizes?.disconnect();
    };
  }, [above, box, measure]);

  return {
    attachAbove,
    attachBox,
    boxHeight,
    boxWidth,
    order,
    pagerRef,
    responsive,
    scrollRef,
    setOrder,
    setSizing,
    setVisibility,
    sizing,
    visibility,
  };
}
