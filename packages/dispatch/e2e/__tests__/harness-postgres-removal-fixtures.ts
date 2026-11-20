/** The fixtures of the rows of `harness-postgres-removal.test.ts`,
 *  `harness-postgres-elsewhere.test.ts`, `harness-postgres-container-removal.test.ts`,
 *  `harness-postgres-waits.test.ts` and `harness-postgres-holders.test.ts`: the marks, data
 *  directories and stand-in postmasters a row plants, the start's sweep and a start's claim, each
 *  run in a child of the row, the end of such a child, and the stack's harness and the stand-in
 *  `pg_ctl` a row kills hard. The rows and what they require stay in those files. The stand-in
 *  `docker`, and a start's removal of one container in a child, are in
 *  `harness-postgres-docker-fixtures.ts`; the program of each row's own limit (`Waits`, `Part`,
 *  `rowMs`, `Row`) in `harness-postgres-waits.ts`; and the machine whose clock a row moves for a
 *  start's removal of one container (`rowMachine`) in `harness-postgres-row-machine.ts`. */

import { expect } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";

import type { RemovalBounds } from "../harness-postgres-container-removal";
import * as marks from "../harness-postgres-marks";
import { claimInfix, type StatReader, statMs, statReader } from "../harness-postgres-processes";
import type { RowAnswer, RowCall } from "./harness-postgres-row-machine";
import { boundOf, type Row } from "./harness-postgres-waits";
import {
  parentLives,
  type StandIn,
  standIn,
  untilParentEnds,
  untilThisEnds,
} from "./planted-marks";

const { marksDirectory, runDirectoryPrefix, stackOwner } = marks;

/** The owners a mark of a row carries, each built from this process: this process, which runs;
 *  its pid with another start, which is a gone stack; another boot; another PID namespace;
 *  another time namespace; and none, which is a stack's where there is no /proc. */
export function owners() {
  const runs = stackOwner();
  if (runs === null) throw new Error("this machine has no /proc, so no start judges an owner");
  return {
    runs,
    gone: { ...runs, start: "0" },
    otherBoot: { ...runs, boot: "00000000-0000-0000-0000-000000000000" },
    otherNamespace: { ...runs, pidNamespace: "pid:[1]" },
    otherTime: { ...runs, timeNamespace: "time:[1]" },
    noProc: null,
  };
}

/** The directory every row of the importing test file plants in, and the one its sweeps look in
 *  for run directories elsewhere (`servedUnder` of `removeLeftoverPostgres`): one of this file's
 *  own, made on its first use, and again after the file's `afterAll` removed it (`--rerun-each`
 *  runs the file's hooks each round), read as the kernel spells it. A sweep reads every postmaster
 *  of this user that serves from a run directory, so where it read the machine's, the rows of two
 *  files the lane runs at once (`bun test --parallel`, a fresh module for each file) removed each
 *  other's dead stacks, said so on lines their rows did not expect, and made holder reads their
 *  limits did not count: 13 failures in CI's repeat lane on 2026-11-20. */
let plants: string | undefined;

export function plantsRoot(): string {
  if (plants === undefined || !existsSync(plants)) {
    plants = realpathSync(mkdtempSync(join(tmpdir(), "marks-rows-")));
  }
  return plants;
}

/** A row's scratch: its own `TMPDIR`, in `parent`, and a run directory in it named as the pool
 *  names one. */
export function scratch(parent = plantsRoot()) {
  const tmp = mkdtempSync(join(parent, "marks-trust-"));
  return { tmp, run: mkdtempSync(join(tmp, runDirectoryPrefix)) };
}

/** Writes the mark of stack `index` in `run`, whose owner is `owner`, naming `data` and `bin`. */
export function writeMark(
  run: string,
  owner: unknown,
  data: string,
  bin: string,
  index = 0
): string {
  const sockets = mkdtempSync(join(run, `pg-${index}-`));
  mkdirSync(join(run, marksDirectory), { recursive: true });
  const file = join(run, marksDirectory, `worker-${index}.json`);
  writeFileSync(
    file,
    JSON.stringify({
      owner,
      run,
      identity: marks.identityOf(run),
      sockets,
      postmaster: { bin, data },
    })
  );
  return file;
}

/** A data directory at `path` holding a file, and a `postmaster.pid` naming `pid` when given. */
export function leaveData(path: string, pid?: number): void {
  mkdirSync(path, { recursive: true });
  writeFileSync(join(path, "PG_VERSION"), "18\n");
  if (pid !== undefined) writeFileSync(join(path, "postmaster.pid"), `${pid}\n${path}\n`);
}

/** Who finds a stand-in postmaster: "its TMPDIR", the row's own starts alone, through the run
 *  directory of their TMPDIR, since its program is `stand-in`, run through a link named
 *  `postgres` (the program a mark's `bin` names); or "every start", whose walk of the kernel's
 *  processes (`servedElsewhere`) reads a program named `postgres`, for a row of a run directory
 *  that no start lists. */
