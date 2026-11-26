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
import { focusBack } from "../features/shell/roving";
import { inputMethodKey } from "../lib/input-method-key";
import { CLIPPING_OVERFLOW, focusableIn } from "./focus-place";
import { PANEL_ATTRIBUTE } from "./key-targets";

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
 *  its highlight marks, whatever moved the highlight, and the one an open keeps shown while its
 *  rows and its box change (`MultiSelect`'s `showRow`), both end here. */
export function scrollToShow(box: Element, span: Span, shown: Span): void {
  if (span.top < shown.top) box.scrollTop -= shown.top - span.top;
  else if (span.bottom > shown.bottom) box.scrollTop += span.bottom - shown.bottom;
}

/** The part of `panel`'s shown span (`shownSpan`) that no sticky child of the panel covers: cut
 *  at the bottom edge of a child stuck to its top and at the top edge of one stuck to its bottom,
 *  as the shared list's text box and Clear foot are, the rows scrolling under both (`MultiSelect`,
 *  whose arrows measure the same span). A sticky child that holds `control` cuts nothing: it
 *  stands where it sticks however the panel scrolls. */
function uncoveredSpan(panel: HTMLElement, control: Element): Span {
  let { bottom, top } = shownSpan(panel);
  for (const child of panel.children) {
    if (child.contains(control)) continue;
    const { bottom: stuckBottom, position, top: stuckTop } = getComputedStyle(child);
    if (position !== "sticky") continue;
    const box = child.getBoundingClientRect();
    // A sticky box sticks to a side whose inset is set: `auto` in a browser, empty in happy-dom,
    // is no side.
    if (stuckTop !== "auto" && stuckTop !== "") top = Math.max(top, box.bottom);
    else if (stuckBottom !== "auto" && stuckBottom !== "") bottom = Math.min(bottom, box.top);
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

/** What dismissed a panel, which `onDismiss` hands its host: Escape, Tab or Shift+Tab past its
 *  last or first control, a pointer press outside it and its anchor, or its anchor gone from the
 *  page or drawn nowhere. A host whose next move depends on how the reader left - the Agents
 *  composer's Issue list, whose Escape is one level out to the row - reads it; any other host
 *  ignores it. */
export type PanelDismissal = "escape" | "gone" | "press" | "tab";

/** Where the focus was when a key opened a panel from its anchor, by anchor: written by
 *  `noteOpenedFrom`, read by Escape in the panel that opens, and forgotten as that panel closes. */
const openedFrom = new WeakMap<HTMLElement, HTMLElement>();

/**
 * Notes, for the panel `anchor` is about to open, the element that has the focus now, so that
 * Escape in that panel hands the focus back there rather than to the anchor: a key that presses
 * a panel's trigger from a row leaves the reader on that row when they back out, and one pressed
 * with nothing focused leaves them on the page itself, where a press on the trigger itself
 * leaves them on the trigger. Called by the key's binding just before it presses the anchor, or
 * moves the focus into the panel the anchor already holds open; the note holds until that panel
 * closes, however it closes.
 */
export function noteOpenedFrom(anchor: HTMLElement): void {
  const focused = document.activeElement;
  if (focused instanceof HTMLElement) openedFrom.set(anchor, focused);
  else openedFrom.delete(anchor);
}

/**
 * The one panel that hangs off a control: portalled to the body so no ancestor can clip it,
 * placed under its anchor - over it where the window has no room under it, and cut to the room
 * on the roomier side where it has room on neither, so it never covers its anchor - and kept
 * there while the page resizes or scrolls, and dismissed by the same rules everywhere — a
 * pointer press outside it or its anchor, Escape, or Tab or Shift+Tab past its last or first
 * control, the last three of which also return focus to the anchor (or to where the host says,
 * `returnFocus`, or for Escape to where a key opened it from, `noteOpenedFrom`). Opening moves
 * focus to the panel's first control that takes it, by the rule its edges and every dialog's
 * trap follow (`focusableIn`), else to the panel itself, unless the host opens it without
 * (`focusOnOpen`).
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
 * The panel's element carries `PANEL_ATTRIBUTE` (`key-targets.ts`), so a key pressed anywhere inside
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
  returnFocus,
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
  /** Where Escape and Tab out of the panel put the focus, read as they do: the anchor where
   *  this answers null or is not given. A menu answers the element that had the focus before
   *  it opened (`ChoiceMenu`, and a single pick in a menu's place, `MultiSelect`'s `menu`). */
  returnFocus?: () => HTMLElement | null;
  role?: "dialog" | "menu";
}): ReactNode {
  const panelRef = useRef<HTMLDivElement>(null);
  const [position, setPosition] = useState<{ left: number; maxHeight?: number; top: number }>({
    left: 0,
    top: 0,
  });
  const dismiss = useRef(onDismiss);
  dismiss.current = onDismiss;
  const back = useRef(returnFocus);
  back.current = returnFocus;
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
      // more room and cut to that room - never over the anchor itself, so the control that
      // opened a panel is always there to press again and close it: a list low in a filter
      // panel, or a filter panel under a trigger low on a phone, goes up rather than over it.
      const below = window.innerHeight - 8 - (box.bottom + 4);
      const above = box.top - 4 - 8;
      let next: { left: number; maxHeight?: number; top: number };
      if (height <= below || below >= above) {
        const maxHeight = height <= below ? undefined : Math.max(below, 0);
        next = { left, maxHeight, top: Math.max(8, box.bottom + 4) };
      } else if (height <= above) {
        next = { left, top: box.top - 4 - height };
      } else {
        next = { left, maxHeight: above, top: 8 };
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
    // The anchor this panel opened from, for the note a key made of where it was pressed from,
    // which this panel's closing forgets, so a later press on the anchor itself finds none.
    const anchor = anchorRef.current;
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
      // leaving it: it closes, and they are back on the anchor, or where the host says.
      if (inside.current) {
        (back.current?.() ?? anchorRef.current)?.focus();
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
      // A press a layer over the panel took first - the hover card's, whose listener the shell
      // adds as it mounts, so it hears the press before the panel's (`RefPreview.tsx`) - is that
      // layer's, and the panel stays open.
      if (event.defaultPrevented) return;
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
      const noted = anchor === null ? undefined : openedFrom.get(anchor);
      const to = back.current?.() ?? (noted?.isConnected ? noted : anchorRef.current);
      // The body where a key opened the panel with nothing focused: back to the page itself.
      focusBack(to);
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
      if (anchor !== null) openedFrom.delete(anchor);
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
