import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { api } from "../../../api/client";
import type { Artifact } from "../../../api/types";
import { parseNodeReference } from "../../refs/routes";
import type { DocumentAdmissions } from "../document-admission";
import { documentOfNode } from "../issue-artifacts";
import type { TreeNode } from "../issue-tree";
import type { NodeType } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { treeAnswer } from "../tree-read/tree-fixtures";
import { outlineQuery, viewKey } from "../tree-read/tree-read";
import { nodeEditable } from "../useNodeEditing";
import {
  type RoomFacts,
  type RoomTarget,
  roomTarget,
  takesCaret,
  useRoomFacts,
} from "./room-target";

/**
 * Where Enter puts the caret on a node: on a converted issue, in the one room, through the
 * fragment that holds the node's text - its document's, or the tree artifact's own for a node
 * written into the issue - and nowhere for a node of another issue, for the conversation or for a
 * result; on an issue the conversion has not reached, in its document's room, as
 * `useNodeEditing.ts` does without a room.
 */

const KEY = "CORE-1";
const TREE = "artifact-tree";
const SPEC = "spec";
const VERIFICATION = "verification-md";

function artifact(id: string, slug: string, name: string, primary = false): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: KEY,
    kind: "doc",
    name,
    primary,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
  };
}

/** The issue's two documents by slug, as `issue-artifacts.ts`'s `artifactsBySlug` holds them. */
const ARTIFACTS: ReadonlyMap<string, Artifact> = new Map(
  [
    artifact("artifact-spec", SPEC, "spec.md", true),
    artifact("artifact-verification", VERIFICATION, "verification.md"),
  ].map((one) => [one.slug, one])
);

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

/** A block of one of `key`'s documents, by its node id and its own address, as `tree-drafts.ts`
 *  draws one. */
function block(slug: string, id: string, type: NodeType, key = KEY): TreeNode {
  return node(blockNodeId(slug, id), type, [], {
    reference: `dispatch://${key}/artifact/${slug}#b-${id}`,
  });
}

/** The issue's tree read as the room reads it: converted or not, its tree artifact, and a view of
 *  no other issue. */
function room(converted: boolean, tree: string | null = TREE): RoomFacts {
  return { converted, issueKey: KEY, tree, viewOf: null };
}

/** A statement of the spec and a check of verification.md. */
const STATEMENT = block(SPEC, "r1", "requirement");
const CHECK = block(VERIFICATION, "c1", "check");

/** A criterion written into the issue, as the tree read drafts it: its bare read id, which is its
 *  block id in the tree artifact, and no address, since the tree read writes no address for a
 *  node no document holds. */
const CRITERION = node("k1", "criterion");

/** An ask written as a block of the spec, which the ask read placed: its address is the ask's. */
const ASK_BLOCK = node(blockNodeId(SPEC, "q1"), "ask", [], {
  reference: `dispatch://${KEY}/ask/ask-1`,
});

/** An ask block of the spec no ask read placed: its address is still the block's. */
const UNREAD_ASK_BLOCK = block(SPEC, "q2", "ask");

const ASK = node("ask-2", "ask", [], { reference: `dispatch://${KEY}/ask/ask-2` });
const COMMENT = node("comment-1", "comment", [], {
  reference: `dispatch://${KEY}/comment/comment-1`,
});
const MESSAGE = node("message-1", "message", [], {
  reference: `dispatch://${KEY}/message/message-1`,
});

/** A result of this issue's check c1, which the tree read drafts with no address. */
const RESULT = node("result-1", "result");

/** A result of a check of another issue, addressed in that issue. */
const BORROWED_RESULT = node("result-9", "result", [], {
  reference: `dispatch://CORE-2/artifact/${VERIFICATION}#b-result-9`,
});

/** Today's rule (`useNodeEditing.ts` without a room): the document `documentOfNode` finds and
 *  the node's block of it, and no text where it finds none. */
function todaysRoom(one: TreeNode): RoomTarget {
  const place = documentOfNode(one.id, ARTIFACTS);
  return place === undefined
    ? { report: "no_text" }
    : { artifactId: place.artifact.id, blockId: place.blockId };
}

test("a converted node's room is its fragment's", () => {
  expect(roomTarget(STATEMENT, ARTIFACTS, room(true))).toEqual({
    artifactId: "artifact-spec",
    blockId: "r1",
  });
  expect(roomTarget(CHECK, ARTIFACTS, room(true))).toEqual({
    artifactId: "artifact-verification",
    blockId: "c1",
  });
});

test("a node written into the issue edits in the issue's own fragment", () => {
  expect(roomTarget(CRITERION, ARTIFACTS, room(true))).toEqual({ artifactId: TREE, blockId: "k1" });
  // An issue whose tree read names no tree artifact has no fragment of its own to edit it in.
  expect(roomTarget(CRITERION, ARTIFACTS, room(true, null))).toEqual({ report: "no_text" });
});

