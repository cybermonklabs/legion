/** The stacks a Playwright run serves its workers from, started before the first row and stopped
 *  after the last (`e2e/global-setup.ts`, `e2e/global-teardown.ts`).
 *
 *  A stack is one whole harness as `e2e/harness-start.ts` starts it - a Dispatch server, the fake
 *  Envoy, the fake GitHub, the fake broker, the plain-HTTP proxy and whatever else the run needs -
 *  with a Postgres, a file-store bucket, a log directory and ports of its own. A run has one stack
 *  per worker, because `e2e/seed.ts` truncates the database before every row and clears that
 *  stack's fakes with it: two workers on one stack would empty each other's state mid-row. A
 *  worker picks its own out of the manifest `globalSetup` writes, through `TEST_PARALLEL_INDEX`
 *  (`e2e/harness-ports.ts`).
 *
 *  Each stack's Postgres comes from `DISPATCH_E2E_PG_IMAGE` or `DISPATCH_E2E_PG_BIN`
 *  (`e2e/harness-postgres.ts`) and keeps its data under this run's own temporary directory, which
 *  `stopStacks` removes. A run of one stack with `DATABASE_URL` set uses that database and starts
 *  no Postgres, as this harness always has.
 *
 *  WHY NO CHECK COMPARES THE STACKS' PORTS WITH EACH OTHER. The wrapper this replaced held every
 *  lane itself and started the Playwright command afterwards, so it could compare every lane's
 *  ports once they were all up and refuse a port two lanes had been handed. The check that
 *  matters to a worker is `harnessPorts()`'s own: it refuses two variables of one stack that name
 *  one port, in the process that is about to address them, and that refusal still stands. Two
 *  stacks on one port is a port the OS handed out twice while nothing held it, which rerunning
 *  answers; and the stack lines below name every port of every stack, so the log of such a run
 *  still shows the collision.
 */
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

import {
  pgBinVariable,
  pgImageVariable,
  type StackDatabase,
  stackPostgres,
} from "./harness-postgres";
import { type Harness, startHarness } from "./harness-start";
import { Interrupted } from "./local-instance-support";

/** What this module's own lines are prefixed with, as every starter in `e2e/` names itself. */
const named = "e2e/harness-pool.ts";

/** One started stack as its callers read it: the index a worker selects it by, and every variable
 *  it carries - its ports, its `DATABASE_URL`, its file-store bucket. */
export interface Stack {
  readonly index: number;
  readonly env: Readonly<Record<string, string>>;
}

export interface StackPoolOptions {
  /** Where this run's own inputs are read: `DATABASE_URL`, the file store and the Postgres
   *  provider (`e2e/harness-postgres.ts`). This process's environment when absent, which is what
   *  both callers pass; a test names one of its own to reach a refusal whatever the machine it
   *  runs on has. The harness itself always starts from this process's environment with the
   *  stack's own variables on top (`e2e/harness-start.ts`). */
  readonly env?: NodeJS.ProcessEnv;
}

/** One stack and what it takes to stop it. */
interface Running extends Stack {
  readonly database: StackDatabase | undefined;
  readonly harness: Harness;
  /** Where its processes write their output, unset for a run of one stack, which writes to this
   *  process's stderr as it always has. */
  readonly logDir: string | undefined;
}

const stacks: Running[] = [];
/** The run's own temporary directory, once it has one. */
let runDir = "";
/** Set by `stopStacks`, so a harness stopping on purpose is not reported as one that died. */
let stopped = false;

/** Cancels a start that is still in flight. Each starter takes this signal, so a stack half-way
 *  through an initdb or a server build stops rather than finishing into a run nobody is waiting
 *  for. */
const stopping = new AbortController();

/** The start that is still in flight, or undefined when none is. SIGTERM waits for this before
 *  it stops anything, because `stacks` takes a stack only once its whole harness is up: a
 *  Postgres that is serving, an initdb halfway through, a server the harness has started are
 *  all held by this promise alone, and it is the start's own `catch` - and the stop paths of
 *  `e2e/harness-start.ts` and `e2e/harness-postgres.ts` under it - that puts them down. It
 *  never rejects; the copy kept here swallows what `startStacks` hands its caller. */
