import { expect, test } from "bun:test";
import { randomInt } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";
import {
  bounds,
  eventually,
  execPause,
  type Listed,
  listedOf,
  masked,
  maxReads,
  named,
  ownUnknown,
  processTableYielding,
  procTreeAt,
  rowLimit,
  runBounded,
  stopTree,
  stopTreeYielding,
  whether,
} from "./bounded-subprocess";

const boundedModule = fileURLToPath(new URL("./bounded-subprocess.ts", import.meta.url));

/** Whether `path` exists: false only where it is missing (ENOENT), as `/proc` is on a machine that
 *  lists its processes with `ps`. Any other failure of the check, EACCES among them, throws, so a
 *  file whose rows need the path fails where it cannot read it, rather than skipping those rows
 *  as a machine without it would. */
function present(path: string): boolean {
  try {
    statSync(path);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
}

/** The command line of every process on the machine, by pid, with its words joined by spaces; or
 *  undefined where neither `/proc` nor `ps` lists processes. Without `/proc`, one `ps` lists them,
 *  run by `Bun.spawnSync` with no bound, so a `ps` that never ends is never stopped. That is a
 *  stated edge: Linux lists its processes under `/proc` and never runs it. */
function commandLines(): Map<number, string> | undefined {
  const lines = new Map<number, string>();
  if (present("/proc/self/cmdline")) {
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
test.skipIf(!present("/proc/self/stat"))(
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

/** One turn of this process's event loop, on a `setImmediate`. */
function nextTurn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** The child of a kill of `scriptedChild`, standing as `child` says, and the collaborators that
 *  drive it over `tables`, a scripted process table: read n answers `tables[n - 1]`, and each
 *  read after the last answers the last again. `procfs` tells the kill whether its table comes
 *  from `/proc`, so a row runs either path whatever this machine has, and the signal of a pid in
 *  `gone` throws, as `process.kill` throws for a process that has ended. `did` records what the
 *  kill did, in order: each call of `read`, `pause`, `signal` and `child.kill`, written as the
 *  stand-in's name and then every argument it was given. So `read` and `pause`, which are given
 *  none, write their name alone, a signal writes `signal <pid>`, and the kill of a running child
 *  writes `child.kill SIGKILL`. No clock runs and no process is started or signalled: the pause,
 *  the signal and `child.kill` only record, and act on no process. A read past `maxReads`
 *  throws, so a loop that lost that bound ends rather than spinning past every row clock. */
function scripted(
  tables: readonly (readonly Listed[] | undefined)[],
  {
    child = exitedByCode,
    gone = [],
    procfs = true,
  }: { child?: ChildState; gone?: readonly number[]; procfs?: boolean }
) {
  const did: string[] = [];
  // A call of the stand-in `name`, given `args`: its name, then each argument.
  const record = (name: string, args: readonly unknown[]): void => {
    did.push([name, ...args].map(String).join(" "));
  };
  let reads = 0;
  const run = {
    child: {
      ...child,
      kill: (...args: unknown[]) => record("child.kill", args),
      pid: scriptedChild.pid,
    },
    start: scriptedChild.start,
    token: scriptedChild.token,
  };
  const collaborators = {
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
  };
  return { collaborators, did, run };
}

/** What a kill of `scriptedChild` did over `tables` (`scripted`), run by each kill in turn:
 *  `stopTree`, in one synchronous pass, and `stopTreeYielding`, whose read and pause each answer a
 *  turn of the event loop later. `runBounded` runs the second and the exit listeners the first,
 *  and both must do the same, in the same order: a kill that differs fails here, naming the one
 *  that turns. */
async function scriptedKill(
  tables: readonly (readonly Listed[] | undefined)[],
  options: { child?: ChildState; gone?: readonly number[]; procfs?: boolean } = {}
): Promise<string[]> {
  const once = scripted(tables, options);
  stopTree(once.run, once.collaborators);
  const turning = scripted(tables, options);
  await stopTreeYielding(turning.run, {
    ...turning.collaborators,
    pause: async () => {
      await nextTurn();
      turning.collaborators.pause();
    },
    read: async () => {
      await nextTurn();
      return turning.collaborators.read();
    },
  });
  expect(turning.did, "stopTreeYielding, against what stopTree did").toEqual(once.did);
  return once.did;
}

test("a kill reads again, after a pause, while a process started since the child reads unknown", async () => {
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
    await scriptedKill([
      [sibling, execing, init],
      [sibling, loaded, init],
      [sibling, loaded, init],
    ])
  ).toEqual(["read", "pause", "read", `signal ${pid}`, "read"]);
});

test("an unknown process that started before the child ends a kill at its first read", async () => {
  const older: Listed = {
    pid: process.pid + 2,
    ppid: 1,
    start: scriptedChild.start - 1,
    tokens: [],
    unknown: true,
  };
  expect(await scriptedKill([[init, older, sibling]])).toEqual(["read"]);
});

test("a process started since the child that stays unknown ends a kill's reads at maxReads", async () => {
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
  expect(await scriptedKill([[stuck, init, sibling, older]]), "the stuck process first").toEqual(
    everyRead
  );
  expect(await scriptedKill([[older, sibling, init, stuck]]), "the stuck process last").toEqual(
    everyRead
  );
});

test("an unknown process the kill signalled already ends its reads", async () => {
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
    await scriptedKill([
      [carrying, init, sibling],
      [exiting, init, sibling],
    ])
  ).toEqual(["read", `signal ${pid}`, "read"]);
});

test("a kill takes a running child through child.kill, and what it started by parent pid", async () => {
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
    await scriptedKill(
      [
        [below(left), init, child, sibling, below(ended)],
        [init, sibling],
      ],
      { child: stillRunning, gone: [ended] }
    )
  ).toEqual(["read", "child.kill SIGKILL", `signal ${ended}`, `signal ${left}`, "read"]);
});

test("a child that has ended is found by its token alone, though a process holds its pid", async () => {
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
    expect(await scriptedKill([[init, sibling, holder]], { child }), `a child that ${how}`).toEqual(
      ["read", `signal ${scriptedChild.pid}`, "read"]
    );
  }
});

