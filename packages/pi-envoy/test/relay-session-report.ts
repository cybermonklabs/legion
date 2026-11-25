// The report one Oh My Pi session whose inbound is the listener's relay socket gives from an OS
// process of its own (`test/relay-session-process.ts`), and the one way to start that process and
// read the report. That script and the rows of `extensions/envoy-relay-socket.test.ts` both read it
// here: the script runs as it loads, so the rows cannot import it (as `test/session-pod-report.ts`
// is to `test/session-pod-process.ts`).
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
 * (`runReportingProcess`). With `waitFor`, the process reports once a notice or a line holds that
 * text, and fails at the `LEGION_PROOF_DEADLINE_MS` its `env` names, the shared wait's deadline
 * outside `bun test`; without, once its start settled. `timeoutMs` kills it.
 */
export async function runRelaySession(options: {
  readonly session: string;
  readonly env: Record<string, string | undefined>;
  readonly cwd: string;
  readonly timeoutMs: number;
  readonly waitFor?: string;
}): Promise<RelaySessionReport> {
  return (await runReportingProcess({
    script: "relay-session-process.ts",
    args: [options.session, ...(options.waitFor === undefined ? [] : [options.waitFor])],
    marker: RELAY_SESSION_REPORT,
    what: `the relay session process for ${options.session}`,
    env: options.env,
    cwd: options.cwd,
    timeoutMs: options.timeoutMs,
  })) as RelaySessionReport;
}
