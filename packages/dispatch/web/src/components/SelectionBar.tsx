import {
  type KeyboardEvent,
  type ReactNode,
  type Ref,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
} from "react";

import { focusOnDocument } from "../features/shell/roving";
import { useMediaQuery } from "../features/shell/useDialog";
import { afterFootPass, footPassNow, useFootInset } from "../hooks/useFootInset";
import { card, dangerText, focusVisibleRing, textPrimaryOnCanvas } from "../theme/classes";
import { CONTROL, type ControlSize } from "./control-names";
import {
  ARM_ON_PRESS,
  armHandOff,
  armOnPress,
  causedByReader,
  focusedOutOfSight,
  focusNoScroll,
  focusOwnStop,
  isRowMark,
  LONE_MODIFIERS,
  landNearest,
  outOfSight,
  pressIsArmedOwn,
  pressMadeIn,
  pressOnRowMark,
  repeatTookFocusFrom,
  scrollToNearest,
  visibleMarkPoint,
} from "./focus-place";
import { ROW_SELECTOR } from "./row-marker";

export interface SelectionBarProps {
  /** The bar's own element, for a page whose keymap puts the reader on it: Escape from a control
   *  inside the bar focuses the bar itself when the press has left no row to return to. */
  readonly barRef?: RefObject<HTMLElement | null>;
  /** What one press does to every selected row. */
  readonly children: ReactNode;
  /** How many rows are selected, said as `N selected` unless `state` says otherwise. */
  readonly count: number;
  /** The toolbar's accessible name. */
  readonly label: string;
  /** A refusal, or anything else the reader must read before pressing again: its own line inside
   *  the bar, so it never squeezes the actions off the end of theirs. */
  readonly notice?: ReactNode;
  /** What takes the keyboard focus when the bar goes while it holds it, or when the control in it
   *  that holds the focus goes from a bar that stays, asked once the commit that took it away is
   *  in the page. It is never the body. The first that applies takes the focus: the checkbox of
   *  the row the reader marked last while the list holds it and no press is taking it away, out
   *  of sight or not; else a row the reader can see, after a press that took the row marked last
   *  away or with no row marked; else the list's own stop, with no scroll (`focusListStop`). The
   *  page's hand-off focuses a row's checkbox itself and shows it whole at its nearest edge
   *  (`useLastMarkedPlace`), and the bar lands an element the page names here the same way
   *  (`landNearest`). Null moves nothing here: the page has put the focus where it goes itself, or
   *  the hand-off asked the table to draw the row and focus its checkbox once drawn
   *  (`MarkedList`'s `drawRow`). */
  readonly returnFocus?: () => HTMLElement | null;
  /** How much room the bar's own box takes (`ControlSize`): its padding. The actions in it are
   *  the page's, and each carries the page's size itself. */
  readonly size?: ControlSize;
  /** The bar's own words in place of the count, for what a count cannot say: the Inbox's
   *  `Every row selected`, which covers rows a fold or a page is holding back, and its
   *  `Snoozing 3…`, which is the press that is in the air rather than what is still marked. */
  readonly state?: ReactNode;
}

/** A list the selection bar stands over, as the bar's hand-off reads it (`useLastMarkedPlace`). */
export interface MarkedList {
  /** The ids of the rows the list holds now, in its order. A table that draws only a window of
   *  its rows names the rows it has not drawn too. */
  readonly order: () => readonly string[];
  /** The row the reader marked last, by id, else null: no row is marked, as after a select-all
   *  that marked none of the reader's own. */
  readonly lastMarked: () => string | null;
  /** Whether the bar is drawn. Where the row marked last stands is read only then. */
  readonly shown: boolean;
  /** The control that takes the focus in row `id`, its mark, while the page draws that row; else
   *  null. */
  readonly control: (id: string) => HTMLElement | null;
  /** Draws row `id`, which the list holds (`order`) and its window has not drawn (`control` is
   *  null), and lands on its mark once drawn, calling `onLand` with that mark in the task that
   *  lands it so the hand-off can run the foot's pass and fit the box against the scroll that
   *  drew it. Answers what takes that landing back. A list that draws every row it holds leaves
   *  this out. */
  readonly drawRow?: (id: string, onLand?: (mark: HTMLElement) => void) => () => void;
  /** Makes the list's own layout current against the room the page gives it now: the hand-off
   *  calls it after `footPassNow()` right before each landing it makes, including the table's
   *  own through `drawRow`. It calls it again after the pass that follows the landing's scroll,
   *  so the box the landing is judged against is the box the list keeps after it. A list whose
   *  layout follows the page by itself leaves this out. */
  readonly fit?: () => void;
  /** Whether a press of the reader's is taking row `id` away while the page still draws it: from
   *  the press until the row is no longer in `order`, or until the press is refused or undone. The
   *  hand-off owes its landing while any listed row is named here, and the page never keeps a row
   *  named here for the hand in it. */
  readonly leaving?: (id: string) => boolean;
  /** The list's sticky head, which its rows scroll under, else null. A mark the hand-off lands is
   *  shown whole below it (`landNearest`). */
  readonly head?: () => Element | null;
  /** The list's own element. It takes the focus at its own stop where no row in sight can. */
  readonly list: () => HTMLElement | null;
}

