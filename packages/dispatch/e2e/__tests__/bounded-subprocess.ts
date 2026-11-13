/** The module that starts every one-shot subprocess a row or hook of this directory waits for,
 *  but the two stated edges below: the argv, the working directory, the environment the caller
 *  chose, how the pipes are read, the bound that stops a child that is stuck, and the line the
 *  run that started a child prints on its own stderr when that child ends. It also holds the
 *  limit a row states for itself, which is read from the bounds of what that row waits for, and
 *  `whether` and `eventually`, the wait a row makes for a state another process reaches, which
 *  the short bound ends.
 *
 * A process a row or hook drives rather than waits for starts outside it by design, since a bound
 * would kill a process its row is still using: the probe the six driven rows of
 * `delivery-live-probe.test.ts` read line by line and stop, the runner the interrupted-runner row
 * of `bounded-subprocess.test.ts` interrupts, the proxies the hooks of `round-robin-proxy.test.ts`
 * and `plain-http-proxy.test.ts` start, the fake Envoy of `fake-envoy.test.ts`, and the processes
 * `local-instance-support.test.ts` starts through its `Supervisor`. Two subprocesses a row can
 * wait for run outside it, as stated edges. One is the psql of `e2e/psql.ts` (the rows and hooks
 * of `psql-session.test.ts`), whose `sqlAt` `e2e/harness-postgres.ts` and `e2e/local-instance.ts`
 * call too, so bounding that psql would reach past `e2e/__tests__/`. The other is the `ps` that
 * reads the process table on a machine with no `/proc`, here (`processTable`) and in
 * `bounded-subprocess.test.ts`, which runs with no bound. Linux lists its processes under `/proc`
 * and never runs that `ps`.
 *
 * `bun test` collects only a file whose name carries a `test` or `spec` token, `*.test.ts` and
 * its kin (`collectedTestFile` of `e2e/row-limits.ts` holds the measured set), so a plain `.ts`
 * module under `__tests__/` runs nowhere by itself, and `e2e/tools/row-map.ts` can import it, as
 * it already imports `e2e/__tests__/playwright-listing.ts`.
 *
 * What a bound is. A bound stops a child that is stuck. It is no budget for a slow one. No row
 * fails because a child is slow, and no row reads a child's time: the end-line row and the
 * late-open row of `delivery-live-probe.test.ts` read the probe's own `open` and `end` lines. So
 * a child that is slow and ends is not a defect. A bound exists so that a child which will never
 * end fails with a name, and fails before the job that runs it is killed from outside.
 *
 * The rule for each value: each bound is at least three times the largest reading of its kind.
 * The readings behind `short`, `listing` and `generation` were taken under load in a pod with two
 * CPUs; the reading behind `twoStackBuild` was taken from cold, on a pod with four cores.
 *
 * - `short`, against 8.78 s: what the row of `playwright-argv.test.ts`, which runs one
 *   `playwright test --help`, cost under the whole lane. The other short commands, the two
 *   compare-recordings runs, the two `bun --eval` probes, and the harness and import children,
 *   read 2.5 s or less.
 * - `listing`, against 84.85 s for a whole file of listings run alone, and against 78.4 s for the
 *   first listing in a fresh checkout.
 * - `generation`, 30 s above the listings inside it. The `row-map` generation starts two listings
 *   of its own, so the inner bound fires first and the inner message is what the reader sees. The
 *   30 s is the tool's own start and the work it does outside its two listings. The row of
 *   `row-map.test.ts` that waits for the generation cost 33.47 s under the whole lane.
 * - `twoStackBuild`, against about nine minutes, measured from cold on that four-core pod. That
 *   child builds two stacks, and each stack is a Go build and an initdb.
 *
 * Where a stuck child fails. A stuck child fails at its own bound, counted from its start, on a
 * laptop as in CI: a `short` child after 60 s, a listing after 5 minutes, the generation after
 * 5.5 minutes, and the two-stack build after 30 minutes. A row's limit is what `rowLimit` reads
 * from the bounds of what the row waits for, plus 30 s for its own work, so the child's bound
 * fires before the row's limit and the failure names the child. The lane runs in the
 * `dispatch-ui` job of `.github/workflows/ci.yaml`, whose limit is 15 minutes. That job ran green
 * in 158 s (job 130837751961) and in 188 s (job 130834905422) on 2026-11-11, and in the first of
 * those the lane started about 2 minutes in. A stuck listing therefore fails about 7 minutes into
 * the job: more than twice as long as a green run of the job, and well inside its limit, so the
 * job reports the failure rather than losing it to a kill from outside. The largest row limit
 * below the two-stack row's is 450 000 ms, on the two rows that start seven `short` children one
 * after another (the first rows of `harness-signin.test.ts` and of the `DISPATCH_E2E_PROCESSES`
 * block of `round-robin-proxy.test.ts`, each read from its lists through a spread the guard does
 * not total). A hung child of either fails at its own 60 s bound; the row's limit fires only
 * when the row itself stalls outside its children, at most 7.5 minutes from its start, so such a
 * row, stalled from the lane's start, fails about 9.5 minutes into the job. The
 * two-stack row's 1 830 000 ms is longer than the job and never fires there: that row skips
 * unless `DISPATCH_E2E_PG_BIN` or `DISPATCH_E2E_PG_IMAGE` names a Postgres, and no job of
 * `ci.yaml` names either.
 */

