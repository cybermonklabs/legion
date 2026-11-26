import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { wholeText } from "../../__tests__/whole-text";
import type { DeliveryDailyPoint, DeliveryMeasuresResponse } from "../../api/types";
import {
  borderDefault,
  chartBar,
  dangerText,
  successText,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { MeasuresPanel } from "./MeasuresPanel";

afterEach(cleanup);

/** Every UTC day of the fixture window, 2026-08-30 to 2026-09-27, with the reference request's
 *  per-day counts; the last day is partial (the window ends 20:00). */
function fixtureDaily(): DeliveryDailyPoint[] {
  const counts: Record<string, Partial<DeliveryDailyPoint>> = {
    "2026-09-05": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-06": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-09": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-11": { concluded: 1, run_success_rate: 0 },
    "2026-09-13": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-14": { concluded: 1, run_success_rate: 0 },
    "2026-09-17": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-19": { cancelled: 1 },
  };
  const days: DeliveryDailyPoint[] = [];
  for (let ms = Date.UTC(2026, 7, 30); ms <= Date.UTC(2026, 8, 27); ms += 86_400_000) {
    const day = new Date(ms).toISOString().slice(0, 10);
    days.push({
      day,
      partial: day === "2026-09-27",
      deploys: 0,
      concluded: 0,
      reached_production: 0,
      cancelled: 0,
      run_success_rate: null,
      ...counts[day],
    });
  }
  return days;
}

type Targets = DeliveryMeasuresResponse["targets"];

/** The six targets as the catalogue's evals hold them, which the answer states until a person
 *  changes one: each with its eval's direction and the statistics its verdict judges. */
const catalogueTargets: { [K in keyof Targets]: NonNullable<Targets[K]> } = {
  deploys_per_day: {
    value: 20,
    inclusive: true,
    direction: "higher_better",
    judges: ["deploys.per_day"],
  },
  change_failure_rate: {
    value: 0.05,
    inclusive: false,
    direction: "lower_better",
    judges: ["per_deploy.rate"],
  },
  merge_to_production_minutes: {
    value: 45,
    inclusive: false,
    direction: "lower_better",
    judges: ["minutes.max", "minutes.median"],
  },
  opened_to_merge_median_minutes: {
    value: 60,
    inclusive: false,
    direction: "lower_better",
    judges: ["minutes.median"],
  },
  deploy_run_success_rate: {
    value: 0.9,
    inclusive: true,
    direction: "higher_better",
    judges: ["runs.rate"],
  },
  unowned_p0: { value: 0, inclusive: true, direction: "lower_better", judges: ["issues.value"] },
};

/** GET /api/v1/delivery/measures for the fixture window, as the server answers it with no flags
 *  stored (the reference request recorded on the slice 2 pull request): no failure source exists,
 *  so the change failure rate's counts are 0 and its rates null, and neither of its verdicts is
 *  reached. */
const fixtureMeasures: DeliveryMeasuresResponse = {
  window: { from: "2026-08-30T00:00:00Z", to: "2026-09-27T20:00:00Z" },
  computed_at: "2026-10-09T04:33:22Z",
  measures: {
    deploy_frequency: {
      successful_deploys: 5,
      deploys_with_prs: 5,
      per_day: 0.17341040462427745,
      with_prs_per_day: 0.17341040462427745,
    },
    lead_time: {
      merge_to_production: { median_minutes: 510, p90_minutes: 2736, max_minutes: 3840 },
      first_commit_to_production: { median_minutes: 1380, p90_minutes: 2988, max_minutes: 4380 },
      opened_to_production: { median_minutes: 1260, p90_minutes: 2874, max_minutes: 4320 },
      opened_to_merge: { median_minutes: 330, p90_minutes: 462.00000000000006, max_minutes: 1080 },
    },
    change_failure_rate: {
      per_pr: { confirmed: 0, pending: 0, rejected: 0, reverts: 0, total: 0, rate: null },
      per_deploy: {
        confirmed: 0,
        pending: 0,
        rejected: 0,
        reverts: 0,
        total: 0,
        rate: null,
        confirmed_or_pending: 0,
        upper_bound_rate: null,
      },
    },
    time_to_restore: { median_minutes: null },
    rework_share: 0.2857142857142857,
    deploy_run_success: {
      concluded: 7,
      reached_production: 5,
      cancelled: 1,
      rate: 0.7142857142857143,
    },
    daily: fixtureDaily(),
  },
  flags_source: "none",
  unowned_p0: [{ key: "ACME-103", title: "Production deploy gate flakes" }],
  targets: catalogueTargets,
  status: {
    deploys_per_day: false,
    change_failure_rate: null,
    change_failure_rate_upper_bound: null,
    merge_to_production_median: false,
    merge_to_production: false,
    opened_to_merge: false,
    deploy_run_success: false,
    unowned_p0: false,
  },
  freshness: {
    last_event_at: null,
    last_reconcile_at: null,
    last_error: null,
    unfetchable_count: 0,
  },
};

function renderPanel(data: DeliveryMeasuresResponse | undefined) {
  render(
    <MemoryRouter>
      <MeasuresPanel data={data} />
    </MemoryRouter>
  );
}

function card(title: string): HTMLElement {
  const found = document.querySelector<HTMLElement>(`[data-kpi="${title}"]`);
  if (found === null) throw new Error(`no [data-kpi="${title}"] card`);
  return found;
}

/** The per-day chart of a card: its dashed target lines, and each day's drawn bar or stub in day
 *  order, the fixture's 29 days from 2026-08-30. */
function chart(title: string): { bars: (string | null)[]; lines: number } {
  const svg = within(card(title)).getByRole("img", { name: "per-day trend" });
  return {
    bars: Array.from(svg.querySelectorAll("rect"))
      .filter((rect) => rect.getAttribute("fill") !== "transparent")
      .map((rect) => rect.getAttribute("class")),
    lines: svg.querySelectorAll("line").length,
  };
}

/** The bars of a chart that take a met or missed tone. */
function toned(bars: readonly (string | null)[]): (string | null)[] {
  const tones: (string | null)[] = [
    chartBar.met,
    chartBar.missed,
    chartBar.metFaint,
    chartBar.missedFaint,
  ];
  return bars.filter((bar) => tones.includes(bar));
}

/** The tone an element's class gives it: met, missed, muted (a pending read's), or plain. */
function toneOf(element: Element): string {
  const className = element.getAttribute("class") ?? "";
  if (className.includes(successText)) return "met";
  if (className.includes(dangerText)) return "missed";
  if (className.includes(textMutedOnSurface)) return "muted";
  return "plain";
}

/** The tone of each element within `scope` that names a statistic (`data-statistic`) of `span`,
 *  by that statistic. */
function tonesIn(scope: Element, span: "day" | "window"): Record<string, string> {
  return Object.fromEntries(
    Array.from(scope.querySelectorAll(`[data-span="${span}"][data-statistic]`), (element) => [
      element.getAttribute("data-statistic"),
      toneOf(element),
    ])
  );
}

/** The tone of each figure of the whole window a card may tone, its headline figures and its
 *  counts, by the statistic it names. */
function tones(title: string): Record<string, string> {
  return tonesIn(card(title), "window");
}

/** What each figure of the whole window within `scope` writes, and its tone, by the statistic it
 *  names: "44.6m met". */
function written(scope: Element): Record<string, string> {
  return Object.fromEntries(
    Array.from(scope.querySelectorAll('[data-span="window"][data-statistic]'), (element) => [
      element.getAttribute("data-statistic"),
      `${element.textContent} ${toneOf(element)}`,
    ])
  );
}

/** The six cards, in the panel's order. */
const TITLES = [
  "Deploys a day",
  "Change failure rate",
  "Merge \u2192 production",
  "PR open \u2192 merge",
  "Deploy runs reaching production",
  "P0 issues with no owner",
] as const;

/** Opens the "Definitions & alternatives" fold, which holds the per-day table. */
async function openFold(): Promise<void> {
  await userEvent.setup().click(screen.getByRole("button", { name: "Definitions & alternatives" }));
}

/** The tone of each cell of one day's row in the fold's per-day table that a target may judge,
 *  its Deploys and its Run success, by the statistic it names. Open the fold first. */
function cellTones(day: string): Record<string, string> {
  const row = within(screen.getByRole("table"))
    .getAllByRole("row")
    .find((candidate) => candidate.querySelector("td")?.textContent === day);
  if (row === undefined) throw new Error(`no row for ${day}`);
  return tonesIn(row, "day");
}

/** The evals the elements within `scope` that name a statistic name (`data-eval`), each once. */
function evalsIn(scope: Element): string[] {
  return [
    ...new Set(
      Array.from(scope.querySelectorAll("[data-statistic]"), (element) =>
        element.getAttribute("data-eval")
      )
    ),
  ].map(String);
}

/** A definition card of the open fold, by its title. */
function definition(title: string): Element {
  const found = screen.getByText(title, { selector: "div.uppercase" }).parentElement;
  if (found === null) throw new Error(`no ${title} definition card`);
  return found;
}

/** A change failure rate a stored failure source measured: 2% of deploys confirmed broken, 4% if
 *  every pending flag is confirmed, and 40% of pull requests. */
const measuredFailureRate: DeliveryMeasuresResponse["measures"]["change_failure_rate"] = {
  per_pr: { confirmed: 2, pending: 0, rejected: 0, reverts: 0, total: 5, rate: 0.4 },
  per_deploy: {
    confirmed: 1,
    pending: 1,
    rejected: 0,
    reverts: 0,
    total: 50,
    rate: 0.02,
    confirmed_or_pending: 2,
    upper_bound_rate: 0.04,
  },
};

test("the panel shows the six KPI cards, each with its badge, number and target", () => {
  renderPanel(fixtureMeasures);

  const grid = screen.getByLabelText("KPI targets");
  expect(grid.querySelectorAll("[data-kpi]")).toHaveLength(6);

  const deploys = within(card("Deploys a day"));
  expect(deploys.getByText("0.2/day")).toBeDefined();
  expect(deploys.getByText(wholeText("5 deploys"))).toBeDefined();
  expect(deploys.getByText("\u2717 missed")).toBeDefined();
  expect(deploys.getByText("target \u2265 20/day")).toBeDefined();
  expect(deploys.getByRole("img", { name: "per-day trend" })).toBeDefined();

  const cfr = within(card("Change failure rate"));
  expect(cfr.getByText("n/a")).toBeDefined();
  expect(cfr.getByText("no data")).toBeDefined();
  expect(cfr.queryByText("\u2713 met")).not.toBeInTheDocument();
  expect(cfr.queryByText("\u2717 missed")).not.toBeInTheDocument();
  expect(cfr.queryByText("confirmed")).not.toBeInTheDocument();
  expect(cfr.getByText("Flags arrive with flag confirmation")).toBeDefined();
  expect(cfr.queryByText(/if every pending flag is confirmed/)).not.toBeInTheDocument();
  expect(cfr.getByText("target < 5% per deploy")).toBeDefined();

  const merge = within(card("Merge \u2192 production"));
  expect(merge.getByText("8.5h")).toBeDefined();
  expect(merge.getByText("2.7d")).toBeDefined();
  // The catalogue judges the longest change and the median, and the longest change under the
  // target puts the median under it: the caption names the longest change alone, as before.
  expect(merge.getByText("target < 45m for every change")).toBeDefined();

  // The median is the judged statistic and is toned by its own value: 5.5h misses under 60m. The
  // p90 reads plain.
  const opened = within(card("PR open \u2192 merge"));
  expect(opened.getByText("5.5h").className).toContain(dangerText);
  expect(opened.getByText("7.7h").className).toContain(textPrimaryOnSurface);
  expect(opened.getByText("target median < 60m")).toBeDefined();

  const runs = within(card("Deploy runs reaching production"));
  expect(runs.getByText("71.4%")).toBeDefined();
  expect(runs.getByText(wholeText("5 of 7 concluded"))).toBeDefined();
  expect(runs.getByText(wholeText("1 cancelled runs not counted"))).toBeDefined();
  expect(runs.getByText("target \u2265 90%")).toBeDefined();

  const p0 = within(card("P0 issues with no owner"));
  expect(p0.getByText("1")).toBeDefined();
  const link = p0.getByRole("link", { name: "ACME-103" });
  expect(link).toHaveAttribute("href", "/issues/ACME-103");
  expect(link).toHaveAttribute("title", "Production deploy gate flakes");
  expect(p0.getByText(/^no claim or route, as of /)).toBeDefined();
  expect(p0.getByText("target 0")).toBeDefined();
});

test("met, missed and no-data badges follow the response's status", () => {
  renderPanel({
    ...fixtureMeasures,
    status: { ...fixtureMeasures.status, deploys_per_day: true, deploy_run_success: null },
  });
  expect(within(card("Deploys a day")).getByText("\u2713 met")).toBeDefined();
  expect(within(card("Deploy runs reaching production")).getByText("no data")).toBeDefined();
  expect(within(card("Change failure rate")).getByText("no data")).toBeDefined();
});

test("each target reads as its eval holds it: its sign follows inclusive, and one turned off reads off", () => {
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      deploys_per_day: { ...catalogueTargets.deploys_per_day, value: 4, inclusive: false },
      merge_to_production_minutes: {
        ...catalogueTargets.merge_to_production_minutes,
        inclusive: true,
      },
      deploy_run_success_rate: null,
    },
    status: { ...fixtureMeasures.status, deploy_run_success: null },
  });
  expect(within(card("Deploys a day")).getByText("target > 4/day")).toBeDefined();
  expect(
    within(card("Merge \u2192 production")).getByText("target \u2264 45m for every change")
  ).toBeDefined();
  // At most 0 unowned P0 issues is exactly 0, so the inclusive target reads as the value alone.
  expect(within(card("P0 issues with no owner")).getByText("target 0")).toBeDefined();
  const runs = within(card("Deploy runs reaching production"));
  expect(runs.getByText("target off")).toBeDefined();
  expect(runs.getByText("no data")).toBeDefined();
});