/**
 * The hand-off of the selection bar's focus to a row of the list it stands over. It answers
 * `MarkedPlace`, whose `land` a page's `returnFocus` calls first. The hand-off has three states,
 * and the host's own commits move it between them (`bar-return-design.md` of dispatch://INFRA-3135):
 *
 * **Idle.** No press of the reader's is out, and the hand-off holds nothing.
 *
 * **Owed.** The bar has gone after the reader's press and the list names a listed row as leaving
 * (`MarkedList`'s `leaving`): the rows that press is taking away are still drawn, so no place is
 * the place yet. Where the going left the focus on the page's body, it is parked with no scroll
 * (`park`): on the mark of the row the reader marked last while the list draws it and the reader
 * can see part of it, else on the list's own stop. A focus the reader holds elsewhere is theirs.
 * The bar asks the hand-off only for a focus its going dropped onto the body (`land`); a going
 * that leaves the focus in a row the list names as leaving - the reader's own untick of the last
 * mark while the press's writes are out, by a click that keeps the focus on the mark - is owed by
 * the hand-off itself, in that commit, and moves nothing then: the row's going would drop the
 * reader's focus onto the body, and the landing takes it then. That row can also go in the commit
 * that takes the bar away, before the hand-off reads the focus in it: the reader unticks the row
 * marked last while another mark keeps the bar up, and the press's writes then take both rows and
 * the bar away together. So while the bar is drawn and nothing is owed, the hand-off remembers
 * the control that holds the focus in a row the list names as leaving, from the focus events
 * (`focusin`) and from the host's commits, never from a frame or a timer. The reader's own act
 * (`READER_ACTS`), a focus on a control outside those rows, a landing owed and the hook's unmount
 * forget it; the focus the act then puts in a leaving row is remembered again. A going that finds
 * the focus on the body and that control gone from the page owes the landing as well, and asks
 * for it at once. Each commit asks again.
 *
 * **Landed.** No listed row is leaving, so the hand-off lands once and then watches nothing. The
 * place is the mark of the row the reader marked last while the list holds it, out of sight or
 * not; else, after a press that took that row away, the mark of the row now at its place, else
 * the nearest before it, else the nearest after it, each judged where the reader can see it below
 * whatever the page draws over the list (`markAtPlace`, `firstMarkShown`, which read the sticky
 * head the hand-off is given); else the list's own stop, with no scroll, which is given
 * `tabindex="-1"` where it has none. The mark takes the focus with no scroll of the engine's own,
 * which centres a control it scrolls to, and is shown whole at its nearest edge below that head,
 * the least scroll (`landNearest`). Every landing is made once the foot's own pass has given the
 * list the room the bar's foot had back (`hooks/useFootInset.ts`'s `afterFootPass`), since that
 * pass moves the box the reader sees the rows through. Before fitting the list (`MarkedList`'s
 * `fit`), the hand-off runs `footPassNow` to account for floats whose boxes changed before their
 * observers ran. Where the list holds the row marked last and its window has not drawn it,
 * a list with `drawRow` draws and lands on the row itself after the fit. The hand-off owes that
 * landing and holds what takes it back. Without `drawRow`, `land` answers false and leaves the
 * landing to the list's own stop. Right after a landing's scroll, the hand-off runs `footPassNow`
 * again, fits the box and checks the landing against that box in the same task. A scroll off the
 * box's end can release kept end room before the first painted frame.
 *
 * **Cancelled.** The reader's own press, key or wheel (`READER_ACTS`) ends what is owed when
 * it leaves focus on a control that stays or puts focus on the body. A press outside every row
 * of the list that leaves focus on the body counts as the reader's choice, even if the parked
 * mark's row leaves before the read. A press the armed press counts as its own - the
 * second press on a control its write holds - is no such act (`pressIsArmedOwn`), and neither is
 * a modifier key alone, which only changes the key pressed with it (`LONE_MODIFIERS`): the Tab of
 * Shift+Tab is the act, and it starts from the parked mark. Where the act left the focus is read
 * once the act has been answered (`afterAnswer`): a task after a press or a wheel, and a task
 * after a key's own keyup, by which the page's keymap has moved the focus and Space has ticked
 * the mark. Nothing lands between the act and that read: the act takes back the table's landing
 * on a row it is drawing too. Where the read finds the focus in a row the list names as leaving -
 * the parked mark, or another leaving row the reader's key moved it into - or on the body because
 * the control that held it went with its row, the landing stays owed, as in Owed, and is made
 * once those rows have gone; the table is asked to draw its row again then. A row the list names
 * need not go: Take out of epic with no Epic filter keeps its rows listed. So where no row is
 * named leaving any more and the row the read found the focus in is still a row of the list, with
 * the focus still in it, that row did not go: the act's end stands, nothing lands, and that row's
 * later going owes nothing. Otherwise nothing of this follows the reader after that. A settle
 * before the read - a new landing owed, or the hook's unmount - ends it unmade. A window `blur`
 * ends the key's wait without a read and removes its listeners, even if the keyup never comes.
 *
 * **Handed on.** The page's own hand-off, or any move of the focus to a control that stays that
 * the hand-off did not make, ends what is owed, and the hand-off never takes the focus back from
 * such a control: on the project List, the question dialog of a Move to epic whose dry run is out
 * takes the focus while the bar's going, both marks unticked by the reader, owes a landing too.
 * Where the focus stands is noted once the landing is owed, and again
 * after each move the hand-off makes itself (`park`, and the list's own stop it puts the focus on
 * in `drawLast`), and read when the landing is made, once the foot's pass has run: a focus that
 * stands elsewhere then, and not on the page's body, on the list's own stop or in a row the list
 * names as leaving, ends it unmade. So a focus that a row's going drops onto the body is still
 * landed. A reader's own act is read by **Cancelled**, which ends what is owed as the act comes,
 * before any landing, and what its read owes again is noted afresh.
 *
 * So a reader deep in a long list keeps their place, and the focus goes neither to the list's first
 * row nor to the page's body. One case is not answered yet: a bar action that a dialog confirms,
 * as the parent-status confirmation dialog for Move to epic does, ends on the body, because the
 * dialog's confirm is then the armed press and the bar's own gate is not.
 */
