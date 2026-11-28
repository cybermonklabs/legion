// The prototype's web/src/components/Sparkbars.tsx, ported; its fills are the `chartBar` tokens,
// since no component writes a colour utility of its own. Where an eval may judge the chart
// (`judged`), each bar takes its own verdict, its day's value of the statistic it draws in its
// target's direction and inclusiveness, and none where the target does not judge that statistic of
// a day (`lib/target.ts`'s `ownVerdict`), and the dashed line is drawn exactly where the bars take
// one (`targetLine`), rather than the prototype's comparison here of each bar against the line;
// and a toned bar's title, the one figure it writes, writes its value on its value's side of the
// target, by the verdict the bar takes (`judgeFigure`, dispatch://INFRA-2916, R3-C1). The Evals
// tab's charts (`features/evals/EvalChart.tsx`) draw through it too, naming the values it draws in
// the DOM (`marks`) and drawing the compare range as a muted second series (`compare`,
// dispatch://INFRA-2918/artifact/plan-md, D4 and D6).
//
// The bars of one fill are one `<path>` (dispatch://INFRA-2918, T33): a chart of 84 days with a
// compare range draws about ten nodes rather than six a day. So no node stands for one day: the
// values a range draws are named together on the group that draws them (`valueList`), and the day
// under the pointer is read from the chart's own points (`usePlaceUnderPointer`), its title written
// as each bar's was.
import { type PointerEvent, useState } from "react";

import { chartBar } from "../../theme/classes";
import { barScale } from "./lib/barScale";
import { type Judge, judgeFigure, type Target, targetLine, type WriteFigure } from "./lib/target";

export interface SparkbarPoint {
  label: string; // tooltip prefix, e.g. "2026-09-27"
  value: number | null; // null: nothing to measure that day
  partial: boolean;
}

/** What an eval chart names of the values it draws (dispatch://INFRA-2918/artifact/plan-md, D4):
 *  the chart, the series, and each point's place on the axis (`x`, one per point), so the values
 *  a chart draws carry `data-chart`, `data-series`, `data-statistic`, `data-range` and their
 *  places and values, which the Evals tab's checks read without a canvas. `statistic` is the
 *  statistic a one-figure chart draws; a chart of two names each of its own. */
export interface ValueMarks {
  readonly chart: string;
  readonly series: string;
  readonly statistic: string;
  readonly x: readonly string[];
}

/** Which range a drawn value is of: the window, or the compare range drawn beside it. */
export type ValueRange = "window" | "compare";

/** The DOM attributes of the value at `index` (`ValueMarks`), none where no marks are given: the
 *  attributes of a node that draws one value, `data-x` its place and `data-value` the value. */
export function valueMarks(
  marks: ValueMarks | undefined,
  index: number,
  value: number,
  range: ValueRange,
  statistic = marks?.statistic
): Record<string, string> {
  if (marks === undefined) return {};
  return {
    "data-chart": marks.chart,
    "data-range": range,
    "data-series": marks.series,
    "data-statistic": statistic ?? "",
    "data-value": String(value),
    "data-x": marks.x[index] ?? "",
  };
}

/** The DOM attributes of every value of one range of a statistic (`ValueMarks`), on the one element
 *  that draws them all, none where no marks are given: `data-xs` the places that hold a value and
 *  `data-values` their values, each list in the axis's order and joined by spaces, so the n-th
 *  value is at the n-th place. */
export function valueList(
  marks: ValueMarks | undefined,
  values: readonly (number | null)[],
  range: ValueRange,
  statistic = marks?.statistic
): Record<string, string> {
  if (marks === undefined) return {};
  const places: string[] = [];
  const drawn: string[] = [];
  values.forEach((value, index) => {
    if (value === null) return;
    places.push(marks.x[index] ?? "");
    drawn.push(String(value));
  });
  return {
    "data-chart": marks.chart,
    "data-range": range,
    "data-series": marks.series,
    "data-statistic": statistic ?? "",
    "data-values": drawn.join(" "),
    "data-xs": places.join(" "),
  };
}

/** A number of a path, to the hundredth: a chart's shapes need no finer place. */
function at(value: number): number {
  return Math.round(value * 100) / 100;
}

/** One bar as a subpath of a `<path>`: its top left corner, its width, and down to the chart's
 *  foot (`height` tall), so the bars of one fill draw as one path. */
export function barPath(x: number, y: number, width: number, height: number): string {
  return `M${at(x)} ${at(y)}h${at(width)}v${at(height)}h${at(-width)}z`;
}

/**
 * The place under the pointer of a chart of `count` places side by side across its whole width,
 * read from where the pointer is over the chart's element and its width as drawn, so no node
 * stands for a place: `place` is null while the pointer is not over the chart, and `onPointerMove`
 * and `onPointerLeave` are the chart's element's.
 */
export function usePlaceUnderPointer(count: number): {
  place: number | null;
  onPointerMove: (event: PointerEvent<Element>) => void;
  onPointerLeave: () => void;
} {
  const [place, setPlace] = useState<number | null>(null);
  return {
    onPointerLeave: () => setPlace(null),
    onPointerMove: (event) => {
      const box = event.currentTarget.getBoundingClientRect();
      if (box.width <= 0 || count === 0) return;
      const index = Math.floor(((event.clientX - box.left) / box.width) * count);
      setPlace(Math.min(count - 1, Math.max(0, index)));
    },
    place,
  };
}

