import {
  closeSync,
  constants,
  lstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  renameSync,
  type Stats,
  writeFileSync,
} from "node:fs";
import { basename, dirname, isAbsolute, join, normalize, relative } from "node:path";

/** What the name of a run's own temporary directory starts with: the pool makes it with
 *  `mkdtemp`, and a start reads the marks of each directory of `tmpdir()` so named. */
export const runDirectoryPrefix = "dispatch-e2e-run-";
/** The directory of a run directory that holds one mark file per Postgres. */
export const marksDirectory = "postgres-marks";
/** The container label that holds the mark of an image provider's container. */
export const markLabel = "dev.legion.dispatch-e2e.postgres-mark";
/** The directory a long `TMPDIR` falls back to, as each module's `internal/testtmp` does; a
 *  socket directory is in the run directory or here (`socketDirectory` of
 *  `e2e/harness-postgres.ts`). */
export const shortTemporaryDirectory = "/tmp";

/** The process that started a Postgres, as `/proc` names it in one boot, PID namespace and time
 *  namespace. */
export interface Owner {
  readonly pid: number;
  /** Field 22 of `/proc/<pid>/stat`, the process's start in clock ticks since boot, as the
   *  owner's own time namespace shifts it. */
  readonly start: string;
  /** `/proc/sys/kernel/random/boot_id`. */
  readonly boot: string;
  /** The target of `/proc/self/ns/pid`, such as `pid:[4026531836]`. */
  readonly pidNamespace: string;
  /** The target of `/proc/self/ns/time`, such as `time:[4026531834]`, or "" on a kernel with no
   *  time namespaces, where every process reads one clock. */
  readonly timeNamespace: string;
  readonly uid: number;
}

/** One Postgres of a stack: the process that started it, null where that process's machine has
 *  no `/proc`, and what the Postgres leaves when that process ends without stopping it. */
export interface Mark {
  readonly owner: Owner | null;
  /** The run's own temporary directory, the one the pool hands `stackPostgres`, or a shell
   *  stack's work directory. */
  readonly run: string;
  /** `run` as `identityOf` read it when the mark was written: the one spelling of the run
   *  directory that every sweep acts on (`markedPaths`). */
  readonly identity: string;
  /** The directory of its unix socket, in `run` or in `shortTemporaryDirectory`. Every postmaster
   *  has one; a container of a shell stack, which serves on a port Docker publishes, has none
   *  (`e2e/postgres-mark.ts`). */
  readonly sockets?: string;
  /** The binaries provider's cluster: the data directory and the directory of the binaries that
   *  run it, whose `postgres` a start compares with the program the postmaster runs. No start runs
   *  anything in it. */
  readonly postmaster?: { readonly bin: string; readonly data: string };
  /** The image provider's or a shell stack's container, by the name its `docker create` gave
   *  it. */
  readonly container?: string;
}

/** The fields of `stat`, the text of a `/proc/<pid>/stat`, from the third on, the state first.
 *  They are split after the last `)`, the one that ends the command name, since that name may
 *  hold spaces and parentheses of its own, so field N of proc(5) is at index N - 3. */
function statFields(stat: string): string[] {
  return stat.slice(stat.lastIndexOf(")") + 2).split(" ");
}

/** Field 22 of `stat`, the text of a `/proc/<pid>/stat`: the process's start in clock ticks since
 *  boot. */
export function startOf(stat: string): string {
  return statFields(stat)[22 - 3] ?? "";
}

/** The `code` of an error a call of `node:fs` threw. */
export function codeOf(error: unknown): string | undefined {
  return (error as NodeJS.ErrnoException).code;
}

/** The first line of what an error says, for a log line. */
export function firstLine(error: unknown): string {
  return (error instanceof Error ? error.message : String(error)).split("\n")[0] ?? "";
}

/** This process as an owner, read once: undefined until the first read, null where there is no
 *  owner to read. */
let thisOwner: Owner | null | undefined;

/** This process as the owner of the Postgres it starts, or null where it cannot be told: where
 *  there is no `/proc` (macOS), and where `/proc` gives this process another pid than
 *  `process.pid`, which is a `/proc` of another PID namespace than its own. */
export function stackOwner(): Owner | null {
  if (thisOwner === undefined) thisOwner = readOwner("self", process.pid);
  return thisOwner;
}

