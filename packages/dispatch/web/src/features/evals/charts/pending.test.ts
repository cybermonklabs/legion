import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/pending-readings.json";
import type { DeclaredInput, DeclaredSpec } from "./kind";
import { pendingKind, pendingModel } from "./pending";
import { readingsOf } from "./readings";

// The pending kind (dispatch://INFRA-2918/artifact/plan-md, D4, D10, T20): an approved graph whose
// kind is not built yet draws its owner's mock-up, as a line or as bars on axes that name no
// scale, or a placeholder naming the chart where it has none; it reads none of the eval's readings
// (`__fixtures__/pending-readings.json`), draws no compare, keeps its builder in an attribute
// alone, and its table holds every number of the mock-up.

const BUILDER = "dispatch://LINUX-1515";

const LINE: DeclaredSpec = {
  approved: "dispatch://DEMO-1/ask/1",
  builder: BUILDER,
  id: "milestone-map",
  kind: "pending",
  mockup: {
    points: [
      { series: "proved", x: 3, y: 5 },
      { series: "proved", x: 1, y: 2 },
      { series: "open", x: 1, y: 9 },
      { series: "proved", x: 3, y: 6 },
      { series: "open", x: 2, y: 7 },
    ],
    shape: "line",
    submitted_at: "2026-11-11T09:00:00Z",
    submitted_by: "Ann",
    x: { label: "Week", unit: "" },
    y: { label: "Packages", unit: "packages" },
  },
  note: "The graph work is building this map.",
  title: "Packages with a proof",
};

const BARS: DeclaredSpec = {
  builder: BUILDER,
  id: "milestone-bars",
  kind: "pending",
  mockup: {
    points: [
      { series: "curl", x: "Mon", y: 2 },
      { series: "tls", x: "Tue", y: 4 },
      { series: "curl", x: "Tue", y: 3 },
    ],
    shape: "bar",
    submitted_at: "2026-11-12T10:00:00Z",
    submitted_by: "Bo",
    x: { label: "Day", unit: "" },
    y: { label: "Count", unit: "" },
  },
  title: "Statements a day",
};

const NONE: DeclaredSpec = {
  builder: BUILDER,
  id: "milestone-later",
  kind: "pending",
  title: "Of the 31 packages, 5 have any proof",
};

function input(compare: EvalRange | null = fixture.compare): DeclaredInput {
  return {
    compare,
    readings: readingsOf(fixture.items as EvalItemRow[]),
    slug: "dependency-graph",
    window: fixture.window,
  };
}

/** Each series' points as `[x, value, label, state]`. */
function points(spec: DeclaredSpec) {
  return pendingModel(spec, input()).series.map((series) => [
    series.name,
    series.points.map((point) => [
      point.x,
      point.values.y?.value ?? null,
      point.values.y?.label,
      point.values.y?.state,
    ]),
  ]);
}

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("a line mock-up: one series a name in the order the points name it, each at every x in order, no compare", () => {
  expect(pendingKind.reads(LINE)).toEqual([]);
  const drawn = pendingModel(LINE, input());
  expect([drawn.name, drawn.kind, drawn.title, drawn.comparable]).toEqual([
    "dependency-graph/milestone-map",
    "pending",
    "Packages with a proof",
    false,
  ]);
  expect(drawn.pending.builder).toBe(BUILDER);
  expect(drawn.pending.placeholder).toBeNull();
  expect(drawn.pending.sketch).toMatchObject({
    line: "Mock-up: nothing is measured yet; Ann drew it on 2026-11-11.",
    shape: "line",
    xTitle: "Week (mock-up, no scale)",
    yTitle: "Packages (packages) (mock-up, no scale)",
  });
  // A series' second point at one x takes a place of its own, after the first.
  expect(drawn.pending.sketch?.places.map((place) => [place.x, place.at])).toEqual([
    ["1", 1],
    ["2", 2],
    ["3", 3],
    ["3 (2)", 3],
  ]);
  expect(drawn.series.map((series) => [series.name, series.tone, series.statistics])).toEqual([
    ["proved", 0, [{ id: "y", label: "Packages (packages)" }]],
    ["open", 1, [{ id: "y", label: "Packages (packages)" }]],
  ]);
  expect(points(LINE)).toEqual([
    [
      "proved",
      [
        ["1", 2, "2", "plain"],
        ["2", null, "no data", "absent"],
        ["3", 5, "5", "plain"],
        ["3 (2)", 6, "6", "plain"],
      ],
    ],
    [
      "open",
      [
        ["1", 9, "9", "plain"],
        ["2", 7, "7", "plain"],
        ["3", null, "no data", "absent"],
        ["3 (2)", null, "no data", "absent"],
      ],
    ],
  ]);
  expect(drawn.series[0]?.points[0]?.values.y?.note).toBe("mock-up");
  // No compare value and no change, with a compare range or not: a pending chart takes none.
  for (const compare of [fixture.compare, null]) {
    const series = pendingModel(LINE, input(compare)).series;
    expect(
      series.every((one) =>
        one.points.every((point) => point.compare === null && point.change === null)
      )
    ).toBe(true);
  }
});