export type Seen = "its TMPDIR" | "every start";

/** Puts a copy of `program` in `bin` as the stand-in postmaster that `seen` names, and answers the
 *  path to run it by, `bin/postgres`. */
function postmasterProgram(bin: string, program: string, seen: Seen): string {
  mkdirSync(bin, { recursive: true });
  const postgres = join(bin, "postgres");
  const copy = seen === "every start" ? postgres : join(bin, "stand-in");
  copyFileSync(program, copy);
  chmodSync(copy, 0o755);
  if (copy !== postgres) symlinkSync(copy, postgres);
  return postgres;
}

/** A stand-in postmaster of the cluster in `data`: a copy of `tail` in `bin`
 *  (`postmasterProgram`) that lives until this process ends (`untilThisEnds`), run with `data` for
 *  its working directory, which `data`'s `postmaster.pid` names. */
export function standInPostmaster(bin: string, data: string, seen: Seen): StandIn {
  const [tail = "", ...until] = untilThisEnds();
  const postgres = postmasterProgram(bin, tail, seen);
  mkdirSync(data, { recursive: true });
  const postmaster = standIn(data, [postgres, ...until]);
  leaveData(data, postmaster.pid);
  return postmaster;
}

/** A stand-in postmaster of the cluster in `data` that writes one line to `signals` for each
 *  SIGQUIT it gets, and ends once it got one and `go` exists, or once this process, its parent,
 *  ended (`parentLives`): a copy of `bash` in `bin` that only the row's own starts find
 *  (`postmasterProgram`), run with `data` for its working directory, which `data`'s
 *  `postmaster.pid` names. */
export function signalCountingPostmaster(
  bin: string,
  data: string,
  signals: string,
  go: string
): StandIn {
  const bash = Bun.which("bash");
  if (bash === null) throw new Error("no bash on PATH to stand in for a postmaster");
  const postgres = postmasterProgram(bin, bash, "its TMPDIR");
  mkdirSync(data, { recursive: true });
  const script = `quit=; trap 'echo QUIT >>"$1"; quit=1' QUIT
while ${parentLives}; do
  if [ -n "$quit" ] && [ -e "$2" ]; then exit 0; fi
  sleep 0.05 & wait $!
done`;
  const postmaster = standIn(data, [postgres, "-c", script, "postgres", signals, go]);
  leaveData(data, postmaster.pid);
  return postmaster;
}

/** A FIFO at `path`: a read of it waits, for `read -t` of a stand-in's shell, with no process
 *  forked to wait. */
function fifoAt(path: string): string {
  const made = Bun.spawnSync(["mkfifo", path], { stdout: "ignore", stderr: "pipe" });
  if (made.exitCode !== 0) throw new Error(`mkfifo ${path} failed: ${made.stderr.toString()}`);
  return path;
}

/** A stand-in postmaster of the cluster in `data` whose child outlives it, as the processes of a
 *  postmaster do for a moment after it ends: a copy of `bash` in `bin` that only the row's own
 *  starts find (`postmasterProgram`), run with `data` for its working directory, which `data`'s
 *  `postmaster.pid` names, and a child of it with `data` for its working directory too. On SIGQUIT
 *  the postmaster ends at once; its child runs 0.5 s more, which is longer than a start's poll of
 *  `/proc` and shorter than its wait for the processes of a postmaster it stopped, then writes
 *  `present` to `saw` where `data` still holds its `PG_VERSION`, or `gone`, and ends. Both wait
 *  with `read -t` on a FIFO beside `saw`, so neither forks a process in `data`. The postmaster
 *  lives until this process, its parent, ends, and its child, where no SIGQUIT came, until the
 *  postmaster ends (`parentLives`). */
export function slowReleasingPostmaster(bin: string, data: string, saw: string): StandIn {
  const bash = Bun.which("bash");
  if (bash === null) throw new Error("no bash on PATH to stand in for a postmaster");
  const postgres = postmasterProgram(bin, bash, "its TMPDIR");
  mkdirSync(data, { recursive: true });
  // The child reads "$1" again once its parent ended, since the postmaster's SIGQUIT writes it
  // just before the postmaster ends.
  const child = `until [ -e "$1" ]; do
  ${parentLives} || [ -e "$1" ] || exit 0
  read -r -t 0.05 -u 3 _
done
read -r -t 0.5 -u 3 _
if [ -e PG_VERSION ]; then echo present; else echo gone; fi >>"$2"`;
  const script = `exec 3<>"$3"
trap 'echo >"$1"; exit 0' QUIT
"$BASH" -c "$4" child "$1" "$2" &
while ${parentLives}; do read -r -t 0.05 -u 3 _; done`;
  const fifo = fifoAt(`${saw}.fifo`);
  const postmaster = standIn(data, [
    postgres,
    "-c",
    script,
    "postgres",
    `${saw}.quit`,
    saw,
    fifo,
    child,
  ]);
  leaveData(data, postmaster.pid);
  return postmaster;
}

