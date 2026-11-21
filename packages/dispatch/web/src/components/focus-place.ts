import { type FocusEvent, type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import { commitsOnBlur, PANEL_ATTRIBUTE, takesText } from "../features/shell/keymap";
import { focusOnDocument } from "../features/shell/roving";
import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { CONTROL } from "./control-names";
import { ROW_SELECTOR } from "./row-marker";

/**
 * The page's focus hand-offs: `useFocusPlace`, a component's own hand-off for a control a press
 * takes out of it, and `useFocusRescue`, the page's one rescue, mounted once in the app shell,
 * for every loss of the focus no component's hand-off answered. A row is `row-marker.ts`'s one
 * rule, `ROW_SELECTOR`. A loss is the reader's while a press of theirs is still live on the lost
 * control, inside any row around it, or inside the dialog, panel or selection bar it was lost from
 * (`causedByReader`). A loss the reader caused lands on what their press drew in the control's
 * place, else on the same control in the row that moved up into that place, else on that row's
 * first control, else on the control now at its place or the nearest one before it. A loss they
 * did not cause leaves the focus on the body, as the page did before it had a rescue, and scrolls
 * nothing; only an open modal dialog takes the focus then, as its own stop (`leaveUncaused`). The
 * one such loss the focus leaves the body for outside a modal is a control the page disabled or
 * hid that is drawn and enabled again within the rescue's wait, while the focus is still on the
 * body: that control gets it back. A loss of a control the reader had scrolled out of their sight
 * scrolls nothing and focuses no control (`outOfSight`): the focus stays on the body, except in a
 * dialog or a panel, which takes it itself, for a loss the reader caused, and in an open modal
 * dialog, whose own stop takes it, for one they did not (`leaveUnseen`).
 */

/** The form controls: the focus reaches one only while it is not `disabled` (`FOCUSABLE`), and a
 *  row holds one as a control either way (`ROW_CONTROL`). */
const FORM_FIELDS = ["button", "textarea", "input", "select"];

/** An element given a Tab stop of its own: any `tabindex` but -1. */
const TAB_STOP = '[tabindex]:not([tabindex="-1"])';

/** What the keyboard focus can reach: a link, a form control that is not `disabled`, or anything
 *  given a Tab stop of its own. A control refused with `aria-disabled` is one, since it keeps its
 *  Tab stop. */
const FOCUSABLE = ["a[href]", ...FORM_FIELDS.map((tag) => `${tag}:not(:disabled)`), TAB_STOP].join(
  ", "
);

/** The controls the focus can reach inside `container`, in order. One under a `hidden` ancestor -
 *  a composer kept mounted, out of sight, for a send it has out - takes no focus, so it is never
 *  where a dialog's trap starts or wraps, nor where a hand-off lands. */
export function focusableIn(container: Element): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(
    (element) => element.closest("[hidden]") === null
  );
}

/** A ref that focuses its element once, as it enters the page: React calls a ref callback again
 *  only when the callback itself is a new one, so this single function never pulls the focus back
 *  on a later render, after the reader has moved it on (AskCard's **Ask back instead**, the issue
 *  title's editor). */
export function focusOnMount(element: HTMLElement | null): void {
  element?.focus();
}

/** Where a hand-off sends the focus a control took away with it, nearest first: the control now
 *  at `index` in `controls`, then each control before that place, walking back. However many
 *  controls went together, the focus lands where they were, and by the same walk in every engine
 *  whether the loss was counted while they were still in the page (Chromium's `focusout`) or
 *  after they went (the removal Firefox and WebKit report alone). */
function placesFrom(controls: readonly HTMLElement[], index: number): HTMLElement[] {
  const before = controls.slice(0, Math.min(index, controls.length)).reverse();
  const at = controls[index];
  return at === undefined ? before : [at, ...before];
}

/** `element`'s place among the controls the focus can reach in `container` (`focusableIn`'s
 *  order): how many of them come before it in the page, counted up to it and no further.
 *  `counted` says whether the element is one of them. One that is not - disabled, hidden, or
 *  given no Tab stop of its own - is placed where the control after it is counted. */
function placeIn(container: Element, element: Element): { index: number; counted: boolean } {
  let index = 0;
  for (const control of container.querySelectorAll(FOCUSABLE)) {
    if (control.closest("[hidden]") !== null) continue;
    if (control === element) return { counted: true, index };
    if (!(control.compareDocumentPosition(element) & Node.DOCUMENT_POSITION_FOLLOWING)) break;
    index += 1;
  }
  return { counted: false, index };
}

/** Where the keyboard focus sat: the element, the container it is counted in, and its place
 *  among that container's focusable controls. */
interface Place {
  readonly container: Element;
  readonly element: Element;
  index: number;
}

/**
 * Keeps the keyboard focus off the page's body when a press takes away the element holding it
 * while the component around it stays - a page control that removes itself, a menu's trigger
 * that goes once its value is picked. The focus goes to the control now at that element's place
 * in its container, else to the nearest one before it, else to what `fallback` names for that
 * container (a table row's fallback is the row itself, and a row that has left the page names none).
 *
 * The component puts the handler this returns on its root as `onFocus`, which every focus inside
 * it bubbles to, from a portal too, since React bubbles through its own tree. `containerOf` names
 * the container a focused element is counted in, or null for one outside every container, whose
 * going moves nothing. An element React bubbled up from a portal is no control of the container,
 * so it is counted at the place of the one counted before it there, and not at all without one:
 * a nested panel's input is counted where the trigger that opened that panel is.
 *
 * The check runs after every commit of the component, so after the one that took the element
 * away, and moves the focus only when the element has left the page and the focus is on the
 * body. An element still in the page that no longer holds the focus is one the reader moved off,
 * so it is forgotten, and its going later moves nothing. Nor does the going of an element the
 * reader had scrolled out of their sight (`outOfSight`), the page's rescue's rule read from the
 * rescue's own record: a live update taking a control out of a row drawn beyond the view, or
 * Mark all read going while the reader is scrolled away, pulls neither a list nor the page back
 * to the control at its place. Nor does a going the reader did not cause (`causedByReader`): the
 * hand-off moves the focus to a row only for a loss the reader caused, and the page's rescue
 * leaves an uncaused one on the body, or on an open modal dialog's own stop, with no scroll.
 */
export function useFocusPlace(
  containerOf: (element: Element) => Element | null,
  fallback: (container: Element) => HTMLElement | null
): (event: FocusEvent) => void {
  const place = useRef<Place | null>(null);
  useLayoutEffect(() => {
    const held = place.current;
    if (held === null) return;
    if (held.element === document.activeElement) {
      // Read again from the page this commit left, so a control drawn or taken away before it
      // moves its place with it. A portal's element keeps the place it was handed.
      if (held.container.contains(held.element)) {
        const { counted, index } = placeIn(held.container, held.element);
        if (counted) held.index = index;
      }
      return;
    }
    place.current = null;
    if (held.element.isConnected || !focusOnDocument() || outOfSight(held.element)) return;
    if (!causedByReader(held.element)) return;
    const placed = held.container.isConnected
      ? placesFrom(focusableIn(held.container), held.index)[0]
      : undefined;
    (placed ?? fallback(held.container))?.focus();
  });
  return (event) => {
    const element = event.target;
    const container = containerOf(element);
    if (container === null) {
      place.current = null;
      return;
    }
    const before = place.current;
    const carried = before?.container === container ? before.index : null;
    if (!container.contains(element)) {
      // A portal's element: at the place of the control counted before it, or nowhere.
      place.current = carried === null ? null : { container, element, index: carried };
      return;
    }
    const { counted, index } = placeIn(container, element);
    place.current = { container, element, index: counted ? index : (carried ?? -1) };
  };
}

/** A dialog, modal or not - the search bar, the Inbox drawer, every `useDialog` dialog, and the
 *  panel a `PanelTrigger` opens - or any other panel an `AnchoredPanel` draws, which carries the
 *  keymap's `PANEL_ATTRIBUTE`: a `MultiSelect` list's, which has no role of its own. Each is a
 *  region of its own, and a press inside it is a press in it, and on the trigger that names it
 *  where one does (`openerOf`). Once it has gone with the element the press landed on, the press
 *  reads as one on the control it was opened from as well (`pressedOn`), since the focus goes
 *  back to its opener when it goes. */
const DIALOG = `[role="dialog"], [${PANEL_ATTRIBUTE}]`;

/** An open modal dialog: a `<dialog>` shown modally (`:modal`, the picture lightbox), or an
 *  element named a dialog that says it is modal (`aria-modal="true"`) - every `useDialog` dialog
 *  that does, the Inbox drawer, the phone's navigation drawer, and the issue page's margin and
 *  contents while the phone draws them as sheets. The page behind one is not the reader's to
 *  reach, so a loss the reader did not cause leaves the focus on its own stop rather than on the
 *  body (`leaveUncaused`). */
const MODAL = 'dialog:modal, [role="dialog"][aria-modal="true"]';

/** Whether `element` is a row of a table or list or inside one: whether it or an element around
 *  it is a row by `row-marker.ts`'s one rule, `ROW_SELECTOR`: a press on a row's mark is read by
 *  it (`pressOnRowMark`). */
function inRow(element: Element): boolean {
  return element.closest(ROW_SELECTOR) !== null;
}

/** Every row around `element` by `row-marker.ts`'s one rule (`ROW_SELECTOR`), itself included,
 *  nearest first: rows nest, as an ask card's `<article>` in an Inbox row's `<li>`, and a loss
 *  is read against each of them (`pressPlaces`, `rowPlacesOf`). */
function rowsAround(element: Element): Element[] {
  const rows: Element[] = [];
  for (
    let row = element.closest(ROW_SELECTOR);
    row !== null;
    row = row.parentElement?.closest(ROW_SELECTOR) ?? null
  ) {
    rows.push(row);
  }
  return rows;
}

