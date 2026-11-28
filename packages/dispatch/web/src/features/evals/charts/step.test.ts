import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import { bandKind } from "./band";
import type { ChartKind, DeclaredSpec } from "./kind";
import { isPlotModel, type PlotModel, type StepHold } from "./plot";
import { type KeptRole, readingsOf } from "./readings";
import { xyKind } from "./xy";

// The step rule (dispatch://INFRA-2918/artifact/plan-md, D21, "The step rule's cases"): every
// chart over measured time with `curve: "step"` draws each group from the window's start at the
// value of its reading marked before the window, holds each value until the group's next reading,
// ends the line at a reading with no value, and holds the newest value to the window's end or to
// the present, whichever comes first; the compare range's line does the same on its own range,
// shifted onto the window's axis. Its table holds the reading before the window, marked so, and
// each reading the bounded read kept, marked with the roles it was kept for.

/** The kinds the step rule draws, by the name their spec gives. */
const MODELS = { band: bandKind, xy: xyKind } as const satisfies Record<string, ChartKind>;
type StepKind = keyof typeof MODELS;

const WINDOW: EvalRange = { days: 3, from: "2026-11-01T00:00:00Z", to: "2026-11-04T00:00:00Z" };
const COMPARE: EvalRange = { days: 3, from: "2026-10-29T00:00:00Z", to: "2026-11-01T00:00:00Z" };
const WINDOW_START = Date.parse(WINDOW.from);

/** A time of 2026, `11-01T06:00`, as the model's holds name it. */
const at = (time: string): string => `2026-${time}:00.000Z`;
const START = "11-01T00:00";
const END = "11-04T00:00";

/** A row of the chart's table as the table names its time, with its marks. */
function placeLabel(time: string, marks: readonly string[] = []): string {
  return [`2026-${time.replace("T", " ")} UTC`, marks.join("; ")].filter(Boolean).join(", ");
}

/** One reading of the series `limit` as the items read answers it. */
interface Logged {
  readonly id: string;
  /** Its measured time, `2026-` and `:00Z` around it where it is short. */
  readonly at: string;
  readonly value: number | null;
  readonly col?: string;
  readonly row?: string;
  readonly range?: "window" | "compare";
  readonly before?: boolean;
  readonly kept?: readonly KeptRole[];
  /** Its interval: the value less and plus 1 where it names none. */
  readonly low?: number;
  readonly high?: number;
}

/** The time of `one`, written whole. */
function measuredOf(one: Logged): string {
  return one.at.endsWith("Z") ? one.at : `2026-${one.at}:00Z`;
}

/** The ends of the interval of `one`, none where it has no value. */
function lowOf(one: Logged): number | null {
  return one.value === null ? null : (one.low ?? one.value - 1);
}
function highOf(one: Logged): number | null {
  return one.value === null ? null : (one.high ?? one.value + 1);
}

function itemRow(one: Logged, kind: StepKind): EvalItemRow {
  const low = lowOf(one);
  const high = highOf(one);
  const measured = measuredOf(one);
  return {
    ...(one.before === true ? { before_window: true } : {}),
    cols: {
      // A band groups its lines by row group, as it takes no `group: "row"` (the chart contract).
      attributes: kind === "band" && one.row !== undefined ? { row_group: one.row } : {},
      ci_high: high,
      ci_level: high === null ? null : 0.9,
      ci_low: low,
      col: one.col ?? "",
      id: one.id,
      n: null,
      n_unit: "",
      note: "",
      recorded_at: measured,
      replaces: "",
      series: "limit",
      unit: "",
      value: one.value,
      value_absent_reason: one.value === null ? "no read" : "",
    },
    href: "",
    ...(one.kept === undefined ? {} : { kept: [...one.kept] }),
    links: [],
    measured_at: measured,
    range: one.range ?? "window",
    row_key: one.row ?? "",
    row_kind: "reading",
    source: "test",
  };
}

