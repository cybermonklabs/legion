/** The sweep with which the next start of a stack removes each Postgres whose mark
 *  (`e2e/harness-postgres-marks.ts`) names a process that is gone.
 *
 *  A stack killed hard (SIGKILL) runs no stop, so its Postgres outlives it: the binaries
 *  provider's postmaster is a daemon of `pg_ctl`, the image provider's container runs detached,
 *  and neither the stops of `e2e/harness-postgres.ts` nor the pool's teardown
 *  (`e2e/harness-pool.ts`) runs. So each start of a stack (`startStacks`) first removes every
 *  Postgres whose mark names a process that is gone.
 *
 *  WHERE A START LOOKS. Three places: each run directory of its own `tmpdir()`; each container
 *  Docker lists with the label; and each other run directory that the kernel shows a postmaster of
 *  this user serving from (`servedElsewhere`), which is how most leftovers are found on a machine
 *  where each session has a `TMPDIR` of its own, and how a run directory someone renamed, or the
 *  stack of another PID namespace, is found. A run directory of another `TMPDIR` whose postmaster
 *  has already ended holds no running server, and no start finds it.
 *
 *  WHAT A START REMOVES. Exactly what a mark of a gone owner names, and only where every check
 *  above held: the container by the id under which Docker lists its label; the postmaster, by the
 *  pid that the start verified itself; and then the data and socket directories the mark names,
 *  each of them in the run directory or, for a socket directory, in `/tmp`. A name only finds a
 *  candidate (a run directory's prefix, a container's label key); the mark decides. A mark whose
 *  paths are not the shapes the providers make is not read. A start runs no program that a mark
 *  names: the mark's `bin` is only compared with the program a process runs. The postmaster is
 *  the pid of its `postmaster.pid`, where the kernel shows that process running as this user,
 *  with that data directory for its working directory and the mark's `postgres` for its program,
 *  with the same start before and after those reads; a start sends it SIGQUIT, the immediate
 *  shutdown that `pg_ctl -m immediate stop` asks for, and SIGKILL where it still runs a while
 *  later, each only while `/proc` names that process by its pid and its start. A pid that a stale
 *  file names and another process now has, with another working directory, gets no signal, and
 *  only the directories go; one whose working directory is that data directory but which is not
 *  this user's or runs another program is left, since a directory is never removed under a
 *  process. A data directory goes only once its postmaster is gone. A container's socket
 *  directory and its mark file go only with the container or after it: the container sweep
 *  removes a container and then its socket directory, and the run directory sweep removes them
 *  for a container Docker no longer lists, never under one that it still lists. The rest of a
 *  dead run's directory, its logs, stays, as a run that keeps its logs leaves them
 *  (`e2e/harness-pool.ts`).
 *
 *  WHAT A START LEAVES, AND SAYS SO. A mark whose owner lives is left without a word. Every other
 *  mark it leaves is named on one line per kind: what it cannot read (another user's run
 *  directory of mode 0700, a file that is not a mark, a label that is the mark of another
 *  container, a `docker ps` that failed), what it does not trust (a check of `whyUntrusted`,
 *  named), what it cannot judge (another boot, PID namespace, time namespace or user, or no
 *  `/proc`), and what a process of this user still holds.
 *  A STACK IS NOT DEAD WHILE A PROCESS IT STARTED HOLDS ITS DIRECTORIES. A stack killed hard while
 *  its `pg_ctl start`, its `initdb` or its `docker create` runs leaves that child running, and
 *  the postmaster such a `pg_ctl` starts. So before a start claims a data or a socket directory,
 *  it reads every process of this user that the kernel shows (`holdersOf`, of
 *  `e2e/harness-postgres-processes.ts`): one whose working directory, an open file or a path one
 *  of its arguments names is that directory or in it holds it, judged by the directory's device
 *  and inode and not by any spelling of it, and the start leaves that directory under its name
 *  with its mark and names it, and a later start judges it again. The postmaster the start
 *  verified, and the processes it started, are not counted: the start stops them itself, and
 *  waits for them to end before the removal. Nor is another start that claimed the directory and
 *  removes it, by what it holds under its claim: that is its removal (`holdersOf`).
 *  A line that says `removed` names only what that start itself removed and is gone. A removal
 *  that fails, or a postmaster that fails a check, is one line, keeps its mark for the next
 *  start, and never fails the start. Two starts that find one dead mark at once both try, and the
 *  line of each says what it did. A directory, and the postmaster that serves from a data
 *  directory, are claimed before a start acts on them (`claimForRemoval`): renamed to a name that
 *  holds the claimer's pid and start, so one start wins, signals the postmaster and removes the
 *  directory, and the other says that another start removed it first; a claim whose claimer died
 *  before its removal the next start claims again. The rename is the claim because one of two
 *  renames wins and a start killed after it leaves nothing else behind, and a start renames only a
 *  directory no process holds. A container is claimed by Docker's own removal, and a removal of
 *  one takes as long as Docker takes over it, within a bound, as
 *  `e2e/harness-postgres-container-removal.ts` says (`removeContainer`); at the bound the sweep
 *  names the container and goes on. A container whose remover died is gone, or still listed with
 *  its label for the next start.
 *
 *  THE WINDOWS NO MARK COVERS. A stack killed between the `mkdtemp` of a socket directory and the
 *  rename of its mark, calls with no await between them, leaves that empty directory and at most
 *  a `.partial` file of the mark, which no start reads. A pid that a dead postmaster leaves can be
 *  taken by another process between the checks of that process and the signal, which only pid
 *  reuse within those milliseconds allows. A child a stack forked in the moment before it was
 *  killed, and that has not run its program yet, names none of the stack's directories, and a
 *  start in that moment does not see it. */
