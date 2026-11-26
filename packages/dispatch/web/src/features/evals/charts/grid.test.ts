import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/grid-readings.json";
import { type GridModel, type GridSeries, gridKind } from "./grid";
import { type DeclaredInput, type DeclaredSpec, type MarkState, STATE_TEXT } from "./kind";
import { readingsOf } from "./readings";

// The grid kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's values, states,
// notes and order from the readings of `__fixtures__/grid-readings.json`, each cell's compare
// value and change, its forms by its count of cells, and its table holding every number its graph
// draws.

const WINDOW = fixture.window as EvalRange;
const COMPARE = fixture.compare as EvalRange;

function input(compare: EvalRange | null = COMPARE, items = fixture.items): DeclaredInput {
  return {
    compare,
    readings: readingsOf(items as EvalItemRow[]),
    slug: "audit",
    window: WINDOW,
  };
}

function model(spec: DeclaredSpec, given = input()): GridModel {
  const drawn = gridKind.model(spec, given);
  if (!("grid" in drawn)) throw new Error("the grid model holds no grid");
  return drawn as GridModel;
}

/** Each point of a column as `[row, label of statistic, compare label, change label]`. */
function cells(series: GridSeries | undefined, statistic: string) {
  return series?.points.map((point) => [
    point.x,
    point.values[statistic]?.label,
    point.compare?.[statistic]?.label ?? null,
    point.change?.[statistic]?.label ?? null,
  ]);
}

/** Each point of a column as the text of each line it prints. */
function lines(series: GridSeries | undefined, part: "lines" | "compareLines" = "lines") {
  return series?.points.map((point) => [
    point.x,
    point[part].map((line) => line.map((piece) => piece.text).join("")),
  ]);
}

const AUDIT: DeclaredSpec = {
  id: "each-item",
  kind: "grid",
  title: "Each audited item",
  x: { label: "Check", series: null },
  y: { label: "Item", series: "audit" },
};

/** A reading row of `series` at a row and col, for the grids too large for the fixture. */
function itemRow(id: string, row: string, col: string, value: number, range = "window") {
  return {
    cols: {
      attributes: {},
      ci_high: null,
      ci_level: null,
      ci_low: null,
      col,
      id,
      n: null,
      n_unit: "",
      note: "",
      recorded_at: "2026-11-02T00:05:00Z",
      replaces: "",
      series: "wide",
      unit: "passed",
      value,
      value_absent_reason: "",
    },
    href: "",
    links: [],
    measured_at: range === "window" ? "2026-11-02T00:00:00Z" : "2026-10-30T00:00:00Z",
    range,
    row_key: row,
    row_kind: "row",
    source: "auditor",
  };
}

/** A grid of `rows` rows at col c0 and `cols` more cols at row r0, so it has rows times cols + 1
 *  cells from rows + cols readings. */
function wide(rows: number, cols: number) {
  return [
    ...Array.from({ length: rows }, (_, index) =>
      itemRow(`r${index}`, `r${index}`, "c0", index % 2)
    ),
    ...Array.from({ length: cols }, (_, index) =>
      itemRow(`c${index + 1}`, "r0", `c${index + 1}`, 1)
    ),
    itemRow("then", "r1", "c0", 0, "compare"),
  ];
}

const WIDE: DeclaredSpec = {
  id: "wide",
  kind: "grid",
  title: "A wide audit",
  y: { series: "wide" },
};

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("the grid reads its y series and the series of its totals, once", () => {
  expect(gridKind.reads({ ...AUDIT, of: "audit-total" })).toEqual([
    { series: "audit" },
    { series: "audit-total" },
  ]);
  expect(gridKind.reads({ ...AUDIT, of: "n" })).toEqual([{ series: "audit" }]);
  expect(gridKind.reads(AUDIT)).toEqual([{ series: "audit" }]);
});

