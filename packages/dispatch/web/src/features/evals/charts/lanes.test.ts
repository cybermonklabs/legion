import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";

import type { EvalItemRow } from "../../../api/types";
import type { EvalRange } from "../chart-model";
import { EvalChart } from "../EvalChart";
import fixture from "./__fixtures__/lanes-readings.json";
import { type DeclaredInput, type DeclaredSpec, type MarkState, STATE_TEXT } from "./kind";
import { type LanesModel, lanesKind } from "./lanes";
import { readingsOf } from "./readings";

// The lanes kind (dispatch://INFRA-2918/artifact/plan-md, D10, T20): its model's lanes, states,
// marks, ends, row columns, notes and order from the readings of `__fixtures__/lanes-readings.json`,
// the compare range's readings shifted onto the window's axis, and its table holding every number
// its chart draws.

const WINDOW = fixture.window as EvalRange;
const COMPARE = fixture.compare as EvalRange;

function input(compare: EvalRange | null = COMPARE, items = fixture.items): DeclaredInput {
  return {
    compare,
    readings: readingsOf(items as EvalItemRow[]),
    slug: "adoption",
    window: WINDOW,
  };
}

function model(spec: DeclaredSpec, given = input()): LanesModel {
  const drawn = lanesKind.model(spec, given);
  if (!("lanes" in drawn)) throw new Error("the lanes model holds no lanes");
  return drawn as LanesModel;
}

const at = (time: string) => Date.parse(time);

const VALUE_STATES = [
  { label: "Waits on us", state: "attention" },
  { from: 1, label: "Waits on its maintainer", state: "context" },
  { from: 2, label: "Merged, no proof", state: "partial" },
  { from: 3, label: "Proved", state: "proved" },
];

const ENROLMENTS: DeclaredSpec = {
  annotations: [{ detail: "first wave", label: "Launch", x: "2026-11-06" }],
  end_heading: "Where it stands",
  id: "enrolments",
  kind: "lanes",
  marks: "events",
  row_columns: [
    {
      decimals: 0,
      empty_label: "none",
      label: "First reply, hours",
      series: "first-reply-hours",
      unit_in_label: true,
    },
    { field: "note", label: "Owner", series: "owner" },
  ],
  row_label: "Project",
  title: "Each enrolment",
  value_states: VALUE_STATES,
  x: { label: "", ticks: "week" },
  y: { series: "enrolment" },
};

/** Each lane's spans of one range as `[state, label, from, to, newest, dot]`. */
function spans(drawn: LanesModel, range: "window" | "compare") {
  return drawn.lanes.lanes.map((lane) => [
    lane.row,
    lane[range].spans.map((span) => [
      span.state,
      span.label,
      new Date(span.from).toISOString(),
      new Date(span.to).toISOString(),
      span.newest,
      span.dot,
    ]),
  ]);
}

