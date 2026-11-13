import type { Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useCallback, useRef } from "react";

import type { DisplayTree } from "../../api/types";
import { markdownClassName } from "./markdown-engine";
import { usePlainHeadline } from "./plain-text";
import { readTextReferences, type TextSource } from "./text-references";
import {
  type DecorateMarkdown,
  type PaintMarkdown,
  useRenderedMarkdown,
} from "./useRenderedMarkdown";
import { useTextReferences } from "./useTextReferences";

/** How many rendered lines a preview shows before the browser cuts it. Tailwind's `line-clamp-N`
 *  utilities are literal class names here so its scanner emits each rule. Each sets the
 *  `display` the clamp needs, so the root carries no `display` utility of its own: `.block` is
 *  emitted after them in the same layer and would win, and nothing would clamp. */
const CLAMP_CLASS: Record<1 | 2 | 3, string> = {
  1: "line-clamp-1",
  2: "line-clamp-2",
  3: "line-clamp-3",
};

/** A textblock's words where it holds nothing but text with no marks on it, which is what a
 *  preview can put in as plain words rather than as serialized inline content; `undefined` for
 *  one holding a mark, a hard break, a reference or a picture, which the serializer must write
 *  itself. */
function unmarkedText(node: ProseMirrorNode): string | undefined {
  let text = "";
  for (let child = 0; child < node.content.childCount; child += 1) {
    const inline = node.content.child(child);
    if (!inline.isText || inline.marks.length > 0) {
      return undefined;
    }
    text += inline.text ?? "";
  }
  return text;
}

/** Every run the server marked as matching a search, as the engine's serializer draws one
 *  (`markdown-serializer.ts`, `displaySchemaOf`). The server reads the snippet and marks the words
 *  it matched, so nothing on this page looks for them in the rendered text. */
const HIGHLIGHT_SELECTOR = "mark[data-search-highlight]";

/**
 * Every textblock's inline content, marks intact, on one line: a heading's words, then the
 * paragraph's bold and code and links, then each list item, space-separated. (Not
 * `MarkdownBody`'s inline flattening, whose later blocks are plain text: a preview whose first
 * block is a heading would lose the formatting of everything after it.) A code block's text goes
 * inside a `<code>`, so it reads as code and `linkifyDispatchRefs` leaves it alone.
 *
 * Blocks holding nothing but unmarked words go in as one text node, however many they are: a
 * preview is one run of text, and a body of thousands of plain blocks - the 8,000 items of a
 * list - would otherwise put 16,000 nodes into the page for the three lines a reader sees, which
 * is ~2 s of WebKit's thread to style and lay out (sjawhar/legion#2519). What the preview reads
 * is the same either way, character for character.
 */
const paintOneLine: PaintMarkdown = (element, parsed, serializer) => {
  element.replaceChildren();
  let first = true;
  /** The plain words waiting to go in, so a run of them lands as one node. */
  let plain = "";
  const putThePlainWordsIn = (): void => {
    if (plain !== "") {
      element.appendChild(document.createTextNode(plain));
      plain = "";
    }
  };
  parsed.descendants((node) => {
    if (!node.isTextblock) {
      return true;
    }
    if (node.content.size === 0) {
      return false;
    }
    if (!first) {
      plain += " ";
    }
    first = false;
    const isCode = node.type.spec.code === true;
    const words = isCode ? undefined : unmarkedText(node);
    if (words !== undefined) {
      plain += words;
      return false;
    }
    putThePlainWordsIn();
    const content = serializer.serializeFragment(node.content);
    if (isCode) {
      const code = document.createElement("code");
      code.appendChild(content);
      element.appendChild(code);
    } else {
      element.appendChild(content);
    }
    return false;
  });
  putThePlainWordsIn();
  // A hard break inside a paragraph would start a second visual line inside a one-line
  // preview; the preview is one run of text, so it reads as a space.
  for (const br of element.querySelectorAll("br")) {
    br.replaceWith(document.createTextNode(" "));
  }
  markWholeReferences(element);
  // A hit the server marked inside a link's own words marks the whole link instead: a
  // reference's words are replaced by its title a moment later (`collectReferenceAnchors`
  // empties the anchor), so a mark left inside would go with them. A bare reference the server
  // marked is inside one mark by now (`markWholeReferences`), and the link is made from the text
  // that mark holds (`linkifyDispatchRefs`).
  for (const link of element.querySelectorAll("a")) {
    if (link.querySelector(HIGHLIGHT_SELECTOR) === null) {
      continue;
    }
    for (const inner of link.querySelectorAll(HIGHLIGHT_SELECTOR)) {
      inner.replaceWith(...inner.childNodes);
    }
    const whole = document.createElement("mark");
    whole.setAttribute("data-search-highlight", "");
    link.replaceWith(whole);
    whole.appendChild(link);
  }
};

