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
import { flushSync } from "react-dom";
import { readLayout, type StoredColumns, writeLayout } from "./table-storage";

/** The columns a table pins at its left edge while it scrolls sideways, and how they fit the
 *  box it is drawn in. */
export interface LayoutPins {
  /** In a desktop's box (`PHONE_BOX_BELOW`): these columns, left to right, within `share` of the
   *  box. Where they would cover more, `flexible` narrows first, down to its minimum, and then
   *  the rightmost unpin one at a time. Their visibility is not the reader's, so none is stored. */
  readonly wide: { readonly columns: readonly string[]; readonly share: number };
  /** In a phone's box: these columns, left to right, within `share` of it and never past the
   *  room inside its borders and beside its scrollbar, so a share of 1 ends them at its inner
   *  edge. Each column of `widths` is drawn at that width whatever the reader dragged, and
   *  `flexible` takes what the others leave, from `flexibleMin` up to its own width, or the whole
   *  of it in a table that fills its box (`fills`) but where the reader dragged it. */
  readonly phone: {
    readonly columns: readonly string[];
    readonly share: number;
    readonly widths: Readonly<Record<string, number>>;
    readonly flexibleMin: number;
  };
  /** The pinned column that gives up width so the others fit. */
  readonly flexible: string;
}

/** The room a box keeps beside its columns for its two borders and the widest classic vertical
 *  scrollbar (17 px on Windows), which a box narrower than `PHONE_BOX_BELOW` must have to spare
 *  to be read as a desktop's. It is no gap in the table's lines: the column that fills the box
 *  takes the box's inner width, read off the box itself (`boxEdge`), so the table is exactly as
 *  wide as the room inside the box's borders and beside its scrollbar, its header line and each
 *  row's line reach the border, and a box whose rows scroll draws no sideways scrollbar. */
const EDGE_ROOM = 20;

/** A box narrower than this is a phone's, unless the table fills it (`fills`) and fits it with
 *  the filling column at its minimum: that is a desktop's box a page has narrowed, as a
 *  priority's page narrows its work table beside the margin (606 px in a 1280 px window), and the
 *  table keeps a desktop's widths and headers there. */
const PHONE_BOX_BELOW = 640;

/** How a column is as wide as what it holds (`LayoutColumns.fits`): as wide as its header or the
 *  widest of its cells needs on one line; and, where `most` is given, no wider than that, past
 *  which its cells wrap - though never narrower than its header on one line or the widest part
 *  of a cell that cannot wrap, so it cuts nothing. */
export interface ColumnFit {
  readonly most?: number;
}

/** Every rule a table's layout follows, from its caller: `useTableLayout` reads no other.
 *  Held in a module constant, since the stored layout is read again whenever it changes. */
