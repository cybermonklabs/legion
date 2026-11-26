import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/xy-readings.json";
import type { DeclaredInput, DeclaredSpec } from "./kind";
import { isPlotModel, type PlotModel } from "./plot";
import { readingsOf } from "./readings";
import { xyKind } from "./xy";

// The xy kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's values, states,
// notes and order from the readings of `__fixtures__/xy-readings.json`, the compare range's
// readings on the window's axis, and its table holding every number its graph draws.

const WINDOW = fixture.window as EvalRange;
const COMPARE = fixture.compare as EvalRange;

function input(compare: EvalRange | null = COMPARE): DeclaredInput {
  return {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "convergence",
    window: WINDOW,
  };
}

function model(spec: DeclaredSpec, given = input()): PlotModel {
  const drawn = xyKind.model(spec, given);
  if (!isPlotModel(drawn)) throw new Error("the xy model holds no plot");
  return drawn;
}

/** Each series' points as `[x, label of statistic, ...]`, its window's values and then, where
 *  the compare range is shown, its compare values and changes. */
function rows(drawn: PlotModel, statistic: string) {
  return drawn.series.map((series) => [
    series.name,
    series.points.map((point) => [
      point.x,
      point.values[statistic]?.label,
      point.compare?.[statistic]?.label ?? null,
      point.change?.[statistic]?.label ?? null,
    ]),
  ]);
}

const OVER_TIME: DeclaredSpec = {
  group: "col",
  id: "tokens-over-time",
  kind: "xy",
  order: ["arm-b"],
  title: "Tokens to converge",
  y: { label: "Tokens to converge", series: "tokens" },
};

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("the readings are the items rows that carry an id, a corrected reading in place of the one it corrects", () => {
  const ids = input().readings.map((reading) => reading.id);
  // Oldest first; the day row and r5, which r6 replaces, are no reading the chart draws.
  expect(ids.filter((id) => ["c1", "c2", "r1", "r2", "r3", "r4", "r5", "r6"].includes(id))).toEqual(
    ["c1", "c2", "r1", "r3", "r2", "r4", "r6"]
  );
  expect(input().readings.some((reading) => reading.rowKind === "day")).toBe(false);
});

test("over time: one series a group, in the order order gives, each point at its measured time", () => {
  const drawn = model(OVER_TIME);
  expect(drawn.name).toBe("convergence/tokens-over-time");
  expect(drawn.plot.form).toBe("time");
  expect(drawn.plot.line).toBe("linear");
  // order names arm-b first: it draws first, in the palette's first colour.
  expect(drawn.series.map((series) => [series.name, series.label, series.tone])).toEqual([
    ["tokens \u00b7 arm-b", "arm-b", 0],
    ["tokens \u00b7 arm-a", "arm-a", 1],
  ]);
  expect(drawn.plot.key.map((entry) => entry.name)).toEqual(["arm-b", "arm-a"]);
  expect(drawn.plot.legend).toBe("key");
  // Every place of the chart, in time; the compare range's arm-b reading of 30 Oct, 12:00,
  // stands three days on, where no window reading is.
  expect(drawn.series[0]?.points.map((point) => point.x)).toEqual([
    "2026-11-01T10:00:00.000Z",
    "2026-11-02T10:00:00.000Z",
    "2026-11-02T12:00:00.000Z",
    "2026-11-03T10:00:00.000Z",
  ]);
  expect(drawn.series[0]?.points.map((point) => point.label)).toEqual([
    "2026-11-01 10:00 UTC",
    "2026-11-02 10:00 UTC",
    "2026-11-02 12:00 UTC",
    "2026-11-03 10:00 UTC",
  ]);
  expect(rows(drawn, "y")).toEqual([
    [
      "tokens \u00b7 arm-b",
      [
        ["2026-11-01T10:00:00.000Z", "\u2265 150", "no data", "no data"],
        ["2026-11-02T10:00:00.000Z", "no data", "no data", "no data"],
        ["2026-11-02T12:00:00.000Z", "no data", "160", "no data"],
        ["2026-11-03T10:00:00.000Z", "not measured: run crashed", "no data", "no data"],
      ],
    ],
    [
      "tokens \u00b7 arm-a",
      [
        // The compare range's reading of 29 Oct, 10:00, shifted onto 1 Nov, 10:00.
        ["2026-11-01T10:00:00.000Z", "120", "130", "\u221210"],
        ["2026-11-02T10:00:00.000Z", "100", "no data", "no data"],
        ["2026-11-02T12:00:00.000Z", "no data", "no data", "no data"],
        // r6, which replaces r5's 999.
        ["2026-11-03T10:00:00.000Z", "95", "no data", "no data"],
      ],
    ],
  ]);
});