import { execFile } from "node:child_process";
import { lstatSync, readdirSync, readFileSync, renameSync, rmSync, type Stats } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { promisify } from "node:util";

import {
  dockerMs,
  type Machine,
  machine,
  type RemovalBounds,
  removalBounds,
  removeContainer,
} from "./harness-postgres-container-removal";
import {
  codeOf,
  firstLine,
  identityOf,
  judge,
  type Mark,
  markedPaths,
  markLabel,
  markOf,
  marksDirectory,
  type Owner,
  readUnlinked,
  runDirectoryPrefix,
  stackOwner,
  startOf,
  stillRuns,
  whyUntrusted,
} from "./harness-postgres-marks";
import {
  claimInfix,
  type Holding,
  holdersOf,
  type Postmaster,
  postmasterOf,
  processName,
  type StatReader,
  servedElsewhere,
  statReader,
} from "./harness-postgres-processes";

const execFileAsync = promisify(execFile);

/** What this module's own lines are prefixed with, as every starter in `e2e/` names itself. */
const named = "e2e/harness-postgres-removal.ts";

/** How long a dead stack's postmaster may take to end after SIGQUIT: as long as `pg_ctl`'s own
 *  wait for a stop, `PGCTLTIMEOUT`, 60 s by default. A postmaster that gets it ends its children,
 *  kills one that has not ended within 5 s, and exits. */
const postmasterQuitMs = 60_000;
/** How long it may take to end after SIGKILL, which ends it the next time it runs: past this the
 *  kernel holds it, and the start goes on with one line. */
const postmasterKillMs = 10_000;
/** How often a start reads `/proc` while it waits for a postmaster to end. */
const postmasterPollMs = 50;
/** How long the processes of a postmaster this start stopped may take to end after it: the 5 s a
 *  postmaster that got SIGQUIT gives its children before it kills them. */
const postmasterChildrenMs = 5_000;
/** The longest one stop of a postmaster can take a start: the wait after SIGQUIT, the wait after
 *  SIGKILL and the wait for its processes after it. A row's own limit counts it for each stop
 *  that row's sweep makes (`Waits` of `e2e/__tests__/harness-postgres-waits.ts`). */
export const stopMs = postmasterQuitMs + postmasterKillMs + postmasterChildrenMs;

/** The data directory of each postmaster that still serves among the marks of the run in `run`:
 *  a stop that failed left it running. Each is read under the mark's one spelling (`markedPaths`),
 *  as a start's sweep reads it. A mark that cannot be read, and a postmaster that cannot be told,
 *  count as serving, since removing the directory under a server is the fault. */
export function servingPostmasters(run: string): string[] {
  let names: string[];
  try {
    names = readdirSync(join(run, marksDirectory));
  } catch (error) {
    if (codeOf(error) === "ENOENT") return [];
    return [`${join(run, marksDirectory)} (${firstLine(error)})`];
  }
  const serving: string[] = [];
  for (const name of names.filter((entry) => entry.endsWith(".json"))) {
    try {
      const mark = markOf(readFileSync(join(run, marksDirectory, name), "utf8"));
      const { data } = mark === undefined ? {} : markedPaths(mark);
      if (
        mark?.postmaster !== undefined &&
        data !== undefined &&
        postmasterOf(data, mark.postmaster.bin, process.getuid?.() ?? -1) !== undefined
      ) {
        serving.push(data);
      }
    } catch (error) {
      serving.push(`${join(run, marksDirectory, name)} (${firstLine(error)})`);
    }
  }
  return serving;
}

/** What `removeDirectory` did: removed the directory; found it gone, with no claim of it, when it
 *  looked; found it claimed by another start, which removed it first; or left it, since a process
 *  of this user still holds it, or may hold it, as a path it names did not answer (`holdersOf`),
 *  named. */
type Removal = "removed" | "absent" | "lost" | { readonly heldBy: Holding };

/** Whether a process holds the directory `holdersOf` read, or may hold it. */
function held({ holders, unread }: Holding): boolean {
  return holders.length > 0 || unread.length > 0;
}

/** `path` as a line names a directory a process still holds, or may hold: "<path>, which pg_ctl 67
 *  still holds", "<path>, which bun 68 may hold, as it names /mnt/x/y, which answered no stat
 *  within 10 s" (`Holding`'s why). */
