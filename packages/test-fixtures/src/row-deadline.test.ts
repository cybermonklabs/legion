import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "./temporary-directories";

// Where a wait ends against the guard of the row it runs in. Each row below runs test files of its
// own in a `bun test` of its own, under a guard of 2400 ms: the row-deadline preload sets each
// wait's deadline 1000 ms (the least margin) before the guard, 1400 ms after the newest mark. A
// file spends more than that margin before its wait starts, so a wait that counted from its own
// start would outlast the guard and leave only Bun's "timed out" to read. Each verdict is what the
// child printed, and in which order its own message and Bun's came: none reads a clock.

const makeDirectory = temporaryDirectories("test-fixtures-row-deadline");

const PRELOAD = join(import.meta.dir, "row-deadline.ts");
const WAIT = JSON.stringify(join(import.meta.dir, "wait.ts"));
const GUARD = "--timeout=2400";
const BEFORE_THE_GUARD = "1000 ms before the row's guard of 2400 ms (bun test --timeout)";

/** Bun's own words for a row or hook its guard ended. */
const BUN_TIMED_OUT = "timed out";

interface Run {
  readonly output: string;
  readonly exitCode: number | null;
}

/** Writes each file, headed by the imports every one of them uses, and runs `bun test` on them. */
function runBunTest(files: Readonly<Record<string, string>>, flags: readonly string[]): Run {
  const work = makeDirectory("run");
  for (const [name, body] of Object.entries(files)) {
    const imports = `import { beforeAll, describe, jest, test } from "bun:test";
import { span, timeLeft, until } from ${WAIT};
`;
    writeFileSync(join(work, name), imports + body);
  }
  const run = Bun.spawnSync(
    [process.execPath, "test", ...flags, ...Object.keys(files).map((name) => `./${name}`)],
    { cwd: work, env: process.env }
  );
  return { output: `${run.stdout.toString()}${run.stderr.toString()}`, exitCode: run.exitCode };
}

/** Runs a Bun driver (no `bun test`) that waits for its third read, with `deadline` as its env. */
function runDriver(deadline: string | undefined): string {
  const work = makeDirectory("driver");
  writeFileSync(
    join(work, "driver.ts"),
    `import { until } from ${WAIT};
let reads = 0;
try {
  await until("the proof's fact", () => ++reads === 3);
  console.log(\`held after \${reads} reads\`);
} catch (error) {
  console.log(\`\${String(error)} after \${reads} reads\`);
}
`
  );
  const env: Record<string, string | undefined> = { ...process.env };
  delete env["LEGION_PROOF_DEADLINE_MS"];
  if (deadline !== undefined) env["LEGION_PROOF_DEADLINE_MS"] = deadline;
  const run = Bun.spawnSync([process.execPath, "./driver.ts"], { cwd: work, env });
  return `${run.stdout.toString()}${run.stderr.toString()}`;
}

/** A wait that ended at its row's deadline: its own message, and no word of Bun's timeout. */
function expectGaveUpBeforeTheGuard(run: Run, what: string): void {
  expect(run.output).toContain(
    `until gave up waiting for ${what}, ${BEFORE_THE_GUARD}; last read: false`
  );
  expect(run.output).not.toContain(BUN_TIMED_OUT);
  expect(run.exitCode).toBe(1);
}

const ROW = `
test("a row with two waits", async () => {
  await span(1_100, "the row's first wait, which the second must not be given back");
  await until("the row's second fact", () => false);
});
`;

const FILE_HOOK = `
beforeAll(async () => {
  await span(1_100, "the hook's first wait, which the second must not be given back");
  await until("the file's fixture", () => false);
});
test("a row the file's hook serves", () => {});
`;

test("a wait ends at its row's deadline, the second wait of a row included", () => {
  expectGaveUpBeforeTheGuard(
    runBunTest({ "row.test.ts": ROW }, [GUARD, "--preload", PRELOAD]),
    "the row's second fact"
  );
});

test("a wait ends at the deadline of the file's beforeAll", () => {
  expectGaveUpBeforeTheGuard(
    runBunTest({ "hook.test.ts": FILE_HOOK }, [GUARD, "--preload", PRELOAD]),
    "the file's fixture"
  );
});

// The row before the describe spends half the hook's budget: counted from that row's start, the
// hook's span would be refused, so the wait reaching its own deadline shows the count began when
// the row ended.
test("a wait ends at the deadline of a describe's beforeAll that follows a row", () => {
  const file = `
test("a row before the describe", async () => {
  await span(700, "the row's own work");
});
describe("a describe", () => {
  beforeAll(async () => {
    await span(1_000, "the hook's first wait");
    await until("the describe's fixture", () => false);
  });
  test("a row the describe's hook serves", () => {});
});
`;
  expectGaveUpBeforeTheGuard(
    runBunTest({ "describe.test.ts": file }, [GUARD, "--preload", PRELOAD]),
    "the describe's fixture"
  );
});