test("one row for each row, each row group together, and one column for each col, as they came", () => {
  const drawn = model(AUDIT);
  expect(drawn.name).toBe("audit/each-item");
  expect(drawn.kind).toBe("grid");
  expect(drawn.title).toBe("Each audited item");
  expect(drawn.comparable).toBe(true);
  expect(drawn.compareRange).toEqual(COMPARE);
  expect(drawn.axis).toEqual({ label: "Item", many: "rows", one: "row" });
  expect(drawn.grid.rowTitle).toBe("Item");
  expect(drawn.grid.colTitle).toBe("Check");
  // item-3 draws with item-1 in row group core, before item-2's extra; item-4, read only in the
  // compare range, after them.
  expect(drawn.grid.rows).toEqual([
    { group: "core", key: "item-1", label: "item-1" },
    { group: "core", key: "item-3", label: "item-3" },
    { group: "extra", key: "item-2", label: "item-2" },
    { group: "", key: "item-4", label: "item-4" },
  ]);
  expect(drawn.series.map((series) => [series.name, series.label, series.col])).toEqual([
    ["audit \u00b7 build", "build", "build"],
    ["audit \u00b7 proof", "proof", "proof"],
  ]);
  for (const series of drawn.series) {
    expect(series.points.map((point) => point.x)).toEqual(["item-1", "item-3", "item-2", "item-4"]);
  }
  expect(drawn.grid.form).toBe("text");
  expect(drawn.grid.cells).toBe(8);

  // order names item-2: it draws first, and its row group with it.
  expect(model({ ...AUDIT, order: ["item-2", "item-9"] }).grid.rows.map((row) => row.key)).toEqual([
    "item-2",
    "item-1",
    "item-3",
    "item-4",
  ]);
});

test("each cell prints its newest reading, its interval, its earlier readings, the reason it has no value, or not read", () => {
  const drawn = model(AUDIT);
  const [build, proof] = drawn.series;
  expect(build?.statistics).toEqual([
    { id: "value", label: "Value" },
    { id: "earlier", label: "Earlier readings" },
  ]);
  expect(proof?.statistics).toEqual([
    { id: "value", label: "Value" },
    { id: "ci_low", label: "Interval low" },
    { id: "ci_high", label: "Interval high" },
  ]);
  expect(lines(build)).toEqual([
    // g2, item-1's second reading of build, over g1.
    ["item-1", ["1 passed", "+1 earlier"]],
    ["item-3", ["1 passed"]],
    ["item-2", ["\u2014 runner lost"]],
    ["item-4", ["not read"]],
  ]);
  expect(lines(proof)).toEqual([
    ["item-1", ["1 passed", "0.8 to 1 passed"]],
    // g7, which replaces g6's 5.
    ["item-3", ["0 passed"]],
    ["item-2", ["not read"]],
    ["item-4", ["not read"]],
  ]);
  expect(
    build?.points.map((point) => [point.values.value?.state, point.values.value?.note])
  ).toEqual([
    ["plain", null],
    ["plain", null],
    ["absent", "runner lost"],
    ["absent", null],
  ]);
  expect(build?.points[2]?.values.value?.label).toBe("not measured: runner lost");
  expect(build?.points[0]?.values.earlier).toEqual({
    label: "1",
    note: null,
    state: "plain",
    value: 1,
  });
  expect(proof?.points[0]?.values).toMatchObject({
    ci_high: { label: "1 passed", value: 1 },
    ci_low: { label: "0.8 passed", value: 0.8 },
    value: { label: "1 passed", note: "checked by hand", value: 1 },
  });
  expect(proof?.points[1]?.values.ci_low?.label).toBe("no interval");
  expect(proof?.points.map((point) => point.tip)).toEqual([
    "item-1, proof: 1 passed (checked by hand)",
    "item-3, proof: 0 passed",
    "item-2, proof: not read",
    "item-4, proof: not read",
  ]);
  expect(drawn.grid.notes).toEqual(["Ranges: 95-in-100."]);
  expect(
    model({ ...AUDIT, interval_label: "by hand", interval_level_in: "footnote" }).grid.notes
  ).toEqual(["Ranges: by hand."]);
});

