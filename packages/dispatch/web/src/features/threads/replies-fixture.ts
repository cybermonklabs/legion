import type { QueryClient } from "@tanstack/react-query";

import { meInboxOwnerQuery, whoAmIQuery } from "../../api/queries";
import type {
  MeInboxAgentRow,
  MeInboxResponse,
  MessageRead,
  MyReplyAgentRow,
  MyReplyRow,
  MyReplyThreadRow,
} from "../../api/types";
import { oneReadAnswer } from "../inbox/inbox-fixture";
import type { RepliesOwner } from "./useMarkThreadRead";

/** One unread thread row for `threadId`, as the one read answers a Reply row: read through a
 *  mark older than its reply, at a version of its own - not 0, so a test cannot pass on a missing
 *  field. An Agent row is a shape of its own, built below (`agentRow`, `plannerAgentRow`, and
 *  `withConversation` for the one read's answer of it). */
export function replyRow(
  threadId: string,
  overrides: Partial<MyReplyThreadRow> = {}
): MyReplyThreadRow {
  return {
    issue: { key: "CORE-1", title: "Core" },
    kind: "comment",
    read_through: "2026-10-11T00:00:00Z",
    ref: `/issues/CORE-1/comments/${threadId}`,
    reply: {
      author: { id: "s1", kind: "session" },
      created_at: "2026-10-12T00:00:00Z",
      first_line: "Done.",
      id: `${threadId}-reply`,
      kind: "reply",
    },
    subject: "Root",
    thread_id: threadId,
    unread: true,
    unread_count: 1,
    version: 3,
    ...overrides,
  };
}

/** One unread Agent row for `threadId`, the other shape the route answers: a message the reader
 *  sent a session, with that session's reply. It is on no issue and in no document, so it
 *  carries no owner, no priority, no project and no read mark. */
export function agentRow(
  threadId: string,
  overrides: Partial<MyReplyAgentRow> = {}
): MyReplyAgentRow {
  const { reply, subject, unread, unread_count } = replyRow(threadId);
  return {
    kind: "agent",
    ref: "/agents/s1/live",
    reply,
    session: { id: "s1", name: "Planner" },
    subject,
    thread_id: threadId,
    unread,
    unread_count,
    unread_reply_ids: [reply.id],
    ...overrides,
  };
}

/** The Agent row the Inbox's own tests draw: the row above as a message the reader sent the
 *  planner session, answered by that session, with the one reply they have not read
 *  (`reply-1`). */
export function plannerAgentRow(overrides: Partial<MyReplyAgentRow> = {}): MyReplyAgentRow {
  return agentRow("message-1", {
    ref: "/agents/planner/live",
    reply: {
      author: { id: "planner", kind: "session" },
      created_at: "2026-10-12T01:00:00Z",
      first_line: "Looked at it.",
      id: "reply-1",
      kind: "reply",
    },
    session: { id: "planner", name: "Planner" },
    subject: "Have a look at the build",
    unread_reply_ids: ["reply-1"],
    ...overrides,
  });
}

/** `row` as the one read answers an Agent row: with the conversation with its session that its
 *  reading pane draws (`agentConversation`), which nothing reads again. */
export function withConversation(row: MyReplyAgentRow, conversation: MessageRead): MeInboxAgentRow {
  return { ...row, conversation };
}

/** The conversation the one read carries for `agentRow("message-1")`: the reader's message to
 *  session `s1`, which nobody has answered yet. */
export const AGENT_CONVERSATION: MessageRead = {
  message: {
    author: { id: "alice", kind: "user" },
    body: "Please check the staging gate.",
    broadcast_id: null,
    created_at: "2026-10-12T00:00:00Z",
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    id: "message-1",
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: "session:s1",
  },
  replies: [],
};

/** The one read's `unread` answer narrowed to one owner, holding the rows `rows`, each an entry of
 *  its thread at its newest reply: what every thread mounted on that owner reads to learn whether
 *  it is unread (`meInboxOwnerQuery`). */
export function unreadSetAnswer(rows: readonly MyReplyRow[]): MeInboxResponse {
  return { ...oneReadAnswer(rows, { asOf: "2026-10-12T00:00:00Z" }), limit: 200 };
}

/** Seeds the signed-in viewer (alice) and `owner`'s unread threads, so every thread mounted on
 *  that owner reads them from the cache rather than a request. */
export function seedUnread(
  queryClient: QueryClient,
  owner: RepliesOwner,
  threadIds: readonly string[]
): void {
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  queryClient.setQueryData(
    meInboxOwnerQuery(owner).queryKey,
    unreadSetAnswer(threadIds.map((id) => replyRow(id)))
  );
}
