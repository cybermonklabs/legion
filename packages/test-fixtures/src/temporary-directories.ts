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
  rmdirSync,
  rmSync,
  type Stats,
  statfsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { HANG_GUARD_MS } from "./hang-guard";

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
    .filter((fields) => mountPoint(fields[4] ?? "") === at)
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

/** A mount point as the path it names: mountinfo writes a space, a tab, a newline and a
 *  backslash in it as `\` and three octal digits, so a line still splits on spaces. */
function mountPoint(field: string): string {
  return field.replace(/\\([0-7]{3})/g, (_, octal: string) =>
    String.fromCharCode(Number.parseInt(octal, 8))
  );
}

/** The longest unix socket path Linux takes: its sun_path holds 108 bytes with the terminating
 *  zero (the Go tests' testtmp.MaxSocketPath). */
const MAX_SOCKET_PATH = 107;

/** What {@link scratchWithout} takes. */
export type ScratchWithoutOptions = {
  /** The longest path, relative to the directory, that the test binds a unix socket at. */
  readonly socket?: string | undefined;
  /** Why a directory made under a base will not do (a hardlink that cannot reach it), or
   *  undefined. A directory it refuses or throws on is removed with whatever it left there. */
  readonly accept?: ((directory: string) => string | undefined) | undefined;
};

/** `refuses` as a pattern that finds every character it matches anywhere in a path: global and
 *  not sticky, whatever flags the caller gave it, since a sticky one matches only at its start. */
function everyMatch(refuses: RegExp): RegExp {
  return refuses.global && !refuses.sticky
    ? refuses
    : new RegExp(refuses.source, `${refuses.flags.replace(/[gy]/g, "")}g`);
}

/**
 * A fresh directory, made now with `prefix`, whose path holds no character `refuses` matches, for
 * a path some tool cannot take a character of, however it is quoted: Bun loads no module from a
 * path that holds a `?`, for one. The directory is made under the first of `bases` that holds
 * none, takes a directory (each but tmpdir() is made when it is missing), when `socket` names the
 * longest path relative to the directory that the test binds a unix socket at, leaves that path
 * within the 107 bytes Linux takes, and passes `accept`. With none, it throws naming what each
 * base answered; it never skips. Only the bases are judged: the prefix is the caller's, and a
 * refused character in it is kept. The caller removes the directory.
 */
export function scratchWithout(
  refuses: RegExp,
  bases: readonly string[],
  prefix: string,
  options: ScratchWithoutOptions = {}
): string {
  const every = everyMatch(refuses);
  const answers: string[] = [];
  for (const base of new Set(bases)) {
    const held = [...new Set(base.match(every))];
    if (held.length > 0) {
      answers.push(`${base} holds ${held.map((char) => JSON.stringify(char)).join(" and ")}`);
      continue;
    }
    let directory: string;
    try {
      if (base !== tmpdir()) mkdirSync(base, { recursive: true, mode: 0o700 });
      directory = mkdtempSync(join(base, prefix));
    } catch (error) {
      answers.push(`${base} made no directory (${messageOf(error)})`);
      continue;
    }
    if (options.socket !== undefined) {
      const bytes = Buffer.byteLength(join(directory, options.socket));
      if (bytes > MAX_SOCKET_PATH) {
        rmSync(directory, { recursive: true, force: true });
        answers.push(`${base} gives ${options.socket} a path of ${bytes} bytes`);
        continue;
      }
    }
    let refusal: string | undefined;
    try {
      refusal = options.accept?.(directory);
    } catch (error) {
      refusal = `its check threw: ${messageOf(error)}`;
    }
    if (refusal !== undefined) {
      rmSync(directory, { recursive: true, force: true });
      answers.push(`${base} made a directory that will not do (${refusal})`);
      continue;
    }
    return directory;
  }
  const fits =
    options.socket === undefined
      ? ""
      : ` and leaves ${options.socket} within the ${MAX_SOCKET_PATH} bytes a unix socket path holds`;
  throw new Error(
    `no base gives a directory whose path holds no character of ${refuses.source}${fits}: ${answers.join("; ")}`
  );
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** The base {@link scratchWithout} would make its directory under, found by making one there and
 *  removing it at once: the `root` a {@link temporaryDirectories} maker takes. */
export function baseWithout(
  refuses: RegExp,
  bases: readonly string[],
  accept?: (directory: string) => string | undefined
): string {
  const probe = scratchWithout(refuses, bases, "probe-", { accept });
  rmdirSync(probe);
  return dirname(probe);
}

/** The closest directory above `directory`, below the root, whose path holds no character
 *  `refuses` matches, as a list of none or one: none when `directory` holds none itself, or when
 *  only the root would. A base of the same tree, and most often the same filesystem, to try
 *  before one elsewhere. */
export function cleanAncestor(directory: string, refuses: RegExp): string[] {
  const every = everyMatch(refuses);
  if ((directory.match(every) ?? []).length === 0) return [];
  for (let parent = dirname(directory); parent !== dirname(parent); parent = dirname(parent)) {
    if ((parent.match(every) ?? []).length === 0) return [parent];
  }
  return [];
}

/**
 * The root of the directories a test has Bun load a module from (an import, a `--preload`, a file
 * `bun` runs): Bun loads no module from a path that holds a `?`, in any form. It is `root`,
 * `tmpdir()` unless given, where its path holds none, else its closest ancestor that holds none,
 * else /tmp ({@link baseWithout}). A {@link temporaryDirectories} maker takes it as `root`; every
 * other temporary path of the test stays where it was, so its quoting is still tested there.
 */
export function moduleRoot(root: string = tmpdir()): string {
  return baseWithout(/\?/g, [root, ...cleanAncestor(root, /\?/g), "/tmp"]);
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
 * refusals together at the end. It runs under {@link HANG_GUARD_MS}, not Bun's 5 s hook default.
 */
export function temporaryDirectories(
  prefix: string,
  options: TemporaryDirectoriesOptions = {}
): TemporaryDirectoryMaker {
  const root = options.root ?? tmpdir();
  const made: string[] = [];
  afterEach(() => {
    removeEvery(made.splice(0));
  }, HANG_GUARD_MS);
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
 * somehow exists already fails the file rather than handing two suites one directory. The
 * `afterAll` runs under {@link HANG_GUARD_MS}, not Bun's 5 s hook default.
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
  });
  afterAll(() => {
    removeTree(directory);
  }, HANG_GUARD_MS);
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