/** The one checkbox a row draws for its mark (the project List's `Select <key>`, the Inbox's
 *  mark): the reader empties a selection from a row by it, from the bar itself otherwise. */
const MARK = 'input[type="checkbox"]';

/** The region a press that empties a selection could land in and still be the reader's own: a
 *  dialog or a panel (`DIALOG`), or the selection bar. */
const PRESS_REGION = `${DIALOG}, [data-control="${CONTROL.selectionBar}"]`;

/** How long the rescue waits for a control disabled under the focus - by a press, or by an update
 *  the reader did not cause - to be enabled again before it hands the focus on as if that control
 *  had gone: the app's write deadline, past which a write is late and a composer lets go of what
 *  it holds the reader to. A control enabled again after the wait gets the focus back no more. */
const RESCUE_WAIT_MS = SEND_DEADLINE_MS;

/** Whether the page draws `element`: under no `hidden` ancestor, and given a box. */
function rendered(element: Element): boolean {
  return element.closest("[hidden]") === null && element.getClientRects().length > 0;
}

/** The region a control is counted in: the dialog or panel holding it (`DIALOG`), modal or not,
 *  else the shell's main content (`main`), else the document. */
function regionOf(element: Element, main: HTMLElement | null): Element {
  return element.closest(DIALOG) ?? (main?.contains(element) ? main : document.body);
}

/** The controls the focus can reach in `region` that the page draws and that are the region's
 *  own - none of a dialog inside it, and, for the document, none of the main content's either -
 *  since a loss is answered inside the region it was lost in. The rescue counts places among
 *  these and lands only on these, so a control drawn at one width alone - `md:hidden`, say -
 *  neither holds a place nor is handed a focus it cannot show. */
function drawnIn(region: Element, main: HTMLElement | null): HTMLElement[] {
  return focusableIn(region).filter(
    (control) => regionOf(control, main) === region && rendered(control)
  );
}

/** Whether `region` is still a place the focus can be answered in: in the page and drawn. A
 *  dialog kept mounted under `hidden` - the phone thread holding a reply - has gone as much as
 *  one taken away. */
function present(region: Element): boolean {
  return region.isConnected && rendered(region);
}

/** Whether `element` stands as a control the reader can use: not disabled, and drawn. A focus
 *  lost from one that does not was taken from the reader (`taken`); the reader leaves one that
 *  still does (`left`). */
function stands(element: Element): boolean {
  return !element.matches(":disabled") && rendered(element);
}

/** How many of `controls` come before `node` in the page. */
function countBefore(controls: readonly Element[], node: Node): number {
  let index = 0;
  for (const control of controls) {
    if (
      control === node ||
      !(control.compareDocumentPosition(node) & Node.DOCUMENT_POSITION_FOLLOWING)
    ) {
      break;
    }
    index += 1;
  }
  return index;
}

/** Whether the keyboard focus is lost: on the body, or still on an element that has left the
 *  page. WebKit and Firefox move the focus off a removed element only at the next rendering
 *  update (the HTML spec's focus fixup), so until then the document still names it. The one
 *  check for a removed focus: the rescue below asks it, and so does a list closing over its own
 *  text box (`MultiSelect`). */
export function focusLost(): boolean {
  const focused = document.activeElement;
  return focused === null || focused === document.body || !focused.isConnected;
}

/** Focuses `element` and, once it holds the focus, scrolls it into view: WebKit scrolls a focused
 *  control into view only a frame later, by when a list's own scroll keeping may have moved the
 *  page again. Answers whether it took the focus. The rescue's landing for a loss the reader
 *  caused, and the Inbox's hand-off after the reader's action (`pane-selection.ts`). */
export function focusIntoView(element: HTMLElement): boolean {
  element.focus();
  if (document.activeElement !== element) return false;
  element.scrollIntoView({ block: "nearest", inline: "nearest" });
  return true;
}

/** Whether any part of `element` is in the reader's sight: its box meets the viewport and every
 *  box in `boxes`, the boxes around it that can hide it (`Sight`). An element whose edge only
 *  touches the edge of what is in sight counts as in sight. */
function inSight(element: Element, boxes: Iterable<Element>): boolean {
  let top = 0;
  let left = 0;
  let bottom = window.innerHeight;
  let right = window.innerWidth;
  for (const box of boxes) {
    const edges = box.getBoundingClientRect();
    top = Math.max(top, edges.top);
    left = Math.max(left, edges.left);
    bottom = Math.min(bottom, edges.bottom);
    right = Math.min(right, edges.right);
  }
  const drawn = element.getBoundingClientRect();
  return drawn.bottom >= top && drawn.top <= bottom && drawn.right >= left && drawn.left <= right;
}

/** The overflow values that clip what overflows a box. */
const CLIPPING_OVERFLOW: Record<string, true> = {
  auto: true,
  clip: true,
  hidden: true,
  scroll: true,
};

/** The boxes around `element` that clip what overflows them, nearest first: every ancestor whose
 *  overflow clips (`CLIPPING_OVERFLOW`), short of the body and the root, whose overflow is the
 *  viewport's. */
function clippingBoxes(element: Element): Element[] {
  const boxes: Element[] = [];
  for (
    let box = element.parentElement;
    box !== null && box !== document.body && box !== document.documentElement;
    box = box.parentElement
  ) {
    const { overflowX, overflowY } = getComputedStyle(box);
    if (CLIPPING_OVERFLOW[overflowX] || CLIPPING_OVERFLOW[overflowY]) boxes.push(box);
  }
  return boxes;
}

/** The element the focus was last noted on, and whether the reader can still see it: read off
 *  every scroll that can move it - the page's, or a box's around it, which `boxes` keeps - and
 *  every resize of the window, against every box around it that clips it, which `boxes` keeps
 *  from then on, while it is in the page and drawn. The rescue reads it when it answers a loss
 *  of that element, which may have gone by then, so it holds what the last read before that saw. */
interface Sight {
  readonly boxes: Set<Element>;
  readonly element: HTMLElement;
  seen: boolean;
  /** Whether a read since the element took the focus has found it in sight: the read at the
   *  focus itself, or one at a scroll or a resize after it (`readSight`). */
  shown: boolean;
}

/** Reads again whether the reader can see `watched`'s element, against the boxes it keeps. One
 *  out of sight now is out of the reader's sight only once a read since it took the focus has
 *  found it in sight: until then no scroll took it from them. A control focused below the fold
 *  is one the engine is still to scroll into view - WebKit does so a frame after the focus - and
 *  a scroll the page makes first can leave it there. Measured in WebKit at 390x664: the Inbox's
 *  list keeping its rows in place (`ViewportAnchor`) scrolled the page 203 px before the engine's
 *  own scroll, which had not come by the Enter on the thread's Dismiss just focused, and that
 *  press's loss was read as one the reader had scrolled out of sight, and left on the body. */
function readSight(watched: Sight): void {
  const now = inSight(watched.element, watched.boxes);
  watched.shown ||= now;
  watched.seen = now || !watched.shown;
}

/** The page's one record of what the reader can see of the element the focus was last noted on,
 *  which the page's rescue keeps (`useFocusRescue`), mounted once in the app shell: null while no
 *  rescue is mounted. Two component hand-offs read it too (`useFocusPlace`, `SelectionBar`), so
 *  one record decides for them and for the rescue. */
let sight: Sight | null = null;

/** Whether the reader had scrolled `element` out of their sight, as the page's rescue last read
 *  it: false for an element the rescue has not noted, and wherever no rescue is mounted. The
 *  rescue asks this of a loss's element before it answers the loss, and the two component
 *  hand-offs that read it (`useFocusPlace`, `SelectionBar`) ask it before they move the focus, so
 *  none of them pulls a list or the page back to a place the reader scrolled away from. */
export function outOfSight(element: Element): boolean {
  return sight?.element === element && !sight.seen;
}

/** Whether a focus rescue is mounted, so the page is recording the reader's presses. Where none
 *  is - a component read in a unit test without the shell - every loss keeps the base hand-off,
 *  as it did before this rule. */
let rescueMounted = false;

/** A control that cannot be pressed now: held while its write is out (`aria-disabled`,
 *  `held-press.ts`), or disabled, itself or through the fieldset around it. */
const HELD = '[aria-disabled="true"], :disabled';

/** The reader's latest press (`isPress`) - a pointer press, a key that acts rather than edits a
 *  field's text, or a pick - which tells a loss the reader caused from one an update made under
 *  them: where it landed, the dialog or panel it landed in (`DIALOG`), the control that opened
 *  that panel where one names it (`control`, `openerOf`) and the loss that region was opened from
 *  (`openedFrom`), the elements it can read as (`pressedOn`) that were already held when it was
 *  made (`heldBefore`, `heldAt`), its own release while that is still to come (`Release`; none for
 *  a pick's `change` or `input`), whether it can still be the cause of a loss (`live`,
 *  `answerPress`), and what stops the timers and the watch that end it. */
interface Press {
  readonly control: Element | null;
  readonly heldBefore: ReadonlySet<Element>;
  live: boolean;
  readonly opener: Loss | null;
  readonly region: Element | null;
  release: Release | null;
  stop: () => void;
  readonly target: Element;
}

/** The release a press waits for: the event that releases it (`pointerup`, `keyup`) from the
 *  pointer or the key that made the press (`by`, `pressedBy`), so that another pointer or key let
 *  go while the press is down - Space let go on a control Enter is still pressing - is not taken
 *  for it. */
interface Release {
  readonly by: number | string | null;
  readonly type: "keyup" | "pointerup";
}

/** The reader's latest press, kept by the page's rescue while it is mounted, else null. */
let lastPress: Press | null = null;

/** The event that releases a press made by each event the rescue notes a press by. */
const RELEASE: Readonly<Record<string, "keyup" | "pointerup">> = {
  keydown: "keyup",
  pointerdown: "pointerup",
};

