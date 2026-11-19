import { afterEach, expect, test } from "bun:test";
import { type ChildProcess, spawn } from "node:child_process";
import { once } from "node:events";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// The per-test bound (`per-test-bound.ts`), run on small suites of its own: each row writes test
// files into a directory of its own, runs the bound's script over them with a bound of a few
// seconds, and reads how that run ended and all it printed. Without the bound, the rows whose
// fixtures block never end, and each fails at its own timeout.
//
// The rows that block or freeze need /proc (the bound reads the run's processes there), a FIFO and
// the shell tools the fixtures start; on a host without them they are skipped.
const skip = skipWithout("mkfifo", "setsid", "sh", "sleep") || !existsSync("/proc/self/stat");

const scratch = temporaryDirectories("per-test-bound");
const packageDirectory = path.join(import.meta.dir, "..");
const script = path.join(import.meta.dir, "per-test-bound.ts");

/** The bound each row's run is given, in ms. Its fixtures' quick tests take milliseconds; the
 * margin is for a loaded host starting `bun test`. */
const BOUND_MS = 4_000;

/** Each row's own time: the run's start, the bound, its grace and the kill, with room for a
 * loaded host. */
const ROW_MS = 60_000;

const started: ChildProcess[] = [];
afterEach(() => {
  // A run that outlives its row, as every blocked one does without the bound, ends here: each run
  // leads a process group of its own.
  for (const child of started.splice(0)) {
    try {
      if (child.pid !== undefined) process.kill(-child.pid, "SIGKILL");
    } catch {}
  }
});

/** Runs `argv` in `cwd` with `env` over the caller's environment (less `GITHUB_ACTIONS`), and
 * answers how it ended and all it printed, once every holder of its output has let it go. */
async function runIn(cwd: string, argv: string[], env: Record<string, string | undefined>) {
  const { GITHUB_ACTIONS: _, ...inherited } = process.env;
  const [command = "", ...args] = argv;
  const child = spawn(command, args, {
    cwd,
    // A name `env` sets to undefined is left out.
    env: Object.fromEntries(
      Object.entries({ ...inherited, ...env }).filter(([, value]) => value !== undefined)
    ),
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
  });
  started.push(child);
  let output = "";
  child.stdout?.on("data", (chunk: Buffer) => {
    output += chunk.toString();
  });
  child.stderr?.on("data", (chunk: Buffer) => {
    output += chunk.toString();
  });
  const [code] = (await once(child, "close")) as [number | null];
  return { code, output };
}

/** Writes `files` into a directory of their own and runs the bound's script over them, in the
 * order given, with {@link BOUND_MS} and `env`. */
async function run(files: Record<string, string>, env: Record<string, string> = {}) {
  const dir = scratch("run");
  for (const [name, source] of Object.entries(files)) writeFileSync(path.join(dir, name), source);
  const argv = [process.execPath, script, ...Object.keys(files).map((name) => `./${name}`)];
  return runIn(dir, argv, { LEGION_TEST_BOUND_MS: String(BOUND_MS), ...env });
}

/** A FIFO that no process writes: reading it blocks the reading thread for good. */
function fifo(): string {
  const file = path.join(scratch("fifo"), "nobody-writes");
  expect(Bun.spawnSync(["mkfifo", file]).exitCode).toBe(0);
  return file;
}

/** A file of two tests that end at once, run before the file a row is about: the bound has to
 * name that row's file, and count its tests from 1 again. */
const QUICK = [
  'import { test } from "bun:test";',
  'test("ends at once", () => {});',
  'test("ends at once too", () => {});',
].join("\n");

/** A test file whose one test blocks its thread on the FIFO `FIXTURE_FIFO` names, with Bun's own
 * bound at 10 minutes, which never fires: its timer runs on the thread the read holds. */
const BLOCKED = [
  'import { readFileSync } from "node:fs";',
  'import { test } from "bun:test";',
  'test("reads a FIFO no process writes", () => { readFileSync(process.env.FIXTURE_FIFO ?? ""); }, 600_000);',
].join("\n");

