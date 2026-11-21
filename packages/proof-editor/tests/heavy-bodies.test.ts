/**
 * The bodies that froze a reader's browser tab (sjawhar/legion#2519): a long list, a list or a
 * quote followed by lines carrying no marker of their own, a run of brackets, and lists nested
 * deeply. Each parses here in time a reader can wait through, and the blocks of each nest no
 * deeper than the document service accepts, so a body the service stores reads the same in the
 * browser and a body it refuses reads as the text it is instead of freezing or overflowing the
 * stack.
 *
 * Three things are asserted:
 *
 * 1. What each shape parses to, and what it costs. Every shape, at the longest body a caller may
 *    write, parses in the read-only engine and in the default mode to the text and the nesting
 *    its row names, in a number of steps in proportion to its length: the steps the parse counts
 *    itself and the steps each stage no patch reaches takes through a counting Proxy
 *    (tests/parse-work.ts) - micromark's tokenizer, which every construct runs through, and the
 *    scans that read events at most `tokenizerStepsPerCharacter` and `scanStepsPerCharacter` a
 *    character of the body, each count moved at least as far as the body says it must, and each
 *    count, the resolvers', subtokenize's, the extensions', the compile's, each transform's and
 *    Milkdown's runner calls among them, grown no faster than the shape's growing dimension from
 *    an eighth of it to the full size. The one shape the default mode refuses is counted the same
 *    way and then asked for its refusal at both sizes. Which dependency patch counts what, and
 *    how to put a patch's scan back to see a row catch it, is in tests/parse-work.ts, "The
 *    dependency patches".
 * 2. The nesting bound, at the boundaries the document service draws them at
 *    (`maxNesting`, packages/envoy/internal/dispatch/pmdoc/nesting.go: a quote, a list, a list
 *    item, a typed block and a footnote definition each count one). A list opener opens a list and
 *    its first item, so it takes two of the hundred; a quote takes one; a `:::` opener takes one.
 *    Every line past the bound is kept as the text it is, never dropped.
 * 3. That the bound reaches a bare `unified().use(remarkParse)` processor, which is the path
 *    Milkdown and the headless engine both parse through. The bound is a rewrite of the `list` and
 *    `blockQuote` constructs `micromark-core-commonmark` exports, so a second copy of that package
 *    in the tree would leave one of them unbounded, and this test is what says so.
 */

import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import remarkParse from "remark-parse";
import { unified } from "unified";
import { createHeadlessProof, type HeadlessProofEditor } from "../src/lib-headless.js";
import { serverBlockSchema } from "./block-schema-fixture";
import {
  countParseStages,
  parsedWithinWorkBounds,
  refusalWithinWorkBounds,
  type WorkShape,
} from "./parse-work";

/** The longest comment, message or broadcast body a caller may write, in characters. */
const bodyCap = 32_000;

/**
 * How many blocks a document may open inside one another: `maxNesting` in
 * packages/envoy/internal/dispatch/pmdoc/nesting.go, which refuses a write that opens one more.
 */
const maxBlockNesting = 100;

/** How deeply list markers alone may nest: each opens a list and its first item. */
const maxListNesting = maxBlockNesting / 2;

/** How deeply quotes alone may nest: each opens one block. */
const maxQuoteNesting = maxBlockNesting;

/** How deeply a quote and a list alternating may nest: the two open three blocks a level. */
const maxAlternatingNesting = Math.floor(maxBlockNesting / 3);

/** `depth` list items nested by indentation, each line opening a list and its item. */
function nestedItems(depth: number): string {
  const lines: string[] = [];
  for (let level = 0; level < depth; level++) lines.push(`${"  ".repeat(level)}- a`);
  return `${lines.join("\n")}\n`;
}

/** The two nested shapes, whose blank lines every open block above them reads. */
const nest49 = nestedItems(maxListNesting - 1);
const nest100 = nestedItems(maxBlockNesting);

