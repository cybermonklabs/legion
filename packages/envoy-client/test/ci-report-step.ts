import { cpSync } from "node:fs";
import { join } from "node:path";
import type { CIResultsStack, JobRun } from "./ci-results-stack";

// The last step of a CI job as the live tests of packages/envoy's ci-report run it: a checkout of
// a commit that holds spec files, and ci-report run in it as the action
// .github/actions/dispatch-results runs it (C3), under the stack's real `agent-secrets
// result-grant`. ci-report.live.test.ts and ci-report-long-output.live.test.ts share it.
// Test-only: nothing outside this package's tests imports it.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH = "" } = process.env;

/** The CLI entry ci-report runs, as a CI job's shell reaches it: this checkout's own `dispatch`. */
export const DISPATCH_ENTRY = join(import.meta.dir, "..", "bin", "dispatch.ts");
/** The step's `--dispatch-cli`: this checkout's CLI, run by this Bun. */
export const DISPATCH_CLI = `${process.execPath} ${DISPATCH_ENTRY}`;

/** One JSON line that ci-report prints. */
export interface ReportLine {
  readonly check: string;
  readonly test: string;
  readonly outcome: string;
  readonly exit: number | null;
  readonly result_id: string;
  readonly why: string;
}

/** What one run of ci-report did. */
export interface Reported {
  readonly code: number;
  readonly lines: ReportLine[];
  readonly stderr: string;
}

/** Runs git with `args` in `cwd`, with no configuration of this machine's and `home` as its HOME,
 *  and answers what it printed on standard output; a git that fails throws its error. */
export function git(home: string, cwd: string, ...args: string[]): string {
  const ran = Bun.spawnSync(
    [
      "git",
      "-c",
      "user.name=ACME CI",
      "-c",
      "user.email=ci@example.com",
      "-c",
      "commit.gpgsign=false",
      ...args,
    ],
    {
      cwd,
      env: {
        PATH: SHELL_PATH,
        HOME: home,
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
      },
    }
  );
  if (ran.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${ran.stderr.toString()}`);
  return ran.stdout.toString();
}

/** A new repository in `dir` whose one commit, with the message `message`, holds `spec` as
 *  .dispatch/spec, and that commit. */
export function commitSpec(home: string, dir: string, spec: string, message: string): string {
  cpSync(spec, join(dir, ".dispatch", "spec"), { recursive: true });
  git(home, dir, "init", "-q");
  git(home, dir, "add", "-A");
  git(home, dir, "commit", "-q", "-m", message);
  return git(home, dir, "rev-parse", "HEAD").trim();
}

/** How one run of the step is set up. */
export interface StepRun {
  /** The ci-report binary. */
  readonly ciReport: string;
  /** The checkout the step runs in, and the job run whose variables it has. */
  readonly dir: string;
  readonly run: JobRun;
  /** The job's go test -json report, the step's `--report`. */
  readonly report: string;
  /** The step's `--job` and `--dispatch-cli`. */
  readonly job: string;
  readonly dispatchCli: string;
  /** The CLI's state directory, also its HOME. */
  readonly cliState: string;
  /** A DISPATCH_URL of the step's own, set inside the grant (`env DISPATCH_URL=<url>` ci-report):
   *  `agent-secrets result-grant` runs nothing when the DISPATCH_URL it is given names a host the
   *  airlock does not bind. */
  readonly dispatchUrl?: string;
  /** More variables of the job's, for `agent-secrets result-grant` and its child. */
  readonly env?: Readonly<Record<string, string>>;
}

/** The job's last step, as the action runs it: ci-report under `agent-secrets result-grant` in the
 *  checkout, over the report, with the job's variables and nothing of this process's own but
 *  PATH; what it printed, each line of standard output read as a ReportLine. */
export async function runStep(stack: CIResultsStack, step: StepRun): Promise<Reported> {
  const argv = [
    step.ciReport,
    "--dir",
    ".dispatch/spec",
    "--job",
    step.job,
    "--report",
    step.report,
    "--project",
    stack.project,
    "--dispatch-cli",
    step.dispatchCli,
  ];
  const child = Bun.spawn(
    stack.jobCommand(
      step.dispatchUrl === undefined ? argv : ["env", `DISPATCH_URL=${step.dispatchUrl}`, ...argv]
    ),
    {
      cwd: step.dir,
      env: {
        PATH: SHELL_PATH,
        HOME: step.cliState,
        DISPATCH_STATE_DIR: step.cliState,
        GITHUB_SHA: step.run.sha,
        ...stack.jobEnv(step.run),
        ...step.env,
      },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  const lines = stdout
    .split("\n")
    .filter((text) => text !== "")
    .map((text) => JSON.parse(text) as ReportLine);
  return { code, lines, stderr };
}

/** Each line as its check, outcome and exit. */
export function outcomes(reported: Reported): string[] {
  return reported.lines.map((line) => `${line.check} ${line.outcome} exit ${line.exit}`);
}