/** The process `pid` of this PID namespace as an owner, or null where it cannot be told: this
 *  process as `stackOwner` reads it, or another, such as the driver of a shell stack, which names
 *  itself by its pid (`e2e/postgres-mark.ts`). */
export function processOwner(pid: number): Owner | null {
  if (pid === process.pid) return stackOwner();
  // `/proc` names that pid as this process's own PID namespace does only where it names this
  // process by `process.pid`.
  if (stackOwner() === null) return null;
  return readOwner(String(pid), pid);
}

/** The owner `/proc/<entry>` names, where its pid is `pid`: "self" for this process. */
function readOwner(entry: string, pid: number): Owner | null {
  const uid = process.getuid?.();
  if (uid === undefined) return null;
  try {
    const stat = readFileSync(`/proc/${entry}/stat`, "utf8");
    if (Number.parseInt(stat, 10) !== pid) return null;
    return {
      pid,
      start: startOf(stat),
      boot: readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim(),
      pidNamespace: readlinkSync(`/proc/${entry}/ns/pid`),
      timeNamespace: timeNamespaceOf(entry),
      uid: entry === "self" ? uid : lstatSync(`/proc/${entry}`).uid,
    };
  } catch {
    return null;
  }
}

/** The target of `/proc/<entry>/ns/time`, or "" where the kernel has no time namespaces (Linux
 *  before 5.6), which no process then can be in. */
function timeNamespaceOf(entry: string): string {
  try {
    return readlinkSync(`/proc/${entry}/ns/time`);
  } catch (error) {
    if (codeOf(error) === "ENOENT") return "";
    throw error;
  }
}

/** The one spelling of the directory `path`, with every link in it resolved: the spelling a mark
 *  holds its run directory by, read once where the mark is written (`markPostgres`), and the one
 *  a start lists the run directories of its `TMPDIR` by. Throws where `path` cannot be resolved. */
export function identityOf(path: string): string {
  return realpathSync(path);
}

/** Writes the mark of one Postgres of the run in `run`, under `name`, and returns it. The caller
 *  writes it before the Postgres can outlive this process: before `pg_ctl start`, and before the
 *  `docker create` that carries it as `labelOf`. */
export function markPostgres(
  run: string,
  name: string,
  names: Pick<Mark, "sockets" | "postmaster" | "container">
): Mark {
  const mark: Mark = { owner: stackOwner(), run, identity: identityOf(run), ...names };
  const directory = join(run, marksDirectory);
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  const file = join(directory, `${name}.json`);
  // Written beside and renamed into place, so a start reads a whole mark or none: a mark cut off
  // mid-write would be one that no start can read, and its Postgres would stay.
  const partial = `${file}.${process.pid}.partial`;
  writeFileSync(partial, JSON.stringify(mark), { mode: 0o600 });
  renameSync(partial, file);
  return mark;
}

/** The `--label` value that carries `mark` on its container. */
export function labelOf(mark: Mark): string {
  return `${markLabel}=${JSON.stringify(mark)}`;
}

/** The mark of the container `container` of a shell stack whose run directory is `run` and whose
 *  owner is `owner`, its driver: a container that serves on a port Docker publishes, with no socket
 *  directory, carried as its label alone (`e2e/postgres-mark.ts`). The run directory is held by
 *  its one spelling (`identityOf`), read here once, for both of the mark's paths: a driver spells
 *  it `${TMPDIR:-/tmp}/…`, with a doubled `/` where `TMPDIR` ends in one, which is no path a mark
 *  holds. Throws where `run` cannot be resolved. */
export function containerMark(owner: Owner | null, run: string, container: string): Mark {
  const identity = identityOf(run);
  return { owner, run: identity, identity, container };
}

/** The name `socketDirectory` gives a socket directory: `pg-`, the stack's index or `template`,
 *  `-`, and the six characters `mkdtemp` adds. */
const socketDirectoryName = /^pg-(?:\d+|template)-[A-Za-z0-9]{6}$/;
/** Where a binaries provider's data directory is in its run directory. */
const dataDirectory = /^(?:worker-\d+|template)\/pgdata$/;
/** A container name as Docker takes one. */
export const containerName = /^[A-Za-z0-9][A-Za-z0-9_.-]*$/;