/**
 * A shape, as a reader is sent it and as a row grows it: `WorkShape` of tests/parse-work.ts, with
 * what the row reads on the parse. `grow` builds the shape at a size of the dimension whose growth
 * costs; the nesting of the two nested shapes is held at its depth while their blank lines shrink,
 * because what costs there is the blank lines times the blocks open above them, and shrinking the
 * whole body instead drops the blank lines and measures another shape.
 */
type HeavyShape = WorkShape & {
  /** What a parse of the full body produces, in a mode that reads it. */
  expectDoc: (doc: ProseMirrorNode) => void;
  /** What the default mode refuses the body with at either size, for the shape it refuses. */
  refusedInDefaultMode?: RegExp;
  /** What the read-only engine refuses the body with at either size, for the shape it refuses. */
  refusedInReadOnlyMode?: RegExp;
};

/** The typed fence's opener, which its closers stop short by, so the body is twice the brackets. */
const calloutLabel = ":::callout[";

/** The opening tag of reader `id`'s ask, as an ask is written into a document's markdown. */
function askOpen(id: number): string {
  return `<span data-dispatch="ask" data-id="a${id}" data-by="reader">`;
}

/** How many marks of type `type` the inline nodes of `node` carry, together. */
function marksOf(node: ProseMirrorNode, type: string): number {
  let count = 0;
  node.descendants((child) => {
    count += child.marks.filter((mark) => mark.type.name === type).length;
  });
  return count;
}

/** How many nodes of type `type` `node` holds. */
function nodesOf(node: ProseMirrorNode, type: string): number {
  let count = 0;
  node.descendants((child) => {
    if (child.type.name === type) count++;
  });
  return count;
}