test("with neither of nor value_states each cell is shaded by its value across the grid's", () => {
  const drawn = model(AUDIT);
  expect(drawn.series.map((series) => series.points.map((point) => point.shade))).toEqual([
    [1, 1, null, null],
    [1, 0, null, null],
  ]);
  expect(drawn.grid.key).toEqual([
    { high: "1 passed", kind: "shade", low: "0 passed" },
    { kind: "empty", label: "no value" },
  ]);
  expect(drawn.series.every((series) => series.points.every((point) => point.state === null))).toBe(
    true
  );
});

test("of: each cell out of its total, shaded by its share, and a cell with no total alone and white", () => {
  const bySeries = model({ ...AUDIT, of: "audit-total" });
  const [build] = bySeries.series;
  expect(build?.statistics.map((statistic) => statistic.id)).toEqual(["value", "total", "earlier"]);
  expect(lines(build)).toEqual([
    ["item-1", ["1 of 2 checks", "+1 earlier"]],
    ["item-3", ["1 passed"]],
    ["item-2", ["\u2014 runner lost"]],
    ["item-4", ["not read"]],
  ]);
  expect(cells(build, "total")).toEqual([
    ["item-1", "2 checks", "2 checks", "no change"],
    ["item-3", "no total", "no data", "no data"],
    ["item-2", "no total", "no data", "no data"],
    ["item-4", "no data", "no total", "no data"],
  ]);
  expect(build?.points.map((point) => point.shade)).toEqual([0.5, null, null, null]);
  expect(build?.points[0]?.tip).toBe("item-1, build: 1 passed of 2 checks");
  expect(bySeries.grid.key).toEqual([
    { high: "100% of its total", kind: "shade", low: "0%" },
    { kind: "empty", label: "no value" },
  ]);

  // of n: the reading's own n, in its n_unit.
  const byN = model({ ...AUDIT, of: "n" });
  expect(lines(byN.series[0])?.[0]).toEqual(["item-1", ["1 of 4 tasks", "+1 earlier"]]);
  expect(cells(byN.series[0], "total")?.[0]).toEqual(["item-1", "4 tasks", "4 tasks", "no change"]);
  expect(byN.series[0]?.points[0]?.shade).toBe(0.25);
});

test("value_states: each cell takes the state of the last entry its value reaches, and the key names each", () => {
  const drawn = model({
    ...AUDIT,
    value_states: [
      { label: "Fails", state: "attention" },
      { from: 1, label: "Passes", state: "proved" },
    ],
  });
  const [build, proof] = drawn.series;
  expect(build?.points.map((point) => [point.state, point.stateLabel, point.compareState])).toEqual(
    [
      ["proved", "Passes", "attention"],
      ["proved", "Passes", null],
      [null, null, null],
      [null, null, "attention"],
    ]
  );
  expect(proof?.points.map((point) => point.state)).toEqual(["proved", "attention", null, null]);
  expect(drawn.series.every((series) => series.points.every((point) => point.shade === null))).toBe(
    true
  );
  expect(build?.points[0]?.tip).toBe("item-1, build: 1 passed, Passes");
  expect(drawn.grid.key).toEqual([
    { kind: "state", label: "Fails", state: "attention" },
    { kind: "state", label: "Passes", state: "proved" },
    { kind: "empty", label: "no value" },
  ]);
  expect(drawn.grid.notStartedGrey).toBe(false);
  expect(
    model({
      ...AUDIT,
      not_started_outline: "grey",
      value_states: [
        { label: "Not yet", state: "not-started" },
        { from: 1, label: "Done", state: "proved" },
      ],
    }).grid.notStartedGrey
  ).toBe(true);
});

