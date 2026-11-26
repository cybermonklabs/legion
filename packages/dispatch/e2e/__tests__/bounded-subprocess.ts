/** The module that starts every one-shot subprocess a row or hook of this directory waits for,
 *  but the two stated edges below: the argv, the working directory, the environment the caller
 *  chose, how the pipes are read, the bound that stops a child that is stuck, and the line the
 *  run that started a child prints on its own stderr when that child ends. It also holds the
 *  limit a row states for itself, which is read from the bounds of what that row waits for, and
 *  `whether` and `eventually`, the wait a row makes for a state another process reaches, which
 *  the short bound ends.
 *
 * That line, and the failure of a child its bound stopped, go to the run's stderr, which CI keeps
 * in a public log, so both name the child as `named` writes it.
 *
 * When a child ends, or its bound passes, `runBounded` kills every process the child started
 * (`stopTreeYielding`, which reads the process table without holding this process's event loop,
 * so a server a row runs in this process answers meanwhile), and a kill whose token match is
 * inverted takes every other process of the pod, so a plant in `treeOf`, `killSteps`, `stopTree`
 * or the table it reads (`processTable`, `processTableYielding`, `entryReads`, `procText`,
 * `procTextYielding`, `listedOf`, `statFields`) runs against the scripted rows of
 * `bounded-subprocess.test.ts` alone, through a `-t` filter.
 *
 * A process a row or hook drives rather than waits for starts outside it by design, since a bound
 * would kill a process its row is still using: the probe the seven driven rows of
 * `delivery-live-probe.test.ts` read line by line and stop, the runner the interrupted-runner row
 * of `bounded-subprocess.test.ts` interrupts, the `sleep` its cgroup row starts in a scope of its
 * own and stops, the proxies the hooks of `round-robin-proxy.test.ts`
 * and `plain-http-proxy.test.ts` start, the fake Envoy of `fake-envoy.test.ts`, the hosts of the
 * fake Dispatches and of the stand-in edges that `fakeHost` of `outside-path-reach-fixtures.ts`
 * starts, one for each fake a row of the four `outside-path-reach*` test files or of the three
 * `outside-edge-refusals*` ones asks for, the processes `local-instance-support.test.ts` starts
 * through its `Supervisor`, the stand-in Dispatches `prepareStandIn` of
 * `outside-agent-tailnet-stand-ins.ts` starts, the squatters `squat` of
 * `outside-agent-acceptance.test.ts` leaves running until its row ends, and the `harness-up.ts`
 * the SIGTERM row of `harness-start.test.ts` signals while its start waits, which that row kills,
 * with the stand-in Envoy it holds, when it fails. Three subprocesses a row
 * can wait for run outside it, as stated edges. One is the psql of `e2e/psql.ts` (the rows and
 * hooks of `psql-session.test.ts`, and the scratch databases of `replace-blocks-guard.test.ts`),
 * whose `sqlAt` `e2e/harness-postgres.ts` and `e2e/local-instance.ts` call too, so bounding that
 * psql would reach past `e2e/__tests__/`. Another is the `ps` that reads the process table on a
 * machine with no `/proc`, here (`processTable`) and in `bounded-subprocess.test.ts`, which runs
 * with no bound. Linux lists its processes under `/proc` and never runs that `ps`. The third is
 * the shell of two synchronous waits for a port, each ended by its own clock within 11 s: the one
 * `squat` of `outside-agent-acceptance.test.ts` waits for while it takes a port over, synchronous
 * because the listener stop it follows is and the port must be taken before that stop returns,
 * and the stand-in Dispatch's readiness wait in `outside-agent-tailnet-stand-ins.ts`.
 *
 * `bun test` collects only a file whose name carries a `test` or `spec` token, `*.test.ts` and
 * its kin (`collectedTestFile` of `e2e/row-limits.ts` holds the measured set), so a plain `.ts`
 * module under `__tests__/` runs nowhere by itself, and `e2e/tools/row-map.ts` can import it, as
 * it already imports `e2e/__tests__/playwright-listing.ts`.
 *
 * What a bound is. A bound stops a child that is stuck. It is no budget for a slow one. The
 * end-line and late-open rows of `delivery-live-probe.test.ts` read the probe's own `open` and
 * `end` lines. One of its rows reads a probe's time against its flag: the hold of a connection
 * never answered. It holds that time to at least half the flag's 1 s and under a limit of at least
 * three times the largest correct reading under load, rounded up, by the rule below, beside its
 * exit code and stderr; a slow probe fails there only when it is more than three times slower
 * than any correct one measured. A bound exists so that a child which will never end fails with a
 * name, before its job is killed from outside.
 *
 * The rule for each value: each bound is at least three times the largest reading of its kind.
 * The readings behind `short`, `listing` and `generation` are the largest cost lines of the
 * five-run whole-lane checks recorded on the issue that set these bounds, at heads 32078f86 through
 * a691dbe5, each run `bun test --parallel --timeout 60000 e2e/__tests__` under nine busy loops,
 * three per CPU, on a pod of 3 CPUs, from 2026-11-15 to 2026-11-16. Three readings are older, and
 * each names the run it came from: the driver's 3.59 s and the listing's 76.20 s and 78.4 s. A
 * later run may read more: a bound holds while it stays at least three times the largest reading
 * of its kind, and that is what a reader of a new run checks. The reading behind `twoStackBuild`
 * was taken from cold, on a pod with four cores.
 *
 * - `short`, against 11.95 s: what the one `playwright test --help` of `playwright-argv.test.ts`
 *   cost. The other short commands, the two compare-recordings runs, the two `bun --eval`
 *   probes, and the harness and import children, read 7.39 s or less; so do the driver of
 *   `replace-blocks-guard.test.ts` and its psql and tail, at most 3.59 s (in the check at
 *   dc68da15 on 2026-11-15), 0.803 s and 0.19 s.
 * - `listing`, against 113.61 s, one of four listings started at once, in a whole-lane run under
 *   nine busy loops, three per CPU, on a pod of 3 CPUs, on 2026-11-16; against 76.20 s, the
 *   largest of the 22 listings of two such runs on 2026-11-15; and against 78.4 s, the spec's
 *   reading of the first listing in a fresh checkout of main at c0c49979, on a pod of 2 CPUs
 *   under six busy loops, on 2026-11-11, whose next two listings read 6.9 s and 7.0 s.
 * - `generation`, 30 s above the listings inside it. The `row-map` generation starts two listings
 *   of its own, so the inner bound fires first and the inner message is what the reader sees. The
 *   30 s is the tool's own start and the work it does outside its two listings. The generation
 *   that `row-map.test.ts` waits for cost 45.23 s at most.
 * - `twoStackBuild`, against about nine minutes, measured from cold on that four-core pod. That
 *   child builds two stacks, and each stack is a Go build and an initdb.
 * - `edgeRun`, against 175.0 s: the slowest of 30 runs, one after another, of
 *   `e2e/acceptance/outside-edge-refusals.sh` against one stand-in edge of its tests, with their
 *   lists and probe bounds, on a dev box of 288 CPUs at a 1-minute load of 1,024 to 1,892 on
 *   2026-11-21. Their median was 3.6 s, and 9 of the 30 took 55 s to 175 s, each a run whose every
 *   probe answered and whose verdict was right, slowed throughout by the starting and ending of
 *   its processes, never by a probe's timeout. A tester's 80 runs of the script against a TLS
 *   edge at a load of about 1,000 to 1,500 the day before read a median of 2.7 s and at most 86.3
 *   s, and under the `short` bound rows of these runs were killed at 60 s in four whole-file runs.
 *   Three times 175.0 s, rounded up to the minute, is 540 s. A run that stops before any probe, at
 *   an input it refuses, stays under `short`.
 * - `codexRun`, against 55.52 s: the slowest of 29 runs of `e2e/acceptance/outside-agent-codex.sh`
 *   against the stand-ins of `outside-agent-codex.test.ts`, its rows started one, two, three and
 *   five at once, on a dev box of 288 CPUs at a 1-minute load of about 1,100 on 2026-11-21. Under
 *   `short` those runs read 1.94 s to 55.52 s, slowed by the start of the 20 or so Bun processes
 *   each run starts and by the leg's polls. Three times 55.52 s, rounded up to the minute, is
 *   180 s. A run that stops before any Codex starts, at an input it refuses, stays under `short`.
 * - `stoppedLeg`, against 28.24 s: the slowest of 35 drivers of the stop rows of
 *   `unit-repeat.test.ts`, each a copy of the repeat tool over a package of one or two files with
 *   its Bun, the stop, the kill or the cancel the row sends, its names runs and its count of what
 *   was left: the seven rows that run such a driver, 5 runs each, on 3 CPUs (`taskset`) with 9
 *   busy loops on them, on a dev box of 288 CPUs at a 1-minute load of 1,233 to 1,466 on
 *   2026-11-21. The others took 6.15 s to 15.92 s. Three times 28.24 s, rounded up to the next
 *   half minute, is 90 s. That driver counted what was left with one `find` over every process's
 *   `/proc/<pid>/cwd`, which took 1.3 to 11.5 s alone on that box; the driver since walks its own
 *   tree instead, and its 35 runs on 8 CPUs (`taskset`, under a CPU quota of 8) with 16 busy loops
 *   on them, on that box at a load of about 400 the same day, took 0.28 s to 2.81 s.
 *
 * Where a stuck child fails. A stuck child fails at its own bound, counted from its start, on a
 * laptop as in CI: a `short` child after 60 s, a stop row's driver (`stoppedLeg`) after 90 s, a
 * listing after 6 minutes, the generation after 6.5 minutes, a run of the Codex leg after 3
 * minutes, a run of the edge check after 9 minutes, and the two-stack build after 30 minutes.
 * A row's limit is what `rowLimit` reads from the bounds of what the row waits for, plus 30 s for
 * its own work, so the child's bound fires before the row's limit and the failure names the child.
 * The lane runs in the `dispatch-ui` job of `.github/workflows/ci.yaml`, whose limit is 15
 * minutes. That job ran green in 158 s (job 130837751961) and in 188 s (job 130834905422) on
 * 2026-11-11, and in the first of those the lane started about 2 minutes in. A stuck listing
 * therefore fails about 8 minutes into the job, and a stuck run of the edge check about 11: more
 * than twice as long as a green run of the job, and inside its limit, so the job reports the
 * failure rather than losing it to a kill from outside. The largest row limit below the two-stack
 * row's is 1 110 000 ms, on the edge check's host-name row, which runs the check twice; a hung run
 * of it fails at its own 540 s bound, and the row's limit fires only when the row itself stalls
 * outside its children. Each row that runs the edge check once states 570 000 ms, and the two rows
 * that start seven `short` children one after another state 450 000 ms (the first rows of
 * `harness-signin.test.ts` and of the `DISPATCH_E2E_PROCESSES` block of
 * `round-robin-proxy.test.ts`, each read from its lists through a spread the guard does not
 * total): a hung child of either fails at its own 60 s bound. The two-stack row's 1 830 000 ms is
 * longer than the job and never fires there: that row skips unless `DISPATCH_E2E_PG_BIN` or
 * `DISPATCH_E2E_PG_IMAGE` names a Postgres, and no job of `ci.yaml` names either.
 */