export function useLastMarkedPlace(list: MarkedList): MarkedPlace {
  // The ids of the list's rows, in order, as the last commit that drew the bar with the row
  // marked last in the list read them.
  const before = useRef<readonly string[]>([]);
  // The list as this render was given it, for a commit after the bar's going to read: the
  // hand-off's own closure holds the render's answers, which that going has moved on from.
  const latest = useRef(list);
  latest.current = list;
  // What is owed, from the bar's going until the landing is made or the reader acts. Null in Idle
  // and once it is settled. Its identity is the token a landing waiting on the foot's pass checks.
  const owed = useRef<Owed | null>(null);
  // Where the focus stood once the landing was owed, or once the hand-off itself last moved it
  // (`park`, the list's own stop in `drawLast`): the landing reads a focus the page has moved on
  // from there to a control that stays as the page's own hand-off (Handed on).
  const owedFocus = useRef<Element | null>(null);
  // The read of where the reader's act left the focus, which `owe` puts off until the act has been
  // answered (`afterAnswer`), as this render wrote it below: it can owe the landing again, through
  // `owePlace`, which is written after `owe` and owes through it.
  const answerAct = useRef<
    (last: string | null, from: Element | null, pressedOutsideRows: boolean) => void
  >(() => undefined);
  // Whether the bar was drawn at the host's last commit, so the commit that takes it away is known.
  const drawn = useRef(false);
  // What ends, unmade, the read of where the reader's act left the focus (`afterAnswer`) while it
  // is still to come, else null.
  const pending = useRef<(() => void) | null>(null);
  // The control that held the focus in a row the list names as leaving while the bar was drawn
  // and nothing was owed, else null: the bar's going owes the landing where that row has gone
  // with the focus before the going could read it there (Owed).
  const leavingFocus = useRef<Element | null>(null);
  // What ends the watch that keeps `leavingFocus` from the focus events and the reader's acts,
  // while it runs, else null.
  const leavingWatch = useRef<AbortController | null>(null);
  /** Forgets the control remembered in a leaving row and ends the watch that keeps it. */
  const forgetLeaving = useCallback(() => {
    leavingWatch.current?.abort();
    leavingWatch.current = null;
    leavingFocus.current = null;
  }, []);
  /** Ends what is owed, its watch on the reader's acts, a read their act left to come, and what
   *  the hand-off remembers of a focus in a leaving row (`forgetLeaving`). It moves no focus. */
  const settle = useCallback(() => {
    const was = owed.current;
    owed.current = null;
    was?.unwatch();
    pending.current?.();
    pending.current = null;
    forgetLeaving();
  }, [forgetLeaving]);
  useEffect(() => settle, [settle]);
  // After every commit, so the commit that takes the press's rows away keeps the order before it.
  useLayoutEffect(() => {
    if (!list.shown) return;
    const last = list.lastMarked();
    if (last === null) return;
    const order = list.order();
    if (order.includes(last)) before.current = order;
  });
  /** Remembers, while the bar is drawn and nothing is owed, the control that holds the focus in a
   *  row the list names as leaving, for the bar's going to read (Owed): as this commit of the host
   *  leaves it, and at each move of the focus until the watch ends (`forgetLeaving`). A focus on a
   *  control outside those rows forgets it, and so does the reader's own act (`READER_ACTS`); the
   *  focus the act then puts in a leaving row is remembered again by its `focusin` or by the commit
   *  after it. A focus on the body keeps it: a control taken out of the page with its row drops
   *  the focus there and raises no `focusin`. */
  const rememberLeaving = useCallback((live: MarkedList) => {
    if (leavingWatch.current === null) {
      const watch = new AbortController();
      leavingWatch.current = watch;
      const options = { capture: true, passive: true, signal: watch.signal };
      document.addEventListener(
        "focusin",
        (focus) => {
          const { target } = focus;
          leavingFocus.current =
            target instanceof Element && leavingRowOf(latest.current, target) !== null
              ? target
              : null;
        },
        options
      );
      const onAct = (event: Event) => {
        if (readerActed(event)) leavingFocus.current = null;
      };
      for (const kind of READER_ACTS) document.addEventListener(kind, onAct, options);
    }
    const focused = document.activeElement;
    if (leavingRowOf(live, focused) !== null) leavingFocus.current = focused;
    else if (!focusOnDocument()) leavingFocus.current = null;
  }, []);
  /** Owes `next` and watches the reader's own acts until it is settled. */
  const owe = useCallback(
    (next: OwedFor) => {
      settle();
      const acted = new AbortController();
      const entry: Owed = { ...next, unwatch: () => acted.abort() };
      const onAct = (event: Event) => {
        if (owed.current !== entry) return;
        if (!readerActed(event)) return;
        // Nothing lands between the act and the read below, the table's own landing included: the
        // page's keymap answers the key before anything moves the focus, and Space ticks the mark
        // at its keyup.
        if (entry.kind === "own") entry.abandon();
        settle();
        const last = entry.kind === "own" ? entry.id : entry.last;
        const pressedOutsideRows =
          event.type === "pointerdown" && listRowOf(latest.current, event.target) === null;
        // The control that held the focus last since the act, which a focus on the body at the
        // read fell from: a move of the focus raises `focusin`, and a control taken out of the
        // page with its row raises none.
        let from = document.activeElement;
        const moves = new AbortController();
        document.addEventListener(
          "focusin",
          (focus) => {
            if (focus.target instanceof Element) from = focus.target;
          },
          { capture: true, passive: true, signal: moves.signal }
        );
        // Read once the act has been answered: a key the page's keymap answers moves the focus
        // itself, and Space ticks the mark at its keyup. Settling again or losing the window's
        // focus before a key's read clears the wait without reading.
        pending.current = afterAnswer(event, moves, (answered) => {
          pending.current = null;
          if (answered) answerAct.current(last, from, pressedOutsideRows);
        });
      };
      for (const kind of READER_ACTS) {
        document.addEventListener(kind, onAct, {
          capture: true,
          passive: true,
          signal: acted.signal,
        });
      }
      owed.current = entry;
      owedFocus.current = document.activeElement;
    },
    [settle]
  );
  /** Has a windowed table draw its listed, undrawn row `id` and land on its mark once drawn, owes
   *  that landing so a reader's act takes it back, and puts the focus on the list's own stop
   *  meanwhile where it stands on the page's body. Answers whether the list gave `drawRow`. */
  const drawLast = useCallback(
    (live: MarkedList, id: string): boolean => {
      if (live.drawRow === undefined) return false;
      owe({
        abandon: live.drawRow(id, (mark) => refitAfterLand(latest.current, mark)),
        id,
        kind: "own",
      });
      if (focusOnDocument()) {
        focusListStop(live.list());
        owedFocus.current = document.activeElement;
      }
      return true;
    },
    [owe]
  );
  /** Lands the place the rows the list holds now give it. */
  const landNow = useCallback(
    (live: MarkedList, last: string | null): void => {
      const order = live.order();
      const head = live.head?.() ?? null;
      const listed = last !== null && order.includes(last);
      const id = listed
        ? last
        : last === null
          ? firstMarkShown(order, live.control, head)
          : markAtPlace(order, before.current, last, live.control, head);
      const mark = id === null ? null : live.control(id);
      if (mark !== null) {
        if (landNearest(mark, head)) refitAfterLand(live, mark);
        return;
      }
      if (listed && drawLast(live, last)) return;
      focusListStop(live.list());
    },
    [drawLast]
  );
  /** Makes the owed landing after the foot's pass, unless the page has moved the focus on since to
   *  a control that stays, which ends it unmade (Handed on). Where the reader's act left the focus
   *  in a row the list named as leaving, and that row is still a row of the list with the focus in
   *  it now that no row is named leaving, the row did not go: the act's end stands, and the landing
   *  ends unmade at once (Cancelled). Reads the floats again before fitting the list so the first
   *  fit uses the room the list keeps. A later commit asks again while the same landing is owed,
   *  and the first answer settles it. */
  const landAfterFootPass = useCallback(() => {
    const place = owed.current;
    if (place?.kind !== "place") return;
    // The row the reader's act left the focus in, still in the page and still a row of the list
    // with the focus in it now that no row is named leaving: it did not go (Cancelled).
    const focused = document.activeElement;
    const { actRow } = place;
    if (
      actRow?.isConnected === true &&
      actRow.contains(focused) &&
      listRowOf(latest.current, focused) !== null
    ) {
      settle();
      return;
    }
    afterFootPass(() => {
      if (owed.current !== place) return;
      settle();
      const live = latest.current;
      if (focusMovedOn(live, owedFocus.current)) return;
      footPassNow();
      live.fit?.();
      landNow(live, place.last);
    });
  }, [landNow, settle]);
  const parkOrLand = useCallback(
    (live: MarkedList, last: string | null) => {
      if (anyLeaving(live)) {
        if (focusOnDocument()) {
          park(live, last);
          owedFocus.current = document.activeElement;
        }
        return;
      }
      landAfterFootPass();
    },
    [landAfterFootPass]
  );
  // Each commit of the host asks the hand-off again while a landing is owed: the commit that lets
  // the press's rows go is the one that makes the landing. The page's own landing is its table's,
  // and is made once the focus stands in the list's rows.
  useLayoutEffect(() => {
    const live = latest.current;
    const went = drawn.current && !live.shown;
    drawn.current = live.shown;
    const place = owed.current;
    if (place === null) {
      // The bar gone with the focus in a row the list names as leaving: the reader unticked the
      // last mark while the press's writes are out, by a click that leaves the focus on the mark,
      // as Chromium's and Firefox's do. The bar hands on only a focus its going dropped onto the
      // body (`land`), and this one is the reader's, so nothing moves now. The row's going would
      // drop it onto the body, so the landing is owed, as after the bar's own hand-off. So it is
      // where this commit took that row away with the bar and dropped the focus onto the body
      // first: the control remembered in a leaving row has left the page. The landing is asked
      // for at once, as each commit asks while it is owed, so with no row leaving it is made now.
      if (
        went &&
        (focusInLeavingRow(live) ||
          (focusOnDocument() && leavingFocus.current?.isConnected === false))
      ) {
        const last = live.lastMarked();
        owe({ actRow: null, kind: "place", last });
        parkOrLand(live, last);
        return;
      }
      if (live.shown) rememberLeaving(live);
      else forgetLeaving();
      return;
    }
    if (place.kind === "own") {
      const stop = live.list();
      const focused = document.activeElement;
      if (stop?.contains(focused) === true && focused !== stop) settle();
      return;
    }
    parkOrLand(live, place.last);
  });
  /** Owes the landing on the place the row `last` gives, with the row a reader's act left the
   *  focus in where its read owes it again (`actRow`, else null), and asks for it at once, as each
   *  commit asks while it is owed: the focus parked while the list names a listed row as leaving,
   *  else the landing made after the foot's pass. */
  const owePlace = useCallback(
    (live: MarkedList, last: string | null, actRow: Element | null) => {
      owe({ actRow, kind: "place", last });
      parkOrLand(live, last);
    },
    [owe, parkOrLand]
  );
  // Where the reader's act left the focus, once it has been answered: in a row the list names as
  // leaving, or on the body because the control that held it (`from`) went with its row, the
  // landing the act ended is owed again, on the place of the same row marked last, with the row
  // the act left the focus in (`actRow`), whose staying in the list ends it unmade once no row is
  // named leaving. A focus on a control that stays, or on the body by the reader's own doing,
  // leaves nothing owed.
  answerAct.current = (last, from, pressedOutsideRows) => {
    const live = latest.current;
    const actRow = leavingRowOf(live, document.activeElement);
    const owesAgain = focusOnDocument()
      ? !pressedOutsideRows && from?.isConnected === false
      : actRow !== null;
    if (owesAgain) {
      owePlace(live, last, actRow);
    }
  };
  return {
    land: () => {
      const last = list.lastMarked();
      // A list that cannot draw the row marked last, which it holds and its window has not drawn,
      // leaves that landing to the page, where nothing is leaving. Else the landing is owed: the
      // focus is parked while a listed row is leaving, and where none is, the place is the place,
      // so Idle goes straight to Landed, after the foot's pass as every landing does, the table's
      // own on a row its window has not drawn included (`landNow`), and nothing is parked
      // meanwhile.
      if (
        list.drawRow === undefined &&
        last !== null &&
        list.control(last) === null &&
        list.order().includes(last) &&
        !anyLeaving(list)
      ) {
        return false;
      }
      owePlace(list, last, null);
      return true;
    },
  };
}