test("over time: each value's state and note, and the interval the table holds of a group that has one", () => {
  const drawn = model(OVER_TIME);
  const [armB, armA] = drawn.series;
  expect(armB?.statistics.map((statistic) => statistic.id)).toEqual(["y"]);
  expect(armA?.statistics).toEqual([
    { id: "y", label: "Tokens to converge (tokens)" },
    { id: "ci_low", label: "Interval low" },
    { id: "ci_high", label: "Interval high" },
  ]);
  expect(armB?.points.map((point) => [point.values.y?.state, point.values.y?.note])).toEqual([
    ["plain", "lower bound: the true value is at least this; stopped at the budget"],
    ["absent", null],
    ["absent", null],
    ["absent", "run crashed"],
  ]);
  expect(armB?.points[0]?.bound).toBe(">=");
  expect(armB?.points[2]?.compare?.y?.note).toBe("measured 2026-10-30 12:00 UTC");
  expect(armA?.points[1]?.values).toMatchObject({
    ci_high: { label: "110", state: "plain", value: 110 },
    ci_low: { label: "90", state: "plain", value: 90 },
    y: { label: "100", state: "plain", value: 100 },
  });
  expect(armA?.points[0]?.values.ci_low).toEqual({
    label: "no interval",
    note: null,
    state: "absent",
    value: null,
  });
  expect(drawn.plot.notes).toEqual([
    "Ranges: 95-in-100.",
    "Lower bound: the true value is at least this.",
  ]);
});

test("over time with no compare range: no point holds a compare value", () => {
  const drawn = model(OVER_TIME, input(null));
  expect(drawn.compareRange).toBeNull();
  expect(drawn.series.flatMap((series) => series.points.map((point) => point.compare))).toEqual(
    drawn.series.flatMap((series) => series.points.map(() => null))
  );
  // The compare range's own place is gone with it.
  expect(drawn.series[0]?.points.map((point) => point.x)).not.toContain("2026-11-02T12:00:00.000Z");
});

test("against an x series: each row's newest reading at its row's x, the compare range's at the same row", () => {
  const drawn = model({
    group: "col",
    id: "tokens-by-size",
    kind: "xy",
    title: "Tokens by size",
    x: { label: "Binary size", series: "size-kb" },
    y: { label: "Tokens", series: "tokens-per-contract" },
  });
  expect(drawn.plot.form).toBe("number");
  // No curve: a scatter, whose points join no line.
  expect(drawn.plot.line).toBe("none");
  expect(drawn.series.map((series) => series.name)).toEqual([
    "tokens-per-contract \u00b7 arm-a",
    "tokens-per-contract \u00b7 arm-b",
  ]);
  const [armA, armB] = drawn.series;
  expect(armA?.statistics.map((statistic) => statistic.label)).toEqual([
    "Binary size (kB)",
    "Tokens (tokens)",
  ]);
  expect(
    armA?.points.map((point) => [point.x, point.at, point.values.x?.label, point.values.y?.label])
  ).toEqual([
    // t1 (50) is newer than t1-old (55).
    ["contract-1", 10, "10", "50"],
    ["contract-2", 20, "20", "70"],
    // contract-4 has no size-kb reading, so no place on the x axis.
    ["contract-4", null, "no x reading", "90"],
  ]);
  expect(armB?.points.map((point) => [point.x, point.at, point.values.y?.label])).toEqual([
    ["contract-1", 10, "40"],
    ["contract-2", null, "no data"],
    ["contract-4", null, "no data"],
  ]);
  // The compare range's contract-1 stands at its own x, 12 kB, at the same row.
  expect(armA?.points[0]).toMatchObject({
    change: { x: { label: "\u22122", value: -2 }, y: { label: "\u221210", value: -10 } },
    compare: { x: { label: "12", value: 12 }, y: { label: "60", value: 60 } },
    compareAt: 12,
  });
  expect(drawn.plot.notes).toEqual(["1 reading not plotted: no reading of size-kb in its row."]);
});

