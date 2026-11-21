// The report one session of a session pod gives from an OS process of its own
// (`test/session-pod-process.ts`), and the one way to start that process and read the report. That
// script, which starts two more of itself, and the row of `extensions/envoy.test.ts` that starts
// it both read it here: the script runs as it loads, so neither can import it.
import { join } from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { settled, timeLeft } from "@legion/test-fixtures/wait";

/** What the process prints before its report, which is one JSON line. */
export const REPORT_MARKER = "session-pod-report ";

/** One session's role claims and `envoy: ENVOY_SESSION_ROLE …` log lines, and its children's. */
export interface Report {
  readonly session: string;
  readonly claims: readonly { readonly role: string; readonly session_id: string }[];
  readonly lines: readonly string[];
  readonly children?: readonly (Report & { readonly spawn: string })[];
}

/**
 * Starts `session-pod-process.ts` for `session` and answers its report; with `spawn`, the process
 * also starts its two children. With no `env`, the process gets the environment this one started
 * with, as a `Bun.spawn` that names no `env` hands it; with one, that environment, with Bun's
 * runtime transpiler cache off (`bunCacheOff`) as `runReportingProcess` (`test/report-process.ts`)
 * hands it, for its reason: a row's `env` names a HOME the row's temporary-directory hook removes.
 * It does not start the process through `runReportingProcess`, whose `timeoutMs` is a bound of its
 * own: the wait for it is the shared one (`settled`), which ends at the caller's deadline and then
 * kills the process, so a hung run leaves no process behind; a run that reports nothing throws
 * with its stderr.
 *
 * A process given an `env` is handed a deadline of its own too (`LEGION_PROOF_DEADLINE_MS`, the
 * shared wait's deadline outside `bun test`), half of what is left of the caller's: its own waits
 * on its children end there, kill them and report, before the caller's wait ends. A process
 * started with no `env` inherits the deadline its parent started with, which is the case that
 * spawn tests.
 */
export async function runSessionPod(options: {
  readonly session: string;
  readonly spawn?: boolean;
  readonly env?: Record<string, string | undefined>;
  readonly cwd?: string;
}): Promise<Report> {
  const argv = [
    process.execPath,
    "--preload",
    join(import.meta.dir, "..", "scripts", "role-takeover", "host-stub.ts"),
    join(import.meta.dir, "session-pod-process.ts"),
    options.session,
    ...(options.spawn === true ? ["spawn"] : []),
  ];
  const deadline = String(Date.now() + Math.floor(timeLeft() / 2));
  const started = Bun.spawn(argv, {
    stdout: "pipe",
    stderr: "pipe",
    ...(options.cwd === undefined ? {} : { cwd: options.cwd }),
    // Left out altogether rather than set to the live environment: the child then starts from the
    // environment this process started with, which is the case a row asks about.
    ...(options.env === undefined
      ? {}
      : { env: { ...options.env, ...bunCacheOff, LEGION_PROOF_DEADLINE_MS: deadline } }),
  });
  let read: [string, string, number];
  try {
    read = await settled(
      `the session pod process for ${options.session} to report and exit`,
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
  const line = stdout.split("\n").find((entry) => entry.startsWith(REPORT_MARKER));
  if (exitCode !== 0 || line === undefined) {
    throw new Error(
      `the session pod process for ${options.session} reported nothing (exit ${exitCode}): ${stderr}`
    );
  }
  return JSON.parse(line.slice(REPORT_MARKER.length)) as Report;
}
