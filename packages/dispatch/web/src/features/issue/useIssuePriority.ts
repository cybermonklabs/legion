import type { IssuePriority } from "../../api/types";
import { pickLook } from "./issue-field-looks";
import { issuePriority, type PriorityView } from "./issue-priority";
import { type IssueFieldWrite, useIssueFieldWrite } from "./useIssueFieldWrite";

/** The priority's write, without `error`: a refused one shows a fixed line (`PriorityControl`),
 *  not the server's reason, and a stopped one what it may have done (`stopped`). `submit` takes a
 *  priority for the issue's own, or null to unset it; `shown` is the priority the picker draws,
 *  the one the issue works at with the issue it comes from. */
export interface IssuePriorityWrite
  extends Omit<IssueFieldWrite<IssuePriority | null>, "error" | "shown"> {
  readonly shown: PriorityView;
}

/**
 * The one way the SPA writes an issue's priority. Every surface that renders it (issue header,
 * board cards, list rows, the tables of the Priorities view, inbox rows) shares this, as do the
 * issue page's keys `0`-`3` and its palette: the pick's write shows the value on the issue detail,
 * every list of issues and every inbox query that shows the issue, keeps it there over every
 * answer until it lands, and puts back the value Dispatch last confirmed if it is refused (see
 * `useIssueFieldWrite` and `issue-field-writes.ts`). `host` is the priority as the caller reads
 * it from the issue's caches (`issuePriority`); draw `shown`.
 *
 * A pick writes the issue's own priority, never the one it inherits. A pick of a level is also
 * the priority the issue works at, from nowhere else, from its press; a pick of none leaves the
 * priority the issue showed before until Dispatch's answer says what it inherits, as an Inherit
 * pick of the repository does (`pickLook`), and the picker says the pick is out (`pending`).
 */
export function useIssuePriority(issueKey: string, host: PriorityView): IssuePriorityWrite {
  const write = useIssueFieldWrite(issueKey, "priority", host.own);
  const own = write.shown;
  // A pick the host does not pass yet shows as the caches draw it from its press (`pickLook`):
  // its own level, or for none, the priority the host shows until the answer.
  const shown =
    own === host.own
      ? host
      : issuePriority({
          priority_from: host.from,
          resolved_priority: host.level,
          ...pickLook({ field: "priority", issueKey, value: own }).fields,
        });
  return {
    failed: write.failed,
    pending: write.pending,
    retry: write.retry,
    shown,
    stopped: write.stopped,
    submit: write.submit,
  };
}
