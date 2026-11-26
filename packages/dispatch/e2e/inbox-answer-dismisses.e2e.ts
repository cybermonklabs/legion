import { setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  replyToMessageDelivery,
} from "./api";
import { describeFocus, frameAndTask, holdsFocus, tabTo } from "./focus";
import { inboxList, inboxPane, paneScroller, selectedRow } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  replyRowId,
  seedAgentThread,
  seedCommentThread,
  sessionActor,
  THREAD_SESSION,
} from "./inbox-threads";
import { sql } from "./psql";
import { READING_PANE_WIDTH } from "./reading-pane";
import { type BrowserContext, expect, type Locator, type Page, type Request, test } from "./test";
import { asUser } from "./users";

/**
 * A person's own answer or reply takes its thread out of their Inbox, with no second press, and
 * the next write by somebody else brings it back (the own-answer rule, R1). The answer's Send and
 * a reply box's Send are held, `aria-disabled`, until their row goes, and the focus then goes to
 * the reading column's own stop for the row the Inbox selects next, never to the page's body.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** The route a Dismiss press posts to, which no answer or reply may post to. */
const DISMISS_PATH = "/api/v1/me/inbox/dismiss";

/** One Inbox row, by the id its `data-inbox-row` names. */
function row(page: Page, id: string): Locator {
  return inboxList(page).locator(`[data-inbox-row="${id}"]`);
}

/** Every `POST` to the dismiss route the page makes. */
function watchDismisses(page: Page): Request[] {
  const posted: Request[] = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && new URL(request.url()).pathname === DISMISS_PATH) {
      posted.push(request);
    }
  });
  return posted;
}

/** Selects a row the way the pointer does: the click lands on the row's own time. */
async function selectRow(page: Page, id: string): Promise<void> {
  await expect(row(page, id)).toBeVisible();
  await row(page, id).locator("time").first().click();
  await expect(row(page, id)).toHaveAttribute("data-inbox-selected", "true");
}

/** The Inbox at a width that draws the reading pane. */
async function inboxPage(
  context: BrowserContext,
  address = "/",
  width = READING_PANE_WIDTH
): Promise<Page> {
  const page = await context.newPage();
  await page.setViewportSize({ height: 900, width });
  await page.goto(address);
  return page;
}

/** Reads `id` in the Dismissed view, on a page of its own, so the Inbox page keeps its state. */
async function expectDismissed(context: BrowserContext, id: string): Promise<void> {
  const dismissed = await inboxPage(context, "/?show=dismissed");
  try {
    await expect(row(dismissed, id)).toBeVisible();
  } finally {
    await dismissed.close();
  }
}

/** Reads `id` out of the Dismissed view, on a page of its own. */
async function expectNotDismissed(context: BrowserContext, id: string): Promise<void> {
  const dismissed = await inboxPage(context, "/?show=dismissed");
  try {
    await expect(inboxList(dismissed)).toBeVisible();
    await expect(row(dismissed, id)).toHaveCount(0);
  } finally {
    await dismissed.close();
  }
}

/** Resolves once the page's own read of its Inbox's rows (`GET /api/v1/me/inbox?state=inbox`),
 *  asked once `after` has resolved, has answered: the page's refresh after a write. */
function inboxReadAfter(page: Page, after: Promise<unknown>): Promise<void> {
  let asking = false;
  void after.then(() => {
    asking = true;
  });
  const read = Promise.withResolvers<void>();
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (
      asking &&
      request.method() === "GET" &&
      url.pathname === "/api/v1/me/inbox" &&
      url.searchParams.get("state") === "inbox"
    ) {
      void request
        .response()
        .then((response) => response?.finished())
        .then(() => read.resolve());
    }
  });
  return read.promise;
}

/**
 * An ask that waits on the reader, whose thread they wrote in before and bob replied in after
 * them, so they hold a row of that thread. Its issue is assigned to them.
 */
async function seedAskWithReaderThread(issueKey: string, question: string): Promise<string> {
  const ask = await createAsk(
    issueKey,
    { options: [{ label: "Ship" }, { label: "Hold" }], question },
    asSession()
  );
  await createComment(issueKey, { ask_id: ask.id, body: "Which way does the canary go?" });
  await createComment(issueKey, { ask_id: ask.id, body: "East first." }, { login: "bob" });
  return ask.id;
}

