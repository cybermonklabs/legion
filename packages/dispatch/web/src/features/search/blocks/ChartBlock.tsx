import type { ReactNode } from "react";
import { useEffect, useMemo, useRef, useState } from "react";

import { borderDefault } from "../../../theme/classes";
import { useMediaQuery } from "../../shell/useDialog";
import { type BlockOf, cellNumber, cellText } from "./block-cells";
import { blockCellClassName, blockHeadClassName } from "./TableBlock";

// echarts is loaded here as the delivery timeline loads it (features/delivery/Timeline.tsx): by
// the one dynamic import, so its chunk arrives with the first chart an answer draws and nothing
// the shell loads earlier pulls it in. The bar is part of the shell; a static import here would
// put echarts in the entry chunk every page pays for.

/** The chart draws on a canvas, so its colours are values: the delivery timeline's axis colours,
 *  and one series colour that reads on either scheme. */
const CHART_COLORS = {
  dark: { axis: "#334155", label: "#94a3b8", split: "#1e293b" },
  light: { axis: "#cbd5e1", label: "#64748b", split: "#e2e8f0" },
};
const SERIES_COLOR = "#0284c7";

/**
 * A chart: one series, the spec's `x` along the axis and its `y` plotted, as bars or a line. Every
 * point is a row the server's read returned, and the same rows are written out beneath the canvas
 * as a table a screen reader reads, so the numbers drawn are the numbers a reader can check.
 * Nothing of the chart is stored: it is drawn from the answer's own block frame, and drawn again
 * from the same frame when the run is replayed.
 */
export function ChartBlock({ block }: { readonly block: BlockOf<"chart"> }): ReactNode {
  const container = useRef<HTMLDivElement>(null);
  const [drawn, setDrawn] = useState(false);
  const dark = useMediaQuery("(prefers-color-scheme: dark)");
  const palette = dark ? CHART_COLORS.dark : CHART_COLORS.light;
  const { mark, x, y } = block.spec;
  const points = useMemo(
    () =>
      block.rows.flatMap((row) => {
        const value = cellNumber(row[y]);
        return value === undefined ? [] : [{ label: cellText(row[x]), value }];
      }),
    [block.rows, x, y]
  );

  useEffect(() => {
    let disposed = false;
    let cleanup: (() => void) | undefined;
    void import("echarts").then((echarts) => {
      const element = container.current;
      if (disposed || element === null) {
        return;
      }
      const chart = echarts.init(element, undefined, { renderer: "canvas" });
      chart.setOption({
        animation: false,
        grid: { bottom: 8, containLabel: true, left: 8, right: 16, top: 16 },
        series: [
          {
            data: points.map((point) => point.value),
            itemStyle: { color: SERIES_COLOR },
            lineStyle: { color: SERIES_COLOR },
            type: mark,
          },
        ],
        tooltip: { trigger: "axis" },
        xAxis: {
          axisLabel: { color: palette.label },
          axisLine: { lineStyle: { color: palette.axis } },
          data: points.map((point) => point.label),
          type: "category",
        },
        yAxis: {
          axisLabel: { color: palette.label },
          splitLine: { lineStyle: { color: palette.split } },
          type: "value",
        },
      });
      const observer = new ResizeObserver(() => chart.resize());
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
  }, [mark, palette, points]);

  return (
    <div>
      <div
        aria-hidden="true"
        className={`h-48 w-full rounded border ${borderDefault}`}
        data-chart-drawn={drawn ? "true" : "false"}
        data-testid="going-block-chart"
        ref={container}
      />
      <table className="sr-only" data-testid="going-block-chart-data">
        <caption>{block.title === "" ? `${y} by ${x}` : block.title}</caption>
        <thead>
          <tr>
            <th className={blockHeadClassName} scope="col">
              {x}
            </th>
            <th className={blockHeadClassName} scope="col">
              {y}
            </th>
          </tr>
        </thead>
        <tbody>
          {points.map((point, index) => (
            <tr
              data-testid="going-block-chart-point"
              // biome-ignore lint/suspicious/noArrayIndexKey: a block's rows never reorder.
              key={index}
            >
              <td className={blockCellClassName}>{point.label}</td>
              <td className={blockCellClassName}>{point.value}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
