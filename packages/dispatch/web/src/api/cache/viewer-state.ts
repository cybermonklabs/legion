import { userAgentStateQuery, userStateQuery } from "../queries";
import type { UserAgentState, UserIssueState } from "../types";
import type { Answered } from "./answered";
import type { Cache } from "./cache";

/** Puts the viewer's state of `issueKey` Dispatch answered with - a pin's, a header pin's, a read
 *  mark's - into the viewer's state (`["user-state"]`), so every surface reads it at once. The
 *  cache holds only Dispatch's answers, so a pin of any issue still out stays composed over it
 *  and none is carried into Dispatch's state. */
export function storeIssueState(
  cache: Cache,
  issueKey: string,
  next: Answered<UserIssueState>
): void {
  cache.setDispatched(userStateQuery().queryKey, next, (current, state) => ({
    ...current,
    [issueKey]: state,
  }));
}

/** Puts the session's state a PUT answered with into the viewer's shared agent-state query, so
 *  every badge and row reads it at once. The cache holds only Dispatch's answers, so a pin of
 *  another session still out is composed over this one as it is read. */
export function storeAgentState(
  cache: Cache,
  sessionId: string,
  next: Answered<UserAgentState>
): void {
  cache.setDispatched(userAgentStateQuery().queryKey, next, (current, state) => ({
    ...current,
    [sessionId]: state,
  }));
}