test.skipIf(skip)(
  "a test that blocks its thread is named by its file and place once it runs past the bound, and every process the run started is killed",
  async () => {
    const { code, output } = await run(
      {
        "a-quick.test.ts": QUICK,
        "b-blocked.test.ts": [
          'import { readFileSync } from "node:fs";',
          'import { test } from "bun:test";',
          'test("ends at once", () => {});',
          'test("starts processes, then reads a FIFO no process writes", () => {',
          // A child, a grandchild and a process whose parent exits at once, every one holding the
          // run's output open: were one left alive, this row's read of that output would wait out
          // its 300 s.
          '  Bun.spawn(["sh", "-c", "(setsid sleep 301 &); sleep 300"], { stdout: "inherit", stderr: "inherit" });',
          '  readFileSync(process.env.FIXTURE_FIFO ?? "");',
          "}, 600_000);",
        ].join("\n"),
      },
      { FIXTURE_FIFO: fifo() }
    );
    expect(code).toBe(1);
    expect(output).toMatch(
      /\nerror: b-blocked\.test\.ts: test 2 of the file \(counting from 1 the tests that ran in it\) is still running \d+\.\d s after it began; the per-test bound is 4\.0 s \(test\/per-test-bound\.ts\), and bun test is killed\.\n/
    );
    expect(output).toMatch(/\n {2}\d+ sleep 300\n/);
    expect(output).toMatch(/\n {2}\d+ sleep 301\n/);
  },
  ROW_MS
);

test.skipIf(skip)(
  "a hook that blocks is inside its test's bound",
  async () => {
    const { code, output } = await run(
      {
        "hooked.test.ts": [
          'import { readFileSync } from "node:fs";',
          'import { afterEach, test } from "bun:test";',
          'afterEach(() => { readFileSync(process.env.FIXTURE_FIFO ?? ""); }, 600_000);',
          'test("ends at once, then its afterEach blocks", () => {});',
        ].join("\n"),
      },
      { FIXTURE_FIFO: fifo() }
    );
    expect(code).toBe(1);
    expect(output).toMatch(/\nerror: hooked\.test\.ts: test 1 of the file .* is still running /);
  },
  ROW_MS
);

