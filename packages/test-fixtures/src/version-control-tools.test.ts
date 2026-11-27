import { afterEach, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { captureEnvironment } from "./environment";
import { skipWithout } from "./host-tools";
import { gitBinary, jjBinary } from "./version-control-tools";

// jjBinary and gitBinary answer the binary the first `jj` and `git` on PATH run, read from the
// running process's executable in /proc, and the first one on PATH itself where there is no /proc
// to read it from. A wrapper first on PATH is what tells the two apart: a script that runs the
// tool, as a version manager's shim or the dotfiles shim does.
const skip = skipWithout("jj", "git");
const withProc = existsSync("/proc/self");

const cleanup: (() => Promise<void> | void)[] = [];
afterEach(async () => {
  for (const undo of cleanup.splice(0).reverse()) await undo();
});

async function scratch(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "test-version-control-tools-"));
  cleanup.push(() => rm(dir, { recursive: true, force: true }));
  return dir;
}

/** Puts a wrapper named `name` that runs `target` first on this process's PATH until the test
 *  ends, and answers its path. */
async function wrapperFirst(dir: string, name: string, target: string): Promise<string> {
  const bin = path.join(dir, `${name}-wrapper`);
  await mkdir(bin);
  const wrapper = path.join(bin, name);
  const quoted = `'${target.replaceAll("'", `'\\''`)}'`;
  await writeFile(wrapper, `#!/bin/sh\nexec ${quoted} "$@"\n`, { mode: 0o755 });
  cleanup.push(captureEnvironment(["PATH"]));
  process.env["PATH"] = `${bin}:${process.env["PATH"] ?? ""}`;
  return wrapper;
}

/** The first `name` on this process's PATH as it is now (`Bun.which` alone reads the PATH the
 *  process started with). */
const firstOnPath = (name: string) => Bun.which(name, { PATH: process.env["PATH"] ?? "" });

const tools = [
  ["jj", jjBinary],
  ["git", gitBinary],
] as const;

for (const [name, resolve] of tools) {
  test.skipIf(skip || !withProc)(
    `with /proc, ${name} first on PATH as a wrapper resolves to the binary the wrapper runs`,
    async () => {
      const dir = await scratch();
      const binary = await resolve(dir);
      const wrapper = await wrapperFirst(dir, name, binary);
      expect(firstOnPath(name)).toBe(wrapper);
      expect(await resolve(dir)).toBe(binary);
    }
  );

  test.skipIf(skip)(
    `with no /proc to read, ${name} resolves to the first ${name} on PATH, as before it read one`,
    async () => {
      const dir = await scratch();
      const onPath = firstOnPath(name);
      if (onPath === null) throw new Error(`${name} is not on PATH`);
      const noProc = path.join(dir, "no-proc");
      expect(await resolve(dir, noProc)).toBe(onPath);
      const wrapper = await wrapperFirst(dir, name, onPath);
      expect(await resolve(dir, noProc)).toBe(wrapper);
    }
  );
}
