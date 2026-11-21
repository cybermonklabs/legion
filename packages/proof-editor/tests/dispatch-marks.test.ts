/**
 * Two readers' asks may cover the same text: the `dispatchAsk` schema declares `excludes: ''`, so
 * a second ask over part of the first adds its own mark instead of cutting the first out of the
 * overlap, removing one ask removes that mark's instance, never every ask on its text, and an ask
 * span nested in another reads back from markdown as both, in the document main's parser gave.
 */

import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { EditorState } from "@milkdown/kit/prose/state";
import { createAskMark, findAskMarkRange, removeAskMark } from "../src/dispatch-marks.js";
import { createHeadlessProof, milkdownMarkOpens } from "../src/lib-headless.js";
import { markedText } from "./mark-text";
import { viewDouble } from "./view-double";

async function twoAsks() {
  const { schema } = await createHeadlessProof();
  const doc = schema.node("doc", null, [
    schema.node("paragraph", null, [schema.text("The quick brown fox")]),
  ]);
  const { view, transactions } = viewDouble(EditorState.create({ doc, schema }));
  // "The quick brown fox": "quick brown" is 5..16, "brown" 11..16 (the paragraph opens at 0).
  const bob = createAskMark(view, { from: 5, to: 16 }, "user:bob");
  const alice = createAskMark(view, { from: 11, to: 16 }, "user:alice");
  if (bob === null || alice === null)
    throw new Error("createAskMark refused a plain paragraph range");
  return { view, transactions, bob: bob.id, alice: alice.id };
}

test("an ask over part of another ask leaves the first whole", async () => {
  const { view, transactions, bob, alice } = await twoAsks();
  const doc = view.state.doc;
  expect(markedText(doc, bob)).toBe("quick brown");
  expect(markedText(doc, alice)).toBe("brown");
  expect(findAskMarkRange(doc, bob)).toEqual({ from: 5, to: 16 });
  expect(findAskMarkRange(doc, alice)).toEqual({ from: 11, to: 16 });
  const second = transactions[1];
  if (second === undefined) throw new Error("the second createAskMark dispatched nothing");
  expect(second.steps.map((step) => step.toJSON().stepType)).toEqual(["addMark"]);
});

test("removing one of two overlapping asks leaves the other whole", async () => {
  for (const [removed, kept, keptText] of [
    ["alice", "bob", "quick brown"],
    ["bob", "alice", "brown"],
  ] as const) {
    const asks = await twoAsks();
    expect(removeAskMark(asks.view, asks[removed])).toBe(true);
    const doc = asks.view.state.doc;
    expect({ removed, text: markedText(doc, asks[removed]) }).toEqual({ removed, text: "" });
    expect({ kept, text: markedText(doc, asks[kept]) }).toEqual({ kept, text: keptText });
    expect(doc.textContent).toBe("The quick brown fox");
  }
});

test("removing an ask that is not in the document reports false and dispatches nothing", async () => {
  const { view, transactions } = await twoAsks();
  expect(removeAskMark(view, "never")).toBe(false);
  expect(transactions).toHaveLength(2);
});

test("an ask span nested inside another reads back from markdown as both asks", async () => {
  const { parseMarkdown, serializeMarkdown } = await createHeadlessProof();
  const ask = (id: string, by: string, text: string) =>
    `<span data-dispatch="ask" data-id="${id}" data-by="${by}">${text}</span>`;
  const markdown = `The ${ask("a1", "user:bob", `quick ${ask("a2", "user:alice", "brown")} fox`)} jumps.\n`;
  const doc = parseMarkdown(markdown);
  expect({ a1: markedText(doc, "a1"), a2: markedText(doc, "a2") }).toEqual({
    a1: "quick brown fox",
    a2: "brown",
  });
  const again = parseMarkdown(serializeMarkdown(doc));
  expect({ a1: markedText(again, "a1"), a2: markedText(again, "a2") }).toEqual({
    a1: "quick brown fox",
    a2: "brown",
  });
});

