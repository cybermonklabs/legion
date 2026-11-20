/** The end of the CI step that runs a test, as `step-end.ts` reads it, and the removal of a test
 *  file's scratch that only that end bounds.
 *
 *  The removal rows run a `bun test` of a file of their own, whose `afterAll` removes directories
 *  the row made through `removeScratch`, with a stand-in `rm` first on its `PATH` where the row
 *  needs one: one that never ends, and one that sleeps past Bun's own 5 s for a hook and then
 *  removes. Each such run is a bounded child of the `short` bound: it starts one `bun` and one
 *  `rm`. The reader's rows read `stepEnds` in this process, setting the variable and putting it
 *  back. */

import { afterEach, expect, test } from "bun:test";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { rowLimit, runBounded } from "./bounded-subprocess";
import {
  bunRowMs,
  noHookLimit,
  removeScratch,
  stepEnds,
  stepEndsVariable,
  stepRoomMs,
} from "./step-end";

const stepEndModule = fileURLToPath(new URL("./step-end.ts", import.meta.url));
const made: string[] = [];

afterEach(
  () => removeScratch(made.splice(0), rowLimit("short")),
  // The rows' own scratch goes as their children's does, which is what this file holds: in a
  // process of its own, bounded by what the CI step leaves, and no limit of Bun's on the hook.
  noHookLimit
);

/** A scratch directory of the row's, removed after it. */
function scratch(): string {
  const directory = mkdtempSync(join(tmpdir(), "step-end-"));
  made.push(directory);
  return directory;
}

/** What a run of a child file did, and how long the row waited for it, in ms. */
interface ChildRun {
  readonly code: number;
  readonly output: string;
  readonly ms: number;
}

/** Runs `bun test` over a file of the row's own, in `row`, whose `afterAll` removes `paths` through
 *  `removeScratch` with `rowMs` for the limit of its rows, and whose one row prints `ROW RAN`.
 *  `rm` is the stand-in `stand` writes, where there is one, first on the child's `PATH`; the
 *  child's environment holds the step's end `ends`, in whole seconds, or none. */
async function childRun(
  row: string,
  paths: readonly string[],
  rowMs: number,
  { rm, ends }: { readonly rm?: "never" | "slow"; readonly ends?: string } = {}
): Promise<ChildRun> {
  const file = join(row, "removal.test.ts");
  writeFileSync(
    file,
    [
      'import { afterAll, test } from "bun:test";',
      `import { noHookLimit, removeScratch } from ${JSON.stringify(stepEndModule)};`,
      `afterAll(() => removeScratch(${JSON.stringify(paths)}, ${rowMs}), noHookLimit);`,
      'test("a row", () => console.log("ROW RAN"));',
      "",
    ].join("\n")
  );
  let path = process.env.PATH ?? "";
  if (rm !== undefined) {
    const bin = join(row, "bin");
    mkdirSync(bin);
    const real = Bun.which("rm");
    if (real === null) throw new Error("this machine has no rm on its PATH");
    // `never` is `rm` held in its call, as on a hung mount: the removal's bound kills it, and the
    // bounded child's kill of its tree ends it where that bound did not. `slow` takes 6 s, past
    // Bun's own limit of 5 s for a hook, and then removes.
    const body = rm === "never" ? "exec sleep 3600" : `sleep 6\nexec ${real} "$@"`;
    writeFileSync(join(bin, "rm"), `#!/bin/sh\n${body}\n`);
    chmodSync(join(bin, "rm"), 0o755);
    path = `${bin}:${path}`;
  }
  const started = performance.now();
  const { code, stderr, stdout } = await runBounded({
    argv: [process.execPath, "test", file],
    bound: "short",
    cwd: row,
    env: {
      HOME: process.env.HOME ?? "",
      PATH: path,
      TMPDIR: row,
      ...(ends === undefined ? {} : { [stepEndsVariable]: ends }),
    },
    from: import.meta.url,
  });
  return { code, ms: performance.now() - started, output: `${stdout}${stderr}` };
}

/** Now, in whole seconds since the epoch, rounded up. */
const nowS = () => Math.ceil(Date.now() / 1000);

