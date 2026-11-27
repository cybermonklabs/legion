/** The views' rows, tabs and counts are read against the server's own listing and board route,
 *  saved views are read by a second login signed in on the same context and on its own, and the
 *  Board's frames come over the live socket. */
// The Issues tab's views (dispatch://INFRA-3221, Verification k3, k4, k10 and k12): the two
// built-in views and the older view addresses, their rows, headers and widths; the views a reader
// saves, per login and per project; the page's words, tooltips and counts; and the Title column's
// width on a desktop and its place on a phone.
//
// ACME is seeded with `seedLegionFixture`, its daemon named at a `startFakeLegionDaemon`, and
// with `seedLegionMetricsCorpus`, and PLAIN is a project with no Legion setting. Each expected
// row, order and count is computed in the row from the seed through the API (`GET /api/v1/issues`
// and the board route), never with the web code's filters. A row that measures a position on
// ACME seeds a failed read of its daemon first and holds every board frame after the first, so
// neither the sampler's own failed read nor its frame moves what it measures.
import type { LegionBoardResponse as Board, IssueSummary } from "../web/src/api/types";
import { CONTROL } from "../web/src/components/control-names";
import { setLiveSessions } from "./agents";
import {
  claimIssue,
  createAsk,
  createIssue,
  createPriority,
  createProject,
  getIssue,
  getLegionBoard,
  getMyState,
  listIssuesWhere,
  mintAgentToken,
  patchIssue,
  putIssueState,
} from "./api";
import { seedSessionGroupsFixture } from "./delivery-seed";
import { control, controlRow, listTrigger } from "./filters";
import {
  ACME,
  type BoardParts,
  boardFacts,
  cardsOf,
  EVERY_COL,
  issuesTable,
  measuredWork,
  readFilter,
  routeLive,
  rowKeys,
  saveView,
  seedAcme,
  seedMeasuredAcme,
  tabName,
  tabNames,
  tabTitles,
} from "./issues-views-shared";
import { waitForIssueFacts, writeRoleEvent } from "./legion-facts-seed";
import { pushLegionObservation } from "./legion-seed";
import { drawnControlNames, straysBetween, surfaceControls, topOf } from "./table-controls";
import { type BrowserContext, expect, type Locator, type Page, test } from "./test";
import { asUser, signIn } from "./users";

const PLAIN = "PLAIN";

/** Open's and Done's headers in a project with a Legion daemon, after Select: the pinned ones,
 *  then the view's other default columns in the Design's order. */
const OPEN_HEADERS = [
  "Priority",
  "Key",
  "Title",
  "Queue",
  "Status",
  "Created by",
  "Open asks",
  "Legion phase",
  "Assignee",
  "Labels",
  "Claimed by",
  "Created",
];
const DONE_HEADERS = [
  "Priority",
  "Key",
  "Title",
  "Closed",
  "Time per phase",
  "Work time",
  "Assignee",
  "Created by",
  "Labels",
];

/** The headers a project with no daemon leaves out of each view. */
const NO_DAEMON_LEAVES = ["Queue", "Legion phase", "Time per phase", "Work time"];

/** The Legion columns' headers, as R9 names them. */
const LEGION_HEADERS = [
  "Legion phase",
  "Workers",
  "Worker state",
  "Time waiting",
  "Why it waits",
  "Time in Legion phase",
  "Work time",
  "Review rounds",
  "Legion tree",
  "Time per phase",
];

/** The five measures' methods, word for word as the Design's columns table gives them (R9, k10,
 *  k11). */
const METHODS: Readonly<Record<string, string>> = {
  "Work time":
    "Dispatch counts the hours from the first Legion worker event until the issue closes, or until Dispatch last read it. It does not count Backlog or Icebox hours, or hours when Legion has admitted the issue but not started it.",
  "Time per phase":
    "Each hour goes to Waiting on a person while the issue's ask or root waits, else to that hour's phase. Other holds the rest, and a root's hours waiting on its children do not count.",
  "Review rounds":
    "Dispatch counts each start of the Reviewing phase. For an issue with no recorded Legion phases, it counts each move to Needs review.",
  "Time waiting":
    "Dispatch shows the time since Legion asked a person, when the issue waits on a person. If it does not wait on a person, it shows the time since its Legion phase started.",
  "Time in Legion phase":
    "Dispatch shows the time since Legion first recorded the issue in its current Legion phase.",
};

/** What every sortable header's tooltip says of its sort button that is not pinned. */
const SORT_HINT = "Sort (shift-click to add a second sort)";
const MOVABLE_HINT = `${SORT_HINT}; drag to move the column`;

/** What a claim chip's tooltip says a claim is. */
const CLAIM_SENTENCE =
  "A claim marks who is implementing this issue, so two agents never take the same work.";

/** The phases Time per phase draws, in the board's order with Other last, with their words. */
const PHASE_WORDS: readonly (readonly [string, string])[] = [
  ["design", "Design"],
  ["planning", "Planning"],
  ["implementing", "Implementing"],
  ["testing", "Testing"],
  ["reviewing", "Reviewing"],
  ["accepting", "Accepting"],
  ["retro", "Retro"],
  ["merging", "Merging"],
  ["awaiting_person", "Waiting on a person"],
  ["held", "Held"],
  ["other", "Other"],
];

/** A project with no Legion setting, with open issues in four statuses, two of them Todo
 *  issues labelled legion, and two done ones. */
async function seedPlain(): Promise<void> {
  await createProject({ key: PLAIN, name: "Plain" });
  const issues: readonly (readonly [string, string, readonly string[]])[] = [
    ["A triage question", "triage", []],
    ["Queued for Legion first", "todo", ["legion"]],
    ["Work in progress", "in_progress", []],
    ["Queued for Legion second", "todo", ["legion"]],
    ["Work in review", "needs_review", []],
    ["Finished work", "done", []],
    ["Finished before it", "done", []],
  ];
  for (const [title, status, labels] of issues) {
    const issue = await createIssue({ project: PLAIN, title });
    await patchIssue(issue.key, labels.length === 0 ? { status } : { labels: [...labels], status });
  }
}

/** A done issue of `project` with priority `priority`. */
async function seedDone(project: string, title: string, priority: 0 | 1 | 2 | 3): Promise<void> {
  const issue = await createIssue({ project, title });
  await patchIssue(issue.key, { priority });
  await patchIssue(issue.key, { status: "done" });
}

/** The places of the Legion queue by issue: the board's `waiting_place`, or, with no daemon, the
 *  Todo issues labelled legion in rank order. */
function placesOf(issues: readonly IssueSummary[], board: Board | undefined): Map<string, number> {
  if (board !== undefined)
    return new Map(
      (board.columns ?? []).flatMap((column) =>
        (column.cards ?? []).flatMap((card) =>
          card.waiting_place === null ? [] : [[card.key, card.waiting_place] as const]
        )
      )
    );
  const queued = issues
    .filter(
      (issue) =>
        issue.status === "todo" &&
        (issue.labels ?? []).some((label) => label.toLowerCase() === "legion")
    )
    .sort((a, b) => (a.rank < b.rank ? -1 : a.rank > b.rank ? 1 : 0));
  return new Map(queued.map((issue, index) => [issue.key, index + 1]));
}

/** `issues` in Open's sort, Queue: the issues with a Place by it, then the rest by rank, ties in
 *  the listing's own order. */
function queueOrder(issues: readonly IssueSummary[], board: Board | undefined): string[] {
  const places = placesOf(issues, board);
  return issues
    .map((issue, index) => ({ issue, index }))
    .sort((a, b) => {
      const left = places.get(a.issue.key);
      const right = places.get(b.issue.key);
      if (left !== undefined && right !== undefined) return left - right;
      if (left !== undefined) return -1;
      if (right !== undefined) return 1;
      if (a.issue.rank !== b.issue.rank) return a.issue.rank < b.issue.rank ? -1 : 1;
      return a.index - b.index;
    })
    .map(({ issue }) => issue.key);
}

/** The header labels of `table`, in their order, without the controls each holds; Select's,
 *  which has no label, left out. */
function headerLabels(table: Locator): Promise<string[]> {
  return table
    .locator("thead th")
    .evaluateAll((cells) =>
      cells
        .map((cell) =>
          (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, "").trim()
        )
        .filter((label) => label !== "")
    );
}

/** The accessible name of a sort button that reads `label`, whatever sort mark it carries. */
function sortName(label: string): RegExp {
  return new RegExp(`^${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?: ?[▲▼])?$`);
}

/** The header cell of `table` whose sort button reads `label`. */
function header(table: Locator, label: string): Locator {
  return table
    .locator("thead th")
    .filter({ has: table.page().getByRole("button", { name: sortName(label) }) });
}

/** The sort button of `table`'s header `label`. */
function sortButton(table: Locator, label: string): Locator {
  return header(table, label).getByRole("button", { name: sortName(label) });
}

/** How a header cell holds what it draws, as `headerFit` reads it. */
interface HeaderFit {
  /** Whether its sort button cuts its label or its sort mark. */
  readonly cut: boolean;
  /** The sort mark it draws, or the empty text. */
  readonly mark: string;
  /** Which of the sort button, the mark and the filter button reach past the cell's edges. */
  readonly outside: readonly string[];
}

/** How the header cell `cell` holds its sort button, its sort mark and its filter button. */
function headerFit(cell: Locator): Promise<HeaderFit> {
  return cell.evaluate((node, filterControl) => {
    const own = node.getBoundingClientRect();
    const button = node.querySelector("button");
    const parts = {
      button,
      mark: button?.lastElementChild ?? null,
      filter: node.querySelector(`[data-control="${filterControl}"]`),
    };
    return {
      cut: button === null || button.scrollWidth > button.clientWidth,
      mark: (parts.mark?.textContent ?? "").trim(),
      outside: Object.entries(parts).flatMap(([name, part]) => {
        const drawn = part?.getBoundingClientRect();
        return drawn === undefined || drawn.left < own.left - 0.5 || drawn.right > own.right + 0.5
          ? [name]
          : [];
      }),
    };
  }, CONTROL.columnFilter);
}

/** A sorting header of a scroll box as `boxHeaders` reads it. */
interface DrawnHeader {
  readonly cut: boolean;
  readonly label: string;
  readonly left: number;
  readonly right: number;
}

/** A table's scroll box as `boxHeaders` reads it. */
interface BoxRead {
  readonly box: { readonly left: number; readonly right: number };
  readonly clientWidth: number;
  readonly headers: readonly DrawnHeader[];
  readonly scrollWidth: number;
}

/** The scroll box `box` as its table draws it: the box's own edges, its widths, and each sorting
 *  header's label, edges and whether its sort button cuts it. */
