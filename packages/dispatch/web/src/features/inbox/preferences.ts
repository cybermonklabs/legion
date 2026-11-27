/**
 * The Inbox's browser-kept choices: the ones that belong to the person's browser rather than to
 * the list they linked. The page and the drawer read the same values, so the two agree, and a
 * reload keeps them. Everything the address can carry - the filters, the sort, the grouping and
 * the switch between the Inbox and what the reader has dismissed out of it - stays in the
 * address on the page and in `useState` in the drawer. A band an address opens is no choice of
 * the reader's either: it stays open for that view alone, beside the folds they keep
 * (`useFoldedGroups`).
 *
 * The thread order is a browser-kept choice too, but it is no Inbox setting: every thread in
 * Dispatch reads in it, so it lives with the thread modules (`features/threads/thread-order.ts`).
 */

import { useCallback, useState } from "react";

import type { InboxGroupBy } from "../refs/routes";
import { useUserPreference } from "../shell/userPreference";
import { DEFAULT_FOLDED_GROUPS } from "./grouping";

/** The folded bands of every grouping, as the preference stores them: one list of band keys per
 *  Group by value, so folding `P1` under Priority says nothing about the Project bands. */
type FoldedByGrouping = Partial<Record<InboxGroupBy, readonly string[]>>;

function readFolded(stored: string | null): FoldedByGrouping {
  if (stored === null) return {};
  try {
    const parsed: unknown = JSON.parse(stored);
    if (parsed === null || typeof parsed !== "object") return {};
    const folded: FoldedByGrouping = {};
    for (const [grouping, keys] of Object.entries(parsed)) {
      if (Array.isArray(keys)) {
        folded[grouping as InboxGroupBy] = keys.filter((key) => typeof key === "string");
      }
    }
    return folded;
  } catch {
    // A value another build wrote, or a person edited: the reader's folds are not worth a
    // broken Inbox, so the list opens with the default fold instead.
    return {};
  }
}

/** The folds of one grouping as the Inbox draws them, and the two ways they change
 *  (`useFoldedGroups`). */
export interface FoldedGroups {
  /** The bands folded shut: the ones the reader keeps folded, less those an address opened for
   *  this view. */
  readonly folded: ReadonlySet<string>;
  /** Opens band `key`, where it is folded, for this view alone: an `?item=` naming a row in it.
   *  A link is no press of the reader's, so it writes nothing to the folds they keep. */
  readonly openFoldedBand: (key: string) => void;
  /** The reader's own press on band `key`'s toggle: it folds or opens the band as it is drawn,
   *  and writes what it leaves to the folds they keep, so a band an address opened for this
   *  view folds shut, as they keep it. */
  readonly toggleFold: (key: string) => void;
}

/**
 * Which bands of `by` are folded shut, and the two ways that changes. **Later** starts folded -
 * its rows are the ones the reader already dealt with by deferring - and from the moment they
 * fold or open anything under this grouping, the set they left is the whole answer. Only the
 * reader's own press writes it (`toggleFold`): a band an address opens (`openFoldedBand`) is
 * open for this view alone, and the folds the reader keeps in their browser stay as they left
 * them.
 */
export function useFoldedGroups(by: InboxGroupBy): FoldedGroups {
  const [stored, setStored] = useUserPreference<FoldedByGrouping>(
    "inbox.folded-groups",
    readFolded,
    (value) => JSON.stringify(value)
  );
  // The bands an address opened for this view: a band is folded where the reader keeps it
  // folded and no address opened it here.
  const [openedHere, setOpenedHere] = useState<ReadonlySet<string>>(() => new Set());
  const kept = new Set(stored[by] ?? DEFAULT_FOLDED_GROUPS);
  const folded =
    openedHere.size === 0 ? kept : new Set([...kept].filter((key) => !openedHere.has(key)));
  const toggleFold = useCallback(
    (key: string) => {
      const shut = !folded.has(key);
      if (shut && openedHere.has(key)) {
        setOpenedHere(new Set([...openedHere].filter((opened) => opened !== key)));
      }
      const next = new Set(stored[by] ?? DEFAULT_FOLDED_GROUPS);
      if (shut) next.add(key);
      else next.delete(key);
      setStored({ ...stored, [by]: [...next] });
    },
    [by, folded, openedHere, setStored, stored]
  );
  const openFoldedBand = useCallback(
    (key: string) => {
      if (folded.has(key)) setOpenedHere((current) => new Set(current).add(key));
    },
    [folded]
  );
  return { folded, openFoldedBand, toggleFold };
}
