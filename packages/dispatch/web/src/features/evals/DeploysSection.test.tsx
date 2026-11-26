import { afterEach, test as bunTest, expect, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { evalItem, evalSeries, evalsView, WINDOW } from "../../__tests__/evals-fixture";
import { api } from "../../api/client";
import type { EvalListItem } from "../../api/types";
import { DEPLOY_CHARTS, DeploysSection } from "./DeploysSection";
import { NARROWED_EVALS } from "./narrowing";
import type { EvalsViewState } from "./useEvalsViews";

// The Deploys and lead time section's rows (dispatch://INFRA-2918/artifact/plan-md, T12), over a
// delivery view as the evals view answers it, and the method each eval's detail read answers.

/** Each chart's eval, its listed series holding `first` on the window's first day and `second`
 *  on its second, of the chart's first statistic, and nothing on the third. */
function charted(
  slug: string,
  first: number,
  second: number,
  overrides: Parameters<typeof evalSeries>[2] = {}
): EvalListItem {
  const chart = DEPLOY_CHARTS.find((candidate) => candidate.slug === slug);
  if (chart === undefined) throw new Error(`no chart of ${slug}`);
  const statistic = chart.spec.statistics[0]?.id ?? "";
  return evalItem(slug, "delivery", [
    evalSeries(chart.series, slug === "rework-share" ? "ratio" : "minutes", {
      days: [
        { day: "2026-11-01", statistics: { [statistic]: first } },
        { day: "2026-11-02", statistics: { [statistic]: second } },
      ],
      is_primary: true,
      window: { [statistic]: first + second },
      ...overrides,
    }),
  ]);
}

/** Every chart's eval, the view holding them out of the section's order. */
const EVERY = [...DEPLOY_CHARTS]
  .reverse()
  .map((chart, index) => charted(chart.slug, index + 1, index + 2));

const spies: { mockRestore: () => void }[] = [];
afterEach(() => {
  for (const spy of spies.splice(0)) spy.mockRestore();
  localStorage.clear();
});

/** A row of this file, its section unmounted inside its body. */
const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

function renderSection(items: EvalListItem[]): void {
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <DeploysSection project="ACME" views={[evalsView("delivery", items)]} />
    </QueryClientProvider>
  );
}

function chart(slug: string): HTMLElement {
  const found = document.querySelector<HTMLElement>(`[data-eval="${slug}"]`);
  if (found === null) throw new Error(`no ${slug} chart`);
  return found;
}

test("the ten charts are drawn in order", async () => {
  renderSection(EVERY);
  expect(
    [...document.querySelectorAll("[data-eval]")].map((drawn) => drawn.getAttribute("data-eval"))
  ).toEqual([
    "deploy-frequency",
    "deploy-run-success",
    "merge-to-production",
    "first-commit-to-production",
    "open-to-production",
    "pr-open-to-merge",
    "rework-share",
    "change-fail-rate",
    "failed-deploy-recovery",
    "deployment-rework-rate",
  ]);
  for (const drawn of document.querySelectorAll("[data-eval]")) {
    expect(drawn.querySelector("figure")?.getAttribute("data-chart")).toBe(
      drawn.getAttribute("data-eval")
    );
  }
});

test("each table's rows equal the per-day values", async () => {
  const user = userEvent.setup();
  renderSection(EVERY);
  for (const spec of DEPLOY_CHARTS) {
    const item = EVERY.find((candidate) => candidate.definition.slug === spec.slug);
    const statistic = spec.spec.statistics[0]?.id ?? "";
    await user.click(within(chart(spec.slug)).getByRole("button", { name: "Table" }));
    const table = within(chart(spec.slug)).getByRole("table");
    const rows = [...table.querySelectorAll("[data-row]")].map((row) => [
      row.getAttribute("data-row"),
      row
        .querySelector(`[data-range="window"][data-statistic="${statistic}"]`)
        ?.getAttribute("data-value") ?? null,
    ]);
    const days = item?.series?.[0]?.days ?? [];
    expect(rows).toEqual([
      ["2026-11-01", String(days[0]?.statistics?.[statistic])],
      ["2026-11-02", String(days[1]?.statistics?.[statistic])],
      // A day with no row of the series holds no value.
      ["2026-11-03", null],
    ]);
  }
});

test("the method text is the detail's doc_text, read when the method is opened", async () => {
  const user = userEvent.setup();
  const method =
    "Each merged pull request's minutes from its merge to the deploy that shipped it.\n\nThe median of the window's.";
  const getEval = spyOn(api, "getEval").mockResolvedValue({
    compare: null,
    doc_text: method,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items: [],
    limit: 1,
    offset: 0,
    total: 1,
    window: WINDOW,
  });
  spies.push(getEval);
  renderSection([charted("merge-to-production", 30, 40)]);
  // The first paint reads no detail.
  expect(getEval).not.toHaveBeenCalled();

  await user.click(within(chart("merge-to-production")).getByRole("button", { name: "Method" }));
  await waitFor(() =>
    expect(chart("merge-to-production").querySelector("[data-method-text]")?.textContent).toBe(
      method
    )
  );
  expect(getEval.mock.calls).toEqual([["ACME", "merge-to-production", { limit: 1 }]]);
});