/** Each point of a series as `[lane, label of statistic, compare label, change label]`. */
function points(drawn: LanesModel, index: number, statistic: string) {
  return drawn.series[index]?.points.map((point) => [
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

test("the lanes read their y series, their marks and each row column's series, once", () => {
  expect(
    lanesKind.reads({
      ...ENROLMENTS,
      row_columns: [
        { label: "First reply", series: "first-reply-hours" },
        { field: "note", label: "Owner", series: "owner" },
        { label: "Owner again", series: "owner" },
      ],
    })
  ).toEqual([
    { series: "enrolment" },
    { series: "events" },
    { series: "first-reply-hours" },
    { series: "owner" },
  ]);
  expect(lanesKind.reads({ kind: "lanes", y: { series: "enrolment" } })).toEqual([
    { series: "enrolment" },
  ]);
});

test("one lane for each row, as they came, its states from its readings, the newest to its newest reading", () => {
  const drawn = model(ENROLMENTS);
  expect(drawn.name).toBe("adoption/enrolments");
  expect(drawn.kind).toBe("lanes");
  expect(drawn.comparable).toBe(true);
  expect(drawn.compareRange).toEqual(COMPARE);
  expect(drawn.axis).toEqual({ label: "Project", many: "lanes", one: "lane" });
  // zlib, read only in the compare range, after the window's lanes.
  expect(drawn.lanes.lanes.map((lane) => lane.row)).toEqual([
    "sqlpage",
    "mod-host",
    "libreac",
    "curl",
    "zlib",
  ]);
  expect(drawn.lanes.drawn).toBe(5);
  expect(spans(drawn, "window")).toEqual([
    [
      "sqlpage",
      [
        // e1 and e2 draw alike: one state from e1 to e3.
        [
          "context",
          "Waits on its maintainer",
          "2026-11-02T09:00:00.000Z",
          "2026-11-06T09:00:00.000Z",
          false,
          false,
        ],
        [
          "partial",
          "Merged, no proof",
          "2026-11-06T09:00:00.000Z",
          "2026-11-10T09:00:00.000Z",
          false,
          false,
        ],
        // The newest state began at the newest reading: a dot.
        ["proved", "Proved", "2026-11-10T09:00:00.000Z", "2026-11-10T09:00:00.000Z", true, true],
      ],
    ],
    [
      "mod-host",
      [
        [
          "attention",
          "Waits on us",
          "2026-11-03T09:00:00.000Z",
          "2026-11-08T09:00:00.000Z",
          false,
          false,
        ],
        // Of m3 and m4, measured at one time, m4 was written later and stands.
        [
          "context",
          "Waits on its maintainer",
          "2026-11-08T09:00:00.000Z",
          "2026-11-12T09:00:00.000Z",
          true,
          false,
        ],
      ],
    ],
    [
      "libreac",
      [
        [
          "partial",
          "Merged, no proof",
          "2026-11-05T09:00:00.000Z",
          "2026-11-05T09:00:00.000Z",
          true,
          true,
        ],
      ],
    ],
    // k1 has no value: no state.
    [
      "curl",
      [
        [
          "attention",
          "Waits on us",
          "2026-11-07T09:00:00.000Z",
          "2026-11-07T09:00:00.000Z",
          true,
          true,
        ],
      ],
    ],
    ["zlib", []],
  ]);
  expect(drawn.lanes.lanes.map((lane) => lane.end)).toEqual([
    { state: "proved", text: "proved in CI" },
    { state: "context", text: "Waits on its maintainer" },
    { state: "partial", text: "Merged, no proof" },
    { state: "attention", text: "Waits on us" },
    null,
  ]);
  expect(drawn.lanes.notes).toEqual(["1 reading with no value not drawn."]);
});

test("each lane's newest value with its state, its marks, and its row columns' cells", () => {
  const drawn = model(ENROLMENTS);
  expect(drawn.series.map((series) => [series.name, series.label, series.statistics])).toEqual([
    ["enrolment", "enrolment", [{ id: "value", label: "Value" }]],
    ["first-reply-hours", "First reply, hours", [{ id: "value", label: "Value" }]],
    ["owner \u00b7 note", "Owner", [{ id: "note", label: "Note" }]],
  ]);
  for (const series of drawn.series) {
    expect(series.points.map((point) => point.x)).toEqual([
      "sqlpage",
      "mod-host",
      "libreac",
      "curl",
      "zlib",
    ]);
  }
  expect(drawn.series[0]?.points[0]?.values.value).toEqual({
    label: "3 (Proved)",
    note: "since 2026-11-10 09:00 UTC; proved in CI",
    state: "plain",
    value: 3,
  });
  expect(drawn.lanes.lanes.map((lane) => [lane.row, lane.window.marks])).toEqual([
    ["sqlpage", [{ at: at("2026-11-05T12:00:00Z"), id: "ev1", note: "first reply" }]],
    ["mod-host", [{ at: at("2026-11-09T12:00:00Z"), id: "ev2", note: "invited" }]],
    ["libreac", []],
    ["curl", []],
    ["zlib", []],
  ]);
  // unit_in_label: the number alone, with its decimals; a lane with no reading, empty_label.
  expect(points(drawn, 1, "value")?.map((point) => point.slice(0, 2))).toEqual([
    ["sqlpage", "14"],
    ["mod-host", "none"],
    ["libreac", "none"],
    ["curl", "none"],
    ["zlib", "none"],
  ]);
  expect(points(drawn, 2, "note")?.map((point) => point.slice(0, 2))).toEqual([
    ["sqlpage", "lerdorf"],
    ["mod-host", "falkTX"],
    ["libreac", "no data"],
    ["curl", "no data"],
    ["zlib", "no data"],
  ]);
  expect(drawn.lanes.columns).toEqual([
    {
      emptyLabel: "none",
      field: "value",
      label: "First reply, hours",
      series: "first-reply-hours",
    },
    { emptyLabel: "", field: "note", label: "Owner", series: "owner \u00b7 note" },
  ]);
  expect(drawn.lanes.namesTitle).toBe("Project");
  expect(drawn.lanes.endTitle).toBe("Where it stands");
  expect(drawn.lanes.groupTitle).toBeNull();
  expect(drawn.lanes.xTitle).toBeNull();
  expect(drawn.lanes.weekly).toBe(true);
  expect([drawn.lanes.from, drawn.lanes.to]).toEqual([at(WINDOW.from), at(WINDOW.to)]);
  expect(drawn.lanes.rules).toEqual([
    { at: at("2026-11-06T00:00:00Z"), detail: "first wave", label: "Launch" },
  ]);

  // With no x label the time axis is titled as an xy's; a row column with its unit prints it.
  const plain = model({
    kind: "lanes",
    id: "plain",
    row_columns: [{ label: "First reply", series: "first-reply-hours" }],
    title: "Plain",
    value_states: VALUE_STATES,
    y: { label: "Project", series: "enrolment" },
  });
  expect(plain.lanes.xTitle).toBe("Measured time (UTC)");
  expect(plain.lanes.weekly).toBe(false);
  expect(plain.lanes.namesTitle).toBe("Project");
  expect(plain.series[1]?.points[0]?.values.value?.label).toBe("13.7 h");
});

test("the compare range's readings shift onto the window's axis, each lane's compare value and change", () => {
  const drawn = model(ENROLMENTS);
  // Fourteen days on: the compare range starts where the window does.
  expect(spans(drawn, "compare")).toEqual([
    [
      "sqlpage",
      [
        [
          "attention",
          "Waits on us",
          "2026-11-03T09:00:00.000Z",
          "2026-11-08T09:00:00.000Z",
          false,
          false,
        ],
        [
          "context",
          "Waits on its maintainer",
          "2026-11-08T09:00:00.000Z",
          "2026-11-08T09:00:00.000Z",
          true,
          true,
        ],
      ],
    ],
    [
      "mod-host",
      [
        [
          "attention",
          "Waits on us",
          "2026-11-05T09:00:00.000Z",
          "2026-11-05T09:00:00.000Z",
          true,
          true,
        ],
      ],
    ],
    ["libreac", []],
    ["curl", []],
    [
      "zlib",
      [["proved", "Proved", "2026-11-04T09:00:00.000Z", "2026-11-04T09:00:00.000Z", true, true]],
    ],
  ]);
  expect(drawn.lanes.lanes[0]?.compare.marks).toEqual([
    { at: at("2026-11-07T12:00:00Z"), id: "cev1", note: "old reply" },
  ]);
  expect(points(drawn, 0, "value")).toEqual([
    ["sqlpage", "3 (Proved)", "1 (Waits on its maintainer)", "+2"],
    ["mod-host", "1 (Waits on its maintainer)", "0 (Waits on us)", "+1"],
    ["libreac", "2 (Merged, no proof)", "no data", "no data"],
    ["curl", "0 (Waits on us)", "no data", "no data"],
    ["zlib", "no data", "3 (Proved)", "no data"],
  ]);
  // A compare value's note names when its state began, in the compare range.
  expect(drawn.series[0]?.points[0]?.compare?.value?.note).toBe("since 2026-10-25 09:00 UTC");
  expect(points(drawn, 1, "value")?.[0]).toEqual(["sqlpage", "14", "20", "\u22126.5"]);
  expect(points(drawn, 2, "note")?.[0]).toEqual(["sqlpage", "lerdorf", "no data", "no data"]);

  const alone = model(ENROLMENTS, input(null));
  expect(alone.compareRange).toBeNull();
  expect(alone.lanes.lanes.map((lane) => [lane.row, lane.compare])).toEqual([
    ["sqlpage", { marks: [], spans: [] }],
    ["mod-host", { marks: [], spans: [] }],
    ["libreac", { marks: [], spans: [] }],
    ["curl", { marks: [], spans: [] }],
  ]);
  for (const series of alone.series) {
    expect(series.points.every((point) => point.compare === null && point.change === null)).toBe(
      true
    );
  }
});

test("order draws its lanes first; groups draw only the lanes they list, each group's label at its first", () => {
  expect(
    model({ ...ENROLMENTS, order: ["libreac", "nobody"] }).lanes.lanes.map((lane) => lane.row)
  ).toEqual(["libreac", "sqlpage", "mod-host", "curl", "zlib"]);

  const grouped = model({
    ...ENROLMENTS,
    group_label: "Owner",
    groups: [
      { label: "Invited", rows: ["mod-host", "sqlpage"] },
      { label: "Listed", rows: ["libreac", "nobody"] },
    ],
  });
  expect(grouped.lanes.lanes.map((lane) => [lane.row, lane.group])).toEqual([
    ["mod-host", "Invited"],
    ["sqlpage", null],
    ["libreac", "Listed"],
  ]);
  expect(grouped.lanes.grouped).toBe(true);
  expect(grouped.lanes.groupTitle).toBe("Owner");
  expect(grouped.lanes.notes).toEqual(["Not drawn, as no group lists them: curl, zlib."]);
  expect(grouped.series[0]?.points.map((point) => point.x)).toEqual([
    "mod-host",
    "sqlpage",
    "libreac",
  ]);

  const none = model({ ...ENROLMENTS, groups: [{ label: "Nobody", rows: ["nobody"] }] });
  expect(none.lanes.drawn).toBe(0);
  expect(none.lanes.notes).toEqual([
    "No lane drawn: the groups list no row that has readings.",
    "Not drawn, as no group lists them: sqlpage, mod-host, libreac, curl, zlib.",
  ]);
});

test("a lane chart draws at most 35 lanes, and its table holds the rest", () => {
  const many = Array.from({ length: 40 }, (_, index) => ({
    ...fixture.items[0],
    cols: { ...fixture.items[0]?.cols, id: `many-${index}` },
    row_key: `project-${String(index).padStart(2, "0")}`,
  }));
  const drawn = model(
    { ...ENROLMENTS, row_columns: undefined },
    input(COMPARE, many as typeof fixture.items)
  );
  expect(drawn.lanes.lanes.length).toBe(40);
  expect(drawn.lanes.drawn).toBe(35);
  expect(drawn.series[0]?.points.length).toBe(40);
  expect(drawn.lanes.notes).toEqual([
    "5 more lanes not drawn: a lane chart draws at most 35; the table holds them.",
  ]);
});

test("a mock-up draws its points as readings where the y series holds no reading, each point's series its lane", () => {
  const drawn = model({
    id: "sketch",
    kind: "lanes",
    mockup: {
      points: [
        { series: "a", x: "2026-11-01", y: 0 },
        { series: "a", x: "2026-11-03", y: 3 },
        { series: "b", x: "2026-11-02T12:00:00Z", y: 1 },
      ],
      submitted_at: "2026-11-11T09:00:00Z",
      submitted_by: "alice",
    },
    title: "A sketch",
    value_states: VALUE_STATES,
    y: { series: "nothing-yet" },
  });
  expect(drawn.lanes.mockup).toBe("Mock-up: nothing is measured yet; alice drew it on 2026-11-11.");
  expect(spans(drawn, "window")).toEqual([
    [
      "a",
      [
        [
          "attention",
          "Waits on us",
          "2026-11-01T00:00:00.000Z",
          "2026-11-03T00:00:00.000Z",
          false,
          false,
        ],
        ["proved", "Proved", "2026-11-03T00:00:00.000Z", "2026-11-03T00:00:00.000Z", true, true],
      ],
    ],
    [
      "b",
      [
        [
          "context",
          "Waits on its maintainer",
          "2026-11-02T12:00:00.000Z",
          "2026-11-02T12:00:00.000Z",
          true,
          true,
        ],
      ],
    ],
  ]);
  expect(drawn.lanes.lanes.map((lane) => lane.end?.text)).toEqual([
    "Proved",
    "Waits on its maintainer",
  ]);
  expect(drawn.series[0]?.points[0]?.values.value?.note).toBe(
    "since 2026-11-03 00:00 UTC; mock-up"
  );
  expect([drawn.lanes.from, drawn.lanes.to]).toEqual([at("2026-11-01"), at("2026-11-03")]);
});

/** Each value drawn under `root`, by what it names: series, statistic, place, range and value. */
function drawnValues(root: Element): string[] {
  return [...root.querySelectorAll("[data-value]")].map((element) =>
    ["data-series", "data-statistic", "data-x", "data-range", "data-value"]
      .map((name) => element.getAttribute(name))
      .join(" ")
  );
}

test("the table holds every number the chart draws", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(ENROLMENTS), renderer: lanesKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Each enrolment" });
  const graph = drawnValues(figure);
  expect(graph).toContain("enrolment value sqlpage window 3");
  expect(graph).toContain("enrolment value zlib compare 3");
  expect(graph).toContain("first-reply-hours value sqlpage window 13.7");
  expect(graph).toContain("first-reply-hours value sqlpage compare 20.2");
  expect(graph.filter((value) => value.includes(" window ")).length).toBeGreaterThanOrEqual(5);
  expect(graph.filter((value) => value.includes(" compare ")).length).toBeGreaterThanOrEqual(3);

  await user.click(within(figure).getByRole("button", { name: "Table" }));
  const tabled = drawnValues(within(figure).getByRole("table"));
  expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
});

test("the chart draws each lane's states, marks, end and cells, the dated rules, and the compare range muted under the lanes", async () => {
  const user = userEvent.setup();
  render(createElement(EvalChart, { model: model(ENROLMENTS), renderer: lanesKind.renderer }));
  const figure = screen.getByRole("figure", { name: "Each enrolment" });
  const lane = (row: string) => figure.querySelector(`svg[data-lane="${row}"]`) as SVGElement;
  const shapes = (row: string, range: string) =>
    [...lane(row).querySelectorAll(`[data-lane-range="${range}"] [data-lane-span]`)].map((span) => [
      span.tagName.toLowerCase(),
      span.getAttribute("data-lane-span"),
    ]);
  expect(shapes("sqlpage", "window")).toEqual([
    ["line", "context"],
    ["line", "partial"],
    ["circle", "proved"],
  ]);
  expect(
    lane("sqlpage").querySelector('[data-lane-span="partial"]')?.getAttribute("stroke-dasharray")
  ).toBe("4 3");
  expect(shapes("mod-host", "window")).toEqual([
    ["line", "attention"],
    ["rect", "context"],
  ]);
  expect(lane("mod-host").querySelector("rect[data-lane-span]")?.getAttribute("class")).toContain(
    "fill-slate-400"
  );
  expect(lane("sqlpage").querySelector('[data-lane-mark="ev1"]')?.textContent).toContain(
    "first reply"
  );
  expect(shapes("sqlpage", "compare")).toEqual([
    ["line", "attention"],
    ["circle", "context"],
  ]);
  expect(
    lane("sqlpage").querySelector('[data-lane-range="compare"] circle')?.getAttribute("class")
  ).toContain("fill-slate-300");
  expect([...figure.querySelectorAll("[data-lane-end]")].map((end) => end.textContent)).toEqual([
    "proved in CI",
    "Waits on its maintainer",
    "Merged, no proof",
    "Waits on us",
    "",
  ]);
  // Each end text is written in its state's colour, with no swatch beside it (ledger 38).
  const ends = [...figure.querySelectorAll("[data-lane-end][data-state]")];
  expect(ends.length).toBe(4);
  for (const end of ends) {
    const state = end.getAttribute("data-state") as MarkState;
    expect(end.getAttribute("class")).toContain(STATE_TEXT[state]);
    expect(end.querySelector("svg")).toBeNull();
  }
  expect(figure.querySelector("[data-lane-rules]")?.textContent).toBe("Launchfirst wave");
  expect(within(figure).getByText("Project")).toBeTruthy();
  expect(within(figure).getByText("Where it stands")).toBeTruthy();
  expect(within(figure).getByText("1 reading with no value not drawn.")).toBeTruthy();
  const sqlpage = figure.querySelector('[data-lane-row="sqlpage"]') as HTMLElement;
  expect(sqlpage.textContent).toContain("14was 20");
  expect(sqlpage.textContent).toContain("lerdorf");
  expect(figure.querySelector("[data-lane-axis]")?.textContent).toBe("2 Nov9 Nov");

  await user.click(within(figure).getByRole("checkbox", { name: "Compare" }));
  expect(figure.querySelector('[data-lane-range="compare"]')).toBeNull();
  expect(figure.querySelector("[data-ghost]")).toBeNull();
});
