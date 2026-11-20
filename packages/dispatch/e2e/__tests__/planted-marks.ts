/** What the rows of the check `v-no-leftover-postgres` plant beside the marks a stack writes: a
 *  live stand-in process, and a hostile mark that names a program of the planter's and a directory
 *  outside its run (`harness-postgres-marks.test.ts`, `harness-postgres-leftover.test.ts`). */

import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import type { Subprocess } from "bun";

import { identityOf, marksDirectory, startOf } from "../harness-postgres-marks";
import { claimEventMs } from "./harness-postgres-waits";

/** The start of the process `pid` as `/proc` gives it, or "" where it is gone or a zombie. */
function startTimeOf(pid: number): string {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
    const state = stat.slice(stat.lastIndexOf(")") + 2, stat.lastIndexOf(")") + 3);
    return state === "Z" || state === "X" ? "" : startOf(stat);
  } catch {
    return "";
  }
}

/** Each process of the process group `group` that has not ended, by its state in `/proc`: one that
 *  is gone, or a zombie, has ended, whoever reaps it. */
export function groupRuns(group: number): number[] {
  const runs: number[] = [];
  for (const entry of readdirSync("/proc")) {
    if (!/^\d+$/.test(entry)) continue;
    try {
      const stat = readFileSync(`/proc/${entry}/stat`, "utf8");
      const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
      if (Number(fields[2]) === group && fields[0] !== "Z" && fields[0] !== "X") {
        runs.push(Number(entry));
      }
    } catch {
      // A process that ended while it was read has ended.
    }
  }
  return runs;
}

/** A live process whose working directory is `cwd`, as a postmaster's is its data directory. */
export interface StandIn {
  readonly pid: number;
  alive(): boolean;
  /** Ends the stand-in and every process it started: SIGKILL to its process group, only while
   *  `/proc` still names the stand-in by its pid and its start, and then waits until no process of
   *  that group has not ended (`groupRuns`), as long as a row waits for one event of another
   *  process (`claimEventMs`), and throws, naming those processes, where one has not; so the row
   *  that stops it ends after it, on its pass and its failure paths alike. */
  stop(): Promise<void>;
}

/** The command of a stand-in that lives until this process, the process of the row that starts
 *  it, ends, and sets no length of its own: GNU `tail`, which follows `/dev/null` until the
 *  process `--pid` names ends, and reads whether it still runs once a second; neither `/dev/null`
 *  nor the pid is a path of a directory a row plants. The row ends it sooner (`StandIn.stop`). A
 *  stand-in that a row gave a fixed 600 s (`sleep 600`) ended inside the row's waits on a shared
 *  Linux machine on 2026-11-19: the third start of the pool that the row waited for took 620 s
 *  inside its own bounds, and the row then read the live process of its hostile mark as gone. */
export function untilThisEnds(): string[] {
  const tail = Bun.which("tail");
  if (tail === null) throw new Error("no tail on PATH for a stand-in that lives as its row does");
  return [tail, `--pid=${process.pid}`, "-f", "/dev/null"];
}

/** A script for `bun --eval` that keeps its process alive until its parent, the process that
 *  started it, ends, and then ends it, with no length of its own: it reads its parent's pid once a
 *  second, which changes once the kernel gives the process to another parent. It names no path,
 *  since a start's holder read reads each word of a process's arguments as one. */
export const untilParentEnds =
  "const parent = process.ppid; setInterval(() => { if (process.ppid !== parent) process.exit(1); }, 1_000);";

/** A test in bash builtins alone, so it forks no process: whether the shell's parent is still the
 *  process that started it, which it is not once that process ended (`untilParentEnds`). The
 *  stand-ins that run it are named `postgres`, so the second field of `/proc/<pid>/stat`, the
 *  name, holds no space. */
export const parentLives = 'read -r _ _ _ ppid _ </proc/$$/stat && [ "$ppid" = "$PPID" ]';

/** Starts `command` in `cwd`, with `stdin` for its standard input where it is given: a file
 *  descriptor of this process, which the child then holds open, or "pipe", a pipe whose other end
 *  this process holds, which a stand-in that reads it to its end reads until this process ends.
 *  `command` lives as long as the row needs it (`untilThisEnds`), never for a fixed time, which
 *  the source check of `harness-postgres-source-rules.test.ts` refuses (`fixedLives`). It runs in
 *  a process group of its own, so its stop ends each process it started too (`StandIn.stop`). A
 *  stand-in that ends with its parent instead (`untilParentEnds`, `parentLives`) ends as an orphan,
 *  which the PID namespace's PID 1 reaps: a namespace whose PID 1 is `bun test` itself reaps no
 *  process it did not start, so each such zombie holds a task of the run's limit until the run
 *  ends (20 reruns of the removal files reached a limit of 512 so in 4 minutes on 2026-11-20), and
 *  such a run needs a PID 1 that reaps, as bash does. */
