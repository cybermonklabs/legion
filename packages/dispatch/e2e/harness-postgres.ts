/** Each stack's Postgres server, from one of the two providers a caller names.
 *
 *  A stack is a whole harness of its own (`e2e/harness-pool.ts`), and `e2e/seed.ts` truncates its
 *  database before every row, so two stacks cannot share one: the pool starts a server per stack
 *  and hands it to that stack's Dispatch servers and psql as `DATABASE_URL`.
 *
 *  - `DISPATCH_E2E_PG_IMAGE` is a `docker run` of the image digest the caller names: a
 *    developer's path on a machine with Docker and no Postgres binaries, and the `visual` job's
 *    of `.github/workflows/dispatch-browser.yaml`, which runs on the runner with only its
 *    Playwright command in a container, so the binaries of an image it does not run in are no
 *    route for it.
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
 *  since the 1.0 s of migrations it takes out of a stack's boot runs in every stack at once. */
import { execFile } from "node:child_process";
import { existsSync, mkdirSync } from "node:fs";
import { createServer } from "node:net";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { Interrupted } from "./local-instance-support";
import { sqlAt } from "./psql";

const execFileAsync = promisify(execFile);

/** A `docker run` of this image serves one stack. */
export const pgImageVariable = "DISPATCH_E2E_PG_IMAGE";
/** This directory's `initdb` and `pg_ctl` serve every stack. */
export const pgBinVariable = "DISPATCH_E2E_PG_BIN";

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
/** A stack's connection limit: what `go` prints for `connectionsArgs`, plus `superuserReserved`.
 *  It comes from the Go code, so the harness holds no count of a Dispatch process's pool. A
 *  server that allows fewer does not fail where the size is wrong. It fails later, as a 53300
 *  refusal inside a room, a lease or a socket, and nothing in that failure names the cause. The
 *  first stack that starts reads it, once per run. */
let maxConnections: Promise<number> | undefined;
async function readMaxConnections(): Promise<number> {
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
  const printed = /^([1-9][0-9]*)\n?$/.exec(stdout)?.[1];
  const server = printed === undefined ? Number.NaN : Number(printed);
  if (!Number.isSafeInteger(server)) {
    throw new Error(
      `The Dispatch e2e harness cannot start: ${named} printed ${JSON.stringify(stdout)}, ` +
        "not a positive whole number of connections."
    );
  }
  return server + superuserReserved;
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
  // whole cluster (`fsync`, `full_page_writes`) to a power cut.
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
async function serverSettings(): Promise<string[]> {
  maxConnections ??= readMaxConnections();
  return [`max_connections=${await maxConnections}`, ...settings];
}
/** How long a stack's server may take to answer `select 1`. */
const readyMs = 120_000;
/** How many ports a cluster tries before it gives up (see `freePort`). */
const bindAttempts = 3;

/** One stack's Postgres. */
export interface StackDatabase {
  /** The `DATABASE_URL` this stack's servers and psql use. */
  readonly url: string;
  /** The port it serves on, which the harness logs and no other stack shares. */
  readonly port: number;
  stop(): Promise<void>;
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
    if (signal?.aborted === true) throw new Interrupted();
    throw error;
  }
}

/** Waits until `url` answers `select 1`: a server that is up has read its data directory, run
 *  its own recovery and opened its port. */
async function ready(url: string, what: string, signal?: AbortSignal): Promise<void> {
  const deadline = Date.now() + readyMs;
  for (;;) {
    try {
      await sqlAt(url, ["select 1"], signal);
      return;
    } catch (error) {
      // The psql above was killed by the signal, or the one before it was: a stopping run waits
      // out none of the deadline that is left.
      if (signal?.aborted === true) throw new Interrupted();
      if (Date.now() > deadline) {
        throw new Error(
          `The Dispatch e2e harness cannot start: ${what} did not answer within ` +
            `${readyMs / 1000} s: ${error instanceof Error ? error.message : String(error)}`
        );
      }
      await sleep(200);
    }
  }
}

