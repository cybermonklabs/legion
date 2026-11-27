import type { ReactNode } from "react";
import { useEffect, useMemo, useRef, useState } from "react";

import {
  TableBody,
  TableCell,
  TableHead,
  TableHeadCell,
  TableRow,
  TableShell,
} from "../../../components/TableShell";
import { CHART_AXIS_COLORS } from "../../../theme/chart-colors";
import { borderDefault } from "../../../theme/classes";
import { issueStatuses } from "../../project/board-model";
import { useMediaQuery } from "../../shell/useDialog";
import { type BlockOf, cellNumber, columnText, columnWords } from "./block-cells";

// echarts is loaded here as the delivery timeline loads it (features/delivery/Timeline.tsx): by
// the one dynamic import, so its chunk arrives with the first chart an answer draws and nothing
// the shell loads earlier pulls it in. The bar is part of the shell; a static import here would
// put echarts in the entry chunk every page pays for.

/** The chart draws on a canvas, so its colours are values: the axis colours every chart shares,
 *  and one series colour that reads on either scheme. */
const SERIES_COLOR = "#0284c7";

/** One point of a chart: its place along the axis in Dispatch's words, and its value. */
export interface ChartPoint {
  readonly label: string;
  readonly value: number;
}

/** Where a status's column stands on the board (`issueStatuses`, the board's columns in their
 *  order); a status the board has no column for stands after them all. */
function boardPlace(status: unknown): number {
  const place =
    typeof status === "string" ? (issueStatuses as readonly string[]).indexOf(status) : -1;
  return place === -1 ? issueStatuses.length : place;
}

/** The points of a chart's block: one for each row with a number in the spec's `y`, its `x` in
 *  Dispatch's words ("In progress", "P1"), as `columnText` writes a table's cell. A chart along
 *  the status puts its places in the order the board puts its columns, whatever order the read
 *  answered its groups in; along any other field, in the read's order. */
export function chartPoints(block: BlockOf<"chart">): readonly ChartPoint[] {
  const { x, y } = block.spec;
  const rows =
    x === "status"
      ? [...block.rows].sort((one, other) => boardPlace(one[x]) - boardPlace(other[x]))
      : block.rows;
  return rows.flatMap((row) => {
    const value = cellNumber(row[y]);
    return value === undefined ? [] : [{ label: columnText(x, row[x]), value }];
  });
}

/** The room, in pixels, that the chart's grid and its value axis's labels take from its width;
 *  the places along the category axis share what is left. */
const AXIS_ROOM = 64;

/** The narrowest place along the axis, in pixels, whose label still sits level under it, wrapped
 *  at its words to that place's width. A narrower place slants its label instead: a label broken
 *  into a letter or two a line reads as nothing. */
const LEVEL_LABEL_MIN = 48;

/** How long a slanted label runs, in pixels, before it is cut with an ellipsis. */
const SLANTED_LABEL_MAX = 96;

/**
 * The echarts option of a chart (design v2, state 5): one series of `points` drawn as `mark`,
 * named as the spec's `y` field is headed, so hovering a bar or a point shows a tooltip with that
 * point's place and its value; each bar or point labelled with its value above it, as the
 * products the design follows label theirs, so the numbers read without a hover; the axes in
 * `palette`'s colours, and no animation. Every place along the axis shows its label, and no two
 * collide: each sits level, wrapped at its words to the width of its place in a chart `width`
 * pixels wide, or slants where that place is too narrow for a word.
 */
