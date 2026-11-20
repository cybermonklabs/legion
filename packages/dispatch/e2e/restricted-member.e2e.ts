/** A restricted member against the real server: Dispatch answers bob's `/auth/whoami` with
 *  `member_kind: "restricted"` and lists him PRIV, which names him, and not the public CORE; the
 *  rail draws what those answers say, Delivery shows him the pull requests of PRIV's issues
 *  alone, and the deployment's own pages answer Page not found. On every page of his, the
 *  dashboard asks him for nothing Dispatch refuses him.
 *
 *  The full lane runs it. The pull-request lane's real-stack rows are at their budget of 150
 *  (e2e/__tests__/pull-request-lane.test.ts), so the lane's copy of what the page does with
 *  those answers is the stubbed rows of `restricted-member.stub.e2e.ts`. */
import { baseUrl, createIssue, createProject, userHeaders } from "./api";
import { seedDeliverySettings, TIMELINE_FIXTURE_QUERY } from "./delivery-seed";
import { sql } from "./psql";
import { railTargets } from "./rail";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

/** The restricted member of this file: a person from outside the sign-in domain whom PRIV's
 *  access list names. */
const BOB = "bob@outside.example";

const PEEK = "Peek at the inbox without leaving this page";

/** Makes bob a restricted member whom PRIV's access list names: the people row outside sign-in
 *  records for a person an access list names, written here because that sign-in is not what
 *  these rows are about, and alice, who made PRIV and owns it, naming him on its access list, as
 *  its Access tab does. */
async function nameBobOnPriv(): Promise<void> {
  await sql(`insert into people (email, member_kind) values ('${BOB}', 'restricted')`);
  const named = await fetch(
    new URL(`/api/v1/projects/PRIV/access/people/${encodeURIComponent(BOB)}`, baseUrl),
    { headers: await userHeaders("alice"), method: "PUT" }
  );
  expect(named.status, await named.text()).toBe(200);
}

test("a restricted member's rail is Inbox, Agents, Delivery, their projects and Settings, and Priorities and Pipeline are not found", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "PRIV", name: "Private work" });
  await nameBobOnPriv();

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

    // bob's rail is Inbox, Agents, Delivery, PRIV alone and Settings, with no Peek, on a page
    // where a full member is offered one. Delivery's timeline answers him the pull requests whose
    // issue he sees.
    const bobPage = await bob.newPage();
    await bobPage.goto("/projects/PRIV");
    await expect
      .poll(() => railTargets(bobPage))
      .toEqual(["/", "/agents", "/delivery", "/projects/PRIV", "/settings"]);
    await expect(bobPage.getByRole("button", { name: PEEK })).toHaveCount(0);

    // Priorities and Pipeline, the views over every project, answer him as an address Dispatch
    // does not know.
    for (const path of ["/priorities", "/pipeline"]) {
      await bobPage.goto(path);
      await expect(bobPage.getByRole("heading", { name: "Page not found" })).toBeVisible();
      await expect(bobPage).toHaveTitle("Not found · Dispatch");
    }
  } finally {
    await alice.close();
    await bob.close();
  }
});

/** Every refusal the page draws from Dispatch: each answer of 403, by its method, its path and
 *  the code it carried. An answer is recorded the moment it arrives and its code once its body is
 *  read; `read` waits for every body still being read, so no refusal that has arrived is missed. */
function refusalsOf(page: Page): { read: () => Promise<string[]> } {
  const refused: Promise<string>[] = [];
  page.on("response", (response) => {
    if (response.status() !== 403) return;
    const call = `${response.request().method()} ${new URL(response.url()).pathname}`;
    refused.push(
      response.json().then(
        (body: { code?: string }) => `${call} ${body.code ?? ""}`.trim(),
        () => call
      )
    );
  });
  return { read: () => Promise.all(refused) };
}

// Dispatch refuses a restricted member every read of the deployment's own rows (403
// DEPLOYMENT_READ_REFUSED, `readclass.Deployment` in api/read_classes.go). The server's own
// refusal is the instrument, so a read of that class any page makes, today's or one added later,
// shows here by name: bob opens each page of his and draws none. The row ends on one such read
// made on purpose, which must show, so the instrument is seen to catch a refusal.
test("a restricted member's pages draw no refusal from Dispatch", async ({ browser }) => {
  await createProject({ key: "PRIV", name: "Private work" });
  await nameBobOnPriv();
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

    await page.goto("/agents/broadcasts");
    await expect.soft(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
    // The machine-login page is his: he approves there the device login of his own agents, and
    // it asks him for no list of the deployment's machine logins, which Dispatch refuses him.
    await page.goto("/credentials/machine");
    await expect.soft(page.getByLabel("Code shown on the machine")).toBeVisible();
    await expect.soft(page.getByRole("heading", { name: "Page not found" })).toHaveCount(0);
    // The instrument's control, made last: a read of the deployment's rows from bob's page is
    // refused and shows by name, and a read the pages above made, which answered before it, would
    // show beside it.
    const control = "GET /api/v1/priorities DEPLOYMENT_READ_REFUSED";
    await page.evaluate(async () => (await fetch("/api/v1/priorities")).status);
    await expect.poll(() => refused.read()).toContain(control);
    expect(await refused.read()).toEqual([control]);
  } finally {
    await bob.close();
  }
});

/** The deploy run of the Delivery row below, which shipped P1 and P2. */
const RUN = 700;

/** The Delivery row's pull requests and deploy, in the day `TIMELINE_FIXTURE_QUERY` spans: P1 for
 *  CORE's issue `core` and P2 for PRIV's issue `priv`, both merged before run 700 and shipped by
 *  it, and P3, for no issue, merged after it and still waiting. */
