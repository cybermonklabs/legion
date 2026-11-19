import { expect, test } from "bun:test";
import { randomInt } from "node:crypto";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";
import {
  bounds,
  execPause,
  type Listed,
  listedOf,
  masked,
  maxReads,
  named,
  rowLimit,
  runBounded,
  stopTree,
  whether,
} from "./bounded-subprocess";

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

/** The pid, start and token of the child of every scripted kill below. A table lists its pid
 *  only in the rows about that pid, and lists this process only in the row about this process,
 *  which a kill never takes. */
const scriptedChild = { pid: process.pid + 1, start: 5_000, token: "the-scripted-child's-token" };

/** The tokens of three other bounded children, which a scripted process carries beside the
 *  child's or in its place. `runBounded` writes a child's tokens the outermost first, so the
 *  outer child's, of a bounded child the scripted child runs inside, comes before the child's;
 *  the inner child's, of a bounded child the scripted child started, comes after it; and the
 *  sibling's, of another bounded child the outer child started, comes after the outer child's,
 *  where the child's would stand. */
const outerToken = "an-outer-child's-token";
const innerToken = "an-inner-child's-token";
const siblingToken = "a-sibling-child's-token";

/** How the child stands when a scripted kill runs, as a `Subprocess` reports it. */
type ChildState = Pick<Subprocess, "exitCode" | "signalCode">;

/** A child still running, as the child of a bound that fired is. */
const stillRunning: ChildState = { exitCode: null, signalCode: null };

/** A child that has exited, as the setsid row's child has by the time its kill runs. */
const exitedByCode: ChildState = { exitCode: 0, signalCode: null };

/** A child a signal ended. */
const endedBySignal: ChildState = { exitCode: null, signalCode: "SIGTERM" };

/** A process every table of `scriptedKill` read from `/proc` lists beside `sibling` and the
 *  processes the row is about: pid 1, which carries no token and whose environment reads whole. A
 *  table lists its processes in no set order, and across the rows the process a row is about
 *  stands first, last and between others, so a kill that read only one end of a table fails a
 *  row. */
const init: Listed = { pid: 1, ppid: 0, start: 1, tokens: [], unknown: false };

/** A process of the sibling, which every table of `scriptedKill` read from `/proc` lists beside
 *  `init`: it started since the child, its environment reads whole, and it carries the outer
 *  child's token and the sibling's but not the child's. No kill of the child takes it, a kill that
 *  took every process with a token would, and it is no reason to read again. */
const sibling: Listed = {
  pid: process.pid + 5,
  ppid: 1,
  start: scriptedChild.start + 1,
  tokens: [outerToken, siblingToken],
  unknown: false,
};

/** `stopTree` of `scriptedChild`, standing as `child` says, driven over `tables`, a scripted
 *  process table: read n answers `tables[n - 1]`, and each read after the last answers the last
 *  again. `procfs` tells the kill whether its table comes from `/proc`, so a row runs either path
 *  whatever this machine has, and the signal of a pid in `gone` throws, as `process.kill` throws
 *  for a process that has ended. It answers what the kill did, in order: each call of `read`,
 *  `pause`, `signal` and `child.kill`, written as the stand-in's name and then every argument it
 *  was given. So `read` and `pause`, which are given none, write their name alone, a signal
 *  writes `signal <pid>`, and the kill of a running child writes `child.kill SIGKILL`. No clock
 *  runs and no process is started or signalled: the pause, the signal and `child.kill` only
 *  record, and act on no process. Its loop is synchronous, so a loop that lost its `maxReads`
 *  bound would spin past every row clock; a read past `maxReads` throws instead. */
function scriptedKill(
  tables: readonly (readonly Listed[] | undefined)[],
  {
    child = exitedByCode,
    gone = [],
    procfs = true,
  }: { child?: ChildState; gone?: readonly number[]; procfs?: boolean } = {}
): string[] {
  const did: string[] = [];
  // A call of the stand-in `name`, given `args`: its name, then each argument.
  const record = (name: string, args: readonly unknown[]): void => {
    did.push([name, ...args].map(String).join(" "));
  };
  let reads = 0;
  stopTree(
    {
      child: {
        ...child,
        kill: (...args: unknown[]) => record("child.kill", args),
        pid: scriptedChild.pid,
      },
      start: scriptedChild.start,
      token: scriptedChild.token,
    },
    {
      pause: (...args: unknown[]) => record("pause", args),
      procfs,
      read: (...args: unknown[]) => {
        reads += 1;
        if (reads > maxReads) {
          throw new Error(`the kill read the table ${reads} times, past maxReads of ${maxReads}`);
        }
        record("read", args);
        return tables[Math.min(reads, tables.length) - 1];
      },
      signal: (pid: number, ...more: unknown[]) => {
        record("signal", [pid, ...more]);
        if (gone.includes(pid)) {
          throw Object.assign(new Error(`kill ${pid}: no such process`), { code: "ESRCH" });
        }
      },
    }
  );
  return did;
}

