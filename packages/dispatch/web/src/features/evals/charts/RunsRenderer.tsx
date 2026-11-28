// The renderer of a `runs` model (runs.ts), the run view: a table of the runs, through the one
// table core, each run's links under its id, with the spec's columns and the start, and every
// other setting, tag and final number the runs hold behind "Choose columns"; a tick box on each
// run picks it; the picked runs' numbers of one series drawn over their steps, one line each, the
// series picked from those they hold; and the picked runs' settings side by side, the ones that
// differ first. While the compare range is shown, each run that started in it is listed too,
// marked, and its line is dashed; else only the window's runs are. Each number it draws names
// itself as the chart's table names it (`data-chart`, `data-series`, `data-statistic`, `data-x`,
// `data-range`, `data-value`).
import type { ColumnDef } from "@tanstack/react-table";
import { type ReactNode, useMemo, useState } from "react";

import { LinkList } from "../../../components/LinkList";
import {
  chartPlot,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../../theme/classes";
import type { ChartRendererProps, ChartValue } from "../chart-model";
import { EvalTable } from "../EvalTable";
import { toneClasses } from "./kind";
import {
  isRunsModel,
  RUN_COLUMNS,
  type Run,
  type RunsModel,
  STARTED,
  STEP_VALUE,
} from "./runs-model";

const WIDTH = 640;
const HEIGHT = 260;
const MARGIN = { bottom: 36, left: 56, right: 12, top: 10 };

/** One value of the run view, named in the DOM as the chart's table names it. */
function RunValue(props: {
  chart: string;
  series: string;
  statistic: string;
  x: string;
  range: Run["range"];
  value: ChartValue | undefined;
}): ReactNode {
  const { value } = props;
  if (value === undefined) return null;
  return (
    <span
      className={value.state === "absent" ? textMutedOnSurface : ""}
      data-chart={props.chart}
      data-range={props.range}
      data-series={props.series}
      data-statistic={props.statistic}
      data-value={value.value === null ? undefined : String(value.value)}
      data-x={props.x}
    >
      {value.label}
    </span>
  );
}

/** About four round ticks from `low` to `high`: 1, 2, 2.5 or 5 times a power of ten apart. */
function ticksOf(low: number, high: number): number[] {
  if (low === high) return [low];
  const raw = (high - low) / 4;
  const power = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((times) => times * power).find((one) => one >= raw) ?? raw;
  const ticks: number[] = [];
  for (let tick = Math.ceil(low / step) * step; tick <= high + step * 1e-9; tick += step) {
    ticks.push(Number(tick.toPrecision(12)));
  }
  return ticks;
}

/** A linear map of `values`' span, widened to its ticks, onto [from, to], and those ticks. */
function scaleOf(values: readonly number[], from: number, to: number) {
  const ticks = ticksOf(Math.min(...values), Math.max(...values));
  const low = Math.min(...values, ...ticks);
  const high = Math.max(...values, ...ticks);
  const span = high - low;
  return {
    at: (value: number) =>
      span === 0 ? (from + to) / 2 : from + ((value - low) / span) * (to - from),
    ticks,
  };
}

/** The picked runs' numbers of `series` over their steps: a line for each run, a dot at each step
 *  it holds a number at, a gap where it was not measured. */
function StepsPlot({
  model,
  runs,
  series,
  unit,
}: {
  model: RunsModel;
  runs: readonly Run[];
  series: string;
  unit: string;
}): ReactNode {
  const left = MARGIN.left;
  const right = WIDTH - MARGIN.right;
  const top = MARGIN.top;
  const bottom = HEIGHT - MARGIN.bottom;
  const steps = runs.flatMap((run) => run.steps[series] ?? []);
  const values = steps.flatMap((one) => (one.value.value === null ? [] : [one.value.value]));
  const x = scaleOf(
    steps.map((one) => one.step),
    left,
    right
  );
  const y = scaleOf(values.length === 0 ? [0] : values, bottom, top);
  return (
    <svg
      aria-label={`${model.title}: ${series} over ${model.runs.stepLabel}`}
      className="block h-auto w-full select-none"
      role="img"
      viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
    >
      {y.ticks.map((tick) => (
        <g key={`y${tick}`}>
          <line className={chartPlot.grid} x1={left} x2={right} y1={y.at(tick)} y2={y.at(tick)} />
          <text
            className={chartPlot.text}
            fontSize={10}
            textAnchor="end"
            x={left - 4}
            y={y.at(tick) + 3}
          >
            {tick.toLocaleString("en-US", { maximumFractionDigits: 3 })}
          </text>
        </g>
      ))}
      {x.ticks.map((tick) => (
        <text
          className={chartPlot.text}
          fontSize={10}
          key={`x${tick}`}
          textAnchor="middle"
          x={x.at(tick)}
          y={bottom + 14}
        >
          {tick.toLocaleString("en-US", { maximumFractionDigits: 3 })}
        </text>
      ))}
      <line className={chartPlot.axis} x1={left} x2={right} y1={bottom} y2={bottom} />
      <text
        className={chartPlot.text}
        data-axis="x"
        fontSize={11}
        textAnchor="middle"
        x={(left + right) / 2}
        y={HEIGHT - 6}
      >
        {model.runs.stepLabel}
      </text>
      <text
        className={chartPlot.text}
        data-axis="y"
        fontSize={11}
        textAnchor="middle"
        transform={`rotate(-90 14 ${(top + bottom) / 2})`}
        x={14}
        y={(top + bottom) / 2}
      >
        {unit === "" ? series : `${series} (${unit})`}
      </text>
      {runs.map((run) => {
        const classes = toneClasses(run.tone, false);
        const own = run.steps[series] ?? [];
        // A step that was not measured breaks the line.
        const path = own
          .map((one, index) => {
            if (one.value.value === null) return "";
            const move = index === 0 || own[index - 1]?.value.value === null ? "M" : "L";
            return `${move}${x.at(one.step)},${y.at(one.value.value)}`;
          })
          .filter((part) => part !== "")
          .join(" ");
        return (
          <g data-run-line={run.id} data-run-range={run.range} key={run.id}>
            <path
              className={`fill-none ${classes.stroke}`}
              d={path}
              strokeDasharray={run.range === "compare" ? "4 3" : undefined}
              strokeWidth={2}
            />
            {own.map((one) =>
              one.value.value === null ? null : (
                <circle
                  className={`${run.range === "compare" ? chartPlot.hollow : classes.fill} ${classes.stroke}`}
                  cx={x.at(one.step)}
                  cy={y.at(one.value.value)}
                  data-chart={model.name}
                  data-range={run.range}
                  data-series={series}
                  data-statistic={STEP_VALUE}
                  data-value={String(one.value.value)}
                  data-x={one.x}
                  key={one.x}
                  r={3}
                  strokeWidth={1.5}
                >
                  <title>{`${run.id}, ${model.runs.stepLabel} ${one.step}: ${one.value.label}`}</title>
                </circle>
              )
            )}
          </g>
        );
      })}
    </svg>
  );
}

/** The picked runs' settings side by side, one column for each run: the settings that differ
 *  between them first, then the rest, each in the run table's order of its columns; and a line
 *  that counts those that differ. */
function SettingsSideBySide({
  model,
  runs,
}: {
  model: RunsModel;
  runs: readonly Run[];
}): ReactNode {
  const rows = useMemo(() => {
    const settings = model.runs.columns.filter((column) => column.field === "setting");
    const marked = settings.map((column) => ({
      column,
      differs: new Set(runs.map((run) => run.cells[column.id]?.label)).size > 1,
    }));
    return [...marked.filter((row) => row.differs), ...marked.filter((row) => !row.differs)];
  }, [model, runs]);
  const columns = useMemo(
    (): ColumnDef<(typeof rows)[number]>[] => [
      {
        accessorFn: (row) => row.column.key,
        enableColumnFilter: false,
        header: "Setting",
        id: "setting",
      },
      ...runs.map(
        (run): ColumnDef<(typeof rows)[number]> => ({
          accessorFn: (row) => run.cells[row.column.id]?.label,
          cell: ({ row }) => (
            <RunValue
              chart={model.name}
              range={run.range}
              series={RUN_COLUMNS}
              statistic={row.original.column.id}
              value={run.cells[row.original.column.id]}
              x={run.id}
            />
          ),
          enableColumnFilter: false,
          header: run.id,
          id: `run:${run.id}`,
        })
      ),
    ],
    [model.name, runs]
  );
  const differ = rows.filter((row) => row.differs).length;
  return (
    <div className="flex flex-col gap-1" data-runs-settings>
      <p className={`text-xs ${textMutedOnSurface}`} data-runs-settings-differ>
        {`${differ} of ${rows.length} settings differ between the picked runs.`}
      </p>
      <EvalTable
        cells="text"
        columns={columns}
        empty="The picked runs hold no setting."
        getRowId={(row) => row.column.id}
        label={`${model.title}: the picked runs' settings`}
        layoutKey={`eval-runs-settings:${model.name}`}
        noun={{ many: "settings", one: "setting" }}
        rows={rows}
        scrollTestId={`${model.name}-settings-scroll`}
      />
    </div>
  );
}

function RunView({ compare, model }: { compare: boolean; model: RunsModel }): ReactNode {
  const { columns, list, name, names } = model.runs;
  const [picked, setPicked] = useState<ReadonlySet<string>>(() => new Set());
  const [added, setAdded] = useState<ReadonlySet<string>>(() => new Set());
  const [chosen, setChosen] = useState<string | null>(null);
  // The window's runs, and while the compare range is shown, the runs that started in it.
  const runs = useMemo(
    (): Run[] => (compare ? [...list] : list.filter((run) => run.range === "window")),
    [compare, list]
  );
  const shown = useMemo(
    () => columns.filter((column) => column.declared || added.has(column.id)),
    [added, columns]
  );
  const tableColumns = useMemo((): ColumnDef<Run>[] => {
    const value = (run: Run, statistic: string) => (
      <RunValue
        chart={model.name}
        range={run.range}
        series={RUN_COLUMNS}
        statistic={statistic}
        value={statistic === STARTED ? run.started : run.cells[statistic]}
        x={run.id}
      />
    );
    return [
      {
        cell: ({ row }) => (
          <input
            aria-label={`Pick run ${row.original.id}`}
            checked={picked.has(row.original.id)}
            onChange={(event) => {
              const next = new Set(picked);
              if (event.target.checked) next.add(row.original.id);
              else next.delete(row.original.id);
              setPicked(next);
            }}
            type="checkbox"
          />
        ),
        enableColumnFilter: false,
        enableSorting: false,
        header: "Pick",
        id: "pick",
      },
      {
        accessorFn: (run) => run.id,
        cell: ({ row }) => (
          <div className="flex min-w-0 flex-col gap-0.5">
            <span className="flex items-baseline gap-1" data-run={row.original.id}>
              {row.original.id}
              {row.original.range === "compare" ? (
                <span className={`text-xs ${textMutedOnSurface}`} data-run-compare>
                  started in the compare range
                </span>
              ) : null}
            </span>
            {row.original.links.length === 0 ? null : (
              <LinkList label={`Links of run ${row.original.id}`} links={row.original.links} />
            )}
          </div>
        ),
        enableColumnFilter: false,
        header: "Run",
        id: "run",
      },
      ...shown.map(
        (column): ColumnDef<Run> => ({
          accessorFn: (run) => {
            const held = run.cells[column.id];
            return column.numeric ? (held?.value ?? undefined) : held?.label;
          },
          cell: ({ row }) => value(row.original, column.id),
          enableColumnFilter: false,
          header: column.label,
          id: column.id,
          sortUndefined: "last",
        })
      ),
      {
        accessorFn: (run) => run.startedMs,
        cell: ({ row }) => value(row.original, STARTED),
        enableColumnFilter: false,
        header: "Started",
        id: STARTED,
      },
    ];
  }, [model.name, picked, shown]);

  if (list.length === 0) {
    return (
      <p className={`text-xs ${textMutedOnSurface}`} data-runs-empty>
        This eval holds no run yet.
      </p>
    );
  }
  const pickedRuns = runs.filter((run) => picked.has(run.id));
  // The series the picked runs hold numbers of at steps, those of more than one step first; the
  // spec's `name` at first, where a picked run holds it.
  const steps = (series: string) =>
    Math.max(0, ...pickedRuns.map((run) => run.steps[series]?.length ?? 0));
  const held = names.filter(({ name: series }) => steps(series) > 0);
  const listed = [
    ...held.filter(({ name: series }) => steps(series) > 1),
    ...held.filter(({ name: series }) => steps(series) === 1),
  ];
  const current =
    listed.find((one) => one.name === chosen) ??
    listed.find((one) => one.name === name) ??
    listed[0];
  const extra = columns.filter((column) => !column.declared);
  return (
    <div className="flex min-w-0 flex-col gap-2" data-runs>
      {extra.length === 0 ? null : (
        <details className={`text-xs ${textSecondaryOnCanvas}`}>
          <summary>Choose columns</summary>
          <fieldset aria-label="Choose columns" className="flex flex-wrap gap-x-3 gap-y-1 pt-1">
            {extra.map((column) => (
              <label className="flex items-center gap-1" key={column.id}>
                <input
                  checked={added.has(column.id)}
                  onChange={(event) => {
                    const next = new Set(added);
                    if (event.target.checked) next.add(column.id);
                    else next.delete(column.id);
                    setAdded(next);
                  }}
                  type="checkbox"
                />
                {column.label}
              </label>
            ))}
          </fieldset>
        </details>
      )}
      <EvalTable
        cells="text"
        columns={tableColumns}
        empty="No run started in the window."
        getRowId={(run) => run.id}
        label={`${model.title}: the runs`}
        layoutKey={`eval-runs:${model.name}`}
        noun={{ many: "runs", one: "run" }}
        rows={runs}
        scrollTestId={`${model.name}-runs-scroll`}
      />
      <section
        aria-label="The picked runs over time"
        className="flex min-w-0 flex-col gap-1"
        data-runs-over-time
      >
        <h5 className={`text-xs font-semibold ${textPrimaryOnSurface}`}>
          The picked runs over time
        </h5>
        {pickedRuns.length === 0 ? (
          <p className={`text-xs ${textMutedOnSurface}`}>
            Pick runs in the table to draw them over their steps.
          </p>
        ) : current === undefined ? (
          <p className={`text-xs ${textMutedOnSurface}`}>
            The picked runs hold no number over steps.
          </p>
        ) : (
          <>
            <label className={`flex items-center gap-1 text-xs ${textSecondaryOnCanvas}`}>
              Number
              <select onChange={(event) => setChosen(event.target.value)} value={current.name}>
                {listed.map((one) => (
                  <option key={one.name} value={one.name}>
                    {one.name}
                  </option>
                ))}
              </select>
            </label>
            <StepsPlot model={model} runs={pickedRuns} series={current.name} unit={current.unit} />
            <ul
              aria-label="Key"
              className={`flex flex-wrap gap-x-3 gap-y-0.5 text-xs ${textSecondaryOnCanvas}`}
            >
              {pickedRuns.map((run) => (
                <li className="flex items-center gap-1" data-key={run.id} key={run.id}>
                  <span
                    aria-hidden="true"
                    className={`inline-block h-2 w-2 rounded-full ${toneClasses(run.tone, false).swatch}`}
                  />
                  {run.range === "compare" ? `${run.id} (compare range)` : run.id}
                </li>
              ))}
            </ul>
          </>
        )}
        {pickedRuns.length === 0 ? null : <SettingsSideBySide model={model} runs={pickedRuns} />}
      </section>
    </div>
  );
}

export function RunsRenderer({ compare, model }: ChartRendererProps): ReactNode {
  if (!isRunsModel(model)) return null;
  return <RunView compare={compare} model={model} />;
}
