/** The one bounded subprocess every row and hook of this directory starts: the argv, the working
 *  directory, the environment the caller chose, how the pipes are read, the bound that stops a
 *  child that is stuck, and the line each child prints when it ends. It also holds the limit a
 *  row states for itself, which is read from the bounds of the children that row waits for.
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
 * limit that can fire there is the seven-short-children row of `harness-signin.test.ts` at
 * 450 000 ms, which fails about 9.5 minutes in. The two-stack row's 1 830 000 ms is longer than
 * the job and never fires there: that row skips unless `DISPATCH_E2E_PG_BIN` or
 * `DISPATCH_E2E_PG_IMAGE` names a Postgres, and no CI job names one. A reader on a laptop waits
 * longer for a failure than before: a stuck child now takes 5 to 7.5 minutes to fail, where it
 * took 60 to 90 seconds.
 */

import { relative } from "node:path";
import { fileURLToPath } from "node:url";

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

/** The one variable this module adds to a child's environment, and the one it reads in the
 *  process it runs in. */
const boundedParent = "DISPATCH_E2E_BOUNDED_PARENT";

/** True in the test process, false inside a bounded child. A child started here is `detached`,
 *  which makes it the leader of a process group of its own, so its bound can take everything it
 *  started with it. A child started inside another bounded child is not, so it stays in the group
 *  its nearest bounded ancestor leads and that ancestor's bound sweeps it too. */
const leadsOwnGroup = process.env[boundedParent] !== "1";

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

/** What the cost line and the failure call a child: the file that started it, the command with
 *  its arguments, and the environment the caller gave it.
 *
 *  The environment here is the entries of `env` whose value differs from this process's own value
 *  for that name, sorted by name. The line exists to tell one run of a command from another
 *  inside one run of the suite, and a variable copied from this process is no choice of the
 *  caller's: the listing's `HOME` and `PATH` floor names nothing, and a caller's
 *  `DISPATCH_E2E_PROCESSES=1` names itself. Four callers start the same default listing with the
 *  same empty environment, so the file is what tells those four apart. */
export function named({ argv, env, from }: Child): string {
  const chosen =
    env === undefined
      ? "this process's environment"
      : Object.entries(env)
          .filter(([name, value]) => value !== process.env[name])
          .sort(([one], [other]) => one.localeCompare(other))
          // A name the caller gives no value to is a name it takes away, which `NAME=` says.
          .map(([name, value]) => `${name}=${value ?? ""}`)
          .join(" ") || "no variable of its own";
  return `${relative(dispatchPackage, fileURLToPath(from))} | ${argv.join(" ")} | ${chosen}`;
}

/** What one bounded child did. `code` is its exit status, `ms` the time it took, and `named` what
 *  the cost line called it, which a caller that refuses a non-zero exit builds its message
 *  from. */
interface BoundedRun {
  readonly code: number;
  readonly ms: number;
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

/** Starts one child, waits for it, and answers what it did.
 *
 *  Both pipes are read whole, as the nine hand-rolled copies in this directory read them before.
 *  The child prints one cost line on this process's stderr when it ends, and never into the text
 *  this answers with: rows of this directory read a child's stderr as exactly empty.
 *
 *  This never throws for an exit status. Four files of this directory run children that are meant
 *  to exit non-zero and read the code, so a helper that always threw would fail every row of
 *  theirs that reads a refusal. A caller that does refuse a non-zero exit builds its message from
 *  `named`.
 *
 *  It throws when the bound passes. The child's group is killed, the pipes are read to their end,
 *  and the message names the file that started the child, the command with its arguments, the
 *  environment the caller gave it, the bound and the time waited. */
export async function runBounded(options: BoundedOptions): Promise<BoundedRun> {
  const { argv, bound, cwd, env, from, relaysLines = false } = options;
  const started = performance.now();
  const child = Bun.spawn([...argv], {
    cwd,
    detached: leadsOwnGroup,
    env: { ...(env ?? process.env), [boundedParent]: "1" },
    stderr: "pipe",
    stdout: "pipe",
  });
  const killGroup = () => {
    try {
      // The group where this child leads one, and the child alone where it does not.
      process.kill(leadsOwnGroup ? -child.pid : child.pid, "SIGKILL");
    } catch {
      // ESRCH: everything that was there has exited already.
    }
  };
  let stuck = false;
  const bounded = setTimeout(() => {
    stuck = true;
    killGroup();
  }, bounds[bound]);
  // The pipes are read with the global `Response`, which is Bun's own here: this half of the
  // package's `bun test` loads no preload and registers no DOM, so nothing puts happy-dom's
  // `Response` on globalThis.
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  // A timer that fired after this point would kill a process the run has finished with.
  clearTimeout(bounded);
  const ms = Math.round(performance.now() - started);
  // Whichever way the child ended, its group goes with it, so nothing it started outlives it.
  killGroup();
  const name = named({ argv, env, from });
  if (relaysLines) {
    for (const line of stderr.split("\n")) {
      if (line.startsWith(marker)) process.stderr.write(`${line}\n`);
    }
  }
  const ended =
    child.signalCode !== null
      ? ` | killed by ${child.signalCode}`
      : code === 0
        ? ""
        : ` | exit ${code}`;
  process.stderr.write(`${marker} ${name} | ${ms} ms${ended}\n`);
  if (stuck) {
    // A child of the test process leads a group; one started inside another bounded child does
    // not, and the group its ancestor leads is what sweeps whatever it left.
    const killed = leadsOwnGroup ? "its process group was killed" : "it was killed";
    throw new Error(
      `the bounded child ${name} did not end inside the ${bound} bound of ${bounds[bound]} ms: ` +
        `${killed} after ${ms} ms`
    );
  }
  return { code, ms, named: name, stderr, stdout };
}
