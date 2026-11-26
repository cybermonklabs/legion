// The model an eval chart draws (dispatch://INFRA-2918/artifact/plan-md, D4): each series of an
// eval read as the values a chart draws of it, each with its label, its state and its note, the
// window's figures beside them, and, where the header names a compare range, each value's compare
// value and its change. Every chart of the Evals tab, a section's or a card's, draws one through
// `EvalChart`, so "no data" (never 0), "no data before <day>" and an input that has not begun
// (spec R18) and the default-repository note (spec R15) read the same wherever a number is. Pure.
import type { ComponentType } from "react";

import type { EvalListItem, EvalListResponse } from "../../api/types";
import { formatMinutes, pct } from "../delivery/lib/format";
import {
  type Judge,
  judgeFigure,
  type Span,
  type Target,
  type WriteFigure,
} from "../delivery/lib/target";
import { DAY_MS, utcDate } from "../delivery/lib/window";

/** One series of an eval as the list read and the evals view answer it. */
export type EvalSeriesValues = NonNullable<EvalListItem["series"]>[number];
/** A range of whole UTC days, as an eval read names its window and its compare range. */
export type EvalRange = EvalListResponse["window"];
type EvalVerdict = NonNullable<EvalListItem["verdict"]>;
type Statistics = NonNullable<EvalSeriesValues["window"]>;

/** What a value with nothing to measure reads: never 0 (spec R5). */
export const NO_DATA = "no data";

/** How a drawn value reads: met or missed by its own verdict against the eval's target, plain
 *  where no target judges it, absent where there is nothing to measure. */
export type ValueState = "met" | "missed" | "plain" | "absent";

/** One number a chart draws, as it draws it. */
export interface ChartValue {
  /** Null: nothing to measure. */
  readonly value: number | null;
  /** The number as written: "4.2/day", "45m", or `NO_DATA`. */
  readonly label: string;
  readonly state: ValueState;
  /** What the value says of itself beside its number: "so far" on a day the window ends inside. */
  readonly note: string | null;
}

/** One statistic a chart draws of a series: a column of its table. */
export interface ChartStatistic {
  /** Its name among the series' statistics, "median". */
  readonly id: string;
  /** Its column's header, "Median". */
  readonly label: string;
}

/** One place on a chart's axis, a day of the window or the window itself: each statistic's value
 *  there, the compare range's value at the same place and the change from it to the value, both
 *  by statistic and both null where the header names no compare range. */
export interface ChartPoint {
  /** The day (YYYY-MM-DD), or `window` for the window's own figures. */
  readonly x: string;
  readonly label: string;
  /** A day the window ends inside, so its values are of the day so far. */
  readonly partial: boolean;
  readonly values: Readonly<Record<string, ChartValue>>;
  readonly compare: Readonly<Record<string, ChartValue>> | null;
  readonly change: Readonly<Record<string, ChartValue>> | null;
}

/** One series of a chart. */
export interface ChartSeries {
  /** The series' name in the eval, which every value it draws names (`data-series`). */
  readonly name: string;
  readonly label: string;
  readonly unit: string;
  /** What its per-day counts count, "PRs": a day's title says "12 PRs". */
  readonly noun: string;
  /** Every statistic the chart's table holds of it, in its columns' order. */
  readonly statistics: readonly ChartStatistic[];
  /** The statistics whose window figures stand beside the chart, the first the largest. */
  readonly headline: readonly string[];
  /** The window's own figures. */
  readonly window: ChartPoint;
  /** Every day of the window, oldest first, a day with no row of the series included. */
  readonly points: readonly ChartPoint[];
  /** "no data before <day>" for a window that starts before the series' first input day, and
   *  "its input has not begun" for a series with none (spec R18); null otherwise. */
  readonly dataNote: string | null;
  /** "<n> by default repository" where the window counts rows that reached the project through
   *  its default repository (spec R15); null where it counts none. */
  readonly repositoryNote: string | null;
  /** The target that judges its figures, null for none. */
  readonly target: Target;
}

