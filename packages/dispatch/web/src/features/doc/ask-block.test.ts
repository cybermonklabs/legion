import { beforeAll, expect, test } from "bun:test";
import { EditorState, TextSelection } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import { DOMSerializer, Schema } from "prosemirror-model";

import type { BlockTypeSchema } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { askBlockEditingPlugin, editingAskBlockPos, installTypedBlocks } from "./ask-block";
import { renderTypedBlock } from "./typed-block-drawing";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const schema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    bullet_list: { content: "list_item+", group: "block" },
    list_item: { content: "paragraph+" },
    ask: {
      attrs: { blockId: { default: null } },
      content: "paragraph+ bullet_list?",
      group: "block",
    },
  },
});

function ask(blockId: string, question: string, ...options: string[]) {
  return schema.node("ask", { blockId }, [
    schema.node("paragraph", undefined, [schema.text(question)]),
    schema.node(
      "bullet_list",
      undefined,
      options.map((option) =>
        schema.node("list_item", undefined, [
          schema.node("paragraph", undefined, [schema.text(option)]),
        ])
      )
    ),
  ]);
}

// doc: paragraph "Context" (0..9), ask "storage" (9..), ask "naming" after it.
const doc = schema.node("doc", undefined, [
  schema.node("paragraph", undefined, [schema.text("Context")]),
  ask("storage", "Which engine?", "Postgres", "SQLite"),
  ask("naming", "Which name?", "legion", "lg"),
]);
const storagePos = 9;
const namingPos = storagePos + doc.child(1).nodeSize;

/** The plugin only needs the state and the node-view DOM lookup; a real `EditorView` needs a
 * browser selection this test does not. */
function fakeView(state: EditorState, shells: Record<number, HTMLElement>) {
  const view = {
    nodeDOM: (pos: number) => shells[pos] ?? null,
    state,
  } as unknown as EditorView & { state: EditorState };
  return view;
}

function caretAt(state: EditorState, pos: number): EditorState {
  return state.apply(state.tr.setSelection(TextSelection.create(state.doc, pos)));
}

test("editingAskBlockPos names the ask node around the caret and nothing outside one", () => {
  const start = EditorState.create({ doc, schema });
  expect(editingAskBlockPos(start)).toBeUndefined();
  // Inside the storage question text.
  expect(editingAskBlockPos(caretAt(start, storagePos + 3))).toBe(storagePos);
  // Inside the second option of the storage list, three levels down.
  const sqlite = storagePos + doc.child(1).nodeSize - 4;
  expect(editingAskBlockPos(caretAt(start, sqlite))).toBe(storagePos);
  expect(editingAskBlockPos(caretAt(start, namingPos + 2))).toBe(namingPos);
  expect(editingAskBlockPos(caretAt(start, 3))).toBeUndefined();
});

test("the editing plugin marks exactly the block the caret is inside, and clears it when the caret leaves", () => {
  const storage = document.createElement("section");
  const naming = document.createElement("section");
  const shells = { [storagePos]: storage, [namingPos]: naming };
  const initial = EditorState.create({ doc, schema });
  const view = fakeView(initial, shells);
  const pluginView = askBlockEditingPlugin.spec.view?.(view);
  if (pluginView?.update === undefined) throw new Error("the plugin has no view");

  expect(storage.dataset.dispatchAskEditing).toBeUndefined();
  expect(naming.dataset.dispatchAskEditing).toBeUndefined();

  view.state = caretAt(initial, storagePos + 3);
  pluginView.update(view, initial);
  expect(storage.dataset.dispatchAskEditing).toBe("true");
  expect(naming.dataset.dispatchAskEditing).toBeUndefined();

  view.state = caretAt(initial, namingPos + 2);
  pluginView.update(view, initial);
  expect(storage.dataset.dispatchAskEditing).toBeUndefined();
  expect(naming.dataset.dispatchAskEditing).toBe("true");

  view.state = caretAt(initial, 3);
  pluginView.update(view, initial);
  expect(storage.dataset.dispatchAskEditing).toBeUndefined();
  expect(naming.dataset.dispatchAskEditing).toBeUndefined();

  view.state = caretAt(initial, storagePos + 3);
  pluginView.update(view, initial);
  expect(storage.dataset.dispatchAskEditing).toBe("true");
  pluginView.destroy?.();
  expect(storage.dataset.dispatchAskEditing).toBeUndefined();
});

