import { EMPTY_STATE_LINES } from "../web/src/features/issue-page/page-words";
import { createIssue, createProject } from "./api";
import { withIssuePage } from "./issue-page-option";
import { editTitleButton } from "./issue-page-p";
import { closeRail, openRail } from "./rail";
import {
  FRAME_SHELL_CHUNK,
  fetchedChunk,
  holdAssetUntilReleased,
  recordTabReloads,
  tabReloads,
} from "./shell";
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
  // Each caller has waited for the page it stands on, today's or the new one, to draw, so one
  // count says whether it is the new one.
  const onNewPage = (await page.locator("[data-issue-page]").count()) > 0;
  // The shell draws the rail, which holds Open navigation, in the render that routes to the new
  // page, from a width query read as it renders, so one count says whether this width has it.
  const navigationControls = await page.getByRole("button", { name: "Open navigation" }).count();
  // The new page's frame (part D) draws no navigation below 768 px until the header's menu
  // comes, so there the reader goes to Settings with its key, `g s`, which moves with no reload
  // as a link does.
  const frameWithoutNavigation = onNewPage && navigationControls === 0;
  if (frameWithoutNavigation) {
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
    await page.keyboard.press("g");
    await page.keyboard.press("s");
  } else {
    const links = await navigation(page);
    await links.getByRole("link", { exact: true, name: "Settings" }).click();
  }
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

/** The new page as the parts so far draw it: the title as the page's one `h1`, no tab, the
 *  header, which draws the key and the title, the view row, the outline's one line for an issue
 *  that holds nothing, and nothing else but what the foot draws - the box to write in and the
 *  read mark - which `e2e/issue-page-l.e2e.ts` reads. */
