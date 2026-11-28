import { afterEach, expect, test } from "bun:test";
import * as ProseMirrorState from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";

import { createEditor, type EditorHandle } from "../../doc/editor";
import { compiledBlockSchema } from "../../doc/schema";
import {
  fragmentOf,
  type LiveBlock,
  type LiveRoom,
  liveRoom,
  paragraphOf,
  roomBlock,
} from "./graph-edit-fixtures";
import { writeNewBlock } from "./room-writes";

/**
 * This part's writes of the issue's room (`room-writes.ts`), made in the editor the page makes
 * (`doc/editor.ts`) over a room whose every update reaches a copy of its own, as the server holds
 * the room. A case reads that copy: the document the room stores, which the server indexes.
 */

const made: { readonly handle: EditorHandle; readonly root: HTMLElement }[] = [];

afterEach(async () => {
  for (const { handle, root } of made.splice(0)) {
    await handle.destroy();
    root.remove();
  }
});

/** The view of the page's editor of `room`, made as a row's editor is made of it. */
async function editorOf(room: LiveRoom): Promise<EditorView> {
  const root = document.body.appendChild(document.createElement("div"));
  const handle = await createEditor(root, {
    awareness: null,
    blockSchema: compiledBlockSchema,
    // The cases' documents hold no decision block and no picture.
    onAskBlockHosts: () => {},
    user: { color: "#f59e0b", name: "alice" },
    watchPicture: () => () => {},
    ydoc: room.browser,
  });
  made.push({ handle, root });
  return handle.view;
}

// R3: the editor holds a fragment of no block as a document of one empty paragraph, and the first
// character's paragraph takes its place, so the room stores the typed paragraph alone, under the
// block id the page chose, with the selection after its words.
test("a new row's first character over a fragment that holds no block stores the typed paragraph alone", async () => {
  const room = liveRoom();
  const view = await editorOf(room);
  expect(fragmentOf(room), "the room stores a block before the write").toEqual([]);
  expect(writeNewBlock(view, ProseMirrorState, "new-row", "Q")).toBe(true);
  expect(fragmentOf(room)).toEqual(["paragraph new-row: Q"]);
  view.dispatch(view.state.tr.insertText("uick"));
  expect(fragmentOf(room), "the reader's next letters went elsewhere").toEqual([
    "paragraph new-row: Quick",
  ]);
});

// R3: every block with words stays, in order, and so does a paragraph without words that stands
// between two of them; the paragraphs without words that end the fragment - one the server
// stamped, one of blank text - give the new paragraph their place at the end.
test("a new row's first character keeps the fragment's blocks and stands last in place of the empty paragraphs that end it", async () => {
  const blocks: readonly LiveBlock[] = [
    {
      attrs: { blockId: "heading", level: 2 },
      content: [{ text: "The issue's own words", type: "text" }],
      type: "heading",
    },
    { attrs: { blockId: "between" }, type: "paragraph" },
    paragraphOf("A paragraph of words.", "words"),
    { attrs: { blockId: "emptied" }, type: "paragraph" },
    paragraphOf("  "),
  ];
  const room = liveRoom();
  room.server.getXmlFragment("prosemirror").insert(0, blocks.map(roomBlock));
  const view = await editorOf(room);
  expect(writeNewBlock(view, ProseMirrorState, "new-row", "Q")).toBe(true);
  expect(fragmentOf(room)).toEqual([
    "heading heading: The issue's own words",
    "paragraph between: ",
    "paragraph words: A paragraph of words.",
    "paragraph new-row: Q",
  ]);
  view.dispatch(view.state.tr.insertText("uick"));
  expect(fragmentOf(room).at(-1), "the reader's next letters went elsewhere").toBe(
    "paragraph new-row: Quick"
  );
});
