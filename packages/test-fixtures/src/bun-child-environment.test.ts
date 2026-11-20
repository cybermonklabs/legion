import { expect, test } from "bun:test";
import { readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { bunCacheOff } from "./bun-child-environment";
import { temporaryDirectories } from "./temporary-directories";

const makeDirectory = temporaryDirectories("test-fixtures-bun-child");

/**
 * A module large enough for Bun to keep its transpiled output (Bun keeps only files larger than
 * 4 KB; a 44-byte module left no entry when measured), written to `directory`.
 */
function writeLargeModule(directory: string): string {
  const keys = Array.from({ length: 4000 }, (_, index) => `  k${index}: ${index},`);
  const file = join(directory, "large.ts");
  writeFileSync(file, `export const large: Record<string, number> = {\n${keys.join("\n")}\n};\n`);
  return file;
}

/** Every entry under `home`, as paths relative to it. */
function entriesUnder(home: string): string[] {
  return readdirSync(home, { recursive: true, encoding: "utf-8" }).sort();
}

/** Runs the module with the same Bun as this test, under `env` alone, and waits for its exit. */
function runUnder(env: Record<string, string>, module: string): number {
  return Bun.spawnSync([process.execPath, module], { env, stdout: "ignore", stderr: "inherit" })
    .exitCode;
}

// The control: the same child, its cache pointed at a directory of its own, writes the module's
// transpiled copy there. If Bun ever stops keeping one for this module, this row says the next one
// no longer shows anything. The row names the directory and hands the child no HOME, so it starts
// no Bun under a moved HOME without the key (`scripts/bun-cache-census.ts` names one that does).
test("a Bun child whose cache is a directory of the test's writes the module's copy there", () => {
  const module = writeLargeModule(makeDirectory("module"));
  const cache = makeDirectory("cache");
  const env = { BUN_RUNTIME_TRANSPILER_CACHE_PATH: cache, PATH: process.env["PATH"] ?? "" };
  expect(runUnder(env, module)).toBe(0);
  expect(entriesUnder(cache).filter((entry) => entry.endsWith(".pile")).length).toBeGreaterThan(0);
});

test("with bunCacheOff, a Bun child under an empty HOME writes nothing there", () => {
  // "0" is one of Bun's two words for no cache, and the one this change uses. A path names a
  // directory, which the row above shows Bun fills, so an empty HOME alone would not show the
  // cache is off.
  expect(bunCacheOff).toEqual({ BUN_RUNTIME_TRANSPILER_CACHE_PATH: "0" });
  const module = writeLargeModule(makeDirectory("module"));
  const home = makeDirectory("home");
  const xdg = makeDirectory("xdg");
  const env = { ...bunCacheOff, PATH: process.env["PATH"] ?? "", HOME: home, XDG_CACHE_HOME: xdg };
  expect(runUnder(env, module)).toBe(0);
  expect(entriesUnder(home)).toEqual([]);
  expect(entriesUnder(xdg)).toEqual([]);
});

// Bun resolves a relative cache path against the child's working directory (measured with Bun
// 1.4.4: `BUN_RUNTIME_TRANSPILER_CACHE_PATH=relcache` wrote `<cwd>/relcache/<hash>.pile`), so a
// Bun that read "0" as a directory name would write there, where the row above does not look.
// This row starts the child in a directory of the test's own and requires that it stay empty.
test("with bunCacheOff, a Bun child writes nothing in its working directory either", () => {
  const module = writeLargeModule(makeDirectory("module"));
  const home = makeDirectory("home");
  const work = makeDirectory("work");
  const run = Bun.spawnSync([process.execPath, module], {
    cwd: work,
    env: { ...bunCacheOff, PATH: process.env["PATH"] ?? "", HOME: home },
    stdout: "ignore",
    stderr: "inherit",
  });
  expect(run.exitCode).toBe(0);
  expect(entriesUnder(work)).toEqual([]);
  expect(entriesUnder(home)).toEqual([]);
});
