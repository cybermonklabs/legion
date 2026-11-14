import { type FocusEvent, type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import { focusOnDocument } from "../features/shell/roving";
import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { CONTROL, controlIn } from "./control-names";

/** What the keyboard focus can reach: a link, a form control that is not `disabled`, or anything
 *  given a Tab stop of its own. A control refused with `aria-disabled` is one, since it keeps its
 *  Tab stop. */
const FOCUSABLE = [
  "a[href]",
  "button:not(:disabled)",
  "textarea:not(:disabled)",
  "input:not(:disabled)",
  "select:not(:disabled)",
  '[tabindex]:not([tabindex="-1"])',
].join(", ");

/** The controls the focus can reach inside `container`, in order. One under a `hidden` ancestor -
 *  a composer kept mounted, out of sight, for a send it has out - takes no focus, so it is never
 *  where a dialog's trap starts or wraps, nor where a hand-off lands. */
export function focusableIn(container: Element): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(
    (element) => element.closest("[hidden]") === null
  );
}

/** Where a hand-off sends the focus a control took away with it: the control now at `index` in
 *  `controls`, else the one before it. */
function controlAt(controls: readonly HTMLElement[], index: number): HTMLElement | undefined {
  return controls[index] ?? controls[index - 1];
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
 * in its container, else to the one before it, else to what `fallback` names for that container
 * (a table row's fallback is the row itself, and a row that has left the page names none).
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
 * so it is forgotten, and its going later moves nothing.
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
    if (held.element.isConnected || !focusOnDocument()) return;
    const placed = held.container.isConnected
      ? controlAt(focusableIn(held.container), held.index)
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

/** A dialog, modal or not: the search bar, the Inbox drawer, every `useDialog` dialog, and the
 *  panel a `PanelTrigger` opens. */
const DIALOG = '[role="dialog"]';

/** How long the rescue waits for a control a press disabled to be enabled again before it hands
 *  the focus on as if that control had gone: the app's write deadline, past which a write is
 *  late and a composer lets go of what it holds the reader to. A slow send never ends the wait
 *  before its composer's own take-back. */
const RESCUE_WAIT_MS = SEND_DEADLINE_MS;

/** Whether the page draws `element`: under no `hidden` ancestor, and given a box. */
function rendered(element: Element): boolean {
  return element.closest("[hidden]") === null && element.getClientRects().length > 0;
}

/** The controls the focus can reach in `region` that the page draws. The rescue counts places
 *  among these and lands only on these, so a control drawn at one width alone - `md:hidden`, say
 *  - neither holds a place nor is handed a focus it cannot show. */
function drawnIn(region: Element): HTMLElement[] {
  return focusableIn(region).filter(rendered);
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
 *  update (the HTML spec's focus fixup), so until then the document still names it. */
function focusLost(): boolean {
  const focused = document.activeElement;
  return focused === null || focused === document.body || !focused.isConnected;
}

/** Focuses `element` and, once it holds the focus, scrolls it into view: WebKit scrolls a focused
 *  control into view only a frame later, by when a list's own scroll keeping may have moved the
 *  page again. Answers whether it took the focus. */
function land(element: HTMLElement): boolean {
  element.focus();
  if (document.activeElement !== element) return false;
  element.scrollIntoView({ block: "nearest", inline: "nearest" });
  return true;
}

/** A focus an element lost: the element, the region it was counted in, its place there among
 *  the controls the page draws, the path of the page it was focused on, whether the page took it
 *  from the reader (`taken`: the element was disabled, hidden or taken away) rather than the
 *  reader moving off it, and - in a dialog - the loss the dialog was opened from (`opener`):
 *  the place the focus left for it, or null for a dialog the focus entered from the body. */
interface Loss {
  readonly element: HTMLElement;
  readonly index: number;
  readonly opener: Loss | null;
  readonly path: string;
  readonly region: Element;
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

/** The record of `records` that took `element` away, itself or with a parent, read at a point
 *  still in the page: where the parent it was taken from went too, later in the same batch, the
 *  record that took that parent. */
function removalOf(
  records: readonly MutationRecord[],
  element: Element
): MutationRecord | undefined {
  let node: Node = element;
  let found: MutationRecord | undefined;
  for (;;) {
    const record = records.find((each) =>
      [...each.removedNodes].some((removed) => removed.contains(node))
    );
    if (record === undefined) return found;
    found = record;
    if (record.target.isConnected) return record;
    node = record.target;
  }
}

/** The place of a control `record` took away, itself or with a parent, among the controls
 *  `region` draws: how many of them come before the point it was taken from - before the sibling
 *  after it while that is still in the page, else up to and through the sibling before it, else
 *  through the parent it was taken from. */
function placeOfRemoval(region: Element, record: MutationRecord): number {
  const next = record.nextSibling?.isConnected ? record.nextSibling : null;
  const through = record.previousSibling?.isConnected ? record.previousSibling : record.target;
  let index = 0;
  for (const control of drawnIn(region)) {
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

/** Hands on the focus `loss`'s element took with it when it went. In a region still in the page:
 *  to the control now at its place there, else the one before it, else the page's search box,
 *  else its filter trigger. From a region that went with it - a dialog that closed - only back
 *  to the place the dialog was opened from (its `opener`, or that dialog's own opener where it
 *  closed too): that element, the control that opened it, if it can still take the focus, else
 *  the control now at its place, else the one before it. A dialog opened from the body hands the
 *  focus nowhere: the body is where the reader was. */
function handOn(loss: Loss, main: HTMLElement | null): void {
  let candidates: (HTMLElement | null | undefined)[] = [];
  if (loss.region.isConnected) {
    candidates = [
      controlAt(drawnIn(loss.region), loss.index),
      main === null ? null : controlIn(main, CONTROL.searchBox),
      main === null ? null : controlIn(main, CONTROL.panelTrigger),
    ];
  } else {
    let opener = loss.opener;
    while (opener !== null && !opener.region.isConnected) opener = opener.opener;
    if (opener !== null && opener.path === loss.path) {
      candidates = [opener.element, controlAt(drawnIn(opener.region), opener.index)];
    }
  }
  for (const candidate of candidates) {
    if (candidate === null || candidate === undefined) continue;
    if (!candidate.isConnected || !rendered(candidate)) continue;
    if (land(candidate)) return;
  }
}

/**
 * The page's one focus rescue, mounted once in the app shell: after any loss of the focus to the
 * body - a press, or an update that takes the focused control away - the keyboard focus never
 * stays on the page's body because the element holding it was disabled, hidden, taken away or
 * drawn again under a new key. A keyboard reader left on the body starts again from the page's
 * first link, a walk through everything above the control they were using.
 *
 * Every element that takes the focus is noted as it does (`focusin`), with its region - the
 * dialog holding it, modal or not, else the shell's main content (`main`), else the document -
 * and the path of the page it was focused on, which a navigation does not change before the old
 * page goes. A dialog the focus enters for the first time notes where it came from: the loss of
 * the element that handed it over, or none when it came from the body. A loss is seen two ways.
 * A control disabled, hidden or left by the reader fires a `focusout` in every engine, while it is
 * still in the page. A control taken away, itself or with a parent, is seen by watching the page
 * (a MutationObserver over the document) in every engine, since Firefox and WebKit fire no
 * `focusout` for it; Chromium fires one as well, and whichever comes first is the loss recorded.
 * Each loss holds its element, its region, its place among the controls the region draws, the
 * path, whether the page took the element (`taken`), and its dialog's opener. A move from one
 * control to another in the same region is no loss. A new loss cancels whatever is still out for
 * the one before it.
 *
 * The check runs a frame and a task after the loss, once React has committed and run its passive
 * effects, so every component's own hand-off has had its turn first: every `useFocusPlace`
 * hand-off (ControlRow, DataTable, PanelTrigger and the pages' own), SelectionBar's
 * `returnFocus`, AskCard's, `useDialog`'s return to the opener, the Agents composer's take-back.
 * Each knows its own target better than a place counted in the page does - the row marked last,
 * the opener, the composer's box - so the rescue answers only a loss none of them answered, and
 * acts only while the focus is still lost. It leaves alone a loss on a page the reader has since
 * left, since the new page owns its focus, and a loss the reader made whose element is still in
 * the page, enabled and drawn - a click on the page itself, a deliberate `.blur()` such as the
 * Inbox's and the Board's Escape. A taken element that is back by then gets the focus back.
 *
 * An element that went - taken away, hidden, no longer drawn, or replaced under a new key - hands
 * the focus on (`handOn`): to the control now at its place in its region, else the one before it,
 * then to the page's search box or filter trigger, and never to an arbitrary first control of the
 * page. From a dialog that closed it goes only back to the place the dialog was opened from; a
 * dialog opened from the body - a single-key shortcut with nothing focused - leaves the focus on
 * the body, where the reader was, and never puts it in a text box whose next key would be typed.
 * An element the press disabled, itself or through the fieldset around it, gets the focus back
 * when it is enabled again, watched rather than polled, since its write's latency is the
 * server's; if it goes instead, or is still disabled after `RESCUE_WAIT_MS`, the focus is handed
 * on as from an element that went. That settle too waits a frame and a task, so a component that
 * watches the same control - the Agents composer, giving its box the focus back - acts on the
 * change first. A pointer press anywhere while a check or a wait is out is the reader moving on -
 * onto a control, or onto the page itself - and ends it, so a control enabled again later never
 * pulls back a focus the reader left on the page. So is the reader's Escape: a control it takes
 * away or leaves - the issue title's editor, whose heading would open it again if it took the
 * focus - is the reader leaving it, and the focus stays where Escape left it, as the Inbox's and
 * the Board's own Escape leave it on the page. The element the rescue lands on is scrolled into
 * view (`land`).
 */
export function useFocusRescue(main: RefObject<HTMLElement | null>): void {
  useEffect(() => {
    /** The element holding the focus, its region and the path it was focused on, until it loses
     *  the focus by a `focusout`; one still noted here once it has left the page went with no
     *  `focusout` at all. */
    let focused: { element: HTMLElement; path: string; region: Element } | null = null;
    /** The loss each dialog the focus has entered was opened from, or null for one it entered
     *  from the body. */
    const openedFrom = new Map<Element, Loss | null>();
    /** Where a `focusout` is handing the focus, and the loss it leaves in another region. */
    let moving: { from: Loss | null; to: Element } | null = null;
    /** Cancels the check or the wait still out for the latest loss. */
    let pending: (() => void) | null = null;

    const regionOf = (element: Element): Element => {
      const content = main.current;
      return element.closest(DIALOG) ?? (content?.contains(element) ? content : document.body);
    };

    /** Waits for `loss`'s element, which the press disabled, to be enabled again or to go. */
    const wait = (loss: Loss): (() => void) => {
      const { element } = loss;
      /** Cancels the settle once it is scheduled. */
      let settling: (() => void) | undefined;
      const settle = () => {
        pending = null;
        if (!focusLost() || window.location.pathname !== loss.path) return;
        if (element.isConnected && rendered(element) && land(element)) return;
        handOn(loss, main.current);
      };
      // The element enabled again, gone, or the wait run out: settled a frame and a task later,
      // as the first check is, so a component answering the same change - its own watcher,
      // started after this one, such as the Agents composer's take-back, or an effect - moves
      // the focus first. The first change of a burst schedules it, and the watching ends there.
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

    const check = (loss: Loss) => {
      pending = null;
      if (window.location.pathname !== loss.path || !focusLost()) return;
      const { element } = loss;
      if (!element.isConnected || !rendered(element)) {
        handOn(loss, main.current);
      } else if (element.matches(":disabled")) {
        pending = wait(loss);
      } else if (loss.taken) {
        land(element);
      }
    };

    const noteFocus = (event: globalThis.FocusEvent) => {
      const element = event.target;
      if (!(element instanceof HTMLElement)) return;
      const region = regionOf(element);
      if (region.matches(DIALOG) && !openedFrom.has(region)) {
        for (const dialog of openedFrom.keys()) {
          if (!dialog.isConnected) openedFrom.delete(dialog);
        }
        openedFrom.set(region, moving?.to === element ? moving.from : null);
      }
      moving = null;
      focused = { element, path: window.location.pathname, region };
    };

    const record = (event: globalThis.FocusEvent) => {
      const element = event.target;
      if (!(element instanceof HTMLElement)) return;
      const path = focused?.element === element ? focused.path : window.location.pathname;
      focused = null;
      pending?.();
      pending = null;
      const region = regionOf(element);
      const next = event.relatedTarget;
      if (leaving && !(next instanceof Element)) return;
      // A move from one control to another in the same region - j and k, the arrows - is no
      // loss, and counts nothing.
      if (next instanceof Element && regionOf(next) === region) {
        moving = { from: null, to: next };
        return;
      }
      const loss: Loss = {
        element,
        index: countBefore(drawnIn(region), element),
        opener: openedFrom.get(region) ?? null,
        path,
        region,
        taken: element.matches(":disabled") || !rendered(element),
      };
      // A focus going straight to another region is kept only as the place it left: the opener
      // of the dialog it is entering.
      if (next instanceof Element) moving = { from: loss, to: next };
      else pending = afterComponents(() => check(loss));
    };

    // A focused element taken away with no `focusout` - Firefox's and WebKit's way, for the
    // element or a parent of it - is read off the mutation that took it, at the place it was
    // taken from, or at the region's end where no record of this batch names it.
    const watcher = new MutationObserver((records) => {
      const held = focused;
      if (held === null || held.element.isConnected) return;
      focused = null;
      if (leaving || !focusLost()) return;
      const removal = removalOf(records, held.element);
      const index =
        removal === undefined || !held.region.isConnected
          ? drawnIn(held.region).length
          : placeOfRemoval(held.region, removal);
      pending?.();
      const loss: Loss = {
        ...held,
        index,
        opener: openedFrom.get(held.region) ?? null,
        taken: true,
      };
      pending = afterComponents(() => check(loss));
    });
    watcher.observe(document.body, { childList: true, subtree: true });
    const readerMoved = () => {
      pending?.();
      pending = null;
    };
    /** Whether the reader's Escape is being answered: set by its `keydown` until the next task,
     *  by when the commit it caused has run. */
    let leaving = false;
    const noteEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      leaving = true;
      window.setTimeout(() => {
        leaving = false;
      }, 0);
    };
    document.addEventListener("focusin", noteFocus, true);
    document.addEventListener("focusout", record, true);
    document.addEventListener("pointerdown", readerMoved, true);
    document.addEventListener("keydown", noteEscape, true);
    return () => {
      watcher.disconnect();
      document.removeEventListener("focusin", noteFocus, true);
      document.removeEventListener("focusout", record, true);
      document.removeEventListener("pointerdown", readerMoved, true);
      document.removeEventListener("keydown", noteEscape, true);
      pending?.();
    };
  }, [main]);
}