let starting: Promise<unknown> | undefined;

/** Set by the SIGTERM listener, so a second signal ends this process rather than starting a
 *  second stop over the top of the first. */
let terminating = false;

/** How long SIGTERM waits for a start in flight to put down what it is holding, after which it
 *  stops what is up and ends this process anyway.
 *
 *  A start that has read the abort unwinds in about a second, since every wait in it takes that
 *  signal; what it cannot cut short is an `execFile` already running - a `pg_ctl -w start` or
 *  the `pg_ctl -w stop` that follows it, each bounded by PGCTLTIMEOUT - and the harness's own
 *  `Supervisor.stop`, which SIGKILLs whatever is left 10 s after its SIGTERM. The bound is not
 *  generous for its own sake, because whoever sent the signal is counting too: a pod's
 *  `terminationGracePeriodSeconds` is 30 s by default and a `timeout -k` is of that order, and
 *  a handler still waiting when the SIGKILL arrives stops nothing at all. 15 s leaves the stop
 *  that follows it the 10 s `Supervisor.stop` can take, inside a 30 s grace. */
const terminateGraceMs = 15_000;

/** The pool's own signal listeners, held so they can be taken off again: `stopStacks` leaves
 *  this process's signal handling as the pool found it, so a second `startStacks` in one
 *  process - Playwright's UI mode, and this package's own unit tests - adds no second copy.
 *
 *  THE TWO SIGNALS GET DIFFERENT ANSWERS, which is the whole of why there are two of them here.
 *
 *  SIGINT is Playwright's. Its runner catches the Ctrl-C and runs `globalTeardown`, which is
 *  `stopStacks`, so an interrupted run stops every server and every database and keeps its logs
 *  by the ordinary route. The pool's own listener is therefore needed only for the minutes
 *  BEFORE there is a run to tear down, while the stacks are still starting; `startStacks` takes
 *  it off again once they are up, leaving the runner the one handler of that signal.
 *
 *  SIGTERM is nobody's. Playwright's runner does not handle it, so `kill <pid>`, a `timeout` and
 *  a pod's grace period reach this process alone - and a listener that only set a flag would be
 *  worse than none, since registering one removes Node's own default exit and the run would
 *  carry on to its end with every server up. So this listener does the work: it cancels the
 *  start, WAITS for it (`starting`, bounded by `terminateGraceMs`), stops the stacks, and then
 *  ends the process with the signal's OWN disposition rather than an exit status this file
 *  invented, which it can do because `stopStacks` has by then removed the listener and the
 *  second SIGTERM meets Node's default. */
let onInterrupt: (() => void) | undefined;
let onTerminate: (() => void) | undefined;

/** What SIGTERM does, in order: cancel the start, wait for it to unwind, stop what is up, and
 *  end this process with the signal's own disposition. */
async function terminate(): Promise<void> {
  stopping.abort();
  // The start in flight is awaited BEFORE anything is stopped, because dying here is what left
  // a Postgres serving a loopback port and a data directory behind it: the stack is not in
  // `stacks` yet, so `stopStacks` would stop nothing and remove the run directory under a
  // server that is still up. The wait is bounded, so a start that never settles still ends.
  if (starting !== undefined) await Promise.race([starting, sleep(terminateGraceMs)]);
  await stopStacks(true);
  process.kill(process.pid, "SIGTERM");
}

function watchSignals(): void {
  if (onInterrupt === undefined) {
    onInterrupt = (): void => stopping.abort();
    process.on("SIGINT", onInterrupt);
  }
  if (onTerminate === undefined) {
    onTerminate = (): void => {
      // The logs are kept: a run this signal ended reported no verdict on anything, and what
      // its servers were doing when it stopped is the only account there is
      // (`e2e/run-outcome.ts` keeps the same rule).
      //
      // A second SIGTERM, while the first is still waiting on the start, is whoever sent it
      // saying they will not wait: the listener comes off and the signal is sent on to Node's
      // default, which ends the process now.
      if (terminating) {
        unwatchSignals();
        process.kill(process.pid, "SIGTERM");
        return;
      }
      terminating = true;
      void terminate();
    };
    process.on("SIGTERM", onTerminate);
  }
}

