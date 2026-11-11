import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

import { ApiError } from "../../api/client";
import {
  deliverySettingsQuery,
  legionMetricsQuery,
  projectMeasuresQuery,
  projectPipelineQuery,
  repoProjectsQuery,
} from "../../api/queries";
import type {
  DeliveryDailyPoint,
  DeliveryTimingDay,
  LegionMetricsResponse,
  LegionMetricsScope,
  LegionPhaseBreakdown,
} from "../../api/types";
import { InfoHint } from "../../components/InfoHint";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  filingScale,
  stageClass,
  surfaceBg,
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
  rangeLabel,
} from "../delivery/Chart";
import { formatDuration, formatRate, pct } from "../delivery/lib/format";
import { DAY_MS, type WindowChoice } from "../delivery/lib/window";
import { type SegmentedOption, SegmentedToggle } from "../delivery/SegmentedToggle";
import { WindowControl } from "../delivery/WindowControl";
import type { ColumnId } from "./issues-table-model";
import { issuesViewHref } from "./issues-table-views";
import {
  daysOf,
  filingClasses,
  formatHours,
  metricsRange,
  reworkTrendSeries,
  splittingDaySeries,
  workTimeTrendSeries,
} from "./LegionMetrics";
import { useWidth } from "./useWidth";

const SECTION = `flex min-w-0 flex-col gap-2 rounded border px-4 py-3 ${surfaceBg} ${borderDefault}`;

/** The three populations the sections count, in the order the toggle offers them. */
const SCOPES: readonly SegmentedOption<LegionMetricsScope>[] = [
  { value: "all", label: "All issues" },
  { value: "roots", label: "Roots" },
  { value: "children", label: "Children" },
];

/** A section title with its info button: the rule behind the numbers, in at most two sentences. */
export function SectionTitle({ info, title }: { info?: ReactNode; title: string }): ReactNode {
  return (
    <h2
      className={`flex items-center gap-1.5 text-base font-semibold ${textPrimaryOnCanvas}`}
      data-legion-title
    >
      {title}
      {info === undefined ? null : (
        <InfoHint label={`About ${title.toLowerCase()}`}>{info}</InfoHint>
      )}
    </h2>
  );
}

/** One section: its title and info button, one line of numbers, and what it shows. */
function Section({
  children,
  headline,
  id,
  info,
  title,
}: {
  children?: ReactNode;
  headline: string;
  id: string;
  info?: ReactNode;
  title: string;
}): ReactNode {
  return (
    <section aria-label={title} className={SECTION} data-legion-section={id}>
      <SectionTitle info={info} title={title} />
      <p className={`text-sm tabular-nums ${textSecondaryOnCanvas}`} data-legion-headline>
        {headline}
      </p>
      {children}
    </section>
  );
}

/** Numbers joined as a headline: "948 finished · median 5.1 h". */
const line = (...parts: readonly (string | false)[]): string =>
  parts.filter((part): part is string => part !== false && part !== "").join(" · ");

function nextUtcDay(day: string): string {
  return new Date(Date.parse(`${day}T00:00:00Z`) + DAY_MS).toISOString().slice(0, 10);
}

function finishedDayHref(
  project: string,
  day: string,
  sort: `-${Extract<ColumnId, "work_hours" | "review_rounds">}`,
  scope: LegionMetricsScope,
  excluded: readonly string[]
): string {
  const overrides = new URLSearchParams([["f.closed", `${day}..${nextUtcDay(day)}`]]);
  if (scope !== "all") overrides.set("f.legion", scope);
  for (const root of excluded) overrides.append("exclude_tree", root);
  overrides.set("sort", sort);
  return issuesViewHref(project, "finished", overrides);
}

/** Whether a failed read is Dispatch saying it measures nothing for these repositories. */
function notMeasured(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.code === "DELIVERY_REPO_NOT_MEASURED" || error.code === "DELIVERY_NOT_CONFIGURED")
  );
}

