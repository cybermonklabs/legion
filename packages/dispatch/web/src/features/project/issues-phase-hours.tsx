import { type ReactNode, useRef } from "react";
import { Tooltip, useTooltip } from "../../components/Tooltip";
import {
  borderDefault,
  focusVisibleRing,
  legionPhaseFill,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useUserPreference } from "../shell/userPreference";
import { type LegionFacts, OTHER_PHASE, WORK_PHASES, workPhaseTitle } from "./issues-legion";

/** How Time per phase draws its bars: each phase's part of that row's own total (Share), or each
 *  bar against the longest total on the current page, with the total written beside it (Hours). */
export type PhaseHoursMode = "share" | "hours";

const readMode = (stored: string | null): PhaseHoursMode =>
  stored === "hours" ? "hours" : "share";
const writeMode = (mode: PhaseHoursMode): string => mode;

/** The reader's mode, kept in this browser for the signed-in login (`project.phase-hours`), so
 *  it survives a reload. Share until the reader picks Hours. */
export function usePhaseHoursMode(): [PhaseHoursMode, (next: PhaseHoursMode) => void] {
  return useUserPreference("project.phase-hours", readMode, writeMode);
}

/** A phase Time per phase draws: one with a fixed fill (`legionPhaseFill`). */
type DrawnPhase = keyof typeof legionPhaseFill;

const isDrawn = (phase: string): phase is DrawnPhase => Object.hasOwn(legionPhaseFill, phase);

/** The phases the cell can draw, in the board's order with Other last (`WORK_PHASES`). */
const DRAWN_PHASES: readonly DrawnPhase[] = WORK_PHASES.filter(isDrawn);

/** A row's hours by phase: each phase with hours in `DRAWN_PHASES`' order, the hours of a key
 *  Time per phase does not draw, a phase a newer server adds among them, counted as Other's, and
 *  their total. */
function phaseHours(facts: Pick<LegionFacts, "hours_by_phase">): {
  readonly phases: readonly (readonly [DrawnPhase, number])[];
  readonly total: number;
} {
  const hours = new Map<DrawnPhase, number>();
  for (const [key, value] of Object.entries(facts.hours_by_phase ?? {})) {
    if (!(value > 0)) continue;
    const phase: DrawnPhase = isDrawn(key) ? key : OTHER_PHASE;
    hours.set(phase, (hours.get(phase) ?? 0) + value);
  }
  const phases = DRAWN_PHASES.flatMap((phase) => {
    const value = hours.get(phase);
    return value === undefined ? [] : [[phase, value] as const];
  });
  return { phases, total: phases.reduce((sum, [, value]) => sum + value, 0) };
}

/** Hours as the cell and its tooltip write them: one decimal under 10 hours, whole above. */
const hoursText = (hours: number): string =>
  `${hours < 10 ? hours.toFixed(1) : Math.round(hours)} h`;

/** The tooltip's lines: one per phase with hours, in the board's order with Other last, each
 *  with its swatch, its name, its hours and its share of the row's total. The lines share their
 *  columns, each as wide as its widest entry, 8 px apart and 8 px inside the tooltip's edges. */
function PhaseHoursLines({
  phases,
  total,
}: {
  phases: readonly (readonly [DrawnPhase, number])[];
  total: number;
}): ReactNode {
  return (
    <ul className="grid grid-cols-[repeat(4,auto)] gap-x-2 gap-y-0.5 px-2 py-0.5">
      {phases.map(([phase, hours]) => (
        <li
          className="col-span-full grid grid-cols-subgrid items-center whitespace-nowrap"
          data-phase-hours-line={phase}
          key={phase}
        >
          <span
            aria-hidden="true"
            className={`size-2.5 rounded-sm ${legionPhaseFill[phase]}`}
            data-phase-swatch={phase}
          />
          <span>{workPhaseTitle(phase)}</span>
          <span className="text-right tabular-nums">{hoursText(hours)}</span>
          <span className={`text-right tabular-nums ${textMutedOnSurface}`}>
            {Math.round((hours / total) * 100)}%
          </span>
        </li>
      ))}
    </ul>
  );
}

/**
 * One row's Time per phase: a bar of its phases, each in its fixed colour, with a 1 px gap
 * between each two segments through which the bar's track shows. In Share the bar fills the
 * cell; in Hours it is drawn against `pageMax`, the longest total on the page, after the total
 * written in hours. The cell is a button, so the keyboard reaches it and a tap opens the tooltip
 * where there is no hover; the tooltip names each phase's hours and share. No part of it
 * carries a native title. An issue whose facts hold no hours draws nothing.
 */
export function PhaseHoursCell({
  facts,
  mode,
  pageMax,
}: {
  facts: Pick<LegionFacts, "hours_by_phase">;
  mode: PhaseHoursMode;
  pageMax: number;
}): ReactNode {
  const anchorRef = useRef<HTMLButtonElement>(null);
  const tooltip = useTooltip();
  const { phases, total } = phaseHours(facts);
  if (total === 0) return null;
  const width = mode === "share" || pageMax <= 0 ? 100 : Math.min(100, (total / pageMax) * 100);
  return (
    <>
      <button
        {...tooltip.props}
        aria-label={`Time per phase, ${hoursText(total)}`}
        className={`flex w-full min-w-0 cursor-default items-center gap-2 rounded-sm outline-none focus-visible:ring-2 ${focusVisibleRing}`}
        data-phase-hours
        onClick={tooltip.show}
        ref={anchorRef}
        type="button"
      >
        {mode === "hours" ? (
          // At a fixed width, so the bars of a page still start together.
          <span
            className={`w-11 shrink-0 text-right text-xs tabular-nums ${textSecondaryOnCanvas}`}
          >
            {hoursText(total)}
          </span>
        ) : null}
        <span className="flex h-3 min-w-0 flex-1">
          <span
            className={`flex h-3 gap-px overflow-hidden rounded-sm ${surfaceMutedBg}`}
            data-phase-hours-bar
            style={{ width: `${width}%` }}
          >
            {/* Each segment's width is its share of the row's total, so the segments fill the
                bar whatever the total. */}
            {phases.map(([phase, hours]) => (
              <span
                className={legionPhaseFill[phase]}
                data-phase-segment={phase}
                key={phase}
                style={{ flexBasis: 0, flexGrow: hours / total }}
              />
            ))}
          </span>
        </span>
      </button>
      <Tooltip anchorRef={anchorRef} trigger={tooltip}>
        <PhaseHoursLines phases={phases} total={total} />
      </Tooltip>
    </>
  );
}

const MODES: readonly { readonly value: PhaseHoursMode; readonly label: string }[] = [
  { value: "share", label: "Share" },
  { value: "hours", label: "Hours" },
];

/** The two-option control in Time per phase's header: Share or Hours. */
export function PhaseHoursModeControl({
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
              : `${textMutedOnCanvas} ${surfaceMutedHoverBg}`
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
