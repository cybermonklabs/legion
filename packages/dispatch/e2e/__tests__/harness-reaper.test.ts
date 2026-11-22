/** The reaper of e2e/harness-reaper.ts and e2e/reaper.ts: what a starter killed with SIGKILL
 *  leaves running, what one that stopped everything leaves, that the reaper signals only the
 *  run's own groups, that a reaper that cannot start refuses the start, and the order the reaper
 *  works in.
 *
 *  The first six rows each drive a starter of its own, a `bun -e` over this checkout's modules,
 *  the way Playwright's runner and `e2e/harness-up.ts` start the harness: each reads the
 *  starter's first line, then kills it or lets it end. A starter that is to be killed stays up
 *  until its stdin, which this process holds, ends, so one that a failed row never killed ends
 *  with this process. The starter's stderr is its reaper's too, so reading it to its end waits
 *  for both, and holds what the reaper said. Every process a row started is killed by its pid in
 *  the row's `finally`, whatever the row found.
 *
 *  A group goes to the reaper with its leader's start time, read from `/proc`, and where there is
 *  no `/proc` no group is handed over at all; the rows about groups skip there, as
 *  `bounded-subprocess.test.ts` skips its own rows that read `/proc`.
 *
 *  The last three rows drive `reap` over a scripted machine, since what they read - no command
 *  while a start is still under way, and a number the kernel handed to another process since - is
 *  something a real reaper meets only inside a window no row can hold open. */
import { afterEach, expect, test } from "bun:test";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Subprocess } from "bun";

import { type Cleanup, type ProcStat, procStat } from "../harness-reaper";
import { type Collaborators, reap } from "../reaper";
import { beforeDeadline, bounds, eventually, pastDeadline, rowLimit } from "./bounded-subprocess";

const makeDirectory = temporaryDirectories("harness-reaper");

const supervisorModule = fileURLToPath(new URL("../local-instance-support.ts", import.meta.url));
const reaperModule = fileURLToPath(new URL("../harness-reaper.ts", import.meta.url));

/** Whether this machine lists its processes under `/proc`, which a group's hand-over reads. */
const procfs = existsSync("/proc/self/stat");

/** How long each `sleep` a row starts runs, in seconds: longer than the row, so a sleep the row
 *  finds gone was ended by what the row tests, never by its own end. */
const outlastsRow = rowLimit("short", "short", "short", "short") / 1000;

/** True while any process of group `pgid` is alive: signal 0 to the group reaches every member. */
function groupAlive(pgid: number): boolean {
  try {
    process.kill(-pgid, 0);
    return true;
  } catch {
    return false;
  }
}

function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

/** Sends SIGKILL to `pid`, a process or, when negative, a process group, where it is still alive:
 *  what a row leaves behind when it fails. ESRCH is a process already gone. */
function killLeft(pid: number | undefined): void {
  if (pid === undefined) return;
  try {
    process.kill(pid, "SIGKILL");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
  }
}

type Starter = Subprocess<"pipe", "pipe", "pipe">;

const starters: Starter[] = [];
afterEach(() => {
  for (const starter of starters.splice(0)) killLeft(starter.pid);
});

/** What a starter that is to be killed runs last: it stays up until its stdin ends. */
const untilKilled = "for await (const _ of process.stdin) {}";

/** Starts a starter that has `spawn`, `Supervisor` and the reaper's module as `reaper` in scope,
 *  runs `body`, which prints one line of JSON first, and answers that line with the starter. The
 *  starter is this very `bun`, named by its path, so a `PATH` of the row's own in `env` decides
 *  which `bun` the reaper is started with and nothing else. */
async function startStarter(
  body: string,
  env: Record<string, string | undefined> = process.env
): Promise<{ readonly starter: Starter; readonly said: Record<string, number | string> }> {
  const script = [
    `import { spawn } from "node:child_process";`,
    `import { Supervisor } from ${JSON.stringify(supervisorModule)};`,
    `import * as reaper from ${JSON.stringify(reaperModule)};`,
    body,
  ].join("\n");
  const starter = Bun.spawn([process.execPath, "-e", script], {
    env,
    stderr: "pipe",
    stdin: "pipe",
    stdout: "pipe",
  });
  starters.push(starter);
  const reader = starter.stdout.getReader();
  const decoder = new TextDecoder();
  let text = "";
  const deadline = performance.now() + bounds.short;
  while (!text.includes("\n")) {
    const chunk = await beforeDeadline(reader.read(), deadline);
    if (chunk === pastDeadline || chunk.done) {
      const stderr = await beforeDeadline(new Response(starter.stderr).text(), deadline);
      throw new Error(
        `the starter printed no line within the short bound: ${JSON.stringify(text)}; its ` +
          `stderr: ${stderr === pastDeadline ? "(unread)" : stderr}`
      );
    }
    text += decoder.decode(chunk.value, { stream: true });
  }
  reader.releaseLock();
  return { said: JSON.parse(text.slice(0, text.indexOf("\n"))), starter };
}