test("a node of another issue is read-only", () => {
  // A borrowed check whose document's slug is one of this issue's: its id alone would name this
  // issue's verification.md.
  const borrowed = block(VERIFICATION, "c9", "check", "CORE-2");
  expect(documentOfNode(borrowed.id, ARTIFACTS)).toBeDefined();
  const task = block("plan-md", "t1", "plan_task", "CORE-2");
  for (const one of [borrowed, task]) {
    expect(roomTarget(one, ARTIFACTS, room(true))).toEqual({ report: "issue_above" });
    // The room knows the issue whether or not the conversion reached it.
    expect(roomTarget(one, ARTIFACTS, room(false))).toEqual({ report: "issue_above" });
  }
});

test("an unconverted node edits in its document's room", () => {
  const facts = room(false);
  expect(roomTarget(STATEMENT, ARTIFACTS, facts)).toEqual({
    artifactId: "artifact-spec",
    blockId: "r1",
  });
  expect(roomTarget(CHECK, ARTIFACTS, facts)).toEqual({
    artifactId: "artifact-verification",
    blockId: "c1",
  });
  expect(roomTarget(ASK_BLOCK, ARTIFACTS, facts)).toEqual({
    artifactId: "artifact-spec",
    blockId: "q1",
  });
  expect(roomTarget(CRITERION, ARTIFACTS, facts)).toEqual({ report: "no_text" });
  expect(roomTarget(COMMENT, ARTIFACTS, facts)).toEqual({ report: "no_text" });
  const nodes = [STATEMENT, CHECK, ASK_BLOCK, UNREAD_ASK_BLOCK, CRITERION, ASK, COMMENT, MESSAGE];
  for (const one of nodes) {
    expect(roomTarget(one, ARTIFACTS, facts)).toEqual(todaysRoom(one));
  }
});

test("an ask, a comment and a message take no caret on a converted issue", () => {
  for (const one of [ASK, ASK_BLOCK, UNREAD_ASK_BLOCK, COMMENT, MESSAGE]) {
    expect(roomTarget(one, ARTIFACTS, room(true))).toEqual({ report: "no_text" });
  }
});

test("a result takes no caret", () => {
  // No room holds a result's text, and a borrowed one says so too rather than naming its issue,
  // whether or not the conversion reached this issue.
  expect(parseNodeReference(BORROWED_RESULT.reference ?? "")).toMatchObject({ key: "CORE-2" });
  for (const one of [RESULT, BORROWED_RESULT]) {
    for (const facts of [room(true), room(false)]) {
      expect(roomTarget(one, ARTIFACTS, facts)).toEqual({ report: "no_text" });
    }
  }
});

test("a node written into the issue takes a caret", () => {
  const none: DocumentAdmissions = new Map();
  // Enter puts the caret in a criterion in the tree artifact's own fragment, so its row takes one
  // by a press too, on an open issue whose room for that artifact admitted the page to write.
  expect(takesCaret(CRITERION, ARTIFACTS, room(true), false, none)).toBe(true);
  expect(takesCaret(CRITERION, ARTIFACTS, room(true), true, none)).toBe(false);
  expect(takesCaret(CRITERION, ARTIFACTS, room(true), false, new Map([[TREE, true]]))).toBe(false);
  // The conversation's text is no fragment's on a converted issue.
  expect(takesCaret(COMMENT, ARTIFACTS, room(true), false, none)).toBe(false);
  // With no room facts the row takes a caret where `nodeEditable` says it does: a statement of a
  // document, and never a criterion, which no document holds.
  expect(takesCaret(STATEMENT, ARTIFACTS, undefined, false, none)).toBe(true);
  expect(takesCaret(CRITERION, ARTIFACTS, undefined, false, none)).toBe(false);
  const specReadOnly: DocumentAdmissions = new Map([["artifact-spec", true]]);
  for (const one of [STATEMENT, CHECK, ASK_BLOCK, CRITERION, COMMENT, RESULT]) {
    for (const closed of [false, true]) {
      for (const admissions of [none, specReadOnly]) {
        expect(takesCaret(one, ARTIFACTS, undefined, closed, admissions)).toBe(
          nodeEditable(documentOfNode(one.id, ARTIFACTS), closed, admissions)
        );
      }
    }
  }
});

/** A criterion and a paragraph written into the issue's own tree, as the tree read names a block
 *  of the tree artifact: by its place in the issue's tree, `<KEY>/tree#<block id>`, and with no
 *  address, since no document holds it. */
const TREE_CRITERION = node(`${KEY}/tree#k2`, "criterion");
const TREE_PROSE = node(`${KEY}/tree#p1`, "prose");

/** A paragraph of the issue's own tree the read names by its bare block id. */
const BARE_PROSE = node("p2", "prose");

/** A paragraph of another issue's tree. */
const OTHER_TREE_PROSE = node("CORE-2/tree#p9", "prose");

