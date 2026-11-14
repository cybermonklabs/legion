import { type FocusEvent, useEffect, useLayoutEffect, useRef } from "react";

import { focusOnDocument } from "../features/shell/roving";
import { CONTROL } from "./control-names";

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
    const left = held.container.isConnected ? focusableIn(held.container) : [];
    (left[held.index] ?? left[held.index - 1] ?? fallback(held.container))?.focus();
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

/** An open modal dialog: the search bar, the Inbox drawer, every `useDialog` dialog. */
const MODAL_DIALOG = '[role="dialog"][aria-modal="true"]';

/** How long the rescue waits for a control a press disabled to be enabled again before it hands
 *  the focus on as if that control had gone: longer than a slow server takes to answer a write,
 *  and short enough that a write that never ends does not leave the reader on the page. */
const RESCUE_WAIT_MS = 10_000;

/** Whether the page draws `element`: under no `hidden` ancestor, and given a box. */
function rendered(element: Element): boolean {
  return element.closest("[hidden]") === null && element.getClientRects().length > 0;
}

/** A focus an element lost: the element, the region it was counted in, its place there
 *  (`placeIn`), and the path of the page it was lost on. */
interface Loss {
  readonly element: HTMLElement;
  readonly index: number;
  readonly path: string;
  readonly region: Element;
}

/** Runs `then` once the components have had their turn with a loss of focus: a frame and then a
 *  task later, after React has committed and run its passive effects. Answers what cancels it. */
function afterComponents(then: () => void): () => void {
  let task: number | undefined;
  const frame = window.requestAnimationFrame(() => {
    task = window.setTimeout(then, 0);
  });
  return () => {
    window.cancelAnimationFrame(frame);
    window.clearTimeout(task);
  };
}

/** Hands on the focus `loss`'s element took with it when it went: to the control now at its
 *  place in its region, else the one before it. A region that went with it - a dialog that
 *  closed - hands it to the last loss `lost` holds of a region still in the page, the region
 *  under it: to that element, the control that opened the dialog, if it can still take the
 *  focus, else the control now at its place, else the one before it. Then to the page's search
 *  box, then to its filter trigger. */
function handOn(loss: Loss, lost: ReadonlyMap<Element, Loss>): void {
  const placed = (at: Loss) => {
    const left = focusableIn(at.region);
    return [left[at.index], left[at.index - 1]];
  };
  let candidates: (HTMLElement | undefined)[] = [];
  if (loss.region.isConnected) {
    candidates = placed(loss);
  } else {
    for (const held of lost.values()) {
      if (held.region.isConnected && held.path === loss.path) {
        candidates = [held.element, ...placed(held)];
      }
    }
  }
  const main = document.getElementById("main-content");
  for (const candidate of [
    ...candidates,
    main?.querySelector<HTMLElement>(`[data-control="${CONTROL.searchBox}"]`),
    main?.querySelector<HTMLElement>(`[data-control="${CONTROL.panelTrigger}"]`),
  ]) {
    if (candidate === null || candidate === undefined) continue;
    if (!candidate.isConnected || !rendered(candidate)) continue;
    candidate.focus();
    if (document.activeElement === candidate) return;
  }
}

/**
 * The page's one focus rescue, mounted once in the app shell: after any press on any page, the
 * keyboard focus never stays on the page's body because the element holding it was disabled,
 * hidden, taken away or drawn again under a new key. A keyboard reader left on the body starts
 * again from the page's first link, a walk through everything above the control they were using.
 *
 * Every focus an element loses is recorded as it goes (`focusout`, in the capture phase, while
 * the element is still in the page): the element, its region (the open modal dialog holding it,
 * else the main content, else the document), its place among the region's controls, and the
 * page's path. Each region keeps its last loss, so a dialog that closes can hand the focus to the
 * place it was opened from. A new loss cancels whatever is still out for the one before it.
 *
 * The check runs a frame and a task after the loss, once React has committed and run its passive
 * effects, so every component's own hand-off has had its turn first: ControlRow's, DataTable's
 * and PanelTrigger's (`useFocusPlace`), SelectionBar's `returnFocus`, AskCard's, `useDialog`'s
 * return to the opener, the Agents composer's take-back. Each knows its own target better than a
 * place counted in the page does - the row marked last, the opener, the composer's box - so the
 * rescue answers only a loss none of them answered, and acts only while the focus is still on
 * the body. It leaves alone a loss on a page the reader has since left, since the new page owns
 * its focus, and a loss whose element is still in the page, enabled and drawn: the reader moved
 * off it - a click on the page itself, a deliberate `.blur()` such as the Inbox's and the Board's
 * Escape - so every place the rescue holds is forgotten.
 *
 * An element that went - taken away, hidden, no longer drawn, or replaced under a new key - hands
 * the focus on (`handOn`): to the control now at its place in its region, else the one before it;
 * from a dialog that closed, to the place it was opened from; then to the page's search box or
 * filter trigger, and never to an arbitrary first control of the page. An element the press
 * disabled, itself or through the fieldset around it, gets the focus back when it is enabled
 * again, watched rather than polled, since its write's latency is the server's; if it goes
 * instead, or is still disabled after `RESCUE_WAIT_MS`, the focus is handed on as from an element
 * that went. That settle too waits a frame and a task, so a component that watches the same
 * control - the Agents composer, giving its box the focus back - acts on the change first.
 * `focus()` scrolls the element it lands on into view.
 */
export function useFocusRescue(): void {
  useEffect(() => {
    /** Each region's last loss, the latest last. */
    const lost = new Map<Element, Loss>();
    /** Cancels the check or the wait still out for the latest loss. */
    let pending: (() => void) | null = null;

    /** Waits for `loss`'s element, which the press disabled, to be enabled again or to go. */
    const wait = (loss: Loss): (() => void) => {
      const { element } = loss;
      /** Cancels the settle once it is scheduled. */
      let settling: (() => void) | undefined;
      const settle = () => {
        pending = null;
        if (!focusOnDocument() || window.location.pathname !== loss.path) return;
        if (element.isConnected && rendered(element)) element.focus();
        if (document.activeElement !== element) handOn(loss, lost);
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
      if (window.location.pathname !== loss.path) {
        lost.clear();
        return;
      }
      if (!focusOnDocument()) return;
      const { element } = loss;
      if (!element.isConnected || !rendered(element)) {
        handOn(loss, lost);
      } else if (element.matches(":disabled")) {
        pending = wait(loss);
      } else {
        lost.clear();
      }
    };

    const record = (event: globalThis.FocusEvent) => {
      const element = event.target;
      if (!(element instanceof HTMLElement)) return;
      pending?.();
      pending = null;
      // The open modal dialog holding it, else the page's main content, else the document.
      const region =
        element.closest(MODAL_DIALOG) ?? element.closest("#main-content") ?? document.body;
      for (const held of lost.keys()) {
        if (!held.isConnected) lost.delete(held);
      }
      const loss: Loss = {
        element,
        index: placeIn(region, element).index,
        path: window.location.pathname,
        region,
      };
      lost.delete(region);
      lost.set(region, loss);
      // A focus going straight to another element is kept only as the place it left: the
      // opener's, while a dialog it opened holds the focus.
      if (event.relatedTarget === null) pending = afterComponents(() => check(loss));
    };
    document.addEventListener("focusout", record, true);
    return () => {
      document.removeEventListener("focusout", record, true);
      pending?.();
    };
  }, []);
}
