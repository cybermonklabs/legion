import type { QueryClient } from "@tanstack/react-query";

import { inboxQuery, projectsQuery } from "../../api/queries";
import type { Ask } from "../../api/types";
import { askArtifactId } from "./ask-comment";

/**
 * The reads that carry an ask besides its own thread: the Inbox, and the lists of whatever owns
 * it - its issue, or the project document it is written on.
 *
 * Every write that moves whose turn an ask is, or whom it waits on, invalidates all of them
 * rather than the Inbox alone: the issue page, a document's margin and a document's ask block
 * read the ask from its owner's list, so a write that refreshed only the Inbox would leave
 * those surfaces showing the person or the turn from before it until the `ask.*` event landed.
 * `useAskAnswerForm` (an answer, a clarification) and `useAskHandOn` (a hand-on) share it.
 */
export function invalidateOwnerReads(queryClient: QueryClient, ask: Ask): void {
  void queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey });
  if (ask.issue_key === null) {
    void queryClient.invalidateQueries({ queryKey: ["artifact", askArtifactId(ask)] });
    void queryClient.invalidateQueries({ queryKey: projectsQuery().queryKey });
    return;
  }
  void queryClient.invalidateQueries({ queryKey: ["asks", ask.issue_key] });
  void queryClient.invalidateQueries({ queryKey: ["issue", ask.issue_key] });
  void queryClient.invalidateQueries({ queryKey: ["issues"] });
}
