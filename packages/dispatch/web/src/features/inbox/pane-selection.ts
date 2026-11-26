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
import { focusOnDocument } from "../shell/roving";
import { answerDraft } from "./answer-drafts";
import { type InboxRoute, useInboxRoute } from "./inbox-route";
import { ROW_ATTRIBUTE, rowIn } from "./inbox-row";
import { type InboxListRow, rowId } from "./list-row";
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

/** The list's empty message; the empty list itself where it draws no message; and the list where
 *  it draws no row because every row it holds is in a folded band (`Inbox.tsx`): what a hand-off
 *  gives the focus to when the reader's action leaves no row in sight (R8). */
const EMPTY_SELECTOR = "[data-inbox-empty]";

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
  /** The row the column showed as the reader acted, by `rowId`. */
  readonly selected: string | undefined;
}

/** The three moments of a hand-off: the action, its landing and its refusal. */
export interface InboxHandOff {
  /** At the action, before any optimistic write takes its rows off the list: where the reader
   *  goes is read off the list as they acted on it. */
  beginHandOff: (ids: readonly string[]) => void;
  /** Once the server has recorded the action on `ids` (every id it took): the row after them is
   *  selected beside the column and given the focus, while the reader has not moved on. */
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
  /** The address this list is on. The selection is written into its `?item=`. */
  filter: InboxRoute;
  /** The list's root, where the selected row's own element is looked up. */
  listRef: RefObject<HTMLElement | null>;
  /** Opens the folded band, for an `?item=` naming a row the reader deferred. */
  openFoldedBand: () => void;
  /** Whether the reading column is on. Without it there is no selection, the column shows
   *  nothing, and nothing is written to the address. */
  paneShown: boolean;
  /** The rows the reader can see, in the order they see them, as the list last rendered them.
   *  The list keeps this record for its own motion hold, and the selection is answered against
   *  exactly the rows in it. */
  presented: RefObject<readonly PlacedRow[]>;
  /** This list's own part of the page - its rows, or its empty message - where a hand-off's
   *  target is looked up, so the drawer's list never reaches into the page under it. */
  region: RefObject<HTMLElement | null>;
  /** Whether a part of the list is still to come: the reader's threads, read apart from the
   *  asks, which the list draws first (`useInboxThreadRows`). An address naming a row the list
   *  does not hold yet waits for that part rather than being answered with the first row. */
  rowsPending: boolean;
}

export interface PaneSelection extends InboxHandOff {
  /** The row the column is showing, by its `rowId`, or undefined while it shows none. */
  id: string | undefined;
  /** Selects a row, or nothing, and writes it to the address. */
  select: (id: string | undefined) => void;
  /** The selected row's own element, or null while the list holds none. */
  selectedRow: () => HTMLElement | null;
  /** Selects the row `delta` from the selected one, in the order the reader sees: what `j`, `k`
   *  and the arrows do while the reading column is on, in place of roving the focus. */
  step: (delta: 1 | -1) => void;
  /**
   * Every row the list holds this commit, a folded band's included, and, from the rows the
   * reader can see, the row the column shows. Called once per commit, after `presented`
   * is written: where the selection would go next is read there, off the rows as they stand,
   * rather than when it is wanted.
   */
  showing: (listed: readonly PlacedRow[]) => InboxListRow | undefined;
  /** Gives the element a landed hand-off is waiting to give the focus to - the row after the
   *  rows acted on, or the list's empty message - the focus now, by the hand-off's own rule
   *  (`focusTarget`), and answers whether one was waiting: what the bar of selected rows hands
   *  the focus on by first as it goes (`SelectionBar`'s `returnFocus`). */
  focusHandOff: () => boolean;
}

/**
 * The Inbox reading column's selection: which row the column shows, how the keys and the
 * address move it, and what keeps it on a question whose row has gone. And, in both modes, the
 * hand-off after the reader's own action (R8): the row after the rows they acted on is selected
 * beside the column, and its row takes the keyboard focus beside it and below its width alike.
 *
 * The address holds the selection (`?item=`), so a reload or a link keeps the reader's place;
 * below the column's width, and in the drawer, there is no selection and nothing is written.
 */
