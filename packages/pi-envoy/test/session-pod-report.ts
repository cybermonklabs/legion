// The report one session of a session pod gives from an OS process of its own
// (`test/session-pod-process.ts`), and the one way to start that process and read the report. That
// script, which starts two more of itself, and the row of `extensions/envoy.test.ts` that starts
// it both read it here: the script runs as it loads, so neither can import it.
import { join } from "node:path";

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
 * with, as a `Bun.spawn` that names no `env` hands it; with one, that environment. It is killed at
 * `timeoutMs`, so a hung run leaves no process behind, and a run that reports nothing throws with
 * its stderr.
 */
export async function runSessionPod(options: {
  readonly session: string;
  readonly spawn?: boolean;
  readonly env?: Record<string, string | undefined>;
  readonly cwd?: string;
  readonly timeoutMs: number;
}): Promise<Report> {
  const argv = [
    process.execPath,
    "--preload",
    join(import.meta.dir, "..", "scripts", "role-takeover", "host-stub.ts"),
    join(import.meta.dir, "session-pod-process.ts"),
    options.session,
    ...(options.spawn === true ? ["spawn"] : []),
  ];
  const started = Bun.spawn(argv, {
    stdout: "pipe",
    stderr: "pipe",
    timeout: options.timeoutMs,
    ...(options.cwd === undefined ? {} : { cwd: options.cwd }),
    // Left out altogether rather than set to the live environment: the child then starts from the
    // environment this process started with, which is the case a row asks about.
    ...(options.env === undefined ? {} : { env: options.env }),
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(started.stdout).text(),
    new Response(started.stderr).text(),
    started.exited,
  ]);
  const line = stdout.split("\n").find((entry) => entry.startsWith(REPORT_MARKER));
  if (exitCode !== 0 || line === undefined) {
    throw new Error(
      `the session pod process for ${options.session} reported nothing (exit ${exitCode}): ${stderr}`
    );
  }
  return JSON.parse(line.slice(REPORT_MARKER.length)) as Report;
}
