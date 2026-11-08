// LegionHealthPanel (dispatch://INFRA-979 prototype): a project's own health numbers on its issues
// page, in place of the separate Delivery and Pipeline pages — the four Legion metrics of the
// project (LegionMetrics), its PR CI and its deploys, each card through `Chart` with the
// graph-or-table toggle and one compare range. Collapsible; the issues page mounts it above its
// status bar.
//
// The CI and deploy cards read `GET /api/v1/delivery/pipeline` and `GET /api/v1/delivery/measures`
// for the project's own repositories (`GET /api/v1/settings/repo-projects`). Dispatch measures one
// deploy repository (the deployment's Delivery settings); a project whose repositories do not
// include it has no CI or deploy measured yet, and its cards say so. Per-project Delivery settings
// are dispatch://INFRA-978's.
//
// `onSelectIssues` is called with the issues behind a Legion number — a table row, a segment of a
// worker type's bar, a headline count — and words naming them; the issues table filters by them.
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useState } from "react";

import { ApiError } from "../../api/client";
import {
  deliverySettingsQuery,
  legionMetricsQuery,
  legionSettingsQuery,
  projectMeasuresQuery,
  projectPipelineQuery,
  repoProjectsQuery,
} from "../../api/queries";
import type {
  DeliveryDailyPoint,
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimingDay,
  LegionMetricsScope,
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
  type ChartCompareSource,
  type ChartRange,
  type ChartSeries,
  chartCompare,
  daysRange,
} from "../delivery/Chart";
import { formatDuration, formatRate } from "../delivery/lib/format";
import type { WindowChoice } from "../delivery/lib/window";
import { type SegmentedOption, SegmentedToggle } from "../delivery/SegmentedToggle";
import { WindowControl } from "../delivery/WindowControl";
import { LegionMetrics, metricsRange } from "./LegionMetrics";

const CARD = `flex min-w-0 flex-col gap-1.5 rounded border px-3 py-2 ${surfaceBg} ${borderDefault}`;

/** The three populations the Legion cards count, in the order the toggle offers them. */
const SCOPES: readonly SegmentedOption<LegionMetricsScope>[] = [
  { value: "all", label: "All issues" },
  { value: "roots", label: "Roots" },
  { value: "children", label: "Children" },
];

/** The PR CI card's chart: each day's run time, median and p90, and that day's success share. */
function timingSeries(days: readonly DeliveryTimingDay[]): ChartSeries[] {
  return [
    { kind: "spread", days, format: formatDuration, label: "Run time", noun: "runs" },
    {
      kind: "spark",
      format: (v) => `${formatRate(v)} success`,
      label: "Success",
      max: 1,
      points: days.map((d) => ({ label: d.day, partial: d.partial, value: d.success_rate })),
    },
  ];
}

/** The deploy card's chart: each day's production deploys and its deploy-run success share. */
function deploySeries(days: readonly DeliveryDailyPoint[]): ChartSeries[] {
  return [
    {
      kind: "spark",
      format: (v) => `${v}`,
      label: "Deploys",
      max: Math.max(1, ...days.map((d) => d.deploys)),
      points: days.map((d) => ({ label: d.day, partial: d.partial, value: d.deploys })),
    },
    {
      kind: "spark",
      format: (v) => `${formatRate(v)} success`,
      label: "Run success",
      max: 1,
      points: days.map((d) => ({ label: d.day, partial: d.partial, value: d.run_success_rate })),
    },
  ];
}

/** Whether a failed read is Dispatch saying it measures nothing for these repositories. */
function notMeasured(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.code === "DELIVERY_REPO_NOT_MEASURED" || error.code === "DELIVERY_NOT_CONFIGURED")
  );
}

