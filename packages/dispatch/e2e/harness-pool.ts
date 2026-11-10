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

import {
  type LaneDatabase,
  lanePostgres,
  pgBinVariable,
  pgImageVariable,
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
  readonly database: LaneDatabase | undefined;
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

/** Stops what has started when the run is interrupted. A Ctrl-C reaches Playwright, which runs
 *  its `globalTeardown` and so `stopStacks`; this is for the minutes before that, while the
 *  stacks are still starting and there is no run to tear down yet. */
const stopping = new AbortController();
let watchingSignals = false;

function watchSignals(): void {
  if (watchingSignals) return;
  watchingSignals = true;
  for (const signal of ["SIGINT", "SIGTERM"] as const) {
    process.on(signal, () => stopping.abort());
  }
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
 *  what started, this run's directory included, and the failure is the caller's to report. */
export async function startStacks(
  count: number,
  options: StackPoolOptions = {}
): Promise<readonly Stack[]> {
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
        : lanePostgres({ dir: runDir, env, run: String(process.pid) });
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
    return stacks;
  } catch (error) {
    await stopStacks(true);
    throw error;
  }
}

/** Stops every harness and every database and clears the run's directory. `keepLogs` is the one
 *  decision this cannot make for itself: the logs are what a failed run is read from, and this
 *  module never learns whether the run passed. */
export async function stopStacks(keepLogs: boolean): Promise<void> {
  stopped = true;
  await Promise.allSettled(stacks.map((stack) => stack.harness.stop()));
  await Promise.allSettled(stacks.map((stack) => stack.database?.stop()));
  clearRunDir(keepLogs);
  stacks.length = 0;
  stopped = false;
}
