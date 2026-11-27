// The renderer of a `grid` (dispatch://INFRA-2918/artifact/plan-md, D10): the readings matrix as
// a table of cells, each printing what its reading supplies on its own lines, shaded by its value
// or its share of its total or coloured by its state, under a key of the shade or the states; a
// grid of more than 1,000 cells as an SVG of 18 px squares with each cell in its hover text, and
// one of more than 10,000 as a line that says so. While the compare range is shown, each cell the
// compare range holds prints its compare value and change as a muted ghost under its own (on a
// square, a muted dot in its corner). Each number it prints names itself as the chart's table
// names it (`data-chart`, `data-series`, `data-statistic`, `data-x`, `data-range`, `data-value`),
// so the table holds every number the graph draws.
import type { ReactNode } from "react";

import {
  borderDefault,
  chartGroup,
  chartLine,
  chartPlot,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../../theme/classes";
import type { ChartModel, ChartRendererProps } from "../chart-model";
import type { GridKeyEntry, GridLine, GridModel, GridPoint, GridSeries } from "./grid";
import { STATE_TEXT, type Tone, toneClasses } from "./kind";

/** A shade's opacity at its lowest and its highest (from about #eff5fb to #9cbcdf on white). */
const SHADE_LOW = 0.07;
const SHADE_HIGH = 0.4;
/** A square of the squares form, and the room its names take. */
const SQUARE = 18;
const NAMES = 240;
const HEADS = 120;
/** About how many characters of a name fit 230 px at 12 px, past which it ends with "…". */
const NAME_CHARS = 34;

function isGridModel(model: ChartModel): model is GridModel {
  return "grid" in model;
}

function shadeOpacity(shade: number): number {
  return SHADE_LOW + (SHADE_HIGH - SHADE_LOW) * shade;
}

function clipped(name: string): string {
  return name.length > NAME_CHARS ? `${name.slice(0, NAME_CHARS - 1)}\u2026` : name;
}

/** A key's or a cell's colour: a state's mark, as a chart's key draws it. */
export function Swatch({
  notStartedGrey,
  tone,
}: {
  notStartedGrey: boolean;
  tone: Tone;
}): ReactNode {
  const classes = toneClasses(tone, notStartedGrey);
  return (
    <svg aria-hidden="true" className="inline-block shrink-0" height={10} width={10}>
      <rect
        className={`${classes.fill} ${classes.stroke}`}
        height={8}
        rx={1}
        strokeWidth={1.5}
        width={8}
        x={1}
        y={1}
      />
    </svg>
  );
}

/** A shade's swatch, or a cell with no value's. */
function ShadeSwatch({ shade }: { shade: number | null }): ReactNode {
  return (
    <svg aria-hidden="true" className="inline-block shrink-0" height={10} width={10}>
      <rect
        className={`${shade === null ? chartPlot.band : chartGroup[0].fill} ${chartPlot.rule}`}
        fillOpacity={shade === null ? undefined : shadeOpacity(shade)}
        height={8}
        strokeWidth={1}
        width={8}
        x={1}
        y={1}
      />
    </svg>
  );
}

function KeyEntry({
  entry,
  notStartedGrey,
}: {
  entry: GridKeyEntry;
  notStartedGrey: boolean;
}): ReactNode {
  if (entry.kind === "state") {
    return (
      <li className="flex items-center gap-1" data-key={entry.label}>
        <Swatch notStartedGrey={notStartedGrey} tone={entry.state} />
        {entry.label}
      </li>
    );
  }
  if (entry.kind === "empty") {
    return (
      <li className="flex items-center gap-1" data-key={entry.label}>
        <ShadeSwatch shade={null} />
        {entry.label}
      </li>
    );
  }
  return (
    <li className="flex items-center gap-1" data-key="shade">
      <ShadeSwatch shade={0} />
      {entry.low}
      {entry.high === null ? null : (
        <>
          {" to "}
          <ShadeSwatch shade={1} />
          {entry.high}
        </>
      )}
    </li>
  );
}

/** One line a cell prints, each number named as the table names it. */
function Line({
  line,
  model,
  point,
  series,
}: {
  line: GridLine;
  model: GridModel;
  point: GridPoint;
  series: GridSeries;
}): ReactNode {
  return (
    <span className="block whitespace-nowrap">
      {line.map((part) => {
        // A line's words differ from each other, and its numbers are each of their own statistic.
        if (part.statistic === null) {
          // Its definition's words, drawn as plain text: `eval-chart-cell-text`
          return <span key={`${part.range}:${part.text}`}>{part.text}</span>;
        }
        const values =
          part.range === "window"
            ? point.values
            : part.range === "compare"
              ? point.compare
              : point.change;
        const value = values?.[part.statistic]?.value ?? null;
        return (
          <span
            data-chart={model.name}
            data-range={part.range}
            data-series={series.name}
            data-statistic={part.statistic}
            data-value={value === null ? undefined : String(value)}
            data-x={point.x}
            key={`${part.range}:${part.statistic}`}
          >
            {/* The number as its line writes it, drawn as plain text: `eval-chart-cell-text` */}
            {part.text}
          </span>
        );
      })}
    </span>
  );
}

/** A cell of the table form: its shade or its state's mark, its lines, and its ghost. */
function Cell({
  compare,
  model,
  point,
  series,
}: {
  compare: boolean;
  model: GridModel;
  point: GridPoint;
  series: GridSeries;
}): ReactNode {
  return (
    <td
      className={`relative border ${borderDefault} px-2 py-1 align-top ${
        point.state !== null
          ? STATE_TEXT[point.state]
          : point.held
            ? textPrimaryOnSurface
            : textMutedOnSurface
      }`}
      data-cell-shade={point.shade === null ? undefined : point.shade.toFixed(3)}
      data-cell-state={point.state ?? undefined}
      data-grid-cell={`${point.x}\n${series.col}`}
      title={point.tip}
    >
      {point.shade === null ? null : (
        <span
          aria-hidden="true"
          className={`pointer-events-none absolute inset-0 ${chartGroup[0].swatch}`}
          style={{ opacity: shadeOpacity(point.shade) }}
        />
      )}
      <span className="relative flex items-start gap-1">
        <span className="flex flex-col">
          {point.lines.map((line, index) => (
            <Line
              key={line.map((part) => part.text).join("") || index}
              line={line}
              model={model}
              point={point}
              series={series}
            />
          ))}
          {compare
            ? point.compareLines.map((line, index) => (
                <span
                  className={`text-[11px] ${textMutedOnSurface}`}
                  data-ghost
                  key={line.map((part) => part.text).join("") || index}
                >
                  <Line line={line} model={model} point={point} series={series} />
                </span>
              ))
            : null}
        </span>
      </span>
    </td>
  );
}

/** The table form: the columns' title over their names, the rows' title over theirs, each row
 *  group's name over its rows, and a cell at each row and column. */
function TextGrid({ compare, model }: { compare: boolean; model: GridModel }): ReactNode {
  const { grid, series } = model;
  const grouped = grid.rows.some((row) => row.group !== "");
  return (
    <div className="overflow-x-auto">
      <table aria-label={model.title} className="border-collapse text-xs">
        <thead className={textSecondaryOnCanvas}>
          {grid.colTitle === null ? null : (
            <tr>
              <th />
              <th className="px-2 text-left font-medium" colSpan={series.length} scope="colgroup">
                {grid.colTitle}
              </th>
            </tr>
          )}
          <tr>
            <th className="px-2 text-left font-medium" scope="col">
              {grid.rowTitle ?? ""}
            </th>
            {series.map((one) => (
              <th className="px-2 text-left font-medium" key={one.name} scope="col">
                {one.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {grid.rows.flatMap((row, index) => {
            const heading =
              grouped && row.group !== "" && row.group !== grid.rows[index - 1]?.group ? (
                <tr data-row-group={row.group} key={`group:${row.group}`}>
                  <th
                    className={`px-2 pt-2 text-left font-semibold ${textSecondaryOnCanvas}`}
                    colSpan={series.length + 1}
                    scope="rowgroup"
                  >
                    {row.group}
                  </th>
                </tr>
              ) : null;
            return [
              ...(heading === null ? [] : [heading]),
              <tr data-grid-row={row.key} key={`row:${row.key}`}>
                <th
                  className={`px-2 py-1 text-left align-top font-medium ${textPrimaryOnSurface}`}
                  scope="row"
                >
                  {row.label}
                </th>
                {series.map((one) => {
                  const point = one.points[index];
                  return point === undefined ? null : (
                    <Cell
                      compare={compare}
                      key={one.name}
                      model={model}
                      point={point}
                      series={one}
                    />
                  );
                })}
              </tr>,
            ];
          })}
        </tbody>
      </table>
    </div>
  );
}

/** The squares form: the columns' names up over the squares, the rows' at their left, each cell a
 *  square in its state's colour or its shade, its row, column, value, total, state and note in its
 *  hover text; a muted dot in its corner where the compare range holds the cell. */
function SquareGrid({ compare, model }: { compare: boolean; model: GridModel }): ReactNode {
  const { grid, series } = model;
  const width = NAMES + series.length * SQUARE;
  const height = HEADS + grid.rows.length * SQUARE;
  return (
    <div className="overflow-x-auto">
      <svg
        aria-label={model.title}
        className="block"
        height={height}
        role="img"
        viewBox={`0 0 ${width} ${height}`}
        width={width}
      >
        {series.map((one, column) => (
          <text
            className={chartPlot.text}
            fontSize={12}
            key={one.name}
            transform={`rotate(-90 ${NAMES + column * SQUARE + SQUARE / 2 + 4} ${HEADS - 4})`}
            x={NAMES + column * SQUARE + SQUARE / 2 + 4}
            y={HEADS - 4}
          >
            {clipped(one.label)}
          </text>
        ))}
        {grid.rows.map((row, index) => (
          <text
            className={chartPlot.text}
            fontSize={12}
            key={row.key}
            textAnchor="end"
            x={NAMES - 6}
            y={HEADS + index * SQUARE + SQUARE / 2 + 4}
          >
            {clipped(row.label)}
          </text>
        ))}
        {series.map((one, column) =>
          one.points.map((point, index) => {
            const x = NAMES + column * SQUARE;
            const y = HEADS + index * SQUARE;
            const value = point.values.value?.value ?? null;
            const stated =
              point.state === null ? null : toneClasses(point.state, grid.notStartedGrey);
            const fill =
              stated !== null
                ? stated.fill
                : point.shade !== null
                  ? chartGroup[0].fill
                  : value === null
                    ? chartPlot.band
                    : chartPlot.hollow;
            const thenValue = point.compare?.value?.value ?? null;
            return (
              <g data-grid-cell={`${point.x}\n${one.col}`} key={`${one.name}:${point.x}`}>
                <rect
                  className={`${fill} ${point.state === "not-started" && stated !== null ? stated.stroke : chartPlot.rule}`}
                  data-cell-shade={point.shade === null ? undefined : point.shade.toFixed(3)}
                  data-cell-state={point.state ?? undefined}
                  data-chart={model.name}
                  data-range="window"
                  data-series={one.name}
                  data-statistic="value"
                  data-value={value === null ? undefined : String(value)}
                  data-x={point.x}
                  fillOpacity={
                    stated === null && point.shade !== null ? shadeOpacity(point.shade) : undefined
                  }
                  height={SQUARE}
                  strokeWidth={point.state === "not-started" ? 1.5 : 1}
                  width={SQUARE}
                  x={x}
                  y={y}
                >
                  <title>{point.tip}</title>
                </rect>
                {compare && point.compareHeld ? (
                  <circle
                    className={chartLine.comparePoint}
                    cx={x + SQUARE - 4}
                    cy={y + 4}
                    data-chart={model.name}
                    data-range="compare"
                    data-series={one.name}
                    data-statistic="value"
                    data-value={thenValue === null ? undefined : String(thenValue)}
                    data-x={point.x}
                    r={2.5}
                  >
                    <title>{`compare: ${point.compare?.value?.label ?? ""}`}</title>
                  </circle>
                ) : null}
              </g>
            );
          })
        )}
      </svg>
    </div>
  );
}

/**
 * Draws a `grid` model (`gridModel`): its mock-up line, the key of its shade or its states, the
 * line that says its values are in more than one unit, its cells in the form their count gives,
 * and the lines under it.
 */
export function GridRenderer({ compare, model }: ChartRendererProps): ReactNode {
  if (!isGridModel(model)) return null;
  const { grid } = model;
  return (
    <div className="flex min-w-0 flex-col gap-1" data-grid={grid.form}>
      {grid.mockup === null ? null : (
        <p className={`text-xs font-medium ${textMutedOnSurface}`} data-mockup-line>
          {grid.mockup}
        </p>
      )}
      {grid.key.length === 0 ? null : (
        <ul
          aria-label="Key"
          className={`flex flex-wrap gap-x-3 gap-y-0.5 text-xs ${textSecondaryOnCanvas}`}
        >
          {grid.key.map((entry) => (
            <KeyEntry
              entry={entry}
              key={entry.kind === "state" ? `state:${entry.label}` : entry.kind}
              notStartedGrey={grid.notStartedGrey}
            />
          ))}
        </ul>
      )}
      {grid.mixedUnits.length === 0 ? null : (
        <p className={`text-xs ${textMutedOnSurface}`} data-mixed-units>
          {`These readings are in more than one unit (${grid.mixedUnits.join(", ")}), so the grid shades no cell.`}
        </p>
      )}
      {grid.rows.length === 0 ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-plot-empty>
          No reading of this chart's series in the window.
        </p>
      ) : grid.form === "none" ? (
        <p className={`text-xs ${textMutedOnSurface}`} data-grid-too-large>
          {`This grid has ${grid.cells.toLocaleString("en-US")} cells: a grid of more than 10,000 is not drawn. Its table holds every reading.`}
        </p>
      ) : (
        <div className="relative">
          {grid.form === "squares" ? (
            <SquareGrid compare={compare} model={model} />
          ) : (
            <TextGrid compare={compare} model={model} />
          )}
          {grid.mockup === null ? null : (
            <span
              aria-hidden="true"
              className={`pointer-events-none absolute inset-0 flex items-center justify-center text-sm font-semibold opacity-60 ${textMutedOnSurface}`}
              data-mockup
            >
              MOCK-UP
            </span>
          )}
        </div>
      )}
      {grid.notes.length === 0 ? null : (
        <div className={`flex flex-col text-[11px] ${textMutedOnSurface}`} data-plot-notes>
          {grid.notes.map((note) => (
            <p key={note}>{note}</p>
          ))}
        </div>
      )}
    </div>
  );
}
