import { afterEach, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import {
  errorFrame,
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import type {
  DeliveryLeadTimeTiming,
  DeliveryPipelineResponse,
  DeliveryTiming,
  DeliveryTimingCell,
  LiveSubscribe,
} from "../../api/types";
import { chartBar, dangerText, successText } from "../../theme/classes";
import { PipelinePage, PipelineView } from "./PipelinePage";

let sockets: FakeLiveSockets;
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  cleanup();
  sockets.restore();
});

// The prototype's web/src/lib/pipeline.fixture.ts, its cells as the server answers them, with the
// deployment's job names replaced by placeholders: seven days, Sep 26 to Oct 2, today Oct 2.
const DAYS = [
  "2026-09-26",
  "2026-09-27",
  "2026-09-28",
  "2026-09-29",
  "2026-09-30",
  "2026-10-01",
  "2026-10-02",
];

const EMPTY: DeliveryTimingCell = {
  n: 0,
  median_minutes: null,
  p90_minutes: null,
  success: 0,
  failure: 0,
  cancelled: 0,
  success_rate: null,
};

function timing(
  cells: Record<string, Partial<DeliveryTimingCell>>,
  total: Partial<DeliveryTimingCell> = {}
): DeliveryTiming {
  return {
    days: DAYS.map((day, index) => ({
      day,
      partial: index === DAYS.length - 1,
      ...EMPTY,
      ...cells[day],
    })),
    total: { ...EMPTY, ...total },
  };
}

function leadTimes(
  today: { n: number; median: number; p90: number; max: number } | null,
  total: { n: number; median: number; p90: number; max: number } | null
): DeliveryLeadTimeTiming {
  const cell = (v: typeof today) =>
    v === null
      ? { n: 0, median_minutes: null, p90_minutes: null, max_minutes: null }
      : { n: v.n, median_minutes: v.median, p90_minutes: v.p90, max_minutes: v.max };
  return {
    days: DAYS.map((day, index) => ({
      day,
      partial: index === DAYS.length - 1,
      ...(index === DAYS.length - 1 ? cell(today) : cell(null)),
    })),
    total: cell(total),
  };
}

const RUN_URL = "https://github.com/acme/widgets/actions/runs/37000000001";

type Targets = DeliveryPipelineResponse["targets"];

/** The six targets as the catalogue's evals hold them, which the answer states until a person
 *  changes one: each with its eval's direction and the statistics its verdict judges. */
