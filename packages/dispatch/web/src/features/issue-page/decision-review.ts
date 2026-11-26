import type {
  Artifact,
  Comment,
  Decision,
  DecisionEntry,
  DisplayTree,
  IssueDetails,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { ownTree, ownWords } from "../issue/decision-text";
import { postDeletion } from "../refs/post-marks";
import { parseNodeReference } from "../refs/routes";
import type { DecisionNode } from "./decision-nodes/decision-nodes";
import type { DecisionState } from "./page-words";
import type { TreeAnswer, TreeAnswerNode } from "./tree-read/tree-read";

/**
 * Which of an issue's decisions wait for the reader's review, and which a reader asked to
 * change.
 *
 * This module is pure and imports no code of the tree, only the types of a decision node and of
 * the tree read, so the tree (`tree-read/tree-nodes.ts`), the Decisions filter
 * (`decisions-filter/decisions-parts.ts`) and the header's count of what waits on the reader
 * (`waiting.ts`) read one rule each and cannot disagree.
 *
 * A decision is a node of the tree read, which the read places, and
 * it places its Change too: a comment anchored inside the decision's words hangs under its node.
 * So a decision's Changes are the comments a person wrote that the read hangs under it, made
 * since it last changed and was last kept, and the newest is the change it waits on
 * (`decisionNodeChanges`); no quote is matched for it. The decision then waits on its agent, not
 * on the reader: it says it was changed, and it leaves the review list and every count of what
 * waits on the reader. Once the agent edits the decision, every change is older than its words
 * and the decision waits for review again. A change its author deleted is no longer asked for:
 * it stays drawn under the decision, as the deleted line (`tree-read/tree-nodes.ts`'s
 * `draftTurn`), but no decision waits on it (`decisionNodeChanges`), so a decision whose every
 * change was deleted waits for review again.
 *
 * A Change is written as a comment anchored to the decision's own words (`DecisionActions.tsx`):
 * the decision node's document, and its `heading`, the words of its first paragraph
 * (`decision-nodes.ts`).
 */

/** The fields of a Change the Decisions filter reads (`decisionsParts`), and when its author
 *  last edited it, which the Decisions view draws a Change with (`decisions-parts.ts`'s
 *  `DecisionChange`). */
export type ChangeComment = Pick<Comment, "author" | "body" | "created_at" | "edited_at" | "id">;

/**
 * Whether `decision` is an entry of the review list (R35): an agent's decision that waits for a
 * person's review - neither kept as it reads now nor replaced, which is the server's
 * `waiting_review` - that a link ties to a statement of this issue, and that the reader has not
 * asked to change since it last changed (`changed`).
 * `isCoverageStatement` answers whether an address names a statement as the list reads one, a
 * requirement the coverage read holds, not the rows' `isStatement` (`issue-tree.ts`): the
 * outline's `draftDecision` (`tree-read/tree-nodes.ts`) and the header's count
 * (`IssuePropertiesRow.tsx`, through `waitingOnReader`) each pass
 * `coverageStatements(issue, coverage)` (`tree-drafts.ts`).
 *
 * It is the one rule that decides the list, so the count of what waits on the reader (R13), the
 * filter's "to review" signal and the node's own `waiting` read it too and cannot disagree with
 * the list. A decision a person decided is no agent's decision, whatever its `waiting_review`
 * says, and is never an entry (`decision-nodes.ts`' `decidedByPerson`, INFRA-1196's decision
 * 516): that rule is read from the decision nodes, whose module reads this one, so each caller
 * reads it before this one (`waiting.ts`, `decisions-filter/decisions-parts.ts`,
 * `tree-read/tree-nodes.ts`' `draftDecision`).
 */
export function inReviewList(
  decision: Pick<DecisionNode, "links" | "waiting_review">,
  isCoverageStatement: (reference: string) => boolean,
  changed = false
): boolean {
  return (
    !changed &&
    decision.waiting_review &&
    (decision.links ?? []).some(
      (link) => link.to.ref !== undefined && isCoverageStatement(link.to.ref)
    )
  );
}

/** A decision's own words and its own reading (`decisionContent`). */
export interface DecisionContent {
  readonly text: string;
  readonly tree?: DisplayTree;
}

/**
 * How a decision node stands with the reader, in the states the page draws
 * (`NODE_STATE_WORDS.decision`), from the review state the read answers on the node's
 * `decision`: replaced by a later one first, since nothing waits on it any longer; then a change
 * the reader asked for, which waits on its agent and not on the reader (`decisionNodeChanges`);
 * then waiting for review; then kept, by the person whose name the row draws after it
 * (`TreeNode.keptBy`). It is the one rule the outline (`tree-read/tree-nodes.ts`'
 * `draftDecision`) and the Decisions filter (`decisions-filter/decisions-parts.ts`) read. A
 * decision a person decided (`decision-nodes.ts`' `decidedByPerson`) stands in no state of an
 * agent's decision's review, kept, asked to change or waiting, so each hands it only whether it
 * was replaced.
 */
export function reviewState(
  decision: Partial<Pick<Decision, "kept" | "replaced" | "waiting_review">>,
  changed: boolean
): DecisionState | undefined {
  if (decision.replaced === true) return "replaced";
  if (changed) return "change_requested";
  if (decision.waiting_review === true) return "waiting_review";
  return decision.kept == null ? undefined : "kept";
}

/**
 * What `decision` says: the decisions read answers a decision's text as its one `decision` block
 * (`decisionread`'s words, the block rendered alone) and the server's reading of it as that
 * block's (`DecisionEntry.text_tree`), so its own words are that text with the lines that open and
 * close the block left out (`ownWords`), and its own reading is the block's content at the top
 * level (`ownTree`), where a section's blocks stood. A text or a reading that is no such block is
 * its own already. `tree` is absent where the read sent no reading. The page reads no decisions
 * read: its decisions are nodes of the tree read, which answers a decision node's text and
 * reading as the same whole block, so `decision-nodes.ts` reads each node's own content here too.
 * Part I's browser rows read a decisions read entry's words here (`e2e/issue-page-i.e2e.ts`).
 */
export function decisionContent(
  decision: Pick<DecisionEntry, "text" | "text_tree">
): DecisionContent {
  const text = ownWords(decision.text);
  const tree = ownTree(decision.text_tree);
  return tree === undefined ? { text } : { text, tree };
}

/** The fields of the issue read a decision's document is found by (`decisionDocumentId`): its
 *  key, its documents and which of them is its spec. */
type DecisionDocuments = Pick<IssueDetails, "key" | "primary_artifact_id"> & {
  readonly artifacts?: readonly Pick<Artifact, "id" | "slug">[] | null;
};

/**
 * The id of the document whose block `decision` is, among the issue's documents: the decisions
 * read names an agent's decision by its block's address (`DecisionEntry.ref`: `spec#b-…` for a
 * block of the issue's spec, `artifact/<slug>#b-…` for one of another document), and a Change's
 * anchor names the document it is anchored in by its id (`Anchor.artifact_id`). Undefined for a
 * person's decision, whose address is its ask's and names no block, for an address of another
 * issue, and while the issue read carries no such document.
 */
export function decisionDocumentId(
  issue: DecisionDocuments,
  decision: Pick<DecisionEntry, "ref">
): string | undefined {
  const route = parseNodeReference(decision.ref);
  if (route === undefined || !("key" in route) || route.key !== issue.key) return undefined;
  if (!("block" in route) || route.block === undefined) return undefined;
  if (route.kind === "spec") {
    return issue.artifacts?.find((artifact) => artifact.id === issue.primary_artifact_id)?.id;
  }
  if (route.kind !== "artifact") return undefined;
  return issue.artifacts?.find((artifact) => artifact.slug === route.slug)?.id;
}

/**
 * The id of the block `decision` is: the block the read names its document by
 * (`DecisionEntry.document`). Undefined for an entry that names none and whose address names no
 * block, an ask on the issue itself.
 */
export function decisionBlockId(
  decision: Pick<DecisionEntry, "document" | "ref">
): string | undefined {
  if (decision.document !== null) return decision.document.block_id;
  // A decision a server of the release before answered names no document (`withEntries`): its
  // address is its section's heading block in the decisions document, and that block is the one
  // a Keep is written against and a Change is placed by.
  // kept for one release: compat-objects
  const route = parseNodeReference(decision.ref);
  return route !== undefined && "block" in route ? route.block?.id : undefined;
}

/** The block id the tree read names `node` by: a typed block's own (`block_id`), else the node's
 *  id, which names a node that is no block. */
export function blockIdOfNode(node: Pick<TreeAnswerNode, "block_id" | "id">): string {
  return node.block_id ?? node.id;
}

/**
 * Each Change of a decision node, by the comment's node, to the decision node it changes: every
 * comment whose first maker - the first of the authors the read lists for it - is a person, that
 * the read hangs under a decision node that was not replaced, made after the decision last
 * changed and after it was last kept, as the node's `decision` answers both. The node a comment
 * hangs under is the one its `parent` names by the outline's rule (`parentsIn`). An agent's
 * comment under it is no Change, and neither is one older than its words, which asked for a
 * change the words have since had, nor one the read records no time for.
 */
function nodeChangeComments(tree: TreeAnswer): Map<TreeAnswerNode, TreeAnswerNode> {
  const changes = new Map<TreeAnswerNode, TreeAnswerNode>();
  for (const [node, parent] of parentsIn(tree.nodes ?? [])) {
    if (
      parent.supertag === "decision" &&
      parent.decision?.replaced !== true &&
      node.supertag === "comment" &&
      node.author?.[0]?.kind === "user" &&
      node.made_at !== null &&
      after(node.made_at, parent.decision ?? {})
    ) {
      changes.set(node, parent);
    }
  }
  return changes;
}

/**
 * The node each node of `order` hangs under: the nearest node before it in `order` that its
 * `parent` names, since a parent comes before its children; else the first node of that id,
 * among `first` where the caller hands it, else among `order`. The outline places a node by this
 * rule (`tree-read/tree-nodes.ts`), so a Change is the comment the outline draws under its
 * decision: it walks the answer in an order of its own, and hands the answer's first node of
 * each id.
 */
export function parentsIn(
  order: readonly TreeAnswerNode[],
  first: ReadonlyMap<string, TreeAnswerNode> = firstOfEachId(order)
): Map<TreeAnswerNode, TreeAnswerNode> {
  const parents = new Map<TreeAnswerNode, TreeAnswerNode>();
  const latest = new Map<string, TreeAnswerNode>();
  for (const node of order) {
    const parent =
      node.parent === null ? undefined : (latest.get(node.parent.id) ?? first.get(node.parent.id));
    if (parent !== undefined) parents.set(node, parent);
    latest.set(node.id, node);
  }
  return parents;
}

/** Each node of `nodes` by its id, the first where two share one: the node of an id `parentsIn`
 *  falls back to, and the one the outline looks a read id up by (`tree-read/tree-nodes.ts`). */
export function firstOfEachId(nodes: readonly TreeAnswerNode[]): Map<string, TreeAnswerNode> {
  const first = new Map<string, TreeAnswerNode>();
  for (const node of nodes) {
    if (!first.has(node.id)) first.set(node.id, node);
  }
  return first;
}

/**
 * The Change each decision node waits on, by the id `idOf` gives the node: the newest of the
 * Changes the read hangs under it (`nodeChangeComments`) that its author has not deleted
 * (`deleted`, the comments the page's comments read says were: the tree read answers a deleted
 * comment with no body and no mark). A deleted Change keeps its place under the decision, as the
 * read puts it, and asks nothing any more. The outline (`tree-read/tree-nodes.ts`) names each
 * node by its tree id (`treeIdOfNode`), so two decision nodes of one block id in two documents
 * are two decisions there; a caller that names no id reads the node's own id in the read. The
 * outline, the Decisions filter (`decisions-filter/decisions-entries.ts`) and the header's count
 * (`waiting.ts`) read this one rule.
 */
export function decisionNodeChanges(
  answer: TreeAnswer,
  idOf: (node: TreeAnswerNode) => string = (node) => node.id,
  deleted: ReadonlySet<string> = NO_DELETED
): ReadonlyMap<string, TreeAnswerNode> {
  const changes = new Map<string, TreeAnswerNode>();
  for (const [comment, decision] of nodeChangeComments(answer)) {
    if (deleted.has(comment.id)) continue;
    const id = idOf(decision);
    const held = changes.get(id);
    // Every Change holds a time (`nodeChangeComments`).
    if (held === undefined || compareTimestamps(comment.made_at ?? "", held.made_at ?? "") > 0) {
      changes.set(id, comment);
    }
  }
  return changes;
}

/** No comment is known deleted: the same empty set on every call. */
const NO_DELETED: ReadonlySet<string> = new Set();

/** The ids of the comments among `comments` that their author deleted (`postDeletion`): the
 *  Changes that ask nothing any more (`decisionNodeChanges`). */
export function deletedPostIds(
  comments: readonly Pick<Comment, "deleted_at" | "id">[] | undefined
): ReadonlySet<string> {
  const ids = new Set<string>();
  for (const comment of comments ?? []) {
    if (postDeletion(comment) !== null) ids.add(comment.id);
  }
  return ids.size === 0 ? NO_DELETED : ids;
}

/** Whether a comment made at `at` is newer than the decision node's words and its last Keep, as
 *  its `decision` answers them. */
function after(at: string, row: Partial<Pick<Decision, "changed_at" | "kept">>): boolean {
  const since = [row.changed_at, row.kept?.kept_at].filter(
    (moment): moment is string => typeof moment === "string"
  );
  return since.every((moment) => compareTimestamps(at, moment) > 0);
}
