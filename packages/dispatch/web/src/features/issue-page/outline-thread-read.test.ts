import { expect, test } from "bun:test";

import type { NodeAnswer, TreeNode } from "./issue-tree";
import { threadMarks } from "./outline-thread-read";
import type { NodeType } from "./page-words";

const KEY = "CORE-1";

/** A node of the outline as `threadMarks` reads it: its kind, its address and its time. */
function node(
  type: NodeType,
  id: string,
  at: string,
  children: readonly TreeNode[] = [],
  answer?: NodeAnswer
): TreeNode {
  const kind = type === "issue" ? undefined : type;
  return {
    alsoUnder: [],
    ...(answer === undefined ? {} : { answer }),
    at,
    children,
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    id,
    question: false,
    ...(kind === undefined ? {} : { reference: `dispatch://${KEY}/${kind}/${id}` }),
    replaced: false,
    text: id,
    transcluded: false,
    type,
    waiting: false,
  } as TreeNode;
}

// Reading a thread on the issue page marks its Inbox row read, as the panel's list of the
// conversation did before the outline replaced it: each thread is marked once, on the row of its
// newest write, under the thread's own id (the head comment, message or ask).
test("each thread of the outline is marked on its newest write's row, under the thread's id", () => {
  const answer: NodeAnswer = {
    at: "2026-11-15T10:09:00Z",
    by: { id: "alice", kind: "user" },
    selected: [],
    text: "Yes.",
  };
  const root = node("issue", KEY, "2026-11-15T09:00:00Z", [
    node("comment", "c1", "2026-11-15T10:00:00Z", [
      node("comment", "c2", "2026-11-15T10:05:00Z", [
        node("comment", "c3", "2026-11-15T10:07:00Z"),
      ]),
      node("comment", "c4", "2026-11-15T10:06:00Z"),
    ]),
    node(
      "ask",
      "a1",
      "2026-11-15T10:01:00Z",
      [node("comment", "c5", "2026-11-15T10:08:00Z")],
      answer
    ),
    node(
      "ask",
      "a2",
      "2026-11-15T10:02:00Z",
      [node("comment", "c6", "2026-11-15T10:10:00Z")],
      answer
    ),
    node("message", "m1", "2026-11-15T10:03:00Z"),
  ]);

  expect(Object.fromEntries(threadMarks(root))).toEqual({
    // The reply to a reply is the comment thread's newest write.
    c3: { at: "2026-11-15T10:07:00Z", issueKey: KEY, threadId: "c1" },
    // The answer, newer than the ask's reply, is drawn in the ask's own row.
    a1: { at: "2026-11-15T10:09:00Z", issueKey: KEY, threadId: "a1" },
    // A reply newer than the answer.
    c6: { at: "2026-11-15T10:10:00Z", issueKey: KEY, threadId: "a2" },
    // A message with no reply has nothing past its head, and marks nothing.
  });
});