test("a judge another judge implies is not named, and its figure is toned all the same by its own value: PR open to merge judged by its longest change and its median", () => {
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      opened_to_merge_median_minutes: {
        ...catalogueTargets.opened_to_merge_median_minutes,
        judges: ["minutes.max", "minutes.median"],
      },
    },
  });
  const opened = within(card("PR open \u2192 merge"));
  expect(opened.getByText("target < 60m for every change")).toBeDefined();
  // The caption names the longest change, which implies the median, and the median is judged all
  // the same: 5.5h misses under 60m by its own value.
  expect(opened.getByText("5.5h").className).toContain(dangerText);
  cleanup();

  // Better higher nothing is implied, so both are named, and the median, 5.5h, meets above 60m by
  // its own value beside the missed badge.
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      opened_to_merge_median_minutes: {
        ...catalogueTargets.opened_to_merge_median_minutes,
        direction: "higher_better",
        judges: ["minutes.max", "minutes.median"],
      },
    },
  });
  const higher = within(card("PR open \u2192 merge"));
  expect(higher.getByText("target longest and median > 60m")).toBeDefined();
  expect(higher.getByText("5.5h").className).toContain(successText);
  expect(higher.getByText("\u2717 missed")).toBeDefined();
});

// R5: a person may judge a delivery eval by any statistic the answer states, and the verdict
// follows the judges. The card names what the verdict is of, rather than the statistic the
// catalogue judges: "median" under a card whose badge judges the p90 would be a wrong caption.
test("a lead-time card names the statistics its target judges and tones those figures: PR open to merge judged by its p90", () => {
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      opened_to_merge_median_minutes: {
        value: 720,
        inclusive: true,
        direction: "lower_better",
        judges: ["minutes.p90"],
      },
      merge_to_production_minutes: {
        ...catalogueTargets.merge_to_production_minutes,
        judges: ["minutes.p90"],
      },
    },
    status: { ...fixtureMeasures.status, opened_to_merge: true },
  });
  const opened = within(card("PR open \u2192 merge"));
  expect(opened.getByText("target p90 \u2264 720m")).toBeDefined();
  expect(opened.queryByText(/^target median/)).not.toBeInTheDocument();
  // The p90, 7.7h, meets at most 720m by its own value, and the median, which the target does not
  // judge, reads plain.
  expect(opened.getByText("7.7h").className).toContain(successText);
  expect(opened.getByText("5.5h").className).toContain(textPrimaryOnSurface);
  expect(opened.getByText("5.5h").className).not.toContain(successText);

  // Merge to production judged by a statistic the card shows no figure of: the caption names it,
  // and the longest change, which the target does not judge, takes no tone.
  const merge = within(card("Merge \u2192 production"));
  expect(merge.getByText("target p90 < 45m")).toBeDefined();
  expect(merge.getByText("2.7d").className).toContain(textPrimaryOnSurface);
  expect(merge.getByText("2.7d").className).not.toContain(dangerText);
});