test("a kill reads again, after a pause, while a process started since the child reads unknown", () => {
  // One of the child's processes, started in the child's own clock tick, which `start` counts as
  // since the child, and caught in its exec at read 1, its environment read empty; carrying the
  // child's token at read 2, once the exec has loaded, after the outer child's, as the processes
  // of a child started inside another bounded child do; and still listed at read 3, killed
  // already, beside nothing new. It stands between `sibling` and `init` at every read.
  const pid = process.pid + 2;
  const execing: Listed = { pid, ppid: 1, start: scriptedChild.start, tokens: [], unknown: true };
  const loaded: Listed = {
    ...execing,
    tokens: [outerToken, scriptedChild.token],
    unknown: false,
  };
  expect(
    scriptedKill([
      [sibling, execing, init],
      [sibling, loaded, init],
      [sibling, loaded, init],
    ])
  ).toEqual(["read", "pause", "read", `signal ${pid}`, "read"]);
});

test("an unknown process that started before the child ends a kill at its first read", () => {
  const older: Listed = {
    pid: process.pid + 2,
    ppid: 1,
    start: scriptedChild.start - 1,
    tokens: [],
    unknown: true,
  };
  expect(scriptedKill([[init, older, sibling]])).toEqual(["read"]);
});

test("a process started since the child that stays unknown ends a kill's reads at maxReads", () => {
  // Beside it stands an unknown process that started before the child, which alone ends the
  // reads at once (the row above): last in one table and first in the other, the stuck process
  // at the other end. So a kill that read only one end of the table, or only its first or its
  // last unknown process, ends at its first read. A pause follows every read, the last as well.
  const stuck: Listed = {
    pid: process.pid + 2,
    ppid: 1,
    start: scriptedChild.start + 1,
    tokens: [],
    unknown: true,
  };
  const older: Listed = { ...stuck, pid: process.pid + 3, start: scriptedChild.start - 1 };
  const everyRead = Array.from({ length: maxReads }, () => ["read", "pause"]).flat();
  expect(scriptedKill([[stuck, init, sibling, older]]), "the stuck process first").toEqual(
    everyRead
  );
  expect(scriptedKill([[older, sibling, init, stuck]]), "the stuck process last").toEqual(
    everyRead
  );
});

test("an unknown process the kill signalled already ends its reads", () => {
  // A process of a bounded child that the child started, which carries at read 1 the child's
  // token, then that inner child's own, and is exiting at read 2, its memory gone, so its
  // environment reads empty.
  const pid = process.pid + 2;
  const carrying: Listed = {
    pid,
    ppid: 1,
    start: scriptedChild.start + 1,
    tokens: [scriptedChild.token, innerToken],
    unknown: false,
  };
  const exiting: Listed = { ...carrying, tokens: [], unknown: true };
  expect(
    scriptedKill([
      [carrying, init, sibling],
      [exiting, init, sibling],
    ])
  ).toEqual(["read", `signal ${pid}`, "read"]);
});

test("a kill takes a running child through child.kill, and what it started by parent pid", () => {
  // The child, caught in an exec at read 1, its environment read empty, and two processes it
  // started that took the tokens' variable out of their environment, as `env -u` does, so the
  // child's pid alone finds each: the first in the table, and the last, which ended already.
  // Each is signalled, the one that ended as well, and all three are gone at read 2. The child
  // gets SIGKILL, which no process can trap: the `bun --eval` of `e2e/global-setup.ts` that
  // `second-global-setup.test.ts` bounds traps SIGTERM (`e2e/harness-pool.ts`), and ends only
  // once its listener has stopped its stacks, or never where that listener cannot run, while
  // the kill signals the child once.
  const [left, ended] = [process.pid + 2, process.pid + 3];
  const child: Listed = {
    pid: scriptedChild.pid,
    ppid: process.pid,
    start: scriptedChild.start,
    tokens: [],
    unknown: true,
  };
  const below = (pid: number): Listed => ({
    pid,
    ppid: scriptedChild.pid,
    start: scriptedChild.start + 1,
    tokens: [],
    unknown: false,
  });
  expect(
    scriptedKill(
      [
        [below(left), init, child, sibling, below(ended)],
        [init, sibling],
      ],
      { child: stillRunning, gone: [ended] }
    )
  ).toEqual(["read", "child.kill SIGKILL", `signal ${ended}`, `signal ${left}`, "read"]);
});

