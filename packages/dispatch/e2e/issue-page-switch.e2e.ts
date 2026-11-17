import { createIssue, createProject } from "./api";
import { withIssuePage } from "./issue-page-option";
import { openRail } from "./rail";
import { expect, type Locator, type Page, test } from "./test";
import { assertPageFits } from "./unclipped";
import { asUser, signIn, withAlice } from "./users";

/**
 * The reader's switch between today's issue page and the new one (dispatch://INFRA-3903): the
 * checkbox "New issue page" under "Preview" on Settings, the store that every tab of one login
 * reads, the chooser at an issue's address, and `withIssuePage` (e2e/issue-page-option.ts),
 * which later rows turn the new page on with. Each row reads the page at 1440 px and again at
 * 390 px, and on the phone asserts that every page it ends on fits the viewport.
 *
 * The project and the issue hold neither `outline` nor `tabs`, because Settings lists the
 * project and the first row reads the whole of Settings for those two words.
 */

declare global {
  interface Window {
    /** Set by a row before the reader's first click; a reload drops it, a link the app follows
     *  keeps it. */
    __issuePageMarker?: true;
  }
}

const desktop = { height: 900, width: 1440 };
const project = "CORE";
const note = "Shows an issue as the new page, which is not finished. Kept in this browser.";

type Seeded = { readonly key: string; readonly title: string };

async function seedIssue(): Promise<Seeded> {
  await createProject({ key: project, name: "Core" });
  return createIssue({ project, title: "Ship the release notes" });
}

/** The navigation the reader follows a link of, opened through `openRail`: the sheet behind
 *  "Open navigation" at 390 px, the page's own column at 1440 px. */
async function navigation(page: Page): Promise<Locator> {
  await openRail(page);
  return page.getByRole("navigation", { name: "Navigation" });
}

