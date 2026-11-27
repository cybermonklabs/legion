/** How a row shows both floats of the foot of the screen (`web/src/hooks/useFootInset.ts`):
 *  Pending changes, which a refused change leaves open, and the update banner, which shows once
 *  every live process serves a build the page does not run. The rows of
 *  `e2e/issues-table.e2e.ts` and `e2e/foot-sweep.e2e.ts` that read the foot show them this way. */
import { createIssue, createProject, patchIssue } from "./api";
import { pendingWrites } from "./keyed-writes";
import { expect, type Locator, type Page } from "./test";

/** The update banner (`DeploymentResilience`), gone while no update shows. */
export function updateBanner(page: Page): Locator {
  return page.getByRole("status", { name: "A new version is available" });
}

/** Routes `page` so that every `PATCH` of the issue `refusedKey` is refused, which leaves the
 *  change pending. */
export async function refuseChanges(page: Page, refusedKey: string): Promise<void> {
  await page.route(`**/api/v1/issues/${refusedKey}`, async (route) => {
    if (route.request().method() === "PATCH")
      await route.fulfill({ status: 403, json: { error: "Move refused" } });
    else await route.continue();
  });
}

/** Routes `page` so that both floats can show: every `PATCH` of the issue `refusedKey` is
 *  refused (`refuseChanges`), which leaves the change pending, and every live process serves one
 *  build the page does not run (`deploymentChanged`), which shows the update banner. */
export async function routeFloats(page: Page, refusedKey: string): Promise<void> {
  await refuseChanges(page, refusedKey);
  await page.route("**/api/v1/dashboard/builds", (route) =>
    route.fulfill({
      json: { live: ["/assets/index-next.js"], serving: "/assets/index-next.js" },
    })
  );
}

/** The key the project List of CORE keeps its stored record under, a table action in it. */
const TABLE_RECORD = "dispatch.project-table.CORE";

/** Dismisses the action a refused Start next leaves on the issues table `page` shows, and waits
 *  until the List has let go of its record. Start next keeps a record of its action from before
 *  its first write, and the record a refused write leaves draws the Interrupted table action
 *  notice and refuses the project's next Start next until the reader dismisses it
 *  (`useTableRecovery`, `web/src/features/project/issues-table-recovery.ts`). The refused change
 *  stays in Pending changes, and the page's next Start next begins afresh. Call it with the
 *  selection bar gone, so that no part of the bar stands over the notice's Dismiss. */
export async function dismissRefusedStartNext(page: Page): Promise<void> {
  const interrupted = page.getByRole("region", { name: "Interrupted table action" });
  await interrupted.getByRole("button", { name: "Dismiss", exact: true }).click({ timeout: 5_000 });
  await expect(interrupted).toHaveCount(0);
  await expect
    .poll(
      () =>
        page.evaluate((key) => {
          const record: unknown = JSON.parse(localStorage.getItem(key) ?? "{}");
          return typeof record === "object" && record !== null && "action" in record
            ? (record.action ?? null)
            : null;
        }, TABLE_RECORD),
      { message: "the refused Start next's record was never let go" }
    )
    .toBeNull();
}

/** Opens `path` on `page`, routed by `routeFloats`, with both floats shown: a change to
 *  `refusedKey` refused on the issues table, which Pending changes keeps across the load, and
 *  the banner, which the page shows from the builds the route answers. The change is the move up
 *  the queue that the selection bar's Start next sends for `refusedKey` alone (`seedRefusedMove`),
 *  and the action it leaves is dismissed (`dismissRefusedStartNext`), so a row that opens the
 *  floats again in the same page makes the change again. */
export async function openWithFloats(page: Page, refusedKey: string, path: string): Promise<void> {
  await page.goto("/projects/CORE/issues");
  await page.getByRole("checkbox", { name: `Select ${refusedKey}`, exact: true }).check();
  const toolbar = page.getByRole("toolbar");
  await toolbar.getByRole("button", { name: "Start next", exact: true }).click({ timeout: 5_000 });
  await expect(pendingWrites(page)).toContainText(refusedKey);
  await toolbar.getByRole("button", { name: "Clear selection" }).click({ timeout: 5_000 });
  await expect(toolbar).toHaveCount(0);
  await dismissRefusedStartNext(page);
  await reloadWithBanner(page, refusedKey, path);
}

/** Loads `path` on `page` again with the update banner shown, as a load of the page shows it
 *  from the builds `routeFloats` answers; Pending changes keeps the change to `refusedKey`
 *  across the load. */
export async function reloadWithBanner(
  page: Page,
  refusedKey: string,
  path: string
): Promise<void> {
  await page.goto(path);
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(updateBanner(page)).toBeVisible();
  await expect(pendingWrites(page)).toContainText(refusedKey);
}

/** Dismisses the update banner, which stays dismissed for the page's life. */
export async function dismissBanner(page: Page): Promise<void> {
  const banner = updateBanner(page);
  await banner.getByRole("button", { name: "Dismiss update notice" }).click({ timeout: 5_000 });
  await expect(banner).toHaveCount(0);
}

/** Discards the refused change, which closes Pending changes. */
export async function discardPending(page: Page): Promise<void> {
  const pending = pendingWrites(page);
  await pending.getByRole("button", { name: "Discard", exact: true }).click({ timeout: 5_000 });
  await expect(pending).toHaveCount(0);
}

/** The project CORE with two issues of Start next's own set, the Todo issues labelled legion
 *  (`isQueued`, `web/src/features/project/issues-table-model.ts`): the second's move ahead of the
 *  first, which the selection bar's Start next sends for it alone, is the change `routeFloats`
 *  refuses. Answers that second issue. */
export async function seedRefusedMove() {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "Parser rollout" });
  const refused = await createIssue({ project: "CORE", title: "Refused move" });
  // One at a time, in the order they were made, which is their rank: the refused issue stands
  // second in the queue.
  for (const issue of [first, refused]) {
    await patchIssue(issue.key, { labels: ["legion"], status: "todo" });
  }
  return refused;
}

/** Issues enough to run the issues table's box, and the Board's Triage column, past the window
 *  at every size. Nothing reads their order among themselves, so they go out side by side. */
export async function seedFillers(): Promise<void> {
  await Promise.all(
    Array.from({ length: 40 }, (_, index) =>
      createIssue({ project: "CORE", title: `Filler issue number ${index}` })
    )
  );
}