/** What a landing of the hand-off owes (`useLastMarkedPlace`): its own landing on the place the row
 *  the reader marked last chooses, with the row a reader's act left the focus in where the act's
 *  read owed it again (`actRow`, else null), or a windowed table's landing on a row it is drawing
 *  (`MarkedList`'s `drawRow`), with what takes that landing back. */
type OwedFor =
  | { readonly actRow: Element | null; readonly kind: "place"; readonly last: string | null }
  | { readonly abandon: () => void; readonly id: string; readonly kind: "own" };

/** An owed landing, with what ends its watch on the reader's acts. */
type Owed = OwedFor & { readonly unwatch: () => void };

/** Whether the list names a row it holds as leaving (`MarkedList`'s `leaving`). */
function anyLeaving(list: MarkedList): boolean {
  const { leaving } = list;
  return leaving !== undefined && list.order().some((row) => leaving(row));
}

/** Whether the focus stands in a row the list holds and names as leaving (`MarkedList`'s
 *  `leaving`): on its mark, or anywhere in the row around it. */
function focusInLeavingRow(list: MarkedList): boolean {
  return leavingRowOf(list, document.activeElement) !== null;
}

/** Whether the focus stands somewhere other than `noted`, where it stood once the landing was
 *  owed or once the hand-off itself last moved it, on a control that stays: not on the page's
 *  body, not on the list's own stop, and in no row the list names as leaving
 *  (`useLastMarkedPlace`'s Handed on). */