function heldPhrase(path: string, { holders, unread }: Holding): string {
  const phrases = [
    ...(holders.length > 0
      ? [`${listOf(holders.map(processName))} still ${holders.length === 1 ? "holds" : "hold"}`]
      : []),
    ...unread.map(
      ({ process, path: named, why }) =>
        `${processName(process)} may hold, as it names ${named}, ${why}`
    ),
  ];
  return `${path}, which ${listOf(phrases)}`;
}

/** How long a start waits for a claim another start holds and still runs: twice the longest stop
 *  of a postmaster that start may make under its claim (`postmasterQuitMs` and `postmasterKillMs`),
 *  so its removal of the directory after the stop has as long again. The removal of a data
 *  directory of 20,000 files took under a second on a shared Linux machine on 2026-11-17, and 141
 *  and 187 s on one whose disk other sessions loaded, on 2026-11-19, past this bound. At the bound
 *  the start leaves the part and its mark to that claim, names the claim, and goes on; the
 *  claimer's removal goes on too, and a claim its claimer's death left a later start takes. */
export const claimWaitMs = 2 * (postmasterQuitMs + postmasterKillMs);
/** How often a start reads a claim another start holds while it waits for it. */
const claimPollMs = 100;

/** What `claimForRemoval` found: this start holds the part, under the name its claim gave it; the
 *  part is gone and no start claims it; another start claimed it and removed it while this start
 *  waited; another start still holds its claim, named, after `claimWaitMs`; or a process of this
 *  user holds the part, named, and it keeps its name. */
export type Claim =
  | { readonly kind: "claimed"; readonly path: string }
  | { readonly kind: "absent" }
  | { readonly kind: "elsewhere" }
  | { readonly kind: "held"; readonly claim: string }
  | { readonly kind: "busy"; readonly heldBy: Holding };

/** Renames `from`, a directory of the user `uid`, to `to`, only where no process holds it:
 *  `holders` (`holdersOf`) reads that just before the rename, so a holder never finds its
 *  directory under another name. Answers "renamed"; "gone" where `from` is gone, or another start
 *  renamed it first; or each holder, and then `from` keeps its name. Throws, naming `from`, where
 *  it is anything else than a directory of `uid`, since a file, a link or another user's directory
 *  is nothing a stack made, and where the rename fails otherwise. */
async function renameDirectory(
  from: string,
  to: string,
  uid: number,
  holders: (directory: string) => Promise<Holding>
): Promise<"renamed" | "gone" | { readonly heldBy: Holding }> {
  let stat: Stats;
  try {
    stat = lstatSync(from);
  } catch (error) {
    if (codeOf(error) === "ENOENT") return "gone";
    throw error;
  }
  if (!stat.isDirectory() || stat.uid !== uid) {
    throw new Error(`${from} is not a directory of user ${uid}, so it is left`);
  }
  const heldBy = await holders(from);
  if (held(heldBy)) return { heldBy };
  try {
    renameSync(from, to);
  } catch (error) {
    if (codeOf(error) === "ENOENT") return "gone";
    throw new Error(`${from} could not be claimed: ${firstLine(error)}`);
  }
  return "renamed";
}

/** Each claim of `path` in its directory: a name `<path>.removing-<pid>-<start>`, with the pid and
 *  the start of the start that claimed it. */
function claimsOf(
  path: string
): { readonly path: string; readonly pid: number; readonly start: string }[] {
  const prefix = `${basename(path)}${claimInfix}`;
  let names: string[];
  try {
    names = readdirSync(dirname(path));
  } catch (error) {
    if (codeOf(error) === "ENOENT") return [];
    throw error;
  }
  return names.flatMap((name) => {
    const claimer = name.startsWith(prefix)
      ? /^(\d+)-(\d+)$/.exec(name.slice(prefix.length))
      : null;
    return claimer === null
      ? []
      : [{ path: join(dirname(path), name), pid: Number(claimer[1]), start: claimer[2] ?? "" }];
  });
}

/**
 * Claims `path`, a directory of the user `uid` that the mark of a dead stack names, for this
 * start's removal of it, and of the postmaster that serves from it: renames it to
 * `<path>.removing-<pid>-<start>`, the pid and the start of this process as `judge` reads an owner,
 * so of two starts that both find it, one rename wins. The rename is the claim, and there is no
 * other: a lock file or a list of claims would be a second thing a start killed mid-way leaves. A
 * claim whose claimer is gone, a start killed between its claim and its removal, this start claims
 * again by the same rename, so what that start left is removed by a later one. A claim whose
 * claimer still runs it waits out, for at most `claimWaitMs`, and says on one line that it waits,
 * and for whom. Before each rename it reads `holders` of the directory under the name it has then
 * (`renameDirectory`), and renames none that a process holds. Every start that judges a mark's
 * owner gone shares that owner's boot, PID namespace, time namespace and user, so a claimer is
 * judged in this start's own. Throws, naming the path, where a rename fails for another reason
 * than a part gone or a claim lost (`renameDirectory`), where `holders` throws, and where this
 * machine has no `/proc`.
 */
