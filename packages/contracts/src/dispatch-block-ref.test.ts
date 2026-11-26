import { expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";

import {
  askOpenerAttributes,
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

/** A block id as each of its code points' own UTF-8 bytes write it, the rule `encodeBlockId`
 *  holds to: every byte percent-encoded in upper-case hex save an ASCII letter, digit or `-`. */
function encodedByCodePoint(id: string): string {
  const encoder = new TextEncoder();
  let encoded = "";
  for (const character of id) {
    for (const byte of encoder.encode(character)) {
      const bare =
        (byte >= 0x30 && byte <= 0x39) ||
        (byte >= 0x41 && byte <= 0x5a) ||
        (byte >= 0x61 && byte <= 0x7a) ||
        byte === 0x2d;
      encoded += bare
        ? String.fromCharCode(byte)
        : `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return encoded;
}

// Ids of ASCII letters, digits and `-` only, the empty one and a long one included.
const plainIds = ["", "ship-it", "R1", "0123456789-ABCXYZ-abcxyz", "-", "a".repeat(3200)];

test("encodeBlockId writes every id byte for byte as each code point's UTF-8 does, and an id of plain characters calls no TextEncoder", () => {
  const ids = [
    ...plainIds,
    "decision.1",
    "two words",
    "ship-it@v2",
    "50%",
    "a/b",
    "décision:1",
    "ship-🚀",
    "\u{1F680}",
    longLatin1,
    "é".repeat(1100),
  ];
  const wanted = ids.map(encodedByCodePoint);
  expect(ids.map(encodeBlockId)).toEqual(wanted);
  const encode = spyOn(TextEncoder.prototype, "encode");
  try {
    expect(plainIds.map(encodeBlockId)).toEqual(plainIds);
    expect(encode, "an id of plain characters called the encoder").toHaveBeenCalledTimes(0);
  } finally {
    encode.mockRestore();
  }
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

/**
 * One row of `packages/envoy/internal/dispatch/pmdoc/testdata/ask-block-scanner.json`, the one
 * corpus of the two hand-written block scanners: `readAskBlocks` here (read through
 * `askOpenerLines`) and Go's `refs.questionBlocks`, whose test reads the same file. `lines` is a
 * document, `ids` the ids of the ask blocks that open in it, `why` the rule the row holds.
 */
interface AskBlockScannerRow {
  readonly name: string;
  readonly why: string;
  readonly lines: readonly string[];
  readonly ids: readonly string[];
}

const SCANNER_CORPUS: readonly AskBlockScannerRow[] = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/testdata/ask-block-scanner.json", import.meta.url),
    "utf8"
  )
);

test("the shared corpus of the two block scanners is not empty", () => {
  expect(SCANNER_CORPUS.length).toBeGreaterThan(0);
});

test.each([...SCANNER_CORPUS])("askOpenerLines: $name", ({ lines, ids, why }) => {
  const openers = askOpenerLines(lines.join("\n"));
  // Each opener is its line from the directive's first colon on, past the containers' markers.
  for (const line of openers) {
    expect(line, why).toMatch(/^:{3,}ask\{/);
    expect(
      lines.some((original) => original.endsWith(line)),
      why
    ).toBe(true);
  }
  // The blocks Go's scanner reads are the ones that name an id.
  expect(
    openers.flatMap((line) => askOpenerBlockId(line) ?? []),
    why
  ).toEqual([...ids]);
});

test("askOpenerLines reads lines that end in a carriage return and a line feed", () => {
  const lines = ["```", opener("a"), "```", opener("b"), ""];
  expect(askOpenerLines(lines.join("\r\n"))).toEqual([opener("b")]);
});

// A pattern with two runs of spaces and tabs side by side tries each split of a long run between
// them, so a line of many tabs and then a character that ends the match took seconds. The second
// and third lines go on with the paragraph (a table's delimiter row is looked for in each), the
// fourth is a blank line, and the sixth stands inside the ask block (a closing line is looked for
// in it). Each is read in time linear in its length.
test("askOpenerLines reads lines of 50,000 tabs in under one second", () => {
  const tabs = "\t".repeat(50_000);
  const lines = ["Some text.", `${tabs}|`, `|-${tabs}x|`, tabs, opener("a"), `:${tabs}x`, ":::"];
  const start = performance.now();
  const openers = askOpenerLines(lines.join("\n"));
  expect(performance.now() - start).toBeLessThan(1000);
  expect(openers).toEqual([opener("a")]);
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

// The character references a value reads back: every numeric reference, which the renderer writes
// for `"`, a line feed and a carriage return (`&#x22;`, `&#xA;`, `&#xD;`) and a browser's parser
// for any character, and the five named ones. A character a reference decodes to starts no
// reference of its own: the value is read in one pass.
test.each([
  { name: "a hexadecimal quote", written: "a&#x22;b", read: 'a"b' },
  { name: "a hexadecimal line feed", written: "a&#xA;b", read: "a\nb" },
  { name: "a hexadecimal carriage return", written: "a&#xD;b", read: "a\rb" },
  { name: "an upper-case hexadecimal marker", written: "&#X41;&#x61;", read: "Aa" },
  { name: "a decimal quote", written: "a&#34;b", read: 'a"b' },
  { name: "a decimal letter", written: "&#65;&#66;", read: "AB" },
  { name: "an astral character", written: "&#x1F4CA;&#128202;", read: "\u{1F4CA}\u{1F4CA}" },
  { name: "an ampersand", written: "R&amp;D", read: "R&D" },
  { name: "a greater-than sign", written: "a&gt;b", read: "a>b" },
  { name: "a less-than sign", written: "a&lt;b", read: "a<b" },
  { name: "a named quote", written: "a&quot;b", read: 'a"b' },
  { name: "a named apostrophe", written: "it&apos;s", read: "it's" },
  { name: "an ampersand that starts a numeric reference", written: "&amp;#x22;", read: "&#x22;" },
  { name: "an ampersand that starts a named reference", written: "&amp;amp;", read: "&amp;" },
  { name: "a named reference this reader does not know", written: "a&nbsp;b", read: "a&nbsp;b" },
  {
    name: "a name that is a property of every object",
    written: "&constructor;",
    read: "&constructor;",
  },
  { name: "a reference with no semicolon", written: "a&amp b &#x22 c", read: "a&amp b &#x22 c" },
  { name: "a bare ampersand", written: "a & b", read: "a & b" },
  { name: "zero", written: "a&#0;b&#x0;c", read: "a\uFFFDb\uFFFDc" },
  { name: "a surrogate", written: "&#xD800;&#57343;", read: "\uFFFD\uFFFD" },
  { name: "a code point past U+10FFFF", written: "&#x110000;&#99999999999;", read: "\uFFFD\uFFFD" },
])("askOpenerAttributes reads $name in a value", ({ written, read }) => {
  expect(askOpenerAttributes(`:::ask{#a answer="${written}"}`).get("answer")).toBe(read);
});

test("askOpenerAttributes reads every quoted attribute of an opener, whatever its id's spelling", () => {
  const attributes = askOpenerAttributes(
    ':::ask{#q state="answered" answered_by="bob" answered_at="2026-11-18T12:05:00Z" selected="[&#x22;REST&#x22;]" answer="Plain &#x22;HTTP&#x22;.&#xA;Next." copied_from="c-1" copied_from_document="CORE-1/spec"}'
  );
  expect(Object.fromEntries(attributes)).toEqual({
    state: "answered",
    answered_by: "bob",
    answered_at: "2026-11-18T12:05:00Z",
    selected: '["REST"]',
    answer: 'Plain "HTTP".\nNext.',
    copied_from: "c-1",
    copied_from_document: "CORE-1/spec",
  });
  // An id written in quotes is an attribute like the others.
  expect(askOpenerAttributes(':::ask{id="a&#xA;b" state="open"}').get("id")).toBe("a\nb");
});

test("askOpenerAttributes reads a name written twice as its last value, and no attribute off a line with no braces", () => {
  expect(askOpenerAttributes(':::ask{#a state="open" state="answered"}').get("state")).toBe(
    "answered"
  );
  expect(askOpenerAttributes(':::ask state="open"').size).toBe(0);
  expect(askOpenerAttributes("").size).toBe(0);
  // A name that every object inherits is an entry like any other.
  expect(askOpenerAttributes(':::ask{#a __proto__="x" constructor="y"}').get("__proto__")).toBe(
    "x"
  );
  expect(askOpenerAttributes(':::ask{#a state="open"}').get("constructor")).toBeUndefined();
});