const TARGETS: { [K in keyof Targets]: NonNullable<Targets[K]> } = {
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

const PIPELINE: DeliveryPipelineResponse = {
  days: DAYS,
  computed_at: "2026-10-02T12:00:00Z",
  pr_checks: {
    run: timing(
      {
        "2026-10-01": {
          n: 300,
          median_minutes: 24.2,
          p90_minutes: 97.1,
          success: 260,
          failure: 40,
          cancelled: 410,
          success_rate: 0.8667,
        },
        "2026-10-02": {
          n: 120,
          median_minutes: 21.5,
          p90_minutes: 93.4,
          success: 102,
          failure: 18,
          cancelled: 150,
          success_rate: 0.85,
        },
      },
      {
        n: 1800,
        median_minutes: 22.8,
        p90_minutes: 95,
        success: 1550,
        failure: 250,
        cancelled: 2400,
        success_rate: 0.8611,
      }
    ),
    slowest_min_results: 50,
    slowest_jobs: [
      {
        name: "build / unit",
        ...timing(
          {
            "2026-10-02": {
              n: 40,
              median_minutes: 92.6,
              p90_minutes: 110.2,
              success: 38,
              failure: 2,
              success_rate: 0.95,
            },
          },
          { n: 400, median_minutes: 93.1, p90_minutes: 112, success: 380, failure: 20 }
        ),
      },
      {
        name: "infra / plan",
        ...timing({}, { n: 900, median_minutes: 17.3, p90_minutes: 21, success: 890, failure: 10 }),
      },
    ],
    failing_jobs: [
      {
        name: "build / unit",
        failures: 6,
        results: 48,
        last_failed_at: "2026-10-02T11:40:00Z",
        last_run_url: RUN_URL,
      },
    ],
  },
  staging_e2e: {
    job: "staging-e2e / run",
    ...timing(
      {
        "2026-10-02": {
          n: 9,
          median_minutes: 41.3,
          p90_minutes: 58.9,
          success: 6,
          failure: 3,
          cancelled: 2,
          success_rate: 0.6667,
        },
      },
      {
        n: 90,
        median_minutes: 40.2,
        p90_minutes: 61,
        success: 63,
        failure: 27,
        cancelled: 15,
        success_rate: 0.7,
      }
    ),
  },
  staging_apply: {
    job: "staging-release / apply",
    ...timing(
      {
        "2026-10-02": {
          n: 12,
          median_minutes: 7.4,
          p90_minutes: 9.8,
          success: 12,
          success_rate: 1,
        },
      },
      { n: 120, median_minutes: 7.6, p90_minutes: 10.5, success: 118, failure: 2 }
    ),
  },
  production_apply: {
    job: "widgets-release / widgets-release",
    ...timing(
      {},
      {
        n: 60,
        median_minutes: 9.1,
        p90_minutes: 12.4,
        success: 59,
        failure: 1,
        success_rate: 0.9833,
      }
    ),
  },
  // The prototype's two PRs: deployed 38 and 52 minutes after merging, one today.
  merge_to_production: leadTimes(
    { n: 1, median: 38, p90: 38, max: 38 },
    { n: 2, median: 45, p90: 50.6, max: 52 }
  ),
  opened_to_merge: leadTimes(
    { n: 1, median: 59.9, p90: 59.9, max: 59.9 },
    { n: 2, median: 59.95, p90: 59.99, max: 60 }
  ),
  targets: TARGETS,
  // The server's verdicts of the two cards: the longest change, 52m, misses "under 45m"; the
  // median from open to merge, 59.95m, meets "under 60m".
  status: { merge_to_production: false, opened_to_merge: true },
  freshness: {
    last_event_at: "2026-10-02T11:59:00Z",
    last_reconcile_at: "2026-10-02T11:58:00Z",
    last_error: null,
    unfetchable_count: 0,
  },
};

function view(pipeline: DeliveryPipelineResponse = PIPELINE): HTMLElement {
  const at = Date.parse("2026-10-02T12:00:00Z");
  // The freshness row reads the live connection's state from the query client.
  render(
    <QueryClientProvider client={new QueryClient()}>
      <PipelineView nowMs={at} pipeline={pipeline} readAtMs={at} />
    </QueryClientProvider>
  );
  return document.body;
}

function card(metric: string): HTMLElement {
  const element = document.querySelector<HTMLElement>(`[data-metric="${metric}"]`);
  if (element === null) throw new Error(`no ${metric} card`);
  return element;
}

test("each card shows today's and the seven days' median and p90 with the success share", () => {
  view();
  const ci = card("pr_ci").innerHTML;
  expect(ci).toContain("PR CI");
  expect(ci).toContain(">22m<"); // today's median, 21.5
  expect(ci).toContain("p90 93m · 120 runs");
  expect(ci).toContain(">23m<"); // the week's median, 22.8
  expect(ci).toContain("p90 95m · 1800 runs");
  expect(ci).toContain(">85%<");
  expect(ci).toContain("(102 of 120)");
  expect(ci).toContain("2400 cancelled in 7 days, not counted");

  const e2e = card("staging_e2e").innerHTML;
  expect(e2e).toContain("staging-e2e / run");
  expect(e2e).toContain("p90 59m · 9 runs");
  expect(e2e).toContain(">67%<");
  expect(e2e).toContain("(6 of 9)");
});

test("a stage with nothing timed today says n/a rather than zero", () => {
  view();
  const production = card("production_apply").innerHTML;
  expect(production).toContain(">n/a<");
  expect(production).toContain("p90 n/a · 0 runs");
  expect(production).toContain("p90 12m · 60 runs");
});

test("a pipeline with no staging end-to-end gate omits that card and keeps the other five", () => {
  view({ ...PIPELINE, staging_e2e: null });
  expect(document.querySelector('[data-metric="staging_e2e"]')).toBeNull();
  for (const metric of [
    "pr_ci",
    "staging_apply",
    "production_apply",
    "merge_to_production",
    "opened_to_merge",
  ]) {
    expect(document.querySelector(`[data-metric="${metric}"]`)).not.toBeNull();
  }
});

/** What a lead-time card says of each statistic its target judges, one line a judge. */
function judged(metric: string): (string | null)[] {
  return [...card(metric).querySelectorAll("[data-judge]")].map((line) => line.textContent);
}

test("merge to production shows the response's verdict and its target as its eval holds it", () => {
  view();
  const merge = card("merge_to_production");
  expect(merge.querySelector("[data-target-met]")?.getAttribute("data-target-met")).toBe("false");
  expect(merge.innerHTML).toContain("target missed");
  expect(
    merge.querySelector('[data-span="day"][data-statistic="minutes.p90"]')?.parentElement
      ?.textContent
  ).toBe("p90 38m · 1 PR");
  // The catalogue judges the longest change and the median, and the longest change under the
  // target puts the median under it: the card names and shows the longest change alone, as it
  // did before the target carried its judges.
  expect(merge.innerHTML).toContain("target under 45m for every change (the dashed line)");
  expect(judged("merge_to_production")).toEqual(["Longest today 38m, 7 days 52m"]);
  expect(card("opened_to_merge").innerHTML).toContain("target median under 60m (the dashed line)");
  // Today's median, 59.9m, and the seven days', 59.95m, meet "under 60m": whole minutes would
  // print the target itself beside their green, so they take the digits that do not (R3-C1).
  expect(judged("opened_to_merge")).toEqual(["Median today 59.9m, 7 days 59.95m"]);
  for (const metric of ["pr_ci", "staging_e2e", "staging_apply", "production_apply"]) {
    expect(card(metric).innerHTML).not.toContain("target");
  }
  cleanup();

  // The verdict is the server's, its eval's own (a person may have moved the figures it judges),
  // and never the page's: the seven days' longest change, 52m, is over "under 45m", which a page
  // judging the card itself would call missed, and the server's met is what the card says.
  view({ ...PIPELINE, status: { ...PIPELINE.status, merge_to_production: true } });
  const met = card("merge_to_production");
  expect(met.querySelector("[data-target-met]")?.getAttribute("data-target-met")).toBe("true");
  expect(met.innerHTML).toContain("target met");
  cleanup();

  view({
    ...PIPELINE,
    targets: {
      ...TARGETS,
      merge_to_production_minutes: {
        ...TARGETS.merge_to_production_minutes,
        value: 52,
        inclusive: true,
      },
    },
    status: { ...PIPELINE.status, merge_to_production: true },
  });
  expect(card("merge_to_production").innerHTML).toContain("target at most 52m for every change");
  cleanup();

  view({
    ...PIPELINE,
    targets: { ...TARGETS, merge_to_production_minutes: null },
    status: { ...PIPELINE.status, merge_to_production: null },
  });
  const off = card("merge_to_production");
  expect(off.querySelector("[data-target-met]")?.getAttribute("data-target-met")).toBe("none");
  expect(off.innerHTML).toContain("target off");
  // An off target judges nothing, so the card takes its verdict away and keeps the figure it
  // showed before targets carried their judges: merge to production's longest change.
  expect(judged("merge_to_production")).toEqual(["Longest today 38m, 7 days 52m"]);
});

// R5: a person may judge a lead-time eval by any statistic the Pipeline states, and the verdict
// follows the judges, so the card names and shows the statistic its badge is of, in the target's
// own direction, rather than the one the catalogue judges.
test("a lead-time card names and shows the statistics its target judges, whichever they are", () => {
  view({
    ...PIPELINE,
    targets: {
      ...TARGETS,
      opened_to_merge_median_minutes: {
        value: 720,
        inclusive: true,
        direction: "lower_better",
        judges: ["minutes.p90"],
      },
    },
  });
  const opened = card("opened_to_merge");
  expect(opened.innerHTML).toContain("target p90 at most 720m (the dashed line)");
  expect(opened.innerHTML).not.toContain("target median");
  expect(judged("opened_to_merge")).toEqual(["P90 today 60m, 7 days 60m"]);
  cleanup();

  // Judged by its count, better higher: a number of pull requests, which the chart's minutes
  // draw no line for.
  view({
    ...PIPELINE,
    targets: {
      ...TARGETS,
      opened_to_merge_median_minutes: {
        value: 2,
        inclusive: true,
        direction: "higher_better",
        judges: ["minutes.n"],
      },
    },
    status: { ...PIPELINE.status, opened_to_merge: true },
  });
  expect(card("opened_to_merge").innerHTML).toContain("target count at least 2<");
  expect(judged("opened_to_merge")).toEqual(["Count today 1, 7 days 2"]);
  expect(targetLines("opened_to_merge")).toBe(0);
  // A count judges the seven days' total, which no day is a share of: today's bars draw its
  // minutes, which the target does not judge, and read neutral.
  expect(todaysBars("opened_to_merge").map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.neutral,
  ]);
  cleanup();

  // The longest change and the median judged better higher: neither implies the other, so the
  // card names both and shows a line of each, where better lower it shows the longest alone.
  view({
    ...PIPELINE,
    targets: {
      ...TARGETS,
      merge_to_production_minutes: {
        ...TARGETS.merge_to_production_minutes,
        direction: "higher_better",
      },
    },
  });
  expect(card("merge_to_production").innerHTML).toContain(
    "target longest and median above 45m (the dashed line)"
  );
  expect(judged("merge_to_production")).toEqual([
    "Longest today 38m, 7 days 52m",
    "Median today 38m, 7 days 45m",
  ]);
});

