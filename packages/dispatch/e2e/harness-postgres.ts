/** Each stack's Postgres server, from one of the two providers a caller names.
 *
 *  A stack is a whole harness of its own (`e2e/harness-pool.ts`), and `e2e/seed.ts` truncates its
 *  database before every row, so two stacks cannot share one: the pool starts a server per stack
 *  and hands it to that stack's Dispatch servers and psql as `DATABASE_URL`.
 *
 *  - `DISPATCH_E2E_PG_IMAGE` is a `docker run` of the image digest the caller names: a
 *    developer's path on a machine with Docker and no Postgres binaries, and the path of every
 *    CI job that runs the suite on the runner rather than in the browser image - the `visual`,
 *    `multiprocess` and `embedder` jobs of `.github/workflows/dispatch-browser.yaml` and the
 *    browser half of `.github/workflows/ci-job-dispatch-two-builds.yaml` - since the binaries of
 *    an image a job does not run in are no route for it.
 *  - `DISPATCH_E2E_PG_BIN` is a directory holding `initdb` and `pg_ctl`, which is the `shard`
 *    and `alone` jobs' path and an issue pod's: those two jobs of that workflow run in a
 *    container that carries PostgreSQL 18 with `vector`, where there are Postgres binaries and
 *    no Docker socket, and they name that installation's directory.
 *
 *  Both keep the stack's data under the run's own temporary directory, which the pool makes and
 *  removes (`e2e/harness-pool.ts`), and both answer on 127.0.0.1: the server's dev sign-in
 *  refuses a `DATABASE_URL` whose host is neither loopback nor a unix socket
 *  (`e2e/run-server.sh`).
 *
 *  The binaries provider runs `initdb` once per run and copies the data directory it made per
 *  stack, because `initdb` is the cost that grows with the stacks: measured in an issue pod, from
 *  nothing to every stack answering `select 1`, `initdb` per stack took 3.7-4.1 s at 2 stacks and
 *  7.1-8.3 s at 4, against 4.5-6.0 s and 5.9-6.2 s for one `initdb` and a copy per stack, which
 *  is flat in the stacks because only the copies grow. The template carries the stack's database
 *  and its pgvector extension, which migration 0072 needs, and not the migrations: the server
 *  applies those at boot, as it does today against a fresh database, and a template the same pod
 *  measured with them applied cost 5.7-6.2 s at 2 stacks and 7.7-8.7 s at 4 — more than it saves,
 *  since the 1.0 s of migrations it takes out of a stack's boot runs in every stack at once.
 *
 *  With `DISPATCH_E2E_PG_STALL_DIR` set, each provider builds e2e/fsync-stall.c into that
 *  directory and starts every server with it preloaded, at the same path, so a row can make the
 *  disk stop answering (e2e/disk-stall.ts). */
import { execFile } from "node:child_process";
import { existsSync, mkdirSync, readdirSync, readFileSync } from "node:fs";
import { createServer } from "node:net";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { buildStallLibrary, diskStallDir, stallEnvironment } from "./disk-stall";
import { announceWait, Interrupted, interruptedBefore } from "./local-instance-support";
import { sqlAt } from "./psql";

const execFileAsync = promisify(execFile);

/** A `docker run` of this image serves one stack. */
export const pgImageVariable = "DISPATCH_E2E_PG_IMAGE";
/** This directory's `initdb` and `pg_ctl` serve every stack. */
export const pgBinVariable = "DISPATCH_E2E_PG_BIN";
/** The connections a stack's server must allow beyond its superuser reserve, as
 *  `connectionsArgs` prints them, given by a caller that cannot run `go`. */
export const pgConnectionsVariable = "DISPATCH_E2E_PG_CONNECTIONS";

/** The database every stack's server holds, named the same in each: the stacks are separate
 *  clusters, and nothing outside a stack reads it. */
