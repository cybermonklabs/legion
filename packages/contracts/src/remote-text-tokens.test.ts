import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { tokens } from "./remote-text";

// The TypeScript half of one rule: this replays packages/envoy/internal/redact/testdata/tokens.json,
// that file itself read by relative path and never a copy, as Go's tokens_fixture_test.go replays
// it against redact.Tokens.
const TABLE_PATH = new URL("../../envoy/internal/redact/testdata/tokens.json", import.meta.url);

type Row = { id: string; text: string; limit: number; tokens: string[]; want: string };

// The table is this repository's own fixture, which Go's test reads under the same shape.
const table: { rows: Row[] } = JSON.parse(readFileSync(TABLE_PATH, "utf8"));
const rows = table.rows;

test("the shared tokens table has rows", () => {
  expect(rows.length).toBeGreaterThan(0);
});

for (const row of rows) {
  test(`tokens table row ${row.id} reads as the table says`, () => {
    expect(tokens(row.text, row.limit, ...row.tokens)).toBe(row.want);
  });
}