// The header shows what the document's author wrote and nothing else. Which attributes are the
// server's is the block type's own schema, not a list kept here: a hand-written copy of that
// flag goes stale the moment a type gains a server-owned attribute, and the new one leaks into
// every reader's view of the block.
const calloutSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    callout: {
      attrs: {
        blockId: { default: null },
        kind: { default: "note" },
        // A server-owned attribute this file has never heard of.
        resolved_by: { default: null },
        title: { default: null },
      },
      content: "paragraph+",
      group: "block",
    },
  },
});

const calloutType: BlockTypeSchema = {
  attributes: {
    kind: { kind: "enum", choices: ["note", "warning"] },
    resolved_by: { kind: "string", server: true },
    title: { kind: "string" },
  },
  content: "paragraph+",
  name: "callout",
  render: "host",
};

/** Every string the drawing puts on the page, in order. */
function renderedText(spec: unknown): string[] {
  if (typeof spec === "string") return [spec];
  if (!Array.isArray(spec)) return [];
  return spec.flatMap((child, index) =>
    index === 1 && !Array.isArray(child) && typeof child === "object" ? [] : renderedText(child)
  );
}

test("a typed block's header shows the author's attributes and hides the schema's server ones", () => {
  const node = calloutSchema.node(
    "callout",
    { blockId: "callout-1", kind: "warning", resolved_by: "alice", title: "Read this" },
    [calloutSchema.node("paragraph", undefined, [calloutSchema.text("Body text.")])]
  );

  const text = renderedText(renderTypedBlock(node, calloutType));

  expect(text).toContain("callout");
  expect(text).toContain("warning");
  expect(text).toContain("Read this");
  // The block's identity and the schema's server-owned attribute, neither of them written by
  // the author, appear nowhere - not as a value and not as a name.
  expect(text).not.toContain("callout-1");
  expect(text).not.toContain("blockId");
  expect(text).not.toContain("alice");
  expect(text).not.toContain("resolved_by");
});

// A `check` block, whose `serves` points at the requirements it proves. The drawing printed
// every array attribute with `JSON.stringify`, so driving the real page showed a reader
// `serves ["dispatch://VER-1/spec#b-r1"]` - the value's JSON instead of the blocks it names
// (INFRA-174). Each node draws as the Proof schema's `toDOM` draws it, a typed block as its
// section and content alone, for an editor to draw before its node views are installed.
const checkSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block", toDOM: () => ["p", 0] },
    text: { group: "inline" },
    check: {
      attrs: { blockId: { default: null }, run: { default: null }, serves: { default: null } },
      content: "paragraph+",
      group: "block",
      toDOM: () => ["section", ["div", { "data-proof-block-content": "" }, 0]],
    },
  },
});

const checkType: BlockTypeSchema = {
  attributes: {
    run: { kind: "string" },
    serves: { kind: "block[]", targets: ["requirement"] },
  },
  content: "paragraph+",
  name: "check",
  render: "host",
};

function checkBlock(serves: string[]): HTMLElement {
  const node = checkSchema.node("check", { blockId: "c1", run: "bun test web/src", serves }, [
    checkSchema.node("paragraph", undefined, [checkSchema.text("The tab lists every check.")]),
  ]);
  return DOMSerializer.renderSpec(document, renderTypedBlock(node, checkType)).dom as HTMLElement;
}

