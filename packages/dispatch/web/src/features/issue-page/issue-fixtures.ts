import type { Agent, Issue, IssueDetails } from "../../api/types";

/** The sessions the claim chip's tests (`ClaimChip.test.tsx`, `IssueClaimControl.test.tsx`) and
 *  the Activity tab's (`ActivityTab.test.tsx`) list as a session, and the answer the tests of the
 *  page's writes (`shared-writes.test.ts`, `IssueMargin.test.tsx`, `issue-writes.test.tsx`) give
 *  a PATCH of the issue. */

/** One live session, as `GET /api/v1/agents` lists it. */
export function running(sessionID: string, title: string): Agent {
  return {
    session_id: sessionID,
    title,
    dir: "/w",
    host: "host",
    machine_id: "host",
    roles: [],
    capabilities: [],
    last_seen: 1,
    open_asks: 0,
    last_activity: null,
    state: "live",
  };
}

/** The same session after it has missed its heartbeats: the listener still lists it, and says
 *  so on the row (`state`), with the heartbeat it was last seen at. */
export function unresponsive(sessionID: string, title: string, lastSeen: number): Agent {
  return { ...running(sessionID, title), last_seen: lastSeen, state: "unresponsive" };
}

/** The server answers a PATCH with the narrow `Issue`: no artifacts, children or open asks. */
export function narrowIssue(details: IssueDetails): Issue {
  const { artifacts: _artifacts, children: _children, open_asks: _openAsks, ...rest } = details;
  return rest;
}