/** The step chart of `kind` over the series `limit`, its lines grouped by `group`. */
function stepSpec(kind: StepKind, group: "col" | "row"): DeclaredSpec {
  return {
    curve: "step",
    group: kind === "band" && group === "row" ? "row_group" : group,
    id: `limit-${kind}`,
    kind,
    title: "Limit",
    y: { label: "Limit", series: "limit" },
  };
}

/** One row of "The step rule's cases". */
interface StepCase {
  readonly rows: readonly Logged[];
  readonly group?: "col" | "row";
  readonly compare?: boolean;
  /** The present, where it falls inside the window. */
  readonly now?: string;
  /** Each series' runs of holds in each range, `[from, to, value]`; none where `every` says
   *  what the line equals instead. */
  readonly holds?: Readonly<
    Record<string, { window: [string, string, number][][]; compare?: [string, string, number][][] }>
  >;
  /** The table's rows: its time and marks, then each series' value, then each compare value. */
  readonly table: readonly (readonly string[])[];
  /** The readings the bounded read kept these from: the line drawn from these equals theirs in
   *  every span of `buckets`. */
  readonly every?: { readonly rows: readonly Logged[]; readonly buckets: number };
}

function modelOf(kind: StepKind, one: StepCase, rows = one.rows): PlotModel {
  const drawn = MODELS[kind].model(stepSpec(kind, one.group ?? "col"), {
    compare: one.compare === true ? COMPARE : null,
    now: one.now === undefined ? undefined : Date.parse(at(one.now)),
    readings: readingsOf(rows.map((row) => itemRow(row, kind))),
    slug: "admission",
    window: WINDOW,
  });
  if (!isPlotModel(drawn)) throw new Error(`the ${kind} model holds no plot`);
  return drawn;
}

/** A hold as the cases write it: its ends as times, its value, and on a band its interval. */
function holdOf(kind: StepKind, hold: StepHold): (string | number | null)[] {
  const ends = [new Date(hold.from).toISOString(), new Date(hold.to).toISOString(), hold.y];
  return kind === "band" ? [...ends, hold.low, hold.high] : ends;
}

function expectedHold(kind: StepKind, [from, to, y]: [string, string, number]) {
  return kind === "band" ? [at(from), at(to), y, y - 1, y + 1] : [at(from), at(to), y];
}

// The M4 rule of the bounded read (plan D21), over every reading of each span of `buckets`
// equal spans of the window ordered by measured time then id: the first and the last, the
// smallest and the largest value, the first with no value, and on a band the smallest ci_low and
// the largest ci_high.
function m4(rows: readonly Logged[], buckets: number, kind: StepKind): Map<string, KeptRole[]> {
  const width = (Date.parse(WINDOW.to) - WINDOW_START) / buckets;
  const spans = new Map<number, Logged[]>();
  for (const one of rows) {
    if (one.before === true) continue;
    const span = Math.floor((Date.parse(measuredOf(one)) - WINDOW_START) / width);
    spans.set(span, [...(spans.get(span) ?? []), one]);
  }
  const kept = new Map<string, KeptRole[]>();
  const keep = (one: Logged | undefined, role: KeptRole) => {
    if (one !== undefined) kept.set(one.id, [...(kept.get(one.id) ?? []), role]);
  };
  for (const span of spans.values()) {
    span.sort((a, b) => measuredOf(a).localeCompare(measuredOf(b)) || a.id.localeCompare(b.id));
    const valued = span.filter((one) => one.value !== null);
    const least = (of: (one: Logged) => number) =>
      valued.reduce<Logged | undefined>(
        (best, one) => (best === undefined || of(one) < of(best) ? one : best),
        undefined
      );
    keep(span[0], "first");
    keep(span.at(-1), "last");
    keep(
      least((one) => one.value ?? 0),
      "min"
    );
    keep(
      least((one) => -(one.value ?? 0)),
      "max"
    );
    keep(
      span.find((one) => one.value === null),
      "no_value"
    );
    if (kind === "band") {
      keep(
        least((one) => lowOf(one) ?? 0),
        "min_low"
      );
      keep(
        least((one) => -(highOf(one) ?? 0)),
        "max_high"
      );
    }
  }
  return kept;
}

