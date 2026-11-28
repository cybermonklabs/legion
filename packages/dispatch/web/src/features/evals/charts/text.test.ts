import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/text-readings.json";
import type { DeclaredSpec } from "./kind";
import { readingsOf } from "./readings";
import { textKind } from "./text";
import { isTextModel, type TextModel } from "./text-model";

// The text kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's rows, headers,
// cells, states, notes and order from the readings of `__fixtures__/text-readings.json`, each
// value's compare value and change, and its table holding every number its drawing prints.

const STATES = [
  { label: "a proof in the last 7 days", state: "context" },
  { from: 7, label: "stalled", state: "attention" },
];

const TABLE: DeclaredSpec = {
  columns: [
    { bar: true, col: "proved", label: "Proved", width: 0.2 },
    { col: "hours", label: "Proof work" },
    { col: "days", label: "Days with no proof", value_states: STATES },
    { col: "pace", field: "note", label: "Pace", lead: true, value_states: STATES },
    { col: "next", field: "note", label: "Next step", width: 0.3 },
    { label: "Share proved", spark: "proved-share" },
    { bar: true, col: "dots", label: "Programs", mark: "dots" },
  ],
  id: "milestones",
  kind: "text",
  order: ["curl"],
  row_label: "Milestone",
  row_subtitles: { TLS: "9 programs", curl: ["28 memory-safety statements.", "Sami"] },
  series: "milestones",
  title: "Where each milestone stands",
};

const items = fixture.items as EvalItemRow[];

function model(
  spec: DeclaredSpec,
  compare: EvalRange | null = fixture.compare,
  rows: readonly EvalItemRow[] = items
): TextModel {
  const drawn = textKind.model(spec, {
    compare,
    readings: readingsOf(rows),
    slug: "milestones",
    window: fixture.window,
  });
  if (!isTextModel(drawn)) throw new Error("the text model holds no table");
  return drawn;
}