function boxHeaders(box: Locator): Promise<BoxRead> {
  return box.evaluate((node) => {
    const own = node.getBoundingClientRect();
    return {
      box: { left: own.left, right: own.left + node.clientWidth },
      clientWidth: node.clientWidth,
      headers: [...node.querySelectorAll("thead th")].flatMap((cell) => {
        const button = cell.querySelector("button");
        if (button === null) return [];
        const rect = cell.getBoundingClientRect();
        return [
          {
            cut: button.scrollWidth > button.clientWidth,
            label: (button.textContent ?? "").replace(/[▲▼]/g, "").trim(),
            left: rect.left,
            right: rect.right,
          },
        ];
      }),
      scrollWidth: node.scrollWidth,
    };
  });
}

/** Each header of `labels` lies whole inside the box `read` measured, before it scrolls. */
function expectInBox(read: BoxRead, labels: readonly string[], where: string): void {
  for (const label of labels) {
    const drawn = read.headers.find((one) => one.label === label);
    expect(drawn, `${where}: no ${label} header`).toBeDefined();
    expect(drawn?.left ?? 0, `${where}: ${label} starts outside the box`).toBeGreaterThanOrEqual(
      read.box.left - 0.5
    );
    expect(drawn?.right ?? 0, `${where}: ${label} ends outside the box`).toBeLessThanOrEqual(
      read.box.right + 0.5
    );
  }
}

/** The Open asks header of `table`, sorted by it with a press on its sort button, lies whole
 *  inside its cell with its sort mark
 *  (dispatch://INFRA-3221/comment/141bbad2-b75e-4bb4-b0a7-b5b1af541a3c). */
async function expectSortedAsksWhole(page: Page, table: Locator, where: string): Promise<void> {
  await sortButton(table, "Open asks").click();
  await expect.poll(() => new URL(page.url()).searchParams.get("sort")).toMatch(/^-?asks$/);
  const fit = await headerFit(header(table, "Open asks"));
  expect(fit.mark, `${where}: the sorted Open asks header draws no sort mark`).toMatch(/^[▲▼]$/);
  expect(fit.cut, `${where}: the sorted Open asks header cuts its label or its mark`).toBe(false);
  expect(fit.outside, `${where}: parts of the sorted Open asks header leave its cell`).toEqual([]);
}

/** Every drawn row's cells by the header label, as text, with whether each cell holds a Time
 *  per phase bar. */
function readCells(table: Locator): Promise<Record<string, Record<string, string>>> {
  return table.evaluate((node) => {
    const labels = [...node.querySelectorAll("thead th")].map((cell) =>
      (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, "").trim()
    );
    const rows: Record<string, Record<string, string>> = {};
    for (const row of node.querySelectorAll<HTMLElement>("tbody tr[data-issue-row]")) {
      const cells: Record<string, string> = {};
      [...row.children].forEach((cell, index) => {
        const label = labels[index] ?? "";
        if (label === "") return;
        const bar = cell.querySelector("[data-phase-hours]");
        cells[label] =
          bar === null ? (cell.textContent ?? "").trim() : `bar ${bar.getAttribute("aria-label")}`;
      });
      rows[row.dataset.issueRow ?? ""] = cells;
    }
    return rows;
  });
}

/** Every column the Columns menu lists, with the muted line under its name where it has one. */
async function columnsMenu(page: Page): Promise<{ name: string; line: string | null }[]> {
  await page.getByRole("button", { exact: true, name: "Columns" }).click();
  const menu = page.getByRole("dialog", { name: "Columns" });
  await expect(menu).toBeVisible();
  const entries = await menu.locator("label").evaluateAll((labels) =>
    labels.map((label) => {
      const line = label.nextElementSibling;
      return {
        line: line?.tagName === "P" ? (line.textContent ?? "").trim() : null,
        name: (label.textContent ?? "").trim(),
      };
    })
  );
  await page.keyboard.press("Escape");
  await expect(menu).toHaveCount(0);
  return entries;
}

/** Presses Tab until `target` holds the keyboard focus, starting from where the focus is, and
 *  fails after `most` presses. */
async function tabTo(page: Page, target: Locator, most = 120): Promise<void> {
  for (let press = 0; press < most; press++) {
    // A Tab press moves the focus before the press resolves, so this read sees where the focus
    // is now.
    if (await target.evaluate((node) => node === document.activeElement)) return;
    await page.keyboard.press("Tab");
  }
  await expect(target).toBeFocused();
}

/** The open tooltip's lines: each paragraph or list item, or its one text where it has neither. */
function tooltipLines(page: Page): Promise<string[]> {
  return page.getByRole("tooltip").evaluate((tooltip) => {
    const lines = [...tooltip.querySelectorAll("p, li")].map((line) =>
      (line.textContent ?? "").trim()
    );
    return lines.length > 0 ? lines : [(tooltip.textContent ?? "").trim()];
  });
}