/** What made `event`: its pointer (`pointerId`), or its key (`code`), else null. */
function pressedBy(event: Event): number | string | null {
  if (event instanceof PointerEvent) return event.pointerId;
  return event instanceof KeyboardEvent ? event.code : null;
}

/** The `<input>` types the reader picks a value in rather than types one into: each fires its
 *  `input` at the pick, as a `<select>` does (`picksValue`). The rest are the text-entry types,
 *  whose `input` is typing, and the button family and hidden, which fire none. */
const PICK_INPUT_TYPES: Record<string, true> = {
  checkbox: true,
  color: true,
  date: true,
  "datetime-local": true,
  file: true,
  month: true,
  radio: true,
  range: true,
  time: true,
  week: true,
};

/** Whether `element` takes its value by the reader's pick: a `<select>`, or an `<input>` of a
 *  pick type (`PICK_INPUT_TYPES`). */
function picksValue(element: Element): boolean {
  if (element instanceof HTMLSelectElement) return true;
  return element instanceof HTMLInputElement && PICK_INPUT_TYPES[element.type] === true;
}

/** Whether `event` is a press of the reader's (`Press`), the act that can make a loss in its row,
 *  dialog or panel theirs (`causedByReader`). A pointer press is one, and so is any key on a
 *  control that takes no typed text - Enter, Space, Escape, the arrows, a page key. In a field
 *  that takes typed text (`takesText`) a character typed is none, whatever it is typed with, and
 *  only a key that acts on the page rather than on the text is one: Escape, Tab, Enter in a
 *  single-line `<input>`, which commits it, as the issue title's editor does, and a key held
 *  with Ctrl, Meta or Alt that types no character (`typesCharacter`), a shortcut such as
 *  Ctrl+Enter or Meta+Enter that sends. A character or editing key there - a printable key, one
 *  held with AltGr or Option, Space, Backspace, Delete, the arrows, Home and End, Enter in a
 *  `<textarea>` or a content-editable element, which inserts a line - only edits the field. A
 *  `change` is one where the control fires it at the reader's pick, which Playwright and
 *  assistive tech make without a key or a pointer: a `<select>`, a checkbox, a radio, a file,
 *  range or color input, and a date, time, datetime-local, month or week box, which fires it from
 *  its picker or with the key that completes the value, though it takes typed text. A `change` is
 *  none on a field whose value is typed text alone (`commitsOnBlur`) - an `<input>` of type text,
 *  search, email, url, tel, password or number, a `<textarea>`, a content-editable element -
 *  where it is the field's commit on blur, which Chromium fires as an update takes the focused
 *  field away. An `input` is one on the controls that take their value by a pick (`picksValue`):
 *  a `<select>`, and an `<input>` of type checkbox, radio, file, range, color, date, time,
 *  datetime-local, month or week. Firefox fires `input` alone as the digit that completes a date
 *  is typed, and a value set without keys, as Playwright's `fill()` and assistive tech set it,
 *  fires `input` and then a `change` the page may already have taken the control away from at
 *  that `input`. An `input` is none on a text-entry field - an `<input>` of type text, search,
 *  email, url, tel, password or number, a `<textarea>`, a content-editable element - where it is
 *  typing, and none on any other element. */
function isPress(event: Event): event is Event & { readonly target: Element } {
  const { target } = event;
  if (!(target instanceof Element)) return false;
  if (event.type === "change") return !commitsOnBlur(target);
  if (event.type === "input") return picksValue(target);
  if (!(event instanceof KeyboardEvent) || !takesText(target)) return true;
  const { key } = event;
  return (
    key === "Escape" ||
    key === "Tab" ||
    (key === "Enter" && target instanceof HTMLInputElement) ||
    ((event.ctrlKey || event.metaKey || event.altKey) && !typesCharacter(event))
  );
}

/** Whether `event`, a key held with Ctrl, Meta or Alt, types a character all the same. It does
 *  while AltGr is held (`AltGraph`), which Windows reports as Ctrl and Alt together and Linux on
 *  its own, as a German layout types `@` with AltGr and Q; and while Alt is held without Ctrl or
 *  Meta on a key that is one character, as macOS's Option types `@`, `[`, `{` or `|`. */
function typesCharacter(event: KeyboardEvent): boolean {
  return (
    event.getModifierState("AltGraph") ||
    (event.altKey && !event.ctrlKey && !event.metaKey && event.key.length === 1)
  );
}

/** The nearest of `opener` and the openers before it whose region is still in the page and drawn
 *  (`present`), else null: where a focus lost from a region that has gone goes back to. */
function drawnOpener(opener: Loss | null): Loss | null {
  let from = opener;
  while (from !== null && !present(from.region)) from = from.opener;
  return from;
}

/** The control `element` is held through while it is in the page: the nearest of itself and the
 *  elements around it that cannot be pressed now (`HELD`) - the button a span or an icon of it is
 *  drawn in - else null. */
function heldControl(element: Element): Element | null {
  return element.isConnected ? element.closest(HELD) : null;
}

/** Whether `element` is in the page and cannot be pressed now (`HELD`). */
function isHeld(element: Element): boolean {
  return heldControl(element) !== null;
}

/** The elements a press on `target` can read as (`pressedOn`) - `target`, the control that opened
 *  the panel it is in (`control`, `openerOf`), and the control each region around it was opened
 *  from, `opener` and each opener before it - that are held now (`isHeld`). Read as the press is
 *  made, before what it does: a control held then takes the press without a write, so its hold
 *  is no doing of the press (`answerPress`). */
function heldAt(
  target: Element,
  control: Element | null,
  opener: Loss | null
): ReadonlySet<Element> {
  const held = new Set<Element>();
  for (const each of control === null ? [target] : [target, control]) {
    if (isHeld(each)) held.add(each);
  }
  for (let from = opener; from !== null; from = from.opener) {
    if (isHeld(from.element)) held.add(from.element);
  }
  return held;
}

/** The elements `press` reads as now: the one it landed on, the control that opened the panel it
 *  landed in (`control`, read as the press was made, while the panel still stood), and - once
 *  that element has left the page with the dialog or panel it landed in (`present`) - the control
 *  that region was opened from too, else the nearest opener before it whose region is still drawn
 *  (`drawnOpener`). A list's pick or a dialog's confirm takes its region away and hands the focus
 *  back to the trigger that opened it; a write the press made that takes that trigger, or the row
 *  or the selection bar around it, away only once the server answers is still the press's doing
 *  while the trigger is held - a snooze picked from the selection bar's menu, which the focus
 *  entered from wherever the bar's key was pressed, is a press the reader made in the bar. The
 *  element it landed on still counts, for a loss inside the region that went with it. A press in
 *  no dialog or panel, or in one opened from the body that names no opener, is read as it
 *  landed. */
function pressedOn(press: Press): Element[] {
  const { control, opener, region, target } = press;
  const landed = control === null ? [target] : [target, control];
  if (target.isConnected || region === null || present(region)) return landed;
  const from = drawnOpener(opener);
  return from === null ? landed : [...landed, from.element];
}

/** Whether a press on `target` lands on a control `press` holds: one `press` reads as
 *  (`pressedOn`), held now by a hold that began after `press` (not in `heldBefore`), with `target`
 *  that control or inside it. The controls are compared, not the nodes the presses landed on, so
 *  a press on a span or an icon of the button counts, and so does a press on the button after
 *  one on a span of it. */
function onHeldControl(press: Press, target: Element): boolean {
  return pressedOn(press).some(
    (each) => !press.heldBefore.has(each) && heldControl(each)?.contains(target) === true
  );
}

/** Keeps `press` live while the update that answers it is still to run: a press is the cause only
 *  of the change it leads to. That is a frame and a task from now (`afterComponents`) - from the
 *  press, and again from its own release, which a click answers (`noteRelease`) - by when React
 *  has committed what the press did and run its effects, and longer only while a control it reads
 *  as (`pressedOn`) is held (`HELD`) by any hold that began after the press - its write out, or
 *  what the press did to its own control, as a pager's Next is held on the last page it led to -
 *  and then until a frame and a task after that hold ends: the control taken away, or its
 *  `aria-disabled` or `disabled` gone. A control already held when the press was made
 *  (`heldBefore`) - the pager's Next on its last page, a select or a trigger held while an
 *  earlier write is out - took the press without a write, so its hold keeps the press live no
 *  longer than a press on a control held by nothing. The hold's end is watched rather than
 *  polled, since a write's latency is the server's, and the watch ends at the write deadline
 *  (`RESCUE_WAIT_MS`) counted from the press's release - from the press itself for a pick's
 *  `change` or `input`, which has none - as the rescue's own wait does, so a hold that never ends
 *  keeps no watch over the page and no press live past it. A press past that is no cause, so an
 *  update after it - a live update taking the row away, a refetch emptying the selection bar - is
 *  a loss the reader did not cause. */
function answerPress(press: Press): void {
  press.stop();
  press.live = true;
  // Once the press's target and its region have left the page, which they never come back to,
  // what the press reads as is kept rather than walked again at every change the watch sees.
  let kept: Element[] | null = null;
  const pressed = (): Element[] => {
    if (kept !== null) return kept;
    const now = pressedOn(press);
    const { region, target } = press;
    if (!target.isConnected && (region === null || !region.isConnected)) kept = now;
    return now;
  };
  const held = () => pressed().some((each) => !press.heldBefore.has(each) && isHeld(each));
  press.stop = afterComponents(() => {
    if (!held()) {
      press.live = false;
      return;
    }
    const end = () => {
      watcher.disconnect();
      window.clearTimeout(bound);
      press.stop = afterComponents(() => {
        press.live = false;
      });
    };
    // The whole document is watched, so that the control going with a parent of it, a fieldset
    // around it enabled again, or the region the press landed in going, ends the hold too.
    const watcher = new MutationObserver(() => {
      if (!held()) end();
    });
    const bound = window.setTimeout(end, RESCUE_WAIT_MS);
    watcher.observe(document.body, {
      attributeFilter: ["aria-disabled", "disabled"],
      childList: true,
      subtree: true,
    });
    press.stop = () => {
      watcher.disconnect();
      window.clearTimeout(bound);
    };
  });
}

