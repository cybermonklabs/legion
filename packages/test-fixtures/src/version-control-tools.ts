// The jj and git binaries a test runs rather than a wrapper on PATH (`jjBinary`, `gitBinary`), and
// the run of one that names the program when it fails (`runTool`).
import { existsSync } from "node:fs";
import { stat, writeFile } from "node:fs/promises";
import * as path from "node:path";

/** The command that prints the executable of the process it runs under, from the proc root its
 *  environment names (`PROC_ROOT`): the parent of the shell that runs it is that process. */
const PARENT_EXE = 'readlink "$PROC_ROOT/$PPID/exe"';

/**
 * The jj binary that `jj` on this process's PATH runs, for a test to link into a directory of its
 * own put first on the PATH the code under test reads, and to run on its fixtures itself. It is
 * read from the running jj: the executable of the process `jj util exec` starts its command under.
 * On CI it is ~/.local/bin/jj itself (.github/actions/install-jj).
 *
 * The first `jj` on PATH cannot be linked into such a directory as it is, because it can be a
 * wrapper rather than jj, and the code under test can run with a HOME and a PATH of its own (an
 * Oh My Pi pane of `packages/pi-shared/test/omp-harness.ts` runs with the test's home, its `bin`
 * and the system directories). A wrapper finds its jj from the HOME and PATH of whoever runs it,
 * so there it finds none: a mise shim reads its version from the mise configuration under $HOME,
 * and the dotfiles jj shim looks for a jj on PATH past itself and exits 127 (`jj shim: no real jj
 * found on PATH`). A wrapper can also change what jj does: the dotfiles shim drops the commit
 * trailers in a repository under a temporary root, which is where every test repository is. So a
 * test runs the binary the wrapper runs, and its result does not depend on what the machine puts
 * first on PATH.
 *
 * The command runs in `dir`, a directory of the test's that no repository holds, and reads no jj
 * configuration: `JJ_CONFIG` names an empty file written there. The running jj's executable is
 * read from `proc` (Linux's /proc). On a machine with no /proc (macOS) there is nothing to read
 * it from, and the answer is the first `jj` on PATH, the one every caller ran before it ran this.
 * Where the exe link is there and cannot be read, or this jj has no `util exec`, it throws naming
 * the jj it could not resolve (`runningBinary`). `proc` is for this helper's own test, which
 * hands it a proc root that does not exist.
 */
export async function jjBinary(dir: string, proc = "/proc"): Promise<string> {
  const emptyConfig = path.join(dir, "jj-binary-config.toml");
  await writeFile(emptyConfig, "");
  return await runningBinary("jj", ["util", "exec", "--", "sh", "-c", PARENT_EXE], dir, proc, {
    JJ_CONFIG: emptyConfig,
  });
}

/**
 * The git binary that `git` on this process's PATH runs, for the reason `jjBinary` gives: the
 * first `git` on PATH can be a wrapper that changes what git does. The dotfiles git shim gives a
 * commit made in an agent session, in a repository with a remote, an `Omp-Session` trailer and
 * checks every push; a Legion pod's `/opt/legion/bin/git` runs the leak guard before a push. It is
 * read from the running git: the executable of the process a shell alias (`!`) runs under. The
 * command runs in `dir` and reads no git configuration of the machine's (`GIT_CONFIG_GLOBAL` and
 * `GIT_CONFIG_NOSYSTEM`). With no `proc` it is the first `git` on PATH, as for `jjBinary`.
 */
export async function gitBinary(dir: string, proc = "/proc"): Promise<string> {
  return await runningBinary("git", ["-c", `alias.binary=!${PARENT_EXE}`, "binary"], dir, proc, {
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
  });
}

/**
 * Runs `binary`, a test's jj or git (`jjBinary`, `gitBinary`), with `args` in `cwd` and `env` as
 * its whole environment, and answers its stdout, trimmed. With `env` undefined the child gets what
 * `Bun.spawn` gives one with no `env`: the environment this process started with, not
 * `process.env` as a test has since changed it. A non-zero exit throws naming the program, the
 * arguments, the status, the signal when one ended it (a spawn killed under the runner exits
 * non-zero with nothing on stderr, which alone reads as the tool refusing the command: LEGION-243)
 * and its stderr.
 */
export async function runTool(
  binary: string,
  cwd: string,
  env: Record<string, string | undefined> | undefined,
  args: readonly string[]
): Promise<string> {
  const child = Bun.spawn([binary, ...args], {
    cwd,
    ...(env === undefined ? {} : { env }),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) {
    const signal = child.signalCode === null ? "" : `, signal ${child.signalCode}`;
    const said = stderr.trim() === "" ? "no stderr" : stderr.trim();
    throw new Error(`${path.basename(binary)} ${args.join(" ")} exited ${code}${signal}: ${said}`);
  }
  return stdout.trim();
}

/**
 * Runs the first `name` on this process's PATH with `args`, which make it run `PARENT_EXE` as its
 * child, in `dir` with this process's environment, `env` over it and `PROC_ROOT` naming `proc`,
 * and answers the absolute path that prints: the binary the PATH's `name` ran. Where `proc` holds
 * no entry for this process (`<proc>/self`), there is no executable to read, and it answers the
 * PATH's `name` itself. It throws naming the program, the command, its exit status, what it
 * printed and its stderr when no absolute path comes back.
 */
async function runningBinary(
  name: string,
  args: readonly string[],
  dir: string,
  proc: string,
  env: Record<string, string>
): Promise<string> {
  // `Bun.which` with no PATH reads the one the process started with, not `process.env`'s.
  const onPath = Bun.which(name, { PATH: process.env["PATH"] ?? "" });
  if (onPath === null) throw new Error(`${name} is not on PATH`);
  if (!existsSync(path.join(proc, "self"))) return onPath;
  const child = Bun.spawn([onPath, ...args], {
    cwd: dir,
    env: { ...process.env, ...env, PROC_ROOT: proc },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  const binary = stdout.trim();
  if (code !== 0 || !path.isAbsolute(binary)) {
    throw new Error(
      `could not resolve the ${name} binary that ${onPath} runs: \`${[onPath, ...args].join(" ")}\` exited ${code} printing ${JSON.stringify(binary)}; it reads the executable of the process that runs it from ${proc}:\n${stderr}`
    );
  }
  await stat(binary);
  return binary;
}