export function LegionSections({ project }: { readonly project: string }): ReactNode {
  const navigate = useNavigate();
  const [chosen, setChosen] = useState<WindowChoice>({ kind: "preset", preset: "28d" });
  const [compare, setCompare] = useState<ChartRange | null>(null);
  const [scope, setScope] = useState<LegionMetricsScope>("all");
  // Tree roots the reader left out of every metric (the largest tree's note offers one), kept in
  // the address as `exclude_tree` so a shared link keeps them.
  const [params, setParams] = useSearchParams();
  const excluded = useMemo(() => params.getAll("exclude_tree"), [params]);
  const setExcluded = (roots: readonly string[]) =>
    setParams(
      (current) => {
        const next = new URLSearchParams(current);
        next.delete("exclude_tree");
        for (const root of roots) next.append("exclude_tree", root);
        return next;
      },
      { replace: true }
    );
  const range = useMemo(
    () => metricsRange(chosen, Math.floor(Date.now() / 60_000) * 60_000),
    [chosen]
  );
  const against = compare ?? range;
  const metrics = useQuery(
    legionMetricsQuery({ exclude_tree: excluded, from: range.from, project, scope, to: range.to })
  );
  const compared = useQuery({
    ...legionMetricsQuery({
      exclude_tree: excluded,
      from: against.from,
      project,
      scope,
      to: against.to,
    }),
    enabled: compare !== null,
  });
  const source = <T,>(data: T | undefined): ChartCompareSource<T> => ({
    data,
    onRange: setCompare,
    range: compare,
  });
  const report = metrics.data;
  // The first UTC day with a finished issue or a filing: the page says once that the window's
  // days before it hold no data, where each chart would say it again.
  const dataStart = useMemo(() => {
    if (report === undefined) return null;
    const days = [
      ...report.work_time.issues.map((issue) => issue.closed_at.slice(0, 10)),
      ...report.splitting.filed.map((one) => one.at.slice(0, 10)),
    ].sort();
    const first = days[0];
    return first !== undefined && first > report.from.slice(0, 10) ? first : null;
  }, [report]);

  return (
    <div className="flex flex-col gap-3" data-legion-sections>
      <div className="flex flex-wrap items-center justify-end gap-2">
        <SegmentedToggle label="Legion issues" onChange={setScope} options={SCOPES} value={scope} />
        <WindowControl onChange={setChosen} value={chosen} />
        <span className={`text-xs tabular-nums ${textMutedOnSurface}`} data-legion-range>
          {rangeLabel(range)}
          {dataStart === null ? "" : ` · no data before ${dataStart}`}
        </span>
      </div>
      {metrics.isError ? (
        <QueryError
          message="Could not load the Legion metrics."
          onRetry={() => void metrics.refetch()}
          retrying={metrics.isFetching}
        />
      ) : report === undefined ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Loading…</p>
      ) : (
        <>
          <PhaseBreakdownSection report={report} />
          <TrendSection
            compare={source(compared.data)}
            headline={line(
              `median ${formatHours(report.work_time.median_hours)}`,
              `p90 ${formatHours(report.work_time.p90_hours)}`
            )}
            id="work_time"
            info="Work time is the time from the first worker event to the merge. It does not include parked time or queued time."
            onSelect={(day) =>
              navigate(finishedDayHref(project, day, "-work_hours", scope, excluded))
            }
            report={report}
            title="Work time"
            trend={workTimeTrendSeries}
          >
            <TreePicker excluded={excluded} onExclude={setExcluded} report={report} />
          </TrendSection>
          <TrendSection
            compare={source(compared.data)}
            headline={line(
              `median ${report.rework.median_review_rounds.toFixed(1)} review rounds`,
              `${pct(report.rework.sent_back_share, 0)} sent back`,
              `median ${formatHours(report.rework.median_hours_after_first_review)} after the first review`
            )}
            id="rework"
            info="Sent back: the issue went back to implementing one or more times. After the first review: the implementing and testing time after the first review."
            onSelect={(day) =>
              navigate(finishedDayHref(project, day, "-review_rounds", scope, excluded))
            }
            report={report}
            title="Rework"
            trend={reworkTrendSeries}
          />
          <TrendSection
            compare={source(compared.data)}
            headline={line(
              `${issues(report.splitting.filed.length)} filed`,
              `${report.splitting.children_per_root.toFixed(1)} children per root`
            )}
            id="splitting"
            info={`Filed: the issues that Legion sessions created in the window. ${count(report.splitting.finished_roots)} roots finished: median work time ${formatHours(report.splitting.median_hours_roots_with_children)} with children, ${formatHours(report.splitting.median_hours_roots_without_children)} without.`}
            report={report}
            title="Splitting"
            trend={splittingDaySeries}
          >
            <FilingClasses report={report} />
          </TrendSection>
        </>
      )}
      <DeliverySection compare={compare} onCompare={setCompare} project={project} range={range} />
    </div>
  );
}