const removalModule = fileURLToPath(new URL("../harness-postgres-removal.ts", import.meta.url));

const rowMachineModule = fileURLToPath(
  new URL("./harness-postgres-row-machine.ts", import.meta.url)
);

/** The start's sweep, as a child of this file runs it: within the start's own bounds of a
 *  container's removal, or the row's (`RemovalBounds`), where `ANSWER_MS`, `BOUND_MS` and
 *  `WATCH_MS` give them, with the start's own reader of where each path a process names leads, or
 *  the row's (`StatReader`), where `STAT_READER` gives it, and looking elsewhere only in
 *  `SERVED_UNDER`, the file's own directory (`plantsRoot`). Where `ROW_DOCKER` gives a table of
 *  answers (`tableAnswer`), each container's removal runs on a machine whose clock the child moves
 *  (`rowMachine`), beside the sweep's own work in real time, to `BOUND_MS` at most, and the child
 *  prints that machine's calls (`RowCall`) as JSON. */
const sweepScript = `
import { removeLeftoverPostgres } from ${JSON.stringify(removalModule)};
import { rowMachine, tableAnswer } from ${JSON.stringify(rowMachineModule)};
const { ANSWER_MS, BOUND_MS, WATCH_MS, STAT_READER, SERVED_UNDER, ROW_DOCKER } = process.env;
const bounds =
  ANSWER_MS === undefined
    ? undefined
    : { answerMs: Number(ANSWER_MS), boundMs: Number(BOUND_MS), watchMs: Number(WATCH_MS) };
const reader = STAT_READER === undefined ? undefined : JSON.parse(STAT_READER);
if (ROW_DOCKER === undefined) {
  await removeLeftoverPostgres(bounds, reader, SERVED_UNDER);
} else {
  const row = rowMachine(tableAnswer(JSON.parse(ROW_DOCKER)));
  await row.drive(removeLeftoverPostgres(bounds, reader, SERVED_UNDER, row.machine), {
    horizonMs: Number(BOUND_MS),
    beside: true,
  });
  console.log(JSON.stringify(row.calls()));
}
`;

/** The environment that gives a start's removal of a container in a child of this file the
 *  row's `bounds`. */
export function boundsEnv({ answerMs, boundMs, watchMs }: RemovalBounds): Record<string, string> {
  return { ANSWER_MS: String(answerMs), BOUND_MS: String(boundMs), WATCH_MS: String(watchMs) };
}

/** A stand-in for the reader a start's holder read runs (`StatReader`): its program reads the
 *  paths from its standard input, separated by NUL, says it is ready, and writes where each path
 *  leads, in turn, as the start's own reader does; but at a path of `never`, or a path under one,
 *  it writes nothing more and waits, as a stat of a path on a mount whose server hangs waits, until
 *  it is killed or the start that ran it ends (its parent pid changes). It sets itself no bound of
 *  its own: one shorter than the row's limit would end the wait in the start's place, and a start
 *  with no bound would pass the row. Its own clock times the start's wait there, which no load on
 *  the start or the row stretches: it writes to `clock`, under its pid, when the kernel forked it,
 *  with the read and the group of paths it serves (`HOLDER_READ`, "<read> <top>"), and then the
 *  time it reached that path and the time again every 500 ms until it is killed
 *  (`clockOf`), each in seconds since the machine booted, the one clock the kernel gives both a
 *  process's start and `/proc/uptime` on. Its mount table is this process's, with each mount of
 *  `mounts` added after it, a mount point and its type, as the row plants a path of `never` on a
 *  network mount, and with no type where `type` is empty, as a line whose type the start cannot
 *  read: the file `table`, which the stand-in writes. The start waits on one path `bound` ms
 *  (`StatReader.statMs`): the row's own, a few seconds, so the row sees that wait end in that time
 *  and not in the start's own `statMs`. */