export function usePaneSelection({
  candidates,
  filter,
  listRef,
  openFoldedBand,
  paneShown,
  presented,
  region,
  rowsPending,
}: PaneSelectionOptions): PaneSelection {
  const [, goToRoute] = useInboxRoute();
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
  /** A commit for the focus to move in: a landing in card mode changes nothing else that
   *  renders. */
  const [, setLandings] = useState(0);

  const select = (id: string | undefined): void => {
    setSelectedId(id);
    selectedNow.current = id;
    shownItem.current = id;
    if (id !== filter.item) {
      // Replace, never push: a reader walking the list with `j` would otherwise have to press
      // Back once per row to leave the page, the rule the assignee filter already holds to;
      // `useInboxRoute` is where that rule lives. Selecting nothing clears `?item=` rather than
      // leaving it on the row that has just left: a reload reads the address, and an address
      // naming a row the reader deferred would open the folded band and put that question back
      // in front of them.
      goToRoute({ ...filter, item: id });
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

  /** Whether the list still draws any row of `acted`. */
  const drawsAny = (acted: ReadonlySet<string>): boolean =>
    presented.current.some((placed) => acted.has(rowId(placed.row)));
  /** The element `target` names in this list: the row, or the empty message. */
  const targetElement = (target: HandOffTarget): HTMLElement | null =>
    target === null
      ? (region.current?.querySelector<HTMLElement>(EMPTY_SELECTOR) ?? null)
      : rowIn(region.current, target);
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
   * acted ones while the list still draws them - below the column's width it holds the row
   * under the reader's hand - and, beside the column, which keeps the answered question past
   * its row, the row below where it stood, else the row above, else the first. Undefined where
   * nothing says where the row stood.
   */
  const targetNow = (acted: ReadonlySet<string>): HandOffTarget | undefined => {
    const visible = presented.current;
    if (drawsAny(acted)) return itemAfter(visible, acted) ?? null;
    if (!paneShown || selectedNow.current === undefined || !acted.has(selectedNow.current)) {
      return visible.length === 0 ? null : undefined;
    }
    const next = nextAfterSelected();
    if (next !== undefined && visible.some((placed) => rowId(placed.row) === next)) return next;
    const first = visible[0];
    return first === undefined ? null : rowId(first.row);
  };

  /**
   * Whether the reader is still where they acted, so the landing may move them: beside the
   * column the selection is still on a row acted on, or on the row the action sends them to, or
   * where it was as they acted; below it the focus is on nothing, in a row acted on or the row
   * it sends them to, or still on what held it as they acted. A reader who has walked off -
   * another row selected, the focus somewhere of their own - is left where they are.
   */
  const stillThere = (handOff: HandOff): boolean => {
    const { acted, target } = handOff;
    if (paneShown) {
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
    const handOff: HandOff = {
      acted,
      focused: began?.focused ?? null,
      selected: began?.selected,
      target,
    };
    if (!stillThere(handOff)) return;
    // The empty message is no row to select. Beside the column, a selected row the action took
    // off the list - an answered question, which the column kept past its row until the server
    // decided it - goes with the selection, so the column shows nothing once nothing is left:
    // the settling effect leaves a question the column keeps where it is. A row the list still
    // draws - a snooze of the last rows with the Later band open, or a dismiss whose rows go at
    // the next commit - keeps the selection, which leaves with it as the settling effect hands
    // it on.
    if (paneShown && target !== null) {
      select(target);
    } else if (paneShown && !drawsAny(acted) && acted.has(selectedNow.current ?? "")) {
      select(undefined);
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
    if (!paneShown || selectedId === undefined) {
      paneSubject.current = undefined;
      return undefined;
    }
    if (listed !== undefined) {
      paneSubject.current = { kept: false, row: listed };
      return listed;
    }
    if (subject === undefined || rowId(subject.row) !== selectedId) {
      paneSubject.current = undefined;
      return undefined;
    }
    const kept = subject.kept || answerDraft(selectedId) !== "";
    const row = candidates.find((candidate) => rowId(candidate) === selectedId) ?? subject.row;
    paneSubject.current = kept ? { kept: true, row } : undefined;
    return kept ? row : undefined;
  };

  const showing = (listed: readonly PlacedRow[]): InboxListRow | undefined => {
    listedRows.current = listed;
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

  // What the selection is, settled after each commit against the rows this list holds.
  //
  // The address names it, and is read as the reader's own move - the first visit, a link, a
  // reload - exactly while it says something this list has not already answered: a row it holds
  // is selected (its band opened first, if the reader linked into a folded one: following the
  // link is what they asked for), and one it does not hold leaves the first visible row
  // selected and the address rewritten, once every part of the list has come. Otherwise the
  // selection is the reader's, and the address follows it.
  //
  // On the layout lane, so that the selection a commit hands on is drawn in the same task as that
  // commit. The commit that takes the selected row off the list draws no column, and takes away
  // whatever in the column held the focus; the next row's column is then drawn in the same task,
  // and the Inbox hands the focus to the new column's own focus stop in that commit (`Inbox.tsx`).
  useLayoutEffect(() => {
    if (!paneShown) return;
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
    if (filter.item !== shownItem.current) {
      const wanted = listedRows.current.find((placed) => rowId(placed.row) === filter.item);
      // A row the list does not hold yet may be in the part still to come - a link to a thread,
      // whose read lands after the asks' - so the address waits for it, unanswered.
      if (wanted === undefined && filter.item !== undefined && rowsPending) return;
      shownItem.current = filter.item;
      if (wanted !== undefined) {
        const id = rowId(wanted.row);
        setSelectedId(id);
        if (!visible.some((placed) => rowId(placed.row) === id)) openFoldedBand();
        return;
      }
    }
    if (visible.some((placed) => rowId(placed.row) === selectedId)) {
      if (selectedId !== filter.item) select(selectedId);
      return;
    }
    // A question the column is keeping for the reader's unsent answer keeps the selection with
    // it: the row has gone, the question has not, and only their next pick ends that.
    if (paneSubject.current?.kept === true) {
      return;
    }
    // The row the selection was on has left the bands - the server took it away, or the reader
    // acted on it - so the selection goes where that row was heading and the address follows
    // it, down to naming no row at all when the list has none left to show. An action of the
    // reader's still in the air said where as they acted: a bulk snooze folds its rows at once,
    // and the row below the selected one may be going with it.
    const handedTo =
      selectedId === undefined
        ? undefined
        : begun.current.find((handOff) => handOff.acted.has(selectedId))?.target;
    const next = handedTo ?? nextAfterSelected();
    const first = visible[0];
    select(
      visible.some((placed) => rowId(placed.row) === next)
        ? next
        : first === undefined
          ? undefined
          : rowId(first.row)
    );
  });

  // `PaneSelection.focusHandOff`, which the effect below runs after every commit too.
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
  // without it.
  //
  // An action still in the air can lose its rows first - a bulk snooze folds them at once - and,
  // with the focus in one of them, the focus with them: the target takes it then rather than
  // leave it on the body, and the landing finds the reader there.
  //
  // A target given the focus while rows acted on still stood before it is shown whole in the
  // first commit that finds none of them there (`intoView`): after the list's anchor has held it
  // at their place, which leaves a target in sight there where it is.
  useEffect(() => {
    const shown = intoView.current;
    if (
      shown !== undefined &&
      (document.activeElement !== shown.target || !actedBefore(shown.acted, shown.target))
    ) {
      intoView.current = undefined;
      if (document.activeElement === shown.target) landNearest(shown.target);
    }
    const handOff = focusTo.current;
    if (handOff !== undefined) {
      if (!focusHandOff() && !drawsAny(handOff.acted)) focusTo.current = undefined;
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
  // be in the column while `j` and `k` step the selection.
  useEffect(() => {
    if (!paneShown || selectedId === undefined) {
      return;
    }
    const row = selectedRow();
    if (row !== null) scrollToNearest(row);
  }, [paneShown, selectedId, selectedRow]);

  return {
    ...stableHandOffs,
    focusHandOff,
    id: selectedId,
    select,
    selectedRow,
    showing,
    step,
  };
}
