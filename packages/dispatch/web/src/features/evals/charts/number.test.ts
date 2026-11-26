import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import { chartStateAttentionText } from "../../../theme/classes";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/number-readings.json";
import type { DeclaredSpec } from "./kind";
import { numberKind } from "./number";
import { isNumberModel, type NumberModel } from "./number-model";
import { readingsOf } from "./readings";

// The number kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's small numbers,
// totals, intervals, counts, states, dates, notes and order from the readings of
// `__fixtures__/number-readings.json`, each value's compare value and change, and its table
// holding every number its drawing prints.

const TILES: DeclaredSpec = {
  adapter: "graph-headline",
  id: "proof-numbers",
  interval_label: "lowest to highest task",
  kind: "number",
  sections: [
    {
      after: "risk-diagram",
      counts: { of: "n", order: ["Sandbox failures"], series: "incidents-by-kind" },
      subtitle: "Each fault found in a grader, counted once.",
      tiles: [{ label: "still unfixed", of: "n", series: "incidents-open" }],
      title: "Grader incidents",
    },
  ],
  series: [
    "proved",
    "in-scope",
    "wait-hours",
    "cost-change",
    "incidents-open",
    "incidents-by-kind",
    "spare",
  ],
  tiles: [
    { label: "statements proved", of: "in-scope", series: "proved" },
    { count: true, label: "median wait for a proof", series: "wait-hours", state: "attention" },
    { label: "cost against last week", series: "cost-change", signed: true },
  ],
  title: "Where the proofs stand",
};

const HEADLINE: DeclaredSpec = {
  id: "headline-number",
  kind: "number",
  sections: [
    {
      after: "risk-diagram",
      counts: { series: "incidents-by-kind" },
      tiles: [{ label: "open", series: "incidents-open" }],
      title: "Incidents",
    },
    {
      after: "risk-diagram",
      counts: { series: "retired-count" },
      tiles: [{ label: "never logged", series: "never" }],
      title: "Not counted yet",
    },
  ],
  series: [
    "proved",
    "in-scope",
    "flaky",
    "incidents-open",
    "incidents-by-kind",
    "never",
    "retired-count",
  ],
  title: "Statements proved",
};

function model(spec: DeclaredSpec, compare: EvalRange | null = fixture.compare): NumberModel {
  const drawn = numberKind.model(spec, {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "proofs",
    window: fixture.window,
  });
  if (!isNumberModel(drawn)) throw new Error("the number model holds no small numbers");
  return drawn;
}

/** Each small number as `[place, part, section, label, value, of, interval low, interval high,
 *  count, date, interval words, state, tone]`. */