export function neverAnswering(
  never: readonly string[],
  clock: string,
  table: string,
  bound: number,
  mounts: readonly { readonly point: string; readonly type: string }[] = []
): StatReader {
  const own = readFileSync("/proc/self/mountinfo", "utf8").trimEnd();
  writeFileSync(
    table,
    [
      own,
      // The fields a line of /proc/self/mountinfo holds, with the planted point and type.
      ...mounts.map(
        ({ point, type }, index) =>
          `${9000 + index} 1 0:${900 + index} / ${point} rw,relatime shared:${900 + index}${type === "" ? "" : ` - ${type} planted:${point} rw`}`
      ),
      "",
    ].join("\n")
  );
  return {
    mounts: table,
    statMs: bound,
    run: [
      process.execPath,
      "--eval",
      `
const { appendFileSync, readFileSync, statSync, writeSync } = require("node:fs");
const never = ${JSON.stringify(never)};
const write = (what, at) => appendFileSync(${JSON.stringify(clock)}, process.pid + " " + what + " " + at + "\\n");
// When the kernel forked this process, which no load after the fork moves: its start in clock
// ticks since boot, 100 a second. Each tick is on that clock too (/proc/uptime): the epoch's clock
// less the boot time /proc/stat gives, a whole second rounded down and moved by each correction
// of the clock since, would put a fork seconds before a tick that came first.
const uptime = () => Number(readFileSync("/proc/uptime", "utf8").split(" ")[0]);
const ticks = Number(readFileSync("/proc/self/stat", "utf8").slice(readFileSync("/proc/self/stat", "utf8").lastIndexOf(")") + 2).split(" ")[19]);
write("forked", ticks / 100 + " " + (process.env.HOLDER_READ ?? ""));
const start = process.ppid;
setInterval(() => {
  if (process.ppid !== start) process.exit(1);
}, 1_000);
const paths = readFileSync(0, "utf8").split("\\0");
writeSync(1, "ready\\n");
for (const path of paths) {
  if (never.some((hung) => path === hung || path.startsWith(hung + "/"))) {
    const tick = () => write("tick", uptime());
    tick();
    setInterval(tick, 500);
    break;
  }
  let line = "-";
  try {
    const stats = statSync(path, { bigint: true });
    line = stats.dev + ":" + stats.ino;
  } catch {}
  writeSync(1, line + "\\n");
}
`,
    ],
  };
}

/** A stand-in for the reader a start's holder read runs (`StatReader`), with this process's mount
 *  table and the start's own bound on one path (`statMs`), which it never runs out, as it answers
 *  each path at once: it reads where each path leads as the start's own reader does, but each path
 *  that is `to` or under it as the same path under `from`: what the kernel answers once `from` has
 *  been renamed to `to`, as another start's claim of a directory renames it (`claimForRemoval` of
 *  `e2e/harness-postgres-removal.ts`). A row so runs a holder read that looked at `from` before
 *  that rename and reads what the processes name after it, an order no row can time: the start
 *  looks at the directory and reads `/proc` with no `await` between them. */
export function readsAfterRename(from: string, to: string): StatReader {
  return {
    mounts: "/proc/self/mountinfo",
    statMs,
    run: [
      process.execPath,
      "--eval",
      `
const { readFileSync, statSync, writeSync } = require("node:fs");
const from = ${JSON.stringify(from)};
const to = ${JSON.stringify(to)};
const paths = readFileSync(0, "utf8").split("\\0");
writeSync(1, "ready\\n");
for (const path of paths) {
  const read = path === to || path.startsWith(to + "/") ? from + path.slice(to.length) : path;
  let line = "-";
  try {
    const stats = statSync(read, { bigint: true });
    line = stats.dev + ":" + stats.ino;
  } catch {}
  writeSync(1, line + "\\n");
}
`,
    ],
  };
}

/** Another start's removal of `CLAIM_PATH` under its claim, as a child of this file runs it: the
 *  claim's name, `<path>.removing-<pid>-<start>` with the child's own pid and start
 *  (`claimInfix`), is a directory the child makes with a directory `d0` in it, and the child holds
 *  `d0` open, as `rmSync` holds each directory it empties, until it is killed, or until this
 *  process, its parent, ends (`untilParentEnds`). The path comes in the environment, as a start's
 *  own arguments name none. */
const removingScript = `
const { mkdirSync, openSync, readFileSync } = require("node:fs");
${untilParentEnds}
const stat = readFileSync("/proc/self/stat", "utf8");
const start = stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19];
const claimed = process.env.CLAIM_PATH + ${JSON.stringify(claimInfix)} + process.pid + "-" + start;
mkdirSync(claimed + "/d0", { recursive: true });
globalThis.held = openSync(claimed + "/d0", "r");
console.log(claimed);
`;

/** Another start's removal of `path` under its claim, in a child of this file (`removingScript`).
 *  `path` keeps its name, so the row's sweep reads it as it was before that claim
 *  (`readsAfterRename`). Answers once the child printed its claim, and charges `row` for the child
 *  (`Row`). */
export async function removingInChild(
  row: Row,
  path: string
): Promise<{ claimed: string; claimer: Subprocess }> {
  const what = `the removal of ${path} under another start's claim`;
  row.child(what);
  const claimer = Bun.spawn([process.execPath, "--eval", removingScript], {
    env: { ...process.env, CLAIM_PATH: path },
    stdout: "pipe",
    stderr: "ignore",
  });
  try {
    const claimed = `${path}${claimInfix}${claimer.pid}-${marks.startOf(readFileSync(`/proc/${claimer.pid}/stat`, "utf8"))}`;
    expect((await firstLineOf(row, claimer, what)).trim()).toBe(claimed);
    return { claimed, claimer };
  } catch (error) {
    await endChild(claimer);
    throw error;
  }
}

