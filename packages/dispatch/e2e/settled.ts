/** Which of a few named states the page is in, read once the page has settled on one of them,
 *  for a helper whose next step depends on what the page shows.
 *
 * A branch on what the page shows reads that state after the page has settled on one of the
 * states it chooses between, never with one zero-wait probe such as `isVisible()`. A probe
 * answers at once, and a page that has not drawn its layout yet answers no to every question, so
 * the helper takes the wrong path and the row fails later, at a line that has nothing to do with
 * the branch. That is how "the rail row, the Agents row and the live view each unpin"
 * (`agent-rail.e2e.ts`) failed at a phone width: `openRail` asked whether the Menu control showed
 * while `/auth/whoami` was still pending and the page was `ShellSkeleton`, which has no Menu
 * control, so the sheet was never opened and the rail's rows read 0 for the whole wait after it.
 *
 * The probes are read inside one `expect.poll`, so each one is a read the poll retries until a
 * state holds, under the project's expect timeout, rather than one answer taken too early. Every
 * state is read in the same callback, because waiting for one of them alone costs its whole
 * timeout whenever the page is in another. The first state that shows wins, so the order is a
 * priority: two states can show at once, as the rail's Menu control does under its open sheet.
 */

import { expect, type Locator } from "@playwright/test";

/** The name of the first of `states` whose locator is visible, read until one is or the project's
 *  expect timeout ends, or `timeoutMs` where it is given: the outside driver
 *  (`e2e/acceptance/outside-edge.ts`) runs with no project, against a page across the public
 *  internet, and bounds each page step itself. No page is passed: each locator already reads its
 *  own. `what` names what is read, for the failure, which names every state: `the rail: none of
 *  open sheet, closed sheet, column showed`. */
export async function settledState<Name extends string>(
  what: string,
  states: readonly (readonly [Name, Locator])[],
  timeoutMs?: number
): Promise<Name> {
  const failure = `${what}: none of ${states.map(([name]) => name).join(", ")} showed`;
  let shown: Name | undefined;
  await expect
    .poll(
      async () => {
        for (const [name, locator] of states) {
          if (await locator.isVisible()) {
            shown = name;
            return true;
          }
        }
        return false;
      },
      // Playwright's own web-first retry schedule, so a page that draws early is read early.
      {
        intervals: [20, 50, 100, 100, 500],
        message: failure,
        ...(timeoutMs === undefined ? {} : { timeout: timeoutMs }),
      }
    )
    .toBe(true);
  // A poll whose row has already ended passes without reading, and leaves no state behind.
  if (shown === undefined) throw new Error(failure);
  return shown;
}

/** Whether the panel `control` opens is open, by the control's own `aria-expanded`, read once
 *  the page shows the control: a count taken while the page still loads finds no panel, and
 *  takes it for a shut one. `what` names the control, for the failure. */
export function expandedState(what: string, control: Locator): Promise<"open" | "shut"> {
  const page = control.page();
  return settledState(what, [
    ["open", control.and(page.locator('[aria-expanded="true"]'))],
    ["shut", control.and(page.locator('[aria-expanded="false"]'))],
  ]);
}
