/** The bar, the Board's columns and every key move and drop are read against the server's own
 *  listing and board route, each write read back through the API, and a later read of the daemon
 *  reaches the Board over the live socket. */
// Open's status bar and the Board as a layout of the view (dispatch://INFRA-3221, Verification k7
// and k14): the bar's segments and legend against the rows the table lists, the legend at four
// widths, and no bar on Done or on a Board; the display menu's name and choices on every page
// that draws one, the Board's columns and rails, its cards against the table's rows, and every
// key move and drop by Status and by Legion phase.
//
// ACME is seeded with `seedLegionFixture`, its daemon named at a `startFakeLegionDaemon`, with
// `seedLegionMetricsCorpus`, and with `pushLegionObservations` and `pushLegionObservation`,
// PLAIN is a project with no Legion setting, and the priority pages read a priority written
// through the API.
// Every expected row, column and count is computed in the row from the seed through the API
// (`GET /api/v1/issues` and the board route), never with the web code's filters. A row that
// measures a position on ACME seeds a failed read first and holds every board frame after the
// first (`seedMeasuredAcme`).
import type { IssueSummary } from "../web/src/api/types";
import { CONTROL } from "../web/src/components/control-names";
import { issueStatusScale } from "../web/src/theme/classes";
import {
  createIssue,
  createPriority,
  createProject,
  getIssue,
  getLegionBoard,
  listIssuesWhere,
  patchIssue,
} from "./api";
import { control, controlRow } from "./filters";
import {
  ACME,
  boardAtRest,
  cardsOf,
  drawnColumns,
  issuesTable,
  mouseDrag,
  queueSorted,
  rowKeys,
  seedAcme,
  seedMeasuredAcme,
} from "./issues-views-shared";
import { pushLegionObservation, pushLegionObservations, seedFailedLegionRead } from "./legion-seed";
import { expandedState } from "./settled";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

const PLAIN = "PLAIN";

/** The statuses in `main`'s order, with their words. */
const STATUS_WORDS: readonly (readonly [string, string])[] = [
  ["triage", "Triage"],
  ["icebox", "Icebox"],
  ["backlog", "Backlog"],
  ["todo", "Todo"],
  ["in_progress", "In progress"],
  ["testing", "Testing"],
  ["needs_review", "Needs review"],
  ["retro", "Retro"],
  ["done", "Done"],
];
const STATUS_ORDER = STATUS_WORDS.map(([status]) => status);
const statusWord = (status: string) => STATUS_WORDS.find(([id]) => id === status)?.[1] ?? status;

/** The board's twelve columns, in the order the route sends them, with their labels. */
const LEGION_COLUMNS = [
  "Queue",
  "Design",
  "Planning",
  "Implementing",
  "Testing",
  "Reviewing",
  "Accepting",
  "Retro",
  "Merging",
  "Waiting on children",
  "Waiting on a person",
  "Held",
];

/** How many of `issues` each status holds. */
function byStatus(issues: readonly IssueSummary[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const issue of issues) counts.set(issue.status, (counts.get(issue.status) ?? 0) + 1);
  return counts;
}

/** One segment of the bar, or one entry of its legend, as the page draws it. */
interface BarPart {
  readonly status: string;
  readonly left: number;
  readonly right: number;
  readonly top: number;
  readonly bottom: number;
  readonly width: number;
  readonly text: string;
  readonly fill: string;
  /** The fill `issueStatusScale` gives the part's status, as this page draws that class. */
  readonly scale: string;
  readonly pressed: string | null;
}

/** The bar under the tabs: its box, its segments and its legend's entries, with their fills. */
interface BarRead {
  readonly bar: { readonly left: number; readonly right: number; readonly width: number };
  readonly segments: readonly BarPart[];
  readonly legend: readonly BarPart[];
  readonly box: {
    readonly left: number;
    readonly right: number;
    readonly top: number;
    readonly bottom: number;
    readonly scrollWidth: number;
    readonly clientWidth: number;
    readonly scrollHeight: number;
    readonly clientHeight: number;
    readonly overflowX: string;
    readonly overflowY: string;
  };
  readonly pageWidth: number;
}

/** Reads the bar of the page, or undefined where it draws none. Each part's fill is set beside
 *  the fill a span of its status's `issueStatusScale` class draws in the same page. */
function readBar(page: Page): Promise<BarRead | undefined> {
  return page.evaluate((scale: Record<string, string>) => {
    const section = document.querySelector('section[aria-label="Issues by status"]');
    if (section === null) return undefined;
    const fillOf = (status: string) => {
      const probe = document.createElement("span");
      probe.className = scale[status] ?? "";
      section.appendChild(probe);
      const fill = getComputedStyle(probe).backgroundColor;
      probe.remove();
      return fill;
    };
    const part = (node: Element, status: string, swatch: Element | null) => {
      const rect = node.getBoundingClientRect();
      return {
        status,
        left: rect.left,
        right: rect.right,
        top: rect.top,
        bottom: rect.bottom,
        width: rect.width,
        text: (node.textContent ?? "").replace(/\s+/g, " ").trim(),
        fill: getComputedStyle(swatch ?? node).backgroundColor,
        scale: fillOf(status),
        pressed: node.getAttribute("aria-pressed"),
      };
    };
    const segments = [...section.querySelectorAll("[data-bar-segment]")];
    const bar = segments[0]?.parentElement?.getBoundingClientRect();
    const legend = section.querySelector("[data-bar-legend]")?.parentElement;
    const legendRect = legend?.getBoundingClientRect();
    const legendStyle = legend === null || legend === undefined ? null : getComputedStyle(legend);
    return {
      bar: { left: bar?.left ?? 0, right: bar?.right ?? 0, width: bar?.width ?? 0 },
      segments: segments.map((node) =>
        part(node, node.getAttribute("data-bar-segment") ?? "", null)
      ),
      legend: [...section.querySelectorAll("[data-bar-legend]")].map((node) =>
        part(node, node.getAttribute("data-bar-legend") ?? "", node.querySelector("span"))
      ),
      box: {
        left: legendRect?.left ?? 0,
        right: legendRect?.right ?? 0,
        top: legendRect?.top ?? 0,
        bottom: legendRect?.bottom ?? 0,
        scrollWidth: legend?.scrollWidth ?? 0,
        clientWidth: legend?.clientWidth ?? 0,
        scrollHeight: legend?.scrollHeight ?? 0,
        clientHeight: legend?.clientHeight ?? 0,
        overflowX: legendStyle?.overflowX ?? "",
        overflowY: legendStyle?.overflowY ?? "",
      },
      pageWidth: document.documentElement.clientWidth,
    };
  }, issueStatusScale);
}