/** A count as the headlines write it: "1,259". */
const count = (value: number): string => value.toLocaleString("en-US");

/** A count of issues as the headlines write it: "1 issue", "1,259 issues". */
const issues = (value: number): string => `${count(value)} issue${value === 1 ? "" : "s"}`;

/** A selector for the most prominent finished trees, kept in the shared URL as `exclude_tree`. */
function TreePicker({
  excluded,
  onExclude,
  report,
}: {
  excluded: readonly string[];
  onExclude: (roots: readonly string[]) => void;
  report: LegionMetricsResponse;
}): ReactNode {
  const roots = [
    ...new Set([
      ...(report.largest_tree === null ? [] : [report.largest_tree.root]),
      ...report.work_time.issues.filter((issue) => issue.root).map((issue) => issue.key),
    ]),
  ].slice(0, 5);
  if (roots.length === 0) return null;
  const button = `rounded border px-2 py-1 text-xs ${borderDefault} ${textPrimaryOnCanvas}`;
  const selected = excluded[0] ?? "";
  return (
    <div className="flex flex-wrap items-center gap-1.5" data-legion-tree-note>
      <select
        aria-label="Leave out a tree"
        className={button}
        onChange={(event) => onExclude(event.target.value === "" ? [] : [event.target.value])}
        value={selected}
      >
        <option value="">Leave out a tree</option>
        {roots.map((root) => (
          <option key={root} value={root}>
            {root}
          </option>
        ))}
      </select>
      {excluded.length === 0 ? null : (
        <>
          <button className={button} onClick={() => onExclude([])} type="button">
            Count {excluded.map((root) => `${root}'s tree`).join(", ")} again
          </button>
          <InfoHint label={`About ${excluded.join(", ")}`}>
            The numbers on this page do not include the issues of {excluded.join(", ")}'s tree.
          </InfoHint>
        </>
      )}
    </div>
  );
}

function TrendSection({
  children,
  compare,
  headline,
  id,
  info,
  onSelect,
  report,
  title,
  trend,
}: {
  compare: ChartCompareSource<LegionMetricsResponse>;
  headline: string;
  id: string;
  info?: ReactNode;
  onSelect?: (day: string) => void;
  report: LegionMetricsResponse;
  title: string;
  trend: (report: LegionMetricsResponse) => ChartSeries[];
  children?: ReactNode;
}): ReactNode {
  const series = useMemo(() => trend(report), [trend, report]);
  const comparedData = compare.data;
  const comparedSeries = useMemo(
    () => (comparedData === undefined ? undefined : trend(comparedData)),
    [comparedData, trend]
  );
  return (
    <Section headline={headline} id={id} info={info} title={title}>
      <Chart
        compare={chartCompare(compare, comparedSeries)}
        defaultMode={daysOf(report).length < 3 ? "table" : "graph"}
        label={title}
        onSelect={onSelect}
        range={{ kind: "custom", from: report.from, to: report.to }}
        rangeOnPage
        series={series}
      />
      {children}
    </Section>
  );
}

