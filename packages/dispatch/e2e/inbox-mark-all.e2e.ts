import { GROUP_PAGE } from "../web/src/features/inbox/InboxGroups";

import { setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, meInboxThreads } from "./api";
import { controlRow } from "./filters";
import { typeFilterCounts } from "./inbox-filter";
import {
  asSession,
  compactInboxLink,
  expectInboxCount,
  expectSidebarInboxCount,
  replyRowId,
  seedAcceptedSend,
  seedAgentThread,
  seedBulkAgentThreads,
  seedBulkReplyThreads,
  seedCommentThread,
  THREAD_SESSION,
  unreadThreadRows,
} from "./inbox-threads";
import { holdPosts, refusePosts } from "./sends";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * The Inbox's **Mark all read** (INFRA-450): one click over a list of six hundred and twenty
 * unread threads, what it takes off the counts before the server answers, what it leaves in the
 * list, and what comes back when it refuses.
 *
 * A thread stays in the Inbox, read or not, until the reader dismisses it or answers or replies
 * in it, so one click changes how every row is drawn and takes none of them away
 * (`e2e/inbox-dismiss.e2e.ts` is the Dismiss that does).
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** How many Reply threads and how many Agent threads one click has to carry. */
const REPLY_THREADS = 600;
const AGENT_THREADS = 20;
/** Two open asks waiting on the reader, so the Inbox count has something left after the click. */
const ASKS = 2;

/** The route one press of **Mark all read** posts to. */
const MARK_ALL = "**/api/v1/me/inbox/read";

interface Seeded {
  readonly issueKey: string;
}

async function seedMany(): Promise<Seeded> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["Ledger rollout", "Mirror retries"]) {
    const issue = await createIssue({ project: "CORE", title });
    await createAsk(issue.key, { question: `${title}: ship tonight?` }, asSession());
  }
  const threads = await createIssue({ project: "CORE", title: "Replay budget" });
  await seedBulkReplyThreads({ count: REPLY_THREADS, issueKey: threads.key, prefix: "Replay" });
  await seedBulkAgentThreads({ count: AGENT_THREADS, prefix: "Rollout" });
  // A Send the session took as a turn of its live conversation: no Inbox row, so one press
  // never marks its reply, which the Agents page goes on counting.
  await seedAcceptedSend({
    body: "Steer: take this as your own turn.",
    reply: "Took it, and here is the answer.",
  });
  expect(await unreadThreadRows()).toBe(REPLY_THREADS);
  expect((await meInboxThreads({ limit: 1 })).total).toBe(REPLY_THREADS + AGENT_THREADS);
  return { issueKey: threads.key };
}

/** The bar's **More** inside `within` - the page, or the drawer - where the Inbox keeps **Mark
 *  all read** at every width. */
function moreOf(within: Locator | Page): Locator {
  return controlRow(within).getByRole("button", { exact: true, name: "More" });
}

/** **Mark all read**, an item of the open **More** menu. */
function markAllItem(page: Page): Locator {
  return page
    .getByRole("menu", { name: "More" })
    .getByRole("menuitem", { exact: true, name: "Mark all read" });
}

/** Presses **Mark all read** as a reader does: from the bar's **More**. */
async function pressMarkAll(page: Page, within: Locator | Page = page): Promise<void> {
  await moreOf(within).click();
  await markAllItem(page).click();
}

// @pr: A write the server settles: one Mark all read over 620 threads, with every row kept and the counts down.
test("one click marks six hundred and twenty threads read, leaves every row in the list, and the counts stay down @pr", async ({
  browser,
}) => {
  const seeded = await seedMany();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows.first()).toBeVisible();
    const unreadMarks = page.locator("[data-inbox-row] [aria-label='Unread']");
    await expectSidebarInboxCount(page, ASKS + REPLY_THREADS + AGENT_THREADS);
    // Six hundred and twenty-two rows stand in the one band this seed makes - no issue here
    // carries a priority - so the band draws its first page of them, the two open asks among
    // them, and every thread row it draws is unread.
    const threadRows = page.locator('[data-inbox-row^="reply:"], [data-inbox-row^="agent:"]');
    await expect(rows).toHaveCount(GROUP_PAGE);
    await expect(threadRows).toHaveCount(GROUP_PAGE - ASKS);
    await expect(unreadMarks).toHaveCount(GROUP_PAGE - ASKS);

    // One click, with the request held open: every row is drawn as read where it stands, the
    // Type filter still counts all of them, and the reader's Inbox number is already at zero.
    const held = await holdPosts(page, MARK_ALL);
    await pressMarkAll(page);
    // **More** held **Mark all read** alone in the page's wide bar, so it goes with it.
    await expect(moreOf(page)).toHaveCount(0);
    await expect(unreadMarks).toHaveCount(0);
    await expect(rows).toHaveCount(GROUP_PAGE);
    expect(await typeFilterCounts(page, ["Threads", "Agent message"])).toEqual([
      REPLY_THREADS,
      AGENT_THREADS,
    ]);
    await expectSidebarInboxCount(page, ASKS);

    // The server answers; a reload reads the same list and the same numbers back from it, every
    // marked thread still in the Inbox and drawn as read.
    held.release();
    await held.settled();
    await expect.poll(async () => unreadThreadRows()).toBe(0);
    await page.reload();
    await expect(rows).toHaveCount(GROUP_PAGE);
    await expect(unreadMarks).toHaveCount(0);
    await expectSidebarInboxCount(page, ASKS);
    expect(await typeFilterCounts(page, ["Threads", "Agent message"])).toEqual([
      REPLY_THREADS,
      AGENT_THREADS,
    ]);

    // The compact header carries the same number, as a link to the Inbox from another page.
    await page.setViewportSize({ height: 900, width: 1024 });
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expect(compactInboxLink(page)).toHaveText(`Inbox ${ASKS}`);
    await page.setViewportSize({ height: 720, width: 1280 });

    // The Agents page counts the accepted Send's reply alone: the twenty Agent rows' replies
    // are read, and that one was never an Inbox row to mark.
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expect(page.getByRole("link", { name: "New reply 1" }).first()).toBeVisible();

    // A reply written after the list loaded comes in unread, and a second click marks it.
    await page.goto("/");
    await expect(moreOf(page)).toHaveCount(0);
    const late = await seedCommentThread(
      seeded.issueKey,
      "One more question about the replay.",
      "One more answer about the replay."
    );
    await seedAgentThread({ body: "And the rollout?", replies: ["Still green."] });
    await expect(page.locator(`[data-inbox-row="${replyRowId(late)}"]`)).toBeVisible();
    await expect(unreadMarks).toHaveCount(2);
    await pressMarkAll(page);
    await expect(unreadMarks).toHaveCount(0);
    await expect.poll(async () => unreadThreadRows()).toBe(0);
  } finally {
    await alice.close();
  }
});

