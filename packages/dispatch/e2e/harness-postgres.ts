/** Each stack's Postgres server, from one of the two providers a caller names.
 *
 *  A stack is a whole harness of its own (`e2e/harness-pool.ts`), and its worker empties its
 *  database before its first row and before the review corpus's seed (`workerDatabase` and
 *  `reviewCorpus` in e2e/test.ts, through `resetDatabase` in `e2e/seed.ts`), deleting whatever
 *  another stack's rows had written there, so two stacks cannot share one: the pool starts a
 *  server per stack and hands it to that stack's Dispatch servers and psql as `DATABASE_URL`.
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
 *  removes (`e2e/harness-pool.ts`), and both serve on a unix socket in a directory of the stack's
 *  own there and on no TCP port: Postgres binds no socket it is handed, and a TCP port found free
 *  and given to it by number is any other process's to take before it binds it. The server's dev
 *  sign-in refuses a `DATABASE_URL` whose host is neither loopback nor a unix socket
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
import { chmodSync, existsSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { buildStallLibrary, diskStallDir, stallEnvironment } from "./disk-stall";
import { Interrupted } from "./local-instance-support";
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
/** How long a stack's server may take to answer `select 1`. */
const readyMs = 120_000;
/** The port a stack's server is told, which names its socket file (`.s.PGSQL.5432`) in the
 *  stack's own socket directory and binds nothing: each server listens on no TCP address. */
const socketPort = 5432;
/** The longest socket path a server can bind: `sun_path` of `struct sockaddr_un` holds 108 bytes
 *  including its terminator, so 107 bytes of path. Measured on this Linux: a 107-byte path binds
 *  and a 108-byte one is refused with EINVAL, "AF_UNIX path too long". */
const socketPathLimit = 107;

/** One stack's Postgres. */
export interface StackDatabase {
  /** The `DATABASE_URL` this stack's servers and psql use. */
  readonly url: string;
  /** The directory of the unix socket it serves on, which the harness logs and no other stack
   *  shares. */
  readonly socketDirectory: string;
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

/** The `DATABASE_URL` of `database` on the server whose socket is in `socketDirectory`, signed in
 *  as `user` (`user:password`): no host in the URL's authority, and the directory as `host` in
 *  its query, the form libpq and pgx both read as a unix socket there. The directory is
 *  percent-encoded whole, so neither reads a space, a `+` or a `/` in it as anything else. A
 *  directory holding a comma is refused: pgx and libpq read `host` as a comma-separated list of
 *  hosts however the comma is encoded, so no URL can name that directory to them. A directory
 *  whose socket file would pass `socketPathLimit` is refused too, naming the path and its length:
 *  the binaries provider's own `pg_ctl` says so at once, but the image provider's server binds
 *  the path inside its container, where nothing this process reads says why, so the run would
 *  otherwise spend `ready`'s {@link readyMs} on a server that can never answer and then show
 *  psql's "too long" from the host side. Exported for `__tests__/harness-postgres-darwin.test.ts`,
 *  which holds both refusals: a start would reach this only after `go` has printed the connection
 *  limit and docker has pulled, which is minutes of work this says nothing about. */
export function socketUrl(user: string, database: string, socketDirectory: string): string {
  if (socketDirectory.includes(",")) {
    throw new Error(
      `The Dispatch e2e harness cannot start: the stack's socket directory ${socketDirectory} ` +
        "holds a comma, which pgx and libpq read as a list of hosts: run it with a TMPDIR whose " +
        "path holds none."
    );
  }
  const socketPath = join(socketDirectory, `.s.PGSQL.${socketPort}`);
  const length = Buffer.byteLength(socketPath);
  if (length > socketPathLimit) {
    throw new Error(
      `The Dispatch e2e harness cannot start: the stack's socket ${socketPath} is ${length} ` +
        `bytes, past the ${socketPathLimit} a unix socket path holds, so no server could bind ` +
        "it: run it with a shorter TMPDIR."
    );
  }
  return (
    `postgres://${user}@/${database}?host=${encodeURIComponent(socketDirectory)}` +
    `&port=${socketPort}&sslmode=disable`
  );
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
    if (signal?.aborted === true) throw new Interrupted();
    throw error;
  }
}

