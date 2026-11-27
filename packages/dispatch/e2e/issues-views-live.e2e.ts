/** Every change is another reader's write through the server, which reaches the open pages over
 *  the live socket, and the Work time a facts write or a close moves is the facts consumer's row
 *  and the items route's. */
// The Issues tab and the pages beside it hold still while another reader writes (dispatch://
// INFRA-3221, Verification k13's browser parts): each write of context B shows in context A with
// no reload; Open and Done open at their top with their first row whole; a row or a card the
// reader rests on, focuses or reads at the top keeps its place through a write above it, in the
// table, on the Board, on the priority pages; /delivery's new merge stands first as on `main`;
// the Work time a facts write and a close move reaches an open page with no reload; and a live
// socket held after its first snapshot changes nothing until a reload.
//
// ACME is seeded with `seedLegionFixture`, its daemon named at a `startFakeLegionDaemon`, with
// `seedLegionMetricsCorpus` and with `pushLegionObservation`, WIDGETS with
// `seedLegionFactsFixture`. Each facts wait (up to 75 s) and each close step (135 s from the
// close) is a row of its own (the plan's D16). A row that measures a position on ACME seeds a
// failed read of its daemon first and holds every board frame after the first
// (`seedMeasuredAcme`).
import type { EvalItemsResponse } from "../web/src/api/types";
import {
  createIssue,
  createPriority,
  createProject,
  getIssue,
  getLegionBoard,
  getLegionWorkTimeItems,
  legionWorkTimeEval,
  listIssuesWhere,
  patchIssue,
  patchPriority,
  waitForLegionWorkTimeEval,
} from "./api";
import { seedDeliveryFixture } from "./delivery-seed";
import {
  ACME,
  boardDrawn,
  cardsOf,
  columnCount,
  issuesTable,
  routeLive,
  rowKeys,
  seedAcme,
  seedMeasuredAcme,
  tabCount,
  workWords,
} from "./issues-views-shared";
import {
  LEGION_FACTS_PROJECT,
  seedLegionFactsFixture,
  waitForIssueFacts,
  writeRoleEvent,
} from "./legion-facts-seed";
import { pushLegionObservation, seedFailedLegionRead, startFakeLegionDaemon } from "./legion-seed";
import { sql } from "./psql";
import { type BrowserContext, expect, type Page, test } from "./test";
import { asUser } from "./users";

const WIDGETS = LEGION_FACTS_PROJECT;
const B = { login: "bob" } as const;
const SIZES = [
  { height: 900, width: 1440 },
  { height: 844, width: 390 },
] as const;

/** The table's scroll box. */
const scrollBox = (page: Page) => page.getByTestId("issues-table-scroll");

/** The box's scroll offset, the top of its first drawn row and its key, and the bottom of the
 *  table's header. */
function topOfTable(page: Page) {
  return scrollBox(page).evaluate((node) => {
    const row = node.querySelector("tbody tr[data-issue-row]");
    return {
      // The box scrolls where its rows pass its height; the window carries the rest.
      offset: node.scrollTop + window.scrollY,
      key: row?.getAttribute("data-issue-row") ?? "",
      rowTop: row?.getBoundingClientRect().top ?? Number.NaN,
      headerBottom: node.querySelector("thead")?.getBoundingClientRect().bottom ?? Number.NaN,
    };
  });
}

/** The top of `key`'s row in the window. */
const rowTop = (page: Page, key: string) =>
  page.locator(`tr[data-issue-row="${key}"]`).evaluate((row) => row.getBoundingClientRect().top);

/** The first row whose top is at or under the header's bottom, and its top. */
function firstOnScreen(page: Page) {
  return scrollBox(page).evaluate((node) => {
    // The header's cells stay at the box's top while the box scrolls (each `th` is sticky; the
    // `thead` itself scrolls away with the rows); once the window scrolls them past the window's
    // top, the window's top is where the rows on screen start.
    const bottom = Math.max(0, node.querySelector("thead th")?.getBoundingClientRect().bottom ?? 0);
    const row = [...node.querySelectorAll("tbody tr[data-issue-row]")].find(
      (one) => one.getBoundingClientRect().top >= bottom - 1
    );
    return {
      key: row?.getAttribute("data-issue-row") ?? "",
      top: row?.getBoundingClientRect().top ?? Number.NaN,
    };
  });
}

/** Moves the pointer to a corner the page draws nothing under, and takes the focus off. */
async function handsOff(page: Page): Promise<void> {
  await page.mouse.move(2, 2);
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
}

/** A title long enough to take more lines in a narrow cell. */
const LONG_TITLE =
  "A title written long enough that it wraps onto more lines than it took before, in any cell";

/** `count` open issues of ACME created through the API, one after another. */
async function seedOpenIssues(count: number, status = "todo"): Promise<string[]> {
  const keys: string[] = [];
  for (let n = 1; n <= count; n++) {
    const issue = await createIssue({ project: ACME, title: `Open ${status} work ${n}` });
    await patchIssue(issue.key, { status });
    keys.push(issue.key);
  }
  return keys;
}

/** A Todo issue with no card, an open issue with Legion work, and the four ACME writes k13's
 *  first part makes as context B: a new issue, the Todo issue to Backlog, the worked issue closed,
 *  and ACME-5's card moved from Queue to Design. */
