import type { KeptWrite } from "./write-storage";
import type { Write } from "./write-store";

// Which writes the shell's two lists show and how they read each one: the pending-writes
// indicator (`PendingWritesIndicator.tsx`) and the sign-in page's kept writes (`KeptWrites.tsx`).
// The indicator is a chunk of its own, and its host in the entry reads the same selection to draw
// nothing while it is empty (`write-hosts.tsx`); the kept list is in the entry, and its host reads
// the selections and hands them to it. So the selections live here, apart from both lists, as do
// the readings another surface shares: the issue header's writes discard a pick that is still
// waiting to send (`waitingToSend`, `features/issue/useIssueFieldWrite.ts`).

/** The writes the indicator shows: every one shown there that Dispatch has not yet made, but for
 *  a refusal a surface mounted now settles with the reader (`claimed`), which is that surface's. */
export function indicatorWrites(rows: readonly Write[]): readonly Write[] {
  return rows.filter(
    (row) =>
      row.presentation === "indicator" &&
      row.status !== "made" &&
      !(row.status === "refused" && row.claimed)
  );
}

/** The writes a signed-out page still keeps for their person: every one they can see that
 *  Dispatch has not made. A read mark is never shown. */
export function keptWrites(rows: readonly Write[]): readonly Write[] {
  return rows.filter(
    (row) => row.login !== undefined && row.presentation !== "quiet" && row.status !== "made"
  );
}

/** What a signed-out page lists of what the reload kept that the store has not taken up yet
 *  (`Writes.notTakenUp`), read from each write's own record: every one but a read mark. A write
 *  the store takes up leaves those records as it becomes a row of the store's, so none shows
 *  twice. */
export function keptRecords(kept: readonly KeptWrite[]): readonly KeptWrite[] {
  return kept.filter((write) => write.presentation !== "quiet");
}

/** Whether `row` waits its turn behind an earlier write to its target and was never sent. */
export function waitingToSend(row: Write): boolean {
  return row.status === "queued" && !row.sent;
}

/** Whether `row` waits for its person's sign-out in another tab of this browser: none of its
 *  attempts starts until that tab says how it ended (`Writes.holdFor`). A refused one waits for
 *  nothing. */
export function heldForSignOut(row: Write): boolean {
  return row.signingOutElsewhere && row.status !== "refused";
}

/** How much of `row`'s upload has gone up, while its body is still going up on its first try;
 *  `undefined` for any other write. Until an attempt of it has failed, it is uploading, not being
 *  tried again. */
export function uploadedPercent(row: Write): number | undefined {
  return (row.status === "sending" || row.status === "late") && !row.retried
    ? row.uploaded
    : undefined;
}

/** How the indicator's status line and the line that stands in the indicator's place
 *  (`ChangesNotSent` in `write-hosts.tsx`) count `n` writes: "1 change", "2 changes". */
export function changesCount(n: number): string {
  return n === 1 ? "1 change" : `${n} changes`;
}