// Without `--isolate` Bun runs a preload's `beforeAll` once, before the first file of the process
// (measured on Bun 1.4.4), so a later file's `beforeAll` counts from the last row of the file
// before it: as above, that row's own work is not counted against the hook.
test("a wait ends at the deadline of a later file's beforeAll in the same process", () => {
  const first = `
test("the first file's row", async () => {
  await span(700, "the row's own work");
});
`;
  const run = runBunTest({ "a.test.ts": first, "b.test.ts": FILE_HOOK }, [
    GUARD,
    "--preload",
    PRELOAD,
  ]);
  // The one row that passes is the first file's: the second file's hook fails its row.
  expect(run.output).toContain(" 1 pass\n 1 fail");
  expectGaveUpBeforeTheGuard(run, "the file's fixture");
});

test("a wait ends at its row's deadline in a --parallel worker", () => {
  const run = runBunTest({ "row.test.ts": ROW, "hook.test.ts": FILE_HOOK }, [
    "--parallel=2",
    GUARD,
    "--preload",
    PRELOAD,
  ]);
  expectGaveUpBeforeTheGuard(run, "the row's second fact");
  expectGaveUpBeforeTheGuard(run, "the file's fixture");
});

// The first row moves the fake clock a minute on and leaves it held through its own end and the
// next row's start. Had the preload marked then, the next row's deadline would be that fake
// minute and the span would pass; counted from the last real mark, the span is refused.
test("the preload takes no mark while a test holds the fake clock", () => {
  const file = `
test("a row that leaves the fake clock held", async () => {
  await span(700, "the row's own work");
  jest.useFakeTimers();
  jest.advanceTimersByTime(60_000);
});
test("the row after it", async () => {
  jest.useRealTimers();
  await span(1_000, "longer than the deadline of the last real mark leaves");
});
`;
  const run = runBunTest({ "fake.test.ts": file }, [GUARD, "--preload", PRELOAD]);
  expect(run.output).toContain(
    "span: a span of 1000 ms (longer than the deadline of the last real mark leaves) would end " +
      `after the deadline, ${BEFORE_THE_GUARD}`
  );
  expect(run.output).not.toContain(BUN_TIMED_OUT);
});

test("without the preload a wait counts from its own start, and says so", () => {
  const file = `
test("a row with one wait", async () => {
  await until("the row's only fact", () => false);
});
`;
  const run = runBunTest({ "bare.test.ts": file }, [GUARD]);
  expect(run.output).toContain(
    `until gave up waiting for the row's only fact, ${BEFORE_THE_GUARD}, counted from the ` +
      "wait's own start because no row-deadline preload took a mark; last read: false"
  );
  expect(run.output).not.toContain(BUN_TIMED_OUT);
});

test("under --timeout 0 there is no deadline, and timeLeft is the longest a timer takes", () => {
  const file = `
test("a row with no guard", () => {
  console.log(\`time left: \${timeLeft()}\`);
});
`;
  const run = runBunTest({ "unguarded.test.ts": file }, ["--timeout=0", "--preload", PRELOAD]);
  expect(run.output).toContain(`time left: ${2 ** 31 - 1}`);
  expect(run.exitCode).toBe(0);
});

test("outside bun test a wait ends at LEGION_PROOF_DEADLINE_MS", () => {
  // The year 9999 is a deadline the wait never reaches, and 1970 one it has passed: past it, the
  // wait reads once and fails.
  expect(runDriver("253402300799000")).toContain("held after 3 reads");
  expect(runDriver("1")).toContain(
    "until gave up waiting for the proof's fact, at the proof's deadline " +
      "1970-01-01T00:00:00.001Z (LEGION_PROOF_DEADLINE_MS); last read: false after 1 reads"
  );
});

test("outside bun test with no usable LEGION_PROOF_DEADLINE_MS a wait refuses to start", () => {
  expect(runDriver(undefined)).toContain(
    "until has no deadline to end by: this process is not `bun test`, and " +
      "LEGION_PROOF_DEADLINE_MS (epoch milliseconds, which a proof's script sets for its Bun " +
      "drivers) is not set after 0 reads"
  );
  expect(runDriver("soon")).toContain(
    'LEGION_PROOF_DEADLINE_MS is "soon", not epoch milliseconds after 0 reads'
  );
});