/** What a stand-in reader's clock holds (`neverAnswering`), for each reader that waited on a path
 *  of `never`: its pid, when the kernel forked it, and the times it wrote while it waited, in
 *  seconds since the machine booted. */
export function clockOf(clock: string): { pid: number; forked: number; ticks: number[] }[] {
  const lines = existsSync(clock) ? readFileSync(clock, "utf8").split("\n").filter(Boolean) : [];
  const readers = new Map<number, { pid: number; forked: number; ticks: number[] }>();
  for (const line of lines) {
    const [pid = "", what = "", at = ""] = line.split(" ");
    const reader = readers.get(Number(pid)) ?? { pid: Number(pid), forked: Number.NaN, ticks: [] };
    if (what === "forked") reader.forked = Number(at);
    else reader.ticks.push(Number(at));
    readers.set(reader.pid, reader);
  }
  return [...readers.values()].filter((reader) => reader.ticks.length > 0);
}

/** The reader a row's sweep runs, the row's own or the start's (`statReader`), with its program
 *  behind a `bash` that writes to `starts`, each time the sweep starts it, its pid, the read and the
 *  top path it serves (`HOLDER_READ` of `e2e/harness-postgres-processes.ts`), and to
 *  `<starts>.<pid>` the paths it is asked, as the start writes them on its standard input, and
 *  then runs it on those paths, so that the row counts the holder reads its sweep made
 *  (`Row.reads`); its mount table is the reader's own. */
function countedReader(starts: string, reader: StatReader = statReader): StatReader {
  const bash = Bun.which("bash");
  // The sweep's own PATH may hold no program (`startSweep`), so the copy runs by its path.
  const cat = Bun.which("cat");
  if (bash === null || cat === null) {
    throw new Error("no bash or cat on PATH to count the holder reads of a sweep");
  }
  return {
    ...reader,
    run: [
      bash,
      "-c",
      `printf "%s %s\\n" "$$" "$HOLDER_READ" >>"$0"; ${JSON.stringify(cat)} >"$0.$$"; exec "$@" <"$0.$$"`,
      starts,
      ...reader.run,
    ],
  };
}

/** How many readers the sweep that ran `countedReader(starts)` started one after another: for
 *  each of its reads, the most readers that one of its top paths started, or that one path was
 *  asked by, since the top paths of a read start theirs at the same time, a top path starts
 *  another only after one it waited on ran out its bound, and a path is asked once in a read that
 *  stops at a path that answers no stat. So a read that goes on past such a path is counted by the
 *  paths it asks again, whatever it names itself. A row's `reads` counts these, each of which may
 *  take `statStartMs` to start and `statMs` to read. */
function readsMade(starts: string): number {
  const lines = existsSync(starts) ? readFileSync(starts, "utf8").split("\n").filter(Boolean) : [];
  const perRead = new Map<string, { tops: Map<string, number>; paths: Map<string, number> }>();
  for (const line of lines) {
    const [pid = "", ...label] = line.split(" ");
    const read = label[0] ?? "";
    const counts = perRead.get(read) ?? { tops: new Map(), paths: new Map() };
    const top = label.join(" ");
    counts.tops.set(top, (counts.tops.get(top) ?? 0) + 1);
    const asked = existsSync(`${starts}.${pid}`) ? readFileSync(`${starts}.${pid}`, "utf8") : "";
    for (const path of new Set(asked.split("\0").filter(Boolean))) {
      counts.paths.set(path, (counts.paths.get(path) ?? 0) + 1);
    }
    perRead.set(read, counts);
  }
  return [...perRead.values()].reduce(
    (sum, { tops, paths }) => sum + Math.max(0, ...tops.values(), ...paths.values()),
    0
  );
}

/** A start's sweep running in a child with `tmp` for its `TMPDIR` and `empty`, a directory with no
 *  program in it, for its `PATH`, so no row changes the environment of this process, and with the
 *  row's `reader` where it gives one, counted (`countedReader`): what it printed so far, the lines
 *  it printed once it ended, and the child, for a row to end. It charges `row` for the child and,
 *  once the child ended, for the holder reads its sweep made (`Row`). The sweep sets no bound of
 *  its own, but where its CI step cut the row's limit (`Row.cut`) the row ends it at the row's
 *  deadline and throws, naming it (`boundOf`). */
