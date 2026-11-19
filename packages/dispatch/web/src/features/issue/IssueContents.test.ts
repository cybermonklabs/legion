import { expect, test } from "bun:test";

import type { Actor } from "../../api/types";
import { type ContentsEntry, contentsOf } from "./IssueContents";
import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";

/**
 * The Contents list's entries (dispatch://INFRA-1196 R14): each question and each thread the
 * reader began, nested under the nearest entry above it, over a tree built by hand. The issue
 * itself is no entry: the header is the issue (the architect's ruling 5).
 */

function node(
  id: string,
  type: NodeType,
  children: readonly TreeNode[] = [],
  extra: Partial<TreeNode> = {}
): TreeNode {
  return {
    alsoUnder: [],
    children,
    chips: [],
    facts: [],
    gaps: [],
    held: children.reduce((total, child) => total + child.held + 1, 0),
    id,
    question: false,
    replaced: false,
    text: id,
    transcluded: false,
    type,
    waiting: false,
    ...extra,
  };
}

const alice: Actor = { id: "alice", kind: "user" };
const bob: Actor = { id: "bob", kind: "user" };
const agent: Actor = { id: "session-1", kind: "agent" };

/** Each entry as its id, with the entries under it after a colon. */
function shape(entries: readonly ContentsEntry[]): string[] {
  return entries.map((entry) =>
    entry.children.length === 0
      ? entry.node.id
      : `${entry.node.id}: ${shape(entry.children).join(", ")}`
  );
}

test("the list holds each question and each thread the reader began, nested, and no issue", () => {
  const root = node("CORE-1", "issue", [
    node("ask-1", "ask", [
      node("requirement-1", "requirement", [node("ask-2", "ask")]),
      node("comment-mine", "comment", [node("reply-mine", "comment", [], { author: alice })], {
        author: alice,
      }),
    ]),
    node("comment-bob", "comment", [], { author: bob }),
    node("message-agent", "message", [], { author: agent }),
    node("message-mine", "message", [], { author: alice }),
    node("ask-1-again", "ask", [], { transcluded: true }),
  ]);
  expect(shape(contentsOf(root, "alice"))).toEqual(["ask-1: ask-2, comment-mine", "message-mine"]);
});

test("a reader the page has not named yet begins no thread of the list", () => {
  const root = node("CORE-1", "issue", [node("comment-mine", "comment", [], { author: alice })]);
  expect(shape(contentsOf(root, undefined))).toEqual([]);
});

// The reader is told from the login as the server holds it, whatever its case
// (`refs/actor.ts`'s `isViewer`).
test("a thread the reader began is theirs whatever the case of their login", () => {
  const root = node("CORE-1", "issue", [node("comment-mine", "comment", [], { author: alice })]);
  expect(shape(contentsOf(root, "Alice"))).toEqual(["comment-mine"]);
});

// A node drawn under a second parent is the same node: its copy, and the branch under the copy,
// are neither listed nor counted again.
test("a transcluded copy and the branch under it are listed and counted once", () => {
  const branch = (transcluded: boolean): TreeNode =>
    node(
      "plan#b-t1",
      "plan_task",
      [node("ask-under", "ask"), node("decisions#b-d1", "decision", [], { waiting: true })],
      { transcluded }
    );
  const root = node("CORE-1", "issue", [
    node("ask-1", "ask", [
      node("spec#b-r1", "requirement", [branch(false)]),
      node("spec#b-r2", "requirement", [branch(true)]),
    ]),
  ]);
  const [entry] = contentsOf(root, "alice");
  expect(shape(entry === undefined ? [] : [entry])).toEqual(["ask-1: ask-under"]);
  expect(entry?.marks.for_review).toBe(1);
});

// R14, the 1440 px picture: each entry's marks count what its own branch holds - its
// statements, its decisions waiting for the reader's review, its child issues and its statements
// a check failed - and a node counts once, on the nearest entry above it.
test("each entry counts what its own branch holds, and a nested entry's branch is its own", () => {
  const failing = node("spec#b-r2", "requirement", [
    node("verification#b-c2", "check", [], { state: "failed" }),
  ]);
  const root = node("CORE-1", "issue", [
    node("spec#b-r1", "requirement"),
    node("CORE-2", "child_issue"),
    node("ask-1", "ask", [
      failing,
      node("decisions#b-d1", "decision", [], { waiting: true }),
      node("decisions#b-d2", "decision"),
      node("ask-2", "ask", [node("spec#b-r3", "requirement")]),
    ]),
  ]);
  const marks = (entries: readonly ContentsEntry[]): Record<string, unknown> =>
    Object.fromEntries(
      entries.flatMap((entry) => [
        [entry.node.id, entry.marks],
        ...Object.entries(marks(entry.children)),
      ])
    );
  expect(marks(contentsOf(root, "alice"))).toEqual({
    // What sits under the issue and under no entry is counted on none: the issue has no entry.
    "ask-1": { children: 0, failing: 1, for_review: 1, statements: 1 },
    "ask-2": { children: 0, failing: 0, for_review: 0, statements: 1 },
  });
});
