/** The Evals tab's declared charts, check row k21 (dispatch://INFRA-2918/artifact/plan-md, D10,
 *  D21, T28): what the eval register stores of each eval's declared charts and answer, version by
 *  version, and refuses of a spec the eval platform's rules refuse or of a credential of another
 *  project; the readings the items route answers, bounded for a step chart by the read rule; and
 *  each declared chart the tab draws from them, compared with the readings alone. */
import { randomUUID } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import type { EvalDetailResponse, EvalItemRow } from "../web/src/api/types";
import { baseUrl, createProject, registerEval, userHeaders } from "./api";
import {
  chartFigure,
  evalsViews,
  getJson,
  listReadings,
  logReadings,
  NO_DAEMON,
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
import { paintedSection } from "./evals-tab";
import { fakeBrokerToken } from "./fake-broker-token";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";
import { expect, type Locator, test } from "./test";

/** The project every declared eval of these rows is written to. */
const DECLARED = "EVALK";

/** A whole UTC day in milliseconds. */
const DAY_MS = 24 * 60 * 60 * 1000;

/** The most days one window holds (`MAX_WINDOW_DAYS`, web/src/features/delivery/lib/window.ts). */
const MAX_DAYS = 84;

// ---------------------------------------------------------------------------------------------
// The documents k21 reads at run time.

/** How many evals the documents of EVALS_K21_DIR hold: the four check k21 names. */
const K21_EVALS = 4;

/** One chart declaration as the register write takes it. */
type ChartSpec = { [key: string]: unknown };

/** One eval's documents as the Job stages them: its charts (a list of declarations, or the
 *  register body that holds one as `charts`, with the eval's name and answer), and its readings
 *  (a list, the readings write's body, or the items route's answer), each read from its file. */
interface StagedEval {
  readonly slug: string;
  readonly register: {
    readonly charts: ChartSpec[];
    readonly name?: string;
    readonly answer?: string;
  };
  readonly readings: ReadingInput[];
}

/** The fields of a reading the readings write takes. */
const READING_FIELDS = [
  "attributes",
  "ci_high",
  "ci_level",
  "ci_low",
  "col",
  "links",
  "measured_at",
  "n",
  "n_unit",
  "note",
  "row_key",
  "row_kind",
  "series",
  "source",
  "unit",
  "value",
  "value_absent_reason",
] as const;

/** The fields of a reading that hold a number. */
const NUMBER_FIELDS: Readonly<Record<string, true>> = {
  ci_high: true,
  ci_level: true,
  ci_low: true,
  n: true,
  value: true,
};

/** A staged reading's fields: its own, and, where it is an items row, those under its `cols`. The
 *  eval platform names a reading's row `row` and the reason it has no value `no_value_reason`,
 *  which Dispatch's readings write names `row_key` and `value_absent_reason`; a reading that names
 *  no row kind is a point of its series. */
function fieldsOf(row: unknown): { readonly [key: string]: unknown } {
  if (typeof row !== "object" || row === null) return {};
  const cols = "cols" in row && typeof row.cols === "object" && row.cols !== null ? row.cols : {};
  const fields: { readonly [key: string]: unknown } = { ...cols, ...row };
  return {
    row_key: fields.row,
    row_kind: "point",
    value_absent_reason: fields.no_value_reason,
    ...fields,
  };
}

/** A staged link as the readings write takes it, or nothing where it is none. */
function linkOf(link: unknown): { href: string; label?: string; rel: string }[] {
  if (typeof link !== "object" || link === null || !("href" in link) || !("rel" in link)) return [];
  const { href, rel } = link;
  if (typeof href !== "string" || typeof rel !== "string") return [];
  const label = "label" in link && typeof link.label === "string" ? link.label : undefined;
  return [{ href, label, rel }];
}

/** A staged reading as the readings write takes it, and the id it was recorded under and the id
 *  it replaces, where it names them. */
function readingOf(row: unknown): { input: ReadingInput; id: string; replaces: string } {
  const fields = fieldsOf(row);
  // The fields are the staged reading's own, unread here: the readings write reads each and
  // refuses the batch naming the first it cannot take, which fails the row with its reason.
  const input = Object.fromEntries(
    READING_FIELDS.flatMap((field) => {
      const value = fields[field];
      if (value === undefined) return [];
      // A document may hold a number as its decimal text ("0.25"), which the write refuses.
      const text = NUMBER_FIELDS[field] === true && typeof value === "string" ? value.trim() : null;
      return [
        [
          field,
          text !== null && text !== "" && Number.isFinite(Number(text)) ? Number(text) : value,
        ],
      ];
    })
  ) as unknown as ReadingInput;
  return {
    id: typeof fields.id === "string" ? fields.id : "",
    input: {
      ...input,
      links: Array.isArray(fields.links) ? fields.links.flatMap(linkOf) : undefined,
    },
    replaces: typeof fields.replaces === "string" ? fields.replaces : "",
  };
}

/** The list a staged document holds under `key`, the document itself where it is a list. */
function listOf(document: unknown, ...keys: readonly string[]): unknown[] | undefined {
  if (Array.isArray(document)) return document;
  if (typeof document !== "object" || document === null) return undefined;
  for (const key of keys) {
    const list: unknown = Reflect.get(document, key);
    if (Array.isArray(list)) return list;
  }
  return undefined;
}

/** The readings of one staged document that still stand: none another names as the one it
 *  replaced, and none recorded as superseded. Only standing readings are drawn, and a replace
 *  names an id of the project the documents came from, so the writes here carry none. */
function standingReadings(document: unknown, file: string): ReadingInput[] {
  const rows = listOf(document, "readings", "items");
  if (rows === undefined) throw new Error(`${file} holds no list of readings`);
  const readings = rows.map(readingOf);
  const replaced = new Set(readings.map((reading) => reading.replaces).filter((id) => id !== ""));
  return rows.flatMap((row, index) => {
    const reading = readings[index];
    const superseded = fieldsOf(row).superseded_at;
    if (reading === undefined || (superseded !== undefined && superseded !== null)) return [];
    return replaced.has(reading.id) ? [] : [reading.input];
  });
}

/** The four evals of EVALS_K21_DIR, each named by its files, `charts-<slug>.json` and
 *  `readings-<slug>.json`: the slugs are read from the names, so none is written here. The
 *  directory unset, or missing a document, fails the row by name: it never passes unread. */
function stagedEvals(): StagedEval[] {
  const directory = process.env.EVALS_K21_DIR ?? "";
  if (directory === "") {
    throw new Error(
      "EVALS_K21_DIR is unset: k21 reads the four evals' chart and reading documents the Job stages there (charts-<slug>.json and readings-<slug>.json)"
    );
  }
  const files = readdirSync(directory);
  const slugs = files.flatMap((file) => /^charts-(.+)\.json$/u.exec(file)?.slice(1, 2) ?? []);
  expect(slugs, `the chart documents of EVALS_K21_DIR among ${files.length} files`).toHaveLength(
    K21_EVALS
  );
  return slugs.map((slug) => {
    const chartsFile = `charts-${slug}.json`;
    const readingsFile = `readings-${slug}.json`;
    const document: unknown = JSON.parse(readFileSync(join(directory, chartsFile), "utf8"));
    const charts = listOf(document, "charts");
    if (charts === undefined) throw new Error(`${chartsFile} declares no list of charts`);
    const { answer, name } = fieldsOf(Array.isArray(document) ? {} : document);
    return {
      readings: standingReadings(
        JSON.parse(readFileSync(join(directory, readingsFile), "utf8")),
        readingsFile
      ),
      register: {
        answer: typeof answer === "string" ? answer : undefined,
        charts: charts.flatMap((chart) =>
          typeof chart === "object" && chart !== null ? [{ ...chart }] : []
        ),
        name: typeof name === "string" ? name : undefined,
      },
      slug,
    };
  });
}

/** The window of whole UTC days that holds `readings`, at most MAX_DAYS ending the day after the
 *  newest. */
function windowOf(readings: readonly ReadingInput[]): { from: string; to: string } {
  const times = readings.map((reading) => Date.parse(reading.measured_at));
  const newest = Math.max(...times);
  const to = Math.floor(newest / DAY_MS) * DAY_MS + DAY_MS;
  const from = Math.max(Math.floor(Math.min(...times) / DAY_MS) * DAY_MS, to - MAX_DAYS * DAY_MS);
  return { from: new Date(from).toISOString(), to: new Date(to).toISOString() };
}

/** Writes `readings` of `slug` a batch at a time, each batch under a key of its own. */
async function writeReadings(slug: string, readings: readonly ReadingInput[]): Promise<void> {
  for (let start = 0; start < readings.length; start += 100) {
    await logReadings(DECLARED, slug, readings.slice(start, start + 100));
  }
}

// ---------------------------------------------------------------------------------------------
// The reference: what the readings alone say each chart draws.

/** Every reading the items route answers of `slug`'s `series` (every series where empty) over
 *  `window`, readings alone, page by page: the window's, and each group's newest before it that a
 *  step chart starts at (`before_window`, plan D20). */
async function readingsOf(
  slug: string,
  window: { from: string; to: string },
  series: string
): Promise<EvalItemRow[]> {
  const rows: EvalItemRow[] = [];
  for (let offset = 0; ; ) {
    const page = await listReadings(
      DECLARED,
      slug,
      window,
      `limit=200&offset=${offset}${series === "" ? "" : `&series=${encodeURIComponent(series)}`}`
    );
    const items = page.items ?? [];
    rows.push(...items);
    offset += items.filter((row) => row.before_window !== true).length;
    if (items.length === 0 || offset >= page.total) return rows;
  }
}

/** A number column of an items row, null where it holds none. */
function numberOf(row: EvalItemRow, column: string): number | null {
  const value = row.cols?.[column];
  return typeof value === "number" ? value : null;
}

/** Every number the readings `rows` hold: each one's value, count, interval, and each number of
 *  its attributes. A declared chart draws nothing a reading does not hold. */
function numbersOf(rows: readonly EvalItemRow[]): Set<string> {
  const numbers = new Set<string>();
  for (const row of rows) {
    for (const column of ["value", "n", "ci_low", "ci_high"]) {
      const value = numberOf(row, column);
      if (value !== null) numbers.add(String(value));
    }
    const attributes = row.cols?.attributes;
    if (typeof attributes === "object" && attributes !== null) {
      for (const value of Object.values(attributes)) {
        if (typeof value === "number") numbers.add(String(value));
      }
    }
  }
  return numbers;
}

/** The group of a step chart's reading (plan D20 (a)): its `col`, its row or its row's group. */
function groupOf(row: EvalItemRow, group: string): string {
  if (group === "row") return row.row_key;
  if (group === "row_group") {
    const attributes: unknown = row.cols?.attributes;
    return typeof attributes === "object" && attributes !== null
      ? String(Reflect.get(attributes, "row_group") ?? "")
      : "";
  }
  return String(row.cols?.col ?? "");
}

/** The values the read rule keeps of a step series' window readings (plan D21, M4): the window
 *  split into `buckets` equal spans of time, and of each span's readings of one group, ordered by
 *  their time then id, the first and the last, the smallest and the largest value, the first with
 *  no value, and for a band the smallest `ci_low` and the largest `ci_high`; with each group's
 *  reading before the window, where the line starts. Each kept value, as the line draws it. */
function keptValues(
  rows: readonly EvalItemRow[],
  window: { from: string; to: string },
  buckets: number,
  group: string,
  band: boolean
): string[] {
  const from = Date.parse(window.from);
  const width = (Date.parse(window.to) - from) / buckets;
  const spans = new Map<string, EvalItemRow[]>();
  for (const row of rows) {
    if (row.before_window === true) continue;
    const span = Math.min(buckets - 1, Math.floor((Date.parse(row.measured_at) - from) / width));
    const key = `${groupOf(row, group)}\n${span}`;
    spans.set(key, [...(spans.get(key) ?? []), row]);
  }
  const kept = new Set<EvalItemRow>(rows.filter((row) => row.before_window === true));
  const order = (a: EvalItemRow, b: EvalItemRow) =>
    Date.parse(a.measured_at) - Date.parse(b.measured_at) ||
    String(a.cols?.id ?? "").localeCompare(String(b.cols?.id ?? ""));
  const extreme = (span: readonly EvalItemRow[], column: string, sign: 1 | -1) =>
    span
      .filter((row) => numberOf(row, column) !== null)
      .reduce<EvalItemRow | undefined>(
        (best, row) =>
          best === undefined ||
          sign * ((numberOf(row, column) ?? 0) - (numberOf(best, column) ?? 0)) > 0
            ? row
            : best,
        undefined
      );
  for (const span of spans.values()) {
    const ordered = [...span].sort(order);
    const picks = [
      ordered[0],
      ordered.at(-1),
      extreme(ordered, "value", -1),
      extreme(ordered, "value", 1),
      ordered.find((row) => numberOf(row, "value") === null),
      ...(band ? [extreme(ordered, "ci_low", -1), extreme(ordered, "ci_high", 1)] : []),
    ];
    for (const pick of picks) if (pick !== undefined) kept.add(pick);
  }
  return [
    ...new Set(
      [...kept].flatMap((row) => {
        const value = numberOf(row, "value");
        return value === null ? [] : [String(value)];
      })
    ),
  ].sort();
}

/** The series a declaration reads by the step rule (`stepReads`, web/src/features/evals/charts/
 *  plot.ts): an `xy` or a `band` with `curve: "step"` over measured time (no `x.series`), and not
 *  points alone; none for any other. */
function stepSeries(spec: ChartSpec): string[] {
  const xSeries: unknown =
    typeof spec.x === "object" && spec.x !== null ? Reflect.get(spec.x, "series") : undefined;
  if (
    (spec.kind !== "xy" && spec.kind !== "band") ||
    spec.curve !== "step" ||
    typeof xSeries === "string" ||
    spec.points === "only"
  ) {
    return [];
  }
  const seriesOf = (part: unknown): unknown =>
    typeof part === "object" && part !== null ? Reflect.get(part, "series") : undefined;
  const options: unknown[] = Array.isArray(spec.y_options) ? spec.y_options : [];
  return [seriesOf(spec.y), ...options.map(seriesOf)].filter(
    (series): series is string => typeof series === "string"
  );
}

/** The numbers `figure` draws, in its graph or in its table's window columns. */
function drawnValues(figure: Locator, scope: "graph" | "table"): Promise<string[]> {
  return figure
    .locator(scope === "graph" ? "[data-value]" : 'table [data-range="window"][data-value]')
    .evaluateAll((values) => values.map((value) => value.getAttribute("data-value") ?? ""));
}

// SLOW(>=2s): writes four evals' charts and readings and compares every chart with its readings.
test("each declared chart of the four evals draws what their readings hold @evals k21", async ({
  page,
}) => {
  // Four evals' readings are written and every chart of each is read in its graph and its table.
  test.setTimeout(300_000);
  const evals = stagedEvals();
  await createProject({ key: DECLARED, name: "Evals declared charts" });
  for (const staged of evals) {
    await writeReadings(staged.slug, staged.readings);
    await registerEval(DECLARED, { ...staged.register, slug: staged.slug });
  }

  for (const staged of evals) {
    const window = windowOf(staged.readings);
    const record = recordLive(page);
    await openTab(page, DECLARED, `${windowQuery(window)}&compare=off`);
    const logged = await paintedSection(page, "logged");
    const declared = logged.locator(`[data-declared="${staged.slug}"]`);
    await expect(declared, staged.slug).toBeVisible();
    const every = numbersOf(await readingsOf(staged.slug, window, ""));

    for (const spec of staged.register.charts) {
      const id = String(spec.id ?? "");
      const what = `${staged.slug}/${id}`;
      const section = declared.locator(`section[data-declared-chart="${id}"]`);
      await expect(section, `${what} is drawn by its kind`).toBeVisible();
      await expect(section.getByRole("status"), what).toHaveCount(0);
      const figure = section.locator("figure[data-chart]").first();
      await expect(figure, what).toBeVisible();
      const graph = await drawnValues(figure, "graph");
      expect(
        graph.filter((value) => !every.has(value)),
        `${what}: drawn, held by no reading`
      ).toEqual([]);
      await showChart(figure, "Table");
      const table = await drawnValues(figure, "table");
      expect(
        table.filter((value) => !every.has(value)),
        `${what}: tabled, held by no reading`
      ).toEqual([]);
      expect(
        graph.filter((value) => !table.includes(value)),
        `${what}: drawn, not tabled`
      ).toEqual([]);
      await showChart(figure, "Graph");

      // A step chart draws, of each series, the readings its one bounded view keeps, at the
      // `buckets` its drawn width names, and each group's reading before the window (plan D21).
      const stepped = stepSeries(spec);
      if (stepped.length === 0) continue;
      const expected = new Set<string>();
      for (const series of stepped) {
        const bounded = () =>
          subscribedViews(record.sent)
            .filter(
              (view) =>
                view.kind === "items" &&
                view.slug === staged.slug &&
                view.query.get("series") === series &&
                view.query.has("buckets")
            )
            .at(-1);
        await expect
          .poll(() => bounded() !== undefined, `${what}: ${series}'s bounded view`)
          .toBe(true);
        const view = bounded();
        const kept = keptValues(
          await readingsOf(staged.slug, window, series),
          window,
          Number(view?.query.get("buckets")),
          view?.query.get("group") ?? "col",
          spec.kind === "band"
        );
        for (const value of kept) expected.add(value);
      }
      expect([...new Set(graph)].sort(), `${what}: the step rule's values`).toEqual(
        [...expected].sort()
      );
    }
  }
});

// ---------------------------------------------------------------------------------------------
// The other kinds, from synthetic fixtures of this file's own.

/** An eval that declares one chart of every kind the validator accepts (the base specs of
 *  internal/dispatch/evals/charts/testdata/corpus.json), `pending` twice, as a mock-up and as a
 *  placeholder, and `network` and `architecture` with no `graph`. */
const EVERY_KIND = {
  charts: [
    { id: "xy-chart", kind: "xy", title: "Wait over time", y: { series: "wait" } },
    { id: "band-chart", kind: "band", title: "Wait spread", y: { series: "wait-band" } },
    { id: "bar-chart", kind: "bar", title: "Counts", y: { series: "count" } },
    { id: "stacked-chart", kind: "stacked-bar", title: "Stacked counts", y: { series: "stack" } },
    { id: "grid-chart", kind: "grid", title: "Cells", y: { series: "cells" } },
    { id: "interval-chart", kind: "interval", title: "Shares", y: { series: "share" } },
    {
      id: "lanes-chart",
      kind: "lanes",
      title: "States",
      value_states: [{ label: "Done", state: "proved" }],
      y: { series: "state" },
    },
    {
      id: "number-chart",
      kind: "number",
      series: ["total"],
      title: "Total",
      approved: "dispatch://ACME-1/ask/1",
    },
    { id: "prose-chart", kind: "prose", paragraphs: ["One paragraph."], title: "Notes" },
    {
      columns: [],
      id: "runs-chart",
      kind: "runs",
      name: "loss",
      step_label: "Step",
      title: "Runs",
    },
    {
      columns: [{ col: "a", label: "A" }],
      id: "text-chart",
      kind: "text",
      series: "grid",
      title: "Text",
    },
    { color: "status", id: "network-chart", kind: "network", size: "weight", title: "Network" },
    {
      bins: [1, 2, 3],
      id: "architecture-chart",
      kind: "architecture",
      title: "Architecture",
      value_series: "risk",
    },
    {
      approved: "dispatch://ACME-1/ask/1",
      builder: "dispatch://ACME-9",
      id: "pending-mockup",
      kind: "pending",
      mockup: {
        points: [{ series: "a", x: 1, y: 2 }],
        shape: "line",
        submitted_at: "2026-11-11T09:00:00Z",
        submitted_by: "Ann",
        x: { label: "Day", unit: "" },
        y: { label: "Count", unit: "tasks" },
      },
      title: "Coming next",
    },
    {
      approved: "dispatch://ACME-1/ask/1",
      builder: "dispatch://ACME-9",
      id: "pending-placeholder",
      kind: "pending",
      title: "Coming later",
    },
  ],
  slug: "e2e-every-kind",
} as const;

/** The readings EVERY_KIND's charts read, in 2026-11-0x. */
const EVERY_KIND_READINGS: readonly ReadingInput[] = [
  ...[3, 5, 4].map((value, day) => ({
    measured_at: `2026-11-0${day + 1}T12:00:00Z`,
    row_kind: "point",
    series: "wait",
    value,
  })),
  ...[3, 5].map((value, day) => ({
    ci_high: value + 1,
    ci_low: value - 1,
    measured_at: `2026-11-0${day + 1}T12:00:00Z`,
    row_kind: "point",
    series: "wait-band",
    value,
  })),
  ...["a", "b"].flatMap((row, index) => [
    {
      measured_at: "2026-11-02T12:00:00Z",
      row_key: row,
      row_kind: "row",
      series: "count",
      value: index + 2,
    },
    {
      col: "x",
      measured_at: "2026-11-02T12:00:00Z",
      row_key: row,
      row_kind: "row",
      series: "stack",
      value: index + 1,
    },
    {
      col: "c1",
      measured_at: "2026-11-02T12:00:00Z",
      row_key: row,
      row_kind: "row",
      series: "cells",
      value: index + 4,
    },
    {
      ci_high: 0.6,
      ci_low: 0.2,
      measured_at: "2026-11-02T12:00:00Z",
      row_key: row,
      row_kind: "row",
      series: "share",
      value: 0.4,
    },
  ]),
  {
    measured_at: "2026-11-02T12:00:00Z",
    row_key: "lane-1",
    row_kind: "row",
    series: "state",
    value: 1,
  },
  { measured_at: "2026-11-03T12:00:00Z", row_kind: "point", series: "total", value: 7 },
  {
    col: "1",
    measured_at: "2026-11-02T12:00:00Z",
    row_key: "run-1",
    row_kind: "row",
    series: "loss",
    value: 0.9,
  },
  {
    col: "2",
    measured_at: "2026-11-02T13:00:00Z",
    row_key: "run-1",
    row_kind: "row",
    series: "loss",
    value: 0.5,
  },
  {
    col: "a",
    measured_at: "2026-11-02T12:00:00Z",
    row_key: "r1",
    row_kind: "row",
    series: "grid",
    value: 3,
  },
  {
    attributes: { status: "ok", weight: 3 },
    measured_at: "2026-11-02T12:00:00Z",
    row_key: "n1",
    row_kind: "node",
    series: "network",
    value: 1,
  },
  {
    attributes: { column: 0, label: "Part one", row: 0 },
    measured_at: "2026-11-02T12:00:00Z",
    row_key: "p1",
    row_kind: "part",
    series: "risk",
    value: 2,
  },
];

/** EVERY_KIND's window. */
const EVERY_KIND_WINDOW = { from: "2026-11-01T00:00:00Z", to: "2026-11-04T00:00:00Z" } as const;

// SLOW(>=2s): writes an eval of every kind and opens its tab, recording the first paint.
test("every other declared kind draws from synthetic readings, read after the first paint @evals k21", async ({
  page,
}) => {
  await createProject({ key: DECLARED, name: "Evals declared charts" });
  await logReadings(DECLARED, EVERY_KIND.slug, EVERY_KIND_READINGS);
  await registerEval(DECLARED, {
    charts: EVERY_KIND.charts.map((chart) => ({ ...chart })),
    slug: EVERY_KIND.slug,
  });
  const record = recordLive(page);
  await openTab(page, DECLARED, `${windowQuery(EVERY_KIND_WINDOW)}&compare=off`);
  const logged = await paintedSection(page, "logged");
  const declared = logged.locator(`[data-declared="${EVERY_KIND.slug}"]`);

  for (const chart of EVERY_KIND.charts) {
    const section = declared.locator(`section[data-declared-chart="${chart.id}"]`);
    await expect(section, chart.id).toHaveAttribute("data-declared-kind", chart.kind);
    const figure = section.locator("figure[data-chart]").first();
    await expect(figure, chart.id).toBeVisible();
    await expect(
      figure.getByRole("button", { exact: true, name: "Table" }),
      chart.id
    ).toBeVisible();
  }
  // A pending chart takes no compare range (spec R20); its mock-up and its placeholder both draw.
  for (const id of ["pending-mockup", "pending-placeholder"]) {
    const figure = chartFigure(declared, `${EVERY_KIND.slug}/${id}`);
    await expect(figure.getByRole("checkbox", { name: "Compare" }), id).toHaveCount(0);
  }

  // The first paint reads no declared chart: the eval's detail, which holds its charts, is read
  // and its charts' items views subscribed only once every evals view has painted (plan D25).
  const painted = Math.max(
    ...evalsViews(record, DECLARED).map(
      (view) => snapshotsOf(record.received, view.sub)[0]?.at ?? Number.POSITIVE_INFINITY
    )
  );
  const detailPath = `/api/v1/projects/${DECLARED}/evals/${EVERY_KIND.slug}`;
  const details = record.requests.filter(({ url }) => url.pathname === detailPath);
  expect(details.length).toBeGreaterThan(0);
  expect(details.filter(({ at }) => at < painted).map(({ url }) => url.href)).toEqual([]);
  expect(
    subscribedViews(record.sent)
      .filter((view) => view.kind === "items" && view.slug === EVERY_KIND.slug && view.at < painted)
      .map((view) => view.query.toString())
  ).toEqual([]);
});

// SLOW(>=2s): writes two versions of an eval's charts and answer and opens its tab.
test("a second version of charts and answer stands beside the first @evals k21", async ({
  page,
}) => {
  const slug = "e2e-versioned";
  const first = {
    answer: "The first answer.",
    charts: [{ id: "v-chart", kind: "xy", title: "First chart", y: { series: "wait" } }],
  };
  const second = {
    answer: "The second answer.",
    charts: [{ id: "v-chart", kind: "xy", title: "Second chart", y: { series: "wait" } }],
  };
  await createProject({ key: DECLARED, name: "Evals declared charts" });
  await logReadings(
    DECLARED,
    slug,
    EVERY_KIND_READINGS.filter((reading) => reading.series === "wait")
  );
  await registerEval(DECLARED, { ...first, slug });
  await registerEval(DECLARED, { ...second, slug });

  const read = await getJson<EvalDetailResponse>(
    `/api/v1/projects/${DECLARED}/evals/${slug}?limit=10`
  );
  const versions = read.items ?? [];
  const holding = (answer: string) =>
    versions.find((version) => version.definition.answer === answer);
  expect(versions[0]?.definition.answer).toBe(second.answer);
  expect(versions[0]?.charts).toEqual(second.charts);
  expect(holding(first.answer)?.charts).toEqual(first.charts);
  expect(holding(first.answer)?.definition.version ?? 0).toBeLessThan(
    versions[0]?.definition.version ?? 0
  );

  // The tab draws the newest version's chart and answer.
  await openTab(page, DECLARED, `${windowQuery(EVERY_KIND_WINDOW)}&compare=off`);
  const declared = (await paintedSection(page, "logged")).locator(`[data-declared="${slug}"]`);
  await expect(declared.locator("[data-answer]")).toHaveText(second.answer);
  await expect(declared.locator('section[data-declared-chart="v-chart"]')).toHaveAttribute(
    "aria-label",
    second.charts[0].title
  );
});

/** A register write sent as alice with no check of its answer: its status and body. */
async function postRegister(
  body: object
): Promise<{ status: number; body: { code?: string; field?: string } }> {
  const written = await fetch(new URL(`/api/v1/projects/${DECLARED}/evals`, baseUrl), {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
    method: "POST",
  });
  // Dispatch's refusal body (ErrorBody, with the field a chart refusal names), or its answer.
  const answer = (await written.json()) as { code?: string; field?: string };
  return { body: answer, status: written.status };
}

/** Specs the eval platform's rules refuse, each with the field its first refusal names (the
 *  validator corpus's cases of these names). */
const BAD_SPECS = [
  {
    charts: [
      {
        color: "status",
        graph: { source_prefix: "/abs/", team: "demo" },
        id: "network-chart",
        kind: "network",
        size: "weight",
        title: "A chart",
      },
    ],
    field: "charts[0].graph.source_prefix",
  },
  {
    charts: [
      {
        bins: [1, 2, 3],
        graph: { source_prefix: "a/" },
        id: "architecture-chart",
        kind: "architecture",
        title: "A chart",
        value_series: "risk",
      },
    ],
    field: "charts[0].graph.team",
  },
] as const;

// SLOW(>=2s): sends refused register writes of a new eval and of one that stands.
test("a spec the platform's rules refuse is refused naming its field, and nothing is written @evals k21", async ({
  page,
}) => {
  await createProject({ key: DECLARED, name: "Evals declared charts" });
  const standing = "e2e-standing";
  await logReadings(
    DECLARED,
    standing,
    EVERY_KIND_READINGS.filter((reading) => reading.series === "wait")
  );
  const before = await readEvalDetail(DECLARED, standing);
  for (const bad of BAD_SPECS) {
    for (const slug of ["e2e-never-written", standing]) {
      const refused = await postRegister({ charts: bad.charts, slug });
      expect(refused.status, `${slug} ${bad.field}`).toBe(400);
      expect(refused.body, `${slug} ${bad.field}`).toMatchObject({
        code: "CHART_SPEC_INVALID",
        field: bad.field,
      });
    }
  }
  expect((await readEvalDetail(DECLARED, "e2e-never-written")).status).toBe(404);
  expect((await readEvalDetail(DECLARED, standing)).body).toEqual(before.body);

  // The tab draws the standing eval as it was: no declared chart.
  await openTab(page, DECLARED, `${windowQuery(EVERY_KIND_WINDOW)}&compare=off`);
  const logged = await paintedSection(page, "logged");
  await expect(logged.locator(`[data-eval="${standing}"]`)).toBeVisible();
  await expect(logged.locator("[data-declared]")).toHaveCount(0);
});

// ---------------------------------------------------------------------------------------------
// A credential of another project (ledger item 59).

/** The one curve every DPoP proof is made with: ES256, the only one Dispatch accepts. */
const CURVE = { name: "ECDSA", namedCurve: "P-256" } as const;

function base64url(bytes: ArrayBuffer | string): string {
  return Buffer.from(typeof bytes === "string" ? bytes : new Uint8Array(bytes)).toString(
    "base64url"
  );
}

async function sha256(text: string): Promise<string> {
  return base64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)));
}