/** Whether `path` is absolute, written with no `.`, `..`, doubled or trailing `/`, and not `/`. */
export function plainPath(path: unknown): path is string {
  return typeof path === "string" && isAbsolute(path) && normalize(path) === path && path !== "/";
}

/** Why another user could have put what is at `path`, or anything above it, where it is, or why a
 *  link leads through it, or undefined where no other user could and no link does. `lstat` reads
 *  each component from `/` down, following no link (`lstatSync`, or a test's own). Each component
 *  is a directory, and `path` itself one or a file as `kind` says; each belongs to root or the
 *  user `uid`, and `path` to that user; none above a directory that no other user may enter
 *  (one of `uid`'s with no permission for its group or others) may be written by its group or
 *  others, but for a directory with the sticky bit above `path`, whose entries only their owners
 *  may rename or remove. Where a component is gone there is nothing there to act on, and the
 *  answer is undefined too. */
export function whyUntrusted(
  path: string,
  uid: number,
  kind: "directory" | "file",
  lstat: (path: string) => Stats = lstatSync
): string | undefined {
  if (!plainPath(path)) return `${path} is not an absolute path written plain`;
  const names = path.split("/").slice(1);
  let closed = false;
  for (let depth = 0; depth <= names.length; depth++) {
    const component = depth === 0 ? "/" : `/${names.slice(0, depth).join("/")}`;
    const last = depth === names.length;
    let stat: Stats;
    try {
      stat = lstat(component);
    } catch (error) {
      if (codeOf(error) === "ENOENT") return undefined;
      return `${component} cannot be read (${codeOf(error) ?? firstLine(error)})`;
    }
    if (stat.isSymbolicLink()) return `${component} is a symbolic link`;
    const what = last ? kind : "directory";
    if (!(what === "file" ? stat.isFile() : stat.isDirectory())) {
      return `${component} is not a ${what}`;
    }
    if (stat.uid !== uid && (last || closed || stat.uid !== 0)) {
      return `${component} belongs to user ${stat.uid}`;
    }
    const othersWrite = (stat.mode & 0o022) !== 0;
    const sticky = (stat.mode & 0o1000) !== 0;
    if (!closed && othersWrite && (last || !sticky)) {
      return `${component} is writable by other users`;
    }
    if (stat.uid === uid && stat.isDirectory() && (stat.mode & 0o077) === 0) closed = true;
  }
  return undefined;
}

/** The text of the file `path`, opened with `O_NOFOLLOW`, so a link put in its place is not read
 *  through. */
export function readUnlinked(path: string): string {
  const descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    return readFileSync(descriptor, "utf8");
  } finally {
    closeSync(descriptor);
  }
}

/** `value` as an owner, null where it is null, or undefined where it is neither. */
function ownerOf(value: unknown): Owner | null | undefined {
  if (value === null) return null;
  if (typeof value !== "object" || value === undefined) return undefined;
  const { pid, start, boot, pidNamespace, timeNamespace, uid } = value as Record<string, unknown>;
  if (
    typeof pid !== "number" ||
    !Number.isSafeInteger(pid) ||
    pid <= 0 ||
    typeof start !== "string" ||
    !/^\d+$/.test(start) ||
    typeof boot !== "string" ||
    boot === "" ||
    typeof pidNamespace !== "string" ||
    pidNamespace === "" ||
    typeof timeNamespace !== "string" ||
    typeof uid !== "number" ||
    !Number.isSafeInteger(uid) ||
    uid < 0
  ) {
    return undefined;
  }
  return { pid, start, boot, pidNamespace, timeNamespace, uid };
}

/** `text` as a mark, or undefined where it is not one the providers write: JSON with an owner or
 *  null, a run directory and its one spelling, which share their last name, a socket directory in
 *  that run directory or in /tmp under the name `socketDirectory` gives one, and exactly one of a
 *  postmaster whose data directory is a stack's or the template's in that run directory, and a
 *  container name. A container of a shell stack names no socket directory; every postmaster names
 *  one. Every path is absolute and plain. */