export interface LayoutColumns extends StoredColumns {
  /** The attribute each drawn row carries, which is the caller's own word for its rows: the
   *  first few of them are measured to tell how short the box may be. */
  readonly rowAttribute: string;
  /** Each column's width before the reader drags it. A column it does not name is drawn at its
   *  definition's `size`, and counts as no width where the pins are fitted. A column of `fits`
   *  stands at it only until what it holds is measured, and where nothing is laid out. */
  readonly sizes?: Readonly<Partial<Record<string, number>>>;
  /** The narrowest each column is drawn at, whatever width the reader stored or dragged. */
  readonly minimums?: Readonly<Partial<Record<string, number>>>;
  /** The columns pinned at the left. A table without it pins nothing. */
  readonly pins?: LayoutPins;
  /** The column that fills the box: in a desktop's box (`PHONE_BOX_BELOW`) it takes the width the
   *  other shown columns and `trailing` leave inside the box's borders and beside its scrollbar,
   *  never below its minimum, so the lines reach the box's border and nothing scrolls sideways
   *  while the columns fit. A width the reader dragged it to stays theirs, and a phone's box
   *  gives the pinned flexible column what the other pinned columns leave of the pins' share
   *  (`LayoutPins.phone`). A table without it fills nothing. */
  readonly fills?: string;
  /** The width the table draws after its columns - a row's actions - which `fills` leaves. */
  readonly trailing?: number;
  /** The columns each as wide as what it holds, by id (`ColumnFit`): measured as the table draws
   *  them, its header and each row of its page the box has drawn, so a column is never wider
   *  than its widest header or cell needs and cuts neither (`useTableLayout`). A width the reader
   *  dragged one to stays theirs where it is wider. A table without it fits nothing. */
  readonly fits?: Readonly<Partial<Record<string, ColumnFit>>>;
  /** The columns that give the column that fills the box its room, by id, each with the width
   *  it leaves that column: in a desktop's box, where `fills` would be narrower than that, the
   *  column narrows from its width, down to its minimum - for a column of `fits`, down to the
   *  narrowest its header and cells need without cutting, and for one of `wrappedHeaders`, down
   *  to what its header needs - to leave it that much, each in turn in the order they are named,
   *  so the first gives way first. A width the reader dragged it to stays theirs. */
  readonly yields?: Readonly<Partial<Record<string, number>>>;
  /** The columns whose header's words wrap onto more lines in a column narrower than they are
   *  on one (`DataTable`'s `wrap`), by id. In a desktop's box none is drawn narrower than its
   *  header needs as the table draws it now, measured there: its words on as many lines as they
   *  take, with its filter beside them and, while the table is sorted by it, its sort arrow, so
   *  nothing in the header overlaps whatever the reader sorts by. A phone's box draws each
   *  header on one line, and these columns at the widths it gives them. */
  readonly wrappedHeaders?: readonly string[];
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
  /** Gives the box the height the page lays out for it now (`boxHeight`), at once, as its
   *  `ResizeObserver` does a frame after any change of the page. A host that hands the focus on
   *  calls it right before, so the box the focus lands against is the box the reader will see
   *  (`features/project/IssuesTable.tsx`'s `returnFocus`). The widths are left to that observer:
   *  a change of them can draw the rows anew, which would take the focus the host is about to
   *  hand on. */
  readonly fitHeight: () => void;
  readonly order: ColumnOrderState;
  readonly pagerRef: RefObject<HTMLDivElement | null>;
  /** The widths the window allows, the columns pinned at its left, and whether the box is a
   *  phone's (`compact`): there each header stands on one line but the one the table stacks
   *  (`DataTable`'s `stackColumn`), and a column takes its phone minimum or width. */
  readonly responsive: {
    readonly sizing: ColumnSizingState;
    readonly pinned: string[];
    readonly compact: boolean;
  };
  readonly scrollRef: RefObject<HTMLDivElement | null>;
  readonly setOrder: Dispatch<SetStateAction<ColumnOrderState>>;
  readonly setSizing: Dispatch<SetStateAction<ColumnSizingState>>;
  readonly setVisibility: Dispatch<SetStateAction<VisibilityState>>;
  readonly sizing: ColumnSizingState;
  readonly visibility: VisibilityState;
  /** Tells the layout the rows the table's page holds, by id: each row's cells count toward the
   *  widths of the columns that fit what they hold (`LayoutColumns.fits`) while the page holds
   *  it, drawn in the box or scrolled out of it, and no longer once it does not. */
  readonly setPageRows: (ids: readonly string[]) => void;
}

/** What a cell needs to hold what it holds, with its padding and borders: on one line, its
 *  content's max-content width (`line`), and as narrow as its content wraps without cutting
 *  anything, its min-content width (`narrowest`). */
interface CellNeed {
  readonly line: number;
  readonly narrowest: number;
}

/** A column's need as read off one cell, with the markup it was read from and the window's width
 *  it was read at: the cell is read again only once what it holds changed, or once it is drawn
 *  in a window of another width, whose breakpoints can move its padding where no markup says
 *  so. Until it is read again it counts as it was read. */
interface ReadNeed {
  readonly markup: string;
  readonly windowWidth: number;
  readonly need: CellNeed;
}

/** A column of `fits` as what it holds needs it, in whole pixels: the width it is drawn at, and
 *  the narrowest it cuts nothing at, which a column whose cells wrap (`ColumnFit.most`) may give
 *  up width down to (`LayoutColumns.yields`). */
interface FittedWidth {
  readonly width: number;
  readonly narrowest: number;
}

/** What the layout reads off the table as it draws it: each column of `fits` as what it holds
 *  needs it (`FittedWidth`), and each column of `wrappedHeaders` as the narrowest its header is
 *  drawn at as it is now, in whole pixels, by id. Nothing of either until it is measured. */
interface Measured {
  readonly fitted: Readonly<Record<string, FittedWidth>>;
  readonly headerFloors: Readonly<Record<string, number>>;
}

