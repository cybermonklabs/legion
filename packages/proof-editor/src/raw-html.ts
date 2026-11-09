/**
 * Raw HTML read as the text a body shows for it, on markdown import.
 *
 * A document is Markdown, and the document service refuses a block of raw HTML in one, so the
 * schema has nothing to put a line of raw HTML in and the parse throws for it. A comment,
 * message or reply body is not a document — its author writes what they like, and a body holding
 * `<details>` or the `<!-- legion: … -->` footer an agent appends still has to read as the
 * Markdown around it, not as its own source with every `**` showing. This transform is that
 * reading, the headless parser's `rawHtml: 'text'` policy (`lib-headless.ts`):
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
 * A document passes no policy and is untouched by this file: the parse still throws for a line
 * of raw HTML, and an HTML comment inside a paragraph stays the node the document writes back.
 */

import type { MdastNode } from "./typed-block-drawing.js";

/** The two ends of an HTML comment, the one raw HTML that draws as nothing. */
const OPENER = "<!--";
const CLOSER = "-->";

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
 * What is kept is held as pieces of `value`. A string built up a character at a time is copied
 * whole every time its end is examined, which costs a value of n characters n², and a body may
 * hold a long one; the seam a dropped comment leaves is three characters wide, so those three
 * are read back out of the pieces instead.
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
    kept.push(value.slice(from, at));
    dropFromEnd(kept, taken);
    at += OPENER.length - taken;
    const closer = value.indexOf(CLOSER, at);
    if (closer < 0) return kept.join("");
    at = closer + CLOSER.length;
    from = at;
  }
  kept.push(value.slice(from));
  return kept.join("");
}

/**
 * The mdast parents whose children are blocks, where an `html` node stands for a line of raw
 * HTML rather than a span inside one. Such a node is wrapped in a paragraph, since the inline
 * atom the schema has for it fits in no block parent — what Milkdown's own
 * `remarkHtmlTransformer` does for the live editor, which the headless parser does not install.
 * A block parent this table does not name keeps its raw HTML unwrapped and the parse throws for
 * it, as it does with the policy off, so the body falls back to its literal text rather than
 * losing it.
 */
const AMONG_BLOCKS: ReadonlySet<string> = new Set([
  "blockquote",
  "containerDirective",
  "footnoteDefinition",
  "listItem",
  "root",
]);

/**
 * Rewrites `node`'s children, and their own, in place: every `html` child loses its comments and
 * is dropped where nothing is left of it, and one among blocks gains the paragraph that can hold
 * it.
 *
 * A node whose children this leaves alone keeps the array it had. This runs over every node of
 * every body on the page and in the parse worker, and the overwhelming majority of bodies hold
 * no raw HTML at all, so the copy is made only once a child has actually changed - the 16,000
 * nodes of an 8,000-item list cost no array each (dispatch://INFRA-341, R11).
 */
function rewrite(node: MdastNode): void {
  const children = node.children;
  if (children === undefined) return;
  const amongBlocks = AMONG_BLOCKS.has(node.type);
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
    kept.push(amongBlocks ? { type: "paragraph", children: [child] } : child);
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