test("by step: each row a line through its newest reading at each step, and a target from its first step", () => {
  const drawn = model({
    id: "pass-by-week",
    kind: "xy",
    reference: { from_start: 0.125, label: "an eighth better" },
    title: "Pass rate by week",
    x: { label: "Week", series: null, steps: ["week-1", "week-2", "week-3"] },
    y: { label: "Pass rate", series: "pass-rate" },
  });
  expect(drawn.plot.form).toBe("category");
  expect(drawn.plot.steps).toEqual(["week-1", "week-2", "week-3"]);
  expect(drawn.axis.label).toBe("Week");
  // run-1's compare reading is its oldest, so its line takes the first place.
  expect(drawn.series.map((series) => [series.name, series.fromStart])).toEqual([
    ["pass-rate \u00b7 run-1", 0.625],
    ["pass-rate \u00b7 run-2", 0.375],
  ]);
  expect(rows(drawn, "y")).toEqual([
    [
      "pass-rate \u00b7 run-1",
      [
        ["week-1", "0.5", "0.25", "+0.25"],
        ["week-2", "0.63", "no data", "no data"],
        ["week-3", "0.75", "no data", "no data"],
      ],
    ],
    [
      "pass-rate \u00b7 run-2",
      [
        ["week-1", "0.25", "no data", "no data"],
        ["week-2", "no data", "no data", "no data"],
        ["week-3", "0.38", "no data", "no data"],
      ],
    ],
  ]);
  expect(drawn.series[0]?.points[1]?.values.y?.value).toBe(0.625);
  expect(drawn.plot.notes).toEqual(["1 reading not plotted: its col is no step."]);
});

test("y_options: each option a view of its own, whose series follow the view before's", () => {
  const drawn = model({
    group: "col",
    id: "tokens-options",
    kind: "xy",
    title: "Tokens",
    y_options: [
      { label: "Over time", series: "tokens" },
      { label: "Per contract", series: "tokens-per-contract" },
    ],
  });
  expect(drawn.plot.layout).toBe("options");
  expect(drawn.plot.views.map((view) => view.title)).toEqual(["Over time", "Per contract"]);
  expect(drawn.series.map((series) => [series.view, series.name, series.label])).toEqual([
    [0, "tokens \u00b7 arm-a", "Over time \u00b7 arm-a"],
    [0, "tokens \u00b7 arm-b", "Over time \u00b7 arm-b"],
    [1, "tokens-per-contract \u00b7 arm-a", "Per contract \u00b7 arm-a"],
    [1, "tokens-per-contract \u00b7 arm-b", "Per contract \u00b7 arm-b"],
  ]);
  // A group keeps one colour in every view.
  expect(drawn.series.map((series) => series.tone)).toEqual([0, 1, 0, 1]);
});

