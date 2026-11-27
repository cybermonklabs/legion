import { setLiveSessions } from "./agents";
import { createIssue, createProject, dismissInbox } from "./api";
import { countDismissedThreads, isDismissedPage, seedDismissedThreads } from "./dismissed-seed";
import { openDismissed, readWindow, row, WIDE } from "./dismissed-view";
import { chooseShow } from "./filters";
import { describeFocus, frameAndTask, holdsFocus, tabTo } from "./focus";
import { drawer } from "./inbox-drawer";
import { inboxList } from "./inbox-pane";
import { THREAD_SESSION } from "./inbox-threads";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * The Dismissed view over a person's whole history of dismissals (R7), under the focus rules of
 * `web/src/components/focus-place.ts`. The view reads the server's pages of 200, newest dismissal
 * first, and draws a window of the rows it has loaded: the rows the list column shows and twelve
 * above and below them (`DismissedWindow`, web/src/features/inbox/Inbox.tsx). A focused row the
 * person scrolls out of that window is no loss of focus and is drawn again once scrolled back,
 * and a row another tab restores stays drawn while it is the row under the hand.
 *
 * The view's timed row, which reads the wall clock, is in `e2e/inbox-dismissed-timing.e2e.ts`,
 * which the `chromium-alone` project runs one worker at a time (`clockSpecNames`,
 * `e2e/configurations.ts`).
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** A row's own checkbox. */
function mark(within: Page | Locator, id: string): Locator {
  return row(within, id).getByRole("checkbox");
}

/** The row the focus is in, or null for none. */
function focusedRow(page: Page): Promise<string | null> {
  return page.evaluate(
    () =>
      document.activeElement?.closest("[data-inbox-row]")?.getAttribute("data-inbox-row") ?? null
  );
}

/** Scrolls `box` down, as far as a person's scroll goes at a time, until row `id` is no longer
 *  in the page, and answers where the box was left. */
async function scrollUntilGone(page: Page, within: Page | Locator, box: Locator, id: string) {
  // Each read after the first follows a scroll and `frameAndTask`. A read taken before the list
  // redraws only costs one more step, and the line after the loop waits for the row to go.
  for (let step = 0; step < 40 && (await row(within, id).count()) > 0; step += 1) {
    await box.evaluate((scroller) => {
      scroller.scrollTop += 400;
    });
    await frameAndTask(page);
  }
  await expect(row(within, id)).toHaveCount(0);
  return box.evaluate((scroller) => scroller.scrollTop);
}

// R7 under the focus rules of web/src/components/focus-place.ts. A focused row the person scrolls
// out of the window is no loss: nothing moves the focus or scrolls, and in the drawer the drawer
// takes the focus. A row another tab restores stays drawn while it is the row under the hand,
// and goes once it is not.
// SLOW(>=2s): 1,000 threads seeded and dismissed, scrolls on the page and in the drawer waited
// out, and a live update from a second tab.
test("a focused row scrolled out of the window is no loss of focus", async ({ browser }) => {
  test.setTimeout(180_000);
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const pile = await createIssue({ project: "CORE", title: "Mirror retries" });
  await seedDismissedThreads({ count: 1_000, issueKey: pile.key, prefix: "Mirror" });
  expect(await countDismissedThreads()).toBe(1_000);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize(WIDE);
    const opened = await openDismissed(page);
    const box = inboxList(page);
    const fifth = opened.ids[4] ?? "";

    // The person tabs to the fifth row's checkbox, and scrolls the list until the window has
    // taken that row away.
    await page.locator("body").focus();
    await tabTo(page, mark(page, fifth), "the fifth row's checkbox", 120);
    const pageAt = await page.evaluate(() => window.scrollY);
    const listAt = await scrollUntilGone(page, page, box, fifth);
    await frameAndTask(page);
    expect(await box.evaluate((scroller) => scroller.scrollTop)).toBe(listAt);
    expect(await page.evaluate(() => window.scrollY)).toBe(pageAt);
    expect(await focusedRow(page), `the focus is on ${await describeFocus(page)}`).toBeNull();
    expect(await describeFocus(page)).toBe("the page's body");

    // Scrolled back, the row is drawn again.
    await box.evaluate((scroller) => {
      scroller.scrollTop = 0;
    });
    await expect(row(page, fifth)).toHaveCount(1);

    // The drawer, on the Dismissed view: the same scroll leaves the focus on the drawer itself.
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("i");
    const peek = drawer(page);
    await expect(peek).toBeVisible();
    await chooseShow(page, "Dismissed", peek);
    await expect(row(peek, fifth)).toHaveCount(1);
    await tabTo(page, mark(peek, fifth), "the fifth row's checkbox in the drawer", 120);
    const pageBefore = await page.evaluate(() => window.scrollY);
    const drawerAt = await scrollUntilGone(page, peek, peek, fifth);
    await frameAndTask(page);
    expect(await holdsFocus(peek), `the focus is on ${await describeFocus(page)}`).toBe(true);
    expect(await peek.evaluate((scroller) => scroller.scrollTop)).toBe(drawerAt);
    expect(await page.evaluate(() => window.scrollY)).toBe(pageBefore);
    await page.keyboard.press("Escape");
    await expect(peek).toHaveCount(0);

    // Last, a second tab restores the row whose checkbox holds the focus, with the pointer on no
    // row: the row stays drawn in its place, with the focus, and nothing scrolls.
    await openDismissed(page);
    await page.mouse.move(1, 1);
    const third = opened.ids[2] ?? "";
    const next = opened.ids[3] ?? "";
    await mark(page, third).focus();
    const before = (await readWindow(box)).drawn;
    const listBefore = await box.evaluate((scroller) => scroller.scrollTop);
    const reread = page.waitForResponse((response) => isDismissedPage(response));
    await dismissInbox({ restore: true, threads: [third] });
    await reread;
    await frameAndTask(page);
    expect((await readWindow(box)).drawn).toEqual(before);
    expect(
      await holdsFocus(mark(page, third)),
      `the focus is on ${await describeFocus(page)}`
    ).toBe(true);
    expect(await box.evaluate((scroller) => scroller.scrollTop)).toBe(listBefore);

    // At each of the row's own stops it stays drawn; once the focus is on the next row's
    // checkbox, the view draws it no more, and the other rows keep their order.
    await tabTo(page, mark(page, next), "the next row's checkbox", 120, async () => {
      // A Tab press moves the focus before it resolves, so one read here is the stop it reached.
      if ((await focusedRow(page)) === third) await expect(row(page, third)).toHaveCount(1);
    });
    await expect(row(page, third)).toHaveCount(0);
    const after = (await readWindow(box)).drawn;
    const others = before.filter((id) => id !== third);
    expect(after.slice(0, others.length)).toEqual(others.slice(0, after.length));
  } finally {
    await alice.close();
  }
});
