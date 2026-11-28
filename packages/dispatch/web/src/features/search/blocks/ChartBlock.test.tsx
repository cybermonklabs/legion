import { expect, test } from "bun:test";

import type { BlockOf } from "./block-cells";
import { chartOption, chartPoints } from "./ChartBlock";

// A chart of issues counted by status (design v2, state 5): each place along the axis in
// Dispatch's words, and a tooltip that shows a bar's or a point's value on hover, named as the
// counted field is headed. The canvas itself is echarts', which the unit suite does not draw.
const byStatus: BlockOf<"chart"> = {
  kind: "chart",
  title: "DEMO issues by status",
  spec: { mark: "bar", params: {}, source: "run", table: "chain", x: "status", y: "issues" },
  rows: [
    { issues: 1, status: "todo" },
    { issues: 2, status: "in_progress" },
    { issues: "many", status: "done" },
  ],
};

const palette = { axis: "#111", label: "#222", split: "#333" };

test("a chart's points are its rows with a number, each place in Dispatch's words", () => {
  expect(chartPoints(byStatus)).toEqual([
    { label: "Todo", value: 1 },
    { label: "In progress", value: 2 },
  ]);
});

// A chart along the status reads as the board does: its places stand in the order of the board's
// columns (`issueStatuses`), whatever order the read answered its groups in, and a status the
// board has no column for comes after them.
test("a chart by status puts its places in the order of the board's columns", () => {
  const outOfOrder: BlockOf<"chart"> = {
    ...byStatus,
    rows: ["todo", "triage", "someday", "done", "backlog", "icebox", "in_progress"].map(
      (status) => ({ issues: 1, status })
    ),
  };
  expect(chartPoints(outOfOrder).map((point) => point.label)).toEqual([
    "Triage",
    "Icebox",
    "Backlog",
    "Todo",
    "In progress",
    "Done",
    "someday",
  ]);
  // Along any other field the places keep the read's order.
  const byPriority: BlockOf<"chart"> = {
    ...byStatus,
    spec: { ...byStatus.spec, x: "priority" },
    rows: [2, 0, 1].map((priority) => ({ issues: 1, priority })),
  };
  expect(chartPoints(byPriority).map((point) => point.label)).toEqual(["P2", "P0", "P1"]);
});

test("hovering a bar or a point shows its value, the series named as its field is headed", () => {
  for (const mark of ["bar", "line"] as const) {
    const option = chartOption(chartPoints(byStatus), mark, "Issues", palette, 720);
    expect(option.tooltip).toEqual({ trigger: "axis" });
    expect(option.series).toEqual([
      expect.objectContaining({ data: [1, 2], name: "Issues", type: mark }),
    ]);
    expect(option.xAxis).toEqual(expect.objectContaining({ data: ["Todo", "In progress"] }));
    expect(option.animation).toBe(false);
  }
});

// Each bar or point carries its value above it (design v3, state 5), as the products the design
// follows label theirs, so the counts read without a hover.
test("each bar or point is labelled with its value", () => {
  for (const mark of ["bar", "line"] as const) {
    const option = chartOption(chartPoints(byStatus), mark, "Issues", palette, 720);
    expect(option.series).toEqual([
      expect.objectContaining({
        label: { color: palette.label, position: "top", show: true },
      }),
    ]);
  }
});

/** Seven places along the axis, as a chart of a project's issues by status draws them. */
const sevenStatuses: BlockOf<"chart"> = {
  ...byStatus,
  rows: ["triage", "todo", "in_progress", "in_review", "testing", "blocked", "done"].map(
    (status, index) => ({ issues: index + 1, status })
  ),
};

test("every place along the axis shows its label, level and wrapped at its words where it fits", () => {
  const points = chartPoints(sevenStatuses);
  expect(points).toHaveLength(7);
  // The bar's width: each of the seven places has about 93 px, and its label wraps within it.
  const option = chartOption(points, "bar", "Issues", palette, 720);
  expect(option.xAxis).toEqual(
    expect.objectContaining({
      axisLabel: {
        color: palette.label,
        hideOverlap: false,
        interval: 0,
        overflow: "break",
        width: 85,
      },
      data: points.map((point) => point.label),
    })
  );
});

test("where a place is too narrow for a word, every label still shows, slanted", () => {
  const points = chartPoints(sevenStatuses);
  // The side panel's width: each of the seven places has about 40 px.
  const option = chartOption(points, "bar", "Issues", palette, 344);
  expect(option.xAxis).toEqual(
    expect.objectContaining({
      axisLabel: {
        color: palette.label,
        hideOverlap: false,
        interval: 0,
        overflow: "truncate",
        rotate: 45,
        width: 96,
      },
    })
  );
  // Hovering still shows a bar's value.
  expect(option.tooltip).toEqual({ trigger: "axis" });
});
