import { describe, expect, test } from "bun:test";
import { NotUtf8Error, readFileText, unstorableBytes } from "../dispatch-file-text";

/** The bytes of `parts`: each string in UTF-8, each number one byte as it is. */
const bytesOf = (...parts: (string | number)[]): Uint8Array =>
  Buffer.concat(
    parts.map((part) => (typeof part === "string" ? Buffer.from(part) : Buffer.of(part)))
  );

const nul = (character: number) =>
  `NUL_CHARACTER: content holds a NUL character (U+0000) at character ${character}, which Dispatch cannot store`;
const notUtf8 = (byte: string, character: number) =>
  `INVALID_UTF8: content holds a byte that is not UTF-8 (${byte}) at character ${character}, which Dispatch cannot store`;

// INFRA-4224 R3 (spec v27): the bytes a file or stdin held are refused as Dispatch refuses them
// (`unstorableText`, packages/envoy/internal/dispatch/api/storable_text.go): at the first U+0000 or
// byte that is not UTF-8, whichever comes first, read as Go's `utf8.DecodeRune` reads them, and
// where it stands counted from 1 in UTF-16 units.
describe("unstorableBytes", () => {
  test.each([
    ["a NUL before a byte that is not UTF-8", bytesOf("A", 0x00, "B", 0xff), nul(2)],
    ["a byte that is not UTF-8 before a NUL", bytesOf("A", 0xff, "B", 0x00), notUtf8("0xFF", 2)],
    // A byte order mark is one unit and a four-byte character two.
    [
      "a byte order mark and a four-byte character before a NUL",
      bytesOf("\uFEFF😀", 0x00, 0xff),
      nul(4),
    ],
    [
      "a byte order mark and a four-byte character before a byte that is not UTF-8",
      bytesOf("\uFEFF😀", 0xfe, 0x00),
      notUtf8("0xFE", 4),
    ],
    // The NUL cuts short the sequence 0xE2 opens, so that byte is refused first.
    ["a NUL that cuts a sequence short", bytesOf("A", 0xe2, 0x82, 0x00), notUtf8("0xE2", 2)],
  ])("refuses %s at the first of them", (_name, bytes, refusal) => {
    expect(unstorableBytes("content", bytes)?.message).toBe(refusal);
  });

  test("refuses nothing in UTF-8 without a NUL, a byte order mark and U+FFFD among it", () => {
    expect(unstorableBytes("content", bytesOf("\uFEFFCafé 😀 \uFFFD\n"))).toBeUndefined();
    expect(unstorableBytes("content", bytesOf())).toBeUndefined();
  });
});

describe("readFileText", () => {
  test("throws NotUtf8Error with every byte it read for bytes that are not UTF-8, and decodes UTF-8 with a NUL", () => {
    const bytes = bytesOf("A", 0x00, "B", 0xff);
    let thrown: unknown;
    try {
      readFileText("-", () => bytes);
    } catch (error) {
      thrown = error;
    }
    expect(thrown).toBeInstanceOf(NotUtf8Error);
    expect((thrown as NotUtf8Error).bytes).toEqual(bytes);
    expect(readFileText("-", () => bytesOf("\uFEFFA", 0x00, "B"))).toBe("\uFEFFA\u0000B");
  });
});
