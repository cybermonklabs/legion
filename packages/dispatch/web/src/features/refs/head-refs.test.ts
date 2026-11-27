import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { headRefs } from "./head-refs";

/** One cut the server's `text.HeadRefs` and this twin are both held to: `text` cut at `limit`
 *  characters with `ellipsis`, read against the deployment at `origin` (none known where it is
 *  absent), gives `head`. */
interface HeadRow {
  readonly name: string;
  readonly text: string;
  readonly limit: number;
  readonly ellipsis: string;
  readonly origin?: string;
  readonly head: string;
}

/** The one table of those cuts, read from where the server's tests read it
 *  (`TestHeadRefsMatchesTheSharedHeadTable` in `text/runes_test.go`), so the two copies of the
 *  rule cannot drift apart: a row either copy cuts otherwise fails here or there. */
const HEAD_ROWS: readonly HeadRow[] = JSON.parse(
  readFileSync(
    fileURLToPath(
      new URL(
        "../../../../../envoy/internal/dispatch/text/testdata/head-refs.json",
        import.meta.url
      )
    ),
    "utf8"
  )
);

test("every row of the server's head table is cut as the server cuts it", () => {
  expect(HEAD_ROWS.length).toBeGreaterThan(0);
  const differing = HEAD_ROWS.flatMap((row) => {
    const head = headRefs(row.text, row.limit, row.ellipsis, [row.origin ?? ""]);
    return head === row.head ? [] : [{ got: head, name: row.name, want: row.head }];
  });
  expect(differing).toEqual([]);
});

// A head drawn as its characters must not end in half a reference: a cut inside one moves back
// to where it starts, the inline link around it taken whole, and drops the spaces before it.
test("a cut inside a mention moves back to the link's start", () => {
  const text = "ask [@Planner](dispatch://agent/planner-session) about it";
  expect(headRefs(text, 20, "…")).toBe("ask…");
  // The cut in the mention's words, and in its address, are both inside the one link.
  expect(headRefs(text, 9, "…")).toBe("ask…");
  expect(headRefs(text, 40, "…")).toBe("ask…");
});

test("a cut inside an address moves back to the address's start", () => {
  expect(headRefs("see dispatch://CORE-1/spec now", 20, "…")).toBe("see…");
  // A link worded by its author is taken whole as well.
  expect(headRefs("see [the plan](dispatch://CORE-1/spec) now", 30, "…")).toBe("see…");
});

test("a cut inside a bare key moves back to the key's start", () => {
  expect(headRefs("fix it in CORE-1234 today", 15, "…")).toBe("fix it in…");
});

// The head with the ellipsis after it must not read a reference the text does not write:
// `CORE-12` of `CORE-12x4`, which is no key, would read as a working link to another issue.
test("a head that would read a reference the text does not write moves back", () => {
  expect(headRefs("see CORE-12x4 now", 12, "…")).toBe("see…");
});

test("a text with no reference is cut by length, as a text at or under the bound is left whole", () => {
  expect(headRefs("plain words that run on and on", 10, "…")).toBe("plain wor…");
  expect(headRefs("plain words", 11, "…")).toBe("plain words");
  // A reference that ends before the cut is no reason to move it.
  expect(headRefs("CORE-1 and plain words after it", 15, "…")).toBe("CORE-1 and pla…");
});

// A head of nothing tells a reader less than half a reference does.
test("a text that opens with a reference longer than the bound keeps the cut by length", () => {
  expect(headRefs("dispatch://CORE-1/spec#b-abc and more", 10, "…")).toBe("dispatch:…");
});

// The window is the bound and 1,024 characters past it, as the server's
// (`TestHeadRefsReadsAWindowOfTheText`), so its last character is the backtick after the 1,028
// x's. The window reads a span that closes there, but the text's run of two backticks there may
// close nothing, so the span may run past the window: it counts as crossing the cut, which moves
// back to its backtick rather than leaving it open.
test("a code span whose closing backtick ends the window counts as crossing the cut", () => {
  expect(headRefs(`See \`${"x".repeat(1028)}\`\` and more`, 10, "…")).toBe("See…");
});

test("the bound counts characters, so no cut splits one", () => {
  expect(headRefs("😀😀😀😀 words", 4, "…")).toBe("😀😀😀…");
});
