import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { ROW_MARGIN_MS, spawnWithin } from "./row-clock";
import { temporaryDirectories } from "./temporary-directories";

const makeDirectory = temporaryDirectories("test-fixtures-row-clock");

// The process runs past its own bound, so the row fails naming the part, not at bun's limit.
test("a process past its own bound is killed, and the row fails naming its part", () => {
  expect(() => spawnWithin({ part: "a sleep of 30 s", boundMs: 1_000 }, ["sleep", "30"])).toThrow(
    "a sleep of 30 s did not end within its bound of 1000 ms"
  );
});

test("a process that ends within its bound answers as Bun.spawnSync does", () => {
  const result = spawnWithin({ part: "echo", boundMs: 10_000 }, ["echo", "here"]);
  expect({ code: result.exitCode, out: result.stdout.toString() }).toEqual({
    code: 0,
    out: "here\n",
  });
});

// This file states no limit, so a part with no bound of its own runs on no clock at all.
test("a part with no bound of its own, in a file that states no limit, runs to its end", () => {
  const result = spawnWithin({ part: "true", boundMs: Number.POSITIVE_INFINITY }, ["true"]);
  expect(result.exitCode).toBe(0);
});

/**
 * The limit the child file below states for each of its rows: its first row waits half of it in
 * the process, and each process the file starts would run past it.
 */
const CHILD_LIMIT_MS = 8_000;
/** The bound of the process the child's beforeAll starts, which needs 2 s of it. */
const HOOK_BOUND_MS = 6_000;
/** The child's bound: its two rows' limits, its beforeAll's bound and bun's own 5 s to start. */
const CHILD_BOUND_MS = 2 * CHILD_LIMIT_MS + HOOK_BOUND_MS + ROW_MARGIN_MS;

/**
 * The row clock read on a `bun test` of its own, whose file states its limit through fileLimit.
 * A row's process is killed at what its row's limit leaves, before bun's own timer, and the row
 * fails naming it and that limit. A describe's beforeAll that runs after a row that spent most of
 * its limit runs on no row's clock: its process gets its own bound, HOOK_BOUND_MS, where the row
 * before it left it less than nothing.
 */
test(
  "a row's process is killed at what its row's limit leaves, and a hook between rows keeps its own bound",
  () => {
    const work = makeDirectory("run");
    writeFileSync(
      join(work, "clock.test.ts"),
      `import { beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { fileLimit, spawnWithin } from ${JSON.stringify(join(import.meta.dir, "row-clock"))};

setDefaultTimeout(fileLimit(${CHILD_LIMIT_MS}));

test("a row that spends half its limit in this process", () => {
  Bun.sleepSync(${CHILD_LIMIT_MS / 2});
});

describe("a describe whose beforeAll starts a process", () => {
  beforeAll(() => {
    spawnWithin({ part: "a sleep of 2 s", boundMs: ${HOOK_BOUND_MS} }, ["sleep", "2"]);
  });
  test("reads that it ran", () => {
    expect(1).toBe(1);
  });
});

test("a row whose process would outlast it", () => {
  spawnWithin({ part: "a sleep of 30 s", boundMs: 60_000 }, ["sleep", "30"]);
});
`
    );
    const child = spawnWithin(
      { part: "the child bun test", boundMs: CHILD_BOUND_MS },
      [process.execPath, "test", "./clock.test.ts"],
      { cwd: work }
    );
    const output = `${child.stdout.toString()}${child.stderr.toString()}`;

    expect(output).toMatch(
      new RegExp(
        `a sleep of 30 s did not end within the \\d+ ms its row's limit of ${CHILD_LIMIT_MS} ms left it: the limit counts less than the parts the row waits for`
      )
    );
    expect(output).not.toContain("timed out");
    expect(output).not.toContain("a sleep of 2 s");
    expect(output).toMatch(/^ 2 pass$/m);
    expect(output).toMatch(/^ 1 fail$/m);
    expect(child.exitCode).not.toBe(0);
  },
  CHILD_BOUND_MS + ROW_MARGIN_MS
);