export function standIn(cwd: string, command: readonly string[], stdin?: number | "pipe"): StandIn {
  const child: Subprocess = Bun.spawn([...command], {
    cwd,
    stdin: stdin ?? "ignore",
    stdout: "ignore",
    stderr: "ignore",
    detached: true,
  });
  const start = startTimeOf(child.pid);
  const alive = () => start !== "" && startTimeOf(child.pid) === start;
  return {
    pid: child.pid,
    alive,
    stop: async () => {
      if (alive()) {
        try {
          process.kill(-child.pid, "SIGKILL");
        } catch {
          // The group ended between the check and the signal.
        }
      }
      const deadline = Date.now() + claimEventMs;
      for (let runs = groupRuns(child.pid); runs.length > 0; runs = groupRuns(child.pid)) {
        if (Date.now() >= deadline) {
          throw new Error(
            `the stand-in ${child.pid}'s processes ${runs.join(", ")} had not ended ${claimEventMs / 1000} s after its stop`
          );
        }
        await Bun.sleep(20);
      }
      await child.exited;
    },
  };
}

/** A directory in `tmp` that holds a `pg_ctl` which records that it ran, in `marker`, and then
 *  kills the process the data directory's `postmaster.pid` names: the program a hostile mark names
 *  as its postmaster's binaries. It uses shell builtins alone, so it runs under any `PATH`. */
export function plantedBin(tmp: string, marker: string): string {
  const bin = mkdtempSync(join(tmp, "planted-bin-"));
  writeFileSync(
    join(bin, "pg_ctl"),
    `#!/bin/sh\nprintf 'ran\\n' >${JSON.stringify(marker)}\n` +
      `read -r pid <"$2/postmaster.pid"\nkill -9 "$pid"\n`,
    { mode: 0o755 }
  );
  return bin;
}

/** What a start must leave of a hostile mark, as `kept` of `HostileMark` reads it. */
export interface HostileMarkState {
  /** Whether the planted program ran: it wrote its marker. */
  readonly ran: boolean;
  /** Whether the stand-in its `postmaster.pid` names still runs. */
  readonly lives: boolean;
  /** Whether the directory outside the run still holds its file. */
  readonly outside: boolean;
  /** Whether the mark file is still there. */
  readonly mark: boolean;
}

/** A hostile mark a row planted (`plantHostileMark`). */
export interface HostileMark {
  /** The file the planted program writes when it runs. */
  readonly marker: string;
  readonly live: StandIn;
  /** The mark file. */
  readonly file: string;
  /** The link out of the run, `worker-0`. */
  readonly link: string;
  kept(): HostileMarkState;
}

/** The mark of stack 0 in `run`, whose owner is `owner` and whose postmaster is a program of the
 *  planter's (`plantedBin`) serving from `worker-0/pgdata`, where `worker-0` is a link to a
 *  directory of `tmp` outside the run, in which a live stand-in has that data directory for its
 *  working directory and is named by its `postmaster.pid`. That stand-in lives until this process
 *  ends (`untilThisEnds`), so it outlives every wait of the row. */
export function plantHostileMark(run: string, tmp: string, owner: unknown): HostileMark {
  const marker = join(mkdtempSync(join(tmp, "marker-")), "the-planted-program-ran");
  const outside = mkdtempSync(join(tmp, "outside-"));
  mkdirSync(join(outside, "pgdata"));
  const live = standIn(join(outside, "pgdata"), untilThisEnds());
  writeFileSync(join(outside, "pgdata", "PG_VERSION"), "18\n");
  writeFileSync(join(outside, "pgdata", "postmaster.pid"), `${live.pid}\n`);
  symlinkSync(outside, join(run, "worker-0"));
  const sockets = mkdtempSync(join(run, "pg-0-"));
  mkdirSync(join(run, marksDirectory), { recursive: true });
  const file = join(run, marksDirectory, "worker-0.json");
  const postmaster = { bin: plantedBin(tmp, marker), data: join(run, "worker-0", "pgdata") };
  writeFileSync(
    file,
    JSON.stringify({ owner, run, identity: identityOf(run), sockets, postmaster })
  );
  return {
    marker,
    live,
    file,
    link: join(run, "worker-0"),
    kept: () => ({
      ran: existsSync(marker),
      lives: live.alive(),
      outside: existsSync(join(outside, "pgdata", "PG_VERSION")),
      mark: existsSync(file),
    }),
  };
}

/** What `kept` of `HostileMark` reads once a start left the hostile mark as it must. */
export const hostileMarkKept: HostileMarkState = {
  ran: false,
  lives: true,
  outside: true,
  mark: true,
};
