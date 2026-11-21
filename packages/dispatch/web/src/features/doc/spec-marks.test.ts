import { expect, test } from "bun:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import { correction } from "../../__tests__/correction-feed";
import { graphNode } from "../../__tests__/graph-fixture";
import { markCorrectedPost } from "../../api/corrected-post";
import type {
  ArtifactBlock,
  Decision,
  DecisionLink,
  GraphReferences,
  IssueDecisions,
} from "../../api/types";
import { changedSinceReadMark, staleBlockMark } from "../../theme/classes";
import {
  changedSinceRead,
  readLineText,
  STALE_BLOCK_TITLE,
  specBlockMarks,
  specBlockParts,
  staleSpecBlocks,
} from "./spec-marks";

function block(id: string, type: string, from: number): ArtifactBlock {
  return {
    from,
    id,
    references: { asks: 0, comments: 0 },
    to: from + 10,
    token: `sha256:${id}`,
    type,
  };
}

function link(to: string, stale: boolean): DecisionLink {
  return {
    created_at: "2026-10-01T00:00:00Z",
    created_version: 1,
    from_block: "d-para",
    stale,
    to: graphNode({ block_id: to, id: `CORE-1/spec#${to}`, kind: "block" }),
  };
}

function decision(blockId: string, links: DecisionLink[], replaced = false): Decision {
  return {
    asks: [],
    block_id: blockId,
    changed_at: "2026-10-02T00:00:00Z",
    changed_version: 2,
    heading: `Decision ${blockId}`,
    kept: null,
    kind: "agent",
    links,
    ref: `dispatch://CORE-1/artifact/decisions-md#b-${blockId}`,
    replaced,
    text: "We decided.",
    text_sha256: `sum-${blockId}`,
    unsure: false,
    waiting_review: true,
  };
}

const decisions: IssueDecisions = {
  decisions: [
    decision("d-1", [link("p-plan", true), link("p-scope", false)]),
    decision("d-2", [link("p-plan", true), link("p-rollout", true)], true),
  ],
  document: "dispatch://CORE-1/artifact/decisions-md",
  open_questions: [],
};

test("every spec block a stale link names is stale, each once, in the order the view names them", () => {
  expect(staleSpecBlocks(decisions)).toEqual(["p-plan", "p-rollout"]);
  expect(staleSpecBlocks(undefined)).toEqual([]);
  expect(staleSpecBlocks({ ...decisions, decisions: [], document: null })).toEqual([]);
});

const references: GraphReferences = {
  edges: [
    {
      created_at: "2026-10-01T00:00:00Z",
      direction: "in",
      excerpt: { block_id: "h-plan", text: "## Plan" },
      kind: "part_of",
      node: graphNode({
        block_id: "h-plan",
        changed_version: 1,
        id: "CORE-1/spec#h-plan",
        kind: "block",
      }),
      source_seq: null,
    },
    {
      created_at: "2026-10-01T00:00:00Z",
      direction: "in",
      excerpt: { block_id: "p-plan", text: "Ship it in two steps." },
      kind: "part_of",
      node: graphNode({
        block_id: "p-plan",
        changed_version: 4,
        id: "CORE-1/spec#p-plan",
        kind: "block",
      }),
      source_seq: null,
    },
    {
      created_at: "2026-10-01T00:00:00Z",
      direction: "in",
      excerpt: { block_id: "h-rollout", text: "## Rollout" },
      kind: "part_of",
      node: graphNode({
        block_id: "h-rollout",
        changed_version: 3,
        id: "CORE-1/spec#h-rollout",
        kind: "block",
      }),
      source_seq: null,
    },
    {
      created_at: "2026-10-01T00:00:00Z",
      direction: "in",
      excerpt: { block_id: "p-rollout", text: "Behind a flag." },
      kind: "part_of",
      node: graphNode({
        block_id: "p-rollout",
        changed_version: 3,
        id: "CORE-1/spec#p-rollout",
        kind: "block",
      }),
      source_seq: null,
    },
    {
      created_at: "2026-10-01T00:00:00Z",
      direction: "in",
      excerpt: { block_id: "p-intro", text: "Why we need notices at all." },
      kind: "part_of",
      node: graphNode({
        block_id: "p-intro",
        changed_version: 5,
        id: "CORE-1/spec#p-intro",
        kind: "block",
      }),
      source_seq: null,
    },
    // An edge that is not a part of the spec, which the read never holds, is not a block of it.
    {
      created_at: "2026-10-01T00:00:00Z",
      direction: "in",
      kind: "mentions",
      node: graphNode({ id: "CORE-2", kind: "issue" }),
      source_seq: 9,
    },
  ],
  node: graphNode({ id: "artifact-1", kind: "artifact" }),
};

// In the document: the intro paragraph, then `## Plan` and its paragraph, then `## Rollout` and
// its paragraph. The listing's order is by position, whatever order it arrives in.
const blocks: ArtifactBlock[] = [
  block("p-rollout", "paragraph", 60),
  block("h-plan", "heading", 20),
  block("p-intro", "paragraph", 0),
  block("p-plan", "paragraph", 30),
  block("h-rollout", "heading", 50),
];

