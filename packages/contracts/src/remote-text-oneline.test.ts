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
