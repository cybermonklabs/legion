/** Done's reads are the server's own pages and windows of the list route and the items route as
 *  the facts consumer and the eval refresher leave them, read request by request with the board
 *  over the live socket, and the list route's sort is read straight from the server. */
// Done and the reads behind it (dispatch://INFRA-3221, Verification k6 and k9): what a fresh
// load of Done reads, with and without a saved view, a saved view of done issues and its counts,
// Done's pages and its Closed window, the reads of every view that lists done issues, the items
// read of `legion-work-time` held, failed and answering 404, the addresses of today's list, the
// list route's sort by `closed_at`, and every parameter the Evals tab's links write.
//
// ACME is seeded with `seedLegionFixture`, its daemon named at a `startFakeLegionDaemon`, and
// with `seedLegionMetricsCorpus`, and its done issues written by SQL with the closes each row
// reads. A row that reads Legion work writes a role event for each issue and waits for the facts
// consumer's row (`waitForIssueFacts`, up to 75 s), one such wait in a row. Each expected row,
// order and count is computed in the row from the seed through the API (`GET /api/v1/issues`,
// the board route and the items route), never with the web code's filters.
import { setTimeout as sleep } from "node:timers/promises";

import type { LegionBoardResponse as Board, EvalItemRow, IssueSummary } from "../web/src/api/types";
import { CONTROL } from "../web/src/components/control-names";
import {
  baseUrl,
  claimIssue,
  createAsk,
  createIssue,
  getLegionBoard,
  getLegionWorkTimeItems,
  getMyState,
  legionWorkTimeEval,
  listIssuesWhere,
  patchIssue,
  userHeaders,
  waitForLegionWorkTimeEval,
} from "./api";
import { control, controlRow } from "./filters";
import {
  ACME,
  type BoardFrame,
  boardFacts,
  cardsOf,
  type Facts,
  hoursWords,
  issuesTable,
  itemFacts,
  listingReads,
  measuredWork,
  type Recorded,
  recordRequests,
  routeLive,
  rowKeys,
  saveView,
  seedAcme,
  tabCount,
  tabName,
  tabNames,
  workWords,
} from "./issues-views-shared";
import { waitForIssueFacts, writeRoleEvent } from "./legion-facts-seed";
import { SEED_ACTOR, settledCounters } from "./legion-seed";
import { sql } from "./psql";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

const DAY = 86_400_000;
const HOUR = 3_600_000;
const LOS_ANGELES = { timezoneId: "America/Los_Angeles" };

/** The first moment of the UTC day `ms` falls in. */
const midnight = (ms: number) => Math.floor(ms / DAY) * DAY;
/** A UTC day as a date picker and the address write it. */
const isoDay = (ms: number) => new Date(ms).toISOString().slice(0, 10);
const iso = (ms: number) => new Date(ms).toISOString();
const keysOf = (issues: readonly IssueSummary[]) => issues.map((issue) => issue.key);
const closeOf = (issue: IssueSummary) => Date.parse(issue.closed_at ?? "");

/** A request's query as sorted `key=value` pairs joined with `&`. */
const queryOf = (url: URL) =>
  [...url.searchParams.entries()]
    .map(([key, value]) => `${key}=${value}`)
    .sort()
    .join("&");

/** The items route of `legion-work-time`. */
const ITEMS_PATH = `/api/v1/projects/${ACME}/evals/${legionWorkTimeEval}/items`;
const itemsReads = (requests: readonly Recorded[]) =>
  requests.filter((request) => request.url.pathname === ITEMS_PATH);

/** One window of the items route: from a UTC midnight to another, at most 84 days apart. */
interface ItemsWindow {
  readonly from: string;
  readonly to: string;
}

/** The windows of at most 84 days that cover the UTC days from `first` to `last`, both in. */
function windowsOver(first: number, last: number): ItemsWindow[] {
  const windows: ItemsWindow[] = [];
  const end = midnight(last) + DAY;
  for (let from = midnight(first); from < end; from += 84 * DAY)
    windows.push({ from: iso(from), to: iso(Math.min(from + 84 * DAY, end)) });
  return windows;
}

/** The windows over the closes of `rows` that the board's `issues` map does not hold, to `now`
 *  where the read is of every done row. */
function windowsOf(rows: readonly IssueSummary[], board: Board, now?: number): ItemsWindow[] {
  const closes = rows
    .filter((row) => row.closed_at !== null && board.issues?.[row.key] === undefined)
    .map(closeOf);
  if (closes.length === 0) return [];
  return windowsOver(Math.min(...closes), Math.max(...closes, now ?? 0));
}

/** How many reads of at most 200 rows the items of `windows` take, from the route's totals. */
async function itemsReadCount(windows: readonly ItemsWindow[]): Promise<number> {
  let reads = 0;
  for (const window of windows) {
    const answer = await getLegionWorkTimeItems(ACME, { ...window, limit: 1, row_kind: "issue" });
    reads += Math.max(1, Math.ceil(answer.total / 200));
  }
  return reads;
}

/** Where the items requests of a load differ from reads of `windows`: each of an issue rows'
 *  window, at most 200 rows a read. */
function itemsDiffer(requests: readonly Recorded[], windows: readonly ItemsWindow[]): string[] {
  const wanted = new Set(
    windows.map((window) => `${Date.parse(window.from)} ${Date.parse(window.to)}`)
  );
  return itemsReads(requests).flatMap((request) => {
    const query = request.url.searchParams;
    const span = `${Date.parse(query.get("from") ?? "")} ${Date.parse(query.get("to") ?? "")}`;
    return [
      ...(query.get("row_kind") === "issue" ? [] : [`${request.url.search} reads no issue rows`]),
      ...(Number(query.get("limit")) <= 200 ? [] : [`${request.url.search} reads over 200 rows`]),
      ...(wanted.has(span) ? [] : [`${request.url.search} reads a window no close asks for`]),
    ];
  });
}

/** The facts of each of `issues`: the board's for a key its map holds, else its row in the items
 *  route over the UTC day of its close; none for an issue with neither. */
async function factsOf(issues: readonly IssueSummary[]): Promise<Map<string, Facts | undefined>> {
  const board = await getLegionBoard(ACME);
  const facts = new Map<string, Facts | undefined>();
  const days = new Map<number, Map<string, EvalItemRow>>();
  for (const issue of issues) {
    const held = boardFacts(board, issue.key);
    if (held !== undefined) {
      facts.set(issue.key, held);
      continue;
    }
    if (issue.closed_at === null) {
      facts.set(issue.key, undefined);
      continue;
    }
    const day = midnight(closeOf(issue));
    let rows = days.get(day);
    if (rows === undefined) {
      const answer = await getLegionWorkTimeItems(ACME, {
        from: iso(day),
        limit: 200,
        row_kind: "issue",
        to: iso(day + DAY),
      });
      rows = new Map((answer.items ?? []).map((row) => [row.row_key, row] as const));
      days.set(day, rows);
    }
    const row = rows.get(issue.key);
    facts.set(issue.key, row === undefined ? undefined : itemFacts(row));
  }
  return facts;
}

const legionPick = (facts: Facts | undefined) =>
  facts === undefined ? "none" : facts.tree_depth === 0 ? "root" : "child";

/** The done issues of ACME, newest close first, as the list route answers them. */
const doneIssues = () => listIssuesWhere({ project: ACME, sort: "-closed_at", status: ["done"] });

/** k6's done issues on ACME, beside the metrics corpus's two: 60 closed over the last 30 days
 *  and two around the end of the seventh day after D, a UTC day 20 days back: one closed at
 *  12:00Z on D+6 and one at 00:00Z on D+7. One title holds the word `sprocket`. */
interface DoneCorpus {
  readonly d: number;
  readonly atNoon: string;
  readonly atMidnight: string;
  readonly word: string;
}