function rows(drawn: NumberModel) {
  return (drawn.series[0]?.points ?? []).map((point) => [
    point.x,
    point.part,
    point.section,
    point.label,
    point.values.value?.label,
    point.values.of?.label ?? null,
    point.values.ci_low?.label ?? null,
    point.values.ci_high?.label ?? null,
    point.values.n?.label ?? null,
    point.date,
    point.interval,
    point.state,
    point.tone,
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("it reads each series its declaration lists, once", () => {
  expect(numberKind.reads({ ...TILES, series: ["proved", "in-scope", "proved"] })).toEqual([
    { series: "proved" },
    { series: "in-scope" },
  ]);
});

test("tiles: each tile's newest value with a value, its total, interval, count, state and date, then each section's tiles and counts", () => {
  const drawn = model(TILES);
  expect(drawn.kind).toBe("number");
  expect(drawn.name).toBe("proofs/proof-numbers");
  expect(drawn.comparable).toBe(true);
  expect(drawn.number.tiles).toBe(true);
  expect(drawn.series.map((series) => series.name)).toEqual(["proof-numbers"]);
  expect(drawn.series[0]?.statistics.map((statistic) => statistic.label)).toEqual([
    "Value",
    "Of",
    "Interval low",
    "Interval high",
    "Count",
  ]);
  expect(rows(drawn)).toEqual([
    // With a total, the value is bare and the total carries the unit: "19 of 28 statements". The
    // interval has a level; its day is the newest reading's, so it draws no date.
    [
      "proved",
      "tile",
      null,
      "statements proved",
      "19",
      "28 statements",
      "18",
      "20",
      null,
      null,
      "95% interval",
      null,
      null,
    ],
    // The newest reading has no value, so the one before it draws; its interval has no level, so
    // it takes the declaration's interval_label; count prints its n.
    [
      "wait-hours",
      "tile",
      null,
      "median wait for a proof",
      "39.9 h",
      null,
      "30.0 h",
      "52.0 h",
      "90 tasks",
      null,
      "lowest to highest task",
      "attention",
      null,
    ],
    // signed: a plus before a positive value; measured on an earlier day, so dated.
    [
      "cost-change",
      "tile",
      null,
      "cost against last week",
      "+12.5%",
      null,
      null,
      null,
      null,
      "2026-11-02",
      null,
      null,
      null,
    ],
    [
      "incidents-open",
      "tile",
      0,
      "still unfixed",
      "3",
      "7 unfixed",
      null,
      null,
      null,
      null,
      null,
      null,
      null,
    ],
    // The counts: rows order names first, each in its colour by that order; a row whose reading
    // has no value draws no count.
    [
      "incidents-by-kind \u00b7 Sandbox failures",
      "count",
      0,
      "Sandbox failures",
      "4 unfixed",
      "9",
      null,
      null,
      null,
      null,
      null,
      null,
      0,
    ],
    [
      "incidents-by-kind \u00b7 Proofs of False accepted",
      "count",
      0,
      "Proofs of False accepted",
      "21 unfixed",
      "42",
      null,
      null,
      null,
      "2026-11-01",
      null,
      null,
      1,
    ],
  ]);
  expect(drawn.series[0]?.points.map((point) => point.values.value?.note)).toEqual([
    null,
    null,
    "Against the week before.",
    null,
    null,
    null,
  ]);
  expect(drawn.number.sections).toEqual([
    {
      after: "risk-diagram",
      counts: "incidents-by-kind",
      countsEmpty: null,
      subtitle: "Each fault found in a grader, counted once.",
      title: "Grader incidents",
    },
  ]);
});

test("with no tiles: the headline, each next series no section owns beside it, then the sections", () => {
  const drawn = model(HEADLINE);
  expect(drawn.number.tiles).toBe(false);
  expect(drawn.series[0]?.statistics.map((statistic) => statistic.id)).toEqual([
    "value",
    "ci_low",
    "ci_high",
  ]);
  expect(rows(drawn)).toEqual([
    // The headline and each number beside it always draw their date.
    [
      "proved",
      "headline",
      null,
      "proved",
      "19 statements",
      null,
      "18",
      "20",
      null,
      "2026-11-03",
      "95% interval",
      null,
      null,
    ],
    [
      "in-scope",
      "beside",
      null,
      "in-scope",
      "28 statements",
      null,
      null,
      null,
      null,
      "2026-11-02",
      null,
      null,
      null,
    ],
    // A series whose readings have no value: not measured, for its reason.
    [
      "flaky",
      "beside",
      null,
      "flaky",
      "not measured",
      null,
      null,
      null,
      null,
      null,
      null,
      null,
      null,
    ],
    // incidents-open, incidents-by-kind, never and retired-count each draw in a section alone.
    [
      "incidents-open",
      "tile",
      0,
      "open",
      "3 unfixed",
      null,
      null,
      null,
      null,
      null,
      null,
      null,
      null,
    ],
    // With no order, the rows in the order of their first readings.
    [
      "incidents-by-kind \u00b7 Proofs of False accepted",
      "count",
      0,
      "Proofs of False accepted",
      "21 unfixed",
      null,
      null,
      null,
      null,
      "2026-11-01",
      null,
      null,
      0,
    ],
    [
      "incidents-by-kind \u00b7 Sandbox failures",
      "count",
      0,
      "Sandbox failures",
      "4 unfixed",
      null,
      null,
      null,
      null,
      null,
      null,
      null,
      1,
    ],
    // A series with no reading: no data, never 0.
    ["never", "tile", 1, "never logged", "no data", null, null, null, null, null, null, null, null],
  ]);
  const flaky = drawn.series[0]?.points[2]?.values.value;
  expect([flaky?.state, flaky?.value, flaky?.note]).toEqual([
    "absent",
    null,
    "the grader timed out",
  ]);
  expect(drawn.number.sections.map((section) => [section.title, section.countsEmpty])).toEqual([
    ["Incidents", null],
    ["Not counted yet", "retired-count: no reading with a value in this window."],
  ]);
});

test("each value's compare value is the compare range's newest with a value of its series and row, and its change", () => {
  const drawn = model(TILES);
  expect(
    drawn.series[0]?.points.map((point) => [
      point.x,
      point.compare?.value?.label,
      point.change?.value?.label,
      point.compare?.of?.label ?? null,
      point.change?.of?.label ?? null,
      point.compare?.n?.label ?? null,
      point.change?.n?.label ?? null,
      point.compare?.ci_low?.label ?? null,
    ])
  ).toEqual([
    ["proved", "15", "+4 statements", "28 statements", "no change", null, null, "no interval"],
    ["wait-hours", "45.0 h", "\u22125.1 h", null, null, "80 tasks", "+10 tasks", "no interval"],
    ["cost-change", "no data", "no data", null, null, null, null, null],
    ["incidents-open", "5", "\u22122 unfixed", "6 unfixed", "+1 unfixed", null, null, null],
    [
      "incidents-by-kind \u00b7 Sandbox failures",
      "no data",
      "no data",
      "no data",
      "no data",
      null,
      null,
      null,
    ],
    [
      "incidents-by-kind \u00b7 Proofs of False accepted",
      "18 unfixed",
      "+3 unfixed",
      "40",
      "+2",
      null,
      null,
      null,
    ],
  ]);
});

test("with no compare range no small number holds a compare value", () => {
  const drawn = model(TILES, null);
  expect(drawn.compareRange).toBeNull();
  expect(
    drawn.series[0]?.points.every((point) => point.compare === null && point.change === null)
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
  ["tiles and sections", TILES, "Where the proofs stand", 2],
  ["the headline and the numbers beside it", HEADLINE, "Statements proved", 1],
] as [
  string,
  DeclaredSpec,
  string,
  number,
][])("the table holds every number the drawing prints: %s", async (_name, spec, title, intervals) => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(spec), renderer: numberKind.renderer }));
  const figure = screen.getByRole("figure", { name: title });
  const graph = drawnValues(figure);
  // Each value, total, interval and count, and each compare value and change beside a value.
  expect(figure.querySelectorAll('[data-statistic="ci_low"][data-range="window"]')).toHaveLength(
    intervals
  );
  expect(
    figure.querySelectorAll('[data-statistic="value"][data-range="compare"][data-value]').length
  ).toBeGreaterThanOrEqual(3);
  expect(
    figure.querySelectorAll('[data-statistic="value"][data-range="change"]').length
  ).toBeGreaterThanOrEqual(3);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const table = within(figure).getByRole("table");
  const tabled = drawnValues(table);
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
  // One row for each small number.
  expect(table.querySelectorAll("[data-row]")).toHaveLength(
    model(spec).series[0]?.points.length ?? 0
  );
});

