import { expect, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";

import { expectOnePass, ONE_PASS_SLACK, stepsAtNAndFourN } from "../../__tests__/one-pass";
import { readerStep } from "./reader-steps";
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
// reference, and a long run of closers followed by anything else is read in one pass too. A trim
// that rescans the run for every start, as an end-anchored regular expression does, took over
// ten seconds on the 64 KiB body alone. One pass is what a count shows, not a clock: the
// reader's loops (`reader-steps.ts`) take at most four times their steps, and a few more, on a
// run four times as long, the body each row cites from being that longer one. The expressions
// it runs over the run are checked as they are written (`reader-expressions.test.ts`).
test.each([
  [
    "parentheses",
    (run: number) => `dispatch://CORE-1${")".repeat(run)}`,
    1 << 20,
    ["dispatch://CORE-1"],
  ],
  [
    "parentheses and underscores",
    (run: number) => `dispatch://CORE-1${")_".repeat(run)}`,
    1 << 19,
    ["dispatch://CORE-1"],
  ],
  [
    "parentheses and asterisks after a dashboard URL",
    (run: number) => `${origin}/issues/CORE-1${")*".repeat(run)}`,
    1 << 19,
    ["dispatch://CORE-1"],
  ],
  [
    "parentheses then a letter",
    (run: number) => `dispatch://CORE-1${")".repeat(run)}x`,
    1 << 16,
    [],
  ],
] as const)("a reference trailed by %s is read in one pass", (_name, body, run, refs) => {
  const references = expectOnePass((n) => composerReferences(body(n), origin), run / 4);
  expect(references.map((item) => item.reference)).toEqual([...refs]);
});

/** The trim those rows guard against: for every end it tries, it counts the parentheses before
 *  that end again, which is what an end-anchored expression costs on a run of closers. */
function trimRescanningTheRun(value: string): string {
  let end = value.length;
  for (; end > 0; end -= 1) {
    let opened = 0;
    let closed = 0;
    for (let index = 0; index < end; index += 1) {
      readerStep();
      const char = value.charAt(index);
      if (char === "(") opened += 1;
      else if (char === ")") closed += 1;
    }
    const char = value.charAt(end - 1);
    if (char === ")" ? opened >= closed : !".,;:!?*_~".includes(char)) {
      break;
    }
  }
  return value.slice(0, end);
}

// The control of the rows above: the same bound fails the trim that rescans the run, so a reader
// that went back to it would fail them.
test("the one-pass bound fails a trim that rescans a closing run for every end it tries", () => {
  const body = (run: number) => `dispatch://CORE-1${")".repeat(run)}`;
  const { atN, atFourN } = stepsAtNAndFourN((n) => trimRescanningTheRun(body(n)), 1 << 8);
  expect(trimRescanningTheRun(body(1 << 8))).toBe("dispatch://CORE-1");
  expect(atFourN).toBeGreaterThan(4 * atN + ONE_PASS_SLACK);
});
