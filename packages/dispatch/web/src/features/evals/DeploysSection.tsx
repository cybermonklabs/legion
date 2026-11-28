import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { evalDetailQuery } from "../../api/queries";
import type { EvalListItem, EvalListResponse } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import {
  type ChartModel,
  type ChartRenderer,
  type ChartStatistic,
  dayChartModel,
  type SeriesSpec,
} from "./chart-model";
import { PerDayBars, PerDaySpread } from "./chart-renderers";
import { EvalChart } from "./EvalChart";
import { NarrowedChart, narrowingOf } from "./narrowing";
import type { EvalsViewFacets, EvalsViewState } from "./useEvalsViews";

const MEDIAN: ChartStatistic = { id: "median", label: "Median" };
const P90: ChartStatistic = { id: "p90", label: "p90" };

/** A lead time's `minutes`, a day at a time and over the window: its median, p90, longest and
 *  how many pull requests it times. */
const LEAD_TIME: SeriesSpec = {
  headline: ["median", "p90", "n"],
  label: "Minutes",
  noun: "PRs",
  statistics: [MEDIAN, P90, { id: "max", label: "Longest" }, { id: "n", label: "PRs" }],
};

/** One chart of the section: the eval it draws, its renderer, and the series it draws with what
 *  its table holds of it. */
interface DeployChart {
  readonly slug: string;
  readonly title: string;
  readonly kind: string;
  readonly renderer: ChartRenderer;
  readonly series: string;
  readonly spec: SeriesSpec;
}

/** The ten delivery charts, in order (dispatch://INFRA-2918/artifact/plan-md, D7): the deploys,
 *  the four lead times, the rework share, and the three failure measures, whose builders read
 *  "no data" until they land. Each draws its eval's listed series as the delivery view answers
 *  it. */
export const DEPLOY_CHARTS: readonly DeployChart[] = [
  {
    kind: "bars",
    renderer: PerDayBars,
    series: "deploys",
    slug: "deploy-frequency",
    spec: {
      headline: ["per_day", "deploys", "with_prs"],
      label: "Deploys",
      statistics: [
        { id: "per_day", label: "Deploys a day" },
        { id: "deploys", label: "Deploys" },
        { id: "with_prs", label: "With PRs" },
        { id: "with_prs_per_day", label: "With PRs a day" },
      ],
    },
    title: "Deploy frequency",
  },
  {
    kind: "bars",
    renderer: PerDayBars,
    series: "runs",
    slug: "deploy-run-success",
    spec: {
      headline: ["rate", "reached_production", "concluded"],
      label: "Deploy runs",
      statistics: [
        { id: "rate", label: "Success" },
        { id: "reached_production", label: "Reached production" },
        { id: "concluded", label: "Concluded" },
        { id: "cancelled", label: "Cancelled" },
      ],
    },
    title: "Deploy runs reaching production",
  },
  {
    kind: "spread",
    renderer: PerDaySpread,
    series: "minutes",
    slug: "merge-to-production",
    spec: LEAD_TIME,
    title: "Merge \u2192 production",
  },
  {
    kind: "spread",
    renderer: PerDaySpread,
    series: "minutes",
    slug: "first-commit-to-production",
    spec: LEAD_TIME,
    title: "First commit \u2192 production",
  },
  {
    kind: "spread",
    renderer: PerDaySpread,
    series: "minutes",
    slug: "open-to-production",
    spec: LEAD_TIME,
    title: "PR open \u2192 production",
  },
  {
    kind: "spread",
    renderer: PerDaySpread,
    series: "minutes",
    slug: "pr-open-to-merge",
    spec: LEAD_TIME,
    title: "PR open \u2192 merge",
  },
  {
    kind: "bars",
    renderer: PerDayBars,
    series: "prs",
    slug: "rework-share",
    spec: { headline: ["rate"], label: "PRs", statistics: [{ id: "rate", label: "Rework share" }] },
    title: "Rework share",
  },
  {
    kind: "bars",
    renderer: PerDayBars,
    series: "per_deploy",
    slug: "change-fail-rate",
    spec: {
      headline: ["rate", "confirmed", "total"],
      label: "Per deploy",
      statistics: [
        { id: "rate", label: "Failure rate" },
        { id: "confirmed", label: "Confirmed" },
        { id: "pending", label: "Pending" },
        { id: "rejected", label: "Rejected" },
        { id: "total", label: "Deploys" },
      ],
    },
    title: "Change failure rate",
  },
  {
    kind: "bars",
    renderer: PerDayBars,
    series: "minutes",
    slug: "failed-deploy-recovery",
    spec: {
      headline: ["median", "not_restored"],
      label: "Minutes",
      statistics: [MEDIAN, { id: "not_restored", label: "Not restored" }],
    },
    title: "Failed deploy recovery",
  },
  {
    kind: "bars",
    renderer: PerDayBars,
    series: "deploys",
    slug: "deployment-rework-rate",
    spec: {
      headline: ["rate", "restoring", "total"],
      label: "Deploys",
      statistics: [
        { id: "rate", label: "Rework rate" },
        { id: "restoring", label: "Restoring deploys" },
        { id: "total", label: "Deploys" },
      ],
    },
    title: "Deployment rework rate",
  },
];

