import { expect, test } from "bun:test";
import { randomInt } from "node:crypto";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";

const boundedModule = fileURLToPath(new URL("./bounded-subprocess.ts", import.meta.url));

/** The command line of every process on the machine, by pid, with its words joined by spaces; or
 *  undefined where neither `/proc` nor `ps` lists processes. */
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

/** Calls `read` every 100 ms until it answers true or `ms` has passed, and answers its last
 *  answer. A process another process starts or ends raises no event this row can await, so the
 *  row reads the process table until the state it waits for is there. */
async function within(ms: number, read: () => boolean): Promise<boolean> {
  const deadline = performance.now() + ms;
  while (!read()) {
    if (performance.now() > deadline) return false;
    await Bun.sleep(100);
  }
  return true;
}

test.skipIf(commandLines() === undefined)(
  "an interrupted `bun test --parallel` takes every bounded child its workers wait for with it",
  async () => {
    // A number of seconds no other process on the machine sleeps for, so every process whose
    // command line holds it is one of this row's: each `bash` and each `sleep`.
    const seconds = `29.${randomInt(10_000_000, 100_000_000)}`;
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
      runner = Bun.spawn(["bun", "test", "--parallel=2", "./a.test.ts", "./b.test.ts"], {
        cwd: directory,
        detached: true,
        stderr: Bun.file(log),
        stdout: Bun.file(log),
      });
      const runnerLog = () => (existsSync(log) ? readFileSync(log, "utf8") : "");
      const sleeping = () =>
        holding(seconds).filter(([, line]) => line.startsWith("sleep ")).length === 2;
      expect(await within(60_000, sleeping), `both sleeps never started:\n${runnerLog()}`).toBe(
        true
      );

      process.kill(-runner.pid, "SIGINT");
      const exited = await Promise.race([runner.exited.then(() => true), Bun.sleep(30_000)]);
      expect(exited, `the runner did not exit within 30 s of SIGINT:\n${runnerLog()}`).toBe(true);

      // A killed process can take a moment to leave the process table; a survivor stays.
      await within(5_000, () => holding(seconds).length === 0);
      const survivors = holding(seconds);
      for (const [pid] of survivors) process.kill(pid, "SIGKILL");
      expect(
        survivors.map(([pid, line]) => `${pid} ${line}`),
        "processes a bounded child started outlived the interrupted runner"
      ).toEqual([]);
    } finally {
      if (runner !== undefined && runner.exitCode === null && runner.signalCode === null) {
        process.kill(-runner.pid, "SIGKILL");
      }
      for (const [pid] of holding(seconds)) process.kill(pid, "SIGKILL");
      rmSync(directory, { force: true, recursive: true });
    }
  },
  // This row drives a `bun test --parallel` runner rather than waiting for a bounded child: up to
  // 60 s for both sleeps to start, 30 s for the runner to exit, 5 s for survivors to go, and room.
  120_000
);
