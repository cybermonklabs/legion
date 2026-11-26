import { useQuery } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { type ReactNode, useMemo } from "react";

import { type ItemsViewParams, itemsViewQuery } from "../../api/queries";
import type { EvalItemRow, EvalListItem, EvalListResponse } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  dangerText,
  linkText,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { formatDuration, formatRate } from "../delivery/lib/format";
import { formatAge } from "../delivery/lib/freshness";
import { lastUtcDate } from "../delivery/lib/window";
import { type ChartModel, type ChartStatistic, dayChartModel } from "./chart-model";
import { Timing } from "./chart-renderers";
import { EvalChart } from "./EvalChart";
import { EvalTable } from "./EvalTable";
import type { EvalsScope, EvalsViewState } from "./useEvalsViews";

/** What a CI eval's timing states of each day and of the window (measures.FoldTiming), in its
 *  table's columns; the figures beside the chart are all of them. */
const TIMING: readonly ChartStatistic[] = [
  { id: "median", label: "Median" },
  { id: "p90", label: "p90" },
  { id: "n", label: "Runs" },
  { id: "success_rate", label: "Success" },
  { id: "success", label: "Succeeded" },
  { id: "failure", label: "Failed" },
  { id: "cancelled", label: "Cancelled" },
];

/** The four CI evals, in order, each drawn as a timing chart of its `minutes` series, as the
 *  Pipeline page drew its four timing cards. */
export const CI_CHARTS: readonly { readonly slug: string; readonly title: string }[] = [
  { slug: "pr-ci", title: "PR CI" },
  { slug: "staging-e2e", title: "Staging end-to-end gate" },
  { slug: "staging-deploy", title: "Deploy to staging" },
  { slug: "production-deploy", title: "Deploy to production" },
];

/** The eval whose job rows the two job lists read. */
const PR_CI = "pr-ci";
/** How many of the slowest jobs the list holds, as the Pipeline page's did
 *  (measures.SlowestJobs). */
export const SLOWEST_JOBS = 5;
/** How many timed results a job needs in the window to be ranked among the slowest: pr-ci's job
 *  series' `min_n` as the catalogue registers it (measures.SlowestMinResults), which the items
 *  route ranks by. The list read answers listed series alone, and that series is unlisted, so the
 *  page names the catalogue's floor. */
export const SLOWEST_MIN_RESULTS = 50;
/** As many jobs as one items page holds: every job that failed in the 24 hours. */
const FAILED_JOBS_LIMIT = 200;

/** A number a job row's column holds, null where it holds none. */
function numberOf(row: EvalItemRow, column: string): number | null {
  const value = row.cols?.[column];
  return typeof value === "number" ? value : null;
}

/** A text a job row's column holds, "" where it holds none. */
function textOf(row: EvalItemRow, column: string): string {
  const value = row.cols?.[column];
  return typeof value === "string" ? value : "";
}

/** The slowest jobs' columns: each job's figures over the window (measures.FoldTiming). */
const SLOWEST_COLUMNS: ColumnDef<EvalItemRow>[] = [
  {
    accessorFn: (row) => row.row_key,
    enableColumnFilter: false,
    header: "Job",
    id: "job",
  },
  {
    accessorFn: (row) => numberOf(row, "median_minutes") ?? undefined,
    cell: ({ row }) => formatDuration(numberOf(row.original, "median_minutes")),
    enableColumnFilter: false,
    header: "Median",
    id: "median",
    sortUndefined: "last",
  },
  {
    accessorFn: (row) => numberOf(row, "p90_minutes") ?? undefined,
    cell: ({ row }) => formatDuration(numberOf(row.original, "p90_minutes")),
    enableColumnFilter: false,
    header: "p90",
    id: "p90",
    sortUndefined: "last",
  },
  {
    accessorFn: (row) => numberOf(row, "n") ?? undefined,
    enableColumnFilter: false,
    header: "Results",
    id: "n",
  },
  {
    accessorFn: (row) => numberOf(row, "success_rate") ?? undefined,
    cell: ({ row }) => formatRate(numberOf(row.original, "success_rate")),
    enableColumnFilter: false,
    header: "Success",
    id: "success_rate",
    sortUndefined: "last",
  },
];

