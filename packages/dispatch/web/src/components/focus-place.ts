import { type FocusEvent, useLayoutEffect, useRef } from "react";

import { focusOnDocument } from "../features/shell/roving";

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
 *  order), or -1: counted up to the element and no further. */
function placeIn(container: Element, element: Element): number {
  let index = 0;
  for (const control of container.querySelectorAll(FOCUSABLE)) {
    if (control.closest("[hidden]") !== null) continue;
    if (control === element) return index;
    index += 1;
  }
  return -1;
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
        const index = placeIn(held.container, held.element);
        if (index !== -1) held.index = index;
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
    const index = placeIn(container, element);
    place.current = { container, element, index: index === -1 ? (carried ?? -1) : index };
  };
}
