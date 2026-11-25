import type { DecisionEntry, DisplayTree } from "../../api/types";

/** The words a node holds, its children's included: what `pmdoc.FindQuote` matches a quote
 *  against, since a quote names the document's text and never its markdown. The server writes
 *  one space wherever two text runs are not adjacent in the document's own positions
 *  (`pmdoc.buildFlattenedText`), which is wherever anything was entered, left or passed between
 *  them: the break inside a wrapped line, or the end of one list item and the start of the
 *  next. Two runs are adjacent only as consecutive children of one parent, so everything else
 *  this walk crosses opens the space the server wrote. */
export function wordsOf(node: DisplayTree): string {
  let words = "";
  let apart = false;
  const read = (current: DisplayTree): void => {
    if (typeof current.text === "string") {
      words += apart && words !== "" ? ` ${current.text}` : current.text;
      apart = false;
      return;
    }
    // The node's own open position, and its close once its children are read; a leaf that is no
    // text, a hard break among them, takes the one position it is.
    apart = true;
    const content = current.content;
    if (Array.isArray(content)) {
      for (const child of content as DisplayTree[]) {
        read(child);
      }
      apart = true;
    }
  };
  read(node);
  return words;
}

/** A decision's text reads as its one `decision` block; what a view draws is that block's own
 *  content. */
const DECISION_OPENING = /^:{3,}decision(\{[^\n]*\})?\n/;
const DECISION_CLOSING = /\n:{3,}\n*$/;

/** The markdown of a decision's own content: its words without the lines that open and close its
 *  block. */
export function ownWords(text: string): string {
  return DECISION_OPENING.test(text)
    ? text.replace(DECISION_OPENING, "").replace(DECISION_CLOSING, "\n")
    : text;
}

/** The tree of a decision's own content: the content of the one `decision` block its text reads
 *  as, at the top level, where a section's blocks stood. */
export function ownTree(tree: DisplayTree | undefined): DisplayTree | undefined {
  const content = tree?.content;
  if (tree === undefined || !Array.isArray(content) || content.length !== 1) return tree;
  const only = (content as DisplayTree[])[0];
  return only?.type === "decision" && Array.isArray(only.content)
    ? { ...tree, content: only.content }
    : tree;
}

/** The block of a decision's own content that names it: the first that holds any words. */
export function nameBlock(content: readonly DisplayTree[]): DisplayTree | undefined {
  return content.find((node) => wordsOf(node).trim() !== "");
}

/** A decision's name as the server's read of a decision names it (`decisionread`'s Heading): the
 *  words of the first block of its own content that holds any (`nameBlock`), so a heading is
 *  named by its words and never by the `#` that marks it one. Read from the tree the server read
 *  the text into; for an answer that carried no reading, from the first line of its own markdown
 *  that holds anything but a directive's fence, its heading marker taken off. */
export function decisionName(tree: DisplayTree | undefined, text: string): string {
  const content = ownTree(tree)?.content;
  if (tree !== undefined && Array.isArray(content)) {
    const block = nameBlock(content as DisplayTree[]);
    return block === undefined ? "" : wordsOf(block).trim();
  }
  const line = ownWords(text)
    .split("\n")
    .map((written) => written.trim())
    .find((written) => written !== "" && !written.startsWith(":::"));
  return line === undefined ? "" : line.replace(/^#+\s*/, "");
}

/** The word that opens a paragraph a decision's words give as its doubt, which a view draws
 *  with the unsure accent. */
export const UNSURE_PREFIX = "Unsure:";

/** Whether a block is the decision's doubt: a paragraph whose markdown starts `Unsure:` and
 *  nothing else. In the reading of such a paragraph the word stands in a first child that is
 *  plain text, since a mark over it - strong, a code span, a link - would have written its own
 *  characters first. */
export function isUnsureBlock(node: DisplayTree): boolean {
  if (node.type !== "paragraph" || !Array.isArray(node.content)) {
    return false;
  }
  const first = (node.content as DisplayTree[])[0];
  const marks = first?.marks;
  return (
    typeof first?.text === "string" &&
    (!Array.isArray(marks) || marks.length === 0) &&
    first.text.startsWith(UNSURE_PREFIX)
  );
}

/** Whether a decision's own words hold their `Unsure:` paragraph: a top-level block of their
 *  reading that is one, or, for an answer that carried no reading, a block of their markdown
 *  that starts with the word. */
export function holdsUnsureParagraph(tree: DisplayTree | undefined, text: string): boolean {
  const content = ownTree(tree)?.content;
  if (tree !== undefined && Array.isArray(content)) {
    return (content as DisplayTree[]).some(isUnsureBlock);
  }
  return ownWords(text)
    .split(/\n\s*\n/)
    .some((block) => block.trim().startsWith(UNSURE_PREFIX));
}

/** The words of the `Unsure:` paragraph a decision's node holds as values: converting a
 *  decisions document moves that paragraph into `uncertainty="high"` and the rest of it into
 *  `uncertainty_reason`. Undefined for a decision whose uncertainty is not high. A view draws
 *  them only where the decision's words hold no such paragraph (`holdsUnsureParagraph`), so a
 *  doubt is never drawn twice. */
export function unsureWords(
  decision: Pick<DecisionEntry, "uncertainty" | "uncertainty_reason">
): string | undefined {
  if (decision.uncertainty !== "high") return undefined;
  const reason = decision.uncertainty_reason?.trim() ?? "";
  return reason === "" ? UNSURE_PREFIX : `${UNSURE_PREFIX} ${reason}`;
}

/** A paragraph of plain words as a reading holds one: the words as one text run, so nothing in
 *  them is parsed, since a node's value is no markdown. */
export function plainParagraph(words: string): DisplayTree {
  return { content: [{ text: words, type: "text" }], type: "paragraph" };
}