/** The starter's stderr to its end, which comes once the starter and its reaper have both ended:
 *  the reaper holds it too. */
async function stderrToEnd(starter: Starter): Promise<string> {
  const text = await beforeDeadline(
    new Response(starter.stderr).text(),
    performance.now() + bounds.short
  );
  if (text === pastDeadline) {
    throw new Error("the starter's stderr did not end within the short bound: its reaper runs on");
  }
  return text;
}

/** The pid a child process writes to `path` as one whole line. */
async function pidFrom(path: string): Promise<number> {
  const read = () => (existsSync(path) ? readFileSync(path, "utf8") : "");
  await eventually(`a pid in ${path}`, () => /^\d+\n$/u.test(read()));
  return Number(read());
}

/** The reaper's directories `directory` holds: what a starter whose `TMPDIR` it is left there. */
function reaperDirectories(directory: string): string[] {
  return readdirSync(directory).filter((entry) => entry.startsWith("dispatch-e2e-reaper-"));
}

test.skipIf(!procfs)(
  "a starter killed with SIGKILL leaves no group it started running, a grandchild included, " +
    "while a process it never handed over lives on",
  async () => {
    const directory = makeDirectory();
    const grandchildFile = join(directory, "grandchild.pid");
    const { said, starter } = await startStarter(
      `
      const reaperPid = await reaper.reaperStarted();
      const made = new Supervisor();
      const leader = made.startPiped("leader", ["bash", "-c", "sleep ${outlastsRow} & echo $! > ${grandchildFile}; wait"], "/", process.env);
      // The control: the same kind of group, started beside the Supervisor's and handed to no one.
      const stray = spawn("sleep", ["${outlastsRow}"], { detached: true, stdio: "ignore" });
      console.log(JSON.stringify({ leader: leader.child.pid, reaper: reaperPid, stray: stray.pid }));
      ${untilKilled}
    `,
      // A variable of the starter's environment that the reaper must not hold: where a caller's
      // DATABASE_URL carries its password. Its value here is none.
      { ...process.env, DATABASE_URL: "planted-for-the-reaper-row" }
    );
    const leader = Number(said.leader);
    const stray = Number(said.stray);
    let grandchild: number | undefined;
    try {
      // The reaper runs in an environment of its own, without the starter's DATABASE_URL.
      const environ = readFileSync(`/proc/${said.reaper}/environ`, "utf8").split("\0");
      expect(environ.some((entry) => entry.startsWith("PATH="))).toBe(true);
      expect(environ.filter((entry) => entry.startsWith("DATABASE_URL="))).toEqual([]);
      grandchild = await pidFrom(grandchildFile);
      const alone = grandchild;
      expect(groupAlive(leader)).toBe(true);
      expect(processAlive(stray)).toBe(true);

      process.kill(starter.pid, "SIGKILL");
      await eventually(
        `the group of the leader ${leader}, its grandchild ${alone} included, to end after its ` +
          `starter ${starter.pid} was killed`,
        () => !groupAlive(leader) && !processAlive(alone)
      );
      const reported = await stderrToEnd(starter);
      expect(reported).toContain(
        `e2e/reaper.ts: process ${starter.pid} ended without stopping what it started`
      );
      expect(reported).toContain(`process group ${leader}`);
      // The same kill and the same read find the group nobody handed over still running: the
      // group above ended by the reaper's hand, not by the kill of its starter.
      expect(processAlive(stray), `the stray process ${stray} ended with its starter`).toBe(true);
    } finally {
      killLeft(-leader);
      killLeft(grandchild);
      killLeft(stray);
    }
  },
  // Four waits the short bound ends: the starter's line, the grandchild's pid, the group's end,
  // the end of the starter's stderr.
  rowLimit("short", "short", "short", "short")
);

