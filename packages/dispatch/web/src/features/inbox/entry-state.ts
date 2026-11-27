/**
 * The Inbox's marks on the history entry of an Inbox address.
 *
 * WHY A MODULE OF ITS OWN. The shell reads them in the entry chunk, to size the page before the
 * Inbox's chunk is in (`useInboxFillsViewport`), and the Inbox's selection writes them
 * (`usePaneSelection`, in that chunk). Rollup puts all of a module in one chunk, so a read from
 * `pane-selection.ts` would bring the whole selection hook and what it imports into the entry,
 * which every page parses before it draws anything (`MAX_ENTRY_BYTES` in
 * `web/src/asset-sizes-in-build.ts`). This module imports nothing.
 */

/** What an Inbox address's history entry says about how its `?item=` got there. Kept in the
 *  entry itself, so Back, a reload and a remount read it exactly as the entry was written, and
 *  a `replace` that does not carry a mark drops it. */
export interface InboxEntryState {
  /** The column chose the row by itself - the first one the reader could see - rather than the
   *  reader picking it. Below the column's width such a row is not open: the list shows. */
  inboxDefaultPick?: true;
  /** The entry was pushed over the list's own by a row the reader opened below the column's
   *  width, so the way back to the list goes back through it. */
  inboxOverList?: true;
}

/** The Inbox's marks on a history entry's state, whatever else the entry holds. */
export function inboxEntryState(state: unknown): InboxEntryState {
  if (typeof state !== "object" || state === null) return {};
  const { inboxDefaultPick, inboxOverList } = state as Record<string, unknown>;
  return {
    ...(inboxDefaultPick === true ? { inboxDefaultPick: true } : {}),
    ...(inboxOverList === true ? { inboxOverList: true } : {}),
  };
}
