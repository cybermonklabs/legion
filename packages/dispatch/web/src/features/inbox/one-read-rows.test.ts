/**
 * The rows the Inbox draws of the one read's entries (`one-read-rows.ts`): an entry carrying an
 * ask, a Reply row or an Agent row becomes the `InboxListRow` the list already draws, in the
 * read's order, and an entry the page draws no row for becomes none.
 */

import { expect, test } from "bun:test";

import type { MeInboxAgentRow, MeInboxEntry, MeInboxResponse, MessageRead } from "../../api/types";
import { agentRow, replyRow } from "../threads/replies-fixture";
import { issueAsk } from "./inbox-fixture";
import { rowId } from "./list-row";
import {
  agentConversation,
  meInboxAsks,
  meInboxCachedRows,
  meInboxListRows,
  meInboxThreads,
} from "./one-read-rows";

const CONVERSATION: MessageRead = {
  message: {
    author: { id: "alice", kind: "user" },
    body: "Please check the staging gate.",
    broadcast_id: null,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    id: "message-1",
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: "session:s1",
  },
  replies: [],
};

const AGENT: MeInboxAgentRow = { ...agentRow("message-1"), conversation: CONVERSATION };

function entry(kind: string, id: string, carried: Partial<MeInboxEntry> = {}): MeInboxEntry {
  return {
    moment: "2026-10-12T00:00:00Z",
    node: { id, kind },
    relations: [{ at: "2026-10-12T00:00:00Z", direction: "inbound", kind: "delivered_to" }],
    ...carried,
  };
}

/** An answer holding one entry of each kind of row, and a mention, which is none. */
const ANSWER: MeInboxResponse = {
  as_of: "2026-10-12T00:00:01Z",
  entries: [
    entry("ask", "ask-a", { ask: issueAsk() }),
    entry("comment", "mention-1"),
    entry("comment", "c1", { thread: replyRow("c1") }),
    entry("message", "message-1", { agent: AGENT }),
  ],
  horizon: 1,
  limit: 0,
  offset: 0,
  person: "alice",
  total: 4,
};

test("each entry that carries a row is that row, in the read's order, and a mention is none", () => {
  expect(meInboxListRows(ANSWER.entries).map(rowId)).toEqual([
    "ask-a",
    "reply:c1",
    "agent:message-1",
  ]);
  expect(meInboxListRows(null)).toEqual([]);
});

test("the asks and the threads of an answer are its rows of each kind", () => {
  expect(meInboxAsks(ANSWER).map((ask) => ask.id)).toEqual(["ask-a"]);
  expect(meInboxThreads(ANSWER).map((thread) => thread.thread_id)).toEqual(["c1", "message-1"]);
  expect(meInboxAsks(undefined)).toEqual([]);
});

test("a cached read is one answer or the pages of the paged one, and anything else holds no row", () => {
  expect(meInboxCachedRows(ANSWER).map(rowId)).toEqual(["ask-a", "reply:c1", "agent:message-1"]);
  expect(
    meInboxCachedRows({ pageParams: [undefined, "next"], pages: [ANSWER, ANSWER] })
  ).toHaveLength(6);
  expect(meInboxCachedRows(3)).toEqual([]);
  expect(meInboxCachedRows(undefined)).toEqual([]);
});

test("an Agent row of the one read carries its conversation, and one of the threads' read none", () => {
  const [, , agent] = meInboxListRows(ANSWER.entries);
  expect(agent?.kind === "thread" ? agentConversation(agent.thread) : undefined).toBe(CONVERSATION);
  expect(agentConversation(agentRow("message-1"))).toBeUndefined();
});
