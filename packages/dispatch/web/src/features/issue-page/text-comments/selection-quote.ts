import { referenceSpans } from "../../refs/reference-spans";
import { wordsOf } from "../../refs/tree-fragment";
import { addressesAsShortForms } from "../tree-drafts";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import { asRead, rowTextOf, type TextRuns, textRuns } from "./row-text-runs";

/**
 * The words a selection in a row quotes, and where they are (R5): the quote as the server's plain
 * text reads it, which match of it the selection is (`occurrence`, as `quoteAnchor` counts), and,
 * for an ask, a comment or a message, which of the item's blocks holds it.
 *
 * The selection is read in the words the row draws (`row-text-runs.ts`), and the quote is taken
 * from the node's own text as the tree read answers it, at the place the selection names. Where
 * the row draws that text as it is - every node with a reading - the two are one string. Where it
 * draws it read to plain words (`plainWords`: a lens's verdict, a risk's heading, a constraint's
 * words), it writes white space as one space and a bare address as its short form, so the place
 * is found by reading the two side by side, and a selection over part of a short form quotes the
 * whole address. Where the drawn words run on past the node's text, or differ from it, the place
 * past that point is in no text the server holds, and the selection takes no quote there: the
 * page offers the control only on words whose plain text it reads as the server does (decision
 * 6; the fixtures of `quotes.json` hold it for each inline kind a row draws).
 */

/** A block of an item's text: a block node the tree read hangs under an ask, a comment or a
 *  message (`itemBlocksOf`), or the block a reply names (`reply_block`). */
export type ItemBlock = Pick<TreeAnswerNode, "block_id" | "text" | "text_tree">;

/** The node a selection is in, as the tree read answers it: its own text, and its reading where
 *  the read sent one; for an ask, a comment or a message, its blocks in the item's order
 *  (`itemBlocksOf`), and none for any other node. */
export interface QuotedNode<Block extends ItemBlock = ItemBlock> {
  readonly blocks?: readonly Block[];
  readonly text: string;
  readonly text_tree?: TreeAnswerNode["text_tree"];
}

/** What a selection quotes: the words, which match of them it is in the text they are counted in
 *  (the block's for a document's block or an item's, the node's for a node anchor), and the
 *  item's block that holds them, null for a node that is no item. */
export interface TextQuote<Block extends ItemBlock = ItemBlock> {
  readonly block: Block | null;
  readonly occurrence: number;
  readonly quote: string;
}

/** The ATX marker a quote may open with to mean "the heading whose text follows" (`FindQuote`),
 *  which `quoteAnchor` leaves out of a quote. */
const HEADING_MARKER = /^#{1,6} +/u;

/** One character of white space, as `plainWords` reads it. */
const SPACE = /\s/u;

/** The kinds of node an item's blocks stand under. */
const ITEM_KINDS: Readonly<Record<string, true>> = { ask: true, comment: true, message: true };

/**
 * The block nodes the tree read hangs under the ask, comment or message `item` names, in the
 * answer's order, which is the item's: each is the item's kind's block (`comment_block`,
 * `message_block`, `ask_block`) with the item as its parent. A node the walk holds twice is read
 * once.
 */
export function itemBlocksOf(answer: TreeAnswer, item: string): TreeAnswerNode[] {
  const seen = new Set<string>();
  return (answer.nodes ?? []).filter((node) => {
    const parent = node.parent;
    if (parent === null || parent.id !== item || ITEM_KINDS[parent.supertag] !== true) {
      return false;
    }
    if (node.supertag !== `${parent.supertag}_block` || seen.has(node.id)) return false;
    seen.add(node.id);
    return true;
  });
}

/**
 * The quote the selection makes in `row`, whose node is `node`: null where the selection is not
 * one range inside the row's own words (`rowTextOf`), holds no character of the node's text,
 * touches what the row draws that is not that text (a chip, a reference's drawn name, a typed
 * block's head, a deleted line), runs past the words the node's text holds, or, in an item, does
 * not lie inside one of its blocks.
 */
