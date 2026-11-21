import type { Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useRef } from "react";

import type { DisplayTree } from "../../api/types";
import { markdownClassName, type SoftBreaks } from "./markdown-engine";
import { paintBlock, paintBlockInSlices } from "./paint-block";
import type { TextSource } from "./text-references";
import { type PaintMarkdown, useRenderedMarkdown } from "./useRenderedMarkdown";
import { useTextReferences } from "./useTextReferences";

/** The first textblock's own inline content (marks intact), plus the flattened plain text of
 * every textblock after it, joined with a single space - see `flattenInline`. */
interface InlineContent {
  head: ProseMirrorNode;
  extra: string;
}

/** A textblock's own text, skipping any code span or code block content — used only for the
 * "extra" (non-first) textblocks `flattenInline` yields, which flatten to plain text and so
 * would otherwise re-expose a code span's `dispatch://` ref as linkifiable bare text once its
 * `code` mark is gone. The first textblock keeps its marks (serialized, not flattened), so its
 * own code spans stay real `<code>` elements and need no such filtering. */
function plainTextExcludingCode(node: ProseMirrorNode): string {
  const parts: string[] = [];
  node.descendants((child) => {
    if (child.type.name === "code_block") {
      return false;
    }
    if (child.isText) {
      const hasCode = child.marks.some((mark) => mark.type.name === "inlineCode");
      if (!hasCode) {
        parts.push(child.text ?? "");
      }
    }
    return true;
  });
  return parts.join("");
}

/** A parsed document collapsed onto one line: the first textblock's inline content verbatim
 * (marks intact) and every later textblock's plain text after it, space-joined, so a list or a
 * multi-paragraph body renders as one run of inline content instead of nesting a `<ul>` or a
 * second `<p>` inside a span. `undefined` for a document with no text at all. */
function flattenInline(root: ProseMirrorNode): InlineContent | undefined {
  let head: ProseMirrorNode | undefined;
  const extra: string[] = [];
  root.descendants((node) => {
    if (!node.isTextblock) {
      return true;
    }
    if (head === undefined) {
      head = node;
    } else {
      const text = plainTextExcludingCode(node).trim();
      if (text !== "") {
        extra.push(text);
      }
    }
    return false;
  });
  return head === undefined ? undefined : { extra: extra.join(" "), head };
}

const paintInline: PaintMarkdown = (element, parsed, serializer) => {
  const flattened = flattenInline(parsed);
  element.replaceChildren();
  if (flattened !== undefined) {
    element.appendChild(serializer.serializeFragment(flattened.head.content));
    if (flattened.extra !== "") {
      element.appendChild(document.createTextNode(` ${flattened.extra}`));
    }
  }
};

/**
 * Renders Markdown text through Proof's own parser and schema, so every question, answer,
 * comment, reply, and message formats identically to the document editor. `block` (the
 * default) renders the full parsed document, including lists and headings, inside a `<div>`;
 * there a task item (`- [x] done`) draws a disabled checkbox, ticked as its source is.
 * `inline` renders only inline marks (bold, code, links) inside a `<span>` with no wrapping
 * `<p>` or block spacing, for single-line contexts like an option label or a resolution reason
 * inside a metadata line:
 * it takes the first textblock's inline content verbatim (marks intact) and appends every
 * later textblock's plain text after it, space-joined, so a list or a multi-paragraph body
 * collapses onto one line instead of nesting a `<ul>`/second `<p>` inside the span. Both
 * variants inherit the surrounding text's size and color (`.dispatch-markdown` in styles.css
 * overrides Tailwind Typography's fixed palette, heading scale, and font size) so a heading
 * inside, say, an ask question reads as bold text at the card's own size rather than a
 * page-size h1, and a resolution reason inline in a `text-xs` line stays that size.
 *
 * A picture whose address is a Dispatch artifact (`![shot.png](dispatch://…@v1)`, what a pasted
 * picture is written as, or the same without `@vN` for its latest version) shows through
 * `DispatchPicture`: at its own size scaled down to the column, opening at full size on a click,
 * in `block`; as a thumbnail beside its caption in `inline`. A picture on another website renders
 * as Proof renders it.
 *
 * Once the schema and Proof's headless engine are loaded (the first body on the page loads
 * them), a body of at most `markdownWorkerThreshold` characters renders synchronously in the
 * layout phase of the commit that mounts it (`useRenderedMarkdown`); only the very first render
 * on a page, or a schema that failed to load, takes the asynchronous path. A longer body is
 * parsed off the page's own thread, so it holds its own text, marked `data-markdown-pending`,
 * until the parse worker answers (`markdown-engine.ts`). A stored text waits for neither: its
 * `tree` is drawn in the commit that mounts it, however long it is. Either way, the `block`
 * variant fills the element a piece at a time (`paintBlockInSlices`) wherever the document holds
 * more nodes than one task puts into the page (`paintsAcrossTasks`), so the 16,000 elements of
 * an 8,000-item list never reach it in one task. A body is not a document, so raw HTML in one
 * is read as the text it shows (`rawHtml: "text"`): an HTML comment - the `<!-- legion: … -->`
 * footer an agent appends, say - draws as nothing, as GitHub draws it, and any other raw HTML
 * draws as its own literal characters, never as HTML, with the Markdown around it formatted as
 * ever. A preview that must clamp its rendered line is `MarkdownPreview`, which draws the same
 * trees with the same engine and the same policy.
 *
 * A bare issue key, and a `#` and the id of a question block of `source`'s document, read as
 * references too, and link where they name something (`useTextReferences`). A caller names the
 * record this text is in `source`, and the server says which document its `#id`s resolve
 * against; text that is no record's passes nothing and gets no `#id` links, which is right.
 * The text those references are read from is `markdown`, unless the caller names another in
 * `referenceText`.
 */
