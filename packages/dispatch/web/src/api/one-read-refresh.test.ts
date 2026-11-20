import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { issueAsk } from "../features/inbox/inbox-fixture";
import { agentRow, replyRow } from "../features/threads/replies-fixture";
import { eventRefreshTargets } from "./event-refresh";
import { meInboxQuery } from "./queries";
import { refreshQueries } from "./query-refresh";
import type { DispatchEvent, MeInboxEntry, MeInboxResponse, MessageRead } from "./types";

/**
 * What refreshes the one read behind the Inbox's opt-in (`meInboxQuery`): it holds the reader's
 * half of the Inbox read, their threads and each Agent row's conversation, so an event that
 * refreshes one of those refreshes it, by the same test over the rows of that read it holds - an
 * issue's retitle where it holds an ask or a thread of that issue and not otherwise, and a
 * session's reply where it holds an Agent row of that session.
 */

function event(
  type: DispatchEvent["type"],
  payload: Record<string, unknown>,
  overrides: Partial<DispatchEvent> = {}
): DispatchEvent {
  return {
    actor: { id: "planner", kind: "session" },
    created_at: "2026-11-16T00:00:00Z",
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

/** A retitle of CORE-1, as the server writes it. */
const RETITLE = event("issue.updated", {
  assignee: "alice@example.com",
  blocked_by: [],
  changed: ["title"],
  closed_at: null,
  components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
  key: "CORE-1",
  parent: null,
  priority: 1,
  project: "CORE",
  status: "in_progress",
  title: "Renamed",
});

/** A session's reply to a direct message of the reader's, which names nobody's threads. */
function sessionReply(session: string): DispatchEvent {
  return event(
    "message.answered",
    { target: `session:${session}`, thread_logins: [] },
    { issue_key: null }
  );
}

function entry(id: string, carried: Partial<MeInboxEntry>): MeInboxEntry {
  return {
    moment: "2026-11-16T00:00:00Z",
    node: { id, kind: "comment" },
    relations: [{ at: "2026-11-16T00:00:00Z", direction: "inbound", kind: "delivered_to" }],
    ...carried,
  };
}

function answer(entries: MeInboxEntry[]): MeInboxResponse {
  return {
    as_of: "2026-11-16T00:00:00Z",
    entries,
    horizon: 1,
    limit: 0,
    offset: 0,
    person: "alice",
    total: entries.length,
  };
}

const CONVERSATION: MessageRead = {
  message: {
    author: { id: "alice", kind: "user" },
    body: "Please check the staging gate.",
    broadcast_id: null,
    created_at: "2026-11-16T00:00:00Z",
    deliveries: [],
    id: "message-1",
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: "session:s1",
  },
  replies: [],
};

/** Whether `event`'s refresh marks the one read holding `held` stale. */
function refreshesOneRead(event: DispatchEvent, held: MeInboxResponse): boolean {
  const client = new QueryClient();
  client.setQueryData(meInboxQuery().queryKey, held);
  refreshQueries(client, eventRefreshTargets(event, "alice@example.com"));
  return client.getQueryState(meInboxQuery().queryKey)?.isInvalidated === true;
}

test("a retitle refreshes the one read where it holds an ask of that issue", () => {
  expect(refreshesOneRead(RETITLE, answer([entry("ask-a", { ask: issueAsk() })]))).toBe(true);
  expect(
    refreshesOneRead(
      RETITLE,
      answer([
        entry("ask-b", {
          ask: issueAsk({
            id: "ask-b",
            issue: { assignee: "alice", key: "CORE-2", title: "Other" },
            issue_key: "CORE-2",
          }),
        }),
      ])
    )
  ).toBe(false);
});

test("a retitle refreshes the one read where it holds a thread of that issue", () => {
  expect(refreshesOneRead(RETITLE, answer([entry("c1", { thread: replyRow("c1") })]))).toBe(true);
  expect(
    refreshesOneRead(
      RETITLE,
      answer([
        entry("c2", { thread: replyRow("c2", { issue: { key: "CORE-2", title: "Other" } }) }),
      ])
    )
  ).toBe(false);
});

test("a session's reply refreshes the one read where it holds that session's Agent row", () => {
  const held = answer([
    entry("message-1", { agent: { ...agentRow("message-1"), conversation: CONVERSATION } }),
  ]);
  expect(refreshesOneRead(sessionReply("s1"), held)).toBe(true);
  expect(refreshesOneRead(sessionReply("s2"), held)).toBe(false);
});
