import { abortOnReaderAct } from "./useNodeEditing";

/**
 * The view's first focus, given where its row stands. Where nothing holds the focus, the stage
 * gives it to the view's tab-stop row once the view draws it (`slots/stage.tsx`'s `ViewStage`),
 * without a scroll; but the row then placed itself on screen by the page's one rule as the focus
 * came to it, and again whenever it grew (`NodeRow.tsx`'s `useRowOnScreen`), which scrolled the
 * page after its first drawing, and R3 brings into the viewport at the first drawing only the
 * question Conversation's first focus names and the row an address names. On the seed at 390 px,
 * where the issue's children stand above the view's rows, Spec, Plan, Decisions and Graph
 * scrolled by 1,185 to 1,421 px once their rows were drawn, and by 20 to 42 px more at both widths
 * as that row grew when a later read answered (CI run 45878460106). So the row the stage gives the
 * first focus to is placed neither as it takes it nor as it grows, as the outline's first row the
 * page opens on is not (`IssueTree.tsx`'s `unplaced`). The reader's first act (`READER_ACTS`), or
 * the focus leaving the row, ends that, and the row is placed by the rule again from the next
 * time it is placed, so the end itself moves nothing.
 */
let given: { readonly ends: AbortController; readonly to: HTMLElement } | undefined;

/** Gives `to` the view's first focus where it stands: focused without a scroll, and left where it
 *  stands while it holds that focus and the reader has not acted (`holdsFirstFocus`). */
export function giveFirstFocus(to: HTMLElement): void {
  given?.ends.abort();
  const ends = abortOnReaderAct();
  const held = { ends, to };
  ends.signal.addEventListener("abort", () => {
    if (given === held) given = undefined;
  });
  to.addEventListener(
    "focusout",
    (event) => {
      const next = event.relatedTarget;
      if (!(next instanceof Node && to.contains(next))) ends.abort();
    },
    { signal: ends.signal }
  );
  // Marked before the focus, whose own render places the row (`useRowOnScreen`).
  given = held;
  to.focus({ preventScroll: true });
}

/** Whether `element` holds the view's first focus as `giveFirstFocus` gave it, or stands inside
 *  what holds it or holds it: a row that places itself is left where it stands while so. */
export function holdsFirstFocus(element: Element): boolean {
  if (given === undefined) return false;
  const { ends, to } = given;
  // A row taken off the page with the focus hears no focusout.
  if (!to.isConnected) {
    ends.abort();
    return false;
  }
  return element === to || element.contains(to) || to.contains(element);
}
