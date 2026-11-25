import type { Artifact, Comment, Decision, IssueDecisions } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { decisionsDocumentId, decisionWords, flat } from "../decision-card/change-anchor";
import { postDeletion } from "../refs/post-marks";
import type { DocumentBlocks } from "./issue-tree";

/**
 * Which of an issue's decisions wait for the reader's review, and which a reader asked to
 * change.
 *
 * This module is pure and imports no code of the tree, only the type of a document's blocks, so
 * the tree (`tree-drafts.ts`), the Decisions filter (`decisions-filter/decisions-parts.ts`) and
 * the header's count of what waits on the reader (`waiting.ts`) read one rule each and cannot
 * disagree.
 *
 * A Change is a comment of the issue anchored to the decision's own words in the decisions
 * document (`DecisionActions.tsx`), which is the write the old Decisions tab made; `changeAnchor`
 * gives the quote and the occurrence it is written with, counted over the decisions document's
 * own reading, so it lands on the decision's first block below its heading though an earlier
 * decision, or the document's text above its first decision, holds the same words. The decisions
 * read holds no state for it. So the page reads it from the comments it already reads: every
 * comment a person anchored in a decision's section of the decisions document, on that
 * decision's words, since that decision last changed is a change they asked for, and is drawn
 * under it (`decisionChangeComments`); the newest is the change the decision waits on
 * (`decisionChanges`). The decision then waits on its agent, not on the reader: it says it was
 * changed, and it leaves the review list and every count of what waits on the reader. Once the
 * agent edits the decision, every change is older than its words and the decision waits for
 * review again. A change its author deleted is no longer asked for: it stays drawn under the
 * decision, as the deleted line (`tree-drafts.ts`'s `addTurns`), but no decision waits on it
 * (`decisionChanges`), so a decision whose every change was deleted waits for review again.
 *
 * A decision's section is its own block (`Decision.block_id`, its heading) and every block after
 * it up to the next decision's, in the decisions document's blocks (`DocumentBlocks`). The block
 * an anchor was written in (`Anchor.block_id`) names the section, and so the decision: the quote
 * alone cannot, since two decisions can hold the same words.
 */

/** The fields of a comment this module reads, and when its author last edited it, which the
 *  Decisions view draws a Change with (`decisions-parts.ts`'s `DecisionChange`). */
export type ChangeComment = Pick<
  Comment,
  "anchor" | "author" | "body" | "created_at" | "deleted_at" | "edited_at" | "id"
>;

/** The fields of a decision this module reads. */
type ChangedDecision = Pick<
  Decision,
  "block_id" | "changed_at" | "kept" | "ref" | "replaced" | "text" | "text_tree"
>;

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
  decision: Pick<Decision, "links" | "waiting_review">,
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

/**
 * Each change a person asked of a decision, by the comment's id, to the decision's own address
 * (`Decision.ref`): every comment a person wrote, anchored in the decisions document (the issue's
 * document at the read's address, `decisionsDocumentId`) on the words of the decision whose
 * section holds the anchor's block (`sectionsOf`), made after the decision last changed and
 * after it was last kept. A decision that was replaced takes no change: nothing waits on it any
 * longer. A quote is matched against the decision's words as the server matches quotes
 * (`pmdoc.FindQuote`), on its text and not its markdown: the words of its text's reading
 * (`decisionWords`).
 *
 * The section is read from `documents`, the blocks of the issue's documents the page has read.
 * Only where the anchor stores no block, or the decisions document's blocks are not read or do not
 * hold the anchor's block yet, does the quote alone decide: the change is the first decision's
 * whose words hold it. A block above the first decision belongs to no decision, and a comment
 * anchored there is no change. Every comment it holds is drawn under its decision; one older than
 * the decision's words is no change and is not held.
 */
export function decisionChangeComments(
  decisions: Pick<IssueDecisions, "decisions" | "document"> | undefined,
  comments: readonly ChangeComment[] | undefined,
  artifacts: readonly Pick<Artifact, "id" | "slug">[] | null | undefined,
  documents: readonly DocumentBlocks[] | undefined
): ReadonlyMap<string, string> {
  const changes = new Map<string, string>();
  const rows = (decisions?.decisions ?? []).filter((row) => !row.replaced);
  const document = decisionsDocumentId(artifacts, decisions);
  if (rows.length === 0 || document === undefined) return changes;
  // A decision's whole words as a quote is matched against them (`decisionWords`).
  const words = new Map(rows.map((row) => [row.ref, flat(decisionWords(row))]));
  const sections = sectionsOf(
    decisions?.decisions ?? [],
    documents?.find((one) => one.artifactId === document)?.blocks
  );
  for (const comment of comments ?? []) {
    const anchor = comment.anchor;
    if (anchor === null || anchor.artifact_id !== document || comment.author.kind !== "user") {
      continue;
    }
    const quote = flat(anchor.quote);
    if (quote === "") continue;
    const holds = (row: ChangedDecision): boolean => words.get(row.ref)?.includes(quote) === true;
    const section = anchor.block_id ? sections.get(anchor.block_id) : undefined;
    const row =
      section === undefined
        ? rows.find(holds)
        : rows.find((one) => one.ref === section && holds(one));
    if (row !== undefined && after(comment.created_at, row)) changes.set(comment.id, row.ref);
  }
  return changes;
}

/**
 * The decision each block of the decisions document sits in, by the block's id, to the
 * decision's own address: the decision whose own block (`Decision.block_id`, its heading) is the
 * last decision block at or before it in document order, a replaced decision's included, and
 * `null` for a block above the first decision. Empty where the blocks are not read.
 */
function sectionsOf(
  decisions: readonly ChangedDecision[],
  blocks: DocumentBlocks["blocks"] | undefined
): ReadonlyMap<string, string | null> {
  const headings = new Map(decisions.map((row) => [row.block_id, row.ref]));
  const sections = new Map<string, string | null>();
  let section: string | null = null;
  for (const block of blocks ?? []) {
    section = headings.get(block.id) ?? section;
    sections.set(block.id, section);
  }
  return sections;
}

/**
 * The change each decision waits on, by the decision's own address (`Decision.ref`): the newest
 * of the changes a person asked of it (`decisionChangeComments`, which `placed` holds where the
 * caller has read it already) that its author has not deleted. A deleted change keeps its place
 * under the decision (`decisionChangeComments` still holds it), but asks nothing any more: a
 * decision whose every change was deleted waits for review again, as it did before the first.
 * `documents` are the blocks of the issue's documents the page has read, `undefined` where it
 * has read none. A decision with none is not held. Each change is the comment as `comments`
 * holds it, so a caller reads whatever else its read gave of it.
 */
export function decisionChanges<Change extends ChangeComment>(
  decisions: Pick<IssueDecisions, "decisions" | "document"> | undefined,
  comments: readonly Change[] | undefined,
  artifacts: readonly Pick<Artifact, "id" | "slug">[] | null | undefined,
  documents: readonly DocumentBlocks[] | undefined,
  placed: ReadonlyMap<string, string> = decisionChangeComments(
    decisions,
    comments,
    artifacts,
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

/** Whether a comment made at `at` is newer than the decision's words and its last Keep. */
function after(at: string, row: ChangedDecision): boolean {
  const since = [row.changed_at, row.kept?.kept_at].filter(
    (moment): moment is string => typeof moment === "string"
  );
  return since.every((moment) => compareTimestamps(at, moment) > 0);
}
