import { type ReactNode, useState } from "react";

import { QueryError } from "../../components/QueryError";
import {
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/shell-classes";
import { useIssueAssignee } from "../issue/useIssueAssignee";
import { useLiveRegistration } from "../writes/useKeyedWrite";

/** Takes an issue nobody holds for the viewer: one PATCH through the shared assignee write, so
 *  the row is theirs at once and rolls back if the server refuses. Under the Unassigned filter
 *  the row stops matching while the save is in flight, so the control tells the Inbox when its
 *  write is live (pending or failed) and the Inbox keeps the row listed until then: `Assigning…`,
 *  and on refusal the server's reason with Retry, are shown by this same instance rather than
 *  lost to a remount.
 *
 *  Its own module rather than `Inbox.tsx`'s, because the Inbox's reading pane draws it too
 *  (`ReadingPaneHead`): the Inbox mounts the pane, so a control the pane's head imports from the
 *  Inbox would close a cycle between the three files. */
export function AssignToMe({
  askId,
  issueKey,
  onLive,
  viewer,
}: {
  /** The row this control sits in; liveness is registered per row, never per issue, so a
   *  sibling row of the same issue leaving the list cannot drop this row's registration. */
  askId: string;
  issueKey: string;
  /** Registers the row of the ask its write was pressed on while that write is in flight or
   *  failed; unregistered once it settles cleanly or the control unmounts. */
  onLive: (askId: string, live: boolean) => void;
  viewer: string;
}): ReactNode {
  // Offered on an issue nobody holds, it stands on no assignee, and draws none: its words say
  // whether its write is out, not who holds the issue.
  const write = useIssueAssignee(issueKey, null);
  // The row held is the one the live save was pressed on, which is this row's unless a host has
  // drawn the control for another ask since that press. The save carries its issue and not the
  // row it was pressed on (`useIssueAssignee`), so the control keeps that row's ask itself.
  const [pressedOn, setPressedOn] = useState<string | undefined>(undefined);
  useLiveRegistration(write.pending || write.failed ? pressedOn : undefined, onLive);
  return (
    <>
      <button
        aria-label={`Assign ${issueKey} to me`}
        className={`min-h-11 shrink-0 rounded-lg px-2 py-1 text-xs font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
        disabled={write.pending}
        onClick={() => {
          setPressedOn(askId);
          write.submit(viewer);
        }}
        type="button"
      >
        {write.pending ? "Assigning…" : "Assign to me"}
      </button>
      {write.failed ? (
        <div className="basis-full">
          <QueryError
            message={write.error ?? `Could not assign ${issueKey} to you.`}
            onRetry={write.retry}
            retrying={write.pending}
          />
        </div>
      ) : null}
    </>
  );
}