function focusMovedOn(list: MarkedList, noted: Element | null): boolean {
  const focused = document.activeElement;
  return (
    focused !== noted && !focusOnDocument() && focused !== list.list() && !focusInLeavingRow(list)
  );
}

/** The row the list holds and names as leaving (`MarkedList`'s `leaving`) that contains `target`,
 *  on its mark or anywhere in the row around it, else null. */
function leavingRowOf(list: MarkedList, target: EventTarget | null): Element | null {
  const { leaving } = list;
  return leaving === undefined ? null : listRowOf(list, target, leaving);
}

/** The element of the first drawn row of the list, in its order, that contains `target`, limited
 *  to rows `matches` accepts if given, else null. */
function listRowOf(
  list: MarkedList,
  target: EventTarget | null,
  matches?: (row: string) => boolean
): Element | null {
  if (!(target instanceof Node)) return null;
  for (const row of list.order()) {
    if (matches !== undefined && !matches(row)) continue;
    const element = list.control(row)?.closest(ROW_SELECTOR);
    if (element?.contains(target) === true) return element;
  }
  return null;
}

/** The bar's hand-off, as the hook answers it (`useLastMarkedPlace`). */
export interface MarkedPlace {
  /** Hands the focus to the place of the row marked last, and answers whether it moved anything:
   *  false only where the list holds that row, has not drawn it and gave no `drawRow`, in which
   *  case the page hands the focus to the list's own stop (`focusListStop`). */
  readonly land: () => boolean;
}

/** What the reader does that the hand-off reads as their act (`useLastMarkedPlace`): while a
 *  landing is owed, and while the bar is drawn, where it forgets the control remembered in a
 *  leaving row. A press, a key or a wheel of theirs, read on the document's capture so nothing can
 *  stop it on the way. Where it leaves the focus decides whether the landing stays owed. A press
 *  the armed press counts as its own is not one (`pressIsArmedOwn`), and neither is a keydown of a
 *  modifier alone (`LONE_MODIFIERS`): a reader who holds Shift for Shift+Tab has not acted yet,
 *  and the Tab that follows is their act (`readerActed`). */
const READER_ACTS = ["pointerdown", "keydown", "wheel"] as const;

/** Whether `event`, one of `READER_ACTS`, is the reader's act: no press the armed press counts as
 *  its own (`pressIsArmedOwn`), and no keydown of a modifier alone (`LONE_MODIFIERS`). */
function readerActed(event: Event): boolean {
  if (event.type !== "wheel" && pressIsArmedOwn(event.target)) return false;
  return !(event instanceof globalThis.KeyboardEvent && LONE_MODIFIERS[event.key] === true);
}

/** Ends the wait for the reader's act and calls `finish` with whether to read the focus.
 *  A press or wheel is read in the next task. A key is read in a task after the keyup with
 *  the same `code`, so Space can tick its mark and a modifier's keyup cannot run the read
 *  early. Window `blur` and the returned cancel function end the wait without a read.
 *  Every end removes the listeners, including the one tracking focus while the act is answered. */
function afterAnswer(
  event: Event,
  moves: AbortController,
  finish: (answered: boolean) => void
): () => void {
  let timer: number | undefined;
  const end = (answered: boolean) => {
    if (moves.signal.aborted) return;
    moves.abort();
    window.clearTimeout(timer);
    finish(answered);
  };
  const queueRead = () => {
    if (timer === undefined) timer = window.setTimeout(() => end(true), 0);
  };
  if (event instanceof globalThis.KeyboardEvent) {
    const { code } = event;
    const options = { passive: true, signal: moves.signal };
    document.addEventListener(
      "keyup",
      (up) => {
        if (up.code === code) queueRead();
      },
      { ...options, capture: true }
    );
    window.addEventListener("blur", () => end(false), options);
  } else {
    queueRead();
  }
  return () => end(false);
}

