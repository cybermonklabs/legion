import { afterEach, test as bunTest, expect } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import {
  type ChartModel,
  type ChartRenderer,
  chartSeries,
  DAY_AXIS,
  type EvalRange,
  type EvalSeriesValues,
  type SeriesSpec,
} from "./chart-model";
import { PerDayBars, PerDayLine, PerDaySpread, Timing } from "./chart-renderers";
import { EvalChart } from "./EvalChart";

// EvalChart's rows (dispatch://INFRA-2918/artifact/plan-md, T7): a section's chart, drawn from a
// series of an eval read as the evals view answers it.

const WINDOW: EvalRange = { days: 3, from: "2026-11-01T00:00:00Z", to: "2026-11-04T00:00:00Z" };
const COMPARE: EvalRange = { days: 3, from: "2026-10-29T00:00:00Z", to: "2026-11-01T00:00:00Z" };
/** After the window, so no day of it is partial. */
const NOW = Date.parse("2026-11-10T12:00:00Z");

/** A series as the list read answers it: no row at all unless the row says otherwise. */
function values(overrides: Partial<EvalSeriesValues>): EvalSeriesValues {
  return {
    agg: "quantile",
    change: null,
    compare: null,
    compare_days: null,
    created_at: "2026-10-01T00:00:00Z",
    data_since: "2026-10-01",
    days: [],
    eval_id: "ACME/merge-to-production",
    is_primary: true,
    links: [],
    listed: true,
    min_n: null,
    narrowed: null,
    role: "headline",
    series: "minutes",
    unit: "minutes",
    version: 1,
    via_repository: 0,
    window: null,
    ...overrides,
  };
}

/** A one-series chart of `series` over `window`, compared with `compare`. */
function chart(
  kind: string,
  series: EvalSeriesValues,
  spec: SeriesSpec,
  window: EvalRange = WINDOW,
  compare: EvalRange | null = COMPARE
): ChartModel {
  return {
    axis: DAY_AXIS,
    comparable: true,
    compareRange: compare,
    kind,
    name: "merge-to-production",
    series: [chartSeries(series, spec, { compare, nowMs: NOW, target: null, window })],
    title: "Merge to production",
  };
}

/** Each day of the window and of the compare range with every statistic of `stats`, each day's
 *  own values: `base` plus the day's place, the compare range's 10 more. */
function everyDay(stats: readonly string[], base: number): Partial<EvalSeriesValues> {
  const day = (date: string, offset: number) => ({
    day: date,
    statistics: Object.fromEntries(stats.map((name, index) => [name, base + offset + index])),
  });
  return {
    compare: Object.fromEntries(stats.map((name) => [name, base + 10])),
    compare_days: [day("2026-10-29", 10), day("2026-10-30", 11), day("2026-10-31", 12)],
    change: Object.fromEntries(stats.map((name) => [name, -10])),
    days: [day("2026-11-01", 0), day("2026-11-02", 1), day("2026-11-03", 2)],
    window: Object.fromEntries(stats.map((name) => [name, base])),
  };
}

/** Each built-in renderer, with a series holding every statistic it draws and its table holds. */
const RENDERERS: [string, ChartRenderer, ChartModel][] = [
  [
    "per-day bars",
    PerDayBars,
    chart(
      "bars",
      values({ series: "deploys", unit: "deploys", ...everyDay(["per_day", "deploys"], 2) }),
      {
        headline: ["per_day"],
        label: "Deploys",
        statistics: [
          { id: "per_day", label: "Deploys a day" },
          { id: "deploys", label: "Deploys" },
        ],
      }
    ),
  ],
  [
    "per-day spread",
    PerDaySpread,
    chart("spread", values(everyDay(["median", "p90", "n"], 30)), {
      headline: ["median", "p90"],
      label: "Minutes",
      noun: "PRs",
      statistics: [
        { id: "median", label: "Median" },
        { id: "p90", label: "p90" },
        { id: "n", label: "PRs" },
      ],
    }),
  ],
  [
    "per-day line",
    PerDayLine,
    chart(
      "line",
      values({ agg: "ratio", series: "prs", unit: "ratio", ...everyDay(["rate"], 0.25) }),
      { headline: ["rate"], label: "Rework", statistics: [{ id: "rate", label: "Rework share" }] }
    ),
  ],
  [
    "timing",
    Timing,
    chart(
      "timing",
      values(
        everyDay(["n", "median", "p90", "success", "failure", "cancelled", "success_rate"], 1)
      ),
      {
        headline: ["median", "p90", "n", "success", "failure", "cancelled"],
        label: "Runs",
        noun: "runs",
        statistics: [
          { id: "median", label: "Median" },
          { id: "p90", label: "p90" },
          { id: "n", label: "Runs" },
          { id: "success", label: "Success" },
          { id: "failure", label: "Failure" },
          { id: "cancelled", label: "Cancelled" },
          { id: "success_rate", label: "Success rate" },
        ],
      }
    ),
  ],
];

