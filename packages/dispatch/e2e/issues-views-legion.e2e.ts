/** The Legion columns, cards, filters and alerts are read against the board route and the items
 *  route as the sampler, the facts consumer and the eval refresher leave them, over the live
 *  socket; Start next's writes are read back through the API. */
// The Issues tab against the Legion board route (dispatch://INFRA-3221, Verification k2, k5 and
// k19): Open's Queue filter and its Board by Legion phase against the board's columns, each card's
// Legion fields, the (i) hint and the alerts of the daemon's reads; every Legion cell against its
// route, the board's `issues` map or the items route of `legion-work-time`; and every column's
// filter, its picks and what each pick writes to the address.
//
// ACME is seeded with `seedLegionFixture`, its daemon named at a `startFakeLegionDaemon`, with
// `seedLegionMetricsCorpus` and with `pushLegionObservation`, and WIDGETS with
// `seedLegionFactsFixture`. Each expected value is computed in the row from the seed through the
// API (`GET /api/v1/issues`, the board route, the items route), never with the web code's
// filters. A row that reads an issue's Legion work writes a role event on it and waits for the
// facts consumer's row (`waitForIssueFacts`, up to 75 s), each such wait a row of its own.
import type {
  LegionBoardResponse as Board,
  LegionBoardCard as Card,
  LegionBoardColumn as Column,
  IssueSummary,
} from "../web/src/api/types";
import { setLiveSessions } from "./agents";
import {
  claimIssue,
  createAsk,
  createIssue,
  createProject,
  getIssue,
  getLegionBoard,
  getLegionWorkTimeItems,
  legionWorkTimeEval,
  listIssuesWhere,
  mintAgentToken,
  patchIssue,
  waitForLegionWorkTimeEval,
} from "./api";
import {
  ACME,
  boardFacts,
  cardsOf,
  columnCount,
  drawnColumns,
  EVERY_COL,
  type Facts,
  type FilterRead,
  hoursWords,
  issuesTable,
  itemFacts,
  measuredWork,
  queueSorted,
  readFilter,
  routeLive,
  rowKeys,
  seedAcme,
  spanWords,
  tabNames,
  tooltipLines,
  workWords,
} from "./issues-views-shared";
import {
  LEGION_FACTS_PROJECT,
  roleEventStatement,
  seedLegionFactsFixture,
  waitForIssueFacts,
  writeRoleEvent,
} from "./legion-facts-seed";
import {
  type Observation,
  pushLegionObservation,
  pushLegionObservations,
  seedFailedLegionRead,
  startFakeLegionDaemon,
  writeEventsForFacts,
} from "./legion-seed";
import { sql } from "./psql";
import { expandedState } from "./settled";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/** The board's twelve columns, in the order the route sends them, with their labels. */
const BOARD_COLUMNS: readonly (readonly [string, string])[] = [
  ["queue", "Queue"],
  ["design", "Design"],
  ["planning", "Planning"],
  ["implementing", "Implementing"],
  ["testing", "Testing"],
  ["reviewing", "Reviewing"],
  ["accepting", "Accepting"],
  ["retro", "Retro"],
  ["merging", "Merging"],
  ["awaiting_children", "Waiting on children"],
  ["awaiting_person", "Waiting on a person"],
  ["held", "Held"],
];

