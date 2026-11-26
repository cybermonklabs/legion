import { type RefObject, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import { scrollToNearest } from "../../components/focus-place";
import type { LandingTarget } from "../../components/SelectionBar";
import { useAddressNavigate, useHistoryEntry } from "../../lib/page-address";
import { focusOnDocument } from "../shell/roving";
import { answerDraft } from "./answer-drafts";
import { buildInboxPath, type InboxRoute } from "./inbox-route";
import { ROW_ATTRIBUTE } from "./inbox-row";
import { type InboxListRow, rowId } from "./list-row";
import { PANE_SCROLLER_SELECTOR } from "./ReadingView";
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

/** The list's empty message: what a hand-off gives the focus to when the reader's press leaves no
 *  row (R8). */
const EMPTY_SELECTOR = "[data-inbox-empty]";

/** The reading view below 1280 px, marked with the row it shows (`ReadingView`). */
const READING_VIEW_SELECTOR = "[data-inbox-reading-row]";

/** Where a hand-off sends the reader: a row, by its `rowId`, or null for the list's empty
 *  message. */
type HandOffTarget = string | null;

/** One press of the selection bar's on rows of the list, and where it sends the reader. */
interface HandOff {
  /** The rows the press is on, by `rowId`. */
  readonly acted: ReadonlySet<string>;
  readonly target: HandOffTarget;
  /** What held the focus as the reader pressed, which still holding it when the press lands
   *  says they are where they were. */
  readonly focused: Element | null;
  /** The row the column or the reading view showed as the reader pressed, by `rowId`. */
  readonly selected: string | undefined;
}

/**
 * The row the reader goes to after acting on the rows `acted` (R8), in `drawn`, the rows as the
 * reader sees them: the first row after the last acted one, else the nearest row before it that
 * was not acted on, else none. A row the action is on is never the answer, so a bulk action's
 * rows are passed over together - for rows that stand together, the row before the first of
 * them - and a folded band's rows are not in `drawn` at all. None too where no acted row is
 * drawn, since there is then no place to go on from.
 */
function itemAfter(drawn: readonly PlacedRow[], acted: ReadonlySet<string>): string | undefined {
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
  /** How many rows a bulk snooze is deferring; zero while none is out. */
  bulkSnoozing: number;
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
   *  snooze, dismiss, Assign to me, hand-on or reply - rather than the server on its own. */
  leftByReader: (id: string) => boolean;
  /** The list's root, where the selected row's own element is looked up. */
  listRef: RefObject<HTMLElement | null>;
  /** Below the column's width, the row with this id, which the reading view showed, left the list
   *  without the reader's own write, and the list is back at that place. */
  onOpenRowLeft: (id: string) => void;
  /** Opens folded band `key` for this view alone, for an `?item=` naming a row in it: a link is
   *  no press of the reader's, so the folds they keep stay as they left them. */
  openFoldedBand: (key: string) => void;
  /** Whether this list selects at all: the Inbox page does at every width, the drawer never
   *  does. Without it there is no selection, no reading view shows the row, and nothing is
   *  written to the address. */
  pane: boolean;
  /** The rows the reader can see, in the order they see them, as the list last rendered them.
   *  The list keeps this record for its own motion hold, and the selection is answered against
   *  exactly the rows in it. */
  presented: RefObject<readonly PlacedRow[]>;
  /** This list's own part of the page - its rows, its empty message, and the reading view below
   *  the column's width - where a hand-off's target is looked up, so the drawer's list never
   *  reaches into the page under it. */
  region: RefObject<HTMLElement | null>;
  /** Whether the list holds every row it will draw from this read: the one read has landed and
   *  the motion hold has handed its thread rows on, or the list reads none (`useInboxThreadRows`'s
   *  `ready`). The asks (`Inbox`'s `adoptedInbox`) and the thread rows (the hold
   *  `useInboxThreadRows` keeps) each go through a motion hold over the list, so both wait for a
   *  pointer moving over the list to settle, and until the thread rows are handed on the selection
   *  is not settled at all: a `?item=` naming a thread row would read as a row this list does not
   *  hold, which closes the reading view below the column's width and has the column pick its top
   *  row beside the list. */
  rowsReady: boolean;
  /** Ask ids whose own snooze write is out. */
  snoozing: ReadonlySet<string>;
}

export interface PaneSelection {
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
  /** The selection as it stands now, for a callback whose closure is older than it: an answer's
   *  own card holds the `onAnswered` of the render its write began in, and by the time the
   *  server records that answer the row has left the list and the selection has moved on. */
  selectedNow: RefObject<string | undefined>;
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
  /** Where the selection goes when the row it is on leaves the bands: the row below where that
   *  one stood, else the row above it, read off the list as it stood while it still held it. */
  nextAfterSelected: () => string | undefined;
  /** At a press of the selection bar that takes rows away, before any optimistic write takes
   *  its rows `ids` off the list: where the reader goes is read off the list as they pressed. */
  beginHandOff: (ids: readonly string[]) => void;
  /** Once the press has taken the rows `ids` away (every id it took): beside the column the row
   *  after them is selected, and in a reading view below its width the view reads it, or gives
   *  way to the list where no row is left; the hand-off waits for the bar's landing to take its
   *  target (`takeHandOff`), while the reader has not moved on. */
  landHandOff: (ids: readonly string[]) => void;
  /** Dispatch refused the press on `ids` whole: its rows are back, and nobody is sent anywhere. */
  dropHandOff: (ids: readonly string[]) => void;
  /** Takes the landed hand-off whose target has not taken the focus yet off the record, and
   *  answers that target - the row after the rows acted on, the reading view's scroller where
   *  the view reads that row, or the list's empty message - as a reading of the element this
   *  list draws for it, null while it draws none; undefined where no hand-off is waiting. The bar
   *  of selected rows lands it itself (`SelectionBar`'s `useLastMarkedPlace`): once the rows its
   *  press takes away have gone and the target is drawn, with the list's own stop holding the
   *  focus until then. So one landing runs per press (`Inbox.tsx`'s `returnFocus`). */
  takeHandOff: () => LandingTarget | undefined;
}

/**
 * The Inbox reading column's selection: which row the column shows, how the keys and the
 * address move it, and what keeps it on a question whose row has gone. And, in every mode, the
 * hand-off after a press of the selection bar that takes rows away (R8): the row after the rows
 * the press is on is selected beside the column, and its row takes the keyboard focus beside it
 * and in the short-row list below its width alike, through the bar's own landing; a reading view
 * below that width reads the row and its scroller takes the focus, and the view gives way to the
 * list where no row is left.
 *
 * The address holds the selection (`?item=`), so a reload or a link keeps the reader's place.
 * From the column's width a row is always selected; below it a row is selected only while its
 * reading view is open, so there an address with no `?item=` is the list. In the drawer there
 * is no selection and nothing is written.
 */
export function usePaneSelection({
  bulkSnoozing,
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
  snoozing,
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
  // One record for every way a row leaves - the reader's own answer, which takes it once the
  // server records it, their own snooze, which takes it once the write lands, and the server
  // withdrawing, resolving or answering the question elsewhere - and read on every commit rather
  // than when it is wanted, because by then the row has gone. While the selected row's snooze
  // write is out the read is held, so where they are sent is read off the list as it stood when
  // they deferred; the list keeps that row where it stood until the write lands, and moves it
  // into `Later` in the commit it lets go of it. The commit that drops the row holds it for the
  // same reason: the row is gone from that commit's list, and where it was heading is exactly
  // what the settling effect is about to read this for.
  const stoodBetween = useRef<Neighbours>({ above: undefined, below: undefined });
  /** Where the selection goes when the row it is on leaves the bands: the next row the reader
   *  can see after it, else the nearest above it, so the column always shows something they
   *  can act on. */
  const nextAfterSelected = (): string | undefined =>
    stoodBetween.current.below ?? stoodBetween.current.above;
  /** Whether the selection's next change brings its row into view. A step, a pick, an address
   *  naming a row the list holds and an answer the reader recorded do. A selection the settling
   *  effect hands on - from a row that left the bands, whatever took it, or onto the first row
   *  while none was selected - does not: the reader did not move the selection there, and a press
   *  that took rows standing above the view scrolls nothing (R4). */
  const revealNext = useRef(true);

  /** The history entry `close` went back from, by its key, while that step is under way. A
   *  browser's `history.go` lands a frame or more later, so until it does the address still
   *  names the row the reader closed: read as a new address, it would select that row again. */
  const leaving = useRef<string | undefined>(undefined);
  const listAnswered = useRef(false);
  /** Below the column's width, the row that now stands where the row the reading view showed
   *  stood, when that row left the list on its own: the list comes back with the focus there. */
  const returnTo = useRef<string | undefined>(undefined);

  const select = (id: string | undefined, mode: SelectMode = "replace"): void => {
    revealNext.current = true;
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

  /** A listed row's own element, or null where the list does not draw it. */
  const rowElement = useCallback(
    (id: string): HTMLElement | null =>
      listRef.current?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${CSS.escape(id)}"]`) ?? null,
    [listRef]
  );
  /** The row the column is showing, or null while it is showing none. Held to the selection,
   *  because the effect keeping that row on screen is the selection's and nothing else's. */
  const selectedRow = useCallback(
    () => (selectedId === undefined ? null : rowElement(selectedId)),
    [rowElement, selectedId]
  );

  /** The row the reader has open, by the rule of the width the Inbox is at. Beside the list it is
   *  the selection. Below it, a row the column picked by itself is no row the reader opened:
   *  nothing is drawn for it, not even for the one commit before `settleReadingView` clears it. */
  const openId = columns || entry.inboxDefaultPick !== true ? selectedId : undefined;
  /** Whether the bar's hand-off acts on the row shown rather than on the row holding the focus:
   *  beside the column, and below it while a reading view is open. */
  const onShownRow = (): boolean => pane && (columns || selectedNow.current !== undefined);

  /** The bar's presses begun and not yet landed: a Snooze whose write is out. */
  const begun = useRef<readonly HandOff[]>([]);
  /** The hand-off that has landed and whose target the bar's landing has not taken yet. */
  const focusTo = useRef<HandOff | undefined>(undefined);
  /** The row a hand-off has just selected (`landHandOff`, and the settling effect's hand-on to a
   *  press still in the air), which the selection's keeping on screen (below) leaves to the bar's
   *  landing: it is selected as the reader acts, while the rows acted on may still stand above it,
   *  and a scroll to it there would carry the view past their place by their height once they have
   *  gone - beside the column the bar's Dismiss carried the list's box 162 px, the height of the
   *  two rows it took away, past the cut of the row marked last (#4304's builds Job
   *  `infra-2160-r22-l-qs6mx`). */
  const selectedByHandOff = useRef<string | undefined>(undefined);
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
   *  where the next Enter reaches the answer box - and nothing while it reads another; the empty
   *  message is drawn once the view has closed. */
  const targetElement = (target: HandOffTarget): HTMLElement | null => {
    const view = readingView();
    if (view !== null) {
      return target !== null && view.dataset.inboxReadingRow === target
        ? view.querySelector<HTMLElement>(PANE_SCROLLER_SELECTOR)
        : null;
    }
    const selector =
      target === null ? EMPTY_SELECTOR : `[${ROW_ATTRIBUTE}="${CSS.escape(target)}"]`;
    return region.current?.querySelector<HTMLElement>(selector) ?? null;
  };
  /** Takes the begun press on rows that include every one of `ids` off the record, and gives it
   *  back, or undefined where none was begun on them. */
  const takeBegun = (ids: readonly string[]): HandOff | undefined => {
    const handOff =
      ids.length === 0
        ? undefined
        : begun.current.find((each) => ids.every((id) => each.acted.has(id)));
    if (handOff !== undefined) begun.current = begun.current.filter((each) => each !== handOff);
    return handOff;
  };
  /**
   * Where a press goes once it lands when the row it began with has gone meanwhile: the row
   * after the acted ones while the list still draws them, and, beside the column or in the
   * reading view, the row below where the selected one stood, else the row above, else the
   * first. Undefined where nothing says where the row stood.
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
   * Whether the reader is still where they pressed, so the landing may move them: beside the
   * column, and in a reading view below its width, the selection is still on a row acted on, or
   * on the row the press sends them to, or where it was as they pressed - or on nothing, where
   * the press leaves no row and sends them to the empty message, since a bar's Snooze that takes
   * the selected row with the last of the rest has the settling effect leave the selection with
   * nothing before its write's landing hands the reader on (`Inbox.tsx`); in the short-row list
   * below it, and in the drawer, the focus is on nothing, in a row acted on or the row it sends
   * them to, or still on what held it as they pressed. A reader who has walked off - another row
   * selected, the focus somewhere of their own - is left where they are.
   */
  const stillThere = (handOff: HandOff): boolean => {
    const { acted, target } = handOff;
    if (columns || (pane && handOff.selected !== undefined)) {
      const now = selectedNow.current;
      if (now === undefined) return target === null;
      return acted.has(now) || now === target || now === handOff.selected;
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
    // The row the press began with, while the list still draws it; else where the press goes
    // read now, since the server may have taken that row away meanwhile.
    const target =
      began !== undefined &&
      (began.target === null ||
        presented.current.some((placed) => rowId(placed.row) === began.target))
        ? began.target
        : targetNow(acted);
    if (target === undefined) return;
    const handOff: HandOff = {
      acted,
      focused: began?.focused ?? null,
      selected: began?.selected,
      target,
    };
    if (!stillThere(handOff)) return;
    // The empty message is no row to select. Beside the column, a selected row the press took
    // off the list goes with the selection, so the column shows nothing once nothing is left, and
    // a reading view gives way to the list. A row the list still draws - a dismiss whose rows go
    // at the next commit - keeps the selection, which leaves with it as the settling effect hands
    // it on. In the reading view the row selected is the one the view reads next. In the
    // short-row list below the column's width nothing is selected: the row takes the focus where
    // it stands, and no reading view opens for it.
    const shownRow = onShownRow();
    if (shownRow && target !== null) {
      // Its own landing shows the row once the rows acted on have gone (`selectedByHandOff`). The
      // settling effect has already handed the selection to it where a snooze took the selected
      // row away with the rest.
      if (target !== selectedNow.current) {
        selectedByHandOff.current = target;
        select(target);
      }
    } else if (shownRow && !drawsAny(acted) && acted.has(selectedNow.current ?? "")) {
      if (columns) select(undefined);
      else leaveView();
    }
    focusTo.current = handOff;
    // The target can wait on a commit still to come - the empty message is drawn once the acted
    // rows go - and the reader's next press says they have moved on without it.
    const movedOn = () => {
      if (focusTo.current === handOff) focusTo.current = undefined;
      document.removeEventListener("keydown", movedOn, true);
      document.removeEventListener("pointerdown", movedOn, true);
    };
    document.addEventListener("keydown", movedOn, true);
    document.addEventListener("pointerdown", movedOn, true);
  };

  const takeHandOff = (): LandingTarget | undefined => {
    const handOff = focusTo.current;
    if (handOff === undefined) return undefined;
    focusTo.current = undefined;
    // The element the target names in this list - the row, the reading view's scroller, or the
    // empty message - read when the bar's landing asks for it.
    return () => targetElement(handOff.target);
  };

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
   * list, the first row this list selected for them included. The reading view below the
   * column's width follows the same rule.
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
    // Read here, where the rows stand as the reader sees them, and held while a snooze of the
    // selected row is out, so the answer is the one taken as the reader deferred: the list keeps
    // that row where it stood until the write lands, and moves it into `Later` in the commit it
    // lets go of it. Held too on a commit whose list no longer holds the selected row at all: that
    // is the commit the settling effect reads this on, and the answer it wants is the one taken
    // while the row was still there.
    if (selectedId === undefined) {
      stoodBetween.current = { above: undefined, below: undefined };
    } else if (!snoozing.has(selectedId) && bulkSnoozing === 0) {
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

  /** Where the selection goes once the row it is on has left the bands: where a press of the
   *  bar's still in the air said as the reader pressed - a bulk snooze takes its rows at once, and
   *  the row below the selected one may be going with it - else where that row was heading. */
  const handedTo = (): string | undefined =>
    selectedId === undefined
      ? undefined
      : (begun.current.find((handOff) => handOff.acted.has(selectedId))?.target ?? undefined);

  /** The step both widths share: the address is read as the reader's own move - the first
   *  visit, a link, a reload, the browser's Back - exactly while it says something this list has
   *  not already answered. A row it holds is selected, its band opened first, for this view
   *  alone, if the address names a row in a folded one: following the link is what they asked
   *  for, and the folds they keep are theirs. */
  const readAddress = (visible: readonly PlacedRow[]): AddressReading => {
    if (filter.item === shownItem.current) return "answered";
    shownItem.current = filter.item;
    const wanted = listedRows.current.find((placed) => rowId(placed.row) === filter.item);
    if (wanted === undefined) return "unlisted";
    const id = rowId(wanted.row);
    revealNext.current = true;
    setSelectedId(id);
    if (!shown(visible, id)) openFoldedBand(wanted.group);
    return "listed";
  };

  /** From the column's width a row is always selected. An address the list cannot answer leaves
   *  the first visible row selected, as the column's own pick, and the address rewritten; a row
   *  that leaves hands the selection to where it was heading, and the list stays where it
   *  stands: the row the selection moves to is not scrolled to. Otherwise the selection is the
   *  reader's, and the address follows it. */
  const settleBesideList = (): void => {
    // A list that holds nothing - still loading, failed, or genuinely empty - answers no
    // address: `?item=` is left unread until there are rows to read it against.
    if (listedRows.current.length === 0) return;
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
      revealNext.current = false;
      return;
    }
    if (shown(visible, selectedId)) {
      if (selectedId !== filter.item) select(selectedId);
      return;
    }
    // A question the column is keeping for the reader's unsent answer keeps the selection with
    // it: the row has gone, the question has not, and only their next pick ends that.
    if (paneSubject.current?.kept === true) return;
    // The row the selection was on has left the bands - the reader deferred it, a press of theirs
    // took it with others, the server took it away - so the selection goes where that row was
    // heading, a pick of the column's own staying its own, down to the first row the reader can
    // see, and to naming no row at all when the list has none left to show.
    const pressed = handedTo();
    const next = pressed ?? nextAfterSelected();
    if (shown(visible, next)) {
      select(next, entry.inboxDefaultPick === true ? "column-default" : "replace");
      revealNext.current = false;
      // The row a press still in the air sends the reader to is that press's landing to show
      // (`selectedByHandOff`).
      if (pressed !== undefined && next === pressed) selectedByHandOff.current = pressed;
      return;
    }
    select(top === undefined ? undefined : rowId(top.row), "column-default");
    revealNext.current = false;
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
    const next = handedTo() ?? nextAfterSelected();
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

  // The selected row is kept on screen as the reader walks the list: the column beside it shows
  // the row they cannot see otherwise. It is shown at its nearest edge by the least scroll, by the
  // window's and each box's visible edges and not the root's scroll padding (`scrollToNearest`),
  // as the bar's landing shows its target (`landNearest`), but moves no focus: the reader's focus
  // can be in the column while `j` and `k` step the selection. A selection the settling effect
  // hands on scrolls nothing (revealNext), and neither does a row a hand-off selected, which is
  // that hand-off's own landing to show (`selectedByHandOff`): the address the selection writes
  // follows on a lane of its own, and the settling effect's write of it again counts as a pick.
  // Below the column's width the reading view stands in the list's place, so there is no list to
  // keep it in.
  useEffect(() => {
    const reveal = revealNext.current;
    revealNext.current = true;
    const handed = selectedByHandOff.current;
    selectedByHandOff.current = undefined;
    if (!columns || selectedId === undefined || selectedId === handed) {
      return;
    }
    if (reveal) {
      const row = selectedRow();
      if (row !== null) scrollToNearest(row);
    }
  }, [columns, selectedId, selectedRow]);

  // Below the column's width, the commit that closes a row's reading view brings the list back
  // at that row: it takes the focus, so the keys walk on from it, and is scrolled to the middle
  // of the screen. A layout effect, so the row is placed in the commit that shows the list
  // again rather than one frame after the list painted at its top. Only a row a reading view was
  // drawn for is remembered: the column's own row, dropped as the window narrows, never had
  // one, so the focus stays where the reader has it - in the search box, say. A row a press of
  // the bar took out of the view is that press's to hand on (R8, `landHandOff`): the list comes
  // back with the focus where the bar's landing sends it, not on the row that is leaving.
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
    const row = rowElement(left) ?? (back === undefined ? null : rowElement(back));
    row?.focus({ preventScroll: true });
    row?.scrollIntoView?.({ block: "center" });
  });

  return {
    beginHandOff,
    close: leaveView,
    dropHandOff,
    id: openId,
    landHandOff,
    nextAfterSelected,
    open: (id) => select(id, columns ? "replace" : "push"),
    // Below the column's width, nothing selected is the way back to the list.
    select: (id) => (id === undefined && !columns ? leaveView() : select(id)),
    selectedNow,
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
