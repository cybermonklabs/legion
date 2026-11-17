import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

import { limitsOfFiles, refusedLimits } from "../row-limits";
import { eventually, rowLimit } from "./bounded-subprocess";

// One correctly bounded polling row in a file of its own: the row waits on `eventually` and
// states `rowLimit("short")`, as the rule asks. The guard reads it as waiting under the short
// bound and refuses it nothing, so a file like this one leaves the guard green, here and in the
// polling row of `row-limits.test.ts`, which asks only that the files that poll include its four.
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));
const thisFile = "__tests__/row-limits-polling.test.ts";

test(
  "a polling row in a file of its own is read under the short bound and refused nothing",
  async () => {
    await eventually("a state that already holds", () => true);
    const limits = limitsOfFiles(e2eDirectory, [thisFile]);
    expect(limits.map((limit) => [limit.waitsFor, limit.bound, limit.stated?.fromBounds])).toEqual([
      [["`eventually`"], "short", true],
    ]);
    expect(refusedLimits(limits)).toEqual([]);
  },
  rowLimit("short")
);
