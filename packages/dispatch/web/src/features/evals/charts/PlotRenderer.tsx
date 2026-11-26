// The renderer of an `xy` and a `band` (dispatch://INFRA-2918/artifact/plan-md, D10): each view of
// the model as an SVG plot on its two axes, its groups' lines, points, error bars and bands, its
// references and annotations, and, while the compare range is shown, the compare range's readings
// muted (over time a second line, a band as an outline; against an x series and by step hollow
// points). Each value it draws names itself as the chart's table names it (`data-chart`,
// `data-series`, `data-statistic`, `data-x`, `data-range`, `data-value`), so the table holds every
// number the graph draws. With `y_options` a button for each option picks the view, with `panels`
// the views stand side by side, with `zoom` a drag on the plot narrows its x axis, and with
// `items` a button for each x lists its items.
import { type PointerEvent, type ReactNode, useId, useRef, useState } from "react";

import {
  chartLine,
  chartPlot,
  secondaryButtonCompact,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../../theme/classes";
import type { ChartRendererProps, ChartValue } from "../chart-model";
import { type Tone, toneClasses } from "./kind";
import {
  type Annotation,
  isPlotModel,
  type PlotModel,
  type PlotPoint,
  type PlotSeries,
  type PlotView,
} from "./plot";

const WIDTH = 640;
const MARGIN = { bottom: 40, left: 60, right: 16, top: 14 };
const DIRECT_LABELS = 112;
const SECOND_AXIS = 60;
const MISSING_STRIP = 56;
const POINT = 3;
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const DAY = 24 * 60 * 60 * 1000;

/** A scale from values to pixels, with its ticks. */
interface Scale {
  readonly at: (value: number) => number;
  readonly ticks: readonly number[];
  readonly low: number;
  readonly high: number;
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
  ends: { min: number | null; max: number | null },
  range: [number, number],
  whole: boolean
): Scale {
  let low = Math.min(...values, ...(ends.min === null ? [] : [ends.min]));
  let high = Math.max(...values, ...(ends.max === null ? [] : [ends.max]));
  if (!Number.isFinite(low) || !Number.isFinite(high)) [low, high] = [0, 1];
  if (low === high)
    [low, high] = low === 0 ? [0, 1] : [low - Math.abs(low) / 2, high + Math.abs(high) / 2];
  const step = niceStep(high - low, 5, whole);
  const from = ends.min ?? low - step / 2;
  const to = ends.max ?? high + step / 2;
  const ticks: number[] = [];
  for (let tick = Math.ceil(from / step) * step; tick <= to + step * 1e-9; tick += step) {
    ticks.push(Number(tick.toPrecision(12)));
  }
  return {
    at: (value) => range[0] + ((value - from) / (to - from || 1)) * (range[1] - range[0]),
    high: to,
    log: false,
    low: from,
    ticks,
  };
}

/** A log scale over the positive `values`: its ends at powers of ten, which it ticks, with 2 and 5
 *  times them where powers of ten alone give fewer than three ticks. */
function logScale(
  values: readonly number[],
  ends: { min: number | null; max: number | null },
  range: [number, number]
): Scale {
  const positive = values.filter((value) => value > 0);
  const lowest = positive.length === 0 ? 1 : Math.min(...positive);
  const highest = positive.length === 0 ? 10 : Math.max(...positive);
  const low = ends.min ?? 10 ** Math.floor(Math.log10(lowest));
  const high = ends.max ?? 10 ** Math.ceil(Math.log10(highest));
  const top = high > low ? high : low * 10;
  const ticks: number[] = [];
  for (let power = Math.ceil(Math.log10(low)); 10 ** power <= top * (1 + 1e-9); power++) {
    ticks.push(10 ** power);
  }
  const all =
    ticks.length >= 3
      ? ticks
      : ticks
          .flatMap((tick) => [tick, tick * 2, tick * 5])
          .concat(ticks.length === 0 ? [low] : [])
          .filter((tick) => tick >= low && tick <= top);
  const span = Math.log10(top) - Math.log10(low) || 1;
  return {
    at: (value) =>
      range[0] +
      ((Math.log10(Math.max(value, low / 1e6)) - Math.log10(low)) / span) * (range[1] - range[0]),
    high: top,
    log: true,
    low,
    ticks: all,
  };
}

/** A time scale: days, or with weekly ticks each Monday, about six ticks across. */
function timeScale(
  values: readonly number[],
  ends: { min: number | null; max: number | null },
  range: [number, number],
  weekly: boolean
): Scale {
  let low = ends.min ?? Math.min(...values);
  let high = ends.max ?? Math.max(...values);
  if (!Number.isFinite(low) || !Number.isFinite(high)) [low, high] = [0, DAY];
  if (low === high) [low, high] = [low - DAY / 2, high + DAY / 2];
  const from = ends.min ?? low - (high - low) * 0.03;
  const to = ends.max ?? high + (high - low) * 0.03;
  const unit = weekly ? 7 * DAY : DAY;
  const every = Math.max(1, Math.ceil((to - from) / unit / 6));
  // A day's first moment, or a Monday's (the epoch began on a Thursday).
  const start = weekly
    ? Math.ceil((from - 4 * DAY) / unit) * unit + 4 * DAY
    : Math.ceil(from / DAY) * DAY;
  const ticks: number[] = [];
  for (let tick = start; tick <= to; tick += unit * every) ticks.push(tick);
  return {
    at: (value) => range[0] + ((value - from) / (to - from)) * (range[1] - range[0]),
    high: to,
    log: false,
    low: from,
    ticks,
  };
}

function tickLabel(value: number, time: boolean): string {
  if (time) {
    const day = new Date(value);
    return `${day.getUTCDate()} ${MONTHS[day.getUTCMonth()]}`;
  }
  return value.toLocaleString("en-US", { maximumFractionDigits: 3 });
}

/** The DOM names of a drawn value, as the chart's table names it. */
function valueNames(
  model: PlotModel,
  series: PlotSeries,
  point: PlotPoint,
  statistic: string,
  range: "window" | "compare",
  value: number
): Record<string, string> {
  return {
    "data-chart": model.name,
    "data-range": range,
    "data-series": series.name,
    "data-statistic": statistic,
    "data-value": String(value),
    "data-x": point.x,
  };
}

/** What a point's tip says: its series, its place, its value and its interval. */
function tip(
  series: PlotSeries,
  point: PlotPoint,
  values: Readonly<Record<string, ChartValue>>,
  compare: boolean
): string {
  const y = values.y?.label ?? "";
  const low = values.ci_low;
  const high = values.ci_high;
  const interval =
    low?.value !== null && low !== undefined && high?.value !== null && high !== undefined
      ? `, interval ${low.label} to ${high.label}`
      : "";
  const x = values.x?.value === null || values.x === undefined ? "" : `, x ${values.x.label}`;
  const note = values.y?.note ? ` (${values.y.note})` : "";
  return `${series.label}, ${point.label}${compare ? ", compare" : ""}${x}: ${y}${interval}${note}`;
}

/** One point's mark: a dot, a ring where its group draws hollow or it is the compare range's, a
 *  censoring triangle for a bound, or a median's level mark. */
function PointMark(props: {
  bound: "<=" | ">=" | null;
  classes: { fill: string; stroke: string };
  hollow: boolean;
  median: boolean;
  names: Record<string, string>;
  shown: boolean;
  title: string;
  tint: boolean;
  x: number;
  y: number;
}): ReactNode {
  const { x, y } = props;
  const paint = props.hollow
    ? `${chartPlot.hollow} ${props.classes.stroke}`
    : `${props.classes.fill} ${props.classes.stroke}`;
  const className = props.shown ? paint : "fill-transparent stroke-transparent";
  const opacity = props.tint ? 0.35 : undefined;
  if (props.median) {
    return (
      <line
        className={props.classes.stroke}
        strokeWidth={3}
        x1={x - 7}
        x2={x + 7}
        y1={y}
        y2={y}
        {...props.names}
      >
        <title>{props.title}</title>
      </line>
    );
  }
  if (props.bound !== null) {
    const d =
      props.bound === ">="
        ? `M${x},${y - 5} L${x + 4.5},${y + 3} L${x - 4.5},${y + 3} Z`
        : `M${x},${y + 5} L${x + 4.5},${y - 3} L${x - 4.5},${y - 3} Z`;
    return (
      <path
        className={props.shown ? `${chartPlot.hollow} ${props.classes.stroke}` : className}
        d={d}
        opacity={opacity}
        strokeWidth={1.5}
        {...props.names}
      >
        <title>{props.title}</title>
      </path>
    );
  }
  return (
    <circle
      className={className}
      cx={x}
      cy={y}
      opacity={opacity}
      r={POINT}
      strokeWidth={1.5}
      {...props.names}
    >
      <title>{props.title}</title>
    </circle>
  );
}

/** A path through `places`, straight or in steps that keep each value until the next. */
function linePath(places: readonly [number, number][], step: boolean): string {
  return places
    .map(([x, y], index) => (index === 0 ? `M${x},${y}` : step ? `H${x} V${y}` : `L${x},${y}`))
    .join(" ");
}

/** The runs of places a line joins: a reading with no value breaks it. */
function runs(places: readonly ([number, number] | null)[]): [number, number][][] {
  const out: [number, number][][] = [];
  let run: [number, number][] = [];
  for (const place of places) {
    if (place === null) {
      if (run.length > 0) out.push(run);
      run = [];
    } else run.push(place);
  }
  if (run.length > 0) out.push(run);
  return out;
}

/** An area from each place's low to its high: along the highs, then back along the lows, each
 *  kept until the next place where the line steps. */
function areaPath(
  places: readonly { x: number; low: number; high: number }[],
  step: boolean
): string {
  const last = places[places.length - 1];
  if (last === undefined) return "";
  const top = linePath(
    places.map(({ x, high }) => [x, high]),
    step
  );
  const back = places
    .slice(0, -1)
    .reverse()
    .map(({ x, low }) => (step ? `V${low} H${x}` : `L${x},${low}`))
    .join(" ");
  return `${top} L${last.x},${last.low} ${back} Z`;
}

/** The value of `statistic` at a point, window's or compare range's. */
function valueAt(point: PlotPoint, statistic: string, range: "window" | "compare"): number | null {
  return (range === "window" ? point.values : point.compare)?.[statistic]?.value ?? null;
}

/** One view's plot. */
function ViewPlot(props: {
  compare: boolean;
  model: PlotModel;
  view: PlotView;
  index: number;
  zoom: [number, number] | null;
  onZoom: (range: [number, number] | null) => void;
  selected: number | null;
}): ReactNode {
  const { compare, model, view } = props;
  const { plot } = model;
  const clipId = useId();
  const hatchId = useId();
  const svg = useRef<SVGSVGElement>(null);
  const [brush, setBrush] = useState<[number, number] | null>(null);
  const series = model.series.filter((one) => one.view === props.index);
  const drawn = view.mixedUnits.length === 0;
  const height = plot.height;
  const right =
    MARGIN.right +
    (plot.legend === "direct" ? DIRECT_LABELS : 0) +
    (plot.secondAxis === null ? 0 : SECOND_AXIS) +
    (plot.missingX ? MISSING_STRIP : 0);
  const left = MARGIN.left;
  const plotRight = WIDTH - right;
  const bottom = height - MARGIN.bottom;
  const top = MARGIN.top;

  // Every place and value the plot draws, which its scales reach.
  const ranges: ("window" | "compare")[] = compare ? ["window", "compare"] : ["window"];
  const xs: number[] = [];
  const ys: number[] = [];
  for (const one of series) {
    for (const point of one.points) {
      for (const range of ranges) {
        const at =
          range === "window"
            ? point.held
              ? point.at
              : null
            : point.compareHeld
              ? point.compareAt
              : null;
        if (at !== null) xs.push(at);
        if (range === "window" ? !point.held : !point.compareHeld) continue;
        for (const statistic of ["y", "ci_low", "ci_high"]) {
          const value = valueAt(point, statistic, range);
          if (value !== null) ys.push(value);
        }
      }
    }
    if (one.fromStart !== null) ys.push(one.fromStart);
  }
  const reference = view.reference;
  if (reference?.type === "rule") ys.push(reference.y);
  if (reference?.type === "band") ys.push(reference.y, reference.yEnd);
  for (const note of view.annotations) {
    xs.push(note.at, ...(note.end === null ? [] : [note.end]));
    if (note.y !== null) ys.push(note.y);
    if (note.yEnd !== null) ys.push(note.yEnd);
  }
  for (const place of plot.items?.places ?? []) xs.push(place.at);

  const xEnds =
    props.zoom === null ? view.x : { ...view.x, max: props.zoom[1], min: props.zoom[0] };
  const stepWidth = plot.steps.length === 0 ? 0 : (plotRight - left) / plot.steps.length;
  const xScale: Scale =
    plot.form === "category"
      ? {
          at: (value) => left + (value + 0.5) * stepWidth,
          high: plot.steps.length - 0.5,
          log: false,
          low: -0.5,
          ticks: plot.steps.map((_, index) => index),
        }
      : plot.form === "time"
        ? timeScale(xs, xEnds, [left, plotRight], view.x.ticks === "week")
        : view.x.log
          ? logScale(xs, xEnds, [left, plotRight])
          : linearScale(xs, xEnds, [left, plotRight], view.x.ticks === "whole");
  const yScale: Scale = view.y.log
    ? logScale(ys, view.y, [bottom, top])
    : linearScale(ys, view.y, [bottom, top], view.y.ticks === "whole");
  const xOf = (value: number) => xScale.at(value);
  const clampY = (value: number) => Math.min(bottom, Math.max(top, yScale.at(value)));

  // A strip's dots of one step stand side by side, in the order of their values.
  const spread = new Map<string, number>();
  if (plot.strip) {
    for (const one of series) {
      const dots = one.points
        .filter((point) => point.held && !point.median && valueAt(point, "y", "window") !== null)
        .sort((a, b) => (valueAt(a, "y", "window") ?? 0) - (valueAt(b, "y", "window") ?? 0));
      const room = stepWidth * 0.4;
      dots.forEach((point, index) => {
        const offset = (index % 2 === 0 ? 1 : -1) * Math.ceil(index / 2) * 7;
        spread.set(`${one.name}\n${point.x}`, Math.max(-room, Math.min(room, offset)));
      });
    }
  }
  const missingAt = plotRight + (plot.legend === "direct" ? DIRECT_LABELS : 0) + MISSING_STRIP / 2;
  const placeX = (one: PlotSeries, point: PlotPoint, at: number | null): number | null => {
    if (at === null) return plot.missingX ? missingAt : null;
    return xOf(at) + (spread.get(`${one.name}\n${point.x}`) ?? 0);
  };

  const press = (event: PointerEvent<SVGRectElement>) => {
    if (!plot.zoom || svg.current === null) return;
    const box = svg.current.getBoundingClientRect();
    const x = ((event.clientX - box.left) / (box.width || 1)) * WIDTH;
    setBrush([x, x]);
  };
  const drag = (event: PointerEvent<SVGRectElement>) => {
    if (brush === null || svg.current === null) return;
    const box = svg.current.getBoundingClientRect();
    setBrush([brush[0], ((event.clientX - box.left) / (box.width || 1)) * WIDTH]);
  };
  const release = () => {
    if (brush === null) return;
    const [a, b] = [Math.min(...brush), Math.max(...brush)];
    setBrush(null);
    if (b - a < 4) return;
    const back = (pixel: number) =>
      xScale.log
        ? 10 **
          (Math.log10(xScale.low) +
            ((pixel - left) / (plotRight - left)) *
              (Math.log10(xScale.high) - Math.log10(xScale.low)))
        : xScale.low + ((pixel - left) / (plotRight - left)) * (xScale.high - xScale.low);
    props.onZoom([back(a), back(b)]);
  };

  const tones = (tone: Tone) => toneClasses(tone, plot.notStartedGrey);
  const noNumbers = plot.noScale || plot.mockup !== null;
  const unitTitle = (title: string, unit: string) => (unit === "" ? title : `${title} (${unit})`);
  const yTitle = plot.noScale ? view.y.title : unitTitle(view.y.title, view.y.unit);
  const xTitle =
    plot.noScale || plot.form !== "number" ? view.x.title : unitTitle(view.x.title, view.x.unit);
  const second = plot.secondAxis;
  const secondTicks =
    second === null ? [] : yScale.ticks.map((tick) => ({ label: tick * second.scale, tick }));
  const secondDraws = secondTicks.every(
    ({ label }) => label === 0 || (Math.abs(label) >= 1e-12 && Math.abs(label) <= 1e15)
  );

  const lineOf = (one: PlotSeries, range: "window" | "compare") => {
    const ordered = one.points
      .filter((point) => (range === "window" ? point.held : point.compareHeld))
      .map((point) => ({
        at: range === "window" ? point.at : point.compareAt,
        y: valueAt(point, "y", range),
      }))
      .filter((place) => place.at !== null)
      .sort((a, b) => (a.at as number) - (b.at as number) || (a.y ?? 0) - (b.y ?? 0));
    return runs(ordered.map(({ at, y }) => (y === null ? null : [xOf(at as number), clampY(y)])));
  };

  const groupLines = (one: PlotSeries, range: "window" | "compare"): ReactNode => {
    const join = plot.line !== "none" || one.joined || one.role === "summary";
    if (!join || (one.role === "median" && !one.joined)) return null;
    const muted = range === "compare";
    return lineOf(one, range).map((run) =>
      run.length < 2 ? null : (
        <path
          className={`fill-none ${muted ? chartLine.compareStroke : tones(one.tone).stroke}`}
          d={linePath(run, plot.line === "step")}
          key={`${range}-${run[0]?.[0]}-${run[0]?.[1]}`}
          opacity={one.role === "run" ? 0.35 : undefined}
          strokeDasharray={muted ? "4 3" : one.joined ? "2 2" : undefined}
          strokeWidth={one.role === "summary" || one.joined ? 1 : 1.5}
        />
      )
    );
  };

  const bandOf = (one: PlotSeries, range: "window" | "compare", outline: boolean): ReactNode => {
    const places = one.points
      .filter((point) => (range === "window" ? point.held : point.compareHeld))
      .map((point) => ({
        at: range === "window" ? point.at : point.compareAt,
        high: valueAt(point, "ci_high", range),
        low: valueAt(point, "ci_low", range),
      }))
      .filter((place) => place.at !== null)
      .sort((a, b) => (a.at as number) - (b.at as number));
    const pieces: { x: number; low: number; high: number }[][] = [];
    let piece: { x: number; low: number; high: number }[] = [];
    for (const place of places) {
      if (place.low === null || place.high === null) {
        if (piece.length > 0) pieces.push(piece);
        piece = [];
        continue;
      }
      piece.push({ high: clampY(place.high), low: clampY(place.low), x: xOf(place.at as number) });
    }
    if (piece.length > 0) pieces.push(piece);
    return pieces.map((part) =>
      part.length < 2 ? null : (
        <path
          className={
            outline
              ? `fill-none ${chartLine.compareStroke}`
              : `${range === "compare" ? "fill-transparent" : tones(one.tone).band} stroke-none`
          }
          d={areaPath(part, plot.line === "step")}
          data-band-range={range}
          key={`${range}-${part[0]?.x}`}
          strokeDasharray={outline ? "4 3" : undefined}
        />
      )
    );
  };

  const pointsOf = (one: PlotSeries, range: "window" | "compare"): ReactNode[] => {
    const muted = range === "compare";
    const classes = muted
      ? { fill: chartLine.comparePoint, stroke: chartLine.compareStroke }
      : tones(one.tone);
    const drawnHere = one.points.filter((point) =>
      range === "window" ? point.held : point.compareHeld
    );
    const lastAt = Math.max(
      ...drawnHere.map(
        (point) => (range === "window" ? point.at : point.compareAt) ?? Number.NEGATIVE_INFINITY
      )
    );
    return drawnHere.flatMap((point) => {
      const value = valueAt(point, "y", range);
      const at = range === "window" ? point.at : point.compareAt;
      const x = placeX(one, point, at);
      if (value === null || x === null) return [];
      // A log axis draws no value at or below 0; the lines under the plot count them.
      if ((yScale.log && value <= 0) || (xScale.log && at !== null && at <= 0)) return [];
      const values = (range === "window" ? point.values : point.compare) ?? {};
      const y = clampY(value);
      const cut =
        view.y.max !== null && value > view.y.max
          ? "above"
          : view.y.min !== null && value < view.y.min
            ? "below"
            : null;
      const low = valueAt(point, "ci_low", range);
      const high = valueAt(point, "ci_high", range);
      const dot =
        plot.marks !== "end" || at === lastAt || range === "compare" || one.role === "summary";
      const median = (plot.strip && point.median) || one.role === "median";
      const bound = range === "window" ? point.bound : point.compareBound;
      const band = plot.kind === "band" && low !== null && high !== null;
      const errorBar =
        (plot.kind === "xy" || one.role === "summary") && low !== null && high !== null;
      const reach = median && plot.strip ? stepWidth * 0.4 : 0;
      return [
        <g data-point={point.x} key={`${range}-${point.x}`}>
          {errorBar || (band && one.role !== "median") ? (
            <>
              {errorBar ? (
                <line
                  className={classes.stroke}
                  opacity={one.role === "run" ? 0.35 : undefined}
                  strokeWidth={1}
                  x1={x}
                  x2={x}
                  y1={clampY(low as number)}
                  y2={clampY(high as number)}
                />
              ) : null}
              {(["ci_low", "ci_high"] as const).map((statistic) => {
                const end = statistic === "ci_low" ? (low as number) : (high as number);
                return (
                  <line
                    className={errorBar ? classes.stroke : "stroke-transparent"}
                    key={statistic}
                    strokeWidth={1}
                    x1={x - 2.5}
                    x2={x + 2.5}
                    y1={clampY(end)}
                    y2={clampY(end)}
                    {...valueNames(model, one, point, statistic, range, end)}
                  />
                );
              })}
            </>
          ) : null}
          {median && plot.strip ? (
            <line
              className={tones(point.overall ? "overall" : one.tone).stroke}
              strokeWidth={3}
              x1={x - reach}
              x2={x + reach}
              y1={y}
              y2={y}
              {...valueNames(model, one, point, "y", range, value)}
            >
              <title>{tip(one, point, values, muted)}</title>
            </line>
          ) : (
            <PointMark
              bound={bound}
              classes={classes}
              hollow={one.hollow || (muted && plot.kind === "xy" && plot.form !== "time")}
              median={median}
              names={valueNames(model, one, point, "y", range, value)}
              shown={dot}
              tint={one.role === "run"}
              title={tip(one, point, values, muted)}
              x={x}
              y={y}
            />
          )}
          {cut === null ? null : (
            <text
              className={chartPlot.text}
              fontSize={9}
              textAnchor="middle"
              x={x}
              y={cut === "above" ? top + 7 : bottom - 2}
            >
              {cut === "above" ? "\u25b2" : "\u25bc"}
            </text>
          )}
        </g>,
      ];
    });
  };

  const annotationMark = (note: Annotation, index: number): ReactNode => {
    const x = xOf(note.at);
    const classes = note.state === null ? null : tones(note.state);
    const text = (at: { x: number; y: number; anchor: "start" | "end" | "middle" }) => (
      <text className={chartPlot.text} fontSize={10} textAnchor={at.anchor} x={at.x} y={at.y}>
        <tspan fontWeight={note.detail === null ? undefined : 600}>{note.label}</tspan>
        {note.detail === null ? null : (
          <tspan dy={12} x={at.x}>
            {note.detail}
          </tspan>
        )}
      </text>
    );
    if (note.edge) {
      return (
        <g data-annotation={index} key={index}>
          {text({ anchor: "end", x: plotRight, y: note.y === null ? top + 10 : clampY(note.y) })}
        </g>
      );
    }
    if (note.end !== null) {
      const end = xOf(note.end);
      return (
        <g data-annotation={index} key={index}>
          <rect
            className={classes?.band ?? chartPlot.span}
            height={bottom - top}
            width={Math.max(1, end - x)}
            x={x}
            y={top}
          />
          {text({ anchor: "start", x: x + 2, y: top + 10 })}
        </g>
      );
    }
    if (note.y === null) {
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
          {text({ anchor: "start", x: x + 3, y: top + 10 })}
        </g>
      );
    }
    const y = clampY(note.y);
    const yEnd = note.yEnd === null ? null : clampY(note.yEnd);
    const stand = yEnd ?? y;
    return (
      <g data-annotation={index} key={index}>
        {yEnd === null ? null : note.bracket ? (
          <path
            className={`fill-none ${classes?.stroke ?? chartPlot.rule}`}
            d={`M${x + 4},${y} H${x} V${yEnd} H${x + 4}`}
          />
        ) : (
          <line
            className={classes?.stroke ?? chartPlot.rule}
            strokeDasharray="1 2"
            x1={x}
            x2={x}
            y1={y}
            y2={yEnd}
          />
        )}
        {note.dot ? (
          <circle
            className={classes === null ? `${chartPlot.text}` : classes.fill}
            cx={x}
            cy={stand}
            r={POINT}
          />
        ) : null}
        {text({
          anchor: "start",
          x: x + 6,
          y: note.bracket && yEnd !== null ? Math.max(y, yEnd) : stand - 4,
        })}
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
    if (reference.type === "band") {
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
    }
    if (reference.type === "diagonal") {
      const low = Math.max(xScale.low, yScale.low);
      const high = Math.min(xScale.high, yScale.high);
      if (!(high > low)) return null;
      return (
        <g data-reference="diagonal">
          <line
            className={chartPlot.rule}
            strokeDasharray="4 3"
            x1={xOf(low)}
            x2={xOf(high)}
            y1={yScale.at(low)}
            y2={yScale.at(high)}
          />
          <text
            className={chartPlot.text}
            fontSize={10}
            textAnchor="end"
            x={xOf(high)}
            y={yScale.at(high) - 3}
          >
            {reference.label}
          </text>
        </g>
      );
    }
    const targets = series.filter((one) => one.fromStart !== null);
    const highest = targets.reduce<PlotSeries | null>(
      (best, one) =>
        best === null || (one.fromStart as number) > (best.fromStart as number) ? one : best,
      null
    );
    const last = plot.steps.length - 1;
    return (
      <g data-reference="from-start">
        {targets.map((one) => {
          const y = clampY(one.fromStart as number);
          const x = xOf(last);
          return (
            <g key={one.name}>
              <line
                className={tones(one.tone).stroke}
                strokeDasharray="3 2"
                x1={x - 10}
                x2={x + 10}
                y1={y}
                y2={y}
              />
              {one === highest ? (
                <text className={chartPlot.text} fontSize={10} x={x + 12} y={y + 3}>
                  {reference.label}
                </text>
              ) : null}
            </g>
          );
        })}
      </g>
    );
  };

  // A strip's thin line for each row group from its dots' median at a step to the next.
  const rowGroupJoins = (): ReactNode => {
    if (!plot.joinRowGroups) return null;
    const byGroup = new Map<string, Map<number, number[]>>();
    for (const one of series) {
      for (const point of one.points) {
        const value = valueAt(point, "y", "window");
        if (
          !point.held ||
          point.median ||
          point.at === null ||
          value === null ||
          point.rowGroup === ""
        )
          continue;
        const steps = byGroup.get(point.rowGroup) ?? new Map<number, number[]>();
        steps.set(point.at, [...(steps.get(point.at) ?? []), value]);
        byGroup.set(point.rowGroup, steps);
      }
    }
    return [...byGroup.entries()].map(([group, steps]) => {
      const places = [...steps.entries()]
        .sort((a, b) => a[0] - b[0])
        .map(([at, values]) => {
          const sorted = [...values].sort((a, b) => a - b);
          const middle = Math.floor(sorted.length / 2);
          const median =
            sorted.length % 2 === 1
              ? (sorted[middle] as number)
              : ((sorted[middle - 1] as number) + (sorted[middle] as number)) / 2;
          return { at, median };
        });
      const pieces: [number, number][][] = [];
      places.forEach((place, index) => {
        const before = places[index - 1];
        if (before !== undefined && place.at === before.at + 1) {
          pieces.push([
            [xOf(before.at), clampY(before.median)],
            [xOf(place.at), clampY(place.median)],
          ]);
        }
      });
      return pieces.map((piece) => (
        <path
          className={`fill-none ${chartPlot.axis}`}
          d={linePath(piece, false)}
          key={`${group}-${piece[0]?.[0]}`}
          strokeWidth={1}
        >
          <title>{group}</title>
        </path>
      ));
    });
  };

  // A median row's range shaded across x (`medians.band`).
  const medianBands = series
    .filter((one) => one.banded)
    .map((one) => <g key={`band-${one.name}`}>{bandOf(one, "window", false)}</g>);

  const directLabels =
    plot.legend !== "direct"
      ? null
      : series
          .filter((one) => one.role !== "summary")
          .map((one) => {
            const last = [...one.points]
              .filter(
                (point) => point.held && point.at !== null && valueAt(point, "y", "window") !== null
              )
              .sort((a, b) => (a.at as number) - (b.at as number))
              .at(-1);
            if (last === undefined) return null;
            const newest = plot.form === "time" ? ` ${last.values.y?.label ?? ""}` : "";
            return (
              <text
                className={chartPlot.text}
                data-direct-label={one.name}
                fontSize={10}
                key={one.name}
                x={plotRight + 6}
                y={clampY(valueAt(last, "y", "window") as number) + 3}
              >
                {`${one.group === "" ? one.label : one.group}${newest}`}
              </text>
            );
          });

  const unplaced = series.flatMap((one) =>
    one.role === "summary" ? [] : one.points.filter((point) => point.held && point.at === null)
  ).length;
  const selectedAt =
    props.selected === null ? null : (plot.items?.places[props.selected]?.at ?? null);
  // A strip's step names its median's count after it: the overall median's, else the first's.
  const stepLabel = (index: number): string => {
    const step = plot.steps[index] ?? "";
    if (!plot.strip) return step;
    const medians = series
      .filter((one) => one.group === step)
      .flatMap((one) => one.points.filter((point) => point.held && point.median));
    const counted = medians.find((point) => point.overall) ?? medians[0];
    const n = counted?.values.n;
    return n === undefined || n.value === null ? step : `${step}, ${n.label}`;
  };

  return (
    <div className="flex min-w-0 flex-col gap-0.5" data-view={props.index}>
      {view.title !== null && plot.layout === "panels" ? (
        <p className={`text-xs font-medium ${textPrimaryOnSurface}`}>{view.title}</p>
      ) : null}
      {drawn ? null : (
        <p className={`text-xs ${textMutedOnSurface}`} data-mixed-units>
          {`These readings are in more than one unit (${view.mixedUnits.join(", ")}), so the chart draws no number.`}
        </p>
      )}
      <svg
        aria-label={`${model.title}${view.title === null ? "" : `, ${view.title}`}`}
        className="block h-auto w-full touch-none select-none"
        ref={svg}
        role="img"
        viewBox={`0 0 ${WIDTH} ${height}`}
      >
        <defs>
          <clipPath id={clipId}>
            <rect height={bottom - top} width={plotRight - left} x={left} y={top} />
          </clipPath>
          <pattern
            height={8}
            id={hatchId}
            patternTransform="rotate(45)"
            patternUnits="userSpaceOnUse"
            width={8}
          >
            <line className={chartPlot.mockup} strokeWidth={2} x1={0} x2={0} y1={0} y2={8} />
          </pattern>
        </defs>
        {/* Gridlines and ticks. */}
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
            {second === null || !secondDraws || noNumbers ? null : (
              <text
                className={chartPlot.text}
                fontSize={10}
                x={plotRight + 4}
                y={yScale.at(tick) + 3}
              >
                {tickLabel(tick * second.scale, false)}
              </text>
            )}
          </g>
        ))}
        {xScale.ticks.map((tick, index) => (
          <g key={`x${tick}`}>
            {plot.form === "category" ? null : (
              <line className={chartPlot.grid} x1={xOf(tick)} x2={xOf(tick)} y1={top} y2={bottom} />
            )}
            {noNumbers && plot.form === "number" ? null : (
              <text
                className={chartPlot.text}
                fontSize={10}
                fontWeight={
                  selectedAt !== null && Math.abs(selectedAt - tick) < DAY / 2 ? 600 : undefined
                }
                textAnchor="middle"
                x={xOf(tick)}
                y={bottom + 13}
              >
                {plot.form === "category"
                  ? stepLabel(index)
                  : tickLabel(tick, plot.form === "time")}
              </text>
            )}
          </g>
        ))}
        <line className={chartPlot.axis} x1={left} x2={plotRight} y1={bottom} y2={bottom} />
        <text
          className={chartPlot.text}
          fontSize={10}
          textAnchor="middle"
          x={(left + plotRight) / 2}
          y={height - 6}
        >
          {xTitle}
        </text>
        <text
          className={chartPlot.text}
          fontSize={10}
          textAnchor="middle"
          transform={`rotate(-90 12 ${(top + bottom) / 2})`}
          x={12}
          y={(top + bottom) / 2}
        >
          {yTitle}
        </text>
        {second === null || !secondDraws ? null : (
          <text
            className={chartPlot.text}
            fontSize={10}
            textAnchor="middle"
            transform={`rotate(90 ${WIDTH - 10} ${(top + bottom) / 2})`}
            x={WIDTH - 10}
            y={(top + bottom) / 2}
          >
            {unitTitle(second.label, second.unit)}
          </text>
        )}
        {plot.missingX && unplaced > 0 ? (
          <text
            className={chartPlot.text}
            fontSize={9}
            textAnchor="middle"
            x={missingAt}
            y={top + 8}
          >
            {`x not recorded (${unplaced})`}
          </text>
        ) : null}
        {selectedAt === null ? null : (
          <line
            className={chartPlot.axis}
            strokeWidth={6}
            opacity={0.3}
            x1={xOf(selectedAt)}
            x2={xOf(selectedAt)}
            y1={top}
            y2={bottom}
          />
        )}
        <g clipPath={`url(#${clipId})`}>
          {referenceMark()}
          {view.annotations.map(annotationMark)}
          {medianBands}
          {drawn
            ? series.map((one) => (
                <g data-series-group={one.name} key={one.name}>
                  {plot.kind === "band" && one.role !== "summary"
                    ? bandOf(one, "window", false)
                    : null}
                  {compare && plot.kind === "band" ? bandOf(one, "compare", true) : null}
                  {compare && (plot.form === "time" || plot.kind === "band")
                    ? groupLines(one, "compare")
                    : null}
                  {groupLines(one, "window")}
                </g>
              ))
            : null}
          {rowGroupJoins()}
        </g>
        {drawn
          ? series.map((one) => (
              <g data-series-points={one.name} key={one.name}>
                {compare ? <g data-series-range="compare">{pointsOf(one, "compare")}</g> : null}
                {pointsOf(one, "window")}
              </g>
            ))
          : null}
        {directLabels}
        {plot.mockup === null ? null : (
          <g data-mockup>
            <rect
              fill={`url(#${hatchId})`}
              height={bottom - top}
              opacity={0.5}
              width={plotRight - left}
              x={left}
              y={top}
            />
            <text
              className={chartPlot.text}
              fontSize={14}
              fontWeight={600}
              opacity={0.6}
              textAnchor="middle"
              x={(left + plotRight) / 2}
              y={(top + bottom) / 2}
            >
              MOCK-UP
            </text>
          </g>
        )}
        {plot.zoom ? (
          <rect
            className="fill-transparent"
            data-zoom-area
            height={bottom - top}
            onPointerDown={press}
            onPointerLeave={release}
            onPointerMove={drag}
            onPointerUp={release}
            width={plotRight - left}
            x={left}
            y={top}
          />
        ) : null}
        {brush === null ? null : (
          <rect
            className={chartPlot.brush}
            height={bottom - top}
            width={Math.abs(brush[1] - brush[0])}
            x={Math.min(...brush)}
            y={top}
          />
        )}
      </svg>
      {second !== null && !secondDraws ? (
        <p className={`text-xs ${textMutedOnSurface}`}>
          {`The second axis \u201c${second.label}\u201d does not draw: its numbers on this chart fall outside 10\u207b\u00b9\u00b2 to 10\u00b9\u2075 in size, the range a second axis draws.`}
        </p>
      ) : null}
    </div>
  );
}