/** The opening tag of reader `id`'s ask. */
const ask = (id: string) => `<span data-dispatch="ask" data-id="${id}" data-by="user:${id}">`;
/** Markdown of `lines`, each a block of its own. */
const blocks = (...lines: string[]) => `${lines.join("\n\n")}\n`;
/** A document of `paragraphs`, each the JSON main printed for it. */
const docJson = (...paragraphs: string[]) => `{"type":"doc","content":[${paragraphs.join(",")}]}`;

// Asks nested inline and across blocks. An ask's opening and closing tags each on a line of their
// own are blocks of HTML, which `normalizeSplitDispatchSpans` pairs into one ask holding whole
// paragraphs, and Milkdown's parser drops every open mark each time a paragraph closes. Each
// expected document is main's: the same markdown parsed by `createHeadlessProof()` at 8a9540682d
// (main before #4437), in both modes, and printed as `JSON.stringify(doc.toJSON())` with each
// random `blockId` written as "<id>", one paragraph a line here. The runner #4437 had at
// 1c711353dc gave `after` in the first row no mark, and `b2` and `a2` in the third none. The last
// two rows put an empty code block and an image after an inner ask, neither of which holds a mark
// in the document. Their documents are main's runner's: 8a9540682d's `parseMarkdown.runner` put in
// this tree's src/dispatch-marks.ts, which gives each of the seven documents above byte for byte,
// printed the same way.
const nestedAsks: (readonly [string, string, string])[] = [
  [
    "the block-level shape A{P(first), B{P(inner)}, P(after)}",
    blocks(ask("a"), "first", ask("b"), "inner", "</span>", "after", "</span>"),
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"first"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"inner"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"after"}]}'
    ),
  ],
  [
    "an ask nested inline in another",
    `The ${ask("a")}quick ${ask("b")}brown</span> fox</span> jumps.\n`,
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","text":"The "},{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"quick "},{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}},{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"brown"},{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":" fox"},{"type":"text","text":" jumps."}]}'
    ),
  ],
  [
    "two block asks nested in a third, A{P(a1), B{P(b1), C{P(c1)}, P(b2)}, P(a2)}",
    blocks(
      ask("a"),
      "a1",
      ask("b"),
      "b1",
      ask("c"),
      "c1",
      "</span>",
      "b2",
      "</span>",
      "a2",
      "</span>"
    ),
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"b1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"c","by":"user:c"}}],"text":"c1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}},{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"b2"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a2"}]}'
    ),
  ],
  [
    "an ask closed right after its child ask, A{P(first), B{P(inner)}} then P(outside)",
    blocks(ask("a"), "first", ask("b"), "inner", "</span>", "</span>", "outside"),
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"first"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"inner"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","text":"outside"}]}'
    ),
  ],
  [
    "an inline ask in a block ask's later paragraph, A{P(first), P(B{x} y)}",
    blocks(ask("a"), "first", `${ask("b")}x</span> y`, "</span>"),
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"first"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"x"},{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":" y"}]}'
    ),
  ],
  [
    "two block asks side by side in a third, A{P(a1), B{P(b1)}, C{P(c1)}, P(a2)}",
    blocks(ask("a"), "a1", ask("b"), "b1", "</span>", ask("c"), "c1", "</span>", "a2", "</span>"),
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"b1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}},{"type":"dispatchAsk","attrs":{"id":"c","by":"user:c"}}],"text":"c1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a2"}]}'
    ),
  ],
  [
    "the same ask nested in itself, A{P(a1), A{P(a2)}, P(a3)}",
    blocks(ask("a"), "a1", ask("a"), "a2", "</span>", "a3", "</span>"),
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a2"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a3"}]}'
    ),
  ],
  [
    "an empty code block after an inner ask, A{P(a1), B{P(b1)}, CODE, P(a2)}",
    blocks(ask("a"), "a1", ask("b"), "b1", "</span>", "```\n```", "a2", "</span>"),
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"b1"}]}',
      '{"type":"code_block","attrs":{"language":null,"blockId":"<id>"}}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","text":"a2"}]}'
    ),
  ],
  [
    "an image opening the paragraph after an inner ask, A{P(a1), B{P(b1)}, P(![i](u) a2)}",
    blocks(ask("a"), "a1", ask("b"), "b1", "</span>", "![i](u) a2", "</span>"),
    docJson(
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":"a1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"b","by":"user:b"}}],"text":"b1"}]}',
      '{"type":"paragraph","attrs":{"blockId":"<id>"},"content":[{"type":"image","attrs":{"src":"u","alt":"i","title":null}},{"type":"text","marks":[{"type":"dispatchAsk","attrs":{"id":"a","by":"user:a"}}],"text":" a2"}]}'
    ),
  ],
];