const MINUTES_SPEC: SeriesSpec = {
  headline: ["median"],
  label: "Minutes",
  noun: "PRs",
  statistics: [
    { id: "median", label: "Median" },
    { id: "p90", label: "p90" },
    { id: "n", label: "PRs" },
  ],
};

afterEach(() => {
  localStorage.clear();
});

/** A row of this file, its chart unmounted inside its body. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

function figure(): HTMLElement {
  return screen.getByRole("figure", { name: "Merge to production" });
}

/** Each value drawn under `root`, by what it names: series, statistic, place, range and value. */
function drawn(root: Element): string[] {
  return [...root.querySelectorAll("[data-value]")].map((element) =>
    ["data-series", "data-statistic", "data-x", "data-range", "data-value"]
      .map((name) => element.getAttribute(name))
      .join(" ")
  );
}

bunTest.each(RENDERERS)(
  "the table holds every number the graph draws: %s",
  async (_name, renderer, model) => {
    try {
      const user = userEvent.setup();
      render(<EvalChart model={model} renderer={renderer} />);
      const graph = drawn(figure());
      // Every day of the window and of the compare range, each drawn.
      expect(graph.filter((value) => value.includes(" window ")).length).toBeGreaterThanOrEqual(3);
      expect(graph.filter((value) => value.includes(" compare ")).length).toBeGreaterThanOrEqual(3);

      await user.click(within(figure()).getByRole("button", { name: "Table" }));
      const table = within(figure()).getByRole("table", { name: "Merge to production per day" });
      const tabled = drawn(table);
      expect(graph.filter((value) => !tabled.includes(value))).toEqual([]);
    } finally {
      cleanup();
    }
  }
);

test('a window before the series\' first input day reads "no data before <day>"', async () => {
  render(
    <EvalChart
      model={chart(
        "spread",
        values({
          data_since: "2026-11-02",
          days: [{ day: "2026-11-02", statistics: { median: 40, n: 2, p90: 50 } }],
          window: { median: 40, n: 2, p90: 50 },
        }),
        MINUTES_SPEC
      )}
      renderer={PerDaySpread}
    />
  );
  expect(within(figure()).getByText("no data before 2026-11-02")).toBeInTheDocument();

  cleanup();
  // A window that ends before the series' first day reads no value at all, and names the day.
  render(
    <EvalChart
      model={chart("spread", values({ data_since: "2026-11-20" }), MINUTES_SPEC)}
      renderer={PerDaySpread}
    />
  );
  const figures = figure().querySelector('[data-window-series="minutes"]');
  expect(figures?.querySelector('[data-window-statistic="median"]')?.textContent).toBe("no data");
  expect(within(figure()).getByText("no data before 2026-11-20")).toBeInTheDocument();
});

test("a series whose input has not begun says so", async () => {
  render(
    <EvalChart
      model={chart("spread", values({ data_since: null }), MINUTES_SPEC)}
      renderer={PerDaySpread}
    />
  );
  const figures = figure().querySelector('[data-window-series="minutes"]');
  expect(figures?.querySelector('[data-window-statistic="median"]')?.textContent).toBe("no data");
  expect(within(figure()).getByText("its input has not begun")).toBeInTheDocument();
  expect(within(figure()).queryByText(/no data before/)).toBeNull();
});

test("a series that counts 2 rows by default repository shows the note with 2 beside its value", async () => {
  render(
    <EvalChart
      model={chart(
        "spread",
        values({ via_repository: 2, window: { median: 40, n: 5, p90: 50 } }),
        MINUTES_SPEC
      )}
      renderer={PerDaySpread}
    />
  );
  const figures = figure().querySelector('[data-window-series="minutes"]');
  expect(figures?.querySelector('[data-window-statistic="median"]')?.textContent).toBe("40m");
  // Beside the value: in the series' own figures, after it.
  expect(figures?.querySelector('[data-note="repository"]')?.textContent).toBe(
    "2 by default repository"
  );
});