/** The reader's latest press while it can still be the cause of a loss (`answerPress`). */
function recentPress(): Press | null {
  return lastPress?.live ? lastPress : null;
}

/** The control that opened the panel `target` is in: the one naming that panel as what it opened
 *  (`aria-controls`), a menu's trigger or a list's. Null outside every panel. A pick in a menu is
 *  the reader's act on the control that opened it: a snooze picked from the selection bar's
 *  menu, which closes the menu, hands the focus back to its trigger and then takes the bar away,
 *  is a press the reader made in the bar. */
export function openerOf(target: Element): Element | null {
  const panel = target.closest(`[${PANEL_ATTRIBUTE}][id]`);
  return panel === null
    ? null
    : document.querySelector(`[aria-controls="${CSS.escape(panel.id)}"]`);
}

/** The panel `trigger` names as what it opened (`aria-controls`), the other way from `openerOf`:
 *  a menu's, a list's, or the box a disclosure draws under it. Null while the trigger names none
 *  or the page holds no element of that id. */
export function panelOf(trigger: Element): HTMLElement | null {
  const id = trigger.getAttribute("aria-controls");
  return id === null ? null : document.getElementById(id);
}

/** The places a press could have taken `element`'s focus from and still be the reader's own doing:
 *  the element itself, every row around it (`rowsAround`, by the rule `inRow` reads), and the
 *  dialog, panel or selection bar it was lost from. Every row counts, not the nearest alone: a
 *  press in an Inbox row outside the ask card it holds is a press in that row, so a loss inside
 *  the card is the reader's own. A press in a list's panel is in that panel, so a pick that takes
 *  the panel away is the reader's own, and the focus goes back to the trigger that opened it. */
function pressPlaces(element: Element): Element[] {
  const places: Element[] = [element, ...rowsAround(element)];
  const region = element.closest(PRESS_REGION);
  if (region !== null) places.push(region);
  return places;
}

/**
 * Whether the reader caused the loss of `element`'s focus: their latest press (`isPress`) - a
 * pointer press, a key that acts rather than edits a field's text, or a pick - is still live, its
 * answer not yet run (`answerPress`), and landed on the element, inside any row it was in, or
 * inside the dialog, panel or selection bar it was lost from (`pressPlaces`), read at every element
 * the press reads as now (`pressedOn`): a pick in a menu or a list counts as a press on the trigger
 * that opened it (`openerOf`), and a pick in a list, a confirm in a dialog, that has gone counts as
 * a press on the control the list or the dialog was opened from as well. A loss the reader caused
 * keeps the hand-off that pulls the focus to the control now at the lost one's place; one they did
 * not - a live update taking a row away, a refetch emptying a bar - leaves the focus on the body,
 * or on an open modal dialog's own stop, and scrolls nothing (`leaveUncaused`). The rescue and
 * every component hand-off that can move the focus to another row read this one rule; a hand-off
 * to the lost control's own row, as AskCard's is, is no move to another row.
 */
export function causedByReader(element: Element): boolean {
  if (!rescueMounted) return true;
  const press = recentPress();
  if (press === null) return false;
  const pressed = pressedOn(press);
  return pressPlaces(element).some((place) =>
    pressed.some((each) => place === each || place.contains(each))
  );
}

/** Whether the reader's latest press, while it is still live (`answerPress`), was on a row's mark,
 *  read at every element the press reads as now (`pressedOn`): the selection bar reads this beside
 *  `causedByReader`, since unticking the last row by its mark empties the bar while the focus may
 *  rest in it (a checkbox a click leaves unfocused, as WebKit does). */
export function pressOnRowMark(): boolean {
  const press = recentPress();
  return press !== null && pressedOn(press).some((each) => inRow(each) && each.matches(MARK));
}

/** The open modal dialog (`MODAL`) a loss in `region` is answered in, else null: of the ones in
 *  the page and drawn (`present`), the one that holds `region`, the region the loss was in, else
 *  the last in the page's order, drawn over the ones before it. */
function openModal(region: Element): HTMLElement | null {
  const open = [...document.querySelectorAll<HTMLElement>(MODAL)].filter(present);
  return open.find((modal) => modal.contains(region)) ?? open.at(-1) ?? null;
}

/** Focuses `dialog`, an open modal dialog, as its own stop, with no scroll. One with no Tab stop
 *  of its own is given `tabindex="-1"` for the focus and has it taken off again once the focus
 *  leaves, so the engine's own Tab order is left as it was. */
function landOnDialog(dialog: HTMLElement): void {
  const needsStop = !dialog.hasAttribute("tabindex");
  if (needsStop) dialog.tabIndex = -1;
  const took = focusNoScroll(dialog);
  if (!needsStop) return;
  if (took) {
    dialog.addEventListener("blur", () => dialog.removeAttribute("tabindex"), { once: true });
  } else {
    dialog.removeAttribute("tabindex");
  }
}

/** Answers a loss the reader did not cause (`causedByReader`), lost in `region`, as the page did
 *  before it had a rescue: the focus stays on the body, where the loss left it, and nothing
 *  scrolls. Whatever control stands near the lost place, the page chose it and the reader did
 *  not, and their next key would act on it or be typed into it. The one exception is an open
 *  modal dialog (`openModal`), which takes the focus itself as its own stop (`landOnDialog`),
 *  since a focus that leaves an open modal reaches the page behind it. Outside a modal, the one
 *  loss the reader did not cause that the focus leaves the body for is a control the page
 *  disabled or hid and then drew and enabled again, which gets the focus back while it is still
 *  on the body and within the rescue's wait (`useFocusRescue`'s `check` and `wait`): the very
 *  control that held it, back where it was. */
function leaveUncaused(region: Element): void {
  const modal = openModal(region);
  if (modal !== null) landOnDialog(modal);
}

/** Answers a loss the reader can no longer see, in a region still in the page: nothing scrolls,
 *  and the focus goes to no control the reader did not pick. For a loss the reader caused it
 *  stays where it is, on the body, except in a dialog or a panel (`DIALOG`), which takes the
 *  focus itself so that the page behind a modal one is never reached - given `tabindex="-1"` for
 *  it where it has no Tab stop of its own, as when it loses every control. A loss they did not
 *  cause is answered as every such loss is (`leaveUncaused`): on the body, or on an open modal
 *  dialog's own stop, and never on a panel that is no modal. */
function leaveUnseen(loss: Loss): void {
  const { caused, region } = loss;
  if (!caused) {
    leaveUncaused(region);
    return;
  }
  if (!(region instanceof HTMLElement) || !region.matches(DIALOG)) return;
  if (!region.hasAttribute("tabindex")) region.tabIndex = -1;
  region.focus({ preventScroll: true });
}

/** Where a node stood in the page while it was there: the parent holding it and the siblings
 *  either side of it, `previous` and `next`, null at either end. */
interface Spot {
  readonly next: Node | null;
  readonly node: Node;
  readonly parent: Node;
  readonly previous: Node | null;
}

/** Where `element` stands in `region`, and where each parent of it up to the region's own
 *  children stands, nearest first: read while the element is still in the page, so that the spot
 *  a press later takes it, or a parent of it, away from is known (`controlsInPlace`), and so are
 *  the spots of the rows around it (`controlsOfRowMovedUp`). */
function spotsOf(element: Element, region: Element): Spot[] {
  const spots: Spot[] = [];
  let node: Node = element;
  for (let parent = node.parentNode; parent !== null && node !== region; parent = node.parentNode) {
    spots.push({ next: node.nextSibling, node, parent, previous: node.previousSibling });
    node = parent;
  }
  return spots;
}

/** Whether `spot`'s node has left the page while the parent it stood in is still in `region`. */
function goneFrom(spot: Spot, region: Element): boolean {
  return !spot.node.isConnected && spot.parent.isConnected && region.contains(spot.parent);
}

/** What a row holds as its controls, by markup alone: what the focus can reach (`FOCUSABLE`), and
 *  a button or form field disabled now as well, since a press can disable its own control before
 *  its row goes - the loss is noted at the disable's `focusout` then - and that control keeps its
 *  place in the row. */
const ROW_CONTROL = ["a[href]", ...FORM_FIELDS, TAB_STOP].join(", ");

/** The controls `row` holds, in order, by markup alone (`ROW_CONTROL`): the row itself first
 *  where it is one, then each inside it. Counted by markup and not by what the page draws, so a
 *  row counts the same whether it is still in the page, as at Chromium's `focusout`, or has left
 *  it, as when Firefox and WebKit report its removal alone, where nothing is drawn. */
function rowControls(row: Element): Element[] {
  const inside = [...row.querySelectorAll(ROW_CONTROL)];
  return row.matches(ROW_CONTROL) ? [row, ...inside] : inside;
}

/** Where a control stood in a row around it: the row - an element around it, itself included,
 *  that `ROW_SELECTOR` names, inside the control's region - and the control's place among that
 *  row's controls (`rowControls`), -1 where it is none of them. */
interface RowPlace {
  readonly index: number;
  readonly row: Element;
}

/** Where `element` stands in each row around it inside `region` (`RowPlace`), nearest first;
 *  none where no row inside the region holds it. Every row is kept, not the nearest alone, since
 *  rows nest and a press can take an outer row away with the inner one in it: an Inbox row with
 *  the ask card it holds. Read as its loss is noted, while the rows still hold it: in the page,
 *  at Chromium's `focusout`, or off the removal Firefox and WebKit report alone, once a row has
 *  left the page with the element still in it, when the rows around it are that row and those
 *  inside it. The element has gone by the time the loss is answered, so its places are kept. */
