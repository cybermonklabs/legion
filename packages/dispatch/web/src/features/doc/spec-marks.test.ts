import { expect, test } from "bun:test";
import { graphNode } from "../../__tests__/graph-fixture";
import type {
  ArtifactBlock,
  DecisionEntry,
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

/** A `shaped` link to block `to` of the document at `document`, stale or not. Its other fact and
 *  its events say the opposite - a stale link's block has not changed since the tie, and a fresh
 *  one's has, by its events too - so a reader of anything but `stale` marks the wrong blocks. Its
 *  node is the issue room's (`CORE-1/tree#<id>`), and the document holding the block is named on
 *  the link, as the server answers a block of an issue room. */
function link(to: string, stale: boolean, document = "dispatch://CORE-1/spec"): DecisionLink {
  return {
    changed_since_tie: !stale,
    created_at: "2026-10-01T00:00:00Z",
    created_version: 1,
    event: 10,
    from_block: "d-1",
    stale,
    text_changed_event: stale ? 9 : 11,
    to: graphNode({ block_id: to, document, id: `CORE-1/tree#${to}`, kind: "block" }),
  };
}

function entry(blockId: string, shaped: DecisionLink[], replaced = false): DecisionEntry {
  return {
    answer: null,
    answer_state: null,
    answered_at: null,
    answered_by: null,
    changed_at: "2026-10-02T00:00:00Z",
    changed_version: 2,
    decided_by: { id: "session-1", kind: "session" },
    declined: false,
    declined_note: null,
    document: {
      artifact_id: "choices-id",
      block: `CORE-1/choices-md#${blockId}`,
      block_id: blockId,
      ref: "dispatch://CORE-1/artifact/choices-md",
    },
    from_ask: [],
    kept: null,
    kept_by: null,
    kept_text_sum: null,
    kind: "agent",
    multiple: null,
    node: { id: `CORE-1/choices-md#${blockId}`, kind: "block" },
    options: null,
    question: null,
    readings: [],
    ref: `dispatch://CORE-1/artifact/choices-md#b-${blockId}`,
    replaced,
    resolution: null,
    selected: null,
    shaped,
    text: ":::decision{#d}\nWe decided.\n:::\n",
    text_sha256: `sum-${blockId}`,
    uncertainty: null,
    uncertainty_reason: null,
    urgency: null,
    waiting_review: true,
  };
}

const decisions: IssueDecisions = {
  decisions: [],
  document: null,
  entries: [
    entry("d-1", [link("p-plan", true), link("p-scope", false)]),
    entry(
      "d-2",
      [
        link("p-plan", true),
        link("p-rollout", true),
        link("p-notes", true, "dispatch://CORE-1/artifact/notes-md"),
      ],
      true
    ),
  ],
  open_questions: [],
};

test("every spec block a stale link names is stale, each once, in the order the read names them", () => {
  expect(staleSpecBlocks(decisions, "dispatch://CORE-1/spec")).toEqual(["p-plan", "p-rollout"]);
  expect(staleSpecBlocks(decisions)).toEqual(["p-plan", "p-rollout", "p-notes"]);
  expect(staleSpecBlocks(undefined, "dispatch://CORE-1/spec")).toEqual([]);
  expect(staleSpecBlocks({ ...decisions, entries: [] }, "dispatch://CORE-1/spec")).toEqual([]);
});

test("an open and a declined ask mark nothing stale, and an answered one does", () => {
  const ask = (id: string, state: string, to: string, declined = false): DecisionEntry => ({
    ...entry(id, [link(to, true)]),
    answer_state: state,
    declined,
    kind: declined ? "declined" : "person",
    node: { id, kind: "ask" },
    question: `Question ${id}?`,
    ref: `dispatch://CORE-1/ask/${id}`,
    waiting_review: false,
  });
  const asks: IssueDecisions = {
    ...decisions,
    entries: [
      ask("open-ask", "open", "p-open"),
      ask("declined-ask", "resolved", "p-declined", true),
      ask("answered-ask", "answered", "p-answered"),
    ],
  };
  expect(staleSpecBlocks(asks, "dispatch://CORE-1/spec")).toEqual(["p-answered"]);
});

// A block whose own text changed after the tie, the decision unchanged since, is not stale: the
// server's `stale` says the decision changed after the block, and its `changed_since_tie` and
// events say only that the block changed, which marks nothing.
test("a block is marked by its link's stale alone, never by its own change since the tie", () => {
  const changedSinceTie: IssueDecisions = {
    ...decisions,
    entries: [entry("d-1", [link("p-plan", false), link("p-scope", false)])],
  };
  expect(staleSpecBlocks(changedSinceTie, "dispatch://CORE-1/spec")).toEqual([]);
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

test("the editor's block marks: stale blocks with their title, changed blocks with theirs", () => {
  expect(specBlockMarks(["p-plan"], ["p-intro", "p-plan"])).toEqual([
    { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
    { blockId: "p-intro", className: changedSinceReadMark },
    { blockId: "p-plan", className: changedSinceReadMark },
  ]);
  // A block is marked for a decision that changed after it (`DecisionLink.stale`), so the title
  // names the decision's change and not one of the block's.
  expect(STALE_BLOCK_TITLE).toBe("A decision that shaped this block changed after it.");
});