async function writeAsB(todo: string, worked: string): Promise<string> {
  const made = await createIssue({ project: ACME, title: "Made by the other reader" }, B);
  await patchIssue(todo, { status: "backlog" }, B);
  await patchIssue(worked, { status: "done" }, B);
  // ACME-5 stands in the Queue because a blocker link holds it; a read whose admission block
  // holds it no more puts its card, still `admitted`, in Design.
  await pushLegionObservation(
    "",
    "admission",
    JSON.stringify({
      cap: 4,
      active: ["ACME-1", "ACME-2"],
      waiting: ["ACME-4", "ACME-3"],
      trees: ["ACME-1", "ACME-2"],
      blocked: [],
    })
  );
  return made.key;
}

/** k13's first part's seed: a Todo issue with no card, and ACME-2 with Legion work. */
async function seedWrites(): Promise<string> {
  const todo = await createIssue({ project: ACME, title: "Todo with no card" });
  await patchIssue(todo.key, { status: "todo" });
  await writeRoleEvent("ACME-2", "architect", "50 minutes");
  await waitForIssueFacts(["ACME-2"]);
  return todo.key;
}

/** Three pages of A on Open, Done and Open with the Queue filter, each once the board's first
 *  snapshot has arrived, with what each lists before B writes. */
async function openThree(context: BrowserContext, block: boolean) {
  const pages: Record<string, Page> = {};
  for (const [name, address] of [
    ["open", "view=open"],
    ["done", "view=done"],
    ["queued", "view=open&f.queue=Queued"],
  ] as const) {
    const page = await context.newPage();
    if (block) await routeLive(page, { block: true });
    await page.setViewportSize({ height: 1600, width: 1440 });
    await page.goto(`/projects/${ACME}/issues?${address}`);
    await expect(issuesTable(page).locator("tbody tr[data-issue-row]").first()).toBeVisible();
    // The Queue cell of ACME-4 is its Place once the board's first snapshot is drawn.
    if (name !== "done")
      await expect(page.locator('tr[data-issue-row="ACME-4"]')).toContainText("#1");
    pages[name] = page;
  }
  return pages as { open: Page; done: Page; queued: Page };
}

