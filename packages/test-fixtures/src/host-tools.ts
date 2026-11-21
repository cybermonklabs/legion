import { isAbsolute } from "node:path";

/** The path of the program on this host's `PATH`, or null when the host does not have it. */
export function hostTool(name: string): string | null {
  return Bun.which(name);
}

/**
 * The interpreter `python3` names in this process's own environment (`sys.executable`), by its
 * absolute path, for a test that starts Python under an environment of its own.
 *
 * `python3` looked up on a child's PATH can be a version manager's shim, and a shim finds its tool
 * through the caller's HOME. On a devbox it is mise's: under a test's own HOME it has no version
 * for python3, and where no other python3 is on the PATH behind it, it fails ("python3 is not a
 * valid shim"); where one is, it runs that one, with every mise install directory taken off the
 * PATH of what it runs. Run by its own path, the interpreter is the one the caller runs, and the
 * child gets exactly the environment the test gives it. Call it once, where the test starts:
 * it runs python3, and it throws, naming what python3 answered, when that is no absolute path.
 */
export function pythonInterpreter(): string {
  return ownExecutable("python3", ["-I", "-c", "import sys; print(sys.executable)"]);
}

/** The interpreter `node` names in this process's own environment (`process.execPath`), by its
 *  absolute path, for the same reason as {@link pythonInterpreter}: node is a mise shim on a
 *  devbox too. Bun's own is `process.execPath` in the test itself. */
export function nodeInterpreter(): string {
  return ownExecutable("node", ["-p", "process.execPath"]);
}

/** What `name`, run with `args` in this process's environment, prints as its own executable,
 *  which must be an absolute path. */
function ownExecutable(name: string, args: readonly string[]): string {
  const answer = Bun.spawnSync([name, ...args], { stdout: "pipe", stderr: "pipe" });
  const executable = answer.stdout.toString().trim();
  if (answer.exitCode !== 0 || !isAbsolute(executable))
    throw new Error(
      `${name} named no interpreter of its own (status ${answer.exitCode}, ${JSON.stringify(executable)}): ${answer.stderr.toString()}`
    );
  return executable;
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
 * The file that called `skipWithout`: the first frame of the stack that is not this module's own.
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
