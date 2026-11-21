/** How many of a table's writes are out at once, when Finish sends a selection's writes for the
 *  first time or Finish is retrying what a refusal left behind. A selection is as large as the
 *  project, and "Select all N matching" is two clicks away, so the table sends a bounded stream
 *  rather than every request at once; each write is still independent in the store.
 *
 *  Its own module, and not `issues-table-model.ts` where it stood, because of who reads it: the
 *  table's write kind (`issues-table-recovery.ts`) is registered from the kinds chunk the write
 *  store's take-up fetches as the person signs in, so that a reload finds a kept table write's
 *  kind (`features/writes/kinds.ts`), and this one number would pull the whole table model and
 *  its match ranking - about 16,000 bytes - into what every signed-in page downloads at sign-in
 *  (dispatch://INFRA-1109, R13). Every reader imports it from here; the model does not re-export
 *  it. */
export const WRITES_IN_FLIGHT = 8;