/** Hands SIGINT back to whoever else handles it, which from here on is Playwright's runner. */
function unwatchInterrupt(): void {
  if (onInterrupt === undefined) return;
  process.off("SIGINT", onInterrupt);
  onInterrupt = undefined;
}

function unwatchSignals(): void {
  unwatchInterrupt();
  if (onTerminate === undefined) return;
  process.off("SIGTERM", onTerminate);
  onTerminate = undefined;
}

/** The stack's bucket in the file store the caller started, made here with one request. A bucket
 *  is mutable state every stack would otherwise share; a run of one stack keeps the caller's own,
 *  the bucket it made itself (`.github/workflows/dispatch-browser.yaml`'s shard job). */
async function stackBucket(
  index: number,
  count: number,
  env: NodeJS.ProcessEnv
): Promise<string | undefined> {
  const endpoint = (env.DISPATCH_E2E_FILE_STORE_ENDPOINT ?? "").trim().replace(/\/$/, "");
  const bucket = (env.DISPATCH_E2E_FILE_STORE_BUCKET ?? "").trim();
  if (count === 1 || endpoint === "" || bucket === "") return undefined;
  const own = `${bucket}-worker-${index}`;
  const made = await fetch(`${endpoint}/${own}`, { method: "PUT" });
  if (!made.ok) {
    throw new Error(
      `the file store at ${endpoint} refused stack ${index}'s bucket ${own}: ` +
        `${made.status} ${await made.text()}`
    );
  }
  return own;
}

/** Removes the run's own directory, which holds every stack's data directory. `keepLogs` keeps
 *  the stacks' logs, the only record of what their servers said, and loses the data directories,
 *  which are the bulk of it; a directory a stack's Postgres made as another user (the image
 *  provider's container) can outlive the run rather than fail its exit status. */
function clearRunDir(keepLogs: boolean): void {
  if (runDir === "") return;
  const root = runDir;
  runDir = "";
  const keep = keepLogs && stacks.some((stack) => stack.logDir !== undefined);
  // Every data directory under the run's own, named from that directory rather than from the
  // stacks that started: a stack that failed to start left a copy of the template there and no
  // entry among them, and those copies are the bulk this branch is here to lose.
  const removed = keep ? readdirSync(root).map((entry) => join(root, entry, "pgdata")) : [root];
  for (const path of removed) {
    try {
      rmSync(path, { force: true, recursive: true });
    } catch (error) {
      console.error(`${named}: ${path} is left behind: ${(error as Error).message}`);
    }
  }
  if (keep) console.error(`${named}: the stacks' logs are in ${root}`);
}

/** Starts `count` stacks at once - each is an initdb or a `docker run` and a server build away,
 *  and they share nothing - and returns them in index order. A stack that fails to start stops
 *  what started, this run's directory included, and the failure is the caller's to report.
 *
 *  The start is held as one promise while it runs, so a SIGTERM that lands in the middle of it
 *  waits for it rather than ending this process on top of what it has started (`starting`). */
export async function startStacks(
  count: number,
  options: StackPoolOptions = {}
): Promise<readonly Stack[]> {
  // `startPool` runs as far as its own `watchSignals()` before it hands this promise back, and
  // a signal is delivered as a task of its own, so no SIGTERM lands between these two lines.
  const start = startPool(count, options);
  // The listener's own copy, which swallows: the rejection a cancelled start hands its caller
  // is handled by the `await` below, and a second reference to it must not become an unhandled
  // rejection of the handler's.
  starting = start.catch(() => undefined);
  try {
    return await start;
  } finally {
    starting = undefined;
  }
}

