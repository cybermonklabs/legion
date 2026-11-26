import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/stacked-bar-readings.json";
import { type BarsModel, isBarsModel } from "./bars";
import type { DeclaredSpec } from "./kind";
import { readingsOf } from "./readings";
import { stackedBarKind } from "./stacked-bar";

// The stacked-bar kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's segments,
// states, notes and order from the readings of `__fixtures__/stacked-bar-readings.json`, over
// time (bars and an area), by row and by week; over time the compare range's readings shifted
// onto the window's dates as a muted second stack, by row each bar's compare cells as a muted
// ghost with each segment's change; and its table holding every number its graph draws.

const WINDOW = fixture.window as EvalRange;
const COMPARE = fixture.compare as EvalRange;

const OVER_TIME: DeclaredSpec = {
  id: "jobs",
  kind: "stacked-bar",
  legend: "direct",
  stack_order: ["fail"],
  title: "Jobs",
  y: { label: "Jobs", series: "jobs" },
};

const AREA: DeclaredSpec = {
  id: "jobs-area",
  kind: "stacked-bar",
  layout: "area",
  title: "Jobs area",
  x: { ticks: "week" },
  y: { series: "jobs" },
};

const BY_ROW: DeclaredSpec = {
  id: "statements",
  kind: "stacked-bar",
  order: "total",
  stack: "row",
  title: "Statements",
  value_states: [
    { label: "Few", state: "attention" },
    { from: 5, label: "Many", state: "proved" },
  ],
  y: { series: "statements" },
};

const FRAMES: DeclaredSpec = {
  frames: "week",
  id: "weekly",
  kind: "stacked-bar",
  stack: "row",
  title: "Weekly",
  y: { series: "weekly" },
};

function model(spec: DeclaredSpec, compare: EvalRange | null = COMPARE): BarsModel {
  const drawn = stackedBarKind.model(spec, {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "builds",
    window: WINDOW,
  });
  if (!isBarsModel(drawn)) throw new Error("the stacked-bar model holds no bars");
  return drawn;
}

