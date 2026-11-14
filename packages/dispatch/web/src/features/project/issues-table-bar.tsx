import { type ReactNode, useRef } from "react";
import {
  borderDefault,
  slotUseFill,
  stageFill,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  tableFilterActive,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import {
  NOT_STARTED_STAGES,
  SLOT_USES,
  STAGES,
  slotUseLabel,
  WAITING_REASONS,
  waitingLabel,
} from "./issues-stages";
import type { Item } from "./issues-table-model";
import type { BarBy } from "./issues-views";

/** One segment of a bar: the value it counts, its label, its fill and how many rows it holds. */
export interface BarSegment {
  readonly value: string;
  readonly label: string;
  readonly fill: string;
  readonly count: number;
}

const STAGE_FILLS: Readonly<Record<string, string>> = stageFill;
const WAITING_FILLS: Readonly<Record<string, string>> = {
  ready: stageFill.planning,
  slot: stageFill.reviewing,
  blocked: stageFill.held,
  person: stageFill.awaiting_person,
};
const SLOT_FILLS: Readonly<Record<string, string>> = slotUseFill;

/** What a bar counts `items` by: their stages in the stage order, their waiting reasons or their
 *  slot uses in the order of those values, each value with no row left out. A row with no value
 *  of a Legion column counts under None, last. */
export function barSegments(items: readonly Item[], by: BarBy): BarSegment[] {
  const counts = new Map<string, number>();
  for (const item of items) {
    const value =
      by === "stage"
        ? item.stage
        : by === "waiting_reason"
          ? (item.card?.card.waiting?.reason ?? "")
          : (item.card?.card.slot_use ?? "");
    counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  const ordered: readonly { value: string; label: string; fill: string }[] =
    by === "stage"
      ? STAGES.map((stage) => ({
          value: stage.id,
          label: stage.label,
          fill: STAGE_FILLS[stage.id] ?? "",
        }))
      : by === "waiting_reason"
        ? WAITING_REASONS.map((reason) => ({
            value: reason,
            label: waitingLabel(reason),
            fill: WAITING_FILLS[reason] ?? "",
          }))
        : SLOT_USES.map((use) => ({
            value: use,
            label: slotUseLabel(use),
            fill: SLOT_FILLS[use] ?? "",
          }));
  const known = new Set(ordered.map((entry) => entry.value));
  const others = [...counts.keys()].filter((value) => !known.has(value));
  return [
    ...ordered,
    ...others.map((value) => ({
      value,
      label: value === "" ? "None" : value,
      fill: stageFill.icebox,
    })),
  ].flatMap((entry) => {
    const count = counts.get(entry.value) ?? 0;
    return count === 0 ? [] : [{ ...entry, count }];
  });
}

/** A bar's segments, with the Not started count taken out of them where the bar writes it as a
 *  number: the five counts it names, and the rest. */
export function cutNotStarted(segments: readonly BarSegment[]): {
  readonly notStarted: readonly BarSegment[];
  readonly work: readonly BarSegment[];
} {
  const notStarted: ReadonlySet<string> = new Set(NOT_STARTED_STAGES);
  return {
    notStarted: segments.filter((segment) => notStarted.has(segment.value)),
    work: segments.filter((segment) => !notStarted.has(segment.value)),
  };
}

/** The segments drawn from the bar's left edge, each as wide as its count's share. */
export function BarTrack({
  className,
  segments,
}: {
  className: string;
  segments: readonly BarSegment[];
}): ReactNode {
  return (
    <span aria-hidden className={`flex overflow-hidden rounded-sm ${surfaceMutedBg} ${className}`}>
      {segments.map((segment) => (
        <span
          className={`${segment.fill} min-w-px`}
          data-bar-segment={segment.value}
          key={segment.value}
          style={{ flexGrow: segment.count, flexBasis: 0 }}
        />
      ))}
    </span>
  );
}

const notStartedText = (notStarted: readonly BarSegment[]) =>
  notStarted.map((segment) => `${segment.label} ${segment.count}`).join(", ");

/**
 * The bar under the view tabs: the listed rows counted by the view's Bar by column, from the
 * left edge. On All open and Created by me the Not started count stands at the left end as a
 * number naming its five counts, and the stages from Design on fill the rest of the width. Each
 * segment's legend entry is its press target: a press narrows the view to that value, a second
 * press restores the view. A view with no bar keeps the bar's height, so a change of view moves
 * no row of the table.
 */
export function ViewBar({
  by,
  items,
  narrowTo,
  notStartedNumber,
  picked,
}: {
  by: BarBy | undefined;
  /** The rows the table lists. */
  items: readonly Item[];
  /** Narrows the view to `values` of the bar's column, or restores the view with `undefined`. */
  narrowTo: (values: readonly string[] | undefined) => void;
  notStartedNumber: boolean;
  /** The values of the bar's column the view is narrowed to by a press, if it is. */
  picked: readonly string[] | undefined;
}): ReactNode {
  // The pick a press made, so the second press restores exactly the view before it.
  const pressed = useRef<string | undefined>(undefined);
  if (by === undefined) return <div aria-hidden className="h-[4.75rem] md:h-12" />;
  const segments = barSegments(items, by);
  const { notStarted, work } = notStartedNumber
    ? cutNotStarted(segments)
    : { notStarted: [], work: segments };
  const notStartedCount = notStarted.reduce((sum, segment) => sum + segment.count, 0);
  const isPicked = (values: readonly string[]) =>
    picked !== undefined &&
    picked.length === values.length &&
    values.every((value) => picked.includes(value));
  const press = (key: string, values: readonly string[]) => {
    if (pressed.current === key && isPicked(values)) {
      pressed.current = undefined;
      narrowTo(undefined);
      return;
    }
    pressed.current = key;
    narrowTo(values);
  };
  const legendClass = (on: boolean) =>
    `inline-flex min-h-9 shrink-0 items-center gap-1 rounded-md px-1 text-xs whitespace-nowrap md:min-h-7 ${
      on ? tableFilterActive : `${textSecondaryOnCanvas} ${surfaceMutedHoverBg}`
    }`;
  return (
    <section
      aria-label={`Listed issues by ${by === "stage" ? "stage" : by === "waiting_reason" ? "waiting reason" : "slot use"}`}
      className="flex h-[4.75rem] flex-col justify-center gap-1 md:h-12"
      data-view-bar={by}
    >
      <div className="flex items-center gap-2">
        {notStartedNumber ? (
          <button
            aria-pressed={isPicked(NOT_STARTED_STAGES)}
            className={`min-h-9 shrink-0 rounded-md border px-1.5 text-xs tabular-nums md:min-h-6 ${borderDefault} ${
              isPicked(NOT_STARTED_STAGES) ? tableFilterActive : surfaceMutedHoverBg
            }`}
            data-not-started={notStartedCount}
            onClick={() => press("not-started", NOT_STARTED_STAGES)}
            title={notStarted.length === 0 ? "No issue not started" : notStartedText(notStarted)}
            type="button"
          >
            <b className={textPrimaryOnCanvas}>{notStartedCount}</b>
            <span className={`ml-1 ${textMutedOnCanvas}`}>not started</span>
          </button>
        ) : null}
        {work.length === 0 ? (
          <span className={`h-3 flex-1 rounded-sm ${surfaceMutedBg}`} />
        ) : (
          <BarTrack className="h-3 flex-1" segments={work} />
        )}
      </div>
      {/* On a phone the legend scrolls sideways; its right end fades so a cut entry reads as
          one that scrolls into view. */}
      <div className="flex min-w-0 gap-0.5 overflow-x-auto [mask-image:linear-gradient(to_right,#000_85%,transparent)] [scrollbar-width:thin] md:[mask-image:none]">
        {work.map((segment) => (
          <button
            aria-pressed={isPicked([segment.value])}
            className={legendClass(isPicked([segment.value]))}
            data-bar-legend={segment.value}
            key={segment.value}
            onClick={() => press(segment.value, [segment.value])}
            type="button"
          >
            <span aria-hidden className={`size-2.5 rounded-sm ${segment.fill}`} />
            {segment.label}
            <span className="tabular-nums">{segment.count}</span>
          </button>
        ))}
      </div>
    </section>
  );
}