/** The shapes, each at the body cap, as a reader is sent them. */
const shapes: HeavyShape[] = [
  {
    name: "a plain list of 8,000 items",
    grow: (size) => "- x\n".repeat(size),
    full: 8_000,
    opensContainers: true,
    expectDoc: (doc) => {
      expect(doc.textContent.length).toBe(8_000);
      expect(depthOf(doc, "bullet_list")).toBe(1);
    },
  },
  {
    name: "a quote then lines carrying no marker",
    grow: (size) => `> x\n${"x\n".repeat(size)}`,
    full: 15_998,
    lazyLines: (size) => size,
    opensContainers: true,
    expectDoc: (doc) => {
      expect(doc.textContent.length).toBe(31_997);
      expect(depthOf(doc, "blockquote")).toBe(1);
    },
  },
  {
    name: "a list item then lines carrying no marker",
    grow: (size) => `- x\n${"x\n".repeat(size)}`,
    full: 15_998,
    lazyLines: (size) => size,
    opensContainers: true,
    expectDoc: (doc) => {
      expect(doc.textContent.length).toBe(31_997);
      expect(depthOf(doc, "bullet_list")).toBe(1);
    },
  },
  {
    name: "a run of 15,000 brackets and as many closers",
    grow: (size) => "[".repeat(size) + "]".repeat(size),
    full: 15_000,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(doc.textContent.length).toBe(30_000);
    },
  },
  {
    name: "49 items nested by indentation then blank lines",
    grow: (size) => nest49 + "\n".repeat(size),
    full: bodyCap - nest49.length,
    opensContainers: true,
    expectDoc: (doc) => {
      expect(doc.textContent).toBe("a".repeat(maxListNesting - 1));
      expect(depthOf(doc, "bullet_list")).toBe(maxListNesting - 1);
    },
  },
  {
    name: "100 items nested by indentation then blank lines",
    grow: (size) => nest100 + "\n".repeat(size),
    full: bodyCap - nest100.length,
    opensContainers: true,
    expectDoc: (doc) => {
      // The 50 items past the bound are the text of the innermost item the bound allows.
      expect(doc.textContent.length).toBe(250);
      expect(depthOf(doc, "bullet_list")).toBe(maxListNesting);
    },
  },
  {
    name: "a chain of 15,999 list markers on one line",
    grow: (size) => `${"- ".repeat(size)}x`,
    full: 15_999,
    opensContainers: true,
    expectDoc: (doc) => {
      expect(doc.textContent.length).toBe(31_899);
      expect(depthOf(doc, "bullet_list")).toBe(maxListNesting);
    },
  },
  {
    name: "a typed fence whose label opens a run of brackets",
    grow: (size) => `${calloutLabel}${"[".repeat(size)}${"]".repeat(size - calloutLabel.length)}`,
    full: 15_000,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(doc.textContent.length).toBe(30_000);
    },
    refusedInDefaultMode: /line 1\b/,
  },
  {
    // Every line ends in spaces: two before a line ending (a hard break), one before the next
    // (a line suffix). micromark's text resolver puts a token in for each, so a resolver that
    // inserts each with a splice of its own shifts the rest of the paragraph once a line.
    name: "a paragraph of 9,000 lines each ending in a hard break or a line suffix",
    grow: (size) => "x  \ny \n".repeat(size),
    full: 4_500,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(doc.childCount).toBe(1);
      let breaks = 0;
      doc.descendants((node) => {
        if (node.type.name === "hardbreak") breaks++;
      });
      expect(breaks).toBe(4_500);
    },
  },
  {
    // The proof marks' own walk (`remarkProofMarks`, `proof-sdk-upstream`'s
    // src/formats/remark-proof-marks.ts) moves each child inside an open `<span data-proof>` into
    // it as `remarkDispatchMarks` does, which was ported from it; unpatched, its splice of each
    // moved child read 47.59 times the steps for 8 times the spans (1,053 at 7, 50,113 at 62,
    // 2026-11-19), and `patches/proof-sdk@…tgz.patch` makes it one pass.
    name: "a paragraph of 500 proof spans side by side",
    grow: (size) =>
      Array.from(
        { length: size },
        (_, id) => `<span data-proof="authored" data-id="p${id}" data-by="human:reader">w</span>`
      ).join(" "),
    full: 500,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(doc.childCount).toBe(1);
      expect(doc.textContent).toBe(Array.from({ length: 500 }, () => "w").join(" "));
    },
  },
  {
    // Every run is a child the same walk moves into the one span, and each `<em>` is read for the
    // `</em>` that closes it (`convertHtmlFormatting`); unpatched, the walk read 52.08 times the
    // steps for 8 times the runs (1,881 at 7, 97,966 at 62, 2026-11-19).
    name: "a proof span holding 500 runs in emphasis tags",
    grow: (size) =>
      `<span data-proof="authored" data-id="p" data-by="human:reader">${"<em>w</em> ".repeat(size)}</span>`,
    full: 500,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(doc.childCount).toBe(1);
    },
  },
  {
    // Each `<em>` is a tag nothing closes, so `convertHtmlFormatting` finds no `</em>` for it: a
    // search forward for the closer read every child after the tag, about the square of the
    // span's children; `patches/proof-sdk@…tgz.patch` finds each tag's closer in one pass from the
    // end (`nextClosingTags`), with the same matches.
    name: "a proof span holding 1,000 emphasis tags that nothing closes",
    grow: (size) =>
      `<span data-proof="authored" data-id="p" data-by="human:reader">${"<em>w ".repeat(size)}</span>`,
    full: 1_000,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(doc.childCount).toBe(1);
      expect(nodesOf(doc, "html")).toBe(1_000);
    },
  },
  {
    // Each ask's opening tag, its text and its closing tag are three children of the paragraph
    // (`remarkDispatchMarks`'s `normalizeSplitDispatchSpans`), so a walk that splices each child
    // it moves into an ask out of the paragraph shifts the rest of it twice an ask.
    name: "a paragraph of 500 asks side by side",
    grow: (size) => Array.from({ length: size }, (_, id) => `${askOpen(id)}w</span>`).join(" "),
    full: 500,
    opensContainers: false,
    asks: (size) => size,
    expectDoc: (doc) => {
      expect(doc.childCount).toBe(1);
      expect(marksOf(doc, "dispatchAsk")).toBe(500);
    },
  },
  {
    // Every ask lies inside the one before it, so a runner that closed the asks around an ask and
    // opened each of them again would open about the square of the depth.
    name: "an ask nested 400 asks deep",
    grow: (size) =>
      `${Array.from({ length: size }, (_, id) => askOpen(id)).join("")}w${"</span>".repeat(size)}`,
    full: 400,
    opensContainers: false,
    asks: (size) => size,
    expectDoc: (doc) => {
      expect(doc.textContent).toBe("w");
      expect(marksOf(doc, "dispatchAsk")).toBe(400);
    },
  },
  {
    // Each block of HTML holds a whole ask and text after it, which `remarkDispatchMarks`'s
    // `visit` reads as two inline nodes in the block's place, so a walk that splices them in
    // shifts every block after it. Inline nodes cannot stand at the document's top, so both modes
    // refuse the body once the transforms have run.
    name: "450 blocks of HTML, each an ask then text",
    grow: (size) =>
      Array.from({ length: size }, (_, id) => `${askOpen(id)}\nw\n</span> t`).join("\n\n"),
    full: 450,
    opensContainers: false,
    htmlBlocks: (size) => size,
    expectDoc: () => {
      throw new Error("both modes refuse this body; no row reads its document");
    },
    refusedInDefaultMode: /Cannot create node for doc/,
    refusedInReadOnlyMode: /Cannot create node for doc/,
  },
  {
    // Each typed block is a `:::` container, whose compile handlers are `mdast-util-directive`'s,
    // counted by Proxy as GFM's are (`COUNTED_EXTENSIONS`, tests/parse-work.ts): a handler that
    // walked the blocks built before its own, or the tree they stand in, would read about the
    // square of the blocks. tests/nested-directive.test.ts's bodies open at most 32 containers,
    // which bounds how often a handler runs there. The container's resolver reads about 127
    // directive-scan steps a block, so each block holds a line of text, as a reader writes one:
    // 64 characters a block, 1.98 directive-scan steps a character (2026-11-21).
    name: "500 typed blocks side by side, each a line of text",
    grow: (size) =>
      ":::callout{}\nA line of text that a typed block holds, too.\n:::\n\n".repeat(size),
    full: 500,
    opensContainers: true,
    opensDirectives: true,
    expectDoc: (doc) => {
      expect(doc.childCount).toBe(500);
      expect(nodesOf(doc, "callout")).toBe(500);
    },
  },
  {
    // Every image is the label of a link, a mark no inline leaf may carry, so `withoutLeafMarks`
    // changes each: a walk that copied a node's earlier children again for each child it changed
    // would copy about the square of the paragraph.
    name: "a paragraph of 2,000 linked images",
    grow: (size) => "[![i](u)](l) ".repeat(size),
    full: 2_000,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(nodesOf(doc, "image")).toBe(2_000);
      let marked = 0;
      doc.descendants((node) => {
        if (node.type.name === "image" && node.marks.length > 0) marked++;
      });
      expect(marked).toBe(0);
    },
  },
  {
    // Every image is inside a strong run: micromark's attention resolver matches each closing
    // `**` with its opener, and spliced each match into the paragraph's whole list of events,
    // shifting every event after it (61.76 times the steps for 8 times the runs, 1,730,040 at 312
    // and 106,852,500 at 2,500, 2026-11-19); `patches/micromark-core-commonmark@2.0.3.patch` puts
    // each match in place once. The strong mark is also one no inline leaf may carry, so
    // `withoutLeafMarks` changes each image, as the linked images above.
    name: "a paragraph of 2,500 bold images",
    grow: (size) => "**![i](u)** ".repeat(size),
    full: 2_500,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(nodesOf(doc, "image")).toBe(2_500);
      let marked = 0;
      doc.descendants((node) => {
        if (node.type.name === "image" && node.marks.length > 0) marked++;
      });
      expect(marked).toBe(0);
    },
  },
  {
    // Every `*` can close and none can open, so the attention resolver finds no opener for any of
    // them: a search that read every event before each closer read about the square of the
    // paragraph; it now reads the openers still open, from the lowest a closer of its kind could
    // still take (cmark's `openers_bottom`).
    name: "a paragraph of 3,000 closers with no opener",
    grow: (size) => "a* ".repeat(size),
    full: 3_000,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(doc.textContent).toBe("a* ".repeat(3_000).trim());
    },
  },
  {
    // Each reference is looked up among the identifiers the definitions define: by its label end
    // (`isDefined`), and, for a full reference (`[w][d1]`), by the reference's own lookup, both in
    // `patches/micromark-core-commonmark@2.0.3.patch`. A lookup that searched micromark's list of
    // the definitions (`parser.defined`) for each label read about references times definitions,
    // here the square of the shape; the patch keeps a set beside the list (`definedIdentifiers`),
    // and the rows count the list's reads through a Proxy (tests/parse-work.ts).
    // `remark-inline-links` spliced each definition out of the document's list of blocks,
    // shifting the definitions after it, which its walk's reads hid at these sizes (16.21 times the
    // steps for 8 times the definitions); the rows count the transform's writes to the tree's lists
    // on their own, where that splice reads 46.93 times. The references name the definitions in
    // reverse order, so a search of the list from its start reads half of it for each reference,
    // on average.
    name: "800 definitions and 800 references to them",
    grow: (size) => {
      const definitions = Array.from({ length: size }, (_, id) => `[d${id}]: /u${id}\n`);
      const references = Array.from({ length: size }, (_, at) =>
        at % 2 === 0 ? `[d${size - 1 - at}]` : `[w][d${size - 1 - at}]`
      );
      return `${definitions.join("")}\n${references.join(" ")}\n`;
    },
    full: 800,
    opensContainers: false,
    expectDoc: (doc) => {
      expect(doc.childCount).toBe(1);
      expect(marksOf(doc, "link")).toBe(800);
    },
  },
  {
    // The HTML (flow) resolver runs once a block, on the events the document has so far, so one
    // that read them from the first would read about the square of the blocks.
    name: "1,000 blocks of HTML, each with a paragraph after it",
    grow: (size) => "<div>x</div>\n\ny\n\n".repeat(size),
    full: 1_000,
    opensContainers: false,
    htmlBlocks: (size) => size,
    expectDoc: (doc) => {
      expect(nodesOf(doc, "html")).toBe(1_000);
    },
  },
];