// SLOW(>=2s): two projects seeded, each opened at eleven addresses and read row by row.
test("Open and Done are the two tabs, every older view address opens Open, and each view lists the rows, order and headers the API gives", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await seedPlain();
    const page = await context.newPage();
    await page.setViewportSize({ height: 1400, width: 1280 });
    const table = issuesTable(page);
    const rowCount = async () => Number(await table.getAttribute("aria-rowcount")) - 1;
    for (const project of [ACME, PLAIN]) {
      const legion = project === ACME;
      const board = legion ? await getLegionBoard(ACME) : undefined;
      const open = await listIssuesWhere({ open: true, project });
      const done = await listIssuesWhere({ project, sort: "-closed_at", status: ["done"] });
      const order = queueOrder(open, board);
      const openTab = tabName("Open", open.length);
      const doneTab = tabName("Done", done.length);
      const leaves = (headers: readonly string[]) =>
        legion ? headers : headers.filter((label) => !NO_DAEMON_LEAVES.includes(label));
      const where = `on ${project}`;

      await page.goto(`/projects/${project}/issues?view=open`);
      await expect.poll(() => tabNames(page), { message: where }).toEqual([openTab, "Done"]);
      await expect.poll(() => rowKeys(table), { message: `${where}: Open's rows` }).toEqual(order);
      expect(await rowCount()).toBe(open.length);
      expect(await headerLabels(table), `${where}: Open's headers`).toEqual(leaves(OPEN_HEADERS));

      // Done, opened in the page: its tab counts every done issue, and keeps its count after.
      await page.getByRole("tab", { name: /^Done/ }).click();
      await expect.poll(() => tabNames(page), { message: where }).toEqual([openTab, doneTab]);
      await expect
        .poll(() => rowKeys(table), { message: `${where}: Done's rows` })
        .toEqual(done.map((issue) => issue.key));
      expect(await rowCount()).toBe(done.length);
      expect(await headerLabels(table), `${where}: Done's headers`).toEqual(leaves(DONE_HEADERS));
      await page.getByRole("tab", { name: /^Open/ }).click();
      await expect.poll(() => tabNames(page), { message: where }).toEqual([openTab, doneTab]);

      // The views this build no longer has open Open with no narrowing.
      for (const view of ["in-flight", "finished", "slots", "triage", "mine"]) {
        await page.goto(`/projects/${project}/issues?view=${view}`);
        await expect
          .poll(() => tabNames(page), { message: `${where}: view=${view}` })
          .toEqual([openTab, "Done"]);
        await expect(page.getByRole("tab", { name: /^Open/ })).toHaveAttribute(
          "aria-selected",
          "true"
        );
        await expect
          .poll(() => rowKeys(table), { message: `${where}: view=${view}` })
          .toEqual(order);
      }

      // The Legion queue's old tabs open Open narrowed to the queue, in the order of the Place,
      // with the tab marked; Clear filters takes the narrowing off.
      const places = placesOf(open, board);
      const cards = board === undefined ? new Map() : cardsOf(board);
      const queued = order.filter((key) => places.has(key) || cards.get(key)?.column === "queue");
      expect(queued.length, `${where}: the seed queues no issue`).toBeGreaterThan(0);
      for (const view of ["queue", "waiting"]) {
        await page.goto(`/projects/${project}/issues?view=${view}`);
        await expect
          .poll(() => new URL(page.url()).searchParams.getAll("f.queue"), {
            message: `${where}: view=${view}`,
          })
          .toEqual(["Queued"]);
        await expect
          .poll(() => tabNames(page), { message: `${where}: view=${view}` })
          .toEqual([tabName("Open", queued.length, true), "Done"]);
        await expect.poll(() => rowKeys(table)).toEqual(queued);
        await page.getByRole("button", { exact: true, name: "Clear filters" }).click();
        await expect.poll(() => rowKeys(table)).toEqual(order);
        await expect.poll(() => tabNames(page)).toEqual([openTab, "Done"]);
        expect(new URL(page.url()).searchParams.has("f.queue")).toBe(false);
      }

      // `f.stage` is dropped: the address lists Open's rows, and the tab is not marked.
      await page.goto(`/projects/${project}/issues?view=open&f.stage=implementing`);
      await expect.poll(() => new URL(page.url()).searchParams.has("f.stage")).toBe(false);
      await expect.poll(() => tabNames(page)).toEqual([openTab, "Done"]);
      await expect.poll(() => rowKeys(table)).toEqual(order);
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): both views read at 1440 px and scrolled to their right edge, then Open sorted by
// Open asks and given its own sort back.
test("at 1440 by 900 with the sidebar open both of ACME's views scroll sideways under Select, Priority, Key and Title, and no header is cut", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    await page.setViewportSize({ height: 900, width: 1440 });
    const table = issuesTable(page);
    const box = page.getByTestId("issues-table-scroll");
    for (const [view, first] of [
      ["open", ["Created by", "Open asks"]],
      ["done", ["Time per phase", "Work time"]],
    ] as const) {
      await page.goto(`/projects/${ACME}/issues?view=${view}`);
      await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
      await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
      await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
      // The row wait above drew the table, and the table draws its whole head with its rows.
      const read = await boxHeaders(box);
      expect(read.scrollWidth, `${view}: the table does not scroll sideways`).toBeGreaterThan(
        read.clientWidth
      );
      for (const drawn of read.headers)
        expect(drawn.cut, `${view}: ${drawn.label} is cut`).toBe(false);
      expectInBox(read, first, view);
      const pinned = async () =>
        table
          .locator("thead th")
          .evaluateAll((cells) =>
            cells.slice(0, 4).map((cell) => Math.round(cell.getBoundingClientRect().left))
          );
      const before = await pinned();
      expect(await headerLabels(table).then((labels) => labels.slice(0, 3))).toEqual([
        "Priority",
        "Key",
        "Title",
      ]);
      await box.evaluate((node) => {
        node.scrollLeft = node.scrollWidth;
      });
      await expect.poll(() => box.evaluate((node) => node.scrollLeft)).toBeGreaterThan(0);
      expect(await pinned(), `${view}: Select, Priority, Key and Title moved`).toEqual(before);
    }

    // Sorted by Open asks, its header takes its sort mark on whole; in Open's own sort again,
    // after a press on the tab, Created by and Open asks still stand whole on the first screen.
    await page.goto(`/projects/${ACME}/issues?view=open`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await expectSortedAsksWhole(page, table, "open at 1440");
    await page.getByRole("tab", { name: /^Open/ }).click();
    await expect.poll(() => new URL(page.url()).searchParams.has("sort")).toBe(false);
    await expect(header(table, "Open asks")).not.toHaveAttribute("aria-sort");
    await box.evaluate((node) => {
      node.scrollLeft = 0;
    });
    // The aria-sort wait above drew Open's own sort, and the head draws every header with it.
    const own = await boxHeaders(box);
    for (const drawn of own.headers)
      expect(drawn.cut, `open in its own sort: ${drawn.label} is cut`).toBe(false);
    expectInBox(own, ["Created by", "Open asks"], "open in its own sort");
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): a narrowing, Clear filters and two sort changes read at two widths, each a page load.
test("Done's header and Open's first row stay where they are through a narrowing, Clear filters and a sort change, and a sorted Open asks header is whole, at 1280 and 390", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    await seedDone(ACME, "A finished urgent fix", 0);
    const done = await listIssuesWhere({ project: ACME, sort: "-closed_at", status: ["done"] });
    const open = await listIssuesWhere({ open: true, project: ACME });
    const urgent = done.filter((issue) => issue.priority === 0);
    const table = issuesTable(page);
    const clear = page.getByRole("button", { exact: true, name: "Clear filters" });
    for (const { height, width } of [
      { height: 900, width: 1280 },
      { height: 844, width: 390 },
    ]) {
      const where = `at ${width}x${height}`;
      await page.setViewportSize({ height, width });
      await page.goto(`/projects/${ACME}/issues?view=done`);
      await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
      await expect.poll(() => rowKeys(table)).toEqual(done.map((issue) => issue.key));
      const head = table.locator("thead");
      const row = controlRow(page.getByTestId("main-content"));
      const before = await topOf(head);
      const rowBefore = (await row.boundingBox())?.height ?? 0;
      await page.getByRole("button", { name: /^Filter Priority/ }).click();
      await page
        .getByRole("listbox", { name: "Priority options" })
        .getByRole("option", { name: /^P0, / })
        .click();
      await page.keyboard.press("Escape");
      await expect.poll(() => new URL(page.url()).searchParams.getAll("f.priority")).toEqual(["0"]);
      await expect.poll(() => rowKeys(table)).toEqual(urgent.map((issue) => issue.key));
      expect(await tabNames(page)).toEqual([
        tabName("Open", open.length),
        tabName("Done", urgent.length, true),
      ]);
      // The narrowing's chip takes the control row's one line of chips (INFRA-3135's R1, which
      // the Design's views section allows to move the rows): the header moves by exactly what
      // the row grew, and nothing else above the table moves it.
      const grown = ((await row.boundingBox())?.height ?? 0) - rowBefore;
      expect(grown, `${where}: the chips line is more than one line`).toBeLessThanOrEqual(52);
      expect(await topOf(head), `${where}: the narrowing moved the header`).toBeCloseTo(
        before + grown,
        0
      );
      await clear.click();
      await expect.poll(() => rowKeys(table)).toEqual(done.map((issue) => issue.key));
      expect(await tabNames(page)).toEqual([
        tabName("Open", open.length),
        tabName("Done", done.length),
      ]);
      expect(await topOf(head), `${where}: Clear filters moved the header`).toBeCloseTo(before, 0);

      // Only the sort changed: the tab is marked, Save view shows and Clear filters does not.
      await page.goto(`/projects/${ACME}/issues`);
      const firstRow = table.locator("tbody tr[data-issue-row]").first();
      await expect(firstRow).toBeVisible();
      const rowTop = await topOf(firstRow);
      const priority = sortButton(table, "Priority");
      await priority.click();
      await priority.click();
      await expect.poll(() => new URL(page.url()).searchParams.get("sort")).toBe("-priority");
      await expect(page.getByRole("tab", { name: /^Open/ })).toHaveAccessibleName(/, changed$/);
      expect(await topOf(table.locator("tbody tr[data-issue-row]").first())).toBeCloseTo(rowTop, 0);
      await expect(clear).toHaveCount(0);
      await expect(page.getByRole("button", { exact: true, name: "Save view" })).toBeVisible();
      await page.getByRole("tab", { name: /^Open/ }).click();
      await expect.poll(() => new URL(page.url()).searchParams.has("sort")).toBe(false);
      await expect(page.getByRole("tab", { name: /^Open/ })).toHaveAccessibleName(
        tabName("Open", open.length)
      );
      // Sorted by Open asks, its header lies whole inside its cell with its sort mark.
      await expectSortedAsksWhole(page, table, where);
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a view saved from Open with status=triage draws as a tab after Done and lists the Triage issues", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await createIssue({ project: ACME, title: "A question to sort" });
    await createIssue({ project: ACME, title: "A second question to sort" });
    const triage = await listIssuesWhere({ project: ACME, status: ["triage"] });
    const page = await context.newPage();
    await page.goto(`/projects/${ACME}/issues?status=triage`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    const table = issuesTable(page);
    await expect
      .poll(async () => (await rowKeys(table)).sort())
      .toEqual(triage.map((issue) => issue.key).sort());
    await page.getByRole("button", { exact: true, name: "Save view" }).click();
    const panel = page.getByRole("dialog", { name: "Save view" });
    await panel.getByLabel("Name of the view").fill("Triage");
    await panel.getByRole("button", { exact: true, name: "Save" }).click();
    await expect.poll(() => tabTitles(page)).toEqual(["Open", "Done", "Triage"]);
    await expect(page.getByRole("tab", { name: /^Triage/ })).toHaveAccessibleName(
      tabName("Triage", triage.length)
    );
    await expect
      .poll(async () => (await rowKeys(table)).sort())
      .toEqual(triage.map((issue) => issue.key).sort());
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): ACME's Issues tab opened four times, in both layouts, with and without a filter.
test("ACME's Issues tab draws one control row with its one filter trigger, the controls the amended list gives it and nothing else between its title and its first row", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    await page.setViewportSize({ height: 900, width: 1440 });
    const root = page.getByTestId("main-content");
    for (const layout of ["table", "board"] as const) {
      for (const narrowing of ["", "f.legion_phase=implementing"]) {
        const name = layout === "table" ? "project List" : "project Board";
        const where = `${name}${narrowing === "" ? "" : ` with ${narrowing}`}`;
        await page.goto(
          `/projects/${ACME}/issues?${[narrowing, layout === "board" ? "layout=board" : ""]
            .filter((part) => part !== "")
            .join("&")}`
        );
        await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
        const firstRow =
          layout === "table"
            ? page.locator("[data-issue-row]").first()
            : page.locator("[data-board-card]").first();
        await expect(firstRow).toBeVisible();
        const strays = await straysBetween(
          root,
          await topOf(root.getByRole("heading", { level: 1 })),
          await topOf(firstRow)
        );
        expect(strays, `${where}: drawn outside the control row`).toEqual([]);
        const expected = surfaceControls(name, true);
        const drawn = await drawnControlNames(root);
        expect(
          drawn.filter((value) => !(expected as readonly string[]).includes(value)),
          `${where}: controls the design does not give it`
        ).toEqual([]);
        await expect(control(controlRow(root), CONTROL.panelTrigger)).toHaveCount(1);
        // Save view is drawn hidden while the view is its own, in the room the row keeps for it.
        await expect(page.locator('button[aria-label="Save view"]')).toHaveAttribute(
          "data-control",
          CONTROL.viewPanel
        );
        await expect(page.locator('button[aria-label="Legion daemon"]')).toHaveAttribute(
          "data-control",
          CONTROL.viewPanel
        );
        if (layout === "board")
          await expect(control(controlRow(root), CONTROL.displayMenu)).toHaveCount(1);
      }
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a table layout and a Board choice stored before the views open as Open's layout, on the Board", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await page.evaluate((project) => {
      localStorage.setItem(
        `dispatch.project-table.${project}`,
        JSON.stringify({ visibility: { labels: false } })
      );
      localStorage.setItem("dispatch.project.issue-view:alice", "board");
    }, ACME);
    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    await expect(page.getByRole("button", { exact: true, name: "Board" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    await page.getByRole("button", { exact: true, name: "Table" }).click();
    const table = issuesTable(page);
    await expect(table).toBeVisible();
    const labels = await headerLabels(table);
    expect(labels).not.toContain("Labels");
    expect(labels).toContain("Assignee");
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The address's parameters a saved view keeps, as a sorted list of `key=value`. */
const viewParams = (url: string) =>
  [...new URL(url).searchParams.entries()].map(([key, value]) => `${key}=${value}`).sort();

/** The Board's cards, every key once, sorted. */
function boardCards(page: Page): Promise<string[]> {
  return page
    .locator("[data-board-card]")
    .evaluateAll((cards) => cards.map((card) => card.getAttribute("data-board-card") ?? "").sort());
}

/** Opens ACME's Open with a filter, a sort and a grouping, hides Labels, takes the Board and
 *  saves it as Test: the run of k4. */
async function saveTest(page: Page): Promise<void> {
  await page.goto(
    `/projects/${ACME}/issues?f.legion_phase=implementing&sort=-priority&group=assignee`
  );
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  await page.getByRole("button", { exact: true, name: "Columns" }).click();
  await page
    .getByRole("dialog", { name: "Columns" })
    .getByRole("checkbox", { exact: true, name: "Labels" })
    .uncheck();
  await page.keyboard.press("Escape");
  await page.getByRole("button", { exact: true, name: "Board" }).click();
  await saveView(page, "Test");
}

// SLOW(>=2s): a view saved, reloaded, opened from its tab, refused under three names, read in
// another project, deleted, saved and deleted in two tabs of one browser, and a stored value
// that does not parse read.
test("a saved view keeps its filter, sort, grouping, columns and layout through a reload, refuses a name in use, a built-in name and a long one, belongs to its project, and survives a save and a delete in a second tab of the same browser", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await createProject({ key: PLAIN, name: "Plain" });
    await createIssue({ project: PLAIN, title: "An issue of a project with no daemon" });
    const page = await context.newPage();
    await page.setViewportSize({ height: 1000, width: 1440 });
    await saveTest(page);
    const saved = viewParams(page.url());
    for (const wanted of [
      "f.legion_phase=implementing",
      "sort=-priority",
      "group=assignee",
      "layout=board",
    ])
      expect(saved).toContain(wanted);
    const cols = new URL(page.url()).searchParams.get("cols")?.split(",") ?? [];
    expect(cols).not.toContain("labels");
    expect(cols).toContain("assignee");
    const cards = await boardCards(page);
    const board = await getLegionBoard(ACME);
    const implementing = [...cardsOf(board)]
      .filter(([, placed]) => placed.column === "implementing")
      .map(([key]) => key)
      .sort();
    expect(cards).toEqual(implementing);

    await page.reload();
    await expect(page.getByRole("tab", { name: /^Test/ })).toHaveAttribute("aria-selected", "true");
    expect(viewParams(page.url())).toEqual(saved);
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    await expect.poll(() => boardCards(page)).toEqual(cards);
    await page.getByRole("tab", { name: /^Open/ }).click();
    await expect(page.getByRole("tab", { name: /^Open/ })).toHaveAttribute("aria-selected", "true");
    await page.getByRole("tab", { name: /^Test/ }).click();
    expect(viewParams(page.url())).toEqual(saved);
    await expect.poll(() => boardCards(page)).toEqual(cards);
    await page.getByRole("button", { exact: true, name: "Table" }).click();
    const table = issuesTable(page);
    expect(await headerLabels(table)).not.toContain("Labels");
    await page.getByRole("button", { exact: true, name: "Board" }).click();

    // A second view, and the three names a third may not take. Open keeps the Board it was left
    // on in this browser, so each address names the table.
    await page.goto(`/projects/${ACME}/issues?f.author=%40me&layout=table`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await saveView(page, "Mine");
    await page.goto(`/projects/${ACME}/issues?status=triage&layout=table`);
    await page.getByRole("button", { exact: true, name: "Save view" }).click();
    const panel = page.getByRole("dialog", { name: "Save view" });
    const name = panel.getByLabel("Name of the view");
    await name.fill("Test");
    await panel.getByRole("button", { exact: true, name: "Save" }).click();
    await expect(panel.getByRole("alert")).toHaveText(
      "A saved view has the name Test. Replace it?"
    );
    await expect(panel.getByRole("button", { exact: true, name: "Replace" })).toBeVisible();
    await name.fill("Done");
    await panel.getByRole("button", { exact: true, name: "Save" }).click();
    await expect(panel.getByRole("alert")).toHaveText(
      "Done is a built-in view. Type a different name."
    );
    await name.fill("Fifteen letters");
    expect("Fifteen letters".length).toBe(15);
    await panel.getByRole("button", { exact: true, name: "Save" }).click();
    await expect(panel.getByRole("alert")).toHaveText("Use 14 characters or fewer.");
    await panel.getByRole("button", { exact: true, name: "Cancel" }).click();
    expect(await tabTitles(page)).toEqual(["Open", "Done", "Test", "Mine"]);

    // The other project has no Test tab.
    await page.goto(`/projects/${PLAIN}/issues`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    expect(await tabTitles(page)).toEqual(["Open", "Done"]);

    // Deleted, Test is gone after a reload.
    await page.goto(`/projects/${ACME}/issues`);
    await page.getByRole("tab", { name: /^Test/ }).click();
    await page.getByRole("button", { exact: true, name: "Delete saved view Test" }).click();
    await page.reload();
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    expect(await tabTitles(page)).toEqual(["Open", "Done", "Mine"]);

    // A second tab of the same browser saves Two while the first is open. The first draws Two
    // with no reload, and its own save of One keeps Two. After a reload both tabs show both; a
    // delete of Two in the second tab keeps One, which the first tab saved.
    const second = await context.newPage();
    await second.setViewportSize({ height: 1000, width: 1440 });
    await second.goto(`/projects/${ACME}/issues?status=todo&layout=table`);
    await expect(second.locator("[data-issue-row]").first()).toBeVisible();
    await saveView(second, "Two");
    await expect(page.getByRole("tab", { name: /^Two(?:,|$)/ })).toBeVisible();
    await saveView(page, "One");
    await Promise.all(
      [page, second].map(async (on) => {
        await on.reload();
        await expect(on.getByRole("tab", { name: /^One(?:,|$)/ })).toBeVisible();
        expect(await tabTitles(on)).toEqual(["Open", "Done", "Mine", "Two", "One"]);
      })
    );
    await second.getByRole("tab", { name: /^Two(?:,|$)/ }).click();
    await second.getByRole("button", { exact: true, name: "Delete saved view Two" }).click();
    await expect(second.getByRole("tab", { name: /^Two(?:,|$)/ })).toHaveCount(0);
    await Promise.all(
      [page, second].map(async (on) => {
        await on.reload();
        await expect(on.getByRole("tab", { name: /^One(?:,|$)/ })).toBeVisible();
        expect(await tabTitles(on)).toEqual(["Open", "Done", "Mine", "One"]);
      })
    );
    await second.close();

    // A stored value that does not parse is dropped, and the built-in views draw.
    await page.evaluate(() =>
      localStorage.setItem("dispatch.project.saved-views:alice", "{not a stored view")
    );
    await page.reload();
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    expect(await tabTitles(page)).toEqual(["Open", "Done"]);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): three views saved by one login, read by a second login in the same browser and in
// a browser of its own.
test("a saved view is kept per login in the reader's browser, and its link opens the same view for another reader, with @me and Unread meaning that reader", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const first = await asUser(browser, "alice");
  let second: BrowserContext | undefined;
  try {
    // The second login's own issue, and two of the first's: one the second has read.
    const theirs = await createIssue(
      { project: ACME, title: "Gadget intake form for the second login" },
      { login: "bob" }
    );
    const unread = await createIssue({ project: ACME, title: "Sprocket ledger migration" });
    const read = await createIssue({ project: ACME, title: "Widget export rounding" });
    await putIssueState(
      read.key,
      { last_read_seq: (await getIssue(read.key)).last_seq },
      { login: "bob" }
    );

    const page = await first.newPage();
    await page.setViewportSize({ height: 1000, width: 1440 });
    await saveTest(page);
    const testUrl = page.url();
    const cards = await boardCards(page);
    await page.goto(`/projects/${ACME}/issues?f.author=%40me&layout=table`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await saveView(page, "Mine");
    const mineUrl = page.url();
    await page.goto(`/projects/${ACME}/issues?unread=1&layout=table`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    await saveView(page, "Unread");
    const unreadUrl = page.url();

    // The second login in the first's browser: no saved tab; back as the first, they are there.
    await signIn(first, "bob");
    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    expect(await tabTitles(page)).toEqual(["Open", "Done"]);
    await signIn(first, "alice");
    await page.reload();
    await expect(page.getByRole("tab", { name: /^Unread/ })).toBeVisible();
    expect(await tabTitles(page)).toEqual(["Open", "Done", "Test", "Mine", "Unread"]);

    // The second login's own browser: no saved tab, and each link opens its view for them.
    second = await asUser(browser, "bob");
    const other = await second.newPage();
    await other.setViewportSize({ height: 1000, width: 1440 });
    await other.goto(`/projects/${ACME}/issues`);
    await expect(other.locator("[data-issue-row]").first()).toBeVisible();
    expect(await tabTitles(other)).toEqual(["Open", "Done"]);
    await other.goto(testUrl);
    await expect(other.getByRole("region", { name: "Project board" })).toBeVisible();
    expect(viewParams(other.url())).toEqual(viewParams(testUrl));
    await expect.poll(() => boardCards(other)).toEqual(cards);

    const open = await listIssuesWhere({ open: true, project: ACME });
    const table = issuesTable(other);
    await other.goto(mineUrl);
    const bobs = open
      .filter((issue) => issue.created_by?.kind === "user" && issue.created_by.id === "bob")
      .map((issue) => issue.key);
    expect(bobs).toEqual([theirs.key]);
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(bobs);

    const state = await getMyState("bob");
    const unreadByBob = open
      .filter((issue) => issue.last_seq > (state[issue.key]?.last_read_seq ?? 0))
      .map((issue) => issue.key)
      .sort();
    expect(unreadByBob).toContain(unread.key);
    expect(unreadByBob).not.toContain(read.key);
    await other.goto(unreadUrl);
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(unreadByBob);
  } finally {
    await second?.close();
    await first.close();
    await daemon.close();
  }
});

/** The text nodes of the Issues tab a reader sees, outside its rows and cards, an open (i) hint,
 *  a tooltip, the Columns menu's method lines, an alert and the empty-state line. */
function visibleTexts(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const section = document.querySelector('section[aria-label="Project issues"]');
    if (section === null) throw new Error("the Issues tab draws no section");
    const outside = [
      "tr[data-issue-row]",
      "[data-board-card]",
      '[role="tooltip"]',
      '[role="alert"]',
      '[role="dialog"][aria-label="Legion daemon"]',
      "p[id]",
    ].join(",");
    const texts: string[] = [];
    const walker = document.createTreeWalker(section, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const text = (node.textContent ?? "").trim();
      const parent = node.parentElement;
      if (text === "" || parent === null || parent.closest(outside) !== null) continue;
      if (text === "No issues match these filters.") continue;
      const style = getComputedStyle(parent);
      if (style.visibility === "hidden" || parent.getClientRects().length === 0) continue;
      if (parent.closest(".sr-only") !== null) continue;
      texts.push(text);
    }
    return texts;
  });
}

// SLOW(>=2s): eight screens read at two widths, two views and two layouts, with every column.
test("the Issues tab's tabs, headers and words are short, the Legion headers are named as R9 names them, and no word names an epic", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const table = issuesTable(page);
    for (const width of [1280, 390]) {
      await page.setViewportSize({ height: 900, width });
      for (const view of ["open", "done"]) {
        for (const layout of ["table", "board"]) {
          const where = `${view} in the ${layout} at ${width}`;
          await page.goto(
            `/projects/${ACME}/issues?view=${view}&cols=${EVERY_COL}${layout === "board" ? "&layout=board" : ""}`
          );
          const names = () =>
            page
              .getByRole("tablist", { name: "Views" })
              .getByRole("tab")
              .evaluateAll((tabs) =>
                tabs.map((tab) => (tab.firstElementChild?.firstChild?.textContent ?? "").trim())
              );
          await expect.poll(names, { message: where }).toEqual(["Open", "Done"]);
          // The poll above settled the tab names on Open and Done, so this loop reads both.
          for (const name of await names()) expect(name.length).toBeLessThanOrEqual(14);
          if (layout === "table") {
            await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
            // The row wait above drew the table, and the table draws its whole head with its rows.
            const labels = await headerLabels(table);
            for (const label of labels)
              expect(label.length, `${where}: ${label}`).toBeLessThanOrEqual(20);
            expect(labels, where).toEqual(expect.arrayContaining(LEGION_HEADERS));
          } else await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
          await page.getByRole("button", { exact: true, name: "Legion daemon" }).click();
          const hint = page.getByRole("dialog", { name: "Legion daemon" });
          await expect(hint).toBeVisible();
          expect(await hint.locator("dt").allTextContents()).toEqual([
            "Capacity",
            "Trees",
            "Last read",
          ]);
          expect(await hint.locator("p").count(), `${where}: the hint holds prose`).toBe(0);
          await page.keyboard.press("Escape");
          // The waits above drew the tabs, the table or the Board and the hint, so these words
          // are drawn.
          const texts = await visibleTexts(page);
          for (const text of texts) {
            expect(text, `${where}: a sentence`).not.toMatch(/\.$/);
            expect(text, where).not.toMatch(/epic/i);
          }
          const main = page.getByTestId("main-content");
          await expect(control(controlRow(main), CONTROL.displayMenu)).toHaveAccessibleName(
            "Group by"
          );
        }
      }
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The filters k10 opens and reads every choice of, in the table's order. */
const FILTERED = [
  "Priority",
  "Queue",
  "Status",
  "Legion phase",
  "Assignee",
  "Created by",
  "Labels",
  "Claimed by",
  "Open asks",
  "Created",
  "Updated",
  "Workers",
  "Worker state",
  "Time waiting",
  "Why it waits",
  "Time in Legion phase",
  "Work time",
  "Review rounds",
  "Legion tree",
  "Time per phase",
];

// SLOW(>=2s): every filter, the Columns menu, both display menus, the selection bar and every
// grouping's headings read, at two widths.
test("no choice of a filter, the Columns menu, the display menus, the selection bar or a group heading names an epic or the queue outside the Queue column, and the menu's lines are the five methods", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const table = issuesTable(page);
    for (const width of [1280, 390]) {
      await page.setViewportSize({ height: 900, width });
      await page.goto(`/projects/${ACME}/issues?cols=${EVERY_COL}`);
      await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
      await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
      const where = `at ${width}`;
      for (const name of FILTERED) {
        const { options } = await readFilter(page, name);
        for (const [words] of options) {
          expect(words, `${where}: ${name}`).not.toMatch(/epic/i);
          if (name !== "Queue" && !(name === "Legion phase" && words === "Queue"))
            expect(words, `${where}: ${name}`).not.toMatch(/queue/i);
        }
      }
      const menu = await columnsMenu(page);
      for (const entry of menu) {
        expect(entry.name, where).not.toMatch(/epic/i);
        expect(entry.line, `${where}: the line under ${entry.name}`).toBe(
          METHODS[entry.name] ?? null
        );
      }
      for (const measured of Object.keys(METHODS))
        expect(menu.map((entry) => entry.name)).toContain(measured);
      // The display menu of each layout.
      for (const layout of ["table", "board"]) {
        if (layout === "board")
          await page.getByRole("button", { exact: true, name: "Board" }).click();
        const trigger = page.getByRole("button", { exact: true, name: "Group by" });
        await expect(trigger).toBeVisible();
        await trigger.click();
        const display = page.getByRole("dialog", { name: "Group by" });
        await expect(display).toBeVisible();
        expect(await display.innerText(), `${where}: the ${layout}'s display menu`).not.toMatch(
          /epic/i
        );
        await page.keyboard.press("Escape");
      }
      await page.getByRole("button", { exact: true, name: "Table" }).click();
      // Two rows selected: the selection bar.
      const rows = table.locator("tbody tr[data-issue-row]");
      await rows.nth(0).getByRole("checkbox").check();
      await rows.nth(1).getByRole("checkbox").check();
      const bar = page.getByRole("toolbar");
      await expect(bar).toBeVisible();
      expect(await bar.innerText(), `${where}: the selection bar`).not.toMatch(/epic/i);
      await page.getByRole("button", { exact: true, name: "Clear selection" }).click();
      // Each grouping's headings and their controls.
      for (const group of [
        "priority",
        "status",
        "legion_phase",
        "assignee",
        "author",
        "claim",
        "created",
        "updated",
      ]) {
        await page.goto(`/projects/${ACME}/issues?group=${group}`);
        await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
        // The row wait above drew the grouped table, which draws its group headings with its rows.
        const headings = await table
          .locator("tbody tr:not([data-issue-row]):not([aria-hidden])")
          .evaluateAll((lines) => lines.map((line) => (line.textContent ?? "").trim()));
        expect(headings.length, `${where}: no heading under group=${group}`).toBeGreaterThan(0);
        for (const heading of headings) expect(heading, `group=${group}`).not.toMatch(/epic/i);
      }
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** A claim on `issue` by a session whose title is 120 characters long, as `claim.e2e.ts` seeds its
 *  claim; `live` lists the session in the fake Envoy, so the chip reads it running. */
async function seedLongClaim(issue: string, live: boolean): Promise<string> {
  const holder = "Implementer whose session title runs on and on and on".padEnd(120, "!");
  const token = await mintAgentToken("views-claim", "alice");
  if (live) await setLiveSessions([{ session_id: "views-claim-session", title: holder }]);
  await claimIssue(issue, {
    actor: { id: "views-claim-session", kind: "session", origin: { session_title: holder } },
    as: "agent",
    token,
  });
  return holder;
}

/** Rests the pointer on `anchor` and answers the tooltip's lines, then moves it away and waits
 *  for the tooltip to close. */
async function hoverLines(page: Page, anchor: Locator): Promise<string[]> {
  await anchor.hover();
  await expect(page.getByRole("tooltip")).toBeVisible();
  const lines = await tooltipLines(page);
  await page.mouse.move(0, 0);
  await expect(page.getByRole("tooltip")).toHaveCount(0);
  return lines;
}

/** Tabs to `anchor` and answers the tooltip's lines, then presses Escape and waits for it to close. */
async function focusLines(page: Page, anchor: Locator, from: Locator): Promise<string[]> {
  await from.focus();
  await tabTo(page, anchor);
  await expect(page.getByRole("tooltip")).toBeVisible();
  const lines = await tooltipLines(page);
  await page.keyboard.press("Escape");
  await expect(page.getByRole("tooltip")).toHaveCount(0);
  return lines;
}

// SLOW(>=2s): six headers' tooltips each opened by the pointer and by the keyboard, then a Queue
// cell's and a long claim's.
test("each measure header's tooltip gives its method and the sort hint, on hover and on focus, a Queue number and a claim chip say what they are in the tooltip, and no sort button or chip has a title", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  // A failed read of the daemon first, and every board frame after the first held: neither the
  // sampler's own failed read nor a frame moves the table under the pointer while it rests.
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    const holder = await seedLongClaim("ACME-3", true);
    await page.setViewportSize({ height: 1000, width: 1440 });
    await page.goto(`/projects/${ACME}/issues?cols=${EVERY_COL}`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    const table = issuesTable(page);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    const sortButton = (label: string) =>
      header(table, label).getByRole("button", { exact: true, name: label });
    const start = sortButton("Priority");
    for (const [label, method] of Object.entries(METHODS)) {
      expect(await hoverLines(page, sortButton(label)), `${label} on hover`).toEqual([
        method,
        MOVABLE_HINT,
      ]);
      expect(await focusLines(page, sortButton(label), start), `${label} on focus`).toEqual([
        method,
        MOVABLE_HINT,
      ]);
    }
    expect(await hoverLines(page, sortButton("Status"))).toEqual([MOVABLE_HINT]);
    const titled = await table
      .locator("thead th button[draggable]")
      .evaluateAll((buttons) => buttons.filter((button) => button.hasAttribute("title")).length);
    expect(titled, "a sort button carries a title").toBe(0);

    // The number of a Queue cell.
    const place = table.locator('tr[data-issue-row="ACME-4"] [data-queue-place]');
    await expect(place).toHaveText("#1");
    expect(await hoverLines(page, place)).toEqual(["Legion starts issues in this order"]);
    await expect(page.getByRole("tooltip")).toHaveCount(0);
    await place.focus();
    await expect(page.getByRole("tooltip")).toHaveText("Legion starts issues in this order");
    await page.keyboard.press("Escape");
    const placeTitles = await table
      .locator("[data-queue-place]")
      .evaluateAll((places) => places.filter((one) => one.hasAttribute("title")).length);
    expect(placeTitles).toBe(0);

    // K's claim chip: the whole holder in the tooltip's first line, and what a claim is.
    const chip = table.locator('tr[data-issue-row="ACME-3"] [data-testid="issue-claim"]');
    await expect(chip).toBeVisible();
    for (const lines of [
      await hoverLines(page, chip),
      await focusLines(page, chip, sortButton("Key")),
    ]) {
      expect(lines[0]).toMatch(/^Claimed by /);
      expect(lines[0]).toContain(holder);
      expect(lines.join(" ")).toContain(CLAIM_SENTENCE);
    }
    // K's holder is cut where a box from its name up to the chip holds more than it shows. The
    // chip draws the name inside a span that cuts it with the ellipsis, and the name itself is an
    // inline box, which reads no width of its own and so never reads as cut.
    const chips = await table.locator('[data-testid="issue-claim"]').evaluateAll((all) =>
      all.map((one) => ({
        cut: [...one.querySelectorAll("[data-agent-name]")].some((name) => {
          let box: Element | null = name;
          while (box !== null && box !== one) {
            if (box.scrollWidth > box.clientWidth) return true;
            box = box.parentElement;
          }
          return false;
        }),
        titled: one.hasAttribute("title"),
      }))
    );
    expect(
      chips.some((one) => one.titled),
      "a claim chip carries a title"
    ).toBe(false);
    expect(chips[0]?.cut, "K's holder is not cut in the table").toBe(true);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): the facts consumer's row of ACME-1, up to 75 s, then its card, an open ask's badge
// and a claim read on the Board and on the issue page.
test("a Board card's Time in Legion phase, Work time, open asks and claim say what they are in the tooltip, on hover and on focus, and carry no title", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await writeRoleEvent("ACME-1", "implementer", "2 hours");
    await waitForIssueFacts(["ACME-1"]);
    await createAsk(
      "ACME-1",
      { question: "Which widget ships first?" },
      {
        actor: { id: "views-asker", kind: "session" },
        as: "agent",
      }
    );
    // K: an issue the API created, which has an issue page, with a card in Implementing.
    const claimed = (await createIssue({ project: ACME, title: "Claimed by a long session" })).key;
    await patchIssue(claimed, { status: "in_progress" });
    await pushLegionObservation(claimed, "phase", '"implementing"');
    const holder = await seedLongClaim(claimed, true);
    const page = await context.newPage();
    await page.setViewportSize({ height: 1000, width: 1440 });
    await page.goto(`/projects/${ACME}/issues?layout=board&group=legion_phase`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    const card = page.locator('[data-board-card="ACME-1"]');
    await expect(card.locator('[data-legion-fact="work-time"]')).toBeVisible();
    const start = page.getByRole("button", { exact: true, name: "Group by" });
    for (const [hook, method] of [
      ["phase-time", METHODS["Time in Legion phase"]],
      ["work-time", METHODS["Work time"]],
    ] as const) {
      const fact = card.locator(`[data-legion-fact="${hook}"]`);
      expect(await hoverLines(page, fact), `${hook} on hover`).toEqual([method]);
      expect(await focusLines(page, fact, start), `${hook} on focus`).toEqual([method]);
    }
    const asks = card.locator('[data-open-asks="1"]');
    expect(await hoverLines(page, asks), "the open asks on hover").toEqual(["1 open ask"]);
    expect(await focusLines(page, asks, start), "the open asks on focus").toEqual(["1 open ask"]);
    expect(await asks.getAttribute("title")).toBeNull();
    const chip = page.locator(`[data-board-card="${claimed}"]`).getByTestId("issue-claim");
    const lines = await hoverLines(page, chip);
    expect(lines[0]).toMatch(/^Claimed by /);
    expect(lines[0]).toContain(holder);
    expect(lines.join(" ")).toContain(CLAIM_SENTENCE);
    expect(await chip.getAttribute("title")).toBeNull();

    // K's issue page: the claim chip in the header.
    await page.goto(`/issues/${claimed}`);
    const header = page.getByTestId("issue-claim");
    await expect(header).toBeVisible();
    const headerLines = await hoverLines(page, header);
    expect(headerLines[0]).toMatch(/^Claimed by /);
    expect(headerLines[0]).toContain(holder);
    expect(headerLines.join(" ")).toContain(CLAIM_SENTENCE);
    expect(await header.getAttribute("title")).toBeNull();
    // The chip was waited for above, and this one read only picks the words of a note that no
    // check reads. The name is cut where a box from it up to the chip holds more than it shows:
    // the chip may draw it inside a span that cuts it with the ellipsis, and the name itself is an
    // inline box, which reads no width of its own and so never reads as cut.
    const cut = await header
      .locator("[data-agent-name]")
      .first()
      .evaluate((name) => {
        const chip = name.closest('[data-testid="issue-claim"]');
        let box: Element | null = name;
        while (box !== null && box !== chip) {
          if (box.scrollWidth > box.clientWidth) return true;
          box = box.parentElement;
        }
        return false;
      });
    test.info().annotations.push({
      description: `the issue page's chip ${cut ? "cuts" : "draws whole"} the holder's name`,
      type: "claim chip",
    });
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** Every sort button of every table `page` draws carries no title, and one per table opens the
 *  tooltip with the sort hint. */
async function expectSortHints(page: Page, where: string): Promise<void> {
  const tables = page.locator('[data-control="data-table"] table');
  await expect(tables.first()).toBeVisible();
  await expect(page.getByRole("status", { name: "Loading issues" })).toHaveCount(0);
  // The waits above drew the first table and ended the loading of each issue table, so every
  // table of the page is drawn.
  const count = await tables.count();
  for (let index = 0; index < count; index++) {
    const buttons = tables.nth(index).locator("thead th button[draggable]");
    const titled = await buttons.evaluateAll(
      (all) => all.filter((button) => button.hasAttribute("title")).length
    );
    expect(titled, `${where}: a sort button of table ${index + 1} carries a title`).toBe(0);
    // A table draws its whole head when it draws, so this count of its sort buttons is whole.
    if ((await buttons.count()) === 0) continue;
    const lines = await hoverLines(page, buttons.first());
    expect(lines.join(" "), `${where}: table ${index + 1}`).toContain(SORT_HINT);
  }
}

// SLOW(>=2s): four native titles of the Issues tab read, then the priority pages seeded and read.
test("the sort buttons of the priority pages carry no title and give the sort hint, and the four native titles R9 keeps read as they did", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const priority = await createPriority({ owner: "alice", title: "Ship the widgets first" });
    await patchIssue("ACME-1", { advances: [priority.id] });
    // A route that reaches nobody, a child for ACME-1's progress, and a claim by a session the
    // listener does not hold.
    await patchIssue("ACME-3", { route: "role:views-nobody" });
    const child = await createIssue({
      project: ACME,
      title: "A child of the widgets",
      parent: "ACME-1",
    });
    await seedLongClaim("ACME-4", false);
    const page = await context.newPage();
    await page.setViewportSize({ height: 1000, width: 1440 });

    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    const table = issuesTable(page);
    const created = table.locator('tr[data-issue-row="ACME-2"] time').first();
    await expect(created).toBeVisible();
    const at = await created.getAttribute("datetime");
    expect(await created.getAttribute("title")).toBe(
      await page.evaluate((moment) => new Date(moment ?? "").toLocaleString(), at)
    );
    await expect(
      table.locator('tr[data-issue-row="ACME-1"] [data-testid="issue-progress-children"]')
    ).toHaveAttribute("title", "0 of 1 child issue is done.");
    expect(child.parent).toBe("ACME-1");
    await expect(
      table.locator('tr[data-issue-row="ACME-3"] [data-testid="issue-route-unreachable"]')
    ).toHaveAttribute(
      "title",
      "No running session holds role:views-nobody right now, so this issue's messages reach nobody at the moment. A session that is restarting or moving between boxes drops out of the listener for minutes and comes back under the same id, so this is a vacancy only when it is still true on a read ten minutes later; then staff the role, re-route it to a live holder, or clear the route and assign it."
    );
    await expect(
      table.locator('tr[data-issue-row="ACME-4"] [data-testid="issue-claim"] [role="img"]')
    ).toHaveAttribute("title", "Not running");
    await page.goto("/priorities");
    await expectSortHints(page, "/priorities");
    await page.goto(`/priorities/${priority.id}`);
    await expectSortHints(page, "a priority's page");
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("the sort buttons of the Delivery page's table carry no title and give the sort hint", async ({
  browser,
}) => {
  await seedSessionGroupsFixture();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 1000, width: 1440 });
    await page.goto("/delivery?mode=list");
    await expectSortHints(page, "/delivery");
  } finally {
    await context.close();
  }
});

/** k10's measure columns, by header. */
const MEASURES = [
  "Work time",
  "Time per phase",
  "Review rounds",
  "Time waiting",
  "Time in Legion phase",
];

// SLOW(>=2s): the facts consumer's rows of two issues, up to 75 s, then the board's frames
// rewritten and five measure columns, their filters, a sort and two cards read.
test("a missing measure draws an empty cell, sorts last and counts under None, and a counted zero reads 0", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    // M (ACME-1) loses its measured work, Z (ACME-2) its review rounds, C (ACME-4) its times;
    // N has no Legion work and no card. M's entry also loses its growth: an entry with no
    // measured work does not grow, so the route sends null for its as_of and its growth.
    const [m, z, c] = ["ACME-1", "ACME-2", "ACME-4"];
    await writeRoleEvent(m, "implementer", "2 hours");
    await writeRoleEvent(z, "architect", "45 minutes");
    await waitForIssueFacts([m, z]);
    const n = (await createIssue({ project: ACME, title: "No Legion work and no card" })).key;
    const rewrite = (board: BoardParts) => {
      for (const column of board.columns ?? [])
        for (const card of column.cards ?? [])
          if (card.key === c) {
            card.phase_since = null;
            if (card.waiting !== null) card.waiting.since = null;
          }
      const issues = board.issues ?? {};
      const facts = (key: string) => issues[key] as Record<string, unknown> | null | undefined;
      // facts reads an entry of the issues map this rewrite is handed, and it reads no page.
      const mFacts = facts(m);
      if (mFacts != null)
        Object.assign(mFacts, {
          as_of: null,
          growth: null,
          hours_by_phase: {},
          work_first_at: null,
          work_hours: 0,
        });
      // facts reads an entry of the issues map this rewrite is handed, and it reads no page.
      const zFacts = facts(z);
      if (zFacts != null) zFacts.review_rounds = 0;
    };
    const page = await context.newPage();
    await routeLive(page, {
      edit: (frame) => {
        if (frame.body !== undefined) rewrite(frame.body);
        rewrite({ columns: frame.set?.columns, issues: frame.maps?.issues });
      },
    });
    await page.setViewportSize({ height: 1400, width: 1920 });
    await page.goto(`/projects/${ACME}/issues?cols=${EVERY_COL}`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    const table = issuesTable(page);
    await expect(table.locator(`tr[data-issue-row="${n}"]`)).toBeVisible();

    // The route as the page is sent it.
    const routed = async () => {
      const board = await getLegionBoard(ACME);
      rewrite(board);
      return board;
    };
    const board = await routed();
    expect(board.issues?.[m], "the board's issues map holds no facts of M").toBeDefined();
    expect(board.issues?.[z], "the board's issues map holds no facts of Z").toBeDefined();
    expect(board.issues?.[n]).toBeUndefined();
    expect(cardsOf(board).has(n)).toBe(false);
    await expect
      .poll(async () => {
        const cells = await readCells(table);
        return Object.fromEntries(
          [m, z, c, n].map((key) => [key, MEASURES.map((measure) => cells[key]?.[measure] ?? "?")])
        );
      })
      .toEqual({
        [m]: ["", "", expect.any(String), expect.any(String), expect.any(String)],
        [z]: [expect.any(String), expect.any(String), "0", expect.any(String), expect.any(String)],
        [c]: [expect.any(String), expect.any(String), expect.any(String), "", ""],
        [n]: ["", "", "", "", ""],
      });
    // The poll above waited for the rewritten frame, and one frame draws the measure cells of
    // every row.
    const cells = await readCells(table);
    for (const [key, row] of Object.entries(cells))
      for (const measure of MEASURES)
        expect(row[measure] ?? "", `${key} ${measure}`).not.toMatch(/^0 [mh]\b/);

    // The filters' counts, from the route as rewritten.
    const open = await listIssuesWhere({ open: true, project: ACME });
    const cards = cardsOf(board);
    const factsOf = (key: string) => board.issues?.[key];
    const work = (key: string) => measuredWork(boardFacts(board, key));
    const none = (picked: (key: string) => boolean) =>
      open.filter((issue) => picked(issue.key)).length;
    const workTime = (await readFilter(page, "Work time")).options;
    expect(workTime.find(([words]) => words === "None")?.[1]).toBe(
      none((key) => work(key) === undefined)
    );
    const rounds = (await readFilter(page, "Review rounds")).options;
    expect(rounds.find(([words]) => words === "0")?.[1]).toBe(
      none((key) => factsOf(key)?.review_rounds === 0)
    );
    expect(rounds.find(([words]) => words === "None")?.[1]).toBe(
      none((key) => factsOf(key) === undefined)
    );
    const waited = (await readFilter(page, "Time waiting")).options;
    expect(waited.find(([words]) => words === "None")?.[1]).toBe(
      none((key) => cards.get(key)?.card.waiting?.since == null)
    );
    const inPhase = (await readFilter(page, "Time in Legion phase")).options;
    expect(inPhase.find(([words]) => words === "None")?.[1]).toBe(
      none((key) => cards.get(key)?.card.phase_since == null)
    );
    // Time per phase offers the phases with hours, in the board's order with Other last, and no
    // None; M, whose hours are gone, and N count under none of them.
    const phases = (await readFilter(page, "Time per phase")).options;
    const counted = new Map<string, number>();
    for (const issue of open) {
      const held = new Set(
        Object.entries(factsOf(issue.key)?.hours_by_phase ?? {}).flatMap(([phase, hours]) =>
          hours > 0 ? [PHASE_WORDS.find(([id]) => id === phase)?.[1] ?? "Other"] : []
        )
      );
      for (const words of held) counted.set(words, (counted.get(words) ?? 0) + 1);
    }
    expect(phases).toEqual(
      PHASE_WORDS.flatMap(([, words]) => {
        const count = counted.get(words);
        return count === undefined ? [] : [[words, count] as const];
      })
    );
    expect(Object.keys(factsOf(m)?.hours_by_phase ?? {})).toEqual([]);
    expect(factsOf(n)).toBeUndefined();

    // Under sort=work_hours M follows every row with a work time.
    await page.goto(`/projects/${ACME}/issues?cols=${EVERY_COL}&sort=work_hours`);
    await expect(table.locator(`tr[data-issue-row="${n}"]`)).toBeVisible();
    const order = await rowKeys(table);
    const measured = open
      .filter((issue) => work(issue.key) !== undefined)
      .map((issue) => issue.key);
    expect(measured.length).toBeGreaterThan(0);
    for (const key of measured) expect(order.indexOf(key)).toBeLessThan(order.indexOf(m));

    // On the Board M's card draws no Work time and C's no Time in Legion phase, and no dash.
    await page.goto(`/projects/${ACME}/issues?layout=board&group=legion_phase`);
    await expect(page.locator(`[data-board-card="${m}"]`)).toBeVisible();
    await expect(
      page.locator(`[data-board-card="${m}"] [data-legion-fact="work-time"]`)
    ).toHaveCount(0);
    await expect(
      page.locator(`[data-board-card="${c}"] [data-legion-fact="phase-time"]`)
    ).toHaveCount(0);
    for (const key of [m, c])
      await expect(page.locator(`[data-board-card="${key}"]`)).not.toContainText("—");
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): three addresses of Open read in the table and on the Board, with a seeded assignee.
test("each count on the tabs, the pager, the bar, the Board's columns and the group headings counts its own set, as R9's exceptions allow", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    // bob's open issues are all Todo.
    for (const title of ["Todo for bob", "Another todo for bob"]) {
      const issue = await createIssue({ project: ACME, title });
      await patchIssue(issue.key, { assignee: "bob", status: "todo" });
    }
    const open = await listIssuesWhere({ open: true, project: ACME });
    const done = await listIssuesWhere({ project: ACME, status: ["done"] });
    const page = await context.newPage();
    await page.setViewportSize({ height: 1000, width: 1440 });
    const table = issuesTable(page);
    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
    await page.getByRole("tab", { name: /^Done/ }).click();
    await page.getByRole("tab", { name: /^Open/ }).click();
    await page.goto(`/projects/${ACME}/issues?f.priority=0`);
    const urgent = open.filter((issue) => issue.priority === 0);
    await expect.poll(() => rowKeys(table)).toHaveLength(urgent.length);
    expect(await tabNames(page)).toEqual([tabName("Open", urgent.length, true), "Done"]);
    const pager = page.getByTestId("issues-table-pager");
    await expect(pager).toContainText(`Rows 1–${urgent.length}`);
    await expect(pager).toContainText("Page 1 of 1");
    // The page size is a single pick, named for its list and then for what it shows.
    await expect(listTrigger(pager, "Rows per page")).toHaveAccessibleName(
      "Rows per page 50 per page"
    );
    // The range reads no total, and no line counts the issues: the tab counts them.
    await expect(pager).not.toContainText(/Rows 1–\d+ of \d+/);
    await expect(pager).not.toContainText(/\bissues?\b/);
    // Done counts its own view once it has been opened in the page.
    await page.getByRole("tab", { name: /^Done/ }).click();
    await page.getByRole("tab", { name: /^Open/ }).click();
    await expect
      .poll(() => tabNames(page))
      .toEqual([tabName("Open", open.length), tabName("Done", done.length)]);

    // One status: the bar's one entry and the Board's one column each count the tab's rows.
    const working = open.filter((issue) => issue.status === "in_progress");
    await page.goto(`/projects/${ACME}/issues?status=in_progress`);
    await expect.poll(() => rowKeys(table)).toHaveLength(working.length);
    expect(await tabNames(page)).toEqual([tabName("Open", working.length, true), "Done"]);
    const legend = page
      .getByRole("region", { name: "Issues by status" })
      .locator("[data-bar-legend]");
    expect(await legend.allTextContents()).toEqual([`In progress ${working.length}`]);
    await page.getByRole("button", { exact: true, name: "Board" }).click();
    const columns = page
      .getByRole("region", { name: "Project board" })
      .locator("section[data-board-column]");
    await expect(columns.first()).toBeVisible();
    const titled = await columns.evaluateAll((sections) =>
      sections
        .filter((section) => !(section.getAttribute("aria-label") ?? "").endsWith("(collapsed)"))
        .map((section) => ({
          count: (
            section.querySelector('[data-testid="board-column-header"]')?.textContent ?? ""
          ).replace(/^\D+/, ""),
          title: section.getAttribute("aria-label"),
        }))
    );
    expect(titled).toEqual([{ count: String(working.length), title: "In progress" }]);
    await expect(page.getByRole("region", { name: "Issues by status" })).toHaveCount(0);
    await page.getByRole("button", { exact: true, name: "Table" }).click();

    // Group headings: Todo's size, its status bar's count and the legend's Todo count are one
    // number; bob's heading and its status bar are another.
    const todo = open.filter((issue) => issue.status === "todo").length;
    await page.goto(`/projects/${ACME}/issues?group=status&size=250`);
    await expect(page.getByRole("button", { name: `Collapse Todo, ${todo} issues` })).toBeVisible();
    await expect(
      table.locator("tbody tr").filter({
        has: page.getByRole("button", { name: `Collapse Todo, ${todo} issues` }),
      })
    ).toContainText(`${todo} todo`);
    await expect(page.locator('[data-bar-legend="todo"]')).toHaveText(`Todo ${todo}`);
    const bobs = open.filter((issue) => issue.assignee === "bob");
    expect(bobs.every((issue) => issue.status === "todo")).toBe(true);
    await page.goto(`/projects/${ACME}/issues?group=assignee&size=250`);
    await expect(
      page.getByRole("button", { name: `Collapse bob, ${bobs.length} issues` })
    ).toBeVisible();
    await expect(
      table.locator("tbody tr").filter({
        has: page.getByRole("button", { name: `Collapse bob, ${bobs.length} issues` }),
      })
    ).toContainText(`${bobs.length} todo`);
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The stored layout of ACME's Open in this browser. */
function storedLayout(page: Page): Promise<{ sizing?: Record<string, number>; order?: string[] }> {
  return page.evaluate(
    (project) => JSON.parse(localStorage.getItem(`dispatch.project-table.${project}`) ?? "{}"),
    ACME
  );
}

test("on a desktop a Title width the reader drags is the width drawn after a reload, and Title scrolls with the table once the pinned columns would pass their share", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    await page.setViewportSize({ height: 900, width: 1400 });
    await page.goto(`/projects/${ACME}/issues`);
    const table = issuesTable(page);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    const title = header(table, "Title");
    await expect(title).toBeVisible();
    // The wait above drew the Title header, and a drawn element has a box, so the box is there.
    const before = await title.boundingBox();
    if (before === null) throw new Error("the Title header has no box");
    await page.mouse.move(before.x + before.width - 2, before.y + before.height / 2);
    await page.mouse.down();
    await page.mouse.move(before.x + before.width + 300, before.y + before.height / 2, {
      steps: 12,
    });
    await page.mouse.up();
    await expect
      .poll(async () => (await storedLayout(page)).sizing?.title ?? 0)
      .toBeGreaterThan(before.width + 200);
    await page.reload();
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    const stored = (await storedLayout(page)).sizing?.title ?? 0;
    const drawn = (await title.boundingBox())?.width ?? 0;
    expect(Math.abs(drawn - stored), `drawn ${drawn} px, stored ${stored} px`).toBeLessThanOrEqual(
      1
    );

    // The pinned share: Title widened past it scrolls with the table.
    const box = page.getByTestId("issues-table-scroll");
    const share = await box.evaluate((node) => node.clientWidth * 0.6);
    const lead = await table
      .locator("thead th")
      .evaluateAll((cells) =>
        cells.slice(0, 3).reduce((sum, cell) => sum + cell.getBoundingClientRect().width, 0)
      );
    expect(lead + stored, "Title is not wide enough to pass the pinned share").toBeGreaterThan(
      share
    );
    const left = (await title.boundingBox())?.x ?? 0;
    await box.evaluate((node) => {
      node.scrollLeft = node.scrollWidth;
    });
    await expect.poll(() => box.evaluate((node) => node.scrollLeft)).toBeGreaterThan(0);
    expect((await title.boundingBox())?.x ?? 0, "Title stayed pinned").toBeLessThan(left - 1);
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** Drags header `from` onto header `onto` with the HTML5 drag events a browser sends, sharing one
 *  `DataTransfer`, the drop a few frames after the start as a real drag lands. */
async function dropHeader(page: Page, from: string, onto: string): Promise<void> {
  await page.evaluate(
    async ([dragged, target]) => {
      const headerOf = (label: string): HTMLElement | undefined =>
        [...document.querySelectorAll<HTMLElement>("thead th")].find(
          (cell) =>
            (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, "").trim() === label
        );
      const handle = headerOf(dragged)?.querySelector("button");
      const to = headerOf(target);
      if (handle === null || handle === undefined || to === undefined)
        throw new Error(`the ${dragged} and ${target} headers are not both drawn`);
      const dataTransfer = new DataTransfer();
      handle.dispatchEvent(new DragEvent("dragstart", { bubbles: true, dataTransfer }));
      for (let frame = 0; frame < 3; frame++) {
        const drawn = Promise.withResolvers<void>();
        requestAnimationFrame(() => drawn.resolve());
        await drawn.promise;
      }
      to.dispatchEvent(
        new DragEvent("dragover", { bubbles: true, cancelable: true, dataTransfer })
      );
      to.dispatchEvent(new DragEvent("drop", { bubbles: true, dataTransfer }));
      handle.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer }));
    },
    [from, onto] as const
  );
}

test("on a phone Select and Key are pinned, Title follows them to the table's right edge, and a header dropped among the others or onto Title stays where it was dropped", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/projects/${ACME}/issues`);
    const table = issuesTable(page);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    const box = page.getByTestId("issues-table-scroll");
    const title = header(table, "Title");
    const before = (await title.boundingBox())?.x ?? 0;
    await box.evaluate((node) => {
      node.scrollLeft = 200;
    });
    await expect.poll(() => box.evaluate((node) => node.scrollLeft)).toBeGreaterThan(0);
    expect((await title.boundingBox())?.x ?? 0, "Title stayed pinned").toBeLessThan(before - 1);
    const pinned = () =>
      table
        .locator("thead th")
        .evaluateAll((cells) => cells.slice(0, 2).map((cell) => cell.getBoundingClientRect().left));
    const scrolled = await pinned();
    await box.evaluate((node) => {
      node.scrollLeft = 0;
    });
    await expect.poll(() => box.evaluate((node) => node.scrollLeft)).toBe(0);
    expect(await pinned(), "Select and Key scrolled").toEqual(scrolled);
    const labels = await headerLabels(table);
    expect(labels.slice(0, 2), "Key and Title do not follow Select").toEqual(["Key", "Title"]);
    await box.evaluate((node) => {
      node.scrollLeft = 0;
    });
    // Title fills the rest of the row: its right edge is the box's.
    const edges = await box.evaluate((node) => {
      const title = node.querySelectorAll("thead th")[2]?.getBoundingClientRect();
      return {
        box: node.getBoundingClientRect().left + node.clientWidth,
        title: title?.right ?? 0,
      };
    });
    expect(
      Math.abs(edges.title - edges.box),
      `Title ends at ${edges.title}, the box at ${edges.box}`
    ).toBeLessThanOrEqual(1);

    // Status dropped onto the column after Title lands before it; the last header dropped onto
    // Title lands right after Title.
    const afterTitle = labels[2] ?? "";
    await dropHeader(page, "Status", afterTitle);
    const once = [
      ...labels.slice(0, 2),
      "Status",
      ...labels.slice(2).filter((label) => label !== "Status"),
    ];
    await expect.poll(() => headerLabels(table)).toEqual(once);
    const last = once[once.length - 1] ?? "";
    await dropHeader(page, last, "Title");
    const twice = [...once.slice(0, 2), last, ...once.slice(2, -1)];
    await expect.poll(() => headerLabels(table)).toEqual(twice);
    await page.reload();
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    expect(await headerLabels(table)).toEqual(twice);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R2 and R5, found by INFRA-3221's tester: a saved view whose filter reads the board counts its
// rows from the board, so while the board's first snapshot has not answered its tab gives no
// count, or the count the board gives - never a count of rows the board has not placed yet.
// SLOW(>=2s): ACME seeded, three views saved, and the Issues tab opened again with the board's
// first snapshot held.
test("a saved view's tab gives no count that the board read it needs has not answered", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const saved = [
      ["Queue", "f.queue=Queued"],
      ["Impl", "f.legion_phase=implementing"],
      ["NoPhase", "f.legion_phase=none"],
    ] as const;
    for (const [name, address] of saved) {
      await page.goto(`/projects/${ACME}/issues?${address}`);
      await expect(page.getByRole("tablist", { name: "Views" })).toBeVisible();
      await saveView(page, name);
    }
    // Each view's rows, from the routes: the open listing and the board's cards.
    const cards = cardsOf(await getLegionBoard(ACME));
    const open = await listIssuesWhere({ project: ACME, open: true });
    const listed = (keep: (key: string) => boolean) =>
      open.filter((issue) => keep(issue.key)).length;
    const counts: Record<(typeof saved)[number][0], number> = {
      Queue: listed((key) => {
        const placed = cards.get(key);
        return placed?.column === "queue" || (placed?.card.waiting_place ?? null) !== null;
      }),
      Impl: listed((key) => cards.get(key)?.column === "implementing"),
      NoPhase: listed((key) => !cards.has(key)),
    };
    const tab = (on: Page, name: string) =>
      on
        .getByRole("tablist", { name: "Views" })
        .getByRole("tab", { name: new RegExp(`^${name}\\b`) });
    await page.getByRole("tab", { name: /^Open/ }).click();
    for (const [name] of saved)
      await expect(tab(page, name)).toHaveAttribute("aria-label", tabName(name, counts[name]));

    const held = await context.newPage();
    const { promise: gate, resolve: release } = Promise.withResolvers<void>();
    await routeLive(held, { first: () => gate });
    await held.goto(`/projects/${ACME}/issues`);
    await expect(held.locator("[data-issue-row]").first()).toBeVisible();
    for (const [name] of saved)
      expect(
        [name, tabName(name, counts[name])],
        `the ${name} tab while the board's first snapshot is held`
      ).toContain(await tab(held, name).getAttribute("aria-label"));
    release();
    for (const [name] of saved)
      await expect(tab(held, name)).toHaveAttribute("aria-label", tabName(name, counts[name]));
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R2, found by INFRA-3221's tester: an address with no `view` is Open, however the reader reached
// it. The project's own link in the page's header leaves the Issues tab drawn, so the saved view
// open before it must not stay the open tab over Open's rows.
// SLOW(>=2s): ACME seeded, a view saved, and the header's link followed from it.
test("the project's link in the header opens Open, unchanged, from an open saved view", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/projects/${ACME}/issues?status=todo`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await saveView(page, "Todo");
    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await page.getByRole("tab", { name: /^Todo/ }).click();
    await expect(page.getByRole("tab", { name: /^Todo/ })).toHaveAttribute("aria-selected", "true");

    await page.getByRole("link", { exact: true, name: ACME }).click();
    await expect.poll(() => new URL(page.url()).search).toBe("");
    const open = await listIssuesWhere({ project: ACME, open: true });
    const openTab = page.getByRole("tab", { name: /^Open/ });
    await expect(openTab).toHaveAttribute("aria-selected", "true");
    await expect(openTab).toHaveAttribute("aria-label", tabName("Open", open.length));
    await expect(page.getByRole("button", { exact: true, name: "Clear filters" })).toBeHidden();
    await expect(page.getByRole("button", { exact: true, name: "Save view" })).toBeHidden();
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R2, found by INFRA-3221's tester: every tab but the open one counts its own view, so a saved
// view that leaves a tree out (`exclude_tree`, which an Evals link writes) gives the same count
// from Open's tab as it does open.
// SLOW(>=2s): ACME seeded, a child added, and a view that leaves its tree out saved and counted.
test("a saved view that leaves a tree out counts from another tab the rows it lists open", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const child = await createIssue({
      project: ACME,
      title: "A child of ACME-1",
      parent: "ACME-1",
    });
    await patchIssue(child.key, { status: "todo" });
    const page = await context.newPage();
    const table = issuesTable(page);
    await page.goto(`/projects/${ACME}/issues?exclude_tree=ACME-1`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await expect.poll(async () => (await rowKeys(table)).includes("ACME-1")).toBe(false);
    await saveView(page, "NoTree");
    const shown = (await rowKeys(table)).length;
    await expect(page.getByRole("tab", { name: /^NoTree/ })).toHaveAttribute(
      "aria-label",
      tabName("NoTree", shown)
    );
    await page.getByRole("tab", { name: /^Open/ }).click();
    await expect(page.getByRole("tab", { name: /^Open/ })).toHaveAttribute("aria-selected", "true");
    await expect(page.getByRole("tab", { name: /^NoTree/ })).toHaveAttribute(
      "aria-label",
      tabName("NoTree", shown)
    );
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R2, found by INFRA-3221's tester: a saved view that lists closed issues shows a count only while
// it is the open tab, and a pick in another case is the same pick (R14), so `status=Done` lists
// closed issues as `status=done` does and gives no count from Open's tab.
// SLOW(>=2s): ACME seeded, and a view of `status=Done` saved and read from Open's tab.
test("a saved view of status=Done gives no count from Open's tab, as one of status=done does", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const done = await listIssuesWhere({ project: ACME, status: ["done"] });
    const page = await context.newPage();
    await page.goto(`/projects/${ACME}/issues?status=Done`);
    await expect.poll(async () => (await rowKeys(issuesTable(page))).length).toBe(done.length);
    await saveView(page, "Shipped");
    await page.getByRole("tab", { name: /^Open/ }).click();
    await expect(page.getByRole("tab", { name: /^Open/ })).toHaveAttribute("aria-selected", "true");
    await expect(page.getByRole("tab", { name: /^Shipped/ })).toHaveAttribute(
      "aria-label",
      tabName("Shipped")
    );
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R2, found by INFRA-3221's tester: the Done tab shows its count once that view has been opened
// in the page, in either layout, so a Done kept on its Board keeps its count on Open's tab too.
// SLOW(>=2s): ACME seeded, Done switched to its Board, and the page loaded again on Open.
test("Done opened on its Board keeps its count on the tab when the reader goes back to Open", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const done = await listIssuesWhere({ project: ACME, status: ["done"] });
    const page = await context.newPage();
    await page.goto(`/projects/${ACME}/issues?view=done`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await page.getByRole("button", { exact: true, name: "Board" }).click();
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await page.getByRole("tab", { name: /^Done/ }).click();
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    await expect(page.getByRole("tab", { name: /^Done/ })).toHaveAttribute(
      "aria-label",
      tabName("Done", done.length)
    );
    await page.getByRole("tab", { name: /^Open/ }).click();
    await expect(page.getByRole("tab", { name: /^Open/ })).toHaveAttribute("aria-selected", "true");
    await expect(page.getByRole("tab", { name: /^Done/ })).toHaveAttribute(
      "aria-label",
      tabName("Done", done.length)
    );
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R4 and the Design's columns, found by INFRA-3221's tester: a measure header's tooltip closes on
// Escape, so one Escape closes it while a filter's panel is open too, as it closes the panel.
// SLOW(>=2s): ACME seeded, a filter panel opened and a measure header's tooltip shown over it.
test("one Escape closes a measure header's tooltip shown while a filter's panel is open", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/projects/${ACME}/issues?cols=key,title,status,work_hours,priority`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await page
      .getByRole("button", { name: /^Filter Status/ })
      .first()
      .click();
    await expect(page.getByRole("listbox", { name: "Status options" })).toBeVisible();
    await issuesTable(page)
      .locator("thead")
      .getByRole("button", { name: /^Work time/ })
      .first()
      .hover();
    await expect(page.getByRole("tooltip")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("listbox", { name: "Status options" })).toBeHidden();
    await expect(page.getByRole("tooltip")).toBeHidden();
  } finally {
    await context.close();
    await daemon.close();
  }
});
