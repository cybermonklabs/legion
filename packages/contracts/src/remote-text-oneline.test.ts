import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { oneLine } from "./remote-text";

// The TypeScript half of one table: Go's redact.OneLine reads it too
// (packages/envoy/internal/redact/oneline_test.go), so both languages print one text another party
// wrote as the same line, strconv.Quote's escapes and all.
const table = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/redact/testdata/oneline.json", import.meta.url),
    "utf8"
  )
) as { rows: { id: string; text: string; quoted: string }[] };

test("the shared one-line table has rows", () => {
  expect(table.rows.length).toBeGreaterThan(0);
});

for (const row of table.rows) {
  test(`oneLine of ${row.id} is what Go's OneLine prints`, () => {
    expect(oneLine(row.text)).toBe(row.quoted);
  });
}

// A JavaScript string can hold a lone surrogate, which a Go string cannot, so the shared table has
// none: oneLine quotes a text that holds one, high or low, at either end or before a character
// that does not complete it, with the surrogate as U+FFFD; a pair, the first and the last pair
// among them, prints as it is.
test.each([
  ["a\ud800", '"a\ufffd"'],
  ["\udc00b", '"\ufffdb"'],
  ["\ud800x", '"\ufffdx"'],
  ["\ud800\ud800\udc00", '"\ufffd\u{10000}"'],
  ["a\ud800\nb", '"a\ufffd\\nb"'],
  ["x\ud83d\ude00", "x\ud83d\ude00"],
  ["\ud83d\ude00 ok", "\ud83d\ude00 ok"],
  ["\udbff\udfff", "\udbff\udfff"],
  ["\ud7ff\ue000", "\ud7ff\ue000"],
] as const)("oneLine of %p is %p", (text, quoted) => {
  expect(oneLine(text)).toBe(quoted);
});