interface Props {
  points: SparkbarPoint[];
  // Where an eval may judge the bars: its slug, the statistic each bar draws a day's value of, and
  // its target, which the bars of each verdict name (`data-eval`, `data-statistic`, with
  // `data-span="day"`) and are judged by. Omitted on a chart no eval judges, whose bars read
  // neutral under no line.
  judged?: { evalSlug: string; statistic: Judge; target: Target };
  // Top of the scale, which the target line and the tallest bar may raise. Omitted, the target
  // line where one is drawn and 1 where none is: a count's chart, scaled to its target.
  max?: number;
  // How a bar's title writes a day's value: its usual digits, and the more a toned bar takes where
  // those would put it on the other side of the target from its verdict (`judgeFigure`).
  write: (value: number) => WriteFigure;
  // An eval chart's names for the values it draws (`ValueMarks`). Omitted, the bars name none.
  marks?: ValueMarks;
  // The compare range's value at each point's place, drawn as a muted bar behind it, null where
  // the compare range has none. Omitted, no compare range is drawn; given with no value, its
  // series is drawn empty.
  compare?: readonly (number | null)[];
  // Whether the bars are drawn; false draws the empty chart at its size, while the Evals tab holds
  // the section's charts back (`ChartRendererProps.drawn`). Omitted, drawn.
  drawn?: boolean;
}

const BAR = 10;
const HEIGHT = 28;

/** What fills a window's bar: its verdict's tone, or the stub of a day with nothing to measure. */
type Fill = "met" | "missed" | "neutral" | "empty";

/** One bar per day with a dashed target line where the bars take a tone: a green bar's value meets
 * the target, a red bar's misses it, a neutral bar has no verdict, and faded bars are days the
 * window only partly covers. A compare range draws a muted bar behind each day, the window's bar
 * narrowed in front of it. Plain SVG, no chart library, so it costs nothing on a filter change;
 * the bars of one fill and fade are one path, and the day under the pointer is the chart's title. */
export function Sparkbars({ points, judged, max, write, marks, compare, drawn = true }: Props) {
  const line = judged === undefined ? undefined : targetLine(judged.target, [judged.statistic]);
  const y = barScale(
    max ?? line ?? 1,
    line,
    [...points.map((p) => p.value), ...(compare ?? [])],
    HEIGHT
  );
  const hover = usePlaceUnderPointer(points.length);
  // Narrowed in front of a compare bar, so the muted bar behind it shows on both sides.
  const inset = compare === undefined ? 1 : 3;
  // Where no eval judges the chart (a null target), judgeFigure gives no verdict and writes the
  // value's usual digits.
  const figure = (p: SparkbarPoint) =>
    p.value === null
      ? { met: null, text: "no runs" }
      : judgeFigure(
          judged?.target ?? null,
          judged?.statistic ?? "",
          "day",
          p.value,
          write(p.value)
        );
  const titleOf = (i: number) => {
    const p = points[i];
    if (p === undefined) return "";
    const { text } = figure(p);
    const compared = compare?.[i];
    const versus =
      compared === undefined
        ? ""
        : `; compare ${compared === null ? "no data" : write(compared)(0)}`;
    return `${p.label}${p.partial ? " (partial day)" : ""}: ${text}${versus}`;
  };
  // The window's bars by fill and fade, each set one path.
  const paths = new Map<string, { fill: Fill; partial: boolean; d: string }>();
  if (drawn)
    points.forEach((p, i) => {
      const { met } = figure(p);
      const fill: Fill =
        p.value === null ? "empty" : met === null ? "neutral" : met ? "met" : "missed";
      const d =
        p.value === null
          ? barPath(i * BAR + 1, HEIGHT - 1, BAR - 2, 1)
          : barPath(i * BAR + inset, y(p.value), BAR - 2 * inset, Math.max(HEIGHT - y(p.value), 1));
      const key = `${fill}:${p.partial}`;
      const set = paths.get(key);
      if (set === undefined) paths.set(key, { d, fill, partial: p.partial });
      else set.d += d;
    });
  return (
    <svg
      viewBox={`0 0 ${points.length * BAR} ${HEIGHT}`}
      preserveAspectRatio="none"
      className="w-full h-7 block"
      role="img"
      aria-label="per-day trend"
      onPointerLeave={hover.onPointerLeave}
      onPointerMove={hover.onPointerMove}
    >
      {!drawn ? null : (
        <>
          {hover.place === null ? null : <title>{titleOf(hover.place)}</title>}
          {compare === undefined ? null : (
            <g data-series-range="compare" {...valueList(marks, compare, "compare")}>
              <path
                className={chartBar.compare}
                d={compare
                  .slice(0, points.length)
                  .map((value, i) =>
                    value === null
                      ? ""
                      : barPath(i * BAR + 1, y(value), BAR - 2, Math.max(HEIGHT - y(value), 1))
                  )
                  .join("")}
              />
            </g>
          )}
          <g
            data-series-range="window"
            {...valueList(
              marks,
              points.map((p) => p.value),
              "window"
            )}
          >
            {[...paths.values()].map(({ d, fill, partial }) => (
              <path
                className={chartBar[fill]}
                d={d}
                data-eval={fill === "empty" ? undefined : judged?.evalSlug}
                data-span={fill === "empty" || judged === undefined ? undefined : "day"}
                data-statistic={fill === "empty" ? undefined : judged?.statistic}
                data-tone={fill}
                key={`${fill}:${partial}`}
                opacity={partial ? 0.4 : undefined}
              />
            ))}
          </g>
          {line !== undefined && (
            <line
              x1={0}
              x2={points.length * BAR}
              y1={y(line)}
              y2={y(line)}
              className={chartBar.targetLine}
              strokeDasharray="3 2"
              vectorEffect="non-scaling-stroke"
            />
          )}
        </>
      )}
    </svg>
  );
}