test(
  "a starter that stopped what it started ends by itself, and its reaper ends then, stopping nothing",
  async () => {
    const directory = makeDirectory();
    const { said, starter } = await startStarter(
      `
      const reaperPid = await reaper.reaperStarted();
      const made = new Supervisor();
      const leader = made.startPiped("leader", ["sleep", "${outlastsRow}"], "/", process.env);
      console.log(JSON.stringify({ leader: leader.child.pid, reaper: reaperPid }));
      await made.stop();
    `,
      { ...process.env, TMPDIR: directory }
    );
    const leader = Number(said.leader);
    const reaperPid = Number(said.reaper);
    try {
      // The starter's end is its own: neither the reaper nor the pipes to it hold it open.
      const code = await beforeDeadline(starter.exited, performance.now() + bounds.short);
      expect(code, "the starter did not end within the short bound").toBe(0);
      const stderr = await stderrToEnd(starter);
      expect(stderr).not.toContain("e2e/reaper.ts");
      await eventually(`the reaper ${reaperPid} to end`, () => !processAlive(reaperPid));
      expect(reaperDirectories(directory), "the reaper left its list behind").toEqual([]);
      expect(groupAlive(leader)).toBe(false);
    } finally {
      killLeft(-leader);
      killLeft(reaperPid);
    }
  },
  // Four waits the short bound ends: the starter's line, its end, the end of its stderr, and the
  // reaper's own end.
  rowLimit("short", "short", "short", "short")
);

test.skipIf(!procfs)(
  "a server group whose leader ended mid-run is off the reaper's list before its starter is killed",
  async () => {
    const { said, starter } = await startStarter(`
      await reaper.reaperStarted();
      const made = new Supervisor();
      // The leader that ends by itself mid-run, as a server that dies does, and one that runs on.
      const ended = made.startPiped("ended", ["true"], "/", process.env);
      await ended.exited;
      const runsOn = made.startPiped("runs on", ["sleep", "${outlastsRow}"], "/", process.env);
      console.log(JSON.stringify({ ended: ended.child.pid, runsOn: runsOn.child.pid }));
      ${untilKilled}
    `);
    const ended = Number(said.ended);
    const runsOn = Number(said.runsOn);
    try {
      process.kill(starter.pid, "SIGKILL");
      await eventually(`the group of ${runsOn} to end`, () => !groupAlive(runsOn));
      const reported = await stderrToEnd(starter);
      // The reaper acted, on the group that was still the run's, and the ended one was not on its
      // list for it to act on.
      expect(reported).toContain(`process group ${runsOn}`);
      expect(reported).not.toContain(`process group ${ended}`);
    } finally {
      killLeft(-runsOn);
    }
  },
  // Three waits the short bound ends: the starter's line, the group's end, the end of its stderr.
  rowLimit("short", "short", "short")
);

test.skipIf(!procfs)(
  "a listed group number that another process's group holds now is left alone, and the run's own is not",
  async () => {
    // A process that ran and ended before the foreign one started: its start time is what a stale
    // entry would carry for a number the kernel then gave to the foreign group.
    const earlier = Bun.spawn(["sleep", "0.2"]);
    const earlierStart = procStat(earlier.pid)?.start;
    await earlier.exited;
    // The foreign group: another starter's detached server, alive and none of the run's.
    const foreign = Bun.spawn(["sleep", `${outlastsRow}`], {
      detached: true,
      stdio: ["ignore", "ignore", "ignore"],
    });
    let own: number | undefined;
    try {
      expect(earlierStart).toBeDefined();
      expect(procStat(foreign.pid)?.start).not.toBe(earlierStart);
      const { said, starter } = await startStarter(`
        await reaper.reaperStarted();
        const made = new Supervisor();
        const own = made.startPiped("own", ["sleep", "${outlastsRow}"], "/", process.env);
        // The list as a stale entry leaves it: the foreign group's number, with the start time of
        // the process that held the number before.
        reaper.reapIfAbandoned("process group ${foreign.pid}", { group: ${foreign.pid}, start: ${JSON.stringify(earlierStart)} });
        console.log(JSON.stringify({ own: own.child.pid }));
        ${untilKilled}
      `);
      own = Number(said.own);
      const ownGroup = own;
      process.kill(starter.pid, "SIGKILL");
      await eventually(`the run's own group ${ownGroup} to end`, () => !groupAlive(ownGroup));
      const reported = await stderrToEnd(starter);
      expect(reported).toContain(
        `process group ${foreign.pid}: left alone, its number now names a process that started at`
      );
      expect(processAlive(foreign.pid), `the foreign group ${foreign.pid} was killed`).toBe(true);
    } finally {
      killLeft(-foreign.pid);
      if (own !== undefined) killLeft(-own);
    }
  },
  // Three waits the short bound ends: the starter's line, the own group's end, the end of its
  // stderr.
  rowLimit("short", "short", "short")
);

