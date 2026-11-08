import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts/dispatch-text-references";

import { composerReferences, isSafeAppPath } from "./routes";

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
// bound is generous for a loaded machine: a trim that rescans the run for every start, as an
// end-anchored regular expression does, takes over ten seconds on the 64 KiB body alone.
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
  const start = performance.now();
  const cited = composerReferences(body, origin).map((item) => item.reference);
  expect(performance.now() - start).toBeLessThan(2000);
  expect(cited).toEqual([...refs]);
});

// Every pill is a link, and what it links to is read out of text a person typed. The pill's
// href is built from the route rather than from that text, and `isSafeAppPath` is what says so
// at the sink: a path of this app, one leading slash and no second one, that the URL parser
// resolves against a base to that same base. These bodies are the shapes that would matter if it
// were not - a scheme, another origin, and a tag - and each either cites nothing or cites a place
// inside the dashboard.
test.each([
  ["a javascript scheme", "see javascript:alert(1) here"],
  ["a protocol-relative address", "see //evil.example/issues/CORE-1 here"],
  ["another origin's dashboard address", "see https://evil.example/issues/CORE-1 here"],
  ["a data URL", "see data:text/html,<script>alert(1)</script> here"],
  ["a tag typed into the body", "see <img src=x onerror=alert(1)> here"],
  ["a tag beside a real reference", "see <img src=x onerror=alert(1)> and dispatch://CORE-1"],
] as const)("a body holding %s links only inside the app", (_name, body) => {
  for (const { href } of composerReferences(body, origin)) {
    expect(href.startsWith("/")).toBe(true);
    expect(href.startsWith("//")).toBe(false);
  }
});

test("a reference whose path is not this app's is no pill at all", () => {
  // The guard itself, since no route builds such a path today: a path that is not the app's is
  // dropped rather than drawn, which is what a route kind added later would meet.
  expect(isSafeAppPath("/issues/CORE-1")).toBe(true);
  expect(isSafeAppPath("/issues/CORE-1?v=2#b-b1")).toBe(true);
  expect(isSafeAppPath("//evil.example/issues/CORE-1")).toBe(false);
  expect(isSafeAppPath("javascript:alert(1)")).toBe(false);
  expect(isSafeAppPath("https://evil.example/")).toBe(false);
  // A browser reads this as `//evil.example/x`, since the URL parser takes the backslash for a
  // slash; the leading-slash half of the guard passes it and the resolved origin refuses it.
  expect(isSafeAppPath("/\\evil.example/issues/CORE-1")).toBe(false);
  expect(new URL("/\\evil.example/issues/CORE-1", origin).origin).toBe("https://evil.example");
});
