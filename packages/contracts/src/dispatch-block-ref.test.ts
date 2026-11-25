import { expect, test } from "bun:test";

import {
  askOpenerBlockId,
  askOpenerLines,
  askOpeners,
  blockPointerValueProblem,
  DISPATCH_BLOCK_ID_ENCODINGS,
  DISPATCH_BLOCK_POINTER_VALUES,
  DISPATCH_TYPED_BLOCK_ID_SPELLINGS,
  decodeBlockId,
  encodeBlockId,
  formatBlockReference,
  MAX_BLOCK_ID_BYTES,
  parseBlockFragment,
  parseBlockPointerValue,
  parseCanonicalBlockReference,
  typedBlockIdAttribute,
} from "./dispatch-block-ref";

// Ids at the bound and one byte past it, in UTF-8 bytes: an `é` is two, so the multi-byte pair
// is one a count of UTF-16 code units would take the wrong way. Neither decoding reads the bound,
// which only a reader of text holds an id to.
const atBound = "x".repeat(MAX_BLOCK_ID_BYTES);
const pastBound = `${atBound}x`;
const multiByteAtBound = "é".repeat(MAX_BLOCK_ID_BYTES / 2);
const multiBytePastBound = `${multiByteAtBound}x`;
// An id past 1024 characters ending in a Latin-1 one, which Bun 1.3's TextEncoder writes as a NUL
// when it encodes the whole string.
const longLatin1 = `${"a".repeat(3200)}é`;

test.each([...DISPATCH_BLOCK_ID_ENCODINGS])("encodes block id %p", (row) => {
  expect(encodeBlockId(row.id)).toBe(row.encoded);
});

test("an encoded id decodes back to the id", () => {
  for (const row of DISPATCH_BLOCK_ID_ENCODINGS) {
    expect(decodeBlockId(encodeBlockId(row.id))).toBe(row.id);
  }
});

test("a long id is encoded byte for byte and decodes back whole", () => {
  const encoded = encodeBlockId(longLatin1);
  expect(encoded).toBe(`${"a".repeat(3200)}%C3%A9`);
  expect(decodeBlockId(encoded)).toBe(longLatin1);
  expect(parseBlockFragment(`${encoded}@v3`)).toEqual({ block: longLatin1, pin: "3" });
});

// No id: a raw `@` (`encodeBlockId` writes `@` as `%40`, so one is only ever a pin), an empty
// fragment, a percent-encoding that is not UTF-8 and a control character. An id of any length is
// read.
test.each([
  ["ship-it@v2", undefined],
  ["", undefined],
  ["%E0%A4", undefined],
  ["%0A", undefined],
  ["%00", undefined],
  [encodeBlockId(atBound), atBound],
  [encodeBlockId(pastBound), pastBound],
  [encodeBlockId(multiByteAtBound), multiByteAtBound],
  [encodeBlockId(multiBytePastBound), multiBytePastBound],
])("decodes block id %p", (encoded, want) => {
  expect(decodeBlockId(encoded)).toBe(want);
});

// The `#b-` fragments of DISPATCH_TEXT_REFERENCES' block references, after the `b-`.
test.each([
  ["ship-it", { block: "ship-it" }],
  ["d%C3%A9cision%3A1", { block: "décision:1" }],
  ["d%c3%a9", { block: "dé" }],
  ["it%40v2", { block: "it@v2" }],
  ["ship-it@v2", { block: "ship-it", pin: "2" }],
  ["d%C3%A9cision%3A1@v12", { block: "décision:1", pin: "12" }],
  // The largest pin Go's `int` holds, its digits kept past what a number holds exactly; one more
  // names nothing, as the server reads it.
  ["ship-it@v9223372036854775807", { block: "ship-it", pin: "9223372036854775807" }],
  ["ship-it@v9223372036854775808", undefined],
  ["ship-it@v10000000000000000000000", undefined],
  ["", undefined],
  ["%E0%A4", undefined],
  ["%0A", undefined],
  ["@v2", undefined],
  ["ship-it@v", undefined],
  ["ship-it@v0", undefined],
  ["ship-it@v01", undefined],
  ["ship-it@vx", undefined],
  ["a@b", undefined],
  ["a@v1@v2", undefined],
  [`${encodeBlockId(multiByteAtBound)}@v2`, { block: multiByteAtBound, pin: "2" }],
  [`${encodeBlockId(multiBytePastBound)}@v2`, { block: multiBytePastBound, pin: "2" }],
])("parses block fragment %p", (fragment, want) => {
  expect(parseBlockFragment(fragment)).toEqual(want);
});