test("the drawer's Mark all read clears the same counts and keeps its rows @phone", async ({
  browser,
}) => {
  await seedMany();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expectInboxCount(page, ASKS + REPLY_THREADS + AGENT_THREADS);
    await page.locator("body").focus();
    await page.keyboard.press("i");
    const drawer = page.getByTestId("inbox-drawer");
    await expect(drawer).toBeVisible();
    await expect(drawer.locator("[data-inbox-row]").first()).toBeVisible();
    await expect(drawer.locator("[data-inbox-row]")).toHaveCount(GROUP_PAGE);

    const held = await holdPosts(page, MARK_ALL);
    await pressMarkAll(page, drawer);
    // The drawer's bar is narrow, so its **More** stays for **View**, and holds no **Mark all
    // read** now.
    await moreOf(drawer).click();
    await expect(page.getByRole("menu", { name: "More" })).toBeVisible();
    await expect(markAllItem(page)).toHaveCount(0);
    await moreOf(drawer).click();
    await expect(drawer.locator("[data-inbox-row] [aria-label='Unread']")).toHaveCount(0);
    expect(await typeFilterCounts(page, ["Threads"], drawer)).toEqual([REPLY_THREADS]);
    await expectInboxCount(page, ASKS);
    await expect(drawer.locator("[data-inbox-row]")).toHaveCount(GROUP_PAGE);

    held.release();
    await held.settled();
    await expect.poll(async () => unreadThreadRows()).toBe(0);
    await expectInboxCount(page, ASKS);
  } finally {
    await alice.close();
  }
});

test("a refused Mark all read brings the marks and the counts back and says nothing was marked", async ({
  browser,
}) => {
  await seedMany();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows.first()).toBeVisible();
    // The asks and the threads are two reads, and `count()` waits for nothing: the threads'
    // own read has to be on the page before the rows are counted, or `before` is the asks
    // alone. Both thread kinds come with that read, so both are waited for.
    await expect(page.locator('[data-inbox-row^="reply:"]').first()).toBeVisible();
    await expect(page.locator('[data-inbox-row^="agent:"]').first()).toBeVisible();
    await expectSidebarInboxCount(page, ASKS + REPLY_THREADS + AGENT_THREADS);
    const before = await rows.count();
    expect(before).toBeGreaterThan(ASKS);

    const unreadBefore = await page.locator("[data-inbox-row] [aria-label='Unread']").count();
    expect(unreadBefore).toBeGreaterThan(0);

    const refuse = await refusePosts(page, MARK_ALL);
    const unreadMarks = page.locator("[data-inbox-row] [aria-label='Unread']");
    await pressMarkAll(page);
    // The press takes the marks and the Inbox number, never a row: a read thread stays in the
    // Inbox until it is dismissed.
    await expect(unreadMarks).toHaveCount(0);
    await expect(rows).toHaveCount(before);
    await expectSidebarInboxCount(page, ASKS);
    refuse();

    const alert = page.locator("[data-inbox-mark-all-refused]");
    await expect(alert).toHaveRole("alert");
    await expect(alert).toHaveText("Nothing was marked read.");
    await expect(unreadMarks).toHaveCount(unreadBefore);
    await expect(rows).toHaveCount(before);
    await expectSidebarInboxCount(page, ASKS + REPLY_THREADS + AGENT_THREADS);
    expect(await unreadThreadRows()).toBe(REPLY_THREADS);
  } finally {
    await alice.close();
  }
});
