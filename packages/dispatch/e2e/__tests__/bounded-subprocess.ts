/** The one bounded subprocess every row of this directory starts, and every hook but the psql of
 *  `e2e/psql.ts`: the argv, the working directory, the environment the caller chose, how the
 *  pipes are read, the bound that stops a child that is stuck, and the line the run that started
 *  a child prints on its own stderr when that child ends. It also holds the limit a row states
 *  for itself, which is read from the bounds of the children that row waits for.
 *
 * `bun test` collects only `*.test.ts`, so a plain `.ts` module under `__tests__/` runs nowhere
 * by itself, and `e2e/tools/row-map.ts` can import it, as it already imports
 * `e2e/__tests__/playwright-listing.ts`.
 *
 * What a bound is. A bound stops a child that is stuck. It is no budget for a slow one. Nothing
 * in this suite reads how long a child takes, so a child that is slow and ends is not a defect.
 * A bound exists so that a child which will never end fails with a name, and fails before the
 * job that runs it is killed from outside.
 *
 * The rule for each value: each bound is at least three times the largest reading of its kind
 * taken under load in a pod with two CPUs.
 *
 * - `short`, against 2.5 s. That is the largest reading of a short command: `playwright test
 *   --help`, the two compare-recordings runs, the two `bun --eval` probes, and the harness and
 *   import children.
 * - `listing`, against 84.85 s for a whole file of listings run alone, and against 78.4 s for the
 *   first listing in a fresh checkout.
 * - `generation`, 30 s above the listings inside it. The `row-map` generation starts two listings
 *   of its own, so the inner bound fires first and the inner message is what the reader sees. The
 *   30 s is the tool's own start and the work it does outside its two listings.
 * - `twoStackBuild`, against about nine minutes measured from cold. That child builds two
 *   stacks, and each stack is a Go build and an initdb.
 *
 * Where a stuck child fails. The lane runs in the `dispatch-ui` job of
 * `.github/workflows/ci.yaml`, whose limit is 15 minutes. The lane starts about 2 minutes into
 * that job, measured in job 130837751961. A stuck listing therefore fails about 7 minutes into
 * the job, and the job reports it rather than losing it to a kill from outside. The largest row
 * limit that can fire there is 450 000 ms, and two rows carry it: the seven-short-children row
 * of `harness-signin.test.ts` and the seven-short-children row of `round-robin-proxy.test.ts`.
 * Either of them fails about 9.5 minutes in. The two-stack row's 1 830 000 ms is longer than
 * the job and never fires there: that row skips unless `DISPATCH_E2E_PG_BIN` or
 * `DISPATCH_E2E_PG_IMAGE` names a Postgres, and no CI job names one. A reader on a laptop waits
 * longer for a failure than before: a stuck child now takes 5 to 7.5 minutes to fail, where it
 * took 60 to 90 seconds.
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
 *  row's own source, which children that row waits for, and they make a wrong value a compiler
 *  error rather than a red run. */
export function rowLimit(...waits: readonly (BoundName | AtOnce)[]): number {
  const spent = waits.map((wait) =>
    typeof wait === "string"
      ? bounds[wait]
      : wait.atOnce.reduce((largest, name) => Math.max(largest, bounds[name]), 0)
  );
  return spent.reduce((total, ms) => total + ms, 0) + rowHeadroom;
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
 *  lists them, gives no process's environment, and the kill finds only the tree below a child
 *  that is still running. */
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
 *  out, since it is gone. */
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
 *  no environment to match and the table is read once there.
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

/** Kills the tree of every child still running when this process ends, as `e2e/psql.ts` stops
 *  its session from `process.once("exit", ...)`.
 *
 *  A child shares this process's process group, so a signal sent to the group reaches it with
 *  this process: a Ctrl-C at a terminal, or `bun test --parallel` ending its workers when it is
 *  interrupted. The listeners below cover what a group signal does not. `exit` runs on a normal
 *  end and on `process.exit`, after which nothing would stop a child still running. SIGINT and
 *  SIGTERM sent to this process's pid alone end it without `exit` running and without reaching
 *  the group. And a process that left the group, such as a Postgres that `pg_ctl` started, gets
 *  no group signal at all, while the token still finds it. A SIGKILL of this process runs
 *  nothing here, and then only the group reaches the children. */
const stopAll = (): void => {
  for (const run of running) stopTree(run);
  running.clear();
};
process.once("exit", stopAll);
for (const name of ["SIGINT", "SIGTERM"] as const) {
  process.once(name, () => {
    stopAll();
    // `once` has taken this listener off, so raising the same signal here runs its default
    // action: this process dies by the signal it was sent.
    process.kill(process.pid, name);
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
 *  Both pipes are read whole, as the nine hand-rolled copies in this directory read them before.
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
  const ended =
    child.signalCode !== null
      ? ` | killed by ${child.signalCode}`
      : child.exitCode === null
        ? " | still running after SIGKILL"
        : child.exitCode === 0
          ? ""
          : ` | exit ${child.exitCode}`;
  process.stderr.write(`${marker} ${name} | ${ms} ms${ended}\n`);
  if (stuck) {
    throw new Error(
      `the bounded child ${name} did not end inside the ${bound} bound of ${bounds[bound]} ms: ` +
        `it and every process it started were killed after ${ms} ms`
    );
  }
  return { code: await child.exited, named: name, stderr, stdout };
}
