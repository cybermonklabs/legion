// The Settings page's Legion daemon field (dispatch://INFRA-1077). No dashboard page draws a
// daemon's board, so the one Legion surface a browser drives is the Settings section where a
// project's daemon is named, listed, refused a bad URL, and removed.
// Every spec is tagged `@legion`, so `--grep @legion` runs this file's work and nothing else.
//
// A project's daemon is named by `PUT /api/v1/projects/{key}/legion/daemon` and removed by its
// DELETE, both deployment-scoped; the route stores a base URL and never calls the host, so these
// specs need no daemon to be reachable.
import { type Refusal, routePath } from "../web/src/api/paths";
import { baseUrl, userHeaders } from "./api";
import { removeLegionDaemon, seedLegionDaemon, seedLegionProject } from "./legion-seed";
import { emptyDatabase, expect, rowKey, test } from "./test";
import { asUser } from "./users";

const DAEMON_URL = "http://legion.acme.internal:9731";

// The section lists every project's daemon of the stack, every row's among them, so a row reads
// its own project's daemon by the row the section draws for it (`data-legion-daemon`), and takes
// it out again in its `finally` (`removeLegionDaemon`): the next row of the worker reads the list
// this one started on.

// A1/A4: naming a project's daemon on the Settings page, beside the Delivery settings. No Legion
// view exists for it to bring, so the row is the naming itself: a project that named no daemon is
// given one, and the section lists it once the page is read again from the server.
test("the Settings page names a project's Legion daemon @legion", async ({ browser }) => {
  const acme = rowKey("ACME");
  await seedLegionProject(acme);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Legion" })).toBeVisible();
    await page.getByLabel("Legion daemon state URL").fill(DAEMON_URL);
    await page.getByRole("combobox", { exact: true, name: "Legion project" }).selectOption(acme);
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith(`/api/v1/projects/${acme}/legion/daemon`)
    );
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    expect((await saved).status()).toBe(200);

    // The section lists the daemon the naming stored, as the server reads it back: the section
    // fills its list before its refetch settles, so only a reload proves what was stored.
    await page.reload();
    await expect(page.locator(`[data-legion-daemon="${acme}"]`)).toContainText(DAEMON_URL);
  } finally {
    await context.close();
    await removeLegionDaemon(acme);
  }
});

// The other half of the Settings field: the URL it refuses, in the server's own words, and the
// Remove that stops the reading. A state URL that is not an absolute http(s) one with a host and no
// path is refused, and the page carries a second copy of that rule (`legionStateUrl` in
// LegionDaemonsSection.tsx) while only the server's copy decides what is stored.
test("the Settings page refuses a state URL that is not a daemon's, and its Remove stops the reading @legion", async ({
  browser,
}) => {
  const acme = rowKey("ACME");
  // The claim after the Remove is a section that lists no daemon and says so, which only an
  // empty stack holds: the section lists every project's daemon of the stack, so no key of this
  // row's makes a list of its own, and a daemon another row left would stand in it. It empties
  // through `emptyDatabase`, which counts the database as the worker's, so the datasets the
  // worker keeps are written again for the rows after this one.
  await emptyDatabase();
  await seedLegionDaemon(acme, DAEMON_URL);
  // Signed in after the emptying, as e2e/issue.e2e.ts's row is: the emptying deletes every
  // session a sign-in wrote but the ones it writes again for alice and bob (e2e/users.ts).
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const bad = `${DAEMON_URL}/legion/v1/state`;

  try {
    // What the server says about this URL, so the page can be held to its wording.
    const daemonPath = routePath("put", "/api/v1/projects/{key}/legion/daemon", { key: acme });
    const refusal = await fetch(new URL(daemonPath, baseUrl), {
      body: JSON.stringify({ state_url: bad }),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "PUT",
    });
    expect(refusal.status).toBe(400);
    const refused = (await refusal.json()) as Refusal<
      "/api/v1/projects/{key}/legion/daemon",
      "put"
    >;
    expect(refused.code).toBe("LEGION_SETTING_INPUT");

    await page.goto("/settings");
    const section = page.locator("[data-legion-daemons]");
    const daemon = section.locator(`[data-legion-daemon="${acme}"]`);
    await expect(daemon).toContainText(DAEMON_URL);

    // The page refuses the same URL, in the server's own words, and sends no write at all.
    let wrote = false;
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        request.url().endsWith(`/api/v1/projects/${acme}/legion/daemon`)
      ) {
        wrote = true;
      }
    });
    await page.getByLabel("Legion daemon state URL").fill(bad);
    await page.getByRole("combobox", { exact: true, name: "Legion project" }).selectOption(acme);
    await page.getByRole("button", { name: "Save Legion daemon" }).click();
    await expect(page.getByRole("alert")).toHaveText(refused.error);
    expect(wrote).toBe(false);
    // The refusal changed nothing: the project still reads the daemon it did.
    await expect(daemon).toContainText(DAEMON_URL);

    // Remove stops the reading, and the section lists no daemon and says so.
    const removed = page.waitForResponse(
      (response) =>
        response.request().method() === "DELETE" &&
        response.url().endsWith(`/api/v1/projects/${acme}/legion/daemon`)
    );
    await page.getByRole("button", { name: `Remove the Legion daemon of ${acme}` }).click();
    expect((await removed).status()).toBeLessThan(300);
    await expect(section.locator("[data-legion-daemon]")).toHaveCount(0);
    await expect(section.locator("[data-legion-daemons-empty]")).toHaveCount(1);
  } finally {
    await context.close();
    await removeLegionDaemon(acme);
  }
});
