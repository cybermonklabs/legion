import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { issueAsk } from "../features/inbox/inbox-fixture";
import { replyRow } from "../features/threads/replies-fixture";
import { eventRefreshTargets } from "./event-refresh";
import { issueQuery, meInboxDismissedQuery, meInboxQuery } from "./queries";
import { refreshQueries } from "./query-refresh";
import type { DispatchEvent, IssueDetails, MeInboxResponse } from "./types";

/**
 * What the events that move an issue's approval gate refresh (R2): the one read's Inbox answer
 * and the pages of what the reader dismissed - each row of an issue carries its issue's gate,
 * which its mark and its reading pane draw - each only where it holds a row of the event's issue,
 * or of its project for `project.updated`, whoever holds a reply in the thread; and on
 * `project.updated` the issue read of each issue of the project the cache holds, whose gate the
 * issue page draws its steps from.
 */

const VIEWER = "alice@example.com";

/** A write of `issue`, in project CORE, as the server writes it; `thread_logins` names whose
 *  thread rows it moved, which names nobody here unless a case says so. */
function event(
  type: DispatchEvent["type"],
  payload: Record<string, unknown>,
  overrides: Partial<DispatchEvent> = {}
): DispatchEvent {
  return {
    actor: { id: "planner", kind: "session" },
    created_at: "2026-11-22T00:00:00Z",
    id: 12,
    issue_key: "CORE-1",
    notify: true,
    payload,
    project: "CORE",
    seq: 4,
    type,
    ...overrides,
  } as DispatchEvent;
}

/** A review of the spec of `issue`, approving it or asking for changes. */
function review(type: "artifact.approved" | "artifact.changes_requested", issue: string) {
  return event(
    type,
    {
      actor: { id: "alice", kind: "user" },
      artifact_id: `${issue}-spec`,
      ask_id: null,
      kind: "person",
      name: "spec.md",
      reason: type === "artifact.approved" ? null : "Name the rollback path.",
      version: 1,
    },
    { issue_key: issue }
  );
}

/** A new version of the spec of `issue`. */
function version(issue: string) {
  return event(
    "artifact.version",
    { artifact_id: `${issue}-spec`, name: "spec.md", version: 2 },
    { issue_key: issue }
  );
}

/** An ask of `issue` opened or resolved with no review, of `kind`, whose thread `logins` hold a
 *  reply. */
function ask(
  type: "ask.opened" | "ask.resolved",
  issue: string,
  { kind = "approval", logins = [] }: { kind?: string; logins?: string[] } = {}
) {
  const opened = issueAsk({
    id: `${issue}-request`,
    issue: { assignee: "alice", key: issue, title: issue },
    issue_key: issue,
    kind,
  });
  return event(
    type,
    {
      ...opened,
      ...(type === "ask.resolved"
        ? { resolution: { reason: "Superseded." }, state: "resolved" }
        : {}),
      thread_logins: logins,
    },
    { issue_key: issue }
  );
}

/** A publish of `project`'s gated steps. */
function projectUpdated(project: string) {
  return event(
    "project.updated",
    {
      created_at: "2026-11-01T00:00:00Z",
      gated_steps: ["criteria", "spec"],
      key: project,
      name: project,
      open_asks: 0,
    },
    { issue_key: null, project }
  );
}

/** One answer of the one read holding an ask and a thread of CORE-1, each with its gate. */
function heldAnswer(): MeInboxResponse {
  const gate = [
    {
      approval: { latest_version: 1, state: "awaiting" },
      artifact_id: "CORE-1-spec",
      step: "spec",
    },
  ];
  const entries = [
    {
      approval_gate: gate,
      ask: issueAsk(),
      moment: "2026-11-21T00:00:00Z",
      node: { id: "ask-a", kind: "ask" },
      relations: [],
    },
    {
      approval_gate: gate,
      moment: "2026-11-21T00:00:00Z",
      node: { id: "c1", kind: "comment" },
      relations: [],
      thread: replyRow("c1", { project: "CORE" }),
    },
  ];
  return {
    as_of: "2026-11-21T00:00:00Z",
    entries,
    horizon: 1,
    limit: 0,
    offset: 0,
    person: "alice",
    total: entries.length,
  };
}

/** Which of the one read's two reads of rows, and which issue reads, `event` marks stale over a
 *  cache holding `inbox` and `dismissed` - each the one answer of rows of CORE-1 unless a case
 *  empties it - and `issues`. */
