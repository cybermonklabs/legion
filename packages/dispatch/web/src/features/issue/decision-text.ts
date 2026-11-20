import type { DisplayTree } from "../../api/types";

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
      // A tree's words joined to match a quote against, drawn nowhere.
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

/** A decision's name as the server's read of a decision names it (`decisionread`'s Heading): the
 *  words of the first block of its own content that holds any, so a heading is named by its
 *  words and never by the `#` that marks it one. Read from the tree the server read the text
 *  into; for an answer that carried no reading, from the first line of its own markdown that
 *  holds anything but a directive's fence, its heading marker taken off. */
export function decisionName(tree: DisplayTree | undefined, text: string): string {
  const content = ownTree(tree)?.content;
  if (tree !== undefined && Array.isArray(content)) {
    for (const node of content as DisplayTree[]) {
      const words = wordsOf(node).trim();
      if (words !== "") return words;
    }
    return "";
  }
  const line = ownWords(text)
    .split("\n")
    .map((written) => written.trim())
    .find((written) => written !== "" && !written.startsWith(":::"));
  return line === undefined ? "" : line.replace(/^#+\s*/, "");
}