import { existsSync, readdirSync, readFileSync } from "node:fs";
import { relative } from "node:path";
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";

/** The bound each kind of child is stopped at, in milliseconds. The header holds the reading
 *  behind each one. */
export const bounds = Object.freeze({
  /** One command that starts nothing: a help, a refusal, a probe, an import. */
  short: 60_000,
  /** One `playwright test --list` run. */
  listing: 300_000,
  /** `bun e2e/tools/row-map.ts`, which starts two listings of its own. */
  generation: 330_000,
  /** The `bun --eval` of `second-global-setup.test.ts`, which builds two stacks. */
  twoStackBuild: 1_800_000,
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
 *  itself, such as an `eventually` below, which the short bound ends. They make a wrong value a
 *  compiler error rather than a red run. */
export function rowLimit(...waits: readonly (BoundName | AtOnce)[]): number {
  const spent = waits.map((wait) =>
    typeof wait === "string"
      ? bounds[wait]
      : wait.atOnce.reduce((largest, name) => Math.max(largest, bounds[name]), 0)
  );
  return spent.reduce((total, ms) => total + ms, 0) + rowHeadroom;
}

/** Reads `holds` until it answers true, and answers true; answers false once `ended` answers true
 *  first, or once the short bound has passed. A state another process reaches - a write to a
 *  file, a process leaving the table, a log line - raises no event a row can await, so the row
 *  reads it until it is there. The short bound only ends a wait for a state that never comes. A
 *  row that waits here, or in `eventually` below, names `"short"` in its `rowLimit` for each such
 *  wait, so the row's clock fires after this one. The reads are 25 ms apart, a gap no verdict
 *  reads: it is only how soon after its coming a state is seen. */
export async function whether(
  holds: () => boolean | Promise<boolean>,
  ended: () => boolean = () => false
): Promise<boolean> {
  const deadline = performance.now() + bounds.short;
  while (!(await holds())) {
    if (ended() || performance.now() > deadline) return false;
    await Bun.sleep(25);
  }
  return true;
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

/** One process of the machine: its pid, its parent's pid, and the bounded tokens of its
 *  environment. */
interface Listed {
  readonly pid: number;
  readonly ppid: number;
  readonly tokens: readonly string[];
}

/** Every process the machine lists, or undefined where neither `/proc` nor `ps` answers.
 *
 *  Under `/proc`, the parent is the fourth field of `/proc/<pid>/stat`, read after the `)` that
 *  ends the command name, since that name may hold spaces and parentheses of its own. The tokens
 *  come from `/proc/<pid>/environ`, which is the environment the process was started with. A
 *  process of another user has an environ this process cannot read, and it carries no token
 *  here. A process that ends between the directory read and the read of its own files is left
 *  out, since it is gone.
 *
 *  Without `/proc`, one `ps` lists them, run by `Bun.spawnSync` with no bound: a `ps` that never
 *  ends is never stopped, and the kill that read it waits with it. That is a stated edge, not a
 *  bounded child; Linux lists its processes under `/proc` and never runs it. */
function processTable(): Listed[] | undefined {
  if (procfs) {
    const table: Listed[] = [];
    const prefix = `${tokensVariable}=`;
    for (const entry of readdirSync("/proc")) {
      if (!/^\d+$/.test(entry)) continue;
      let stat: string;
      try {
        stat = readFileSync(`/proc/${entry}/stat`, "utf8");
      } catch {
        continue;
      }
      const ppid = Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1]);
      let tokens: string[] = [];
      try {
        const line = readFileSync(`/proc/${entry}/environ`, "utf8")
          .split("\0")
          .find((variable) => variable.startsWith(prefix));
        if (line !== undefined) tokens = line.slice(prefix.length).split(" ");
      } catch {
        // EACCES: another user's process. ENOENT: it ended after its stat was read.
      }
      table.push({ pid: Number(entry), ppid, tokens });
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
    .map(([pid, ppid]) => ({ pid: pid as number, ppid: ppid as number, tokens: [] }));
}

/** The pids one child's kill takes from `table`: every process whose environment carries `token`,
 *  the child itself where `root` names it, and every descendant of those by parent pid. This
 *  process is never among them. */
function treeOf(table: readonly Listed[], token: string, root: number | undefined): Set<number> {
  const children = new Map<number, number[]>();
  for (const { pid, ppid } of table) children.set(ppid, [...(children.get(ppid) ?? []), pid]);
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
 *  tree that keeps forking faster than it is killed reaches this. */
const maxReads = 100;

/** One bounded child being waited for, and the token its environment carries. */
interface Running {
  readonly child: Subprocess;
  readonly token: string;
}

/** Kills one bounded child and every process it started.
 *
 *  What it reaches: the child while it runs, every process below it by parent pid, and, under
 *  `/proc`, every process whose environment carries the child's token, wherever it now stands in
 *  the tree. The token is what reaches a process whose parent ended and left it re-parented: a
 *  Postgres that `pg_ctl` started, or a grandchild that still holds a pipe after the child itself
 *  exited. Each read of the table is followed by SIGKILL to every process it found, and the table
 *  is read again until a read finds nothing it has not killed already. A process forked between
 *  one read and the kill of its parent is re-parented when that parent dies, and the next read
 *  finds it by the token it inherited.
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
 *  whose environment this process cannot read; a process outside this pid namespace, such as a
 *  container a daemon started on a child's request; and, where `/proc` is missing, anything a
 *  child left once that child has exited, and anything forked during the kill, since `ps` gives
 *  no environment to match and the table is read once there, by one `ps` that runs with no bound
 *  (`processTable`).
 *
 *  The child's own pid is signalled only through `child.kill`, and only while the runtime has
 *  not seen the child exit. Once the runtime reaps a child the system may give its pid to another
 *  process, so the child's pid seeds only the first read, taken before the child is killed, and
 *  a child that has exited is found by its token alone. */
function stopTree({ child, token }: Running): void {
  const alive = child.exitCode === null && child.signalCode === null;
  if (!alive && !procfs) return;
  // The child's pid counts as killed from the start, so nothing but `child.kill` signals it.
  const killed = new Set<number>(alive ? [child.pid] : []);
  for (let read = 0; read < maxReads; read++) {
    const root = alive && read === 0 ? child.pid : undefined;
    const found = treeOf(processTable() ?? [], token, root);
    if (root !== undefined) child.kill("SIGKILL");
    const fresh = [...found].filter((pid) => !killed.has(pid));
    for (const pid of fresh) {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // ESRCH: it ended already.
      }
      killed.add(pid);
    }
    if (fresh.length === 0 || !procfs) break;
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

/** The three variables a caller passes only to give a child a home, a path and a temporary
 *  directory. Every caller that names an environment at all writes them, so they tell no two
 *  children apart and the cost line leaves them out. */
const environmentFloor = new Set(["HOME", "PATH", "TMPDIR"]);

/** One word of a command, or one value of an environment, with every line break written as an
 *  escape. `local-instance-support.test.ts` hands `bun -e` a script of several lines, and a cost
 *  line that carried those breaks would be four lines of a run's stderr rather than one, which
 *  is neither the one line R4 asks for nor what a reader greps for. */
function oneLine(text: string): string {
  return text.replaceAll("\r", "\\r").replaceAll("\n", "\\n");
}

/** The password of a URL inside a text, which `named` writes as `***`:
 *  `postgres://user:***@host/db`. `harness-stacks.test.ts` gives a child `LEGION_TEST_PG_DSN` as
 *  its `DATABASE_URL` and inside a stacks JSON, and a cost line or a failure that printed either
 *  whole would put that password on the run's stderr. It is a scheme, `://`, a user, `:`, the
 *  password and `@`, with no space, slash, quote or backslash among them, so a URL inside JSON or
 *  inside a shell word is read too. A password that holds an `@` runs to the last `@` before the
 *  host. */
const urlPassword = /([a-z][a-z0-9+.-]*:\/\/[^\s:/?#@"'\\]*):[^\s/?#"'\\]*@/gi;

/** What the cost line and the failure call a child: the file that started it, the command with
 *  its arguments, and the environment the caller gave it, with every URL's password written as
 *  `***` wherever it stands.
 *
 *  The environment here is read BY NAME: every entry of `env` but the three of
 *  `environmentFloor`, sorted by name, whatever this process's own value for a name is. Reading
 *  the value instead - keeping what differs from this process - would hide the one entry that
 *  tells two children apart: run from a shell that exports `DISPATCH_E2E_PROCESSES=2`, which
 *  this suite documents, the default listing of `no-browser-rows.test.ts` and its multiprocess
 *  listing would carry the same file, the same command and the same environment, and a bound
 *  failure could not say which of them hung. Four callers start the same default listing with
 *  the same floor and nothing else, so the file is what tells those four apart. */
function named({ argv, env, from }: Child): string {
  const chosen =
    env === undefined
      ? "this process's environment"
      : Object.entries(env)
          .filter(([name]) => !environmentFloor.has(name))
          .sort(([one], [other]) => one.localeCompare(other))
          // A name the caller gives no value to is a name it takes away, which `NAME=` says.
          .map(([name, value]) => `${name}=${oneLine(value ?? "")}`)
          .join(" ") || "no variable of its own";
  const command = argv.map(oneLine).join(" ");
  return `${relative(dispatchPackage, fileURLToPath(from))} | ${command} | ${chosen}`.replace(
    urlPassword,
    "$1:***@"
  );
}

/** What one bounded child did. `code` is its exit status and `named` what the cost line called
 *  it, which a caller that refuses a non-zero exit builds its message from. How long the child
 *  took is in the cost line and no caller reads it, so it is no field here. */
interface BoundedRun {
  readonly code: number;
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
 *  This never throws for an exit status. Five files of this directory run children that are
 *  meant to exit non-zero and read the code - `compare-recordings-refusal.test.ts`,
 *  `delivery-live-probe.test.ts`, `harness-signin.test.ts`, `harness-stacks.test.ts` and
 *  `round-robin-proxy.test.ts` - so a helper that always threw would fail every row of theirs
 *  that reads a refusal. A caller that does refuse a non-zero exit builds its message from
 *  `named`.
 *
 *  The bound covers the whole wait: the child's exit and the end of both pipes, so a process the
 *  child left holding a pipe is stuck as the child itself would be. It holds at any depth, since
 *  a child started inside another bounded child has a bound and a kill of its own, and its
 *  failure is named before its ancestor's bound fires. When the bound passes, `stopTree` kills
 *  the child and every process it started, the pipes get `pipeGrace` to end and are then
 *  cancelled, and this throws a message naming the file that started the child, the command with
 *  its arguments, the environment the caller gave it, the bound and the time waited. */
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
  const run: Running = { child, token };
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
  let timer: Timer | undefined;
  // A promise that answers "passed" once `ms` has gone by; the latest timer is the one `finally`
  // clears.
  const passing = (ms: number): Promise<"passed"> => {
    const { promise, resolve } = Promise.withResolvers<"passed">();
    timer = setTimeout(() => resolve("passed"), ms);
    return promise;
  };
  let stuck = false;
  try {
    if ((await Promise.race([whole, passing(bounds[bound])])) === "passed") {
      stuck = true;
      stopTree(run);
      if ((await Promise.race([whole, passing(pipeGrace)])) === "passed") {
        // Each pending read ends as the end of its stream would; what each pipe carried so far
        // stays in its chunks.
        stdoutReader.cancel().catch(() => {});
        stderrReader.cancel().catch(() => {});
      }
    }
  } finally {
    // Every way out of this wait, a pipe that rejected included: the timer goes; this child
    // leaves the set the exit listener kills; and, unless the bound's kill ran already, whatever
    // the child left behind is killed, so nothing it started outlives it. A child that exited is
    // found by its token here and never by its pid, which the runtime has reaped.
    clearTimeout(timer);
    running.delete(run);
    if (!stuck) stopTree(run);
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
  return { code: await child.exited, named: name, stderr, stdout };
}