/** The headers of the columns the board route feeds, which a project with no daemon never draws. */
const BOARD_FED_HEADERS = [
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

/** `cols=` with Status and every column id of the Design's columns table (k5). */
const LEGION_COLS =
  "status,closed,legion_phase,queue,asks,roles,activity,waited,waiting_reason,time_in_phase,work_hours,review_rounds,legion,phase_hours";

/** The statuses in `main`'s order, with the words the Status cell draws for each. */
const STATUS_WORDS: Readonly<Record<string, string>> = {
  triage: "Triage",
  icebox: "Icebox",
  backlog: "Backlog",
  todo: "Todo",
  in_progress: "In progress",
  testing: "Testing",
  needs_review: "Needs review",
  retro: "Retro",
  done: "Done",
};

/** The Legion roles in the order the Workers column names them (`LEGION_ROLES`). */
const ROLE_ORDER = ["architect", "planner", "implementer", "tester", "reviewer", "merger"];

/** The words of each `slot_use`, in Worker state's order. */
const WORKER_STATE_WORDS: readonly (readonly [string, string])[] = [
  ["working", "Working"],
  ["between_turns", "Between turns"],
  ["held_children", "Waiting on child issues"],
  ["held_person", "Waiting on a person"],
  ["held_idle", "Idle"],
];

/** The words of each `waiting.reason`, in Why it waits' order. */
const WAITING_WORDS: readonly (readonly [string, string])[] = [
  ["ready", "Ready to start"],
  ["slot", "Capacity to go on"],
  ["blocked", "A blocker"],
  ["person", "A person"],
];

/** The phases Time per phase draws, in the board's order and Other last. */
const WORK_PHASES = [
  "design",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
  "merging",
  "awaiting_person",
  "held",
  "other",
];

/** The duration picks, their address values and their words, each length with its lower bound
 *  and without its upper. */
const DURATIONS: readonly (readonly [string, string, number])[] = [
  ["under-1h", "Under 1 hour", 3_600_000],
  ["1h-1d", "1 hour to 1 day", 86_400_000],
  ["1d-7d", "1 to 7 days", 7 * 86_400_000],
  ["over-7d", "Over 7 days", Number.POSITIVE_INFINITY],
];

const wordOf = (pairs: readonly (readonly [string, string])[], value: string): string =>
  pairs.find(([id]) => id === value)?.[1] ?? value;

/** The two readings a time since `since` may be drawn with: at `nowMs`, and a minute before it,
 *  since the page draws its own clock a moment before the row reads its. */
function sinceWords(since: string, nowMs: number): string[] {
  return [nowMs, nowMs - 60_000].map((at) =>
    spanWords(Math.floor(Math.max(0, at - Date.parse(since)) / 60_000))
  );
}

/** The duration pick of a length of `ms`, None for no length. */
function durationPick(ms: number | undefined): string {
  if (ms === undefined) return "none";
  return DURATIONS.find(([, , below]) => ms < below)?.[0] ?? "over-7d";
}

/** The phases of `facts` with hours, in Time per phase's order, a key no phase names as Other. */
function phasesWithHours(facts: Facts | undefined): string[] {
  const hours = new Map<string, number>();
  for (const [phase, value] of Object.entries(facts?.hours_by_phase ?? {})) {
    if (!(value > 0)) continue;
    const drawn = WORK_PHASES.includes(phase) ? phase : "other";
    hours.set(drawn, (hours.get(drawn) ?? 0) + value);
  }
  return WORK_PHASES.filter((phase) => hours.has(phase));
}

const phaseTotal = (facts: Facts | undefined): number =>
  Object.values(facts?.hours_by_phase ?? {}).reduce(
    (sum, value) => (value > 0 ? sum + value : sum),
    0
  );

/** The keys of one column's cards, in the board's order. */
function columnKeys(board: Board, id: string): string[] {
  return (board.columns?.find((column) => column.id === id)?.cards ?? []).map((card) => card.key);
}

/** What one cell of a row draws: its text, the moment of its time, the keys of its links, and,
 *  for Time per phase, its segments and the total it names. */
interface CellRead {
  readonly text: string;
  readonly time: string | null;
  readonly links: readonly string[];
  readonly segments: readonly string[];
  readonly total: string | null;
}

/** Every drawn row of `table` by its key, each cell by its column's header label. */
function readRows(table: Locator): Promise<Record<string, Record<string, CellRead>>> {
  return table.evaluate((node) => {
    const labels = [...node.querySelectorAll("thead th")].map((cell) =>
      (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, "").trim()
    );
    const rows: Record<string, Record<string, CellRead>> = {};
    for (const row of node.querySelectorAll<HTMLElement>("tbody tr[data-issue-row]")) {
      const cells: Record<string, CellRead> = {};
      [...row.children].forEach((cell, index) => {
        const label = labels[index] ?? "";
        if (label === "") return;
        cells[label] = {
          text: (cell.textContent ?? "").replace(/\s+/g, " ").trim(),
          time: cell.querySelector("time")?.getAttribute("datetime") ?? null,
          links: [...cell.querySelectorAll("a")].map((link) => (link.textContent ?? "").trim()),
          segments: [...cell.querySelectorAll("[data-phase-segment]")].map(
            (segment) => segment.getAttribute("data-phase-segment") ?? ""
          ),
          total: cell.querySelector("[data-phase-hours]")?.getAttribute("aria-label") ?? null,
        };
      });
      rows[row.dataset.issueRow ?? ""] = cells;
    }
    return rows;
  });
}

/** Where a cell's drawn value differs from `expected`, one line naming the issue, the column, what
 *  is drawn and what the route gives; nothing where it matches one of `expected`'s readings. */
function differs(
  key: string,
  label: string,
  cell: CellRead | undefined,
  expected: string | readonly string[]
): string[] {
  const readings = typeof expected === "string" ? [expected] : expected;
  const drawn = cell?.text;
  return drawn !== undefined && readings.includes(drawn)
    ? []
    : [`${key} ${label}: drawn ${JSON.stringify(drawn)}, the route gives ${readings.join(" or ")}`];
}

/** Where the four facts cells of `key` differ from `facts` (undefined for an issue with no Legion
 *  work), and where one of them reads a zero in place of a missing value. */
function factsDiffer(
  key: string,
  cells: Record<string, CellRead> | undefined,
  facts: Facts | undefined
): string[] {
  const work = measuredWork(facts);
  const phases = phasesWithHours(facts);
  const total = phaseTotal(facts);
  const phaseCell = cells?.["Time per phase"];
  const zero = ["Work time", "Review rounds", "Legion tree", "Time per phase"].flatMap((label) =>
    facts === undefined && /^0 [mh]\b|^0$/.test(cells?.[label]?.text ?? "")
      ? [`${key} ${label} reads ${cells?.[label]?.text} for an issue with no Legion work`]
      : []
  );
  return [
    ...zero,
    ...differs(key, "Work time", cells?.["Work time"], work === undefined ? "" : workWords(work)),
    ...differs(
      key,
      "Review rounds",
      cells?.["Review rounds"],
      facts === undefined ? "" : String(facts.review_rounds)
    ),
    ...differs(
      key,
      "Legion tree",
      cells?.["Legion tree"],
      facts === undefined ? "" : facts.tree_depth === 0 ? "Root" : facts.root_key
    ),
    ...(JSON.stringify(phaseCell?.segments ?? []) === JSON.stringify(phases) &&
    (phaseCell?.total ?? null) === (total > 0 ? `Time per phase, ${hoursWords(total)}` : null)
      ? []
      : [
          `${key} Time per phase: drawn ${JSON.stringify(phaseCell?.segments)} ${phaseCell?.total}, the route gives ${JSON.stringify(phases)} of ${total} h`,
        ]),
  ];
}

/** Where the card cells of `issue` differ from the board: Status from the listing, and Legion
 *  phase, Queue, Workers, Worker state, Time waiting, Why it waits and Time in Legion phase from
 *  its card, each blank for an issue with no card. */
function cardCellsDiffer(
  issue: IssueSummary,
  cells: Record<string, CellRead> | undefined,
  board: Board,
  nowMs: number
): string[] {
  const placed = cardsOf(board).get(issue.key);
  const card = placed?.card;
  const roles = [...(card?.roles ?? [])].sort(
    (a, b) => ROLE_ORDER.indexOf(a) - ROLE_ORDER.indexOf(b)
  );
  const blockers = card?.waiting?.reason === "blocked" ? (card.waiting.blocked_by ?? []) : [];
  const queue =
    card?.waiting_place != null
      ? `#${card.waiting_place}`
      : placed?.column === "queue"
        ? "In Legion queue"
        : "";
  return [
    ...differs(issue.key, "Status", cells?.Status, STATUS_WORDS[issue.status] ?? issue.status),
    ...differs(issue.key, "Legion phase", cells?.["Legion phase"], placed?.label ?? ""),
    ...differs(issue.key, "Queue", cells?.Queue, queue),
    ...differs(
      issue.key,
      "Workers",
      cells?.Workers,
      roles.map((role) => `${role.slice(0, 1).toUpperCase()}${role.slice(1)}`).join(", ")
    ),
    ...differs(
      issue.key,
      "Worker state",
      cells?.["Worker state"],
      card?.slot_use == null || card.slot_use === ""
        ? ""
        : wordOf(WORKER_STATE_WORDS, card.slot_use)
    ),
    ...differs(
      issue.key,
      "Time waiting",
      cells?.["Time waiting"],
      card?.waiting?.since == null ? "" : sinceWords(card.waiting.since, nowMs)
    ),
    ...differs(
      issue.key,
      "Why it waits",
      cells?.["Why it waits"],
      card?.waiting == null || card.waiting.reason === ""
        ? ""
        : `${wordOf(WAITING_WORDS, card.waiting.reason)}${blockers.join("")}`
    ),
    ...differs(
      issue.key,
      "Time in Legion phase",
      cells?.["Time in Legion phase"],
      card?.phase_since == null ? "" : sinceWords(card.phase_since, nowMs)
    ),
    ...differs(
      issue.key,
      "Open asks",
      cells?.["Open asks"],
      issue.open_asks > 0 ? String(issue.open_asks) : ""
    ),
  ];
}

/** The admission block of `seedLegionFixture` with `extra` added to what a blocker link holds,
 *  and the waiting line and slots `more` gives, as a read a minute later finds it. */
function admission(more: {
  cap?: number | null;
  active?: readonly string[];
  waiting?: readonly string[];
  trees?: readonly string[] | null;
  blocked?: readonly { issue: string; blockedBy: readonly string[] }[];
}): string {
  return JSON.stringify({
    ...(more.cap === null ? {} : { cap: more.cap ?? 4 }),
    active: ["ACME-1", "ACME-2", ...(more.active ?? [])],
    waiting: ["ACME-4", "ACME-3", ...(more.waiting ?? [])],
    ...(more.trees === null ? {} : { trees: ["ACME-1", "ACME-2", ...(more.trees ?? [])] }),
    blocked: [{ issue: "ACME-5", blockedBy: ["ACME-1"] }, ...(more.blocked ?? [])],
  });
}

/** ACME with k2's two more issues: ACME-8, an open child of the root ACME-1 that its tree has not
 *  started, which the board counts on ACME-1's card; and ACME-9, a Queue card a blocker link
 *  holds on ACME-2, written through the admission block a later read finds. */
async function seedQueueAndTree(): Promise<void> {
  const child = await createIssue({
    project: ACME,
    title: "A child not started",
    parent: "ACME-1",
  });
  await patchIssue(child.key, { status: "todo" });
  const held = await createIssue({ project: ACME, title: "Held behind the gadgets ask" });
  await patchIssue(held.key, { status: "todo" });
  await pushLegionObservations([
    { issue: child.key, field: "phase", value: '"admitted"', ago: "0 seconds" },
    { issue: held.key, field: "phase", value: '"admitted"', ago: "0 seconds" },
    {
      issue: "",
      field: "admission",
      value: admission({ blocked: [{ issue: held.key, blockedBy: ["ACME-2"] }] }),
      ago: "0 seconds",
    },
  ]);
  expect([child.key, held.key], "ACME's next keys after the seed").toEqual(["ACME-8", "ACME-9"]);
}

/** The (i) hint's terms and values, opened from its trigger, with the moment its read time names. */
async function readHint(page: Page): Promise<{ terms: string[]; values: string[]; at: string }> {
  const hint = page.getByRole("dialog", { name: "Legion daemon" });
  const trigger = page.getByRole("button", { exact: true, name: "Legion daemon" });
  if ((await expandedState("the Legion daemon hint", trigger)) === "shut") await trigger.click();
  await expect(hint).toBeVisible();
  return {
    terms: await hint.locator("dt").allTextContents(),
    values: await hint.locator("dd").allTextContents(),
    at: (await hint.locator("dd time").getAttribute("datetime")) ?? "",
  };
}

/** The tick boxes the Columns menu offers, by the column each names; the menu is shut after. */
async function columnsMenuNames(page: Page): Promise<string[]> {
  await page.getByRole("button", { exact: true, name: "Columns" }).click();
  const menu = page.getByRole("dialog", { name: "Columns" });
  await expect(menu).toBeVisible();
  const names = (await menu.locator("label").allTextContents()).map((name) => name.trim());
  await page.keyboard.press("Escape");
  await expect(menu).toHaveCount(0);
  return names;
}

/** k2's thirteenth column, which a newer server adds after Held, by its id and its label. */
const X_PHASE = { id: "x_phase", label: "X phase" } as const;

/** The hours k2 gives the issue in the thirteenth column under that column's own key. */
const X_PHASE_HOURS = 2;

/** Adds the thirteenth column after Held to a board snapshot, holding the card of `key`, with the
 *  column's hours in that issue's facts: what a newer server sends for an issue in a phase this
 *  build does not know. The route's last column is Held, which the row asserts. */
function addThirteenthColumn(board: Board, key: string): void {
  const card: Card = {
    blocked_by: null,
    children: 0,
    key,
    phase: X_PHASE.id,
    phase_since: null,
    priority: null,
    roles: null,
    slot: null,
    slot_since: null,
    slot_use: null,
    title: key,
    tree: key,
    tree_slot: null,
    waiting: null,
    waiting_on: null,
    waiting_place: null,
  };
  board.columns = [
    ...(board.columns ?? []),
    // The generated column ids name today's twelve: a newer server's column is none of them.
    { id: X_PHASE.id as string as Column["id"], label: X_PHASE.label, cards: [card] },
  ];
  const now = new Date().toISOString();
  board.issues = {
    ...board.issues,
    [key]: {
      as_of: null,
      closed_at: null,
      growth: null,
      hours_by_phase: { [X_PHASE.id]: X_PHASE_HOURS },
      opened_at: now,
      review_rounds: 0,
      root_key: key,
      slot_hours: 0,
      tree_depth: 0,
      updated_at: now,
      waiting_reason: "",
      work_first_at: null,
      work_hours: 0,
      work_last_at: null,
    },
  };
}

// SLOW(>=2s): ACME seeded with two more issues through later reads, then Open read in the table
// with the Queue filter and on the Board by Legion phase, then a third issue and Open read again
// under one board frame that adds a thirteenth column.
test("Open's Legion queue lists the Queue column's cards in the board's order, and its Board by Legion phase draws the board's twelve columns and a thirteenth one a newer server adds", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await seedQueueAndTree();
    const board = await getLegionBoard(ACME);
    const queue = columnKeys(board, "queue");
    expect(queue, "the Queue column holds the daemon's line, then what a blocker holds").toEqual([
      "ACME-4",
      "ACME-3",
      "ACME-5",
      "ACME-9",
    ]);
    const open = await listIssuesWhere({ project: ACME, open: true });
    const cards = cardsOf(board);
    // The Legion queue: every issue with a Place, and every card of the Queue column.
    const queued = queueSorted(
      open.filter(
        (issue) =>
          cards.get(issue.key)?.card.waiting_place != null ||
          cards.get(issue.key)?.column === "queue"
      ),
      board
    ).map((issue) => issue.key);

    const page = await context.newPage();
    await page.setViewportSize({ height: 1600, width: 1440 });
    await page.goto(`/projects/${ACME}/issues?f.queue=Queued`);
    const table = issuesTable(page);
    await expect(table).toBeVisible();
    await expect.poll(() => rowKeys(table)).toEqual(queued);
    const rows = await readRows(table);
    expect(
      queued.filter((key) => rows[key]?.["Legion phase"]?.text === "Queue"),
      "the rows whose Legion phase is Queue, in their order"
    ).toEqual(queue);

    // The Board by Legion phase of the same view: one column per board column, empty ones too.
    await page.goto(`/projects/${ACME}/issues?layout=board&group=legion_phase`);
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    const routeColumns = (board.columns ?? []).map((column) => [column.id, column.label]);
    await expect
      .poll(async () =>
        (await drawnColumns(page))
          .filter((column) => !column.rail)
          .map(({ id, title }) => [id, title])
      )
      .toEqual(routeColumns);
    expect(routeColumns, "the board route's columns").toEqual(BOARD_COLUMNS);
    const openKeys = new Set(open.map((issue) => issue.key));
    // The poll above settled the columns this loop walks, and the Board draws their cards with
    // them.
    for (const drawn of (await drawnColumns(page)).filter((column) => !column.rail)) {
      const expected = columnKeys(board, drawn.id).filter((key) => openKeys.has(key));
      if (drawn.id === "queue") expect(drawn.cards, "the Queue column's cards").toEqual(expected);
      else
        expect([...drawn.cards].sort(), `the ${drawn.title} column's cards`).toEqual(
          [...expected].sort()
        );
    }
    expect(await columnCount(page, "queue")).toBe(board.header.queue_length);

    // One board frame, the snapshot, with a thirteenth column after Held that holds the card of C,
    // an open issue with hours under that column's key; no later frame reaches the page. The
    // Board draws it as a column, the table names it in C's cell and in the Legion phase filter,
    // and Time per phase, which has no colour for it, counts its hours as Other.
    const c = (await createIssue({ project: ACME, title: "Worked in a thirteenth column" })).key;
    const routed = await context.newPage();
    await routeLive(routed, {
      hold: true,
      edit: (frame) => {
        if (frame.body !== undefined) addThirteenthColumn(frame.body, c);
      },
    });
    await routed.setViewportSize({ height: 1600, width: 1440 });
    await routed.goto(
      `/projects/${ACME}/issues?layout=board&group=legion_phase&cols=key,title,legion_phase,phase_hours`
    );
    await expect(routed.getByRole("region", { name: "Project board" })).toBeVisible();
    await expect
      .poll(async () =>
        (await drawnColumns(routed))
          .filter((column) => !column.rail)
          .map(({ id, title }) => [id, title])
      )
      .toEqual([...BOARD_COLUMNS, [X_PHASE.id, X_PHASE.label]]);
    await expect
      .poll(async () => (await drawnColumns(routed)).find(({ id }) => id === X_PHASE.id)?.cards)
      .toEqual([c]);

    await routed.getByRole("button", { exact: true, name: "Table" }).click();
    const routedTable = issuesTable(routed);
    await expect(routedTable.locator(`tr[data-issue-row="${c}"]`)).toBeVisible();
    await expect
      .poll(async () => (await readRows(routedTable))[c]?.["Legion phase"]?.text)
      .toBe(X_PHASE.label);
    const phasePicks = (await readFilter(routed, "Legion phase")).options;
    expect(
      phasePicks.map(([words]) => words),
      "the Legion phase filter: the board's columns in its order, then None"
    ).toEqual([...BOARD_COLUMNS.map(([, label]) => label), X_PHASE.label, "None"]);
    expect(phasePicks.find(([words]) => words === X_PHASE.label)?.[1]).toBe(1);

    // C's Time per phase: its hours under the thirteenth column's key are Other's, with no line of
    // their own.
    const cHours = routedTable.locator(`tr[data-issue-row="${c}"] [data-phase-hours]`);
    await expect(cHours).toHaveAttribute(
      "aria-label",
      `Time per phase, ${hoursWords(X_PHASE_HOURS)}`
    );
    await cHours.hover();
    await expect(routed.getByRole("tooltip").locator("[data-phase-hours-line]")).toHaveCount(1);
    // The count wait above drew the one line this read takes.
    expect(
      (await tooltipLines(routed)).map(({ phase, name, hours, share }) => [
        phase,
        name,
        hours,
        share,
      ])
    ).toEqual([["other", "Other", hoursWords(X_PHASE_HOURS), "100%"]]);
    await routed.mouse.move(5, 5);
    await expect(routed.getByRole("tooltip")).toHaveCount(0);

    // Time per phase's filter offers no pick for the thirteenth column, and its Other pick lists C.
    const hourPicks = (await readFilter(routed, "Time per phase")).options.map(([words]) => words);
    expect(hourPicks).toContain("Other");
    expect(hourPicks).not.toContain(X_PHASE.label);
    expect(hourPicks).not.toContain(X_PHASE.id);
    await routed.getByRole("button", { exact: true, name: "Filter Time per phase" }).click();
    await routed
      .getByRole("listbox", { name: "Time per phase options" })
      .getByRole("option", { name: /^Other, \d+ issues?$/ })
      .click();
    await routed.keyboard.press("Escape");
    await expect
      .poll(() => new URL(routed.url()).searchParams.getAll("f.phase_hours"))
      .toEqual(["other"]);
    await expect.poll(() => rowKeys(routedTable)).toContain(c);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): the facts consumer's rows of three issues, up to 75 s, before their cards are read.
test("a Board card draws the Legion fields the board route gives it and no slot number", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await seedQueueAndTree();
    // The root ACME-1, ACME-2 waiting on a person, and ACME-9, which a blocker holds.
    const three = ["ACME-1", "ACME-2", "ACME-9"];
    await writeRoleEvent("ACME-1", "implementer", "2 hours");
    await writeRoleEvent("ACME-2", "architect", "45 minutes");
    await writeRoleEvent("ACME-9", "architect", "30 minutes");
    await waitForIssueFacts(three);

    const page = await context.newPage();
    await page.setViewportSize({ height: 1200, width: 1440 });
    await page.goto(`/projects/${ACME}/issues?layout=board&group=legion_phase`);
    // `layout=board` draws the Board, by Legion phase.
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    await expect(page.locator('[data-board-card="ACME-9"]')).toBeVisible();

    const facts = async (key: string) =>
      page.locator(`[data-board-card="${key}"]`).evaluate((card) =>
        Object.fromEntries(
          [...card.querySelectorAll("[data-legion-fact]")].map((fact) => [
            fact.getAttribute("data-legion-fact") ?? "",
            // A fact of a name and a value draws them as two spans with no space between.
            fact.children.length >= 2
              ? [...fact.children]
                  .slice(0, 2)
                  .map((part) => (part.textContent ?? "").trim())
                  .join(" ")
              : (fact.textContent ?? "").replace(/\s+/g, " ").trim(),
          ])
        )
      );
    // What each card's Legion fields must read, from the route at the moment of the read.
    const expected = async (key: string, board: Board): Promise<Record<string, string[]>> => {
      const card = cardsOf(board).get(key)?.card;
      if (card === undefined) throw new Error(`the board has no card ${key}`);
      const work = measuredWork(boardFacts(board, key));
      const roles = [...(card.roles ?? [])]
        .sort((a, b) => ROLE_ORDER.indexOf(a) - ROLE_ORDER.indexOf(b))
        .map((role) => `${role.slice(0, 1).toUpperCase()}${role.slice(1)}`);
      const blockers =
        (card.blocked_by ?? []).length > 0
          ? (card.blocked_by ?? [])
          : (card.waiting?.blocked_by ?? []);
      const fields: Record<string, string[]> = {};
      if (card.phase_since !== null)
        fields["phase-time"] = sinceWords(card.phase_since, Date.now()).map(
          (time) => `Time in Legion phase ${time}`
        );
      if (work !== undefined) fields["work-time"] = [`Work time ${workWords(work)}`];
      if (roles.length > 0) fields.roles = [`Workers ${roles.join(", ")}`];
      if (blockers.length > 0) fields["blocked-by"] = [`Blocked by ${blockers.join(", ")}`];
      if (card.waiting_on !== null) {
        const since =
          card.waiting_on.since === null
            ? ""
            : ` since ${await page.evaluate((at) => new Date(at).toLocaleString(), card.waiting_on.since)}`;
        fields["waiting-on"] = [
          `Waiting on ${card.waiting_on.kind}${since}${card.waiting_on.answered_at === null ? "" : ", answered"}`,
        ];
      }
      if (card.children > 0)
        fields.children = [
          `${card.children} ${card.children === 1 ? "issue" : "issues"} of this tree not started`,
        ];
      return fields;
    };
    await expect
      .poll(
        async () => {
          const board = await getLegionBoard(ACME);
          const wrong: string[] = [];
          for (const key of three) {
            const drawn = await facts(key);
            const wanted = await expected(key, board);
            const names = [...new Set([...Object.keys(drawn), ...Object.keys(wanted)])].sort();
            for (const name of names) {
              const text = drawn[name];
              if (text === undefined || !(wanted[name] ?? []).includes(text))
                wrong.push(`${key} ${name}: drawn ${text}, the route gives ${wanted[name]}`);
            }
          }
          return wrong;
        },
        { timeout: 30_000 }
      )
      .toEqual([]);
    const board = await getLegionBoard(ACME);
    // Each field the comparison reads holds a value on one of the three cards at least.
    const read = Object.assign({}, ...(await Promise.all(three.map(facts)))) as Record<
      string,
      string
    >;
    for (const name of ["phase-time", "work-time", "roles", "blocked-by", "waiting-on", "children"])
      expect(read[name], `no card of the three draws its ${name}`).toBeDefined();
    expect((await facts("ACME-9"))["blocked-by"]).toBe("Blocked by ACME-2");
    expect((await facts("ACME-1")).children).toBe("1 issue of this tree not started");
    // A Queue card whose issue has no worker in a turn draws no Workers line.
    expect(cardsOf(board).get("ACME-4")?.card.roles ?? []).toEqual([]);
    expect((await facts("ACME-4")).roles).toBeUndefined();
    expect((await facts("ACME-2")).roles).toBeUndefined();
    // No card draws a slot number: the slot holders ACME-1 (slot 0) and ACME-2 (slot 1) carry
    // no number of their own, and no card names a slot.
    for (const [key, slot] of [
      ["ACME-1", 0],
      ["ACME-2", 1],
    ] as const) {
      expect(cardsOf(board).get(key)?.card.slot).toBe(slot);
      const card = page.locator(`[data-board-card="${key}"]`);
      await expect(card.locator("[data-queue-place]")).toHaveCount(0);
      await expect(card).not.toContainText(`#${slot}`);
    }
    // The card waits and the facts poll above drew the Board, which draws all of its cards at
    // once.
    for (const text of await page.locator("[data-board-card]").allTextContents())
      expect(text).not.toMatch(/\bslots?\b/i);
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("a claimed card's chip writes the session's whole title on lines inside the card, with the age and state under it", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const holder = "Implementer whose session title runs on past the width of a card".padEnd(
      120,
      "!"
    );
    const token = await mintAgentToken("views-claim", "alice");
    await setLiveSessions([{ session_id: "views-claim-session", title: holder }]);
    await claimIssue("ACME-1", {
      actor: { id: "views-claim-session", kind: "session", origin: { session_title: holder } },
      as: "agent",
      token,
    });
    const page = await context.newPage();
    for (const { height, width } of [
      { height: 900, width: 1440 },
      { height: 844, width: 390 },
    ]) {
      await page.setViewportSize({ height, width });
      await page.goto(`/projects/${ACME}/issues?layout=board&group=legion_phase`);
      await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
      const card = page.locator('[data-board-card="ACME-1"]');
      await expect(card.getByTestId("issue-claim")).toBeVisible();
      await card.scrollIntoViewIfNeeded();
      const drawn = await card.evaluate((node) => {
        const chip = node.querySelector<HTMLElement>('[data-testid="issue-claim"]');
        const name = chip?.children[0];
        const state = chip?.children[1];
        if (chip === null || name === undefined || state === undefined)
          throw new Error("the card's claim chip has no name line and state line");
        const range = document.createRange();
        range.selectNodeContents(name);
        const lines = [...range.getClientRects()].filter((rect) => rect.width > 0);
        const box = node.getBoundingClientRect();
        const below = state.getBoundingClientRect();
        return {
          card: { left: box.left, right: box.right, top: box.top, bottom: box.bottom },
          lines: lines.map((rect) => ({
            bottom: rect.bottom,
            left: rect.left,
            right: rect.right,
            top: rect.top,
          })),
          name: (name.textContent ?? "").replace(/\s+/g, " "),
          nameBottom: name.getBoundingClientRect().bottom,
          state: { bottom: below.bottom, left: below.left, right: below.right, top: below.top },
          stateText: (state.textContent ?? "").trim(),
        };
      });
      const where = `at ${width}x${height}`;
      expect(drawn.name, `${where}: the chip names the whole title`).toContain(holder);
      expect(
        new Set(drawn.lines.map((line) => Math.round(line.top))).size,
        `${where}: the name is written on one line`
      ).toBeGreaterThan(1);
      for (const line of [...drawn.lines, drawn.state]) {
        expect(line.left, `${where}: a line starts left of the card`).toBeGreaterThanOrEqual(
          drawn.card.left - 0.5
        );
        expect(line.right, `${where}: a line runs past the card`).toBeLessThanOrEqual(
          drawn.card.right + 0.5
        );
        expect(line.bottom).toBeLessThanOrEqual(drawn.card.bottom + 0.5);
      }
      expect(
        drawn.state.top,
        `${where}: the age and state are not under the name`
      ).toBeGreaterThanOrEqual(drawn.nameBottom - 0.5);
      expect(drawn.stateText).not.toBe("");
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): the hint, the tabs and the Board read, a failed read seeded and read at two widths,
// then a second project with a daemon and no read opened with its later frames held.
test("the (i) hint reads the board's capacity, trees and read time, a failed read names its error under the tabs, and no text names a slot", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const open = await listIssuesWhere({ project: ACME, open: true });
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await expect
      .poll(() => tabNames(page))
      .toEqual([`Open, ${open.length} ${open.length === 1 ? "issue" : "issues"}`, "Done"]);

    // The hint: Capacity, Trees and Last read, each the route's header at the moment it is read.
    await expect
      .poll(async () => {
        const { header } = await getLegionBoard(ACME);
        const hint = await readHint(page);
        return {
          matches:
            hint.values[0] ===
              (header.slot_cap === null
                ? String(header.slots_in_use)
                : `${header.slots_in_use} of ${header.slot_cap}`) &&
            hint.values[1] === String(header.trees) &&
            Date.parse(hint.at) === Date.parse(header.read_at ?? ""),
          terms: hint.terms,
        };
      })
      .toEqual({ matches: true, terms: ["Capacity", "Trees", "Last read"] });
    const hint = page.getByRole("dialog", { name: "Legion daemon" });
    await expect(hint).not.toContainText(/queue/i);
    const main = page.getByTestId("main-content");
    expect(await main.innerText()).not.toMatch(/\bslots?\b/i);
    await page.keyboard.press("Escape");

    // The layout control offers the table and the Board, and nothing else.
    expect(
      await page.getByRole("group", { name: "Layout" }).getByRole("button").allTextContents()
    ).toEqual(["Table", "Board"]);
    await page.getByRole("button", { exact: true, name: "Board" }).click();
    await page.getByRole("button", { exact: true, name: "Group by" }).click();
    await page
      .getByRole("dialog", { name: "Group by" })
      .locator('input[type="radio"][value="legion_phase"]')
      .click();
    await page.keyboard.press("Escape");
    await expect(page.locator('section[data-board-column="queue"]')).toBeVisible();
    const { header } = await getLegionBoard(ACME);
    expect(await columnCount(page, "queue")).toBe(header.queue_length);
    expect(await main.innerText()).not.toMatch(/\bslots?\b/i);

    // A failed read: its whole error on a line of its own under the tabs, the tabs whole.
    await seedFailedLegionRead(ACME, "the daemon answered 503 Service Unavailable");
    await page.getByRole("button", { exact: true, name: "Table" }).click();
    for (const { height, width } of [
      { height: 900, width: 1440 },
      { height: 844, width: 390 },
    ]) {
      await page.setViewportSize({ height, width });
      const alert = page
        .getByRole("alert")
        .filter({ hasText: "The last read of the Legion daemon failed" });
      await expect
        .poll(async () => {
          const { header: now } = await getLegionBoard(ACME);
          return (
            now.read_ok === false &&
            (await alert.textContent()) ===
              `The last read of the Legion daemon failed: ${now.read_error}`
          );
        })
        .toBe(true);
      // The alert poll above settled the alert, and the tabs poll at the top drew both tabs this
      // reads.
      const placed = await page.evaluate(() => {
        const tabs = document.querySelector('[role="tablist"][aria-label="Views"]');
        const line = [...document.querySelectorAll('[role="alert"]')].find((node) =>
          (node.textContent ?? "").startsWith("The last read of the Legion daemon failed")
        );
        if (tabs === null || line === undefined) throw new Error("no tabs or no alert");
        const row = tabs.getBoundingClientRect();
        const box = line.getBoundingClientRect();
        return {
          alert: {
            clipped: line.scrollWidth > line.clientWidth,
            left: box.left,
            top: box.top,
            width: box.width,
          },
          row: { bottom: row.bottom, left: row.left, width: row.width },
          tabs: [...tabs.querySelectorAll('[role="tab"]')].map((tab) => {
            const rect = tab.getBoundingClientRect();
            return {
              clipped: tab.scrollWidth > tab.clientWidth,
              left: rect.left,
              right: rect.right,
            };
          }),
        };
      });
      const where = `at ${width}x${height}`;
      expect(placed.alert.top, `${where}: the alert is not under the tabs`).toBeGreaterThanOrEqual(
        placed.row.bottom - 0.5
      );
      expect(placed.alert.left, `${where}: the alert does not start its own line`).toBeCloseTo(
        placed.row.left,
        0
      );
      expect(placed.alert.clipped, `${where}: the alert's error is cut`).toBe(false);
      for (const tab of placed.tabs) {
        expect(tab.clipped, `${where}: a tab is cut`).toBe(false);
        expect(tab.left).toBeGreaterThanOrEqual(0);
        expect(tab.right).toBeLessThanOrEqual(width);
      }
    }

    // A project with a daemon setting and no read: its hint says the daemon has not been read.
    // Every board frame after the first is held, so the hint draws the first snapshot. The
    // sampler reads a new setting within a minute, so a project whose first snapshot already
    // carries a read is set aside for the next.
    await page.setViewportSize({ height: 900, width: 1440 });
    let unread = false;
    for (const key of ["GADGETS", "GIZMOS", "SPROCKETS"]) {
      await createProject({ key, name: key.slice(0, 1) + key.slice(1).toLowerCase() });
      await sql(
        `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
           VALUES ('${key}', '${daemon.url}', '{"kind":"user","id":"alice"}', now())`
      );
      const other = await context.newPage();
      await routeLive(other, { hold: true });
      await other.goto(`/projects/${key}/issues`);
      await expect(other.getByRole("tablist", { name: "Views" })).toBeVisible();
      await other.getByRole("button", { exact: true, name: "Legion daemon" }).click();
      const hint = other.getByRole("dialog", { name: "Legion daemon" });
      // Its first snapshot answers the hint: either no read, or the read the sampler made.
      await expect(hint).toHaveText(/^(?:Dispatch has not read the daemon yet\.|Capacity.*)$/, {
        timeout: 30_000,
      });
      // The toHaveText wait above settled the hint on one of its two texts, and the page holds
      // every later frame.
      if ((await hint.textContent())?.startsWith("Capacity")) {
        await other.close();
        continue;
      }
      expect(await tabNames(other)).toEqual(["Open, 0 issues", "Done"]);
      await expect(hint).toHaveText("Dispatch has not read the daemon yet.");
      await other.close();
      unread = true;
      break;
    }
    expect(unread, "the sampler read each new project before its first snapshot").toBe(true);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): the daemon setting's read held, failed through its retries, answered, then failed
// again once it is stale, 30 s after the answer.
test("the daemon setting's read: held it shows the loading state, failed it names its error with Retry and offers no board-fed column, and a later failure keeps what the answer gave", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    let mode: "hold" | "fail" | "pass" = "hold";
    let reads = 0;
    const released = Promise.withResolvers<void>();
    const message = "The daemon setting store did not answer.";
    await page.route(`**/api/v1/projects/${ACME}/legion/daemon`, async (route) => {
      reads += 1;
      if (mode === "hold") await released.promise;
      if (mode === "fail") {
        await route.fulfill({
          body: JSON.stringify({ code: "INTERNAL", error: message }),
          contentType: "application/json",
          status: 500,
        });
        return;
      }
      await route.continue();
    });
    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.getByRole("status", { name: "Loading issues" })).toBeVisible();
    await expect(page.getByRole("tablist", { name: "Views" })).toHaveCount(0);

    // Failed: an alert under the tabs names the error, with Retry; no board-fed column.
    mode = "fail";
    released.resolve();
    const alert = page
      .getByRole("alert")
      .filter({ hasText: "Could not read the Legion daemon setting" });
    await expect(alert).toHaveText(`Could not read the Legion daemon setting: ${message} Retry`);
    await expect(
      alert.getByRole("button", { name: "Retry reading the Legion daemon setting" })
    ).toBeVisible();
    expect(await tabNames(page)).toEqual([expect.stringMatching(/^Open, \d+ issues$/), "Done"]);
    const failedNames = await columnsMenuNames(page);
    for (const fed of BOARD_FED_HEADERS) expect(failedNames).not.toContain(fed);

    // Answered after Retry: Legion phase and the other board-fed columns are offered.
    mode = "pass";
    await alert.getByRole("button", { name: "Retry reading the Legion daemon setting" }).click();
    await expect(alert).toHaveCount(0);
    expect(await tabNames(page)).toEqual([expect.stringMatching(/^Open, \d+ issues$/), "Done"]);
    const answeredNames = await columnsMenuNames(page);
    for (const fed of BOARD_FED_HEADERS) expect(answeredNames).toContain(fed);

    // The next read fails: it comes once the answer is stale, on the window's focus, and the
    // page stays as the answered read drew it.
    mode = "fail";
    const before = reads;
    await expect
      .poll(
        async () => {
          await page.evaluate(() =>
            document.dispatchEvent(new Event("visibilitychange", { bubbles: true }))
          );
          return reads - before;
        },
        { intervals: [1_000], timeout: 60_000 }
      )
      .toBeGreaterThanOrEqual(3);
    await expect(alert).toHaveCount(0);
    expect(await tabNames(page)).toEqual([expect.stringMatching(/^Open, \d+ issues$/), "Done"]);
    expect(await columnsMenuNames(page)).toEqual(answeredNames);
  } finally {
    await context.close();
    await daemon.close();
  }
});