/** A token the harness's broker (e2e/fake-broker.ts) answers live, traded for a session acting
 *  for alice and scoped to `project` alone (`project_scope` named), bound to a key of its own;
 *  and the headers that present it on one request, with a fresh proof of that key for the
 *  request's method and address (RFC 9449), as e2e/acceptance/broker-session.ts signs them. */
async function projectToken(
  project: string
): Promise<(method: string, url: URL) => Promise<Record<string, string>>> {
  const { privateKey } = await crypto.subtle.generateKey(CURVE, true, ["sign", "verify"]);
  const key = await crypto.subtle.exportKey("jwk", privateKey);
  const publicHalf = { crv: key.crv, kty: key.kty, x: key.x, y: key.y };
  const token = fakeBrokerToken({
    active: true,
    client_id: "dispatch-token-exchange",
    exp: Math.floor(Date.now() / 1000) + 3600,
    jkt: await sha256(JSON.stringify(publicHalf)),
    person: "alice",
    project_scope: "named",
    projects: [project],
    scope: "dispatch.write",
    session: "e2e-evals-k21-scoped",
  });
  const signer = await crypto.subtle.importKey("jwk", key, CURVE, false, ["sign"]);
  return async (method, url) => {
    const target = new URL(url);
    target.search = "";
    const header = { alg: "ES256", jwk: publicHalf, typ: "dpop+jwt" };
    const claims = {
      ath: await sha256(token),
      htm: method,
      htu: target.toString(),
      iat: Math.floor(Date.now() / 1000),
      jti: randomUUID(),
    };
    const signing = `${base64url(JSON.stringify(header))}.${base64url(JSON.stringify(claims))}`;
    const signature = await crypto.subtle.sign(
      { hash: "SHA-256", name: "ECDSA" },
      signer,
      new TextEncoder().encode(signing)
    );
    return { Authorization: `DPoP ${token}`, DPoP: `${signing}.${base64url(signature)}` };
  };
}

