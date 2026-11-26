/** The priority, read and written in a browser against the real stack: the list page and its two
 *  fault lists, a priority's own page and its margin, the line an issue's header carries, and the
 *  rail's count. Five checks of dispatch://INFRA-2947 are this one file - the page's order, the
 *  writes a person makes, the margin, the issue's line and the two absences - so a row that fails
 *  fails all five, and each row below is one of their sentences.
 *
 *  The pages' own components are read by `web/src/features/priorities/*.test.tsx`; what is here is
 *  what only a browser against a server can say: that a write lands in the store and comes back
 *  after a reload, that a count moves with no reload at all, and that two pages agree about one
 *  relation.
 */
import { readFileSync } from "node:fs";

import { PRIORITY_ID_MAX } from "@legion/contracts";

import { SEND_DEADLINE_MS } from "../web/src/features/writes/keyed-write";

import {
  claimIssue,
  createIssue,
  createPriority,
  createProject,
  dashboardOrigin,
  endPriority,
  getIssue,
  getPriorityHistory,
  listPriorities,
  patchIssue,
  patchPriority,
} from "./api";
import { actionBar, placeCaretAtEnd } from "./editor";
import { pendingWrites } from "./keyed-writes";
import { pauseClock } from "./page-clock";
import { settledValue } from "./settled";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

const PRIORITIES_PATH = "/priorities";

/** One case of the rule by which the server makes a priority's id from a title the create
 *  names no id beside: the title, and the id it gives. */
interface IdCase {
  readonly title: string;
  readonly id: string;
}

/** The cases of that rule, written once for the server's tests and the row below
 *  (`packages/contracts/fixtures/priority-ids.json`). The file is read by its path rather than
 *  imported, so it stays out of this package's TypeScript program. */
const idFixture: { readonly cases: readonly IdCase[] } = JSON.parse(
  readFileSync(new URL("../../contracts/fixtures/priority-ids.json", import.meta.url), "utf8")
);

/** An id the server makes from a title's hash, where the title gives no slug it can use
 *  (`priority-` and the first 12 hex characters of the title's SHA-256). */
const HASHED_ID = /^priority-[0-9a-f]{12}$/;

/** The case of that list with the longest title whose id is its slug. Its slug is longer than
 *  `PRIORITY_ID_MAX`, so the id the server derives from it is the slug cut at a word's end. A
 *  case whose id is made from the title's hash is no slug cut. */
const LONG_CASE = idFixture.cases
  .filter((one) => !HASHED_ID.test(one.id))
  .reduce((longest, one) => (one.title.length > longest.title.length ? one : longest));

/** The two fault tables, by the names the Issues table gives them: the `label` each fault list of
 *  `PrioritiesPage.tsx` passes, which `features/project/IssuesTable.tsx` names its table by with
 *  " table" after it. */
const NO_PRIORITY_TABLE = "Active under no priority table";
const BACKLOG_TABLE = "Priority work in backlog or icebox table";

/** What the server says of a deployment with no live priority, in its own words
 *  (`priorityNoneWritten`, `internal/dispatch/api/priority_queries.go`): the empty page draws the
 *  sentence the read answers rather than a sentence of its own. */
const NONE_LIVE =
  "No priority is live now, so the list is empty. Write one, then say which work advances it.";

/** The columns each fault list opens with, by their headers (INFRA-2947, R6): the issue, its
 *  title, its status, who holds it, how long it has stood and its project; and on the backlog-or-icebox
 *  list when it was moved there and by whom, beside the priority it stands under. There the
 *  move is how long the work has stood, so Updated opens hidden, one tick away under Columns.
 *  Every other column of the Issues table opens hidden, for a reader to show from its column
 *  control; no list has a Reason column until a reason is recorded (dispatch://INFRA-2934). */
const FAULT_COLUMNS = ["Key", "Title", "Status", "Claimed by", "Updated", "Project"] as const;
const BACKLOG_COLUMNS = ["Advances", "Moved there"] as const;
const HIDDEN_COLUMNS = [
  "Priority",
  "Queue",
  "Epic",
  "Assignee",
  "Created by",
  "Labels",
  "Open asks",
  "Created",
  "Advances",
] as const;

/** The corpus every row seeds: three priorities in one order, work under each in three
 *  projects, an issue in each of the four in-flight statuses that advances none, an issue that
 *  advances one through its parent and stands in the backlog, and one whose own row names it
 *  that stands there too. */
interface Seeded {
  /** The three priority ids, in rank order: `#1`, `#2`, `#3` on the page. */
  readonly ids: readonly [string, string, string];
  readonly titles: readonly [string, string, string];
  /** The description `#1` carries, several paragraphs of it. */
  readonly description: string;
  /** `#1`'s work: one issue in flight and one waiting. */
  readonly rollout: string;
  readonly smoke: string;
  /** `#2`'s one issue, in `testing`. */
  readonly queue: string;
  /** `#3`'s work: an issue in `needs_review`, the parent that states the relation, the child
   *  in `backlog` that inherits it, and the issue in `backlog` that states it on its own row -
   *  the one priority work that stands in backlog or icebox (`own`). */
  readonly ledger: string;
  readonly cutover: string;
  readonly child: string;
  readonly archive: string;
  /** Two of the issues in flight under no priority this file writes to. */
  readonly clock: string;
  readonly lease: string;
}

const FIRST_DESCRIPTION =
  "Every machine of the fleet runs the daemon, or the rollout is not done.\n\n" +
  "The work under this priority is the rollout itself, the smoke suite that proves a machine " +
  "took the build, and the roll-back a machine that refused it needs.\n\n" +
  "It is finished when no machine is left on the old build and nothing is waiting on one.";

/** A description that nests emphasis 4,000 deep, 24,001 characters: the text #4835 measured at
 *  about 55 s of the browser's parser, which the server reads as its own characters. */
const NESTED = `${"*a ".repeat(4_000)}b${" c*".repeat(4_000)}`;

async function seedPriorities(): Promise<Seeded> {
  await Promise.all([
    createProject({ key: "CORE", name: "Core" }),
    createProject({ key: "OPS", name: "Ops" }),
    createProject({ key: "WEB", name: "Web" }),
  ]);

  // Created one after another, because a priority takes the last place of the one order as it
  // is written (`POST /api/v1/priorities`), and these three are read as #1, #2 and #3.
  const first = await createPriority({
    description: FIRST_DESCRIPTION,
    owner: "alice",
    title: "Ship the daemon everywhere",
  });
  const second = await createPriority({ owner: "bob", title: "Cut the review queue" });
  const third = await createPriority({ owner: "alice", title: "Retire the old ledger" });

  const rollout = await createIssue({ project: "CORE", title: "Daemon rollout to the fleet" });
  const smoke = await createIssue({ project: "CORE", title: "Daemon smoke suite" });
  const queue = await createIssue({ project: "OPS", title: "Review queue triage" });
  const ledger = await createIssue({ project: "WEB", title: "Ledger read path" });
  const cutover = await createIssue({ project: "WEB", title: "Ledger cut-over" });
  // The child states nothing of its own: it advances `#3` because the issue above it does
  // (INFRA-2947, R4), which is what the issue page's inherited entry reads. Standing in the
  // backlog, it is not on the backlog-or-icebox list, which holds the work whose own row names
  // a priority, which the archive below is.
  const child = await createIssue({
    parent: cutover.key,
    project: "WEB",
    title: "Ledger client migration",
  });
  const archive = await createIssue({ project: "WEB", title: "Ledger archive" });
  // One issue in each of the four statuses the first fault is about, spread over the three
  // projects, so the count line under its rule names more than one board.
  const clock = await createIssue({ project: "CORE", title: "Audit the clock" });
  const lease = await createIssue({ project: "CORE", title: "Lease handover" });
  const mirror = await createIssue({ project: "OPS", title: "Mirror retries" });
  const sweep = await createIssue({ project: "WEB", title: "Phone layout sweep" });

  // One write per issue: a status, and the one relation it states. A write naming no revision
  // asserts one relation at a time, which is what each of these is (`api/issue_advances.go`).
  await Promise.all([
    patchIssue(rollout.key, { advances: [first.id], status: "in_progress" }),
    patchIssue(smoke.key, { advances: [first.id], status: "todo" }),
    patchIssue(queue.key, { advances: [second.id], status: "testing" }),
    patchIssue(ledger.key, { advances: [third.id], status: "needs_review" }),
    patchIssue(cutover.key, { advances: [third.id], status: "todo" }),
    patchIssue(child.key, { status: "backlog" }),
    patchIssue(archive.key, { advances: [third.id], status: "backlog" }),
    patchIssue(clock.key, { status: "in_progress" }),
    patchIssue(lease.key, { status: "retro" }),
    patchIssue(mirror.key, { status: "testing" }),
    patchIssue(sweep.key, { status: "needs_review" }),
  ]);
  // One of the two fault rows read cell by cell below is claimed and the other is not, while
  // both are assigned to alice: "Claimed by" names the claim and nothing else, as the issues
  // table of this application reads it (`features/project/issues-table-columns.tsx`).
  await claimIssue(clock.key, { as: "user", login: "alice" });

  return {
    archive: archive.key,
    child: child.key,
    clock: clock.key,
    cutover: cutover.key,
    description: FIRST_DESCRIPTION,
    ids: [first.id, second.id, third.id],
    ledger: ledger.key,
    lease: lease.key,
    queue: queue.key,
    rollout: rollout.key,
    smoke: smoke.key,
    titles: [first.title, second.title, third.title],
  };
}

/** A page of alice's at the desktop size the page is measured at (1440 by 900 px, INFRA-2947
 *  R7), which is wider than the width the sidebar's rail is drawn from (1280 px,
 *  `WIDE_LAYOUT_MIN_PX` in `e2e/review-routes.ts`), so three rows below read the Priorities
 *  entry from it. */
async function priorityPage(page: Page): Promise<void> {
  await page.setViewportSize(WIDE);
}

/** The desktop `priorityPage` sets (1440 by 900 px, INFRA-2947 R7). */
const WIDE = { height: 900, width: 1440 } as const;

/** The phone the page is measured on (390 by 844 px, INFRA-2947 R7). */
const PHONE = { height: 844, width: 390 } as const;

/** The desktop the visual review reads the pages at (1280 by 800 px), from which up no table of
 *  the priorities page's three tabs or of a priority's page scrolls sideways. */
const DESKTOP = { height: 800, width: 1280 } as const;

/** The widest the backlog-or-icebox list's Advances column stands at 1280 px: the chip and the priority's
 *  title wrapping after it, while Title takes the room every other column leaves. */
const BACKLOG_ADVANCES_CAP = 180;

const section = (page: Page, label: string): Locator =>
  page.locator(`section[aria-label="${label}"]`);

/** A priority's title on its own page: the heading of the page's header, which the description,
 *  drawn as the markdown it is, may hold first-level headings beside. */
const titleHeading = (page: Page): Locator =>
  page.getByTestId("priority-header").getByRole("heading", { level: 1 });

/** The box a priority's title is written in, which a click on its heading opens, as the reader
 *  opens it. */
async function openTitle(page: Page): Promise<Locator> {
  await titleHeading(page).click();
  return page.getByRole("textbox", { name: "Title" });
}

/** Where a section's box starts down the page, which is what says one part of a page stands
 *  above or below another, or has moved. Read once the section shows. */
async function topOf(locator: Locator): Promise<number> {
  await expect(locator).toBeVisible();
  // The wait above drew the section, and a drawn element has a box, so the box is there.
  const box = await locator.boundingBox();
  if (box === null) throw new Error("the section this row reads is not on the page");
  return box.y;
}

/** The page's three tabs, by the names their buttons read, which end in the count each holds:
 *  `Priorities 3`. Each is written into the address as `?tab=` with its key. */
const TABS = {
  order: /^Priorities \d+$/,
  none: /^Active under no priority \d+$/,
  backlog: /^Priority work in backlog or icebox \d+$/,
} as const;

/** Chooses one of the page's tabs, and waits for it to be selected and for the address to name
 *  it. A click on the tab already chosen writes nothing, so the Priorities tab, which the page
 *  opens on while the address names no tab, may also stand with no `tab=` in the address. */
async function chooseTab(page: Page, tab: keyof typeof TABS): Promise<void> {
  const control = page.getByRole("tab", { name: TABS[tab] });
  await control.click();
  await expect(control).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(
    (url) =>
      url.searchParams.get("tab") === tab || (tab === "order" && !url.searchParams.has("tab"))
  );
}

/** How far below the window's bottom edge the pager under a tab's table ends: at or above it,
 *  where that table is the last thing on the page and its box takes the window rather than
 *  standing under another table below the fold. */
function pagerBelowWindow(page: Page): Promise<number> {
  return page
    .getByTestId("issues-table-pager")
    .evaluate((pager) => pager.getBoundingClientRect().bottom - window.innerHeight);
}

/** How much wider than its box each table the page draws is, by the table's name: nothing,
 *  where every shown column fits the box and no scrollbar runs along its bottom edge. The box is
 *  the one each table scrolls in, named by its component's own test identifier
 *  (`priorities-table-scroll`, `issues-table-scroll`). */
function tablesOverflow(page: Page): Promise<Record<string, number>> {
  return page.locator("table").evaluateAll((tables) =>
    Object.fromEntries(
      tables.map((table) => {
        const box = table.closest('[data-testid$="-table-scroll"]');
        if (box === null) throw new Error("a table on the page stands in no scroll box");
        return [table.getAttribute("aria-label") ?? "", box.scrollWidth - box.clientWidth];
      })
    )
  );
}

/** How much wider than the window the page's document is: nothing, where the page itself does
 *  not scroll sideways. */
function documentOverflow(page: Page): Promise<number> {
  return page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
}

/** The headers of a table whose words its column cuts: a header's sort control that holds more
 *  than its box shows. A header whose words wrap onto a second line shows them whole. */
function cutHeaders(table: Locator): Promise<string[]> {
  return table
    .locator('thead th button[title^="Sort"]')
    .evaluateAll((buttons) =>
      buttons
        .filter((button) => button.scrollWidth > button.clientWidth)
        .map((button) => button.textContent ?? "")
    );
}

/** Where the text an element holds ends at the right, whatever box the element itself takes. */
function textRight(locator: Locator): Promise<number> {
  return locator.evaluate((element) => {
    const range = document.createRange();
    range.selectNodeContents(element);
    return range.getBoundingClientRect().right;
  });
}

/** That the page as drawn says no date anything is wanted by, no portion of it done and no word
 *  judging how it goes: every `<time>` is a moment that has passed, and no relative label reads
 *  forwards. Read once the caller has waited for what the page draws, so one read is enough. */
async function expectNoForecast(page: Page): Promise<void> {
  const future = await page
    .locator("time[datetime]")
    .evaluateAll(
      (nodes, now) =>
        nodes
          .map((node) => node.getAttribute("datetime") ?? "")
          .filter((at) => Date.parse(at) > now),
      Date.now()
    );
  expect(future).toEqual([]);
  const drawn = await page.locator("body").innerText();
  expect(drawn).not.toMatch(/\bin \d+ (second|minute|hour|day|week|month|year)s?\b/);
  expect(drawn).not.toMatch(/\d\s?%/);
  expect(drawn).not.toMatch(/\b(on track|at risk|off track|behind schedule|ahead of schedule)\b/i);
}

/** Every row of a table, by the name a reader hears: `#1 <title>` on the priorities table and
 *  `<KEY> <title>` on a fault list. */
function rowNames(scope: Locator): Promise<string[]> {
  return scope
    .locator("tr[data-issue-row]")
    .evaluateAll((rows) => rows.map((row) => row.getAttribute("aria-label") ?? ""));
}

/** The rows a table holds, whatever order it draws them in, once the read behind it has
 *  answered: a table a navigation has just reached holds no row yet, and the rows of a fault
 *  list or of the work under a priority are a set rather than an order - the order each table
 *  opens in is its own and is read where a row is about it. */
async function expectRowNames(scope: Locator, names: readonly string[]): Promise<void> {
  await expect.poll(async () => (await rowNames(scope)).sort()).toEqual([...names].sort());
}

/** The rows a table holds in the order it draws them, which on the priorities table is the one
 *  order a reader drags and so is the claim itself. */
async function expectRowOrder(scope: Locator, names: readonly string[]): Promise<void> {
  await expect.poll(() => rowNames(scope)).toEqual([...names]);
}

/** Where a column stands in a table, by the text its header starts with: a reader drags the
 *  columns of the Issues table, so a column's place is the reader's, and its header is what it
 *  is. Its filter control follows the header's name in the same cell, so the name is the
 *  header's start, as `PrioritiesPage.test.tsx` reads it. */
async function columnIndex(table: Locator, header: string): Promise<number> {
  const headers = await table.getByRole("columnheader").allTextContents();
  const at = headers.findIndex((text) => text.trim().startsWith(header));
  if (at === -1) throw new Error(`the table draws no ${header} column`);
  return at;
}

/** A row's cell under the column whose header is `header`. */
async function cellUnder(table: Locator, row: Locator, header: string): Promise<Locator> {
  return row.getByRole("cell").nth(await columnIndex(table, header));
}

/** The text of one column of every row of a table, which is how a row reads the cells R6 names
 *  without spelling each row out. */
async function columnTexts(table: Locator, header: string): Promise<string[]> {
  const at = await columnIndex(table, header);
  return table
    .locator("tr[data-issue-row]")
    .evaluateAll(
      (rows, index) =>
        rows.map((row) => row.querySelectorAll("td")[index]?.textContent?.trim() ?? ""),
      at
    );
}

/** That a table draws the columns `shown` and none of `hidden`, read off its headers, each of
 *  which starts with its column's name. */
async function expectColumns(
  table: Locator,
  shown: readonly string[],
  hidden: readonly string[]
): Promise<void> {
  const headers = (await table.getByRole("columnheader").allTextContents()).map((text) =>
    text.trim()
  );
  for (const name of shown)
    expect(
      headers.some((text) => text.startsWith(name)),
      name
    ).toBe(true);
  for (const name of hidden)
    expect(
      headers.some((text) => text.startsWith(name)),
      name
    ).toBe(false);
}

/** Chooses a fault's tab and reloads the page, which opens on the tab the address kept. */
async function chooseTabAndReload(page: Page, tab: "none" | "backlog"): Promise<void> {
  await chooseTab(page, tab);
  await page.reload();
  await expect(page).toHaveURL(new RegExp(`[?&]tab=${tab}(&|$)`));
  await expect(page.getByRole("tab", { name: TABS[tab] })).toHaveAttribute("aria-selected", "true");
}

/** The badge beside the rail's Priorities entry, which is its last span; an entry with no count
 *  draws two spans and no third. */
const railEntry = (page: Page): Locator => page.getByRole("link", { name: /^Priorities/ });

/** The priorities an issue advances in its header from a tablet's width up: the chips and the
 *  picker's "+", which stand in the header's state row, the row of the status, the P0-P3 tag and
 *  the assignee. On a phone they open the details row instead. */
const chipsLine = (page: Page): Locator =>
  page.getByTestId("issue-header").getByTestId("issue-state-actions").getByTestId("issue-advances");