const NOTHING_MEASURED: Measured = { fitted: {}, headerFloors: {} };

/** What each cell of `cells` needs (`CellNeed`), its min-content width read only where
 *  `narrowest` asks for it. Each is read off a copy of what the cell holds, put inside the cell at
 *  that width and hidden, so the copy takes the cell's own type; every copy is put in before any
 *  is read and taken out after, so the page lays out once however many cells are read. */
function cellNeeds(cells: readonly { readonly cell: Element; readonly narrowest: boolean }[]) {
  const copies = cells.map(({ cell, narrowest }) => {
    const content = [...cell.childNodes];
    const copy = (width: string) => {
      const node = document.createElement("div");
      node.style.cssText = `position:absolute;left:0;top:0;visibility:hidden;width:${width}`;
      for (const child of content) node.append(child.cloneNode(true));
      cell.append(node);
      return node;
    };
    return { cell, line: copy("max-content"), narrowest: narrowest ? copy("min-content") : null };
  });
  const needs = copies.map(({ cell, line, narrowest }): CellNeed => {
    const style = getComputedStyle(cell);
    const edges = [
      style.paddingLeft,
      style.paddingRight,
      style.borderLeftWidth,
      style.borderRightWidth,
    ].reduce((sum, value) => sum + (Number.parseFloat(value) || 0), 0);
    const oneLine = line.getBoundingClientRect().width + edges;
    return {
      line: oneLine,
      narrowest: narrowest === null ? oneLine : narrowest.getBoundingClientRect().width + edges,
    };
  });
  for (const { line, narrowest } of copies) {
    line.remove();
    narrowest?.remove();
  }
  return needs;
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
 *
 * A table whose columns fit what they hold (`fits`) reads those columns' header and cells too,
 * and one whose headers wrap (`wrappedHeaders`) those headers: when the box is attached, when
 * the window's width changes, when the page's rows change (`setPageRows`), and when anything in
 * the box is drawn anew - a row the virtualizer draws, a cell whose words change, a header that
 * gains its sort arrow - which a `MutationObserver` says. A cell is read once for what it holds
 * at the window's width and kept for as long as its row is on the page, drawn or not, so the
 * rows a scroll draws and takes away move no width back and forth: a width that moved the
 * virtualizer's window cannot move itself back by it. A change of the window's width reads the
 * header and the drawn cells again, and counts each cell the box does not draw as it was read
 * until its row is drawn, so a resize moves no width by what the box happens to draw either; one
 * that leaves the window's width as it was, its height alone, reads nothing.
 */
export function useTableLayout(layoutKey: string, columns: LayoutColumns): TableLayoutModel {
  const saved = useMemo(() => readLayout(layoutKey, columns), [layoutKey, columns]);
  const [sizing, setSizing] = useState<ColumnSizingState>(saved.sizing);
  const [order, setOrder] = useState<ColumnOrderState>(saved.order);
  const [visibility, setVisibility] = useState<VisibilityState>(saved.visibility);
  const [boxWidth, setBoxWidth] = useState(1000);
  /** What of the box's width is not room for the table: its two borders, and a classic vertical
   *  scrollbar while its rows scroll. */
  const [boxEdge, setBoxEdge] = useState(0);
  const [boxHeight, setBoxHeight] = useState(400);
  const scrollRef = useRef<HTMLDivElement>(null);
  const pagerRef = useRef<HTMLDivElement>(null);
  const [box, setBox] = useState<HTMLDivElement | null>(null);
  const [above, setAbove] = useState<HTMLDivElement | null>(null);
  const { rowAttribute } = columns;
  /** What the table's cells and headers were measured to need (`Measured`). */
  const [measured, setMeasured] = useState<Measured>(NOTHING_MEASURED);
  /** What each column of `fits` needs, as read off its header and off each row's cell, by
   *  column and by row, and what each header of `wrappedHeaders` needs. */
  const needs = useRef({
    headers: new Map<string, ReadNeed>(),
    rows: new Map<string, Map<string, ReadNeed>>(),
  });
  /** The window's width the header and cells of `fits` were last read at (`ReadNeed`). */
  const readWidth = useRef<number | undefined>(undefined);
  /** The rows the table's page holds, by id, once the table has said (`setPageRows`). */
  const pageRows = useRef<ReadonlySet<string> | undefined>(undefined);
  /** The observer of what the box draws anew, whose records of the copies a measure puts in and
   *  takes out are dropped before they call it again. */
  const drawn = useRef<MutationObserver | null>(null);
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
    const { fills, ids, minimums = {}, pins, sizes = {}, trailing = 0, yields = {} } = columns;
    const { fitted, headerFloors } = measured;
    // Each column at the reader's width, else the width what it holds needs (`fits`), else its
    // starting width, never below its minimum nor below what it holds needs; a column with none
    // of these keeps the width its definition gives it.
    const widths: ColumnSizingState = Object.fromEntries(
      ids.flatMap((id): [string, number][] => {
        const fit = fitted[id]?.width;
        const width = sizing[id] ?? fit ?? sizes[id];
        return width === undefined ? [] : [[id, Math.max(minimums[id] ?? 0, fit ?? 0, width)]];
      })
    );
    // What every shown column but the one that fills the box takes, with what the table draws
    // after its columns.
    const others = () =>
      ids.reduce(
        (sum, id) => (id === fills || visibility[id] === false ? sum : sum + (widths[id] ?? 0)),
        trailing
      );
    // A desktop's box: one 640 px wide or wider, or a narrower one the table fits with the
    // column that fills it at its minimum (`PHONE_BOX_BELOW`).
    const desktop =
      boxWidth >= PHONE_BOX_BELOW ||
      (fills !== undefined && others() + (minimums[fills] ?? 0) + EDGE_ROOM <= boxWidth);
    // On a desktop, where a header's words wrap, a column whose header does so is never narrower
    // than it needs as the table draws it now (`wrappedHeaders`), sorted or not.
    if (desktop)
      for (const [id, floor] of Object.entries(headerFloors)) {
        const width = widths[id];
        if (width !== undefined && width < floor) widths[id] = floor;
      }
    // On a desktop, the column that fills the box takes what the others leave of the room inside
    // the box's borders and beside its scrollbar (`fills`), to the whole pixel, so the table's
    // lines reach the border and no fraction of a pixel scrolls sideways. A column that yields
    // to it (`yields`) first gives it room where the others would leave it less than that column
    // leaves it, and stops at its minimum, at the narrowest its content needs for a column of
    // `fits`, and at what its header needs for one of `wrappedHeaders`.
    const fill = () => {
      if (fills === undefined || sizing[fills] !== undefined) return;
      const room = () => Math.floor(boxWidth - boxEdge) - others();
      for (const [id, leaving = 0] of Object.entries(yields)) {
        const width = widths[id];
        const short = leaving - room();
        if (
          width === undefined ||
          short <= 0 ||
          visibility[id] === false ||
          sizing[id] !== undefined
        )
          continue;
        widths[id] = Math.max(
          minimums[id] ?? 0,
          fitted[id]?.narrowest ?? 0,
          headerFloors[id] ?? 0,
          width - short
        );
      }
      widths[fills] = Math.max(minimums[fills] ?? 0, room());
    };
    if (pins === undefined) {
      if (desktop) fill();
      return { sizing: widths, pinned: [], compact: !desktop };
    }
    const { flexible, phone, wide } = pins;
    const covered = (pinned: readonly string[]) =>
      pinned.reduce((sum, id) => sum + (widths[id] ?? 0), 0);
    const flexibleWidth = widths[flexible];
    if (!desktop) {
      Object.assign(widths, phone.widths);
      const room = Math.min(boxWidth * phone.share, Math.floor(boxWidth - boxEdge));
      // Where the layout fills the box, the flexible column takes the room the other pinned
      // columns leave, whatever its own width, so a box of a share of 1 - a phone held
      // sideways, 500 to 639 px - has its lines reach its right border, as a desktop's box does
      // (`fill`); elsewhere, and at a width the reader dragged it to, it stops at its width.
      const spare = room - covered(phone.columns.filter((id) => id !== flexible));
      if (flexibleWidth !== undefined)
        widths[flexible] = Math.max(
          phone.flexibleMin,
          fills === undefined || sizing[flexible] !== undefined
            ? Math.min(flexibleWidth, spare)
            : spare
        );
      return { sizing: widths, pinned: [...phone.columns], compact: true };
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
    fill();
    return { sizing: widths, pinned, compact: false };
  }, [boxEdge, boxWidth, columns, measured, sizing, visibility]);

  const fitHeight = useCallback(() => {
    const box = scrollRef.current;
    if (!box) return;
    const bounds = box.getBoundingClientRect();
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
    // grows as well as shrinks, whatever stands above it. The place is the line's in the
    // document, not on the screen: read on the screen, a measure made while the document is
    // scrolled past the table - a window resize, the page growing - took the scroll for room, and
    // the browser's scroll anchoring then kept the pager where it was, so each measure grew the box
    // by the last one's growth, without end (the Priorities page's fault lists at 1600x1000,
    // scrolled to its end before the resize).
    const foot =
      (pagerRef.current?.getBoundingClientRect().bottom ?? bounds.bottom) + window.scrollY;
    const padding = (element: Element | null) =>
      element === null ? 0 : Number.parseFloat(getComputedStyle(element).paddingBottom) || 0;
    const slack =
      window.innerHeight - padding(box.parentElement) - padding(box.closest("main")) - foot;
    const available = bounds.height + slack;
    const height = Math.max(400, header + rowsHeight + 2, available);
    // The box takes the height at once as well as in the render its state draws next, so a host
    // that fits it right before it hands the focus on lands against the box the reader will see
    // (`features/project/IssuesTable.tsx`'s `returnFocus`). That render writes the same value.
    if (box.style.maxHeight !== "") box.style.maxHeight = `${height}px`;
    else if (box.style.height !== "") box.style.height = `${height}px`;
    setBoxHeight(height);
  }, [rowAttribute]);
  const measure = useCallback(() => {
    const box = scrollRef.current;
    if (!box) return;
    setBoxWidth(box.getBoundingClientRect().width);
    // The box's border box less the room inside it that is beside any scrollbar: its borders
    // and a classic scrollbar. Both are whole pixels, read where the page lays them out.
    setBoxEdge(box.offsetWidth - box.clientWidth);
    fitHeight();
  }, [fitHeight]);
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

  // The widths of the columns that fit what they hold (`fits`): each column's widest need, read
  // off its header - which `DataTable` names by the column's id (`data-column`) - and off the
  // cell under it in each row the box draws (`rowAttribute`), with what was read before off
  // the rows of the page the box no longer draws; and the narrowest each header that wraps
  // (`wrappedHeaders`) is drawn at as it is now, its words on as many lines as they take, read off
  // it at its min-content width. Undefined where nothing is to be measured or nothing is laid
  // out: the unit DOM, whose copies would fit every column to no width.
  const fit = useCallback((): Measured | undefined => {
    const { fits, wrappedHeaders } = columns;
    const box = scrollRef.current;
    if ((fits === undefined && wrappedHeaders === undefined) || box === null) return undefined;
    const fitsColumn = (id: string) => fits?.[id] !== undefined;
    const wrapsHeader = (id: string) => wrappedHeaders?.includes(id) === true;
    const { headers, rows } = needs.current;
    const held = pageRows.current;
    if (held !== undefined) for (const row of rows.keys()) if (!held.has(row)) rows.delete(row);
    const shown = [...box.querySelectorAll<HTMLTableCellElement>("thead th[data-column]")].filter(
      (header) =>
        fitsColumn(header.dataset.column ?? "") || wrapsHeader(header.dataset.column ?? "")
    );
    if (shown.length === 0 || shown.every((header) => header.offsetWidth === 0)) return undefined;
    const windowWidth = window.innerWidth;
    readWidth.current = windowWidth;
    const drawnRows = [...box.querySelectorAll<HTMLTableRowElement>(`tr[${rowAttribute}]`)];
    // The header and the cells whose markup changed since they were read, or that were read in
    // a window of another width, or never read.
    const unread: { cell: Element; narrowest: boolean; keep: (need: CellNeed) => void }[] = [];
    for (const header of shown) {
      const id = header.dataset.column ?? "";
      const markup = header.innerHTML;
      const headerRead = headers.get(id);
      if (headerRead?.markup !== markup || headerRead.windowWidth !== windowWidth)
        unread.push({
          cell: header,
          narrowest: wrapsHeader(id),
          keep: (need) => headers.set(id, { markup, need, windowWidth }),
        });
      if (!fitsColumn(id)) continue;
      const narrowest = fits?.[id]?.most !== undefined;
      for (const line of drawnRows) {
        const row = line.getAttribute(rowAttribute) ?? "";
        const cell = line.cells[header.cellIndex];
        if (cell === undefined || (held !== undefined && !held.has(row))) continue;
        const own = rows.get(row) ?? new Map<string, ReadNeed>();
        rows.set(row, own);
        const html = cell.innerHTML;
        const cellRead = own.get(id);
        if (cellRead?.markup !== html || cellRead.windowWidth !== windowWidth)
          unread.push({
            cell,
            narrowest,
            keep: (need) => own.set(id, { markup: html, need, windowWidth }),
          });
      }
    }
    const read = cellNeeds(unread);
    unread.forEach(({ keep }, at) => {
      const need = read[at];
      if (need !== undefined) keep(need);
    });
    drawn.current?.takeRecords();
    const ids = shown.map((header) => header.dataset.column ?? "");
    return {
      fitted: Object.fromEntries(
        ids.filter(fitsColumn).map((id) => {
          // A header of `fits` stands on one line, so its need is its line's at the narrowest too.
          const own = headers.get(id)?.need.line ?? 0;
          let line = own;
          let narrowest = own;
          for (const cells of rows.values()) {
            const need = cells.get(id)?.need;
            if (need === undefined) continue;
            line = Math.max(line, need.line);
            narrowest = Math.max(narrowest, need.narrowest);
          }
          const most = fits?.[id]?.most;
          const width = Math.ceil(
            most === undefined ? line : Math.max(narrowest, Math.min(most, line))
          );
          return [id, { narrowest: Math.min(width, Math.ceil(narrowest)), width }];
        })
      ),
      headerFloors: Object.fromEntries(
        ids.filter(wrapsHeader).map((id) => [id, Math.ceil(headers.get(id)?.need.narrowest ?? 0)])
      ),
    };
  }, [columns, rowAttribute]);
  // A measure that changed no width keeps the widths it had, so nothing is drawn anew.
  const keepFit = useCallback((next: Measured | undefined) => {
    if (next === undefined) return;
    setMeasured((current) => {
      const ids = Object.keys(next.fitted);
      const floors = Object.keys(next.headerFloors);
      const same =
        ids.length === Object.keys(current.fitted).length &&
        ids.every(
          (id) =>
            current.fitted[id]?.width === next.fitted[id]?.width &&
            current.fitted[id]?.narrowest === next.fitted[id]?.narrowest
        ) &&
        floors.length === Object.keys(current.headerFloors).length &&
        floors.every((id) => current.headerFloors[id] === next.headerFloors[id]);
      return same ? current : next;
    });
  }, []);
  const setPageRows = useCallback(
    (ids: readonly string[]) => {
      pageRows.current = new Set(ids);
      keepFit(fit());
    },
    [fit, keepFit]
  );
  useLayoutEffect(() => {
    if ((columns.fits === undefined && columns.wrappedHeaders === undefined) || box === null)
      return;
    keepFit(fit());
    // A resize can move a cell's padding across a breakpoint, which no markup says, so one that
    // changes the window's width reads the header and each drawn cell again, and counts each cell
    // the box does not draw as it was read until its row is drawn (`ReadNeed`). One that leaves
    // the window's width as it was, its height alone, reads nothing.
    const refit = () => {
      if (window.innerWidth !== readWidth.current) keepFit(fit());
    };
    window.addEventListener("resize", refit);
    // happy-dom has a `MutationObserver`, and lays nothing out, so `fit` reads nothing there.
    // The widths are drawn in the frame whatever was drawn anew is, before it is painted.
    const observer = new MutationObserver(() => {
      const next = fit();
      if (next !== undefined) flushSync(() => keepFit(next));
    });
    observer.observe(box, {
      attributeFilter: ["class"],
      characterData: true,
      childList: true,
      subtree: true,
    });
    drawn.current = observer;
    return () => {
      window.removeEventListener("resize", refit);
      observer.disconnect();
      drawn.current = null;
    };
  }, [box, columns.fits, columns.wrappedHeaders, fit, keepFit]);

  return {
    attachAbove,
    attachBox,
    boxHeight,
    boxWidth,
    fitHeight,
    order,
    pagerRef,
    responsive,
    scrollRef,
    setOrder,
    setPageRows,
    setSizing,
    setVisibility,
    sizing,
    visibility,
  };
}
