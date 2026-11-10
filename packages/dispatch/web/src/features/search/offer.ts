import type { ReactNode } from "react";
import { useState } from "react";

import { textMutedOnSurface } from "../../theme/classes";
import { type Attempt, plainWrite, type Writes } from "../writes/write-store";

/** The one row an offer lists in the bar: the id the arrows and `aria-activedescendant` read,
 *  what it says, and what pressing it does. The row carries its own `select`, so an offer with
 *  nothing to press lists no row at all rather than a row the bar presses nothing with. */
export interface OfferRow {
  readonly id: string;
  readonly kind: "offer";
  readonly label: string;
  readonly select: () => void;
}

/** What an offer is at this moment: the one row the bar lists for it, if any, and the line the
 *  bar says underneath about what it wrote or could not write. */
export interface Offer {
  readonly row: OfferRow | undefined;
  readonly said: ReactNode;
}

/**
 * An offer's state, which is always of the words that were in the box when it was set:
 * `undefined` is the offer with nothing pressed, and every other state names its own words. A
 * box the person has typed on since is another question, so the state of the words before it is
 * dropped rather than read as this one's answer. The drop is made during the render that
 * notices it, as React's own render-phase update, so no row and no line of the words before
 * ever reaches the reader.
 */
export function useWordsState<T extends { readonly words: string }>(
  words: string
): readonly [T | undefined, (state: T) => void] {
  const [state, setState] = useState<T | undefined>(undefined);
  if (state !== undefined && state.words !== words) {
    setState(undefined);
  }
  return [state?.words === words ? state : undefined, setState];
}

/** The quiet line the bar says under its list: what an offer is writing, what it wrote, what it
 *  could not write, and how many more matches the search found than the list holds. */
export const barNoteClassName = `shrink-0 px-3 py-2 text-xs ${textMutedOnSurface}`;

/** One request, under a key of its own, tried until Dispatch decides it and named in the shell's
 *  indicator while it is out. A filing is four of these rather than one, because one write
 *  makes one request (`OneRequestPerWriteError`): Dispatch would refuse a second request under a
 *  key it has already decided. `inline` because the bar says a refusal itself. */
export function press<T>(writes: Writes, label: string, write: Attempt<T>): Promise<T | null> {
  return writes.run({
    detail: null,
    inline: true,
    kind: plainWrite,
    label,
    presentation: "indicator",
    write,
  });
}

/** Dispatch's own refusal where it gave one, and `whenSilent` for whatever else stopped the
 *  write: a reader told only "could not" has nothing to act on. */
export function refusalText(error: unknown, whenSilent: string): string {
  return error instanceof Error && error.message !== "" ? error.message : whenSilent;
}