/** What a chart's table calls the places on its axis. */
export interface ChartAxis {
  readonly label: string;
  readonly one: string;
  readonly many: string;
}

/** Every day-by-day chart's axis. */
export const DAY_AXIS: ChartAxis = { label: "Day", one: "day", many: "days" };

/** Everything one chart draws: its series, and the controls it takes. */
export interface ChartModel {
  /** The chart's name, which every value it draws names (`data-chart`): the eval's slug where the
   *  chart draws one eval. */
  readonly name: string;
  /** Which renderer draws it (`data-chart-kind`). */
  readonly kind: string;
  readonly title: string;
  readonly axis: ChartAxis;
  readonly series: readonly ChartSeries[];
  /** The compare range the header names, null where it is off. */
  readonly compareRange: EvalRange | null;
  /** Whether the chart takes the compare control: every kind but `pending`, and not the merge
   *  timeline, which spec R7 exempts. */
  readonly comparable: boolean;
}

/** What draws a chart's graph: its model, and whether the compare range is shown. */
export interface ChartRendererProps {
  readonly model: ChartModel;
  readonly compare: boolean;
}

/** A renderer of one kind of chart. */
export type ChartRenderer = ComponentType<ChartRendererProps>;

/** The statistics a count names, written as the whole number it is. */
const COUNTS: ReadonlySet<string> = new Set([
  "n",
  "success",
  "failure",
  "cancelled",
  "deploys",
  "with_prs",
  "concluded",
  "reached_production",
  "confirmed",
  "pending",
  "rejected",
  "failed",
  "restoring",
  "not_restored",
  "total",
  "reverts",
  "issues",
  "children",
  "sent_back",
  "confirmed_or_pending",
]);

/** The statistics a share names, written as a percentage. */
const SHARES: ReadonlySet<string> = new Set(["rate", "share", "success_rate", "idle_share"]);

/** The statistics a rate a day names. */
const PER_DAY: ReadonlySet<string> = new Set(["per_day", "with_prs_per_day"]);

/** How a value of `statistic`, of a series in `unit`, is written, with `extra` decimals past its
 *  usual ones where a toned figure takes them (`lib/target.ts`'s `judgeFigure`). Minutes as the
 *  measures panel writes a lead time ("45m", "3.5h", "2.1d"); hours with a space before the unit,
 *  so a figure in hours is never read back as minutes when its decimals are judged. */
export function writerOf(unit: string, statistic: string): (value: number) => WriteFigure {
  if (PER_DAY.has(statistic)) return (value) => (extra) => `${value.toFixed(1 + extra)}/day`;
  if (SHARES.has(statistic) || unit === "ratio") return (value) => (extra) => pct(value, 1 + extra);
  if (COUNTS.has(statistic)) return (value) => () => `${value}`;
  if (unit === "minutes") return (value) => (extra) => formatMinutes(value, extra);
  if (unit === "hours") return (value) => (extra) => `${value.toFixed(1 + extra)} h`;
  return (value) => (extra) =>
    Number.isInteger(value) ? `${value}` : `${Number(value.toFixed(2 + extra))}`;
}

/** A change from a compare value to a value, written with its sign. */
function changeLabel(write: (value: number) => WriteFigure, change: number): string {
  if (change === 0) return "no change";
  return `${change > 0 ? "+" : "\u2212"}${write(Math.abs(change))(0)}`;
}

/** A statistic of a series as a target names what it judges, `<series>.<stat>`. A delivery
 *  target's judges are an enum in the contract; an eval's verdict may judge any statistic of its
 *  series, which is judged the same way, by its name. */
export function judgeOf(series: string, statistic: string): Judge {
  return `${series}.${statistic}` as Judge;
}

/** An eval's verdict as the target its figures are judged by (`lib/target.ts`), or null where the
 *  eval has none: the list read answers a verdict's target, direction, inclusiveness and the
 *  statistics it judges, the shape of a delivery target. */
export function targetOf(verdict: EvalVerdict | null): Target {
  if (verdict === null) return null;
  return {
    direction: verdict.direction === "higher_better" ? "higher_better" : "lower_better",
    inclusive: verdict.inclusive,
    judges: (verdict.judges ?? []).map((judge) => judge.judge as Judge),
    value: verdict.target,
  };
}