/** The eval's method under its chart (plan D12): closed on the first paint, so the tab reads no
 *  detail then; opened, the eval's detail read at the version the view answers (`doc_text`, word
 *  for word), read again when a register write moves the version (D11a). */
function Method({ project, item }: { project: string; item: EvalListItem }): ReactNode {
  const [open, setOpen] = useState(false);
  const detail = useQuery({
    ...evalDetailQuery(project, item.definition.slug, item.definition.version),
    enabled: open,
  });
  return (
    <div className="flex flex-col text-xs" data-method={item.definition.slug}>
      <DisclosureToggle
        expanded={open}
        label="Method"
        onToggle={() => setOpen(!open)}
        textClassName={textSecondaryOnSurface}
      />
      {!open ? null : detail.isError ? (
        <QueryError
          error={detail.error}
          message="Could not read the method."
          onRetry={() => void detail.refetch()}
        />
      ) : detail.data === undefined ? (
        <p className={textMutedOnSurface} role="status">
          Reading the method…
        </p>
      ) : detail.data.doc_text === null ? (
        <p className={textMutedOnSurface}>No method is written for this eval.</p>
      ) : (
        <p className={`whitespace-pre-wrap ${textPrimaryOnSurface}`} data-method-text>
          {detail.data.doc_text}
        </p>
      )}
    </div>
  );
}

/** One delivery eval's chart, in a box naming the eval (`data-eval`), "no data" where the view
 *  holds no row of its series, what it says of the facets set (`narrowingOf`), and its method
 *  under it. */
function DeployChartBox({
  answer,
  chart,
  facets,
  item,
  project,
}: {
  answer: EvalListResponse;
  chart: DeployChart;
  facets: EvalsViewFacets | undefined;
  item: EvalListItem;
  project: string;
}): ReactNode {
  const model = useMemo(
    (): ChartModel | null =>
      dayChartModel(
        answer,
        item,
        { kind: chart.kind, name: chart.slug, title: chart.title },
        [{ name: chart.series, spec: chart.spec }],
        Date.now()
      ),
    [answer, chart, item]
  );
  const narrowing = narrowingOf(
    facets,
    chart.slug,
    item.series?.find((series) => series.series === chart.series)
  );
  return (
    <article
      aria-label={chart.title}
      className={`flex min-w-0 flex-col gap-1 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-eval={chart.slug}
    >
      <NarrowedChart narrowing={narrowing}>
        {model === null ? (
          <>
            <span className={`text-xs ${textMutedOnSurface}`}>{chart.title}</span>
            <p className={`text-sm ${textMutedOnSurface}`}>no data</p>
          </>
        ) : (
          <EvalChart model={model} opensPanel={chart.slug} renderer={chart.renderer} />
        )}
      </NarrowedChart>
      <Method item={item} project={project} />
    </article>
  );
}

/**
 * Deploys and lead time (dispatch://INFRA-2918/artifact/plan-md, D7, D12; spec R15): the ten
 * delivery charts of the delivery view, the view the target cards read, in order, each an
 * `EvalChart` with the default-repository note beside a figure that counts such rows, and its
 * eval's method under it. An eval the view does not hold draws no chart. While a facet or the
 * search is set, the five pull-request charts draw the narrowed view's values and name the facets
 * applied, under a loading mark while the values are still the unnarrowed view's, and the deploy,
 * run and failure charts read "window and repository only" (plan D3).
 */
export function DeploysSection({
  project,
  views,
}: {
  project: string;
  views: readonly EvalsViewState[];
}): ReactNode {
  const view = views.find((candidate) => candidate.kind === "delivery");
  const answer = view?.answer;
  if (view === undefined || answer === undefined) return null;
  const drawn = DEPLOY_CHARTS.flatMap((chart) => {
    const item = view.items.find((candidate) => candidate.definition.slug === chart.slug);
    return item === undefined ? [] : [{ chart, item }];
  });
  if (drawn.length === 0) return <p className={`text-sm ${textMutedOnCanvas}`}>no data</p>;
  return (
    <div className="@container">
      <div className="grid grid-cols-1 gap-2 @min-[42rem]:grid-cols-2">
        {drawn.map(({ chart, item }) => (
          <DeployChartBox
            answer={answer}
            chart={chart}
            facets={view.facets}
            item={item}
            key={chart.slug}
            project={project}
          />
        ))}
      </div>
    </div>
  );
}
