import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/architecture-readings.json";
import { architectureKind } from "./architecture";
import { type ArchitectureModel, isArchitectureModel } from "./architecture-model";
import type { DeclaredSpec } from "./kind";
import { readingsOf } from "./readings";

// The architecture kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's parts,
// cells, bins, badges, values, notes and order from the part and edge readings of
// `__fixtures__/architecture-readings.json`, the second reading as the table's second series, each
// value's compare value and change printed beside it, and its table holding every number its
// diagram draws.

const DIAGRAM: DeclaredSpec = {
  bands: { above: "Read first", below: "Kept after" },
  bins: [1, 5, 15, 25],
  columns: ["Inputs", "Grading", "Kernels"],
  decimals: 1,
  // Accepted and drawn from nothing: Dispatch reads the parts from readings (plan D10).
  graph: { source_prefix: "ledgers/", team: "demo" },
  id: "grader-risk",
  key: { fault_shown: "a wrong grade shown here", kernel: "a kernel a grade uses" },
  kind: "architecture",
  numbered: 2,
  second_reading: "risk-deployed",
  title: "Where the risk sits",
  value_series: "risk",
};

function model(
  spec: DeclaredSpec = DIAGRAM,
  compare: EvalRange | null = fixture.compare
): ArchitectureModel {
  const drawn = architectureKind.model(spec, {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "grading",
    window: fixture.window,
  });
  if (!isArchitectureModel(drawn)) throw new Error("the architecture model holds no diagram");
  return drawn;
}

