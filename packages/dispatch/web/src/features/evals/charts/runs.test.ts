import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/runs-readings.json";
import type { DeclaredInput, DeclaredSpec } from "./kind";
import { readingsOf } from "./readings";
import { runsKind } from "./runs";
import { isRunsModel, RUN_COLUMNS, type RunsModel } from "./runs-model";

// The runs kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20; spec R20): each run a row of
// the readings of `__fixtures__/runs-readings.json`, with its settings, tags and final numbers in
// the run table and its numbers over steps for the runs a reader picks; each run that started in
// the compare range marked, its numbers the compare range's; and the chart's table holding every
// number its graph draws.

const TRAINING: DeclaredSpec = {
  columns: [
    { setting: "lr" },
    { tag: "arm" },
    { final: "usd" },
    { final: "loss" },
    { setting: "batch" },
  ],
  id: "training-runs",
  kind: "runs",
  name: "loss",
  step_label: "Step",
  title: "Training runs",
};

function input(compare: EvalRange | null = fixture.compare): DeclaredInput {
  return {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "training",
    window: fixture.window,
  };
}

function model(given = input()): RunsModel {
  const drawn = runsKind.model(TRAINING, given);
  if (!isRunsModel(drawn)) throw new Error("the runs model holds no runs");
  return drawn;
}

