// The Legion tab's measured sections (dispatch://INFRA-979), one column, full width, in the order
// a reader asks: where the finished issues' time went (the phase breakdown, and each issue's own
// bar), work time to merge, rework, splitting and follow-ups, then CI and deploys. Each section
// opens with one line of plain numbers, each with its unit and what it counts; each chart has a
// value axis, a time axis and a legend, and the one compare range. Every issue key opens that
// issue's timeline (`onOpenIssue`).
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useMemo, useRef, useState } from "react";

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
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  borderStrong,
  chartParts,
  legionPhaseScale,
  surfaceBg,
  surfaceMutedBg,
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
} from "../delivery/Chart";
import { formatDuration, formatRate, pct } from "../delivery/lib/format";
import type { WindowChoice } from "../delivery/lib/window";
import { type SegmentedOption, SegmentedToggle } from "../delivery/SegmentedToggle";
import { WindowControl } from "../delivery/WindowControl";
import {
  daysOf,
  filingClasses,
  formatHours,
  metricsRange,
  reworkIssueSeries,
  reworkTrendSeries,
  splittingDaySeries,
  workTimeIssueSeries,
  workTimeTrendSeries,
} from "./LegionMetrics";

const SECTION = `flex min-w-0 flex-col gap-2 rounded border px-4 py-3 ${surfaceBg} ${borderDefault}`;
const KEY_BUTTON = "font-mono text-xs underline decoration-dotted underline-offset-2";

/** The three populations the sections count, in the order the toggle offers them. */
const SCOPES: readonly SegmentedOption<LegionMetricsScope>[] = [
  { value: "all", label: "All issues" },
  { value: "roots", label: "Roots" },
  { value: "children", label: "Children" },
];

/** One section: its title, its headline line of numbers, and what it shows. */
function Section({
  children,
  headline,
  id,
  title,
}: {
  children?: ReactNode;
  headline: string;
  id: string;
  title: string;
}): ReactNode {
  return (
    <section aria-label={title} className={SECTION} data-legion-section={id}>
      <h2 className={`text-base font-semibold ${textPrimaryOnCanvas}`}>{title}</h2>
      <p className={`text-sm ${textSecondaryOnCanvas}`} data-legion-headline>
        {headline}
      </p>
      {children}
    </section>
  );
}

/** Whether a failed read is Dispatch saying it measures nothing for these repositories. */
function notMeasured(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.code === "DELIVERY_REPO_NOT_MEASURED" || error.code === "DELIVERY_NOT_CONFIGURED")
  );
}