export async function claimForRemoval(
  path: string,
  uid: number,
  holders: (directory: string) => Promise<Holding>
): Promise<Claim> {
  const self = stackOwner();
  if (self === null) throw new Error(`${path} cannot be claimed: this machine has no /proc`);
  const mine = `${path}${claimInfix}${self.pid}-${self.start}`;
  const deadline = Date.now() + claimWaitMs;
  let sawClaim = false;
  let saidWait = false;
  // What the rename of `from` to this start's claim answers, or undefined where `from` was gone.
  const take = async (from: string): Promise<Claim | undefined> => {
    const taken = await renameDirectory(from, mine, uid, holders);
    if (taken === "gone") return undefined;
    return taken === "renamed"
      ? { kind: "claimed", path: mine }
      : { kind: "busy", heldBy: taken.heldBy };
  };
  for (;;) {
    const own = await take(path);
    if (own !== undefined) return own;
    const claims = claimsOf(path);
    if (claims.length === 0) {
      // A claimer whose removal failed gave the name back (`removeClaimed`) after this look.
      return (await take(path)) ?? (sawClaim ? { kind: "elsewhere" } : { kind: "absent" });
    }
    sawClaim = true;
    for (const claim of claims) {
      if (claim.path === mine) return { kind: "claimed", path: mine };
      const verdict = judge({ ...self, pid: claim.pid, start: claim.start }, self);
      const taken = verdict.kind === "gone" ? await take(claim.path) : undefined;
      if (taken !== undefined) return taken;
    }
    if (Date.now() >= deadline) {
      return { kind: "held", claim: listOf(claims.map((claim) => claim.path)) };
    }
    if (!saidWait) {
      console.error(
        `${named}: waits for another start's claim of ${path} (${listOf(claims.map((claim) => `start ${claim.pid}`))}), at most ${claimWaitMs / 1000} s`
      );
      saidWait = true;
    }
    await sleep(claimPollMs);
  }
}

/** Gives `path` back the name `claimForRemoval` took from it, so the mark names it again for the
 *  next start; where that rename fails, the claim stays under this process's name, and a start
 *  after this one claims it again once this process is gone. */
function giveBack(claimed: string, path: string): void {
  try {
    renameSync(claimed, path);
  } catch {
    // The claim stays under this process's name, for a start after it (`claimForRemoval`).
  }
}

/** Removes `claimed`, the name `claimForRemoval` gave `path` for this start. Where the removal
 *  fails it gives the name `path` back (`giveBack`) and throws, naming `path`. */
function removeClaimed(path: string, claimed: string): void {
  try {
    rmSync(claimed, { recursive: true, force: true });
  } catch (error) {
    giveBack(claimed, path);
    throw new Error(`${path} could not be removed: ${firstLine(error)}`);
  }
}

/** What a claim another start holds past `claimWaitMs` throws, naming `path` and that claim. */
function heldError(path: string, claim: string): Error {
  return new Error(
    `${path} is claimed by another start that still runs, as ${claim}, ${claimWaitMs / 1000} s after this start found it`
  );
}

/** Removes the directory `path` where it is a directory of the user `uid`, and nothing where it
 *  is gone, under this start's claim (`claimForRemoval`): of two starts that both find it, one
 *  removes it, and the other says that another start removed it ("lost"). A directory that a
 *  process of this user still holds (`holdersOf`, read before the claim, with `spellings`, the
 *  paths the mark names it by, through `reader`) it leaves under its name, naming each holder, for
 *  a later start to judge again. Throws, naming `path`, where it is anything else, since a file, a
 *  link or another user's directory is nothing a stack made, where another start's claim of it
 *  outlasts the wait, and where the removal fails, which gives the directory back under `path`
 *  for the next start. */
async function removeDirectory(
  path: string,
  spellings: readonly string[],
  uid: number,
  reader: StatReader
): Promise<Removal> {
  const claim = await claimForRemoval(path, uid, (name) => holdersOf(name, spellings, uid, reader));
  if (claim.kind === "absent") return "absent";
  if (claim.kind === "elsewhere") return "lost";
  if (claim.kind === "busy") return { heldBy: claim.heldBy };
  if (claim.kind === "held") throw heldError(path, claim.claim);
  removeClaimed(path, claim.path);
  return "removed";
}

/** Ends `postmaster`, the process `postmasterOf` verified, by its pid alone: SIGQUIT, and SIGKILL
 *  where it still runs after `postmasterQuitMs`, each only while `/proc` still names that process
 *  by its pid and its start. Answers whether a signal reached it: a postmaster that ended by
 *  itself after `postmasterOf` read it, before the first signal, got none, and a line must not say
 *  that this start removed it. Throws where it still runs after both. */
export async function stopPostmaster({ pid, start }: Postmaster, data: string): Promise<boolean> {
  let signalled = false;
  for (const [signal, ms] of [
    ["SIGQUIT", postmasterQuitMs],
    ["SIGKILL", postmasterKillMs],
  ] as const) {
    if (!stillRuns(pid, start)) return signalled;
    try {
      process.kill(pid, signal);
      signalled = true;
    } catch (error) {
      if (codeOf(error) !== "ESRCH") throw error;
    }
    const deadline = Date.now() + ms;
    while (stillRuns(pid, start) && Date.now() < deadline) await sleep(postmasterPollMs);
  }
  if (stillRuns(pid, start)) {
    throw new Error(`the postmaster ${pid} of ${data} still runs after SIGQUIT and SIGKILL`);
  }
  return signalled;
}

