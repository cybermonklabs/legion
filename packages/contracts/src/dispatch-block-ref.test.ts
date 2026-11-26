import { expect, spyOn, test } from "bun:test";

import {
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