/** The series an eval is judged by and a card draws: its primary listed series, else its first
 *  listed one. */
export function primarySeries(item: EvalListItem): EvalSeriesValues | undefined {
  const listed = (item.series ?? []).filter((series) => series.listed);
  return listed.find((series) => series.is_primary) ?? listed[0];
}

/** Every UTC day of a range, oldest first, as YYYY-MM-DD. */
export function daysOf(range: EvalRange): string[] {
  const days: string[] = [];
  for (let at = Date.parse(range.from); at < Date.parse(range.to); at += DAY_MS) {
    days.push(new Date(at).toISOString().slice(0, 10));
  }
  return days;
}

/** What a series says of its input over the window (spec R18): the day it began where the window
 *  starts before it, and that it has not begun where it has no such day. */
function dataNoteOf(dataSince: string | null, window: EvalRange): string | null {
  if (dataSince === null) return "its input has not begun";
  return utcDate(window.from) < dataSince ? `no data before ${dataSince}` : null;
}

/** What a chart shows of one series: its label, every statistic its table holds, the headline
 *  figures beside it, and what its per-day counts count. */
export interface SeriesSpec {
  readonly label: string;
  readonly statistics: readonly ChartStatistic[];
  readonly headline: readonly string[];
  readonly noun?: string;
}

/** The answer a series is drawn from: the read's window and compare range, the target that judges
 *  the series' figures, and the present, which says which day the window ends inside. */
export interface SeriesContext {
  readonly window: EvalRange;
  readonly compare: EvalRange | null;
  readonly target: Target;
  readonly nowMs: number;
}

/** A series of an eval read as a chart draws it. Each day of the window is a point, a day with no
 *  row of the series reading "no data"; the compare range's days are drawn at the same offset from
 *  the window's first day (plan D6), its days past the window's length left out; a day the window
 *  ends inside is judged by no target, as the measures panel judged it. */
export function chartSeries(
  values: EvalSeriesValues,
  spec: SeriesSpec,
  context: SeriesContext
): ChartSeries {
  const ids = spec.statistics.map((statistic) => statistic.id);
  const write = (id: string) => writerOf(values.unit, id);
  const drawn = (
    stats: Statistics | null | undefined,
    id: string,
    judge: { span: Span; target: Target } | null,
    note: string | null
  ): ChartValue => {
    const value = stats?.[id] ?? null;
    if (value === null) return { value: null, label: NO_DATA, state: "absent", note: null };
    const { met, text } = judgeFigure(
      judge?.target ?? null,
      judgeOf(values.series, id),
      judge?.span ?? "day",
      value,
      write(id)(value)
    );
    return { value, label: text, state: met === null ? "plain" : met ? "met" : "missed", note };
  };
  const changes = (
    now: Readonly<Record<string, ChartValue>>,
    then: Readonly<Record<string, ChartValue>>
  ): Record<string, ChartValue> =>
    Object.fromEntries(
      ids.map((id) => {
        const value = now[id]?.value ?? null;
        const before = then[id]?.value ?? null;
        if (value === null || before === null) {
          return [id, { value: null, label: NO_DATA, state: "absent", note: null }];
        }
        const change = value - before;
        return [
          id,
          { value: change, label: changeLabel(write(id), change), state: "plain", note: null },
        ];
      })
    );

  const statisticsOf = (
    stats: Statistics | null | undefined,
    judge: { span: Span; target: Target } | null,
    note: string | null
  ) => Object.fromEntries(ids.map((id) => [id, drawn(stats, id, judge, note)]));

  const days = daysOf(context.window);
  const byDay = new Map((values.days ?? []).map((point) => [point.day, point.statistics]));
  const comparing = context.compare !== null && values.compare_days !== null;
  const compareDays = context.compare === null ? [] : daysOf(context.compare);
  const compareByDay = new Map(
    (values.compare_days ?? []).map((point) => [point.day, point.statistics])
  );
  const points = days.map((day, index): ChartPoint => {
    const partial = Date.parse(`${day}T00:00:00Z`) + DAY_MS > context.nowMs;
    const own = statisticsOf(
      byDay.get(day),
      partial ? null : { span: "day", target: context.target },
      partial ? "so far" : null
    );
    const compareDay = compareDays[index];
    const compare = comparing
      ? statisticsOf(compareDay === undefined ? null : compareByDay.get(compareDay), null, null)
      : null;
    return {
      x: day,
      label: day,
      partial,
      values: own,
      compare,
      change: compare === null ? null : changes(own, compare),
    };
  });

  const windowValues = statisticsOf(
    values.window,
    { span: "window", target: context.target },
    null
  );
  const windowCompare = comparing ? statisticsOf(values.compare, null, null) : null;
  const windowChange = comparing
    ? Object.fromEntries(
        ids.map((id) => {
          const change = values.change?.[id] ?? null;
          return [
            id,
            change === null
              ? { value: null, label: NO_DATA, state: "absent" as const, note: null }
              : {
                  value: change,
                  label: changeLabel(write(id), change),
                  state: "plain" as const,
                  note: null,
                },
          ];
        })
      )
    : null;

  return {
    name: values.series,
    label: spec.label,
    unit: values.unit,
    noun: spec.noun ?? "values",
    statistics: spec.statistics,
    headline: spec.headline,
    window: {
      x: "window",
      label: "Window",
      partial: false,
      values: windowValues,
      compare: windowCompare,
      change: windowChange,
    },
    points,
    dataNote: dataNoteOf(values.data_since, context.window),
    repositoryNote:
      values.via_repository > 0 ? `${values.via_repository} by default repository` : null,
    target: context.target,
  };
}

