import type { Comment, Decision, DisplayTree, IssueDecisions } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { wordsOf } from "../refs/tree-fragment";

/**
 * Which of an issue's decisions wait for the reader's review, and which a reader asked to
 * change (dispatch://INFRA-1196 R7, R13, R35).
 *
 * This module is pure and imports nothing of the tree, so the tree (`tree-drafts.ts`), the
 * Decisions filter (`decisions-filter/decisions-parts.ts`) and the header's count of what waits
 * on the reader (`waiting.ts`) read one rule each and cannot disagree.
 *
 * A Change is a comment of the issue anchored to the decision's own words in the decisions
 * document (`DecisionActions.tsx`), which is the write the old Decisions tab made; the decisions
 * read holds no state for it. So the page reads it from the comments it already reads: the
 * newest comment a person anchored to a decision's words since that decision last changed is
 * the change they asked for. The decision then waits on its agent, not on the reader: it says
 * it was changed, it leaves the review list and every count of what waits on the reader, and the
 * change is drawn under it. Once the agent edits the decision, the change is older than its
 * words and the decision waits for review again.
 */

/** The fields of a comment this module reads. */
export type ChangeComment = Pick<Comment, "anchor" | "author" | "body" | "created_at" | "id">;

/** The fields of a decision this module reads. */
type ChangedDecision = Pick<
  Decision,
  "changed_at" | "kept" | "ref" | "replaced" | "text" | "text_tree"
>;

/**
 * Whether `decision` is an entry of the review list (R35): an agent's decision that waits for a
 * person's review - neither kept as it reads now nor replaced, which is the server's
 * `waiting_review` (dispatch://INFRA-1150 R2) - that a `shaped` tie names a statement of this
 * issue for, and that the reader has not asked to change since it last changed (`changed`).
 * `isStatement` answers whether an address names a statement.
 *
 * It is the one rule that decides the list, so the count of what waits on the reader (R13), the
 * filter's "to review" signal and the node's own `waiting` read it too and cannot disagree with
 * the list.
 */
export function inReviewList(
  decision: Pick<Decision, "links" | "waiting_review">,
  isStatement: (reference: string) => boolean,
  changed = false
): boolean {
  return (
    !changed &&
    decision.waiting_review &&
    (decision.links ?? []).some((link) => link.to.ref !== undefined && isStatement(link.to.ref))
  );
}

/**
 * The change a person asked of each decision, by the decision's own address (`Decision.ref`):
 * the newest comment a person anchored to the decision's words in the decisions document, made
 * after the decision last changed and after it was last kept. A decision that was replaced
 * takes no change: nothing waits on it any longer. Comments are matched by their anchor's
 * quote, which is the decision's text as the server matches quotes (`pmdoc.FindQuote`), so a
 * quote of a decision's words names that decision and no other.
 */
export function decisionChanges(
  decisions: Pick<IssueDecisions, "decisions" | "document"> | undefined,
  comments: readonly ChangeComment[] | undefined
): ReadonlyMap<string, ChangeComment> {
  const changes = new Map<string, ChangeComment>();
  const rows = (decisions?.decisions ?? []).filter((row) => !row.replaced);
  if (rows.length === 0 || decisions?.document == null) return changes;
  const document = decisions.document;
  // A decision's whole words as a quote is matched against them: its reading where the read
  // carried one, and its stored text otherwise.
  const words = new Map(
    rows.map((row) => [
      row.ref,
      flat(row.text_tree === undefined ? row.text : wordsOf(row.text_tree as DisplayTree)),
    ])
  );
  for (const comment of comments ?? []) {
    const anchor = comment.anchor;
    if (anchor === null || anchor.artifact_id !== document || comment.author.kind !== "user") {
      continue;
    }
    const quote = flat(anchor.quote);
    if (quote === "") continue;
    const row = rows.find((one) => words.get(one.ref)?.includes(quote) === true);
    if (row === undefined || !after(comment.created_at, row)) continue;
    const held = changes.get(row.ref);
    if (held === undefined || compareTimestamps(comment.created_at, held.created_at) > 0) {
      changes.set(row.ref, comment);
    }
  }
  return changes;
}

/**
 * The words of the decision's first block below its heading, which is what a change is anchored
 * to: the server matches an anchor quote against the document's own text (`pmdoc.FindQuote`),
 * never its markdown, so a list or a hard break quoted as markdown would anchor on nothing. They
 * are read from the tree Go read the decision's text into (`Decision.text_tree`), whose
 * top-level blocks are the paragraphs the markdown would have been split into; an answer that
 * carried no reading - a pod of the build before during a rolling deploy - leaves the first
 * paragraph's own markdown, which is all the browser has. Undefined for a decision whose heading
 * is all it holds.
 */
export function anchorQuote(decision: Pick<Decision, "text" | "text_tree">): string | undefined {
  const content = decision.text_tree?.content;
  if (!Array.isArray(content)) {
    return firstParagraph(decision.text);
  }
  const first = (content as DisplayTree[]).find((node) => node.type !== "heading");
  return first === undefined ? undefined : wordsOf(first);
}

/** The decision's first paragraph below its heading, as markdown: the quote of an answer that
 *  carried no reading of the decision's text. Undefined for a decision whose heading is all it
 *  holds. */
function firstParagraph(text: string): string | undefined {
  return text
    .split(/\n\s*\n/)
    .map((paragraph) => paragraph.trim())
    .find((paragraph) => paragraph !== "" && !paragraph.startsWith("#"));
}

/** `text` with every run of white space one space, so a quote and the words it was taken from
 *  are compared as words. */
function flat(text: string): string {
  return text.replace(/\s+/gu, " ").trim();
}

/** Whether a comment made at `at` is newer than the decision's words and its last Keep. */
function after(at: string, row: ChangedDecision): boolean {
  const since = [row.changed_at, row.kept?.kept_at].filter(
    (moment): moment is string => typeof moment === "string"
  );
  return since.every((moment) => compareTimestamps(at, moment) > 0);
}