export function markOf(text: string): Mark | undefined {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof value !== "object" || value === null) return undefined;
  const { owner, run, identity, sockets, postmaster, container } = value as Record<string, unknown>;
  const judged = ownerOf(owner);
  if (judged === undefined || !plainPath(run)) return undefined;
  if (!plainPath(identity) || basename(identity) !== basename(run)) return undefined;
  if ((postmaster === undefined) === (container === undefined)) return undefined;
  let checkedSockets: string | undefined;
  if (sockets !== undefined || postmaster !== undefined) {
    if (!plainPath(sockets)) return undefined;
    if (dirname(sockets) !== run && dirname(sockets) !== shortTemporaryDirectory) return undefined;
    if (!socketDirectoryName.test(basename(sockets))) return undefined;
    checkedSockets = sockets;
  }
  if (postmaster !== undefined) {
    if (typeof postmaster !== "object" || postmaster === null) return undefined;
    const { bin, data } = postmaster as Record<string, unknown>;
    if (!plainPath(bin) || !plainPath(data) || !dataDirectory.test(relative(run, data))) {
      return undefined;
    }
    return { owner: judged, run, identity, sockets: checkedSockets, postmaster: { bin, data } };
  }
  if (typeof container !== "string" || !containerName.test(container)) return undefined;
  return checkedSockets === undefined
    ? { owner: judged, run, identity, container }
    : { owner: judged, run, identity, sockets: checkedSockets, container };
}

/** The directories `mark` names as both sweeps act on them: under its run directory's one
 *  spelling (`identity`), and a socket directory in /tmp as the mark names it. */
export function markedPaths(mark: Mark): { readonly sockets?: string; readonly data?: string } {
  const sockets =
    mark.sockets === undefined || dirname(mark.sockets) !== mark.run
      ? mark.sockets
      : join(mark.identity, basename(mark.sockets));
  if (mark.postmaster === undefined) return { sockets };
  return { sockets, data: join(mark.identity, relative(mark.run, mark.postmaster.data)) };
}

/** What this process can tell of the owner of a mark: that it lives; that it is gone, with its
 *  pid and the user whose directories its Postgres left; or why it cannot tell. */
type Verdict =
  | { readonly kind: "lives" }
  | { readonly kind: "gone"; readonly pid: number; readonly uid: number }
  | { readonly kind: "unjudged"; readonly why: string };

/** Whether `owner`, the owner of a mark, still runs, as `self`, this process, can see it. */
export function judge(owner: Owner | null, self: Owner | null): Verdict {
  if (owner === null) return { kind: "unjudged", why: "its stack ran where there is no /proc" };
  if (self === null) return { kind: "unjudged", why: "this machine has no /proc" };
  if (owner.boot !== self.boot) return { kind: "unjudged", why: `boot ${owner.boot}` };
  if (owner.pidNamespace !== self.pidNamespace) {
    return { kind: "unjudged", why: `PID namespace ${owner.pidNamespace}` };
  }
  // The start the kernel shows is shifted by the reader's own time namespace, so the owner's
  // reading of its start and this process's are of one clock only in one time namespace.
  if (owner.timeNamespace !== self.timeNamespace) {
    return { kind: "unjudged", why: `time namespace ${owner.timeNamespace || "(none)"}` };
  }
  if (owner.uid !== self.uid) return { kind: "unjudged", why: `user ${owner.uid}` };
  const gone: Verdict = { kind: "gone", pid: owner.pid, uid: owner.uid };
  let stat: string;
  try {
    stat = readFileSync(`/proc/${owner.pid}/stat`, "utf8");
  } catch (error) {
    if (codeOf(error) === "ENOENT") return gone;
    return { kind: "unjudged", why: `its process could not be read: ${firstLine(error)}` };
  }
  return running(stat, owner.start) ? { kind: "lives" } : gone;
}

/** Whether `stat`, the text of a `/proc/<pid>/stat`, is of a process that started at `start` and
 *  is not a zombie: a process with the pid that started at another time took the pid over, and a
 *  zombie runs no more. */
export function running(stat: string, start: string): boolean {
  const fields = statFields(stat);
  return fields[22 - 3] === start && fields[0] !== "Z" && fields[0] !== "X";
}

/** Whether the process `pid` that started at `start` still runs, read from `/proc`. */
export function stillRuns(pid: number, start: string): boolean {
  try {
    return running(readFileSync(`/proc/${pid}/stat`, "utf8"), start);
  } catch (error) {
    if (codeOf(error) === "ENOENT") return false;
    throw error;
  }
}
