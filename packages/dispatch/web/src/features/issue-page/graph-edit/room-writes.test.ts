import { afterEach, expect, test } from "bun:test";
import * as ProseMirrorState from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import * as Y from "yjs";

import { createEditor, type EditorHandle } from "../../doc/editor";
import { compiledBlockSchema } from "../../doc/schema";
import { writeNewBlock } from "./room-writes";

/**
 * This part's writes of the issue's room (`room-writes.ts`), made in the editor the page makes
 * (`doc/editor.ts`) over a room whose every update reaches a copy of its own, as the server holds
 * the room. A case reads that copy: the document the room stores, which the server indexes.
 */

/** A block of the room as y-prosemirror writes one: its type, its attributes - its `blockId`
 *  among them - and its words, none where it holds nothing. */
interface RoomBlock {
  readonly attrs?: Readonly<Record<string, string | number>>;
  readonly type: string;
  readonly words?: string;
}

/** The browser's copy of a room and the server's, each update to either relayed to the other. */
interface Room {
  readonly browser: Y.Doc;
  readonly server: Y.Doc;
}

/** A room whose fragment holds `blocks` before any editor of it is made. */
function roomOf(blocks: readonly RoomBlock[]): Room {
  const browser = new Y.Doc();
  const server = new Y.Doc();
  for (const [from, to] of [
    [server, browser],
    [browser, server],
  ] as const) {
    from.on("update", (update: Uint8Array, origin: unknown) => {
      if (origin !== "relay") Y.applyUpdate(to, update, "relay");
    });
  }
  const elements = blocks.map(({ attrs, type, words }) => {
    const element = new Y.XmlElement(type) as Y.XmlElement<Record<string, string | number>>;
    for (const [name, value] of Object.entries(attrs ?? {})) element.setAttribute(name, value);
    if (words !== undefined) element.insert(0, [new Y.XmlText(words)]);
    return element;
  });
  server.getXmlFragment("prosemirror").insert(0, elements);
  return { browser, server };
}

const made: { readonly handle: EditorHandle; readonly root: HTMLElement }[] = [];

afterEach(async () => {
  for (const { handle, root } of made.splice(0)) {
    await handle.destroy();
    root.remove();
  }
});

/** The view of the page's editor of `room`, made as a row's editor is made of it. */
async function editorOf(room: Room): Promise<EditorView> {
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

/** Each block the server's copy of `room` holds: its type, its block id and its words. Strings,
 *  never nodes, are what a case compares. */
function stored(room: Room): string[] {
  return room.server
    .getXmlFragment("prosemirror")
    .toArray()
    .map((block) => {
      if (!(block instanceof Y.XmlElement)) return "text";
      // `Y.XmlText.toString()` writes formatting marks (`proofAuthored`) as XML tags; its delta
      // holds the block's plain words.
      const words = block
        .toArray()
        .map((child) =>
          child instanceof Y.XmlText
            ? (child.toDelta() as readonly { readonly insert?: unknown }[])
                .map((op) => (typeof op.insert === "string" ? op.insert : ""))
                .join("")
            : ""
        )
        .join("");
      return `${block.nodeName} ${String(block.getAttribute("blockId"))}: ${words}`;
    });
}

// R3: the editor holds a fragment of no block as a document of one empty paragraph, and the first
// character's paragraph takes its place, so the room stores the typed paragraph alone, under the
// block id the page chose, with the selection after its words.
test("a new row's first character over a fragment that holds no block stores the typed paragraph alone", async () => {
  const room = roomOf([]);
  const view = await editorOf(room);
  expect(stored(room), "the room stores a block before the write").toEqual([]);
  expect(writeNewBlock(view, ProseMirrorState, "new-row", "Q")).toBe(true);
  expect(stored(room)).toEqual(["paragraph new-row: Q"]);
  view.dispatch(view.state.tr.insertText("uick"));
  expect(stored(room), "the reader's next letters went elsewhere").toEqual([
    "paragraph new-row: Quick",
  ]);
});

// R3: every block with words stays, in order, and so does a paragraph without words that stands
// between two of them; the paragraphs without words that end the fragment - one the server
// stamped, one of blank text - give the new paragraph their place at the end.
test("a new row's first character keeps the fragment's blocks and stands last in place of the empty paragraphs that end it", async () => {
  const room = roomOf([
    { attrs: { blockId: "heading", level: 2 }, type: "heading", words: "The issue's own words" },
    { attrs: { blockId: "between" }, type: "paragraph" },
    { attrs: { blockId: "words" }, type: "paragraph", words: "A paragraph of words." },
    { attrs: { blockId: "emptied" }, type: "paragraph" },
    { type: "paragraph", words: "  " },
  ]);
  const view = await editorOf(room);
  expect(writeNewBlock(view, ProseMirrorState, "new-row", "Q")).toBe(true);
  expect(stored(room)).toEqual([
    "heading heading: The issue's own words",
    "paragraph between: ",
    "paragraph words: A paragraph of words.",
    "paragraph new-row: Q",
  ]);
  view.dispatch(view.state.tr.insertText("uick"));
  expect(stored(room).at(-1), "the reader's next letters went elsewhere").toBe(
    "paragraph new-row: Quick"
  );
});
