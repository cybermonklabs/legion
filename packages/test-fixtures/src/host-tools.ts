import { isAbsolute } from "node:path";

/** The path of the program on this host's `PATH`, or null when the host does not have it. */
export function hostTool(name: string): string | null {
  return Bun.which(name);
}

/**
 * Whether the calling file's tests skip because this host is missing any of the named programs,
 * for `describe.skipIf(...)` and `test.skipIf(...)`. `job` is the ci.yaml job that runs the file,
 * the one that must put those programs on its runner's `PATH`.
 *
 * Off GitHub Actions a missing program skips the tests: it prints one warning that names every
 * missing program and the file whose tests will not run, so the reader knows what to install, and
 * answers true. Under `GITHUB_ACTIONS=true` it throws instead, naming the programs, the job and the
 * file: a CI job is set up to give each test it runs what that test needs, so a skip there would
 * pass the job with the test's proof gone from it, and nobody reads the skip line. It is the rule
 * `packages/envoy/testprereq` holds for the Go tests. With every program present, or with no names,
 * it is false and prints nothing.
 *
 * Call it once, at the test file's module scope, and hand the result to every `skipIf` of the
 * file: a call per test would print the warning per test. A throw there fails the file's load, so
 * the job fails naming the file and the prerequisite.
 */
export function skipWithout(names: readonly string[], job: string): boolean {
  const missing = names.filter((name) => hostTool(name) === null);
  if (missing.length === 0) return false;
  return missingPrerequisite(
    `${missing.join(", ")} on this host's PATH`,
    job,
    (where) =>
      `Skipped the tests ${where}. This host does not have: ${missing.join(", ")}. ` +
      "Install the missing programs and run the tests again."
  );
}

/**
 * `skipWithout` for a prerequisite that is no program on `PATH`: a variable that names a binary or
 * a server, a database URL, a free port, a capability of the machine. `present` says whether this
 * run has it, and `what` names it as the reader must supply it (`LEGION_TEST_PG_DSN, naming a
 * server this run may create a database on`). It is false when `present` is true, and otherwise
 * skips or throws by `skipWithout`'s rule, with `what` in place of the programs.
 *
 * A file that needs several such things, or a program beside them, builds one list of what it
 * lacks and hands it here joined (`packages/dispatch/e2e/__tests__/psql-session.test.ts`), so a
 * host that lacks them all reads one line naming each rather than one line per gate.
 */
export function requireOrSkip(what: string, present: boolean, job: string): boolean {
  if (present) return false;
  return missingPrerequisite(
    what,
    job,
    (where) =>
      `Skipped the tests ${where}. This run does not have: ${what}. ` +
      "Supply what is missing and run the tests again."
  );
}

/**
 * What both gates do once something is missing: throw under `GITHUB_ACTIONS=true`, in the words
 * `packages/envoy/testprereq` fails a Go test with, or else print `warning` and answer true. The
 * switch is `GITHUB_ACTIONS`, not `CI`, which agent harnesses on the devbox export.
 */
function missingPrerequisite(what: string, job: string, warning: (where: string) => string): true {
  const file = callerFile(new Error().stack);
  const where = file === undefined ? "in this file" : `in ${file}`;
  if (process.env["GITHUB_ACTIONS"] === "true") {
    throw new Error(
      `a prerequisite that ci.yaml's ${job} job must give on GitHub Actions is missing: ${what}, ` +
        `for the tests ${where}`
    );
  }
  console.warn(warning(where));
  return true;
}

/**
 * The file whose tests the gate decides: the outermost frame of the stack whose path is not this
 * module's. Without it the message says only what is missing, and a reader of a run of hundreds of
 * files cannot tell whose tests did not run. The outermost rather than the nearest, so a gate that
 * a shared fixture calls for its caller (`ompNatsGuard` in `packages/pi-shared/test/omp-harness.ts`)
 * names the test file that called the fixture: Bun evaluates a module's body with no frame of its
 * own below it, so at module scope the outermost frame is the file being loaded.
 *
 * A frame is `at <name> (<path>:<line>:<col>)`, or `at <path>:<line>:<col>` where it has no name.
 * The path is read whole, from the parenthesis or from `at ` up to the last `:<line>:<col>` of the
 * frame, so a checkout under a directory whose name holds a space — `/home/u/my repo/pkg/a.test.ts`
 * — is named as it is rather than cut at the space. A frame of the runtime's own (`native`) has no
 * absolute path and names no file.
 */
function callerFile(stack: string | undefined): string | undefined {
  if (stack === undefined) return undefined;
  let file: string | undefined;
  for (const line of stack.split("\n")) {
    const frame = line.trimEnd();
    const path = /\((.+):\d+:\d+\)$/.exec(frame)?.[1] ?? /^\s*at (.+):\d+:\d+$/.exec(frame)?.[1];
    if (path !== undefined && path !== import.meta.path && isAbsolute(path)) file = path;
  }
  return file;
}
