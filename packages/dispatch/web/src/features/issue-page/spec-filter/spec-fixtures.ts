import type {
  Actor,
  Artifact,
  DecisionLink,
  IssueCoverage,
  IssueDetails,
} from "../../../api/types";
import {
  artifact,
  KEY,
  node,
  READER,
  READER_ACTOR,
  SPEC_SLUG,
} from "../decisions-filter/decisions-fixtures";
import type { TreeAsk, TreeNode } from "../issue-tree";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "../tree-read/tree-read";

/**
 * What the Spec filter's two unit tests are given: one issue, one tree and one answer of the
 * Spec view read, whose decision nodes are the decisions the column draws (`decisionNodes`),
 * built by hand, so the model and the body are read without a server
 * (`spec-decided-by.test.ts`, `SpecFilterBody.drawn.test.tsx`). A case that needs the tree as
 * the page's model places its nodes builds it with `buildIssueTree` from the reads below
 * (`askRead`, `coverageRead`) instead.
 *
 * The issue's key, the reader, a document, a node and the issue's tree are the Decisions
 * filter's own fixtures (`decisions-filter/decisions-fixtures.ts`), the same in value and in
 * what they build. What is here differs from that module's fixture of the same name, and is the
 * Spec view's own: the issue's decisions document is `artifact-dec`, which each decision node
 * here names as its document, and its `artifacts` is typed as present, so a case can add a
 * document; a statement is marked a statement, carries its counts and opens with its number; a
 * `shaped` tie names a requirement; a decision is given its kind and waits for no review.
 *
 * The addresses are the ones this issue would answer with: a statement is a block of the spec,
 * a decision a block of `decisions.md`, and an ask its own id, so a tie resolves through the
 * page's own rule for addressing a node (`tree-drafts.ts`'s `treeContext`) rather than through
 * a rule written here.
 */

/** The slug of the issue's decisions document, whose blocks the decisions here are. The
 *  Decisions filter's own fixtures name no such document: their decisions are blocks of the
 *  spec. */
export const DECISIONS_SLUG = "decisions-md";

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

/** One node of a read of the issue's tree, under the issue, with the fields a case needs on it. */
export function answerNode(
  id: string,
  supertag: string,
  partial: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return {
    attributes: {},
    author: [],
    id,
    made_at: "2026-11-05T00:00:00Z",
    parent: KEY,
    rank: null,
    state: "live",
    supertag,
    text: "",
    visibility: "project",
    ...partial,
  };
}

/** One decision of the issue's decisions document as the Spec view read answers it, as context
 *  (`mark: "context"`): a decision block, with the members the decisions read
 *  answers for that decision, by the same names. */
export function decision(block: string, partial: Partial<TreeAnswerNode> = {}): TreeAnswerNode {
  return answerNode(block, "decision", {
    asks: [],
    changed_at: "2026-11-14T12:00:00Z",
    changed_version: 1,
    document: { artifact_id: "artifact-dec", position: 0, slug: DECISIONS_SLUG },
    kept: null,
    kind: "agent",
    links: [],
    made_at: "2026-11-14T12:00:00Z",
    mark: "context",
    replaced: false,
    text: block,
    text_sha256: "",
    unsure: false,
    waiting_review: false,
    ...partial,
  });
}

/** The Spec view read's answer (`?view=spec`), on one page: `nodes` under the issue's own node,
 *  with `tuples` between them. It is what the page holds on the issue's node
 *  (`TreeNode.views.spec`), whose decisions the column draws. */
export function specViewAnswer(
  nodes: readonly TreeAnswerNode[],
  tuples: readonly TreeTuple[] = []
): TreeAnswer {
  return {
    converted: false,
    cursor: null,
    nodes: [
      answerNode(KEY, "issue", { parent: null, text: "The issue page draws one ordered body" }),
      ...nodes,
    ],
    tree: null,
    tuples: [...tuples],
    view_of: null,
  };
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

/** One decision of the decisions document as the tree draws it: the node the Spec view's
 *  decision of the same block (`decision`) is addressed to. */
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