/** The failed jobs' columns, as the Pipeline page's list had them: each job's name and the
 *  repository whose workflow ran it (a row is keyed by both, `<repo>/<name>`, since two
 *  repositories' workflows can name a job alike), its failures, the results they are of, their
 *  share, and its latest failure, linking to its run. */
function failedColumns(nowMs: number): ColumnDef<EvalItemRow>[] {
  return [
    {
      accessorFn: (row) => textOf(row, "name"),
      enableColumnFilter: false,
      header: "Job",
      id: "job",
    },
    {
      accessorFn: (row) => textOf(row, "repo"),
      enableColumnFilter: false,
      header: "Repository",
      id: "repo",
    },
    {
      accessorFn: (row) => numberOf(row, "failures") ?? undefined,
      cell: ({ row }) => (
        <span className={`font-semibold ${dangerText}`}>{numberOf(row.original, "failures")}</span>
      ),
      enableColumnFilter: false,
      header: "Failures",
      id: "failures",
    },
    {
      accessorFn: (row) => numberOf(row, "results") ?? undefined,
      enableColumnFilter: false,
      header: "Of runs",
      id: "results",
    },
    {
      accessorFn: (row) => {
        const failures = numberOf(row, "failures");
        const results = numberOf(row, "results");
        return failures === null || !results ? undefined : failures / results;
      },
      cell: ({ getValue }) => {
        const share = getValue<number | undefined>();
        return formatRate(share ?? null);
      },
      enableColumnFilter: false,
      header: "Failure share",
      id: "share",
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => textOf(row, "last_failed_at"),
      cell: ({ row }) => {
        const at = textOf(row.original, "last_failed_at");
        return (
          <a
            className={`hover:underline ${linkText}`}
            href={textOf(row.original, "last_run_url") || row.original.href}
            rel="noreferrer"
            target="_blank"
          >
            {at === "" ? "" : formatAge(Math.max(0, (nowMs - Date.parse(at)) / 1000))}
          </a>
        );
      },
      enableColumnFilter: false,
      header: "Last failure",
      id: "last_failed_at",
    },
  ];
}

/** One CI eval's timing chart, in a box naming the eval (`data-eval`), "no data" where the view
 *  holds no row of its series. */
