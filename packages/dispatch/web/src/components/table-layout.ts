import type { ColumnOrderState, ColumnSizingState, VisibilityState } from "@tanstack/react-table";
import {
  type Dispatch,
  type RefObject,
  type SetStateAction,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { readLayout, type StoredColumns, writeLayout } from "./table-storage";

/** The columns a table pins at its left edge while it scrolls sideways, and how they fit the
 *  box it is drawn in. */
export interface LayoutPins {
  /** In a box 640 px wide or wider: these columns, left to right, within `share` of the box.
   *  Where they would cover more, `flexible` narrows first, down to its minimum, and then the
   *  rightmost unpin one at a time. Their visibility is not the reader's, so none is stored. */
  readonly wide: { readonly columns: readonly string[]; readonly share: number };
  /** In a narrower box, a phone's: these columns, left to right, within `share` of it. Each
   *  column of `widths` is drawn at that width whatever the reader dragged, and `flexible` takes
   *  what the others leave, between `flexibleMin` and its own width. */
  readonly phone: {
    readonly columns: readonly string[];
    readonly share: number;
    readonly widths: Readonly<Record<string, number>>;
    readonly flexibleMin: number;
  };
  /** The pinned column that gives up width so the others fit. */
  readonly flexible: string;
}

/** Every rule a table's layout follows, from its caller: `useTableLayout` reads no other.
 *  Held in a module constant, since the stored layout is read again whenever it changes. */
export interface LayoutColumns extends StoredColumns {
  /** The attribute each drawn row carries, which is the caller's own word for its rows: the
   *  first few of them are measured to tell how short the box may be. */
  readonly rowAttribute: string;
  /** Each column's width before the reader drags it. A column it does not name is drawn at its
   *  definition's `size`, and counts as no width where the pins are fitted. */
  readonly sizes?: Readonly<Partial<Record<string, number>>>;
  /** The narrowest each column is drawn at, whatever width the reader stored or dragged. */
  readonly minimums?: Readonly<Partial<Record<string, number>>>;
  /** The columns pinned at the left. A table without it pins nothing. */
  readonly pins?: LayoutPins;
}

/** A table's layout as `useTableLayout` keeps it: the reader's widths, order and visibility,
 *  what the box measures, and the refs and setters the table and its box are wired with. */
export interface TableLayoutModel {
  /** Attaches everything drawn above the box, whose size moves the box's top edge. */
  readonly attachAbove: (node: HTMLDivElement | null) => void;
  /** Attaches the box the rows scroll in. */
  readonly attachBox: (node: HTMLDivElement | null) => void;
  readonly boxHeight: number;
  readonly boxWidth: number;
  readonly order: ColumnOrderState;
  readonly pagerRef: RefObject<HTMLDivElement | null>;
  /** The widths the window allows and the columns pinned at its left. */
  readonly responsive: { readonly sizing: ColumnSizingState; readonly pinned: string[] };
  readonly scrollRef: RefObject<HTMLDivElement | null>;
  readonly setOrder: Dispatch<SetStateAction<ColumnOrderState>>;
  readonly setSizing: Dispatch<SetStateAction<ColumnSizingState>>;
  readonly setVisibility: Dispatch<SetStateAction<VisibilityState>>;
  readonly sizing: ColumnSizingState;
  readonly visibility: VisibilityState;
}

/** The column order a drag leaves behind: `dragged` taken out of `current` and put back
 *  immediately before `before`. A drop on itself, or on an id the order does not hold, leaves
 *  the order as it was. */
export function reorder(current: readonly string[], dragged: string, before: string): string[] {
  if (dragged === before || !current.includes(dragged) || !current.includes(before))
    return [...current];
  const next = current.filter((id) => id !== dragged);
  next.splice(next.indexOf(before), 0, dragged);
  return next;
}

/**
 * How wide the table's columns are, in what order, which are shown, and how big a box it gets
 * in the window. The widths, order and visibility are the reader's, saved under `layoutKey` —
 * the whole key, which the caller names and this prefixes nothing to — for the `columns` the
 * table has; the box and the pinned columns follow the window, by the rules `columns` gives.
 *
 * Measuring reads layout, so it happens only when something that changes the answer happens: a
 * `ResizeObserver` over the box and over everything drawn above it — which covers the rows the
 * page holds, a notice appearing above the table and moving its top edge, and a sidebar
 * changing its width — and a window resize, which the observer cannot see because the box's own
 * height is what this sets. A scroll does none of these, and the virtualizer commits on every
 * visible range, so measuring there would be a reflow per scrolled row.
 */
export function useTableLayout(layoutKey: string, columns: LayoutColumns): TableLayoutModel {
  const saved = useMemo(() => readLayout(layoutKey, columns), [layoutKey, columns]);
  const [sizing, setSizing] = useState<ColumnSizingState>(saved.sizing);
  const [order, setOrder] = useState<ColumnOrderState>(saved.order);
  const [visibility, setVisibility] = useState<VisibilityState>(saved.visibility);
  const [boxWidth, setBoxWidth] = useState(1000);
  const [boxHeight, setBoxHeight] = useState(400);
  const scrollRef = useRef<HTMLDivElement>(null);
  const pagerRef = useRef<HTMLDivElement>(null);
  const [box, setBox] = useState<HTMLDivElement | null>(null);
  const [above, setAbove] = useState<HTMLDivElement | null>(null);
  const { rowAttribute } = columns;
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
    writeLayout(layoutKey, { sizing, order, visibility });
  }, [layoutKey, sizing, order, visibility]);

  const responsive = useMemo(() => {
    const { ids, minimums = {}, pins, sizes = {} } = columns;
    // Each column at the reader's width, else its starting width, never below its minimum; a
    // column with neither keeps the width its definition gives it.
    const widths: ColumnSizingState = Object.fromEntries(
      ids.flatMap((id): [string, number][] => {
        const width = sizing[id] ?? sizes[id];
        return width === undefined ? [] : [[id, Math.max(minimums[id] ?? 0, width)]];
      })
    );
    if (pins === undefined) return { sizing: widths, pinned: [] };
    const { flexible, phone, wide } = pins;
    const covered = (pinned: readonly string[]) =>
      pinned.reduce((sum, id) => sum + (widths[id] ?? 0), 0);
    const flexibleWidth = widths[flexible];
    if (boxWidth < 640) {
      Object.assign(widths, phone.widths);
      if (flexibleWidth !== undefined)
        widths[flexible] = Math.max(
          phone.flexibleMin,
          Math.min(
            flexibleWidth,
            boxWidth * phone.share - covered(phone.columns.filter((id) => id !== flexible))
          )
        );
      return { sizing: widths, pinned: [...phone.columns] };
    }
    const budget = boxWidth * wide.share;
    const pinned = [...wide.columns];
    if (flexibleWidth !== undefined)
      widths[flexible] = Math.max(
        minimums[flexible] ?? 0,
        flexibleWidth - Math.max(0, covered(pinned) - budget)
      );
    // At readable minimums, unpin from the right rather than cover the viewport.
    while (pinned.length > 1 && covered(pinned) > budget) pinned.pop();
    return { sizing: widths, pinned };
  }, [boxWidth, columns, sizing]);

  const measure = useCallback(() => {
    const box = scrollRef.current;
    if (!box) return;
    const bounds = box.getBoundingClientRect();
    setBoxWidth(bounds.width);
    const header = box.querySelector("thead")?.getBoundingClientRect().height ?? 61;
    const firstRows = [...box.querySelectorAll<HTMLElement>(`[${rowAttribute}]`)].slice(0, 3);
    const rowsHeight = firstRows.reduce(
      (height, row) => height + row.getBoundingClientRect().height,
      0
    );
    // The room left over under the table's last line, which the box takes: the window, less the
    // bottom padding the section and the page keep for the floating selection bar, less where
    // that line's last pixel is now. Measured against the pager's own place — the box's own
    // bottom edge where the table has no pager — rather than the page's height, which on a page
    // shorter than the window is the window, leaving the box only able to shrink; so the box
    // grows as well as shrinks, whatever stands above it.
    const foot = pagerRef.current?.getBoundingClientRect().bottom ?? bounds.bottom;
    const padding = (element: Element | null) =>
      element === null ? 0 : Number.parseFloat(getComputedStyle(element).paddingBottom) || 0;
    const slack =
      window.innerHeight - padding(box.parentElement) - padding(box.closest("main")) - foot;
    const available = bounds.height + slack;
    setBoxHeight(Math.max(400, header + rowsHeight + 2, available));
  }, [rowAttribute]);
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