export function LegionHealthPanel({
  onSelectIssues,
  project,
}: {
  readonly project: string;
  readonly onSelectIssues?: (keys: string[], label: string) => void;
}): ReactNode {
  const headingId = useId();
  const [open, setOpen] = useState(true);
  const [chosen, setChosen] = useState<WindowChoice>({ kind: "preset", preset: "7d" });
  const [compare, setCompare] = useState<ChartRange | null>(null);
  const [scope, setScope] = useState<LegionMetricsScope>("all");
  // The window as two instants, resolved when the choice changes: a preset's end is the minute
  // it was resolved in, so a re-render reads no new range.
  const range = useMemo(
    () => metricsRange(chosen, Math.floor(Date.now() / 60_000) * 60_000),
    [chosen]
  );

  const legionSettings = useQuery(legionSettingsQuery());
  const hasLegion = (legionSettings.data ?? []).some((setting) => setting.project === project);
  const metrics = useQuery({
    ...legionMetricsQuery({ from: range.from, project, scope, to: range.to }),
    enabled: open && hasLegion,
  });
  const comparedMetrics = useQuery({
    ...legionMetricsQuery({
      from: (compare ?? range).from,
      project,
      scope,
      to: (compare ?? range).to,
    }),
    enabled: open && hasLegion && compare !== null,
  });

  const mappings = useQuery(repoProjectsQuery());
  const delivery = useQuery(deliverySettingsQuery());
  const repos = useMemo(
    () =>
      (mappings.data ?? [])
        .filter((mapping) => mapping.project === project)
        .map((mapping) => mapping.repo.toLowerCase()),
    [mappings.data, project]
  );
  const deployRepo = delivery.data?.deploy_repo;
  const measured = deployRepo !== undefined && repos.includes(deployRepo);
  const loadingDelivery = mappings.isPending || delivery.isPending;
  const pipeline = useQuery({
    ...projectPipelineQuery({ from: range.from, repo: repos, to: range.to }),
    enabled: open && measured,
  });
  const comparedPipeline = useQuery({
    ...projectPipelineQuery({
      from: (compare ?? range).from,
      repo: repos,
      to: (compare ?? range).to,
    }),
    enabled: open && measured && compare !== null,
  });
  const measures = useQuery({
    ...projectMeasuresQuery({ from: range.from, repo: repos, to: range.to }),
    enabled: open && measured,
  });
  const comparedMeasures = useQuery({
    ...projectMeasuresQuery({
      from: (compare ?? range).from,
      repo: repos,
      to: (compare ?? range).to,
    }),
    enabled: open && measured && compare !== null,
  });
  const source = <T,>(data: T | undefined): ChartCompareSource<T> => ({
    data,
    onRange: setCompare,
    range: compare,
  });

  return (
    <section aria-labelledby={headingId} className={`mb-3 ${CARD}`} data-legion-health={project}>
      <header className="flex flex-wrap items-center justify-between gap-2">
        <button
          aria-controls={`${headingId}-body`}
          aria-expanded={open}
          className={`text-sm font-semibold ${textPrimaryOnCanvas}`}
          id={headingId}
          onClick={() => setOpen(!open)}
          type="button"
        >
          {open ? "▾" : "▸"} {project} health: Legion, CI and deploys
        </button>
        {open ? (
          <div className="flex flex-wrap items-center gap-2">
            {hasLegion ? (
              <SegmentedToggle
                label="Legion issues"
                onChange={setScope}
                options={SCOPES}
                value={scope}
              />
            ) : null}
            <WindowControl onChange={setChosen} value={chosen} />
          </div>
        ) : null}
      </header>
      {open ? (
        <div className="flex flex-col gap-2" id={`${headingId}-body`}>
          {legionSettings.isPending ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>Loading Legion…</p>
          ) : !hasLegion ? (
            <p className={`text-sm ${textSecondaryOnCanvas}`} data-legion-health-unconfigured>
              {project} names no Legion daemon, so there are no Legion numbers. The Settings page
              names one.
            </p>
          ) : metrics.isError ? (
            <QueryError
              message="Couldn't load the Legion metrics."
              onRetry={() => void metrics.refetch()}
              retrying={metrics.isFetching}
            />
          ) : metrics.data === undefined ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>Loading the Legion metrics…</p>
          ) : (
            <LegionMetrics
              compare={source(comparedMetrics.data)}
              onSelectIssues={onSelectIssues}
              report={metrics.data}
            />
          )}
          <div className="@container">
            <div className="grid min-w-0 grid-cols-1 gap-2 @min-[56rem]:grid-cols-2">
              <DeliveryCard
                loading={loadingDelivery || (measured && pipeline.isPending)}
                measured={measured && !notMeasured(pipeline.error)}
                metric="ci"
                none="No CI measured for this project yet."
                onRetry={() => void pipeline.refetch()}
                failed={pipeline.isError && !notMeasured(pipeline.error)}
                title="PR CI"
              >
                {pipeline.data === undefined ? null : (
                  <CiChart compare={source(comparedPipeline.data)} pipeline={pipeline.data} />
                )}
              </DeliveryCard>
              <DeliveryCard
                loading={loadingDelivery || (measured && measures.isPending)}
                measured={measured && !notMeasured(measures.error)}
                metric="deploy"
                none="No deploy measured for this project yet."
                onRetry={() => void measures.refetch()}
                failed={measures.isError && !notMeasured(measures.error)}
                title="Deploys"
              >
                {measures.data === undefined ? null : (
                  <DeployChart compare={source(comparedMeasures.data)} measures={measures.data} />
                )}
              </DeliveryCard>
            </div>
          </div>
        </div>
      ) : null}
    </section>
  );
}

