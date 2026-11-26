import type {
  Actor,
  Artifact,
  Decision,
  DecisionLink,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
} from "../../../api/types";
import {
  artifact,
  DECISIONS_SLUG,
  KEY,
  node,
  READER,
  READER_ACTOR,
  SPEC_SLUG,
} from "../decisions-filter/decisions-fixtures";
import type { TreeAsk, TreeNode } from "../issue-tree";

/**
 * What the Spec filter's two unit tests are given: one issue, one tree and one decisions read,
 * built by hand, so the model and the body are read without a server
 * (`spec-decided-by.test.ts`, `SpecFilterBody.drawn.test.tsx`). A case that needs the tree as
 * the page's model places its nodes builds it with `buildIssueTree` from the reads below
 * (`askRead`, `coverageRead`) instead.
 *
 * The issue's key, the reader, a document, a node and the issue's tree are the Decisions
 * filter's own fixtures (`decisions-filter/decisions-fixtures.ts`), the same in value and in
 * what they build. What is here differs from that module's fixture of the same name, and is the
 * Spec view's own: the issue's decisions document is `artifact-dec`, which `decisionsRead` names
 * as its document, and its `artifacts` is typed as present, so a case can add a document; a
 * statement is marked a statement, carries its counts and opens with its number; a `shaped` tie
 * names a requirement; a decision is given its kind and waits for no review.
 *
 * The addresses are the ones this issue would answer with: a statement is a block of the spec,
 * a decision a block of `decisions.md`, and an ask its own id, so a tie resolves through the
 * page's own rule for addressing a node (`tree-drafts.ts`'s `treeContext`) rather than through
 * a rule written here.
 */

/** The issue, with its spec and its decisions document, which is all the addressing rule reads
 *  of it. Its `artifacts` is typed as present, so a case can add a document to the list. */
export function issueDetails(): IssueDetails & { artifacts: Artifact[] } {
  return {
    advances: [],
    advances_revision: 0,
    artifacts: [
      artifact("artifact-spec", SPEC_SLUG, true),
      artifact("artifact-dec", DECISIONS_SLUG),
    ],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: READER_ACTOR,
    decisions_artifact_id: "artifact-dec",
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

/** One `shaped` tie, from a decision to the node its link names. */
export function shaped(ref: string): DecisionLink {
  return {
    changed_since_tie: false,
    created_at: "2026-11-14T10:00:00Z",
    created_version: 1,
    from_block: "d1",
    stale: false,
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

/** One decision of the issue's decisions document, as the decisions read answers it. */
export function decision(
  partial: Partial<Decision> & { block_id: string; kind: "agent" | "person" }
): Decision {
  return {
    asks: [],
    changed_at: "2026-11-14T12:00:00Z",
    changed_version: 1,
    heading: partial.block_id,
    kept: null,
    links: [],
    ref: `dispatch://${KEY}/artifact/${DECISIONS_SLUG}#b-${partial.block_id}`,
    replaced: false,
    text: partial.block_id,
    text_sha256: "",
    unsure: false,
    waiting_review: false,
    ...partial,
  };
}

/** The decisions read over `made`. */
export function decisionsRead(made: readonly Decision[]): IssueDecisions {
  return { decisions: [...made], document: "artifact-dec", entries: [], open_questions: [] };
}

/** The address of one statement, which is one block of the issue's spec. */
export const statementRef = (block: string) => `dispatch://${KEY}/spec#b-${block}`;

/** One statement of the spec, by its block id, with its number in its own words as the spec
 *  writes one: a requirement of the issue's own spec, which the page's model marks a statement
 *  (`isStatement`). */
export function statement(block: string, partial: Partial<TreeNode> = {}): TreeNode {
  return node({
    counts: { checks: 1, planTasks: 1 },
    id: `spec#b-${block}`,
    reference: statementRef(block),
    statement: true,
    text: `**${block.toUpperCase()}.** The page draws ${block}.`,
    type: "requirement",
    ...partial,
  });
}

/** The question a person answered, with their answer on it as the tree holds one. */
export function answeredAsk(
  id: string,
  answer: { at?: string; by?: Actor; selected?: string[]; text: string },
  children: readonly TreeNode[] = []
): TreeNode {
  return node({
    answer: {
      at: answer.at ?? "2026-11-14T09:00:00Z",
      by: answer.by ?? READER_ACTOR,
      selected: answer.selected ?? [],
      text: answer.text,
    },
    at: "2026-11-14T08:00:00Z",
    author: { id: "session-architect", kind: "agent" },
    children: [...children],
    id,
    reference: `dispatch://${KEY}/ask/${id}`,
    state: "answered",
    text: "Does the built page work for you?",
    type: "ask",
  });
}

/** One decision of the decisions document as the tree draws it: the node the decisions read's
 *  own entry is addressed to. */
export function decisionNode(block: string, partial: Partial<TreeNode> = {}): TreeNode {
  return node({
    at: "2026-11-13T07:00:00Z",
    author: { id: "session-planner", kind: "agent" },
    id: `${DECISIONS_SLUG}#b-${block}`,
    reference: `dispatch://${KEY}/artifact/${DECISIONS_SLUG}#b-${block}`,
    text: `The decision ${block}.`,
    type: "decision",
    ...partial,
  });
}

/** A question asked of the issue, as the asks read answers it, which the reader answered with
 *  `text`: what the page's model builds the answered question's node from. */
export function askRead(id: string, text: string): TreeAsk {
  return {
    anchor: null,
    answer: { at: "2026-11-14T09:00:00Z", selected: [], text, user: READER },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-architect", kind: "agent" },
    block_id: null,
    created_at: "2026-11-14T08:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    kind: "question",
    multiple: false,
    options: [],
    question: "Does the built page work for you?",
    state: "answered",
    urgency: "med",
  };
}

/** The coverage read over the statements `blocks`, in that order, each served by a check: what
 *  the page's model builds each statement's node from. */
export function coverageRead(blocks: readonly string[]): IssueCoverage {
  return {
    gaps: [],
    has_plan: false,
    issue: KEY,
    requirements: blocks.map((block) => ({
      block_id: block,
      checks: 1,
      gaps: [],
      plan_tasks: 1,
      ref: statementRef(block),
      text: `**${block.toUpperCase()}.** The page draws ${block}.`,
    })),
    spec_artifact_id: "artifact-spec",
  };
}
