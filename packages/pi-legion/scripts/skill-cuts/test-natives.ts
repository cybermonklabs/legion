/**
 * The pinned Oh My Pi's native addon for this directory's tests whose reads call it: a window that
 * leaves lines of a file out, whose block context opens it (block-context.ts), and a whole read of
 * a code file the pin may summarize (arms.ts's `summaryAnswer`). The addon is the one the runner
 * itself would open (natives.ts's `pinnedAddon`: the first file the pin's loader tries, PI_NATIVES_DIR
 * first, whose SHA-256 is the pin's), linked into a natives directory of the test file's own that
 * PI_NATIVES_DIR names while its rows run, with no WebAssembly grammar installed beside it, so
 * which grammars a read finds does not depend on the host. Oh My Pi at the repository's pin
 * (.omp-pin, LEGION_TEST_OMP) is a later build of 18.18.6 whose addon is another file, so it gives
 * these rows nothing: run them with PI_NATIVES_DIR naming the pin's addon (natives.ts's header
 * says how to get it).
 */
import { afterAll, beforeAll } from "bun:test";
import { mkdirSync, symlinkSync } from "node:fs";
import path from "node:path";
import { captureEnvironment, type EnvironmentRestore } from "@legion/test-fixtures/environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { NATIVES_VERSION, pinnedAddon } from "./natives";

/**
 * Makes the pinned addon there for the rows of the test file `file` that need it, registering its
 * hooks where it is called (the file's module scope, as `temporaryDirectory` is called), and
 * returns whether those rows skip: where the runner finds no pinned addon. A skip prints one line
 * naming the file and why, with the steps that give the addon, as `skipWithout` prints one
 * (docs/testing.md): call it once, at the file's module scope.
 */
export function pinNatives(file: string): boolean {
  const found = pinnedAddon();
  if ("why" in found) {
    console.warn(
      `Skipped the rows of ${file} that need the pinned Oh My Pi's native addon (a read whose block context or summary it gives): ${found.why}`
    );
    return true;
  }
  const natives = temporaryDirectory("skill-cuts-natives");
  let restore: EnvironmentRestore | undefined;
  beforeAll(() => {
    const release = path.join(natives, NATIVES_VERSION);
    mkdirSync(release, { recursive: true });
    symlinkSync(found.file, path.join(release, path.basename(found.file)));
    restore = captureEnvironment(["PI_NATIVES_DIR"]);
    process.env.PI_NATIVES_DIR = natives;
  });
  afterAll(() => restore?.());
  return false;
}
