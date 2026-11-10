import type { ColumnOrderState, ColumnSizingState, VisibilityState } from "@tanstack/react-table";
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import {
  COLUMN_MIN,
  COLUMN_SIZES,
  COMPACT_COLUMN_MIN,
  PHONE_PINNED_SHARE,
  PINNED,
  PINNED_SHARE,
} from "./issues-table-model";
import { readLayout, writeLayout } from "./issues-table-storage";

/**
 * How wide the table's columns are, in what order, which are shown, and how big a box it gets
 * in the window. The widths, order and visibility are the reader's, saved per project; the box
 * and the pinned columns follow the window, so a phone keeps the key and the title readable.
 *
 * Measuring reads layout, so it happens only when something that changes the answer happens:
 * the window resizes, or the caller says its rows changed (`measure`). A scroll does neither.
 */
export function useTableLayout(project: string) {
  const saved = useMemo(() => readLayout(project), [project]);
  const [sizing, setSizing] = useState<ColumnSizingState>(saved.sizing);
  const [order, setOrder] = useState<ColumnOrderState>(saved.order);
  const [visibility, setVisibility] = useState<VisibilityState>(saved.visibility);
  const [boxWidth, setBoxWidth] = useState(1000);
  const [boxHeight, setBoxHeight] = useState(400);
  const scrollRef = useRef<HTMLDivElement>(null);
  const pagerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    writeLayout(project, { sizing, order, visibility });
  }, [project, sizing, order, visibility]);

  const responsive = useMemo(() => {
    const widths = Object.fromEntries(
      Object.entries(COLUMN_SIZES).map(([id, fallback]) => [
        id,
        Math.max(COLUMN_MIN[id] ?? 116, sizing[id] ?? fallback),
      ])
    );
    // On a phone the key and title are the row, so they take most of the box, leaving an edge of
    // the next column to show the table scrolls sideways; wider boxes keep room for the rest.
    const budget = boxWidth * (boxWidth < 640 ? PHONE_PINNED_SHARE : PINNED_SHARE);
    if (boxWidth < 640) {
      widths.key = COMPACT_COLUMN_MIN.key ?? widths.key ?? 88;
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
  }, [boxWidth, sizing]);

  const measure = useCallback(() => {
    const box = scrollRef.current;
    if (!box) return;
    const bounds = box.getBoundingClientRect();
    const header = box.querySelector("thead")?.getBoundingClientRect().height ?? 61;
    const firstRows = [...box.querySelectorAll<HTMLElement>("[data-issue-row]")].slice(0, 3);
    const rowsHeight = firstRows.reduce(
      (height, row) => height + row.getBoundingClientRect().height,
      0
    );
    const available =
      window.innerHeight -
      (bounds.top + window.scrollY) -
      (pagerRef.current?.getBoundingClientRect().height ?? 80) -
      32;
    setBoxHeight(Math.max(400, header + rowsHeight + 2, available));
    setBoxWidth(bounds.width);
  }, []);
  useLayoutEffect(() => {
    measure();
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [measure]);

  return {
    boxHeight,
    boxWidth,
    measure,
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
