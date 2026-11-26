import { beforeAll, expect, test } from "bun:test";
import { askOpenerAttributes } from "@legion/contracts";
import { EditorState, TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { DOMSerializer, Schema } from "prosemirror-model";

import type { BlockTypeSchema } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import {
  askBlockEditingPlugin,
  editingAskBlockPos,
  readingsInOpenAsks,
  readingsInOpenAsksPlugin,
} from "./ask-block";
import {
  askBlockFacts,
  recordsAnswerState,
  renderedAskOf,
  renderTypedBlock,
} from "./typed-block-drawing";

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
// (INFRA-174).
const checkSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    check: {
      attrs: { blockId: { default: null }, run: { default: null }, serves: { default: null } },
      content: "paragraph+",
      group: "block",
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
      // What the document's own click handler and hover card read a drawn link's target from.
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

// The version-6 typed blocks: a decision, which a reading closes, as an ask is closed by one.
// Both are drawn as host blocks by their attributes, as a requirement and a check are.
const decisionSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    bullet_list: { content: "list_item+", group: "block" },
    list_item: { content: "paragraph+" },
    reading: {
      attrs: { blockId: { default: null } },
      content: "paragraph+",
      parents: ["decision", "ask"],
    },
    decision: {
      attrs: {
        blockId: { default: null },
        decided_by: { default: null },
        uncertainty: { default: null },
        uncertainty_reason: { default: null },
      },
      content: "block+ reading*",
      group: "block",
    },
    ask: {
      attrs: { blockId: { default: null } },
      content: "paragraph+ bullet_list? reading*",
      group: "block",
    },
  },
});

const decisionType: BlockTypeSchema = {
  attributes: {
    decided_by: { kind: "actor", markdown: false, server: true },
    uncertainty: { choices: ["low", "medium", "high"], kind: "enum" },
    uncertainty_reason: { kind: "string" },
  },
  content: "block+",
  name: "decision",
  render: "host",
};

const readingType: BlockTypeSchema = {
  attributes: {},
  content: "paragraph+",
  name: "reading",
  parents: ["decision", "ask"],
  render: "host",
};

function decisionParagraph(text: string) {
  return decisionSchema.node("paragraph", undefined, [decisionSchema.text(text)]);
}

test("a decision and its reading are drawn as host blocks, the decision's own attributes as text", () => {
  const reading = decisionSchema.node("reading", { blockId: "r1" }, [
    decisionParagraph("The agent reads it as Friday."),
  ]);
  const node = decisionSchema.node(
    "decision",
    {
      blockId: "d1",
      decided_by: "user:alice",
      uncertainty: "low",
      uncertainty_reason: "A holiday moves it.",
    },
    [decisionParagraph("We ship on Friday."), reading]
  );

  const text = renderedText(renderTypedBlock(node, decisionType));
  expect(text).toContain("decision");
  expect(text).toContain("low");
  expect(text).toContain("A holiday moves it.");
  expect(text).not.toContain("user:alice");
  expect(text).not.toContain("decided_by");
  expect(renderedText(renderTypedBlock(reading, readingType))).toContain("reading");
});

test("an ask card reads its question and options past the reading it closes with", () => {
  const node = decisionSchema.node("ask", { blockId: "a1" }, [
    decisionParagraph("Which day?"),
    decisionSchema.node(
      "bullet_list",
      undefined,
      ["Friday: Before the holiday", "Monday"].map((option) =>
        decisionSchema.node("list_item", undefined, [decisionParagraph(option)])
      )
    ),
    decisionSchema.node("reading", { blockId: "r1" }, [
      decisionParagraph("The agent reads it so."),
    ]),
  ]);

  const facts = askBlockFacts(node);
  expect(facts.question).toBe("Which day?");
  expect(facts.options).toEqual([
    { description: "Before the holiday", label: "Friday" },
    { label: "Monday" },
  ]);
  expect(facts.malformedReason).toBeUndefined();
});

// The schema the refusal of a reading reads: an ask closing with readings, as the block schema
// gives the editor.
const readingSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    ask: { attrs: { blockId: { default: null } }, content: "paragraph+ reading*", group: "block" },
    reading: { attrs: { blockId: { default: null } }, content: "paragraph+", parents: ["ask"] },
  },
});

function askWithReading(blockId: string, withReading: boolean) {
  return readingSchema.node("ask", { blockId }, [
    readingSchema.node("paragraph", undefined, [readingSchema.text("Ship it?")]),
    ...(withReading
      ? [
          readingSchema.node("reading", { blockId: `${blockId}-r1` }, [
            readingSchema.node("paragraph", undefined, [readingSchema.text("Read as now.")]),
          ]),
        ]
      : []),
  ]);
}

