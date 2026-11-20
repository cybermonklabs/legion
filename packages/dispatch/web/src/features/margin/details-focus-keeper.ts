import { type RefObject, useEffect } from "react";

/** What can hold the focus inside Details: the controls a reader reaches, and any element given a
 *  `tabindex`. */
const CONTROLS = "a[href], button, input, select, textarea, [tabindex]";

/** The `overflow` values that cut what an element draws down to its own box. */
const CLIPPING: Readonly<Record<string, true>> = {
  auto: true,
  clip: true,
  hidden: true,
  scroll: true,
};

/** The `overflow` values of a box that scrolls its content. */
const SCROLLING: Readonly<Record<string, true>> = {
  auto: true,
  scroll: true,
};

/** The `position` values that pin an element to the screen or to the box it scrolls in, so that
 *  it stands in sight, at no distance, wherever the reader has scrolled to: the drawer's resize
 *  handle, as tall as the screen, for one. */
const PINNED: Readonly<Record<string, true>> = {
  fixed: true,
  sticky: true,
};

/** What the keeper hears: the two attributes that take a control from the reader, and every
 *  element added or removed. */
const WATCHED: MutationObserverInit = {
  attributeFilter: ["disabled", "hidden"],
  attributes: true,
  childList: true,
  subtree: true,
};

/** The attribute each entry of the new issue page's history carries, its own id there, by which
 *  the keeper tells a redrawn control of one entry from a control of another
 *  (`ControlIdentity`). */
export const HISTORY_ENTRY_ATTRIBUTE = "data-history-entry";

/** What tells a control apart across a redraw, where React draws it anew as another element: the
 *  history entry it stands in, its own `id`, its tag, its role and its name, in one key. */
type ControlIdentity = string;

/** A stretch of the window from `top` down to `bottom`, in CSS pixels. */
interface Span {
  readonly bottom: number;
  readonly top: number;
}

/** A rectangle of the window, in CSS pixels. */
interface Bounds extends Span {
  readonly left: number;
  readonly right: number;
}

/** What the keeper reads of an element between a control and the box, once per search: the part
 *  of the window it cuts what it draws down to, where it cuts it (`CLIPPING`), and whether it is
 *  pinned (`PINNED`). */
interface Frame {
  readonly clip: Bounds | null;
  readonly pinned: boolean;
}

/** How the control that held the focus lost it. */
type Loss = "disabled" | "hidden" | "removed";

/**
 * Keeps a keyboard reader's focus inside Details while `active` holds: when the control that has
 * the focus is removed, turns disabled or is no longer drawn, and the focus has fallen to the
 * page, the keeper gives it to another place in `box`. Chromium drops the focus of a button that
 * turns disabled, or is removed, to the body. Without the keeper, a reader whose Retry goes with
 * its line once its read answers, or whose Load older goes away with the log's last page, starts
 * again from the top of the page.
 *
 * The focus goes, in this order: to the control that replaced the lost one, a control drawn in the
 * same change with the same identity (`ControlIdentity`), as when React draws an entry anew under
 * a new key; else to the control nearest to where the lost one stood, among those that stay
 * enabled, drawn and in sight and are not pinned (`PINNED`), looked for first inside the box that
 * scrolls the lost one, Details' body, and only where that holds none in the rest of the box; else
 * to the box's heading, its first `h2`.
 *
 * It is a backstop under each site's own hand-on, not a replacement for it. It hears a change
 * through one MutationObserver on the box, and through a `focusout` that names no next element,
 * which it reads a microtask later, and it moves the focus one task after that. A site hands the
 * focus on in an effect of the commit that changed the DOM. A layout effect runs before the
 * observer's callback. A passive effect runs after it when the change came from outside an event,
 * a save that lands, but in a task React queued as the commit ended, before the keeper queued its
 * own. So the site's choice already holds the focus when the keeper moves: the keeper moves only a
 * focus still on the page, or still on the disabled or hidden control itself, which Chromium moves
 * to the page a moment later. A focus the reader puts anywhere else, inside the box or outside it,
 * stays where they put it.
 *
 * It does nothing while `active` is false or the box is not drawn, and nothing while the focus is
 * outside the box: its observer runs only while a control inside the box holds the focus.
 */
