import type { MeInboxResponse } from "../web/src/api/types";
import { setLiveSessions } from "./agents";
import { createIssue, createProject, getMeInbox } from "./api";
import { countDismissedThreads, isDismissedPage, seedDismissedThreads } from "./dismissed-seed";
import { type Opening, openDismissed, readWindow, row, WIDE } from "./dismissed-view";
import { chooseShow } from "./filters";
import { frameAndTask } from "./focus";
import { inboxList } from "./inbox-pane";
import { THREAD_SESSION } from "./inbox-threads";
import { median } from "./shard-split";
import { type BrowserContext, expect, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * The Dismissed view over a person's whole history of dismissals (R7), timed. The view reads the
 * server's pages of 200, newest dismissal first, and draws a window of the rows it has loaded:
 * the rows the list column shows and twelve above and below them (`DismissedWindow`,
 * web/src/features/inbox/Inbox.tsx). Its first row comes no later with 5,000 dismissed threads
 * than with 250, it reads the next page as the window nears the end of the loaded rows, and the
 * keys reach every loaded row, drawn or not.
 *
 * The row reads the wall clock, so the whole file runs in the `chromium-alone` project, with the
 * machine to itself (`clockSpecNames` in `e2e/configurations.ts` and `aloneProjects` in
 * `e2e/playwright.config.ts`). That project is out of the default selection, so a command names
 * it: `bun run e2e -- --project=chromium-alone e2e/inbox-dismissed-timing.e2e.ts --workers=1`,
 * which names the one worker the two medians rest on. The row reads a clock of its own and never
 * meets `refuseASharedRun` (`e2e/main-thread-hold.ts`), so that command is what holds it to one
 * worker, as it holds the rows of `agent-workspace-timing.e2e.ts`. In a shard of four workers the
 * ratio of its two medians swung from 1.29 to 2.79 on one head (run 45267651005 at `daf02ac4`).
 * The window's focus rules read no clock, and run in `chromium` as any other row does, in
 * `inbox-dismissed-window.e2e.ts`.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** Rows the window draws past each edge of what the list column shows (`WINDOW_OVERSCAN`). */
const OVERSCAN = 12;

/** How many times each pile is opened and timed. */
const READINGS = 11;

/** The first row's median time with 5,000 dismissed threads is at most this many times the
 *  median with 250. */
const SLOWER_AT_MOST = 1.25;

/** Notes, from the start of every document the page loads, when the first Inbox row is drawn:
 *  `performance.now()`, which counts from the start of the navigation, on the root element. */
async function noteFirstRow(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const watch = new MutationObserver(() => {
      if (document.querySelector("[data-inbox-row]") === null) return;
      document.documentElement.dataset.firstInboxRowAt = String(performance.now());
      watch.disconnect();
    });
    watch.observe(document, { childList: true, subtree: true });
  });
}

/** A page of `context` at the measured size that notes each document's first row. It loads the
 *  Dismissed view once and discards that load, so the build's code is in the page's cache before
 *  the first timing. */
async function timedPage(context: BrowserContext): Promise<Page> {
  const page = await context.newPage();
  await page.setViewportSize(WIDE);
  await noteFirstRow(page);
  await openDismissed(page);
  return page;
}

/** Fails, naming `login`, unless that person has nothing in the Inbox: the one read's `inbox`
 *  state answers `total` 0. */
async function expectEmptyInbox(login: string): Promise<void> {
  const { total } = await getMeInbox({ state: "inbox" }, { login });
  if (total !== 0) throw new Error(`${login} has ${total} entries in the Inbox before the timing`);
}