/**
 * The two engines, each built once: building one costs about as much as a small parse, and a parse
 * that throws drops only the parser state it opened (`parseMarkdown`, src/lib-headless.ts), so the
 * tests below share them. Each hands its parsers to tests/parse-work.ts, whose rows count each
 * stage of a parse they read.
 */
let readOnly: Promise<HeadlessProofEditor> | undefined;
let editor: Promise<HeadlessProofEditor> | undefined;

function readOnlyProof(): Promise<HeadlessProofEditor> {
  readOnly ??= createHeadlessProof({
    blockSchema: serverBlockSchema,
    readOnly: true,
    observeParser: countParseStages,
  });
  return readOnly;
}

function defaultProof(): Promise<HeadlessProofEditor> {
  editor ??= createHeadlessProof({
    blockSchema: serverBlockSchema,
    observeParser: countParseStages,
  });
  return editor;
}

/** How many blocks of type `type` stand inside one another on `node`'s deepest path. */
function depthOf(node: ProseMirrorNode, type: string): number {
  let deepest = 0;
  node.forEach((child) => {
    deepest = Math.max(deepest, depthOf(child, type));
  });
  return node.type.name === type ? deepest + 1 : deepest;
}

for (const shape of shapes) {
  const refused = shape.refusedInDefaultMode;
  const refusedReadOnly = shape.refusedInReadOnlyMode;

  if (refusedReadOnly === undefined) {
    test(`the read-only engine parses ${shape.name} at a cost proportional to its length`, async () => {
      const proof = await readOnlyProof();
      const doc = parsedWithinWorkBounds(
        `the read-only engine's parse of ${shape.name}`,
        shape,
        (body) => proof.parseMarkdown(body)
      );
      shape.expectDoc(doc);
    });
  } else {
    test(`the read-only engine refuses ${shape.name} at a cost proportional to its length`, async () => {
      const proof = await readOnlyProof();
      refusalWithinWorkBounds(
        `the read-only engine's refusal of ${shape.name}`,
        shape,
        (body) => proof.parseMarkdown(body),
        refusedReadOnly
      );
    });
  }

  if (refused === undefined) {
    test(`the default mode parses ${shape.name} at a cost proportional to its length`, async () => {
      const proof = await defaultProof();
      const doc = parsedWithinWorkBounds(
        `the default mode's parse of ${shape.name}`,
        shape,
        (body) => proof.parseMarkdown(body)
      );
      shape.expectDoc(doc);
    });
  } else {
    test(`the default mode refuses ${shape.name} at a cost proportional to its length`, async () => {
      const proof = await defaultProof();
      refusalWithinWorkBounds(
        `the default mode's refusal of ${shape.name}`,
        shape,
        (body) => proof.parseMarkdown(body),
        refused
      );
    });
  }
}

