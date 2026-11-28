// The report one session of a session pod gives from an OS process of its own
// (`test/session-pod-process.ts`), and the one way to start that process and read the report. That
// script, which starts two more of itself, and the row of `extensions/envoy.test.ts` that starts
// it both read it here: the script runs as it loads, so neither can import it.
import { timeLeft } from "@legion/test-fixtures/wait";
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
 * Starts `session-pod-process.ts` for `session` and answers its report, through
 * `runReportingProcess` (`test/report-process.ts`), whose shared wait kills a hung run at the
 * caller's deadline; with `spawn`, the process also starts its two children. With no `env`, the
 * process gets the environment this one started with, as a `Bun.spawn` that names no `env` hands
 * it; with one, that environment, with Bun's runtime transpiler cache off.
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
  const deadline = String(Date.now() + Math.floor(timeLeft() / 2));
  return (await runReportingProcess({
    script: "session-pod-process.ts",
    args: [options.session, ...(options.spawn === true ? ["spawn"] : [])],
    marker: REPORT_MARKER,
    what: `the session pod process for ${options.session}`,
    env:
      options.env === undefined
        ? undefined
        : { ...options.env, LEGION_PROOF_DEADLINE_MS: deadline },
    cwd: options.cwd,
  })) as Report;
}