/** A CI or deploy card: its chart, or why there is none. */
function DeliveryCard({
  children,
  failed,
  loading,
  measured,
  metric,
  none,
  onRetry,
  title,
}: {
  children: ReactNode;
  failed: boolean;
  loading: boolean;
  measured: boolean;
  metric: string;
  none: string;
  onRetry: () => void;
  title: string;
}): ReactNode {
  return (
    <div className={CARD} data-health-card={metric}>
      <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>{title}</h3>
      {!measured && !loading ? (
        <p className={`text-sm ${textMutedOnSurface}`} data-health-unmeasured>
          {none}
        </p>
      ) : failed ? (
        <QueryError message={`Couldn't load ${title}.`} onRetry={onRetry} retrying={false} />
      ) : loading ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Loading…</p>
      ) : (
        children
      )}
    </div>
  );
}

/** One headline number of a CI or deploy card. */
function Figure({ label, value }: { label: string; value: string }): ReactNode {
  return (
    <div className="flex items-baseline gap-1.5">
      <dt className={`text-xs ${textMutedOnSurface}`}>{label}</dt>
      <dd className={`text-base font-semibold ${textPrimaryOnCanvas}`} data-health-figure={label}>
        {value}
      </dd>
    </div>
  );
}

function CiChart({
  compare,
  pipeline,
}: {
  compare: ChartCompareSource<DeliveryPipelineResponse>;
  pipeline: DeliveryPipelineResponse;
}): ReactNode {
  const series = useMemo(() => timingSeries(pipeline.pr_checks.run.days), [pipeline]);
  const compared = useMemo(
    () => (compare.data === undefined ? undefined : timingSeries(compare.data.pr_checks.run.days)),
    [compare.data]
  );
  const first = pipeline.days[0];
  const last = pipeline.days[pipeline.days.length - 1];
  if (first === undefined || last === undefined) return null;
  const total = pipeline.pr_checks.run.total;
  return (
    <>
      <dl className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <Figure label="Median run" value={formatDuration(total.median_minutes)} />
        <Figure label="p90 run" value={formatDuration(total.p90_minutes)} />
        <Figure label="Success" value={formatRate(total.success_rate)} />
        <Figure label="Runs" value={`${total.n}`} />
      </dl>
      <Chart
        compare={chartCompare(compare, compared)}
        label="PR CI"
        range={daysRange(first, last)}
        series={series}
      />
    </>
  );
}

function DeployChart({
  compare,
  measures,
}: {
  compare: ChartCompareSource<DeliveryMeasuresResponse>;
  measures: DeliveryMeasuresResponse;
}): ReactNode {
  const days = measures.measures.daily;
  const series = useMemo(() => deploySeries(days), [days]);
  const compared = useMemo(
    () => (compare.data === undefined ? undefined : deploySeries(compare.data.measures.daily)),
    [compare.data]
  );
  const first = days[0];
  const last = days[days.length - 1];
  if (first === undefined || last === undefined) return null;
  const {
    deploy_frequency: frequency,
    lead_time: lead,
    deploy_run_success: runs,
  } = measures.measures;
  return (
    <>
      <dl className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <Figure label="Deploys a day" value={frequency.per_day.toFixed(1)} />
        <Figure
          label="Merge to production, median"
          value={formatDuration(lead.merge_to_production.median_minutes)}
        />
        <Figure label="Run success" value={formatRate(runs.rate)} />
      </dl>
      <Chart
        compare={chartCompare(compare, compared)}
        label="Deploys"
        range={daysRange(first.day, last.day)}
        series={series}
      />
    </>
  );
}
