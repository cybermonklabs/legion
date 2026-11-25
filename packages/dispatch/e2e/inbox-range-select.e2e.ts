import type { MeInboxResponse } from "../web/src/api/types";
import { setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, patchIssue } from "./api";
import { isDismissedPage, pageIds, seedDismissedThreads } from "./dismissed-seed";
import { chooseShow, listTrigger, openFilterPanel, pickFromList } from "./filters";
import { describeFocus, frameAndTask, holdsFocus, tabTo } from "./focus";
import { drawer } from "./inbox-drawer";
import { inboxList, READING_PANE_WIDTH, selectedRow } from "./inbox-pane";
import {
  asSession,
  replyRowId,
  seedBulkReplyThreads,
  seedCommentThread,
  THREAD_SESSION,
} from "./inbox-threads";
import { drawnIds } from "./inbox-view";
import { type BrowserContext, expect, type Locator, type Page, type Request, test } from "./test";
import { asUser } from "./users";

/**
 * A shift-click on a row's checkbox selects a range: the clicked row, the anchor and every row
 * the list draws between them (R4). The anchor is the row marked or unmarked last without the
 * shift key. `Shift+X`, `Shift+J` and `Shift+K` make the same range from the keyboard. One press
 * of the bar acts on the whole range, a screen reader hears its count, and the bar's focus
 * return still reads the row the person marked last.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** The route one press of Dismiss or of Move to Inbox posts to. */
const DISMISS_PATH = "/api/v1/me/inbox/dismiss";

/** The route one ask's snooze is written to. */
const SNOOZE_PATH = /^\/api\/v1\/me\/asks\/([^/]+)\/snooze$/;

/** The Inbox, 900 pixels tall, by default at a width that draws the reading column. */
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

/** Every row the list draws. */
function rows(page: Page): Locator {
  return inboxList(page).locator("[data-inbox-row]");
}

/** One row, by the id its `data-inbox-row` names. */
function row(page: Page, id: string): Locator {
  return inboxList(page).locator(`[data-inbox-row="${id}"]`);
}

/** A row's own checkbox. */
function mark(page: Page, id: string): Locator {
  return row(page, id).getByRole("checkbox");
}

/** The ids of the drawn rows whose checkbox is ticked, in the order the list draws them. */
function markedIds(page: Page): Promise<string[]> {
  return rows(page).evaluateAll((nodes) =>
    nodes
      .filter((node) => node.querySelector<HTMLInputElement>('input[type="checkbox"]')?.checked)
      .map((node) => node.getAttribute("data-inbox-row") ?? "")
  );
}

/** The selection bar. */
function bar(page: Page): Locator {
  return page.getByRole("toolbar", { name: "Selected rows" });
}

/** Reads the bar counting `count` rows, in its own words. */
async function expectCount(page: Page, count: number): Promise<void> {
  await expect(bar(page).getByText(`${count} selected`, { exact: true })).toBeVisible();
}

/** The number the bar counts, read off its own words, or null while it draws no bar. */
async function barCount(page: Page): Promise<number | null> {
  // Its one caller is an `expect.poll`, which reads again while the bar is not drawn yet.
  if ((await bar(page).count()) === 0) return null;
  // The same poll reads the bar's words again until they count the run it waits for.
  const counted = /(\d+) selected/.exec((await bar(page).textContent()) ?? "");
  return counted === null ? null : Number(counted[1]);
}

/** Every band heading the list draws, top to bottom, with its count. */
function bandHeadings(page: Page): Promise<string[]> {
  return inboxList(page)
    .locator("h2 button")
    .evaluateAll((nodes) => nodes.map((node) => (node.textContent ?? "").trim()));
}

/** Every `POST` to the dismiss route the page makes from here on. */
function watchDismisses(page: Page): Request[] {
  const posted: Request[] = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && new URL(request.url()).pathname === DISMISS_PATH) {
      posted.push(request);
    }
  });
  return posted;
}

/** The thread ids the next press of Dismiss or of Move to Inbox names, once it is sent. */
async function namedByPress(page: Page, press: () => Promise<void>): Promise<string[]> {
  const sent = page.waitForRequest(
    (request) => request.method() === "POST" && new URL(request.url()).pathname === DISMISS_PATH
  );
  await press();
  const request = await sent;
  // The answer is read before the next step, so the list has taken the press's write.
  await (await request.response())?.finished();
  const body: { threads?: string[] } = request.postDataJSON();
  return body.threads ?? [];
}

/** Selects a row as the pointer does: one click on its own time. */
async function selectRow(page: Page, id: string): Promise<void> {
  await row(page, id).locator("time").first().click();
  await expect(row(page, id)).toHaveAttribute("data-inbox-selected", "true");
}

/** The window's scroll position and the list column's, which scrolls in itself beside the
 *  reading column. */
function scrollPositions(page: Page): Promise<{ list: number; window: number }> {
  return inboxList(page).evaluate((list) => ({ list: list.scrollTop, window: window.scrollY }));
}

/** Whether row `id`'s checkbox lies wholly inside the list column's visible box. */
function markInSight(page: Page, id: string): Promise<boolean> {
  return mark(page, id).evaluate((box) => {
    const list = box.closest('[data-testid="inbox-list"]');
    if (list === null) return false;
    const seen = list.getBoundingClientRect();
    const drawn = box.getBoundingClientRect();
    return drawn.top >= seen.top && drawn.bottom <= seen.bottom;
  });
}

/** The id of the first drawn row whose checkbox, the box the bar's return judges rather than the
 *  row's, lies wholly inside the list column's visible box; undefined while no row's does. */
function firstMarkInSight(page: Page): Promise<string | undefined> {
  return inboxList(page).evaluate((list) => {
    const seen = list.getBoundingClientRect();
    return [...list.querySelectorAll<HTMLElement>("[data-inbox-row]")].find((node) => {
      const box = node.querySelector('input[type="checkbox"]')?.getBoundingClientRect();
      return box !== undefined && box.top >= seen.top && box.bottom <= seen.bottom;
    })?.dataset.inboxRow;
  });
}

/** How far a landing checkbox stands cut by the list column's edge before a press. */
const CUT_PX = 4;

/** The slack of every read here of an edge or of a scroll: a layout in fractions of a pixel
 *  scrolls by whole ones. */
const EDGE_SLACK_PX = 1;

/** Where a row's checkbox stands against the list column's visible box, the list's own box cut
 *  to the window's, in viewport pixels. */
interface MarkSight {
  /** The checkbox's bottom edge. */
  readonly bottom: number;
  /** Whether the checkbox's centre lies inside the visible box. */
  readonly centreInside: boolean;
  /** How far the visible box's top edge cuts into the checkbox; 0 where it does not. */
  readonly cutAbove: number;
  /** How far the visible box's bottom edge cuts into the checkbox; 0 where it does not. */
  readonly cutBelow: number;
  /** The visible box's bottom edge. */
  readonly seenBottom: number;
  /** The visible box's top edge. */
  readonly seenTop: number;
  /** The checkbox's top edge. */
  readonly top: number;
}