test("the (i) hint of a read whose admission block names no cap reads the capacity in use alone and the trees it is given", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await pushLegionObservation("", "admission", admission({ cap: null, trees: null }));
    const { header } = await getLegionBoard(ACME);
    expect(header.slot_cap).toBeNull();
    expect(header.trees).toBe(0);
    const page = await context.newPage();
    await page.goto(`/projects/${ACME}/issues`);
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    await expect
      .poll(async () => (await readHint(page)).values.slice(0, 2))
      .toEqual([String(header.slots_in_use), "0"]);
    expect((await readHint(page)).terms).toEqual(["Capacity", "Trees", "Last read"]);
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The facts' rows a k5 row waits for, with the role event each issue's Legion work starts from. */
const ACME_ROLE_EVENTS: readonly (readonly [string, string, string])[] = [
  ["ACME-1", "implementer", "2 hours"],
  ["ACME-2", "architect", "50 minutes"],
  ["ACME-3", "architect", "85 minutes"],
  ["ACME-4", "architect", "75 minutes"],
  ["ACME-5", "architect", "65 minutes"],
];

// SLOW(>=2s): the eval refresher's pass for ACME and the facts consumer's rows of eight issues, up
// to 75 s, then Open and Done read cell by cell against the routes.
test("each Legion cell of ACME's open and done rows equals its route's value, and an open issue with Legion work and no card takes the board's facts", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    // O: Legion work, and no card: no observation names it and no card's tree holds it.
    const other = await createIssue({ project: ACME, title: "Worked by Legion with no card" });
    for (const [key, role, ago] of ACME_ROLE_EVENTS) await writeRoleEvent(key, role, ago);
    await writeRoleEvent(other.key, "implementer", "40 minutes");
    await Promise.all([
      waitForIssueFacts([...ACME_ROLE_EVENTS.map(([key]) => key), other.key, "ACME-6", "ACME-7"]),
      waitForLegionWorkTimeEval(ACME),
    ]);

    const items: string[] = [];
    const page = await context.newPage();
    page.on("request", (request) => {
      if (request.url().includes(`/evals/${legionWorkTimeEval}/items`)) items.push(request.url());
    });
    await page.setViewportSize({ height: 1600, width: 1920 });
    await page.goto(`/projects/${ACME}/issues?cols=${LEGION_COLS}`);
    const table = issuesTable(page);
    await expect(table.locator(`tr[data-issue-row="${other.key}"]`)).toBeVisible();
    // ACME-4 is first in the daemon's line: its Queue cell is its Place.
    await expect.poll(async () => (await readRows(table))["ACME-4"]?.Queue?.text).toBe("#1");

    const open = await listIssuesWhere({ project: ACME, open: true });
    const shown = open.filter(
      (issue) =>
        issue.key === other.key ||
        ["ACME-1", "ACME-2", "ACME-3", "ACME-4", "ACME-5"].includes(issue.key)
    );
    expect(shown.length).toBe(6);
    await expect
      .poll(
        async () => {
          const board = await getLegionBoard(ACME);
          const rows = await readRows(table);
          const nowMs = Date.now();
          return shown.flatMap((issue) => [
            ...cardCellsDiffer(issue, rows[issue.key], board, nowMs),
            ...factsDiffer(issue.key, rows[issue.key], boardFacts(board, issue.key)),
            ...differs(issue.key, "Closed", rows[issue.key]?.Closed, ""),
          ]);
        },
        { timeout: 30_000 }
      )
      .toEqual([]);
    const board = await getLegionBoard(ACME);
    // The map holds O, an open issue with Legion work and no card, and O's cells are its facts.
    expect(board.issues?.[other.key], "the board's issues map holds no entry for O").toBeDefined();
    expect(cardsOf(board).has(other.key)).toBe(false);
    // Each facts column holds a value on one of the rows at least.
    const held = shown.map((issue) => boardFacts(board, issue.key));
    expect(held.some((facts) => measuredWork(facts) !== undefined)).toBe(true);
    expect(held.some((facts) => phaseTotal(facts) > 0)).toBe(true);
    expect(held.some((facts) => facts !== undefined && facts.tree_depth === 0)).toBe(true);
    // Open reads no items row: it lists no closed issue.
    expect(items, "Open read the items route").toEqual([]);

    // Done: ACME-6 and ACME-7, closed in the last 28 days, which the board's map does not hold,
    // take their facts from their items rows.
    await page.goto(`/projects/${ACME}/issues?view=done&cols=${LEGION_COLS}`);
    await expect(table.locator('tr[data-issue-row="ACME-7"]')).toBeVisible();
    const done = await listIssuesWhere({ project: ACME, sort: "-closed_at", status: ["done"] });
    const page1 = done.slice(0, 50);
    const doneBoard = await getLegionBoard(ACME);
    const unheld = page1.filter((issue) => doneBoard.issues?.[issue.key] === undefined);
    expect(unheld.map((issue) => issue.key).sort()).toEqual(["ACME-6", "ACME-7"]);
    const closes = unheld.map((issue) => Date.parse(issue.closed_at ?? ""));
    const day = 86_400_000;
    const from = new Date(Math.floor(Math.min(...closes) / day) * day).toISOString();
    const to = new Date((Math.floor(Math.max(...closes) / day) + 1) * day).toISOString();
    const answered = await getLegionWorkTimeItems(ACME, {
      from,
      limit: 200,
      row_kind: "issue",
      to,
    });
    const rowsByKey = new Map((answered.items ?? []).map((row) => [row.row_key, row]));
    await expect
      .poll(
        async () => {
          const rows = await readRows(table);
          return page1.flatMap((issue) => {
            const item = rowsByKey.get(issue.key);
            const facts =
              boardFacts(doneBoard, issue.key) ??
              (item === undefined ? undefined : itemFacts(item));
            const cells = rows[issue.key];
            return [
              ...differs(issue.key, "Status", cells?.Status, "Done"),
              ...differs(issue.key, "Legion phase", cells?.["Legion phase"], ""),
              ...(Date.parse(cells?.Closed?.time ?? "") === Date.parse(issue.closed_at ?? "")
                ? []
                : [`${issue.key} Closed: drawn ${cells?.Closed?.time}, listed ${issue.closed_at}`]),
              ...factsDiffer(issue.key, cells, facts),
            ];
          });
        },
        { timeout: 30_000 }
      )
      .toEqual([]);
    expect(rowsByKey.has("ACME-6") && rowsByKey.has("ACME-7")).toBe(true);
    // Done read the items over the UTC days of those closes, issue rows, at most 200 a read.
    expect(items.length).toBeGreaterThan(0);
    for (const url of items) {
      const query = new URL(url).searchParams;
      expect(query.get("row_kind")).toBe("issue");
      expect(Number(query.get("limit"))).toBeLessThanOrEqual(200);
      expect(Date.parse(query.get("from") ?? "")).toBe(Date.parse(from));
      expect(Date.parse(query.get("to") ?? "")).toBe(Date.parse(to));
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): WIDGETS' 24 cards and the facts consumer's rows of three of them, up to 75 s, with
// the eval refresher's pass for WIDGETS, then Open read cell by cell against the board route.
test("each Legion cell of WIDGETS' open rows equals the board route's card and facts", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await startFakeLegionDaemon();
  const context = await asUser(browser, "alice");
  try {
    const fixture = await seedLegionFactsFixture(daemon.url);
    await writeRoleEvent("WIDGETS-1", "implementer", "70 minutes");
    await writeRoleEvent("WIDGETS-5", "implementer", "50 minutes");
    await writeRoleEvent("WIDGETS-14", "reviewer", "60 minutes");
    await Promise.all([
      waitForIssueFacts(["WIDGETS-1", "WIDGETS-5", "WIDGETS-14"]),
      waitForLegionWorkTimeEval(LEGION_FACTS_PROJECT),
    ]);
    const page = await context.newPage();
    await page.setViewportSize({ height: 2400, width: 1920 });
    await page.goto(`/projects/${LEGION_FACTS_PROJECT}/issues?cols=${LEGION_COLS}`);
    const table = issuesTable(page);
    await expect(table.locator('tr[data-issue-row="WIDGETS-24"]')).toBeVisible();
    const open = await listIssuesWhere({ open: true, project: LEGION_FACTS_PROJECT });
    const carded = open.filter((issue) => fixture.keys.includes(issue.key));
    expect(carded.length).toBe(fixture.keys.length);
    await expect
      .poll(
        async () => {
          const board = await getLegionBoard(LEGION_FACTS_PROJECT);
          const rows = await readRows(table);
          const nowMs = Date.now();
          return carded.flatMap((issue) => [
            ...cardCellsDiffer(issue, rows[issue.key], board, nowMs),
            ...factsDiffer(issue.key, rows[issue.key], boardFacts(board, issue.key)),
          ]);
        },
        { timeout: 30_000 }
      )
      .toEqual([]);
    const board = await getLegionBoard(LEGION_FACTS_PROJECT);
    const cards = [...cardsOf(board).values()];
    // Each card column holds a value on one of the rows at least.
    expect(cards.some((placed) => (placed.card.roles ?? []).length > 0)).toBe(true);
    expect(cards.some((placed) => placed.card.slot_use !== null)).toBe(true);
    expect(cards.some((placed) => placed.card.waiting?.since != null)).toBe(true);
    expect(cards.some((placed) => placed.card.waiting_place !== null)).toBe(true);
    expect(measuredWork(boardFacts(board, "WIDGETS-1"))).toBeDefined();
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): two more issues seeded through a later read, then six addresses of Open read.
test("Status and Legion phase are two columns, the Legion phase filter offers the board's columns and None, and the Queue filter selects the Legion queue", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 1400, width: 1920 });
    const table = issuesTable(page);
    // The Legion queue: ACME-4 and ACME-3 in the daemon's line, then ACME-5, a Queue card with no
    // Place, which reads In Legion queue with no number.
    await page.goto(`/projects/${ACME}/issues?f.queue=Queued`);
    await expect.poll(() => rowKeys(table)).toEqual(["ACME-4", "ACME-3", "ACME-5"]);
    const queued = await readRows(table);
    expect(queued["ACME-4"]?.Queue?.text).toBe("#1");
    expect(queued["ACME-3"]?.Queue?.text).toBe("#2");
    expect(queued["ACME-5"]?.Queue?.text).toBe("In Legion queue");

    // Two In progress issues: one with a card in Implementing, one with none.
    const carded = await createIssue({ project: ACME, title: "Implemented by Legion" });
    await patchIssue(carded.key, { status: "in_progress" });
    await pushLegionObservation(carded.key, "phase", '"implementing"');
    const bare = await createIssue({ project: ACME, title: "In progress with no card" });
    await patchIssue(bare.key, { status: "in_progress" });
    const board = await getLegionBoard(ACME);
    const cards = cardsOf(board);
    expect(cards.get(carded.key)?.column).toBe("implementing");
    expect(cards.has(bare.key)).toBe(false);
    const open = await listIssuesWhere({ project: ACME, open: true });
    const keysOf = (issues: readonly IssueSummary[]) => issues.map((issue) => issue.key).sort();

    await page.goto(`/projects/${ACME}/issues?f.queue=Not+queued`);
    await expect
      .poll(async () => (await rowKeys(table)).sort())
      .toEqual(
        keysOf(
          open.filter(
            (issue) =>
              cards.get(issue.key)?.card.waiting_place == null &&
              cards.get(issue.key)?.column !== "queue"
          )
        )
      );

    await page.goto(`/projects/${ACME}/issues?status=in_progress`);
    const inProgress = (await listIssuesWhere({ project: ACME, status: ["in_progress"] })).filter(
      (issue) => issue.status === "in_progress"
    );
    expect(keysOf(inProgress)).toEqual(expect.arrayContaining([carded.key, bare.key]));
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(keysOf(inProgress));
    const rows = await readRows(table);
    expect([rows[carded.key]?.Status?.text, rows[carded.key]?.["Legion phase"]?.text]).toEqual([
      "In progress",
      "Implementing",
    ]);
    expect([rows[bare.key]?.Status?.text, rows[bare.key]?.["Legion phase"]?.text]).toEqual([
      "In progress",
      "",
    ]);

    await page.goto(`/projects/${ACME}/issues?f.legion_phase=implementing`);
    await expect
      .poll(async () => (await rowKeys(table)).sort())
      .toEqual(keysOf(open.filter((issue) => cards.get(issue.key)?.column === "implementing")));
    await page.goto(`/projects/${ACME}/issues?f.legion_phase=none`);
    await expect
      .poll(async () => (await rowKeys(table)).sort())
      .toEqual(keysOf(open.filter((issue) => !cards.has(issue.key))));
    const phases = await readFilter(page, "Legion phase");
    expect(phases.options.map(([words]) => words)).toEqual([
      ...BOARD_COLUMNS.map(([, label]) => label),
      "None",
    ]);

    // A project with no Legion setting: Closed and Queue are offered, and no board-fed column.
    await createProject({ key: "PLAIN", name: "Plain" });
    await createIssue({ project: "PLAIN", title: "An issue of a project with no daemon" });
    await page.goto("/projects/PLAIN/issues");
    await expect(page.locator("[data-issue-row]").first()).toBeVisible();
    const names = await columnsMenuNames(page);
    expect(names).toEqual(expect.arrayContaining(["Closed", "Queue"]));
    for (const fed of BOARD_FED_HEADERS) expect(names).not.toContain(fed);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): three Start next actions, each waited on through the API.
test("Start next moves the Todo issues labelled legion apart from the Place, refuses a selection with none of them, and leaves the rest of a mixed selection where it is", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 1200, width: 1440 });
    const table = issuesTable(page);
    // The Legion queue holds ACME-4, ACME-3 and ACME-5; none is labelled legion.
    await page.goto(`/projects/${ACME}/issues?f.queue=Queued`);
    await expect.poll(() => rowKeys(table)).toEqual(["ACME-4", "ACME-3", "ACME-5"]);

    const first = await createIssue({ project: ACME, title: "Labelled for Legion first" });
    const second = await createIssue({ project: ACME, title: "Labelled for Legion second" });
    await patchIssue(first.key, { labels: ["legion"], status: "todo" });
    await patchIssue(second.key, { labels: ["legion"], status: "todo" });
    const rankOf = async (key: string) => (await getIssue(key)).rank;
    const fixed = async () => Promise.all(["ACME-3", "ACME-4", "ACME-5"].map((key) => rankOf(key)));
    const fixedBefore = await fixed();
    expect((await rankOf(first.key)) < (await rankOf(second.key)), "A is ranked before B").toBe(
      true
    );

    const startNext = page
      .getByRole("toolbar")
      .getByRole("button", { exact: true, name: "Start next" });
    // B alone: written before A.
    await page.goto(`/projects/${ACME}/issues`);
    await page.getByRole("checkbox", { exact: true, name: `Select ${second.key}` }).check();
    await startNext.click();
    await expect
      .poll(async () => (await rankOf(second.key)) < (await rankOf(first.key)))
      .toBe(true);
    expect(await fixed()).toEqual(fixedBefore);

    // ACME-4 alone, from the Legion queue: refused, naming the set, and nothing written.
    const firstRank = await rankOf(first.key);
    const secondRank = await rankOf(second.key);
    await page.goto(`/projects/${ACME}/issues?f.queue=Queued`);
    await page.getByRole("checkbox", { exact: true, name: "Select ACME-4" }).check();
    await startNext.click();
    // The region also holds the board header's alert whenever ACME's last daemon read failed,
    // which the minute's sampler read of the fake daemon, answered 404, can leave between rows:
    // the refusal is the alert that names Start next.
    await expect(
      page
        .getByRole("region", { exact: true, name: "Project issues" })
        .getByRole("alert")
        .filter({ hasText: "Start next" })
    ).toHaveText("Start next moves only Todo issues labelled legion.");
    expect(await fixed()).toEqual(fixedBefore);
    expect([await rankOf(first.key), await rankOf(second.key)]).toEqual([firstRank, secondRank]);

    // ACME-4 and A: the confirm names one selected issue outside the set; A alone moves.
    await page.goto(`/projects/${ACME}/issues`);
    await page.getByRole("checkbox", { exact: true, name: "Select ACME-4" }).check();
    await page.getByRole("checkbox", { exact: true, name: `Select ${first.key}` }).check();
    await startNext.click();
    const confirm = page.getByRole("dialog", { name: "Start these next?" });
    await expect(confirm).toContainText(
      "1 selected issue is not a Todo issue labelled legion. It stays where it is."
    );
    await confirm.getByRole("button", { exact: true, name: "Start next" }).click();
    await expect
      .poll(async () => (await rankOf(first.key)) < (await rankOf(second.key)))
      .toBe(true);
    expect(await fixed()).toEqual(fixedBefore);
    expect(await rankOf(second.key)).toBe(secondRank);
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** One issue of k19's corpus: its title, status, parent, the slot it holds, the phases the daemon
 *  recorded of it (oldest first, each with its age), the roles in a turn, and its role event. */