test("PR open to merge shows the response's verdict of its median", () => {
  view();
  const opened = card("opened_to_merge");
  expect(opened.innerHTML).toContain("PR open to merge");
  expect(opened.querySelector("[data-target-met]")?.getAttribute("data-target-met")).toBe("true");
  expect(opened.innerHTML).toContain("target met");
  expect(opened.innerHTML).toContain("target median under 60m");
  cleanup();

  // The seven days' median, 59.95m, is under 60m, which a page judging the card itself would call
  // met; the server's verdict is missed, and the card says that.
  view({ ...PIPELINE, status: { ...PIPELINE.status, opened_to_merge: false } });
  const missed = card("opened_to_merge");
  expect(missed.querySelector("[data-target-met]")?.getAttribute("data-target-met")).toBe("false");
  expect(missed.innerHTML).toContain("target missed");
  cleanup();

  view({
    ...PIPELINE,
    opened_to_merge: leadTimes(null, null),
    status: { ...PIPELINE.status, opened_to_merge: null },
  });
  const none = card("opened_to_merge");
  expect(none.querySelector("[data-target-met]")?.getAttribute("data-target-met")).toBe("none");
  expect(none.innerHTML).toContain("no data");
});

/** The drawn bars of a lead-time card's last day, today: its p90 and its median, behind the
 *  transparent hover target the day carries. */
function todaysBars(metric: string): Element[] {
  const days = card(metric).querySelectorAll('svg[aria-label="per-day median and p90"] g');
  return [...(days[days.length - 1]?.querySelectorAll("rect") ?? [])].filter(
    (rect) => rect.getAttribute("fill") !== "transparent"
  );
}

/** How many dashed target lines a lead-time card's chart draws. */
function targetLines(metric: string): number {
  return card(metric).querySelectorAll('svg[aria-label="per-day median and p90"] line').length;
}

