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

import { PANEL_ATTRIBUTE } from "../features/shell/keymap";
import { focusableIn } from "./focus-place";

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
  /** Where Escape and Tab out of the panel put the focus, read as they do: the anchor where
   *  this answers null or is not given. A menu answers the element that had the focus before
   *  it opened (`ChoiceMenu`). */
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
    // anywhere else, which is noted.
    const followFocus = (event: FocusEvent) => {
      const target = event.target;
      const edge =
        target === startEdge.current ? "start" : target === endEdge.current ? "end" : undefined;
      const open = panelRef.current;
      if (edge === undefined) {
        inside.current = target instanceof Node && open?.contains(target) === true;
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
      // An Escape that ends an IME composition is the input method's: it cancels what the reader
      // was composing in the panel's text box, and the panel stays open.
      if (event.key !== "Escape" || event.isComposing) return;
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
      // The page itself, where a key opened the panel with nothing focused: only taking the
      // focus off the control that holds it puts it back there.
      if (to === document.body) {
        if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
      } else {
        to?.focus();
      }
      dismiss.current("escape");
    };
    document.addEventListener("focusin", followFocus, true);
    document.addEventListener("pointerdown", outside, true);
    document.addEventListener("keydown", dismissOnEscape, true);
    return () => {
      document.removeEventListener("focusin", followFocus, true);
      document.removeEventListener("pointerdown", outside, true);
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
        style={{ position: "fixed", zIndex: PANEL_LAYER, ...position }}
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
