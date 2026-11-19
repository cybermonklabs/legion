import { afterEach, expect, test } from "bun:test";
import { mkdirSync, readdirSync, rmSync, symlinkSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  cleanAncestor,
  insideCheckout,
  MODULE_PATH_REFUSES,
  outsideModuleBase,
  PLAIN_WORD_REFUSES,
  scratchWithout,
  shellQuote,
  UNQUOTED_WORD_REFUSES,
} from "./test-paths";

const made: string[] = [];
afterEach(() => {
  for (const directory of made.splice(0)) rmSync(directory, { recursive: true, force: true });
});

/** A root of this test's own, under a base holding none of the characters any row plants. */
function root(): string {
  const directory = scratchWithout(
    PLAIN_WORD_REFUSES,
    [os.tmpdir(), ...cleanAncestor(os.tmpdir(), PLAIN_WORD_REFUSES), "/tmp"],
    "test-paths-"
  );
  made.push(directory);
  return directory;
}

test("a directory inside the checkout is refused by its own path or through a symlink", () => {
  const top = root();
  const checkout = path.join(top, "checkout");
  mkdirSync(path.join(checkout, ".cache", "tmp"), { recursive: true });
  symlinkSync(path.join(checkout, ".cache"), path.join(top, "alias"));
  expect(insideCheckout(path.join(checkout, ".cache", "tmp"), checkout)).toContain(
    "inside the checkout"
  );
  expect(insideCheckout(path.join(top, "alias", "tmp"), checkout)).toContain("inside the checkout");
  expect(insideCheckout(checkout, checkout)).toContain("inside the checkout");
  expect(insideCheckout(top, checkout)).toBeUndefined();
});

test("the outside module base is outside the checkout whatever TMPDIR names", () => {
  const top = root();
  const checkout = path.join(top, "checkout");
  mkdirSync(path.join(checkout, ".cache", "tmp-q?x"), { recursive: true });
  symlinkSync(checkout, path.join(top, "alias"));
  const saved = process.env.TMPDIR;
  try {
    for (const tmp of [
      checkout,
      path.join(checkout, ".cache", "tmp-q?x"),
      path.join(top, "alias"),
    ]) {
      process.env.TMPDIR = tmp;
      expect(os.tmpdir()).toBe(tmp);
      expect(insideCheckout(outsideModuleBase(checkout), checkout)).toBeUndefined();
    }
  } finally {
    if (saved === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = saved;
  }
});

test("a base holding a refused character is passed over for the next", () => {
  const top = root();
  for (const name of ["tmp with space", "tmp-q?x", "tmp-sq'x"]) {
    const held = path.join(top, name);
    const plain = path.join(top, "plain");
    mkdirSync(held, { recursive: true });
    const directory = scratchWithout(UNQUOTED_WORD_REFUSES, [held, plain], "row-");
    expect(path.dirname(directory)).toBe(plain);
  }
});

test("a base the accept check refuses is passed over, its directory removed, and the refusal named", () => {
  const top = root();
  const refused = path.join(top, "refused");
  const plain = path.join(top, "plain");
  const accept = (directory: string) =>
    directory.startsWith(refused) ? "no link lands here" : undefined;
  expect(
    path.dirname(scratchWithout(MODULE_PATH_REFUSES, [refused, plain], "row-", { accept }))
  ).toBe(plain);
  expect(readdirSync(refused)).toEqual([]);
  expect(() => scratchWithout(MODULE_PATH_REFUSES, [refused], "row-", { accept })).toThrow(
    `${refused} made a directory that will not do (no link lands here)`
  );
});

test("a base too long for the socket is passed over, and no base at all is a failure naming each", () => {
  const top = root();
  const long = path.join(top, "d".repeat(100));
  mkdirSync(long);
  expect(
    path.dirname(scratchWithout(MODULE_PATH_REFUSES, [long, top], "row-", { socket: "s.sock" }))
  ).toBe(top);
  expect(() => scratchWithout(MODULE_PATH_REFUSES, ["/a/tmp-q?x", "/b/c?d"], "row-")).toThrow(
    '/a/tmp-q?x holds "?"; /b/c?d holds "?"'
  );
});

test("the closest clean ancestor is the directory above that holds no refused character", () => {
  expect(cleanAncestor("/mnt/scratch/run/tmp-q?x", MODULE_PATH_REFUSES)).toEqual([
    "/mnt/scratch/run",
  ]);
  expect(cleanAncestor("/mnt/a b/tmp-q?x/deeper", UNQUOTED_WORD_REFUSES)).toEqual(["/mnt"]);
  expect(cleanAncestor("/mnt/scratch/run/plain", MODULE_PATH_REFUSES)).toEqual([]);
  expect(cleanAncestor("/tmp-q?x", MODULE_PATH_REFUSES)).toEqual([]);
});

test("shellQuote makes any path one word a shell reads back whole", () => {
  const top = root();
  for (const name of ["tmp with space", "tmp-q?x", "tmp-sq'x", "a'b'c", "$(x)"]) {
    const value = path.join(top, name);
    const read = Bun.spawnSync(["sh", "-c", `printf '%s' ${shellQuote(value)}`]);
    expect(read.stdout.toString()).toBe(value);
  }
});

// scratchWithout makes each base but os.tmpdir() when it is missing.
test("a missing base is made", () => {
  const top = root();
  const missing = path.join(top, "missing", "deeper");
  expect(path.dirname(scratchWithout(MODULE_PATH_REFUSES, [missing], "row-"))).toBe(missing);
});