test(
  "a reaper that cannot start refuses the start by name, takes nothing more, and leaves no list behind",
  async () => {
    const directory = makeDirectory();
    const bin = join(directory, "bin");
    const tmp = join(directory, "tmp");
    mkdirSync(bin);
    mkdirSync(tmp);
    // A `bun` on PATH that is a broken version-manager shim: it starts, complains, and exits 1.
    writeFileSync(join(bin, "bun"), "#!/bin/sh\necho 'broken bun shim' >&2\nexit 1\n", {
      mode: 0o755,
    });
    const { said, starter } = await startStarter(
      `
      const refused = await reaper.reaperStarted().then(() => "", (error) => error.message);
      let handed = "";
      try {
        reaper.reapIfAbandoned("a directory", { commands: [["true"]] });
      } catch (error) {
        handed = error.message;
      }
      console.log(JSON.stringify({ handed, refused }));
    `,
      { ...process.env, PATH: `${bin}:${process.env.PATH ?? ""}`, TMPDIR: tmp }
    );
    const stderr = await stderrToEnd(starter);
    expect(String(said.refused)).toContain("its reaper (bun ");
    expect(String(said.refused)).toContain("exited 1 before it said it was ready");
    expect(said.handed).toBe(said.refused);
    expect(stderr).toContain("broken bun shim");
    expect(reaperDirectories(tmp)).toEqual([]);
  },
  // Two waits the short bound ends: the starter's line and the end of its stderr.
  rowLimit("short", "short")
);

test(
  "the reaper runs the commands of what is still handed over, the last handed over first",
  async () => {
    const directory = makeDirectory();
    const order = join(directory, "order");
    const append = (word: string) => JSON.stringify(["sh", "-c", `echo ${word} >> "$0"`, order]);
    const { starter } = await startStarter(`
      await reaper.reaperStarted();
      reaper.reapIfAbandoned("first", { commands: [${append("first")}] });
      reaper.reapIfAbandoned("second", { commands: [${append("second")}] });
      reaper.reapIfAbandoned("dropped", { commands: [${append("dropped")}] });
      reaper.reapIfAbandoned("third", { commands: [${append("third")}, ${append("third-again")}] });
      // A key handed over again keeps its place and takes its new commands.
      reaper.reapIfAbandoned("first", { commands: [${append("first-again")}] });
      reaper.stopReaping("dropped");
      console.log("{}");
      ${untilKilled}
    `);
    process.kill(starter.pid, "SIGKILL");
    const stderr = await stderrToEnd(starter);
    expect(stderr).toContain("waited for 0 starts under way and ran 4 commands, for 3 entries");
    expect(readFileSync(order, "utf8")).toBe("third\nthird-again\nsecond\nfirst-again\n");
  },
  // Two waits the short bound ends: the starter's line and the end of its stderr.
  rowLimit("short", "short")
);

/** A machine `reap` drives in place of this one: every call it makes, in order, in `events`.
 *  `stat` answers for each pid from `table`, given the number of reads of that pid so far; a pid
 *  absent from it names no process. `kill` of a group named in `gone` answers ESRCH. Each `pause`
 *  moves the clock 50 ms on, so a wait that never ends reaches the bound with no real time. */
function scripted(
  table: Record<number, (read: number) => ProcStat | undefined>,
  gone: readonly number[] = []
) {
  const events: string[] = [];
  const reads = new Map<number, number>();
  let clock = 0;
  const machine: Collaborators = {
    kill: (pid, signal) => {
      events.push(`kill ${pid} ${signal}`);
      if (gone.includes(-pid)) throw Object.assign(new Error("no such process"), { code: "ESRCH" });
    },
    now: () => clock,
    pause: async () => {
      clock += 50;
    },
    run: (command) => {
      events.push(`run ${command.join(" ")}`);
      return command[0] === "docker" ? "exited 1: Error response from daemon" : undefined;
    },
    stat: (pid) => {
      const read = (reads.get(pid) ?? 0) + 1;
      reads.set(pid, read);
      const seen = table[pid]?.(read);
      events.push(
        `read ${pid} ${seen === undefined ? "absent" : seen.ended ? "ended" : `started ${seen.start}`}`
      );
      return seen;
    },
  };
  return { events, machine };
}

