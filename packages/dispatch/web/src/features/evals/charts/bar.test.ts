import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/bar-readings.json";
import { barKind } from "./bar";
import { type BarsModel, isBarsModel } from "./bars";
import type { DeclaredSpec } from "./kind";
import { readingsOf } from "./readings";

// The bar kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's values, states,
// notes and order from the readings of `__fixtures__/bar-readings.json` (by row, grouped,
// horizontal against totals, a dot chart with columns, a facet table, a mock-up), each category's
// newest compare value as a tick with its change, and its table holding every number its graph
// draws.

const WINDOW = fixture.window as EvalRange;
const COMPARE = fixture.compare as EvalRange;

const BY_ROW: DeclaredSpec = {
  id: "done",
  kind: "bar",
  title: "Tasks done",
  y: { label: "Tasks done", series: "tasks-done" },
};

const GROUPED: DeclaredSpec = {
  group: "col",
  id: "suites",
  kind: "bar",
  layout: "grouped",
  legend: "direct",
  of: "n",
  title: "By suite",
  y: { label: "Tasks", series: "tasks-by-suite" },
};

const HORIZONTAL: DeclaredSpec = {
  groups: [{ label: "Libraries", rows: ["openssl", "curl"] }],
  id: "proved",
  kind: "bar",
  of: "statements",
  orientation: "horizontal",
  row_notes: "note",
  title: "Proved",
  y: { label: "Statements proved", series: "proved" },
};

const DOTS: DeclaredSpec = {
  id: "dots",
  kind: "bar",
  mark: "dot",
  orientation: "horizontal",
  plot_header: "Proved",
  row_columns: [
    { field: "note", label: "Owner", series: "owner" },
    { decimals: 1, empty_label: "none", label: "Size", series: "size" },
  ],
  states: { curl: "proved" },
  title: "Dots",
  y: { series: "proved" },
};

const FACET: DeclaredSpec = {
  facet: "row",
  group: "col",
  id: "throughput",
  kind: "bar",
  row_subtitles: { Commits: "to the proofs repository" },
  stack_order: ["proved", "bug"],
  states: { bug: "attention" },
  title: "Throughput",
  y: { series: "throughput" },
};

function model(spec: DeclaredSpec, compare: EvalRange | null = COMPARE): BarsModel {
  const drawn = barKind.model(spec, {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "tasks",
    window: WINDOW,
  });
  if (!isBarsModel(drawn)) throw new Error("the bar model holds no bars");
  return drawn;
}

