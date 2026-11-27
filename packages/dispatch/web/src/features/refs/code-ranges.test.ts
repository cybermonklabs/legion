import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { type CodeRange, codeRanges } from "./code-ranges";

/** One body the server's `text.CodeRanges` and this reader are both held to, and the code
 *  contexts it holds, in order, each counted in UTF-16 units as this reader counts them. */
interface CodeRow {
  readonly name: string;
  readonly body: string;
  readonly ranges: readonly CodeRange[];
}

/** The one table of those bodies, read from where the server's tests read it
 *  (`TestCodeRangesMatchesTheSharedCodeTable` in `text/code_test.go`), which writes it from the
 *  server's reading with each byte offset counted in UTF-16 units, so the two readers cannot
 *  drift apart: a body either reads otherwise fails here or there. */
const CODE_ROWS: readonly CodeRow[] = JSON.parse(
  readFileSync(
    fileURLToPath(
      new URL(
        "../../../../../envoy/internal/dispatch/text/testdata/code-ranges.json",
        import.meta.url
      )
    ),
    "utf8"
  )
);

/** The ranges as one line, which a failure prints beside the row's name. */
function written(ranges: readonly CodeRange[]): string {
  return ranges
    .map(({ block, end, start }) => `${start}-${end}${block ? " block" : ""}`)
    .join(", ");
}

test("every body of the server's code table has the code contexts the server reads", () => {
  expect(CODE_ROWS.length).toBeGreaterThan(0);
  const differing = CODE_ROWS.flatMap((row) => {
    const got = written(codeRanges(row.body));
    const want = written(row.ranges);
    return got === want ? [] : [{ got, name: row.name, want }];
  });
  expect(differing).toEqual([]);
});