export function useDetailsFocusKeeper(box: RefObject<HTMLElement | null>, active: boolean): void {
  useEffect(() => {
    const root = box.current;
    if (!active || root === null) return;
    return keepFocusIn(root);
  }, [active, box]);
}

/** Keeps the focus inside `root` (`useDetailsFocusKeeper`) until the returned function is called. */
function keepFocusIn(root: HTMLElement): () => void {
  // The control inside the box that holds the focus, with its identity as it was when it took it.
  let held: { readonly control: HTMLElement; readonly identity: ControlIdentity } | null = null;
  // The move the keeper makes a task after the held control was lost: every record heard since,
  // and the timer that makes it.
  let pending: { readonly records: MutationRecord[]; readonly timer: number } | null = null;
  const observer = new MutationObserver((records) => keep(records, false));
  const listening = new AbortController();

  function note(target: EventTarget | null): void {
    // The box itself holding the focus - the phone's sheet - leaves no control to keep.
    if (!(target instanceof HTMLElement) || target === root) {
      forget();
      return;
    }
    cancel();
    held = { control: target, identity: identityOf(target) };
    observer.observe(root, WATCHED);
  }

  function forget(): void {
    cancel();
    held = null;
    observer.disconnect();
  }

  function cancel(): void {
    if (pending === null) return;
    window.clearTimeout(pending.timer);
    pending = null;
  }

  // `measure` reads whether the held control is still drawn, a layout read the observer makes only
  // when a `hidden` changed around it.
  function keep(records: readonly MutationRecord[], measure: boolean): void {
    const focus = document.activeElement;
    const onPage = focus === null || focus === document.body;
    if (!onPage && !root.contains(focus)) {
      forget();
      return;
    }
    if (held === null) return;
    const { control } = held;
    const loss = lossOf(
      control,
      root,
      records,
      measure ||
        records.some(
          (record) => record.attributeName === "hidden" && record.target.contains(control)
        )
    );
    if (loss === null) {
      // The control stands and the focus is on the page: the reader put it there.
      if (onPage) forget();
      return;
    }
    // Off the page, the focus is the keeper's to move only while it is still on the disabled or
    // hidden control itself; a focus a site or the reader put anywhere else stays.
    if (!onPage && (focus !== control || loss === "removed")) return;
    // A site's passive effect, where the change came from outside an event, runs after this, in a
    // task React queued as the commit ended: the keeper moves in a task of its own, queued behind
    // that one, with every record it heard until then.
    if (pending === null) {
      pending = { records: [...records], timer: window.setTimeout(move, 0) };
    } else {
      pending.records.push(...records);
    }
  }

  function move(): void {
    if (pending === null || held === null) return;
    const { records } = pending;
    pending = null;
    const { control, identity } = held;
    // A focus a site's passive effect or the reader put somewhere since stays where they put it,
    // and so does one still on the lost control once it stands again.
    const focus = document.activeElement;
    if (
      focus !== null &&
      focus !== document.body &&
      (focus !== control || lossOf(control, root, [], true) === null)
    ) {
      return;
    }
    if (!drawn(root)) return;
    const removal = removalOf(control, records);
    const stood = placeOf(control, root, removal);
    const target =
      replacementOf(identity, root, records, stood) ??
      nearestTo(stood, root, scrollerOf(removal?.target ?? control.parentElement, root)) ??
      headingOf(root);
    target?.focus({ preventScroll: true });
  }

  root.addEventListener("focusin", (event) => note(event.target), { signal: listening.signal });
  root.addEventListener(
    "focusout",
    (event) => {
      const next = event.relatedTarget;
      if (next instanceof Node && root.contains(next)) return;
      if (next !== null) {
        forget();
        return;
      }
      // Read once the change that blurred it is done; the observer's records not yet delivered
      // are read with it, so a removed control's place is known here too.
      queueMicrotask(() => {
        if (held !== null) keep(observer.takeRecords(), true);
      });
    },
    { signal: listening.signal }
  );
  // A focus that came into the box before the keeper listened: the drawer's own Close, which the
  // commit that opens Details focuses.
  if (root.contains(document.activeElement)) note(document.activeElement);
  return () => {
    listening.abort();
    forget();
  };
}

function lossOf(
  control: HTMLElement,
  root: HTMLElement,
  records: readonly MutationRecord[],
  measure: boolean
): Loss | null {
  if (!root.contains(control)) return "removed";
  if (control.matches(":disabled")) return "disabled";
  if (measure && !drawn(control)) return "hidden";
  // Taken out and put back, as a keyed list drawn in a new order moves its elements: Chromium
  // drops the focus of a moved element as it does a removed one's.
  return removalOf(control, records) === undefined ? null : "removed";
}