// R5, and review v2's R2-3: a figure takes a tone only where the card's target judges the
// statistic it shows. The answer's verdicts of the merge median and of the failure rate's upper
// bound are null where the target's judges leave that statistic out (api/delivery_measures.go
// measuresStatus, deliveryJudgeVerdict), as these judges do, and the figure reads plain rather
// than toned by its own value (dispatch://INFRA-2916: the testing round of 2026-11-18 at
// 0ea9989f drove a median verdict under the longest change's judge from a server that then
// judged the median whatever the judges, judges-median-W.r10.out).
test("a figure whose statistic the target does not judge takes no tone, and the answer carries no verdict of it: the merge median under its longest change, the upper bound under the per-PR rate", () => {
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      merge_to_production_minutes: {
        value: 1000,
        inclusive: false,
        direction: "lower_better",
        judges: ["minutes.max"],
      },
    },
    status: {
      ...fixtureMeasures.status,
      merge_to_production: false,
      merge_to_production_median: null,
    },
  });
  const merge = within(card("Merge \u2192 production"));
  expect(merge.getByText(/^target /).textContent ?? "").not.toContain("median");
  expect(merge.getByText("2.7d").className).toContain(dangerText);
  expect(merge.getByText("8.5h").className).toContain(textPrimaryOnSurface);
  expect(merge.getByText("8.5h").className).not.toContain(successText);
  cleanup();

  renderPanel({
    ...fixtureMeasures,
    flags_source: "flags",
    measures: {
      ...fixtureMeasures.measures,
      change_failure_rate: {
        per_pr: { confirmed: 0, pending: 0, rejected: 0, reverts: 0, total: 5, rate: 0 },
        per_deploy: {
          confirmed: 0,
          pending: 2,
          rejected: 0,
          reverts: 0,
          total: 5,
          rate: 0,
          confirmed_or_pending: 2,
          upper_bound_rate: 0.4,
        },
      },
    },
    targets: {
      ...catalogueTargets,
      change_failure_rate: {
        value: 0.05,
        inclusive: false,
        direction: "lower_better",
        judges: ["per_pr.rate"],
      },
    },
    status: {
      ...fixtureMeasures.status,
      change_failure_rate: true,
      change_failure_rate_upper_bound: null,
    },
  });
  const failures = within(card("Change failure rate"));
  expect(failures.getByText(/^target /).textContent ?? "").not.toContain("upper");
  expect(failures.getByText("40.00%").className).not.toContain(dangerText);
  expect(failures.getByText("40.00%").className).not.toContain(successText);
});

// R5: a card's headline figure is one statistic of its eval and its badge the server's verdict of
// every statistic the target judges: where a person judges another statistic, the badge follows
// that verdict and the headline figure reads plain. Under the catalogue's judges each headline
// figure is the statistic judged, and is toned by its own value.
test("a headline figure its target does not judge takes no tone: the per-deploy rate judged by the per-PR rate, deploys a day judged by the deploys with a PR", () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: { ...fixtureMeasures.measures, change_failure_rate: measuredFailureRate },
    targets: {
      ...catalogueTargets,
      change_failure_rate: { ...catalogueTargets.change_failure_rate, judges: ["per_pr.rate"] },
      deploys_per_day: {
        ...catalogueTargets.deploys_per_day,
        judges: ["deploys.with_prs_per_day"],
      },
    },
    // The per-PR rate, 40%, misses under 5%, and 0.17 deploys with a PR a day miss at least 20.
    status: { ...fixtureMeasures.status, change_failure_rate: false, deploys_per_day: false },
  });
  const failures = within(card("Change failure rate"));
  expect(failures.getByText("\u2717 missed")).toBeDefined();
  expect(failures.getByText("2.00%").className).toContain(textPrimaryOnSurface);
  expect(failures.getByText("2.00%").className).not.toContain(dangerText);
  const deploys = within(card("Deploys a day"));
  expect(deploys.getByText("\u2717 missed")).toBeDefined();
  expect(deploys.getByText("0.2/day").className).toContain(textPrimaryOnSurface);
  expect(deploys.getByText("0.2/day").className).not.toContain(dangerText);
  cleanup();

  // The catalogue's judges: each headline figure by its own value. The per-deploy rate, 2%, meets
  // under 5%; 0.17 deploys a day, 71.4% and one unowned P0 miss.
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: { ...fixtureMeasures.measures, change_failure_rate: measuredFailureRate },
    status: { ...fixtureMeasures.status, change_failure_rate: true },
  });
  expect(within(card("Change failure rate")).getByText("2.00%").className).toContain(successText);
  expect(within(card("Deploys a day")).getByText("0.2/day").className).toContain(dangerText);
  expect(within(card("Deploy runs reaching production")).getByText("71.4%").className).toContain(
    dangerText
  );
  expect(within(card("P0 issues with no owner")).getByText("1").className).toContain(dangerText);
});

test("a target is phrased and its figures are judged in the direction it carries: the same value reads and tones the other way under lower_better", () => {
  const halfADay = { ...catalogueTargets.deploys_per_day, value: 0.5 };
  renderPanel({
    ...fixtureMeasures,
    targets: { ...catalogueTargets, deploys_per_day: halfADay },
  });
  expect(within(card("Deploys a day")).getByText("target \u2265 0.5/day")).toBeDefined();
  // 2026-09-04 deployed nothing and 2026-09-05 once: the sixth and seventh day of the window.
  expect(chart("Deploys a day").bars.slice(5, 7)).toEqual([chartBar.missed, chartBar.met]);
  // The window's 0.17 deploys a day miss at least 0.5, and meet at most 0.5 beside the same
  // missed badge.
  expect(tones("Deploys a day")["deploys.per_day"]).toBe("missed");
  cleanup();

  renderPanel({
    ...fixtureMeasures,
    targets: { ...catalogueTargets, deploys_per_day: { ...halfADay, direction: "lower_better" } },
  });
  expect(within(card("Deploys a day")).getByText("target \u2264 0.5/day")).toBeDefined();
  expect(chart("Deploys a day").bars.slice(5, 7)).toEqual([chartBar.met, chartBar.missed]);
  expect(tones("Deploys a day")["deploys.per_day"]).toBe("met");
});

