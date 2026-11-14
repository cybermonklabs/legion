import { expect, test } from "bun:test";
import { randomInt } from "node:crypto";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";
import { bounds, rowLimit, runBounded, whether } from "./bounded-subprocess";

const boundedModule = fileURLToPath(new URL("./bounded-subprocess.ts", import.meta.url));

/** The command line of every process on the machine, by pid, with its words joined by spaces; or
 *  undefined where neither `/proc` nor `ps` lists processes. Without `/proc`, one `ps` lists them,
 *  run by `Bun.spawnSync` with no bound, so a `ps` that never ends is never stopped. That is a
 *  stated edge: Linux lists its processes under `/proc` and never runs it. */
function commandLines(): Map<number, string> | undefined {
  const lines = new Map<number, string>();
  if (existsSync("/proc/self/cmdline")) {
    for (const entry of readdirSync("/proc")) {
      if (!/^\d+$/.test(entry)) continue;
      try {
        const words = readFileSync(`/proc/${entry}/cmdline`, "utf8").split("\0");
        lines.set(Number(entry), words.join(" ").trim());
      } catch {
        // The process ended between the directory read and the read of its command line.
      }
    }
    return lines;
  }
  const ps = Bun.spawnSync(["ps", "-A", "-o", "pid=,args="], { stderr: "ignore" });
  if (!ps.success) return undefined;
  for (const line of ps.stdout.toString().split("\n")) {
    const [, pid, args] = /^\s*(\d+)\s+(.*)$/.exec(line) ?? [];
    if (pid !== undefined && args !== undefined) lines.set(Number(pid), args);
  }
  return lines;
}

/** The pid and command line of every process whose command line holds `word`. */
function holding(word: string): [number, string][] {
  return [...(commandLines() ?? new Map<number, string>())].filter(([, line]) =>
    line.includes(word)
  );
}

/** Sends `signal` to `pid`, a process or, when negative, a process group. A process that ended
 *  between the read of the table and this kill answers ESRCH, which is no failure of the row, so
 *  it does not replace the assertion the row is making. */
function kill(pid: number, signal: NodeJS.Signals): void {
  try {
    process.kill(pid, signal);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
  }
}

