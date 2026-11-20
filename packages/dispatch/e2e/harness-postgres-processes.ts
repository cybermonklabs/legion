/** What the kernel shows of the processes of this user, as the sweep of
 *  `e2e/harness-postgres-removal.ts` reads them before it acts on what a dead stack's mark names:
 *  the postmaster that serves a data directory (`postmasterOf`), each process that still holds a
 *  directory (`holdersOf`), and each postmaster that serves from a run directory the sweep's own
 *  listing does not read (`servedElsewhere`). Every read is of `/proc` and of the paths it names;
 *  none signals, renames or removes anything. The paths that the processes name are read in a
 *  child, each within a bound (`identities`), since one on a mount whose server hangs never
 *  answers; the program that child runs is the caller's (`StatReader`), so a row can stand in one
 *  that never answers. */
import { spawn } from "node:child_process";
import { lstatSync, readdirSync, readFileSync, readlinkSync, realpathSync } from "node:fs";
import { basename, dirname, isAbsolute, join } from "node:path";

import {
  codeOf,
  readUnlinked,
  runDirectoryPrefix,
  running,
  startOf,
} from "./harness-postgres-marks";

/** A postmaster as `postmasterOf` verified it: its pid, and its start in clock ticks since boot. */
export interface Postmaster {
  readonly pid: number;
  readonly start: string;
}

/** The postmaster that serves the cluster in `data`, or undefined where none does, as the kernel
 *  shows it: the pid that `postmaster.pid` names, where the process with that pid has `data` for
 *  its working directory, which a postmaster's is from its start (Postgres's `ChangeToDataDir`),
 *  and its start, read before and after the checks, is the same both times. `data` is the one
 *  spelling a mark's paths are acted on under (`markedPaths`), with no link in it, which is how
 *  the kernel names a working directory. A `postmaster.pid` that a postmaster killed hard left,
 *  whose pid another process has since, names no postmaster here. Throws, naming the check, where
 *  that process runs as another user than `uid` or runs another program than `bin`'s `postgres`,
 *  and where it cannot tell. */
export function postmasterOf(data: string, bin: string, uid: number): Postmaster | undefined {
  let pidFile: string;
  try {
    pidFile = readUnlinked(join(data, "postmaster.pid"));
  } catch (error) {
    if (codeOf(error) === "ENOENT" || codeOf(error) === "ENOTDIR") return undefined;
    throw error;
  }
  const pid = Number.parseInt(pidFile, 10);
  if (!Number.isSafeInteger(pid) || pid <= 0) return undefined;
  const names = `the postmaster.pid of ${data} names pid ${pid}`;
  try {
    const before = startOf(readFileSync(`/proc/${pid}/stat`, "utf8"));
    if (readlinkSync(`/proc/${pid}/cwd`) !== data) return undefined;
    // The real, effective, saved and file system user of the process, in that order.
    const users = /^Uid:\s+(.+)$/m.exec(readFileSync(`/proc/${pid}/status`, "utf8"))?.[1];
    if (users?.split(/\s+/).some((user) => user !== String(uid)) !== false) {
      throw new Error(`${names}, which runs as user ${users ?? "unknown"}, not ${uid} alone`);
    }
    const program = readlinkSync(`/proc/${pid}/exe`);
    let postgres = join(bin, "postgres");
    try {
      postgres = realpathSync(postgres);
    } catch {
      // A postgres that is gone is the program of no process, so the comparison below fails.
    }
    if (program !== postgres) throw new Error(`${names}, which runs ${program}, not ${postgres}`);
    if (!running(readFileSync(`/proc/${pid}/stat`, "utf8"), before)) return undefined;
    return { pid, start: before };
  } catch (error) {
    if (codeOf(error) === "ENOENT") return undefined;
    throw error;
  }
}

/** Whether `argument`, one argument of a process, names `path` whole: after its start or a space,
 *  a `=`, a `:`, a `,` or a quote, and before its end, a `/` or one of those, as `postgres -D
 *  <data>`, `pg_ctl`'s `-o "-k <sockets>"` and Docker's `--mount type=bind,source=<sockets>,…`
 *  write it. */