interface Seeded {
  readonly title: string;
  readonly status: "todo" | "in_progress";
  readonly parent?: string;
  readonly slot?: number;
  readonly phases?: readonly (readonly [string, string])[];
  readonly working?: string;
  readonly role?: readonly [string, string];
}

/** k19's open issues with Legion work and cards, keyed by what each stands for: the four lengths
 *  of a wait and of a phase (Q*, the daemon's line), the four lengths of work (W*), the review
 *  rounds (R*), a child of the root W3, a Queue card a blocker holds, a started root that waits
 *  for capacity (S1), and an issue with no Legion work. */
const CORPUS: Readonly<Record<string, Seeded>> = {
  W1: {
    title: "Work under an hour",
    status: "in_progress",
    slot: 2,
    phases: [["implementing", "30 minutes"]],
    working: "implementer",
    role: ["implementer", "30 minutes"],
  },
  W2: {
    title: "Work of three hours",
    status: "in_progress",
    slot: 3,
    phases: [["testing", "3 hours"]],
    working: "tester",
    role: ["tester", "3 hours"],
  },
  W3: {
    title: "Work of three days",
    status: "in_progress",
    slot: 4,
    phases: [["implementing", "3 days"]],
    role: ["implementer", "3 days"],
  },
  W4: {
    title: "Work of ten days",
    status: "in_progress",
    slot: 5,
    phases: [["implementing", "10 days"]],
    role: ["implementer", "10 days"],
  },
  R1: {
    title: "One review round",
    status: "in_progress",
    slot: 6,
    phases: [
      ["implementing", "6 hours"],
      ["reviewing", "5 hours"],
    ],
    role: ["reviewer", "6 hours"],
  },
  R2: {
    title: "Two review rounds",
    status: "in_progress",
    slot: 7,
    phases: [
      ["implementing", "9 hours"],
      ["reviewing", "8 hours"],
      ["implementing", "7 hours"],
      ["reviewing", "6 hours"],
    ],
    role: ["reviewer", "9 hours"],
  },
  R4: {
    title: "Four review rounds",
    status: "in_progress",
    slot: 8,
    phases: [
      ["implementing", "13 hours"],
      ["reviewing", "12 hours"],
      ["implementing", "11 hours"],
      ["reviewing", "10 hours"],
      ["implementing", "9 hours"],
      ["reviewing", "8 hours"],
      ["implementing", "7 hours"],
      ["reviewing", "6 hours"],
    ],
    role: ["reviewer", "13 hours"],
  },
  C1: {
    title: "A child of the three-day root",
    status: "in_progress",
    parent: "W3",
    phases: [["implementing", "2 hours"]],
    role: ["implementer", "2 hours"],
  },
  Q1: {
    title: "Waiting for half an hour",
    status: "todo",
    phases: [["admitted", "30 minutes"]],
    role: ["architect", "30 minutes"],
  },
  Q2: {
    title: "Waiting for three hours",
    status: "todo",
    phases: [["admitted", "3 hours"]],
    role: ["architect", "3 hours"],
  },
  Q3: {
    title: "Waiting for three days",
    status: "todo",
    phases: [["admitted", "3 days"]],
    role: ["architect", "3 days"],
  },
  Q4: {
    title: "Waiting for ten days",
    status: "todo",
    phases: [["admitted", "10 days"]],
    role: ["architect", "10 days"],
  },
  QB: {
    title: "Held by the hour of work",
    status: "todo",
    phases: [["admitted", "2 hours"]],
    role: ["architect", "2 hours"],
  },
  S1: {
    title: "Started and waiting for capacity",
    status: "in_progress",
    phases: [["implementing", "5 hours"]],
    role: ["implementer", "5 hours"],
  },
  N1: { title: "No Legion work at all", status: "todo" },
};