/** Each figure of a lead-time card a target may tone, keyed by where it stands and what it shows:
 *  its Today and Last 7 days figures as `<span> <statistic>`, and each judged line's two as
 *  `line <span> <statistic>`; with its tone, met, missed or plain, and what it writes. */
function figures(metric: string): [string, { tone: string; text: string }][] {
  return Array.from(card(metric).querySelectorAll("span[data-statistic]"), (figure) => {
    const className = figure.getAttribute("class") ?? "";
    const tone = className.includes(successText)
      ? "met"
      : className.includes(dangerText)
        ? "missed"
        : "plain";
    const line = figure.closest("[data-judge]") === null ? "" : "line ";
    return [
      `${line}${figure.getAttribute("data-span")} ${figure.getAttribute("data-statistic")}`,
      { tone, text: figure.textContent ?? "" },
    ];
  });
}

/** The tone of each figure of a lead-time card a target may tone (`figures`). */
function tones(metric: string): Record<string, string> {
  return Object.fromEntries(figures(metric).map(([key, { tone }]) => [key, tone]));
}

/** What each figure of a lead-time card a target may tone writes, and its tone (`figures`):
 *  "44.6m met". */
function written(metric: string): Record<string, string> {
  return Object.fromEntries(
    figures(metric).map(([key, { tone, text }]) => [key, `${text} ${tone}`])
  );
}

/** Today's and the seven days' lead times of a card's every figure: 3 pull requests, a median of
 *  30m, a p90 of 40m and a longest change of 52m, which misses under 45m where the rest meet. */
const LONG_TAIL = { n: 3, median: 30, p90: 40, max: 52 };

test("a lead-time bar is judged by its eval's rule, so a median at an inclusive target meets it", () => {
  const atSixty = leadTimes(
    { n: 1, median: 60, p90: 60, max: 60 },
    { n: 1, median: 60, p90: 60, max: 60 }
  );
  view({
    ...PIPELINE,
    opened_to_merge: atSixty,
    targets: {
      ...TARGETS,
      opened_to_merge_median_minutes: {
        ...TARGETS.opened_to_merge_median_minutes,
        inclusive: true,
      },
    },
    status: { ...PIPELINE.status, opened_to_merge: true },
  });
  // The catalogue judges PR open to merge by its median: the p90 bar, which no judge names, reads
  // neutral.
  expect(todaysBars("opened_to_merge").map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.met,
  ]);
  cleanup();

  // The same day at a strict target of the same value misses it.
  view({
    ...PIPELINE,
    opened_to_merge: atSixty,
    status: { ...PIPELINE.status, opened_to_merge: false },
  });
  expect(todaysBars("opened_to_merge").map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.missed,
  ]);
});

test("a lead-time bar is toned by its own day's value of the statistic it draws: the median bar by the day's median, and the p90 bar, which no judge names, neutral", () => {
  // Today's median and p90 are under 45m and its longest change is over. Judged by the longest
  // change and the median, the median bar takes the median's verdict, met; no bar draws the
  // longest change, and no judge names the p90.
  const longTail = leadTimes(LONG_TAIL, LONG_TAIL);
  view({ ...PIPELINE, merge_to_production: longTail });
  expect(todaysBars("merge_to_production").map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.met,
  ]);
  cleanup();

  // Judged by its median alone, the median bar meets, and under the other direction it misses.
  const byMedian: NonNullable<Targets["merge_to_production_minutes"]> = {
    ...TARGETS.merge_to_production_minutes,
    judges: ["minutes.median"],
  };
  view({
    ...PIPELINE,
    merge_to_production: longTail,
    targets: { ...TARGETS, merge_to_production_minutes: byMedian },
  });
  expect(todaysBars("merge_to_production").map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.met,
  ]);
  expect(card("merge_to_production").innerHTML).toContain("target median under 45m");
  cleanup();

  view({
    ...PIPELINE,
    merge_to_production: longTail,
    targets: {
      ...TARGETS,
      merge_to_production_minutes: { ...byMedian, direction: "higher_better" },
    },
  });
  expect(todaysBars("merge_to_production").map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.missed,
  ]);
  expect(card("merge_to_production").innerHTML).toContain("target median above 45m");
});

// R5, the architect's ruling of 2026-11-19 on the tester's round-11 finding: a bar is a figure
// too, so it takes a tone only where the target judges the statistic it draws. Judged by its
// count of pull requests, merge to production's median and p90 bars draw statistics the target
// is not of: each reads neutral, with no target line. At 06b73b46 a day with a 25m median and 1
// pull request draws red bars and a day with a 300m median and 4 draws green ones
// (dispatch://INFRA-2916, the testing round of 2026-11-19 at 06b73b46).
test("a lead-time bar is toned only where the target judges the statistic it draws: merge to production judged by its count draws neutral bars", () => {
  const byCount: NonNullable<Targets["merge_to_production_minutes"]> = {
    value: 2,
    inclusive: true,
    direction: "higher_better",
    judges: ["minutes.n"],
  };
  for (const today of [
    { n: 1, median: 25, p90: 30, max: 30 },
    { n: 4, median: 300, p90: 500, max: 540 },
  ]) {
    view({
      ...PIPELINE,
      merge_to_production: leadTimes(today, today),
      targets: { ...TARGETS, merge_to_production_minutes: byCount },
      status: { ...PIPELINE.status, merge_to_production: today.n >= 2 },
    });
    expect(todaysBars("merge_to_production").map((bar) => bar.getAttribute("class"))).toEqual([
      chartBar.neutralFaint,
      chartBar.neutral,
    ]);
    expect(
      card("merge_to_production").querySelectorAll('svg[aria-label="per-day median and p90"] line')
        .length
    ).toBe(0);
    cleanup();
  }
});

