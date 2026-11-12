import { afterTheFirstBatch, theFirstBatchIsAwaited } from "../doc/first-batch";

/**
 * A key press the issue page's chrome is not drawn for yet, and the commit that answers it.
 *
 * While the page holds its header and its tab strip for the editor's first batch
 * (`features/doc/first-batch.ts`), every binding of the `issue` scope resolves against a
 * control only the real header and the real strip render, so each would do nothing: `s`, `p`,
 * `l`, the title key, Shift+P and every `t …` chord. A pointer reader presses the held box and
 * has the chrome (`features/doc/AfterTheFirstBatch.tsx`); this is the same reach, made from the
 * keyboard (`IssuePage.tsx`, dispatch://INFRA-341).
 *
 * The press is kept here and done from the commit that mounts the chrome, never from a budget
 * of animation frames. The chrome arrives behind a chunk this page asks for only once the hold
 * is released, and behind two commits after that, which on the phone this change is written for
 * is the slow path and not the quick one: a budget large enough to cover it is a key that fires
 * seconds after it was pressed, and one that does not cover it drops the press with nothing
 * said. Both were the same 180 frames. The chrome's own mount is the moment there is something
 * to act on, so that is what runs it.
 */
let queued: (() => boolean) | undefined;

/**
 * Does what a binding came to do, drawing the chrome first where the page is still holding it.
 * `act` answers whether it found its control, and is kept until a commit of the chrome lets it
 * find one.
 *
 * A second press before the first is answered replaces it: what the reader asked for last is
 * what they are waiting for, and the control the first press wanted is a press away once the
 * chrome they both reached for is on the page.
 */
export function reachingTheChrome(act: () => boolean): void {
  if (act() || afterTheFirstBatch()) {
    return;
  }
  queued = act;
  theFirstBatchIsAwaited(false);
}

/** What the header and the tab strip tell this module as they mount (`IssueHeader`,
 *  `IssueTabs`): a press waiting for either is done now. One that still finds nothing - a tab
 *  chord answered by the header's commit, which draws no tablist - waits for the other. */
export function theChromeIsHere(): void {
  if (queued?.() === true) {
    queued = undefined;
  }
}

/** The page letting go of a press whose chrome never came, as it leaves (`IssuePage.tsx`). */
export function forgetTheReach(): void {
  queued = undefined;
}
