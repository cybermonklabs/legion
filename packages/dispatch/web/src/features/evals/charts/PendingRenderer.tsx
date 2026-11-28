// The renderer of a `pending` model (pending.ts): its owner's mock-up, a line for each series
// through its points or a bar for each point grouped by category, on axes that carry their titles
// and no tick value, under a hatched MOCK-UP, with a key naming each series; or the placeholder
// that names the chart. The builder's link stands in `data-builder` alone. Each drawn point names
// itself as the chart's table names it (`data-chart`, `data-series`, `data-statistic`, `data-x`,
// `data-range`, `data-value`). It draws no compare: a pending chart takes none.
import { type ReactNode, useId } from "react";

import { chartPlot, textMutedOnSurface, textSecondaryOnCanvas } from "../../../theme/classes";
import type { ChartModel, ChartRendererProps } from "../chart-model";
import { toneClasses } from "./kind";
import type { PendingModel, PendingSeries, Sketch } from "./pending";

const WIDTH = 640;
const HEIGHT = 240;
const MARGIN = { bottom: 36, left: 36, right: 12, top: 12 };
const POINT = 3.5;

function isPendingModel(model: ChartModel): model is PendingModel {
  return "pending" in model;
}

/** A linear map of [low, high] onto [from, to], a little inside both ends; a span of one value
 *  stands in the middle. */
function linear(low: number, high: number, from: number, to: number) {
  if (low === high) return () => (from + to) / 2;
  const pad = (high - low) * 0.06;
  return (value: number) => from + ((value - (low - pad)) / (high - low + 2 * pad)) * (to - from);
}

/** The points' marks: each series a line through its points in the order of x and a dot at each,
 *  or under each category a bar for each series that has a point there. */
function Marks({
  model,
  series,
  sketch,
}: {
  model: PendingModel;
  series: readonly PendingSeries[];
  sketch: Sketch;
}): ReactNode {
  const left = MARGIN.left;
  const right = WIDTH - MARGIN.right;
  const top = MARGIN.top;
  const bottom = HEIGHT - MARGIN.bottom;
  const ys = series.flatMap((one) =>
    one.points.flatMap((point) => {
      const value = point.values.y?.value ?? null;
      return value === null ? [] : [value];
    })
  );
  const names = (one: PendingSeries, x: string, value: number) => ({
    "data-chart": model.name,
    "data-range": "window",
    "data-series": one.name,
    "data-statistic": "y",
    "data-value": String(value),
    "data-x": x,
  });
  if (sketch.shape === "bar") {
    // Bars stand on 0, so 0 is on the axis whatever the points' signs.
    const yOf = linear(Math.min(0, ...ys), Math.max(0, ...ys), bottom, top);
    const slot = (right - left) / Math.max(1, sketch.places.length);
    const width = (slot * 0.8) / Math.max(1, series.length);
    return (
      <g>
        {series.map((one, index) => (
          <g data-series-marks={one.name} key={one.name}>
            {one.points.map((point, place) => {
              const value = point.values.y?.value ?? null;
              if (value === null) return null;
              const x = left + slot * place + slot * 0.1 + width * index;
              const [from, to] = [yOf(0), yOf(value)];
              return (
                <rect
                  className={toneClasses(one.tone, false).fill}
                  height={Math.max(1, Math.abs(to - from))}
                  key={point.x}
                  width={Math.max(1, width - 1)}
                  x={x}
                  y={Math.min(from, to)}
                  {...names(one, point.x, value)}
                />
              );
            })}
          </g>
        ))}
      </g>
    );
  }
  const ats = sketch.places.map((place) => place.at);
  const xOf = linear(Math.min(...ats), Math.max(...ats), left, right);
  const yOf = linear(Math.min(...ys), Math.max(...ys), bottom, top);
  return (
    <g>
      {series.map((one) => {
        const held = one.points.flatMap((point, place) => {
          const value = point.values.y?.value ?? null;
          const at = sketch.places[place]?.at;
          return value === null || at === undefined ? [] : [{ at, point, value }];
        });
        const classes = toneClasses(one.tone, false);
        return (
          <g data-series-marks={one.name} key={one.name}>
            <path
              className={`fill-none ${classes.stroke}`}
              d={held
                .map(({ at, value }, index) => `${index === 0 ? "M" : "L"}${xOf(at)},${yOf(value)}`)
                .join(" ")}
              strokeWidth={2}
            />
            {held.map(({ at, point, value }) => (
              <circle
                className={`${classes.fill} ${classes.stroke}`}
                cx={xOf(at)}
                cy={yOf(value)}
                key={point.x}
                r={POINT}
                {...names(one, point.x, value)}
              />
            ))}
          </g>
        );
      })}
    </g>
  );
}

export function PendingRenderer({ model }: ChartRendererProps): ReactNode {
  const hatchId = useId();
  if (!isPendingModel(model)) return null;
  const { builder, placeholder, sketch } = model.pending;
  if (sketch === null) {
    return (
      <p
        className={`text-xs ${textMutedOnSurface}`}
        data-builder={builder ?? undefined}
        data-pending-placeholder
      >
        {placeholder}
      </p>
    );
  }
  const left = MARGIN.left;
  const right = WIDTH - MARGIN.right;
  const top = MARGIN.top;
  const bottom = HEIGHT - MARGIN.bottom;
  return (
    <div
      className="flex min-w-0 flex-col gap-1"
      data-builder={builder ?? undefined}
      data-plot="pending"
    >
      <p className={`text-xs font-medium ${textMutedOnSurface}`} data-mockup-line>
        {sketch.line}
      </p>
      <svg
        aria-label={`${model.title}, mock-up`}
        className="block h-auto w-full select-none"
        role="img"
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
      >
        <defs>
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
        <line className={chartPlot.axis} x1={left} x2={right} y1={bottom} y2={bottom} />
        <line className={chartPlot.axis} x1={left} x2={left} y1={top} y2={bottom} />
        <text
          className={chartPlot.text}
          data-axis="x"
          fontSize={11}
          textAnchor="middle"
          x={(left + right) / 2}
          y={HEIGHT - 10}
        >
          {sketch.xTitle}
        </text>
        <text
          className={chartPlot.text}
          data-axis="y"
          fontSize={11}
          textAnchor="middle"
          transform={`rotate(-90 ${left - 20} ${(top + bottom) / 2})`}
          x={left - 20}
          y={(top + bottom) / 2}
        >
          {sketch.yTitle}
        </text>
        <Marks model={model} series={model.series} sketch={sketch} />
        <g data-mockup>
          <rect
            fill={`url(#${hatchId})`}
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
      </svg>
      <ul
        aria-label="Key"
        className={`flex flex-wrap gap-x-3 gap-y-0.5 text-xs ${textSecondaryOnCanvas}`}
      >
        {model.series.map((one) => (
          <li className="flex items-center gap-1" data-key={one.name} key={one.name}>
            <span
              aria-hidden="true"
              className={`inline-block h-2 w-2 rounded-full ${toneClasses(one.tone, false).swatch}`}
            />
            {one.label}
          </li>
        ))}
      </ul>
    </div>
  );
}