test(
  "a removal whose process never ends fails at what the CI step leaves, naming its directory, with no hook limit of Bun's",
  async () => {
    const row = scratch();
    const kept = mkdtempSync(join(row, "kept-"));
    // The end leaves the removal 8 s once the child starts: room for a slow start of the child,
    // which would otherwise find the end already past (a row below holds that refusal).
    const endS = nowS() + stepRoomMs / 1000 + 8;
    const run = await childRun(row, [kept], bunRowMs, { ends: String(endS), rm: "never" });
    expect(run.code, run.output).toBe(1);
    expect(run.output).toContain(`the removal of ${kept} did not end within `);
    expect(run.output).toContain(
      `before the end of the CI step that runs it, at ${new Date(endS * 1000).toISOString()} (${stepEndsVariable}), and was killed`
    );
    expect(run.output).toContain("ROW RAN");
    expect(run.output).not.toContain("timed out");
    // A timer fires no sooner than its time, so the kill came no sooner than the step left.
    expect(Date.now()).toBeGreaterThanOrEqual(endS * 1000 - stepRoomMs);
    // The stand-in removed nothing: the run's `rm` was the stand-in.
    expect(existsSync(kept)).toBe(true);
  },
  rowLimit("short")
);

test(
  "with no step's end, a removal whose process never ends fails at the limit of the file's rows, naming its directory",
  async () => {
    const row = scratch();
    const kept = mkdtempSync(join(row, "kept-"));
    const run = await childRun(row, [kept], 2_000, { rm: "never" });
    expect(run.code, run.output).toBe(1);
    expect(run.output).toContain(
      `the removal of ${kept} did not end within 2 s, the limit of this file's rows, and was killed`
    );
    expect(run.output).not.toContain("timed out");
    expect(run.ms).toBeGreaterThanOrEqual(2_000);
    expect(existsSync(kept)).toBe(true);
  },
  rowLimit("short")
);

test(
  "a removal slower than Bun's own limit for a hook ends, and the hook passes",
  async () => {
    const row = scratch();
    const removed = mkdtempSync(join(row, "removed-"));
    writeFileSync(join(removed, "file"), "x");
    const run = await childRun(row, [removed], 60_000, { rm: "slow" });
    expect(run.code, run.output).toBe(0);
    expect(run.output).not.toContain("timed out");
    expect(run.ms).toBeGreaterThanOrEqual(6_000);
    expect(existsSync(removed)).toBe(false);
  },
  rowLimit("short")
);

// The control of the stand-ins above: the real `rm` removes every directory it is given.
test(
  "the removal removes each directory it names",
  async () => {
    const row = scratch();
    const removed = [mkdtempSync(join(row, "one-")), mkdtempSync(join(row, "two-"))];
    for (const directory of removed) writeFileSync(join(directory, "file"), "x");
    const run = await childRun(row, removed, bunRowMs, { ends: String(nowS() + 600) });
    expect(run.code, run.output).toBe(0);
    expect(removed.filter((directory) => existsSync(directory))).toEqual([]);
  },
  rowLimit("short")
);

test(
  "a step's end that is already past fails the file as it starts, naming the variable",
  async () => {
    const row = scratch();
    const kept = mkdtempSync(join(row, "kept-"));
    const run = await childRun(row, [kept], bunRowMs, { ends: String(nowS() - 60) });
    expect(run.code, run.output).toBe(1);
    expect(run.output).toContain(`${stepEndsVariable} is `);
    expect(run.output).toContain("which was already past when the first test file");
    expect(run.output).not.toContain("ROW RAN");
  },
  rowLimit("short")
);

/** `stepEnds()` with the variable set to `value`, or unset for undefined, and put back after. */
function readWith(value: string | undefined): number | undefined {
  const saved = process.env[stepEndsVariable];
  try {
    if (value === undefined) delete process.env[stepEndsVariable];
    else process.env[stepEndsVariable] = value;
    return stepEnds();
  } finally {
    if (saved === undefined) delete process.env[stepEndsVariable];
    else process.env[stepEndsVariable] = saved;
  }
}

test("the step's end is read in whole seconds, and none is no end", () => {
  const endS = nowS() + 600;
  expect(readWith(String(endS))).toBe(endS * 1000);
  expect(readWith(undefined)).toBeUndefined();
});

test("a step's end that is no whole number of seconds is refused by name, the empty one too", () => {
  for (const value of ["", " 1800000000", "1800000000.5", "1.8e9", "-1", "0x6B49D200", "soon"]) {
    expect(() => readWith(value)).toThrow(
      `${stepEndsVariable} is ${JSON.stringify(value)}, which is no whole number of seconds since the epoch`
    );
  }
});

test("a step's end already past, or farther than any job runs, as one in milliseconds is, is refused by name", () => {
  // An hour back is before this file started; an end that passes after the file started is the
  // step's own clock running out, which its waits meet as an end with no time left.
  expect(() => readWith(String(nowS() - 3_600))).toThrow("which was already past when");
  expect(() => readWith(String(Date.now() + 600_000))).toThrow(
    "which is more than the 120 hours any job runs after"
  );
});
