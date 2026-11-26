import { setLiveSessions } from "./agents";
import { createIssue, createProject, listMyReplies, markThreadRead } from "./api";
import { peekTrigger } from "./inbox-drawer";
import { inboxRow, selectedRow, selectRow } from "./inbox-pane";
import { replyRowId, seedCommentThread, THREAD_SESSION, unreadThreadRows } from "./inbox-threads";
import { hasReadingPane } from "./reading-pane";
import { expect, type Locator, type Page, type Route, test } from "./test";
import { asUser } from "./users";

/**
 * Reading a thread in the Inbox marks it read (INFRA-450, A7): "a thread is marked when its
 * newest message has been on screen, in the pane or on a card ... three quarters of the message
 * visible for one second, or, for a message taller than the screen, the message filling three
 * quarters of the screen for one second". The row stays where it is, drawn as read: a thread
 * leaves the Inbox only when the reader dismisses it (`e2e/inbox-dismiss.e2e.ts`).
 *
 * The reading pane is an inner scroller about 200 px shorter than the window, so a reply taller
 * than the pane can never cover three quarters of the *window*: at the suite's own viewport
 * (`Desktop Chrome`, 1280x720) the pane is 519 px tall and the rule asks for 540 px. A reader who
 * scrolls such a reply through the pane for as long as they like never clears its row, which is
 * the complaint this issue exists to answer.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** A reply far taller than the reading pane at any window height the suite runs at. */
const LONG_REPLY = Array.from(
  { length: 40 },
  (_, index) => `Line ${index + 1} of the replay answer.`
).join("\n\n");

// @pr: The read mark's own row: reading a reply taller than the pane marks it read and leaves its row in the list.
test("a reply taller than the reading pane is marked read by reading it, and its row stays @pr", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  const thread = await seedCommentThread(issue.key, "Why does it give up?", LONG_REPLY);
  // A second thread, so the selection has somewhere to move once the long one has been read.
  const other = await seedCommentThread(
    issue.key,
    "And how far back does the replay look?",
    "One hour, then it gives up."
  );
  expect(await unreadThreadRows()).toBe(2);
  // Whether the long thread itself is still unread. The other row is read as soon as the column
  // draws it, so the count of every unread thread would not say which of the two this is about.
  const longUnread = async (): Promise<boolean> =>
    ((await listMyReplies({ state: "unread" })).rows ?? []).some((row) => row.thread_id === thread);
  expect(await longUnread()).toBe(true);

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    const row = page.locator(`[data-inbox-row="${replyRowId(thread)}"]`);
    await expect(row).toBeVisible();
    test.skip(!hasReadingPane(page), "the reading pane is this row's subject");
    await row.locator("time").first().click();
    await expect(row).toHaveAttribute("data-inbox-selected", "true");
    const reply = page.locator("[data-inbox-thread-turn]").last();
    await expect(reply).toBeVisible();

    // The reader scrolls the reply through the pane, a part of a screen at a time, pausing on
    // each - the dwell the read mark asks for, given ten times over. The poll is both halves of
    // that: each attempt moves the wheel once, and the 1,200 ms between attempts is the pause
    // the mark asks a reader for, so the row ends on the mark rather than on a count of turns.
    const pane = await reply.boundingBox();
    expect(pane).not.toBeNull();
    await expect
      .poll(
        async () => {
          await page.mouse.move((pane?.x ?? 0) + 10, 400);
          await page.mouse.wheel(0, 300);
          return await longUnread();
        },
        { intervals: [1200], timeout: 12_000 }
      )
      .toBe(false);
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);

    // The row stays where it stood, drawn as read: only a dismiss takes a thread out of the
    // Inbox. The selection moves to the other row, and a reload reads both rows back.
    await selectRow(page, replyRowId(other));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", replyRowId(other));
    await expect(row).toBeVisible();
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);
    await page.reload();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(2);
    await expect(row).toBeVisible();
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

/** What a row holds of the reads the page makes of one route: `hold` keeps every later GET the
 *  route names unanswered, and `out` counts how many are kept. A kept GET is never answered, so
 *  the page gives it up at its own 30 s deadline. */
interface BoundReads {
  readonly hold: () => void;
  readonly out: () => number;
}