test("the Go encoder's table is the same table", async () => {
  // `packages/envoy/internal/dispatch/text` cannot import TypeScript, so it walks a JSON copy.
  // Regenerate it from `packages/contracts` rather than editing it:
  //   bun -e 'import { DISPATCH_BLOCK_ID_ENCODINGS as t } from "./src/dispatch-block-ref.ts";
  //           await Bun.write("../envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json",
  //             JSON.stringify(t, null, 2) + "\n")'
  const path = new URL(
    "../../envoy/internal/dispatch/text/testdata/dispatch-block-id-encodings.json",
    import.meta.url
  );
  expect(JSON.parse(await Bun.file(path).text())).toEqual([...DISPATCH_BLOCK_ID_ENCODINGS]);
});

test.each([...DISPATCH_BLOCK_POINTER_VALUES])("decides block[] value %p", (row) => {
  expect(parseBlockPointerValue(row.value) !== undefined).toBe(row.ok);
  // A refused value has a reason, and an accepted one none.
  expect(blockPointerValueProblem(row.value) === undefined).toBe(row.ok);
  if (row.problem !== undefined) {
    expect(blockPointerValueProblem(row.value)).toContain(row.problem);
  }
});

test("each reference of an accepted value is written back as it was", () => {
  for (const row of DISPATCH_BLOCK_POINTER_VALUES.filter((row) => row.ok && row.value !== "")) {
    for (const value of row.value.split(" ")) {
      const reference = parseCanonicalBlockReference(value);
      expect(reference && formatBlockReference(reference)).toBe(value);
    }
  }
});

test("the Go schema's table of block[] values is the same table", async () => {
  // `packages/envoy/internal/dispatch/pmdoc` cannot import TypeScript, so it walks a JSON copy.
  // Regenerate it from `packages/contracts` rather than editing it:
  //   bun -e 'import { DISPATCH_BLOCK_POINTER_VALUES as t } from "./src/dispatch-block-ref.ts";
  //           await Bun.write("../envoy/internal/dispatch/pmdoc/testdata/block-pointer-values.json",
  //             JSON.stringify(t, null, 2) + "\n")'
  const path = new URL(
    "../../envoy/internal/dispatch/pmdoc/testdata/block-pointer-values.json",
    import.meta.url
  );
  expect(JSON.parse(await Bun.file(path).text())).toEqual([...DISPATCH_BLOCK_POINTER_VALUES]);
});

test.each([...DISPATCH_TYPED_BLOCK_ID_SPELLINGS])("writes typed block id %p", (row) => {
  expect(typedBlockIdAttribute(row.id)).toBe(row.written);
});

test("the Go renderer's table of typed block id spellings is the same table", async () => {
  // `packages/envoy/internal/dispatch/pmdoc` cannot import TypeScript, so it walks a JSON copy.
  // Regenerate it from `packages/contracts` rather than editing it:
  //   bun -e 'import { DISPATCH_TYPED_BLOCK_ID_SPELLINGS as t } from "./src/dispatch-block-ref.ts";
  //           await Bun.write("../envoy/internal/dispatch/pmdoc/testdata/typed-block-id-spellings.json",
  //             JSON.stringify(t, null, 2) + "\n")'
  const path = new URL(
    "../../envoy/internal/dispatch/pmdoc/testdata/typed-block-id-spellings.json",
    import.meta.url
  );
  expect(JSON.parse(await Bun.file(path).text())).toEqual([...DISPATCH_TYPED_BLOCK_ID_SPELLINGS]);
});

/** A line that opens the ask block `id`, with the attributes the release before wrote on it. */
const opener = (id: string) => `:::ask{#${id} state="answered" answer="Ship it."}`;

