// The renderer of a declared `text` (dispatch://INFRA-2918/artifact/plan-md, D10): its table, a
// column of the rows' names (with the lines `row_subtitles` write under each) and one column for
// each declared column, at its declared share of the width. A value cell prints its value, "of"
// its total, its bar with its share or its dots, its interval and, under it, its note; a note cell
// its note (large with `lead`) and, under it, its value; a spark cell a small line of its series
// over the window and its newest value. A value's state from `value_states` is a dot in the
// state's colour, its label the cell's tooltip. While the compare range is shown, each value
// prints its compare value and its change beside it. Each figure names itself as the chart's
// table names it (`FigureText`).
import type { ReactNode } from "react";

import {
  borderDefault,
  borderStrong,
  borderTransparent,
  chartPlot,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../../theme/classes";
import type { ChartRendererProps } from "../chart-model";
import { ComparedText, type FigurePlace, FigureText } from "./FigureText";
import { toneClasses } from "./kind";
import { isTextModel, type TextCell, type TextModel, type TextSeries } from "./text-model";

const SPARK_WIDTH = 80;
const SPARK_HEIGHT = 18;
/** The blue a table's bars, dots and spark lines draw in: the palette's first. */
const BLUE = toneClasses(0, false);

/** A value's state: a dot in the state's colour. */
function StateDot({ cell }: { cell: TextCell }): ReactNode {
  if (cell.state === null) return null;
  return (
    <span
      aria-hidden="true"
      className={`inline-block size-2 shrink-0 self-center rounded-full ${toneClasses(cell.state.state, false).swatch}`}
    />
  );
}

/** A value with, where it has them, "of" its total, its share and its interval. */
function Valued(props: { className?: string; compare: boolean; place: FigurePlace }): ReactNode {
  const { place } = props;
  const { values } = place.point;
  return (
    <span className="flex flex-wrap items-baseline gap-x-1">
      <FigureText className={props.className} place={place} range="window" statistic="value" />
      {values.n === undefined ? null : (
        <>
          <span className={`text-xs ${textMutedOnSurface}`}>of</span>
          <FigureText place={place} range="window" statistic="n" />
        </>
      )}
      {values.share === undefined ? null : (
        <FigureText className="text-xs" place={place} range="window" statistic="share" />
      )}
      {values.ci_low?.value == null ? null : (
        <span className={`text-xs ${textMutedOnSurface}`}>
          {"["}
          <FigureText place={place} range="window" statistic="ci_low" />
          {", "}
          <FigureText place={place} range="window" statistic="ci_high" />
          {"]"}
        </span>
      )}
      <ComparedText compare={props.compare} place={place} statistic="value" />
    </span>
  );
}

/** A bar cell's bar: its length a share of the cell's width. */
function Bar({ share }: { share: number }): ReactNode {
  return (
    <span className={`block h-1.5 w-full rounded-sm ${surfaceMutedBg}`} data-text-bar={share}>
      <span
        className={`block h-full rounded-sm ${BLUE.swatch}`}
        style={{ width: `${share * 100}%` }}
      />
    </span>
  );
}

/** A dots cell's dots: one for each unit of its total, filled up to its value. */
function Dots({ dots }: { dots: { filled: number; total: number } }): ReactNode {
  return (
    <span className="flex flex-wrap gap-0.5" data-text-dots={`${dots.filled}/${dots.total}`}>
      {Array.from({ length: dots.total }, (_unused, at) => (
        <span
          className={`size-2 rounded-full border ${at < dots.filled ? `${borderTransparent} ${BLUE.swatch}` : borderStrong}`}
          data-filled={at < dots.filled}
          // biome-ignore lint/suspicious/noArrayIndexKey: a dot is its place among the total's units.
          key={at}
        />
      ))}
    </span>
  );
}

/** A spark cell's line: its readings over the window, 80 by 18 px with no axis, and a dotted done
 *  line at the top of a share. */
function Spark({ cell, done }: { cell: TextCell; done: number | null }): ReactNode {
  if (cell.spark.length === 0) return null;
  const ats = cell.spark.map((point) => point.at);
  const values = [...cell.spark.map((point) => point.value), ...(done === null ? [] : [done])];
  const [first, last] = [Math.min(...ats), Math.max(...ats)];
  const [low, high] = [Math.min(0, ...values), Math.max(...values)];
  const x = (at: number) =>
    last === first ? SPARK_WIDTH / 2 : 1 + ((at - first) / (last - first)) * (SPARK_WIDTH - 2);
  const y = (value: number) =>
    high === low
      ? SPARK_HEIGHT / 2
      : SPARK_HEIGHT - 1 - ((value - low) / (high - low)) * (SPARK_HEIGHT - 2);
  return (
    <svg
      aria-hidden="true"
      className="shrink-0"
      data-text-spark={cell.spark.length}
      height={SPARK_HEIGHT}
      viewBox={`0 0 ${SPARK_WIDTH} ${SPARK_HEIGHT}`}
      width={SPARK_WIDTH}
    >
      {done === null ? null : (
        <line
          className={chartPlot.rule}
          data-text-done={done}
          strokeDasharray="2 2"
          x1={0}
          x2={SPARK_WIDTH}
          y1={y(done)}
          y2={y(done)}
        />
      )}
      {cell.spark.length === 1 ? (
        <circle
          className={BLUE.fill}
          cx={x(cell.spark[0]?.at ?? 0)}
          cy={y(cell.spark[0]?.value ?? 0)}
          r={1.5}
        />
      ) : (
        <polyline
          className={`fill-none ${BLUE.stroke}`}
          points={cell.spark.map((point) => `${x(point.at)},${y(point.value)}`).join(" ")}
          strokeWidth={1.5}
        />
      )}
    </svg>
  );
}

/** One cell of the table, as its column draws it. */
function Cell(props: {
  cell: TextCell;
  compare: boolean;
  model: TextModel;
  series: TextSeries;
}): ReactNode {
  const { cell, series } = props;
  const { column } = series;
  const place: FigurePlace = { chart: props.model.name, point: cell, series: series.name };
  const note = cell.values.note;
  const lead = "text-[15px] font-semibold";
  if (column.kind === "spark") {
    return (
      <span className="flex items-center gap-2">
        <Spark cell={cell} done={column.done} />
        <Valued compare={props.compare} place={place} />
      </span>
    );
  }
  if (column.kind === "note") {
    return (
      <span className="flex flex-col gap-0.5">
        {note === undefined ? null : (
          <span className={`flex items-baseline gap-1 ${column.lead ? lead : ""}`}>
            {column.lead ? <StateDot cell={cell} /> : null}
            <FigureText place={place} range="window" statistic="note" />
          </span>
        )}
        {cell.values.value?.value == null ? null : (
          <span className={`flex items-baseline gap-1 text-xs ${textMutedOnSurface}`}>
            {column.lead ? null : <StateDot cell={cell} />}
            <Valued compare={props.compare} place={place} />
          </span>
        )}
      </span>
    );
  }
  return (
    <span className="flex flex-col gap-0.5">
      <span className="flex items-baseline gap-1">
        <StateDot cell={cell} />
        <Valued className={cell.state === null ? "" : lead} compare={props.compare} place={place} />
      </span>
      {cell.dots === null ? null : <Dots dots={cell.dots} />}
      {cell.bar === null ? null : <Bar share={cell.bar} />}
      {note === undefined ? null : (
        <span className={`text-xs ${textMutedOnSurface}`}>{note.label}</span>
      )}
    </span>
  );
}

/**
 * A declared text table (plan D10); in its place, the line that says its series holds no reading
 * of the window, and under it the line that counts the rows it leaves out.
 */
export function TextRenderer({ compare, model }: ChartRendererProps): ReactNode {
  if (!isTextModel(model)) return null;
  if (model.text.empty !== null) {
    return (
      <p className={`text-xs ${textMutedOnSurface}`} data-text-empty={model.name}>
        {model.text.empty}
      </p>
    );
  }
  const head = `border-b ${borderDefault} px-2 py-1 text-left align-bottom text-xs font-medium ${textMutedOnSurface}`;
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <div className="overflow-x-auto">
        <table
          className={`w-full border-collapse text-sm ${textPrimaryOnSurface}`}
          data-text-table={model.name}
        >
          <thead>
            <tr>
              <th className={head} scope="col">
                {model.axis.label}
              </th>
              {model.series.map((series) => (
                <th
                  className={head}
                  key={series.name}
                  scope="col"
                  style={
                    series.column.width === null
                      ? undefined
                      : { width: `${series.column.width * 100}%` }
                  }
                >
                  {series.column.header}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {model.text.rows.map((row, index) => (
              <tr className={`border-b ${borderDefault}`} data-text-row={row.name} key={row.name}>
                <th className="px-2 py-1 text-left align-top font-normal" scope="row">
                  <span className="block">{row.name}</span>
                  {row.subtitles.map((line) => (
                    <span className={`block text-xs ${textMutedOnSurface}`} key={line}>
                      {line}
                    </span>
                  ))}
                </th>
                {model.series.map((series) => {
                  const cell = series.points[index];
                  return (
                    <td
                      className="px-2 py-1 align-top"
                      data-state={cell?.state?.state}
                      data-text-cell={series.name}
                      key={series.name}
                      title={cell?.state?.label}
                    >
                      {cell === undefined ? null : (
                        <Cell cell={cell} compare={compare} model={model} series={series} />
                      )}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {model.text.notes.map((note) => (
        <p className={`text-xs ${textMutedOnSurface}`} key={note}>
          {note}
        </p>
      ))}
    </div>
  );
}