test("a kill takes each process whose tokens hold the child's, first, last or in the middle", async () => {
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
    await scriptedKill([
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

test("where `/proc` is missing, a kill reads once, and not at all for a child that ended", async () => {
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
  expect(await scriptedKill([table], { child: stillRunning, procfs: false })).toEqual([
    "read",
    "child.kill SIGKILL",
    `signal ${below}`,
  ]);
  // A `ps` that fails lists nothing, and the running child is killed all the same.
  expect(await scriptedKill([undefined], { child: stillRunning, procfs: false })).toEqual([
    "read",
    "child.kill SIGKILL",
  ]);
  expect(await scriptedKill([table], { procfs: false })).toEqual([]);
});

test("a kill never signals this process, even where its table reads it as one of the child's", async () => {
  const self: Listed = {
    pid: process.pid,
    ppid: 1,
    start: scriptedChild.start + 1,
    tokens: [outerToken, scriptedChild.token],
    unknown: false,
  };
  expect(await scriptedKill([[self, init, sibling]])).toEqual(["read"]);
});

test("a kill with its own signal and pause sends SIGKILL through process.kill and waits execPause", async () => {
  // Every scripted row above hands each kill a stand-in `signal` and `pause`, so none of them
  // reaches the defaults, which call `process.kill`, and `Bun.sleepSync` in `stopTree` and
  // `Bun.sleep` in `stopTreeYielding`. The setsid row above reaches the yielding kill's through
  // `runBounded`, but its `sleep` dies of SIGTERM as of SIGKILL. This row hands each kill `read`
  // and `procfs` alone, and stands in for those three globals while the kills run, each recording
  // every argument it is given. Its table is that of the first scripted row: a process started
  // since the child, unknown at read 1 and carrying the child's token at read 2, here between an
  // unknown process older than the child and a process of the sibling. Every pid its table lists
  // is above 2 ** 22, Linux's `PID_MAX_LIMIT` and the most `/proc/sys/kernel/pid_max` can hold,
  // so no process holds one, and a stand-in that failed to take its global's place would signal
  // nothing real.
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
  // What one kill did through its child and the three globals, and the child and the read it is
  // handed.
  const records = () => {
    const did = {
      childKills: [] as unknown[][],
      reads: [] as unknown[][],
      kills: [] as unknown[][],
      sleepSyncs: [] as unknown[][],
      sleeps: [] as unknown[][],
    };
    const run = {
      child: {
        ...exitedByCode,
        kill: (...args: unknown[]) => did.childKills.push(args),
        pid: scriptedChild.pid,
      },
      start: scriptedChild.start,
      token: scriptedChild.token,
    };
    const read = (...args: unknown[]) => {
      did.reads.push(args);
      if (did.reads.length > maxReads) {
        throw new Error(`the kill read the table ${did.reads.length} times, past maxReads`);
      }
      return tables[Math.min(did.reads.length, tables.length) - 1];
    };
    return { did, read, run };
  };
  const once = records();
  const turning = records();
  const realKill = process.kill;
  const realSleepSync = Bun.sleepSync;
  const realSleep = Bun.sleep;
  // The kill that runs now records into its own `did`.
  let current = once.did;
  try {
    Object.assign(process, {
      kill: (...args: unknown[]) => {
        current.kills.push(args);
        return true;
      },
    });
    Object.assign(Bun, {
      sleepSync: (...args: unknown[]) => current.sleepSyncs.push(args),
      sleep: (...args: unknown[]) => {
        current.sleeps.push(args);
        return Promise.resolve();
      },
    });
    expect(process.kill, "process.kill is not the stand-in").not.toBe(realKill);
    expect(Bun.sleepSync, "Bun.sleepSync is not the stand-in").not.toBe(realSleepSync);
    expect(Bun.sleep, "Bun.sleep is not the stand-in").not.toBe(realSleep);
    stopTree(once.run, { procfs: true, read: once.read });
    current = turning.did;
    await stopTreeYielding(turning.run, { procfs: true, read: turning.read });
  } finally {
    Object.assign(process, { kill: realKill });
    Object.assign(Bun, { sleepSync: realSleepSync, sleep: realSleep });
  }
  const each = { childKills: [], reads: [[], [], []], kills: [[pid, "SIGKILL"]] };
  expect(once.did, "stopTree").toEqual({ ...each, sleepSyncs: [[execPause]], sleeps: [] });
  expect(turning.did, "stopTreeYielding").toEqual({
    ...each,
    sleepSyncs: [],
    sleeps: [[execPause]],
  });
});

// The kill `runBounded` runs once a child ends reads each process's files under `/proc` through
// `Bun.file` (`processTableYielding`), whose read waits off this process's event loop, so a server
// this process runs, as the origin stand-ins of `outside-edge.test.ts` do, answers while it reads;
// the slow-read row below holds that such a read holds no timer. This row stands in for
// `Bun.file` and counts the files under `/proc` it is asked for while one bounded `true` runs and
// its kill reads the table. A kill that read the table synchronously, as `stopTree` does, asks for
// none. The row skips where `/proc` is missing. It counts the processes just before the child
// starts, and not when the file loads: the rows run in random order, and a count taken minutes
// before the kill's read, on a machine whose processes come and go by the thousand, could hold
// many more processes than that read finds.
test.skipIf(!present("/proc/self/stat"))(
  "a bounded child's kill hands this process's event loop back while it reads the process table",
  async () => {
    const realFile = Bun.file;
    let asked = 0;
    let procProcesses = 0;
    try {
      Object.assign(Bun, {
        file: (...args: unknown[]) => {
          if (typeof args[0] === "string" && args[0].startsWith("/proc/")) asked += 1;
          return Reflect.apply(realFile, Bun, args);
        },
      });
      expect(Bun.file, "Bun.file is not the stand-in").not.toBe(realFile);
      procProcesses = readdirSync("/proc").filter((entry) => /^\d+$/.test(entry)).length;
      await runBounded({ argv: ["true"], bound: "short", from: import.meta.url });
    } finally {
      Object.assign(Bun, { file: realFile });
    }
    // At least one file, its stat, for every two of the processes this row counted, so a kill
    // that read half the table synchronously fails too, while the table may have lost processes
    // between this row's count and the kill's read. A count of none would hold nothing.
    expect(procProcesses, "processes this row counted under /proc").toBeGreaterThan(0);
    expect(
      asked,
      `files the kill read through Bun.file, of ${procProcesses} processes under /proc`
    ).toBeGreaterThanOrEqual(Math.floor(procProcesses / 2));
  },
  rowLimit("short")
);

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

test("an unknown process stays unknown in this process's cgroup alone, and not where it is gone", () => {
  const unknown = listedOf(readProcess.pid, statText(), "");
  if (unknown === undefined) throw new Error("listedOf listed no process");
  const own = "0::/user.slice/user-1000.slice/app.slice/run-u1.scope\n";
  const other = "0::/user.slice/user-1000.slice/app.slice/run-u2.scope\n";
  // One of the child's, caught in its exec: it starts where the child does.
  expect(ownUnknown(unknown, own, own)).toEqual(unknown);
  // Another session's program started with `env -i`, which reads unknown for as long as it runs.
  expect(ownUnknown(unknown, other, own)).toEqual({ ...unknown, unknown: false });
  // A process whose cgroup cannot be read is gone.
  expect(ownUnknown(unknown, undefined, own)).toEqual({ ...unknown, unknown: false });
  // Where this process's cgroup cannot be read, every unknown process stays unknown.
  expect(ownUnknown(unknown, other, undefined)).toEqual(unknown);
});

// The kill `runBounded` runs reads the table through `processTableYielding`, whose `entryReads`
// hands the cgroup of each unknown process to `ownUnknown`. The row above holds `ownUnknown`
// alone, and the systemd-run row below holds the real table's entry for a process of another
// cgroup, where this machine can start one there: CI's runners can, and a pod cannot, so it skips
// in a pod. This row lays out a `/proc` of its own in a
// scratch directory, a `self` in one cgroup and one process whose environment reads empty and
// which started at the child's start, and drives the yielding kill over it with `runBounded`'s
// read, handed that directory. With the process in another cgroup than `self`'s, it is listed as
// not unknown and the kill reads once; in the same cgroup, which shows the kill read this
// directory and can wait on it, it is listed as unknown and the kill waits after every read until
// `maxReads`. No process is signalled: the process carries no token, its parent is no child, and
// the signal and the child's `kill` only record.
test("a kill reads a /proc whose process of another cgroup reads empty, and does not wait on it", async () => {
  const root = mkdtempSync(join(tmpdir(), "bounded-proc-"));
  try {
    const own = "0::/user.slice/user-1000.slice/app.slice/run-u1.scope\n";
    const other = "0::/user.slice/user-1000.slice/app.slice/run-u2.scope\n";
    mkdirSync(join(root, "self"));
    writeFileSync(join(root, "self", "cgroup"), own);
    const entry = join(root, String(readProcess.pid));
    mkdirSync(entry);
    writeFileSync(join(entry, "stat"), statText());
    writeFileSync(join(entry, "environ"), "");
    const tree = procTreeAt(root);
    expect(tree.own, "the cgroup of the tree's self").toBe(own);
    // What the yielding kill's read lists, and what the kill did, with the process in `cgroup`.
    const readAndKill = async (cgroup: string) => {
      writeFileSync(join(entry, "cgroup"), cgroup);
      const table = await processTableYielding(tree);
      const did: string[] = [];
      await stopTreeYielding(
        {
          child: {
            ...exitedByCode,
            kill: () => {
              did.push("child.kill");
            },
            pid: scriptedChild.pid,
          },
          start: readProcess.start,
          token: scriptedChild.token,
        },
        {
          pause: () => {
            did.push("pause");
          },
          procfs: true,
          read: () => {
            did.push("read");
            return processTableYielding(tree);
          },
          signal: (pid) => {
            did.push(`signal ${pid}`);
          },
        }
      );
      return { did, table };
    };
    expect(await readAndKill(other), "the process in another cgroup").toEqual({
      did: ["read"],
      table: [{ ...readProcess, tokens: [], unknown: false }],
    });
    expect(await readAndKill(own), "the process in this cgroup").toEqual({
      did: Array.from({ length: maxReads }, () => ["read", "pause"]).flat(),
      table: [{ ...readProcess, tokens: [], unknown: true }],
    });
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
});

/** Whether this machine runs a user service manager that starts a command in a scope, a cgroup, of
 *  its own (`systemd-run --user --scope`), as a dev box many sessions share does, and as CI's
 *  runners do (job 137301512515 ran the row below on `ubuntu-24.04-arm`). The row below needs one
 *  to start a process outside this process's cgroup, and skips where there is none, as in a pod,
 *  where the two rows above hold the rule: `ownUnknown` alone, and the kill over a `/proc` laid
 *  out with a process in another cgroup. */
const userScopes =
  present("/proc/self/cgroup") &&
  Bun.which("systemd-run") !== null &&
  present(join(process.env.XDG_RUNTIME_DIR ?? "/nonexistent", "systemd", "private"));

test.skipIf(!userScopes)(
  "the table a kill reads lists a process of another cgroup whose environment reads empty as not unknown",
  async () => {
    // Another session's program, as on a shared dev box: a `sleep` with an empty environment, in
    // a scope of its own, carrying no token. A number of seconds no other process sleeps for, so
    // the row finds it by its command line. It is this row's to stop, and outlives every wait of
    // the row.
    const seconds = `${rowLimit("short", "short") / 1000}.${randomInt(10_000_000, 100_000_000)}`;
    const stranger = Bun.spawn(
      ["systemd-run", "--user", "--scope", "--quiet", "env", "-i", "sleep", seconds],
      { detached: true, stderr: "ignore", stdout: "ignore" }
    );
    try {
      const listed = () => holding(seconds).find(([, line]) => line.startsWith("sleep "));
      await eventually("the other cgroup's sleep, listed", () => listed() !== undefined);
      const [pid] = listed() ?? [0];
      expect(readFileSync(`/proc/${pid}/environ`, "utf8"), "its environment").toBe("");
      expect(readFileSync(`/proc/${pid}/cgroup`, "utf8"), "its cgroup").not.toBe(
        readFileSync("/proc/self/cgroup", "utf8")
      );
      // The kill waits only on a process its table lists as unknown (`killSteps`), which the row
      // above holds over a laid-out `/proc`. This row reads the real table the kill reads and
      // decides on the sleep's own entry, so another process of this process's cgroup whose
      // environment reads empty changes nothing here: under `bun test --parallel` every worker
      // stands in one cgroup, and the shapes row of `outside-agent-acceptance.test.ts` starts an
      // `env -i sleep` there, which reads unknown for as long as it runs.
      const table = await processTableYielding();
      expect(
        table?.find((one) => one.pid === pid),
        "the sleep, as the kill's table lists it"
      ).toMatchObject({ pid, unknown: false });
    } finally {
      kill(-stranger.pid, "SIGKILL");
    }
  },
  rowLimit("short", "short")
);

// A read of one `/proc/<pid>/environ` can take seconds by itself on a machine of many processes,
// and the kill reads every process's. This row lays out a `/proc` of its own whose one process's
// `environ` is a named pipe, which a read waits on until a writer opens it and closes it. The
// writer, a bounded child, waits for a file that this row's timer, due 50 ms after the read
// begins, writes, and opens the pipe once the file is there, or after 10 s without it. So where
// the read holds no timer of this process, the timer fires and the read ends after it; a read
// that held the loop, as a synchronous one does, ends after the writer's 10 s with the timer not
// yet fired. The table it reads is held too: the process, whose environment read empty, in this
// tree's cgroup, so unknown.
test(
  "a kill's read of the process table holds no timer of this process while a read of /proc waits",
  async () => {
    const root = mkdtempSync(join(tmpdir(), "bounded-proc-"));
    try {
      const own = "0::/user.slice/user-1000.slice/app.slice/run-u1.scope\n";
      mkdirSync(join(root, "self"));
      writeFileSync(join(root, "self", "cgroup"), own);
      const entry = join(root, String(readProcess.pid));
      mkdirSync(entry);
      writeFileSync(join(entry, "stat"), statText());
      writeFileSync(join(entry, "cgroup"), own);
      const environ = join(entry, "environ");
      const go = join(root, "go");
      await runBounded({ argv: ["mkfifo", environ], bound: "short", from: import.meta.url });
      const writing = runBounded({
        argv: [
          "sh",
          "-c",
          'i=0; while [ ! -e "$1" ] && [ "$i" -lt 200 ]; do sleep 0.05; i=$((i + 1)); done; : > "$2"',
          "writer",
          go,
          environ,
        ],
        bound: "short",
        from: import.meta.url,
      });
      const started = performance.now();
      let firedAt = Number.POSITIVE_INFINITY;
      // A real timer, not a fake clock: what the row holds is whether this process's event loop
      // runs a timer while a read of the table waits, which only the loop itself can answer.
      const timer = setTimeout(() => {
        firedAt = performance.now() - started;
        writeFileSync(go, "");
      }, 50);
      // The writer's exit, or what its wait threw.
      let wrote: unknown;
      try {
        const table = await processTableYielding(procTreeAt(root));
        const endedAt = performance.now() - started;
        expect(table, "the table").toEqual([{ ...readProcess, tokens: [], unknown: true }]);
        expect(
          firedAt,
          `ms to the timer due at 50 ms, against the read's end at ${Math.round(endedAt)} ms`
        ).toBeLessThan(endedAt);
      } finally {
        clearTimeout(timer);
        writeFileSync(go, "");
        wrote = await writing.then(
          ({ code }) => code,
          (error: unknown) => error
        );
      }
      expect(wrote, "the writer's exit").toBe(0);
    } finally {
      rmSync(root, { force: true, recursive: true });
    }
  },
  rowLimit("short", "short", "short")
);

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
// loop for ever and the row would hang: no row clock stops a synchronous loop, and `stopTree`
// walks it in one pass, so only the job's limit would end it. The rows run in random order
// (packages/dispatch/bunfig.toml), so the rows of this file that had not run by then would not
// report either.
test("a kill's walk by parent pid ends where its table lists two processes as each other's parent", async () => {
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
  expect(await scriptedKill([table])).toEqual(["read", `signal ${one}`, `signal ${other}`, "read"]);
});

// A machine can list one parent with tens of thousands of children: on a dev box of 288 CPUs on
// 2026-11-22, pid 2, which starts the kernel's threads, had about 36,000. A walk that built each
// parent's list of children by copying the list for every child it added would take time that
// grows with the square of that count: at a load average of about 2,150 there, 2.6 s for 36,000
// children and 13.5 s for 100,000, against 2.4 ms when each child is pushed onto its parent's list.
// Each kill below walks the table twice and `scriptedKill` runs both kills, so four walks over
// 300,000 children end far inside the row's limit when the lists grow in place, and take minutes
// when they are copied. `stopTree` walks in one synchronous pass, so the row clock fires only after
// the first kill has ended.
test(
  "a kill's walk by parent pid takes a parent's many children without copying its list for each",
  async () => {
    // The child's own process, which carries its token, and 300,000 kernel threads of pid 2, which
    // carry none. Every thread's pid is above 2 ** 22, Linux's `PID_MAX_LIMIT`, so no process holds
    // one.
    const own: Listed = {
      pid: process.pid + 2,
      ppid: 1,
      start: scriptedChild.start + 1,
      tokens: [scriptedChild.token],
      unknown: false,
    };
    const threads = Array.from(
      { length: 300_000 },
      (_, at): Listed => ({ pid: 2 ** 23 + at, ppid: 2, start: 2, tokens: [], unknown: false })
    );
    expect(await scriptedKill([[init, own, sibling, ...threads]])).toEqual([
      "read",
      `signal ${own.pid}`,
      "read",
    ]);
  },
  rowLimit("short")
);

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
