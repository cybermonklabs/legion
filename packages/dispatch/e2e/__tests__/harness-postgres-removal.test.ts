/** Rows of the check `v-no-leftover-postgres` (requirement R3 of pull request #4236): "The next
 *  start of a test stack removes each Postgres of a stack that it can show dead. It removes no
 *  Postgres of a stack that still runs. It leaves a Postgres whose stack it cannot judge, and it
 *  names that Postgres in one line." The rows of `harness-postgres-leftover.test.ts` start real
 *  stacks under each provider; these plant marks and stand-in processes and need no Postgres.
 *
 *  A start reads the marks of every run directory of `TMPDIR`, which is `/tmp` by default, and of
 *  every run directory the kernel shows a postmaster of its user serving from, and acts as the user
 *  who starts the stack. So the first rows plant what a hostile mark can hold and require that the
 *  sweep leaves it, names it on one line, and acts on none of it: the program the mark names never
 *  runs (it would write a marker file), the live process its `postmaster.pid` names keeps running,
 *  and the directory a link leads to stays. The next rows pin the line of each case the start
 *  cannot judge, the one spelling of a run directory, and a container's mark where the start has
 *  no `docker`. The rows of a run directory of another `TMPDIR` and of two starts at once are in
 *  `harness-postgres-elsewhere.test.ts`, the rows of a container's removal in
 *  `harness-postgres-container-removal.test.ts`, and the rows of the rows' own limits in
 *  `harness-postgres-waits.test.ts`.
 *
 *  The rows run the start's own sweep, `removeLeftoverPostgres`, in a child of this file, under a
 *  `TMPDIR` of the row's own and a `PATH` that holds no `docker`, so the sweep reads no container
 *  of another run on the machine. They need Linux (`/proc`). A mark another user wrote needs a
 *  second user, which a test run has none of, so the rows of `whyUntrusted`
 *  (`harness-postgres-marks.test.ts`) read owners from a stubbed `lstat`; these rows plant what
 *  one user can: a link, a program, a directory others may write. A stand-in process is one this
 *  file starts and records by its pid and start, and stops only while `/proc` still names it; a
 *  stand-in postmaster is a copy of `sleep` that serves from the data directory its
 *  `postmaster.pid` names.
 *
 *  ANOTHER START BESIDE A ROW. A dead stack that a row plants is one every start must remove, and
 *  any start of this user may run beside the row: another session's on the machine, or
 *  `harness-stacks.test.ts`'s in the same `bun test --parallel` run. So each row plants its
 *  stacks under a TMPDIR of its own, which no other start lists, and its stand-in postmasters run
 *  a program that is not named `postgres` (`Seen`), which no other start's walk of the kernel's
 *  processes reads: only the row's own starts find them. A row's own sweep walks for run
 *  directories only in the file's own directory (`plantsRoot`), so it judges no stack that a row
 *  of another file, or of another session, planted beside it. */