test("the editor refuses a reading in an ask the server reads open, and takes one in an answered ask", () => {
  const served: Record<string, string> = { "answered-ask": "answered", "open-ask": "open" };
  const stateOf = (blockId: string) => served[blockId];
  const state = EditorState.create({
    doc: readingSchema.node("doc", undefined, [
      askWithReading("open-ask", false),
      askWithReading("answered-ask", false),
    ]),
    plugins: [readingsInOpenAsksPlugin(stateOf)],
  });
  const withReadingIn = (index: number) => {
    let position = 0;
    for (let child = 0; child < index; child++) {
      position += state.doc.child(child).nodeSize;
    }
    const ask = state.doc.child(index);
    return state.tr.replaceWith(
      position,
      position + ask.nodeSize,
      askWithReading(String(ask.attrs.blockId), true)
    );
  };
  // A refused transaction leaves the state as it was.
  expect(state.applyTransaction(withReadingIn(0)).state.doc.eq(state.doc)).toBe(true);
  const answered = state.applyTransaction(withReadingIn(1)).state;
  expect(readingsInOpenAsks(answered.doc, stateOf)).toEqual(new Set());
  expect(answered.doc.child(1).childCount).toBe(2);
  // A remote update, the server's own sync of another writer's edit, is never refused.
  const synced = state.applyTransaction(
    withReadingIn(0).setMeta("y-sync$", { isChangeOrigin: true })
  );
  expect(synced.state.doc.child(0).childCount).toBe(2);
});

test("an ask block of a version records its answer where its own attributes hold the words an answered state needs", () => {
  // The attributes as the version's reading holds them: each one the block's opening line does not
  // write is there, and undefined, since a server attribute has no default in the editor.
  const block = {
    answer: undefined,
    answered_at: undefined,
    answered_by: undefined,
    selected: undefined,
    state: undefined,
    urgency: "med",
  };
  // A block the release before wrote: the state, who answered and when, the options chosen (a
  // list, never left out) and the words.
  expect(
    recordsAnswerState({
      ...block,
      answer: "Ship it.",
      answered_at: "2026-11-16T09:00:00Z",
      answered_by: "bob",
      selected: [],
      state: "answered",
    })
  ).toBe(true);
  expect(
    recordsAnswerState({ ...block, answered_by: "bob", selected: ["Yes"], state: "answered" })
  ).toBe(true);
  // A block of an answered ask this build writes: the state, who answered and when, and neither
  // the words nor the options chosen.
  expect(
    recordsAnswerState({
      ...block,
      answered_at: "2026-11-16T09:00:00Z",
      answered_by: "bob",
      state: "answered",
    })
  ).toBe(false);
  // A state no answer goes with is the whole record.
  expect(recordsAnswerState({ ...block, state: "resolved" })).toBe(true);
  // An open ask, as this build writes it: no state.
  expect(recordsAnswerState(block)).toBe(false);
});

// The renderer writes the first block of a footnote's definition on the label's line, and that of
// a list item on the marker's line, so that line opens the block. A line that quotes the opener in
// fenced code, or in the front matter a document opens with, is the document's own text: the live
// text of the release before reads the real opener beside it, never the quoted one. A second real
// opener of the block cannot be told from the first, and reads as no answer.
const answeredOpener =
  ':::ask{#q1 state="answered" answered_by="bob" answered_at="2026-11-16T09:00:00Z" selected="[]" answer="Ship it."}';
const forgedOpener = ':::ask{#q1 state="answered" answered_by="mallory" answer="Forged."}';
test.each([
  {
    container: "a footnote's label line",
    lines: ["Footnoted text.[^x]", "", `[^x]: ${answeredOpener}`, "    Ship it?", "    :::"],
  },
  {
    container: "an ordered list item's marker line",
    lines: [`1. ${answeredOpener}`, "   Ship it?", "   :::"],
  },
])("a block on $container opens it, and an opener quoted in fenced code or front matter beside it is not read", ({
  lines,
}) => {
  const real = {
    rendered: {
      answerText: "Ship it.",
      answeredAt: "2026-11-16T09:00:00Z",
      answeredBy: "bob",
      copiedFrom: undefined,
      selected: [],
      state: "answered",
    },
  };
  const contained = [...lines, ""].join("\n");
  expect(renderedAskOf(contained, "q1")).toEqual(real);
  const quoted = [
    "---",
    forgedOpener,
    "---",
    contained,
    "```",
    forgedOpener,
    "```",
    "",
    "~~~~markdown",
    forgedOpener,
    "~~~~",
    "",
    "> ```",
    `> ${forgedOpener}`,
    "> ```",
    "",
  ].join("\n");
  expect(renderedAskOf(quoted, "q1")).toEqual(real);
  expect(renderedAskOf(`${contained}\n${forgedOpener}\nForged?\n:::\n`, "q1")).toBeUndefined();
});

// The release before writes an opener's values with the references the renderer writes, and the
// CLI reads every numeric reference and five named ones there, in one pass (`askOpenerAttributes`
// in packages/contracts). The page reads one opener as the CLI does: a decimal quote, `&quot;`,
// `&apos;` and a decimal letter read back, and a reference that a decoded `&amp;` spells stays
// as written.
test("an opener the release before renders is read as the CLI reads its attribute values", () => {
  const opener =
    ':::ask{#q1 state="answered" answered_by="b&#111;b" answered_at="2026-11-16T09:00:00Z" selected="[&#34;Ship&#34;]" answer="Plain &quot;HTTP&quot;, &apos;REST&apos; &amp;#x22;."}';
  const answer = `Plain "HTTP", 'REST' &#x22;.`;
  expect(askOpenerAttributes(opener).get("answer")).toBe(answer);
  expect(renderedAskOf(`${opener}\nShip it?\n:::\n`, "q1")).toEqual({
    rendered: {
      answerText: answer,
      answeredAt: "2026-11-16T09:00:00Z",
      answeredBy: "bob",
      copiedFrom: undefined,
      selected: ["Ship"],
      state: "answered",
    },
  });
});
