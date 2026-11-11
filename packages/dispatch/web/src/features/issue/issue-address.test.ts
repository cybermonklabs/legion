import { expect, test } from "bun:test";

import { conversationStatePath, issueAddress, pathToTarget, presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";

function node(id: string, type: NodeType, over: Partial<TreeNode> = {}): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    id,
    question: false,
    replaced: false,
    text: id,
    transcluded: false,
    type,
    waiting: false,
    ...over,
  };
}

const askNode = node("spec#b-q-open", "ask", {
  reference: "dispatch://CORE-1/ask/ask-9",
});
const commentNode = node("comment-7", "comment", {
  reference: "dispatch://CORE-1/comment/comment-7",
});
const tree = node("CORE-1", "issue", {
  children: [node("spec", "document", { children: [askNode] }), commentNode],
});

test("each preset's address is the one its tab carries", () => {
  expect(presetPath("CORE-1", "tree")).toBe("/issues/CORE-1");
  expect(presetPath("CORE-1", "checks")).toBe("/issues/CORE-1/verification");
  expect(presetPath("CORE-1", "spec")).toBe("/issues/CORE-1/spec");
});

// Two of the conversation's four states are addresses of their own, so picking one is
// navigation - but only from an address those two can stand in for. From a preset the move
// would drop the preset, the `?q=` that narrows it and the document it has open, so there the
// state is the panel's alone (R19).
test("a picked state opens an address only where the state is the address", () => {
  expect(conversationStatePath({ key: "CORE-1", kind: "issue" }, "everything")).toBe(
    "/issues/CORE-1/activity"
  );
  expect(conversationStatePath({ key: "CORE-1", kind: "activity" }, "words")).toBe(
    "/issues/CORE-1/conversation"
  );
  expect(conversationStatePath({ key: "CORE-1", kind: "conversation" }, "everything")).toBe(
    "/issues/CORE-1/activity"
  );
  // The two states that name no address never open one, wherever they are picked.
  expect(conversationStatePath({ key: "CORE-1", kind: "issue" }, "questions")).toBeUndefined();
  expect(conversationStatePath({ key: "CORE-1", kind: "activity" }, "pinned")).toBeUndefined();
  // Every address that opens something besides the conversation keeps it, in every state.
  const elsewhere = [
    { key: "CORE-1", kind: "spec" },
    { key: "CORE-1", kind: "plan" },
    { key: "CORE-1", kind: "verification" },
    { key: "CORE-1", kind: "decisions" },
    { key: "CORE-1", kind: "children" },
    { key: "CORE-1", kind: "collaborators" },
    { key: "CORE-1", kind: "artifacts" },
    { key: "CORE-1", kind: "artifact", slug: "plan-md" },
  ] as const;
  for (const route of elsewhere) {
    for (const state of ["everything", "pinned", "questions", "words"] as const) {
      expect(conversationStatePath(route, state), `${route.kind} at ${state}`).toBeUndefined();
    }
  }
});

test("the address says which preset it opens and which node it names", () => {
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "", false).preset).toBe("tree");
  expect(issueAddress({ key: "CORE-1", kind: "verification" }, "", "", false).preset).toBe(
    "checks"
  );
  // A route naming the issue's own spec by its slug is the Spec preset's address too.
  expect(
    issueAddress({ key: "CORE-1", kind: "artifact", slug: "spec" }, "?v=2", "", true).preset
  ).toBe("spec");
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "?ask=ask-9", "", false).target).toEqual({
    id: "ask-9",
    kind: "item",
  });
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-r3", false).target).toEqual({
    id: "r3",
    kind: "block",
  });
  // An ask or a comment a conversation address names by its own route is a turn, not an item
  // beside a document: the conversation places the reader on it and the tree leaves the focus
  // alone (`IssueBody`'s `focusReveal`).
  expect(issueAddress({ id: "ask-9", key: "CORE-1", kind: "ask" }, "", "", false).target).toEqual({
    id: "ask-9",
    kind: "turn",
  });
});

test("an item is found by its own id, and an ask by the address of the ask it was indexed from", () => {
  const byId = pathToTarget(tree, { id: "comment-7", kind: "item" });
  expect(byId?.map((found) => found.id)).toEqual(["CORE-1", "comment-7"]);

  const byReference = pathToTarget(tree, { id: "ask-9", kind: "item" });
  expect(byReference?.map((found) => found.id)).toEqual(["CORE-1", "spec", "spec#b-q-open"]);

  const byBlock = pathToTarget(tree, { id: "q-open", kind: "block" });
  expect(byBlock?.map((found) => found.id)).toEqual(["CORE-1", "spec", "spec#b-q-open"]);

  expect(pathToTarget(tree, { id: "nothing", kind: "item" })).toBeUndefined();
  expect(pathToTarget(tree, undefined)).toBeUndefined();
});

// A fragment is a reader's own text and can be anything: `#b-50%` is no percent-escape, and
// `decodeURIComponent` throws a `URIError` on it. This runs in a render, so a throw here puts
// the whole issue page in its error boundary; an unreadable fragment names no block instead,
// and the address falls back to what its route names.
test("an unreadable block fragment names no block and keeps the route's own target", () => {
  expect(
    issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-50%", false).target
  ).toBeUndefined();
  expect(
    issueAddress({ key: "CORE-1", kind: "artifact", slug: "plan-md" }, "", "#b-50%", false).target
  ).toEqual({ id: "plan-md", kind: "node" });
  // An escape that does read is the block it names, decoded.
  expect(issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-a%20b", false).target).toEqual({
    document: undefined,
    id: "a b",
    kind: "block",
  });
});

// A block id is the author's and unique inside its own document alone, so an address naming a
// document resolves inside that document: `/issues/CORE-1/artifacts/plan-md#b-t1` is the plan's
// `t1` and never the spec's. An address naming no document - the issue's own, with a fragment -
// has only the block id to go on.
test("a block of a document's address is found in that document, not in another one's", () => {
  const specTask = node("spec#b-t1", "plan_task");
  const planTask = node("plan-md#b-t1", "plan_task");
  const documents = node("CORE-1", "issue", {
    children: [
      node("spec", "document", { children: [specTask] }),
      node("plan-md", "document", { children: [planTask] }),
    ],
  });

  const address = issueAddress(
    { key: "CORE-1", kind: "artifact", slug: "plan-md" },
    "",
    "#b-t1",
    false
  );
  expect(address.target).toEqual({ document: "plan-md", id: "t1", kind: "block" });
  expect(pathToTarget(documents, address.target)?.map((found) => found.id)).toEqual([
    "CORE-1",
    "plan-md",
    "plan-md#b-t1",
  ]);
  // A document the tree does not hold that block of answers nothing, rather than another
  // document's block of the same id.
  expect(pathToTarget(documents, { document: "verification-md", id: "t1", kind: "block" })).toBe(
    undefined
  );
  // The issue's own address with a fragment names no document: the first node carrying the
  // block id answers, which is the behaviour every such address has always had.
  expect(
    pathToTarget(
      documents,
      issueAddress({ key: "CORE-1", kind: "issue" }, "", "#b-t1", false).target
    )?.map((found) => found.id)
  ).toEqual(["CORE-1", "spec", "spec#b-t1"]);
});