test("a bar mock-up: one place a category, in the order the points first name it", () => {
  const drawn = pendingModel(BARS, input());
  expect(drawn.pending.sketch?.shape).toBe("bar");
  expect(drawn.pending.sketch?.places.map((place) => [place.x, place.at])).toEqual([
    ["Mon", 0],
    ["Tue", 1],
  ]);
  expect(drawn.axis.label).toBe("Day");
  expect(points(BARS)).toEqual([
    [
      "curl",
      [
        ["Mon", 2, "2", "plain"],
        ["Tue", 3, "3", "plain"],
      ],
    ],
    [
      "tls",
      [
        ["Mon", null, "no data", "absent"],
        ["Tue", 4, "4", "plain"],
      ],
    ],
  ]);
});

test("with no mock-up: a placeholder naming the chart, and no series", () => {
  const drawn = pendingModel(NONE, input());
  expect(drawn.series).toEqual([]);
  expect(drawn.pending.sketch).toBeNull();
  render(createElement(EvalChart, { model: drawn, renderer: pendingKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Of the 31 packages, 5 have any proof" });
  const placeholder = figure.querySelector("[data-pending-placeholder]");
  expect(placeholder?.textContent).toBe(
    "Of the 31 packages, 5 have any proof: this graph is not built yet, and its owner has drawn no mock-up of it."
  );
  // The builder stands in an attribute, never in the text.
  expect(placeholder?.getAttribute("data-builder")).toBe(BUILDER);
  expect(figure.textContent).not.toContain(BUILDER);
  expect(within(figure).queryByRole("checkbox", { name: "Compare" })).toBeNull();
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
  ["a line", LINE, "Packages with a proof", 5],
  ["bars", BARS, "Statements a day", 3],
] as [
  string,
  DeclaredSpec,
  string,
  number,
][])("%s: the mock-up draws under a hatched MOCK-UP with no compare control, and the table holds every number", async (_name, spec, title, count) => {
  const user = userEvent.setup();
  render(
    createElement(EvalChart, { model: pendingModel(spec, input()), renderer: pendingKind.renderer })
  );
  const figure = screen.getByRole("figure", { name: title });
  // A pending chart takes no compare, though the header names a compare range.
  expect(within(figure).queryByRole("checkbox", { name: "Compare" })).toBeNull();
  expect(figure.querySelector("[data-compare-legend]")).toBeNull();
  expect(figure.querySelector("[data-mockup]")?.textContent).toBe("MOCK-UP");
  expect(figure.querySelector("[data-plot]")?.getAttribute("data-builder")).toBe(BUILDER);
  expect(figure.textContent).not.toContain(BUILDER);
  // Its axes carry their titles and no tick value.
  expect(
    [...figure.querySelectorAll("svg text[data-axis]")].map((text) => text.textContent)
  ).toEqual(
    spec === LINE
      ? ["Week (mock-up, no scale)", "Packages (packages) (mock-up, no scale)"]
      : ["Day (mock-up, no scale)", "Count (mock-up, no scale)"]
  );
  expect(figure.querySelectorAll("svg text")).toHaveLength(3);
  expect(
    [...within(figure).getByRole("list", { name: "Key" }).querySelectorAll("li")].map(
      (item) => item.textContent
    )
  ).toEqual(spec === LINE ? ["proved", "open"] : ["curl", "tls"]);
  const graph = drawnValues(figure);
  expect(graph).toHaveLength(count);
  expect(graph.every((value) => value.includes(" window "))).toBe(true);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});