/** The count a legend entry writes after its status's words. */
const entryCount = (entry: BarPart) => Number(entry.text.match(/(\d+)$/)?.[1] ?? Number.NaN);

/** Where the bar `read` draws differs from `issues`, the rows the table lists: its segments in
 *  `main`'s status order with no empty status, each as wide as its count's share and each from
 *  the end of the one before it at the bar's left edge, each in its status's scale fill, and one
 *  legend entry for each segment with its words and its count. */
function barDiffers(read: BarRead | undefined, issues: readonly IssueSummary[]): string[] {
  if (read === undefined) return ["no bar is drawn"];
  const counts = byStatus(issues);
  const statuses = STATUS_ORDER.filter((status) => counts.has(status));
  const wrong: string[] = [];
  const drawn = read.segments.map((segment) => segment.status);
  if (JSON.stringify(drawn) !== JSON.stringify(statuses))
    wrong.push(`the segments are ${drawn.join(", ")}, the rows' statuses ${statuses.join(", ")}`);
  const legendStatuses = read.legend.map((entry) => entry.status);
  if (JSON.stringify(legendStatuses) !== JSON.stringify(statuses))
    wrong.push(
      `the legend is ${legendStatuses.join(", ")}, the rows' statuses ${statuses.join(", ")}`
    );
  const sum = read.legend.reduce((total, entry) => total + entryCount(entry), 0);
  if (sum !== issues.length)
    wrong.push(`the legend counts ${sum} rows, the table lists ${issues.length}`);
  let left = read.bar.left;
  for (const segment of read.segments) {
    const count = counts.get(segment.status) ?? 0;
    const share = (count / issues.length) * read.bar.width;
    if (Math.abs(segment.width - share) > 1)
      wrong.push(`${segment.status} is ${segment.width} px wide, its share ${share} px`);
    if (Math.abs(segment.left - left) > 1)
      wrong.push(`${segment.status} starts at ${segment.left}, the segment before ends at ${left}`);
    left = segment.right;
    if (segment.fill !== segment.scale)
      wrong.push(`${segment.status} is filled ${segment.fill}, its scale fill ${segment.scale}`);
  }
  for (const entry of read.legend) {
    const wanted = `${statusWord(entry.status)} ${counts.get(entry.status) ?? 0}`;
    if (entry.text !== wanted)
      wrong.push(`a legend entry reads ${entry.text}, the rows give ${wanted}`);
    if (entry.fill !== entry.scale)
      wrong.push(`${entry.status}'s swatch is ${entry.fill}, its scale fill ${entry.scale}`);
  }
  return wrong;
}

/** One issue in each open status ACME's seed leaves empty, and a second In progress issue that
 *  has no card: the seed's ACME-1, In progress, has its card in Implementing. */
async function seedEveryOpenStatus(): Promise<string> {
  const statuses: readonly (readonly [string, string])[] = [
    ["A question to sort", "triage"],
    ["Kept for later", "icebox"],
    ["Next after this", "backlog"],
    ["Under test", "testing"],
    ["Waiting for a review", "needs_review"],
    ["Looking back at it", "retro"],
    ["Worked with no card", "in_progress"],
  ];
  let bare = "";
  for (const [title, status] of statuses) {
    const issue = await createIssue({ project: ACME, title });
    if (status !== "triage") await patchIssue(issue.key, { status });
    if (status === "in_progress") bare = issue.key;
  }
  return bare;
}

