import { Plugin, PluginKey } from "@milkdown/kit/prose/state";
import { prosemirrorToYXmlFragment, yCursorPluginKey } from "y-prosemirror";
import { Awareness } from "y-protocols/awareness";
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
  });
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

await test("after rebindElement the root has the target's own type and attributes, blockId among them", async () => {
  const room = await seededRoom();
  const fragment = room.getXmlFragment("prosemirror");
  const source = findBlock(fragment, "r1");
  const before = source.toString();
  const target = new Y.XmlElement("paragraph");
  target.insert(0, [new Y.XmlText("An independent paragraph.")]);
  target.setAttribute("blockId", "p1");
  fragment.insert(2, [target]);

  try {
    await withEditor(room, { element: source }, (handle) => {
      const view = handle.view;
      handle.rebindElement(target);
      assert(handle.view === view, "rebind must keep the view");
      assert(view.state.doc.type.name === "paragraph", "rebind must take the target's type");
      assert(view.state.doc.attrs.blockId === "p1", "rebind must take the target's blockId");
      assert(
        view.state.doc.textContent === "An independent paragraph.",
        "rebind must draw the target's text"
      );
      view.dispatch(view.state.tr.insertText("Rebound. ", 0));
      assert(
        target.toString().includes("Rebound. An independent paragraph."),
        "typed text must reach the target"
      );
      assert(source.toString() === before, "rebind and typing must leave the source untouched");
    });
  } finally {
    room.destroy();
  }
});

await test("rebindElement keeps every plugin the view holds, including readingsInOpenAsksPlugin", async () => {
  const room = await seededRoom();
  const fragment = room.getXmlFragment("prosemirror");
  const awareness = new Awareness(room);
  try {
    await withEditor(room, { element: findBlock(fragment, "r1"), awareness }, (handle) => {
      const { view } = handle;
      // As Dispatch's installTypedBlocks does, append a transaction filter to the state
      // without putting it in Milkdown's ctx; add a second plugin through the view props.
      const readingsInOpenAsks = new Plugin({
        key: new PluginKey("dispatch-readings-in-open-asks"),
      });
      const hostView = new Plugin({ key: new PluginKey("dispatch-ask-block-editing") });
      view.updateState(
        view.state.reconfigure({ plugins: [...view.state.plugins, readingsInOpenAsks] })
      );
      view.setProps({ plugins: [...(view.props.plugins ?? []), hostView] });
      const statePlugins = [...view.state.plugins];
      const viewPlugins = [...(view.props.plugins ?? [])];

      handle.rebindElement(findBlock(fragment, "r2"), awareness);

      // Collaboration gets new instances on the new element, under the same keys.
      for (const plugin of statePlugins) {
        const key = plugin.spec.key;
        assert(
          key === undefined
            ? view.state.plugins.includes(plugin)
            : key.get(view.state) !== undefined,
          `the rebind lost a state plugin: ${String(key)}`
        );
      }
      assert(
        view.state.plugins.includes(readingsInOpenAsks),
        "the rebind lost the live ask-state filter"
      );
      for (const plugin of viewPlugins) {
        assert(view.props.plugins?.includes(plugin), "the rebind lost a view-prop plugin");
      }
    });
  } finally {
    awareness.destroy();
    room.destroy();
  }
});

await test("a cursor install of the old bind does not run after the rebind", async () => {
  const room = await seededRoom();
  const empty = new Y.XmlElement("paragraph");
  room.getXmlFragment("scratch").insert(0, [empty]);
  const awareness = new Awareness(room);
  const frames: FrameRequestCallback[] = [];
  const original = Object.getOwnPropertyDescriptor(globalThis, "requestAnimationFrame");
  Object.defineProperty(globalThis, "requestAnimationFrame", {
    configurable: true,
    value: (frame: FrameRequestCallback) => frames.push(frame),
  });
  try {
    await withEditor(room, { element: empty, awareness }, (handle) => {
      assert(frames.length > 0, "the empty block must leave its first cursor install waiting");
      assert(
        yCursorPluginKey.get(handle.view.state) === undefined,
        "the first cursor is not installed yet"
      );
      // A populated target has a sync mapping, but this bind asks for no cursors.
      handle.rebindElement(findBlock(room.getXmlFragment("prosemirror"), "r2"));
      for (const frame of frames.splice(0)) frame(0);
      assert(
        yCursorPluginKey.get(handle.view.state) === undefined,
        "a stale cursor install reached the new bind"
      );
    });
  } finally {
    if (original === undefined) Reflect.deleteProperty(globalThis, "requestAnimationFrame");
    else Object.defineProperty(globalThis, "requestAnimationFrame", original);
    awareness.destroy();
    room.destroy();
  }
});

await test("a full-document editor made without element refuses rebindElement", async () => {
  const room = await seededRoom();
  try {
    await withEditor(room, {}, (handle) => {
      assert(
        typeof handle.rebindElement === "function",
        "the handle must expose the rebind method"
      );
      const before = handle.view.state.doc;
      let refusal: unknown;
      try {
        handle.rebindElement(findBlock(room.getXmlFragment("prosemirror"), "r2"));
      } catch (error) {
        refusal = error;
      }
      assert(
        refusal instanceof Error && refusal.message === "Only a block editor can be rebound.",
        "a full-document editor must refuse the rebind"
      );
      assert(handle.view.state.doc === before, "the refusal must leave the full document bound");
    });
  } finally {
    room.destroy();
  }
});