test("a bar is toned by its own day's value of the statistic it draws, and reads neutral under no target line where the target judges a statistic no bar draws", () => {
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      // Run success judged by its concluded runs, at least one.
      deploy_run_success_rate: {
        value: 1,
        inclusive: true,
        direction: "higher_better",
        judges: ["runs.concluded"],
      },
      // Deploys judged by those that shipped a pull request, which no day row states.
      deploys_per_day: { ...catalogueTargets.deploys_per_day, judges: ["deploys.with_prs"] },
    },
  });
  const runs = chart("Deploy runs reaching production");
  expect(
    within(card("Deploy runs reaching production")).getByText("target concluded runs \u2265 1")
  ).toBeDefined();
  // 2026-09-11, the thirteenth day: one run concluded and none reached production. The bars draw
  // the success rate, which the target is not of, and a count of the window judges no day: the
  // bar reads neutral, as every bar does, and no line is drawn over them.
  expect(runs.bars[12]).toBe(chartBar.neutral);
  expect(toned(runs.bars)).toEqual([]);
  expect(runs.lines).toBe(0);

  const deploys = chart("Deploys a day");
  expect(
    within(card("Deploys a day")).getByText("target deploys with a PR \u2265 20")
  ).toBeDefined();
  expect(new Set(deploys.bars)).toEqual(new Set([chartBar.neutral]));
  expect(deploys.lines).toBe(0);
  cleanup();

  // The catalogue's judges: each bar is its own day's value's verdict, under its target line.
  renderPanel(fixtureMeasures);
  expect(chart("Deploy runs reaching production").bars[12]).toBe(chartBar.missed);
  expect(chart("Deploy runs reaching production").bars[6]).toBe(chartBar.met);
  expect(chart("Deploy runs reaching production").lines).toBe(1);
  expect(chart("Deploys a day").lines).toBe(1);
});

// R5, the architect's ruling of 2026-11-19 on the tester's round-11 finding: a bar is a figure
// too, so it takes a tone only where the target judges the statistic it draws. Judged by its
// concluded runs, the success-rate bars draw a statistic the target is not of: every bar reads
// neutral and no line is drawn, as the headline rate and the table's rate cells already do. At
// 06b73b46 the bars take each day's concluded-count verdict instead: under "at least 2" every
// bar is red, the 100% days among them, under a met badge (dispatch://INFRA-2916, the testing
// round of 2026-11-19 at 06b73b46).
test("a bar is toned only where the target judges the statistic it draws: run success judged by its concluded runs draws neutral bars", () => {
  for (const value of [1, 2]) {
    renderPanel({
      ...fixtureMeasures,
      targets: {
        ...catalogueTargets,
        deploy_run_success_rate: {
          value,
          inclusive: true,
          direction: "higher_better",
          judges: ["runs.concluded"],
        },
      },
      status: { ...fixtureMeasures.status, deploy_run_success: true },
    });
    const runs = chart("Deploy runs reaching production");
    const toned: string[] = [
      chartBar.met,
      chartBar.missed,
      chartBar.metFaint,
      chartBar.missedFaint,
    ];
    expect(runs.bars.filter((bar) => toned.includes(bar ?? ""))).toEqual([]);
    // 2026-09-11, the thirteenth day: 0% success, one concluded run.
    expect(runs.bars[12]).toBe(chartBar.neutral);
    expect(runs.lines).toBe(0);
    cleanup();
  }
});

// R5's tone rule (dispatch://INFRA-2916, check k31, the architect's of 2026-11-19), for every
// element of the panel that can take a met or missed tone: the badge, each headline figure and
// count, each per-day cell, each chart bar and each chart's dashed target line. The tests below
// state its clauses, each with values where an element's own value and its card's badge disagree,
// so an element toned by the badge, or by a day's verdict over every judge, reads wrong.

// R5 (1): an eval with no target, which a person turned off, judges nothing.
test("an eval with no target tones nothing: no figure, count, bar or per-day cell takes a tone, and no chart draws a target line", async () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: { ...fixtureMeasures.measures, change_failure_rate: measuredFailureRate },
    targets: {
      deploys_per_day: null,
      change_failure_rate: null,
      merge_to_production_minutes: null,
      opened_to_merge_median_minutes: null,
      deploy_run_success_rate: null,
      unowned_p0: null,
    },
    status: {
      deploys_per_day: null,
      change_failure_rate: null,
      change_failure_rate_upper_bound: null,
      merge_to_production_median: null,
      merge_to_production: null,
      opened_to_merge: null,
      deploy_run_success: null,
      unowned_p0: null,
    },
  });
  expect(Object.fromEntries(TITLES.map((title) => [title, tones(title)]))).toEqual({
    "Deploys a day": { "deploys.per_day": "plain", "deploys.deploys": "plain" },
    "Change failure rate": {
      "per_deploy.rate": "plain",
      "upper_bound.rate": "plain",
      "per_deploy.confirmed": "plain",
    },
    "Merge \u2192 production": { "minutes.median": "plain", "minutes.max": "plain" },
    "PR open \u2192 merge": { "minutes.median": "plain", "minutes.p90": "plain" },
    "Deploy runs reaching production": {
      "runs.rate": "plain",
      "runs.reached_production": "plain",
      "runs.concluded": "plain",
      "runs.cancelled": "plain",
    },
    "P0 issues with no owner": { "issues.value": "plain" },
  });
  // Every element a target may tone names the eval whose target it is, the strip's included.
  expect(TITLES.map((title) => evalsIn(card(title)))).toEqual([
    ["deploy-frequency"],
    ["change-fail-rate"],
    ["merge-to-production"],
    ["pr-open-to-merge"],
    ["deploy-run-success"],
    ["unowned-p0"],
  ]);
  expect(evalsIn(screen.getByTestId("measures-strip"))).toEqual([
    "deploy-frequency",
    "merge-to-production",
    "change-fail-rate",
  ]);
  // A headline figure that takes no tone reads in the card's primary text, not muted.
  for (const figure of document.querySelectorAll(".text-lg[data-statistic]")) {
    expect(figure.className).toContain(textPrimaryOnSurface);
  }
  for (const title of ["Deploys a day", "Deploy runs reaching production"]) {
    expect(toned(chart(title).bars)).toEqual([]);
    expect(chart(title).lines).toBe(0);
  }
  await openFold();
  // 2026-09-04 deployed nothing and ran nothing, 2026-09-05 deployed once and every run succeeded,
  // and 2026-09-11 deployed nothing and its one run failed.
  for (const day of ["2026-09-04", "2026-09-05", "2026-09-11"]) {
    expect(cellTones(day)).toEqual({ "deploys.per_day": "plain", "runs.rate": "plain" });
  }
});

// R5 (2) and (3): the badge is the server's verdict of the whole window (`status`); every other
// figure is toned by its own value, so none takes the badge's tone where its own value says
// otherwise.
test("the badge is the server's verdict of the window and no figure takes it: each figure is toned by its own value where the two disagree", () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: { ...fixtureMeasures.measures, change_failure_rate: measuredFailureRate },
    targets: {
      ...catalogueTargets,
      opened_to_merge_median_minutes: {
        ...catalogueTargets.opened_to_merge_median_minutes,
        value: 400,
      },
    },
    // Each verdict the opposite of what its card's judged figures say of themselves.
    status: {
      deploys_per_day: true,
      change_failure_rate: false,
      change_failure_rate_upper_bound: false,
      merge_to_production_median: true,
      merge_to_production: true,
      opened_to_merge: false,
      deploy_run_success: true,
      unowned_p0: true,
    },
  });
  expect(
    TITLES.map((title) =>
      card(title).querySelector("[data-target-met]")?.getAttribute("data-target-met")
    )
  ).toEqual(["true", "false", "true", "false", "true", "true"]);
  // 0.17 deploys a day miss at least 20; 2% of deploys meet under 5%; the merge median, 8.5h, and
  // longest change, 2.7d, miss under 45m; the open median, 5.5h, meets under 400m; 71.4% misses
  // at least 90%; and one unowned P0 misses 0. Every other figure is of a statistic no judge
  // names.
  expect(Object.fromEntries(TITLES.map((title) => [title, tones(title)]))).toEqual({
    "Deploys a day": { "deploys.per_day": "missed", "deploys.deploys": "plain" },
    "Change failure rate": {
      "per_deploy.rate": "met",
      "upper_bound.rate": "plain",
      "per_deploy.confirmed": "plain",
    },
    "Merge \u2192 production": { "minutes.median": "missed", "minutes.max": "missed" },
    "PR open \u2192 merge": { "minutes.median": "met", "minutes.p90": "plain" },
    "Deploy runs reaching production": {
      "runs.rate": "missed",
      "runs.reached_production": "plain",
      "runs.concluded": "plain",
      "runs.cancelled": "plain",
    },
    "P0 issues with no owner": { "issues.value": "missed" },
  });
  // The strip's figures too: its deploys a day and its merge median miss, and no judge names the
  // per-PR rate. Its time to restore and its rework share are of no eval's statistic.
  expect(tonesIn(screen.getByTestId("measures-strip"), "window")).toEqual({
    "deploys.per_day": "missed",
    "minutes.median": "missed",
    "per_pr.rate": "plain",
  });
});

