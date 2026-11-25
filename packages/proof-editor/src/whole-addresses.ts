/**
 * Keeps a bare `dispatch://` address whole, on markdown import.
 *
 * A person's address is `dispatch://person/<login>`, and a login is an email address. remark-gfm
 * reads bare addresses twice, and both of its readers start an email or a URL after punctuation:
 * micromark-extension-gfm-autolink-literal's tokenizer starts an email after a `=`, a URL after a
 * `;` and a `www.` after a `_`, and mdast-util-gfm-autolink-literal's transform starts either after
 * a `.` or a `-`. So `dispatch://person/john.doe@example.com` reads as the text
 * `dispatch://person/john.` and a link to `mailto:doe@example.com`, and no reader has the address
 * whole to name the person by.
 *
 * After both readers have run, this puts back as the text it showed every literal link, one either
 * reader made, that starts inside a `dispatch://` address of its run of text: the address spans of
 * `@legion/contracts`' `referenceSpans(text, "dispatch://")`, the rule the server reads a reference
 * by. An angle autolink (`<https://a.test>`) and a written link (`[a](b)`) stay as they are, and so
 * does every literal link that starts outside an address.
 *
 * Its twin is `keepAddressesWhole` in `packages/envoy/internal/dispatch/pmdoc/whole_addresses.go`,
 * which Go's reader runs over the same runs, so the two readers of a document agree.
 *
 * Import only: a unified transformer runs on parse (`ParserState.create` calls `remark.runSync`
 * with the markdown as its file, `@milkdown/transformer/lib/index.js:160`) and never on
 * `stringify`, so registering it on the editor's shared remark processor cannot change what the
 * editor writes.
 */

import { referenceSpans } from "@legion/contracts";
import type { Root } from "@milkdown/kit/transformer";
import { $remark } from "@milkdown/kit/utils";
import type { Transformer } from "unified";
import { markdownSource } from "./typed-block-drawing";

type MdastNode = {
  type: string;
  value?: string;
  children?: MdastNode[];
  position?: { start: { offset?: number } };
};

/**
 * The text `node` shows when it is a literal link, one of remark-gfm's readers made of a bare
 * address; otherwise undefined. A literal link holds exactly one text. The transform's link has no
 * position, and neither has its text; a link remark-inline-links made has none either, but its
 * text has one, and it is no literal. The tokenizer's link starts at the address's first
 * character, where an angle autolink starts at its `<` and a written link at its `[`.
 */
function literalText(node: MdastNode, source: string): string | undefined {
  if (node.type !== "link" || node.children?.length !== 1) return undefined;
  const [text] = node.children;
  if (text.type !== "text" || text.value === undefined) return undefined;
  const start = node.position?.start.offset;
  if (start === undefined) return text.position === undefined ? text.value : undefined;
  const opener = source.charAt(start);
  return opener === "<" || opener === "[" ? undefined : text.value;
}

/**
 * Replaces each link of the run that starts at `children[from]`, whose nodes show `shown`, with a
 * text of what it showed, where the link starts inside a `dispatch://` address of the run's text.
 * It merges no text into the texts beside it: Milkdown joins texts that carry the same marks, and
 * Go merges none.
 */
function unlinkInRun(children: MdastNode[], from: number, shown: string[]): void {
  const spans = referenceSpans(shown.join(""), "dispatch://");
  let span = 0;
  let start = 0;
  for (let index = 0; index < shown.length; index += 1) {
    const child = children[from + index];
    if (child.type !== "text") {
      while (span < spans.length && spans[span].start + spans[span].value.length <= start) {
        span += 1;
      }
      if (span === spans.length) return;
      if (spans[span].start <= start) {
        const text: MdastNode = { type: "text", value: shown[index] };
        if (child.position !== undefined) text.position = child.position;
        children[from + index] = text;
      }
    }
    start += shown[index].length;
  }
}

/**
 * Walks `node`: in each run of its children that are `text` nodes and literal links
 * (`literalText`), and that holds a link, puts back as text each link inside an address
 * (`unlinkInRun`), then walks each child. `shown` is scratch space, the text each node of a run
 * shows.
 */
function keepWhole(node: MdastNode, source: string, shown: string[]): void {
  const children = node.children;
  if (children === undefined) return;
  for (let from = 0; from < children.length; from += shown.length + 1) {
    shown.length = 0;
    let links = false;
    for (let index = from; index < children.length; index += 1) {
      const child = children[index];
      const text = child.type === "text" ? child.value : literalText(child, source);
      if (text === undefined) break;
      links ||= child.type !== "text";
      shown.push(text);
    }
    if (links) unlinkInRun(children, from, shown);
  }
  for (const child of children) keepWhole(child, source, shown);
}

/**
 * remark transformer: every parse seam registers it, right after remark-gfm; `stringify` never
 * runs it. It reads each link's opener from the markdown the processor is given as its file, as
 * the tree's offsets count into it (`markdownSource`). An empty document's file holds no markdown,
 * and a tree without one has nothing to read.
 */
export function remarkKeepAddressesWhole() {
  return (tree: MdastNode, file: { value?: unknown; toString(): string }) => {
    if (typeof file.value !== "string") {
      if ((tree.children?.length ?? 0) > 0) {
        throw new Error(
          "remarkKeepAddressesWhole: the file holds no markdown, so no link's opener can be read; run the processor with the markdown as its file"
        );
      }
      return;
    }
    keepWhole(tree, markdownSource(file), []);
  };
}

export const libraryRemarkKeepAddressesWholePlugin = $remark(
  "remarkKeepAddressesWhole",
  () => remarkKeepAddressesWhole as () => Transformer<Root, Root>
);