/** The roles of a kept row as its table names them. */
const ROLE_WORDS: Readonly<Record<KeptRole, string>> = {
  first: "the first",
  last: "the last",
  max: "the highest",
  max_high: "the highest interval high",
  min: "the lowest",
  min_low: "the lowest interval low",
  no_value: "the first with no value",
};

function keptMark(roles: readonly KeptRole[]): string {
  const words = roles.map((role) => ROLE_WORDS[role]);
  const listed =
    words.length === 1 ? words[0] : `${words.slice(0, -1).join(", ")} and ${words.at(-1)}`;
  return `kept as ${listed} in its span of time`;
}

/**
 * What a step line draws in each of `buckets` equal spans of the window, its CSS pixel columns at
 * the view's buckets: the value it enters at, its smallest and largest, and the value it leaves
 * at, and on a band its spread's lowest and highest and the ends it enters and leaves at.
 */
function columns(kind: StepKind, drawn: PlotModel, buckets: number): string[] {
  const width = (Date.parse(WINDOW.to) - WINDOW_START) / buckets;
  const holds = drawn.series.flatMap((series) => series.steps?.window.flat() ?? []);
  return Array.from({ length: buckets }, (_, column) => {
    const from = WINDOW_START + column * width;
    const inside = holds.filter((hold) => hold.from < from + width && hold.to > from);
    if (inside.length === 0) return `${column}: nothing`;
    const ys = inside.map((hold) => hold.y);
    const ends = (of: (hold: StepHold) => number | null) => {
      const values = inside.flatMap((hold) => {
        const value = of(hold);
        return value === null ? [] : [value];
      });
      return [values[0], Math.min(...values), Math.max(...values), values.at(-1)];
    };
    const band = kind === "band" ? [...ends((hold) => hold.low), ...ends((hold) => hold.high)] : [];
    return `${column}: ${[ys[0], Math.min(...ys), Math.max(...ys), ys.at(-1), ...band].join(" ")}`;
  });
}

/** Many readings in four spans of 160, a reading with no value among them, and one in each of
 *  two other spans, which the bounded read at 160 keeps of. */
function busyReadings(kind: StepKind): { every: Logged[]; kept: Logged[] } {
  const buckets = 160;
  const width = (Date.parse(WINDOW.to) - WINDOW_START) / buckets;
  const pattern = [5, 3, 8, 1, 9, 2, 7, 4, 6, 0, 9, 1];
  const every: Logged[] = [
    { at: "2026-10-31T20:00:00Z", before: true, id: "busy-before", value: 4 },
  ];
  for (const span of [10, 11, 50, 51]) {
    pattern.forEach((value, index) => {
      every.push({
        at: new Date(WINDOW_START + span * width + (index + 1) * 120_000).toISOString(),
        high: value + ((index * 5) % 7) / 2,
        id: `busy-${span}-${String(index).padStart(2, "0")}`,
        low: value - (index % 4),
        value: span === 50 && index === 5 ? null : value,
      });
    });
  }
  for (const span of [100, 140]) {
    every.push({
      at: new Date(WINDOW_START + span * width + 60_000).toISOString(),
      id: `busy-${span}`,
      value: span / 20,
    });
  }
  const roles = m4(every, buckets, kind);
  const kept = every.flatMap((one) => {
    const of = roles.get(one.id);
    if (one.before === true) return [one];
    return of === undefined ? [] : [{ ...one, kept: of }];
  });
  return { every, kept };
}

