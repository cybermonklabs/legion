// The built-in renderers of `EvalChart` (dispatch://INFRA-2918/artifact/plan-md, D4): per-day bars
// through the delivery pages' `Sparkbars`, the per-day spread of a median and its p90 through
// `SpreadBars`, a per-day line, a job's timing (its median and p90 a day, and how often it
// succeeded), and one bar per category (a Legion role) over the whole window. Each draws every
// series of its model, names every value it draws in the DOM (`data-chart`, `data-series`, and
// each value's place and value: `data-x` and `data-value` on a node of one value, `data-xs` and
// `data-values` on the group that draws a whole range of a per-day chart, whose days one path
// draws, dispatch://INFRA-2918, T33), and, while the compare range is shown, draws it as a muted
// second series at the same places, empty where the range has no data. Each draws only its frame,
// the same elements at the same size with no mark in them, while the section around holds its
// charts back (`drawn`, `ChartRendererProps`).
import type { ReactNode } from "react";

import { chartBar, chartLine, chartSegment } from "../../theme/classes";
import { barScale } from "../delivery/lib/barScale";
import {
  Sparkbars,
  usePlaceUnderPointer,
  type ValueMarks,
  valueList,
  valueMarks,
} from "../delivery/Sparkbars";
import { SpreadBars } from "../delivery/SpreadBars";
import {
  type ChartModel,
  type ChartRendererProps,
  type ChartSeries,
  type ChartStatistic,
  judgeOf,
  NO_DATA,
  writerOf,
} from "./chart-model";

/** The DOM names of `series`' values of `statistic` (`ValueMarks`). */
function marksOf(model: ChartModel, series: ChartSeries, statistic: string): ValueMarks {
  return {
    chart: model.name,
    series: series.name,
    statistic,
    x: series.points.map((point) => point.x),
  };
}

/** `series`' values of `statistic`, a day at a time, the window's or the compare range's. */
function valuesOf(
  series: ChartSeries,
  statistic: string,
  range: "values" | "compare"
): (number | null)[] {
  return series.points.map((point) => point[range]?.[statistic]?.value ?? null);
}

/** One bar per day of each series' first statistic, toned by the series' target where it judges a
 *  day's value of it. A share is drawn on a scale up to 100%. */
export function PerDayBars({ model, compare, drawn }: ChartRendererProps): ReactNode {
  return model.series.map((series) => {
    const statistic = series.statistics[0]?.id;
    if (statistic === undefined) return null;
    return (
      <Sparkbars
        compare={compare ? valuesOf(series, statistic, "compare") : undefined}
        drawn={drawn}
        judged={
          series.target === null
            ? undefined
            : {
                evalSlug: model.name,
                statistic: judgeOf(series.name, statistic),
                target: series.target,
              }
        }
        key={series.name}
        marks={marksOf(model, series, statistic)}
        max={series.unit === "ratio" ? 1 : undefined}
        points={series.points.map((point) => ({
          label: point.x,
          partial: point.partial,
          value: point.values[statistic]?.value ?? null,
        }))}
        write={writerOf(series.unit, statistic)}
      />
    );
  });
}

/** Each series' median and p90 a day (`SpreadBars`), its days' counts in each day's title. */
export function PerDaySpread({ model, compare, drawn }: ChartRendererProps): ReactNode {
  return model.series.map((series) => (
    <SpreadBars
      compare={
        compare
          ? series.points.map((point) => ({
              median: point.compare?.median?.value ?? null,
              p90: point.compare?.p90?.value ?? null,
            }))
          : undefined
      }
      days={series.points.map((point) => ({
        day: point.x,
        median_minutes: point.values.median?.value ?? null,
        n: point.values.n?.value ?? 0,
        p90_minutes: point.values.p90?.value ?? null,
        partial: point.partial,
      }))}
      drawn={drawn}
      judged={series.target === null ? undefined : { evalSlug: model.name, target: series.target }}
      key={series.name}
      marks={marksOf(model, series, "median")}
      noun={series.noun}
      write={writerOf(series.unit, "median")}
    />
  ));
}

const STEP = 10;
const HEIGHT = 28;
/** A point's radius on the line. */
const POINT = 1.5;

/** One range's line as two paths: a stroke through each run of consecutive days with a value, a
 *  day with no value breaking it, and a point on each day with one. */
