import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";

import { referenceTrimReads } from "./reference-spans";
import { composerReferences } from "./routes";

const origin = "https://dispatch.test";

// The dashboard's half of the shared text table: the composer's reference pills and unfurl cards
// cite exactly what the server's `text.Extract` indexes for the same body, so the dashboard and
// the reference graph never disagree about what a text cites. The Go reader walks the same rows.
// A row marked `grammar` holds a bare key, a `#id` or an id prefix, which the pills read through
// neither a `dispatch://` reference nor a dashboard address; `textReferences` walks those.
test.each(
  DISPATCH_TEXT_REFERENCES.filter((row) => row.grammar === undefined).map(
    (row) => [row.body, row.origin ?? origin, row.refs] as const
  )
)("%s cites, read against %s, %p", (body, server, refs) => {
  expect(composerReferences(body, server).map((item) => item.reference)).toEqual([...refs]);
});

// A body near the server's 1 MiB request cap that trails a reference with a run of closing
// parentheses, alone or between the emphasis delimiters the trim also drops, still cites that
// reference, and a long run of closers followed by anything else is read in one pass too. The
// pass is counted rather than timed (`referenceTrimReads`): each body is one span, so its one
// trim reads every character of it once to count the parentheses and walks back over at most
// as many again. A trim that rescans the run for every start, as an end-anchored regular
// expression does, took over ten seconds on the 64 KiB body alone; written as a loop it reads
// about the square of the body, and handed to a regular expression it reads nothing here, and
// either way the count below fails, naming the count read and the bound it broke.
test.each([
  ["parentheses", `dispatch://CORE-1${")".repeat(1 << 20)}`, ["dispatch://CORE-1"]],
  [
    "parentheses and underscores",
    `dispatch://CORE-1${")_".repeat(1 << 19)}`,
    ["dispatch://CORE-1"],
  ],
  [
    "parentheses and asterisks after a dashboard URL",
    `${origin}/issues/CORE-1${")*".repeat(1 << 19)}`,
    ["dispatch://CORE-1"],
  ],
  ["parentheses then a letter", `dispatch://CORE-1${")".repeat(1 << 16)}x`, []],
] as const)("a reference trailed by %s is read in one pass", (_name, body, refs) => {
  const before = referenceTrimReads();
  const cited = composerReferences(body, origin).map((item) => item.reference);
  const read = referenceTrimReads() - before;
  expect(read).toBeGreaterThanOrEqual(body.length);
  expect(read).toBeLessThanOrEqual(2 * body.length);
  expect(cited).toEqual([...refs]);
});