function CiChart({
  answer,
  item,
  title,
}: {
  answer: EvalListResponse;
  item: EvalListItem;
  title: string;
}): ReactNode {
  const slug = item.definition.slug;
  const model = useMemo(
    (): ChartModel | null =>
      dayChartModel(
        answer,
        item,
        { kind: "timing", name: slug, title },
        [
          {
            name: "minutes",
            spec: {
              headline: ["median", "p90", "n", "success_rate", "success", "failure", "cancelled"],
              label: title,
              noun: "runs",
              statistics: TIMING,
            },
          },
        ],
        Date.now()
      ),
    [answer, item, slug, title]
  );
  return (
    <article
      aria-label={title}
      className={`flex min-w-0 flex-col gap-1 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-eval={slug}
    >
      {model === null ? (
        <>
          <span className={`text-xs ${textMutedOnSurface}`}>{title}</span>
          <p className={`text-sm ${textMutedOnSurface}`}>no data</p>
        </>
      ) : (
        <EvalChart model={model} opensPanel={slug} renderer={Timing} />
      )}
    </article>
  );
}

/** One of pr-ci's job lists, read through its items view (plan D9): its heading and words, then
 *  its table, or what it says while the view is out, refused, or answers no job. */
function JobList({
  columns,
  empty,
  id,
  note,
  scope,
  title,
  view,
}: {
  columns: ColumnDef<EvalItemRow>[];
  empty: string;
  id: string;
  note: string;
  scope: EvalsScope;
  title: string;
  view: Pick<ItemsViewParams, "sort" | "limit">;
}): ReactNode {
  const items = useQuery(
    itemsViewQuery({
      compare: null,
      project: scope.project,
      repos: scope.repos,
      slug: PR_CI,
      window: scope.window,
      ...view,
    })
  );
  const rows = useMemo(() => items.data?.items ?? [], [items.data]);
  return (
    <section aria-label={title} className="flex flex-col gap-1" data-job-list={id}>
      <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>{title}</h3>
      <p className={`text-xs ${textMutedOnCanvas}`}>{note}</p>
      {items.isError ? (
        <QueryError
          error={items.error}
          message={`${title}: could not load.`}
          onRetry={() => void items.refetch()}
        />
      ) : items.data === undefined ? (
        <p className={`text-xs ${textMutedOnCanvas}`} role="status">
          {title}: loading…
        </p>
      ) : rows.length === 0 ? (
        <p className={`text-xs ${textMutedOnCanvas}`}>{empty}</p>
      ) : (
        <EvalTable
          columns={columns}
          empty={empty}
          getRowId={(row) => row.row_key}
          label={title}
          layoutKey={`evals-jobs:${id}`}
          noun={{ many: "jobs", one: "job" }}
          rows={rows}
          scrollTestId={`${id}-scroll`}
        />
      )}
    </section>
  );
}

/**
 * CI health (dispatch://INFRA-2918/artifact/plan-md, D7, D9; spec R15): the four CI evals of the
 * CI view, each a timing chart (each day's median and p90 with its runs, and how often it
 * succeeded), with the default-repository note beside a figure that counts such rows; then pr-ci's
 * slowest jobs (the items view, `sort=median`, ranked over the window with the floor of timed
 * results) and the jobs that failed in the 24 hours before the window ends (`sort=failures`), each
 * a table of the one table core, as the Pipeline page listed them.
 */
export function CiHealth({
  scope,
  views,
}: {
  scope: EvalsScope;
  views: readonly EvalsViewState[];
}): ReactNode {
  const view = views.find((candidate) => candidate.kind === "ci");
  const answer = view?.answer;
  const failed = useMemo(() => failedColumns(Date.now()), []);
  if (view === undefined || answer === undefined) return null;
  const drawn = CI_CHARTS.flatMap(({ slug, title }) => {
    const item = view.items.find((candidate) => candidate.definition.slug === slug);
    return item === undefined ? [] : [{ item, title }];
  });
  const floor = `${SLOWEST_MIN_RESULTS} timed results`;
  return (
    <div className="flex flex-col gap-3">
      {drawn.length === 0 ? (
        <p className={`text-sm ${textMutedOnCanvas}`}>no data</p>
      ) : (
        <div className="@container">
          <div className="grid grid-cols-1 gap-2 @min-[42rem]:grid-cols-2">
            {drawn.map(({ item, title }) => (
              <CiChart answer={answer} item={item} key={item.id} title={title} />
            ))}
          </div>
        </div>
      )}
      <JobList
        columns={SLOWEST_COLUMNS}
        empty={`No job has ${floor} in the window yet.`}
        id="slowest-jobs"
        note={`The ${SLOWEST_JOBS} slowest by their median over the window; a job needs ${floor} in the window to be ranked.`}
        scope={scope}
        title="PR CI: slowest jobs"
        view={{ limit: SLOWEST_JOBS, sort: "median" }}
      />
      <JobList
        columns={failed}
        empty="No job failed in the 24 hours."
        id="failing-jobs"
        note={`Every job that failed in the 24 hours before the window ends (${lastUtcDate(answer.window.to)}, UTC) but the roll-up job the settings name, most failures first; a failure is often the pull request's own change.`}
        scope={scope}
        title="PR CI: jobs that failed in the last 24 hours"
        view={{ limit: FAILED_JOBS_LIMIT, sort: "failures" }}
      />
    </div>
  );
}