import { existsSync, readdirSync, readFileSync } from "node:fs";
import { readdir } from "node:fs/promises";
import { relative } from "node:path";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";

/** The bound each kind of child is stopped at, in milliseconds. The header holds the reading
 *  behind each one. */
export const bounds = Object.freeze({
  /** One command that starts nothing: a help, a refusal, a probe, an import. */
  short: 60_000,
  /** One `playwright test --list` run. */
  listing: 360_000,
  /** `bun e2e/tools/row-map.ts`, which starts two listings of its own. */
  generation: 390_000,
  /** The `bun --eval` of `second-global-setup.test.ts`, which builds two stacks. */
  twoStackBuild: 1_800_000,
  /** One run of `e2e/acceptance/outside-edge-refusals.sh` that probes a stand-in edge. */
  edgeRun: 540_000,
  /** One run of `e2e/acceptance/outside-agent-codex.sh` against the stand-ins of its tests. */
  codexRun: 180_000,
  /** One driver of a stop row of `unit-repeat.test.ts`: a copy of the repeat tool, its Bun, the
   *  stop or kill the row sends, and its names runs. */
  stoppedLeg: 90_000,
});

/** The name of a bound, which is what a row and a run name rather than a number. */
export type BoundName = keyof typeof bounds;

/** What a row gets above the children it waits for: the row's own work, and the moment between a
 *  child's bound firing and the row reading the failure. It is why the helper's message reaches
 *  the reader and the row's own clock does not fire. */
const rowHeadroom = 30_000;

/** Children a row starts together. They run at the same time, so they cost the largest bound in
 *  the group and not the sum of the group. */
interface AtOnce {
  readonly atOnce: readonly BoundName[];
}

/** The limit a row states for itself, in milliseconds.
 *
 *  The rule: the row limit is the sum of what the row waits for one after another, where a group
 *  started at once counts as the largest bound in it, plus 30 000 ms.
 *
 *  Each argument is a bound name, or `{ atOnce: [...] }` for children the row starts together. An
 *  argument is never a number. A number is how this directory wrote its limits before, and each
 *  of those numbers was measured from one file run alone, which is the defect this module
 *  answers: under the whole-suite shape one of them fired falsely. The names also say, in the
 *  row's own source, what that row waits for: each child, and each wait the row ends at a bound
 *  itself, such as an `eventually` below, which the short bound ends. A name that is not a bound
 *  is a compiler error rather than a red run. */
export function rowLimit(...waits: readonly (BoundName | AtOnce)[]): number {
  const spent = waits.map((wait) =>
    typeof wait === "string"
      ? bounds[wait]
      : wait.atOnce.reduce((largest, name) => Math.max(largest, bounds[name]), 0)
  );
  return spent.reduce((total, ms) => total + ms, 0) + rowHeadroom;
}

/** What `beforeDeadline` answers where the deadline came before the answer. */
export const pastDeadline = Symbol("past the deadline");

/** Answers `answer` where it settles before `deadline`, a `performance.now()` reading, and
 *  `pastDeadline` where the deadline comes first. A rejection before the deadline rejects here.
 *  The timer is cleared either way. */
