import type { DispatchReadingSurface } from "@legion/contracts";
import type { Node as ProseMirrorNode } from "prosemirror-model";
import { type ReactNode, useRef } from "react";

import type { DisplayTree } from "../../api/types";
import { markdownClassName, type SoftBreaks } from "./markdown-engine";
import { paintBlock, paintBlockInSlices } from "./paint-block";
import { hostDrawsMarkup, hostLinksReferences, useReferenceHost } from "./reference-host";
import { RecordIssue } from "./role-holders";
import type { TextSource } from "./text-references";
import { UnfurlRead } from "./Unfurl";
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

/** What every body draws from: the text, the reading of it the server sent, and what the
 *  surface drawing it says about it. One shape for the public body and for the painted one it
 *  hands its props to (`PaintedBody`). */
interface MarkdownBodyProps {
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
  /** The row of the reading list this body is drawn on (`DISPATCH_READING_SURFACES`). The body
   *  draws nothing from it: it names the surface a mount draws, which is how the list's unit
   *  test ties each mount to its row (`surface-lists.test.ts`). */
  surface?: DispatchReadingSurface;
  /** The reading Go made of this stored text, which the body draws rather than parsing the
   *  markdown again. Absent for the three texts Dispatch does not store - a live agent turn, a
   *  GitHub body in a link preview, and a write the server has not stored yet - and for a stored
   *  text whose answer came from a pod of the build before this one during a rolling deploy,
   *  which is why `reference-target.ts` and `markdown-engine.ts` still reach the browser parser
   *  for a stored text: that fallback is the two-builds case, not a defect. */
  tree?: DisplayTree;
  variant?: "block" | "inline";
}

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
 *
 * A body that is nothing but references draws a card for each of them under itself
 * (`Unfurl`): one renderer draws the record and everything that hangs off it, so no surface
 * pairs a body with a card of its own. Whether a reference in the body is a live link, and
 * whether those cards are drawn at all, is the host's property (`ReferenceHostIs`): inside a
 * link or a button nothing is a link of its own, and an element that can hold no markup - or a
 * diff, whose point is the characters it swaps - holds this text as the characters it is.
 *
 * The body takes no class. Its size and its line come from the element around it
 * (`.dispatch-markdown`'s `font-size: inherit; line-height: inherit`, styles.css, a rule in no
 * layer that overrides any utility on the renderer's own element), so a caller sizes a wrapper,
 * as it does for `MarkdownPreview`.
 */
export function MarkdownBody(props: MarkdownBodyProps): ReactNode {
  const host = useReferenceHost();
  return hostDrawsMarkup(host) ? <PaintedBody {...props} /> : props.markdown;
}

/** The body as a host that draws markup draws it: painted, with its references linked where the
 *  host links them, and the cards under a body that is nothing but references. */
function PaintedBody({
  markdown,
  onRendered,
  referenceText = markdown,
  softBreaks = "space",
  source,
  tree,
  variant = "block",
}: MarkdownBodyProps): ReactNode {
  const host = useReferenceHost();
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
  // Each reference drawn in the body reads its role by the issue the record is on, and by no
  // issue for a text that is no record's (`RecordIssue`).
  const links = <RecordIssue.Provider value={source?.issue}>{portals}</RecordIssue.Provider>;

  if (inline) {
    return (
      <span
        className={markdownClassName}
        data-markdown-fallback={isFallback || undefined}
        data-markdown-pending={isPending || undefined}
        ref={inlineRoot}
      >
        {links}
      </span>
    );
  }
  return (
    <>
      <div
        className={`${markdownClassName} max-w-none`}
        data-markdown-fallback={isFallback || undefined}
        data-markdown-pending={isPending || undefined}
        ref={blockRoot}
      >
        {links}
      </div>
      {/* A card is a link, so a body inside a link or a button draws none: there is nothing for
          the card to be. One line of a card has no room for one either, which is why the
          `inline` variant above ends there. The cards are read from the readings this body was
          linked by, so the text is read once however many readers of it the page holds - and
          those readings are of `referenceText`, so a caller drawing one block of a longer field
          draws no cards: the references are the field's, and the body that holds it whole is
          where its cards belong. */}
      {hostLinksReferences(host) && referenceText === markdown ? (
        <UnfurlRead
          body={referenceText}
          reading={references.reading}
          surface="body-reference-cards"
          written={references.written}
        />
      ) : null}
    </>
  );
}
