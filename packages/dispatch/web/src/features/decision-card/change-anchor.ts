import type { Artifact, Decision, DisplayTree, IssueDecisions } from "../../api/types";
import { parseNodeReference } from "../refs/routes";
import { wordsOf } from "../refs/tree-fragment";
import { storedReading } from "./stored-reading";

/**
 * Where a Change of a decision is anchored, and the document it is anchored in
 * (dispatch://INFRA-5702/spec#b-r-two-lists).
 *
 * A Change is a comment of the issue anchored to the decision's own words in the decisions
 * document, which is the write today's Decisions tab, its comments margin and the new issue
 * page's `DecisionActions.tsx` make; `changeAnchor` gives the quote and the occurrence it is
 * written with, counted over the decisions document's own reading, so it lands on the decision's
 * first block below its heading though an earlier decision, or the document's text above its
 * first decision, holds the same words.
 *
 * The decision card's callers read these functions, and the new issue page's
 * `issue-page/decision-review.ts` reads `decisionsDocumentId`, `decisionWords` and `flat` to find
 * the Changes a person asked. This module, as every file under `features/decision-card/`, imports
 * no file under `features/issue-page/` (R22).
 */

/**
 * The id of the document the decisions read names (`IssueDecisions.document`), among the
 * issue's documents: the read names it by its `dispatch://` address, and a Change's anchor names
 * the document it is anchored in by its id (`Anchor.artifact_id`). Undefined for an issue with
 * no decisions document, and while the issue read carries none of that address.
 */
export function decisionsDocumentId(
  artifacts: readonly Pick<Artifact, "id" | "slug">[] | null | undefined,
  decisions: Pick<IssueDecisions, "document"> | undefined
): string | undefined {
  const route = decisions?.document == null ? undefined : parseNodeReference(decisions.document);
  if (route?.kind !== "artifact") return undefined;
  return artifacts?.find((artifact) => artifact.slug === route.slug)?.id;
}

/**
 * The decision's first block below its heading, which is what a change is anchored to, as a
 * reading of that block alone: the server matches an anchor quote against the document's own
 * text (`pmdoc.FindQuote`), never its markdown, so a list or a hard break quoted as markdown
 * would anchor on nothing. It is read from the decision's reading (`readingOf`), whose top-level
 * blocks are the paragraphs the markdown would have been split into: the tree Go read the
 * decision's text into (`Decision.text_tree`), and where the read carried none that holds blocks
 * - a pod of the build before during a rolling deploy - the reading of its stored text
 * (`storedReading`). Undefined for a decision whose heading is all it holds. The Change draws
 * this block's plain-text projection (`DecisionActions.tsx`), and anchors to its words
 * (`anchorQuote`).
 */
export function anchorBlock(
  decision: Pick<Decision, "text" | "text_tree">
): DisplayTree | undefined {
  const reading = readingOf(decision);
  const blocks = reading.content;
  const block = Array.isArray(blocks)
    ? (blocks as DisplayTree[]).find((node) => node.type !== "heading")
    : undefined;
  return block === undefined ? undefined : { ...reading, content: [block] };
}

/** The words a change is anchored to: the words of the decision's first block below its heading
 *  (`anchorBlock`), as the document's own text holds them, so a decision's quote is the words of
 *  its text either way, never its markdown. Undefined for a decision whose heading is all it
 *  holds. */
export function anchorQuote(decision: Pick<Decision, "text" | "text_tree">): string | undefined {
  const block = anchorBlock(decision);
  return block === undefined ? undefined : wordsOf(block);
}

/**
 * The anchor a Change of a decision is written with: the quote (`anchorQuote`) and its zero-based
 * occurrence in the decisions document's text. The occurrence is absent where the document's
 * reading is not in hand, or does not line up with the decisions read (`changeAnchor`). A write
 * without it is anchored by the server where the quote stands once in the document, and refused
 * (409) where it stands more than once, rather than landed on the wrong place.
 */
export interface ChangeAnchor {
  readonly occurrence?: number;
  readonly quote: string;
}

/** The heading of the section that holds the replaced decisions (`replacedHeading`,
 *  `api/decisions.go`): its `###` sections are the replaced decisions, and it is none itself. */
const REPLACED_HEADING = "Replaced";

