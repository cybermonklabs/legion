import { textMutedOnSurface } from "../../theme/classes";

/** The one row an offer lists in the bar: the id the arrows and `aria-activedescendant` read,
 *  what it says, and what pressing it does. The row carries its own `select`, so an offer with
 *  nothing to press lists no row at all rather than a row the bar presses nothing with. */
export interface OfferRow {
  readonly id: string;
  readonly kind: "offer";
  readonly label: string;
  readonly select: () => void;
}

/** The quiet line the bar says under its list: what the bar cannot do, and how many more matches
 *  the search found than the list holds. */
export const barNoteClassName = `shrink-0 px-3 py-2 text-xs ${textMutedOnSurface}`;

/** Dispatch's own refusal where it gave one, and `whenSilent` for whatever else ended the write:
 *  a reader told only "could not" has nothing to act on. */
export function refusalText(error: unknown, whenSilent: string): string {
  return error instanceof Error && error.message !== "" ? error.message : whenSilent;
}