async function openSettings(page: Page): Promise<void> {
  const links = await navigation(page);
  await links.getByRole("link", { exact: true, name: "Settings" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();
}

/** Opens the issue as a reader does: the project's link in the navigation, then the issue's link
 *  on the project's page. */
async function openIssue(page: Page, key: string): Promise<void> {
  const links = await navigation(page);
  await links.locator(`a[href="/projects/${project}"]`).click();
  await page.locator(`a[data-issue-link][href="/issues/${key}"]`).click();
  await expect(page).toHaveURL(`/issues/${key}`);
}

/** What the browser keeps under `dispatch.issue.page:<login>`. The key is written as a literal,
 *  as other rows write storage keys, so a change of the key in the code fails these rows. */
function storedChoice(page: Page, login: string): Promise<string | null> {
  return page.evaluate((key) => window.localStorage.getItem(key), `dispatch.issue.page:${login}`);
}

async function setMarker(page: Page): Promise<void> {
  await page.evaluate(() => {
    window.__issuePageMarker = true;
  });
}

function marked(page: Page): Promise<boolean> {
  return page.evaluate(() => window.__issuePageMarker === true);
}

async function expectTodaysPage(page: Page): Promise<void> {
  await expect(page.getByRole("tab", { exact: true, name: "Spec" })).toBeVisible();
  await expect(page.locator("[data-issue-page]")).toHaveCount(0);
}

function newPageHeading(page: Page): Locator {
  return page.locator("[data-issue-page]").getByRole("heading", { level: 1 });
}

/** The new page as this part draws it: the key and the title as the page's one `h1`, no tab, and
 *  nothing else. */
async function expectNewPageAlone(page: Page, issue: Seeded): Promise<void> {
  const newPage = page.locator("[data-issue-page]");
  await expect(newPageHeading(page)).toHaveText(issue.title);
  // Reads the whole page: a chooser that drew both pages fails here (R4: in place of today's page).
  await expect(page.getByRole("tab", { exact: true, name: "Spec" })).toHaveCount(0);
  await expect(newPage.getByRole("tab")).toHaveCount(0);
  // The part that fills a slot changes these two: each slot drawn so far adds no text or control.
  await expect(newPage).toHaveText(`${issue.key} ${issue.title}`, { useInnerText: true });
  await expect(newPage.locator("button, a, input, select, textarea")).toHaveCount(0);
}

/** Every request to `/api/` or `/ws/` the page sends, with its body, and every frame it sends on
 *  a socket, into `sent`; every request for the new page's chunk into `outlineChunks`. */
function recordTraffic(page: Page, sent: string[], outlineChunks: string[]): void {
  page.on("request", (request) => {
    const { pathname } = new URL(request.url());
    if (pathname.startsWith("/api/") || pathname.startsWith("/ws/")) {
      sent.push(`${request.method()} ${request.url()} ${request.postData() ?? ""}`);
    }
    if (pathname.startsWith("/assets/IssueOutlinePage-")) {
      outlineChunks.push(pathname);
    }
  });
  page.on("websocket", (socket) => {
    sent.push(`socket ${socket.url()}`);
    socket.on("framesent", ({ payload }) => {
      sent.push(`frame ${socket.url()} ${String(payload)}`);
    });
  });
}

// SLOW(>=2s): five loads of the issue - the first, its Spec address, bob's on this context and in
// a context of his own, alice's again - and two walks to Settings and back by links.
test("a reader turns the new issue page on in Settings and off again, by links and with no reload @phone", async ({
  browser,
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  const issue = await seedIssue();
  const sent: string[] = [];
  const outlineChunks: string[] = [];
  recordTraffic(page, sent, outlineChunks);
  if (!isPhone) await page.setViewportSize(desktop);

  // A reader who has stored nothing gets today's page, and downloads nothing of the new one.
  await page.goto(`/issues/${issue.key}`);
  await expectTodaysPage(page);
  expect(outlineChunks).toEqual([]);

  await setMarker(page);
  await openSettings(page);
  const preview = page.getByRole("region", { name: "Preview" });
  const checkbox = preview.getByRole("checkbox", { exact: true, name: "New issue page" });
  await expect(checkbox).not.toBeChecked();
  await expect(preview.getByText(note, { exact: true })).toBeVisible();
  await expect(checkbox).toHaveAccessibleDescription(note);
  if (isPhone) {
    const label = await preview.locator("label", { has: page.getByRole("checkbox") }).boundingBox();
    expect(label?.height ?? 0).toBeGreaterThanOrEqual(44);
  }
  // Settings is read once no section shows a loading line. The Preview section, the only text this
  // part adds to Settings, is drawn by then.
  const main = page.getByRole("main");
  await expect(main.getByText(/^Loading/)).toHaveCount(0);
  expect(await main.innerText()).not.toMatch(/\b(outline|tabs)\b/i);
  expect(outlineChunks).toEqual([]);

  await checkbox.check();
  expect(await storedChoice(page, "alice")).toBe("outline");
  await openIssue(page, issue.key);
  await expectNewPageAlone(page, issue);
  expect(await marked(page)).toBe(true);
  // The chunk the setting turned on was asked for, so the two readings of none above are of it.
  expect(outlineChunks).not.toEqual([]);

  await page.goto(`/issues/${issue.key}/spec`);
  await expectNewPageAlone(page, issue);

  // The value is the signed-in login's: bob, signed in on the same browser, stored nothing.
  await signIn(page.context(), "bob");
  await page.goto(`/issues/${issue.key}`);
  await expectTodaysPage(page);
  expect(await storedChoice(page, "bob")).toBeNull();
  const bob = await asUser(browser, "bob");
  try {
    const bobPage = await bob.newPage();
    recordTraffic(bobPage, sent, outlineChunks);
    if (!isPhone) await bobPage.setViewportSize(desktop);
    await bobPage.goto(`/issues/${issue.key}`);
    await expectTodaysPage(bobPage);
    if (isPhone) await assertPageFits(bobPage);
  } finally {
    await bob.close();
  }
  await signIn(page.context(), "alice");
  await page.goto(`/issues/${issue.key}`);
  await expect(newPageHeading(page)).toHaveText(issue.title);
  await expect(page.getByRole("tab", { exact: true, name: "Spec" })).toHaveCount(0);

  await setMarker(page);
  await openSettings(page);
  await expect(checkbox).toBeChecked();
  await checkbox.uncheck();
  expect(await storedChoice(page, "alice")).toBe("tabs");
  await openIssue(page, issue.key);
  await expectTodaysPage(page);
  expect(await marked(page)).toBe(true);
  if (isPhone) await assertPageFits(page);

  expect(sent.filter((line) => /outline|issue\.page/.test(line))).toEqual([]);
});

// SLOW(>=2s): two pages of one context each load the issue, and the second draws the other page
// twice, as the first turns the setting on in Settings and off again.
test("a second tab of the same login follows the setting with no reload @phone", async ({
  context,
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  const issue = await seedIssue();
  const second = await context.newPage();
  for (const tab of [page, second]) {
    if (!isPhone) await tab.setViewportSize(desktop);
    await tab.goto(`/issues/${issue.key}`);
    await expectTodaysPage(tab);
  }
  await setMarker(second);

  await openSettings(page);
  const checkbox = page
    .getByRole("region", { name: "Preview" })
    .getByRole("checkbox", { exact: true, name: "New issue page" });
  await checkbox.check();
  await expect(newPageHeading(second)).toHaveText(issue.title);
  await expect(second.getByRole("tab", { exact: true, name: "Spec" })).toHaveCount(0);
  expect(await marked(second)).toBe(true);
  if (isPhone) await assertPageFits(second);

  await checkbox.uncheck();
  await expectTodaysPage(second);
  expect(await marked(second)).toBe(true);
  if (isPhone) {
    await assertPageFits(page);
    await assertPageFits(second);
  }
});

// SLOW(>=2s): two contexts, the fixture's and withAlice's, each load the issue's new page.
test("withIssuePage turns the new issue page on at the first load of the default context and of a context the row makes @phone", async ({
  browser,
  context,
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  const issue = await seedIssue();

  await withIssuePage(context, "alice");
  if (!isPhone) await page.setViewportSize(desktop);
  await page.goto(`/issues/${issue.key}`);
  await expect(newPageHeading(page)).toHaveText(issue.title);
  await expect(page.getByRole("tab", { exact: true, name: "Spec" })).toHaveCount(0);
  if (isPhone) await assertPageFits(page);

  await withAlice(browser, async (alice) => {
    await withIssuePage(alice.context(), "alice");
    if (!isPhone) await alice.setViewportSize(desktop);
    await alice.goto(`/issues/${issue.key}`);
    await expect(newPageHeading(alice)).toHaveText(issue.title);
    await expect(alice.getByRole("tab", { exact: true, name: "Spec" })).toHaveCount(0);
    if (isPhone) await assertPageFits(alice);
  });
});

// SLOW(>=2s): four loads of the new page, two of which wait out the two retries a refused read
// and the server's 404 each get before the page says so.
test("with the setting on, an issue's loading, failure, absence and refused address are drawn inside the new page @phone", async ({
  context,
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  const issue = await seedIssue();
  await withIssuePage(context, "alice");
  if (!isPhone) await page.setViewportSize(desktop);
  const newPage = page.locator("[data-issue-page]");
  const heading = newPageHeading(page);

  // The issue's own read, and no other read under its path, is held until released, then passed
  // on, or refused while `failing` is set.
  let failing = false;
  const held = Promise.withResolvers<void>();
  const issueRead = (url: URL) => url.pathname === `/api/v1/issues/${issue.key}`;
  await page.route(issueRead, async (route) => {
    if (failing) return route.fulfill({ json: { code: "INTERNAL", error: "boom" }, status: 500 });
    await held.promise;
    return route.fallback();
  });

  // Each state below is the first draw of an address the reader opens, so each is a load.
  await page.goto(`/issues/${issue.key}`);
  await expect(newPage.getByText("Loading issue…", { exact: true })).toBeVisible();
  held.resolve();
  await expect(heading).toHaveText(issue.title);
  await expect(page).toHaveTitle(`${issue.key} · ${issue.title} · Dispatch`);
  if (isPhone) await assertPageFits(page);

  // A refused read is asked again twice, `QUERY_RETRY_DELAY_MS` apart
  // (web/src/api/query-client.ts), before the page says it failed.
  failing = true;
  await page.goto(`/issues/${issue.key}`);
  await expect(heading).toHaveText("Couldn't load this issue");
  const failure = newPage.getByRole("alert").filter({ hasText: "Couldn't load this issue." });
  await expect(failure).toBeVisible();
  if (isPhone) await assertPageFits(page);
  await page.unroute(issueRead);
  await failure.getByRole("button", { name: "Retry" }).click();
  await expect(heading).toHaveText(issue.title);

  const missing = `${project}-999`;
  await page.goto(`/issues/${missing}`);
  await expect(heading).toHaveText("Issue not found");
  await expect(
    newPage.getByText(`${missing} doesn't exist, or you don't have access to it.`, { exact: true })
  ).toBeVisible();
  await expect(page).toHaveTitle("Dispatch");
  if (isPhone) await assertPageFits(page);

  await page.goto(`/issues/${issue.key}/no-such-tab`);
  await expect(heading).toHaveText("Page not found");
  if (isPhone) await assertPageFits(page);
});