// R5 (3): the fold's figures are figures too. Each of a statistic a delivery eval's target may
// judge names that eval and takes its own verdict under that target, counts and rates alike; one
// of a statistic no eval judges names none and takes no tone.
test("each fold figure of a statistic a delivery eval may judge is toned by its own value under that eval's target, and a figure of no eval's statistic by nothing", async () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: { ...fixtureMeasures.measures, change_failure_rate: measuredFailureRate },
    targets: {
      ...catalogueTargets,
      deploys_per_day: {
        value: 1,
        inclusive: true,
        direction: "higher_better",
        judges: ["deploys.per_day", "deploys.with_prs"],
      },
      merge_to_production_minutes: { ...catalogueTargets.merge_to_production_minutes, value: 600 },
      change_failure_rate: {
        value: 1,
        inclusive: false,
        direction: "lower_better",
        judges: ["per_pr.rate", "per_deploy.total"],
      },
    },
  });
  await openFold();
  // At least 1: 0.17 deploys a day miss, 5 deploys with a PR meet. Under 600m: the merge median,
  // 8.5h, meets, its longest change, 2.7d, misses, and no judge names its p90. Under 1: a 40%
  // failure rate per PR meets, 50 deploys miss.
  const titles = [
    "Deploy frequency",
    "Lead time (merge \u2192 prod)",
    "Lead time (first commit \u2192 prod)",
    "Lead time (PR opened \u2192 prod)",
    "Change failure rate (per PR)",
    "Change failure rate (per deploy)",
    "Time to restore",
    "Rework share",
  ];
  expect(
    Object.fromEntries(titles.map((title) => [title, tonesIn(definition(title), "window")]))
  ).toEqual({
    "Deploy frequency": {
      "deploys.per_day": "missed",
      "deploys.deploys": "plain",
      "deploys.with_prs": "met",
      "deploys.with_prs_per_day": "plain",
    },
    "Lead time (merge \u2192 prod)": {
      "minutes.median": "met",
      "minutes.p90": "plain",
      "minutes.max": "missed",
    },
    "Lead time (first commit \u2192 prod)": {},
    "Lead time (PR opened \u2192 prod)": {},
    "Change failure rate (per PR)": {
      "per_pr.rate": "met",
      "per_pr.confirmed": "plain",
      "per_pr.reverts": "plain",
      "per_pr.pending": "plain",
      "per_pr.rejected": "plain",
      "per_pr.total": "plain",
    },
    "Change failure rate (per deploy)": {
      "per_deploy.rate": "plain",
      "per_deploy.confirmed": "plain",
      "per_deploy.total": "missed",
      "upper_bound.rate": "plain",
    },
    "Time to restore": {},
    "Rework share": {},
  });
  // Each names the eval whose target judges it.
  expect(titles.map((title) => evalsIn(definition(title)))).toEqual([
    ["deploy-frequency"],
    ["merge-to-production"],
    [],
    [],
    ["change-fail-rate"],
    ["change-fail-rate"],
    [],
    [],
  ]);
  // The strip under the same targets: its deploys a day miss, and its merge median and its per-PR
  // failure rate meet. The strip's per-PR rate is the one figure outside the fold that a judge of
  // the per-PR rate tones.
  expect(tonesIn(screen.getByTestId("measures-strip"), "window")).toEqual({
    "deploys.per_day": "missed",
    "minutes.median": "met",
    "per_pr.rate": "met",
  });
  // The cards' text reads as before.
  expect(definition("Deploy frequency").textContent).toBe(
    "Deploy frequency0.17/day5 successful (5 shipped a PR, 0.17/day)"
  );
  expect(definition("Change failure rate (per PR)").textContent).toBe(
    "Change failure rate (per PR)40.0%2 confirmed (0 reverts), 0 pending, 0 rejected of 5 PRs. Incidents: no source yet."
  );
  expect(definition("Change failure rate (per deploy)").textContent).toBe(
    "Change failure rate (per deploy)2.0%1 of 50 deploys; 4.0% if every pending flag is confirmed"
  );
});

// R5 (3) and (4), one judge on a rate the bars draw: its window figure, each day's bar and each
// day's cell by its own value.
test("deploy run success judged by its rate tones that rate, each day's bar and each day's cell by its own value, under its target line", async () => {
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      deploy_run_success_rate: { ...catalogueTargets.deploy_run_success_rate, value: 0.7 },
    },
    status: { ...fixtureMeasures.status, deploy_run_success: false },
  });
  // The window's 71.4% meets at least 70% beside a missed badge; its counts are not judged.
  expect(tones("Deploy runs reaching production")).toEqual({
    "runs.rate": "met",
    "runs.reached_production": "plain",
    "runs.concluded": "plain",
    "runs.cancelled": "plain",
  });
  // 2026-09-05, the seventh day, succeeded every run, and 2026-09-11, the thirteenth, none.
  const runs = chart("Deploy runs reaching production");
  expect([runs.bars[6], runs.bars[12], runs.lines]).toEqual([chartBar.met, chartBar.missed, 1]);
  await openFold();
  // 2026-09-04 concluded no run, so it has no success rate to judge.
  expect(
    ["2026-09-04", "2026-09-05", "2026-09-11"].map((day) => cellTones(day)["runs.rate"])
  ).toEqual(["plain", "met", "missed"]);
});

// R5 (4), one judge on a count of the window: the count's own figure takes its verdict, and no
// day of it takes a tone, so no bar or cell does and no line is drawn.
test("deploy run success judged by its concluded runs tones that count alone: neutral bars, plain cells and no target line", async () => {
  // The window concluded 7 runs: at least 7 meets and at least 8 misses, each beside the other
  // badge. A day's one concluded run would miss either, were a day judged by the window's count.
  for (const [value, verdict] of [
    [7, "met"],
    [8, "missed"],
  ] as const) {
    renderPanel({
      ...fixtureMeasures,
      targets: {
        ...catalogueTargets,
        deploy_run_success_rate: {
          value,
          inclusive: true,
          direction: "higher_better",
          judges: ["runs.concluded"],
        },
      },
      status: { ...fixtureMeasures.status, deploy_run_success: verdict === "missed" },
    });
    expect(tones("Deploy runs reaching production")).toEqual({
      "runs.rate": "plain",
      "runs.reached_production": "plain",
      "runs.concluded": verdict,
      "runs.cancelled": "plain",
    });
    const runs = chart("Deploy runs reaching production");
    expect([toned(runs.bars), runs.lines]).toEqual([[], 0]);
    await openFold();
    expect(
      ["2026-09-04", "2026-09-05", "2026-09-11"].map((day) => cellTones(day)["runs.rate"])
    ).toEqual(["plain", "plain", "plain"]);
    cleanup();
  }
});