function linePaths(
  values: readonly (number | null)[],
  y: (value: number) => number
): { stroke: string; points: string } {
  let stroke = "";
  let points = "";
  let joined = false;
  values.forEach((value, index) => {
    if (value === null) {
      joined = false;
      return;
    }
    const cx = Math.round((index * STEP + STEP / 2) * 100) / 100;
    const cy = Math.round(y(value) * 100) / 100;
    stroke += `${joined ? "L" : "M"}${cx} ${cy}`;
    joined = true;
    points += `M${cx - POINT} ${cy}a${POINT} ${POINT} 0 1 0 ${2 * POINT} 0a${POINT} ${POINT} 0 1 0 ${-2 * POINT} 0`;
  });
  return { points, stroke };
}

/** One range's line: its stroke and its points, the range's values named on its group. */
function Line(props: {
  className: { stroke: string; point: string };
  dashed: boolean;
  marks: ValueMarks;
  range: "window" | "compare";
  values: readonly (number | null)[];
  y: (value: number) => number;
}): ReactNode {
  const { points, stroke } = linePaths(props.values, props.y);
  return (
    <g data-series-range={props.range} {...valueList(props.marks, props.values, props.range)}>
      <path
        className={`fill-none ${props.className.stroke}`}
        d={stroke}
        strokeDasharray={props.dashed ? "2 2" : undefined}
        strokeWidth={1.5}
        vectorEffect="non-scaling-stroke"
      />
      <path className={props.className.point} d={points} />
    </g>
  );
}

/** One series' line of its first statistic a day, the compare range's muted and dashed under it,
 *  and the day under the pointer, with both ranges' values there, as its title. */
function SeriesLine({
  compared,
  drawn,
  marks,
  values,
  write,
}: {
  compared: readonly (number | null)[] | undefined;
  drawn: boolean;
  marks: ValueMarks;
  values: readonly (number | null)[];
  write: (value: number) => string;
}): ReactNode {
  const hover = usePlaceUnderPointer(values.length);
  const y = barScale(1, undefined, [...values, ...(compared ?? [])], HEIGHT - 4);
  const written = (value: number | null | undefined) =>
    value === null || value === undefined ? NO_DATA : write(value);
  return (
    <svg
      aria-label="per-day line"
      className="block h-7 w-full"
      onPointerLeave={hover.onPointerLeave}
      onPointerMove={hover.onPointerMove}
      preserveAspectRatio="none"
      role="img"
      viewBox={`0 -2 ${values.length * STEP} ${HEIGHT}`}
    >
      {!drawn ? null : (
        <>
          {hover.place === null ? null : (
            <title>
              {`${marks.x[hover.place] ?? ""}: ${written(values[hover.place])}${compared === undefined ? "" : `; compare ${written(compared[hover.place])}`}`}
            </title>
          )}
          {compared === undefined ? null : (
            <Line
              className={{ point: chartLine.comparePoint, stroke: chartLine.compareStroke }}
              dashed
              marks={marks}
              range="compare"
              values={compared}
              y={y}
            />
          )}
          <Line
            className={{ point: chartLine.point, stroke: chartLine.stroke }}
            dashed={false}
            marks={marks}
            range="window"
            values={values}
            y={y}
          />
        </>
      )}
    </svg>
  );
}

/** A line through each series' first statistic a day, a day with no value breaking it; the
 *  compare range's line muted and dashed under it. */
export function PerDayLine({ model, compare, drawn = true }: ChartRendererProps): ReactNode {
  return model.series.map((series) => {
    const statistic = series.statistics[0]?.id;
    if (statistic === undefined) return null;
    const write = writerOf(series.unit, statistic);
    return (
      <SeriesLine
        compared={compare ? valuesOf(series, statistic, "compare") : undefined}
        drawn={drawn}
        key={series.name}
        marks={marksOf(model, series, statistic)}
        values={valuesOf(series, statistic, "values")}
        write={(value) => write(value)(0)}
      />
    );
  });
}

/** A job's timing (the Pipeline page's timing card): each day's median and p90 with its count of
 *  runs in the day's title, and each day's success rate under them. */
export function Timing({ model, compare, drawn }: ChartRendererProps): ReactNode {
  return model.series.map((series) => (
    <div className="flex flex-col gap-1" key={series.name}>
      <PerDaySpread compare={compare} drawn={drawn} model={{ ...model, series: [series] }} />
      <Sparkbars
        compare={compare ? valuesOf(series, "success_rate", "compare") : undefined}
        drawn={drawn}
        marks={marksOf(model, series, "success_rate")}
        max={1}
        points={series.points.map((point) => ({
          label: point.x,
          partial: point.partial,
          value: point.values.success_rate?.value ?? null,
        }))}
        write={writerOf(series.unit, "success_rate")}
      />
    </div>
  ));
}

