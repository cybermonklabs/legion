// The renderer of a `lanes` chart (dispatch://INFRA-2918/artifact/plan-md, D10): a row for each
// lane, with its group's name at its group's first lane, its own name, its row columns' cells,
// its lane over the time axis and its end text, under a row of headers and the labels of the
// dated rules, and over the time axis's ticks. A lane draws each earlier state as a thin grey
// line, a partial one dashed, the newest as a bar in its state's colour (a dot where it began at
// the newest reading), and its marks as near-black dots with their notes; while the compare range
// is shown, the compare range's states and marks, shifted onto the window's axis, as a muted line
// under the lane, and each row column's compare value beside its own. Each value it draws names
// itself as the chart's table names it (`data-chart`, `data-series`, `data-statistic`, `data-x`,
// `data-range`, `data-value`), so the table holds every number the chart draws.
//
// Each lane is as high as the model's `pitch` (24 px down to 18 px as the lanes grow from 26 to
// 35), and the chart takes the platform's narrow form where its wide form would not fit
// (`useNarrowForm`): no header over the groups' names, the lanes' names and the end texts, each
// group's label over its first lane's name in the one column of names, no rule between groups,
// and each lane's end text under its line, ending where its newest reading stands. So the chart
// never scrolls sideways.
import { type ReactNode, type RefObject, useLayoutEffect, useRef, useState } from "react";