/** Waits until `url` answers `select 1`: a server that is up has read its data directory, run
 *  its own recovery and opened its socket. With `pidOne`, it waits until the server that answers
 *  is its container's process 1, read from the first line of its `postmaster.pid`: the image's
 *  entrypoint first runs a server of its own on the same socket to make the database, stops it,
 *  and only then hands the container to the server it was given. A server on a TCP port the
 *  entrypoint's own never listens on was the only one to answer there; on the socket both do. */
async function ready(
  url: string,
  what: string,
  signal?: AbortSignal,
  pidOne = false
): Promise<void> {
  const deadline = Date.now() + readyMs;
  for (;;) {
    let failure: unknown;
    try {
      const statement = pidOne
        ? "select split_part(pg_read_file('postmaster.pid'), chr(10), 1)"
        : "select 1";
      const answer = (await sqlAt(url, [statement], signal)).trim();
      if (!pidOne || answer === "1") return;
      failure = `the server that answers is process ${answer}, the entrypoint's own, not process 1`;
    } catch (error) {
      // The psql above was killed by the signal, or the one before it was: a stopping run waits
      // out none of the deadline that is left.
      if (signal?.aborted === true) throw new Interrupted();
      failure = error instanceof Error ? error.message : String(error);
    }
    if (Date.now() > deadline) {
      throw new Error(
        `The Dispatch e2e harness cannot start: ${what} did not answer within ` +
          `${readyMs / 1000} s: ${String(failure)}`
      );
    }
    await sleep(200);
  }
}

/** Starts the cluster in `data`, with its log and its unix socket in `dir` and no TCP address
 *  (`listen_addresses` empty): its databases are `socketUrl(…, dir)`'s. Returns or throws with no
 *  postmaster of `data` left running. `server` is the environment pg_ctl, and so the postmaster,
 *  runs with. */