/** Ends the postmaster of `data`, where one still serves it, and then removes `data`, under this
 *  start's claim of `data` (`claimForRemoval`), so only the start that holds it signals the
 *  postmaster. Answers whether this call's signal stopped a postmaster, and what came of the
 *  directory: a start that finds `data` claimed by another start stops nothing. A directory that
 *  another process of this user still holds (`holdersOf`, with `spellings`, the paths the mark
 *  names it by, through `reader`), before the claim or after the stop, it leaves under its name,
 *  naming each holder: before the claim it claims and stops nothing. Throws, giving `data` back
 *  under its name, where the postmaster fails a check of `postmasterOf` or still runs after its
 *  stop. */
async function removePostmaster(
  bin: string,
  data: string,
  spellings: readonly string[],
  uid: number,
  reader: StatReader
): Promise<{ readonly stopped: boolean; readonly removal: Removal }> {
  // Before the claim, the postmaster that serves the directory under the name it has, which this
  // start would stop itself, and each other process that holds it.
  const claim = await claimForRemoval(data, uid, (name) =>
    holdersOf(name, spellings, uid, reader, postmasterOf(name, bin, uid)?.pid)
  );
  if (claim.kind === "absent") return { stopped: false, removal: "absent" };
  if (claim.kind === "elsewhere") return { stopped: false, removal: "lost" };
  if (claim.kind === "busy") return { stopped: false, removal: { heldBy: claim.heldBy } };
  if (claim.kind === "held") throw heldError(data, claim.claim);
  let stopped = false;
  try {
    // The kernel names a working directory by its latest name, the claimed one.
    const postmaster = postmasterOf(claim.path, bin, uid);
    if (postmaster !== undefined) stopped = await stopPostmaster(postmaster, data);
  } catch (error) {
    giveBack(claim.path, data);
    throw error;
  }
  // The processes of a postmaster end once it has: one that SIGQUIT reached waits for them
  // first, and one that SIGKILL ended leaves them to see it gone. A start reads the directory's
  // holders once after the stop; where a process still holds it, the start waits for each process
  // that read named to end, by its pid and its start in `/proc`, at most as long as a postmaster
  // gives them after SIGQUIT, and reads the holders once more. So a stop costs two holder reads at
  // most, whatever the timing, and a start leaves a directory that a process still holds then.
  let after: Holding = { holders: [], unread: [] };
  if (stopped) {
    after = await holdersOf(claim.path, spellings, uid, reader);
    if (held(after)) {
      const deadline = Date.now() + postmasterChildrenMs;
      // Each process by the pid the read kept, never read back out of the text a line names it by.
      const named = [...after.holders, ...after.unread.map(({ process }) => process)];
      const ending = named.flatMap(({ pid }) => {
        try {
          return [{ pid, start: startOf(readFileSync(`/proc/${pid}/stat`, "utf8")) }];
        } catch {
          return [];
        }
      });
      while (ending.some(({ pid, start }) => stillRuns(pid, start)) && Date.now() < deadline) {
        await sleep(postmasterPollMs);
      }
      after = await holdersOf(claim.path, spellings, uid, reader);
    }
  }
  if (held(after)) {
    giveBack(claim.path, data);
    return { stopped, removal: { heldBy: after } };
  }
  removeClaimed(data, claim.path);
  return { stopped, removal: "removed" };
}

/** `phrases` as one list for a line: "a", "a and b", "a, b and c". */
function listOf(phrases: readonly string[]): string {
  return phrases.length < 2
    ? (phrases[0] ?? "")
    : `${phrases.slice(0, -1).join(", ")} and ${phrases.at(-1)}`;
}

/** What a sweep leaves and says so: one list per kind, each of which is one line at its end. */
interface Left {
  readonly unread: string[];
  readonly untrusted: string[];
  readonly unjudged: string[];
  /** A dead stack's directory that a process of this user still holds (`holdersOf`). */
  readonly held: string[];
}

/** What the container sweep leaves the run directory sweep to read: the name of each container
 *  Docker still lists with a mark label after the sweep, and of each one the sweep removed. */
interface Containers {
  readonly listed: ReadonlySet<string>;
  readonly removed: ReadonlySet<string>;
}

/** Removes every container whose label marks a Postgres of a stack that is gone, with its socket
 *  directory, one after another, each within `bounds` (`removeContainer`), and answers what Docker
 *  lists then, or undefined where it cannot tell: where `docker ps` failed, which is one line, or
 *  where there is no `docker` on PATH, which says nothing here, since a start of the binaries
 *  provider needs none. A start that cannot list the containers leaves each mark of a container
 *  and names it (`sweepRun`): with no `docker` it cannot tell whether the container still runs,
 *  from a `docker` of another PATH. Whether a process holds a socket directory it reads through
 *  `reader` (`removeDirectory`). Each removal runs on `on`, the machine's own but for a row
 *  (`Machine`). */
