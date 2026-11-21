import { afterAll, afterEach, beforeAll } from "bun:test";
import { randomBytes } from "node:crypto";
import {
  accessSync,
  chmodSync,
  constants,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  type Stats,
  statfsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/**
 * The bound of each hook this fixture registers, every one of which makes or removes a directory
 * tree and so does real work rather than wait on anything: a removal's cost grows with the tree a
 * caller staged. Bun's default for a hook is 5 s, which nobody chose for that work. Measured on
 * 2026-11-15 in an issue pod with two CPUs, against the tree packages/pi-legion's
 * scripts/skill-scenarios/score.test.ts stages: removing it took 0.74 to 0.79 s with the pod quiet,
 * three runs, and with the pod's CPUs loaded one removal ran 5.32 s and failed that file after all
 * 118 of its rows had passed. Thirty seconds lets a loaded machine finish such a removal, and only
 * lengthens the bound, so it turns no passing suite red: what it reaches is a hook that fails today.
 * The work is synchronous, so Bun cannot interrupt it: a hook that hangs is reported timed out once
 * it returns, not stopped at the bound.
 */
const HOOK_WORK_TIMEOUT_MS = 30_000;

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
   * Returns the path under `realpathSync` of its parent, `root` or else `tmpdir()`. A macOS
   * `tmpdir()` is a symlink (`/var` to `/private/var`), so a test that compares a path a child
   * process printed with the path the fixture returned needs the resolved one.
   */
  real?: boolean;
  /** The directory the one directory is made in, `tmpdir()` by default; see {@link memoryRoot}. */
  root?: string;
};

/** What {@link memoryRoot} takes. */
export type MemoryRootOptions = {
  /** The bytes the tmpfs must have free, 0 by default: see {@link memoryRoot}. */
  need?: number;
  /** The mount table it reads, `/proc/self/mountinfo` by default, for a test of its own. */
  mountinfo?: string;
  /** The mount point it judges, `/dev/shm` by default, for a test of its own. */
  at?: string;
};

/**
 * `/dev/shm` where `mountinfo` shows a tmpfs mounted there read-write and without `noexec`, this
 * process may write in and search it, and it has `need` bytes free; else `tmpdir()`, and `tmpdir()`
 * when it cannot read `mountinfo`. The last mount at `/dev/shm` in the file is the one a process
 * sees. A suite that makes or removes many directory entries inside one row's or hook's time passes
 * it as `root`: on a disk other programs share, one creation can take milliseconds, and the suite's
 * time then follows what those programs do. `noexec` is refused because a suite may write programs
 * into its directory and run them. A caller that holds much there passes `need`: the most one run
 * of its suite holds at once, times the runs that may share the host. A tmpfs is often small (a
 * Kubernetes pod's `/dev/shm` is 64 MiB), keeps what a stopped run never removed, and fails a write
 * past its end with ENOSPC; a tmpfs page holds one file's data however short, so the count of files
 * kept at once sets that most, not their size.
 */
export function memoryRoot(options: MemoryRootOptions = {}): string {
  const at = options.at ?? "/dev/shm";
  const need = options.need ?? 0;
  let text: string;
  try {
    text = readFileSync(options.mountinfo ?? "/proc/self/mountinfo", "utf8");
  } catch {
    return tmpdir();
  }
  // A line is the mount's id, its parent's, the device, the root, the mount point, its options,
  // optional fields, `-`, then the filesystem type.
  const mount = text
    .split("\n")
    .map((line) => line.split(" "))
    .filter((fields) => fields[4] === at)
    .at(-1);
  if (mount === undefined) return tmpdir();
  const flags = (mount[5] ?? "").split(",");
  const type = mount[mount.indexOf("-") + 1];
  if (type !== "tmpfs" || !flags.includes("rw") || flags.includes("noexec")) return tmpdir();
  try {
    accessSync(at, constants.W_OK | constants.X_OK);
    const { bavail, bsize } = statfsSync(at);
    return bavail * bsize >= need ? at : tmpdir();
  } catch {
    return tmpdir();
  }
}

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
 */
export function temporaryDirectories(
  prefix: string,
  options: TemporaryDirectoriesOptions = {}
): TemporaryDirectoryMaker {
  const root = options.root ?? tmpdir();
  const made: string[] = [];
  afterEach(() => {
    removeEvery(made.splice(0));
  }, HOOK_WORK_TIMEOUT_MS);
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
 * `root` is the directory the one directory is made in, `tmpdir()` unless given; it must exist
 * once the `beforeAll` runs. `real` resolves that parent once, at the call, and joins the
 * reserved name onto the resolved parent: `realpathSync` needs a path that exists, and the
 * directory itself does not yet, so with `real` the parent must exist at the call.
 *
 * The name is the prefix, a dash and eight random hexadecimal characters, two more than
 * `mkdtemp` adds. The directory is made exclusively, so `mode` applies to it and a path that
 * somehow exists already fails the file rather than handing two suites one directory.
 */
export function temporaryDirectory(
  prefix: string,
  options: TemporaryDirectoryOptions = {}
): string {
  const root = options.root ?? tmpdir();
  const parent = options.real === true ? realpathSync(root) : root;
  const directory = join(
    parent,
    `${nameTemplate(prefix, undefined)}${randomBytes(4).toString("hex")}`
  );
  beforeAll(() => {
    mkdirSync(directory, { mode: 0o700 });
  }, HOOK_WORK_TIMEOUT_MS);
  afterAll(() => {
    removeTree(directory);
  }, HOOK_WORK_TIMEOUT_MS);
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
