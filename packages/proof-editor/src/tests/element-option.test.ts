import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import { serverBlockSchema } from "../../tests/block-schema-fixture.js";
import { withDomWindow } from "../../tests/dom-window.js";
import {
  type CreateProofEditorOptions,
  createProofEditor,
  type ProofEditorHandle,
} from "../lib.js";
import { createHeadlessProof } from "../lib-headless.js";
import { test } from "./harness.js";

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

const SPEC_BODY = [
  ":::requirement{#r1}",
  "First requirement body.",
  ":::",
  "",
  ":::requirement{#r2}",
  "Second requirement body.",
  ":::",
  "",
].join("\n");

async function seededRoom(): Promise<Y.Doc> {
  const { parseMarkdown } = await createHeadlessProof({ blockSchema: serverBlockSchema });
  const room = new Y.Doc();
  prosemirrorToYXmlFragment(parseMarkdown(SPEC_BODY), room.getXmlFragment("prosemirror"));
  return room;
}

function findBlock(fragment: Y.XmlFragment, blockId: string): Y.XmlElement {
  for (const child of fragment.toArray()) {
    if (child instanceof Y.XmlElement && child.getAttribute("blockId") === blockId) {
      return child;
    }
  }
  throw new Error(`expected block ${JSON.stringify(blockId)} in fragment`);
}

async function withEditor(
  room: Y.Doc,
  options: Partial<CreateProofEditorOptions>,
  run: (handle: ProofEditorHandle) => void | Promise<void>
): Promise<void> {
  await withDomWindow(async (window) => {
    const root = window.document.body.appendChild(window.document.createElement("div"));
    const handle = await createProofEditor(root as unknown as HTMLElement, {
      blockSchema: serverBlockSchema,
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
      await handle.destroy();
    }
  });
}

console.log("\n=== createProofEditor element option ===");

await test("without element the editor draws and saves what the base does", async () => {
  const room = await seededRoom();
  await withEditor(room, {}, (handle) => {
    const { view } = handle;
    assert(
      view.state.doc.type.name === "doc",
      `expected root node type "doc", got ${JSON.stringify(view.state.doc.type.name)}`
    );
    assert(
      view.state.doc.childCount === 2,
      `expected both blocks in the default view, got ${view.state.doc.childCount}`
    );
    assert(
      view.state.doc.textContent.includes("First requirement body.") &&
        view.state.doc.textContent.includes("Second requirement body."),
      `expected both blocks' text in the default view, got ${JSON.stringify(view.state.doc.textContent)}`
    );

    const endOfFirst = view.state.doc.child(0).nodeSize - 2;
    view.dispatch(view.state.tr.insertText(" Saved.", endOfFirst));

    const fragmentText = room.getXmlFragment("prosemirror").toString();
    assert(
      fragmentText.includes("First requirement body. Saved."),
      `expected edit written to the room fragment, got ${JSON.stringify(fragmentText)}`
    );
    const saved = handle.getMarkdown();
    assert(
      saved.includes("#r1") &&
        saved.includes("First requirement body. Saved.") &&
        saved.includes("#r2") &&
        saved.includes("Second requirement body."),
      `expected markdown to save both blocks as at the base, got ${JSON.stringify(saved)}`
    );
  });
});

await test("with element the view holds that block alone, with its type and attributes", async () => {
  const room = await seededRoom();
  const fragment = room.getXmlFragment("prosemirror");
  const r1 = findBlock(fragment, "r1");
  const r2 = findBlock(fragment, "r2");
  const r2Before = r2.toString();

  await withEditor(room, { element: r1 }, (handle) => {
      const { view } = handle;
      assert(
        view.state.doc.type.name === "requirement",
        `expected root node type "requirement", got ${JSON.stringify(view.state.doc.type.name)}`
      );
      assert(
        view.state.doc.attrs.blockId === "r1",
        `expected root blockId "r1", got ${JSON.stringify(view.state.doc.attrs.blockId)}`
      );
      assert(
        view.state.doc.textContent === "First requirement body.",
        `expected view to hold r1 alone, got ${JSON.stringify(view.state.doc.textContent)}`
      );
      assert(
        !view.state.doc.textContent.includes("Second requirement body."),
        "expected view not to include the sibling block"
      );

      const endOfParagraph = view.state.doc.content.size - 1;
      view.dispatch(view.state.tr.insertText(" Edited.", endOfParagraph));

      assert(
        r1.toString().includes("First requirement body. Edited."),
        `expected edit written inside r1, got ${JSON.stringify(r1.toString())}`
      );
      assert(
        r1.getAttribute("blockId") === "r1",
        `expected r1 to keep blockId "r1", got ${JSON.stringify(r1.getAttribute("blockId"))}`
      );
      assert(
        r2.toString() === r2Before,
        `expected sibling block r2 untouched, got ${JSON.stringify(r2.toString())}`
      );
      const serialized = handle.getMarkdown();
      assert(
        serialized.includes("#r1") && serialized.includes("First requirement body. Edited."),
        `expected getMarkdown() to serialize the single-block root, got ${JSON.stringify(serialized)}`
      );
    }
  );
});

await test("an element of a document-level node is refused", async () => {
  const room = await seededRoom();
  const fragment = room.getXmlFragment("prosemirror");
  const docElement = new Y.XmlElement("doc");
  room.getXmlFragment("scratch").insert(0, [docElement]);

  for (const [label, candidate] of [
    ["a bare XmlFragment", fragment as unknown as Y.XmlElement],
    ["a doc XmlElement", docElement],
  ] as const) {
    let refused = false;
    try {
      await withEditor(room, { element: candidate }, () => {});
    } catch {
      refused = true;
    }
    assert(refused, `expected createProofEditor to refuse ${label} as element`);
  }
});