import {
  borderDefault,
  chartGroupOther,
  chartLine,
  chartPlot,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../../theme/classes";
import type { ChartModel, ChartPoint, ChartRendererProps } from "../chart-model";
import { timeLabel } from "./grid";
import { STATE_TEXT, toneClasses } from "./kind";
import type { Lane, LaneRange, LaneSpan, LanesModel } from "./lanes";

const DAY = 24 * 60 * 60 * 1000;
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
/** A tier of the rules' labels: a label and its detail. */
const TIER = 24;
/** About how much of the plot's width a character of a rule's label takes, in percent. */
const CHAR_PERCENT = 1.1;
/** A lane chart narrower than this takes its narrow form, and so does one whose wide form leaves
 *  its plot less than PLOT_MIN px beside its columns: room for five dates 55 px apart (the
 *  platform's lane chart). */
const NARROW_WIDTH = 560;
const PLOT_MIN = 300;

function isLanesModel(model: ChartModel): model is LanesModel {
  return "lanes" in model;
}

/** A lane's geometry at its height (`LanesPlot.pitch`): its line's place in it, the compare
 *  range's line under that, and its newest state's bar, half its height, as the platform draws
 *  it. */
interface LaneBox {
  readonly height: number;
  readonly middle: number;
  readonly under: number;
  readonly bar: number;
}

/** The DOM names of a value of `point`, as the chart's table names it. */
function valueNames(
  model: LanesModel,
  series: string,
  statistic: string,
  point: ChartPoint,
  range: "window" | "compare"
): Record<string, string | undefined> {
  const value = (range === "window" ? point.values : point.compare)?.[statistic]?.value ?? null;
  return {
    "data-chart": model.name,
    "data-range": range,
    "data-series": series,
    "data-statistic": statistic,
    "data-value": value === null ? undefined : String(value),
    "data-x": point.x,
  };
}

/** The ticks of the time axis: each day, or each Monday, about eight at most; a range with no
 *  Monday ticks the start of its first day. */
function ticksOf(from: number, to: number, weekly: boolean): number[] {
  const unit = weekly ? 7 * DAY : DAY;
  // A day's first moment, or a Monday's (the epoch began on a Thursday).
  const start = weekly
    ? Math.ceil((from - 4 * DAY) / unit) * unit + 4 * DAY
    : Math.ceil(from / DAY) * DAY;
  if (start > to) return [Math.floor(from / DAY) * DAY];
  const every = Math.max(1, Math.ceil((Math.floor((to - start) / unit) + 1) / 8));
  const ticks: number[] = [];
  for (let tick = start; tick <= to; tick += unit * every) ticks.push(tick);
  return ticks;
}

function dayLabel(at: number): string {
  const day = new Date(at);
  return `${day.getUTCDate()} ${MONTHS[day.getUTCMonth()]}`;
}

/** Where a time stands across the plot: its share of the time axis (0 to 100), and that share as
 *  an SVG length; and the length from one time to another. */
interface Place {
  readonly share: (ms: number) => number;
  readonly at: (ms: number) => string;
  readonly width: (from: number, to: number) => string;
}

/**
 * Whether the chart drawn in `chart` takes its narrow form: where it is narrower than
 * NARROW_WIDTH, or its wide form leaves its plot less than PLOT_MIN px. The wide form's columns
 * are measured whenever it is drawn, and held while the narrow form is, whose columns are not
 * the wide form's, so a chart that has room again takes the wide form back. A chart that is not
 * laid out (0 px wide, as in a test's DOM) takes the wide form.
 */
function useNarrowForm(chart: RefObject<HTMLDivElement | null>): boolean {
  const [width, setWidth] = useState(0);
  const [columns, setColumns] = useState(0);
  const narrow = width > 0 && (width < NARROW_WIDTH || width - columns < PLOT_MIN);
  useLayoutEffect(() => {
    const element = chart.current;
    if (element === null) return;
    const read = () => setWidth(element.getBoundingClientRect().width);
    read();
    if (typeof ResizeObserver === "undefined") return;
    const sizes = new ResizeObserver(read);
    sizes.observe(element);
    return () => sizes.disconnect();
  }, [chart]);
  // The room the wide form's columns take beside its plot, read after each paint of that form.
  useLayoutEffect(() => {
    const element = chart.current;
    if (narrow || element === null) return;
    const plot = element.querySelector("[data-lane-axis]");
    if (plot === null) return;
    const drawnAt = element.getBoundingClientRect().width;
    if (drawnAt === 0) return;
    const room = drawnAt - plot.getBoundingClientRect().width;
    setColumns((held) => (Math.abs(held - room) < 1 ? held : room));
  });
  return narrow;
}

/** One range's states and marks of a lane: the window's in their colours, the compare range's
 *  muted under the lane. */
function LaneMarks({
  box,
  lane,
  model,
  place,
  point,
  range,
}: {
  box: LaneBox;
  lane: LaneRange;
  model: LanesModel;
  place: Place;
  point: ChartPoint;
  range: "window" | "compare";
}): ReactNode {
  const { at, width } = place;
  const grey = model.lanes.notStartedGrey;
  const ySeries = model.series[0]?.name ?? "";
  const muted = range === "compare";
  const spanOf = (span: LaneSpan) => {
    const names = span.newest ? valueNames(model, ySeries, "value", point, range) : {};
    const title = (
      <title>{`${span.label}: ${timeLabel(span.from)} to ${timeLabel(span.to)}`}</title>
    );
    const key = `${range}:${span.from}`;
    const classes = toneClasses(span.state, grey);
    if (muted) {
      if (!span.newest) {
        return (
          <line
            className={chartLine.compareStroke}
            data-lane-span={span.state}
            key={key}
            strokeWidth={2}
            x1={at(span.from)}
            x2={at(span.to)}
            y1={box.under}
            y2={box.under}
          >
            {title}
          </line>
        );
      }
      return span.dot ? (
        <circle
          className={chartLine.comparePoint}
          cx={at(span.from)}
          cy={box.under}
          data-lane-span={span.state}
          key={key}
          r={3}
          {...names}
        >
          {title}
        </circle>
      ) : (
        <rect
          className={chartLine.comparePoint}
          data-lane-span={span.state}
          height={4}
          key={key}
          width={width(span.from, span.to)}
          x={at(span.from)}
          y={box.under - 2}
          {...names}
        >
          {title}
        </rect>
      );
    }
    if (span.dot) {
      return (
        <circle
          className={`${classes.fill} ${classes.stroke}`}
          cx={at(span.from)}
          cy={box.middle}
          data-lane-span={span.state}
          data-lane-shape="dot"
          key={key}
          r={4.5}
          strokeWidth={1.5}
          {...names}
        >
          {title}
        </circle>
      );
    }
    if (span.state === "partial" || !span.newest) {
      return (
        <line
          className={span.state === "partial" ? classes.stroke : chartGroupOther.stroke}
          data-lane-shape={span.state === "partial" ? "dashed" : "line"}
          data-lane-span={span.state}
          key={key}
          strokeDasharray={span.state === "partial" ? "4 3" : undefined}
          strokeWidth={span.state === "partial" ? 2 : 1.5}
          x1={at(span.from)}
          x2={at(span.to)}
          y1={box.middle}
          y2={box.middle}
          {...names}
        >
          {title}
        </line>
      );
    }
    return (
      <rect
        className={`${classes.fill} ${classes.stroke}`}
        data-lane-shape="bar"
        data-lane-span={span.state}
        height={box.bar}
        key={key}
        strokeWidth={1}
        width={width(span.from, span.to)}
        x={at(span.from)}
        y={box.middle - box.bar / 2}
        {...names}
      >
        {title}
      </rect>
    );
  };
  const earlier = lane.spans.filter((span) => !span.newest);
  const newest = lane.spans.filter((span) => span.newest);
  const overall = toneClasses("overall", grey);
  return (
    <g data-lane-range={range}>
      {earlier.map(spanOf)}
      {lane.marks.map((mark) =>
        muted ? (
          <circle
            className={chartLine.comparePoint}
            cx={at(mark.at)}
            cy={box.under}
            data-lane-mark={mark.id}
            key={mark.id}
            r={3}
          >
            <title>{`${mark.note === "" ? "mark" : mark.note}: ${timeLabel(mark.at)}`}</title>
          </circle>
        ) : (
          <g data-lane-mark={mark.id} key={mark.id}>
            <circle className={overall.fill} cx={at(mark.at)} cy={box.middle} r={4.5}>
              <title>{`${mark.note === "" ? "mark" : mark.note}: ${timeLabel(mark.at)}`}</title>
            </circle>
            {mark.note === "" ? null : (
              <text
                className={chartPlot.text}
                dx={7}
                fontSize={9}
                x={at(mark.at)}
                y={box.middle - 4}
              >
                {mark.note}
              </text>
            )}
          </g>
        )
      )}
      {newest.map(spanOf)}
    </g>
  );
}

/** One lane's plot: its line (none where groups part the lanes), the dated rules across it, and
 *  its states and marks. */
function LanePlot({
  box,
  compare,
  index,
  lane,
  model,
  place,
}: {
  box: LaneBox;
  compare: boolean;
  index: number;
  lane: Lane;
  model: LanesModel;
  place: Place;
}): ReactNode {
  const point = model.series[0]?.points[index];
  if (point === undefined) return null;
  const { at } = place;
  return (
    <svg
      // The lane's end names its newest reading's own words, as plain text: `eval-reading-note`
      aria-label={`${lane.row}${lane.end === null ? "" : `: ${lane.end.text}`}`}
      className="block w-full overflow-visible"
      data-lane={lane.row}
      height={box.height}
      role="img"
    >
      {model.lanes.grouped ? null : (
        <line className={chartPlot.grid} x1="0%" x2="100%" y1={box.middle} y2={box.middle} />
      )}
      {model.lanes.rules.map((rule) => (
        <line
          className={chartPlot.rule}
          key={`${rule.at}:${rule.label}`}
          strokeDasharray="3 3"
          x1={at(rule.at)}
          x2={at(rule.at)}
          y1={0}
          y2={box.height}
        />
      ))}
      {compare ? (
        <LaneMarks
          box={box}
          lane={lane.compare}
          model={model}
          place={place}
          point={point}
          range="compare"
        />
      ) : null}
      <LaneMarks
        box={box}
        lane={lane.window}
        model={model}
        place={place}
        point={point}
        range="window"
      />
    </svg>
  );
}

/**
 * Draws a `lanes` model (`lanesModel`): its mock-up line, its headers and the labels of its dated
 * rules, each lane it draws, its time axis, and the lines under it, in the wide form, or in the
 * narrow form where the wide one does not fit (`useNarrowForm`).
 */
export function LanesRenderer({ compare, model }: ChartRendererProps): ReactNode {
  const chart = useRef<HTMLDivElement>(null);
  const narrow = useNarrowForm(chart);
  if (!isLanesModel(model)) return null;
  const { lanes: plot } = model;
  const box: LaneBox = {
    bar: plot.pitch / 2,
    height: plot.pitch,
    middle: plot.pitch / 2,
    under: plot.pitch - 4,
  };
  const span = plot.to - plot.from;
  const share = (ms: number) => Math.min(1, Math.max(0, (ms - plot.from) / span)) * 100;
  const place: Place = {
    at: (ms) => `${share(ms).toFixed(3)}%`,
    share,
    width: (from, to) => `${(share(to) - share(from)).toFixed(3)}%`,
  };
  const { at } = place;
  const columns = plot.columns.map((column, index) => ({
    ...column,
    series: model.series[index + 1],
  }));
  // The narrow form stands each group's label in the column of names and each end text under its
  // lane, so it has no column of either.
  const grouped = plot.grouped && !narrow;
  const template = [
    ...(grouped ? ["minmax(0, max-content)"] : []),
    "minmax(0, max-content)",
    ...columns.map(() => "max-content"),
    narrow ? "minmax(9.375rem, 1fr)" : "minmax(10rem, 1fr)",
    ...(narrow ? [] : ["minmax(0, max-content)"]),
  ].join(" ");
  // The rules' labels in tiers, a label in the first tier where it meets no label before it.
  const tiers: number[] = [];
  const rules = [...plot.rules]
    .sort((a, b) => a.at - b.at)
    .map((rule) => {
      const left = share(rule.at);
      const width = Math.max(rule.label.length, rule.detail?.length ?? 0) * CHAR_PERCENT;
      let tier = tiers.findIndex((end) => end < left);
      if (tier === -1) tier = tiers.push(0) - 1;
      tiers[tier] = left + width;
      return { ...rule, tier };
    });
  // The narrow form heads only its row columns and its dated rules.
  const titled =
    !narrow && (plot.groupTitle !== null || plot.namesTitle !== null || plot.endTitle !== null);
  const headed = titled || columns.length > 0 || rules.length > 0;
  const head = `text-xs font-medium ${textSecondaryOnCanvas}`;
  const drawn = plot.lanes.slice(0, plot.drawn);
  const lineHeight = { minHeight: box.height };
  return (
    <div
      className="flex min-w-0 flex-col gap-1"
      data-lane-form={narrow ? "narrow" : "wide"}
      data-lanes
      ref={chart}
    >
      {plot.mockup === null ? null : (
        <p className={`text-xs font-medium ${textMutedOnSurface}`} data-mockup-line>
          {plot.mockup}
        </p>
      )}
      {drawn.length === 0 ? (
        plot.notes.length > 0 ? null : (
          <p className={`text-xs ${textMutedOnSurface}`} data-plot-empty>
            No reading of this chart's series in the window.
          </p>
        )
      ) : (
        <div className="relative min-w-0">
          <div className="grid text-xs" style={{ gridTemplateColumns: template }}>
            {headed ? (
              <>
                {grouped ? <span className={`${head} px-1`}>{plot.groupTitle ?? ""}</span> : null}
                <span className={`${head} self-end px-1`}>
                  {narrow ? "" : (plot.namesTitle ?? "")}
                </span>
                {columns.map((column) => (
                  <span
                    className={`${head} self-end px-1 ${column.field === "value" ? "text-right" : ""}`}
                    data-lane-column={column.series?.name}
                    key={column.label}
                  >
                    {column.label}
                  </span>
                ))}
                <svg
                  aria-hidden="true"
                  className="block w-full overflow-visible"
                  data-lane-rules
                  height={tiers.length * TIER}
                >
                  {rules.map((rule) => (
                    <g key={`${rule.at}:${rule.label}`}>
                      <text
                        className={chartPlot.text}
                        dx={3}
                        fontSize={10}
                        fontWeight={600}
                        x={at(rule.at)}
                        y={rule.tier * TIER + 10}
                      >
                        {rule.label}
                      </text>
                      {rule.detail === null ? null : (
                        <text
                          className={chartPlot.text}
                          dx={3}
                          fontSize={9}
                          x={at(rule.at)}
                          y={rule.tier * TIER + 21}
                        >
                          {rule.detail}
                        </text>
                      )}
                    </g>
                  ))}
                </svg>
                {narrow ? null : (
                  <span className={`${head} self-end px-1`}>{plot.endTitle ?? ""}</span>
                )}
              </>
            ) : null}
            {drawn.map((lane, index) => {
              const row = `flex items-center px-1 ${
                !narrow && lane.group !== null && index > 0 ? `border-t ${borderDefault}` : ""
              }`;
              const end = (
                <span
                  className={`${narrow ? "block text-right text-[11px]" : "flex items-center gap-1 px-1"} ${
                    lane.end === null ? textPrimaryOnSurface : STATE_TEXT[lane.end.state]
                  }`}
                  data-lane-end
                  data-state={lane.end?.state}
                  style={
                    narrow
                      ? // Under its lane's line, ending where its newest reading stands.
                        {
                          paddingRight: `${(100 - share(lane.window.spans.at(-1)?.to ?? plot.to)).toFixed(3)}%`,
                        }
                      : lineHeight
                  }
                >
                  {/* Its newest reading's own words, as plain text: `eval-reading-note` */}
                  {lane.end?.text}
                </span>
              );
              return (
                <div className="contents" data-lane-row={lane.row} key={lane.row}>
                  {grouped ? (
                    <span
                      className={`${row} max-w-[17.5rem] font-semibold ${textPrimaryOnSurface}`}
                      data-lane-group
                      style={lineHeight}
                    >
                      {lane.group ?? ""}
                    </span>
                  ) : null}
                  {narrow ? (
                    <span
                      className={`flex max-w-[17.5rem] flex-col justify-center px-1 ${textPrimaryOnSurface}`}
                      style={lineHeight}
                    >
                      {lane.group === null ? null : (
                        <span className="font-semibold" data-lane-group>
                          {lane.group}
                        </span>
                      )}
                      <span data-lane-name>{lane.row}</span>
                    </span>
                  ) : (
                    <span
                      className={`${row} max-w-[17.5rem] ${textPrimaryOnSurface}`}
                      data-lane-name
                      style={lineHeight}
                    >
                      {lane.row}
                    </span>
                  )}
                  {columns.map((column) => {
                    const point = column.series?.points[index];
                    const statistic = column.field;
                    const own = point?.values[statistic];
                    const then = point?.compare?.[statistic];
                    return (
                      <span
                        className={`${row} gap-1 tabular-nums ${textMutedOnSurface} ${
                          column.field === "value" ? "justify-end" : ""
                        }`}
                        key={column.label}
                        style={lineHeight}
                      >
                        {point === undefined || own === undefined ? null : (
                          <span
                            {...valueNames(
                              model,
                              column.series?.name ?? "",
                              statistic,
                              point,
                              "window"
                            )}
                          >
                            {own.state === "absent" ? column.emptyLabel : own.label}
                          </span>
                        )}
                        {compare &&
                        point !== undefined &&
                        then !== undefined &&
                        then.state !== "absent" ? (
                          <span className="text-[11px]" data-ghost>
                            {"was "}
                            <span
                              {...valueNames(
                                model,
                                column.series?.name ?? "",
                                statistic,
                                point,
                                "compare"
                              )}
                            >
                              {then.label}
                            </span>
                          </span>
                        ) : null}
                      </span>
                    );
                  })}
                  <span
                    className={narrow ? "flex flex-col justify-center px-1" : row}
                    data-lane-plot
                    style={lineHeight}
                  >
                    <LanePlot
                      box={box}
                      compare={compare}
                      index={index}
                      lane={lane}
                      model={model}
                      place={place}
                    />
                    {narrow ? end : null}
                  </span>
                  {narrow ? null : end}
                </div>
              );
            })}
            {grouped ? <span /> : null}
            <span />
            {columns.map((column) => (
              <span key={column.label} />
            ))}
            <svg
              aria-hidden="true"
              className="block w-full overflow-visible"
              data-lane-axis
              height={16}
            >
              <line className={chartPlot.axis} x1="0%" x2="100%" y1={0} y2={0} />
              {ticksOf(plot.from, plot.to, plot.weekly).map((tick) => (
                <g key={tick}>
                  <line className={chartPlot.axis} x1={at(tick)} x2={at(tick)} y1={0} y2={3} />
                  <text
                    className={chartPlot.text}
                    fontSize={10}
                    textAnchor="middle"
                    x={at(tick)}
                    y={13}
                  >
                    {dayLabel(tick)}
                  </text>
                </g>
              ))}
            </svg>
            {narrow ? null : <span />}
            {plot.xTitle === null ? null : (
              <p
                className={`text-center text-[10px] ${textMutedOnSurface}`}
                data-lane-axis-title
                style={{ gridColumn: "1 / -1" }}
              >
                {plot.xTitle}
              </p>
            )}
          </div>
          {plot.mockup === null ? null : (
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