// Ledger 38: the platform colours a cell's text by its state, with no swatch beside it.
test("value_states: a cell's text is written in its state's colour, with no swatch beside it", () => {
  const drawn = model({
    ...AUDIT,
    value_states: [
      { label: "Fails", state: "attention" },
      { from: 1, label: "Passes", state: "proved" },
    ],
  });
  render(createElement(EvalChart, { model: drawn, renderer: gridKind.renderer }));
  const cells = [...document.querySelectorAll("td[data-grid-cell]")];
  const stated = cells.filter((cell) => cell.hasAttribute("data-cell-state"));
  expect(stated.map((cell) => cell.getAttribute("data-cell-state")).sort()).toEqual([
    "attention",
    "proved",
    "proved",
    "proved",
  ]);
  for (const cell of stated) {
    const state = cell.getAttribute("data-cell-state") as MarkState;
    expect(cell.getAttribute("class")).toContain(STATE_TEXT[state]);
    expect(cell.querySelector("svg")).toBeNull();
  }
  // A cell with no state keeps the plain text colours: none of this grid's states' colours.
  for (const cell of cells.filter((one) => !one.hasAttribute("data-cell-state"))) {
    expect(cell.getAttribute("class")).not.toContain(STATE_TEXT.proved);
    expect(cell.getAttribute("class")).not.toContain(STATE_TEXT.attention);
  }
});

test("the compare range: each cell's newest compare reading and its change, and none with no compare range", () => {
  const drawn = model(AUDIT);
  const [build, proof] = drawn.series;
  expect(cells(build, "value")).toEqual([
    ["item-1", "1 passed", "0 passed", "+1 passed"],
    ["item-3", "1 passed", "no data", "no data"],
    ["item-2", "not measured: runner lost", "no data", "no data"],
    ["item-4", "no data", "0 passed", "no data"],
  ]);
  expect(cells(build, "earlier")?.[0]).toEqual(["item-1", "1", "0", "+1"]);
  expect(cells(proof, "value")?.[2]).toEqual(["item-2", "no data", "1 passed", "no data"]);
  expect(lines(build, "compareLines")).toEqual([
    ["item-1", ["was 0 passed \u00b7 +1 passed"]],
    ["item-3", []],
    ["item-2", []],
    ["item-4", ["was 0 passed"]],
  ]);

  const alone = model(AUDIT, input(null));
  expect(alone.compareRange).toBeNull();
  expect(alone.grid.rows.map((row) => row.key)).toEqual(["item-1", "item-3", "item-2"]);
  for (const series of alone.series) {
    for (const point of series.points) {
      expect([point.compare, point.change, point.compareLines]).toEqual([null, null, []]);
    }
  }
});

test("values in more than one unit: no cell is shaded and the grid has no key", () => {
  const drawn = model({ id: "latency", kind: "grid", title: "Latency", y: { series: "latency" } });
  expect(drawn.grid.mixedUnits).toEqual(["ms", "s"]);
  expect(drawn.grid.key).toEqual([]);
  expect(drawn.series.map((series) => [series.name, series.label])).toEqual([
    ["latency", "latency"],
  ]);
  expect(lines(drawn.series[0])).toEqual([
    ["a", ["120 ms"]],
    ["b", ["2 s"]],
  ]);
  expect(drawn.series[0]?.points.map((point) => point.shade)).toEqual([null, null]);
  expect(drawn.grid.rowTitle).toBeNull();
  expect(drawn.grid.colTitle).toBeNull();
  expect(drawn.axis.label).toBe("Row");
});

test("a mock-up draws its points as readings where the y series holds no reading", () => {
  const drawn = model({
    id: "sketch",
    kind: "grid",
    mockup: {
      ci_level: 0.8,
      points: [
        { col: "c1", n: 4, row: "r1", series: "a", y: 3 },
        { ci_high: 2, ci_low: 0, col: "c1", row: "r2", series: "a", y: 1 },
      ],
      submitted_at: "2026-11-11T09:00:00Z",
      submitted_by: "alice",
    },
    of: "n",
    title: "A sketch",
    y: { series: "nothing-yet" },
  });
  expect(drawn.grid.mockup).toBe("Mock-up: nothing is measured yet; alice drew it on 2026-11-11.");
  expect(drawn.grid.rows.map((row) => row.key)).toEqual(["r1", "r2"]);
  expect(lines(drawn.series[0])).toEqual([
    ["r1", ["3 of 4"]],
    ["r2", ["1", "0 to 2"]],
  ]);
  expect(drawn.series[0]?.points[0]?.values.value?.note).toBe("mock-up");
  expect(drawn.grid.notes).toEqual(["Ranges: 80-in-100."]);
  // A grid whose series holds readings draws them, not its mock-up.
  expect(
    model({ ...AUDIT, mockup: { points: [{ col: "c", row: "r", y: 1 }] } }).grid.mockup
  ).toBeNull();
});