// R5 (4): the window's count of deploys judges no day, so the deploys bars and cells, each a day's
// deploys a day, take no tone under it; the count itself does, on its card and in the fold.
test("deploys judged by their count in the window tone that count alone: neutral bars, plain cells and no target line", async () => {
  // The window deployed 5 times: at least 5 meets and at least 6 misses, each beside the other
  // badge. A day's one deploy would miss either, were a day judged by the window's count.
  for (const [value, verdict] of [
    [5, "met"],
    [6, "missed"],
  ] as const) {
    renderPanel({
      ...fixtureMeasures,
      targets: {
        ...catalogueTargets,
        deploys_per_day: {
          value,
          inclusive: true,
          direction: "higher_better",
          judges: ["deploys.deploys"],
        },
      },
      status: { ...fixtureMeasures.status, deploys_per_day: verdict === "missed" },
    });
    expect(within(card("Deploys a day")).getByText(`target deploys \u2265 ${value}`)).toBeDefined();
    expect(tones("Deploys a day")).toEqual({
      "deploys.per_day": "plain",
      "deploys.deploys": verdict,
    });
    const deploys = chart("Deploys a day");
    expect([toned(deploys.bars), deploys.lines]).toEqual([[], 0]);
    await openFold();
    expect(["2026-09-04", "2026-09-05"].map((day) => cellTones(day)["deploys.per_day"])).toEqual([
      "plain",
      "plain",
    ]);
    // The fold's deploy frequency card writes the same count, "5 successful", which takes the
    // same verdict; its other figures are not judged.
    expect(tonesIn(definition("Deploy frequency"), "window")).toEqual({
      "deploys.per_day": "plain",
      "deploys.deploys": verdict,
      "deploys.with_prs": "plain",
      "deploys.with_prs_per_day": "plain",
    });
    cleanup();
  }
});

// R5 (3): a count no figure of the card shows tones none of its figures.
test("merge to production judged by its count of pull requests tones neither its median nor its longest change, in its seeded direction and the other", () => {
  for (const direction of ["lower_better", "higher_better"] as const) {
    renderPanel({
      ...fixtureMeasures,
      targets: {
        ...catalogueTargets,
        merge_to_production_minutes: {
          value: 2,
          inclusive: true,
          direction,
          judges: ["minutes.n"],
        },
      },
      status: { ...fixtureMeasures.status, merge_to_production: true },
    });
    expect(tones("Merge \u2192 production")).toEqual({
      "minutes.median": "plain",
      "minutes.max": "plain",
    });
    cleanup();
  }
});

// R5 (3) and (4): the change failure rate's breakdown counts the deploys with a confirmed break,
// per_deploy.confirmed, and the deploys with only unreviewed flags, confirmed_or_pending less it,
// which no judge names (per_deploy.pending counts every deploy with a pending flag).
test("the change failure rate judged by its deploys with a confirmed break tones that count alone, and its deploys with only unreviewed flags by nothing", () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: { ...fixtureMeasures.measures, change_failure_rate: measuredFailureRate },
    targets: {
      ...catalogueTargets,
      change_failure_rate: {
        value: 0,
        inclusive: true,
        direction: "lower_better",
        judges: ["per_deploy.confirmed"],
      },
    },
    status: { ...fixtureMeasures.status, change_failure_rate: true },
  });
  const failures = within(card("Change failure rate"));
  expect(
    failures.getByText(
      wholeText("1 deploy with a confirmed break; 1 more with only unreviewed flags")
    )
  ).toBeDefined();
  // One deploy with a confirmed break misses none beside a met badge; the rates are not judged,
  // and the deploys with only unreviewed flags name no statistic.
  expect(tones("Change failure rate")).toEqual({
    "per_deploy.rate": "plain",
    "upper_bound.rate": "plain",
    "per_deploy.confirmed": "missed",
  });
});

// R5 (3): the failure rate's upper bound, drawn where a failure source stores flags, judged by
// itself.
test("the change failure rate judged by its upper bound tones that figure by its own value: met under the target, missed over it", () => {
  // 4% if every pending flag is confirmed: under 5% meets and under 3% misses, each beside the
  // other badge.
  for (const [value, verdict] of [
    [0.05, "met"],
    [0.03, "missed"],
  ] as const) {
    renderPanel({
      ...fixtureMeasures,
      flags_source: "stored",
      measures: { ...fixtureMeasures.measures, change_failure_rate: measuredFailureRate },
      targets: {
        ...catalogueTargets,
        change_failure_rate: {
          ...catalogueTargets.change_failure_rate,
          value,
          judges: ["upper_bound.rate"],
        },
      },
      status: {
        ...fixtureMeasures.status,
        change_failure_rate: verdict === "missed",
        change_failure_rate_upper_bound: verdict === "missed",
      },
    });
    expect(
      within(card("Change failure rate")).getByText(
        wholeText("up to 4.00% if every pending flag is confirmed")
      )
    ).toBeDefined();
    expect(tones("Change failure rate")).toEqual({
      "per_deploy.rate": "plain",
      "upper_bound.rate": verdict,
      "per_deploy.confirmed": "plain",
    });
    cleanup();
  }
});

// R5 (3), two judges on one card, one met and one missed: each figure by its own judge, and a
// figure of a statistic neither names by nothing.
test("merge to production judged by its longest change and its median tones each by its own value, in its seeded direction and the other", () => {
  // Under 600m, lower_better as the catalogue seeds it, the median, 8.5h, meets and the longest
  // change, 2.7d, misses; above 600m the other way round. The badge says met throughout.
  for (const [direction, caption, median, max] of [
    ["lower_better", "target < 600m for every change", "met", "missed"],
    ["higher_better", "target longest and median > 600m", "missed", "met"],
  ] as const) {
    renderPanel({
      ...fixtureMeasures,
      targets: {
        ...catalogueTargets,
        merge_to_production_minutes: {
          ...catalogueTargets.merge_to_production_minutes,
          value: 600,
          direction,
        },
      },
      status: {
        ...fixtureMeasures.status,
        merge_to_production: true,
        merge_to_production_median: false,
      },
    });
    expect(within(card("Merge \u2192 production")).getByText(caption)).toBeDefined();
    expect(tones("Merge \u2192 production")).toEqual({
      "minutes.median": median,
      "minutes.max": max,
    });
    cleanup();
  }

  // PR open to merge under the same two judges: the median, 5.5h, meets under 400m, and no judge
  // names the p90, which reads plain though 7.7h is over 400m.
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      opened_to_merge_median_minutes: {
        ...catalogueTargets.merge_to_production_minutes,
        value: 400,
      },
    },
    status: { ...fixtureMeasures.status, opened_to_merge: false },
  });
  expect(
    within(card("PR open \u2192 merge")).getByText("target < 400m for every change")
  ).toBeDefined();
  expect(tones("PR open \u2192 merge")).toEqual({
    "minutes.median": "met",
    "minutes.p90": "plain",
  });
});

test("PR open to merge judged by its median and its p90 tones each by its own value: the median met, the p90 missed", () => {
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...catalogueTargets,
      opened_to_merge_median_minutes: {
        value: 400,
        inclusive: false,
        direction: "lower_better",
        judges: ["minutes.median", "minutes.p90"],
      },
    },
    status: { ...fixtureMeasures.status, opened_to_merge: true },
  });
  expect(
    within(card("PR open \u2192 merge")).getByText("target median and p90 < 400m")
  ).toBeDefined();
  // The median, 5.5h, meets under 400m and the p90, 7.7h, misses it beside a met badge.
  expect(tones("PR open \u2192 merge")).toEqual({
    "minutes.median": "met",
    "minutes.p90": "missed",
  });
});

