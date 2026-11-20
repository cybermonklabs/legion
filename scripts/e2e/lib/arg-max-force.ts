import { join } from "node:path";

/**
 * argMaxForceHolds() says whether this machine's kernel holds an exec to ARG_MAX under a 512 KiB
 * stack, the force lib/arg-max-force.sh puts a row under: an environment of two 100,000-byte
 * variables, each under the kernel's limit for one string, together past the 128 KiB that stack
 * allows, must make the exec of a program of no arguments fail. Linux holds it. gVisor (a Legion
 * pod's kernel, 4.19.0-gvisor) starts that program, so there the force cannot be put in force, and a
 * row under it proves nothing either way: the rows that need it are skipped there, by this name,
 * rather than failed or passed.
 */
let holds: boolean | undefined;

export function argMaxForceHolds(): boolean {
  if (holds === undefined) {
    const probe = Bun.spawnSync(
      [
        "bash",
        "-c",
        'ulimit -s 512 && a=$(printf "%100000s" "") && PADDING1=$a PADDING2=$a "$(type -P true)"',
      ],
      { env: { PATH: process.env.PATH ?? "" }, stdout: "ignore", stderr: "ignore" }
    );
    holds = probe.exitCode !== 0;
  }
  return holds;
}

const force = join(import.meta.dir, "arg-max-force.sh");

/**
 * The files and directories arg_max_force_entries makes, at most, in a proc root at the path ROOT
 * that holds none yet, for ENTRY and ROOM: four for each entry, and entries enough that their
 * paths take twice ROOM bytes as arguments, each counted at a pid of one digit, so the count is
 * never under the one the shell makes, whose pids, above the kernel's pid_max, have more.
 */
export function forceEntriesSize(root: string, entry: string, room: number): number {
  const perEntry = root.length + 1 + entry.length + 2 + 9;
  return 4 * Math.ceil((2 * room) / perEntry);
}

/**
 * How long one file or directory of the force's entries may take to make, in milliseconds: twice
 * the slowest one took in 40 runs of arg_max_force_entries for `environ 6144` beside the load of
 * the devbox the rows failed on, 4.4 ms (568 files and directories in 2.48 s; the 40 runs took
 * 0.21 to 2.48 s, at one-minute loads of 360 to 847, on 2026-11-20). A hook that makes the entries
 * (forceEntriesBound) is held to that many of them at this each.
 */
export const forceEntryMs = 8.8;

/**
 * The bound, in milliseconds, of a hook that makes the force's entries for each of ROOTS:
 * forceEntriesSize's count of the files and directories of each, at forceEntryMs each. That is the
 * whole of the force's cost on the disk, which a row's own bound, held to its walk, no longer
 * carries.
 */
export function forceEntriesBound(
  roots: readonly { readonly root: string; readonly entry: string; readonly room: number }[]
): number {
  let size = 0;
  for (const { root, entry, room } of roots) size += forceEntriesSize(root, entry, room);
  return Math.ceil(size * forceEntryMs);
}

/**
 * Makes the force's entries (arg_max_force_entries) in the proc root ROOT, which must exist, for
 * ENTRY and ROOM, in a bash of its own, and throws naming what it printed where that fails. A row
 * whose harness then calls arg_max_force on ROOT finds them there and makes none.
 */
export function makeForceEntries(root: string, entry: string, room: number): void {
  const made = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail\n. ${JSON.stringify(force)}\narg_max_force_entries "$1" "$2"`,
      "-",
      entry,
      String(room),
    ],
    { env: { PATH: process.env.PATH ?? "", proc_root: root }, stdout: "pipe", stderr: "pipe" }
  );
  if (made.exitCode !== 0) {
    throw new Error(
      `arg_max_force_entries ${entry} ${room} in ${root} exited ${made.exitCode}: ${made.stderr.toString()}`
    );
  }
}

/**
 * The line a forced row's harness prints on its standard output once arg_max_force has returned,
 * and before its walk starts: walkWithin starts the walk's deadline there.
 */
export const forcedLine = "arg_max_force: in force";

/**
 * The most one walk of every process may take, in milliseconds. wait_for_run_processes
 * (scripts/e2e/parked-decision-devbox.sh), the longest wait on run_processes, gives every process
 * of the run 600 s to end and polls run_processes at least 10 times in them, so a walk past 60 s
 * runs that wait past its own deadline.
 */
export const walkDeadline = 60_000;

/** What walkWithin answers. */
export interface ForcedWalk {
  /** The harness's exit status, or null where a signal ended it. */
  readonly code: number | null;
  /** What the harness printed on its standard output after forcedLine: the walk's output. */
  readonly out: string;
  readonly err: string;
  /** Whether the walk was still running at its deadline, and was ended for it. */
  readonly timedOut: boolean;
}

/**
 * Runs ARGV, a bash that leads a process group of its own (ownGroup) and prints forcedLine once it
 * is under the force, then walks, and holds its walk, what it does after that line, to DEADLINE
 * milliseconds: one still running then is ended with every process of its group, by a SIGKILL to
 * that group sent while the runtime has not reaped its leader, so the group's id is still the
 * harness's own. The force before that line is held to nothing here: its entries come from a hook
 * (makeForceEntries), and the rest is the row's own. The harness is a real process, which no fake
 * clock drives, so the deadline is a timer of the real clock. A harness that never prints the line
 * is waited for to its end, and answers all it printed.
 */
export async function walkWithin(
  argv: string[],
  env: Record<string, string>,
  deadline: number
): Promise<ForcedWalk> {
  const child = Bun.spawn(argv, { env, stdout: "pipe", stderr: "pipe" });
  const err = new Response(child.stderr).text();
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  const late = Promise.withResolvers<"late">();
  let printed = "";
  let timer: Timer | undefined;
  let timedOut = false;
  let pending = reader.read();
  for (;;) {
    const next = timedOut ? await pending : await Promise.race([pending, late.promise]);
    if (next === "late") {
      timedOut = true;
      if (child.exitCode === null && child.signalCode === null) process.kill(-child.pid, "SIGKILL");
      continue;
    }
    if (next.done) break;
    printed += decoder.decode(next.value, { stream: true });
    if (timer === undefined && printed.includes(`${forcedLine}\n`)) {
      timer = setTimeout(() => late.resolve("late"), deadline);
    }
    pending = reader.read();
  }
  clearTimeout(timer);
  await child.exited;
  const at = printed.indexOf(`${forcedLine}\n`);
  return {
    code: child.exitCode,
    out: at < 0 ? printed : printed.slice(at + forcedLine.length + 1),
    err: await err,
    timedOut,
  };
}