/** One node of a run of words a search mark can cut: a text node, or a search mark holding
 *  nothing but text, which is what the serializer draws the highlight as (it is the schema's last
 *  mark, so it is the innermost element over a run). */
interface RunPiece {
  readonly node: ChildNode;
  readonly text: string;
  readonly marked: boolean;
}

function runPiece(node: ChildNode): RunPiece | undefined {
  if (node instanceof Text) {
    return { marked: false, node, text: node.data };
  }
  if (
    node instanceof HTMLElement &&
    node.matches(HIGHLIGHT_SELECTOR) &&
    Array.from(node.childNodes).every((child) => child instanceof Text)
  ) {
    return { marked: true, node, text: node.textContent ?? "" };
  }
  return undefined;
}

/** Whether `node` is inside a link, a code context or a search mark of `root`, whose words no
 *  run of `markWholeReferences` reads: a link is already one, code holds no reference a renderer
 *  links, and a mark is a piece of its parent's run. */
function insideNoRun(node: HTMLElement, root: HTMLElement): boolean {
  for (let current: HTMLElement | null = node; current !== null && current !== root; ) {
    if (current.matches("a, code, pre") || current.matches(HIGHLIGHT_SELECTOR)) {
      return true;
    }
    current = current.parentElement;
  }
  return false;
}

/**
 * Puts each reference a search mark falls in under one mark of its own, whole. Postgres's
 * `ts_headline` marks the words it matched, and it cuts an address or a key into its own words to
 * do so: a search for `CORE-1` answers `dispatch://<b>CORE</b><b>-1</b>` and `<b>CORE</b><b>-1</b>`
 * (measured against the pod's Postgres, 2026-11-13, INFRA-1702), so the snippet's tree holds the
 * reference in three text nodes. The link pass reads one text node at a time
 * (`linkifyDispatchRefs`), so it would find no reference there, and the hit would show the raw
 * address where the reader should see the issue's name.
 *
 * So every run of adjacent text nodes and marks is read as the one text it is, and each
 * reference in it that holds a marked character is marked from its first character to its last.
 * The run is then written again as one node per stretch of marked or unmarked words, so each
 * marked reference stands in one text node inside one mark, which the link pass turns into a
 * link and the title replaces. A run no mark falls in, or whose marks touch no reference, is
 * left as it was.
 */
function markWholeReferences(element: HTMLElement): void {
  const containers = [element, ...element.querySelectorAll<HTMLElement>("*")].filter(
    (container) => !insideNoRun(container, element)
  );
  for (const container of containers) {
    let run: RunPiece[] = [];
    for (const child of Array.from(container.childNodes)) {
      const piece = runPiece(child);
      if (piece === undefined) {
        markReferencesOfRun(run);
        run = [];
      } else {
        run.push(piece);
      }
    }
    markReferencesOfRun(run);
  }
}

function markReferencesOfRun(run: readonly RunPiece[]): void {
  if (!run.some((piece) => piece.marked)) {
    return;
  }
  const text = run.map((piece) => piece.text).join("");
  const marked: boolean[] = [];
  for (const piece of run) {
    for (let index = 0; index < piece.text.length; index += 1) {
      marked.push(piece.marked);
    }
  }
  let widened = false;
  for (const reference of readTextReferences(text).references) {
    if (!marked.slice(reference.start, reference.end).includes(true)) {
      continue;
    }
    marked.fill(true, reference.start, reference.end);
    widened = true;
  }
  if (!widened) {
    return;
  }
  const first = run[0];
  if (first === undefined) {
    return;
  }
  const parent = first.node.parentNode;
  if (parent === null) {
    return;
  }
  for (let from = 0; from < text.length; ) {
    let to = from + 1;
    while (to < text.length && marked[to] === marked[from]) {
      to += 1;
    }
    const words = document.createTextNode(text.slice(from, to));
    if (marked[from] === true) {
      const mark = document.createElement("mark");
      mark.setAttribute("data-search-highlight", "");
      mark.appendChild(words);
      parent.insertBefore(mark, first.node);
    } else {
      parent.insertBefore(words, first.node);
    }
    from = to;
  }
  for (const piece of run) {
    piece.node.remove();
  }
}

