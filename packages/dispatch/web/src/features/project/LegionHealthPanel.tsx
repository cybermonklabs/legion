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
  LegionMetricsResponse,
  LegionMetricsScope,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  borderStrong,
  chartParts,
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
import { formatDuration, formatRate, pct } from "../delivery/lib/format";
import type { WindowChoice } from "../delivery/lib/window";
import { type SegmentedOption, SegmentedToggle } from "../delivery/SegmentedToggle";
import { WindowControl } from "../delivery/WindowControl";
import {
  formatHours,
  LegionCard,
  type LegionMetricKey,
  metricsRange,
  roleName,
} from "./LegionMetrics";

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

/** Which card is open under the tile row. */
type CardKey = LegionMetricKey | "ci" | "deploy";

/** A number's change against the compare range, signed, in the number's own format; empty while
 *  either is unknown. */
function delta(
  now: number | null | undefined,
  before: number | null | undefined,
  format: (v: number) => string
): string {
  if (now === null || now === undefined || before === null || before === undefined) return "";
  const change = now - before;
  if (change === 0) return "no change";
  return `${change > 0 ? "+" : "−"}${format(Math.abs(change))}`;
}

/** Filings per week over a report's range. */
function filedPerWeek(report: LegionMetricsResponse): number {
  const weeks = (Date.parse(report.to) - Date.parse(report.from)) / (7 * 24 * 3600 * 1000);
  return weeks > 0 ? report.splitting.filed.length / weeks : 0;
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
  const [card, setCard] = useState<CardKey | null>(null);
  const [chosen, setChosen] = useState<WindowChoice>({ kind: "preset", preset: "7d" });
  const [compare, setCompare] = useState<ChartRange | null>(null);
  const [scope, setScope] = useState<LegionMetricsScope>("all");
  // The window as two instants, resolved when the choice changes: a preset's end is the minute
  // it was resolved in, so a re-render reads no new range.
  const range = useMemo(
    () => metricsRange(chosen, Math.floor(Date.now() / 60_000) * 60_000),
    [chosen]
  );
  const against = compare ?? range;

  const legionSettings = useQuery(legionSettingsQuery());
  const hasLegion = (legionSettings.data ?? []).some((setting) => setting.project === project);
  const metrics = useQuery({
    ...legionMetricsQuery({ from: range.from, project, scope, to: range.to }),
    enabled: open && hasLegion,
  });
  const comparedMetrics = useQuery({
    ...legionMetricsQuery({ from: against.from, project, scope, to: against.to }),
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
    ...projectPipelineQuery({ from: against.from, repo: repos, to: against.to }),
    enabled: open && measured && compare !== null,
  });
  const measures = useQuery({
    ...projectMeasuresQuery({ from: range.from, repo: repos, to: range.to }),
    enabled: open && measured,
  });
  const comparedMeasures = useQuery({
    ...projectMeasuresQuery({ from: against.from, repo: repos, to: against.to }),
    enabled: open && measured && compare !== null,
  });
  const source = <T,>(data: T | undefined): ChartCompareSource<T> => ({
    data,
    onRange: setCompare,
    range: compare,
  });
  const report = metrics.data;
  const before = compare === null ? undefined : comparedMetrics.data;
  const ciMeasured = measured && !notMeasured(pipeline.error);
  const deployMeasured = measured && !notMeasured(measures.error);
  const legionNote = legionSettings.isPending
    ? "Loading…"
    : !hasLegion
      ? "No Legion daemon"
      : metrics.isError
        ? "Couldn't load"
        : "Loading…";
  const ci = pipeline.data?.pr_checks.run.total;
  const ciBefore = compare === null ? undefined : comparedPipeline.data?.pr_checks.run.total;
  const deploys = measures.data?.measures;
  const deploysBefore = compare === null ? undefined : comparedMeasures.data?.measures;
  const toggle = (key: CardKey) => setCard(card === key ? null : key);

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
          {open ? "▾" : "▸"} {project} health
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
          <fieldset
            aria-label={`${project} health tiles`}
            className="grid grid-cols-2 gap-2 md:grid-cols-3 xl:grid-cols-6"
          >
            <Tile
              card="work_time"
              change={delta(
                report?.work_time.median_hours,
                before?.work_time.median_hours,
                formatHours
              )}
              name="Work time to merge"
              note={report === undefined ? legionNote : undefined}
              onToggle={toggle}
              open={card}
            >
              {report === undefined ? null : (
                <>
                  <TileNumber label="median" value={formatHours(report.work_time.median_hours)} />
                  <TileNumber label="p90" value={formatHours(report.work_time.p90_hours)} />
                </>
              )}
            </Tile>
            <Tile
              card="phase_time"
              change={delta(report?.phase_time.idle_share, before?.phase_time.idle_share, (v) =>
                pct(v)
              )}
              name="Time per worker type"
              note={report === undefined ? legionNote : undefined}
              onToggle={toggle}
              open={card}
            >
              {report === undefined ? null : <WorkerBar report={report} />}
            </Tile>
            <Tile
              card="rework"
              change={delta(
                report?.rework.median_review_rounds,
                before?.rework.median_review_rounds,
                (v) => v.toFixed(1)
              )}
              name="Rework"
              note={report === undefined ? legionNote : undefined}
              onToggle={toggle}
              open={card}
            >
              {report === undefined ? null : (
                <>
                  <TileNumber
                    label="rounds"
                    value={report.rework.median_review_rounds.toFixed(1)}
                  />
                  <TileNumber label="sent back" value={pct(report.rework.sent_back_share, 0)} />
                </>
              )}
            </Tile>
            <Tile
              card="splitting"
              change={delta(report && filedPerWeek(report), before && filedPerWeek(before), (v) =>
                v.toFixed(0)
              )}
              name="Splitting"
              note={report === undefined ? legionNote : undefined}
              onToggle={toggle}
              open={card}
            >
              {report === undefined ? null : (
                <>
                  <TileNumber label="filed/wk" value={filedPerWeek(report).toFixed(0)} />
                  <TileNumber
                    label="per root"
                    value={report.splitting.children_per_root.toFixed(1)}
                  />
                </>
              )}
            </Tile>
            <Tile
              card="ci"
              change={delta(ci?.median_minutes, ciBefore?.median_minutes, formatDuration)}
              name="PR CI"
              note={
                loadingDelivery || (measured && pipeline.isPending)
                  ? "Loading…"
                  : !ciMeasured
                    ? "Not measured yet"
                    : pipeline.isError
                      ? "Couldn't load"
                      : undefined
              }
              onToggle={toggle}
              open={card}
            >
              {ci === undefined ? null : (
                <>
                  <TileNumber label="median run" value={formatDuration(ci.median_minutes)} />
                  <TileNumber label="success" value={formatRate(ci.success_rate)} />
                </>
              )}
            </Tile>
            <Tile
              card="deploy"
              change={delta(
                deploys?.deploy_frequency.per_day,
                deploysBefore?.deploy_frequency.per_day,
                (v) => v.toFixed(1)
              )}
              name="Deploys"
              note={
                loadingDelivery || (measured && measures.isPending)
                  ? "Loading…"
                  : !deployMeasured
                    ? "Not measured yet"
                    : measures.isError
                      ? "Couldn't load"
                      : undefined
              }
              onToggle={toggle}
              open={card}
            >
              {deploys === undefined ? null : (
                <>
                  <TileNumber label="a day" value={deploys.deploy_frequency.per_day.toFixed(1)} />
                  <TileNumber
                    label="run success"
                    value={formatRate(deploys.deploy_run_success.rate)}
                  />
                </>
              )}
            </Tile>
          </fieldset>
          {card === null ? null : card === "ci" || card === "deploy" ? (
            <DeliveryCard
              failed={
                card === "ci" ? pipeline.isError && ciMeasured : measures.isError && deployMeasured
              }
              loading={
                loadingDelivery ||
                (measured && (card === "ci" ? pipeline.isPending : measures.isPending))
              }
              measured={card === "ci" ? ciMeasured : deployMeasured}
              metric={card}
              none={
                card === "ci"
                  ? "No CI measured for this project yet."
                  : "No deploy measured for this project yet."
              }
              onRetry={() => void (card === "ci" ? pipeline.refetch() : measures.refetch())}
              title={card === "ci" ? "PR CI" : "Deploys"}
            >
              {card === "ci" && pipeline.data !== undefined ? (
                <CiChart compare={source(comparedPipeline.data)} pipeline={pipeline.data} />
              ) : card === "deploy" && measures.data !== undefined ? (
                <DeployChart compare={source(comparedMeasures.data)} measures={measures.data} />
              ) : null}
            </DeliveryCard>
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
          ) : report === undefined ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>Loading the Legion metrics…</p>
          ) : (
            <LegionCard
              compare={source(comparedMetrics.data)}
              metric={card}
              onSelectIssues={onSelectIssues}
              report={report}
            />
          )}
        </div>
      ) : null}
    </section>
  );
}

