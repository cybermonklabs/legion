import type { Cache } from "../../api/cache/cache";

import { inboxQuery, projectsQuery } from "../../api/queries";
import type { Ask } from "../../api/types";

/** A document ask's artifact; the server always sets it, so its absence is a contract error. */
export function askArtifactId(ask: Ask): string {
  if (ask.artifact_id === null || ask.artifact_id === undefined) {
    throw new Error("document ask is missing its artifact id");
  }
  return ask.artifact_id;
}

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
export function invalidateOwnerReads(cache: Cache, ask: Ask): void {
  void cache.invalidate({ queryKey: inboxQuery().queryKey });
  if (ask.issue_key === null) {
    void cache.invalidate({ queryKey: ["artifact", askArtifactId(ask)] });
    void cache.invalidate({ queryKey: projectsQuery().queryKey });
    return;
  }
  void cache.invalidate({ queryKey: ["asks", ask.issue_key] });
  void cache.invalidate({ queryKey: ["issue", ask.issue_key] });
  void cache.invalidate({ queryKey: ["issues"] });
}