// SLOW(>=2s): an answer and two replies sent from the pane, then three replies by others waited
// back live; the Dismissed view is read six times, each on a page of its own.
test("an answer or a reply takes the thread out of the Inbox until somebody else writes", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Canary rollout" });
  const ask = await seedAskWithReaderThread(issue.key, "Ship the canary today?");
  const comment = await seedCommentThread(issue.key, "Who holds the pager?", "I do, until six.");
  const agent = await seedAgentThread({ body: "Is the mirror warm?", replies: ["Warming it."] });

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const dismisses = watchDismisses(page);
    // The reader holds a row of the ask's thread, with bob's reply after their own. The Inbox
    // draws no Reply row for it while the ask waits on them: it lists the ask instead.
    const ownThreadRows = await sql(
      `select count(*) from user_threads where login = 'alice' and thread_id = '${ask}' and kind = 'ask' and replied_at is not null and dismissed_at is null`
    );
    expect(Number(ownThreadRows), "alice's own row of the ask's thread before she answers").toBe(1);
    // Before: the ask the reader is waited on, the comment thread and the Agent row.
    for (const id of [ask, replyRowId(comment), agentRowId(agent)]) {
      await expect(row(page, id)).toBeVisible();
    }
    await expect(inboxList(page).locator("[data-inbox-row]")).toHaveCount(3);

    // The answer, in the reading pane. Once the page has read its Inbox again after the answer
    // landed, no row of that ask stays, neither the ask's nor its thread's: at the parent of R1's
    // commit the thread's row is drawn there then, read. The thread stands in the Dismissed view.
    await selectRow(page, ask);
    const pane = inboxPane(page);
    await pane.getByRole("radio", { name: "Ship" }).check();
    const answered = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/v1/asks/${ask}/answer`
    );
    const refreshed = inboxReadAfter(page, answered);
    await pane.getByRole("button", { exact: true, name: "Answer" }).click();
    await refreshed;
    await frameAndTask(page);
    const standing = await inboxList(page)
      .locator(`[data-inbox-row="${ask}"], [data-inbox-row="${replyRowId(ask)}"]`)
      .allInnerTexts();
    expect(
      standing,
      "the rows of the answered ask the Inbox still draws after its refresh"
    ).toEqual([]);
    await expectDismissed(alice, replyRowId(ask));

    // A reply in the comment thread's box, and in the Agent row's box, takes each out the same way.
    await selectRow(page, replyRowId(comment));
    const reply = pane.getByRole("textbox", { name: "Reply" });
    await reply.fill("Then I take it from six.");
    await pane.getByRole("button", { name: "Send" }).click();
    await expect(row(page, replyRowId(comment))).toHaveCount(0);
    await expectDismissed(alice, replyRowId(comment));

    await selectRow(page, agentRowId(agent));
    const agentBox = pane.getByRole("textbox", { name: "Comment" });
    await agentBox.fill("Tell me when it is warm.");
    await pane.getByRole("button", { name: "Send" }).click();
    await expect(row(page, agentRowId(agent))).toHaveCount(0);
    await expectDismissed(alice, agentRowId(agent));

    // No second press took any of them out.
    expect(dismisses).toHaveLength(0);

    // The return: a session replies in the comment thread, bob in the ask's thread, and the
    // session to the direct message. With no reload each row is back in the Inbox, carrying its
    // own unread mark, and out of the Dismissed view.
    await createComment(issue.key, { body: "Six it is.", reply_to: comment }, asSession());
    await createComment(issue.key, { ask_id: ask, body: "Shipping east now." }, { login: "bob" });
    await replyToMessageDelivery(agent, { attempt: 1, body: "Warm now." }, sessionActor(), {
      followUp: true,
    });
    for (const id of [replyRowId(ask), replyRowId(comment), agentRowId(agent)]) {
      await expect(row(page, id)).toBeVisible();
      await expect(row(page, id).getByRole("img", { name: "Unread" })).toBeVisible();
    }
    for (const id of [replyRowId(ask), replyRowId(comment), agentRowId(agent)]) {
      await expectNotDismissed(alice, id);
    }
  } finally {
    await alice.close();
  }
});

/** The width the focus test runs at: the reading-column layout, 1440 pixels wide. */
const FOCUS_WIDTH = 1440;

/** Moves the selection to `id` with the Inbox's own keys, `j` and `k`, from the row it is on. */
async function selectWithKeys(page: Page, id: string): Promise<void> {
  await expect(selectedRow(page)).toHaveCount(1);
  const ids = await inboxList(page)
    .locator("[data-inbox-row]")
    .evaluateAll((rows) => rows.map((drawn) => drawn.getAttribute("data-inbox-row")));
  // The selection was read settled on one row by the line above, and the keys move it only
  // after this read, so one instant's answer is the row the moves start from.
  const from = ids.indexOf(await selectedRow(page).getAttribute("data-inbox-row"));
  const to = ids.indexOf(id);
  expect(to, `${id} among the rows ${ids.join(", ")}`).toBeGreaterThanOrEqual(0);
  for (let at = from; at !== to; at += to > from ? 1 : -1) {
    await page.keyboard.press(to > from ? "j" : "k");
  }
  await expect(row(page, id)).toHaveAttribute("data-inbox-selected", "true");
}

/** What a Send carries while its write is out. */
function heldAttributes(
  send: Locator
): Promise<{ "aria-disabled": string | null; disabled: boolean }> {
  return send.evaluate((button) => ({
    "aria-disabled": button.getAttribute("aria-disabled"),
    disabled: button.hasAttribute("disabled"),
  }));
}

/** Reads `send` held while its write is out: `aria-disabled`, never `disabled`, and the focus
 *  still on it. */
async function expectHeldWithTheFocus(page: Page, send: Locator, what: string): Promise<void> {
  await expect
    .poll(() => heldAttributes(send), { message: `${what} while its write is out` })
    .toEqual({ "aria-disabled": "true", disabled: false });
  expect(
    await holdsFocus(send),
    `while ${what} is held the focus is on ${await describeFocus(page)}`
  ).toBe(true);
}

/**
 * After a Send whose row left, a frame and a task on: the focus is on the column's own stop, the
 * pane's scroller, which is the region named Question body; the pane shows the row the Inbox
 * selected next, by the words `shows` names for it, and not the row that left; nothing scrolled.
 */
async function expectTheColumnStop(
  page: Page,
  shows: Record<string, string>,
  left: string,
  scrollBefore: number
): Promise<void> {
  await frameAndTask(page);
  const scroller = paneScroller(page);
  expect(
    await holdsFocus(scroller),
    `the focus is on the column's own stop, the region named Question body; it is on ${await describeFocus(page)}`
  ).toBe(true);
  await expect(inboxPane(page).getByRole("region", { name: "Question body" })).toHaveAttribute(
    "data-testid",
    "inbox-pane-scroller"
  );
  const followsHead = await scroller.evaluate((stop) => {
    const head = stop.closest('[data-testid="inbox-pane"]')?.querySelector(":scope > header");
    return (
      head != null && (head.compareDocumentPosition(stop) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
    );
  });
  expect(followsHead, "the focused pane scroller follows the pane head").toBe(true);
  await expect(selectedRow(page)).toHaveCount(1);
  const next = (await selectedRow(page).getAttribute("data-inbox-row")) ?? "";
  expect(next, "the row the Inbox selected next").not.toBe(left);
  await expect(scroller).toContainText(shows[next] ?? `the words of ${next}`);
  await expect(scroller).not.toContainText(shows[left] ?? `the words of ${left}`);
  expect(await page.evaluate(() => window.scrollY), "the page's scroll position").toBe(
    scrollBefore
  );
}