/** One headline tile: the metric's name, its numbers, and their change against the compare
 *  range. A click opens its card under the row, and a second click closes it. */
function Tile({
  card,
  change,
  children,
  name,
  note,
  onToggle,
  open,
}: {
  card: CardKey;
  change: string;
  children: ReactNode;
  name: string;
  note: string | undefined;
  onToggle: (key: CardKey) => void;
  open: CardKey | null;
}): ReactNode {
  const pressed = open === card;
  return (
    <button
      aria-expanded={pressed}
      className={`flex h-20 min-w-0 flex-col justify-between rounded border px-2 py-1.5 text-left ${surfaceBg} ${pressed ? `border-2 ${borderStrong}` : borderDefault}`}
      data-health-tile={card}
      onClick={() => onToggle(card)}
      type="button"
    >
      <span className={`truncate text-xs font-medium ${textSecondaryOnCanvas}`}>{name}</span>
      {note === undefined ? (
        <span className="flex min-w-0 flex-wrap items-baseline gap-x-2 overflow-hidden">
          {children}
        </span>
      ) : (
        <span className={`text-xs ${textMutedOnSurface}`} data-health-tile-note>
          {note}
        </span>
      )}
      <span className={`truncate text-[11px] ${textMutedOnSurface}`} data-health-tile-change>
        {change === "" ? "\u00a0" : `vs compare ${change}`}
      </span>
    </button>
  );
}

