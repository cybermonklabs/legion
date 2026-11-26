// The prototype's web/src/components/SpreadBars.tsx, ported as it is; its fills are the
// `chartBar` tokens, since no component writes a colour utility of its own. Two differences: where
// a lead-time eval may judge the chart (`judged`), each of a day's two bars takes its own verdict,
// the median bar by the day's median and the p90 bar by its p90, in the target's direction and
// inclusiveness, and none where the target does not judge that statistic (`lib/target.ts`'s
// `ownVerdict`), and the dashed line is drawn exactly where the bars take one (`targetLine`),
// rather than the prototype's strict "under the target" of each bar's figure, since a person may
// make a target inclusive, turn its direction, and judge one of the two statistics and not the
// other; and a day's title writes each toned bar's value on its value's side of the target, by the
// verdict that bar takes (`judgeFigure`, dispatch://INFRA-2916, R3-C1).
import { chartBar } from "../../theme/classes";
import { barScale } from "./lib/barScale";
import { type Judge, judgeFigure, type Target, targetLine, type WriteFigure } from "./lib/target";

export interface SpreadBarDay {
  day: string; // YYYY-MM-DD, the tooltip's prefix
  n: number;
  median_minutes: number | null;
  p90_minutes: number | null;
  partial: boolean;
}

const MEDIAN: Judge = "minutes.median";
const P90: Judge = "minutes.p90";

/** The statistics a judged chart's bars draw, a day's value of each: a lead time's median and its
 *  p90. A lead-time card's caption names its dashed line where `targetLine` draws one over them. */
export const SPREAD_STATISTICS: readonly Judge[] = [MEDIAN, P90];

interface Props {
  days: readonly SpreadBarDay[];
  // Where a lead-time eval may judge the bars: its slug and its target, which each bar names
  // (`data-eval`, and `data-statistic` `minutes.median` or `minutes.p90`, with
  // `data-span="day"`) and is judged by. Omitted on a chart no eval judges, whose bars read
  // neutral under no line.
  judged?: { evalSlug: string; target: Target };
  noun: string; // what `n` counts, "runs"
  // How a day's title writes a bar's minutes: their usual digits, and the more a toned bar takes
  // where those would put its value on the other side of the target from its verdict
  // (`judgeFigure`).
  write: (minutes: number) => WriteFigure;
}

const BAR = 12;
const HEIGHT = 36;

/** One bar per day: the p90 drawn faint behind the median. Where an eval judges the chart, a dashed
 * line marks its target where the bars take a tone, and each bar is green where its own value
 * meets the target, red where it misses it and neutral where it has no verdict, so a day's two
 * bars may differ. Today's partial day is faded. Plain SVG, like `Sparkbars`. */
export function SpreadBars({ days, judged, noun, write }: Props) {
  const line = judged === undefined ? undefined : targetLine(judged.target, SPREAD_STATISTICS);
  const y = barScale(
    1,
    line,
    days.map((d) => d.p90_minutes),
    HEIGHT
  );
  // A day's value of statistic, null where it has none: its minutes, the verdict its bar takes and
  // its title's text of them, which judgeFigure writes in the usual digits under no verdict where
  // no eval judges the chart (a null target).
  const figure = (statistic: Judge, minutes: number | null) => {
    if (minutes === null) return null;
    const target = judged?.target ?? null;
    return { minutes, ...judgeFigure(target, statistic, "day", minutes, write(minutes)) };
  };
  return (
    <svg
      viewBox={`0 0 ${days.length * BAR} ${HEIGHT}`}
      preserveAspectRatio="none"
      className="block h-9 w-full"
      role="img"
      aria-label="per-day median and p90"
    >
      {days.map((d, i) => {
        const median = figure(MEDIAN, d.median_minutes);
        const p90 = figure(P90, d.p90_minutes);
        return (
          <g key={d.day} opacity={d.partial ? 0.55 : 1}>
            <title>
              {`${d.day}${d.partial ? " (today so far)" : ""}: ${
                median === null || p90 === null
                  ? `no ${noun}`
                  : `median ${median.text}, p90 ${p90.text}, ${d.n} ${noun}`
              }`}
            </title>
            <rect x={i * BAR} y={0} width={BAR} height={HEIGHT} fill="transparent" />
            {median === null || p90 === null ? (
              <rect
                x={i * BAR + 1}
                y={HEIGHT - 1}
                width={BAR - 2}
                height={1}
                className={chartBar.empty}
              />
            ) : (
              <>
                <rect
                  x={i * BAR + 1}
                  y={y(p90.minutes)}
                  width={BAR - 2}
                  height={Math.max(HEIGHT - y(p90.minutes), 1)}
                  className={
                    p90.met === null
                      ? chartBar.neutralFaint
                      : p90.met
                        ? chartBar.metFaint
                        : chartBar.missedFaint
                  }
                  data-eval={judged?.evalSlug}
                  data-span={judged === undefined ? undefined : "day"}
                  data-statistic={judged === undefined ? undefined : P90}
                />
                <rect
                  x={i * BAR + 1}
                  y={y(median.minutes)}
                  width={BAR - 2}
                  height={Math.max(HEIGHT - y(median.minutes), 1)}
                  className={
                    median.met === null
                      ? chartBar.neutral
                      : median.met
                        ? chartBar.met
                        : chartBar.missed
                  }
                  data-eval={judged?.evalSlug}
                  data-span={judged === undefined ? undefined : "day"}
                  data-statistic={judged === undefined ? undefined : MEDIAN}
                />
              </>
            )}
          </g>
        );
      })}
      {line !== undefined && (
        <line
          x1={0}
          x2={days.length * BAR}
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
