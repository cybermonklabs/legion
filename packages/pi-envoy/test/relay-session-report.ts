// The report one Oh My Pi session whose inbound is the listener's relay socket gives from an OS
// process of its own (`test/relay-session-process.ts`), and the one way to start that process and
// read the report. That script and the rows of `extensions/envoy-relay-socket.test.ts` both read it
// here: the script runs as it loads, so the rows cannot import it (as `test/session-pod-report.ts`
// is to `test/session-pod-process.ts`).
import { join } from "node:path";

/** What the process prints before its report, which is one JSON line. */
export const RELAY_SESSION_REPORT = "relay-session-report ";

/** The notices the session was shown and the `envoy:` lines it logged. */
export interface RelaySessionReport {
  readonly notices: readonly string[];
  readonly lines: readonly string[];
}

/**
 * Starts `relay-session-process.ts` for `session` in `env`, and answers its report. It is killed at
 * `timeoutMs`, so a hung run leaves no process behind, and a run that reports nothing throws with
 * its stderr.
 */
export async function runRelaySession(options: {
  readonly session: string;
  readonly env: Record<string, string | undefined>;
  readonly cwd: string;
  readonly timeoutMs: number;
}): Promise<RelaySessionReport> {
  const started = Bun.spawn(
    [
      process.execPath,
      "--preload",
      join(import.meta.dir, "..", "scripts", "role-takeover", "host-stub.ts"),
      join(import.meta.dir, "relay-session-process.ts"),
      options.session,
    ],
    {
      cwd: options.cwd,
      env: options.env,
      stdout: "pipe",
      stderr: "pipe",
      timeout: options.timeoutMs,
    }
  );
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(started.stdout).text(),
    new Response(started.stderr).text(),
    started.exited,
  ]);
  const line = stdout.split("\n").find((entry) => entry.startsWith(RELAY_SESSION_REPORT));
  if (exitCode !== 0 || line === undefined) {
    throw new Error(
      `the relay session process for ${options.session} reported nothing (exit ${exitCode}): ${stderr}`
    );
  }
  return JSON.parse(line.slice(RELAY_SESSION_REPORT.length)) as RelaySessionReport;
}
