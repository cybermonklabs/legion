import { expect, test } from "bun:test";

import { messageCorrectionFields } from "../../__tests__/message-fixture";
import type { Actor, Ask, Comment, MessageRead } from "../../api/types";
import { agentsOnlyIds } from "./agents-only";

/**
 * Which comments and messages the history in the panel draws, each thread whole, and the outline
 * none of: a message thread the Legion daemon began, whoever wrote in it, and a comment thread
 * tied to no text that only agents wrote in.
 */

const PERSON: Actor = { id: "alice", kind: "user" };
const AGENT: Actor = { id: "planner", kind: "session" };
const REVIEWER: Actor = { id: "reviewer", kind: "session" };
/** The Legion daemon of the issue's project, as it writes. */
const DAEMON: Actor = { id: "legion-daemon:CORE", kind: "session" };

/** Where a comment's head is tied to the words of a document. */
const TIED = {
  artifact_id: "spec",
  block_id: null,
  mark_id: "mark-1",
  orphaned: false,
  quote: "The words it is tied to.",
  version: 1,
};

type Turn = Pick<Comment, "anchor" | "ask_id" | "author" | "id" | "reply_to">;

/** A comment of the issue by `author`, a thread's head tied to no text unless `over` says. */
function comment(id: string, author: Actor, over: Partial<Turn> = {}): Turn {
  return { anchor: null, ask_id: null, author, id, reply_to: null, ...over };
}

/** A message of the issue by `author`, with no target unless `over` says. */
function message(
  id: string,
  author: Actor,
  over: Partial<MessageRead["message"]> = {}
): MessageRead["message"] {
  return {
    author,
    body: `Message ${id}.`,
    broadcast_id: null,
    created_at: "2026-11-15T09:00:00Z",
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: null,
    issue_key: "CORE-1",
    reply_block: null,
    target: null,
    ...over,
  };
}

/** The message thread of `root` and `replies`, as the server lists it. */
function thread(root: MessageRead["message"], ...replies: MessageRead["message"][]): MessageRead {
  return { message: root, replies };
}

/** The ids in the set for these reads, sorted. */
function ids(
  comments: readonly Turn[],
  messages: readonly MessageRead[],
  asks: readonly Pick<Ask, "id" | "kind">[] = []
): string[] {
  return [...agentsOnlyIds(comments, messages, asks)].sort();
}

test("a thread whose root message the Legion daemon wrote is in the set with every message of it, a person's reply included", () => {
  const root = message("daemon", DAEMON);
  const agent = message("agent-reply", AGENT, { in_reply_to: "daemon" });
  const person = message("person-reply", PERSON, { in_reply_to: "daemon" });

  expect(ids([], [thread(root, agent)])).toEqual(["agent-reply", "daemon"]);
  expect(
    ids([], [thread(root, agent, person)]),
    "a person's reply took the daemon's thread out of the set"
  ).toEqual(["agent-reply", "daemon", "person-reply"]);
});

test("an agent's untargeted note and a thread whose root targets a role are not in the set, with or without a person's reply", () => {
  const note = message("note", AGENT);
  const toRole = message("to-role", AGENT, { target: "role:legion-p-core-1-reviewer" });
  const roleReply = message("role-reply", REVIEWER, { in_reply_to: "to-role" });
  const noteAnswered = message("note-answered", PERSON, { in_reply_to: "note" });
  const roleAnswered = message("role-answered", PERSON, { in_reply_to: "to-role" });

  expect(ids([], [thread(note), thread(toRole, roleReply)])).toEqual([]);
  expect(ids([], [thread(note, noteAnswered), thread(toRole, roleReply, roleAnswered)])).toEqual(
    []
  );
});

// A reply never carries an anchor of its own (the server refuses one), so whether a comment
// thread is tied to text is its head's anchor. The comments under an approval request have the
// ask at their head and no comment, and no anchor keeps them out.
test("an agents' comment thread whose head has an anchor is not in the set, and one under an approval request is", () => {
  const tied = comment("tied", AGENT, { anchor: TIED });
  const tiedReply = comment("tied-reply", REVIEWER, { reply_to: "tied" });
  const underRequest = comment("under-request", AGENT, { ask_id: "approval-1" });

  expect(
    ids([tied, tiedReply, underRequest], [], [{ id: "approval-1", kind: "approval" }])
  ).toEqual(["under-request"]);
});

// The history draws a comment thread from its head, and whether a head is tied to text is known
// only from the head itself: replies whose head the read does not hold stay in the outline.
test("an agents' post is in the set until a person writes in it, and replies whose head is not read are not", () => {
  const post = comment("post", AGENT);
  const reply = comment("post-reply", REVIEWER, { reply_to: "post" });
  const person = comment("person-reply", PERSON, { reply_to: "post" });
  const headless = comment("headless-reply", AGENT, { reply_to: "unread-head" });

  expect(ids([post, reply, headless], [])).toEqual(["post", "post-reply"]);
  expect(ids([post, reply, person, headless], [])).toEqual([]);
});