/** The two modes, which read a body's blocks the same way: only their directives differ. */
const modes = [
  ["the read-only engine", readOnlyProof],
  ["the default mode", defaultProof],
] as const;

for (const [mode, proofOf] of modes) {
  test(`${mode} nests 50 list markers and reads the 51st as text`, async () => {
    const proof = await proofOf();
    const deepest = proof.parseMarkdown(`${"- ".repeat(maxListNesting)}x`);
    expect(depthOf(deepest, "bullet_list")).toBe(maxListNesting);
    expect(deepest.textContent).toBe("x");

    const over = proof.parseMarkdown(`${"- ".repeat(maxListNesting + 1)}x`);
    expect(depthOf(over, "bullet_list")).toBe(maxListNesting);
    expect(over.textContent).toBe("- x");
  });

  test(`${mode} nests 100 quotes and reads the 101st as text`, async () => {
    const proof = await proofOf();
    const deepest = proof.parseMarkdown(`${"> ".repeat(maxQuoteNesting)}x`);
    expect(depthOf(deepest, "blockquote")).toBe(maxQuoteNesting);
    expect(deepest.textContent).toBe("x");

    const over = proof.parseMarkdown(`${"> ".repeat(maxQuoteNesting + 1)}x`);
    expect(depthOf(over, "blockquote")).toBe(maxQuoteNesting);
    expect(over.textContent).toBe("> x");
  });

  test(`${mode} nests 33 quote-and-list levels and reads the 34th list as text`, async () => {
    const proof = await proofOf();
    const deepest = proof.parseMarkdown(`${"> - ".repeat(maxAlternatingNesting)}x`);
    expect(depthOf(deepest, "blockquote")).toBe(maxAlternatingNesting);
    expect(depthOf(deepest, "bullet_list")).toBe(maxAlternatingNesting);
    expect(deepest.textContent).toBe("x");

    // The 34th quote stands at the bound's hundredth block; the list its line would open next
    // would stand past it, so the rest of the line is that quote's text.
    const over = proof.parseMarkdown(`${"> - ".repeat(maxAlternatingNesting + 1)}x`);
    expect(depthOf(over, "blockquote")).toBe(maxAlternatingNesting + 1);
    expect(depthOf(over, "bullet_list")).toBe(maxAlternatingNesting);
    expect(over.textContent).toBe("- x");
  });

  test(`${mode} opens a typed block inside 99 blocks and reads one inside 100 as text`, async () => {
    const proof = await proofOf();
    const fence = [":::callout{}", "a", ":::"];
    const insideQuotes = "> ".repeat(maxBlockNesting - 1);
    const inside = proof.parseMarkdown(`${fence.map((line) => insideQuotes + line).join("\n")}\n`);
    expect(depthOf(inside, "blockquote")).toBe(maxBlockNesting - 1);
    expect(depthOf(inside, "callout")).toBe(1);
    expect(inside.textContent).toBe("a");

    const atBound = "> ".repeat(maxBlockNesting);
    const over = proof.parseMarkdown(`${fence.map((line) => atBound + line).join("\n")}\n`);
    expect(depthOf(over, "blockquote")).toBe(maxBlockNesting);
    expect(depthOf(over, "callout")).toBe(0);
    expect(over.textContent).toContain(":::callout{}");
  });

  test(`${mode} keeps every line past the bound as the text it is`, async () => {
    const proof = await proofOf();
    const past = 5;
    const lines: string[] = [];
    for (let level = 1; level <= maxListNesting + past; level++) {
      // Each line past the bound stands at the indentation of the innermost item the bound allows,
      // so every one of them is a list opener the bound refuses rather than a deeper item.
      const indent = Math.min(level, maxListNesting + 1) - 1;
      lines.push(`${"  ".repeat(indent)}- level ${level} words`);
    }
    const doc = proof.parseMarkdown(`${lines.join("\n")}\n`);

    expect(depthOf(doc, "bullet_list")).toBe(maxListNesting);
    for (let level = 1; level <= maxListNesting + past; level++) {
      expect(doc.textContent).toContain(`level ${level} words`);
    }
    // The refused markers are text too, not a marker the reader never sees.
    expect(doc.textContent).toContain(`- level ${maxListNesting + 1} words`);
  });

  test(`${mode} reads a chain of 1,000 list markers as text past the bound`, async () => {
    // Without the bound this chain overflows the stack of Milkdown's parser, which is a page the
    // reader loses rather than a body they cannot read.
    const proof = await proofOf();
    const doc = proof.parseMarkdown(`${"- ".repeat(1_000)}x`);
    expect(depthOf(doc, "bullet_list")).toBe(maxListNesting);
    expect(doc.textContent).toBe(`${"- ".repeat(1_000 - maxListNesting)}x`);
  });
}