// R5's tone rule (dispatch://INFRA-2916, check k31, the architect's of 2026-11-19), for every
// element of a lead-time card that can take a met or missed tone: the badge, each of the Today and
// Last 7 days figures, each judged line's two values, each bar and the dashed target line. The
// tests below state its clauses, each with values where an element's own value and the card's
// badge disagree, so an element toned by the badge, or by a day's verdict over every judge, reads
// wrong.

// R5 (1): an eval with no target, which a person turned off, judges nothing.
test("a lead-time card whose target is off tones nothing: no figure in colour, neutral bars and no target line", () => {
  view({
    ...PIPELINE,
    merge_to_production: leadTimes(LONG_TAIL, LONG_TAIL),
    opened_to_merge: leadTimes(LONG_TAIL, LONG_TAIL),
    targets: {
      ...TARGETS,
      merge_to_production_minutes: null,
      opened_to_merge_median_minutes: null,
    },
    status: { merge_to_production: null, opened_to_merge: null },
  });
  for (const [metric, judge, slug] of [
    ["merge_to_production", "minutes.max", "merge-to-production"],
    ["opened_to_merge", "minutes.median", "pr-open-to-merge"],
  ] as const) {
    expect(tones(metric)).toEqual({
      "day minutes.median": "plain",
      "day minutes.p90": "plain",
      "day minutes.n": "plain",
      "window minutes.median": "plain",
      "window minutes.p90": "plain",
      "window minutes.n": "plain",
      [`line day ${judge}`]: "plain",
      [`line window ${judge}`]: "plain",
    });
    expect(todaysBars(metric).map((bar) => bar.getAttribute("class"))).toEqual([
      chartBar.neutralFaint,
      chartBar.neutral,
    ]);
    expect(targetLines(metric)).toBe(0);
    // Every element a target may tone names the eval whose target it is.
    expect([
      ...new Set(
        Array.from(card(metric).querySelectorAll("[data-statistic]"), (element) =>
          element.getAttribute("data-eval")
        )
      ),
    ]).toEqual([slug]);
  }
  // The cards no eval judges name no statistic.
  for (const metric of ["pr_ci", "staging_e2e", "staging_apply", "production_apply"]) {
    expect(card(metric).querySelectorAll("[data-statistic]")).toHaveLength(0);
  }
});

// R5 (2) and (3): the badge is the server's verdict of the window (`status`); every other figure
// is toned by its own value, so none takes the badge's tone where its own value says otherwise.
test("a lead-time card's badge is the server's verdict of the window and no figure takes it: each figure is toned by its own value where the two disagree", () => {
  view({ ...PIPELINE, status: { merge_to_production: true, opened_to_merge: false } });
  expect(
    ["merge_to_production", "opened_to_merge"].map((metric) =>
      card(metric).querySelector("[data-target-met]")?.getAttribute("data-target-met")
    )
  ).toEqual(["true", "false"]);
  // Merge to production under 45m, judged by its longest change and its median: today's 38m meets
  // and the seven days' median, 45m, and longest change, 52m, miss, beside a met badge.
  expect(tones("merge_to_production")).toEqual({
    "day minutes.median": "met",
    "day minutes.p90": "plain",
    "day minutes.n": "plain",
    "window minutes.median": "missed",
    "window minutes.p90": "plain",
    "window minutes.n": "plain",
    "line day minutes.max": "met",
    "line window minutes.max": "missed",
  });
  // PR open to merge under 60m, judged by its median: today's 59.9m and the seven days' 59.95m
  // meet, beside a missed badge.
  expect(tones("opened_to_merge")).toEqual({
    "day minutes.median": "met",
    "day minutes.p90": "plain",
    "day minutes.n": "plain",
    "window minutes.median": "met",
    "window minutes.p90": "plain",
    "window minutes.n": "plain",
    "line day minutes.median": "met",
    "line window minutes.median": "met",
  });
});

