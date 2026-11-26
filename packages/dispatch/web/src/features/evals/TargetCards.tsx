import { type ReactNode, useMemo } from "react";

import type { EvalKind } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { borderDefault, linkText, surfaceBg, textMutedOnSurface } from "../../theme/classes";
import { targetLabel } from "../delivery/lib/target";
import { lastUtcDate, utcDate } from "../delivery/lib/window";
import { TargetBadge } from "../delivery/TargetBadge";
import {
  type ChartModel,
  type ChartStatistic,
  chartSeries,
  DAY_AXIS,
  primarySeries,
  targetOf,
} from "./chart-model";
import { PerDayBars, PerDaySpread } from "./chart-renderers";
import { EvalChart } from "./EvalChart";
import { openWords, readIssueKeys, useIssuesLink } from "./issue-links";
import { NarrowedChart, narrowingOf } from "./narrowing";
import type { EvalsViewState } from "./useEvalsViews";

/** One target card: the eval it draws, the view it reads, its chart, and the statistics of the
 *  eval's primary series its table holds, those of `headline` standing beside the chart. */
interface CardSpec {
  readonly slug: string;
  readonly title: string;
  readonly kind: Extract<EvalKind, "delivery" | "legion">;
  readonly chart: "bars" | "spread";
  readonly statistics: readonly ChartStatistic[];
  readonly headline: readonly string[];
  /** What a day's count counts, in a spread card's day titles. */
  readonly noun?: string;
}

const MEDIAN: ChartStatistic = { id: "median", label: "Median" };
const P90: ChartStatistic = { id: "p90", label: "p90" };

/** The unowned P0 card's eval, whose card opens the issues it counts. */
const UNOWNED_P0 = "unowned-p0";

/** The ten target cards (dispatch://INFRA-2918/artifact/plan-md, D7), in order: main's six, the
 *  two other failure measures beside the change failure rate, and the two Legion cards. Each draws
 *  its eval's primary listed series, as the evals view answers it. */
export const TARGET_CARDS: readonly CardSpec[] = [
  {
    chart: "bars",
    headline: ["per_day", "deploys"],
    kind: "delivery",
    slug: "deploy-frequency",
    statistics: [
      { id: "per_day", label: "Deploys a day" },
      { id: "deploys", label: "Deploys" },
    ],
    title: "Deploys a day",
  },
  {
    chart: "bars",
    headline: ["rate", "failed"],
    kind: "delivery",
    slug: "change-fail-rate",
    statistics: [
      { id: "rate", label: "Failure rate" },
      { id: "failed", label: "Failed deploys" },
      { id: "total", label: "Deploys" },
    ],
    title: "Change failure rate",
  },
  {
    chart: "spread",
    headline: ["median", "max"],
    kind: "delivery",
    noun: "PRs",
    slug: "merge-to-production",
    statistics: [MEDIAN, P90, { id: "max", label: "Longest" }, { id: "n", label: "PRs" }],
    title: "Merge \u2192 production",
  },
  {
    chart: "spread",
    headline: ["median", "p90"],
    kind: "delivery",
    noun: "PRs",
    slug: "pr-open-to-merge",
    statistics: [MEDIAN, P90, { id: "n", label: "PRs" }],
    title: "PR open \u2192 merge",
  },
  {
    chart: "bars",
    headline: ["rate", "reached_production", "concluded"],
    kind: "delivery",
    slug: "deploy-run-success",
    statistics: [
      { id: "rate", label: "Success" },
      { id: "reached_production", label: "Reached production" },
      { id: "concluded", label: "Concluded" },
    ],
    title: "Deploy runs reaching production",
  },
  {
    chart: "bars",
    headline: ["value"],
    kind: "delivery",
    slug: UNOWNED_P0,
    statistics: [{ id: "value", label: "Issues" }],
    title: "P0 issues with no owner",
  },
  {
    chart: "bars",
    headline: ["median", "not_restored"],
    kind: "delivery",
    slug: "failed-deploy-recovery",
    statistics: [MEDIAN, { id: "not_restored", label: "Not restored" }],
    title: "Failed deploy recovery",
  },
  {
    chart: "bars",
    headline: ["rate", "restoring"],
    kind: "delivery",
    slug: "deployment-rework-rate",
    statistics: [
      { id: "rate", label: "Rework rate" },
      { id: "restoring", label: "Restoring deploys" },
      { id: "total", label: "Deploys" },
    ],
    title: "Deployment rework rate",
  },
  {
    chart: "spread",
    headline: ["median", "p90"],
    kind: "legion",
    noun: "issues",
    slug: "legion-work-time",
    statistics: [MEDIAN, P90, { id: "n", label: "Issues" }],
    title: "Legion work time",
  },
  {
    chart: "bars",
    headline: ["median"],
    kind: "legion",
    slug: "legion-rework",
    statistics: [MEDIAN, P90, { id: "n", label: "Issues" }],
    title: "Review rounds",
  },
];

/** The words the issue table names the unowned P0 card's issues by. */
const UNOWNED_P0_LABEL = "P0 with no owner";

/** One target card: its eval's primary series as a chart (`EvalChart`), its verdict's badge
 *  beside the title, and its target. A card whose eval the view does not hold, or whose series it
 *  holds no row of, reads "no data", never 0 (spec R5). While a facet or the search is set, a card
 *  of an eval they narrow names them, or is marked loading while its values are the unnarrowed
 *  ones, and any other card reads "window and repository only" (plan D3). */