test("a criterion and a prose block of the issue's own tree edit in its tree artifact at their block id", () => {
  expect(roomTarget(TREE_CRITERION, ARTIFACTS, room(true))).toEqual({
    artifactId: TREE,
    blockId: "k2",
  });
  expect(roomTarget(TREE_PROSE, ARTIFACTS, room(true))).toEqual({
    artifactId: TREE,
    blockId: "p1",
  });
  expect(roomTarget(BARE_PROSE, ARTIFACTS, room(true))).toEqual({
    artifactId: TREE,
    blockId: "p2",
  });
  // A block of another issue's tree is that issue's, read-only here.
  expect(roomTarget(OTHER_TREE_PROSE, ARTIFACTS, room(true))).toEqual({ report: "issue_above" });
  // With no tree artifact, or on an issue the conversion has not reached, no fragment holds it.
  for (const one of [TREE_CRITERION, TREE_PROSE]) {
    expect(roomTarget(one, ARTIFACTS, room(true, null))).toEqual({ report: "no_text" });
    expect(roomTarget(one, ARTIFACTS, room(false))).toEqual({ report: "no_text" });
  }
  // A prose block of a document is edited in that document's fragment.
  expect(roomTarget(block(SPEC, "p3", "prose"), ARTIFACTS, room(true))).toEqual({
    artifactId: "artifact-spec",
    blockId: "p3",
  });
});

test("a criterion and a prose block of the issue's own tree take a caret", () => {
  const none: DocumentAdmissions = new Map();
  for (const one of [TREE_CRITERION, TREE_PROSE, BARE_PROSE]) {
    expect(takesCaret(one, ARTIFACTS, room(true), false, none), one.id).toBe(true);
    expect(takesCaret(one, ARTIFACTS, room(true), true, none), one.id).toBe(false);
    expect(takesCaret(one, ARTIFACTS, room(true), false, new Map([[TREE, true]])), one.id).toBe(
      false
    );
    // With no room no document holds it, so it takes no typing.
    expect(takesCaret(one, ARTIFACTS, undefined, false, none), one.id).toBe(false);
  }
  expect(takesCaret(OTHER_TREE_PROSE, ARTIFACTS, room(true), false, none)).toBe(false);
});

/** A converted issue's tree read, as the page holds it: its tree artifact and a view of no other
 *  issue. */
const HELD = treeAnswer([], [], { converted: true, issue: KEY, tree: TREE, view_of: null });

/** `useRoomFacts` of `issue` under `client`, its issue changed by `rerender`. */
function roomFactsOf(client: QueryClient, issue: string | undefined = KEY) {
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
  return renderHook<RoomFacts | undefined, { key: string | undefined }>(
    ({ key }) => useRoomFacts(key),
    { initialProps: { key: issue }, wrapper }
  );
}

test("the room facts are those of the tree read the cache holds, one object while they stand", () => {
  const client = new QueryClient();
  const read = spyOn(api, "getIssueTree");
  try {
    const hook = roomFactsOf(client);
    expect(hook.result.current, "facts with no tree read held").toBeUndefined();
    const graph = outlineQuery(KEY, "graph").queryKey;
    act(() => {
      client.setQueryData(graph, HELD);
    });
    expect(hook.result.current).toEqual({
      converted: true,
      issueKey: KEY,
      tree: TREE,
      viewOf: null,
    });
    const first = hook.result.current;
    // A new answer that leaves the facts as they were keeps the same object.
    act(() => {
      client.setQueryData(graph, { ...HELD, version: 3 });
    });
    expect(hook.result.current, "an answer with the same facts drew new facts").toBe(first);
    // One that changes them draws new facts.
    act(() => {
      client.setQueryData(graph, { ...HELD, view_of: "CORE-0" });
    });
    expect(hook.result.current).toEqual({
      converted: true,
      issueKey: KEY,
      tree: TREE,
      viewOf: "CORE-0",
    });
    // No tree read of another issue is held, and no issue has none.
    hook.rerender({ key: "CORE-2" });
    expect(hook.result.current).toBeUndefined();
    hook.rerender({ key: undefined });
    expect(hook.result.current).toBeUndefined();
    hook.unmount();
    expect(read, "the room facts read the tree themselves").not.toHaveBeenCalled();
  } finally {
    read.mockRestore();
    client.clear();
  }
});

test("the room facts are read from Conversation's read or a view where Graph's is not held", () => {
  const client = new QueryClient();
  try {
    const hook = roomFactsOf(client);
    // A query under the tree's key whose answer is no tree read gives no facts.
    act(() => {
      client.setQueryData(viewKey(KEY, "spec"), { nodes: [] });
    });
    expect(hook.result.current).toBeUndefined();
    act(() => {
      client.setQueryData(viewKey(KEY, "plan"), { ...HELD, tree: "artifact-view-tree" });
    });
    expect(hook.result.current?.tree).toBe("artifact-view-tree");
    act(() => {
      client.setQueryData(outlineQuery(KEY, "conversation").queryKey, HELD);
    });
    expect(hook.result.current?.tree, "Conversation's read is read before a view").toBe(TREE);
    act(() => {
      client.setQueryData(outlineQuery(KEY, "graph").queryKey, { ...HELD, converted: false });
    });
    expect(hook.result.current?.converted, "Graph's read is read first").toBe(false);
    act(() => {
      client.removeQueries({ exact: true, queryKey: outlineQuery(KEY, "graph").queryKey });
    });
    expect(hook.result.current?.converted).toBe(true);
    hook.unmount();
  } finally {
    client.clear();
  }
});
