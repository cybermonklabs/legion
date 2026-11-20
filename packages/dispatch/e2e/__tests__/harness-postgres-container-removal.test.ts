/** Rows of the check `v-no-leftover-postgres` (requirement R3 of pull request #4236): "The next
 *  start of a test stack removes each Postgres of a stack that it can show dead. It removes no
 *  Postgres of a stack that still runs. It leaves a Postgres whose stack it cannot judge, and it
 *  names that Postgres in one line." These are the rows of a start's removal of a dead stack's container
 *  (`removeContainer` of `e2e/harness-postgres-container-removal.ts`), one container in a child
 *  with a stand-in `docker` that runs no real one (`standInDocker`), or on a machine whose clock
 *  the row moves (`rowMachine`) where the row requires how a removal ends at its bound, which on
 *  the real clock depends on how fast the stand-in answers; and of the start's sweep of two such
 *  containers. The rows that run a child need Linux (`/proc`). */

import { afterAll, expect, spyOn, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { removalBounds, removeContainer } from "../harness-postgres-container-removal";
import * as marks from "../harness-postgres-marks";
import {
  clockRoomS,
  endStandIns,
  removeInChild,
  standInDocker,
  standInId,
  waitLines,
} from "./harness-postgres-docker-fixtures";
import {
  holding,
  leaveData,
  owners,
  plantsRoot,
  scratch,
  sweepInChild,
  until,
  writeMark,
} from "./harness-postgres-removal-fixtures";
import { type RowAnswer, type RowCall, rowMachine } from "./harness-postgres-row-machine";
import {
  claimEventMs,
  neverEnds,
  type Row,
  rowMs,
  rowOf,
  type Waits,
} from "./harness-postgres-waits";
import { noHookLimit, removeScratch } from "./step-end";

afterAll(
  () => removeScratch([plantsRoot()], rowMs({ children: 1, reads: 0, bounds: [] })),
  // The file's own directory of plants (`plantsRoot`), which each row empties, goes in a process of
  // its own, which may take what the CI step leaves, or one child's limit, and the hook has no
  // limit of Bun's: step-end.ts says why.
  noHookLimit
);

const onLinux = process.platform === "linux";

/** The bounds of a start's removal of a container in the next rows (`RemovalBounds`), each the
 *  row's own and a few seconds, through the same parameter a start's removal reads its own from
 *  (`removalBounds`), so each row runs in seconds and not in the start's own minutes. One ask of
 *  Docker may go unanswered for 3 s: a stand-in `docker`, a bash script, answers in well under a
 *  second, so only a load that held one such script for 3 s could fail a row by it. The start
 *  asks every second and waits 10 s in all. A start that ignored these bounds and kept its own
 *  would wait 600 s and fail each row at its limit; a fixed bound of any length past 10 s never
 *  fires within these rows: one short of the start's own 600 s fails the row that runs the start's
 *  own bounds on a clock the row moves (`rowMachine`), and one on the real clock fails the row that
 *  reads the removal's source. */
const rowBounds = { answerMs: 3_000, boundMs: 10_000, watchMs: 1_000 };

/** What a row waits for that runs one start's removal of a container in a child: the child, the
 *  removal's bound, and the end of each `docker` the start ran (`Waits`). */
const oneRemoval: Waits = { children: 1, reads: 0, bounds: [rowBounds.boundMs, claimEventMs] };

/** How long the next row's stand-in `docker rm` takes: twice as long as one ask may go
 *  unanswered, so a start that bounds its removal by that time gives it up long before, and 4 s
 *  short of the removal's bound, so a start that waits for Docker ends it well inside. */
const removalSeconds = (2 * rowBounds.answerMs) / 1000;

/** A stand-in `docker ps` of one container that lists it, as Docker lists a container while it
 *  removes it: `docker ps --all --quiet --no-trunc --filter id=<id>` prints the id. */
const listsIt = `  ps) echo "\${6#id=}" ;;`;

/** A stand-in `docker rm` that Docker refuses, as it refuses a removal while another removal of
 *  the container is in progress. */
const inProgress =
  '  rm) echo "Error response from daemon: removal of container $4 is already in progress" >&2; exit 1 ;;';

// A daemon that many sessions share took 74 s and 91 s over one `docker rm` while it answered
// every other call within a second. The start waits while Docker answers each ask, says so once
// the removal has run as long as one ask may go unanswered, and says it removed the container
// once Docker ends the removal.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start waits for its removal of a dead stack's container as long as Docker takes over it while Docker answers, says so once it has waited as long as one ask may take, and then says it removed it",
  async () => {
    const row = rowOf(oneRemoval);
    const { tmp } = scratch();
    try {
      const docker = standInDocker(tmp, [
        'case "$1" in',
        `  rm) sleep ${removalSeconds}; echo "$4" ;;`,
        listsIt,
        "esac",
      ]);

      const { removal, lines, seconds } = await removeInChild(
        row,
        docker.bin,
        standInId,
        rowBounds
      );

      expect(removal).toEqual({ kind: "removed" });
      expect(seconds).toBeGreaterThan(removalSeconds);
      expect(docker.calls().filter((call) => call.startsWith("rm "))).toEqual([
        `rm --force --volumes ${standInId}`,
      ]);
      expect(waitLines(lines, standInId)).toHaveLength(1);
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one child and its removal.
  rowMs(oneRemoval)
);

// A daemon that hangs answers neither the removal nor the asks beside it: the start gives the
// removal up at the first ask that goes unanswered, and stops both calls it made.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start gives up its removal of a dead stack's container where Docker answers no ask while it runs, says so, and leaves no docker it started running",
  async () => {
    const row = rowOf(oneRemoval);
    const { tmp } = scratch();
    const docker = standInDocker(tmp, [neverEnds(row)]);
    try {
      const { removal } = await removeInChild(row, docker.bin, standInId, rowBounds);

      expect(removal).toEqual({
        kind: "failed",
        reason: `Docker answered no docker ps within ${rowBounds.answerMs / 1000} s while this start removed it`,
      });
      // Each `docker` the start ran ends with the start's removal: the stand-in's `sleep`, which
      // keeps its path for a name, runs on to the row's end where no signal reached it.
      await until(
        row,
        () => docker.running().length === 0,
        "the end of each docker the start ran",
        claimEventMs
      );
    } finally {
      await endStandIns(docker);
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one child and its removal,
  // and then the end of each `docker` it ran.
  rowMs(oneRemoval)
);

// A `docker ps` that exits other than 0, as one gives where the daemon cannot be reached, is no
// answer: the start gives its removal up at that ask and says why, and does not wait on.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start reads a docker ps that fails as no answer: it gives up its removal of a dead stack's container at that ask, says why, and leaves no docker it started running",
  async () => {
    const row = rowOf(oneRemoval);
    const { tmp } = scratch();
    const refused =
      "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?";
    const docker = standInDocker(tmp, [
      'case "$1" in',
      `  rm) ${neverEnds(row)} ;;`,
      `  ps) echo ${JSON.stringify(refused)} >&2; exit 1 ;;`,
      "esac",
    ]);
    try {
      const { removal } = await removeInChild(row, docker.bin, standInId, rowBounds);

      expect(removal).toEqual({
        kind: "failed",
        reason: `docker ps failed (${refused}) while this start removed it`,
      });
      await until(
        row,
        () => docker.running().length === 0,
        "the end of each docker the start ran",
        claimEventMs
      );
    } finally {
      await endStandIns(docker);
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one child and its removal,
  // and then the end of each `docker` it ran.
  rowMs(oneRemoval)
);

/** The bounds of the next rows (`RemovalBounds`): an ask may go unanswered three minutes, and the
 *  removal waits 4 s in all, asking every second, so a stand-in `docker ps` that answers after
 *  150 s answers within the ask's own limit and long after the bound. */
const slowAskBounds = { answerMs: 180_000, boundMs: 4_000, watchMs: 1_000 };

/** What one of the next rows waits for (`Waits`): one child, whose removal may run to the ask's
 *  limit where the bound does not cut it short, and then the end of each `docker` it ran. */
const slowAsk: Waits = { children: 1, reads: 0, bounds: [slowAskBounds.answerMs, claimEventMs] };

// The bound is a deadline: an ask that is still out when the bound comes ends there, and does not
// carry the removal on to its own limit. The stand-in's `docker ps` answers after 150 s, within the
// ask's 180 s, and the bound is 4 s. Its clock times the start's end, the last end of a call it
// ran: a start that gave the ask its own limit ends at the answer, 150 s after the bound.
for (const docker of [
  { removal: "never ends", rm: (row: Row) => `  rm) ${neverEnds(row)} ;;` },
  { removal: "is refused as already in progress", rm: () => inProgress },
] as const) {
  test.skipIf(!onLinux)(
    `v-no-leftover-postgres: a start whose removal of a dead stack's container ${docker.removal} ends it at its bound while its ask of Docker is still out: no ask carries the removal past its bound`,
    async () => {
      const row = rowOf(slowAsk);
      const { tmp } = scratch();
      const standIn = standInDocker(tmp, [
        'case "$1" in',
        docker.rm(row),
        `  ps) sleep 150 & wait $!; echo "\${6#id=}" ;;`,
        "esac",
      ]);
      try {
        const { removal } = await removeInChild(row, standIn.bin, standInId, slowAskBounds);

        expect(removal).toEqual({
          kind: "failed",
          reason:
            docker.removal === "is refused as already in progress"
              ? `Error response from daemon: removal of container ${standInId} is already in progress, and Docker had not answered whether it still lists it when the ${slowAskBounds.boundMs / 1000} s bound of this start's removal came`
              : `Docker did not end the removal within ${slowAskBounds.boundMs / 1000} s, and it had not answered the last ask when that bound came`,
        });
        await until(
          row,
          () => standIn.running().length === 0,
          "the end of each docker the start ran",
          claimEventMs
        );
        const ticks = standIn.clock();
        const began = ticks.find((tick) => tick.call.startsWith("rm "))?.at ?? Number.NaN;
        const ended = Math.max(
          ...ticks.filter((tick) => tick.event === "end").map((tick) => tick.at)
        );
        expect(ended - began).toBeLessThan(slowAskBounds.boundMs / 1000 + clockRoomS);
      } finally {
        await endStandIns(standIn);
        rmSync(tmp, { recursive: true, force: true });
      }
    },
    // What the row waits for, summed by the fixtures' program (`rowMs`): one child and its
    // removal, up to the ask's own limit, and then the end of each `docker` it ran.
    rowMs(slowAsk)
  );
}

/** How many asks the next row's machine lists the container for: one more than the asks of one
 *  pause each (`watchMs`) that fit in the time after which the start says that it waits
 *  (`answerMs`), so the start has said so once before Docker lists the container no more. */
const listedAsks = rowBounds.answerMs / rowBounds.watchMs + 1;

/** Docker's refusal of a `docker rm` of the container `standInId` while another removal of it is
 *  in progress. */
const inProgressRefusal = `Error response from daemon: removal of container ${standInId} is already in progress`;

/** The line a start prints once its removal of the container `standInId` has run `ran` s. */
function waitsLine(ran: number, boundMs: number): string {
  return `: waits for Docker's removal of container ${standInId.slice(0, 12)}: the removal has run ${ran} s and Docker answers each ask, so the start waits for it, at most ${boundMs / 1000} s in all`;
}

/** Runs a start's removal of the container `standInId` within `bounds` on a machine whose clock
 *  the row moves (`rowMachine`), and whose `docker` answers as `answer` says, to the removal's
 *  bound at most. Answers how the removal ended, the time of that clock it ended at, each line it
 *  printed with the time of that clock, and each call the machine got (`RowCall`): each time is
 *  the removal's own, so no load on the row moves one. */
async function removeOnRowClock(
  answer: (args: readonly string[]) => RowAnswer,
  bounds: typeof removalBounds
) {
  const docker = rowMachine(answer);
  const said: [number, string][] = [];
  const lines = spyOn(console, "error").mockImplementation((...words: unknown[]) => {
    said.push([docker.machine.now(), words.join(" ")]);
  });
  try {
    const removal = await docker.drive(
      removeContainer(standInId, undefined, bounds, docker.machine),
      { horizonMs: bounds.boundMs }
    );
    return { removal, at: docker.machine.now(), said, calls: docker.calls() };
  } finally {
    lines.mockRestore();
  }
}

/** The times of the asks of `calls`, a machine's (`RowCall`): each `docker ps` of the container. */
function askTimes(calls: readonly RowCall[]): number[] {
  return calls.filter((call) => call.args.startsWith("ps ")).map((call) => call.at);
}

// Another start's removal of the container, which Docker still runs: Docker refuses this start's
// removal, and the start sends none again. It asks whether Docker still lists the container, and
// says another start removed it once Docker lists it no more. The machine lists it for
// `listedAsks` asks, one a second on a clock the row moves (`rowMachine`), so the fifth ask finds
// it gone at 5 s of the removal whatever the load: on the real clock, a slow `docker` stand-in
// moved that ask toward the 10 s bound.
test("v-no-leftover-postgres: a start whose removal of a dead stack's container Docker refuses while another removal of it is in progress sends no docker rm again, waits for that removal while Docker answers, says so, and then says another start removed it", async () => {
  let asks = 0;
  const { removal, at, said, calls } = await removeOnRowClock((args) => {
    if (args[0] === "rm") return { after: 0, stderr: `${inProgressRefusal}\n` };
    asks += 1;
    return { after: 0, stdout: asks <= listedAsks ? `${standInId}\n` : "" };
  }, rowBounds);

  expect({ removal, at }).toEqual({
    removal: { kind: "elsewhere", reason: inProgressRefusal },
    at: (listedAsks + 1) * rowBounds.watchMs,
  });
  expect(calls.filter((call) => call.args.startsWith("rm "))).toHaveLength(1);
  expect(askTimes(calls)).toEqual(
    Array.from({ length: listedAsks + 1 }, (_, index) => (index + 1) * rowBounds.watchMs)
  );
  expect(said).toEqual([
    [rowBounds.answerMs, expect.stringContaining(waitsLine(3, rowBounds.boundMs))],
  ]);
});

// Docker refuses a second removal from the moment the first marks the container's removal in
// progress, and goes on refusing while that removal kills the container: moby v28.5.2 marks it at
// `daemon/delete.go:40-44`, before `:91-101` kills a container that runs, through a kill that
// containerd is given with no deadline (`daemon/kill.go:111`), and `docker inspect` reads the
// container as `running` until its process has ended (`container/state.go:121-136`). So a start
// that another start's stuck removal holds gets "already in progress" and `running` for as long as
// the daemon takes. It sends no `docker rm` again, asks no `docker inspect` at all, says that it
// waits once the removal has run as long as one ask may take, and gives it up at its bound, on one
// line that names the container. The row's clock (`rowMachine`) ends the removal at its bound with
// that line whatever the load: on the real clock, an ask that a slow `docker` stand-in carried
// into the bound's last second ended it with another line.
test("v-no-leftover-postgres: a start whose removal of a dead stack's container Docker refuses as already in progress while docker inspect reads the container running sends no docker rm again, says that it waits, and gives the removal up at its bound", async () => {
  const { removal, at, said, calls } = await removeOnRowClock(
    (args) =>
      args[0] === "rm"
        ? { after: 0, stderr: `${inProgressRefusal}\n` }
        : args[0] === "inspect"
          ? { after: 0, stdout: "running\n" }
          : { after: 0, stdout: `${standInId}\n` },
    rowBounds
  );

  expect({ removal, at }).toEqual({
    removal: {
      kind: "failed",
      reason: `${inProgressRefusal}, and Docker still lists it ${rowBounds.boundMs / 1000} s after this start's removal began`,
    },
    at: rowBounds.boundMs,
  });
  expect(calls.filter((call) => call.args.startsWith("rm "))).toHaveLength(1);
  expect(calls.filter((call) => call.args.startsWith("inspect"))).toEqual([]);
  expect(askTimes(calls)).toEqual(
    Array.from(
      { length: rowBounds.boundMs / rowBounds.watchMs - 1 },
      (_, index) => (index + 1) * rowBounds.watchMs
    )
  );
  expect(said).toEqual([
    [rowBounds.answerMs, expect.stringContaining(waitsLine(3, rowBounds.boundMs))],
  ]);
});

// The start's own bounds: a removal that Docker never ends, while it answers each ask at once, runs
// the 600 s of the start's bound and no less, and says that it waits at 60 s. The row runs it on a
// clock it moves (`rowMachine`), which moves only while the removal waits on it, so the 600 s take
// a moment of real time, and each time the row reads is the removal's own, which no load
// stretches. A fixed limit of the removal short of 600 s, on that clock or as the `timeout` of its
// `docker rm`, ends the removal sooner, with another reason; a limit on the real clock is the next
// row's.
test("v-no-leftover-postgres: a start whose removal of a dead stack's container Docker never ends, within the start's own bounds, says that it waits at 60 s and gives the removal up at 600 s and no sooner, on a clock the row moves", async () => {
  const { removal, said, calls } = await removeOnRowClock(
    (args) => (args[0] === "ps" ? { after: 0, stdout: `${standInId}\n` } : "never"),
    removalBounds
  );

  expect(removal).toEqual({
    kind: "failed",
    reason: "Docker did not end the removal within 600 s, while it answered each ask",
  });
  expect(said).toEqual([[60_000, expect.stringContaining(waitsLine(60, 600_000))]]);
  expect(calls.filter((call) => call.args.startsWith("rm "))).toEqual([
    { args: `rm --force --volumes ${standInId}`, at: 0, ended: 600_000, how: "aborted" },
  ]);
  // One ask every 5 s from the first pause to the bound, each answered at once.
  expect(askTimes(calls)).toEqual(Array.from({ length: 119 }, (_, index) => (index + 1) * 5_000));
});

// Another start's removal that Docker never ends, within the start's own bounds: Docker refuses
// this start's `docker rm` as already in progress, and lists the container at each ask, as moby
// does while the other removal's kill of a container that runs waits on containerd (the row of
// `docker inspect` reading `running` above names the source lines). On a clock the row moves
// (`rowMachine`), the start sends no `docker rm` again, asks every 5 s, says that it waits at 60 s
// and gives the container up at 600 s and no sooner. A wait of the other removal with no bound, a
// second `docker rm`, no line, or a bound of any other length fails it.
test("v-no-leftover-postgres: a start whose docker rm of a dead stack's container Docker refuses as already in progress, while Docker lists it at every ask, within the start's own bounds, sends no docker rm again, says that it waits at 60 s and gives the container up at 600 s and no sooner, on a clock the row moves", async () => {
  const { removal, said, calls } = await removeOnRowClock(
    (args) =>
      args[0] === "rm"
        ? { after: 0, stderr: `${inProgressRefusal}\n` }
        : { after: 0, stdout: `${standInId}\n` },
    removalBounds
  );

  expect(removal).toEqual({
    kind: "failed",
    reason: `${inProgressRefusal}, and Docker still lists it 600 s after this start's removal began`,
  });
  expect(said).toEqual([[60_000, expect.stringContaining(waitsLine(60, 600_000))]]);
  expect(calls.filter((call) => call.args.startsWith("rm "))).toEqual([
    { args: `rm --force --volumes ${standInId}`, at: 0, ended: 0, how: "answered" },
  ]);
  // One ask every 5 s from the first pause to the bound, each answered at once.
  expect(askTimes(calls)).toEqual(Array.from({ length: 119 }, (_, index) => (index + 1) * 5_000));
});

// Each pause between two asks is cut to what is left of the bound (`within`), so a removal gives up
// at its bound exactly, and also where the bound is no whole number of pauses: with an ask every
// 7 s on the start's 600 s, the last pause is 5 s. The two rows above cannot see a pause that takes
// its whole length, since 600 s is a whole number of their 5 s pauses; with 7 s such a pause would
// run the removal 2 s past its bound, on a clock the row moves (`rowMachine`), whose horizon, the
// bound, ends it there, and these rows fail.
for (const [docker, answer, reason] of [
  [
    "never ends",
    (args: readonly string[]): RowAnswer =>
      args[0] === "ps" ? { after: 0, stdout: `${standInId}\n` } : "never",
    "Docker did not end the removal within 600 s, while it answered each ask",
  ],
  [
    "refuses as already in progress",
    (args: readonly string[]): RowAnswer =>
      args[0] === "rm"
        ? { after: 0, stderr: `${inProgressRefusal}\n` }
        : { after: 0, stdout: `${standInId}\n` },
    `${inProgressRefusal}, and Docker still lists it 600 s after this start's removal began`,
  ],
] as const) {
  test(`v-no-leftover-postgres: a start whose removal of a dead stack's container Docker ${docker}, asking every 7 s, gives the container up at 600 s exactly and no later, on a clock the row moves`, async () => {
    const { removal, at, calls } = await removeOnRowClock(answer, {
      ...removalBounds,
      watchMs: 7_000,
    });

    expect({ removal, at }).toEqual({ removal: { kind: "failed", reason }, at: 600_000 });
    // An ask every 7 s from the first pause, the last at 595 s, and then the last 5 s.
    expect(askTimes(calls)).toEqual(Array.from({ length: 85 }, (_, index) => (index + 1) * 7_000));
  });
}

/** What the next rows wait for (`Waits`): one sweep in a child, whose removals of containers run
 *  on a clock the child moves and so take no time of the row's, and three holder reads (the next
 *  container's socket directory, and the run directory's data and socket directories). */
const sweepOfTwo: Waits = { children: 1, reads: 3, bounds: [] };

/** Two containers a sweep lists, each with the mark of a stack that is gone, in the listing file
 *  `listing` of `tmp`, and a data directory with its mark in `run`: the first container's removal
 *  is the row's case, and the second, which the start removes, shows that the sweep goes on. The
 *  stand-in `docker` that `lists` holds answers the sweep's listing; `answers` is the table of the
 *  machine each removal runs on (`tableAnswer`), which answers the first container's `docker rm`
 *  with `stuck` and lists it at each ask, and removes the second. */
function twoContainers(tmp: string, run: string) {
  const gone = owners().gone;
  const stuck = { id: "1".repeat(64), name: "dispatch-e2e-pg-stuck-0" };
  const next = { id: "2".repeat(64), name: "dispatch-e2e-pg-next-1" };
  const listing = join(tmp, "listing");
  writeFileSync(
    listing,
    [stuck, next]
      .map(
        ({ id, name }) => `${id} ${name} ${JSON.stringify(marks.containerMark(gone, run, name))}\n`
      )
      .join("")
  );
  const data = join(run, "worker-0", "pgdata");
  leaveData(data);
  const file = writeMark(run, gone, data, mkdtempSync(join(tmp, "no-bin-")));
  const lists = `  ps) cat ${JSON.stringify(listing)} ;;`;
  const answers = (rm: RowAnswer): Record<string, RowAnswer> => ({
    [`rm --force --volumes ${stuck.id}`]: rm,
    [`ps --all --quiet --no-trunc --filter id=${stuck.id}`]: { after: 0, stdout: `${stuck.id}\n` },
    [`rm --force --volumes ${next.id}`]: { after: 0, stdout: `${next.id}\n` },
  });
  return { by: `which stack ${gone.pid} left`, stuck, next, data, file, lists, answers };
}

/** The calls a sweep of `twoContainers` makes of its removals' machine, as `how` the first
 *  container's `docker rm` ends: that `docker rm` at 0, an ask a second until its bound, and then
 *  the next container's `docker rm`, at the bound, on the machine's clock. */
function callsOfTwo(
  stuck: string,
  next: string,
  how: RowCall["how"]
): { args: string; at: number; how: RowCall["how"] }[] {
  const answered: RowCall["how"] = "answered";
  return [
    { args: `rm --force --volumes ${stuck}`, at: 0, how },
    ...Array.from({ length: rowBounds.boundMs / rowBounds.watchMs - 1 }, (_, index) => ({
      args: `ps --all --quiet --no-trunc --filter id=${stuck}`,
      at: (index + 1) * rowBounds.watchMs,
      how: answered,
    })),
    { args: `rm --force --volumes ${next}`, at: rowBounds.boundMs, how: answered },
  ];
}

// A removal that Docker never ends while it answers each ask, as of a container whose removal is
// stuck in the daemon: the start says that it waits, gives the removal up at its bound on one line
// that names the container, and goes on, to the next container and the run directories. The
// removals run on a clock the sweep's child moves (`rowMachine`), so the removal ends at its bound
// with that line whatever the load: on the real clock, an ask that a slow `docker` stand-in
// carried into the bound's last second ended it with another line: 1 run of 20 in CI's repeat
// workflow on 2026-11-20, and a stand-in slowed by 0.10 s to 0.45 s on a shared Linux machine.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start whose removal of a dead stack's container Docker never ends, while Docker answers, says that it waits, gives the removal up at its bound on one line that names the container, and goes on to the next container and the run directories",
  async () => {
    const row = rowOf(sweepOfTwo);
    const { tmp, run } = scratch();
    const { by, stuck, next, data, file, lists, answers } = twoContainers(tmp, run);
    const docker = standInDocker(tmp, ['case "$1" in', lists, "esac"]);
    try {
      const { lines, calls } = await sweepInChild(
        row,
        tmp,
        docker.bin,
        rowBounds,
        answers("never")
      );

      expect(waitLines(lines, stuck.id)).toEqual([
        expect.stringContaining(`container ${stuck.id.slice(0, 12)} (${stuck.name}), ${by}:`),
      ]);
      expect(holding(lines, `container ${stuck.id.slice(0, 12)} (`)).toContainEqual(
        expect.stringContaining(
          `: container ${stuck.id.slice(0, 12)} (${stuck.name}), ${by}, is not removed: Docker did not end the removal within ${rowBounds.boundMs / 1000} s, while it answered each ask`
        )
      );
      expect(
        holding(lines, `removed container ${next.id.slice(0, 12)} (${next.name})`)
      ).toHaveLength(1);
      const real = marks.identityOf(run);
      expect([existsSync(data), existsSync(file)]).toEqual([false, false]);
      expect(
        holding(lines, `removed ${join(real, "worker-0", "pgdata")} and ${real}/pg-0-`)
      ).toHaveLength(1);
      expect(calls.map(({ args, at, how }) => ({ args, at, how }))).toEqual(
        callsOfTwo(stuck.id, next.id, "aborted")
      );
    } finally {
      await endStandIns(docker);
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep and its three
  // holder reads.
  rowMs(sweepOfTwo)
);

// Every answer of Docker about the first container is that another removal of it is in progress:
// the start sends no `docker rm` of it again, says that it waits, gives it up at its bound on one
// line that names the container, and goes on to the next container and the run directories, on a
// clock the sweep's child moves (`rowMachine`), as the row above.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start whose every removal of a dead stack's container Docker refuses as already in progress says that it waits, gives the container up at its bound on one line that names it, and goes on to the next container and the run directories",
  async () => {
    const row = rowOf(sweepOfTwo);
    const { tmp, run } = scratch();
    const { by, stuck, next, data, file, lists, answers } = twoContainers(tmp, run);
    const docker = standInDocker(tmp, ['case "$1" in', lists, "esac"]);
    const refusal = `Error response from daemon: removal of container ${stuck.id} is already in progress`;
    try {
      const { lines, calls } = await sweepInChild(
        row,
        tmp,
        docker.bin,
        rowBounds,
        answers({ after: 0, stderr: `${refusal}\n` })
      );

      expect(waitLines(lines, stuck.id)).toHaveLength(1);
      expect(holding(lines, `container ${stuck.id.slice(0, 12)} (`)).toContainEqual(
        expect.stringContaining(
          `: container ${stuck.id.slice(0, 12)} (${stuck.name}), ${by}, is not removed: ${refusal}, and Docker still lists it ${rowBounds.boundMs / 1000} s after this start's removal began`
        )
      );
      expect(
        holding(lines, `removed container ${next.id.slice(0, 12)} (${next.name})`)
      ).toHaveLength(1);
      expect([existsSync(data), existsSync(file)]).toEqual([false, false]);
      expect(calls.map(({ args, at, how }) => ({ args, at, how }))).toEqual(
        callsOfTwo(stuck.id, next.id, "answered")
      );
    } finally {
      await endStandIns(docker);
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep and its three
  // holder reads.
  rowMs(sweepOfTwo)
);