test("a child that has ended is found by its token alone, though a process holds its pid", () => {
  // Once the runtime reaped the child, one of the child's own processes took its pid; it carries
  // the token, and the kill signals it as it would any other. The child itself gets nothing. The
  // process belongs to a bounded child that the child started, where the child runs inside
  // another, so its tokens hold the child's between the outer child's and the inner child's.
  const holder: Listed = {
    pid: scriptedChild.pid,
    ppid: 1,
    start: scriptedChild.start + 1,
    tokens: [outerToken, scriptedChild.token, innerToken],
    unknown: false,
  };
  for (const [child, how] of [
    [exitedByCode, "exited 0"],
    [endedBySignal, "ended by SIGTERM"],
  ] as const) {
    expect(scriptedKill([[init, sibling, holder]], { child }), `a child that ${how}`).toEqual([
      "read",
      `signal ${scriptedChild.pid}`,
      "read",
    ]);
  }
});

test("a kill takes each process whose tokens hold the child's, first, last or in the middle", () => {
  // `runBounded` writes the tokens of every bounded child a process runs inside, the outermost
  // first, so the child's token stands anywhere in a list: last in the child's own processes
  // where the child runs inside another bounded child, first in those of a bounded child the
  // child started, and in the middle in those of that inner child where the child runs inside
  // another. The table lists one of each, first, in the middle and last; `sibling`, which
  // carries the outer child's token and not the child's, is not taken.
  const [own, ofInner, ofBoth] = [process.pid + 2, process.pid + 3, process.pid + 4];
  const carrying = (pid: number, tokens: readonly string[]): Listed => ({
    pid,
    ppid: 1,
    start: scriptedChild.start + 1,
    tokens,
    unknown: false,
  });
  expect(
    scriptedKill([
      [
        carrying(own, [outerToken, scriptedChild.token]),
        init,
        carrying(ofInner, [scriptedChild.token, innerToken]),
        sibling,
        carrying(ofBoth, [outerToken, scriptedChild.token, innerToken]),
      ],
    ])
  ).toEqual(["read", `signal ${ofBoth}`, `signal ${ofInner}`, `signal ${own}`, "read"]);
});

test("where `/proc` is missing, a kill reads once, and not at all for a child that ended", () => {
  // `ps` lists a pid and its parent, and no token, no start and nothing unknown. The process the
  // child started stands between pid 1 and the child.
  const below = process.pid + 2;
  const psListed = (pid: number, ppid: number): Listed => ({
    pid,
    ppid,
    start: 0,
    tokens: [],
    unknown: false,
  });
  const table = [
    psListed(1, 0),
    psListed(below, scriptedChild.pid),
    psListed(scriptedChild.pid, process.pid),
  ];
  expect(scriptedKill([table], { child: stillRunning, procfs: false })).toEqual([
    "read",
    "child.kill SIGKILL",
    `signal ${below}`,
  ]);
  // A `ps` that fails lists nothing, and the running child is killed all the same.
  expect(scriptedKill([undefined], { child: stillRunning, procfs: false })).toEqual([
    "read",
    "child.kill SIGKILL",
  ]);
  expect(scriptedKill([table], { procfs: false })).toEqual([]);
});

test("a kill never signals this process, even where its table reads it as one of the child's", () => {
  const self: Listed = {
    pid: process.pid,
    ppid: 1,
    start: scriptedChild.start + 1,
    tokens: [outerToken, scriptedChild.token],
    unknown: false,
  };
  expect(scriptedKill([[self, init, sibling]])).toEqual(["read"]);
});