/** The whole of a start, as the one promise `startStacks` holds it by. */
async function startPool(count: number, options: StackPoolOptions): Promise<readonly Stack[]> {
  const env = options.env ?? process.env;
  watchSignals();
  try {
    const given = (env.DATABASE_URL ?? "").trim();
    runDir = mkdtempSync(join(tmpdir(), "dispatch-e2e-run-"));
    // A run of one stack against the database the caller named starts no Postgres, which is what
    // every other user of this harness does (packages/dispatch/README.md).
    const postgres =
      count === 1 && given !== ""
        ? undefined
        : stackPostgres({ dir: runDir, env, run: String(process.pid) });
    if (count > 1 && postgres === undefined) {
      throw new Error(
        `${count} Playwright workers run ${count} stacks, each with a Postgres of its own, so ` +
          `one of ${pgImageVariable} (a \`docker run\` of that image per stack) and ` +
          `${pgBinVariable} (a directory holding initdb and pg_ctl) must name one.`
      );
    }
    const startStack = async (index: number): Promise<void> => {
      const logDir = count === 1 ? undefined : join(runDir, `worker-${index}`, "log");
      const database =
        postgres === undefined ? undefined : await postgres.start(index, stopping.signal);
      // Everything from here on runs with a server of this stack's own up, so each of them — the
      // bucket the file store makes as much as the harness — stops it rather than leave it behind.
      try {
        const own: Record<string, string> = {};
        // How many stacks share this machine, which each stack's own server reads: it divides the
        // machine's CPUs by this for its GOMAXPROCS, so four servers on a four-vCPU runner take
        // one core each rather than four (e2e/run-server.sh). One stack passes it too, and the
        // script leaves Go's own sizing alone at one.
        own.DISPATCH_E2E_STACK_COUNT = String(count);
        const url = database?.url ?? given;
        if (url !== "") own.DATABASE_URL = url;
        const bucket = await stackBucket(index, count, env);
        if (bucket !== undefined) own.DISPATCH_E2E_FILE_STORE_BUCKET = bucket;
        const harness = await startHarness({ env: own, logDir, signal: stopping.signal });
        stacks.push({ database, env: { ...own, ...harness.env }, harness, index, logDir });
      } catch (error) {
        await database?.stop();
        throw error;
      }
    };
    const started = await Promise.allSettled(
      Array.from({ length: count }, (_, index) => startStack(index))
    );
    stacks.sort((first, second) => first.index - second.index);
    const failed = started.find((stack) => stack.status === "rejected");
    if (failed !== undefined) throw (failed as PromiseRejectedResult).reason;
    if (stopping.signal.aborted) throw new Interrupted();

    if (postgres !== undefined)
      console.error(`${named}: each stack's Postgres is ${postgres.name}`);
    for (const stack of stacks) {
      const ports = Object.entries(stack.harness.env)
        .map(([variable, port]) => `${variable}=${port}`)
        .join(" ");
      const postgresPort =
        stack.database === undefined ? "" : `POSTGRES_PORT=${stack.database.port} `;
      const output = stack.logDir ?? "its output is on this process's stderr";
      console.error(`${named}: stack ${stack.index}: ${postgresPort}${ports} (${output})`);
    }
    // The two numbers a reader of a CI log reads this line for. One stack per worker is the
    // invariant: a start that could not give a worker its own stack throws above rather than
    // returning fewer than it was asked for, so the first number is counted from the stacks that
    // are up and the second is the workers they were started for.
    console.error(`${named}: ${stacks.length} stacks for ${count} workers`);

    // A harness process that stops by itself is named, so rows failing against a server that died
    // say why, and every stack is watched, since a row runs in whichever stack its worker took.
    for (const stack of stacks) {
      stack.harness.firstExit().catch((error: Error) => {
        if (!stopped) console.error(`${named}: stack ${stack.index}: ${error.message}`);
      });
    }
    // The stacks are up, so there is a run for Playwright's runner to tear down, and its own
    // Ctrl-C handling is the whole answer from here (the listeners above say why). SIGTERM
    // stays on, because nothing else handles it.
    unwatchInterrupt();
    return stacks;
  } catch (error) {
    await stopStacks(true);
    throw error;
  }
}

/** Stops every harness and every database, clears the run's directory and takes the pool's
 *  signal listeners off again. `keepLogs` is the one decision this cannot make for itself: the
 *  logs are what a failed run is read from, and this module never learns whether the run passed
 *  (`e2e/run-outcome.ts`). */
export async function stopStacks(keepLogs: boolean): Promise<void> {
  stopped = true;
  await Promise.allSettled(stacks.map((stack) => stack.harness.stop()));
  await Promise.allSettled(stacks.map((stack) => stack.database?.stop()));
  clearRunDir(keepLogs);
  stacks.length = 0;
  stopped = false;
  unwatchSignals();
}
