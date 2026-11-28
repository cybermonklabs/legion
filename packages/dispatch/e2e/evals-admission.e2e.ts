/** The admission eval on the Evals tab, check rows k25 and k28 (dispatch://INFRA-2918/artifact/
 *  plan-md, D21 to D23, T42), on the local stack with Dispatch's Legion intake: a stub daemon
 *  (e2e/legion-daemon-stub.ts) answers the project's state_url and a state lane fed over the
 *  harness's NATS (e2e/legion-state-feed.ts) carries its states. The rows stay on the real stack
 *  because the server decides every number they read: the intake reads the daemon whole and applies
 *  its state messages from its durable, registers the eval and writes each admission reading once
 *  by its row key, the items route keeps what the read rule (M4) keeps at a view's buckets and the
 *  reading before each range, the live hub answers the bounded views and the header's daemon read,
 *  and its socket refuses a view past its limit. */
import { spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";

import type { EvalItemsResponse, LegionReadResponse, LiveView } from "../web/src/api/types";
import { baseUrl, createProject, userHeaders } from "./api";
import {
  chartFigure,
  getJson,
  type LiveRecord,
  listReadings,
  logReadings,
  openTab,
  postReadings,
  type ReadingInput,
  readEvalDetail,
  recordLive,
  showChart,
  snapshotsOf,
  subscribedViews,
  windowQuery,
} from "./evals-seed";
import { evalsSection, paintedSection } from "./evals-tab";
import {
  type CapChange,
  capChange,
  type DaemonActor,
  type DaemonStore,
  type LegionDaemonStub,
  type SignalChange,
  signalChange,
  startLegionDaemonStub,
} from "./legion-daemon-stub";
import { openStateFeed, requireNats, type StateFeed } from "./legion-state-feed";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";
import { sourceFiles } from "./source-files";
import { type BrowserContext, expect, type Locator, type Page, test } from "./test";

/** The slug of each project's admission eval, which the intake registers. */
const SLUG = "admission";

const HOUR_MS = 3_600_000;
const DAY_MS = 24 * HOUR_MS;

/** k25's project and k28's: synthetic keys, no deployment's. */
const CAP_PROJECT = "ADMIT";
const SIGNALS_PROJECT = "ADMITS";

/** How long the intake has to take a starting point or a whole read: a read it calls for while
 *  the newest began less than a minute before waits for that minute (`ReadRetryAfter`), and its
 *  message comes back from the stream when the minute ends. */
const READ_WAIT_MS = 150_000;

/** How long a tab of the worst rate's readings has to draw its charts and answer its views. */
const DRAW_WAIT_MS = 120_000;

function iso(ms: number): string {
  return new Date(ms).toISOString();
}

/** Today's first UTC moment, where each row's windows end: a window of whole UTC days, as the
 *  server reads an eval's window (`evalDaysOf`, api/evals.go), whose every reading is past. */
function lastMidnight(): number {
  return Math.floor(Date.now() / DAY_MS) * DAY_MS;
}

// ---------------------------------------------------------------------------------------------
// The project, its daemon and its intake.

/** `PUT /api/v1/projects/{key}/legion/daemon` as alice: names `stateUrl` as `project`'s daemon,
 *  which wakes the intake, whose starting point reads the daemon whole. */
async function nameDaemon(project: string, stateUrl: string): Promise<void> {
  const named = await fetch(
    new URL(`/api/v1/projects/${encodeURIComponent(project)}/legion/daemon`, baseUrl),
    {
      body: JSON.stringify({ state_url: stateUrl }),
      headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
      method: "PUT",
    }
  );
  const answer = await named.text();
  expect(named.status, `naming ${project}'s daemon: ${answer}`).toBe(200);
}

/** The project's newest daemon read as the header's view answers it
 *  (`GET /api/v1/projects/{key}/legion/read`). */
function daemonRead(project: string): Promise<LegionReadResponse> {
  return getJson<LegionReadResponse>(`/api/v1/projects/${encodeURIComponent(project)}/legion/read`);
}

/** The sequence the intake has applied the project's state events to, null before its first. */
async function appliedSeq(project: string): Promise<number | null> {
  const seq = (
    await sql(`SELECT last_seq FROM legion_event_cursors WHERE project = ${sqlLiteral(project)}`)
  ).trim();
  return seq === "" ? null : Number(seq);
}

/** How many standing readings the project's admission eval holds. */
async function readingCount(project: string): Promise<number> {
  const count = await sql(
    `SELECT count(*) FROM eval_readings WHERE eval_id = ${sqlLiteral(`${project}/${SLUG}`)} AND superseded_at IS NULL`
  );
  return Number(count.trim());
}

/** Creates `project`, starts its stub daemon serving `store` and names it, waits for the intake's
 *  starting read, which registers the admission eval (the store serves both records), and opens
 *  the project's state lane at the store's sequence. */
async function daemonProject(
  project: string,
  name: string,
  store: DaemonStore
): Promise<{ feed: StateFeed; stub: LegionDaemonStub }> {
  await createProject({ key: project, name });
  const stub = await startLegionDaemonStub(project, store);
  await nameDaemon(project, stub.url);
  await expect
    .poll(
      async () => {
        const read = await daemonRead(project);
        return read.ok && read.read_at !== null;
      },
      { message: `the intake's starting read of ${project}'s daemon`, timeout: READ_WAIT_MS }
    )
    .toBe(true);
  await expect
    .poll(async () => (await readEvalDetail(project, SLUG)).status, {
      message: `the intake registers ${project}/${SLUG}`,
      timeout: READ_WAIT_MS,
    })
    .toBe(200);
  const feed = await openStateFeed({
    applied: () => appliedSeq(project),
    project,
    run: store.stateRun,
    seq: store.stateSeq,
    stateId: store.stateId,
  });
  return { feed, stub };
}

// ---------------------------------------------------------------------------------------------
// What a step chart draws.

/** One value a chart's graph draws: a dot, named as the table names the value (`valueNames`,
 *  PlotRenderer.tsx), where it stands and its tip. */
interface DrawnPoint {
  readonly series: string;
  readonly range: string;
  readonly x: string;
  readonly value: number;
  readonly cx: number;
  readonly cy: number;
  readonly title: string;
}

/** One run of a step line (`data-step-line`): its series, its range and its path. */
interface DrawnLine {
  readonly series: string;
  readonly range: string;
  readonly d: string;
}

interface DrawnPlot {
  readonly points: readonly DrawnPoint[];
  readonly lines: readonly DrawnLine[];
}

/** Every dot and every step line `figure`'s graph draws, read at once. */
function drawnPlot(figure: Locator): Promise<DrawnPlot> {
  return figure.evaluate((root) => ({
    lines: [...root.querySelectorAll("[data-series-group] path[data-step-line]")].map((path) => ({
      d: path.getAttribute("d") ?? "",
      range: path.getAttribute("data-step-line") ?? "",
      series: path.closest("[data-series-group]")?.getAttribute("data-series-group") ?? "",
    })),
    points: [
      ...root.querySelectorAll('[data-series-points] circle[data-statistic="y"][data-value]'),
    ].map((dot) => ({
      cx: Number(dot.getAttribute("cx")),
      cy: Number(dot.getAttribute("cy")),
      range: dot.getAttribute("data-range") ?? "",
      series: dot.getAttribute("data-series") ?? "",
      title: dot.querySelector("title")?.textContent ?? "",
      value: Number(dot.getAttribute("data-value")),
      x: dot.getAttribute("data-x") ?? "",
    })),
  }));
}

/** Every value `figure`'s table holds, as `series|range|x|value`: its window's and its compare
 *  range's, each named as the graph names the value it draws. */
function tabledValues(figure: Locator): Promise<string[]> {
  return figure
    .locator(
      'table [data-statistic="y"][data-value]:is([data-range="window"], [data-range="compare"])'
    )
    .evaluateAll((cells) =>
      cells.map(
        (cell) =>
          `${cell.getAttribute("data-series")}|${cell.getAttribute("data-range")}|${cell.getAttribute("data-x")}|${cell.getAttribute("data-value")}`
      )
    );
}

/** The holds of a step line's path (`stepPath`, PlotRenderer.tsx): `M x,y H x`, then `V y H x` for
 *  each next hold, each as [from, to, y] in the plot's units. */
function holdsOf(d: string): [number, number, number][] {
  const holds: [number, number, number][] = [];
  let x = 0;
  let y = 0;
  for (const [, command, first, second] of d.matchAll(
    /([MHV])\s*(-?[\d.e+-]+)(?:,(-?[\d.e+-]+))?/g
  )) {
    const value = Number(first);
    if (command === "M") {
      x = value;
      y = Number(second);
    } else if (command === "V") {
      y = value;
    } else {
      holds.push([x, value, y]);
      x = value;
    }
  }
  return holds;
}

/** The window's axis as a plot draws it: the moment a place across stands for, and the
 *  milliseconds one unit across holds. */
interface Axis {
  readonly toMs: (place: number) => number;
  readonly msPerUnit: number;
}

/** `plot`'s axis, read off its two dots furthest apart in time among those measured in [from, to]
 *  (a reading before a range stands at the window's start, so it names no place of its own); none
 *  where fewer than two such dots stand apart. */
function axisOf(plot: DrawnPlot, from: number, to: number): Axis | undefined {
  const placed = plot.points
    .map((point) => ({ at: Date.parse(point.x), cx: point.cx }))
    .filter((point) => point.at >= from && point.at <= to)
    .sort((a, b) => a.at - b.at);
  const first = placed[0];
  const last = placed.at(-1);
  if (first === undefined || last === undefined || last.cx === first.cx) return undefined;
  const msPerUnit = (last.at - first.at) / (last.cx - first.cx);
  return { msPerUnit, toMs: (place) => first.at + (place - first.cx) * msPerUnit };
}

/** Fails the row on a plot with no axis to read its line against (`axisOf`). */
function noAxis(from: number, to: number): never {
  throw new Error(`the plot draws fewer than two readings between ${iso(from)} and ${iso(to)}`);
}

/** One hold of a step line: the reading it holds (the place its dot names and its value) and where
 *  it starts and ends on the window's axis, in epoch milliseconds. */
interface Hold {
  readonly x: string;
  readonly value: number;
  readonly from: number;
  readonly to: number;
}

/** The step line of `series` in `range`: each run, each hold of it starting at a drawn dot. */
function runsOf(
  plot: DrawnPlot,
  series: string,
  range: string,
  toMs: (place: number) => number
): Hold[][] {
  return plot.lines
    .filter((line) => line.series === series && line.range === range)
    .map((line) =>
      holdsOf(line.d).map(([from, to, y]) => {
        const dot = plot.points.find(
          (point) =>
            point.series === series &&
            point.range === range &&
            Math.abs(point.cx - from) < 1e-6 &&
            Math.abs(point.cy - y) < 1e-6
        );
        return { from: toMs(from), to: toMs(to), value: dot?.value ?? Number.NaN, x: dot?.x ?? "" };
      })
    );
}

/** `runs` beside `expected`, each end written as a time, and as the expected one's where it is
 *  within `tolerance` of it, so a difference reads as the time the line drew. */
function nearRuns(runs: readonly Hold[][], expected: readonly Hold[][], tolerance: number) {
  const written = (got: number, wanted: number | undefined) =>
    wanted !== undefined && Math.abs(got - wanted) <= tolerance
      ? iso(wanted)
      : Number.isFinite(got)
        ? iso(got)
        : String(got);
  return runs.map((run, index) =>
    run.map((hold, at) => {
      const want = expected[index]?.[at];
      return {
        from: written(hold.from, want?.from),
        to: written(hold.to, want?.to),
        value: hold.value,
        x: hold.x,
      };
    })
  );
}

function writtenRuns(runs: readonly Hold[][]) {
  return runs.map((run) =>
    run.map((hold) => ({ from: iso(hold.from), to: iso(hold.to), value: hold.value, x: hold.x }))
  );
}

// ---------------------------------------------------------------------------------------------
// The search of the page's code (plan D22).

const WEB_SOURCE = fileURLToPath(new URL("../web/src/", import.meta.url));

/** Every file under packages/dispatch/web/src, relative to it. */
function webSourceFiles(): string[] {
  return sourceFiles(WEB_SOURCE, { directory: () => true, file: () => true });
}

/** Each line of `files` that `pattern` finds, as `path:line: text`. */
function searchLines(files: readonly string[], pattern: RegExp): string[] {
  return files.flatMap((file) =>
    readFileSync(join(WEB_SOURCE, file), "utf8")
      .split("\n")
      .flatMap((line, index) =>
        pattern.test(line) ? [`${file}:${index + 1}: ${line.trim()}`] : []
      )
  );
}

/** A test file: no code of the page. */
const TEST_FILE = /\.test\.tsx?$/;

// ---------------------------------------------------------------------------------------------
// k25: the cap.

/** Who set a cap, as the daemon names them and as a note names them (`actorText`,
 *  evals/admission.go). */
function actorWords(actor: DaemonActor): string {
  return actor.session === undefined ? actor.kind : `${actor.kind} (session ${actor.session})`;
}

/** One cap change of k25: its id, when it was made, the cap before and after it, the ceiling, who
 *  set it and why. */
interface CapStep {
  readonly id: number;
  readonly at: number;
  readonly before: number;
  readonly after: number;
  readonly ceiling: number;
  readonly setBy: DaemonActor;
  readonly reason: string;
}

/** A cap change as a reading of the eval, the one the intake's writer gives an entry of
 *  `capChanges` (`capEntry`, evals/admission.go). */
function capReading(stateId: string, step: CapStep): ReadingInput {
  return {
    attributes: {
      cap_before: step.before,
      ceiling: step.ceiling,
      reason: step.reason,
      set_by: step.setBy,
    },
    measured_at: iso(step.at),
    note: `cap ${step.before} to ${step.after}, ceiling ${step.ceiling}, set by ${actorWords(step.setBy)}: ${step.reason}`,
    row_key: `${stateId}:${step.id}`,
    row_kind: "cap_change",
    series: "admission-cap",
    unit: "issues",
    value: step.after,
  };
}

// SLOW(>=2s): waits for the intake's starting read, logs five readings, feeds the state of hour 70, reads the chart three times, the last after a whole read that fails.
test("the cap draws a step line from the window's start, under the daemon read, and ends at a reading with no value @evals k25", async ({
  page,
}) => {
  requireNats();
  // The intake's starting read, the feed's state and the failed whole read, which the read rule
  // makes at most once a minute, each wait on the server; and the chart is read three times.
  test.setTimeout(8 * 60_000);
  const end = lastMidnight();
  const start = end - 72 * HOUR_MS;
  const hour = (hours: number) => start + hours * HOUR_MS;
  const store: DaemonStore = {
    cap: null,
    capChanges: [],
    signalChanges: [],
    stateId: randomUUID(),
    stateRun: randomUUID(),
    stateSeq: 40,
  };
  const { feed, stub } = await daemonProject(CAP_PROJECT, "Admission cap", store);
  try {
    // The readings, each logged in a call of its own under a key of its own: two before the window
    // (the compare range's start and its hour 70), the changes of hours 10 and 50, and the cap the
    // daemon's state reported at hour 60, which no change explains.
    const operator: DaemonActor = { kind: "operator" };
    const controller: DaemonActor = { kind: "controller", session: "ses_k25_controller" };
    const steps: CapStep[] = [
      {
        after: 2,
        at: start - 80 * HOUR_MS,
        before: 3,
        ceiling: 8,
        id: 1,
        reason: "the provider's outage began",
        setBy: operator,
      },
      {
        after: 4,
        at: start - 2 * HOUR_MS,
        before: 2,
        ceiling: 8,
        id: 2,
        reason: "the provider's outage is over",
        setBy: operator,
      },
      {
        after: 6,
        at: hour(10),
        before: 4,
        ceiling: 8,
        id: 3,
        reason: "the merge queue is empty",
        setBy: controller,
      },
      {
        after: 3,
        at: hour(50),
        before: 6,
        ceiling: 8,
        id: 4,
        reason: "the merge queue waits 52 minutes, above its limit of 45",
        setBy: controller,
      },
    ];
    const observed: ReadingInput = {
      attributes: { set_by: { kind: "daemon" } },
      measured_at: iso(hour(60)),
      note: "cap 5, as the daemon's state at sequence 39 reports it",
      row_key: `${store.stateId}:39`,
      row_kind: "cap_observed",
      series: "admission-cap",
      unit: "issues",
      value: 5,
    };
    const alice = await userHeaders("alice");
    const keys = new Map<number, string>();
    for (const step of steps) {
      const key = randomUUID();
      keys.set(step.id, key);
      const written = await postReadings(CAP_PROJECT, SLUG, [capReading(store.stateId, step)], {
        ...alice,
        "Idempotency-Key": key,
      });
      expect(written.status, `the change of ${iso(step.at)}: ${written.body}`).toBe(201);
    }
    await logReadings(CAP_PROJECT, SLUG, [observed]);
    expect(await readingCount(CAP_PROJECT)).toBe(5);

    // The call of hour 10 again, under its first key: the write answers it as before and adds no
    // reading.
    const hourTen = steps[2] as CapStep;
    const again = await postReadings(CAP_PROJECT, SLUG, [capReading(store.stateId, hourTen)], {
      ...alice,
      "Idempotency-Key": keys.get(hourTen.id) ?? "",
    });
    expect(again.status, `the repeated call of hour 10: ${again.body}`).toBe(201);
    expect(await readingCount(CAP_PROJECT), "the repeated call adds no row").toBe(5);

    // The daemon's state at hour 70: the cap 5 and no entry. The eval's newest cap is 5 already,
    // so the intake applies it and writes no reading.
    const seventy = await feed.admission(iso(hour(70)), {
      cap: 5,
      capChanges: [],
      signalChanges: [],
    });
    expect(seventy).toBe(store.stateSeq + 1);
    await feed.settled();
    expect(await readingCount(CAP_PROJECT), "the state of hour 70 adds no reading").toBe(5);

    // The first read: the chart as a graph and as a table, with no compare range.
    const window = { from: iso(start), to: iso(end) };
    await openTab(page, CAP_PROJECT, `${windowQuery(window)}&compare=off`);
    const logged = await paintedSection(page, "logged");
    const figure = chartFigure(logged.locator(`[data-declared="${SLUG}"]`), `${SLUG}/cap`);
    await expect(figure.locator('path[data-step-line="window"]')).toHaveCount(1);
    // Read once the line above is drawn: the readings it holds landed in one bounded view.
    const first = await drawnPlot(figure);
    const firstRead = Date.now();
    const axis = axisOf(first, start, end) ?? noAxis(start, end);
    const tolerance = 2 * Math.abs(axis.msPerUnit);
    const windowLine: Hold[][] = [
      [
        { from: start, to: hour(10), value: 4, x: iso(start - 2 * HOUR_MS) },
        { from: hour(10), to: hour(50), value: 6, x: iso(hour(10)) },
        { from: hour(50), to: hour(60), value: 3, x: iso(hour(50)) },
        { from: hour(60), to: end, value: 5, x: iso(hour(60)) },
      ],
    ];
    expect(
      nearRuns(runsOf(first, "admission-cap", "window", axis.toMs), windowLine, tolerance),
      "the line starts at the window's start at 4, steps at hours 10, 50 and 60 and holds 5 to the end"
    ).toEqual(writtenRuns(windowLine));

    // Each change's note, in the tip of its value: the cap before, the ceiling, who set it and why.
    const tips = steps.slice(1).map((step) => ({
      parts: [
        `cap ${step.before} to ${step.after}`,
        `ceiling ${step.ceiling}`,
        `set by ${actorWords(step.setBy)}`,
        step.reason,
      ],
      tip:
        first.points.find((point) => point.range === "window" && point.x === iso(step.at))?.title ??
        "",
    }));
    expect(
      tips.map(({ parts, tip }) => parts.filter((part) => !tip.includes(part))),
      "each change's note names the cap before, the ceiling, who set it and why"
    ).toEqual([[], [], []]);

    // The table: exactly four readings, the one before the window marked so, whose value starts
    // the line, and the three inside the window.
    await showChart(figure, "Table");
    const rows = figure.locator("[data-row]");
    await expect(rows).toHaveCount(4);
    const before = rows.filter({ hasText: "before the window" });
    await expect(before).toHaveCount(1);
    await expect(before.locator('[data-range="window"][data-value="4"]')).toHaveCount(1);
    await expect
      .poll(async () => (await tabledValues(figure)).sort())
      .toEqual(
        ["4", "6", "3", "5"]
          .map((value, index) => `admission-cap|window|${windowLine[0]?.[index]?.x}|${value}`)
          .sort()
      );
    await showChart(figure, "Graph");

    // The header gives the daemon read of hour 70, which succeeded.
    const header = page.locator("[data-daemon-read]");
    await expect(header).toHaveAttribute("data-red", "false");
    await expect(header).toContainText("Daemon read");
    await expect(header).not.toContainText("failed");
    await expect
      .poll(async () => Date.parse((await header.getAttribute("data-read-at")) ?? ""))
      .toBe(hour(70));

    // The compare range, the 72 hours before the window: its line starts at its start at 2,
    // steps to 4 at its hour 70 and holds 4 to its end, drawn on the window's axis.
    await page.getByLabel("Compare with").selectOption("previous");
    await expect(figure.locator('path[data-step-line="compare"]')).toHaveCount(1);
    // Read once the compare line above is drawn.
    const compared = await drawnPlot(figure);
    const compareAxis = axisOf(compared, start, end) ?? noAxis(start, end);
    const compareLine: Hold[][] = [
      [
        { from: start, to: hour(70), value: 2, x: iso(start - 80 * HOUR_MS + 72 * HOUR_MS) },
        { from: hour(70), to: end, value: 4, x: iso(hour(70)) },
      ],
    ];
    expect(
      nearRuns(
        runsOf(compared, "admission-cap", "compare", compareAxis.toMs),
        compareLine,
        tolerance
      ),
      "the compare line starts at its start at 2, steps to 4 at its hour 70 and holds 4 to its end"
    ).toEqual(writtenRuns(compareLine));
    expect(
      nearRuns(runsOf(compared, "admission-cap", "window", compareAxis.toMs), windowLine, tolerance)
    ).toEqual(writtenRuns(windowLine));

    // A reading with no value at hour 66, then a whole read that fails: the daemon answers 503,
    // and a state message past the next sequence calls for the read. Dispatch makes it at its own
    // clock (departure 9), after the chart's first read.
    await logReadings(CAP_PROJECT, SLUG, [
      {
        measured_at: iso(hour(66)),
        row_key: "k25-hour-66",
        row_kind: "cap_observed",
        series: "admission-cap",
        unit: "issues",
        value: null,
        value_absent_reason: "the daemon's state reported no cap",
      },
    ]);
    stub.answer(503);
    const failedFrom = Date.now();
    await feed.admission(
      iso(failedFrom),
      { cap: 5, capChanges: [], signalChanges: [] },
      feed.seq() + 2
    );
    await expect(header).toHaveAttribute("data-red", "true", { timeout: READ_WAIT_MS });
    await expect(header).toContainText("Daemon read failed");
    await expect(header).toContainText("503");
    await expect
      .poll(async () => Date.parse((await header.getAttribute("data-read-at")) ?? ""))
      .toBeGreaterThanOrEqual(failedFrom);
    expect(failedFrom).toBeGreaterThan(firstRead);
    expect(
      stub.reads().filter((read) => read.status === 503 && read.at >= failedFrom).length,
      "the stub answered the failed read"
    ).toBeGreaterThan(0);

    // After the last step the line ends at hour 66.
    const ended: Hold[][] = [
      [
        ...(windowLine[0] ?? []).slice(0, 3),
        { from: hour(60), to: hour(66), value: 5, x: iso(hour(60)) },
      ],
    ];
    await expect
      .poll(
        async () => {
          const drawn = await drawnPlot(figure);
          return nearRuns(
            runsOf(
              drawn,
              "admission-cap",
              "window",
              (axisOf(drawn, start, end) ?? noAxis(start, end)).toMs
            ),
            ended,
            tolerance
          );
        },
        { message: "the line ends at hour 66" }
      )
      .toEqual(writtenRuns(ended));

    // No code of the page names the eval.
    const files = webSourceFiles();
    const pageFiles = files.filter((file) => !TEST_FILE.test(file));
    expect(files.length, "the search read the page's files").toBeGreaterThan(0);
    expect(searchLines(files, /admission-cap/)).toEqual([]);
    expect(searchLines(pageFiles, /["'`]admission["'`]/)).toEqual([]);
    const inTests = searchLines(
      files.filter((file) => TEST_FILE.test(file)),
      /["'`]admission["'`]/
    );
    const read = {
      charts: [`${SLUG}/cap`],
      files: files.length,
      pageFiles: pageFiles.length,
      quotedInTests: inTests,
      readings: await readingCount(CAP_PROJECT),
      tableRows: 4,
    };
    test.info().annotations.push({ description: JSON.stringify(read), type: "k25" });
    console.log(`k25 read ${JSON.stringify(read)}`);
  } finally {
    await feed.close();
    await stub.close();
  }
});

// ---------------------------------------------------------------------------------------------
// k28: the signals.

/** A seeded random number in [0, 1), the same each run (mulberry32). */
function randomOf(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let mixed = state;
    mixed = Math.imul(mixed ^ (mixed >>> 15), mixed | 1);
    mixed ^= mixed + Math.imul(mixed ^ (mixed >>> 7), mixed | 61);
    return ((mixed ^ (mixed >>> 14)) >>> 0) / 4_294_967_296;
  };
}

function hundredths(value: number): number {
  return Math.round(value * 100) / 100;
}

/** One value series of the eval: its signal and part as the daemon's record names them, the `col`
 *  of its hold in admission-holds, its value entries on the median full day, and how its values
 *  and limits move. */
interface ValueSignal {
  readonly series: string;
  readonly signal: string;
  readonly part: string | null;
  readonly hold: string;
  readonly median: number;
  readonly value: (random: () => number) => number;
  readonly limit: (random: () => number) => number;
}

/** The five value series. `median` is each signal's value entries on the median of the six full
 *  days dispatch://INFRA-3199/artifact/value-entries-2026-11-25-md measured, at the steps INFRA-3199's
 *  spec v51 names (idle 0.05, queue 15 minutes, kills and approvals 5 at the passes of the live rule;
 *  headroom 0.10 at the passes that move a slot). */
const VALUE_SIGNALS: readonly ValueSignal[] = [
  {
    hold: "idle",
    limit: (random) => hundredths(0.3 + 0.5 * random()),
    median: 36.5,
    part: null,
    series: "admission-idle",
    signal: "idle",
    value: (random) => hundredths(random()),
  },
  {
    hold: "queue",
    limit: (random) => hundredths(2 + 6 * random()),
    median: 87.5,
    part: null,
    series: "admission-queue",
    signal: "queue",
    value: (random) => hundredths(12 * random()),
  },
  {
    hold: "provider.headroom",
    limit: (random) => hundredths(0.6 + 0.35 * random()),
    median: 98,
    part: "headroom",
    series: "admission-headroom",
    signal: "provider",
    value: (random) => hundredths(1.5 * random()),
  },
  {
    hold: "kills",
    limit: (random) => 2 + Math.floor(6 * random()),
    median: 11.5,
    part: null,
    series: "admission-kills",
    signal: "kills",
    value: (random) => Math.floor(13 * random()),
  },
  {
    hold: "approvals",
    limit: (random) => 5 + Math.floor(20 * random()),
    median: 27.5,
    part: null,
    series: "admission-approvals",
    signal: "approvals",
    value: (random) => Math.floor(31 * random()),
  },
];

/** The value entries of all five series on the median full day and on the worst full day of the
 *  same study (its range was 206 to 381). Both counted the refusals, which the eval keeps no
 *  series of (dispatch://INFRA-3199's spec v52), so each day's total is shared among the five value
 *  series by their medians above. */
const MEDIAN_DAY = 321;
const WORST_DAY = 381;

/** The days the seed covers: the window of 84 days and its compare range of 84 before it. */
const SEEDED_DAYS = 168;

/** `total` value entries shared among the five value series by their medians, rounded so they
 *  add up to `total` (the largest remainders take the units left). */
function valueCounts(total: number): number[] {
  const sum = VALUE_SIGNALS.reduce((all, signal) => all + signal.median, 0);
  const shares = VALUE_SIGNALS.map((signal) => (total * signal.median) / sum);
  const counts = shares.map(Math.floor);
  const order = shares
    .map((share, index) => ({ index, rest: share - Math.floor(share) }))
    .sort((a, b) => b.rest - a.rest);
  for (
    let left = total - counts.reduce((all, count) => all + count, 0), at = 0;
    left > 0;
    left--, at++
  ) {
    const index = order[at % order.length]?.index ?? 0;
    counts[index] = (counts[index] ?? 0) + 1;
  }
  return counts;
}

/** One reading the seed gave the eval, as the intake's writer writes it: its series and `col`,
 *  its row kind and key, when it was measured and its value (null for none). */
interface Seeded {
  readonly series: string;
  readonly col: string;
  readonly rowKind: string;
  readonly rowKey: string;
  readonly at: number;
  readonly value: number | null;
}

/** The reading a signal entry gives (`signalSeries`, evals/admission.go): a value or a limit in
 *  its signal's series under its `col`, a hold in admission-holds under its signal's, the switch in
 *  admission-switch under "". */
function signalReading(stateId: string, entry: SignalChange): Seeded {
  const signal = VALUE_SIGNALS.find(
    (one) => one.signal === entry.signal && one.part === entry.part
  );
  const [series, col] =
    entry.what === "switch"
      ? ["admission-switch", ""]
      : entry.what === "holds"
        ? ["admission-holds", signal?.hold ?? ""]
        : [signal?.series ?? "", entry.what];
  return {
    at: Date.parse(entry.at),
    col,
    rowKey: `${stateId}:${entry.id}`,
    rowKind: "signal_change",
    series,
    value: entry.value,
  };
}

/** The daemon's record of k28, built a stage at a time, and every reading it gives the eval. */
class SignalSeed {
  readonly readings: Seeded[] = [];
  private nextId = 0;
  private readonly taken = new Map<string, Set<number>>();

  constructor(
    private readonly stateId: string,
    private readonly end: number,
    private readonly random: () => number
  ) {}

  /** The next entry of the record, at `at`, written down as the reading it gives. */
  private entry(fields: Omit<SignalChange, "id">): SignalChange {
    this.nextId += 1;
    const entry = signalChange({ ...fields, id: this.nextId });
    const reading = signalReading(this.stateId, entry);
    const group = `${reading.series}\n${reading.col}`;
    const taken = this.taken.get(group) ?? new Set<number>();
    taken.add(reading.at);
    this.taken.set(group, taken);
    this.readings.push(reading);
    return entry;
  }

  /** A moment in the seeded days that no reading of `series` and `col` holds, half a second past a
   *  whole second, so no reading stands on the edge of a span of any `buckets`. */
  private freeMoment(series: string, col: string): number {
    const taken = this.taken.get(`${series}\n${col}`);
    const first = this.end - SEEDED_DAYS * DAY_MS;
    const seconds = (SEEDED_DAYS * DAY_MS) / 1000;
    for (;;) {
      const at = first + Math.floor(this.random() * seconds) * 1000 + 500;
      if (taken?.has(at) !== true) return at;
    }
  }

  /** One reading of each series and `col`, before both ranges: the cap's change, and each signal's
   *  value, limit and hold, and the switch. */
  oneReading(): { capChanges: CapChange[]; signalChanges: SignalChange[] } {
    const at = this.end - SEEDED_DAYS * DAY_MS - 2 * DAY_MS;
    const minute = (index: number) => iso(at + index * 60_000 + 500);
    const cap = capChange({
      cap: 4,
      capAfter: 4,
      capBefore: 3,
      ceiling: 8,
      id: 1,
      reason: "the owner set the cap for the quarter",
      setAt: minute(0),
      setBy: { kind: "operator" },
    });
    this.readings.push({
      at: Date.parse(cap.setAt),
      col: "",
      rowKey: `${this.stateId}:${cap.id}`,
      rowKind: "cap_change",
      series: "admission-cap",
      value: cap.capAfter,
    });
    const signals = VALUE_SIGNALS.flatMap((signal, index) => [
      this.entry({
        absent: null,
        at: minute(3 * index + 1),
        part: signal.part,
        reason: null,
        setBy: null,
        signal: signal.signal,
        value: signal.value(this.random),
        what: "value",
      }),
      this.entry({
        absent: null,
        at: minute(3 * index + 2),
        part: signal.part,
        reason: "the owner set the limit for the quarter",
        setBy: { kind: "operator" },
        signal: signal.signal,
        value: signal.limit(this.random),
        what: "limit",
      }),
      this.entry({
        absent: null,
        at: minute(3 * index + 3),
        part: signal.part,
        reason: null,
        setBy: null,
        signal: signal.signal,
        value: 0,
        what: "holds",
      }),
    ]);
    const on = this.entry({
      absent: null,
      at: minute(20),
      part: null,
      reason: "the owner turned admission by signals on",
      setBy: { kind: "operator" },
      signal: null,
      value: 1,
      what: "switch",
    });
    return { capChanges: [cap], signalChanges: [...signals, on] };
  }

  /** The median full day's value entries over the seeded days (MEDIAN_DAY a day), each signal's
   *  limit set and cleared, its hold taken and released, the switch turned off and on, and one
   *  value of the queue in the window with no value: oldest first. */
  median(): SignalChange[] {
    const first = this.end - SEEDED_DAYS * DAY_MS;
    const counts = valueCounts(MEDIAN_DAY * SEEDED_DAYS);
    const entries: SignalChange[] = [];
    VALUE_SIGNALS.forEach((signal, index) => {
      for (let made = 0; made < (counts[index] ?? 0); made++) {
        entries.push(this.value(signal, this.freeMoment(signal.series, "value")));
      }
      for (let change = 0; change < 12; change++) {
        const cleared = change % 2 === 1;
        entries.push(
          this.entry({
            absent: null,
            at: iso(first + (change + 0.5) * 14 * DAY_MS + index * 60_000 + 500),
            part: signal.part,
            reason: cleared
              ? "the owner cleared the limit"
              : "the owner set the limit in the weekly plan",
            setBy: { kind: "controller", session: "ses_k28_controller" },
            signal: signal.signal,
            value: cleared ? null : signal.limit(this.random),
            what: "limit",
          })
        );
      }
      for (let change = 0; change < 24; change++) {
        entries.push(
          this.entry({
            absent: null,
            at: iso(first + (change + 0.25) * 7 * DAY_MS + index * 60_000 + 500),
            part: signal.part,
            reason: null,
            setBy: null,
            signal: signal.signal,
            value: change % 2 === 0 ? 1 : 0,
            what: "holds",
          })
        );
      }
    });
    for (let change = 0; change < 6; change++) {
      const on = change % 2 === 1;
      entries.push(
        this.entry({
          absent: null,
          at: iso(first + (change + 0.6) * 28 * DAY_MS + 500),
          part: null,
          reason: on
            ? "the owner turned admission by signals on"
            : "the owner turned admission by signals off",
          setBy: { kind: "operator" },
          signal: null,
          value: on ? 1 : 0,
          what: "switch",
        })
      );
    }
    // The queue's value with no value, in the window: a quarter of a second past a whole second,
    // which no other value reading of the queue is (`freeMoment`), and no edge of a span either.
    const queue = VALUE_SIGNALS[1] as ValueSignal;
    entries.push(
      this.entry({
        absent: "the daemon has not begun keeping the merge queue's events",
        at: iso(this.end - 41 * DAY_MS - 3_037_000 + 250),
        part: queue.part,
        reason: null,
        setBy: null,
        signal: queue.signal,
        value: null,
        what: "value",
      })
    );
    return entries.sort((a, b) => Date.parse(a.at) - Date.parse(b.at) || a.id - b.id);
  }

  /** The value entries that raise the median day's to the worst day's (WORST_DAY a day), oldest
   *  first. */
  raise(): SignalChange[] {
    const median = valueCounts(MEDIAN_DAY * SEEDED_DAYS);
    const worst = valueCounts(WORST_DAY * SEEDED_DAYS);
    return VALUE_SIGNALS.flatMap((signal, index) =>
      Array.from({ length: (worst[index] ?? 0) - (median[index] ?? 0) }, () =>
        this.value(signal, this.freeMoment(signal.series, "value"))
      )
    ).sort((a, b) => Date.parse(a.at) - Date.parse(b.at) || a.id - b.id);
  }

  private value(signal: ValueSignal, at: number): SignalChange {
    return this.entry({
      absent: null,
      at: iso(at),
      part: signal.part,
      reason: null,
      setBy: null,
      signal: signal.signal,
      value: signal.value(this.random),
      what: "value",
    });
  }
}

/** One range a view reads: its name, its ends in epoch milliseconds and how far its readings move
 *  to stand on the window's axis. */
interface SpanRange {
  readonly name: "window" | "compare";
  readonly from: number;
  readonly to: number;
  readonly shift: number;
}

/** The readings the read rule keeps of one group's `readings` over `range` cut into `buckets`
 *  equal spans (plan D21; `bucketedReadingsIn`, api/evals_items.go): of each span's readings in
 *  time order, the first and the last, the one of the smallest and of the largest value (the
 *  earliest of equal ones), and the first with no value. */
function keptOf(readings: readonly Seeded[], range: SpanRange, buckets: number): Seeded[] {
  const spans = new Map<number, Seeded[]>();
  for (const reading of readings) {
    if (reading.at < range.from || reading.at >= range.to) continue;
    const span = Math.floor(((reading.at - range.from) * buckets) / (range.to - range.from));
    const held = spans.get(span) ?? [];
    held.push(reading);
    spans.set(span, held);
  }
  const kept = new Set<Seeded>();
  const extreme = (span: readonly Seeded[], sign: 1 | -1) =>
    span.reduce<Seeded | undefined>(
      (best, reading) =>
        reading.value !== null &&
        (best === undefined || sign * (reading.value - (best.value ?? 0)) > 0)
          ? reading
          : best,
      undefined
    );
  for (const span of spans.values()) {
    span.sort((a, b) => a.at - b.at);
    const picks = [
      span[0],
      span.at(-1),
      extreme(span, -1),
      extreme(span, 1),
      span.find((reading) => reading.value === null),
    ];
    for (const pick of picks) if (pick !== undefined) kept.add(pick);
  }
  return [...kept].sort((a, b) => a.at - b.at);
}

/** One chart of the eval: its id, the series it draws, and whether it draws a line for each `col`
 *  (the cap's chart names no group, and draws one). */
interface AdmissionChart {
  readonly id: string;
  readonly series: string;
  readonly grouped: boolean;
}

const ADMISSION_CHARTS: readonly AdmissionChart[] = [
  { grouped: false, id: "cap", series: "admission-cap" },
  { grouped: true, id: "idle", series: "admission-idle" },
  { grouped: true, id: "queue", series: "admission-queue" },
  { grouped: true, id: "headroom", series: "admission-headroom" },
  { grouped: true, id: "kills", series: "admission-kills" },
  { grouped: true, id: "approvals", series: "admission-approvals" },
  { grouped: true, id: "holds", series: "admission-holds" },
  { grouped: true, id: "switch", series: "admission-switch" },
];

/** The name a chart's series draws under (`data-series`, plot.ts): its y series, then its group. */
function drawnSeries(chart: AdmissionChart, col: string): string {
  return chart.grouped && col !== "" ? `${chart.series} \u00b7 ${col}` : chart.series;
}

/** One run of a step line as the rule draws it: the place of the reading it starts at (`data-x`)
 *  and where it ends on the window's axis, in epoch milliseconds. */
interface RunBounds {
  readonly start: string;
  readonly end: number;
}

/** What a step chart must draw (`expectedChart`): each value the read rule keeps, and each
 *  group's reading before each range, as `series|range|x|value`; every place the table names, as
 *  its `data-x`; and each group's runs in each range, keyed `series|range`. */
interface ExpectedChart {
  readonly dots: Set<string>;
  readonly places: Set<string>;
  readonly runs: Map<string, RunBounds[]>;
}

/** What a step chart must draw of `readings` over `ranges` at `buckets` (plan D21): a run starts at
 *  the reading before the range or at the first value after a reading with no value, and ends at
 *  the next reading with no value, or at the range's end on the window's axis. */
function expectedChart(
  chart: AdmissionChart,
  readings: readonly Seeded[],
  ranges: readonly SpanRange[],
  buckets: number
): ExpectedChart {
  const dots = new Set<string>();
  const places = new Set<string>();
  const runs = new Map<string, RunBounds[]>();
  const groups = new Map<string, Seeded[]>();
  for (const reading of readings) {
    if (reading.series !== chart.series) continue;
    const held = groups.get(reading.col) ?? [];
    held.push(reading);
    groups.set(reading.col, held);
  }
  for (const [col, group] of groups) {
    const name = drawnSeries(chart, col);
    for (const range of ranges) {
      const before = group
        .filter((reading) => reading.at < range.from)
        .reduce<Seeded | undefined>(
          (newest, reading) => (newest === undefined || reading.at > newest.at ? reading : newest),
          undefined
        );
      const line = [...(before === undefined ? [] : [before]), ...keptOf(group, range, buckets)];
      const bounds: { start: string; end: number }[] = [];
      let holding = false;
      for (const reading of line) {
        const x = iso(reading.at + range.shift);
        places.add(x);
        if (reading.value !== null) {
          dots.add(`${name}|${range.name}|${x}|${reading.value}`);
          if (!holding) bounds.push({ end: range.to + range.shift, start: x });
          holding = true;
        } else if (holding) {
          const run = bounds[bounds.length - 1];
          if (run !== undefined) run.end = reading.at + range.shift;
          holding = false;
        }
      }
      runs.set(`${name}|${range.name}`, bounds);
    }
  }
  return { dots, places, runs };
}

/** Where `chart` as drawn in `plot` differs from what the rule says it draws (`expectedChart`):
 *  each value drawn and not kept, kept and not drawn, and each group's runs, each by the reading it
 *  starts at and where it ends (read only where the plot has an axis to read it against,
 *  `axisOf`), at most 40 lines. Empty where they agree. */
function chartDifferences(
  chart: AdmissionChart,
  plot: DrawnPlot,
  expected: ExpectedChart,
  ranges: readonly SpanRange[]
): string[] {
  const drawn = new Set(
    plot.points
      .filter(
        (point) =>
          point.series === chart.series || point.series.startsWith(`${chart.series} \u00b7 `)
      )
      .map((point) => `${point.series}|${point.range}|${point.x}|${point.value}`)
  );
  const window = ranges.find((range) => range.name === "window") as SpanRange;
  const axis = axisOf(plot, window.from, window.to);
  const tolerance = axis === undefined ? 0 : 2 * Math.abs(axis.msPerUnit);
  const written = (runs: readonly RunBounds[]) =>
    runs.map(({ end, start }) => `${start} to ${Number.isFinite(end) ? iso(end) : "?"}`).join(", ");
  const runDifferences = [...expected.runs].flatMap(([key, bounds]) => {
    const [series = "", range = ""] = key.split("|");
    const got = plot.lines
      .filter((line) => line.series === series && line.range === range)
      .map((line): RunBounds => {
        const holds = holdsOf(line.d);
        const [from = Number.NaN, , y = Number.NaN] = holds[0] ?? [];
        const dot = plot.points.find(
          (point) =>
            point.series === series &&
            point.range === range &&
            Math.abs(point.cx - from) < 1e-6 &&
            Math.abs(point.cy - y) < 1e-6
        );
        const to = holds.at(-1)?.[1] ?? Number.NaN;
        return { end: axis === undefined ? Number.NaN : axis.toMs(to), start: dot?.x ?? "" };
      });
    const agrees =
      got.length === bounds.length &&
      got.every(
        (run, index) =>
          run.start === bounds[index]?.start &&
          (axis === undefined || Math.abs(run.end - (bounds[index]?.end ?? 0)) <= tolerance)
      );
    return agrees
      ? []
      : [`runs of ${key}: drawn ${written(got)}; the rule draws ${written(bounds)}`];
  });
  return [
    ...[...expected.dots].filter((dot) => !drawn.has(dot)).map((dot) => `kept, not drawn: ${dot}`),
    ...[...drawn].filter((dot) => !expected.dots.has(dot)).map((dot) => `drawn, not kept: ${dot}`),
    ...runDifferences,
  ].slice(0, 40);
}

/** The items views a tab opened for the eval up to `until`, each as its subscribe frame named it,
 *  and its subscription. */
function admissionViews(
  record: LiveRecord,
  until: number
): { sub: string; view: Extract<LiveView, { kind: "items" }> }[] {
  return record.sent.flatMap(({ at, frame }) =>
    frame.type === "subscribe" &&
    frame.view.kind === "items" &&
    frame.view.slug === SLUG &&
    at <= until
      ? [{ sub: frame.sub, view: frame.view }]
      : []
  );
}

/** The bounded view (`buckets`) of `series` the tab subscribed last. */
function boundedView(record: LiveRecord, series: string) {
  return subscribedViews(record.sent)
    .filter(
      (view) =>
        view.kind === "items" &&
        view.slug === SLUG &&
        view.query.get("series") === series &&
        view.query.has("buckets")
    )
    .at(-1);
}

/** What one open of the tab showed: the distinct items views it opened for the eval until every
 *  chart was drawn, and, read when asked, the socket's refusals and the compressed bytes of every
 *  items snapshot for the eval. */
interface Opened {
  readonly page: Page;
  readonly record: LiveRecord;
  readonly declared: Locator;
  readonly views: readonly string[];
  refused(): string[];
  bytes(): number;
}

/** Opens the project's tab at `query` in a page of its own, as the first reader after the seed,
 *  and waits for every chart of the eval to draw its window's line, and its compare range's where
 *  `compared`. */
async function openAdmission(
  context: BrowserContext,
  project: string,
  query: string,
  compared: boolean
): Promise<Opened> {
  const page = await context.newPage();
  const record = recordLive(page);
  await openTab(page, project, query);
  const logged = evalsSection(page, "logged");
  await expect(logged).toHaveAttribute("data-state", "painted", { timeout: DRAW_WAIT_MS });
  const declared = logged.locator(`[data-declared="${SLUG}"]`);
  for (const chart of ADMISSION_CHARTS) {
    const figure = chartFigure(declared, `${SLUG}/${chart.id}`);
    await expect(figure.locator('path[data-step-line="window"]').first(), chart.id).toBeAttached({
      timeout: DRAW_WAIT_MS,
    });
    if (compared) {
      await expect(figure.locator('path[data-step-line="compare"]').first(), chart.id).toBeAttached(
        {
          timeout: DRAW_WAIT_MS,
        }
      );
    }
  }
  const drawnAt = Date.now();
  return {
    bytes: () =>
      admissionViews(record, Number.POSITIVE_INFINITY).reduce(
        (sum, { sub }) =>
          sum +
          snapshotsOf(record.received, sub).reduce(
            (all, { frame }) => all + gzipSync(JSON.stringify(frame.body)).length,
            0
          ),
        0
      ),
    declared,
    page,
    record,
    refused: () =>
      record.received.flatMap(({ frame }) =>
        frame.type === "error" && frame.code === "TOO_MANY_SUBSCRIPTIONS" ? [frame.message] : []
      ),
    views: [...new Set(admissionViews(record, drawnAt).map(({ view }) => JSON.stringify(view)))],
  };
}

/** Holds that each chart of `opened` draws what the read rule keeps of `readings` at the buckets
 *  its series' bounded view names, once that view has answered. Answers each series' buckets. */
async function expectChartsKept(
  opened: Opened,
  readings: readonly Seeded[],
  ranges: readonly SpanRange[]
): Promise<Map<string, number>> {
  const buckets = new Map<string, number>();
  for (const chart of ADMISSION_CHARTS) {
    const figure = chartFigure(opened.declared, `${SLUG}/${chart.id}`);
    await expect
      .poll(
        async () => {
          const view = boundedView(opened.record, chart.series);
          if (view === undefined) return [`no bounded view of ${chart.series}`];
          if (snapshotsOf(opened.record.received, view.sub).length === 0) {
            return [`the bounded view of ${chart.series} has not answered`];
          }
          if (view.query.get("group") !== "col")
            return [`${chart.series} is read by ${view.query.get("group")}`];
          const count = Number(view.query.get("buckets"));
          buckets.set(chart.series, count);
          const plot = await drawnPlot(figure);
          return chartDifferences(
            chart,
            plot,
            expectedChart(chart, readings, ranges, count),
            ranges
          );
        },
        { message: `${chart.id} draws what the read rule keeps`, timeout: DRAW_WAIT_MS }
      )
      .toEqual([]);
  }
  return buckets;
}

/** Runs EVALS_K28_MEASURE_CMD, where the run names one, on `views` (each written to a file of its
 *  own, as `evalbench measure items -view-file` reads one) with `budgetMs`: the command restarts
 *  the seeded server and measures each items read's database time on the first read after the
 *  restart. Answers its output, or why nothing was measured. */
function measureItems(
  label: string,
  views: readonly Extract<LiveView, { kind: "items" }>[],
  budgetMs: number
): string {
  const command = process.env.EVALS_K28_MEASURE_CMD ?? "";
  const named = views.map((view) => JSON.stringify(view));
  if (command === "") {
    return `${label}: not measured, EVALS_K28_MEASURE_CMD is unset; the views: ${named.join(" ")}`;
  }
  const directory = mkdtempSync(join(tmpdir(), "k28-items-"));
  try {
    const files = named.map((view, index) => {
      const file = join(directory, `view-${index}.json`);
      writeFileSync(file, view);
      return file;
    });
    const run = spawnSync(
      "bash",
      ["-c", `${command} "$@"`, "k28-measure", String(budgetMs), ...files],
      {
        encoding: "utf8",
        // A restart of the server and one read of each view, each bounded by the command itself.
        timeout: 15 * 60_000,
      }
    );
    const output = `${run.stdout ?? ""}${run.stderr ?? ""}`;
    expect(run.status, `${label}: ${command} on ${files.length} views\n${output}`).toBe(0);
    return `${label}: ${output.trim()}`;
  } finally {
    rmSync(directory, { force: true, recursive: true });
  }
}

// SLOW(>=2s): feeds the intake 64,000 readings in messages of 500, and opens the tab four times, reading every chart as a graph and as a table and paging the rows to the end.
test("each signal draws its kept steps from one bounded view a series, whatever its readings @evals k28", async ({
  page,
}) => {
  requireNats();
  // The intake applies some 130 messages of 500 readings, one at a time, and the tab is opened four
  // times over them, each chart compared with the read rule and the worst rate's tables and rows
  // read whole.
  test.setTimeout(55 * 60_000);
  const end = lastMidnight();
  const window = { from: iso(end - 84 * DAY_MS), to: iso(end) };
  const ranges: SpanRange[] = [
    { from: end - 84 * DAY_MS, name: "window", shift: 0, to: end },
    { from: end - 168 * DAY_MS, name: "compare", shift: 84 * DAY_MS, to: end - 84 * DAY_MS },
  ];
  const store: DaemonStore = {
    cap: null,
    capChanges: [],
    signalChanges: [],
    stateId: randomUUID(),
    stateRun: randomUUID(),
    stateSeq: 70,
  };
  const { feed, stub } = await daemonProject(SIGNALS_PROJECT, "Admission signals", store);
  const record: { [stage: string]: unknown } = {};
  try {
    const seed = new SignalSeed(store.stateId, end, randomOf(28));
    // The 84 days and the compare range before them: the tab's default compare range.
    const query = windowQuery(window);

    // One reading of each series and col, before both ranges.
    const one = seed.oneReading();
    await feed.entries(4, one.capChanges, one.signalChanges);
    await feed.settled();
    const first = await openAdmission(page.context(), SIGNALS_PROJECT, query, true);
    await expectChartsKept(first, seed.readings, ranges);
    expect(first.refused(), "the socket refuses no view").toEqual([]);
    expect(first.views.length, "the tab opens a view for the eval").toBeGreaterThan(0);
    record.oneReading = {
      bytes: first.bytes(),
      readings: seed.readings.length,
      views: first.views.length,
    };
    await first.page.close();

    // The median day's readings over the 168 days.
    const median = seed.median();
    const medianMessages = await feed.entries(4, [], median);
    await feed.settled();
    const atMedian = await openAdmission(page.context(), SIGNALS_PROJECT, query, true);
    await expectChartsKept(atMedian, seed.readings, ranges);
    expect(atMedian.refused(), "the socket refuses no view at the median rate").toEqual([]);
    expect(atMedian.views.length, "the views do not grow with the readings").toBe(
      first.views.length
    );
    record.median = {
      bytes: atMedian.bytes(),
      messages: medianMessages,
      readings: seed.readings.length,
      values: seed.readings.filter((reading) => reading.series !== "admission-cap").length,
      views: atMedian.views.length,
    };
    await atMedian.page.close();

    // Raised to the worst day's.
    const raised = seed.raise();
    const worstMessages = await feed.entries(4, [], raised);
    await feed.settled();
    expect(await readingCount(SIGNALS_PROJECT), "every seeded reading is the eval's").toBe(
      seed.readings.length
    );
    const atWorst = await openAdmission(page.context(), SIGNALS_PROJECT, query, true);
    const buckets = await expectChartsKept(atWorst, seed.readings, ranges);
    expect(atWorst.refused(), "the socket refuses no view at the worst rate").toEqual([]);
    expect(atWorst.views.length, "the views do not grow with the readings").toBe(
      first.views.length
    );

    // Each chart as a table: every place the rule keeps, the readings before the window and before
    // the compare range among them, holding the values its graph draws.
    for (const chart of ADMISSION_CHARTS) {
      const figure = chartFigure(atWorst.declared, `${SLUG}/${chart.id}`);
      const expected = expectedChart(chart, seed.readings, ranges, buckets.get(chart.series) ?? 0);
      // Read once expectChartsKept above has seen the graph draw what the rule keeps.
      const graph = await drawnPlot(figure);
      await showChart(figure, "Table");
      await expect(figure.locator("[data-row]"), chart.id).toHaveCount(expected.places.size, {
        timeout: DRAW_WAIT_MS,
      });
      await expect(
        figure.locator("[data-row]").filter({ hasText: "before the window" }),
        chart.id
      ).not.toHaveCount(0);
      await expect(
        figure.locator("[data-row]").filter({ hasText: "before the compare range" }),
        chart.id
      ).not.toHaveCount(0);
      await expect
        .poll(async () => (await tabledValues(figure)).sort(), { message: `${chart.id}'s table` })
        .toEqual(
          graph.points
            .map((point) => `${point.series}|${point.range}|${point.x}|${point.value}`)
            .sort()
        );
      await showChart(figure, "Graph");
    }

    // The rows behind the eval's number: its panel reads the window's readings alone, with no
    // repository, a page at a time; the same read 200 at a time to its end holds every reading
    // seeded in the window.
    const inWindow = seed.readings.filter(
      (reading) => reading.at >= ranges[0].from && reading.at < ranges[0].to
    );
    await atWorst.declared.getByRole("button", { exact: true, name: "Admission" }).first().click();
    const panel = atWorst.page.locator(`[data-eval-panel="${SLUG}"]`);
    await expect(panel.getByText(/^Rows \d+ to \d+ of \d+$/)).toHaveText(
      `Rows 1 to 50 of ${inWindow.length}`,
      { timeout: DRAW_WAIT_MS }
    );
    await panel.getByRole("button", { name: "Later rows" }).click();
    await expect(panel.getByText(/^Rows \d+ to \d+ of \d+$/)).toHaveText(
      `Rows 51 to 100 of ${inWindow.length}`
    );
    const rowsView = subscribedViews(atWorst.record.sent).find(
      (view) =>
        view.kind === "items" && view.slug === SLUG && view.query.get("source") === "readings"
    );
    expect(rowsView?.query.has("repo"), "the rows name no repository").toBe(false);
    expect(rowsView?.query.has("compare_from"), "the rows read the window alone").toBe(false);
    const paged: string[] = [];
    let total = -1;
    for (let offset = 0; total < 0 || offset < total; ) {
      const rows: EvalItemsResponse = await listReadings(
        SIGNALS_PROJECT,
        SLUG,
        window,
        `limit=200&offset=${offset}`
      );
      const items = (rows.items ?? []).filter((row) => row.before_window !== true);
      total = rows.total;
      paged.push(...items.map((row) => `${row.row_kind}|${row.row_key}`));
      offset += 200;
      if (items.length === 0) break;
    }
    expect(total, "the rows count every reading seeded in the window").toBe(inWindow.length);
    expect(paged.sort(), "the rows page every reading seeded in the window").toEqual(
      inWindow.map((reading) => `${reading.rowKind}|${reading.rowKey}`).sort()
    );

    const measured = [
      measureItems(
        "84 days with a compare range",
        ADMISSION_CHARTS.flatMap((chart) => {
          const view = admissionViews(atWorst.record, Number.POSITIVE_INFINITY)
            .filter(
              ({ view: one }) => new URLSearchParams(one.query).get("series") === chart.series
            )
            .at(-1)?.view;
          return view === undefined ? [] : [view];
        }),
        150
      ),
    ];
    record.worst = {
      buckets: Object.fromEntries(buckets),
      bytes: atWorst.bytes(),
      messages: worstMessages,
      readings: seed.readings.length,
      rows: total,
      values: seed.readings.filter((reading) => reading.series !== "admission-cap").length,
      views: atWorst.views.length,
    };
    await atWorst.page.close();

    // 28 days with no compare range: its views, measured as the 84 days' were.
    const recent = await openAdmission(
      page.context(),
      SIGNALS_PROJECT,
      `${windowQuery({ from: iso(end - 28 * DAY_MS), to: iso(end) })}&compare=off`,
      false
    );
    expect(recent.refused(), "the socket refuses no view at 28 days").toEqual([]);
    measured.push(
      measureItems(
        "28 days with no compare range",
        admissionViews(recent.record, Number.POSITIVE_INFINITY).map(({ view }) => view),
        60
      )
    );
    record.recent = { bytes: recent.bytes(), views: recent.views.length };
    await recent.page.close();
    record.measured = measured;

    // No code of the page names the eval's signals.
    const files = webSourceFiles();
    expect(files.length, "the search read the page's files").toBeGreaterThan(0);
    expect(searchLines(files, /admission-queue|admission-holds|signalChanges/)).toEqual([]);
    record.files = files.length;
    record.charts = ADMISSION_CHARTS.map((chart) => `${SLUG}/${chart.id}`);
    record.stubReads = stub.reads().length;
    test.info().annotations.push({ description: JSON.stringify(record), type: "k28" });
    console.log(`k28 read ${JSON.stringify(record)}`);
  } finally {
    await feed.close();
    await stub.close();
  }
});