export async function beforeDeadline<T>(
  answer: T | Promise<T>,
  deadline: number
): Promise<T | typeof pastDeadline> {
  const timeUp = Promise.withResolvers<typeof pastDeadline>();
  const timer = setTimeout(
    () => timeUp.resolve(pastDeadline),
    Math.max(0, deadline - performance.now())
  );
  try {
    return await Promise.race([answer, timeUp.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** Reads `holds` until it answers true, and answers true; answers false once `ended` answers true
 *  first, or once the short bound has passed. A state another process reaches - a write to a
 *  file, a process leaving the table, a log line - raises no event a row can await, so the row
 *  reads it until it is there. The short bound only ends a wait that would not end by itself: a
 *  state that never comes, or a read of `holds` that never settles. Each read races the time
 *  left, so a read still pending at the bound answers false there. A row that waits here, or in
 *  `eventually` below, names `"short"` in its `rowLimit` for each such wait, so the row's clock
 *  fires after this one. The reads are 25 ms apart, a gap no verdict reads: it is only how soon
 *  after its coming a state is seen. */
export async function whether(
  holds: () => boolean | Promise<boolean>,
  ended: () => boolean = () => false
): Promise<boolean> {
  const deadline = performance.now() + bounds.short;
  for (;;) {
    const answer = await beforeDeadline(holds(), deadline);
    if (answer === pastDeadline) return false;
    if (answer) return true;
    if (ended() || performance.now() > deadline) return false;
    await Bun.sleep(25);
  }
}

/** Waits as `whether` does, with no `ended`, and throws naming `what`, the state the row waited
 *  for, where the short bound passed before it came. */
export async function eventually(
  what: string,
  holds: () => boolean | Promise<boolean>
): Promise<void> {
  if (!(await whether(holds))) {
    throw new Error(`waited the short bound of ${bounds.short} ms for ${what}, and it never came`);
  }
}

/** Answers whether `waiting` has settled yet, without awaiting it. `silence-read.test.ts` keeps
 *  a copy of its own, since importing this module adds its exit and signal listeners. */
export function watch(waiting: Promise<unknown>): () => boolean {
  let settled = false;
  const done = () => {
    settled = true;
  };
  waiting.then(done, done);
  return () => settled;
}

/** What every line this module writes on stderr starts with. It is how a caller that starts
 *  bounded children of its own relays their lines and nothing else of what they printed. */
const marker = "bounded child:";

/** The variable this module adds to a child's environment: the token of every bounded child the
 *  child runs inside, the outermost first and the child's own last, separated by spaces. A child
 *  started inside another bounded child carries that ancestor's token as well as its own, so the
 *  ancestor's kill finds the inner child's processes by the same match. A process the child
 *  starts inherits the variable with the rest of its environment. */
const tokensVariable = "DISPATCH_E2E_BOUNDED_TOKENS";

/** Whether this machine lists its processes under `/proc`, as Linux does. Where it does not, `ps`
 *  lists them, runs with no bound of its own, gives no process's environment, and the kill finds
 *  only the tree below a child that is still running. */
const procfs = existsSync("/proc/self/stat");

/** One process of the machine: its pid, its parent's pid, the bounded tokens of its environment,
 *  when it started, and whether its read could tell its tokens at all. Exported for the rows of
 *  `bounded-subprocess.test.ts` that script the table `stopTree` reads. */
export interface Listed {
  readonly pid: number;
  readonly ppid: number;
  readonly tokens: readonly string[];
  /** When it started, in clock ticks since boot: field 22 of `/proc/<pid>/stat`. 0 where `ps`
   *  lists it. */
  readonly start: number;
  /** Whether its environment read empty although it is neither a zombie nor a kernel thread, so
   *  the read could not tell whether it carries a token (`listedOf`). Never where `ps` lists it. */
  readonly unknown: boolean;
}

/** The bit of field 9 of `/proc/<pid>/stat`, the process's flags, that marks a kernel thread:
 *  `PF_KTHREAD` of the kernel's `include/linux/sched.h`. */
const kernelThread = 0x00200000;

/** The fields of `stat`, the text of a `/proc/<pid>/stat`, from the third on, the state first.
 *  They are split after the last `)`, the one that ends the command name, since that name may
 *  hold spaces and parentheses of its own, so field N of proc(5) is at index N - 3. */
function statFields(stat: string): string[] {
  return stat.slice(stat.lastIndexOf(")") + 2).split(" ");
}

/** A file of a `/proc/<pid>` directory that the table reads. */
type ProcFile = "cgroup" | "environ" | "stat";

/** The text of `<root>/<pid>/<file>`, where `root` is `/proc` or a directory laid out as it is
 *  (`ProcTree`), or undefined where it cannot be read: where the process is gone (ENOENT), and,
 *  for its `environ`, where it is another user's (EACCES). */
function procText(root: string, pid: number | string, file: ProcFile): string | undefined {
  try {
    return readFileSync(`${root}/${pid}/${file}`, "utf8");
  } catch {
    return undefined;
  }
}

/** `procText`, read through `Bun.file`, whose read waits off this process's event loop, so a read
 *  that takes long, as one of a `/proc/<pid>/environ` can, holds up no other work of the loop.
 *  Measured with bun 1.4.4 on 2026-11-22: while a read of a named pipe waited 2 s for its writer,
 *  a timer due 50 ms after the read began fired at 50 ms. `Bun.file` is read when it is called, so
 *  the row of `bounded-subprocess.test.ts` that counts the kill's reads reaches it. */
async function procTextYielding(
  root: string,
  pid: number | string,
  file: ProcFile
): Promise<string | undefined> {
  try {
    return await Bun.file(`${root}/${pid}/${file}`).text();
  } catch {
    return undefined;
  }
}

/** What the table lists of the process `pid`, from `stat` and `environ`, the texts of its
 *  `/proc/<pid>/stat` and `/proc/<pid>/environ`, each undefined where its read failed; or
 *  undefined where `stat` is, since the process is gone. `processTable` reads the two files and
 *  hands them here. Exported for the rows of `bounded-subprocess.test.ts` that hand it built
 *  texts, and hold each condition of it with no process and no clock.
 *
 *  The state, the parent, the flags and the start time are fields 3, 4, 9 and 22 of the stat
 *  (`statFields`). The tokens come from the environ's `tokensVariable` entry, split on spaces.
 *  The environ is the environment the process was started with: the one its last exec wrote. An
 *  environ that could not be read, as another user's cannot, or as a process's that ended after
 *  its stat was read, carries no token and is not `unknown`.
 *
 *  An environ that reads empty says nothing of the tokens, and it reads empty in five cases. A
 *  process in the middle of an exec: Linux gives it the new program's memory first (`exec_mmap`)
 *  and records where the environment lies in that memory only once the program is loaded
 *  (`create_elf_tables` in `fs/binfmt_elf.c`), and its environ reads empty in between. A process
 *  that is exiting, whose memory is gone already. A process started with an empty environment
 *  (`env -i`). A zombie and a kernel thread, which have no memory and read empty for good. So a
 *  process whose environ reads empty is `unknown` unless its state is `Z` or its flags carry
 *  `kernelThread`, and `stopTree` reads again while an unknown process may be one it has to
 *  kill, where it stands in this process's cgroup (`ownUnknown`). A kernel thread is told by its
 *  flag, not by a parent pid of 2: in a pid namespace, as in a container, no kernel thread is
 *  listed and pid 2 is an ordinary process. */
export function listedOf(
  pid: number,
  stat: string | undefined,
  environ: string | undefined
): Listed | undefined {
  if (stat === undefined) return undefined;
  const fields = statFields(stat);
  const prefix = `${tokensVariable}=`;
  const line = environ?.split("\0").find((variable) => variable.startsWith(prefix));
  return {
    pid,
    ppid: Number(fields[1]),
    start: Number(fields[19]),
    tokens: line === undefined ? [] : line.slice(prefix.length).split(" "),
    unknown: environ === "" && fields[0] !== "Z" && (Number(fields[6]) & kernelThread) === 0,
  };
}

/** Where a kill reads the process table: `root`, `/proc` or a directory laid out as it is, and
 *  `own`, the text of the cgroup of the process that reads it, its `<root>/self/cgroup`, read
 *  once, or undefined where it cannot be read. A bounded child starts in that cgroup, and so does
 *  every process the child starts, but one that a service manager or a container runtime places
 *  elsewhere (`ownUnknown`). Exported for the row of `bounded-subprocess.test.ts` that lays out a
 *  `/proc` of its own. */
export interface ProcTree {
  readonly root: string;
  readonly own: string | undefined;
}

/** The tree under `root`, with the cgroup its `self` stands in (`ProcTree`). Exported for the row
 *  of `bounded-subprocess.test.ts` that lays out a `/proc` of its own. */
export function procTreeAt(root: string): ProcTree {
  return { root, own: procText(root, "self", "cgroup") };
}

/** This machine's `/proc`, with this process's cgroup, read once as this module loads; or
 *  undefined where `/proc` is missing and `ps` lists the processes. */
const machineTree = procfs ? procTreeAt("/proc") : undefined;

/** `listed`, an unknown process, as the kill reads it beside `cgroup`, the text of its
 *  `/proc/<pid>/cgroup`, and `own`, the cgroup of the process that reads the table, this
 *  process's (`ProcTree`): unknown only where it stands in this process's cgroup, where the
 *  child's processes start. An unknown process elsewhere is no process of the child's caught in
 *  its exec, and it is not unknown: on a machine that many sessions share, each in a cgroup of its
 *  own, another session's program started with an empty environment (`env -i`) reads unknown for
 *  as long as it runs. Measured on a dev box of 288 CPUs on 2026-11-22: five such programs of
 *  another session's scope, each running 600 s, and a kill of a child that had started before
 *  them read the table `maxReads` times, holding this process's event loop 144.7 s, so a row
 *  timed out at its 105 s with its child long ended.
 *  Where this process's cgroup cannot be read, an unknown process stays unknown, as in a pod
 *  whose processes all stand in one cgroup; where the process's own cannot, it is gone, and not
 *  unknown. Exported for the rows of `bounded-subprocess.test.ts` that hold each of those. */
export function ownUnknown(
  listed: Listed,
  cgroup: string | undefined,
  own: string | undefined
): Listed {
  if (own === undefined || cgroup === own) return listed;
  return { ...listed, unknown: false };
}

/** What the table lists of the entry `entry` of a `/proc` whose reading process stands in the
 *  cgroup `own` (`ProcTree`), as steps that both reads of the table run (`listedAt`,
 *  `listedAtYielding`): it yields the name of each file of the entry it needs, in order, which its
 *  driver reads and hands back to `next`, and returns what `listedOf` makes of the entry's `stat`
 *  and its `environ` and, for a process that reads unknown, of its `cgroup` (`ownUnknown`); or
 *  undefined where the entry names no process, or where its stat cannot be read, since the
 *  process ended between the directory read and that read, and then its environ is not read. */
function* entryReads(
  entry: string,
  own: string | undefined
): Generator<ProcFile, Listed | undefined, string | undefined> {
  if (!/^\d+$/.test(entry)) return undefined;
  const stat = yield "stat";
  const environ = stat === undefined ? undefined : yield "environ";
  const listed = listedOf(Number(entry), stat, environ);
  if (listed?.unknown !== true) return listed;
  return ownUnknown(listed, yield "cgroup", own);
}

/** What the table lists of the entry `entry` of `tree`'s root (`entryReads`), each of its files
 *  read in one synchronous call (`procText`). */
function listedAt(entry: string, { own, root }: ProcTree): Listed | undefined {
  const reads = entryReads(entry, own);
  let step = reads.next();
  while (step.done !== true) step = reads.next(procText(root, entry, step.value));
  return step.value;
}

/** `listedAt`, each file read off this process's event loop (`procTextYielding`). */
async function listedAtYielding(
  entry: string,
  { own, root }: ProcTree
): Promise<Listed | undefined> {
  const reads = entryReads(entry, own);
  let step = reads.next();
  while (step.done !== true) step = reads.next(await procTextYielding(root, entry, step.value));
  return step.value;
}

/** Every process the machine lists, or undefined where neither `/proc` nor `ps` answers.
 *
 *  Under `/proc`, each process is what `listedAt` reads of its entry, in one synchronous pass:
 *  this process's event loop runs nothing until it ends, which on a machine of many processes is
 *  seconds (`processTableYielding`). Only the exit and signal listeners read it so (`stopAll`),
 *  since nothing may wait there; `runBounded` reads `processTableYielding`.
 *
 *  Without `/proc`, one `ps` lists them, run by `Bun.spawnSync` with no bound: a `ps` that never
 *  ends is never stopped, and the kill that read it waits with it. That is a stated edge, not a
 *  bounded child; Linux lists its processes under `/proc` and never runs it. */
function processTable(): Listed[] | undefined {
  if (machineTree !== undefined) {
    const table: Listed[] = [];
    for (const entry of readdirSync(machineTree.root)) {
      const listed = listedAt(entry, machineTree);
      if (listed !== undefined) table.push(listed);
    }
    return table;
  }
  const ps = Bun.spawnSync(["ps", "-A", "-o", "pid=,ppid="], { stderr: "ignore" });
  if (!ps.success) return undefined;
  return ps.stdout
    .toString()
    .split("\n")
    .map((line) => line.trim().split(/\s+/).map(Number))
    .filter(([pid, ppid]) => Number.isInteger(pid) && Number.isInteger(ppid))
    .map(([pid, ppid]) => ({
      pid: pid as number,
      ppid: ppid as number,
      start: 0,
      tokens: [],
      unknown: false,
    }));
}

/** How many entries of `/proc` `processTableYielding` reads at once. Its reads wait off this
 *  process's event loop, so one after another they cost the kill time where they hold the loop no
 *  longer: measured on a dev box of 288 CPUs at a load average of about 1 900 and about 59 000
 *  entries on 2026-11-22, in two rounds, one read of the table took 14.0 s and 18.6 s with one
 *  entry at a time, 3.0 s and 2.1 s with 16 at once, and 2.5 s and 1.8 s with 64, and the loop's
 *  longest stretch without a turn was 121 ms in any of them. */
const entriesAtOnce = 16;

/** `processTable`, read without holding this process's event loop: the directory through
 *  `readdir` and each entry through `listedAtYielding`, whose reads wait off the loop, so a server
 *  this process runs answers while a child's kill reads, as the origin stand-ins of
 *  `outside-edge.test.ts` must, whose answers the outside driver reads against its own deadlines.
 *  Each entry is read as `processTable` reads it, its files one after the other (`entryReads`).
 *  `entriesAtOnce` readers each take the next entry no reader has taken, so a read that waits
 *  holds up its own reader alone, and the table lists the entries in the order the directory
 *  gave them, as `processTable` does, whatever order their reads end in. The table is read over
 *  a longer time than one pass, and a process that started or ended meanwhile is listed or not as
 *  one pass would list one that did so during its own reads. Without `/proc` it is
 *  `processTable`'s one `ps`.
 *
 *  Why no read is synchronous. Measured on a dev box of 288 CPUs and 18 000 to 19 300 processes
 *  on 2026-11-22: one synchronous pass took 0.8 s to 5.4 s at a load average of 1 800 to 2 000,
 *  and 5.7 s to 17.4 s at about 2 400, and a request from another process to a `Bun.serve` of the
 *  reading process took 2.2 s to 16.0 s, each about the pass it met. The passes of children that
 *  end close together ran one after another: in ten runs of the device-login row of
 *  `outside-edge.test.ts` by name, eight children at once, at a load of 1 270 to 2 560, the row's
 *  loop was held for 8.8 s to 41.8 s at a stretch, and in one run of the driver's three test
 *  files a stand-in's answer to the broker client's device authorization came after that client's
 *  own 15 s, so the case read UNKNOWN where its row wants FAIL. A turn of the loop every 32
 *  entries brought the request to 1 ms to 362 ms; but at about 53 000 processes one synchronous
 *  read of a `/proc/<pid>/environ` took 1.5 s to 2.4 s by itself, and the loop was held 2.9 s to
 *  3.7 s at a stretch. A read that waits off the loop delays the kill alone.
 *
 *  It reads `tree`, this machine's `/proc` unless its caller hands another (`ProcTree`).
 *  `runBounded`'s kill hands none. Two rows of `bounded-subprocess.test.ts` hand a directory
 *  they laid out: one holds the kill's cgroup scope with an unknown process in a cgroup other
 *  than the tree's own, so the scope is held where no process can be started in another cgroup,
 *  as in a pod; the other holds that a read which waits holds no timer of this process. Those
 *  rows are why this is exported. */
export async function processTableYielding(tree = machineTree): Promise<Listed[] | undefined> {
  if (tree === undefined) return processTable();
  const entries = await readdir(tree.root);
  const listed: (Listed | undefined)[] = [];
  let next = 0;
  await Promise.all(
    Array.from({ length: entriesAtOnce }, async () => {
      for (let at = next++; at < entries.length; at = next++) {
        listed[at] = await listedAtYielding(entries[at] as string, tree);
      }
    })
  );
  return listed.filter((one): one is Listed => one !== undefined);
}

/** The pids one child's kill takes from `table`: every process whose environment carries `token`,
 *  the child itself where `root` names it, and every descendant of those by parent pid. This
 *  process is never among them. */
function treeOf(table: readonly Listed[], token: string, root: number | undefined): Set<number> {
  // Each child is pushed onto its parent's list. A list copied for every child it gains takes time
  // that grows with the square of a parent's count, and pid 2 lists every kernel thread: about
  // 36,000 on a dev box, where that walk held this process's event loop 1.8 s to 3.0 s.
  const children = new Map<number, number[]>();
  for (const { pid, ppid } of table) {
    const siblings = children.get(ppid);
    if (siblings === undefined) children.set(ppid, [pid]);
    else siblings.push(pid);
  }
  const found = new Set<number>();
  const seeds = table.filter(({ tokens }) => tokens.includes(token)).map(({ pid }) => pid);
  if (root !== undefined) seeds.push(root);
  while (seeds.length > 0) {
    const pid = seeds.pop() as number;
    if (found.has(pid)) continue;
    found.add(pid);
    seeds.push(...(children.get(pid) ?? []));
  }
  found.delete(process.pid);
  return found;
}

/** The most reads of the process table one kill makes. Each read kills what it finds, so only a
 *  tree that keeps forking faster than it is killed reaches this, or an `unknown` process started
 *  since the child in this process's cgroup that stays unknown, such as one started with an empty
 *  environment: the reads wait on it, `execPause` apart, until this bound (`ownUnknown`).
 *  Exported for the row of `bounded-subprocess.test.ts` that counts those reads. */
export const maxReads = 100;

/** How long a kill waits, in milliseconds, before it reads the table again for an `unknown`
 *  process alone (`stopTree`). No verdict rests on it: the reads end at the first one that finds
 *  nothing new and lists no such process, and `maxReads` bounds them. The wait hands the
 *  processor to the exec the kill waits on, which a loaded machine would otherwise share with
 *  this loop. Measured in a gVisor pod of 3 CPUs under nine busy loops, on 2026-11-16, a kill
 *  that waits so missed nothing in 500 runs of the row of `bounded-subprocess.test.ts` whose
 *  child leaves a `setsid sleep` behind, nor in 2 000 runs of the same child in one loop, where
 *  a kill that read the table once missed 2 and 5 (`stopTree`). Exported for the row of
 *  `bounded-subprocess.test.ts` that runs each kill's own `pause` and reads the wait it asks
 *  `Bun.sleepSync` or `Bun.sleep` for. */
export const execPause = 5;

/** One bounded child being waited for, the token its environment carries, and when it started:
 *  field 22 of its `/proc/<pid>/stat`, in clock ticks since boot, read once as it spawns. Every
 *  process the child starts starts after it. It is 0 where `/proc` is missing, or where the child
 *  was gone before that read, and 0 counts every process as started since the child. `child` is
 *  what `stopTree` reads of the child and does to it: a `Subprocess` has it, and the rows of
 *  `bounded-subprocess.test.ts` that drive `stopTree` hand a stand-in. */
interface Running {
  readonly child: Pick<Subprocess, "exitCode" | "kill" | "pid" | "signalCode">;
  readonly start: number;
  readonly token: string;
}

/** What `stopTree` reads the process table with, signals a process with and waits with, and
 *  whether that table comes from `/proc`. Each defaults to the real one: the rows of
 *  `bounded-subprocess.test.ts` that drive the kill over a scripted table pass their own, one of
 *  them passing `read` and `procfs` alone, and no other caller passes any. */
interface Collaborators {
  /** Whether `read` lists processes from `/proc`, with their tokens and `unknown` reads: this
   *  machine's `procfs`. The rows pass it, so they run either path on any machine. */
  readonly procfs?: boolean;
  /** Reads the process table: `processTable`. */
  readonly read?: () => readonly Listed[] | undefined;
  /** Sends SIGKILL to one pid, and may throw where it is gone: `process.kill`. */
  readonly signal?: (pid: number) => void;
  /** Waits after a read that finds nothing new but lists an `unknown` process that started at or
   *  after the child and that this kill has not signalled, the `maxReads`-th read too, after
   *  which no read follows: `Bun.sleepSync(execPause)`. */
  readonly pause?: () => void;
}

/** What `stopTreeYielding` reads the process table with, signals a process with and waits with:
 *  `Collaborators`, whose `read` and `pause` may answer a promise, which the kill awaits. Each
 *  defaults to the real one: `processTableYielding`, `process.kill` and `Bun.sleep(execPause)`.
 *  The scripted rows of `bounded-subprocess.test.ts` pass their own, as they do to `stopTree`. */
interface YieldingCollaborators {
  readonly procfs?: boolean;
  readonly read?: () => readonly Listed[] | undefined | Promise<readonly Listed[] | undefined>;
  readonly signal?: (pid: number) => void;
  readonly pause?: () => void | Promise<void>;
}

/** The real `signal` of both kills: SIGKILL through `process.kill`, read when it is called, so the
 *  row that stands in for `process.kill` reaches it. */
function sigkill(pid: number): void {
  process.kill(pid, "SIGKILL");
}

/** One kill's loop, as `stopTree` describes it, which both kills run: it yields `"read"` where it
 *  needs the process table, which its driver hands back to `next`, and `"pause"` where it waits
 *  `execPause` before it reads again. `stopTree` answers each in one synchronous pass, and
 *  `stopTreeYielding` awaits each, so the two kills take the same processes in the same order. */
function* killSteps(
  { child, start, token }: Running,
  fromProc: boolean,
  signal: (pid: number) => void
): Generator<"read" | "pause", void, readonly Listed[] | undefined> {
  const alive = child.exitCode === null && child.signalCode === null;
  if (!alive && !fromProc) return;
  // The child's pid counts as killed from the start, so nothing but `child.kill` signals it.
  const killed = new Set<number>(alive ? [child.pid] : []);
  for (let reads = 0; reads < maxReads; reads++) {
    const root = alive && reads === 0 ? child.pid : undefined;
    const table = (yield "read") ?? [];
    const found = treeOf(table, token, root);
    if (root !== undefined) child.kill("SIGKILL");
    const fresh = [...found].filter((pid) => !killed.has(pid));
    for (const pid of fresh) {
      try {
        signal(pid);
      } catch {
        // ESRCH: it ended already.
      }
      killed.add(pid);
    }
    if (!fromProc) break;
    if (fresh.length > 0) continue;
    const pending = table.some(
      (listed) => listed.unknown && listed.start >= start && !killed.has(listed.pid)
    );
    if (!pending) break;
    yield "pause";
  }
}

/** Kills one bounded child and every process it started.
 *
 *  What it reaches: the child while it runs, every process below it by parent pid, and, under
 *  `/proc`, every process whose environment carries the child's token, wherever it now stands in
 *  the tree. The token is what reaches a process whose parent ended and left it re-parented: a
 *  Postgres that `pg_ctl` started, or a grandchild that still holds a pipe after the child itself
 *  exited. Each read of the table is followed by SIGKILL to every process it found. The table is
 *  read again, up to `maxReads` times, until a read finds nothing it has not killed already and
 *  lists no `unknown` process that started at or after the child and that this kill has not
 *  signalled (`listedOf`). A process forked between one read and the kill of its parent is
 *  re-parented when that parent dies, and the next read finds it by the token it inherited.
 *
 *  An unknown process is read again because it may be one of the child's in the middle of an
 *  exec, whose token reads again once the exec has loaded its program. A child that starts a job
 *  and exits at once, as `bash -c 'setsid sleep 150 &'` does, can end while that job execs
 *  `setsid` or `sleep`. In a gVisor pod of 3 CPUs under nine busy loops, on 2026-11-16, a kill
 *  that stopped at its first read missed that sleep in 2 of 500 runs of its row and in 5 of
 *  2 000 runs of the same child in one loop, and at each of those 5 the sleep's process read
 *  empty, in state `R`, at that read; the row failed the same way in CI, in job 133098513525. A
 *  read made for unknown processes alone waits `execPause` first, and 500 and 2 000 such runs
 *  with those reads missed nothing (`execPause`). An unknown process that started before the
 *  child cannot carry its token, and one this kill signalled is exiting, not exec'ing, so
 *  neither keeps the reads going.
 *
 *  The rows of `bounded-subprocess.test.ts` that drive this loop over a scripted table, through
 *  the second argument below, hold each condition of it and of `treeOf` with no clock and no real
 *  process, and each drives `stopTreeYielding` over the same table and holds that it does the
 *  same. Of the reads: a process read `unknown` and then with the child's token is read again
 *  after one pause and killed, and the next read, which finds nothing new, ends the reads; an
 *  unknown process that started before the child, or that this kill signalled, ends them at once,
 *  as a process started since the child that reads whole without the token does; and one that
 *  stays unknown ends them at `maxReads`. Others hold what is taken: a running child through
 *  `child.kill` alone, with SIGKILL, and what it started by parent pid, a process whose tokens
 *  hold the child's first, last or in the middle, an ended child by its token alone, the one read
 *  where `/proc` is missing, this process never, and a walk that ends where the table lists a
 *  loop of parents. One row hands each kill `read` and `procfs` alone, and stands in for
 *  `process.kill`, `Bun.sleepSync` and `Bun.sleep` instead, so the default `signal` sends
 *  SIGKILL and the default `pause` waits `execPause`. The row whose child leaves a `setsid sleep`
 *  behind does not hold the second read: a kill without it misses that sleep only where the sleep
 *  is caught in its exec, which was 2 of the 500 runs above.
 *
 *  Nothing is stopped with SIGSTOP first. A child shares this process's group, and where that
 *  group is orphaned - no member's parent stands in another group of the same session, as under
 *  `setsid` or in a pod - the kernel sends SIGHUP to every member of a group that holds a stopped
 *  process when a member exits, which is POSIX's rule for orphaned groups. That takes this
 *  process and the test runner with the child: a probe that stopped a child before killing it
 *  ended this process by SIGHUP, exit 129.
 *
 *  What it does NOT reach: a process that both left the tree, by its parent ending, and carries
 *  no token, because it cleared or replaced its environment (`env -i`) or runs as another user
 *  whose environment this process cannot read; a process that left the tree and is still in the
 *  middle of its exec when the `maxReads` reads end, or that stands in another cgroup than this
 *  process's (`ownUnknown`), since its token cannot be read until the exec has loaded its
 *  program; a process outside this pid namespace, such as a container a
 *  daemon started on a child's request; and, where `/proc` is missing, anything a child left
 *  once that child has exited, and anything forked during the kill, since `ps` gives no
 *  environment to match and the table is read once there, by one `ps` that runs with no bound
 *  (`processTable`).
 *
 *  The child's own pid is signalled only through `child.kill`, and only while the runtime has
 *  not seen the child exit. Once the runtime reaps a child the system may give its pid to another
 *  process, so the child's pid seeds only the first read, taken before the child is killed, and
 *  a child that has exited is found by its token alone.
 *
 *  The second argument is for those rows, which are why this is exported, and no other caller
 *  passes it: each of its fields defaults to the real one (`Collaborators`). `child.kill` is the
 *  child's own, so the rows hand a stand-in child, whose `kill` only records its signal.
 *
 *  It runs in one synchronous pass, its reads of the table and its pauses included, so it holds
 *  this process's event loop until it ends (`processTableYielding`). The exit and signal
 *  listeners call it (`stopAll`), where nothing may wait; `runBounded` calls `stopTreeYielding`. */
export function stopTree(
  run: Running,
  {
    procfs: fromProc = procfs,
    read = processTable,
    signal = sigkill,
    pause = () => Bun.sleepSync(execPause),
  }: Collaborators = {}
): void {
  const steps = killSteps(run, fromProc, signal);
  let step = steps.next();
  while (step.done !== true) {
    if (step.value === "read") {
      step = steps.next(read());
    } else {
      pause();
      step = steps.next();
    }
  }
}

/** `stopTree`, run with this process's event loop turning: it reads the table with
 *  `processTableYielding`, whose reads wait off the loop, and pauses with `Bun.sleep`, so a
 *  server this process runs answers while a child's kill reads. It takes what `stopTree` takes,
 *  in the same order (`killSteps`). One thing a turn changes: the runtime may see a running child
 *  exit, and reap it, between the loop's reading of `alive` and its `child.kill`. Bun's
 *  `Subprocess.kill` then sends nothing: traced on bun 1.4.4, the `kill` of a child the runtime
 *  had seen exit made no `kill` or `pidfd_send_signal` call, and that of a running one made
 *  `kill(pid, SIGKILL)`. So the child's pid is still signalled only while the runtime has not seen
 *  it exit. The second argument is for the scripted rows of `bounded-subprocess.test.ts`
 *  (`YieldingCollaborators`). */
export async function stopTreeYielding(
  run: Running,
  {
    procfs: fromProc = procfs,
    read = processTableYielding,
    signal = sigkill,
    pause = () => Bun.sleep(execPause),
  }: YieldingCollaborators = {}
): Promise<void> {
  const steps = killSteps(run, fromProc, signal);
  let step = steps.next();
  while (step.done !== true) {
    if (step.value === "read") {
      step = steps.next(await read());
    } else {
      await pause();
      step = steps.next();
    }
  }
}

/** The bounded children of this process that are still being waited for. */
const running = new Set<Running>();

/** Kills the tree of every child this instance of the module still waits for, as `e2e/psql.ts`
 *  stops its session from `process.once("exit", ...)`.
 *
 *  A child shares this process's process group, so a signal sent to the group reaches it with
 *  this process: a Ctrl-C at a terminal, or `bun test --parallel` ending its workers when it is
 *  interrupted. The listeners below cover what a group signal does not. SIGINT and SIGTERM sent
 *  to this process's pid alone reach no other member of the group. A process that left the
 *  group, such as a Postgres that `pg_ctl` started, gets no group signal at all, while the token
 *  still finds it. `exit` runs when a script such as `bun e2e/tools/row-map.ts` ends or calls
 *  `process.exit`. A SIGKILL of this process runs nothing here, and then only the group reaches
 *  the children.
 *
 *  Under `bun test` these listeners hold per instance and reach less than that. Measured with
 *  bun 1.4.4: `bun test` runs no `exit` listener when it ends, with or without `--parallel`, and
 *  a SIGTERM to its pid runs the SIGTERM listener in both. Without `--parallel`, the run keeps
 *  one instance of this module, and a child still running when the run ends outlives it. With
 *  `--parallel`, each test file gets a fresh instance in the worker that runs it, and once a
 *  file ends its instance has no listener left; the runtime kills that file's own child, and
 *  what the child started outlives the file and the run. So a `retained(listing(...))` whose
 *  rows were filtered out with `-t` or skipped loses its bound and these listeners when its file
 *  ends, and only a group signal reaches what it started then. A row that awaits its child is
 *  covered without them: `runBounded`'s `finally` kills what the child left before the row
 *  ends. */
const stopAll = (): void => {
  for (const run of running) stopTree(run);
  running.clear();
};
process.once("exit", stopAll);
for (const name of ["SIGINT", "SIGTERM"] as const) {
  process.once(name, () => {
    stopAll();
    // `once` has taken this listener off. When no other listener for the signal is left,
    // raising it again runs its default action, and this process dies by the signal it was sent.
    // When another listener is left, that listener decides how this process ends, and nothing is
    // raised: this listener runs first, since this module registers at import, and a second
    // signal would reach that listener as a second signal. `e2e/harness-pool.ts` reads a second
    // SIGTERM as "do not wait" and would skip the stop of its stacks.
    if (process.listenerCount(name) === 0) process.kill(process.pid, name);
  });
}

/** The directory a file's name in a cost line is read against, so a line names
 *  `e2e/__tests__/pull-request-lane.test.ts` and not an absolute path. */
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

interface Child {
  /** The command and its arguments. */
  readonly argv: readonly string[];
  /** The environment the child is given, or undefined where it inherits this process's. */
  readonly env?: Readonly<Record<string, string | undefined>>;
  /** The caller's `import.meta.url`: the file that starts the child. */
  readonly from: string;
}

/** The three variables `named` leaves out of a child's name: an entry named `HOME`, `PATH` or
 *  `TMPDIR` prints not at all, whatever it holds, a name taken away included. `named` prints
 *  every other name the caller gave. A caller writes these three only to give a child a home, a
 *  path or a temporary directory, never to tell two runs apart, so their names would say nothing
 *  of which run a line is. Not every caller writes all three: `harness-signin.test.ts` gives no
 *  `HOME`, and several callers give no `TMPDIR`. */
const environmentFloor = new Set(["HOME", "PATH", "TMPDIR"]);

/** The environment variables whose value `named` prints, as `NAME=value`: those whose value tells
 *  one run of a command from another, read from the `env` each caller of `runBounded` and
 *  `listing` in this directory passes. `DISPATCH_E2E_PROCESSES` and `DISPATCH_E2E_SIGNIN` choose
 *  the harness's mode, and are what tells apart the listings of `no-browser-rows.test.ts` and the
 *  imports and `run-server.sh` runs of `harness-signin.test.ts` and `round-robin-proxy.test.ts`;
 *  `TEST_PARALLEL_INDEX` chooses the stack a worker reads in `harness-stacks.test.ts`;
 *  `DISPATCH_E2E_SERVER_BIN`, the path of the server binary `run-server.sh` runs, is all that
 *  tells two of its runs in `round-robin-proxy.test.ts` apart; and `DISPATCH_E2E_CASE` is set by
 *  a caller whose runs of one command differ only in a variable whose value is not printed. It
 *  exists to label the run: the child never reads it. Such a caller gives each of its cases a
 *  value of its own, and a later caller with the same need sets this variable too, so this list
 *  does not grow for each caller. A variable joins this list only when its value tells two runs
 *  apart and is no URL, no credential and no path to a secret. `named` reads a listed value
 *  through `masked`, as it reads an argument, so the code holds that rule too for each password
 *  `masked` knows: a caller that puts one in a listed value, as `DISPATCH_E2E_CASE=password=…`,
 *  prints it as `***`. A URL with no password in it, or a secret `masked` does not know, still
 *  prints whole; for those the rule rests on this list alone. */
const runTellingVariables: ReadonlySet<string> = new Set([
  "DISPATCH_E2E_CASE",
  "DISPATCH_E2E_PROCESSES",
  "DISPATCH_E2E_SERVER_BIN",
  "DISPATCH_E2E_SIGNIN",
  "TEST_PARALLEL_INDEX",
]);

/** A text with every line break written as an escape. `local-instance-support.test.ts` hands
 *  `bun -e` a script of several lines, and a cost line that carried those breaks would be four
 *  lines of a run's stderr rather than one, which is neither the one line R4 asks for nor what a
 *  reader greps for. `named` escapes them only after `masked` has read the text: escaped first, a
 *  break before a `password` keyword would leave an `n` before the key, and the key would not
 *  count. */
function oneLine(text: string): string {
  return text.replaceAll("\r", "\\r").replaceAll("\n", "\\n");
}

/** A URL's userinfo password, which `masked` hides: a scheme, `://`, a user, `:`, then everything
 *  up to the last `@` of the same word, a word ending at whitespace, a quote or a backslash. A URL
 *  inside JSON or inside a shell word is read too, and a password that holds `/`, `?`, `#` or `@`
 *  is hidden whole. That hides too much, never too little: a word that holds an `@` after the `:`
 *  that follows a URL's user or host is hidden from that `:` to the last `@` even where no
 *  password is there, as `postgres://127.0.0.1:5433/mine?application_name=a@b` prints as
 *  `postgres://127.0.0.1:***@b`, since a password may hold `/` or `?` and no reader of the text
 *  can tell where one would end. A URL whose user has no `:` after it, as in
 *  `postgres://user@host/db`, or whose word has no `@` after that `:`, as in
 *  `postgres://127.0.0.1:5433/db`, is left as it is. */
const urlPassword = /([a-z][a-z0-9+.-]*:\/\/[^\s:/?#@"'\\]*):[^\s"'\\]*@/gi;

/** The connection parameters whose value libpq reads as a secret, which `passwordParameter`
 *  hides: `password`, `sslpassword`, `oauth_client_secret`, `scram_client_key` and
 *  `scram_server_key`. The list is read from libpq itself, and is read again the same way when
 *  libpq adds a parameter: walk the array `PQconndefaults()` answers, as python3's ctypes does
 *  against `libpq.so.5`, and read each parameter's `dispchar`. libpq marks a parameter whose value
 *  it treats as a password with `*`, which are `password`, `sslpassword` and
 *  `oauth_client_secret`, and a debug parameter with `D`. Of the four `D` parameters,
 *  `scram_client_key` and `scram_server_key` are secrets, keys that postgres_fdw and dblink pass
 *  through and each of which authenticates by itself; `replication` and `sslkeylogfile` are not.
 *  That walk was of the libpq whose `PQlibVersion()` is 180006. A path, such as `passfile`,
 *  `sslkey` or `sslkeylogfile`, is not hidden: it names the file a secret is kept in, not the
 *  secret, and nothing here reads that file. Of the five, only `password` has an environment
 *  variable in that walk, `PGPASSWORD`, so `PGPASSWORD` is the one variable `passwordParameter`
 *  reads. */
const secretKeys = [
  "password",
  "sslpassword",
  "oauth_client_secret",
  "scram_client_key",
  "scram_server_key",
];

/** The keys of `secretKeys` as `passwordParameter` reads a key, any one of them: each character
 *  as itself or as `%` and the hex code of that character, in either case, as `pass%57ord` writes
 *  its `W` and `oauth%5Fclient_secret` its first `_`. A character with no case, such as `_`, has
 *  one code, and the `i` flag of `passwordParameter` reads its hex digits in either case, as
 *  `%5F` or `%5f`. */
const passwordKey = secretKeys
  .map((key) =>
    [...key]
      .map((character) => {
        const [lower, upper] = [character, character.toUpperCase()].map((each) =>
          each.charCodeAt(0).toString(16)
        );
        return lower === upper
          ? `(?:${character}|%${lower})`
          : `(?:${character}|%${lower}|%${upper})`;
      })
      .join("")
  )
  .join("|");

/** The value of a secret keyword, as in `host=h password=*** dbname=d`, of a secret query
 *  parameter, as in `postgres://h/?sslpassword=***&dbname=d`, or of a `PGPASSWORD` assignment, as
 *  in `PGPASSWORD=*** psql`, which `masked` hides. A secret key is any one of `secretKeys`, read
 *  percent-decoded and in any case (`passwordKey`). `PGPASSWORD` is read in any case, as a shell
 *  or `env` writes the variable libpq reads a password from, with no whitespace on either side of
 *  its `=`, since a shell ends the assignment at whitespace. Every key stands at the start of the
 *  text or after a character no key holds, which is anything but a letter, a digit, `_` or `%`:
 *  after whitespace, `?` or `&` where libpq or a shell reads it, after `=` or a quote where a
 *  variable, a flag or JSON holds the connection string, and after `-`, as in `--password=x`. A
 *  key also stands right after short options, `-` and one or more option letters, as psql groups
 *  them (`-XAtqwd<conninfo>`), at the start of a word, where a word starts the text or follows
 *  whitespace, a quote or a backslash. A digit counts as an option letter, since psql groups its
 *  `-1` the same way (`-1Xd<conninfo>`). psql reads `-d<conninfo>`, so `-dpassword=x`,
 *  `-Xdpassword=x`, `-1dpassword=x` and `-dsslpassword=x host=h` hide, and `-ePGPASSWORD=x` and
 *  `-XdPGPASSWORD=x` hide alike. After `-`, any run of letters reads as options, so a longer key
 *  that ends in a secret key is hidden too, as `-dmypassword=x` is, while `mypassword=x`, with no
 *  `-` before it, stays as written. Whitespace may stand on either side of a secret key's `=`, as
 *  libpq reads `password = sekrit`, and it is kept as written. The value starts after that
 *  whitespace and runs to the next whitespace, `&`, quote or backslash, or, where it opens with
 *  `'`, as libpq and a shell quote a value holding a space, to the `'` that closes it. An empty
 *  value, as in `password=`, `password=&dbname=d`, `password=''` or `PGPASSWORD=`, holds no
 *  password and is left as it is. No other key is hidden, but a longer key that ends in a secret
 *  key after `-` and option letters (above). `_` is a character a key holds, and with no `-` and
 *  option letters before it a key is read whole, so `dbname=mine`, `passwords=x`, `mypassword=x`,
 *  `sslpasswords=x`, `mysslpassword=x`, `my_oauth_client_secret=x`, `scram_client_keys=x`,
 *  `-dbname=x`, `-Xdbname=x`, `-dmydb` and `-dpasswords=x` stay, but in `password= dbname=mine`
 *  libpq reads `dbname=mine` as the password, and it is hidden. */
const passwordParameter = new RegExp(
  String.raw`(?:(?<![\w%])|(?<=(?:^|[\s"'\\])-[a-z0-9]+))` +
    String.raw`(pgpassword=|(?:${passwordKey})\s*=\s*)` +
    String.raw`(?:'(?:[^'\\]|\\[\s\S])+'?|[^\s&"'\\]+)`,
  "gi"
);

/** `text` with three kinds of secret written as `***`: a URL's userinfo password
 *  (`urlPassword`); the value of a keyword or query parameter of `secretKeys`, its name read
 *  percent-decoded and in any case; and the value of a `PGPASSWORD=value` assignment inside a
 *  word, in any case, as `env PGPASSWORD=… psql` and `bash -c "PGPASSWORD=… psql"` start a child
 *  (both `passwordParameter`). A variable libpq does not read, such as `DB_PASSWORD`, is not
 *  hidden. A child's command can hold a database URL with a password in it, as the `--dbname`
 *  `replace-blocks-guard.test.ts` hands psql. The cost line and the failure go to the run's
 *  stderr, which CI keeps in a public log, so neither may print a password. Two forms still
 *  show: a value not opened with `'` that holds a raw `&`, quote or backslash shows what follows
 *  that character, as a shell's `PGPASSWORD="…"` does from its opening `"`, and a URL password
 *  holding a quote or backslash is not matched, so it shows whole. Hiding more than a password
 *  costs a reader part of a word, as `urlPassword` does where a URL with no password holds a
 *  later `@`; hiding less would put a password in that log. `named` gives this each argument on
 *  its own, so a mask never reaches past the argument it is given: an argument that ends in an
 *  empty `password=` leaves the next argument whole. */
export function masked(text: string): string {
  return text.replace(urlPassword, "$1:***@").replace(passwordParameter, "$1***");
}

/** What the cost line and the failure call a child: the file that started it, the command with
 *  its arguments, and the environment the caller gave it. Each argument is masked on its own, as
 *  `masked` says, before the arguments are joined and before `oneLine` escapes their breaks, so a
 *  mask never reaches past its own argument. The arguments are read against a list of what to
 *  hide because they are what tells two runs of a file apart, and none of them can be left out.
 *
 *  An environment entry is read against a list of what to show: an entry of
 *  `runTellingVariables` prints as `NAME=value`, its value read by `masked` as an argument is,
 *  and every other entry as its bare `NAME`, whatever its value holds. The cost line and the
 *  failure go to the run's stderr, which CI keeps in a public log, and a list of what to hide
 *  would print any secret it does not know, such as a token or a key a caller hands a child,
 *  where a name alone says the caller gave that variable and shows nothing of its value. A name
 *  the caller takes away, by giving it no value, prints as `NAME=` with nothing after the `=`. A
 *  listed value that is empty, or holds whitespace or a `"`, prints in double quotes, as
 *  `JSON.stringify` writes it: a value never ends at a space inside it, where a reader would
 *  take what follows for another name, and an empty one never reads as a name taken away. So a
 *  bare `NAME` is a variable the caller gave a value this line does not print; `NAME=` is one
 *  the caller took away; `NAME=""` is a listed one it gave the empty value; and `NAME=value`,
 *  which ends at the next space, or `NAME="value"`, which ends at its closing quote, is a listed
 *  one and the value it gave. A `"` in a value quotes it too, so a value that opens with `"` is
 *  always one `JSON.stringify` wrote.
 *
 *  The environment here is read BY NAME: every entry of `env` but the three of
 *  `environmentFloor`, sorted by name, whatever this process's own value for a name is. Reading
 *  the value instead - keeping what differs from this process - would hide the one entry that
 *  tells two children apart: run from a shell that exports `DISPATCH_E2E_PROCESSES=2`, which
 *  this suite documents, the default listing of `no-browser-rows.test.ts` and its multiprocess
 *  listing would carry the same file, the same command and the same environment, and a bound
 *  failure could not say which of them hung. Four callers start the same default listing with
 *  the same floor and nothing else, so the file is what tells those four apart. */
export function named({ argv, env, from }: Child): string {
  const chosen =
    env === undefined
      ? "this process's environment"
      : Object.entries(env)
          .filter(([name]) => !environmentFloor.has(name))
          .sort(([one], [other]) => one.localeCompare(other))
          // A name the caller gives no value to is a name it takes away, which `NAME=` says. A
          // listed value is masked first and then quoted as the legend above says, and
          // `JSON.stringify` writes a line break in it as `\n`.
          .map(([name, value]) => {
            if (value === undefined) return `${name}=`;
            if (!runTellingVariables.has(name)) return name;
            const shown = masked(value);
            return `${name}=${shown === "" || /[\s"]/.test(shown) ? JSON.stringify(shown) : shown}`;
          })
          .join(" ") || "no variable of its own";
  const command = argv.map((word) => oneLine(masked(word))).join(" ");
  return `${relative(dispatchPackage, fileURLToPath(from))} | ${command} | ${chosen}`;
}

/** What one bounded child did. `code` is its exit status and `named` what the cost line called
 *  it, which a caller that refuses a non-zero exit builds its message from. `endedAt` is when the
 *  child and both its pipes had ended, `Date.now()` in milliseconds, read before the kill of what
 *  the child left: the child's own end, for a row that holds a child to a time. The kill reads the
 *  process table, which takes seconds on a machine of many processes (`processTableYielding`), so
 *  the time `runBounded` answers at is the child's end and that kill, and the cost line's time is
 *  both. */
interface BoundedRun {
  readonly code: number;
  readonly endedAt: number;
  readonly named: string;
  readonly stderr: string;
  readonly stdout: string;
}

interface BoundedOptions extends Child {
  /** The bound this child is stopped at. */
  readonly bound: BoundName;
  /** The working directory, or undefined for this process's. */
  readonly cwd?: string;
  /** Writes every cost line this child captured on to this process's stderr. One caller passes
   *  it: the `row-map` generation, whose two listings are bounded children of its own. */
  readonly relaysLines?: boolean;
}

/** How long the pipes get to end after a stuck child's tree is killed, before both readers are
 *  cancelled. A killed process's pipe ends close as it dies, so the pipes end at once when the
 *  kill took every holder; what keeps one open past this is a process the kill did not reach,
 *  and the wait does not hang on it. */
const pipeGrace = 2_000;

/** Reads one pipe to its end into `chunks`, decoding as it goes, so what arrived before a cancel
 *  is kept. A `cancel()` of the reader ends the loop the way the end of the stream does. */
async function drain(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  chunks: string[]
): Promise<void> {
  const decoder = new TextDecoder();
  for (let read = await reader.read(); !read.done; read = await reader.read()) {
    chunks.push(decoder.decode(read.value, { stream: true }));
  }
  chunks.push(decoder.decode());
}

/** Starts one child, waits for it, and answers what it did.
 *
 *  Both pipes are read to their end while the child runs, each by `drain`, and answered whole.
 *  The run that starts the child prints one cost line on its own stderr when that child ends,
 *  and never into the text this answers with: rows of this directory read a child's stderr as
 *  exactly empty.
 *
 *  This never throws for an exit status. Eleven files of this directory run children that are
 *  meant to exit non-zero and read the code - `compare-recordings-refusal.test.ts`,
 *  `delivery-live-probe.test.ts`, `harness-signin.test.ts`, `harness-stacks.test.ts`,
 *  `outside-agent-acceptance.test.ts`, `outside-agent-public.test.ts`,
 *  `outside-edge-refusals.test.ts` (through `outside-edge-refusals-fixtures.ts`),
 *  `replace-blocks-guard.test.ts`, `round-robin-proxy.test.ts`, `unit-repeat.test.ts` and
 *  `visual-run.test.ts` - so a helper that always threw
 *  would fail every row of theirs that reads a refusal. A caller that does refuse a non-zero exit
 *  builds its message from `named`.
 *
 *  The bound covers the whole wait: the child's exit and the end of both pipes, so a process the
 *  child left holding a pipe is stuck as the child itself would be. It holds at any depth, since
 *  a child started inside another bounded child has a bound and a kill of its own, and its
 *  failure is named before its ancestor's bound fires. When the bound passes,
 *  `stopTreeYielding` kills the child and every process it started, the pipes get `pipeGrace` to
 *  end and are then cancelled, and this throws a message naming the file that started the child,
 *  the command with its arguments, the environment the caller gave it, the bound and the time
 *  waited. */
export async function runBounded(options: BoundedOptions): Promise<BoundedRun> {
  const { argv, bound, cwd, env, from, relaysLines = false } = options;
  const started = performance.now();
  const token = crypto.randomUUID();
  const ancestors = process.env[tokensVariable];
  // No `detached`: the child stays in this process's process group, so a signal sent to that
  // group reaches it too. `bun test --parallel` ends its workers by their groups when it is
  // interrupted, and `bounded-subprocess.test.ts` holds that no child outlives that.
  const child = Bun.spawn([...argv], {
    cwd,
    env: {
      ...(env ?? process.env),
      [tokensVariable]: ancestors === undefined ? token : `${ancestors} ${token}`,
    },
    stderr: "pipe",
    stdout: "pipe",
  });
  // Read now, while the child is listed: the kill waits only on unknown processes that started
  // at or after it. A child the runtime reaped already reads as gone, and gets 0.
  const stat = procfs ? procText("/proc", child.pid, "stat") : undefined;
  const start = stat === undefined ? 0 : Number(statFields(stat)[19] ?? 0);
  const run: Running = { child, start, token };
  running.add(run);
  const stdoutReader = child.stdout.getReader();
  const stderrReader = child.stderr.getReader();
  const stdoutChunks: string[] = [];
  const stderrChunks: string[] = [];
  const whole = Promise.all([
    child.exited,
    drain(stdoutReader, stdoutChunks),
    drain(stderrReader, stderrChunks),
  ]);
  let stuck = false;
  // Read once the child and its pipes ended, before its kill; a stuck child throws instead.
  let endedAt = Number.NaN;
  try {
    const settled = await beforeDeadline(whole, performance.now() + bounds[bound]);
    endedAt = Date.now();
    if (settled === pastDeadline) {
      stuck = true;
      await stopTreeYielding(run);
      if ((await beforeDeadline(whole, performance.now() + pipeGrace)) === pastDeadline) {
        // Each pending read ends as the end of its stream would; what each pipe carried so far
        // stays in its chunks.
        stdoutReader.cancel().catch(() => {});
        stderrReader.cancel().catch(() => {});
      }
    }
  } finally {
    // Every way out of this wait, a pipe that rejected included: unless the bound's kill ran
    // already, whatever the child left behind is killed, so nothing it started outlives it, and
    // then this child leaves the set the exit listener kills. It stays in that set while its kill
    // reads, since the loop turns meanwhile and a signal may come. A child that exited is found by
    // its token here and never by its pid, which the runtime has reaped.
    if (!stuck) await stopTreeYielding(run);
    running.delete(run);
  }
  const ms = Math.round(performance.now() - started);
  const name = named({ argv, env, from });
  const stdout = stdoutChunks.join("");
  const stderr = stderrChunks.join("");
  if (relaysLines) {
    for (const line of stderr.split("\n")) {
      if (line.startsWith(marker)) process.stderr.write(`${line}\n`);
    }
  }
  // A stuck child can have exited 0 while a process it left held a pipe, so its own status alone
  // would read like a clean run; `bound passed` says the bound fired whatever that status is.
  const ended =
    (stuck ? " | bound passed" : "") +
    (child.signalCode !== null
      ? ` | killed by ${child.signalCode}`
      : child.exitCode === null
        ? " | still running after SIGKILL"
        : child.exitCode === 0
          ? ""
          : ` | exit ${child.exitCode}`);
  process.stderr.write(`${marker} ${name} | ${ms} ms${ended}\n`);
  if (stuck) {
    throw new Error(
      `the bounded child ${name} did not end inside the ${bound} bound of ${bounds[bound]} ms: ` +
        `it and every process it started were killed after ${ms} ms`
    );
  }
  return { code: await child.exited, endedAt, named: name, stderr, stdout };
}
