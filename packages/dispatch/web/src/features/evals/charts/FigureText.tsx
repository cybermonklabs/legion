// One figure that a declared `number` or `text` prints, named as the chart's table names it
// (`data-chart`, `data-series`, `data-statistic`, `data-x`, `data-range`, `data-value`), and the
// compare range's value beside it with the change from it (dispatch://INFRA-2918/artifact/plan-md,
// D10: number and text print each value's compare value and change beside it).
import type { ReactNode } from "react";

import { textMutedOnSurface } from "../../../theme/classes";
import type { ChartPoint } from "../chart-model";

/** Which of a point's figures: its own, the compare range's, or the change between them. */
export type FigureRange = "window" | "compare" | "change";

/** Where a figure stands: its chart, its series in the model and its point. */
export interface FigurePlace {
  readonly chart: string;
  readonly series: string;
  readonly point: ChartPoint;
}

/** The point's figure of `statistic` in `range`; nothing where the point has none. */
export function FigureText(props: {
  className?: string;
  place: FigurePlace;
  range: FigureRange;
  statistic: string;
}): ReactNode {
  const { point } = props.place;
  const { range } = props;
  const figures =
    range === "window" ? point.values : range === "compare" ? point.compare : point.change;
  const value = figures?.[props.statistic];
  if (value === undefined) return null;
  return (
    <span
      className={`${value.state === "absent" ? textMutedOnSurface : ""} ${props.className ?? ""}`}
      data-chart={props.place.chart}
      data-range={range}
      data-series={props.place.series}
      data-statistic={props.statistic}
      data-value={value.value === null ? undefined : String(value.value)}
      data-x={point.x}
    >
      {value.label}
    </span>
  );
}

/** While the compare range is shown, "vs" its value of `statistic`, and the change from it where
 *  both have one. */
export function ComparedText(props: {
  compare: boolean;
  place: FigurePlace;
  statistic: string;
}): ReactNode {
  const { place, statistic } = props;
  const { point } = place;
  if (!props.compare || point.compare === null || point.compare[statistic] === undefined) {
    return null;
  }
  const change = point.change?.[statistic];
  return (
    <span className={`text-xs ${textMutedOnSurface}`} data-compared={point.x}>
      {"vs "}
      <FigureText place={place} range="compare" statistic={statistic} />
      {change === undefined || change.value === null ? null : (
        <>
          {" ("}
          <FigureText place={place} range="change" statistic={statistic} />
          {")"}
        </>
      )}
    </span>
  );
}
