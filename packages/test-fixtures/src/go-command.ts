import * as path from "node:path";

/**
 * A go command a test starts: `go <command> <flags...> <packages...> <args...>`, run in `cwd`.
 *
 * It runs with GOPROXY=off, set over the caller's environment, so it builds from the module cache
 * alone. A fetch while a test runs turns a fault of the module proxy into that test's failure, on a
 * change that touched nothing it tests; the job that runs the test fetches what it needs in a step
 * before the tests instead (`.github/scripts/go-fetch.sh deps DIR PACKAGE...`, tried again on
 * failure). A module the cache lacks then fails the command, and the error names that fetch.
 */
export interface GoCommand {
  /** The subcommand: `build`, or `run`, whose one package is the main it runs. */
  readonly command: "build" | "run";
  /** The directory the command runs in: a module's, or one under it. */
  readonly cwd: string;
  /** What comes between the subcommand and the packages: `-o <path>`, `-overlay <file>`. */
  readonly flags?: readonly string[];
  /** The packages it builds: the one main package of a `go run`. */
  readonly packages: readonly string[];
  /** A `go run`'s arguments to the program, after its package. */
  readonly args?: readonly string[];
  /**
   * The packages, in `cwd`, that the fetch command of a refusal names, where `packages` cannot be
   * listed without the command's own flags: the main of a `go run -overlay` exists only in the
   * overlay, so the fetch names packages that hold every module that main imports.
   */
  readonly fetch?: readonly string[];
  /** The command's environment, to which GOPROXY=off is added: this process's when absent. */
  readonly env?: Readonly<Record<string, string | undefined>>;
  /** The command's standard input: Bun's default when absent. */
  readonly stdin?: "inherit" | "ignore" | Uint8Array;
}

/** What a go command left: its exit code, and each stream read whole. */
export interface GoOutput {
  readonly exitCode: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** The repository's root, which the fetch command of a refusal is run from. */
const REPOSITORY_ROOT = path.resolve(import.meta.dir, "../../..");

/**
 * Runs `command` and waits for it, blocking this process: for a caller that cannot await, such as
 * a test file's module scope. It returns what the command left whatever its exit code, and throws
 * only where the command needed a module the module cache lacks ({@link goFetchCommand}).
 */
export function goCommandSync(command: GoCommand): GoOutput {
  const ran = Bun.spawnSync(["go", ...goArguments(command)], {
    cwd: command.cwd,
    env: { ...(command.env ?? process.env), GOPROXY: "off" },
    ...(command.stdin === undefined ? {} : { stdin: command.stdin }),
    stdout: "pipe",
    stderr: "pipe",
  });
  return refusingFetch(command, {
    exitCode: ran.exitCode,
    stdout: ran.stdout.toString(),
    stderr: ran.stderr.toString(),
  });
}

/**
 * Runs `command` and resolves what it left once it exits, whatever its exit code; it rejects only
 * where the command needed a module the module cache lacks, as {@link goCommandSync} throws.
 */
export async function goCommand(command: GoCommand): Promise<GoOutput> {
  const child = Bun.spawn(["go", ...goArguments(command)], {
    cwd: command.cwd,
    env: { ...(command.env ?? process.env), GOPROXY: "off" },
    ...(command.stdin === undefined ? {} : { stdin: command.stdin }),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return refusingFetch(command, { exitCode, stdout, stderr });
}

/**
 * The command that downloads, before the tests, every module a build of `packages` in `cwd` needs:
 * `.github/scripts/go-fetch.sh deps <cwd from the repository root> <packages...>`, run from the
 * repository root. A `cwd` outside the repository is named by its absolute path.
 */
export function goFetchCommand(cwd: string, packages: readonly string[]): string {
  const absolute = path.resolve(cwd);
  const relative = path.relative(REPOSITORY_ROOT, absolute);
  const outside =
    relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative);
  const dir = outside ? absolute : relative === "" ? "." : relative;
  return [".github/scripts/go-fetch.sh", "deps", dir, ...packages].join(" ");
}

function goArguments(command: GoCommand): string[] {
  return [command.command, ...(command.flags ?? []), ...command.packages, ...(command.args ?? [])];
}

/**
 * `output`, unless the command failed on a module the cache lacks: the go command says so naming
 * GOPROXY=off ("module lookup disabled by GOPROXY=off"), and the failure is then thrown with the
 * fetch that prevents it, in the words packages/envoy/internal/cmdtest's fetchFirst uses for the
 * Go tests' builds. Any other failure is the caller's to report, as it reported one before.
 */
function refusingFetch(command: GoCommand, output: GoOutput): GoOutput {
  if (output.exitCode === 0 || !`${output.stderr}${output.stdout}`.includes("GOPROXY=off")) {
    return output;
  }
  throw new Error(
    `go ${goArguments(command).join(" ")} in ${command.cwd} exited ${output.exitCode}:\n` +
      `${output.stderr}${output.stdout}\n` +
      "The module cache lacks a module this build needs, and a test fetches no module while it " +
      "runs. Fetch them before the tests, from the repository root:\n  " +
      goFetchCommand(command.cwd, command.fetch ?? command.packages)
  );
}
