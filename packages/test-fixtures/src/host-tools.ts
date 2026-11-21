/** The path of the program on this host's `PATH`, or null when the host does not have it. */
export function hostTool(name: string): string | null {
  return Bun.which(name);
}

/**
 * Whether this host is missing any of the named programs, for `describe.skipIf(...)` and
 * `test.skipIf(...)`. It prints one warning that names every missing program and the file whose
 * tests will not run, so a reader of the CI output knows what to install. With no names it is
 * false and prints nothing.
 *
 * Call it once, at the test file's module scope, and hand the result to every `skipIf` of the
 * file: a call per test would print the warning per test.
 */
export function skipWithout(...names: string[]): boolean {
  const missing = names.filter((name) => hostTool(name) === null);
  if (missing.length === 0) return false;
  const file = callerFile(new Error().stack);
  const where = file === undefined ? "in this file" : `in ${file}`;
  console.warn(
    `Skipped the tests ${where}. This host does not have: ${missing.join(", ")}. ` +
      "Install the missing programs and run the tests again."
  );
  return true;
}

/**
 * The gate for a test that needs host programs, under the repository's convention for a test a
 * machine cannot run (skipOutsideCI in packages/daemon/internal/launcher/rolenamespace_test.go):
 * outside CI it is `skipWithout`, true with one warning naming every missing program and the
 * file; under GITHUB_ACTIONS a missing program is a broken runner, never a green job with the
 * proof gone from it, so it throws, naming them and the file, and the file fails. With no names,
 * or with every program present, it is false and prints nothing. Call it once, at module scope.
 */
export function skipOutsideCI(...names: string[]): boolean {
  const missing = names.filter((name) => hostTool(name) === null);
  return skipOutsideCIBecause(
    missing.length === 0 ? "" : `This host does not have: ${missing.join(", ")}.`
  );
}

/**
 * `skipOutsideCI` for a reason of the caller's own, a sentence such as "This host has no libpq.":
 * an empty reason is false and prints nothing; any other skips outside CI, warning with the reason
 * and the file, and throws them under GITHUB_ACTIONS.
 */
export function skipOutsideCIBecause(reason: string): boolean {
  if (reason === "") return false;
  const file = callerFile(new Error().stack);
  const where = file === undefined ? "in this file" : `in ${file}`;
  if (process.env.GITHUB_ACTIONS === "true") {
    throw new Error(
      `The tests ${where} cannot run on this CI runner. ${reason} ` +
        "The job must provide it: under GITHUB_ACTIONS a test this machine cannot run fails, never skips."
    );
  }
  console.warn(
    `Skipped the tests ${where}. ${reason} Install what is missing and run the tests again.`
  );
  return true;
}

/**
 * For a test that never skips: throws, naming every program of `names` the host lacks, so the test
 * fails by name rather than at whichever line first runs the program. Call it in the test's body.
 */
export function requireHostTools(...names: string[]): void {
  const missing = names.filter((name) => hostTool(name) === null);
  if (missing.length === 0) return;
  throw new Error(
    `This test needs ${missing.join(", ")} on PATH, which this host does not have; it fails rather than skips.`
  );
}

/**
 * The file that called a gate of this module: the first frame of the stack that is not this
 * module's own.
 * Without it the warning says only which programs are missing, and a reader of a run of hundreds of
 * files cannot tell whose tests did not run.
 *
 * A frame is `at <name> (<path>:<line>:<col>)`, or `at <path>:<line>:<col>` where it has no name.
 * The path is read whole, from the parenthesis or from `at ` up to the last `:<line>:<col>` of the
 * frame, so a checkout under a directory whose name holds a space — `/home/u/my repo/pkg/a.test.ts`
 * — is named as it is rather than cut at the space.
 */
function callerFile(stack: string | undefined): string | undefined {
  if (stack === undefined) return undefined;
  for (const line of stack.split("\n")) {
    const frame = line.trimEnd();
    const file = /\((.+):\d+:\d+\)$/.exec(frame)?.[1] ?? /^\s*at (.+):\d+:\d+$/.exec(frame)?.[1];
    if (file !== undefined && file !== import.meta.path) return file;
  }
  return undefined;
}