function drawn(element: Element): boolean {
  return element.getClientRects().length > 0;
}

/** The last record that took `control`, or an element around it, out of the DOM. */
function removalOf(control: Node, records: readonly MutationRecord[]): MutationRecord | undefined {
  let removal: MutationRecord | undefined;
  for (const record of records) {
    for (const removed of record.removedNodes) {
      if (removed.contains(control)) {
        removal = record;
        break;
      }
    }
  }
  return removal;
}

function identityOf(control: HTMLElement): ControlIdentity {
  return JSON.stringify([
    control.closest(`[${HISTORY_ENTRY_ATTRIBUTE}]`)?.getAttribute(HISTORY_ENTRY_ATTRIBUTE) ?? null,
    control.id,
    control.tagName,
    control.getAttribute("role"),
    control.getAttribute("aria-label") ?? (control.textContent ?? "").trim(),
  ]);
}

/** Where the lost control stood. A removed one's place is the gap its `removal` left between the
 *  drawn elements beside it; a disabled one is still drawn where it was; a hidden one stands where
 *  the outermost hidden element around it does among its siblings. */
function placeOf(
  control: HTMLElement,
  root: HTMLElement,
  removal: MutationRecord | undefined
): Span | null {
  if (removal !== undefined) {
    return between(removal.previousSibling, removal.nextSibling, removal.target, root);
  }
  if (!root.contains(control)) return null;
  if (drawn(control)) return control.getBoundingClientRect();
  let outer: Element = control;
  while (
    outer.parentElement !== null &&
    outer.parentElement !== root &&
    !drawn(outer.parentElement)
  ) {
    outer = outer.parentElement;
  }
  return between(outer.previousSibling, outer.nextSibling, outer.parentNode ?? root, root);
}

function between(
  before: Node | null,
  after: Node | null,
  parent: Node,
  root: HTMLElement
): Span | null {
  const above = drawnSibling(before, "previousSibling")?.getBoundingClientRect().bottom;
  const below = drawnSibling(after, "nextSibling")?.getBoundingClientRect().top;
  if (above !== undefined && below !== undefined) {
    return { bottom: Math.max(above, below), top: Math.min(above, below) };
  }
  const edge = above ?? below ?? drawnAround(parent, root)?.getBoundingClientRect().top;
  return edge === undefined ? null : { bottom: edge, top: edge };
}

function drawnSibling(start: Node | null, step: "nextSibling" | "previousSibling"): Element | null {
  for (let node = start; node !== null; node = node[step]) {
    if (node instanceof Element && node.isConnected && drawn(node)) return node;
  }
  return null;
}

/** The nearest drawn element at or around `start`, no further out than `root`. */
function drawnAround(start: Node, root: HTMLElement): Element | null {
  for (
    let node: Node | null = start;
    node !== null;
    node = node === root ? null : node.parentNode
  ) {
    if (node instanceof Element && node.isConnected && drawn(node)) return node;
  }
  return null;
}

/** How far `rect` stands above or below `stood`: none where the two overlap. */
function distance(rect: Span, stood: Span): number {
  return Math.max(0, stood.top - rect.bottom, rect.top - stood.bottom);
}

/** The control drawn in this change with the lost one's identity, nearest to where it stood. */
function replacementOf(
  identity: ControlIdentity,
  root: HTMLElement,
  records: readonly MutationRecord[],
  stood: Span | null
): HTMLElement | null {
  let replacement: HTMLElement | null = null;
  let nearest = Number.POSITIVE_INFINITY;
  for (const record of records) {
    for (const added of record.addedNodes) {
      if (!(added instanceof Element) || !root.contains(added)) continue;
      for (const control of [added, ...added.querySelectorAll(CONTROLS)]) {
        if (
          !(control instanceof HTMLElement) ||
          !control.matches(CONTROLS) ||
          control.matches(":disabled") ||
          !drawn(control) ||
          identityOf(control) !== identity
        ) {
          continue;
        }
        const away = stood === null ? 0 : distance(control.getBoundingClientRect(), stood);
        if (away < nearest) {
          nearest = away;
          replacement = control;
        }
      }
    }
  }
  return replacement;
}

