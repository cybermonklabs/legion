import {
  type ReactNode,
  type Ref,
  type RefObject,
  useEffect,
  useLayoutEffect,
  useRef,
} from "react";

import { focusOnDocument } from "../features/shell/roving";
import { card, dangerText, focusVisibleRing, textPrimaryOnCanvas } from "../theme/classes";
import { CONTROL } from "./control-names";
import {
  causedByReader,
  focusOwnStop,
  inSightNow,
  outOfSight,
  pressOnRowMark,
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
   *  the row the reader marked last while the list holds it and no press is taking it away, which
   *  the page names and the bar focuses and brings into view; else a row the reader can see,
   *  which the page's hand-off focuses with no scroll (`useLastMarkedPlace`), after a press that
   *  took the row marked last away or with no row marked; else the list's own stop, with no
   *  scroll (`focusListStop`). Null moves nothing here: the page has put the focus where it goes
   *  itself, or a table row the window has not drawn yet takes the focus as it lands
   *  (`DataTableModel.focusRow`). */
  readonly returnFocus?: () => HTMLElement | null;
  /** The bar's own words in place of the count, for what a count cannot say: the Inbox's
   *  `Every row selected`, which covers rows a fold or a page is holding back, and its
   *  `Snoozing 3…`, which is the press that is in the air rather than what is still marked. */
  readonly state?: ReactNode;
}

/** A list the selection bar stands over, as the bar's hand-off reads it where the row the reader
 *  marked last cannot take the focus (`useLastMarkedPlace`). */
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
  /** Whether a press of the reader's is taking row `id` away while the page still draws it. */
  readonly leaving?: (id: string) => boolean;
  /** The list's own element. It takes the focus at its own stop where no row in sight can. */
  readonly list: () => HTMLElement | null;
}

/**
 * The hand-off of the selection bar's focus where the row the reader marked last cannot take it:
 * a press took that row away, or no row is marked. The hook reads, after each commit that draws
 * the bar, the order of the list's rows while the row marked last is in it. It answers the
 * function a page's `returnFocus` calls first.
 *
 * Where the row marked last is still listed and no press is taking it away, the function moves
 * nothing and answers false: the page names that row's checkbox, which takes the focus and is
 * brought into view. Where a press took that row away, or is taking it away (`leaving`), the
 * function puts the focus, with no scroll, on the mark of the row that now stands at that row's
 * place when the reader can see it (`inSightNow`). That row is the first row after it, in the
 * order last read, that the list still holds. Else the focus goes to the mark of the nearest row
 * before that place that the reader can see. With no reading of where the row stood, the focus
 * goes to the list's own stop. Where no row is marked, the function puts the focus, with no
 * scroll, on the mark of the first row the reader can see below whatever the page draws over the
 * list (`markShown`), a row a press is taking away passed over. Where no row can take the focus,
 * in either case, it goes to the list's own stop, which is given `tabindex="-1"` where it has
 * none. The function then answers true. Nothing scrolls, so a reader deep in a long list keeps
 * their place, and the focus goes neither to the list's first row nor to the page's body.
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
    const kept = list.order().filter((id) => list.leaving?.(id) !== true);
    if (last !== null && kept.includes(last)) return false;
    // A row a press is still taking away (`leaving`) is drawn until the write's optimistic apply
    // lands, and the row after it moves up into its place only then: until then that place is
    // where the row marked last stands, read from its own mark.
    const mark =
      last === null
        ? firstMarkShown(kept, list.control)
        : markAtPlace(kept, before.current, last, list.control);
    if (mark !== null) {
      mark.focus({ preventScroll: true });
      return true;
    }
    focusListStop(list.list());
    return true;
  };
}

/** Puts the focus on a list's own stop with no scroll, as the page's rescue gives a region its
 *  own (`focusOwnStop`). It is where the selection bar's hand-off ends when no row in sight can
 *  take the focus, the emptied list included, since the page's search box takes typed text the
 *  reader never chose. Answers null, so a page's `returnFocus` can end with it and leave the bar
 *  nothing to move. */
export function focusListStop(stop: HTMLElement | null): null {
  if (stop !== null) focusOwnStop(stop);
  return null;
}

/** The mark the focus takes after the row `last` left the list: the mark of the row now at its
 *  place in `kept`, the rows the list still holds, when the reader can see that place with
 *  nothing drawn over it (`markShown`), else the mark of the nearest row before it they can see
 *  so, else null. The place is the place of the first row after `last` in `before`, the order
 *  last read with it, that `kept` holds; past the last row where none is; none where `before`
 *  does not hold `last`. While the row marked last is still drawn, the place is read from its own
 *  mark: the row after it stands below it until it goes. Measured at 390 px in Chromium: a
 *  Dismiss handed the focus on while the dismissed thread still stood in view, and the row after
 *  it, below the window, moved up into view a frame later. */
function markAtPlace(
  kept: readonly string[],
  before: readonly string[],
  last: string,
  control: (id: string) => HTMLElement | null
): HTMLElement | null {
  const from = before.indexOf(last);
  if (from === -1) return null;
  const lastMark = control(last);
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
    if (mark === null) continue;
    const seen = index === place && lastMark !== null ? lastMark : mark;
    if (markShown(seen, clips)) return mark;
  }
  return null;
}