async function removeContainers(
  self: Owner | null,
  left: Left,
  bounds: RemovalBounds,
  reader: StatReader,
  on: Machine
): Promise<Containers | undefined> {
  const listed = new Set<string>();
  const removed = new Set<string>();
  let lines: string;
  try {
    ({ stdout: lines } = await execFileAsync(
      "docker",
      [
        "ps",
        "--all",
        "--no-trunc",
        "--filter",
        `label=${markLabel}`,
        "--format",
        `{{.ID}} {{.Names}} {{.Label "${markLabel}"}}`,
      ],
      { timeout: dockerMs }
    ));
  } catch (error) {
    if (codeOf(error) === "ENOENT") return undefined;
    left.unread.push(`the containers, since docker ps failed: ${firstLine(error)}`);
    return undefined;
  }
  for (const line of lines.split("\n")) {
    if (line.trim() === "") continue;
    // An id and a name hold no space; the label, a path of a mark among it, may.
    const [id = "", name = "", ...label] = line.split(" ");
    listed.add(name);
    const what = `container ${id.slice(0, 12)}`;
    const mark = markOf(label.join(" "));
    if (mark?.container === undefined) {
      left.unread.push(`${what} (its label is no mark)`);
      continue;
    }
    // `docker commit` copies a container's labels into its image, and every container made from
    // that image carries them: a label is the mark of the container whose name it names alone.
    if (mark.container !== name) {
      left.unread.push(`${what} (its label is the mark of ${mark.container}, not of ${name})`);
      continue;
    }
    const verdict = judge(mark.owner, self);
    if (verdict.kind === "lives") continue;
    if (verdict.kind === "unjudged") {
      left.unjudged.push(`${what} (${verdict.why})`);
      continue;
    }
    // The label is Docker's, but the socket directory it names is acted on as any path is, under
    // the one spelling the run directory sweep acts on too.
    const { sockets } = markedPaths(mark);
    const why = sockets === undefined ? undefined : whyUntrusted(sockets, verdict.uid, "directory");
    if (why !== undefined) {
      left.untrusted.push(`${what} (${why})`);
      continue;
    }
    const by = `stack ${verdict.pid}`;
    const removal = await removeContainer(
      id,
      `${what} (${mark.container}), which ${by} left`,
      bounds,
      on
    );
    if (removal.kind === "failed") {
      console.error(
        `${named}: ${what} (${mark.container}), which ${by} left, is not removed: ${removal.reason}`
      );
      continue;
    }
    listed.delete(name);
    if (removal.kind === "elsewhere") {
      // Its socket directory is the run directory sweep's, which reads the mark of a container
      // Docker no longer lists.
      console.error(
        `${named}: ${what} (${mark.container}), which ${by} left, another start removed first: ${removal.reason}`
      );
      continue;
    }
    removed.add(name);
    try {
      // The mark names its socket directory as the stack spelt it, and as the sweep acts on it.
      const socketRemoval =
        sockets === undefined
          ? "absent"
          : await removeDirectory(sockets, [sockets, mark.sockets ?? sockets], verdict.uid, reader);
      const andSockets = socketRemoval === "removed" ? ` and ${sockets}` : "";
      console.error(
        `${named}: removed ${what} (${mark.container})${andSockets}, which ${by} left when it ` +
          `ended without stopping ${andSockets === "" ? "it" : "them"}`
      );
      // The mark stays with the directory, for a later start (`sweepRun`).
      if (sockets !== undefined && typeof socketRemoval === "object") {
        left.held.push(`${what} (${heldPhrase(sockets, socketRemoval.heldBy)})`);
      }
    } catch (error) {
      console.error(
        `${named}: removed ${what} (${mark.container}), which ${by} left when it ended without ` +
          `stopping it; its socket directory is not removed: ${firstLine(error)}`
      );
    }
  }
  return { listed, removed };
}

/** Whether nothing is at `path`, read with `lstat`. */
function absent(path: string): boolean {
  try {
    lstatSync(path);
    return false;
  } catch (error) {
    if (codeOf(error) === "ENOENT") return true;
    throw error;
  }
}

/** Removes, for each mark in the run directory `run` whose stack is gone, what the mark names
 *  outside a container, and then the mark: the postmaster and its data directory, and the socket
 *  directory. A container's mark is read only once its container is gone (`containers`): the
 *  container sweep judges, removes or names a container Docker lists, and its socket directory
 *  goes with it. It reads a run directory, a mark and a path a mark names only where
 *  `whyUntrusted` finds no other user could have put it there, and acts on the paths a mark names
 *  only where the mark's one spelling of its run directory is the run directory it checked
 *  (`markedPaths`). Whether a process holds a directory it reads through `reader`
 *  (`removePostmaster`, `removeDirectory`). Answers the data directory of each postmaster mark it
 *  read, or undefined where it named the run directory itself on a line. */