test("a series with a count of 0 shows no note", async () => {
  render(
    <EvalChart
      model={chart(
        "spread",
        values({ via_repository: 0, window: { median: 40, n: 5, p90: 50 } }),
        MINUTES_SPEC
      )}
      renderer={PerDaySpread}
    />
  );
  expect(figure().querySelector('[data-note="repository"]')).toBeNull();
  expect(within(figure()).queryByText(/by default repository/)).toBeNull();
});

test("a chart with one point shows a table with one row", async () => {
  const user = userEvent.setup();
  const day: EvalRange = { days: 1, from: "2026-11-01T00:00:00Z", to: "2026-11-02T00:00:00Z" };
  render(
    <EvalChart
      model={chart(
        "bars",
        values({
          days: [{ day: "2026-11-01", statistics: { deploys: 3, per_day: 3 } }],
          series: "deploys",
          unit: "deploys",
          window: { deploys: 3, per_day: 3 },
        }),
        {
          headline: ["per_day"],
          label: "Deploys",
          statistics: [{ id: "per_day", label: "Deploys a day" }],
        },
        day,
        null
      )}
      renderer={PerDayBars}
    />
  );
  await user.click(within(figure()).getByRole("button", { name: "Table" }));
  const table = within(figure()).getByRole("table", { name: "Merge to production per day" });
  const rows = table.querySelectorAll("[data-row]");
  expect(rows).toHaveLength(1);
  expect(rows[0]?.textContent).toBe("2026-11-013.0/day");
});

test("a compare range with no data shows an empty second series", async () => {
  render(
    <EvalChart
      model={chart(
        "bars",
        values({
          compare_days: [],
          days: [{ day: "2026-11-02", statistics: { deploys: 3, per_day: 3 } }],
          series: "deploys",
          unit: "deploys",
          window: { deploys: 3, per_day: 1 },
        }),
        {
          headline: ["per_day"],
          label: "Deploys",
          statistics: [{ id: "per_day", label: "Deploys a day" }],
        }
      )}
      renderer={PerDayBars}
    />
  );
  const second = figure().querySelector('[data-series-range="compare"]');
  expect(second).not.toBeNull();
  expect(second?.querySelectorAll("[data-value]")).toHaveLength(0);
  expect(figure().querySelector("[data-compare-legend]")?.textContent).toBe(
    "compare 2026-10-29 to 2026-10-31: no data"
  );
  // The window's own series is drawn beside it.
  expect(drawn(figure())).toEqual(["deploys per_day 2026-11-02 window 3"]);
});

test("the toggle keeps the compare", async () => {
  const user = userEvent.setup();
  const [, renderer, model] = RENDERERS[0] ?? [];
  if (renderer === undefined || model === undefined) throw new Error("no bars renderer");
  render(<EvalChart model={model} renderer={renderer} />);
  const compare = () => within(figure()).getByRole("checkbox", { name: "Compare" });
  const windowChange = () => figure().querySelector('[data-window-change="per_day"]')?.textContent;
  expect(compare()).toBeChecked();
  // The window's figure says its change from the compare range's.
  expect(windowChange()).toBe("\u221210.0/day vs compare");

  // Shown in the graph, then in the table: each day's compare value and change beside its value.
  await user.click(within(figure()).getByRole("button", { name: "Table" }));
  expect(compare()).toBeChecked();
  const table = () => within(figure()).getByRole("table", { name: "Merge to production per day" });
  expect(
    within(table()).getByRole("columnheader", { name: /Deploys a day compare/ })
  ).toBeInTheDocument();
  expect(
    within(table()).getByRole("columnheader", { name: /Deploys a day change/ })
  ).toBeInTheDocument();

  // Turned off in the table, it stays off in the graph, and back in the table.
  await user.click(compare());
  expect(within(table()).queryByRole("columnheader", { name: /compare/ })).toBeNull();
  await user.click(within(figure()).getByRole("button", { name: "Graph" }));
  expect(compare()).not.toBeChecked();
  expect(figure().querySelector('[data-series-range="compare"]')).toBeNull();
  expect(windowChange()).toBeUndefined();
  await user.click(within(figure()).getByRole("button", { name: "Table" }));
  expect(compare()).not.toBeChecked();
  expect(table().querySelector('[data-range="compare"]')).toBeNull();
});
