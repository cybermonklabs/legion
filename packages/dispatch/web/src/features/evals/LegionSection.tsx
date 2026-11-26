import { type ReactNode, useId, useMemo, useState } from "react";

import type { EvalItemRow, EvalListItem, EvalListResponse } from "../../api/types";
import {
  borderDefault,
  borderStrong,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { DAY_MS, lastUtcDate, utcDate } from "../delivery/lib/window";
import {
  type Category,
  type ChartModel,
  type ChartRenderer,
  type ChartSeriesSpec,
  type ChartStatistic,
  categoryChartModel,
  dayChartModel,
  type SeriesSpec,
} from "./chart-model";
import { CategoryBars, PerDayBars, PerDaySpread, PHASE_SEGMENTS } from "./chart-renderers";
import { EvalChart, type RowLink } from "./EvalChart";
import { IssuesLink, readIssueKeys } from "./issue-links";
import type { EvalsViewState } from "./useEvalsViews";

/** The issues a Legion number counts: every issue, the roots alone, or the children alone, each a
 *  cut every Legion series is stored in (metrics.Cuts). */
type Cut = "all" | "roots" | "children";

const CUTS: readonly { readonly value: Cut; readonly label: string }[] = [
  { value: "all", label: "All issues" },
  { value: "roots", label: "Roots" },
  { value: "children", label: "Children" },
];

/** A Legion series as `cut` stores it (metrics.CutSeries): the series itself for every issue,
 *  `<series>:roots` and `<series>:children` for the other two. */
function inCut(series: string, cut: Cut): string {
  return cut === "all" ? series : `${series}:${cut}`;
}

const MEDIAN: ChartStatistic = { id: "median", label: "Median" };
const P90: ChartStatistic = { id: "p90", label: "p90" };
const ISSUES_N: ChartStatistic = { id: "n", label: "Issues" };
const ROOTS_N: ChartStatistic = { id: "n", label: "Roots" };
const ISSUES: ChartStatistic = { id: "issues", label: "Issues" };

/** The roles a Legion issue's work falls to, each a bar of phase time and of idle time. */
const ROLES: readonly Category[] = [
  { name: "architect", label: "Architect" },
  { name: "planner", label: "Planner" },
  { name: "implementer", label: "Implementer" },
  { name: "tester", label: "Tester" },
  { name: "reviewer", label: "Reviewer" },
  { name: "merger", label: "Merger" },
];
/** Phase time's seventh bar: the hours no role's phase holds. */
const NO_ROLE: Category = { name: "no_role", label: "No role" };

/** A count of issues a day, as legion-splitting's filings and finished issues are. */
function issuesSeries(name: string, label: string): ChartSeriesSpec {
  return { name, spec: { headline: ["issues"], label, statistics: [ISSUES] } };
}

/** Where a chart's numbers link, by the issues each counts (plan D13): `series`' window figure of
 *  `statistic` and each day's, which count the issues of the eval's rows that `keep` keeps. */
interface DayLinks {
  readonly series: string;
  readonly statistic: string;
  readonly keep?: (row: EvalItemRow) => boolean;
}

/** A Legion chart drawn a day at a time, each of its series in the chosen cut. */
interface DayChart {
  readonly by: "day";
  readonly slug: string;
  readonly title: string;
  readonly kind: string;
  readonly renderer: ChartRenderer;
  readonly series: readonly ChartSeriesSpec[];
  /** The issues its headline and each day's row count. */
  readonly links?: DayLinks;
}

/** A Legion chart of one bar per role over the whole window, each role a series of the eval. */
interface RoleChart {
  readonly by: "role";
  readonly slug: string;
  readonly title: string;
  readonly roles: readonly Category[];
  readonly spec: SeriesSpec;
  /** The issues each bar counts: its window figure of `statistic`, and the rows `keep` keeps of
   *  the role's. */
  readonly links?: {
    readonly statistic: string;
    readonly keep: (role: string) => (row: EvalItemRow) => boolean;
  };
}

/** The six Legion evals, in order (dispatch://INFRA-2918/artifact/plan-md, D7): work time and
 *  rework a day at a time, each day's row linking to the issues it counts; phase time one bar per
 *  role split into its segments, each bar linking to the issues holding hours on it; idle time one
 *  bar per role; slot wait; and the issues Legion sessions file and finish. */
export const LEGION_CHARTS: readonly (DayChart | RoleChart)[] = [
  {
    by: "day",
    kind: "spread",
    // An issue with no first worker event has no work time and is in no figure of it.
    links: { keep: (row) => row.cols?.started_at != null, series: "hours", statistic: "n" },
    renderer: PerDaySpread,
    series: [
      {
        name: "hours",
        spec: {
          headline: ["median", "p90", "n"],
          label: "Work time",
          noun: "issues",
          statistics: [MEDIAN, P90, ISSUES_N],
        },
      },
    ],
    slug: "legion-work-time",
    title: "Work time",
  },
  {
    by: "role",
    // A bar counts the issues holding hours on it.
    links: {
      keep: (role) => (row) => Number(row.cols?.[`bar_${role}`] ?? 0) > 0,
      statistic: "issues",
    },
    roles: [...ROLES, NO_ROLE],
    slug: "legion-phase-time",
    spec: {
      headline: [],
      label: "Phase time",
      statistics: [
        { id: "hours", label: "Hours" },
        ISSUES,
        { id: "median_per_issue", label: "Median per issue" },
        ...PHASE_SEGMENTS,
      ],
    },
    title: "Phase time by role",
  },
  {
    by: "role",
    roles: ROLES,
    slug: "legion-idle-time",
    spec: {
      headline: [],
      label: "Idle time",
      statistics: [{ id: "hours", label: "Idle hours" }, MEDIAN, P90, ISSUES_N],
    },
    title: "Idle time by role",
  },
  {
    by: "day",
    kind: "spread",
    renderer: PerDaySpread,
    series: [
      {
        name: "hours",
        spec: {
          headline: ["median", "p90", "n"],
          label: "Slot wait",
          noun: "issues",
          statistics: [MEDIAN, P90, { id: "hours", label: "Hours" }, ISSUES_N],
        },
      },
    ],
    slug: "legion-slot-wait",
    title: "Slot wait",
  },
  {
    by: "day",
    kind: "bars",
    links: { series: "review_rounds", statistic: "n" },
    renderer: PerDayBars,
    series: [
      {
        name: "review_rounds",
        spec: {
          headline: ["median", "n"],
          label: "Review rounds",
          statistics: [MEDIAN, P90, ISSUES_N],
        },
      },
      {
        name: "sent_back",
        spec: {
          headline: ["share", "sent_back"],
          label: "Sent back",
          statistics: [
            { id: "share", label: "Share" },
            { id: "sent_back", label: "Issues sent back" },
            ISSUES_N,
          ],
        },
      },
      {
        name: "hours_after_first_review",
        spec: {
          headline: ["median"],
          label: "After the first review",
          statistics: [MEDIAN, P90, { id: "hours", label: "Hours" }, ISSUES_N],
        },
      },
    ],
    slug: "legion-rework",
    title: "Rework",
  },
  {
    by: "day",
    kind: "bars",
    renderer: PerDayBars,
    series: [
      issuesSeries("filed:in_tree_at_design", "Filed in the tree at design"),
      issuesSeries("filed:in_tree_later", "Filed in the tree later"),
      issuesSeries("filed:out_of_tree", "Filed out of the tree"),
      issuesSeries("filed:by_the_controller", "Filed by the controller"),
      issuesSeries("finished", "Finished"),
      {
        name: "roots",
        spec: {
          headline: ["issues", "children_per_root"],
          label: "Roots finished",
          statistics: [
            { id: "issues", label: "Roots" },
            { id: "children", label: "Children" },
            { id: "children_per_root", label: "Children per root" },
          ],
        },
      },
      {
        name: "hours_with_children",
        spec: {
          headline: ["median"],
          label: "Root work time with children",
          statistics: [MEDIAN, P90, ROOTS_N],
        },
      },
      {
        name: "hours_without_children",
        spec: {
          headline: ["median"],
          label: "Root work time without children",
          statistics: [MEDIAN, P90, ROOTS_N],
        },
      },
    ],
    slug: "legion-splitting",
    title: "Splitting and follow-ups",
  },
];

/** The UTC day after `day` (YYYY-MM-DD), at its midnight: where a day's issues end. */
function nextMidnight(day: string): string {
  return `${new Date(Date.parse(`${day}T00:00:00Z`) + DAY_MS).toISOString().slice(0, 10)}T00:00:00Z`;
}

/** What one Legion chart reads and links by: the project, the repositories every number folds,
 *  the cut, and the answer whose window its links name. */
interface ChartScope {
  readonly answer: EvalListResponse;
  readonly cut: Cut;
  readonly project: string;
  readonly repos: readonly string[];
}

/** The words a group link names its issues by in the issue table: the chart, the days, the cut. */
function groupWords(title: string, days: string, cut: Cut): string {
  return `${title}, ${days}${cut === "all" ? "" : `, ${cut}`}`;
}

/** A chart's model in the scope's cut, and its links: the headline's and each table row's. */
function useLegionChart(
  chart: DayChart | RoleChart,
  item: EvalListItem,
  scope: ChartScope
): { model: ChartModel | null; headline: ReactNode; rowLink: RowLink | undefined } {
  const { answer, cut, project, repos } = scope;
  const model = useMemo(
    () =>
      chart.by === "day"
        ? dayChartModel(
            answer,
            item,
            { kind: chart.kind, name: chart.slug, title: chart.title },
            chart.series.map((series) => ({ ...series, name: inCut(series.name, cut) })),
            Date.now()
          )
        : categoryChartModel(
            answer,
            item,
            {
              axis: { label: "Role", many: "roles", one: "role" },
              kind: "roles",
              name: chart.slug,
              series: inCut("roles", cut),
              title: chart.title,
            },
            chart.roles.map((role) => ({ ...role, name: inCut(role.name, cut) })),
            chart.spec,
            Date.now()
          ),
    [answer, chart, cut, item]
  );
  const window = `${utcDate(answer.window.from)} to ${lastUtcDate(answer.window.to)}`;
  return useMemo(() => {
    const none = { headline: null, model, rowLink: undefined };
    if (model === null) return none;
    if (chart.by === "role") {
      const roleLinks = chart.links;
      const series = model.series[0];
      if (roleLinks === undefined || series === undefined) return none;
      const counts = new Map(
        series.points.map((point) => [point.x, point.values[roleLinks.statistic]?.value ?? 0])
      );
      return {
        headline: null,
        model,
        rowLink: {
          cell: (x: string) => {
            const role = chart.roles.find((candidate) => inCut(candidate.name, cut) === x);
            if (role === undefined) return null;
            return (
              <IssuesLink
                count={counts.get(x) ?? 0}
                label={groupWords(`${chart.title}: ${role.label}`, window, cut)}
                project={project}
                read={() =>
                  readIssueKeys(
                    project,
                    chart.slug,
                    { from: answer.window.from, repo: repos, series: x, to: answer.window.to },
                    roleLinks.keep(role.name)
                  )
                }
              />
            );
          },
          header: "Open",
        },
      };
    }
    const dayLinks = chart.links;
    const name = inCut(dayLinks?.series ?? "", cut);
    const series = model.series.find((candidate) => candidate.name === name);
    if (dayLinks === undefined || series === undefined) return none;
    const counts = new Map(
      series.points.map((point) => [point.x, point.values[dayLinks.statistic]?.value ?? 0])
    );
    const read = (from: string, to: string) => () =>
      readIssueKeys(project, chart.slug, { from, repo: repos, series: name, to }, dayLinks.keep);
    return {
      headline: (
        <IssuesLink
          count={series.window.values[dayLinks.statistic]?.value ?? 0}
          label={groupWords(chart.title, window, cut)}
          project={project}
          read={read(answer.window.from, answer.window.to)}
        />
      ),
      model,
      rowLink: {
        cell: (day: string) => (
          <IssuesLink
            count={counts.get(day) ?? 0}
            label={groupWords(chart.title, day, cut)}
            project={project}
            read={read(`${day}T00:00:00Z`, nextMidnight(day))}
          />
        ),
        header: "Open",
      },
    };
  }, [answer, chart, cut, model, project, repos, window]);
}

/** One Legion eval's chart, in a box of its own naming the eval (`data-eval`): "no data" where
 *  the view holds none of its series, and its headline's link to the issues it counts under it. */
function LegionChart({
  chart,
  item,
  scope,
}: {
  chart: DayChart | RoleChart;
  item: EvalListItem;
  scope: ChartScope;
}): ReactNode {
  const { headline, model, rowLink } = useLegionChart(chart, item, scope);
  return (
    <article
      aria-label={chart.title}
      className={`flex min-w-0 flex-col gap-1 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-eval={chart.slug}
    >
      {model === null ? (
        <>
          <span className={`text-xs ${textMutedOnSurface}`}>{chart.title}</span>
          <p className={`text-sm ${textMutedOnSurface}`}>no data</p>
        </>
      ) : (
        <EvalChart
          model={model}
          opensPanel={chart.slug}
          renderer={chart.by === "day" ? chart.renderer : CategoryBars}
          rowLink={rowLink}
        />
      )}
      {headline === null ? null : <div>{headline}</div>}
    </article>
  );
}

/**
 * The Legion measures (dispatch://INFRA-2918/artifact/plan-md, D7, D13): the six Legion evals of
 * the Legion view, each an `EvalChart`, over the issues of one cut (every issue, the roots, or the
 * children), which the reader picks. Work time's and rework's tables hold one row per UTC day of
 * the window. Each number that counts a group of issues links to the issue table on exactly those
 * issues (`IssuesLink`): work time's and rework's window figures and each of their days, and each
 * bar of phase time. The tab lists no issue itself (spec R8).
 */
export function LegionSection({
  project,
  repos,
  views,
}: {
  project: string;
  /** The repositories every number folds, which the links read the rows of. */
  repos: readonly string[];
  views: readonly EvalsViewState[];
}): ReactNode {
  const cutId = useId();
  const [cut, setCut] = useState<Cut>("all");
  const view = views.find((candidate) => candidate.kind === "legion");
  const answer = view?.answer;
  if (view === undefined || answer === undefined) return null;
  const scope: ChartScope = { answer, cut, project, repos };
  const drawn = LEGION_CHARTS.flatMap((chart) => {
    const item = view.items.find((candidate) => candidate.definition.slug === chart.slug);
    return item === undefined ? [] : [{ chart, item }];
  });
  return (
    <div className="flex flex-col gap-2">
      <div className={`flex items-center gap-2 text-sm ${textMutedOnCanvas}`}>
        <label htmlFor={cutId}>Legion issues</label>
        <select
          className={`rounded border px-2 py-1 text-sm ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`}
          id={cutId}
          onChange={(event) => setCut(event.target.value as Cut)}
          value={cut}
        >
          {CUTS.map(({ label, value }) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </select>
      </div>
      {drawn.length === 0 ? (
        <p className={`text-sm ${textMutedOnCanvas}`}>no data</p>
      ) : (
        <div className="@container">
          <div className="grid grid-cols-1 gap-2 @min-[42rem]:grid-cols-2">
            {drawn.map(({ chart, item }) => (
              <LegionChart chart={chart} item={item} key={chart.slug} scope={scope} />
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
