import type { ColumnDef } from "@tanstack/react-table";
import { type ReactNode, useId, useMemo, useState } from "react";

import {
  borderStrong,
  dangerText,
  dragHandleBg,
  successText,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { lastUtcDate, utcDate } from "../delivery/lib/window";
import type {
  ChartModel,
  ChartPoint,
  ChartRenderer,
  ChartSeries,
  ChartValue,
  ValueState,
} from "./chart-model";
import { EvalTitle } from "./EvalPanel";
import { EvalTable } from "./EvalTable";

/** A drawn value's colour by its state: its own verdict, or muted where there is nothing to
 *  measure. */
const TONE: Readonly<Record<ValueState, string>> = {
  absent: textMutedOnSurface,
  met: successText,
  missed: dangerText,
  plain: "",
};

/** What a chart shows: its graph, or its table. */
export type ChartMode = "graph" | "table";

/** The Graph | Table segmented toggle every chart of the tab draws, the merge timeline's too, as
 *  the Delivery page's Timeline | List toggle draws. */
export function ChartModeToggle({
  mode,
  onChange,
}: {
  mode: ChartMode;
  onChange: (mode: ChartMode) => void;
}): ReactNode {
  const item = (value: ChartMode, label: string) => (
    <button
      aria-pressed={mode === value}
      className={`h-6 px-2 text-xs font-medium first:rounded-l-md last:rounded-r-md ${
        mode === value ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}` : textSecondaryOnCanvas
      }`}
      onClick={() => onChange(value)}
      type="button"
    >
      {label}
    </button>
  );
  return (
    <fieldset aria-label="Graph or table" className={`flex rounded-md border ${borderStrong}`}>
      {item("graph", "Graph")}
      <span aria-hidden="true" className={`w-px ${dragHandleBg}`} />
      {item("table", "Table")}
    </fieldset>
  );
}

/** One series' window figures, each with its change from the compare range while that is shown,
 *  then what the series says of its input (spec R18) and the rows it counts by the project's
 *  default repository (spec R15), beside the value they qualify. They stand outside the graph and
 *  the table, which draw the series a day at a time, so they name no `data-value`. */
function SeriesFigures({ compare, series }: { compare: boolean; series: ChartSeries }): ReactNode {
  return (
    <div
      className={`flex flex-wrap items-baseline gap-x-3 gap-y-0.5 ${textPrimaryOnSurface}`}
      data-window-series={series.name}
    >
      {series.headline.map((id, index) => {
        const value = series.window.values[id];
        if (value === undefined) return null;
        const change = series.window.change?.[id];
        const label = series.statistics.find((statistic) => statistic.id === id)?.label ?? id;
        return (
          <span className="flex items-baseline gap-1" key={id}>
            <span
              className={`${index === 0 ? "text-lg" : "text-sm"} font-semibold ${TONE[value.state]}`}
              data-window-statistic={id}
            >
              {value.label}
            </span>
            <span className={`text-xs ${textMutedOnSurface}`}>{label.toLowerCase()}</span>
            {compare && change !== undefined ? (
              <span className={`text-xs ${textMutedOnSurface}`} data-window-change={id}>
                {change.value === null ? "no change to compare" : `${change.label} vs compare`}
              </span>
            ) : null}
          </span>
        );
      })}
      {series.dataNote === null ? null : (
        <span className={`text-xs ${textMutedOnSurface}`} data-note="data">
          {series.dataNote}
        </span>
      )}
      {series.repositoryNote === null ? null : (
        <span className={`text-xs ${textMutedOnSurface}`} data-note="repository">
          {series.repositoryNote}
        </span>
      )}
    </div>
  );
}

/** One row of a chart's table: a place on its axis, and each series' point there. */
interface TableRow {
  readonly id: string;
  readonly label: string;
  readonly points: readonly (ChartPoint | undefined)[];
}

/** Which of a point's values a column holds: its own, the compare range's, or the change. */
type Part = "values" | "compare" | "change";

/** One value of the table, named in the DOM as the graph names the value it draws. */
function TableValue(props: {
  chart: string;
  series: string;
  statistic: string;
  x: string;
  part: Part;
  value: ChartValue | undefined;
}): ReactNode {
  const { value } = props;
  if (value === undefined) return null;
  return (
    <span
      className={TONE[value.state]}
      data-chart={props.chart}
      data-range={props.part === "values" ? "window" : props.part}
      data-series={props.series}
      data-statistic={props.statistic}
      data-value={value.value === null ? undefined : String(value.value)}
      data-x={props.x}
    >
      {value.label}
    </span>
  );
}

/** What each row of a chart's table holds after its values, in a column of its own: a Legion
 *  day's or role's link to the issues it counts. */
export interface RowLink {
  readonly header: string;
  /** The row's cell, by the row's place on the chart's axis. */
  readonly cell: (x: string) => ReactNode;
}

/** The table's columns: the axis, then each statistic of each series, and with the compare range
 *  shown, each one's compare value and its change beside it; then the row's link, where the chart
 *  has one. */
function tableColumns(
  model: ChartModel,
  compare: boolean,
  rowLink: RowLink | undefined
): ColumnDef<TableRow>[] {
  const several = model.series.length > 1;
  const columns: ColumnDef<TableRow>[] = [
    {
      accessorFn: (row) => row.id,
      cell: ({ row }) => row.original.label,
      enableColumnFilter: false,
      header: model.axis.label,
      id: "x",
    },
  ];
  model.series.forEach((series, index) => {
    for (const statistic of series.statistics) {
      const name = several ? `${series.label} ${statistic.label.toLowerCase()}` : statistic.label;
      const column = (part: Part, header: string): ColumnDef<TableRow> => ({
        accessorFn: (row) => row.points[index]?.[part]?.[statistic.id]?.value ?? undefined,
        cell: ({ row }) => (
          <TableValue
            chart={model.name}
            part={part}
            series={series.name}
            statistic={statistic.id}
            value={row.original.points[index]?.[part]?.[statistic.id]}
            x={row.original.id}
          />
        ),
        enableColumnFilter: false,
        header,
        id: `${series.name}:${statistic.id}:${part}`,
        sortUndefined: "last",
      });
      columns.push(column("values", name));
      if (compare)
        columns.push(column("compare", `${name} compare`), column("change", `${name} change`));
    }
  });
  if (rowLink !== undefined)
    columns.push({
      cell: ({ row }) => rowLink.cell(row.original.id),
      enableColumnFilter: false,
      enableSorting: false,
      header: rowLink.header,
      id: "link",
    });
  return columns;
}

/** A chart's table: one row per place on the chart's axis, every row drawn (`EvalTable`), so the
 *  table holds every number the graph draws in the page, as well as on the screen. */
function ChartTable({
  compare,
  model,
  rowLink,
}: {
  compare: boolean;
  model: ChartModel;
  rowLink: RowLink | undefined;
}): ReactNode {
  const columns = useMemo(() => tableColumns(model, compare, rowLink), [model, compare, rowLink]);
  const rows = useMemo(
    (): TableRow[] =>
      (model.series[0]?.points ?? []).map((point, index) => ({
        id: point.x,
        label: point.partial ? `${point.label} (so far)` : point.label,
        points: model.series.map((series) => series.points[index]),
      })),
    [model]
  );
  return (
    <EvalTable
      columns={columns}
      empty={`No ${model.axis.many}.`}
      getRowId={(row) => row.id}
      label={`${model.title} per ${model.axis.one}`}
      layoutKey={`eval-chart:${model.name}${compare ? ":compare" : ""}`}
      noun={{ many: model.axis.many, one: model.axis.one }}
      rows={rows}
      scrollTestId={`${model.name}-table-scroll`}
    />
  );
}

/**
 * The one chart component of the Evals tab (dispatch://INFRA-2918/artifact/plan-md, D4; spec R7):
 * it draws `model` through `renderer`, the renderer of its kind, under two controls. Graph or
 * Table: the table is the one table core, one row per place on the chart's axis, whose columns are
 * the model's values and, while the compare range is shown, each value's compare value and its
 * change, so it holds every number the graph draws. Compare: the header's compare range, shown by
 * default where the header names one and drawn as a muted second series; a chart that takes no
 * compare (`pending`, the merge timeline) has no such control. Both modes keep the reader's
 * compare choice. Each series' window figures stand above, with what the series says of its input
 * and of the rows it counts by default repository beside them, in a section as on a card. On the
 * Evals tab the title opens the number's panel of the eval `opensPanel` names (`EvalTitle`). A
 * declared chart's own words stand under its title (`subtitle`) and after its graph or table
 * (`footer`), in both modes.
 */
export function EvalChart({
  beside,
  footer,
  model,
  opensPanel,
  renderer: Renderer,
  rowLink,
  subtitle,
}: {
  /** What stands beside the chart's title: a card's verdict. */
  beside?: ReactNode;
  /** What stands after the graph or the table: a declared chart's footnote and notes. */
  footer?: ReactNode;
  model: ChartModel;
  /** The eval whose panel the title opens, where the chart draws one eval. */
  opensPanel?: string;
  renderer: ChartRenderer;
  /** What each row of the table holds after its values, where the chart's rows link anywhere. */
  rowLink?: RowLink;
  /** What stands under the title: a declared chart's subtitle. */
  subtitle?: ReactNode;
}): ReactNode {
  const titleId = useId();
  const [mode, setMode] = useState<ChartMode>("graph");
  // The reader's choice, kept apart from whether the header names a compare range: turned off
  // here, the compare stays off across Graph and Table and across a new answer.
  const [hidden, setHidden] = useState(false);
  const compareRange = model.comparable ? model.compareRange : null;
  const compare = compareRange !== null && !hidden;
  const compareEmpty =
    compare &&
    model.series.every((series) =>
      series.points.every((point) =>
        Object.values(point.compare ?? {}).every((value) => value.value === null)
      )
    );
  return (
    <figure
      aria-labelledby={titleId}
      className="m-0 flex min-w-0 flex-col gap-1"
      data-chart={model.name}
      data-chart-kind={model.kind}
    >
      <div className="flex flex-wrap items-center gap-2">
        <figcaption className={`mr-auto text-xs ${textMutedOnSurface}`} id={titleId}>
          <EvalTitle slug={opensPanel} title={model.title} />
        </figcaption>
        {beside}
        <ChartModeToggle mode={mode} onChange={setMode} />
        {model.comparable ? (
          <label className={`flex items-center gap-1 text-xs ${textSecondaryOnCanvas}`}>
            <input
              checked={compare}
              disabled={compareRange === null}
              onChange={(event) => setHidden(!event.target.checked)}
              type="checkbox"
            />
            Compare
          </label>
        ) : null}
      </div>
      {subtitle}
      {model.series.map((series) => (
        <SeriesFigures compare={compare} key={series.name} series={series} />
      ))}
      {compare && compareRange !== null ? (
        <p className={`text-[11px] ${textMutedOnSurface}`} data-compare-legend>
          {`compare ${utcDate(compareRange.from)} to ${lastUtcDate(compareRange.to)}`}
          {compareEmpty ? ": no data" : ""}
        </p>
      ) : null}
      {mode === "graph" ? (
        <Renderer compare={compare} model={model} />
      ) : (
        <ChartTable
          compare={compare}
          key={compare ? "compare" : "window"}
          model={model}
          rowLink={rowLink}
        />
      )}
      {footer}
    </figure>
  );
}