// R7. A person's whole history of dismissals: 5,000 threads, of which the view reads the newest
// 200 and draws the ones the list column shows and twelve either side. Its first row is timed
// against a second person's 250 dismissals, on the same build in the same run. Neither person has
// a thread in the Inbox, so the two loads differ in the Dismissed pile alone: a pile taken down
// would stand in its person's Inbox, and the page's Inbox read would be timed as well. Every
// timing is read before the first hard assertion, so a build without the window prints its
// medians and sizes before it fails.
// SLOW(>=2s): 5,250 threads seeded and dismissed, 22 timed page loads, three more pages read
// and thirty steps of the selection walked.
test("the Dismissed view draws a window of its rows over thousands dismissed", async ({
  browser,
}, testInfo) => {
  test.setTimeout(300_000);
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const pile = await createIssue({ project: "CORE", title: "Mirror retries" });
  const short = await createIssue({ project: "CORE", title: "Lease handover" });
  // Each thread's other writer is a session (`seedBulkReplyThreads`), so neither person is tied to
  // the other's threads, and each has dismissed every thread of theirs.
  await seedDismissedThreads({ count: 5_000, issueKey: pile.key, prefix: "Mirror" });
  await seedDismissedThreads({ count: 250, issueKey: short.key, login: "bob", prefix: "Lease" });
  expect.soft(await countDismissedThreads("alice"), "alice's dismissed threads").toBe(5_000);
  expect.soft(await countDismissedThreads("bob"), "bob's dismissed threads").toBe(250);
  await expectEmptyInbox("alice");
  await expectEmptyInbox("bob");

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const page = await timedPage(alice);
    const bobPage = await timedPage(bob);
    // After both warmups, alternate 5,000 then 250, one load at a time, for eleven readings each.
    const many: Opening[] = [];
    const few: Opening[] = [];
    for (let reading = 0; reading < READINGS; reading += 1) {
      many.push(await openDismissed(page));
      few.push(await openDismissed(bobPage));
    }
    await bobPage.close();

    const manyMedian = median(many.map(({ ms }) => ms));
    const fewMedian = median(few.map(({ ms }) => ms));
    const report = [
      `5,000 dismissed: median ${manyMedian.toFixed(1)} ms to the first row over ${many
        .map(({ ms }) => ms.toFixed(1))
        .join(", ")}; first response ${many.map(({ bytes }) => bytes).join(", ")} bytes`,
      `250 dismissed: median ${fewMedian.toFixed(1)} ms to the first row over ${few
        .map(({ ms }) => ms.toFixed(1))
        .join(", ")}; first response ${few.map(({ bytes }) => bytes).join(", ")} bytes`,
    ].join("\n");
    console.log(report);
    await testInfo.attach("dismissed-window-timings", { body: report, contentType: "text/plain" });

    // Only now does it assert.
    expect(
      manyMedian,
      `the first row's median with 5,000 dismissed (${manyMedian.toFixed(1)} ms) is at most ${SLOWER_AT_MOST} times the median with 250 (${fewMedian.toFixed(1)} ms)`
    ).toBeLessThanOrEqual(SLOWER_AT_MOST * fewMedian);

    // The first person again, for the rest.
    const nextPages: string[] = [];
    page.on("request", (request) => {
      const url = new URL(request.url());
      const cursor = url.searchParams.get("cursor");
      if (
        url.pathname === "/api/v1/me/inbox" &&
        url.searchParams.get("state") === "dismissed" &&
        cursor !== null
      ) {
        nextPages.push(cursor);
      }
    });
    const opened = await openDismissed(page);
    expect(opened.ids.length).toBeGreaterThan(0);
    expect(opened.ids.length).toBeLessThanOrEqual(200);
    expect(opened.cursor).toBeDefined();

    // The window: the rows the list column shows, and at most twelve either side of them, out of
    // the 200 loaded.
    const box = inboxList(page);
    // Two frames, by when the window has measured the rows it first drew and drawn again.
    await frameAndTask(page);
    await frameAndTask(page);
    const first = await readWindow(box);
    expect(
      first.drawn.length,
      `the list draws ${first.drawn.length} rows, and shows ${first.shown}`
    ).toBeLessThanOrEqual(first.shown + 2 * OVERSCAN);
    expect(first.drawn.length).toBeLessThan(opened.ids.length);
    expect(first.drawn).toEqual(opened.ids.slice(0, first.drawn.length));

    // The end of the loaded rows: one read of the next page, from the first page's cursor, and
    // 400 rows loaded.
    const second = page.waitForResponse((response) => isDismissedPage(response, opened.cursor));
    await box.evaluate((scroller) => {
      scroller.scrollTop = scroller.scrollHeight;
    });
    const secondAnswer = (await (await second).json()) as MeInboxResponse;
    await expect.poll(async () => (await readWindow(box)).loaded).toBe(400);
    expect(nextPages).toEqual([opened.cursor]);

    // **Show more** still reads the next page from the keyboard. The focus on it brings the end
    // of the loaded rows into view, which reads the next page as any scroll there does; Enter
    // then reads one page more, from the cursor the last page answered.
    const more = inboxList(page).locator('[data-inbox-show-more="dismissed"]');
    const third = page.waitForResponse((response) =>
      isDismissedPage(response, secondAnswer.next_cursor ?? undefined)
    );
    await more.focus();
    const thirdAnswer = (await (await third).json()) as MeInboxResponse;
    await expect.poll(async () => (await readWindow(box)).loaded).toBe(600);
    await expect(more).toHaveText("Show 200 more");
    const fourth = page.waitForResponse((response) =>
      isDismissedPage(response, thirdAnswer.next_cursor ?? undefined)
    );
    await page.keyboard.press("Enter");
    await fourth;
    await expect.poll(async () => (await readWindow(box)).loaded).toBe(800);
    expect(nextPages).toEqual([opened.cursor, secondAnswer.next_cursor, thirdAnswer.next_cursor]);

    // `j` from the first row to the 30th: the row in hand is drawn and in view at each step.
    await box.evaluate((scroller) => {
      scroller.scrollTop = 0;
    });
    const firstId = opened.ids[0] ?? "";
    await row(page, firstId).locator("time").first().click();
    await expect(row(page, firstId)).toHaveAttribute("data-inbox-selected", "true");
    for (let at = 1; at < 30; at += 1) {
      await page.keyboard.press("j");
      const id = opened.ids[at] ?? "";
      await expect(row(page, id)).toHaveAttribute("data-inbox-selected", "true");
      await expect
        .poll(
          () =>
            row(page, id).evaluate((node) => {
              const list = node.closest('[data-testid="inbox-list"]');
              if (list === null) return false;
              const sight = list.getBoundingClientRect();
              const drawn = node.getBoundingClientRect();
              return drawn.top >= sight.top && drawn.bottom <= sight.bottom;
            }),
          { message: `row ${at + 1}, the row in hand, is in view` }
        )
        .toBe(true);
    }

    // Move to Inbox on the row in hand: it leaves the view and stands in the Inbox.
    const back = opened.ids[29] ?? "";
    await row(page, back).locator(`[data-inbox-dismiss="${back}"]`).click();
    await expect(row(page, back)).toHaveCount(0);
    await chooseShow(page, "Inbox");
    await expect(row(page, back)).toBeVisible();
  } finally {
    await bob.close();
    await alice.close();
  }
});
