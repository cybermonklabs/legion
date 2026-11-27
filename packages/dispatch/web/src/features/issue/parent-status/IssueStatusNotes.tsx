import type { ReactNode } from "react";

import { refusedNote } from "../../writes/useKeyedWrite";
import { ParentStatusDialog } from "./ParentStatusDialog";
import type { IssueStatusMove } from "./useIssueStatusMove";

/** What a surface says of a status write of `issueKey` Dispatch refused (`refusal`): the issue
 *  headers' words, which the board says too for a write it moved on from (`useBoardMoves`). */
export function statusRefusalNote(issueKey: string, refusal: unknown): string {
  return refusedNote(`change the status of ${issueKey}`, refusal);
}

/**
 * What an issue header shows of its status writes' question (`useIssueStatusMove`): why the dry
 * run of the newest one was refused, which put the status back, or why Dispatch refused a chosen
 * write the question was closed on past that write's deadline (`left`) - said as the header says
 * why for a write it shows refused itself - and the question while it is open. `noteClassName` is
 * the header's own look for a refusal.
 */
export function IssueStatusNotes({
  issueKey,
  noteClassName,
  statusMove,
}: {
  issueKey: string;
  noteClassName: string;
  statusMove: Pick<IssueStatusMove, "question" | "refusal">;
}): ReactNode {
  return (
    <>
      {statusMove.refusal === undefined ? null : (
        <p className={noteClassName} role="alert">
          {statusRefusalNote(issueKey, statusMove.refusal)}
        </p>
      )}
      {statusMove.question === undefined ? null : (
        <ParentStatusDialog question={statusMove.question} />
      )}
    </>
  );
}