export function startSweep(
  row: Row,
  tmp: string,
  {
    empty = mkdtempSync(join(tmp, "no-programs-")),
    reader,
  }: { empty?: string; reader?: StatReader } = {}
): { stderr(): string; readonly lines: Promise<string[]>; readonly child: Subprocess } {
  const what = `the sweep of ${tmp}`;
  row.child(what);
  const counts = mkdtempSync(join(tmpdir(), "sweep-reads-"));
  const starts = join(counts, "starts");
  const child = Bun.spawn([process.execPath, "--eval", sweepScript], {
    env: {
      TMPDIR: tmp,
      PATH: empty,
      STAT_READER: JSON.stringify(countedReader(starts, reader)),
      SERVED_UNDER: plantsRoot(),
    },
    stdout: "ignore",
    stderr: "pipe",
  });
  let stderr = "";
  const read = (async () => {
    const stream = child.stderr.getReader();
    const decoder = new TextDecoder();
    for (let chunk = await stream.read(); !chunk.done; chunk = await stream.read()) {
      stderr += decoder.decode(chunk.value, { stream: true });
    }
  })();
  const lines = (async () => {
    const { ms, named } = boundOf(row, Number.POSITIVE_INFINITY);
    const code = await (Number.isFinite(ms)
      ? Promise.race([child.exited, Bun.sleep(ms).then(() => undefined)])
      : child.exited);
    if (code === undefined) await endChild(child);
    await read;
    const made = readsMade(starts);
    rmSync(counts, { recursive: true, force: true });
    row.reads(made, what);
    if (code === undefined) throw new Error(`${what} did not end ${named}: ${stderr}`);
    if (code !== 0) throw new Error(`the sweep exited ${code}: ${stderr}`);
    return stderr.split("\n");
  })();
  return { stderr: () => stderr, lines, child };
}

/** Runs the start's sweep in a child (`startSweep`), and answers the lines it printed. */
export function sweep(row: Row, tmp: string, empty?: string): Promise<string[]> {
  return startSweep(row, tmp, { empty }).lines;
}

/** Waits until `met` holds, and throws naming `what` where it has not after `ms`, or by the row's
 *  deadline where its CI step cut the row's limit (`boundOf`). What it waits for is a write of
 *  another process (a stand-in's signal line, a child's line), which no event of this process
 *  reports, so it reads again every 50 ms. */
export async function until(row: Row, met: () => boolean, what: string, ms: number): Promise<void> {
  const bound = boundOf(row, ms);
  const deadline = Date.now() + bound.ms;
  while (!met()) {
    if (Date.now() >= deadline) throw new Error(`${what} did not come ${bound.named}`);
    await Bun.sleep(50);
  }
}

/** A run directory of another `TMPDIR` than the sweep's, in `parent`, the file's own directory
 *  (`plantsRoot`) but for a row that plants beside it, with what a stack left there: a stand-in
 *  postmaster of stack 0 serving from its data directory, which every start finds, or only that
 *  data directory where `serving` is false, and the mark of `owner` beside it, or no mark where
 *  `owner` is undefined. */
export function leaveElsewhere(owner?: unknown, serving = true, parent = plantsRoot()) {
  const other = mkdtempSync(join(parent, "marks-elsewhere-"));
  const run = mkdtempSync(join(other, runDirectoryPrefix));
  const bin = join(other, "bin");
  const data = join(realpathSync(run), "worker-0", "pgdata");
  if (!serving) leaveData(data);
  const postmaster = serving ? standInPostmaster(bin, data, "every start") : undefined;
  const file = owner === undefined ? undefined : writeMark(run, owner, data, bin);
  return { other, run, bin, data, postmaster, file };
}

/** The mark of a container of the run directory `run`, whose owner is `owner`, beside its socket
 *  directory: what the image provider writes for stack 0. Answers both paths and the name. */
export function leaveContainerMark(run: string, owner: unknown) {
  const sockets = mkdtempSync(join(run, "pg-0-"));
  const container = "dispatch-e2e-pg-no-docker-0";
  mkdirSync(join(run, marksDirectory), { recursive: true });
  const file = join(run, marksDirectory, "worker-0.json");
  writeFileSync(
    file,
    JSON.stringify({ owner, run, identity: marks.identityOf(run), sockets, container })
  );
  return { sockets, container, file };
}

/** A start's claim of `CLAIM_PATH` (`claimForRemoval`) in a child of this file: it prints the
 *  claim, and then waits to be killed, or for the end of this process, its parent
 *  (`untilParentEnds`). No process holds what a row claims so, so it reads none. */
const claimScript = `
import { claimForRemoval } from ${JSON.stringify(removalModule)};
${untilParentEnds}
console.log(JSON.stringify(await claimForRemoval(process.env.CLAIM_PATH, process.getuid(), async () => ({ holders: [], unread: [] }))));
`;

/** SIGKILLs `child`, a child of this file, where it still runs, and waits for its end. */
export async function endChild(child: Subprocess): Promise<void> {
  if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  await child.exited;
}

/** What `child` printed up to its first line feed, or all it printed where it ended first. It
 *  waits with no bound of its own, but where its CI step cut the row's limit (`Row.cut`) it throws
 *  at the row's deadline, naming `what`, the child it waits for (`boundOf`). */
