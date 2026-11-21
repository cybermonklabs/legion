import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

const census = join(import.meta.dir, "bun-cache-census.ts");
const makeDirectory = temporaryDirectories("bun-cache-census");
const withoutStrace = skipWithout("strace");

/** A module larger than the 4 KB that Bun keeps a transpiled copy of. */
function writeLargeModule(directory: string): string {
  const keys = Array.from({ length: 4000 }, (_, index) => `  k${index}: ${index},`);
  const file = join(directory, "large.ts");
  writeFileSync(file, `export const large: Record<string, number> = {\n${keys.join("\n")}\n};\n`);
  return file;
}

/**
 * Runs the census over a parent Bun that starts one Bun child running `module` under `childEnv`,
 * as a test does, and returns the census's exit status and what it printed.
 */
function censusOfChild(childEnv: Record<string, string>, module: string) {
  const parent = `const r = Bun.spawnSync([process.execPath, ${JSON.stringify(module)}], { env: ${JSON.stringify(childEnv)} }); process.exit(r.exitCode);`;
  const run = Bun.spawnSync([process.execPath, census, "--", process.execPath, "-e", parent], {
    stdout: "pipe",
    stderr: "pipe",
  });
  return { code: run.exitCode, stderr: run.stderr.toString() };
}

test.skipIf(withoutStrace)(
  "a Bun child under a HOME of its own without the key is two violations",
  () => {
    const home = makeDirectory("home");
    const { code, stderr } = censusOfChild(
      { PATH: process.env.PATH ?? "", HOME: home },
      writeLargeModule(makeDirectory("module"))
    );
    expect(code).toBe(1);
    expect(stderr).toContain("without BUN_RUNTIME_TRANSPILER_CACHE_PATH");
    expect(stderr).toContain("a transpiler cache file was written in");
    expect(stderr).toContain(", 2 violations");
  }
);

test.skipIf(withoutStrace)("the same child with bunCacheOff is clean", () => {
  const home = makeDirectory("home");
  const { code, stderr } = censusOfChild(
    { ...bunCacheOff, PATH: process.env.PATH ?? "", HOME: home },
    writeLargeModule(makeDirectory("module"))
  );
  expect(stderr).toContain(", 0 violations");
  expect(stderr).toMatch(/[1-9]\d* of them Bun/);
  expect(code).toBe(0);
});

test.skipIf(withoutStrace)("a command that fails keeps its own status", () => {
  const run = Bun.spawnSync([process.execPath, census, "--", "false"], { stderr: "pipe" });
  expect(run.exitCode).toBe(1);
  expect(run.stderr.toString()).toContain(", 0 violations");
});

test.skipIf(withoutStrace)("a trace that saw no program is no clean result", () => {
  const run = Bun.spawnSync(
    [process.execPath, census, "--", join(makeDirectory("none"), "no-such-program")],
    { stderr: "pipe" }
  );
  expect(run.exitCode).toBe(2);
  expect(run.stderr.toString()).toContain("the census saw nothing");
});
