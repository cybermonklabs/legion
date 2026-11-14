/**
 * Raw HTML read as the text a body shows for it, on markdown import.
 *
 * A document is Markdown, and the document service refuses a block of raw HTML in one, so the
 * schema has no node for a line of raw HTML and the parse draws it as a paragraph of the same
 * characters instead (`remarkHtmlBlockParagraphs`, ./html-blocks.ts), which writes back as the
 * bytes its author wrote. A comment, message or reply body is not a document — its author
 * writes what they like, and a body holding `<details>` or the `<!-- legion: … -->` footer an
 * agent appends still has to read as the Markdown around it, not as its own source with every
 * `**` showing. This transform is that reading, the headless parser's `rawHtml: 'text'` policy
 * (`lib-headless.ts`):
 *
 * - an HTML comment draws as nothing, as GitHub draws it;
 * - any other raw HTML draws as the literal text its author typed, never as HTML.
 *
 * Both forms of raw HTML are covered. Remark leaves a line of it as an `html` node of its own
 * among the blocks, and a tag-shaped span inside a paragraph as an `html` node among that
 * paragraph's inline nodes. Milkdown's commonmark preset takes an `html` node as an inline atom
 * whose DOM is a span of its value as text, so an inline one already reads as its own
 * characters and only a block one needs a paragraph around it.
 *
 * A document passes no policy and is untouched by this file: an HTML comment, in a paragraph or
 * on a line of its own, stays the node the document writes back byte for byte.
 */

import type { MdastNode } from "./typed-block-drawing.js";

/** The opener of an HTML comment, the one raw HTML that draws as nothing. */
const OPENER = "<!--";

/** The two characters both of a comment's closers open with, which `afterTheCloser` scans for.
 *  `-->` is the closer CommonMark knows; `--!>` is one more that HTML's tokenizer takes, in its
 *  comment end bang state - the "incorrectly-closed-comment" parse error it recovers from -
 *  after which a browser shows what follows as text. A block of raw HTML reaches here as one
 *  `html` node, `<!--a--!>b` on a line of its own included, so it is read as the browser reads
 *  it, which is what a body is drawn for. */
const DASHES = "--";

/** Drops the last `count` characters of the text `kept` spells. */
function dropFromEnd(kept: string[], count: number): void {
  let left = count;
  while (left > 0 && kept.length > 0) {
    const piece = kept[kept.length - 1];
    if (piece.length > left) {
      kept[kept.length - 1] = piece.slice(0, piece.length - left);
      return;
    }
    kept.pop();
    left -= piece.length;
  }
}

/**
 * Where the comment whose opener ends at `at` ends in `value` - the index just past its closer
 * - or -1 where the value never closes it.
 *
 * A comment closes against its own opener. HTML's tokenizer ends one at the `>` of `<!-->` and
 * at the `->` of `<!--->` (its "abrupt closing of empty comment"), and CommonMark 0.31 says the
 * same: "An HTML comment consists of `<!-->`, `<!--->`, or `<!--`, a string of characters not
 * including the string `-->`, and `-->`" (the Raw HTML section; micromark reads both shapes in
 * `html-text.js`, whose `commentEnd` state ends at a `>` the moment the opener is read). The
 * closer searched for after the fourth character alone dropped the `shown` of `<!-->shown` and
 * of `<!--->shown`, which a browser and remark both show (dispatch://INFRA-341, R11).
 *
 * One scan, over the `--` either closer opens with, rather than one `indexOf` per closer: a
 * value that holds no `--!>` - which is almost every value - would otherwise be read to its end
 * once for each comment in it, which is the same n² the pieces below exist to avoid.
 */
function afterTheCloser(value: string, at: number): number {
  if (value[at] === ">") return at + 1;
  if (value[at] === "-" && value[at + 1] === ">") return at + 2;
  for (
    let dashes = value.indexOf(DASHES, at);
    dashes >= 0;
    dashes = value.indexOf(DASHES, dashes + 1)
  ) {
    if (value[dashes + 2] === ">") return dashes + 3;
    if (value[dashes + 2] === "!" && value[dashes + 3] === ">") return dashes + 4;
  }
  return -1;
}