// SLOW(>=2s): three projects, three priorities and ten issues, then the whole page read cell by
// cell, on a desktop and on a phone, and two reloads.
test("the priorities page opens on the order, and each fault list is a tab of its own", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(PRIORITIES_PATH);
    const main = page.getByTestId("main-content");

    // The page is its heading and three tabs, and nothing under the heading: no subtitle, and
    // no heading over any table. The order is the tab an address naming none opens, and it
    // draws its one table.
    await expect(main.getByRole("heading", { level: 1, name: "Priorities" })).toBeVisible();
    await expect(main.getByRole("heading")).toHaveCount(1);
    await expect(page.locator("h1 + p")).toHaveCount(0);
    await expect(main.getByText(/read now|their owner drags/)).toHaveCount(0);
    const tabs = page.getByRole("tab");
    await expect(tabs).toHaveCount(3);
    await expect(tabs.nth(0)).toHaveAccessibleName(TABS.order);
    await expect(tabs.nth(1)).toHaveAccessibleName(TABS.none);
    await expect(tabs.nth(2)).toHaveAccessibleName(TABS.backlog);
    await expect(page.getByRole("tab", { name: TABS.order })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    const priorities = section(page, "Priorities");
    const prioritiesTable = page.getByRole("table", { exact: true, name: "Priorities table" });
    await expect(prioritiesTable).toBeVisible();
    await expect(page.getByRole("table")).toHaveCount(1);

    // The order itself: the rows in rank order, each named by its place and its title, and
    // each title a link to the priority's own page, beside its owner, the three counts of the
    // work under it as it stands - in flight, waiting, and in backlog or icebox, where the
    // child that inherits #3 stands - and when anything under it last moved.
    await expectRowOrder(priorities, [
      `#1 ${seed.titles[0]}`,
      `#2 ${seed.titles[1]}`,
      `#3 ${seed.titles[2]}`,
    ]);
    const rows = [
      { counts: ["1", "1", "0"], owner: "alice" },
      { counts: ["1", "0", "0"], owner: "bob" },
      { counts: ["1", "1", "2"], owner: "alice" },
    ];
    for (const [place, { counts, owner }] of rows.entries()) {
      const row = priorities.getByRole("row", {
        exact: true,
        name: `#${place + 1} ${seed.titles[place]}`,
      });
      await expect(
        row.getByRole("link", { exact: true, name: seed.titles[place] })
      ).toHaveAttribute("href", `/priorities/${seed.ids[place]}`);
      const cell = (header: string) => cellUnder(prioritiesTable, row, header);
      await expect(await cell("Owner")).toHaveText(owner);
      await expect(await cell("Active")).toHaveText(counts[0] ?? "");
      await expect(await cell("Triage, Todo")).toHaveText(counts[1] ?? "");
      await expect(await cell("Backlog, icebox")).toHaveText(counts[2] ?? "");
      await expect((await cell("Newest event")).locator("time")).toHaveCount(1);
    }
    // A closed row shows the start of its description under its title, its paragraphs run
    // together in a box at most two lines high that ends in '…': a two-line clamp that holds
    // more than it shows. The opened row holds the whole of it, every paragraph (below).
    const firstRow = priorities.getByRole("row", { name: `#1 ${seed.titles[0]}` });
    await expect(firstRow).toContainText(
      "Every machine of the fleet runs the daemon, or the rollout is not done."
    );
    await expect
      .poll(() =>
        firstRow.getByText(/^Every machine of the fleet/).evaluate((excerpt) => {
          const style = getComputedStyle(excerpt);
          const lines =
            excerpt.getBoundingClientRect().height / Number.parseFloat(style.lineHeight);
          return {
            atMostTwoLines: Math.round(lines) <= 2,
            clamp: style.webkitLineClamp,
            holdsMore: excerpt.scrollHeight > excerpt.clientHeight,
          };
        })
      )
      .toEqual({ atMostTwoLines: true, clamp: "2", holdsMore: true });

    // At 1440 by 900 the table fits its box: no column runs past the box's right edge, and the
    // pager under the box is on the first screen.
    await expect.poll(() => tablesOverflow(page)).toEqual({ "Priorities table": 0 });
    await expect.poll(() => documentOverflow(page)).toBeLessThanOrEqual(0);
    await expect.poll(() => pagerBelowWindow(page)).toBeLessThanOrEqual(0);

    // The grouping control reads "Group by".
    await expect(priorities.getByRole("combobox", { exact: true, name: "Group by" })).toBeVisible();

    // The count closed in the last 7 days is hidden until the column control turns it on.
    const closed = prioritiesTable.getByRole("columnheader", { name: /^Closed, 7 days/ });
    await expect(closed).toHaveCount(0);
    await priorities.getByRole("button", { exact: true, name: "Columns" }).click();
    await page
      .getByRole("dialog", { name: "Columns" })
      .getByRole("checkbox", { name: "Closed, 7 days" })
      .check();
    await expect(closed).toBeVisible();
    await page.keyboard.press("Escape");
    // Each column reads in at most 16 characters (INFRA-3221, R13): the counts read "Active",
    // "Triage, Todo" and "Backlog, icebox".
    // Each header's words are its text less its filter control's word ("Filter") and its sort
    // arrow.
    const headers = (await prioritiesTable.locator("thead th").allTextContents())
      .map((text) =>
        text
          .replace(/Filter$/, "")
          .replace(/[▲▼]/g, "")
          .trim()
      )
      .filter((text) => text !== "");
    expect(headers).toEqual(expect.arrayContaining(["Active", "Triage, Todo", "Backlog, icebox"]));
    for (const header of headers) expect(header.length, header).toBeLessThanOrEqual(16);

    // At 1440 by 900 every header's words show whole, Closed's among them: none is cut at its
    // column's edge. A count is no value a reader filters by, so a count's header is its words
    // and its sort arrow alone, and its numbers stand at its right edge, ending where its words
    // do.
    expect(await cutHeaders(prioritiesTable)).toEqual([]);
    for (const header of ["Active", "Triage, Todo", "Backlog, icebox", "Closed, 7 days"]) {
      const column = prioritiesTable.getByRole("columnheader", { name: new RegExp(`^${header}`) });
      await expect(column.getByRole("button", { name: /^Filter/ })).toHaveCount(0);
      const words = await textRight(column.locator('button[title^="Sort"]'));
      const count = await textRight(
        (await cellUnder(prioritiesTable, firstRow, header)).locator("span")
      );
      expect(Math.abs(words - count), header).toBeLessThan(1);
    }

    // Three rows are fewer than the window holds, so the box ends with its last row, its bottom
    // border under it, rather than standing empty down to the window's foot.
    await expect
      .poll(() =>
        page.getByTestId("priorities-table-scroll").evaluate((box) => {
          const last = [...box.querySelectorAll("tr[data-issue-row]")].at(-1);
          if (last === undefined) return Number.NaN;
          return box.getBoundingClientRect().bottom - last.getBoundingClientRect().bottom;
        })
      )
      .toBeLessThanOrEqual(2);

    // At 1280 by 800 the table fits its box as well, every header's words whole, two lines of
    // them at most where a count's column is as narrow as its numbers; the Priority column takes
    // what the others leave. The start of a closed row's description, its paragraphs run
    // together, wraps under its title and is not cut sideways.
    await page.setViewportSize(DESKTOP);
    await expect.poll(() => tablesOverflow(page)).toEqual({ "Priorities table": 0 });
    expect(await cutHeaders(prioritiesTable)).toEqual([]);
    expect(
      await firstRow
        .getByTitle("Every machine of the fleet runs the daemon, or the rollout is not done.")
        .evaluate((line) => ({
          cut: line.scrollWidth > line.clientWidth,
          overflow: getComputedStyle(line).textOverflow,
        }))
    ).toEqual({ cut: false, overflow: "clip" });
    await priorityPage(page);

    // Opened, the row holds the whole description, every paragraph of the seed's.
    await priorities.getByRole("button", { name: `Open #1 ${seed.titles[0]}` }).click();
    const description = page.getByRole("textbox", { exact: true, name: "Description of #1" });
    for (const paragraph of seed.description.split("\n\n"))
      await expect(description).toContainText(paragraph);
    await expect(description.locator("p")).toHaveCount(3);
    await priorities.getByRole("button", { name: `Close #1 ${seed.titles[0]}` }).click();

    // Each fault is a tab of its own, which the address keeps across a reload, and which reads
    // its name and its one count; nothing stands between the tab and its one table - no line of
    // description and no counts - which is this repository's Issues table, read by its own box's
    // test identifier.
    await chooseTabAndReload(page, "none");
    const unplaced = section(page, "Active under no priority");
    await expect(page.getByRole("tab", { name: "Active under no priority 4" })).toBeVisible();
    await expect(main.getByText(/advance no priority|\d+ issues? · /)).toHaveCount(0);
    await expect(prioritiesTable).toHaveCount(0);
    const unplacedTable = page
      .getByTestId("issues-table-scroll")
      .getByRole("table", { exact: true, name: NO_PRIORITY_TABLE });
    await expect(page.getByRole("table")).toHaveCount(1);
    await expect(unplaced.getByRole("table")).toHaveAccessibleName(NO_PRIORITY_TABLE);

    // Its rows are a read of their own beside the priorities read, so they are waited for: one
    // issue in each of the four in-flight statuses, and nothing that advances a priority.
    const clockRow = unplacedTable.getByRole("row", {
      exact: true,
      name: `${seed.clock} Audit the clock`,
    });
    await expect(clockRow).toBeVisible();
    await expect
      .poll(async () => (await columnTexts(unplacedTable, "Status")).sort())
      .toEqual(["In progress", "Needs review", "Retro", "Testing"]);
    // The table opens with R6's columns alone, and at 1440 by 900 they fit its box; its box
    // takes the window, down to the pager under it.
    await expectColumns(unplacedTable, FAULT_COLUMNS, [...BACKLOG_COLUMNS, ...HIDDEN_COLUMNS]);
    await expect.poll(() => tablesOverflow(page)).toEqual({ [NO_PRIORITY_TABLE]: 0 });
    await expect.poll(() => documentOverflow(page)).toBeLessThanOrEqual(0);
    await expect.poll(() => pagerBelowWindow(page)).toBeLessThanOrEqual(0);
    // And at 1280 by 800.
    await page.setViewportSize(DESKTOP);
    await expect.poll(() => tablesOverflow(page)).toEqual({ [NO_PRIORITY_TABLE]: 0 });
    await priorityPage(page);
    // Every status pill is whole, "Needs review" the longest: no pill runs past its own box, and
    // none past the edge of its cell, which clips what passes it.
    const statusAt = await columnIndex(unplacedTable, "Status");
    expect(
      await unplacedTable.locator("tr[data-issue-row]").evaluateAll(
        (rows, at) =>
          rows.flatMap((row) => {
            const cell = row.querySelectorAll("td")[at];
            const pill = cell?.firstElementChild;
            if (cell === undefined || pill === null || pill === undefined) return ["no pill"];
            const cut =
              pill.scrollWidth > pill.clientWidth ||
              cell.scrollWidth > cell.clientWidth ||
              pill.getBoundingClientRect().right > cell.getBoundingClientRect().right;
            return cut ? [pill.textContent ?? ""] : [];
          }),
        statusAt
      )
    ).toEqual([]);
    // And one row of it cell by cell (R6), each cell read under its column's header: the issue,
    // its title, its status, its project, who holds it, and how long it has stood as it is.
    const clockCell = (header: string) => cellUnder(unplacedTable, clockRow, header);
    await expect((await clockCell("Key")).getByRole("link")).toHaveText(seed.clock);
    await expect(
      (await clockCell("Title")).getByRole("link", {
        exact: true,
        name: `${seed.clock} Audit the clock`,
      })
    ).toHaveText("Audit the clock");
    await expect(await clockCell("Status")).toHaveText("In progress");
    await expect(await clockCell("Project")).toHaveText("CORE");
    // Who holds it is who claimed it: alice claimed this one in the seed, and the cell is the
    // claim's chip naming her.
    await expect((await clockCell("Claimed by")).getByTestId("issue-claim")).toContainText("alice");
    await expect((await clockCell("Updated")).locator("time")).toHaveCount(1);
    // The row's issue is a link to its own page, and back is the tab the reader left.
    await (await clockCell("Key")).getByRole("link").click();
    await expect(page).toHaveURL(`/issues/${seed.clock}`);
    await page.goBack();
    await expect(page).toHaveURL(/[?&]tab=none(&|$)/);
    await expect(clockRow).toBeVisible();

    // The second fault's tab, whose one row is the child, which advances #3 through the issue
    // above it, and it names when it was moved out of the way and by whom.
    await chooseTabAndReload(page, "backlog");
    const backlog = section(page, "Priority work in backlog or icebox");
    await expect(
      page.getByRole("tab", { name: "Priority work in backlog or icebox 1" })
    ).toBeVisible();
    await expect(main.getByText(/backlog or the icebox|\d+ issues? · /)).toHaveCount(0);
    const backlogTable = page
      .getByTestId("issues-table-scroll")
      .getByRole("table", { exact: true, name: BACKLOG_TABLE });
    await expect(backlog.getByRole("table")).toHaveAccessibleName(BACKLOG_TABLE);
    await expect(page.getByRole("table")).toHaveCount(1);
    await expectRowOrder(backlogTable, [`${seed.archive} Ledger archive`]);
    // It opens with the fault's columns but Updated, beside the priority each row stands under
    // and the move, which is how long the work has stood.
    await expectColumns(
      backlogTable,
      [...FAULT_COLUMNS.filter((name) => name !== "Updated"), ...BACKLOG_COLUMNS],
      [...HIDDEN_COLUMNS.filter((name) => name !== "Advances"), "Updated", "Reason"]
    );
    await expect.poll(() => tablesOverflow(page)).toEqual({ [BACKLOG_TABLE]: 0 });
    await expect.poll(() => documentOverflow(page)).toBeLessThanOrEqual(0);
    // At 1280 by 800 too, where Title takes the room the others leave and Advances stands at its
    // cap: the chip, and the title wrapping whole after it.
    await page.setViewportSize(DESKTOP);
    await expect.poll(() => tablesOverflow(page)).toEqual({ [BACKLOG_TABLE]: 0 });
    expect(
      await backlogTable
        .getByRole("columnheader", { exact: true, name: "Advances" })
        .evaluate((header) => header.getBoundingClientRect().width)
    ).toBeLessThanOrEqual(BACKLOG_ADVANCES_CAP);
    await priorityPage(page);
    // The one row of that tab is the archive, whose own row names #3: the child that inherits #3 and
    // stands in the backlog beside it is not on that tab.
    const childRow = backlogTable.getByRole("row", {
      exact: true,
      name: `${seed.archive} Ledger archive`,
    });
    const childCell = (header: string) => cellUnder(backlogTable, childRow, header);
    await expect((await childCell("Key")).getByRole("link")).toHaveAttribute(
      "href",
      `/issues/${seed.archive}`
    );
    await expect(await childCell("Project")).toHaveText("WEB");
    await expect(await childCell("Status")).toHaveText("Backlog");
    // Nobody claimed this one: this column says who holds the work, so it is empty here, as
    // every Issues table draws an issue nobody holds.
    await expect(await childCell("Claimed by")).toHaveText("");
    // Updated is one tick away under Columns, unticked.
    await backlog.getByRole("button", { name: "Columns" }).click();
    await expect(
      page.getByRole("dialog", { name: "Columns" }).getByRole("checkbox", { name: "Updated" })
    ).not.toBeChecked();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Columns" })).toHaveCount(0);
    // Moved there: the moment the seed moved it into the backlog, and who moved it.
    const moved = await childCell("Moved there");
    await expect(moved.locator("time")).toHaveCount(1);
    await expect(moved).toContainText("alice");
    // Advances: the priority it stands under, as the chip of its place in the order, #3, and
    // then its title, drawn whole: which priority the work stands under is what the tab is
    // for, and a phone has no tooltip to read it in. Both are one link to that priority, whose
    // name and tooltip carry the title.
    const advancesCell = await childCell("Advances");
    const advances = advancesCell.getByRole("link");
    await expect(advances.locator("span").first()).toHaveText("#3");
    await expect(advances).toHaveAccessibleName(`#3 ${seed.titles[2]}`);
    await expect(advances).toHaveAttribute("title", seed.titles[2]);
    await expect(advances).toHaveAttribute("href", `/priorities/${seed.ids[2]}`);
    const drawnTitle = advancesCell.getByText(seed.titles[2], { exact: true });
    await expect(drawnTitle).toBeVisible();
    // Whole: it wraps rather than ends in an ellipsis or a clamp, nothing of it lies past its own
    // box, and the cell, which clips what passes its edge, holds all of it.
    expect(
      await drawnTitle.evaluate((title) => {
        const style = getComputedStyle(title);
        const box = title.getBoundingClientRect();
        const cell = title.closest("td")?.getBoundingClientRect();
        return {
          clamp: style.webkitLineClamp,
          cut: title.scrollWidth > title.clientWidth || title.scrollHeight > title.clientHeight,
          inside: cell !== undefined && box.right <= cell.right && box.bottom <= cell.bottom,
          overflow: style.textOverflow,
          wrap: style.whiteSpace,
        };
      })
    ).toEqual({ clamp: "none", cut: false, inside: true, overflow: "clip", wrap: "normal" });

    // On a phone the page's document is no wider than the window on any tab: each fault's
    // sentence wraps and its counts take a line of their own, and a table scrolls inside its box.
    await page.setViewportSize(PHONE);
    for (const tab of ["order", "none", "backlog"] as const) {
      await chooseTab(page, tab);
      await expect(page.getByRole("table")).toHaveCount(1);
      await expect(page.getByRole("table")).toBeVisible();
      await expect.poll(() => documentOverflow(page)).toBeLessThanOrEqual(0);
    }
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): fifty-one issues written and moved into flight, and the tab's two pages read.
test("a fault tab of more than one page counts every row, keeps its status strip, and pages to the last", async ({
  browser,
}) => {
  await createProject({ key: "BULK", name: "Bulk" });
  // Fifty-one issues in progress that advance no priority: one more than a page of the table.
  const keys = await Promise.all(
    Array.from({ length: 51 }, async (_, at) => {
      const created = await createIssue({ project: "BULK", title: `Active issue ${at + 1}` });
      await patchIssue(created.key, { status: "in_progress" });
      return created.key;
    })
  );
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(`${PRIORITIES_PATH}?tab=none`);
    // The tab counts all fifty-one, and its table is the Issues table with its status strip.
    await expect(page.getByRole("tab", { name: "Active under no priority 51" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    const unplaced = section(page, "Active under no priority");
    await expect(unplaced.getByRole("group", { name: "Issues by status" })).toBeVisible();
    const table = page.getByRole("table", { exact: true, name: NO_PRIORITY_TABLE });
    await expect(table.locator("tr[data-issue-row]").first()).toBeVisible();
    // A page holds fifty rows, and the pager counts all fifty-one; its next page holds the one
    // row the first left over, the fifty-first. The rows of a page are drawn as they scroll into
    // the box, so the second page's one row is what this reads.
    const pager = page.getByTestId("issues-table-pager");
    await expect(pager).toContainText("51");
    await pager.getByRole("button", { name: "Next ›" }).click();
    // The second page's one row, read until it is the only row drawn: its key is one of the
    // fifty-one written, taken as the row name's first word.
    await expect
      .poll(async () =>
        (await rowNames(table)).map((name) => keys.includes(name.split(" ", 1).join("")))
      )
      .toEqual([true]);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): twenty priorities written one after another, and the box scrolled.
test("more priorities than the window holds scroll inside the table, under a header that stays", async ({
  browser,
}) => {
  // One after another, so the order is the order they were written in. Each carries a
  // description, whose first line every closed row shows under its title: twenty rows two lines
  // tall are more than the box a 1440x900 window gives the table.
  for (let place = 1; place <= 20; place += 1)
    await createPriority({
      description: `What priority ${place} is for.`,
      owner: "alice",
      title: `Priority number ${place}`,
    });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(PRIORITIES_PATH);
    const box = page.getByTestId("priorities-table-scroll");
    await expect(box.getByRole("row", { name: "#1 Priority number 1" })).toBeVisible();

    // The table and its pager fit the window: the page itself scrolls nowhere, sideways or down,
    // and the twenty rows scroll inside the box, which is the scroll this row reads next.
    await expect.poll(() => tablesOverflow(page)).toEqual({ "Priorities table": 0 });
    await expect.poll(() => pagerBelowWindow(page)).toBeLessThanOrEqual(0);
    await expect
      .poll(() =>
        page.evaluate(
          () => document.documentElement.scrollHeight - document.documentElement.clientHeight
        )
      )
      .toBeLessThanOrEqual(0);
    expect(
      await box.evaluate((element) => element.scrollHeight - element.clientHeight)
    ).toBeGreaterThan(0);

    // Scrolled, the rows move under the header, which stays at the box's top edge.
    const headerTop = () =>
      box.evaluate(
        (element) =>
          (element.querySelector("thead th")?.getBoundingClientRect().top ?? Number.NaN) -
          element.getBoundingClientRect().top
      );
    const before = await headerTop();
    await box.evaluate((element) => {
      element.scrollTop = 300;
    });
    await expect.poll(() => box.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
    expect(await headerTop()).toBe(before);
    await expect(box.getByRole("columnheader", { name: /^Priority/ })).toBeInViewport();
    await expect(box.getByRole("row", { name: "#1 Priority number 1" })).not.toBeInViewport();
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): an empty page, the first priority written through it, and a reload that reads it.
test("with no priority the page reads the server's sentence, and writes the first one", async ({
  browser,
}) => {
  // No seed: the row starts from the emptied database (`e2e/test.ts`), which holds no priority.
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(PRIORITIES_PATH);
    await expect(page.getByRole("heading", { level: 1, name: "Priorities" })).toBeVisible();

    // The sentence is the server's own, and no table is drawn where there is nothing to order.
    // The rail's entry carries no number: nothing is off a list that does not exist.
    await expect(page.getByText(NONE_LIVE, { exact: true })).toBeVisible();
    await expect(page.getByRole("table", { exact: true, name: "Priorities table" })).toHaveCount(0);
    await expect(railEntry(page).locator("span")).toHaveCount(2);

    // The page with no priority is where the first is written, with the control the table's
    // toolbar carries once there is one.
    const added = "Ship the daemon everywhere";
    await page.getByRole("button", { exact: true, name: "New priority" }).click();
    await page.getByLabel("Title of the new priority").fill(added);
    await page
      .locator('form[aria-label="New priority"]')
      .getByRole("button", { name: "Add priority" })
      .click();
    await expectRowOrder(section(page, "Priorities"), [`#1 ${added}`]);
    await expect(page.getByText(NONE_LIVE, { exact: true })).toHaveCount(0);

    // The store's, not the page's: read back from the server, and from a page loaded again.
    await expect
      .poll(async () => (await listPriorities()).priorities?.map((row) => row.title))
      .toEqual([added]);
    await page.reload();
    await expectRowOrder(section(page, "Priorities"), [`#1 ${added}`]);
    await expect(page.getByText(NONE_LIVE, { exact: true })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a priority written through the page's form, its row read, and a refused create.
test("a long title through the New priority form lands under its cut id", async ({ browser }) => {
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(PRIORITIES_PATH);

    // The form sends the title alone, so the id is the one the server derives from it.
    await page.getByRole("button", { exact: true, name: "New priority" }).click();
    await page.getByLabel("Title of the new priority").fill(LONG_CASE.title);
    await page
      .locator('form[aria-label="New priority"]')
      .getByRole("button", { name: "Add priority" })
      .click();

    // The table holds the whole title, and the title's link opens the page of the cut id.
    await expectRowOrder(section(page, "Priorities"), [`#1 ${LONG_CASE.title}`]);
    const row = page.getByRole("row", { exact: true, name: `#1 ${LONG_CASE.title}` });
    await expect(row.getByRole("link", { exact: true, name: LONG_CASE.title })).toHaveAttribute(
      "href",
      `/priorities/${LONG_CASE.id}`
    );

    // An id the caller names is not cut: one character past the bound is refused, by the bound.
    // `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
    // cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin).
    const refused = await page.request.post("/api/v1/priorities", {
      data: { id: "n".repeat(PRIORITY_ID_MAX + 1), title: "Name the id by hand" },
      headers: { Origin: dashboardOrigin },
    });
    expect(refused.status()).toBe(400);
    const body = await refused.json();
    expect(body.code).toBe("PRIORITY_INPUT");
    expect(body.error).toContain(String(PRIORITY_ID_MAX));
    expect((await listPriorities()).priorities?.map((stored) => stored.id)).toEqual([LONG_CASE.id]);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, three writes a person makes on a priority's page and on a row, and a
// reload that reads them back.
test("a priority row opens its description, its title its page, and what is written there reads back on the row after a reload", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(PRIORITIES_PATH);

    // An opened row shows the priority's description and nothing more: the tab draws the one
    // table, so the keys a table is walked with are its alone, and `j` on a focused priority
    // moves to the next priority.
    await page.getByRole("button", { name: `Open #3 ${seed.titles[2]}` }).click();
    await expect(
      page.getByRole("textbox", { exact: true, name: "Description of #3" })
    ).toBeVisible();
    await expect(page.getByRole("table")).toHaveCount(1);
    await expect(page.locator('section[aria-label^="Work advancing"]')).toHaveCount(0);
    await page.locator(`tr[data-issue-row="${seed.ids[1]}"]`).focus();
    await page.keyboard.press("j");
    await expect(page.locator(`tr[data-issue-row="${seed.ids[2]}"]`)).toBeFocused();
    await page.getByRole("button", { name: `Close #3 ${seed.titles[2]}` }).click();

    // The work is on the priority's own page, which the title opens: there the work table is
    // the one table, and the same keys walk its rows.
    await page.getByRole("link", { exact: true, name: seed.titles[2] }).click();
    await expect(page).toHaveURL(`/priorities/${seed.ids[2]}`);
    const work = section(page, "Work advancing this priority");
    await expectRowNames(work, [
      `${seed.archive} Ledger archive`,
      `${seed.child} Ledger client migration`,
      `${seed.cutover} Ledger cut-over`,
      `${seed.ledger} Ledger read path`,
    ]);
    const workRows = work.locator("tr[data-issue-row]");
    const firstKey = await workRows.first().getAttribute("data-issue-row");
    const secondKey = await workRows.nth(1).getAttribute("data-issue-row");
    await workRows.first().focus();
    await page.keyboard.press("j");
    await expect(work.locator(`tr[data-issue-row="${secondKey}"]`)).toBeFocused();
    await page.keyboard.press("k");
    await expect(work.locator(`tr[data-issue-row="${firstKey}"]`)).toBeFocused();
    await page.getByRole("link", { name: "‹ Priorities" }).click();
    await expect(page.getByRole("table", { name: "Priorities table" })).toBeVisible();

    // Renamed and described on its own page, which its title opens: no edit mode and no Save
    // there, so leaving each field writes it, and the list it is read from again names it so.
    // The title reads as the page's heading until a click opens it.
    const renamed = "Cut the review queue in half";
    const explained = "Every review is answered within a day.";
    await page.getByRole("link", { exact: true, name: seed.titles[1] }).click();
    await expect(page).toHaveURL(`/priorities/${seed.ids[1]}`);
    const ownTitle = await openTitle(page);
    await ownTitle.fill(renamed);
    await ownTitle.blur();
    await expect(titleHeading(page)).toHaveText(renamed);
    const ownEditor = page.getByRole("textbox", { exact: true, name: "Description" });
    await ownEditor.click();
    await page.keyboard.type(explained);
    await ownEditor.blur();
    await page.getByRole("link", { name: "‹ Priorities" }).click();
    await expect(page.getByRole("row", { name: `#2 ${renamed}` })).toBeVisible();

    // Described in the row that holds it too, in the same documents' editor, every paragraph of
    // it, and written where it is read. It is the editor Dispatch's documents are written in, as
    // on the priority's own page, and not a plain text box: a selection in it offers no comment,
    // suggestion or ask, since a priority takes none.
    const described = "The fleet is one build, or the rollout is not done.";
    const editor = page.getByRole("textbox", { exact: true, name: "Description of #1" });
    const finished = "It is finished when no machine is left on the old build";
    await page.getByRole("button", { name: `Open #1 ${seed.titles[0]}` }).click();
    await expect(editor).toContainText(finished);
    await expect(editor).toHaveClass(/\bProseMirror\b/);
    await expect(editor).toHaveAttribute("contenteditable", "true");
    // The row's column wraps the paragraph over several lines, and End and Shift+Home select one
    // of them: a run of the paragraph's words, which is what a selection here is. ProseMirror's
    // focus handler (prosemirror-view `handlers.focus`) writes the editor's own selection back to
    // the page 20 ms after the click focuses it, and undoes a run the keys selected before then
    // (`editor.ts`'s `setEditorRange`), so the gesture is retried until the run is selected.
    await expect(async () => {
      await editor.getByText(finished).click();
      await page.keyboard.press("End");
      await page.keyboard.press("Shift+Home");
      expect(
        await page.evaluate(() => String(window.getSelection()).trim().length)
      ).toBeGreaterThan(0);
    }).toPass({ timeout: 5_000 });
    const paragraph =
      "It is finished when no machine is left on the old build and nothing is waiting on one.";
    expect(paragraph).toContain((await page.evaluate(() => String(window.getSelection()))).trim());
    await expect(actionBar(page)).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^(Comment|Suggest|Ask)$/ })).toHaveCount(0);
    await page.keyboard.press("ControlOrMeta+a");
    await page.keyboard.type(described);
    await editor.blur();
    await expect(editor).toHaveText(described);

    // Each is the store's now, not the page's: read back from the server, and then from a page
    // that was loaded again, on the rows of the list.
    const stored = async (id: string) =>
      (await listPriorities()).priorities?.find((row) => row.id === id);
    await expect
      .poll(async () => {
        const second = await stored(seed.ids[1]);
        return [second?.title, second?.description, (await stored(seed.ids[0]))?.description];
      })
      .toEqual([renamed, `${explained}\n`, `${described}\n`]);
    await page.reload();
    await expectRowOrder(section(page, "Priorities"), [
      `#1 ${seed.titles[0]}`,
      `#2 ${renamed}`,
      `#3 ${seed.titles[2]}`,
    ]);
    await page.getByRole("button", { name: `Open #2 ${renamed}` }).click();
    await expect(page.getByRole("textbox", { exact: true, name: "Description of #2" })).toHaveText(
      explained
    );
    await page.getByRole("button", { name: `Open #1 ${seed.titles[0]}` }).click();
    await expect(editor).toHaveText(described);
    // The store's text: what the reader wrote, and no Proof mark of the editor's own.
    expect((await stored(seed.ids[0]))?.description).not.toContain("data-proof");

    // What the page does not say about a priority (INFRA-2947, R10): no date it is wanted by,
    // no portion of it done, and no word judging how it is going. Every `<time>` the page draws
    // is a moment that has passed, and no relative label reads forwards. Each absence is read on
    // each tab once the parts that carry dates and counts have drawn - the opened description
    // and each fault list, each a read of its own - since a page still loading them would pass
    // it on text it never drew.
    await expectNoForecast(page);
    await chooseTab(page, "none");
    await expect(
      page
        .getByRole("table", { exact: true, name: NO_PRIORITY_TABLE })
        .locator("tr[data-issue-row]")
    ).toHaveCount(4);
    await expectNoForecast(page);
    await chooseTab(page, "backlog");
    await expectRowOrder(page.getByRole("table", { exact: true, name: BACKLOG_TABLE }), [
      `${seed.archive} Ledger archive`,
    ]);
    await expectNoForecast(page);
  } finally {
    await context.close();
  }
});

// @firefox: this row proves that a drag by the grip writes the order in Firefox as in Chromium.
// It does not prove the grip's `setData` line (`priorities-columns.tsx`), which a person's Firefox
// needs, since it starts no HTML drag whose `dragstart` carries no data: Playwright's Firefox
// passes this row with that line taken out. The unit row "dragging a row onto another writes the
// place it landed" (`PrioritiesPage.test.tsx`) is what holds it. @webkit: WebKit dispatches the
// drag's `dragover` and `drop` to the row under it by its own rule, which the row's
// `preventDefault` must meet for the drop to land.
// SLOW(>=2s): the seed, a priority added, a row dragged, and a reload that reads the order back.
test("a priority added through the form and a row dragged write the order, which reads back after a reload @firefox @webkit", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(PRIORITIES_PATH);
    const priorities = section(page, "Priorities");
    await expectRowOrder(priorities, [
      `#1 ${seed.titles[0]}`,
      `#2 ${seed.titles[1]}`,
      `#3 ${seed.titles[2]}`,
    ]);

    // Added, which takes the last place of the order.
    const added = "Keep the fleet on one build";
    await page.getByRole("button", { exact: true, name: "New priority" }).click();
    await page.getByLabel("Title of the new priority").fill(added);
    await page
      .locator('form[aria-label="New priority"]')
      .getByRole("button", { name: "Add priority" })
      .click();
    await expect(page.getByRole("row", { name: `#4 ${added}` })).toBeVisible();

    // And dragged by its grip: the third row onto the second, which writes the place the reader
    // can see.
    const handle = page.getByRole("button", {
      name: `Move #3 ${seed.titles[2]}: drag it, or press the up and down arrows`,
    });
    await expect(handle).toBeVisible();
    await handle.dragTo(page.locator(`tr[data-issue-row="${seed.ids[1]}"]`));
    await expect(page.getByRole("row", { name: `#2 ${seed.titles[2]}` })).toBeVisible();

    // Both are the store's now, not the page's: read back from the server, and then from a page
    // that was loaded again.
    await expect
      .poll(async () => (await listPriorities()).priorities?.map((row) => row.title))
      .toEqual([seed.titles[0], seed.titles[2], seed.titles[1], added]);
    await page.reload();
    await expectRowOrder(priorities, [
      `#1 ${seed.titles[0]}`,
      `#2 ${seed.titles[2]}`,
      `#3 ${seed.titles[1]}`,
      `#4 ${added}`,
    ]);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, the rail's entry followed and held open across two writes, and a third
// read after a reload.
test("the rail's Priorities count falls as work leaves flight and as a parent link moves", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    // Read from another page, because the number is the rail's and not the priorities page's.
    await page.goto(`/issues/${seed.rollout}`);
    const entry = railEntry(page);
    await expect(entry).toBeVisible();
    await expect(entry.locator("span").last()).toHaveText("5");

    // The entry leads to the priorities page, which nothing below reloads until the two counts
    // have fallen. A mark on the document would be gone after a reload, so its presence then
    // says there was none.
    await entry.click();
    await expect(page).toHaveURL(PRIORITIES_PATH);
    await expect(page.getByRole("heading", { level: 1, name: "Priorities" })).toBeVisible();
    // The page it opens is the order's tab, the one an address naming none opens.
    await expect(page.getByRole("tab", { name: TABS.order })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await page.evaluate(() => {
      document.documentElement.dataset.heldOpen = "yes";
    });

    // Four in flight under no priority plus one in backlog. An issue that leaves `in_progress` is
    // no longer in flight, so the first fault loses it - with no reload: the event that moved
    // it refreshes the two counts (`web/src/api/event-refresh.ts`).
    await patchIssue(seed.clock, { status: "done" });
    await expect(entry.locator("span").last()).toHaveText("4");

    // And an issue in flight under no priority placed under a parent that advances #1 now
    // advances #1 through it (INFRA-2947, R4), so it leaves the first fault, and the count
    // falls again.
    await patchIssue(seed.lease, { parent: seed.rollout });
    await expect(entry.locator("span").last()).toHaveText("3");
    expect(await page.evaluate(() => document.documentElement.dataset.heldOpen)).toBe("yes");

    // A deployment that has written no priority has nothing off a list that does not exist, so
    // the entry carries no number: its label and its line, and no badge beside them.
    for (const id of seed.ids) await endPriority(id);
    await page.reload();
    await expect(railEntry(page)).toBeVisible();
    await expect(railEntry(page).locator("span")).toHaveCount(2);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, two people's pages, a rename made on one and a delete beside it, each
// read on the other with no reload.
test("another person's rename and delete of a priority redraw an open page with no reload", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  // #3's one issue in flight leaves flight first, so the delete below moves no issue into the
  // first fault list, and what the rail counts can only fall by the work in backlog under #3.
  await patchIssue(seed.ledger, { status: "todo" });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // Reader A: alice, on the priorities page, which nothing below reloads. A mark on the
    // document would be gone after a reload, so its presence at the end says there was none.
    const reader = await alice.newPage();
    await priorityPage(reader);
    await reader.goto(PRIORITIES_PATH);
    await reader.evaluate(() => {
      document.documentElement.dataset.heldOpen = "yes";
    });
    const priorities = section(reader, "Priorities");
    const backlogTable = reader.getByRole("table", { exact: true, name: BACKLOG_TABLE });
    const childRow = backlogTable.getByRole("row", {
      exact: true,
      name: `${seed.archive} Ledger archive`,
    });
    const badge = railEntry(reader).locator("span").last();
    await expectRowOrder(priorities, [
      `#1 ${seed.titles[0]}`,
      `#2 ${seed.titles[1]}`,
      `#3 ${seed.titles[2]}`,
    ]);
    // Four in flight under no priority plus the one in backlog.
    await expect(badge).toHaveText("5");

    // Reader B: bob renames #3 on its own page. Alice's row takes the new title, and so does
    // the Advances cell of the work in backlog on its tab, which names a priority by the title a person
    // wrote: the title the page's one read of the priorities holds, which choosing a tab does
    // not read again.
    const renamed = "Retire the old ledger for good";
    const writer = await bob.newPage();
    await writer.goto(`/priorities/${seed.ids[2]}`);
    const writersTitle = await openTitle(writer);
    await writersTitle.fill(renamed);
    await writersTitle.blur();
    await expectRowOrder(priorities, [
      `#1 ${seed.titles[0]}`,
      `#2 ${seed.titles[1]}`,
      `#3 ${renamed}`,
    ]);
    await chooseTab(reader, "backlog");
    await expect(childRow).toBeVisible();
    // The Advances column opens shown on the backlog-or-icebox tab: the priority each row stands under,
    // whose chip carries the title in its name, and which draws the title after the chip.
    const backlogUnder = await cellUnder(backlogTable, childRow, "Advances");
    await expect(backlogUnder.getByRole("link")).toHaveAccessibleName(`#3 ${renamed}`);
    await expect(backlogUnder.getByText(renamed, { exact: true })).toBeVisible();

    // Then bob retires #3, over the API, since neither page offers a delete. The archive
    // advanced only #3, so it is no fault at all now: it leaves the backlog-or-icebox list in front of
    // alice, the tab's count and the rail's fall by it, and the order loses the row.
    await endPriority(seed.ids[2], { login: "bob" });
    await expectRowNames(backlogTable, []);
    await expect(
      reader.getByRole("tab", { name: "Priority work in backlog or icebox 0" })
    ).toBeVisible();
    await expect(badge).toHaveText("4");
    await chooseTab(reader, "order");
    await expectRowOrder(priorities, [`#1 ${seed.titles[0]}`, `#2 ${seed.titles[1]}`]);

    expect(await reader.evaluate(() => document.documentElement.dataset.heldOpen)).toBe("yes");
  } finally {
    await Promise.all([alice.close(), bob.close()]);
  }
});

// SLOW(>=2s): the seed, two writes read back from the server, and a reload.
test("a priority's own page writes its title and description and lists only its work", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  // A description with a heading and a list, so what the page draws of it is read as those
  // elements rather than as the markdown source it is stored as.
  const outline = "# Why\n\n- Every machine runs the daemon\n- Nothing waits on the old build";
  await patchPriority(seed.ids[0], { description: outline });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(`/priorities/${seed.ids[0]}`);

    await expect(page.getByRole("link", { name: "‹ Priorities" })).toBeVisible();
    // The header's heading is the title, whole, and no field shows until a click opens it. The
    // owner is a chip under it, which a click opens into its field; left, it is the chip again.
    await expect(titleHeading(page)).toHaveText(seed.titles[0]);
    await expect(page.getByRole("textbox", { name: "Title" })).toHaveCount(0);
    await page.getByRole("button", { name: "Owner alice" }).click();
    const owner = page.getByRole("combobox", { name: "Owner" });
    await expect(owner).toHaveValue("alice");
    await owner.blur();
    await expect(owner).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Owner alice" })).toBeVisible();
    // One page and no tab strip: everything of the priority is on this page, and what happened
    // to it is in the margin beside it.
    await expect(page.getByRole("tablist")).toHaveCount(0);
    await expect(page.getByRole("tab")).toHaveCount(0);

    const retitled = "Ship the daemon to every machine";
    const title = await openTitle(page);
    await title.fill(retitled);
    await title.blur();

    // The description is the editor Dispatch's documents are written in (`features/doc/
    // editor.ts`), named for the description: the markdown it is, drawn as a heading and a
    // list, with no source and no edit mode.
    const editor = page.getByRole("textbox", { exact: true, name: "Description" });
    await expect(editor).toHaveClass(/\bProseMirror\b/);
    await expect(editor).toHaveAttribute("contenteditable", "true");
    await expect(editor.getByRole("heading", { name: "Why" })).toBeVisible();
    await expect(editor.getByRole("listitem")).toHaveText([
      "Every machine runs the daemon",
      "Nothing waits on the old build",
    ]);

    // A selection in it offers no comment, suggestion or ask, since a priority takes none: the
    // documents' selection bar, which their editor draws as it is built, is not drawn at all.
    // The selection is the keyboard's. ProseMirror's focus handler (prosemirror-view
    // `handlers.focus`) writes the editor's own selection back to the page 20 ms after the click
    // focuses it, and undoes a run the keys selected before then (`editor.ts`'s
    // `setEditorRange`), so the gesture is retried until the run is selected.
    await expect(async () => {
      await editor.getByText("Nothing waits on the old build").click();
      await page.keyboard.press("End");
      await page.keyboard.press("Shift+Home");
      expect(await page.evaluate(() => String(window.getSelection()))).toBe(
        "Nothing waits on the old build"
      );
    }).toPass({ timeout: 5_000 });
    await expect(actionBar(page)).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^(Comment|Suggest|Ask)$/ })).toHaveCount(0);

    // A line typed after the list, with one word made bold through the editor's own Mod-b, and
    // left: leaving it writes it at once.
    const line = "The roll-back is written down";
    // The run the previous step selected is still selected when this click lands, so an Enter
    // pressed before the caret collapses replaces it.
    const item = editor.getByRole("listitem").last();
    await expect(async () => {
      await item.click();
      await page.keyboard.press("End");
      expect(
        await item.evaluate((node) => {
          const selection = window.getSelection();
          return selection?.isCollapsed === true && node.contains(selection.anchorNode);
        })
      ).toBe(true);
    }).toPass({ timeout: 5_000 });
    await page.keyboard.press("Enter");
    await page.keyboard.type("The ");
    await page.keyboard.press("ControlOrMeta+b");
    await page.keyboard.type("roll-back");
    await page.keyboard.press("ControlOrMeta+b");
    await page.keyboard.type(" is written down");
    await expect(editor.getByRole("listitem").last()).toHaveText(line);
    await expect(editor.getByRole("listitem").last().locator("strong")).toHaveText("roll-back");
    await editor.blur();

    // The store holds both, the line as markdown with its word in `**`, as the editor writes a
    // list, and nothing the reader did not write: no Proof mark of the editor's own, such as the
    // authorship of what was typed (`<span data-proof="authored" ...>`). The write has landed
    // once the store holds the last words typed; the page loaded again draws the text from the
    // store, and what is read back is the store's, not the editor's.
    const described =
      "# Why\n\n* Every machine runs the daemon\n* Nothing waits on the old build\n" +
      "* The **roll-back** is written down\n";
    const stored = async () =>
      (await listPriorities()).priorities?.find((one) => one.id === seed.ids[0]);
    await expect.poll(async () => (await stored())?.description ?? "").toContain("written down");
    await page.reload();
    await expect(titleHeading(page)).toHaveText(retitled);
    await expect(editor.getByRole("listitem").last()).toHaveText(line);
    await expect(editor.getByRole("listitem").last().locator("strong")).toHaveText("roll-back");
    const row = await stored();
    expect(row?.description).not.toContain("data-proof");
    expect([row?.title, row?.description]).toEqual([retitled, described]);

    // The keyboard reaches the editor as it reaches any field: the next stop after the
    // disclosure above it.
    const description = section(page, "Description");
    await description.getByRole("button", { exact: true, name: "Description" }).focus();
    await page.keyboard.press("Tab");
    await expect(editor).toBeFocused();
    await editor.blur();

    // The work is this priority's and nothing else's: the two issues that advance it, and
    // neither of the issues under the other two.
    const work = section(page, "Work advancing this priority");
    await expectRowNames(work, [
      `${seed.rollout} Daemon rollout to the fleet`,
      `${seed.smoke} Daemon smoke suite`,
    ]);

    // The description is a disclosure, and it shuts and opens; shut, the work moves up into
    // the room it left.
    const open = await topOf(work);
    await description.getByRole("button", { exact: true, name: "Description" }).click();
    await expect(editor).toHaveCount(0);
    await expect.poll(() => topOf(work)).toBeLessThan(open);
    await description.getByRole("button", { exact: true, name: "Description, closed" }).click();
    await expect(editor.getByRole("listitem").last()).toHaveText(line);
    await expect.poll(() => topOf(work)).toBe(open);

    // At 1280 by 800 the margin leaves the work table a box 606 px wide, which is a desktop's:
    // the table fits it, Title taking what the issue, its status and its project leave, and
    // Title's header reads its name and its filter on one line from the cell's left edge, as
    // every other header does.
    await page.setViewportSize(DESKTOP);
    await expect.poll(() => tablesOverflow(page)).toEqual({ "Priority issues table": 0 });
    expect(
      await work.getByRole("columnheader", { name: /^Title/ }).evaluate((header) => {
        const name = header.querySelector('button[title^="Sort"]')?.getBoundingClientRect();
        const filter = header.querySelector('[aria-label="Filter Title"]')?.getBoundingClientRect();
        const cell = header.getBoundingClientRect();
        const padding = Number.parseFloat(getComputedStyle(header).paddingLeft);
        return {
          left: name !== undefined && Math.abs(name.left - cell.left - padding) < 1,
          oneLine:
            name !== undefined &&
            filter !== undefined &&
            filter.top < name.bottom &&
            name.top < filter.bottom,
        };
      })
    ).toEqual({ left: true, oneLine: true });
  } finally {
    await context.close();
  }
});

// INFRA-2947 R7 (spec v76, its third point; spec v85, v89) and check 138e9e4d (Verification v62,
// v66): at 1280 by 800 the work under a priority takes the whole of its box. Title takes all the
// width the key, the project and the status leave, so no space stands empty at the table's
// right: the last column that draws anything ends at the box's right edge, less the room a
// filling column leaves there for the box's borders and a scrollbar (`EDGE_ROOM`, 20 px,
// `features/project/issues-table-layout.ts`). An issue title of 190 characters reads whole there
// and on each fault tab: no ellipsis, no clamp, nothing of it past its box. One of 900, as long
// as production's longest run, is drawn in eight lines ending in '…', its whole text in its
// tooltip.
// SLOW(>=2s): the seed, six more issues, and three tables read at the desktop's size.
test("at 1280 by 800 a priority's work table leaves no empty room at its right, and it and each fault tab read a 190-character title whole", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const title =
    "Daemon rollout to every build host, every developer laptop and every review machine, with " +
    "the smoke suite proving each took the build and the roll-back ready for any machine that " +
    "refused it.";
  expect(title).toHaveLength(190);
  const longTitle = `${"The rollout moves every machine of the fleet onto the new daemon and proves that each one took the build. ".repeat(12).slice(0, 899)}.`;
  expect(longTitle).toHaveLength(900);
  // One issue of each title under #1 in flight, one under no priority in flight, and one whose
  // own row names #1 standing in the backlog: the work table, the first fault tab and the
  // second.
  const work = await createIssue({ project: "CORE", title });
  const none = await createIssue({ project: "OPS", title });
  const inBacklog = await createIssue({ project: "WEB", title });
  const workLong = await createIssue({ project: "CORE", title: longTitle });
  const noneLong = await createIssue({ project: "OPS", title: longTitle });
  const backlogLong = await createIssue({ project: "WEB", title: longTitle });
  await Promise.all([
    patchIssue(work.key, { advances: [seed.ids[0]], status: "in_progress" }),
    patchIssue(none.key, { status: "in_progress" }),
    patchIssue(inBacklog.key, { advances: [seed.ids[0]], status: "backlog" }),
    patchIssue(workLong.key, { advances: [seed.ids[0]], status: "in_progress" }),
    patchIssue(noneLong.key, { status: "in_progress" }),
    patchIssue(backlogLong.key, { advances: [seed.ids[0]], status: "backlog" }),
  ]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize(DESKTOP);
    const soft = expect.configure({ soft: true });
    // Whether the title in `row` is drawn whole: nothing from the title up to its cell holds
    // more than its box shows, which is what an ellipsis or a clamp needs.
    const expectWhole = async (table: Locator, key: string, where: string) => {
      const row = table.getByRole("row", { exact: true, name: `${key} ${title}` });
      // The row first: a tab just chosen draws its table's headers with its rows, not before.
      await expect(row).toBeVisible();
      const text = (await cellUnder(table, row, "Title")).getByText(title, { exact: true });
      await expect(text).toBeVisible();
      await soft
        .poll(
          () =>
            text.evaluate((element) => {
              const cut: string[] = [];
              const cell = element.closest("td");
              for (let node: Element | null = element; node !== null; node = node.parentElement) {
                if (node.scrollWidth > node.clientWidth || node.scrollHeight > node.clientHeight)
                  cut.push(
                    `${node.tagName.toLowerCase()} ${node.clientWidth}x${node.clientHeight}`
                  );
                if (node === cell) break;
              }
              return cut;
            }),
          { message: `${where}: the 190-character title reads whole` }
        )
        .toEqual([]);
    };
    // Whether the 900-character title in `key`'s row is drawn in eight lines ending in '…' - a
    // clamp of eight lines that holds more than it shows - with its whole text in a tooltip of
    // its own or of an element around it in its cell.
    const expectEightLines = async (table: Locator, key: string, where: string) => {
      const row = table.getByRole("row", { exact: true, name: `${key} ${longTitle}` });
      await expect(row).toBeVisible();
      const text = (await cellUnder(table, row, "Title")).getByText(longTitle, { exact: true });
      await expect(text).toBeVisible();
      const drawn = await settledValue(`${where}: the 900-character title`, () =>
        text.evaluate((element) => {
          const style = getComputedStyle(element);
          const lines = Math.round(
            element.getBoundingClientRect().height / Number.parseFloat(style.lineHeight)
          );
          const cell = element.closest("td");
          let tooltip = "";
          for (let node: Element | null = element; node !== null; node = node.parentElement) {
            const own = node.getAttribute("title");
            if (own !== null && own !== "") {
              tooltip = own;
              break;
            }
            if (node === cell) break;
          }
          return {
            endsInEllipsis:
              style.webkitLineClamp === "8" && element.scrollHeight > element.clientHeight,
            lines,
            tooltip,
          };
        })
      );
      soft(
        { endsInEllipsis: drawn.endsInEllipsis, lines: drawn.lines },
        `${where}: the 900-character title is drawn in eight lines ending in '…'`
      ).toEqual({ endsInEllipsis: true, lines: 8 });
      soft(drawn.tooltip, `${where}: the 900-character title's whole text is in its tooltip`).toBe(
        longTitle
      );
    };

    // A priority's page.
    await page.goto(`/priorities/${seed.ids[0]}`);
    const workTable = section(page, "Work advancing this priority");
    await expectRowNames(workTable, [
      `${seed.rollout} Daemon rollout to the fleet`,
      `${seed.smoke} Daemon smoke suite`,
      `${work.key} ${title}`,
      `${workLong.key} ${longTitle}`,
      // The ones in backlog advance #1 as well, so the work under #1 lists them.
      `${inBacklog.key} ${title}`,
      `${backlogLong.key} ${longTitle}`,
    ]);
    await expect.poll(() => tablesOverflow(page)).toEqual({ "Priority issues table": 0 });

    // The room between the right edge of the last column that draws anything - a header's words
    // or a cell's visible element - and the box's inner right edge.
    const emptyRoom = () =>
      workTable.locator('[data-testid$="-table-scroll"]').evaluate((box) => {
        // A cell or header draws something where an element in it is on screen and holds words,
        // or is a control or a picture, which hold none.
        const drawn = (element: Element) =>
          [element, ...element.querySelectorAll("*")].some((node) => {
            const style = getComputedStyle(node);
            const rect = node.getBoundingClientRect();
            return (
              rect.width > 0 &&
              rect.height > 0 &&
              style.visibility !== "hidden" &&
              style.opacity !== "0" &&
              ((node.textContent ?? "").trim() !== "" || node.matches("input, button, svg, img"))
            );
          });
        const headers = [...box.querySelectorAll("thead th")];
        const rows = [...box.querySelectorAll("tbody tr[data-issue-row]")];
        let last = -1;
        headers.forEach((header, at) => {
          const cells = rows.map((row) => row.querySelectorAll("td")[at]);
          if (drawn(header) || cells.some((cell) => cell !== undefined && drawn(cell))) last = at;
        });
        const inner = box.getBoundingClientRect().left + box.clientLeft + box.clientWidth;
        const right =
          headers[last]?.getBoundingClientRect().right ?? box.getBoundingClientRect().left;
        return Math.round(inner - right);
      });
    await soft.poll(emptyRoom).toBeLessThanOrEqual(20);
    await expectWhole(workTable, work.key, "a priority's page");
    await expectEightLines(workTable, workLong.key, "a priority's page");

    // The two fault tabs.
    await page.goto(PRIORITIES_PATH);
    await chooseTab(page, "none");
    const noneTable = page.getByRole("table", { exact: true, name: NO_PRIORITY_TABLE });
    await expectWhole(noneTable, none.key, "the none tab");
    await expectEightLines(noneTable, noneLong.key, "the none tab");
    await chooseTab(page, "backlog");
    const backlogTable = page.getByRole("table", { exact: true, name: BACKLOG_TABLE });
    await expectWhole(backlogTable, inBacklog.key, "the backlog-or-icebox tab");
    await expectEightLines(backlogTable, backlogLong.key, "the backlog-or-icebox tab");
  } finally {
    await context.close();
  }
});

// INFRA-2947 R7 (spec v82, v85) and check 138e9e4d (Verification v62), read at 1280 by 800 on
// data shaped as production's: an owner of 43 characters, drawn whole; a description of several
// paragraphs whose first line is short and names an issue by its Dispatch reference, which a
// closed row draws as its start, the paragraphs run together, in at most two lines ending in '…',
// the reference read as the short name Dispatch's documents give it, and an open row draws whole;
// on every table of these pages the header line and each row's line reaching the right border of
// the table's box; on each toolbar of these pages every control at one height; and no margin
// entry for the visibility a priority is written with, which no control on the page sets.
// SLOW(>=2s): the seed, one more priority, and the three tabs and one priority's page read.
test("at 1280 by 800 the priorities pages draw production's owners and descriptions, reach their boxes' right borders, keep each toolbar at one height, and list no visibility in the margin", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const owner = "The INFRA controller, for its human sponsor";
  expect(owner).toHaveLength(43);
  const reference = `dispatch://${seed.rollout}`;
  const firstLine = `Start with ${reference}.`;
  const rest =
    "Every reader of this deployment reaches the same corpus in one command, on any developer " +
    "machine, and the pages both review layers read are the ones a person opens. The work under " +
    "it is the seeding, the sign-in, the pages and the checks that read them, and it is finished " +
    "when a reader who has never run the stack can start it, sign in and read the same " +
    "priorities page the visual review reads, from one command, with nothing to install by hand " +
    "and nothing to ask anybody for.";
  const description = `${firstLine}\n\n${rest}`;
  const long = await createPriority({ description, owner, title: "Local testing is one command" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize(DESKTOP);
    const soft = expect.configure({ soft: true });

    // What of an element's text its boxes cut, from the element up to its table cell: each box
    // that holds more than it shows, which is what an ellipsis, a clamp or a clip needs.
    const cutBoxes = (locator: Locator) =>
      locator.evaluate((element) => {
        const cut: string[] = [];
        const cell = element.closest("td");
        for (let node: Element | null = element; node !== null; node = node.parentElement) {
          if (node.scrollWidth > node.clientWidth || node.scrollHeight > node.clientHeight)
            cut.push(`${node.tagName.toLowerCase()} ${node.clientWidth}x${node.clientHeight}`);
          if (node === cell) break;
        }
        return cut;
      });
    // How far short of its box's right border each table's lines stop: the header row's, and the
    // widest a body row reaches.
    const linesShort = () =>
      page.locator("table").evaluateAll((tables) =>
        Object.fromEntries(
          tables.map((table) => {
            const box = table.closest('[data-testid$="-table-scroll"]');
            if (box === null) throw new Error("a table on the page stands in no scroll box");
            const inner = box.getBoundingClientRect().left + box.clientLeft + box.clientWidth;
            const right = (cells: Element[]) =>
              Math.max(...cells.map((cell) => cell.getBoundingClientRect().right));
            const header = right([...table.querySelectorAll("thead th")]);
            const rows = [...table.querySelectorAll("tbody tr")]
              .map((row) => [...row.querySelectorAll("td")])
              .filter((cells) => cells.length > 0)
              .map(right);
            return [
              table.getAttribute("aria-label") ?? "",
              {
                header: Math.max(0, Math.round(inner - header)),
                rows: rows.length === 0 ? 0 : Math.max(0, Math.round(inner - Math.min(...rows))),
              },
            ];
          })
        )
      );
    // Each toolbar's controls by their heights: the row the table's search box stands in, and
    // every input, select and button drawn in it.
    const toolbarHeights = () =>
      page.locator('input[type="search"]').evaluateAll((boxes) =>
        boxes
          .filter((box) => box.getBoundingClientRect().height > 0)
          .map((box) =>
            [...(box.parentElement?.querySelectorAll("input, select, button") ?? [])]
              .filter((control) => control.getBoundingClientRect().height > 0)
              .map((control) => ({
                height: control.getBoundingClientRect().height,
                // A select by the words of the label it stands in (`Group by`), not its options.
                name: (
                  control.getAttribute("aria-label") ??
                  (control.tagName === "SELECT"
                    ? control.closest("label")?.firstChild?.textContent
                    : control.textContent) ??
                  ""
                )
                  .trim()
                  .slice(0, 20),
              }))
          )
      );
    // One height, to within a pixel of rounding.
    const oneHeight = (controls: readonly { height: number }[]) =>
      Math.max(...controls.map((c) => c.height)) - Math.min(...controls.map((c) => c.height)) <= 1;
    const expectTablesAndToolbars = async (where: string) => {
      const short = await settledValue(`${where}: the tables' lines`, linesShort);
      for (const [table, gap] of Object.entries(short)) {
        soft(
          gap.header <= 1 && gap.rows <= 1,
          `${where}: the ${table}'s header line and row lines reach its box's right border (short by ${JSON.stringify(gap)} px)`
        ).toBe(true);
      }
      for (const controls of await settledValue(`${where}: the toolbars`, toolbarHeights)) {
        const heights = controls.map((c) => `${c.name} ${Math.round(c.height)}`).join(", ");
        soft(
          oneHeight(controls),
          `${where}: every control of the toolbar at one height: ${heights}`
        ).toBe(true);
      }
    };

    // The Priorities tab.
    await page.goto(PRIORITIES_PATH);
    const table = page.getByRole("table", { exact: true, name: "Priorities table" });
    const row = table.getByRole("row", { exact: true, name: `#4 ${long.title}` });
    await expect(row).toBeVisible();
    await expect.poll(() => tablesOverflow(page)).toEqual({ "Priorities table": 0 });
    // The owner, whole.
    const ownerText = (await cellUnder(table, row, "Owner")).getByText(owner, { exact: true });
    await expect(ownerText).toBeVisible();
    soft(await cutBoxes(ownerText), "the 43-character owner is drawn whole").toEqual([]);
    // The description, closed: its start, the paragraphs run together, in at most two lines
    // ending in '…' where it runs on - a clamp of two lines whose text holds more than it shows,
    // or a text cut and ended in '…' - with the reference read as its short name, the issue's key
    // or its title, and never as its `dispatch://` address.
    const summary = (await cellUnder(table, row, "Priority")).getByText(/^Start with/).first();
    await expect(summary).toBeVisible();
    const closed = await settledValue("the closed row's excerpt", () =>
      summary.evaluate((element) => {
        const style = getComputedStyle(element);
        const lines = Math.round(
          element.getBoundingClientRect().height / Number.parseFloat(style.lineHeight)
        );
        const clamped =
          style.webkitLineClamp === "2" && element.scrollHeight > element.clientHeight;
        const text = (element.textContent ?? "").trim();
        return { atMostTwoLines: lines <= 2, endsInEllipsis: clamped || text.endsWith("…"), text };
      })
    );
    soft(
      { atMostTwoLines: closed.atMostTwoLines, endsInEllipsis: closed.endsInEllipsis },
      "a closed row draws the description in at most two lines ending in '…'"
    ).toEqual({ atMostTwoLines: true, endsInEllipsis: true });
    soft(
      closed.text,
      "a closed row runs the paragraphs together: the second follows the short first line"
    ).toContain("Every reader of this deployment");
    soft(
      {
        address: closed.text.includes("dispatch://"),
        shortName:
          closed.text.includes(seed.rollout) || closed.text.includes("Daemon rollout to the fleet"),
      },
      `a closed row reads the reference by its short name (${closed.text.slice(0, 80)})`
    ).toEqual({ address: false, shortName: true });
    // Open, the row draws the whole of it: both paragraphs.
    await row.getByRole("button", { name: new RegExp(`^Open #4 ${long.title}$`) }).click();
    const opened = page.getByRole("textbox", { exact: true, name: "Description of #4" });
    await soft(opened).toContainText("Start with");
    await soft(opened).toContainText(rest);
    await expectTablesAndToolbars("the Priorities tab");

    // The two fault tabs.
    for (const tab of ["none", "backlog"] as const) {
      await chooseTab(page, tab);
      await expect(page.getByRole("table")).toHaveCount(1);
      await expect(page.locator("tbody tr[data-issue-row]").first()).toBeVisible();
      await expectTablesAndToolbars(`the ${tab} tab`);
    }

    // A priority's own page: its work table and toolbar.
    await page.goto(`/priorities/${seed.ids[0]}`);
    const work = section(page, "Work advancing this priority");
    await expectRowNames(work, [
      `${seed.rollout} Daemon rollout to the fleet`,
      `${seed.smoke} Daemon smoke suite`,
    ]);
    await expectTablesAndToolbars("a priority's page");

    // The margin of the priority written once above: one entry for each act its history holds
    // but the visibility it was written with, which no control on the page sets. Each of those
    // acts writes another field, so no run folds them, and the entries are read by their count
    // rather than by the words an entry would carry, which this clause does not fix.
    const acts = await getPriorityHistory(long.id);
    expect(acts.some((act) => act.field === "visibility")).toBe(true);
    const fields = acts.map((act) => act.field);
    expect(new Set(fields).size).toBe(fields.length);
    await page.goto(`/priorities/${long.id}`);
    const entries = page.getByRole("complementary", { name: "Margin" }).getByRole("listitem");
    await expect(entries.first()).toBeVisible();
    const shown = await settledValue("the margin's entries", () => entries.count());
    soft(
      shown,
      `the margin lists one entry for each act but the visibility (${acts.length} acts: ${fields.join(", ")})`
    ).toBe(acts.filter((act) => act.field !== "visibility").length);
  } finally {
    await context.close();
  }
});

// INFRA-2947 R7 (spec v83 to v85) and check 138e9e4d (Verification v60 to v62), read on a phone,
// 390 by 844: the priorities tab draws each priority as its place beside one cell, which holds
// its title, the start of its description, its owner, its three counts under their column names
// and its newest event, and no other column, so the table does not scroll sideways. So each
// column's header label and its controls stand on one line; the cell of the grip, the place and
// the disclosure is no wider than 80 px; each row's owner, counts and newest event are drawn
// inside the box; and the table's scroll width is its client width. And (spec v84, Verification
// v61) no two controls of the header row have 44 px touch targets that overlap, and the `#`
// column offers its sort and no other control.
// SLOW(>=2s): the seed and one page read at a phone's size.
test("on a phone the priorities tab keeps each place and title together as wide as the box, under headers on one line", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize(PHONE);
    await page.goto(PRIORITIES_PATH);
    const table = page.getByRole("table", { exact: true, name: "Priorities table" });
    await expectRowOrder(table, [
      `#1 ${seed.titles[0]}`,
      `#2 ${seed.titles[1]}`,
      `#3 ${seed.titles[2]}`,
    ]);
    const soft = expect.configure({ soft: true });

    // Each header whose label and controls - the sort button with its arrow, and the filter -
    // stand on more than one line: the parts whose boxes share no band across the header.
    const headersOnTwoLines = () =>
      table.locator("thead th").evaluateAll((headers) =>
        headers.flatMap((header) => {
          const parts: DOMRect[] = [];
          for (const part of header.querySelectorAll(
            'button[title^="Sort"], [aria-label^="Filter"]'
          )) {
            const range = document.createRange();
            range.selectNodeContents(part);
            parts.push(part.getBoundingClientRect(), ...range.getClientRects());
          }
          const drawn = parts.filter((rect) => rect.width > 0 && rect.height > 0);
          if (drawn.length === 0) return [];
          const top = Math.max(...drawn.map((rect) => rect.top));
          const bottom = Math.min(...drawn.map((rect) => rect.bottom));
          return top < bottom ? [] : [(header.textContent ?? "").trim()];
        })
      );
    soft(
      await settledValue("the headers' lines", headersOnTwoLines),
      "each header's label and controls stand on one line"
    ).toEqual([]);

    // The room #1's grip, place and disclosure take at the row's start: from the left edge of the
    // grip's cell to the right edge of whichever of the three ends last - its cell, or the
    // disclosure where the row draws it in the next cell.
    const row = table.getByRole("row", { exact: true, name: `#1 ${seed.titles[0]}` });
    const disclosure = row.getByRole("button", { name: `Open #1 ${seed.titles[0]}` });
    const placeWidth = async () => {
      const grip = await row.getByRole("button", { name: /^Move #1 / }).evaluate((element) => {
        const cell = element.closest("td")?.getBoundingClientRect();
        return { left: cell?.left ?? 0, right: cell?.right ?? 0 };
      });
      const opener = await disclosure.evaluate((element) => element.getBoundingClientRect().right);
      return Math.round(Math.max(grip.right, opener) - grip.left);
    };
    const place = await settledValue("the place's room", placeWidth);
    soft(
      place,
      `the grip, the place and the disclosure take no more than 80 px (${place} px)`
    ).toBeLessThanOrEqual(80);

    // Each row's owner, its three counts under their column names and its newest event, drawn
    // inside the box as it stands at its start: each is in the row, and nothing the row draws
    // lies past the box's inner edges.
    const owners = ["alice", "bob", "alice"];
    const rowsInBox = () =>
      table.evaluate((element, owned) => {
        const box = element.closest('[data-testid$="-table-scroll"]');
        if (box === null) throw new Error("the priorities table stands in no scroll box");
        box.scrollLeft = 0;
        const outer = box.getBoundingClientRect();
        const left = outer.left + box.clientLeft;
        const right = left + box.clientWidth;
        const inside = (node: Element) => {
          const rect = node.getBoundingClientRect();
          return rect.left >= left - 0.5 && rect.right <= right + 0.5;
        };
        const shown = (node: Element) => {
          const rect = node.getBoundingClientRect();
          const style = getComputedStyle(node);
          return rect.width > 0 && rect.height > 0 && style.visibility !== "hidden";
        };
        return [...element.querySelectorAll("tbody tr[data-issue-row]")].map((row, at) => {
          const leaves = [...row.querySelectorAll("*")].filter(
            (node) =>
              shown(node) && node.children.length === 0 && (node.textContent ?? "").trim() !== ""
          );
          const named = (words: string) =>
            leaves.some((node) => (node.textContent ?? "").trim() === words && inside(node));
          const times = [...row.querySelectorAll("time")].filter(shown);
          return {
            row: at + 1,
            owner: named(owned[at] ?? ""),
            counts: ["Active", "Triage, Todo", "Backlog, icebox"].every((name) =>
              leaves.some(
                (node) => (node.textContent ?? "").trim().startsWith(name) && inside(node)
              )
            ),
            newestEvent: times.length > 0 && times.every(inside),
            outside: leaves
              .filter((node) => !inside(node))
              .map((node) => (node.textContent ?? "").trim().slice(0, 20)),
          };
        });
      }, owners);
    const rows = await settledValue("each row's parts in the box", rowsInBox);
    soft(
      rows,
      "each row's owner, its three counts under their names and its newest event are drawn inside the box"
    ).toEqual(
      owners.map((_, at) => ({
        counts: true,
        newestEvent: true,
        outside: [],
        owner: true,
        row: at + 1,
      }))
    );
    // The table does not scroll sideways: its box's scroll width is its client width.
    const sideways = () =>
      table.evaluate((element) => {
        const box = element.closest('[data-testid$="-table-scroll"]');
        return box === null ? Number.POSITIVE_INFINITY : box.scrollWidth - box.clientWidth;
      });
    soft(
      await settledValue("the box's scroll width", sideways),
      "the table's scroll width is its client width"
    ).toBe(0);
    // Verification v64: the tab offers no Columns control on a phone, where it draws no column
    // to choose; and the box's right border is the one line at the table's right, so no cell of
    // the last column draws a right border of its own beside it.
    await soft(
      page.getByRole("tabpanel").getByRole("button", { exact: true, name: "Columns" }),
      "the priorities tab offers no Columns control on a phone"
    ).toHaveCount(0);
    const rightLines = () =>
      table.evaluate((element) => {
        const cells = [...element.querySelectorAll("th, td")].filter(
          (cell) => cell.getBoundingClientRect().width > 0
        );
        const right = Math.max(...cells.map((cell) => cell.getBoundingClientRect().right));
        return cells
          .filter((cell) => {
            const style = getComputedStyle(cell);
            return (
              cell.getBoundingClientRect().right >= right - 0.5 &&
              Number.parseFloat(style.borderRightWidth) > 0 &&
              style.borderRightStyle !== "none" &&
              !/rgba\(.*,\s*0\)$/.test(style.borderRightColor) &&
              style.borderRightColor !== "transparent"
            );
          })
          .map(
            (cell) =>
              `${cell.tagName.toLowerCase()} ${(cell.textContent ?? "").trim().slice(0, 16)}`
          );
      });
    soft(
      await settledValue("the last column's right borders", rightLines),
      "one line at the box's right border: no cell of the last column draws its own"
    ).toEqual([]);

    // The header row's controls, each by its 44 px touch target: its box, grown about its centre
    // to 44 px where it is smaller. No two of them may overlap, and the `#` header holds one
    // control, its sort.
    const headerTargets = () =>
      table.locator("thead").evaluate((head) => {
        const controls = [
          ...head.querySelectorAll("button, input, select, a[href], [role='button']"),
        ]
          .map((control) => ({ control, rect: control.getBoundingClientRect() }))
          .filter(({ rect }) => rect.width > 0 && rect.height > 0);
        const targets = controls.map(({ control, rect }) => {
          const width = Math.max(44, rect.width);
          const height = Math.max(44, rect.height);
          const x = rect.left + rect.width / 2 - width / 2;
          const y = rect.top + rect.height / 2 - height / 2;
          const header = (control.closest("th")?.textContent ?? "").trim().slice(0, 16);
          const name = (
            control.getAttribute("aria-label") ??
            control.getAttribute("title") ??
            control.textContent ??
            ""
          )
            .trim()
            .slice(0, 16);
          return {
            header,
            name,
            x,
            y,
            right: x + width,
            bottom: y + height,
            sort: control.matches('button[title^="Sort"]'),
          };
        });
        const overlaps: string[] = [];
        for (const [at, one] of targets.entries()) {
          for (const other of targets.slice(at + 1)) {
            const across = Math.min(one.right, other.right) - Math.max(one.x, other.x);
            const down = Math.min(one.bottom, other.bottom) - Math.max(one.y, other.y);
            if (across > 0.5 && down > 0.5)
              overlaps.push(
                `${one.header}/${one.name} and ${other.header}/${other.name} by ${Math.round(across)} px`
              );
          }
        }
        const place = targets.filter((target) => target.header.startsWith("#"));
        return {
          overlaps,
          placeControls: place.map((target) => `${target.sort ? "sort" : "other"} ${target.name}`),
        };
      });
    const targets = await settledValue("the header row's touch targets", headerTargets);
    soft(
      targets.overlaps,
      "no two controls' 44 px touch targets in the header row overlap"
    ).toEqual([]);
    soft(
      targets.placeControls.map((control) => control.split(" ")[0]),
      `the # column's sort is the only control of its header (${targets.placeControls.join(", ")})`
    ).toEqual(["sort"]);
    await expect.poll(() => documentOverflow(page)).toBeLessThanOrEqual(0);
  } finally {
    await context.close();
  }
});

