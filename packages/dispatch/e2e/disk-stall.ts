/** A run's Postgres whose disk can be made to stop answering, for the rows that read what the
 *  suite does while a commit cannot reach the disk (e2e/read-marker-disk-stall.multiprocess.e2e.ts).
 *
 *  `DISPATCH_E2E_PG_STALL_DIR` names an absolute directory that holds `fsync-stall.so`, built from
 *  e2e/fsync-stall.c, and that the run's Postgres server sees at the same path, with that library
 *  preloaded (`LD_PRELOAD`) and `PG_STALL_FILE` naming `stalled` in the same directory. While that
 *  file exists, every `fsync` and `fdatasync` the server makes waits, as a disk that has stopped
 *  answering makes it wait; once it is gone, each call goes through. `stallDisk` and `endStall`
 *  make and remove it.
 *
 *  The harness starts its stacks' Postgres that way itself when the variable is set
 *  (e2e/harness-postgres.ts builds the library into the directory and gives it to the server). A
 *  run against a `DATABASE_URL` it was given starts no Postgres, so that server has to have been
 *  started so already; `stallServer` is how a row tells the two apart from one that was not. */
import { execFile } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { isAbsolute, join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { sql } from "./psql";

const execFileAsync = promisify(execFile);

/** The directory the library and the stall file are in, the same path for this process and for
 *  the Postgres server. */
export const diskStallVariable = "DISPATCH_E2E_PG_STALL_DIR";

const library = "fsync-stall.so";
const source = fileURLToPath(new URL("./fsync-stall.c", import.meta.url));

/** The directory `DISPATCH_E2E_PG_STALL_DIR` names, or undefined when it is unset or empty. A
 *  relative path is refused: the server is handed the same path, and a container resolves it
 *  against a working directory of its own. */
export function diskStallDir(env: NodeJS.ProcessEnv = process.env): string | undefined {
  const dir = (env[diskStallVariable] ?? "").trim();
  if (dir === "") return undefined;
  if (!isAbsolute(dir)) {
    throw new Error(
      `${diskStallVariable}=${JSON.stringify(dir)} is not an absolute path. The run's Postgres is ` +
        "given the same path, and a container would resolve a relative one against its own."
    );
  }
  return dir;
}

/** What a server started with the stall needs in its environment. Every stack of a run shares
 *  the one stall file, so a stall reaches each stack's server: the row that stalls runs in
 *  two-process mode, which is one stack, and CI's multiprocess job runs one worker. */
export function stallEnvironment(dir: string) {
  return { LD_PRELOAD: join(dir, library), PG_STALL_FILE: join(dir, "stalled") };
}

/** Builds `fsync-stall.so` into `dir` with the machine's C compiler, readable by any user, since
 *  a container's Postgres runs as a user of its own: the directory is made readable by every user
 *  too, and a container sees all of it. So it must be empty, missing, or hold only what an
 *  earlier build left there; a directory of the developer's own, which that would widen and
 *  expose, is refused. */
export async function buildStallLibrary(dir: string): Promise<void> {
  const others = existsSync(dir)
    ? readdirSync(dir).filter((name) => name !== library && name !== "stalled")
    : [];
  if (others.length > 0) {
    throw new Error(
      `The Dispatch e2e harness cannot start: ${diskStallVariable}=${dir} holds ` +
        `${others.slice(0, 3).join(", ")}${others.length > 3 ? ", ..." : ""}. Name a directory ` +
        "that is empty or does not exist yet: the harness makes it readable by every user (0755) " +
        "and mounts the whole of it into a container."
    );
  }
  mkdirSync(dir, { recursive: true });
  chmodSync(dir, 0o755);
  const built = join(dir, library);
  try {
    await execFileAsync("cc", ["-shared", "-fPIC", "-O2", "-o", built, source]);
  } catch (error) {
    throw new Error(
      `The Dispatch e2e harness cannot start: ${diskStallVariable} is set, and building ${source} ` +
        `into ${built} with cc failed: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  chmodSync(built, 0o755);
}

/** Where the server's fsync probe leaves its exit status: its data directory, the working
 *  directory of each of its processes and the root of `pg_read_file`'s relative paths. */
const probeStatus = "fsync-stall-probe.rc";

/** How the server's fsync probe ended: 124 when `dd`'s `fsync` was still waiting at `timeout`'s
 *  1 s. The server runs it (`COPY ... TO PROGRAM`), so it carries the server's own environment,
 *  the `LD_PRELOAD` and `PG_STALL_FILE` the library reads, and sees the server's files. Its
 *  output is /dev/null, whose `fsync` fails at once (`EINVAL`, status 1) where the library lets
 *  it through, so no disk decides how long it takes; and the statement writes nothing the
 *  server commits, so it does not wait on a server whose commits wait for the disk. */
async function serverFsyncProbe(): Promise<string> {
  await sql(
    "COPY (SELECT 1) TO PROGRAM 'timeout 1 dd if=/dev/zero of=/dev/null bs=1 count=1 " +
      `conv=fsync status=none 2>/dev/null; echo $? > ${probeStatus}'`
  );
  return sql(`SELECT trim(pg_read_file('${probeStatus}'))`);
}

/** Stops the disk answering, as `stallDisk` does, and returns what keeps that stall from holding
 *  the server this run's SQL reaches, read on that server by the superuser the suite connects as;
 *  none once it holds, which is when every commit of that server that waits for the disk waits
 *  for as long as the stall file exists:
 *
 *  - its processes have `dir`'s fsync-stall.so in their memory map;
 *  - an `fsync` made in its environment goes through before the stall file exists, and waits
 *    once it does (`serverFsyncProbe`). That reads what the library does in the server's
 *    environment rather than what the server was given: a server that watches another file, or
 *    cannot see this one, lets the second probe through, and one whose `fsync` waits with no
 *    stall, on a file that is never removed or a probe too slow to tell, holds the first; and
 *  - its WAL flush is an `fsync` or an `fdatasync` (`wal_sync_method`), the calls the library
 *    holds, and not a write opened `O_SYNC` or `O_DSYNC`.
 *
 *  No statement of the server's own can stand in for the probe: with `fsync` off, the setting the
 *  stall exists to test, Postgres makes no `fsync` at all, so a `CHECKPOINT` or a
 *  `synchronous_commit = on` commit goes through while the stall holds, as it does on a server
 *  the stall never reached. */
export async function stallServer(dir: string): Promise<string[]> {
  const preloaded = stallEnvironment(dir).LD_PRELOAD;
  const missing: string[] = [];
  const before = await serverFsyncProbe();
  if (before === "124") {
    missing.push("an fsync made in its environment was still waiting after 1 s before the stall");
  }
  stallDisk(dir);
  const during = await serverFsyncProbe();
  if (during !== "124") {
    missing.push(
      `an fsync made in its environment ended with status ${during} during the stall, rather ` +
        `than waiting out the probe's 1 s: it does not wait on ${stallEnvironment(dir).PG_STALL_FILE}`
    );
  }
  const [loaded, flush] = (
    await sql(
      `SELECT position('${preloaded.replaceAll("'", "''")}' in pg_read_file('/proc/self/maps')) > 0, ` +
        "current_setting('wal_sync_method')"
    )
  ).split("|");
  if (loaded !== "t") missing.push(`its processes have not loaded ${preloaded}`);
  if (flush !== "fsync" && flush !== "fdatasync") {
    missing.push(`its WAL flush is wal_sync_method=${flush}, not fsync or fdatasync`);
  }
  return missing;
}

/** The disk stops answering: from here every fsync and fdatasync of the server waits. */
export function stallDisk(dir: string): void {
  writeFileSync(stallEnvironment(dir).PG_STALL_FILE, "");
}

/** The disk answers again, and every call that waited goes through. */
export function endStall(dir: string): void {
  rmSync(stallEnvironment(dir).PG_STALL_FILE, { force: true });
}
