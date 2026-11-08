/**
 * `packUpstream`, the half of `scripts/build-upstream-tarball.ts` that writes the committed
 * tarball from a source tree (the other half fetches that tree, and a test of it would fetch
 * 31 MB from api.github.com on every run — the network dependency the tarball exists to remove).
 *
 * The bytes matter rather than the contents: `bun.lock` records the tarball's `sha512`, so a
 * rebuild of a pin already committed must produce the file already committed, or a lockfile check
 * fails for a change nobody made. Each case below pins one of the reasons the bytes are a
 * function of the source tree's contents alone: the member list, the order, the mtimes, the
 * ownership, the modes and the gzip header's timestamp.
 */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
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

/**
 * The fixture tree: the three members the archive holds and one file beside them that it must
 * not. The modes are the caller's umask's, which is what the umask case below varies.
 */
function writeSource(dir: string): string {
  mkdirSync(join(dir, "src", "editor", "plugins"), { recursive: true });
  writeFileSync(join(dir, "src", "lib.ts"), "export const lib = 1;\n");
  writeFileSync(join(dir, "src", "editor", "plugins", "marks.ts"), "export const marks = 2;\n");
  writeFileSync(join(dir, "package.json"), '{ "name": "proof-sdk" }\n');
  writeFileSync(join(dir, "LICENSE"), "MIT\n");
  // Neither under src/ nor one of the two named files, so the archive must not hold it.
  writeFileSync(join(dir, "tsconfig.json"), '{ "compilerOptions": {} }\n');
  return dir;
}

/** `dir`, written under `umask` and packed, with the umask put back whatever happens. */
function packUnderUmask(dir: string, umask: number, outputPath: string): string {
  const previous = process.umask(umask);
  try {
    writeSource(dir);
  } finally {
    process.umask(previous);
  }
  packUpstream({ sourceDir: dir, mtime: MTIME, outputPath });
  return outputPath;
}

/**
 * The same tree with `src/lib.ts` executable, written under `umask`. `writeFileSync`'s mode is
 * masked by the umask, as an extract of the upstream tarball is, so the file lands at 755 under
 * umask 022 and at 700 under umask 077; it is removed first, because a mode is applied when a
 * file is created and not when an existing one is overwritten.
 */
function writeExecutableSource(dir: string, umask: number): string {
  const previous = process.umask(umask);
  try {
    writeSource(dir);
    const executable = join(dir, "src", "lib.ts");
    rmSync(executable);
    writeFileSync(executable, "export const lib = 1;\n", { mode: 0o777 });
  } finally {
    process.umask(previous);
  }
  return dir;
}

beforeAll(() => {
  workDir = mkdtempSync(join(tmpdir(), "proof-sdk-pack-test-"));
  sourceDir = writeSource(join(workDir, "source"));
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

test("every member of a tree with no executable is at mode 644, whatever the umask left", () => {
  for (const line of listing(first)) {
    expect(line.split(/\s+/)[0]).toBe("-rw-r--r--");
  }
});

/**
 * tar records each file's mode as it finds it, and the extract that wrote the upstream tree
 * applied the caller's umask — so without `--mode` one commit packed under umask 022 and under
 * umask 002 gave two sha256 values, which is what `--mode=u=rwX,go=rX` answers.
 */
test("one tree packed under two umasks gives the same bytes", () => {
  const lax = join(workDir, "lax");
  const strict = join(workDir, "strict");
  const laxArchive = packUnderUmask(lax, 0o002, join(workDir, "lax.tgz"));
  const strictArchive = packUnderUmask(strict, 0o022, join(workDir, "strict.tgz"));
  // The two trees differ on disk, which is the difference the archives must not carry.
  expect(statSync(join(lax, "src", "lib.ts")).mode & 0o777).toBe(0o664);
  expect(statSync(join(strict, "src", "lib.ts")).mode & 0o777).toBe(0o644);
  expect(readFileSync(laxArchive)).toEqual(readFileSync(strictArchive));
});

/**
 * The executable bit is the one thing about a member's mode the archive keeps, and the umask may
 * not change what it keeps. `a+r,go-w`, which stood here before, left the execute bits as the
 * extract wrote them: an executable member packed at 755 under umask 022 and at 744 under umask
 * 077, two sha256 values from one commit again. `u=rwX,go=rX` asks only whether anyone may
 * execute the file and writes 755 or 644.
 */
test("an executable member is 755 under either umask, and the bytes are the same", () => {
  const lax = writeExecutableSource(join(workDir, "exec-lax"), 0o022);
  const strict = writeExecutableSource(join(workDir, "exec-strict"), 0o077);
  // 755 and 700 on disk: the same file, written under two umasks.
  expect(statSync(join(lax, "src", "lib.ts")).mode & 0o777).toBe(0o755);
  expect(statSync(join(strict, "src", "lib.ts")).mode & 0o777).toBe(0o700);
  const laxArchive = join(workDir, "exec-lax.tgz");
  const strictArchive = join(workDir, "exec-strict.tgz");
  packUpstream({ sourceDir: lax, mtime: MTIME, outputPath: laxArchive });
  packUpstream({ sourceDir: strict, mtime: MTIME, outputPath: strictArchive });
  for (const archive of [laxArchive, strictArchive]) {
    const modes = new Map(
      listing(archive).map((line) => {
        const fields = line.split(/\s+/);
        return [fields[fields.length - 1], fields[0]];
      })
    );
    expect(modes.get("package/src/lib.ts")).toBe("-rwxr-xr-x");
    expect(modes.get("package/LICENSE")).toBe("-rw-r--r--");
  }
  expect(readFileSync(strictArchive)).toEqual(readFileSync(laxArchive));
});

test("the gzip header carries no timestamp", () => {
  expect(Array.from(readFileSync(first).subarray(4, 8))).toEqual([0, 0, 0, 0]);
});
