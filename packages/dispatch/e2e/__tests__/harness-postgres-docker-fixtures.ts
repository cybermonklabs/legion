/** The stand-in `docker` of the rows of a start's removal of a container
 *  (`harness-postgres-container-removal.test.ts`) and of the leftover rows' fixture `docker`
 *  (`harness-postgres-waits.test.ts`): a `docker` that runs no real one, with its own clock
 *  (`standInDocker`, `Tick`), a start's removal of one container in a child (`removeInChild`), a
 *  call of the leftover fixtures' `docker` in a child (`fixtureDockerInChild`), and what those rows
 *  share. The other fixtures of these rows are in `harness-postgres-removal-fixtures.ts`. */

import { mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { RemovalBounds } from "../harness-postgres-container-removal";
import * as marks from "../harness-postgres-marks";
import { boundsEnv, holding, inChild } from "./harness-postgres-removal-fixtures";
import { claimEventMs, type Row, type Waits } from "./harness-postgres-waits";

const containerRemovalModule = fileURLToPath(
  new URL("../harness-postgres-container-removal.ts", import.meta.url)
);

/** A start's removal of the container `CONTAINER_ID` (`removeContainer`) in a child of this file,
 *  within the row's bounds (`boundsEnv`): it prints how the removal ended, and its own lines on
 *  stderr. */
const removeScript = `
import { removeContainer } from ${JSON.stringify(containerRemovalModule)};
const { CONTAINER_ID: id = "", ANSWER_MS, BOUND_MS, WATCH_MS } = process.env;
const bounds = { answerMs: Number(ANSWER_MS), boundMs: Number(BOUND_MS), watchMs: Number(WATCH_MS) };
console.log(JSON.stringify(await removeContainer(id, undefined, bounds)));
`;

/** One entry of a stand-in's own clock: when a call reached it, or when it ended, in seconds since
 *  the epoch (`EPOCHREALTIME`), with the call's arguments. */
export interface Tick {
  readonly at: number;
  readonly event: "start" | "end";
  readonly call: string;
}

/** A stand-in `docker` (`standInDocker`): its directory, the calls it got, its clock, each process
 *  it wrote, by its pid and start, and each process that runs now under its path. */
export interface StandInDocker {
  readonly bin: string;
  calls(): string[];
  clock(): Tick[];
  processes(): { pid: number; start: string }[];
  running(): { pid: number; start: string }[];
}

/** A directory in `tmp` with a stand-in `docker`, which runs no real one, so a row of it acts on
 *  no container of the machine: it writes the arguments of each call it gets as a line of
 *  `calls`, the stat of its own process as a file of `processes`, and, on its own clock, when the
 *  call reached it and when it ended (`Tick`), and then runs the lines of bash `body`. That clock
 *  is the stand-in's, which no load on the start or on the row stretches: a row times the start's
 *  waits on it. A body that execs another program keeps the stand-in's path as its name (`exec -a
 *  "$0"`), so `running` finds it, and writes no end; one that waits in the background (`sleep N &
 *  wait $!`) ends on SIGTERM, which a start's stop of a call sends, writes its end then, and ends
 *  what it waited for. Answers the directory, the calls so far, the clock so far, each process
 *  the stand-in wrote, by its pid and start, and each process that runs now under its path. */
export function standInDocker(tmp: string, body: readonly string[]): StandInDocker {
  const bin = mkdtempSync(join(tmp, "docker-"));
  const calls = join(bin, "calls");
  const clock = join(bin, "clock");
  const processes = join(bin, "processes");
  mkdirSync(processes);
  writeFileSync(
    join(bin, "docker"),
    [
      "#!/usr/bin/env bash",
      `echo "$*" >>${JSON.stringify(calls)}`,
      `cat /proc/$$/stat >${JSON.stringify(processes)}/$$`,
      `printf '%s start %s\\n' "$EPOCHREALTIME" "$*" >>${JSON.stringify(clock)}`,
      `trap 'kill $(jobs -p) 2>/dev/null; printf "%s end %s\\n" "$EPOCHREALTIME" "$*" >>${JSON.stringify(clock)}' EXIT`,
      "trap 'exit 143' TERM",
      ...body,
      "",
    ].join("\n"),
    { mode: 0o755 }
  );
  const ofStandIn = `${join(bin, "docker")}\0`;
  return {
    bin,
    calls: (): string[] => readFileSync(calls, "utf8").trim().split("\n"),
    clock: (): Tick[] =>
      readFileSync(clock, "utf8")
        .split("\n")
        .filter(Boolean)
        .map((line) => {
          const [at = "", event = "", ...call] = line.split(" ");
          return { at: Number(at), event: event === "end" ? "end" : "start", call: call.join(" ") };
        }),
    processes: (): { pid: number; start: string }[] =>
      readdirSync(processes).map((pid) => ({
        pid: Number(pid),
        start: marks.startOf(readFileSync(join(processes, pid), "utf8")),
      })),
    // A stand-in that has not reached its first line yet, or wrote no stat before a signal ended
    // it, is found here all the same: its command line names the stand-in's path.
    running: (): { pid: number; start: string }[] =>
      readdirSync("/proc").flatMap((entry) => {
        if (!/^\d+$/.test(entry)) return [];
        try {
          if (!readFileSync(`/proc/${entry}/cmdline`, "utf8").includes(ofStandIn)) return [];
          return [
            {
              pid: Number(entry),
              start: marks.startOf(readFileSync(`/proc/${entry}/stat`, "utf8")),
            },
          ];
        } catch {
          return [];
        }
      }),
  };
}

/** Removes the container `id` in a child (`removeScript`) whose `docker` is the one in `bin`,
 *  within `bounds`, and charges `row` for the child. Answers how the removal ended, the lines the
 *  start printed, and how many seconds the child took; kills the child and throws where it has
 *  not ended within everything the row waits for (`Row.ms`). */
export async function removeInChild(row: Row, bin: string, id: string, bounds: RemovalBounds) {
  row.child(`the removal of container ${id.slice(0, 12)}`);
  const { stdout, stderr, seconds } = await inChild(
    removeScript,
    bin,
    { CONTAINER_ID: id, ...boundsEnv(bounds) },
    row
  );
  return { removal: JSON.parse(stdout) as unknown, lines: stderr.split("\n"), seconds };
}

const leftoverFixtures = fileURLToPath(
  new URL("./harness-postgres-leftover-fixtures.ts", import.meta.url)
);
const waitsModule = fileURLToPath(new URL("./harness-postgres-waits.ts", import.meta.url));

/** One call of the leftover rows' fixture `docker` (`harness-postgres-leftover-fixtures.ts`) in a
 *  child of this file, with the arguments `ARGS` gives, for a row of the waits `WAITS` that starts
 *  with the child (`rowOf` of `harness-postgres-waits.ts`): it prints what the call answered, or
 *  the message of what it threw. The call is the subject of the row that runs the child, so the
 *  child's row does not charge it as a call that makes a container (`Row.made`): it runs to the
 *  deadline `WAITS` sets, and not for `makeMs` of `harness-postgres-waits.ts`. Where `BEFORE` is
 *  set (`{ what, ms }`), the row first waits `ms`, a wait it names `what` (`during` of
 *  `harness-postgres-waits.ts`), as a row awaits a start of its own. Where `RUN` is set it then
 *  runs a row's `finally`, as the leftover rows do: a stop that takes `STOP_MS`, which the row's
 *  `Waits.ends` counts, and then the removal of each container whose mark names `RUN`
 *  (`removeContainersOf`), and prints whether that removal ended or what it threw. */
const fixtureCallScript = `
import { docker, removeContainersOf } from ${JSON.stringify(leftoverFixtures)};
import { during, rowOf } from ${JSON.stringify(waitsModule)};
const { ARGS = "[]", BEFORE, RUN, STOP_MS, WAITS = "" } = process.env;
const row = { ...rowOf(JSON.parse(WAITS)), made() {} };
const ended = {};
try {
  if (BEFORE !== undefined) {
    const { what, ms } = JSON.parse(BEFORE);
    await during(what, () => Bun.sleep(ms));
  }
  ended.stdout = await docker(row, ...JSON.parse(ARGS));
} catch (error) {
  ended.error = error instanceof Error ? error.message : String(error);
}
if (RUN !== undefined) {
  await Bun.sleep(Number(STOP_MS));
  try {
    await removeContainersOf(row, RUN);
    ended.removed = true;
  } catch (error) {
    ended.removal = error instanceof Error ? error.message : String(error);
  }
}
console.log(JSON.stringify(ended));
`;

/** Runs the leftover rows' fixture `docker` with `args` in a child whose `docker` is the one in
 *  `bin` (`fixtureCallScript`), for a row of the waits `waits` that starts with the child: first,
 *  where `before` is given, a wait of `before.ms` that the row names `before.what`; then the call;
 *  and then, where `end` is given, that row's `finally`: a stop of `end.stopMs` and the removal of
 *  the containers whose mark names `end.run`. Charges `row` for the child. Answers what the call
 *  answered or threw, what the removal did, and the lines the child printed; kills the child and
 *  throws where it has not ended within everything the row waits for (`Row.ms`). */
export async function fixtureDockerInChild(
  row: Row,
  bin: string,
  args: readonly string[],
  waits: Waits,
  {
    before,
    end,
  }: {
    readonly before?: { readonly what: string; readonly ms: number };
    readonly end?: { readonly stopMs: number; readonly run: string };
  } = {}
) {
  row.child(`the fixture's docker ${args.join(" ")}`);
  const { stdout, stderr } = await inChild(
    fixtureCallScript,
    bin,
    {
      WAITS: JSON.stringify(waits),
      ARGS: JSON.stringify(args),
      ...(before === undefined ? {} : { BEFORE: JSON.stringify(before) }),
      ...(end === undefined ? {} : { RUN: end.run, STOP_MS: String(end.stopMs) }),
    },
    row
  );
  return {
    ended: JSON.parse(stdout) as {
      stdout?: string;
      error?: string;
      removed?: true;
      removal?: string;
    },
    lines: stderr.split("\n"),
  };
}

/** The id of the container the next rows remove, which their stand-in `docker` alone answers
 *  for: it names no container of the machine. */
export const standInId = "0".repeat(64);

/** The lines of `lines` that say the start waits for its removal of the container `id`. */
export function waitLines(lines: readonly string[], id: string): string[] {
  return holding(lines, `: waits for Docker's removal of container ${id.slice(0, 12)}`);
}

/** The room a row gives the stand-in's clock (`Tick`) on each side of a bound, in seconds: the
 *  start of one `bash` stand-in, which is when a call reaches that clock, and one timer of the
 *  start's that fires late, each under load. It is less than the 10 s of `rowBounds`, so a start
 *  that gives the removal up 7 s or more before its bound falls outside it, and so does a bound of
 *  twice the row's (20 s for 10 s). */
export const clockRoomS = 6;

/** Kills each `docker` stand-in a row's start ran that still runs, by its pid and start, and waits
 *  until each has ended, gone or a zombie by its state in `/proc`, as long as a row waits for one
 *  event of another process (`claimEventMs`); throws naming those that have not, so the row ends
 *  after them. */
export async function endStandIns(docker: StandInDocker): Promise<void> {
  const killed = [...docker.processes(), ...docker.running()].filter(({ pid, start }) =>
    marks.stillRuns(pid, start)
  );
  for (const { pid } of killed) process.kill(pid, "SIGKILL");
  const deadline = Date.now() + claimEventMs;
  for (
    let left = killed.filter(({ pid, start }) => marks.stillRuns(pid, start));
    left.length > 0;
    left = left.filter(({ pid, start }) => marks.stillRuns(pid, start))
  ) {
    if (Date.now() >= deadline) {
      throw new Error(
        `the docker stand-ins ${left.map(({ pid }) => pid).join(", ")} had not ended ${claimEventMs / 1000} s after their kill`
      );
    }
    await Bun.sleep(20);
  }
}