async function startCluster(
  bin: string,
  data: string,
  dir: string,
  what: string,
  env: NodeJS.ProcessEnv,
  server: NodeJS.ProcessEnv
): Promise<void> {
  const options = [
    `-p ${socketPort}`,
    "-c listen_addresses=",
    `-k ${dir}`,
    ...(await serverSettings(env)).map((s) => `-c ${s}`),
  ];
  try {
    await execFileAsync(
      join(bin, "pg_ctl"),
      ["-D", data, "-l", join(dir, "postgres.log"), "-o", options.join(" "), "-w", "start"],
      { env: server }
    );
  } catch (error) {
    // `pg_ctl -w start` gives up its own wait after `PGCTLTIMEOUT` and exits non-zero, but the
    // postmaster it started goes on running and holding `postmaster.pid`: a throw that left it
    // would leave a server serving while the pool removes the data directory under it
    // (`e2e/harness-pool.ts`). A stop that fails is a server that never started, which is why the
    // failure the caller sees stays the start's.
    await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "immediate", "-w", "stop"]).catch(
      () => {}
    );
    throw new Error(
      `The Dispatch e2e harness cannot start: ${what} did not start ` +
        `(${join(dir, "postgres.log")}): ${error instanceof Error ? error.message : String(error)}`
    );
  }
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
    await startCluster(bin, data, templateDir, "the stacks' Postgres template", env, server);
    try {
      const admin = socketUrl("postgres", "postgres", templateDir);
      await ready(admin, "the stacks' Postgres template", signal);
      await sqlAt(admin, [`create database ${databaseName}`], signal);
      await sqlAt(
        socketUrl("postgres", databaseName, templateDir),
        ["create extension vector"],
        signal
      );
    } finally {
      // Whatever happened above: the postmaster is up from `startCluster` on, and a throw that
      // left it running would leave it serving while the pool removes the data directory under
      // it.
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
        await startCluster(bin, data, stackDir, what, env, server);
        const stop = async (): Promise<void> => {
          await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "immediate", "-w", "stop"]);
        };
        const url = socketUrl("postgres", databaseName, stackDir);
        try {
          await ready(url, what, signal);
        } catch (error) {
          // The postmaster is up from `startCluster` on, and a wait the run's signal or the
          // deadline ended returns no `StackDatabase`, so no caller holds a stop for it: the
          // pool would delete this data directory under a server still serving. A stop that fails
          // here is a server that is already gone, which is why the failure the caller sees is the
          // wait's.
          await stop().catch(() => {});
          throw error;
        }
        return { socketDirectory: stackDir, url, stop };
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
  dir: string,
  run: string,
  env: NodeJS.ProcessEnv,
  stall: Stall | undefined
): StackPostgres {
  // Each stack's server answers on a unix socket in a directory of this machine that docker mounts
  // into its container. A docker engine that runs its containers in a VM carries no unix socket
  // across that mount, so the socket would never answer: on macOS, where every engine is such a
  // VM (Docker Desktop, colima), the run is refused here, by name, before any container starts.
  // The same holds for a VM-backed engine on Linux, which this cannot read from the platform:
  // such a run fails at the ready wait with psql finding no socket, and the refusal below names
  // the variable that runs a local Postgres instead.
  if (process.platform === "darwin") {
    throw new Error(
      `The Dispatch e2e harness cannot start: ${pgImageVariable} serves each stack's Postgres on ` +
        "a unix socket in a directory docker mounts into the container, and a docker engine in a " +
        "VM (Docker Desktop, colima, and every engine on macOS) carries no unix socket out of " +
        "that VM. Name a local Postgres installation's " +
        `directory, the one holding initdb and pg_ctl, in ${pgBinVariable} instead, and unset ` +
        `${pgImageVariable}.`
    );
  }
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
        const started = await serverSettings(env);
        await stall?.built();
        const container = `dispatch-e2e-pg-${run}-${index}`;
        // The server's socket directory: a directory of the stack's own under the run's, mounted
        // where the image's server makes its socket. The server runs as the image's own `postgres`
        // user, which the directory lets write there (0777, with no sticky bit, under the run's
        // directory, which only this user may enter), so this user may remove its socket and lock
        // file with the run however the server ended. Not this process's own user: the image's
        // entrypoint runs a user its /etc/passwd does not name under nss_wrapper, setting
        // LD_PRELOAD to that library and unsetting it after initdb, so a disk stall's library
        // (`stalled` below) would never reach the server.
        const socketDirectory = join(dir, `pg-${index}`);
        const url = socketUrl("postgres:dispatch", databaseName, socketDirectory);
        mkdirSync(socketDirectory, { recursive: true });
        chmodSync(socketDirectory, 0o777);
        const remove = async (): Promise<void> => {
          await execFileAsync("docker", ["rm", "--force", "--volumes", container]).catch(() => {});
        };
        try {
          await execFileAsync("docker", [
            "run",
            "--detach",
            "--name",
            container,
            "--user",
            "postgres",
            "--volume",
            `${socketDirectory}:/var/run/postgresql`,
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
            // No TCP address: the stack reaches the server through its socket alone, and no port
            // is published, so none is one another process could take first.
            "-c",
            "listen_addresses=",
            ...started.flatMap((setting) => ["-c", setting]),
          ]);
        } catch (error) {
          const said = await containerLog(container);
          await remove();
          throw new Error(
            `The Dispatch e2e harness cannot start: ${what} did not start from ${image}: ` +
              `${error instanceof Error ? error.message : String(error)}; the container said: ${said}`
          );
        }
        try {
          await ready(url, what, signal, true);
          // The image carries the extension; the database the image made still has to install it,
          // as migration 0072 needs it.
          await sqlAt(url, ["create extension if not exists vector"], signal);
        } catch (error) {
          // It is the server inside that never answered. Its own last lines are read before it is
          // removed.
          const said = await containerLog(container);
          await remove();
          throw new Error(
            `The Dispatch e2e harness cannot start: ${what} came up from ${image} on its socket in ` +
              `${socketDirectory} and then ${error instanceof Error ? error.message : String(error)}; ` +
              `the container said: ${said}`
          );
        }
        return {
          socketDirectory,
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
  if (image !== "") return imageProvider(image, options.dir, options.run, env, stall);
  if (bin !== "") return binariesProvider(options.dir, bin, env, stall);
  return undefined;
}