/** One number of a tile, with its few words. */
function TileNumber({ label, value }: { label: string; value: string }): ReactNode {
  return (
    <span className="flex items-baseline gap-1 whitespace-nowrap" data-health-number={label}>
      <span className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>{value}</span>
      <span className={`text-[10px] ${textMutedOnSurface}`}>{label}</span>
    </span>
  );
}

/** The worker-type tile: one small bar of the seven bars' hours, each part a worker type, and the
 *  idle share, or that no observation exists to judge it by. */
function WorkerBar({ report }: { report: LegionMetricsResponse }): ReactNode {
  const bars = report.phase_time.bars;
  const total = bars.reduce((sum, bar) => sum + bar.hours, 0);
  const idle = report.phase_time.idle_share;
  return (
    <span className="flex w-full min-w-0 flex-col gap-0.5">
      <span className="flex h-2.5 w-full overflow-hidden rounded-sm" data-health-worker-bar>
        {bars.map((bar, index) =>
          bar.hours === 0 || total === 0 ? null : (
            <span
              className={chartParts[index % chartParts.length]}
              key={bar.role}
              style={{ width: `${(100 * bar.hours) / total}%` }}
              title={`${roleName(bar.role)}: ${formatHours(bar.hours)}`}
            />
          )
        )}
      </span>
      <span className={`truncate text-[11px] ${textMutedOnSurface}`} data-health-idle>
        {idle === null ? "idle: not observed yet" : `idle ${pct(idle, 0)}`}
      </span>
    </span>
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
