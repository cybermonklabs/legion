/** `bun e2e/reaper.ts <list> <pid>`: stops what process `<pid>` started and did not stop, once it
 *  has died. e2e/harness-reaper.ts starts it, says why it exists, and keeps `<list>`.
 *
 *  Once it listens for the end of its stdin it prints `readyLine` on its stdout, which is what its
 *  starter waits for before it starts anything. The end of stdin comes when `<pid>`, the only
 *  holder of the pipe's other end, exits or is killed. It then reads the list and `reap`s it. Then
 *  it says on stderr what it did, removes the list's directory, and exits 0. A list with nothing
 *  on it - the starter stopped everything before it ended, which is every run that was not killed
 *  - ends it at once, saying nothing.
 *
 *  Its waits are bounded so it always ends, and no verdict reads them: whatever run this reaper
 *  serves has already ended. */
import { spawnSync } from "node:child_process";
import { readFileSync, rmSync } from "node:fs";
import { dirname } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

import { type Cleanup, type ProcStat, procStat, readyLine } from "./harness-reaper";

const named = "e2e/reaper.ts";
/** How long `reap` waits for every awaited process. A `pg_ctl -w start` gives up after its own
 *  PGCTLTIMEOUT, 60 s by default, and this outlasts that. */
const awaitBoundMs = 120_000;
/** How long one command may run: a `pg_ctl -w stop` waits up to that same PGCTLTIMEOUT. */
const commandBoundMs = 120_000;

/** What `reap` signals, reads and runs with. `realCollaborators` are the machine's; the rows of
 *  e2e/__tests__/harness-reaper.test.ts that drive `reap` over a scripted machine pass their own. */
export interface Collaborators {
  /** `process.kill`: throws ESRCH for a process or group that has ended. */
  readonly kill: (pid: number, signal: NodeJS.Signals) => void;
  /** `procStat`: when the process with this number started and whether it has ended, or
   *  undefined where no process has the number or there is no `/proc` to say. */
  readonly stat: (pid: number) => ProcStat | undefined;
  /** Runs one command to its end, and answers why it failed, or undefined when it exited 0. */
  readonly run: (command: readonly string[]) => string | undefined;
  /** One interval between two reads of whether an awaited process still runs. */
  readonly pause: () => Promise<void>;
  /** Milliseconds, as `Date.now()` reads them. */
  readonly now: () => number;
}

/** What `reap` did, as its account says it. */
export interface Reaped {
  /** Process groups it sent SIGKILL to that still had a member. */
  readonly killed: number;
  /** Process groups it sent nothing, each with its reason in `notes`. */
  readonly leftAlone: number;
  /** Processes it waited for. */
  readonly awaited: number;
  /** Commands it ran. */
  readonly ran: number;
  /** One line for each group left alone and for each thing that went wrong. */
  readonly notes: readonly string[];
}

/** Whether the process numbered `pid` is still the one that started at `start` and has not
 *  ended. With no `start` recorded nothing can be told about it, and it counts as ended. */
function stillTheOne(machine: Collaborators, pid: number, start: string | undefined): boolean {
  const seen = machine.stat(pid);
  return start !== undefined && seen !== undefined && !seen.ended && seen.start === start;
}

/** Stops what `entries` hold, in this order:
 *
 *  1. sends SIGKILL to every process group whose leader still runs with the start time its entry
 *     records. The process that would have stopped them gracefully is gone, and SIGKILL is the
 *     one signal no server can wait out. A group is left alone where its number now names a
 *     process that started at another time, which the kernel can hand any new process once the
 *     old one is gone - another run's server, a daemon - or where no process has the number at
 *     all, since a group whose leader is gone can hold members of a group that number names now.
 *     A leader that has died and waits to be reaped still has its number and start time, and its
 *     group is still the run's;
 *  2. waits for every awaited process to end - a `pg_ctl -w start` or a `docker run` that was
 *     still under way - so the commands after it find the server it started, rather than racing
 *     it to a data directory or a container name. A number another process holds now is ended;
 *  3. runs the commands of each entry, the entries last-added first, as a stack unwinds: a
 *     `pg_ctl stop` before the removal of the run's directory, which was added before any
 *     database in it. */