test("the spec's blocks are the part_of edges' block nodes, with their changed versions and text", () => {
  expect(specBlockParts(references)).toEqual([
    { blockId: "h-plan", changedVersion: 1, text: "## Plan" },
    { blockId: "p-plan", changedVersion: 4, text: "Ship it in two steps." },
    { blockId: "h-rollout", changedVersion: 3, text: "## Rollout" },
    { blockId: "p-rollout", changedVersion: 3, text: "Behind a flag." },
    { blockId: "p-intro", changedVersion: 5, text: "Why we need notices at all." },
  ]);
});

test("changed since a read: the blocks above its version, named by the heading of their section", () => {
  const changed = changedSinceRead(blocks, specBlockParts(references), 3);
  expect(changed).toEqual({
    blockIds: ["p-intro", "p-plan"],
    // A block before any heading is named by its own text; a heading is named once however many
    // of its blocks changed.
    headings: ["Why we need notices at all.", "Plan"],
    version: 3,
  });
  if (changed === undefined) throw new Error("nothing changed");
  expect(readLineText(changed)).toBe(
    "Changed since you last read version 3: Why we need notices at all., Plan"
  );

  const fromTwo = changedSinceRead(blocks, specBlockParts(references), 2);
  expect(fromTwo?.blockIds).toEqual(["p-intro", "p-plan", "h-rollout", "p-rollout"]);
  expect(fromTwo?.headings).toEqual(["Why we need notices at all.", "Plan", "Rollout"]);
});

test("no read version, or nothing changed since it, shows no line and marks nothing", () => {
  expect(changedSinceRead(blocks, specBlockParts(references), null)).toBeUndefined();
  expect(changedSinceRead(blocks, specBlockParts(references), 5)).toBeUndefined();
});

// N2 of test round 46 (at 8259e87d) on dispatch://INFRA-1523: the line names its sections from the
// spec's `part_of` read, whose block texts are the ones the document's latest indexed version gave
// them (`document_blocks`). A pending insert put the spec's first paragraph in, and its comment's
// delete took it out of the document; the read the page holds from before the delete still gives
// its words, with no mark to tell them by. The delete's correction drops that read whole
// (`markCorrectedPost`), and the page, which draws the line from it, reads it again: Dispatch
// answers it as the delete left the spec, with no such paragraph, and the line names none of the
// words. The control: a delete that leaves its quote - a replace suggestion's, a plain comment's,
// the document's own text - drops nothing, and the page reads nothing again.
test("the line names none of a deleted insert's words once the delete's correction reaches the spec's part_of read", async () => {
  const key = ["references", "to", "dispatch://CORE-1/spec", "part_of"];
  const parent = { id: "artifact-1", kind: "artifact", ref: "dispatch://CORE-1/spec" };
  const inserted = "Why we need notices at all, for the security review.";
  const cached: GraphReferences = {
    ...references,
    edges: (references.edges ?? []).map((edge) =>
      edge.node.kind !== "block"
        ? edge
        : {
            ...edge,
            excerpt:
              edge.node.block_id === "p-intro"
                ? { block_id: "p-intro", text: inserted }
                : edge.excerpt,
            node: { ...edge.node, parent },
          }
    ),
  };
  // The spec's parts as Dispatch reads them once the delete took the paragraph out.
  const afterDelete: GraphReferences = {
    ...references,
    edges: (references.edges ?? []).filter((edge) => edge.node.block_id !== "p-intro"),
  };
  const lineAfter = async (quote: string): Promise<string | undefined> => {
    const client = new QueryClient();
    client.setQueryData(key, cached);
    // The page's read of the parts, which it draws the line from.
    const page = new QueryObserver(client, {
      queryFn: () => Promise.resolve(afterDelete),
      queryKey: key,
      staleTime: Number.POSITIVE_INFINITY,
    });
    const unsubscribe = page.subscribe(() => {});
    try {
      markCorrectedPost(
        client,
        correction("comment", "comment-insert", false, {
          anchor: {
            artifact_id: "artifact-1",
            block_id: "p-intro",
            mark_id: "mark-insert",
            orphaned: false,
            quote,
            version: 1,
          },
        })
      );
      await client.getQueryCache().find({ exact: true, queryKey: key })?.promise;
      const read = client.getQueryData<GraphReferences>(key);
      const changed =
        read === undefined ? undefined : changedSinceRead(blocks, specBlockParts(read), 3);
      return changed === undefined ? undefined : readLineText(changed);
    } finally {
      unsubscribe();
    }
  };
  expect({ insert: await lineAfter(""), kept: await lineAfter("Why we need notices") }).toEqual({
    insert: "Changed since you last read version 3: Plan",
    kept: `Changed since you last read version 3: ${inserted}, Plan`,
  });
});

test("the editor's block marks: stale blocks with their title, changed blocks with theirs", () => {
  expect(specBlockMarks(["p-plan"], ["p-intro", "p-plan"])).toEqual([
    { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
    { blockId: "p-intro", className: changedSinceReadMark },
    { blockId: "p-plan", className: changedSinceReadMark },
  ]);
  expect(STALE_BLOCK_TITLE).toBe("A decision that shaped this block changed after it.");
});