test("more than 1,000 cells draw as squares, and more than 10,000 are not drawn", () => {
  const squares = model(WIDE, input(COMPARE, wide(41, 24) as typeof fixture.items));
  expect(squares.grid.cells).toBe(41 * 25);
  expect(squares.grid.form).toBe("squares");
  // On squares a cell with no reading takes the key's no-value entry.
  expect(squares.grid.key.at(-1)).toEqual({ kind: "empty", label: "no value" });

  const none = model(WIDE, input(COMPARE, wide(101, 99) as typeof fixture.items));
  expect(none.grid.cells).toBe(101 * 100);
  expect(none.grid.form).toBe("none");
  render(createElement(EvalChart, { model: none, renderer: gridKind.renderer }));
  expect(screen.getByText(/This grid has 10,100 cells/).textContent).toBe(
    "This grid has 10,100 cells: a grid of more than 10,000 is not drawn. Its table holds every reading."
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

test.each([
  ["by value", AUDIT, input()],
  ["out of a total", { ...AUDIT, of: "audit-total" }, input()],
  [
    "as squares",
    { ...WIDE, title: "Each audited item" },
    input(COMPARE, wide(41, 24) as typeof fixture.items),
  ],
] as [
  string,
  DeclaredSpec,
  DeclaredInput,
][])("the table holds every number the graph draws: %s", async (_name, spec, given) => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(spec, given), renderer: gridKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Each audited item" });
  const graph = drawnValues(figure);
  expect(graph.filter((value) => value.includes(" window ")).length).toBeGreaterThanOrEqual(3);
  expect(graph.filter((value) => value.includes(" compare ")).length).toBeGreaterThanOrEqual(1);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});

test("the graph draws each cell's lines, its shade and the key, and the compare range as a muted ghost with its change", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(AUDIT), renderer: gridKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Each audited item" });
  const cell = (row: string, col: string) =>
    figure.querySelector(`[data-grid-cell="${row}\n${col}"]`) as HTMLElement;
  // Each entry of the key: the shade's two ends, then the cell with no value.
  expect(
    [...within(figure).getByRole("list", { name: "Key" }).children].map(
      (entry) => entry.textContent
    )
  ).toEqual(["0 passed to 1 passed", "no value"]);
  expect(figure.querySelectorAll("[data-row-group]").length).toBe(2);
  expect(cell("item-1", "build").getAttribute("data-cell-shade")).toBe("1.000");
  expect(cell("item-1", "build").getAttribute("title")).toBe("item-1, build: 1 passed");
  const ghost = cell("item-1", "build").querySelector("[data-ghost]");
  expect(ghost?.textContent).toBe("was 0 passed \u00b7 +1 passed");
  expect(
    [...(ghost?.querySelectorAll("[data-value]") ?? [])].map((part) => [
      part.getAttribute("data-range"),
      part.getAttribute("data-value"),
    ])
  ).toEqual([
    ["compare", "0"],
    ["change", "1"],
  ]);
  expect(cell("item-4", "build").textContent).toBe("not readwas 0 passed");
  expect(within(figure).getByText("Ranges: 95-in-100.")).toBeTruthy();

  await user.click(within(figure).getByRole("checkbox", { name: "Compare" }));
  expect(figure.querySelector("[data-ghost]")).toBeNull();
  expect(cell("item-4", "build").textContent).toBe("not read");
});
