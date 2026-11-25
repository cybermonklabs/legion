import {
  type KeyboardEvent,
  type ReactNode,
  type Ref,
  type RefObject,
  useEffect,
  useLayoutEffect,
  useRef,
} from "react";

import { focusOnDocument } from "../features/shell/roving";
import { useMediaQuery } from "../features/shell/useDialog";
import { spaceTaken } from "../features/shell/ViewportAnchor";
import { useFootInset } from "../hooks/useFootInset";
import { card, dangerText, focusVisibleRing, textPrimaryOnCanvas } from "../theme/classes";
import { CONTROL, type ControlSize } from "./control-names";
import {
  ARM_ON_PRESS,
  aboveSightNow,
  armHandOff,
  armOnPress,
  causedByReader,
  focusedOutOfSight,
  focusNoScroll,
  focusOwnStop,
  inSightNow,
  isRowMark,
  landWhole,
  type MovedUp,
  outOfSight,
  pressMadeIn,
  pressOnRowMark,
  repeatTookFocusFrom,
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
   *  (`landWhole`). Null moves nothing here: the page has put the focus where it goes itself, or
   *  a table row the window has not drawn yet takes the focus as it lands
   *  (`DataTableModel.focusRow`). */
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
  /** Whether a press of the reader's is taking row `id` away while the page still draws it. The
   *  page's list lets the rows after such a row move up by the space it takes once it goes
   *  (`ViewportAnchor`'s `leaving`), and the hand-off reads each row where it will stand then. */
  readonly leaving?: (id: string) => boolean;
  /** The list's sticky head, which its rows scroll under, else null. A mark the hand-off lands is
   *  shown whole below it (`landMark`). */
  readonly head?: () => Element | null;
  /** The list's own element. It takes the focus at its own stop where no row in sight can. */
  readonly list: () => HTMLElement | null;
}

/**
 * The hand-off of the selection bar's focus to a row of the list it stands over. The hook reads,
 * after each commit that draws the bar, the order of the list's rows while the row marked last is
 * in it. It answers the function a page's `returnFocus` calls first.
 *
 * Where the row marked last is still listed and no press is taking it away, the function puts the
 * focus on that row's mark, out of sight or not. Where the list lists that row without drawing
 * it, as a table's window can, the function moves nothing and answers false, and the page brings
 * the row in itself. Where a press took that row away, or is taking it away (`leaving`), the
 * function puts the focus on the mark of the row that now stands at that row's place when the
 * reader can see it (`inSightNow`). That row is the first row after it, in the order last read,
 * that the list still holds. Else the focus goes to the mark of the nearest row before that place
 * that the reader can see. With no reading of where the row stood, the focus goes to the list's
 * own stop. Where no row is marked, the function puts the focus on the mark of the first row the
 * reader can see below whatever the page draws over the list (`markShown`), a row a press is
 * taking away passed over. A mark takes the focus with no scroll of the engine's own, which
 * centres a control it scrolls to, and is shown whole at its nearest edge, the least scroll
 * (`landMark`). Where no row can take the focus, in either case, it goes to the list's own stop,
 * with no scroll, which is given `tabindex="-1"` where it has none. The function then answers
 * true. So a reader deep in a long list keeps their place, and the focus goes neither to the
 * list's first row nor to the page's body.
 *
 * Rows a press is still taking away are drawn until the write's optimistic apply lands, and the
 * rows after them move up by the space they take only then (`onceGone`). So each row is judged
 * where it will stand once they have gone: its top less the space of the rows a press is taking
 * away above it. Where that moves the row at the place wholly above the view, which happens when
 * the rows taken away stood above the view, the focus goes to the mark of the nearest row after
 * that place that the reader will see, and only else to a row before it. A mark that will move up
 * into the reader's sight takes the focus with no scroll, and the list's anchor shows it whole
 * once those rows have gone (`ViewportAnchor`).
 */