/** One series a section's chart draws of an eval: its name in the eval, and how it shows. */
export interface ChartSeriesSpec {
  readonly name: string;
  readonly spec: SeriesSpec;
}

/** What names a section's chart of one eval: its name (`data-chart`, the eval's slug), its
 *  renderer's kind and its title. */
export interface ChartHead {
  readonly name: string;
  readonly kind: string;
  readonly title: string;
}

/** A section's chart of `item`, one eval of `answer`, a day at a time: each of `series` the eval
 *  holds, judged by its verdict, over the answer's window and compare range; null where it holds
 *  none of them. */
export function dayChartModel(
  answer: EvalListResponse,
  item: EvalListItem,
  head: ChartHead,
  series: readonly ChartSeriesSpec[],
  nowMs: number
): ChartModel | null {
  const context = {
    compare: answer.compare,
    nowMs,
    target: targetOf(item.verdict),
    window: answer.window,
  };
  const drawn = series.flatMap(({ name, spec }) => {
    const values = item.series?.find((candidate) => candidate.series === name);
    return values === undefined ? [] : [chartSeries(values, spec, context)];
  });
  if (drawn.length === 0) return null;
  return {
    axis: DAY_AXIS,
    comparable: true,
    compareRange: answer.compare,
    kind: head.kind,
    name: head.name,
    series: drawn,
    title: head.title,
  };
}

/** What an eval is called on the page: its name, or its slug where it has none (an eval its first
 *  reading registered). */
export function evalName(item: EvalListItem): string {
  return item.definition.name === "" ? item.definition.slug : item.definition.name;
}

/** What a statistic an eval's own fold states is called in a chart of an eval no section draws by
 *  name; one not listed here reads as the fold names it. */
const STATISTIC_LABELS: Readonly<Record<string, string>> = {
  max: "Max",
  mean: "Mean",
  median: "Median",
  n: "Count",
  p90: "p90",
  rate: "Rate",
  sum: "Sum",
  value: "Value",
};

/** The statistic a series' agg states first, as the server names a logged series' headline
 *  (`evals.HeadlineStatistic`): `value` for `last` and any agg not listed here. */
const AGG_HEADLINE: Readonly<Record<string, string>> = {
  max: "max",
  quantile: "median",
  ratio: "rate",
  sum: "sum",
  weighted_mean: "mean",
};