/** Each group's points as `[x, ...the statistics' labels, compare value, change]`. */
function rows(drawn: BarsModel, statistics: readonly string[] = ["value"]) {
  return drawn.series.map((series) => [
    series.name,
    series.points.map((point) => [
      point.x,
      ...statistics.map((id) => point.values[id]?.label),
      point.compare?.value?.label ?? null,
      point.change?.value?.label ?? null,
    ]),
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("by row: each row's newest reading, largest first, with its interval, bound and reason", () => {
  const drawn = model(BY_ROW);
  expect([drawn.kind, drawn.name, drawn.title, drawn.comparable]).toEqual([
    "bar",
    "tasks/done",
    "Tasks done",
    true,
  ]);
  expect([drawn.bars.layout, drawn.bars.grouped, drawn.bars.mark, drawn.bars.legend]).toEqual([
    "upright",
    false,
    "bar",
    "none",
  ]);
  expect(drawn.series[0]?.statistics.map((statistic) => statistic.label)).toEqual([
    "Tasks done (tasks)",
    "Interval low",
    "Interval high",
  ]);
  expect(rows(drawn, ["value", "ci_low", "ci_high"])).toEqual([
    [
      "tasks-done",
      [
        // beta's newest reading is of col "", which stands for its older reading of col x.
        ["beta", "20", "18", "22", "22", "\u22122"],
        ["alpha", "14", "no interval", "no interval", "10", "+4"],
        ["delta", "\u2265 3", "no interval", "no interval", "no data", "no data"],
        // A reading with no value draws last, its reason in its place.
        ["gamma", "not measured: runner down", "no interval", "no interval", "no data", "no data"],
      ],
    ],
  ]);
  const points = drawn.series[0]?.points ?? [];
  expect(points.map((point) => [point.values.value?.state, point.bound, point.reason])).toEqual([
    ["plain", null, ""],
    ["plain", null, ""],
    ["plain", ">=", ""],
    ["absent", null, "runner down"],
  ]);
  expect(points[0]?.values.value?.note).toBe("two reruns");
  expect(points[2]?.values.value?.note).toBe("lower bound: the true value is at least this");
  expect(drawn.bars.notes).toEqual([
    "Ranges: 90-in-100.",
    "Lower bound: the true value is at least this.",
    "1 reading not plotted: a newer reading of another col stands for its row.",
  ]);
  expect(barKind.reads(BY_ROW)).toEqual([{ series: "tasks-done" }]);
});

test("order names categories first, order total sorts by value, and states, highlight and value states colour the bars", () => {
  expect(
    model({ ...BY_ROW, order: ["gamma", "delta"] }).bars.places.map((place) => place.x)
  ).toEqual(["gamma", "delta", "beta", "alpha"]);
  expect(model({ ...BY_ROW, order: "total" }).bars.places.map((place) => place.x)).toEqual([
    "beta",
    "alpha",
    "delta",
    "gamma",
  ]);
  const tones = (spec: DeclaredSpec) =>
    model(spec).series[0]?.points.map((point) => [point.x, point.tone]);
  expect(tones({ ...BY_ROW, highlight: "alpha" })).toEqual([
    ["beta", "other"],
    ["alpha", 0],
    ["delta", "other"],
    ["gamma", "other"],
  ]);
  expect(tones({ ...BY_ROW, states: { delta: "attention" } })).toEqual([
    ["beta", null],
    ["alpha", null],
    ["delta", "attention"],
    ["gamma", null],
  ]);
  const VALUE_STATES: DeclaredSpec = {
    ...BY_ROW,
    state_of: "tasks-ok",
    value_states: [
      { label: "Not shown", state: "attention" },
      { from: 1, label: "Shown", state: "proved" },
    ],
  };
  // Each bar takes the state of its row's newest tasks-ok reading; a row with none takes none.
  expect(tones(VALUE_STATES)).toEqual([
    ["beta", "proved"],
    ["alpha", "attention"],
    ["delta", null],
    ["gamma", null],
  ]);
  expect(model(VALUE_STATES).bars.legend).toBe("states");
  expect(barKind.reads(VALUE_STATES)).toEqual([{ series: "tasks-done" }, { series: "tasks-ok" }]);
});

test("grouped: a cluster per row and a bar per col, each against its reading's n", () => {
  const drawn = model(GROUPED);
  expect([drawn.bars.grouped, drawn.bars.of, drawn.bars.legend]).toEqual([true, true, "direct"]);
  expect(drawn.bars.key).toEqual([
    { name: "fast", tone: 0 },
    { name: "slow", tone: 1 },
  ]);
  expect(drawn.series[0]?.statistics.map((statistic) => statistic.label)).toEqual([
    "Tasks (tasks)",
    "Total",
  ]);
  expect(rows(drawn, ["value", "total"])).toEqual([
    [
      "tasks-by-suite \u00b7 fast",
      [
        ["suite-a", "8", "12 tasks", "6", "+2"],
        ["suite-b", "not measured: not open yet", "no total", "no data", "no data"],
      ],
    ],
    [
      "tasks-by-suite \u00b7 slow",
      [
        ["suite-a", "5", "12 tasks", "no data", "no data"],
        ["suite-b", "\u2265 4", "10 tasks", "no data", "no data"],
      ],
    ],
  ]);
  expect(drawn.bars.notes).toEqual(["Lower bound: the true value is at least this."]);

  render(createElement(EvalChart, { model: drawn, renderer: barKind.renderer }));
  const figure = screen.getByRole("figure", { name: "By suite" });
  // A count still growing draws as an open bar, labelled "so far"; a cell with no value draws its
  // reason at the foot of its place; each col is named under its bar.
  const growing = figure.querySelector(
    'rect[data-series="tasks-by-suite \u00b7 slow"][data-x="suite-b"][data-range="window"]'
  );
  expect(growing?.getAttribute("class")).toContain("fill-none");
  expect(growing?.getAttribute("stroke-dasharray")).toBe("3 2");
  const labels = [...figure.querySelectorAll('[data-value-label="suite-b"]')].map(
    (label) => label.textContent
  );
  expect(labels).toEqual(["\u2265 4 of 10 tasks so far"]);
  expect(figure.querySelector('[data-reason="suite-b"]')?.textContent).toContain("not open yet");
  expect(
    [...figure.querySelectorAll("[data-direct-label]")].map((label) => label.textContent)
  ).toEqual(["fast", "slow", "fast", "slow"]);
});

test("horizontal against totals: a row per category in its group's order, each a share of its total, a row with no total left out", () => {
  const drawn = model(HORIZONTAL);
  expect([
    drawn.bars.layout,
    drawn.bars.of,
    drawn.bars.valueAxis.share,
    drawn.bars.rowNotes,
  ]).toEqual(["horizontal", true, true, true]);
  expect(drawn.bars.places.map((place) => [place.x, place.heading])).toEqual([
    ["openssl", "Libraries"],
    ["curl", null],
  ]);
  expect(rows(drawn, ["value", "total"])).toEqual([
    [
      "proved",
      [
        ["openssl", "3", "11 statements", "no data", "no data"],
        ["curl", "19", "28 statements", "15", "+4"],
      ],
    ],
  ]);
  const curl = drawn.series[0]?.points[1];
  expect([curl?.note, curl?.compare?.total?.label, curl?.change?.total?.label]).toEqual([
    "milestone 2",
    "25 statements",
    "+3",
  ]);
  expect(drawn.bars.notes).toEqual([
    "1 row not drawn, with no total (no reading of statements): zlib.",
  ]);
  expect(barKind.reads(HORIZONTAL)).toEqual([{ series: "proved" }, { series: "statements" }]);

  render(createElement(EvalChart, { model: drawn, renderer: barKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Proved" });
  expect(figure.querySelector('[data-value-label="curl"]')?.textContent).toBe(
    "19 of 28 statements+4"
  );
  expect(figure.querySelector('[data-row-note="curl"]')?.textContent).toContain("milestone 2");
  expect(figure.querySelector("[data-row-heading]")?.textContent).toBe("Libraries");
});

test("a dot chart with columns: each row's dot, by value, and a cell of each column's series", () => {
  const drawn = model(DOTS);
  expect([drawn.bars.mark, drawn.bars.plotHeader]).toEqual(["dot", "Proved"]);
  expect(drawn.bars.columns).toEqual([
    { field: "note", header: "Owner", statistic: "column:0" },
    { field: "value", header: "Size", statistic: "column:1" },
  ]);
  // Each column's cells are a statistic of the chart's one series, which its table holds.
  expect(drawn.series[0]?.statistics.map((statistic) => statistic.label)).toEqual([
    "proved (statements)",
    "Owner",
    "Size",
  ]);
  expect(
    drawn.series[0]?.points.map((point) => [
      point.x,
      point.values.value?.label,
      point.tone,
      point.values["column:0"]?.label,
      point.values["column:1"]?.label,
    ])
  ).toEqual([
    ["curl", "19", "proved", "Ann", "2.5 MB"],
    ["zlib", "7", null, "", "none"],
    ["openssl", "3", null, "", "0.0 MB"],
  ]);
  // A cell whose reading is 0 prints in grey.
  expect(drawn.series[0]?.points[2]?.values["column:1"]?.state).toBe("absent");
  expect(barKind.reads(DOTS)).toEqual([
    { series: "proved" },
    { series: "owner" },
    { series: "size" },
  ]);

  render(createElement(EvalChart, { model: drawn, renderer: barKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Dots" });
  // Each number a column prints names itself as the table names it; a row with no reading, none.
  expect(
    [...figure.querySelectorAll('[data-column="column:1"]')].map((cell) => [
      cell.getAttribute("data-statistic"),
      cell.getAttribute("data-value"),
    ])
  ).toEqual([
    ["column:1", "2.5"],
    [null, null],
    ["column:1", "0"],
  ]);
  expect(figure.textContent).toContain("Proved");
});

test("a facet table: a row per row and a column per col, each cell's bar the sum of its parts", () => {
  const drawn = model(FACET);
  expect([drawn.bars.layout, drawn.bars.legend]).toEqual(["facet", "none"]);
  expect(drawn.bars.parts).toEqual([
    { name: "proved", state: null },
    { name: "bug", state: "attention" },
  ]);
  expect(drawn.bars.places.map((place) => [place.x, place.subtitle, place.unit])).toEqual([
    ["Programs", null, ""],
    ["Commits", "to the proofs repository", ""],
  ]);
  expect(rows(drawn, ["value", "part:proved", "part:bug"])).toEqual([
    [
      "throughput \u00b7 w1",
      [
        ["Programs", "8", "6", "2", "no data", "no data"],
        ["Commits", "40", "no data", "no data", "30", "+10"],
      ],
    ],
    [
      "throughput \u00b7 w2",
      [
        ["Programs", "9", "9", "no data", "no data", "no data"],
        ["Commits", "52", "no data", "no data", "no data", "no data"],
      ],
    ],
  ]);
  expect(drawn.series.map((series) => series.points.map((point) => point.note))).toEqual([
    ["6 finished", ""],
    ["", "release week"],
  ]);
  expect(drawn.bars.notes).toEqual([
    "1 reading not plotted: its row's parts stand for it in its col.",
  ]);

  render(createElement(EvalChart, { model: drawn, renderer: barKind.renderer }));
  const table = screen.getByRole("figure", { name: "Throughput" }).querySelector("[data-facet]");
  expect(table?.textContent).toContain("to the proofs repository");
  // The bug part takes its state's colour.
  const bug = table?.querySelector('rect[data-statistic="part:bug"]');
  expect(bug?.getAttribute("class")).toContain("fill-orange-700");
});

test("a mock-up draws its points as the chart's categories where nothing is measured", () => {
  const drawn = model({
    id: "soon",
    kind: "bar",
    mockup: {
      points: [
        { series: "a", x: "row a", y: 1 },
        { series: "a", x: "row b", y: 3 },
      ],
      submitted_at: "2026-11-11T09:00:00Z",
      submitted_by: "Ann",
    },
    title: "Soon",
    y: { series: "none-yet" },
  });
  expect(drawn.bars.mockup).toBe("Mock-up: nothing is measured yet; Ann drew it on 2026-11-11.");
  expect(rows(drawn)).toEqual([
    [
      "none-yet",
      [
        ["row b", "3", "no data", "no data"],
        ["row a", "1", "no data", "no data"],
      ],
    ],
  ]);
  render(createElement(EvalChart, { model: drawn, renderer: barKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Soon" });
  expect(figure.querySelector("[data-mockup]")).not.toBeNull();
  // A mock-up prints no value label.
  expect(figure.querySelector("[data-value-label]")).toBeNull();
});

test("with no compare range no point holds a compare value", () => {
  for (const spec of [BY_ROW, GROUPED, HORIZONTAL, FACET]) {
    const drawn = model(spec, null);
    expect(
      drawn.series.every((series) => series.points.every((point) => point.compare === null))
    ).toBe(true);
  }
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
  ["by row", BY_ROW],
  ["grouped", GROUPED],
  ["horizontal against totals", HORIZONTAL],
  ["a dot chart with columns", DOTS],
  ["a facet table", FACET],
] as [
  string,
  DeclaredSpec,
][])("the table holds every number the graph draws: %s", async (_name, spec) => {
  const user = userEvent.setup();
  const drawn = model(spec);
  render(createElement(EvalChart, { model: drawn, renderer: barKind.renderer }));
  const figure = screen.getByRole("figure", { name: drawn.title });
  const graph = drawnValues(figure);
  expect(graph.length).toBeGreaterThan(0);
  if (spec !== DOTS) {
    // Each category's compare value, and its change printed beside its value.
    expect(figure.querySelectorAll('[data-value][data-range="compare"]').length).toBeGreaterThan(0);
    expect(figure.querySelectorAll('[data-value][data-range="change"]').length).toBeGreaterThan(0);
  }

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});

test("the compare range draws each category's newest value as a tick across its bar, its change over the value", () => {
  render(createElement(EvalChart, { model: model(BY_ROW), renderer: barKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Tasks done" });
  const tick = figure.querySelector('line[data-x="beta"][data-range="compare"]');
  expect(tick?.getAttribute("class")).toContain("stroke-slate-300");
  expect(tick?.getAttribute("data-value")).toBe("22");
  expect(figure.querySelector('[data-change-label="beta"]')?.textContent).toBe("\u22122");
  expect(figure.querySelector('[data-change-label="alpha"]')?.textContent).toBe("+4");

  cleanup();
  render(createElement(EvalChart, { model: model(BY_ROW, null), renderer: barKind.renderer }));
  const plain = screen.getByRole("figure", { name: "Tasks done" });
  expect(plain.querySelector('[data-range="compare"]')).toBeNull();
  expect(plain.querySelector("[data-change-label]")).toBeNull();
});