test("a block[] attribute draws the blocks it points at, each linked to its block", () => {
  const drawn = checkBlock([
    "dispatch://CORE-1/spec#b-r1",
    "dispatch://CORE-1/artifact/plan-md#b-t1",
  ]);

  const serves = drawn.querySelector('dd[data-proof-block-attribute="serves"]');
  expect(
    Array.from(serves?.querySelectorAll("a") ?? [], (link) => ({
      href: link.getAttribute("href"),
      // What the editor's link plugin (`@legion/proof-editor`'s `dispatch-link-open.ts`) and the
      // hover card read a drawn link's target from.
      reference: link.getAttribute("data-dispatch-href"),
      text: link.textContent,
    }))
  ).toEqual([
    {
      href: "/issues/CORE-1/spec#b-r1",
      reference: "dispatch://CORE-1/spec#b-r1",
      text: "CORE-1 spec block",
    },
    {
      href: "/issues/CORE-1/artifacts/plan-md#b-t1",
      reference: "dispatch://CORE-1/artifact/plan-md#b-t1",
      text: "CORE-1 plan-md block",
    },
  ]);
  // None of the JSON a reader used to see, and the pointers read as separate words.
  expect(serves?.textContent).toBe("CORE-1 spec block CORE-1 plan-md block");
  // Every other kind is drawn as it was: the check's `run` is still its own text.
  expect(drawn.querySelector('dd[data-proof-block-attribute="run"]')?.textContent).toBe(
    "bun test web/src"
  );
});

test("a block[] attribute pointing at no block is not drawn at all", () => {
  const drawn = checkBlock([]);

  expect(drawn.querySelector('[data-proof-block-attribute="serves"]')).toBeNull();
  expect(drawn.textContent).not.toContain("serves");
  expect(drawn.querySelector('dd[data-proof-block-attribute="run"]')?.textContent).toBe(
    "bun test web/src"
  );
});

/** A real editor holding one check, its typed blocks installed, whose `serves` link has taken the
 *  keyboard focus and been named by the hover card in its `aria-describedby`, as a Tab onto the
 *  link leaves it, with the check's element as the editor drew it before that change. `remove`
 *  takes the editor down. */
async function checkWithFocusedLink() {
  const mount = document.createElement("div");
  document.body.append(mount);
  const node = checkSchema.node(
    "check",
    { blockId: "c1", run: "bun test web/src", serves: ["dispatch://CORE-1/spec#b-r1"] },
    [checkSchema.node("paragraph", undefined, [checkSchema.text("The tab lists every check.")])]
  );
  const view = new EditorView(mount, {
    state: EditorState.create({ doc: checkSchema.node("doc", undefined, [node]) }),
  });
  const remove = () => {
    view.destroy();
    mount.remove();
  };
  installTypedBlocks(
    view,
    { types: [checkType], version: 1 },
    () => {},
    () => () => {}
  );
  const block = view.dom.querySelector<HTMLElement>('[data-proof-block-type="check"]');
  const link = block?.querySelector<HTMLElement>('dd[data-proof-block-attribute="serves"] a');
  if (block === null || link === null || link === undefined) {
    remove();
    throw new Error("expected the check and its serves link");
  }
  link.focus();
  link.setAttribute("aria-describedby", "ref-preview-card");
  // ProseMirror reads the page's changes in a MutationObserver's callback, a microtask later.
  await Promise.resolve();
  return { block, link, remove, view };
}

// INFRA-2160: a pointer's link in a typed block's header - a check's `serves`, a plan task's -
// takes the keyboard focus, and the hover card names itself in the link's `aria-describedby`.
// ProseMirror read that change in the block's drawing as an edit and drew the block anew, so the
// link holding the focus left the page and the focus went with it: a Tab into the Plan tab's
// document went back to the top of the page.
test("a change the page makes in a typed block's header leaves the block and its focused link in place", async () => {
  const { block, link, remove, view } = await checkWithFocusedLink();
  try {
    expect(view.dom.querySelector('[data-proof-block-type="check"]')).toBe(block);
    expect(link.isConnected).toBe(true);
    expect(document.activeElement).toBe(link);
  } finally {
    remove();
  }
});

// The header's changes are the only ones the block's view keeps from ProseMirror: a line the
// browser draws in the block's body - as a paste or a phone keyboard's Enter draws one - is still
// read into the document. A change to the text of a line already there is no test of the view:
// ProseMirror asks that line's own text, not the block's view, whether to read it.
test("a line drawn in a typed block's body is read into the document", async () => {
  const { block, remove, view } = await checkWithFocusedLink();
  try {
    const body = block.querySelector("[data-proof-block-content]");
    if (body === null) throw new Error("expected the check's body");
    const line = document.createElement("p");
    line.textContent = "And its run.";
    body.append(line);
    await Promise.resolve();

    expect(view.state.doc.textContent).toBe("The tab lists every check.And its run.");
  } finally {
    remove();
  }
});