export function LegionSections({
  onOpenIssue,
  project,
}: {
  readonly project: string;
  readonly onOpenIssue: (key: string) => void;
}): ReactNode {
  const [chosen, setChosen] = useState<WindowChoice>({ kind: "preset", preset: "28d" });
  const [compare, setCompare] = useState<ChartRange | null>(null);
  const [scope, setScope] = useState<LegionMetricsScope>("all");
  // Tree roots the reader left out of every metric (the largest tree's note offers one).
  const [excluded, setExcluded] = useState<readonly string[]>([]);
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

  return (
    <div className="flex flex-col gap-3" data-legion-sections>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className={`text-sm ${textSecondaryOnCanvas}`}>
          Issues that finished in the window, with Legion phase work.
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <SegmentedToggle
            label="Legion issues"
            onChange={setScope}
            options={SCOPES}
            value={scope}
          />
          <WindowControl onChange={setChosen} value={chosen} />
        </div>
      </div>
      {metrics.isError ? (
        <QueryError
          message="Couldn't load the Legion metrics."
          onRetry={() => void metrics.refetch()}
          retrying={metrics.isFetching}
        />
      ) : report === undefined ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Loading the Legion metrics…</p>
      ) : (
        <>
          <PhaseBreakdownSection onOpenIssue={onOpenIssue} report={report} />
          <TrendSection
            compare={source(compared.data)}
            headline={`${report.finished} issues finished · median ${formatHours(report.work_time.median_hours)} from first worker event to merge, less parked and queued time · p90 ${formatHours(report.work_time.p90_hours)}`}
            id="work_time"
            onOpenIssue={onOpenIssue}
            report={report}
            table={workTimeIssueSeries}
            title="Work time to merge"
            trend={workTimeTrendSeries}
          >
            <LargestTreeNote excluded={excluded} onExclude={setExcluded} report={report} />
          </TrendSection>
          <TrendSection
            compare={source(compared.data)}
            headline={`median ${report.rework.median_review_rounds.toFixed(1)} review rounds per finished issue · ${pct(report.rework.sent_back_share, 0)} sent back to implementing at least once · median ${formatHours(report.rework.median_hours_after_first_review)} implementing and testing after the first review`}
            id="rework"
            onOpenIssue={onOpenIssue}
            report={report}
            table={reworkIssueSeries}
            title="Rework"
            trend={reworkTrendSeries}
          />
          <TrendSection
            compare={source(compared.data)}
            headline={`${report.splitting.filed.length.toLocaleString("en-US")} issues filed by Legion sessions · of the ${report.work_time.issues.length.toLocaleString("en-US")} issues finished, ${report.splitting.finished_roots} were roots, with ${report.splitting.children_per_root.toFixed(1)} children each on average · median work time ${formatHours(report.splitting.median_hours_roots_with_children)} for roots with children, ${formatHours(report.splitting.median_hours_roots_without_children)} without`}
            id="splitting"
            report={report}
            title="Splitting and follow-ups"
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

/** The share of the finished issues one tree holds at or past which its note is shown. */
const LARGE_TREE_SHARE = 0.25;

/** Under the work-time chart: when one tree holds a large share of the finished issues, how many,
 *  and their median work time against the rest's, with the one filter that sets them apart. */
function LargestTreeNote({
  excluded,
  onExclude,
  report,
}: {
  excluded: readonly string[];
  onExclude: (roots: readonly string[]) => void;
  report: LegionMetricsResponse;
}): ReactNode {
  const tree = report.largest_tree;
  const button = `self-start rounded border px-2 py-1 text-xs ${borderDefault} ${textPrimaryOnCanvas}`;
  if (excluded.length > 0) {
    return (
      <div className="flex flex-wrap items-center gap-2" data-legion-tree-note>
        <p className={`text-xs ${textSecondaryOnCanvas}`}>
          Every number on this page leaves out {excluded.join(", ")}'s tree.
        </p>
        <button className={button} onClick={() => onExclude([])} type="button">
          Count it again
        </button>
      </div>
    );
  }
  if (tree === null || tree.finished < 2 || tree.finished < report.finished * LARGE_TREE_SHARE) {
    return null;
  }
  return (
    <div className="flex flex-wrap items-center gap-2" data-legion-tree-note>
      <p className={`text-xs ${textSecondaryOnCanvas}`}>
        {tree.finished} of the {report.finished} finished issues are in {tree.root}'s tree
        {tree.title === "" ? "" : ` (${tree.title})`}: median work time{" "}
        {formatHours(tree.median_hours)} against {formatHours(tree.rest_median_hours)} for the rest.
        They carry Legion phase work, so they are counted.
      </p>
      <button className={button} onClick={() => onExclude([tree.root])} type="button">
        Leave out {tree.root}'s tree
      </button>
    </div>
  );
}

/** A section whose graph is a per-day trend and whose table is one row per issue (or the trend's
 *  own days where it has no per-issue table). A window of fewer than three days opens on the
 *  table. */
function TrendSection({
  children,
  compare,
  headline,
  id,
  onOpenIssue,
  report,
  table,
  title,
  trend,
}: {
  compare: ChartCompareSource<LegionMetricsResponse>;
  headline: string;
  id: string;
  onOpenIssue?: (key: string) => void;
  report: LegionMetricsResponse;
  table?: (report: LegionMetricsResponse) => ChartSeries[];
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
  const rows = useMemo(() => table?.(report), [table, report]);
  const comparedRows = useMemo(
    () => (comparedData === undefined ? undefined : table?.(comparedData)),
    [comparedData, table]
  );
  return (
    <Section headline={headline} id={id} title={title}>
      <Chart
        compare={chartCompare(compare, comparedSeries)}
        defaultMode={daysOf(report).length < 3 ? "table" : "graph"}
        label={title}
        onSelect={
          rows === undefined || onOpenIssue === undefined ? undefined : (row) => onOpenIssue(row)
        }
        range={{ kind: "custom", from: report.from, to: report.to }}
        series={series}
        table={
          rows === undefined
            ? undefined
            : {
                compare: comparedRows,
                rows: { header: "Issue", over: "issue", pair: "name" },
                series: rows,
              }
        }
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
      <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>
        Where the filed issues went
      </h3>
      <div className="flex h-3 w-full overflow-hidden rounded-sm">
        {classes.map((one, index) =>
          one.count === 0 ? null : (
            <span
              className={chartParts[index] ?? ""}
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
              className={`inline-block h-3 w-3 shrink-0 rounded-sm ${chartParts[index] ?? ""}`}
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

/** Where the finished issues' work hours went: one full-width bar by phase, in the issues
 *  table's colours, its legend with each part's hours and share, and each issue's own bar on one
 *  scale of hours, most hours first or most hours in the part the reader picked. */
function PhaseBreakdownSection({
  onOpenIssue,
  report,
}: {
  onOpenIssue: (key: string) => void;
  report: LegionMetricsResponse;
}): ReactNode {
  const breakdown = report.phase_breakdown;
  const [sortBy, setSortBy] = useState<string | null>(null);
  const [all, setAll] = useState(false);
  const biggest = [...breakdown.phases].sort((a, b) => b.hours - a.hours)[0];
  const phase = report.phase_time;
  const turn = phase.bars.reduce((sum, bar) => sum + bar.segments.in_a_turn, 0);
  const ci = phase.bars.reduce((sum, bar) => sum + bar.segments.ci_wait, 0);
  const headline = [
    `${breakdown.issues.length.toLocaleString("en-US")} issues finished`,
    `${formatHours(breakdown.hours)} of work in phases, each hour counted once, from first worker event to merge, less parked and queued time`,
    biggest === undefined
      ? ""
      : `most of it ${biggest.label.toLowerCase()}: ${pct(biggest.share, 0)} (${formatHours(biggest.hours)})`,
  ]
    .filter((part) => part !== "")
    .join(" · ");
  const sorted = useMemo(
    () =>
      [...breakdown.issues].sort((a, b) =>
        sortBy === null ? b.hours - a.hours : (b.phases[sortBy] ?? 0) - (a.phases[sortBy] ?? 0)
      ),
    [breakdown.issues, sortBy]
  );
  const top = Math.max(1, ...breakdown.issues.map((issue) => issue.hours));
  const shown = all ? sorted : sorted.slice(0, 25);
  const label = (id: string) => breakdown.phases.find((one) => one.id === id)?.label ?? id;
  const mostly = (issue: (typeof breakdown.issues)[number]) => {
    const [id, spent] = Object.entries(issue.phases).sort((a, b) => b[1] - a[1])[0] ?? ["", 0];
    const parts =
      issue.hours === 0
        ? []
        : [`mostly ${label(id).toLowerCase()}, ${pct(spent / issue.hours, 0)}`];
    if (issue.waiting_on_children_hours > 0) {
      parts.push(`also waited ${formatHours(issue.waiting_on_children_hours)} on its children`);
    }
    return parts.join(" · ");
  };
  return (
    <Section headline={headline} id="phase_breakdown" title="Where the finished issues' time went">
      <PhaseBar breakdown={breakdown} onPick={setSortBy} picked={sortBy} />
      {breakdown.waiting_on_children_hours > 0 ? (
        <p className={`text-xs ${textSecondaryOnCanvas}`} data-legion-children-wait>
          Roots also waited {formatHours(breakdown.waiting_on_children_hours)} on their children,
          whose work is counted above.
        </p>
      ) : null}
      <p className={`text-xs ${textMutedOnSurface}`} data-legion-turn-note>
        {phase.observed_hours === 0
          ? "Not observed yet: in a turn and CI wait. They need the daemon's claim states and CI verdicts, which start with its pushed events; the phases above come from each issue's status history."
          : `Of ${formatHours(phase.observed_hours)} the daemon observed: ${formatHours(turn)} with a role in a turn, ${formatHours(ci)} waiting on CI.`}
      </p>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>
          {sortBy === null
            ? "Issues by work hours"
            : `Issues by hours ${label(sortBy).toLowerCase()}`}
        </h3>
        {sortBy === null ? (
          <span className={`text-xs ${textMutedOnSurface}`}>
            Pick a phase in the bar or its legend to rank the issues by it.
          </span>
        ) : (
          <button
            className={`text-xs underline ${textSecondaryOnCanvas}`}
            onClick={() => setSortBy(null)}
            type="button"
          >
            Rank by all work hours
          </button>
        )}
      </div>
      <ol className="flex flex-col" data-legion-breakdown-issues>
        {shown.map((issue) => (
          <li
            className={`grid grid-cols-[minmax(0,1fr)_4rem] items-center gap-x-3 gap-y-0.5 border-t py-1 md:grid-cols-[6.5rem_minmax(0,22rem)_4rem_minmax(0,1fr)] ${borderDefault}`}
            data-legion-breakdown-issue={issue.key}
            key={issue.key}
          >
            <button
              className={`${KEY_BUTTON} order-1 justify-self-start md:order-none`}
              onClick={() => onOpenIssue(issue.key)}
              type="button"
            >
              {issue.key}
            </button>
            <span className="order-3 col-span-2 flex min-w-0 flex-col md:order-none md:col-span-1">
              <span className={`truncate text-xs ${textSecondaryOnCanvas}`} title={issue.title}>
                {issue.title}
              </span>
              <span className={`text-[11px] ${textMutedOnSurface}`} data-legion-breakdown-mostly>
                {mostly(issue)}
              </span>
            </span>
            <span
              className={`order-2 text-right text-xs tabular-nums md:order-none ${textPrimaryOnCanvas}`}
            >
              {formatHours(sortBy === null ? issue.hours : (issue.phases[sortBy] ?? 0))}
            </span>
            <span
              className="order-4 col-span-2 flex h-3 md:order-none md:col-span-1"
              style={{ width: "100%" }}
            >
              <span
                className={`flex h-3 overflow-hidden rounded-sm ${surfaceMutedBg}`}
                style={{ width: `${(100 * issue.hours) / top}%` }}
                title={`${issue.key}: ${formatHours(issue.hours)}`}
              >
                {breakdown.phases.map((part) => {
                  const spent = issue.phases[part.id] ?? 0;
                  return spent <= 0 ? null : (
                    <span
                      className={legionPhaseScale[part.id] ?? ""}
                      key={part.id}
                      style={{ width: `${(100 * spent) / issue.hours}%` }}
                      title={`${part.label}: ${formatHours(spent)}`}
                    />
                  );
                })}
              </span>
            </span>
          </li>
        ))}
      </ol>
      {sorted.length > 25 ? (
        <button
          className={`self-start text-xs underline ${textSecondaryOnCanvas}`}
          onClick={() => setAll(!all)}
          type="button"
        >
          {all ? "Show the first 25" : `Show all ${sorted.length} issues`}
        </button>
      ) : null}
    </Section>
  );
}

/** About how wide one character of the bar's 12px label is, and the label's side padding. */
const LABEL_CHAR_PX = 6.8;
const LABEL_PAD_PX = 10;

/** The bar's width in pixels, measured; before a measure (or where nothing measures, as in a
 *  test) a desktop width, so the labels are those a wide page draws. */
function useWidth(): [(node: HTMLElement | null) => void, number] {
  const [width, setWidth] = useState(1000);
  const observer = useRef<ResizeObserver | null>(null);
  const ref = useCallback((node: HTMLElement | null) => {
    observer.current?.disconnect();
    if (node === null || typeof ResizeObserver === "undefined") return;
    observer.current = new ResizeObserver(([entry]) => {
      if (entry !== undefined && entry.contentRect.width > 0) setWidth(entry.contentRect.width);
    });
    observer.current.observe(node);
  }, []);
  return [ref, width];
}

/** A segment's label for its width in pixels: its name and share where both fit, the share
 *  alone where only that does, else nothing (the legend names it). */
function segmentLabel(label: string, share: number, px: number): string {
  const full = `${label} ${pct(share, 0)}`;
  if (px >= full.length * LABEL_CHAR_PX + LABEL_PAD_PX) return full;
  const short = pct(share, 0);
  return px >= short.length * LABEL_CHAR_PX + LABEL_PAD_PX ? short : "";
}

/** The full-width bar of the breakdown and its legend: each part in the issues table's colour,
 *  as wide as its share at every page width, labelled where it has room, its hours and share in
 *  the legend. A part picked ranks the issues by it. */
function PhaseBar({
  breakdown,
  onPick,
  picked,
}: {
  breakdown: LegionPhaseBreakdown;
  onPick: (id: string | null) => void;
  picked: string | null;
}): ReactNode {
  const [barRef, width] = useWidth();
  return (
    <>
      <div className="flex h-9 w-full overflow-hidden rounded" data-legion-phase-bar ref={barRef}>
        {breakdown.phases.map((part) => (
          <button
            aria-pressed={picked === part.id}
            className={`h-9 overflow-hidden px-1 text-left text-xs font-medium whitespace-nowrap ${legionPhaseScale[part.id] ?? ""} ${picked !== null && picked !== part.id ? "opacity-50" : ""}`}
            data-legion-phase-part={part.id}
            key={part.id}
            onClick={() => onPick(picked === part.id ? null : part.id)}
            // The page's 44px touch minimum is off here: a segment is as wide as its share,
            // and the legend under the bar is the touch target for each phase.
            style={{ flex: `0 0 ${part.share * 100}%`, minWidth: 0, minHeight: 0 }}
            title={`${part.label}: ${formatHours(part.hours)}, ${pct(part.share, 0)}`}
            type="button"
          >
            {segmentLabel(part.label, part.share, part.share * width)}
          </button>
        ))}
      </div>
      <ul
        className="grid grid-cols-2 gap-x-2 gap-y-1 text-xs lg:grid-cols-3"
        data-legion-phase-legend
      >
        {breakdown.phases.map((part) => (
          <li key={part.id}>
            <button
              aria-pressed={picked === part.id}
              className={`flex w-full cursor-pointer items-center gap-1.5 rounded border px-1.5 py-1 text-left text-[11px] shadow-sm hover:underline sm:gap-2 sm:px-2 sm:text-xs ${picked === part.id ? `border-2 ${borderStrong} ${surfaceBg} font-semibold` : `${borderDefault} ${surfaceBg}`}`}
              data-legion-phase-pick={part.id}
              onClick={() => onPick(picked === part.id ? null : part.id)}
              title={`Rank the issues by hours ${part.label.toLowerCase()}`}
              type="button"
            >
              <span
                aria-hidden
                className={`inline-block h-3 w-3 shrink-0 rounded-sm ${legionPhaseScale[part.id] ?? ""}`}
              />
              {/* On a phone the hours go under the name, so the two never run together. */}
              <span className="flex min-w-0 flex-1 flex-col sm:flex-row sm:items-baseline sm:gap-2">
                <span
                  className={`min-w-0 leading-tight sm:flex-1 sm:truncate ${textSecondaryOnCanvas}`}
                >
                  {part.label}
                </span>
                <span className={`shrink-0 tabular-nums ${textPrimaryOnCanvas}`}>
                  {formatHours(part.hours)}
                </span>
              </span>
              <span
                className={`hidden w-10 text-right tabular-nums sm:inline ${textMutedOnSurface}`}
              >
                {pct(part.share, 0)}
              </span>
            </button>
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
      <Section
        headline={`Not measured for this project yet. Dispatch measures one deploy repository${deployRepo === undefined ? "" : ` (${deployRepo})`}, and ${project} maps none of its repositories to it.`}
        id="delivery"
        title="CI and deploys"
      />
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
            : `${ci.total.n} PR check runs · median ${formatDuration(ci.total.median_minutes)} each · p90 ${formatDuration(ci.total.p90_minutes)} · ${formatRate(ci.total.success_rate)} succeeded`
        }
        id="ci"
        title="CI"
      >
        {ci === undefined ? null : (
          <Chart
            compare={chartCompare(
              source(comparedPipeline.data),
              comparedPipeline.data === undefined
                ? undefined
                : timing(comparedPipeline.data.pr_checks.run.days)
            )}
            label="CI"
            range={range}
            series={timing(ci.days)}
          />
        )}
      </Section>
      <Section
        headline={
          daily === undefined
            ? "Loading…"
            : `${daily.deploy_frequency.successful_deploys} production deploys · ${daily.deploy_frequency.per_day.toFixed(1)} a day · median ${formatDuration(daily.lead_time.merge_to_production.median_minutes)} from merge to production`
        }
        id="deploys"
        title="Deploys"
      >
        {daily === undefined ? null : (
          <Chart
            compare={chartCompare(
              source(comparedMeasures.data),
              comparedMeasures.data === undefined
                ? undefined
                : deploys(comparedMeasures.data.measures.daily)
            )}
            label="Deploys"
            range={range}
            series={deploys(daily.daily)}
          />
        )}
      </Section>
    </>
  );
}
