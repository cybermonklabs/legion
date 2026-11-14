import type { ReactNode } from "react";
import { dangerText, successText, textMutedOnCanvas } from "../theme/classes";

/** Which side of its target a reading has to be on to meet it: a score is held to a floor, a
 *  cost or a failure rate to a ceiling. */
export type TargetDirection = "atLeast" | "atMost";

export interface TargetCellProps {
  /** The reading, or nothing where the row has none: a measure with no result yet reads as
   *  `empty` and never as zero. */
  readonly value: number | undefined;
  /** What the reading is held to. */
  readonly target: number;
  /** How a reading, a target and a difference are written — a percentage, a count, a duration.
   *  One function for all three, so the difference is written in the units the reading is.
   *  Without it they read as the numbers they are. */
  readonly format?: (value: number) => string;
  /** Whether meeting the target means reaching it (`atLeast`, the default) or staying under it
   *  (`atMost`). */
  readonly direction?: TargetDirection;
  /** What the cell says where there is no reading. */
  readonly empty?: ReactNode;
}

/**
 * A row's reading against the target it is held to, with the difference between them: the
 * reading in the colour of its verdict, the target beside it, and how far above or below it the
 * reading is. The verdict is the caller's rule, not a sign: a reading under a ceiling has met
 * its target as surely as one over a floor.
 */
export function TargetCell({
  direction = "atLeast",
  empty = "no data",
  format,
  target,
  value,
}: TargetCellProps): ReactNode {
  if (value === undefined) return <span className={textMutedOnCanvas}>{empty}</span>;
  const show = format ?? ((reading: number) => String(reading));
  const difference = value - target;
  const met = direction === "atMost" ? difference <= 0 : difference >= 0;
  const verdict = met ? successText : dangerText;
  const sign = difference > 0 ? "+" : difference < 0 ? "-" : "±";
  return (
    <span className="flex flex-wrap items-baseline gap-x-2 tabular-nums">
      <span className={verdict}>{show(value)}</span>
      <span className={`text-xs ${textMutedOnCanvas}`}>of {show(target)}</span>
      <span className={`text-xs ${verdict}`}>
        {sign}
        {show(Math.abs(difference))}
      </span>
    </span>
  );
}
