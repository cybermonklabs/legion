import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/band-readings.json";
import { bandKind } from "./band";
import type { DeclaredSpec } from "./kind";
import { isPlotModel, type PlotModel } from "./plot";
import { readingsOf } from "./readings";

// The band kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's medians, spreads,
// states, notes and order from the readings of `__fixtures__/band-readings.json`, the compare
// range's readings on the window's axis, and its table holding every number its graph draws.

const OVER_TIME: DeclaredSpec = {
  curve: "step",
  group: "col",
  highlight: "fast",
  id: "latency-band",
  interval_label: "lowest to highest run",
  kind: "band",
  title: "Latency",
  y: { label: "Latency", series: "latency" },
};

const BY_SIZE: DeclaredSpec = {
  id: "latency-by-size",
  kind: "band",
  title: "Latency",
  x: { label: "Input", series: "input-kb" },
  y: { label: "Latency", series: "latency-by-size" },
};

function model(spec: DeclaredSpec, compare: EvalRange | null = fixture.compare): PlotModel {
  const drawn = bandKind.model(spec, {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "latency",
    window: fixture.window,
  });
  if (!isPlotModel(drawn)) throw new Error("the band model holds no plot");
  return drawn;
}

/** Each series' points as `[x, median, low, high, compare median, change]`. */
function rows(drawn: PlotModel) {
  return drawn.series.map((series) => [
    series.name,
    series.points.map((point) => [
      point.x,
      point.values.y?.label,
      point.values.ci_low?.label,
      point.values.ci_high?.label,
      point.compare?.y?.label ?? null,
      point.change?.y?.label ?? null,
    ]),
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("over time: each group's medians and spreads in time, the highlighted group in the first colour", () => {
  const drawn = model(OVER_TIME);
  expect(drawn.kind).toBe("band");
  expect(drawn.plot.form).toBe("time");
  // A band always joins its points; curve step keeps each until the next.
  expect(drawn.plot.line).toBe("step");
  expect(drawn.series.map((series) => [series.name, series.tone])).toEqual([
    ["latency \u00b7 fast", 0],
    ["latency \u00b7 slow", "other"],
  ]);
  expect(drawn.series[0]?.statistics.map((statistic) => statistic.label)).toEqual([
    "Latency (seconds)",
    "Interval low",
    "Interval high",
  ]);
  expect(rows(drawn)).toEqual([
    [
      "latency \u00b7 fast",
      [
        // The compare range's 29 and 30 Oct, three days on.
        ["2026-11-01T00:00:00.000Z", "10", "8", "12", "9", "+1"],
        ["2026-11-02T00:00:00.000Z", "11", "9", "13", "10", "+1"],
        // A reading with no interval: a point with no band.
        ["2026-11-03T00:00:00.000Z", "12", "no interval", "no interval", "no data", "no data"],
      ],
    ],
    [
      "latency \u00b7 slow",
      [
        ["2026-11-01T00:00:00.000Z", "20", "15", "25", "no data", "no data"],
        ["2026-11-02T00:00:00.000Z", "no data", "no data", "no data", "no data", "no data"],
        ["2026-11-03T00:00:00.000Z", "no data", "no data", "no data", "no data", "no data"],
      ],
    ],
  ]);
  expect(
    drawn.series[0]?.points.map((point) => [point.values.y?.state, point.values.ci_low?.state])
  ).toEqual([
    ["plain", "plain"],
    ["plain", "plain"],
    ["plain", "absent"],
  ]);
  // An interval with no level reads as the spec's interval_label names it.
  expect(drawn.plot.notes).toEqual(["Ranges: 90-in-100; lowest to highest run."]);
});

test("against an x series: each row's median at its row's x, the compare range's at the same row", () => {
  const drawn = model(BY_SIZE);
  expect(drawn.plot.form).toBe("number");
  // A band joins its points against an x series too, straight where no curve says otherwise.
  expect(drawn.plot.line).toBe("linear");
  expect(drawn.series.map((series) => series.name)).toEqual(["latency-by-size"]);
  expect(
    drawn.series[0]?.points.map((point) => [
      point.x,
      point.at,
      point.values.x?.label,
      point.values.y?.label,
      point.values.ci_low?.label,
      point.compare?.y?.label,
      point.change?.y?.label,
    ])
  ).toEqual([
    ["in-1", 1, "1", "5", "4", "6", "\u22121"],
    ["in-2", 2, "2", "7", "6", "no data", "no data"],
    ["in-3", null, "no x reading", "9", "no interval", "no data", "no data"],
  ]);
  expect(drawn.plot.notes).toEqual([
    "Ranges: recorded interval.",
    "1 reading not plotted: no reading of input-kb in its row.",
  ]);
});

test("with no compare range no point holds a compare value", () => {
  const drawn = model(OVER_TIME, null);
  expect(
    drawn.series.every((series) => series.points.every((point) => point.compare === null))
  ).toBe(true);
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
  ["against an x series", BY_SIZE],
] as [
  string,
  DeclaredSpec,
][])("the table holds every number the graph draws: %s", async (_name, spec) => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(spec), renderer: bandKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Latency" });
  const graph = drawnValues(figure);
  // Each median and each end of its spread, the window's and the compare range's.
  expect(
    figure.querySelectorAll('[data-statistic="ci_low"][data-range="window"]').length
  ).toBeGreaterThanOrEqual(2);
  expect(
    figure.querySelectorAll('[data-value][data-range="compare"]').length
  ).toBeGreaterThanOrEqual(3);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});

test("the window's spread is shaded in its group's colour, and the compare range's drawn as an outline", () => {
  render(createElement(EvalChart, { model: model(OVER_TIME), renderer: bandKind.renderer }));
  const fast = screen
    .getByRole("figure", { name: "Latency" })
    .querySelector('[data-series-group="latency \u00b7 fast"]');
  const window = fast?.querySelector('path[data-band-range="window"]');
  expect(window?.getAttribute("class")).toContain("fill-sky-700/20");
  const compare = fast?.querySelector('path[data-band-range="compare"]');
  expect(compare?.getAttribute("class")).toContain("fill-none");
  expect(compare?.getAttribute("stroke-dasharray")).toBe("4 3");

  // Turned off, the compare range draws nothing.
  cleanup();
  render(createElement(EvalChart, { model: model(OVER_TIME, null), renderer: bandKind.renderer }));
  expect(
    screen.getByRole("figure", { name: "Latency" }).querySelector('[data-band-range="compare"]')
  ).toBeNull();
});