const databaseName = "dispatch_e2e";
/** packages/envoy, the Go module `connectionsArgs` runs in. */
const envoyDirectory = fileURLToPath(new URL("../../envoy", import.meta.url));
/** `go` with these prints `testlimits.ServerConnections`
 *  (packages/envoy/internal/dispatch/testlimits): the connections a Dispatch test server must
 *  allow beyond its superuser reserve. */
const connectionsArgs = ["run", "./internal/dispatch/store/cmd/connections"];
/** Postgres's default `superuser_reserved_connections`. The harness does not set it, so each
 *  stack's server keeps this many slots for superusers, and the suite's connections get none. */
const superuserReserved = 3;
/** A stack's connection limit: `testlimits.ServerConnections` plus `superuserReserved`. It comes
 *  from the Go code, so the harness holds no count of a Dispatch process's pool. A server that
 *  allows fewer does not fail where the size is wrong. It fails later, as a 53300 refusal inside
 *  a room, a lease or a socket, and nothing in that failure names the cause. The first stack
 *  that starts reads it, once per run.
 *
 *  `DISPATCH_E2E_PG_CONNECTIONS`, when set, is that number. Every CI job that starts stacks here
 *  sets it from a step that runs `connectionsArgs` before its browser step: the `shard` and
 *  `alone` jobs of `.github/workflows/dispatch-browser.yaml` run in a container with no Go, so
 *  their `binaries` job runs it, and the `visual`, `multiprocess` and `embedder` jobs of that
 *  file and the `dispatch-two-builds` job of `.github/workflows/ci-job-dispatch-two-builds.yaml`
 *  each run it in a step of their own, outside their browser steps' budgets. Unset, which is a
 *  developer's or an issue pod's run, where Go is installed, the harness runs `go` with
 *  `connectionsArgs` itself. */
let maxConnections: Promise<number> | undefined;
async function readMaxConnections(env: NodeJS.ProcessEnv): Promise<number> {
  const given = env[pgConnectionsVariable];
  if (given !== undefined) {
    const server = positiveWholeNumber(given);
    if (server === undefined) {
      throw new Error(
        `The Dispatch e2e harness cannot start: ${pgConnectionsVariable}=${JSON.stringify(given)} ` +
          "is not a positive whole number of connections."
      );
    }
    return server + superuserReserved;
  }
  const named = `\`go ${connectionsArgs.join(" ")}\` in ${envoyDirectory}`;
  let stdout: string;
  try {
    ({ stdout } = await execFileAsync("go", connectionsArgs, { cwd: envoyDirectory }));
  } catch (error) {
    throw new Error(
      `The Dispatch e2e harness cannot start: ${named} failed: ` +
        `${error instanceof Error ? error.message : String(error)}`
    );
  }
  const server = positiveWholeNumber(stdout);
  if (server === undefined) {
    throw new Error(
      `The Dispatch e2e harness cannot start: ${named} printed ${JSON.stringify(stdout)}, ` +
        "not a positive whole number of connections."
    );
  }
  return server + superuserReserved;
}
/** `text` as a positive whole number, one optional trailing newline allowed; else undefined. */
function positiveWholeNumber(text: string): number | undefined {
  const printed = /^([1-9][0-9]*)\n?$/.exec(text)?.[1];
  const value = printed === undefined ? Number.NaN : Number(printed);
  return Number.isSafeInteger(value) ? value : undefined;
}
/** A stack's server. Its data dies with the run and its database is 46 MB with a working set of
 *  one scenario's rows, so each setting here gives up something only a database that outlives
 *  its run can use. What each costs if the database were real is on its own line. Measured in an
 *  issue pod over two stacks and 58 rows of `inbox` and `search`, none of them moves the reset's
 *  own median (51.5 ms before, 52.0 ms after): they take work off a stack's server that this
 *  pod's two cores were not short of, and the runner's four are. */
