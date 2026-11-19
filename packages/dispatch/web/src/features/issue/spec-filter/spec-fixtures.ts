import type {
  Actor,
  Artifact,
  Decision,
  DecisionLink,
  IssueDecisions,
  IssueDetails,
} from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import type { NodeType } from "../page-words";

/**
 * What the Spec filter's two unit tests are given: one issue, one tree and one decisions read,
 * built by hand, so the model and the body are read without a server
 * (`spec-decided-by.test.ts`, `SpecFilterBody.drawn.test.tsx`).
 *
 * The addresses are the ones this issue would answer with: a statement is a block of the spec,
 * a decision a block of `decisions.md`, and an ask its own id, so a tie resolves through the
 * page's own rule for addressing a node (`tree-drafts.ts`'s `treeContext`) rather than through
 * a rule written here.
 */

export const KEY = "CORE-1";
export const SPEC_SLUG = "spec";
export const DECISIONS_SLUG = "decisions-md";

/** Who is reading, whose own words read as theirs (R34). */
export const READER = "alice";

export const READER_ACTOR: Actor = { id: READER, kind: "user" };

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

/** The issue, with its spec and its decisions document, which is all the addressing rule reads
 *  of it. */
export function issueDetails(): IssueDetails {
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

/** One `shaped` tie, from a decision to the node its link names (dispatch://INFRA-1150 R1). */
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
      kind: "requirement",
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
  return { decisions: [...made], document: "artifact-dec", open_questions: [] };
}

/** The address of one statement, which is one block of the issue's spec. */
export const statementRef = (block: string) => `dispatch://${KEY}/spec#b-${block}`;

/** The issue with `children` under it. */
export function issueTree(children: readonly TreeNode[]): TreeNode {
  return node({ children: [...children], id: KEY, type: "issue" });
}

/** One statement of the spec, by its block id, with its number in its own words as the spec
 *  writes one. */
export function statement(block: string, partial: Partial<TreeNode> = {}): TreeNode {
  return node({
    counts: { checks: 1, planTasks: 1 },
    id: `spec#b-${block}`,
    reference: statementRef(block),
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
