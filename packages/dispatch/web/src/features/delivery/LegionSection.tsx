// The Delivery page's first section (INFRA-1077's spec, R3, R4 and the Design's "The Delivery
// page", section 1, as dispatch://INFRA-2916's R14 moves it onto the eval registry): Legion, issue
// to merge — the four metrics of the issues that finished in the page's window, one block for
// each project with a Legion setting.
//
// Every number comes from the eval routes: the server holds the rules (the work time less parked
// and queued hours, the seven bars and their five segments in their one precedence, the review
// rounds and send-backs, the four classes of filing) and folds a window's stored day rows through
// them. The section adds up only what the routes answer apart: the four classes' filings into the
// issues filed, and legion-splitting's per-day counts into the UTC weeks the card draws
// (`weekOf`). The projects are the ones `GET /api/v1/settings/legion` names, every one of them,
// and their Legion evals are one read across them (`GET /api/v1/evals?project=…&kind=legion`).
// The work-time and the rework cards list the issues that finished in the range, one read of
// legion-work-time's items (`GET /api/v1/projects/{key}/evals/{slug}/items`), which are
// legion-rework's issues too. Each measure is three series, every issue, the roots of their trees
// and the rest (`<series>`, `<series>:roots`, `<series>:children`), and a card draws the cuts it
// needs beside one another: no toggle picks one.
//
// Each metric goes through `Chart` (`Chart.tsx`), so each has the graph-or-table toggle and the
// compare range every chart of a measure over a range has; the comparison is the page's own, read
// as a second call of the same routes over the compare range, the way `PipelineSection` reads its
// own route twice. The page gives the section the window and the comparison it gives the Pipeline
// section and the delivery timeline (`DeliverySectionProps`), so every chart on the page follows
// one window control and one compare range. The routes take two instants rather than the page's
// preset, so a preset is resolved to a range once per choice (`metricsRange`).
import { useQuery } from "@tanstack/react-query";
import { memo, type ReactNode, useMemo } from "react";

