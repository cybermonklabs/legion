import { fileLimit, ROW_MARGIN_MS, type Wait } from "@legion/test-fixtures/row-clock";

/**
 * A row's or a hook's own work in this process, in milliseconds: its reads, writes and copies of
 * files and its assertions, which no process of its own bounds and a loaded machine stretches. At
 * a load of 401, judge.test.ts's row "a judge whose answer does not cover its input fails the run
 * naming the file", which runs judge.ts six times in this process, ran past bun's 5 s default; at
 * a load of about 690, rule-search.test.ts's afterAll took 36 s to remove its copies of the role
 * texts in this process. 60 s is past both.
 */
export const ROW_WORK_MS = 60_000;

/**
 * The limit of a row of this directory's test files, in milliseconds: the bound of each thing the
 * row waits for, one after another, the row's own work (ROW_WORK_MS), and ROW_MARGIN_MS, which
 * spawnWithin leaves between a process's kill and the row's limit.
 */
export function rowLimit(...waits: readonly Wait[]): number {
  return waits.reduce((sum, wait) => sum + wait.boundMs, ROW_WORK_MS + ROW_MARGIN_MS);
}

/**
 * rowLimit for every row and hook of the calling file, which passes it to `setDefaultTimeout`:
 * the file states it to the row clock of `@legion/test-fixtures/row-clock` (`fileLimit`), which
 * gives each process `spawnWithin` starts no more than its row has left, and gives the fixture's
 * removal of the file's temporary directories a clock of its own at that limit.
 */
export function fileRowLimit(...waits: readonly Wait[]): number {
  return fileLimit(rowLimit(...waits));
}

/**
 * A step of rig.sh run as a process of its own (a `bash -c` of its functions, or `rig.sh
 * <command>`). The build of the heads fixture took 22 to 59 s in 18 of 20 runs at 1-minute loads
 * of 436 to 621 on the devbox, and passed 60 s in the other two; it took 6.8 to 44 s at loads of
 * 530 to 830. Four times the slowest run that finished.
 */
export const RIG_STEP: Wait = { part: "a rig.sh step", boundMs: 240_000 };

/**
 * One git or jj command. In the heads fixture's build at a 1-minute load of 830 the slowest was
 * `jj git clone`, 7.5 s, and a `git commit` took up to 4.6 s.
 */
export const VCS_COMMAND: Wait = { part: "a git or jj command", boundMs: 60_000 };

/**
 * One of this directory's scripts run by bun as a process of its own (`score.ts`,
 * `hidden-proof.ts`, `visible-world.ts`, `gh-standin.ts`). The slowest measured was score.ts over
 * its staged runs, 8.2 s at a load of 268, and a row of score.test.ts that runs it took 37 s at a
 * load of 410.
 */
export const SCRIPT_RUN: Wait = { part: "a bun script", boundMs: 120_000 };

/**
 * One run of label-checkouts.sh: three or four clones of the fixture repository, each with a
 * `bun install`. Its slowest row took 19.4 s at a load of 372.
 */
export const LABEL_CHECKOUTS: Wait = { part: "label-checkouts.sh", boundMs: 120_000 };

/**
 * One batch of rig-isolation.test.ts's rig, its two runs started side by side, so the batch waits
 * for the slower run. Its slowest row took 14 s at a load of 344.
 */
export const RIG_BATCH: Wait = { part: "a batch of rig.sh run", boundMs: 120_000 };