export function quoteOf<Block extends ItemBlock = ItemBlock>(
  selection: Selection,
  row: Element,
  node: QuotedNode<Block>
): TextQuote<Block> | null {
  const words = rowTextOf(row);
  if (words === null || selection.rangeCount !== 1) return null;
  const range = selection.getRangeAt(0);
  if (range.collapsed) return null;
  const runs = textRuns(words);
  const start = runs.offsetOf(range.startContainer, range.startOffset);
  const end = runs.offsetOf(range.endContainer, range.endOffset);
  if (start === null || end === null) return null;
  const drawn = drawnSelection(runs, start, end);
  if (drawn === null) return null;
  const source = sourceOf(node);
  const alignment = aligned(runs.text, source);
  if (drawn.end > alignment.length) return null;
  let from = alignment.from[drawn.start] ?? 0;
  const to = alignment.to[drawn.end - 1] ?? 0;
  for (
    let marker = HEADING_MARKER.exec(source.slice(from, to));
    marker !== null && marker[0].length < to - from;
    marker = HEADING_MARKER.exec(source.slice(from, to))
  ) {
    from += marker[0].length;
  }
  const quote = source.slice(from, to);
  if (quote === "") return null;
  if (node.blocks === undefined) {
    return { block: null, occurrence: matchesBefore(source, quote, from), quote };
  }
  const holder = blockSpans(source, node.blocks)?.find(
    (span) => span.start <= from && from < span.end
  );
  if (holder === undefined || to > holder.end) return null;
  return {
    block: holder.block,
    occurrence: matchesBefore(source.slice(holder.start, holder.end), quote, from - holder.start),
    quote,
  };
}

/**
 * The DOM range of match `occurrence` of a stored `quote` in the words `row` draws, counted as
 * `quoteOf` counts it: in the node's text, or in `block`, the item's block that holds the quote.
 * `node` is the row's node, which a row that draws its text read to plain words needs (`quoteOf`
 * reads the place of a selection in it), and the item's blocks where `block` is one of them; with
 * none the quote is matched in the words as the row draws them. Null where the row draws no such
 * match: its words are gone, or stand where the row draws none of them.
 */
export function rangeOf(
  row: Element,
  quote: string,
  occurrence: number,
  { block, node }: { readonly block?: ItemBlock; readonly node?: QuotedNode } = {}
): Range | null {
  const words = rowTextOf(row);
  if (words === null || quote === "") return null;
  const runs = textRuns(words);
  const source = node === undefined ? runs.text : sourceOf(node);
  let within = { end: source.length, start: 0 };
  if (block !== undefined) {
    const blocks =
      node?.blocks?.some((one) => sameBlock(one, block)) === true ? node.blocks : [block];
    const span = blockSpans(source, blocks)?.find((one) => sameBlock(one.block, block));
    if (span === undefined) return null;
    within = span;
  }
  const from = matchAt(source, quote, occurrence, within.start, within.end);
  if (from === undefined) return null;
  const alignment = aligned(runs.text, source);
  let start: number | undefined;
  let end: number | undefined;
  for (let index = 0; index < alignment.length; index += 1) {
    if (start === undefined && (alignment.to[index] ?? 0) > from) start = index;
    if ((alignment.from[index] ?? 0) < from + quote.length) end = index + 1;
  }
  // The words the row draws must hold the whole match, not only its start.
  if (
    start === undefined ||
    end === undefined ||
    end <= start ||
    (alignment.to[end - 1] ?? 0) < from + quote.length
  ) {
    return null;
  }
  const range = words.ownerDocument.createRange();
  return startAt(range, runs, start) && endAt(range, runs, end) ? range : null;
}

/** The node's text as the server's plain text reads it: its reading's text runs joined as
 *  `FindInBlock` joins them (`wordsOf`), else its stored text. */
