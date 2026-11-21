import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";

import { referenceReads, referenceReadsSince } from "./reference-reads";
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
// pass is counted rather than timed (`reference-reads.ts`): each body is one span, so its one
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
  const before = referenceReads();
  const cited = composerReferences(body, origin).map((item) => item.reference);
  const read = referenceReadsSince(before);
  expect(read.trimReference.characters).toBeGreaterThanOrEqual(body.length);
  expect(read.trimReference.characters).toBeLessThanOrEqual(2 * body.length);
  // Each body is one span, handed to the route reader once.
  expect(read.referenceRouteFromHref.calls).toBe(1);
  expect(read.referenceRouteFromHref.characters).toBeLessThanOrEqual(body.length);
  expect(cited).toEqual([...refs]);
});

// The composer hands each span of a body to `referenceRouteFromHref` once, so its reading of a
// body costs one pass over the body's spans whatever they hold, counted rather than timed
// (`reference-reads.ts`). The spans here take its longest way: a `dispatch://` reference to an
// ask's part pinned to a revision (the fragment read apart, the reference before it parsed
// again), and a 65,536-character id that parses as far as the extra segment after it. A caller
// that handed it the body, or the rest of it, for every span reads about the square of the body.
test("a body of many references hands each span to referenceRouteFromHref once", () => {
  const count = 4096;
  const pinned = Array.from(
    { length: count },
    (_, index) => `dispatch://CORE-1/ask/q${index}#b-q${index}@v1`
  );
  const long = `dispatch://CORE-1/ask/${"a".repeat(1 << 16)}/x`;
  const body = `${pinned.join(" ")} ${long}`;
  const before = referenceReads();
  const cited = composerReferences(body, origin).map((item) => item.reference);
  const read = referenceReadsSince(before);
  expect(cited).toEqual(pinned);
  expect(read.referenceRouteFromHref.calls).toBe(count + 1);
  expect(read.referenceRouteFromHref.characters).toBe(body.length - count);
});