import { legionEvalItemsQuery, legionEvalsQuery, legionSettingsQuery } from "../../api/queries";
import type {
  EvalItemRow,
  EvalListItem,
  EvalListResponse,
  EvalSeriesValues,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import {
  Chart,
  type ChartCompare,
  type ChartRange,
  type ChartSeries,
  type DeliverySectionProps,
  NOTHING,
  PENDING,
} from "./Chart";
import { formatDuration, pct } from "./lib/format";
import { DAY_MS, liveWindow, type WindowChoice } from "./lib/window";

const CARD = `flex min-w-0 flex-col gap-1.5 rounded border px-3 py-2 ${surfaceBg} ${borderDefault}`;

/** The four Legion evals the cards draw, by slug (`packages/envoy/internal/dispatch/evals/
 *  legion_days.go`). */
const SLUG = {
  workTime: "legion-work-time",
  phaseTime: "legion-phase-time",
  rework: "legion-rework",
  splitting: "legion-splitting",
} as const;

/** The two cuts beside every issue, as a series names them (`metrics.CutSeries`): `hours:roots`
 *  holds the roots of their trees and `hours:children` every other issue. */
const CUTS = [
  { suffix: ":roots", label: "roots" },
  { suffix: ":children", label: "children" },
] as const;

/** The five segments of a bar, in the precedence the server books an hour by (the spec's "Rule
 *  details — Precedence"), each a run of bars over the seven worker types, by the statistic each
 *  bar's series states it as. */
const SEGMENTS: readonly { readonly key: string; readonly label: string }[] = [
  { key: "waiting_on_person", label: "Waiting on a person" },
  { key: "in_a_turn", label: "In a turn" },
  { key: "ci_wait", label: "CI wait" },
  { key: "waiting_on_children", label: "Waiting on children" },
  { key: "idle", label: "Idle" },
];

/** The four classes of filing, in the order the card lists them (`metrics.classes`), each the
 *  series `filed:<class>` of legion-splitting. */
const CLASSES: readonly { readonly key: string; readonly label: string }[] = [
  { key: "in_tree_at_design", label: "In the tree, at design" },
  { key: "in_tree_later", label: "In the tree, later" },
  { key: "out_of_tree", label: "Out of the tree" },
  { key: "by_the_controller", label: "By the controller" },
];

/** The seven bars of metric 2, each legion-phase-time's series of that role: the worker type, and
 *  "no role" for the seventh bar, which holds the phases no role works. */
const ROLES: readonly { readonly role: string; readonly label: string }[] = [
  { role: "architect", label: "architect" },
  { role: "planner", label: "planner" },
  { role: "implementer", label: "implementer" },
  { role: "tester", label: "tester" },
  { role: "reviewer", label: "reviewer" },
  { role: "merger", label: "merger" },
  { role: "no_role", label: "no role" },
];

/** Hours as the page's other durations read ("3.2h"), which `formatDuration` writes in minutes. */
const hours = (value: number): string => formatDuration(value * 60);

const asCount = (value: number): string => `${value}`;

/** A figure's text: the value as `format` writes it, or `NOTHING` where the answer holds none — a
 *  window before the series' first day, or no issue to count: never a zero. */
const shown = (value: number | null, format: (value: number) => string): string =>
  value === null ? NOTHING : format(value);

/** The top of a run of bars: the largest value it draws, and 1 for a run of nothing, since a
 *  scale whose top is zero draws no bar at all. */
const topOf = (values: readonly (number | null)[]): number =>
  Math.max(1, ...values.filter((value): value is number => value !== null));

/** The range the eval routes read for a window choice: a custom range as the reader named it,
 *  and a preset as its own days ending at `nowMs`. The routes take two instants, so a preset is
 *  resolved once per choice rather than on every render, which would read the routes again every
 *  time anything else on the page moved. */
function metricsRange(choice: WindowChoice, nowMs: number): ChartRange {
  const live = liveWindow(choice);
  if (!("preset" in live)) return { kind: "custom", from: live.from, to: live.to };
  return {
    kind: "custom",
    from: new Date(nowMs - live.preset * DAY_MS).toISOString(),
    to: new Date(nowMs).toISOString(),
  };
}

/** One project's Legion evals of one answer, by slug. */
type ProjectEvals = ReadonlyMap<string, EvalListItem>;

/** The evals of a project the compare range's read holds none of. */
const NO_EVALS: ProjectEvals = new Map();

/** Each project's Legion evals in an answer of the cross-project read, by project key. */
function evalsByProject(answer: EvalListResponse | undefined): ReadonlyMap<string, ProjectEvals> {
  const projects = new Map<string, Map<string, EvalListItem>>();
  for (const item of answer?.items ?? []) {
    const project = item.definition.project_key;
    const held = projects.get(project) ?? new Map<string, EvalListItem>();
    held.set(item.definition.slug, item);
    projects.set(project, held);
  }
  return projects;
}

/** One series of one eval, as the answer listed it. */
function seriesOf(
  evals: ProjectEvals | undefined,
  slug: string,
  series: string
): EvalSeriesValues | undefined {
  return evals?.get(slug)?.series?.find((one) => one.series === series);
}

/** One statistic of one series over the window, null where the answer holds no value. */
function stat(
  evals: ProjectEvals | undefined,
  slug: string,
  series: string,
  name: string
): number | null {
  return seriesOf(evals, slug, series)?.window?.[name] ?? null;
}

/** The first day an eval's series holds data, where the answer's window starts before it: every
 *  figure of the window reads no value then, and the card says from when it has data. */
function dataSinceAfter(
  evals: ProjectEvals | undefined,
  slug: string,
  series: string,
  windowFrom: string | undefined
): string | null {
  const since = seriesOf(evals, slug, series)?.data_since ?? null;
  if (since === null || windowFrom === undefined) return null;
  return since > windowFrom.slice(0, 10) ? since : null;
}

/** One issue a Legion eval counts, read off its items row: the stored facts the cards draw. A
 *  row that carries none of them — a reading someone logged under the eval's slug — is no issue
 *  of the stored facts, and the lists leave it out. */
interface LegionIssue {
  readonly key: string;
  readonly workHours: number;
  readonly parkedHours: number;
  readonly queuedHours: number;
  readonly reviewRounds: number;
  readonly implementingAfterFirstReview: number;
  readonly testingAfterFirstReview: number;
}

function issueOf(row: EvalItemRow): LegionIssue | null {
  const cols = row.cols ?? {};
  const number = (name: string): number | null => {
    const value = cols[name];
    return typeof value === "number" ? value : null;
  };
  const workHours = number("work_hours");
  const parkedHours = number("parked_hours");
  const queuedHours = number("queued_hours");
  const reviewRounds = number("review_rounds");
  const implementing = number("implementing_after_first_review");
  const testing = number("testing_after_first_review");
  if (
    workHours === null ||
    parkedHours === null ||
    queuedHours === null ||
    reviewRounds === null ||
    implementing === null ||
    testing === null
  ) {
    return null;
  }
  return {
    implementingAfterFirstReview: implementing,
    key: row.row_key,
    parkedHours,
    queuedHours,
    reviewRounds,
    testingAfterFirstReview: testing,
    workHours,
  };
}

/** The issues of an items answer, in its own order (the server reads its issues by key). */
function issuesOf(rows: readonly EvalItemRow[]): LegionIssue[] {
  return rows.map(issueOf).filter((issue): issue is LegionIssue => issue !== null);
}

/** A run of bars over a card's issues, one row per issue. */
function issueRun(
  issues: readonly LegionIssue[],
  label: string,
  format: (value: number) => string,
  max: number,
  value: (issue: LegionIssue) => number
): ChartSeries {
  return {
    kind: "spark",
    format,
    label,
    max,
    points: issues.map((issue) => ({ label: issue.key, partial: false, value: value(issue) })),
  };
}

/** Metric 1's chart: each finished issue's work time, and the hours taken out of it — the time it
 *  sat parked and the time it sat back in the queue. One row per issue of legion-work-time's
 *  items. */
function workTimeSeries(rows: readonly EvalItemRow[]): ChartSeries[] {
  const issues = issuesOf(rows);
  const max = topOf(issues.map((issue) => issue.workHours));
  return [
    issueRun(issues, "Work time", hours, max, (issue) => issue.workHours),
    issueRun(issues, "Parked", hours, max, (issue) => issue.parkedHours),
    issueRun(issues, "Queued", hours, max, (issue) => issue.queuedHours),
  ];
}

/** Metric 2's chart: each worker type's bar, split into its five segments, the median hours per
 *  issue beside them, how many issues held hours in that bar, and the bar's hours in the roots and
 *  the children cuts. One row per worker type, each legion-phase-time's series of that role; a
 *  bar's whole time is its five segments, so the table holds every hour the card counts.
 *
 *  The median runs over every finished issue of the window, an issue the role never worked
 *  counting as zero, so the seven bars describe one population; `Issues with hours` is how many
 *  issues that bar holds hours for, which is what keeps a median of 0 readable as a population
 *  most issues are not in rather than as a false number. It counts the issues the bar holds
 *  hours for anywhere, so it qualifies the five segment rows as much as the median. */
function phaseTimeSeries(evals: ProjectEvals | undefined): ChartSeries[] {
  // One bar per worker type of one statistic; the segments and the cuts' hours share the scale of
  // the bars' whole hours, and the median and the count each take their own.
  const run = (
    label: string,
    format: (value: number) => string,
    value: (role: string) => number | null,
    scale?: number
  ): ChartSeries => {
    const points = ROLES.map((bar) => ({
      label: bar.label,
      partial: false,
      value: value(bar.role),
    }));
    const max = scale ?? topOf(points.map((point) => point.value));
    return { kind: "spark", format, label, max, points };
  };
  const max = topOf(ROLES.map((bar) => stat(evals, SLUG.phaseTime, bar.role, "hours")));
  return [
    ...SEGMENTS.map((segment) =>
      run(segment.label, hours, (role) => stat(evals, SLUG.phaseTime, role, segment.key), max)
    ),
    run("Median per issue", hours, (role) => stat(evals, SLUG.phaseTime, role, "median_per_issue")),
    run("Issues with hours", asCount, (role) => stat(evals, SLUG.phaseTime, role, "issues")),
    ...CUTS.map((cut) =>
      run(
        `Hours, ${cut.label}`,
        hours,
        (role) => stat(evals, SLUG.phaseTime, `${role}${cut.suffix}`, "hours"),
        max
      )
    ),
  ];
}

/** Metric 3's chart: each finished issue's review rounds, and the implementing and testing hours
 *  it spent after its first review. One row per issue of the range's one items read, whose
 *  finished issues are legion-rework's as well as legion-work-time's. */
function reworkSeries(rows: readonly EvalItemRow[]): ChartSeries[] {
  const issues = issuesOf(rows);
  const after = topOf(
    issues.flatMap((issue) => [issue.implementingAfterFirstReview, issue.testingAfterFirstReview])
  );
  const rounds = topOf(issues.map((issue) => issue.reviewRounds));
  return [
    issueRun(issues, "Review rounds", asCount, rounds, (issue) => issue.reviewRounds),
    issueRun(
      issues,
      "Implementing after the first review",
      hours,
      after,
      (issue) => issue.implementingAfterFirstReview
    ),
    issueRun(
      issues,
      "Testing after the first review",
      hours,
      after,
      (issue) => issue.testingAfterFirstReview
    ),
  ];
}

/** The UTC Monday a `YYYY-MM-DD` day's week begins on, as a `YYYY-MM-DD` day: the server's own
 *  week rule (`weekStart` in `packages/envoy/internal/dispatch/legion/metrics/compute.go`, which
 *  `metrics.FoldSplitting` groups its weeks by), stated once more here because the eval read
 *  answers legion-splitting by the day and the card has always drawn it by the week. */
export function weekOf(day: string): string {
  const at = new Date(`${day}T00:00:00Z`);
  const back = (at.getUTCDay() + 6) % 7;
  return new Date(at.getTime() - back * DAY_MS).toISOString().slice(0, 10);
}

/** Metric 4's chart: what Legion sessions filed each UTC week, by class, against the issues that
 *  finished that week, and the issues that finished in the roots and the children cuts. One row
 *  per week, named by the Monday it begins on: each legion-splitting series' per-day counts
 *  added up by `weekOf`, a week with no day of a series holding no value of it. */
function splittingSeries(evals: ProjectEvals | undefined): ChartSeries[] {
  const weeks = (series: string) => {
    const counts = new Map<string, number | null>();
    for (const day of seriesOf(evals, SLUG.splitting, series)?.days ?? []) {
      const week = weekOf(day.day);
      const count = day.statistics?.issues ?? null;
      const held = counts.get(week) ?? null;
      counts.set(week, count === null ? held : (held ?? 0) + count);
    }
    return [...counts].map(([week, value]) => ({ label: week, partial: false, value }));
  };
  const runs = [
    ...CLASSES.map((filing) => ({ label: filing.label, points: weeks(`filed:${filing.key}`) })),
    { label: "Issues finished", points: weeks("finished") },
    ...CUTS.map((cut) => ({
      label: `Issues finished, ${cut.label}`,
      points: weeks(`finished${cut.suffix}`),
    })),
  ];
  const max = topOf(runs.flatMap((run) => run.points.map((point) => point.value)));
  return runs.map((run): ChartSeries => ({ kind: "spark", format: asCount, max, ...run }));
}

/** The Delivery page's `#legion` section: one block for each project with a Legion setting, each
 *  the four Legion metrics of that project over the page's window. A deployment where no project
 *  names a Legion daemon says so and reads nothing. Memoised, as the Pipeline section is: the page
 *  re-renders on every timeline frame, and the section's props move only with the window and the
 *  compare range. */
export const LegionSection = memo(function LegionSection({
  compare,
  onCompare,
  window: chosen,
}: DeliverySectionProps): ReactNode {
  const settings = useQuery(legionSettingsQuery());
  const projects = useMemo(
    () => (settings.data ?? []).map((setting) => setting.project),
    [settings.data]
  );
  // The window as two instants, resolved when the page's choice changes and not on every render:
  // a preset's end is the minute it was resolved in, so a re-render reads no new range.
  const range = useMemo(
    () => metricsRange(chosen, Math.floor(Date.now() / 60_000) * 60_000),
    [chosen]
  );
  const query = useQuery({
    ...legionEvalsQuery({ from: range.from, projects, to: range.to }),
    enabled: projects.length > 0,
  });
  // The comparison's own read, as a range of its own: while nothing is compared the query names
  // the section's own range and is disabled, so it reads nothing.
  const compared = useQuery({
    ...legionEvalsQuery({
      from: (compare ?? range).from,
      projects,
      to: (compare ?? range).to,
    }),
    enabled: compare !== null && projects.length > 0,
  });
  const byProject = useMemo(() => evalsByProject(query.data), [query.data]);
  // A disabled query still answers what the cache holds under its key, and while nothing is
  // compared that key is the window's own read, so the compare answer is read only while a
  // compare range exists; undefined while its read is out.
  const comparedAnswer = compare === null ? undefined : compared.data;
  const comparedByProject = useMemo(
    () => (comparedAnswer === undefined ? undefined : evalsByProject(comparedAnswer)),
    [comparedAnswer]
  );

  return (
    <section aria-labelledby="legion-heading" className="flex flex-col gap-3" id="legion">
      <header className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2">
        <h2 className={`text-lg font-semibold ${textPrimaryOnCanvas}`} id="legion-heading">
          Legion: issue to merge
        </h2>
      </header>
      {settings.isPending ? <p className={textMutedOnCanvas}>Loading Legion projects…</p> : null}
      {projects.length === 0 && !settings.isPending ? (
        <p className={`text-sm ${textSecondaryOnCanvas}`} data-legion-metrics-unconfigured>
          No project names a Legion daemon, so there is nothing to measure here. The Settings page
          names one.
        </p>
      ) : null}
      {query.isPending && projects.length > 0 ? (
        <p className={textMutedOnCanvas}>Loading the Legion metrics…</p>
      ) : null}
      {query.isError ? (
        <QueryError
          message="Couldn't load the Legion metrics."
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : null}
      {query.data === undefined
        ? null
        : projects.map((project) => (
            <LegionProject
              compare={compare}
              comparedEvals={
                comparedByProject === undefined
                  ? undefined
                  : (comparedByProject.get(project) ?? NO_EVALS)
              }
              evals={byProject.get(project)}
              key={project}
              onCompare={onCompare}
              project={project}
              range={{ kind: "custom", from: query.data.window.from, to: query.data.window.to }}
            />
          ))}
    </section>
  );
});

/** One project's block: its four cards over the answer's window, the lists of issues of two of
 *  them read once per range. `compare` and `onCompare` are the page's comparison, and
 *  `comparedEvals` the project's evals in the compare range's read: undefined while that read is
 *  out or nothing is compared, and empty where that read holds no eval of the project, whose
 *  compare columns then read no value rather than a read still out. */
function LegionProject({
  compare,
  comparedEvals,
  evals,
  onCompare,
  project,
  range,
}: {
  compare: ChartRange | null;
  comparedEvals: ProjectEvals | undefined;
  evals: ProjectEvals | undefined;
  onCompare: (next: ChartRange | null) => void;
  project: string;
  range: ChartRange;
}): ReactNode {
  // The issues that finished in a range, which the work-time and the rework lists both draw:
  // the items route reads either eval's issues from the same closed issues' stored facts, every
  // issue of the range, so one read of legion-work-time's serves both cards.
  const finishedIn = (over: ChartRange | null) => ({
    ...legionEvalItemsQuery(project, SLUG.workTime, {
      from: (over ?? range).from,
      series: "hours",
      to: (over ?? range).to,
    }),
    enabled: over !== null,
  });
  const finished = useQuery(finishedIn(range));
  const finishedCompared = useQuery(finishedIn(compare));
  // As the section's compare read: the disabled query's cache is the window's own while nothing
  // is compared.
  const comparedIssues = compare === null ? undefined : finishedCompared.data;

  // What each card draws, and what the compare range's answers draw beside it: built once per
  // answer rather than on every render, since `Chart` builds its table of every number from the
  // series it is handed.
  const series = useMemo(
    () => ({
      phaseTime: phaseTimeSeries(evals),
      rework: finished.data === undefined ? undefined : reworkSeries(finished.data),
      splitting: splittingSeries(evals),
      workTime: finished.data === undefined ? undefined : workTimeSeries(finished.data),
    }),
    [evals, finished.data]
  );
  const compared = useMemo(
    () => ({
      phaseTime: comparedEvals === undefined ? undefined : phaseTimeSeries(comparedEvals),
      rework: comparedIssues === undefined ? undefined : reworkSeries(comparedIssues),
      splitting: comparedEvals === undefined ? undefined : splittingSeries(comparedEvals),
      workTime: comparedIssues === undefined ? undefined : workTimeSeries(comparedIssues),
    }),
    [comparedEvals, comparedIssues]
  );
  // Each chart's comparison: the page's range and callback, with the compare range's series of
  // that chart, undefined while the read it comes from is out.
  const against = (drawn: readonly ChartSeries[] | undefined): ChartCompare => ({
    onRange: onCompare,
    range: compare,
    series: drawn,
  });
  const work = (series: string, name: string) => stat(evals, SLUG.workTime, series, name);
  const rounds = (series: string, name: string) => stat(evals, SLUG.rework, series, name);
  const split = (series: string, name: string) => stat(evals, SLUG.splitting, series, name);
  const filed = (suffix: string): number | null => {
    const classes = CLASSES.map((filing) => split(`filed:${filing.key}${suffix}`, "issues"));
    return classes.every((count) => count === null)
      ? null
      : classes.reduce<number>((sum, count) => sum + (count ?? 0), 0);
  };
  const since = (slug: string, series: string) => dataSinceAfter(evals, slug, series, range.from);
  const label = (title: string) => `${project}: ${title}`;

  return (
    <div className="flex min-w-0 flex-col gap-2" data-legion-project={project}>
      <h3 className={`text-base font-semibold ${textPrimaryOnCanvas}`}>{project}</h3>
      <div className="@container">
        <div className="grid min-w-0 grid-cols-1 gap-2 @min-[56rem]:grid-cols-2">
          <MetricCard
            figures={[
              { label: "Median", value: shown(work("hours", "median"), hours) },
              { label: "p90", value: shown(work("hours", "p90"), hours) },
              { label: "Issues finished", value: shown(work("hours", "n"), asCount) },
              ...CUTS.map((cut) => ({
                label: `Median, ${cut.label}`,
                value: shown(work(`hours${cut.suffix}`, "median"), hours),
              })),
            ]}
            metric="work_time"
            since={since(SLUG.workTime, "hours")}
            title="Work time to merge"
          >
            <IssueChart
              compare={against(compared.workTime)}
              label={label("Work time to merge")}
              query={finished}
              range={range}
              series={series.workTime}
            />
          </MetricCard>
          <MetricCard
            figures={[
              {
                label: "Idle share",
                value: shown(stat(evals, SLUG.phaseTime, "architect", "idle_share"), pct),
              },
              ...CUTS.map((cut) => ({
                label: `Idle share, ${cut.label}`,
                value: shown(
                  stat(evals, SLUG.phaseTime, `architect${cut.suffix}`, "idle_share"),
                  pct
                ),
              })),
            ]}
            metric="phase_time"
            since={since(SLUG.phaseTime, "architect")}
            title="Phase time by worker type"
          >
            <Chart
              compare={against(compared.phaseTime)}
              label={label("Phase time by worker type")}
              range={range}
              rows={{ header: "Worker type", over: "worker type", pair: "name" }}
              series={series.phaseTime}
            />
          </MetricCard>
          <MetricCard
            figures={[
              {
                label: "Median review rounds",
                value: shown(rounds("review_rounds", "median"), (value) => value.toFixed(1)),
              },
              {
                label: "Sent back at least once",
                value: shown(rounds("sent_back", "share"), pct),
              },
              {
                label: "Median hours after the first review",
                value: shown(rounds("hours_after_first_review", "median"), hours),
              },
              ...CUTS.map((cut) => ({
                label: `Median review rounds, ${cut.label}`,
                value: shown(rounds(`review_rounds${cut.suffix}`, "median"), (value) =>
                  value.toFixed(1)
                ),
              })),
            ]}
            metric="rework"
            since={since(SLUG.rework, "review_rounds")}
            title="Rework"
          >
            <IssueChart
              compare={against(compared.rework)}
              label={label("Rework")}
              query={finished}
              range={range}
              series={series.rework}
            />
          </MetricCard>
          <MetricCard
            figures={[
              { label: "Issues filed", value: shown(filed(""), asCount) },
              ...CUTS.map((cut) => ({
                label: `Issues filed, ${cut.label}`,
                value: shown(filed(cut.suffix), asCount),
              })),
              { label: "Roots finished", value: shown(split("roots", "issues"), asCount) },
              {
                label: "Children per root",
                value: shown(split("roots", "children_per_root"), (value) => value.toFixed(1)),
              },
              {
                label: "Median work time, roots with children",
                value: shown(split("hours_with_children", "median"), hours),
              },
              {
                label: "Median work time, roots without children",
                value: shown(split("hours_without_children", "median"), hours),
              },
            ]}
            metric="splitting"
            since={since(SLUG.splitting, "finished")}
            title="Splitting and follow-ups"
          >
            <Chart
              compare={against(compared.splitting)}
              label={label("Splitting and follow-ups")}
              range={range}
              rows={{ header: "Week", over: "UTC week", pair: "position" }}
              series={series.splitting}
            />
          </MetricCard>
        </div>
      </div>
    </div>
  );
}

/** A card's chart over its list of issues, once the read of the range's finished issues has
 *  landed: one row per issue, paired with the compare range's row of the same issue. */
function IssueChart({
  compare,
  label,
  query,
  range,
  series,
}: {
  compare: ChartCompare;
  label: string;
  query: { isError: boolean; isFetching: boolean; refetch: () => unknown };
  range: ChartRange;
  series: readonly ChartSeries[] | undefined;
}): ReactNode {
  if (query.isError) {
    return (
      <QueryError
        message="Couldn't load the issues of this metric."
        onRetry={() => void query.refetch()}
        retrying={query.isFetching}
      />
    );
  }
  if (series === undefined) return <p className={`text-xs ${textMutedOnSurface}`}>{PENDING}</p>;
  return (
    <Chart
      compare={compare}
      label={label}
      range={range}
      rows={{ header: "Issue", over: "issue", pair: "name" }}
      series={series}
    />
  );
}

/** One metric: its headline numbers, from when its eval has data where the window starts before
 *  that, and its chart. */
function MetricCard({
  children,
  figures,
  metric,
  since,
  title,
}: {
  children: ReactNode;
  figures: readonly { readonly label: string; readonly value: string }[];
  metric: string;
  since: string | null;
  title: string;
}): ReactNode {
  return (
    <div className={CARD} data-legion-metric={metric}>
      <h4 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>{title}</h4>
      <dl className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        {figures.map((figure) => (
          <div className="flex items-baseline gap-1.5" key={figure.label}>
            <dt className={`text-xs ${textMutedOnSurface}`}>{figure.label}</dt>
            <dd
              className={`text-base font-semibold ${textPrimaryOnCanvas}`}
              data-legion-figure={figure.label}
            >
              {figure.value === "" ? PENDING : figure.value}
            </dd>
          </div>
        ))}
      </dl>
      {since === null ? null : (
        <p className={`text-xs ${textMutedOnSurface}`} data-legion-since={since}>
          {`Measured from ${since}: a window that starts before then reads no value.`}
        </p>
      )}
      {children}
    </div>
  );
}
