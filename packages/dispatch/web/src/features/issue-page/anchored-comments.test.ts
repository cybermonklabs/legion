import { expect, test } from "bun:test";

import type { Artifact, ArtifactBlock, Comment, IssueDetails } from "../../api/types";
import { buildIssueTree, type TreeNode } from "./issue-tree";
import { blockNodeId } from "./tree-drafts";

/**
 * Where the outline draws a comment anchored inside a statement: under that statement, which it
 * finds by the anchor's block, mapped to the statement whose
 * range holds it in the spec's blocks read, since the server stores the lowest block holding the
 * anchor's words (`pmdoc.BlockIDForRange`) - the paragraph of a one-paragraph statement, the
 * statement itself for words across two of its paragraphs.
 */

const KEY = "CORE-1";
const SPEC = "spec";

function artifact(id: string, slug: string, primary = false): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: KEY,
    kind: "doc",
    name: `${slug}.md`,
    primary,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
  };
}

function issueDetails(artifacts: readonly Artifact[]): IssueDetails {
  return {
    advances: [],
    advances_revision: 0,
    artifacts: [...artifacts],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    decisions_artifact_id: "",
    design_gates: [],
    external_links: [],
    holder_state: null,
    key: KEY,
    labels: [],
    last_seen: null,
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: "",
    primary_artifact_id: artifacts.find((one) => one.primary)?.id ?? "",
    priority: null,
    progress: { children: null, tasks: null },
    project: "CORE",
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    takeable_at: null,
    title: "The issue page draws one box",
    updated_at: "2026-11-05T00:00:00Z",
    verification_artifact_id: "",
  };
}

/** One block of a document, at the positions `from`..`to` it covers there. */
function block(id: string, type: string, from: number, to: number): ArtifactBlock {
  return { attributes: [], from, id, references: { asks: 0, comments: 0 }, to, token: id, type };
}

/** A person's comment on the issue, anchored where `anchor` says: a document by its id and the
 *  block the server stored for the anchor's words. */
function comment(
  id: string,
  createdAt: string,
  anchor?: { readonly artifactId: string; readonly blockId: string }
): Comment {
  return {
    anchor:
      anchor === undefined
        ? null
        : {
            artifact_id: anchor.artifactId,
            block_id: anchor.blockId,
            mark_id: `mark-${id}`,
            orphaned: false,
            quote: `The words ${id} is on.`,
            version: 1,
          },
    answers: null,
    artifact_id: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: `Comment ${id}.`,
    created_at: createdAt,
    deliveries: [],
    edited_at: null,
    id,
    issue_key: KEY,
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** The node each placement of `id` sits under. */
function parentsOf(root: TreeNode, id: string): string[] {
  const found: string[] = [];
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      if (child.id === id) found.push(node.id);
      walk(child);
    }
  };
  walk(root);
  return found;
}

// R11: a comment that answers nothing else and is anchored inside a statement hangs under that
// statement, wherever in it the server stored its block; one that answers another comment keeps
// its place under what it answers; one anchored outside every statement, or in a document whose
// blocks the page has not read, stays under the issue.
test("a comment anchored inside a statement hangs under it, by the blocks read's range", () => {
  const spec = artifact("artifact-spec", SPEC, true);
  // A document of the issue whose blocks the outline does not read.
  const notes = artifact("artifact-notes", "notes-md");
  const inSpec = (blockId: string) => ({ artifactId: spec.id, blockId });
  const head = comment("c0", "2026-11-04T09:00:00Z");
  const root = buildIssueTree({
    comments: [
      head,
      // The one paragraph of the first statement, which is the block the server stores for
      // words inside a statement of one paragraph.
      comment("c1", "2026-11-04T10:00:00Z", inSpec("p1")),
      // The second statement itself, which the server stores for words across its two
      // paragraphs.
      comment("c2", "2026-11-04T11:00:00Z", inSpec("r2")),
      // A reply to `c0`, anchored inside the first statement all the same.
      { ...comment("c3", "2026-11-04T12:00:00Z", inSpec("p1")), answers: "c0", reply_to: "c0" },
      // The spec's own text, outside every statement.
      comment("c4", "2026-11-04T13:00:00Z", inSpec("p0")),
      comment("c5", "2026-11-04T14:00:00Z", { artifactId: notes.id, blockId: "n1" }),
    ],
    documents: [
      {
        artifactId: spec.id,
        blocks: [
          block("p0", "paragraph", 0, 20),
          block("r1", "requirement", 20, 60),
          block("p1", "paragraph", 21, 59),
          block("r2", "requirement", 60, 140),
          block("p2", "paragraph", 61, 99),
          block("p3", "paragraph", 99, 139),
        ],
      },
    ],
    issue: issueDetails([spec, notes]),
  });

  expect(parentsOf(root, "c1")).toEqual([blockNodeId(SPEC, "r1")]);
  expect(parentsOf(root, "c2")).toEqual([blockNodeId(SPEC, "r2")]);
  expect(parentsOf(root, "c3")).toEqual(["c0"]);
  expect(parentsOf(root, "c4")).toEqual([KEY]);
  expect(parentsOf(root, "c5")).toEqual([KEY]);
  // Each statement is a node of its own under the issue, and holds the comment on it alone.
  expect(parentsOf(root, blockNodeId(SPEC, "r1"))).toEqual([KEY]);
  expect(
    root.children
      .find((child) => child.id === blockNodeId(SPEC, "r2"))
      ?.children.map((child) => child.id)
  ).toEqual(["c2"]);
});

// The reviewer's finding 5942007779 and decision 152: only a `requirement` block of the spec is
// a statement. The plan's own `requirement` block is a block and nothing more, which nothing is
// placed under, so a comment anchored inside it stays under the issue, whether the server stored
// the block's paragraph or the block itself; one anchored inside the spec's statement still hangs
// under that statement.
test("a comment anchored inside a requirement block of the plan stays under the issue", () => {
  const spec = artifact("artifact-spec", SPEC, true);
  const plan = artifact("artifact-plan", "plan-md");
  const inPlan = (blockId: string) => ({ artifactId: plan.id, blockId });
  const planned = blockNodeId(plan.slug, "r9");
  const root = buildIssueTree({
    comments: [
      // The one paragraph of the plan's block.
      comment("c1", "2026-11-04T10:00:00Z", inPlan("q1")),
      // The plan's block itself.
      comment("c2", "2026-11-04T11:00:00Z", inPlan("r9")),
      comment("c3", "2026-11-04T12:00:00Z", { artifactId: spec.id, blockId: "p1" }),
    ],
    documents: [
      {
        artifactId: spec.id,
        blocks: [block("r1", "requirement", 0, 40), block("p1", "paragraph", 1, 39)],
      },
      {
        artifactId: plan.id,
        blocks: [block("r9", "requirement", 0, 40), block("q1", "paragraph", 1, 39)],
      },
    ],
    issue: { ...issueDetails([spec, plan]), plan_artifact_id: plan.id },
  });

  expect(parentsOf(root, "c1")).toEqual([KEY]);
  expect(parentsOf(root, "c2")).toEqual([KEY]);
  expect(parentsOf(root, "c3")).toEqual([blockNodeId(SPEC, "r1")]);
  // The plan's block is a node of its own under the issue, and holds no comment.
  expect(parentsOf(root, planned)).toEqual([KEY]);
  expect(root.children.find((child) => child.id === planned)?.children).toEqual([]);
});