test.skipIf(commandLines() === undefined)(
  "an interrupted `bun test --parallel` takes every bounded child its workers wait for with it",
  async () => {
    // A number of seconds no other process on the machine sleeps for, so every process whose
    // command line holds it is one of this row's: each `bash` and each `sleep`. It is this row's
    // own limit, so a sleep that survives is still listed at every read the row makes.
    const seconds = `${rowLimit("short", "short", "short") / 1000}.${randomInt(10_000_000, 100_000_000)}`;
    const row =
      `import { test } from "bun:test";\n` +
      `import { rowLimit, runBounded } from ${JSON.stringify(boundedModule)};\n` +
      `test("waits for a bounded child that never ends by itself", async () => {\n` +
      `  await runBounded({ argv: ["bash", "-c", "sleep ${seconds} & wait"], bound: "short", from: import.meta.url });\n` +
      `}, rowLimit("short"));\n`;
    const directory = mkdtempSync(join(tmpdir(), "bounded-subprocess-"));
    const log = join(directory, "runner.log");
    let runner: Subprocess | undefined;
    try {
      writeFileSync(join(directory, "a.test.ts"), row);
      writeFileSync(join(directory, "b.test.ts"), row);
      // This row drives the runner, so it is no bounded child: it leads a process group of its
      // own, as the runner does when a terminal starts it, and that group is what SIGINT is sent to.
      // So an interrupt of the outer run reaches neither the runner nor its workers, and where it
      // ends this run before the `finally` below, they and their `bash` and `sleep` pairs run on
      // until the inner rows' short bound kills each pair, about 60 s after it started, and the
      // runner then exits by itself.
      runner = Bun.spawn(["bun", "test", "--parallel=2", "./a.test.ts", "./b.test.ts"], {
        cwd: directory,
        detached: true,
        stderr: Bun.file(log),
        stdout: Bun.file(log),
      });
      const runnerLog = () => (existsSync(log) ? readFileSync(log, "utf8") : "");
      const sleeping = () =>
        holding(seconds).filter(([, line]) => line.startsWith("sleep ")).length === 2;
      const started = runner;
      const exited = () => started.exitCode !== null || started.signalCode !== null;
      expect(
        await whether(sleeping, exited),
        exited()
          ? `the runner exited before both sleeps started:\n${runnerLog()}`
          : `both sleeps did not start inside the short bound of ${bounds.short} ms:\n${runnerLog()}`
      ).toBe(true);

      kill(-runner.pid, "SIGINT");
      expect(
        await whether(exited),
        `the runner did not exit inside the short bound of ${bounds.short} ms after SIGINT:\n${runnerLog()}`
      ).toBe(true);
      // A worker's `runBounded` writes its cost line to the runner's log when its child ends,
      // with `bound passed` when the child's own short bound ended it. That bound ends about when
      // the wait above does, so were SIGINT to reach no worker, the bound would kill the sleeps
      // and the runner would exit after it, inside that wait; the cost line says which ended them.
      expect(runnerLog(), "a bound, not SIGINT, ended a child").not.toContain("bound passed");

      // A killed process can take a moment to leave the process table; a survivor stays, since
      // its sleep outlasts every wait of this row.
      await whether(() => holding(seconds).length === 0);
      const survivors = holding(seconds);
      for (const [pid] of survivors) kill(pid, "SIGKILL");
      expect(
        survivors.map(([pid, line]) => `${pid} ${line}`),
        "processes a bounded child started outlived the interrupted runner: still listed after " +
          `the short bound of ${bounds.short} ms`
      ).toEqual([]);
    } finally {
      if (runner !== undefined && runner.exitCode === null && runner.signalCode === null) {
        kill(-runner.pid, "SIGKILL");
      }
      for (const [pid] of holding(seconds)) kill(pid, "SIGKILL");
      rmSync(directory, { force: true, recursive: true });
    }
  },
  // This row drives a `bun test --parallel` runner rather than waiting for a bounded child. Its
  // three waits run one after another and the short bound ends each: both sleeps starting, the
  // runner exiting after SIGINT, and every process holding the argument leaving the table.
  rowLimit("short", "short", "short")
);

// The token match reads each process's environment under `/proc`. Where `/proc` is missing,
// `ps` lists no environment, and nothing finds a process a child left once that child exited,
// so this row has nothing to hold there.
test.skipIf(!existsSync("/proc/self/stat"))(
  "a bounded child that exits leaves no process it started behind",
  async () => {
    // A number of seconds no other process on the machine sleeps for, so every process whose
    // command line holds it is the `sleep` this row's child started. It is this row's own limit,
    // so a sleep the sweep missed is still listed at every read the row makes.
    const seconds = `${rowLimit("short", "short") / 1000}.${randomInt(10_000_000, 100_000_000)}`;
    // `setsid` takes the sleep out of this process's group and its output off the pipes, so the
    // child exits at once and only the token of its environment finds the sleep it left.
    await runBounded({
      argv: ["bash", "-c", `setsid sleep ${seconds} >/dev/null 2>&1 &`],
      bound: "short",
      from: import.meta.url,
    });
    // A killed process can take a moment to leave the process table; a survivor stays, since
    // its sleep outlasts every wait of this row.
    await whether(() => holding(seconds).length === 0);
    const survivors = holding(seconds);
    for (const [pid] of survivors) kill(pid, "SIGKILL");
    expect(
      survivors.map(([pid, line]) => `${pid} ${line}`),
      "processes a bounded child started outlived its normal end: still listed after the short " +
        `bound of ${bounds.short} ms`
    ).toEqual([]);
  },
  // The bounded child, then the wait for what it left to leave the table, which the short bound
  // ends.
  rowLimit("short", "short")
);