function TargetCard({
  days,
  project,
  spec,
  view,
}: {
  days: { from: string; to: string } | null;
  project: string;
  spec: CardSpec;
  view: EvalsViewState;
}): ReactNode {
  const item = view.items.find((candidate) => candidate.definition.slug === spec.slug);
  const answer = view.answer;
  const series = item === undefined ? undefined : primarySeries(item);
  const verdict = item?.verdict ?? null;
  const target = useMemo(() => targetOf(verdict), [verdict]);
  const model = useMemo((): ChartModel | null => {
    if (answer === undefined || series === undefined) return null;
    return {
      axis: DAY_AXIS,
      comparable: true,
      compareRange: answer.compare,
      kind: spec.chart,
      name: spec.slug,
      series: [
        chartSeries(
          series,
          {
            headline: spec.headline,
            label: spec.title,
            noun: spec.noun,
            statistics: spec.statistics,
          },
          { compare: answer.compare, nowMs: Date.now(), target, window: answer.window }
        ),
      ],
      title: spec.title,
    };
  }, [answer, series, spec, target]);
  // The P0 card's link: every issue key unowned-p0's rows name now, the issues the card counts,
  // so its link and its count are one set (plan D13).
  const link = useIssuesLink(
    project,
    () => readIssueKeys(project, UNOWNED_P0, {}),
    UNOWNED_P0_LABEL
  );
  const state = view.status;
  const narrowing = narrowingOf(view.facets, spec.slug, series);
  const badge =
    verdict === null ? null : (
      <TargetBadge met={verdict.met} metLabel={"\u2713 met"} missedLabel={"\u2717 missed"} />
    );
  // The issues the P0 card counts: the window's value of its series, the count its link opens.
  const unowned =
    spec.slug === UNOWNED_P0 && model !== null
      ? (model.series[0]?.window.values.value?.value ?? null)
      : null;
  return (
    <article
      aria-busy={link.state.kind === "resolving" ? true : undefined}
      aria-label={spec.title}
      className={`flex min-w-0 flex-col gap-1 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-card={spec.slug}
      data-state={state}
    >
      <NarrowedChart narrowing={narrowing}>
        {model === null ? (
          <>
            <div className="flex items-start justify-between gap-2">
              <span className={`text-xs ${textMutedOnSurface}`}>{spec.title}</span>
              {badge}
            </div>
            <p className={`text-sm ${textMutedOnSurface}`}>
              {state === "subscribed"
                ? "loading\u2026"
                : state === "failed"
                  ? "could not load"
                  : "no data"}
            </p>
          </>
        ) : (
          <EvalChart
            beside={badge}
            model={model}
            opensPanel={spec.slug}
            renderer={spec.chart === "spread" ? PerDaySpread : PerDayBars}
          />
        )}
      </NarrowedChart>
      <div className={`text-[11px] ${textMutedOnSurface}`}>
        {target === null ? "no target" : `target ${targetLabel(target)}`}
      </div>
      {days === null ? null : (
        <div className={`text-[11px] ${textMutedOnSurface}`} data-card-days>
          {`brushed days ${utcDate(days.from)} to ${lastUtcDate(days.to)}`}
        </div>
      )}
      {unowned === null || unowned === 0 ? null : (
        <div className="text-xs">
          {link.state.kind === "failed" ? (
            <QueryError
              error={link.state.error}
              message="Could not read the P0 issues with no owner."
              onRetry={link.open}
            />
          ) : (
            <button
              className={`hover:underline disabled:opacity-50 ${linkText}`}
              data-group-count={unowned}
              disabled={link.state.kind === "resolving"}
              onClick={link.open}
              type="button"
            >
              {link.state.kind === "resolving" ? "Reading the issues\u2026" : openWords(unowned)}
            </button>
          )}
        </div>
      )}
    </article>
  );
}

/**
 * The target cards (dispatch://INFRA-2918/artifact/plan-md, D7): the ten cards, each its eval's
 * primary series from the delivery or the Legion view, judged by its target (`lib/target.ts`),
 * with its change against the compare range, "no data", "no data before <day>" and the default-
 * repository note (`EvalChart`), and the P0 link to the issues it counts. Each card paints
 * as its own view lands. A card of a view the tab does not read (no Legion daemon) is not drawn,
 * and a delivery card is not drawn while `delivery` is off, though its eval is in the view (spec
 * R5). While the merge timeline's brush is set, the views are of the days it touches, which each
 * card names (`days`, decision 3).
 */
export function TargetCards({
  days = null,
  delivery,
  project,
  views,
}: {
  /** The brushed days the views read; null with no brush. */
  days?: { from: string; to: string } | null;
  delivery: boolean;
  project: string;
  views: readonly EvalsViewState[];
}): ReactNode {
  return (
    <div className="@container">
      <div className="grid grid-cols-1 gap-2 @min-[28.5rem]:grid-cols-2 @min-[42.25rem]:grid-cols-3 @min-[83.5rem]:grid-cols-5">
        {TARGET_CARDS.map((spec) => {
          const view = views.find((candidate) => candidate.kind === spec.kind);
          if (view === undefined || (spec.kind === "delivery" && !delivery)) return null;
          return (
            <TargetCard days={days} key={spec.slug} project={project} spec={spec} view={view} />
          );
        })}
      </div>
    </div>
  );
}
