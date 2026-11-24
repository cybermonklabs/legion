import type { ReactNode } from "react";

import { refusedNote } from "../../writes/useKeyedWrite";
import { ParentStatusDialog } from "./ParentStatusDialog";
import type { IssueStatusMove } from "./useIssueStatusMove";

/**
 * What an issue header shows of its status writes' question (`useIssueStatusMove`): why the dry
 * run of the newest one was refused, which put the status back - said as the header says why for
 * a write it shows refused itself - and the question while it is open. `noteClassName` is the
 * header's own look for a refusal.
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
          {refusedNote(`change the status of ${issueKey}`, statusMove.refusal)}
        </p>
      )}
      {statusMove.question === undefined ? null : (
        <ParentStatusDialog question={statusMove.question} />
      )}
    </>
  );
}
