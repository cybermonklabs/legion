// The report one session of a session pod gives from an OS process of its own
// (`test/session-pod-process.ts`), and the one way to start that process and read the report. That
// script, which starts two more of itself, and the row of `extensions/envoy.test.ts` that starts
// it both read it here: the script runs as it loads, so neither can import it.
import { runReportingProcess } from "./report-process";

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
 * Starts `session-pod-process.ts` for `session` and answers its report (`runReportingProcess`,
 * which says what `env` and `timeoutMs` do); with `spawn`, the process also starts its two
 * children.
 */
export async function runSessionPod(options: {
  readonly session: string;
  readonly spawn?: boolean;
  readonly env?: Record<string, string | undefined>;
  readonly cwd?: string;
  readonly timeoutMs: number;
}): Promise<Report> {
  return (await runReportingProcess({
    script: "session-pod-process.ts",
    args: [options.session, ...(options.spawn === true ? ["spawn"] : [])],
    marker: REPORT_MARKER,
    what: `the session pod process for ${options.session}`,
    timeoutMs: options.timeoutMs,
    ...(options.cwd === undefined ? {} : { cwd: options.cwd }),
    ...(options.env === undefined ? {} : { env: options.env }),
  })) as Report;
}
