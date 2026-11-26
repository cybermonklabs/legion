import { type RefObject, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import { scrollToNearest } from "../../components/focus-place";
import type { LandingTarget } from "../../components/SelectionBar";
import { focusOnDocument } from "../shell/roving";
import { answerDraft } from "./answer-drafts";
import type { InboxRoute } from "./inbox-route";
import { ROW_ATTRIBUTE } from "./inbox-row";
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

/** The list's empty message: what a hand-off gives the focus to when the reader's press leaves no
 *  row (R8). */
const EMPTY_SELECTOR = "[data-inbox-empty]";

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
  /** The row the column showed as the reader pressed, by `rowId`. */
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
  /** The address this list is on. The selection is written into its `?item=`. */
  filter: InboxRoute;
  /** Writes the address this list is on (`useInboxRoute`'s writer, which the Inbox holds): the
   *  selection goes into its `?item=`. */
  goToRoute: (next: InboxRoute) => void;
  /** The list's root, where the selected row's own element is looked up. */
  listRef: RefObject<HTMLElement | null>;
  /** Opens folded band `key` for this view alone, for an `?item=` naming a row in it: a link is
   *  no press of the reader's, so the folds they keep stay as they left them. */
  openFoldedBand: (key: string) => void;
  /** Whether the reading column is on. Without it there is no selection, the column shows
   *  nothing, and nothing is written to the address. */
  paneShown: boolean;
  /** The rows the reader can see, in the order they see them, as the list last rendered them.
   *  The list keeps this record for its own motion hold, and the selection is answered against
   *  exactly the rows in it. */
  presented: RefObject<readonly PlacedRow[]>;
  /** This list's own column - its rows, or its empty message - where a hand-off's target is
   *  looked up, so the drawer's list never reaches into the page under it. */
  region: RefObject<HTMLElement | null>;
  /** Ask ids whose own snooze write is out. */
  snoozing: ReadonlySet<string>;
}

export interface PaneSelection {
  /** The row the column is showing, by its `rowId`, or undefined while it shows none. */
  id: string | undefined;
  /** Selects a row, or nothing, and writes it to the address. */
  select: (id: string | undefined) => void;
  /** The selection as it stands now, for a callback whose closure is older than it: an answer's
   *  own card holds the `onAnswered` of the render its write began in, and by the time the
   *  server records that answer the row has left the list and the selection has moved on. */
  selectedNow: RefObject<string | undefined>;
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
  /** Where the selection goes when the row it is on leaves the bands: the row below where that
   *  one stood, else the row above it, read off the list as it stood while it still held it. */
  nextAfterSelected: () => string | undefined;
  /** At a press of the selection bar that takes rows away, before any optimistic write takes
   *  its rows `ids` off the list: where the reader goes is read off the list as they pressed. */
  beginHandOff: (ids: readonly string[]) => void;
  /** Once the press has taken the rows `ids` away (every id it took): beside the column the row
   *  after them is selected, and the hand-off waits for the bar's landing to take its target
   *  (`takeHandOff`), while the reader has not moved on. */
  landHandOff: (ids: readonly string[]) => void;
  /** Dispatch refused the press on `ids` whole: its rows are back, and nobody is sent anywhere. */
  dropHandOff: (ids: readonly string[]) => void;
  /** Takes the landed hand-off whose target has not taken the focus yet off the record, and
   *  answers that target - the row after the rows acted on, or the list's empty message - as a
   *  reading of the element this list draws for it, null while it draws none; undefined where no
   *  hand-off is waiting. The bar of selected rows lands it itself (`SelectionBar`'s
   *  `useLastMarkedPlace`): once the rows its press takes away have gone and the target is drawn,
   *  with the list's own stop holding the focus until then. So one landing runs per press
   *  (`Inbox.tsx`'s `returnFocus`). */
  takeHandOff: () => LandingTarget | undefined;
}

/**
 * The Inbox reading column's selection: which row the column shows, how the keys and the
 * address move it, and what keeps it on a question whose row has gone. And, in both modes, the
 * hand-off after a press of the selection bar that takes rows away (R8): the row after the rows
 * the press is on is selected beside the column, and its row takes the keyboard focus beside it
 * and below its width alike, through the bar's own landing.
 *
 * The address holds the selection (`?item=`), so a reload or a link keeps the reader's place;
 * below the column's width, and in the drawer, there is no selection and nothing is written.
 */
