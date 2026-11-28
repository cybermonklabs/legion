// The renderer of an `architecture` (dispatch://INFRA-2918/artifact/plan-md, D10): the model's
// parts as a grid of boxes, column by column under the columns' titles, each shaded by its value's
// bin (the higher the darker; a part of no value a dashed white box), with its label, its value
// and, where a fault through it has been shown, a mark before the value; each of the heaviest with
// its number in a badge; each place a frame around the parts it holds, dashed where no edge
// reaches it; the rows above the first place and below the last as titled bands; and each edge as
// an arrow (`flow`), a dashed route (`carried`) or a line ending in a dot at a kernel (`kernel`).
// Under it, the heaviest parts with their notes, and the sources of its values with their
// readings' links. While the compare range is shown, each value prints its compare value and its
// change beside it. Each value it draws names itself as the chart's table names it (`data-chart`,
// `data-series`, `data-statistic`, `data-x`, `data-range`, `data-value`), so the table holds every
// number the graph draws; the second reading draws nothing here, and is the table's.
import { type ReactNode, useId } from "react";

import { LinkList } from "../../../components/LinkList";
import {
  chartGroup,
  chartPlot,
  chartState,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../../theme/classes";
import type { ChartPoint, ChartRendererProps, ChartValue } from "../chart-model";
import {
  type ArchitectureBand,
  type ArchitectureModel,
  type ArchitecturePart,
  isArchitectureModel,
} from "./architecture-model";

const COLUMN = 176;
const ROW = 64;
/** The room between a cell's edge and its box. */
const INSET = 6;
const BOX = { height: ROW - 2 * INSET, width: COLUMN - 2 * INSET } as const;
/** The strip of the columns' titles, and of a band's title. */
const TITLE = 18;
/** The most characters of a label drawn in a box or a title; its tip holds the whole. */
const LABEL_CHARS = 27;
/** The shade of the darkest bin: a box's text stays legible on it. */
const DARKEST = 0.5;

/** `text`, cut to `chars` characters with an ellipsis where it is longer. */
function cut(text: string, chars: number): string {
  return text.length > chars ? `${text.slice(0, chars - 1)}\u2026` : text;
}

/** A bin's shade: its share of the darkest, the lightest bin a fifth of it where there are
 *  five. */
function shadeOf(bin: number, bins: number): number {
  return Math.round(((bin + 1) / Math.max(bins, 1)) * DARKEST * 100) / 100;
}

/** Where each row stands: under the columns' titles, and under the title strip of the band it is
 *  the first row of. */
function rowTops(model: ArchitectureModel): (row: number) => number {
  const { above, below, columns } = model.architecture;
  const top = columns.some((title) => title !== "") ? TITLE : 0;
  const aboveTitle = above !== null && above.title !== "" ? TITLE : 0;
  return (row) =>
    top +
    aboveTitle +
    row * ROW +
    (below !== null && below.title !== "" && row >= below.first ? TITLE : 0);
}

/** One value the graph draws, named as the chart's table names it. */
function Value(props: {
  model: ArchitectureModel;
  point: ChartPoint;
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
      data-statistic="value"
      data-value={value.value === null ? undefined : String(value.value)}
      data-x={props.point.x}
    >
      {`${props.prefix ?? ""}${value.label}`}
    </tspan>
  );
}

/** A band of rows: its shade across the diagram, and its title in the strip over it. */
function BandMark(props: {
  band: ArchitectureBand;
  rowTop: (row: number) => number;
  width: number;
  name: string;
}): ReactNode {
  const { band, rowTop } = props;
  const titled = band.title !== "";
  const top = rowTop(band.first) - (titled ? TITLE : 0);
  return (
    <g data-architecture-band={props.name}>
      <rect
        className={chartPlot.band}
        height={rowTop(band.last) + ROW - top}
        width={props.width}
        x={0}
        y={top}
      />
      {titled ? (
        <text className={`${chartPlot.text} text-[11px] font-medium`} x={INSET} y={top + 13}>
          {band.title}
        </text>
      ) : null}
    </g>
  );
}

/** A part's box: shaded by its bin, its label, its fault mark and value, its compare value and
 *  change while the compare range is shown, and its badge. */
function PartBox(props: {
  model: ArchitectureModel;
  part: ArchitecturePart;
  point: ChartPoint | undefined;
  rowTop: (row: number) => number;
  compare: boolean;
}): ReactNode {
  const { compare, model, part, point } = props;
  const x = part.column * COLUMN + INSET;
  const y = props.rowTop(part.row) + INSET;
  const value = point?.values.value;
  const note = value?.note ?? null;
  return (
    <g data-architecture-part={part.id} data-bin={part.bin ?? undefined}>
      <title>
        {[
          part.label,
          value === undefined ? "" : `${value.label}${note === null ? "" : `: ${note}`}`,
        ]
          .filter((line) => line !== "")
          .join("\n")}
      </title>
      <rect
        className={`${part.bin === null ? chartPlot.hollow : chartGroup[0].fill} ${chartPlot.axis}`}
        fillOpacity={
          part.bin === null ? undefined : shadeOf(part.bin, model.architecture.bins.length)
        }
        height={BOX.height}
        rx={part.kind === "kernel" ? 12 : 3}
        strokeDasharray={part.bin === null ? "4 3" : undefined}
        strokeWidth={1}
        width={BOX.width}
        x={x}
        y={y}
      />
      <text className={`${chartPlot.text} text-[11px]`} x={x + 8} y={y + 18}>
        {cut(part.label, LABEL_CHARS)}
      </text>
      {part.fault ? (
        <g data-fault>
          <path
            className={chartState.attention.fill}
            d={`M${x + 14},${y + 27} L${x + 21},${y + 39} L${x + 7},${y + 39} Z`}
          />
          <text
            className={`${chartPlot.hollow} text-[9px] font-bold`}
            textAnchor="middle"
            x={x + 14}
            y={y + 38}
          >
            !
          </text>
        </g>
      ) : null}
      {point === undefined ? null : (
        <text
          className={`${chartPlot.text} text-[11px] font-semibold`}
          x={x + (part.fault ? 26 : 8)}
          y={y + 38}
        >
          {part.bin === null ? null : (
            <Value model={model} point={point} range="window" value={value} />
          )}
          {compare && point.compare !== null ? (
            <Value
              model={model}
              point={point}
              prefix=" was "
              range="compare"
              value={point.compare.value}
            />
          ) : null}
          {compare && point.change !== null ? (
            <Value
              model={model}
              point={point}
              prefix={" \u00b7 "}
              range="change"
              value={point.change.value}
            />
          ) : null}
        </text>
      )}
      {part.badge === null ? null : (
        <g data-badge={part.badge}>
          <circle className={chartState.overall.fill} cx={x + BOX.width - 9} cy={y + 9} r={8} />
          <text
            className={`${chartPlot.hollow} text-[10px] font-bold`}
            textAnchor="middle"
            x={x + BOX.width - 9}
            y={y + 12.5}
          >
            {part.badge}
          </text>
        </g>
      )}
    </g>
  );
}

/** Where an edge leaves `from`'s box toward `to`'s centre. */
function boxEdge(
  from: { readonly x: number; readonly y: number },
  to: { readonly x: number; readonly y: number }
): { readonly x: number; readonly y: number } {
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const scale = Math.min(
    dx === 0 ? Number.POSITIVE_INFINITY : BOX.width / 2 / Math.abs(dx),
    dy === 0 ? Number.POSITIVE_INFINITY : BOX.height / 2 / Math.abs(dy),
    1
  );
  return { x: from.x + dx * scale, y: from.y + dy * scale };
}

/** The diagram's key: each bin's shade, a part of no value, the fault mark and the kernel's line
 *  where they draw, and the unit of the values. */
function Key({ model }: { model: ArchitectureModel }): ReactNode {
  const { bins, fault, kernel } = model.architecture;
  const unit = model.series[0]?.unit ?? "";
  return (
    <ul
      aria-label="Key"
      className={`flex flex-wrap gap-x-3 gap-y-0.5 text-xs ${textSecondaryOnCanvas}`}
    >
      {bins.map((label, bin) => (
        <li className="flex items-center gap-1" data-key-bin={bin} key={label}>
          <span
            aria-hidden="true"
            className={`inline-block h-2.5 w-3.5 rounded-sm ${chartGroup[0].swatch}`}
            style={{ opacity: shadeOf(bin, bins.length) }}
          />
          {label}
        </li>
      ))}
      <li className="flex items-center gap-1">
        <span
          aria-hidden="true"
          className="inline-block h-2.5 w-3.5 rounded-sm border border-dashed border-current"
        />
        no value
      </li>
      {fault === null ? null : (
        <li className="flex items-center gap-1" data-key-fault>
          <svg aria-hidden="true" className="h-2.5 w-3" viewBox="0 0 12 10">
            <path className={chartState.attention.fill} d="M6,0 L12,10 L0,10 Z" />
          </svg>
          {fault}
        </li>
      )}
      {kernel === null ? null : (
        <li className="flex items-center gap-1" data-key-kernel>
          <svg aria-hidden="true" className="h-2.5 w-3" viewBox="0 0 12 10">
            <circle className={chartPlot.text} cx={6} cy={5} r={3} />
          </svg>
          {kernel}
        </li>
      )}
      {unit === "" ? null : <li>{`Values in ${unit}`}</li>}
    </ul>
  );
}

/** Draws an `architecture` model (plan D10); any other model draws nothing. */
export function ArchitectureRenderer({ compare, model }: ChartRendererProps): ReactNode {
  const flowId = useId();
  const carriedId = useId();
  if (!isArchitectureModel(model)) return null;
  const { architecture } = model;
  const points = new Map((model.series[0]?.points ?? []).map((point) => [point.x, point]));
  const byId = new Map(architecture.parts.map((part) => [part.id, part]));
  const rowTop = rowTops(model);
  const width = Math.max(architecture.columns.length, 1) * COLUMN;
  const height = rowTop(Math.max(architecture.rows, 1));
  const centreOf = (part: ArchitecturePart) => ({
    x: part.column * COLUMN + COLUMN / 2,
    y: rowTop(part.row) + ROW / 2,
  });
  const heaviest = architecture.heaviest.flatMap((heavy) => {
    const point = points.get(heavy.id);
    return point === undefined ? [] : [{ ...heavy, point }];
  });
  return (
    <div className="flex min-w-0 flex-col gap-1" data-architecture>
      <Key model={model} />
      {architecture.parts.length === 0 ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-architecture-empty>
          No part of this diagram has a reading in the window.
        </p>
      ) : (
        <svg
          aria-label={model.title}
          className="block h-auto w-full"
          role="img"
          viewBox={`0 0 ${width} ${height}`}
        >
          <defs>
            {[
              [flowId, chartPlot.text],
              [carriedId, chartState.attention.fill],
            ].map(([id, paint]) => (
              <marker
                id={id}
                key={id}
                markerHeight={7}
                markerWidth={7}
                orient="auto"
                refX={8}
                refY={4}
                viewBox="0 0 8 8"
              >
                <path className={paint} d="M0,0 L8,4 L0,8 Z" />
              </marker>
            ))}
          </defs>
          {architecture.above === null ? null : (
            <BandMark band={architecture.above} name="above" rowTop={rowTop} width={width} />
          )}
          {architecture.below === null ? null : (
            <BandMark band={architecture.below} name="below" rowTop={rowTop} width={width} />
          )}
          {architecture.columns.map((title, column) =>
            title === "" ? null : (
              <text
                className={`${chartPlot.text} text-[11px] font-medium`}
                data-architecture-column={column}
                // biome-ignore lint/suspicious/noArrayIndexKey: a column is its place; the titles never move.
                key={column}
                textAnchor="middle"
                x={column * COLUMN + COLUMN / 2}
                y={13}
              >
                <title>{title}</title>
                {cut(title, LABEL_CHARS)}
              </text>
            )
          )}
          {architecture.parts.map((part) =>
            part.kind !== "place" ? null : (
              <g data-architecture-place={part.id} key={part.id}>
                <rect
                  className={`fill-none ${chartPlot.axis}`}
                  height={rowTop(part.span.lastRow) + ROW - rowTop(part.span.row) - 4}
                  rx={6}
                  strokeDasharray={part.reached ? undefined : "5 4"}
                  strokeWidth={1.5}
                  width={(part.span.lastColumn - part.span.column + 1) * COLUMN - 4}
                  x={part.span.column * COLUMN + 2}
                  y={rowTop(part.span.row) + 2}
                />
                <text
                  className={`${chartPlot.text} text-[11px] font-medium`}
                  x={part.column * COLUMN + 10}
                  y={rowTop(part.row) + 18}
                >
                  <title>{part.label}</title>
                  {cut(part.label, LABEL_CHARS)}
                </text>
              </g>
            )
          )}
          {architecture.edges.map((edge) => {
            const from = byId.get(edge.from);
            const to = byId.get(edge.to);
            if (from === undefined || to === undefined || from === to) return null;
            const start = boxEdge(centreOf(from), centreOf(to));
            const end = boxEdge(centreOf(to), centreOf(from));
            const kind = edge.kind === "carried" || edge.kind === "kernel" ? edge.kind : "flow";
            return (
              <g data-architecture-edge={kind} key={`${edge.from}\u2192${edge.to}`}>
                <title>{`${from.label} \u2192 ${to.label}${edge.label === "" ? "" : `: ${edge.label}`}`}</title>
                <line
                  className={kind === "carried" ? chartState.attention.stroke : chartPlot.rule}
                  markerEnd={
                    kind === "kernel"
                      ? undefined
                      : `url(#${kind === "carried" ? carriedId : flowId})`
                  }
                  strokeDasharray={kind === "carried" ? "5 3" : undefined}
                  strokeWidth={kind === "kernel" ? 1 : 1.25}
                  x1={start.x}
                  x2={end.x}
                  y1={start.y}
                  y2={end.y}
                />
                {kind === "kernel" ? (
                  <circle className={chartPlot.text} cx={end.x} cy={end.y} r={3} />
                ) : null}
                {edge.label === "" ? null : (
                  <text
                    className={`${chartPlot.text} text-[10px]`}
                    textAnchor="middle"
                    x={(start.x + end.x) / 2}
                    y={(start.y + end.y) / 2 - 4}
                  >
                    {cut(edge.label, LABEL_CHARS)}
                  </text>
                )}
              </g>
            );
          })}
          {architecture.parts.map((part) =>
            part.kind === "place" ? null : (
              <PartBox
                compare={compare}
                key={part.id}
                model={model}
                part={part}
                point={points.get(part.id)}
                rowTop={rowTop}
              />
            )
          )}
        </svg>
      )}
      {heaviest.length === 0 ? null : (
        <div className={`flex flex-col gap-0.5 text-xs ${textPrimaryOnSurface}`} data-heaviest>
          <p className="font-medium">The heaviest parts</p>
          <ol className="flex flex-col gap-0.5">
            {heaviest.map(({ badge, id, name, point }) => {
              const value = point.values.value;
              return (
                <li data-heavy={id} key={id}>
                  {`${badge}. ${name}: `}
                  <span
                    data-chart={model.name}
                    data-range="window"
                    data-series={model.series[0]?.name}
                    data-statistic="value"
                    data-value={
                      value === undefined || value.value === null ? undefined : String(value.value)
                    }
                    data-x={id}
                  >
                    {value?.label}
                  </span>
                  {value?.note ? (
                    <span className={textMutedOnSurface}>{` \u2014 ${value.note}`}</span>
                  ) : null}
                </li>
              );
            })}
          </ol>
        </div>
      )}
      {architecture.sources.length === 0 ? null : (
        <p className={`text-[11px] ${textMutedOnSurface}`} data-architecture-sources>
          {architecture.sources.length === 1
            ? `Values from ${architecture.sources[0]}.`
            : `Values from ${architecture.sources.length} sources: ${architecture.sources.join(", ")}.`}
        </p>
      )}
      {architecture.links.length === 0 ? null : (
        <LinkList label={`${model.title}: links of its values`} links={architecture.links} />
      )}
      {compare && architecture.compareOnly.length > 0 ? (
        <p className={`text-[11px] ${textMutedOnSurface}`} data-architecture-compare-only>
          {`In the compare range only: ${architecture.compareOnly.join(", ")}.`}
        </p>
      ) : null}
      {architecture.notes.length === 0 ? null : (
        <div className={`flex flex-col text-[11px] ${textMutedOnSurface}`} data-architecture-notes>
          {architecture.notes.map((note) => (
            <p key={note}>{note}</p>
          ))}
        </div>
      )}
    </div>
  );
}
