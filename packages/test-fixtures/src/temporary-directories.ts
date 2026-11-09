import { afterAll, afterEach, beforeAll } from "bun:test";
import { randomBytes } from "node:crypto";
import {
  chmodSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  realpathSync,
  rmSync,
  type Stats,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/** What {@link temporaryDirectories} takes. */
export type TemporaryDirectoriesOptions = {
  /**
   * The directory the temporary ones are made in, `tmpdir()` by default. It is for a test that
   * needs another filesystem, such as a `tmpfs` for a rename that must cross one
   * (`packages/envoy-client/src/__tests__/envoy-config.test.ts` is the caller).
   */
  root?: string;
};

/** What {@link temporaryDirectory} takes. */
export type TemporaryDirectoryOptions = {
  /**
   * Returns the path under `realpathSync` of `tmpdir()`. A macOS `tmpdir()` is a symlink (`/var`
   * to `/private/var`), so a test that compares a path a child process printed with the path the
   * fixture returned needs the resolved one.
   */
  real?: boolean;
};

/**
 * Makes one fresh directory and returns its path. `suffix` goes into the directory's name, so a
 * test that makes several says which is which.
 */
export type TemporaryDirectoryMaker = (suffix?: string) => string;

/**
 * Registers the removal of every directory the returned maker makes, and returns the maker.
 *
 * Call it at the test file's module scope, or in the body of a `describe`: it registers the
 * `afterEach` that removes the directories where it is called, and `bun:test` takes the scope of
 * a hook from the point of the call. The call itself makes nothing, so a scope whose hooks never
 * run leaves nothing behind.
 *
 * **Call the maker itself from a test body or from a `beforeEach`, never from a `beforeAll` and
 * never at module scope or in a `describe` body.** The maker's only removal is the `afterEach`
 * registered at the call, so a directory made in a `beforeAll` is removed after the first row of
 * the scope and every later row finds it gone; a whole file or `describe` that wants one
 * directory of its own takes {@link temporaryDirectory} instead. The maker makes its directory at
 * the call, and Bun runs a module body and a `describe` body in three cases where it then runs no
 * hook of that scope: a `bun test -t <pattern>` that selects no row of the file, one that selects
 * no row of the `describe`, and `describe.skipIf(true)`. In each of them a maker called there
 * makes its directory and nothing removes it again, and nothing runs at the end of a `bun test`
 * process to catch it.
 * {@link temporaryDirectory} is the form for one directory a whole file or `describe` shares: it
 * reserves the path at the call and makes the directory in a `beforeAll`, so the making and the
 * removal run together or not at all.
 *
 * The fixture's own module body registers nothing, because Bun evaluates an imported module once
 * per process: a hook there would land in the scope of the first file that imported it, and every
 * later file would leave its directories behind.
 *
 * The maker is synchronous, so a test body uses it without `await`.
 *
 * `options.root` is the directory the temporary ones are made in, for a test that needs another
 * filesystem.
 *
 * The `afterEach` attempts every directory even when one of them cannot be removed, since the
 * directories are independent and one refusal must not leave the rest on disk, and reports the
 * refusals together at the end.
 *
 * The `afterEach` carries an explicit hang guard — a limit of its own rather than Bun's
 * unwritten 5 s default, which has failed correct runs of the suites that call this fixture — so
 * a removal that never returns reddens with its test named; the comment beside the hook has the
 * measurements it is sized from.
 */
export function temporaryDirectories(
  prefix: string,
  options: TemporaryDirectoriesOptions = {}
): TemporaryDirectoryMaker {
  const root = options.root ?? tmpdir();
  const made: string[] = [];
  // A hang guard, not a bound on the work: what can hang is removing a tree on a loaded host's
  // filesystem. Timed round the body on this two-CPU pod, and charged by Bun to within 0.3 ms of
  // it: over the 1059 invocations of 3 idle runs of envoy-client's dispatch-execute.test.ts, the
  // consumer with the most rows (353), 0.00 / 0.00 / 0.99 ms (min / median / max), and over the
  // 3530 of 10 runs at that package's rung of 16 busy loops (8 per CPU) 0.00 / 0.01 / 6.81 ms;
  // in pi-envoy's askgate.test.ts 0.01 / 0.31 / 0.74 ms over the 12 invocations of 3 idle runs
  // and 0.02 / 0.36 / 1511.63 ms over the 40 of 10 runs at 240 busy loops (120 per CPU). Bun's
  // unwritten 5 s default for a hook is what failed correct runs of these suites at 5015.84 ms,
  // 11035.86 ms and 15887.94 ms — charges, not bodies, since a starved host charges a hook for
  // the time it is not scheduled. 60_000 is 39 times the worst body measured here and 3.8 times
  // the worst of those charges, and sits well inside the consumers' timeout-minutes: 10
  // (.github/workflows/ci.yaml, 600 s), so a removal that never returns reddens with its test
  // named instead of cancelling the job, which names nothing.
  afterEach(() => {
    removeEvery(made.splice(0));
  }, 60_000);
  return (suffix) => {
    const directory = mkdtempSync(join(root, nameTemplate(prefix, suffix)));
    made.push(directory);
    return directory;
  };
}

/**
 * Reserves one directory for the whole suite and returns its path: the one-shot form of
 * {@link temporaryDirectories}, for a test file or a `describe` body that needs a single scratch
 * directory every one of its tests reads.
 *
 * Call it at the test file's module scope, or in the body of a `describe`, never inside a test
 * body: it registers the `beforeAll` that makes the directory and the `afterAll` that removes it
 * where it is called, and `bun:test` takes the scope of a hook from the point of the call. The
 * path is reserved at the call and nothing is made there, so a scope Bun evaluates and then runs
 * no hook of — a `-t` filter that selects no row of the file or of the `describe`, and
 * `describe.skipIf(true)` — makes nothing and leaves nothing behind.
 *
 * Nothing exists at the path until that `beforeAll` has run. A caller writes into the directory
 * from a test body, or from a hook of its own registered below this call: Bun runs the hooks of a
 * scope in the order they were registered.
 *
 * `real` resolves `tmpdir()` once, at the call, and joins the reserved name onto the resolved
 * parent: `realpathSync` needs a path that exists, and the directory itself does not yet. This
 * form takes no `root`: nothing needs one directory for a whole file on another filesystem, and
 * the maker is what a cross-filesystem row uses.
 *
 * The name is the prefix, a dash and eight random hexadecimal characters, two more than
 * `mkdtemp` adds. The directory is made exclusively, so `mode` applies to it and a path that
 * somehow exists already fails the file rather than handing two suites one directory.
 *
 * Both hooks carry an explicit hang guard — a limit of their own rather than Bun's unwritten 5 s
 * default, which has failed correct runs of the suites that call this fixture — so a making or a
 * removal that never returns reddens with its test named; the comments beside them have the
 * measurements they are sized from.
 */
export function temporaryDirectory(
  prefix: string,
  options: TemporaryDirectoryOptions = {}
): string {
  const parent = options.real === true ? realpathSync(tmpdir()) : tmpdir();
  const directory = join(
    parent,
    `${nameTemplate(prefix, undefined)}${randomBytes(4).toString("hex")}`
  );
  // A hang guard, not a bound on the work: what can hang is making the directory on a loaded
  // host's filesystem. Timed round the body on this two-CPU pod: in pi-envoy's askgate.test.ts
  // 0.08 / 0.13 / 0.30 ms (min / median / max) over the 24 invocations of 3 idle runs and
  // 0.12 / 0.26 / 1064.66 ms over the 80 of 10 runs at 240 busy loops (120 per CPU); in
  // pi-legion's score.test.ts 0.15 / 0.27 / 0.28 ms over 3 idle runs and 0.12 / 0.30 / 157.22 ms
  // over 10 runs at 96 busy loops (48 per CPU). Bun's unwritten 5 s default for a hook is what
  // failed correct runs of these suites at 5015.84 ms, 11035.86 ms and 15887.94 ms — charges,
  // not bodies, since a starved host charges a hook for the time it is not scheduled. 60_000 is
  // 56 times the worst body measured here and 3.8 times the worst of those charges, and sits
  // well inside the consumers' timeout-minutes: 10 (.github/workflows/ci.yaml, 600 s), so a
  // making that never returns reddens with its test named instead of cancelling the job, which
  // names nothing.
  beforeAll(() => {
    mkdirSync(directory, { mode: 0o700 });
  }, 60_000);
  // A hang guard, not a bound on the work: what can hang is removing a tree on a loaded host's
  // filesystem, which for a suite that points TMPDIR at this directory is every file that suite
  // made. Timed round the body on this two-CPU pod, and charged by Bun to within 0.3 ms of it:
  // in pi-envoy's askgate.test.ts 0.38 / 0.97 / 620.02 ms (min / median / max) over the 24
  // invocations of 3 idle runs and 0.42 / 116.55 / 212482.46 ms over the 80 of 10 runs at 240
  // busy loops (120 per CPU), where that suite's own TMPDIR root cost 588.79 / 619.88 /
  // 620.02 ms idle and 42545.54 / 81877.79 / 212482.46 ms loaded; in pi-legion's score.test.ts
  // 9.00 / 398.24 / 804.29 ms idle and 56.41 / 2447.13 / 46650.29 ms over 10 runs at 96 busy
  // loops (48 per CPU). Bun's unwritten 5 s default for a hook is what failed correct runs of
  // these suites at 5015.84 ms, 11035.86 ms and 15887.94 ms, and it failed this hook in 10 of
  // those 10 askgate runs. Three times the worst, 212.5 s, is 637 s, past the 600 s the
  // consumers' timeout-minutes: 10 allows (.github/workflows/ci.yaml), so that ceiling rules and
  // 300_000 is half of it, 1.4 times that worst. Plainly: at 120 busy loops per CPU, a load far
  // past anything CI applies, removing a large tree can cost minutes, so this guard catches a
  // removal that never returns and nothing smaller, and leaves the job room to redden with this
  // test named rather than cancelling, which names nothing.
  afterAll(() => {
    removeTree(directory);
  }, 300_000);
  return directory;
}

/** `-`, the one character the name template trims from a caller's prefix. */
const DASH = 45;

/**
 * The name both forms build on: the prefix, the suffix when the caller named one, and one dash,
 * whether or not the prefix already ends with dashes. `temporaryDirectories` hands it to
 * `mkdtemp`, which adds six random characters to it; `temporaryDirectory` adds eight of its own.
 *
 * The trailing dashes are counted rather than matched: a `/-+$/` on a caller's own string is a
 * polynomial regular expression on library input, which CodeQL refuses as a high-severity alert
 * (a prefix of many dashes would make it slow), and this costs one pass of the string's tail.
 */
function nameTemplate(prefix: string, suffix: string | undefined): string {
  const stem = suffix === undefined || suffix === "" ? prefix : `${prefix}-${suffix}`;
  let end = stem.length;
  while (end > 0 && stem.charCodeAt(end - 1) === DASH) end -= 1;
  return `${stem.slice(0, end)}-`;
}

/**
 * Removes every directory of `directories`, newest first, so one made inside another goes before
 * the directory holding it. Every tree is attempted even when one of them refuses, since the
 * directories are independent and one refusal must not leave the rest on disk, and the refusals
 * are reported together at the end: a hook that stopped at the first failure would leave every
 * later directory behind and name only the one it stopped on (the behaviour
 * `packages/envoy-client/test/temp-dir.ts` had, which this fixture took over from it).
 */
function removeEvery(directories: readonly string[]): void {
  const failures: string[] = [];
  for (const directory of [...directories].reverse()) {
    try {
      removeTree(directory);
    } catch (error) {
      failures.push(`${directory}: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
  if (failures.length > 0) {
    throw new Error(`temporary directories left behind:\n  ${failures.join("\n  ")}`);
  }
}

/**
 * Removes one tree, making every directory in it writable, searchable and readable first.
 * `rmSync` alone fails on a tree a test made read-only on purpose, and a test can make one
 * without meaning to: Go's module cache writes 0555 directories and 0444 files, so a suite that
 * runs `go` under its temporary directory leaves a tree nothing can unlink.
 */
function removeTree(directory: string): void {
  makeWritable(directory);
  rmSync(directory, { recursive: true, force: true });
}

/** `chmod -R u+rwx` on a tree's directories, following no symlink. A path already gone is
 * nothing to do. */
function makeWritable(path: string): void {
  const entry = makeSelfWritable(path);
  if (entry === undefined || !entry.isDirectory()) return;
  let entries: string[];
  try {
    entries = readdirSync(path);
  } catch {
    return;
  }
  for (const name of entries) makeWritable(join(path, name));
}

/**
 * `chmod u+rwx` on one path when it is a directory, following no symlink. Answers what it found
 * there, or nothing for a path that is gone or a symlink.
 *
 * A file's own mode is left alone: unlinking a file needs write permission on the directory
 * holding it, never on the file, and a file in a temporary tree can be a hard link to one
 * outside it, whose mode is the same inode's.
 */
function makeSelfWritable(path: string): Stats | undefined {
  let entry: Stats;
  try {
    entry = lstatSync(path);
  } catch {
    return undefined;
  }
  if (entry.isSymbolicLink()) return undefined;
  if (!entry.isDirectory()) return entry;
  const mode = entry.mode & 0o7777;
  const wanted = mode | 0o700;
  if (wanted !== mode) {
    try {
      chmodSync(path, wanted);
    } catch {
      // A mode this user cannot set is left to rmSync to report.
    }
  }
  return entry;
}
