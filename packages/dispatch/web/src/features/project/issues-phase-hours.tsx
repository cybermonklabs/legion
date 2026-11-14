import { createContext, type ReactNode, useContext } from "react";
import type { LegionBoardFacts } from "../../api/types";
import {
  borderDefault,
  stageFill,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useUserPreference } from "../shell/userPreference";
import { isStageId, stageLabel } from "./issues-stages";

/** How Time per phase draws its bars: each phase's part of that row's own total (Share), or each
 *  bar against the longest total on the current page, with the total written beside it (Hours). */
export type PhaseHoursMode = "share" | "hours";

/** What every Time per phase cell of the page reads: the mode, and the longest total on the
 *  page, which Hours scales each bar against. Given once by the table rather than to each column
 *  definition, so a new page or a switch of mode draws no column anew. */
export const PhaseHoursContext = createContext<{
  readonly mode: PhaseHoursMode;
  readonly pageMax: number;
}>({ mode: "share", pageMax: 0 });

/** The reader's mode, kept in this browser for the signed-in login, so it survives a reload.
 *  Share until the reader picks Hours. */
export function usePhaseHoursMode(): [PhaseHoursMode, (next: PhaseHoursMode) => void] {
  return useUserPreference<PhaseHoursMode>(
    "project.phase-hours",
    (stored) => (stored === "hours" ? "hours" : "share"),
    (mode) => mode
  );
}

/** The phases of a row with hours, in the stage order, and their sum. */
export function phaseHours(facts: LegionBoardFacts | undefined): {
  readonly phases: readonly (readonly [string, number])[];
  readonly total: number;
} {
  const phases = Object.entries(facts?.hours_by_phase ?? {}).filter(([, hours]) => hours > 0);
  return { phases, total: phases.reduce((sum, [, hours]) => sum + hours, 0) };
}

const fillOf = (phase: string) => (isStageId(phase) ? stageFill[phase] : stageFill.held);

const hoursText = (hours: number) => `${hours < 10 ? hours.toFixed(1) : Math.round(hours)} h`;

/** One row's Time per phase: a bar of its phases, in the stage palette. */
export function PhaseHoursCell({ facts }: { facts: LegionBoardFacts | undefined }): ReactNode {
  const { mode, pageMax } = useContext(PhaseHoursContext);
  const { phases, total } = phaseHours(facts);
  if (total === 0) return null;
  const title = phases
    .map(([phase, hours]) => `${stageLabel(phase)} ${hoursText(hours)}`)
    .join(", ");
  const width = mode === "share" || pageMax === 0 ? 100 : (total / pageMax) * 100;
  return (
    // In Hours the total is written first, at a fixed width so the bars still start together:
    // on a phone the column is wider than the room beside the pinned key, and a total at the
    // cell's right end would be off screen.
    <span className="flex min-w-0 items-center gap-2" title={title}>
      {mode === "hours" ? (
        <span className={`w-11 shrink-0 text-right text-xs tabular-nums ${textSecondaryOnCanvas}`}>
          {hoursText(total)}
        </span>
      ) : null}
      <span className="flex h-3 min-w-0 flex-1">
        <span
          className={`flex h-3 overflow-hidden rounded-sm ${surfaceMutedBg}`}
          data-phase-hours-bar
          style={{ width: `${width}%` }}
        >
          {phases.map(([phase, hours]) => (
            <span className={fillOf(phase)} key={phase} style={{ flexGrow: hours }} />
          ))}
        </span>
      </span>
    </span>
  );
}

const MODES: readonly { readonly value: PhaseHoursMode; readonly label: string }[] = [
  { value: "share", label: "Share" },
  { value: "hours", label: "Hours" },
];

/** The two-option control in the Time per phase header. */
export function PhaseHoursModeToggle({
  mode,
  onChange,
}: {
  mode: PhaseHoursMode;
  onChange: (mode: PhaseHoursMode) => void;
}): ReactNode {
  return (
    <fieldset
      aria-label="Time per phase as"
      className={`inline-flex shrink-0 rounded-md border p-0.5 ${borderDefault}`}
    >
      {MODES.map((option) => (
        <button
          aria-pressed={mode === option.value}
          className={`min-h-6 rounded px-1 text-[10px] font-medium ${
            mode === option.value
              ? `${surfaceMutedStrongBg} ${textSecondaryOnCanvas}`
              : textMutedOnCanvas
          }`}
          key={option.value}
          onClick={() => onChange(option.value)}
          type="button"
        >
          {option.label}
        </button>
      ))}
    </fieldset>
  );
}