test("a kill with its own signal and pause sends SIGKILL through process.kill and waits execPause", () => {
  // Every scripted row above hands `stopTree` a stand-in `signal` and `pause`, so none of them
  // reaches the defaults, which call `process.kill` and `Bun.sleepSync`. The setsid row above
  // reaches them through `runBounded`, but its `sleep` dies of SIGTERM as of SIGKILL. This row
  // hands `read` and `procfs` alone, and stands in for those two globals while the kill runs,
  // each recording every argument it is given. Its table is that of the first scripted row: a
  // process started since the child, unknown at read 1 and carrying the child's token at read
  // 2, here between an unknown process older than the child and a process of the sibling. Every
  // pid its table lists is above 2 ** 22, Linux's `PID_MAX_LIMIT` and the most
  // `/proc/sys/kernel/pid_max` can hold, so no process holds one, and a stand-in that failed to
  // take its global's place would signal nothing real.
  const pid = 2 ** 30 + 2;
  const older: Listed = {
    pid: 2 ** 30 + 1,
    ppid: 1,
    start: scriptedChild.start - 1,
    tokens: [],
    unknown: true,
  };
  const execing: Listed = { pid, ppid: 1, start: scriptedChild.start, tokens: [], unknown: true };
  const loaded: Listed = {
    ...execing,
    tokens: [outerToken, scriptedChild.token],
    unknown: false,
  };
  const ofSibling: Listed = { ...sibling, pid: 2 ** 30 + 3 };
  const tables = [
    [older, execing, ofSibling],
    [older, loaded, ofSibling],
  ];
  const childKills: unknown[][] = [];
  const reads: unknown[][] = [];
  const kills: unknown[][] = [];
  const sleeps: unknown[][] = [];
  const realKill = process.kill;
  const realSleep = Bun.sleepSync;
  try {
    Object.assign(process, {
      kill: (...args: unknown[]) => {
        kills.push(args);
        return true;
      },
    });
    Object.assign(Bun, { sleepSync: (...args: unknown[]) => sleeps.push(args) });
    expect(process.kill, "process.kill is not the stand-in").not.toBe(realKill);
    expect(Bun.sleepSync, "Bun.sleepSync is not the stand-in").not.toBe(realSleep);
    stopTree(
      {
        child: {
          ...exitedByCode,
          kill: (...args: unknown[]) => childKills.push(args),
          pid: scriptedChild.pid,
        },
        start: scriptedChild.start,
        token: scriptedChild.token,
      },
      {
        procfs: true,
        read: (...args: unknown[]) => {
          reads.push(args);
          if (reads.length > maxReads) {
            throw new Error(`the kill read the table ${reads.length} times, past maxReads`);
          }
          return tables[Math.min(reads.length, tables.length) - 1];
        },
      }
    );
  } finally {
    Object.assign(process, { kill: realKill });
    Object.assign(Bun, { sleepSync: realSleep });
  }
  expect({ childKills, reads, kills, sleeps }).toEqual({
    childKills: [],
    reads: [[], [], []],
    kills: [[pid, "SIGKILL"]],
    sleeps: [[execPause]],
  });
});

/** The pid, parent and start time of the process the rows of `listedOf` below read. */
const readProcess = { pid: 4_242, ppid: 4_200, start: 9_183_936 };

/** The flags, field 9 of `/proc/<pid>/stat`, of an ordinary process: `PF_RANDOMIZE` and
 *  `PF_SUPERPRIV`, and not `PF_KTHREAD`, 0x00200000, so only that bit tells a kernel thread. */
const ordinaryFlags = 0x00400100;

/** The text Linux gives for the `/proc/<pid>/stat` of `readProcess`, named `name`, in `state`,
 *  with `flags`: its pid, its name in parentheses, and fields 3 to 52 of proc(5), each 0 but the
 *  state, the parent, the flags and the start time. So each of the four, read one place off,
 *  reads something other than its own value. */
function statText({ flags = ordinaryFlags, name = "sleep", state = "R" } = {}): string {
  const fields: Record<number, string> = {
    1: String(readProcess.pid),
    2: `(${name})`,
    3: state,
    4: String(readProcess.ppid),
    9: String(flags),
    22: String(readProcess.start),
  };
  return `${Array.from({ length: 52 }, (_, index) => fields[index + 1] ?? "0").join(" ")}\n`;
}

/** The text of a `/proc/<pid>/environ` holding `variables`, each one ended by a NUL. */
function environText(...variables: string[]): string {
  return variables.map((variable) => `${variable}\0`).join("");
}

/** What the table lists of `readProcess`, in state R and no kernel thread, whose environment
 *  holds `variables`. */
function listedWith(...variables: string[]): Listed | undefined {
  return listedOf(readProcess.pid, statText(), environText(...variables));
}

test("the table lists a process whose environment reads empty, in state R and no kernel thread, as unknown", () => {
  // As a process in the middle of its exec reads, or one that is exiting, or one `env -i` started.
  expect(listedOf(readProcess.pid, statText(), "")).toEqual({
    ...readProcess,
    tokens: [],
    unknown: true,
  });
});

test("the table lists a zombie whose environment reads empty as not unknown", () => {
  expect(listedOf(readProcess.pid, statText({ state: "Z" }), "")).toEqual({
    ...readProcess,
    tokens: [],
    unknown: false,
  });
});

test("the table lists a kernel thread, told by its flags, whose environment reads empty as not unknown", () => {
  // A kworker's flags, `PF_KTHREAD` among them, in a state other than `Z`, so the flag alone
  // tells it.
  const kworker = statText({ flags: 0x04208060, name: "kworker/0:1", state: "I" });
  expect(listedOf(readProcess.pid, kworker, "")).toEqual({
    ...readProcess,
    tokens: [],
    unknown: false,
  });
});