import { afterAll, expect, test } from "bun:test";
import {
  chmodSync,
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { basename, join } from "node:path";
import * as marks from "../harness-postgres-marks";
import { servingPostmasters } from "../harness-postgres-removal";
import {
  holding,
  leaveContainerMark,
  leaveData,
  owners,
  plantsRoot,
  scratch,
  standInPostmaster,
  sweep,
  writeMark,
} from "./harness-postgres-removal-fixtures";
import { rowMs, rowOf, type Waits } from "./harness-postgres-waits";
import {
  hostileMarkKept,
  plantedBin,
  plantHostileMark,
  type StandIn,
  standIn,
  untilThisEnds,
} from "./planted-marks";
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

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a mark whose postmaster.bin is the planter's and whose stack directory is a link out of the run is left: its program never runs, the live process stays, the directory outside stays",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp, run } = scratch();
    const hostile = plantHostileMark(run, tmp, owners().gone);
    try {
      const lines = await sweep(row, tmp);

      expect(hostile.kept()).toEqual(hostileMarkKept);
      expect(holding(lines, `${hostile.file} (${hostile.link} is a symbolic link)`)).toHaveLength(
        1
      );
    } finally {
      await hostile.live.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: the start runs no program a mark names: a postmaster.pid that names a process which is no postgres of the mark's binaries leaves the mark, and that process runs on",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp, run } = scratch();
    const marker = join(tmp, "the-planted-program-ran");
    const data = join(run, "worker-0", "pgdata");
    mkdirSync(data, { recursive: true });
    const live = standIn(data, untilThisEnds());
    try {
      leaveData(data, live.pid);
      const file = writeMark(run, owners().gone, data, plantedBin(tmp, marker));

      const lines = await sweep(row, tmp);

      expect(existsSync(marker)).toBe(false);
      expect(live.alive()).toBe(true);
      expect(existsSync(join(data, "PG_VERSION"))).toBe(true);
      expect(existsSync(file)).toBe(true);
      const left = holding(lines, `names pid ${live.pid}`);
      expect(left).toHaveLength(1);
      expect(left[0]).toContain(data);
      expect(left[0]).toContain("is not removed");
    } finally {
      await live.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a link in any component of what a mark names is never followed: a stack directory that links out of the run leaves the directory it leads to",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp, run } = scratch();
    const outside = join(tmp, "outside");
    try {
      leaveData(join(outside, "pgdata"));
      symlinkSync(outside, join(run, "worker-0"));
      const noBin = mkdtempSync(join(tmp, "no-bin-"));
      const file = writeMark(run, owners().gone, join(run, "worker-0", "pgdata"), noBin);

      const lines = await sweep(row, tmp);

      expect(existsSync(join(outside, "pgdata", "PG_VERSION"))).toBe(true);
      expect(existsSync(file)).toBe(true);
      expect(holding(lines, `${file} (${join(run, "worker-0")} is a symbolic link)`)).toHaveLength(
        1
      );
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a run directory that is a link is never read: the marks of the directory it leads to stay, with what they name",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp } = scratch();
    const elsewhere = mkdtempSync(join(tmp, "elsewhere-"));
    const link = join(tmp, `${runDirectoryPrefix}link`);
    try {
      const data = join(elsewhere, "worker-0", "pgdata");
      leaveData(data);
      const file = writeMark(elsewhere, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
      symlinkSync(elsewhere, link);

      const lines = await sweep(row, tmp);

      expect(existsSync(join(data, "PG_VERSION"))).toBe(true);
      expect(existsSync(file)).toBe(true);
      expect(holding(lines, `${link} (${link} is a symbolic link)`)).toHaveLength(1);
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a run directory that another user may write is never read, in a TMPDIR shared and sticky as /tmp is: its gone stack's directories stay",
  async () => {
    const row = rowOf(oneSweep);
    // Built in /tmp itself: a TMPDIR anywhere under a directory of this user's that no other user
    // may enter (a session's own, mode 0700) is one where no other user reaches a run directory.
    const { tmp, run } = scratch("/tmp");
    try {
      const data = join(run, "worker-0", "pgdata");
      leaveData(data);
      const file = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
      // /tmp, the default TMPDIR, is root's of mode 1777, which every user may write and whose
      // sticky bit keeps each from renaming another's entries; the row's TMPDIR is made the same.
      chmodSync(tmp, 0o1777);
      chmodSync(run, 0o777);

      const lines = await sweep(row, tmp);

      expect(existsSync(join(data, "PG_VERSION"))).toBe(true);
      expect(existsSync(file)).toBe(true);
      expect(holding(lines, `${run} (${run} is writable by other users)`)).toHaveLength(1);
    } finally {
      chmodSync(run, 0o700);
      chmodSync(tmp, 0o700);
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a mark of another boot, of another PID namespace, of another time namespace and of a stack where there is no /proc is each left with its directories, and each is named with its case on the one line of what the start cannot judge",
  async () => {
    const row = rowOf(oneSweep);
    const who = owners();
    const { tmp, run } = scratch();
    try {
      const bin = mkdtempSync(join(tmp, "no-bin-"));
      const cases = [
        { owner: who.otherBoot, why: `boot ${who.otherBoot.boot}` },
        { owner: who.otherNamespace, why: "PID namespace pid:[1]" },
        { owner: who.otherTime, why: "time namespace time:[1]" },
        { owner: who.noProc, why: "its stack ran where there is no /proc" },
      ].map(({ owner, why }, index) => {
        const data = join(run, `worker-${index}`, "pgdata");
        leaveData(data);
        return { data, why, file: writeMark(run, owner, data, bin, index) };
      });

      const lines = await sweep(row, tmp);

      const unjudged = holding(lines, "left the Postgres of stacks whose process it cannot see:");
      expect(unjudged).toHaveLength(1);
      for (const { data, why, file } of cases) {
        expect(unjudged[0]).toContain(`${file} (${why})`);
        expect([existsSync(join(data, "PG_VERSION")), existsSync(file)]).toEqual([true, true]);
      }
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

/** What a row waits for that runs one start's sweep, which removes a dead stack's data and socket
 *  directories that no postmaster serves: two holder reads, one before each claim (`Waits`). */
const sweepRemoving: Waits = { children: 1, reads: 2, bounds: [] };

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a TMPDIR spelt through a link of its user's is read under its one spelling: the gone stack's directories go, each named under that spelling, and no line says a link was refused",
  async () => {
    const row = rowOf(sweepRemoving);
    const { tmp } = scratch();
    const link = `${tmp}-link`;
    try {
      symlinkSync(tmp, link);
      // The run directory as a stack under that TMPDIR spells it, through the link.
      const run = mkdtempSync(join(link, runDirectoryPrefix));
      const data = join(run, "worker-0", "pgdata");
      leaveData(data);
      const file = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
      const real = marks.identityOf(run);
      expect(real.startsWith(`${tmp}/`)).toBe(true);

      const lines = await sweep(row, link);

      expect([existsSync(data), existsSync(file)]).toEqual([false, false]);
      expect(
        holding(lines, `removed ${join(real, "worker-0", "pgdata")} and ${real}/pg-0-`)
      ).toHaveLength(1);
      expect(holding(lines, "is a symbolic link")).toEqual([]);
    } finally {
      rmSync(link, { force: true });
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep and its two
  // holder reads.
  rowMs(sweepRemoving)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a mark whose one spelling of its run directory is another directory's is no mark of the run directory it is in: it is left with what it names, and named",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp, run } = scratch();
    try {
      const data = join(run, "worker-0", "pgdata");
      leaveData(data);
      const file = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
      // The mark of a run directory of the same name elsewhere, copied into this one.
      const elsewhere = join(mkdtempSync(join(tmp, "elsewhere-")), basename(run));
      const written = JSON.parse(readFileSync(file, "utf8")) as Record<string, unknown>;
      writeFileSync(file, JSON.stringify({ ...written, identity: elsewhere }));

      const lines = await sweep(row, tmp);

      expect([existsSync(join(data, "PG_VERSION")), existsSync(file)]).toEqual([true, true]);
      expect(holding(lines, `${file} (no mark of this run directory)`)).toHaveLength(1);
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start with no docker on its PATH leaves the mark of a dead stack's container, with its socket directory, and names it: it cannot tell whether that container still runs",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp, run } = scratch();
    try {
      const { sockets, container, file } = leaveContainerMark(run, owners().gone);

      const lines = await sweep(row, tmp);

      expect([existsSync(sockets), existsSync(file)]).toEqual([true, true]);
      expect(
        holding(
          lines,
          `${file} (its container ${container}, since this start could not list Docker's containers)`
        )
      ).toHaveLength(1);
      expect(holding(lines, "removed")).toEqual([]);
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start with no docker on its PATH leaves the mark of a live stack's container without a word: it judges the owner before it reads Docker",
  async () => {
    const row = rowOf(oneSweep);
    const { tmp, run } = scratch();
    try {
      const { sockets, file } = leaveContainerMark(run, owners().runs);

      const lines = await sweep(row, tmp);

      expect([existsSync(sockets), existsSync(file)]).toEqual([true, true]);
      expect(holding(lines, run)).toEqual([]);
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep.
  rowMs(oneSweep)
);

test.skipIf(!onLinux)(
  "v-no-leftover-postgres: where the target of a TMPDIR link changed between the write of a mark and the teardown, the teardown reads the stack's postmaster under the mark's one spelling, the path the sweep acts on",
  async () => {
    const { tmp } = scratch();
    const link = `${tmp}-link`;
    const moved = `${tmp}-moved`;
    let postmaster: StandIn | undefined;
    try {
      const first = join(tmp, "first");
      mkdirSync(first);
      symlinkSync(first, link);
      // The run directory as its stack spells it, through the link; its postmaster serves from the
      // one spelling, which the mark holds.
      const run = mkdtempSync(join(link, runDirectoryPrefix));
      const identity = marks.identityOf(run);
      const served = join(identity, "worker-0", "pgdata");
      postmaster = standInPostmaster(join(tmp, "bin"), served, "its TMPDIR");
      writeMark(run, owners().runs, join(run, "worker-0", "pgdata"), join(tmp, "bin"));
      // The link now leads to a copy of that run directory, marks and all, elsewhere: copied in
      // this process, so the row starts no process it would need a limit for.
      mkdirSync(moved);
      cpSync(identity, join(moved, basename(run)), {
        preserveTimestamps: true,
        recursive: true,
        verbatimSymlinks: true,
      });
      rmSync(link);
      symlinkSync(moved, link);

      expect(servingPostmasters(run)).toEqual([served]);
    } finally {
      await postmaster?.stop();
      rmSync(link, { force: true });
      rmSync(moved, { recursive: true, force: true });
      rmSync(tmp, { recursive: true, force: true });
    }
  }
);
