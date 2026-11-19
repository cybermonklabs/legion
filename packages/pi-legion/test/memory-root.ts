import { readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { captureEnvironment } from "@legion/test-fixtures/environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

/**
 * The root for test files that make many entries: `/dev/shm` where `/proc/self/mountinfo` shows a
 * tmpfs mounted there, read-write and without `noexec`, and `tmpdir()` otherwise, or when the file
 * cannot be read. The last mount at `/dev/shm` in the file is the one a path there reaches. On a
 * dev box's shared disk each entry waits for the metadata and I/O every session of the box shares:
 * score.test.ts's staging hook, 626 directories and 3,726 files, took 47,248 ms there in a 2-CPU
 * cgroup and 483 ms on `/dev/shm`, where Bun gives a hook or a test 5 s. `noexec` is refused
 * because a file may run what it writes there (score.ts runs a git link).
 *
 * #4034's `memoryRoot` (`@legion/test-fixtures/temporary-directories`) makes this same choice, and
 * `temporaryDirectory`'s `root` there takes it; they replace this module, whose callers then take
 * `temporaryDirectory(prefix, { root: memoryRoot() })`: whichever of the two pull requests lands
 * second makes that swap. This copy is here because `packages/test-fixtures` is frozen meanwhile.
 */
export function memoryRoot(): string {
  let lines: string[];
  try {
    lines = readFileSync("/proc/self/mountinfo", "utf8").split("\n");
  } catch {
    return tmpdir();
  }
  let found: { fstype: string; options: string[] } | undefined;
  for (const line of lines) {
    // <id> <parent> <major:minor> <root> <mount point> <mount options> [optional fields...] - <fstype> <source> <super options>
    const fields = line.split(" ");
    const separator = fields.indexOf("-", 6);
    if (fields[4] !== "/dev/shm" || separator < 0) continue;
    found = { fstype: fields[separator + 1] ?? "", options: (fields[5] ?? "").split(",") };
  }
  const writable = found?.options.includes("rw") === true && !found.options.includes("noexec");
  return found?.fstype === "tmpfs" && writable ? "/dev/shm" : tmpdir();
}

/**
 * `temporaryDirectory(prefix)`, reserved under `memoryRoot()`: call it where
 * `temporaryDirectory` is called, at a test file's module scope or in a `describe` body. That
 * fixture takes no root at this head and reads `tmpdir()` at its call, so `TMPDIR` names the
 * memory root for that one call and is put back before anything else runs; the fixture still
 * makes the directory in its `beforeAll` and removes it in its `afterAll`.
 */
export function memoryDirectory(prefix: string): string {
  const restore = captureEnvironment(["TMPDIR"]);
  process.env.TMPDIR = memoryRoot();
  try {
    return temporaryDirectory(prefix);
  } finally {
    restore();
  }
}