/** Where row `id`'s checkbox stands against the list column's visible box, or, with `up`, where
 *  it will stand once its row has moved up by that much: once the rows a press takes away above
 *  it have gone. */
async function markSight(page: Page, id: string, up = 0): Promise<MarkSight> {
  const { bottom, seenBottom, seenTop, top } = await mark(page, id).evaluate((box, by) => {
    const list = box.closest('[data-testid="inbox-list"]');
    if (list === null) throw new Error("the checkbox is not in the Inbox list");
    const seen = list.getBoundingClientRect();
    const drawn = box.getBoundingClientRect();
    return {
      bottom: drawn.bottom - by,
      seenBottom: Math.min(seen.bottom, window.innerHeight),
      seenTop: Math.max(seen.top, 0),
      top: drawn.top - by,
    };
  }, up);
  const centre = (top + bottom) / 2;
  return {
    bottom,
    centreInside: centre > seenTop && centre < seenBottom,
    cutAbove: Math.max(0, seenTop - top),
    cutBelow: Math.max(0, bottom - seenBottom),
    seenBottom,
    seenTop,
    top,
  };
}

/** Whether the checkbox `sight` reads stands whole in the list column's visible box. */
function wholeInSight(sight: MarkSight): boolean {
  return sight.cutAbove <= EDGE_SLACK_PX && sight.cutBelow <= EDGE_SLACK_PX;
}

/** What `sight` read, in words, for a failure. */
function sightWords(sight: MarkSight): string {
  return `the checkbox stands from ${sight.top} to ${sight.bottom} px in the list's visible box from ${sight.seenTop} to ${sight.seenBottom} px, cut by ${sight.cutAbove} px at the top and by ${sight.cutBelow} px at the bottom, its centre ${sight.centreInside ? "inside" : "outside"} the box`;
}

/** The ids of the drawn rows that stand wholly below the list column's visible box, in the order
 *  the list draws them. */
function rowsBelowSight(page: Page): Promise<string[]> {
  return inboxList(page).evaluate((list) => {
    const bottom = Math.min(list.getBoundingClientRect().bottom, window.innerHeight);
    return [...list.querySelectorAll<HTMLElement>("[data-inbox-row]")]
      .filter((node) => node.getBoundingClientRect().top >= bottom)
      .map((node) => node.dataset.inboxRow ?? "");
  });
}

/** Moves the selection to the row at `to` among `ids` with `j` and `k`, from the row it is on. */
async function selectWithKeys(page: Page, ids: readonly string[], to: number): Promise<void> {
  // Each caller reads this once the selection stands on one row, and nothing moves it before
  // the keys below.
  const from = ids.indexOf((await selectedRow(page).getAttribute("data-inbox-row")) ?? "");
  for (let at = from; at !== to; at += to > from ? 1 : -1) {
    await page.keyboard.press(to > from ? "j" : "k");
  }
  await expect(row(page, ids[to] ?? "")).toHaveAttribute("data-inbox-selected", "true");
}

// SLOW(>=2s): a Dismiss, three snoozes and a Move to Inbox over ten seeded rows, then 300
// dismissed threads seeded and the Dismissed view scrolled 600 px at a time to its 150th row.
test("a shift-click selects the drawn rows between two checkboxes", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const lease = await createIssue({ project: "CORE", title: "Lease handover" });
  await patchIssue(lease.key, { priority: 1 });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  // The two bands' threads interleave in time, so the order the list draws them in, band by
  // band, is not the order of the rows sorted by their last reply.
  await seedBulkReplyThreads({ count: 3, issueKey: lease.key, prefix: "Lease" });
  await seedBulkReplyThreads({ count: 3, issueKey: mirror.key, olderThan: 1, prefix: "Mirror" });
  // Three asks with one thread row among them, in a third band.
  const canary = await createIssue({ project: "CORE", title: "Canary rollout" });
  await patchIssue(canary.key, { priority: 3 });
  const asks = [
    (await createAsk(canary.key, { question: "Ship the canary today?" }, asSession())).id,
  ];
  const amid = replyRowId(await seedCommentThread(canary.key, "Who holds the pager?", "I do."));
  asks.push((await createAsk(canary.key, { question: "Widen it tomorrow?" }, asSession())).id);
  asks.push((await createAsk(canary.key, { question: "Roll it back?" }, asSession())).id);

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const dismisses = watchDismisses(page);
    await expect(rows(page)).toHaveCount(10);
    expect(await bandHeadings(page)).toEqual(["P1 (3)", "P2 (3)", "P3 (4)"]);
    const drawn = await drawnIds(inboxList(page));

    // From the second row of the first band to the second row of the second band: the run is
    // the rows the list draws between them, in its order.
    const run = drawn.slice(1, 5);
    await mark(page, drawn[1] ?? "").click();
    await expectCount(page, 1);
    await mark(page, drawn[4] ?? "").click({ modifiers: ["Shift"] });
    // The selection's ids come before the count, so a wrong run fails printing both lists.
    await expect.poll(() => markedIds(page)).toEqual(run);
    await expectCount(page, run.length);

    // One press of Dismiss is one request naming exactly the run, and the run leaves the list.
    const named = await namedByPress(page, () =>
      bar(page).getByRole("button", { name: "Dismiss selected threads" }).click()
    );
    expect([...named].sort()).toEqual([...run].sort());
    expect(dismisses).toHaveLength(1);
    await expect(rows(page)).toHaveCount(drawn.length - run.length);
    expect(await drawnIds(inboxList(page))).toEqual(drawn.filter((id) => !run.includes(id)));

    // Over the third band's asks and the thread among them, one press of Snooze writes one
    // snooze for each ask of the run, and none for the thread.
    const band = drawn.slice(6);
    expect(band).toEqual([asks[2], asks[1], amid, asks[0]]);
    const snoozed: string[] = [];
    page.on("request", (request) => {
      const ask = SNOOZE_PATH.exec(new URL(request.url()).pathname)?.[1];
      if (request.method() === "PUT" && ask !== undefined) snoozed.push(ask);
    });
    await mark(page, band[0] ?? "").click();
    await mark(page, band[3] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, band.length);
    expect(await markedIds(page)).toEqual(band);
    await pickFromList(listTrigger(bar(page), "Snooze selected asks"), "Tomorrow");
    // The asks move into the folded Later band once their writes land, and the thread, which
    // no snooze took, stays marked.
    for (const ask of asks) await expect(row(page, ask)).toHaveCount(0);
    await expectCount(page, 1);
    expect([...snoozed].sort()).toEqual([...asks].sort());
    expect(dismisses).toHaveLength(1);
    await bar(page).getByRole("button", { name: "Clear selection" }).click();

    // In the Dismissed view a range runs over the rows it has loaded, and one press of Move to
    // Inbox names exactly those, which leave the view and stand in the Inbox again.
    await chooseShow(page, "Dismissed");
    await expect(page).toHaveURL(/show=dismissed/);
    await expect(rows(page)).toHaveCount(run.length);
    const loaded = await drawnIds(inboxList(page));
    const back = loaded.slice(0, 3);
    await mark(page, loaded[0] ?? "").click();
    await mark(page, loaded[2] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, back.length);
    expect(await markedIds(page)).toEqual(back);
    const restored = await namedByPress(page, () =>
      bar(page).getByRole("button", { name: "Move to Inbox selected threads" }).click()
    );
    expect([...restored].sort()).toEqual([...back].sort());
    expect(dismisses).toHaveLength(2);
    await expect(rows(page)).toHaveCount(loaded.length - back.length);
    await chooseShow(page, "Inbox");
    for (const id of back) await expect(row(page, id)).toBeVisible();

    // The Dismissed view draws a window of the rows it has loaded, and a range there still runs
    // over every loaded row, drawn or not: 300 dismissed threads, a first page of 200, and a run
    // of the first 150 of them.
    const drill = await createIssue({ project: "CORE", title: "Window drill" });
    await seedDismissedThreads({ count: 300, issueKey: drill.key, prefix: "Drill" });
    const firstPage = page.waitForResponse((response) => isDismissedPage(response));
    await page.goto("/?show=dismissed");
    const loadedIds = pageIds((await (await firstPage).json()) as MeInboxResponse);
    expect(loadedIds).toHaveLength(200);
    const span = loadedIds.slice(0, 150);
    const spanFirst = span[0] ?? "";
    const spanLast = span.at(-1) ?? "";
    await expect(row(page, spanFirst)).toBeVisible();
    const windowed = await drawnIds(inboxList(page));
    expect(windowed.length, `the window draws ${windowed.length} of 200 loaded rows`).toBeLessThan(
      150
    );
    await mark(page, spanFirst).click();
    await expectCount(page, 1);
    // Down the list column until the window draws the 150th row.
    for (let step = 0; step < 60 && (await row(page, spanLast).count()) === 0; step += 1) {
      await inboxList(page).evaluate((list) => {
        list.scrollTop += 600;
      });
      await frameAndTask(page);
    }
    const drawnAtClick = await drawnIds(inboxList(page));
    await mark(page, spanLast).click({ modifiers: ["Shift"] });
    await expect
      .poll(() => barCount(page), {
        message: `the bar counts the run of ${span.length} loaded rows`,
      })
      .toBe(span.length);
    const undrawn = span.filter((id) => !drawnAtClick.includes(id));
    expect(undrawn.length, "rows of the run the window did not draw at the click").toBeGreaterThan(
      0
    );
    const moved = await namedByPress(page, () =>
      bar(page).getByRole("button", { name: "Move to Inbox selected threads" }).click()
    );
    expect([...moved].sort()).toEqual([...span].sort());
    expect(dismisses).toHaveLength(3);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): fourteen seeded rows ranged by thirteen shift-clicks, a filter set and cleared, a
