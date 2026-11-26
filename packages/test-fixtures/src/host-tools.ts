import { beforeEach, describe as bunDescribe, test as bunTest } from "bun:test";
import { isAbsolute } from "node:path";

/** The path of the program on this host's `PATH`, or null when the host does not have it. */
export function hostTool(name: string): string | null {
  return Bun.which(name);
}

/**
 * Whether the calling file's tests skip because this host is missing any of the named programs,
 * for `describe.skipIf(...)` and `test.skipIf(...)`.
 *
 * Off GitHub Actions a missing program skips the tests: it prints one warning that names every
 * missing program and the file whose tests will not run, so the reader knows what to install, and
 * answers true. Under `GITHUB_ACTIONS=true` it throws instead, naming the programs and the file: a
 * CI job is set up to give each test it runs what that test needs, so a skip there would pass the
 * job with the test's proof gone from it, and nobody reads the skip line. It is the rule
 * `packages/envoy/testprereq` holds for the Go tests. With every program present, or with no names,
 * it is false and prints nothing.
 *
 * Call it once, at the test file's module scope, and hand the result to every `skipIf` of the
 * file: a call per test would print the warning per test. A throw there fails the file's load, so
 * the job fails naming the file and the prerequisite.
 */
export function skipWithout(...names: string[]): boolean {
  const missing = names.filter((name) => hostTool(name) === null);
  if (missing.length === 0) return false;
  return missingPrerequisite(
    `${missing.join(", ")} on this host's PATH`,
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
 * lacks and hands it here joined, so a host that lacks them all reads one line naming each rather
 * than one line per gate.
 */
export function requireOrSkip(what: string, present: boolean): boolean {
  if (present) return false;
  return missingPrerequisite(
    what,
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
function missingPrerequisite(what: string, warning: (where: string) => string): true {
  const file = callerFile(new Error().stack);
  const where = file === undefined ? "in this file" : `in ${file}`;
  if (process.env["GITHUB_ACTIONS"] === "true") {
    throw new Error(
      `a prerequisite that the ci.yaml job running the tests ${where} must give on GitHub Actions ` +
        `is missing: ${what}`
    );
  }
  console.warn(warning(where));
  return true;
}

/** What a host gate needs: programs and a reason under the CI rule, and programs never skipped for. */
export interface HostGateNeeds {
  /** Programs a missing one of which skips the gated tests outside CI and fails them in CI. */
  readonly programs?: readonly string[];
  /** A sentence of the caller's own that holds the same way, such as "This host has no libpq."; empty for none. */
  readonly reason?: string;
  /** Programs a missing one of which fails the gated tests on any host, never skipping them. */
  readonly never?: readonly string[];
}

/** A gate's own `test` and `describe`, and whether anything it needs is missing. */
export interface HostGate {
  /** True when anything the gate needs is missing: a setup hook of the gated tests skips its work. */
  readonly missing: boolean;
  /** `test` under the gate. */
  readonly test: (
    name: string,
    fn: Parameters<typeof bunTest>[1],
    options?: Parameters<typeof bunTest>[2]
  ) => void;
  /** `describe` under the gate. */
  readonly describe: (name: string, fn: () => void) => void;
}

/**
 * The gate for tests that need host programs, under the repository's rule for a test a machine
 * cannot run (skipOutsideCI in packages/daemon/internal/launcher/rolenamespace_test.go). Where
 * nothing it needs is missing, its `test` and `describe` are bun's. Where something is missing,
 * outside CI each gated test is skipped, with one warning naming what is missing and the file, as
 * `skipWithout` does; under GITHUB_ACTIONS each gated test fails, by its own name, with an error
 * naming what is missing, since the job that runs it is set up to run it and a skip there would
 * be a green job with the proof gone. A run is CI only where GITHUB_ACTIONS is exactly `true`, as
 * GitHub's runner sets it; any other value, or none, is a run outside CI. A missing program of
 * `never` fails each gated test on any host. `reason` takes a host condition of the caller's own
 * that no program names (a sudo that asks for a password, a kernel without the feature, another
 * platform) under the same rule. Call it once, at module scope, and declare the gated tests with
 * its `test` and `describe`.
 */
export function hostGate(needs: HostGateNeeds): HostGate {
  const never = (needs.never ?? []).filter((name) => hostTool(name) === null);
  const programs = (needs.programs ?? []).filter((name) => hostTool(name) === null);
  const reasons = [
    never.length === 0
      ? ""
      : `This host does not have: ${never.join(", ")}, which these tests never skip for.`,
    programs.length === 0 ? "" : `This host does not have: ${programs.join(", ")}.`,
    needs.reason ?? "",
  ].filter((reason) => reason !== "");
  const file = callerFile(new Error().stack);
  const where = file === undefined ? "in this file" : `in ${file}`;
  const fails =
    never.length > 0 || (reasons.length > 0 && process.env["GITHUB_ACTIONS"] === "true");
  const skips = reasons.length > 0 && !fails;
  const failure =
    `This test cannot run on this host. ${reasons.join(" ")} ` +
    (never.length > 0
      ? "It fails rather than skips."
      : "Under GITHUB_ACTIONS a test this machine cannot run fails, never skips.");
  if (skips) {
    console.warn(
      `Skipped the tests ${where}. ${reasons.join(" ")} Install what is missing and run the tests again.`
    );
  }
  return {
    missing: reasons.length > 0,
    test: (name, fn, options) => {
      if (skips) bunTest.skip(name, fn, options);
      else if (fails)
        bunTest(
          name,
          () => {
            throw new Error(failure);
          },
          options
        );
      else bunTest(name, fn, options);
    },
    describe: (name, fn) => {
      if (skips) bunDescribe.skip(name, fn);
      else if (fails)
        bunDescribe(name, () => {
          beforeEach(() => {
            throw new Error(failure);
          });
          fn();
        });
      else bunDescribe(name, fn);
    },
  };
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
 * The file whose tests a gate of this module decides: the outermost frame of the stack whose path
 * is not this module's. Without it the message says only what is missing, and a reader of a run of
 * hundreds of files cannot tell whose tests did not run. The outermost rather than the nearest, so
 * a gate that a shared fixture calls for its caller names the test file that called the fixture:
 * Bun evaluates a module's body with no frame of its own below it, so at module scope the
 * outermost frame is the file being loaded.
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
