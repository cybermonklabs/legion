import { expect, test } from "bun:test";
import { createProofEditor } from "@legion/proof-editor";
import { createHeadlessProof } from "@legion/proof-editor/headless";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import serverBlockSchema from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import type { BlockSchema } from "../../api/types";
import { dropFirstScreen, firstScreenAwaits, takeFirstScreen } from "./first-screen";

/**
 * What the server's draw may never touch: the document in the reader's own editor.
 *
 * An `id` is one namespace for the whole page, and the document's own text writes into it:
 * milkdown's heading schema gives a heading that carries none the slug of its text
 * (`defaultHeadingIdGenerator`), so a document opening with a heading called "First screen" is
 * drawn as `<h2 id="first-screen">` inside the live editor - the same id `index.html` gives the
 * element the server draws into. Once that element has been taken (`DocumentPreview`'s first
 * commit), a lookup by id alone finds the heading, and a tab switch - `openDocumentEarly`'s
 * `dropFirstScreen` on every `pathname` change - removed it from the editor, where ProseMirror's
 * DOM observer read the removal as the reader's own edit and sent it to every other reader
 * (dispatch://INFRA-341).
 */

const blockSchema = serverBlockSchema as unknown as BlockSchema;

/** The document the reader has open, in a live editor over `ydoc`, as the page builds one. */
async function liveEditor(
  markdown: string,
  ydoc: Y.Doc
): Promise<{ destroy: () => Promise<void>; root: HTMLElement }> {
  const { parseMarkdown } = await createHeadlessProof({ blockSchema, readOnly: true });
  prosemirrorToYXmlFragment(parseMarkdown(markdown), ydoc.getXmlFragment("prosemirror"));
  const page = document.getElementById("root") as HTMLElement;
  const root = page.appendChild(document.createElement("div"));
  const handle = await createProofEditor(root, {
    blockSchema,
    heatMapMode: "hidden",
    onMarkClick: () => {},
    onMarkHover: () => {},
    user: { color: "#0369a1", name: "alice" },
    ydoc,
  });
  handle.reveal();
  return { destroy: () => handle.destroy(), root };
}

test("a tab switch leaves the live editor's own heading, and its document, where they are", async () => {
  const address = "/issues/CORE-1/artifacts/spec-md";
  // The page as the server sent it, and the preview adopting the server's blocks on its first
  // commit, which takes that element out of the page.
  document.body.innerHTML =
    `<div id="root"></div>` +
    `<div id="first-screen">` +
    `<div role="document" aria-label="Document text" data-first-screen-address="${address}">` +
    `<h2 data-block-id="b-1">First screen</h2></div></div>`;
  expect(takeFirstScreen(address)).toBeDefined();

  const ydoc = new Y.Doc();
  const editor = await liveEditor("## First screen\n\nThe document's own words.\n", ydoc);
  try {
    const heading = editor.root.querySelector("h2");
    // The editor drew the document's own heading under the id the server's element carried.
    expect(heading?.id).toBe("first-screen");
    const blockId = heading?.getAttribute("data-block-id");
    const fragment = ydoc.getXmlFragment("prosemirror");
    const document_ = fragment.toJSON();

    // The tab switch: `/issues/CORE-1` to `/issues/CORE-1/conversation`, which reaches
    // `dropFirstScreen` with the new address while the document's panel stays mounted.
    dropFirstScreen("/issues/CORE-1/conversation");
    // ProseMirror's DOM observer reads a removal in the microtask its MutationObserver is
    // delivered in, so the document it would write is written by now.
    await Promise.resolve();

    // The Yjs document is what must not change: it is every reader's, and every comment, ask
    // and mark is anchored to the blocks in it.
    expect(fragment.toJSON()).toBe(document_);
    const after = editor.root.querySelector(`[data-block-id="${blockId}"]`);
    expect(after?.tagName).toBe("H2");
    expect(firstScreenAwaits("/issues/CORE-1/conversation")).toBe(false);
  } finally {
    await editor.destroy();
    document.body.innerHTML = "";
  }
});