function sourceOf(node: Pick<QuotedNode, "text" | "text_tree">): string {
  return asRead(node.text_tree === undefined ? node.text : wordsOf(node.text_tree));
}

/** Whether `left` and `right` are one block of an item: the same node, or the same block id. */
function sameBlock(left: ItemBlock, right: ItemBlock): boolean {
  return left === right || (left.block_id !== undefined && left.block_id === right.block_id);
}

/**
 * The span `[start, end)` of the runs' text a selection from `start` to `end` takes: widened over
 * each address the row draws short (a selection that takes part of it takes all of it), without
 * the separator a block's edge or an atom makes at either end, as `quoteAnchor` leaves it out.
 * Null where that holds no character, or touches what the row draws that is not the node's text.
 */
function drawnSelection(
  runs: TextRuns,
  start: number,
  end: number
): { readonly end: number; readonly start: number } | null {
  let from = start;
  let to = end;
  for (const run of runs.runs) {
    if (run.whole !== true) continue;
    if (from > run.start && from < run.end) from = run.start;
    if (to > run.start && to < run.end) to = run.end;
  }
  const drawnAt = (offset: number): boolean =>
    runs.runs.some((run) => run.start <= offset && offset < run.end) ||
    runs.foreign.some((span) => span.start <= offset && offset < span.end);
  while (from < to && !drawnAt(from)) from += 1;
  while (to > from && !drawnAt(to - 1)) to -= 1;
  if (from >= to) return null;
  const touches = runs.foreign.some((span) =>
    span.start === span.end
      ? from < span.start && span.start < to
      : from < span.end && span.start < to
  );
  return touches ? null : { end: to, start: from };
}

/** How the drawn words stand for the node's text: for each of the first `length` characters of
 *  the drawn words, the span of the text it stands for. A character the row writes as it stands
 *  stands for itself; a run of white space the row writes otherwise, or a short form, stands for
 *  the whole run or the whole address. */
interface Alignment {
  readonly from: Int32Array;
  readonly length: number;
  readonly to: Int32Array;
}

/**
 * The drawn words `drawn` read beside the node's text `source`, from their starts, as far as they
 * agree: a character the same in both, a run of white space in both (`plainWords` writes one
 * space for each run, and leaves out a run that opens the text), and a bare address in `source`
 * that `drawn` writes as its short form (`addressesAsShortForms`). Past the first place they part,
 * no drawn character stands for any of the text.
 */
function aligned(drawn: string, source: string): Alignment {
  const from = new Int32Array(drawn.length);
  const to = new Int32Array(drawn.length);
  if (drawn === source) {
    for (let index = 0; index < drawn.length; index += 1) {
      from[index] = index;
      to[index] = index + 1;
    }
    return { from, length: drawn.length, to };
  }
  const addresses = new Map<number, { readonly end: number; readonly short: string }>();
  for (const { start, value } of referenceSpans(source, "dispatch://")) {
    const short = addressesAsShortForms(value);
    if (short !== value && short !== "") addresses.set(start, { end: start + value.length, short });
  }
  const stand = (
    drawnFrom: number,
    drawnTo: number,
    sourceFrom: number,
    sourceTo: number
  ): void => {
    for (let index = drawnFrom; index < drawnTo; index += 1) {
      from[index] = sourceFrom;
      to[index] = sourceTo;
    }
  };
  let at = 0;
  let read = 0;
  while (at < drawn.length && read < source.length) {
    const address = addresses.get(read);
    if (
      address !== undefined &&
      !drawn.startsWith(source.slice(read, address.end), at) &&
      drawn.startsWith(address.short, at)
    ) {
      stand(at, at + address.short.length, read, address.end);
      at += address.short.length;
      read = address.end;
      continue;
    }
    const drawnSpace = spaceEnd(drawn, at);
    const sourceSpace = spaceEnd(source, read);
    if (drawnSpace > at && sourceSpace > read) {
      if (drawnSpace - at === sourceSpace - read) {
        for (let index = 0; index < drawnSpace - at; index += 1) {
          stand(at + index, at + index + 1, read + index, read + index + 1);
        }
      } else {
        stand(at, drawnSpace, read, sourceSpace);
      }
      at = drawnSpace;
      read = sourceSpace;
      continue;
    }
    if (sourceSpace > read && at === 0) {
      read = sourceSpace;
      continue;
    }
    if (drawnSpace > at || sourceSpace > read || drawn[at] !== source[read]) break;
    stand(at, at + 1, read, read + 1);
    at += 1;
    read += 1;
  }
  return { from, length: at, to };
}