/**
 * `value` with the comments in it gone.
 *
 * What the scan reads to decide is what it has kept, not what it has read, because dropping a
 * comment joins the text on either side of it and the join can spell an opener that stood in
 * neither half: drop the `<!---->` of `<!<!---->--` and `<!` meets `--`. A pass over the value -
 * one `replace`, or any number of them, since what is left holds no `-->` for a second pass to
 * match - writes that opener out as text, where remark and a browser read a comment starting.
 * For the same reason an opener the value never closes takes the rest of the value rather than
 * drawing as the characters its author typed, which is how both read it too.
 *
 * What is kept is held as pieces of `value`, and only a piece that holds something is kept. A
 * string built up a character at a time is copied whole every time its end is examined, which
 * costs a value of n characters n², and a body may hold a long one; the seam a dropped comment
 * leaves is three characters wide, so those three are read back out of the pieces instead. An
 * empty piece would be one of them: between two comments written against each other there is
 * nothing to keep, and a piece kept for it would stand between the seam and the characters
 * before it, so the walk back over three characters would be a walk over every comment dropped
 * so far. `"<!---->".repeat(m)` in one `html` node, which any commenter can write, cost 35,000
 * characters 76 ms, 140,000 characters 2,132 ms and 280,000 characters 7,712 ms; the same
 * three cost 4 ms, 7 ms and 6 ms with the empty piece left out and the one closer scan below
 * (dispatch://INFRA-341, R11). The scan is linear in the value's length.
 */
function withoutComments(value: string): string {
  if (!value.includes(OPENER)) return value;
  const kept: string[] = [];
  /** The piece of `value` not yet kept: it begins at `from`, and the reading stands at `at`. */
  let from = 0;
  let at = 0;
  /** The character `back` places before the end of what is kept, the piece being read included,
   *  or "" before the start of it. */
  const before = (back: number): string => {
    let left = back;
    if (left < at - from) return value[at - 1 - left];
    left -= at - from;
    for (let piece = kept.length - 1; piece >= 0; piece -= 1) {
      const text = kept[piece];
      if (left < text.length) return text[text.length - 1 - left];
      left -= text.length;
    }
    return "";
  };
  /** How many of an opener's characters are kept already where the reading stands: 1, 2 or 3 at
   *  a seam, else 0. A whole one is impossible, each being dropped as it appears. */
  const straddling = (): number => {
    for (let taken = OPENER.length - 1; taken > 0; taken -= 1) {
      let spelled = true;
      for (let index = 0; index < OPENER.length && spelled; index += 1) {
        spelled =
          index < taken
            ? before(taken - 1 - index) === OPENER[index]
            : value[at + index - taken] === OPENER[index];
      }
      if (spelled) return taken;
    }
    return 0;
  };
  for (;;) {
    const taken = straddling();
    if (taken === 0) {
      const opener = value.indexOf(OPENER, at);
      if (opener < 0) break;
      at = opener;
    }
    if (at > from) kept.push(value.slice(from, at));
    dropFromEnd(kept, taken);
    at += OPENER.length - taken;
    const closed = afterTheCloser(value, at);
    if (closed < 0) return kept.join("");
    at = closed;
    from = at;
  }
  if (value.length > from) kept.push(value.slice(from));
  return kept.join("");
}

/**
 * Rewrites `node`'s children, and their own, in place: every `html` child loses the comments in
 * it, and is dropped where nothing is left of it.
 *
 * What stands where a block of raw HTML stood is `remarkHtmlBlockParagraphs`'s business
 * (./html-blocks.ts), which runs right after this transform in the same pipeline
 * (`lib-headless.ts`): it wraps every block `html` node - one under any parent whose children
 * are not phrasing content, not a table of five parent types - in the paragraph the schema can
 * hold, and keeps the node's `position`, which the later transforms read the source offsets off
 * (./typed-block-drawing.ts). All this transform owes it is a node worth wrapping, so a node
 * whose whole value was comments goes before the wrap can leave an empty paragraph standing
 * where the comment was.
 *
 * A node whose children this leaves alone keeps the array it had. This runs over every node of
 * every body on the page and in the parse worker, and the overwhelming majority of bodies hold
 * no raw HTML at all, so the copy is made only once a child has actually changed - the 16,000
 * nodes of an 8,000-item list cost no array each (dispatch://INFRA-341, R11).
 */
function rewrite(node: MdastNode): void {
  const children = node.children;
  if (children === undefined) return;
  /** The rewritten children, once one of them differs from what stood there; until then the
   *  node's own array is still right. */
  let kept: MdastNode[] | undefined;
  for (const [index, child] of children.entries()) {
    if (child.type !== "html") {
      rewrite(child);
      kept?.push(child);
      continue;
    }
    kept ??= children.slice(0, index);
    // What the raw HTML shows: itself without the comments in it, which draw as nothing. Raw
    // HTML that was nothing but comments shows nothing, so its node goes.
    const shown = withoutComments(child.value ?? "").trim();
    if (shown === "") continue;
    child.value = shown;
    kept.push(child);
  }
  if (kept !== undefined) {
    node.children = kept;
  }
}

/** remark transformer — the headless parser's `rawHtml: 'text'` policy, which only a body
 *  renderer asks for. */
export function remarkRawHtmlAsText() {
  return (tree: MdastNode) => {
    rewrite(tree);
  };
}