/** The window's filings by class, as one bar with each class's count and share. */
function FilingClasses({ report }: { report: LegionMetricsResponse }): ReactNode {
  const classes = filingClasses(report);
  if (report.splitting.filed.length === 0) return null;
  return (
    <div className="flex flex-col gap-1" data-legion-filing-classes>
      <h3 className={`flex items-center gap-1.5 text-sm font-semibold ${textPrimaryOnCanvas}`}>
        Filed issues
        <InfoHint label="About filed issues">
          Tree: the filer's own tree. At design: while the filer was in design.
        </InfoHint>
      </h3>
      <div className="flex h-3 w-full overflow-hidden rounded-sm">
        {classes.map((one, index) =>
          one.count === 0 ? null : (
            <span
              className={filingScale[index] ?? ""}
              key={one.key}
              style={{ width: `${one.share * 100}%` }}
              title={`${one.label}: ${one.count}`}
            />
          )
        )}
      </div>
      <ul className="grid grid-cols-1 gap-x-6 gap-y-1 text-xs sm:grid-cols-2">
        {classes.map((one, index) => (
          <li className="flex items-center gap-2" key={one.key}>
            <span
              aria-hidden
              className={`inline-block h-3 w-3 shrink-0 rounded-sm ${filingScale[index] ?? ""}`}
            />
            <span className={`flex-1 ${textSecondaryOnCanvas}`}>{one.label}</span>
            <span className={`tabular-nums ${textPrimaryOnCanvas}`}>{one.count}</span>
            <span className={`w-10 text-right tabular-nums ${textMutedOnSurface}`}>
              {pct(one.share, 0)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** Time per phase: the finished issues' work hours by phase, as one bar and its legend. */
function PhaseBreakdownSection({ report }: { report: LegionMetricsResponse }): ReactNode {
  const breakdown = report.phase_breakdown;
  const phase = report.phase_time;
  const turn = phase.bars.reduce((sum, bar) => sum + bar.segments.in_a_turn, 0);
  const ci = phase.bars.reduce((sum, bar) => sum + bar.segments.ci_wait, 0);
  return (
    <Section
      headline={line(
        `${issues(breakdown.issues.length)} finished`,
        `${formatHours(breakdown.hours)} in phases`,
        breakdown.waiting_on_children_hours > 0 &&
          `${formatHours(breakdown.waiting_on_children_hours)} roots waited on children`,
        phase.observed_hours > 0 && `${formatHours(turn)} in a turn`,
        phase.observed_hours > 0 && `${formatHours(ci)} CI wait`
      )}
      id="phase_breakdown"
      info={
        <>
          The issues that finished in the window with Legion phase work, from the first worker event
          to the merge. A root's time while it waits on its children is not in the bar, because the
          children's rows count that time.
          {phase.observed_hours === 0
            ? " Time in a turn and CI wait need the daemon's events, so they are not shown yet."
            : ""}
        </>
      }
      title="Time per phase"
    >
      <PhaseBar breakdown={breakdown} />
    </Section>
  );
}

/** About how wide one character of the bar's 12px label is, and the label's side padding. */
const LABEL_CHAR_PX = 6.8;
const LABEL_PAD_PX = 10;

/** A segment's label for its width in pixels: its name and share where both fit, else nothing
 *  (the legend and the hover name it). */
function segmentLabel(label: string, share: number, px: number): string {
  const full = `${label} ${pct(share, 0)}`;
  return px >= full.length * LABEL_CHAR_PX + LABEL_PAD_PX ? full : "";
}

/** The full-width bar of the breakdown and its legend: each part as wide as its share at every
 *  page width, labelled where it has room, its hours and share in the legend. */
function PhaseBar({ breakdown }: { breakdown: LegionPhaseBreakdown }): ReactNode {
  const [barRef, width] = useWidth();
  return (
    <>
      <div className="flex h-9 w-full overflow-hidden rounded" data-legion-phase-bar ref={barRef}>
        {breakdown.phases.map((part) => (
          <span
            className={`flex h-9 items-center overflow-hidden px-1 text-xs font-medium whitespace-nowrap ${stageClass(`phase:${part.id}`)}`}
            data-legion-phase-part={part.id}
            key={part.id}
            style={{ flex: `0 0 ${part.share * 100}%` }}
            title={`${part.label}: ${formatHours(part.hours)}, ${pct(part.share, 0)}`}
          >
            {segmentLabel(part.label, part.share, part.share * width)}
          </span>
        ))}
      </div>
      <ul
        className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-3 lg:grid-cols-4"
        data-legion-phase-legend
      >
        {breakdown.phases.map((part) => (
          <li className="flex items-baseline gap-1.5" key={part.id}>
            <span
              aria-hidden
              className={`inline-block h-2.5 w-2.5 shrink-0 self-start mt-0.5 rounded-sm sm:self-center sm:mt-0 ${stageClass(`phase:${part.id}`)}`}
            />
            {/* On a phone the hours go under the label, so the label is never cut. */}
            <span className="flex min-w-0 flex-1 flex-col sm:flex-row sm:items-baseline sm:gap-1.5">
              <span className={`min-w-0 sm:truncate ${textSecondaryOnCanvas}`}>{part.label}</span>
              <span className={`shrink-0 tabular-nums sm:ml-auto ${textPrimaryOnCanvas}`}>
                {formatHours(part.hours)}
              </span>
            </span>
            <span
              className={`hidden w-8 shrink-0 text-right tabular-nums sm:inline ${textMutedOnSurface}`}
            >
              {pct(part.share, 0)}
            </span>
          </li>
        ))}
      </ul>
    </>
  );
}

/** CI and deploys for the project's own repositories: one line where nothing is measured for
 *  them, else a section with each one's numbers and chart. */
function DeliverySection({
  compare,
  onCompare,
  project,
  range,
}: {
  compare: ChartRange | null;
  onCompare: (next: ChartRange | null) => void;
  project: string;
  range: ChartRange;
}): ReactNode {
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
  const against = compare ?? range;
  const pipeline = useQuery({
    ...projectPipelineQuery({ from: range.from, repo: repos, to: range.to }),
    enabled: measured,
  });
  const measures = useQuery({
    ...projectMeasuresQuery({ from: range.from, repo: repos, to: range.to }),
    enabled: measured,
  });
  const comparedPipeline = useQuery({
    ...projectPipelineQuery({ from: against.from, repo: repos, to: against.to }),
    enabled: measured && compare !== null,
  });
  const comparedMeasures = useQuery({
    ...projectMeasuresQuery({ from: against.from, repo: repos, to: against.to }),
    enabled: measured && compare !== null,
  });
  if (mappings.isPending || delivery.isPending) return null;
  if (!measured || notMeasured(pipeline.error) || notMeasured(measures.error)) {
    return (
      <p
        className={`flex items-center gap-1.5 px-1 text-sm ${textMutedOnSurface}`}
        data-legion-delivery-unmeasured
      >
        CI and deploys: not measured
        <InfoHint label="About CI and deploys">
          {`Dispatch measures CI and deploys for one deploy repository${deployRepo === undefined ? "" : `, ${deployRepo}`}. ${project} has no repository mapped to it.`}
        </InfoHint>
      </p>
    );
  }
  const ci = pipeline.data?.pr_checks.run;
  const daily = measures.data?.measures;
  const source = <T,>(data: T | undefined): ChartCompareSource<T> => ({
    data,
    onRange: onCompare,
    range: compare,
  });
  const timing = (days: readonly DeliveryTimingDay[]): ChartSeries[] => [
    {
      kind: "spread",
      days,
      format: formatDuration,
      label: "Run time",
      noun: "runs",
      unit: "minutes per run",
    },
    {
      kind: "spark",
      format: (v) => formatRate(v),
      label: "Success",
      unit: "share of runs",
      max: 1,
      points: days.map((d) => ({ label: d.day, partial: d.partial, value: d.success_rate })),
    },
  ];
  const deploys = (days: readonly DeliveryDailyPoint[]): ChartSeries[] => [
    {
      kind: "spark",
      format: (v) => `${v}`,
      label: "Production deploys",
      unit: "deploys a day",
      max: 1,
      points: days.map((d) => ({ label: d.day, partial: d.partial, value: d.deploys })),
    },
  ];
  return (
    <>
      <Section
        headline={
          ci === undefined
            ? "Loading…"
            : line(
                `${count(ci.total.n)} check runs`,
                `median ${formatDuration(ci.total.median_minutes)}`,
                `p90 ${formatDuration(ci.total.p90_minutes)}`,
                `${formatRate(ci.total.success_rate)} passed`
              )
        }
        id="ci"
        info="The pull-request check runs of this project's repositories."
        title="CI"
      >
        {ci === undefined ? null : (
          <Chart
            compare={chartCompare(
              source(comparedPipeline.data),
              comparedPipeline.data === undefined
                ? undefined
                : timing(comparedPipeline.data.pr_checks.run.days ?? [])
            )}
            label="CI"
            range={range}
            rangeOnPage
            series={timing(ci.days ?? [])}
          />
        )}
      </Section>
      <Section
        headline={
          daily === undefined
            ? "Loading…"
            : line(
                `${count(daily.deploy_frequency.successful_deploys)} deploys`,
                `${daily.deploy_frequency.per_day.toFixed(1)} a day`,
                `median ${formatDuration(daily.lead_time.merge_to_production.median_minutes)} merge to production`
              )
        }
        id="deploys"
        info="Production deploys of the deploy repository."
        title="Deploys"
      >
        {daily === undefined ? null : (
          <Chart
            compare={chartCompare(
              source(comparedMeasures.data),
              comparedMeasures.data === undefined
                ? undefined
                : deploys(comparedMeasures.data.measures.daily ?? [])
            )}
            label="Deploys"
            range={range}
            rangeOnPage
            series={deploys(daily.daily ?? [])}
          />
        )}
      </Section>
    </>
  );
}
