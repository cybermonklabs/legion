import { expect, test } from "bun:test";

import type { Artifact } from "../../../api/types";
import { parseNodeReference } from "../../refs/routes";
import { documentOfNode } from "../issue-artifacts";
import type { TreeNode } from "../issue-tree";
import type { NodeType } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { type RoomFacts, type RoomTarget, roomTarget } from "./room-target";

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