// R5 (4), one judge on a count of the window: the seven days' count takes its own verdict, and
// no day of it takes a tone, today's count and bars included, so no line is drawn.
test("merge to production judged by its count of pull requests tones the seven days' count alone, in its seeded direction and the other: neutral bars, no target line", () => {
  // Two pull requests in the seven days: at most 1, lower_better as the catalogue seeds merge to
  // production, misses; at least 1 meets. Each beside the other badge.
  for (const [direction, verdict] of [
    ["lower_better", "missed"],
    ["higher_better", "met"],
  ] as const) {
    view({
      ...PIPELINE,
      targets: {
        ...TARGETS,
        merge_to_production_minutes: {
          value: 1,
          inclusive: true,
          direction,
          judges: ["minutes.n"],
        },
      },
      status: { ...PIPELINE.status, merge_to_production: verdict === "missed" },
    });
    expect(judged("merge_to_production")).toEqual(["Count today 1, 7 days 2"]);
    expect(tones("merge_to_production")).toEqual({
      "day minutes.median": "plain",
      "day minutes.p90": "plain",
      "day minutes.n": "plain",
      "window minutes.median": "plain",
      "window minutes.p90": "plain",
      "window minutes.n": verdict,
      "line day minutes.n": "plain",
      "line window minutes.n": verdict,
    });
    expect(todaysBars("merge_to_production").map((bar) => bar.getAttribute("class"))).toEqual([
      chartBar.neutralFaint,
      chartBar.neutral,
    ]);
    expect(targetLines("merge_to_production")).toBe(0);
    expect(card("merge_to_production").innerHTML).not.toContain("(the dashed line)");
    cleanup();
  }
});

// R5 (4): a count and a quantile judged together: the count's seven-day figures take its verdict,
// no day of it does, and the median's figures and bars take the median's own.
test("merge to production judged by its count and its median tones each by its own value, and no day by the count", () => {
  const busy = { n: 50, median: 30, p90: 40, max: 52 };
  view({
    ...PIPELINE,
    merge_to_production: leadTimes(busy, busy),
    targets: {
      ...TARGETS,
      merge_to_production_minutes: {
        ...TARGETS.merge_to_production_minutes,
        judges: ["minutes.n", "minutes.median"],
      },
    },
    status: { ...PIPELINE.status, merge_to_production: true },
  });
  // Under 45: 50 pull requests miss, which tones no day; the median, 30m, meets.
  expect(tones("merge_to_production")).toEqual({
    "day minutes.median": "met",
    "day minutes.p90": "plain",
    "day minutes.n": "plain",
    "window minutes.median": "met",
    "window minutes.p90": "plain",
    "window minutes.n": "missed",
    "line day minutes.n": "plain",
    "line window minutes.n": "missed",
    "line day minutes.median": "met",
    "line window minutes.median": "met",
  });
  expect(todaysBars("merge_to_production").map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.met,
  ]);
  expect(targetLines("merge_to_production")).toBe(1);
});

// R5 (3), two judges on one card, one met and one missed: each figure, each judged line's values
// and each bar by its own judge, and those of a statistic neither names by nothing.
test("merge to production judged by its longest change and its median tones each by its own value, in its seeded direction and the other", () => {
  // Under 45m, lower_better as the catalogue seeds it: the median, 30m, meets, the longest change,
  // 52m, misses, and no judge names the p90. Above 45m the other way round, and neither implies
  // the other, so the card shows a line of each.
  for (const [direction, badge, expected, bars] of [
    [
      "lower_better",
      true,
      {
        "day minutes.median": "met",
        "window minutes.median": "met",
        "line day minutes.max": "missed",
        "line window minutes.max": "missed",
      },
      [chartBar.neutralFaint, chartBar.met],
    ],
    [
      "higher_better",
      false,
      {
        "day minutes.median": "missed",
        "window minutes.median": "missed",
        "line day minutes.max": "met",
        "line window minutes.max": "met",
        "line day minutes.median": "missed",
        "line window minutes.median": "missed",
      },
      [chartBar.neutralFaint, chartBar.missed],
    ],
  ] as const) {
    view({
      ...PIPELINE,
      merge_to_production: leadTimes(LONG_TAIL, LONG_TAIL),
      targets: {
        ...TARGETS,
        merge_to_production_minutes: { ...TARGETS.merge_to_production_minutes, direction },
      },
      status: { ...PIPELINE.status, merge_to_production: badge },
    });
    expect(tones("merge_to_production")).toEqual({
      "day minutes.p90": "plain",
      "day minutes.n": "plain",
      "window minutes.p90": "plain",
      "window minutes.n": "plain",
      ...expected,
    });
    expect(todaysBars("merge_to_production").map((bar) => bar.getAttribute("class"))).toEqual([
      ...bars,
    ]);
    expect(targetLines("merge_to_production")).toBe(1);
    cleanup();
  }
});

test("PR open to merge judged by its median and its p90 tones each by its own value: the median met, the p90 missed, and a day's two bars apart", () => {
  const spread = { n: 3, median: 30, p90: 50, max: 52 };
  view({
    ...PIPELINE,
    opened_to_merge: leadTimes(spread, spread),
    targets: {
      ...TARGETS,
      opened_to_merge_median_minutes: {
        value: 45,
        inclusive: false,
        direction: "lower_better",
        judges: ["minutes.median", "minutes.p90"],
      },
    },
    status: { ...PIPELINE.status, opened_to_merge: true },
  });
  // Under 45m the median, 30m, meets and the p90, 50m, misses beside a met badge.
  expect(judged("opened_to_merge")).toEqual([
    "Median today 30m, 7 days 30m",
    "P90 today 50m, 7 days 50m",
  ]);
  expect(tones("opened_to_merge")).toEqual({
    "day minutes.median": "met",
    "day minutes.p90": "missed",
    "day minutes.n": "plain",
    "window minutes.median": "met",
    "window minutes.p90": "missed",
    "window minutes.n": "plain",
    "line day minutes.median": "met",
    "line window minutes.median": "met",
    "line day minutes.p90": "missed",
    "line window minutes.p90": "missed",
  });
  expect(todaysBars("opened_to_merge").map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.missedFaint,
    chartBar.met,
  ]);
  expect(targetLines("opened_to_merge")).toBe(1);
});

