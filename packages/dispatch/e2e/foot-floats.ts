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

/** Routes `page` so that both floats can show: every `PATCH` of the issue `refusedKey` is
 *  refused, which leaves the change pending, and every live process serves one build the page
 *  does not run (`deploymentChanged`), which shows the update banner. */
export async function routeFloats(page: Page, refusedKey: string): Promise<void> {
  await page.route(`**/api/v1/issues/${refusedKey}`, async (route) => {
    if (route.request().method() === "PATCH")
      await route.fulfill({ status: 403, json: { error: "Move refused" } });
    else await route.continue();
  });
  await page.route("**/api/v1/dashboard/builds", (route) =>
    route.fulfill({
      json: { live: ["/assets/index-next.js"], serving: "/assets/index-next.js" },
    })
  );
}

/** Opens `path` on `page`, routed by `routeFloats`, with both floats shown: a change to
 *  `refusedKey` refused on the issues table, which Pending changes keeps across the load, and
 *  the banner, which the page shows from the builds the route answers. */
export async function openWithFloats(page: Page, refusedKey: string, path: string): Promise<void> {
  const pending = pendingWrites(page);
  await page.goto("/projects/CORE/issues");
  await page.getByRole("checkbox", { name: `Select ${refusedKey}`, exact: true }).check();
  await page
    .getByRole("toolbar")
    .getByRole("button", { name: "Take out of epic" })
    .click({ timeout: 5_000 });
  await expect(pending).toContainText(refusedKey);
  await page.goto(path);
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(updateBanner(page)).toBeVisible();
  await expect(pending).toContainText(refusedKey);
}

/** The project CORE with an epic and an issue in it, whose move out of the epic `routeFloats`
 *  refuses. Answers that issue. */
export async function seedRefusedMove() {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Parser rollout" });
  await patchIssue(epic.key, { labels: ["epic"] });
  return createIssue({ project: "CORE", parent: epic.key, title: "Refused move" });
}