/** Each segment's points as `[x, value, compare value, change]`. */
function rows(drawn: BarsModel) {
  return drawn.series.map((series) => [
    series.name,
    series.points.map((point) => [
      point.x,
      point.values.value?.label,
      point.compare?.value?.label ?? null,
      point.change?.value?.label ?? null,
    ]),
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("over time: a bar per measured date, a segment per col in stack_order, the compare range's on the window's dates", () => {
  const drawn = model(OVER_TIME);
  expect([drawn.kind, drawn.bars.layout, drawn.bars.overTime, drawn.bars.legend]).toEqual([
    "stacked-bar",
    "stack",
    true,
    "direct",
  ]);
  expect(drawn.bars.places.map((place) => [place.x, place.at])).toEqual([
    ["2026-11-01", Date.parse("2026-11-01T00:00:00Z")],
    ["2026-11-02", Date.parse("2026-11-02T00:00:00Z")],
    ["2026-11-03", Date.parse("2026-11-03T00:00:00Z")],
  ]);
  expect(drawn.series.map((series) => [series.name, series.tone])).toEqual([
    ["jobs \u00b7 fail", 0],
    ["jobs \u00b7 pass", 1],
  ]);
  expect(drawn.series[0]?.statistics.map((statistic) => statistic.label)).toEqual(["Jobs (jobs)"]);
  expect(rows(drawn)).toEqual([
    [
      "jobs \u00b7 fail",
      [
        // The compare range's 29 October, three days on.
        ["2026-11-01", "1", "1", "no change"],
        ["2026-11-02", "2", "no data", "no data"],
        ["2026-11-03", "no data", "no data", "no data"],
      ],
    ],
    [
      "jobs \u00b7 pass",
      [
        ["2026-11-01", "5", "3", "+2"],
        ["2026-11-02", "6", "2", "+4"],
        ["2026-11-03", "4", "no data", "no data"],
      ],
    ],
  ]);
  // side's earlier reading of 1 November's pass stands behind main's.
  expect(drawn.bars.notes).toEqual([
    "1 reading not plotted: a newer reading stands for its date and col.",
  ]);
  expect(drawn.bars.placeTitle).toBe("Measured time (UTC)");

  render(createElement(EvalChart, { model: drawn, renderer: stackedBarKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Jobs" });
  // Each group named right of its newest segment, with its newest value.
  expect(
    [...figure.querySelectorAll("[data-direct-label]")].map((label) => [
      label.getAttribute("data-direct-label"),
      label.querySelector("[data-value]")?.getAttribute("data-x"),
      label.querySelector("[data-value]")?.textContent,
    ])
  ).toEqual([
    ["jobs \u00b7 fail", "2026-11-02", "2"],
    ["jobs \u00b7 pass", "2026-11-03", "4"],
  ]);
  // The compare range's stack stands muted beside each date's.
  const ghost = figure.querySelector(
    '[data-ghost] rect[data-series="jobs \u00b7 pass"][data-x="2026-11-01"][data-range="compare"]'
  );
  expect(ghost?.getAttribute("data-value")).toBe("3");
  expect(ghost?.getAttribute("class")).toContain("fill-slate-200");
});

test("over time by row group: a segment per row group", () => {
  const drawn = model({ ...OVER_TIME, group: "row_group", stack_order: undefined });
  expect(rows(drawn).map(([name, points]) => [name, (points as unknown[][])[0]])).toEqual([
    // The groups stack in the order of their first readings: side's nightly reading came first.
    ["jobs \u00b7 nightly", ["2026-11-01", "9", "no data", "no data"]],
    // 1 November's newest ci reading is its fail count.
    ["jobs \u00b7 ci", ["2026-11-01", "1", "1", "no change"]],
  ]);
  expect(drawn.bars.notes).toEqual([
    "2 readings not plotted: a newer reading stands for its date and row group.",
  ]);
});

test("by row: a bar per row by its total, a segment per col coloured by its value's state", () => {
  const drawn = model(BY_ROW);
  expect([drawn.bars.overTime, drawn.bars.legend]).toEqual([false, "states"]);
  expect(drawn.bars.places.map((place) => place.x)).toEqual(["lib1", "lib2", "lib3"]);
  expect(rows(drawn)).toEqual([
    [
      "statements \u00b7 proved",
      [
        ["lib1", "10", "7", "+3"],
        ["lib2", "3", "no data", "no data"],
        ["lib3", "1", "no data", "no data"],
      ],
    ],
    [
      "statements \u00b7 open",
      [
        ["lib1", "4", "no data", "no data"],
        ["lib2", "8", "no data", "no data"],
        ["lib3", "no data", "no data", "no data"],
      ],
    ],
  ]);
  expect(drawn.series.map((series) => series.points.map((point) => point.tone))).toEqual([
    ["proved", "attention", "attention"],
    ["attention", "proved", null],
  ]);
  // stack_order puts its parts at the baseline.
  expect(model({ ...BY_ROW, stack_order: ["open"] }).series.map((series) => series.group)).toEqual([
    "open",
    "proved",
  ]);
  // order names the bars first.
  expect(model({ ...BY_ROW, order: ["lib3"] }).bars.places.map((place) => place.x)).toEqual([
    "lib3",
    "lib1",
    "lib2",
  ]);

  render(createElement(EvalChart, { model: drawn, renderer: stackedBarKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Statements" });
  const ghost = figure.querySelector('[data-ghost] rect[data-x="lib1"][data-range="compare"]');
  expect(ghost?.getAttribute("data-value")).toBe("7");
  // The change of each segment prints in it.
  const change = figure.querySelector('text[data-x="lib1"][data-range="change"]');
  expect([change?.textContent, change?.getAttribute("data-series")]).toEqual([
    "+3",
    "statements \u00b7 proved",
  ]);
  expect([...figure.querySelectorAll("[data-key]")].map((entry) => entry.textContent)).toEqual([
    "Few",
    "Many",
  ]);
});

test("frames: a frame per week of measured time, the newest first, picked by its Week control", async () => {
  const user = userEvent.setup();
  const drawn = model(FRAMES);
  expect(drawn.bars.frames).toEqual([
    { label: "Week of 2 Nov", places: [0, 1], week: "2026-11-02" },
    { label: "Week of 26 Oct", places: [2], week: "2026-10-26" },
  ]);
  expect(drawn.bars.places.map((place) => [place.x, place.label])).toEqual([
    ["2026-11-02 \u00b7 lib1", "Week of 2 Nov \u00b7 lib1"],
    ["2026-11-02 \u00b7 lib2", "Week of 2 Nov \u00b7 lib2"],
    ["2026-10-26 \u00b7 lib1", "Week of 26 Oct \u00b7 lib1"],
  ]);
  expect(rows(drawn)).toEqual([
    [
      "weekly \u00b7 done",
      [
        ["2026-11-02 \u00b7 lib1", "5", "no data", "no data"],
        ["2026-11-02 \u00b7 lib2", "3", "no data", "no data"],
        // The compare range's 29 October reading shifts into the week of 26 October.
        ["2026-10-26 \u00b7 lib1", "2", "1", "+1"],
      ],
    ],
    [
      "weekly \u00b7 left",
      [
        ["2026-11-02 \u00b7 lib1", "1", "no data", "no data"],
        ["2026-11-02 \u00b7 lib2", "no data", "no data", "no data"],
        ["2026-10-26 \u00b7 lib1", "no data", "no data", "no data"],
      ],
    ],
  ]);

  render(createElement(EvalChart, { model: drawn, renderer: stackedBarKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Weekly" });
  const week = within(figure).getByRole("group", { name: "Week" });
  expect(
    within(week).getByRole("button", { name: "Week of 2 Nov" }).getAttribute("aria-pressed")
  ).toBe("true");
  expect(figure.querySelector('[data-place="2026-11-02 \u00b7 lib1"]')).not.toBeNull();
  expect(figure.querySelector('[data-place="2026-10-26 \u00b7 lib1"]')).toBeNull();
  await user.click(within(week).getByRole("button", { name: "Week of 26 Oct" }));
  expect(figure.querySelector('[data-place="2026-10-26 \u00b7 lib1"]')).not.toBeNull();
  expect(figure.querySelector('[data-place="2026-11-02 \u00b7 lib1"]')).toBeNull();
});

test("a stacked area: each group a band over the dates, the compare range's a dashed outline", () => {
  const drawn = model(AREA);
  expect([drawn.bars.layout, drawn.bars.weekTicks]).toEqual(["area", true]);
  expect(drawn.series.map((series) => series.name)).toEqual([
    "jobs \u00b7 pass",
    "jobs \u00b7 fail",
  ]);
  render(createElement(EvalChart, { model: drawn, renderer: stackedBarKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Jobs area" });
  expect(
    figure.querySelector('[data-area="jobs \u00b7 pass"][data-area-range="window"] polygon')
  ).not.toBeNull();
  const outline = figure.querySelector(
    '[data-area="jobs \u00b7 pass"][data-area-range="compare"] polyline'
  );
  expect(outline?.getAttribute("stroke-dasharray")).toBe("4 3");

  cleanup();
  render(createElement(EvalChart, { model: model(AREA, null), renderer: stackedBarKind.renderer }));
  expect(
    screen.getByRole("figure", { name: "Jobs area" }).querySelector('[data-area-range="compare"]')
  ).toBeNull();
});

test("a mock-up over time draws its points at their dates, each series a segment", () => {
  const drawn = model({
    id: "soon",
    kind: "stacked-bar",
    mockup: {
      points: [
        { series: "a", x: "2026-11-01", y: 1 },
        { series: "b", x: "2026-11-01", y: 2 },
      ],
      submitted_at: "2026-11-11T09:00:00Z",
      submitted_by: "Ann",
    },
    title: "Soon",
    y: { series: "nothing-yet" },
  });
  expect(drawn.bars.mockup).toBe("Mock-up: nothing is measured yet; Ann drew it on 2026-11-11.");
  expect(rows(drawn)).toEqual([
    ["nothing-yet \u00b7 a", [["2026-11-01", "1", "no data", "no data"]]],
    ["nothing-yet \u00b7 b", [["2026-11-01", "2", "no data", "no data"]]],
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
  ["a stacked area", AREA],
  ["by row", BY_ROW],
  ["by week", FRAMES],
] as [
  string,
  DeclaredSpec,
][])("the table holds every number the graph draws: %s", async (_name, spec) => {
  const user = userEvent.setup();
  const drawn = model(spec);
  render(createElement(EvalChart, { model: drawn, renderer: stackedBarKind.renderer }));
  const figure = screen.getByRole("figure", { name: drawn.title });
  const graph = drawnValues(figure);
  expect(graph.length).toBeGreaterThan(0);
  if (spec !== FRAMES) {
    expect(figure.querySelectorAll('[data-value][data-range="compare"]').length).toBeGreaterThan(0);
  }

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});
