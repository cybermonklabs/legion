import {
  createContext,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { focusNoScroll, landNearest, scrollToNearest } from "../../components/focus-place";
import type { LandingTarget } from "../../components/SelectionBar";
import { useAddressNavigate, useHistoryEntry } from "../../lib/page-address";
import { focusOnDocument } from "../shell/roving";
import { answerDraft } from "./answer-drafts";
import { buildInboxPath, type InboxRoute } from "./inbox-route";
import { ROW_ATTRIBUTE, rowIn } from "./inbox-row";
import { type InboxListRow, rowId } from "./list-row";
import { PANE_SCROLLER_SELECTOR } from "./pane-placement";
import type { InboxSection } from "./sections";

/** A row as the reader last saw it: the band it was drawn under and the turn band it sat in,
 *  whatever the server says now. */
export interface PlacedRow {
  /** The grouping band the row was drawn under, whichever Group by the list is on. */
  group: string;
  row: InboxListRow;
  section: InboxSection;
}

/** The rows a row of the reader's stood between, as they last saw it: either is undefined
 *  where it stood at that end of the list. */
interface Neighbours {
  above: string | undefined;
  below: string | undefined;
}

/** What an Inbox address's history entry says about how its `?item=` got there. Kept in the
 *  entry itself, so Back, a reload and a remount read it exactly as the entry was written, and
 *  a `replace` that does not carry a mark drops it. */
export interface InboxEntryState {
  /** The column chose the row by itself - the first one the reader could see - rather than the
   *  reader picking it. Below the column's width such a row is not open: the list shows. */
  inboxDefaultPick?: true;
  /** The entry was pushed over the list's own by a row the reader opened below the column's
   *  width, so the way back to the list goes back through it. */
  inboxOverList?: true;
}

/** The Inbox's marks on a history entry's state, whatever else the entry holds. */
export function inboxEntryState(state: unknown): InboxEntryState {
  if (typeof state !== "object" || state === null) return {};
  const { inboxDefaultPick, inboxOverList } = state as Record<string, unknown>;
  return {
    ...(inboxDefaultPick === true ? { inboxDefaultPick: true } : {}),
    ...(inboxOverList === true ? { inboxOverList: true } : {}),
  };
}

type SelectMode = "column-default" | "push" | "replace";

/** What the address says on a commit, read against the rows the list holds: something this list
 *  has answered already, a listed row the reader linked or went Back to (now selected), or a row
 *  the list does not hold, or none. */
type AddressReading = "answered" | "listed" | "unlisted";

/** The list's empty message; the empty list itself where it draws no message; and the list where
 *  it draws no row because every row it holds is in a folded band (`Inbox.tsx`): what a hand-off
 *  gives the focus to when the reader's action leaves no row in sight (R8). */
const EMPTY_SELECTOR = "[data-inbox-empty]";

/** The reading view below 1280 px, marked with the row it shows (`ReadingView`). */
const READING_VIEW_SELECTOR = "[data-inbox-reading-row]";

/** Where a hand-off sends the reader: a row, by its `rowId`, or null for the list's empty
 *  message. */
type HandOffTarget = string | null;

/** One action of the reader's on rows of the list, and where it sends them. */
interface HandOff {
  /** The rows the action is on, by `rowId`. */
  readonly acted: ReadonlySet<string>;
  readonly target: HandOffTarget;
  /** What held the focus as the reader acted, which still holding it when the action lands
   *  says they are where they were. */
  readonly focused: Element | null;
  /** The row the column or the reading view showed as the reader acted, by `rowId`. */
  readonly selected: string | undefined;
}

/** The three moments of a hand-off: the action, its landing and its refusal. */
export interface InboxHandOff {
  /** At the action, before any optimistic write takes its rows off the list: where the reader
   *  goes is read off the list as they acted on it. */
  beginHandOff: (ids: readonly string[]) => void;
  /** Once the server has recorded the action on `ids` (every id it took): the row after them is
   *  selected beside the column, or read in the reading view below its width, and given the
   *  focus, while the reader has not moved on. */
  landHandOff: (ids: readonly string[]) => void;
  /** The server refused the action on `ids`: its rows are back, and nobody is sent anywhere. */
  dropHandOff: (ids: readonly string[]) => void;
}

/**
 * The hand-off the Inbox gives the controls it draws that act on a row without a prop of its
 * own: the Snooze of a row and of the reading column's head (`SnoozeControl`). Each Inbox
 * provides its own - the page's and the drawer's are separate - and with no provider a control
 * hands nothing on.
 */
export const InboxHandOffContext = createContext<InboxHandOff | undefined>(undefined);

/**
 * A press of a write on a short row beside the reading column that is not the item in hand: its
 * Snooze, its Assign to me or its hand-on, which the row draws only while that write is out or
 * refused (`AskShortRow`). R8 hands the reader on from the item in hand alone, so such a press
 * hands nobody on, and this runs at the press, before the write: a focus the press left in that
 * row, which the write may take out of the list, goes first, with no scroll, to the item being
 * read, as a Dismiss on such a row sends it (`Inbox.tsx`, `asideFromRow`). Undefined everywhere
 * else - on the item in hand, in a card, outside an Inbox - where a press is the item in hand's.
 */
export const RowAsideContext = createContext<(() => void) | undefined>(undefined);

/**
 * The row the reader goes to after acting on the rows `acted` (R8), in `drawn`, the rows as the
 * reader sees them: the first row after the last acted one, else the nearest row before it that
 * was not acted on, else none. A row the action is on is never the answer, so a bulk action's
 * rows are passed over together - for rows that stand together, the row before the first of
 * them - and a folded band's rows are not in `drawn` at all. None too where no acted row is
 * drawn, since there is then no place to go on from.
 */
export function itemAfter(
  drawn: readonly PlacedRow[],
  acted: ReadonlySet<string>
): string | undefined {
  const ids = drawn.map(({ row }) => rowId(row));
  let last = ids.length - 1;
  while (last >= 0 && !acted.has(ids[last] ?? "")) last -= 1;
  if (last === -1) return undefined;
  const after = ids[last + 1];
  if (after !== undefined) return after;
  for (let before = last - 1; before >= 0; before -= 1) {
    const id = ids[before];
    if (id !== undefined && !acted.has(id)) return id;
  }
  return undefined;
}

export interface PaneSelectionOptions {
  /** Every row the list could hold before its five fields narrow it (`Inbox`'s `candidates`). */
  candidates: readonly InboxListRow[];
  /** Whether the reading column stands beside the list (`READING_PANE_QUERY`, 1280 px). There a
   *  row is always selected - the first one the reader can see, until they pick another - and
   *  the address follows the selection. Below it the address is the selection: no `?item=` is
   *  the list with nothing open, and a row the reader opens is a new history entry whose reading
   *  view takes the list's place. */
  columns: boolean;
  /** The address this list is on. The selection is written into its `?item=`. */
  filter: InboxRoute;
  /** Whether the reader's own write took the row with this id out of the list - their answer,
   *  snooze, dismiss, Assign to me or hand-on - rather than the server on its own. */
  leftByReader: (id: string) => boolean;
  /** The list's root, where the selected row's own element is looked up. */
  listRef: RefObject<HTMLElement | null>;
  /** Below the column's width, the row with this id, which the reading view showed, left the list
   *  without the reader's own write, and the list is back at that place. */
  onOpenRowLeft: (id: string) => void;
  /** Opens the folded band, for an `?item=` naming a row the reader deferred. */
  openFoldedBand: () => void;
  /** Whether this list selects at all: the Inbox page does at every width, the drawer never
   *  does. Without it there is no selection, no reading view shows the row, and nothing is
   *  written to the address. */
  pane: boolean;
  /** The rows the reader can see, in the order they see them, as the list last rendered them.
   *  The list keeps this record for its own motion hold, and the selection is answered against
   *  exactly the rows in it. */
  presented: RefObject<readonly PlacedRow[]>;
  /** This list's own part of the page - its rows, or its empty message - where a hand-off's
   *  target is looked up, so the drawer's list never reaches into the page under it. */
  region: RefObject<HTMLElement | null>;
  /** Whether the list holds every row it will draw from this read: the asks' read has landed and
   *  the thread side has answered too, or has none to read (`useInboxThreadRows`'s `ready`). The
   *  thread rows come from a read of their own, which can land after the asks', so until then
   *  the selection is not settled at all: a `?item=` naming a thread row would read as a row
   *  this list does not hold, which closes the reading view below the column's width and has the
   *  column pick its top row beside the list. */
  rowsReady: boolean;
}

export interface PaneSelection extends InboxHandOff {
  /** Leaves the row's reading view for the list, below the column's width: back through the
   *  history entry `open` pushed where the entry says it did, else to the address with no
   *  `?item=`. The list then hands the focus to the row the reader left, scrolled into view. */
  close: () => void;
  /** The row the column or the reading view is showing, by its `rowId`, or undefined while it
   *  shows none. */
  id: string | undefined;
  /** The reader's own pick of a row, by a click or Enter: from the column's width it is
   *  `select`; below it, it opens the row's reading view in place of the list as a new history
   *  entry, so the browser's Back returns to the list as the view's own Inbox control does. */
  open: (id: string) => void;
  /** Selects a row, or nothing, and writes it to the address in place of the entry it is on.
   *  Below the column's width, nothing selected on an entry `open` pushed goes back through it. */
  select: (id: string | undefined) => void;
  /** The selected row's own element, or null while the list holds none. */
  selectedRow: () => HTMLElement | null;
  /** Selects the row `delta` from the selected one, in the order the reader sees: what `j`, `k`
   *  and the arrows do while the reading column or view is on, in place of roving the focus. */
  step: (delta: 1 | -1) => void;
  /**
   * Every row the list holds this commit, a folded band's included, and, from the rows the
   * reader can see, the row the column shows. Called once per commit, after `presented`
   * is written: where the selection would go next is read there, off the rows as they stand,
   * rather than when it is wanted.
   */
  showing: (listed: readonly PlacedRow[]) => InboxListRow | undefined;
  /** Takes the landed hand-off whose target has not taken the focus yet off the record, and
   *  answers that target - the row after the rows acted on, or the list's empty message - as a
   *  reading of the element this list draws for it, null while it draws none; undefined where no
   *  hand-off is waiting. The bar of selected rows lands it itself (`SelectionBar`'s
   *  `useLastMarkedPlace`): once the rows its press takes away have gone and the target is drawn,
   *  with the list's own stop holding the focus until then. So one landing runs per press, and
   *  this list's own focus of the target never runs for it (`Inbox.tsx`'s `returnFocus`). */
  takeHandOff: () => LandingTarget | undefined;
}

/**
 * The Inbox reading column's selection: which row the column shows, how the keys and the
 * address move it, and what keeps it on a question whose row has gone. And, at every width, the
 * hand-off after the reader's own action (R8): the row after the rows they acted on is selected
 * beside the column and its row takes the keyboard focus; below the column's width it takes the
 * focus in the list, and in a reading view the view reads it, its scroller taking the focus.
 *
 * The address holds the selection (`?item=`), so a reload or a link keeps the reader's place.
 * From the column's width a row is always selected; below it a row is selected only while its
 * reading view is open, so there an address with no `?item=` is the list. In the drawer there
 * is no selection and nothing is written.
 */
export function usePaneSelection({
  candidates,
  columns,
  filter,
  leftByReader,
  listRef,
  onOpenRowLeft,
  openFoldedBand,
  pane,
  presented,
  region,
  rowsReady,
}: PaneSelectionOptions): PaneSelection {
  const go = useAddressNavigate();
  const historyEntry = useHistoryEntry();
  const entry = inboxEntryState(historyEntry.state);
  const [selectedId, setSelectedId] = useState<string | undefined>(undefined);
  const selectedNow = useRef<string | undefined>(undefined);
  selectedNow.current = selectedId;
  /** The `?item=` this list has already answered: without it, writing the selection to the
   *  address would read back as a reader's own navigation and select the row again, one step
   *  behind every `j`. */
  const shownItem = useRef<string | undefined>(undefined);

  /** Every row this list holds, a folded band's included: what `?item=` is answered against. */
  const listedRows = useRef<readonly PlacedRow[]>([]);
  /** The row the column is showing, as the Inbox last held it, and whether it is being kept
   *  there past its row. */
  const paneSubject = useRef<{ row: InboxListRow; kept: boolean } | undefined>(undefined);
  // The rows the selected one stood between when the list last held it, either of them
  // undefined where it stood at that end: where it stood, rather than the one row the
  // selection would move to, because that one row cannot say where it stood. For the last row
  // there is nothing below, so the row above names it, and a step reading that as the row
  // which took its place walks up for `j` and over a row for `k`.
  //
  // Read on every commit while the list holds the selected row, and kept as it was once the
  // list no longer does: the commit that drops the row is the one the settling effect reads
  // this on, and the reader's own answer takes its row once the server records it, so where
  // that answer sends them is read here too. A snooze and a dismiss do not read it: they say
  // where they send the reader as they act (`beginHandOff`), off the list as it stood then,
  // since a snooze moves its row into `Later`, which is still drawn while the band is open, and
  // the list keeps that row where it stood until the write lands.
  const stoodBetween = useRef<Neighbours>({ above: undefined, below: undefined });
  /** Where the selection goes when the row it is on leaves the bands: the next row the reader
   *  can see after it, else the nearest above it, so the column always shows something they
   *  can act on. */
  const nextAfterSelected = (): string | undefined =>
    stoodBetween.current.below ?? stoodBetween.current.above;

  /** The history entry `close` went back from (`HistoryEntry.key`) while that step is under way.
   *  A browser's `history.go` lands a frame or more later, so until it does the address still
   *  names the row the reader closed: read as a new address, it would select that row again. */
  const leaving = useRef<string | undefined>(undefined);
  const listAnswered = useRef(false);
  /** Below the column's width, the row that now stands where the row the reading view showed
   *  stood, when that row left the list on its own: the list comes back with the focus there. */
  const returnTo = useRef<string | undefined>(undefined);

  /** The actions begun and not yet landed: a snooze whose write is out. */
  const begun = useRef<readonly HandOff[]>([]);
  /** The hand-off that has landed and whose target has not taken the focus yet. */
  const focusTo = useRef<HandOff | undefined>(undefined);
  /** A target `focusTarget` gave the focus with no scroll while rows of `acted` still stood
   *  before it: once none does, it is shown whole at its nearest edge by the least scroll where
   *  the reader cannot see the whole of it (`landNearest`), while it still holds the focus. */
  const intoView = useRef<{ acted: ReadonlySet<string>; target: HTMLElement } | undefined>(
    undefined
  );
  /** The row a landed hand-off has just selected (`landHandOff`), which the selection's keeping
   *  on screen (below) leaves to that hand-off's own landing: it is selected as the reader acts,
   *  while the rows acted on may still stand above it, and a scroll to it there would carry the
   *  view past their place by their height once they have gone. */
  const selectedByHandOff = useRef<string | undefined>(undefined);
  /** A commit for the focus to move in: a landing that selects no row - in the drawer, and in the
   *  short-row list below the column's width (`onShownRow`) - changes nothing else that renders. */
  const [, setLandings] = useState(0);

  const select = (id: string | undefined, mode: SelectMode = "replace"): void => {
    setSelectedId(id);
    selectedNow.current = id;
    shownItem.current = id;
    const written: InboxEntryState =
      mode === "push"
        ? { inboxOverList: true }
        : id === undefined
          ? {}
          : inboxEntryState({
              inboxDefaultPick: mode === "column-default",
              inboxOverList: entry.inboxOverList,
            });
    if (
      id === filter.item &&
      written.inboxDefaultPick === entry.inboxDefaultPick &&
      written.inboxOverList === entry.inboxOverList
    ) {
      return;
    }
    // Replace, but for a row the reader opens below the column's width: a reader walking the
    // list with `j` would otherwise have to press Back once per row to leave the page, the rule
    // the assignee filter already holds to (`lib/page-address.ts`). Selecting nothing clears
    // `?item=` rather than leaving it on the row that has just left: a reload reads the address,
    // and an address naming a row the reader deferred would open the folded band and put that
    // question back in front of them.
    go(buildInboxPath({ ...filter, item: id }), {
      replace: mode !== "push",
      state: Object.keys(written).length === 0 ? undefined : written,
    });
  };

  /** Below the column's width, the way from a row's reading view to the list: back through the
   *  entry `open` pushed over the list's own, rather than writing the list a second entry the
   *  next Back would only land on again, and else nothing selected on the entry the view is on.
   *  The view stays as it is until a back step lands, so nothing is drawn for an address it is
   *  about to leave. */
  const leaveView = (): void => {
    if (entry.inboxOverList !== true) {
      select(undefined);
      return;
    }
    if (leaving.current !== historyEntry.key) {
      leaving.current = historyEntry.key;
      historyEntry.back();
    }
  };

  const step = (delta: 1 | -1): void => {
    const visible = presented.current;
    const at = visible.findIndex((placed) => rowId(placed.row) === selectedId);
    if (at !== -1) {
      const next = visible[at + delta];
      if (next !== undefined) select(rowId(next.row));
      return;
    }
    // The column is holding a question past its row (their unsent answer), so there is no row to
    // step from: the place that row held is read off the rows it stood between, and the step runs
    // from there rather than from the top of the list. The row it stood above took its place;
    // where it stood last there is no such row, and the row it stood below is the one before the
    // place it held.
    const { above, below } = stoodBetween.current;
    const belowAt = visible.findIndex((placed) => rowId(placed.row) === below);
    const aboveAt = visible.findIndex((placed) => rowId(placed.row) === above);
    const stood = belowAt !== -1 ? belowAt : aboveAt !== -1 ? aboveAt + 1 : -1;
    const next =
      stood === -1
        ? visible[delta === 1 ? 0 : visible.length - 1]
        : visible[delta === 1 ? stood : stood - 1];
    if (next !== undefined) select(rowId(next.row));
  };

  /** The row the column is showing, or null while it is showing none. Held to the selection,
   *  because the effect keeping that row on screen is the selection's and nothing else's. */
  const selectedRow = useCallback(
    () => rowIn(listRef.current, selectedId ?? null),
    [listRef, selectedId]
  );

  /** The row the reader has open, by the rule of the width the Inbox is at. Beside the list it is
   *  the selection. Below it, a row the column picked by itself is no row the reader opened:
   *  nothing is drawn for it, not even for the one commit before `settleReadingView` clears it. */
  const openId = columns || entry.inboxDefaultPick !== true ? selectedId : undefined;
  /** Whether the keys and the hand-off act on the row shown rather than on the row holding the
   *  focus: beside the column, and below it while a reading view is open. */
  const onShownRow = (): boolean => pane && (columns || selectedNow.current !== undefined);

  /** Whether the list still draws any row of `acted`. */
  const drawsAny = (acted: ReadonlySet<string>): boolean =>
    presented.current.some((placed) => acted.has(rowId(placed.row)));
  /** The reading view below the column's width, while it is drawn in this list's part of the
   *  page. */
  const readingView = (): HTMLElement | null =>
    region.current?.querySelector<HTMLElement>(READING_VIEW_SELECTOR) ?? null;
  /** The element `target` names in this list: the row, or the empty message. While a reading view
   *  is drawn its list is hidden behind it, so the row the hand-off sends the reader to is the
   *  view's scroller once the view reads that row - the focus stop `l` moves to beside the list,
   *  where Space scrolls and Enter reaches the answer box - and nothing while it reads another;
   *  the empty message is drawn once the view has closed. */
  const targetElement = (target: HandOffTarget): HTMLElement | null => {
    const view = readingView();
    if (view !== null) {
      return target !== null && view.dataset.inboxReadingRow === target
        ? view.querySelector<HTMLElement>(PANE_SCROLLER_SELECTOR)
        : null;
    }
    return target === null
      ? (region.current?.querySelector<HTMLElement>(EMPTY_SELECTOR) ?? null)
      : rowIn(region.current, target);
  };
  /** Whether a row of `acted` still stands before `target` in this list: the target then
   *  stands below where it will stand once they have gone. */
  const actedBefore = (acted: ReadonlySet<string>, target: HTMLElement): boolean =>
    [...acted].some((id) => {
      const row = rowIn(region.current, id);
      return (
        row !== null &&
        (row.compareDocumentPosition(target) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
      );
    });
  /**
   * Gives a hand-off's `target` the keyboard focus. While a row of `acted` still stands before
   * it in this list, with no scroll: the rows acted on go in a later commit - a dismiss's
   * optimistic removal, an answered row the list lets go of, a snooze folding its rows away -
   * and their going moves the target up into the place the reader saw them in, where the list's
   * anchor holds it (`ViewportAnchor`). A scroll to the target where it stands below them would
   * carry the view past that place by the height still to go. So it is shown whole only once
   * they have gone (`intoView`), where the reader cannot see the whole of it then; with none of
   * them before it, at once. Either way it is shown at its nearest edge by the least scroll,
   * against the window's and each box's visible edges and not the root's `scroll-padding-bottom`
   * below 1280 px (`landNearest`), which the engine's own nearest-edge scroll counts: the rule the
   * selection bar's landing takes (`components/SelectionBar.tsx`).
   */
  const focusTarget = (acted: ReadonlySet<string>, target: HTMLElement): void => {
    if (actedBefore(acted, target)) {
      focusNoScroll(target);
      intoView.current = { acted, target };
    } else {
      landNearest(target);
    }
  };
  /** Takes the begun action on rows that include every one of `ids` off the record, and gives
   *  it back, or undefined where none was begun on them. */
  const takeBegun = (ids: readonly string[]): HandOff | undefined => {
    const handOff =
      ids.length === 0
        ? undefined
        : begun.current.find((each) => ids.every((id) => each.acted.has(id)));
    if (handOff !== undefined) begun.current = begun.current.filter((each) => each !== handOff);
    return handOff;
  };

  /**
   * Where an action that began nothing goes once it lands - the reader's answer, an approval, a
   * request for changes, which the server records before anything moves: the row after the
   * acted ones while the list still draws them - in the short-row list below the column's width
   * it holds the row under the reader's hand - and, beside the column or in the reading view,
   * which keep the answered question past its row, the row below where it stood, else the row
   * above, else the first. Undefined where nothing says where the row stood.
   */
  const targetNow = (acted: ReadonlySet<string>): HandOffTarget | undefined => {
    const visible = presented.current;
    if (drawsAny(acted)) return itemAfter(visible, acted) ?? null;
    if (!onShownRow() || selectedNow.current === undefined || !acted.has(selectedNow.current)) {
      return visible.length === 0 ? null : undefined;
    }
    const next = nextAfterSelected();
    if (next !== undefined && visible.some((placed) => rowId(placed.row) === next)) return next;
    const first = visible[0];
    return first === undefined ? null : rowId(first.row);
  };

  /**
   * Whether the reader is still where they acted, so the landing may move them: beside the
   * column, and in the reading view below its width, the selection is still on a row acted on,
   * or on the row the action sends them to, or where it was as they acted; in the short-row list
   * and the drawer the focus is on nothing, in a row acted on or the row it sends them to, or
   * still on what held it as they acted. A reader who has walked off - another row selected, the
   * focus somewhere of their own - is left where they are.
   */
  const stillThere = (handOff: HandOff): boolean => {
    const { acted, target } = handOff;
    if (columns || (pane && handOff.selected !== undefined)) {
      const now = selectedNow.current;
      return now !== undefined && (acted.has(now) || now === target || now === handOff.selected);
    }
    if (focusOnDocument()) return true;
    const active = document.activeElement;
    const row = active?.closest<HTMLElement>(`[${ROW_ATTRIBUTE}]`)?.getAttribute(ROW_ATTRIBUTE);
    return (
      (row !== null && row !== undefined && (acted.has(row) || row === target)) ||
      (handOff.focused !== null && active === handOff.focused)
    );
  };

  const beginHandOff = (ids: readonly string[]): void => {
    const acted = new Set(ids);
    begun.current = [
      ...begun.current.filter((handOff) => !ids.some((id) => handOff.acted.has(id))),
      {
        acted,
        focused: document.activeElement,
        selected: selectedNow.current,
        target: itemAfter(presented.current, acted) ?? null,
      },
    ];
  };

  const dropHandOff = (ids: readonly string[]): void => {
    takeBegun(ids);
  };

  const landHandOff = (ids: readonly string[]): void => {
    const began = takeBegun(ids);
    const acted = began?.acted ?? new Set(ids);
    // The row the action began with, while the list still draws it; else where the action goes
    // read now, since the server may have taken that row away meanwhile.
    const target =
      began !== undefined &&
      (began.target === null ||
        presented.current.some((placed) => rowId(placed.row) === began.target))
        ? began.target
        : targetNow(acted);
    if (target === undefined) return;
    const shownRow = onShownRow();
    // An action that began nothing - the reader's answer, which the list keeps where they saw it
    // until Dispatch records it (`recordAnswered`) - holds no record of where the reader stood as
    // they acted. In the reading view below the column's width the row the view reads is that
    // record: while it is a row answered, the reader is still where they answered, and is handed
    // on from it as from a dismiss, to the next row or to the empty message. While it is another
    // row, they have moved on - to the row the answer hands on to as well, perhaps with the caret
    // in its answer box - and the landing leaves them where they are.
    const read = began === undefined && shownRow && !columns ? selectedNow.current : undefined;
    if (read !== undefined && !acted.has(read)) return;
    const handOff: HandOff = {
      acted,
      focused: began?.focused ?? null,
      selected: began?.selected ?? read,
      target,
    };
    if (!stillThere(handOff)) return;
    // The empty message is no row to select. Beside the column, and in the reading view, a
    // selected row the action took off the list - an answered question, which the column kept
    // past its row until the server decided it - goes with the selection, so the column shows
    // nothing once nothing is left, and the reading view gives way to the list: the settling
    // effect leaves a question the column keeps where it is. A row the list still draws - a
    // snooze of the last rows with the Later band open, or a dismiss whose rows go at the next
    // commit - keeps the selection, which leaves with it as the settling effect hands it on. In
    // the reading view the row selected is the one the view reads next.
    if (shownRow && target !== null) {
      // Its own landing shows the row once the rows acted on have gone (`selectedByHandOff`).
      if (target !== selectedNow.current) selectedByHandOff.current = target;
      select(target);
    } else if (shownRow && !drawsAny(acted) && acted.has(selectedNow.current ?? "")) {
      if (columns) select(undefined);
      else leaveView();
    }
    focusTo.current = handOff;
    // The target can wait on a commit still to come - the empty message is drawn once the acted
    // rows go - and the reader's next press says they have moved on without it: a snooze of the
    // last rows with the Later band open leaves them drawn, and no message comes.
    const movedOn = () => {
      if (focusTo.current === handOff) focusTo.current = undefined;
      document.removeEventListener("keydown", movedOn, true);
      document.removeEventListener("pointerdown", movedOn, true);
    };
    document.addEventListener("keydown", movedOn, true);
    document.addEventListener("pointerdown", movedOn, true);
    setLandings((count) => count + 1);
  };

  /** The hand-off's methods as this render made them, for the stable ones handed out below: a
   *  write's callback lands long after the render that began it, and must select through this
   *  render's address and mode. */
  const handOffs = useRef<InboxHandOff>({ beginHandOff, dropHandOff, landHandOff });
  handOffs.current = { beginHandOff, dropHandOff, landHandOff };
  const [stableHandOffs] = useState<InboxHandOff>(() => ({
    beginHandOff: (ids) => handOffs.current.beginHandOff(ids),
    dropHandOff: (ids) => handOffs.current.dropHandOff(ids),
    landHandOff: (ids) => handOffs.current.landHandOff(ids),
  }));

  /**
   * What the column shows this commit, given the selected row among the ones the list renders
   * (none, where it renders none at all). Normally that row, which leaves the column as it
   * leaves the list. The reader's unsent answer to a question outlives its row: a question whose
   * row the server dropped while they had typed one into its box stays in the column - the card
   * draws its closed state, with that text under it - until they select another row, so an
   * answer they are in the middle of writing is never taken off the screen under their hands.
   *
   * Their sent answer needs no hold of the column's: the list keeps the row where they saw it
   * while the answer or a decline is out (`Inbox.tsx`, `heldRow`), though the write takes it out
   * of the Inbox read, so the selected row is among those the list renders. The column hands the
   * selection on when the server records the answer (`recordAnswered`), and a refusal leaves the
   * question here with the card's own refusal and its Retry under what they answered.
   *
   * A kept row is drawn as its row among `candidates` where it is one, else as last seen.
   *
   * Any other row - an ask they have typed no answer into, and every thread row, which a dismiss
   * takes out of the list under its own optimistic update - leaves the column as it leaves the
   * list, the first row this list selected for them included.
   */
  const paneRow = (listed: InboxListRow | undefined): InboxListRow | undefined => {
    const subject = paneSubject.current;
    if (!pane || openId === undefined) {
      paneSubject.current = undefined;
      return undefined;
    }
    if (listed !== undefined) {
      paneSubject.current = { kept: false, row: listed };
      return listed;
    }
    if (subject === undefined || rowId(subject.row) !== openId) {
      paneSubject.current = undefined;
      return undefined;
    }
    const kept = subject.kept || answerDraft(openId) !== "";
    const row = candidates.find((candidate) => rowId(candidate) === openId) ?? subject.row;
    paneSubject.current = kept ? { kept: true, row } : undefined;
    return kept ? row : undefined;
  };

  const showing = (listed: readonly PlacedRow[]): InboxListRow | undefined => {
    listedRows.current = listed;
    listAnswered.current = true;
    const visible = presented.current;
    // Read here, where the rows stand as the reader sees them, and left as it was on a commit
    // whose list no longer holds the selected row at all: that is the commit the settling effect
    // reads this on, and the answer it wants is the one taken while the row was still there.
    if (selectedId === undefined) {
      stoodBetween.current = { above: undefined, below: undefined };
    } else {
      const at = visible.findIndex(({ row }) => rowId(row) === selectedId);
      if (at !== -1) {
        stoodBetween.current = {
          above: visible[at - 1] === undefined ? undefined : rowId(visible[at - 1].row),
          below: visible[at + 1] === undefined ? undefined : rowId(visible[at + 1].row),
        };
      }
    }
    return paneRow(visible.find(({ row }) => rowId(row) === selectedId)?.row);
  };

  /** Where the selection goes once the row it is on has left the bands: where an action of the
   *  reader's still in the air said as they acted - a bulk snooze folds its rows at once, and the
   *  row below the selected one may be going with it - else where that row was heading. */
  const nextAfterLeft = (): string | undefined => {
    const handedTo =
      selectedId === undefined
        ? undefined
        : begun.current.find((handOff) => handOff.acted.has(selectedId))?.target;
    return handedTo ?? nextAfterSelected();
  };

  /** The step both widths share: the address is read as the reader's own move - the first
   *  visit, a link, a reload, the browser's Back - exactly while it says something this list has
   *  not already answered. A row it holds is selected, its band opened first if the reader linked
   *  into a folded one: following the link is what they asked for. */
  const readAddress = (visible: readonly PlacedRow[]): AddressReading => {
    if (filter.item === shownItem.current) return "answered";
    shownItem.current = filter.item;
    const wanted = listedRows.current.find((placed) => rowId(placed.row) === filter.item);
    if (wanted === undefined) return "unlisted";
    const id = rowId(wanted.row);
    setSelectedId(id);
    if (!shown(visible, id)) openFoldedBand();
    return "listed";
  };

  /** From the column's width a row is always selected. An address the list cannot answer leaves
   *  the first visible row selected, as the column's own pick, and the address rewritten; a row
   *  that leaves hands the selection to where it was heading. Otherwise the selection is the
   *  reader's, and the address follows it. */
  const settleBesideList = (): void => {
    // A list that holds nothing - still loading, failed, or genuinely empty - answers no
    // address: `?item=` is left unread until there are rows to read it against. A selected row
    // that went with the rest - the last row dismissed, or taken away by the server, so that no
    // candidate is that row any more - leaves the selection and the address, unless the column
    // keeps its question for the reader's answer. A row the typed text or the filter fields only
    // hide is still a candidate: it stays selected and named in the address, so taking that
    // narrowing off brings the reader back to the item they were reading.
    if (listedRows.current.length === 0) {
      if (
        selectedId !== undefined &&
        paneSubject.current?.kept !== true &&
        !candidates.some((row) => rowId(row) === selectedId)
      ) {
        select(undefined);
      }
      return;
    }
    const visible = presented.current;
    const reading = readAddress(visible);
    if (reading === "listed") return;
    // Back onto an entry that names no row - the one a row opened below the column's width was
    // pushed over - is the reader's own way back: the column takes its first row by itself,
    // rather than writing back the row the Back left.
    const top = visible[0];
    if (
      reading === "unlisted" &&
      filter.item === undefined &&
      historyEntry.navigationType === "POP" &&
      top !== undefined
    ) {
      select(rowId(top.row), "column-default");
      return;
    }
    if (shown(visible, selectedId)) {
      if (selectedId !== filter.item) select(selectedId);
      return;
    }
    // A question the column is keeping for the reader's unsent answer keeps the selection with
    // it: the row has gone, the question has not, and only their next pick ends that.
    if (paneSubject.current?.kept === true) return;
    // The row the selection was on has left the bands - the server took it away, or the reader
    // acted on it - so the selection goes where that row was heading, a pick of the column's own
    // staying its own, and with nowhere to go to the first row the reader can see.
    const next = nextAfterLeft();
    if (shown(visible, next)) {
      select(next, entry.inboxDefaultPick === true ? "column-default" : "replace");
      return;
    }
    select(top === undefined ? undefined : rowId(top.row), "column-default");
  };

  /** Below the column's width the address is the selection: no `?item=` is the list, and a row
   *  the column picked by itself is not open either. An open row the reader's own write takes
   *  out of the list hands the view to where it was heading; one that leaves on its own brings
   *  the list back at its place, saying so; with nowhere to go the view gives way to the list. */
  const settleReadingView = (): void => {
    // An address that names no row is the list with nothing open, whatever was open before: the
    // browser's Back, or the reading view's own Inbox control. Read before the rows are, since
    // an empty list is still a list to come back to.
    if (filter.item === undefined) {
      shownItem.current = undefined;
      if (selectedId !== undefined) setSelectedId(undefined);
      return;
    }
    if (entry.inboxDefaultPick === true) {
      leaveView();
      return;
    }
    // Before the Inbox's read has landed there are no rows to read `?item=` against, so it is
    // left unread. Once the list has answered and holds nothing, the row the address names
    // cannot be shown: the view gives way to the list - saying so when the row left on its own -
    // unless it is keeping a question for an unsent answer.
    if (listedRows.current.length === 0) {
      if (listAnswered.current && paneSubject.current?.kept !== true) {
        if (selectedId !== undefined && !leftByReader(selectedId)) onOpenRowLeft(selectedId);
        leaveView();
      }
      return;
    }
    const visible = presented.current;
    if (readAddress(visible) === "listed") return;
    if (shown(visible, selectedId)) {
      if (selectedId !== filter.item) select(selectedId);
      return;
    }
    if (paneSubject.current?.kept === true) return;
    const next = nextAfterLeft();
    // The list is hidden behind the view, so a row that left on its own is not swapped for one
    // the reader never opened: the list comes back at that place, and says the row left.
    if (selectedId !== undefined && !leftByReader(selectedId)) {
      returnTo.current = shown(visible, next) ? next : undefined;
      onOpenRowLeft(selectedId);
      leaveView();
      return;
    }
    if (shown(visible, next)) {
      select(next);
      return;
    }
    leaveView();
  };

  // What the selection is, settled after each commit against the rows this list holds, by the
  // rule of the width the Inbox is at: one or the other, chosen once. Not before every row the
  // list will draw has arrived (`rowsReady`): the address, a reload's or a link's, is read once.
  //
  // On the layout lane, so that the selection a commit hands on is drawn in the same task as that
  // commit. The commit that takes the selected row off the list draws no column, and takes away
  // whatever in the column held the focus; the next row's column is then drawn in the same task,
  // and the Inbox hands the focus to the new column's own focus stop in that commit (`Inbox.tsx`).
  useLayoutEffect(() => {
    if (!pane || !rowsReady) return;
    // A back step `leaveView` started is under way: the address still names the row it is
    // leaving, and is read again once the step lands.
    if (leaving.current !== undefined) {
      if (leaving.current === historyEntry.key) return;
      leaving.current = undefined;
    }
    if (columns) settleBesideList();
    else settleReadingView();
  });

  /** Gives the element a landed hand-off is waiting to give the focus to - the row after the rows
   *  acted on, or the list's empty message - the focus now, by the hand-off's own rule
   *  (`focusTarget`), and answers whether one was waiting. The effect below runs it after every
   *  commit. */
  const focusHandOff = (): boolean => {
    const handOff = focusTo.current;
    const target = handOff === undefined ? null : targetElement(handOff.target);
    if (handOff === undefined || target === null) return false;
    focusTo.current = undefined;
    focusTarget(handOff.acted, target);
    return true;
  };

  // The hand-off's focus, after every commit: the row the reader's action sends them to, or the
  // list's empty message, takes the keyboard focus once the page draws it (`focusHandOff`), and
  // is shown whole at its nearest edge by the least scroll where the reader cannot see the whole
  // of it (`landNearest`) - once no row acted on stands above it - since WebKit scrolls a focused
  // element into view only a frame later; a row in sight moves nothing. After every commit,
  // because the acted row and the control that held the focus leave in their own commits - a
  // closing dialog's own return of the focus (`useDialog`), the column's card swapped for the
  // next one, a row taken off the list - and a passive effect runs after their cleanups, so the
  // target wins over each of them. The empty message is drawn only once the acted rows have gone,
  // so the hand-off waits for it while they are still drawn, and is spent once they have gone
  // without it. A reading view below the column's width waits too, while it reads another row or
  // is still to close onto the empty list (`targetElement`).
  //
  // An action still in the air can lose its rows first - a bulk snooze folds them at once - and,
  // with the focus in one of them, the focus with them: the target takes it then rather than
  // leave it on the body, and the landing finds the reader there.
  //
  // A target given the focus while rows acted on still stood before it is shown whole in the
  // first commit that finds none of them there (`intoView`): after the list's anchor has held it
  // at their place, which leaves a target in sight there where it is.
  useEffect(() => {
    const inView = intoView.current;
    if (
      inView !== undefined &&
      (document.activeElement !== inView.target || !actedBefore(inView.acted, inView.target))
    ) {
      intoView.current = undefined;
      if (document.activeElement === inView.target) landNearest(inView.target);
    }
    const handOff = focusTo.current;
    if (handOff !== undefined) {
      if (!focusHandOff() && !drawsAny(handOff.acted) && readingView() === null) {
        focusTo.current = undefined;
      }
      return;
    }
    if (!focusOnDocument()) return;
    for (const handOff of [...begun.current].reverse()) {
      if (drawsAny(handOff.acted)) continue;
      const target = targetElement(handOff.target);
      if (target !== null) landNearest(target);
      return;
    }
  });

  // The selected row is kept on screen as the reader walks the list: the column beside it shows
  // the row they cannot see otherwise. It is shown at its nearest edge by the least scroll, by the
  // window's and each box's visible edges and not the root's scroll padding (`scrollToNearest`),
  // as the hand-off lands its target (`landNearest`), but moves no focus: the reader's focus can
  // be in the column while `j` and `k` step the selection. A row a landed hand-off selected is
  // that hand-off's to show (`selectedByHandOff`): beside the column the bar's Dismiss selects the
  // row after the marked ones in the commit the bar goes in, while the marked rows still stand
  // above it, and a scroll to it then carried the list's box 162 px, the height of the two rows
  // it took away, past the cut of the row marked last (builds Job `infra-2160-r22-l-qs6mx`,
  // `e2e/table-controls.e2e.ts`, Chromium and Firefox at 1440x900). Below the column's width the
  // reading view stands in the list's place, so there is no list to keep it in.
  useEffect(() => {
    const handed = selectedByHandOff.current;
    selectedByHandOff.current = undefined;
    if (!columns || selectedId === undefined || selectedId === handed) {
      return;
    }
    const row = selectedRow();
    if (row !== null) scrollToNearest(row);
  }, [columns, selectedId, selectedRow]);

  // Below the column's width, the commit that closes a row's reading view brings the list back
  // at that row: it takes the focus, so the keys walk on from it, and is scrolled to the middle
  // of the screen. A layout effect, so the row is placed in the commit that shows the list
  // again rather than one frame after the list painted at its top. Only a row a reading view was
  // drawn for is remembered: the column's own row, dropped as the window narrows, never had
  // one, so the focus stays where the reader has it - in the search box, say. A row the reader's
  // own action took out of the view is that action's to hand on (R8, `landHandOff`): the list
  // comes back with the focus where the hand-off sends it, not on the row that is leaving.
  const lastSelected = useRef<string | undefined>(undefined);
  useLayoutEffect(() => {
    const left = lastSelected.current;
    lastSelected.current = columns ? undefined : openId;
    if (!pane || columns || left === undefined || selectedId !== undefined) {
      return;
    }
    const back = returnTo.current;
    returnTo.current = undefined;
    if (focusTo.current?.acted.has(left) === true) return;
    const row = rowIn(listRef.current, left) ?? rowIn(listRef.current, back ?? null);
    row?.focus({ preventScroll: true });
    row?.scrollIntoView?.({ block: "center" });
  });

  // `PaneSelection.takeHandOff`.
  const takeHandOff = (): LandingTarget | undefined => {
    const handOff = focusTo.current;
    if (handOff === undefined) return undefined;
    focusTo.current = undefined;
    return () => targetElement(handOff.target);
  };

  return {
    ...stableHandOffs,
    close: leaveView,
    id: openId,
    open: (id) => select(id, columns ? "replace" : "push"),
    // Below the column's width, nothing selected is the way back to the list.
    select: (id) => (id === undefined && !columns ? leaveView() : select(id)),
    selectedRow,
    showing,
    step,
    takeHandOff,
  };
}

/** Whether the reader can see the row with this id among `visible`. */
function shown(visible: readonly PlacedRow[], id: string | undefined): boolean {
  return visible.some((placed) => rowId(placed.row) === id);
}