test("the table lists the tokens of a process's environment in their order, split on spaces", () => {
  // A child started inside another bounded child carries the outer token, then its own, and one
  // started two deep carries three, the outermost first; the list keeps that order. The tokens'
  // variable stands first, in the middle and last among the variables.
  const tokens = (value: string) => `DISPATCH_E2E_BOUNDED_TOKENS=${value}`;
  expect({
    first: listedWith(tokens("outer inner"), "HOME=/root", "PATH=/usr/bin"),
    middle: listedWith("HOME=/root", tokens("outer middle inner"), "PATH=/usr/bin"),
    last: listedWith("HOME=/root", "PATH=/usr/bin", tokens("outer inner")),
  }).toEqual({
    first: { ...readProcess, tokens: ["outer", "inner"], unknown: false },
    middle: { ...readProcess, tokens: ["outer", "middle", "inner"], unknown: false },
    last: { ...readProcess, tokens: ["outer", "inner"], unknown: false },
  });
});

test("the table lists no token for an environment whose variables only hold the tokens' name", () => {
  // One variable's value holds the tokens' variable and its `=`, and another's name starts with
  // that variable's: neither is the entry for it. Each stands first, in the middle and last.
  const note = "NOTE=DISPATCH_E2E_BOUNDED_TOKENS=outer";
  const saved = "DISPATCH_E2E_BOUNDED_TOKENS_SAVED=inner";
  const home = "HOME=/root";
  const none = { ...readProcess, tokens: [], unknown: false };
  expect([
    listedWith(note, saved, home),
    listedWith(saved, home, note),
    listedWith(home, note, saved),
  ]).toEqual([none, none, none]);
});

test("the table lists a process whose environment could not be read with no token, and not as unknown", () => {
  // Another user's, or that of a process that ended after its stat was read. It is in state R
  // and no kernel thread, so the failed read alone decides.
  expect(listedOf(readProcess.pid, statText(), undefined)).toEqual({
    ...readProcess,
    tokens: [],
    unknown: false,
  });
});

test("the table lists no process whose stat could not be read", () => {
  expect(listedOf(readProcess.pid, undefined, undefined)).toBeUndefined();
});

test("the table lists the parent and start of a process whose command name holds spaces and `)`", () => {
  // A command name is the program's file name, or one the process set, and may read as fields:
  // read from its first `)`, this one would list a zombie whose parent is pid 1.
  expect(listedOf(readProcess.pid, statText({ name: "x) Z 1 (y" }), "")).toEqual({
    ...readProcess,
    tokens: [],
    unknown: true,
  });
});

// Were the walk's cycle guard, the `found.has` of `treeOf`, to regress, the walk would take this
// loop for ever and the row would hang: no row clock stops a synchronous loop, so only the job's
// limit would end it. The rows run in random order (packages/dispatch/bunfig.toml), so the rows
// of this file that had not run by then would not report either.
test("a kill's walk by parent pid ends where its table lists two processes as each other's parent", () => {
  // `processTable` reads one process at a time, so where a pid is reused between two of its
  // reads, a table can list a process as the parent of its own parent.
  const [one, other] = [process.pid + 2, process.pid + 3];
  const table: Listed[] = [
    init,
    {
      pid: one,
      ppid: other,
      start: scriptedChild.start + 1,
      tokens: [scriptedChild.token, innerToken],
      unknown: false,
    },
    sibling,
    { pid: other, ppid: one, start: scriptedChild.start + 2, tokens: [], unknown: false },
  ];
  expect(scriptedKill([table])).toEqual(["read", `signal ${one}`, `signal ${other}`, "read"]);
});

