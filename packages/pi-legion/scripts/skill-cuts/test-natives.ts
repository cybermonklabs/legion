/**
 * The pinned Oh My Pi's native addon for this directory's tests whose reads call it: a window that
 * leaves lines of a file out, whose block context opens it (block-context.ts), and a whole read of
 * a code file the pin may summarize (arms.ts's `summaryAnswer`). The addon is the one the runner
 * itself would open (natives.ts's `pinnedAddon`: the first file the pin's loader tries,
 * SKILL_CUTS_NATIVES_DIR and then PI_NATIVES_DIR first, whose SHA-256 is the pin's), linked into a
 * natives directory of the test file's own that PI_NATIVES_DIR names while its rows run, with
 * SKILL_CUTS_NATIVES_DIR unset and no WebAssembly grammar installed beside it, so which grammars a
 * read finds does not depend on the host. Oh My Pi at the repository's pin (.omp-pin,
 * LEGION_TEST_OMP) is a later build of 18.18.6 whose addon is another file, so it gives these rows
 * nothing: run them with PI_NATIVES_DIR naming the pin's addon (natives.ts's header says how to
 * get it). CI's pi-legion job installs it (.github/actions/install-skill-cuts-omp) and names it in
 * SKILL_CUTS_NATIVES_DIR. The rows that run the pin's own binary (checkout.ts's `pinAnswer`) are
 * gated the same way on it too (`pinBinary`): SKILL_CUTS_OMP, which the same action sets.
 */
import { afterAll, beforeAll, test } from "bun:test";
import { mkdirSync, symlinkSync } from "node:fs";
import path from "node:path";
import { captureEnvironment, type EnvironmentRestore } from "@legion/test-fixtures/environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { NATIVES_VERSION, pinnedAddon, pinnedOmp } from "./natives";

/** A row of a test file: `test`'s own arguments. */
type Row = (
  name: string,
  fn: Parameters<typeof test>[1],
  options?: Parameters<typeof test>[2]
) => void;

/**
 * Makes the pinned addon there for the rows of the test file `file` that need it, registering its
 * hooks where it is called (the file's module scope, as `temporaryDirectory` is called), and
 * returns the `test` those rows are declared with. Where the runner finds no pinned addon, outside
 * CI each such row skips, with one line printed here naming the file and why, with the steps that
 * give the addon, as `skipWithout` prints one; under GITHUB_ACTIONS (exactly `true`) each fails by
 * its own name with that reason, since the job that runs them installs the addon, and a skip there
 * would be a green job with the proof gone (docs/testing.md, `hostGate`). Call it once, at the
 * file's module scope.
 */
export function pinNatives(file: string): { readonly test: Row } {
  const found = pinnedAddon();
  if ("why" in found) {
    return gatedRows(
      file,
      "the pinned Oh My Pi's native addon (a read whose block context or summary it gives)",
      found.why
    );
  }
  const natives = temporaryDirectory("skill-cuts-natives");
  let restore: EnvironmentRestore | undefined;
  beforeAll(() => {
    const release = path.join(natives, NATIVES_VERSION);
    mkdirSync(release, { recursive: true });
    symlinkSync(found.file, path.join(release, path.basename(found.file)));
    restore = captureEnvironment(["PI_NATIVES_DIR", "SKILL_CUTS_NATIVES_DIR"]);
    process.env.PI_NATIVES_DIR = natives;
    delete process.env.SKILL_CUTS_NATIVES_DIR;
  });
  afterAll(() => restore?.());
  return { test };
}

/**
 * The `test` of the rows of the test file `file` that run the pinned Oh My Pi's own binary
 * (checkout.ts's `pinAnswer`): it needs the binary SKILL_CUTS_OMP names (natives.ts's
 * `pinnedOmp`) and the pin's addon (`pinnedAddon`), which CI's pi-legion job installs both of
 * (.github/actions/install-skill-cuts-omp). Where the runner finds either not, each such row
 * skips outside CI, with one line printed naming the file and why, and fails by its own name
 * under GITHUB_ACTIONS, as `pinNatives`' rows do. Call it once, at the file's module scope, after
 * `pinNatives`, whose hooks name the addon.
 */
export function pinBinary(file: string): { readonly test: Row } {
  const omp = pinnedOmp();
  const addon = pinnedAddon();
  const why = "why" in omp ? omp.why : "why" in addon ? addon.why : undefined;
  if (why === undefined) return { test };
  return gatedRows(file, "the pinned Oh My Pi's own binary and its native addon", why);
}

/** The rows of `file` that need `needs`, which the runner lacks for `why`: under GITHUB_ACTIONS
 *  (exactly `true`) each fails by its own name, saying so; elsewhere each skips, one line printed
 *  naming the file, what it needs and why. */
function gatedRows(file: string, needs: string, why: string): { readonly test: Row } {
  if (process.env.GITHUB_ACTIONS === "true") {
    const failure = `This row of ${file} needs ${needs}, and the runner finds none: ${why}. Under GITHUB_ACTIONS such a row fails, never skips.`;
    return {
      test: (name, _fn, options) =>
        test(
          name,
          () => {
            throw new Error(failure);
          },
          options
        ),
    };
  }
  console.warn(`Skipped the rows of ${file} that need ${needs}: ${why}`);
  return { test: (name, fn, options) => test.skip(name, fn, options) };
}