function names(argument: string, path: string): boolean {
  for (let at = argument.indexOf(path); at !== -1; at = argument.indexOf(path, at + 1)) {
    const before = argument[at - 1] ?? "";
    const after = argument[at + path.length] ?? "";
    if (/^[\s=:,'"]?$/.test(before) && /^[\s=:,'"/]?$/.test(after)) return true;
  }
  return false;
}

/** The paths that `argument`, one argument of a process, can name, each as the process spells it:
 *  each piece between its spaces, `=`, `:`, `,` and quotes, as `postgres -D <data>`, `pg_ctl`'s
 *  `-o "-k <sockets>"` and Docker's `--mount type=bind,source=<sockets>,…` write it, but an
 *  option, and the value an option of one letter holds glued to it, as `-D<data>` writes it. */
function pathsIn(argument: string): string[] {
  return argument.split(/[\s=:,'"]+/).flatMap((piece) => {
    if (!piece.startsWith("-")) return piece === "" ? [] : [piece];
    const glued = /^-[A-Za-z](.+)$/.exec(piece)?.[1];
    return glued === undefined ? [] : [glued];
  });
}

/** How long one path that a process names may go unanswered while a start reads whether it leads
 *  into a directory (`holdersOf`): a stat of a path on a local file system answers within
 *  microseconds, a read of 13,244 paths took under 0.5 s at a load of 270 on a shared Linux machine
 *  on 2026-11-19, and a stat of a path on a network or FUSE mount whose server hangs never answers,
 *  and would hold the start in the kernel. Past this the start cannot judge a process that names
 *  that path, or a path under it, and leaves the directory: since each directory is read before
 *  the paths under it, and none under one that did not answer is asked, a mount that hangs costs
 *  this bound once, however many of its paths the processes name (`identities`). */
export const statMs = 10_000;
/** How long the child that reads those paths (`StatReader`) may take to start: a `bun` child took
 *  more than 180 s once to start its sweep on a shared Linux machine under a load of 1,217, on
 *  2026-11-17. */
export const statStartMs = 240_000;

/** The script of the start's own reader of where each path leads (`statReader`): it reads the
 *  paths from its standard input, separated by NUL, says it is ready, and then writes one line for
 *  each path in turn as soon as it has read it: its device and inode, or `-` where it names
 *  nothing. */
const statScript = `
const { readFileSync, statSync, writeSync } = require("node:fs");
const paths = readFileSync(0, "utf8").split("\\0");
writeSync(1, "ready\\n");
for (const path of paths) {
  let line = "-";
  try {
    const stats = statSync(path, { bigint: true });
    line = stats.dev + ":" + stats.ino;
  } catch {}
  writeSync(1, line + "\\n");
}
`;

/** How a start reads where each path a process names leads: the program that reads, with its
 *  arguments, run as a child of the start, which reads and writes as `statScript` does (`run`);
 *  the mount table, a file in the format of `/proc/self/mountinfo`, by which the start puts the
 *  paths into the groups it reads at the same time (`mounts`, `topOf`); and how long one path may
 *  go unanswered (`statMs`). A start runs `statReader`; a row runs a stand-in program that never
 *  answers one path, as a stat on a mount whose server hangs never does, beside a mount table that
 *  says that path is on such a mount, to see the start's bound on it (`identities`), and sets that
 *  bound low, a few seconds, to see it end in that time and not in the start's own. */
export interface StatReader {
  readonly run: readonly [program: string, ...args: string[]];
  readonly mounts: string;
  readonly statMs: number;
}

/** The start's own `StatReader`: the runtime of this process, Bun or Node, with `statScript`, the
 *  kernel's mount table of this process, and the start's own bound on one path (`statMs`). */
export const statReader: StatReader = {
  run: [process.execPath, "-e", statScript],
  mounts: "/proc/self/mountinfo",
  statMs,
};

/** What this module's own lines are prefixed with, as every starter in `e2e/` names itself. */
const named = "e2e/harness-postgres-processes.ts";

/** Each path that answered no stat within the bound of the reader that read it (`StatReader`) in
 *  this process, with that bound: it is not asked again, and no path under it is, since a mount
 *  that hangs holds each later stat of it, and of what is on it. */
const unanswered = new Map<string, number>();

/** Whether `path` is one that answered no stat in this process, or is under one (`unanswered`). */
function underUnanswered(path: string): boolean {
  for (const hung of unanswered.keys())
    if (path === hung || path.startsWith(`${hung}/`)) return true;
  return false;
}

/** What one child (`StatReader`) read of the paths it was given, in their order: whether it
 *  started, and where each path it read leads, "<dev>:<ino>", or null where it names nothing. */
interface ChildRead {
  readonly started: boolean;
  readonly read: readonly (string | null)[];
}

/** What one child that runs `reader` read of `paths` before it ended or was ended (`ChildRead`).
 *  It is ended where it has not started within `statStartMs`, or has read no next path within
 *  the reader's bound (`StatReader.statMs`), and nothing waits for a child that the kernel still
 *  holds after SIGKILL. The child is told which read of this process, and which of its top
 *  paths, it serves (`HOLDER_READ`, `read` and `top` of `identities`), which the start's own
 *  reader ignores and a row's count of reads reads. */
function statInChild(paths: readonly string[], reader: StatReader, of: string): Promise<ChildRead> {
  const { promise, resolve } = Promise.withResolvers<ChildRead>();
  const [program, ...args] = reader.run;
  const child = spawn(program, args, {
    stdio: ["pipe", "pipe", "ignore"],
    env: { ...process.env, HOLDER_READ: of },
  });
  const read: (string | null)[] = [];
  let started = false;
  let pending = "";
  let done = false;
  let timer: Timer | undefined;
  const end = (): void => {
    if (done) return;
    done = true;
    clearTimeout(timer);
    child.kill("SIGKILL");
    child.stdin?.destroy();
    child.stdout?.destroy();
    child.unref();
    resolve({ started, read });
  };
  timer = setTimeout(end, statStartMs);
  child.stdout?.setEncoding("utf8");
  child.stdout?.on("data", (chunk: string) => {
    pending += chunk;
    for (let at = pending.indexOf("\n"); at !== -1; at = pending.indexOf("\n")) {
      const line = pending.slice(0, at);
      pending = pending.slice(at + 1);
      if (started) read.push(line === "-" ? null : line);
      else started = line === "ready";
      clearTimeout(timer);
      timer = setTimeout(end, reader.statMs);
    }
    if (read.length === paths.length) end();
  });
  child.on("error", end);
  child.on("close", end);
  // A child that ends before it read all of its input closes its end of the pipe.
  child.stdin?.on("error", () => undefined);
  child.stdin?.end(paths.join("\0"));
  return promise;
}

/** One mount of a mount table (`StatReader.mounts`): its mount point, with the octal escapes of its
 *  spaces and tabs read, and its filesystem type, or undefined where the line holds none. */
interface Mount {
  readonly point: string;
  readonly type: string | undefined;
}

/** The mounts of the table at `file`, a file in the format of `/proc/self/mountinfo` ("ID PARENT
 *  MAJOR:MINOR ROOT POINT OPTIONS [FIELDS...] - TYPE SOURCE OPTIONS"); none where it cannot be
 *  read. */
function mountsOf(file: string): Mount[] {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch {
    return [];
  }
  return text.split("\n").flatMap((line) => {
    const words = line.split(" ");
    const point = words[4];
    if (point === undefined || point === "") return [];
    const dash = words.indexOf("-", 6);
    return [
      {
        point: point.replace(/\\([0-7]{3})/g, (_, code: string) =>
          String.fromCharCode(Number.parseInt(code, 8))
        ),
        type: dash === -1 ? undefined : words[dash + 1],
      },
    ];
  });
}

/** The filesystems whose `stat` the kernel answers itself, from a disk the machine holds or from its
 *  own memory, with no server or daemon that can hang: a closed list, each type with its reason. A
 *  mount of any other type, a network or FUSE mount (`nfs`, `cifs`, `fuse.*`, `9p`, `ceph`,
 *  `virtiofs`), an automount (`autofs`, whose read can start a mount that hangs, as it asks a
 *  daemon), or one whose type the table does not give, is read by a reader of its own (`topOf`): a
 *  type missing here costs one reader more, never a path left unread. */
const kernelAnswered: Record<string, true> = {
  // A filesystem on a disk of the machine: a stat reads that block device, and asks no server.
  ext2: true,
  ext3: true,
  ext4: true,
  xfs: true,
  btrfs: true,
  vfat: true,
  // A read-only image on a disk or in a file of the machine.
  squashfs: true,
  // Files in the machine's memory.
  tmpfs: true,
  ramfs: true,
  // A container's root: layers that are directories of the machine's own filesystems.
  overlay: true,
  // The kernel's processes and objects, answered from its memory.
  proc: true,
  sysfs: true,
  // Device nodes in memory: a stat of a node opens no device.
  devtmpfs: true,
  // The pseudo-terminals, in memory.
  devpts: true,
  // The control groups, in memory.
  cgroup: true,
  cgroup2: true,
  // POSIX message queues, in memory.
  mqueue: true,
  // Files of huge pages, in memory.
  hugetlbfs: true,
  // The security modules' files, which the kernel answers.
  securityfs: true,
  // The kernel's debug files, in memory.
  debugfs: true,
  // The kernel's tracing files, in memory.
  tracefs: true,
  // The records of the kernel's last crash, which it read when it mounted them.
  pstore: true,
  // Pinned BPF objects, in memory.
  bpf: true,
  // Kernel objects that a user makes, in memory.
  configfs: true,
  // The control files of FUSE connections, which the kernel answers, not a FUSE server.
  fusectl: true,
  // The firmware's variables, read through the kernel.
  efivarfs: true,
  // Handles of namespaces, in memory.
  nsfs: true,
  // The kernel's table of binary formats, in memory. A box mounts it on `/proc/sys/fs/binfmt_misc`
  // over that point's automount, and the last mount of a point is the one a stat reaches (`topOf`).
  binfmt_misc: true,
};

/** The top path of the paths a start reads on the filesystems the kernel answers itself
 *  (`kernelAnswered`): one reader reads them all. */
const local = "the machine's own filesystems";

/** The top path `path` is read under (`identities`): `local` where the deepest mount it is on is of
 *  a type the kernel answers itself; that mount's point where it is of another type or of no type
 *  the table gives; and, where the table names no mount of it, as where it cannot be read, its first
 *  component, `/` alone for `/`. Two mounts that may hang are two top paths, and one that hangs
 *  holds the stat of no other. `mounts` holds each mount by its point, the last the table gives for
 *  a point, which is mounted over those before it; the path and each directory above it are looked
 *  up there in turn, the deepest first. */
function topOf(path: string, mounts: ReadonlyMap<string, Mount>): string {
  for (let at = path; ; at = dirname(at)) {
    const on = mounts.get(at);
    if (on !== undefined) {
      return on.type !== undefined && kernelAnswered[on.type] === true ? local : on.point;
    }
    if (dirname(at) === at) return /^\/[^/]+/.exec(path)?.[0] ?? path;
  }
}

/** How many reads of where paths lead this process has made, which numbers each (`identities`). */
let reads = 0;

/** Each path a holder read of this process did not reach, since the read of its group stopped at
 *  a path before it that answered no stat (`identities`), with that path. */
const notReached = new Map<string, string>();

/** At most how many of the paths that read left unread a line names, before it counts the rest. */
const namedLeft = 5;

/** Where each of `paths` leads, as children of this process that run `reader` read them
 *  (`statInChild`): its device and inode, "<dev>:<ino>", or null where it names nothing. The paths
 *  are grouped by the mount they are on (`topOf`, by the mount table `reader.mounts`): the paths of
 *  the filesystems the kernel answers itself in one group, and those of each other mount in one of
 *  its own. Each group is read by a child of its own, and the children of the groups are started
 *  at the same time, so mounts that hang run out their bounds together: two that hang cost one
 *  bound, not two, and a path of a mount that answers is read at once. Within a group each
 *  directory is read before the paths under it. A path that answered no stat within the reader's
 *  bound (`StatReader.statMs`), or is under one, here or earlier in this process (`unanswered`),
 *  has no entry and is not asked: where a child waits on a path past that bound, the path joins
 *  `unanswered`, and a line names it at once, with `directory`, whose holders the read is for. The
 *  read of that group stops there, whichever mount it is of, and leaves the paths after it unread
 *  (`notReached`), and the line names them. A child that did not start leaves the paths it was
 *  given without an entry, and they are asked again at the next read. */
async function identities(
  paths: readonly string[],
  reader: StatReader,
  directory: string
): Promise<Map<string, string | null>> {
  const found = new Map<string, string | null>();
  // How deep a path is, its count of `/`, read once for each path: a read sorts about 13,000.
  const depths = new Map<string, number>();
  const depth = (path: string): number => {
    let count = depths.get(path);
    if (count === undefined) {
      count = 0;
      for (let at = path.indexOf("/"); at !== -1; at = path.indexOf("/", at + 1)) count += 1;
      depths.set(path, count);
    }
    return count;
  };
  const mounts = new Map(mountsOf(reader.mounts).map((mount) => [mount.point, mount]));
  const tops = new Map<string, string[]>();
  for (const path of paths) {
    if (underUnanswered(path)) continue;
    const top = topOf(path, mounts);
    const those = tops.get(top);
    if (those === undefined) tops.set(top, [path]);
    else those.push(path);
  }
  reads += 1;
  const read = reads;
  notReached.clear();
  await Promise.all(
    [...tops].map(async ([top, those]) => {
      const sorted = those.sort(
        (first, second) =>
          depth(first) - depth(second) || (first < second ? -1 : first > second ? 1 : 0)
      );
      const { started, read: answers } = await statInChild(sorted, reader, `${read} ${top}`);
      for (const [at, identity] of answers.entries()) found.set(sorted[at] ?? "", identity);
      const hung = sorted[answers.length];
      if (!started || hung === undefined) return;
      unanswered.set(hung, reader.statMs);
      const after = sorted.slice(answers.length + 1).filter((path) => !underUnanswered(path));
      // The read of this group stops here, whichever mount it is of, and leaves the paths after
      // the hung one unread (`notReached`), which the line names: a group that read on would
      // start a second reader, and wait a second bound, which a row that counts one holder read
      // of this group has not counted. The `dispatch-ui` job and the harness lane's rounds 1 and
      // 2 of run 44647294777 met that at `c07d6906ed5`, where a path answered no stat under load
      // and its group read on: "the row's limit counts 1 holder reads, and its sweeps made 2". A
      // path left unread leaves its process unjudged (`whyUnread`), so the start leaves the
      // directory for a later one, which is what an unanswered path already did.
      for (const path of after) notReached.set(path, hung);
      const left =
        after.length === 0
          ? ""
          : `, and reads none of the ${after.length} paths of ${top === local ? "the machine's own filesystems" : top} after it: ${after.slice(0, namedLeft).join(", ")}${after.length > namedLeft ? ` and ${after.length - namedLeft} more` : ""}`;
      console.error(
        `${named}: the read of what holds ${directory} waits no longer on ${hung}, which answered no stat within ${reader.statMs / 1000} s, nor asks a path under it${left}: a process that names one may hold ${directory}`
      );
    })
  );
  return found;
}

/** A process a holder read names: its program, the name of the file it runs, and its pid. The
 *  start waits for it by its pid (`removePostmaster` of `e2e/harness-postgres-removal.ts`); a line
 *  names it as `processName` writes it. */
export interface HolderProcess {
  readonly program: string;
  readonly pid: number;
}

/** How a line names `process`: "<program> <pid>". */
export function processName({ program, pid }: HolderProcess): string {
  return `${program} ${pid}`;
}

/** What `holdersOf` read of a directory: each process that holds it, and each process it cannot
 *  judge, since a path that process names (`path`) has no answer of the read, with why
 *  (`whyUnread`). */
export interface Holding {
  readonly holders: readonly HolderProcess[];
  readonly unread: readonly {
    readonly process: HolderProcess;
    readonly path: string;
    readonly why: string;
  }[];
}

/** Why `path` has no answer of a holder read, as a line says it: it answered no stat within the
 *  bound of the reader that read it, or a directory above it did (`unanswered`); the read of its
 *  group stopped at a path before it (`notReached`), which answered no stat within that bound; or
 *  the reader that was to read it did not start within `statStartMs`. */
function whyUnread(path: string): string {
  const hung = (at: string): string =>
    `answered no stat within ${(unanswered.get(at) ?? Number.NaN) / 1000} s`;
  for (let at = path; ; at = dirname(at)) {
    if (unanswered.has(at))
      return at === path ? `which ${hung(at)}` : `under ${at}, which ${hung(at)}`;
    const before = notReached.get(at);
    if (before !== undefined)
      return `which the read did not reach, as ${before}, before it, ${hung(before)}`;
    if (dirname(at) === at) {
      return `which no reader read, as the reader did not start within ${statStartMs / 1000} s`;
    }
  }
}

/** `paths`, each path that the process `pid` names, but those in a directory that process claimed
 *  itself (`claimInfix`): a start that claimed a dead stack's directory holds descriptors under the
 *  claim's name while it removes it. A start whose holder read looked at that directory before the
 *  claim, and read what the processes name after it, finds those descriptors leading into the
 *  directory: in 6 of 80 runs of two starts at once on one dead mark, on a shared Linux machine on
 *  2026-11-19, the second start named the first as a process that still held the directory, and
 *  left its mark. That is the other start's removal, which holds nothing the stack started. A claim
 *  holds its claimer's own pid and start, so no path of another process is left out, and no path of
 *  the claimer's anywhere else. */
function outsideOwnClaims(pid: string, paths: readonly string[]): string[] {
  if (!paths.some((path) => path.includes(claimInfix))) return [...paths];
  const own = `${claimInfix}${pid}-${startOf(readFileSync(`/proc/${pid}/stat`, "utf8"))}`;
  return paths.filter((path) => !path.split("/").some((part) => part.endsWith(own)));
}

/** Each process of the user `uid` that still holds `directory`, a directory of a dead stack's
 *  mark, as the kernel shows it: its working directory, a file it holds open, or a path one of its
 *  arguments names (`pathsIn`), read from its working directory where it is relative, is that
 *  directory or in it. A holder is judged by the directory itself, its device and its inode, and
 *  by no spelling of it: a `pg_ctl` that a stack started under a TMPDIR spelt through a link, with
 *  a `-D` relative to where it runs or with a doubled `/`, holds it all the same. `spellings`, the
 *  paths the mark names it by, are read in the arguments as they are written too (`names`), for a
 *  spelling that no longer leads to it, such as a link removed since. A process the stack started
 *  that its death did not end holds a directory so: a `pg_ctl start` or an `initdb` still running,
 *  a `docker create` that mounts the socket directory, the postmaster such a `pg_ctl` is starting.
 *  A stack is not dead while one of them runs, so a start removes no directory it holds.
 *  `postmaster` is the pid of the postmaster `postmasterOf` verified, which this start stops
 *  itself, and neither it nor a process it started (its parent pid) is counted; nor is another
 *  start by its paths under its own claim (`outsideOwnClaims`), which are its removal. A process
 *  that ends while it is read, or whose links this user may not read, holds nothing here, and
 *  nothing holds a directory that is gone. `/proc` is read here; where each path leads is read in
 *  a child that runs `reader` (`StatReader`), all of them within its bound (`identities`), and a
 *  process none of whose paths leads into the directory, but one of whose paths has no answer, is
 *  one the start cannot judge (`unread`). Answers each holder and each process it cannot judge,
 *  in pid order (`Holding`). */
export async function holdersOf(
  directory: string,
  spellings: readonly string[],
  uid: number,
  reader: StatReader,
  postmaster?: number
): Promise<Holding> {
  let target: string;
  try {
    const stats = lstatSync(directory, { bigint: true });
    target = `${stats.dev}:${stats.ino}`;
  } catch {
    // A directory gone, or one whose identity this user cannot read, has no holder to read here;
    // the rename or the removal that follows says what it is.
    return { holders: [], unread: [] };
  }
  // What each process names, from `/proc` alone: whether an argument spells the directory as the
  // mark does, and each path it names, its working directory first.
  const named: {
    readonly process: HolderProcess;
    readonly spelt: boolean;
    readonly paths: string[];
  }[] = [];
  for (const pid of readdirSync("/proc").filter((entry) => /^\d+$/.test(entry))) {
    try {
      if (Number(pid) === postmaster || lstatSync(`/proc/${pid}`).uid !== uid) continue;
      const parent = /^PPid:\s+(\d+)$/m.exec(readFileSync(`/proc/${pid}/status`, "utf8"))?.[1];
      if (postmaster !== undefined && Number(parent) === postmaster) continue;
      const cwd = readlinkSync(`/proc/${pid}/cwd`);
      const args = readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0");
      const open = readdirSync(`/proc/${pid}/fd`).flatMap((fd) => {
        try {
          const path = readlinkSync(`/proc/${pid}/fd/${fd}`);
          return isAbsolute(path) ? [path] : [];
        } catch {
          return [];
        }
      });
      named.push({
        process: { program: basename(readlinkSync(`/proc/${pid}/exe`)), pid: Number(pid) },
        spelt: args.some((argument) => spellings.some((path) => names(argument, path))),
        paths: outsideOwnClaims(pid, [
          cwd,
          ...args.flatMap(pathsIn).map((path) => (isAbsolute(path) ? path : `${cwd}/${path}`)),
          ...open,
        ]),
      });
    } catch {
      // A process that ended, or whose links this user may not read, holds nothing here.
    }
  }
  // Each path and each directory above it, read in one child: a path that names nothing, such as
  // a file a process will write, is in its parent.
  const every = new Set<string>();
  for (const { paths } of named) {
    for (let path of paths) {
      while (!every.has(path)) {
        every.add(path);
        const parent = dirname(path);
        if (parent === path) break;
        path = parent;
      }
    }
  }
  const leads = await identities([...every], reader, directory);
  // Whether `path` is `directory` or in it, or undefined where a stat of it or of a directory
  // above it did not answer and none above it is the directory.
  const known = new Map<string, boolean | undefined>();
  const inside = (path: string): boolean | undefined => {
    if (known.has(path)) return known.get(path);
    const parent = dirname(path);
    const above = parent === path ? false : inside(parent);
    const self = leads.has(path) ? leads.get(path) === target : undefined;
    const answer =
      self === true || above === true
        ? true
        : self === false && above === false
          ? false
          : undefined;
    known.set(path, answer);
    return answer;
  };
  const holders: HolderProcess[] = [];
  const unread: { process: HolderProcess; path: string; why: string }[] = [];
  for (const { process, spelt, paths } of named) {
    const judged = paths.map(inside);
    if (spelt || judged.includes(true)) {
      holders.push(process);
      continue;
    }
    const path = paths[judged.indexOf(undefined)];
    if (path !== undefined) unread.push({ process, path, why: whyUnread(path) });
  }
  return { holders, unread };
}

/** How a postmaster's working directory, its data directory, names the run directory it serves
 *  from: the run directory, then its stack's place in it. A run directory someone renamed after
 *  its stack started has another name than the pool gives one, and is found all the same. A data
 *  directory a start claimed (`claimForRemoval` of `e2e/harness-postgres-removal.ts`) and that
 *  start's death left claimed has the claim's name, `pgdata.removing-<pid>-<start>`, which the
 *  kernel shows as the postmaster's working directory, and is found under the name its mark
 *  holds, without the claim. */
const servedFrom = /^(.+)\/(?:worker-\d+|template)\/pgdata(?:\.removing-\d+-\d+)?$/;
/** What a claimed directory's name holds between the name of the part and the claimer's pid and
 *  start: a start claims a part for its removal by renaming it to `<part>.removing-<pid>-<start>`
 *  (`claimForRemoval` of `e2e/harness-postgres-removal.ts`). */
export const claimInfix = ".removing-";
/** The claim a claimed data directory's name ends in (`claimInfix`). */
const claimed = /\.removing-\d+-\d+$/;

/** A postmaster the kernel shows serving from a run directory that this start's listing of its
 *  TMPDIR does not read, with its pid and its start as the listing read them. */
export interface Served extends Postmaster {
  readonly run: string;
  readonly data: string;
}

/** Each postmaster of the user `uid` that serves from a run directory that the listing of `real`,
 *  this start's TMPDIR, does not read, as the kernel shows it: a process of that user (the owner of
 *  `/proc/<pid>`) whose program is a `postgres`, whose working directory has the shape of a
 *  stack's data directory in a run directory (`servedFrom`), and which that directory's
 *  `postmaster.pid` names, as it names a postmaster and none of its children. That file holds
 *  the pid the postmaster's own PID namespace gives it, the last of its `NSpid`, which is its pid
 *  here where it runs in this start's PID namespace. The run directories of `real` that the pool
 *  named are the listing's; one of another name there, a run directory renamed after its stack
 *  started, is found here. That shape only finds a candidate: the mark in that run directory, its
 *  owner and the postmaster's own `/proc` facts decide (`sweepRun`). Only a run directory in
 *  `under` is listed: `/`, every one, for a start (the default of `removeLeftoverPostgres`); a
 *  row's own directory for a row, so that the sweep of one row judges no stack that another row,
 *  or another session of the machine, planted beside it. A process that ends while it is read is
 *  not listed. */
export function servedElsewhere(real: string, uid: number, under: string): Served[] {
  const within = under.endsWith("/") ? under : `${under}/`;
  let pids: string[];
  try {
    pids = readdirSync("/proc").filter((entry) => /^\d+$/.test(entry));
  } catch {
    return [];
  }
  const served: Served[] = [];
  for (const pid of pids) {
    try {
      if (lstatSync(`/proc/${pid}`).uid !== uid) continue;
      const start = startOf(readFileSync(`/proc/${pid}/stat`, "utf8"));
      if (basename(readlinkSync(`/proc/${pid}/exe`)) !== "postgres") continue;
      const data = readlinkSync(`/proc/${pid}/cwd`);
      const run = servedFrom.exec(data)?.[1];
      if (run === undefined || !run.startsWith(within)) continue;
      if (dirname(run) === real && basename(run).startsWith(runDirectoryPrefix)) continue;
      const status = readFileSync(`/proc/${pid}/status`, "utf8");
      const own = /^NSpid:.*?(\d+)[ \t]*$/m.exec(status)?.[1] ?? pid;
      if (Number.parseInt(readUnlinked(join(data, "postmaster.pid")), 10) === Number(own)) {
        served.push({ run, data: data.replace(claimed, ""), pid: Number(pid), start });
      }
    } catch {
      // A process that ended, or whose links this user may not read, serves from nothing here.
    }
  }
  return served.sort((first, second) => first.data.localeCompare(second.data));
}