// R5 (5): a chart draws its dashed target line exactly where its bars take a tone: where the
// target judges one of the day statistics the chart's bars draw, whether or not a day has a value.
test("a chart draws its target line exactly where its bars take a tone: where the target judges a statistic of a day its bars draw", () => {
  const deployJudges: [NonNullable<Targets["deploys_per_day"]>["judges"], boolean][] = [
    [["deploys.per_day"], true],
    [["deploys.per_day", "deploys.deploys"], true],
    [["deploys.deploys"], false],
    [["deploys.with_prs_per_day"], false],
  ];
  for (const [judges, drawn] of deployJudges) {
    renderPanel({
      ...fixtureMeasures,
      targets: {
        ...catalogueTargets,
        deploys_per_day: { ...catalogueTargets.deploys_per_day, value: 0.5, judges },
      },
    });
    const deploys = chart("Deploys a day");
    expect([judges, toned(deploys.bars).length > 0, deploys.lines]).toEqual([
      judges,
      drawn,
      drawn ? 1 : 0,
    ]);
    cleanup();
  }

  const runJudges: [NonNullable<Targets["deploy_run_success_rate"]>["judges"], boolean][] = [
    [["runs.rate"], true],
    [["runs.rate", "runs.concluded"], true],
    [["runs.concluded"], false],
  ];
  for (const [judges, drawn] of runJudges) {
    renderPanel({
      ...fixtureMeasures,
      targets: {
        ...catalogueTargets,
        deploy_run_success_rate: { ...catalogueTargets.deploy_run_success_rate, judges },
      },
    });
    const runs = chart("Deploy runs reaching production");
    expect([judges, toned(runs.bars).length > 0, runs.lines]).toEqual([
      judges,
      drawn,
      drawn ? 1 : 0,
    ]);
    cleanup();
  }

  // A window in which no day concluded a run: no bar has a value to tone, and the line is drawn
  // all the same, since the target judges the statistic the bars draw.
  renderPanel({
    ...fixtureMeasures,
    measures: {
      ...fixtureMeasures.measures,
      daily: fixtureDaily().map((day) => ({ ...day, run_success_rate: null })),
    },
  });
  const empty = chart("Deploy runs reaching production");
  expect([toned(empty.bars), empty.lines]).toEqual([[], 1]);
});

// R3-C1, the architect's rule of 2026-11-19 for review round 3: a toned figure's printed text lies
// on the same side of its target as its value. A figure that rounds to the target itself or past
// it is written with more digits, until it does not: "5.0%" in green beside "< 5%", or "45m" in
// green beside "< 45m for every change", reads as a figure its own tone contradicts.
test("a toned figure is written on its value's side of the target: more digits where the usual ones would print the target itself or past it", async () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: {
      ...fixtureMeasures.measures,
      deploy_frequency: { ...fixtureMeasures.measures.deploy_frequency, per_day: 19.96 },
      lead_time: {
        ...fixtureMeasures.measures.lead_time,
        merge_to_production: { median_minutes: 44.6, p90_minutes: 44.8, max_minutes: 44.9 },
      },
      change_failure_rate: {
        ...measuredFailureRate,
        per_deploy: { ...measuredFailureRate.per_deploy, rate: 0.0496 },
      },
      deploy_run_success: { ...fixtureMeasures.measures.deploy_run_success, rate: 0.8996 },
    },
  });
  // At least 20 a day: 19.96 misses, where one decimal would print 20.0. Under 5%: 4.96% meets,
  // which the headline's two decimals already print. Under 45m for every change: a median of 44.6m
  // and a longest change of 44.9m meet, where whole minutes would print 45m. At least 90%: 89.96%
  // misses, where one decimal would print 90.0%. Every other figure is of a statistic no judge
  // names, and keeps its usual digits.
  expect(Object.fromEntries(TITLES.map((title) => [title, written(card(title))]))).toEqual({
    "Deploys a day": { "deploys.per_day": "19.96/day missed", "deploys.deploys": "5 plain" },
    "Change failure rate": {
      "per_deploy.rate": "4.96% met",
      "upper_bound.rate": "4.00% plain",
      "per_deploy.confirmed": "1 plain",
    },
    "Merge \u2192 production": { "minutes.median": "44.6m met", "minutes.max": "44.9m met" },
    "PR open \u2192 merge": { "minutes.median": "5.5h missed", "minutes.p90": "7.7h plain" },
    "Deploy runs reaching production": {
      "runs.rate": "89.96% missed",
      "runs.reached_production": "5 plain",
      "runs.concluded": "7 plain",
      "runs.cancelled": "1 plain",
    },
    "P0 issues with no owner": { "issues.value": "1 missed" },
  });
  // The strip writes deploys a day to two decimals, which already print 19.96.
  expect(written(screen.getByTestId("measures-strip"))).toEqual({
    "deploys.per_day": "19.96/day missed",
    "minutes.median": "44.6m met",
    "per_pr.rate": "40.0% plain",
  });
  await openFold();
  // The fold writes rates to one decimal: the per-deploy rate, 4.96%, would print 5.0%. The p90,
  // which no judge names, keeps its whole minutes.
  expect(written(definition("Lead time (merge \u2192 prod)"))).toEqual({
    "minutes.median": "44.6m met",
    "minutes.p90": "45m plain",
    "minutes.max": "44.9m met",
  });
  expect(written(definition("Change failure rate (per deploy)"))).toEqual({
    "per_deploy.rate": "4.96% met",
    "per_deploy.confirmed": "1 plain",
    "per_deploy.total": "50 plain",
    "upper_bound.rate": "4.0% plain",
  });
});

// R3-C1: the digits are added only where the usual ones would print the figure on the other side
// of its target; a figure whose usual digits already print it on its value's side keeps them.
test("a toned figure whose usual digits print it on its value's side of the target keeps them", async () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: {
      ...fixtureMeasures.measures,
      lead_time: {
        ...fixtureMeasures.measures.lead_time,
        merge_to_production: { median_minutes: 44.4, p90_minutes: 2736, max_minutes: 3840 },
      },
      change_failure_rate: {
        ...measuredFailureRate,
        per_deploy: { ...measuredFailureRate.per_deploy, rate: 0.0449 },
      },
    },
  });
  // Under 5%: 4.49% meets, and one decimal prints 4.5%, under it. Under 45m: a median of 44.4m
  // prints 44m, under it, and a longest change of 2.7d misses, far over it.
  expect(written(card("Merge \u2192 production"))).toEqual({
    "minutes.median": "44m met",
    "minutes.max": "2.7d missed",
  });
  expect(written(card("Change failure rate"))["per_deploy.rate"]).toBe("4.49% met");
  expect(written(card("Deploys a day"))["deploys.per_day"]).toBe("0.2/day missed");
  expect(written(card("Deploy runs reaching production"))["runs.rate"]).toBe("71.4% missed");
  expect(written(screen.getByTestId("measures-strip"))["minutes.median"]).toBe("44m met");
  await openFold();
  expect(written(definition("Change failure rate (per deploy)"))["per_deploy.rate"]).toBe(
    "4.5% met"
  );
  expect(written(definition("Lead time (merge \u2192 prod)"))["minutes.median"]).toBe("44m met");
});

// R3-T1: a day's Deploys cell is that day's deploys a day, toned by its own value where the
// target judges deploys.per_day; a partial day's cell takes no tone.
test("a day's Deploys cell is toned by its own deploys under deploys a day: a complete day meeting, one missing, and a partial day's cell untoned", async () => {
  renderPanel({
    ...fixtureMeasures,
    measures: {
      ...fixtureMeasures.measures,
      // The partial day, 2026-09-27, deployed twice, which would meet the target.
      daily: fixtureDaily().map((day) => (day.partial ? { ...day, deploys: 2 } : day)),
    },
    targets: {
      ...catalogueTargets,
      deploys_per_day: { ...catalogueTargets.deploys_per_day, value: 1 },
    },
  });
  await openFold();
  // At least one a day: 2026-09-05 deployed once and meets, 2026-09-04 deployed nothing and
  // misses, beside a missed badge.
  expect(
    card("Deploys a day").querySelector("[data-target-met]")?.getAttribute("data-target-met")
  ).toBe("false");
  expect(
    ["2026-09-05", "2026-09-04", "2026-09-27 (partial)"].map(
      (day) => cellTones(day)["deploys.per_day"]
    )
  ).toEqual(["met", "missed", "plain"]);
});