async function seedDeliveryOfTwoProjects(core: string, priv: string): Promise<void> {
  await seedDeliverySettings();
  await sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 1, 'feat: a public widget', 'https://github.com/acme/widgets/pull/1', 'octocat',
        '2024-06-01T00:00:00Z', '2024-06-01T00:30:00Z', 3, 1, false, '${core}', ARRAY[]::text[], false),
       ('acme/widgets', 2, 'feat: a private widget', 'https://github.com/acme/widgets/pull/2', 'octocat',
        '2024-06-01T00:10:00Z', '2024-06-01T00:40:00Z', 5, 2, false, '${priv}', ARRAY[]::text[], false),
       ('acme/widgets', 3, 'feat: an unclaimed widget', 'https://github.com/acme/widgets/pull/3', 'octocat',
        '2024-06-01T02:00:00Z', '2024-06-01T03:00:00Z', 2, 2, false, null, ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
     ) VALUES
       ('acme/widgets', ${RUN}, 'deploy', 'deadbeef', '2024-06-01T01:00:00Z',
        '2024-06-01T01:30:00Z', '2024-06-01T01:30:00Z', '2024-06-01T01:40:00Z', 'success',
        'https://github.com/acme/widgets/actions/runs/${RUN}', 'main', 'push')`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) VALUES
       ('acme/widgets', ${RUN}, 'build', '2024-06-01T01:30:00Z', '2024-06-01T01:32:00Z', 'success'),
       ('acme/widgets', ${RUN}, 'widgets-release / widgets-release', '2024-06-01T01:32:00Z', '2024-06-01T01:40:00Z', 'success')`
  );
}

// Delivery's timeline and measures have the read class `project` (api/read_classes.go): they
// answer a restricted member the pull requests whose issue he sees, and the runs that shipped
// them with those pull requests alone. The page's other reads are the deployment's - a run's
// jobs, the delivery settings, the session-group writes - so bob's page makes none of them and
// leaves out what each would draw: the run's jobs, the setup form and the groups editor. alice,
// a full member, sees all three pull requests, the run with both it shipped, its jobs and the
// editor, so each absence on bob's page is the read rule's and not the seed's.
test("a restricted member reads Delivery for the pull requests of the projects that name them", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "PRIV", name: "Private work" });
  await nameBobOnPriv();
  const core = await createIssue({ project: "CORE", title: "Ship the public widgets" });
  const priv = await createIssue({ project: "PRIV", title: "Ship the private widgets" });
  await seedDeliveryOfTwoProjects(core.key, priv.key);
  const timeline = `/delivery?${TIMELINE_FIXTURE_QUERY}&mode=list`;
  const runButton = { name: `Open deploy run #${RUN}` };

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, BOB);
  try {
    const alicePage = await alice.newPage();
    await alicePage.goto(timeline);
    await expect(alicePage.locator("tbody tr[aria-rowindex]")).toHaveCount(3);
    await expect(alicePage.getByRole("button", { name: "Edit groups" })).toBeVisible();
    await alicePage.getByRole("button", runButton).first().click();
    const aliceDetails = alicePage.getByRole("dialog", { name: "Details" });
    await expect(aliceDetails.getByText("PRs shipped (2)")).toBeVisible();
    await expect(aliceDetails.getByText("widgets-release / widgets-release")).toBeVisible();

    const page = await bob.newPage();
    const refused = refusalsOf(page);
    await page.goto(timeline);
    const rows = page.locator("tbody tr[aria-rowindex]");
    await expect(rows).toHaveCount(1);
    await expect(rows).toContainText("feat: a private widget");
    await expect(page.getByText("feat: a public widget")).toHaveCount(0);
    await expect(page.getByText("feat: an unclaimed widget")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Edit groups" })).toHaveCount(0);

    // The run's drill-down: the run, its link and P2 alone of what it shipped, and no jobs.
    await page.getByRole("button", runButton).click();
    const details = page.getByRole("dialog", { name: "Details" });
    await expect(details.getByRole("heading", { name: `Deploy: run #${RUN}` })).toBeVisible();
    await expect(details.getByRole("link", { name: "Open run" })).toBeVisible();
    await expect(details.getByText("PRs shipped (1)")).toBeVisible();
    await expect(details.getByText("acme/widgets#2 — feat: a private widget")).toBeVisible();
    await expect(details.getByText("feat: a public widget")).toHaveCount(0);
    await expect(details.getByText("Jobs", { exact: true })).toHaveCount(0);
    await expect(details.getByText("widgets-release / widgets-release")).toHaveCount(0);
    await expect(details.getByText("The run's jobs could not be read.")).toHaveCount(0);
    await details.getByRole("button", { name: "Close details" }).click();

    // With no delivery settings the page says so, and offers bob no setup form, which would read
    // and write the deployment's settings.
    await sql("DELETE FROM delivery_settings");
    await page.goto("/delivery");
    await expect(page.getByText("Delivery is not set up yet.")).toBeVisible();
    await expect(page.getByRole("heading", { name: "Set up the delivery timeline" })).toHaveCount(
      0
    );
    await expect(page.getByLabel("Deploy repository")).toHaveCount(0);

    // The instrument's control, made last, as in the row above: a read of the deployment's rows
    // from bob's page is refused and shows by name, beside any read the page made above.
    const control = "GET /api/v1/priorities DEPLOYMENT_READ_REFUSED";
    await page.evaluate(async () => (await fetch("/api/v1/priorities")).status);
    await expect.poll(() => refused.read()).toContain(control);
    expect(await refused.read()).toEqual([control]);
  } finally {
    await alice.close();
    await bob.close();
  }
});