export async function firstLineOf(
  row: Row,
  child: Subprocess<"ignore", "pipe", "ignore">,
  what: string
): Promise<string> {
  const { ms, named } = boundOf(row, Number.POSITIVE_INFINITY);
  const ends = Date.now() + ms;
  let printed = "";
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  try {
    while (!printed.includes("\n")) {
      const chunk = await (Number.isFinite(ms)
        ? Promise.race([
            reader.read(),
            Bun.sleep(Math.max(0, ends - Date.now())).then(() => "late" as const),
          ])
        : reader.read());
      if (chunk === "late") throw new Error(`the first line of ${what} did not come ${named}`);
      if (chunk.done) break;
      printed += decoder.decode(chunk.value, { stream: true });
    }
  } finally {
    reader.releaseLock();
  }
  return printed;
}

/** Claims `path` in a child with the real `claimForRemoval`, and answers once the child printed
 *  its claim: the claimed name, which holds its claimer's pid and start, as the judge reads an
 *  owner, and the claimer, which holds the claim until it is killed (`endChild`). It charges `row`
 *  for the child (`Row`). */
export async function claimInChild(
  row: Row,
  path: string
): Promise<{ claimed: string; claimer: Subprocess }> {
  const what = `the claim of ${path}`;
  row.child(what);
  const claimer = Bun.spawn([process.execPath, "--eval", claimScript], {
    env: { ...process.env, CLAIM_PATH: path },
    stdout: "pipe",
    stderr: "ignore",
  });
  try {
    const claimed = `${path}.removing-${claimer.pid}-${marks.startOf(readFileSync(`/proc/${claimer.pid}/stat`, "utf8"))}`;
    const printed = await firstLineOf(row, claimer, what);
    expect(JSON.parse(printed)).toEqual({ kind: "claimed", path: claimed });
    return { claimed, claimer };
  } catch (error) {
    await endChild(claimer);
    throw error;
  }
}

/** What a start killed between its claim of `path` and its removal leaves: `claimInChild`, and
 *  the claimer SIGKILLed. Answers the claimed name. */
export async function claimAndDie(row: Row, path: string): Promise<string> {
  const { claimed, claimer } = await claimInChild(row, path);
  await endChild(claimer);
  return claimed;
}

const marksModule = fileURLToPath(new URL("../harness-postgres-marks.ts", import.meta.url));

/** A stack's harness, as `e2e/harness-postgres.ts` starts stack 0 of the binaries provider: it
 *  marks its Postgres, makes its data directory and runs `HARNESS_BIN`'s `pg_ctl ... start` on it,
 *  with the arguments the provider gives it, and prints that pg_ctl's pid. Then it waits to be
 *  killed, which is the row's own SIGKILL, or for the end of this process, its parent
 *  (`untilParentEnds`). `HARNESS_DATA`, where it is set, is the data directory as the stack
 *  spells it to `pg_ctl -D`, and `HARNESS_CWD` the directory that `pg_ctl` runs in: a spelling of
 *  its own, which the mark does not hold. */
export const harnessScript = `
import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { markPostgres } from ${JSON.stringify(marksModule)};
${untilParentEnds}
const { HARNESS_RUN: run = "", HARNESS_BIN: bin = "", HARNESS_SOCKETS: sockets = "" } = process.env;
const data = join(run, "worker-0", "pgdata");
markPostgres(run, "worker-0", { sockets, postmaster: { bin, data } });
mkdirSync(data, { recursive: true });
writeFileSync(join(data, "PG_VERSION"), "18\\n");
const options = \`-c listen_addresses='' -k \${sockets}\`;
const log = join(run, "worker-0", "postgres.log");
const spelt = process.env.HARNESS_DATA || data;
const ctl = spawn(join(bin, "pg_ctl"), ["-D", spelt, "-l", log, "-o", options, "-w", "start"], {
  cwd: process.env.HARNESS_CWD || undefined,
  stdio: "ignore",
});
console.log(ctl.pid);
`;

/** A stand-in of `pg_ctl -D <data> ... start` in `bin`, beside a stand-in postmaster that only
 *  the row's own starts find (`postmasterProgram`): it waits for `go`, and then starts that
 *  postmaster with `<data>` for its working directory, names it in `<data>`'s `postmaster.pid` and
 *  ends, as `pg_ctl -w` ends once its server is up; where `<data>` is gone, it fails, as `pg_ctl`
 *  does. It uses shell builtins and that postmaster alone: it waits with `read -t` on a FIFO in
 *  `bin` (`fifoAt`), so it forks no process while it waits. A wait of `sleep 0.05` forked a shell
 *  that named `<data>` in its arguments until it became `sleep`, and a start's holder read that
 *  met that moment named it as a second holder beside `pg_ctl`: 1 of 20 runs of a row on a shared
 *  Linux machine on 2026-11-19, and 2,830 of 301,387 reads of the stand-in's children in 10 s.
 *  The stack that runs it dies first, as a stack killed hard does, so it outlives its parent, and
 *  so does the postmaster it starts: both end on their own once `life` seconds have passed, the
 *  time the row that runs them has left (`lifeOf` of `harness-postgres-waits.ts`); a `pg_ctl`
 *  that `go` did not reach by then starts nothing. */