async function seedDoneCorpus(): Promise<DoneCorpus> {
  const now = Date.now();
  const d = midnight(now) - 20 * DAY;
  const rows: { title: string; closed: number; priority: number }[] = [
    { title: "Closed at noon on the last day", closed: d + 6 * DAY + 12 * HOUR, priority: 1 },
    { title: "Closed at the next midnight", closed: d + 7 * DAY, priority: 2 },
  ];
  for (let n = 1; n <= 60; n++)
    rows.push({
      title: n === 7 ? "Closed the sprocket audit" : `Closed work ${n}`,
      closed: now - n * 11.75 * HOUR,
      priority: n % 4,
    });
  await sql(
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank, created_at, closed_at) VALUES ${rows
      .map(
        (row, index) =>
          `('${ACME}-${index + 8}', '${ACME}', ${index + 8}, '${row.title}', 'done', ${row.priority}, ${SEED_ACTOR}, 'z${String(index + 8).padStart(4, "0")}', '${iso(row.closed - 2 * DAY)}', '${iso(row.closed)}')`
      )
      .join(", ")}`,
    ...settledCounters(ACME)
  );
  return { d, atNoon: `${ACME}-8`, atMidnight: `${ACME}-9`, word: "sprocket" };
}

/** Tall enough that the table draws every row of a page of 25: it draws only the rows in view. */
const TALL = { height: 3000, width: 1440 };

// SLOW(>=2s): 64 done issues seeded, two fresh loads with the board's snapshot held 5 s each,
// two views saved and four views read.
test("a fresh load of Done reads the open count, one page of its rows, the board and the items of their close days and no other listing, with a saved view stored too, and a saved view of done issues counts on its own tab alone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice", LOS_ANGELES);
  try {
    await seedDoneCorpus();
    await waitForLegionWorkTimeEval(ACME);
    const [open, done, board] = await Promise.all([
      listIssuesWhere({ open: true, project: ACME }),
      doneIssues(),
      getLegionBoard(ACME),
    ]);
    const firstPage = done.slice(0, 25);
    const windows = windowsOf(firstPage, board);
    const reads = await itemsReadCount(windows);
    const openCount = `limit=1&open=true&project=${ACME}`;
    const donePage = `limit=25&offset=0&project=${ACME}&sort=-closed_at&status=done`;

    // A new page's first load of Done, its board's snapshot held 5 s.
    const freshLoad = async (tabs: readonly string[], name: string) => {
      const page = await context.newPage();
      const requests = recordRequests(page);
      const frames: BoardFrame[] = [];
      await routeLive(page, {
        first: async () => {
          // The hold this row loads under: the board's first snapshot waits 5 s, so the rows and
          // the counts are read and drawn without it, and the items reads, which wait for the
          // board's map, go out only once it has passed.
          await sleep(5_000);
        },
        seen: (frame, isBoard) => {
          if (isBoard) frames.push(frame);
        },
      });
      await page.setViewportSize(TALL);
      await page.goto(`/projects/${ACME}/issues?view=done&size=25`);
      const table = issuesTable(page);
      await expect.poll(() => rowKeys(table), { timeout: 30_000 }).toEqual(keysOf(firstPage));
      await expect
        .poll(() => itemsReads(requests.list).length, { timeout: 30_000 })
        .toBeGreaterThanOrEqual(reads);
      await expect.poll(() => tabNames(page)).toEqual([...tabs]);
      // An absence window, not a wait for a state: every read the row expects has answered, and
      // a listing or items read the load must not make, which the checks below refuse, would go
      // out in the render that an answer it follows causes, well inside this second.
      await sleep(1_000);
      await requests.settled();
      await test.info().attach(`${name}: every request of the load`, {
        body: JSON.stringify(
          requests.list.map((request) => ({
            bytes: request.bytes,
            method: request.method,
            status: request.status,
            url: `${request.url.pathname}${request.url.search}`,
          })),
          null,
          2
        ),
        contentType: "application/json",
      });
      expect(
        listingReads(requests.list, ACME)
          .map((request) => queryOf(request.url))
          .sort(),
        `${name}: the listings it reads`
      ).toEqual([openCount, donePage].sort());
      expect(
        requests.list.some(
          (request) => request.url.pathname === `/api/v1/projects/${ACME}/legion/daemon`
        ),
        `${name}: it does not read the daemon setting`
      ).toBe(true);
      expect(itemsReads(requests.list).length, `${name}: the items reads`).toBe(reads);
      expect(itemsDiffer(requests.list, windows), name).toEqual([]);
      const snapshot = frames.find((frame) => frame.type === "snapshot");
      expect(
        (snapshot?.body?.columns ?? [])
          .flatMap((column) => (column.cards ?? []).map((card) => card.key))
          .sort(),
        `${name}: the board's cards`
      ).toEqual([...cardsOf(board).keys()].sort());
      return page;
    };

    const first = await freshLoad(
      [tabName("Open", open.length), tabName("Done", done.length)],
      "the first load"
    );

    // A view saved from Open with status=triage, then a second first load of Done.
    const triage = await listIssuesWhere({ project: ACME, status: ["triage"] });
    await first.goto(`/projects/${ACME}/issues?status=triage`);
    await expect
      .poll(async () => (await rowKeys(issuesTable(first))).sort())
      .toEqual(keysOf(triage).sort());
    await saveView(first, "Triage");
    const second = await freshLoad(
      [tabName("Open", open.length), tabName("Done", done.length), "Triage"],
      "the load with a saved view"
    );

    // A view saved from Open with status=done counts on its own tab alone.
    await second.goto(`/projects/${ACME}/issues?status=done`);
    await expect(issuesTable(second).locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await saveView(second, "Closed");
    await second.goto(`/projects/${ACME}/issues?view=open`);
    await expect.poll(() => tabCount(second, "Open")).toBe(open.length);
    expect(await tabNames(second)).toContain("Closed");
    await second.goto(`/projects/${ACME}/issues?view=done`);
    await expect.poll(() => tabCount(second, "Done")).toBe(done.length);
    expect(await tabNames(second)).toContain("Closed");
    await second.getByRole("tab", { name: /^Closed/ }).click();
    await expect.poll(() => tabCount(second, "Closed")).toBe(done.length);
    // The table lists that many rows across its pages.
    let listed = 0;
    for (let turn = 0; turn < 10; turn++) {
      await expect(issuesTable(second).locator("tbody tr[data-issue-row]").first()).toBeVisible();
      const keys = await rowKeys(issuesTable(second));
      listed += keys.length;
      const next = second.getByRole("button", { name: "Next ›" });
      // The pager draws Next from the rows it draws, and the row wait above drew this page.
      if ((await next.getAttribute("aria-disabled")) === "true") break;
      await next.click();
      await expect.poll(async () => (await rowKeys(issuesTable(second)))[0]).not.toBe(keys[0]);
    }
    expect(listed, "the rows Closed lists across its pages").toBe(done.length);
    await second.getByRole("tab", { name: /^Open/ }).click();
    await expect.poll(() => tabCount(second, "Open")).toBe(open.length);
    expect(await tabNames(second)).toContain("Closed");

    // Done at the All rows page size reads status=done with no limit.
    const all = await context.newPage();
    const allRequests = recordRequests(all);
    await all.setViewportSize(TALL);
    await all.goto(`/projects/${ACME}/issues?view=done&size=${Number.MAX_SAFE_INTEGER}`);
    await expect(issuesTable(all).locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await expect.poll(() => tabCount(all, "Done")).toBe(done.length);
    await allRequests.settled();
    expect(
      listingReads(allRequests.list, ACME)
        .map((request) => queryOf(request.url))
        .sort()
    ).toEqual([openCount, `project=${ACME}&status=done`].sort());
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): 64 done issues seeded, then fourteen loads each read request by request, two of
// them with the board's snapshot held 5 s.
test("Done's next page, a Closed window picked in its panel and a 28-day window each read a page of their own, and the search, a sort, the Boards and the views that list done issues read status=done with no limit and never the full listing", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice", LOS_ANGELES);
  try {
    const corpus = await seedDoneCorpus();
    const labelled = await createIssue({ project: ACME, title: "Label the widget crates" });
    await patchIssue(labelled.key, { labels: ["widgets"], status: "todo" });
    await waitForLegionWorkTimeEval(ACME);
    const open = await listIssuesWhere({ open: true, project: ACME });
    const done = await doneIssues();
    const board = await getLegionBoard(ACME);
    const full = await fetch(new URL(`/api/v1/issues?project=${ACME}&status=done`, baseUrl), {
      headers: await userHeaders(),
    });
    const fullBytes = (await full.arrayBuffer()).byteLength;

    const page = await context.newPage();
    const requests = recordRequests(page);
    let holdSnapshot = false;
    let released = true;
    await routeLive(page, {
      first: async () => {
        if (!holdSnapshot) return;
        released = false;
        // The hold under test: the board's first snapshot waits 5 s, and Open's rows and the
        // Queue filter's loading state must show within it, while `released` is still false.
        await sleep(5_000);
        released = true;
      },
    });
    await page.setViewportSize(TALL);
    const table = issuesTable(page);
    const reads = async (step: string) => {
      // An absence window, not a wait for a state: each step's check drew the rows its reads
      // answered, and a listing the step must not read, the full listing above all, would go out
      // in the render that an answer it follows causes, well inside this half second.
      await sleep(500);
      await requests.settled();
      const listings = listingReads(requests.list, ACME);
      for (const read of listings) {
        const query = read.url.searchParams;
        expect(
          query.has("open") || query.getAll("status").length > 0,
          `${step}: ${read.url.search} is the full listing`
        ).toBe(true);
        if (query.has("limit") && query.get("limit") !== "1")
          expect(read.bytes, `${step}: a page is no smaller than every done issue`).toBeLessThan(
            fullBytes
          );
      }
      return listings.map((read) => queryOf(read.url)).sort();
    };

    // Done's next page reads offset=25 and the items over its own close days.
    await page.goto(`/projects/${ACME}/issues?view=done&size=25`);
    await expect.poll(() => rowKeys(table)).toEqual(keysOf(done.slice(0, 25)));
    await requests.settled();
    requests.clear();
    await page.getByRole("button", { name: "Next ›" }).click();
    await expect.poll(() => rowKeys(table)).toEqual(keysOf(done.slice(25, 50)));
    await expect.poll(() => itemsReads(requests.list).length).toBeGreaterThan(0);
    expect(await reads("Done's next page")).toEqual([
      `limit=25&offset=25&project=${ACME}&sort=-closed_at&status=done`,
    ]);
    // The items over the next page's own close days are read; a read of the first page's days
    // that was still out is that page's, not this one's.
    const nextWindows = windowsOf(done.slice(25, 50), board);
    const firstWindows = windowsOf(done.slice(0, 25), board);
    expect(itemsDiffer(requests.list, [...nextWindows, ...firstWindows])).toEqual([]);
    for (const window of nextWindows)
      expect(
        itemsReads(requests.list).some(
          (read) =>
            Date.parse(read.url.searchParams.get("from") ?? "") === Date.parse(window.from) &&
            Date.parse(read.url.searchParams.get("to") ?? "") === Date.parse(window.to)
        ),
        `the next page's items over ${window.from}..${window.to}`
      ).toBe(true);

    // The Closed panel: D to D+6 writes D..D+7, reads that window in UTC, and opens again on
    // D+6.
    const { d } = corpus;
    await page.goto(`/projects/${ACME}/issues?view=done&size=25`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await requests.settled();
    requests.clear();
    await page.getByRole("button", { name: "Filter Closed" }).click();
    const panel = page.getByRole("dialog", { name: "Filter Closed" });
    await expect(panel).toContainText("Or from and to (UTC)");
    await panel.getByLabel("Closed from").fill(isoDay(d));
    await panel.getByLabel("Closed to").fill(isoDay(d + 6 * DAY));
    await panel.getByRole("button", { exact: true, name: "Apply" }).click();
    await expect
      .poll(() => new URL(page.url()).searchParams.get("f.closed"))
      .toBe(`${isoDay(d)}..${isoDay(d + 7 * DAY)}`);
    const windowed = done.filter((issue) => closeOf(issue) >= d && closeOf(issue) < d + 7 * DAY);
    await expect.poll(() => tabCount(page, "Done")).toBe(windowed.length);
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(keysOf(windowed).sort());
    expect(await reads("the picked window")).toEqual([
      `closed_before=${iso(d + 7 * DAY)}&closed_since=${iso(d)}&limit=25&offset=0&project=${ACME}&sort=-closed_at&status=done`,
    ]);
    expect(await rowKeys(table)).toContain(corpus.atNoon);
    expect(await rowKeys(table)).not.toContain(corpus.atMidnight);
    await page.getByRole("button", { name: "Filter Closed" }).click();
    await expect(
      page.getByRole("dialog", { name: "Filter Closed" }).getByLabel("Closed to")
    ).toHaveValue(isoDay(d + 6 * DAY));
    await page.keyboard.press("Escape");

    // The search, a sort by priority, the views that list done issues and the Boards read
    // status=done with no limit, and the items over the close days of the rows they answer.
    const steps: readonly (readonly [string, readonly string[], () => Promise<void>])[] = [
      [
        `view=done&q=${corpus.word}`,
        [`project=${ACME}&status=done`],
        async () => {
          const matching = done.filter((issue) => issue.title.includes(corpus.word));
          await expect
            .poll(async () => (await rowKeys(table)).sort())
            .toEqual(keysOf(matching).sort());
        },
      ],
      [
        "view=done&sort=-priority",
        [`project=${ACME}&status=done`],
        async () => {
          await expect.poll(() => tabCount(page, "Done")).toBe(done.length);
        },
      ],
      [
        "view=open&status=done",
        [`project=${ACME}&status=done`],
        async () => {
          await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
          await expect.poll(() => tabCount(page, "Open")).toBe(done.length);
        },
      ],
      [
        "view=open&nofilter=1",
        [`open=true&project=${ACME}`, `project=${ACME}&status=done`],
        async () => {
          await expect.poll(() => tabCount(page, "Open")).toBe(open.length + done.length);
        },
      ],
      [
        "view=open&layout=board",
        [`open=true&project=${ACME}`],
        async () => {
          await expect(page.locator("[data-board-card]").first()).toBeVisible();
        },
      ],
      [
        "view=open&layout=board&label=widgets",
        [`open=true&project=${ACME}`],
        async () => {
          await expect
            .poll(async () =>
              page
                .locator("[data-board-card]")
                .evaluateAll((cards) => cards.map((card) => card.getAttribute("data-board-card")))
            )
            .toEqual([labelled.key]);
        },
      ],
      [
        "view=done&layout=board",
        [`project=${ACME}&status=done`],
        async () => {
          await expect
            .poll(() => page.locator('section[data-board-column="done"] [data-board-card]').count())
            .toBe(done.length);
        },
      ],
    ];
    for (const [address, wanted, check] of steps) {
      await requests.settled();
      requests.clear();
      await page.goto(`/projects/${ACME}/issues?${address}`);
      await check();
      const listings = await reads(address);
      for (const query of wanted) expect(listings, address).toContain(query);
      for (const query of listings)
        expect(
          [...wanted, `limit=1&open=true&project=${ACME}`],
          `${address}: a listing it should not read`
        ).toContain(query);
      if (wanted.includes(`project=${ACME}&status=done`)) {
        await expect.poll(() => itemsReads(requests.list).length).toBeGreaterThan(0);
        expect(itemsDiffer(requests.list, windowsOf(done, board, Date.now())), address).toEqual([]);
      }
    }

    // A 28-day window reads a page from the window's first moment.
    await requests.settled();
    requests.clear();
    const loaded = Date.now();
    await page.goto(`/projects/${ACME}/issues?view=done&size=25&f.closed=28d`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    const rolling = listingReads(requests.list, ACME).find((read) =>
      read.url.searchParams.has("closed_since")
    );
    expect(rolling, "f.closed=28d reads no window").toBeDefined();
    const since = Date.parse(rolling?.url.searchParams.get("closed_since") ?? "");
    expect(Math.abs(since - (loaded - 28 * DAY)), "the window's first moment").toBeLessThan(60_000);
    expect(rolling?.url.searchParams.get("limit")).toBe("25");
    await reads("f.closed=28d");

    // Open reads open=true with no limit, and its rows show before the board's snapshot.
    holdSnapshot = true;
    released = false;
    await requests.settled();
    requests.clear();
    await page.goto(`/projects/${ACME}/issues?view=open`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    expect(released, "Open's rows waited for the board's snapshot").toBe(false);
    expect(await reads("view=open")).toEqual([`open=true&project=${ACME}`]);

    // The Board from the layout control reads what its view reads.
    await requests.settled();
    requests.clear();
    await page.getByRole("button", { exact: true, name: "Board" }).click();
    await expect(page.locator("[data-board-card]").first()).toBeVisible();
    for (const query of await reads("the Board from the layout control"))
      expect(query).toBe(`open=true&project=${ACME}`);
    await page.getByRole("button", { exact: true, name: "Table" }).click();

    // Open with the Queue filter shows its loading state, not unfiltered rows, until the
    // snapshot arrives.
    await requests.settled();
    requests.clear();
    released = false;
    await page.goto(`/projects/${ACME}/issues?view=open&f.queue=Queued`);
    await expect(page.getByRole("status", { name: "Loading issues" })).toBeVisible();
    expect(released).toBe(false);
    expect(await table.locator("tbody tr[data-issue-row]").count()).toBe(0);
    const queued = [...cardsOf(board)]
      .filter(([, placed]) => placed.column === "queue" || placed.card.waiting_place !== null)
      .map(([key]) => key);
    await expect
      .poll(async () => (await rowKeys(table)).sort(), { timeout: 15_000 })
      .toEqual(queued.sort());
    await reads("view=open&f.queue=Queued");
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** A done root R with Legion work, closed two days ago, that no observation names and no tree of
 *  a card holds, so its facts come from the items route alone; waited until its facts row
 *  carries its close and the board's map holds no entry for it. */
async function seedRoot(): Promise<string> {
  const root = `${ACME}-8`;
  await sql(
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank, created_at, closed_at) VALUES
       ('${root}', '${ACME}', 8, 'Root closed outside the board', 'done', 1, ${SEED_ACTOR}, 'z0008', now() - interval '5 days', now() - interval '2 days')`,
    ...settledCounters(ACME)
  );
  await writeRoleEvent(root, "architect", "3 days");
  await Promise.all([
    waitForIssueFacts([root, `${ACME}-6`, `${ACME}-7`]),
    waitForLegionWorkTimeEval(ACME),
  ]);
  await expect
    .poll(
      async () =>
        (
          await sql(`SELECT closed_at IS NOT NULL FROM issue_facts WHERE issue_key = '${root}'`)
        ).trim(),
      { message: "R's facts row carries its close", timeout: 75_000 }
    )
    .toBe("t");
  expect((await getLegionBoard(ACME)).issues?.[root], "the board's map holds R").toBeUndefined();
  return root;
}

/** Holds, fails or passes every request to the items route of `page`, and counts them. */
async function routeItems(page: Page, message: string) {
  const held = Promise.withResolvers<void>();
  const state = { mode: "hold" as "hold" | "fail" | "pass", calls: 0, release: held.resolve };
  await page.route(`**${ITEMS_PATH}**`, async (route) => {
    state.calls += 1;
    if (state.mode === "hold") await held.promise;
    if (state.mode === "fail") {
      await route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: message }),
        contentType: "application/json",
        status: 500,
      });
      return;
    }
    await route.continue();
  });
  return state;
}

/** What each facts cell of `key` draws: its read's mark, or its text. */
function factsCells(table: Locator, key: string): Promise<Record<string, string>> {
  return table.evaluate((node, row) => {
    const labels = [...node.querySelectorAll("thead th")].map((cell) =>
      (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, "").trim()
    );
    const cells: Record<string, string> = {};
    const drawn = node.querySelector(`tr[data-issue-row="${row}"]`);
    [...(drawn?.children ?? [])].forEach((cell, index) => {
      const label = labels[index] ?? "";
      if (!["Work time", "Time per phase", "Review rounds", "Legion tree"].includes(label)) return;
      const mark = cell.querySelector("[data-facts-read]")?.getAttribute("data-facts-read");
      // Time per phase draws a bar, whose words are its total; every other cell its text.
      const words =
        label === "Time per phase"
          ? (cell.querySelector("[data-phase-hours]")?.getAttribute("aria-label") ?? "")
          : (cell.textContent ?? "").trim();
      cells[label] = mark ?? `text:${words}`;
    });
    return cells;
  }, key);
}

// SLOW(>=2s): the facts consumer's row of R and the eval refresher's pass, up to 75 s, then two
// pages whose items reads are held, failed through their two retries and let through.
test("while the items read of Done's rows is held or failing, Done narrowed to Legion roots shows its loading state and then its error with Retry, never a list without R, and the rows' facts cells show a loading mark and then an error mark with the alert under the tabs", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const root = await seedRoot();
    const done = await doneIssues();
    const facts = await factsOf(done);
    const roots = done.filter((issue) => legionPick(facts.get(issue.key)) === "root");
    expect(keysOf(roots), "the done roots with Legion work").toContain(root);
    const message = "The items store did not answer.";

    // Done with f.legion=root: held, then failing, then let through with Retry.
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const items = await routeItems(page, message);
    await page.goto(`/projects/${ACME}/issues?view=done&f.legion=root`);
    const table = issuesTable(page);
    const neverWrong = async (step: string) => {
      expect(await tabNames(page), `${step}: a tab counts 0`).not.toContain(tabName("Done", 0));
      const keys = await table.locator("tbody tr[data-issue-row]").count();
      expect(keys, `${step}: a list without R`).toBe(0);
      await expect(page.getByText("No issues match these filters.")).toHaveCount(0);
    };
    await expect(page.getByRole("status", { name: "Loading issues" })).toBeVisible();
    await expect.poll(() => items.calls).toBeGreaterThan(0);
    // An absence window, not a wait for a state: the items read stays held through this second,
    // and no tab counting 0, no list without R and no empty-state line may show in it; the page
    // would draw any of them in the render after its reads went out, well inside a second.
    await sleep(1_000);
    await neverWrong("while the read is held");
    await expect(page.getByRole("status", { name: "Loading issues" })).toBeVisible();
    const held = items.calls;
    items.mode = "fail";
    items.release();
    const error = page.getByText("Could not read the Legion columns, which this view needs.");
    await expect(error).toBeVisible({ timeout: 15_000 });
    await neverWrong("once the read failed");
    expect(items.calls, "each items request tried once and retried twice").toBe(held * 3);
    await test.info().attach("the items requests of Done narrowed to roots", {
      body: `${items.calls} requests`,
      contentType: "text/plain",
    });
    items.mode = "pass";
    await page.getByRole("button", { exact: true, name: "Retry" }).click();
    await expect.poll(async () => (await rowKeys(table)).sort()).toEqual(keysOf(roots).sort());
    await expect.poll(() => tabCount(page, "Done")).toBe(roots.length);

    // Done with no filter: R's facts cells show a loading mark, then an error mark, with the
    // alert under the tabs.
    const second = await context.newPage();
    await second.setViewportSize({ height: 900, width: 1440 });
    const secondItems = await routeItems(second, message);
    await second.goto(`/projects/${ACME}/issues?view=done`);
    const secondTable = issuesTable(second);
    await expect(secondTable.locator(`tr[data-issue-row="${root}"]`)).toBeVisible();
    const marks = async () => Object.values(await factsCells(secondTable, root));
    await expect.poll(marks).toEqual(["pending", "pending"]);
    secondItems.mode = "fail";
    secondItems.release();
    await expect.poll(marks, { timeout: 15_000 }).toEqual(["error", "error"]);
    await expect(
      second
        .getByRole("alert")
        .filter({ hasText: "Could not read the Legion columns of the closed issues" })
    ).toHaveText(`Could not read the Legion columns of the closed issues: ${message} Retry`);
    await expect(
      second.getByRole("button", { name: "Retry reading the Legion columns" })
    ).toBeVisible();
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): ACME seeded with the metrics corpus, then Done drawn with every facts cell of its
// rows waiting on the refused items read.
test("an items route that answers 404 EVAL_NOT_FOUND draws the error mark in every facts cell of the rows it would fill, and the alert under the tabs names it with Retry", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const done = await doneIssues();
    const board = await getLegionBoard(ACME);
    const filled = done.filter((issue) => board.issues?.[issue.key] === undefined);
    expect(filled.length, "no done row takes its facts from the items route").toBeGreaterThan(0);
    const page = await context.newPage();
    const refusal = `No eval ${legionWorkTimeEval} in ${ACME}.`;
    await page.route(`**${ITEMS_PATH}**`, (route) =>
      route.fulfill({
        body: JSON.stringify({ code: "EVAL_NOT_FOUND", error: refusal }),
        contentType: "application/json",
        status: 404,
      })
    );
    await page.goto(
      `/projects/${ACME}/issues?view=done&cols=key,title,closed,phase_hours,work_hours,review_rounds,legion`
    );
    const table = issuesTable(page);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    for (const issue of filled)
      await expect
        .poll(async () => Object.values(await factsCells(table, issue.key)), { message: issue.key })
        .toEqual(["error", "error", "error", "error"]);
    await expect(
      page
        .getByRole("alert")
        .filter({ hasText: "Could not read the Legion columns of the closed issues" })
    ).toHaveText(`Could not read the Legion columns of the closed issues: ${refusal} Retry`);
    await expect(
      page.getByRole("button", { name: "Retry reading the Legion columns" })
    ).toBeVisible();
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** One page of the list route, read straight: its status and its body. */
async function listPage(query: string): Promise<{ status: number; body: Record<string, unknown> }> {
  const response = await fetch(new URL(`/api/v1/issues?project=${ACME}&${query}`, baseUrl), {
    headers: await userHeaders(),
  });
  return { status: response.status, body: (await response.json()) as Record<string, unknown> };
}

// SLOW(>=2s): 64 done issues and three open ones seeded, eleven addresses read row by row, then
// six reads of the list route.
test("each of today's addresses lists the rows the API gives, q=epic only the rows that hold the word and f.asks=1 every issue with an open ask, and the list route sorts done issues by closed_at with an issue with no close last", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    await seedDoneCorpus();
    const asker = { actor: { id: "views-asker", kind: "session" as const }, as: "agent" as const };
    const one = await createIssue({ project: ACME, title: "Fix the widget loader" });
    await patchIssue(one.key, { labels: ["widgets"], priority: 0, status: "todo" });
    await createAsk(one.key, { question: "Which loader ships first?" }, asker);
    const two = await createIssue({ project: ACME, title: "Plan the epic rollout" });
    await createAsk(two.key, { question: "Who signs off the rollout?" }, asker);
    await createAsk(two.key, { question: "When does it start?" }, asker);
    await claimIssue(two.key, { as: "user", login: "alice" });
    const three = await createIssue({ project: ACME, title: "Tidy the gadget docs" });
    await patchIssue(three.key, { labels: ["epic"] });

    const open = await listIssuesWhere({ open: true, project: ACME });
    const state = (await getMyState("alice")) as unknown as Record<
      string,
      { readonly last_read_seq?: number } | undefined
    >;
    const holds = (issue: IssueSummary, word: string) =>
      [
        issue.key,
        issue.title,
        issue.assignee ?? "",
        issue.created_by?.id ?? "",
        issue.claim?.actor.id ?? "",
        ...(issue.labels ?? []),
      ].some((text) => text.toLowerCase().includes(word));
    const addresses: readonly (readonly [string, (issue: IssueSummary) => boolean])[] = [
      ["label=widgets", (issue) => (issue.labels ?? []).includes("widgets")],
      ["q=gadget", (issue) => holds(issue, "gadget")],
      ["unclaimed=1", (issue) => issue.claim === null],
      ["needs-you=1", (issue) => issue.open_asks > 0],
      ["unread=1", (issue) => issue.last_seq > (state[issue.key]?.last_read_seq ?? 0)],
      ["group=epic", () => true],
      ["f.priority=0", (issue) => issue.priority === 0],
      ["f.assignee=alice", (issue) => issue.assignee === "alice"],
      ["f.asks=0", (issue) => issue.open_asks === 0],
      ["f.asks=1", (issue) => issue.open_asks > 0],
      ["q=epic", (issue) => holds(issue, "epic")],
    ];
    const page = await context.newPage();
    await page.setViewportSize(TALL);
    const table = issuesTable(page);
    for (const [address, keeps] of addresses) {
      const wanted = open
        .filter(keeps)
        .map((issue) => issue.key)
        .sort();
      expect(wanted.length, `${address}: the seed lists no row`).toBeGreaterThan(0);
      await page.goto(`/projects/${ACME}/issues?${address}`);
      await expect
        .poll(async () => (await rowKeys(table)).sort(), { message: address })
        .toEqual(wanted);
    }
    expect(open.filter((issue) => issue.open_asks > 1).map((issue) => issue.key)).toEqual([
      two.key,
    ]);

    // The list route's sort by closed_at: newest or oldest first, a done issue with no close
    // last in both, and each page's total every done issue.
    const done = await doneIssues();
    const unclosed = done[10]?.key ?? "";
    await sql(`UPDATE issues SET closed_at = NULL WHERE key = '${unclosed}'`);
    const total = done.length;
    for (const [sort, newestFirst] of [
      ["-closed_at", true],
      ["closed_at", false],
    ] as const) {
      const first = await listPage(`status=done&sort=${sort}&limit=25&offset=0`);
      const later = await listPage(`status=done&sort=${sort}&limit=25&offset=50`);
      expect([first.status, later.status]).toEqual([200, 200]);
      expect([first.body.total, later.body.total], `sort=${sort}: the totals`).toEqual([
        total,
        total,
      ]);
      const rows = [
        ...((first.body.issues as IssueSummary[]) ?? []),
        ...((later.body.issues as IssueSummary[]) ?? []),
      ];
      const closes = rows.filter((row) => row.closed_at !== null).map(closeOf);
      expect(closes, `sort=${sort}: the order of the closes`).toEqual(
        [...closes].sort((a, b) => (newestFirst ? b - a : a - b))
      );
      expect(rows.at(-1)?.key, `sort=${sort}: the issue with no close`).toBe(unclosed);
      expect(rows.at(-1)?.closed_at).toBeNull();
    }
    const refused = await listPage("status=done&sort=title&limit=25&offset=0");
    expect([refused.status, refused.body.code]).toEqual([400, "INVALID_QUERY"]);
    const own = await listPage("status=done&limit=25&offset=0");
    const ranks = ((own.body.issues as IssueSummary[]) ?? []).map((row) => row.rank);
    expect(ranks, "the route's own order").toEqual([...ranks].sort());
  } finally {
    await context.close();
    await daemon.close();
  }
});

/** k9's done issues on ACME beside the metrics corpus's: X, closed 40 days ago, which no
 *  observation names and no tree of a card holds, and two closed around the end of D, a UTC day
 *  10 days back: one at 23:00Z on D and one at 00:00Z on D+1; each with Legion work, waited for
 *  with the eval refresher's pass. */
interface WindowCorpus {
  readonly d: number;
  readonly old: string;
  readonly late: string;
  readonly next: string;
}

async function seedWindowCorpus(): Promise<WindowCorpus> {
  const d = midnight(Date.now()) - 10 * DAY;
  const rows = [
    { key: `${ACME}-8`, title: "Closed forty days ago", closed: Date.now() - 40 * DAY },
    { key: `${ACME}-9`, title: "Closed late on the day", closed: d + 23 * HOUR },
    { key: `${ACME}-10`, title: "Closed at the next midnight", closed: d + DAY },
  ];
  await sql(
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank, created_at, closed_at) VALUES ${rows
      .map(
        (row, index) =>
          `('${row.key}', '${ACME}', ${index + 8}, '${row.title}', 'done', ${index}, ${SEED_ACTOR}, 'z${index + 8}', '${iso(row.closed - 3 * DAY)}', '${iso(row.closed)}')`
      )
      .join(", ")}`,
    ...settledCounters(ACME)
  );
  for (const row of rows)
    await sql(
      `WITH next AS (UPDATE issues SET last_seq = last_seq + 1 WHERE key = '${row.key}' RETURNING last_seq)
       INSERT INTO events (issue_key, seq, type, actor, payload, notify, created_at)
         SELECT '${row.key}', last_seq, 'comment.created',
           '{"kind":"session","id":"s-implementer-${row.key}","origin":{"session_title":"Legion implementer · ${row.key}"}}'::jsonb,
           '{}'::jsonb, false, '${iso(row.closed - DAY)}'
         FROM next`
    );
  await Promise.all([
    waitForIssueFacts([...rows.map((row) => row.key), `${ACME}-6`, `${ACME}-7`]),
    waitForLegionWorkTimeEval(ACME),
  ]);
  const [old, late, next] = rows.map((row) => row.key);
  return { d, old: old ?? "", late: late ?? "", next: next ?? "" };
}

/** Whether `keys` stand in the order `value` gives them, the empty values last. */
function inOrder(
  keys: readonly string[],
  value: (key: string) => number | undefined
): { order: (number | null)[]; sorted: boolean } {
  const values = keys.map(value);
  const defined = values.filter((one): one is number => one !== undefined);
  const firstEmpty = values.indexOf(undefined);
  const sorted =
    defined.every((one, index) => index === 0 || (defined[index - 1] ?? one) <= one) &&
    (firstEmpty === -1 || values.slice(firstEmpty).every((one) => one === undefined));
  return { order: values.map((one) => one ?? null), sorted };
}

/** The control row's chips, by their words. */
const chipTexts = (page: Page) => control(controlRow(page), CONTROL.chip).allTextContents();

// SLOW(>=2s): the facts consumer's rows of five issues and the eval refresher's pass, up to 75 s,
// then fourteen addresses of Done read row by row.
test("each parameter of an Evals link narrows Done to the rows the routes give: a UTC day, a range of times, 28 days, the Legion tree, a phase's hours, a tree left out, three sorts, the columns and the issues a link names", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice", LOS_ANGELES);
  try {
    const corpus = await seedWindowCorpus();
    const done = await doneIssues();
    const facts = await factsOf(done);
    const page = await context.newPage();
    await page.setViewportSize(TALL);
    const table = issuesTable(page);
    const listed = async (address: string, wanted: readonly IssueSummary[]) => {
      await page.goto(`/projects/${ACME}/issues?${address}`);
      await expect
        .poll(async () => (await rowKeys(table)).sort(), { message: address })
        .toEqual(keysOf(wanted).sort());
    };
    const closedIn = (from: number, to: number) =>
      done.filter((issue) => closeOf(issue) >= from && closeOf(issue) < to);
    const { d } = corpus;

    await listed("view=done", done);
    expect(await rowKeys(table)).toContain(corpus.old);
    await listed(`view=done&f.closed=${isoDay(d)}..${isoDay(d + DAY)}`, closedIn(d, d + DAY));
    expect(await rowKeys(table)).toContain(corpus.late);
    expect(await rowKeys(table)).not.toContain(corpus.next);
    await listed(
      `view=done&f.closed=${iso(d - 12 * HOUR)}..${iso(d + 23 * HOUR + 30 * 60_000)}`,
      closedIn(d - 12 * HOUR, d + 23 * HOUR + 30 * 60_000)
    );
    await listed("view=done&f.closed=28d", closedIn(Date.now() - 28 * DAY, Date.now() + DAY));
    expect(await rowKeys(table)).not.toContain(corpus.old);
    const pick = (keep: (one: Facts | undefined) => boolean) =>
      done.filter((issue) => keep(facts.get(issue.key)));
    await listed(
      "view=done&f.legion=root",
      pick((one) => legionPick(one) === "root")
    );
    await listed(
      "view=done&f.legion=child",
      pick((one) => legionPick(one) === "child")
    );
    await listed(
      "view=done&f.legion=root&f.legion=child",
      pick((one) => one !== undefined)
    );
    await listed(
      "view=done&f.phase_hours=reviewing",
      pick((one) => (one?.hours_by_phase.reviewing ?? 0) > 0)
    );

    // The largest tree, left out, and its chip.
    const trees = new Map<string, string[]>();
    for (const issue of done) {
      const tree = facts.get(issue.key);
      if (tree === undefined) continue;
      const root = tree.tree_depth === 0 ? issue.key : tree.root_key;
      trees.set(root, [...(trees.get(root) ?? []), issue.key]);
    }
    const largest = [...trees].sort((a, b) => b[1].length - a[1].length)[0];
    if (largest === undefined) throw new Error("the seed has no tree");
    await listed(
      `view=done&exclude_tree=${largest[0]}`,
      done.filter((issue) => !largest[1].includes(issue.key))
    );
    expect((await chipTexts(page)).some((chip) => chip.includes(largest[0]))).toBe(true);

    // Three sorts, each by its column.
    const sorts: readonly (readonly [string, (key: string) => number | undefined])[] = [
      ["closed", (key) => closeOf(done.find((issue) => issue.key === key) as IssueSummary)],
      ["work_hours", (key) => facts.get(key)?.work_hours],
      ["review_rounds", (key) => facts.get(key)?.review_rounds],
    ];
    for (const [sort, value] of sorts) {
      await listed(`view=done&sort=${sort}`, done);
      expect(inOrder(await rowKeys(table), value), `sort=${sort}`).toMatchObject({ sorted: true });
    }

    // cols= names the columns shown.
    await page.goto(`/projects/${ACME}/issues?view=done&cols=key,title,closed,work_hours`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    expect(
      await table
        .locator("thead th")
        .evaluateAll((cells) =>
          cells
            .map((cell) =>
              (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, "").trim()
            )
            .filter((label) => label !== "")
        )
    ).toEqual(["Key", "Title", "Closed", "Work time"]);

    // only with onlyLabel naming one open and one done key: Done lists the done key alone, and
    // with nofilter=1 and no view both.
    const only = `only=ACME-1,${corpus.late}&onlyLabel=Two%20issues`;
    await page.goto(`/projects/${ACME}/issues?view=done&${only}`);
    await expect.poll(() => rowKeys(table)).toEqual([corpus.late]);
    expect((await chipTexts(page)).some((chip) => chip.startsWith("Showing: Two issues"))).toBe(
      true
    );
    await page.goto(`/projects/${ACME}/issues?${only}&nofilter=1`);
    await expect
      .poll(async () => (await rowKeys(table)).sort())
      .toEqual(["ACME-1", corpus.late].sort());
  } finally {
    await context.close();
    await daemon.close();
  }
});

// SLOW(>=2s): the facts consumer's rows of five issues and the eval refresher's pass, up to 75 s,
// then Open sorted three ways and by Work time and Created, and Done's reads of an issue closed
// 40 days ago.
test("Open sorts by Time waiting, Time in Legion phase and Worker state as the board gives them, and by Work time then Created with the rows with no Work time in Created's order, and Done with no window reads the items over windows of at most 84 days that hold every close and draws the facts of an issue closed 40 days ago", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice", LOS_ANGELES);
  try {
    const corpus = await seedWindowCorpus();
    const board = await getLegionBoard(ACME);
    const cards = cardsOf(board);
    const page = await context.newPage();
    await page.setViewportSize(TALL);
    const table = issuesTable(page);
    const open = await listIssuesWhere({ open: true, project: ACME });
    const since = (moment: string | null | undefined) =>
      moment === null || moment === undefined ? undefined : -Date.parse(moment);
    const STATES = ["working", "between_turns", "held_children", "held_person", "held_idle"];
    const sorts: readonly (readonly [string, (key: string) => number | undefined])[] = [
      ["waited", (key) => since(cards.get(key)?.card.waiting?.since)],
      ["time_in_phase", (key) => since(cards.get(key)?.card.phase_since)],
      [
        "activity",
        (key) => {
          const use = cards.get(key)?.card.slot_use;
          return use === null || use === undefined || use === "" ? undefined : STATES.indexOf(use);
        },
      ],
    ];
    for (const [sort, value] of sorts) {
      await page.goto(`/projects/${ACME}/issues?view=open&sort=${sort}`);
      await expect.poll(async () => (await rowKeys(table)).length).toBe(open.length);
      expect(inOrder(await rowKeys(table), value), `sort=${sort}`).toMatchObject({ sorted: true });
    }

    // Sorted by Work time, a shift-click on Created adds a second sort: the rows with no Work time
    // come after the rows with one, and among themselves in Created's order. Their creation times
    // are written an hour apart in the listing's own order with its first two swapped, so neither
    // that order nor its reverse is Created's.
    const facts = await getLegionBoard(ACME);
    const measured = (key: string) => measuredWork(boardFacts(facts, key));
    const unmeasured = open.filter((issue) => measured(issue.key) === undefined);
    expect(unmeasured.length, "Open's rows with no Work time").toBeGreaterThan(2);
    const firstCreated = Date.now() - 30 * DAY;
    const created = new Map(
      unmeasured.map(
        (issue, index) =>
          [issue.key, firstCreated + (index < 2 ? 1 - index : index) * HOUR] as const
      )
    );
    await sql(
      ...[...created].map(
        ([key, at]) => `UPDATE issues SET created_at = '${iso(at)}' WHERE key = '${key}'`
      )
    );
    await page.goto(
      `/projects/${ACME}/issues?view=open&sort=-work_hours&cols=key,title,work_hours,created`
    );
    await expect.poll(async () => (await rowKeys(table)).length).toBe(open.length);
    await table
      .locator("thead")
      .getByRole("button", { name: /^Created/ })
      .click({ modifiers: ["Shift"] });
    await expect
      .poll(() => new URL(page.url()).searchParams.get("sort"))
      .toBe("-work_hours,created");
    await expect
      .poll(async () => {
        const keys = await rowKeys(table);
        const empty = keys.filter((key) => measured(key) === undefined);
        return {
          measuredFirst: inOrder(keys, (key) => (measured(key) === undefined ? undefined : 0))
            .sorted,
          createdOrder: inOrder(empty, (key) => created.get(key)).sorted,
        };
      })
      .toEqual({ measuredFirst: true, createdOrder: true });

    // Done with no window: the items over windows of at most 84 days that hold every close, and
    // the facts cells of the issue closed 40 days ago equal to its items row.
    const requests = recordRequests(page);
    const done = await doneIssues();
    await page.goto(
      `/projects/${ACME}/issues?view=done&cols=key,title,closed,phase_hours,work_hours,review_rounds,legion`
    );
    await expect(table.locator(`tr[data-issue-row="${corpus.old}"]`)).toBeVisible();
    await expect.poll(() => itemsReads(requests.list).length).toBeGreaterThan(0);
    const spans = () =>
      itemsReads(requests.list).map((read) => [
        Date.parse(read.url.searchParams.get("from") ?? ""),
        Date.parse(read.url.searchParams.get("to") ?? ""),
      ]);
    const unheld = done.filter((one) => board.issues?.[one.key] === undefined);
    await expect
      .poll(
        () => {
          const read = spans();
          return unheld
            .filter(
              (issue) =>
                !read.some(
                  ([from, to]) => closeOf(issue) >= (from ?? 0) && closeOf(issue) < (to ?? 0)
                )
            )
            .map((issue) => issue.key);
        },
        { message: "the closes no items window holds" }
      )
      .toEqual([]);
    await requests.settled();
    for (const [from, to] of spans())
      expect((to ?? 0) - (from ?? 0), "an items window over 84 days").toBeLessThanOrEqual(84 * DAY);
    const old = (await factsOf(done.filter((one) => one.key === corpus.old))).get(corpus.old);
    if (old === undefined) throw new Error("the items route holds no row of the issue 40 days old");
    const total = Object.values(old.hours_by_phase).reduce(
      (sum, one) => (one > 0 ? sum + one : sum),
      0
    );
    await expect
      .poll(() => factsCells(table, corpus.old))
      .toEqual({
        "Legion tree": `text:${old.tree_depth === 0 ? "Root" : old.root_key}`,
        "Review rounds": `text:${old.review_rounds}`,
        "Time per phase": `text:${total > 0 ? `Time per phase, ${hoursWords(total)}` : ""}`,
        "Work time": `text:${workWords(old.work_hours)}`,
      });
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R5 and the Design's date range filter, found by INFRA-3221's tester: a Closed window the list
// route refuses is never what Done asks it for. A window whose To comes before its From, picked in
// the Closed filter's panel or written in an address, holds no time and so no close; a day count
// past the earliest date a read can send holds every close. Neither draws a failed read.
// SLOW(>=2s): ACME seeded, and Done opened over four windows, one of them picked in its panel.
test("a Closed window the list route would refuse draws no failed read: To before From lists no row, and a count past every date lists every done row", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const refused: string[] = [];
    page.on("response", (response) => {
      if (response.url().includes("/api/v1/issues") && response.status() >= 400)
        refused.push(`${response.status()} ${new URL(response.url()).search}`);
    });
    const table = issuesTable(page);
    const failed = page.getByRole("alert").filter({ hasText: /Could not load/ });
    const noRow = page.getByText("No issues match these filters.");
    const done = await listIssuesWhere({ project: ACME, status: ["done"] });

    // To before From, picked in the panel: the picker keeps it out of the address, or Done lists
    // no row, since such a window holds no close.
    await page.goto(`/projects/${ACME}/issues?view=done`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await page
      .getByRole("button", { name: /^Filter Closed/ })
      .first()
      .click();
    await page.getByLabel("Closed from").fill("2026-11-10");
    await page.getByLabel("Closed to").fill("2026-11-03");
    await page.getByRole("button", { exact: true, name: "Apply" }).click();
    await expect
      .poll(
        async () => !new URL(page.url()).searchParams.has("f.closed") || (await noRow.isVisible()),
        { message: "the picked window To before From: left out, or listed as no row" }
      )
      .toBe(true);
    await expect(failed).toBeHidden();
    expect(refused).toEqual([]);

    // The same window in an address: no row, and its chip shows the window.
    const closedChip = control(controlRow(page), CONTROL.chip).filter({
      hasText: /^Closed: 2026-11-10 to /,
    });
    await page.goto(`/projects/${ACME}/issues?view=done&f.closed=2026-11-10..2026-11-03`);
    await expect(noRow).toBeVisible();
    await expect(closedChip).toHaveCount(1);
    await expect(failed).toBeHidden();
    expect(refused).toEqual([]);

    // k9, found by INFRA-3221's tester: a To day equal to its From day holds no moment either, so
    // Done lists no row, sends no request the route refuses, and its chip shows the window.
    await page.goto(`/projects/${ACME}/issues?view=done&f.closed=2026-11-10..2026-11-10`);
    await expect(noRow).toBeVisible();
    await expect(closedChip).toHaveCount(1);
    await expect(failed).toBeHidden();
    expect(refused).toEqual([]);

    // 750,000 days reaches back past the year 0, a date the route cannot read: every done row.
    await page.goto(`/projects/${ACME}/issues?view=done&f.closed=750000d`);
    await expect.poll(async () => (await rowKeys(table)).length).toBe(done.length);
    await expect(failed).toBeHidden();
    expect(refused).toEqual([]);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R2, the Design's Loading and the architect's ruling on INFRA-3221's review: a page read whose
// answer names no `sort` comes from a server older than `sort` while a release rolls out, which
// answers the route's own order with `closed_at` on its rows. Done draws none of its rows and
// counts none on its tab or its pager: it reads the page again, then says the server did not sort
// the closed issues, with Retry. Once a server that applies the sort answers, Retry draws the page.
// SLOW(>=2s): ACME seeded, and Done opened over page reads whose answers lose their sort.
test("a Done page whose answer names no sort draws no row, counts none, and says the server did not sort it, with Retry", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const table = issuesTable(page);
    const done = page.getByRole("tab", { name: /^Done/ });
    let older = true;
    let reads = 0;
    await page.route(/\/api\/v1\/issues\?.*sort=-closed_at/, async (route) => {
      reads += 1;
      if (!older) return route.continue();
      const response = await route.fetch();
      const body: { sort?: string } = await response.json();
      delete body.sort;
      await route.fulfill({ response, json: body });
    });
    await page.goto(`/projects/${ACME}/issues?view=done`);
    const unsorted = page
      .getByRole("alert")
      .filter({ hasText: "The server did not sort the closed issues by close time." });
    await expect(unsorted).toBeVisible();
    // The first read and the two the shared retry policy makes again, none of them drawn.
    expect(reads).toBeGreaterThanOrEqual(3);
    await expect(table.locator("tbody tr[data-issue-row]")).toHaveCount(0);
    await expect(done).toHaveAttribute("aria-label", tabName("Done"));
    await expect(page.getByText(/^Rows 1–/)).toHaveCount(0);

    older = false;
    await unsorted.getByRole("button", { name: "Retry loading issues" }).click();
    const closed = await listIssuesWhere({ project: ACME, status: ["done"] });
    await expect(done).toHaveAttribute("aria-label", tabName("Done", closed.length));
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await expect(unsorted).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});

// R2, found by INFRA-3221's tester: the open tab counts the rows the table lists. While the page
// read of a newly picked Closed window is out, Done shows no count of the window before it beside
// the rows it lists.
// SLOW(>=2s): ACME seeded, and Done narrowed to one day while that page read is held.
test("while a newly picked Closed window's page read is out, Done's tab counts no rows the table does not list", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const table = issuesTable(page);
    await page.goto(`/projects/${ACME}/issues?view=done`);
    await expect(table.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    const { promise: gate, resolve: release } = Promise.withResolvers<void>();
    const { promise: asked, resolve: ask } = Promise.withResolvers<void>();
    await page.route(/\/api\/v1\/issues\?.*closed_since=/, async (route) => {
      ask();
      await gate;
      await route.continue();
    });
    await page
      .getByRole("button", { name: /^Filter Closed/ })
      .first()
      .click();
    await page.getByRole("button", { exact: true, name: "1 day" }).click();
    await asked;
    const listed = (await rowKeys(table)).length;
    expect([tabName("Done", undefined, true), tabName("Done", listed, true)]).toContain(
      await page.getByRole("tab", { name: /^Done/ }).getAttribute("aria-label")
    );
    release();
    const day = await listIssuesWhere({
      project: ACME,
      status: ["done"],
      closed_since: new Date(Date.now() - DAY).toISOString(),
    });
    await expect(page.getByRole("tab", { name: /^Done/ })).toHaveAttribute(
      "aria-label",
      tabName("Done", day.length, true)
    );
  } finally {
    await context.close();
    await daemon.close();
  }
});

// k6 (R2 and the Design's Loading), found by INFRA-3221's tester: the list route names in a page's
// `sort` member the sort it applied, and names none where it was asked for none. Done draws nothing
// of a page whose answer names no sort, no row and no count at any moment from the page's open to
// the alert; it reads that page three times, each read 500 ms or more after the one before, as the
// shared retry policy waits (`queryRetryDelay`), and then says the server did not sort it. A
// windowed read whose rows lose `closed_at` lists no row and shows no alert.
// SLOW(>=2s): ACME seeded, the list route read six times, and Done opened twice over answers this
// row rewrites, the first through three reads 500 ms apart.
test("the list route names the sort it applied, Done draws nothing of an unsorted page before its alert after three reads 500 ms apart, and a windowed read that loses closed_at lists no row and no alert", async ({
  browser,
}) => {
  const daemon = await seedAcme();
  const context = await asUser(browser, "alice");
  try {
    // Each answer names the sort it applied, and the answer to no sort names none.
    for (const sort of ["-closed_at", "closed_at"]) {
      for (const offset of [0, 50]) {
        const read = await listPage(`status=done&sort=${sort}&limit=25&offset=${offset}`);
        expect([read.status, read.body.sort], `sort=${sort} at offset ${offset}`).toEqual([
          200,
          sort,
        ]);
      }
    }
    const refused = await listPage("status=done&sort=title&limit=25&offset=0");
    expect([refused.status, refused.body.code]).toEqual([400, "INVALID_QUERY"]);
    const own = await listPage("status=done&limit=25&offset=0");
    expect(own.status).toBe(200);
    expect(Object.hasOwn(own.body, "sort"), "the answer to no sort names no sort").toBe(false);

    // Done over page answers with their sort taken out, watched from the page's open.
    const page = await context.newPage();
    const starts: number[] = [];
    let older = true;
    await page.route(/\/api\/v1\/issues\?.*sort=-closed_at/, async (route) => {
      if (!older) return route.continue();
      starts.push(Date.now());
      const response = await route.fetch();
      const body: { sort?: string } = await response.json();
      delete body.sort;
      await route.fulfill({ response, json: body });
    });
    // Every distinct drawn state: the rows the table lists and the Done tab's accessible name.
    await page.addInitScript(() => {
      const seen: string[] = [];
      Object.assign(window, { __doneStates: seen });
      new MutationObserver(() => {
        const rows = document.querySelectorAll("tbody tr[data-issue-row]").length;
        const done = [...document.querySelectorAll('[role="tab"]')].find((tab) =>
          (tab.getAttribute("aria-label") ?? "").startsWith("Done")
        );
        const state = `${rows}|${done?.getAttribute("aria-label") ?? ""}`;
        if (seen.at(-1) !== state) seen.push(state);
      }).observe(document, { attributes: true, childList: true, subtree: true });
    });
    await page.goto(`/projects/${ACME}/issues?view=done`);
    const unsorted = page
      .getByRole("alert")
      .filter({ hasText: "The server did not sort the closed issues by close time." });
    await expect(unsorted).toBeVisible();
    const states = await page.evaluate(
      () => (window as unknown as { __doneStates: string[] }).__doneStates
    );
    expect(
      states.filter((state) => state !== "0|" && state !== `0|${tabName("Done")}`),
      "a drawn row or a Done count before the alert"
    ).toEqual([]);
    expect(starts, "the reads of the page before the alert").toHaveLength(3);
    expect(
      starts.slice(1).map((start, index) => start - (starts[index] ?? 0) >= 500),
      "each read 500 ms or more after the one before"
    ).toEqual([true, true]);
    const issuesTableHere = issuesTable(page);
    await expect(issuesTableHere.locator("tbody tr[data-issue-row]")).toHaveCount(0);
    await expect(page.getByText(/^Rows 1–/)).toHaveCount(0);

    // A server that applies the sort answers Retry with the page and its count.
    older = false;
    await unsorted.getByRole("button", { name: "Retry loading issues" }).click();
    const closed = await listIssuesWhere({ project: ACME, status: ["done"] });
    await expect(page.getByRole("tab", { name: /^Done/ })).toHaveAttribute(
      "aria-label",
      tabName("Done", closed.length)
    );
    await expect(issuesTableHere.locator("tbody tr[data-issue-row]").first()).toBeVisible();
    await expect(unsorted).toHaveCount(0);

    // A windowed read whose rows lose closed_at: no row and no alert under the tabs.
    const windowed = await context.newPage();
    let stripped = 0;
    await windowed.route(/\/api\/v1\/issues\?.*closed_since=/, async (route) => {
      const response = await route.fetch();
      const body: unknown = await response.json();
      const rows = Array.isArray(body)
        ? body
        : ((body as { issues?: Record<string, unknown>[] | null }).issues ?? []);
      for (const row of rows as Record<string, unknown>[]) delete row.closed_at;
      stripped += rows.length;
      await route.fulfill({ response, json: body });
    });
    await windowed.goto(`/projects/${ACME}/issues?view=done&f.closed=28d&sort=-priority`);
    await expect(windowed.getByText("No issues match these filters.")).toBeVisible();
    expect(stripped, "the windowed read's rows the route answered").toBeGreaterThan(0);
    await expect(issuesTable(windowed).locator("tbody tr[data-issue-row]")).toHaveCount(0);
    await expect(
      windowed
        .getByRole("alert")
        .filter({ hasText: /Could not load|did not sort|closed issues|Could not read/ })
    ).toHaveCount(0);
  } finally {
    await context.close();
    await daemon.close();
  }
});
