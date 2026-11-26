import { beforeEach, describe as bunDescribe, test as bunTest } from "bun:test";
import { accessSync, constants, existsSync, realpathSync, statSync } from "node:fs";
import { basename, delimiter, isAbsolute, join, relative, sep } from "node:path";

/** The path of the program on this host's `PATH`, or null when the host does not have it. */
export function hostTool(name: string): string | null {
  return Bun.which(name);
}

/**
 * The program `name` itself, never a mise shim of it, or null when this host has neither: the
 * path a test hands a child whose `HOME` it replaces. A mise shim reads its configuration from
 * the caller's home and from the directories above the child's working directory, and under a
 * `HOME` of the test's own it refuses the real home's config there as untrusted ("Config files in
 * <home>/.config/mise/config.toml are not trusted") or finds no version ("No version is set for
 * shim"), and exits 1 before the program runs. Read it once, at a test file's module scope, before
 * any directory of the test's own goes first on PATH.
 *
 * It walks `PATH` in order and takes the first executable `name`. A link to the `mise` binary is a
 * shim: it is replaced by what `mise which <name>` names, or, where that names nothing, by the
 * next `name` on PATH, the one the shim would fall back to. On a host without mise this is the
 * first `name` on PATH, as `Bun.which` gives. A directory at or below `outside`, a test's own
 * stand-in directory, is skipped, so a stand-in never resolves to itself.
 */
export function realHostTool(name: string, outside?: string): string | null {
  for (const directory of (process.env["PATH"] ?? "").split(delimiter)) {
    if (directory === "") continue;
    if (outside !== undefined) {
      const fromOutside = relative(outside, directory);
      const beyond =
        fromOutside === ".." || fromOutside.startsWith(`..${sep}`) || isAbsolute(fromOutside);
      if (!beyond) continue;
    }
    const candidate = join(directory, name);
    try {
      accessSync(candidate, constants.X_OK);
      if (!statSync(candidate).isFile()) continue;
    } catch {
      continue;
    }
    const target = realpathSync(candidate);
    if (basename(target) !== "mise") return candidate;
    const named = Bun.spawnSync([target, "which", name]);
    const path = named.stdout.toString().trim();
    if (named.exitCode === 0 && path !== "" && existsSync(path)) return path;
  }
  return null;
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