// R3-C1: a caption writes its target exactly, as the person set it. Rounded to a whole percent, a
// target of 75.5% read "> 76%" beside a run success of 75.6% in green.
test("a card's caption writes its rate target exactly: above 75.5% beside a run success of 75.6% that meets it, under 5% per deploy", () => {
  renderPanel({
    ...fixtureMeasures,
    measures: {
      ...fixtureMeasures.measures,
      deploy_run_success: { ...fixtureMeasures.measures.deploy_run_success, rate: 0.756 },
    },
    targets: {
      ...catalogueTargets,
      deploy_run_success_rate: {
        ...catalogueTargets.deploy_run_success_rate,
        value: 0.755,
        inclusive: false,
      },
    },
  });
  const runs = card("Deploy runs reaching production");
  expect(within(runs).getByText("target > 75.5%")).toBeDefined();
  expect(written(runs)["runs.rate"]).toBe("75.6% met");
  expect(within(card("Change failure rate")).getByText("target < 5% per deploy")).toBeDefined();
});

// R3-C1: a figure on its target is read back as the target itself. Read back by a floating-point
// product, 95.0% read as 0.9500000000000001, past "> 95%", and the card wrote the run success to
// its last digit: "95.000000000000000000%".
test("a run success on its strict target reads 95.0% and missed, with no digits past its usual one", () => {
  renderPanel({
    ...fixtureMeasures,
    measures: {
      ...fixtureMeasures.measures,
      deploy_run_success: { ...fixtureMeasures.measures.deploy_run_success, rate: 0.95 },
    },
    targets: {
      ...catalogueTargets,
      deploy_run_success_rate: {
        ...catalogueTargets.deploy_run_success_rate,
        value: 0.95,
        inclusive: false,
      },
    },
  });
  const runs = card("Deploy runs reaching production");
  expect(within(runs).getByText("target > 95%")).toBeDefined();
  expect(written(runs)["runs.rate"]).toBe("95.0% missed");
});

test("a lead-time card whose target is off reads off alone, with no verdict", () => {
  renderPanel({
    ...fixtureMeasures,
    targets: {
      ...fixtureMeasures.targets,
      merge_to_production_minutes: null,
      opened_to_merge_median_minutes: null,
    },
    status: {
      ...fixtureMeasures.status,
      merge_to_production: null,
      merge_to_production_median: null,
      opened_to_merge: null,
    },
  });
  for (const title of ["Merge \u2192 production", "PR open \u2192 merge"]) {
    const lead = within(card(title));
    expect(lead.getByText("target off")).toBeDefined();
    expect(lead.getByText("no data")).toBeDefined();
  }
});

test("with stored flags the change failure rate card shows the upper bound and the breakdown", () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    status: {
      ...fixtureMeasures.status,
      change_failure_rate: false,
      change_failure_rate_upper_bound: false,
    },
    measures: {
      ...fixtureMeasures.measures,
      change_failure_rate: {
        per_pr: { confirmed: 2, pending: 1, rejected: 1, reverts: 1, total: 14, rate: 2 / 14 },
        per_deploy: {
          confirmed: 2,
          pending: 0,
          rejected: 1,
          reverts: 1,
          total: 5,
          rate: 0.4,
          confirmed_or_pending: 2,
          upper_bound_rate: 0.4,
        },
      },
    },
  });
  const cfr = within(card("Change failure rate"));
  expect(cfr.getByText("40.00%", { selector: "span.text-lg" })).toBeDefined();
  expect(cfr.getByText("\u2717 missed")).toBeDefined();
  expect(cfr.getByText("confirmed")).toBeDefined();
  expect(
    cfr.getByText(
      (_, element) =>
        element?.tagName === "DIV" &&
        element.textContent === "up to 40.00% if every pending flag is confirmed"
    )
  ).toBeDefined();
  expect(
    cfr.getByText(wholeText("2 deploys with a confirmed break; none with only unreviewed flags"))
  ).toBeDefined();
  expect(cfr.queryByText("Flags arrive with flag confirmation")).not.toBeInTheDocument();
});

test("more than six unowned P0 issues list six and name the rest", () => {
  const unowned = Array.from({ length: 8 }, (_, i) => ({
    key: `ACME-${i + 1}`,
    title: `P0 ${i + 1}`,
  }));
  renderPanel({ ...fixtureMeasures, unowned_p0: unowned });
  const p0 = within(card("P0 issues with no owner"));
  expect(p0.getByText("8")).toBeDefined();
  expect(p0.getAllByRole("link")).toHaveLength(6);
  const more = p0.getByText("+2 more");
  expect(more).toHaveAttribute("title", "ACME-7, ACME-8");
});

test("the strip shows the five headline numbers", () => {
  renderPanel(fixtureMeasures);
  const strip = screen.getByTestId("measures-strip");
  for (const [value, label] of [
    ["0.17/day", "Deploy freq"],
    ["8.5h", "Lead time (merge\u2192prod)"],
    ["n/a", "CFR (per PR)"],
    ["n/a", "Time to restore"],
    ["28.6%", "Rework share"],
  ]) {
    const stat = within(strip).getByText(label).parentElement;
    expect(stat?.textContent).toBe(`${value}${label}`);
  }
});

test("the definitions fold opens to the definition cards and the per-day table, newest first", async () => {
  const user = userEvent.setup();
  renderPanel(fixtureMeasures);
  const toggle = screen.getByRole("button", { name: "Definitions & alternatives" });
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  expect(screen.queryByText("Lead time (first commit \u2192 prod)")).not.toBeInTheDocument();
  expect(screen.queryByRole("table")).not.toBeInTheDocument();

  await user.click(toggle);

  expect(toggle).toHaveAttribute("aria-expanded", "true");
  expect(screen.getByText("Lead time (first commit \u2192 prod)")).toBeDefined();
  expect(screen.getByText("23.0h")).toBeDefined();
  const perPR = screen.getByText("Change failure rate (per PR)").parentElement;
  expect(perPR?.textContent).toBe(
    "Change failure rate (per PR)n/aNo failure source yet: flags arrive with flag confirmation. Incidents: no source yet."
  );
  const table = screen.getByRole("table");
  const rows = within(table).getAllByRole("row");
  // A header row, then every one of the window's 29 days.
  expect(rows).toHaveLength(30);
  expect(rows[1]?.textContent?.startsWith("2026-09-27 (partial)")).toBe(true);
  expect(rows[29]?.textContent?.startsWith("2026-08-30")).toBe(true);
  const sept5 = rows.find((row) => row.textContent?.startsWith("2026-09-05"));
  expect(Array.from(sept5?.querySelectorAll("td") ?? [], (cell) => cell.textContent)).toEqual([
    "2026-09-05",
    "1",
    "1",
    "1",
    "100.0%",
    "0",
  ]);
});

test("the per-day table pads its cells 2 px above and below, in a box with a rounded border", async () => {
  const user = userEvent.setup();
  renderPanel(fixtureMeasures);
  await user.click(screen.getByRole("button", { name: "Definitions & alternatives" }));
  const table = screen.getByRole("table", { name: "Per day (UTC), newest first" });
  const cells = Array.from(table.querySelectorAll("tbody td"));
  // Every one of the window's 29 days, six cells each.
  expect(cells.length).toBe(29 * 6);
  // `py-0.5` is 0.125rem: a row is the 16 px line, 2 px above and below it, and its top border.
  const unpadded = cells.filter((cell) => !cell.classList.contains("py-0.5"));
  expect(unpadded.map((cell) => `${cell.textContent}: ${cell.className}`)).toEqual([]);
  const box = table.closest('[data-control="table-shell"]');
  expect(box?.className.split(" ")).toEqual(
    expect.arrayContaining(["rounded", "border", ...borderDefault.split(" ")])
  );
});

test("a pending read shows the six cards' frame with an ellipsis for each number", () => {
  renderPanel(undefined);
  const grid = screen.getByLabelText("KPI targets");
  const cards = grid.querySelectorAll("[data-kpi]");
  expect(cards).toHaveLength(6);
  for (const kpi of cards) {
    expect(within(kpi as HTMLElement).getAllByText("\u2026").length).toBeGreaterThan(0);
  }
});
