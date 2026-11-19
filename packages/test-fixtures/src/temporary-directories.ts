import { afterAll, afterEach, beforeAll } from "bun:test";
import { randomBytes } from "node:crypto";
import { type Dirent, mkdirSync, mkdtempSync, realpathSync, type Stats } from "node:fs";
import { chmod, lstat, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

/**
 * bun's bound on the hooks that remove this fixture's directories: none. A removal is the disk's
 * work, and on a loaded machine with a full disk it passed bun's unstated 5 s, which failed rows
 * whose behaviour held: the 160-entry tree a child `bun` leaves under its HOME took up to 4 s to
 * remove at a 1-minute load of 550 on 288 CPUs. bun 1.4.4 gives a hook whose bound is infinite no
 * deadline of its own, under `bun test --timeout` too, which otherwise sets the hooks' default. A
 * removal that never ends is ended by the run's own deadline, after `REMOVAL_NOTE_MS` has named it.
 */
const NO_BOUND = Number.POSITIVE_INFINITY;

/** How long a removal runs before its hook names the directories on stderr, in milliseconds. The
 *  note decides nothing: it says what a run that its own deadline ends was removing. */
export const REMOVAL_NOTE_MS = 10_000;

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
 * refusals together at the end. It waits for the removal itself, with no bound on the hook
 * (`NO_BOUND`), and names the directories once the removal has run `REMOVAL_NOTE_MS`.
 */
export function temporaryDirectories(
  prefix: string,
  options: TemporaryDirectoriesOptions = {}
): TemporaryDirectoryMaker {
  const root = options.root ?? tmpdir();
  const made: string[] = [];
  afterEach(() => removeDirectories(made.splice(0)), NO_BOUND);
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
 * somehow exists already fails the file rather than handing two suites one directory. The
 * `afterAll` removes the directory as {@link temporaryDirectories}' `afterEach` does: it waits for
 * the removal, with no bound on the hook.
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
  beforeAll(() => {
    mkdirSync(directory, { mode: 0o700 });
  });
  afterAll(() => removeDirectories([directory]), NO_BOUND);
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
 * the directory holding it, and waits for the removal itself. Every tree is attempted even when one
 * of them refuses, since the directories are independent and one refusal must not leave the rest
 * on disk, and the refusals are reported together at the end: a hook that stopped at the first
 * failure would leave every later directory behind and name only the one it stopped on (the
 * behaviour `packages/envoy-client/test/temp-dir.ts` had, which this fixture took over from it).
 * Once the removal has run `REMOVAL_NOTE_MS`, it names the directories on stderr and goes on
 * waiting. `removeOne` is the removal of one tree, `removeTree` unless a test of this module holds
 * it.
 */
export async function removeDirectories(
  directories: readonly string[],
  removeOne: (directory: string) => Promise<void> = removeTree
): Promise<void> {
  if (directories.length === 0) return;
  // A real timer: the removal is the filesystem's work, which bun's fake timers cannot advance,
  // and the note only names it.
  const note = setTimeout(() => {
    console.warn(
      `still removing ${directories.join(" ")} after ${REMOVAL_NOTE_MS} ms; the hook waits for the removal to finish`
    );
  }, REMOVAL_NOTE_MS);
  const failures: string[] = [];
  try {
    for (const directory of [...directories].reverse()) {
      try {
        await removeOne(directory);
      } catch (error) {
        failures.push(`${directory}: ${error instanceof Error ? error.message : String(error)}`);
      }
    }
  } finally {
    clearTimeout(note);
  }
  if (failures.length > 0) {
    throw new Error(`temporary directories left behind:\n  ${failures.join("\n  ")}`);
  }
}

/**
 * Removes one tree, making every directory in it writable, searchable and readable first.
 * `rm` alone fails on a tree a test made read-only on purpose, and a test can make one without
 * meaning to: Go's module cache writes 0555 directories and 0444 files, so a suite that runs `go`
 * under its temporary directory leaves a tree nothing can unlink.
 */
async function removeTree(directory: string): Promise<void> {
  await makeWritable(directory);
  await rm(directory, { recursive: true, force: true });
}

/** `chmod -R u+rwx` on a tree's directories, following no symlink. A path already gone is
 * nothing to do. Only a directory is visited: an entry its directory lists as a file or a
 * symlink (`Dirent`) is left alone. */
async function makeWritable(path: string): Promise<void> {
  const entry = await makeSelfWritable(path);
  if (entry === undefined || !entry.isDirectory()) return;
  let entries: Dirent[];
  try {
    entries = await readdir(path, { withFileTypes: true });
  } catch {
    return;
  }
  for (const child of entries) {
    if (child.isDirectory()) await makeWritable(join(path, child.name));
  }
}

/**
 * `chmod u+rwx` on one path when it is a directory, following no symlink. Answers what it found
 * there, or nothing for a path that is gone or a symlink.
 *
 * A file's own mode is left alone: unlinking a file needs write permission on the directory
 * holding it, never on the file, and a file in a temporary tree can be a hard link to one
 * outside it, whose mode is the same inode's.
 */
async function makeSelfWritable(path: string): Promise<Stats | undefined> {
  let entry: Stats;
  try {
    entry = await lstat(path);
  } catch {
    return undefined;
  }
  if (entry.isSymbolicLink()) return undefined;
  if (!entry.isDirectory()) return entry;
  const mode = entry.mode & 0o7777;
  const wanted = mode | 0o700;
  if (wanted !== mode) {
    try {
      await chmod(path, wanted);
    } catch {
      // A mode this user cannot set is left to rm to report.
    }
  }
  return entry;
}