const entries: readonly (readonly [string, Cleanup])[] = [
  ["run directory", { commands: [["rm", "-rf", "dir"]] }],
  ["group 10", { group: 10, start: "100" }],
  ["Postgres a", { awaits: 40, commands: [["pg_ctl", "stop", "a"]], start: "400" }],
  ["group 30", { group: 30, start: "300" }],
  ["container b", { awaits: 41, commands: [["docker", "rm", "b"], ["true"]], start: "410" }],
];

test("reap sends every SIGKILL first, runs no command while a start is under way, and unwinds the entries", async () => {
  // Process 40 is found running on its first two reads; 41 has ended; group 30's leader runs and
  // the kill finds no member left.
  const { events, machine } = scripted(
    {
      10: () => ({ ended: false, start: "100" }),
      30: () => ({ ended: false, start: "300" }),
      40: (read) => ({ ended: read > 2, start: "400" }),
    },
    [30]
  );
  const reaped = await reap(entries, machine);
  expect(events).toEqual([
    "read 10 started 100",
    "kill -10 SIGKILL",
    "read 30 started 300",
    "kill -30 SIGKILL",
    "read 40 started 400",
    "read 41 absent",
    "read 40 started 400",
    "read 41 absent",
    "read 40 ended",
    "read 41 absent",
    "read 40 ended",
    "read 41 absent",
    "run docker rm b",
    "run true",
    "run pg_ctl stop a",
    "run rm -rf dir",
  ]);
  expect(reaped).toEqual({
    awaited: 2,
    killed: 1,
    leftAlone: 0,
    notes: ["container b: `docker rm b` exited 1: Error response from daemon"],
    ran: 4,
  });
});

test("reap gives a start that never ends up at its bound, names it, and still runs every command", async () => {
  const { events, machine } = scripted({ 40: () => ({ ended: false, start: "400" }) });
  const reaped = await reap(entries, machine);
  expect(reaped.notes).toEqual([
    "group 10: left alone, no process has its number now",
    "group 30: left alone, no process has its number now",
    "Postgres a: process 40 still runs after 120 s",
    "container b: `docker rm b` exited 1: Error response from daemon",
  ]);
  expect(events.filter((event) => event.startsWith("run "))).toEqual([
    "run docker rm b",
    "run true",
    "run pg_ctl stop a",
    "run rm -rf dir",
  ]);
  // 120 s of 50 ms pauses, each followed by one read of process 40, and the read that names it.
  expect(events.filter((event) => event === "read 40 started 400")).toHaveLength(2_400 + 2);
});

test("reap signals no number another process holds now, none no process holds, and a group whose leader has died with its members", async () => {
  const { events, machine } = scripted({
    // Group 10's number names a process that started later: another starter's server.
    10: () => ({ ended: false, start: "999" }),
    // Group 30's leader has died and waits to be reaped, its number and members still the run's.
    30: () => ({ ended: true, start: "300" }),
    // Group 50 was handed over with no start time, and runs.
    50: () => ({ ended: false, start: "500" }),
    // The awaited start's number names a process that started later.
    40: () => ({ ended: false, start: "777" }),
  });
  const reaped = await reap(
    [
      ["group 10", { group: 10, start: "100" }],
      ["group 20", { group: 20, start: "200" }],
      ["group 30", { group: 30, start: "300" }],
      ["group 50", { group: 50 }],
      ["Postgres a", { awaits: 40, start: "400" }],
    ],
    machine
  );
  expect(events).toEqual([
    "read 10 started 999",
    "read 20 absent",
    "read 30 ended",
    "kill -30 SIGKILL",
    "read 50 started 500",
    "read 40 started 777",
    "read 40 started 777",
  ]);
  expect(reaped).toEqual({
    awaited: 1,
    killed: 1,
    leftAlone: 3,
    notes: [
      "group 10: left alone, its number now names a process that started at 999, not 100",
      "group 20: left alone, no process has its number now",
      "group 50: left alone, no start time was recorded for it",
    ],
    ran: 0,
  });
});