/** Seeds k19's corpus into ACME and answers each issue's key by its name in `CORPUS`. W2 holds an
 *  open ask; the others hold none. */
async function seedFilterCorpus(): Promise<Record<string, string>> {
  const keys: Record<string, string> = {};
  for (const [name, seeded] of Object.entries(CORPUS)) {
    const parent = seeded.parent === undefined ? undefined : keys[seeded.parent];
    const issue = await createIssue({
      project: ACME,
      title: seeded.title,
      ...(parent === undefined ? {} : { parent }),
    });
    await patchIssue(issue.key, { status: seeded.status });
    keys[name] = issue.key;
  }
  const key = (name: string) => keys[name] ?? name;
  const observations: Observation[] = [];
  for (const [name, seeded] of Object.entries(CORPUS)) {
    for (const [phase, ago] of seeded.phases ?? [])
      observations.push({ issue: key(name), field: "phase", value: JSON.stringify(phase), ago });
    if (seeded.slot !== undefined)
      observations.push({
        issue: key(name),
        field: "slot",
        value: String(seeded.slot),
        ago: seeded.phases?.[0]?.[1] ?? "1 hour",
      });
    if (seeded.working !== undefined)
      observations.push({
        issue: key(name),
        field: `claim.${seeded.working}`,
        value: '"working"',
        ago: "10 minutes",
      });
  }
  const slotted = Object.entries(CORPUS).flatMap(([name, seeded]) =>
    seeded.slot === undefined ? [] : [key(name)]
  );
  observations.push({
    issue: "",
    field: "admission",
    value: admission({
      cap: 12,
      active: slotted,
      waiting: ["Q1", "Q2", "Q3", "Q4", "S1"].map(key),
      trees: ["W1", "W2", "W3", "W4", "R1", "R2", "R4"].map(key),
      blocked: [{ issue: key("QB"), blockedBy: [key("W1")] }],
    }),
    ago: "1 minute",
  });
  await pushLegionObservations(observations);
  await writeEventsForFacts(
    ...Object.entries(CORPUS).flatMap(([name, seeded]) =>
      seeded.role === undefined ? [] : [roleEventStatement(key(name), ...seeded.role)]
    )
  );
  await createAsk(
    key("W2"),
    { question: "Which gadget should the tests cover?" },
    { actor: { id: "views-asker", kind: "session" }, as: "agent" }
  );
  return keys;
}

