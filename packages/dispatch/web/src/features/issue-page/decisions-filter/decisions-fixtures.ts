import type {
  Actor,
  Artifact,
  AskResolution,
  Decision,
  DecisionLink,
  IssueDecisions,
  IssueDetails,
} from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import type { NodeType } from "../page-words";
import type { DecisionsAsk } from "./decisions-parts";

/**
 * What the Decisions filter's two unit tests are given: one issue, one tree, one decisions read
 * and the issue's asks, built by hand, so the model and the body are read without a server
 * (`decisions-parts.test.ts`, `DecisionsFilterBody.drawn.test.tsx`).
 *
 * The addresses are the ones this issue would answer with: a statement is a block of the spec,
 * a decision a block of `decisions.md`, and an ask its own id, so a tie resolves through the
 * page's own rule for addressing a node (`tree-drafts.ts`'s `treeContext`) and not through a
 * rule written here.
 */

export const KEY = "CORE-1";
export const SPEC_SLUG = "spec";
export const DECISIONS_SLUG = "decisions-md";

/** Who is reading, whose own words read as theirs (R34). */
export const READER = "alice";
export const READER_ACTOR: Actor = { id: READER, kind: "user" };

/** The agent that asks the questions and writes the decisions, by the title its session stamps
 *  on a write, which is the name the page draws it by. */
export const AGENT: Actor = {
  id: "session-architect",
  kind: "session",
  origin: { session_title: "Architect" },
};

/** One document of the issue. */
export function artifact(id: string, slug: string, primary = false): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: READER_ACTOR,
    id,
    issue_key: KEY,
    kind: "doc",
    name: slug,
    primary,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
  };
}

/** The issue's decisions document, which a Keep is written against. */
export const DECISIONS_DOCUMENT = artifact("artifact-decisions", DECISIONS_SLUG);

/** The issue, with its spec and its decisions document, which is all the addressing rule reads
 *  of it. */
export function issueDetails(): IssueDetails {
  return {
    advances: [],
    advances_revision: 0,
    artifacts: [artifact("artifact-spec", SPEC_SLUG, true), DECISIONS_DOCUMENT],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: READER_ACTOR,
    decisions_artifact_id: DECISIONS_DOCUMENT.id,
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
    primary_artifact_id: "artifact-spec",
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
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
    verification_artifact_id: "",
  };
}

/** One node of the tree, with the fields a case needs on it. */
export function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** The issue with `children` under it. */
export function issueTree(children: readonly TreeNode[]): TreeNode {
  return node({ children: [...children], id: KEY, type: "issue" });
}

/** The address of one block of the issue's spec. */
export const specRef = (block: string) => `dispatch://${KEY}/spec#b-${block}`;

/** The address of one decision, which is one block of the issue's decisions document. */
export const decisionRef = (block: string) =>
  `dispatch://${KEY}/artifact/${DECISIONS_SLUG}#b-${block}`;

/** One statement of the spec, by its block id. */
export function statement(block: string, partial: Partial<TreeNode> = {}): TreeNode {
  return node({
    id: `${SPEC_SLUG}#b-${block}`,
    reference: specRef(block),
    text: `The page draws ${block}. It draws nothing else.`,
    type: "requirement",
    ...partial,
  });
}

/** One question of the issue, asked by the agent, as the tree holds it. */
export function ask(id: string, partial: Partial<TreeNode> = {}): TreeNode {
  return node({
    at: "2026-11-10T08:00:00Z",
    author: AGENT,
    id,
    reference: `dispatch://${KEY}/ask/${id}`,
    text: `Question ${id}?`,
    type: "ask",
    ...partial,
  });
}

/** A question nobody has answered, whose turn is the reader's unless `waiting` says not. */
export function openAsk(id: string, waiting = true, partial: Partial<TreeNode> = {}): TreeNode {
  return ask(id, { question: true, state: "open", waiting, ...partial });
}

/** A question a person answered, with their answer on it as the tree holds one. */
export function answeredAsk(
  id: string,
  answer: { at: string; by?: Actor; selected?: string[]; text: string },
  partial: Partial<TreeNode> = {}
): TreeNode {
  return ask(id, {
    answer: {
      at: answer.at,
      by: answer.by ?? READER_ACTOR,
      selected: answer.selected ?? [],
      text: answer.text,
    },
    state: "answered",
    ...partial,
  });
}

/** How one ask was closed, as the asks read answers it beside the ask. */
export function closing(
  id: string,
  resolution: Partial<AskResolution> & { at: string; kind: string }
): DecisionsAsk {
  return { id, resolution: { actor: READER_ACTOR, reason: "", ...resolution } };
}

/** One decision of the decisions document as the tree draws it. */
export function decisionNode(block: string, partial: Partial<TreeNode> = {}): TreeNode {
  return node({
    id: `${DECISIONS_SLUG}#b-${block}`,
    reference: decisionRef(block),
    text: `The decision ${block}.`,
    type: "decision",
    ...partial,
  });
}

/** One `shaped` tie, as today's read carries it: a link from a block of the decision to the
 *  node `ref` names. */
export function shaped(ref: string, stale = false): DecisionLink {
  return {
    created_at: "2026-11-10T00:00:00Z",
    created_version: 1,
    from_block: "d",
    stale,
    to: {
      changed_at: null,
      changed_by: null,
      id: ref,
      kind: "block",
      made_at: null,
      made_by: null,
      parent: null,
      parent_version: null,
      ref,
    },
  };
}

/** One decision of the decisions document, as the decisions read answers it: an agent's, which
 *  waits for review unless the case says otherwise. */
export function decision(block: string, partial: Partial<Decision> = {}): Decision {
  return {
    asks: [],
    block_id: block,
    changed_at: "2026-11-10T12:00:00Z",
    changed_version: 1,
    heading: `The decision ${block}`,
    kept: null,
    kind: "agent",
    links: [],
    ref: decisionRef(block),
    replaced: false,
    text: `The decision ${block}.`,
    text_sha256: `sha256:${block}`,
    unsure: false,
    waiting_review: true,
    ...partial,
  };
}

/** The decisions read over `made`, which names the decisions document they are blocks of by its
 *  `dispatch://` address, as the server's read does (`api/decisions.go`); a Change's anchor names
 *  the same document by its id, which the issue's documents tie to the address
 *  (`decisionsDocumentId`). */
export function decisionsRead(made: readonly Decision[]): IssueDecisions {
  return {
    decisions: [...made],
    document: `dispatch://${KEY}/artifact/${DECISIONS_SLUG}`,
    open_questions: [],
  };
}
