// What the Legion tab plots from `GET /api/v1/legion/metrics` (dispatch://INFRA-979): per-day
// trends of the finished issues' work time, rework and filings, and one row per issue for the
// work-time and rework tables. The server holds every rule; this only turns its answer into the
// series `Chart` draws, each with the unit its value axis is in.
import type {
  LegionFilingClass,
  LegionMetricsResponse,
  LegionWorkTimeIssue,
} from "../../api/types";
import type { ChartRange, ChartSeries } from "../delivery/Chart";
import { formatDuration, pct } from "../delivery/lib/format";
import { DAY_MS, liveWindow, type WindowChoice } from "../delivery/lib/window";

/** Hours as the page writes them ("3.2h"; minutes under an hour). */
export const formatHours = (value: number): string =>
  value >= 100 ? `${Math.round(value).toLocaleString("en-US")}h` : formatDuration(value * 60);

/** The four classes of filing, in the order the splitting chart lists them. */
const CLASSES: readonly { readonly key: LegionFilingClass; readonly label: string }[] = [
  { key: "in_tree_at_design", label: "In the filer's tree, while designing it" },
  { key: "in_tree_later", label: "In the filer's tree, later" },
  { key: "out_of_tree", label: "Outside the filer's tree" },
  { key: "by_the_controller", label: "By the controller" },
];

/** The range the metrics route reads for a window choice: a custom range as named, and a preset
 *  as its own days ending at `nowMs`. */
export function metricsRange(choice: WindowChoice, nowMs: number): ChartRange {
  const live = liveWindow(choice);
  if (!("preset" in live)) return { kind: "custom", from: live.from, to: live.to };
  return {
    kind: "custom",
    from: new Date(nowMs - live.preset * DAY_MS).toISOString(),
    to: new Date(nowMs).toISOString(),
  };
}

/** The UTC days a report's window covers, oldest first, each partial where the window covers only
 *  part of it. */
export function daysOf(report: Pick<LegionMetricsResponse, "from" | "to">): {
  day: string;
  partial: boolean;
}[] {
  const from = Date.parse(report.from);
  const to = Date.parse(report.to);
  const days: { day: string; partial: boolean }[] = [];
  for (let start = Math.floor(from / DAY_MS) * DAY_MS; start < to; start += DAY_MS) {
    days.push({
      day: new Date(start).toISOString().slice(0, 10),
      partial: start < from || start + DAY_MS > to,
    });
  }
  return days;
}

/** The q-quantile of values by linear interpolation, null for none. */
function quantile(values: readonly number[], q: number): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const at = (sorted.length - 1) * q;
  const low = Math.floor(at);
  const high = Math.min(low + 1, sorted.length - 1);
  return (sorted[low] ?? 0) + ((sorted[high] ?? 0) - (sorted[low] ?? 0)) * (at - low);
}

/** A per-day series: one value a UTC day of the window, null for a day nothing counted. */
function perDay(
  report: LegionMetricsResponse,
  label: string,
  unit: string,
  format: (value: number) => string,
  value: (day: string) => number | null
): ChartSeries {
  const points = daysOf(report).map(({ day, partial }) => ({
    label: day,
    partial,
    value: value(day),
  }));
  return {
    kind: "spark",
    format,
    label,
    unit,
    max: Math.max(1, ...points.map((point) => point.value ?? 0)),
    points,
  };
}

/** The finished issues by the UTC day they closed. */
function closedByDay(report: LegionMetricsResponse): Map<string, LegionWorkTimeIssue[]> {
  const byDay = new Map<string, LegionWorkTimeIssue[]>();
  for (const issue of report.work_time.issues) {
    const day = issue.closed_at.slice(0, 10);
    byDay.set(day, [...(byDay.get(day) ?? []), issue]);
  }
  return byDay;
}

/** Work time's graph: each UTC day's median and p90 work time of the issues that closed that day. */
export function workTimeTrendSeries(report: LegionMetricsResponse): ChartSeries[] {
  const byDay = closedByDay(report);
  const of = (q: number) => (day: string) =>
    quantile(
      (byDay.get(day) ?? []).map((issue) => issue.hours),
      q
    );
  return [
    perDay(report, "Median work time", "hours", formatHours, of(0.5)),
    perDay(report, "p90 work time", "hours", formatHours, of(0.9)),
  ];
}

