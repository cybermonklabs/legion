// The renderer of a `bar`, an `interval` and a `stacked-bar` (dispatch://INFRA-2918/artifact/
// plan-md, D10): the model's places as an SVG plot of upright bars, points or stacks over their
// categories or dates, of horizontal rows, or as a facet table, with its reference, annotations
// and key; and, while the compare range is shown, each category's compare value as a tick (a bar)
// or a muted ghost (an interval's point, a stack), its change printed beside its value, and a
// stack over time's compare range as a muted second stack at each date. Each value it draws names
// itself as the chart's table names it (`data-chart`, `data-series`, `data-statistic`, `data-x`,
// `data-range`, `data-value`), so the table holds every number the graph draws.
import { type ReactNode, useId, useState } from "react";

import {
  chartBar,
  chartLine,
  chartPlot,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../../theme/classes";
import type { ChartRendererProps } from "../chart-model";
import {
  type BarsAnnotation,
  type BarsModel,
  type BarsPoint,
  type BarsSeries,
  isBarsModel,
} from "./bars";
import { type Tone, toneClasses } from "./kind";

const WIDTH = 640;
const MARGIN = { bottom: 40, left: 60, right: 16, top: 14 };
const DIRECT_LABELS = 112;
const NOTE_TIER = 14;
const DEFAULT_HEIGHT = 340;
const POINT = 3.5;
const DAY = 24 * 60 * 60 * 1000;
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/** Which of a point's values a mark draws: its own, the compare range's, or the change. */
type Part = "window" | "compare" | "change";

/** A scale from values to pixels, with its ticks. */
interface Scale {
  readonly at: (value: number) => number;
  readonly ticks: readonly number[];
  readonly low: number;
  readonly log: boolean;
}

/** The step of about `count` ticks over `span`: 1, 2, 2.5 or 5 times a power of ten, a whole
 *  number where the axis ticks whole numbers only. */
function niceStep(span: number, count: number, whole: boolean): number {
  const raw = span / count;
  if (!(raw > 0)) return 1;
  const power = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((times) => times * power).find((one) => one >= raw) ?? raw;
  return whole ? Math.max(1, Math.ceil(step)) : step;
}

/** A linear scale over `values` onto [from, to]: an end the spec declares is the axis's end, and
 *  an end it does not reaches half a tick past the outermost value. */
function linearScale(
  values: readonly number[],
  ends: { readonly min: number | null; readonly max: number | null },
  range: readonly [number, number],
  whole: boolean
): Scale {
  let low = Math.min(...values, ...(ends.min === null ? [] : [ends.min]));
  let high = Math.max(...values, ...(ends.max === null ? [] : [ends.max]));
  if (!Number.isFinite(low) || !Number.isFinite(high)) [low, high] = [0, 1];
  if (low === high)
    [low, high] = low === 0 ? [0, 1] : [low - Math.abs(low) / 2, high + Math.abs(high) / 2];
  const step = niceStep(high - low, 5, whole);
  // A bar's baseline at 0 is an end of the axis, not half a tick inside it.
  const from = ends.min ?? (low === 0 ? 0 : low - step / 2);
  const to = ends.max ?? (high === 0 ? 0 : high + step / 2);
  const ticks: number[] = [];
  for (let tick = Math.ceil(from / step) * step; tick <= to + step * 1e-9; tick += step) {
    ticks.push(Number(tick.toPrecision(12)));
  }
  return {
    at: (value) => range[0] + ((value - from) / (to - from || 1)) * (range[1] - range[0]),
    log: false,
    low: from,
    ticks,
  };
}

/** A log scale over the positive `values`: its ends at powers of ten, which it ticks, with 2 and 5
 *  times them where powers of ten alone give fewer than three ticks. */
function logScale(
  values: readonly number[],
  ends: { readonly min: number | null; readonly max: number | null },
  range: readonly [number, number]
): Scale {
  const positive = values.filter((value) => value > 0);
  const lowest = positive.length === 0 ? 1 : Math.min(...positive);
  const highest = positive.length === 0 ? 10 : Math.max(...positive);
  const low = ends.min ?? 10 ** Math.floor(Math.log10(lowest));
  const high = ends.max ?? 10 ** Math.ceil(Math.log10(highest));
  const top = high > low ? high : low * 10;
  const powers: number[] = [];
  for (let power = Math.ceil(Math.log10(low)); 10 ** power <= top * (1 + 1e-9); power++) {
    powers.push(10 ** power);
  }
  const ticks =
    powers.length >= 3
      ? powers
      : powers
          .flatMap((tick) => [tick, tick * 2, tick * 5])
          .filter((tick) => tick >= low && tick <= top);
  const span = Math.log10(top) - Math.log10(low) || 1;
  return {
    at: (value) =>
      range[0] +
      ((Math.log10(Math.max(value, low / 1e6)) - Math.log10(low)) / span) * (range[1] - range[0]),
    log: true,
    low,
    ticks,
  };
}

function tickLabel(value: number, share: boolean): string {
  if (share) return `${Math.round(value * 100)}%`;
  return value.toLocaleString("en-US", { maximumFractionDigits: 3 });
}

/** `text` cut to `most` characters, with "…" where it is longer. */
function cut(text: string, most: number): string {
  return text.length <= most ? text : `${text.slice(0, Math.max(1, most - 1))}\u2026`;
}

/** The value of `statistic` at a point, the window's or the compare range's. */
function valueAt(point: BarsPoint, statistic: string, part: Part): number | null {
  const values =
    part === "window" ? point.values : part === "compare" ? point.compare : point.change;
  return values?.[statistic]?.value ?? null;
}

/** The DOM names of a drawn value, as the chart's table names it; none where it has no value. */
function valueNames(
  model: BarsModel,
  series: BarsSeries,
  point: BarsPoint,
  statistic: string,
  part: Part
): Record<string, string> {
  const value = valueAt(point, statistic, part);
  if (value === null) return {};
  return {
    "data-chart": model.name,
    "data-range": part,
    "data-series": series.name,
    "data-statistic": statistic,
    "data-value": String(value),
    "data-x": point.x,
  };
}

/** What a mark's tip says: its series, its place and its value, with its interval and its note. */
function tip(series: BarsSeries, point: BarsPoint, part: "window" | "compare"): string {
  const values = (part === "window" ? point.values : point.compare) ?? {};
  const low = values.ci_low;
  const high = values.ci_high;
  const interval =
    low?.value != null && high?.value != null ? `, interval ${low.label} to ${high.label}` : "";
  const total = values.total?.value != null ? ` of ${values.total.label}` : "";
  const note = values.value?.note ? ` (${values.value.note})` : "";
  return `${series.label}, ${point.label}${part === "compare" ? ", compare" : ""}: ${
    values.value?.label ?? ""
  }${total}${interval}${note}`;
}

/** A label's words where it carries a detail: the label bold, its detail after it. */
function AnnotationText(props: {
  note: BarsAnnotation;
  x: number;
  y: number;
  anchor: "start" | "middle" | "end";
}): ReactNode {
  const { note } = props;
  return (
    <text
      className={chartPlot.text}
      fontSize={10}
      textAnchor={props.anchor}
      x={props.x}
      y={props.y}
    >
      <tspan fontWeight={note.detail === null ? undefined : 600}>{note.label}</tspan>
      {note.detail === null ? null : <tspan opacity={0.75}>{` \u00b7 ${note.detail}`}</tspan>}
    </text>
  );
}

/** The marks of a cut-off value: a small triangle past its end, pointing the way the true value
 *  lies (up or right for at least, down or left for at most). */
function boundMark(bound: "<=" | ">=", x: number, y: number, horizontal: boolean): string {
  const way = bound === ">=" ? -1 : 1;
  return horizontal
    ? `M${x - way * 6},${y} L${x - way * 1},${y - 3.5} L${x - way * 1},${y + 3.5} Z`
    : `M${x},${y + way * 6} L${x - 3.5},${y + way * 1} L${x + 3.5},${y + way * 1} Z`;
}

/** The hatching over a mock-up's plot, and its word. */
function MockupCover(props: {
  hatchId: string;
  left: number;
  right: number;
  top: number;
  bottom: number;
}): ReactNode {
  const { bottom, left, right, top } = props;
  return (
    <g data-mockup>
      <rect
        fill={`url(#${props.hatchId})`}
        height={bottom - top}
        opacity={0.5}
        width={right - left}
        x={left}
        y={top}
      />
      <text
        className={chartPlot.text}
        fontSize={14}
        fontWeight={600}
        opacity={0.6}
        textAnchor="middle"
        x={(left + right) / 2}
        y={(top + bottom) / 2}
      >
        MOCK-UP
      </text>
    </g>
  );
}

function Hatch({ id }: { id: string }): ReactNode {
  return (
    <pattern
      height={8}
      id={id}
      patternTransform="rotate(45)"
      patternUnits="userSpaceOnUse"
      width={8}
    >
      <line className={chartPlot.mockup} strokeWidth={2} x1={0} x2={0} y1={0} y2={8} />
    </pattern>
  );
}

/** The plot of upright bars, points or stacks over the shown places (`shown`, indices into the
 *  model's places): its value axis, its places' names or dates, its reference and annotations,
 *  its marks, and with the compare range shown each category's tick or ghost and its change. */
function ColumnsPlot(props: {
  compare: boolean;
  model: BarsModel;
  shown: readonly number[];
}): ReactNode {
  const { compare, model, shown } = props;
  const { bars } = model;
  const clipId = useId();
  const hatchId = useId();
  const groups = model.series;
  const stacked = bars.layout === "stack" || bars.layout === "area";
  const tiered = bars.annotations.some((note) => note.category !== null && note.y === null);
  const direct = bars.legend === "direct" && stacked;
  const top = MARGIN.top + (tiered ? NOTE_TIER : 0);
  const left = MARGIN.left;
  const plotRight = WIDTH - MARGIN.right - (direct ? DIRECT_LABELS : 0);
  const height = bars.height ?? DEFAULT_HEIGHT;
  const bottom = height - MARGIN.bottom - (bars.grouped && bars.legend === "direct" ? 10 : 0);
  const places = shown.flatMap((index) => {
    const place = bars.places[index];
    return place === undefined ? [] : [place];
  });
  const tones = (tone: Tone) => toneClasses(tone, bars.notStartedGrey);
  const parts: ("window" | "compare")[] = compare ? ["window", "compare"] : ["window"];

  // Where each place stands: a slot of a category axis, or its date on a time axis.
  const times = bars.overTime
    ? [
        ...places.flatMap((place) => (place.at === null ? [] : [place.at, place.at + DAY])),
        ...bars.annotations.flatMap((note) => (note.at === null ? [] : [note.at])),
      ]
    : [];
  const timeLow = Math.min(...times);
  const timeHigh = Math.max(...times);
  const xOfTime = (at: number) =>
    left + ((at - timeLow) / (timeHigh - timeLow || 1)) * (plotRight - left);
  const slot = bars.overTime
    ? ((plotRight - left) * DAY) / (timeHigh - timeLow || DAY)
    : (plotRight - left) / Math.max(1, places.length);
  const centre = (index: number) =>
    bars.overTime ? xOfTime((places[index]?.at ?? timeLow) + DAY / 2) : left + (index + 0.5) * slot;
  const centreOf = (category: string) => {
    const index = places.findIndex((place) => place.category === category);
    return index < 0 ? null : centre(index);
  };

  // The values the value axis reaches.
  const values: number[] = [];
  const stackOf = (index: number, part: "window" | "compare") => {
    let positive = 0;
    let negative = 0;
    return groups.map((one) => {
      const point = one.points[index] as BarsPoint;
      const value = (part === "window" ? point.held : point.compareHeld)
        ? valueAt(point, "value", part)
        : null;
      if (value === null) return null;
      const from = value >= 0 ? positive : negative;
      if (value >= 0) positive += value;
      else negative += value;
      return { from, to: from + value };
    });
  };
  if (stacked) {
    values.push(0);
    for (const index of shown) {
      for (const part of parts) {
        for (const segment of stackOf(index, part)) {
          if (segment !== null) values.push(segment.from, segment.to);
        }
      }
    }
  } else {
    if (bars.mark === "bar" && !bars.valueAxis.log) values.push(0);
    for (const one of groups) {
      for (const index of shown) {
        const point = one.points[index] as BarsPoint;
        for (const part of parts) {
          if (part === "window" ? !point.held : !point.compareHeld) continue;
          for (const statistic of ["value", "ci_low", "ci_high"]) {
            const value = valueAt(point, statistic, part);
            if (value !== null) values.push(value);
          }
        }
      }
    }
  }
  const reference = bars.reference;
  if (reference !== null)
    values.push(reference.y, ...(reference.type === "band" ? [reference.yEnd] : []));
  for (const note of bars.annotations) if (note.y !== null) values.push(note.y);
  const axis = bars.valueAxis;
  const yScale = axis.log
    ? logScale(values, axis, [bottom, top])
    : linearScale(values, axis, [bottom, top], axis.whole);
  const clampY = (value: number) => Math.min(bottom, Math.max(top, yScale.at(value)));
  const base = axis.log ? bottom : clampY(0);
  const drawable = (value: number | null): value is number =>
    value !== null && !(axis.log && value <= 0);
  // A mock-up and a shape mock-up's own drawing name no number on the plot.
  const noNumbers = bars.noScale || bars.mockup !== null;

  // An upright bar's or point's marks at one place of one group.
  const uprightMarks = (one: BarsSeries, index: number, x: number, width: number): ReactNode => {
    const point = one.points[index] as BarsPoint;
    const value = valueAt(point, "value", "window");
    const classes = tones(point.tone ?? one.tone);
    const nodes: ReactNode[] = [];
    let labelY = bottom;
    if (point.held && value === null) {
      // A reading with no value draws no mark: its reason stands at the foot of its place.
      const reason = point.reason === "" ? "not measured" : point.reason;
      nodes.push(
        <text
          className={chartPlot.text}
          data-reason={point.x}
          fontSize={9}
          key="reason"
          opacity={0.75}
          textAnchor="middle"
          x={x}
          y={bottom - 3}
        >
          {/* A reading's own words, drawn as plain text: `eval-reading-reason` */}
          {cut(reason, Math.max(4, Math.floor(width / 5)))}
          <title>{tip(one, point, "window")}</title>
        </text>
      );
    }
    if (point.held && drawable(value)) {
      const y = clampY(value);
      const low = valueAt(point, "ci_low", "window");
      const high = valueAt(point, "ci_high", "window");
      const interval = low !== null && high !== null;
      const soFar = bars.grouped && bars.mark === "bar" && point.bound === ">=" && !interval;
      if (bars.mark === "bar") {
        nodes.push(
          <rect
            className={soFar ? `fill-none ${classes.stroke}` : `${classes.fill} ${classes.stroke}`}
            height={Math.max(1, Math.abs(base - y))}
            key="bar"
            strokeDasharray={soFar ? "3 2" : undefined}
            strokeWidth={1}
            width={width}
            x={x - width / 2}
            y={Math.min(base, y)}
            {...valueNames(model, one, point, "value", "window")}
          >
            <title>{tip(one, point, "window")}</title>
          </rect>
        );
      } else {
        nodes.push(
          <circle
            className={`${classes.fill} ${classes.stroke}`}
            cx={x}
            cy={y}
            key="point"
            r={POINT}
            strokeWidth={1.5}
            {...valueNames(model, one, point, "value", "window")}
          >
            <title>{tip(one, point, "window")}</title>
          </circle>
        );
      }
      if (interval && drawable(low) && drawable(high)) {
        nodes.push(
          <g key="interval">
            <line
              className={bars.mark === "bar" ? chartPlot.rule : classes.stroke}
              strokeWidth={1}
              x1={x}
              x2={x}
              y1={clampY(low)}
              y2={clampY(high)}
            />
            {(["ci_low", "ci_high"] as const).map((statistic) => (
              <line
                className={bars.mark === "bar" ? chartPlot.rule : classes.stroke}
                key={statistic}
                strokeWidth={1}
                x1={x - 3}
                x2={x + 3}
                y1={clampY(statistic === "ci_low" ? low : high)}
                y2={clampY(statistic === "ci_low" ? low : high)}
                {...valueNames(model, one, point, statistic, "window")}
              />
            ))}
          </g>
        );
      }
      if (point.bound !== null && !soFar) {
        nodes.push(
          <path
            className={`${chartPlot.hollow} ${classes.stroke}`}
            d={boundMark(point.bound, x, point.bound === ">=" ? y - 2 : y + 2, false)}
            key="bound"
            strokeWidth={1}
          />
        );
      }
      labelY = Math.min(y, interval && drawable(high) ? clampY(high) : y) - 4;
      const totalValue = valueAt(point, "total", "window");
      const label = point.values.value?.label ?? "";
      if (bars.mark === "bar" && !noNumbers) {
        nodes.push(
          <text
            className={chartPlot.text}
            data-value-label={point.x}
            fontSize={10}
            key="label"
            textAnchor="middle"
            x={x}
            y={Math.max(top + 9, labelY)}
          >
            <tspan {...valueNames(model, one, point, "value", "window")}>{label}</tspan>
            {totalValue === null ? null : (
              <>
                {" of "}
                <tspan {...valueNames(model, one, point, "total", "window")}>
                  {point.values.total?.label}
                </tspan>
              </>
            )}
            {soFar ? " so far" : null}
          </text>
        );
      } else if (bars.labels && !noNumbers) {
        nodes.push(
          <text
            className={chartPlot.text}
            data-value-label={point.x}
            fontSize={10}
            key="label"
            x={x + 6}
            y={y + 3}
            {...valueNames(model, one, point, "value", "window")}
          >
            {label}
          </text>
        );
      }
      if (bars.mark === "point" && !interval) {
        nodes.push(
          <text
            className={chartPlot.text}
            fontSize={9}
            key="no-interval"
            opacity={0.75}
            x={x + 6}
            y={y + (bars.labels ? 14 : 3)}
          >
            no interval
          </text>
        );
      }
    }
    // The compare range's newest value: a tick across a bar, a muted ghost beside a point, and
    // the change from it printed over the value.
    const compareValue = valueAt(point, "value", "compare");
    if (compare && point.compareHeld && drawable(compareValue)) {
      const y = clampY(compareValue);
      if (bars.mark === "bar") {
        nodes.push(
          <line
            className={chartLine.compareStroke}
            key="compare"
            strokeWidth={2}
            x1={x - width / 2 - 2}
            x2={x + width / 2 + 2}
            y1={y}
            y2={y}
            {...valueNames(model, one, point, "value", "compare")}
          >
            <title>{tip(one, point, "compare")}</title>
          </line>
        );
      } else {
        const low = valueAt(point, "ci_low", "compare");
        const high = valueAt(point, "ci_high", "compare");
        nodes.push(
          <g key="compare">
            {drawable(low) && drawable(high) ? (
              <line
                className={chartLine.compareStroke}
                strokeWidth={1}
                x1={x - 7}
                x2={x - 7}
                y1={clampY(low)}
                y2={clampY(high)}
              />
            ) : null}
            {(["ci_low", "ci_high"] as const).map((statistic) => {
              const end = statistic === "ci_low" ? low : high;
              return drawable(end) ? (
                <line
                  className={chartLine.compareStroke}
                  key={statistic}
                  strokeWidth={1}
                  x1={x - 9}
                  x2={x - 5}
                  y1={clampY(end)}
                  y2={clampY(end)}
                  {...valueNames(model, one, point, statistic, "compare")}
                />
              ) : null;
            })}
            <circle
              className={`${chartPlot.hollow} ${chartLine.compareStroke}`}
              cx={x - 7}
              cy={y}
              r={POINT - 0.5}
              strokeWidth={1.5}
              {...valueNames(model, one, point, "value", "compare")}
            >
              <title>{tip(one, point, "compare")}</title>
            </circle>
          </g>
        );
      }
      if (valueAt(point, "value", "change") !== null) {
        const over =
          point.held && drawable(value) ? labelY - (bars.mark === "bar" ? 11 : 0) : y - 6;
        nodes.push(
          <text
            className={chartPlot.text}
            data-change-label={point.x}
            fontSize={9}
            key="change"
            opacity={0.75}
            textAnchor="middle"
            x={x}
            y={Math.max(top + 8, bars.mark === "bar" ? over : Math.min(over, labelY - 4))}
            {...valueNames(model, one, point, "value", "change")}
          >
            {point.change?.value?.label}
          </text>
        );
      }
    }
    return nodes;
  };

  // A stack's segments at one place, the window's or the compare range's (a muted ghost).
  const stackMarks = (index: number, part: "window" | "compare", x: number, width: number) =>
    stackOf(index, part).map((segment, order) => {
      const one = groups[order] as BarsSeries;
      const point = one.points[index] as BarsPoint;
      if (segment === null) return null;
      const y0 = clampY(Math.max(segment.from, segment.to));
      const y1 = clampY(Math.min(segment.from, segment.to));
      const tall = y1 - y0;
      const muted = part === "compare";
      const classes = tones(point.tone ?? one.tone);
      const change = valueAt(point, "value", "change");
      return (
        <g data-segment={one.name} key={`${part}-${one.name}`}>
          {/* A half-pixel gap parts each segment from the next. */}
          <rect
            className={muted ? `${chartBar.compare} ${chartLine.compareStroke}` : classes.fill}
            height={Math.max(0.5, tall - 1)}
            strokeWidth={muted ? 1 : 0}
            width={width}
            x={x - width / 2}
            y={y0 + 0.5}
            {...valueNames(model, one, point, "value", part)}
          >
            <title>{tip(one, point, part)}</title>
          </rect>
          {!muted && !noNumbers && tall >= 12 && width >= 18 ? (
            <text
              className={chartPlot.text}
              fontSize={9}
              textAnchor="middle"
              x={x}
              y={
                y0 +
                tall / 2 +
                (compare && change !== null && !bars.overTime && tall >= 24 ? -1 : 3)
              }
              {...valueNames(model, one, point, "value", "window")}
            >
              {point.values.value?.label}
            </text>
          ) : null}
          {!muted && compare && !bars.overTime && change !== null && tall >= 24 && width >= 18 ? (
            <text
              className={chartPlot.text}
              fontSize={8}
              opacity={0.75}
              textAnchor="middle"
              x={x}
              y={y0 + tall / 2 + 9}
              {...valueNames(model, one, point, "value", "change")}
            >
              {point.change?.value?.label}
            </text>
          ) : null}
        </g>
      );
    });

  // A stacked area: each group a band over the shown dates, with a mark at each value.
  const areaMarks = (part: "window" | "compare"): ReactNode => {
    const stacks = shown.map((index) => stackOf(index, part));
    return groups.map((one, order) => {
      const placed = shown.flatMap((index, at) => {
        const segment = stacks[at]?.[order] ?? null;
        return segment === null ? [] : [{ at, index, segment }];
      });
      if (placed.length === 0) return null;
      const classes = tones(one.tone);
      const tops = placed.map(({ at, segment }) => `${centre(at)},${clampY(segment.to)}`);
      const feet = placed
        .map(({ at, segment }) => `${centre(at)},${clampY(segment.from)}`)
        .reverse();
      const muted = part === "compare";
      return (
        <g data-area={one.name} data-area-range={part} key={`${part}-${one.name}`}>
          {muted ? (
            <polyline
              className={`fill-none ${chartLine.compareStroke}`}
              points={tops.join(" ")}
              strokeDasharray="4 3"
              strokeWidth={1.5}
            />
          ) : (
            <polygon className={classes.fill} opacity={0.8} points={[...tops, ...feet].join(" ")} />
          )}
          {placed.map(({ at, index, segment }) => {
            const point = one.points[index] as BarsPoint;
            return (
              <circle
                className={
                  muted
                    ? `${chartPlot.hollow} ${chartLine.compareStroke}`
                    : `${classes.fill} ${classes.stroke}`
                }
                cx={centre(at)}
                cy={clampY(segment.to)}
                key={point.x}
                r={2.5}
                strokeWidth={1}
                {...valueNames(model, one, point, "value", part)}
              >
                <title>{tip(one, point, part)}</title>
              </circle>
            );
          })}
        </g>
      );
    });
  };

  const annotationMark = (note: BarsAnnotation, index: number): ReactNode => {
    const x =
      note.category === null
        ? note.at === null
          ? null
          : xOfTime(note.at)
        : centreOf(note.category);
    if (x === null) return null;
    const classes = note.state === null ? null : tones(note.state);
    if (note.y !== null) {
      const y = clampY(note.y);
      return (
        <g data-annotation={index} key={index}>
          <circle className={classes?.fill ?? chartPlot.text} cx={x} cy={y} r={POINT} />
          <AnnotationText anchor="start" note={note} x={x + 6} y={y - 4} />
        </g>
      );
    }
    if (note.category !== null) {
      return (
        <g data-annotation={index} key={index}>
          <AnnotationText anchor="middle" note={note} x={x} y={MARGIN.top + 8} />
        </g>
      );
    }
    return (
      <g data-annotation={index} key={index}>
        <line
          className={classes?.stroke ?? chartPlot.rule}
          strokeDasharray="3 3"
          x1={x}
          x2={x}
          y1={top}
          y2={bottom}
        />
        <AnnotationText anchor="start" note={note} x={x + 3} y={top + 10} />
      </g>
    );
  };

  const referenceMark = (): ReactNode => {
    if (reference === null) return null;
    if (reference.type === "rule") {
      const y = clampY(reference.y);
      const classes = reference.state === null ? null : tones(reference.state);
      return (
        <g data-reference="rule">
          <line
            className={classes?.stroke ?? chartPlot.rule}
            strokeDasharray={reference.state === null ? "4 3" : undefined}
            x1={left}
            x2={plotRight}
            y1={y}
            y2={y}
          />
          <text className={chartPlot.text} fontSize={10} textAnchor="end" x={plotRight} y={y - 3}>
            {reference.label}
          </text>
        </g>
      );
    }
    const high = clampY(reference.yEnd);
    return (
      <g data-reference="band">
        <rect
          className={chartPlot.band}
          height={Math.max(1, clampY(reference.y) - high)}
          width={plotRight - left}
          x={left}
          y={high}
        />
        <text className={chartPlot.text} fontSize={10} x={left + 3} y={high + 11}>
          {reference.label}
        </text>
      </g>
    );
  };

  // Each group named beside its newest segment, right of the last stack, with its newest value
  // over time; by row its name alone.
  const directLabels = !direct
    ? null
    : groups.map((one, order) => {
        let last: { at: number; index: number } | null = null;
        for (const [at, index] of shown.entries()) {
          if (stackOf(index, "window")[order] !== null) last = { at, index };
        }
        if (last === null) return null;
        const { at, index } = last;
        const segment = stackOf(index, "window")[order] as { from: number; to: number };
        const point = one.points[index] as BarsPoint;
        return (
          <text
            className={chartPlot.text}
            data-direct-label={one.name}
            fontSize={10}
            key={one.name}
            x={plotRight + 6}
            y={(clampY(segment.from) + clampY(segment.to)) / 2 + 3}
          >
            {one.group}
            {bars.overTime ? (
              <tspan dx={4} {...valueNames(model, one, point, "value", "window")}>
                {point.values.value?.label}
              </tspan>
            ) : null}
            <title>{`${one.group}, ${places[at]?.label ?? ""}`}</title>
          </text>
        );
      });

  // The ticks of a time axis: each Monday where it ticks weeks, else about six days.
  const timeTicks: number[] = [];
  if (bars.overTime && places.length > 0) {
    const weekly = bars.weekTicks;
    const unit = weekly ? 7 * DAY : DAY;
    const every = Math.max(1, Math.ceil((timeHigh - timeLow) / unit / 6));
    const start = weekly
      ? Math.ceil((timeLow - 4 * DAY) / unit) * unit + 4 * DAY
      : Math.ceil(timeLow / DAY) * DAY;
    for (let tick = start; tick < timeHigh; tick += unit * every) timeTicks.push(tick);
  }
  const unitTitle = axis.unit === "" || noNumbers ? axis.title : `${axis.title} (${axis.unit})`;

  return (
    <svg
      aria-label={model.title}
      className="block h-auto w-full select-none"
      role="img"
      viewBox={`0 0 ${WIDTH} ${height}`}
    >
      <defs>
        <clipPath id={clipId}>
          <rect height={bottom - top + 1} width={plotRight - left} x={left} y={top} />
        </clipPath>
        <Hatch id={hatchId} />
      </defs>
      {yScale.ticks.map((tick) => (
        <g key={`y${tick}`}>
          <line
            className={chartPlot.grid}
            x1={left}
            x2={plotRight}
            y1={yScale.at(tick)}
            y2={yScale.at(tick)}
          />
          {noNumbers ? null : (
            <text
              className={chartPlot.text}
              fontSize={10}
              textAnchor="end"
              x={left - 4}
              y={yScale.at(tick) + 3}
            >
              {tickLabel(tick, false)}
            </text>
          )}
        </g>
      ))}
      <line className={chartPlot.axis} x1={left} x2={plotRight} y1={base} y2={base} />
      {bars.overTime
        ? timeTicks.map((tick) => {
            const day = new Date(tick);
            return (
              <text
                className={chartPlot.text}
                fontSize={10}
                key={`x${tick}`}
                textAnchor="start"
                x={xOfTime(tick) + 2}
                y={bottom + 13}
              >
                {`${day.getUTCDate()} ${MONTHS[day.getUTCMonth()]}`}
              </text>
            );
          })
        : places.map((place, index) => (
            <text
              className={chartPlot.text}
              data-place-label={place.x}
              fontSize={10}
              key={`x${place.x}`}
              textAnchor="middle"
              x={centre(index)}
              y={bottom + (bars.grouped && bars.legend === "direct" ? 24 : 13)}
            >
              {cut(
                place.category === "" ? place.label : place.category,
                Math.max(3, Math.floor(slot / 6))
              )}
              <title>{place.label}</title>
            </text>
          ))}
      <text
        className={chartPlot.text}
        fontSize={10}
        textAnchor="middle"
        x={(left + plotRight) / 2}
        y={height - 6}
      >
        {bars.placeTitle}
      </text>
      <text
        className={chartPlot.text}
        fontSize={10}
        textAnchor="middle"
        transform={`rotate(-90 12 ${(top + bottom) / 2})`}
        x={12}
        y={(top + bottom) / 2}
      >
        {unitTitle}
      </text>
      {bars.annotations
        .filter((note) => note.category !== null && note.y === null)
        .map(annotationMark)}
      <g clipPath={`url(#${clipId})`}>
        {referenceMark()}
        {bars.annotations
          .map((note, index) => ({ index, note }))
          .filter(({ note }) => !(note.category !== null && note.y === null))
          .map(({ index, note }) => annotationMark(note, index))}
        {bars.layout === "area" ? (
          <>
            {areaMarks("window")}
            {compare ? areaMarks("compare") : null}
          </>
        ) : null}
        {bars.layout === "stack"
          ? shown.map((index, at) => {
              const x = centre(at);
              const ghost = Math.min(slot * 0.22, 20);
              const width = Math.min(slot * (compare ? 0.5 : 0.7), 48);
              return (
                <g data-place={bars.places[index]?.x} key={bars.places[index]?.x ?? index}>
                  {stackMarks(index, "window", compare ? x - ghost / 2 - 1 : x, width)}
                  {compare ? (
                    <g data-ghost>{stackMarks(index, "compare", x + width / 2 + 1, ghost)}</g>
                  ) : null}
                </g>
              );
            })
          : null}
      </g>
      {bars.layout === "upright"
        ? shown.map((index, at) => {
            const marks = bars.grouped ? groups : groups.slice(0, 1);
            const cluster = slot * 0.76;
            const each = cluster / Math.max(1, marks.length);
            const width = Math.min(each * 0.84, 56);
            return (
              <g data-place={bars.places[index]?.x} key={bars.places[index]?.x ?? index}>
                {marks.map((one, order) => {
                  const x = centre(at) - cluster / 2 + (order + 0.5) * each;
                  const point = one.points[index] as BarsPoint;
                  // A grouped bar names each group under its bar in every cluster; a grouped
                  // interval names each over its point in the first cluster.
                  const named =
                    bars.grouped && bars.legend === "direct" && (bars.mark === "bar" || at === 0);
                  const value = valueAt(point, "value", "window");
                  return (
                    <g data-series-mark={one.name} key={one.name}>
                      {uprightMarks(one, index, x, width)}
                      {named ? (
                        <text
                          className={chartPlot.text}
                          data-direct-label={one.name}
                          fontSize={8}
                          textAnchor="middle"
                          x={x}
                          y={
                            bars.mark === "bar"
                              ? bottom + 11
                              : value === null
                                ? top + 8
                                : Math.max(
                                    top + 8,
                                    clampY(valueAt(point, "ci_high", "window") ?? value) - 8
                                  )
                          }
                        >
                          {cut(one.group, Math.max(3, Math.floor(each / 5)))}
                        </text>
                      ) : null}
                    </g>
                  );
                })}
              </g>
            );
          })
        : null}
      {directLabels}
      {bars.mockup === null ? null : (
        <MockupCover bottom={bottom} hatchId={hatchId} left={left} right={plotRight} top={top} />
      )}
    </svg>
  );
}

/** The plot of a horizontal bar: one row per shown place, its name, its bar, dot or dots against
 *  the value axis (against its total, a share of its pale track), its value, its note, and each
 *  row column's cell right of the plot. */
function RowsPlot(props: {
  compare: boolean;
  model: BarsModel;
  shown: readonly number[];
}): ReactNode {
  const { compare, model, shown } = props;
  const { bars } = model;
  const hatchId = useId();
  const one = model.series[0];
  const columns = bars.columns;
  if (one === undefined) return null;
  const tones = (tone: Tone) => toneClasses(tone, bars.notStartedGrey);
  const notes = bars.rowNotes || bars.annotations.length > 0;
  const nameWidth = 150;
  const plotWidth = 300;
  const labelWidth = bars.of ? 130 : 80;
  const noteWidth = notes ? 170 : 0;
  const columnWidth = 100;
  const width = nameWidth + plotWidth + labelWidth + noteWidth + columns.length * columnWidth + 8;
  const left = nameWidth;
  const right = nameWidth + plotWidth;
  const header = bars.plotHeader !== null || columns.length > 0 ? 16 : 0;
  const top = 8 + header;
  const headings = shown.filter((index) => bars.places[index]?.heading != null).length;
  const headingHeight = 18;
  const footer = 36;
  const rowHeight =
    bars.height === null
      ? 22
      : Math.max(
          14,
          (bars.height - top - footer - headings * headingHeight) / Math.max(1, shown.length)
        );
  const rowsBottom = top + shown.length * rowHeight + headings * headingHeight;
  const height = rowsBottom + footer;
  const axis = bars.valueAxis;
  const parts: ("window" | "compare")[] = compare ? ["window", "compare"] : ["window"];

  // A row's length on the value axis: its value, or against its total its share of it.
  const lengthOf = (point: BarsPoint, part: "window" | "compare"): number | null => {
    const value = valueAt(point, "value", part);
    if (value === null || !axis.share) return value;
    const total = valueAt(point, "total", part);
    return total === null || total === 0 ? null : value / total;
  };
  const values: number[] = [];
  if (axis.share) values.push(0, 1);
  else {
    if (bars.mark === "bar" && !axis.log) values.push(0);
    for (const index of shown) {
      const point = one.points[index] as BarsPoint;
      for (const part of parts) {
        for (const statistic of ["value", "ci_low", "ci_high"]) {
          const value = valueAt(point, statistic, part);
          if (value !== null) values.push(value);
        }
      }
    }
    if (bars.reference !== null) {
      values.push(
        bars.reference.y,
        ...(bars.reference.type === "band" ? [bars.reference.yEnd] : [])
      );
    }
  }
  const scale = axis.share
    ? linearScale(values, { max: 1, min: 0 }, [left, right], false)
    : axis.log
      ? logScale(values, axis, [left, right])
      : linearScale(values, axis, [left, right], axis.whole);
  const clampX = (value: number) => Math.min(right, Math.max(left, scale.at(value)));
  const base = axis.log || axis.share ? left : clampX(0);
  const drawable = (value: number | null): value is number =>
    value !== null && !(axis.log && !axis.share && value <= 0);
  const noteFor = (category: string) =>
    bars.annotations.filter((note) => note.category === category);

  let cursor = top;
  const rows = shown.map((index) => {
    const place = bars.places[index];
    const point = one.points[index] as BarsPoint;
    if (place === undefined) return null;
    const heading = place.heading;
    const headingY = cursor;
    if (heading !== null) cursor += headingHeight;
    const y = cursor + rowHeight / 2;
    cursor += rowHeight;
    const classes = tones(point.tone ?? one.tone);
    const length = point.held ? lengthOf(point, "window") : null;
    const total = valueAt(point, "total", "window");
    const dots =
      bars.mark === "dots" && total !== null && Number.isInteger(total) && total > 0 && total <= 20;
    const value = valueAt(point, "value", "window");
    let end = left;
    const marks: ReactNode[] = [];
    if (axis.share && point.held) {
      marks.push(
        <rect
          className={chartPlot.band}
          height={8}
          key="track"
          width={right - left}
          x={left}
          y={y - 4}
        />
      );
    }
    if (point.held && value !== null && dots && total !== null) {
      const step = Math.min(16, (right - left) / total);
      for (let dot = 0; dot < total; dot++) {
        marks.push(
          <circle
            className={
              dot < Math.round(value)
                ? `${classes.fill} ${classes.stroke}`
                : `${chartPlot.hollow} ${classes.stroke}`
            }
            cx={left + step * (dot + 0.5)}
            cy={y}
            key={`dot-${dot}`}
            r={Math.min(5, step / 2 - 1)}
            strokeWidth={1}
          />
        );
      }
      end = left + step * total;
    } else if (drawable(length)) {
      const x = clampX(length);
      end = Math.max(x, base);
      if (bars.mark === "dot") {
        if (columns.length > 0) {
          marks.push(
            <line className={chartPlot.grid} key="leader" x1={left} x2={x} y1={y} y2={y} />
          );
        }
        marks.push(
          <circle
            className={`${classes.fill} ${classes.stroke}`}
            cx={x}
            cy={y}
            key="dot"
            r={POINT + 0.5}
            strokeWidth={1.5}
            {...valueNames(model, one, point, "value", "window")}
          >
            <title>{tip(one, point, "window")}</title>
          </circle>
        );
      } else {
        marks.push(
          <rect
            className={`${classes.fill} ${classes.stroke}`}
            height={Math.min(12, rowHeight - 6)}
            key="bar"
            strokeWidth={1}
            width={Math.max(1, Math.abs(x - base))}
            x={Math.min(x, base)}
            y={y - Math.min(12, rowHeight - 6) / 2}
            {...valueNames(model, one, point, "value", "window")}
          >
            <title>{tip(one, point, "window")}</title>
          </rect>
        );
      }
      const low = valueAt(point, "ci_low", "window");
      const high = valueAt(point, "ci_high", "window");
      if (!axis.share && drawable(low) && drawable(high)) {
        end = Math.max(end, clampX(high));
        marks.push(
          <g key="interval">
            <line className={chartPlot.rule} x1={clampX(low)} x2={clampX(high)} y1={y} y2={y} />
            {(["ci_low", "ci_high"] as const).map((statistic) => (
              <line
                className={chartPlot.rule}
                key={statistic}
                x1={clampX(statistic === "ci_low" ? low : high)}
                x2={clampX(statistic === "ci_low" ? low : high)}
                y1={y - 3}
                y2={y + 3}
                {...valueNames(model, one, point, statistic, "window")}
              />
            ))}
          </g>
        );
      }
      if (point.bound !== null) {
        marks.push(
          <path
            className={`${chartPlot.hollow} ${classes.stroke}`}
            d={boundMark(point.bound, point.bound === ">=" ? x + 2 : x - 2, y, true)}
            key="bound"
            strokeWidth={1}
          />
        );
      }
    }
    const compareLength = compare && point.compareHeld ? lengthOf(point, "compare") : null;
    if (drawable(compareLength)) {
      const x = clampX(compareLength);
      marks.push(
        <line
          className={chartLine.compareStroke}
          key="compare"
          strokeWidth={2}
          x1={x}
          x2={x}
          y1={y - 7}
          y2={y + 7}
          {...valueNames(model, one, point, "value", "compare")}
        >
          <title>{tip(one, point, "compare")}</title>
        </line>
      );
    }
    const change = compare ? valueAt(point, "value", "change") : null;
    const reason =
      point.held && value === null ? (point.reason === "" ? "not measured" : point.reason) : null;
    const rowNotes = bars.rowNotes
      ? point.note === ""
        ? []
        : [point.note]
      : noteFor(place.category).map((note) => note.label);
    const proved = point.tone === "proved" && columns.length > 0;
    return (
      <g data-row={place.x} key={place.x}>
        {heading === null ? null : (
          <text
            className={chartPlot.text}
            data-row-heading
            fontSize={10}
            fontWeight={600}
            x={4}
            y={headingY + 13}
          >
            {/* Its definition's words, drawn as plain text: `eval-chart-heading` */}
            {heading}
          </text>
        )}
        <text
          className={chartPlot.text}
          fontSize={10}
          fontWeight={proved ? 600 : undefined}
          x={4}
          y={y + 3}
        >
          {cut(place.label, 26)}
          <title>{place.label}</title>
        </text>
        {marks}
        <text
          className={chartPlot.text}
          data-value-label={place.x}
          fontSize={10}
          opacity={reason === null ? undefined : 0.75}
          x={(dots ? end : Math.max(end, base)) + 6}
          y={y + 3}
        >
          {reason !== null ? (
            // A reading's own words, drawn as plain text: `eval-reading-reason`
            cut(reason, Math.floor(labelWidth / 5))
          ) : value === null || bars.mockup !== null ? null : (
            <>
              <tspan {...valueNames(model, one, point, "value", "window")}>
                {point.values.value?.label}
              </tspan>
              {total === null ? null : (
                <>
                  {" of "}
                  <tspan {...valueNames(model, one, point, "total", "window")}>
                    {point.values.total?.label}
                  </tspan>
                </>
              )}
            </>
          )}
          {change === null ? null : (
            <tspan dx={6} opacity={0.75} {...valueNames(model, one, point, "value", "change")}>
              {point.change?.value?.label}
            </tspan>
          )}
        </text>
        {rowNotes.length === 0 ? null : (
          <text
            className={chartPlot.text}
            data-row-note={place.x}
            fontSize={9}
            opacity={0.8}
            x={right + labelWidth}
            y={y + 3}
          >
            {cut(rowNotes.join("; "), Math.floor(noteWidth / 5))}
            <title>{rowNotes.join("; ")}</title>
          </text>
        )}
        {columns.map((column, at) => {
          const text = point.values[column.statistic];
          const x = right + labelWidth + noteWidth + at * columnWidth;
          return (
            <text
              className={chartPlot.text}
              data-column={column.statistic}
              fontSize={10}
              key={column.statistic}
              opacity={text?.state === "absent" ? 0.6 : undefined}
              textAnchor={column.field === "value" ? "end" : "start"}
              x={column.field === "value" ? x + columnWidth - 8 : x}
              y={y + 3}
              {...valueNames(model, one, point, column.statistic, "window")}
            >
              {cut(text?.label ?? "", Math.floor(columnWidth / 5.5))}
              <title>{text?.label ?? ""}</title>
            </text>
          );
        })}
      </g>
    );
  });

  return (
    <svg
      aria-label={model.title}
      className="block h-auto w-full select-none"
      role="img"
      viewBox={`0 0 ${width} ${height}`}
    >
      <defs>
        <Hatch id={hatchId} />
      </defs>
      {bars.plotHeader === null ? null : (
        <text className={chartPlot.text} fontSize={10} fontWeight={600} x={left} y={top - 6}>
          {bars.plotHeader}
        </text>
      )}
      {columns.map((column, at) => {
        const x = right + labelWidth + noteWidth + at * columnWidth;
        return (
          <text
            className={chartPlot.text}
            fontSize={10}
            fontWeight={600}
            key={column.statistic}
            textAnchor={column.field === "value" ? "end" : "start"}
            x={column.field === "value" ? x + columnWidth - 8 : x}
            y={top - 6}
          >
            {cut(column.header, Math.floor(columnWidth / 6))}
            <title>{column.header}</title>
          </text>
        );
      })}
      {scale.ticks.map((tick) => (
        <g key={`t${tick}`}>
          <line
            className={chartPlot.grid}
            x1={scale.at(tick)}
            x2={scale.at(tick)}
            y1={top}
            y2={rowsBottom}
          />
          {bars.noScale ? null : (
            <text
              className={chartPlot.text}
              fontSize={10}
              textAnchor="middle"
              x={scale.at(tick)}
              y={rowsBottom + 12}
            >
              {tickLabel(tick, axis.share)}
            </text>
          )}
        </g>
      ))}
      <text
        className={chartPlot.text}
        fontSize={10}
        textAnchor="middle"
        x={(left + right) / 2}
        y={rowsBottom + 28}
      >
        {axis.share
          ? `${axis.title}, share of each row's total`
          : axis.unit === ""
            ? axis.title
            : `${axis.title} (${axis.unit})`}
      </text>
      {bars.reference === null ? null : bars.reference.type === "rule" ? (
        <g data-reference="rule">
          <line
            className={
              bars.reference.state === null ? chartPlot.rule : tones(bars.reference.state).stroke
            }
            strokeDasharray={bars.reference.state === null ? "4 3" : undefined}
            x1={clampX(bars.reference.y)}
            x2={clampX(bars.reference.y)}
            y1={top}
            y2={rowsBottom}
          />
          <text
            className={chartPlot.text}
            fontSize={10}
            textAnchor="middle"
            x={clampX(bars.reference.y)}
            y={top - 2}
          >
            {bars.reference.label}
          </text>
        </g>
      ) : (
        <g data-reference="band">
          <rect
            className={chartPlot.band}
            height={rowsBottom - top}
            width={Math.max(1, clampX(bars.reference.yEnd) - clampX(bars.reference.y))}
            x={clampX(bars.reference.y)}
            y={top}
          />
          <text
            className={chartPlot.text}
            fontSize={10}
            x={clampX(bars.reference.y) + 3}
            y={top - 2}
          >
            {bars.reference.label}
          </text>
        </g>
      )}
      {rows}
      {bars.mockup === null ? null : (
        <MockupCover bottom={rowsBottom} hatchId={hatchId} left={left} right={right} top={top} />
      )}
    </svg>
  );
}

/** A facet table: a row per shown place and a column per group, each cell a short bar of its
 *  value scaled within its row (its parts stacked from the left), its value beside it and its
 *  note under it; with the compare range shown, the compare value as a tick on the bar and the
 *  change after the value. */
function FacetTable(props: {
  compare: boolean;
  model: BarsModel;
  shown: readonly number[];
}): ReactNode {
  const { compare, model, shown } = props;
  const { bars } = model;
  const groups = model.series;
  const tones = (tone: Tone) => toneClasses(tone, bars.notStartedGrey);
  return (
    <table className={`w-full border-collapse text-xs ${textPrimaryOnSurface}`} data-facet>
      <thead>
        <tr>
          <th className="text-left font-medium" scope="col">
            <span className="sr-only">Row</span>
          </th>
          {groups.map((one) => (
            <th className="px-1 text-left font-medium" key={one.name} scope="col">
              {one.group}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {shown.map((index) => {
          const place = bars.places[index];
          if (place === undefined) return null;
          const lengths = groups.flatMap((one) => {
            const point = one.points[index] as BarsPoint;
            return [
              valueAt(point, "value", "window"),
              compare ? valueAt(point, "value", "compare") : null,
            ].filter((value): value is number => value !== null && value > 0);
          });
          const largest = Math.max(0, ...lengths);
          return (
            <tr className="align-top" data-row={place.x} key={place.x}>
              <th className="py-1 pr-2 text-left font-medium" scope="row">
                {place.label}
                {place.unit === "" ? null : (
                  <span className={textMutedOnSurface}>{` (${place.unit})`}</span>
                )}
                {place.subtitle === null ? null : (
                  <span className={`block font-normal ${textSecondaryOnCanvas}`}>
                    {place.subtitle}
                  </span>
                )}
              </th>
              {groups.map((one) => {
                const point = one.points[index] as BarsPoint;
                const value = valueAt(point, "value", "window");
                const compareValue = compare ? valueAt(point, "value", "compare") : null;
                const change = compare ? valueAt(point, "value", "change") : null;
                const tone = point.tone ?? one.tone;
                const bar = value !== null && value > 0 && !place.mixed && largest > 0;
                let from = 0;
                const segments =
                  bars.parts.length === 0
                    ? [{ name: "", tone, value: value ?? 0 }]
                    : bars.parts.flatMap((part) => {
                        const own = valueAt(point, `part:${part.name}`, "window");
                        return own === null || own <= 0
                          ? []
                          : [{ name: part.name, tone: part.state ?? tone, value: own }];
                      });
                return (
                  <td className="px-1 py-1" data-cell={one.name} key={one.name}>
                    {!point.held ? null : point.values.value?.state === "absent" ? (
                      <span className={textMutedOnSurface}>
                        {/* A reading's own words, drawn as plain text: `eval-reading-reason` */}
                        {point.reason === "" ? "not measured" : point.reason}
                      </span>
                    ) : (
                      <span className="flex flex-col gap-0.5">
                        {bar ? (
                          <svg
                            aria-hidden="true"
                            className="block h-2 w-full"
                            preserveAspectRatio="none"
                            viewBox="0 0 100 8"
                          >
                            {segments.map((segment) => {
                              // A part keeps a gap from the next.
                              const width =
                                (segment.value / largest) * 100 - (segment.name === "" ? 0 : 0.8);
                              const x = (from / largest) * 100;
                              from += segment.value;
                              return (
                                <rect
                                  className={tones(segment.tone).fill}
                                  height={8}
                                  key={segment.name}
                                  width={Math.max(0.2, width)}
                                  x={x}
                                  y={0}
                                  {...(segment.name === ""
                                    ? valueNames(model, one, point, "value", "window")
                                    : valueNames(
                                        model,
                                        one,
                                        point,
                                        `part:${segment.name}`,
                                        "window"
                                      ))}
                                />
                              );
                            })}
                            {compareValue !== null && compareValue > 0 ? (
                              <rect
                                className={chartLine.comparePoint}
                                height={8}
                                width={1.2}
                                x={Math.min(98.8, (compareValue / largest) * 100)}
                                y={0}
                                {...valueNames(model, one, point, "value", "compare")}
                              />
                            ) : null}
                          </svg>
                        ) : null}
                        <span>
                          <span {...valueNames(model, one, point, "value", "window")}>
                            {point.values.value?.label}
                          </span>
                          {change === null ? null : (
                            <span
                              className={`ml-1 ${textMutedOnSurface}`}
                              {...valueNames(model, one, point, "value", "change")}
                            >
                              {point.change?.value?.label}
                            </span>
                          )}
                        </span>
                        {point.note === "" ? null : (
                          <span className={textMutedOnSurface}>{point.note}</span>
                        )}
                      </span>
                    )}
                  </td>
                );
              })}
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

/**
 * Draws a `bar`, an `interval` or a `stacked-bar` model (`barsModel`): its mock-up line, its week
 * control (`frames`), its key (each group's colour, or each value state's), its plot by its
 * layout, and the lines under it.
 */
export function BarsRenderer({ compare, model }: ChartRendererProps): ReactNode {
  const [frame, setFrame] = useState(0);
  if (!isBarsModel(model)) return null;
  const { bars } = model;
  const shown =
    bars.frames === null
      ? bars.places.map((_, index) => index)
      : (bars.frames[Math.min(frame, bars.frames.length - 1)]?.places ?? []);
  // With no reading in the window or the compare range, the chart has nothing to place.
  const empty = model.series.every((one) =>
    shown.every((index) => {
      const point = one.points[index];
      return point === undefined || (!point.held && !(compare && point.compareHeld));
    })
  );
  const drawn = bars.mixedUnits.length === 0;
  return (
    <div
      className="flex min-w-0 flex-col gap-1"
      data-bars={bars.kind}
      data-bars-layout={bars.layout}
    >
      {bars.mockup === null ? null : (
        <p className={`text-xs font-medium ${textMutedOnSurface}`} data-mockup-line>
          {bars.mockup}
        </p>
      )}
      {bars.frames === null || bars.frames.length === 0 ? null : (
        <fieldset aria-label="Week" className="flex flex-wrap gap-1">
          {bars.frames.map((one, index) => (
            <button
              aria-pressed={frame === index}
              className={`h-6 rounded-md px-2 text-xs font-medium ${
                frame === index
                  ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}`
                  : textSecondaryOnCanvas
              }`}
              key={one.week}
              onClick={() => setFrame(index)}
              type="button"
            >
              {one.label}
            </button>
          ))}
        </fieldset>
      )}
      {bars.legend === "key" || bars.legend === "states" ? (
        <ul
          aria-label="Key"
          className={`flex flex-wrap gap-x-3 gap-y-0.5 text-xs ${textSecondaryOnCanvas}`}
        >
          {(bars.legend === "states"
            ? bars.valueStates.map((entry) => ({ name: entry.label, tone: entry.state as Tone }))
            : bars.key
          ).map((entry) => (
            <li className="flex items-center gap-1" data-key={entry.name} key={entry.name}>
              <span
                aria-hidden="true"
                className={`inline-block h-2 w-2 rounded-full ${toneClasses(entry.tone, bars.notStartedGrey).swatch}`}
              />
              {entry.name}
            </li>
          ))}
        </ul>
      ) : null}
      {drawn ? null : (
        <p className={`text-xs ${textMutedOnSurface}`} data-mixed-units>
          {`These readings are in more than one unit (${bars.mixedUnits.join(", ")}), so the chart draws no number.`}
        </p>
      )}
      {empty ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-plot-empty>
          No reading of this chart's series in the window.
        </p>
      ) : !drawn ? null : bars.layout === "facet" ? (
        <FacetTable compare={compare} model={model} shown={shown} />
      ) : bars.layout === "horizontal" ? (
        <RowsPlot compare={compare} model={model} shown={shown} />
      ) : (
        <ColumnsPlot compare={compare} model={model} shown={shown} />
      )}
      {bars.notes.length === 0 ? null : (
        <div className={`flex flex-col text-[11px] ${textMutedOnSurface}`} data-plot-notes>
          {bars.notes.map((note) => (
            <p key={note}>{note}</p>
          ))}
        </div>
      )}
    </div>
  );
}
