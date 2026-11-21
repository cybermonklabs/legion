// The prototype's web/src/components/Sparkbars.tsx, ported as it is; its fills are the `chartBar`
// tokens, since no component writes a colour utility of its own. Two differences: where an eval
// may judge the chart (`judged`), each bar takes its own verdict, its day's value of the statistic
// it draws in its target's direction and inclusiveness, and none where the target does not judge
// that statistic of a day (`lib/target.ts`'s `ownVerdict`), and the dashed line is drawn exactly
// where the bars take one (`targetLine`), rather than the prototype's comparison here of each bar
// against the line; and a toned bar's title, the one figure it writes, writes its value on its
// value's side of the target, by the verdict the bar takes (`judgeFigure`, dispatch://INFRA-2916,
// R3-C1).
import { chartBar } from "../../theme/classes";
import { barScale } from "./lib/barScale";
import { type Judge, judgeFigure, type Target, targetLine, type WriteFigure } from "./lib/target";

export interface SparkbarPoint {
  label: string; // tooltip prefix, e.g. "2026-09-27"
  value: number | null; // null: nothing to measure that day
  partial: boolean;
}

interface Props {
  points: SparkbarPoint[];
  // Where an eval may judge the bars: its slug, the statistic each bar draws a day's value of, and
  // its target, which every bar names (`data-eval`, `data-statistic`, with `data-span="day"`) and
  // is judged by. Omitted on a chart no eval judges, whose bars read neutral under no line.
  judged?: { evalSlug: string; statistic: Judge; target: Target };
  // Top of the scale, which the target line and the tallest bar may raise. Omitted, the target
  // line where one is drawn and 1 where none is: a count's chart, scaled to its target.
  max?: number;
  // How a bar's title writes a day's value: its usual digits, and the more a toned bar takes where
  // those would put it on the other side of the target from its verdict (`judgeFigure`).
  write: (value: number) => WriteFigure;
}

const BAR = 10;
const HEIGHT = 28;

/** One bar per day with a dashed target line where the bars take a tone: a green bar's value meets
 * the target, a red bar's misses it, a neutral bar has no verdict, and faded bars are days the
 * window only partly covers. Plain SVG, no chart library, so it costs nothing on a filter change. */
export function Sparkbars({ points, judged, max, write }: Props) {
  const line = judged === undefined ? undefined : targetLine(judged.target, [judged.statistic]);
  const y = barScale(
    max ?? line ?? 1,
    line,
    points.map((p) => p.value),
    HEIGHT
  );
  return (
    <svg
      viewBox={`0 0 ${points.length * BAR} ${HEIGHT}`}
      preserveAspectRatio="none"
      className="w-full h-7 block"
      role="img"
      aria-label="per-day trend"
    >
      {points.map((p, i) => {
        // Where no eval judges the chart (a null target), judgeFigure gives no verdict and writes
        // the value's usual digits.
        const { met, text } =
          p.value === null
            ? { met: null, text: "no runs" }
            : judgeFigure(
                judged?.target ?? null,
                judged?.statistic ?? "",
                "day",
                p.value,
                write(p.value)
              );
        return (
          <g key={p.label} opacity={p.partial ? 0.4 : 1}>
            <title>{`${p.label}${p.partial ? " (partial day)" : ""}: ${text}`}</title>
            <rect x={i * BAR} y={0} width={BAR} height={HEIGHT} fill="transparent" />
            {p.value === null ? (
              <rect
                x={i * BAR + 1}
                y={HEIGHT - 1}
                width={BAR - 2}
                height={1}
                className={chartBar.empty}
              />
            ) : (
              <rect
                x={i * BAR + 1}
                y={y(p.value)}
                width={BAR - 2}
                height={Math.max(HEIGHT - y(p.value), 1)}
                className={met === null ? chartBar.neutral : met ? chartBar.met : chartBar.missed}
                data-eval={judged?.evalSlug}
                data-span={judged === undefined ? undefined : "day"}
                data-statistic={judged?.statistic}
              />
            )}
          </g>
        );
      })}
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
    </svg>
  );
}