test("the drawing: a tile's value in its state's colour, a count's dot in its row's colour, each section after its chart, the compare beside each value", () => {
  render(createElement(EvalChart, { model: model(TILES), renderer: numberKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Where the proofs stand" });
  const wait = figure.querySelector('[data-number-place="wait-hours"]');
  expect(wait?.getAttribute("data-state")).toBe("attention");
  // The platform colours the text that states the state, with no swatch beside it.
  expect(
    wait?.querySelector('[data-statistic="value"][data-range="window"]')?.getAttribute("class")
  ).toContain(chartStateAttentionText);
  expect(wait?.querySelector('[class*="bg-orange"]')).toBeNull();
  expect(
    figure
      .querySelector('[data-number-place="proved"] [data-statistic="value"][data-range="window"]')
      ?.getAttribute("class")
  ).not.toContain("text-orange");
  expect(wait?.textContent).toContain("from 90 tasks");
  expect(wait?.textContent).toContain("[30.0 h, 52.0 h] (lowest to highest task)");
  expect(figure.querySelector('[data-number-place="proved"]')?.textContent).toContain(
    "19of28 statements"
  );
  const sandbox = figure.querySelector(
    '[data-number-place="incidents-by-kind \u00b7 Sandbox failures"] [aria-hidden="true"]'
  );
  expect(sandbox?.getAttribute("class")).toContain("bg-sky-700");
  const section = within(figure).getByRole("region", { name: "Grader incidents" });
  expect(section.getAttribute("data-after")).toBe("risk-diagram");
  expect(section.textContent).toContain("Each fault found in a grader, counted once.");
  expect(figure.querySelector('[data-compared="proved"]')?.textContent).toBe(
    "vs 15 (+4 statements)"
  );
  // A compare range with no reading prints its absence and no change.
  expect(figure.querySelector('[data-compared="cost-change"]')?.textContent).toBe("vs no data");
  // The adapter is stored and nothing is drawn from it (the plan's decision 5d).
  expect(figure.textContent).not.toContain("graph-headline");

  // Turned off, the compare range prints nothing.
  cleanup();
  render(createElement(EvalChart, { model: model(TILES, null), renderer: numberKind.renderer }));
  expect(
    screen.getByRole("figure", { name: "Where the proofs stand" }).querySelector("[data-compared]")
  ).toBeNull();
});

test("the headline draws large with its date, and a counts series with no value draws its line", () => {
  render(createElement(EvalChart, { model: model(HEADLINE), renderer: numberKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Statements proved" });
  const headline = figure.querySelector('[data-number-part="headline"]');
  expect(
    headline?.querySelector('[data-statistic="value"][data-range="window"]')?.getAttribute("class")
  ).toContain("text-[22px]");
  expect(headline?.textContent).toContain("2026-11-03");
  expect(figure.querySelector('[data-number-place="flaky"]')?.textContent).toContain(
    "the grader timed out"
  );
  expect(figure.querySelector('[data-number-empty="retired-count"]')?.textContent).toBe(
    "retired-count: no reading with a value in this window."
  );
});