function rowPlacesOf(element: Element, region: Element): RowPlace[] {
  const places: RowPlace[] = [];
  for (const row of rowsAround(element)) {
    // A row around the region holds every control of it, and so does each row around that one.
    if (row.contains(region)) break;
    places.push({ index: rowControls(row).indexOf(element), row });
  }
  return places;
}

/** A focus an element lost: the element, the region it was counted in, its place there among
 *  the controls the page draws, the path of the page it was focused on, whether the page took it
 *  from the reader (`taken`: the element was disabled, hidden or taken away) rather than the
 *  reader moving off it, whether the reader left it (`left`), and - in a dialog - the loss the
 *  dialog was opened from (`opener`): the place the focus left for it, or null for a dialog the
 *  focus entered from the body. A loss kept only as a dialog's opener holds no spots, no arrivals
 *  and no row places: the hand-off reads only its element, region, place, path and opener. */
interface Loss {
  /** The elements the page added from the moment the element went until the loss is answered,
   *  noted into it while its check or its wait is out - none the page only moved, which stood in
   *  the page before (`noteArrivals`): what can now stand where it stood (`standingAt`). */
  readonly arrived: Set<Node>;
  readonly caused: boolean;
  readonly element: HTMLElement;
  readonly index: number;
  /** Whether the reader left the element (`record`): the focus went to nothing from it while it
   *  was enabled and drawn, and it still stood in the page so once the event that moved the focus
   *  had run. Set in a microtask after the loss, long before its check, which then does nothing. */
  left: boolean;
  readonly opener: Loss | null;
  readonly path: string;
  readonly region: Element;
  /** Where the element stood in each row around it, nearest first, read as the loss is noted
   *  (`rowPlacesOf`), and none where it stood in no row: each row, and the element's place among
   *  its controls, by which the row that moves up into the place of the row that left offers the
   *  focus (`controlsOfRowMovedUp`). */
  readonly rowPlaces: readonly RowPlace[];
  /** Where the element stood, and where each parent of it up to the region stood, nearest first
   *  (`controlsInPlace`, `controlsOfRowMovedUp`): read at its `focusout` while it is still in the
   *  page, or, for an element taken away with no `focusout`, off the last record that took it
   *  (`removalOf`), ahead of the spots read when it took the focus. */
  readonly spots: readonly Spot[];
  readonly taken: boolean;
}

/** The longest the rescue waits for an animation frame before it goes on without one. WebKit can
 *  hold a page's frames back while nothing on it is drawn - measured in headless WebKit, a check
 *  waited 775 ms for its frame, and another had none in 2 s while the page's timers ran - and a
 *  keyboard reader is not to sit on the body for that. React has committed and run its passive
 *  effects well inside this. */
const FRAME_WAIT_MS = 100;

/** Runs `then` once the components have had their turn with a loss of focus: a frame, or
 *  `FRAME_WAIT_MS` where no frame comes, and then a task later, after React has committed and run
 *  its passive effects. Answers what cancels it. */
function afterComponents(then: () => void): () => void {
  let task: number | undefined;
  const go = () => {
    if (task !== undefined) return;
    window.cancelAnimationFrame(frame);
    window.clearTimeout(late);
    task = window.setTimeout(then, 0);
  };
  const frame = window.requestAnimationFrame(go);
  const late = window.setTimeout(go, FRAME_WAIT_MS);
  return () => {
    window.cancelAnimationFrame(frame);
    window.clearTimeout(late);
    window.clearTimeout(task);
  };
}

/** A removal `records` tell of: where the node the record took stood (`spot`), and which of the
 *  records took the lost element out of the page (`at`), so that what the page added from there on
 *  can be told from what it held before. */
interface Removal {
  readonly at: number;
  readonly spot: Spot;
}

/** The last of `records` that took `node` away, itself or with a parent: its index (`at`), the
 *  record, and the node it removed that holds `node` (`taken`). The last, since a node the batch
 *  moved - a removal and an addition of it - and then took away left the page from the place the
 *  last record names, not the one the move took it from. */
function recordTaking(
  records: readonly MutationRecord[],
  node: Node
): { at: number; record: MutationRecord; taken: Node } | undefined {
  let found: { at: number; record: MutationRecord; taken: Node } | undefined;
  for (const [at, record] of records.entries()) {
    for (const taken of record.removedNodes) {
      if (taken.contains(node)) found = { at, record, taken };
    }
  }
  return found;
}

/** The removal in `records` that took `element` away, itself or with a parent, read off the last
 *  record that took it (`recordTaking`) at a point still in the page: where the parent it was
 *  taken from went too, later in the same batch, the last record that took that parent. */
function removalOf(records: readonly MutationRecord[], element: Element): Removal | undefined {
  let node: Node = element;
  let removal: Removal | undefined;
  for (;;) {
    const found = recordTaking(records, node);
    if (found === undefined) return removal;
    const { at, record, taken } = found;
    removal = {
      at: removal?.at ?? at,
      spot: {
        next: record.nextSibling,
        node: taken,
        parent: record.target,
        previous: record.previousSibling,
      },
    };
    if (record.target.isConnected) return removal;
    node = record.target;
  }
}

/** The place of a control taken away from `spot`, itself or with a parent, among the controls
 *  `region` draws: how many of them come before the point it was taken from - before the sibling
 *  after it while that is still in the page, else up to and through the sibling before it, else
 *  through the parent it was taken from. */
function placeOfRemoval(region: Element, spot: Spot, main: HTMLElement | null): number {
  const next = spot.next?.isConnected ? spot.next : null;
  const through = spot.previous?.isConnected ? spot.previous : spot.parent;
  let index = 0;
  for (const control of drawnIn(region, main)) {
    const before =
      next === null
        ? through.contains(control) ||
          control.compareDocumentPosition(through) & Node.DOCUMENT_POSITION_FOLLOWING
        : control.compareDocumentPosition(next) & Node.DOCUMENT_POSITION_FOLLOWING;
    if (!before) break;
    index += 1;
  }
  return index;
}

/** What now stands at `spot`: the elements its parent holds between the siblings its node stood
 *  between that the page added since the loss (`arrived`). A sibling no longer in that parent
 *  bounds nothing, so the parent's start or end bounds that side instead; only what arrived
 *  counts, so an element that stood in the parent before the loss is never taken for what stands
 *  there, and a block taken away with nothing drawn in its place leaves nothing standing. */
function standingAt(spot: Spot, arrived: ReadonlySet<Node>): Element[] {
  const { parent } = spot;
  const previous = spot.previous?.parentNode === parent ? spot.previous : null;
  const next = spot.next?.parentNode === parent ? spot.next : null;
  const standing: Element[] = [];
  for (
    let node = previous === null ? parent.firstChild : previous.nextSibling;
    node !== null && node !== next;
    node = node.nextSibling
  ) {
    if (node instanceof Element && arrived.has(node)) standing.push(node);
  }
  return standing;
}

/** The first `<form>` among `elements`, itself one of them or inside one, else null. */
function formIn(elements: readonly Element[]): Element | null {
  for (const element of elements) {
    const form = element.matches("form") ? element : element.querySelector("form");
    if (form !== null) return form;
  }
  return null;
}

/** The controls of what now stands where `loss`'s element left the page, among `drawn`, in the
 *  order the focus is offered them: those of a `<form>` there first - the editor a press opened in
 *  the control's place, as Change answer and Add a priority open theirs - so that a header the new
 *  content repeats above the form is not taken for the place, then the rest. None where nothing
 *  new stands there (`standingAt`). The place is the nearest of the loss's spots whose node has
 *  gone while the parent it stood in is still in the region: the element's own spot, or that of
 *  the parent it went with. There is none while the element and every parent of it are still in
 *  the page - a control disabled, hidden or moved. Only what arrived counts: a row that moved up
 *  into the place of a row taken away stood in the page before the loss, so it is nothing new
 *  there, and `controlsOfRowMovedUp` answers for it instead. */
function controlsInPlace(loss: Loss, drawn: readonly HTMLElement[]): HTMLElement[] {
  const spot = loss.spots.find((each) => goneFrom(each, loss.region));
  if (spot === undefined) return [];
  const standing = standingAt(spot, loss.arrived);
  if (standing.length === 0) return [];
  const controls = drawn.filter((control) => standing.some((each) => each.contains(control)));
  const form = formIn(standing);
  if (form === null) return controls;
  return [
    ...controls.filter((control) => form.contains(control)),
    ...controls.filter((control) => !form.contains(control)),
  ];
}

/** The first element among `node` and the siblings after it, else null. */
function elementFrom(node: Node | null): Element | null {
  for (let each = node; each !== null; each = each.nextSibling) {
    if (each instanceof Element) return each;
  }
  return null;
}

/** The controls of the row that now stands where a row around `loss`'s element stood, among
 *  `drawn`, in the order the focus is offered them: the same control as the lost one, so the
 *  reader can go on - unpinning, as Dismiss goes on to the next item - then that row's first
 *  control. The row that left is the one of the rows around the element (`rowPlaces`) whose own
 *  spot's node has left the page while the parent it stood in is still in the region
 *  (`goneFrom`): with rows nested, the outermost one that went, as an Inbox row a press takes
 *  away with the ask card in it, since each row inside it went with a parent of its own. The row
 *  now at its place is the element that stood right after it in that parent, still there and a
 *  row (`ROW_SELECTOR`) - the row that moved up, the same element. The same control is the one at
 *  the lost control's place among that row's controls (`rowControls`), of the same tag and role,
 *  and drawn: read by markup in both rows, so every engine finds the same one. None where the
 *  element stood in no row, where each row around it is still in the page or went with a parent
 *  of its own, where no row stood after the one that left, or where that row draws no control. */