/** Rework's graph: each UTC day's median review rounds and share sent back, of the issues that
 *  closed that day, each on its own value axis. */
export function reworkTrendSeries(report: LegionMetricsResponse): ChartSeries[] {
  const byDay = closedByDay(report);
  const rework = new Map(report.rework.issues.map((issue) => [issue.key, issue]));
  const rows = (day: string) =>
    (byDay.get(day) ?? []).flatMap((issue) => {
      const row = rework.get(issue.key);
      return row === undefined ? [] : [row];
    });
  return [
    perDay(
      report,
      "Median review rounds",
      "review rounds",
      (v) => v.toFixed(1),
      (day) =>
        quantile(
          rows(day).map((row) => row.review_rounds),
          0.5
        )
    ),
    perDay(
      report,
      "Share sent back to implementing",
      "share of issues",
      (v) => pct(v, 0),
      (day) => {
        const closed = rows(day);
        return closed.length === 0
          ? null
          : closed.filter((row) => row.sent_back).length / closed.length;
      }
    ),
  ];
}

/** Splitting's graph: each UTC day's issues Legion sessions filed against the issues that
 *  finished; whether filing outruns finishing is what it answers. */
export function splittingDaySeries(report: LegionMetricsResponse): ChartSeries[] {
  const byDay = closedByDay(report);
  const filed = new Map<string, number>();
  for (const one of report.splitting.filed) {
    const day = one.at.slice(0, 10);
    filed.set(day, (filed.get(day) ?? 0) + 1);
  }
  const count = (value: number) => `${value}`;
  return [
    perDay(
      report,
      "Issues filed by Legion sessions",
      "issues a day",
      count,
      (day) => filed.get(day) ?? 0
    ),
    perDay(report, "Issues finished", "issues a day", count, (day) => byDay.get(day)?.length ?? 0),
  ];
}

/** The window's filings by class, each with its count and share, in the order they are listed. */
export function filingClasses(
  report: LegionMetricsResponse
): { key: LegionFilingClass; label: string; count: number; share: number }[] {
  const total = report.splitting.filed.length;
  return CLASSES.map((filing) => {
    const count = report.splitting.filed.filter((one) => one.class === filing.key).length;
    return { ...filing, count, share: total === 0 ? 0 : count / total };
  });
}

/** Work time's table: one row per finished issue, its work time and the hours taken out of it. */
export function workTimeIssueSeries(report: LegionMetricsResponse): ChartSeries[] {
  const issues = [...report.work_time.issues].sort((a, b) => b.hours - a.hours);
  const run = (label: string, value: (issue: LegionWorkTimeIssue) => number): ChartSeries => ({
    kind: "spark",
    format: formatHours,
    label,
    max: 1,
    points: issues.map((issue) => ({ label: issue.key, partial: false, value: value(issue) })),
  });
  return [
    run("Work time", (issue) => issue.hours),
    run("Parked", (issue) => issue.parked_hours),
    run("Queued", (issue) => issue.queued_hours),
  ];
}

/** Rework's table: one row per finished issue, its review rounds and its hours after the first
 *  review. */
export function reworkIssueSeries(report: LegionMetricsResponse): ChartSeries[] {
  const issues = [...report.rework.issues].sort((a, b) => b.review_rounds - a.review_rounds);
  const run = (
    label: string,
    format: (value: number) => string,
    value: (issue: (typeof issues)[number]) => number
  ): ChartSeries => ({
    kind: "spark",
    format,
    label,
    max: 1,
    points: issues.map((issue) => ({ label: issue.key, partial: false, value: value(issue) })),
  });
  return [
    run(
      "Review rounds",
      (v) => `${v}`,
      (issue) => issue.review_rounds
    ),
    run(
      "Implementing after the first review",
      formatHours,
      (issue) => issue.implementing_hours_after_first_review
    ),
    run(
      "Testing after the first review",
      formatHours,
      (issue) => issue.testing_hours_after_first_review
    ),
  ];
}