test.each([
  {
    name: "a line opens the block past its indentation and its containers' markers",
    lines: [
      opener("a"),
      `   ${opener("b")}`,
      `> ${opener("c")}`,
      `>>${opener("d")}`,
      `- ${opener("e")}`,
      `*\t${opener("f")}`,
      `+ ${opener("g")}`,
      `1. ${opener("h")}`,
      `12) ${opener("i")}`,
      `[^note]: ${opener("j")}`,
      `> - [^x]:  ${opener("k")}`,
      `::::${opener("l").slice(3)}`,
    ],
    want: [
      opener("a"),
      opener("b"),
      opener("c"),
      opener("d"),
      opener("e"),
      opener("f"),
      opener("g"),
      opener("h"),
      opener("i"),
      opener("j"),
      opener("k"),
      `::::${opener("l").slice(3)}`,
    ],
  },
  {
    // A list marker with no space or tab after it, a blank footnote label, text before the
    // directive, two colons and another block type open no ask block.
    name: "a line opens no ask block past what is not a container marker",
    lines: [
      `-${opener("a")}`,
      `1.${opener("b")}`,
      `[^ ]: ${opener("c")}`,
      `Quoted: ${opener("d")}`,
      `::${opener("e").slice(3)}`,
      ":::callout{#f}",
    ],
    want: [],
  },
  {
    // A fence of backticks or tildes, three or more, is closed only by a fence of the same
    // character at least as long with nothing after it but blanks.
    name: "no line of fenced code opens a block",
    lines: [
      "```",
      opener("a"),
      "```",
      "~~~",
      opener("b"),
      "~~~  ",
      "````markdown",
      opener("c"),
      "```",
      opener("d"),
      "`````",
      "~~~",
      opener("e"),
      "```",
      opener("f"),
      "~~~~",
      "```",
      opener("g"),
      "``` text",
      opener("h"),
      "```",
      opener("i"),
    ],
    want: [opener("i")],
  },
  {
    // A backtick fence's info string holds no backtick, so a line that starts with a code span
    // opens no fence; nor do two backticks.
    name: "a line that only starts like a fence opens none",
    lines: ["``` a ` b", opener("a"), "``", opener("b")],
    want: [opener("a"), opener("b")],
  },
  {
    name: "a fence opened past a container's marker is code like any other",
    lines: [
      "> ```",
      `> ${opener("a")}`,
      "> ```",
      "[^x]: ```",
      `    ${opener("b")}`,
      "    ```",
      "- ~~~",
      `  ${opener("c")}`,
      "  ~~~",
      opener("d"),
    ],
    want: [opener("d")],
  },
  {
    name: "no line of the front matter a document opens with opens a block",
    lines: ["---  ", opener("a"), "title: Plan", "---\t", opener("b")],
    want: [opener("b")],
  },
  {
    // A rule after text is no front matter.
    name: "front matter opens only on the first line",
    lines: ["Intro", "---", opener("a"), "---"],
    want: [opener("a")],
  },
  {
    name: "an opening `---` nothing closes is no front matter",
    lines: ["---", opener("a")],
    want: [opener("a")],
  },
  {
    // A backslash before ASCII punctuation escapes it, so `\]` and `\[` are part of the label. An
    // unescaped `[` makes the line no footnote label.
    name: "a footnote label holds an escaped bracket, and a block opens on its line",
    lines: [
      `[^a\\]b]: ${opener("a")}`,
      "",
      `[^c\\[d]: ${opener("b")}`,
      "",
      `[^e[f]: ${opener("c")}`,
    ],
    want: [opener("a"), opener("b")],
  },
  {
    name: "a fence opened on the line of a footnote label that holds an escaped bracket is code",
    lines: ["[^a\\]b]: ```", `    ${opener("a")}`, "    ```", "", opener("b")],
    want: [opener("b")],
  },
  {
    // An inline code span keeps the spaces at the start of its later lines, so such a line can
    // start with an opener. Four columns or more past its containers' lines, the line goes on
    // with the paragraph and opens no block. The columns of a footnote definition or a list item
    // that the line goes on with are its containers' columns.
    name: "a line of an inline code span four columns past its containers opens no block",
    lines: [
      "Text `code",
      `    ${opener("a")}\` more.`,
      "",
      "[^n]: Note `code",
      `        ${opener("b")}\` more.`,
      "",
      `    ${opener("c")}`,
      "",
      "10. Item",
      "",
      `    ${opener("d")}`,
    ],
    want: [opener("c"), opener("d")],
  },
  {
    // An opener four columns or more past the marker before it is indented code.
    name: "a line four columns past its containers' markers opens no block",
    lines: [`    > ${opener("a")}`, "", `>     ${opener("b")}`, "", `-     ${opener("c")}`],
    want: [],
  },
])("askOpenerLines: $name", ({ lines, want }) => {
  expect(askOpenerLines(lines.join("\n"))).toEqual([...want]);
});

test("askOpenerLines reads lines that end in a carriage return and a line feed", () => {
  const lines = ["```", opener("a"), "```", opener("b"), ""];
  expect(askOpenerLines(lines.join("\r\n"))).toEqual([opener("b")]);
});

test("askOpenerBlockId reads back each id as the renderer writes it, and no other spelling", () => {
  for (const { id, written } of DISPATCH_TYPED_BLOCK_ID_SPELLINGS) {
    expect(askOpenerBlockId(`:::ask{${written} state="open"}`)).toBe(id);
    expect(askOpenerBlockId(`::::ask{${written}}`)).toBe(id);
    expect(askOpeners([`:::ask{${written}}`], id)).toEqual([`:::ask{${written}}`]);
  }
  // The renderer writes `#a`, never `id="a"`. A `#id` ends at a character the shortcut cannot
  // hold, and only a space or the closing brace may follow it.
  for (const line of [':::ask{id="a"}', ":::ask{#a.b}", ":::ask{#}", ':::ask{state="open" #a}']) {
    expect(askOpenerBlockId(line)).toBeUndefined();
  }
});
