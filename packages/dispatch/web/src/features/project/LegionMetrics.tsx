// The four Legion metrics of one project (INFRA-1077's spec, R3 and R4; dispatch://INFRA-979 puts
// them on the project's issues page, in LegionHealthPanel): the work time to merge, the time by
// worker type, rework, and splitting and follow-ups of the issues that finished in a range.
//
// Every number comes from `GET /api/v1/legion/metrics` and nothing is computed here: the server
// holds the rules, and this draws what it answers. Each metric goes through `Chart`
// (`delivery/Chart.tsx`), so each has the graph-or-table toggle and the compare range. A card's
// table rows, its numbers and its headline counts name the issues behind them through
// `onSelectIssues`, which the issues table filters itself by.
import { type ReactNode, useMemo } from "react";

import type {
  LegionBarRole,
  LegionFilingClass,
  LegionMetricsResponse,
  LegionReworkIssue,
  LegionSegments,
  LegionWorkTimeIssue,
} from "../../api/types";
import {
  borderDefault,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import {
  Chart,
  type ChartCompareSource,
  type ChartRange,
  type ChartRows,
  type ChartSeries,
  chartCompare,
  PENDING,
} from "../delivery/Chart";
import { formatDuration, pct } from "../delivery/lib/format";
import { DAY_MS, liveWindow, type WindowChoice } from "../delivery/lib/window";

const CARD = `flex min-w-0 flex-col gap-1.5 rounded border px-3 py-2 ${surfaceBg} ${borderDefault}`;

/** The five segments of a bar, in the precedence the server books an hour by (the spec's "Rule
 *  details — Precedence"), each a run of bars over the seven worker types. */
const SEGMENTS: readonly { readonly key: keyof LegionSegments; readonly label: string }[] = [
  { key: "waiting_on_person", label: "Waiting on a person" },
  { key: "in_a_turn", label: "In a turn" },
  { key: "ci_wait", label: "CI wait" },
  { key: "waiting_on_children", label: "Waiting on children" },
  { key: "idle", label: "Idle" },
  { key: "unknown", label: "Unknown (not observed)" },
];

/** The four classes of filing, in the order the card lists them (`metrics.classes`). */
const CLASSES: readonly { readonly key: LegionFilingClass; readonly label: string }[] = [
  { key: "in_tree_at_design", label: "In the tree, at design" },
  { key: "in_tree_later", label: "In the tree, later" },
  { key: "out_of_tree", label: "Out of the tree" },
  { key: "by_the_controller", label: "By the controller" },
];

/** What each bar of metric 2 is called: the worker type, and "no role" for the seventh bar,
 *  which holds the phases no role works. */
const ROLES: Readonly<Record<LegionBarRole, string>> = {
  architect: "architect",
  planner: "planner",
  implementer: "implementer",
  tester: "tester",
  reviewer: "reviewer",
  merger: "merger",
  no_role: "no role",
};

/** Hours as the page's other durations read ("3.2h"), which `formatDuration` writes in minutes. */
const hours = (value: number): string => formatDuration(value * 60);

const asCount = (value: number): string => `${value}`;

/** The top of a run of bars: the largest value it draws, and 1 for a run of nothing, since a
 *  scale whose top is zero draws no bar at all. */
const topOf = (values: readonly number[]): number => Math.max(1, ...values);

/** The range the metrics route reads for a window choice: a custom range as the reader named it,
 *  and a preset as its own days ending at `nowMs`. The route takes two instants, so a preset is
 *  resolved once per choice rather than on every render, which would read the route again every
 *  time anything else on the page moved. */
export function metricsRange(choice: WindowChoice, nowMs: number): ChartRange {
  const live = liveWindow(choice);
  if (!("preset" in live)) return { kind: "custom", from: live.from, to: live.to };
  return {
    kind: "custom",
    from: new Date(nowMs - live.preset * DAY_MS).toISOString(),
    to: new Date(nowMs).toISOString(),
  };
}

/** Metric 1's chart: each finished issue's work time, and the hours taken out of it — the time it
 *  sat parked and the time it sat back in the queue. One row per issue, in the answer's own order
 *  (the server reads its issues by key). */
export function workTimeSeries(report: LegionMetricsResponse): ChartSeries[] {
  const issues = report.work_time.issues;
  const max = topOf(issues.map((issue) => issue.hours));
  const run = (label: string, value: (issue: LegionWorkTimeIssue) => number): ChartSeries => ({
    kind: "spark",
    format: hours,
    label,
    max,
    points: issues.map((issue) => ({ label: issue.key, partial: false, value: value(issue) })),
  });
  return [
    run("Work time", (issue) => issue.hours),
    run("Parked", (issue) => issue.parked_hours),
    run("Queued", (issue) => issue.queued_hours),
  ];
}

/** Metric 2's chart: each worker type's bar, split into its five segments, and the median hours
 *  per issue beside them. One row per worker type; a bar's whole time is its five segments, so
 *  the table holds every hour the card counts. */
export function phaseTimeSeries(report: LegionMetricsResponse): ChartSeries[] {
  const bars = report.phase_time.bars;
  const max = topOf(bars.map((bar) => bar.hours));
  const points = (value: (bar: (typeof bars)[number]) => number) =>
    bars.map((bar) => ({
      label: ROLES[bar.role],
      partial: false,
      value: value(bar),
    }));
  return [
    ...SEGMENTS.map(
      (segment): ChartSeries => ({
        kind: "spark",
        format: hours,
        label: segment.label,
        max,
        points: points((bar) => bar.segments[segment.key]),
      })
    ),
    {
      kind: "spark",
      format: hours,
      label: "Median per issue",
      max: topOf(bars.map((bar) => bar.median_hours_per_issue)),
      points: points((bar) => bar.median_hours_per_issue),
    },
  ];
}

/** Metric 3's chart: each finished issue's review rounds, and the implementing and testing hours
 *  it spent after its first review. One row per issue. */
export function reworkSeries(report: LegionMetricsResponse): ChartSeries[] {
  const issues = report.rework.issues;
  const after = topOf(
    issues.flatMap((issue) => [
      issue.implementing_hours_after_first_review,
      issue.testing_hours_after_first_review,
    ])
  );
  const run = (
    label: string,
    format: (value: number) => string,
    max: number,
    value: (issue: LegionReworkIssue) => number
  ): ChartSeries => ({
    kind: "spark",
    format,
    label,
    max,
    points: issues.map((issue) => ({ label: issue.key, partial: false, value: value(issue) })),
  });
  const rounds = topOf(issues.map((issue) => issue.review_rounds));
  return [
    run("Review rounds", asCount, rounds, (issue) => issue.review_rounds),
    run("Implementing after the first review", hours, after, (issue) => {
      return issue.implementing_hours_after_first_review;
    }),
    run("Testing after the first review", hours, after, (issue) => {
      return issue.testing_hours_after_first_review;
    }),
  ];
}

/** Metric 4's chart: what Legion sessions filed each week, by class, against the issues that
 *  finished that week. One row per UTC week, named by the Monday it begins on. */
export function splittingSeries(report: LegionMetricsResponse): ChartSeries[] {
  const weeks = report.splitting.weeks;
  const max = topOf(weeks.flatMap((week) => [week.finished, ...Object.values(week.filed)]));
  const run = (label: string, value: (week: (typeof weeks)[number]) => number): ChartSeries => ({
    kind: "spark",
    format: asCount,
    label,
    max,
    points: weeks.map((week) => ({
      label: week.week.slice(0, 10),
      partial: false,
      value: value(week),
    })),
  });
  return [
    ...CLASSES.map((filing) => run(filing.label, (week) => week.filed[filing.key])),
    run("Issues finished", (week) => week.finished),
  ];
}

/** Which of the four Legion cards. */
export type LegionMetricKey = "work_time" | "phase_time" | "rework" | "splitting";

/** One of the four Legion cards for one answer of the route, opened in full: its headline numbers
 *  and its chart. `compare` is the panel's comparison; `onSelectIssues`, when given, makes the
 *  numbers and table rows name the issues behind them. */
export function LegionCard({
  compare,
  metric,
  onSelectIssues,
  report,
}: {
  compare?: ChartCompareSource<LegionMetricsResponse>;
  metric: LegionMetricKey;
  onSelectIssues?: (keys: string[], label: string) => void;
  report: LegionMetricsResponse;
}): ReactNode {
  const series = useMemo(() => SERIES[metric](report), [metric, report]);
  const comparedData = compare?.data;
  const compared = useMemo(
    () => (comparedData === undefined ? undefined : SERIES[metric](comparedData)),
    [comparedData, metric]
  );
  const range: ChartRange = { kind: "custom", from: report.from, to: report.to };
  const select = useMemo(() => selectors(report), [report]);
  const on = (pick: (row: string, column: string | null) => [string[], string]) =>
    onSelectIssues === undefined
      ? undefined
      : (row: string, column: string | null) => onSelectIssues(...pick(row, column));
  const figureKeys = (keys: string[], label: string) =>
    onSelectIssues === undefined ? undefined : () => onSelectIssues(keys, label);
  const { rework, splitting, phase_time: phase } = report;
  const chart = (label: string, rows: ChartRows, pick: Parameters<typeof on>[0]) => (
    <Chart
      compare={chartCompare(compare, compared)}
      label={label}
      onSelect={on(pick)}
      range={range}
      rows={rows}
      series={series}
    />
  );
  const byIssue: ChartRows = { header: "Issue", over: "issue", pair: "name" };
  switch (metric) {
    case "work_time":
      return (
        <MetricCard
          figures={[
            { label: "Median", value: hours(report.work_time.median_hours) },
            { label: "p90", value: hours(report.work_time.p90_hours) },
            {
              label: "Issues finished",
              value: `${report.finished}`,
              onSelect: figureKeys(select.finished, "Finished in the range"),
            },
          ]}
          metric="work_time"
          title="Work time to merge"
        >
          {chart("Work time to merge", byIssue, (row) => [[row], `Work time: ${row}`])}
        </MetricCard>
      );
    case "phase_time":
      return (
        <MetricCard
          figures={[
            {
              label: "Idle share",
              value: phase.idle_share === null ? "not observed yet" : pct(phase.idle_share),
            },
            { label: "Observed hours", value: hours(phase.observed_hours) },
            { label: "Unknown hours", value: hours(phase.unknown_hours) },
          ]}
          metric="phase_time"
          title="Time per worker type"
        >
          <p className={`text-xs ${textMutedOnSurface}`} data-legion-unknown-note>
            {phase.observed_hours === 0
              ? `The daemon reported nothing of these issues in this range, so in a turn, CI wait, waiting on children and idle are unknown: all ${hours(phase.unknown_hours)} not waiting on a person are booked as unknown, not as idle.`
              : `${hours(phase.unknown_hours)} fall before the daemon reported on their issue, so whether a role was in a turn, CI was running or nothing happened is unknown; they are booked as unknown, not as idle.`}
          </p>
          {chart(
            "Time per worker type",
            { header: "Worker type", over: "worker type", pair: "name" },
            select.phase
          )}
        </MetricCard>
      );
    case "rework":
      return (
        <MetricCard
          figures={[
            { label: "Median review rounds", value: rework.median_review_rounds.toFixed(1) },
            { label: "Sent back at least once", value: pct(rework.sent_back_share) },
            {
              label: "Median hours after the first review",
              value: hours(rework.median_hours_after_first_review),
            },
          ]}
          metric="rework"
          title="Rework"
        >
          {chart("Rework", byIssue, (row) => [[row], `Rework: ${row}`])}
        </MetricCard>
      );
    case "splitting":
      return (
        <MetricCard
          figures={[
            {
              label: "Issues filed",
              value: `${splitting.filed.length}`,
              onSelect: figureKeys(select.filed, "Filed by Legion in the range"),
            },
            {
              label: "Roots finished",
              value: `${splitting.finished_roots}`,
              onSelect: figureKeys(select.roots, "Roots finished in the range"),
            },
            { label: "Children per root", value: splitting.children_per_root.toFixed(1) },
            {
              label: "Median work time, roots with children",
              value: hours(splitting.median_hours_roots_with_children),
            },
            {
              label: "Median work time, roots without children",
              value: hours(splitting.median_hours_roots_without_children),
            },
          ]}
          metric="splitting"
          title="Splitting and follow-ups"
        >
          {chart(
            "Splitting and follow-ups",
            { header: "Week", over: "UTC week", pair: "position" },
            select.week
          )}
        </MetricCard>
      );
  }
}

/** Each card's series builder. */
const SERIES: Record<LegionMetricKey, (report: LegionMetricsResponse) => ChartSeries[]> = {
  phase_time: phaseTimeSeries,
  rework: reworkSeries,
  splitting: splittingSeries,
  work_time: workTimeSeries,
};

/** The worker types' names, in the route's bar order, for the panel's stacked bar. */
export const roleName = (role: LegionBarRole): string => ROLES[role];

/** Hours as the cards write them. */
export const formatHours = hours;

/** One metric: its headline numbers and its chart. */
function MetricCard({
  children,
  figures,
  metric,
  title,
}: {
  children: ReactNode;
  figures: readonly {
    readonly label: string;
    readonly value: string;
    readonly onSelect?: (() => void) | undefined;
  }[];
  metric: string;
  title: string;
}): ReactNode {
  return (
    <div className={CARD} data-legion-metric={metric}>
      <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>{title}</h3>
      <dl className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        {figures.map((figure) => (
          <div className="flex items-baseline gap-1.5" key={figure.label}>
            <dt className={`text-xs ${textMutedOnSurface}`}>{figure.label}</dt>
            <dd
              className={`text-base font-semibold ${textPrimaryOnCanvas}`}
              data-legion-figure={figure.label}
            >
              {figure.onSelect === undefined ? (
                figure.value === "" ? (
                  PENDING
                ) : (
                  figure.value
                )
              ) : (
                <button
                  aria-label={`Show the issues: ${figure.label}`}
                  className="underline decoration-dotted underline-offset-2 hover:decoration-solid"
                  onClick={figure.onSelect}
                  type="button"
                >
                  {figure.value}
                </button>
              )}
            </dd>
          </div>
        ))}
      </dl>
      {children}
    </div>
  );
}

/** The Monday 00:00 UTC that begins `iso`'s UTC week, as `YYYY-MM-DD`: the name of a splitting
 *  card's row. */
function weekOf(iso: string): string {
  const at = new Date(iso);
  const day = (at.getUTCDay() + 6) % 7;
  return new Date(Date.UTC(at.getUTCFullYear(), at.getUTCMonth(), at.getUTCDate() - day))
    .toISOString()
    .slice(0, 10);
}

/** The issues behind each number of the four cards, for one answer. */
function selectors(report: LegionMetricsResponse) {
  const finished = report.work_time.issues.map((issue) => issue.key);
  const filed = report.splitting.filed;
  return {
    finished,
    filed: filed.map((one) => one.key),
    roots: report.work_time.issues.filter((issue) => issue.root).map((issue) => issue.key),
    /** A worker type's row: every issue on its bar, or those of one segment of it. */
    phase: (row: string, column: string | null): [string[], string] => {
      const bar = report.phase_time.bars.find((one) => ROLES[one.role] === row);
      const segment = SEGMENTS.find((one) => one.label === column);
      if (bar === undefined) return [[], row];
      if (segment === undefined) return [[...bar.issue_keys], `Time on the ${row} bar`];
      return [[...(bar.segment_issues[segment.key] ?? [])], `${segment.label} on the ${row} bar`];
    },
    /** A UTC week's row: the issues filed that week, of one class, or the ones that finished. */
    week: (row: string, column: string | null): [string[], string] => {
      if (column === "Issues finished") {
        return [
          report.work_time.issues
            .filter((issue) => weekOf(issue.closed_at) === row)
            .map((issue) => issue.key),
          `Finished in the week of ${row}`,
        ];
      }
      const filing = CLASSES.find((one) => one.label === column);
      return [
        filed
          .filter(
            (one) => weekOf(one.at) === row && (filing === undefined || one.class === filing.key)
          )
          .map((one) => one.key),
        filing === undefined
          ? `Filed in the week of ${row}`
          : `Filed in the week of ${row}: ${filing.label}`,
      ];
    },
  };
}
