// The report one Oh My Pi session whose inbound is the listener's relay socket gives from an OS
// process of its own (`test/relay-session-process.ts`), and the one way to start that process and
// read the report. That script and the rows of `extensions/envoy-relay-socket.test.ts` both read it
// here: the script runs as it loads, so the rows cannot import it (as `test/session-pod-report.ts`
// is to `test/session-pod-process.ts`).
import type { EnvoyTaskSignal } from "@legion/pi-shared/interface";
import { runReportingProcess } from "./report-process";

/** What the process prints before its report, which is one JSON line. */
export const RELAY_SESSION_REPORT = "relay-session-report ";

/** The notices the session was shown, the `envoy:` lines it logged, and the task signals the
 *  extension handed the Legion entry's taker, in order. */
export interface RelaySessionReport {
  readonly notices: readonly string[];
  readonly lines: readonly string[];
  readonly signals: readonly EnvoyTaskSignal[];
}

/**
 * Starts `relay-session-process.ts` for `session` in `env`, and answers its report
 * (`runReportingProcess`). With `waitFor`, the process reports once a notice or a line holds that
 * text, and with `taskSignals`, once it has taken `count` task signals, each appended to `file` as
 * it comes; with both, once both hold; either way 20 s after its start settled at the latest, and
 * with neither, once its start settled.
 */
export async function runRelaySession(options: {
  readonly session: string;
  readonly env: Record<string, string | undefined>;
  readonly cwd: string;
  readonly timeoutMs: number;
  readonly waitFor?: string;
  readonly taskSignals?: { readonly file: string; readonly count: number };
}): Promise<RelaySessionReport> {
  return (await runReportingProcess({
    script: "relay-session-process.ts",
    args: [
      options.session,
      options.waitFor ?? "",
      options.taskSignals?.file ?? "",
      String(options.taskSignals?.count ?? 0),
    ],
    marker: RELAY_SESSION_REPORT,
    what: `the relay session process for ${options.session}`,
    env: options.env,
    cwd: options.cwd,
    timeoutMs: options.timeoutMs,
  })) as RelaySessionReport;
}
