/** Rows of the check `v-no-leftover-postgres` (requirement R3 of pull request #4236) for a dead
 *  stack whose directories a process of its user still holds: "It removes no Postgres of a stack
 *  that still runs." A stack killed hard while its `pg_ctl start`, `initdb` or `docker create`
 *  runs leaves that child running, so a start leaves each directory such a process holds, with
 *  its mark, names it in one line, and never renames it, and the next start after that process
 *  ended removes it. Each row holds a directory one way: by an argument of a `pg_ctl`, however
 *  the stack spelt it, by a working directory alone, by an open file alone, by a mount of the
 *  socket directory, and by a postmaster's process that outlives it a moment; and one row has a
 *  process name a path that never answers, which the start cannot judge it by within its bound.
 *  The fixtures and the way the rows run the start's sweep are those of
 *  `harness-postgres-removal.test.ts` (`harness-postgres-removal-fixtures.ts`, and the program of
 *  each row's own limit, `harness-postgres-waits.ts`), under a TMPDIR of each row's own. */

import { afterAll, expect, test } from "bun:test";
import {
  closeSync,
  existsSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { basename, dirname, join } from "node:path";
import type { Subprocess } from "bun";

import * as marks from "../harness-postgres-marks";
import { statMs } from "../harness-postgres-processes";
import {
  clockOf,
  endChild,
  firstLineOf,
  harnessScript,
  holding,
  leaveData,
  neverAnswering,
  owners,
  plantsRoot,
  readsAfterRename,
  removingInChild,
  scratch,
  slowReleasingPostmaster,
  startingPgCtl,
  startSweep,
  sweep,
  until,
  writeMark,
} from "./harness-postgres-removal-fixtures";
import { claimEventMs, lifeOf, rowMs, rowOf, type Waits, waitsMs } from "./harness-postgres-waits";
import { type StandIn, standIn, untilParentEnds, untilThisEnds } from "./planted-marks";
import { noHookLimit, removeScratch } from "./step-end";

const { marksDirectory } = marks;

afterAll(
  () => removeScratch([plantsRoot()], rowMs({ children: 1, reads: 0, bounds: [] })),
  // The file's own directory of plants (`plantsRoot`), which each row empties, goes in a process of
  // its own, which may take what the CI step leaves, or one child's limit, and the hook has no
  // limit of Bun's: step-end.ts says why.
  noHookLimit
);

const onLinux = process.platform === "linux";

/** How a stack spells its directories to its `pg_ctl` in the rows below: as the pool makes them;
 *  under a TMPDIR spelt through a link of its user's, which the README allows, and under such a
 *  link that is gone when the next start sweeps, so that the spelling no longer leads to the
 *  directory and only the mark's own spellings name it; with a `-D` relative to the directory
 *  `pg_ctl` runs in; and with a doubled `/`. The mark holds its run directory's one spelling and
 *  the spellings the stack gave it, and a start judges a holder by the directory an argument
 *  names, not by its spelling. `spell` answers the `-D` argument and the directory `pg_ctl` runs
 *  in, for the run directory `run` as the stack spells it. */
const pgCtlSpellings: readonly {
  readonly title: string;
  readonly link: "none" | "kept" | "gone";
  readonly spell: (
    run: string,
    alias: string
  ) => {
    readonly HARNESS_DATA?: string;
    readonly HARNESS_CWD?: string;
  };
}[] = [
  {
    title:
      "v-no-leftover-postgres: a start leaves the Postgres of a stack killed while its pg_ctl start runs, and names it in one line, since that pg_ctl still holds its directories; the next start after that pg_ctl ended removes the postmaster it started",
    link: "none",
    spell: () => ({}),
  },
  {
    title:
      "v-no-leftover-postgres: under a TMPDIR spelt through a link, a start leaves the Postgres of a stack killed while its pg_ctl start runs, and names it in one line, though that pg_ctl names its directories through the link; the next start after that pg_ctl ended removes the postmaster it started",
    link: "kept",
    spell: () => ({}),
  },
  {
    title:
      "v-no-leftover-postgres: under a TMPDIR spelt through a link that is gone when the start sweeps, a start leaves the Postgres of a stack killed while its pg_ctl start runs, and names it in one line, by the spelling its mark holds; the next start after that pg_ctl ended removes the postmaster it started",
    link: "gone",
    spell: () => ({}),
  },
  {
    title:
      "v-no-leftover-postgres: a start leaves the Postgres of a stack killed while its pg_ctl start runs with a -D relative to its working directory, and names it in one line; the next start after that pg_ctl ended removes the postmaster it started",
    link: "none",
    spell: (run) => ({ HARNESS_DATA: "pgdata", HARNESS_CWD: join(run, "worker-0") }),
  },
  {
    title:
      "v-no-leftover-postgres: a start leaves the Postgres of a stack killed while its pg_ctl start runs with a -D spelt with a doubled /, and names it in one line; the next start after that pg_ctl ended removes the postmaster it started",
    link: "none",
    spell: (run) => ({ HARNESS_DATA: `${run}//worker-0/pgdata` }),
  },
  {
    title:
      "v-no-leftover-postgres: a start leaves the Postgres of a stack killed while its pg_ctl start runs with a -D through a link that its mark does not hold, and names it in one line; the next start after that pg_ctl ended removes the postmaster it started",
    link: "none",
    spell: (run, alias) => ({ HARNESS_DATA: join(alias, basename(run), "worker-0", "pgdata") }),
  },
];

/** What each of the next rows waits for (`Waits`): the stack's harness, a child, and its pg_ctl's
 *  start and end, each at most `claimEventMs`; and two sweeps, each a child: the first reads the
 *  holders of the data directory, which the pg_ctl holds, and claims nothing, and the second
 *  reads them before its claim of the data directory and before its claim of the socket
 *  directory, and stops that postmaster, with its reads after the stop. */
const pgCtlRow: Waits = { children: 3, reads: 3, stops: 1, bounds: [claimEventMs, claimEventMs] };

// A stack killed hard while its `pg_ctl start` runs leaves that pg_ctl running, and the
// postmaster it then starts. A start in between finds no postmaster.pid yet: it must leave the
// directories that pg_ctl names, with the mark, and name them, and the next start after that
// pg_ctl ended removes the postmaster it started.
for (const { title, link, spell } of pgCtlSpellings) {
  test.skipIf(!onLinux)(
    title,
    async () => {
      const row = rowOf(pgCtlRow);
      const { tmp, run: own } = scratch();
      // The TMPDIR the stack and the starts are given, and the run directory as the stack spells
      // it there; the lines name the run directory's one spelling.
      const given = link === "none" ? tmp : `${tmp}-link`;
      if (link !== "none") symlinkSync(tmp, given);
      // A link of this user's to the TMPDIR that no mark holds, for a `-D` spelt through it.
      const alias = `${tmp}-alias`;
      symlinkSync(tmp, alias);
      const run = join(given, basename(own));
      const bin = join(tmp, "bin");
      const go = join(tmp, "go");
      const sockets = mkdtempSync(join(run, "pg-0-"));
      const data = join(run, "worker-0", "pgdata");
      const [named, namedSockets] = [join(own, "worker-0", "pgdata"), join(own, basename(sockets))];
      const mark = join(own, marksDirectory, "worker-0.json");
      const spelt = spell(run, alias);
      startingPgCtl(bin, go, lifeOf(row));
      row.child("the stack's harness");
      const harness = Bun.spawn([process.execPath, "--eval", harnessScript], {
        env: {
          ...process.env,
          HARNESS_RUN: run,
          HARNESS_BIN: bin,
          HARNESS_SOCKETS: sockets,
          ...spelt,
        },
        stdout: "pipe",
        stderr: "ignore",
      });
      let pgCtl: { pid: number; start: string } | undefined;
      let postmaster: { pid: number; start: string } | undefined;
      const startOfPid = (pid: number) => marks.startOf(readFileSync(`/proc/${pid}/stat`, "utf8"));
      try {
        const pid = Number(await firstLineOf(row, harness, "the stack's harness"));
        // The harness's child runs pg_ctl once it has its arguments, which name the data
        // directory as the stack spells it.
        const argument = spelt.HARNESS_DATA ?? data;
        await until(
          row,
          () => readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0").includes(argument),
          "the start of the stack's pg_ctl",
          claimEventMs
        );
        pgCtl = { pid, start: startOfPid(pid) };
        // The harness dies as a stack killed hard does: its pg_ctl runs on.
        await endChild(harness);
        expect(marks.stillRuns(pgCtl.pid, pgCtl.start)).toBe(true);
        // A link that is gone leads nowhere: the first start lists the TMPDIR it led to.
        if (link === "gone") rmSync(given);

        const first = await sweep(row, link === "gone" ? tmp : given);

        expect([existsSync(named), existsSync(namedSockets), existsSync(mark)]).toEqual([
          true,
          true,
          true,
        ]);
        expect(holding(first, "removed")).toEqual([]);
        const left = holding(first, mark);
        expect(left).toEqual([expect.stringContaining(`${mark} (${named}, which `)]);
        expect(left[0]).toContain(` ${pgCtl.pid} still holds)`);

        // The link again, for the pg_ctl, which goes into its data directory as it spells it.
        if (link === "gone") symlinkSync(tmp, given);

        writeFileSync(go, "");
        const ended = pgCtl;
        await until(
          row,
          () => !marks.stillRuns(ended.pid, ended.start),
          "the end of the stack's pg_ctl",
          claimEventMs
        );
        const served = Number.parseInt(readFileSync(join(data, "postmaster.pid"), "utf8"), 10);
        postmaster = { pid: served, start: startOfPid(served) };
        const second = await sweep(row, given);

        // One check of what the second start did and of every line it printed, so that a failure
        // shows those lines: in 1 of 19 runs of this round's repeats its second start left the data
        // directory, the socket directory and the mark while the postmaster no longer ran, and the
        // three checks that stood here printed none of its lines.
        expect({
          postmaster: marks.stillRuns(postmaster.pid, postmaster.start),
          left: [existsSync(data), existsSync(sockets), existsSync(mark)],
          removed: holding(
            second,
            `removed the postmaster of ${named}, that directory and ${namedSockets}`
          ).length,
          second,
        }).toEqual({ postmaster: false, left: [false, false, false], removed: 1, second });
      } finally {
        await endChild(harness);
        for (const each of [pgCtl, postmaster]) {
          if (each !== undefined && marks.stillRuns(each.pid, each.start)) {
            process.kill(each.pid, "SIGKILL");
          }
        }
        if (link !== "none") rmSync(given, { force: true });
        rmSync(alias, { force: true });
        rmSync(tmp, { recursive: true, force: true });
      }
    },
    // What the row waits for, summed by the fixtures' program (`rowMs`): a harness, its pg_ctl's
    // start and end, and two sweeps with their holder reads and one stop of a postmaster.
    rowMs(pgCtlRow)
  );
}

/** The socket directory that the mark `file` names, as the stack spelt it. */
function socketsOf(file: string): string {
  const sockets = marks.markOf(readFileSync(file, "utf8"))?.sockets;
  if (sockets === undefined) throw new Error(`${file} is no mark that names a socket directory`);
  return sockets;
}

/** When the entries of the directory `path` last changed: a rename of one of them, even one that
 *  is undone, changes it. */
function entriesChangedAt(path: string): bigint {
  return statSync(path, { bigint: true }).mtimeNs;
}

/** What each of the next two rows waits for (`Waits`): two sweeps, each a child: the first reads
 *  the holders of the data directory, which a process holds, and claims nothing, and the second,
 *  after that process's end, at most `claimEventMs`, reads them before each of its two claims. */
const heldThenGone: Waits = { children: 2, reads: 3, bounds: [claimEventMs] };

// A process of this user whose working directory is a dead stack's data directory holds it, though
// no argument and no open file of it names that directory: a `postgres` that `pg_ctl` started and
// that has not yet written its postmaster.pid is so. The start reads it before it would rename the
// directory, so the holder never has its directory under another name.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start leaves a dead stack's data directory that a process of this user holds by its working directory alone, and its mark, never renames it, and names it in one line; the next start after that process ended removes both",
  async () => {
    const row = rowOf(heldThenGone);
    const { tmp, run } = scratch();
    const data = join(run, "worker-0", "pgdata");
    leaveData(data);
    const mark = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
    const sockets = socketsOf(mark);
    // `tail` with no file reads its standard input, a pipe this process holds, to its end, which
    // comes as this process ends: it names no path, so its working directory alone holds the
    // directory.
    const tail = Bun.which("tail");
    if (tail === null) throw new Error("no tail on PATH to hold a directory");
    const holder = standIn(data, [tail], "pipe");
    try {
      const before = entriesChangedAt(dirname(data));
      const first = await sweep(row, tmp);

      expect([existsSync(data), existsSync(mark)]).toEqual([true, true]);
      expect(entriesChangedAt(dirname(data))).toBe(before);
      expect(holding(first, "removed")).toEqual([]);
      expect(holding(first, `${mark} (${data}, which `)).toEqual([
        expect.stringContaining(` ${holder.pid} still holds)`),
      ]);

      await holder.stop();
      await until(row, () => !holder.alive(), "the end of the holder", claimEventMs);
      const second = await sweep(row, tmp);

      expect([existsSync(data), existsSync(mark)]).toEqual([false, false]);
      expect(holding(second, `removed ${data} and ${sockets}`)).toHaveLength(1);
    } finally {
      await holder.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): two sweeps, their three
  // holder reads, and the holder's end between them.
  rowMs(heldThenGone)
);

// A process of this user that holds a file of a dead stack's data directory open holds that
// directory, though its working directory and its arguments are elsewhere.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start leaves a dead stack's data directory in which a process of this user holds a file open, and its mark, and names it in one line; the next start after that process ended removes both",
  async () => {
    const row = rowOf(heldThenGone);
    const { tmp, run } = scratch();
    const data = join(run, "worker-0", "pgdata");
    leaveData(data);
    const mark = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
    const sockets = socketsOf(mark);
    const opened = openSync(join(data, "PG_VERSION"), "r");
    let holder: StandIn | undefined;
    try {
      holder = standIn(tmp, untilThisEnds(), opened);
      closeSync(opened);

      const first = await sweep(row, tmp);

      expect([existsSync(data), existsSync(mark)]).toEqual([true, true]);
      expect(holding(first, "removed")).toEqual([]);
      expect(holding(first, `${mark} (${data}, which `)).toEqual([
        expect.stringContaining(` ${holder.pid} still holds)`),
      ]);

      const ended = holder;
      await ended.stop();
      await until(row, () => !ended.alive(), "the end of the holder", claimEventMs);
      const second = await sweep(row, tmp);

      expect([existsSync(data), existsSync(mark)]).toEqual([false, false]);
      expect(holding(second, `removed ${data} and ${sockets}`)).toHaveLength(1);
    } finally {
      await holder?.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): two sweeps, their three
  // holder reads, and the holder's end between them.
  rowMs(heldThenGone)
);

/** What the next row waits for (`Waits`): the other start's removal, a child, with its claim, at
 *  most `claimEventMs`; and one sweep, a child, whose holder read before its claim of the data
 *  directory meets that removal and a process under the claim, and leaves the directory. */
const removalUnderClaim: Waits = { children: 2, reads: 1, bounds: [] };

// Two starts at once on one dead mark: the first claims the data directory, renaming it to a name
// that holds the first start's pid and start (`claimForRemoval`), and removes it, with directories
// of it open while it does. Where the second start looked at the directory before that rename and
// reads what the processes name after it, the first start's open directories lead into it: on
// 2026-11-19 the second start then named the first as a process that still held it, and left its
// mark, in 6 of 80 runs of the binaries row of two starts at once on a shared Linux machine. The
// row's reader reads the claim as the directory, as the kernel does after that rename
// (`readsAfterRename`). Beside the other start, a process that claimed nothing has a directory
// under the claim for its working directory, and holds the directory still.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start's holder read does not read another start's removal of a dead stack's data directory under that start's own claim as a process that holds it, and still reads a process that names a path under the claim as one",
  async () => {
    const row = rowOf(removalUnderClaim);
    const { tmp, run } = scratch();
    const data = join(run, "worker-0", "pgdata");
    leaveData(data);
    const mark = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
    const tail = Bun.which("tail");
    if (tail === null) throw new Error("no tail on PATH to hold a directory");
    let removal: { claimed: string; claimer: Subprocess } | undefined;
    let holder: StandIn | undefined;
    try {
      removal = await removingInChild(row, data);
      holder = standIn(join(removal.claimed, "d0"), [tail], "pipe");

      const lines = await startSweep(row, tmp, {
        reader: readsAfterRename(data, removal.claimed),
      }).lines;

      expect([existsSync(data), existsSync(mark)]).toEqual([true, true]);
      expect(holding(lines, "removed")).toEqual([]);
      expect(holding(lines, `${mark} (${data}, which `)).toEqual([
        expect.stringContaining(`, which tail ${holder.pid} still holds)`),
      ]);
      expect(holding(lines, `${basename(process.execPath)} ${removal.claimer.pid}`)).toEqual([]);
    } finally {
      await holder?.stop();
      if (removal !== undefined) await endChild(removal.claimer);
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): the other start's removal,
  // and one sweep with its holder read.
  rowMs(removalUnderClaim)
);

/** What the next row waits for (`Waits`): the holder, a child, with its arguments and its end,
 *  each at most `claimEventMs`; and two sweeps, each a child: the first reads the holders before
 *  its claims of the data directory and of the socket directory, which the holder holds, and the
 *  second, after the holder's end, before its claim of the socket directory. */
const mountThenGone: Waits = { children: 3, reads: 3, bounds: [claimEventMs, claimEventMs] };

// The check before the removal of each socket directory, which the container sweep's removal
// shares: a process whose argument mounts the socket directory, as `docker create` writes it,
// holds it.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start leaves a dead stack's socket directory that an argument of a process of this user mounts, as docker create writes it, and its mark, never renames it, and names it in one line; the next start after that process ended removes both",
  async () => {
    const row = rowOf(mountThenGone);
    const { tmp, run } = scratch();
    const data = join(run, "worker-0", "pgdata");
    leaveData(data);
    const mark = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
    const sockets = socketsOf(mark);
    const mount = `type=bind,source=${sockets},target=/var/run/postgresql`;
    row.child("the holder");
    const holder = standIn(tmp, [process.execPath, "--eval", untilParentEnds, "--mount", mount]);
    try {
      await until(
        row,
        () => readFileSync(`/proc/${holder.pid}/cmdline`, "utf8").includes(mount),
        "the holder's arguments",
        claimEventMs
      );

      const before = entriesChangedAt(run);
      const first = await sweep(row, tmp);

      expect([existsSync(sockets), existsSync(mark)]).toEqual([true, true]);
      expect(entriesChangedAt(run)).toBe(before);
      expect(holding(first, `${mark} (${sockets}, which `)).toEqual([
        expect.stringContaining(` ${holder.pid} still holds)`),
      ]);

      await holder.stop();
      await until(row, () => !holder.alive(), "the end of the holder", claimEventMs);
      const second = await sweep(row, tmp);

      expect([existsSync(sockets), existsSync(mark)]).toEqual([false, false]);
      expect(holding(second, `removed ${sockets}`)).toHaveLength(1);
    } finally {
      await holder.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): a holder, its arguments
  // and its end, and two sweeps with their three holder reads.
  rowMs(mountThenGone)
);

/** What the next row waits for (`Waits`): one sweep, a child, which reads the holders before its
 *  claim of the data directory and before its claim of the socket directory, and stops the
 *  postmaster, with its two reads after the stop, the second once the postmaster's last process
 *  ended; and then the last process's look, at most `claimEventMs`. */
const stopThenRelease: Waits = { children: 1, reads: 2, stops: 1, bounds: [claimEventMs] };

// The wait after a stop: a postmaster's processes outlive it a moment, and the directory stays
// until the last of them has ended.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start that stops a dead stack's postmaster removes its data directory only once the postmaster's last process, which outlives it a moment, has ended",
  async () => {
    const row = rowOf(stopThenRelease);
    const { tmp, run } = scratch();
    const bin = join(tmp, "bin");
    const saw = join(tmp, "saw");
    const data = join(run, "worker-0", "pgdata");
    const postmaster = slowReleasingPostmaster(bin, data, saw);
    const mark = writeMark(run, owners().gone, data, bin);
    const sockets = socketsOf(mark);
    try {
      const lines = await sweep(row, tmp);
      await until(
        row,
        () => existsSync(saw) && readFileSync(saw, "utf8") !== "",
        "the last process's look at the data directory",
        claimEventMs
      );

      expect(readFileSync(saw, "utf8").split("\n").filter(Boolean)).toEqual(["present"]);
      expect(postmaster.alive()).toBe(false);
      expect([existsSync(data), existsSync(sockets), existsSync(mark)]).toEqual([
        false,
        false,
        false,
      ]);
      expect(
        holding(lines, `removed the postmaster of ${data}, that directory and ${sockets}`)
      ).toHaveLength(1);
    } finally {
      await postmaster.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one sweep, its holder reads
  // and its stop of a postmaster, and then the last process's look.
  rowMs(stopThenRelease)
);

/** What each of the next rows waits for (`Waits`): the holder, a child, with its arguments, at
 *  most `claimEventMs`; and one sweep, a child, whose holder read before its claim of the data
 *  directory starts one reader, which never answers a path the holder names: the read of that
 *  group stops there and reads no path after it (`notReached` of
 *  `e2e/harness-postgres-processes.ts`), so it starts no second reader. */
const neverAnswers: Waits = { children: 2, reads: 1, bounds: [claimEventMs] };

/** How long the start of the next rows waits on one path that answers no stat (`StatReader.statMs`
 *  of the row's stand-in reader, `neverAnswering`): the row's own, 3 s, through the same parameter
 *  the start's own reader gives its own `statMs`, 10 s, so each row waits seconds and not the
 *  start's own bound. */
const readerMs = 3_000;

/** The room the next rows give the stand-in reader's clock above the bound of the start's wait
 *  (`readerMs`), in seconds: the timer of the start that ends that wait, which fires late under
 *  load, and the last tick of the stand-in's own clock before its kill, 500 ms apart. In 680 runs
 *  of the two-mounts rows beside a shared Linux machine's load on 2026-11-19, from the first hung
 *  reader's fork to its last tick took at most 0.1 s past the start's own 10 s bound. The room is
 *  less than the 7 s from the row's bound to that 10 s (`statMs`), so a start that keeps its own
 *  bound and not the reader's falls outside it, and so do a wait bounded by the 240 s a reader has
 *  to start (`statStartMs`), and a bound for each path in turn, which nine paths under one
 *  directory make 27 s, and not one bound for the directory they are under. */
const clockRoomS = 5;
if ((statMs - readerMs) / 1000 <= clockRoomS) {
  throw new Error("the rows' room reaches the start's own bound on one path, which they must see");
}

// A path on a network or FUSE mount whose server hangs never answers a stat, nor does any path
// under it, and a start whose holder read waits on one waits for ever with no line. The row's start
// reads where each path leads through a stand-in reader (`neverAnswering`) that answers every path
// at once but `never` and the paths under it, which it never answers, as such a mount does; a real
// mount of that kind needs `/dev/fuse` and `fusermount3`, which a CI runner may not have. The
// holder names one such path, or nine under one directory that never answers: either way the read
// waits one bound, says so on one line when it does, and names the path it stopped at. The
// stand-in's own clock times that wait, which no load on the start or on the row stretches.
for (const holder of [
  { names: "a path", paths: ["pgdata"], never: "pgdata" },
  {
    names: "nine paths under one directory",
    paths: ["a", "b", "c", "d", "e", "f", "g", "h", "i"],
    never: "",
  },
] as const) {
  test.skipIf(!onLinux)(
    `v-no-leftover-postgres: a start leaves a dead stack's data directory, and its mark, that a process may hold since ${holder.names} it names answers no stat, names that process and that path in one line, and waits on them for one bound in all`,
    async () => {
      const row = rowOf(neverAnswers);
      const { tmp, run } = scratch();
      const data = join(run, "worker-0", "pgdata");
      leaveData(data);
      const mark = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
      const hung = join(tmp, "never-answers");
      const named = holder.paths.map((path) => join(hung, path));
      const never = holder.never === "" ? hung : join(hung, holder.never);
      const clock = join(tmp, "never-answers-clock");
      row.child("the holder");
      const holds = standIn(tmp, [process.execPath, "--eval", untilParentEnds, ...named]);
      let sweeping: Subprocess | undefined;
      try {
        await until(
          row,
          () => readFileSync(`/proc/${holds.pid}/cmdline`, "utf8").includes(named.at(-1) ?? ""),
          "the holder's arguments",
          claimEventMs
        );

        // The hung directory is on the machine's own filesystem here, which the mount table says
        // (this process's own table): the read of those filesystems stops at it, and its line
        // names the paths it then leaves unread, the limit of a local filesystem that hangs.
        const reader = neverAnswering([never], clock, join(tmp, "mountinfo"), readerMs);
        const started = startSweep(row, tmp, { reader });
        sweeping = started.child;
        // A bound on another process, which no fake clock of this one reaches: the row's own end
        // for a start that waits on the path for ever, the sweep and its holder read as the
        // fixtures' program sums them.
        const limit = waitsMs({ children: 1, reads: neverAnswers.reads, bounds: [] });
        const lines = await Promise.race([started.lines, Bun.sleep(limit).then(() => undefined)]);
        if (lines === undefined) throw new Error(`the sweep did not end within ${limit / 1000} s`);

        expect([existsSync(data), existsSync(mark)]).toEqual([true, true]);
        // The holder is named by the first path it names that has no answer: the hung one, or one
        // of the machine's own filesystems that the read stopped before, such as its program.
        const held = holding(lines, `${mark} (${data}, which `);
        expect(held).toHaveLength(1);
        const [path = "", ...rest] = (
          held[0]?.split(`bun ${holds.pid} may hold, as it names `)[1] ?? ""
        ).split(", ");
        const reason = rest.join(", ");
        const hungReason = `${never === named[0] ? "which" : `under ${never}, which`} answered no stat within ${readerMs / 1000} s`;
        const stoppedReason = `which the read did not reach, as ${never}, before it, answered no stat within ${readerMs / 1000} s`;
        expect(
          (path === named[0] && reason.startsWith(hungReason)) || reason.startsWith(stoppedReason)
        ).toBe(true);
        expect(holding(lines, `: the read of what holds ${data} waits no longer on `)).toEqual([
          expect.stringContaining(
            `waits no longer on ${never}, which answered no stat within ${readerMs / 1000} s, nor asks a path under it, and reads none of the `
          ),
        ]);
        const waited = clockOf(clock);
        expect(waited.length).toBeGreaterThan(0);
        for (const { ticks } of waited) {
          expect((ticks.at(-1) ?? Number.NaN) - (ticks[0] ?? Number.NaN)).toBeLessThan(
            readerMs / 1000 + clockRoomS
          );
        }
      } finally {
        if (sweeping !== undefined) await endChild(sweeping);
        await holds.stop();
        rmSync(tmp, { recursive: true, force: true });
      }
    },
    // What the row waits for, summed by the fixtures' program (`rowMs`): a holder and its
    // arguments, and one sweep with its holder read.
    rowMs(neverAnswers)
  );
}

/** What the next row waits for (`Waits`): one holder, a child, with its arguments; and one sweep,
 *  a child, whose holder read reads the one mount that never answers in a group of its own. The
 *  read waits one bound there, whatever else that mount holds: it stops at the path that answered
 *  no stat and reads none of the mount's paths after it, so the group starts one reader and no
 *  more (`identities` of `e2e/harness-postgres-processes.ts`). */
const hungMountAfter: Waits = { children: 2, reads: 1, bounds: [claimEventMs] };

// One mount that never answers, holding two paths of its own, neither under the other, which one
// holder names. A read of that mount that went on to the path after the hung one would start a
// second reader of that group, a second bound this row's limit does not count, and the row's own
// check of what it waits for would stop it. That is what the `dispatch-ui` job and the harness
// lane's rounds 1 and 2 met at `c07d6906ed5` in run 44647294777, under load, where a path of a
// group answered no stat and the group read on: "the row's limit counts 1 holder reads, and its
// sweeps made 2". The read now stops at the hung path on every mount, as it already did on the
// machine's own filesystems, and names the paths it leaves unread.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start whose holder read meets a mount that never answers reads none of that mount's paths after the hung one, so the read costs one bound and no more",
  async () => {
    const row = rowOf(hungMountAfter);
    const { tmp, run } = scratch();
    const data = join(run, "worker-0", "pgdata");
    leaveData(data);
    const mark = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
    const hung = `/never-answers-${process.pid}`;
    const first = join(hung, "a");
    const after = join(hung, "b");
    const clock = join(tmp, "never-answers-clock");
    row.child("the holder of the hung mount's two paths");
    const holds = standIn(tmp, [process.execPath, "--eval", untilParentEnds, first, after]);
    let sweeping: Subprocess | undefined;
    try {
      await until(
        row,
        () => readFileSync(`/proc/${holds.pid}/cmdline`, "utf8").includes(after),
        "the holder's arguments",
        claimEventMs
      );
      // The mount never answers at its first path alone: its path after that one answers at once,
      // so a read that asked it would answer, and only a read that stops leaves it unread.
      const reader = neverAnswering([first], clock, join(tmp, "mountinfo"), readerMs, [
        { point: hung, type: "nfs4" },
      ]);
      const started = startSweep(row, tmp, { reader });
      sweeping = started.child;
      const limit = waitsMs({ children: 1, reads: hungMountAfter.reads, bounds: [] });
      const lines = await Promise.race([started.lines, Bun.sleep(limit).then(() => undefined)]);
      if (lines === undefined) throw new Error(`the sweep did not end within ${limit / 1000} s`);

      expect([existsSync(data), existsSync(mark)]).toEqual([true, true]);
      expect(holding(lines, `: the read of what holds ${data} waits no longer on `)).toEqual([
        expect.stringContaining(
          `waits no longer on ${first}, which answered no stat within ${readerMs / 1000} s, nor asks a path under it, and reads none of the 1 paths of ${hung} after it: ${after}`
        ),
      ]);
      // One reader of that group, and one bound: its own clock, which no load stretches.
      const waited = clockOf(clock);
      expect(waited).toHaveLength(1);
    } finally {
      if (sweeping !== undefined) await endChild(sweeping);
      await holds.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): a holder and its
  // arguments, and one sweep with its holder read.
  rowMs(hungMountAfter)
);

/** What each of the next rows waits for (`Waits`): two holders, each a child, with their
 *  arguments, each at most `claimEventMs`; and one sweep, a child, whose holder read before its
 *  claim of the data directory reads its groups of paths at the same time, so each of the two
 *  mounts that never answer starts one reader, and the two wait out one bound together. */
const twoHungMounts: Waits = { children: 3, reads: 1, bounds: [claimEventMs, claimEventMs] };

// Two mounts whose servers hang, as two network or FUSE mounts of one machine can at once: a start
// reads the paths of each such mount, and of each mount of a type it does not know or that the
// mount table does not give, in a group of its own, and the groups at the same time, so the two
// cost one bound, `readerMs`, and not one after the other, twice that; and a path of the
// machine's own filesystems, which answer, is read all the same. The row runs the start's read on a
// mount table of its own (`neverAnswering`): this process's, with the two hung directories planted
// as two mounts (`/never-answers-a-<pid>`, `/never-answers-b-<pid>`), which the stand-in reader
// never answers. Its own clock, which no load stretches, times each wait: each hung reader was forked
// before the other's wait ended. The healthy holder holds the data directory only through a link,
// which only a read of where it leads, and no spelling of the mark, shows.
for (const mounts of [
  { title: "two network mounts that never answer", types: ["nfs4", "nfs4"] },
  {
    title: "a network mount and a mount of a type it does not know, which never answer",
    types: ["nfs4", "weirdfs"],
  },
  {
    title:
      "a network mount and a mount whose type the mount table does not give, which never answer",
    types: ["nfs4", ""],
  },
] as const) {
  test.skipIf(!onLinux)(
    `v-no-leftover-postgres: a start whose holder read meets ${mounts.title} waits on both for one bound, names each on a line, and still reads the paths of the machine's own filesystems, so it names the process that holds the data directory through it`,
    async () => {
      const row = rowOf(twoHungMounts);
      const { tmp, run } = scratch();
      const data = join(run, "worker-0", "pgdata");
      leaveData(data);
      const mark = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
      const hung = [`/never-answers-a-${process.pid}`, `/never-answers-b-${process.pid}`];
      const named = [join(hung[0] ?? "", "x"), join(hung[1] ?? "", "y")];
      const link = join(tmp, "data-link");
      symlinkSync(data, link);
      const clock = join(tmp, "never-answers-clock");
      row.child("the holder of the two hung paths");
      const holds = standIn(tmp, [process.execPath, "--eval", untilParentEnds, ...named]);
      row.child("the holder of the data directory through a link");
      const healthy = standIn(tmp, [
        process.execPath,
        "--eval",
        untilParentEnds,
        join(link, "PG_VERSION"),
      ]);
      let sweeping: Subprocess | undefined;
      try {
        for (const [holder, last] of [
          [holds, named.at(-1) ?? ""],
          [healthy, link],
        ] as const) {
          await until(
            row,
            () => readFileSync(`/proc/${holder.pid}/cmdline`, "utf8").includes(last),
            "a holder's arguments",
            claimEventMs
          );
        }

        const reader = neverAnswering(
          hung,
          clock,
          join(tmp, "mountinfo"),
          readerMs,
          hung.map((point, index) => ({ point, type: mounts.types[index] ?? "" }))
        );
        const started = startSweep(row, tmp, { reader });
        sweeping = started.child;
        // A bound on another process, which no fake clock of this one reaches: the row's own end
        // for a start that waits on the paths for ever, the sweep and its holder read as the
        // fixtures' program sums them.
        const limit = waitsMs({ children: 1, reads: twoHungMounts.reads, bounds: [] });
        const lines = await Promise.race([started.lines, Bun.sleep(limit).then(() => undefined)]);
        if (lines === undefined) throw new Error(`the sweep did not end within ${limit / 1000} s`);

        const waited = clockOf(clock);
        // What the waits cost together, from the first hung reader's fork to the last tick, on
        // the stand-in's own clock, printed before any check of it, for the body and for a run
        // that fails.
        const forks = Math.min(...waited.map(({ forked }) => forked));
        const last = Math.max(...waited.map(({ ticks }) => ticks.at(-1) ?? Number.NaN));
        console.error(
          `two hung mounts (${mounts.types.map((type) => type || "no type").join(", ")}): ${waited.length} hung readers, ${(last - forks).toFixed(1)} s from the first hung reader's fork to the last tick`
        );
        expect([existsSync(data), existsSync(mark)]).toEqual([true, true]);
        const why = `under ${hung[0]}, which answered no stat within ${readerMs / 1000} s`;
        expect(holding(lines, `${mark} (${data}, which `)).toEqual([
          expect.stringContaining(
            `${data}, which bun ${healthy.pid} still holds and bun ${holds.pid} may hold, as it names ${named[0]}, ${why})`
          ),
        ]);
        expect(
          holding(lines, `: the read of what holds ${data} waits no longer on `).sort()
        ).toEqual(
          hung.map((path) =>
            expect.stringContaining(
              `waits no longer on ${path}, which answered no stat within ${readerMs / 1000} s, nor asks a path under it: `
            )
          )
        );
        expect(waited).toHaveLength(2);
        const [first, second] = waited;
        if (first === undefined || second === undefined) throw new Error("no two hung readers");
        // One bound for both: each hung reader was forked before the other's wait ended, on the
        // stand-in's own clock, so neither waited for the other. One after the other, the second
        // is forked only once the first's bound has run out, after its last tick.
        expect(second.forked).toBeLessThan(first.ticks.at(-1) ?? Number.NaN);
        expect(first.forked).toBeLessThan(second.ticks.at(-1) ?? Number.NaN);
        for (const { ticks } of waited) {
          expect((ticks.at(-1) ?? Number.NaN) - (ticks[0] ?? Number.NaN)).toBeLessThan(
            readerMs / 1000 + clockRoomS
          );
        }
      } finally {
        if (sweeping !== undefined) await endChild(sweeping);
        await holds.stop();
        await healthy.stop();
        rmSync(tmp, { recursive: true, force: true });
      }
    },
    // What the row waits for, summed by the fixtures' program (`rowMs`): two holders and their
    // arguments, and one sweep with its holder read.
    rowMs(twoHungMounts)
  );
}

/** What the next row waits for (`Waits`): two holders, each a child, with their arguments, each at
 *  most `claimEventMs`; and one sweep, a child, with its one holder read before its claim of the
 *  data directory, which a holder holds. */
const kernelTypeMount: Waits = { children: 3, reads: 1, bounds: [claimEventMs, claimEventMs] };

// A mount of a type the kernel answers itself, such as the `binfmt_misc` a box mounts on
// `/proc/sys/fs/binfmt_misc`, is one of the machine's own filesystems: a start reads its paths in
// the one reader of those (`kernelAnswered` of e2e/harness-postgres-processes.ts) and starts no
// reader more for it. On 2026-11-19 a process of another session named that path, and each holder
// read of a start on that machine started a second reader for it. The row plants such a mount in
// the start's mount table (`neverAnswering`, with no path that never answers) and a holder that
// names a path on it, beside a holder of the data directory through a link. Each reader writes the
// group of paths it reads when it starts, so the row reads which groups the read made: none is the
// planted mount's, whatever other mounts the processes of this user name on the machine.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a start whose holder read meets a binfmt_misc mount reads its paths in the one reader of the machine's own filesystems, and names the process that holds the data directory",
  async () => {
    const row = rowOf(kernelTypeMount);
    const { tmp, run } = scratch();
    const data = join(run, "worker-0", "pgdata");
    leaveData(data);
    const mark = writeMark(run, owners().gone, data, mkdtempSync(join(tmp, "no-bin-")));
    const point = `/binfmt-misc-${process.pid}`;
    const named = join(point, "register");
    const link = join(tmp, "data-link");
    symlinkSync(data, link);
    const clock = join(tmp, "readers-clock");
    row.child("the holder of a path on the binfmt_misc mount");
    const onMount = standIn(tmp, [process.execPath, "--eval", untilParentEnds, named]);
    row.child("the holder of the data directory through a link");
    const healthy = standIn(tmp, [
      process.execPath,
      "--eval",
      untilParentEnds,
      join(link, "PG_VERSION"),
    ]);
    let sweeping: Subprocess | undefined;
    try {
      for (const [holder, last] of [
        [onMount, named],
        [healthy, link],
      ] as const) {
        await until(
          row,
          () => readFileSync(`/proc/${holder.pid}/cmdline`, "utf8").includes(last),
          "a holder's arguments",
          claimEventMs
        );
      }

      const reader = neverAnswering([], clock, join(tmp, "mountinfo"), readerMs, [
        { point, type: "binfmt_misc" },
      ]);
      const started = startSweep(row, tmp, { reader });
      sweeping = started.child;
      // A bound on another process, which no fake clock of this one reaches: the sweep and its
      // holder read as the fixtures' program sums them.
      const limit = waitsMs({ children: 1, reads: kernelTypeMount.reads, bounds: [] });
      const lines = await Promise.race([started.lines, Bun.sleep(limit).then(() => undefined)]);
      if (lines === undefined) throw new Error(`the sweep did not end within ${limit / 1000} s`);

      // The group of each reader the read started, from its line "<pid> forked <at> <read> <top>".
      const groups = readFileSync(clock, "utf8")
        .split("\n")
        .filter((line) => line.split(" ")[1] === "forked")
        .map((line) => line.split(" ").slice(4).join(" "));
      console.error(`a binfmt_misc mount: the read started readers for ${JSON.stringify(groups)}`);
      expect(groups).toContain("the machine's own filesystems");
      expect(groups).not.toContain(point);
      expect([existsSync(data), existsSync(mark)]).toEqual([true, true]);
      expect(holding(lines, `${mark} (${data}, which `)).toEqual([
        expect.stringContaining(`${data}, which bun ${healthy.pid} still holds)`),
      ]);
      expect(holding(lines, `: the read of what holds ${data} waits no longer on `)).toEqual([]);
    } finally {
      if (sweeping !== undefined) await endChild(sweeping);
      await onMount.stop();
      await healthy.stop();
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): two holders and their
  // arguments, and one sweep with its holder read.
  rowMs(kernelTypeMount)
);
