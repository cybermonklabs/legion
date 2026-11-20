/** Rows of the check `v-no-leftover-postgres` (requirement R3 of pull request #4236): "The next
 *  start of a test stack removes each Postgres of a stack that it can show dead. It removes no
 *  Postgres of a stack that still runs. It leaves a Postgres whose stack it cannot judge, and it
 *  names that Postgres in one line." These are the rows of a run directory of another `TMPDIR`, which a start finds
 *  through the kernel's processes, and of two starts at once on one dead stack, which claim its
 *  parts (`claimForRemoval`); the rows of the marks a start reads in its own `TMPDIR` are in
 *  `harness-postgres-removal.test.ts`.
 *
 *  ANOTHER START BESIDE A ROW. A dead stack that a row plants is one every start must remove, and
 *  any start of this user may run beside the row: another session's on the machine, or
 *  `harness-stacks.test.ts`'s in the same `bun test --parallel` run. The rows of a run directory
 *  of another TMPDIR, which a start finds by its walk of the kernel's processes alone, plant a
 *  `postgres` that every start finds, and so decide on what any start did with the plant
 *  (`removedByAStart`). The other way round, a row's own sweep walks for run directories only in
 *  the file's own directory (`plantsRoot`), so it judges no stack that a row of another file, or
 *  of another session, planted beside it. They need Linux (`/proc`). */