/** Parks the focus while a landing is owed, with no scroll.
 *  The park is the mark of the row the reader marked last, while the list draws that row and the
 *  reader can see part of the mark below whatever the page draws over the list, as the landing
 *  judges a mark (`markShown`): their own row is where the page reads their place from, which a
 *  list's anchor keeps the view against (`features/shell/ViewportAnchor.tsx`). A mark they cannot
 *  see would be a checkbox their Space toggles out of their sight, so the list's own stop takes
 *  the focus there, as it does once that row has gone. The landing itself is still owed either way:
 *  the commit that takes the press's rows away makes it, wherever the park left the focus. */
function park(list: MarkedList, last: string | null): void {
  const mark = last === null ? null : list.control(last);
  if (mark === null || !markShown(mark, new Map(), list.head?.() ?? null)) {
    focusListStop(list.list());
    return;
  }
  focusNoScroll(mark);
}

/** Runs the foot's own pass early after a landing's scroll, fits the list's box again and checks
 *  the landing once more against the box as it then stands, in the landing's own task: a pass
 *  that ran as the bar went keeps a scroller's end room while a box it holds stands at its end,
 *  and a landing that scrolls that box off its end lets this pass take the room away before the
 *  first painted frame, so the box never resizes a frame after the focus lands. Moves nothing
 *  where the mark already stands whole below the head. */
function refitAfterLand(live: MarkedList, mark: HTMLElement): void {
  const roomChanged = footPassNow();
  if (!roomChanged && live.fit === undefined) return;
  live.fit?.();
  if (document.activeElement === mark) scrollToNearest(mark, live.head?.() ?? null);
}

/** Puts the focus on a list's own stop with no scroll, as a component's hand-off gives a region
 *  its own (`focusOwnStop`). It is where the selection bar's hand-off ends when no row in sight can
 *  take the focus, the emptied list included, since the page's search box takes typed text the
 *  reader never chose. Answers null, so a page's `returnFocus` can end with it and leave the bar
 *  nothing to move. */
export function focusListStop(stop: HTMLElement | null): null {
  if (stop !== null) focusOwnStop(stop);
  return null;
}

/** The row whose mark the focus takes after the row `last` left the list: the row now at its
 *  place in `kept`, the rows the list still holds, when the reader can see any part of its mark
 *  there (`markShown`), else the nearest row before it whose mark they can see, else the nearest
 *  row after it whose mark they can see, else null. The place is the place of the first row after
 *  `last` in `before`, the order last read with it, that `kept` holds; past the last row where
 *  none is; none where `before` does not hold `last`. Each row is read as it stands: the landing
 *  is made once the rows the press took away have gone. A row after the place takes the focus
 *  where nothing at it or before it is in sight: the rows that went stood above the view, or the
 *  reader scrolled past the place before the press. */
function markAtPlace(
  kept: readonly string[],
  before: readonly string[],
  last: string,
  control: (id: string) => HTMLElement | null,
  head: Element | null
): string | null {
  const from = before.indexOf(last);
  if (from === -1) return null;
  const places = new Map(kept.map((id, index) => [id, index]));
  let place = kept.length;
  for (let index = from + 1; index < before.length; index += 1) {
    const now = places.get(before[index]);
    if (now !== undefined) {
      place = now;
      break;
    }
  }
  const clips = new Map<Element, boolean>();
  for (let index = Math.min(place, kept.length - 1); index >= 0; index -= 1) {
    const mark = control(kept[index]);
    if (mark !== null && markShown(mark, clips, head)) return kept[index];
  }
  // Nothing at the place or before it is in sight: the rows the going took away stood above the
  // reader's view, or the reader scrolled past the place long before, so the nearest row after it
  // that they can see is their place now. Only where none is does the list's own stop take it.
  for (let index = place + 1; index < kept.length; index += 1) {
    const mark = control(kept[index]);
    if (mark !== null && markShown(mark, clips, head)) return kept[index];
  }
  return null;
}

/** The row whose mark the focus takes when no row is marked: the first row in `kept`, the rows
 *  the list holds, whose mark the reader can see below whatever the page draws over the list
 *  (`markShown`), else null. */
function firstMarkShown(
  kept: readonly string[],
  control: (id: string) => HTMLElement | null,
  head: Element | null
): string | null {
  const clips = new Map<Element, boolean>();
  for (const id of kept) {
    const mark = control(id);
    if (mark !== null && markShown(mark, clips, head)) return id;
  }
  return null;
}

/** Whether the reader can see the mark now. Hit-test the middle of its visible part, clipped to
 *  the window and scroll boxes and below the sticky head: the mark's own centre can be hidden even
 *  when part of it is visible. The hit must be in the mark's row. When happy-dom reports no hit,
 *  use the geometry alone. */
function markShown(mark: HTMLElement, clips: Map<Element, boolean>, head: Element | null): boolean {
  const point = visibleMarkPoint(mark, clips, head);
  if (point === null) return false;
  const drawn = document.elementFromPoint(point.x, point.y);
  return drawn === null || (mark.closest(ROW_SELECTOR) ?? mark).contains(drawn);
}

