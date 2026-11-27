import { expect, test } from "bun:test";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import {
  type CreateProofEditorOptions,
  createProofEditor,
  type ProofEditorHandle,
} from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withDomWindow } from "./dom-window";

/**
 * The fragment of the host's Yjs document an editor binds (the issue-tree spec). An issue's room is
 * one document holding one fragment per document of the issue, each named by that document's
 * artifact id, beside the issue's own fragment, `prosemirror`; the host names the fragment its
 * document is, and an editor given no name binds `prosemirror`, the whole of a room that is its
 * own document.
 */

/** A document's fragment in its issue's room: named by the document's artifact id. */
const DOCUMENT_FRAGMENT = "0b4f6c1e-9d2a-4e7b-8c35-2f1a6d9e7b40";

/** A room holding the issue's own fragment and one document's, each with a paragraph of its own. */
async function issueRoom(): Promise<Y.Doc> {
  const { parseMarkdown } = await createHeadlessProof();
  const room = new Y.Doc();
  prosemirrorToYXmlFragment(
    parseMarkdown("The issue's own node.\n"),
    room.getXmlFragment("prosemirror")
  );
  prosemirrorToYXmlFragment(
    parseMarkdown("The plan's text.\n"),
    room.getXmlFragment(DOCUMENT_FRAGMENT)
  );
  return room;
}

/** Runs `run` against the editor `createProofEditor` builds over `room` with `options`. */
async function withEditor(
  room: Y.Doc,
  options: Partial<CreateProofEditorOptions>,
  run: (handle: ProofEditorHandle) => void | Promise<void>
): Promise<void> {
  await withDomWindow(async (window) => {
    const root = window.document.body.appendChild(window.document.createElement("div"));
    const handle = await createProofEditor(root as unknown as HTMLElement, {
      heatMapMode: "hidden",
      onMarkClick: () => {},
      onMarkHover: () => {},
      user: { color: "#0369a1", name: "alice" },
      ydoc: room,
      ...options,
    });
    try {
      await run(handle);
    } finally {
      // The editor's own teardown reads the DOM as it goes, so it ends while the window is open.
      await handle.destroy();
    }
  });
}

test("an editor given a fragment draws that fragment of its document and writes its edits there", async () => {
  const room = await issueRoom();
  const issueOwn = room.getXmlFragment("prosemirror").toString();
  await withEditor(room, { fragment: DOCUMENT_FRAGMENT }, ({ view }) => {
    expect(view.state.doc.textContent).toBe("The plan's text.");

    view.dispatch(view.state.tr.insertText(" Typed.", view.state.doc.content.size - 1));

    expect(room.getXmlFragment(DOCUMENT_FRAGMENT).toString()).toContain("The plan's text. Typed.");
    expect(room.getXmlFragment("prosemirror").toString()).toBe(issueOwn);
  });
});

test("an editor given no fragment binds prosemirror", async () => {
  const room = await issueRoom();
  const documentOwn = room.getXmlFragment(DOCUMENT_FRAGMENT).toString();
  await withEditor(room, {}, ({ view }) => {
    expect(view.state.doc.textContent).toBe("The issue's own node.");

    view.dispatch(view.state.tr.insertText(" Typed.", view.state.doc.content.size - 1));

    expect(room.getXmlFragment("prosemirror").toString()).toContain("The issue's own node. Typed.");
    expect(room.getXmlFragment(DOCUMENT_FRAGMENT).toString()).toBe(documentOwn);
  });
});