/** The end of the run of white space that starts at `at` in `text`: `at` itself where none does. */
function spaceEnd(text: string, at: number): number {
  let end = at;
  while (end < text.length && SPACE.test(text[end] ?? "")) end += 1;
  return end;
}

/** Where each of `blocks` stands in `source`, the item's text: each block's own text found in
 *  order after the one before it. Null where a block's text is not there, so the page and the
 *  server would not read the same block. */
function blockSpans<Block extends ItemBlock>(
  source: string,
  blocks: readonly Block[]
): { readonly block: Block; readonly end: number; readonly start: number }[] | null {
  const spans: { readonly block: Block; readonly end: number; readonly start: number }[] = [];
  let at = 0;
  for (const block of blocks) {
    const words = sourceOf(block);
    const start = source.indexOf(words, at);
    if (start < 0) return null;
    spans.push({ block, end: start + words.length, start });
    at = start + words.length;
  }
  return spans;
}

/** How many matches of `quote` start in `text` before `before`, overlapping ones too, as
 *  `quoteAnchor` counts them and the server's search steps one unit at a time. */
function matchesBefore(text: string, quote: string, before: number): number {
  let count = 0;
  for (
    let index = text.indexOf(quote);
    index >= 0 && index < before;
    index = text.indexOf(quote, index + 1)
  ) {
    count += 1;
  }
  return count;
}

/** Where match `occurrence` of `quote` starts in `text` between `start` and `end`, counting from
 *  `start` and overlapping matches included; undefined where there is no such match. */
function matchAt(
  text: string,
  quote: string,
  occurrence: number,
  start: number,
  end: number
): number | undefined {
  let seen = 0;
  for (
    let index = text.indexOf(quote, start);
    index >= 0 && index + quote.length <= end;
    index = text.indexOf(quote, index + 1)
  ) {
    if (seen === occurrence) return index;
    seen += 1;
  }
  return undefined;
}

/** Starts `range` at the character `offset` of the runs' text: in the run that holds it, before
 *  the link a reference's words stand in, else where `pointOf` puts it. */
function startAt(range: Range, runs: TextRuns, offset: number): boolean {
  const span = runs.foreign.find((one) => one.start <= offset && offset < one.end);
  if (span !== undefined && !runs.runs.some((run) => run.start <= offset && offset < run.end)) {
    range.setStartBefore(span.node);
    return true;
  }
  const point = runs.pointOf(offset);
  if (point === null) return false;
  range.setStart(point.node, point.offset);
  return true;
}

/** Ends `range` after the character before `offset` of the runs' text: at the end of the run that
 *  holds it, after the link a reference's words stand in, else where `pointOf` puts it. */
function endAt(range: Range, runs: TextRuns, offset: number): boolean {
  const run = runs.runs.find((one) => one.start < offset && offset <= one.end);
  if (run !== undefined) {
    range.setEnd(run.node, run.whole === true ? run.node.length : offset - run.start);
    return true;
  }
  const span = runs.foreign.find((one) => one.start < offset && offset <= one.end);
  if (span !== undefined) {
    range.setEndAfter(span.node);
    return true;
  }
  const point = runs.pointOf(offset);
  if (point === null) return false;
  range.setEnd(point.node, point.offset);
  return true;
}