/** Starts the cluster in `data`, with its log and its unix socket in `dir`, and returns the port
 *  it serves on. Returns or throws with no postmaster of `data` left running. */
async function startCluster(bin: string, data: string, dir: string, what: string): Promise<number> {
  let last = "";
  const started = await serverSettings();
  for (let attempt = 1; attempt <= bindAttempts; attempt++) {
    const port = await freePort();
    const options = [`-p ${port}`, "-h 127.0.0.1", `-k ${dir}`, ...started.map((s) => `-c ${s}`)];
    try {
      await execFileAsync(join(bin, "pg_ctl"), [
        "-D",
        data,
        "-l",
        join(dir, "postgres.log"),
        "-o",
        options.join(" "),
        "-w",
        "start",
      ]);
      return port;
    } catch (error) {
      last = error instanceof Error ? error.message : String(error);
      // `pg_ctl -w start` gives up its own wait after `PGCTLTIMEOUT` and exits non-zero, but
      // the postmaster it started goes on running and holding `postmaster.pid`: the next
      // attempt's start would fail on that lock rather than on its port, and a throw here would
      // leave a server serving a loopback port while the pool removes the data directory under
      // it (`e2e/harness-pool.ts`). A stop that fails is a server that never bound, which is why
      // the failure the caller sees stays the start's.
      await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "immediate", "-w", "stop"]).catch(
        () => {}
      );
    }
  }
  throw new Error(
    `The Dispatch e2e harness cannot start: ${what} did not start on any of ${bindAttempts} ` +
      `ports (${join(dir, "postgres.log")}): ${last}`
  );
}

function binariesProvider(dir: string, bin: string): StackPostgres {
  for (const tool of ["initdb", "pg_ctl"]) {
    if (!existsSync(join(bin, tool))) {
      throw new Error(
        `The Dispatch e2e harness cannot start: ${pgBinVariable}=${bin} holds no ${tool}. It ` +
          "must name the directory of a Postgres installation, the one holding initdb and pg_ctl."
      );
    }
  }
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
    const port = await startCluster(bin, data, templateDir, "the stacks' Postgres template");
    try {
      const admin = `postgres://postgres@127.0.0.1:${port}/postgres?sslmode=disable`;
      await ready(admin, "the stacks' Postgres template", signal);
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
        const port = await startCluster(bin, data, stackDir, what);
        const stop = async (): Promise<void> => {
          await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "immediate", "-w", "stop"]);
        };
        const url = `postgres://postgres@127.0.0.1:${port}/${databaseName}?sslmode=disable`;
        try {
          await ready(url, what, signal);
        } catch (error) {
          // The postmaster is up from `startCluster` on, and a wait the run's signal or the
          // deadline ended returns no `StackDatabase`, so no caller holds a stop for it: the
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

/** The container's own last lines, for a refusal that would otherwise say only that nothing
 *  answered: a server that exits at once leaves its reason there and nowhere else. */
async function containerLog(container: string): Promise<string> {
  try {
    const { stdout, stderr } = await execFileAsync("docker", ["logs", "--tail", "20", container]);
    const lines = `${stdout}${stderr}`.trim();
    return lines === "" ? "it logged nothing" : lines;
  } catch (error) {
    return `its log could not be read: ${error instanceof Error ? error.message : String(error)}`;
  }
}

function imageProvider(image: string, run: string): StackPostgres {
  return {
    name: `the Postgres image ${image}`,
    start: (index, signal) =>
      stackOf(signal, async (): Promise<StackDatabase> => {
        const what = `stack ${index}'s Postgres`;
        let refused = "";
        const started = await serverSettings();
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
          try {
            await ready(url, what, signal);
            // The image carries the extension; the database the image made still has to install it,
            // as migration 0072 needs it.
            await sqlAt(url, ["create extension if not exists vector"], signal);
          } catch (error) {
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
          }
          return {
            port,
            url,
            stop: async () => {
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
 *  none, which is a run against the `DATABASE_URL` it was given. */
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
  if (image !== "") return imageProvider(image, options.run);
  if (bin !== "") return binariesProvider(options.dir, bin);
  return undefined;
}
