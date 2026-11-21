// The Settings page's Legion daemon field (dispatch://INFRA-1077). No dashboard page draws a
// daemon's board, so the one Legion surface a browser drives is the Settings section where a
// project's daemon is named, listed, refused a bad URL, and removed.
// Every spec is tagged `@legion`, so `--grep @legion` runs this file's work and nothing else.
//
// A project's daemon is named by `PUT /api/v1/projects/{key}/legion/daemon` and removed by its
// DELETE, both deployment-scoped; the route stores a base URL and never calls the host, so these
// specs need no daemon to be reachable.
import { baseUrl, userHeaders } from "./api";
import { listOf, listTrigger } from "./filters";
import { LEGION_PROJECT, seedLegionDaemon, seedLegionProject } from "./legion-seed";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

const DAEMON_URL = "http://legion.acme.internal:9731";

/** Picks LEGION_PROJECT from the Legion project list (`components/MultiSelect.tsx`), as a reader
 *  does: presses its trigger, whose name begins with the list's label and goes on to what it
 *  shows, then the project's option in the list it opens, which is portalled out of the section
 *  and so found from the trigger that names it (`listOf`). */
async function pickLegionProject(page: Page): Promise<void> {
  const trigger = listTrigger(page, "Legion project");
  await trigger.click();
  await listOf(trigger)
    .getByRole("option", { name: new RegExp(`^${LEGION_PROJECT} · `) })
    .click();
}

// A1/A4: naming a project's daemon on the Settings page, beside the Delivery settings. No Legion
// view exists for it to bring, so the row is the naming itself: a project that named no daemon is
// given one, and the section lists it once the page is read again from the server.
test("the Settings page names a project's Legion daemon @legion", async ({ browser }) => {
  await seedLegionProject();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Legion" })).toBeVisible();
    await page.getByLabel("Legion daemon state URL").fill(DAEMON_URL);
    await pickLegionProject(page);
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith(`/api/v1/projects/${LEGION_PROJECT}/legion/daemon`)
    );
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    expect((await saved).status()).toBe(200);

    // The section lists the daemon the naming stored, as the server reads it back: the section
    // fills its list before its refetch settles, so only a reload proves what was stored.
    await page.reload();
    await expect(page.locator("[data-legion-daemons]")).toContainText(DAEMON_URL);
  } finally {
    await context.close();
  }
});

// The other half of the Settings field: the URL it refuses, in the server's own words, and the
// Remove that stops the reading. A state URL that is not an absolute http(s) one with a host and no
// path is refused, and the page carries a second copy of that rule (`legionStateUrl` in
// LegionDaemonsSection.tsx) while only the server's copy decides what is stored.
test("the Settings page refuses a state URL that is not a daemon's, and its Remove stops the reading @legion", async ({
  browser,
}) => {
  await seedLegionDaemon(DAEMON_URL);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const bad = `${DAEMON_URL}/legion/v1/state`;

  try {
    // What the server says about this URL, so the page can be held to its wording.
    const refusal = await fetch(
      new URL(`/api/v1/projects/${LEGION_PROJECT}/legion/daemon`, baseUrl),
      {
        body: JSON.stringify({ state_url: bad }),
        headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
        method: "PUT",
      }
    );
    expect(refusal.status).toBe(400);
    const refused = (await refusal.json()) as { code: string; error: string };
    expect(refused.code).toBe("LEGION_SETTING_INPUT");

    await page.goto("/settings");
    const section = page.locator("[data-legion-daemons]");
    await expect(section).toContainText(DAEMON_URL);

    // The page refuses the same URL, in the server's own words, and sends no write at all.
    let wrote = false;
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        request.url().endsWith(`/api/v1/projects/${LEGION_PROJECT}/legion/daemon`)
      ) {
        wrote = true;
      }
    });
    await page.getByLabel("Legion daemon state URL").fill(bad);
    await pickLegionProject(page);
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    await expect(page.getByRole("alert")).toHaveText(refused.error);
    expect(wrote).toBe(false);
    // The refusal changed nothing: the project still reads the daemon it did.
    await expect(section).toContainText(DAEMON_URL);

    // Remove stops the reading, and the section lists no daemon.
    const removed = page.waitForResponse(
      (response) =>
        response.request().method() === "DELETE" &&
        response.url().endsWith(`/api/v1/projects/${LEGION_PROJECT}/legion/daemon`)
    );
    await page
      .getByRole("button", { name: `Remove the Legion daemon of ${LEGION_PROJECT}` })
      .click();
    expect((await removed).status()).toBeLessThan(300);
    await expect(section.locator("[data-legion-daemons-empty]")).toBeVisible();
  } finally {
    await context.close();
  }
});