/** "The step rule's cases", each run against the `xy` and the `band` model. */
const CASES: readonly [string, (kind: StepKind) => StepCase][] = [
  [
    "one before the window, none in it",
    () => ({
      holds: { limit: { window: [[[START, END, 2]]] } },
      rows: [{ at: "10-31T12:00", before: true, id: "before", value: 2 }],
      table: [[placeLabel("10-31T12:00", ["before the window"]), "2"]],
    }),
  ],
  [
    "one before the window, three in it",
    () => ({
      holds: {
        limit: {
          window: [
            [
              [START, "11-01T06:00", 2],
              ["11-01T06:00", "11-02T00:00", 3],
              ["11-02T00:00", "11-03T00:00", 5],
              ["11-03T00:00", END, 4],
            ],
          ],
        },
      },
      rows: [
        { at: "10-31T12:00", before: true, id: "before", value: 2 },
        { at: "11-01T06:00", id: "w1", value: 3 },
        { at: "11-02T00:00", id: "w2", value: 5 },
        { at: "11-03T00:00", id: "w3", value: 4 },
      ],
      table: [
        [placeLabel("10-31T12:00", ["before the window"]), "2"],
        [placeLabel("11-01T06:00"), "3"],
        [placeLabel("11-02T00:00"), "5"],
        [placeLabel("11-03T00:00"), "4"],
      ],
    }),
  ],
  [
    "a reading with no value in the window",
    () => ({
      holds: { limit: { window: [[[START, "11-01T12:00", 2]], [["11-02T00:00", END, 6]]] } },
      rows: [
        { at: "10-31T12:00", before: true, id: "before", value: 2 },
        { at: "11-01T12:00", id: "gap", value: null },
        { at: "11-02T00:00", id: "w1", value: 6 },
      ],
      table: [
        [placeLabel("10-31T12:00", ["before the window"]), "2"],
        [placeLabel("11-01T12:00"), "not measured: no read"],
        [placeLabel("11-02T00:00"), "6"],
      ],
    }),
  ],
  [
    "the newest reading has no value",
    () => ({
      holds: {
        limit: {
          window: [
            [
              [START, "11-02T00:00", 2],
              ["11-02T00:00", "11-03T00:00", 3],
            ],
          ],
        },
      },
      rows: [
        { at: "10-31T12:00", before: true, id: "before", value: 2 },
        { at: "11-02T00:00", id: "w1", value: 3 },
        { at: "11-03T00:00", id: "gap", value: null },
      ],
      table: [
        [placeLabel("10-31T12:00", ["before the window"]), "2"],
        [placeLabel("11-02T00:00"), "3"],
        [placeLabel("11-03T00:00"), "not measured: no read"],
      ],
    }),
  ],
  [
    "a window that ends after the present",
    () => ({
      holds: {
        limit: {
          window: [
            [
              [START, "11-02T00:00", 2],
              ["11-02T00:00", "11-03T12:00", 3],
            ],
          ],
        },
      },
      now: "11-03T12:00",
      rows: [
        { at: "10-31T12:00", before: true, id: "before", value: 2 },
        { at: "11-02T00:00", id: "w1", value: 3 },
      ],
      table: [
        [placeLabel("10-31T12:00", ["before the window"]), "2"],
        [placeLabel("11-02T00:00"), "3"],
      ],
    }),
  ],
  [
    "none before the window",
    () => ({
      holds: {
        limit: {
          window: [
            [
              ["11-01T06:00", "11-02T00:00", 3],
              ["11-02T00:00", END, 5],
            ],
          ],
        },
      },
      rows: [
        { at: "11-01T06:00", id: "w1", value: 3 },
        { at: "11-02T00:00", id: "w2", value: 5 },
      ],
      table: [
        [placeLabel("11-01T06:00"), "3"],
        [placeLabel("11-02T00:00"), "5"],
      ],
    }),
  ],
  [
    "a compare range",
    () => ({
      compare: true,
      holds: {
        limit: {
          // The compare range's reading of 28 Oct, before its range, and its reading of 30 Oct,
          // three days on; its newest held to its end, the window's end on the window's axis.
          compare: [
            [
              [START, "11-02T00:00", 1],
              ["11-02T00:00", END, 4],
            ],
          ],
          window: [
            [
              [START, "11-01T06:00", 2],
              ["11-01T06:00", END, 3],
            ],
          ],
        },
      },
      rows: [
        { at: "10-31T18:00", before: true, id: "before", value: 2 },
        { at: "11-01T06:00", id: "w1", value: 3 },
        { at: "10-28T12:00", before: true, id: "compare-before", range: "compare", value: 1 },
        { at: "10-30T00:00", id: "c1", range: "compare", value: 4 },
      ],
      table: [
        [placeLabel("10-31T12:00", ["before the compare range"]), "no data", "1"],
        [placeLabel("10-31T18:00", ["before the window"]), "2", "no data"],
        [placeLabel("11-01T06:00"), "3", "no data"],
        [placeLabel("11-02T00:00"), "no data", "4"],
      ],
    }),
  ],
  [
    "grouped by `col`, two cols",
    () => ({
      holds: {
        "limit \u00b7 a": {
          window: [
            [
              [START, "11-02T00:00", 2],
              ["11-02T00:00", END, 3],
            ],
          ],
        },
        "limit \u00b7 b": {
          window: [
            [
              [START, "11-01T12:00", 7],
              ["11-01T12:00", END, 8],
            ],
          ],
        },
      },
      rows: [
        { at: "10-31T12:00", before: true, col: "a", id: "before-a", value: 2 },
        { at: "10-30T00:00", before: true, col: "b", id: "before-b", value: 7 },
        { at: "11-01T12:00", col: "b", id: "b1", value: 8 },
        { at: "11-02T00:00", col: "a", id: "a1", value: 3 },
      ],
      // Each col's line, b's first: its first reading is the earlier.
      table: [
        [placeLabel("10-30T00:00", ["before the window"]), "7", "no data"],
        [placeLabel("10-31T12:00", ["before the window"]), "no data", "2"],
        [placeLabel("11-01T12:00"), "8", "no data"],
        [placeLabel("11-02T00:00"), "no data", "3"],
      ],
    }),
  ],
  [
    "grouped by `row`",
    () => ({
      group: "row",
      holds: {
        "limit \u00b7 r1": {
          window: [
            [
              [START, "11-02T00:00", 1],
              ["11-02T00:00", END, 2],
            ],
          ],
        },
        "limit \u00b7 r2": {
          window: [
            [
              [START, "11-03T00:00", 5],
              ["11-03T00:00", END, 6],
            ],
          ],
        },
      },
      rows: [
        { at: "10-31T00:00", before: true, id: "before-1", row: "r1", value: 1 },
        { at: "10-31T06:00", before: true, id: "before-2", row: "r2", value: 5 },
        { at: "11-02T00:00", id: "w1", row: "r1", value: 2 },
        { at: "11-03T00:00", id: "w2", row: "r2", value: 6 },
      ],
      table: [
        [placeLabel("10-31T00:00", ["before the window"]), "1", "no data"],
        [placeLabel("10-31T06:00", ["before the window"]), "no data", "5"],
        [placeLabel("11-02T00:00"), "2", "no data"],
        [placeLabel("11-03T00:00"), "no data", "6"],
      ],
    }),
  ],
  [
    "readings kept by M4 with their roles",
    (kind) => {
      const { every, kept } = busyReadings(kind);
      return {
        every: { buckets: 160, rows: every },
        rows: kept,
        table: kept.map((one) => [
          `${new Date(measuredOf(one)).toISOString().slice(0, 16).replace("T", " ")} UTC, ${
            one.before === true ? "before the window" : keptMark(one.kept ?? [])
          }`,
          one.value === null ? "not measured: no read" : String(one.value),
        ]),
      };
    },
  ],
];