export function startingPgCtl(bin: string, go: string, life: number): void {
  const sleep = Bun.which("sleep");
  if (sleep === null) throw new Error("no sleep on PATH to stand in for a postmaster");
  const postgres = postmasterProgram(bin, sleep, "its TMPDIR");
  const pause = fifoAt(join(bin, "pg_ctl.pause"));
  writeFileSync(
    join(bin, "pg_ctl"),
    `#!/usr/bin/env bash\nend=$((SECONDS + ${life}))\nexec 3<>${JSON.stringify(pause)}\n` +
      `until [ -e ${JSON.stringify(go)} ] || [ "$SECONDS" -ge "$end" ]; do read -r -t 0.05 -u 3 _; done\n` +
      `[ -e ${JSON.stringify(go)} ] || exit 1\n` +
      // The postmaster gets no descriptor of the FIFO.
      "exec 3<&-\n" +
      // A command that a script runs in the background ignores SIGQUIT unless job control is
      // on, and a start's stop of a postmaster sends SIGQUIT first.
      `cd "$2" || exit 1\nset -m\n${JSON.stringify(postgres)} "$((end > SECONDS ? end - SECONDS : 1))" </dev/null >/dev/null 2>&1 &\n` +
      `echo "$!" >postmaster.pid\n`,
    { mode: 0o755 }
  );
}

/** The lines of `lines` that hold `text`. */
export function holding(lines: readonly string[], text: string): string[] {
  return lines.filter((line) => line.includes(text));
}

/** Runs `script` in a child of this file with `env` over this process's own, and `bin`, a
 *  directory of stand-ins, first on its `PATH`. Answers what it printed on stdout and stderr and
 *  how many seconds it took; kills the child and throws where it has not ended within everything
 *  `row` waits for (`Row.ms`), or by its deadline where its CI step cut its limit (`boundOf`), and
 *  where it exits other than 0. */
export async function inChild(script: string, bin: string, env: Record<string, string>, row: Row) {
  const started = Date.now();
  const { ms, named } = boundOf(row, row.ms);
  const child = Bun.spawn([process.execPath, "--eval", script], {
    env: { ...process.env, ...env, PATH: `${bin}:${process.env.PATH ?? ""}` },
    stdout: "pipe",
    stderr: "pipe",
  });
  const printed = Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  const code = await Promise.race([child.exited, Bun.sleep(ms).then(() => undefined)]);
  if (code === undefined) {
    await endChild(child);
    throw new Error(`the child did not end ${named}`);
  }
  const [stdout, stderr] = await printed;
  if (code !== 0) throw new Error(`the child exited ${code}: ${stderr}`);
  return { stdout, stderr, seconds: (Date.now() - started) / 1000 };
}

const _waitsModule = fileURLToPath(new URL("./harness-postgres-waits.ts", import.meta.url));

/** Runs the start's whole sweep (`sweepScript`) in a child with `tmp` for its `TMPDIR` and `bin`,
 *  a directory holding a stand-in `docker`, first on its `PATH`, within `bounds`, with the start's
 *  own reader, counted (`countedReader`), and charges `row` for the child and for the holder reads
 *  its sweep made. Where `docker` is given, a table of answers (`tableAnswer`), each container's
 *  removal runs on a machine whose clock the child moves, and that answers each `docker` call of
 *  the removal by that table, while the sweep's listing of the containers still runs the stand-in
 *  in `bin`. Answers the lines it printed, how many seconds it took, and the calls of that machine
 *  (`RowCall`, none where no table is given); kills the child and throws where it has not ended
 *  within everything the row waits for (`Row.ms`). */
export async function sweepInChild(
  row: Row,
  tmp: string,
  bin: string,
  bounds: RemovalBounds,
  docker?: Readonly<Record<string, RowAnswer>>
) {
  const what = `the sweep of ${tmp}`;
  row.child(what);
  const counts = mkdtempSync(join(tmpdir(), "sweep-reads-"));
  const starts = join(counts, "starts");
  try {
    const { stdout, stderr, seconds } = await inChild(
      sweepScript,
      bin,
      {
        TMPDIR: tmp,
        ...boundsEnv(bounds),
        STAT_READER: JSON.stringify(countedReader(starts)),
        SERVED_UNDER: plantsRoot(),
        ...(docker === undefined ? {} : { ROW_DOCKER: JSON.stringify(docker) }),
      },
      row
    );
    row.reads(readsMade(starts), what);
    const calls: RowCall[] = docker === undefined ? [] : JSON.parse(stdout);
    return { lines: stderr.split("\n"), seconds, calls };
  } finally {
    rmSync(counts, { recursive: true, force: true });
  }
}
