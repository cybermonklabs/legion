import { baseUrl, createProject, userHeaders } from "./api";
import { sql } from "./psql";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

/** The restricted member of this file: a person from outside the sign-in domain whom PRIV's
 *  access list names. */
const BOB = "bob@outside.example";

const PEEK = "Peek at the inbox without leaving this page";

/** Where each link of the rail goes, in the order the rail draws them. */
function railTargets(page: Page): Promise<(string | null)[]> {
  return page
    .getByRole("navigation", { name: "Navigation" })
    .getByRole("link")
    .evaluateAll((links) => links.map((link) => link.getAttribute("href")));
}

// @pr: Private projects: a restricted member's rail is Inbox, Agents, their own projects and Settings, and the views over every project answer Page not found.
test("a restricted member's rail is Inbox, Agents, their projects and Settings, and the views over every project are not found @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "PRIV", name: "Private work" });
  // bob is a restricted member: the people row outside sign-in records for a person an access
  // list names, written here because that sign-in is not what this row is about. alice, who made
  // PRIV and owns it, names him on its access list, as its Access tab does.
  await sql(`insert into people (email, member_kind) values ('${BOB}', 'restricted')`);
  const named = await fetch(
    new URL(`/api/v1/projects/PRIV/access/people/${encodeURIComponent(BOB)}`, baseUrl),
    { headers: await userHeaders("alice"), method: "PUT" }
  );
  expect(named.status, await named.text()).toBe(200);

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, BOB);
  try {
    // A full member's rail is the one it always was: every view over every project, the Peek,
    // and every project they can see.
    const alicePage = await alice.newPage();
    await alicePage.goto("/projects/CORE");
    await expect
      .poll(() => railTargets(alicePage))
      .toEqual([
        "/",
        "/agents",
        "/priorities",
        "/delivery",
        "/pipeline",
        "/projects/CORE",
        "/projects/PRIV",
        "/settings",
      ]);
    await expect(alicePage.getByRole("button", { name: PEEK })).toBeVisible();

    // bob's rail is Inbox, Agents, PRIV alone and Settings, with no Peek, on a page where a
    // full member is offered one.
    const bobPage = await bob.newPage();
    await bobPage.goto("/projects/PRIV");
    await expect
      .poll(() => railTargets(bobPage))
      .toEqual(["/", "/agents", "/projects/PRIV", "/settings"]);
    await expect(bobPage.getByRole("button", { name: PEEK })).toHaveCount(0);

    // Each view over every project answers him as an address Dispatch does not know.
    for (const path of ["/priorities", "/delivery", "/pipeline"]) {
      await bobPage.goto(path);
      await expect(bobPage.getByRole("heading", { name: "Page not found" })).toBeVisible();
      await expect(bobPage).toHaveTitle("Not found · Dispatch");
    }
  } finally {
    await alice.close();
    await bob.close();
  }
});