for (const [shape, markdown, main] of nestedAsks) {
  test(`${shape} parses to main's document, byte for byte`, async () => {
    for (const readOnly of [false, true]) {
      const { parseMarkdown } = await createHeadlessProof({ readOnly });
      const parsed = JSON.stringify(parseMarkdown(markdown).toJSON(), (key, value) =>
        key === "blockId" ? "<id>" : value
      );
      expect({ readOnly, parsed }).toEqual({ readOnly, parsed: main });
    }
  });
}

/** How many ask marks the nodes of `doc` hold, together. */
function askMarksOf(doc: ProseMirrorNode): number {
  let count = 0;
  doc.descendants((node) => {
    count += node.marks.filter((mark) => mark.type.name === "dispatchAsk").length;
  });
  return count;
}

/** Block asks nested `depth` deep, A{P(x1), B{P(x2), ... Z{P(xD)} ..., AFTER}, AFTER}, with the
 *  block `after` builds after each inner ask, at the depth of the ask that holds it. */
function nestedBlockAsks(depth: number, after: (level: number) => string): string {
  const lines: string[] = [];
  for (let level = 1; level <= depth; level++) lines.push(ask(`a${level}`), `x${level}`);
  for (let level = depth; level >= 1; level--) {
    lines.push("</span>");
    if (level > 1) lines.push(after(level - 1));
  }
  return blocks(...lines);
}

// Block asks nested D deep with a block after each inner ask: each inner ask ends after its
// paragraph closed, which dropped the marks of the asks around it. A paragraph after it holds the
// marks of every ask around it, so the document holds D(D+1)/2 ask marks, as main's does, and a
// runner that opens a mark for each of them opens about D²/2 for a body of length D, in proportion
// to the document. A block that holds no mark (an empty code block), or a paragraph of an image,
// which holds none in any document the engine returns, leaves the document D ask marks, and the
// runner that opened every enclosing ask again at each such end opened about D²/2 for those too.
// The runner opens a mark only where a node then holds it (`openAsksOf`, src/dispatch-marks.ts),
// so its opens, counted (`milkdownMarkOpens`), are at most one for each ask and one for each ask
// mark the document holds, whatever stands after each inner ask.
const depth = 100;
const afterEachInnerAsk: (readonly [string, (level: number) => string, number])[] = [
  ["a paragraph", (level) => `y${level}`, (depth * (depth + 1)) / 2],
  ["an empty code block", () => "```\n```", depth],
  ["a paragraph of an image", () => "![i](u)", depth],
];
for (const [after, block, held] of afterEachInnerAsk) {
  test(`block asks nested ${depth} deep with ${after} after each inner ask open no more marks than the asks and the document hold`, async () => {
    const markdown = nestedBlockAsks(depth, block);
    for (const readOnly of [false, true]) {
      const { parseMarkdown } = await createHeadlessProof({ readOnly });
      const before = milkdownMarkOpens();
      const doc = parseMarkdown(markdown);
      const opened = milkdownMarkOpens() - before;
      const marks = askMarksOf(doc);
      expect({ readOnly, marks }).toEqual({ readOnly, marks: held });
      expect(
        opened,
        `${opened} marks opened for ${depth} asks and the ${marks} ask marks the document holds (readOnly ${readOnly})`
      ).toBeLessThanOrEqual(depth + marks);
    }
  });
}
