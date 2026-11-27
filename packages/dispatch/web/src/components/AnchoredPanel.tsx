import {
  type CSSProperties,
  createContext,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

import { hearPresses } from "../features/issue-page/focus-keeper";
import { PANEL_ATTRIBUTE } from "../features/shell/keymap";
import { inputMethodKey } from "../lib/input-method-key";
import { CLIPPING_OVERFLOW, focusableIn } from "./focus-place";

/** The panel's widest, before the 8 px margin it keeps from each viewport edge. */
const PANEL_WIDTH = 256;

/**
 * The layer every panel stands in, the one place it is set: an open panel is the reader's
 * topmost layer, over whatever it was opened over - the page, a dialog (`z-40` and `z-50`), the
 * Inbox drawer, a toast, a reference's hover card (`z-[60]`) and a document's own floating bars,
 * the selection bar with its Comment and Suggest and the mark popovers `@legion/proof-editor`
 * draws at z-index 190 to 280 - so a reader with text selected in a document who opens its
 * Version list presses its options rather than the bar drawn over them. Only the browser's top
 * layer is above it, a modal `<dialog>` (the picture lightbox), which no panel is opened over. A
 * panel opened inside another stands in the same layer, after it in the body, so over it. Any
 * press outside a panel closes it, so nothing it covers is left under it for long.
 */
const PANEL_LAYER = 300;

/** Counts `panel`, opened from inside the panel that hands this out, as part of that panel for
 *  as long as it stands open, and of every panel around that one. Answers what stops counting it. */
type HoldPanel = (panel: HTMLElement) => () => void;

/** What the panel a panel is opened inside hands its content: a filter panel's multi-selects,
 *  the search bar's Project and Status lists. Null outside every panel. */
const EnclosingPanel = createContext<HoldPanel | null>(null);

/** A Tab stop of the panel's own on either side of it, drawn nowhere and pressed by nothing. */
const EDGE: CSSProperties = {
  opacity: 0,
  outline: "none",
  pointerEvents: "none",
  position: "fixed",
};

/** The keymap's mark on the panel's element: a key pressed inside it is the panel's own
 *  (`PANEL_ATTRIBUTE`). */
const PANEL_MARK = { [PANEL_ATTRIBUTE]: "" };

/** Where the panel stands in the window, and the height it is cut to where it is cut. */
interface Placement {
  readonly left: number;
  readonly maxHeight?: number;
  readonly top: number;
}

/** A top and a bottom, in the window. */
interface Span {
  readonly bottom: number;
  readonly top: number;
}

/** The top and bottom, in the window, of what `box` shows of what it holds: inside its border
 *  and above a horizontal scrollbar. */
export function shownSpan(box: Element): Span {
  const top = box.getBoundingClientRect().top + box.clientTop;
  return { bottom: top + box.clientHeight, top };
}

/** Scrolls `box` the least that shows `span` whole in `shown`, the part of the window the box
 *  shows what it holds in: up to `span`'s top where it starts above `shown`, down to its bottom
 *  where it ends below, and not at all where it stands whole. It sets the box's own `scrollTop`
 *  rather than call `scrollIntoView`, which scrolls the window too. A panel's rule for the
 *  control the focus reaches in it (`scrollPanelToShow`) and the shared list's for the option
 *  it opens on or makes active by arrow keys or by an update (`MultiSelect`'s `showOption`,
 *  including `showOption(activeRow)`) all end here. */
export function scrollToShow(box: Element, span: Span, shown: Span): void {
  if (span.top < shown.top) box.scrollTop -= shown.top - span.top;
  else if (span.bottom > shown.bottom) box.scrollTop += span.bottom - shown.bottom;
}

/** The side of its panel `child` sticks to, `top` or `bottom`, as the shared list's text box and
 *  Clear foot do, or null for a child that sticks to none. A sticky box sticks to a side whose
 *  inset is set: `auto` in a browser, empty in happy-dom, is no side. */
function stuckSide(child: Element): "bottom" | "top" | null {
  const { bottom, position, top } = getComputedStyle(child);
  if (position !== "sticky") return null;
  if (top !== "auto" && top !== "") return "top";
  return bottom !== "auto" && bottom !== "" ? "bottom" : null;
}

/** The part of `panel`'s shown span (`shownSpan`) that no sticky child of the panel covers: cut
 *  at the bottom edge of a child stuck to its top and at the top edge of one stuck to its bottom,
 *  as the shared list's text box and Clear foot are, the rows scrolling under both (`MultiSelect`,
 *  whose arrows measure the same span). A sticky child that holds `control` cuts nothing: it
 *  stands where it sticks however the panel scrolls. */
function uncoveredSpan(panel: Element, control: Element): Span {
  let { bottom, top } = shownSpan(panel);
  for (const child of panel.children) {
    if (child.contains(control)) continue;
    const side = stuckSide(child);
    if (side === null) continue;
    const box = child.getBoundingClientRect();
    if (side === "top") top = Math.max(top, box.bottom);
    else bottom = Math.min(bottom, box.top);
  }
  return { bottom, top };
}

/**
 * Scrolls `panel` the least that shows `control`, which has just taken the focus inside it, whole
 * in the part of the panel's box that no sticky child of the panel covers (`uncoveredSpan`): up
 * to that part's top edge where the control starts above it, down to its bottom edge where it ends
 * below, and not at all where it stands whole. Chromium and WebKit scroll a control the focus
 * reaches into view themselves; Firefox leaves one that is partly in sight cut at the box's edge
 * (Remove Waiting on you filter in the Inbox's `+N more` panel, 19 px below it at 390x664). A row
 * of the shared list that takes the focus under its text box or its Clear foot, as a screen
 * reader's cursor puts it there, stands covered in every engine, since each counts the whole box
 * as in view. Only the panel's box scrolls: this sets its `scrollTop` rather than call
 * `scrollIntoView`, which scrolls the window too (`scrollToShow`).
 *
 * A control inside a box of its own that cuts it - a box inside the panel whose `overflow-y` clips
 * (`CLIPPING_OVERFLOW`), as a scroll box's does - is shown as far as that box shows it: what the
 * box cuts off is drawn nowhere, so no scroll of the panel can show it, and the panel scrolls the
 * least that shows the drawn part whole. A control that box shows none of scrolls the panel
 * nothing: no scroll of the panel shows it until the box itself scrolls to it.
 */
function scrollPanelToShow(panel: HTMLElement, control: Element): void {
  let { bottom, top } = control.getBoundingClientRect();
  for (let box = control.parentElement; box !== null && box !== panel; box = box.parentElement) {
    if (CLIPPING_OVERFLOW[getComputedStyle(box).overflowY] !== true) continue;
    const cut = shownSpan(box);
    top = Math.max(top, cut.top);
    bottom = Math.min(bottom, cut.bottom);
  }
  if (top > bottom) return;
  scrollToShow(panel, { bottom, top }, uncoveredSpan(panel, control));
}

/**
 * The least height `panel` is cut to where the window has too little room for it: its border and
 * padding, each child stuck to its top or its bottom (`stuckSide`), and one row, the first control
 * it holds outside those children. In the shared list that is its text box, its first option and
 * its Clear foot while the foot is drawn: cut to the room alone, the issue header's Labels list at
 * 390x360 showed no option whole between its text box and its foot, which left 22 px for the
 * rows. A panel counts only the parts it has: a filter panel, which has no sticky child, is never
 * cut below its first control, and a list that shows no option is never cut below its text box
 * and its foot. Each part is read as it is drawn.
 */
function leastHeight(panel: HTMLElement): number {
  const { paddingBottom, paddingTop } = getComputedStyle(panel);
  let least =
    panel.offsetHeight -
    panel.clientHeight +
    (Number.parseFloat(paddingTop) || 0) +
    (Number.parseFloat(paddingBottom) || 0);
  const stuck: Element[] = [];
  for (const child of panel.children) {
    if (stuckSide(child) === null) continue;
    stuck.push(child);
    least += child.getBoundingClientRect().height;
  }
  const row = focusableIn(panel).find((control) => !stuck.some((child) => child.contains(control)));
  return least + (row?.getBoundingClientRect().height ?? 0);
}

/**
 * Scrolls what holds `anchor` the least that moves it `by` px up the window, or down for a `by`
 * below 0: each box around it whose overflow scrolls, nearest first, and then the page. Each box
 * moves it no further than the box's own scroll reaches, and than keeps the anchor whole in the
 * part of the box no sticky child of the box covers (`uncoveredSpan`) - a drawer's head, say - so
 * the control that opened a panel is still there to press. The walk stops at the anchor or a box
 * around it that is `fixed` or `sticky`: a scroll of what holds that box need not move the anchor,
 * and would move the page under a panel that stays where it stood.
 */
function moveAnchor(anchor: Element, by: number): void {
  let left = by;
  for (let box: Element | null = anchor; box !== null && left !== 0; box = box.parentElement) {
    const { overflowY, position } = getComputedStyle(box);
    const page = box === document.scrollingElement;
    if (box !== anchor && (page || overflowY === "auto" || overflowY === "scroll")) {
      const shown = page ? { bottom: window.innerHeight, top: 0 } : uncoveredSpan(box, anchor);
      const at = anchor.getBoundingClientRect();
      const step =
        left > 0
          ? Math.min(left, box.scrollHeight - box.clientHeight - box.scrollTop, at.top - shown.top)
          : Math.max(left, -box.scrollTop, at.bottom - shown.bottom);
      if (Math.sign(step) === Math.sign(left)) {
        box.scrollTop += step;
        left -= at.top - anchor.getBoundingClientRect().top;
      }
    }
    if (position === "fixed" || position === "sticky") return;
  }
}

/** What dismissed a panel, which `onDismiss` hands its host: Escape, Tab or Shift+Tab past its
 *  last or first control, a pointer press outside it and its anchor, or its anchor gone from the
 *  page or drawn nowhere. A host whose next move depends on how the reader left - the Agents
 *  composer's Issue list, whose Escape is one level out to the row - reads it; any other host
 *  ignores it. */
export type PanelDismissal = "escape" | "gone" | "press" | "tab";

/**
 * The one panel that hangs off a control: portalled to the body so no ancestor can clip it,
 * placed under its anchor - over it where the window has no room under it, and cut to the room
 * on the roomier side where it has room on neither, so it never covers its anchor - and kept
 * there while the page resizes or scrolls, and dismissed by the same rules everywhere — a
 * pointer press outside it or its anchor, Escape, or Tab or Shift+Tab past its last or first
 * control, the last three of which also return focus to the anchor. Opening moves focus to the
 * panel's first control that takes it, by the rule its edges and every dialog's trap follow
 * (`focusableIn`), else to the panel itself, unless the host opens it without (`focusOnOpen`).
 *
 * A cut panel is never cut below its least height (`leastHeight`): the shared list's text box,
 * one option and its Clear foot. Where the roomier side has less room than that, the panel takes
 * that side at its least height and the page scrolls the least that shows it within the window
 * (`moveAnchor`); where the page cannot scroll so far, it is cut to the room it has then, so it
 * never draws past the window's edge. The page scrolls so as the panel opens, never against a
 * later scroll: once a scroll has been answered, a panel short of room is cut to the room it has.
 *
 * The press outside is read at the document's capture phase, before any control under it can
 * stop it on its way up: an ask card's hand-on keeps its press from the row it sits in, and a
 * press there still closes every other panel, another card's hand-on list among them.
 *
 * Tab never leaves the panel for whatever the body holds after it: portalled there, the panel is
 * outside the dialog it was opened in, whose own Tab cycle never sees it, so the focus would go
 * past the end of the page and on to the page behind the dialog. Closing at either end and
 * handing the focus back to the anchor puts the reader back in the cycle they came from, as
 * `MultiSelect`'s search box always has. The ends are the panel's own two Tab stops, one before
 * it and one after, so the browser's own Tab order says when the focus has gone past the last
 * control - past a radio group's chosen radio, past the last field of a date box - and a stop
 * reached from the page rather than from the panel hands the focus on into it instead.
 *
 * A panel opened from inside this one - a filter panel's multi-select - is portalled out of it
 * too, but a press there is no press outside this panel: the reader picking a value is still in
 * the panel they picked it from. Escape and Tab stay each panel's own, so Escape in the inner
 * list closes that list alone.
 *
 * A control the focus reaches inside the panel stands whole in the part of the panel's box no
 * sticky child of the panel covers, which scrolls the least that shows it, in every engine
 * (`scrollPanelToShow`); a focus in a panel opened from inside this one is that panel's own, and
 * scrolls this one nothing.
 *
 * The panel's element carries the keymap's `PANEL_ATTRIBUTE`, so a key pressed anywhere inside
 * it fires only the page's bindings that fire in a text field (`inEditable`): a radio or a
 * checkbox here stands where a select used to, and `j` on it must not walk the table behind.
 *
 * The host owns whether it is open: it renders the panel or it does not. Every popover in
 * Dispatch is built on this, so a dismissal fix is made once (`MultiSelect`, and the issues
 * table's column filters, Columns menu and epic actions).
 */
export function AnchoredPanel({
  align = "left",
  anchorRef,
  children,
  className,
  focusOnOpen = true,
  id,
  label,
  onDismiss,
  overflowAnchor,
  role,
}: {
  align?: "left" | "right";
  anchorRef: RefObject<HTMLElement | null>;
  children: ReactNode;
  className: string;
  /** Whether opening moves the focus into the panel. Read once, as the panel opens. */
  focusOnOpen?: boolean;
  /** The panel element's id, for an anchor that names what it opened (`aria-controls`). */
  id?: string;
  /** The panel's accessible name, for a panel that names itself. */
  label?: string;
  onDismiss: (how: PanelDismissal) => void;
  /** The browser's scroll anchoring of the panel box itself. Defaults to the browser's own. */
  overflowAnchor?: CSSProperties["overflowAnchor"];
  role?: "dialog";
}): ReactNode {
  const panelRef = useRef<HTMLDivElement>(null);
  const [position, setPosition] = useState<Placement>({ left: 0, top: 0 });
  const dismiss = useRef(onDismiss);
  dismiss.current = onDismiss;
  const takesFocus = useRef(focusOnOpen);
  // Whether the focus was last on the panel or one of its own controls: what tells an edge the
  // reader reached by Tab out of the panel from one they reached by Tab into it.
  const inside = useRef(false);
  const startEdge = useRef<HTMLSpanElement>(null);
  const endEdge = useRef<HTMLSpanElement>(null);
  // The panels opened from inside this one that stand open now, each counted by `hold`.
  const [inner] = useState(() => new Set<HTMLElement>());
  // The tallest the panel's own class lets it be, read whenever no cut of this placement's is on
  // it: what its height is capped at, cut or not.
  const classMax = useRef(Number.POSITIVE_INFINITY);
  // Whether a scroll outside the panel has been answered since it opened: the reader's, or the
  // one its own placement made to give it its least height. From then on, for as long as this
  // panel stays open, no placement scrolls the page or a box around the anchor (`moveAnchor`), so
  // no scroll of the reader's is undone: not by the placement that answers it, nor by the one
  // after the render that placement makes. A panel opened again starts with none answered.
  const scrolled = useRef(false);
  const enclosing = useContext(EnclosingPanel);
  const hold = useCallback<HoldPanel>(
    (panel) => {
      inner.add(panel);
      const release = enclosing?.(panel);
      return () => {
        inner.delete(panel);
        release?.();
      };
    },
    [enclosing, inner]
  );
  useEffect(() => {
    const panel = panelRef.current;
    if (panel === null || enclosing === null) return;
    return enclosing(panel);
  }, [enclosing]);

  // Placed after every render - the anchor moves when its own row reflows - and on resize and
  // scroll, which is when the viewport coordinates it is placed at change. A scroll inside the
  // panel, or inside a panel opened from it, moves neither the panel nor its anchor, so it
  // places nothing.
  useLayoutEffect(() => {
    const place = () => {
      const anchor = anchorRef.current;
      const box = anchor?.getBoundingClientRect();
      // A panel hanging off nothing is one the reader cannot place: when its anchor has left
      // the page, or is drawn nowhere while the page around it is drawn — a row the virtualizer
      // took away, a control a narrow window folded — the panel closes rather than standing
      // where the anchor used to be. The second half is asked only of an environment that lays
      // anything out at all, so a test DOM with no layout keeps its panels.
      if (anchor === null || anchor === undefined || !anchor.isConnected) {
        dismiss.current("gone");
        return;
      }
      if (box === undefined) return;
      const drawn = panelRef.current?.getBoundingClientRect();
      if (box.width === 0 && box.height === 0 && drawn !== undefined && drawn.height > 0) {
        dismiss.current("gone");
        return;
      }
      const panel = panelRef.current;
      const width = Math.min(panel?.offsetWidth || PANEL_WIDTH, window.innerWidth - 16);
      const left = Math.max(
        8,
        Math.min(align === "right" ? box.right - width : box.left, window.innerWidth - width - 8)
      );
      // The panel's height as its own class allows it, so the side it goes on is decided by what
      // it holds rather than by the last cut: all it holds (`scrollHeight`) with its border, up
      // to the class's own cap. Read without touching the panel: a layout forced at another
      // height would clamp the panel's own scroll to that layout's, so a cut panel could never
      // be scrolled to its end.
      if (panel && panel.style.maxHeight === "") {
        const cap = Number.parseFloat(getComputedStyle(panel).maxHeight);
        classMax.current = Number.isNaN(cap) ? Number.POSITIVE_INFINITY : cap;
      }
      const height = panel
        ? Math.min(panel.scrollHeight + panel.offsetHeight - panel.clientHeight, classMax.current)
        : 200;
      // Under the anchor where it fits, else over it where it fits there, else on the side with
      // more room and cut to that room, though never below `floor` - never over the anchor
      // itself, so the control that opened a panel is always there to press again and close it:
      // a list low in a filter panel, or a filter panel under a trigger low on a phone, goes up
      // rather than over it. `short` is how far `floor` runs past that room: past the window's
      // bottom margin as a count over 0, past its top margin as one under 0.
      const placed = (at: DOMRect, floor: number): { short: number; spot: Placement } => {
        const below = window.innerHeight - 8 - (at.bottom + 4);
        const above = at.top - 4 - 8;
        if (height <= below) return { short: 0, spot: { left, top: Math.max(8, at.bottom + 4) } };
        if (below >= above) {
          const room = Math.max(below, 0);
          const maxHeight = Math.max(room, floor);
          const top = Math.max(8, at.bottom + 4);
          return { short: maxHeight - room, spot: { left, maxHeight, top } };
        }
        if (height <= above) return { short: 0, spot: { left, top: at.top - 4 - height } };
        const maxHeight = Math.max(above, floor);
        return { short: above - maxHeight, spot: { left, maxHeight, top: at.top - 4 - maxHeight } };
      };
      let next = placed(box, Number.NEGATIVE_INFINITY).spot;
      // A cut panel is cut no lower than its least height (`leastHeight`), or than all it holds
      // where it holds less. Where the room on its side is less than that, the page scrolls the
      // least that gives the panel that room (`moveAnchor`). Where it cannot scroll so far, the
      // panel is cut to the room it has then, so it never draws past the window's edge. Once a
      // scroll has been answered (`scrolled`), nothing scrolls and the panel is cut to the room
      // it has, as with no least height: where that room holds the least height, it cuts the
      // panel no differently.
      if (panel && next.maxHeight !== undefined && !scrolled.current) {
        const least = Math.min(leastHeight(panel), height);
        let short: number;
        ({ short, spot: next } = placed(box, least));
        if (short !== 0) {
          moveAnchor(anchor, short);
          const moved = anchor.getBoundingClientRect();
          ({ short, spot: next } = placed(moved, least));
          if (short !== 0) next = placed(moved, Number.NEGATIVE_INFINITY).spot;
        }
      }
      setPosition((current) =>
        current.left === next.left &&
        current.top === next.top &&
        current.maxHeight === next.maxHeight
          ? current
          : next
      );
    };
    const placeOnScroll = (event: Event) => {
      const target = event.target;
      if (target instanceof Node) {
        if (panelRef.current?.contains(target)) return;
        for (const held of inner) if (held.contains(target)) return;
      }
      scrolled.current = true;
      place();
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", placeOnScroll, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", placeOnScroll, true);
    };
  });

  useEffect(() => {
    const panel = panelRef.current;
    if (takesFocus.current && panel !== null && !panel.contains(document.activeElement))
      (focusableIn(panel)[0] ?? panel).focus();
    inside.current = panel?.contains(document.activeElement) === true;
    // Every move of the focus: onto one of the panel's two edges, which is answered here, or
    // anywhere else, which is noted, and which, on a control of the panel's own, shows that
    // control whole in the panel's box (`scrollPanelToShow`) - unless a pointer press in the panel
    // is down (`pressing`). That is read here rather than by a focus handler on the panel's
    // element: React hands that handler the focus of a panel opened from inside this one too, as
    // it carries an event through a portal along its own tree, while the page tells them apart,
    // since the inner panel stands outside this one in the body.
    //
    // A pointer press focuses the control under it as the pointer goes down, in Chromium, Firefox
    // and WebKit alike. A scroll then moves the control under the pointer before it comes up: on a
    // chip of the Inbox's +N more panel cut 20 px by the panel's bottom at 390x664, the panel
    // scrolled 24 px between the pointer's down and up in all three (builds Job
    // infra-3135-probe1-8lssx). The reader pressed what they saw, so while a press in the panel is
    // down no focus scrolls the panel, the press's own focus included. A focus by a key, or one a
    // script hands on once the press is over - a press's own hand-off in the click that the
    // pointer's up makes included - still shows its control whole, since the reader cannot see
    // where it went. A press is the one `hearPresses` reads
    // (`features/issue-page/focus-keeper.ts`), from its `pointerdown` in the panel: it ends at the
    // `pointerup` once the press has moved the focus, or where it is of a button other than the
    // primary one, which sends no click; else at its click, before the click's own handlers, so a
    // touch, which focuses its control at the `mousedown` the browser makes after the finger has
    // gone up, is still down then; and a press that sends no release ends at its `pointercancel`,
    // its `dragend`, its `contextmenu` or the blur of the page's window.
    let pressing = false;
    const followFocus = (event: FocusEvent) => {
      const target = event.target;
      const edge =
        target === startEdge.current ? "start" : target === endEdge.current ? "end" : undefined;
      const open = panelRef.current;
      if (edge === undefined) {
        const own = open !== null && target instanceof Element && open.contains(target);
        inside.current = own;
        if (own && target !== open && !pressing) scrollPanelToShow(open, target);
        return;
      }
      if (open === null) return;
      // From the panel, by Tab past its last control or Shift+Tab past its first, the reader is
      // leaving it: it closes, and they are back on the anchor.
      if (inside.current) {
        anchorRef.current?.focus();
        dismiss.current("tab");
        return;
      }
      // From the page before the panel, or from what the body draws after it, they are coming
      // in: on to the panel's first control, or its last.
      const controls = focusableIn(open);
      ((edge === "start" ? controls[0] : controls.at(-1)) ?? open).focus();
    };
    // Read in the capture phase, so a control that stops its own press from propagating - to keep
    // it from the row or card under it - cannot keep this panel open.
    const outside = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (panelRef.current?.contains(target)) {
        // A press on the panel's own prose focuses nothing, which would leave focus on the
        // body and Escape with no owner. The panel takes it, so the reader stays in the panel.
        if (
          !(document.activeElement instanceof HTMLElement) ||
          !panelRef.current.contains(document.activeElement)
        )
          panelRef.current.focus();
        return;
      }
      if (anchorRef.current?.contains(target)) return;
      for (const held of inner) if (held.contains(target)) return;
      dismiss.current("press");
    };
    const dismissOnEscape = (event: KeyboardEvent) => {
      // An Escape that ends an IME composition is the input method's (`inputMethodKey`): it
      // cancels what the reader was composing in the panel's text box, and the panel stays open.
      if (event.key !== "Escape" || inputMethodKey(event)) return;
      // Escape belongs to whatever the reader is in. This panel takes it only while the press
      // comes from inside the panel or from its anchor; a press in the command bar opened over
      // the panel, or in any other layer, travels on to whoever owns that layer. The listener
      // is still at the document's capture phase, so a panel inside a dialog closes itself and
      // leaves the dialog open.
      const target = event.target;
      const mine =
        target instanceof Node &&
        (panelRef.current?.contains(target) === true ||
          anchorRef.current?.contains(target) === true);
      if (!mine) return;
      event.preventDefault();
      event.stopPropagation();
      anchorRef.current?.focus();
      dismiss.current("escape");
    };
    document.addEventListener("focusin", followFocus, true);
    document.addEventListener("pointerdown", outside, true);
    const stopPresses = hearPresses(
      document,
      (event) => {
        pressing =
          event.target instanceof Node && panelRef.current?.contains(event.target) === true;
      },
      () => {
        pressing = false;
      }
    );
    document.addEventListener("keydown", dismissOnEscape, true);
    return () => {
      document.removeEventListener("focusin", followFocus, true);
      document.removeEventListener("pointerdown", outside, true);
      stopPresses();
      document.removeEventListener("keydown", dismissOnEscape, true);
    };
  }, [anchorRef, inner]);

  const named = role === undefined ? {} : { role, "aria-label": label };
  return createPortal(
    <>
      {/* biome-ignore lint/a11y/noNoninteractiveTabindex: the panel's own Tab stop before it, which no reader rests on: its focus is answered at once (`followFocus`). */}
      <span ref={startEdge} style={EDGE} tabIndex={0} />
      <div
        className={className}
        id={id}
        ref={panelRef}
        style={{ overflowAnchor, position: "fixed", zIndex: PANEL_LAYER, ...position }}
        tabIndex={-1}
        {...named}
        {...PANEL_MARK}
      >
        <EnclosingPanel.Provider value={hold}>{children}</EnclosingPanel.Provider>
      </div>
      {/* biome-ignore lint/a11y/noNoninteractiveTabindex: the panel's own Tab stop after it, which no reader rests on: its focus is answered at once (`followFocus`). */}
      <span ref={endEdge} style={EDGE} tabIndex={0} />
    </>,
    document.body
  );
}