async function sweepRun(
  run: string,
  uid: number,
  self: Owner | null,
  containers: Containers | undefined,
  left: Left,
  reader: StatReader
): Promise<ReadonlySet<string> | undefined> {
  const untrusted = whyUntrusted(run, uid, "directory");
  if (untrusted !== undefined) {
    left.untrusted.push(`${run} (${untrusted})`);
    return undefined;
  }
  const read = new Set<string>();
  const marks = join(run, marksDirectory);
  let names: string[];
  try {
    names = readdirSync(marks);
  } catch (error) {
    // A run directory with no marks started no Postgres, or is no directory at all.
    if (codeOf(error) === "ENOENT" || codeOf(error) === "ENOTDIR") return read;
    left.unread.push(`${run} (${codeOf(error) ?? firstLine(error)})`);
    return undefined;
  }
  for (const name of names.filter((file) => file.endsWith(".json")).sort()) {
    const file = join(marks, name);
    const written = whyUntrusted(file, uid, "file");
    if (written !== undefined) {
      left.untrusted.push(`${file} (${written})`);
      continue;
    }
    let mark: Mark | undefined;
    try {
      mark = markOf(readUnlinked(file));
    } catch (error) {
      // A mark another start removed after this one listed it is no longer there to read.
      if (codeOf(error) === "ENOENT") continue;
      left.unread.push(`${file} (${codeOf(error) ?? firstLine(error)})`);
      continue;
    }
    if (mark === undefined || mark.identity !== run) {
      left.unread.push(`${file} (no mark of this run directory)`);
      continue;
    }
    // The paths the mark names, under the run directory checked above: markOf holds the data
    // directory to its stack's place in the run and the socket directory to the run or /tmp.
    const { sockets, data } = markedPaths(mark);
    if (data !== undefined) read.add(data);
    // A mark whose owner lives is left without a word, whatever this start can read of Docker.
    const verdict = judge(mark.owner, self);
    if (verdict.kind === "lives") continue;
    if (mark.container !== undefined) {
      if (containers === undefined) {
        left.unread.push(
          `${file} (its container ${mark.container}, since this start could not list Docker's containers)`
        );
        continue;
      }
      // A container Docker still lists is the container sweep's, which named it if it left it.
      if (containers.listed.has(mark.container)) continue;
      // One the container sweep removed went with its socket directory, which that sweep named
      // where it could not remove it: the mark goes with the directory, or stays for the next
      // start.
      if (containers.removed.has(mark.container)) {
        if (sockets === undefined || absent(sockets)) rmSync(file, { force: true });
        continue;
      }
    }
    if (verdict.kind === "unjudged") {
      left.unjudged.push(`${file} (${verdict.why})`);
      continue;
    }
    const why = [data, sockets]
      .map((path) =>
        path === undefined ? undefined : whyUntrusted(path, verdict.uid, "directory")
      )
      .find((reason) => reason !== undefined);
    if (why !== undefined) {
      left.untrusted.push(`${file} (${why})`);
      continue;
    }
    const what =
      data !== undefined
        ? `the postmaster of ${data}, that directory and ${sockets}`
        : sockets === undefined
          ? `the mark of container ${mark.container}`
          : `${sockets} of container ${mark.container}`;
    const by = `stack ${verdict.pid}`;
    // Each part this start removed itself, under its claim, which its line names, and each part
    // another start claimed and removed first (`claimForRemoval`): a line says "removed" of nothing
    // another start removed, and only the start that holds the data directory's claim signals its
    // postmaster. A part that a process of this user still holds (`holdersOf`) is left with the
    // rest of the mark, which stays for a later start to judge again.
    const done: string[] = [];
    const lost: string[] = [];
    const held: string[] = [];
    let failure: { readonly error: unknown } | undefined;
    try {
      if (mark.postmaster !== undefined && data !== undefined) {
        const { stopped, removal } = await removePostmaster(
          mark.postmaster.bin,
          data,
          [data, mark.postmaster.data],
          verdict.uid,
          reader
        );
        if (stopped) done.push(`the postmaster of ${data}`);
        if (removal === "removed") done.push(stopped ? "that directory" : data);
        else if (removal === "lost") lost.push(data);
        else if (removal !== "absent") held.push(heldPhrase(data, removal.heldBy));
      }
      if (sockets !== undefined && held.length === 0) {
        const removal = await removeDirectory(
          sockets,
          [sockets, mark.sockets ?? sockets],
          verdict.uid,
          reader
        );
        if (removal === "removed") done.push(sockets);
        else if (removal === "lost") lost.push(sockets);
        else if (removal !== "absent") held.push(heldPhrase(sockets, removal.heldBy));
      }
      if (held.length === 0) rmSync(file, { force: true });
    } catch (error) {
      failure = { error };
    }
    if (held.length > 0) left.held.push(`${file} (${listOf(held)})`);
    const elsewhere = lost.length === 0 ? "" : `; another start removed ${listOf(lost)} first`;
    if (failure !== undefined) {
      const why = firstLine(failure.error);
      console.error(
        done.length === 0
          ? `${named}: ${what}, which ${by} left, is not removed: ${why}${elsewhere}`
          : `${named}: removed ${listOf(done)}, which ${by} left, and the rest is not removed: ${why}${elsewhere}`
      );
      continue;
    }
    const container =
      mark.container === undefined
        ? ""
        : `; its container ${mark.container} is gone, as Docker lists no such container`;
    if (done.length > 0) {
      console.error(
        `${named}: removed ${listOf(done)}, which ${by} left when it ended without stopping them${elsewhere}${container}`
      );
    } else if (lost.length > 0) {
      console.error(`${named}: ${listOf(lost)}, which ${by} left, another start removed first`);
    }
  }
  return read;
}