/**
 * The anchor a Change of `decision` is written with: the quote `anchorQuote` gives, and which of
 * that quote's places in the decisions document is the decision's own first block below its
 * heading. Two decisions can open with the same sentence, and the server refuses a quote that
 * matches more than one place unless the write names its zero-based occurrence (409, "matches 2
 * places; pass a zero-based occurrence"); so a Change names it. Undefined for a decision whose
 * heading is all it holds.
 *
 * The server counts the quote's places over the whole decisions document (`docs.MarkQuote`,
 * `pmdoc.FindQuote`), so this counts over it too. The text above the
 * first decision (the document's title, an intro), the `## Replaced` heading and a `#` section are
 * in no row of the decisions read, so a count over the rows missed a match there, and the Change
 * then landed outside every decision. The blocks read (`ArtifactBlock`) cannot serve either: it
 * holds each block's id, type and markdown range, and no text. So the caller passes `document`,
 * the decisions document's own reading: the `markdown_tree` of its text read
 * (`api.getArtifactText`), read once when the reader sends the Change. `decisions` are the
 * decisions read's rows, replaced ones included, in document order.
 *
 * The document's text is every text node's text joined, with one space wherever two text runs are
 * not adjacent, as between two blocks or two list items (`buildFlattenedText`): the words of the
 * reading (`wordsOf`). A quote's matches are every exact match in that text, overlapping ones
 * included, in document order (`findAll`), and the occurrence picks one of them: it is the number
 * of matches that start before the decision's first block. A match that starts in the heading and
 * runs into the block counts too, since the server lists it first. Where the block's words start
 * is read off the nodes above it, never found by searching for the quote.
 *
 * The decision's heading is found among the reading's top-level nodes by the server's section
 * rule (`decisionSections`, `isDecisionHeading`), walking them in order. A heading closes an open
 * section whose heading's level is its own or deeper; a `#` or `##` heading says whether what
 * follows is under `## Replaced`; and a heading opens a decision where none is open, at `##`
 * outside `## Replaced` and at `###` under it. The decision's heading is the one at the
 * decision's own index in `decisions` (by `block_id`). The reading lines up with the read only
 * where that heading's words are the decision's heading, and the node right after it is a block,
 * no heading, whose words are the quote. Otherwise, and where `document` is undefined, the anchor
 * carries no occurrence (`ChangeAnchor`). That is so for a heading the server reads with markup
 * or closing hashes, and for a sub-heading right below the decision's heading.
 *
 * One assumption remains: the reading's text runs are the stored document's, which the server
 * makes its display reading from.
 */
export function changeAnchor(
  decision: Pick<Decision, "block_id" | "heading" | "text" | "text_tree">,
  decisions: readonly Pick<Decision, "block_id">[] | null | undefined,
  document: DisplayTree | undefined
): ChangeAnchor | undefined {
  const quote = anchorQuote(decision);
  if (quote === undefined) return undefined;
  if (document === undefined || !Array.isArray(document.content)) return { quote };
  const nodes = document.content as DisplayTree[];
  const index = (decisions ?? []).findIndex((row) => row.block_id === decision.block_id);
  const heading = index === -1 ? undefined : decisionHeadings(nodes)[index];
  const block = heading === undefined ? undefined : nodes[heading + 1];
  if (
    heading === undefined ||
    wordsOf(nodes[heading]).trim() !== decision.heading.trim() ||
    block === undefined ||
    block.type === "heading" ||
    wordsOf(block) !== quote
  ) {
    return { quote };
  }
  // The block's words start one space after the words of the nodes above it, where those hold
  // any: `wordsOf` writes that space between two blocks, as the server does.
  const above = wordsOf({ ...document, content: nodes.slice(0, heading + 1) });
  const start = above === "" ? 0 : above.length + 1;
  return { occurrence: matchesBefore(wordsOf(document), quote, start), quote };
}

/** Where each decision's heading stands among the top-level nodes of the decisions document's
 *  reading, in document order, by the server's section rule (`changeAnchor`). */
function decisionHeadings(nodes: readonly DisplayTree[]): number[] {
  const headings: number[] = [];
  // The level of the open decision's heading, and 0 while no decision is open.
  let open = 0;
  let replaced = false;
  for (const [index, node] of nodes.entries()) {
    const level = headingLevel(node);
    if (level === 0) continue;
    if (level <= open) open = 0;
    if (level <= 2) replaced = level === 2 && wordsOf(node).trim() === REPLACED_HEADING;
    if (open === 0 && level === (replaced ? 3 : 2)) {
      open = level;
      headings.push(index);
    }
  }
  return headings;
}

/** A node's heading level (its `attrs.level`), and 0 for a node that is no heading, as the server
 *  reads a block that is none (`headingOf`). */
function headingLevel(node: DisplayTree): number {
  if (node.type !== "heading") return 0;
  const attrs = node.attrs;
  const level =
    typeof attrs === "object" && attrs !== null && "level" in attrs ? attrs.level : undefined;
  return typeof level === "number" ? level : 0;
}

/** A decision's whole words, as a Change's quote is matched against them: the words of its
 *  reading (`readingOf`), which `anchorQuote` reads its first block from too. */
export function decisionWords(row: Pick<Decision, "text" | "text_tree">): string {
  return wordsOf(readingOf(row));
}

/** The reading a decision is read by, the one rule this module reads a decision by: the server's
 *  reading of its text (`Decision.text_tree`) where the read carried one that holds blocks, and
 *  otherwise the reading of its stored text (`storedReading`), which the row holds, so it is
 *  remembered while the row lives. A reading with no `content` holds no blocks and is none, so a
 *  decision whose read sent one is read by its stored text. */
function readingOf(row: Pick<Decision, "text" | "text_tree">): DisplayTree {
  const tree = row.text_tree;
  return tree !== undefined && Array.isArray(tree.content) ? tree : storedReading(row.text, row);
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
export function flat(text: string): string {
  return text.replace(/\s+/gu, " ").trim();
}