function controlsOfRowMovedUp(loss: Loss, drawn: readonly HTMLElement[]): HTMLElement[] {
  const { element, region, rowPlaces, spots } = loss;
  let left: { readonly index: number; readonly spot: Spot } | null = null;
  for (const { index, row } of rowPlaces) {
    const spot = spots.find(({ node }) => node === row);
    if (spot !== undefined && goneFrom(spot, region)) {
      left = { index, spot };
      break;
    }
  }
  if (left === null) return [];
  const { index, spot } = left;
  const moved = elementFrom(spot.next);
  if (moved === null || moved.parentNode !== spot.parent || !moved.matches(ROW_SELECTOR)) {
    return [];
  }
  const controls = drawn.filter((control) => moved.contains(control));
  const first = controls[0];
  if (first === undefined) return [];
  const same = rowControls(moved)[index];
  const alike =
    same instanceof HTMLElement &&
    same !== first &&
    controls.includes(same) &&
    same.tagName === element.tagName &&
    same.getAttribute("role") === element.getAttribute("role");
  return alike ? [same, first] : [first];
}

/** Focuses `target` without a scroll: what a press drew where its control stood, which stands
 *  where the reader was, a control given back, and a modal dialog's own stop. Answers whether it
 *  took the focus. */
function focusNoScroll(target: HTMLElement): boolean {
  target.focus({ preventScroll: true });
  return document.activeElement === target;
}

/** Hands on the focus `loss`'s element took with it when it went. A loss the reader did not
 *  cause - an update of the server's, which can draw a form of its own where the control stood,
 *  or take a row away - is handed nowhere (`leaveUncaused`): whatever stands there now, whatever
 *  row moved up and whatever control is near, the page chose it and the reader did not, so the
 *  focus stays on the body, or on the stop of an open modal dialog, and nothing scrolls. A loss
 *  the reader caused is answered inside the region it was lost in while that region is still in
 *  the page and drawn (`present`). Where something now stands where the element stood - what
 *  their press drew in its place - the focus lands on that first: on the first control of a
 *  `<form>` there, the editor the press opened in the control's place, else on the first control
 *  of what stands there (`controlsInPlace`), focused with no scroll, since it stands where the
 *  reader was. Where nothing new stands there and the press took away a row the element was in,
 *  the focus lands on the same control in the row that now stands at that row's place - the row
 *  that stood right after it, moved up - so the reader can go on, else on that row's first control
 *  (`controlsOfRowMovedUp`), and that control is scrolled into view (`focusIntoView`). Only where
 *  no row stands there, or that row draws no control to take it, does the focus walk back: to the
 *  control the region now draws at its place, else the nearest one before it however many went
 *  together (`placesFrom`), else the region itself, which is every region but the document, and
 *  that control is scrolled into view (`focusIntoView`). From a region that has gone - a dialog
 *  or a panel that closed, or one hidden while it stays mounted - the focus goes only back to the
 *  place it was opened from (its `opener`, or that region's own opener where it has gone too):
 *  that element, the control that opened it - a list's trigger - if it can still take the focus,
 *  else the control now at its place, else the nearest one before it. A dialog opened from the
 *  body hands the focus nowhere: the body is where the reader was. */
function handOn(loss: Loss, main: HTMLElement | null): void {
  const { caused, region } = loss;
  if (!caused) {
    leaveUncaused(region);
    return;
  }
  let candidates: HTMLElement[] = [];
  if (present(region)) {
    const drawn = drawnIn(region, main);
    for (const control of controlsInPlace(loss, drawn)) {
      if (focusNoScroll(control)) return;
    }
    for (const control of controlsOfRowMovedUp(loss, drawn)) {
      if (focusIntoView(control)) return;
    }
    candidates = placesFrom(drawn, loss.index);
  } else {
    const opener = drawnOpener(loss.opener);
    if (opener !== null && opener.path === loss.path) {
      candidates = [opener.element, ...placesFrom(drawnIn(opener.region, main), opener.index)];
    }
  }
  for (const candidate of candidates) {
    if (!candidate.isConnected || !rendered(candidate)) continue;
    if (focusIntoView(candidate)) return;
  }
  if (!present(region) || region === document.body || !(region instanceof HTMLElement)) return;
  if (!region.hasAttribute("tabindex")) region.tabIndex = -1;
  focusIntoView(region);
}

/**
 * The page's one focus rescue, mounted once in the app shell: after a loss of the focus to the
 * body - a press, or an update that takes the focused control away - it answers a focus the
 * element holding it took with it when it was disabled, hidden, taken away or drawn again under a
 * new key, since a keyboard reader left on the body starts again from the page's first link, a
 * walk through everything above the control they were using. It leaves the focus on the body
 * where that is where the reader put it or left the control (below): a dialog opened from the
 * body that closes, the reader's Escape, their click on the page or their Enter that commits a
 * field, and a control they had scrolled out of their sight; and for every loss they did not
 * cause (`leaveUncaused`), as the page did before it had a rescue, save in an open modal dialog,
 * whose own stop takes the focus, and save a control the page disabled or hid that is drawn and
 * enabled again within the rescue's wait, which gets the focus back.
 *
 * Every element that takes the focus is noted as it does (`focusin`), with its region - the
 * dialog or panel holding it (`DIALOG`), modal or not, else the shell's main content (`main`),
 * else the document - and the path of the page it was focused on, which a navigation does not
 * change before the old page goes. A dialog or panel the focus enters for the first time notes
 * where it came from: the loss of the element that handed it over - a list's trigger, for the
 * list's text box - or none when it came from the body. A loss is seen two ways.
 * A control disabled, hidden or left by the reader fires a `focusout` in every engine, while it is
 * still in the page. A control taken away, itself or with a parent, is seen by watching the page
 * (a MutationObserver over the document) in every engine, since Firefox and WebKit fire no
 * `focusout` for it; Chromium fires one as well, and whichever comes first is the loss recorded.
 * Each loss holds its element, its region, its place among the controls the region draws, the
 * path, whether the page took the element (`taken`), whether the reader caused the loss
 * (`causedByReader`), and its dialog's opener. The reader caused it when a press of theirs
 * (`isPress`) is still live in its row, dialog or panel; a key, a `change` or an `input` that is
 * no press neither starts a press nor ends the live one (`notePress`). It holds where the element
 * stood in the page too: the parent it stood in and the siblings either side of it, and the same
 * for each parent of it up to the region (`spotsOf`). For a `focusout` they are read there and
 * then, while the element is still in the page; for a control taken away with no `focusout`, the
 * last record that took it - its parent and its siblings at that moment, since a control the batch
 * moved first was taken from there - is that spot, with the spots read as the element took the
 * focus behind it for a record whose parent has gone too. It holds each row around the element,
 * and the element's place among that row's controls (`rowPlacesOf`), read then too, since
 * the element has gone by the time the loss is answered. From the loss on, while its check or its
 * wait is out, the page's watcher notes every element the page adds (`arrived`), but none it only
 * moves - a keyed node React moves with `insertBefore`, which one batch reports as its removal and
 * its addition, stood in the page before; for a loss read off a removal, the batch that took the
 * element counts from that last record on. The focus moving from one control to another in the
 * same region is no loss. Nor is the reader leaving a control: their Enter that commits a field
 * with the page's own `blur()`, or their click on the page's text, sends the focus to nothing
 * from a control still enabled and drawn, and the focus stays where that left it. Such a loss is
 * marked left (`left`) when the control still stands so in a microtask queued from its
 * `focusout`, once the event that moved the focus has run, and nothing answers it, even when a
 * commit after that takes the control away - the issue title's editor, whose save draws the
 * heading in its place. A control a commit takes away fires Chromium's `focusout` during the
 * removal itself, while it is still in the page, so by that microtask it has gone, and its loss
 * is answered. A new loss cancels whatever is still out for the one before it.
 *
 * The check runs a frame and a task after the loss, once React has committed and run its passive
 * effects, so every component's own hand-off has had its turn first: every `useFocusPlace`
 * hand-off (ControlRow, DataTable, PanelTrigger and the pages' own), SelectionBar's
 * `returnFocus`, AskCard's, `useDialog`'s return to the opener. Each knows its own target better
 * than a place counted in the page does - the row marked last, the opener - so the rescue answers
 * only a loss none of them answered, and acts only while the focus is still lost. It leaves alone
 * a loss the reader left (above), and a loss on a page the reader has since left, since the new
 * page owns its focus. A taken element that is back by then gets the focus back, whoever caused
 * its loss.
 *
 * An element that went - taken away, hidden, no longer drawn, or replaced under a new key - hands
 * the focus on (`handOn`). For a loss the reader caused (`causedByReader`), it is answered inside
 * the region it was lost in, and what their press drew where the element stood takes it first:
 * the elements the page added since the loss between the siblings either side of the element, or
 * of the parent it went with, in the parent it was taken from, which is still in the region
 * (`standingAt`). Where that holds a `<form>` - an editor the press opened in the control's
 * place, as Change answer and Add a priority do - the form's first control takes the focus, so
 * that a header the new content repeats above the form is not taken for the place; else the
 * first control of what stands there does. It is focused with no scroll, since it stands where
 * the reader was. Where nothing new stands there and the press took away a row the element was in -
 * with rows nested, the outermost that went, as an Inbox row with the ask card it holds - the row
 * that stood right after that row has moved up into its place, the same element, so nothing new
 * there, and the same control in it takes the focus, at the lost one's place among the row's
 * controls and of its tag and role, else that row's first control (`controlsOfRowMovedUp`), so the
 * reader can go on, unpinning as Dismiss goes on to the next item. Only where no row stands
 * there - a block taken away with nothing in its place, or the last row - or that row draws no
 * control does the focus walk back, as `handOn` states; that walk stays inside the region, so the
 * page behind a modal dialog is never reached. A loss the reader did not cause - an update of the
 * server's, which can draw a form of its own where the control stood, as a withdrawn question's
 * answer form gives way to a Reply form, or a live update taking a row away - is handed nowhere
 * (`leaveUncaused`): the focus stays on the body, as it did before the page had a rescue, and
 * nothing scrolls, since the page, not the reader, chose whatever stands near the lost place, and
 * their next key would act on it or be typed into it. Only an open modal dialog takes the focus
 * then, as its own stop, since a focus that leaves an open modal reaches the page behind it. Only
 * a region that has gone hands the focus out of it, and only for a loss the reader caused: from a
 * dialog or a panel that closed it goes only back to the place it was opened from - a list a pick
 * takes away to its trigger; a dialog opened from the body - a single-key shortcut with nothing
 * focused - leaves the focus on the body, where the reader was, and never puts it in a text box
 * whose next key would be typed. An element disabled under the focus, itself or through the
 * fieldset around it, gets the focus back when it is enabled again, watched rather than polled,
 * since its write's latency is the server's; if it goes instead, or is still disabled after
 * `RESCUE_WAIT_MS`, the focus is handed on as from an element that went. So it is for a control
 * disabled by an update the reader did not cause, the one such loss the focus leaves the body for
 * outside a modal: the very control that held the focus, back where it was, is no guess. It gets
 * the focus back only while the focus is still on the body, and only within that wait, never
 * after it. That settle too waits a frame and a task, so a component that watches the same
 * control acts on the change first. A pointer press anywhere while a check or a wait is out is
 * the reader moving on - onto a control, or onto the page itself - and ends it, so a control
 * enabled again later never pulls back a focus the reader left on the page. The reader's Escape
 * ends no check or wait already out; what it does is mark the loss it causes in its own task as
 * the reader leaving the control (`leaving`): a control it takes away or leaves - the issue
 * title's editor, which it closes - is not answered, and the focus stays where Escape left it
 * rather than going to what is drawn in its place, as the Inbox's and the Board's own Escape
 * leave it on the page.
 * The control a loss the reader caused lands on in the row that moved up, or walks back to, is
 * scrolled into view (`focusIntoView`); what stands where the element stood after a loss they
 * caused, a control given back, and a modal dialog's own stop are focused with no scroll.
 *
 * A loss the reader can no longer see is not answered: the element was scrolled out of their
 * sight - out of the viewport, or out of a scroll box around it - before it went, or before the
 * control a wait is on came back, and its region is still in the page. Every scroll that can move
 * the noted element, the page's or a box's around it, reads whether any of it is still in sight
 * (`Sight`), while a loss of it could still be answered; the document's capture listener hears a
 * box's scroll before the box's own listeners, so a virtual list that takes the row away in its
 * listener has it read while it is still in the page. So does every resize of the window, which
 * can move the element with nothing scrolled - a box sized to the window shrinks under its row,
 * which the virtual list may then stop drawing - read against every box around it that clips
 * it, since none of them has to have scrolled. An element no longer drawn is read no more: what
 * the last read before it went saw stands. Nothing scrolls and no control the reader did not
 * pick takes the focus, so neither a list nor the page is pulled back to a row the reader
 * scrolled away from - a live update taking that row away, say - and their next key acts on
 * nothing they did not choose. The focus stays on the body (`leaveUnseen`), except, for a loss
 * the reader caused, in a dialog or a panel (`DIALOG`), which takes it itself without a scroll,
 * and, for one they did not, in an open modal dialog, whose own stop takes it (`leaveUncaused`),
 * so the page behind a modal one is never reached. The record is the page's one (`outOfSight`),
 * which two component hand-offs read too (`useFocusPlace`, `SelectionBar`), so neither answers
 * such a loss before the rescue's check. Only an element a read has found in sight since it took
 * the focus can be read out of it (`readSight`): one focused below the fold, which a scroll the
 * page made before the engine's own left there, was never in the reader's sight.
 */