// SLOW(>=2s): two Sends whose responses the test holds two seconds each, both reached by the
// keyboard alone through j, k, l and Tab walks.
test("the focus after an own Send goes to the column's own stop", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Canary rollout" });
  const firstQuestion = "Ship the canary today?";
  const secondQuestion = "Widen the canary tomorrow?";
  const first = await seedAskWithReaderThread(issue.key, firstQuestion);
  const second = await seedAskWithReaderThread(issue.key, secondQuestion);
  // The comment thread whose reply box sends, and one more a live update takes away while that
  // Send is held.
  const pager = "Who holds the pager?";
  const runbook = "Is the runbook current?";
  const thread = await seedCommentThread(issue.key, pager, "I do, until six.");
  const other = await seedCommentThread(issue.key, runbook, "It was checked last week.");
  const shows: Record<string, string> = {
    [first]: firstQuestion,
    [second]: secondQuestion,
    [replyRowId(thread)]: pager,
    [replyRowId(other)]: runbook,
  };

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice, "/", FOCUS_WIDTH);
    for (const id of Object.keys(shows)) {
      await expect(row(page, id)).toBeVisible();
    }
    await expect(inboxList(page).locator("[data-inbox-row]")).toHaveCount(4);
    // The pointer leaves the list, so the list does not hold a row under it where the reader last
    // saw it, and the keymap, bound once sign-in resolves, takes the keys from the body.
    await page.mouse.move(0, 0);
    await page.locator("body").focus();
    const pane = inboxPane(page);

    // The answer's response is held two seconds.
    const answerHeld = Promise.withResolvers<void>();
    await page.route(`**/api/v1/asks/${first}/answer`, async (route) => {
      await answerHeld.promise;
      await route.continue();
    });

    // The keyboard alone: `j` and `k` select the first ask, `l` steps into its pane, Tab reaches
    // Ship, Space picks it, Tab reaches Answer, and Enter presses it.
    await selectWithKeys(page, first);
    await page.keyboard.press("l");
    await expect(paneScroller(page)).toBeFocused();
    const ship = pane.getByRole("radio", { name: "Ship" });
    await tabTo(page, ship, "the Ship option", 40);
    await page.keyboard.press("Space");
    await expect(ship).toBeChecked();
    // Answer reads "Answering…" while its write is out, so the button is matched by both labels.
    const send = pane.getByRole("button", { name: /^Answer(ing…)?$/ });
    await tabTo(page, send, "the answer's Send", 40);
    const scrollBefore = await page.evaluate(() => window.scrollY);
    await page.keyboard.press("Enter");
    await expectHeldWithTheFocus(page, send, "the answer's Send");
    // The answer's response is held for these two seconds: Send must stay held, with the focus on
    // it, for the whole time its write is out, and two seconds is longer than any redraw or
    // focus hand-off the page makes on its own.
    await page.waitForTimeout(2_000);
    await expectHeldWithTheFocus(page, send, "the answer's Send");
    answerHeld.resolve();

    // The answer lands and its row leaves.
    await expect(row(page, first)).toHaveCount(0);
    await expectTheColumnStop(page, shows, first, scrollBefore);

    // The same with a reply box's Send, in the comment thread, held two seconds; while it is
    // held a live update takes another row away - the reader's own reply to the other thread,
    // written elsewhere - and the focus does not move.
    const replyHeld = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${issue.key}/comments`, async (route) => {
      if (route.request().method() === "POST") await replyHeld.promise;
      await route.continue();
    });
    await selectWithKeys(page, replyRowId(thread));
    await page.keyboard.press("l");
    await expect(paneScroller(page)).toBeFocused();
    const box = pane.getByRole("textbox", { name: "Reply" });
    await tabTo(page, box, "the reply box", 40);
    await page.keyboard.type("Then I take it from six.");
    // The reply box's own Send, which reads "Sending…" while its write is out.
    const replySend = pane.getByRole("button", { name: /^(Send|Sending…)$/ });
    await tabTo(page, replySend, "the reply box's Send", 40);
    const replyScrollBefore = await page.evaluate(() => window.scrollY);
    await page.keyboard.press("Enter");
    await expectHeldWithTheFocus(page, replySend, "the reply box's Send");
    await createComment(issue.key, { body: "It is, as of today.", reply_to: other });
    await expect(row(page, replyRowId(other))).toHaveCount(0);
    await frameAndTask(page);
    await expectHeldWithTheFocus(page, replySend, "the reply box's Send");
    // The reply's response is held for these two seconds: Send must stay held, with the focus on
    // it, for the whole time its write is out, and two seconds is longer than any redraw or
    // focus hand-off the page makes on its own.
    await page.waitForTimeout(2_000);
    await expectHeldWithTheFocus(page, replySend, "the reply box's Send");
    replyHeld.resolve();

    await expect(row(page, replyRowId(thread))).toHaveCount(0);
    await expectTheColumnStop(page, shows, replyRowId(thread), replyScrollBefore);
  } finally {
    await alice.close();
  }
});
