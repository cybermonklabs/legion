// The one way the suite starts a script that runs one Oh My Pi session in an OS process of its own
// and prints one JSON report, and reads that report: `test/session-pod-report.ts` and
// `test/relay-session-report.ts` start theirs through it. The script runs under the preload that
// supplies the host package the extension imports (`scripts/role-takeover/host-stub.ts`).
import { join } from "node:path";

/**
 * Starts `script` (a file of this directory) with `args`, and answers the JSON its stdout prints
 * after `marker`. With no `env`, the process gets the environment this one started with, as a
 * `Bun.spawn` that names no `env` hands it; with one, that environment, with Bun's runtime
 * transpiler cache off: a row's `env` names a HOME the row's temporary-directory hook removes, and
 * the cache would put about 360 files, 6 MB, under it, all of which that hook would remove too
 * (measured: a row's HOME held 359 files with the cache, the cache's `@t@` directory all but 3 of
 * them). That hook is the `afterEach` of `temporaryDirectories` (test-fixtures'
 * temporary-directories.ts), which names no limit of its own, so the 250 s `bun run test` gives
 * it (package.json) bounds it, and bun's 5 s default does only in a run with no `--timeout`. It is
 * killed at `timeoutMs`, and its stdin is held open, written nothing, for as long as this
 * process runs, so a script that exits at that pipe's end (`session-pod-process.ts`) leaves no
 * process behind when a test runner abandons the row that started it. A run that reports nothing
 * throws, naming `what` and giving its stderr.
 */
export async function runReportingProcess(options: {
  readonly script: string;
  readonly args: readonly string[];
  readonly marker: string;
  readonly what: string;
  readonly env?: Record<string, string | undefined>;
  readonly cwd?: string;
  readonly timeoutMs: number;
}): Promise<unknown> {
  const started = Bun.spawn(
    [
      process.execPath,
      "--preload",
      join(import.meta.dir, "..", "scripts", "role-takeover", "host-stub.ts"),
      join(import.meta.dir, options.script),
      ...options.args,
    ],
    {
      // Held open, and written nothing, for as long as this process runs.
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
      timeout: options.timeoutMs,
      ...(options.cwd === undefined ? {} : { cwd: options.cwd }),
      // Left out altogether rather than set to the live environment: the child then starts from
      // the environment this process started with, which is the case a row may ask about.
      ...(options.env === undefined
        ? {}
        : { env: { ...options.env, BUN_RUNTIME_TRANSPILER_CACHE_PATH: "0" } }),
    }
  );
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(started.stdout).text(),
    new Response(started.stderr).text(),
    started.exited,
  ]);
  const line = stdout.split("\n").find((entry) => entry.startsWith(options.marker));
  if (exitCode !== 0 || line === undefined) {
    throw new Error(`${options.what} reported nothing (exit ${exitCode}): ${stderr}`);
  }
  return JSON.parse(line.slice(options.marker.length));
}
