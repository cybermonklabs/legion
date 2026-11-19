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
 *  started so already; `stallLoaded` is how a row tells the two apart from one that was not. */
import { execFile } from "node:child_process";
import { chmodSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
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

/** What a server started with the stall needs in its environment. */
export function stallEnvironment(dir: string): Record<string, string> {
  return { LD_PRELOAD: join(dir, library), PG_STALL_FILE: join(dir, "stalled") };
}

/** Builds `fsync-stall.so` into `dir` with the machine's C compiler, readable by any user, since
 *  a container's Postgres runs as a user of its own. */
export async function buildStallLibrary(dir: string): Promise<void> {
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

/** Whether the server this run's SQL reaches loaded the library: its backend's own memory map,
 *  read by the superuser the suite connects as. */
export async function stallLoaded(): Promise<boolean> {
  return (await sql(`SELECT pg_read_file('/proc/self/maps') LIKE '%/${library}%'`)) === "t";
}

/** The disk stops answering: from here every fsync and fdatasync of the server waits. */
export function stallDisk(dir: string): void {
  writeFileSync(stallEnvironment(dir).PG_STALL_FILE, "");
}

/** The disk answers again, and every call that waited goes through. */
export function endStall(dir: string): void {
  rmSync(stallEnvironment(dir).PG_STALL_FILE, { force: true });
}