/** An mdast node, as much of one as the bare processor's tree is read through here. */
type MdastNode = {
  type: string;
  value?: string;
  children?: MdastNode[];
};

/** How many nodes of type `type` stand inside one another on `node`'s deepest path. */
function mdastDepthOf(node: MdastNode, type: string): number {
  let deepest = 0;
  for (const child of node.children ?? []) deepest = Math.max(deepest, mdastDepthOf(child, type));
  return node.type === type ? deepest + 1 : deepest;
}

/** Every character of text `node` holds, in order. */
function mdastText(node: MdastNode): string {
  if (node.children === undefined) return node.value ?? "";
  return node.children.map(mdastText).join("");
}

test("the bound holds through a bare remark-parse processor", async () => {
  // Loading the headless engine loads the module that holds micromark's own `list` and
  // `blockQuote` to the bound. The processor below shares those two objects with every parser in
  // this tree - unless a second copy of `micromark-core-commonmark` is installed, which is what
  // this test fails on.
  await readOnlyProof();
  const processor = unified().use(remarkParse);

  const items = processor.parse(`${"- ".repeat(maxListNesting + 1)}x`) as unknown as MdastNode;
  expect(mdastDepthOf(items, "list")).toBe(maxListNesting);
  expect(mdastText(items)).toBe("- x");

  const quotes = processor.parse(`${"> ".repeat(maxQuoteNesting + 1)}x`) as unknown as MdastNode;
  expect(mdastDepthOf(quotes, "blockquote")).toBe(maxQuoteNesting);
  expect(mdastText(quotes)).toBe("> x");
});
