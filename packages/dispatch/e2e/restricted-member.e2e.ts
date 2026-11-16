/** A restricted member against the real server: Dispatch answers bob's `/auth/whoami` with
 *  `member_kind: "restricted"` and lists him PRIV, which names him, and not the public CORE; the
 *  rail draws what those answers say, and the deployment's own pages answer Page not found. On
 *  every page of his, the dashboard asks him for nothing Dispatch refuses him.
 *
 *  The full lane runs it. The pull-request lane's real-stack rows are at their budget of 150
 *  (e2e/__tests__/pull-request-lane.test.ts), so the lane's copy of what the page does with
 *  those answers is the stubbed rows of `restricted-member.stub.e2e.ts`. */
import { baseUrl, createIssue, createProject, userHeaders } from "./api";
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

test("a restricted member's rail is Inbox, Agents, their projects and Settings, and the views over every project are not found", async ({
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

/** Every refusal the page has drawn from Dispatch so far: each answer of 403, by its method, its
 *  path and the code it carried. */
function refusalsOf(page: Page): string[] {
  const refused: string[] = [];
  page.on("response", async (response) => {
    if (response.status() !== 403) return;
    const { pathname } = new URL(response.url());
    const body = (await response.json().catch(() => ({}))) as { code?: string };
    refused.push(`${response.request().method()} ${pathname} ${body.code ?? ""}`.trim());
  });
  return refused;
}

// Dispatch refuses a restricted member every read of the deployment's own rows (403
// DEPLOYMENT_READ_REFUSED, `readclass.Deployment` in api/read_classes.go). The server's own
// refusal is the instrument, so a read of that class any page makes, today's or one added later,
// shows here by name: bob opens each page of his and draws none.
test("a restricted member's pages draw no refusal from Dispatch", async ({ browser }) => {
  await createProject({ key: "PRIV", name: "Private work" });
  await sql(`insert into people (email, member_kind) values ('${BOB}', 'restricted')`);
  const named = await fetch(
    new URL(`/api/v1/projects/PRIV/access/people/${encodeURIComponent(BOB)}`, baseUrl),
    { headers: await userHeaders("alice"), method: "PUT" }
  );
  expect(named.status, await named.text()).toBe(200);
  const issue = await createIssue({ project: "PRIV", title: "Chart the private reef" });

  const bob = await asUser(browser, BOB);
  try {
    const page = await bob.newPage();
    const refused = refusalsOf(page);

    // Each page's own check is soft, so one run reports every page that drew wrong and then the
    // whole list of refusals, rather than the first page alone.
    await page.goto("/");
    await expect.soft(page.getByText("Nothing needs you")).toBeVisible();
    await expect.soft(page.getByText("Couldn't load credential requests.")).toHaveCount(0);

    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expect.soft(page.getByRole("link", { name: "Broadcasts" })).toHaveCount(0);

    // The pages with no project of their own: his answers, the machines, and his own page.
    await page.goto("/answers");
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await page.goto("/machines");
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await page.goto(`/people/${encodeURIComponent(BOB)}`);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();

    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("heading", { name: "Chart the private reef" })).toBeVisible();
    await expect.soft(page.getByTestId("issue-advances")).toHaveCount(0);

    await page.goto("/projects/PRIV/documents");
    await expect(page.getByRole("button", { name: "New document" })).toBeVisible();
    await page.goto("/projects/PRIV/access");
    await expect(page.getByTestId("access-visibility")).toHaveText("Public");

    // The search bar over a project page offers words that matched nothing as a new issue
    // once it has read the deployment's new-issue settings; bob's never reads them.
    await page.goto("/projects/PRIV/issues");
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const words = "a sextant nobody has written down";
    await page.getByRole("combobox", { name: "Search" }).fill(words);
    await expect(page.getByText(`No results for "${words}"`)).toBeVisible();
    await page.keyboard.press("Escape");

    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Agent tokens" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Architecture sources" })).toBeVisible();
    await expect.soft(page.getByRole("heading", { name: "Groups" })).toHaveCount(0);
    await expect.soft(page.getByRole("heading", { name: "Delivery timeline" })).toHaveCount(0);

    for (const path of ["/agents/broadcasts", "/credentials/machine"]) {
      await page.goto(path);
      await expect.soft(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
    }
    expect(refused).toEqual([]);
  } finally {
    await bob.close();
  }
});