test('the failure measures whose builders have not landed read "no data"', async () => {
  const none = { median: null, not_restored: null, rate: null, restoring: null, total: null };
  renderSection([
    evalItem("change-fail-rate", "delivery", [
      evalSeries("per_deploy", "ratio", { is_primary: true, window: none }),
    ]),
    evalItem("failed-deploy-recovery", "delivery", [
      evalSeries("minutes", "minutes", { is_primary: true, window: none }),
    ]),
  ]);
  expect(
    chart("change-fail-rate").querySelector('[data-window-statistic="rate"]')?.textContent
  ).toBe("no data");
  expect(
    chart("failed-deploy-recovery").querySelector('[data-window-statistic="not_restored"]')
      ?.textContent
  ).toBe("no data");
  expect(
    within(screen.getByRole("article", { name: "Change failure rate" })).queryByText(/^0/)
  ).toBeNull();
});

test("a series with via_repository 2 shows the note with 2 beside its value, and one with 0 none (R15)", async () => {
  renderSection([
    charted("merge-to-production", 30, 40, { via_repository: 2 }),
    charted("pr-open-to-merge", 30, 40, { via_repository: 0 }),
  ]);
  const figures = chart("merge-to-production").querySelector('[data-window-series="minutes"]');
  expect(figures?.querySelector('[data-window-statistic="median"]')?.textContent).toBe("1.2h");
  expect(figures?.querySelector('[data-note="repository"]')?.textContent).toBe(
    "2 by default repository"
  );
  expect(chart("pr-open-to-merge").querySelector('[data-note="repository"]')).toBeNull();
  expect(within(chart("pr-open-to-merge")).queryByText(/by default repository/)).toBeNull();
});

/** The chart of `slug`'s values of its first statistic on each day of the window, as its table
 *  holds them. */
function dayValues(slug: string): (string | null)[] {
  const statistic =
    DEPLOY_CHARTS.find((candidate) => candidate.slug === slug)?.spec.statistics[0]?.id ?? "";
  return [...within(chart(slug)).getByRole("table").querySelectorAll("[data-row]")].map(
    (row) =>
      row
        .querySelector(`[data-range="window"][data-statistic="${statistic}"]`)
        ?.getAttribute("data-value") ?? null
  );
}

/** What the chart of `slug` says of the facets: its mark's kind and words; null for no mark. */
function markOf(slug: string): [string | null, string | null] | null {
  const mark = chart(slug).querySelector("[data-narrowing]");
  return mark === null ? null : [mark.getAttribute("data-narrowing"), mark.textContent];
}

test('a facet set moves the five pull-request charts to the narrowed view\'s values, each naming the facets applied, and leaves the deploy and run charts on their stored values with the "window and repository only" mark', async () => {
  const user = userEvent.setup();
  // Each chart's eval as the stored day rows answer it, and as the view narrowed by the author
  // facet answers it: the pull-request evals ten times over, every other eval as stored.
  const stored = DEPLOY_CHARTS.map((spec, index) => charted(spec.slug, index + 1, index + 2));
  const narrowed = DEPLOY_CHARTS.map((spec, index) =>
    NARROWED_EVALS[spec.slug] === true
      ? charted(spec.slug, (index + 1) * 10, (index + 2) * 10, { narrowed: ["author"] })
      : stored[index]
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = (view: EvalsViewState) => (
    <QueryClientProvider client={client}>
      <DeploysSection project="ACME" views={[view]} />
    </QueryClientProvider>
  );
  const { rerender } = render(tree(evalsView("delivery", stored)));
  for (const spec of DEPLOY_CHARTS) {
    await user.click(within(chart(spec.slug)).getByRole("button", { name: "Table" }));
  }
  expect(document.querySelector("[data-narrowing]")).toBeNull();
  const pullRequests = DEPLOY_CHARTS.filter((spec) => NARROWED_EVALS[spec.slug] === true);
  expect(pullRequests.map((spec) => spec.slug)).toEqual([
    "merge-to-production",
    "first-commit-to-production",
    "open-to-production",
    "pr-open-to-merge",
    "rework-share",
  ]);
  const storedValues = (index: number) => [String(index + 1), String(index + 2), null];

  // The facet set, its narrowed view not yet answered: the stored values, the five under the
  // loading mark.
  rerender(tree({ ...evalsView("delivery", stored), facets: { loading: true, set: true } }));
  DEPLOY_CHARTS.forEach((spec, index) => {
    expect(dayValues(spec.slug)).toEqual(storedValues(index));
    expect(markOf(spec.slug)).toEqual(
      NARROWED_EVALS[spec.slug] === true
        ? ["loading", "narrowing to the facets set\u2026"]
        : ["window-only", "window and repository only"]
    );
  });

  // The narrowed view answered: the five draw its values and name the facet; the others keep
  // theirs. Each chart keeps the table the reader chose.
  rerender(tree({ ...evalsView("delivery", narrowed), facets: { loading: false, set: true } }));
  DEPLOY_CHARTS.forEach((spec, index) => {
    if (NARROWED_EVALS[spec.slug] === true) {
      expect(dayValues(spec.slug)).toEqual([
        String((index + 1) * 10),
        String((index + 2) * 10),
        null,
      ]);
      expect(markOf(spec.slug)).toEqual(["narrowed", "narrowed by Author"]);
      expect(chart(spec.slug).querySelector('[aria-busy="true"]')).toBeNull();
    } else {
      expect(dayValues(spec.slug)).toEqual(storedValues(index));
      expect(markOf(spec.slug)).toEqual(["window-only", "window and repository only"]);
    }
  });
});