const settings = [
  // Crash safety. Nothing here survives the run, so a cluster that dies loses nothing anyone
  // reads: a real database would lose committed transactions (`synchronous_commit`) or the
  // whole cluster (`fsync`, `full_page_writes`) to a power cut. With these off no commit waits
  // for the disk, and that is what keeps a runner's disk that stalls for a few seconds out of
  // the reset: a backend flushing its commit record holds its interrupts and its locks, so a
  // read mark a closed page left in flight held `user_document_reads` past the reset's 5 s in
  // merge-group run 44456291758, whose database kept these on
  // (e2e/read-marker-disk-stall.multiprocess.e2e.ts reproduces it).
  "fsync=off",
  "full_page_writes=off",
  "synchronous_commit=off",
  // The buffer pool, against the image's 128MB. 64MB holds the whole 46 MB stack database, so
  // nothing this cluster reads can be evicted, and four stacks' pools are 256MB smaller. A real
  // database would read its working set back from the operating system on every eviction.
  "shared_buffers=64MB",
  // Expression JIT. Compiling an expression costs a tenth of a second of pure CPU and earns it
  // back only over a long scan; no table here is long enough, so a plan whose estimate passes
  // `jit_above_cost` is an estimate that is wrong. A real database loses that on its analytics.
  "jit=off",
  // Parallel query. At this size no plan is expected to take a worker, and this makes that
  // certain: one gather on one stack is four more processes on a runner whose cores are the
  // bound. A real database loses parallel scans, joins and aggregates.
  "max_parallel_workers_per_gather=0",
  // WAL. `minimal` is the least this cluster can write and still recover its own crash, and it
  // writes none at all for a bulk load into a relation the same transaction made or truncated.
  // It rules out replication and point-in-time recovery, which every real deployment is built
  // on, and so needs `max_wal_senders=0`.
  "wal_level=minimal",
  "max_wal_senders=0",
  // Checkpoints. A stack writes far too little WAL to reach `max_wal_size`, so its only
  // checkpoints are the timed ones - one every five minutes per stack, each writing every dirty
  // buffer - and a timer longer than any run leaves none. A real database would face a recovery
  // that replays an hour of WAL, after a checkpoint that wrote all of it at once.
  "checkpoint_timeout=60min",
  "max_wal_size=4GB",
];
/** Every setting a stack's server starts with: its connection limit, then `settings`. */
async function serverSettings(env: NodeJS.ProcessEnv): Promise<string[]> {
  maxConnections ??= readMaxConnections(env);
  return [`max_connections=${await maxConnections}`, ...settings];
}
/** How many ports a cluster tries before it gives up (see `freePort`). */
const bindAttempts = 3;

/** One stack's Postgres. */
export interface StackDatabase {
  /** The `DATABASE_URL` this stack's servers and psql use. */
  readonly url: string;
  /** The port it serves on, which the harness logs and no other stack shares. */
  readonly port: number;
  /** Stops the server. `keepLog` asks the image provider to print the container's whole log on
   *  stderr before the container goes, since it is the one record of what that server said: the
   *  pool asks for it for a run whose logs it keeps, a failed one, and for a start that failed or
   *  was interrupted, a SIGTERM during the start included; not when a SIGTERM stops stacks that
   *  are already up, whose run's outcome it does not know (`e2e/harness-pool.ts`).
   *  The binaries provider's log is a file under the run's directory (`postgres.log`), which the
   *  pool keeps or loses with the rest. */
  stop(keepLog: boolean): Promise<void>;
}

/** The provider a run starts every stack's Postgres from. */
export interface StackPostgres {
  /** What a log line and a refusal call it. */
  readonly name: string;
  /** One stack's server. Everything it leaves behind is under the run's own directory, which the
   *  pool removes (`e2e/harness-pool.ts`), or a container the returned `stop` removes. `signal`
   *  is the pool's own stop: a Ctrl-C while this waits for the server to answer ends the wait as
   *  `Interrupted` rather than as a start that failed. */
  start(index: number, signal?: AbortSignal): Promise<StackDatabase>;
}

/** A port the OS chose and nothing holds. Another listener can take it between here and the
 *  moment the server binds it, which is why `startCluster` and the image provider try another
 *  port rather than failing: a Postgres server cannot inherit the socket the way the harness's
 *  own listeners do (`scripts/e2e/lib/held-port.py`). */
