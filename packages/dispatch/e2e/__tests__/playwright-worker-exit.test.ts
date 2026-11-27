/** The two runner defects patches/playwright@1.64.1.patch fixes, and the guard it keeps.
 *
 *  Playwright 1.64.1's runner removes a stopped worker's artifacts directory
 *  (`.playwright-artifacts-<n>`, which holds every trace file the worker's rows recorded) before
 *  it sends the 'exit' the worker's stop waits for, and that stop judges the wait by the worker's
 *  heartbeats, which end when the worker exits. On a disk where each unlink waits, the removal of
 *  a worker that ran a hundred rows took minutes, and the runner printed "worker-<n> process did
 *  not exit within 300000ms after stop, force-killed it" for a worker that had exited in under a
 *  second. The patch sends 'exit' when the process exits, removes the directory afterwards in the
 *  background, and stops applying the deadline once the process has exited.
 *
 *  Each row runs `fixtures/worker-exit.config.ts`, one worker and one row with no browser, with
 *  the deadline at 2 s (`PWTEST_CHILD_PROCESS_TIMEOUT`, Playwright's own setting for it). The
 *  first holds the runner's removal of the artifacts directory 6 s
 *  (`fixtures/hold-artifacts-removal.cjs`); without the patch that run fails with the line above.
 *  The second holds the worker's own main thread 8 s after its stop: a worker that really has not
 *  exited, which the runner must still kill and report, patch or no patch. */

import { expect, test } from "bun:test";
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const config = fileURLToPath(new URL("./fixtures/worker-exit.config.ts", import.meta.url));
const holdRemoval = fileURLToPath(
  new URL("./fixtures/hold-artifacts-removal.cjs", import.meta.url)
);
const deadlineMs = 2_000;

/** One run of the fixture config with `env` added, and the artifacts directories its output
 *  directory still holds once the run has ended. */
async function workerExitRun(env: Record<string, string>) {
  const output = mkdtempSync(join(tmpdir(), "worker-exit-"));
  try {
    const run = await runBounded({
      argv: ["bunx", "playwright", "test", "--config", config],
      bound: "short",
      cwd: dispatchPackage,
      env: {
        ...process.env,
        CI: undefined,
        // `bun test --parallel`, which the package's test script runs, sets JEST_WORKER_ID in its
        // workers, and Playwright refuses to declare a row while it is set ("Playwright Test needs
        // to be invoked via 'npx playwright test' and excluded from Jest test runs").
        JEST_WORKER_ID: undefined,
        PWTEST_CHILD_PROCESS_TIMEOUT: String(deadlineMs),
        WORKER_EXIT_OUTPUT: output,
        ...env,
      },
      from: import.meta.url,
    });
    const left = readdirSync(output).filter((name) => name.startsWith(".playwright-artifacts-"));
    return { ...run, left };
  } finally {
    rmSync(output, { force: true, recursive: true });
  }
}

test(
  "a worker whose artifacts the runner is still removing has exited, and the run passes",
  async () => {
    const run = await workerExitRun({
      NODE_OPTIONS: `--require ${holdRemoval}`,
      WORKER_EXIT_HOLD_REMOVAL_MS: "6000",
    });
    // The hold must have held a removal, or the run proves nothing about one.
    expect(run.stderr + run.stdout).toContain("worker-exit hold: ");
    expect(run.stdout + run.stderr).not.toContain("did not exit within");
    expect(run.stdout).toContain("1 passed");
    expect(run.code).toBe(0);
    // The removal ran in the background and the run waited for it before it ended.
    expect(run.left).toEqual([]);
  },
  rowLimit("short")
);

test(
  "a worker that has not exited after its stop is still killed and reported",
  async () => {
    const run = await workerExitRun({ WORKER_EXIT_HOLD_MAIN_MS: "8000" });
    expect(run.stdout + run.stderr).toContain(
      `worker-0 process did not exit within ${deadlineMs}ms after stop, force-killed it`
    );
    expect(run.code).not.toBe(0);
  },
  rowLimit("short")
);