/**
 * Draws an `xy` or a `band` model (`plotModel`): its mock-up line, its option buttons, its key,
 * its views, its zoom's reset, its items and the lines under its plot.
 */
export function PlotRenderer({ compare, model }: ChartRendererProps): ReactNode {
  const [option, setOption] = useState(0);
  const [zoom, setZoom] = useState<[number, number] | null>(null);
  const [selected, setSelected] = useState<number | null>(null);
  if (!isPlotModel(model)) return null;
  const { plot } = model;
  const shown = plot.layout === "options" ? [option] : plot.views.map((_, index) => index);
  const items = plot.items;
  const chosen = selected === null ? null : (items?.places[selected] ?? null);
  // With no reading in the window or the compare range, the chart has nothing to place.
  const empty = model.series.every((series) =>
    series.points.every((point) => !point.held && !(compare && point.compareHeld))
  );
  return (
    <div className="flex min-w-0 flex-col gap-1" data-plot={plot.kind}>
      {plot.mockup === null ? null : (
        <p className={`text-xs font-medium ${textMutedOnSurface}`} data-mockup-line>
          {plot.mockup}
        </p>
      )}
      {plot.layout === "options" ? (
        <fieldset aria-label="Value on y" className="flex flex-wrap gap-1">
          {plot.views.map((view, index) => (
            <button
              aria-pressed={option === index}
              className={`h-6 rounded-md px-2 text-xs font-medium ${
                option === index
                  ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}`
                  : textSecondaryOnCanvas
              }`}
              key={view.title ?? index}
              onClick={() => setOption(index)}
              type="button"
            >
              {view.title ?? `Option ${index + 1}`}
            </button>
          ))}
        </fieldset>
      ) : null}
      {plot.legend === "key" ? (
        <ul
          aria-label="Key"
          className={`flex flex-wrap gap-x-3 gap-y-0.5 text-xs ${textSecondaryOnCanvas}`}
        >
          {plot.key.map((entry) => {
            const classes = toneClasses(entry.tone, plot.notStartedGrey);
            return (
              <li className="flex items-center gap-1" data-key={entry.name} key={entry.name}>
                <span
                  aria-hidden="true"
                  className={`inline-block h-2 w-2 rounded-full ${entry.hollow ? "border border-current" : classes.swatch}`}
                />
                {entry.name}
              </li>
            );
          })}
        </ul>
      ) : null}
      {empty ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-plot-empty>
          No reading of this chart's series in the window.
        </p>
      ) : (
        <div
          className={
            plot.layout === "panels" ? "grid grid-cols-1 gap-2 @min-[44rem]:grid-cols-2" : ""
          }
        >
          {shown.map((index) => {
            const view = plot.views[index];
            return view === undefined ? null : (
              <ViewPlot
                compare={compare}
                index={index}
                key={index}
                model={model}
                onZoom={setZoom}
                selected={selected}
                view={view}
                zoom={zoom}
              />
            );
          })}
        </div>
      )}
      {zoom === null ? null : (
        <button
          className={`${secondaryButtonCompact} self-start`}
          onClick={() => setZoom(null)}
          type="button"
        >
          Reset zoom
        </button>
      )}
      {items === null ? null : (
        <div className="flex flex-col gap-1" data-items>
          <fieldset aria-label="Items by x" className="flex flex-wrap gap-1">
            {items.places.map((place, index) => (
              <button
                aria-pressed={selected === index}
                className={secondaryButtonCompact}
                key={place.x}
                onClick={() => setSelected(selected === index ? null : index)}
                type="button"
              >
                {`${place.label} (${place.items.length})`}
              </button>
            ))}
          </fieldset>
          {chosen === null ? null : (
            <ul
              aria-label={`Items at ${chosen.label}`}
              className="flex flex-col gap-1 text-xs"
              data-outlines={items.outlines ?? undefined}
            >
              {chosen.items.map((item) => (
                <li className="flex flex-col" key={item.title}>
                  {item.group === "" && item.note === "" ? null : (
                    <span className={textMutedOnSurface}>
                      {item.group === "" ? null : (
                        <span
                          aria-hidden="true"
                          className={`mr-1 inline-block h-2 w-2 rounded-full ${toneClasses(item.tone, plot.notStartedGrey).swatch}`}
                        />
                      )}
                      {[item.group, item.note].filter((part) => part !== "").join(" \u00b7 ")}
                    </span>
                  )}
                  <span className={textPrimaryOnSurface}>{item.title}</span>
                  {item.parts.length === 0 ? null : (
                    <span
                      className={textMutedOnSurface}
                    >{`${items.partsLabel}: ${item.parts.join(", ")}`}</span>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {plot.notes.length === 0 ? null : (
        <div className={`flex flex-col text-[11px] ${textMutedOnSurface}`} data-plot-notes>
          {plot.notes.map((note) => (
            <p key={note}>{note}</p>
          ))}
        </div>
      )}
    </div>
  );
}