test("a mock-up draws its points as readings where the chart's series holds no reading", () => {
  const drawn = model({
    id: "sketch",
    kind: "xy",
    mockup: {
      points: [
        { series: "fast", x: "2026-11-01", y: 3 },
        { series: "fast", x: "2026-11-02", y: 4 },
        { series: "slow", x: "2026-11-01", y: 7 },
      ],
      submitted_at: "2026-11-11T09:00:00Z",
      submitted_by: "alice",
    },
    title: "A sketch",
    group: "col",
    y: { label: "Seconds", series: "no-reading-yet" },
  });
  expect(drawn.plot.mockup).toBe("Mock-up: nothing is measured yet; alice drew it on 2026-11-11.");
  expect(rows(drawn, "y")).toEqual([
    [
      "no-reading-yet \u00b7 fast",
      [
        ["2026-11-01T00:00:00.000Z", "3", "no data", "no data"],
        ["2026-11-02T00:00:00.000Z", "4", "no data", "no data"],
      ],
    ],
    [
      "no-reading-yet \u00b7 slow",
      [
        ["2026-11-01T00:00:00.000Z", "7", "no data", "no data"],
        ["2026-11-02T00:00:00.000Z", "no data", "no data", "no data"],
      ],
    ],
  ]);
  expect(drawn.series[0]?.points[0]?.values.y?.note).toBe("mock-up");
});

test("the xy reads each series its spec names, once", () => {
  expect(
    xyKind.reads({
      items: { series: "faults" },
      kind: "xy",
      panels: [
        { x: { label: "Hours", series: "hours" } },
        { x: { label: "Tokens", series: "tokens" } },
      ],
      x: { series: "size-kb" },
      y: { series: "tokens" },
    })
  ).toEqual([
    { series: "tokens" },
    { series: "size-kb" },
    { series: "hours" },
    { series: "faults" },
  ]);
});

/** Each value drawn under `root`, by what it names: series, statistic, place, range and value. */
function drawnValues(root: Element): string[] {
  return [...root.querySelectorAll("[data-value]")].map((element) =>
    ["data-series", "data-statistic", "data-x", "data-range", "data-value"]
      .map((name) => element.getAttribute(name))
      .join(" ")
  );
}

test.each([
  ["over time", OVER_TIME],
  [
    "against an x series",
    {
      group: "col",
      id: "tokens-by-size",
      kind: "xy",
      title: "Tokens to converge",
      x: { label: "Binary size", series: "size-kb" },
      y: { label: "Tokens", series: "tokens-per-contract" },
    },
  ],
  [
    "by step",
    {
      id: "pass-by-week",
      kind: "xy",
      title: "Tokens to converge",
      x: { label: "Week", series: null, steps: ["week-1", "week-2", "week-3"] },
      y: { label: "Pass rate", series: "pass-rate" },
    },
  ],
] as [
  string,
  DeclaredSpec,
][])("the table holds every number the graph draws: %s", async (_name, spec) => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(spec), renderer: xyKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Tokens to converge" });
  const graph = drawnValues(figure);
  // The window's readings and the compare range's, each drawn.
  expect(graph.filter((value) => value.includes(" window ")).length).toBeGreaterThanOrEqual(3);
  expect(graph.filter((value) => value.includes(" compare ")).length).toBeGreaterThanOrEqual(1);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const table = within(figure).getByRole("table");
  const tabled = drawnValues(table);
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});

test("the compare range draws over time as muted points on the window's axis, and against an x series as hollow points", () => {
  render(createElement(EvalChart, { model: model(OVER_TIME), renderer: xyKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Tokens to converge" });
  const compared = figure.querySelectorAll('[data-range="compare"][data-statistic="y"]');
  expect([...compared].map((mark) => mark.getAttribute("data-value")).sort()).toEqual([
    "130",
    "160",
  ]);
  expect(figure.querySelector('circle[data-range="compare"]')?.getAttribute("class")).toContain(
    "fill-slate-300"
  );

  cleanup();
  render(
    createElement(EvalChart, {
      model: model({
        id: "by-size",
        kind: "xy",
        title: "By size",
        x: { series: "size-kb" },
        y: { series: "tokens-per-contract" },
      }),
      renderer: xyKind.renderer,
    })
  );
  const scatter = screen.getByRole("figure", { name: "By size" });
  const hollow = scatter.querySelector('circle[data-range="compare"]');
  expect(hollow?.getAttribute("data-value")).toBe("60");
  expect(hollow?.getAttribute("class")).toContain("fill-white");
});