/** Sweeps each run directory of `parent`, this start's TMPDIR, that the pool named, and each other
 *  run directory in `servedUnder` that a postmaster of this user serves from (`servedElsewhere`),
 *  by the same steps (`sweepRun`, with `reader`), and names each such postmaster that no mark of
 *  its run directory names and that still runs once that sweep is done: one another start removed
 *  meanwhile is no Postgres this start left. */
async function removeRunDirectories(
  parent: string,
  self: Owner | null,
  containers: Containers | undefined,
  left: Left,
  reader: StatReader,
  servedUnder: string
): Promise<void> {
  const uid = process.getuid?.() ?? -1;
  let real = parent;
  let entries: string[] = [];
  try {
    // TMPDIR may be spelt through a link of its user's: the run directories are listed under its
    // one spelling, which a mark holds its own run directory by, and `whyUntrusted` then reads
    // each component of it.
    real = identityOf(parent);
    entries = readdirSync(real);
  } catch (error) {
    // A TMPDIR this start cannot read lists no run directory of its own; the postmasters that
    // serve from the run directories of other TMPDIRs are looked for all the same.
    left.unread.push(`${parent} (${firstLine(error)})`);
  }
  for (const entry of entries.filter((name) => name.startsWith(runDirectoryPrefix)).sort()) {
    await sweepRun(join(real, entry), uid, self, containers, left, reader);
  }
  // The kernel shows a working directory with no link in it, the one spelling marks hold.
  const served = servedElsewhere(real, uid, servedUnder);
  for (const run of new Set(served.map((postmaster) => postmaster.run))) {
    const read = await sweepRun(run, uid, self, containers, left, reader);
    if (read === undefined) continue;
    for (const postmaster of served.filter((each) => each.run === run && !read.has(each.data))) {
      // Read again, by its pid and the start the listing read: another start may have removed
      // that stack's mark, and stopped its postmaster, since the listing.
      if (!stillRuns(postmaster.pid, postmaster.start)) continue;
      left.unjudged.push(
        `${postmaster.data} (postmaster ${postmaster.pid} serves it from a run directory this start does not list, and no mark there names it)`
      );
    }
  }
}

/** Removes each Postgres that a stack which is gone left on this machine: every container that
 *  Docker lists with a mark, every mark in a run directory of `tmpdir()`, and every mark in a run
 *  directory elsewhere that a postmaster of this user serves from. Leaves every other one, and
 *  names what it left on one line per kind. Never throws: a removal that fails is one line, and
 *  the start goes on. `bounds` are the start's own (`removalBounds`) but for a row, and so
 *  is `reader`, the program that reads where each path a process names leads (`statReader`), and
 *  `servedUnder`, the directory it looks in for those run directories elsewhere: the whole
 *  machine for a start, and a row's own directory for a row, so that the sweep of one row judges
 *  no stack that another row, or another session of the machine, planted beside it; and so is
 *  `on`, the machine each container's removal runs on (`Machine`): a row's moves its own clock,
 *  so the line a removal ends with at its bound depends on no load. */
export async function removeLeftoverPostgres(
  bounds = removalBounds,
  reader = statReader,
  servedUnder = "/",
  on = machine
): Promise<void> {
  const self = stackOwner();
  const left: Left = { unread: [], untrusted: [], unjudged: [], held: [] };
  try {
    // The containers first, so a container goes before the socket directory it serves in.
    const containers = await removeContainers(self, left, bounds, reader, on);
    await removeRunDirectories(tmpdir(), self, containers, left, reader, servedUnder);
  } catch (error) {
    console.error(
      `${named}: the Postgres of stacks that are gone is not looked for: ${firstLine(error)}`
    );
  }
  if (left.unread.length > 0) {
    console.error(`${named}: left what it cannot read: ${left.unread.join(", ")}`);
  }
  if (left.untrusted.length > 0) {
    console.error(
      `${named}: left what another user could have written, or a link leads out of: ${left.untrusted.join(", ")}`
    );
  }
  if (left.unjudged.length > 0) {
    console.error(
      `${named}: left the Postgres of stacks whose process it cannot see: ${left.unjudged.join(", ")}`
    );
  }
  if (left.held.length > 0) {
    console.error(
      `${named}: left the Postgres of stacks whose directories a process of this user still holds, or may hold, for a later start to judge again: ${left.held.join(", ")}`
    );
  }
}
