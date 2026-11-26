import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/interval-readings.json";
import { type BarsModel, isBarsModel } from "./bars";
import { intervalKind } from "./interval";
import type { DeclaredSpec } from "./kind";
import { readingsOf } from "./readings";

// The interval kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's points,
// intervals, states, notes and order from the readings of `__fixtures__/interval-readings.json`,
// by row and grouped, each category's newest compare value as a muted ghost with its change, and
// its table holding every number its graph draws.

const WINDOW = fixture.window as EvalRange;
const COMPARE = fixture.compare as EvalRange;

const BY_ROW: DeclaredSpec = {
  id: "pass",
  interval_label: "lowest to highest task",
  kind: "interval",
  title: "Pass rate",
  y: { label: "Pass rate", series: "pass-rate" },
};

const GROUPED: DeclaredSpec = {
  group: "col",
  id: "arms",
  kind: "interval",
  labels: "value",
  layout: "grouped",
  legend: "direct",
  order: ["t2"],
  reference: { label: "Half", y: 0.5 },
  title: "By arm",
  y: { series: "pass-by-arm" },
};

function model(spec: DeclaredSpec, compare: EvalRange | null = COMPARE): BarsModel {
  const drawn = intervalKind.model(spec, {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "passes",
    window: WINDOW,
  });
  if (!isBarsModel(drawn)) throw new Error("the interval model holds no bars");
  return drawn;
}

/** Each group's points as `[x, value, low, high, compare value, change]`. */
function rows(drawn: BarsModel) {
  return drawn.series.map((series) => [
    series.name,
    series.points.map((point) => [
      point.x,
      point.values.value?.label,
      point.values.ci_low?.label,
      point.values.ci_high?.label,
      point.compare?.value?.label ?? null,
      point.change?.value?.label ?? null,
    ]),
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("by row: each row's newest point and its interval, largest first, and the level of each interval under it", () => {
  const drawn = model(BY_ROW);
  expect([drawn.kind, drawn.bars.mark, drawn.bars.layout, drawn.bars.grouped]).toEqual([
    "interval",
    "point",
    "upright",
    false,
  ]);
  expect(drawn.series[0]?.statistics.map((statistic) => statistic.label)).toEqual([
    "Pass rate",
    "Interval low",
    "Interval high",
  ]);
  expect(rows(drawn)).toEqual([
    [
      "pass-rate",
      [
        ["b", "0.8", "0.7", "0.9", "no data", "no data"],
        ["a", "0.6", "0.5", "0.7", "0.55", "+0.05"],
        // A point with no interval reads "no interval".
        ["c", "0.4", "no interval", "no interval", "no data", "no data"],
        ["d", "0.3", "0.2", "0.4", "no data", "no data"],
      ],
    ],
  ]);
  const a = drawn.series[0]?.points[1];
  expect([a?.compare?.ci_low?.label, a?.compare?.ci_high?.label]).toEqual(["0.45", "0.65"]);
  expect(drawn.series[0]?.points[2]?.values.value?.note).toBe("one run");
  expect(
    drawn.series[0]?.points.map((point) => [point.values.ci_low?.state, point.values.value?.state])
  ).toEqual([
    ["plain", "plain"],
    ["plain", "plain"],
    ["absent", "plain"],
    ["plain", "plain"],
  ]);
  // d's interval has no level, which interval_label names.
  expect(drawn.bars.notes).toEqual(["Ranges: 90-in-100; lowest to highest task."]);
  expect(intervalKind.reads(BY_ROW)).toEqual([{ series: "pass-rate" }]);
});

test("interval_level_in leaves the levels out of the line under the chart", () => {
  const { interval_label: _label, ...unlabelled } = BY_ROW;
  expect(
    model({ ...unlabelled, interval_level_in: "note", note: "90% ranges." }).bars.notes
  ).toEqual([]);
  expect(model({ ...BY_ROW, interval_level_in: "note", note: "90% ranges." }).bars.notes).toEqual([
    "Ranges: lowest to highest task.",
  ]);
});

test("grouped: a cluster per row in the declared order, a point per col in its colour, each value printed beside it", () => {
  const drawn = model(GROUPED);
  expect([drawn.bars.grouped, drawn.bars.labels, drawn.bars.legend]).toEqual([
    true,
    true,
    "direct",
  ]);
  expect(drawn.bars.key).toEqual([
    { name: "base", tone: 0 },
    { name: "tuned", tone: 1 },
  ]);
  expect(drawn.bars.reference).toEqual({ label: "Half", state: null, type: "rule", y: 0.5 });
  expect(rows(drawn)).toEqual([
    [
      "pass-by-arm \u00b7 base",
      [
        ["t2", "0.45", "0.35", "0.55", "no data", "no data"],
        ["t1", "0.5", "0.4", "0.6", "no data", "no data"],
      ],
    ],
    [
      "pass-by-arm \u00b7 tuned",
      [
        ["t2", "not measured: not run", "no interval", "no interval", "no data", "no data"],
        ["t1", "0.7", "0.6", "0.8", "0.6", "+0.1"],
      ],
    ],
  ]);

  render(createElement(EvalChart, { model: drawn, renderer: intervalKind.renderer }));
  const figure = screen.getByRole("figure", { name: "By arm" });
  expect(
    [...figure.querySelectorAll("[data-value-label]")].map((label) => label.textContent)
  ).toEqual(["0.45", "0.5", "0.7"]);
  // Each col is named over its point in the first cluster.
  expect(
    [...figure.querySelectorAll("[data-direct-label]")].map((label) => label.textContent)
  ).toEqual(["base", "tuned"]);
  expect(figure.querySelector('[data-reference="rule"]')?.textContent).toBe("Half");
  expect(figure.querySelector('[data-reason="t2"]')?.textContent).toContain("not run");
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
] as [
  string,
  DeclaredSpec,
][])("the table holds every number the graph draws: %s", async (_name, spec) => {
  const user = userEvent.setup();
  const drawn = model(spec);
  render(createElement(EvalChart, { model: drawn, renderer: intervalKind.renderer }));
  const figure = screen.getByRole("figure", { name: drawn.title });
  const graph = drawnValues(figure);
  // Each point and each end of its interval, and the compare range's ghost with its change.
  expect(
    figure.querySelectorAll('[data-statistic="ci_low"][data-range="window"]').length
  ).toBeGreaterThanOrEqual(2);
  expect(
    figure.querySelectorAll('[data-value][data-range="compare"]').length
  ).toBeGreaterThanOrEqual(3);
  expect(figure.querySelectorAll('[data-value][data-range="change"]').length).toBe(1);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});

test("the compare range draws each category's newest point as a muted ghost beside it, its change over it", () => {
  render(createElement(EvalChart, { model: model(BY_ROW), renderer: intervalKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Pass rate" });
  const ghost = figure.querySelector('circle[data-x="a"][data-range="compare"]');
  expect(ghost?.getAttribute("class")).toContain("stroke-slate-300");
  expect(ghost?.getAttribute("data-value")).toBe("0.55");
  expect(figure.querySelector('[data-change-label="a"]')?.textContent).toBe("+0.05");
  // A point with no interval says so beside it.
  expect(figure.textContent).toContain("no interval");

  cleanup();
  render(createElement(EvalChart, { model: model(BY_ROW, null), renderer: intervalKind.renderer }));
  const plain = screen.getByRole("figure", { name: "Pass rate" });
  expect(plain.querySelector('[data-range="compare"]')).toBeNull();
  expect(
    model(BY_ROW, null).series.every((series) =>
      series.points.every((point) => point.compare === null)
    )
  ).toBe(true);
});