import { afterAll, expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import type { Subprocess } from "bun";
import * as marks from "../harness-postgres-marks";
import { servedElsewhere } from "../harness-postgres-processes";
import { claimWaitMs, stopPostmaster } from "../harness-postgres-removal";
import {
  claimAndDie,
  claimInChild,
  endChild,
  holding,
  leaveData,
  leaveElsewhere,
  owners,
  plantsRoot,
  scratch,
  signalCountingPostmaster,
  standInPostmaster,
  startSweep,
  sweep,
  until,
  writeMark,
} from "./harness-postgres-removal-fixtures";
import { type Row, rowMs, rowOf, type Waits, waitsMs } from "./harness-postgres-waits";
import { type StandIn, untilThisEnds } from "./planted-marks";
import { noHookLimit, removeScratch } from "./step-end";

afterAll(
  () => removeScratch([plantsRoot()], rowMs({ children: 1, reads: 0, bounds: [] })),
  // The file's own directory of plants (`plantsRoot`), which each row empties, goes in a process of
  // its own, which may take what the CI step leaves, or one child's limit, and the hook has no
  // limit of Bun's: step-end.ts says why.
  noHookLimit
);

const { runDirectoryPrefix } = marks;

const onLinux = process.platform === "linux";

/** What a row waits for that runs one start's sweep, which claims nothing, so it reads no holder,
 *  and waits for nothing else (`Waits`). */
const oneSweep: Waits = { children: 1, reads: 0, bounds: [] };

/** How long another start's removal of a plant may still take once the row's own start ended: a
 *  start that found the plant claimed waited for that claim, so only the last steps of the other
 *  start's removal, its stop of a stand-in and a few file removals, are left. */
const settleMs = 60_000;

/** What any start did with a plant that every start finds, `postmaster` serving from `data`, or
 *  from `claimed` where a dead start claimed `data`, with its mark `file` (`leaveElsewhere`),
 *  whose stack is gone: R3 asks each start to remove it, and the row's own start or another may
 *  be the one. Waits until the postmaster has ended and `data`, `claimed` and `file` are gone, and
 *  requires of `lines`, what one start printed, that each line naming `named` says this start
 *  removed the postmaster of `data`, or that another start removed it first, or that this start
 *  waits for another start's claim of it, and that at most one says this start removed it: none
 *  says it left it. */
async function removedByAStart(
  row: Row,
  lines: readonly string[],
  plant: {
    readonly postmaster?: StandIn;
    readonly data: string;
    readonly claimed?: string;
    readonly file?: string;
  },
  named = plant.data
): Promise<void> {
  await until(
    row,
    () =>
      plant.postmaster?.alive() !== true &&
      [plant.data, plant.claimed, plant.file].every(
        (path) => path === undefined || !existsSync(path)
      ),
    "the end of the plant's postmaster, data directory and mark",
    settleMs
  );
  const removed = `removed the postmaster of ${plant.data}`;
  const naming = holding(lines, named);
  const said = ["another start removed", "waits for another start's claim", removed];
  expect(naming.filter((line) => !said.some((words) => line.includes(words)))).toEqual([]);
  expect(holding(naming, removed).length).toBeLessThanOrEqual(1);
}

/** What a row waits for that runs one start's sweep of a stack elsewhere whose postmaster serves,
 *  and then for what another start may still be removing of it (`settleMs`): the sweep claims the
 *  data directory, stops the postmaster and claims the socket directory, a holder read before each
 *  claim, and one stop, with its reads after it (`Waits`). */
const sweepStopping: Waits = { children: 1, reads: 2, stops: 1, bounds: [settleMs] };

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a stack under another TMPDIR that the kernel shows serving and whose stack is gone is removed by the same steps: its postmaster ends, its data and socket directories and its mark go",
  async () => {
    const row = rowOf(sweepStopping);
    const { tmp } = scratch();
    const left = leaveElsewhere(owners().gone);
    try {
      await removedByAStart(row, await sweep(row, tmp), left);
    } finally {
      await left.postmaster?.stop();
      rmSync(left.other, { recursive: true, force: true });
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep, its holder reads
  // and its stop of a postmaster, and then `settleMs`.
  rowMs(sweepStopping)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start whose TMPDIR no longer exists still removes a stack under another TMPDIR that the kernel shows serving and whose stack is gone, and names the TMPDIR it cannot read on one line",
  async () => {
    const row = rowOf(sweepStopping);
    const { tmp } = scratch();
    const programs = mkdtempSync(join(tmp, "no-programs-"));
    const gone = join(tmp, "tmpdir-gone");
    const left = leaveElsewhere(owners().gone);
    try {
      const lines = await sweep(row, gone, programs);

      await removedByAStart(row, lines, left);
      expect(holding(lines, `left what it cannot read: ${gone} (ENOENT`)).toHaveLength(1);
    } finally {
      await left.postmaster?.stop();
      rmSync(left.other, { recursive: true, force: true });
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep, its holder reads
  // and its stop of a postmaster, and then `settleMs`.
  rowMs(sweepStopping)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a stack under another TMPDIR whose stack runs is not touched, and the start prints nothing of it",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp } = scratch();
    const left = leaveElsewhere(owners().runs);
    try {
      const lines = await sweep(row, tmp);

      expect(left.postmaster?.alive()).toBe(true);
      expect([
        existsSync(join(left.data, "PG_VERSION")),
        left.file !== undefined && existsSync(left.file),
      ]).toEqual([true, true]);
      expect(holding(lines, left.run)).toEqual([]);
    } finally {
      await left.postmaster?.stop();
      rmSync(left.other, { recursive: true, force: true });
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a postmaster of this user under a run directory of another TMPDIR that no mark names is left and named",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp } = scratch();
    const left = leaveElsewhere();
    try {
      const lines = await sweep(row, tmp);

      expect(left.postmaster?.alive()).toBe(true);
      expect(existsSync(join(left.data, "PG_VERSION"))).toBe(true);
      expect(
        holding(
          lines,
          `${left.data} (postmaster ${left.postmaster?.pid} serves it from a run directory this start does not list, and no mark there names it)`
        )
      ).toHaveLength(1);
    } finally {
      await left.postmaster?.stop();
      rmSync(left.other, { recursive: true, force: true });
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

// A row's sweep looks for run directories elsewhere only in its file's own directory
// (`plantsRoot`, as `servedUnder`), so that it judges no stack that a row of another file, or of
// another session, planted beside it: two files' rows run at once read each other's plants, and
// failed 13 rows of CI's repeat workflow, on 2026-11-20. Rows that run alone cannot see a sweep
// that looks at the whole machine again, since nothing else is planted then; this row plants a
// postmaster that no mark names beside the file's own directory, which every start leaves and
// names, and requires that the row's sweep names it not, while it names the same plant inside
// the file's own directory, so a sweep that looked nowhere fails it too.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a row's sweep reads the run directories of its own file's directory and of no other: a postmaster that no mark names beside that directory is not named, and one inside it is",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp } = scratch();
    const sibling = realpathSync(mkdtempSync(join(dirname(plantsRoot()), "marks-sibling-")));
    const inside = leaveElsewhere();
    const beside = leaveElsewhere(undefined, true, sibling);
    try {
      const lines = await sweep(row, tmp);

      expect(holding(lines, `${inside.data} (postmaster ${inside.postmaster?.pid} `)).toHaveLength(
        1
      );
      expect(holding(lines, sibling)).toEqual([]);
      expect([beside.postmaster?.alive(), existsSync(join(beside.data, "PG_VERSION"))]).toEqual([
        true,
        true,
      ]);
    } finally {
      await inside.postmaster?.stop();
      await beside.postmaster?.stop();
      rmSync(inside.other, { recursive: true, force: true });
      rmSync(sibling, { recursive: true, force: true });
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

// A start, unlike a row, looks for run directories elsewhere on the whole machine: its sweep's
// `servedUnder` is "/" by default (`removeLeftoverPostgres`), and passes it on to the walk of the
// kernel's processes (`servedElsewhere`); the source rules hold that default to "/" and every
// production call of the sweep to no argument. No row can run that sweep in CI without judging
// what every other row planted, so this row runs the walk under "/": it lists a postmaster
// planted beside the file's own directory, where the walk a row's sweep makes, under that
// directory, does not.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start's walk of the kernel's processes under its sweep's default, /, looks for run directories on the whole machine: it lists a postmaster served beside the file's own directory, which a row's walk under that directory does not",
  async () => {
    const sibling = realpathSync(mkdtempSync(join(dirname(plantsRoot()), "marks-sibling-")));
    const beside = leaveElsewhere(undefined, true, sibling);
    try {
      const real = realpathSync(tmpdir());
      const uid = process.getuid?.() ?? -1;
      const runs = (served: readonly { run: string }[]) => served.map(({ run }) => run);
      expect(runs(servedElsewhere(real, uid, "/"))).toContain(realpathSync(beside.run));
      expect(runs(servedElsewhere(real, uid, plantsRoot()))).not.toContain(
        realpathSync(beside.run)
      );
    } finally {
      await beside.postmaster?.stop();
      rmSync(sibling, { recursive: true, force: true });
    }
  }
);

/** What the next row waits for (`Waits`): two sweeps, each a child; the first's holder reads of
 *  the data directory before its claim, and of the socket directory, and the second's of the
 *  socket directory, where it gets there first; the first's stop of the postmaster, with its reads
 *  after it; and the second's wait for the first's claim. */
const twoOnOne: Waits = { children: 2, reads: 3, stops: 1, bounds: [claimWaitMs] };

// Two starts at once on one dead mark whose postmaster still serves. The first claims the data
// directory (`claimForRemoval`) and signals the postmaster, which then runs on until the row says
// go; the second starts only after that signal, so it finds the claim held, and says that it waits
// for it. Only then does the postmaster end.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: of two starts on one dead mark whose postmaster serves, the one that claims it sends the one signal and says it removed it, and the other waits for that claim and says another start removed it",
  async () => {
    const row = rowOf(twoOnOne);
    const { tmp, run } = scratch();
    const bin = join(tmp, "bin");
    const signals = join(tmp, "signals");
    const go = join(tmp, "go");
    const data = join(run, "worker-0", "pgdata");
    const postmaster = signalCountingPostmaster(bin, data, signals, go);
    const mark = writeMark(run, owners().gone, data, bin);
    const signalled = () =>
      existsSync(signals) ? readFileSync(signals, "utf8").split("\n").filter(Boolean) : [];
    try {
      const first = startSweep(row, tmp);
      // The signal comes after the first sweep's start and its holder read before the claim.
      await until(
        row,
        () => signalled().length > 0,
        "the first start's signal",
        waitsMs({ children: 1, reads: 1, bounds: [] })
      );
      const second = startSweep(row, tmp);
      await until(
        row,
        () =>
          second.stderr().includes(`waits for another start's claim of ${data}`) ||
          signalled().length > 1,
        "the second start's wait for the first start's claim, or its own signal",
        waitsMs({ children: 1, reads: 1, bounds: [] })
      );
      writeFileSync(go, "");
      const [firstLines, secondLines] = await Promise.all([first.lines, second.lines]);

      expect(signalled()).toEqual(["QUIT"]);
      expect(postmaster.alive()).toBe(false);
      expect(holding(firstLines, `removed the postmaster of ${data}`)).toHaveLength(1);
      expect(holding(secondLines, `removed the postmaster of ${data}`)).toEqual([]);
      expect(
        holding(secondLines, data).filter((line) => line.includes("another start removed"))
      ).toHaveLength(1);
      expect([existsSync(data), existsSync(mark)]).toEqual([false, false]);
      expect(readdirSync(dirname(data)).filter((name) => name.startsWith("pgdata"))).toEqual([]);
    } finally {
      await postmaster.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): two sweeps, their four
  // holder reads, the first's stop and the second's wait for its claim.
  rowMs(twoOnOne)
);

// The postmaster `postmasterOf` verified may end by itself before the start's first signal. No
// `await` is between that read and the signal, so no row can time an end into that window: this
// row asks `stopPostmaster` of a process that ended already, which is what the window gives it. It
// must answer that no signal of its reached that process, which keeps "removed the postmaster"
// off the start's line; of a process that runs, that its signal did, and the process ends.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start says it stopped a postmaster only where its own signal reached it: a postmaster that ended before the signal got none",
  async () => {
    const sleeper = () => {
      const child = Bun.spawn(untilThisEnds(), { stdout: "ignore", stderr: "ignore" });
      return { child, start: marks.startOf(readFileSync(`/proc/${child.pid}/stat`, "utf8")) };
    };
    const ended = sleeper();
    const runs = sleeper();
    try {
      ended.child.kill("SIGKILL");
      await ended.child.exited;
      expect(await stopPostmaster({ pid: ended.child.pid, start: ended.start }, "ended")).toBe(
        false
      );
      expect(await stopPostmaster({ pid: runs.child.pid, start: runs.start }, "runs")).toBe(true);
      expect(marks.stillRuns(runs.child.pid, runs.start)).toBe(false);
    } finally {
      for (const { child } of [ended, runs]) {
        if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
        await child.exited;
      }
    }
  },
  // The stop polls `/proc` every 50 ms until the stand-in that got SIGQUIT has ended, which takes
  // well under a second; 60 s holds a machine under heavy load.
  60_000
);

/** What the next row waits for (`Waits`): a claimer, a child that prints its claim, and then one
 *  sweep, a child that claims the data directory again, stops its postmaster and removes the
 *  socket directory, with a holder read before each claim, and its stop, with its reads after it. */
const claimThenSweep: Waits = { children: 2, reads: 2, stops: 1, bounds: [] };

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: what a start killed between its claim and its removal left, the next start claims again: the postmaster that serves from the claimed directory ends, and the directory goes",
  async () => {
    const row = rowOf(claimThenSweep);
    const { tmp, run } = scratch();
    const bin = join(tmp, "bin");
    const data = join(run, "worker-0", "pgdata");
    const postmaster = standInPostmaster(bin, data, "its TMPDIR");
    const mark = writeMark(run, owners().gone, data, bin);
    try {
      const claimed = await claimAndDie(row, data);
      expect([existsSync(data), existsSync(claimed), postmaster.alive()]).toEqual([
        false,
        true,
        true,
      ]);

      const lines = await sweep(row, tmp);

      expect(postmaster.alive()).toBe(false);
      expect([existsSync(claimed), existsSync(mark)]).toEqual([false, false]);
      expect(holding(lines, `removed the postmaster of ${data}`)).toHaveLength(1);
    } finally {
      await postmaster.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): a claimer, and one sweep
  // with its holder reads and its stop of a postmaster.
  rowMs(claimThenSweep)
);

/** What the next row waits for (`Waits`): `claimThenSweep`, and then what another start of the
 *  machine may still be removing of the plant (`settleMs`). */
const claimThenSweepElsewhere: Waits = { children: 2, reads: 2, stops: 1, bounds: [settleMs] };

// The claim comes before the postmaster here: until a postmaster serves from it, no start but
// this row's own reads the run directory, so the claimed directory is there for the row to see.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: what a start killed between its claim and its removal left in a run directory of another TMPDIR, a start of this TMPDIR finds by the claimed name the kernel shows: the postmaster ends, the directory and the mark go, and one line says so",
  async () => {
    const row = rowOf(claimThenSweepElsewhere);
    const { tmp } = scratch();
    const left = leaveElsewhere(owners().gone, false);
    let postmaster: StandIn | undefined;
    try {
      const claimed = await claimAndDie(row, left.data);
      expect([existsSync(left.data), existsSync(claimed)]).toEqual([false, true]);
      // The kernel names the postmaster's working directory by the claim's name.
      postmaster = standInPostmaster(left.bin, claimed, "every start");

      const lines = await sweep(row, tmp);

      // A line that names that stack says a start removed it: none says it was left. The start
      // prints the run directory's one spelling, the one its data directory is written in.
      await removedByAStart(
        row,
        lines,
        { postmaster, data: left.data, claimed, file: left.file },
        dirname(dirname(left.data))
      );
    } finally {
      await postmaster?.stop();
      rmSync(left.other, { recursive: true, force: true });
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): a claimer, one sweep with
  // its holder reads and its stop of a postmaster, and then `settleMs`.
  rowMs(claimThenSweepElsewhere)
);

/** What the next row waits for (`Waits`): a claimer and two sweeps, each a child; each sweep's
 *  holder reads of the stacks it removes, a read before each claim, two claims a stack, and one
 *  more, where the first start meets the second's socket directory before the second claimed it;
 *  each sweep's wait for the claim the claimer holds; and the stops of both postmasters, each with
 *  its reads after it. */
const claimAndTwo: Waits = {
  children: 3,
  reads: 5,
  stops: 2,
  bounds: [claimWaitMs, claimWaitMs, settleMs],
};

// Two starts at once. The first, Z, lists two postmasters of run directories it does not list,
// and finds the data directory of the first claimed by a claimer that still runs, so it waits for
// that claim. Meanwhile the second, X, removes the stack of the second run directory, which is in
// its own TMPDIR. Once the claimer is killed, Z goes on to that second run directory, whose mark
// and postmaster X removed, so that postmaster is no Postgres Z left. Each postmaster serves only
// once the claim is made, as in the row above, so no other start can take the held stack first.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start names as left only a postmaster that still runs once it has read its run directory: one it listed that another start removed meanwhile it does not name",
  async () => {
    const row = rowOf(claimAndTwo);
    const parent = mkdtempSync(join(plantsRoot(), "marks-two-starts-"));
    // `servedElsewhere` lists by data directory, so the held stack, in `a-held`, comes first.
    const [heldTmp, otherTmp, startTmp] = ["a-held", "b-other", "c-start"].map((name) => {
      mkdirSync(join(parent, name));
      return join(parent, name);
    }) as [string, string, string];
    const plant = (tmp: string) => {
      const run = mkdtempSync(join(tmp, runDirectoryPrefix));
      const data = join(realpathSync(run), "worker-0", "pgdata");
      leaveData(data);
      const bin = join(tmp, "bin");
      return { data, bin, file: writeMark(run, owners().gone, data, bin) };
    };
    const held = plant(heldTmp);
    const other = plant(otherTmp);
    let claimer: Subprocess | undefined;
    let heldPostmaster: StandIn | undefined;
    let otherPostmaster: StandIn | undefined;
    try {
      const claim = await claimInChild(row, held.data);
      claimer = claim.claimer;
      heldPostmaster = standInPostmaster(held.bin, claim.claimed, "every start");
      otherPostmaster = standInPostmaster(other.bin, other.data, "every start");
      const first = startSweep(row, startTmp);
      // The first start's wait comes after its start, and the first stack it reads is the held one.
      await until(
        row,
        () => first.stderr().includes(`waits for another start's claim of ${held.data}`),
        "the first start's wait for the claim the claimer holds",
        waitsMs({ children: 1, reads: 0, bounds: [] })
      );
      const second = startSweep(row, otherTmp);
      const otherServes = otherPostmaster;
      // The second start's stop comes after its start and its holder read before the claim.
      await until(
        row,
        () => !otherServes.alive(),
        "the second start's stop",
        waitsMs({ children: 1, reads: 1, bounds: [] })
      );
      await endChild(claimer);
      const [firstLines, secondLines] = await Promise.all([first.lines, second.lines]);

      // The line a start names an unmarked postmaster on, which the first start must not write.
      const unmarked = "serves it from a run directory this start does not list";
      expect(holding(holding(firstLines, other.data), unmarked)).toEqual([]);
      for (const lines of [firstLines, secondLines]) {
        await removedByAStart(row, lines, {
          ...held,
          claimed: claim.claimed,
          postmaster: heldPostmaster,
        });
        await removedByAStart(row, lines, { ...other, postmaster: otherPostmaster });
      }
    } finally {
      if (claimer !== undefined) await endChild(claimer);
      await heldPostmaster?.stop();
      await otherPostmaster?.stop();
      rmSync(parent, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): a claimer and two sweeps,
  // with their holder reads, their waits for the claim and their stops of the two postmasters.
  rowMs(claimAndTwo)
);