// live arrival waited on, a Dismiss, and a second page that dismisses a held row.
test("the anchor of a range is the row marked last without the shift key", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  const backup = await createIssue({ project: "CORE", title: "Backup window" });
  await patchIssue(backup.key, { priority: 3 });
  await seedBulkReplyThreads({ count: 8, issueKey: mirror.key, prefix: "Mirror" });
  await seedBulkReplyThreads({ count: 6, issueKey: backup.key, olderThan: 20, prefix: "Backup" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pageErrors: string[] = [];
    page.on("pageerror", (error) => pageErrors.push(`${error.name}: ${error.message}`));
    await page.setViewportSize({ height: 900, width: READING_PANE_WIDTH });
    await page.goto("/");
    await expect(rows(page)).toHaveCount(14);
    const drawn = await drawnIds(inboxList(page));
    /** Row `n` of the list, counted from 1. */
    const at = (n: number) => drawn[n - 1] ?? "";
    const rowsFrom = (first: number, last: number) => drawn.slice(first - 1, last);
    const clear = () => bar(page).getByRole("button", { name: "Clear selection" }).click();

    // First pass, with no other row marked: from an anchor on row 2, a shift-click on row 9 and
    // then one on row 5. The second gives the range of its own click alone.
    await mark(page, at(2)).click();
    await mark(page, at(9)).click({ modifiers: ["Shift"] });
    await expectCount(page, 8);
    await mark(page, at(5)).click({ modifiers: ["Shift"] });
    await expectCount(page, 4);
    expect(await markedIds(page)).toEqual(rowsFrom(2, 5));
    await clear();

    // Second pass: row 12 marked with `x` before the anchor is set on row 2. It is outside both
    // runs, so it stays selected.
    await selectRow(page, at(12));
    await page.keyboard.press("x");
    await expectCount(page, 1);
    await mark(page, at(2)).click();
    await mark(page, at(9)).click({ modifiers: ["Shift"] });
    await expectCount(page, 9);
    await mark(page, at(5)).click({ modifiers: ["Shift"] });
    await expectCount(page, 5);
    expect(await markedIds(page)).toEqual([...rowsFrom(2, 5), at(12)]);

    // A shift-click on a selected row takes the whole run out of the selection: row 12 was
    // marked at the anchor, so the run from row 2 to it is unmarked.
    await mark(page, at(12)).click({ modifiers: ["Shift"] });
    await expect(bar(page)).toHaveCount(0);
    expect(await markedIds(page)).toEqual([]);

    // A row marked with `x` is the anchor of the shift-click that follows.
    await selectRow(page, at(6));
    await page.keyboard.press("x");
    await mark(page, at(8)).click({ modifiers: ["Shift"] });
    await expectCount(page, 3);
    expect(await markedIds(page)).toEqual(rowsFrom(6, 8));
    // So is a row unmarked with its checkbox: from row 7, the run to row 4 is marked, and row 8,
    // marked at that anchor, stays.
    await mark(page, at(7)).click();
    await expectCount(page, 2);
    await mark(page, at(4)).click({ modifiers: ["Shift"] });
    await expectCount(page, 5);
    expect(await markedIds(page)).toEqual(rowsFrom(4, 8));

    // After Clear selection the first shift-click marks its own row alone.
    await clear();
    await mark(page, at(9)).click({ modifiers: ["Shift"] });
    await expectCount(page, 1);
    expect(await markedIds(page)).toEqual([at(9)]);

    // After Select all the first shift-click unmarks its own row alone, and that row is the next
    // anchor: from it, the run to row 6, marked at that anchor, is unmarked.
    await bar(page).getByRole("button", { name: "Select all" }).click();
    await expect(bar(page)).toContainText("Every row selected");
    expect(await markedIds(page)).toEqual(drawn);
    await mark(page, at(3)).click({ modifiers: ["Shift"] });
    await expectCount(page, drawn.length - 1);
    expect(await markedIds(page)).toEqual(drawn.filter((id) => id !== at(3)));
    await mark(page, at(6)).click({ modifiers: ["Shift"] });
    await expectCount(page, drawn.length - 4);
    expect(await markedIds(page)).toEqual(drawn.filter((id) => !rowsFrom(3, 6).includes(id)));
    await clear();

    // A filter that hides the anchor: a shift-click then marks its own row alone, which is the
    // next anchor, and the page shows no error.
    const alerts = page.getByRole("alert");
    const plantedAlert = await page.evaluateHandle(() => {
      const element = document.createElement("div");
      element.setAttribute("role", "alert");
      element.textContent = "Range selection alert probe";
      document.body.append(element);
      return element;
    });
    await expect(alerts, "the page's alert locator finds a planted alert").toHaveCount(1);
    await plantedAlert.evaluate((element) => element.remove());
    await plantedAlert.dispose();
    await mark(page, at(3)).click();
    await openFilterPanel(page);
    await page.getByRole("button", { name: "Filter by priority" }).click();
    await page
      .getByRole("listbox", { name: "Priority options" })
      .getByRole("option", { name: /^P3\b/ })
      .click();
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Filter" })).toHaveCount(0);
    await expect(rows(page)).toHaveCount(6);
    const filtered = await drawnIds(inboxList(page));
    expect(filtered.length).toBeGreaterThan(1);
    expect(filtered.length).toBeLessThan(drawn.length);
    expect(filtered).not.toContain(at(3));
    await mark(page, filtered[1] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 1);
    await mark(page, filtered[3] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 3);
    expect(await markedIds(page)).toEqual(filtered.slice(1, 4));
    await expect(alerts, "no alert after changing the range with the anchor hidden").toHaveCount(0);
    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(rows(page)).toHaveCount(drawn.length);
    await clear();

    // A thread arrives between a click and a shift-click. It is the newest of the second band,
    // so the list draws it inside the run, and the run takes it.
    await mark(page, at(7)).click();
    const arrived = replyRowId(
      await seedCommentThread(backup.key, "Backup root new", "Backup reply new")
    );
    await expect(rows(page)).toHaveCount(drawn.length + 1);
    const withArrival = await drawnIds(inboxList(page));
    expect(withArrival.indexOf(arrived)).toBe(withArrival.indexOf(at(8)) + 1);
    await mark(page, at(10)).click({ modifiers: ["Shift"] });
    const counted = [at(7), at(8), arrived, at(9), at(10)];
    await expectCount(page, counted.length);
    expect(await markedIds(page)).toEqual(counted);
    const named = await namedByPress(page, () =>
      bar(page).getByRole("button", { name: "Dismiss selected threads" }).click()
    );
    expect([...named].sort()).toEqual([...counted].sort());
    await expect(rows(page)).toHaveCount(withArrival.length - counted.length);

    // A row the list holds in place under the pointer after the server dropped it: another tab
    // dismisses it. It is drawn, and the range from the anchor to the row in hand leaves it out.
    const left = await drawnIds(inboxList(page));
    const [anchor, , held, , hand] = left;
    if (anchor === undefined || held === undefined || hand === undefined) {
      throw new Error(`the list draws too few rows: ${left.join(", ")}`);
    }
    await mark(page, held).click();
    await mark(page, anchor).click();
    await expectCount(page, 2);
    await selectRow(page, hand);
    // Escape takes the focus off the row, so the hand the list holds a row under is the pointer.
    await page.keyboard.press("Escape");
    await expect(page.locator("body")).toBeFocused();
    await row(page, held).locator("time").first().hover();
    const other = await inboxPage(alice);
    await selectRow(other, held);
    await other.locator(`[data-inbox-dismiss="${held}"]`).first().click();
    await expect(row(other, held)).toHaveCount(0);
    await other.close();
    // The page has read the list again once the held row's own mark has gone from the bar.
    await expectCount(page, 1);
    await expect(row(page, held)).toBeVisible();
    await page.keyboard.press("Shift+X");
    const run = left.slice(0, 5).filter((id) => id !== held);
    await expectCount(page, run.length);
    expect(await markedIds(page)).toEqual(run);
    await expect(row(page, held)).toBeVisible();
    expect(pageErrors, "uncaught page errors during range selection").toEqual([]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): 300 threads seeded in one statement and drawn by five presses of Show more, then
// three more bands seeded and read again, one of them closed over an ask whose snooze is held.
test("one Dismiss over a range of 300 rows is one request", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  await seedBulkReplyThreads({ count: 300, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const dismisses = watchDismisses(page);
    await expect(rows(page)).toHaveCount(50);
    for (let shown = 100; shown <= 300; shown += 50) {
      await page.locator('[data-inbox-show-more="P2"]').click();
      await expect(rows(page)).toHaveCount(shown);
    }
    expect(await bandHeadings(page)).toEqual(["P2 (300)"]);
    const drawn = await drawnIds(inboxList(page));
    await mark(page, drawn[0] ?? "").click();
    await mark(page, drawn[299] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 300);
    const named = await namedByPress(page, () =>
      bar(page).getByRole("button", { name: "Dismiss selected threads" }).click()
    );
    expect(named).toHaveLength(300);
    expect([...named].sort()).toEqual([...drawn].sort());
    await expect(rows(page)).toHaveCount(0);
    expect(dismisses).toHaveLength(1);

    // Three bands: one wider than a page, one closed over an ask whose snooze is in flight, and
    // one drawn whole. A run across them counts the rows the list draws and no other.
    const lease = await createIssue({ project: "CORE", title: "Lease handover" });
    await patchIssue(lease.key, { priority: 1 });
    await seedBulkReplyThreads({ count: 60, issueKey: lease.key, prefix: "Lease" });
    const ask = (await createAsk(mirror.key, { question: "Hold the mirror?" }, asSession())).id;
    await seedBulkReplyThreads({ count: 3, issueKey: mirror.key, olderThan: 700, prefix: "Late" });
    const backup = await createIssue({ project: "CORE", title: "Backup window" });
    await patchIssue(backup.key, { priority: 3 });
    await seedBulkReplyThreads({ count: 4, issueKey: backup.key, prefix: "Backup" });
    await page.reload();
    await expect(rows(page)).toHaveCount(50 + 4 + 4);
    // The ask's snooze is held for the rest of the test, so it stays in flight.
    await page.route("**/api/v1/me/asks/*/snooze", () => undefined);
    await selectRow(page, ask);
    await pickFromList(page.locator("[data-inbox-snooze]").first(), "Tomorrow");
    await inboxList(page)
      .getByRole("button", { name: /^P2 \(/ })
      .click();
    expect(await bandHeadings(page)).toEqual(["P1 (60)", "P2 (4)", "P3 (4)"]);
    await expect(rows(page)).toHaveCount(50 + 1 + 4);
    await expect(row(page, ask)).toBeVisible();
    const across = await drawnIds(inboxList(page));
    await mark(page, across[0] ?? "").click();
    await mark(page, across.at(-1) ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, across.length);
    expect(await markedIds(page)).toEqual(across);
    expect(across).toContain(ask);
    // The rows the run did not draw were not taken into it: the ten behind P1's Show more and
    // the three the closed P2 band hid are drawn unticked once shown, and the bar's count holds.
    await page.locator('[data-inbox-show-more="P1"]').click();
    await expect(rows(page)).toHaveCount(across.length + 10);
    await inboxList(page)
      .getByRole("button", { name: /^P2 \(/ })
      .click();
    await expect(rows(page)).toHaveCount(across.length + 10 + 3);
    expect(await markedIds(page)).toEqual(across);
    await expectCount(page, across.length);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): ten seeded rows ranged from the keyboard in four ways, and the help and the palette
// each opened and read for the three range keys.
test("the keyboard makes the same range from the anchor", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  await seedBulkReplyThreads({ count: 10, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    await expect(rows(page)).toHaveCount(10);
    const drawn = await drawnIds(inboxList(page));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", drawn[0] ?? "");
    // The spoken count is in the page before the first mark.
    const spoken = page.locator("[data-inbox-selection-count]");
    await expect(spoken).toHaveCount(1);
    await expect(spoken).toHaveAttribute("role", "status");
    await expect(spoken).toHaveText("");
    // The pointer leaves the list, and the keymap takes the keys from the body.
    await page.mouse.move(0, 0);
    await page.locator("body").focus();

    // `x` on the second row and `Shift+X` on the seventh: the run between them.
    await selectWithKeys(page, drawn, 1);
    await page.keyboard.press("x");
    await selectWithKeys(page, drawn, 6);
    await page.keyboard.press("Shift+X");
    await expectCount(page, 6);
    expect(await markedIds(page)).toEqual(drawn.slice(1, 7));
    await expect(spoken).toHaveText("6 rows selected");
    await expect(page.getByText("6 selected", { exact: true })).toHaveCount(1);

    // From the same anchor, `Shift+J` three times takes the run to the fourth row with the row in
    // hand, and `Shift+K` once back to the third.
    await selectWithKeys(page, drawn, 1);
    for (let pressed = 0; pressed < 3; pressed += 1) await page.keyboard.press("Shift+J");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", drawn[4] ?? "");
    await expectCount(page, 4);
    expect(await markedIds(page)).toEqual(drawn.slice(1, 5));
    await page.keyboard.press("Shift+K");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", drawn[3] ?? "");
    await expectCount(page, 3);
    expect(await markedIds(page)).toEqual(drawn.slice(1, 4));
    await expect(spoken).toHaveText("3 rows selected");

    // The help lists the three keys, and the palette offers none of them, as it does not offer
    // `x`, while it does offer the Inbox's `f`.
    await page.locator("body").focus();
    await page.keyboard.press("?");
    const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    const inbox = help.getByRole("region", { name: "Inbox" });
    for (const [label, key] of [
      [
        "Select or deselect the rows from the last one marked or unmarked without Shift to the selected row",
        "Shift+X",
      ],
      ["Next row, taking the range to it", "Shift+J"],
      ["Previous row, taking the range to it", "Shift+K"],
    ] as const) {
      await expect(
        inbox.getByRole("listitem").filter({ hasText: label }).locator("kbd")
      ).toHaveText([key]);
    }
    await page.keyboard.press("Escape");
    await expect(help).toHaveCount(0);
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(actions).toBeVisible();
    await expect(actions.getByRole("option", { name: "Search asks" })).toHaveCount(1);
    for (const label of [
      "Select or deselect the selected row",
      "Select or deselect the rows from the last one marked or unmarked without Shift to the selected row",
      "Next row, taking the range to it",
      "Previous row, taking the range to it",
    ]) {
      await expect(actions.getByRole("option", { name: label })).toHaveCount(0);
    }
    await page.keyboard.press("Escape");
    await bar(page).getByRole("button", { name: "Clear selection" }).click();
    await expect(spoken).toHaveText("");

    // With no anchor, as after Clear selection, `Shift+J` only moves the row in hand: it ticks no
    // row and sets no anchor, so the shift-click after it ticks its own row alone.
    await page.locator("body").focus();
    await page.keyboard.press("Shift+J");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", drawn[4] ?? "");
    await expect(bar(page)).toHaveCount(0);
    expect(await markedIds(page)).toEqual([]);
    await mark(page, drawn[7] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 1);
    expect(await markedIds(page)).toEqual([drawn[7]]);
    await bar(page).getByRole("button", { name: "Clear selection" }).click();
    await expect(bar(page)).toHaveCount(0);

    // This engine passes the shift key to the click the space bar makes: the space bar on one
    // checkbox and the shift key with it on a later one make the same run.
    await mark(page, drawn[1] ?? "").focus();
    await page.keyboard.press("Space");
    await expectCount(page, 1);
    await mark(page, drawn[6] ?? "").focus();
    await page.keyboard.press("Shift+Space");
    await expectCount(page, 6);
    expect(await markedIds(page)).toEqual(drawn.slice(1, 7));
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): twelve seeded rows drawn as cards, a shift-click ten rows down, then a scroll back
// and a five-step pointer drag across one row's words.
test("a shift-click selects no text and keeps the focus on its checkbox", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  await seedBulkReplyThreads({ count: 12, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    // Below the column's width each row is a card, whose title is plain text: on a short row
    // the title is a link, which a drag carries rather than selects.
    const page = await inboxPage(alice, "/", READING_PANE_WIDTH - 256);
    await expect(rows(page)).toHaveCount(12);
    const drawn = await drawnIds(inboxList(page));
    const selectedText = () => page.evaluate(() => window.getSelection()?.toString() ?? "");

    // The third row's quoted words: the root's words the card quotes.
    const words = row(page, drawn[2] ?? "")
      .getByRole("paragraph")
      .filter({ hasText: /^Mirror root \d+$/ });

    // A click, a click in the third row's quoted words, and a shift-click ten rows from the
    // first: both checkbox presses landed, and no text is selected. The click in the words puts
    // the caret in text, as a reader's earlier click does, so the read below sees any text a
    // shift-click selects from that caret to the checkbox.
    await mark(page, drawn[0] ?? "").click();
    await words.click();
    const caretInWords = await words.evaluate((element) => {
      const selection = window.getSelection();
      const anchor = selection?.anchorNode;
      return (
        selection?.isCollapsed && anchor?.nodeType === Node.TEXT_NODE && element.contains(anchor)
      );
    });
    expect(caretInWords, "the plain click leaves a collapsed caret in the third row's quote").toBe(
      true
    );
    await mark(page, drawn[10] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 11);
    expect(await selectedText(), "the page's selected text").toBe("");
    expect(
      await holdsFocus(mark(page, drawn[10] ?? "")),
      `the shift-clicked checkbox holds the focus; the focus is on ${await describeFocus(page)}`
    ).toBe(true);

    // A drag across one row's text still selects it, and only it: the third row's quoted words.
    // The shift-click scrolled the eleventh row into view, so the words stand above the window:
    // a drag must start where the pointer can reach them, as Firefox finds nothing to select
    // outside the window and Chromium selects everything from the page's top.
    await words.scrollIntoViewIfNeeded();
    // The count of eleven above waited for the list's rows, and nothing moves this card between
    // the scroll and the drag.
    const box = await words.boundingBox();
    if (box === null) throw new Error("the row's quoted words are not drawn");
    await page.mouse.move(box.x + 1, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width - 1, box.y + box.height / 2, { steps: 5 });
    await page.mouse.up();
    expect(await selectedText(), "the text a drag across the row's words selects").toBe(
      (await words.textContent()) ?? ""
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): twelve seeded rows, a range of six made and dismissed from the keyboard, then a
// second Inbox page opened to dismiss a marked row and its live update waited on.
test("a range Dismiss hands the focus to a row that stays", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  await seedBulkReplyThreads({ count: 12, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    await expect(rows(page)).toHaveCount(12);
    // The count above waited for all twelve rows, and nothing moves them before the keys do.
    const drawn = await drawnIds(inboxList(page));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", drawn[0] ?? "");
    await page.mouse.move(0, 0);
    await page.locator("body").focus();

    // The keyboard alone: `x` on row 3, `Shift+X` on row 8, the focus into the bar, and Enter
    // on Dismiss.
    await selectWithKeys(page, drawn, 2);
    await page.keyboard.press("x");
    await selectWithKeys(page, drawn, 7);
    await page.keyboard.press("Shift+X");
    await expectCount(page, 6);
    const dismiss = bar(page).getByRole("button", { name: "Dismiss selected threads" });
    await tabTo(page, dismiss, "the bar's Dismiss", 80);
    const before = await scrollPositions(page);
    await page.keyboard.press("Enter");
    for (const id of drawn.slice(2, 8)) await expect(row(page, id)).toHaveCount(0);

    // The row now at the place of row 3, the row marked last, is row 9, and it is in sight: the
    // focus is on its checkbox, and nothing scrolled.
    await frameAndTask(page);
    const place = drawn[8] ?? "";
    expect(await drawnIds(inboxList(page))).toEqual([...drawn.slice(0, 2), ...drawn.slice(8)]);
    expect(await markInSight(page, place), "row 9 is in the list's visible box").toBe(true);
    expect(
      await holdsFocus(mark(page, place)),
      `the focus is on row 9's checkbox; it is on ${await describeFocus(page)}`
    ).toBe(true);
    expect(await scrollPositions(page)).toEqual(before);

    // Two rows marked, and a second tab dismisses one of them while the focus is on its
    // checkbox. The person did not cause that loss: the bar's count drops by one, the row stays
    // drawn in its place, held under the person's hand, with the focus still on its checkbox,
    // and nothing scrolls.
    const [kept, gone] = [drawn[9] ?? "", drawn[10] ?? ""];
    await mark(page, kept).click();
    await mark(page, gone).click();
    await expectCount(page, 2);
    await expect(mark(page, gone)).toBeFocused();
    const standing = await drawnIds(inboxList(page));
    const still = await scrollPositions(page);
    const other = await inboxPage(alice);
    await selectRow(other, gone);
    await other.locator(`[data-inbox-dismiss="${gone}"]`).first().click();
    await expect(row(other, gone)).toHaveCount(0);
    await other.close();
    await expectCount(page, 1);
    await frameAndTask(page);
    expect(await drawnIds(inboxList(page)), "the held row is drawn in its place").toEqual(standing);
    expect(
      await holdsFocus(mark(page, gone)),
      `the focus stays on the held row's checkbox; it is on ${await describeFocus(page)}`
    ).toBe(true);
    expect(await scrollPositions(page)).toEqual(still);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): thirty-two dismissed threads seeded and the Dismissed view loaded, a range of four
// marked, the list scrolled and one Move to Inbox pressed.
test("a range action whose rows stood above the view hands the focus to the first row after them in sight", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  const count = 32;
  await seedDismissedThreads({ count, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice, "/?show=dismissed");
    const box = inboxList(page);
    await expect(rows(page).first()).toHaveAttribute("aria-setsize", String(count));
    const drawn = await drawnIds(box);
    expect(drawn.length, "the Dismissed window draws at least ten rows").toBeGreaterThanOrEqual(10);
    // The reading pane shows the first row, and the range holds it: the press takes the selected
    // row away with the others, and the selection moves to a row that stays with no scroll. The
    // bar's return scrolls the list by the 4 px that show the landing checkbox whole, and the
    // window not at all.
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", drawn[0] ?? "");
    const range = drawn.slice(0, 4);
    await mark(page, range[0] ?? "").click();
    await mark(page, range[3] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 4);
    expect(await markedIds(page)).toEqual(range);

    // The tenth row lands. Each row after the four moves up by the space the four take, from the
    // first one's top to the top of the row after them, once they have gone. The list is scrolled
    // so that the tenth row's checkbox will stand cut by the list's top edge by 4 px then, its
    // centre inside the list's visible box, where the bar's return still reads a row's mark as in
    // sight (`markShown`). The range, the selected row among it, and the row after it stand wholly
    // above the edge now, so only the search forward from the row at the place reaches a row in
    // sight.
    const landing = drawn[9] ?? "";
    const space = await box.evaluate(
      (list, ids) => {
        const top = (id: string) =>
          list.querySelector(`[data-inbox-row="${id}"]`)?.getBoundingClientRect().top ?? Number.NaN;
        return top(ids.after) - top(ids.first);
      },
      { after: drawn[4] ?? "", first: range[0] ?? "" }
    );
    expect(space, `the four marks take ${space} px of the list`).toBeGreaterThan(0);
    const placing = await markSight(page, landing, space);
    const scroll = placing.top - (placing.seenTop - CUT_PX);
    await box.evaluate((list, by) => {
      list.scrollTop += by;
    }, scroll);
    await frameAndTask(page);
    const above = await box.evaluate((list) => {
      const top = list.getBoundingClientRect().top;
      return [...list.querySelectorAll<HTMLElement>("[data-inbox-row]")]
        .filter((node) => node.getBoundingClientRect().bottom <= top)
        .map((node) => node.dataset.inboxRow);
    });
    expect(
      above.slice(0, 5),
      "the four marks, the selected row among them, and the row after them stand wholly above sight"
    ).toEqual(drawn.slice(0, 5));
    const coming = await markSight(page, landing, space);
    expect(
      Math.abs(coming.cutAbove - CUT_PX),
      `once the four have gone, the tenth row's checkbox stands cut by the list's top edge by ${CUT_PX} px; ${sightWords(coming)}`
    ).toBeLessThanOrEqual(EDGE_SLACK_PX);
    expect(
      coming.centreInside,
      `once the four have gone, the tenth row's checkbox has its centre inside the list's visible box; ${sightWords(coming)}`
    ).toBe(true);
    const before = await scrollPositions(page);
    await bar(page).getByRole("button", { name: "Move to Inbox selected threads" }).click();
    for (const id of range) await expect(row(page, id)).toHaveCount(0);
    await frameAndTask(page);
    // The selection moved to a row that stays and stood above sight, so a scroll to it would have
    // moved the list: the read below of the list's scroll, 4 px and no more, holds the move to
    // scrolling nothing.
    await expect(selectedRow(page)).toHaveCount(1);
    const selected = await selectedRow(page).getAttribute("data-inbox-row");
    expect(
      above.slice(range.length),
      "the selection moved to a row that stays, above sight"
    ).toContain(selected);

    // The focus is on the tenth row's checkbox, and the bar's return has shown it whole at the
    // list's top edge: the list scrolled up by the 4 px of its cut and no more.
    expect(
      await holdsFocus(mark(page, landing)),
      `the focus is on the tenth row's checkbox (${landing}), the first after the four in sight; it is on ${await describeFocus(page)}`
    ).toBe(true);
    const shown = await markSight(page, landing);
    expect(
      wholeInSight(shown),
      `the tenth row's checkbox stands whole in the list's visible box; ${sightWords(shown)}`
    ).toBe(true);
    expect(
      await firstMarkInSight(page),
      "the tenth row is the first row after the four whose checkbox is in sight"
    ).toBe(landing);
    const after = await scrollPositions(page);
    expect(
      Math.abs(before.list - after.list - CUT_PX),
      `the list scrolled up by ${before.list - after.list} px, from ${before.list} to ${after.list}; want ${CUT_PX} px`
    ).toBeLessThanOrEqual(EDGE_SLACK_PX);
    expect(after.window, `the window's scroll, ${before.window} before the press`).toBe(
      before.window
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): twenty-four seeded rows, the last three marked, the list scrolled and one Dismiss
// pressed.
test("a range action that lands on a checkbox partly in sight with nothing taken away above it shows that checkbox whole", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  const count = 24;
  await seedBulkReplyThreads({ count, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    await expect(rows(page)).toHaveCount(count);
    // The count above waited for every row, and nothing moves them before the marks do.
    const drawn = await drawnIds(inboxList(page));

    // The list's last three rows, the last one marked first: the row marked last without the
    // shift key is the list's last, so no row stands after it, at its place. The press takes
    // nothing away above the row before the three, which lands as the nearest row in sight
    // before that place.
    const range = drawn.slice(-3);
    const landing = drawn.at(-4) ?? "";
    await mark(page, range[2] ?? "").click();
    await mark(page, range[0] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 3);
    expect(await markedIds(page)).toEqual(range);

    // The list is scrolled back up until the three stand wholly below the view and the landing
    // row's checkbox stands cut by the list's bottom edge by 4 px, its centre inside the list's
    // visible box, where the bar's return still reads a row's mark as in sight (`markShown`). The
    // row after the landing, the first of the three, stands wholly below the view, so it cannot
    // take the focus. Once the three have gone, the landing row itself still reaches below the
    // view, so the list's scroll range still holds the scroll.
    const placing = await markSight(page, landing);
    const scroll = placing.bottom - (placing.seenBottom + CUT_PX);
    await inboxList(page).evaluate((list, by) => {
      list.scrollTop += by;
    }, scroll);
    await frameAndTask(page);
    const cut = await markSight(page, landing);
    expect(
      Math.abs(cut.cutBelow - CUT_PX),
      `the landing checkbox stands cut by the list's bottom edge by ${CUT_PX} px; ${sightWords(cut)}`
    ).toBeLessThanOrEqual(EDGE_SLACK_PX);
    expect(
      cut.centreInside,
      `the landing checkbox has its centre inside the list's visible box; ${sightWords(cut)}`
    ).toBe(true);
    expect(
      await rowsBelowSight(page),
      "the three marks, the row after the landing first, and no other row stand wholly below the view"
    ).toEqual(range);
    const before = await scrollPositions(page);
    await bar(page).getByRole("button", { name: "Dismiss selected threads" }).click();
    for (const id of range) await expect(row(page, id)).toHaveCount(0);
    await frameAndTask(page);

    // The focus is on the landing checkbox, and the bar's return has shown it whole at the list's
    // bottom edge: the list scrolled down by the 4 px of its cut and no more.
    expect(
      await drawnIds(inboxList(page)),
      "the press took the three marks away and no other row"
    ).toEqual(drawn.slice(0, -3));
    expect(
      await holdsFocus(mark(page, landing)),
      `the focus is on the checkbox of the row before the three (${landing}); it is on ${await describeFocus(page)}`
    ).toBe(true);
    const shown = await markSight(page, landing);
    expect(
      wholeInSight(shown),
      `the landing checkbox stands whole in the list's visible box; ${sightWords(shown)}`
    ).toBe(true);
    const after = await scrollPositions(page);
    expect(
      Math.abs(after.list - before.list - CUT_PX),
      `the list scrolled down by ${after.list - before.list} px, from ${before.list} to ${after.list}; want ${CUT_PX} px`
    ).toBeLessThanOrEqual(EDGE_SLACK_PX);
    expect(after.window, `the window's scroll, ${before.window} before the press`).toBe(
      before.window
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): forty seeded rows, one marked, the list scrolled back to its top and Clear
// selection pressed.
test("the return to the row marked last takes the least scroll to its checkbox", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  const count = 40;
  await seedBulkReplyThreads({ count, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    await expect(rows(page)).toHaveCount(count);
    // The count above waited for every row, and nothing moves them before the mark does.
    const drawn = await drawnIds(inboxList(page));

    // A mark on row 21, then the list scrolled back to its top: row 21 stands wholly below the
    // view, with the list's last rows below it.
    const marked = drawn[20] ?? "";
    await mark(page, marked).click();
    await expectCount(page, 1);
    await inboxList(page).evaluate((list) => {
      list.scrollTop = 0;
    });
    await frameAndTask(page);
    const below = await rowsBelowSight(page);
    expect(
      drawn.slice(20).filter((id) => !below.includes(id)),
      "row 21 and every row after it stand wholly below the view; these do not"
    ).toEqual([]);

    // The press is Clear selection, which takes no row away. The bar goes with the focus in it,
    // and its return puts the focus on row 21's checkbox, the row marked last, which the list
    // still draws, and shows it whole with the least scroll: its bottom edge at the view's bottom
    // edge, the nearest. A browser that centres the focused control puts it far from that edge.
    const standing = await drawnIds(inboxList(page));
    const before = await scrollPositions(page);
    await bar(page).getByRole("button", { name: "Clear selection" }).click();
    await expect(bar(page)).toHaveCount(0);
    await frameAndTask(page);
    expect(await drawnIds(inboxList(page)), "Clear selection took no row away").toEqual(standing);
    expect(
      await holdsFocus(mark(page, marked)),
      `the focus is on row 21's checkbox, the row marked last; it is on ${await describeFocus(page)}`
    ).toBe(true);
    const shown = await markSight(page, marked);
    expect(
      wholeInSight(shown),
      `row 21's checkbox stands whole in the list's visible box; ${sightWords(shown)}`
    ).toBe(true);
    expect(
      Math.abs(shown.bottom - shown.seenBottom),
      `row 21's checkbox stands with its bottom edge at the view's bottom edge; ${sightWords(shown)}`
    ).toBeLessThanOrEqual(EDGE_SLACK_PX);
    const after = await scrollPositions(page);
    expect(after.window, `the window's scroll, ${before.window} before the press`).toBe(
      before.window
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): eight seeded rows, the Agents page loaded and the drawer opened over it, then one
// range made by click and another by key.
test("the drawer makes the same range by click and by key", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  await seedBulkReplyThreads({ count: 8, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: READING_PANE_WIDTH });
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("i");
    const peek = drawer(page);
    const listed = inboxList(peek).locator("[data-inbox-row]");
    await expect(listed).toHaveCount(8);
    const drawn = await listed.evaluateAll((nodes) =>
      nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
    );
    const box = (id: string) => peek.locator(`[data-inbox-row="${id}"]`).getByRole("checkbox");
    const ticked = () =>
      listed.evaluateAll((nodes) =>
        nodes
          .filter((node) => node.querySelector<HTMLInputElement>('input[type="checkbox"]')?.checked)
          .map((node) => node.getAttribute("data-inbox-row") ?? "")
      );
    const peekBar = peek.getByRole("toolbar", { name: "Selected rows" });

    // By click: the second row, then the fifth with the shift key.
    await box(drawn[1] ?? "").click();
    await box(drawn[4] ?? "").click({ modifiers: ["Shift"] });
    await expect(peekBar.getByText("4 selected", { exact: true })).toBeVisible();
    expect(await ticked()).toEqual(drawn.slice(1, 5));
    await peekBar.getByRole("button", { name: "Clear selection" }).click();
    await expect(peekBar).toHaveCount(0);

    // By key, on the focused row, as the drawer's keys act: `x` on the second row, `Shift+X` on
    // the fourth, then `Shift+J` to the fifth.
    await peek.locator(`[data-inbox-row="${drawn[1]}"]`).focus();
    await page.keyboard.press("x");
    await page.keyboard.press("j");
    await page.keyboard.press("j");
    await expect(peek.locator(`[data-inbox-row="${drawn[3]}"]`)).toBeFocused();
    await page.keyboard.press("Shift+X");
    await expect(peekBar.getByText("3 selected", { exact: true })).toBeVisible();
    await page.keyboard.press("Shift+J");
    await expect(peek.locator(`[data-inbox-row="${drawn[4]}"]`)).toBeFocused();
    await expect(peekBar.getByText("4 selected", { exact: true })).toBeVisible();
    expect(await ticked()).toEqual(drawn.slice(1, 5));
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): forty seeded rows drawn twice, over a reload, with a 29-row range and a Select all
// each cleared and the focus read after.
test("the bar's return keeps the row marked last, or takes the first row in sight with no mark", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const mirror = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(mirror.key, { priority: 2 });
  await seedBulkReplyThreads({ count: 40, issueKey: mirror.key, prefix: "Mirror" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    await expect(rows(page)).toHaveCount(40);
    const drawn = await drawnIds(inboxList(page));
    const list = inboxList(page);

    // A mark on row 2, then a range taken to row 30 far below it: Clear selection hands the
    // focus to row 2's checkbox, the row marked last, and brings it into view.
    await mark(page, drawn[1] ?? "").click();
    await mark(page, drawn[29] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 29);
    expect(await markInSight(page, drawn[1] ?? ""), "row 2 is out of sight").toBe(false);
    await bar(page).getByRole("button", { name: "Clear selection" }).click();
    await expect(bar(page)).toHaveCount(0);
    await frameAndTask(page);
    expect(
      await holdsFocus(mark(page, drawn[1] ?? "")),
      `the focus is on row 2's checkbox; it is on ${await describeFocus(page)}`
    ).toBe(true);
    expect(await markInSight(page, drawn[1] ?? ""), "row 2 is in sight").toBe(true);

    // A page with no row marked yet: the first mark is a shift-click with no anchor, which
    // marks no row as the one marked last. Select all, then Clear selection, hands the focus to
    // the first row in sight, with no scroll.
    await page.reload();
    await expect(rows(page)).toHaveCount(40);
    await list.evaluate((node) => {
      node.scrollTop = 600;
    });
    await frameAndTask(page);
    await mark(page, drawn[25] ?? "").click({ modifiers: ["Shift"] });
    await expectCount(page, 1);
    await bar(page).getByRole("button", { name: "Select all" }).click();
    await expect(bar(page)).toContainText("Every row selected");
    const before = await scrollPositions(page);
    const firstInSight = (await firstMarkInSight(page)) ?? "";
    expect(
      drawn.indexOf(firstInSight),
      "the first row in sight is below the list's top"
    ).toBeGreaterThan(0);
    await bar(page).getByRole("button", { name: "Clear selection" }).click();
    await expect(bar(page)).toHaveCount(0);
    await frameAndTask(page);
    expect(
      await holdsFocus(mark(page, firstInSight)),
      `the focus is on the first row in sight; it is on ${await describeFocus(page)}`
    ).toBe(true);
    expect(await scrollPositions(page)).toEqual(before);
  } finally {
    await alice.close();
  }
});
