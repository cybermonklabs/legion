// The removal of one temporary tree, for temporary-directories.ts: called there inside a hook, and
// run as a process of its own (`bun remove-tree.ts <directory>`) for a directory reserved with
// `removeInBackground`, whose removal no hook of the test runner may wait for.
import type { Stats } from "node:fs";
import { chmod, lstat, readdir, rm } from "node:fs/promises";
import { join } from "node:path";

/**
 * Removes one tree, making every directory in it writable, searchable and readable first.
 * `rm` alone fails on a tree a test made read-only on purpose, and a test can make one without
 * meaning to: Go's module cache writes 0555 directories and 0444 files, so a suite that runs `go`
 * under its temporary directory leaves a tree nothing can unlink. It removes asynchronously, so a
 * removal waiting on a loaded disk holds the hook alone, never the process's event loop.
 */
export async function removeTree(directory: string): Promise<void> {
  await makeWritable(directory);
  await rm(directory, { recursive: true, force: true });
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

// As a process of its own: remove the one directory it is given, and name it on stderr, exiting
// 1, when it cannot.
if (import.meta.main) {
  const directory = process.argv[2] ?? "";
  const leftBehind = (error: unknown): void => {
    console.error(
      `temporary directory left behind: ${directory}: ${error instanceof Error ? error.message : String(error)}`
    );
    process.exitCode = 1;
  };
  if (directory === "") leftBehind(new Error("no directory given"));
  else removeTree(directory).catch(leftBehind);
}