/** Each series' rows: each part's id, label, and the value's words in `part` of the point. */
function rows(drawn: ArchitectureModel, part: "values" | "compare" | "change") {
  return drawn.series.map((series) => [
    series.name,
    series.points.map((point) => [point.x, point.label, point[part]?.value?.label ?? null]),
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("an architecture reads its value series' and second reading's parts, and its eval's edges", () => {
  expect(architectureKind.reads(DIAGRAM)).toEqual([
    { rowKind: "part", series: "risk" },
    { rowKind: "part", series: "risk-deployed" },
    { rowKind: "edge" },
  ]);
  const { second_reading: _second, ...alone } = DIAGRAM;
  expect(architectureKind.reads(alone)).toEqual([
    { rowKind: "part", series: "risk" },
    { rowKind: "edge" },
  ]);
});

test("each part at its cell, column by column, shaded by its value's bin", () => {
  const drawn = model();
  expect([drawn.kind, drawn.name, drawn.title, drawn.comparable]).toEqual([
    "architecture",
    "grading/grader-risk",
    "Where the risk sits",
    true,
  ]);
  expect(drawn.axis).toEqual({ label: "Part", many: "parts", one: "part" });
  const { architecture } = drawn;
  expect(
    architecture.parts.map((part) => [
      part.id,
      part.kind,
      part.column,
      part.row,
      part.bin,
      part.badge,
      part.fault,
    ])
  ).toEqual([
    ["sandbox", "place", 0, 1, null, null, false],
    ["runner", "part", 0, 2, 2, null, false],
    ["parser", "part", 1, 0, 0, null, false],
    // The cache names no row: the first free row of its column.
    ["cache", "part", 1, 1, 4, 1, false],
    // The grader's reading of 2 Nov: its value, its fault shown. Tied with the kernel, both
    // take the second badge.
    ["grader", "part", 1, 2, 3, 2, true],
    ["kernel", "kernel", 2, 2, 3, 2, false],
    // No value: no bin, no badge.
    ["store", "part", 2, 3, null, null, false],
    // The audit names the store's row: the next free row below it.
    ["audit", "part", 2, 4, 0, null, false],
    ["vault", "place", 3, 1, null, null, false],
  ]);
  // A place frames its own cell and the parts it holds; an edge to the runner reaches the
  // sandbox, and none reaches the vault.
  expect(
    architecture.parts
      .filter((part) => part.kind === "place")
      .map((part) => [part.id, part.label, part.span, part.reached])
  ).toEqual([
    ["sandbox", "The sandbox", { column: 0, lastColumn: 0, lastRow: 2, row: 1 }, true],
    ["vault", "Key vault", { column: 3, lastColumn: 3, lastRow: 1, row: 1 }, false],
  ]);
  expect(architecture.parts.find((part) => part.id === "runner")?.within).toBe("sandbox");
  expect([architecture.rows, architecture.columns]).toEqual([
    5,
    ["Inputs", "Grading", "Kernels", ""],
  ]);
  expect([architecture.above, architecture.below]).toEqual([
    { first: 0, last: 0, title: "Read first" },
    { first: 3, last: 4, title: "Kept after" },
  ]);
  expect(architecture.bins).toEqual(["up to 1", "1 to 5", "5 to 15", "15 to 25", "over 25"]);
  expect([architecture.fault, architecture.kernel]).toEqual([
    "a wrong grade shown here",
    "a kernel a grade uses",
  ]);
  expect(architecture.edges.map((edge) => [edge.from, edge.to, edge.kind, edge.label])).toEqual([
    ["parser", "grader", "carried", "answers"],
    ["runner", "grader", "flow", ""],
    ["grader", "kernel", "kernel", ""],
    ["cache", "runner", "", ""],
  ]);
  expect(architecture.notes).toEqual(["1 edge not drawn: an end names no part of the window."]);
  expect(architecture.heaviest).toEqual([
    { badge: 1, id: "cache", name: "Response cache" },
    { badge: 2, id: "grader", name: "Grader" },
    { badge: 2, id: "kernel", name: "Lean kernel" },
  ]);
  expect(architecture.sources).toEqual([
    "dispatch://DEMO-1/artifact/ledger-md@v29",
    "dispatch://DEMO-1/artifact/ledger-md@v30",
  ]);
});

test("each part's value with its decimals and its note; the second reading the second series", () => {
  const drawn = model();
  expect(drawn.series.map((series) => [series.name, series.statistics])).toEqual([
    ["risk", [{ id: "value", label: "Value (points)" }]],
    ["risk-deployed", [{ id: "value", label: "Value (points)" }]],
  ]);
  expect(rows(drawn, "values")).toEqual([
    [
      "risk",
      [
        ["runner", "Task runner, with its harness", "7.3"],
        ["parser", "Answer parser", "0.5"],
        // A trailing zero stays.
        ["cache", "Response cache, shared", "30.0"],
        ["grader", "Grader; shared by every kind", "22.4"],
        ["kernel", "Lean kernel", "22.4"],
        ["store", "Record store", "no data"],
        // 0 is written as any number is.
        ["audit", "Audit log", "0"],
      ],
    ],
    [
      "risk-deployed",
      [
        ["runner", "Task runner, with its harness", "5.0"],
        ["parser", "Answer parser", "no data"],
        ["cache", "Response cache, shared", "no data"],
        ["grader", "Grader; shared by every kind", "18.0"],
        ["kernel", "Lean kernel", "no data"],
        ["store", "Record store", "no data"],
        ["audit", "Audit log", "no data"],
      ],
    ],
  ]);
  const risk = drawn.series[0]?.points ?? [];
  expect(risk.map((point) => point.values.value?.value)).toEqual([
    7.29,
    0.5,
    30,
    22.39,
    22.39,
    null,
    0,
  ]);
  expect(risk.map((point) => [point.values.value?.state, point.values.value?.note])).toEqual([
    ["plain", "Runs code the agent wrote"],
    ["plain", "Reads one format"],
    ["plain", "Serves stale grades"],
    ["plain", "A wrong grade was shown"],
    ["plain", "Checks every proof"],
    // The reason its reading gives for having no value.
    ["absent", "not measured"],
    ["plain", null],
  ]);
});

test("without decimals each value is written as any number is", () => {
  const { decimals: _decimals, ...plain } = DIAGRAM;
  expect(model(plain).series[0]?.points.map((point) => point.values.value?.label)).toEqual([
    "7.29",
    "0.5",
    "30",
    "22.39",
    "22.39",
    "no data",
    "0",
  ]);
});

test("the compare range: each value there and its change, no data where it holds none", () => {
  const drawn = model();
  expect(rows(drawn, "compare")).toEqual([
    [
      "risk",
      [
        ["runner", "Task runner, with its harness", "9.0"],
        ["parser", "Answer parser", "no data"],
        ["cache", "Response cache, shared", "no data"],
        ["grader", "Grader; shared by every kind", "22.4"],
        ["kernel", "Lean kernel", "no data"],
        ["store", "Record store", "no data"],
        ["audit", "Audit log", "no data"],
      ],
    ],
    [
      "risk-deployed",
      [
        ["runner", "Task runner, with its harness", "6.0"],
        ["parser", "Answer parser", "no data"],
        ["cache", "Response cache, shared", "no data"],
        ["grader", "Grader; shared by every kind", "no data"],
        ["kernel", "Lean kernel", "no data"],
        ["store", "Record store", "no data"],
        ["audit", "Audit log", "no data"],
      ],
    ],
  ]);
  // Each change with the diagram's decimals.
  expect(
    drawn.series.map((series) =>
      series.points.flatMap((point) =>
        point.change?.value?.value === null ? [] : [[point.x, point.change?.value?.label]]
      )
    )
  ).toEqual([
    [
      ["runner", "\u22121.7"],
      ["grader", "no change"],
    ],
    [["runner", "\u22121.0"]],
  ]);
  expect(drawn.series[0]?.points[0]?.change?.value?.value).toBeCloseTo(-1.71, 9);
  expect(drawn.series[0]?.points[0]?.compare?.value?.note).toBe("Ran unchecked code");
  expect(drawn.architecture.compareOnly).toEqual(["Old checker"]);
});

test("with no compare range no part holds a compare value", () => {
  const drawn = model(DIAGRAM, null);
  expect(
    drawn.series.every((series) =>
      series.points.every((point) => point.compare === null && point.change === null)
    )
  ).toBe(true);
  expect(drawn.architecture.compareOnly).toEqual([]);
});

/** Each value drawn under `root`, by what it names: series, statistic, place, range and value. */
function drawnValues(root: Element): string[] {
  return [...root.querySelectorAll("[data-value]")].map((element) =>
    ["data-series", "data-statistic", "data-x", "data-range", "data-value"]
      .map((name) => element.getAttribute(name))
      .join(" ")
  );
}

test("the table holds every number the diagram draws, the compare range's and the changes too", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(), renderer: architectureKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Where the risk sits" });
  const graph = drawnValues(figure);
  expect(graph).toEqual(
    expect.arrayContaining([
      "risk value runner window 7.29",
      "risk value audit window 0",
      "risk value runner compare 9",
      "risk value grader change 0",
    ])
  );
  // The second reading draws nothing on the diagram.
  expect(figure.querySelector('[data-series="risk-deployed"]')).toBeNull();

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
  expect(tabled).toEqual(
    expect.arrayContaining([
      "risk-deployed value runner window 5",
      "risk-deployed value runner compare 6",
      "risk-deployed value runner change -1",
    ])
  );
});

test("the diagram: shaded boxes, badges, the fault mark, places, bands and edges", () => {
  render(createElement(EvalChart, { model: model(), renderer: architectureKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Where the risk sits" });
  const box = (id: string) => figure.querySelector(`[data-architecture-part="${id}"] rect`);
  // The fourth of five bins draws at four fifths of the darkest shade.
  expect(box("grader")?.getAttribute("fill-opacity")).toBe("0.4");
  expect(box("parser")?.getAttribute("fill-opacity")).toBe("0.1");
  // A part of no value: a dashed white box with no number.
  expect(box("store")?.getAttribute("stroke-dasharray")).toBe("4 3");
  expect(box("store")?.getAttribute("class")).toContain("fill-white");
  expect(figure.querySelector('[data-architecture-part="store"] [data-range="window"]')).toBeNull();
  expect(
    [...figure.querySelectorAll("[data-badge]")].map((badge) => [
      badge.closest("[data-architecture-part]")?.getAttribute("data-architecture-part"),
      badge.textContent,
    ])
  ).toEqual([
    ["cache", "1"],
    ["grader", "2"],
    ["kernel", "2"],
  ]);
  expect(
    [...figure.querySelectorAll("[data-fault]")].map((mark) =>
      mark.closest("[data-architecture-part]")?.getAttribute("data-architecture-part")
    )
  ).toEqual(["grader"]);
  const frame = (id: string) => figure.querySelector(`[data-architecture-place="${id}"] rect`);
  expect(frame("sandbox")?.getAttribute("stroke-dasharray")).toBeNull();
  expect(frame("vault")?.getAttribute("stroke-dasharray")).toBe("5 4");
  expect(
    [...figure.querySelectorAll("[data-architecture-band]")].map((band) => band.textContent)
  ).toEqual(["Read first", "Kept after"]);
  expect(
    [...figure.querySelectorAll("[data-architecture-edge]")].map((edge) =>
      edge.getAttribute("data-architecture-edge")
    )
  ).toEqual(["carried", "flow", "kernel", "flow"]);
  const key = within(figure).getByRole("list", { name: "Key" });
  for (const words of ["up to 1", "over 25", "a wrong grade shown here", "a kernel a grade uses"]) {
    expect(within(key).getByText(words)).toBeTruthy();
  }
  expect([...figure.querySelectorAll("[data-heavy]")].map((item) => item.textContent)).toEqual([
    "1. Response cache: 30.0 \u2014 Serves stale grades",
    "2. Grader: 22.4 \u2014 A wrong grade was shown",
    "2. Lean kernel: 22.4 \u2014 Checks every proof",
  ]);
  expect(
    within(figure).getByText(
      "Values from 2 sources: dispatch://DEMO-1/artifact/ledger-md@v29, dispatch://DEMO-1/artifact/ledger-md@v30."
    )
  ).toBeTruthy();
});

// D10: the links a value's reading carries draw beside the sources line, through the link list.
test("the links of its values' readings draw under the sources", () => {
  const link = {
    href: "https://ledger.example.com/grader",
    label: "grader's row",
    recorded_at: "2026-11-02T09:00:00Z",
    rel: "describedby",
  };
  const rows = (fixture.items as EvalItemRow[]).map((row) =>
    row.row_key === "grader" && row.source.endsWith("@v30") ? { ...row, links: [link] } : row
  );
  const drawn = architectureKind.model(DIAGRAM, {
    compare: fixture.compare,
    readings: readingsOf(rows),
    slug: "grading",
    window: fixture.window,
  });
  if (!isArchitectureModel(drawn)) throw new Error("the architecture model holds no diagram");
  expect(drawn.architecture.links).toEqual([link]);
  expect(model().architecture.links).toEqual([]);

  render(createElement(EvalChart, { model: drawn, renderer: architectureKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Where the risk sits" });
  const links = within(figure).getByRole("list", {
    name: "Where the risk sits: links of its values",
  });
  expect(within(links).getByRole("link", { name: /grader's row/ })).toHaveAttribute(
    "href",
    "https://ledger.example.com/grader"
  );
});

test("each value prints its compare value and change beside it while the compare range shows", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(), renderer: architectureKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Where the risk sits" });
  const runner = figure.querySelector('[data-architecture-part="runner"]');
  expect(runner?.querySelector('[data-range="compare"]')?.textContent).toBe(" was 9.0");
  expect(runner?.querySelector('[data-range="change"]')?.textContent).toBe(" \u00b7 \u22121.7");
  expect(within(figure).getByText("In the compare range only: Old checker.")).toBeTruthy();

  await user.click(within(figure).getByRole("checkbox", { name: "Compare" }));
  expect(figure.querySelector('[data-range="compare"], [data-range="change"]')).toBeNull();
  expect(within(figure).queryByText("In the compare range only: Old checker.")).toBeNull();
});
