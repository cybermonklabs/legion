import { expect, test } from "bun:test";

import {
  askOpenerLines,
  blockPointerValueProblem,
  DISPATCH_BLOCK_ID_ENCODINGS,
  DISPATCH_BLOCK_POINTER_VALUES,
  DISPATCH_TYPED_BLOCK_ID_SPELLINGS,
  decodeBlockId,
  encodeBlockId,
  formatBlockReference,
  formatItemBlockReference,
  MAX_BLOCK_ID_BYTES,
  parseBlockFragment,
  parseBlockPointerValue,
  parseCanonicalBlockReference,
  parseCanonicalItemBlockReference,
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
  expect(parseBlockPointerValue(row.value, row.items) !== undefined).toBe(row.ok);
  // A refused value has a reason, and an accepted one none.
  expect(blockPointerValueProblem(row.value, row.items) === undefined).toBe(row.ok);
  if (row.problem !== undefined) {
    expect(blockPointerValueProblem(row.value, row.items)).toContain(row.problem);
  }
});

// A reference of an accepted value is written back as it was: a block of a document by
// formatBlockReference, a block of an issue's item by formatItemBlockReference, and an issue is
// its own reference.
test("each reference of an accepted value is written back as it was", () => {
  for (const row of DISPATCH_BLOCK_POINTER_VALUES.filter((row) => row.ok && row.value !== "")) {
    for (const value of row.value.split(" ")) {
      const document = parseCanonicalBlockReference(value);
      const item = parseCanonicalItemBlockReference(value);
      const written =
        document !== undefined
          ? formatBlockReference(document)
          : item !== undefined
            ? formatItemBlockReference(item)
            : value;
      expect(written).toBe(value);
      expect(document !== undefined || item !== undefined || row.items?.includes("issue")).toBe(
        true
      );
    }
  }
});

// An item reference names an issue's ask, comment or message, or an unpinned block of one, by the
// item's whole lower-case uuid, as Go's text.CanonicalItemBlockRef reads it: each row's want is
// the item's kind and the block id it reads, or "" for a value it refuses.
test.each([
  ["dispatch://CORE-1/ask/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f#b-q%3A1", "ask q:1"],
  ["dispatch://CORE-1/comment/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f#b-b1", "comment b1"],
  ["dispatch://CORE-1/message/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f#b-b1", "message b1"],
  ["dispatch://CORE-1/ask/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f", "ask"],
  ["dispatch://CORE-1/comment/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f", "comment"],
  ["dispatch://CORE-1/message/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f", "message"],
  ["dispatch://CORE-1/comment/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f#b-b1@v1", ""],
  ["dispatch://CORE-1/comment/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f@v1", ""],
  ["dispatch://CORE-1/comment/4F1C2D3E-5A6B-4C7D-8E9F-0A1B2C3D4E5F#b-b1", ""],
  ["dispatch://CORE-1/comment/4F1C2D3E-5A6B-4C7D-8E9F-0A1B2C3D4E5F", ""],
  ["dispatch://CORE-1/comment/4f1c2d3e#b-b1", ""],
  ["dispatch://CORE-1/comment/4f1c2d3e", ""],
  ["dispatch://CORE/artifact/handbook-md/comment/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f#b-b1", ""],
  ["dispatch://CORE/artifact/handbook-md/comment/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f", ""],
  ["dispatch://CORE-1/comment/4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f#b-%62", ""],
  ["dispatch://CORE-1/spec#b-b1", ""],
])("reads item block reference %p", (value, want) => {
  const read = parseCanonicalItemBlockReference(value);
  const parts = read === undefined ? [] : [read.item, read.block];
  expect(parts.filter((part) => part !== undefined).join(" ")).toBe(want);
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
])("askOpenerLines: $name", ({ lines, want }) => {
  expect(askOpenerLines(lines.join("\n"))).toEqual([...want]);
});

test("askOpenerLines reads lines that end in a carriage return and a line feed", () => {
  const lines = ["```", opener("a"), "```", opener("b"), ""];
  expect(askOpenerLines(lines.join("\r\n"))).toEqual([opener("b")]);
});
