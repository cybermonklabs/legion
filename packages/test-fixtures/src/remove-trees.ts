/**
 * The removal of temporary-directories.ts, which its hooks run as a process of their own
 * (`bun remove-trees.ts <directory>...`; temporary-directories.ts says why): removes every
 * directory it is given, newest first, so one made inside another goes before the directory
 * holding it. Every tree is attempted even when one of them refuses, since the directories are
 * independent and one refusal must not leave the rest on disk, and the refusals are written
 * together to stderr, with exit 1, at the end: a removal that stopped at the first failure would
 * leave every later directory behind and name only the one it stopped on (the behaviour
 * `packages/envoy-client/test/temp-dir.ts` had, which the fixture took over from it).
 *
 * It imports only `node:` modules, so it runs as a plain bun script, outside `bun test`.
 */
import { chmodSync, lstatSync, readdirSync, rmSync, type Stats } from "node:fs";
import { join } from "node:path";

const failures: string[] = [];
for (const directory of process.argv.slice(2).reverse()) {
  try {
    removeTree(directory);
  } catch (error) {
    failures.push(`${directory}: ${error instanceof Error ? error.message : String(error)}`);
  }
}
if (failures.length > 0) {
  process.stderr.write(`temporary directories left behind:\n  ${failures.join("\n  ")}\n`);
  process.exit(1);
}

/**
 * Removes one tree, making every directory in it writable, searchable and readable first.
 * `rm` alone fails on a tree a test made read-only on purpose, and a test can make one without
 * meaning to: Go's module cache writes 0555 directories and 0444 files, so a suite that runs `go`
 * under its temporary directory leaves a tree nothing can unlink. It removes asynchronously, so a
 * removal waiting on a loaded disk holds the hook alone, never the process's event loop.
 */
function removeTree(directory: string): void {
  makeWritable(directory);
  rmSync(directory, { recursive: true, force: true });
}

/** `chmod -R u+rwx` on a tree's directories, following no symlink. A path already gone is
 * nothing to do. */
async function makeWritable(path: string): Promise<void> {
  const entry = await makeSelfWritable(path);
  if (entry === undefined || !entry.isDirectory()) return;
  let entries: string[];
  try {
    entries = await readdir(path);
  } catch {
    return;
  }
  for (const name of entries) await makeWritable(join(path, name));
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
