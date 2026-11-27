import type { ReactNode } from "react";

import {
  bgTransparent,
  focusVisibleRing,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textPrimaryOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurfaceMuted,
} from "../theme/classes";
import { CONTROL, type ControlSize, controlPadding } from "./control-names";

/** One part of the switch, as the page names it. */
export interface SegmentedPart {
  /** How many the part holds, drawn beside its label: the asks and the credential requests that
   *  wait for the reader, on the Inbox's **Blocked on you**. No number is drawn while the count
   *  is not known (undefined) or at zero, so a part that holds nothing says nothing about it. */
  readonly count?: number;
  readonly label: string;
  /** The part's words in the narrow layout (`narrow`), where the bar has no room for its whole
   *  label: the Inbox's **You**, drawn **You · 3** with its count. Its accessible name stays the
   *  one it has at every width: its `title`, else its whole label and count. */
  readonly short?: string;
  /** The part's accessible name and tooltip, where it says more than its words: the Inbox's
   *  **Blocked on you** is named `Blocked on you: 3 items, oldest 2 days` (`blockedOnYouText`).
   *  Its words, with its count, where unset. */
  readonly title?: string;
  /** What the page is handed back when the part is chosen. */
  readonly value: string;
}

export interface SegmentedControlProps {
  /** The group's accessible name, which no part draws: `Inbox section`, `Layout`. */
  readonly label: string;
  /** The control bar's narrow layout, below 640 px of the bar's width, which the bar tells the
   *  switch (`ControlRow`): a part given a `short` label draws it there. */
  readonly narrow?: boolean;
  /** Told the value of the part the reader pressed. The page owns which part is chosen. */
  readonly onChange: (value: string) => void;
  /** The parts, in their order: two or more, so a switch always offers a choice. */
  readonly parts: readonly [SegmentedPart, SegmentedPart, ...SegmentedPart[]];
  readonly size?: ControlSize;
  /** The chosen part's value. A value naming no part leaves none chosen. */
  readonly value: string;
}

/** A part, chosen or not. The chosen part has a filled surface and a heavier label - two
 *  differences, and not a colour alone - and every other part has no fill and no border, as the
 *  control bar's quiet buttons have. */
const PART = `inline-flex shrink-0 items-center gap-1.5 rounded-lg text-sm whitespace-nowrap outline-none focus-visible:ring-2 ${focusVisibleRing}`;
const CHOSEN = `font-semibold shadow-sm ${surfaceBg} ${textPrimaryOnSurface}`;
const UNCHOSEN = `font-medium ${bgTransparent} ${textSecondaryOnSurfaceMuted} ${textSecondaryHoverToPrimary}`;

/** The number beside a part's label. */
const COUNT = `rounded-full px-1.5 text-xs font-semibold tabular-nums ${surfaceMutedStrongBg}`;

/**
 * The one switch: two or more parts of one list, one of them chosen, where each part shows a part
 * of the same list - the Inbox's **All** and **Blocked on you**, the Issues tab's **Table** and
 * **Board**. It stands after the control bar's search box (`ControlRow`), so it leads the Inbox's
 * bar, whose search box stands in the header line, as GitHub's All and Unread and Outlook's
 * Focused and Other stand at the start of their lists' bars; Primer names the segmented control
 * for a filter one of whose parts shows the whole list.
 *
 * It is drawn as the repository's own groups of pressed buttons are (the project page's List and
 * Board): a group named by the page (`fieldset`, whose role is `group`, with the page's
 * `aria-label`), its parts buttons whose `aria-pressed` says which one is chosen, so a screen
 * reader reads the chosen part as chosen. Each part is a Tab stop and a press of its own. A press
 * on a part hands the page that part's value; the page decides what choosing it does, and which
 * part is chosen, so a part reads as chosen only once the page says so. A part of the switch
 * never goes while the switch is drawn, so it hands no focus on. In the control bar's narrow
 * layout a part given a short label draws it - the Inbox's **All** and **You · 3** - and keeps the
 * accessible name it has at every width.
 */
export function SegmentedControl({
  label,
  narrow = false,
  onChange,
  parts,
  size = "comfortable",
  value,
}: SegmentedControlProps): ReactNode {
  return (
    <fieldset
      aria-label={label}
      className={`inline-flex shrink-0 items-stretch rounded-lg ${surfaceMutedBg}`}
      data-control={CONTROL.segmentedControl}
    >
      {parts.map((part) => {
        const chosen = part.value === value;
        const counted = part.count !== undefined && part.count > 0;
        const short = narrow ? part.short : undefined;
        // Its name at every width: its title, else its whole words and count, which a short
        // label drawn in the narrow layout does not change.
        const name =
          part.title ??
          (short === undefined ? undefined : counted ? `${part.label} ${part.count}` : part.label);
        return (
          <button
            aria-label={name}
            aria-pressed={chosen}
            className={`${PART} ${controlPadding[size]} ${chosen ? CHOSEN : UNCHOSEN}`}
            key={part.value}
            onClick={() => onChange(part.value)}
            title={part.title}
            type="button"
          >
            {short === undefined ? (
              <>
                {part.label}
                {/* A space, which the flex line draws as nothing, so a screen reader reads the
                    count as a word of its own: `Blocked on you 3`. */}
                {counted ? (
                  <>
                    {" "}
                    <span className={COUNT}>{part.count}</span>
                  </>
                ) : null}
              </>
            ) : counted ? (
              `${short} · ${part.count}`
            ) : (
              short
            )}
          </button>
        );
      })}
    </fieldset>
  );
}
