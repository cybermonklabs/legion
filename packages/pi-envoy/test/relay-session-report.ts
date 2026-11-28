// The report one Oh My Pi session whose inbound is the listener's relay socket gives from an OS
// process of its own (`test/relay-session-process.ts`), and the one way to start that process and
// read the report. That script and the rows of `extensions/envoy-relay-socket.test.ts` both read it
// here: the script runs as it loads, so the rows cannot import it (as `test/session-pod-report.ts`
// is to `test/session-pod-process.ts`).
import { timeLeft } from "@legion/test-fixtures/wait";
import { runReportingProcess } from "./report-process";

/** What the process prints before its report, which is one JSON line. */
export const RELAY_SESSION_REPORT = "relay-session-report ";

/** The notices the session was shown and the `envoy:` lines it logged. */
export interface RelaySessionReport {
  readonly notices: readonly string[];
  readonly lines: readonly string[];
}

/**
 * Starts `relay-session-process.ts` for `session` in `env`, and answers its report
 * (`runReportingProcess`, whose shared wait kills a hung run at the caller's deadline). With
 * `waitFor`, the process reports once a notice or a line holds that text; without, once its start
 * settled. The process is handed half of what is left of the caller's deadline
 * (`LEGION_PROOF_DEADLINE_MS`), so a text that never comes ends its own wait, which names it, before
 * the caller's wait for the process gives up.
 */
export async function runRelaySession(options: {
  readonly session: string;
  readonly env: Record<string, string | undefined>;
  readonly cwd: string;
  readonly waitFor?: string;
}): Promise<RelaySessionReport> {
  const deadline = String(Date.now() + Math.floor(timeLeft() / 2));
  return (await runReportingProcess({
    script: "relay-session-process.ts",
    args: [options.session, ...(options.waitFor === undefined ? [] : [options.waitFor])],
    marker: RELAY_SESSION_REPORT,
    what: `the relay session process for ${options.session}`,
    env: { ...options.env, LEGION_PROOF_DEADLINE_MS: deadline },
    cwd: options.cwd,
  })) as RelaySessionReport;
}