afterEach(() => {
  cleanup();
  localStorage.clear();
});

/** Each value drawn under `root`, by what it names: series, statistic, place, range and value. */
function drawnValues(root: Element): string[] {
  return [...root.querySelectorAll("[data-value]")].map((element) =>
    ["data-series", "data-statistic", "data-x", "data-range", "data-value"]
      .map((name) => element.getAttribute(name))
      .join(" ")
  );
}

test.each(
  CASES.flatMap(([name, build]) =>
    (Object.keys(MODELS) as StepKind[]).map((kind) => [kind, name, build] as const)
  )
)("%s: %s", async (kind, _name, build) => {
  const one = build(kind);
  const drawn = modelOf(kind, one);
  expect(drawn.plot.line).toBe("step");

  // The drawn points: each series' holds in each range.
  if (one.holds !== undefined) {
    expect(
      Object.fromEntries(
        drawn.series.map((series) => [
          series.name,
          {
            compare: series.steps?.compare.map((run) => run.map((hold) => holdOf(kind, hold))),
            window: series.steps?.window.map((run) => run.map((hold) => holdOf(kind, hold))),
          },
        ])
      )
    ).toEqual(
      Object.fromEntries(
        Object.entries(one.holds).map(([name, ranges]) => [
          name,
          {
            compare: (ranges.compare ?? []).map((run) =>
              run.map((hold) => expectedHold(kind, hold))
            ),
            window: ranges.window.map((run) => run.map((hold) => expectedHold(kind, hold))),
          },
        ])
      )
    );
  }
  // Drawn from the kept readings, the line covers in each of the view's spans what the line of
  // every reading covers: it enters at the same value, spans the same values, and leaves at the
  // same value.
  if (one.every !== undefined) {
    expect(one.rows.length).toBeLessThan(one.every.rows.length);
    expect(columns(kind, drawn, one.every.buckets)).toEqual(
      columns(kind, modelOf(kind, one, one.every.rows), one.every.buckets)
    );
  }

  // The renderer draws each run of each range as a line of its own, a band's spread under it.
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: drawn, renderer: MODELS[kind].renderer }));
  const figure = screen.getByRole("figure", { name: "Limit" });
  for (const series of drawn.series) {
    const group = figure.querySelector(`[data-series-group="${series.name}"]`);
    for (const range of ["window", "compare"] as const) {
      const runs = series.steps?.[range].length ?? 0;
      expect([
        series.name,
        range,
        group?.querySelectorAll(`[data-step-line="${range}"]`).length,
      ]).toEqual([series.name, range, runs]);
      if (kind === "band") {
        expect(group?.querySelectorAll(`[data-band-range="${range}"]`).length).toBe(runs);
      }
    }
  }
  const graph = drawnValues(figure);

  // The table: each place in time, marked, with each series' value there, then its compare value.
  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const table = within(figure).getByRole("table");
  expect(
    [...table.querySelectorAll("tbody tr[data-row]")].map((row) => [
      row.querySelector("td")?.textContent ?? "",
      ...[...row.querySelectorAll('[data-statistic="y"][data-range="window"]')].map(
        (cell) => cell.textContent ?? ""
      ),
      ...[...row.querySelectorAll('[data-statistic="y"][data-range="compare"]')].map(
        (cell) => cell.textContent ?? ""
      ),
    ])
  ).toEqual(one.table.map((row) => [...row]));
  const tabled = drawnValues(table);
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});