export function useLastMarkedPlace(list: MarkedList): () => boolean {
  // The ids of the list's rows, in order, as the last commit that drew the bar with the row
  // marked last in the list read them.
  const before = useRef<readonly string[]>([]);
  // After every commit, so the commit that takes the row away keeps the reading before it.
  useLayoutEffect(() => {
    if (!list.shown) return;
    const last = list.lastMarked();
    if (last === null) return;
    const order = list.order();
    if (order.includes(last)) before.current = order;
  });
  return () => {
    const last = list.lastMarked();
    const order = list.order();
    const kept = order.filter((id) => list.leaving?.(id) !== true);
    const listed = last !== null && kept.includes(last);
    const landing = onceGone(order, list);
    const id = listed
      ? last
      : last === null
        ? firstMarkShown(kept, list.control, landing)
        : markAtPlace(kept, before.current, last, list.control, landing);
    const mark = id === null ? null : list.control(id);
    if (id !== null && mark !== null) {
      landMark(mark, landing.moves.get(id), list.head?.() ?? null);
      return true;
    }
    if (listed) return false;
    focusListStop(list.list());
    return true;
  };
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

/** Where the rows a list keeps will stand once the rows a press is still taking away (`leaving`)
 *  have gone, as the page's list lets them go (`ViewportAnchor`): each kept row that will move
 *  up, by id, with how far, and `listed`, every row the list draws and every group heading that
 *  goes with the rows taken away. Where a row will move up, one of `listed` stands now. */
interface Landing {
  readonly listed: ReadonlySet<Element>;
  readonly moves: ReadonlyMap<string, MovedUp>;
}

/** Reads `Landing` for the rows `order` names. A drawn row a press is taking away frees the space
 *  it takes (`spaceTaken`) for every row after it. So does the element right before it that is
 *  no row, a group's heading, where every row from that element to the next element that is no
 *  row is being taken away too: the group goes with its last rows. A row already gone frees
 *  nothing more, since the rows after it stand where they are. Where the list names no row as
 *  leaving, no row moves and nothing is read. */
function onceGone(order: readonly string[], list: MarkedList): Landing {
  const { control, leaving } = list;
  const moves = new Map<string, MovedUp>();
  if (leaving === undefined || !order.some((id) => leaving(id))) {
    return { listed: new Set(), moves };
  }
  const idOf = new Map<Element, string>();
  for (const id of order) {
    const row = control(id)?.closest(ROW_SELECTOR);
    if (row !== null && row !== undefined) idOf.set(row, id);
  }
  const listed = new Set<Element>(idOf.keys());
  let by = 0;
  for (const [row, id] of idOf) {
    if (!leaving(id)) {
      if (by > 0) moves.set(id, { by, row });
      continue;
    }
    const lead = row.previousElementSibling;
    if (lead !== null && !idOf.has(lead) && groupGoes(lead, idOf, leaving)) {
      listed.add(lead);
      by += spaceTaken(lead);
    }
    by += spaceTaken(row);
  }
  return { listed, moves };
}

/** Whether every row `idOf` names from the element after `lead` to the next element that is no
 *  row is being taken away (`leaving`). */
function groupGoes(
  lead: Element,
  idOf: ReadonlyMap<Element, string>,
  leaving: (id: string) => boolean
): boolean {
  for (let next = lead.nextElementSibling; next !== null; next = next.nextElementSibling) {
    const id = idOf.get(next);
    if (id === undefined) return true;
    if (!leaving(id)) return false;
  }
  return true;
}

/** The row whose mark the focus takes after the row `last` left the list: the row now at its
 *  place in `kept`, the rows the list still holds, when the reader can see its mark there with
 *  nothing drawn over it (`markShown`), else the nearest row before it whose mark they can see
 *  so, else null. The place is the place of the first row after `last` in `before`, the order
 *  last read with it, that `kept` holds; past the last row where none is; none where `before`
 *  does not hold `last`. Each row is read where it will stand once the rows a press is still
 *  taking away have gone (`landing`). Measured at 390 px in Chromium: a Dismiss handed the focus
 *  on while the dismissed thread still stood in view, and the row after it, below the window,
 *  moved up into view a frame later.
 *
 *  Where those rows going moves the row at the place wholly above the view, the rows after it
 *  move up into the view with it: the mark of the nearest of them the reader will see takes the
 *  focus, passed over while it still reaches above the view, and only where none does is a row
 *  before the place tried. */
function markAtPlace(
  kept: readonly string[],
  before: readonly string[],
  last: string,
  control: (id: string) => HTMLElement | null,
  landing: Landing
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
  const placeId: string | undefined = kept[place];
  const atPlace = placeId === undefined ? null : control(placeId);
  const lifted = placeId === undefined ? undefined : landing.moves.get(placeId);
  if (
    atPlace !== null &&
    lifted !== undefined &&
    !inSightNow(atPlace, clips, lifted) &&
    aboveSightNow(atPlace, clips, lifted)
  ) {
    for (let index = place + 1; index < kept.length; index += 1) {
      const mark = control(kept[index]);
      if (mark === null) break;
      const moved = landing.moves.get(kept[index]);
      if (markShown(mark, clips, moved, landing.listed)) return kept[index];
      if (!aboveSightNow(mark, clips, moved)) break;
    }
  }
  for (let index = Math.min(place, kept.length - 1); index >= 0; index -= 1) {
    const mark = control(kept[index]);
    if (mark !== null && markShown(mark, clips, landing.moves.get(kept[index]), landing.listed)) {
      return kept[index];
    }
  }
  return null;
}

/** The row whose mark the focus takes when no row is marked: the first row in `kept`, the rows
 *  the list holds, whose mark the reader can see below whatever the page draws over the list
 *  (`markShown`), each read where it will stand once the rows a press is still taking away have
 *  gone (`landing`), else null. */
function firstMarkShown(
  kept: readonly string[],
  control: (id: string) => HTMLElement | null,
  landing: Landing
): string | null {
  const clips = new Map<Element, boolean>();
  for (const id of kept) {
    const mark = control(id);
    if (mark !== null && markShown(mark, clips, landing.moves.get(id), landing.listed)) return id;
  }
  return null;
}

/** Lands `mark`, the row mark the bar's hand-off chose, with no scroll of the engine's own, and
 *  shows it whole at its nearest edge, the least scroll, wherever it stands, below the list's
 *  sticky head `head` too (`landWhole`). A mark whose row will move up into the reader's sight once
 *  the rows a press is still taking away have gone (`moved`) takes the focus with no scroll
 *  instead: shown now, it would be shown where it stands before they go, and the list's anchor
 *  shows it whole once they have gone (`ViewportAnchor`). */
function landMark(mark: HTMLElement, moved: MovedUp | undefined, head: Element | null): void {
  if (moved !== undefined && inSightNow(mark, new Map(), moved)) {
    focusNoScroll(mark);
  } else {
    landWhole(mark, head);
  }
}

/** Whether the reader can see the row mark `mark` with nothing drawn over it, now or, with
 *  `moved`, once its row has moved up. Its box must be in sight (`inSightNow`), and its centre
 *  inside the window. What the page draws at that centre must be the mark or a part of its row
 *  (`ROW_SELECTOR`). A list's sticky head, or other chrome the rows scroll under, can stand over a
 *  row whose box is in sight, and is what the page draws there then. A row that will move up is
 *  drawn at that point only once the rows above it have gone, so there a part of any element of
 *  `listed`, the list's rows and the headings that go with them, stands for its own. That holds
 *  for a point in a row nested inside such an element, as an Inbox row draws an ask card's
 *  `<article>`. It holds too for a point inside the list that is in none of its rows: a gap its
 *  spacing leaves between them. The list is the element that holds the row that moves up. A mark
 *  with its centre out of the window shows half of itself at most, and one whose edge only
 *  touches the window's edge shows none of itself. Where the engine names no element at a point
 *  inside the window (happy-dom lays nothing out), the box and the centre decide alone. */
function markShown(
  mark: HTMLElement,
  clips: Map<Element, boolean>,
  moved: MovedUp | undefined,
  listed: ReadonlySet<Element>
): boolean {
  if (!inSightNow(mark, clips, moved)) return false;
  const box = mark.getBoundingClientRect();
  const x = box.left + box.width / 2;
  const y = box.top - (moved?.by ?? 0) + box.height / 2;
  if (x < 0 || y < 0 || x >= window.innerWidth || y >= window.innerHeight) return false;
  const drawn = document.elementFromPoint(x, y);
  if (drawn === null || (mark.closest(ROW_SELECTOR) ?? mark).contains(drawn)) return true;
  if (moved === undefined) return false;
  // Rows nest, so every row around the point is tried, the innermost first.
  const inner = drawn.closest(ROW_SELECTOR);
  for (let row = inner; row !== null; row = row.parentElement?.closest(ROW_SELECTOR) ?? null) {
    if (listed.has(row)) return true;
  }
  // A point of the list that none of its rows holds is a gap its spacing leaves between them.
  const list = moved.row.parentElement;
  return list?.contains(drawn) === true && (inner === null || !list.contains(inner));
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
 * The bar is one line of actions that scrolls sideways rather than wrapping - an action pushed
 * onto a second line would make the bar taller under the reader's hand - and a notice, which is
 * the one thing that takes a line of its own.
 *
 * A bar that goes while it holds the keyboard focus because the reader caused it - **Clear
 * selection** pressed, Escape from a control inside it, a press on a row's mark that took the last
 * marked row away (`causedByReader`, `pressOnRowMark`) - hands the focus to what the page's
 * `returnFocus` names, rather than drop it onto the page, where a keyboard reader would start
 * again from the page's first link. So does a bar its own press takes away after the reader's
 * press on a control outside it that the press holds while its write is out took the focus from
 * it, where that control goes as well: Take out of epic's writes hold every row's Start next, and
 * a click or a tap on a marked row's one puts the focus on that Start next, on the row around it
 * or on the body, and leaves the press the cause (`repeatTookFocusFrom`). So does a bar that
 * stays while the control holding the focus goes from it: **Select all**, which the bar stops
 * offering once it has run. The focus goes to the first of these that applies, which the page's
 * own hand-off focuses (`useLastMarkedPlace`). The checkbox of the row the reader marked
 * last, while the list holds it and no press is taking it away, out of sight or not. Else a row
 * the reader can see: after a press that took that row away, the row at its place or the nearest
 * before it, each judged where it will stand once the rows the press is taking away have gone,
 * and the nearest row after that place where their going leaves the row at it wholly above the
 * view; with no row marked, the first row in sight below whatever the page draws over the list.
 * A row's checkbox is shown whole at its nearest edge, the least scroll, never centred as the
 * engine's own scroll of a focus would; one that will move up into sight once those rows have
 * gone is left to the list's anchor (`ViewportAnchor`). Else the list's own stop, with no
 * scroll. The bar lands what the page names the same way. A bar that goes while the focus is
 * elsewhere moves nothing, and neither does one whose going left the focus somewhere other than
 * the body, since the reader put it there, nor the later going of a control the reader had moved
 * the focus off, nor the going of one they had scrolled out of their sight - the bar's line of
 * actions scrolls sideways (`outOfSight`). Nor does a going the reader did not cause - a refetch
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
      if (named !== null) landWhole(named);
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

/** The bar's own box at each size. The comfortable size is the bar the Inbox draws. The compact
 *  size is the smaller box of a page whose controls are compact, in the padding the compact
 *  controls take (`controlPadding`). */
const BAR_BOX: Record<ControlSize, string> = {
  comfortable: "px-4 py-2",
  compact: "px-2 py-1",
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
      <div className="flex flex-nowrap items-center gap-2 overflow-x-auto whitespace-nowrap [&>*]:shrink-0">
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
