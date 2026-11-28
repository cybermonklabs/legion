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
 *  against a working directory of its own. So is a path holding any whitespace or a colon: the
 *  loader splits `LD_PRELOAD` on a space and a colon, so the server would never load the library,
 *  and its memory map escapes a newline, so the check that it did would fail; a tab or other
 *  whitespace is refused with them, one rule rather than a list of the characters each part
 *  reads. */
export function diskStallDir(env: NodeJS.ProcessEnv = process.env): string | undefined {
  const dir = (env[diskStallVariable] ?? "").trim();
  if (dir === "") return undefined;
  if (!isAbsolute(dir)) {
    throw new Error(
      `${diskStallVariable}=${JSON.stringify(dir)} is not an absolute path. The run's Postgres is ` +
        "given the same path, and a container would resolve a relative one against its own."
    );
  }
  if (/[\s:]/u.test(dir)) {
    throw new Error(
      `${diskStallVariable}=${JSON.stringify(dir)} holds whitespace (a space, a tab, a newline or ` +
        "any other) or a colon, and none is allowed. The run's Postgres preloads the library " +
        "under it through LD_PRELOAD, which the loader splits on a space and a colon, and its " +
        "memory map escapes a newline, so the run could not load the library or tell that it had."
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
  // A stall file an earlier run left behind would hold the row's first probe, made before its own
  // stall, so the row would fail as though the server's flush were slow; a build clears it. (No
  // write of the server's own would wait on it: every server this build serves runs fsync off.)
  rmSync(stallEnvironment(dir).PG_STALL_FILE, { force: true });
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

/** Where the server's flush probe leaves its exit status: its data directory, the working
 *  directory of each of its processes and the root of `pg_read_file`'s relative paths. */
const probeStatus = "fsync-stall-probe.rc";

/** The program the probe runs for each WAL flush the library holds: coreutils `sync` on
 *  /dev/null makes exactly that call, `fdatasync` with `--data`, `fsync` without, and the call
 *  fails at once (`EINVAL`, status 1) where the library lets it through. `dd conv=fdatasync`
 *  cannot stand in: on /dev/null it falls back to `fsync`, so a library that held `fsync` alone
 *  would pass for one that holds the server's `fdatasync`. */
const flushProgram = {
  fdatasync: "sync --data /dev/null",
  fsync: "sync /dev/null",
} as const;

/** How the server's flush probe ended: 124 when the `sync` was still waiting at `timeout`'s 1 s.
 *  The server runs it (`COPY ... TO PROGRAM`), so it carries the server's own environment, the
 *  `LD_PRELOAD` and `PG_STALL_FILE` the library reads, and sees the server's files; and the
 *  statement writes nothing the server commits, so it does not wait on a server whose commits
 *  wait for the disk. The query sends the program no row: the program never reads its input, and
 *  a row Postgres flushes into the pipe after the program has exited fails the close with
 *  `Broken pipe`, which a backend paused for a few milliseconds hits. */
async function serverFlushProbe(call: keyof typeof flushProgram): Promise<string> {
  await sql(
    `COPY (SELECT 1 WHERE false) TO PROGRAM 'timeout 1 ${flushProgram[call]} 2>/dev/null; ` +
      `echo $? > ${probeStatus}'`
  );
  return sql(`SELECT trim(pg_read_file('${probeStatus}'))`);
}

/** Stops the disk answering, as `stallDisk` does, and returns what keeps that stall from holding
 *  the server this run's SQL reaches, read on that server by the superuser the suite connects as;
 *  none once it holds, which is when every commit of that server that waits for the disk waits
 *  for as long as the stall file exists:
 *
 *  - its WAL flush is an `fsync` or an `fdatasync` (`wal_sync_method`), the calls the library
 *    holds, and not a write opened `O_SYNC` or `O_DSYNC`;
 *  - that very call, made in its environment, goes through before the stall file exists, and
 *    waits once it does (`serverFlushProbe`). That reads what the library does in the server's
 *    environment rather than what the server was given: a server that watches another file, or
 *    cannot see this one, or runs a library that holds the other call only, lets the second probe
 *    through, and one whose flush waits with no stall, on a file that is never removed or a probe
 *    too slow to tell, holds the first; and
 *  - its processes have `dir`'s fsync-stall.so in their memory map.
 *
 *  No statement of the server's own can stand in for the probe: with `fsync` off, the setting the
 *  stall exists to test, Postgres makes no `fsync` at all, so a `CHECKPOINT` or a
 *  `synchronous_commit = on` commit goes through while the stall holds, as it does on a server
 *  the stall never reached. */
export async function stallServer(dir: string): Promise<string[]> {
  const preloaded = stallEnvironment(dir).LD_PRELOAD;
  const missing: string[] = [];
  const flush = await sql("SELECT current_setting('wal_sync_method')");
  if (flush !== "fsync" && flush !== "fdatasync") {
    missing.push(`its WAL flush is wal_sync_method=${flush}, not fsync or fdatasync`);
  }
  // A flush the library does not hold is probed as an fsync, which the reason above already fails.
  const call = flush === "fdatasync" ? "fdatasync" : "fsync";
  const before = await serverFlushProbe(call);
  if (before === "124") {
    missing.push(`an ${call} made in its environment was still waiting after 1 s before the stall`);
  }
  stallDisk(dir);
  const during = await serverFlushProbe(call);
  if (during !== "124") {
    missing.push(
      `an ${call} made in its environment ended with status ${during} during the stall, rather ` +
        `than waiting out the probe's 1 s: it does not wait on ${stallEnvironment(dir).PG_STALL_FILE}`
    );
  }
  // A maps line ends with the mapped file's path, after a space, so the path is matched between
  // the two: another directory ending in this one's name does not match. A library rebuilt under a
  // running server is listed with ` (deleted)` after its path, and one preloaded through a
  // symlinked directory under its resolved path; either reads as not loaded, which fails the row
  // rather than passing it.
  const loaded = await sql(
    `SELECT position(' ${preloaded.replaceAll("'", "''")}' || chr(10) in pg_read_file('/proc/self/maps')) > 0`
  );
  if (loaded !== "t") {
    missing.push(
      `its processes have not loaded ${preloaded}, or it was replaced since they started`
    );
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
