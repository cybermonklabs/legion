// The renderer of a `network` (dispatch://INFRA-2918/artifact/plan-md, D10): each node of the
// model as a circle at its column and row, as large as its size and filled with the colour of its
// value of the attribute the reader colours by (the spec's `color`, or one of its `selector`, under
// the "Colour by" buttons), ringed in near black where its outline attribute says yes, with its
// name, size and colour value under it; and each edge as an arrow from node to node. While the
// compare range is shown, each node the compare range holds draws a dashed ring as large as its
// size there and in its colour there, with the change of its size and colour under its values.
// Each value it draws names itself as the chart's table names it (`data-chart`, `data-series`,
// `data-statistic`, `data-x`, `data-range`, `data-value`), so the table holds every number the
// graph draws.
import { type ReactNode, useId, useState } from "react";

import {
  chartPlot,
  chartState,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../../theme/classes";
import type { ChartPoint, ChartRendererProps, ChartValue } from "../chart-model";
import { toneClasses } from "./kind";
import {
  type Colouring,
  isNetworkModel,
  type NetworkModel,
  type NetworkNode,
  NODE_RADIUS,
} from "./network-model";

const COLUMN = 168;
const ROW = 96;
const PAD = 16;
/** The most characters of a node's name drawn under it; its tip holds the whole. */
const NAME_CHARS = 24;

/** `text`, cut to `chars` characters with an ellipsis where it is longer. */
function cut(text: string, chars: number): string {
  return text.length > chars ? `${text.slice(0, chars - 1)}\u2026` : text;
}

/** A node's centre. */
function centreOf(node: NetworkNode): { readonly x: number; readonly y: number } {
  return {
    x: PAD + node.column * COLUMN + COLUMN / 2,
    y: PAD + node.row * ROW + NODE_RADIUS.most,
  };
}

/** One value the graph draws, named as the chart's table names it. */
function Value(props: {
  model: NetworkModel;
  point: ChartPoint;
  statistic: string;
  range: "window" | "compare" | "change";
  value: ChartValue | undefined;
  prefix?: string;
}): ReactNode {
  const { value } = props;
  if (value === undefined) return null;
  return (
    <tspan
      data-chart={props.model.name}
      data-range={props.range}
      data-series={props.model.series[0]?.name}
      data-statistic={props.statistic}
      data-value={value.value === null ? undefined : String(value.value)}
      data-x={props.point.x}
    >
      {`${props.prefix ?? ""}${value.label}`}
    </tspan>
  );
}

/** What a node's tip says: its name and each of its values, and the compare range's. */
function tipOf(model: NetworkModel, point: ChartPoint, compare: boolean): string {
  const statistics = model.series[0]?.statistics ?? [];
  const lines = statistics.map(({ id, label }) => {
    const own = point.values[id]?.label ?? "";
    const then = compare ? point.compare?.[id]?.label : undefined;
    return then === undefined ? `${label}: ${own}` : `${label}: ${own} (compare ${then})`;
  });
  return [point.label, ...lines].join("\n");
}

/** One node: its compare ring under it while the compare range is shown, its circle, and its
 *  name and values under it. */
function NodeMark(props: {
  model: NetworkModel;
  node: NetworkNode;
  point: ChartPoint;
  colouring: Colouring | undefined;
  compare: boolean;
}): ReactNode {
  const { colouring, compare, model, node, point } = props;
  const { x, y } = centreOf(node);
  const size = model.network.size.attribute;
  const colour = colouring?.attribute;
  const toneOf = (categories: Readonly<Record<string, string>> | null) =>
    colouring?.categories.find((category) => category.key === categories?.[colouring.attribute])
      ?.tone ?? "other";
  const ring = compare ? node.compareRadius : null;
  const then = point.compare?.[size];
  return (
    <g data-network-node={node.id}>
      <title>{tipOf(model, point, compare)}</title>
      <circle
        className={`${toneClasses(toneOf(node.categories), false).fill} ${
          node.outlined ? chartState.overall.stroke : chartPlot.axis
        }`}
        cx={x}
        cy={y}
        r={node.radius}
        strokeDasharray={node.sized ? undefined : "3 2"}
        strokeWidth={node.outlined ? 2.5 : 1}
      />
      {ring === null ? null : (
        <circle
          className={`fill-none ${toneClasses(toneOf(node.compareCategories), false).stroke}`}
          cx={x}
          cy={y}
          data-chart={model.name}
          data-network-ring
          data-range="compare"
          data-series={model.series[0]?.name}
          data-statistic={size}
          data-value={then === undefined || then.value === null ? undefined : String(then.value)}
          data-x={point.x}
          r={ring}
          strokeDasharray="4 3"
          strokeWidth={1.5}
        />
      )}
      <text
        className={`${chartPlot.text} text-[11px]`}
        textAnchor="middle"
        x={x}
        y={y + NODE_RADIUS.most + 14}
      >
        {cut(point.label, NAME_CHARS)}
      </text>
      <text
        className={`${chartPlot.text} text-[10px]`}
        textAnchor="middle"
        x={x}
        y={y + NODE_RADIUS.most + 27}
      >
        <Value
          model={model}
          point={point}
          range="window"
          statistic={size}
          value={point.values[size]}
        />
        {colour === undefined || colour === size ? null : (
          <Value
            model={model}
            point={point}
            prefix={" \u00b7 "}
            range="window"
            statistic={colour}
            value={point.values[colour]}
          />
        )}
      </text>
      {compare && point.change !== null ? (
        <text
          className={`${chartPlot.text} text-[10px]`}
          data-network-change
          textAnchor="middle"
          x={x}
          y={y + NODE_RADIUS.most + 40}
        >
          <Value
            model={model}
            point={point}
            range="change"
            statistic={size}
            value={point.change[size]}
          />
          {colour === undefined || colour === size ? null : (
            <Value
              model={model}
              point={point}
              prefix={" \u00b7 "}
              range="change"
              statistic={colour}
              value={point.change[colour]}
            />
          )}
        </text>
      ) : null}
    </g>
  );
}

/** The graph's key: the colouring's values and their colours, and what size, outline and the
 *  compare ring mean. */
function Key(props: {
  model: NetworkModel;
  colouring: Colouring | undefined;
  compare: boolean;
}): ReactNode {
  const { colouring, compare, model } = props;
  const { outline, size } = model.network;
  return (
    <ul
      aria-label="Key"
      className={`flex flex-wrap gap-x-3 gap-y-0.5 text-xs ${textSecondaryOnCanvas}`}
    >
      {(colouring?.categories ?? []).map((category) => (
        <li className="flex items-center gap-1" data-key={category.label} key={category.key}>
          <span
            aria-hidden="true"
            className={`inline-block h-2 w-2 rounded-full ${toneClasses(category.tone, false).swatch}`}
          />
          {category.label}
        </li>
      ))}
      <li data-key-size>{`Size: ${size.label}`}</li>
      {outline === null ? null : <li data-key-outline>{`Ringed: ${outline.label}`}</li>}
      {compare ? (
        <li data-key-compare>Dashed ring: the size and colour in the compare range</li>
      ) : null}
    </ul>
  );
}

/** Draws a `network` model (plan D10); any other model draws nothing. */
export function NetworkRenderer({ compare, model }: ChartRendererProps): ReactNode {
  const arrowId = useId();
  const [colourBy, setColourBy] = useState(0);
  if (!isNetworkModel(model)) return null;
  const { network } = model;
  const colouring = network.colourings[colourBy] ?? network.colourings[0];
  const points = new Map((model.series[0]?.points ?? []).map((point) => [point.x, point]));
  const byId = new Map(network.nodes.map((node) => [node.id, node]));
  const width = PAD * 2 + Math.max(network.columns, 1) * COLUMN;
  const height = PAD * 2 + Math.max(network.rows, 1) * ROW;
  return (
    <div className="flex min-w-0 flex-col gap-1" data-network>
      {network.colourings.length > 1 ? (
        <fieldset aria-label="Colour by" className="flex flex-wrap gap-1">
          {network.colourings.map((one, index) => (
            <button
              aria-pressed={one === colouring}
              className={`h-6 rounded-md px-2 text-xs font-medium ${
                one === colouring
                  ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}`
                  : textSecondaryOnCanvas
              }`}
              key={one.attribute}
              onClick={() => setColourBy(index)}
              type="button"
            >
              {one.label}
            </button>
          ))}
        </fieldset>
      ) : null}
      <Key colouring={colouring} compare={compare} model={model} />
      {network.nodes.length === 0 ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-network-empty>
          No node of this network has a reading in the window.
        </p>
      ) : (
        <svg
          aria-label={model.title}
          className="block h-auto w-full"
          role="img"
          viewBox={`0 0 ${width} ${height}`}
        >
          <defs>
            <marker
              id={arrowId}
              markerHeight={7}
              markerWidth={7}
              orient="auto"
              refX={8}
              refY={4}
              viewBox="0 0 8 8"
            >
              <path className={chartPlot.text} d="M0,0 L8,4 L0,8 Z" />
            </marker>
          </defs>
          {network.edges.map((edge) => {
            const from = byId.get(edge.from);
            const to = byId.get(edge.to);
            if (from === undefined || to === undefined || from === to) return null;
            const start = centreOf(from);
            const end = centreOf(to);
            const length = Math.hypot(end.x - start.x, end.y - start.y) || 1;
            const ux = (end.x - start.x) / length;
            const uy = (end.y - start.y) / length;
            return (
              <line
                className={chartPlot.rule}
                data-network-edge={`${edge.from}\u2192${edge.to}`}
                key={`${edge.from}\u2192${edge.to}`}
                markerEnd={`url(#${arrowId})`}
                strokeWidth={1}
                x1={start.x + ux * from.radius}
                x2={end.x - ux * (to.radius + 2)}
                y1={start.y + uy * from.radius}
                y2={end.y - uy * (to.radius + 2)}
              >
                <title>{`${from.label} \u2192 ${to.label}${edge.label === "" ? "" : `: ${edge.label}`}`}</title>
              </line>
            );
          })}
          {network.nodes.map((node) => {
            const point = points.get(node.id);
            return point === undefined ? null : (
              <NodeMark
                colouring={colouring}
                compare={compare}
                key={node.id}
                model={model}
                node={node}
                point={point}
              />
            );
          })}
        </svg>
      )}
      {compare && network.compareOnly.length > 0 ? (
        <p className={`text-[11px] ${textMutedOnSurface}`} data-network-compare-only>
          {`In the compare range only: ${network.compareOnly.join(", ")}.`}
        </p>
      ) : null}
      {network.notes.length === 0 ? null : (
        <div className={`flex flex-col text-[11px] ${textMutedOnSurface}`} data-network-notes>
          {network.notes.map((note) => (
            <p key={note}>{note}</p>
          ))}
        </div>
      )}
    </div>
  );
}