// SLOW(>=2s): trades a project's token, writes its own project, and is refused another's.
test("a write with another project's token is refused PROJECT_SCOPE and writes nothing @evals k21", async ({
  page,
}) => {
  const slug = "e2e-scoped-widgets";
  await createProject({ key: NO_DAEMON, name: "Evals without a daemon" });
  await createProject({ key: DECLARED, name: "Evals declared charts" });
  // A named token's project narrows the private projects it does not name (project_scope.go).
  await sql(`UPDATE projects SET visibility = 'private' WHERE key = ${sqlLiteral(DECLARED)}`);
  const present = await projectToken(NO_DAEMON);
  const reading = EVERY_KIND_READINGS.filter((one) => one.series === "wait");
  const readingsUrl = (project: string) =>
    new URL(`/api/v1/projects/${project}/evals/${slug}/readings`, baseUrl);

  // The token writes its own project's eval: it is live and it writes.
  const own = await postReadings(
    NO_DAEMON,
    slug,
    reading,
    await present("POST", readingsUrl(NO_DAEMON))
  );
  expect(own.status, own.body).toBe(201);

  // The readings write and the register write of the other project are refused, and neither
  // stores a version or a reading.
  const refused = await postReadings(
    DECLARED,
    slug,
    reading,
    await present("POST", readingsUrl(DECLARED))
  );
  expect(refused.status, refused.body).toBe(403);
  expect(JSON.parse(refused.body)).toMatchObject({ code: "PROJECT_SCOPE" });
  const registerUrl = new URL(`/api/v1/projects/${DECLARED}/evals`, baseUrl);
  const register = await fetch(registerUrl, {
    body: JSON.stringify({ name: "Scoped widgets", slug }),
    headers: { "Content-Type": "application/json", ...(await present("POST", registerUrl)) },
    method: "POST",
  });
  expect(register.status).toBe(403);
  expect(await register.json()).toMatchObject({ code: "PROJECT_SCOPE" });
  const evalId = sqlLiteral(`${DECLARED}/${slug}`);
  expect(await sql(`SELECT count(*) FROM eval_versions WHERE eval_id = ${evalId}`)).toBe("0");
  expect(await sql(`SELECT count(*) FROM eval_readings WHERE eval_id = ${evalId}`)).toBe("0");

  // The private project's tab holds no such eval; its own project's does.
  await openTab(page, NO_DAEMON, `${windowQuery(EVERY_KIND_WINDOW)}&compare=off`);
  await expect(
    (await paintedSection(page, "logged")).locator(`[data-eval="${slug}"]`)
  ).toBeVisible();
  expect(await getJson<object>(`/api/v1/projects/${NO_DAEMON}/evals/${slug}`)).toBeDefined();
});
