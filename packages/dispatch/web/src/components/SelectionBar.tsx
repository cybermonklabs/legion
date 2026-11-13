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
  /** What takes the keyboard focus when the bar goes while it holds it, asked once the commit
   *  that took the bar away is in the page: the page's own row, never the body. */
  readonly returnFocus?: () => HTMLElement | null;
  /** The bar's own words in place of the count, for what a count cannot say: the Inbox's
   *  `Every row selected`, which covers rows a fold or a page is holding back, and its
   *  `Snoozing 3…`, which is the press that is in the air rather than what is still marked. */
  readonly state?: ReactNode;
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
 * A bar that goes while it holds the keyboard focus - **Clear selection** pressed, Escape from a
 * control inside it, a press that took every marked row away - hands the focus to what the page's
 * `returnFocus` names, rather than drop it onto the page, where a keyboard reader would start
 * again from the page's first link. The hand-off is the bar's alone, so no page writes one: the
 * page only says where. A bar that goes while the focus is elsewhere moves nothing, and neither
 * does one whose going left the focus somewhere other than the body, since the reader put it
 * there.
 */
export function SelectionBar({ returnFocus, ...bar }: SelectionBarProps): ReactNode {
  // Set by the drawn bar as it goes, while its element still holds the focus, and read after the
  // commit that took it away.
  const wentHolding = useRef(false);
  // After every commit, so after the one that removed the bar's element and dropped the focus
  // with it. No dependency list: the `returnFocus` called is this commit's own, so a page that
  // hands a new function on every render starts nothing by it. Passive rather than layout: the
  // Inbox's `ViewportAnchor` keeps its rows in place from its `componentDidUpdate`, which runs
  // in the layout phase after this bar's own layout effects and would scroll the list back over
  // the target that focusing brought into view.
  useEffect(() => {
    if (!wentHolding.current) return;
    wentHolding.current = false;
    if (focusOnDocument()) returnFocus?.()?.focus();
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
  /** Set when the bar goes with the focus inside it. */
  readonly wentHolding: RefObject<boolean>;
}): ReactNode {
  const ownRef = useRef<HTMLElement>(null);
  const ref = barRef ?? ownRef;
  // The cleanup runs before React takes the element out of the page, the one moment the focus
  // can still be read inside it: the removal itself drops it onto the body.
  useLayoutEffect(() => {
    const node = ref.current;
    return () => {
      if (node?.contains(document.activeElement) === true) wentHolding.current = true;
    };
  }, [ref, wentHolding]);
  return (
    <div
      aria-label={label}
      className={`fixed inset-x-2 top-14 z-30 mx-auto flex w-fit max-w-[calc(100vw-1rem)] flex-col gap-2 rounded-2xl border px-4 py-2 text-sm shadow-xl outline-none focus-visible:ring-2 sm:top-auto sm:bottom-4 ${card} ${textPrimaryOnCanvas} ${focusVisibleRing}`}
      data-control={CONTROL.selectionBar}
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