/**
 * The one bar a page shows while rows are marked: how many, what one press does to all of them,
 * and the way out.
 *
 * It is `fixed`, over the list rather than in it, which is what keeps the list's height:
 * selecting a row leaves the top of the list's first row where it was, and a bar drawn in the
 * page would push every row down the moment the first one was marked. It sits
 * under the header on a narrow screen and at the foot of the screen from `sm`, so the rows a
 * reader is marking stay in sight at either width.
 *
 * The bar is one line of actions that scrolls sideways - an action pushed onto a second line
 * would make the bar taller under the reader's hand - and a notice, which takes a line of its own.
 * The compact line, the project List's, wraps from `sm` up, where the bar stands at the foot and
 * the foot inset gives the list its room (`BAR_LINE`).
 *
 * A bar that goes while it holds the keyboard focus because the reader caused it - **Clear
 * selection** pressed, Escape from a control inside it, a press on a row's mark that took the last
 * marked row away (`causedByReader`, `pressOnRowMark`) - hands the focus to what the page's
 * `returnFocus` names, rather than drop it onto the page, where a keyboard reader would start
 * again from the page's first link. So does a bar its own press takes away after the reader's
 * press on a control outside it that the press holds while its write is out took the focus from
 * it, if that control goes too or stays and the focus is on the body (`focusOnDocument`).
 * Take out of epic's writes hold every row's Start next, and a click or a tap on a marked row's
 * one puts the focus on that Start next, on the row around it or on the body, and leaves the
 * press the cause (`repeatTookFocusFrom`). So does a bar that stays while the control holding
 * the focus goes from it: **Select all**, which the bar stops offering once it has run. The focus
 * goes to the first of these that applies, which the page's own hand-off focuses
 * (`useLastMarkedPlace`). The checkbox of the row the reader marked last, while the list holds it
 * and no press is taking it away, out of sight or not. Else a row the reader can see: after a
 * press that took that row away, the row at its place, else the nearest before it, else the
 * nearest after it, read once those rows have gone and the list stands as the reader sees it;
 * with no row marked, the first row in sight below whatever the page draws over the list. A row's
 * checkbox is shown whole at its nearest edge, the least scroll, never centred as the engine's own
 * scroll of a focus would. Else the list's own stop, with no scroll. The bar lands what the page
 * names the same way. A bar that goes while the focus is elsewhere moves nothing, and neither
 * does one whose going left the focus somewhere other than the body, since the reader put it
 * there, nor the later going of a control the reader had moved the focus off, nor the going of one
 * they had scrolled out of their sight - the bar's line of actions may scroll sideways
 * (`outOfSight`). Nor does a going the reader did not cause - a refetch
 * that drops every marked row and empties the bar - which leaves the focus on the body, in no row,
 * and scrolls nothing. The press these read is the one that armed the hand-off (`armHandOff`): a
 * press on the bar's own actions arms it at the bar's root (`armOnPress`), and so does Escape from
 * a control inside the bar (`armOnBarKey`); a press on a row's mark arms it while the bar is
 * drawn. A going with nothing armed leaves the focus on the body and scrolls nothing.
 */
export function SelectionBar({ returnFocus, ...bar }: SelectionBarProps): ReactNode {
  // Set by the drawn bar to what held the focus when the focus dropped with it - the bar's
  // element, or a control in it, as the bar goes, the bar's element as well where the reader's
  // press on a control outside it that the bar's press holds took the focus from it, or the
  // focused control inside a bar that stays - and read after the commit that took it away.
  const wentHolding = useRef<Element | null>(null);
  // After every commit, so after the one that removed the bar's element, or the control, and
  // dropped the focus with it. No dependency list: the `returnFocus` called is this commit's own,
  // so a page that hands a new function on every render starts nothing by it. Passive rather than
  // layout: the Inbox's `ViewportAnchor` keeps its rows in place from its `componentDidUpdate`,
  // which runs in the layout phase after this bar's own layout effects and would scroll the list
  // back over the target that focusing brought into view.
  useEffect(() => {
    const held = wentHolding.current;
    if (held === null) return;
    wentHolding.current = null;
    if (focusOnDocument() && !outOfSight(held) && (causedByReader(held) || pressOnRowMark())) {
      const named = returnFocus?.() ?? null;
      if (named !== null) landNearest(named);
    }
  });
  if (bar.count === 0 && bar.state === undefined) return null;
  return <DrawnBar {...bar} wentHolding={wentHolding} />;
}

/** How the bar's line of actions stands at each size. Each line is one line that scrolls sideways
 *  where it must, and each control keeps its own text on one line and its own size at every size.
 *  The compact line, the project List's bar, wraps onto more lines from `sm` up, where the bar
 *  stands at the foot of the screen and the foot inset gives the list the room the bar takes
 *  (`hooks/useFootInset.ts`): its actions hold more room than its box at 900 px, and main's layout
 *  rule reads a box that scrolls sideways as a breach (builds Job
 *  `infra-3135-infra-3135-r29m-lay-z5wrh`, and cybermonklabs/legion#4304). Below `sm` the bar stands
 *  under the header, over the list's top rows, with no room given for it, so it keeps one line
 *  there: wrapped at 390x664 it stood 210 px tall over the rows it acts on, and a double click on a
 *  marked row's held Start next landed on the bar instead (builds Jobs
 *  `infra-3135-infra-3135-r30j-dblp` and `-r30k-dblp`). `e2e/layout-breaches.ts` holds the
 *  sideways scroll that one line leaves at 360. */
const BAR_LINE: Record<ControlSize, string> = {
  comfortable: "flex-nowrap overflow-x-auto",
  compact: "flex-nowrap overflow-x-auto sm:flex-wrap sm:overflow-x-visible",
  toolbar: "flex-nowrap overflow-x-auto",
};

/** The bar's own box at each size. The comfortable size is the bar the Inbox draws. The compact
 *  size is the smaller box of a page whose controls are compact, in the padding the compact
 *  controls take (`controlPadding`); the toolbar size's controls take that padding too. */
const BAR_BOX: Record<ControlSize, string> = {
  comfortable: "px-4 py-2",
  compact: "px-2 py-1",
  toolbar: "px-2 py-1",
};

/** Tailwind's `sm` itself: true exactly when the bar's `sm:` classes put it at the foot of the
 *  screen rather than under the header. In `rem`, for `MD_VIEWPORT_QUERY`'s reason
 *  (`features/shell/useDialog.ts`): Tailwind declares `sm` as `40rem`, so only a `rem` query
 *  switches with those classes at any default font size. */
const SM_VIEWPORT_QUERY = "(min-width: 40rem)";

/** The bar's own arming of its hand-off at its root, for a key: what every component's root arms
 *  for (`armOnPress`), and Escape from a control inside the bar besides, which the bar's hand-off
 *  counts as the reader's own (`SelectionBar`): on a page whose keymap clears the selection on
 *  Escape, as the Inbox's does, Escape there clears the selection the bar holds (the project List
 *  binds no such key), where a component's root reads it as the reader leaving a control. */