// R5 (5): a chart draws its dashed target line exactly where its bars take a tone: where the
// target judges the median or the p90, whether or not a day has a value.
test("a lead-time chart draws its target line exactly where its bars take a tone: where the target judges its median or its p90", () => {
  const cases: [
    NonNullable<Targets["merge_to_production_minutes"]>["judges"],
    (string | null)[],
    boolean,
  ][] = [
    [["minutes.max"], [chartBar.neutralFaint, chartBar.neutral], false],
    [["minutes.n"], [chartBar.neutralFaint, chartBar.neutral], false],
    [["minutes.max", "minutes.median"], [chartBar.neutralFaint, chartBar.met], true],
    [["minutes.p90"], [chartBar.metFaint, chartBar.neutral], true],
  ];
  for (const [judges, bars, drawn] of cases) {
    view({
      ...PIPELINE,
      merge_to_production: leadTimes(LONG_TAIL, LONG_TAIL),
      targets: {
        ...TARGETS,
        merge_to_production_minutes: { ...TARGETS.merge_to_production_minutes, judges },
      },
    });
    expect([
      judges,
      todaysBars("merge_to_production").map((bar) => bar.getAttribute("class")),
      targetLines("merge_to_production"),
      card("merge_to_production").innerHTML.includes("(the dashed line)"),
    ]).toEqual([judges, bars, drawn ? 1 : 0, drawn]);
    cleanup();
  }

  // Seven days with no merged pull request: no bar has a value to tone, and the line is drawn all
  // the same, since the catalogue's target judges the median the bars draw.
  view({ ...PIPELINE, merge_to_production: leadTimes(null, null) });
  expect(targetLines("merge_to_production")).toBe(1);
});

// R3-C1, the architect's rule of 2026-11-19 for review round 3: a toned figure's printed text lies
// on the same side of its target as its value. A lead time that rounds to the target itself, "45m"
// in green beside "under 45m for every change", is written with more digits until it does not.
test("a toned lead-time figure is written on its value's side of the target: more digits where whole minutes would print the target itself", () => {
  view({
    ...PIPELINE,
    merge_to_production: leadTimes(
      { n: 1, median: 44.6, p90: 44.6, max: 44.6 },
      { n: 2, median: 44.6, p90: 44.8, max: 44.9 }
    ),
  });
  // Under 45m for every change: today's 44.6m and the seven days' median, 44.6m, and longest
  // change, 44.9m, meet, where whole minutes would print 45m. No judge names the p90, which keeps
  // its whole minutes, nor the count.
  expect(judged("merge_to_production")).toEqual(["Longest today 44.6m, 7 days 44.9m"]);
  expect(written("merge_to_production")).toEqual({
    "day minutes.median": "44.6m met",
    "day minutes.p90": "45m plain",
    "day minutes.n": "1 plain",
    "window minutes.median": "44.6m met",
    "window minutes.p90": "45m plain",
    "window minutes.n": "2 plain",
    "line day minutes.max": "44.6m met",
    "line window minutes.max": "44.9m met",
  });
  // PR open to merge under 60m, judged by its median: today's 59.9m and the seven days' 59.95m
  // meet, where whole minutes would print 60m, and one decimal 60.0m.
  expect(judged("opened_to_merge")).toEqual(["Median today 59.9m, 7 days 59.95m"]);
  expect(written("opened_to_merge")).toMatchObject({
    "day minutes.median": "59.9m met",
    "window minutes.median": "59.95m met",
  });
});

// R3-C1: the digits are added only where whole minutes would print the figure on the other side of
// its target; a lead time whose whole minutes print it on its value's side keeps them, one at the
// target itself included.
test("a toned lead-time figure whose whole minutes print it on its value's side of the target keeps them", () => {
  view();
  // Under 45m for every change: today's 38m meets; the seven days' median, 45m, sits on the
  // target and misses, as its value does; the longest change, 52m, misses.
  expect(judged("merge_to_production")).toEqual(["Longest today 38m, 7 days 52m"]);
  expect(written("merge_to_production")).toEqual({
    "day minutes.median": "38m met",
    "day minutes.p90": "38m plain",
    "day minutes.n": "1 plain",
    "window minutes.median": "45m missed",
    "window minutes.p90": "51m plain",
    "window minutes.n": "2 plain",
    "line day minutes.max": "38m met",
    "line window minutes.max": "52m missed",
  });
});

test("lists the slowest jobs in the server's order and the jobs that failed in the last day", () => {
  view();
  const slowest = document.querySelector<HTMLElement>('[data-section="slowest-jobs"]');
  const rows = [...(slowest?.querySelectorAll("[data-job]") ?? [])].map((row) =>
    row.getAttribute("data-job")
  );
  expect(rows).toEqual(["build / unit", "infra / plan"]);
  const unit = slowest?.querySelector('[data-job="build / unit"]');
  expect(unit?.innerHTML).toContain(">93m<"); // today's median 92.6 and the week's 93.1

  const failing = document.querySelector<HTMLElement>('[data-section="failing-jobs"]');
  const row = failing?.querySelector('[data-failing-job="build / unit"]');
  expect(row?.innerHTML).toContain(">6<");
  expect(row?.innerHTML).toContain(">13%<"); // 6 of 48
  const link = row?.querySelector("a");
  expect(link?.getAttribute("href")).toBe(RUN_URL);
  expect(link?.textContent).toBe("20 min ago");
});