async function freePort(): Promise<number> {
  const socket = createServer();
  const chosen = Promise.withResolvers<number>();
  socket.once("error", chosen.reject);
  socket.listen(0, "127.0.0.1", () => {
    const address = socket.address();
    if (address === null || typeof address === "string") {
      chosen.reject(new Error("the OS named no port for a listening socket"));
    } else {
      chosen.resolve(address.port);
    }
  });
  try {
    return await chosen.promise;
  } finally {
    const closed = Promise.withResolvers<void>();
    socket.close(() => closed.resolve());
    await closed.promise;
  }
}

/** One stack's server, with a stop under way reported as the interruption it is: psql rejects a
 *  statement the signal killed with an abort of its own, and a run a Ctrl-C stopped reads as
 *  interrupted rather than as a Postgres that would not start. */
async function stackOf(
  signal: AbortSignal | undefined,
  started: () => Promise<StackDatabase>
): Promise<StackDatabase> {
  try {
    return await started();
  } catch (error) {
    if (signal?.aborted === true) throw error instanceof Interrupted ? error : new Interrupted();
    throw error;
  }
}

/** Waits until `url` answers `select 1`: a server that is up has read its data directory, run
 *  its own recovery and opened its port.
 *
 *  The wait has no bound of its own, as the harness's wait for its listeners has none (`serving`
 *  in e2e/harness-start.ts says why): how long a server takes to come up is the machine's, and a
 *  bound of 120 s failed a start that was only slow, under a caller whose own deadline was far
 *  away. It ends on the answer; on the server's end, which `gone` reads after each try that
 *  failed, with the last try's error; on a psql that cannot be started at all, such as one that
 *  is not on PATH, which no later try can change; or on the caller's abort (`signal`), whose
 *  Interrupted names the server it waited for (`interruptedBefore`). A try that never returns, a
 *  server that takes the connection and never answers, is ended by that abort too, since the psql
 *  it runs takes the signal. It writes when the wait begins and ends (`announceWait`; `what`
 *  names the stack). Exported for e2e/__tests__/harness-postgres.test.ts. */
export async function ready(
  url: string,
  what: string,
  gone: () => boolean,
  signal?: AbortSignal
): Promise<void> {
  const since = Date.now();
  const waited = announceWait("e2e/harness-postgres.ts", `${what} answers select 1`);
  try {
    for (;;) {
      try {
        await sqlAt(url, ["select 1"], signal);
        return;
      } catch (error) {
        // The psql above was killed by the signal, or the one before it was.
        if (signal?.aborted === true) throw interruptedBefore(`${what} answered select 1`, since);
        // A psql that ran and failed has a numeric exit `code`; one that could not be started has
        // the spawn's own (ENOENT, EACCES), which every later try would meet again.
        const code = (error as NodeJS.ErrnoException).code;
        if (typeof code === "string" && code !== "ABORT_ERR") {
          throw new Error(
            `The Dispatch e2e harness cannot start: psql, which asks ${what} for select 1, could ` +
              `not be started: ${error instanceof Error ? error.message : String(error)}`
          );
        }
        if (gone()) {
          throw new Error(
            `The Dispatch e2e harness cannot start: ${what} stopped before it answered select 1: ` +
              `${error instanceof Error ? error.message : String(error)}`
          );
        }
      }
      // An abort ends the pause at once, and the next try's psql, given an aborted signal, fails
      // before it starts, which the branch above names.
      await sleep(200, undefined, { signal }).catch(() => {});
    }
  } finally {
    waited();
  }
}

/** Whether the postmaster of the cluster in `data` has ended: the pid its `postmaster.pid` names,
 *  read once here, after `pg_ctl -w start` returned, is no running process, or there was no
 *  file to read. The postmaster is no child of this process (`pg_ctl` starts it as a daemon), so
 *  its pid is the one handle there is. */