test("a child's name hides each password libpq reads and keeps the rest", () => {
  // Each URL that carries a password is built from its parts, as replace-blocks-guard.test.ts
  // builds its own, so this source holds no `user:password@host` literal for a credential scanner
  // to stop on. Every password here is made up, and the forms include each one that file gives
  // its driver.
  const withPassword = (password: string) =>
    `postgres://legion_test:${password}@127.0.0.1:5433/mine`;
  const hiddenUrl = withPassword("***");
  const forms: Record<string, readonly [text: string, shown: string]> = {
    "a URL password holding a slash": [withPassword("abc/def+ghi"), hiddenUrl],
    "a URL password holding a question mark": [withPassword("abc?def"), hiddenUrl],
    "a URL password holding a hash": [withPassword("abc#def"), hiddenUrl],
    "a URL password holding an at sign": [withPassword("ab@cd"), hiddenUrl],
    "a URL password holding a query": [withPassword("x?a=b&dbname=mine&"), hiddenUrl],
    "the keyword form": [
      "host=127.0.0.1 port=5433 password=sekrit dbname=mine",
      "host=127.0.0.1 port=5433 password=*** dbname=mine",
    ],
    "the keyword form with a single-quoted password": [
      "host=127.0.0.1 password='se kr it' dbname=mine",
      "host=127.0.0.1 password=*** dbname=mine",
    ],
    "the keyword form with the keyword in capitals": [
      "host=127.0.0.1 PASSWORD=sekrit dbname=mine",
      "host=127.0.0.1 PASSWORD=*** dbname=mine",
    ],
    "the keyword form with whitespace on both sides of its =": [
      "host=127.0.0.1 password = sekrit dbname=mine",
      "host=127.0.0.1 password = *** dbname=mine",
    ],
    "the keyword form with whitespace after its =": [
      "host=127.0.0.1 password= sekrit dbname=mine",
      "host=127.0.0.1 password= *** dbname=mine",
    ],
    "the keyword form across lines": [
      "host=127.0.0.1\npassword=sekrit\ndbname=mine",
      "host=127.0.0.1\npassword=***\ndbname=mine",
    ],
    "the keyword form as a variable's value": [
      "DATABASE_URL=password=sekrit host=127.0.0.1",
      "DATABASE_URL=password=*** host=127.0.0.1",
    ],
    "the keyword form inside JSON": [
      '{"dsn":"password=sekrit","other":"dbname=mine"}',
      '{"dsn":"password=***","other":"dbname=mine"}',
    ],
    "a password parameter spelled with a capital letter": [
      "postgres://127.0.0.1:5433/?Password=sekrit&db%6Eame=mine",
      "postgres://127.0.0.1:5433/?Password=***&db%6Eame=mine",
    ],
    "a password parameter whose name escapes a capital letter": [
      "postgres://127.0.0.1:5433/?pass%57ord=sekrit&db%6Eame=mine",
      "postgres://127.0.0.1:5433/?pass%57ord=***&db%6Eame=mine",
    ],
    "the oauth_client_secret keyword": [
      "host=127.0.0.1 oauth_client_secret=sekrit dbname=mine",
      "host=127.0.0.1 oauth_client_secret=*** dbname=mine",
    ],
    "an oauth_client_secret parameter whose name escapes an underscore": [
      "postgres://127.0.0.1:5433/?oauth%5Fclient_secret=sekrit&dbname=mine",
      "postgres://127.0.0.1:5433/?oauth%5Fclient_secret=***&dbname=mine",
    ],
    "a scram_client_key query parameter": [
      "postgres://127.0.0.1:5433/?scram_client_key=sekrit&dbname=mine",
      "postgres://127.0.0.1:5433/?scram_client_key=***&dbname=mine",
    ],
    "the scram_server_key keyword in capitals": [
      "host=127.0.0.1 SCRAM_SERVER_KEY=sekrit dbname=mine",
      "host=127.0.0.1 SCRAM_SERVER_KEY=*** dbname=mine",
    ],
    "a URL with no password": [
      "postgres://127.0.0.1:5433/mine?sslmode=disable",
      "postgres://127.0.0.1:5433/mine?sslmode=disable",
    ],
    "a URL with a user and no password": [
      "postgres://legion_test@127.0.0.1:5433/mine",
      "postgres://legion_test@127.0.0.1:5433/mine",
    ],
    // Over-masked on purpose: a password may hold `/` or `?`, so an `@` later in the word reads
    // as the end of one, and the port and the path go with it.
    "a URL with no password and an at sign in its query": [
      "postgres://127.0.0.1:5433/mine?application_name=a@b",
      "postgres://127.0.0.1:***@b",
    ],
    "keys that are not password": [
      "host=127.0.0.1 passwords=x mypassword=x dbname=mine",
      "host=127.0.0.1 passwords=x mypassword=x dbname=mine",
    ],
    "keys that are not oauth_client_secret or scram_client_key": [
      "host=127.0.0.1 my_oauth_client_secret=x scram_client_keys=x dbname=mine",
      "host=127.0.0.1 my_oauth_client_secret=x scram_client_keys=x dbname=mine",
    ],
    "an empty keyword value, which holds no password": [
      "host=127.0.0.1 password=",
      "host=127.0.0.1 password=",
    ],
    "an empty password parameter, which holds no password": [
      "postgres://127.0.0.1:5433/?password=&dbname=mine",
      "postgres://127.0.0.1:5433/?password=&dbname=mine",
    ],
    "a PGPASSWORD assignment in lower case": [
      "pgpassword=sekrit psql -c 'select 1'",
      "pgpassword=*** psql -c 'select 1'",
    ],
    "a PGPASSWORD assignment with a single-quoted value": [
      "PGPASSWORD='se kr it' psql",
      "PGPASSWORD=*** psql",
    ],
    "an empty PGPASSWORD assignment, which holds no password": [
      "PGPASSWORD= psql",
      "PGPASSWORD= psql",
    ],
    "variables libpq does not read": [
      "DB_PASSWORD=x MY_PGPASSWORD=x psql",
      "DB_PASSWORD=x MY_PGPASSWORD=x psql",
    ],
    "the keyword form glued to psql's -d": ["-dpassword=sekrit", "-dpassword=***"],
    "the sslpassword keyword glued to psql's -d": [
      "-dsslpassword=sekrit host=127.0.0.1",
      "-dsslpassword=*** host=127.0.0.1",
    ],
    "a PGPASSWORD assignment glued to a short option": ["-ePGPASSWORD=sekrit", "-ePGPASSWORD=***"],
    "the keyword form after two grouped short options": ["-Xdpassword=sekrit", "-Xdpassword=***"],
    "the keyword form after the short options psql groups": [
      "-XAtqwdpassword=sekrit",
      "-XAtqwdpassword=***",
    ],
    "a PGPASSWORD assignment after grouped short options": [
      "-XdPGPASSWORD=sekrit",
      "-XdPGPASSWORD=***",
    ],
    // psql's `-1` is an option too, and groups with `-d` as a letter does.
    "the keyword form after psql's -1 grouped with -d": ["-1dpassword=sekrit", "-1dpassword=***"],
    // Over-masked on purpose: after `-`, any run of letters reads as options, so a longer key that
    // ends in a secret key is hidden too.
    "a longer key that ends in a secret key, after a short option": [
      "-dmypassword=x",
      "-dmypassword=***",
    ],
    // A key after `-` stands after a character no key holds, so a long option hides as the
    // keyword form does.
    "the keyword form as a long option": ["--password=sekrit", "--password=***"],
    "short options that hold no secret key": [
      "-dbname=x -dmydb -dpasswords=x -Xdbname=x",
      "-dbname=x -dmydb -dpasswords=x -Xdbname=x",
    ],
  };
  expect(
    Object.fromEntries(Object.entries(forms).map(([form, [text]]) => [form, masked(text)])),
    "A child's name, which the cost line and a failure print on the run's stderr, still holds a " +
      "password of the form named, or hides other than what this row expects of that form."
  ).toEqual(Object.fromEntries(Object.entries(forms).map(([form, [, shown]]) => [form, shown])));
  // `named` itself, for what it does beyond `masked` with an argument: each argument is masked on
  // its own, so an empty `password=` at the end of one leaves the next whole; an argument that
  // assigns PGPASSWORD, as `env` or a shell's `-c` takes it, hides its value; and a line break in
  // an argument is escaped only after `masked` has read it. What `named` prints of the
  // environment is the last row of this file.
  expect(
    {
      emptyInAnArgument: named({
        argv: ["psql", "host=127.0.0.1 password=", "-c", "select 1"],
        from: import.meta.url,
      }),
      assignedByEnv: named({
        argv: ["env", "PGPASSWORD=sekrit", "psql", "-c", "select 1"],
        from: import.meta.url,
      }),
      assignedInAShell: named({
        argv: ["bash", "-c", "PGPASSWORD=sekrit psql -c 'select 1'"],
        from: import.meta.url,
      }),
      brokenInAnArgument: named({
        argv: ["psql", "host=127.0.0.1\npassword=sekrit", "-c", "select 1"],
        from: import.meta.url,
      }),
    },
    "A child's name printed part of a password, or a mask reached past its own argument and hid " +
      "a part of the command that is no password."
  ).toEqual({
    emptyInAnArgument:
      "e2e/__tests__/bounded-subprocess.test.ts | psql host=127.0.0.1 password= -c select 1 | " +
      "this process's environment",
    assignedByEnv:
      "e2e/__tests__/bounded-subprocess.test.ts | env PGPASSWORD=*** psql -c select 1 | " +
      "this process's environment",
    assignedInAShell:
      "e2e/__tests__/bounded-subprocess.test.ts | bash -c PGPASSWORD=*** psql -c 'select 1' | " +
      "this process's environment",
    brokenInAnArgument:
      "e2e/__tests__/bounded-subprocess.test.ts | psql host=127.0.0.1\\npassword=*** -c select 1 | " +
      "this process's environment",
  });
});