export function useFocusRescue(main: RefObject<HTMLElement | null>): void {
  useEffect(() => {
    rescueMounted = true;
    /** The element holding the focus, its region, the path it was focused on and where it stood
     *  as it took the focus (`spotsOf`), until it loses the focus by a `focusout`; one still noted
     *  here once it has left the page went with no `focusout` at all. */
    let focused: Pick<Loss, "element" | "path" | "region" | "spots"> | null = null;
    /** The loss each dialog or panel (`DIALOG`) the focus has entered was opened from, or null for
     *  one it entered from the body. */
    const openedFrom = new Map<Element, Loss | null>();
    /** Where a `focusout` is handing the focus, and the loss it leaves in another region. */
    let moving: { from: Loss | null; to: Element } | null = null;
    /** The check or the wait still out for the latest loss: what cancels it, and the set the
     *  page's watcher adds the page's new elements to while it is out (that loss's `arrived`). */
    let pending: { readonly arrived: Set<Node>; readonly cancel: () => void } | null = null;
    /** The arrivals of a loss kept only as a dialog's opener, which is never checked: none. */
    const noArrivals = new Set<Node>();
    /** Notes into `arrived` each element `records` added to the page from the record at `from`
     *  on, as one a loss's place may now hold. A node an earlier record of the same batch removed,
     *  and that had not arrived, is one the page moved - React moves a keyed node with
     *  `insertBefore`, which the batch reports as its removal and then its addition - so it stood
     *  in the page before the loss, and it is not noted. */
    const noteArrivals = (records: readonly MutationRecord[], arrived: Set<Node>, from = 0) => {
      const removed = new Set<Node>();
      for (const [at, each] of records.entries()) {
        if (at >= from) {
          for (const node of each.addedNodes) {
            if (node instanceof Element && !removed.has(node)) arrived.add(node);
          }
        }
        for (const node of each.removedNodes) {
          if (!arrived.has(node)) removed.add(node);
        }
      }
    };
    /** Whether the reader had scrolled `loss`'s element out of their sight before it went, in a
     *  region still in the page. */
    const unseen = (loss: Loss) => outOfSight(loss.element) && present(loss.region);

    /** Waits for `loss`'s element, disabled under the focus by the reader's press or by an update,
     *  to be enabled again or to go, for `RESCUE_WAIT_MS` at most: the wait settles then, and a
     *  control enabled again after that gets nothing back. Only `check` begins it, so a loss the
     *  reader left (`left`) never has one. */
    const wait = (loss: Loss): (() => void) => {
      const { element } = loss;
      /** Cancels the settle once it is scheduled. */
      let settling: (() => void) | undefined;
      const settle = () => {
        pending = null;
        if (!focusLost() || window.location.pathname !== loss.path) return;
        if (unseen(loss)) {
          leaveUnseen(loss);
          return;
        }
        // The very control back, whoever caused the loss, and for a loss the reader did not cause
        // the one answer outside a modal that is not the body. Still disabled as the wait runs
        // out, it takes no focus, and the loss is handed on as from a control that went.
        if (element.isConnected && rendered(element)) {
          if (loss.caused ? focusIntoView(element) : focusNoScroll(element)) return;
        }
        handOn(loss, main.current);
      };
      // The element enabled again, gone, or the wait run out: settled a frame and a task later,
      // as the first check is, so a component answering the same change - its own watcher,
      // started after this one, or an effect - moves the focus first. The first change of a burst
      // schedules it, and the watching ends there.
      const later = () => {
        if (settling !== undefined) return;
        watcher.disconnect();
        window.clearTimeout(bound);
        settling = afterComponents(settle);
      };
      // The whole document is watched, not only the element's own `disabled` and its
      // ancestors', so that the element going - with its dialog, say - ends the wait too.
      const watcher = new MutationObserver(() => {
        const held =
          element.isConnected &&
          element.matches(":disabled") &&
          element.closest("[hidden]") === null;
        if (!held) later();
      });
      const bound = window.setTimeout(later, RESCUE_WAIT_MS);
      watcher.observe(document.body, {
        attributeFilter: ["disabled", "hidden"],
        childList: true,
        subtree: true,
      });
      return () => {
        watcher.disconnect();
        window.clearTimeout(bound);
        settling?.();
      };
    };

    /** Answers `loss` a frame and a task after it, while the focus is still lost on the page it
     *  was lost on; a loss the reader left (`left`) it leaves where the reader put the focus. A
     *  control taken and back by then - hidden and drawn again, disabled and enabled again - gets
     *  the focus back, whoever caused the loss: for a loss the reader did not cause, the one
     *  answer outside a modal that is not the body (`leaveUncaused`). */
    const check = (loss: Loss) => {
      pending = null;
      if (loss.left || window.location.pathname !== loss.path || !focusLost()) return;
      const { element } = loss;
      if (unseen(loss)) {
        leaveUnseen(loss);
      } else if (!element.isConnected || !rendered(element)) {
        handOn(loss, main.current);
      } else if (element.matches(":disabled")) {
        pending = { arrived: loss.arrived, cancel: wait(loss) };
      } else if (loss.taken) {
        if (loss.caused) focusIntoView(element);
        else focusNoScroll(element);
      }
    };

    const noteFocus = (event: globalThis.FocusEvent) => {
      const element = event.target;
      if (!(element instanceof HTMLElement)) return;
      const region = regionOf(element, main.current);
      if (region.matches(DIALOG) && !openedFrom.has(region)) {
        for (const dialog of openedFrom.keys()) {
          if (!dialog.isConnected) openedFrom.delete(dialog);
        }
        openedFrom.set(region, moving?.to === element ? moving.from : null);
      }
      moving = null;
      focused = {
        element,
        path: window.location.pathname,
        region,
        spots: spotsOf(element, region),
      };
      // The same element taking the focus again - the window given back the focus - keeps what
      // the scrolls since it was first noted saw. A new one is read where it took the focus,
      // before the engine has scrolled it into view.
      if (sight?.element !== element) {
        const boxes = new Set<Element>();
        sight = { boxes, element, seen: true, shown: inSight(element, boxes) };
      }
    };

    const record = (event: globalThis.FocusEvent) => {
      const element = event.target;
      if (!(element instanceof HTMLElement)) return;
      const path = focused?.element === element ? focused.path : window.location.pathname;
      focused = null;
      pending?.cancel();
      pending = null;
      const region = regionOf(element, main.current);
      const next = event.relatedTarget;
      if (leaving && !(next instanceof Element)) return;
      // A move from one control to another in the same region - j and k, the arrows - is no
      // loss, and counts nothing.
      if (next instanceof Element && regionOf(next, main.current) === region) {
        moving = { from: null, to: next };
        return;
      }
      const fields = {
        caused: causedByReader(element),
        element,
        index: countBefore(drawnIn(region, main.current), element),
        left: false,
        opener: openedFrom.get(region) ?? null,
        path,
        region,
        taken: !stands(element),
      };
      // A focus going straight to another region is kept only as the place it left: the opener
      // of the dialog it is entering, whose spots, arrivals and row places the hand-off never
      // reads.
      if (next instanceof Element) {
        moving = { from: { ...fields, arrived: noArrivals, rowPlaces: [], spots: [] }, to: next };
        return;
      }
      // What the page changed before this loss is none of what a press draws in its place: those
      // records are dropped unread, and only the ones after it are noted as arrivals.
      watcher.takeRecords();
      const loss: Loss = {
        ...fields,
        arrived: new Set<Node>(),
        rowPlaces: rowPlacesOf(element, region),
        spots: spotsOf(element, region),
      };
      pending = { arrived: loss.arrived, cancel: afterComponents(() => check(loss)) };
      // The reader leaving the control - their Enter that commits it with the page's own
      // `blur()`, their click on the page's text - is no loss: the control still stands in the
      // page, enabled and drawn, once the event that moved the focus has run, and is marked left
      // then. A control a commit takes away fires Chromium's `focusout` during the removal
      // itself, so by this microtask it has gone, and its loss stays one to answer.
      if (!loss.taken) {
        queueMicrotask(() => {
          loss.left = element.isConnected && stands(element);
        });
      }
    };

    // A focused element taken away with no `focusout` - Firefox's and WebKit's way, for the
    // element or a parent of it - is read off the last mutation that took it, at the place it was
    // taken from, or at the region's end where no record of this batch names it. Every batch
    // that comes while a loss's check or wait is out is noted first as that loss's arrivals.
    const watcher = new MutationObserver((records) => {
      if (pending !== null) noteArrivals(records, pending.arrived);
      const held = focused;
      if (held === null || held.element.isConnected) return;
      focused = null;
      if (leaving || !focusLost()) return;
      const removal = removalOf(records, held.element);
      const index =
        removal === undefined || !held.region.isConnected
          ? drawnIn(held.region, main.current).length
          : placeOfRemoval(held.region, removal.spot, main.current);
      pending?.cancel();
      // What this batch added from the last record that took the element on is what can stand in
      // its place; what it added before that record is not.
      const arrived = new Set<Node>();
      noteArrivals(records, arrived, removal?.at ?? 0);
      const loss: Loss = {
        arrived,
        caused: causedByReader(held.element),
        ...held,
        index,
        left: false,
        opener: openedFrom.get(held.region) ?? null,
        rowPlaces: rowPlacesOf(held.element, held.region),
        spots: removal === undefined ? held.spots : [removal.spot, ...held.spots],
        taken: true,
      };
      pending = { arrived, cancel: afterComponents(() => check(loss)) };
    });
    watcher.observe(document.body, { childList: true, subtree: true });
    /** The record a scroll or a resize reads into: the noted element's, while a loss of it could
     *  still be answered - it holds the focus, or a check or a wait is out - and while the page
     *  still draws it. A hidden element's box is the viewport's top-left corner, which would read
     *  as in sight, so what the last read before it went saw stands. */
    const readable = (): Sight | null => {
      const watched = sight;
      if (watched === null || (focused === null && pending === null)) return null;
      return watched.element.isConnected && rendered(watched.element) ? watched : null;
    };
    // A scroll that can move the noted element reads whether the reader can still see it. A box's
    // `scroll` does not bubble; the document hears it in its capture phase, before the box's own
    // listeners.
    const noteScroll = (event: Event) => {
      const watched = readable();
      if (watched === null) return;
      const { target } = event;
      if (target instanceof Element) {
        if (!target.contains(watched.element)) return;
        watched.boxes.add(target);
      }
      readSight(watched);
    };
    // A resize moves the noted element with nothing scrolled - a shorter window, a zoom, a box
    // sized to the window shrinking under it - so it reads against every box around it that clips
    // it, none of which has to have scrolled, and keeps those boxes for the scrolls after it.
    const noteResize = () => {
      const watched = readable();
      if (watched === null) return;
      for (const box of clippingBoxes(watched.element)) watched.boxes.add(box);
      readSight(watched);
    };
    /** Records the reader's own act, a press (`isPress`) - a pointer press, a key that acts rather
     *  than edits a field's text, or a pick, which Playwright and assistive tech make with an
     *  `input` and a `change` alone - so a loss while it is live can be told from one an update
     *  made under them (`causedByReader`), with the dialog or panel it landed in, the control that
     *  opened it (`openerOf`) and the loss that region was opened from, read now, while the region
     *  is still in the page, for when a pick or a confirm takes it away (`pressedOn`), the controls
     *  it can read as that are already held (`heldAt`), read before the act runs, and the release
     *  it waits for (`Release`). A key, a `change` or an `input` that is no press - a character
     *  typed in a field, the field's commit on blur, the `input` of its typing - neither starts an
     *  act nor ends the live one. A newer act ends the one before it, and the watch on that one's
     *  hold with it, except an act on a control the live act holds by a hold that began after it,
     *  or on anything inside that control (`onHeldControl`) - a double click, a repeated Enter, any
     *  other key or pointer press on that control or on a span or an icon of it: the held control
     *  takes the act without a write, so the act before it stays the cause, with its release and
     *  its write deadline as they were (`noteRelease`). Tab, with Shift or without, is the one key
     *  the rescue reads as a new act on a held control: it moves the focus off that control to
     *  another, and an act is the cause only of the loss of its own control and of what it takes
     *  away, so the Tab ends the act before it and is noted as a new act, and a loss of the control
     *  it moved the focus to is judged on its own. Other keys that move the focus away, such as the
     *  Inbox's j and k, are passed over like any other key on the held control. */
    const notePress = (event: Event) => {
      if (!isPress(event)) return;
      const movesOff = event instanceof KeyboardEvent && event.key === "Tab";
      if (!movesOff && lastPress?.live === true && onHeldControl(lastPress, event.target)) return;
      lastPress?.stop();
      const region = event.target.closest(DIALOG);
      const opener = region === null ? null : (openedFrom.get(region) ?? null);
      const control = openerOf(event.target);
      const release = RELEASE[event.type];
      lastPress = {
        control,
        heldBefore: heldAt(event.target, control, opener),
        live: true,
        opener,
        region,
        release: release === undefined ? null : { by: pressedBy(event), type: release },
        stop: () => undefined,
        target: event.target,
      };
      answerPress(lastPress);
    };
    /** The release of the reader's latest press - the `pointerup` of the pointer it was made by,
     *  the `keyup` of its key (`Release`) - which is what a click answers: a control pressed by the
     *  pointer, or by Space, acts on the release, however long the reader held it down. The press
     *  is live again from there, as if made then, on the control it was made on, and from that
     *  release alone, once: the release is spent then, so an act on the control the press holds,
     *  which the rescue does not note (`notePress`), and that act's release leave the press's
     *  watch and its write deadline as they were. */
    const noteRelease = (event: Event) => {
      const press = lastPress;
      if (press?.release?.type !== event.type || press.release.by !== pressedBy(event)) return;
      press.release = null;
      answerPress(press);
    };
    const readerMoved = (event: Event) => {
      notePress(event);
      pending?.cancel();
      pending = null;
    };
    /** Whether the reader's Escape is being answered: set by its `keydown` until the next task,
     *  by when the commit it caused has run. */
    let leaving = false;
    const noteKey = (event: KeyboardEvent) => {
      notePress(event);
      if (event.key !== "Escape") return;
      leaving = true;
      window.setTimeout(() => {
        leaving = false;
      }, 0);
    };
    document.addEventListener("focusin", noteFocus, true);
    document.addEventListener("focusout", record, true);
    document.addEventListener("pointerdown", readerMoved, true);
    document.addEventListener("pointerup", noteRelease, true);
    document.addEventListener("keydown", noteKey, true);
    document.addEventListener("keyup", noteRelease, true);
    document.addEventListener("change", notePress, true);
    document.addEventListener("input", notePress, true);
    document.addEventListener("scroll", noteScroll, { capture: true, passive: true });
    window.addEventListener("resize", noteResize);
    return () => {
      watcher.disconnect();
      document.removeEventListener("focusin", noteFocus, true);
      document.removeEventListener("focusout", record, true);
      document.removeEventListener("pointerdown", readerMoved, true);
      document.removeEventListener("pointerup", noteRelease, true);
      document.removeEventListener("keydown", noteKey, true);
      document.removeEventListener("keyup", noteRelease, true);
      document.removeEventListener("change", notePress, true);
      document.removeEventListener("input", notePress, true);
      document.removeEventListener("scroll", noteScroll, true);
      window.removeEventListener("resize", noteResize);
      pending?.cancel();
      sight = null;
      rescueMounted = false;
      lastPress?.stop();
      lastPress = null;
    };
  }, [main]);
}