/** The keys the facts consumer must write a row of before a k19 row reads the filters. */
const factsKeys = (keys: Record<string, string>): string[] => [
  ...Object.entries(CORPUS).flatMap(([name, seeded]) =>
    seeded.role === undefined ? [] : [keys[name] ?? name]
  ),
];

/** What one open issue files under in each column's filter, by the column's header and the words
 *  of each pick, computed from the listing and the board route. */
function picksOf(issue: IssueSummary, board: Board, nowMs: number): Record<string, string[]> {
  const placed = cardsOf(board).get(issue.key);
  const card = placed?.card;
  const facts = boardFacts(board, issue.key);
  const sinceMs = (since: string | null | undefined) =>
    since == null ? undefined : nowMs - Date.parse(since);
  const durationWords = (ms: number | undefined) =>
    DURATIONS.find(([id]) => id === durationPick(ms))?.[1] ?? "None";
  const age = (at: string) => {
    const days = (nowMs - Date.parse(at)) / 86_400_000;
    return days < 1 ? "Today" : days < 7 ? "This week" : days < 30 ? "This month" : "Older";
  };
  const roles = card?.roles ?? [];
  const work = measuredWork(facts);
  const rounds = facts?.review_rounds;
  const author = issue.created_by?.kind === "user" ? issue.created_by.id : "Unknown";
  return {
    Priority: [issue.priority === null ? "No priority" : `P${issue.priority}`],
    Queue: [
      card?.waiting_place != null || placed?.column === "queue"
        ? "In Legion queue"
        : "Not in Legion queue",
    ],
    Status: [STATUS_WORDS[issue.status] ?? issue.status],
    "Legion phase": [placed?.label ?? "None"],
    Assignee: [issue.assignee ?? "Unassigned"],
    "Created by":
      issue.created_by?.kind === "user" && issue.created_by.id === "alice"
        ? [author, "Me"]
        : [author],
    Labels: issue.labels ?? [],
    "Claimed by": issue.claim === null ? ["Nobody", "Nobody or lapsed"] : [],
    "Open asks": [issue.open_asks > 0 ? "With open asks" : "No open asks"],
    Created: [age(issue.created_at)],
    Updated: [age(issue.updated_at)],
    Workers:
      roles.length === 0
        ? ["None"]
        : roles.map((role) => `${role.slice(0, 1).toUpperCase()}${role.slice(1)}`),
    "Worker state": [
      card?.slot_use == null || card.slot_use === ""
        ? "None"
        : wordOf(WORKER_STATE_WORDS, card.slot_use),
    ],
    "Time waiting": [durationWords(sinceMs(card?.waiting?.since))],
    "Why it waits": [
      card?.waiting == null || card.waiting.reason === ""
        ? "None"
        : wordOf(WAITING_WORDS, card.waiting.reason),
    ],
    "Time in Legion phase": [durationWords(sinceMs(card?.phase_since))],
    "Work time": [durationWords(work === undefined ? undefined : work * 3_600_000)],
    "Review rounds": [rounds === undefined ? "None" : rounds >= 3 ? "3 or more" : String(rounds)],
    "Legion tree": [facts === undefined ? "None" : facts.tree_depth === 0 ? "Root" : "Child"],
    "Time per phase": phasesWithHours(facts).map(
      (phase) =>
        BOARD_COLUMNS.find(([id]) => id === phase)?.[1] ?? (phase === "other" ? "Other" : phase)
    ),
  };
}