function postmasterGone(data: string): () => boolean {
  let pid = Number.NaN;
  try {
    pid = Number(readFileSync(join(data, "postmaster.pid"), "utf8").split("\n")[0]);
  } catch {
    // No file: the server that `pg_ctl` started has already gone.
  }
  return () => {
    if (!Number.isInteger(pid) || pid <= 0) return true;
    try {
      process.kill(pid, 0);
      return false;
    } catch (error) {
      return (error as NodeJS.ErrnoException).code === "ESRCH";
    }
  };
}

/** The `-t` every `pg_ctl -w start` here is given, in seconds: a year. pg_ctl has no wait without
 *  a limit, and its default limit, `PGCTLTIMEOUT` or 60 s, is a wall-clock bound on how long a
 *  server takes to come up, which is the machine's (`ready` says why the harness keeps none). With
 *  this limit its clock decides nothing: the wait ends when the server takes connections, or when
 *  its postmaster has died, both of which pg_ctl reads itself, or on the caller's abort, which
 *  ends the pg_ctl and the postmaster it started (`startCluster`). */
const pgCtlStartWait = String(365 * 24 * 60 * 60);

/** Kills, with SIGKILL, every process that serves the cluster in `data`: a `postgres -D <data>`,
 *  read from `/proc/<pid>/cmdline`. It is the Linux backstop for the one case `startCluster`'s
 *  SIGINT does not reach: a pg_ctl that the abort ended after it forked the postmaster and before
 *  it installed the handler that passes SIGINT on (pg_ctl.c, `do_start`). That postmaster runs in a
 *  session of its own, so it outlives its pg_ctl, and it may not have written `postmaster.pid`
 *  yet, which leaves `pg_ctl stop` nothing to stop. A postmaster's children rewrite their command
 *  lines, so this finds the postmaster alone, and its death ends them. A machine with no `/proc`
 *  has no such backstop, and a postmaster that is still between its fork and its exec when the
 *  table is read is not found either; both cases need the abort to land in pg_ctl's first
 *  instants. */
function killServersOf(data: string): void {
  let pids: string[];
  try {
    pids = readdirSync("/proc").filter((entry) => /^\d+$/.test(entry));
  } catch {
    return;
  }
  for (const pid of pids) {
    let argv: string[];
    try {
      argv = readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0");
    } catch {
      continue;
    }
    const at = argv.indexOf("-D");
    if (at < 0 || argv[at + 1] !== data || !(argv[0] ?? "").endsWith("postgres")) continue;
    try {
      process.kill(Number(pid), "SIGKILL");
    } catch {
      // gone already
    }
  }
}

/** Starts the cluster in `data`, with its log and its unix socket in `dir`, and returns the port
 *  it serves on. Returns or throws with no postmaster of `data` left running. `server` is the
 *  environment pg_ctl, and so the postmaster, runs with. `signal`, the caller's abort, ends the
 *  start's wait with an Interrupted that names the server it waited for: it sends the pg_ctl
 *  SIGINT, which a pg_ctl waiting on `-w start` passes on to the postmaster it started before it
 *  ends (pg_ctl.c, `trap_sigint_during_startup`), so the postmaster shuts down on every platform;
 *  `killServersOf` covers the instant before pg_ctl installs that handler, on Linux. Each attempt
 *  writes when its wait begins and ends (`announceWait`). */