export function MarkdownBody({
  markdown,
  onRendered,
  referenceText = markdown,
  softBreaks = "space",
  source,
  tree,
  variant = "block",
}: {
  /** The text this body holds: parsed and drawn where it has no `tree`, and written into the
   *  element as its literal characters where neither the tree nor the markdown could be drawn -
   *  no engine, a tree the schema refuses, a parse the parser refuses (`useRenderedMarkdown`).
   *  So it is this element's own text and no more of its field than that: a caller drawing one
   *  block of a longer text passes that block's text here, or the fallback writes the whole
   *  field once for every block of it. */
  markdown: string;
  onRendered?: () => void;
  /** The text whose references this body links, where that is not the text it draws: the whole
   *  field a caller is drawing one block of. The references belong to the field - a link's
   *  target and the backticks that end a key are in its markdown, and a block of it holds
   *  neither - while what this element draws is the block alone, so the two are different texts
   *  and each is read for its own thing. Defaults to `markdown`, which is every caller drawing
   *  a whole field. */
  referenceText?: string;
  /** What a single newline inside a paragraph becomes: `"space"` for a document (hard-wrapped
   *  Markdown is one paragraph), `"line"` for text whose author meant its lines as lines, such
   *  as a model's streamed turn (`SoftBreaks`). Read only for a text with no `tree`: a stored
   *  text was read by the server under the document's own policy. */
  softBreaks?: SoftBreaks;
  /** The record this text is - the ask, comment, message or document version whose text it is
   *  (`TextSource`); none for text of no record, which links no `#id`. */
  source?: TextSource;
  /** The reading Go made of this stored text, which the body draws rather than parsing the
   *  markdown again. Absent for the three texts Dispatch does not store - a live agent turn, a
   *  GitHub body in a link preview, and a write the server has not stored yet - and for a stored
   *  text whose answer came from a pod of the build before this one during a rolling deploy,
   *  which is why `reference-target.ts` and `markdown-engine.ts` still reach the browser parser
   *  for a stored text: that fallback is the two-builds case, not a defect. */
  tree?: DisplayTree;
  variant?: "block" | "inline";
}): ReactNode {
  // One ref per tag, since a `<div>` and a `<span>` type their refs differently.
  const blockRoot = useRef<HTMLDivElement>(null);
  const inlineRoot = useRef<HTMLSpanElement>(null);
  const inline = variant === "inline";
  const references = useTextReferences(referenceText, source);
  const { isFallback, isPending, portals } = useRenderedMarkdown(
    inline ? inlineRoot : blockRoot,
    markdown,
    tree,
    inline ? paintInline : paintBlock,
    {
      pictures: variant,
      onRendered,
      paintInSlices: inline ? undefined : paintBlockInSlices,
      rawHtml: "text",
      references,
      softBreaks,
    }
  );

  return inline ? (
    <span
      className={markdownClassName}
      data-markdown-fallback={isFallback || undefined}
      data-markdown-pending={isPending || undefined}
      ref={inlineRoot}
    >
      {portals}
    </span>
  ) : (
    <div
      className={`${markdownClassName} max-w-none`}
      data-markdown-fallback={isFallback || undefined}
      data-markdown-pending={isPending || undefined}
      ref={blockRoot}
    >
      {portals}
    </div>
  );
}
