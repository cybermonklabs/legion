/** How many of a table's writes are out at once, when Finish sends a selection's writes for the
 *  first time or Finish is retrying what a refusal left behind. A selection is as large as the
 *  project, and "Select all N matching" is two clicks away, so the table sends a bounded stream
 *  rather than every request at once; each write is still independent in the store.
 *
 *  Its own module, and not `issues-table-model.ts` where it stood, because of who reads it: the
 *  table's write kind (`issues-table-recovery.ts`) is registered by the shell at boot, so that a
 *  reload finds a kept table write's kind (`features/writes/kinds.ts`), and this one number was
 *  pulling the whole table model and its match ranking - about 16,000 bytes - into the entry
 *  chunk every page of the dashboard parses before it draws anything (dispatch://INFRA-1109,
 *  R13). The model re-exports it, so every other reader is unchanged. */
export const WRITES_IN_FLIGHT = 8;