// SLOW(>=2s): Open read with and without a status filter, its legend pressed twice, grouped, at
// four widths, then Done and three Boards.
test("Open's bar counts the rows the table lists by status from its left edge in issueStatusScale's colours, its legend narrows and restores the view and never scrolls, and Done and every Board draw no bar", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    const bare = await seedEveryOpenStatus();
    const board = await getLegionBoard(ACME);
    expect(cardsOf(board).get("ACME-1")?.column, "ACME-1's card").toBe("implementing");
    expect(cardsOf(board).has(bare), "the second In progress issue has a card").toBe(false);
    await page.setViewportSize({ height: 900, width: 1440 });
    const table = issuesTable(page);
    const keysOf = (issues: readonly IssueSummary[]) => issues.map((issue) => issue.key).sort();

    // With a status filter on, the bar counts the rows the table lists: In progress and Todo.
    const narrowed = await listIssuesWhere({ project: ACME, status: ["in_progress", "todo"] });
    await page.goto(`/projects/${ACME}/issues?status=in_progress&status=todo`);
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(keysOf(narrowed));
    await expect
      .poll(async () => barDiffers(await readBar(page), narrowed), {
        message: "the bar with status=in_progress and status=todo",
      })
      .toEqual([]);
    const inProgress = narrowed.filter((issue) => issue.status === "in_progress");
    expect(inProgress.map((issue) => issue.key)).toEqual(expect.arrayContaining(["ACME-1", bare]));
    // The barDiffers poll above settled the legend on these rows, so one read gives its words.
    const entry = (await readBar(page))?.legend.find((one) => one.status === "in_progress");
    expect(entry?.text).toBe(`In progress ${inProgress.length}`);

    // Open: every open status that lists a row, Triage first.
    const open = await listIssuesWhere({ open: true, project: ACME });
    expect(new Set(open.map((issue) => issue.status)).size, "the seed's open statuses").toBe(8);
    await page.goto(`/projects/${ACME}/issues?view=open`);
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(keysOf(open));
    await expect
      .poll(async () => barDiffers(await readBar(page), open), { message: "Open's bar" })
      .toEqual([]);
    expect((await readBar(page))?.segments[0]?.status).toBe("triage");

    // A press on a legend entry narrows the rows to its status, and a second press gives the
    // view back.
    const todo = open.filter((issue) => issue.status === "todo");
    await page.locator('[data-bar-legend="todo"]').click();
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(keysOf(todo));
    await expect(page.locator('[data-bar-legend="todo"]')).toHaveAttribute("aria-pressed", "true");
    await page.locator('[data-bar-legend="todo"]').click();
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(keysOf(open));
    await expect.poll(async () => barDiffers(await readBar(page), open)).toEqual([]);

    // Grouped by Assignee, a heading's bar counts its group's rows by status.
    const alice = open.filter((issue) => issue.assignee === "alice");
    expect(alice.length, "no open issue is alice's").toBeGreaterThan(0);
    await page.goto(`/projects/${ACME}/issues?group=assignee`);
    const heading = page.getByRole("button", { name: /^Collapse alice, \d+ issues?$/ });
    await expect(heading).toBeVisible();
    // The heading wait above drew the grouped table, which draws each heading and its bar from
    // all of its rows at once.
    const grouped = await heading.evaluate((button, scale: Record<string, string>) => {
      const slot = button.nextElementSibling;
      const bar = slot?.querySelector("span[aria-hidden]");
      const parts = [...(bar?.children ?? [])].map((span) => {
        const probe = document.createElement("span");
        const status =
          Object.entries(scale).find(([, classes]) => span.className === classes)?.[0] ?? "";
        probe.className = scale[status] ?? "";
        document.body.appendChild(probe);
        const fill = getComputedStyle(probe).backgroundColor;
        probe.remove();
        return {
          status,
          grow: Number((span as HTMLElement).style.flexGrow),
          fill: getComputedStyle(span).backgroundColor,
          scale: fill,
        };
      });
      return {
        label: button.getAttribute("aria-label") ?? "",
        parts,
        text: (slot?.lastElementChild?.textContent ?? "").trim(),
      };
    }, issueStatusScale);
    const aliceCounts = byStatus(alice);
    const aliceStatuses = STATUS_ORDER.filter((status) => aliceCounts.has(status));
    expect(grouped.label).toBe(
      `Collapse alice, ${alice.length} ${alice.length === 1 ? "issue" : "issues"}`
    );
    expect(grouped.parts.map((part) => [part.status, part.grow])).toEqual(
      aliceStatuses.map((status) => [status, aliceCounts.get(status)])
    );
    for (const part of grouped.parts) expect(part.fill, `${part.status}'s fill`).toBe(part.scale);
    expect(grouped.text).toBe(
      aliceStatuses
        .map((status) => `${aliceCounts.get(status)} ${statusWord(status).toLowerCase()}`)
        .join(" · ")
    );

    // The legend at four widths: it never scrolls, every entry lies whole inside it and the
    // page, and it takes more lines where its entries do not fit on one.
    for (const width of [1024, 1280, 1440, 390]) {
      await page.setViewportSize({ height: 900, width });
      await page.goto(`/projects/${ACME}/issues`);
      await expect.poll(async () => (await rowKeys(table)).length).toBe(open.length);
      // The row poll above drew every row, and the bar draws from those rows in the same render.
      const read = await readBar(page);
      const where = `at ${width} px`;
      if (read === undefined) throw new Error(`${where}: no bar is drawn`);
      expect(read.box.scrollWidth, `${where}: the legend scrolls sideways`).toBe(
        read.box.clientWidth
      );
      expect(read.box.scrollHeight, `${where}: the legend scrolls down`).toBe(
        read.box.clientHeight
      );
      expect([read.box.overflowX, read.box.overflowY], `${where}: the legend's overflow`).toEqual([
        "visible",
        "visible",
      ]);
      for (const entry of read.legend) {
        expect(
          entry.left,
          `${where}: ${entry.status} starts left of the legend`
        ).toBeGreaterThanOrEqual(read.box.left - 0.5);
        expect(
          entry.right,
          `${where}: ${entry.status} ends right of the legend`
        ).toBeLessThanOrEqual(read.box.right + 0.5);
        expect(entry.top).toBeGreaterThanOrEqual(read.box.top - 0.5);
        expect(entry.bottom).toBeLessThanOrEqual(read.box.bottom + 0.5);
        expect(entry.left, `${where}: ${entry.status} starts off the page`).toBeGreaterThanOrEqual(
          0
        );
        expect(entry.right, `${where}: ${entry.status} ends off the page`).toBeLessThanOrEqual(
          read.pageWidth
        );
      }
      const lines = new Set(read.legend.map((entry) => Math.round(entry.top))).size;
      const needed = read.legend.reduce((sum, entry) => sum + entry.width, 0);
      if (needed > read.box.clientWidth)
        expect(lines, `${where}: the entries do not fit on one line`).toBeGreaterThan(1);
      else expect(lines, `${where}: the entries fit on one line`).toBe(1);
    }

    // Done draws no bar and no legend, and neither does any Board, in either grouping: each
    // Board column counts its cards instead.
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/projects/${ACME}/issues?view=done`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await expect(page.locator("[data-bar-segment], [data-bar-legend]")).toHaveCount(0);
    await expect(page.locator('section[aria-label="Issues by status"]')).toHaveCount(0);
    for (const address of [
      "layout=board",
      "layout=board&group=legion_phase",
      "view=done&layout=board",
    ]) {
      await page.goto(`/projects/${ACME}/issues?${address}`);
      await expect(page.locator("[data-board-card]").first()).toBeVisible();
      await expect(page.locator("[data-bar-segment], [data-bar-legend]")).toHaveCount(0);
      await expect(page.locator('section[aria-label="Issues by status"]')).toHaveCount(0);
      // The card wait above drew the whole Board, which draws every column at once. Each column
      // draws its count from the cards it draws.
      for (const column of await drawnColumns(page))
        if (!column.rail)
          expect(column.count, `${address}: ${column.title}'s count`).toBe(column.cards.length);
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The display menu's trigger words and the choices it offers, each with whether it is taken. */
async function readDisplayMenu(
  page: Page,
  scope: Locator | Page = page
): Promise<{ trigger: string; choices: string[] }> {
  const trigger = scope.locator('button[data-control="display-menu"]').first();
  const words = ((await trigger.textContent()) ?? "").trim();
  await trigger.click();
  const menu = page.getByRole("dialog", { name: words });
  await expect(menu).toBeVisible();
  const choices = await menu
    .locator("label")
    .evaluateAll((labels) =>
      labels.map(
        (label) =>
          `${(label.textContent ?? "").trim()}${label.querySelector("input")?.checked ? " (taken)" : ""}`
      )
    );
  await page.keyboard.press("Escape");
  await expect(menu).toHaveCount(0);
  return { trigger: words, choices };
}

/** k14's seed on ACME: six P0 Todo issues with no card, the first labelled `widgets`; two P0
 *  Queue cards in the daemon's waiting line after ACME-4 and ACME-3; ACME-1 and one more P0 In
 *  progress issue with their cards in Implementing; one P0 Testing issue with no card; and one
 *  Triage issue with no priority. */
interface K14Seed {
  readonly todo: readonly string[];
  readonly queue: readonly string[];
  readonly implementing: readonly string[];
  readonly testing: string;
  readonly unset: string;
}

const LABEL = "widgets";

/** The admission block of `seedLegionFixture` with `waiting` after the seed's own line. */
const admission = (waiting: readonly string[]) =>
  JSON.stringify({
    cap: 4,
    active: ["ACME-1", "ACME-2"],
    waiting: ["ACME-4", "ACME-3", ...waiting],
    trees: ["ACME-1", "ACME-2"],
    blocked: [{ issue: "ACME-5", blockedBy: ["ACME-1"] }],
  });

async function seedK14(): Promise<K14Seed> {
  const made = async (title: string, status: string, priority: 0 | null, labels: string[] = []) => {
    const issue = await createIssue({ project: ACME, title });
    await patchIssue(issue.key, {
      ...(priority === null ? {} : { priority }),
      ...(labels.length === 0 ? {} : { labels }),
      ...(status === "triage" ? {} : { status }),
    });
    return issue.key;
  };
  const todo: string[] = [];
  for (let n = 1; n <= 6; n++)
    todo.push(await made(`Todo work ${n}`, "todo", 0, n === 1 ? [LABEL] : []));
  const queue = [await made("Queued first", "todo", 0), await made("Queued second", "todo", 0)];
  const second = await made("Implemented beside the widgets", "in_progress", 0);
  const testing = await made("Tested with no card", "testing", 0);
  const unset = await made("Sorted with no priority", "triage", null);
  await pushLegionObservations([
    ...queue.map((issue) => ({ issue, field: "phase", value: '"admitted"', ago: "0 seconds" })),
    { issue: second, field: "phase", value: '"implementing"', ago: "0 seconds" },
    { issue: "", field: "admission", value: admission(queue), ago: "0 seconds" },
  ]);
  const board = await getLegionBoard(ACME);
  const cards = cardsOf(board);
  expect(
    queue.map((key) => [cards.get(key)?.column, cards.get(key)?.card.waiting_place]),
    "the two P0 Queue cards"
  ).toEqual([
    ["queue", 3],
    ["queue", 4],
  ]);
  expect(cards.get(second)?.column).toBe("implementing");
  return { todo, queue, implementing: ["ACME-1", second], testing, unset };
}

// SLOW(>=2s): seven pages read for their display menus, on two projects and the priority pages.
test("the display menu reads Group by on the Issues tab in both layouts and on both priority pages, with the choices each offers, and the Board of a project with no daemon draws none", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await createProject({ key: PLAIN, name: "Plain" });
    const plain = await createIssue({ project: PLAIN, title: "Plain work" });
    const priority = await createPriority({ owner: "alice", title: "Ship the widgets first" });
    await patchIssue("ACME-1", { advances: [priority.id] });
    await patchIssue(plain.key, { status: "todo" });
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const table = issuesTable(page);
    const tableChoices = [
      "Nothing (taken)",
      "Priority",
      "Status",
      "Legion phase",
      "Assignee",
      "Created by",
      "Claimed by",
      "Created",
      "Updated",
    ];

    // ACME's table, then its Board.
    await page.goto(`/projects/${ACME}/issues`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    expect(await readDisplayMenu(page)).toEqual({ trigger: "Group by", choices: tableChoices });
    await page.goto(`/projects/${ACME}/issues?layout=board`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    expect(await readDisplayMenu(page)).toEqual({
      trigger: "Group by",
      choices: ["Status (taken)", "Legion phase"],
    });

    // The project with no Legion setting: the table's choices less Legion phase, and no menu on
    // its Board.
    await page.goto(`/projects/${PLAIN}/issues`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    expect(await readDisplayMenu(page)).toEqual({
      trigger: "Group by",
      choices: tableChoices.filter((choice) => choice !== "Legion phase"),
    });
    await page.goto(`/projects/${PLAIN}/issues?layout=board`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    await expect(page.locator('button[data-control="display-menu"]')).toHaveCount(0);

    // The priority pages: every display menu, and the priorities table's grouping control, read
    // Group by.
    for (const path of ["/priorities", `/priorities/${priority.id}`]) {
      await page.goto(path);
      await expect(page.locator("tr[data-issue-row]").first()).toBeVisible();
      await expect(page.getByRole("status", { name: "Loading issues" })).toHaveCount(0);
      const triggers = page.locator('button[data-control="display-menu"]');
      expect(await triggers.count(), `${path}: its display menus`).toBeGreaterThan(0);
      // The waits above drew the page and ended the loading of each issue table, so every
      // display menu of the page is drawn.
      for (const words of await triggers.allTextContents())
        expect(words.trim(), `${path}: a display menu`).toBe("Group by");
    }
    await page.goto("/priorities");
    const priorities = page.locator('section[aria-label="Priorities"]');
    await expect(priorities.locator("tr[data-priority-row]").first()).toBeVisible();
    expect((await readDisplayMenu(page, priorities)).trigger).toBe("Group by");
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): the seed's later reads, then seven Boards read column by column.
test("the Board of a view draws the table's rows in the view's sort, its status columns and rails, and by Legion phase the board's twelve columns and the None rail", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    const seed = await seedK14();
    await createProject({ key: PLAIN, name: "Plain" });
    for (const [title, status] of [
      ["Plain question", "triage"],
      ["Plain work", "todo"],
    ] as const) {
      const issue = await createIssue({ project: PLAIN, title });
      if (status !== "triage") await patchIssue(issue.key, { status });
    }
    await page.setViewportSize({ height: 1200, width: 1920 });
    const board = await getLegionBoard(ACME);
    const open = await listIssuesWhere({ open: true, project: ACME });

    // f.priority=0: the Board's cards are the table's rows of the same address, column by
    // column in the table's order, and the control row names the narrowing.
    const p0 = queueSorted(
      open.filter((issue) => issue.priority === 0),
      board
    );
    await page.goto(`/projects/${ACME}/issues?f.priority=0`);
    const table = issuesTable(page);
    await expect.poll(() => rowKeys(table)).toEqual(p0.map((issue) => issue.key));
    const rows = await rowKeys(table);
    await page.goto(`/projects/${ACME}/issues?f.priority=0&layout=board`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    // Every status Open picks is a column, an empty one too, and Icebox a rail.
    await expect
      .poll(async () =>
        (await drawnColumns(page))
          .filter((column) => !column.rail)
          .map((column) => [column.id, column.cards])
      )
      .toEqual(
        STATUS_ORDER.filter((status) => status !== "done" && status !== "icebox").map((status) => [
          status,
          rows.filter((key) => p0.find((issue) => issue.key === key)?.status === status),
        ])
      );
    await expect(
      control(controlRow(page), CONTROL.chip).filter({ hasText: /Priority/ })
    ).toHaveCount(1);

    // view=open by Status: the open statuses in main's order, Icebox a rail after Triage with a
    // count and no column title, and the Done rail with no count.
    const openStatuses = STATUS_ORDER.filter((status) => status !== "done");
    await page.goto(`/projects/${ACME}/issues?layout=board`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    // The card wait above drew the whole Board, which draws every column and rail at once.
    const statusBoard = await drawnColumns(page);
    expect(statusBoard.map((column) => [column.id, column.rail])).toEqual([
      ...openStatuses.map((status) => [status, status === "icebox"]),
      ["done", true],
    ]);
    const icebox = statusBoard.find((column) => column.id === "icebox");
    expect(icebox?.count, "the Icebox rail's count").toBe(
      open.filter((issue) => issue.status === "icebox").length
    );
    expect(statusBoard.find((column) => column.id === "done")?.count).toBeNaN();
    for (const column of statusBoard.filter((one) => !one.rail))
      expect(column.title).toBe(statusWord(column.id));
    // Every card with no priority shows the Priority tag.
    await expect(
      page.locator(
        `[data-board-card="${seed.unset}"] select[aria-label="Priority of ${seed.unset}"]`
      )
    ).toHaveValue("");
    await expect(page.locator(`[data-board-card="${seed.unset}"]`)).toContainText("Priority");
    for (const issue of open.filter((one) => one.priority === null))
      await expect(page.locator(`[data-board-card="${issue.key}"]`)).toContainText("Priority");

    // status=triage: the one column is Triage.
    await page.goto(`/projects/${ACME}/issues?layout=board&status=triage`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    expect(
      (await drawnColumns(page)).filter((column) => !column.rail).map((column) => column.id)
    ).toEqual(["triage"]);

    // nofilter=1: only the statuses its rows are in, computed from the seed, with Done.
    const done = await listIssuesWhere({ project: ACME, status: ["done"] });
    const listed = new Set([...open, ...done].map((issue) => issue.status));
    await page.goto(`/projects/${ACME}/issues?layout=board&nofilter=1`);
    await expect(
      page.locator('section[data-board-column="done"] [data-board-card]').first()
    ).toBeVisible();
    expect((await drawnColumns(page)).map((column) => [column.id, column.rail])).toEqual(
      STATUS_ORDER.filter((status) => listed.has(status)).map((status) => [
        status,
        status === "icebox",
      ])
    );

    // By Legion phase: the twelve Legion phases, empty ones too, and the None rail with the
    // count of Open's rows with no card; no Show Icebox and no Done rail.
    const cards = cardsOf(board);
    await page.goto(`/projects/${ACME}/issues?layout=board&group=legion_phase`);
    await expect(page.locator('section[data-board-column="queue"]')).toBeVisible();
    const phases = await drawnColumns(page);
    expect(phases.map((column) => column.title)).toEqual([...LEGION_COLUMNS, "None"]);
    expect(phases.at(-1)?.rail).toBe(true);
    expect(phases.at(-1)?.count, "the None rail's count").toBe(
      open.filter((issue) => !cards.has(issue.key)).length
    );
    await expect(page.getByRole("button", { exact: true, name: "Show Icebox" })).toHaveCount(0);
    expect(phases.some((column) => column.id === "done")).toBe(false);

    // f.legion_phase=implementing: Implementing alone.
    await page.goto(
      `/projects/${ACME}/issues?layout=board&group=legion_phase&f.legion_phase=implementing`
    );
    await expect(page.locator('section[data-board-column="implementing"]')).toBeVisible();
    expect((await drawnColumns(page)).map((column) => column.title)).toEqual(["Implementing"]);

    // The project with no Legion setting: status columns only.
    await page.goto(`/projects/${PLAIN}/issues?layout=board`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    // The card wait above drew the whole Board, which draws every column of the project at once.
    for (const column of await drawnColumns(page))
      expect(STATUS_ORDER, `${PLAIN}: a column that is no status`).toContain(column.id);
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The issues of ACME by key, as `GET /api/v1/issues` answers them now. */
async function acmeNow(): Promise<Map<string, IssueSummary>> {
  return new Map(
    (await listIssuesWhere({ project: ACME })).map((issue) => [issue.key, issue] as const)
  );
}

/** The keys of one Board column's cards, in their order. */
const columnCards = async (page: Page, id: string) =>
  (await drawnColumns(page)).find((column) => column.id === id)?.cards ?? [];

/** The Board's live region. */
const liveRegion = (page: Page) => page.getByRole("status", { name: "Board announcements" });

// SLOW(>=2s): seven key moves and four drops, each waited on through the API.
test("by Status a key move or a drop writes the status or the rank the Board's rules give, and the live region reads each move", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    const seed = await seedK14();
    await page.setViewportSize({ height: 1200, width: 1920 });
    await page.goto(`/projects/${ACME}/issues?f.priority=0&layout=board`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    const card = (key: string) => page.locator(`[data-board-card="${key}"]`);
    const todo = await columnCards(page, "todo");
    expect(todo.length, "the Todo column's cards").toBe(8);
    const [first, second] = todo;
    if (first === undefined || second === undefined) throw new Error("no Todo cards");

    // Shift+K writes the rank within Todo: the second card goes before the first.
    await card(second).focus();
    await page.keyboard.press("Shift+K");
    await expect
      .poll(async () => {
        const now = await acmeNow();
        return (now.get(second)?.rank ?? "") < (now.get(first)?.rank ?? "");
      })
      .toBe(true);
    await expect(liveRegion(page)).toHaveText(new RegExp(`^${second} → Todo, position 1 of 8$`));

    // Shift+L moves it to the top of In progress and sets that status.
    await card(second).focus();
    await page.keyboard.press("Shift+L");
    await expect.poll(async () => (await getIssue(second)).status).toBe("in_progress");
    await expect.poll(async () => (await columnCards(page, "in_progress"))[0]).toBe(second);
    await expect(liveRegion(page)).toHaveText(
      new RegExp(`^${second} → In progress, position 1 of \\d+$`)
    );

    // Another Todo card, Shift+L until it reaches the Done rail, which closes it.
    const walker = (await columnCards(page, "todo")).at(-1) ?? "";
    for (const [status, title] of [
      ["in_progress", "In progress"],
      ["testing", "Testing"],
      ["needs_review", "Needs review"],
      ["retro", "Retro"],
    ]) {
      await card(walker).focus();
      await page.keyboard.press("Shift+L");
      await expect.poll(async () => (await getIssue(walker)).status).toBe(status);
      await expect(liveRegion(page)).toHaveText(
        new RegExp(`^${walker} → ${title}, position \\d+ of \\d+$`)
      );
    }
    await card(walker).focus();
    await page.keyboard.press("Shift+L");
    await expect.poll(async () => (await getIssue(walker)).status).toBe("done");
    expect((await getIssue(walker)).closed_at).not.toBeNull();
    await expect(liveRegion(page)).toHaveText(`${walker} → Done, closed`);

    // Show Icebox turns the rail into the Icebox column.
    await page.getByRole("button", { exact: true, name: "Show Icebox" }).click();
    await expect(page.getByRole("region", { exact: true, name: "Icebox" })).toBeVisible();

    // The drops: a Todo card onto Backlog, an In progress issue with a card in Implementing onto
    // Backlog, a Todo card onto Testing, and a card onto the Done rail.
    const column = (id: string) => page.locator(`section[data-board-column="${id}"]`);
    const todoNow = await columnCards(page, "todo");
    const [toBacklog, toTesting, toDone] = todoNow.slice(-3);
    if (toBacklog === undefined || toTesting === undefined || toDone === undefined)
      throw new Error("fewer than three Todo cards are left");
    await mouseDrag(page, card(toBacklog), column("backlog"));
    await expect.poll(async () => (await getIssue(toBacklog)).status).toBe("backlog");
    const [implemented] = seed.implementing;
    if (implemented === undefined) throw new Error("no In progress card");
    await mouseDrag(page, card(implemented), column("backlog"));
    await expect.poll(async () => (await getIssue(implemented)).status).toBe("backlog");
    await mouseDrag(page, card(toTesting), column("testing"));
    await expect.poll(async () => (await getIssue(toTesting)).status).toBe("testing");
    await mouseDrag(page, card(toDone), page.getByRole("region", { name: "Done (collapsed)" }));
    await expect.poll(async () => (await getIssue(toDone)).status).toBe("done");
    expect((await getIssue(toDone)).closed_at).not.toBeNull();
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): a drop within Queue and a later read the daemon follows, then four refused drops
// and two refused key moves, each read through the API.
test("by Legion phase only a drop within Queue writes, the column follows the daemon's next read, and every other drop and key move is refused with one short sentence and writes nothing", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const context = await asUser(browser, "alice");
  const daemon = await seedAcme();
  const page = await context.newPage();
  try {
    const seed = await seedK14();
    // The failed read is seeded after the seed's good read, so it is the daemon's newest read and
    // its alert is there from the start: no frame of the sampler's failed reads moves the Board.
    // The board's frames pass, since the admission block this row pushes reaches the page live.
    await seedFailedLegionRead(ACME, "the daemon did not answer its state route");
    const failedRead = page
      .getByRole("alert")
      .filter({ hasText: "The last read of the Legion daemon failed" });
    const [q1, q2] = seed.queue;
    const [i1, i2] = seed.implementing;
    if (q1 === undefined || q2 === undefined || i1 === undefined || i2 === undefined)
      throw new Error("the seed's cards are missing");
    await page.setViewportSize({ height: 1200, width: 1920 });
    // The Board by Legion phase of the P0 issues, from the Group by menu.
    await page.goto(`/projects/${ACME}/issues?f.priority=0&layout=board`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    await expect(failedRead).toBeVisible();
    await page.getByRole("button", { exact: true, name: "Group by" }).click();
    await page
      .getByRole("dialog", { name: "Group by" })
      .locator('input[type="radio"][value="legion_phase"]')
      .click();
    await page.keyboard.press("Escape");
    await expect.poll(() => new URL(page.url()).searchParams.get("group")).toBe("legion_phase");
    await expect(page.locator('section[data-board-column="queue"]')).toBeVisible();
    const card = (key: string) => page.locator(`[data-board-card="${key}"]`);
    const column = (id: string) => page.locator(`section[data-board-column="${id}"]`);
    expect(await columnCards(page, "queue")).toEqual([q1, q2]);

    // The drop within Queue writes the rank; the column follows once the daemon's next read
    // has the new order.
    await mouseDrag(page, card(q2), card(q1), 4);
    await expect
      .poll(async () => {
        const now = await acmeNow();
        return (now.get(q2)?.rank ?? "") < (now.get(q1)?.rank ?? "");
      })
      .toBe(true);
    await expect
      .poll(() => columnCards(page, "queue"), {
        message: "the column before the daemon's next read",
      })
      .toEqual([q1, q2]);
    await pushLegionObservation("", "admission", admission([q2, q1]));
    await expect.poll(() => columnCards(page, "queue")).toEqual([q2, q1]);
    // That push is a good read, which takes the alert away; a failed read seeded after it brings
    // the alert back before the refused moves, so the sampler's own failed read moves nothing
    // under them.
    await seedFailedLegionRead(ACME, "the daemon did not answer its state route");
    await expect(failedRead).toBeVisible();

    // Every other move is refused in one short sentence and writes nothing.
    const patches: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH") patches.push(request.url());
    });
    const before = await acmeNow();
    const refusal = page.getByTestId("board-refusal");
    const refused = async (what: string) => {
      await expect(refusal, what).toBeVisible();
      const text = ((await refusal.textContent()) ?? "").trim();
      expect(text.split(/(?<=\.)\s+/).length, `${what}: the refusal is one sentence`).toBe(1);
      expect(text.split(/\s+/).length, `${what}: the refusal is short`).toBeLessThanOrEqual(20);
      await expect(liveRegion(page), what).toHaveText(text);
      return text;
    };
    await mouseDrag(page, card(q1), column("design"));
    await refused("a Queue card dropped on Design");
    await mouseDrag(page, card(i1), column("queue"));
    await refused("an Implementing card dropped on Queue");
    await mouseDrag(page, card(i2), card(i1), 4);
    await refused("the second Implementing card dropped above the first");
    await mouseDrag(page, card(q1), page.getByRole("region", { name: "None (collapsed)" }));
    await refused("a card dropped on the None rail");
    const firstImplementing = (await columnCards(page, "implementing"))[0] ?? "";
    await card(firstImplementing).focus();
    await page.keyboard.press("Shift+J");
    await refused("Shift+J on the first Implementing card");
    await card(firstImplementing).focus();
    await page.keyboard.press("Shift+L");
    await refused("Shift+L on the first Implementing card");
    // No write may follow a refused move: the Board sends a move's PATCH from the handler that
    // reads the drop or the key, so one sent for any of these would be recorded within 300 ms.
    await page.waitForTimeout(300);
    expect(patches, "a refused move sent a write").toEqual([]);
    const after = await acmeNow();
    for (const key of [q1, q2, i1, i2])
      expect(
        { status: after.get(key)?.status, rank: after.get(key)?.rank },
        `${key} after the refused moves`
      ).toEqual({ status: before.get(key)?.status, rank: before.get(key)?.rank });
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): two drops under a sort by Created, a label picked, three reloads and two views'
// Boards.
test("under a sort by Created a drop within Todo is refused naming the sort and a drop on Backlog writes no rank, the Labels pick narrows the Board as the table, and the Filters panel opens after a reload as it was left", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const daemon = await seedMeasuredAcme(page);
  try {
    const seed = await seedK14();
    await page.setViewportSize({ height: 1200, width: 1920 });
    const base = `/projects/${ACME}/issues?f.priority=0&sort=-created&layout=board`;
    await page.goto(base);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    const card = (key: string) => page.locator(`[data-board-card="${key}"]`);
    const todo = await columnCards(page, "todo");
    const [top, next] = todo;
    if (top === undefined || next === undefined) throw new Error("no Todo cards");

    // A drop within Todo under the sort by Created is refused with a text that names the sort.
    const writes: { url: string; body: unknown }[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH")
        writes.push({ url: request.url(), body: request.postDataJSON() });
    });
    const ranks = await acmeNow();
    await mouseDrag(page, card(next), card(top), 4);
    const refusal = page.getByTestId("board-refusal");
    await expect(refusal).toContainText("Created");
    // No write may follow the refused drop: the Board sends a move's PATCH from the handler that
    // reads the drop, so one sent for this drop would be recorded within 300 ms of the refusal.
    await page.waitForTimeout(300);
    expect(writes).toEqual([]);
    expect((await acmeNow()).get(next)?.rank).toBe(ranks.get(next)?.rank);

    // A drop onto Backlog writes the status and no rank.
    await mouseDrag(page, card(next), page.locator('section[data-board-column="backlog"]'));
    await expect.poll(async () => (await getIssue(next)).status).toBe("backlog");
    expect(writes.map((write) => write.body)).toEqual([{ status: "backlog" }]);
    expect((await getIssue(next)).rank).toBe(ranks.get(next)?.rank);

    // The Filters panel's Labels pick writes label=, shows a chip, and the cards are the
    // table's rows of that address.
    const trigger = page.locator('[data-control="control-row"] [data-control="panel-trigger"]');
    if ((await expandedState("the Filters trigger", trigger)) === "shut") await trigger.click();
    const panel = page.getByRole("dialog", { name: "Filters" });
    await expect(panel).toBeVisible();
    await panel.getByRole("button", { name: /^Labels/ }).click();
    await page.getByRole("option", { name: new RegExp(`^${LABEL}`) }).click();
    await page.keyboard.press("Escape");
    await expect.poll(() => new URL(page.url()).searchParams.getAll("label")).toEqual([LABEL]);
    await expect(control(controlRow(page), CONTROL.chip).filter({ hasText: LABEL })).toHaveCount(1);
    const [labelled] = seed.todo;
    await expect
      .poll(async () => (await drawnColumns(page)).flatMap((one) => one.cards))
      .toEqual([labelled]);
    const address = new URL(page.url());
    address.searchParams.delete("layout");
    await page.goto(`${address.pathname}${address.search}`);
    await expect.poll(() => rowKeys(issuesTable(page))).toEqual([labelled]);

    // Needs you on, the panel closed: after a reload it is closed; opened, after a reload it is
    // open.
    await page.goto(`${base}&needs-you=1`);
    await expect(page.locator('section[data-board-column="todo"]')).toBeVisible();
    if ((await expandedState("the Filters trigger", trigger)) === "open") {
      await trigger.click();
    }
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    await page.reload();
    await expect(page.locator('section[data-board-column="todo"]')).toBeVisible();
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    await trigger.click();
    await expect(trigger).toHaveAttribute("aria-expanded", "true");
    await page.reload();
    await expect(page.locator('section[data-board-column="todo"]')).toBeVisible();
    await expect(trigger).toHaveAttribute("aria-expanded", "true");
    await page.keyboard.press("Escape");

    // Done's Board draws the Done column with its done cards; Open's the Done rail and no done
    // card.
    const done = await listIssuesWhere({ project: ACME, status: ["done"] });
    await page.goto(`/projects/${ACME}/issues?view=done&layout=board`);
    await expect(
      page.locator('section[data-board-column="done"] [data-board-card]').first()
    ).toBeVisible();
    expect([...(await columnCards(page, "done"))].sort()).toEqual(
      done.map((issue) => issue.key).sort()
    );
    await page.goto(`/projects/${ACME}/issues?view=open&layout=board`);
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    await expect(page.getByRole("region", { name: "Done (collapsed)" })).toBeVisible();
    for (const issue of done) await expect(card(issue.key)).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// The Design's moves by Status and by Legion phase, found by INFRA-3221's tester: the daemon
// gives Places in an order of its own, so two cards next to each other by Place need not be in
// rank order (ACME-4 has Place 1 and rank X, ACME-3 Place 2 and rank W). A move between them
// either writes a rank the server takes, or is refused with a text that names why; it never
// sends a rank the server refuses and reads that refusal as a board that changed.
// SLOW(>=2s): ACME seeded with three placed cards, and one card dropped between two others on
// the Board by Legion phase and by Status.
test("a drop between two placed cards whose ranks run against their Places sends no rank the server refuses", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await pushLegionObservation(
      "",
      "admission",
      JSON.stringify({
        cap: 4,
        active: ["ACME-1", "ACME-2"],
        waiting: ["ACME-4", "ACME-3", "ACME-5"],
        trees: ["ACME-1", "ACME-2"],
        blocked: [],
      })
    );
    const ranks = async () =>
      Promise.all(["ACME-3", "ACME-4"].map(async (key) => (await getIssue(key)).rank));
    const [rank3, rank4] = await ranks();
    expect((rank3 ?? "") < (rank4 ?? ""), "ACME-3 is ranked before ACME-4, against its Place").toBe(
      true
    );
    const page = await context.newPage();
    const refused: string[] = [];
    page.on("response", (response) => {
      if (response.request().method() === "PATCH" && response.status() >= 400)
        refused.push(`${response.status()} ${response.url()} ${response.request().postData()}`);
    });
    await page.setViewportSize({ height: 1200, width: 1920 });
    const card = (key: string) => page.locator(`[data-board-card="${key}"]`);
    const board = page.getByRole("region", { name: "Project board" });
    for (const address of ["group=legion_phase&layout=board", "layout=board"]) {
      await page.goto(`/projects/${ACME}/issues?${address}`);
      const queue = page.locator(
        address.includes("legion_phase")
          ? 'section[data-board-column="queue"]'
          : 'section[data-board-column="todo"]'
      );
      await expect
        .poll(() =>
          queue
            .locator("[data-board-card]")
            .evaluateAll((cards) => cards.map((one) => one.getAttribute("data-board-card")))
        )
        .toEqual(["ACME-4", "ACME-3", "ACME-5"]);
      // ACME-5 dropped above ACME-3: between Place 1 and Place 2. A move refused in the page
      // sends no write, so the wait for one ends at its timeout, which is no failure.
      const written = page
        .waitForResponse((response) => response.request().method() === "PATCH", { timeout: 10_000 })
        .catch(() => undefined);
      await mouseDrag(page, card("ACME-5"), card("ACME-3"), 4);
      const write = await written;
      expect(refused, address).toEqual([]);
      // A write the server took, or a refusal in the page that names why; never the Board's
      // alert, which is what a write the server refused draws. The Board draws that alert as it
      // lands the answer, so it is read once the cards are at rest.
      if (write === undefined) await expect(board.getByTestId("board-refusal")).toBeVisible();
      await boardAtRest(page);
      await expect(board.getByRole("alert"), address).toHaveCount(0);
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});