/** A column's cells as `[row, ...each statistic's label]`, then its state, bar and dots. */
function cells(drawn: TextModel, column: number) {
  const series = drawn.series[column];
  return (series?.points ?? []).map((cell) => [
    cell.x,
    ...(series?.statistics ?? []).map(({ id }) => cell.values[id]?.label ?? null),
    cell.state?.state ?? null,
    cell.bar,
    cell.dots,
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("it reads its own series and each spark column's", () => {
  expect(textKind.reads(TABLE)).toEqual([{ series: "milestones" }, { series: "proved-share" }]);
});

test("its rows: those order names first, then the rest by name, each with its subtitles", () => {
  const drawn = model(TABLE);
  expect(drawn.kind).toBe("text");
  expect(drawn.name).toBe("milestones/milestones");
  expect(drawn.axis.label).toBe("Milestone");
  expect(drawn.text.rows).toEqual([
    { name: "curl", subtitles: ["28 memory-safety statements.", "Sami"] },
    { name: "Linux", subtitles: [] },
    { name: "TLS", subtitles: ["9 programs"] },
    { name: "Zeta", subtitles: [] },
  ]);
  expect(drawn.text.empty).toBeNull();
  expect(drawn.text.notes).toEqual([]);
});

test("its columns: one series each, the header naming a unit every drawn value shares once", () => {
  const drawn = model(TABLE);
  expect(
    drawn.series.map((series) => [
      series.name,
      series.column.kind,
      series.column.header,
      series.column.width,
      series.statistics.map((statistic) => statistic.id),
    ])
  ).toEqual([
    ["milestones \u00b7 proved", "value", "Proved (statements)", 0.2, ["value", "n", "share"]],
    [
      "milestones \u00b7 hours",
      "value",
      "Proof work (agent hours)",
      null,
      ["value", "ci_low", "ci_high", "note"],
    ],
    // Its label names its unit already.
    ["milestones \u00b7 days", "value", "Days with no proof", null, ["value", "note"]],
    ["milestones \u00b7 pace \u00b7 note", "note", "Pace", null, ["note", "value"]],
    ["milestones \u00b7 next \u00b7 note", "note", "Next step", 0.3, ["note"]],
    // A share keeps its % in each cell, so the header names no unit.
    ["proved-share", "spark", "Share proved", null, ["value"]],
    ["milestones \u00b7 dots", "value", "Programs", null, ["value", "n", "share"]],
  ]);
  expect(drawn.series[5]?.column.done).toBe(1);
});

test("its cells: the newest reading of each row and col, with total, bar or dots, interval, note and state", () => {
  const drawn = model(TABLE);
  // The header names the unit, so the cells print their numbers bare; a bar runs to its n.
  expect(cells(drawn, 0)).toEqual([
    ["curl", "19", "28", "67.9%", null, 19 / 28, null],
    ["Linux", "no data", null, null, null, null, null],
    ["TLS", "9", "9", "100.0%", null, 1, null],
    ["Zeta", "1", "4", "25.0%", null, 0.25, null],
  ]);
  // A value with no reading reads no data; one whose reading has no value, not measured; a note
  // stands under the value where no column prints the col's notes.
  expect(cells(drawn, 1)).toEqual([
    ["curl", "0.43", null, null, "15.3 h of proof work for 0.97 h of code.", null, null, null],
    ["Linux", "not measured", null, null, null, null, null, null],
    ["TLS", "2.5", "2", "3", null, null, null, null],
    ["Zeta", "no data", null, null, null, null, null, null],
  ]);
  expect(drawn.series[1]?.points[1]?.values.value?.note).toBe("not started");
  expect(drawn.series[2]?.points.map((cell) => [cell.x, cell.state])).toEqual([
    ["curl", { label: "stalled", state: "attention" }],
    ["Linux", null],
    ["TLS", { label: "a proof in the last 7 days", state: "context" }],
    ["Zeta", null],
  ]);
  // A note column: its note, and under it the value with its unit; a row with no reading of the
  // col is empty.
  expect(cells(drawn, 3)).toEqual([
    ["curl", "stalled", "10 days with no proof", "attention", null, null],
    ["Linux", null, null, null, null, null],
    ["TLS", "22 Apr 2027", "6 days with no proof", "context", null, null],
    ["Zeta", null, null, null, null, null],
  ]);
  expect(cells(drawn, 4)).toEqual([
    ["curl", "Prove the parser.", null, null, null],
    ["Linux", "Start intake.", null, null, null],
    ["TLS", null, null, null, null],
    ["Zeta", null, null, null, null],
  ]);
  // A spark: the row's readings with a value over the window, and the newest beside them.
  expect(cells(drawn, 5)).toEqual([
    ["curl", "68.0%", null, null, null],
    ["Linux", "no data", null, null, null],
    ["TLS", "100.0%", null, null, null],
    ["Zeta", "no data", null, null, null],
  ]);
  expect(drawn.series[5]?.points.map((cell) => cell.spark.map((point) => point.value))).toEqual([
    [0.5, 0.6, 0.68],
    [],
    [1],
    [],
  ]);
  // Dots for a total of 20 or fewer, filled up to the value; a larger total draws as the bar.
  expect(cells(drawn, 6)).toEqual([
    ["curl", "2", "5", "40.0%", null, null, { filled: 2, total: 5 }],
    ["Linux", "no data", null, null, null, null, null],
    ["TLS", "30", "40", "75.0%", null, 0.75, null],
    ["Zeta", "no data", null, null, null, null, null],
  ]);
});

test("each value's compare value is the compare range's newest reading of its cell, and its change", () => {
  const drawn = model(TABLE);
  const curl = (column: number) => {
    const cell = drawn.series[column]?.points[0];
    return Object.fromEntries(
      Object.keys(cell?.values ?? {}).map((id) => [
        id,
        [cell?.compare?.[id]?.label ?? null, cell?.change?.[id]?.label ?? null],
      ])
    );
  };
  expect(curl(0)).toEqual({
    n: ["28", "no change"],
    share: ["53.6%", "+14.3%"],
    value: ["15", "+4 statements"],
  });
  // The compare reading has no note: its note is left out, and a note has no change.
  expect(curl(1)).toEqual({
    note: [null, null],
    value: ["0.5", "\u22120.07 agent hours"],
  });
  expect(curl(2)).toEqual({
    note: [null, null],
    value: ["8", "+7 days with no proof"],
  });
  expect(curl(5)).toEqual({ value: ["40.0%", "+28.0%"] });
  // A row with no compare reading.
  expect(drawn.series[0]?.points[2]?.compare).toEqual({
    n: { label: "no data", note: null, state: "absent", value: null },
    share: { label: "no data", note: null, state: "absent", value: null },
    value: { label: "no data", note: null, state: "absent", value: null },
  });
});

test("with no compare range no cell holds a compare value", () => {
  const drawn = model(TABLE, null);
  expect(
    drawn.series.every((series) =>
      series.points.every((cell) => cell.compare === null && cell.change === null)
    )
  ).toBe(true);
});

/** One reading row of series `many`, in row `row` with value `value`. */
function manyRow(row: string, value: number): EvalItemRow {
  return {
    cols: { col: "a", id: `many-${row}`, series: "many", unit: "", value },
    href: "",
    links: [],
    measured_at: "2026-11-02T09:00:00Z",
    range: "window",
    row_key: row,
    row_kind: "row",
    source: "test",
  };
}

test("it draws 40 rows at most and says how many it leaves out; with no reading, its line", () => {
  const many = Array.from({ length: 42 }, (_unused, at) =>
    manyRow(`r${String(at).padStart(2, "0")}`, at)
  );
  const spec: DeclaredSpec = {
    columns: [{ col: "a", label: "A" }],
    id: "many",
    kind: "text",
    order: ["r41"],
    series: "many",
    title: "Many",
  };
  const drawn = model(spec, null, many);
  expect(drawn.text.rows).toHaveLength(40);
  expect(drawn.text.rows.slice(0, 3).map((row) => row.name)).toEqual(["r41", "r00", "r01"]);
  expect(drawn.text.notes).toEqual(["2 rows are left out: a table draws 40 rows at most."]);

  const none = model({ ...spec, series: "nothing" }, null, many);
  expect(none.text.empty).toBe("No reading of nothing in this window.");
  render(createElement(EvalChart, { model: none, renderer: textKind.renderer }));
  expect(screen.getByRole("figure", { name: "Many" }).textContent).toContain(
    "No reading of nothing in this window."
  );
});

/** Each value drawn under `root`, by what it names: series, statistic, place, range and value. */
function drawnValues(root: Element): string[] {
  return [...root.querySelectorAll("[data-value]")].map((element) =>
    ["data-series", "data-statistic", "data-x", "data-range", "data-value"]
      .map((name) => element.getAttribute(name))
      .join(" ")
  );
}

test("the table holds every number the drawing prints", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(TABLE), renderer: textKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Where each milestone stands" });
  const graph = drawnValues(figure);
  expect(figure.querySelectorAll('[data-value][data-range="window"]').length).toBeGreaterThan(20);
  expect(figure.querySelectorAll('[data-value][data-range="compare"]').length).toBeGreaterThan(3);
  expect(figure.querySelectorAll('[data-value][data-range="change"]').length).toBeGreaterThan(3);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const table = within(figure).getByRole("table");
  const tabled = drawnValues(table);
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
  expect(table.querySelectorAll("[data-row]")).toHaveLength(4);
});

test("the drawing: widths, subtitles, states, a lead note, bars, dots, a spark and its done line, the compare beside each value", () => {
  render(createElement(EvalChart, { model: model(TABLE), renderer: textKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Where each milestone stands" });
  const table = within(figure).getByRole("table");
  const headers = within(table)
    .getAllByRole("columnheader")
    .map((header) => [header.textContent, (header as HTMLElement).style.width]);
  expect(headers).toEqual([
    ["Milestone", ""],
    ["Proved (statements)", "20%"],
    ["Proof work (agent hours)", ""],
    ["Days with no proof", ""],
    ["Pace", ""],
    ["Next step", "30%"],
    ["Share proved", ""],
    ["Programs", ""],
  ]);
  const row = (name: string) => table.querySelector(`[data-text-row="${name}"]`);
  const cell = (name: string, series: string) =>
    row(name)?.querySelector(`[data-text-cell="${series}"]`);
  expect(within(row("curl") as HTMLElement).getByRole("rowheader").textContent).toBe(
    "curl28 memory-safety statements.Sami"
  );
  const days = cell("curl", "milestones \u00b7 days");
  expect(days?.getAttribute("data-state")).toBe("attention");
  expect(days?.getAttribute("title")).toBe("stalled");
  expect(days?.querySelector('[aria-hidden="true"]')?.getAttribute("class")).toContain(
    "bg-orange-700"
  );
  expect(days?.textContent).toContain("Stalled: no proof in the last 7 days.");
  expect(days?.querySelector("[data-compared]")?.textContent).toBe("vs 8 (+7 days with no proof)");
  const pace = cell("curl", "milestones \u00b7 pace \u00b7 note");
  expect(
    pace?.querySelector('[data-statistic="note"]')?.parentElement?.getAttribute("class")
  ).toContain("text-[15px]");
  expect(pace?.textContent).toContain("10 days with no proof");
  expect(
    cell("curl", "milestones \u00b7 proved")
      ?.querySelector("[data-text-bar] > span")
      ?.getAttribute("style")
  ).toContain(`width: ${(19 / 28) * 100}%`);
  const dots = cell("curl", "milestones \u00b7 dots")?.querySelectorAll("[data-filled]");
  expect([...(dots ?? [])].map((dot) => dot.getAttribute("data-filled"))).toEqual([
    "true",
    "true",
    "false",
    "false",
    "false",
  ]);
  const spark = cell("curl", "proved-share")?.querySelector("svg");
  expect(spark?.querySelector("polyline")?.getAttribute("points")?.split(" ")).toHaveLength(3);
  expect(spark?.querySelector("[data-text-done]")?.getAttribute("data-text-done")).toBe("1");
  expect(cell("curl", "proved-share")?.querySelector("[data-compared]")?.textContent).toBe(
    "vs 40.0% (+28.0%)"
  );

  // Turned off, the compare range prints nothing.
  cleanup();
  render(createElement(EvalChart, { model: model(TABLE, null), renderer: textKind.renderer }));
  expect(
    screen
      .getByRole("figure", { name: "Where each milestone stands" })
      .querySelector("[data-compared]")
  ).toBeNull();
});
