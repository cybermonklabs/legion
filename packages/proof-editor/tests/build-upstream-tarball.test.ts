/**
 * `packUpstream`, the half of `scripts/build-upstream-tarball.ts` that writes the committed
 * tarball from a source tree (the other half fetches that tree, and a test of it would fetch
 * 31 MB from api.github.com on every run — the network dependency the tarball exists to remove).
 *
 * The bytes matter rather than the contents: `bun.lock` records the tarball's `sha512`, so a
 * rebuild of a pin already committed must produce the file already committed, or a lockfile check
 * fails for a change nobody made. Each case below pins one of the reasons the bytes are a
 * function of the source tree alone: the member list, the order, the mtimes, the ownership, the
 * permission bits and the gzip header's timestamp.
 */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { packUpstream } from "../scripts/build-upstream-tarball";

const MTIME = "2026-10-17T22:41:59Z";
const MEMBERS = [
  "package/LICENSE",
  "package/package.json",
  "package/src/editor/plugins/marks.ts",
  "package/src/lib.ts",
];

let workDir: string;
let sourceDir: string;
let first: string;
let second: string;

beforeAll(() => {
  workDir = mkdtempSync(join(tmpdir(), "proof-sdk-pack-test-"));
  sourceDir = join(workDir, "source");
  mkdirSync(join(sourceDir, "src", "editor", "plugins"), { recursive: true });
  writeFileSync(join(sourceDir, "src", "lib.ts"), "export const lib = 1;\n");
  writeFileSync(
    join(sourceDir, "src", "editor", "plugins", "marks.ts"),
    "export const marks = 2;\n"
  );
  writeFileSync(join(sourceDir, "package.json"), '{ "name": "proof-sdk" }\n');
  writeFileSync(join(sourceDir, "LICENSE"), "MIT\n");
  // Neither under src/ nor one of the two named files, so the archive must not hold it.
  writeFileSync(join(sourceDir, "tsconfig.json"), '{ "compilerOptions": {} }\n');
  // 0600, because `writeFileSync` would otherwise leave whatever this process's umask leaves, and
  // the archive must read the same under either: the mode case below has nothing to prove against
  // a tree that is already 0644.
  for (const name of ["src/lib.ts", "src/editor/plugins/marks.ts", "package.json", "LICENSE"]) {
    chmodSync(join(sourceDir, name), 0o600);
  }
  first = join(workDir, "first.tgz");
  second = join(workDir, "second.tgz");
  packUpstream({ sourceDir, mtime: MTIME, outputPath: first });
  packUpstream({ sourceDir, mtime: MTIME, outputPath: second });
});

afterAll(() => {
  rmSync(workDir, { recursive: true, force: true });
});

/** `tar --list --verbose`'s lines for an archive, one per member, with dates in UTC. */
function listing(archive: string): string[] {
  const result = spawnSync(
    "tar",
    ["--list", "--verbose", "--utc", "--numeric-owner", "--file", archive],
    { encoding: "utf8" }
  );
  expect(result.status).toBe(0);
  return result.stdout.trimEnd().split("\n");
}

/** The mode `tar --list --verbose` records for one member of an archive. */
function modeOf(archive: string, member: string): string {
  const line = listing(archive).find((entry) => entry.endsWith(` ${member}`));
  if (line === undefined) throw new Error(`${archive} holds no ${member}`);
  return line.split(/\s+/)[0];
}

test("two runs over one source tree write the same bytes", () => {
  expect(readFileSync(second)).toEqual(readFileSync(first));
});

test("the archive holds the three sources under package/, in sorted order", () => {
  expect(listing(first).map((line) => line.split(" ").pop())).toEqual(MEMBERS);
});

test("a file beside src/, package.json and LICENSE stays out of the archive", () => {
  expect(listing(first).join("\n")).not.toContain("tsconfig.json");
});

test("every member carries the given mtime and is owned by 0:0", () => {
  for (const line of listing(first)) {
    const [, owner, , date, time] = line.split(/\s+/);
    expect(owner).toBe("0/0");
    expect(`${date}T${time}`).toBe("2026-10-17T22:41");
  }
});

test("the gzip header carries no timestamp", () => {
  expect(Array.from(readFileSync(first).subarray(4, 8))).toEqual([0, 0, 0, 0]);
});

test("one source tree at 0644 and the same tree at 0600 write the same bytes", () => {
  // Its own tree, because the cases above read the shared one's bytes and these chmods change them.
  const modeDir = join(workDir, "modes");
  mkdirSync(join(modeDir, "src"), { recursive: true });
  writeFileSync(join(modeDir, "src", "lib.ts"), "export const lib = 1;\n");
  writeFileSync(join(modeDir, "package.json"), '{ "name": "proof-sdk" }\n');
  writeFileSync(join(modeDir, "LICENSE"), "MIT\n");
  const files = [
    join(modeDir, "src", "lib.ts"),
    join(modeDir, "package.json"),
    join(modeDir, "LICENSE"),
  ];

  const readableByAll = join(workDir, "readable-by-all.tgz");
  for (const file of files) {
    chmodSync(file, 0o644);
  }
  packUpstream({ sourceDir: modeDir, mtime: MTIME, outputPath: readableByAll });

  const readableByOwner = join(workDir, "readable-by-owner.tgz");
  for (const file of files) {
    chmodSync(file, 0o600);
  }
  packUpstream({ sourceDir: modeDir, mtime: MTIME, outputPath: readableByOwner });

  expect(readFileSync(readableByOwner)).toEqual(readFileSync(readableByAll));
});

test("every member is recorded as -rw-r--r--, whatever its mode on disk", () => {
  for (const line of listing(first)) {
    expect(line.split(/\s+/)[0]).toBe("-rw-r--r--");
  }
});

test("an executable at 0755 and at 0700 write the same bytes, recorded -rwxr-xr-x", () => {
  // Its own tree again, and the only one holding an executable: the shared fixture is all 0600
  // files, so the case above has nothing to say about the bit this one is about.
  const executableDir = join(workDir, "executable");
  mkdirSync(join(executableDir, "src"), { recursive: true });
  const script = join(executableDir, "src", "cli.ts");
  writeFileSync(script, "#!/usr/bin/env bun\nexport const cli = 3;\n");
  writeFileSync(join(executableDir, "src", "lib.ts"), "export const lib = 1;\n");
  writeFileSync(join(executableDir, "package.json"), '{ "name": "proof-sdk" }\n');
  writeFileSync(join(executableDir, "LICENSE"), "MIT\n");
  for (const name of ["src/lib.ts", "package.json", "LICENSE"]) {
    chmodSync(join(executableDir, name), 0o644);
  }

  const executableByAll = join(workDir, "executable-by-all.tgz");
  chmodSync(script, 0o755);
  packUpstream({ sourceDir: executableDir, mtime: MTIME, outputPath: executableByAll });

  const executableByOwner = join(workDir, "executable-by-owner.tgz");
  chmodSync(script, 0o700);
  packUpstream({ sourceDir: executableDir, mtime: MTIME, outputPath: executableByOwner });

  expect(readFileSync(executableByOwner)).toEqual(readFileSync(executableByAll));
  expect(modeOf(executableByOwner, "package/src/cli.ts")).toBe("-rwxr-xr-x");
});
