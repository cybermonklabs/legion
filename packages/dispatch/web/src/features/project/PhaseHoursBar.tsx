// One issue's hours by Legion phase as one stacked bar (dispatch://INFRA-979): the finished-issues
// table's phase column and the Issues table's optional "Time per phase" column draw it.
import type { ReactNode } from "react";

import { legionPhaseScale, surfaceMutedBg } from "../../theme/classes";
import { formatHours } from "./LegionMetrics";

/** The phases in workflow order, with the label each block's title names. */
export const PHASE_ORDER: readonly { id: string; label: string }[] = [
  { id: "design", label: "Design" },
  { id: "planning", label: "Planning" },
  { id: "implementing", label: "Implementing" },
  { id: "testing", label: "Testing" },
  { id: "reviewing", label: "Reviewing" },
  { id: "accepting", label: "Accepting" },
  { id: "merging", label: "Merging" },
  { id: "awaiting_person", label: "Waiting on a person" },
  { id: "held", label: "Held" },
  { id: "other", label: "Other" },
];

/** The sum of an issue's hours by phase. */
export function phaseHoursTotal(hours: Readonly<Record<string, number>>): number {
  return Object.values(hours).reduce((sum, value) => sum + value, 0);
}

/**
 * The stacked bar. In `hours` mode its length is the issue's hours against `max` (the longest
 * issue of the table, so all rows share one scale; the issue's own total when no `max`). In
 * `share` mode every bar is full length, and each block is its share of the issue's hours.
 */
export function PhaseHoursBar({
  hours,
  mode,
  max,
}: {
  readonly hours: Readonly<Record<string, number>>;
  readonly mode: "hours" | "share";
  readonly max?: number;
}): ReactNode {
  const total = phaseHoursTotal(hours);
  if (total <= 0) return null;
  const length = mode === "share" ? 1 : Math.min(1, total / Math.max(max ?? total, total));
  const known = new Set(PHASE_ORDER.map((phase) => phase.id));
  const parts = [
    ...PHASE_ORDER,
    ...Object.keys(hours)
      .filter((id) => !known.has(id))
      .map((id) => ({ id, label: id })),
  ].filter((phase) => (hours[phase.id] ?? 0) > 0);
  return (
    <span className="flex items-center gap-1" data-phase-hours-bar={mode}>
      {mode === "hours" ? <span data-phase-hours-total>{formatHours(total)}</span> : null}
      <span className={`flex h-3 ${mode === "hours" ? "min-w-0 flex-1" : "w-full"}`}>
        <span
          className={`flex h-3 overflow-hidden rounded-sm ${surfaceMutedBg}`}
          data-phase-hours-length={String(length * 100)}
          style={{ width: `${length * 100}%` }}
        >
          {parts.map((phase) => {
            const spent = hours[phase.id] ?? 0;
            return (
              <span
                className={legionPhaseScale[phase.id] ?? legionPhaseScale.other ?? ""}
                data-phase-hours-part={phase.id}
                key={phase.id}
                style={{ width: `${(100 * spent) / total}%` }}
                title={`${phase.label}: ${formatHours(spent)}, ${Math.round((100 * spent) / total)}%`}
              />
            );
          })}
        </span>
      </span>
    </span>
  );
}
