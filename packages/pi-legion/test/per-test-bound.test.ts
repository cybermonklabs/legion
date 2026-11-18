import { afterEach, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// The per-test bound (`per-test-bound.ts`), run on small suites of its own: each row starts a
// `bun test` of fixture files with the preload and a bound of a few seconds, and reads what that
// run printed and how it ended. Without the preload, the first two rows' runs never end, and the
// row fails at its own timeout.

const scratch = temporaryDirectories("per-test-bound");
const preload = path.join(import.meta.dir, "per-test-bound.ts");

/** The bound each row's run is given. Its fixtures' quick tests take milliseconds; the margin is
 * for a loaded host starting `bun test` and the watch. */
const BOUND_MS = 5_000;

/** Each row's time: the run's start, the bound, and the watch's kill, with room for a loaded host. */
const ROW_MS = 60_000;

const started: Bun.Subprocess[] = [];
afterEach(() => {
  // A run that outlives its row, as every blocked one does without the preload, ends here.
  for (const child of started.splice(0)) child.kill("SIGKILL");
});

/** Runs `bun test` over `files`, each written into a directory of its own, in the order given,
 * with the preload and {@link BOUND_MS}; answers how the run ended and all it printed. */
async function run(files: Record<string, string>, env: Record<string, string> = {}) {
  const dir = scratch("run");
  for (const [name, source] of Object.entries(files)) writeFileSync(path.join(dir, name), source);
  const { GITHUB_ACTIONS: _, ...inherited } = process.env;
  const child = Bun.spawn(
    [
      process.execPath,
      "test",
      "--preload",
      preload,
      ...Object.keys(files).map((name) => `./${name}`),
    ],
    {
      cwd: dir,
      env: { ...inherited, ...env, LEGION_TEST_BOUND_MS: String(BOUND_MS) },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  started.push(child);
  const [stdout, stderr] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code: child.exitCode, signal: child.signalCode, dir, output: stdout + stderr };
}

test(
  "a test that blocks the thread Bun's timers run on is named, its file and place, once it runs past the bound",
  async () => {
    const dir = scratch("fifo");
    const fifo = path.join(dir, "nobody-writes");
    expect(Bun.spawnSync(["mkfifo", fifo]).exitCode).toBe(0);
    const { code, signal, output } = await run(
      {
        "blocked.test.ts": [
          'import { readFileSync } from "node:fs";',
          'import { test } from "bun:test";',
          'test("ends at once", () => {});',
          // Bun's own 10-minute bound never fires: its timer runs on the thread the read holds.
          'test("reads a FIFO no process writes", () => { readFileSync(process.env.FIXTURE_FIFO ?? ""); }, 600_000);',
        ].join("\n"),
      },
      { FIXTURE_FIFO: fifo }
    );
    expect({ code, signal }).toEqual({ code: null, signal: "SIGKILL" });
    expect(output).toContain(
      "error: blocked.test.ts: test 2 of the file (counting from 1 the tests that ran in it) is still running 5 s after it began; the per-test bound is 5 s (test/per-test-bound.ts), and bun test is killed."
    );
  },
  ROW_MS
);

test(
  "a test file whose module body never settles is named by the end of the file before it",
  async () => {
    const { code, signal, output } = await run({
      "first.test.ts": ['import { test } from "bun:test";', 'test("ends at once", () => {});'].join(
        "\n"
      ),
      // A module body is no test: Bun's per-test timeout never starts for it.
      "stuck.test.ts": [
        'import { test } from "bun:test";',
        "await Promise.withResolvers<void>().promise;",
        'test("never reached", () => {});',
      ].join("\n"),
    });
    expect({ code, signal }).toEqual({ code: null, signal: "SIGKILL" });
    expect(output).toContain(
      "error: first.test.ts: no test began 5 s after test 1 of the file ended (its hooks, or the next file's module body and beforeAll)"
    );
  },
  ROW_MS
);

test(
  "a test Bun's own timeout ends inside the bound is Bun's to report, and the watch names the slowest test",
  async () => {
    const { code, signal, output } = await run({
      "settles.test.ts": [
        'import { test } from "bun:test";',
        'test("ends at once", () => {});',
        'test("waits on a promise that never settles", () => Promise.withResolvers<void>().promise, 1_000);',
      ].join("\n"),
    });
    expect({ code, signal }).toEqual({ code: 1, signal: null });
    expect(output).toContain("this test timed out after 1000ms");
    expect(output).not.toContain("the per-test bound is");
    expect(output).toMatch(
      /per-test bound: the slowest test was test 2 of settles\.test\.ts, 1 s of the 5 s bound/
    );
  },
  ROW_MS
);