async function startCluster(
  bin: string,
  data: string,
  dir: string,
  what: string,
  env: NodeJS.ProcessEnv,
  server: NodeJS.ProcessEnv,
  signal?: AbortSignal
): Promise<number> {
  let last = "";
  const started = await serverSettings(env);
  const since = Date.now();
  for (let attempt = 1; attempt <= bindAttempts; attempt++) {
    const port = await freePort();
    const options = [`-p ${port}`, "-h 127.0.0.1", `-k ${dir}`, ...started.map((s) => `-c ${s}`)];
    const waited = announceWait(
      "e2e/harness-postgres.ts",
      `${what} takes connections on port ${port} (pg_ctl -w start, attempt ${attempt} of ${bindAttempts})`
    );
    try {
      await execFileAsync(
        join(bin, "pg_ctl"),
        [
          "-D",
          data,
          "-l",
          join(dir, "postgres.log"),
          "-o",
          options.join(" "),
          "-t",
          pgCtlStartWait,
          "-w",
          "start",
        ],
        { env: server, killSignal: "SIGINT", signal }
      ).finally(waited);
      return port;
    } catch (error) {
      last = error instanceof Error ? error.message : String(error);
      // A pg_ctl that ended without the server taking connections, or that the caller's abort
      // killed, can leave the postmaster it started running and holding `postmaster.pid`: the next
      // attempt's start would fail on that lock rather than on its port, and a throw here would
      // leave a server serving a loopback port while the pool removes the data directory under
      // it (`e2e/harness-pool.ts`). A stop that fails is a server that never bound, which is why
      // the failure the caller sees stays the start's.
      await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "immediate", "-w", "stop"]).catch(
        () => {}
      );
      if (signal?.aborted === true) {
        killServersOf(data);
        throw interruptedBefore(`${what} took connections`, since);
      }
    }
  }
  throw new Error(
    `The Dispatch e2e harness cannot start: ${what} did not start on any of ${bindAttempts} ` +
      `ports (${join(dir, "postgres.log")}): ${last}`
  );
}

/** The disk stall a run asked for (`DISPATCH_E2E_PG_STALL_DIR`, e2e/disk-stall.ts): its
 *  directory, what each server is started with, and the library's build, made once, before the
 *  first server, whichever stack starts first. */
interface Stall {
  readonly dir: string;
  readonly environment: Readonly<Record<string, string>>;
  built(): Promise<void>;
}

function stallOf(env: NodeJS.ProcessEnv): Stall | undefined {
  const dir = diskStallDir(env);
  if (dir === undefined) return undefined;
  let build: Promise<void> | undefined;
  return {
    built: () => {
      build ??= buildStallLibrary(dir);
      return build;
    },
    dir,
    environment: stallEnvironment(dir),
  };
}

function binariesProvider(
  dir: string,
  bin: string,
  env: NodeJS.ProcessEnv,
  stall: Stall | undefined
): StackPostgres {
  for (const tool of ["initdb", "pg_ctl"]) {
    if (!existsSync(join(bin, tool))) {
      throw new Error(
        `The Dispatch e2e harness cannot start: ${pgBinVariable}=${bin} holds no ${tool}. It ` +
          "must name the directory of a Postgres installation, the one holding initdb and pg_ctl."
      );
    }
  }
  /** What pg_ctl, and so each postmaster, runs with. */
  const server: NodeJS.ProcessEnv =
    stall === undefined ? process.env : { ...process.env, ...stall.environment };
  /** The one `initdb` of the run, with the stack database and its pgvector extension, copied per
   *  stack. Started once however many stacks ask for it. */
  let template: Promise<string> | undefined;
  const makeTemplate = async (signal?: AbortSignal): Promise<string> => {
    const templateDir = join(dir, "template");
    const data = join(templateDir, "pgdata");
    mkdirSync(templateDir, { recursive: true });
    await execFileAsync(join(bin, "initdb"), [
      "-D",
      data,
      "-U",
      "postgres",
      "--auth=trust",
      "--no-sync",
      "-E",
      "UTF8",
    ]);
    await stall?.built();
    const port = await startCluster(
      bin,
      data,
      templateDir,
      "the stacks' Postgres template",
      env,
      server,
      signal
    );
    try {
      const admin = `postgres://postgres@127.0.0.1:${port}/postgres?sslmode=disable`;
      await ready(admin, "the stacks' Postgres template", postmasterGone(data), signal);
      await sqlAt(admin, [`create database ${databaseName}`], signal);
      await sqlAt(
        `postgres://postgres@127.0.0.1:${port}/${databaseName}?sslmode=disable`,
        ["create extension vector"],
        signal
      );
    } finally {
      // Whatever happened above: the postmaster is up from `startCluster` on, and a throw that
      // left it running would leave it serving a loopback port while the pool removes the data
      // directory under it.
      await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "fast", "-w", "stop"]);
    }
    return data;
  };
  return {
    name: `the Postgres binaries in ${bin}`,
    start: (index, signal) =>
      stackOf(signal, async (): Promise<StackDatabase> => {
        template ??= makeTemplate(signal);
        const stackDir = join(dir, `worker-${index}`);
        const data = join(stackDir, "pgdata");
        mkdirSync(stackDir, { recursive: true });
        // `cp -a` keeps the 0700 a data directory's server refuses to start without.
        await execFileAsync("cp", ["-a", await template, data]);
        const what = `stack ${index}'s Postgres`;
        const port = await startCluster(bin, data, stackDir, what, env, server, signal);
        const stop = async (): Promise<void> => {
          await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "immediate", "-w", "stop"]);
        };
        const url = `postgres://postgres@127.0.0.1:${port}/${databaseName}?sslmode=disable`;
        try {
          await ready(url, what, postmasterGone(data), signal);
        } catch (error) {
          // The postmaster is up from `startCluster` on, and a wait the run's signal or the
          // server's end ended returns no `StackDatabase`, so no caller holds a stop for it: the
          // pool would delete this data directory under a server still serving a loopback port.
          // A stop that fails here is a server that is already gone, which is why the failure
          // the caller sees is the wait's.
          await stop().catch(() => {});
          throw error;
        }
        return { port, url, stop };
      }),
  };
}