/** One series' values of `statistic` as `[x, value, compare value, change]` labels. */
function rows(drawn: RunsModel, series: string, statistic: string) {
  const found = drawn.series.find((one) => one.name === series);
  return found?.points.map((point) => [
    point.x,
    point.values[statistic]?.label,
    point.compare?.[statistic]?.label ?? null,
    point.change?.[statistic]?.label ?? null,
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("a runs chart reads its name's series and each final column's", () => {
  expect(runsKind.reads(TRAINING)).toEqual([{ series: "loss" }, { series: "usd" }]);
});

test("each run a row, newest start first, its columns the spec's then every other the runs hold", () => {
  const drawn = model();
  expect([drawn.name, drawn.kind, drawn.title, drawn.comparable, drawn.axis.label]).toEqual([
    "training/training-runs",
    "runs",
    "Training runs",
    true,
    "Run",
  ]);
  expect(drawn.runs.stepLabel).toBe("Step");
  // A setting is a number column when each run that holds it holds a number; lr's "auto" is not.
  expect(
    drawn.runs.columns.map((column) => [column.id, column.label, column.numeric, column.declared])
  ).toEqual([
    ["setting:lr", "lr", false, true],
    ["tag:arm", "arm (tag)", false, true],
    ["final:usd", "usd (final)", true, true],
    ["final:loss", "loss (final)", true, true],
    ["setting:batch", "batch", true, true],
    ["setting:model", "model", false, false],
    ["tag:void", "void (tag)", false, false],
  ]);
  // The accuracy reading is of no series the spec names, and n1 of no run.
  expect(drawn.runs.list.map((run) => [run.id, run.range, run.started.label, run.tone])).toEqual([
    ["run-b", "window", "2026-11-02 09:00 UTC", 0],
    ["run-a", "window", "2026-11-01 10:00 UTC", 1],
    ["run-c", "compare", "2026-10-30 08:00 UTC", 2],
  ]);
  // A later reading's setting stands over an earlier one's (run-a's lr), a number setting is
  // written as the run holds it, and a value a run does not hold is "not set".
  expect(
    drawn.runs.list.map((run) =>
      drawn.runs.columns.map((column) => {
        const cell = run.cells[column.id];
        return [cell?.label, cell?.value, cell?.state];
      })
    )
  ).toEqual([
    [
      ["0.01", null, "plain"],
      ["b", null, "plain"],
      ["not set", null, "absent"],
      ["0.5", 0.5, "plain"],
      ["64", 64, "plain"],
      ["m-small", null, "plain"],
      ["not set", null, "absent"],
    ],
    [
      ["0.001", null, "plain"],
      ["a", null, "plain"],
      ["12.5", 12.5, "plain"],
      ["0.4", 0.4, "plain"],
      ["32", 32, "plain"],
      ["m-small", null, "plain"],
      ["no", null, "plain"],
    ],
    [
      ["auto", null, "plain"],
      ["not set", null, "absent"],
      ["9", 9, "plain"],
      ["0.7", 0.7, "plain"],
      ["16", 16, "plain"],
      ["not set", null, "absent"],
      ["not set", null, "absent"],
    ],
  ]);
  expect(drawn.runs.list[1]?.cells["final:loss"]?.note).toBe("early stop");
  // The series the runs hold numbers of at steps: usd holds none.
  expect(drawn.runs.names).toEqual([{ name: "loss", unit: "nats" }]);
  // A step not measured says why; the corrected step 3 draws its correction.
  expect(
    drawn.runs.list[0]?.steps.loss?.map((one) => [
      one.step,
      one.x,
      one.value.label,
      one.value.value,
      one.value.state,
      one.value.note,
    ])
  ).toEqual([
    [1, "run-b@1", "0.8", 0.8, "plain", null],
    [2, "run-b@2", "not measured: diverged", null, "absent", "diverged"],
    [3, "run-b@3", "0.5", 0.5, "plain", null],
  ]);
});

test("the table's rows: each run, then its steps; a run of the compare range holds its numbers as compare values", () => {
  const drawn = model();
  expect(
    drawn.series.map((series) => [series.name, series.statistics.map((one) => one.label)])
  ).toEqual([
    [
      RUN_COLUMNS,
      ["lr", "arm (tag)", "usd (final)", "loss (final)", "batch", "model", "void (tag)", "Started"],
    ],
    ["loss", ["loss (nats)"]],
  ]);
  expect(drawn.series[0]?.points.map((point) => point.label)).toEqual([
    "run-b",
    "run-b \u00b7 Step 1",
    "run-b \u00b7 Step 2",
    "run-b \u00b7 Step 3",
    "run-a",
    "run-a \u00b7 Step 1",
    "run-a \u00b7 Step 2",
    "run-a \u00b7 Step 3",
    "run-c \u00b7 started in the compare range",
    "run-c \u00b7 Step 1",
    "run-c \u00b7 Step 2",
  ]);
  // Each series holds a point at every row, in one order.
  expect(drawn.series[1]?.points.map((point) => point.x)).toEqual(
    drawn.series[0]?.points.map((point) => point.x)
  );
  expect(rows(drawn, RUN_COLUMNS, "final:usd")).toEqual([
    ["run-b", "not set", "", ""],
    ["run-b@1", "", "", ""],
    ["run-b@2", "", "", ""],
    ["run-b@3", "", "", ""],
    ["run-a", "12.5", "", ""],
    ["run-a@1", "", "", ""],
    ["run-a@2", "", "", ""],
    ["run-a@3", "", "", ""],
    // The run that started in the compare range: its number is the compare range's, and no run
    // has a change, since each is in one range.
    ["run-c", "", "9", ""],
    ["run-c@1", "", "", ""],
    ["run-c@2", "", "", ""],
  ]);
  expect(rows(drawn, "loss", "value")).toEqual([
    ["run-b", "", "", ""],
    ["run-b@1", "0.8", "", ""],
    ["run-b@2", "not measured: diverged", "", ""],
    ["run-b@3", "0.5", "", ""],
    ["run-a", "", "", ""],
    ["run-a@1", "0.9", "", ""],
    ["run-a@2", "0.6", "", ""],
    ["run-a@3", "0.4", "", ""],
    ["run-c", "", "", ""],
    ["run-c@1", "", "1", ""],
    ["run-c@2", "", "0.7", ""],
  ]);
});

test("with no compare range no run is marked and no point holds a compare value", () => {
  const drawn = model(input(null));
  expect(drawn.runs.list.every((run) => run.range === "window")).toBe(true);
  expect(drawn.series[0]?.points.find((point) => point.x === "run-c")?.label).toBe("run-c");
  expect(
    drawn.series.every((series) =>
      series.points.every((point) => point.compare === null && point.change === null)
    )
  ).toBe(true);
});

test("the run table lists the window's runs, and while the compare range is shown the runs that started in it, marked", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(), renderer: runsKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Training runs" });
  const table = () => within(figure).getByRole("table", { name: "Training runs: the runs" });
  const listed = () =>
    [...table().querySelectorAll("tr[data-row]")].map((row) => row.getAttribute("data-row"));
  expect(listed()).toEqual(["run-b", "run-a", "run-c"]);
  expect(
    [...table().querySelectorAll("th")].map((header) => header.textContent?.replace(/[▲▼]/g, ""))
  ).toEqual(["Pick", "Run", "lr", "arm (tag)", "usd (final)", "loss (final)", "batch", "Started"]);
  expect(table().querySelector('[data-run="run-c"] [data-run-compare]')?.textContent).toBe(
    "started in the compare range"
  );
  expect(table().querySelector('[data-run="run-a"] [data-run-compare]')).toBeNull();
  expect(
    table()
      .querySelector('[data-statistic="final:usd"][data-x="run-c"]')
      ?.getAttribute("data-range")
  ).toBe("compare");

  // The columns the spec does not name are the reader's to add.
  await user.click(within(figure).getByText("Choose columns"));
  await user.click(within(figure).getByRole("checkbox", { name: "void (tag)" }));
  expect(table().querySelector('[data-statistic="tag:void"][data-x="run-a"]')?.textContent).toBe(
    "no"
  );

  // With the compare range turned off, only the window's runs.
  await user.click(within(figure).getByRole("checkbox", { name: "Compare" }));
  expect(listed()).toEqual(["run-b", "run-a"]);
});

// D10: a reading's `links` are its row's own links, and the run view draws each run's under its id.
test("each run's row draws the links its readings carry, through the link list", () => {
  const link = {
    href: "https://ci.example.com/runs/run-a",
    label: "run-a's log",
    recorded_at: "2026-11-02T09:00:00Z",
    rel: "via",
  };
  const rows = (fixture.items as EvalItemRow[]).map((row) =>
    row.row_key === "run-a" && row.cols?.series === "usd" ? { ...row, links: [link] } : row
  );
  const given = { ...input(), readings: readingsOf(rows) };
  expect(model(given).runs.list.find((run) => run.id === "run-a")?.links).toEqual([link]);
  expect(model(given).runs.list.find((run) => run.id === "run-b")?.links).toEqual([]);

  render(createElement(EvalChart, { model: model(given), renderer: runsKind.renderer }));
  const table = within(screen.getByRole("figure", { name: "Training runs" })).getByRole("table", {
    name: "Training runs: the runs",
  });
  const links = within(table).getByRole("list", { name: "Links of run run-a" });
  const anchor = within(links).getByRole("link", { name: /run-a's log/ });
  expect(anchor).toHaveAttribute("href", "https://ci.example.com/runs/run-a");
  expect(anchor).toHaveAttribute("data-link-kind", "via");
  expect(within(table).queryByRole("list", { name: "Links of run run-b" })).toBeNull();
});

test("the picked runs draw over their steps, and their settings stand side by side, the ones that differ first", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(), renderer: runsKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Training runs" });
  const overTime = within(figure).getByRole("region", { name: "The picked runs over time" });
  expect(overTime.textContent).toContain("Pick runs in the table to draw them over their steps.");

  await user.click(within(figure).getByRole("checkbox", { name: "Pick run run-a" }));
  await user.click(within(figure).getByRole("checkbox", { name: "Pick run run-b" }));
  // The number over steps starts at the spec's name.
  expect(within(overTime).getByRole<HTMLSelectElement>("combobox", { name: "Number" }).value).toBe(
    "loss"
  );
  const dots = (run: string) =>
    [...overTime.querySelectorAll(`[data-run-line="${run}"] circle`)].map((dot) =>
      ["data-x", "data-range", "data-value"].map((name) => dot.getAttribute(name)).join(" ")
    );
  // In the table's order; run-b's step 2, not measured, draws no dot.
  expect(
    [...overTime.querySelectorAll("[data-run-line]")].map((line) =>
      line.getAttribute("data-run-line")
    )
  ).toEqual(["run-b", "run-a"]);
  expect(dots("run-b")).toEqual(["run-b@1 window 0.8", "run-b@3 window 0.5"]);
  expect(dots("run-a")).toEqual(["run-a@1 window 0.9", "run-a@2 window 0.6", "run-a@3 window 0.4"]);
  expect(
    [...overTime.querySelectorAll("svg text[data-axis]")].map((text) => text.textContent)
  ).toEqual(["Step", "loss (nats)"]);

  const settings = within(overTime).getByRole("table", {
    name: "Training runs: the picked runs' settings",
  });
  expect(
    [...settings.querySelectorAll("tr[data-row]")].map((row) =>
      [...row.querySelectorAll("td")].map((cell) => cell.textContent)
    )
  ).toEqual([
    // The ones that differ, then the rest, each in the run table's order of its columns.
    ["lr", "0.01", "0.001"],
    ["batch", "64", "32"],
    ["model", "m-small", "m-small"],
  ]);
  expect(overTime.querySelector("[data-runs-settings-differ]")?.textContent).toBe(
    "2 of 3 settings differ between the picked runs."
  );

  // A run that started in the compare range draws dashed, its dots hollow and the compare range's.
  await user.click(within(figure).getByRole("checkbox", { name: "Pick run run-c" }));
  const compareLine = overTime.querySelector('[data-run-line="run-c"]');
  expect(compareLine?.getAttribute("data-run-range")).toBe("compare");
  expect(compareLine?.querySelector("path")?.getAttribute("stroke-dasharray")).toBe("4 3");
  expect(dots("run-c")).toEqual(["run-c@1 compare 1", "run-c@2 compare 0.7"]);
  expect(
    within(overTime)
      .getAllByRole("listitem")
      .map((item) => item.textContent)
  ).toEqual(["run-b", "run-a", "run-c (compare range)"]);
});

/** Each value drawn under `root`, by what it names: series, statistic, place, range and value. */
function drawnValues(root: Element): string[] {
  return [...root.querySelectorAll("[data-value]")].map((element) =>
    ["data-series", "data-statistic", "data-x", "data-range", "data-value"]
      .map((name) => element.getAttribute(name))
      .join(" ")
  );
}

test("the table holds every number the graph draws", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(), renderer: runsKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Training runs" });
  for (const run of ["run-a", "run-b", "run-c"]) {
    await user.click(within(figure).getByRole("checkbox", { name: `Pick run ${run}` }));
  }
  await user.click(within(figure).getByText("Choose columns"));
  for (const column of ["model", "void (tag)"]) {
    await user.click(within(figure).getByRole("checkbox", { name: column }));
  }
  const graph = drawnValues(figure);
  // The run table's numbers, each picked run's numbers over steps, its numeric settings side by
  // side, the window's and the compare range's.
  expect(graph).toContain(`${RUN_COLUMNS} final:usd run-a window 12.5`);
  expect(graph).toContain(`${RUN_COLUMNS} setting:batch run-c compare 16`);
  expect(graph).toContain("loss value run-c@2 compare 0.7");
  expect(graph.filter((value) => value.startsWith("loss value "))).toHaveLength(7);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});

test("an eval that holds no run says so", () => {
  render(
    createElement(EvalChart, {
      model: model({ ...input(), readings: [] }),
      renderer: runsKind.renderer,
    })
  );
  const figure = screen.getByRole("figure", { name: "Training runs" });
  expect(figure.querySelector("[data-runs-empty]")?.textContent).toBe(
    "This eval holds no run yet."
  );
});