/** The options each filter must offer, in their order where the column has one, with their
 *  counts, from the issues' picks. `always` are the picks a column offers whatever the rows hold,
 *  in its order; a column with no order is compared by its words. */
const FILTER_ORDERS: Readonly<Record<string, { order?: readonly string[]; always?: boolean }>> = {
  Priority: { order: ["P0", "P1", "P2", "P3", "No priority"] },
  Queue: { order: ["In Legion queue", "Not in Legion queue"] },
  Status: {
    order: Object.values(STATUS_WORDS),
    always: true,
  },
  "Legion phase": { order: [...BOARD_COLUMNS.map(([, label]) => label), "None"], always: true },
  Assignee: {},
  "Created by": {},
  Labels: {},
  "Claimed by": {},
  "Open asks": { order: ["With open asks", "No open asks"], always: true },
  Created: { order: ["Today", "This week", "This month", "Older"] },
  Updated: { order: ["Today", "This week", "This month", "Older"] },
  Workers: {
    order: [
      ...ROLE_ORDER.map((role) => `${role.slice(0, 1).toUpperCase()}${role.slice(1)}`),
      "None",
    ],
  },
  "Worker state": { order: [...WORKER_STATE_WORDS.map(([, words]) => words), "None"] },
  "Time waiting": { order: [...DURATIONS.map(([, words]) => words), "None"], always: true },
  "Why it waits": { order: [...WAITING_WORDS.map(([, words]) => words), "None"] },
  "Time in Legion phase": {
    order: [...DURATIONS.map(([, words]) => words), "None"],
    always: true,
  },
  "Work time": { order: [...DURATIONS.map(([, words]) => words), "None"], always: true },
  "Review rounds": { order: ["0", "1", "2", "3 or more", "None"], always: true },
  "Legion tree": { order: ["Root", "Child", "None"], always: true },
  "Time per phase": {
    order: WORK_PHASES.map(
      (phase) =>
        BOARD_COLUMNS.find(([id]) => id === phase)?.[1] ?? (phase === "other" ? "Other" : phase)
    ),
  },
};

/** The options a filter must offer, with counts, from every open row's picks. */
function expectedOptions(
  name: string,
  issues: readonly IssueSummary[],
  board: Board,
  nowMs: number
): (readonly [string, number])[] {
  const counts = new Map<string, number>();
  for (const issue of issues)
    for (const pick of new Set(picksOf(issue, board, nowMs)[name] ?? []))
      counts.set(pick, (counts.get(pick) ?? 0) + 1);
  const { always = false, order } = FILTER_ORDERS[name] ?? {};
  const offered = new Set([...counts.keys(), ...(always && order !== undefined ? order : [])]);
  // Open reads no closed issue, so Status offers Done as not loaded rather than count it (-1).
  const options = [...offered].map(
    (words) =>
      [words, name === "Status" && words === "Done" ? -1 : (counts.get(words) ?? 0)] as const
  );
  return order === undefined
    ? options.sort(([a], [b]) => a.localeCompare(b))
    : options.sort(([a], [b]) => order.indexOf(a) - order.indexOf(b));
}