/** The container's own lines, the last 20 for a refusal that would otherwise say only that
 *  nothing answered (a server that exits at once leaves its reason there and nowhere else), or
 *  all of them for a failed run's record (`StackDatabase.stop`). */
async function containerLog(container: string, tail: "20" | "all" = "20"): Promise<string> {
  try {
    const { stdout, stderr } = await execFileAsync("docker", ["logs", "--tail", tail, container], {
      maxBuffer: 64 * 1024 * 1024,
    });
    const lines = `${stdout}${stderr}`.trim();
    return lines === "" ? "it logged nothing" : lines;
  } catch (error) {
    return `its log could not be read: ${error instanceof Error ? error.message : String(error)}`;
  }
}

function imageProvider(
  image: string,
  run: string,
  env: NodeJS.ProcessEnv,
  stall: Stall | undefined
): StackPostgres {
  // The stall's directory, read-only at its own path, and the library preloaded into every
  // process of the container, the server's among them.
  const stalled =
    stall === undefined
      ? []
      : [
          "--volume",
          `${stall.dir}:${stall.dir}:ro`,
          ...Object.entries(stall.environment).flatMap(([name, value]) => [
            "--env",
            `${name}=${value}`,
          ]),
        ];
  return {
    name: `the Postgres image ${image}`,
    start: (index, signal) =>
      stackOf(signal, async (): Promise<StackDatabase> => {
        const what = `stack ${index}'s Postgres`;
        let refused = "";
        const started = await serverSettings(env);
        await stall?.built();
        for (let attempt = 1; attempt <= bindAttempts; attempt++) {
          // The name carries the attempt: a container whose port was taken keeps its name until it
          // is removed, and the run removes it with the stack.
          const container = `dispatch-e2e-pg-${run}-${index}-${attempt}`;
          const port = await freePort();
          const remove = async (): Promise<void> => {
            await execFileAsync("docker", ["rm", "--force", "--volumes", container]).catch(
              () => {}
            );
          };
          try {
            await execFileAsync("docker", [
              "run",
              "--detach",
              "--name",
              container,
              "--publish",
              `127.0.0.1:${port}:5432`,
              "--env",
              "POSTGRES_PASSWORD=dispatch",
              "--env",
              `POSTGRES_DB=${databaseName}`,
              "--env",
              "POSTGRES_INITDB_ARGS=--no-sync",
              // Nothing is mounted at the data directory, so the image places it itself and it
              // dies with the container, which is what a stack's data is meant to do. Two mounts
              // were tried and refused: a bind mount of a directory this process made, which the
              // image's own user may neither write nor chown, and a tmpfs at
              // /var/lib/postgresql/data, which this image's Postgres 18 entrypoint reports as
              // "an unused mount/volume" and then exits, since 18 wants one mount a level up
              // (docker-library/postgres#1259).
              ...stalled,
              image,
              ...started.flatMap((setting) => ["-c", setting]),
            ]);
          } catch (error) {
            // `docker run` publishes the port itself, so this is the port another listener took
            // between `freePort` and here, which is what another port answers.
            refused = `${error instanceof Error ? error.message : String(error)}; the container said: ${await containerLog(container)}`;
            await remove();
            continue;
          }
          const url = `postgres://postgres:dispatch@127.0.0.1:${port}/${databaseName}?sslmode=disable`;
          // The container's end is the server's: `docker wait` returns once it has exited, and is
          // stopped once the wait below is over, however it ends.
          const watching = new AbortController();
          let exited = false;
          execFileAsync("docker", ["wait", container], { signal: watching.signal }).then(
            () => {
              exited = true;
            },
            () => {}
          );
          try {
            await ready(url, what, () => exited, signal);
            // The image carries the extension; the database the image made still has to install it,
            // as migration 0072 needs it.
            await sqlAt(url, ["create extension if not exists vector"], signal);
          } catch (error) {
            if (error instanceof Interrupted) {
              await remove();
              throw error;
            }
            // The container holds the port, so no other port is a different question: it is the
            // server inside that never answered, and the refusal says that rather than counting
            // ports. Its own last lines are read before it is removed.
            const said = await containerLog(container);
            await remove();
            throw new Error(
              `The Dispatch e2e harness cannot start: ${what} came up from ${image} on port ` +
                `${port} and then ${error instanceof Error ? error.message : String(error)}; ` +
                `the container said: ${said}`
            );
          } finally {
            watching.abort();
          }
          return {
            port,
            url,
            stop: async (keepLog) => {
              // The log of a `services:` container, which GitHub printed at the end of the job,
              // is what merge-group run 44456291758's failure was read from; a stack's
              // container goes with its run, so a failed run prints it here first.
              if (keepLog) {
                console.error(
                  `e2e/harness-postgres.ts: ${what} (container ${container}) logged:\n` +
                    (await containerLog(container, "all"))
                );
              }
              await execFileAsync("docker", ["rm", "--force", "--volumes", container]);
            },
          };
        }
        throw new Error(
          `The Dispatch e2e harness cannot start: ${what} got none of ${bindAttempts} ports from ` +
            `${image}: ${refused}`
        );
      }),
  };
}

/** The provider this run starts its stacks' Postgres from, or undefined when the caller named
 *  none, which is a run against the `DATABASE_URL` it was given. A disk stall the run asked for
 *  goes to whichever provider it names; a run against a `DATABASE_URL` starts no server to give it
 *  to (e2e/disk-stall.ts). */
export function stackPostgres(options: {
  /** The run's own temporary directory, which it removes when it ends. */
  readonly dir: string;
  /** What names this run's containers apart from another run's on one machine. */
  readonly run: string;
  readonly env?: NodeJS.ProcessEnv;
}): StackPostgres | undefined {
  const env = options.env ?? process.env;
  const image = (env[pgImageVariable] ?? "").trim();
  const bin = (env[pgBinVariable] ?? "").trim();
  if (image !== "" && bin !== "") {
    throw new Error(
      `${pgImageVariable} and ${pgBinVariable} both name a Postgres for this run's stacks. Set ` +
        "one of them: the image is a `docker run` per stack, the directory a server per stack " +
        "from its initdb and pg_ctl."
    );
  }
  const stall = stallOf(env);
  if (image !== "") return imageProvider(image, options.run, env, stall);
  if (bin !== "") return binariesProvider(options.dir, bin, env, stall);
  return undefined;
}
