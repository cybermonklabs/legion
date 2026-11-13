import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

import { limitsOfFiles, refusedLimits } from "../row-limits";
import { eventually, rowLimit } from "./bounded-subprocess";

// RED TEST of the testing phase, dispatch://INFRA-3082, 2026-11-13. This file holds one correctly
// bounded polling row in a file of its own: the row waits on `eventually` and states
// `rowLimit("short")`, as the rule asks. The guard reads it so and refuses it nothing. The polling
// row of `row-limits.test.ts` compared the files that poll with an exact list of four, so a file
// like this one turned that row red although the rule refuses nothing in it.
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