test.skipIf(skip)(
  "a test file whose module body never settles is named by the end of the file before it",
  async () => {
    const { code, output } = await run({
      "a-quick.test.ts": QUICK,
      // A module body is no test: Bun's per-test timeout never starts for it.
      "b-stuck.test.ts": [
        'import { test } from "bun:test";',
        "await Promise.withResolvers<void>().promise;",
        'test("never reached", () => {});',
      ].join("\n"),
    });
    expect(code).toBe(1);
    expect(output).toMatch(
      /\nerror: no test began \d+\.\d s after a-quick\.test\.ts: test 2 of the file ended \(its hooks, or the next file's module body and beforeAll\); the per-test bound is 4\.0 s/
    );
  },
  ROW_MS
);

test.skipIf(skip)(
  "a first test file whose module body never settles is named as the start of bun test",
  async () => {
    const { code, output } = await run({
      "stuck.test.ts": [
        'import { test } from "bun:test";',
        "await Promise.withResolvers<void>().promise;",
        'test("never reached", () => {});',
      ].join("\n"),
    });
    expect(code).toBe(1);
    expect(output).toMatch(
      /\nerror: no test began \d+\.\d s after bun test started \(the first file's module body and beforeAll\); the per-test bound is 4\.0 s/
    );
  },
  ROW_MS
);

test(
  "tests that each end inside the bound pass however long the run, and the slowest is named, not the last",
  async () => {
    const { code, output } = await run({
      // The bound is a wall-clock one, so these tests spend real time: 6 s in all, past the 4 s
      // bound, none of them past it alone.
      "steady.test.ts": [
        'import { test } from "bun:test";',
        'test("waits 3 s", () => Bun.sleep(3_000));',
        'for (const n of [1, 2, 3]) test("waits 1 s, number " + n, () => Bun.sleep(1_000));',
      ].join("\n"),
    });
    expect(output).not.toContain("the per-test bound is");
    expect(output).toMatch(
      /per-test bound: the slowest test was test 1 of steady\.test\.ts, \d+\.\d s;/
    );
    expect(code).toBe(0);
  },
  ROW_MS
);

test(
  "a test Bun's own timeout ends inside the bound is Bun's to report",
  async () => {
    const { code, output } = await run({
      "a-quick.test.ts": QUICK,
      "b-settles.test.ts": [
        'import { test } from "bun:test";',
        'test("waits on a promise that never settles", () => Promise.withResolvers<void>().promise, 1_000);',
      ].join("\n"),
    });
    expect(output).toContain("this test timed out after 1000ms");
    expect(output).not.toContain("the per-test bound is");
    expect(output).toMatch(/per-test bound: the slowest test was test 1 of b-settles\.test\.ts, /);
    expect(code).toBe(1);
  },
  ROW_MS
);

test.skipIf(skip)(
  "a test that ends past the bound while the bound could not act fails the run, though bun test passed it",
  async () => {
    const { code, output } = await run({
      // The first test stops the bound's process (this run's parent) for 7 s; the second spends
      // 5 s, past the 4 s bound, and bun test has passed it and exited before the bound goes on.
      "slow.test.ts": [
        'import { test } from "bun:test";',
        'test("stops the bound for 7 s", () => {',
        '  process.kill(process.ppid, "SIGSTOP");',
        '  Bun.spawn(["sh", "-c", "sleep 7; kill -CONT " + process.ppid]);',
        "});",
        'test("spends 5 s", () => Bun.sleep(5_000), 10_000);',
      ].join("\n"),
    });
    expect(output).toContain("2 pass");
    expect(output).toMatch(
      /\nerror: slow\.test\.ts: test 2 of the file \(counting from 1 the tests that ran in it\) ran \d+\.\d s, past the per-test bound of 4\.0 s/
    );
    expect(code).toBe(1);
  },
  ROW_MS
);

test.skipIf(skip)(
  "a bound whose process stopped while tests went on reads the events it missed before it judges",
  async () => {
    const { code, output } = await run({
      // Its first test stops the bound's process (this run's parent) for 6 s, past the 4 s bound,
      // while the tests after it go on, each ending inside the bound, until after the bound goes
      // on again.
      "frozen.test.ts": [
        'import { test } from "bun:test";',
        'test("stops the bound for 6 s", () => {',
        '  process.kill(process.ppid, "SIGSTOP");',
        '  Bun.spawn(["sh", "-c", "sleep 6; kill -CONT " + process.ppid]);',
        "});",
        'for (const n of [1, 2, 3, 4, 5]) test("waits 1.5 s, number " + n, () => Bun.sleep(1_500));',
      ].join("\n"),
    });
    expect(output).not.toContain("the per-test bound is");
    expect(output).toContain("6 pass");
    expect(code).toBe(0);
  },
  ROW_MS
);

test.skipIf(skip)(
  "events sent while the bound's process is stopped are all read, however many",
  async () => {
    const { code, output } = await run(
      {
        "flood.test.ts": [
          'import { readFileSync } from "node:fs";',
          'import { test } from "bun:test";',
          'test("stops the bound for 2 s", () => {',
          '  process.kill(process.ppid, "SIGSTOP");',
          '  Bun.spawn(["sh", "-c", "sleep 2; kill -CONT " + process.ppid]);',
          "});",
          // More events than the pipe holds while nothing reads it.
          'for (let n = 0; n < 2000; n++) test("quick " + n, () => {});',
          'test("reads a FIFO no process writes", () => { readFileSync(process.env.FIXTURE_FIFO ?? ""); }, 600_000);',
        ].join("\n"),
      },
      { FIXTURE_FIFO: fifo() }
    );
    expect(code).toBe(1);
    expect(output).toMatch(/\nerror: flood\.test\.ts: test 2002 of the file .* is still running /);
  },
  ROW_MS
);

test.skipIf(skip)(
  "under GitHub Actions the report is an error annotation on the test file, named from the workspace",
  async () => {
    const workspace = scratch("workspace");
    const dir = path.join(workspace, "package");
    mkdirSync(dir);
    writeFileSync(path.join(dir, "blocked.test.ts"), BLOCKED);
    const { code, output } = await runIn(dir, [process.execPath, script, "./blocked.test.ts"], {
      GITHUB_ACTIONS: "true",
      GITHUB_WORKSPACE: workspace,
      LEGION_TEST_BOUND_MS: String(BOUND_MS),
      FIXTURE_FIFO: fifo(),
    });
    expect(code).toBe(1);
    expect(output).toContain(
      "::error file=package/blocked.test.ts,title=per-test bound::blocked.test.ts: test 1 of the file"
    );
  },
  ROW_MS
);

test(
  "the package's test command runs the bound at its own value",
  async () => {
    const file = path.join(scratch("command"), "quick.test.ts");
    writeFileSync(file, QUICK);
    const { code, output } = await runIn(
      packageDirectory,
      [process.execPath, "run", "test", file],
      {
        LEGION_TEST_BOUND_MS: undefined,
      }
    );
    expect(output).toContain("2 pass");
    expect(output).toMatch(
      /per-test bound: the slowest test was test \d of .*quick\.test\.ts, .*; the bound is 160\.0 s\./
    );
    expect(code).toBe(0);
  },
  ROW_MS
);
