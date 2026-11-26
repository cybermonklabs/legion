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
import { afterFootPass, useFootInset } from "../hooks/useFootInset";
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
  landNearest,
  outOfSight,
  pressIsArmedOwn,
  pressMadeIn,
  pressOnRowMark,
  repeatTookFocusFrom,
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
   *  (`useLastMarkedPlace`), or, where the page names the row its press hands the reader on to,
   *  gives that row itself the focus and shows it whole the same way; and the bar lands an
   *  element the page names here the same way (`landNearest`). Null moves nothing here: the page
   *  has put the focus where it goes itself, or the table will show and focus a checkbox once its
   *  row is drawn (`DataTableModel.focusRow`). */
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
  /** The row `id` itself while the page draws it, else null: the element that takes the focus,
   *  and is shown whole, where the page names the row a press hands the reader on to
   *  (`MarkedPlace`'s `land`). */
  readonly row?: (id: string) => HTMLElement | null;
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
 * `MarkedPlace`: `land`, which a page's `returnFocus` calls first, and `own`, the page's own
 * landing on a row its table is drawing. The hand-off has three states, and the host's own commits
 * move it between them (`bar-return-design.md` of dispatch://INFRA-3135):
 *
 * **Idle.** No press of the reader's is out, and the hand-off holds nothing.
 *
 * **Owed.** The bar has gone after the reader's press and the list names a listed row as leaving
 * (`MarkedList`'s `leaving`): the rows that press is taking away are still drawn, so no place is
 * the place yet. Where the going left the focus on the page's body, it is parked with no scroll
 * (`park`): on the mark of the row the reader marked last while the list draws it and the reader
 * can see part of it, else on the list's own stop. A focus the reader holds elsewhere is theirs.
 * Each commit asks again.
 *
 * **Landed.** No listed row is leaving, so the hand-off lands once and then watches nothing. The
 * place is the mark of the row the reader marked last while the list holds it, out of sight or
 * not; else, after a press that took that row away, the mark of the row now at its place, else
 * the nearest before it, else the nearest after it, each judged where the reader can see it below
 * whatever the page draws over the list (`markAtPlace`, `firstMarkShown`, which read the sticky
 * head the hand-off is given); else the list's own stop, with no scroll, which is given
 * `tabindex="-1"` where it has none. The mark takes the focus with no scroll of the engine's own,
 * which centres a control it scrolls to, and is shown whole at its nearest edge below that head,
 * the least scroll (`landNearest`). Where the page names the row its press hands the reader on
 * to (`land`'s `to`, the Inbox's R8 of dispatch://INFRA-2160) and the list still holds it, that
 * row is the place instead: the row itself takes the focus (`MarkedList`'s `row`), and is shown
 * whole at its nearest edge the same way. The landing is made once the foot's own pass has given
 * the list the room the bar's foot had back (`hooks/useFootInset.ts`'s `afterFootPass`), since
 * that pass moves the box the reader sees the rows through. Where the list holds the row marked
 * last and its window has not drawn it, `land` moves nothing and answers false: the page's table
 * draws the row and lands on it itself, and the hand-off holds what takes that landing back
 * (`own`).
 *
 * **Cancelled.** The reader's own press, key or wheel (`READER_ACTS`) ends whatever is owed, the
 * page's own landing on a row it is drawing included. A press the armed press counts as its own -
 * the second press on a control its write holds - is no such act (`pressIsArmedOwn`). Where the
 * act leaves the focus on the parked mark of a leaving row, read once its own handlers have run,
 * the list's own stop takes it with no scroll, so it does not fall to the body with that row.
 * Nothing of this follows the reader after that.
 *
 * So a reader deep in a long list keeps their place, and the focus goes neither to the list's first
 * row nor to the page's body.
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
  // The mark a park left the focus on while its row is leaving, else null.
  const parked = useRef<HTMLElement | null>(null);
  /** Ends what is owed and its watch on the reader's acts. It moves no focus. */
  const settle = useCallback(() => {
    const was = owed.current;
    owed.current = null;
    parked.current = null;
    was?.unwatch();
  }, []);
  useEffect(() => settle, [settle]);
  // After every commit, so the commit that takes the press's rows away keeps the order before it.
  useLayoutEffect(() => {
    if (!list.shown) return;
    const last = list.lastMarked();
    if (last === null) return;
    const order = list.order();
    if (order.includes(last)) before.current = order;
  });
  /** Lands the place the rows the list holds now give it: the row `to` names, where the page named
   *  one and the list still holds it, else the place of the row marked last. */
  const landNow = useCallback(
    (live: MarkedList, last: string | null, to: string | undefined): void => {
      const order = live.order();
      const head = live.head?.() ?? null;
      const named =
        to !== undefined && order.includes(to) ? (live.row?.(to) ?? live.control(to)) : null;
      if (named !== null) {
        landNearest(named, head);
        return;
      }
      const id =
        last !== null && order.includes(last)
          ? last
          : last === null
            ? firstMarkShown(order, live.control, head)
            : markAtPlace(order, before.current, last, live.control, head);
      const mark = id === null ? null : live.control(id);
      if (mark !== null) landNearest(mark, head);
      else focusListStop(live.list());
    },
    []
  );
  /** Makes the owed landing once the foot's pass has given the list its room back. A later commit
   *  asks again while the same landing is owed, and the first answer settles it. */
  const landAfterFootPass = useCallback(() => {
    const place = owed.current;
    if (place?.kind !== "place") return;
    afterFootPass(() => {
      if (owed.current !== place) return;
      settle();
      landNow(latest.current, place.last, place.to);
    });
  }, [landNow, settle]);
  /** Owes `next` and watches the reader's own acts until it is settled. */
  const owe = useCallback(
    (next: OwedFor) => {
      settle();
      const acted = new AbortController();
      const entry: Owed = { ...next, unwatch: () => acted.abort() };
      const onAct = (event: Event) => {
        if (owed.current !== entry) return;
        if (event.type !== "wheel" && pressIsArmedOwn(event.target)) return;
        const stood = parked.current;
        if (entry.kind === "own") entry.abandon();
        settle();
        if (stood === null) return;
        // Read once the act's own handlers have run: a key the page's keymap answers moves the
        // focus itself, and Space ticks the mark. Only a focus the act left on the parked mark of
        // a leaving row goes to the list's own stop.
        window.setTimeout(() => {
          if (document.activeElement === stood) focusListStop(latest.current.list());
        }, 0);
      };
      for (const kind of READER_ACTS) {
        document.addEventListener(kind, onAct, {
          capture: true,
          passive: true,
          signal: acted.signal,
        });
      }
      owed.current = entry;
    },
    [settle]
  );
  // Each commit of the host asks the hand-off again while a landing is owed: the commit that lets
  // the press's rows go is the one that makes the landing. The page's own landing is its table's,
  // and is made once the focus stands in the list's rows.
  useLayoutEffect(() => {
    const place = owed.current;
    if (place === null) return;
    const live = latest.current;
    if (place.kind === "own") {
      const stop = live.list();
      const focused = document.activeElement;
      if (stop?.contains(focused) === true && focused !== stop) settle();
      return;
    }
    if (anyLeaving(live)) {
      if (focusOnDocument()) parked.current = park(live, place.last);
      return;
    }
    landAfterFootPass();
  });
  return {
    land: (to) => {
      const last = list.lastMarked();
      if (anyLeaving(list)) {
        owe({ kind: "place", last, to });
        if (focusOnDocument()) parked.current = park(list, last);
        return true;
      }
      // Nothing is leaving, so the place is the place: Idle goes straight to Landed, after the
      // foot's pass as every landing does, and nothing is parked meanwhile.
      if (last !== null && list.order().includes(last) && list.control(last) === null) {
        return false;
      }
      owe({ kind: "place", last, to });
      landAfterFootPass();
      return true;
    },
    own: (abandon: () => void) => {
      owe({ abandon, kind: "own" });
      if (focusOnDocument()) focusListStop(list.list());
    },
  };
}

/** What a landing of the hand-off owes (`useLastMarkedPlace`): its own landing on the place the row
 *  the reader marked last chooses, or on the row the page named (`to`), or the page's table's
 *  landing on a row it is drawing, with what takes that landing back. */
type OwedFor =
  | { readonly kind: "place"; readonly last: string | null; readonly to: string | undefined }
  | { readonly abandon: () => void; readonly kind: "own" };

/** An owed landing, with what ends its watch on the reader's acts. */
type Owed = OwedFor & { readonly unwatch: () => void };

/** Whether the list names a row it holds as leaving (`MarkedList`'s `leaving`). */
function anyLeaving(list: MarkedList): boolean {
  const { leaving } = list;
  return leaving !== undefined && list.order().some((row) => leaving(row));
}

/** The bar's hand-off, as the hook answers it (`useLastMarkedPlace`). */
export interface MarkedPlace {
  /** Hands the focus to the place of the row marked last, and answers whether it moved anything:
   *  false where the list holds that row and its window has not drawn it, which the page answers
   *  by having its table draw the row and land on it (`own`). `to` names the row the page's press
   *  hands the reader on to, by id, which is the place while the list still holds it: the row
   *  itself takes the focus and is shown whole (`MarkedList`'s `row`). */
  readonly land: (to?: string) => boolean;
  /** The page's table's own landing on the row marked last, which it is drawing for this, and
   *  `abandon`, which takes that landing back. The list's own stop holds the focus until the
   *  table lands, and the reader's own act takes the landing back. */
  readonly own: (abandon: () => void) => void;
}

/** What the reader does that ends a landing that is owed (`useLastMarkedPlace`): a press, a key or
 *  a wheel of theirs, read on the document's capture so nothing can stop it on the way. A press
 *  the armed press counts as its own is not one (`pressIsArmedOwn`). */
const READER_ACTS = ["pointerdown", "keydown", "wheel"] as const;

/** Parks the focus while a landing is owed, with no scroll, and answers the mark it left it on.
 *  The park is the mark of the row the reader marked last, while the list draws that row and the
 *  reader can see part of the mark below whatever the page draws over the list, as the landing
 *  judges a mark (`markShown`): their own row is where the page reads their place from, which a
 *  list's anchor keeps the view against (`features/shell/ViewportAnchor.tsx`). A mark they cannot
 *  see would be a checkbox their Space toggles out of their sight, so the list's own stop takes
 *  the focus there, as it does once that row has gone. The landing itself is still owed either way:
 *  the commit that takes the press's rows away makes it, wherever the park left the focus. */
function park(list: MarkedList, last: string | null): HTMLElement | null {
  const mark = last === null ? null : list.control(last);
  if (mark === null || !markShown(mark, new Map(), list.head?.() ?? null)) {
    focusListStop(list.list());
    return null;
  }
  focusNoScroll(mark);
  return mark;
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
 * Its groups, in this order: the count (or `state`) with the actions the page puts in the bar,
 * and a notice, which is the one thing that takes a line of its own. The bar is one line of
 * actions that scrolls sideways - an action pushed onto a second line would make the bar taller
 * under the reader's hand. The compact line, the project List's, wraps from `sm` up, where the
 * bar stands at the foot and the foot inset gives the list its room (`BAR_LINE`).
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

/** Tailwind's `sm` itself: true exactly when the bar's `sm:` classes put it at the foot of the
 *  screen rather than under the header. In `rem`, for `MD_VIEWPORT_QUERY`'s reason
 *  (`features/shell/useDialog.ts`): Tailwind declares `sm` as `40rem`, so only a `rem` query
 *  switches with those classes at any default font size. */
const SM_VIEWPORT_QUERY = "(min-width: 40rem)";

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

/** The bar's own arming of its hand-off at its root, for a key: what every component's root arms
 *  for (`armOnPress`), and Escape from a control inside the bar besides, which the bar's hand-off
 *  counts as the reader's own (`SelectionBar`): Escape there clears the selection the bar holds,
 *  where a component's root reads it as the reader leaving a control. */
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
  // there. It counts only while it is drawn, which is while this component is mounted.
  const atFoot = useMediaQuery(SM_VIEWPORT_QUERY);
  useFootInset(ref, atFoot, "page");
  // What inside the bar took the focus last, the bar's own element included, until the reader
  // moves the focus off it: a focus event bubbles up to the bar from every control in it. Only a
  // control in the bar's own element counts. React also bubbles the focus of a list a control of
  // the bar opened (the Snooze's text box, portalled out of the bar), and that list gives the
  // focus back itself as it closes, to where it was as it opened - the page itself where nothing
  // had it (`MultiSelect`'s `menu`) - which the bar must not take for a control of its own gone.
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
        if (event.currentTarget.contains(event.target)) focused.current = event.target;
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