/** How a chart of an eval no section draws by name shows one of its series: every statistic the
 *  read answers of it, led by one the eval's verdict judges of it (`judged`), else the one its agg
 *  states first, else the first by name; and its count beside the lead, where it has one. */
function everySeriesSpec(values: EvalSeriesValues, judged: readonly string[]): SeriesSpec {
  const answered = new Set([
    ...Object.keys(values.window ?? {}),
    ...(values.days ?? []).flatMap((day) => Object.keys(day.statistics ?? {})),
  ]);
  const byName = [...answered].sort();
  const stated = AGG_HEADLINE[values.agg] ?? "value";
  const lead =
    judged.find((id) => answered.has(id)) ??
    (answered.has(stated) ? stated : (byName[0] ?? stated));
  // A series the read answers no statistic of yet has its agg's, which a logged fold always states.
  const counted = lead !== "n" && (answered.has("n") || answered.size === 0);
  const ids = [
    lead,
    ...byName.filter((id) => id !== lead && id !== "n"),
    ...(counted ? ["n"] : []),
  ];
  return {
    headline: counted ? [lead, "n"] : [lead],
    label: values.unit === "" ? values.series : `${values.series} (${values.unit})`,
    statistics: ids.map((id) => ({ id, label: STATISTIC_LABELS[id] ?? id })),
  };
}

/** A chart of `item`, an eval no section draws by name (a logged eval, or a computed one a later
 *  catalogue adds; plan D7), a day at a time: each listed series in the order the read answers
 *  them, the primary first, as a line of its leading statistic, judged by the eval's verdict, over
 *  the answer's window and compare range; null where it lists none. */
export function everyEvalModel(
  answer: EvalListResponse,
  item: EvalListItem,
  nowMs: number
): ChartModel | null {
  const judges = (item.verdict?.judges ?? []).map(({ judge }) => judge);
  const series = (item.series ?? [])
    .filter((values) => values.listed)
    .map((values) => {
      const prefix = `${values.series}.`;
      const judged = judges.flatMap((judge) =>
        judge.startsWith(prefix) ? [judge.slice(prefix.length)] : []
      );
      return { name: values.series, spec: everySeriesSpec(values, judged) };
    });
  return dayChartModel(
    answer,
    item,
    { kind: "line", name: item.definition.slug, title: evalName(item) },
    series,
    nowMs
  );
}

/** One place of a category chart: the eval's series it is (a Legion role's), and its label. */
export interface Category {
  readonly name: string;
  readonly label: string;
}

/**
 * A section's chart of `item` with one place per category over the whole window, each category one
 * series of the eval (a Legion role's bar), its window figures, compare values and changes as that
 * series states them (`chartSeries`'s window point), each statistic of `spec`. The chart draws them
 * as one series named `series`, whose places are named by the eval's series (`data-x`); what the
 * categories' series say of their input and of the rows counted by default repository is the
 * first's they share. Null where the eval holds none of the categories.
 */
export function categoryChartModel(
  answer: EvalListResponse,
  item: EvalListItem,
  head: ChartHead & { readonly axis: ChartAxis; readonly series: string },
  categories: readonly Category[],
  spec: SeriesSpec,
  nowMs: number
): ChartModel | null {
  const context = { compare: answer.compare, nowMs, target: null, window: answer.window };
  const drawn = categories.flatMap(({ name, label }) => {
    const values = item.series?.find((candidate) => candidate.series === name);
    if (values === undefined) return [];
    const one = chartSeries(values, spec, context);
    return [{ one, point: { ...one.window, x: name, label } }];
  });
  const first = drawn[0]?.one;
  if (first === undefined) return null;
  return {
    axis: head.axis,
    comparable: true,
    compareRange: answer.compare,
    kind: head.kind,
    name: head.name,
    series: [
      {
        ...first,
        headline: [],
        label: head.title,
        name: head.series,
        points: drawn.map(({ point }) => point),
        window: {
          change: null,
          compare: null,
          label: "Window",
          partial: false,
          values: {},
          x: "window",
        },
      },
    ],
    title: head.title,
  };
}