/** The filters k19 opens, in its order, with the kind of each. */
const K19_FILTERS: readonly string[] = [
  "Priority",
  "Queue",
  "Key",
  "Title",
  "Status",
  "Legion phase",
  "Assignee",
  "Created by",
  "Labels",
  "Claimed by",
  "Open asks",
  "Created",
  "Updated",
  "Closed",
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

// SLOW(>=2s): seventeen issues seeded with their phase histories and the facts consumer's rows of
// fourteen of them, up to 75 s, then every column's filter opened and read.
test("every column but Select opens a filter, each offering its picks in order with the counts the routes give", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const keys = await seedFilterCorpus();
    await waitForIssueFacts(factsKeys(keys));
    const page = await context.newPage();
    await page.setViewportSize({ height: 2000, width: 1920 });
    await page.goto(`/projects/${ACME}/issues?cols=${EVERY_COL}`);
    const table = issuesTable(page);
    await expect(table.locator(`tr[data-issue-row="${keys.N1}"]`)).toBeVisible();
    // Every filter opens; the options are read as each opens, and compared once all have.
    const read = new Map<string, FilterRead>();
    for (const name of K19_FILTERS) read.set(name, await readFilter(page, name));
    for (const name of ["Key", "Title", "Closed"])
      expect(read.get(name)?.opens, `${name} opens a list`).toBe("dialog");

    const open = await listIssuesWhere({ project: ACME, open: true });
    await expect
      .poll(
        async () => {
          const board = await getLegionBoard(ACME);
          const nowMs = Date.now();
          const wrong: string[] = [];
          for (const name of K19_FILTERS) {
            if (["Key", "Title", "Closed"].includes(name)) continue;
            const drawn = read.get(name)?.options ?? [];
            const wanted = expectedOptions(name, open, board, nowMs);
            const ordered = FILTER_ORDERS[name]?.order !== undefined;
            const shown = ordered ? drawn : [...drawn].sort(([a], [b]) => a.localeCompare(b));
            if (JSON.stringify(shown) !== JSON.stringify(wanted))
              wrong.push(
                `${name}: drawn ${JSON.stringify(shown)}, the routes give ${JSON.stringify(wanted)}`
              );
          }
          // A filter read a second later counts the same rows: read the drawn filters again
          // where a length of time may have crossed a bound meanwhile.
          if (wrong.length > 0)
            for (const name of K19_FILTERS) read.set(name, await readFilter(page, name));
          return wrong;
        },
        { timeout: 30_000 }
      )
      .toEqual([]);

    // The seed reaches every length and every count it names.
    const board = await getLegionBoard(ACME);
    const nowMs = Date.now();
    const filed = (name: string, words: string) =>
      open.filter((issue) => (picksOf(issue, board, nowMs)[name] ?? []).includes(words)).length;
    for (const name of ["Time waiting", "Time in Legion phase", "Work time"])
      for (const [, words] of DURATIONS)
        expect(filed(name, words), `${name}: ${words}`).toBeGreaterThan(0);
    for (const words of ["0", "1", "2", "3 or more"])
      expect(filed("Review rounds", words), `Review rounds: ${words}`).toBeGreaterThan(0);
    for (const words of ["Implementer", "Tester", "None"])
      expect(filed("Workers", words), `Workers: ${words}`).toBeGreaterThan(0);
    for (const words of ["Root", "Child", "None"])
      expect(filed("Legion tree", words), `Legion tree: ${words}`).toBeGreaterThan(0);
    for (const words of ["With open asks", "No open asks"])
      expect(filed("Open asks", words), `Open asks: ${words}`).toBeGreaterThan(0);

    // The blocked card names its blocker; the started root in the line waits for capacity.
    const rows = await readRows(table);
    expect(rows[keys.QB ?? ""]?.["Why it waits"]?.text).toBe(`A blocker${keys.W1}`);
    expect(rows[keys.QB ?? ""]?.["Why it waits"]?.links).toEqual([keys.W1]);
    expect(rows[keys.S1 ?? ""]?.["Why it waits"]?.text).toBe("Capacity to go on");
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** The picks k19 makes: the filter, the option's words, the address value it writes, and which
 *  open rows the pick keeps, by their picks. */
const K19_PICKS: readonly (readonly [string, string, string, string])[] = [
  ["Open asks", "With open asks", "f.asks", "any"],
  ["Workers", "Implementer", "f.roles", "implementer"],
  ["Worker state", "Working", "f.activity", "working"],
  ["Time waiting", "1 to 7 days", "f.waited", "1d-7d"],
  ["Why it waits", "Ready to start", "f.waiting_reason", "ready"],
  ["Time in Legion phase", "Under 1 hour", "f.time_in_phase", "under-1h"],
  ["Work time", "Over 7 days", "f.work_hours", "over-7d"],
  ["Review rounds", "3 or more", "f.review_rounds", "3-or-more"],
  ["Legion tree", "Root", "f.legion", "root"],
  ["Legion phase", "None", "f.legion_phase", "none"],
  ["Worker state", "None", "f.activity", "none"],
  ["Why it waits", "None", "f.waiting_reason", "none"],
  ["Workers", "None", "f.roles", "none"],
];

// SLOW(>=2s): the same corpus and the facts consumer's rows, up to 75 s, then thirteen picks, each
// read before and after a reload.
test("each pick of a Legion filter writes its own value to the address, lists the rows the routes give, and holds through a reload", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const keys = await seedFilterCorpus();
    await waitForIssueFacts(factsKeys(keys));
    const open = await listIssuesWhere({ project: ACME, open: true });
    const page = await context.newPage();
    await page.setViewportSize({ height: 2000, width: 1920 });
    const table = issuesTable(page);
    const base = `/projects/${ACME}/issues?cols=${EVERY_COL}`;
    for (const [name, words, param, value] of K19_PICKS) {
      await page.goto(base);
      await expect(table.locator(`tr[data-issue-row="${keys.N1}"]`)).toBeVisible();
      await page.getByRole("button", { exact: true, name: `Filter ${name}` }).click();
      await page
        .getByRole("listbox", { name: `${name} options` })
        .getByRole("option", { name: new RegExp(`^${words}, \\d+ issues?$`) })
        .click();
      await page.keyboard.press("Escape");
      await expect.poll(() => new URL(page.url()).searchParams.getAll(param)).toEqual([value]);
      const wanted = async () => {
        const board = await getLegionBoard(ACME);
        const nowMs = Date.now();
        return open
          .filter((issue) => (picksOf(issue, board, nowMs)[name] ?? []).includes(words))
          .map((issue) => issue.key)
          .sort();
      };
      await expect
        .poll(
          async () => {
            const [drawn, expected] = [(await rowKeys(table)).sort(), await wanted()];
            return JSON.stringify(drawn) === JSON.stringify(expected)
              ? "same"
              : `${drawn} against ${expected}`;
          },
          { message: `${name}: ${words}` }
        )
        .toBe("same");
      const listed = (await rowKeys(table)).sort();
      await page.reload();
      await expect(
        page.getByRole("button", { exact: true, name: `Filter ${name}: 1 picked` })
      ).toBeVisible();
      await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(listed);
      await page.getByRole("button", { exact: true, name: `Filter ${name}: 1 picked` }).click();
      await expect(
        page
          .getByRole("listbox", { name: `${name} options` })
          .getByRole("option", { name: new RegExp(`^${words}, \\d+ issues?$`) })
      ).toHaveAttribute("aria-selected", "true");
      await page.keyboard.press("Escape");
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R14 and D12, found by INFRA-3221's tester, with the architect's decision: a second tab does not
// end the first tab's bulk action. While tab one's Start next holds the move's Web Lock, tab two
// offers no Finish and no Dismiss; once tab one closes, its lock goes, and tab two offers both, as
// D12 offers a record a tab left behind, and its Finish moves every selected issue.
// SLOW(>=2s): four issues seeded and moved by Start next in one tab while a second tab is open.
test("a second tab offers no Finish or Dismiss while the first tab's Start next holds its lock, and both once that tab closes", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const keys: string[] = [];
    for (const name of ["first", "second", "third", "fourth"]) {
      const made = await createIssue({ project: ACME, title: `Labelled for Legion ${name}` });
      await patchIssue(made.key, { labels: ["legion"], status: "todo" });
      keys.push(made.key);
    }
    const [stays, ...moved] = keys;
    if (stays === undefined) throw new Error("the seed has no issue");
    // Tab one's writes are held, so its move is live, and its lock held, until it closes.
    const { promise: gate, resolve: release } = Promise.withResolvers<void>();
    const { promise: writing, resolve: wrote } = Promise.withResolvers<void>();
    const one = await context.newPage();
    await one.route(/\/api\/v1\/issues\/[^/?]+$/, async (route) => {
      if (route.request().method() === "PATCH") {
        wrote();
        await gate;
      }
      await route.continue().catch(() => undefined);
    });
    await one.goto(`/projects/${ACME}/issues`);
    await expect(one.locator("[data-issue-row]").first()).toBeVisible();
    for (const key of moved)
      await one.getByRole("checkbox", { exact: true, name: `Select ${key}` }).check();
    // Every selected row is a Todo issue labelled legion and none is off the page, so Start next
    // asks no confirm and writes at once.
    await one.getByRole("toolbar").getByRole("button", { exact: true, name: "Start next" }).click();
    await writing;

    const two = await context.newPage();
    await two.goto(`/projects/${ACME}/issues`);
    await expect(two.locator("[data-issue-row]").first()).toBeVisible();
    // Tab two has its answer from the lock once it has asked for the lock tab one holds: its
    // request then waits in the browser's lock manager, which the page shares with tab one. The
    // lock is named for the listing (`actionLockName` in issues-table-recovery.ts).
    await expect
      .poll(
        () =>
          two.evaluate(async () => {
            const locks = await navigator.locks.query();
            const named = (lock: LockInfo) =>
              lock.name?.startsWith("dispatch-table-action:") === true;
            return (locks.held ?? []).some(named) && (locks.pending ?? []).some(named);
          }),
        { message: "tab one holds the move's lock and tab two waits for it" }
      )
      .toBe(true);
    const finish = two.getByRole("button", { exact: true, name: "Finish" });
    const dismiss = two.getByRole("button", { exact: true, name: "Dismiss" });
    await expect(finish).toHaveCount(0);
    await expect(dismiss).toHaveCount(0);

    // Tab one closes with its move unfinished: the lock goes, and tab two offers both.
    await one.close();
    release();
    await expect(finish).toBeVisible();
    await expect(dismiss).toBeVisible();
    await finish.click();
    const rankOf = async (key: string) => (await getIssue(key)).rank;
    await expect
      .poll(
        async () => {
          const bottom = await rankOf(stays);
          const ranks = await Promise.all(moved.map((key) => rankOf(key)));
          return ranks.every((rank) => rank < bottom);
        },
        { message: "tab two's Finish moves every selected issue ahead of the one left out" }
      )
      .toBe(true);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// The Design's columns, found by INFRA-3221's tester: every column sorts and a shift-click adds a
// second sort, and each view's sort ends with the listing's own order, so the rows that tie on
// the first sort - the rows with no card, by Legion phase first or last - follow the second.
// SLOW(>=2s): ACME seeded with four issues that have no card, and Open sorted four ways.
test("rows with no value in a Legion column's sort follow the second sort", async ({ browser }) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const made: { key: string; priority: number }[] = [];
    for (const priority of [2, 0, 3, 1]) {
      const issue = await createIssue({ project: ACME, title: `No card at P${priority}` });
      await patchIssue(issue.key, { status: "todo", priority });
      made.push({ key: issue.key, priority });
    }
    const byPriority = [...made].sort((a, b) => a.priority - b.priority).map(({ key }) => key);
    const page = await context.newPage();
    const table = issuesTable(page);
    for (const [sort, order] of [
      ["legion_phase,priority", byPriority],
      ["-legion_phase,priority", byPriority],
      ["legion_phase,-priority", [...byPriority].reverse()],
      ["waited,priority", byPriority],
    ] as const) {
      await page.goto(`/projects/${ACME}/issues?sort=${sort}&cols=key,priority,legion_phase,title`);
      await expect
        .poll(async () => (await rowKeys(table)).filter((key) => order.includes(key)), {
          message: `sort=${sort}`,
        })
        .toEqual(order);
    }
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R1 and the Design's columns, found by INFRA-3221's tester: the group heading's next in Legion
// queue names the Place, the Queue column's number, and not Start next's own count of the Todo
// issues labelled legion, of which ACME has none.
// SLOW(>=2s): ACME seeded, and Open grouped by Status.
test("the Todo group's heading names the first Place in the Legion queue", async ({ browser }) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const cards = cardsOf(await getLegionBoard(ACME));
    const todo = await listIssuesWhere({ project: ACME, status: ["todo"] });
    const places = todo.flatMap((issue) => {
      const place = cards.get(issue.key)?.card.waiting_place ?? null;
      return place === null ? [] : [place];
    });
    expect(places.length, "ACME's Todo issues hold a Place").toBeGreaterThan(0);
    const page = await context.newPage();
    await page.goto(`/projects/${ACME}/issues?group=status`);
    const heading = page.getByRole("button", { name: /^Collapse Todo,/ });
    await expect(heading).toContainText(`next in Legion queue #${Math.min(...places)}`);
  } finally {
    await context.close();
    await daemon.close();
  }
});