test("says when no job has enough results and when no job failed", () => {
  view({ ...PIPELINE, pr_checks: { ...PIPELINE.pr_checks, failing_jobs: [], slowest_jobs: [] } });
  expect(document.body).toHaveTextContent("No job has 50 timed results yet.");
  expect(document.body).toHaveTextContent("No job failed in the last 24 hours.");
});

test("the slowest jobs say the threshold the server ranked by, not 50 alone", () => {
  view({
    ...PIPELINE,
    pr_checks: { ...PIPELINE.pr_checks, slowest_jobs: [], slowest_min_results: 120 },
  });
  const slowest = document.querySelector<HTMLElement>('[data-section="slowest-jobs"]');
  expect(slowest).toHaveTextContent("a job needs 120 timed results in the 7 days to be ranked.");
  expect(slowest).toHaveTextContent("No job has 120 timed results yet.");
  expect(slowest?.textContent).not.toContain("50 timed");
  cleanup();

  // A min_n of 0 still ranks only a job with a median, so the page says one result is needed.
  view({
    ...PIPELINE,
    pr_checks: { ...PIPELINE.pr_checks, slowest_jobs: [], slowest_min_results: 0 },
  });
  const zero = document.querySelector<HTMLElement>('[data-section="slowest-jobs"]');
  expect(zero).toHaveTextContent("a job needs 1 timed result in the 7 days to be ranked.");
  expect(zero).toHaveTextContent("No job has 1 timed result yet.");
});

test("the failing-jobs note names the roll-up the server leaves out only by its role", () => {
  view();
  const note = document.querySelector('[data-section="failing-jobs"] p')?.textContent ?? "";
  expect(note).toContain("Every failed job but the roll-up job the settings name");
});

test("names the shown days in the header", () => {
  view();
  expect(document.body).toHaveTextContent("Last 7 days, Sep 26 to Oct 2 (UTC)");
});

function renderPage(path = "/pipeline") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <PipelinePage />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

/** The page's live socket, accepted, and the pipeline subscription it sent on it. */
async function subscribed(): Promise<{ socket: FakeLiveSocket; frame: LiveSubscribe }> {
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  const frame = await waitFor(() => socket.subscribed("pipeline"));
  return { socket, frame };
}

test("the page subscribes the pipeline, forwarding a pinned at from its URL, and titles the tab", async () => {
  renderPage("/pipeline?at=2026-10-02T12:00:00Z");
  expect(await screen.findByRole("heading", { level: 1, name: "Pipeline" })).toBeDefined();
  const { socket, frame } = await subscribed();
  expect(frame.view).toEqual({ kind: "pipeline", at: "2026-10-02T12:00:00Z" });
  act(() => socket.receive(snapshotFrame(frame.sub, PIPELINE)));
  await screen.findByText("PR CI");
  // A failure's age is measured from the pinned at (11:40 against 12:00), not the clock.
  expect(document.querySelector('[data-failing-job="build / unit"] a')?.textContent).toBe(
    "20 min ago"
  );
  expect(document.title).toBe("Pipeline · Dispatch");
});

test("without at in its URL the page subscribes today's pipeline", async () => {
  renderPage();
  const { socket, frame } = await subscribed();
  expect(frame.view).toEqual({ kind: "pipeline" });
  act(() => socket.receive(snapshotFrame(frame.sub, PIPELINE)));
  await screen.findByText("PR CI");
  expect(socket.subscribes()).toHaveLength(1);
});

test("an unconfigured delivery points at the settings that set it up", async () => {
  renderPage();
  const { socket, frame } = await subscribed();
  act(() =>
    socket.receive(errorFrame(frame.sub, "DELIVERY_NOT_CONFIGURED", "delivery is not configured"))
  );
  const link = await screen.findByRole("link", { name: "Set up the delivery timeline" });
  expect(link).toHaveAttribute("href", "/delivery");
});

test("while the snapshot is out the page says it is loading, and shows nothing else", async () => {
  renderPage();
  await subscribed();
  expect(await screen.findByText("Loading pipeline…")).toBeDefined();
  expect(screen.queryByText("PR CI")).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

test("a refused subscription says so with Retry, and Retry subscribes again", async () => {
  const user = userEvent.setup();
  renderPage();
  const { socket, frame } = await subscribed();
  act(() => socket.receive(errorFrame(frame.sub, "UNAVAILABLE", "try later")));
  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent("Couldn't load the pipeline.");
  expect(
    screen.queryByRole("link", { name: "Set up the delivery timeline" })
  ).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: "Retry" }));
  const retried = await waitFor(() => {
    const again = socket.subscribed("pipeline");
    expect(again.sub).not.toBe(frame.sub);
    return again;
  });
  act(() => socket.receive(snapshotFrame(retried.sub, PIPELINE)));
  await screen.findByText("PR CI");
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});
