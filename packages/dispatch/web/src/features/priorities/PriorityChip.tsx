import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { usePageWords, useTooltips } from "../../components/tooltips";
import { badgePrimary, textMutedOnCanvas } from "../../theme/classes";
import { borderStrong, borderTransparent, textSecondaryOnCanvas } from "../../theme/shell-classes";
import { orderLabel, priorityName } from "./priorities-model";
import { priorityPath } from "./priority-routes";

/**
 * One priority an issue advances, drawn as its place in the one order (`#3`): a compact chip that
 * links to the priority's own page. The whole title is in the chip's accessible name ("#3 Ship
 * it") and, where the surface has tooltips, in its `title`, so a row stays one line tall however
 * long the title is. A priority the issue inherits from an issue above takes the muted, dashed
 * style, and its name and tooltip say it is inherited.
 *
 * `titled` draws the title after the chip, inside the same link, wrapped whole and never cut: for
 * a cell whose point is which priority the row is under, as a fault list's Advances cell is,
 * where a phone has no tooltip to read the title in.
 *
 * `place` and `title` are the priorities read's: a priority that read does not hold, one deleted
 * since, is named by its id.
 *
 * The chip is the link's inner box, so the link can grow to the 44 px target narrow windows give
 * every link (`styles.css`) while the chip keeps its size. Below 768 px, where the untitled chip
 * stands first in the issue header's details row, the link is as wide as the chip and its 44 px
 * target is a box drawn round it (`::after`, centred on the chip) rather than room at its right:
 * the row then places what follows the chip by the chip's own edge, whatever its place's digits
 * (`IssuePriorityLine`'s "+", 8 px after it).
 */
export function PriorityChip({
  id,
  inherited,
  place,
  title,
  titled = false,
}: {
  id: string;
  inherited: boolean;
  /** The priority's place in the order, from 1: what `#3` reads from. */
  place: number | undefined;
  title: string | undefined;
  /** Whether the title follows the chip, whole, in the link. */
  titled?: boolean;
}): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  const label = place === undefined ? id : orderLabel(place - 1);
  const named = priorityName(id, place, title);
  const whole = title ?? id;
  const chip = `rounded-sm border px-1.5 py-0.5 text-xs font-semibold tabular-nums whitespace-nowrap group-hover:underline ${
    inherited
      ? `border-dashed ${borderStrong} ${textMutedOnCanvas}`
      : `${borderTransparent} ${badgePrimary.bg} ${badgePrimary.text}`
  }`;
  return (
    <Link
      aria-label={
        inherited ? `${named}, ${page?.CONTROL_LABELS.advances_inherited ?? "inherited"}` : named
      }
      className={
        titled
          ? "group flex min-w-0 items-baseline gap-1.5"
          : "group inline-flex shrink-0 items-center max-md:relative max-md:min-w-0! max-md:after:absolute max-md:after:inset-y-0 max-md:after:left-1/2 max-md:after:w-11 max-md:after:-translate-x-1/2"
      }
      title={tooltips ? (inherited ? `${whole}, inherited from an issue above` : whole) : undefined}
      to={priorityPath(id)}
    >
      <span className={titled ? `shrink-0 ${chip}` : chip}>{label}</span>
      {/* The space between the two is no box of the flex row, but it is in the link's words, so
          they read "#3 Ship it" as its name does (axe's `label-content-name-mismatch`). */}
      {titled && title !== undefined ? (
        <>
          {" "}
          <span className={`min-w-0 break-words group-hover:underline ${textSecondaryOnCanvas}`}>
            {title}
          </span>
        </>
      ) : null}
    </Link>
  );
}
