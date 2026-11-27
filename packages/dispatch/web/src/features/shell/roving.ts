/** The nearest element matching `selector` at or above `node`; `null` off any such element. */
export function closestMatching(node: Element | null, selector: string): HTMLElement | null {
  return node?.closest<HTMLElement>(selector) ?? null;
}

/** Whether focus has fallen to the document, where a control that is disabled or leaves the page
 *  drops it. That is the only focus a page takes back on the reader's behalf: anywhere else, the
 *  reader put it there. */
export function focusOnDocument(): boolean {
  const focused = document.activeElement;
  return focused === null || focused === document.body;
}

/** Puts the focus on `to` as a panel or a list closes, by `focus` (the element's own `focus()`
 *  unless given), or on the page itself where `to` is the body, where a key opened it with nothing
 *  focused: only taking the focus off the element that holds it puts it back there. */
export function focusBack(
  to: HTMLElement | null,
  focus: (element: HTMLElement) => void = (element) => element.focus()
): void {
  if (to === document.body) {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    return;
  }
  if (to !== null) focus(to);
}

/** Gives the focus back (`back`) on a press outside a menu, in the document's capture-phase
 *  `pointerdown` that closes it: at once, before the browser's own focus on the press, which is
 *  the default action of its `mousedown`, after. So a press on a text box, a button or a link
 *  focuses that control and acts on that one press; the press's own focus has landed by its
 *  `mouseup`, a finger's too, and a press on nothing that takes the focus, which leaves it on the
 *  document, has it given back again then. A press the browser cancels (a scroll) moves no focus,
 *  and the next press is another press, so either ends the wait. */
export function focusBackOverPress(back: () => void): void {
  back();
  const settle = () => {
    stop();
    if (focusOnDocument()) back();
  };
  const stop = () => {
    document.removeEventListener("mouseup", settle, true);
    document.removeEventListener("pointercancel", stop, true);
    document.removeEventListener("pointerdown", stop, true);
  };
  document.addEventListener("mouseup", settle, true);
  document.addEventListener("pointercancel", stop, true);
  document.addEventListener("pointerdown", stop, true);
}

const PRESS_CANCELLATIONS = ["pointercancel", "dragend", "contextmenu"] as const;

/** Hears the ways a press ends without a release on the page. Listen for the window's own
 * blur, not a capture-phase blur, which also hears every element's blur. */
export function hearPressCancellation(page: Document, end: () => void): () => void {
  const heard = new AbortController();
  const { signal } = heard;
  for (const type of PRESS_CANCELLATIONS) {
    page.addEventListener(type, end, { capture: true, signal });
  }
  page.defaultView?.addEventListener("blur", end, { signal });
  return () => heard.abort();
}

/**
 * The element matching `selector` that holds keyboard focus *itself* - not one merely containing
 * a focused control. Every roving list asks this of its own rows: a key bound to the row acts
 * while the reader is on the row, and stays out of the way while they are in something inside it.
 */
export function focusedMatching(selector: string): HTMLElement | null {
  const active = document.activeElement;
  return active instanceof HTMLElement && active.matches(selector) ? active : null;
}

/**
 * The rows of `nodes` a reader can actually reach, for the keys that step over them and for
 * the `when` that offers those keys - one rule, so the two cannot disagree.
 *
 * A row inside a collapsed `details` is in the DOM, is not rendered, and can take no focus at
 * all, so counting it would dead-end the keys on the row before the band and strand every row
 * past it. None of the lists that rove keeps rows in a closed `details` today - the Inbox
 * renders no row of a folded band and Architecture renders only the current level's rows - and
 * this rule is what lets the next banded list use one safely.
 * The DOM alone answers that case, so the rule reads no style: `checkVisibility()` would answer
 * it and more - a row the stylesheet hides as well - at the cost of a style read of every row on
 * each key.
 */
function reachableRows(nodes: readonly HTMLElement[]): HTMLElement[] {
  return nodes.filter((node) => node.closest("details:not([open])") === null);
}

/**
 * Moves keyboard focus one step through the rows of `nodes` the reader can reach: from
 * `current` to its neighbour, clamped at both ends; when nothing in the list holds focus, to
 * the first node going down and the last going up. Nothing happens when none is reachable.
 */
export function roveFocus(
  nodes: readonly HTMLElement[],
  current: HTMLElement | null,
  delta: 1 | -1
): void {
  const reachable = reachableRows(nodes);
  if (reachable.length === 0) {
    return;
  }
  const at = current === null ? -1 : reachable.indexOf(current);
  const next =
    at === -1
      ? delta === 1
        ? 0
        : reachable.length - 1
      : Math.max(0, Math.min(reachable.length - 1, at + delta));
  reachable[next]?.focus();
}

/**
 * Where a roving focus goes from `at` of `count` controls on `key`: `back` and `forward` step
 * one, wrapping at the ends, and Home and End go to the first and the last; undefined for any
 * other key. A menu's choices step so (`ChoiceMenu`, and the issue page's menu of views in
 * `IssuePresets`); `roveFocus` clamps instead, for the rows `j` and `k` walk.
 */
export function stepIndex(
  key: string,
  at: number,
  count: number,
  back: string,
  forward: string
): number | undefined {
  if (key === forward) return (at + 1) % count;
  if (key === back) return (at - 1 + count) % count;
  if (key === "Home") return 0;
  if (key === "End") return count - 1;
  return undefined;
}