async function expectNewPageAlone(page: Page, issue: Seeded): Promise<void> {
  const newPage = page.locator("[data-issue-page]");
  await expect(newPageHeading(page)).toHaveText(issue.title);
  await expect(newPage.locator("h1"), "the new page draws an h1 beside its title").toHaveCount(1);
  // Reads the whole page: a chooser that drew both pages fails here (R4: in place of today's page).
  await expect(page.getByRole("tab", { exact: true, name: "Spec" })).toHaveCount(0);
  await expect(newPage.getByRole("tab")).toHaveCount(0);
  // A part that fills another slot adds its selector here: so far the header
  // (`[data-issue-header]`) and the view row (`[data-filter-row]`) are filled, the outline draws
  // its one line for an issue that holds nothing at the issue's own address, and each other slot
  // drawn so far adds no text or control: a view's address (`/spec`) draws nothing in the body
  // until that view's own slot is filled. The box at the foot (`[data-testid="issue-composer"]`)
  // and the read mark at the outline's end (`[data-outline-end]`) are left out of both counts.
  const header = newPage.locator("[data-issue-header]");
  const row = newPage.locator("[data-filter-row]");
  await expect(header).toHaveCount(1);
  await expect(row).toHaveCount(1);
  const atIssue = new URL(page.url()).pathname === `/issues/${issue.key}`;
  const body = atIssue ? EMPTY_STATE_LINES.conversation : "";
  const footSlot = '[data-testid="issue-composer"], [data-outline-end]';
  await expect
    .poll(
      () =>
        newPage.evaluate((root, left) => {
          let text = (root as HTMLElement).innerText;
          for (const part of root.querySelectorAll<HTMLElement>(
            `[data-issue-header], [data-filter-row], ${left}`
          )) {
            text = text.replace(part.innerText, " ");
          }
          return text.replace(/\s+/g, " ").trim();
        }, footSlot),
      { message: "the new page draws text outside its header and its view row" }
    )
    .toBe(body);
  const controls = "button, a, input, select, textarea";
  expect(
    (await newPage.locator(controls).count()) -
      (await newPage.locator(`:is(${footSlot}) :is(${controls})`).count()),
    "the new page draws a control outside its header and its view row"
  ).toBe((await header.locator(controls).count()) + (await row.locator(controls).count()));
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

  // The landing carries the issue as well: its one answer seeds the issue under the key the new
  // page reads it by (`web/src/api/queries.ts`, `seedIssuePage`), so a cold load given that part
  // would never ask the read held above, and the page would draw no loading state and no
  // failure of its own. This row is about the new page's own states while that read is held,
  // refused or answered, so the landing answers the issue as it answers an issue it could not
  // read - null (`issue.detail` in `packages/envoy/internal/dispatch/api/landing.go`) - which the
  // seed leaves alone, and the page reads the issue from its own route. Every other part of the
  // answer stays as the server gave it. Each of the four loads below is a cold load and asks the
  // landing again, so this holds for each of them; the Retry asks the route, not the landing,
  // since the page does not reload. It stays past `page.unroute(issueRead)` and changes nothing
  // there: the landing of the missing key has no issue to remove, and the address of the unknown
  // tab is no issue page, so its landing is the Inbox's and carries no issue page at all.
  await page.route("**/api/v1/landing*", async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as {
      page: { issue: Record<string, unknown> | null };
    };
    const landed = body.page.issue;
    await route.fulfill({
      json:
        landed === null
          ? body
          : { ...body, page: { ...body.page, issue: { ...landed, issue: null } } },
      response,
    });
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

/** The Settings switch, on `page`. */
function issuePageCheckbox(page: Page): Locator {
  return page
    .getByRole("region", { name: "Preview" })
    .getByRole("checkbox", { exact: true, name: "New issue page" });
}

// The switch changes which issue page the reader is drawn and nothing above it: the shell, the
// page the reader is on and every part drawn beside them stay mounted for both choices
// (`AppShell`, `web/src/app.tsx`). So the checkbox the reader pressed is the one that keeps the
// focus, in each direction. Two shells for the two choices would mount everything again at the
// press, and the focus would land on the document's body.
// SLOW(>=2s): a cold load of Settings, whose sections each read the server before the switch is
// pressed twice.
test("turning the new issue page on and off in Settings keeps the focus on its checkbox @phone", async ({
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  if (!isPhone) await page.setViewportSize(desktop);
  await page.goto("/settings");
  const checkbox = issuePageCheckbox(page);
  await checkbox.focus();
  await page.keyboard.press("Space");
  await expect(checkbox).toBeChecked();
  await expect(checkbox).toBeFocused();
  await page.keyboard.press("Space");
  await expect(checkbox).not.toBeChecked();
  await expect(checkbox).toBeFocused();
  if (isPhone) await assertPageFits(page);
});

// A choice made in another tab reaches this one through the window's `storage` event
// (`web/src/features/issue-page/stored-choice.ts`), and it too changes nothing above the issue
// page: this tab's Settings stays mounted, with what the reader typed into its new-project form
// and has not sent (`web/src/features/settings/ProjectsSection.tsx`). Two shells for the two
// choices would mount this tab's whole page again at the other tab's press, and the form would
// come back empty.
// SLOW(>=2s): two tabs each load Settings cold, and the other tab's switch is pressed twice.
test("another tab turning the new issue page on and off leaves this tab's unsent form text @phone", async ({
  context,
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  const second = await context.newPage();
  for (const tab of [page, second]) {
    if (!isPhone) await tab.setViewportSize(desktop);
    await tab.goto("/settings");
  }
  const projectName = page.getByRole("textbox", { exact: true, name: "Name" });
  await projectName.fill("Unsent project");

  await issuePageCheckbox(second).check();
  // This tab's own switch reads the choice the other tab stored, so the event has reached it.
  await expect(issuePageCheckbox(page)).toBeChecked();
  await expect(projectName).toHaveValue("Unsent project");
  await issuePageCheckbox(second).uncheck();
  await expect(issuePageCheckbox(page)).not.toBeChecked();
  await expect(projectName).toHaveValue("Unsent project");
  if (isPhone) await assertPageFits(page);
});

// The same for an agent token's value, which Settings shows once, as the token is minted, and
// never again (`web/src/features/settings/AgentTokensSection.tsx`): a reader who had not copied
// it yet when the other tab turned the new page on would have lost it for good.
// SLOW(>=2s): two tabs each load Settings cold, and the token is minted through the server.
test("another tab turning the new issue page on leaves this tab's shown-once agent token @phone", async ({
  context,
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  const second = await context.newPage();
  for (const tab of [page, second]) {
    if (!isPhone) await tab.setViewportSize(desktop);
    await tab.goto("/settings");
  }
  await page.getByLabel("Token label").fill("Architect");
  await page.getByRole("button", { name: "New token" }).click();
  const shown = page
    .getByRole("status")
    .filter({ hasText: "Copy this token now." })
    .locator("code")
    .first();
  // textContent waits for the code element to attach, and an element's text is never null, so
  // the `?? ""` in the last check below only narrows the type.
  const token = await shown.textContent();
  expect(token).toMatch(/^dsp_/);

  await issuePageCheckbox(second).check();
  await expect(issuePageCheckbox(page)).toBeChecked();
  await expect(shown).toHaveText(token ?? "");
});

// The new page's frame is a chunk of its own (`web/src/features/issue-page/frame-chunk.ts`),
// which the entry starts at boot only in a browser where some login chose the new page. So
// Settings starts it as it draws the switch (`IssuePagePreference.tsx`): a reader who turns the
// page on finds it in, or on its way, rather than waiting on a download only the press began.
// SLOW(>=2s): a cold load of Settings, read until its frame's download is in the page's timing.
test("Settings asks for the new issue page's frame before the reader turns it on @phone", async ({
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  if (!isPhone) await page.setViewportSize(desktop);
  await page.goto("/settings");
  await expect(issuePageCheckbox(page)).not.toBeChecked();
  await fetchedChunk(page, FRAME_SHELL_CHUNK);
  if (isPhone) await assertPageFits(page);
});

// On an issue's address the frame changes the shell (`frameOf`, `frame-shell.tsx`), and its chunk
// can still be on the wire when another tab turns the new page on. This tab then draws the new page
// in today's shell, and draws the frame around it once the chunk is in (`ChosenShell`,
// `web/src/app.tsx`). The late frame changes what the reader sees, and nothing else: the shell and
// the page in it stay mounted, so the text the reader typed into the page meanwhile, and the focus
// in it, are still there. Two shells for the two states, or the page drawn again in the frame,
// would take both. Today's page itself goes at the other tab's press, with or without the frame, as
// the switch says (`IssuePage.tsx`), so the text goes into the new page's title, the issue page's
// own text field, while the frame is held.
// SLOW(>=2s): two tabs each load cold, the issue and Settings, and the other tab's switch is
// pressed while this tab's frame is held on the wire.
test("another tab turning the new issue page on while this tab's frame is on the wire leaves the text typed into the page, and the focus, as the frame comes @phone", async ({
  context,
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  const issue = await seedIssue();
  const second = await context.newPage();
  if (!isPhone) {
    await page.setViewportSize(desktop);
    await second.setViewportSize(desktop);
  }
  const frameChunk = await holdAssetUntilReleased(page, FRAME_SHELL_CHUNK);
  await page.goto(`/issues/${issue.key}`);
  await expectTodaysPage(page);
  await second.goto("/settings");

  await issuePageCheckbox(second).check();
  await expect(newPageHeading(page)).toHaveText(issue.title);
  // The frame's download is held from its first request on, so `url` answers only once the switch
  // has asked for it; the poll bounds that wait and names a switch that asks for no frame.
  let frameAsked = false;
  void frameChunk.url.then(() => {
    frameAsked = true;
  });
  await expect.poll(() => frameAsked, { message: "the switch asked for no frame" }).toBe(true);
  // The frame marks its `main` as no place for reference cards (`frame-shell.tsx`), at every
  // width; today's shell does not.
  const frameMain = page.locator("main[data-no-hover-preview]");
  await expect(frameMain, "the frame was drawn with its chunk held").toHaveCount(0);
  // The header lays its Edit title over the title's heading (`IssueTitleRow.tsx`), and that
  // button opens the editor, as a reader's click on the title does.
  await editTitleButton(page).click();
  const title = page.getByRole("textbox", { exact: true, name: "Issue title" });
  const unsent = "Ship the release notes, not sent yet";
  await title.fill(unsent);
  await expect(title).toBeFocused();

  frameChunk.release();
  await expect(frameMain, "the frame did not come once its chunk was in").toHaveCount(1);
  await expect(title, "the frame took the focus from the page's field").toBeFocused();
  await expect(title, "the frame took the text typed into the page").toHaveValue(unsent);
  if (isPhone) await assertPageFits(page);
});

// The new page's frame can fail to download while the browser is online - a deployment that
// replaced the build the page loaded - with no recovery reload to follow: the tab's three reloads
// are spent here, as a tab whose assets kept failing has spent them. The shell is then today's,
// the one every reader who has not turned the new page on is drawn, which needs nothing of that
// chunk (`web/src/app.tsx`): the reader stays signed in, with today's navigation, and the issue's
// address draws the new page without its frame. A failure thrown into the render would reach the
// app's top boundary (`web/src/main.tsx`), which replaces the whole app. The failure is not kept
// for the life of the page: once the chunk can come down, turning the new page off and on again
// in Settings asks for the frame again, and the issue's address draws the new page in it.
// SLOW(>=2s): two loads of the issue, the first only to spend the tab's reloads in its own
// storage, and a walk to Settings and back by links.
test("with the new issue page's frame failing to download online and no reload to follow, the reader gets today's shell, signed in, and turning the page off and on asks again @phone", async ({
  context,
  page,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  const issue = await seedIssue();
  if (!isPhone) await page.setViewportSize(desktop);
  await page.goto(`/issues/${issue.key}`);
  await expectTodaysPage(page);
  const spent = await recordTabReloads(page, [3, 2, 1]);

  await withIssuePage(context, "alice");
  const refused: string[] = [];
  await page.route(FRAME_SHELL_CHUNK, (route) => {
    refused.push(route.request().url());
    return route.abort();
  });
  await page.goto(`/issues/${issue.key}`);
  await expect(newPageHeading(page)).toHaveText(issue.title);
  expect(refused, "the new page's shell was never asked for").not.toEqual([]);
  await expect(page.getByTestId("error-boundary")).toHaveCount(0);
  await expect(page.getByTestId("icon-rail")).toHaveCount(0);
  if (isPhone) await assertPageFits(page);
  await openRail(page);
  await expect(page.getByText("Signed in as alice")).toBeVisible();
  expect(await tabReloads(page), "the failure reloaded the page").toEqual(spent);

  await page.unroute(FRAME_SHELL_CHUNK);
  await closeRail(page);
  await setMarker(page);
  await openSettings(page);
  const checkbox = issuePageCheckbox(page);
  await expect(checkbox).toBeChecked();
  await checkbox.uncheck();
  await checkbox.check();
  await openIssue(page, issue.key);
  await expect(newPageHeading(page)).toHaveText(issue.title);
  // The frame marks its `main` as no place for reference cards (`frame-shell.tsx`), at every
  // width; today's shell does not.
  await expect(
    page.locator("main[data-no-hover-preview]"),
    "turning the new page off and on again drew it without its frame"
  ).toHaveCount(1);
  await expect(page.getByTestId("error-boundary")).toHaveCount(0);
  expect(await marked(page), "the walk reloaded the page").toBe(true);
  if (isPhone) await assertPageFits(page);
});
