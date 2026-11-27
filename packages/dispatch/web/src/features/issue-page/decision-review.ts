import type {
  Actor,
  Anchor,
  Artifact,
  Comment,
  CommentWithTrees,
  DecisionEntry,
  DisplayTree,
  IssueDecisions,
  IssueDetails,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { isPersonDecision } from "../issue/decision-entries";
import { ownTree, ownWords } from "../issue/decision-text";
import { postDeletion } from "../refs/post-marks";
import { parseNodeReference } from "../refs/routes";
import { wordsOf } from "../refs/tree-fragment";
import type { DocumentBlocks } from "./issue-tree";
import { storedReading } from "./stored-reading";
import { olderCommentsBefore, splitPosition } from "./tree-read/comment-positions";
import type { TreeAnswer, TreeAnswerNode } from "./tree-read/tree-read";

/**
 * Which of an issue's decisions wait for the reader's review, and which a reader asked to
 * change.
 *
 * This module is pure and imports no code of the tree, only the types of a document's blocks and
 * of the tree read and the read's own comment positions (`tree-read/comment-positions.ts`), so the
 * tree (`issue-tree.ts`, `tree-read/tree-nodes.ts`), the Decisions filter
 * (`decisions-filter/decisions-parts.ts`) and the header's count of what waits on the reader
 * (`waiting.ts`) read one rule each and cannot disagree.
 *
 * An agent's decision is a `decision` block of one of the issue's documents, the spec's or
 * another's (`decisionread`): the decisions read's entry for it (`DecisionEntry`) names it by that
 * block's address (`DecisionEntry.ref`) and answers its text as that one block, whose own content
 * is what the decision says (`decisionContent`). A person's decision is the answered ask itself,
 * whose address is the ask's, or a `decision` block whose `decided_by` names a person
 * (`isPersonDecision`): no Change is written on it, and it is on no review list.
 *
 * A Change is a comment of the issue anchored to the decision's own words in the document its
 * block stands in (`DecisionActions.tsx`'s Change composer writes it so);
 * `changeAnchor` gives the quote and the occurrence it is written with, counted over that
 * document's own reading, so it lands on the decision's first block that is no heading though
 * another decision, or the document's text around it, holds the same words. The decisions read
 * holds no state for it. So the page reads it from the comments the tree read answers: every
 * comment a person anchored inside a decision's block, on that decision's words, since that
 * decision last changed is a change they asked for, and is drawn under it
 * (`decisionChangeComments`); the newest is the change the decision waits on (`decisionChanges`).
 * The decision then waits on its agent, not on the reader: it says it was changed, and it leaves
 * the review list and every count of what waits on the reader. Once the agent edits the
 * decision, every change is older than its words and the decision waits for review again. A
 * change its author deleted is no longer asked for: it stays drawn under the decision, as the
 * deleted line (`tree-drafts.ts`'s `addTurns`), but no decision waits on it (`decisionChanges`),
 * so a decision whose every change was deleted waits for review again.
 *
 * What a decision's block holds is every block of its document whose markdown range lies inside
 * the decision's own (`ArtifactBlock.from`, `to`), the decision's block itself among them, in the
 * document's blocks (`DocumentBlocks`). The block an anchor was written in (`Anchor.block_id`) is
 * the lowest block holding the quote that carries an id (`pmdoc.BlockIDForRange`), so it names
 * the decision: the quote alone cannot, since two decisions can hold the same words.
 *
 * A decision node of the tree read is placed by the read, and so is its Change: a comment
 * anchored inside the decision's words hangs under its node, so its Changes are the comments a
 * person wrote that the read hangs under it, made since it last changed and was last kept
 * (`decisionNodeChanges`), and no quote is matched for it. A decision the read holds no node
 * for is found by the decision's block that holds the anchor's block where its document's blocks
 * are given, and by its words alone where they are not. The Decisions filter
 * (`decisions-entries.ts`) reads the blocks of the spec, the plan and the Verification document,
 * each where its decisions stand in it, so it finds such a decision there by its block once they
 * answer, and one of any other document by its words alone; the outline and the header's count
 * read no document's blocks, so they find it by its words alone: the change is then the first
 * decision's whose words hold the quote.
 */

/** The fields of a comment this module reads, and when its author last edited it, which the
 *  Decisions view draws a Change with (`decisions-parts.ts`'s `DecisionChange`). A comment the
 *  tree read answers carries its author's marks only where they are set (`treeComments`): an
 *  absent mark is none, as the contract's node declares it - "not deleted" or "not edited". */
export type ChangeComment = Pick<Comment, "anchor" | "author" | "body" | "created_at" | "id"> &
  Partial<Pick<Comment, "deleted_at" | "edited_at">>;

/** A comment as the tree read answers it (`treeComments`): the fields a Change is found and drawn
 *  by, the server's reading of its words, and who deleted it, where the read answers that. */
export type TreeComment = ChangeComment &
  Pick<CommentWithTrees, "body_tree"> &
  Partial<Pick<Comment, "deleted_by">>;

/** No comment: the same empty list on every call. */
const NO_COMMENTS: readonly TreeComment[] = [];

/** The author of a comment node the read names none for, which is no person's, so it is no
 *  Change (`decisionChangeComments`). */
const NO_AUTHOR: Actor = { id: "", kind: "" };

/** The comments of each answer `treeComments` has read, one list per answer, held by the answer
 *  and gone with it. */
const commentsOfAnswer = new WeakMap<TreeAnswer, readonly TreeComment[]>();

/**
 * The comments the tree read's `answer` holds, every page of it merged: each comment node, with
 * its document anchor (none for a comment on a node, which no quote is matched in), its first
 * author, its words and the server's reading of them, when it was made, and what its author did
 * to it (`edited_at`, `deleted_at`, `deleted_by`, `TreeAnswerNode`). The answer holds every comment
 * the page draws and every comment on the text of a node it returns, so the outline's post marks
 * and Changes (`tree-read/tree-nodes.ts`, `tree-drafts.ts`' `addDecisions`), the header's count
 * (`waiting.ts`) and the Decisions filter (`decisions-filter/decisions-entries.ts`) read the
 * comments from it and the page reads no comment list of its own. One list for each answer, so
 * every one of them reads the same list.
 */
export function treeComments(answer: TreeAnswer | undefined): readonly TreeComment[] {
  if (answer === undefined) return NO_COMMENTS;
  const held = commentsOfAnswer.get(answer);
  if (held !== undefined) return held;
  const comments: TreeComment[] = [];
  const nodes: readonly TreeAnswerNode[] = answer.nodes ?? [];
  for (const node of nodes) {
    if (node.supertag !== "comment") continue;
    comments.push({
      anchor: documentAnchor(node.anchor),
      author: node.author?.[0] ?? NO_AUTHOR,
      body: node.text,
      ...(node.text_tree === undefined ? {} : { body_tree: node.text_tree }),
      created_at: node.made_at ?? "",
      // An answer leaves out a mark that is none: the post was not edited, or not deleted.
      ...(node.deleted_at === undefined ? {} : { deleted_at: node.deleted_at }),
      ...(node.deleted_by === undefined ? {} : { deleted_by: node.deleted_by }),
      ...(node.edited_at === undefined ? {} : { edited_at: node.edited_at }),
      id: node.id,
    });
  }
  commentsOfAnswer.set(answer, comments);
  return comments;
}

/** A comment node's anchor as a comment's (`Anchor`) where it is in a document, and none where it
 *  is on a node or where there is none. */
function documentAnchor(anchor: TreeAnswerNode["anchor"]): Anchor | null {
  if (anchor?.artifact_id === undefined) return null;
  return {
    artifact_id: anchor.artifact_id,
    block_id: anchor.block_id ?? null,
    mark_id: anchor.mark_id ?? "",
    orphaned: anchor.orphaned,
    quote: anchor.quote,
    version: anchor.version ?? 0,
  };
}

/** The fields of a decision this module reads: an entry of the decisions read. */
type ChangedDecision = Pick<
  DecisionEntry,
  "changed_at" | "document" | "kept" | "node" | "ref" | "replaced" | "text" | "text_tree"
>;

/** The fields of the issue read a decision's document is found by, where the decisions read names
 *  none (`decisionDocumentId`): its key, its documents and which of them is its spec. Every view
 *  that finds a decision's document hands it the issue read it already holds: the Decisions tab,
 *  the tree, the header's count, the Decisions filter and the margin's decision cards. */
export type DecisionDocuments = Pick<IssueDetails, "key" | "primary_artifact_id"> & {
  readonly artifacts?: readonly Pick<Artifact, "id" | "slug">[] | null;
};

/**
 * Whether `decision` is an entry of the review list (R35): an agent's decision that waits for a
 * person's review - neither kept as it reads now nor replaced, which is the server's
 * `waiting_review` - that a link ties to a statement of this issue, and that the reader has not
 * asked to change since it last changed (`changed`).
 * `isCoverageStatement` answers whether an address names a statement as the list reads one, a
 * requirement the coverage read holds: a caller passes `coverageStatements(issue, coverage)`
 * (`tree-drafts.ts`), not the rows' `isStatement` (`issue-tree.ts`).
 *
 * It is the one rule that decides the list, so the count of what waits on the reader (R13), the
 * filter's "to review" signal and the node's own `waiting` read it too and cannot disagree with
 * the list.
 */
export function inReviewList(
  decision: Pick<DecisionEntry, "shaped" | "waiting_review">,
  isCoverageStatement: (reference: string) => boolean,
  changed = false
): boolean {
  return (
    !changed &&
    decision.waiting_review &&
    (decision.shaped ?? []).some(
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
 * What `decision` says: the decisions read answers a decision's text as its one `decision` block
 * (`decisionread`'s words, the block rendered alone) and the server's reading of it as that
 * block's (`DecisionEntry.text_tree`), so its own words are that text with the lines that open and
 * close the block left out (`ownWords`), and its own reading is the block's content at the top
 * level (`ownTree`), where a section's blocks stood. A text or a reading that is no such block is
 * its own already. `tree` is absent where the read sent no reading. Every reader of a decision's
 * words reads them here: the tree's node (`tree-drafts.ts`) and a Change's quote (`anchorQuote`).
 */
export function decisionContent(
  decision: Pick<DecisionEntry, "text" | "text_tree">
): DecisionContent {
  const text = ownWords(decision.text);
  const tree = ownTree(decision.text_tree);
  return tree === undefined ? { text } : { text, tree };
}

/**
 * The id of the document whose block `decision` is. The Decisions view of the issue's tree names
 * it (`DecisionEntry.document`) only for a typed block of a role document - the spec, the plan,
 * the Verification document - whether or not the issue read has listed the issue's documents
 * yet; for a decision of any other document it names none, and the decision's own address
 * (`DecisionEntry.ref`, `artifact/<slug>#b-…`) names the document, found among the issue's
 * documents once the issue read lists them. A Change's anchor names the document it is anchored
 * in by that id (`Anchor.artifact_id`), a Keep is written against it, and an agent's decision's
 * words are its record (`decision-entries.ts`'s `entryTextSource`). Undefined for an entry that
 * names none and whose address names no block of the issue's documents: an ask, whose address
 * names the ask and no block (its words are the ask's own record), a block of a document the
 * issue read does not list yet, and an address of another issue. Undefined too for an entry that
 * names none while `issue` is undefined, where the caller's issue read has not answered.
 */
export function decisionDocumentId(
  issue: DecisionDocuments | undefined,
  decision: Pick<DecisionEntry, "document" | "ref">
): string | undefined {
  const named = decision.document?.artifact_id;
  if (named !== undefined) return named;
  // The view named no document: read the one the decision's own address names.
  if (issue === undefined) return undefined;
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
 * Each of the issue's decisions a Change can be asked of, by the document its block stands in
 * (`decisionDocumentId`), replaced ones included: their blocks hold blocks too, and a comment
 * anchored in one changes no other decision. Those are the decisions an agent made: an agent's
 * `decision` block. A person's decision (`isPersonDecision`) is none - an answered ask, whatever
 * document its block stands in, and a `decision` block whose `decided_by` names a person - since
 * what a person decided is theirs and every view places it as its decider's, and no Change is
 * written on it. Nor is an ask that is open or declined, which decided nothing.
 */
function changeableByDocument(
  issue: DecisionDocuments,
  decisions: Pick<IssueDecisions, "entries"> | undefined
): ReadonlyMap<string, readonly ChangedDecision[]> {
  const byDocument = new Map<string, ChangedDecision[]>();
  for (const row of decisions?.entries ?? []) {
    const document =
      row.node.kind === "ask" || isPersonDecision(row) ? undefined : decisionDocumentId(issue, row);
    if (document === undefined) continue;
    const held = byDocument.get(document);
    if (held === undefined) byDocument.set(document, [row]);
    else held.push(row);
  }
  return byDocument;
}

/**
 * The documents a Change of one of the issue's decisions can be anchored in
 * (`changeableByDocument`). Until part U only the Decisions filter reads the blocks of any of
 * them (part Q's seam, `decisions-filter/decisions-entries.ts`): those of the spec, the plan and
 * the Verification document, where they are among them, which name the decision whose block
 * holds a Change's anchor (`decisionChangeComments`); a Change in any other is matched by its
 * quote alone.
 * The outline and the header's count read no block of a document to match a Change (R3): each
 * takes it from the comment the tree read hangs under a decision node, and for a decision the
 * tree holds no node of, by the quote alone.
 */
export function changeDocumentIds(
  issue: DecisionDocuments,
  decisions: Pick<IssueDecisions, "entries"> | undefined
): ReadonlySet<string> {
  return new Set(changeableByDocument(issue, decisions).keys());
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
  // The tree read names a document only on a typed block of a role document, so the entry of a
  // decision of any other document names none (`api/decisions-tree.ts`): its own address names
  // the block a Keep is written against and a Change is placed by.
  const route = parseNodeReference(decision.ref);
  return route !== undefined && "block" in route ? route.block?.id : undefined;
}

/** The block id the tree read names `node` by: a typed block's own (`block_id`), else the node's
 *  id, which names a node that is no block. */
export function blockIdOfNode(node: Pick<TreeAnswerNode, "block_id" | "id">): string {
  return node.block_id ?? node.id;
}

/**
 * The decision node of the tree read each decision of the decisions read is, by the decision's
 * address (`DecisionEntry.ref`): a decision node of this issue whose block id (`block_id`) is the
 * decision's block (`decisionBlockId`), in the document the entry names
 * (`DecisionEntry.document`), or the one its address names where it names none, wherever the node
 * names its document. Two documents can each hold a decision block of one id, and each is a
 * decision of its own, so the other document's is not this decision's node. A decision no node is
 * is not held: a section of the decisions document a server of the release before answers, or a
 * person's decision, which is the answered ask.
 */
export function decisionNodes(
  decisions: readonly Pick<DecisionEntry, "document" | "ref">[],
  tree: TreeAnswer | undefined
): ReadonlyMap<string, TreeAnswerNode> {
  const nodes = new Map<string, TreeAnswerNode[]>();
  for (const node of tree?.nodes ?? []) {
    if (node.supertag !== "decision" || node.issue !== undefined) continue;
    const block = blockIdOfNode(node);
    const same = nodes.get(block);
    if (same === undefined) nodes.set(block, [node]);
    else same.push(node);
  }
  const held = new Map<string, TreeAnswerNode>();
  for (const decision of decisions) {
    const block = decisionBlockId(decision);
    if (block === undefined) continue;
    const artifact = decision.document?.artifact_id;
    const route = parseNodeReference(decision.ref);
    const slug = route?.kind === "artifact" ? route.slug : undefined;
    const node = nodes
      .get(block)
      ?.find(
        (one) =>
          one.document === undefined ||
          (artifact === undefined
            ? one.document.slug === slug
            : one.document.artifact_id === artifact)
      );
    if (node !== undefined) held.set(decision.ref, node);
  }
  return held;
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
function nodeChangeComments(tree: TreeAnswer | undefined): Map<TreeAnswerNode, TreeAnswerNode> {
  const changes = new Map<TreeAnswerNode, TreeAnswerNode>();
  for (const [node, parent] of parentsIn(tree?.nodes ?? [])) {
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
 * (`deleted`, the comments the answer marks deleted, `treeComments`: the read answers a deleted
 * comment with no body). A deleted Change keeps its place under the decision, as the
 * read puts it, and asks nothing any more, as `decisionChanges` has it for a Change found by its
 * quote. The outline (`tree-read/tree-nodes.ts`) names each node by its tree id
 * (`treeIdOfNode`), so two decision nodes of one block id in two documents are two decisions
 * there; a caller that names no id reads the node's own id in the read. The outline and the
 * header's count (`waiting.ts`, through `decisionChangeComments`) read this one rule.
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
 *  Changes that ask nothing any more, whichever way a Change is found. */
export function deletedPostIds(
  comments: readonly Pick<ChangeComment, "deleted_at" | "id">[] | undefined
): ReadonlySet<string> {
  const ids = new Set<string>();
  for (const comment of comments ?? []) {
    if (postDeletion(comment) !== null) ids.add(comment.id);
  }
  return ids.size === 0 ? NO_DELETED : ids;
}

/**
 * Each change a person asked of a decision, by the comment's id, to the decision's own address
 * (`DecisionEntry.ref`). A decision a node of the tree read holds (`decisionNodes`) takes the
 * Changes the read hangs under that node (`nodeChangeComments`). Any other takes every comment a
 * person wrote, anchored in the document a decision's block stands in (`decisionDocumentId`) on
 * the words of the decision whose block holds the anchor's block (`sectionsOf`), made after the
 * decision last changed and after it was last kept. A decision that was replaced takes no change:
 * nothing waits on it any longer. A quote is matched against the decision's words as the server
 * matches quotes (`pmdoc.FindQuote`), on its text and not its markdown: the words of its own
 * reading (`decisionWords`).
 *
 * The decision is read from `documents`, the blocks of the issue's documents the page has read.
 * Only where the anchor stores no block, or that document's blocks are not read or do not hold
 * the anchor's block yet, does the quote alone decide: the change is the first decision of that
 * document whose words hold it. A block no decision's block holds belongs to no decision, and a
 * comment anchored there is no change. Every comment it holds is drawn under its decision; one
 * older than the decision's words is no change and is not held.
 */
export function decisionChangeComments(
  decisions: Pick<IssueDecisions, "entries"> | undefined,
  comments: readonly ChangeComment[] | undefined,
  issue: DecisionDocuments,
  documents: readonly DocumentBlocks[] | undefined,
  tree?: TreeAnswer
): ReadonlyMap<string, string> {
  const changes = new Map<string, string>();
  const standing = (decisions?.entries ?? []).filter((row) => !row.replaced);
  const onNodes = decisionNodes(standing, tree);
  const byNode = new Map([...onNodes].map(([ref, node]) => [node, ref]));
  for (const [comment, node] of nodeChangeComments(tree)) {
    const ref = byNode.get(node);
    if (ref !== undefined) changes.set(comment.id, ref);
  }
  // Every decision that stands is a node, so no quote is matched for any.
  if (standing.every((row) => onNodes.has(row.ref))) return changes;
  const byDocument = changeableByDocument(issue, decisions);
  // A decision's whole words as a quote is matched against them (`decisionWords`), for each one a
  // change can be asked of by a quote: none a node of the tree read holds.
  const words = new Map(
    [...byDocument.values()]
      .flat()
      .filter((row) => !row.replaced && !onNodes.has(row.ref))
      .map((row) => [row.ref, flat(decisionWords(row))])
  );
  if (words.size === 0) return changes;
  const holds = (row: ChangedDecision, quote: string): boolean =>
    words.get(row.ref)?.includes(quote) === true;
  // The decision each block sits in, by document, read once for the first comment of each.
  const sections = new Map<string, ReadonlyMap<string, string | null>>();
  for (const comment of comments ?? []) {
    const anchor = comment.anchor;
    if (anchor === null || comment.author.kind !== "user") continue;
    const rows = byDocument.get(anchor.artifact_id);
    if (rows === undefined) continue;
    const quote = flat(anchor.quote);
    if (quote === "") continue;
    let placed = sections.get(anchor.artifact_id);
    if (placed === undefined) {
      placed = sectionsOf(
        rows,
        documents?.find((one) => one.artifactId === anchor.artifact_id)?.blocks
      );
      sections.set(anchor.artifact_id, placed);
    }
    const section = anchor.block_id ? placed.get(anchor.block_id) : undefined;
    const row =
      section === undefined
        ? rows.find((one) => holds(one, quote))
        : rows.find((one) => one.ref === section && holds(one, quote));
    if (row !== undefined && after(comment.created_at, row)) changes.set(comment.id, row.ref);
  }
  return changes;
}

/**
 * The decision each block of one document sits in, by the block's id, to the decision's own
 * address: the decision whose own block (`decisionBlockId`) is that block or holds it - its
 * markdown range covers the block's - a replaced decision's included, and `null` for a block no
 * decision's block holds. Empty where the document's blocks are not read.
 */
function sectionsOf(
  decisions: readonly ChangedDecision[],
  blocks: DocumentBlocks["blocks"] | undefined
): ReadonlyMap<string, string | null> {
  const own = new Map(
    decisions.flatMap((row) => {
      const block = decisionBlockId(row);
      return block === undefined ? [] : [[block, row.ref] as const];
    })
  );
  const spans = (blocks ?? []).flatMap((block) => {
    const ref = own.get(block.id);
    return ref === undefined ? [] : [{ from: block.from, ref, to: block.to }];
  });
  const sections = new Map<string, string | null>();
  for (const block of blocks ?? []) {
    const holder = spans.find((span) => span.from <= block.from && block.to <= span.to);
    sections.set(block.id, holder?.ref ?? null);
  }
  return sections;
}

/**
 * The change each decision waits on, by the decision's own address (`DecisionEntry.ref`): the
 * newest of the changes a person asked of it (`decisionChangeComments`, which `placed` holds where
 * the caller has read it already) that its author has not deleted. A deleted change keeps its
 * place under the decision (`decisionChangeComments` still holds it), but asks nothing any more: a
 * decision whose every change was deleted waits for review again, as it did before the first.
 * `documents` are the blocks of the issue's documents the page has read, `undefined` where it has
 * read none. A decision with none is not held. Each change is the comment as `comments` holds it,
 * so a caller reads whatever else its read gave of it.
 */
export function decisionChanges<Change extends ChangeComment>(
  decisions: Pick<IssueDecisions, "entries"> | undefined,
  comments: readonly Change[] | undefined,
  issue: DecisionDocuments,
  documents: readonly DocumentBlocks[] | undefined,
  placed: ReadonlyMap<string, string> = decisionChangeComments(
    decisions,
    comments,
    issue,
    documents
  )
): ReadonlyMap<string, Change> {
  const changes = new Map<string, Change>();
  for (const comment of comments ?? []) {
    const ref = placed.get(comment.id);
    if (ref === undefined || postDeletion(comment) !== null) continue;
    const held = changes.get(ref);
    if (held === undefined || compareTimestamps(comment.created_at, held.created_at) > 0) {
      changes.set(ref, comment);
    }
  }
  return changes;
}

/**
 * The decision's first block that is no heading, which is what a change is anchored to, as a
 * reading of that block alone: the server matches an anchor quote against the document's own
 * text (`pmdoc.FindQuote`), never its markdown, so a list or a hard break quoted as markdown
 * would anchor on nothing. It is read from the decision's reading (`readingOf`), whose top-level
 * blocks are the blocks of the decision's own content: the tree Go read the decision's text into
 * (`DecisionEntry.text_tree`), and where the read carried none that holds blocks - a pod of the
 * build before during a rolling deploy - the reading of its stored text (`storedReading`).
 * Undefined for a decision whose headings are all it holds. The Change draws this block's
 * plain-text projection (`DecisionActions.tsx`), and anchors to its words (`anchorQuote`).
 */
export function anchorBlock(
  decision: Pick<DecisionEntry, "text" | "text_tree">
): DisplayTree | undefined {
  const reading = readingOf(decision);
  const blocks = reading.content;
  const block = Array.isArray(blocks)
    ? (blocks as DisplayTree[]).find((node) => node.type !== "heading")
    : undefined;
  return block === undefined ? undefined : { ...reading, content: [block] };
}

/** The words a change is anchored to: the words of the decision's first block that is no heading
 *  (`anchorBlock`), as the document's own text holds them, so a decision's quote is the words of
 *  its text either way, never its markdown. Undefined for a decision whose headings are all it
 *  holds. */
export function anchorQuote(
  decision: Pick<DecisionEntry, "text" | "text_tree">
): string | undefined {
  const block = anchorBlock(decision);
  return block === undefined ? undefined : wordsOf(block);
}

/**
 * The anchor a Change of a decision is written with: the quote (`anchorQuote`) and its zero-based
 * occurrence in the text of the document the decision's block stands in. The occurrence is absent
 * where that document's reading is not in hand, or does not line up with the decisions read
 * (`changeAnchor`). A write without it is anchored by the server where the quote stands once in
 * the document, and refused (409) where it stands more than once, rather than landed on the wrong
 * place.
 */
export interface ChangeAnchor {
  readonly occurrence?: number;
  readonly quote: string;
}

/**
 * The anchor a Change of `decision` is written with: the quote `anchorQuote` gives, and which of
 * that quote's places in the decision's document is the decision's own first block that is no
 * heading. Two decisions can open with the same sentence, and the spec's text around a decision
 * can hold it too, and the server refuses a quote that matches more than one place unless the
 * write names its zero-based occurrence (409, "matches 2 places; pass a zero-based occurrence");
 * so a Change names it. Undefined for a decision whose headings are all it holds.
 *
 * The server counts the quote's places over the whole document (`docs.MarkQuote`,
 * `pmdoc.FindQuote`), so this counts over it too: the text before the decision's block, every
 * other decision's block, a replaced one's included, and a heading's words. The blocks read
 * (`ArtifactBlock`) cannot serve: it holds each block's id, type and markdown range, and no text.
 * So the caller passes `document`, the reading of the document the decision's block stands in
 * (`decisionDocumentId`): the `markdown_tree` of its text read (`api.getArtifactText`), read once
 * when the reader sends the Change.
 *
 * The document's text is every text node's text joined, with one space wherever two text runs are
 * not adjacent, as between two blocks or two list items (`buildFlattenedText`): the words of the
 * reading (`wordsOf`). A quote's matches are every exact match in that text, overlapping ones
 * included, in document order (`findAll`), and the occurrence picks one of them: it is the number
 * of matches that start before the decision's first block that is no heading. A match that starts
 * in a heading and runs into the block counts too, since the server lists it first. Where the
 * block's words start is read off the nodes above it, never found by searching for the quote.
 *
 * The decision is found in the reading as its `decision` block, whose id (`attrs.blockId`) is the
 * decision's own (`decisionBlockId`), at whatever depth it stands. The reading lines up with the
 * read only where that block is there and its first child that is no heading is a block whose
 * words are the quote. Otherwise, and where `document` is undefined, the anchor carries no
 * occurrence (`ChangeAnchor`): a reading of another version than the read, one that lacks the
 * decision, or one whose block opens with other words.
 *
 * One assumption remains: the reading's text runs are the stored document's, which the server
 * makes its display reading from.
 */
export function changeAnchor(
  decision: Pick<DecisionEntry, "document" | "ref" | "text" | "text_tree">,
  document: DisplayTree | undefined
): ChangeAnchor | undefined {
  const quote = anchorQuote(decision);
  if (quote === undefined) return undefined;
  if (document === undefined) return { quote };
  const blockId = decisionBlockId(decision);
  const path = blockId === undefined ? undefined : decisionPath(document, blockId);
  const owner = path === undefined ? undefined : nodeAt(document, path);
  const children = owner?.content;
  const first = Array.isArray(children)
    ? (children as DisplayTree[]).findIndex((node) => node.type !== "heading")
    : -1;
  const block = path === undefined || first === -1 ? undefined : (children as DisplayTree[])[first];
  if (path === undefined || block === undefined || wordsOf(block) !== quote) return { quote };
  // The block's words start one space after the words of the nodes above it, where those hold
  // any: `wordsOf` writes that space before a block, as the server does.
  const above = wordsOf(before(document, [...path, first]));
  const start = above === "" ? 0 : above.length + 1;
  return { occurrence: matchesBefore(wordsOf(document), quote, start), quote };
}

/** Where the `decision` block of id `blockId` stands in `node`'s reading, as the index of each
 *  node on the way down to it, at whatever depth; undefined where the reading holds none. */
function decisionPath(node: DisplayTree, blockId: string): number[] | undefined {
  const content = node.content;
  if (!Array.isArray(content)) return undefined;
  for (const [index, child] of (content as DisplayTree[]).entries()) {
    if (child.type === "decision" && blockIdOf(child) === blockId) return [index];
    const below = decisionPath(child, blockId);
    if (below !== undefined) return [index, ...below];
  }
  return undefined;
}

/** A typed block's id in a reading (`attrs.blockId`), and undefined for a node that holds none. */
function blockIdOf(node: DisplayTree): string | undefined {
  const attrs = node.attrs;
  const id =
    typeof attrs === "object" && attrs !== null && "blockId" in attrs ? attrs.blockId : undefined;
  return typeof id === "string" ? id : undefined;
}

/** The node at `path` below `node` (`decisionPath`). */
function nodeAt(node: DisplayTree, path: readonly number[]): DisplayTree | undefined {
  let at: DisplayTree | undefined = node;
  for (const index of path) {
    const content: unknown = at?.content;
    at = Array.isArray(content) ? (content as DisplayTree[])[index] : undefined;
  }
  return at;
}

/** `node` cut to what stands before the node at `path` below it: on the way down, each node's
 *  children before the one the path goes on to, then that one cut the same way, and at the end
 *  of the path its children before the node it names. Its words are the words above that node. */
function before(node: DisplayTree, path: readonly number[]): DisplayTree {
  const [index = 0, ...rest] = path;
  const content = Array.isArray(node.content) ? (node.content as DisplayTree[]) : [];
  const kept = content.slice(0, index);
  const next = content[index];
  return {
    ...node,
    content: rest.length === 0 || next === undefined ? kept : [...kept, before(next, rest)],
  };
}

/** A decision's whole words, as a Change's quote is matched against them: the words of its
 *  reading (`readingOf`), which `anchorQuote` reads its first block from too. */
function decisionWords(row: Pick<DecisionEntry, "text" | "text_tree">): string {
  return wordsOf(readingOf(row));
}

/** The reading a decision is read by, the one rule this module reads a decision by: the server's
 *  reading of its own content (`decisionContent`) where the read carried one that holds blocks,
 *  and otherwise the reading of its own stored words (`storedReading`), which the row holds, so it
 *  is remembered while the row lives. A reading with no `content` holds no blocks and is none, so
 *  a decision whose read sent one is read by its stored words. */
function readingOf(row: Pick<DecisionEntry, "text" | "text_tree">): DisplayTree {
  const own = decisionContent(row);
  return own.tree !== undefined && Array.isArray(own.tree.content)
    ? own.tree
    : storedReading(own.text, row);
}

/** How many places of `text` hold `quote` and start before `end`, overlapping ones included, as
 *  the server lists a quote's matches (`pmdoc.findAll`); an empty quote matches nowhere, as
 *  there. */
function matchesBefore(text: string, quote: string, end: number): number {
  if (quote === "") return 0;
  let count = 0;
  for (let at = text.indexOf(quote); at !== -1 && at < end; at = text.indexOf(quote, at + 1)) {
    count += 1;
  }
  return count;
}

/** `text` with every run of white space one space, so a quote and the words it was taken from
 *  are compared as words. */
function flat(text: string): string {
  return text.replace(/\s+/gu, " ").trim();
}

/** Whether a comment made at `at` is newer than the decision's words and its last Keep: a row of
 *  the decisions read, or a decision node's `decision`, which answers both by the same names. */
function after(at: string, row: Partial<Pick<DecisionEntry, "changed_at" | "kept">>): boolean {
  const since = [row.changed_at, row.kept?.kept_at].filter(
    (moment): moment is string => typeof moment === "string"
  );
  return since.every((moment) => compareTimestamps(at, moment) > 0);
}

/**
 * Whether a Change of `decision` may stand among the comments the tree read's `answer` does not
 * hold: the read pages the issue's comments, the newest first, and an answer with an older page
 * names where it starts (`comments_before`, `<made_at>,<id>` of the oldest comment it holds,
 * `tree-read/comment-positions.ts`' `olderCommentsBefore`). A Change is newer than the decision's
 * words and its last Keep (`after`), so a comment older than the oldest held can be one only where
 * that oldest comment is newer than both. A decision drawn with no Change it may have then carries
 * the page's mark of a count with more to read (`tree-drafts.ts`' `markChangeUnread`), rather than
 * reading as unchanged.
 */
export function changeMayBeOlder(
  decision: Partial<Pick<DecisionEntry, "changed_at" | "kept">>,
  answer: TreeAnswer | undefined
): boolean {
  const before = answer === undefined ? undefined : olderCommentsBefore(answer);
  return before !== undefined && after(splitPosition(before)[0], decision);
}