test("a child's name hides the sslpassword libpq reads, as it hides password", () => {
  // `sslpassword` is the libpq connection parameter that holds the password of the client's SSL
  // key, in the keyword form and as a URL query parameter alike, and libpq reads it beside
  // `password`. `packages/envoy/loadtest/guard.go` counts it as a secret beside `password` too.
  // Every password here is made up.
  const forms: Record<string, readonly [text: string, shown: string]> = {
    "the sslpassword keyword": [
      "host=127.0.0.1 sslpassword=sekrit dbname=mine",
      "host=127.0.0.1 sslpassword=*** dbname=mine",
    ],
    "an sslpassword query parameter": [
      "postgres://127.0.0.1:5433/?sslpassword=sekrit&dbname=mine",
      "postgres://127.0.0.1:5433/?sslpassword=***&dbname=mine",
    ],
    "keys that are not sslpassword": [
      "host=127.0.0.1 sslmode=require sslpasswords=x mysslpassword=x dbname=mine",
      "host=127.0.0.1 sslmode=require sslpasswords=x mysslpassword=x dbname=mine",
    ],
  };
  expect(
    Object.fromEntries(Object.entries(forms).map(([form, [text]]) => [form, masked(text)])),
    "A child's name, which the cost line and a failure print on the run's stderr, still holds " +
      "the sslpassword of the form named, or hides a key that is no password."
  ).toEqual(Object.fromEntries(Object.entries(forms).map(([form, [, shown]]) => [form, shown])));
});

