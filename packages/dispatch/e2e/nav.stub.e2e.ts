/** Where the application's own routing takes a reader: the issue tabs and their addresses, the
 *  page a missing issue draws after the reads it is allowed, and the dialog the rail's New issue
 *  button opens.
 *
 *  Both rows read only what the page draws over fixed answers, so each runs against a recording
 *  rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The two
 *  rows of `nav.e2e.ts` stayed there because they carry `@phone`, and the `iphone` project runs
 *  no stubbed row.
 */
import { expect, type Page } from "@playwright/test";

import { QUERY_RETRY_DELAY_MS } from "../web/src/api/query-client";
import { createIssue, createProject } from "./api";
import { stubbed } from "./stub/stub-test";

// ARC sorts first in the project list the create dialog falls back to, so only the route's own
// project can make that dialog's selector read CORE.
const test = stubbed(async () => {
  await createProject({ key: "ARC", name: "Archive" });
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ship the release" });
  await createIssue({ parent: issue.key, project: "CORE", title: "Child task" });
  return { issueKey: issue.key, issueTitle: issue.title, project: "CORE" };
});

/** Counts the reads of `path` the page made and the answers to them it took that were not ok.
 *  A row waits on `refused` for a refusal the page has already had before it runs the clock
 *  over that refusal's retry wait, and asserts on `asked` when its claim is that no further
 *  read was made: a request the clock has just started counts the moment it leaves, where its
 *  answer lands later and would let such an assertion pass over the very read it must catch. */
function reads(page: Page, path: string): { asked: () => number; refused: () => number } {
  let asked = 0;
  let refused = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === path) asked += 1;
  });
  page.on("response", (response) => {
    if (new URL(response.url()).pathname === path && !response.ok()) refused += 1;
  });
  return { asked: () => asked, refused: () => refused };
}

test("issue tabs are URL-driven and keyboard-navigable, and titles follow direct issue routes @pr", async ({
  page,
  scene,
}) => {
  await page.clock.install();
  await page.goto(`/issues/${scene.issueKey}`);
  await expect(page).toHaveTitle(`${scene.issueKey} · ${scene.issueTitle} · Dispatch`);
  await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");

  await page.getByRole("tab", { name: "Children" }).click();
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.issueKey}/children$`));
  await page.reload();
  await expect(page.getByRole("tab", { name: "Children" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expect(page.getByRole("tabpanel")).toContainText("Child task");

  await page.getByRole("tab", { name: "Children" }).focus();
  await page.keyboard.press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Conversation" })).toBeFocused();
  await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.issueKey}/conversation$`));

  // Collaborators is the last tab, with Decisions and Activity before it, each with its own
  // route.
  await page.keyboard.press("End");
  await expect(page.getByRole("tab", { name: "Collaborators" })).toBeFocused();
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.issueKey}/collaborators$`));
  await page.keyboard.press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Decisions" })).toBeFocused();
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.issueKey}/decisions$`));
  await page.keyboard.press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Activity" })).toBeFocused();
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.issueKey}/activity$`));
  await expect(page.getByRole("list", { name: "Issue activity" })).toBeVisible();

  // A read the server answers 404 is asked again twice, QUERY_RETRY_DELAY_MS apart
  // (web/src/api/query-client.ts), before the page says the issue is missing: the clock runs
  // those two waits once the page has taken each refusal. `page.clock.install()` leaves the
  // page's clock running at real speed, so a refusal can land between two samples of a
  // counter that only grows; each wait is synchronised on "this refusal has been taken",
  // never on an exact count a late sample steps over and can never see again. The exact
  // total - the read and its two retries, and no third - is read at the end, once the page
  // has given up and the clock has been run far past any wait a retry could be on. It counts
  // the reads the page made, not the refusals it took: a third retry the last runFor started
  // is already a request, where its own 404 is still in flight and would leave the count at
  // three until after this row had passed on it.
  const retries = 2;
  const missing = reads(page, "/api/v1/issues/CORE-999");
  await page.goto("/issues/CORE-999");
  for (let refusal = 1; refusal <= retries; refusal += 1) {
    await expect
      .poll(missing.refused, { intervals: [50, 50, 100, 200, 500] })
      .toBeGreaterThanOrEqual(refusal);
    await page.clock.runFor(QUERY_RETRY_DELAY_MS + 50);
  }
  await expect(page.getByRole("heading", { name: "Issue not found" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Back to inbox" })).toBeVisible();
  await page.clock.runFor(QUERY_RETRY_DELAY_MS * 10);
  expect(missing.asked()).toBe(retries + 1);

  await page.goto(`/issues/${scene.issueKey}/not-a-tab`);
  await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
  await expect(page.getByRole("tablist", { name: "Issue detail" })).toHaveCount(0);
  await expect(page).toHaveTitle("Not found · Dispatch");
  await page.goto("/definitely-not-a-dispatch-route");
  await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
  await expect(page).toHaveTitle("Not found · Dispatch");
  await expect(page.getByRole("link", { name: "Back to inbox" })).toBeVisible();
});

test("the rail's New issue button opens the create dialog, preselecting the page's project @pr", async ({
  page,
  scene,
}) => {
  await page.goto(`/projects/${scene.project}`);
  await page
    .getByRole("complementary", { name: "Navigation" })
    .getByRole("button", { name: "New issue" })
    .click();
  const dialog = page.getByRole("dialog", { name: "Create issue" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("textbox", { name: "Title" })).toBeFocused();
  await expect(dialog.getByRole("combobox", { name: "Project" })).toHaveValue(scene.project);
});