/** The mark the focus takes when no row is marked: the mark of the first row in `kept`, the rows
 *  the list holds, that the reader can see below whatever the page draws over the list
 *  (`markShown`), else null. */
function firstMarkShown(
  kept: readonly string[],
  control: (id: string) => HTMLElement | null
): HTMLElement | null {
  const clips = new Map<Element, boolean>();
  for (const id of kept) {
    const mark = control(id);
    if (mark !== null && markShown(mark, clips)) return mark;
  }
  return null;
}

/** Whether the reader can see the row mark `mark` now with nothing drawn over it. Its box must
 *  be in sight (`inSightNow`), and its centre inside the window. What the page draws at that
 *  centre must be the mark or a part of its row (`ROW_SELECTOR`). A list's sticky head, or other
 *  chrome the rows scroll under, can stand over a row whose box is in sight, and is what the
 *  page draws there then. A mark with its centre out of the window shows half of itself at most,
 *  and one whose edge only touches the window's edge shows none of itself. Where the engine names
 *  no element at a point inside the window (happy-dom lays nothing out), the box and the centre
 *  decide alone. */
function markShown(mark: HTMLElement, clips: Map<Element, boolean>): boolean {
  if (!inSightNow(mark, clips)) return false;
  const box = mark.getBoundingClientRect();
  const x = box.left + box.width / 2;
  const y = box.top + box.height / 2;
  if (x < 0 || y < 0 || x >= window.innerWidth || y >= window.innerHeight) return false;
  const drawn = document.elementFromPoint(x, y);
  return drawn === null || (mark.closest(ROW_SELECTOR) ?? mark).contains(drawn);
}

/**
 * The one bar a page shows while rows are marked: how many, what one press does to all of them,
 * and the way out.
 *
 * It is `fixed`, over the list rather than in it, which is what keeps the height rule
 * (INFRA-3135, R1): selecting a row must leave the top of the list's first row where it was, and
 * a bar drawn in the page would push every row down the moment the first one was marked. It sits
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
 * again from the page's first link. So does a bar that stays while the control holding the focus
 * goes from it: **Select all**, which the bar stops offering once it has run. The focus goes to
 * the first of these that applies. The checkbox of the row the reader marked last, while the list
 * holds it and no press is taking it away: the page names it, and the bar focuses it and brings it
 * into view. Else a row the reader can see, which the page's own hand-off focuses with no scroll
 * (`useLastMarkedPlace`): after a press that took that row away, the row at its place or the
 * nearest before it; with no row marked, the first row in sight below whatever the page draws
 * over the list. Else the list's own stop, with no scroll. A bar that goes while the focus is
 * elsewhere moves nothing, and neither does one whose going left the focus somewhere other than
 * the body, since the reader put it there, nor the later going of a control the reader had moved
 * the focus off, nor the going of one they had scrolled out of their sight - the bar's line of
 * actions scrolls sideways (`outOfSight`). A going the reader did not cause - a refetch that
 * drops every marked row and empties the bar - the bar leaves to the page's focus rescue, which
 * lands it in no row and scrolls nothing.
 */
export function SelectionBar({ returnFocus, ...bar }: SelectionBarProps): ReactNode {
  // Set by the drawn bar to what held the focus when the focus dropped with it - the bar's
  // element, or a control in it, as the bar goes, or the focused control inside a bar that stays -
  // and read after the commit that took it away.
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
      returnFocus?.()?.focus();
    }
  });
  if (bar.count === 0 && bar.state === undefined) return null;
  return <DrawnBar {...bar} wentHolding={wentHolding} />;
}

/** The bar itself, mounted exactly while it is drawn, so its going is its own unmount. */
function DrawnBar({
  barRef,
  children,
  count,
  label,
  notice,
  state,
  wentHolding,
}: Omit<SelectionBarProps, "returnFocus"> & {
  /** Set to what held the focus when the bar, or the control inside it that held the focus, goes
   *  with it. */
  readonly wentHolding: RefObject<Element | null>;
}): ReactNode {
  const ownRef = useRef<HTMLElement>(null);
  const ref = barRef ?? ownRef;
  // What inside the bar took the focus last, the bar's own element included, until the reader
  // moves the focus off it: a focus event bubbles up to the bar from every control in it.
  const focused = useRef<Element | null>(null);
  // The cleanup runs before React takes the element out of the page, the one moment the focus
  // can still be read inside it: the removal itself drops it onto the body.
  useLayoutEffect(() => {
    const node = ref.current;
    return () => {
      if (node?.contains(document.activeElement) === true)
        wentHolding.current = document.activeElement;
    };
  }, [ref, wentHolding]);
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
      className={`fixed inset-x-2 top-14 z-30 mx-auto flex w-fit max-w-[calc(100vw-1rem)] flex-col gap-2 rounded-2xl border px-4 py-2 text-sm shadow-xl outline-none focus-visible:ring-2 sm:top-auto sm:bottom-4 ${card} ${textPrimaryOnCanvas} ${focusVisibleRing}`}
      data-control={CONTROL.selectionBar}
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
