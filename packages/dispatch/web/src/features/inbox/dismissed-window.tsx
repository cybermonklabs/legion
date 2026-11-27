/**
 * The Dismissed view's window over the rows it has loaded (`DismissedWindow`), which the Inbox
 * draws in place of its bands while it lists what the reader dismissed out of it (`Inbox.tsx`),
 * and where each row it draws sits in it (`WindowSlotContext`), which the row's own `<li>` reads.
 */

import { useVirtualizer, useWindowVirtualizer, type Virtualizer } from "@tanstack/react-virtual";
import {
  createContext,
  type ReactNode,
  type RefObject,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { WINDOW_OVERSCAN } from "../../components/DataTable";
import { focusLost, focusOwnStop } from "../../components/focus-place";
import { ROW_ATTRIBUTE } from "./inbox-row";
import { type InboxListRow, rowId } from "./list-row";

/** Where a row of the Dismissed window sits among the loaded rows, how many rows are loaded, and
 *  the virtualizer's measure of the row (`DismissedWindow`). The row's own `<li>` takes them
 *  (`Inbox.tsx`'s `InboxRowShell`), so it stays a direct child of the list, and a screen reader
 *  hears the row's place among every loaded row, drawn or not, as `DataTable` says it of its own
 *  window. Undefined outside that window, where every row is drawn. */
interface WindowSlot {
  readonly count: number;
  readonly index: number;
  readonly measure: (node: Element | null) => void;
}

export const WindowSlotContext = createContext<WindowSlot | undefined>(undefined);

/** The space between two rows of the Dismissed list, in pixels: its `gap-3`. */
const DISMISSED_ROW_GAP = 12;

/** How tall a row of the Dismissed view is before it is drawn and measured: a short row beside
 *  the reading column, and a card without it. Close to what each draws at, so the scroll box is
 *  about the right height from the first draw and the rows move little once measured. */
export const DISMISSED_SHORT_ROW_PX = 58;
export const DISMISSED_CARD_ROW_PX = 120;

/** What the Inbox asks of the Dismissed window. */
export interface DismissedWindowHandle {
  /** Focuses row `id`, the loaded row at `index`: at once where the window draws it, and else
   *  once the window has scrolled to it and drawn it. */
  focusRow: (id: string, index: number) => void;
}

interface DismissedWindowProps {
  /** The box the rows scroll in: the list column beside the reading column, or the drawer's own
   *  box. Undefined where the window scrolls them. */
  readonly box: RefObject<HTMLElement | null> | undefined;
  /** The row in hand where the reading column is on: the selected row, which `j` and `k` move.
   *  Undefined without the column, where the keys move the focus instead (`focusRow`). */
  readonly hand: string | undefined;
  readonly handle: RefObject<DismissedWindowHandle | null>;
  /** Reads the next page of dismissals. Undefined where there is none, or while it loads. */
  readonly onNearEnd: (() => void) | undefined;
  readonly renderRow: (row: InboxListRow) => ReactNode;
  /** How tall a row is before it is measured. */
  readonly rowHeight: number;
  /** Every loaded row, in the order the view lists them: what the window draws a part of. */
  readonly rows: readonly InboxListRow[];
}

/**
 * The Dismissed view's rows, drawn as a window: the rows in sight and `WINDOW_OVERSCAN` more
 * above and below them, out of every row the view has loaded. A person can have thousands of
 * dismissed threads, and drawing every loaded row made the view slower with each page. The rows
 * stay the view's own `<li>`s in its own `<ul>`, with the virtualizer `DataTable` uses.
 *
 * The window scrolls the box the list's anchor scrolls: the list column beside the reading
 * column, the drawer's box, or the window itself. Each kind needs its own virtualizer hook, so
 * this picks one of two components.
 */
export function DismissedWindow(props: DismissedWindowProps): ReactNode {
  return props.box === undefined ? (
    <DismissedPageWindow {...props} />
  ) : (
    <DismissedBoxWindow {...props} box={props.box} />
  );
}

/** What both windows ask of their virtualizer: the loaded rows, each keyed by its row, and
 *  `margin`, where the list starts in what the window scrolls (`DismissedRows`). */
function dismissedWindowOptions(props: DismissedWindowProps, margin: number) {
  return {
    count: props.rows.length,
    estimateSize: () => props.rowHeight,
    gap: DISMISSED_ROW_GAP,
    getItemKey: (index: number) =>
      props.rows[index] === undefined ? index : rowId(props.rows[index]),
    overscan: WINDOW_OVERSCAN,
    scrollMargin: margin,
  };
}

/** The window over rows the page itself scrolls. */
function DismissedPageWindow(props: DismissedWindowProps): ReactNode {
  const [margin, setMargin] = useState(0);
  const virtualizer = useWindowVirtualizer(dismissedWindowOptions(props, margin));
  return <DismissedRows {...props} onMargin={setMargin} virtualizer={virtualizer} />;
}

/** The window over rows a box scrolls. */
function DismissedBoxWindow(
  props: DismissedWindowProps & { readonly box: RefObject<HTMLElement | null> }
): ReactNode {
  const [margin, setMargin] = useState(0);
  const virtualizer = useVirtualizer({
    ...dismissedWindowOptions(props, margin),
    getScrollElement: () => props.box.current,
  });
  return <DismissedRows {...props} onMargin={setMargin} virtualizer={virtualizer} />;
}

/**
 * The drawn part of the window, and what keeps it right.
 *
 * The list starts below whatever the box or the page draws above it, so where it starts is read
 * after each commit and handed to the window (`onMargin`), whose virtualizer counts its rows from
 * it (`scrollMargin`). The row in hand is always drawn: a step of `j` or `k` to a row the window
 * does not draw scrolls the window to it first, and the row takes the focus, or the selection,
 * once it is drawn. The next page is read once the window reaches the last loaded row, one
 * request for each number of loaded rows; **Show more** still reads it by key.
 *
 * A focused row the person scrolls out of the window is no loss. The window takes the row away,
 * which is no press of the person's, so the focus goes to no row and nothing scrolls: on the page
 * it stays on the page. In the drawer, a modal dialog, a focus left on the page would reach the
 * page behind the dialog, so the window hands it to the drawer's own stop, with no scroll, as a
 * component hands a lost focus to its own region's stop whoever caused the loss (`focusOwnStop`).
 * Scrolled back, the row is drawn again. A row a live update takes away while it is the row
 * under the hand stays in the rows the Inbox hands here (`heldRow`), so it stays drawn in its
 * place.
 */
function DismissedRows({
  box,
  hand,
  handle,
  onMargin,
  onNearEnd,
  renderRow,
  rows,
  virtualizer,
}: DismissedWindowProps & {
  readonly onMargin: (margin: number) => void;
  readonly virtualizer: Virtualizer<HTMLElement, Element> | Virtualizer<Window, Element>;
}): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  /** The row that takes the focus once the window draws it. */
  const pendingFocus = useRef<string | null>(null);
  /** How many rows were loaded when the window last asked for the next page. */
  const askedAt = useRef<number | null>(null);
  /** The focused element of a row this list draws, and that row's id, noted as it takes the
   *  focus, for the window's own hand-off below. */
  const focusedIn = useRef<{ readonly element: Element; readonly id: string } | null>(null);
  const drawnRow = (id: string) =>
    list.current?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${CSS.escape(id)}"]`) ?? null;

  useImperativeHandle(handle, () => ({
    focusRow: (id, index) => {
      const row = drawnRow(id);
      if (row !== null) {
        pendingFocus.current = null;
        row.focus();
        return;
      }
      pendingFocus.current = id;
      virtualizer.scrollToIndex(index, { align: "auto" });
    },
  }));

  // Where the list starts in what the box or the page scrolls, which the virtualizer counts its
  // rows from. Read after every commit: a bar or a filter row above the list can come and go.
  useLayoutEffect(() => {
    const element = list.current;
    if (element === null) return;
    const top = element.getBoundingClientRect().top;
    const scroller = box?.current ?? null;
    const start =
      scroller === null
        ? top + window.scrollY
        : top - scroller.getBoundingClientRect().top + scroller.scrollTop;
    if (Math.abs(start - virtualizer.options.scrollMargin) >= 1) onMargin(start);
  });

  // A row a step asked for takes the focus once it is drawn, with no scroll of its own: the
  // window has already brought it into view.
  useLayoutEffect(() => {
    const id = pendingFocus.current;
    if (id === null) return;
    const row = drawnRow(id);
    if (row === null) return;
    pendingFocus.current = null;
    row.focus({ preventScroll: true });
  });

  // The selected row stays drawn and in view as `j` and `k` move it. A selected row the window
  // draws is brought into view by the selection itself (`pane-selection.ts`). On the passive
  // lane, as the selection's own scroll is: the list's anchor (`ViewportAnchor`) holds the row
  // under the hand in place at the end of the commit that moved the selection, and a scroll made
  // inside that commit would be undone by it.
  // biome-ignore lint/correctness/useExhaustiveDependencies: a change of the row in hand is what this answers, and nothing else.
  useEffect(() => {
    if (hand === undefined || drawnRow(hand) !== null) return;
    const index = rows.findIndex((row) => rowId(row) === hand);
    if (index !== -1) virtualizer.scrollToIndex(index, { align: "auto" });
  }, [hand]);

  // The window's own hand-off, after every commit: a focused row the window took away on the
  // person's scroll - still a loaded row, drawn no more - hands the focus to the modal dialog the
  // list stands in, its own stop with no scroll, and leaves it on the page anywhere else. A row
  // that left the loaded rows is not the window's doing, and a focus already placed moves nothing.
  useLayoutEffect(() => {
    const noted = focusedIn.current;
    if (noted === null || noted.element.isConnected) return;
    focusedIn.current = null;
    if (!focusLost() || !rows.some((row) => rowId(row) === noted.id)) return;
    const modal = list.current?.closest<HTMLElement>('[role="dialog"][aria-modal="true"]');
    if (modal != null) focusOwnStop(modal);
  });

  const items = virtualizer.getVirtualItems();
  const first = items[0];
  const last = items.at(-1);
  const lastIndex = last?.index;
  useEffect(() => {
    if (onNearEnd === undefined || lastIndex === undefined || lastIndex < rows.length - 1) return;
    if (askedAt.current === rows.length) return;
    askedAt.current = rows.length;
    onNearEnd();
  });

  const { scrollMargin } = virtualizer.options;
  return (
    <ul
      className="flex flex-col gap-3"
      onFocus={(event) => {
        const row = event.target.closest(`[${ROW_ATTRIBUTE}]`);
        const id = row?.getAttribute(ROW_ATTRIBUTE);
        focusedIn.current = id == null ? null : { element: event.target, id };
      }}
      ref={list}
      style={{
        paddingBottom:
          last === undefined ? 0 : virtualizer.getTotalSize() - (last.end - scrollMargin),
        paddingTop: first === undefined ? 0 : first.start - scrollMargin,
      }}
    >
      {items.map((item) => {
        const row = rows[item.index];
        if (row === undefined) return null;
        return (
          <WindowSlotContext
            key={rowId(row)}
            value={{ count: rows.length, index: item.index, measure: virtualizer.measureElement }}
          >
            {renderRow(row)}
          </WindowSlotContext>
        );
      })}
    </ul>
  );
}