/** The five segments a phase-time bar splits a role's hours into (legion-phase-time's statistics,
 *  metrics.Segments), in the order `CategoryBars` draws them. */
export const PHASE_SEGMENTS: readonly (ChartStatistic & { id: keyof typeof chartSegment })[] = [
  { id: "in_a_turn", label: "In a turn" },
  { id: "ci_wait", label: "CI wait" },
  { id: "waiting_on_person", label: "Waiting on a person" },
  { id: "waiting_on_children", label: "Waiting on children" },
  { id: "idle", label: "Idle" },
];

/** One bar per place on the axis (a Legion role) of each series' first statistic over the whole
 *  window, as long as its share of the longest bar, its value written after it. A series whose
 *  statistics hold the phase-time segments (`PHASE_SEGMENTS`) splits each bar into them, each
 *  segment its own value; the compare range's value is a muted bar behind it. Its frame is each
 *  place's row, its label and its value, with an empty bar. */
export function CategoryBars({ model, compare, drawn = true }: ChartRendererProps): ReactNode {
  return model.series.map((series) => {
    const statistic = series.statistics[0]?.id;
    if (statistic === undefined) return null;
    const segments = PHASE_SEGMENTS.filter((segment) =>
      series.statistics.some(({ id }) => id === segment.id)
    );
    const values = valuesOf(series, statistic, "values");
    const compared = compare ? valuesOf(series, statistic, "compare") : [];
    const longest = Math.max(
      0,
      ...[...values, ...compared].filter((value): value is number => value !== null)
    );
    const length = (value: number) => (longest > 0 ? (value / longest) * 100 : 0);
    const write = writerOf(series.unit, statistic);
    const marks = marksOf(model, series, statistic);
    return (
      <div className="flex flex-col gap-1" key={series.name}>
        {series.points.map((point, index) => {
          const value = values[index] ?? null;
          const before = compared[index] ?? null;
          let offset = 0;
          return (
            <div
              className="grid grid-cols-[8rem_minmax(0,1fr)_4rem] items-center gap-2 text-xs"
              key={point.x}
            >
              <span>{point.label}</span>
              <svg
                aria-label={`${point.label} bar`}
                className="block h-3 w-full"
                preserveAspectRatio="none"
                role="img"
                viewBox="0 0 100 10"
              >
                {!drawn ? null : (
                  <>
                    {before === null ? null : (
                      <rect
                        className={chartBar.compare}
                        height={10}
                        width={length(before)}
                        {...valueMarks(marks, index, before, "compare")}
                      >
                        <title>{`${point.label}, compare: ${write(before)(0)}`}</title>
                      </rect>
                    )}
                    {value === null || segments.length > 0 ? null : (
                      <rect className={chartBar.neutral} height={6} width={length(value)} y={2} />
                    )}
                    {value === null
                      ? null
                      : segments.map((segment) => {
                          const hours = point.values[segment.id]?.value ?? null;
                          if (hours === null) return null;
                          const x = offset;
                          offset += length(hours);
                          return (
                            <rect
                              className={chartSegment[segment.id]}
                              height={6}
                              key={segment.id}
                              width={length(hours)}
                              x={x}
                              y={2}
                              {...valueMarks(
                                marksOf(model, series, segment.id),
                                index,
                                hours,
                                "window"
                              )}
                            >
                              <title>{`${point.label}, ${segment.label.toLowerCase()}: ${writerOf(series.unit, segment.id)(hours)(0)}`}</title>
                            </rect>
                          );
                        })}
                  </>
                )}
              </svg>
              <span
                className="text-right tabular-nums"
                {...(value === null ? {} : valueMarks(marks, index, value, "window"))}
              >
                {value === null ? NO_DATA : write(value)(0)}
              </span>
            </div>
          );
        })}
        {segments.length === 0 ? null : (
          <ul className="flex flex-wrap gap-x-3 text-[11px]">
            {segments.map((segment) => (
              <li className="flex items-center gap-1" key={segment.id}>
                <svg aria-hidden="true" className="h-2 w-2" viewBox="0 0 2 2">
                  <rect className={chartSegment[segment.id]} height={2} width={2} />
                </svg>
                {segment.label}
              </li>
            ))}
          </ul>
        )}
      </div>
    );
  });
}