/** The port's corpus of the validator's answers (`internal/dispatch/evals/charts/testdata`). */
const CORPUS = join(
  import.meta.dir,
  "../../../../../../envoy/internal/dispatch/evals/charts/testdata/corpus.json"
);

test("every kind that accepts curve step draws by the step rule", () => {
  const corpus = JSON.parse(readFileSync(CORPUS, "utf8")) as {
    cases: { charts?: unknown; platform: unknown[] }[];
  };
  // The kinds of every chart with `curve: "step"` the validator accepts.
  const accepting = new Set(
    corpus.cases.flatMap((one) =>
      one.platform.length > 0 || !Array.isArray(one.charts)
        ? []
        : (one.charts as DeclaredSpec[]).flatMap((spec) =>
            spec.curve === "step" && typeof spec.kind === "string" ? [spec.kind] : []
          )
    )
  );
  expect([...accepting].sort()).toEqual(Object.keys(MODELS).sort());
  // Every case of the rule runs against each of them, and each draws its line by the rule: from
  // the window's start at its reading before the window.
  expect(CASES.map(([name]) => name)).toHaveLength(10);
  for (const kind of accepting as Set<StepKind>) {
    const [, build] = CASES[0] as (typeof CASES)[number];
    const hold = modelOf(kind, build(kind)).series[0]?.steps?.window[0]?.[0];
    expect([kind, hold === undefined ? null : holdOf(kind, hold)[0]]).toEqual([kind, at(START)]);
  }
});