export function chartOption(
  points: readonly ChartPoint[],
  mark: BlockOf<"chart">["spec"]["mark"],
  name: string,
  palette: { readonly axis: string; readonly label: string; readonly split: string },
  width: number
): Record<string, unknown> {
  const place = Math.floor((width - AXIS_ROOM) / Math.max(points.length, 1));
  const fit =
    place >= LEVEL_LABEL_MIN
      ? { overflow: "break", width: place - 8 }
      : { overflow: "truncate", rotate: 45, width: SLANTED_LABEL_MAX };
  return {
    animation: false,
    // The top of the grid leaves room for the label above the tallest bar.
    grid: { bottom: 8, containLabel: true, left: 8, right: 16, top: 24 },
    series: [
      {
        data: points.map((point) => point.value),
        itemStyle: { color: SERIES_COLOR },
        label: { color: palette.label, position: "top", show: true },
        lineStyle: { color: SERIES_COLOR },
        name,
        type: mark,
      },
    ],
    tooltip: { trigger: "axis" },
    xAxis: {
      axisLabel: { color: palette.label, hideOverlap: false, interval: 0, ...fit },
      axisLine: { lineStyle: { color: palette.axis } },
      data: points.map((point) => point.label),
      type: "category",
    },
    yAxis: {
      axisLabel: { color: palette.label },
      splitLine: { lineStyle: { color: palette.split } },
      type: "value",
    },
  };
}

/**
 * A chart: one series, the spec's `x` along the axis and its `y` plotted, as bars or a line, each
 * place in Dispatch's words, every one labelled at the width the chart has, and again as that
 * width changes; hovering a bar or a point shows its value. Every point is a row the server's
 * read returned, and the same rows are written out beneath the canvas as a table a screen reader
 * reads, so the numbers drawn are the numbers a reader can check. Nothing of the chart is stored:
 * it is drawn from the answer's own block frame, and drawn again from the same frame when the run
 * is replayed.
 */
export function ChartBlock({ block }: { readonly block: BlockOf<"chart"> }): ReactNode {
  const container = useRef<HTMLDivElement>(null);
  const [drawn, setDrawn] = useState(false);
  const dark = useMediaQuery("(prefers-color-scheme: dark)");
  const palette = dark ? CHART_AXIS_COLORS.dark : CHART_AXIS_COLORS.light;
  const { mark, x, y } = block.spec;
  const points = useMemo(() => chartPoints(block), [block]);
  const xWords = columnWords(x);
  const yWords = columnWords(y);

  useEffect(() => {
    let disposed = false;
    let cleanup: (() => void) | undefined;
    void import("echarts").then((echarts) => {
      const element = container.current;
      if (disposed || element === null) {
        return;
      }
      const chart = echarts.init(element, undefined, { renderer: "canvas" });
      chart.setOption(chartOption(points, mark, yWords, palette, element.clientWidth));
      const observer = new ResizeObserver(() => {
        chart.resize();
        chart.setOption(chartOption(points, mark, yWords, palette, element.clientWidth));
      });
      observer.observe(element);
      setDrawn(true);
      cleanup = () => {
        observer.disconnect();
        chart.dispose();
      };
    });
    return () => {
      disposed = true;
      cleanup?.();
      setDrawn(false);
    };
  }, [mark, palette, points, yWords]);

  return (
    <div>
      <div
        aria-hidden="true"
        className={`h-48 w-full rounded border ${borderDefault}`}
        data-chart-drawn={drawn ? "true" : "false"}
        data-testid="going-block-chart"
        ref={container}
      />
      {/* The chart's points for a reader who cannot see it, in the shared table, named by what
          the chart counts. */}
      <div className="sr-only" data-testid="going-block-chart-data">
        <TableShell
          label={block.title === "" ? `${yWords} by ${xWords}` : block.title}
          size="compact"
        >
          <TableHead>
            <TableHeadCell>{xWords}</TableHeadCell>
            <TableHeadCell>{yWords}</TableHeadCell>
          </TableHead>
          <TableBody>
            {points.map((point, index) => (
              <TableRow
                data-testid="going-block-chart-point"
                // biome-ignore lint/suspicious/noArrayIndexKey: a block's rows never reorder.
                key={index}
              >
                <TableCell>{point.label}</TableCell>
                <TableCell>{point.value}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </TableShell>
      </div>
    </div>
  );
}