/** The control in the reader's tab order, enabled, in sight and not pinned, nearest to where the
 *  lost one stood; the first in the box's order of those as near. It is looked for first inside
 *  `scroller`, the box that scrolled the lost one, and only where that holds none in the rest of
 *  the box, so the reader's next Tab goes on from their place in Details' body rather than from
 *  its head, from which it goes to the body's first control and scrolls the body back to its top. */
function nearestTo(
  stood: Span | null,
  root: HTMLElement,
  scroller: Element | null
): HTMLElement | null {
  if (stood === null) return null;
  const sight = sightOf(root);
  const frames = new Map<Element, Frame>();
  const nearestIn = (scope: Element, searched: Element | null): HTMLElement | null => {
    let nearest: HTMLElement | null = null;
    let best = Number.POSITIVE_INFINITY;
    for (const control of scope.querySelectorAll(CONTROLS)) {
      if (
        !(control instanceof HTMLElement) ||
        control.tabIndex < 0 ||
        control.matches(":disabled") ||
        !drawn(control) ||
        searched?.contains(control) === true
      ) {
        continue;
      }
      const rect = control.getBoundingClientRect();
      const away = distance(rect, stood);
      if (away < best && inSightUnpinned(control, rect, root, sight, frames)) {
        best = away;
        nearest = control;
      }
    }
    return nearest;
  };
  return (scroller === null ? null : nearestIn(scroller, null)) ?? nearestIn(root, scroller);
}

/** The nearest box at or around `start` that scrolls its content, inside the box and short of it:
 *  Details' body, for a control drawn in it. Null where none stands between `start` and the box. */
function scrollerOf(start: Node | null, root: HTMLElement): Element | null {
  if (start === null || !root.contains(start)) return null;
  for (
    let node = start instanceof Element ? start : start.parentElement;
    node !== null && node !== root;
    node = node.parentElement
  ) {
    const style = getComputedStyle(node);
    if (SCROLLING[style.overflowY] === true || SCROLLING[style.overflowX] === true) return node;
  }
  return null;
}

/** The part of the window the box covers. */
function sightOf(root: HTMLElement): Bounds {
  const rect = root.getBoundingClientRect();
  return {
    bottom: Math.min(rect.bottom, window.innerHeight),
    left: Math.max(rect.left, 0),
    right: Math.min(rect.right, window.innerWidth),
    top: Math.max(rect.top, 0),
  };
}

/** Whether `control` can be where the reader's place is: neither it nor any element between it
 *  and the box is pinned (`PINNED`), and some of `rect` shows - inside the box's part of the
 *  window, and inside every element between the control and the box that cuts what it draws: the
 *  box's scrolling body, for one, whose controls scrolled out of it still stand inside the box's
 *  own rect. */
function inSightUnpinned(
  control: HTMLElement,
  rect: Bounds,
  root: HTMLElement,
  sight: Bounds,
  frames: Map<Element, Frame>
): boolean {
  if (PINNED[getComputedStyle(control).position] === true) return false;
  let { bottom, left, right, top } = sight;
  for (
    let ancestor = control.parentElement;
    ancestor !== null && ancestor !== root;
    ancestor = ancestor.parentElement
  ) {
    let frame = frames.get(ancestor);
    if (frame === undefined) {
      const style = getComputedStyle(ancestor);
      frame = {
        clip:
          CLIPPING[style.overflowX] === true || CLIPPING[style.overflowY] === true
            ? ancestor.getBoundingClientRect()
            : null,
        pinned: PINNED[style.position] === true,
      };
      frames.set(ancestor, frame);
    }
    if (frame.pinned) return false;
    const { clip } = frame;
    if (clip !== null) {
      bottom = Math.min(bottom, clip.bottom);
      left = Math.max(left, clip.left);
      right = Math.min(right, clip.right);
      top = Math.max(top, clip.top);
    }
  }
  return rect.bottom > top && rect.top < bottom && rect.right > left && rect.left < right;
}

/** The box's heading, able to take the focus: given `tabindex="-1"` where it takes none. */
function headingOf(root: HTMLElement): HTMLElement | null {
  const heading = root.querySelector("h2");
  if (heading === null) return null;
  if (!heading.hasAttribute("tabindex")) heading.setAttribute("tabindex", "-1");
  return heading;
}