function armOnBarKey(event: KeyboardEvent<HTMLDivElement>): void {
  if (event.key === "Escape" && event.target instanceof Element) {
    armHandOff(event.target, event.currentTarget);
  } else {
    armOnPress(event);
  }
}

/** The bar itself, mounted exactly while it is drawn, so its going is its own unmount. */
function DrawnBar({
  barRef,
  children,
  count,
  label,
  notice,
  size = "comfortable",
  state,
  wentHolding,
}: Omit<SelectionBarProps, "returnFocus"> & {
  /** Set to what held the focus when the bar, or the control inside it that held the focus, goes
   *  with it: the bar's own element where the reader's press on a control outside it that the
   *  bar's press holds took the focus from it. */
  readonly wentHolding: RefObject<Element | null>;
}): ReactNode {
  const ownRef = useRef<HTMLElement>(null);
  const ref = barRef ?? ownRef;
  // From `sm` up the bar sits at the foot of the screen, in the foot's page layer
  // (`hooks/useFootInset.ts` lists the foot's layers and the elements in each), so the hook holds
  // its band, its box on the screen, and the floats keep clear of its controls. Below `sm` the
  // bar sits under the header, at the top of the screen and not at its foot, so it counts nothing
  // there.
  const atFoot = useMediaQuery(SM_VIEWPORT_QUERY);
  useFootInset(ref, atFoot, "page");
  // What inside the bar took the focus last, the bar's own element included, until the reader
  // moves the focus off it: a focus event bubbles up to the bar from every control in it.
  const focused = useRef<Element | null>(null);
  // The cleanup runs before React takes the element out of the page, the one moment the focus
  // can still be read inside it: the removal itself drops it onto the body. A focus that the
  // reader's press on a control outside the bar took from it, where the bar's press holds that
  // control while its write is out - a click or a tap on a marked row's Start next, held while
  // Take out of epic's writes are out - is the bar's to hand on too: that press takes nothing but
  // the hold, and the bar's press stays the cause (`armHandOff`). The armed press says so from
  // its own events, whatever renders came between (`repeatTookFocusFrom`): the focus was in the
  // bar as that press came, and has stayed since on that control, on the row around it, or on
  // the body, where WebKit can leave it. The commit that takes the bar away can take that control
  // away first, with its row - the List draws its table before its bar - and the focus with it.
  // The bar's own element is what it went holding then, the element that holds the press, unless
  // the reader had scrolled the element the focus was last on out of their sight
  // (`focusedOutOfSight`).
  useLayoutEffect(() => {
    const node = ref.current;
    return () => {
      const focus = document.activeElement;
      if (node === null) return;
      if (node.contains(focus)) {
        wentHolding.current = focus;
      } else if (pressMadeIn(node) && repeatTookFocusFrom(node) && !focusedOutOfSight()) {
        wentHolding.current = node;
      }
    };
  }, [ref, wentHolding]);
  // A press on a row's mark arms the hand-off while the bar is drawn (`armHandOff`), as a press
  // on the bar's own actions does at its root: unticking the last marked row by its mark takes
  // the bar away, and that going is the reader's (`pressOnRowMark`). Heard on the document, in
  // its capture phase, since the rows are the page's and stand outside the bar.
  useEffect(() => {
    const armOnMark = (event: Event) => {
      const { target } = event;
      if (target instanceof Element && isRowMark(target)) armHandOff(target);
    };
    document.addEventListener("click", armOnMark, true);
    document.addEventListener("change", armOnMark, true);
    return () => {
      document.removeEventListener("click", armOnMark, true);
      document.removeEventListener("change", armOnMark, true);
    };
  }, []);
  // After every render that leaves the bar drawn. A press can take the focused control out of a
  // bar that stays - **Select all** - and the removal drops the focus onto the body with no
  // unmount for the cleanup above to read it in, so it is read here: the control gone from the
  // page and the focus on the body. Read from the page rather than from a blur: the blur Chrome
  // fires for an element taken out of the page arrives while the element still reads as in the
  // page, so it cannot tell that going from the reader moving off. A control still in the page
  // that no longer holds the focus is one the reader moved off, so it is forgotten and its going
  // later moves nothing.
  useLayoutEffect(() => {
    const held = focused.current;
    if (held === null || held === document.activeElement) return;
    focused.current = null;
    if (!held.isConnected && focusOnDocument()) wentHolding.current = held;
  });
  return (
    <div
      aria-label={label}
      className={`fixed inset-x-2 top-14 z-30 mx-auto flex w-fit max-w-[calc(100vw-1rem)] flex-col gap-2 rounded-2xl border text-sm shadow-xl outline-none focus-visible:ring-2 sm:top-auto sm:bottom-4 ${BAR_BOX[size]} ${card} ${textPrimaryOnCanvas} ${focusVisibleRing}`}
      data-control={CONTROL.selectionBar}
      {...ARM_ON_PRESS}
      onKeyDownCapture={armOnBarKey}
      onFocus={(event) => {
        focused.current = event.target;
      }}
      // The element is always this one, so a caller holding a ref to the bar as an `HTMLElement`
      // - all a keymap needs to focus it - is handed exactly what it asked for.
      ref={ref as Ref<HTMLDivElement>}
      role="toolbar"
      // Focusable by a keymap and by nothing else: the bar is where Escape leaves the reader when
      // the press it answered took the row they came from off the list.
      tabIndex={-1}
    >
      <div className={`flex items-center gap-2 whitespace-nowrap [&>*]:shrink-0 ${BAR_LINE[size]}`}>
        <span className="mr-1 font-semibold">{state ?? `${count} selected`}</span>
        {children}
      </div>
      {notice === undefined ? null : (
        <p className={dangerText} role="alert">
          {notice}
        </p>
      )}
    </div>
  );
}