/** Routes the page's GETs that `matches` names, which go through until `hold` is called. */
async function routeBoundReads(page: Page, matches: (url: URL) => boolean): Promise<BoundReads> {
  let holding = false;
  let kept = 0;
  await page.route(matches, async (route: Route) => {
    if (!holding || route.request().method() !== "GET") return route.fallback();
    kept += 1;
    return undefined;
  });
  return {
    hold: () => {
      holding = true;
    },
    out: () => kept,
  };
}

// The tester's red row of round 21 (read-bound.ts:40-47: a read that has not settled is said "for
// the latest read" alone, and "forgets at once why any earlier read had not settled"; the
// architect's ruling of 2026-11-26T04:19Z). Every thread card of the Inbox owns a `useReadBound`
// (ThreadRowCard.tsx:53), though every Mark read reads the Inbox's list again (:78), so a Mark
// read on the second card begins a new read of the list and takes nothing away from the line the
// first Mark read left for the read before it. At c3b536cc the first card still says "Could not
// load your inbox. Dispatch did not answer within 30 seconds." for the whole 15 s the row waits
// after the second mark landed. Measured in Chromium and Firefox, twice each. Those lines are
// c3b536cc's: since fec0003c the list owns the one bound (`useListReadBound`, Inbox.tsx:1279) and
// hands every card its hold (ThreadRowCard.tsx:53-66), and a Mark read's read of the list is at
// ThreadRowCard.tsx:91. Since cybermonklabs/legion#5223 the Inbox page lists short rows at every
// width and only the Peek drawer draws these cards, the same `ThreadRowCard` (Inbox.tsx:787), so
// the row presses Mark read in the drawer over a project page.
// SLOW(>=2s): two threads seeded and marked read, the Peek drawer open over a project page, two
// marks written and the page's clock moved once past the 30 s bound.
// @firefox: the timers of a read's own deadline and of the bound a held control waits on, which
// each engine runs.
test("a Mark read on a second thread card takes away the line the first Mark read left for the Inbox read that had not settled @firefox", async ({
  page,
}) => {
  const step = (text: string) => `Mark read on two thread cards of the Inbox: ${text}`;
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  const firstThread = await seedCommentThread(
    issue.key,
    "How far back does a replay go?",
    "One hour, then it gives up."
  );
  const secondThread = await seedCommentThread(
    issue.key,
    "Who owns the cutover?",
    "The platform team does."
  );
  // Both threads are read before the page opens, so no card sends a mark of its own as it is
  // watched, and every mark of the row is a press of Mark read.
  await markThreadRead(firstThread, new Date().toISOString());
  await markThreadRead(secondThread, new Date().toISOString());
  expect(await unreadThreadRows(), step("both seeded threads are read.")).toBe(0);
  const listReads = await routeBoundReads(page, (url) => url.pathname === "/api/v1/me/replies");
  await page.setViewportSize({ height: 900, width: 1024 });
  await page.clock.install();
  await page.goto("/projects/CORE");
  await peekTrigger(page).click();
  const markOf = (thread: string) =>
    inboxRow(page, replyRowId(thread)).locator('[data-inbox-action="mark-read"]');
  await expect(markOf(firstThread), step("the first thread's card draws Mark read.")).toHaveText(
    "Mark read"
  );
  await expect(markOf(secondThread), step("the second thread's card draws Mark read.")).toHaveText(
    "Mark read"
  );
  const unreadLine = (thread: string): Locator =>
    inboxRow(page, replyRowId(thread))
      .getByRole("alert")
      .filter({ hasText: /did not answer within 30 seconds/ });
  const marks = (thread: string) =>
    page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/threads/${thread}/read`
    );

  listReads.hold();
  const firstAnswer = marks(firstThread);
  await markOf(firstThread).click();
  expect((await firstAnswer).ok(), step("the first mark lands at Dispatch.")).toBe(true);
  await expect
    .poll(listReads.out, { message: step("the list read the first mark starts is out.") })
    .toBeGreaterThan(0);
  await page.clock.fastForward(30_000);
  await expect(
    unreadLine(firstThread),
    step("at the 30 s bound the first card says why the Inbox is not read.")
  ).toHaveText(["Could not load your inbox. Dispatch did not answer within 30 seconds."]);

  const secondAnswer = marks(secondThread);
  await markOf(secondThread).click();
  expect((await secondAnswer).ok(), step("the second mark lands at Dispatch.")).toBe(true);
  await expect(
    unreadLine(firstThread),
    step(
      "the first card loses its line as the second mark lands and a new read of the Inbox begins."
    )
  ).toHaveCount(0);
});
