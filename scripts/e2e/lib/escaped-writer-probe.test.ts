// Temporary, and a measurement rather than a test: the probe of INFRA-2035. CI's `test` job runs
// it through `bun test scripts/e2e/lib` on an ubuntu-24.04 runner, which is the stock Linux kernel
// the issue pod's gVisor is not. Its numbers are read from that job's log, and the next commit
// deletes this file. It asserts nothing: there is no `expect` here, and a throw inside an arm is
// printed rather than raised, so no reading it takes can turn the job red.
import { describe, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const script = join(import.meta.dir, "app-logins.ts");

/** The row's own escaped descendant: it writes down its pid, then prints one byte a second for
 * thirty seconds. `trap "" PIPE; ` at its head is the committed fix, written here exactly as the
 * row writes it. */
const loop = (trap: boolean, pidFile: string) =>
  `${trap ? 'trap "" PIPE; ' : ""}echo $$ >${pidFile}; ` +
  "i=0; while [ $i -lt 30 ]; do printf x >&2; sleep 1; i=$((i + 1)); done";

/** What one run saw of the writer it started. */
type Run = {
  /** Whether the cleanup's `SIGKILL` reached a running writer, which is what the row requires. */
  alive: boolean;
  /** The wait status the wrapper recorded for the loop. 141 is `SIGPIPE`. */
  status: string;
};

/** The status the wrapper recorded, once it has landed: the wrapper writes it just after the loop
 * ends, which can be a moment after the kill that ended it. The wait is on the real clock because
 * the writer is another process and the kernel's own timing is what this file measures. */
async function recordedStatus(statusFile: string): Promise<string> {
  for (let waited = 0; waited < 20 && !existsSync(statusFile); waited++) await Bun.sleep(50);
  return existsSync(statusFile) ? readFileSync(statusFile, "utf8").trim() : "(none recorded)";
}

/**
 * One run of the row's shape. The helper of app-logins.ts is given a config whose implement token
 * command starts the writer and then exits 3, so the helper fails on the token command and never
 * calls GitHub; `http://127.0.0.1:1/` stands in for the API and is never reached. `setsid` puts
 * the writer in a session of its own, so no process group the helper kills reaches it, and the
 * only reader of the socket it writes into is the helper, which has gone by the time the run
 * returns.
 *
 * The writer runs under a wrapper that writes to neither fd 1 nor fd 2, so the wrapper itself
 * cannot take `SIGPIPE`: it runs the loop in an `sh -c` of its own, which records the loop's pid
 * before the loop starts, and puts the loop's `$?` into a file after it ends. That is how a status
 * the loop died of can still be read once the loop is gone.
 *
 * `waitMs` is the pause between the run's return and the cleanup's kill, on the real clock: it is
 * the quantity each arm varies, not a guess at how long something takes.
 */
async function measure(trap: boolean, waitMs: number): Promise<Run> {
  const dir = mkdtempSync(join(tmpdir(), "infra-2035-probe-"));
  try {
    const pidFile = join(dir, "writer.pid");
    const statusFile = join(dir, "writer.status");
    const wrapper = join(dir, "wrapper.sh");
    writeFileSync(wrapper, `sh -c '${loop(trap, pidFile)}'\necho $? >${statusFile}\n`);
    const config = join(dir, "legion.yaml");
    const token = `setsid sh ${wrapper} >&2 & exit 3`;
    writeFileSync(
      config,
      `project: ACME\ngithub_apps:\n  implement:\n    app_id: "101"\n` +
        `    token_command: ${JSON.stringify(token)}\n` +
        `  review:\n    app_id: "202"\n    token_command: "echo {}"\n`
    );
    const child = Bun.spawn(["bun", script, config, "http://127.0.0.1:1/"], {
      env: { PATH: process.env.PATH ?? "", HOME: dir },
      stdin: new Blob(["stdin-leak"]),
      stdout: "pipe",
      stderr: "pipe",
    });
    await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    if (waitMs > 0) await Bun.sleep(waitMs);
    if (!existsSync(pidFile)) return { alive: false, status: "(no pid file)" };
    const pid = Number.parseInt(readFileSync(pidFile, "utf8").trim(), 10);
    let alive = true;
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      alive = false;
    }
    return { alive, status: await recordedStatus(statusFile) };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

/** `count` runs of one shape, one after another: runs at the same time would load the machine and
 * change the timing this file measures. */
async function arm(count: number, trap: boolean, waitMs: number): Promise<Run[]> {
  const runs: Run[] = [];
  for (let run = 0; run < count; run++) runs.push(await measure(trap, waitMs));
  return runs;
}

/** Prints what one arm saw, on the few lines a person reads in the job log. */
function report(name: string, measuring: string, runs: Run[], verdict: string) {
  const ended = runs.filter((run) => !run.alive);
  const counts = new Map<string, number>();
  for (const run of ended) counts.set(run.status, (counts.get(run.status) ?? 0) + 1);
  console.log(`\n${name}: ${measuring}`);
  console.log(
    `  ${runs.length} runs: the writer was alive at the kill in ${runs.length - ended.length} ` +
      `of them and gone in ${ended.length}`
  );
  if (ended.length > 0) {
    console.log(
      `  the status of each writer that had ended: ${ended.map((r) => r.status).join(" ")}`
    );
    for (const [status, count] of counts) {
      const sigpipe = status === "141" ? " (SIGPIPE)" : "";
      console.log(`    status ${status}${sigpipe}: ${count} of the ${runs.length} runs`);
    }
  }
  console.log(`  verdict: ${verdict}`);
}

/** Runs one arm and prints whatever it throws, so that no arm can fail the job. */
async function printing(name: string, body: () => Promise<void>) {
  try {
    await body();
  } catch (error) {
    const shown = error instanceof Error ? (error.stack ?? error.message) : String(error);
    console.log(`\n${name} threw, and the probe prints it rather than failing: ${shown}`);
  }
}

// Each run takes about 5.1 seconds, the drain bound the helper fails on, plus the arm's own wait.
// The timeouts below are four times what the arm needs, so no arm can end on one.
describe("INFRA-2035 probe: what ends the row's escaped writer", () => {
  test("arm A: no trap, the kill as soon as the run returns", async () => {
    await printing("arm A", async () => {
      const runs = await arm(20, false, 0);
      const gone = runs.filter((run) => !run.alive).length;
      report(
        "arm A",
        "the flake as it stands: the row's descendant without the trap, killed the moment the " +
          "run returns, as the row's own cleanup kills it",
        runs,
        gone === 0
          ? "every writer was still running, so no run of this arm would have failed the row"
          : `${gone} of ${runs.length} runs lost the writer before the kill, and each of those ` +
              "is a run that fails the row"
      );
    });
  }, 420_000);

  test("arm B: no trap, 1.2 seconds before the kill", async () => {
    await printing("arm B", async () => {
      const runs = await arm(10, false, 1_200);
      const sigpipe = runs.filter((run) => !run.alive && run.status === "141").length;
      report(
        "arm B",
        "the cause: the same descendant, given 1.2 seconds to reach its next write before the " +
          "kill, so the race the row loses at random is lost every time",
        runs,
        sigpipe === runs.length
          ? `all ${runs.length} writers ended on SIGPIPE, which is the cause the issue names`
          : `${sigpipe} of ${runs.length} writers ended on SIGPIPE, so this kernel does not ` +
              "raise it on every write into a socket whose reader has gone"
      );
    });
  }, 300_000);

  test("arm C: the committed trap, 1.2 seconds before the kill", async () => {
    await printing("arm C", async () => {
      const runs = await arm(10, true, 1_200);
      const alive = runs.filter((run) => run.alive).length;
      report(
        "arm C",
        'the fix: the descendant with `trap "" PIPE; ` at its head, over the same 1.2 seconds',
        runs,
        alive === runs.length
          ? `all ${runs.length} writers were still running, so the trap holds the writer through ` +
              "the wait that ends an untrapped one"
          : `${alive} of ${runs.length} writers were still running, so the trap does not hold ` +
              `the other ${runs.length - alive}`
      );
    });
  }, 300_000);
});
