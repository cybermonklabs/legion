import type {
  Actor,
  Artifact,
  AskResolution,
  DecisionLink,
  IssueDetails,
} from "../../../api/types";
import { type DecisionNode, decisionNodes } from "../decision-nodes/decision-nodes";
import type { TreeNode } from "../issue-tree";
import type { NodeType } from "../page-words";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import type { DecisionsAsk } from "./decisions-parts";

/**
 * What the Decisions filter's unit tests are given: one issue, one tree, the decision nodes of
 * the tree read and of its decisions view, and the issue's asks, built by hand, so the model and
 * the body are read without a server (`decisions-parts.test.ts`, `decisions-entries.test.tsx`,
 * `DecisionsFilterBody.drawn.test.tsx`).
 *
 * The addresses are the ones this issue would answer with: a statement is a block of the spec,
 * a decision a decision block of the spec, addressed as the tree read addresses a typed block of
 * a document (`decision-nodes.ts`), and an ask its own id, so a tie resolves through the page's
 * own rule for addressing a node (`tree-drafts.ts`'s `treeContext`) and not through a rule
 * written here.
 */

export const KEY = "CORE-1";
export const SPEC_SLUG = "spec";

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

/** The issue's spec, which holds its statements and its decisions, and which a Keep and a
 *  Change are written against. */
export const SPEC_DOCUMENT = artifact("artifact-spec", SPEC_SLUG, true);

/** The issue, with its spec, which is all the addressing rule reads of it. */
export function issueDetails(): IssueDetails {
  return {
    advances: [],
    advances_revision: 0,
    artifacts: [SPEC_DOCUMENT],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: READER_ACTOR,
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
    primary_artifact_id: SPEC_DOCUMENT.id,
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

/** The address of one decision, a decision block of the spec, as the tree read addresses a
 *  typed block of a document (`decision-nodes.ts`). */
export const decisionRef = (block: string) => `dispatch://${KEY}/artifact/${SPEC_SLUG}#b-${block}`;

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

/** One decision of the spec as the tree draws it. */
export function decisionNode(block: string, partial: Partial<TreeNode> = {}): TreeNode {
  return node({
    id: `${SPEC_SLUG}#b-${block}`,
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

/** One decision block of the spec as the tree read answers it, with
 *  the members the decisions read answers for that decision, by the same names: an
 *  agent's, which waits for review unless the case says otherwise. */
export function decision(block: string, partial: Partial<TreeAnswerNode> = {}): TreeAnswerNode {
  return {
    asks: [],
    attributes: {},
    author: [],
    changed_at: "2026-11-10T12:00:00Z",
    changed_version: 1,
    document: { artifact_id: SPEC_DOCUMENT.id, position: 0, slug: SPEC_SLUG },
    id: block,
    kept: null,
    kind: "agent",
    links: [],
    made_at: "2026-11-10T12:00:00Z",
    parent: KEY,
    rank: null,
    replaced: false,
    state: "live",
    supertag: "decision",
    text: `The decision ${block}.`,
    text_sha256: `sha256:${block}`,
    unsure: false,
    visibility: "project",
    waiting_review: true,
    ...partial,
  };
}

/** A comment a person wrote that the tree read hangs under the decision block `block`: the
 *  Change they asked of it, where it is newer than the decision's words and its Keep
 *  (`decision-review.ts`). */
export function changeComment(
  id: string,
  block: string,
  at: string,
  body: string,
  by: Actor = READER_ACTOR
): TreeAnswerNode {
  return {
    attributes: {},
    author: [by],
    id,
    made_at: at,
    parent: block,
    rank: null,
    state: "live",
    supertag: "comment",
    text: body,
    visibility: "project",
  };
}

/** One answer of a read of the issue's tree - the tree read itself, or its decisions view -
 *  holding `nodes` under the issue's own node, on one page. */
export function treeAnswer(nodes: readonly TreeAnswerNode[]): TreeAnswer {
  return {
    converted: false,
    cursor: null,
    nodes: [
      {
        attributes: {},
        author: [],
        id: KEY,
        made_at: "2026-11-05T00:00:00Z",
        parent: null,
        rank: null,
        state: "live",
        supertag: "issue",
        text: "The issue page draws one ordered body",
        visibility: "project",
      },
      ...nodes,
    ],
    tree: null,
    tuples: [],
    view_of: null,
  };
}

/** The decisions of `made` as every reader of a decision reads them (`decisionNodes`), from an
 *  answer that holds them. */
export function decisionsOf(made: readonly TreeAnswerNode[]): DecisionNode[] {
  return decisionNodes(treeAnswer(made), { asks: undefined, issue: { key: KEY } });
}

/** The one decision `made` is, as every reader of a decision reads it (`decisionsOf`). */
export function decisionOf(made: TreeAnswerNode): DecisionNode {
  const [one] = decisionsOf([made]);
  if (one === undefined) throw new Error(`the read answers no decision for ${made.id}`);
  return one;
}