function refreshed(
  event: DispatchEvent,
  {
    dismissed = heldAnswer(),
    inbox = heldAnswer(),
    issues = [],
  }: { dismissed?: MeInboxResponse; inbox?: MeInboxResponse; issues?: IssueDetails[] } = {}
): { dismissed: boolean; inbox: boolean; issues: boolean[] } {
  const client = new QueryClient();
  client.setQueryData(meInboxQuery().queryKey, inbox);
  client.setQueryData(meInboxDismissedQuery().queryKey, {
    pageParams: [undefined],
    pages: [dismissed],
  });
  for (const issue of issues) client.setQueryData(issueQuery(issue.key).queryKey, issue);
  refreshQueries(client, eventRefreshTargets(event, VIEWER));
  const stale = (queryKey: readonly unknown[]) =>
    client.getQueryState(queryKey)?.isInvalidated === true;
  return {
    dismissed: stale(meInboxDismissedQuery().queryKey),
    inbox: stale(meInboxQuery().queryKey),
    issues: issues.map((issue) => stale(issueQuery(issue.key).queryKey)),
  };
}

/** An answer holding no row at all. */
const EMPTY: MeInboxResponse = { ...heldAnswer(), entries: [], total: 0 };

/** The issue read of `key`, of `project`, as the cache holds it. */
function issueRead(key: string, project: string): IssueDetails {
  return { key, project } as unknown as IssueDetails;
}

for (const type of ["artifact.approved", "artifact.changes_requested"] as const) {
  test(`${type} refreshes both reads of rows that hold a row of its issue, and neither for another issue`, () => {
    expect(refreshed(review(type, "CORE-1"))).toMatchObject({ dismissed: true, inbox: true });
    expect(refreshed(review(type, "CORE-2"))).toMatchObject({ dismissed: false, inbox: false });
  });
}

test("artifact.version of a gate document refreshes both reads of rows that hold a row of its issue, and neither for another issue", () => {
  expect(refreshed(version("CORE-1"))).toMatchObject({ dismissed: true, inbox: true });
  expect(refreshed(version("CORE-2"))).toMatchObject({ dismissed: false, inbox: false });
});

test("ask.opened of an approval request refreshes both reads of rows of its issue whoever holds a reply, and the Dismissed read of no other issue", () => {
  for (const logins of [[], [VIEWER]]) {
    expect(refreshed(ask("ask.opened", "CORE-1", { logins })), `logins ${logins}`).toMatchObject({
      dismissed: true,
      inbox: true,
    });
  }
  // The Inbox read of another issue's request is the unnarrowed refresh that brings a new ask
  // in, which the next case reads.
  expect(refreshed(ask("ask.opened", "CORE-2")).dismissed).toBe(false);
});

test("ask.resolved of a request no review decided refreshes both reads of rows of its issue whoever holds a reply, and neither for another issue", () => {
  for (const logins of [[], [VIEWER]]) {
    expect(refreshed(ask("ask.resolved", "CORE-1", { logins })), `logins ${logins}`).toMatchObject({
      dismissed: true,
      inbox: true,
    });
  }
  expect(refreshed(ask("ask.resolved", "CORE-2"))).toMatchObject({
    dismissed: false,
    inbox: false,
  });
});

test("ask.opened of a question still refreshes the Inbox read whatever it holds", () => {
  expect(refreshed(ask("ask.opened", "CORE-2", { kind: "question" })).inbox).toBe(true);
  expect(
    refreshed(ask("ask.opened", "CORE-2", { kind: "question" }), { dismissed: EMPTY, inbox: EMPTY })
      .inbox
  ).toBe(true);
});

test("project.updated refreshes both reads of rows that hold a row of its project and every issue read of it, and neither of the two reads for another project", () => {
  const core = issueRead("CORE-1", "CORE");
  expect(refreshed(projectUpdated("CORE"), { issues: [core] })).toEqual({
    dismissed: true,
    inbox: true,
    issues: [true],
  });
  // Every issue read refreshes on any project's update, whatever project it holds: the update can
  // change that project's default repository, which each issue naming no repository resolves to,
  // and not every issue read's key names its project (`ownerTargets`). The approval gate's own
  // refresh is narrowed to the project; the Inbox and Dismissed reads stay put.
  expect(refreshed(projectUpdated("OTHER"), { issues: [core] })).toEqual({
    dismissed: false,
    inbox: false,
    issues: [true],
  });
});

test("project.updated refreshes an issue read of its project the cache holds while the Inbox holds no row of it", () => {
  const unlisted = issueRead("CORE-9", "CORE");
  expect(
    refreshed(projectUpdated("CORE"), { dismissed: EMPTY, inbox: EMPTY, issues: [unlisted] })
  ).toEqual({ dismissed: false, inbox: false, issues: [true] });
});