export function usePaneSelection({
  bulkSnoozing,
  candidates,
  filter,
  goToRoute,
  listRef,
  openFoldedBand,
  paneShown,
  presented,
  region,
  snoozing,
}: PaneSelectionOptions): PaneSelection {
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

  const select = (id: string | undefined): void => {
    revealNext.current = true;
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
    () =>
      selectedId === undefined
        ? null
        : (listRef.current?.querySelector<HTMLElement>(
            `[${ROW_ATTRIBUTE}="${CSS.escape(selectedId)}"]`
          ) ?? null),
    [listRef, selectedId]
  );

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
   * after the acted ones while the list still draws them, and, beside the column, the row below
   * where the selected one stood, else the row above, else the first. Undefined where nothing
   * says where the row stood.
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
   * Whether the reader is still where they pressed, so the landing may move them: beside the
   * column the selection is still on a row acted on, or on the row the press sends them to, or
   * where it was as they pressed - or on nothing, where the press leaves no row and sends them to
   * the empty message, since a bar's Snooze that takes the selected row with the last of the rest
   * has the settling effect leave the selection with nothing before its write's landing hands
   * the reader on (`Inbox.tsx`); below it the focus is on nothing, in a row acted on or the row it
   * sends them to, or still on what held it as they pressed. A reader who has walked off -
   * another row selected, the focus somewhere of their own - is left where they are.
   */
  const stillThere = (handOff: HandOff): boolean => {
    const { acted, target } = handOff;
    if (paneShown) {
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
    // off the list goes with the selection, so the column shows nothing once nothing is left. A
    // row the list still draws - a dismiss whose rows go at the next commit - keeps the
    // selection, which leaves with it as the settling effect hands it on.
    if (paneShown && target !== null) {
      // Its own landing shows the row once the rows acted on have gone (`selectedByHandOff`). The
      // settling effect has already handed the selection to it where a snooze took the selected
      // row away with the rest.
      if (target !== selectedNow.current) {
        selectedByHandOff.current = target;
        select(target);
      }
    } else if (paneShown && !drawsAny(acted) && acted.has(selectedNow.current ?? "")) {
      select(undefined);
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
    // The element the target names in this list, the row or the empty message, read when the
    // bar's landing asks for it.
    const { target } = handOff;
    const selector =
      target === null ? EMPTY_SELECTOR : `[${ROW_ATTRIBUTE}="${CSS.escape(target)}"]`;
    return () => region.current?.querySelector<HTMLElement>(selector) ?? null;
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

  // What the selection is, settled after each commit against the rows this list holds.
  //
  // The address names it, and is read as the reader's own move - the first visit, a link, a
  // reload - exactly while it says something this list has not already answered: a row it holds
  // is selected (its band opened first, for this view alone, if the address names a row in a
  // folded one: following the link is what they asked for, and the folds they keep are theirs),
  // and one it does not hold leaves the first visible row selected and the address rewritten.
  // Otherwise the selection is the reader's, and the address follows it.
  //
  // On the layout lane, so that the selection a commit hands on is drawn in the same task as that
  // commit. The commit that takes the selected row off the list draws no column, and takes away
  // whatever in the column held the focus; the next row's column is then drawn in the same task,
  // and the Inbox hands the focus to the new column's own focus stop in that commit (`Inbox.tsx`).
  useLayoutEffect(() => {
    // A list that holds nothing - still loading, failed, or genuinely empty - answers no
    // address: `?item=` is left unread until there are rows to read it against.
    if (!paneShown || listedRows.current.length === 0) {
      return;
    }
    const visible = presented.current;
    if (filter.item !== shownItem.current) {
      shownItem.current = filter.item;
      const wanted = listedRows.current.find((placed) => rowId(placed.row) === filter.item);
      if (wanted !== undefined) {
        const id = rowId(wanted.row);
        revealNext.current = true;
        setSelectedId(id);
        if (!visible.some((placed) => rowId(placed.row) === id)) openFoldedBand(wanted.group);
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
    // The row the selection was on has left the bands - the reader deferred it, a press of theirs
    // took it with others, the server took it away - so the selection goes where that row was
    // heading and the address follows it, down to naming no row at all when the list has none
    // left to show. The list stays where it stands: the row the selection moves to is not
    // scrolled to. A press of the bar's still in the air said where as the reader pressed: a
    // bulk snooze takes its rows at once, and the row below the selected one may be going with it.
    const handedTo =
      selectedId === undefined
        ? undefined
        : begun.current.find((handOff) => handOff.acted.has(selectedId))?.target;
    const next = handedTo ?? nextAfterSelected();
    const first = visible[0];
    const handedOn = visible.some((placed) => rowId(placed.row) === next)
      ? next
      : first === undefined
        ? undefined
        : rowId(first.row);
    select(handedOn);
    revealNext.current = false;
    // The row a press still in the air sends the reader to is that press's landing to show
    // (`selectedByHandOff`).
    if (handedTo !== undefined && handedOn === handedTo) selectedByHandOff.current = handedTo;
  });

  // The selected row is kept on screen as the reader walks the list: the column beside it shows
  // the row they cannot see otherwise. It is shown at its nearest edge by the least scroll, by the
  // window's and each box's visible edges and not the root's scroll padding (`scrollToNearest`),
  // as the bar's landing shows its target (`landNearest`), but moves no focus: the reader's focus
  // can be in the column while `j` and `k` step the selection. A selection the settling effect
  // hands on scrolls nothing (revealNext), and neither does a row a hand-off selected, which is
  // that hand-off's own landing to show (`selectedByHandOff`): the address the selection writes
  // follows on a lane of its own, and the settling effect's write of it again counts as a pick.
  useEffect(() => {
    const reveal = revealNext.current;
    revealNext.current = true;
    const handed = selectedByHandOff.current;
    selectedByHandOff.current = undefined;
    if (!paneShown || selectedId === undefined || selectedId === handed) {
      return;
    }
    if (reveal) {
      const row = selectedRow();
      if (row !== null) scrollToNearest(row);
    }
  }, [paneShown, selectedId, selectedRow]);

  return {
    beginHandOff,
    dropHandOff,
    id: selectedId,
    landHandOff,
    nextAfterSelected,
    select,
    selectedNow,
    selectedRow,
    showing,
    step,
    takeHandOff,
  };
}