// INFRA-2947 R7 (spec v86 to v98) and check 138e9e4d (Verification v64 to v73), read on data
// shaped as production's: a holder's claim chip on each fault tab drawn whole at 1280, its words
// no wider than its box, for a holder named by a long session title, by a session title of 55
// characters led by an emoji, and by a bare session id; at 1280 Title at least 260 px wide on
// the Active tab and 200 px on the backlog-or-icebox tab with the seed's own keys, and at least
// 240 px and 155 px with an issue key of 12 characters among the rows, unsorted and sorted by
// each column that sorts; on both no other column wider than its widest header or cell
// needs, with its padding; each column header's sort arrow clear of its filter; and the status
// bar over the work
// under a priority, at 1280 and at 390, and over each fault tab at 390, cutting no status's
// name - each name is whole in its segment, in view, or whole in a legend under the bar - and at
// 390 drawing every count inside the bar.
// SLOW(>=2s): the seed, twenty-four more issues, six claims, each header sorted, and three pages
// read at two sizes.
test("the fault tabs draw a holder's claim whole, and no status bar of these pages cuts a status's name", async ({
  browser,
}) => {
  // Measured in this pod: 42.3 s (2026-11-22), 57.6 s at ccd4a40c, and the suite's 60 s budget hit
  // once at c92acaa1 before its retry passed in 38.7 s: the budget leaves this row no margin.
  test.slow();
  const seed = await seedPriorities();
  // Under #1, beside its rollout in flight and its smoke suite waiting, one issue in each other
  // status a priority's work stands in, so its bar holds eight segments, as production's do.
  const statuses = ["triage", "testing", "needs_review", "retro", "backlog", "icebox"] as const;
  for (const status of statuses) {
    const issue = await createIssue({ project: "CORE", title: `Under #1 in ${status}` });
    await patchIssue(issue.key, { advances: [seed.ids[0]], status });
  }
  // A claimed issue on each fault tab for each of three holders shaped as production's are: a
  // session named by a long title, one by a title of 55 characters led by an emoji, and one
  // named by its bare id, which carries no title.
  const emoji = "🚀 Legion implementer · INFRA-2947 · build host, rack 42";
  expect([...emoji]).toHaveLength(55);
  const holders = [
    {
      id: "019a1c0f-6d4b-7e80-bf39-4a1b5c8d9e0f",
      kind: "session" as const,
      origin: { session_title: "Legion implementer · INFRA-2947 · build host" },
    },
    {
      id: "019a1c10-7e5c-7f91-8a4b-5b2c6d9e0f10",
      kind: "session" as const,
      origin: { session_title: emoji },
    },
    { id: "019a1c11-8f6d-7aa2-9b5c-6c3d7e0f1a11", kind: "session" as const },
  ];
  const claimed: { holder: string; backlog: string; unplaced: string }[] = [];
  for (const [at, holder] of holders.entries()) {
    const unplaced = await createIssue({
      project: "OPS",
      title: `Claimed under no priority ${at + 1}`,
    });
    const inBacklog = await createIssue({
      project: "WEB",
      title: `Claimed and in backlog under #1 ${at + 1}`,
    });
    await patchIssue(unplaced.key, { status: "in_progress" });
    await patchIssue(inBacklog.key, { advances: [seed.ids[0]], status: "backlog" });
    for (const key of [unplaced.key, inBacklog.key]) {
      await claimIssue(key, { actor: holder, as: "agent" });
    }
    claimed.push({
      holder: holder.origin?.session_title ?? holder.id,
      backlog: inBacklog.key,
      unplaced: unplaced.key,
    });
  }
  // A key of 12 characters on each fault tab, as a project's keys grow by digits: INFRAWIDE-11
  // in backlog under #1 and INFRAWIDE-12 in flight under no priority. Written once the tabs are
  // read with the seed's own keys.
  const writeWideKeys = async () => {
    await createProject({ key: "INFRAWIDE", name: "Infra wide" });
    // Twelve titles with no word in common, so Dispatch's near-copy check takes each create.
    const words = [
      "Alpha",
      "Bravo",
      "Charlie",
      "Delta",
      "Echo",
      "Foxtrot",
      "Golf",
      "Hotel",
      "India",
      "Juliet",
      "Kilo",
      "Lima",
    ];
    const keys: string[] = [];
    for (const word of words) {
      keys.push((await createIssue({ project: "INFRAWIDE", title: word })).key);
    }
    const [backlog, active] = [keys[10] ?? "", keys[11] ?? ""];
    expect([backlog, active]).toEqual(["INFRAWIDE-11", "INFRAWIDE-12"]);
    await patchIssue(backlog, { advances: [seed.ids[0]], status: "backlog" });
    await patchIssue(active, { status: "in_progress" });
    return { active, backlog };
  };
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const soft = expect.configure({ soft: true });

    // Each status name the bar under `scope` draws that a reader cannot read whole: cut inside
    // its segment, or with its segment scrolled past the bar's edge, and named whole in no
    // legend under the bar.
    const cutNames = (scope: Locator) =>
      scope.locator('fieldset[aria-label="Issues by status"]').evaluate((bar) => {
        const edge = bar.getBoundingClientRect();
        const left = edge.left + bar.clientLeft;
        const right = left + bar.clientWidth;
        const whole = (element: Element) =>
          element.scrollWidth <= element.clientWidth &&
          element.scrollHeight <= element.clientHeight;
        // A legend a reader sees: under the bar, drawn, and not hidden from sight or from a
        // screen reader, as the bar's own measuring copy of its names is.
        const legend = [...(bar.parentElement?.querySelectorAll("*") ?? [])].filter(
          (element) =>
            !bar.contains(element) &&
            (bar.compareDocumentPosition(element) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0 &&
            element.getBoundingClientRect().width > 0 &&
            getComputedStyle(element).visibility !== "hidden" &&
            element.closest('[aria-hidden="true"]') === null &&
            whole(element)
        );
        return [...bar.querySelectorAll("button")].flatMap((segment) => {
          const name =
            (segment.getAttribute("title") ?? segment.textContent ?? "").split(":")[0]?.trim() ??
            "";
          const rect = segment.getBoundingClientRect();
          const inView = rect.left >= left - 0.5 && rect.right <= right + 0.5;
          if (inView && whole(segment)) return [];
          return legend.some((element) => (element.textContent ?? "").includes(name)) ? [] : [name];
        });
      });
    // Each segment of the bar under `scope` whose count is not drawn inside the bar as it stands
    // at its start: inside the bar's inner box and inside its own segment. A segment's count is
    // the number its tooltip names after the status.
    const countsOutside = (scope: Locator) =>
      scope.locator('fieldset[aria-label="Issues by status"]').evaluate((bar) => {
        bar.scrollLeft = 0;
        const edge = bar.getBoundingClientRect();
        const left = edge.left + bar.clientLeft;
        const right = left + bar.clientWidth;
        return [...bar.querySelectorAll("button")].flatMap((segment) => {
          const label = segment.getAttribute("title") ?? "";
          const count = label.split(":")[1]?.trim() ?? "";
          const box = segment.getBoundingClientRect();
          const walker = document.createTreeWalker(segment, NodeFilter.SHOW_TEXT);
          for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
            const text = node.textContent ?? "";
            const at = text.lastIndexOf(count);
            if (count === "" || at === -1) continue;
            const range = document.createRange();
            range.setStart(node, at);
            range.setEnd(node, at + count.length);
            const rect = range.getBoundingClientRect();
            const inside =
              rect.left >= Math.max(left, box.left) - 0.5 &&
              rect.right <= Math.min(right, box.right) + 0.5;
            return inside ? [] : [label];
          }
          return [`${label} (no count drawn)`];
        });
      });
    // Title's width, and each other column whose header names it - all but the row's checkbox -
    // that stands wider than its widest header or cell needs, with its padding and borders. What
    // a header or cell needs is its content laid out at its max-content width, measured on a
    // hidden copy of it inside the cell, so it takes the cell's own type.
    const columnFit = (table: Locator) =>
      table.evaluate((element) => {
        const need = (cell: Element) => {
          const probe = document.createElement("div");
          probe.style.cssText =
            "position:absolute;left:0;top:0;visibility:hidden;width:max-content;";
          for (const child of cell.childNodes) probe.append(child.cloneNode(true));
          cell.append(probe);
          const width = probe.getBoundingClientRect().width;
          probe.remove();
          const style = getComputedStyle(cell);
          return (
            width +
            Number.parseFloat(style.paddingLeft) +
            Number.parseFloat(style.paddingRight) +
            Number.parseFloat(style.borderLeftWidth) +
            Number.parseFloat(style.borderRightWidth)
          );
        };
        const headers = [...element.querySelectorAll("thead th")];
        const rows = [...element.querySelectorAll("tbody tr[data-issue-row]")];
        let title = 0;
        const wide: string[] = [];
        headers.forEach((header, at) => {
          const name = (header.querySelector('button[title^="Sort"]')?.textContent ?? "").trim();
          const width = header.getBoundingClientRect().width;
          if (name.startsWith("Title")) {
            title = Math.round(width);
            return;
          }
          if (name === "") return;
          const cells = rows.flatMap((row) => {
            const cell = row.querySelectorAll("td")[at];
            return cell === undefined ? [] : [cell];
          });
          const needed = Math.max(need(header), ...cells.map(need));
          // A column's width is a whole number of pixels and its content's need a fraction: the
          // need rounded up, and one pixel for the rounding of the cells beside it.
          if (width > Math.ceil(needed) + 1)
            wide.push(`${name} ${width.toFixed(1)} px, needs ${needed.toFixed(1)}`);
        });
        return { title, wide };
      });

    // The ids of `table`'s columns that sort, and of those that also filter, in their order.
    const sortingColumns = (table: Locator) =>
      table.locator("thead th").evaluateAll((headers) =>
        headers
          .filter((header) => header.querySelector('button[title^="Sort"]') !== null)
          .map((header) => ({
            filters: header.querySelectorAll("button").length > 1,
            id: header.getAttribute("data-column") ?? "",
            words: (header.querySelector('button[title^="Sort"]')?.textContent ?? "").trim(),
          }))
      );
    // Sorts `table` by one column, and waits for its header to say so.
    const sortBy = async (table: Locator, column: string) => {
      const header = table.locator(`thead th[data-column="${column}"]`);
      const sort = header.locator('button[title^="Sort"]');
      const isSorted = async () =>
        /ascending|descending/.test((await header.getAttribute("aria-sort")) ?? "");
      await sort.click();
      // A column the table opens sorted by (the Priorities tab's #) loses its sort on the
      // first click and takes it again on the second.
      await expect
        .poll(isSorted, { timeout: 2000 })
        .toBe(true)
        .catch(() => sort.click());
      await expect(header).toHaveAttribute("aria-sort", /ascending|descending/);
      return header;
    };
    // Title's floor on each fault tab at 1280: unsorted, and then sorted by each column that
    // sorts, in turn (Verification v73).
    const readFloors = async (least: { backlog: number; none: number }, keys: string) => {
      await page.goto(PRIORITIES_PATH);
      for (const [tab, name] of [
        ["none", NO_PRIORITY_TABLE],
        ["backlog", BACKLOG_TABLE],
      ] as const) {
        await chooseTab(page, tab);
        const table = page.getByRole("table", { exact: true, name });
        await expect(table.locator("tr[data-issue-row]").first()).toBeVisible();
        await expect(table.locator("th[aria-sort]")).toHaveCount(0);
        const narrow: string[] = [];
        const unsorted = await settledValue(`the ${tab} tab's columns`, () => columnFit(table));
        if (unsorted.title < least[tab]) narrow.push(`unsorted ${unsorted.title}`);
        // The headers are the column definitions', drawn with the first row awaited above.
        const columns = await sortingColumns(table);
        // The sorted reads are read on a table whose columns sort, Claimed by among them.
        expect(columns.map((column) => column.words)).toContain("Claimed by");
        for (const column of columns) {
          await sortBy(table, column.id);
          const fit = await settledValue(`the ${tab} tab sorted by ${column.words}`, () =>
            columnFit(table)
          );
          if (fit.title < least[tab]) narrow.push(`sorted by ${column.words} ${fit.title}`);
        }
        soft(
          narrow,
          `at 1280 with ${keys}, unsorted and sorted by each of ${columns.length} columns, Title is at least ${least[tab]} px wide on the ${tab} tab`
        ).toEqual([]);
      }
    };
    // Each header of `table` whose sort arrow, once that column is sorted, is cut by its own
    // button or overlaps its filter: each column that sorts and filters is sorted in turn, and
    // its arrow read.
    const arrowsOverFilters = async (table: Locator) => {
      // The headers are the column definitions', drawn with the rows the caller awaited.
      const columns = (await sortingColumns(table)).filter((column) => column.filters);
      const overlaps: string[] = [];
      for (const { id: column } of columns) {
        const header = await sortBy(table, column);
        const read = await settledValue(`the ${column} header's sort arrow`, () =>
          header.evaluate((element) => {
            const button = element.querySelector('button[title^="Sort"]');
            const filter = [...element.querySelectorAll("button")].find((b) => b !== button);
            const arrow = [...(button?.querySelectorAll("span") ?? [])].find((span) =>
              /^[▲▼]$/.test((span.textContent ?? "").trim())
            );
            if (button === null || filter === undefined || arrow === undefined) return "no arrow";
            const a = arrow.getBoundingClientRect();
            const s = button.getBoundingClientRect();
            const f = filter.getBoundingClientRect();
            const words = (button.textContent ?? "").trim();
            if (a.width === 0 || a.right > s.right + 0.5)
              return `${words}: arrow cut by its button`;
            const meet =
              a.left < f.right - 0.5 &&
              a.right > f.left + 0.5 &&
              a.top < f.bottom - 0.5 &&
              a.bottom > f.top + 0.5;
            return meet ? `${words}: arrow ${Math.round(a.right - f.left)} px over its filter` : "";
          })
        );
        if (read !== "") overlaps.push(read);
      }
      return overlaps;
    };

    // At 1280 with the seed's own keys: Title's floor on each fault tab.
    await page.setViewportSize(DESKTOP);
    await readFloors({ backlog: 200, none: 260 }, "the seed's own keys");

    // At 1280 with a 12-character key among the rows: Title's floor; each other column's fit;
    // each fault tab's claim chip, whole; each header's sort arrow; and the bar over #1's work.
    const wide = await writeWideKeys();
    await readFloors({ backlog: 155, none: 240 }, "a 12-character key");
    await page.goto(PRIORITIES_PATH);
    for (const [tab, name] of [
      ["none", NO_PRIORITY_TABLE],
      ["backlog", BACKLOG_TABLE],
    ] as const) {
      await chooseTab(page, tab);
      const table = page.getByRole("table", { exact: true, name });
      // A row by its own key: its name starts with the key and a space, so OPS-1 is not OPS-12.
      const rowOf = (key: string) => table.locator(`tr[data-issue-row][aria-label^="${key} "]`);
      const first = claimed[0];
      if (first === undefined) throw new Error("no claimed issue was written");
      await expect(rowOf(tab === "none" ? first.unplaced : first.backlog)).toBeVisible();
      await expect(rowOf(tab === "none" ? wide.active : wide.backlog)).toBeVisible();
      const fit = await settledValue(`the ${tab} tab's columns`, () => columnFit(table));
      soft(
        fit.wide,
        `at 1280 no column of the ${tab} tab but Title is wider than its widest header or cell needs`
      ).toEqual([]);
      // The chips that take more than one line on this tab, which the radius clause reads.
      let wrapped = 0;
      for (const one of claimed) {
        const chip = rowOf(tab === "none" ? one.unplaced : one.backlog).getByTestId("issue-claim");
        await expect(chip).toBeVisible();
        const read = await settledValue(`the ${tab} tab's claim chip of ${one.holder}`, () =>
          chip.evaluate((element) => {
            const cell = element.closest("td")?.getBoundingClientRect();
            const parts = [element, ...element.querySelectorAll("*")].filter(
              (part) =>
                part.getBoundingClientRect().width > 0 &&
                (part.scrollWidth > part.clientWidth || part.scrollHeight > part.clientHeight)
            );
            const box = element.getBoundingClientRect();
            // The lines the chip's words take: its text's line boxes grouped by their tops, two
            // boxes on one line when their tops lie within half a line of each other, since the
            // chip's parts align on their centres and their tops can differ by a pixel or two.
            const range = document.createRange();
            range.selectNodeContents(element);
            const half = Number.parseFloat(getComputedStyle(element).lineHeight) / 2 || 8;
            const sorted = [...range.getClientRects()]
              .filter((rect) => rect.width > 0 && rect.height > 0)
              .map((rect) => rect.top)
              .sort((a, b) => a - b);
            const tops = sorted.filter(
              (top, at) => at === 0 || top - (sorted[at - 1] ?? top) > half
            );
            return {
              cut: parts.map(
                (part) =>
                  `${part.tagName.toLowerCase()} ${(part.textContent ?? "").trim().slice(0, 24)}`
              ),
              insideCell: cell !== undefined && box.right <= cell.right + 0.5,
              lines: tops.length,
              radius: Number.parseFloat(getComputedStyle(element).borderTopLeftRadius),
            };
          })
        );
        soft(
          { cut: read.cut, insideCell: read.insideCell },
          `the ${tab} tab draws the claim chip of ${one.holder} whole`
        ).toEqual({ cut: [], insideCell: true });
        // Verification v67: a chip that takes more than one line has a corner radius of at most
        // 8 px, so its fill holds its words; a pill's full radius draws an oval that leaves them
        // outside it.
        if (read.lines > 1) {
          wrapped += 1;
          soft(
            read.radius,
            `the ${tab} tab's claim chip of ${one.holder}, on ${read.lines} lines, has a corner radius of at most 8 px`
          ).toBeLessThanOrEqual(8);
        }
      }
      // On the backlog-or-icebox tab, the narrower of the two, at least one of these holders'
      // chips takes more than one line, so the radius clause is read on a chip it is about.
      if (tab === "backlog") {
        soft(
          wrapped,
          "at least one claim chip of the backlog-or-icebox tab takes more than one line"
        ).toBeGreaterThan(0);
      }
      // Verification v72: each column header's sort arrow, with that column sorted, clear of its
      // filter and not cut by its own button.
      soft(
        await arrowsOverFilters(table),
        `at 1280 each header's sort arrow on the ${tab} tab stands clear of its filter`
      ).toEqual([]);
    }
    // And the Priorities tab's headers, Newest event among them.
    await page.goto(PRIORITIES_PATH);
    const order = page.getByRole("table", { exact: true, name: "Priorities table" });
    await expect(order.getByRole("row").nth(1)).toBeVisible();
    soft(
      await arrowsOverFilters(order),
      "at 1280 each header's sort arrow on the Priorities tab stands clear of its filter"
    ).toEqual([]);
    await page.goto(`/priorities/${seed.ids[0]}`);
    const work = section(page, "Work advancing this priority");
    await expect(work.getByRole("row", { name: /Under #1 in retro$/ })).toBeVisible();
    soft(
      await settledValue("the bar over #1's work at 1280", () => cutNames(work)),
      "at 1280 the bar over a priority's work cuts no status's name"
    ).toEqual([]);

    // At 390: the same bar, and each fault tab's.
    await page.setViewportSize(PHONE);
    await page.reload();
    await expect(work.getByRole("row", { name: /Under #1 in retro$/ })).toBeVisible();
    soft(
      await settledValue("the bar over #1's work at 390", () => cutNames(work)),
      "at 390 the bar over a priority's work cuts no status's name"
    ).toEqual([]);
    soft(
      await settledValue("the counts of the bar over #1's work at 390", () => countsOutside(work)),
      "at 390 the bar over a priority's work draws every count inside the bar"
    ).toEqual([]);
    await page.goto(PRIORITIES_PATH);
    for (const tab of ["none", "backlog"] as const) {
      await chooseTab(page, tab);
      const panel = page.getByRole("tabpanel");
      await expect(panel.locator("tbody tr[data-issue-row]").first()).toBeVisible();
      soft(
        await settledValue(`the ${tab} tab's bar at 390`, () => cutNames(panel)),
        `at 390 the ${tab} tab's bar cuts no status's name`
      ).toEqual([]);
      soft(
        await settledValue(`the counts of the ${tab} tab's bar at 390`, () => countsOutside(panel)),
        `at 390 the ${tab} tab's bar draws every count inside the bar`
      ).toEqual([]);
    }
  } finally {
    await context.close();
  }
});

// INFRA-2947 R15 and check 0abe1d81 (Verification v69): a project's Issues tab, the one page
// outside the priorities pages this change reaches. At 1280 by 800 the search box, each select,
// Columns, Needs you and Unread each stand 34 px high; at 900 px wide the search box takes a row
// of its own above the other controls; and at 390 by 844 each of those controls is at least
// 44 px high.
// SLOW(>=2s): a project and one issue written, and the tab read at three sizes.
test("a project's Issues tab draws its toolbar 34 px high at 1280, its search box on a row of its own at 900, and each control 44 px or more on a phone", async ({
  browser,
}) => {
  await createProject({ key: "TOOLS", name: "Tools" });
  await createIssue({ project: "TOOLS", title: "An issue the table draws" });
  // Each control of the toolbar the search box stands in, by its name and its box: the box
  // itself, the select by the words of the label it stands in (`Group by`), and each button.
  const toolbar = (page: Page) =>
    page.getByRole("searchbox", { exact: true, name: "Search issues" }).evaluate((box) =>
      [...(box.parentElement?.querySelectorAll("input, select, button") ?? [])]
        .filter((control) => control.getBoundingClientRect().height > 0)
        .map((control) => {
          const rect = control.getBoundingClientRect();
          return {
            bottom: rect.bottom,
            height: rect.height,
            name: (
              control.getAttribute("aria-label") ??
              (control.tagName === "SELECT"
                ? control.closest("label")?.firstChild?.textContent
                : control.textContent) ??
              ""
            ).trim(),
            top: rect.top,
          };
        })
    );
  const named = ["Search issues", "Group by", "Columns", "Needs you", "Unread"];
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const soft = expect.configure({ soft: true });
    const search = page.getByRole("searchbox", { exact: true, name: "Search issues" });
    const read = async (size: { height: number; width: number }, where: string) => {
      await page.setViewportSize(size);
      await page.goto("/projects/TOOLS/issues");
      await expect(search).toBeVisible();
      const controls = await settledValue(`${where}: the Issues tab's toolbar`, () =>
        toolbar(page)
      );
      // Each control the check names is drawn, so no clause below passes on a missing one.
      soft(
        named.filter((name) => !controls.some((control) => control.name.startsWith(name))),
        `${where}: the toolbar draws ${named.join(", ")}`
      ).toEqual([]);
      return controls;
    };

    // At 1280 by 800, each control 34 px high, to within half a pixel of rounding.
    const desktop = await read(DESKTOP, "at 1280");
    soft(
      desktop
        .filter((control) => Math.abs(control.height - 34) > 0.5)
        .map((control) => `${control.name.slice(0, 20)} ${Math.round(control.height)}`),
      "at 1280 every control of the Issues tab's toolbar is 34 px high"
    ).toEqual([]);

    // At 900 px wide, the search box on a row of its own, above every other control.
    const narrow = await read({ height: 800, width: 900 }, "at 900");
    const box = narrow.find((control) => control.name === "Search issues");
    if (box === undefined) throw new Error("at 900 the toolbar draws no search box");
    soft(
      narrow
        .filter((control) => control !== box && control.top < box.bottom - 0.5)
        .map((control) => control.name.slice(0, 20)),
      "at 900 the search box takes a row of its own above the other controls"
    ).toEqual([]);

    // At 390 by 844, each control at least 44 px high.
    const phone = await read(PHONE, "at 390");
    soft(
      phone
        .filter((control) => control.height < 43.5)
        .map((control) => `${control.name.slice(0, 20)} ${Math.round(control.height)}`),
      "at 390 every control of the Issues tab's toolbar is at least 44 px high"
    ).toEqual([]);
  } finally {
    await context.close();
  }
});

// INFRA-2947 check 8c968bed (Verification v71): what a person reads holds none of the word the
// person refused for work in backlog or icebox. Each of the three tabs, a priority's own page and
// an issue's header, at 1280 by 800 and at 390 by 844: the page's text, every `title` and
// `aria-label` on it, and its address match that word in no case.
// SLOW(>=2s): the seed, and five pages read at two sizes.
test("no tab of the priorities page, no priority's page and no issue header uses the word the person refused", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  // The word, in any case, but not inside "spark", as check 1a3e1452 reads it. The bracket keeps
  // the word itself out of this file.
  const refused = "(?<![sS])p[a]rk";
  // What of the page holds the word: each line of its text, each `title` and `aria-label`, the
  // document's title and its address.
  const holding = (page: Page) =>
    page.evaluate((source) => {
      const word = new RegExp(source, "i");
      const text = document.body.innerText.split("\n").filter((line) => word.test(line));
      const attributes = [...document.querySelectorAll("[title], [aria-label]")].flatMap(
        (element) =>
          ["title", "aria-label"]
            .map((name) => element.getAttribute(name) ?? "")
            .filter((value) => word.test(value))
            .map((value) => `${element.tagName.toLowerCase()} ${value}`)
      );
      const where = [document.title, window.location.href].filter((value) => word.test(value));
      return [...text, ...attributes, ...where].map((value) => value.trim().slice(0, 120));
    }, refused);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const soft = expect.configure({ soft: true });
    for (const [size, where] of [
      [DESKTOP, "at 1280"],
      [PHONE, "at 390"],
    ] as const) {
      await page.setViewportSize(size);
      await page.goto(PRIORITIES_PATH);
      for (const tab of ["order", "none", "backlog"] as const) {
        await chooseTab(page, tab);
        await expect(page.getByRole("table")).toBeVisible();
        soft(
          await settledValue(`${where}: the ${tab} tab`, () => holding(page)),
          `${where}: the ${tab} tab holds no line, title, label or address with the word`
        ).toEqual([]);
      }
      // #3's own page, which lists the archive standing in the backlog under it.
      await page.goto(`/priorities/${seed.ids[2]}`);
      await expect(titleHeading(page)).toHaveText(seed.titles[2] ?? "");
      await expect(
        section(page, "Work advancing this priority").getByRole("row").nth(1)
      ).toBeVisible();
      soft(
        await settledValue(`${where}: #3's page`, () => holding(page)),
        `${where}: a priority's page holds no line, title, label or address with the word`
      ).toEqual([]);
      // The archive's own page, whose header names #3 while the issue stands in the backlog.
      await page.goto(`/issues/${seed.archive}`);
      await expect(page.getByText("Ledger archive").first()).toBeVisible();
      soft(
        await settledValue(`${where}: the archive's page`, () => holding(page)),
        `${where}: an issue's header holds no line, title, label or address with the word`
      ).toEqual([]);
    }
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, two reloads, and two writes read back from the server.
test("what a reader types into a priority survives a reload made before the typing rests", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    // The page's own clock, held while the reader types (`pauseClock`): the half-second timer
    // cannot write then, so only leaving the page can. The reloaded page reads the priorities on
    // its own timers, so its clock runs again once it has loaded.
    await page.clock.install();
    await page.goto(`/priorities/${seed.ids[0]}`);
    const stored = async () =>
      (await listPriorities()).priorities?.find((one) => one.id === seed.ids[0]);

    // The title, typed and the page reloaded at once: inside the half second a write waits for
    // the typing to rest. Leaving by a reload or by closing the tab is leaving the field as much
    // as a click elsewhere is, and what the reader typed is theirs.
    // The title is the header's heading, which a click opens into the box it is written in.
    const title = titleHeading(page);
    await expect(title).toHaveText(seed.titles[0]);
    await pauseClock(page);
    await title.click();
    await expect(page.getByRole("textbox", { name: "Title" })).toBeFocused();
    await page.keyboard.press("End");
    await page.keyboard.type(" now");
    await page.reload();
    await page.clock.resume();
    await expect.poll(async () => (await stored())?.title).toBe(`${seed.titles[0]} now`);

    // The description, the same way.
    const editor = page.getByRole("textbox", { exact: true, name: "Description" });
    await expect(editor).toContainText("nothing is waiting on one");
    await pauseClock(page);
    await editor.click();
    await page.keyboard.press("ControlOrMeta+End");
    await page.keyboard.type(" Typed before the reload.");
    await page.reload();
    await page.clock.resume();
    await expect
      .poll(async () => (await stored())?.description ?? "")
      .toContain("Typed before the reload.");
    await expect(editor).toContainText("Typed before the reload.");
  } finally {
    await context.close();
  }
});

// INFRA-2947 check 3bca8d03, and main's #4835 (a priority's description is drawn from the tree
// the server read it into, so no reader's browser parses it): a person, who may write it, opens a
// priority whose description nests emphasis 4,000 deep - the text #4835 measured at about 55 s of
// the browser's parser - and reads it drawn whole, on the priority's own page and in its open row
// of the priorities page, with no line saying it could not be drawn.
// SLOW(>=2s): a priority written, and two pages read.
test("a person reads a description nested 4,000 deep on its priority's page and in its open row", async ({
  browser,
}) => {
  const deep = await createPriority({ description: NESTED, owner: "alice", title: "Nested deep" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const soft = expect.configure({ soft: true });
    const drawnWhole = async (editor: Locator, where: string) => {
      await soft(editor, `${where} draws the description`).toBeVisible({ timeout: 30_000 });
      // The server's reading holds the text nested past its bound as its own characters, every
      // one of them; the browser's own parse of it would draw 16,001 and lose the stars.
      soft(
        (await editor.allTextContents()).join("").length,
        `${where} draws every character of the server's reading of it`
      ).toBe(NESTED.length);
      await soft(
        page.getByText(/could not be drawn/),
        `${where} says nothing of a description it could not draw`
      ).toHaveCount(0);
    };
    await page.goto(`/priorities/${deep.id}`);
    await drawnWhole(
      page.getByRole("textbox", { exact: true, name: "Description" }),
      "the priority's own page"
    );
    await page.goto(PRIORITIES_PATH);
    await page.getByRole("button", { name: "Open #1 Nested deep" }).click();
    await drawnWhole(
      page.getByRole("textbox", { exact: true, name: "Description of #1" }),
      "its open row"
    );
  } finally {
    await context.close();
  }
});

// INFRA-2947 check 3bca8d03 (review round 4): a writer's editor is filled only from a reading
// that loses nothing. A description Parse refuses - nested past its bound, or holding one block
// it will not store - is drawn read-only to a person who may write it, and a keystroke there
// writes nothing; a description Parse takes keeps its heading, its list and its stars through a
// one-word edit, where a reading of it as literal text would write each back escaped.
// SLOW(>=2s): three priorities written, three pages read, and two writes awaited.
test("a writer's keystroke on a description the editor cannot keep whole writes nothing, and a one-word edit keeps the rest", async ({
  browser,
}) => {
  const withHtml = "## Plan\n\n<div>held as HTML</div>\n\n- first host\n- second host";
  const plain = "## Plan\n\n- first host\n- second host\n\nShip **every** host, *then* check.";
  const deep = await createPriority({ description: NESTED, owner: "alice", title: "Nested deep" });
  const held = await createPriority({ description: withHtml, owner: "alice", title: "Holds HTML" });
  const kept = await createPriority({ description: plain, owner: "alice", title: "Plain plan" });
  const stored = async (id: string) =>
    (await listPriorities()).priorities?.find((row) => row.id === id)?.description;
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const soft = expect.configure({ soft: true });
    // The text a reader sees of `words`, and whether any of it can be typed into: no part of the
    // page that holds those words is editable.
    const readOnly = async (words: string, where: string) => {
      await expect(page.getByText(words, { exact: false }).first()).toBeVisible({
        timeout: 30_000,
      });
      await soft(
        page.locator('[contenteditable="true"]', { hasText: words }),
        `${where} offers its description to no keystroke`
      ).toHaveCount(0);
    };
    // A person's click into the description and a key there, then the focus taken away.
    const typeInto = async (words: string) => {
      await page.getByText(words, { exact: false }).first().click();
      await page.keyboard.press("End");
      await page.keyboard.type("x");
      await page.keyboard.press("Tab");
    };

    // A description Parse takes: one word at its end, and the rest read back as it was written,
    // its blocks of the same kinds and no character of it escaped.
    await page.goto(`/priorities/${kept.id}`);
    const editor = page.getByRole("textbox", { exact: true, name: "Description" });
    await expect(editor).toBeVisible();
    // ProseMirror holds the caret at the end before the first key (`placeCaretAtEnd`). A click
    // and ControlOrMeta+End typed the word where the click fell when ProseMirror's focus handler
    // ran between the key's caret move and its read of that move.
    await placeCaretAtEnd(editor);
    await page.keyboard.type(" Done.");
    await editor.blur();
    await expect.poll(() => stored(kept.id), { timeout: 15_000 }).not.toBe(plain);
    const written = (await stored(kept.id)) ?? "";
    soft(written, "the edit escapes no character").not.toContain("\\");
    soft(written, "the heading is still a heading").toMatch(/^## Plan$/m);
    soft(written, "the list's items are still a list's").toMatch(
      /^[-*] first host\n[-*] second host$/m
    );
    soft(written, "the stars are still stars").toContain(
      "Ship **every** host, *then* check. Done."
    );

    for (const [priority, text, words, where] of [
      // Drawn as the server reads it, its own characters, stars and all.
      [deep, NESTED, "*a *a *a", "a description nested 4,000 deep"],
      [held, withHtml, "first host", "a description holding a block of HTML"],
    ] as const) {
      await page.goto(`/priorities/${priority.id}`);
      await readOnly(words, where);
      await typeInto(words);
      // A save starts on the blur at once and lands within a second here; five seconds after
      // it, a write that was coming has landed, so its absence is read once the wait is over.
      await page.waitForTimeout(5_000);
      soft(await stored(priority.id), `a keystroke in ${where} writes nothing`).toBe(text);
    }
  } finally {
    await context.close();
  }
});

// INFRA-2947 check 3bca8d03: another person's description that the server's parser does not take
// whole - one holding an HTML block - lands while the reader's typing is still unwritten. Her text
// is kept beside his, as it is beside any other text of another person's, and no write of hers
// goes over his: a write that named his text as the one it replaced would put her edit, made on
// the text before his, over a text she never saw.
// SLOW(>=2s): a priority written, her typing held, his write, and the page's answer awaited.
test("another person's description the editor cannot keep whole, landing while the reader types, is not written over", async ({
  browser,
}) => {
  const plain = "## Plan\n\n- first host\n- second host";
  const withHtml = "## Plan\n\n<div>held as HTML</div>\n\n- first host\n- second host";
  const raced = await createPriority({ description: plain, owner: "alice", title: "Raced plan" });
  const stored = async () =>
    (await listPriorities()).priorities?.find((row) => row.id === raced.id)?.description;
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    // The page's own clock, held while she types (`pauseClock`): the half-second timer cannot
    // write then, so her typing is still unwritten when his text lands.
    await page.clock.install();
    await page.goto(`/priorities/${raced.id}`);
    const editor = page.getByRole("textbox", { exact: true, name: "Description" });
    await expect(editor).toContainText("second host");
    await pauseClock(page);
    await editor.click();
    await page.keyboard.press("ControlOrMeta+End");
    await page.keyboard.type(" typed");
    await patchPriority(raced.id, { description: withHtml }, { login: "bob" });
    await page.clock.resume();
    // His text is drawn, read-only, and hers is kept beside it for her to choose.
    await expect(page.getByText("held as HTML").first()).toBeVisible();
    await expect(
      page.getByRole("alert", { name: "Description, changed by another person" })
    ).toContainText("second host typed");
    // No write of hers went over his: once the page has set her text beside his, the server
    // still holds his.
    expect(await stored()).toBe(withHtml);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, a description typed and the tab closed at once, and the server read.
test("what a reader types into a priority's description is written as the tab closes", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    // The page's clock is held while the reader types (`pauseClock`): the half-second timer
    // cannot write then, so only the tab closing can, inside 500 ms of the last key.
    await page.clock.install();
    await page.goto(`/priorities/${seed.ids[0]}`);
    const editor = page.getByRole("textbox", { exact: true, name: "Description" });
    await expect(editor).toContainText("nothing is waiting on one");
    await pauseClock(page);
    await editor.click();
    await page.keyboard.press("ControlOrMeta+End");
    await page.keyboard.type(" Typed before the tab closed.");
    // Closing the tab is leaving the field: the write goes out as the page goes (`pagehide`,
    // sent with `keepalive`), since nothing of the tab is left to send it later.
    await page.close();
    await expect
      .poll(
        async () =>
          (await listPriorities()).priorities?.find((one) => one.id === seed.ids[0])?.description ??
          ""
      )
      .toContain("Typed before the tab closed.");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, a write held until another person's lands, and a reload that resumes one.
test("a priority write another person's change refused leaves no 'Not saved' line and no Retry of the refused text", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    // Every write of #1 the page sends, with its answer: a body the server refused 409
    // PRIORITY_CHANGED names a text it went over, and sending that same body again is refused
    // the same way, whoever presses it.
    const refused: string[] = [];
    page.on("response", (answer) => {
      const sent = answer.request();
      if (sent.method() === "PATCH" && sent.url().endsWith(`/api/v1/priorities/${seed.ids[0]}`)) {
        if (answer.status() === 409) refused.push(sent.postData() ?? "");
      }
    });
    // The title is the header's heading, which a click opens into the box it is written in.
    const heading = titleHeading(page);
    const title = page.getByRole("textbox", { name: "Title" });
    const notice = page.getByRole("alert", { name: "Title, changed by another person" });

    // Bob's rename lands after alice's edit began and before her write reaches the server.
    let first = true;
    await page.route(`**/api/v1/priorities/${seed.ids[0]}`, async (route) => {
      if (route.request().method() === "PATCH" && first) {
        first = false;
        await patchPriority(seed.ids[0], { title: "Bob's rename" }, { login: "bob" });
      }
      await route.continue();
    });
    await page.goto(`/priorities/${seed.ids[0]}`);
    await expect(heading).toHaveText(seed.titles[0]);
    await heading.click();
    await page.keyboard.press("End");
    await page.keyboard.type(" first");
    await title.blur();
    // The notice holds her text. That does not say the page took the 409 - the field draws the
    // same notice when Bob's text reaches the page while her write is still out - so the count
    // waits for that answer itself.
    await expect(notice).toContainText(`${seed.titles[0]} first`);
    await expect.poll(() => refused.length).toBe(1);
    // She uses her text, and it lands over Bob's.
    const used = page.waitForResponse(
      (answer) =>
        answer.request().method() === "PATCH" &&
        answer.url().endsWith(`/api/v1/priorities/${seed.ids[0]}`)
    );
    await notice.getByRole("button", { name: "Use my text" }).click();
    await used;
    await expect
      .poll(
        async () =>
          (await listPriorities()).priorities?.find((one) => one.id === seed.ids[0])?.title
      )
      .toBe(`${seed.titles[0]} first`);
    // The page has taken Dispatch's answer to that write once the shell's line of pending
    // writes, which names the write from its press until its answer is taken, is gone: the
    // answer has reached the page by now, so only then does the absence of a refusal say
    // anything.
    await expect(page.getByTestId("pending-writes")).toHaveCount(0);
    // Her text is written, so nothing on the page says it was not - neither a refusal nor a
    // conflict on the shell's line - and no control on it sends the refused body again.
    await expect(page.getByText(/^Not saved/)).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Retry" })).toHaveCount(0);
    await page.unroute(`**/api/v1/priorities/${seed.ids[0]}`);

    // A write a reload cut off, resumed after Bob wrote again: refused 409. Whatever the page
    // offers then, it does not send that refused body a second time.
    let hold = true;
    await page.route(`**/api/v1/priorities/${seed.ids[0]}`, async (route) => {
      if (route.request().method() === "PATCH" && hold) {
        hold = false;
        return;
      }
      await route.continue();
    });
    await heading.click();
    await page.keyboard.press("End");
    await page.keyboard.type(" second");
    await title.blur();
    await patchPriority(seed.ids[0], { title: "Bob's second rename" }, { login: "bob" });
    await page.reload();
    await expect(heading).toHaveText("Bob's second rename");
    await expect.poll(() => refused.length).toBe(2);
    // The page has taken the resumed write's 409 once its notice holds her text: after the
    // reload no write of hers is out, so only that refusal draws it. Only then does the line's
    // absence say anything.
    await expect(notice).toContainText(`${seed.titles[0]} first second`);
    await expect(page.getByText(/^Not saved/)).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Retry" })).toHaveCount(0);
    expect(new Set(refused).size).toBe(refused.length);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed and three issue pages.
test("an issue's header names the priorities it advances and links to each", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(`/issues/${seed.rollout}`);
    // Each priority is a chip of its place alone, in the header's state row directly after the
    // P0-P3 tag, with its title in its name and its tooltip: no word of the header says
    // "Advances", and no row of the header is the priorities'. The details row under it, which
    // scrolls sideways, holds none of them.
    const header = page.getByTestId("issue-header");
    const stateRow = header.getByTestId("issue-state-actions");
    const line = chipsLine(page);
    const link = line.getByRole("link", { name: `#1 ${seed.titles[0]}` });
    await expect(link).toBeVisible();
    await expect(link).toHaveText("#1");
    await expect(link).toHaveAttribute("title", seed.titles[0]);
    // An own chip is a filled badge with a solid border; an inherited one, below, is dashed.
    await expect(link.locator("span")).toHaveCSS("border-top-style", "solid");
    await expect(header).not.toContainText("Advances");
    const rail = header.getByTestId("issue-metadata-rail");
    await expect(rail.getByTestId("issue-advances")).toHaveCount(0);
    expect(
      await line.evaluate(
        (element) =>
          element.previousElementSibling?.querySelector("select")?.getAttribute("aria-label") ??
          null
      )
    ).toBe(`Priority of ${seed.rollout}`);
    const tagBox = await stateRow.getByLabel(/^Priority of /).boundingBox();
    const lineBox = await line.boundingBox();
    expect(lineBox?.x ?? Number.NaN).toBeGreaterThan(tagBox?.x ?? Number.NaN);
    expect(lineBox?.width ?? Number.POSITIVE_INFINITY).toBeLessThan(120);
    // That row wraps rather than scrolls at 1440 by 900: nothing of it lies past its edge, and the
    // chips stand inside it on the tag's line. Below 768 px the state row has no room left on its
    // first line, so the chips stand first in the details row instead, which scrolls: the first
    // item of that row, inside the viewport with the row unscrolled, and the state row holds none.
    for (const size of [WIDE, PHONE]) {
      await page.setViewportSize(size);
      await expect
        .poll(() =>
          stateRow.evaluate((row) => {
            const style = getComputedStyle(row);
            return {
              overflow: style.overflowX,
              past: row.scrollWidth - row.clientWidth,
              wrap: style.flexWrap,
            };
          })
        )
        .toEqual({ overflow: "visible", past: 0, wrap: "wrap" });
      if (size === PHONE) {
        await expect(line).toHaveCount(0);
        const inRail = rail.getByTestId("issue-advances");
        await expect(inRail.getByRole("link", { name: `#1 ${seed.titles[0]}` })).toBeVisible();
        expect(
          await rail.evaluate(
            (element) => element.firstElementChild?.getAttribute("data-testid") ?? null
          )
        ).toBe("issue-advances");
        expect(await rail.evaluate((element) => element.scrollLeft)).toBe(0);
        const railBox = await rail.boundingBox();
        const inRailBox = await inRail.boundingBox();
        const right = (inRailBox?.x ?? Number.NaN) + (inRailBox?.width ?? 0);
        expect(inRailBox?.x ?? Number.NaN).toBeGreaterThanOrEqual(0);
        expect(right).toBeLessThanOrEqual((railBox?.x ?? Number.NaN) + (railBox?.width ?? 0));
        expect(right).toBeLessThanOrEqual(PHONE.width);
      } else {
        const rowBox = await stateRow.boundingBox();
        const chipsBox = await line.boundingBox();
        expect((chipsBox?.x ?? Number.NaN) + (chipsBox?.width ?? 0)).toBeLessThanOrEqual(
          (rowBox?.x ?? Number.NaN) + (rowBox?.width ?? 0)
        );
        const statusBox = await stateRow.getByLabel("Status", { exact: true }).boundingBox();
        const statusBottom = (statusBox?.y ?? Number.NaN) + (statusBox?.height ?? 0);
        expect(chipsBox?.y ?? Number.NaN).toBeLessThan(statusBottom);
      }
      await expect.poll(() => documentOverflow(page)).toBeLessThanOrEqual(0);
    }
    await priorityPage(page);
    await link.click();
    await expect(page).toHaveURL(`/priorities/${seed.ids[0]}`);
    await expect(titleHeading(page)).toHaveText(seed.titles[0]);

    // An issue that advances none carries no line at all: no label, no name, only the trigger of
    // the picker that places it under a priority.
    await page.goto(`/issues/${seed.clock}`);
    await expect(chipsLine(page).getByRole("link")).toHaveCount(0);
    await expect(chipsLine(page)).not.toContainText("Advances");
    await expect(page.getByRole("button", { name: "Edit priorities" })).toBeVisible();

    // A priority an issue inherits says so, and the picker offers no row that would take it off:
    // that relation is the ancestor's, and a relation on a child places nothing above it. The
    // row the child could add is read first, so the absence is read off a list that has loaded.
    await page.goto(`/issues/${seed.child}`);
    const inherited = chipsLine(page);
    const inheritedChip = inherited.getByRole("link", { name: `#3 ${seed.titles[2]}` });
    await expect(inheritedChip).toBeVisible();
    await expect(inheritedChip).toHaveText("#3");
    await expect(inheritedChip).toHaveAccessibleName(`#3 ${seed.titles[2]}, inherited`);
    await expect(inheritedChip).toHaveAttribute(
      "title",
      `${seed.titles[2]}, inherited from an issue above`
    );
    // It draws unlike an own chip: a dashed, muted border, where an own chip's is solid.
    await expect(inheritedChip.locator("span")).toHaveCSS("border-top-style", "dashed");
    await page.getByRole("button", { name: "Edit priorities" }).click();
    const offered = page.getByRole("listbox", { name: "Priorities options" });
    await expect(
      offered.getByRole("option", { exact: true, name: `#1 ${seed.titles[0]}` })
    ).toBeVisible();
    await expect(offered.getByRole("option", { name: `#3 ${seed.titles[2]}` })).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(offered).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, five writes, and the margin read for its history and for what it lacks.
test("a priority's margin is its history alone, newest first, each act with who made it and when", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  // Two acts after the making, in a known order, so "newest first" is a claim with two ends.
  await patchPriority(seed.ids[0], { owner: "bob" });
  const retitled = "Ship the daemon everywhere, and prove it";
  await patchPriority(seed.ids[0], { title: retitled });
  // Then three saves of the description by one person, one after another, as the page makes
  // them while a reader writes it: the history keeps each of them.
  for (const text of ["Every machine.", "Every machine runs it.", "Every machine runs the daemon."])
    await patchPriority(seed.ids[0], { description: text });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(`/priorities/${seed.ids[0]}`);
    const margin = page.getByRole("complementary", { name: "Margin" });
    const entries = margin.getByRole("listitem");
    // The history has answered once the rename is drawn, so what the margin lacks below is read
    // off a margin that is all there rather than off its skeleton.
    await expect(margin).toContainText(`wrote the title “${retitled}”`);

    // The page carries no tab strip.
    await expect(page.getByRole("tablist")).toHaveCount(0);
    await expect(page.getByRole("tab")).toHaveCount(0);

    // The margin carries no control of states and no view that can only be empty: a priority
    // carries no comments, questions or pins until dispatch://INFRA-1098 makes its description
    // a document (dispatch://INFRA-2947/ask/e31ca6f8-216b-4729-933a-333c949a2a59).
    await expect(margin.getByRole("combobox")).toHaveCount(0);
    await expect(margin.getByRole("listbox")).toHaveCount(0);
    await expect(margin.locator("select")).toHaveCount(0);
    for (const word of ["Words anyone wrote", "Questions", "Pinned"]) {
      await expect(margin).not.toContainText(word);
    }
    // Nor a pin control, nor an entry for a document: the description is no document.
    await expect(margin.getByRole("button", { name: /pin/i })).toHaveCount(0);
    await expect(margin.getByRole("link", { name: /pin|document/i })).toHaveCount(0);
    await expect(margin).not.toContainText(/document/i);

    // The server holds every act, newest first: the three saves of the description, the
    // rename, the owner write, then everything before them. The order is the record's own, not
    // the wall clock's: each write stamps its own moment and the rows are kept in the order they
    // reached the table, so two writes a millisecond apart can carry their moments the other way
    // round, which a comparison of timestamps would read as a failure of an order that is right.
    const acts = await getPriorityHistory(seed.ids[0]);
    expect(acts.slice(0, 5).map((act) => act.field)).toEqual([
      "description",
      "description",
      "description",
      "title",
      "owner",
    ]);
    expect(acts[3]?.value).toBe(retitled);
    expect(acts[4]?.value).toBe("bob");
    // The margin lists them in that order, with a run of writes of one field by one person -
    // here the three saves - read as one entry, its newest. A write of another field, or by
    // another person, ends a run. The visibility the priority was written with is part of
    // "wrote this priority" and no entry of its own, so it is left out before the runs fold
    // (INFRA-2947 R7, check 138e9e4d). So each entry is read against the act the server holds
    // at its place once runs are folded: who made it, and the moment it carries.
    const shown = acts.filter((act) => act.field !== "visibility");
    const listed = shown.filter((act, at) => {
      const newer = shown[at - 1];
      return (
        newer === undefined ||
        act.field === "advances" ||
        newer.field !== act.field ||
        newer.actor.kind !== act.actor.kind ||
        newer.actor.id !== act.actor.id
      );
    });
    expect(listed.slice(0, 3)).toEqual([acts[0], acts[3], acts[4]]);
    await expect(entries).toHaveCount(listed.length);
    for (const [place, act] of listed.entries()) {
      const entry = entries.nth(place);
      await expect(entry).toContainText(act.actor.id);
      await expect(entry.locator("time")).toHaveAttribute("datetime", act.at);
    }
    await expect(entries.nth(0)).toContainText("wrote the description");
    await expect(entries.nth(1)).toContainText(`wrote the title “${retitled}”`);
    await expect(entries.nth(2)).toContainText("wrote the owner “bob”");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, a relation written and retracted from an issue, and three pages that
// read each of those two writes.
test("a person places an issue under a priority from the issue and takes it off again", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(`/issues/${seed.clock}`);

    // The picker after the chips in the header's state row, as the labels' is in the row under
    // it: a tick stages the set and the close writes it.
    const picker = chipsLine(page).getByRole("button", { name: "Edit priorities" });
    const option = page
      .getByRole("listbox", { name: "Priorities options" })
      .getByRole("option", { exact: true, name: `#2 ${seed.titles[1]}` });
    await picker.click();
    await option.click();
    await expect(option).toHaveAttribute("aria-selected", "true");
    await picker.click();
    const named = chipsLine(page).getByRole("link", { name: `#2 ${seed.titles[1]}` });
    await expect(named).toBeVisible();
    await page.reload();
    await expect(named).toBeVisible();

    // The priority's own page lists the issue among its work, which is the same relation read
    // from the other end.
    await page.goto(`/priorities/${seed.ids[1]}`);
    const work = section(page, "Work advancing this priority");
    await expectRowNames(work, [
      `${seed.clock} Audit the clock`,
      `${seed.queue} Review queue triage`,
    ]);

    // Taken off from the same picker, and gone from both ends after a reload.
    await page.goto(`/issues/${seed.clock}`);
    await expect(named).toBeVisible();
    await picker.click();
    await expect(option).toHaveAttribute("aria-selected", "true");
    await option.click();
    await picker.click();
    await expect(chipsLine(page).getByRole("link")).toHaveCount(0);
    await page.reload();
    await expect(chipsLine(page).getByRole("link")).toHaveCount(0);
    await page.goto(`/priorities/${seed.ids[1]}`);
    await expectRowNames(section(page, "Work advancing this priority"), [
      `${seed.queue} Review queue triage`,
    ]);

    // And the priority's margin holds both acts, newest first, each in the words
    // `PriorityMargin.tsx` writes it in and each naming the person who made it: a relation is
    // an act of the priority as much as of the issue, and the retraction is not the assertion.
    // The margin reads the history apart from the work table above, so each entry is waited
    // for rather than read off a margin that may not have drawn yet.
    const entries = page.getByRole("complementary", { name: "Margin" }).getByRole("listitem");
    await expect
      .poll(() => entries.nth(0).innerText())
      .toContain(`retracted the relation to the issue ${seed.clock}`);
    await expect.poll(() => entries.nth(0).innerText()).toContain("alice");
    await expect
      .poll(() => entries.nth(1).innerText())
      .toContain(`related the issue ${seed.clock}`);
    await expect.poll(() => entries.nth(1).innerText()).toContain("alice");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the seed, an issue page held open across another person's delete, a write from its
// header, and a reload that reads it back.
test("another person's delete of a priority leaves an open issue page, whose next write lands", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  // The daemon's smoke suite advances #1 already; it states #3 beside it - one relation more,
  // which a write with no revision may assert - so the delete of #3 below leaves the issue one
  // relation of its own for the next write to keep.
  await patchIssue(seed.smoke, { advances: [seed.ids[0], seed.ids[2]] });
  const alice = await asUser(browser, "alice");
  try {
    // The reader: alice, on the issue's page, which nothing below reloads until the write has
    // landed. A mark on the document would be gone after a reload, so its presence then says
    // there was none.
    const reader = await alice.newPage();
    await priorityPage(reader);
    await reader.goto(`/issues/${seed.smoke}`);
    await reader.evaluate(() => {
      document.documentElement.dataset.heldOpen = "yes";
    });
    const line = reader.getByTestId("issue-advances");
    const kept = line.getByRole("link", { name: `#1 ${seed.titles[0]}` });
    const deleted = line.locator(`a[href="/priorities/${seed.ids[2]}"]`);
    await expect(kept).toBeVisible();
    await expect(deleted).toHaveText("#3");
    await expect(deleted).toHaveAccessibleName(`#3 ${seed.titles[2]}`);

    // Another person retires #3 over the API. The open header stops naming it, by its title or
    // by its id, with no reload: the issue's read is fetched again, so the set the next write
    // sends is the one the server holds.
    await endPriority(seed.ids[2], { login: "bob" });
    await expect(deleted).toHaveCount(0);
    await expect(kept).toBeVisible();

    // The reader places the issue under #2 from the header's own picker, which holds #1 ticked
    // and writes on its close. The write sends #1 and #2, which the server takes; a set still
    // naming #3 would be refused, a priority that is gone.
    const picker = reader.getByRole("button", { name: "Edit priorities" });
    const offered = reader.getByRole("listbox", { name: "Priorities options" });
    await picker.click();
    await expect(
      offered.getByRole("option", { exact: true, name: `#1 ${seed.titles[0]}` })
    ).toHaveAttribute("aria-selected", "true");
    await offered.getByRole("option", { exact: true, name: `#2 ${seed.titles[1]}` }).click();
    await picker.click();
    const added = line.getByRole("link", { name: `#2 ${seed.titles[1]}` });
    await expect(added).toBeVisible();
    expect(await reader.evaluate(() => document.documentElement.dataset.heldOpen)).toBe("yes");

    // What the server holds, read back after a reload: #1 and #2, and nothing of #3.
    await expect
      .poll(async () =>
        ((await getIssue(seed.smoke)).advances ?? []).map((entry) => entry.id).sort()
      )
      .toEqual([seed.ids[0], seed.ids[1]].sort());
    await reader.reload();
    await expect(kept).toBeVisible();
    await expect(added).toBeVisible();
    await expect(deleted).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the seed and one page read for what it does not draw.
test("a priority's page offers no versions and no delete, and carries no line about its order", async ({
  browser,
}) => {
  const seed = await seedPriorities();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await priorityPage(page);
    await page.goto(`/priorities/${seed.ids[0]}`);
    const main = page.getByTestId("main-content");
    await expect(titleHeading(page)).toHaveText(seed.titles[0]);

    // The description is an attribute of the node and not a Dispatch document, so nothing here
    // offers its versions, and there is no delete: a priority is retired by its state.
    await expect(main.getByRole("button", { name: /version/i })).toHaveCount(0);
    await expect(main.getByRole("link", { name: /version/i })).toHaveCount(0);
    await expect(main.getByText(/\bv\d+\b/)).toHaveCount(0);
    await expect(main.getByRole("button", { name: /delete/i })).toHaveCount(0);
    await expect(main.getByRole("link", { name: /delete/i })).toHaveCount(0);

    // Its place in the order and the counts of its work are the priorities list's to show
    // (INFRA-3221, R13): this page carries no line about either, and no `#N`. Its way out to the
    // work is one link per project, which reads "Open in" and the project's key.
    await expect(main.getByText(/in the order/)).toHaveCount(0);
    await expect(main.getByText(/^#\d+$/)).toHaveCount(0);
    await expect(
      main.getByText(/\b(Active|In flight|Triage, Todo|Backlog, icebox) \d/)
    ).toHaveCount(0);
    await expect(main.getByRole("link", { exact: true, name: "Open in CORE" })).toHaveAttribute(
      "href",
      /^\/projects\/CORE\/issues\?/
    );
  } finally {
    await context.close();
  }
});

// INFRA-2947 check 3bca8d03: the state table of a priority's title and description writes. Each
// row below is one cell: the field in one state - nothing out, typing unwritten, a write out, the
// last write landed, refused and kept, or ended with no answer - and one event - the reader types
// and rests or leaves, another person's text arrives, read exactly or not, or renames the title,
// the reader presses Use my text, or a write of hers that ended with no answer lands late. Every
// cell reads the five rules its outcome must keep, rather than the one a finding named:
//   1. No write of hers replaces a text the server holds that is not one of the field's own - its
//      start, her landed texts, her texts whose end is unknown - except Use my text, naming it.
//   2. Her text is never lost: it is on the server, in the field, or kept with the notice, after
//      a reload as before it. A later edit of hers lets a stopped one go only where it was made
//      on the stopped one's text; one made on the server's text, the field drawn again or
//      another person's text taken in, carries none of it.
//   3. When nobody else wrote, every edit of hers lands, with no 409 and no notice, whatever her
//      earlier writes ended as - but for two limits. First, a write pressed while an earlier one
//      is out, where the earlier one is then stopped or refused for its body and never lands, may
//      be refused 409 and offered as another person's change, with nothing lost and Use my text
//      landing it. Second, a stopped write of hers that a later edit of hers does not carry (one
//      made on the server's text: the field drawn again) stays on the shell's line. When she
//      presses its Retry, it is refused 409 and offered as another person's change, with nothing
//      lost.
//   4. One conflict gives one notice, and Use my text lands or meets a newer conflict.
//   5. Nothing is written from a reading of the server's that is not exact.
// A key that expired is no cell here: the page cannot make the server forget a write's key inside
// a run (the keys are the server's, kept past any row's length), so that ending is reached only as
// the Stop trying the cells below press.

/** The description a cell starts from, and another person's texts: one the server reads exactly,
 *  and one holding an HTML block, which it does not. */
const TABLE_START = "## Plan\n\n- first host\n- second host";
const BOB_EXACT = "## Bob's plan\n\n- third host";
const BOB_HTML = "## Plan\n\n<div>bob's block</div>\n\n- first host";
const TITLE_START = "Raced title";
const BOB_TITLE = "Bob's rename";

type TableField = "description" | "title";

/** One write of the field the page sent, with its answer once it came. */
interface LoggedWrite {
  /** The texts the write named as the ones it may go over: its `expected`, one string or a list
   *  of them, as a list; empty where it named none. */
  readonly expected: readonly string[];
  readonly text: string | undefined;
  /** Whether the reader's press of Use my text made it. */
  readonly use: boolean;
  status?: number;
  /** When the page had the write's answer (`Date.now()`). */
  answeredAt?: number;
}

/** One read of the priorities the page sent, `GET /api/v1/priorities`: when it went, and when the
 *  page had its answer. */
interface LoggedRead {
  readonly sentAt: number;
  answeredAt?: number;
}

/** A cell's page, field and records: every write of the priority the page sent, the texts other
 *  people wrote, and the reader's own suffixes, each a text no other cell types. */
interface TableCell {
  readonly id: string;
  readonly field: TableField;
  readonly page: Page;
  readonly writes: LoggedWrite[];
  /** Every read of the priorities the page sent. */
  readonly reads: LoggedRead[];
  readonly others: string[];
  /** Another person's texts the field took as its own start: drawn in it while nothing of hers
   *  was unwritten or out, so an edit of hers afterwards is made on them. */
  readonly taken: string[];
  /** Set while the reader presses Use my text, so the write it makes is told from the others. */
  using: boolean;
}

async function tableCell(page: Page, field: TableField, title: string): Promise<TableCell> {
  const made = await createPriority({
    description: TABLE_START,
    owner: "alice",
    title: field === "title" ? TITLE_START : title,
  });
  const cell: TableCell = {
    field,
    id: made.id,
    others: [],
    page,
    reads: [],
    taken: [],
    using: false,
    writes: [],
  };
  const sent = new Map<object, LoggedWrite>();
  const read = new Map<object, LoggedRead>();
  page.on("request", (request) => {
    if (request.method() !== "GET" || new URL(request.url()).pathname !== "/api/v1/priorities")
      return;
    const one: LoggedRead = { sentAt: Date.now() };
    cell.reads.push(one);
    read.set(request, one);
  });
  page.on("request", (request) => {
    if (request.method() !== "PATCH" || !request.url().endsWith(`/api/v1/priorities/${made.id}`))
      return;
    const body = request.postDataJSON() as Record<string, unknown>;
    const named = (body.expected as Record<string, string | string[]> | undefined)?.[field];
    const write: LoggedWrite = {
      expected: named === undefined ? [] : typeof named === "string" ? [named] : named,
      text: body[field] as string | undefined,
      use: cell.using,
    };
    cell.writes.push(write);
    sent.set(request, write);
  });
  page.on("response", (response) => {
    const write = sent.get(response.request());
    if (write !== undefined) {
      write.status = response.status();
      write.answeredAt = Date.now();
    }
    const one = read.get(response.request());
    if (one !== undefined) one.answeredAt = Date.now();
  });
  await page.goto(`/priorities/${made.id}`);
  await expect(page.getByText("second host").first()).toBeVisible();
  return cell;
}

/** The field's own control: `editor` when given, else the description's editor, or the title's
 *  box, opened from its heading as a reader opens it. In an editor, the selection the click makes
 *  can reach ProseMirror after ControlOrMeta+End and put the caret back where the click fell, so
 *  the gesture is made again until the caret stands collapsed at the editor's end. */
async function openTableField(cell: TableCell, editor?: Locator): Promise<Locator> {
  if (editor !== undefined || cell.field === "description") {
    const control = editor ?? cell.page.getByRole("textbox", { exact: true, name: "Description" });
    await expect(async () => {
      await control.click();
      await cell.page.keyboard.press("ControlOrMeta+End");
      expect(await control.evaluate(caretAtEnd), "the caret stands at the editor's end").toBe(true);
    }).toPass({ timeout: 5_000 });
    return control;
  }
  await titleHeading(cell.page).click();
  const box = cell.page.getByRole("textbox", { name: "Title" });
  await expect(box).toBeFocused();
  await cell.page.keyboard.press("End");
  return box;
}

/** Whether the selection is one caret inside `node` with no text of `node` after it. */
function caretAtEnd(node: Element): boolean {
  const selection = window.getSelection();
  const anchor = selection?.anchorNode ?? null;
  if (selection?.isCollapsed !== true || anchor === null || !node.contains(anchor)) return false;
  const after = document.createRange();
  after.setStart(anchor, selection.anchorOffset);
  after.setEnd(node, node.childNodes.length);
  return after.toString() === "";
}

/** Her suffix typed at the end of the field, or of `editor` when given, which she then leaves. */
async function typeAndLeave(cell: TableCell, suffix: string, editor?: Locator): Promise<void> {
  const field = await openTableField(cell, editor);
  await cell.page.keyboard.type(suffix);
  await field.blur();
}

async function tableStored(cell: TableCell): Promise<string> {
  const row = (await listPriorities()).priorities?.find((one) => one.id === cell.id);
  return (cell.field === "title" ? row?.title : row?.description) ?? "";
}

/** Another person's text of the field, written through the API as bob. */
async function otherWrites(cell: TableCell, text: string): Promise<void> {
  cell.others.push(text);
  await patchPriority(cell.id, cell.field === "title" ? { title: text } : { description: text }, {
    login: "bob",
  });
}

function tableNotice(cell: TableCell): Locator {
  const name = cell.field === "title" ? "Title" : "Description";
  return cell.page.getByRole("alert", { name: `${name}, changed by another person` });
}

/** Her press of Use my text on the field's notice. */
async function useMyText(cell: TableCell): Promise<void> {
  cell.using = true;
  await tableNotice(cell)
    .getByRole("button", { name: /^Use (my|this) text$/ })
    .first()
    .click();
}

/** Rule 1 and rule 5 over every write the page sent: a write the server took that named another
 *  person's text as one it may replace was Use my text's, or named a text the field had taken as
 *  its start, and no write carries the escapes a reading written back as markdown leaves. */
function checkWrites(cell: TableCell): void {
  const soft = expect.configure({ soft: true });
  const overwrote = cell.writes.filter(
    (write) =>
      write.status === 200 &&
      !write.use &&
      write.expected.some((text) => cell.others.includes(text) && !cell.taken.includes(text))
  );
  soft(overwrote, "no write of hers but Use my text replaced another person's text").toEqual([]);
  soft(
    cell.writes.filter((write) => /\\[<#*-]/.test(write.text ?? "")),
    "no write carries a reading's escapes"
  ).toEqual([]);
}

/** Rule 3: nobody else wrote, so her last text landed, with no 409 and no notice. */
async function checkClean(cell: TableCell, suffix: string): Promise<void> {
  const soft = expect.configure({ soft: true });
  await soft
    .poll(() => tableStored(cell), { message: "her last text landed", timeout: 15_000 })
    .toContain(suffix);
  soft(
    cell.writes.filter((write) => write.status === 409).length,
    "no write of hers was refused"
  ).toBe(0);
  // A notice comes from a read of the priorities the page takes in, so no notice is read only once
  // a read sent after her last write was answered and that answer drawn.
  const landed = Math.max(0, ...cell.writes.map((write) => write.answeredAt ?? 0));
  await expect
    .poll(() => cell.reads.some((one) => one.sentAt > landed && one.answeredAt !== undefined), {
      message: "the page read the priorities after her last write's answer",
      timeout: 15_000,
    })
    .toBe(true);
  await cell.page.evaluate(
    () => new Promise((drawn) => requestAnimationFrame(() => requestAnimationFrame(drawn)))
  );
  await soft(tableNotice(cell), "no notice of another person's text").toHaveCount(0);
  checkWrites(cell);
}

/** Rules 2 and 3 where an older write of hers was let go: her newer text stays on the server and
 *  in the field, no notice of another person's text shows, and a reload changes none of that. A
 *  refusal of the older write is allowed; a notice of it is not. */
async function checkLetGo(cell: TableCell, newer: string): Promise<void> {
  const soft = expect.configure({ soft: true });
  const field =
    cell.field === "title"
      ? titleHeading(cell.page)
      : cell.page.getByRole("textbox", { exact: true, name: "Description" });
  const landed = Math.max(0, ...cell.writes.map((write) => write.answeredAt ?? 0));
  await expect
    .poll(() => cell.reads.some((one) => one.sentAt > landed && one.answeredAt !== undefined), {
      message: "the page read the priorities after the Retry's answer",
      timeout: 15_000,
    })
    .toBe(true);
  await cell.page.evaluate(
    () => new Promise((drawn) => requestAnimationFrame(() => requestAnimationFrame(drawn)))
  );
  soft(await tableStored(cell), "the server holds her newer text").toContain(newer);
  await soft(field, "the field shows her newer text").toContainText(newer.trim());
  await soft(tableNotice(cell), "no notice of another person's text").toHaveCount(0);
  checkWrites(cell);
  await cell.page.reload();
  await expect(field).toContainText(newer.trim());
  soft(await tableStored(cell), "after a reload the server holds her newer text").toContain(newer);
  await soft(tableNotice(cell), "after a reload no notice of another person's text").toHaveCount(0);
}

/** `text` with each character a regular expression reads as syntax escaped. */
function escapedForRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** Rules 1 and 2 for a stopped write her landed edit carries none of: unless the server holds her
 *  stopped text, it stays on the shell's line, and its Retry, naming the text it went over, is
 *  refused 409 into the notice that holds her text. */
async function checkStoppedKept(cell: TableCell, suffix: string): Promise<void> {
  checkWrites(cell);
  if ((await tableStored(cell)).includes(suffix)) return;
  const retry = pendingWrites(cell.page).getByRole("button", { exact: true, name: "Retry" });
  await expect(retry.first(), "her stopped write is still on the shell's line").toBeVisible();
  await retry.first().click();
  await expect
    .poll(
      () =>
        cell.writes.some((write) => write.status === 409 && (write.text ?? "").includes(suffix)),
      { message: "her stopped write's Retry was sent and refused 409", timeout: 15_000 }
    )
    .toBe(true);
  await expect(
    cell.page
      .getByRole("alert", { name: /changed by another person$/ })
      .filter({ hasText: suffix.trim() }),
    "one notice holds her stopped text"
  ).toHaveCount(1, { timeout: 15_000 });
  checkWrites(cell);
}

/** Rules 1, 2 and 4 after another person's text met hers: one notice holds her text, the server
 *  holds his, and after a reload her text is still kept with the notice. */
async function checkKept(cell: TableCell, suffix: string, theirs: string): Promise<void> {
  const soft = expect.configure({ soft: true });
  await expect(tableNotice(cell)).toHaveCount(1);
  await expect(tableNotice(cell)).toContainText(suffix.trim());
  soft(await tableStored(cell), "the server holds the other person's text").toBe(theirs);
  checkWrites(cell);
  await cell.page.reload();
  await soft(
    tableNotice(cell),
    "after a reload her text is still kept with the notice"
  ).toContainText(suffix.trim());
}

/** Rule 2 where her text was neither refused nor landed for certain: after a reload it is on the
 *  server, in the field, or kept with the notice. */
async function checkNotLost(cell: TableCell, suffix: string): Promise<void> {
  // A stopped write is kept across a reload as refused (`KeptRefusal` "stopped"), and the shell
  // offers its Retry. Without that Retry her text must be stored.
  await cell.page.reload();
  const retry = pendingWrites(cell.page).getByRole("button", { exact: true, name: "Retry" });
  await expect
    .poll(async () => (await tableStored(cell)).includes(suffix) || (await retry.count()) > 0, {
      message: "her text is stored, or the shell offers its Retry, after a reload",
      timeout: 15_000,
    })
    .toBe(true);
  if (!(await tableStored(cell)).includes(suffix)) await retry.first().click();
  // The Retry lands her text, or, where another person's text came meanwhile, is refused and
  // her text is kept beside the field, in its notice: the shell's line names her text too, so a
  // match of the page's text alone would pass on the line.
  await expect
    .poll(
      async () =>
        (await tableStored(cell)).includes(suffix) ||
        (await tableNotice(cell).filter({ hasText: suffix.trim() }).count()) > 0,
      { message: "her text landed, or is kept in the field's notice", timeout: 15_000 }
    )
    .toBe(true);
}

/** Holds each write of the field the page sends until `release`, then passes it on. */
async function holdTableWrites(cell: TableCell) {
  const release = Promise.withResolvers<void>();
  let held = 0;
  await cell.page.route(`**/api/v1/priorities/${cell.id}`, async (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    held += 1;
    await release.promise;
    await route.fallback();
  });
  return { held: () => held, release: () => release.resolve() };
}

/** Answers each write of the field 503, with a second's Retry-After, until `stop` presses the
 *  shell's Stop trying for it: the write ends with no answer. Where `reaches` is set, the first
 *  try reaches the server, which takes it, and only its answer is lost. Set before she types. A
 *  try is counted (`tried`) once its 503 is sent: `stop` goes on, and unroutes, only then. An
 *  unroute while a try's handler still waits on the server's answer hands that route on
 *  (Playwright continues a route in flight whose handler is removed), and the handler's
 *  `route.fulfill` then throws "Route is already handled!". */
async function refuseTableWrites(cell: TableCell, reaches: boolean) {
  let first = true;
  let tried = 0;
  await cell.page.route(`**/api/v1/priorities/${cell.id}`, async (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    if (first && reaches) {
      first = false;
      await route.fetch();
    }
    await route.fulfill({
      headers: { "Retry-After": "1" },
      json: { code: "DATABASE_BUSY", error: "the database is too busy to take this request" },
      status: 503,
    });
    tried += 1;
  });
  return {
    stop: async () => {
      await lateThenStop(cell, () => tried);
      await cell.page.unroute(`**/api/v1/priorities/${cell.id}`);
    },
  };
}

/** Once the store has tried the write, the page's clock jumps past its send deadline
 *  (`SEND_DEADLINE_MS`), the shell offers Stop trying for it, and the reader presses it. */
async function lateThenStop(cell: TableCell, tried: () => number): Promise<void> {
  await expect.poll(tried, { message: "the store tried the write" }).toBeGreaterThan(0);
  await cell.page.clock.fastForward(SEND_DEADLINE_MS + 1);
  const stop = pendingWrites(cell.page).getByRole("button", { exact: true, name: "Stop trying" });
  await expect(stop.first()).toBeVisible();
  await stop.first().click();
}

const tableCells: {
  readonly name: string;
  readonly field: TableField;
  readonly slow?: boolean;
  readonly run: (cell: TableCell) => Promise<void>;
}[] = [
  // Nothing out.
  {
    field: "description",
    name: "nothing out, another person's exact text arrives, then she edits",
    run: async (cell) => {
      await otherWrites(cell, BOB_EXACT);
      await expect(cell.page.getByText("third host").first()).toBeVisible();
      cell.taken.push(BOB_EXACT);
      await typeAndLeave(cell, " zero-a");
      await checkClean(cell, " zero-a");
    },
  },
  {
    field: "description",
    name: "nothing out, another person's inexact text arrives, then she presses a key",
    run: async (cell) => {
      await otherWrites(cell, BOB_HTML);
      await expect(cell.page.getByText("bob's block").first()).toBeVisible();
      await cell.page.getByText("first host").first().click();
      await cell.page.keyboard.type("x");
      await cell.page.keyboard.press("Tab");
      // A write starts on the leave at once and lands within a second here; five seconds after
      // it, a write that was coming has landed, so its absence is read once the wait is over.
      await cell.page.waitForTimeout(5_000);
      expect(await tableStored(cell)).toBe(BOB_HTML);
      expect(cell.writes).toEqual([]);
    },
  },
  {
    field: "title",
    name: "nothing out, another person renames the title, then she edits it",
    run: async (cell) => {
      await otherWrites(cell, BOB_TITLE);
      await expect(titleHeading(cell.page)).toHaveText(BOB_TITLE);
      cell.taken.push(BOB_TITLE);
      await typeAndLeave(cell, " zero-t");
      await checkClean(cell, " zero-t");
    },
  },
  // Typing unwritten.
  {
    field: "description",
    name: "typing unwritten, she rests",
    run: async (cell) => {
      await openTableField(cell);
      await cell.page.keyboard.type(" one-a");
      await checkClean(cell, " one-a");
    },
  },
  {
    field: "description",
    name: "typing unwritten, another person's exact text arrives",
    run: async (cell) => {
      await pauseClock(cell.page);
      await openTableField(cell);
      await cell.page.keyboard.type(" one-b");
      await otherWrites(cell, BOB_EXACT);
      await cell.page.clock.resume();
      await checkKept(cell, " one-b", BOB_EXACT);
    },
  },
  {
    field: "description",
    name: "typing unwritten, another person's inexact text arrives",
    run: async (cell) => {
      await pauseClock(cell.page);
      await openTableField(cell);
      await cell.page.keyboard.type(" one-c");
      await otherWrites(cell, BOB_HTML);
      await cell.page.clock.resume();
      await checkKept(cell, " one-c", BOB_HTML);
    },
  },
  {
    field: "title",
    name: "typing unwritten, another person renames the title",
    run: async (cell) => {
      await pauseClock(cell.page);
      await openTableField(cell);
      await cell.page.keyboard.type(" one-t");
      await otherWrites(cell, BOB_TITLE);
      await cell.page.clock.resume();
      await checkKept(cell, " one-t", BOB_TITLE);
    },
  },
  // A write out.
  {
    field: "description",
    name: "a write out, she types again and rests",
    run: async (cell) => {
      const held = await holdTableWrites(cell);
      await typeAndLeave(cell, " two-a");
      await expect.poll(held.held).toBe(1);
      await typeAndLeave(cell, " two-b");
      held.release();
      await checkClean(cell, " two-a two-b");
    },
  },
  {
    field: "description",
    name: "a write out, another person's exact text lands before it",
    run: async (cell) => {
      const held = await holdTableWrites(cell);
      await typeAndLeave(cell, " two-c");
      await expect.poll(held.held).toBe(1);
      await otherWrites(cell, BOB_EXACT);
      held.release();
      await checkKept(cell, " two-c", BOB_EXACT);
    },
  },
  {
    field: "description",
    name: "a write out, another person's inexact text lands before it",
    run: async (cell) => {
      const held = await holdTableWrites(cell);
      await typeAndLeave(cell, " two-d");
      await expect.poll(held.held).toBe(1);
      await otherWrites(cell, BOB_HTML);
      held.release();
      await checkKept(cell, " two-d", BOB_HTML);
    },
  },
  {
    field: "title",
    name: "a write out, another person's rename lands before it",
    run: async (cell) => {
      const held = await holdTableWrites(cell);
      await typeAndLeave(cell, " two-t");
      await expect.poll(held.held).toBe(1);
      await otherWrites(cell, BOB_TITLE);
      held.release();
      await checkKept(cell, " two-t", BOB_TITLE);
    },
  },
  // The last write landed.
  {
    field: "description",
    name: "the last write landed, she edits again",
    run: async (cell) => {
      await typeAndLeave(cell, " three-a");
      await expect.poll(() => tableStored(cell)).toContain(" three-a");
      await typeAndLeave(cell, " three-b");
      await checkClean(cell, " three-a three-b");
    },
  },
  {
    field: "description",
    name: "the last write landed, another person's exact text arrives, then she edits",
    run: async (cell) => {
      await typeAndLeave(cell, " three-c");
      await expect.poll(() => tableStored(cell)).toContain(" three-c");
      await otherWrites(cell, BOB_EXACT);
      await expect(cell.page.getByText("third host").first()).toBeVisible();
      cell.taken.push(BOB_EXACT);
      await typeAndLeave(cell, " three-d");
      await checkClean(cell, " three-d");
    },
  },
  {
    field: "description",
    name: "the last write landed, another person's inexact text arrives, then she presses a key",
    run: async (cell) => {
      await typeAndLeave(cell, " three-e");
      await expect.poll(() => tableStored(cell)).toContain(" three-e");
      await otherWrites(cell, BOB_HTML);
      await expect(cell.page.getByText("bob's block").first()).toBeVisible();
      const before = cell.writes.length;
      await cell.page.getByText("first host").first().click();
      await cell.page.keyboard.type("x");
      await cell.page.keyboard.press("Tab");
      // A write starts on the leave at once and lands within a second here; five seconds after
      // it, a write that was coming has landed, so its absence is read once the wait is over.
      await cell.page.waitForTimeout(5_000);
      expect(await tableStored(cell)).toBe(BOB_HTML);
      expect(cell.writes.length).toBe(before);
    },
  },
  // Refused and kept.
  {
    field: "description",
    name: "refused and kept, she presses Use my text",
    run: async (cell) => {
      await pauseClock(cell.page);
      await openTableField(cell);
      await cell.page.keyboard.type(" four-a");
      await otherWrites(cell, BOB_EXACT);
      await cell.page.clock.resume();
      await expect(tableNotice(cell)).toContainText("four-a");
      await useMyText(cell);
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" four-a");
      await expect(tableNotice(cell)).toHaveCount(0);
      checkWrites(cell);
    },
  },
  {
    field: "description",
    name: "refused and kept over an inexact text, she presses Use my text",
    run: async (cell) => {
      await pauseClock(cell.page);
      await openTableField(cell);
      await cell.page.keyboard.type(" four-b");
      await otherWrites(cell, BOB_HTML);
      await cell.page.clock.resume();
      await expect(tableNotice(cell)).toContainText("four-b");
      await useMyText(cell);
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" four-b");
      expect(await tableStored(cell)).not.toContain("bob's block");
      checkWrites(cell);
    },
  },
  {
    field: "description",
    name: "refused and kept, another person writes again, then she presses Use my text",
    run: async (cell) => {
      await pauseClock(cell.page);
      await openTableField(cell);
      await cell.page.keyboard.type(" four-c");
      await otherWrites(cell, BOB_EXACT);
      await cell.page.clock.resume();
      await expect(tableNotice(cell)).toContainText("four-c");
      await otherWrites(cell, `${BOB_EXACT}\n- fourth host`);
      await useMyText(cell);
      // Use my text lands, or meets the newer text and keeps hers with one notice again.
      await expect
        .poll(
          async () =>
            (await tableStored(cell)).includes(" four-c") ||
            ((await tableNotice(cell).count()) === 1 &&
              (await tableNotice(cell).innerText()).includes("four-c")),
          { timeout: 15_000 }
        )
        .toBe(true);
      checkWrites(cell);
    },
  },
  {
    field: "title",
    name: "refused and kept, she presses Use my text on the title",
    run: async (cell) => {
      await pauseClock(cell.page);
      await openTableField(cell);
      await cell.page.keyboard.type(" four-t");
      await otherWrites(cell, BOB_TITLE);
      await cell.page.clock.resume();
      await expect(tableNotice(cell)).toContainText("four-t");
      await useMyText(cell);
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" four-t");
      checkWrites(cell);
    },
  },
  // Ended with no answer.
  {
    field: "description",
    name: "a write that never reached the server stopped, she edits again",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " five-a");
      await busy.stop();
      await typeAndLeave(cell, " five-b");
      await checkClean(cell, " five-a five-b");
    },
  },
  {
    field: "description",
    name: "a write whose answer was lost stopped, she edits again",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, true);
      await typeAndLeave(cell, " five-c");
      await busy.stop();
      await typeAndLeave(cell, " five-d");
      await checkClean(cell, " five-c five-d");
    },
  },
  {
    field: "title",
    name: "a title write that never reached the server stopped, she edits again",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " five-t");
      await busy.stop();
      await typeAndLeave(cell, " five-u");
      await checkClean(cell, " five-t five-u");
    },
  },
  {
    field: "description",
    name: "a write that never reached the server stopped, she reloads",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " five-e");
      await busy.stop();
      await checkNotLost(cell, " five-e");
    },
  },
  {
    field: "description",
    name: "a write that never reached the server stopped, another person's exact text arrives",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " five-f");
      await busy.stop();
      await otherWrites(cell, BOB_EXACT);
      await expect(cell.page.getByText("third host").first()).toBeVisible();
      checkWrites(cell);
      await checkNotLost(cell, " five-f");
    },
  },
  {
    field: "description",
    name: "a write that never reached the server stopped, she edits again and it lands, then she presses its Retry",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " five-h");
      await busy.stop();
      await typeAndLeave(cell, " five-i");
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" five-h five-i");
      // Her stopped write is older than the one that landed: its Retry, made or not, is let go,
      // and nothing of it reaches the field or the server.
      const retry = pendingWrites(cell.page).getByRole("button", { exact: true, name: "Retry" });
      await expect(retry.first()).toBeVisible();
      await retry.first().click();
      await expect.soft(retry, "the stopped write's line is gone").toHaveCount(0);
      await checkLetGo(cell, " five-h five-i");
    },
  },
  {
    field: "description",
    name: "a write that never reached the server stopped, another person's text arrives, she edits his text and it lands: her stopped text is not lost",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " eight-a");
      await busy.stop();
      await otherWrites(cell, BOB_EXACT);
      await expect(cell.page.getByText("third host").first()).toBeVisible();
      cell.taken.push(BOB_EXACT);
      await typeAndLeave(cell, " eight-b");
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" eight-b");
      // Her edit landed over his text, not over her stopped one, so it carries none of it: her
      // stopped text stays hers to put back, after a reload too.
      checkWrites(cell);
      await checkNotLost(cell, " eight-a");
    },
  },
  {
    field: "title",
    name: "a title write that never reached the server stopped, another person renames, she edits his title and it lands: her stopped title is not lost",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " eight-t");
      await busy.stop();
      await otherWrites(cell, BOB_TITLE);
      await expect(titleHeading(cell.page)).toHaveText(BOB_TITLE);
      cell.taken.push(BOB_TITLE);
      await typeAndLeave(cell, " eight-u");
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" eight-u");
      checkWrites(cell);
      await checkNotLost(cell, " eight-t");
    },
  },
  {
    field: "title",
    name: "a title write that never reached the server stopped, she edits the description and it lands, then she presses the title's Retry: her title lands",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " eight-x");
      await busy.stop();
      // The description's write lands; it is of another field, so it makes nothing of the
      // stopped title needless.
      await typeAndLeave(
        cell,
        " eight-y",
        cell.page.getByRole("textbox", { exact: true, name: "Description" })
      );
      await expect
        .poll(
          async () =>
            (await listPriorities()).priorities?.find((one) => one.id === cell.id)?.description ??
            "",
          { message: "her description landed", timeout: 15_000 }
        )
        .toContain(" eight-y");
      const retry = pendingWrites(cell.page).getByRole("button", { exact: true, name: "Retry" });
      await expect(retry.first()).toBeVisible();
      await retry.first().click();
      await checkClean(cell, " eight-x");
    },
  },
  // The field drawn again while her stopped write stands on the shell's line: her next edit is
  // made on the server's text, not on hers, so it carries none of it (rule 2).
  {
    field: "description",
    name: "a write stopped in a row of the list, the row closed and opened again, she edits and it lands: her stopped text is kept",
    slow: true,
    run: async (cell) => {
      await cell.page.goto(PRIORITIES_PATH);
      const title = (await listPriorities()).priorities?.find((one) => one.id === cell.id)?.title;
      const named = (verb: string) =>
        cell.page.getByRole("button", {
          name: new RegExp(`^${verb} #\\d+ ${escapedForRegExp(title ?? "")}$`),
        });
      const editor = cell.page.getByRole("textbox", { name: /^Description of #\d+$/ });
      await named("Open").click();
      await expect(editor).toBeVisible();
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " ten-a", editor);
      await busy.stop();
      await named("Close").click();
      await expect(editor).toHaveCount(0);
      await named("Open").click();
      await expect(editor).toBeVisible();
      await typeAndLeave(cell, " ten-b", editor);
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" ten-b");
      await checkStoppedKept(cell, " ten-a");
    },
  },
  {
    field: "description",
    name: "a write stopped, the page reloaded, she edits and it lands: her stopped text is kept",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " ten-c");
      await busy.stop();
      await cell.page.reload();
      await expect(cell.page.getByText("second host").first()).toBeVisible();
      await typeAndLeave(cell, " ten-d");
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" ten-d");
      await checkStoppedKept(cell, " ten-c");
    },
  },
  {
    field: "title",
    name: "a title write stopped, the page reloaded, she edits the title and it lands: her stopped title is kept",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " ten-t");
      await busy.stop();
      await cell.page.reload();
      await expect(titleHeading(cell.page)).toBeVisible();
      await typeAndLeave(cell, " ten-u");
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" ten-u");
      await checkStoppedKept(cell, " ten-t");
    },
  },
  {
    field: "title",
    name: "a title write stopped, she goes to the list and back, edits the title and it lands: her stopped title is kept",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " ten-v");
      await busy.stop();
      await cell.page.getByRole("link", { name: "‹ Priorities" }).click();
      await expect(cell.page.getByRole("table", { name: "Priorities table" })).toBeVisible();
      await cell.page.goBack();
      await expect(titleHeading(cell.page)).toBeVisible();
      await typeAndLeave(cell, " ten-w");
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" ten-w");
      await checkStoppedKept(cell, " ten-v");
    },
  },
  // Another person writes, then writes back exactly the text her stopped write went over: her
  // next edit names that text as hers did, but is made on his, so it carries none of hers.
  {
    field: "description",
    name: "a write stopped, another person writes and then writes back the text it went over, she edits and it lands: her stopped text is kept",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " ten-e");
      await busy.stop();
      await otherWrites(cell, BOB_EXACT);
      await expect(cell.page.getByText("third host").first()).toBeVisible();
      await otherWrites(cell, TABLE_START);
      await expect(cell.page.getByText("third host")).toHaveCount(0);
      cell.taken.push(BOB_EXACT, TABLE_START);
      await typeAndLeave(cell, " ten-f");
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" ten-f");
      await checkStoppedKept(cell, " ten-e");
    },
  },
  {
    field: "title",
    name: "a title write stopped, another person renames and then renames back, she edits the title and it lands: her stopped title is kept",
    slow: true,
    run: async (cell) => {
      const busy = await refuseTableWrites(cell, false);
      await typeAndLeave(cell, " ten-g");
      await busy.stop();
      await otherWrites(cell, BOB_TITLE);
      await expect(titleHeading(cell.page)).toHaveText(BOB_TITLE);
      await otherWrites(cell, TITLE_START);
      await expect(titleHeading(cell.page)).toHaveText(TITLE_START);
      cell.taken.push(BOB_TITLE, TITLE_START);
      await typeAndLeave(cell, " ten-h");
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" ten-h");
      await checkStoppedKept(cell, " ten-g");
    },
  },
  {
    field: "description",
    name: "a write stopped while out lands late, then she edits again",
    slow: true,
    run: async (cell) => {
      const late = Promise.withResolvers<void>();
      let tried = 0;
      await cell.page.route(`**/api/v1/priorities/${cell.id}`, async (route) => {
        if (route.request().method() !== "PATCH") return route.fallback();
        tried += 1;
        await late.promise;
        // The page gave the write up; it reaches the server all the same, as a slow network's
        // would, and its answer goes nowhere.
        await route.fetch().catch(() => undefined);
        await route.abort().catch(() => undefined);
      });
      await typeAndLeave(cell, " five-g");
      await lateThenStop(cell, () => tried);
      late.resolve();
      await expect.poll(() => tableStored(cell), { timeout: 15_000 }).toContain(" five-g");
      await cell.page.unroute(`**/api/v1/priorities/${cell.id}`);
      await typeAndLeave(cell, " five-h");
      await checkClean(cell, " five-g five-h");
    },
  },
];

for (const { field, name, run, slow } of tableCells) {
  // SLOW(>=2s): a priority written, a page read, and the cell's writes and reads awaited.
  test(`state table, ${field}: ${name}`, async ({ browser }) => {
    test.setTimeout(slow === true ? 150_000 : 60_000);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.clock.install();
      const cell = await tableCell(page, field, `Table ${name}`.slice(0, 120));
      try {
        await run(cell);
      } finally {
        // Every write the page sent and its answer, for the reader of a failed cell.
        await test.info().attach("writes", {
          body: JSON.stringify(cell.writes, null, 1),
          contentType: "application/json",
        });
      }
    } finally {
      await context.close();
    }
  });
}