test("a child's name prints an environment value only for a variable that tells runs apart", () => {
  // The cost line and a failure print on the run's stderr, which CI keeps in a public log, so
  // `named` prints an environment entry's value only where its variable tells one run of a command
  // from another (`runTellingVariables`), and every other entry by its name alone: a token no mask
  // knows, a connection string that holds a password, and PGPASSWORD alike. A caller whose runs
  // differ only in such a name tells them apart by DISPATCH_E2E_CASE, whose value prints. A name
  // the caller takes away prints as `NAME=`. `named` prints every name the caller gave but HOME,
  // PATH and TMPDIR, which it leaves out because a caller writes them only to give a child a home,
  // a path or a temporary directory, never to tell two runs apart. A value that prints is masked
  // as an argument is, and is quoted as JSON writes it where it is empty or holds whitespace or a
  // `"`, so `NAME=""` is an empty value and never a name taken away. Every value here is made up.
  const from = import.meta.url;
  expect(
    {
      token: named({ argv: ["psql"], env: { SOME_TOKEN: "secret" }, from }),
      runTelling: named({
        argv: ["psql"],
        env: {
          DISPATCH_E2E_PROCESSES: "2",
          DISPATCH_E2E_SERVER_BIN: "/given/envoy-dispatch",
          DISPATCH_E2E_SIGNIN: "oidc",
          TEST_PARALLEL_INDEX: "1",
        },
        from,
      }),
      labelled: named({
        argv: ["psql"],
        env: { DATABASE_URL: "host=127.0.0.1 password=sekrit", DISPATCH_E2E_CASE: "x" },
        from,
      }),
      maskedValue: named({
        argv: ["psql"],
        env: { DISPATCH_E2E_CASE: "password=sekrit" },
        from,
      }),
      spacedAndEmpty: named({
        argv: ["psql"],
        env: { DISPATCH_E2E_PROCESSES: "", DISPATCH_E2E_SIGNIN: " oidc" },
        from,
      }),
      brokenAndQuoted: named({
        argv: ["psql"],
        env: { DISPATCH_E2E_CASE: '"a"\nb' },
        from,
      }),
      libpq: named({
        argv: ["psql"],
        env: {
          DATABASE_URL: "host=127.0.0.1\npassword=sekrit",
          HOME: "/home/given",
          PASSWORD: "sekrit",
          PGHOST: "127.0.0.1",
          PGPASSWORD: "se kr it",
        },
        from,
      }),
      takenAway: named({
        argv: ["psql"],
        env: { DISPATCH_E2E_SIGNIN: undefined, PGPASSWORD: undefined },
        from,
      }),
    },
    "A child's name printed the value of a variable that does not tell runs apart, which may be " +
      "a secret, or hid the value of one that does, or lost a name the caller took away, or " +
      "printed a password in a value it prints, or wrote a value it prints in a way the legend " +
      "of `named` does not read back."
  ).toEqual({
    token: "e2e/__tests__/bounded-subprocess.test.ts | psql | SOME_TOKEN",
    runTelling:
      "e2e/__tests__/bounded-subprocess.test.ts | psql | DISPATCH_E2E_PROCESSES=2 " +
      "DISPATCH_E2E_SERVER_BIN=/given/envoy-dispatch DISPATCH_E2E_SIGNIN=oidc TEST_PARALLEL_INDEX=1",
    labelled: "e2e/__tests__/bounded-subprocess.test.ts | psql | DATABASE_URL DISPATCH_E2E_CASE=x",
    maskedValue: "e2e/__tests__/bounded-subprocess.test.ts | psql | DISPATCH_E2E_CASE=password=***",
    spacedAndEmpty:
      "e2e/__tests__/bounded-subprocess.test.ts | psql | " +
      'DISPATCH_E2E_PROCESSES="" DISPATCH_E2E_SIGNIN=" oidc"',
    brokenAndQuoted:
      'e2e/__tests__/bounded-subprocess.test.ts | psql | DISPATCH_E2E_CASE="\\"a\\"\\nb"',
    libpq:
      "e2e/__tests__/bounded-subprocess.test.ts | psql | DATABASE_URL PASSWORD PGHOST PGPASSWORD",
    takenAway: "e2e/__tests__/bounded-subprocess.test.ts | psql | DISPATCH_E2E_SIGNIN= PGPASSWORD=",
  });
});