export async function reap(
  entries: readonly (readonly [string, Cleanup])[],
  machine: Collaborators
): Promise<Reaped> {
  const notes: string[] = [];

  let killed = 0;
  let leftAlone = 0;
  for (const [key, { group, start }] of entries) {
    if (group === undefined) continue;
    const seen = machine.stat(group);
    if (start === undefined || seen === undefined || seen.start !== start) {
      leftAlone += 1;
      notes.push(
        start === undefined
          ? `${key}: left alone, no start time was recorded for it`
          : seen === undefined
            ? `${key}: left alone, no process has its number now`
            : `${key}: left alone, its number now names a process that started at ${seen.start}, not ${start}`
      );
      continue;
    }
    try {
      machine.kill(-group, "SIGKILL");
      killed += 1;
    } catch (error) {
      // ESRCH: every process of the group had already ended.
      if ((error as NodeJS.ErrnoException).code !== "ESRCH") {
        notes.push(`${key}: SIGKILL failed: ${(error as Error).message}`);
      }
    }
  }

  const awaited = entries.flatMap(([key, { awaits, start }]) =>
    awaits === undefined ? [] : [{ key, pid: awaits, start }]
  );
  const runs = () => awaited.filter(({ pid, start }) => stillTheOne(machine, pid, start));
  const deadline = machine.now() + awaitBoundMs;
  while (runs().length > 0 && machine.now() < deadline) {
    await machine.pause();
  }
  for (const { key, pid } of runs()) {
    notes.push(`${key}: process ${pid} still runs after ${awaitBoundMs / 1000} s`);
  }

  let ran = 0;
  for (const [key, { commands = [] }] of [...entries].reverse()) {
    for (const command of commands) {
      const why = machine.run(command);
      ran += 1;
      if (why !== undefined) notes.push(`${key}: \`${command.join(" ")}\` ${why}`);
    }
  }
  return { awaited: awaited.length, killed, leftAlone, notes, ran };
}

const realCollaborators: Collaborators = {
  kill: (pid, signal) => process.kill(pid, signal),
  now: () => Date.now(),
  pause: async () => {
    // An orphaned process raises no event in this one, so its end is read again each interval.
    await sleep(50);
  },
  run: ([file, ...args]) => {
    if (file === undefined) return "is empty";
    const result = spawnSync(file, args, {
      encoding: "utf8",
      killSignal: "SIGKILL",
      stdio: ["ignore", "ignore", "pipe"],
      timeout: commandBoundMs,
    });
    if (result.error !== undefined) return result.error.message;
    if (result.status === 0) return undefined;
    return `exited ${result.status ?? result.signal}: ${result.stderr.trim()}`;
  },
  stat: procStat,
};

/** Reads the list, reaps it, and says what it did. */
async function reapList(list: string, starter: string): Promise<void> {
  let entries: [string, Cleanup][] = [];
  try {
    entries = JSON.parse(readFileSync(list, "utf8")) as [string, Cleanup][];
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  if (entries.length === 0) return;
  const started = Date.now();
  const { awaited, killed, leftAlone, notes, ran } = await reap(entries, realCollaborators);
  process.stderr.write(
    `${named}: process ${starter} ended without stopping what it started; in ` +
      `${Date.now() - started} ms this sent SIGKILL to ${killed} process groups, left ` +
      `${leftAlone} alone, waited for ${awaited} starts under way and ran ${ran} commands, for ` +
      `${entries.length} entries (${entries.map(([key]) => key).join(", ")})\n` +
      notes.map((line) => `${named}: ${line}\n`).join("")
  );
}

if (import.meta.main) {
  const [list, starter] = process.argv.slice(2);
  if (list === undefined || starter === undefined) {
    process.stderr.write(`${named}: usage: bun e2e/reaper.ts <list> <pid>\n`);
    process.exit(2);
  }
  // A write to a stream whose reader has gone must not end this process before its work is done:
  // the starter closes stdout once it has read the ready line.
  process.on("SIGPIPE", () => {});
  process.stdout.on("error", () => {});
  process.stderr.on("error", () => {});
  let ending = false;
  const end = (): void => {
    if (ending) return;
    ending = true;
    reapList(list, starter)
      .catch((error: unknown) => {
        process.stderr.write(
          `${named}: ${error instanceof Error ? (error.stack ?? error.message) : String(error)}\n`
        );
      })
      .finally(() => {
        rmSync(dirname(list), { force: true, recursive: true });
        process.exit(0);
      });
  };
  // Nothing is ever written to stdin; its end, or its failure, is the starter's death.
  process.stdin.on("data", () => {});
  process.stdin.once("end", end);
  process.stdin.once("close", end);
  process.stdin.once("error", end);
  process.stdout.write(`${readyLine}\n`);
}