/**
 * A line or two of Markdown someone wrote, formatted and then cut to fit: a search hit's
 * snippet, a hover card's body, a reply quote, a margin card's collapsed root. A stored text is
 * drawn from `tree`, the reading Go made of it, with the same engine and schema as
 * `MarkdownBody`, so bold, code, links and `dispatch://` references render as they do in the
 * full body, and block structure (a heading, a list, a second paragraph) collapses onto one run
 * of inline content as `MarkdownBody`'s `inline` variant does. The clamp is CSS on the rendered
 * element, after the drawing, so a cut never leaves half a mark: the reader sees `**held**` as
 * bold "held" cut by an ellipsis where it overflows, never a stray `**`. A caller that shows
 * only the start of a text cuts the tree, never the markdown (`tree-fragment.ts`), so no cut
 * can leave a dangling delimiter for anything to balance. A Dispatch picture shows as its
 * thumbnail beside its caption (`DispatchPicture`'s `inline`), as `MarkdownBody`'s `inline`
 * variant shows it.
 *
 * Whether a reference in it is a live link is its host's property, never a prop here: a preview
 * drawn inside a link or a button reads as links that are none (`useRenderedMarkdown`'s
 * `inertLinks`), since the host is the one thing a click follows, and a preview a reader reads
 * as a record keeps them real. A host says which it is once, for everything under it
 * (`ReferenceHostIs`).
 *
 * `lines` is the clamp (`1` is `truncate`-like but keeps the inline markup; `2` and `3` wrap
 * then clamp). `className` styles the element, whose `display` is the clamp's own, so a parent
 * that needs a flex item passes `min-w-0` there as it would for any block.
 * `highlightClassName` styles each run the server marked as matching a search, which rides in
 * the snippet's own tree (`HIGHLIGHT_SELECTOR`); a reference whose words hold one is marked
 * whole, since those words are replaced by its title. Until the engine has answered (one
 * pre-paint commit once it is loaded on the page) the element is empty; a text with no tree
 * that is too long for the page's own thread holds its own source, clamped, marked
 * `data-markdown-pending`, until the parse worker answers (`markdown-engine.ts`); and when the
 * engine cannot load, or the tree does not fit its schema, the literal source renders as text
 * with `data-markdown-fallback`, as `MarkdownBody` does.
 *
 * A bare issue key, and a `#` and the id of a question block of `source`'s document, read as
 * references too, and link where they name something (`useTextReferences`), as in a full body.
 */
export function MarkdownPreview({
  className = "",
  fullTitle = false,
  highlightClassName = "",
  lead,
  lines,
  markdown,
  source,
  tree,
}: {
  className?: string;
  /** Whether a pointer reader sees the whole text on hover, for a surface whose text is long
   *  enough to be cut: its plain words, as `TruncatedText`'s `title` offers, never its source. */
  fullTitle?: boolean;
  highlightClassName?: string;
  /** The app's own words before the text, plain and never parsed: a reply quote's
   *  `Replying to Planner — `. Clamped with the text as one line. */
  lead?: string;
  lines: 1 | 2 | 3;
  markdown: string;
  /** The record this text is - the ask, comment, message or document version whose text it is
   *  (`TextSource`); none for text of no record, which links no `#id`. */
  source?: TextSource;
  /** The reading Go made of this stored text, which the preview draws; absent only for a text
   *  Dispatch does not store (`MarkdownBody`). */
  tree?: DisplayTree;
}): ReactNode {
  const root = useRef<HTMLSpanElement>(null);
  const decorate = useCallback<DecorateMarkdown>(
    (element) => {
      if (highlightClassName !== "") {
        for (const mark of element.querySelectorAll<HTMLElement>(HIGHLIGHT_SELECTOR)) {
          mark.className = highlightClassName;
        }
      }
      if (lead !== undefined && lead !== "") {
        element.prepend(document.createTextNode(lead));
      }
    },
    [highlightClassName, lead]
  );
  const references = useTextReferences(markdown, source);
  const { isFallback, isPending, portals } = useRenderedMarkdown(
    root,
    markdown,
    tree,
    paintOneLine,
    { pictures: "inline", decorate, references }
  );
  // The whole text for a pointer reader is the one plain-text projection, where a reference
  // reads as its target's own name rather than as its short form (`PlainText`). A text with no
  // tree is one Dispatch does not store, and its own source is all that projection can read
  // (`useMarkdownHeadline`).
  const { reads, text: title } = usePlainHeadline(
    fullTitle ? tree : undefined,
    fullTitle && tree === undefined ? markdown : undefined
  );

  return (
    <span
      className={`${CLAMP_CLASS[lines]} ${markdownClassName} max-w-none ${className}`}
      data-markdown-fallback={isFallback || undefined}
      data-markdown-pending={isPending || undefined}
      data-markdown-preview=""
      ref={root}
      title={title?.text}
    >
      {reads}
      {portals}
    </span>
  );
}