// SLOW(>=2s): the facts consumer's row of ACME-2, up to 75 s, then three pages read through four
// writes of another reader.
test("with the live socket open each write of another reader shows on Open, on Done and on Open with the Queue filter with no reload", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const todo = await seedWrites();
    const open = await listIssuesWhere({ open: true, project: ACME });
    const done = await listIssuesWhere({ project: ACME, status: ["done"] });
    const pages = await openThree(context, false);
    const queuedBefore = await rowKeys(issuesTable(pages.queued));
    expect(queuedBefore).toContain("ACME-5");
    await expect.poll(() => tabCount(pages.open, "Open")).toBe(open.length);

    const made = await writeAsB(todo, "ACME-2");

    // Open: the new issue listed, its count one more, Backlog in the Status cell and on the
    // bar, and ACME-5 in Design.
    const openTable = issuesTable(pages.open);
    await expect(openTable.locator(`tr[data-issue-row="${made}"]`)).toBeVisible();
    await expect.poll(() => tabCount(pages.open, "Open")).toBe(open.length);
    await expect(openTable.locator(`tr[data-issue-row="${todo}"]`)).toContainText("Backlog");
    await expect(pages.open.locator('[data-bar-legend="backlog"]')).toBeVisible();
    await expect(openTable.locator('tr[data-issue-row="ACME-5"]')).toContainText("Design");
    await expect(openTable.locator('tr[data-issue-row="ACME-2"]')).toHaveCount(0);

    // Done: the closed issue first, its count one more, and Open's count after the create and
    // the close.
    const doneTable = issuesTable(pages.done);
    await expect.poll(async () => (await rowKeys(doneTable))[0]).toBe("ACME-2");
    await expect.poll(() => tabCount(pages.done, "Done")).toBe(done.length + 1);
    await expect.poll(() => tabCount(pages.done, "Open")).toBe(open.length);

    // Open with the Queue filter: ACME-5 left the queue.
    const queuedTable = issuesTable(pages.queued);
    await expect
      .poll(() => rowKeys(queuedTable))
      .toEqual(queuedBefore.filter((key) => key !== "ACME-5"));
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): the facts consumer's row of ACME-2, up to 75 s, then three pages with their live
// sockets held after the first snapshot, read through four writes and a reload.
test("with every live socket held after its first snapshot, reconnects included, no write of another reader changes a page until it reloads", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const todo = await seedWrites();
    const pages = await openThree(context, true);
    const before = {
      open: await rowKeys(issuesTable(pages.open)),
      done: await rowKeys(issuesTable(pages.done)),
      queued: await rowKeys(issuesTable(pages.queued)),
      openTab: await tabCount(pages.open, "Open"),
      doneTab: await tabCount(pages.done, "Done"),
    };
    const made = await writeAsB(todo, "ACME-2");
    await expect.poll(async () => (await getIssue("ACME-2")).status).toBe("done");
    // Nothing may change on the held pages: the row above reads these same writes at once on an
    // open socket, so three seconds is room for a write the held sockets let through to show.
    await pages.open.waitForTimeout(3_000);
    expect(await rowKeys(issuesTable(pages.open))).toEqual(before.open);
    expect(await rowKeys(issuesTable(pages.done))).toEqual(before.done);
    expect(await rowKeys(issuesTable(pages.queued))).toEqual(before.queued);
    expect(await tabCount(pages.open, "Open")).toBe(before.openTab);
    expect(await tabCount(pages.done, "Done")).toBe(before.doneTab);
    await expect(issuesTable(pages.open).locator(`tr[data-issue-row="${todo}"]`)).not.toContainText(
      "Backlog"
    );
    // The reload shows them.
    await pages.open.reload();
    await expect(issuesTable(pages.open).locator(`tr[data-issue-row="${made}"]`)).toBeVisible();
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): Open and Done each opened at two sizes, the second time with a Place written.
test("Open and Done open at the box's top with the first row whole under the header at both sizes, and with the board's snapshot held until the rows are painted Open still starts with the issue of Place 1", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const first = await context.newPage();
  const daemon = await seedMeasuredAcme(first);
  try {
    for (const size of SIZES) {
      for (const view of ["open", "done"]) {
        const page = await context.newPage();
        await routeLive(page, { hold: true });
        await page.setViewportSize(size);
        await page.goto(`/projects/${ACME}/issues?view=${view}`);
        await expect(issuesTable(page).locator("tbody tr[data-issue-row]").first()).toBeVisible();
        const read = await topOfTable(page);
        const where = `${view} at ${size.width}x${size.height}`;
        expect(read.offset, `${where}: the box's offset`).toBe(0);
        expect(
          Math.abs(read.rowTop - read.headerBottom),
          `${where}: the first row's top`
        ).toBeLessThanOrEqual(1);
        await page.close();
      }
    }

    // The open issue first by rank gets Place 2, another Place 1.
    const open = await listIssuesWhere({ open: true, project: ACME });
    const byRank = [...open].sort((a, b) => (a.rank < b.rank ? -1 : 1));
    const [rankFirst, other] = [byRank[0]?.key ?? "", byRank.at(-1)?.key ?? ""];
    await pushLegionObservation(
      "",
      "admission",
      JSON.stringify({
        cap: 4,
        active: ["ACME-1", "ACME-2"],
        waiting: [other, rankFirst],
        trees: ["ACME-1", "ACME-2"],
        blocked: [],
      })
    );
    await expect
      .poll(async () => cardsOf(await getLegionBoard(ACME)).get(other)?.card.waiting_place)
      .toBe(1);
    for (const size of SIZES) {
      const page = await context.newPage();
      const painted = Promise.withResolvers<void>();
      await routeLive(page, { first: () => painted.promise, hold: true });
      await page.setViewportSize(size);
      await page.goto(`/projects/${ACME}/issues`);
      await expect(issuesTable(page).locator("tbody tr[data-issue-row]").first()).toBeVisible();
      painted.resolve();
      await expect(page.locator(`tr[data-issue-row="${other}"]`)).toContainText("#1");
      const read = await topOfTable(page);
      const where = `at ${size.width}x${size.height}`;
      expect(read.offset, `${where}: the box's offset`).toBe(0);
      expect(read.key, `${where}: the first row`).toBe(other);
      expect(
        Math.abs(read.rowTop - read.headerBottom),
        `${where}: the first row's top`
      ).toBeLessThanOrEqual(1);
      await page.close();
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): 30 issues seeded, then at two sizes four writes of another reader with the pointer
// on a row, a failed listing read with its alert, and a write with the box at its top.
test("in the table a row the pointer rests on keeps its top through writes above it and an alert over the table, and at the box's top the first row on screen keeps its top as a new issue goes above it", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    await seedOpenIssues(30);
    for (const size of SIZES) {
      const where = `at ${size.width}x${size.height}`;
      await page.setViewportSize(size);
      await page.goto(`/projects/${ACME}/issues?view=open&sort=-created`);
      const table = issuesTable(page);
      await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
      await boardDrawn(page);
      const keys = await rowKeys(table);
      // Five rows above the box's top, and R the row under them.
      await scrollBox(page).evaluate((node) => {
        const rows = [...node.querySelectorAll("tbody tr[data-issue-row]")];
        const head = node.querySelector("thead")?.getBoundingClientRect().bottom ?? 0;
        const sixth = rows[6];
        if (sixth === undefined) return;
        // The box where it scrolls; the window where the box keeps still.
        const before = node.scrollTop;
        node.scrollTop += sixth.getBoundingClientRect().top - head;
        if (node.scrollTop === before) window.scrollBy(0, sixth.getBoundingClientRect().top);
      });
      const r = (await firstOnScreen(page)).key;
      const index = keys.indexOf(r);
      expect(index, `${where}: rows above R`).toBeGreaterThanOrEqual(5);
      await page.locator(`tr[data-issue-row="${r}"]`).hover();
      const top = await rowTop(page, r);
      let count = (await tabCount(page, "Open")) ?? 0;
      const holds = async (what: string) => {
        await expect
          .poll(async () => Math.abs((await rowTop(page, r)) - top), {
            message: `${where}: ${what}`,
          })
          .toBeLessThanOrEqual(1);
      };
      await patchIssue(keys[index - 2] ?? "", { title: `${LONG_TITLE} ${size.width}` }, B);
      await expect(page.locator(`tr[data-issue-row="${keys[index - 2]}"]`)).toContainText(
        LONG_TITLE
      );
      await holds("a title above R changed");
      const made = await createIssue(
        {
          project: ACME,
          title: size.width === 1440 ? "Lantern shelf bracket" : "Pulley rope spool",
        },
        B
      );
      await patchIssue(made.key, { status: "todo" }, B);
      await expect.poll(() => tabCount(page, "Open")).toBe(count + 1);
      count += 1;
      await holds("an issue made above R");
      await patchIssue(keys[index - 3] ?? "", { status: "backlog" }, B);
      await expect(page.locator(`tr[data-issue-row="${keys[index - 3]}"]`)).toContainText(
        "Backlog"
      );
      await holds("an issue above R set to Backlog");
      expect(await tabCount(page, "Open")).toBe(count);

      // The next reads of the open listing fail: the alert stands above the table and R keeps
      // its top.
      let failing = true;
      await page.route(`**/api/v1/issues?**`, async (route) => {
        const query = new URL(route.request().url()).searchParams;
        if (failing && query.get("open") === "true" && !query.has("limit")) {
          await route.fulfill({
            body: JSON.stringify({ code: "INTERNAL", error: "The listing did not answer." }),
            contentType: "application/json",
            status: 500,
          });
          return;
        }
        await route.continue();
      });
      await patchIssue(keys[index + 3] ?? "", { title: `Changed below R ${size.width}` }, B);
      await expect(
        page.getByRole("alert").filter({ hasText: "Could not load issues." })
      ).toBeVisible({
        timeout: 15_000,
      });
      await holds("the alert over the table");
      failing = false;
      await page.unroute(`**/api/v1/issues?**`);
      await page.getByRole("button", { name: "Retry loading issues" }).click();
      await expect(
        page.getByRole("alert").filter({ hasText: "Could not load issues." })
      ).toHaveCount(0);

      // The pointer off and the box at its top: a new issue goes above the first row on screen,
      // which keeps its top, and the box's offset grows by the new row's height.
      await handsOff(page);
      await scrollBox(page).evaluate((node) => {
        node.scrollTop = 0;
        window.scrollTo(0, 0);
      });
      const first = await firstOnScreen(page);
      const offset = (await topOfTable(page)).offset;
      const newest = await createIssue(
        { project: ACME, title: size.width === 1440 ? "Copper kettle lid" : "Granite step stone" },
        B
      );
      await patchIssue(newest.key, { status: "todo" }, B);
      await expect.poll(async () => (await rowKeys(table))[0], { message: where }).toBe(newest.key);
      await expect
        .poll(async () => Math.abs((await rowTop(page, first.key)) - first.top))
        .toBeLessThanOrEqual(1);
      const after = await topOfTable(page);
      const height = await page
        .locator(`tr[data-issue-row="${newest.key}"]`)
        .evaluate((row) => row.getBoundingClientRect().height);
      expect(
        Math.abs(after.offset - offset - height),
        `${where}: the box's offset`
      ).toBeLessThanOrEqual(1);
      expect(after.key).toBe(newest.key);
      await expect.poll(() => tabCount(page, "Open")).toBe(count + 1);
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** A Board card by its key. */
const card = (page: Page, key: string) => page.locator(`[data-board-card="${key}"]`);
const cardTop = (page: Page, key: string) =>
  card(page, key).evaluate((node) => node.getBoundingClientRect().top);
const columnCards = (page: Page, id: string) =>
  page
    .locator(`section[data-board-column="${id}"] [data-board-card]`)
    .evaluateAll((cards) => cards.map((one) => one.getAttribute("data-board-card") ?? ""));

// SLOW(>=2s): 16 Todo issues seeded, then at two sizes five writes of another reader with the
// pointer on a card, the focus on one, and the card itself moved.
test("on the Board a card the pointer rests on or the focus holds keeps its top through writes above it, and the window does not follow the card itself to Backlog", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    await seedOpenIssues(16);
    for (const size of SIZES) {
      const where = `at ${size.width}x${size.height}`;
      await page.setViewportSize(size);
      await page.goto(`/projects/${ACME}/issues?view=open&layout=board`);
      await expect(
        page.locator('section[data-board-column="todo"] [data-board-card]').first()
      ).toBeVisible();
      await boardDrawn(page);
      const todo = await columnCards(page, "todo");
      expect(todo.length).toBeGreaterThanOrEqual(12);
      // Todo in view sideways, and the window down until three Todo cards lie above its top.
      await card(page, todo[0] ?? "").scrollIntoViewIfNeeded();
      const fifth = todo[4] ?? "";
      await page.evaluate(async (key) => {
        const node = document.querySelector(`[data-board-card="${key}"]`);
        window.scrollBy(0, (node?.getBoundingClientRect().top ?? 0) - 8);
      }, fifth);
      const t = fifth;
      await card(page, t).hover();
      const top = await cardTop(page, t);
      const backlog = await columnCount(page, "backlog");
      const holds = async (key: string, at: number, what: string) => {
        await expect
          .poll(async () => Math.abs((await cardTop(page, key)) - at), {
            message: `${where}: ${what}`,
          })
          .toBeLessThanOrEqual(1);
      };
      await patchIssue(todo[1] ?? "", { title: `${LONG_TITLE} ${size.width}` }, B);
      await expect(card(page, todo[1] ?? "")).toContainText(LONG_TITLE);
      await holds(t, top, "a title above T changed");
      await patchIssue(todo[2] ?? "", { status: "backlog" }, B);
      await expect.poll(() => columnCount(page, "todo")).toBe(todo.length - 1);
      await expect.poll(() => columnCount(page, "backlog")).toBe(backlog + 1);
      await holds(t, top, "a card above T set to Backlog");

      // The focus on a Todo card F, the pointer off: a title above F changes and F keeps its top.
      const f = todo[6] ?? "";
      await handsOff(page);
      await card(page, f).focus();
      await page.mouse.move(2, 2);
      const fTop = await cardTop(page, f);
      await patchIssue(todo[3] ?? "", { title: `${LONG_TITLE} again ${size.width}` }, B);
      await expect(card(page, todo[3] ?? "")).toContainText("again");
      await holds(f, fTop, "a title above F changed");

      // T itself to Backlog, the pointer on it: the window does not follow, and the Board's box
      // keeps the browser's own anchoring off.
      await handsOff(page);
      await card(page, t).hover();
      const scrolled = await page.evaluate(() => window.scrollY);
      await patchIssue(t, { status: "backlog" }, B);
      await expect(
        page.locator(`section[data-board-column="backlog"] [data-board-card="${t}"]`)
      ).toBeAttached();
      expect(await page.evaluate(() => window.scrollY), `${where}: the window followed T`).toBe(
        scrolled
      );
      expect(
        await page
          .locator(
            '[data-testid="board-lanes-container"], section[aria-label="Project board"] div.overflow-x-auto'
          )
          .first()
          .evaluate((node) => getComputedStyle(node).overflowAnchor),
        `${where}: the Board's box`
      ).toBe("none");
      await page.evaluate(() => window.scrollTo(0, 0));
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The Board column nearest the middle of what the window shows of the Board, of those that
 *  hold a card: the middle the Board's anchor probes (`firstRowOnScreen` in ViewportAnchor.tsx),
 *  the Board's own visible box, which the sidebar narrows, rather than the window's. */
function middleColumn(page: Page): Promise<string> {
  return page.evaluate(() => {
    const board = document.querySelector('section[aria-label="Project board"]');
    const box = board?.getBoundingClientRect();
    const middle =
      box === undefined
        ? window.innerWidth / 2
        : (Math.max(box.left, 0) + Math.min(box.right, window.innerWidth)) / 2;
    const columns = [...document.querySelectorAll("section[data-board-column]")].filter(
      (one) => one.querySelector("[data-board-card]") !== null
    );
    const centre = (one: Element) =>
      one.getBoundingClientRect().left + one.getBoundingClientRect().width / 2;
    const nearest = columns.sort(
      (a, b) => Math.abs(centre(a) - middle) - Math.abs(centre(b) - middle)
    )[0];
    return nearest?.getAttribute("data-board-column") ?? "";
  });
}

// SLOW(>=2s): 24 issues seeded in three statuses, then at two sizes two cards written to the top
// of a column, the second after the window went down and back.
test("on the Board at the window's top with no hand a card written to the top of the middle column goes above its first card, which keeps its top, also after the window went down and back", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    await seedOpenIssues(8, "todo");
    await seedOpenIssues(8, "in_progress");
    // Triage holds cards too. On a phone the Board opens on its first column, Triage, which is
    // then the column nearest the middle of the window, and the anchor holds only a card that is
    // on screen. A new issue starts in Triage.
    for (let n = 1; n <= 8; n++) await createIssue({ project: ACME, title: `Triage work ${n}` });
    for (const size of SIZES) {
      const where = `at ${size.width}x${size.height}`;
      await page.setViewportSize(size);
      await page.goto(`/projects/${ACME}/issues?view=open&sort=-created&layout=board`);
      await expect(page.locator("[data-board-card]").first()).toBeVisible();
      await boardDrawn(page);
      await page.evaluate(() => window.scrollTo(0, 0));
      await handsOff(page);
      const column = await middleColumn(page);
      // middleColumn picked a column that holds a card, and the page holds every later board
      // frame, so the column still holds it.
      const [firstCard] = await columnCards(page, column);
      if (firstCard === undefined) throw new Error(`${where}: the middle column is empty`);
      await expect(
        card(page, firstCard),
        `${where}: the middle column's first card`
      ).toBeInViewport();
      const top = await cardTop(page, firstCard);
      const count = await columnCount(page, column);
      const tab = (await tabCount(page, "Open")) ?? 0;
      const writeAbove = async (above: string, title: string) => {
        const made = await createIssue({ project: ACME, title }, B);
        await patchIssue(made.key, { rank: { before: above }, status: column }, B);
        return made.key;
      };
      const made = await writeAbove(
        firstCard,
        size.width === 1440 ? "Gizmo crate label" : "Harbor buoy chain"
      );
      await expect
        .poll(async () => (await columnCards(page, column))[0], { message: where })
        .toBe(made);
      await expect
        .poll(async () => Math.abs((await cardTop(page, firstCard)) - top))
        .toBeLessThanOrEqual(1);
      await expect.poll(() => columnCount(page, column)).toBe(count + 1);
      await expect.poll(() => tabCount(page, "Open")).toBe(tab + 1);

      // The window down until three cards lie above its top, a card in view changed, and back.
      const cards = await columnCards(page, column);
      await page.evaluate((key) => {
        const node = document.querySelector(`[data-board-card="${key}"]`);
        window.scrollBy(0, (node?.getBoundingClientRect().top ?? 0) - 8);
      }, cards[3] ?? "");
      await patchIssue(cards[4] ?? "", { title: `${LONG_TITLE} ${size.width}` }, B);
      await expect(card(page, cards[4] ?? "")).toContainText(LONG_TITLE);
      await page.evaluate(() => window.scrollTo(0, 0));
      await handsOff(page);
      const [head] = await columnCards(page, column);
      const headTop = await cardTop(page, head ?? "");
      const second = await writeAbove(
        head ?? "",
        size.width === 1440 ? "Sprocket audit sheet" : "Meadow fence post"
      );
      await expect.poll(async () => (await columnCards(page, column))[0]).toBe(second);
      await expect
        .poll(async () => Math.abs((await cardTop(page, head ?? "")) - headTop), { message: where })
        .toBeLessThanOrEqual(1);
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): 30 priorities and 30 issues under one seeded, then a priority written first on
// /priorities and an issue written first on a priority's page.
test("on /priorities and on a priority's page, at the box's top with no hand, a row written first goes above the first row, which keeps its top", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const context = await asUser(browser, "alice");
  try {
    await createProject({ key: "CORE", name: "Core" });
    const ids: string[] = [];
    for (let n = 1; n <= 30; n++)
      ids.push((await createPriority({ owner: "alice", title: `Priority ${n}` })).id);
    const work: string[] = [];
    for (let n = 1; n <= 30; n++) {
      const issue = await createIssue({ project: "CORE", title: `Work under it ${n}` });
      await patchIssue(issue.key, { advances: [ids[0] ?? ""], status: "todo" });
      work.push(issue.key);
    }
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/priorities");
    const table = page.getByRole("table", { name: "Priorities table" });
    await expect(table.locator("tr[data-priority-row]").first()).toBeVisible();
    const box = table.locator("xpath=ancestor::*[@data-control='data-table'][1]");
    await handsOff(page);
    const firstRow = table.locator("tr[data-priority-row]").first();
    const firstId = (await firstRow.getAttribute("data-priority-row")) ?? "";
    const top = await firstRow.evaluate((row) => row.getBoundingClientRect().top);
    const offset = await box.evaluate((node) => node.scrollTop);
    const made = await createPriority({ owner: "bob", title: "Written first" }, B);
    await patchPriority(made.id, { rank: { before: firstId } }, B);
    await expect
      .poll(() => table.locator("tr[data-priority-row]").first().getAttribute("data-priority-row"))
      .toBe(made.id);
    await expect
      .poll(() =>
        table
          .locator(`tr[data-priority-row="${firstId}"]`)
          .evaluate((row, at) => Math.abs(row.getBoundingClientRect().top - at), top)
      )
      .toBeLessThanOrEqual(1);
    const height = await table
      .locator(`tr[data-priority-row="${made.id}"]`)
      .evaluate((row) => row.getBoundingClientRect().height);
    expect(
      Math.abs((await box.evaluate((node) => node.scrollTop)) - offset - height)
    ).toBeLessThanOrEqual(1);

    // A priority's page: an issue written first in its table.
    await page.goto(`/priorities/${ids[0]}`);
    const own = page.getByRole("table", { name: "Priority issues table" });
    await expect(own.locator("tr[data-issue-row]").first()).toBeVisible();
    const ownBox = own.locator("xpath=ancestor::*[@data-control='data-table'][1]");
    await handsOff(page);
    const firstIssue =
      (await own.locator("tr[data-issue-row]").first().getAttribute("data-issue-row")) ?? "";
    const issueTop = await rowTop(page, firstIssue);
    const ownOffset = await ownBox.evaluate((node) => node.scrollTop);
    const head = await getIssue(firstIssue);
    const written = await createIssue({ project: "CORE", title: "Written first under it" }, B);
    await patchIssue(
      written.key,
      {
        advances: [ids[0] ?? ""],
        labels: head.labels ?? [],
        rank: { before: firstIssue },
        status: head.status,
      },
      B
    );
    await expect
      .poll(() => own.locator("tr[data-issue-row]").first().getAttribute("data-issue-row"))
      .toBe(written.key);
    await expect
      .poll(async () => Math.abs((await rowTop(page, firstIssue)) - issueTop))
      .toBeLessThanOrEqual(1);
    const rowHeight = await page
      .locator(`tr[data-issue-row="${written.key}"]`)
      .evaluate((row) => row.getBoundingClientRect().height);
    expect(
      Math.abs((await ownBox.evaluate((node) => node.scrollTop)) - ownOffset - rowHeight)
    ).toBeLessThanOrEqual(1);
  } finally {
    await context.close();
  }
});

test("on /delivery a new merge's row is the first row and in view at the list's top, and the row that was first moves down, as on main", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  await sql(
    `UPDATE delivery_settings SET last_event_at = now(), last_reconcile_at = now() WHERE singleton`,
    // The fixture's merges are in 2024, outside the list's window: one merge of a few minutes
    // ago is the first row the list opens with.
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES (
       'acme/widgets', 694, 'feat: the merge that was first', 'https://github.com/acme/widgets/pull/694', 'octocat',
       now() - interval '10 minutes', now() - interval '5 minutes', 2, 1, false, null, ARRAY[]::text[], false
     )`
  );
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/delivery?mode=list");
    await expect(page.getByRole("heading", { name: "Delivery timeline" })).toBeVisible();
    const rows = page.locator("[data-pr-row]");
    await expect(rows.first()).toBeVisible();
    await handsOff(page);
    const firstTitle = (await rows.first().locator("[data-pr-open]").getAttribute("title")) ?? "";
    const firstTop = await rows.first().evaluate((row) => row.getBoundingClientRect().top);
    const title = "feat: a merge read at the top";
    await sql(
      `INSERT INTO delivery_pull_requests (
         repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
       ) VALUES (
         'acme/widgets', 695, '${title}', 'https://github.com/acme/widgets/pull/695', 'octocat',
         now() - interval '1 minute', now(), 3, 1, false, null, ARRAY[]::text[], false
       )`
    );
    await expect(rows.first()).toContainText(title);
    await expect(rows.first()).toBeInViewport();
    const moved = rows.filter({ hasText: firstTitle.slice(0, 40) }).first();
    expect(await moved.evaluate((row) => row.getBoundingClientRect().top)).toBeGreaterThan(
      firstTop
    );
  } finally {
    await context.close();
  }
});

/** A page that counts its loads. */
async function countedPage(context: BrowserContext, address: string) {
  const page = await context.newPage();
  const loads = { count: 0 };
  page.on("load", () => {
    loads.count += 1;
  });
  await page.setViewportSize({ height: 1200, width: 1440 });
  await page.goto(address);
  return { page, loads };
}

/** The Work time cell of `key`'s row in the table of `page`. */
function workText(page: Page, key: string): Promise<string> {
  return issuesTable(page).evaluate((table, row) => {
    const index = [...table.querySelectorAll("thead th")].findIndex(
      (cell) =>
        (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, "").trim() ===
        "Work time"
    );
    const drawn = table.querySelector(`tr[data-issue-row="${row}"]`);
    return (drawn?.children[index]?.textContent ?? "").trim();
  }, key);
}

/** WIDGETS's first card, an open issue in Implementing, with an implementer's role event of 70
 *  minutes ago and its facts row, the project's daemon named at `daemonUrl`; a table page and a
 *  Board page of A open on it. */
async function factsWrite(context: BrowserContext, daemonUrl: string) {
  const fixture = await seedLegionFactsFixture(daemonUrl);
  const key = fixture.keys[0] ?? "";
  expect(cardsOf(await getLegionBoard(WIDGETS)).get(key)?.column).toBe("implementing");
  await seedFailedLegionRead(WIDGETS, "the daemon did not answer its state route");
  await writeRoleEvent(key, "implementer", "70 minutes");
  await waitForIssueFacts([key]);
  const table = await countedPage(
    context,
    `/projects/${WIDGETS}/issues?view=open&cols=key,title,work_hours`
  );
  const board = await countedPage(
    context,
    `/projects/${WIDGETS}/issues?view=open&layout=board&group=legion_phase`
  );
  return { key, table, board };
}

/** The Work time a card draws, as its words. */
const cardWork = (page: Page, key: string) =>
  card(page, key).locator('[data-legion-fact="work-time"]').textContent();

// SLOW(>=2s): WIDGETS seeded and the facts consumer's row of its first card, up to 75 s, then two
// pages read.
test("a facts write reaches an open table and an open Board: the issue's Work time reads about an hour in its cell and on its card", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await startFakeLegionDaemon();
  const context = await asUser(browser, "alice");
  try {
    const { key, table, board } = await factsWrite(context, daemon.url);
    await expect.poll(() => workText(table.page, key)).toMatch(/^1 h \d+ m$/);
    await expect
      .poll(async () => ((await cardWork(board.page, key)) ?? "").trim())
      .toMatch(/^Work time ?1 h \d+ m$/);
    expect([table.loads.count, board.loads.count]).toEqual([1, 1]);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): WIDGETS seeded and two facts consumer's rows of its first card, up to 75 s each,
// then two pages read with no reload.
test("a second role event's facts write moves the open table's and the open Board's Work time to about nine hours with no reload, and wakes the board through delivery_changes", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const daemon = await startFakeLegionDaemon();
  const context = await asUser(browser, "alice");
  try {
    const { key, table, board } = await factsWrite(context, daemon.url);
    await expect.poll(() => workText(table.page, key)).toMatch(/^1 h \d+ m$/);
    const before = (
      await sql(
        `SELECT coalesce(max(seq), 0) FROM delivery_changes WHERE kind = 'issue_facts' AND ref = '${key}'`
      )
    ).trim();
    await writeRoleEvent(key, "architect", "9 hours");
    // The facts row first, as the server stores it, then the pages. The row is closed at the
    // issue's newest input, the claim the fixture saw 30 minutes before the seed
    // (`metrics.ProjectFactsClosed`), so it stores nine hours less that half hour and the time
    // since the seed, a little under 8.5, and a reader grows it to its own clock.
    await expect
      .poll(
        async () =>
          Number(
            (await sql(`SELECT work_hours FROM issue_facts WHERE issue_key = '${key}'`)).trim()
          ),
        { message: "the facts row's work hours after the architect's event", timeout: 75_000 }
      )
      .toBeGreaterThan(8);
    await expect
      .poll(() => workText(table.page, key), {
        timeout: 75_000,
      })
      .toMatch(/^(?:8|9) h \d+ m$/);
    await expect
      .poll(async () => ((await cardWork(board.page, key)) ?? "").trim(), { timeout: 30_000 })
      .toMatch(/^Work time ?(?:8|9) h \d+ m$/);
    expect([table.loads.count, board.loads.count]).toEqual([1, 1]);
    const newest = (
      await sql(
        `SELECT kind || ' ' || repo || ' ' || ref FROM delivery_changes WHERE kind = 'issue_facts' AND ref = '${key}' AND seq > ${before} ORDER BY seq DESC LIMIT 1`
      )
    ).trim();
    expect(newest).toBe(`issue_facts ${WIDGETS} ${key}`);
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The items route of `legion-work-time` with row_kind=issue over the two days from the start of
 *  the day that holds `at`, so over a window that holds `at`. */
function issueItems(at: number) {
  const day = Math.floor(at / 86_400_000) * 86_400_000;
  return getLegionWorkTimeItems(WIDGETS, {
    from: new Date(day).toISOString(),
    limit: 200,
    row_kind: "issue",
    to: new Date(day + 2 * 86_400_000).toISOString(),
  });
}

/** An issue of WIDGETS with no parent, no child and no phase observation, so no observation names
 *  it and the board never shows it, with an implementer's role event of 70 minutes ago and its
 *  facts row. Once that row is written, the items route of `legion-work-time` is read over a
 *  window that holds now, and the issue is closed at once. `page`'s first read of the items route
 *  after the close is answered from that earlier read, which lacks the issue, so only a later
 *  read can show it: the page's read 60 s after that answer (`CLOSE_ITEMS_AGAIN_MS`). */
async function closeUnseen(page: Page, title: string): Promise<{ key: string; closedAt: number }> {
  let earlier: EvalItemsResponse | undefined;
  await page.route(
    `**/api/v1/projects/${WIDGETS}/evals/${legionWorkTimeEval}/items?**`,
    (route) => {
      const json = earlier;
      if (json === undefined) return route.fallback();
      earlier = undefined;
      return route.fulfill({ json });
    }
  );
  const issue = await createIssue({ project: WIDGETS, title }, B);
  await patchIssue(issue.key, { status: "in_progress" }, B);
  // Made two hours ago, so a role event of 70 minutes ago falls in its life.
  await sql(`UPDATE issues SET created_at = now() - interval '2 hours' WHERE key = '${issue.key}'`);
  await writeRoleEvent(issue.key, "implementer", "70 minutes");
  await waitForIssueFacts([issue.key]);
  earlier = await issueItems(Date.now());
  await patchIssue(issue.key, { status: "done" }, B);
  return { key: issue.key, closedAt: Date.now() };
}

/** Waits 135 s from the close, then reads `key`'s row in the items route over a window that holds
 *  the close and its Work time cell on `page`, which equals that row's `work_hours`. */
async function workFromItems(page: Page, key: string, closedAt: number): Promise<void> {
  // The span the check gives: 75 s for a facts consumer pass to store the close, then the page's
  // 60 s before it reads the items again. A read before then cannot show that the page read again.
  await page.waitForTimeout(Math.max(0, closedAt + 135_000 - Date.now()));
  const row = ((await issueItems(closedAt)).items ?? []).find((one) => one.row_key === key);
  expect(row, `the items route's row of ${key}`).toBeDefined();
  expect(await workText(page, key)).toBe(workWords(Number(row?.cols?.work_hours)));
}

/** Wakes a pass of the eval refresher, the pass that gives WIDGETS its eval. The fixture writes
 *  WIDGETS by SQL, which wakes no pass (`waitForLegionWorkTimeEval`), and an event that commits
 *  wakes one: a role event of WIDGETS-1, an issue neither of the two rows below lists. */
const wakeEvalRefresher = () => writeRoleEvent(`${WIDGETS}-1`, "implementer", "70 minutes");

// SLOW(>=2s): the eval refresher's pass for WIDGETS, a facts consumer's row, up to 75 s, then 135 s
// from a close before the open Done page's Work time is read.
test("the close of an issue the board never shows fills its Work time on an open Done with no reload and no other event", async ({
  browser,
}) => {
  // Up to 60 s for the eval, up to 75 s for the facts row, 135 s from the close, and the seed.
  test.setTimeout(330_000);
  const daemon = await startFakeLegionDaemon();
  const context = await asUser(browser, "alice");
  try {
    await seedLegionFactsFixture(daemon.url);
    await wakeEvalRefresher();
    await waitForLegionWorkTimeEval(WIDGETS);
    const done = await countedPage(
      context,
      `/projects/${WIDGETS}/issues?view=done&cols=key,title,closed,work_hours`
    );
    const { key, closedAt } = await closeUnseen(done.page, "Closed out of the board's sight");
    await expect(issuesTable(done.page).locator(`tr[data-issue-row="${key}"]`)).toBeVisible();
    await workFromItems(done.page, key, closedAt);
    expect(done.loads.count).toBe(1);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): the eval refresher's pass for WIDGETS, a facts consumer's row, up to 75 s, then 135 s
// from a close before the Work time of an open page of Open with status=done is read.
test("the close of an issue the board never shows fills its Work time on an open Open with status=done with no reload and no other event", async ({
  browser,
}) => {
  // Up to 60 s for the eval, up to 75 s for the facts row, 135 s from the close, and the seed.
  test.setTimeout(330_000);
  const daemon = await startFakeLegionDaemon();
  const context = await asUser(browser, "alice");
  try {
    await seedLegionFactsFixture(daemon.url);
    await wakeEvalRefresher();
    await waitForLegionWorkTimeEval(WIDGETS);
    const open = await countedPage(
      context,
      `/projects/${WIDGETS}/issues?view=open&status=done&cols=key,title,closed,work_hours`
    );
    const { key, closedAt } = await closeUnseen(open.page, "Closed out of sight on Open");
    await expect(issuesTable(open.page).locator(`tr[data-issue-row="${key}"]`)).toBeVisible();
    await workFromItems(open.page, key, closedAt);
    expect(open.loads.count).toBe(1);
  } finally {
    await context.close();
    await daemon.close();
  }
});
