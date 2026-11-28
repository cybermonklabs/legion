// The one way the suite starts a script that runs one Oh My Pi session in an OS process of its own
// and prints one JSON report, and reads that report: `test/session-pod-report.ts` and
// `test/relay-session-report.ts` start theirs through it. The script runs under the preload that
// supplies the host package the extension imports (`scripts/role-takeover/host-stub.ts`).
import { join } from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { settled } from "@legion/test-fixtures/wait";

/**
 * Starts `script` (a file of this directory) with `args`, and answers the JSON its stdout prints
 * after `marker`. With no `env`, the process gets the environment this one started with, as a
 * `Bun.spawn` that names no `env` hands it; with one, that environment, with Bun's runtime
 * transpiler cache off (`bunCacheOff`): a row's `env` names a HOME the row's temporary-directory
 * hook removes, and the cache would put about 360 files, 6 MB, under it, whose removal on a loaded
 * box can outlast Bun's 5 s bound on a hook (measured: a row's HOME held 359 files with the cache,
 * the cache's `@t@` directory all but 3 of them). The wait for the process to report and exit is
 * the shared one (`settled`), with no bound of its own: it ends at the caller's deadline and then
 * kills the process, so a hung run leaves no process behind and names `what`. A run that reports
 * nothing throws, naming `what` and giving its stderr.
 */
export async function runReportingProcess(options: {
  readonly script: string;
  readonly args: readonly string[];
  readonly marker: string;
  readonly what: string;
  readonly env?: Record<string, string | undefined>;
  readonly cwd?: string;
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
      stdout: "pipe",
      stderr: "pipe",
      ...(options.cwd === undefined ? {} : { cwd: options.cwd }),
      // Left out altogether rather than set to the live environment: the child then starts from
      // the environment this process started with, which is the case a row may ask about.
      ...(options.env === undefined ? {} : { env: { ...options.env, ...bunCacheOff } }),
    }
  );
  let read: [string, string, number];
  try {
    read = await settled(
      `${options.what} to report and exit`,
      Promise.all([
        new Response(started.stdout).text(),
        new Response(started.stderr).text(),
        started.exited,
      ])
    );
  } catch (error) {
    started.kill("SIGKILL");
    throw error;
  }
  const [stdout, stderr, exitCode] = read;
  const line = stdout.split("\n").find((entry) => entry.startsWith(options.marker));
  if (exitCode !== 0 || line === undefined) {
    throw new Error(`${options.what} reported nothing (exit ${exitCode}): ${stderr}`);
  }
  return JSON.parse(line.slice(options.marker.length));
}
